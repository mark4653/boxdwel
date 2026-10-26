//go:build linux

package relay

import (
	"bytes"
	"crypto/sha256"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mark4653/boxdwel/internal/agent"
	"github.com/mark4653/boxdwel/internal/agentproto"
)

func startAgentRelay(t *testing.T) (*Relay, net.Conn) {
	t.Helper()
	guest, agentSide := net.Pipe()
	go func() { _ = agent.Serve(agentSide) }()
	r, err := New(guest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = r.Wait()
		_ = agentSide.Close()
	})
	return r, agentSide
}

func TestTwoClientsIndependent(t *testing.T) {
	r, _ := startAgentRelay(t)
	a := attachClient(t, r, 1, 24, 80)
	b := attachClient(t, r, 2, 40, 100)

	a.input("printf 'A%s\\n' _ONE\n")
	if out := a.outputUntil("A_ONE"); !strings.Contains(out, "A_ONE") {
		t.Fatal(out)
	}
	b.input("printf 'B%s\\n' _TWO\n")
	if out := b.outputUntil("B_TWO"); !strings.Contains(out, "B_TWO") {
		t.Fatal(out)
	}

	a.input("printf '한글%s\\n' '-✓'\n")
	if out := a.outputUntil("한글-✓"); !strings.Contains(out, "한글-✓") {
		t.Fatal(out)
	}

	a.resize(30, 90)
	a.input("stty size\n")
	if out := a.outputUntil("30 90"); !strings.Contains(out, "30 90") {
		t.Fatal(out)
	}
	b.input("stty size\n")
	if out := b.outputUntil("40 100"); !strings.Contains(out, "40 100") {
		t.Fatal(out)
	}

	a.input("X=alpha; printf 'X%s\\n' $X\n")
	a.outputUntil("Xalpha")
	b.input("printf 'X%s\\n' ${X:-unset}\n")
	if out := b.outputUntil("Xunset"); strings.Contains(out, "Xalpha") {
		t.Fatalf("shell state leaked: %q", out)
	}

	a.input("exit 5\n")
	if reason, code := a.exited(); reason != agentproto.ReasonShellExit || code != 5 {
		t.Fatalf("a: reason=%d code=%d", reason, code)
	}
	b.input("printf 'B%s\\n' _ALIVE\n")
	if out := b.outputUntil("B_ALIVE"); !strings.Contains(out, "B_ALIVE") {
		t.Fatalf("b did not survive: %q", out)
	}
	b.input("exit 0\n")
	if reason, code := b.exited(); reason != agentproto.ReasonShellExit || code != 0 {
		t.Fatalf("b: reason=%d code=%d", reason, code)
	}

	// The relay survives the last client and accepts a new one.
	c := attachRetry(t, r, 3, 24, 80, 3*time.Second)
	c.input("printf 'C%s\\n' _NEW\n")
	if out := c.outputUntil("C_NEW"); !strings.Contains(out, "C_NEW") {
		t.Fatal(out)
	}
	c.input("exit\n")
	c.exited()
}

func TestTailBytesThroughRelay(t *testing.T) {
	r, _ := startAgentRelay(t)
	c := attachClient(t, r, 1, 24, 80)
	c.input("head -c 65536 /dev/zero; exit 7\n")

	var out []byte
	var (
		reason uint8
		code   int32
	)
	deadline := time.Now().Add(25 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("tail not delivered; have %d bytes", len(out))
		}
		f := c.recv(20 * time.Second)
		switch f.Type {
		case agentproto.TypeOutput:
			out = append(out, f.Payload...)
		case agentproto.TypeExited:
			reason, code, _ = agentproto.ParseExited(f.Payload)
			goto done
		case agentproto.TypeError:
			_, msg, _ := agentproto.ParseError(f.Payload)
			t.Fatalf("session error: %s", msg)
		default:
			t.Fatalf("unexpected frame %d", f.Type)
		}
	}
done:
	if reason != agentproto.ReasonShellExit || code != 7 {
		t.Fatalf("reason=%d code=%d", reason, code)
	}
	first, last := bytes.IndexByte(out, 0), bytes.LastIndexByte(out, 0)
	if first < 0 {
		t.Fatal("no NUL payload")
	}
	run := out[first : last+1]
	if len(run) != 65536 {
		t.Fatalf("delivered %d of 65536 NUL bytes", len(run))
	}
	want := sha256.Sum256(make([]byte, 65536))
	if got := sha256.Sum256(run); got != want {
		t.Fatal("payload hash mismatch")
	}
}

func TestClientDisconnectIsolation(t *testing.T) {
	r, _ := startAgentRelay(t)
	a := attachClient(t, r, 1, 24, 80)
	b := attachClient(t, r, 2, 24, 80)

	_ = a.conn.Close()

	b.input("printf 'B%s\\n' _OK\n")
	b.outputUntil("B_OK")

	// The slot is released when the guest confirms the closed session.
	c := attachRetry(t, r, 3, 24, 80, 3*time.Second)
	c.input("printf 'C%s\\n' _OK\n")
	c.outputUntil("C_OK")

	b.input("exit\n")
	b.exited()
	c.input("exit\n")
	c.exited()
}

