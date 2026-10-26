//go:build linux

package agent

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/mark4653/boxdwel/internal/agentproto"
)

// rawHeader builds a product header for tests that need to send malformed or
// partial frames before the implementation is fixed.
func rawHeader(typ uint8, control, session, length uint16) []byte {
	return []byte{'B', 'w', 1, typ,
		byte(control >> 8), byte(control),
		byte(session >> 8), byte(session),
		byte(length >> 8), byte(length)}
}

func startAgentTCP(t *testing.T) *host {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
		ln.Close()
	}()
	hc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ac := <-accepted
	done := make(chan error, 1)
	go func() { done <- Serve(ac) }()
	h := &host{t: t, conn: hc, done: done}
	t.Cleanup(func() {
		_ = hc.Close()
		h.wait(t, 5*time.Second)
	})
	h.hello(1)
	return h
}

// TestExperimentalSixByteRejectedImmediately: a 6-byte experimental Hello must
// fail on the magic, without waiting for a 10-byte product header.
func TestExperimentalSixByteRejectedImmediately(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(a) }()
	go func() { _, _ = b.Write([]byte{0x01, 0x07, 0, 0, 0, 0}) }()
	select {
	case err := <-done:
		if !errors.Is(err, agentproto.ErrBadMagic) {
			t.Fatalf("got %v, want bad magic", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("6-byte experimental frame was not rejected")
	}
	_ = b.Close()
}

// TestMetadataRejectedBeforePayload: an unknown type with a nonzero length
// must fail before waiting for the payload.
func TestMetadataRejectedBeforePayload(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(a) }()
	go func() { _, _ = b.Write(rawHeader(0x63, 0, 1, 5)) }()
	select {
	case err := <-done:
		if !errors.Is(err, agentproto.ErrBadType) {
			t.Fatalf("got %v, want bad type", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unknown type waited for a payload")
	}
	_ = b.Close()
}

// TestTruncatedPayloadIsNotCleanEOF: a complete header followed by EOF on the
// payload is a truncated frame, not a clean session end.
func TestTruncatedPayloadIsNotCleanEOF(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(a) }()
	h := &host{t: t, conn: b, done: done}
	t.Cleanup(func() { b.Close() })
	h.hello(1)
	if _, err := b.Write(rawHeader(byte(agentproto.TypeInput), 0, 1, 4)); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("truncated payload was treated as a clean EOF")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not finish")
	}
}

// TestStalledSessionKeepsWireAlive: a foreground process that does not read
// its PTY must not stop Ping, other sessions, or Close.
func TestStalledSessionKeepsWireAlive(t *testing.T) {
	h := startAgentTCP(t)
	h.open(1, 24, 80)
	h.open(2, 24, 80)

	h.input(1, "sh -c 'printf \"STALL%s\\n\" _READY; exec sleep 30'\n")
	h.outputUntil(1, "STALL_READY")

	for i := 0; i < 16; i++ {
		_ = h.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := agentproto.WriteFrame(h.conn, agentproto.Frame{
			Type: agentproto.TypeInput, Session: 1, Payload: bytes.Repeat([]byte{'x'}, 4096),
		}); err != nil {
			t.Fatalf("flood write %d: %v", i, err)
		}
	}
	time.Sleep(200 * time.Millisecond) // let the agent reach the stalled PTY write

	if err := h.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	h.send(agentproto.Frame{Type: agentproto.TypePing, Control: 21, Payload: agentproto.NoncePayload(1)})
	for {
		f, err := agentproto.ReadFrame(h.conn)
		if err != nil {
			t.Fatalf("queued ping was not answered: %v", err)
		}
		if f.Type == agentproto.TypePong && f.Control == 21 {
			break
		}
	}

	h.input(2, "printf 'SECOND%s\\n' _OK\n")
	h.outputUntil(2, "SECOND_OK")
	h.send(agentproto.Frame{Type: agentproto.TypeClose, Session: 2})
	h.exited(2)
}

// TestSerialCloseUnblocksPendingRead: closing the opened serial device must
// release a blocked Read.
func TestSerialCloseUnblocksPendingRead(t *testing.T) {
	_, path := testPTY(t)
	conn, err := OpenSerial(path)
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := conn.Read(buf)
		readDone <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock a pending serial read")
	}
}

