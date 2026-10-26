package winrelay

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark4653/boxdwel/internal/agentproto"
)

type peer struct {
	t    *testing.T
	conn net.Conn
}

func (p *peer) read() agentproto.Frame {
	p.t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	f, err := agentproto.ReadFrame(p.conn)
	if err != nil {
		p.t.Fatalf("peer read: %v", err)
	}
	return f
}

func (p *peer) write(f agentproto.Frame) {
	p.t.Helper()
	_ = p.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := agentproto.WriteFrame(p.conn, f); err != nil {
		p.t.Fatalf("peer write: %v", err)
	}
}

type runResult struct {
	code int32
	err  error
}

type signalBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	ch  chan struct{}
}

func newSignalBuffer() *signalBuffer { return &signalBuffer{ch: make(chan struct{}, 1)} }

func (b *signalBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	b.mu.Unlock()
	select {
	case b.ch <- struct{}{}:
	default:
	}
	return n, err
}

func (b *signalBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func readyOne() []byte {
	p := agentproto.ReadyPayload()
	p[0] = 1
	return p
}

func fixedSize(rows, cols uint16) func() (uint16, uint16, error) {
	return func() (uint16, uint16, error) { return rows, cols, nil }
}

func waitOutput(t *testing.T, out *signalBuffer, marker string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		if strings.Contains(out.String(), marker) {
			return
		}
		select {
		case <-out.ch:
		case <-deadline:
			t.Fatalf("output %q never contained %q", out.String(), marker)
		}
	}
}

func startClient(t *testing.T, stream io.ReadWriteCloser, size func() (uint16, uint16, error), out io.Writer) (*io.PipeWriter, chan runResult) {
	t.Helper()
	inR, inW := io.Pipe()
	done := make(chan runResult, 1)
	go func() {
		code, err := RunClient(stream, ClientOptions{
			Size:             size,
			Input:            inR,
			Output:           out,
			ResizeInterval:   time.Hour,
			HandshakeTimeout: 200 * time.Millisecond,
			WriteTimeout:     time.Second,
		})
		done <- runResult{code, err}
	}()
	t.Cleanup(func() { _ = inW.Close(); _ = stream.Close() })
	return inW, done
}

func TestRunClientRoundTrip(t *testing.T) {
	client, server := net.Pipe()
	p := &peer{t: t, conn: server}
	t.Cleanup(func() { _ = server.Close() })
	out := newSignalBuffer()
	inW, done := startClient(t, client, fixedSize(24, 80), out)
	go func() {
		hello := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeReady, Control: hello.Control, Payload: readyOne()})
		open := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeOpened, Session: open.Session, Payload: open.Payload})
		for {
			f := p.read()
			switch f.Type {
			case agentproto.TypeInput:
				p.write(agentproto.Frame{Type: agentproto.TypeOutput, Session: f.Session, Payload: f.Payload})
			case agentproto.TypeClose:
				p.write(agentproto.Frame{Type: agentproto.TypeExited, Session: f.Session,
					Payload: agentproto.ExitedPayload(agentproto.ReasonShellExit, 3)})
				return
			}
		}
	}()
	if _, err := inW.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, out, "hello", 3*time.Second)
	_ = inW.Close()
	select {
	case res := <-done:
		if res.err != nil || res.code != 3 {
			t.Fatalf("code=%d err=%v", res.code, res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not finish")
	}
}

func TestRunClientBadReady(t *testing.T) {
	bad := readyOne()
	bad[0] = 2
	client, server := net.Pipe()
	p := &peer{t: t, conn: server}
	_, done := startClient(t, client, fixedSize(24, 80), newSignalBuffer())
	go func() {
		hello := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeReady, Control: hello.Control, Payload: bad})
		// A client that wrongly accepts the Ready proceeds to Open; complete
		// the session so the test sees success rather than a read timeout.
		_ = server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		open, err := agentproto.ReadFrame(server)
		if err != nil {
			return
		}
		p.write(agentproto.Frame{Type: agentproto.TypeOpened, Session: open.Session, Payload: open.Payload})
		p.write(agentproto.Frame{Type: agentproto.TypeExited, Session: open.Session,
			Payload: agentproto.ExitedPayload(agentproto.ReasonShellExit, 0)})
	}()
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("two-session Ready was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not reject Ready")
	}
}

func TestRunClientOpenedSizeMismatch(t *testing.T) {
	client, server := net.Pipe()
	p := &peer{t: t, conn: server}
	_, done := startClient(t, client, fixedSize(24, 80), newSignalBuffer())
	go func() {
		hello := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeReady, Control: hello.Control, Payload: readyOne()})
		open := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeOpened, Session: open.Session, Payload: mustSize(1, 1)})
	}()
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("mismatched Opened was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not reject Opened")
	}
}

