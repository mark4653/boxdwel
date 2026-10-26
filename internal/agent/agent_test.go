//go:build linux

package agent

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/mark4653/boxdwel/internal/agentproto"
)

type host struct {
	t     *testing.T
	conn  net.Conn
	done  chan error
	once  sync.Once
	err   error
	ready bool
}

func startAgent(t *testing.T) *host {
	t.Helper()
	a, b := net.Pipe()
	h := &host{t: t, conn: b, done: make(chan error, 1)}
	go func() { h.done <- Serve(a) }()
	t.Cleanup(func() {
		_ = b.Close()
		h.wait(t, 10*time.Second)
	})
	h.hello(1)
	return h
}

func (h *host) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	h.once.Do(func() {
		select {
		case h.err = <-h.done:
		case <-time.After(timeout):
			t.Fatal("agent did not stop")
		}
	})
	return h.err
}

func (h *host) send(f agentproto.Frame) {
	h.t.Helper()
	if err := h.conn.SetWriteDeadline(time.Now().Add(20 * time.Second)); err != nil {
		h.t.Fatal(err)
	}
	if err := agentproto.WriteFrame(h.conn, f); err != nil {
		h.t.Fatalf("send %d: %v", f.Type, err)
	}
}

func (h *host) recv() agentproto.Frame {
	h.t.Helper()
	if err := h.conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		h.t.Fatal(err)
	}
	f, err := agentproto.ReadFrame(h.conn)
	if err != nil {
		h.t.Fatalf("recv: %v", err)
	}
	return f
}

func (h *host) hello(control uint16) {
	h.t.Helper()
	h.send(agentproto.Frame{Type: agentproto.TypeHello, Control: control})
	f := h.recv()
	if f.Type != agentproto.TypeReady || f.Control != control {
		h.t.Fatalf("want Ready control %d, got %+v", control, f)
	}
	sessions, payload, output, err := agentproto.ParseReady(f.Payload)
	if err != nil || sessions != agentproto.MaxSessions || payload != agentproto.MaxPayload || output != agentproto.MaxOutputChunk {
		h.t.Fatalf("ready %d/%d/%d err=%v", sessions, payload, output, err)
	}
	h.ready = true
}

func (h *host) open(id uint16, rows, cols uint16) {
	h.t.Helper()
	p, err := agentproto.Size(rows, cols)
	if err != nil {
		h.t.Fatal(err)
	}
	h.send(agentproto.Frame{Type: agentproto.TypeOpen, Session: id, Payload: p})
	for {
		f := h.recv()
		if f.Type == agentproto.TypeOutput {
			continue // another session may already be running
		}
		if f.Type != agentproto.TypeOpened || f.Session != id || !bytes.Equal(f.Payload, p) {
			h.t.Fatalf("want Opened %d, got %+v", id, f)
		}
		return
	}
}

func (h *host) input(id uint16, s string) {
	h.t.Helper()
	h.send(agentproto.Frame{Type: agentproto.TypeInput, Session: id, Payload: []byte(s)})
}

func (h *host) resize(id uint16, rows, cols uint16) {
	h.t.Helper()
	p, err := agentproto.Size(rows, cols)
	if err != nil {
		h.t.Fatal(err)
	}
	h.send(agentproto.Frame{Type: agentproto.TypeResize, Session: id, Payload: p})
}

// outputUntil collects Output for one session until marker appears.
func (h *host) outputUntil(id uint16, marker string) string {
	h.t.Helper()
	var out strings.Builder
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f := h.recv()
		switch f.Type {
		case agentproto.TypeOutput:
			if f.Session != id {
				continue // another session's output may interleave
			}
			out.Write(f.Payload)
			if strings.Contains(out.String(), marker) {
				return out.String()
			}
		case agentproto.TypeError:
			code, msg, _ := agentproto.ParseError(f.Payload)
			h.t.Fatalf("session %d error %d: %s", f.Session, code, msg)
		default:
			h.t.Fatalf("unexpected %d while waiting for %q", f.Type, marker)
		}
	}
	h.t.Fatalf("marker %q not seen", marker)
	return ""
}

// exited reads until Exited, discarding output still in flight.
func (h *host) exited(id uint16) (uint8, int32) {
	h.t.Helper()
	for {
		f := h.recv()
		if f.Type == agentproto.TypeOutput {
			continue
		}
		if f.Type != agentproto.TypeExited || f.Session != id {
			h.t.Fatalf("want Exited %d, got %+v", id, f)
		}
		reason, code, err := agentproto.ParseExited(f.Payload)
		if err != nil {
			h.t.Fatal(err)
		}
		return reason, code
	}
}