func TestSlowClientIsolated(t *testing.T) {
	r, _ := startAgentRelay(t)
	a := attachClient(t, r, 1, 24, 80)
	b := attachClient(t, r, 2, 24, 80)

	// A never reads its output again.
	a.input("head -c 200000 /dev/zero\n")

	b.input("printf 'B%s\\n' _OK\n")
	b.outputUntil("B_OK")
	b.ping(7, 42)

	a.expectClosed(3 * time.Second)

	b.input("exit\n")
	b.exited()
}

func TestGuestFailureClosesClients(t *testing.T) {
	r, agentSide := startAgentRelay(t)
	a := attachClient(t, r, 1, 24, 80)
	b := attachClient(t, r, 2, 24, 80)

	_ = agentSide.Close()

	done := make(chan error, 1)
	go func() { done <- r.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("guest failure was reported as a clean close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guest failure did not stop the relay")
	}
	a.expectClosed(2 * time.Second)
	b.expectClosed(2 * time.Second)
}

func TestClientProtocolViolationOnlyClosesThatClient(t *testing.T) {
	r, _ := startAgentRelay(t)
	a := attachClient(t, r, 1, 24, 80)
	b := attachClient(t, r, 2, 24, 80)

	// A client may not send a guest-only frame.
	var bad bytes.Buffer
	_ = agentproto.WriteFrame(&bad, agentproto.Frame{Type: agentproto.TypeOutput, Session: 1, Payload: []byte("x")})
	_ = a.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = a.conn.Write(bad.Bytes())
	a.expectClosed(2 * time.Second)

	b.input("printf 'B%s\\n' _OK\n")
	if out := b.outputUntil("B_OK"); !strings.Contains(out, "B_OK") {
		t.Fatalf("b did not survive: %q", out)
	}
	b.input("exit\n")
	b.exited()
}

func TestIdleLivenessWithRealAgent(t *testing.T) {
	guest, agentSide := net.Pipe()
	go func() { _ = agent.Serve(agentSide) }()
	opt := defaults()
	opt.pingInterval = 50 * time.Millisecond
	opt.pongTimeout = 500 * time.Millisecond
	r, err := newRelay(guest, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = r.Wait()
		_ = agentSide.Close()
	})

	time.Sleep(300 * time.Millisecond) // several pings while idle
	select {
	case <-r.done:
		t.Fatalf("idle relay stopped: %v", r.Wait())
	default:
	}

	c := attachClient(t, r, 1, 24, 80)
	c.input("printf 'L%s\\n' _OK\n")
	c.outputUntil("L_OK")
	c.input("exit\n")
	c.exited()
}

func TestInvalidClientResizeIsolated(t *testing.T) {
	r, _ := startAgentRelay(t)
	a := attachClient(t, r, 1, 24, 80)
	b := attachClient(t, r, 2, 24, 80)

	bad, _ := agentproto.Size(24, 80)
	bad[0], bad[1], bad[2], bad[3] = 0, 0, 0, 0 // zero rows/columns
	a.send(agentproto.Frame{Type: agentproto.TypeResize, Session: 1, Payload: bad})
	a.expectClosed(2 * time.Second)

	b.input("printf 'B%s\\n' _OK\n")
	if out := b.outputUntil("B_OK"); !strings.Contains(out, "B_OK") {
		t.Fatalf("agent did not survive the invalid resize: %q", out)
	}
	b.input("exit\n")
	b.exited()
}

func TestSlotHeldUntilFinalWriterDone(t *testing.T) {
	guest, agentSide := net.Pipe()
	go func() { _ = agent.Serve(agentSide) }()
	opt := defaults()
	opt.writeTimeout = 300 * time.Millisecond
	r, err := newRelay(guest, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = r.Wait()
		_ = agentSide.Close()
	})

	a := attachClient(t, r, 1, 24, 80)
	_ = attachClient(t, r, 2, 24, 80) // keeps the second slot busy
	a.input("exit\n")
	time.Sleep(100 * time.Millisecond) // let the final reach the blocked writer

	cs, rs := net.Pipe()
	err = r.Attach(rs)
	if err == nil {
		_ = cs.Close()
		t.Fatal("slot was released before the final frame was written")
	}
	_ = cs.Close()

	var c *testClient
	deadline := time.Now().Add(3 * time.Second)
	for c == nil {
		cs, rs := net.Pipe()
		if err := r.Attach(rs); err == nil {
			c = &testClient{t: t, conn: cs, local: 3}
			t.Cleanup(func() { _ = cs.Close() })
			c.send(agentproto.Frame{Type: agentproto.TypeHello, Control: 1})
			if f := c.recv(5 * time.Second); f.Type != agentproto.TypeReady {
				t.Fatalf("want Ready, got %+v", f)
			}
			p, _ := agentproto.Size(24, 80)
			c.send(agentproto.Frame{Type: agentproto.TypeOpen, Session: 3, Payload: p})
			if f := c.recv(5 * time.Second); f.Type != agentproto.TypeOpened {
				t.Fatalf("want Opened, got %+v", f)
			}
			break
		}
		_ = cs.Close()
		if time.Now().After(deadline) {
			t.Fatal("slot was never released")
		}
		time.Sleep(20 * time.Millisecond)
	}

	c.input("printf 'C%s\\n' _OK\n")
	c.outputUntil("C_OK")
	c.input("exit\n")
	c.exited()
}