func TestRunClientForeignOutput(t *testing.T) {
	client, server := net.Pipe()
	p := &peer{t: t, conn: server}
	out := newSignalBuffer()
	_, done := startClient(t, client, fixedSize(24, 80), out)
	go func() {
		hello := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeReady, Control: hello.Control, Payload: readyOne()})
		open := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeOpened, Session: open.Session, Payload: open.Payload})
		p.write(agentproto.Frame{Type: agentproto.TypeOutput, Session: open.Session + 1, Payload: []byte("secret")})
	}()
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("foreign session output was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not reject foreign output")
	}
	if strings.Contains(out.String(), "secret") {
		t.Fatalf("foreign bytes reached the UI: %q", out.String())
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestRunClientShortOutputWrite(t *testing.T) {
	client, server := net.Pipe()
	p := &peer{t: t, conn: server}
	_, done := startClient(t, client, fixedSize(24, 80), shortWriter{})
	go func() {
		hello := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeReady, Control: hello.Control, Payload: readyOne()})
		open := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeOpened, Session: open.Session, Payload: open.Payload})
		p.write(agentproto.Frame{Type: agentproto.TypeOutput, Session: open.Session, Payload: []byte("data")})
	}()
	select {
	case res := <-done:
		if !errors.Is(res.err, io.ErrShortWrite) {
			t.Fatalf("got %v", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("short write was not reported")
	}
}

func TestRunClientCloseTimeout(t *testing.T) {
	client, server := net.Pipe()
	p := &peer{t: t, conn: server}
	inW, done := startClient(t, client, fixedSize(24, 80), newSignalBuffer())
	go func() {
		hello := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeReady, Control: hello.Control, Payload: readyOne()})
		open := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeOpened, Session: open.Session, Payload: open.Payload})
		p.read() // Close; never answer
	}()
	_ = inW.Close()
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("missing Close response was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close timeout was not bounded")
	}
}

type failAfterStream struct {
	net.Conn
	mu        sync.Mutex
	writes    int
	failAfter int
}

func (s *failAfterStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.writes++
	fail := s.writes > s.failAfter
	s.mu.Unlock()
	if fail {
		return 0, errors.New("write failed")
	}
	return s.Conn.Write(p)
}

func TestRunClientInputWriteFailure(t *testing.T) {
	client, server := net.Pipe()
	stream := &failAfterStream{Conn: client, failAfter: 2} // Hello + Open succeed
	p := &peer{t: t, conn: server}
	out := newSignalBuffer()
	inR, inW := io.Pipe()
	done := make(chan runResult, 1)
	go func() {
		code, err := RunClient(stream, ClientOptions{
			Size: fixedSize(24, 80), Input: inR, Output: out,
			ResizeInterval: time.Hour, HandshakeTimeout: 200 * time.Millisecond,
		})
		done <- runResult{code, err}
	}()
	go func() {
		hello := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeReady, Control: hello.Control, Payload: readyOne()})
		open := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeOpened, Session: open.Session, Payload: open.Payload})
	}()
	if _, err := inW.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("input write failure was not reported")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("input write failure did not finish")
	}
	_ = inW.Close()
}