func TestShellRoundTripUTF8AndExit(t *testing.T) {
	h := startAgent(t)
	h.open(1, 24, 80)

	h.input(1, "printf 'MARK%s\\n' _ONE\n")
	if out := h.outputUntil(1, "MARK_ONE"); !strings.Contains(out, "MARK_ONE") {
		t.Fatal(out)
	}

	h.input(1, "printf '한글%s\\n' '-✓'\n")
	if out := h.outputUntil(1, "한글-✓"); !strings.Contains(out, "한글-✓") {
		t.Fatal(out)
	}

	h.input(1, "exit 7\n")
	reason, code := h.exited(1)
	if reason != agentproto.ReasonShellExit || code != 7 {
		t.Fatalf("reason=%d code=%d", reason, code)
	}
	h.input(1, "ignored\n") // late frame for a retired session is discarded
	h.send(agentproto.Frame{Type: agentproto.TypePing, Control: 5, Payload: agentproto.NoncePayload(9)})
	f := h.recv()
	if f.Type != agentproto.TypePong || f.Control != 5 || !bytes.Equal(f.Payload, agentproto.NoncePayload(9)) {
		t.Fatalf("want Pong, got %+v", f)
	}
}

func TestResize(t *testing.T) {
	h := startAgent(t)
	h.open(1, 24, 80)
	h.resize(1, 40, 100)
	h.input(1, "stty size\n")
	h.outputUntil(1, "40 100")
	h.input(1, "exit\n")
	h.exited(1)
}

func TestCtrlCLeavesShellUsable(t *testing.T) {
	h := startAgent(t)
	h.open(1, 24, 80)
	// The foreground child prints its own marker before becoming sleep, so the
	// Ctrl+C below reaches a running foreground job.
	h.input(1, "printf 'BEFORE%s\\n' _CTRL_C; sh -c 'printf \"CHILD%s\\n\" _READY; exec sleep 30'\n")
	h.outputUntil(1, "CHILD_READY")
	h.input(1, "\x03")
	h.input(1, "printf 'AFTER%s\\n' _CTRL_C\n")
	h.outputUntil(1, "AFTER_CTRL_C")
	h.input(1, "exit\n")
	h.exited(1)
}

func TestCloseKillsSession(t *testing.T) {
	h := startAgent(t)
	h.open(1, 24, 80)
	h.input(1, "printf 'PID:%s\\n' $$; printf 'DONE%s\\n' _X\n")
	out := h.outputUntil(1, "DONE_X")
	pid := parsePID(t, out)

	start := time.Now()
	h.send(agentproto.Frame{Type: agentproto.TypeClose, Session: 1})
	reason, _ := h.exited(1)
	if reason != agentproto.ReasonClosed {
		t.Fatalf("reason=%d", reason)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("close was not bounded")
	}
	waitGone(t, pid)
}

func TestSessionLimitAndIsolation(t *testing.T) {
	h := startAgent(t)
	h.open(1, 24, 80)
	h.open(2, 24, 80)

	p, _ := agentproto.Size(24, 80)
	h.send(agentproto.Frame{Type: agentproto.TypeOpen, Session: 3, Payload: p})
	var f agentproto.Frame
	for {
		f = h.recv()
		if f.Type != agentproto.TypeOutput {
			break
		}
	}
	if f.Type != agentproto.TypeError || f.Session != 3 {
		t.Fatalf("want session Error, got %+v", f)
	}
	if code, _, _ := agentproto.ParseError(f.Payload); code != agentproto.CodeSessionLimit {
		t.Fatalf("code=%d", code)
	}

	// The refused id is not retired, so it can be opened after a slot frees.
	h.input(1, "printf 'A%s\\n' _OK\n")
	h.outputUntil(1, "A_OK")
	h.input(2, "printf 'B%s\\n' _OK\n")
	h.outputUntil(2, "B_OK")

	h.send(agentproto.Frame{Type: agentproto.TypeClose, Session: 1})
	h.exited(1)

	// Session 2 survives session 1's close and keeps writing.
	h.input(2, "printf 'B%s\\n' _ALIVE\n")
	h.outputUntil(2, "B_ALIVE")
	h.input(2, "exit\n")
	h.exited(2)

	h.open(3, 24, 80)
	h.input(3, "exit\n")
	h.exited(3)
}