// failAfterWriteConn succeeds at the first Write (Ready) and fails later.
type failAfterWriteConn struct {
	mu    sync.Mutex
	in    []byte
	wrote bool
	done  chan struct{}
	once  sync.Once
}

func newFailAfterWriteConn() *failAfterWriteConn {
	return &failAfterWriteConn{done: make(chan struct{})}
}

func (c *failAfterWriteConn) feed(p []byte) {
	c.mu.Lock()
	c.in = append(c.in, p...)
	c.mu.Unlock()
}

func (c *failAfterWriteConn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.in) > 0 {
			n := copy(p, c.in)
			c.in = c.in[n:]
			c.mu.Unlock()
			return n, nil
		}
		c.mu.Unlock()
		select {
		case <-c.done:
			return 0, io.EOF
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (c *failAfterWriteConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	first := !c.wrote
	c.wrote = true
	c.mu.Unlock()
	if first {
		return len(p), nil
	}
	return 0, errors.New("wire write failed")
}

func (c *failAfterWriteConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

// TestWireWriteFailureStopsAgent: a failed Pong write must end the connection,
// not be ignored while the reader keeps waiting.
func TestWireWriteFailureStopsAgent(t *testing.T) {
	c := newFailAfterWriteConn()
	done := make(chan error, 1)
	go func() { done <- Serve(c) }()
	var hello bytes.Buffer
	if err := agentproto.WriteFrame(&hello, agentproto.Frame{Type: agentproto.TypeHello, Control: 1}); err != nil {
		t.Fatal(err)
	}
	c.feed(hello.Bytes())
	time.Sleep(200 * time.Millisecond) // let Ready be written

	var ping bytes.Buffer
	if err := agentproto.WriteFrame(&ping, agentproto.Frame{Type: agentproto.TypePing, Control: 2, Payload: agentproto.NoncePayload(1)}); err != nil {
		t.Fatal(err)
	}
	c.feed(ping.Bytes())
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("write failure was reported as a clean EOF")
		}
	case <-time.After(3 * time.Second):
		c.Close()
		t.Fatal("agent ignored the wire write failure")
	}
}

// TestRawClearsStopbitsAndFlowControl: an existing CSTOPB/CRTSCTS/IXOFF/IXON
// setup must become 8N1 with no flow control.
func TestRawClearsStopbitsAndFlowControl(t *testing.T) {
	_, path := testPTY(t)
	raw, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	var tm syscall.Termios
	if err := ioctl(raw.Fd(), syscall.TCGETS, unsafe.Pointer(&tm)); err != nil {
		t.Fatal(err)
	}
	tm.Cflag |= syscall.CSTOPB | cflagCRTSCTS
	tm.Iflag |= syscall.IXON | syscall.IXOFF | syscall.IXANY
	if err := ioctl(raw.Fd(), syscall.TCSETS, unsafe.Pointer(&tm)); err != nil {
		t.Fatal(err)
	}

	conn, err := OpenSerial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	rc, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got syscall.Termios
	var ioErr error
	if err := rc.Control(func(fd uintptr) {
		ioErr = ioctl(fd, syscall.TCGETS, unsafe.Pointer(&got))
	}); err != nil {
		t.Fatal(err)
	}
	if ioErr != nil {
		t.Fatal(ioErr)
	}
	if got.Cflag&syscall.CSTOPB != 0 || got.Cflag&cflagCRTSCTS != 0 {
		t.Fatalf("stop bits/flow control remain: cflag=%#x", got.Cflag)
	}
	if got.Iflag&(syscall.IXON|syscall.IXOFF|syscall.IXANY) != 0 {
		t.Fatalf("input flow control remains: iflag=%#x", got.Iflag)
	}
	if got.Cflag&syscall.CSIZE != syscall.CS8 || got.Cflag&syscall.CREAD == 0 || got.Cflag&syscall.CLOCAL == 0 {
		t.Fatalf("not 8N1 local: cflag=%#x", got.Cflag)
	}
	if got.Cflag&0x100f != syscall.B115200 {
		t.Fatalf("not 115200: cflag=%#x", got.Cflag)
	}
}

