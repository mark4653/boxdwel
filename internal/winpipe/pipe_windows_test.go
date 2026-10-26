//go:build windows

package winpipe

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPipeName(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return `\\.\pipe\boxdwel-winpipe-test-` + hex.EncodeToString(b[:])
}

func selfExe(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type accepted struct {
	s   *Server
	err error
}

func acceptAsync(l *Listener, timeout time.Duration) chan accepted {
	ch := make(chan accepted, 1)
	go func() {
		s, err := l.Accept(timeout)
		ch <- accepted{s, err}
	}()
	return ch
}

func waitAccept(t *testing.T, ch chan accepted, d time.Duration) *Server {
	t.Helper()
	select {
	case a := <-ch:
		if a.err != nil {
			t.Fatalf("accept: %v", a.err)
		}
		return a.s
	case <-time.After(d):
		t.Fatal("accept did not return")
		return nil
	}
}

func TestServerClientRoundTrip(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	acceptCh := acceptAsync(l, 3*time.Second)
	client, err := OpenClient(name, selfExe(t), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	srv := waitAccept(t, acceptCh, 3*time.Second)
	defer srv.Close()

	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := srv.Read(buf); err != nil || string(buf) != "ping" {
		t.Fatalf("server read %q err=%v", buf, err)
	}
	if _, err := srv.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(buf); err != nil || string(buf) != "pong" {
		t.Fatalf("client read %q err=%v", buf, err)
	}
}

func TestTwoClientsAndNamespaceKept(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	acceptCh := acceptAsync(l, 3*time.Second)
	c1, err := OpenClient(name, selfExe(t), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	s1 := waitAccept(t, acceptCh, 3*time.Second)
	defer s1.Close()

	// The listener created the next pending instance before returning s1, so
	// a second client can connect immediately.
	acceptCh = acceptAsync(l, 3*time.Second)
	c2, err := OpenClient(name, selfExe(t), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	s2 := waitAccept(t, acceptCh, 3*time.Second)
	defer s2.Close()

	if _, err := c1.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := s1.Read(buf); err != nil || string(buf) != "one" {
		t.Fatalf("s1 %q err=%v", buf, err)
	}
	if _, err := s2.Read(buf); err != nil || string(buf) != "two" {
		t.Fatalf("s2 %q err=%v", buf, err)
	}
}

func TestSecondListenerRejected(t *testing.T) {
	name := testPipeName(t)
	first, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Listen(name); err == nil {
		_ = second.Close()
		t.Fatal("second first-instance listener was accepted")
	}
}

func TestAcceptTimeoutThenRecovery(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if s, err := l.Accept(100 * time.Millisecond); err == nil {
		_ = s.Close()
		t.Fatal("accept without a client succeeded")
	} else if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}

	acceptCh := acceptAsync(l, 3*time.Second)
	client, err := OpenClient(name, selfExe(t), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	srv := waitAccept(t, acceptCh, 3*time.Second)
	defer srv.Close()
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := srv.Read(buf); err != nil || buf[0] != 'x' {
		t.Fatalf("read %q err=%v", buf, err)
	}
}

func TestCloseCancelsPendingAccept(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	ch := acceptAsync(l, time.Minute)
	time.Sleep(100 * time.Millisecond) // let the accept reach ConnectNamedPipe
	_ = l.Close()
	select {
	case a := <-ch:
		if a.err == nil {
			_ = a.s.Close()
			t.Fatal("accept succeeded after listener close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not cancel the pending accept")
	}
}

func TestCloseUnblocksServerReadAndWrite(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	acceptCh := acceptAsync(l, 3*time.Second)
	client, err := OpenClient(name, selfExe(t), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	srv := waitAccept(t, acceptCh, 3*time.Second)

	readDone := make(chan error, 1)
	go func() {
		_, err := srv.Read(make([]byte, 64))
		readDone <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("pending read returned success after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending read did not return after close")
	}

	name2 := testPipeName(t)
	l2, err := Listen(name2)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	acceptCh = acceptAsync(l2, 3*time.Second)
	client2, err := OpenClient(name2, selfExe(t), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()
	srv2 := waitAccept(t, acceptCh, 3*time.Second)
	writeDone := make(chan error, 1)
	go func() {
		_, err := srv2.Write(make([]byte, 1<<20))
		writeDone <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := srv2.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blocked write returned success after close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked write did not return after close")
	}
}

func TestClientCloseUnblocksRead(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	acceptCh := acceptAsync(l, 3*time.Second)
	client, err := OpenClient(name, selfExe(t), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	srv := waitAccept(t, acceptCh, 3*time.Second)
	defer srv.Close()

	done := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 64))
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	_ = client.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("client read returned success after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client read did not return after close")
	}
}

func TestPeerDisconnect(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	acceptCh := acceptAsync(l, 3*time.Second)
	client, err := OpenClient(name, selfExe(t), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	srv := waitAccept(t, acceptCh, 3*time.Second)
	defer srv.Close()
	_ = client.Close()

	buf := make([]byte, 8)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := srv.Read(buf)
		if err != nil {
			return // EOF or broken pipe
		}
		if time.Now().After(deadline) {
			t.Fatal("server read kept working after the peer closed")
		}
	}
}

func TestIdentityMismatchRejected(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	acceptCh := acceptAsync(l, 3*time.Second)
	if _, err := OpenClient(name, `C:\definitely\wrong\server.exe`, 3*time.Second); err == nil {
		t.Fatal("wrong server executable was accepted")
	}
	select {
	case a := <-acceptCh:
		if a.s != nil {
			_ = a.s.Close()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accept did not finish after the rejected client")
	}
}

func TestOpenMissingPipe(t *testing.T) {
	if _, err := OpenClient(testPipeName(t), selfExe(t), 500*time.Millisecond); err == nil {
		t.Fatal("opening a missing pipe succeeded")
	}
}

func TestRawInputMode(t *testing.T) {
	got := rawInputMode(0x0001 | 0x0002 | 0x0004 | 0x0040 | 0x0010 | 0x0008 | 0x0080)
	if got&(0x0001|0x0002|0x0004|0x0040|0x0010|0x0008) != 0 {
		t.Fatalf("raw mode kept editing bits: %#x", got)
	}
	if got&0x0080 == 0 || got&0x0200 == 0 {
		t.Fatalf("raw mode missing extended/VT bits: %#x", got)
	}
}

func TestValidSize(t *testing.T) {
	for _, tc := range []struct {
		rows, cols uint16
		ok         bool
	}{
		{24, 80, true}, {1, 1, true}, {500, 500, true},
		{0, 80, false}, {24, 0, false}, {501, 80, false}, {24, 501, false},
	} {
		err := ValidSize(tc.rows, tc.cols)
		if (err == nil) != tc.ok {
			t.Errorf("ValidSize(%d,%d)=%v", tc.rows, tc.cols, err)
		}
	}
}

func TestConsoleSizeAndRestore(t *testing.T) {
	c, err := EnableConsole()
	if err != nil {
		if strings.Contains(err.Error(), "interactive console required") || strings.Contains(err.Error(), "console code page") {
			t.Skip("NOT RUN: no interactive console in this environment")
		}
		t.Fatal(err)
	}
	defer func() {
		if err := c.Restore(); err != nil {
			t.Errorf("restore: %v", err)
		}
	}()
	rows, cols, err := c.Size()
	if err != nil {
		t.Fatal(err)
	}
	if rows == 0 || cols == 0 {
		t.Fatalf("size %dx%d", rows, cols)
	}
}

func TestListenerFailsClosedOnInstanceCap(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var extras []*Server
	for i := 0; i < maxInstances-1; i++ {
		s, err := newServer(name, false)
		if err != nil {
			t.Fatalf("extra instance %d: %v", i, err)
		}
		extras = append(extras, s)
	}
	for _, s := range extras {
		defer s.Close()
	}
	_, err = l.Accept(100 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("expected explicit namespace failure, got %v", err)
	}
}

func TestListenerNamespaceLostFailsClosed(t *testing.T) {
	l := &Listener{name: testPipeName(t)}
	if _, err := l.Accept(100 * time.Millisecond); err == nil {
		t.Fatal("listener without a pending instance was accepted")
	}
	if !l.closed {
		t.Fatal("listener did not fail closed")
	}
}

func TestRepeatedCloseWaitsForAccept(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	acceptDone := make(chan error, 1)
	go func() {
		_, err := l.Accept(time.Minute)
		acceptDone <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the accept register
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Close()
		}()
	}
	closed := make(chan struct{})
	go func() {
		wg.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("repeated Close returned before the accept was joined")
	}
	select {
	case <-acceptDone:
	case <-time.After(2 * time.Second):
		t.Fatal("accept did not return after Close")
	}
}

func TestPipeNameAndExecutableValidation(t *testing.T) {
	for _, name := range []string{
		"relative",
		`\\server\pipe\x`,
		`\\.\pipe\`,
		`\\.\pipe\a\b`,
		`\\.\pipe\..\x`,
	} {
		if _, err := Listen(name); err == nil {
			t.Errorf("Listen(%q) was accepted", name)
		}
	}
	name := testPipeName(t)
	exe := selfExe(t)
	for _, bad := range []string{"", "relative.exe", `\\server\share\x.exe`, `//server/share/x.exe`} {
		if _, err := OpenClient(name, bad, 100*time.Millisecond); err == nil {
			t.Errorf("OpenClient expected executable %q was accepted", bad)
		} else if bad == "" && !strings.Contains(err.Error(), "required") {
			t.Errorf("empty executable error = %v", err)
		}
	}
	if _, err := OpenClient(`\\server\pipe\x`, exe, 100*time.Millisecond); err == nil {
		t.Error("remote pipe name was accepted")
	}
}

func TestRepeatedCloseWaitsForPendingTeardown(t *testing.T) {
	for i := 0; i < 20; i++ {
		name := testPipeName(t)
		l, err := Listen(name)
		if err != nil {
			t.Fatal(err)
		}
		pending := l.pending
		var wg sync.WaitGroup
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = l.Close()
			}()
		}
		wg.Wait()
		pending.Stream.mu.Lock()
		closed := pending.Stream.closed
		pending.Stream.mu.Unlock()
		if !closed {
			t.Fatalf("iteration %d: repeated Close returned before the pending handle closed", i)
		}
	}
}