func TestNoEnvironmentInheritance(t *testing.T) {
	t.Setenv("BOXDWEL_TEST_SECRET", "must-not-appear")
	h := startAgent(t)
	h.open(1, 24, 80)
	h.input(1, "printf 'ENV%s\\n' _BEGIN; env; printf 'ENV%s\\n' _END\n")
	out := h.outputUntil(1, "ENV_END")
	if strings.Contains(out, "BOXDWEL_TEST_SECRET") || strings.Contains(out, "must-not-appear") {
		t.Fatalf("environment leaked:\n%s", out)
	}
	for _, want := range []string{"TERM=xterm-256color", "SHELL=/bin/sh", "PATH="} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	h.input(1, "exit\n")
	h.exited(1)
}

func TestDuplicateRetiredIDClosesConnection(t *testing.T) {
	h := startAgent(t)
	h.open(1, 24, 80)
	h.input(1, "exit\n")
	h.exited(1)
	// A retired id cannot be opened again.
	p, _ := agentproto.Size(24, 80)
	h.send(agentproto.Frame{Type: agentproto.TypeOpen, Session: 1, Payload: p})
	f := h.recv()
	if f.Type != agentproto.TypeError || f.Session != 0 {
		t.Fatalf("want control Error, got %+v", f)
	}
	if err := h.wait(t, 5*time.Second); err == nil {
		t.Fatal("agent must close after a protocol error")
	}
}

func TestUnknownSessionClosesConnection(t *testing.T) {
	h := startAgent(t)
	h.input(9, "x")
	f := h.recv()
	if f.Type != agentproto.TypeError || f.Session != 0 {
		t.Fatalf("want control Error, got %+v", f)
	}
	if err := h.wait(t, 5*time.Second); err == nil {
		t.Fatal("agent must close after an unknown session")
	}
}

func TestBadOpenSizeClosesConnection(t *testing.T) {
	h := startAgent(t)
	h.send(agentproto.Frame{Type: agentproto.TypeOpen, Session: 1, Payload: []byte{0, 0, 0, 0}})
	f := h.recv()
	if f.Type != agentproto.TypeError {
		t.Fatalf("want Error, got %+v", f)
	}
	if err := h.wait(t, 5*time.Second); err == nil {
		t.Fatal("agent must close after bad dimensions")
	}
}

func TestEOFClosesSessions(t *testing.T) {
	h := startAgent(t)
	h.open(1, 24, 80)
	h.input(1, "printf 'PID:%s\\n' $$; printf 'DONE%s\\n' _X\n")
	pid := parsePID(t, h.outputUntil(1, "DONE_X"))
	if err := h.conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.wait(t, 10*time.Second); err != nil {
		t.Fatalf("EOF must be clean, got %v", err)
	}
	waitGone(t, pid)
}

func TestRejectsExperimentalPeer(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(a) }()
	go func() { _, _ = b.Write([]byte{0x01, 0x07, 0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x00, 0x01}) }()
	select {
	case err := <-done:
		if !errors.Is(err, agentproto.ErrBadMagic) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not reject the experimental frame")
	}
	_ = b.Close()
}

func TestOpenSerialRaw(t *testing.T) {
	master, slavePath := testPTY(t)
	conn, err := OpenSerial(slavePath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var term syscall.Termios
	if err := ioctl(conn.Fd(), syscall.TCGETS, unsafe.Pointer(&term)); err != nil {
		t.Fatal(err)
	}
	if term.Lflag&0x2 != 0 || term.Oflag&0x1 != 0 || term.Cflag&cflagCBAUD != syscall.B115200 {
		t.Fatalf("not raw 115200: lflag=%#x oflag=%#x cflag=%#x", term.Lflag, term.Oflag, term.Cflag)
	}
	if _, err := conn.Write([]byte("z")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := master.Read(buf); err != nil || buf[0] != 'z' {
		t.Fatalf("read %q err=%v", buf, err)
	}
}

func parsePID(t *testing.T, out string) int {
	t.Helper()
	marker := "PID:"
	if j := strings.LastIndex(out, "JOB_READY:"); j >= 0 {
		marker = "JOB_READY:"
	}
	i := strings.LastIndex(out, marker)
	if i < 0 {
		t.Fatalf("no pid in %q", out)
	}
	rest := strings.TrimSpace(out[i+len(marker):])
	line := strings.SplitN(rest, "\n", 2)[0]
	line = strings.TrimSpace(line)
	pid, err := strconv.Atoi(line)
	if err != nil || pid <= 1 {
		t.Fatalf("bad pid %q", line)
	}
	return pid
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists", pid)
}

func testPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	var unlock int32
	if err := ioctl(master.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		t.Fatal(err)
	}
	var n uint32
	if err := ioctl(master.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		slave.Close()
		master.Close()
	})
	return master, slave.Name()
}