func TestRunClientResize(t *testing.T) {
	client, server := net.Pipe()
	p := &peer{t: t, conn: server}
	rowsCh := make(chan uint16, 1)
	var rowsMu sync.Mutex
	rows := uint16(24)
	size := func() (uint16, uint16, error) {
		rowsMu.Lock()
		defer rowsMu.Unlock()
		return rows, 80, nil
	}
	inR, inW := io.Pipe()
	out := newSignalBuffer()
	done := make(chan runResult, 1)
	go func() {
		code, err := RunClient(client, ClientOptions{
			Size: size, Input: inR, Output: out,
			ResizeInterval: 10 * time.Millisecond, HandshakeTimeout: 200 * time.Millisecond,
		})
		done <- runResult{code, err}
	}()
	resized := make(chan struct{}, 1)
	opened := make(chan struct{})
	go func() {
		hello := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeReady, Control: hello.Control, Payload: readyOne()})
		open := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeOpened, Session: open.Session, Payload: open.Payload})
		close(opened)
		for {
			f := p.read()
			switch f.Type {
			case agentproto.TypeResize:
				r, _, _ := agentproto.ParseSize(f.Payload)
				select {
				case rowsCh <- r:
				default:
				}
				select {
				case resized <- struct{}{}:
				default:
				}
			case agentproto.TypeClose:
				p.write(agentproto.Frame{Type: agentproto.TypeExited, Session: f.Session,
					Payload: agentproto.ExitedPayload(agentproto.ReasonShellExit, 0)})
				return
			}
		}
	}()
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("handshake did not complete")
	}
	rowsMu.Lock()
	rows = 40
	rowsMu.Unlock()
	select {
	case r := <-rowsCh:
		if r != 40 {
			t.Fatalf("resize rows %d", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resize was not sent")
	}
	_ = inW.Close()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("client: %v", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not finish")
	}
}

func mustSize(rows, cols uint16) []byte {
	p, err := agentproto.Size(rows, cols)
	if err != nil {
		panic(err)
	}
	return p
}

type closedWriteStream struct {
	net.Conn
	mu        sync.Mutex
	writes    int
	failAfter int
}

func (s *closedWriteStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.writes++
	fail := s.writes > s.failAfter
	s.mu.Unlock()
	if fail {
		return 0, io.ErrClosedPipe
	}
	return s.Conn.Write(p)
}

func handshakePeer(t *testing.T, p *peer) {
	t.Helper()
	hello := p.read()
	p.write(agentproto.Frame{Type: agentproto.TypeReady, Control: hello.Control, Payload: readyOne()})
	open := p.read()
	p.write(agentproto.Frame{Type: agentproto.TypeOpened, Session: open.Session, Payload: open.Payload})
}

func startClosedWriteClient(t *testing.T, stream io.ReadWriteCloser, size func() (uint16, uint16, error)) (io.WriteCloser, chan runResult) {
	t.Helper()
	inR, inW := io.Pipe()
	done := make(chan runResult, 1)
	go func() {
		code, err := RunClient(stream, ClientOptions{
			Size: size, Input: inR, Output: newSignalBuffer(),
			ResizeInterval: time.Hour, HandshakeTimeout: 200 * time.Millisecond,
			WriteTimeout: time.Second,
		})
		done <- runResult{code, err}
	}()
	t.Cleanup(func() { _ = inW.Close(); _ = stream.Close() })
	return inW, done
}

func TestRunClientInputClosedPipe(t *testing.T) {
	client, server := net.Pipe()
	stream := &closedWriteStream{Conn: client, failAfter: 2}
	p := &peer{t: t, conn: server}
	inW, done := startClosedWriteClient(t, stream, fixedSize(24, 80))
	go handshakePeer(t, p)
	if _, err := inW.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if !errors.Is(res.err, io.ErrClosedPipe) {
			t.Fatalf("got %v, want closed pipe", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read-open/write-closed stream hung RunClient")
	}
}

func TestRunClientResizeClosedPipe(t *testing.T) {
	client, server := net.Pipe()
	stream := &closedWriteStream{Conn: client, failAfter: 2}
	p := &peer{t: t, conn: server}
	var mu sync.Mutex
	rows := uint16(24)
	size := func() (uint16, uint16, error) {
		mu.Lock()
		defer mu.Unlock()
		return rows, 80, nil
	}
	inR, inW := io.Pipe()
	done := make(chan runResult, 1)
	go func() {
		code, err := RunClient(stream, ClientOptions{
			Size: size, Input: inR, Output: newSignalBuffer(),
			ResizeInterval: 10 * time.Millisecond, HandshakeTimeout: 200 * time.Millisecond,
		})
		done <- runResult{code, err}
	}()
	t.Cleanup(func() { _ = inW.Close() })
	opened := make(chan struct{})
	go func() {
		handshakePeer(t, p)
		close(opened)
	}()
	<-opened
	mu.Lock()
	rows = 40
	mu.Unlock()
	select {
	case res := <-done:
		if !errors.Is(res.err, io.ErrClosedPipe) {
			t.Fatalf("got %v, want closed pipe", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resize write failure did not finish")
	}
}

func TestRunClientCloseClosedPipe(t *testing.T) {
	client, server := net.Pipe()
	stream := &closedWriteStream{Conn: client, failAfter: 2}
	p := &peer{t: t, conn: server}
	inW, done := startClosedWriteClient(t, stream, fixedSize(24, 80))
	opened := make(chan struct{})
	go func() {
		handshakePeer(t, p)
		close(opened)
	}()
	<-opened
	_ = inW.Close()
	select {
	case res := <-done:
		if !errors.Is(res.err, io.ErrClosedPipe) {
			t.Fatalf("got %v, want closed pipe", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close write failure hung RunClient")
	}
}

func TestRunClientOpenForeignError(t *testing.T) {
	client, server := net.Pipe()
	p := &peer{t: t, conn: server}
	_, done := startClient(t, client, fixedSize(24, 80), newSignalBuffer())
	go func() {
		hello := p.read()
		p.write(agentproto.Frame{Type: agentproto.TypeReady, Control: hello.Control, Payload: readyOne()})
		_ = p.read() // Open
		p.write(agentproto.Frame{Type: agentproto.TypeError, Session: 2,
			Payload: agentproto.ErrorPayload(agentproto.CodeProtocol, "foreign")})
	}()
	select {
	case res := <-done:
		if res.err == nil || !strings.Contains(res.err.Error(), "foreign session") {
			t.Fatalf("got %v", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreign open error was not rejected")
	}
}
