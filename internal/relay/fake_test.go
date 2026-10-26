package relay

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark4653/boxdwel/internal/agentproto"
)

func testOpts() options {
	o := defaults()
	o.handshakeTimeout = 2 * time.Second
	o.writeTimeout = 2 * time.Second
	o.pingInterval = time.Hour
	o.pongTimeout = time.Second
	return o
}

// fakeAgent answers the handshake and session control so tests can inject
// guest frames and observe what the relay sent.
type fakeAgent struct {
	conn       net.Conn
	ready      []byte
	in         chan agentproto.Frame
	got        chan agentproto.Frame
	noPong     bool
	bad        bool // reply with a wrong nonce
	noOpened   bool
	noExited   bool
	badOpened  bool
	holdMu     sync.Mutex
	release    chan struct{}
	startReads chan struct{}
	stop       chan struct{}
	done       chan struct{}
}

func startFake(t *testing.T, ready []byte) (*Relay, *fakeAgent) {
	t.Helper()
	a, b := net.Pipe()
	fa := &fakeAgent{
		conn:  a,
		ready: ready,
		in:    make(chan agentproto.Frame, 32),
		got:   make(chan agentproto.Frame, 64),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go fa.run()
	r, err := newRelay(b, testOpts())
	if err != nil {
		_ = a.Close()
		t.Fatalf("relay: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = r.Wait()
		_ = a.Close()
	})
	return r, fa
}

func (f *fakeAgent) run() {
	defer close(f.done)
	defer func() {
		if f.stop != nil {
			close(f.stop)
		}
	}()
	hello, err := agentproto.ReadFrame(f.conn)
	if err != nil || hello.Type != agentproto.TypeHello {
		return
	}
	if err := agentproto.WriteFrame(f.conn, agentproto.Frame{
		Type: agentproto.TypeReady, Control: hello.Control, Payload: f.ready,
	}); err != nil {
		return
	}
	if f.startReads != nil {
		select {
		case <-f.startReads:
		case <-f.stop:
			return
		}
	}
	read := make(chan agentproto.Frame)
	go func() {
		defer close(read)
		for {
			if rel := f.gate(); rel != nil {
				select {
				case <-rel:
				case <-f.stop:
					return
				}
			}
			fr, err := agentproto.ReadFrame(f.conn)
			if err != nil {
				return
			}
			select {
			case read <- fr:
			case <-f.stop:
				return
			}
		}
	}()
	for {
		select {
		case fr, ok := <-read:
			if !ok {
				return
			}
			select {
			case f.got <- fr:
			default:
			}
			switch fr.Type {
			case agentproto.TypePing:
				if f.noPong {
					continue
				}
				nonce, _ := agentproto.ParseNonce(fr.Payload)
				if f.bad {
					nonce++
				}
				_ = agentproto.WriteFrame(f.conn, agentproto.Frame{
					Type: agentproto.TypePong, Control: fr.Control, Payload: agentproto.NoncePayload(nonce),
				})
			case agentproto.TypeOpen:
				if f.noOpened {
					continue
				}
				opened := fr.Payload
				if f.badOpened {
					opened, _ = agentproto.Size(1, 1)
				}
				_ = agentproto.WriteFrame(f.conn, agentproto.Frame{
					Type: agentproto.TypeOpened, Session: fr.Session, Payload: opened,
				})
			case agentproto.TypeInput:
				_ = agentproto.WriteFrame(f.conn, agentproto.Frame{
					Type: agentproto.TypeOutput, Session: fr.Session, Payload: fr.Payload,
				})
			case agentproto.TypeClose:
				if f.noExited {
					continue
				}
				_ = agentproto.WriteFrame(f.conn, agentproto.Frame{
					Type: agentproto.TypeExited, Session: fr.Session,
					Payload: agentproto.ExitedPayload(agentproto.ReasonClosed, -1),
				})
			}
		case fr := <-f.in:
			if err := agentproto.WriteFrame(f.conn, fr); err != nil {
				return
			}
		}
	}
}

func (f *fakeAgent) inject(fr agentproto.Frame) {
	f.in <- fr
}

func (f *fakeAgent) waitFrame(t *testing.T, typ agentproto.Type, d time.Duration) agentproto.Frame {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case fr := <-f.got:
			if fr.Type == typ {
				return fr
			}
		case <-deadline:
			t.Fatalf("guest never received frame %d", typ)
		}
	}
}

func TestGuestHandshakeFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		typ   agentproto.Type
		ctrl  uint16
		ready []byte
	}{
		{"not ready", agentproto.TypeOpened, 1, nil},
		{"wrong control", agentproto.TypeReady, 2, agentproto.ReadyPayload()},
		{"one session", agentproto.TypeReady, 1, []byte{1, 0, 0x10, 0, 0x04, 0}},
		{"small payload", agentproto.TypeReady, 1, []byte{2, 0, 0x04, 0, 0x04, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			go func() {
				_, _ = agentproto.ReadFrame(a)
				_ = agentproto.WriteFrame(a, agentproto.Frame{Type: tc.typ, Control: tc.ctrl, Payload: tc.ready})
				_ = a.Close()
			}()
			r, err := newRelay(b, testOpts())
			if err == nil {
				_ = r.Close()
				_ = r.Wait()
				t.Fatal("handshake was accepted")
			}
		})
	}
}

func TestGuestHandshakeTimeout(t *testing.T) {
	a, b := net.Pipe()
	release := make(chan struct{})
	go func() {
		_, _ = agentproto.ReadFrame(a) // consume Hello, never reply
		<-release
	}()
	opt := testOpts()
	opt.handshakeTimeout = 50 * time.Millisecond
	start := time.Now()
	_, err := newRelay(b, opt)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
	close(release)
	_ = a.Close()
}

func TestGuestPingFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fakeAgent)
		want string
	}{
		{"timeout", func(f *fakeAgent) { f.noPong = true }, "pong timeout"},
		{"nonce", func(f *fakeAgent) { f.bad = true }, "nonce"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			fa := &fakeAgent{conn: a, ready: agentproto.ReadyPayload(),
				in: make(chan agentproto.Frame, 1), got: make(chan agentproto.Frame, 8), done: make(chan struct{})}
			tc.set(fa)
			go fa.run()
			opt := testOpts()
			opt.pingInterval = 20 * time.Millisecond
			opt.pongTimeout = 100 * time.Millisecond
			r, err := newRelay(b, opt)
			if err != nil {
				t.Fatal(err)
			}
			err = r.Wait()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			_ = a.Close()
		})
	}
}

func TestUnknownGuestSessionFails(t *testing.T) {
	r, fa := startFake(t, agentproto.ReadyPayload())
	fa.inject(agentproto.Frame{Type: agentproto.TypeOutput, Session: 99, Payload: []byte("x")})
	if err := r.Wait(); err == nil {
		t.Fatal("unknown guest session was accepted")
	}
}

func TestLateGuestFrameDropped(t *testing.T) {
	r, fa := startFake(t, agentproto.ReadyPayload())
	a := attachClient(t, r, 10, 24, 80)
	fa.inject(agentproto.Frame{Type: agentproto.TypeOutput, Session: 1, Payload: []byte("tail")})
	if out := a.outputUntil("tail"); !strings.Contains(out, "tail") {
		t.Fatal(out)
	}
	fa.inject(agentproto.Frame{Type: agentproto.TypeExited, Session: 1,
		Payload: agentproto.ExitedPayload(agentproto.ReasonShellExit, 4)})
	if reason, code := a.exited(); reason != agentproto.ReasonShellExit || code != 4 {
		t.Fatalf("reason=%d code=%d", reason, code)
	}
	a.expectClosed(2 * time.Second)

	// The relay survives and a new client gets a new guest id.
	b := attachClient(t, r, 11, 24, 80)
	fa.inject(agentproto.Frame{Type: agentproto.TypeOutput, Session: 1, Payload: []byte("late")})
	fa.inject(agentproto.Frame{Type: agentproto.TypeOutput, Session: 2, Payload: []byte("fresh")})
	if out := b.outputUntil("fresh"); strings.Contains(out, "late") {
		t.Fatalf("late frame leaked to a new client: %q", out)
	}
}

