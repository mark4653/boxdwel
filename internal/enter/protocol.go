// Package enter implements the M2 single-serial BusyBox shell session.
package enter

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// ReadyPrefix starts the nonce-specific ready marker line.
	ReadyPrefix = "M2_READY_"
	// ExitPrefix starts the nonce-specific shell-exit marker line.
	ExitPrefix = "M2_EXIT_"

	maxPending = 1 << 20
)

// ErrTimeout reports a bounded wait that expired.
var ErrTimeout = errors.New("timeout")

var noncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// NewNonce returns a fresh 128-bit hex marker suffix.
func NewNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// readyCommand installs an EXIT trap bound to this nonce and prints the ready
// marker. The trap writes the exit marker only when this exact shell exits.
func readyCommand(nonce string) ([]byte, error) {
	if !noncePattern.MatchString(nonce) {
		return nil, errors.New("nonce must be 32 lowercase hex characters")
	}
	return []byte("trap 'echo " + ExitPrefix + nonce + "' EXIT; echo " + ReadyPrefix + nonce + "\n"), nil
}

func exitMarker(nonce string) string { return ExitPrefix + nonce }

type chunk struct {
	data []byte
	err  error
}

// Conn wraps a byte stream with one reader so ready waits and the session pump
// can be split across calls without losing data or starting a second reader.
type Conn struct {
	stream io.ReadWriteCloser

	chunks   chan chunk
	pending  []byte
	discard  bool
	sendMu   sync.Mutex
	closeOne sync.Once
	done     chan struct{}
	closeErr error
}

// NewConn starts the single background reader for stream.
func NewConn(stream io.ReadWriteCloser) *Conn {
	c := &Conn{stream: stream, chunks: make(chan chunk, 64), done: make(chan struct{})}
	go c.readLoop()
	return c
}

func (c *Conn) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := c.stream.Read(buf)
		if n > 0 {
			select {
			case c.chunks <- chunk{data: append([]byte(nil), buf[:n]...)}:
			case <-c.done:
				return
			}
		}
		if err != nil {
			select {
			case c.chunks <- chunk{err: err}:
			case <-c.done:
			}
			return
		}
	}
}

// Send writes all bytes; a short write is retried, and a closed stream fails.
func (c *Conn) Send(p []byte) error {
	select {
	case <-c.done:
		return errors.New("session closed")
	default:
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	for len(p) > 0 {
		n, err := c.stream.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return fmt.Errorf("guest write: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Close wakes blocked readers/writers by closing the stream; it never waits for
// the read goroutine.
func (c *Conn) Close() error {
	c.closeOne.Do(func() {
		close(c.done)
		c.closeErr = c.stream.Close()
	})
	return c.closeErr
}

func (c *Conn) takeLines(match func(string) bool) bool {
	for {
		i := bytes.IndexByte(c.pending, '\n')
		if i < 0 {
			return false
		}
		line := strings.TrimSuffix(string(c.pending[:i]), "\r")
		c.pending = c.pending[i+1:]
		if match(line) {
			return true
		}
	}
}

// WaitLine echoes incoming bytes and waits for an exactly matching line.
func (c *Conn) WaitLine(want string, echo io.Writer, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if c.takeLines(func(line string) bool { return line == want }) {
			return nil
		}
		select {
		case <-c.done:
			return errors.New("session closed")
		case ch, ok := <-c.chunks:
			if !ok {
				return readError(ch)
			}
			if ch.err != nil {
				return readError(ch)
			}
			if echo != nil {
				if _, err := echo.Write(ch.data); err != nil {
					return err
				}
			}
			c.pending = append(c.pending, ch.data...)
			if len(c.pending) > maxPending {
				return fmt.Errorf("line exceeded %d bytes without %q", maxPending, want)
			}
		case <-timer.C:
			return fmt.Errorf("waiting for %q: %w", want, ErrTimeout)
		}
	}
}

// Pump copies stdin to the guest and guest output to out until the nonce exit
// marker. Overlong lines are discarded without matching inside them.
func (c *Conn) Pump(in io.Reader, out io.Writer, marker string) error {
	inputErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				if werr := c.Send(buf[:n]); werr != nil {
					inputErr <- werr
					return
				}
			}
			if err != nil {
				inputErr <- err
				return
			}
		}
	}()
	for {
		if !c.discard && c.takeLines(func(line string) bool { return line == marker }) {
			return nil
		}
		select {
		case <-c.done:
			return errors.New("session closed")
		case err := <-inputErr:
			// A finished stdin is not a guest exit; keep reading guest output.
			if err != io.EOF {
				return fmt.Errorf("local input: %w", err)
			}
			inputErr = nil
		case ch, ok := <-c.chunks:
			if !ok {
				return readError(ch)
			}
			if ch.err != nil {
				return readError(ch)
			}
			if _, err := out.Write(ch.data); err != nil {
				return err
			}
			c.pending = append(c.pending, ch.data...)
			if len(c.pending) > maxPending {
				c.pending = nil
				c.discard = true
			}
			if c.discard {
				if i := bytes.IndexByte(c.pending, '\n'); i >= 0 {
					c.pending = c.pending[i+1:]
					c.discard = false
				}
			}
		}
	}
}

func readError(ch chunk) error {
	if ch.err != nil {
		return fmt.Errorf("guest stream: %w", ch.err)
	}
	return errors.New("guest stream closed")
}