func TestNaturalExitCodesRepeat(t *testing.T) {
	h := startAgent(t)
	for i, want := range []int32{0, 3, 7, 3} {
		id := uint16(i + 1)
		h.open(id, 24, 80)
		h.input(id, fmt.Sprintf("exit %d\n", want))
		reason, code := h.exited(id)
		if reason != agentproto.ReasonShellExit || code != want {
			t.Fatalf("session %d: reason=%d code=%d want code=%d", id, reason, code, want)
		}
	}
}

// TestCloseKillsHupIgnoringForegroundJob: killing only the shell's own process
// group leaves a foreground job that ignores HUP. The session owner must kill
// the foreground group too.
func TestCloseKillsHupIgnoringForegroundJob(t *testing.T) {
	h := startAgent(t)
	h.open(1, 24, 80)
	h.input(1, "sh -c 'trap \"\" HUP; printf \"JOB%s:%s\\n\" _READY $$; exec sleep 60'\n")
	out := h.outputUntil(1, "JOB_READY:")
	job := parsePID(t, out)

	h.send(agentproto.Frame{Type: agentproto.TypeClose, Session: 1})
	reason, _ := h.exited(1)
	if reason != agentproto.ReasonClosed {
		t.Fatalf("reason=%d", reason)
	}
	waitGone(t, job)
}

func TestWorkingDirectoryIsFixed(t *testing.T) {
	h := startAgent(t)
	h.open(1, 24, 80)
	h.input(1, "printf 'PWD%s:%s\\n' _IS \"$PWD\"; pwd\n")
	out := h.outputUntil(1, "PWD_IS:/")
	if strings.Contains(out, "/home/") {
		t.Fatalf("host working directory leaked: %q", out)
	}
	h.input(1, "exit\n")
	h.exited(1)
}

func TestUnknownCloseIsProtocolError(t *testing.T) {
	h := startAgent(t)
	if err := h.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	h.send(agentproto.Frame{Type: agentproto.TypeClose, Session: 7})
	f, err := agentproto.ReadFrame(h.conn)
	if err != nil {
		t.Fatalf("unknown close was ignored: %v", err)
	}
	if f.Type != agentproto.TypeError || f.Session != 0 {
		t.Fatalf("want control Error, got %+v", f)
	}
}

func TestReadyPayloadValidation(t *testing.T) {
	bad := map[string][]byte{
		"reserved": {2, 1, 0x10, 0x00, 0x04, 0x00},
		"too many": {3, 0, 0x10, 0x00, 0x04, 0x00},
		"zero":     {0, 0, 0x10, 0x00, 0x04, 0x00},
		"payload":  {2, 0, 0x00, 0x00, 0x04, 0x00},
		"output":   {2, 0, 0x10, 0x00, 0x00, 0x00},
	}
	for name, p := range bad {
		if _, _, _, err := agentproto.ParseReady(p); err == nil {
			t.Errorf("%s: accepted %v", name, p)
		}
	}
}

func TestEnqueueBounded(t *testing.T) {
	ch := make(chan []byte, 1)
	if !enqueue(ch, []byte("a")) {
		t.Fatal("first push must fit")
	}
	if enqueue(ch, []byte("b")) {
		t.Fatal("full queue must reject without blocking")
	}
}