func TestThirdClientBusy(t *testing.T) {
	r, _ := startFake(t, agentproto.ReadyPayload())
	attachClient(t, r, 1, 24, 80)
	attachClient(t, r, 2, 24, 80)

	cs, rs := net.Pipe()
	defer cs.Close()
	if err := r.Attach(rs); !errors.Is(err, ErrBusy) {
		t.Fatalf("third attach err=%v", err)
	}
}

func TestAttachAfterClose(t *testing.T) {
	r, _ := startFake(t, agentproto.ReadyPayload())
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Wait(); err != nil {
		t.Fatalf("clean close returned %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	cs, rs := net.Pipe()
	defer cs.Close()
	if err := r.Attach(rs); !errors.Is(err, ErrClosed) {
		t.Fatalf("attach after close err=%v", err)
	}
}

func TestGuestIDExhaustion(t *testing.T) {
	r := &Relay{nextGuest: 65535}
	id, err := r.allocGuest()
	if err != nil || id != 65535 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if _, err := r.allocGuest(); err == nil {
		t.Fatal("id space wrapped instead of failing")
	}
}

func TestClientWrongSessionClosesOnlyThatClient(t *testing.T) {
	r, _ := startFake(t, agentproto.ReadyPayload())
	a := attachClient(t, r, 5, 24, 80)
	b := attachClient(t, r, 6, 24, 80)

	// The relay closes this client as soon as it reads the foreign id, so the
	// closing write may race with the close; only the close is asserted.
	var bad bytes.Buffer
	_ = agentproto.WriteFrame(&bad, agentproto.Frame{Type: agentproto.TypeInput, Session: 99, Payload: []byte("x")})
	_ = a.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = a.conn.Write(bad.Bytes())
	a.expectClosed(2 * time.Second)

	b.input("printf 'B%s\\n' _OK\n")
	// The fake agent does not run a shell, so only delivery is checked.
	fr := waitForType(t, b, agentproto.TypeOutput, 2*time.Second)
	if fr.Session != 6 {
		t.Fatalf("frame for session %d", fr.Session)
	}
}

func waitForType(t *testing.T, c *testClient, typ agentproto.Type, d time.Duration) agentproto.Frame {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := c.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		f, err := agentproto.ReadFrame(c.conn)
		if err != nil {
			continue
		}
		if f.Type == typ {
			return f
		}
	}
	t.Fatalf("no frame %d", typ)
	return agentproto.Frame{}
}

func TestAttachAndCloseRace(t *testing.T) {
	r, _ := startFake(t, agentproto.ReadyPayload())
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			cs, rs := net.Pipe()
			if err := r.Attach(rs); err != nil {
				_ = cs.Close()
				return
			}
			_ = cs.Close()
		}()
	}
	go r.Close()
	for i := 0; i < 8; i++ {
		<-done
	}
	if err := r.Wait(); err != nil {
		t.Fatalf("close race returned %v", err)
	}
}

func TestClientBadVersionOnlyClosesThatClient(t *testing.T) {
	r, _ := startFake(t, agentproto.ReadyPayload())
	good := attachClient(t, r, 1, 24, 80)

	cs, rs := net.Pipe()
	if err := r.Attach(rs); err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	raw := []byte{'B', 'w', 2, byte(agentproto.TypeHello), 0, 1, 0, 0, 0, 0}
	_ = cs.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = cs.Write(raw)
	_ = cs.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := cs.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("bad-version client received data")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("bad-version client stayed open")
	}

	good.input("ok")
	fr := waitForType(t, good, agentproto.TypeOutput, 2*time.Second)
	if fr.Session != 1 {
		t.Fatalf("frame for session %d", fr.Session)
	}
}

