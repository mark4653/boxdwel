// Package winrelay drives one terminal client over the relay protocol and, on
// Windows, attaches verified named-pipe clients to the relay core. The client
// frame/state code has no OS dependency and is tested on any platform; only the
// pipe serving and console helpers are Windows-specific.
package winrelay

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/mark4653/boxdwel/internal/agentproto"
)

const (
	clientSession = 1
	maxDimension  = 500
)

// ClientOptions supplies the terminal side of one session. Size is required;
// there is no implicit fallback size.
//
// The Input goroutine may outlive RunClient (a blocking console read cannot be
// canceled). Output and Size must return promptly, or completion and joins can
// block. This is a short-CLI helper; a long-lived broker must not reuse it.
type ClientOptions struct {
	Size             func() (rows, cols uint16, err error)
	Input            io.Reader
	Output           io.Writer
	ResizeInterval   time.Duration
	HandshakeTimeout time.Duration
	WriteTimeout     time.Duration
}

// RunClient drives one terminal session and takes ownership of stream from
// entry: stream is closed on every return, including handshake errors. It
// returns the guest exit code or the first session/transport error. The caller
// owns the console mode and must restore it on every path.
func RunClient(stream io.ReadWriteCloser, opt ClientOptions) (int32, error) {
	defer stream.Close()
	if opt.Size == nil || opt.Input == nil || opt.Output == nil {
		return 0, errors.New("winrelay: Size, Input and Output are required")
	}
	if opt.ResizeInterval <= 0 {
		opt.ResizeInterval = 250 * time.Millisecond
	}
	if opt.HandshakeTimeout <= 0 {
		opt.HandshakeTimeout = 5 * time.Second
	}
	if opt.WriteTimeout <= 0 {
		opt.WriteTimeout = 10 * time.Second
	}
	rows, cols, err := opt.Size()
	if err != nil {
		return 0, err
	}
	if rows < 1 || rows > maxDimension || cols < 1 || cols > maxDimension {
		return 0, fmt.Errorf("winrelay: terminal size %dx%d outside 1..%d", rows, cols, maxDimension)
	}
	sizePayload, err := agentproto.Size(rows, cols)
	if err != nil {
		return 0, err
	}
	if err := handshake(stream, sizePayload, opt.HandshakeTimeout, opt.WriteTimeout); err != nil {
		return 0, err
	}
	opened, err := readFrameTimed(stream, opt.HandshakeTimeout)
	if err != nil {
		return 0, err
	}
	switch opened.Type {
	case agentproto.TypeOpened:
		if opened.Session != clientSession {
			return 0, fmt.Errorf("winrelay: Opened for session %d", opened.Session)
		}
		orows, ocols, err := agentproto.ParseSize(opened.Payload)
		if err != nil {
			return 0, fmt.Errorf("winrelay: Opened size: %w", err)
		}
		if orows != rows || ocols != cols {
			return 0, fmt.Errorf("winrelay: Opened size %dx%d does not match %dx%d", orows, ocols, rows, cols)
		}
	case agentproto.TypeError:
		if opened.Session != 0 && opened.Session != clientSession {
			return 0, fmt.Errorf("winrelay: Error for foreign session %d", opened.Session)
		}
		_, msg, _ := agentproto.ParseError(opened.Payload)
		return 0, fmt.Errorf("winrelay: session open failed: %s", msg)
	default:
		return 0, fmt.Errorf("winrelay: unexpected open frame %d", opened.Type)
	}

	var (
		writeMu sync.Mutex
		done    = make(chan struct{})
		once    sync.Once
		code    int32
		runErr  error
	)
	finish := func(c int32, err error) {
		once.Do(func() {
			code, runErr = c, err
			close(done)
			_ = stream.Close()
		})
	}
	write := func(f agentproto.Frame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		select {
		case <-done:
			return io.ErrClosedPipe
		default:
		}
		return writeFrameTimed(stream, f, opt.WriteTimeout)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			f, err := agentproto.ReadFrame(stream)
			if err != nil {
				select {
				case <-done:
					return
				default:
				}
				finish(0, err)
				return
			}
			switch f.Type {
			case agentproto.TypeOutput:
				if f.Session != clientSession {
					finish(0, fmt.Errorf("winrelay: output for foreign session %d", f.Session))
					return
				}
				n, err := opt.Output.Write(f.Payload)
				if err != nil {
					finish(0, err)
					return
				}
				if n != len(f.Payload) {
					finish(0, io.ErrShortWrite)
					return
				}
			case agentproto.TypeExited:
				if f.Session != clientSession {
					finish(0, fmt.Errorf("winrelay: Exited for foreign session %d", f.Session))
					return
				}
				reason, exitCode, err := agentproto.ParseExited(f.Payload)
				if err != nil {
					finish(0, err)
					return
				}
				if reason != agentproto.ReasonShellExit {
					exitCode = -1
				}
				finish(exitCode, nil)
				return
			case agentproto.TypeError:
				if f.Session != 0 && f.Session != clientSession {
					finish(0, fmt.Errorf("winrelay: Error for foreign session %d", f.Session))
					return
				}
				c, msg, _ := agentproto.ParseError(f.Payload)
				finish(0, fmt.Errorf("winrelay: session error %d: %s", c, msg))
				return
			default:
				finish(0, fmt.Errorf("winrelay: unexpected frame %d", f.Type))
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		lastRows, lastCols := rows, cols
		ticker := time.NewTicker(opt.ResizeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			rows, cols, err := opt.Size()
			if err != nil {
				finish(0, err)
				return
			}
			if rows == lastRows && cols == lastCols {
				continue
			}
			if rows < 1 || rows > maxDimension || cols < 1 || cols > maxDimension {
				finish(0, fmt.Errorf("winrelay: resize %dx%d outside 1..%d", rows, cols, maxDimension))
				return
			}
			p, err := agentproto.Size(rows, cols)
			if err != nil {
				finish(0, err)
				return
			}
			if err := write(agentproto.Frame{Type: agentproto.TypeResize, Session: clientSession, Payload: p}); err != nil {
				finish(0, err)
				return
			}
			lastRows, lastCols = rows, cols
		}
	}()
	go func() {
		buf := make([]byte, agentproto.MaxOutputChunk)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := opt.Input.Read(buf)
			if n > 0 {
				if werr := write(agentproto.Frame{Type: agentproto.TypeInput, Session: clientSession, Payload: buf[:n]}); werr != nil {
					finish(0, werr)
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					if werr := write(agentproto.Frame{Type: agentproto.TypeClose, Session: clientSession}); werr != nil {
						finish(0, werr)
					}
					// A peer that never confirms the Close must not block the
					// caller forever.
					select {
					case <-done:
					case <-time.After(opt.HandshakeTimeout):
						finish(0, errors.New("winrelay: no exit after stdin EOF"))
					}
					return
				}
				finish(0, err)
				return
			}
		}
	}()

	<-done
	wg.Wait()
	return code, runErr
}