func TestApplyInputTimeout(t *testing.T) {
	in := make(chan []byte, 1)
	done := make(chan struct{})
	block := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once
	release := func() {
		once.Do(func() { close(released); close(block) })
	}
	in <- []byte("x")
	errCh := make(chan error, 1)
	go func() {
		errCh <- applyInput(in, done, 100*time.Millisecond, func([]byte) error {
			<-block
			return nil
		}, release)
	}()
	select {
	case err := <-errCh:
		if err != errInputTimeout {
			t.Fatalf("got %v, want input timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked input write was not timed out")
	}
	select {
	case <-released:
	default:
		t.Fatal("timeout did not release the write")
	}
}

// TestPTYCloseReleasesBlockedWrite: a PTY input write that blocks because the
// output side is full must return when the master is closed.
func TestPTYCloseReleasesBlockedWrite(t *testing.T) {
	p, err := startShell(24, 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.master.Write([]byte("yes\n")); err != nil {
		t.Fatal(err)
	}
	// Let yes fill the unread output buffer; then the input write blocks.
	blocked := false
	for i := 0; i < 20 && !blocked; i++ {
		done := make(chan struct{})
		go func() {
			_, _ = p.master.Write(bytes.Repeat([]byte{'x'}, 4096))
			close(done)
		}()
		select {
		case <-done:
			time.Sleep(20 * time.Millisecond)
		case <-time.After(50 * time.Millisecond):
			blocked = true
			p.closeMaster()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Close did not release a blocked PTY write")
			}
		}
	}
	if !blocked {
		p.closeMaster()
		p.kill()
		t.Skip("kernel accepts PTY input faster than this test can fill it")
	}
	p.kill()
	_, _ = p.wait()
}

func TestHostErrorFrameRejected(t *testing.T) {
	h := startAgent(t)
	if err := h.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	h.send(agentproto.Frame{Type: agentproto.TypeError, Control: 4, Payload: agentproto.ErrorPayload(1, "host side")})
	f, err := agentproto.ReadFrame(h.conn)
	if err != nil {
		t.Fatalf("no protocol error reported: %v", err)
	}
	if f.Type != agentproto.TypeError || f.Session != 0 {
		t.Fatalf("want control Error, got %+v", f)
	}
}

// TestSlowPeerTearsDownWithinTimeout: when the host stops reading, the output
// pump's blocked write must time out, close the connection, and clean up the
// owned shell.
func TestSlowPeerTearsDownWithinTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
		ln.Close()
	}()
	hc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	if tc, ok := hc.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	ac := <-accepted
	done := make(chan error, 1)
	go func() {
		done <- serve(ac, options{
			writeTimeout: 300 * time.Millisecond,
			inputTimeout: 300 * time.Millisecond,
			exitWait:     time.Second,
		})
	}()
	h := &host{t: t, conn: hc, done: done}
	h.hello(1)
	h.open(1, 24, 80)
	h.input(1, "printf 'PID:%s\\n' $$; printf 'DONE%s\\n' _X\n")
	pid := parsePID(t, h.outputUntil(1, "DONE_X"))

	h.input(1, "yes\n")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("slow peer was reported as a clean EOF")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked wire write was not bounded")
	}
	waitGone(t, pid)
}