func TestSecondOpenClosesThatClient(t *testing.T) {
	r, _ := startFake(t, agentproto.ReadyPayload())
	a := attachClient(t, r, 4, 24, 80)
	var second bytes.Buffer
	p, _ := agentproto.Size(24, 80)
	_ = agentproto.WriteFrame(&second, agentproto.Frame{Type: agentproto.TypeOpen, Session: 4, Payload: p})
	_ = a.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = a.conn.Write(second.Bytes())
	a.expectClosed(2 * time.Second)
}

func TestGuestWriteFailureStopsRelay(t *testing.T) {
	a, b := net.Pipe()
	ready := make(chan struct{})
	go func() {
		_, _ = agentproto.ReadFrame(a)
		_ = agentproto.WriteFrame(a, agentproto.Frame{
			Type: agentproto.TypeReady, Control: 1, Payload: agentproto.ReadyPayload(),
		})
		close(ready)
		// Never read another frame, so the guest writer blocks.
		time.Sleep(2 * time.Second)
		_ = a.Close()
	}()
	opt := testOpts()
	opt.writeTimeout = 100 * time.Millisecond
	r, err := newRelay(b, opt)
	if err != nil {
		t.Fatal(err)
	}
	<-ready
	cs, rs := net.Pipe()
	defer cs.Close()
	if err := r.Attach(rs); err != nil {
		t.Fatal(err)
	}
	_ = agentproto.WriteFrame(cs, agentproto.Frame{Type: agentproto.TypeHello, Control: 1})
	if _, err := agentproto.ReadFrame(cs); err != nil {
		t.Fatal(err)
	}
	p, _ := agentproto.Size(24, 80)
	_ = cs.SetWriteDeadline(time.Now().Add(time.Second))
	if err := agentproto.WriteFrame(cs, agentproto.Frame{Type: agentproto.TypeOpen, Session: 1, Payload: p}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = r.Wait()
	if err == nil {
		t.Fatal("blocked guest write was reported as a clean close")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("guest write failure was not bounded: %s", time.Since(start))
	}
}

func (f *fakeAgent) hold() {
	ch := make(chan struct{})
	f.holdMu.Lock()
	f.release = ch
	f.holdMu.Unlock()
}

func (f *fakeAgent) unhold() {
	f.holdMu.Lock()
	if f.release != nil {
		close(f.release)
		f.release = nil
	}
	f.holdMu.Unlock()
}

func (f *fakeAgent) gate() chan struct{} {
	f.holdMu.Lock()
	defer f.holdMu.Unlock()
	return f.release
}

// attachRawOpen performs Hello/Ready/Open but does not wait for Opened.
func attachRawOpen(t *testing.T, r *Relay, local, rows, cols uint16) *testClient {
	t.Helper()
	cs, rs := net.Pipe()
	if err := r.Attach(rs); err != nil {
		t.Fatalf("attach: %v", err)
	}
	c := &testClient{t: t, conn: cs, local: local}
	t.Cleanup(func() { _ = cs.Close() })
	c.send(agentproto.Frame{Type: agentproto.TypeHello, Control: 1})
	if f := c.recv(5 * time.Second); f.Type != agentproto.TypeReady {
		t.Fatalf("want Ready, got %+v", f)
	}
	p, _ := agentproto.Size(rows, cols)
	c.send(agentproto.Frame{Type: agentproto.TypeOpen, Session: local, Payload: p})
	return c
}

func waitError(t *testing.T, r *Relay, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		return nil
	}
}