// handshake validates Ready before sending Open, so a peer with unsupported
// limits never sees a session request.
func handshake(stream io.ReadWriteCloser, sizePayload []byte, readTimeout, writeTimeout time.Duration) error {
	if err := writeFrameTimed(stream, agentproto.Frame{Type: agentproto.TypeHello, Control: 1}, writeTimeout); err != nil {
		return err
	}
	ready, err := readFrameTimed(stream, readTimeout)
	if err != nil {
		return err
	}
	if ready.Type != agentproto.TypeReady || ready.Control != 1 {
		return fmt.Errorf("winrelay: unexpected handshake frame %d", ready.Type)
	}
	if err := validateReady(ready); err != nil {
		return err
	}
	return writeFrameTimed(stream, agentproto.Frame{
		Type: agentproto.TypeOpen, Session: clientSession, Payload: sizePayload,
	}, writeTimeout)
}

func validateReady(f agentproto.Frame) error {
	sessions, payload, output, err := agentproto.ParseReady(f.Payload)
	if err != nil {
		return fmt.Errorf("winrelay: bad Ready: %w", err)
	}
	if sessions != 1 {
		return fmt.Errorf("winrelay: Ready reports %d sessions, want 1", sessions)
	}
	if payload < agentproto.MaxPayload || output < agentproto.MaxOutputChunk {
		return fmt.Errorf("winrelay: Ready limits %d/%d are below %d/%d", payload, output, agentproto.MaxPayload, agentproto.MaxOutputChunk)
	}
	return nil
}

func readFrameTimed(s io.ReadWriteCloser, d time.Duration) (agentproto.Frame, error) {
	type result struct {
		f   agentproto.Frame
		err error
	}
	ch := make(chan result, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := agentproto.ReadFrame(s)
		ch <- result{f, err}
	}()
	select {
	case res := <-ch:
		return res.f, res.err
	case <-time.After(d):
		_ = s.Close()
		<-done
		return agentproto.Frame{}, fmt.Errorf("winrelay: read timeout after %s", d)
	}
}

func writeFrameTimed(s io.ReadWriteCloser, f agentproto.Frame, d time.Duration) error {
	timer := time.AfterFunc(d, func() { _ = s.Close() })
	err := agentproto.WriteFrame(s, f)
	timer.Stop()
	return err
}
