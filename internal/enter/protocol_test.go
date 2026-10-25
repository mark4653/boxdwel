package enter

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type duplex struct {
	r *io.PipeReader // guest -> host
	w *io.PipeWriter // host -> guest
}

func (d duplex) Read(b []byte) (int, error)  { return d.r.Read(b) }
func (d duplex) Write(b []byte) (int, error) { return d.w.Write(b) }
func (d duplex) Close() error                { d.r.Close(); return d.w.Close() }

// newStream returns the host side plus the guest output writer and input reader.
func newStream() (duplex, *io.PipeWriter, *io.PipeReader) {
	hostIn, guestOut := io.Pipe()
	guestIn, hostOut := io.Pipe()
	return duplex{r: hostIn, w: hostOut}, guestOut, guestIn
}

const testNonce = "0123456789abcdef0123456789abcdef"

func TestReadyCommand(t *testing.T) {
	cmd, err := readyCommand(testNonce)
	if err != nil {
		t.Fatal(err)
	}
	want := "trap 'echo " + ExitPrefix + testNonce + "' EXIT; echo " + ReadyPrefix + testNonce + "\n"
	if string(cmd) != want {
		t.Fatalf("command = %q", cmd)
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 32), strings.Repeat("0", 31), testNonce + "\n"} {
		if _, err := readyCommand(bad); err == nil {
			t.Fatalf("nonce %q accepted", bad)
		}
	}
}

func TestWaitLineSplitReadsAndExactness(t *testing.T) {
	s, guestOut, _ := newStream()
	defer s.Close()
	c := NewConn(s)
	var echo bytes.Buffer
	go func() {
		io.WriteString(guestOut, "Linux version 6.18.58\r\n# trap 'echo M2_EXIT_x' EXIT; echo M2_RE")
		time.Sleep(20 * time.Millisecond)
		io.WriteString(guestOut, "ADY_"+testNonce+"\r\nM2_READY_"+testNonce+"\r\n# ")
	}()
	if err := c.WaitLine(ReadyPrefix+testNonce, &echo, 2*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !strings.Contains(echo.String(), "M2_READY_"+testNonce+"\r\n") || !strings.Contains(echo.String(), "# ") {
		t.Fatalf("echo = %q", echo.String())
	}
}

func TestWaitLineRejectsSubstring(t *testing.T) {
	s, guestOut, _ := newStream()
	defer s.Close()
	c := NewConn(s)
	go func() {
		io.WriteString(guestOut, "echo M2_READY_"+testNonce+"\n")
		time.Sleep(80 * time.Millisecond)
		io.WriteString(guestOut, "M2_READY_"+testNonce+"0\n")
		time.Sleep(80 * time.Millisecond)
		io.WriteString(guestOut, "M2_READY_"+testNonce+"\n")
	}()
	if err := c.WaitLine(ReadyPrefix+testNonce, io.Discard, 2*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

func TestWaitLineTimeoutAndReadError(t *testing.T) {
	s, _, _ := newStream()
	defer s.Close()
	c := NewConn(s)
	if err := c.WaitLine("never", io.Discard, 50*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
	s2, guestOut2, _ := newStream()
	defer s2.Close()
	c2 := NewConn(s2)
	go func() { guestOut2.Write([]byte("partial")); guestOut2.Close() }()
	if err := c2.WaitLine("never", io.Discard, time.Second); err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("closed stream error = %v", err)
	}
}

func TestPumpForwardsAndStopsAtNonceExit(t *testing.T) {
	s, guestOut, guestIn := newStream()
	defer s.Close()
	c := NewConn(s)
	in, inW := io.Pipe()
	defer inW.Close()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- c.Pump(in, &out, exitMarker(testNonce)) }()
	go func() {
		io.WriteString(guestOut, "# sleep 60\r\nM2_SHELL_EXIT\r\nM2_EXIT_ffffffffffffffffffffffffffffffff\r\n")
		io.WriteString(inW, "uname\n")
		got := make([]byte, len("uname\n"))
		if _, err := io.ReadFull(guestIn, got); err != nil || string(got) != "uname\n" {
			t.Errorf("forwarded %q err=%v", got, err)
		}
		io.WriteString(guestOut, "Linux\r\n# exit\r\nM2_EXIT_"+testNonce+"\r\n")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pump: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not stop at the nonce exit marker")
	}
	if !strings.Contains(out.String(), "M2_EXIT_"+testNonce) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestPumpDiscardsOverlongLineWithoutFalseMatch(t *testing.T) {
	s, guestOut, _ := newStream()
	defer s.Close()
	c := NewConn(s)
	done := make(chan error, 1)
	go func() { done <- c.Pump(strings.NewReader(""), io.Discard, exitMarker(testNonce)) }()
	go func() {
		io.WriteString(guestOut, strings.Repeat("x", maxPending+10))
		io.WriteString(guestOut, " M2_EXIT_"+testNonce+"\n")
		time.Sleep(50 * time.Millisecond)
		io.WriteString(guestOut, "M2_EXIT_"+testNonce+"\n")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pump: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pump stopped or hung on an overlong line")
	}
}

func TestCloseUnblocksSend(t *testing.T) {
	s, _, _ := newStream()
	c := NewConn(s)
	writeErr := make(chan error, 1)
	go func() { writeErr <- c.Send([]byte(strings.Repeat("y", 1024))) }()
	time.Sleep(50 * time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind a blocked write")
	}
	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("blocked write reported success after close")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked write not released by close")
	}
}