func TestDuplicateCloseDoesNotExhaustControl(t *testing.T) {
	r, fa := startFake(t, agentproto.ReadyPayload())
	a := attachClient(t, r, 1, 24, 80)
	b := attachClient(t, r, 2, 24, 80)
	fa.hold()

	var clos bytes.Buffer
	_ = agentproto.WriteFrame(&clos, agentproto.Frame{Type: agentproto.TypeClose, Session: 1})
	for i := 0; i < 12; i++ {
		_ = a.conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := a.conn.Write(clos.Bytes()); err != nil {
			break
		}
	}
	b.ping(9, 1)
	fa.unhold()
	a.expectClosed(2 * time.Second)
	b.input("ok")
	if fr := waitForType(t, b, agentproto.TypeOutput, 2*time.Second); fr.Session != 2 {
		t.Fatalf("frame for session %d", fr.Session)
	}
}

func TestInputFloodIsolatedByClientBudget(t *testing.T) {
	r, fa := startFake(t, agentproto.ReadyPayload())
	a := attachClient(t, r, 1, 24, 80)
	b := attachClient(t, r, 2, 24, 80)
	fa.hold()

	var flood bytes.Buffer
	_ = agentproto.WriteFrame(&flood, agentproto.Frame{
		Type: agentproto.TypeInput, Session: 1, Payload: bytes.Repeat([]byte{'x'}, 64),
	})
	for i := 0; i < 80; i++ {
		_ = a.conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := a.conn.Write(flood.Bytes()); err != nil {
			break
		}
	}
	a.expectClosed(2 * time.Second)
	b.ping(9, 1)
	b.input("ok")
	fa.unhold()
	if fr := waitForType(t, b, agentproto.TypeOutput, 2*time.Second); fr.Session != 2 {
		t.Fatalf("frame for session %d", fr.Session)
	}
}

