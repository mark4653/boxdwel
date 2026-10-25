//go:build windows

package enter

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"
)

func pipeTestName(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return `\\.\pipe\vboxdistro-enter-test-` + hex.EncodeToString(b[:])
}

func TestPipeCreateAndLocalPeer(t *testing.T) {
	name := pipeTestName(t)
	p, err := createConsolePipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	clientCh := make(chan *os.File, 1)
	errCh := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			errCh <- err
			return
		}
		clientCh <- f
	}()
	if err := p.Accept(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	var client *os.File
	select {
	case client = <-clientCh:
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("client did not connect")
	}
	defer client.Close()
	if _, err := p.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := client.Read(buf); err != nil || string(buf) != "ping" {
		t.Fatalf("client read %q err=%v", buf, err)
	}
	if _, err := client.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Read(buf); err != nil || string(buf) != "pong" {
		t.Fatalf("host read %q err=%v", buf, err)
	}
}

func TestPipeSecondInstanceRejected(t *testing.T) {
	name := pipeTestName(t)
	first, err := createConsolePipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := createConsolePipe(name); err == nil {
		second.Close()
		t.Fatal("second first-instance pipe was accepted")
	} else if !strings.Contains(err.Error(), "another session") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPipeAcceptTimeout(t *testing.T) {
	name := pipeTestName(t)
	p, err := createConsolePipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Accept(200 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("accept timeout error = %v", err)
	}
}

func connectPeer(t *testing.T, name string, p *consolePipe) *os.File {
	t.Helper()
	ch := make(chan *os.File, 1)
	errCh := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			errCh <- err
			return
		}
		ch <- f
	}()
	if err := p.Accept(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-ch:
		return f
	case err := <-errCh:
		t.Fatal(err)
		return nil
	case <-time.After(3 * time.Second):
		t.Fatal("peer did not connect")
		return nil
	}
}

func TestPipeCloseUnblocksPendingRead(t *testing.T) {
	name := pipeTestName(t)
	p, err := createConsolePipe(name)
	if err != nil {
		t.Fatal(err)
	}
	client := connectPeer(t, name, p)
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := p.Read(buf)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pending read returned success after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending read did not return after close")
	}
}

func TestPipeCloseUnblocksBlockedWrite(t *testing.T) {
	name := pipeTestName(t)
	p, err := createConsolePipe(name)
	if err != nil {
		t.Fatal(err)
	}
	client := connectPeer(t, name, p)
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		_, err := p.Write(make([]byte, 1<<20))
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked write returned success after close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked write did not return after close")
	}
}