// TestNaturalExitWithSlaveHolderDrains: after the shell exits, a background
// job that keeps the slave open must not hold the session forever.
func TestNaturalExitWithSlaveHolderDrains(t *testing.T) {
	h := startAgentTCP(t)
	h.open(1, 24, 80)
	h.input(1, "printf 'TAIL%s\\n' _OUT; sleep 2 & exit 3\n")
	if out := h.outputUntil(1, "TAIL_OUT"); !strings.Contains(out, "TAIL_OUT") {
		t.Fatalf("tail output lost: %q", out)
	}
	type result struct {
		reason uint8
		code   int32
	}
	got := make(chan result, 1)
	go func() {
		reason, code := h.exited(1)
		got <- result{reason, code}
	}()
	select {
	case r := <-got:
		if r.reason != agentproto.ReasonShellExit || r.code != 3 {
			t.Fatalf("reason=%d code=%d", r.reason, r.code)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("shell exit was not drained while a slave holder kept the PTY open")
	}
	// The slot must be free again.
	h.open(2, 24, 80)
	h.input(2, "exit\n")
	h.exited(2)
}

// budgetConn allows a fixed number of successful frame writes, then fails
// with io.ErrClosedPipe; reads stay open until Close.
type budgetConn struct {
	mu     sync.Mutex
	in     []byte
	writes int
	allow  int
	done   chan struct{}
	once   sync.Once
}

func newBudgetConn(allow int) *budgetConn {
	return &budgetConn{allow: allow, done: make(chan struct{})}
}

func (c *budgetConn) feed(p []byte) {
	c.mu.Lock()
	c.in = append(c.in, p...)
	c.mu.Unlock()
}

func (c *budgetConn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.in) > 0 {
			n := copy(p, c.in)
			c.in = c.in[n:]
			c.mu.Unlock()
			return n, nil
		}
		c.mu.Unlock()
		select {
		case <-c.done:
			return 0, os.ErrClosed
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (c *budgetConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	ok := c.writes <= c.allow
	c.mu.Unlock()
	if !ok {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func (c *budgetConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func feedHello(t *testing.T, c *budgetConn) {
	t.Helper()
	var hello bytes.Buffer
	if err := agentproto.WriteFrame(&hello, agentproto.Frame{Type: agentproto.TypeHello, Control: 1}); err != nil {
		t.Fatal(err)
	}
	c.feed(hello.Bytes())
	time.Sleep(50 * time.Millisecond)
}

func feedOpen(t *testing.T, c *budgetConn) {
	t.Helper()
	p, err := agentproto.Size(24, 80)
	if err != nil {
		t.Fatal(err)
	}
	var open bytes.Buffer
	if err := agentproto.WriteFrame(&open, agentproto.Frame{Type: agentproto.TypeOpen, Session: 1, Payload: p}); err != nil {
		t.Fatal(err)
	}
	c.feed(open.Bytes())
	time.Sleep(50 * time.Millisecond)
}

// TestWriteOnlyClosedPipeIsError: the output pump's write fails with a closed
// pipe while the read side is still open. The session must not be reported as
// a clean EOF.
func TestWriteOnlyClosedPipeIsError(t *testing.T) {
	c := newBudgetConn(2) // Ready + Opened, then the first Output fails
	done := make(chan error, 1)
	go func() { done <- Serve(c) }()
	feedHello(t, c)
	feedOpen(t, c)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("write-only closed pipe was reported as a clean EOF")
		}
	case <-time.After(3 * time.Second):
		c.Close()
		t.Fatal("write-only closed pipe did not end the connection")
	}
}

// TestPartialFrameWithWriteCloseIsError: a truncated frame arrives while the
// output write fails with a closed pipe. The framing error must not be hidden.
func TestPartialFrameWithWriteCloseIsError(t *testing.T) {
	c := newBudgetConn(2)
	done := make(chan error, 1)
	go func() { done <- Serve(c) }()
	feedHello(t, c)
	feedOpen(t, c)
	c.feed(append(rawHeader(byte(agentproto.TypeInput), 0, 1, 4), 'a', 'b'))
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("partial frame was reported as a clean EOF")
		}
	case <-time.After(3 * time.Second):
		c.Close()
		t.Fatal("agent did not finish")
	}
}

func TestLateCloseAfterNaturalExit(t *testing.T) {
	h := startAgent(t)
	h.open(1, 24, 80)
	h.input(1, "exit 7\n")
	reason, code := h.exited(1)
	if reason != agentproto.ReasonShellExit || code != 7 {
		t.Fatalf("reason=%d code=%d", reason, code)
	}
	// A late and duplicate Close for a reaped session must be ignored.
	h.send(agentproto.Frame{Type: agentproto.TypeClose, Session: 1})
	h.send(agentproto.Frame{Type: agentproto.TypeClose, Session: 1})
	h.send(agentproto.Frame{Type: agentproto.TypePing, Control: 9, Payload: agentproto.NoncePayload(3)})
	f := h.recv()
	if f.Type != agentproto.TypePong || f.Control != 9 {
		t.Fatalf("want Pong, got %+v", f)
	}
	if err := h.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if f, err := agentproto.ReadFrame(h.conn); err == nil {
		t.Fatalf("unexpected frame after Exited: %+v", f)
	}
}

func TestFinalFrameIsLast(t *testing.T) {
	sess := &session{}
	entered := make(chan struct{})
	release := make(chan struct{})
	var (
		mu    sync.Mutex
		order []string
	)
	outDone := make(chan error, 1)
	go func() {
		outDone <- sess.outputFrame(func() error {
			close(entered)
			<-release
			mu.Lock()
			order = append(order, "output")
			mu.Unlock()
			return nil
		})
	}()
	<-entered

	finalDone := make(chan struct{})
	go func() {
		sess.finalFrame(func() {
			mu.Lock()
			order = append(order, "final")
			mu.Unlock()
		})
		close(finalDone)
	}()
	select {
	case <-finalDone:
		t.Fatal("final frame overtook an in-flight output frame")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-outDone; err != nil {
		t.Fatal(err)
	}
	<-finalDone
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "output" || got[1] != "final" {
		t.Fatalf("order=%v", got)
	}
	if err := sess.outputFrame(func() error { t.Error("output after final"); return nil }); err != errSessionFinal {
		t.Fatalf("late output error=%v", err)
	}
	if sess.finalFrame(func() { t.Error("duplicate final") }) {
		t.Fatal("second final frame was sent")
	}
}

// TestSlowWirePreservesTail: the host reads Output slowly (100 ms per frame)
// while the shell writes 64 KiB and exits. Every byte and the real exit code
// must survive; the drain follows output progress, not an absolute deadline.
func TestSlowWirePreservesTail(t *testing.T) {
	h := startAgent(t) // unbuffered net.Pipe: its Writes block on our Reads
	h.open(1, 24, 80)
	h.input(1, "head -c 65536 /dev/zero; exit 7\n")

	var (
		out    []byte
		reason uint8
		code   int32
	)
	deadline := time.Now().Add(25 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("tail not delivered; have %d bytes", len(out))
		}
		f := h.recv()
		switch f.Type {
		case agentproto.TypeOutput:
			if f.Session != 1 {
				t.Fatalf("output for session %d", f.Session)
			}
			out = append(out, f.Payload...)
			time.Sleep(200 * time.Millisecond)
		case agentproto.TypeExited:
			reason, code, _ = agentproto.ParseExited(f.Payload)
			goto done
		case agentproto.TypeError:
			c, msg, _ := agentproto.ParseError(f.Payload)
			t.Fatalf("session error %d: %s", c, msg)
		default:
			t.Fatalf("unexpected frame %d", f.Type)
		}
	}
done:
	if reason != agentproto.ReasonShellExit || code != 7 {
		t.Fatalf("reason=%d code=%d", reason, code)
	}
	first := bytes.IndexByte(out, 0)
	last := bytes.LastIndexByte(out, 0)
	if first < 0 {
		t.Fatal("no NUL output")
	}
	run := out[first : last+1]
	if len(run) != 65536 {
		t.Fatalf("delivered %d of 65536 NUL bytes", len(run))
	}
	want := sha256.Sum256(make([]byte, 65536))
	if got := sha256.Sum256(run); got != want {
		t.Fatalf("payload hash mismatch")
	}
}

// TestContinuousDrainCapsWithTimeoutError: output that keeps arriving after
// the shell exits must not be cut into a successful Exited.
func TestContinuousDrainCapsWithTimeoutError(t *testing.T) {
	hc, ac := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serve(ac, options{
			writeTimeout: 2 * time.Second,
			inputTimeout: 2 * time.Second,
			exitWait:     2 * time.Second,
			exitDrain:    100 * time.Millisecond,
			exitDrainMax: 400 * time.Millisecond,
		})
	}()
	h := &host{t: t, conn: hc, done: done}
	t.Cleanup(func() {
		_ = hc.Close()
		h.wait(t, 5*time.Second)
	})
	h.hello(1)
	h.open(1, 24, 80)
	h.input(1, "sh -c 'for i in $(seq 1 100); do printf X; sleep 0.01; done' & exit 3\n")

	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("continuous post-exit output was not capped")
		}
		f := h.recv()
		switch f.Type {
		case agentproto.TypeOutput:
			continue
		case agentproto.TypeExited:
			t.Fatal("continuous output ended as a successful Exited")
		case agentproto.TypeError:
			code, _, err := agentproto.ParseError(f.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if f.Session != 1 || code != agentproto.CodeTimeout {
				t.Fatalf("want session timeout Error, got session=%d code=%d", f.Session, code)
			}
			return
		default:
			t.Fatalf("unexpected frame %d", f.Type)
		}
	}
}