func TestOpenConfirmationTimeout(t *testing.T) {
	a, b := net.Pipe()
	fa := &fakeAgent{conn: a, ready: agentproto.ReadyPayload(), noOpened: true,
		in: make(chan agentproto.Frame, 1), got: make(chan agentproto.Frame, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	go fa.run()
	opt := testOpts()
	opt.confirmTimeout = 100 * time.Millisecond
	r, err := newRelay(b, opt)
	if err != nil {
		t.Fatal(err)
	}
	_ = attachRawOpen(t, r, 1, 24, 80)
	if err := waitError(t, r, time.Second); err == nil {
		t.Fatal("missing Opened confirmation was not enforced")
	}
}

func TestCloseConfirmationTimeout(t *testing.T) {
	a, b := net.Pipe()
	fa := &fakeAgent{conn: a, ready: agentproto.ReadyPayload(), noExited: true,
		in: make(chan agentproto.Frame, 1), got: make(chan agentproto.Frame, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	go fa.run()
	opt := testOpts()
	opt.confirmTimeout = 100 * time.Millisecond
	r, err := newRelay(b, opt)
	if err != nil {
		t.Fatal(err)
	}
	c := attachClient(t, r, 1, 24, 80)
	c.closeSession()
	if err := waitError(t, r, time.Second); err == nil {
		t.Fatal("missing Exited confirmation was not enforced")
	}
}

func TestPingTimerStartsAfterWrite(t *testing.T) {
	a, b := net.Pipe()
	start := make(chan struct{})
	fa := &fakeAgent{conn: a, ready: agentproto.ReadyPayload(), startReads: start,
		in: make(chan agentproto.Frame, 1), got: make(chan agentproto.Frame, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	go fa.run()
	opt := testOpts()
	opt.pingInterval = 50 * time.Millisecond
	opt.pongTimeout = 100 * time.Millisecond
	opt.writeTimeout = 2 * time.Second
	r, err := newRelay(b, opt)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // longer than pongTimeout, write is blocked
	close(start)
	time.Sleep(300 * time.Millisecond)
	select {
	case <-r.done:
		t.Fatalf("pong timer started before the write completed: %v", r.Wait())
	default:
	}
	_ = r.Close()
	_ = r.Wait()
}

func TestGuestOpenedSizeMismatchFails(t *testing.T) {
	a, b := net.Pipe()
	fa := &fakeAgent{conn: a, ready: agentproto.ReadyPayload(), badOpened: true,
		in: make(chan agentproto.Frame, 1), got: make(chan agentproto.Frame, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	go fa.run()
	r, err := newRelay(b, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = attachRawOpen(t, r, 1, 24, 80)
	if err := waitError(t, r, time.Second); err == nil {
		t.Fatal("mismatched Opened size was accepted")
	}
}

func TestOutputBeforeOpenedFails(t *testing.T) {
	a, b := net.Pipe()
	fa := &fakeAgent{conn: a, ready: agentproto.ReadyPayload(), noOpened: true,
		in: make(chan agentproto.Frame, 1), got: make(chan agentproto.Frame, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	go fa.run()
	r, err := newRelay(b, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = attachRawOpen(t, r, 1, 24, 80)
	fa.inject(agentproto.Frame{Type: agentproto.TypeOutput, Session: 1, Payload: []byte("x")})
	if err := waitError(t, r, time.Second); err == nil {
		t.Fatal("Output before Opened was accepted")
	}
}

func TestPreOpenedInputClosesOnlyThatClient(t *testing.T) {
	a, b := net.Pipe()
	fa := &fakeAgent{conn: a, ready: agentproto.ReadyPayload(), noOpened: true, noExited: true,
		in: make(chan agentproto.Frame, 1), got: make(chan agentproto.Frame, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	go fa.run()
	opt := testOpts()
	opt.confirmTimeout = time.Hour
	r, err := newRelay(b, opt)
	if err != nil {
		t.Fatal(err)
	}
	c := attachRawOpen(t, r, 1, 24, 80)
	c.input("x")
	c.expectClosed(2 * time.Second)
}

func TestStaleInputHeldUntilDrained(t *testing.T) {
	r, fa := startFake(t, agentproto.ReadyPayload())
	a := attachClient(t, r, 1, 24, 80)
	b := attachClient(t, r, 2, 24, 80) // keeps the other slot busy
	fa.hold()
	for i := 0; i < maxClientInputFrames; i++ {
		a.input("x")
	}
	fa.inject(agentproto.Frame{Type: agentproto.TypeExited, Session: 1,
		Payload: agentproto.ExitedPayload(agentproto.ReasonShellExit, 0)})
	if reason, code := a.exited(); reason != agentproto.ReasonShellExit || code != 0 {
		t.Fatalf("reason=%d code=%d", reason, code)
	}

	// The other slot is busy and the old client's queued input still occupies
	// the shared queue, so no attach may succeed until it drains.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		cs, rs := net.Pipe()
		err := r.Attach(rs)
		_ = cs.Close()
		if err == nil {
			t.Fatal("slot was reused while stale input was still queued")
		}
		time.Sleep(10 * time.Millisecond)
	}

	fa.unhold()
	c := attachRetry(t, r, 3, 24, 80, 3*time.Second)
	c.input("C_OK")
	c.outputUntil("C_OK")
	c.closeSession()
	c.exited()
	_ = b
}

func TestAttachAfterCloseWithFullSlots(t *testing.T) {
	r, _ := startFake(t, agentproto.ReadyPayload())
	attachClient(t, r, 1, 24, 80)
	attachClient(t, r, 2, 24, 80)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Wait(); err != nil {
		t.Fatalf("close returned %v", err)
	}
	cs, rs := net.Pipe()
	defer cs.Close()
	if err := r.Attach(rs); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed with full slots, got %v", err)
	}
}

func TestAttachCloseWaitGate(t *testing.T) {
	for i := 0; i < 20; i++ {
		r, _ := startFake(t, agentproto.ReadyPayload())
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cs, rs := net.Pipe()
				_ = r.Attach(rs)
				_ = cs.Close()
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Close()
		}()
		wg.Wait()
		if err := r.Wait(); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}
