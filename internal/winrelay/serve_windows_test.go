//go:build windows

package winrelay

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mark4653/boxdwel/internal/agentproto"
	"github.com/mark4653/boxdwel/internal/relay"
	"github.com/mark4653/boxdwel/internal/winpipe"
)

func fakeGuest(t *testing.T) *relay.Relay {
	t.Helper()
	guest, guestSide := net.Pipe()
	go func() {
		hello, err := agentproto.ReadFrame(guest)
		if err != nil || hello.Type != agentproto.TypeHello {
			return
		}
		if err := agentproto.WriteFrame(guest, agentproto.Frame{
			Type: agentproto.TypeReady, Control: hello.Control, Payload: agentproto.ReadyPayload(),
		}); err != nil {
			return
		}
		for {
			f, err := agentproto.ReadFrame(guest)
			if err != nil {
				return
			}
			switch f.Type {
			case agentproto.TypeOpen:
				_ = agentproto.WriteFrame(guest, agentproto.Frame{
					Type: agentproto.TypeOpened, Session: f.Session, Payload: f.Payload,
				})
			case agentproto.TypeInput:
				_ = agentproto.WriteFrame(guest, agentproto.Frame{
					Type: agentproto.TypeOutput, Session: f.Session, Payload: f.Payload,
				})
			case agentproto.TypeClose:
				_ = agentproto.WriteFrame(guest, agentproto.Frame{
					Type: agentproto.TypeExited, Session: f.Session,
					Payload: agentproto.ExitedPayload(agentproto.ReasonShellExit, 0),
				})
			case agentproto.TypePing:
				_ = agentproto.WriteFrame(guest, agentproto.Frame{
					Type: agentproto.TypePong, Control: f.Control, Payload: f.Payload,
				})
			}
		}
	}()
	r, err := relay.New(guestSide)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = r.Wait()
		_ = guest.Close()
	})
	return r
}

func pipeName(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return `\\.\pipe\boxdwel-winrelay-test-` + hex.EncodeToString(b[:])
}

func TestServeTwoWindowsClients(t *testing.T) {
	r := fakeGuest(t)
	name := pipeName(t)
	l, err := winpipe.Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- Serve(r, l, stop, ServeOptions{AcceptTimeout: 200 * time.Millisecond})
	}()

	exe, err := winpipe.SelfPath()
	if err != nil {
		t.Fatal(err)
	}
	c1, err := winpipe.OpenClient(name, exe, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := winpipe.OpenClient(name, exe, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	out1, out2 := newSignalBuffer(), newSignalBuffer()
	in1R, in1W := io.Pipe()
	in2R, in2W := io.Pipe()
	d1 := make(chan runResult, 1)
	d2 := make(chan runResult, 1)
	go func() {
		code, err := RunClient(c1, ClientOptions{Size: fixedSize(24, 80), Input: in1R, Output: out1, ResizeInterval: time.Hour})
		d1 <- runResult{code, err}
	}()
	go func() {
		code, err := RunClient(c2, ClientOptions{Size: fixedSize(30, 90), Input: in2R, Output: out2, ResizeInterval: time.Hour})
		d2 <- runResult{code, err}
	}()
	if _, err := in1W.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := in2W.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, out1, "first", 5*time.Second)
	waitOutput(t, out2, "second", 5*time.Second)
	if strings.Contains(out1.String(), "second") || strings.Contains(out2.String(), "first") {
		t.Fatalf("client output crossed: %q %q", out1.String(), out2.String())
	}

	_ = in1W.Close()
	select {
	case res := <-d1:
		if res.err != nil || res.code != 0 {
			t.Fatalf("client 1: code=%d err=%v", res.code, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client 1 did not finish")
	}
	if _, err := in2W.Write([]byte("alive")); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, out2, "alive", 5*time.Second)
	_ = in2W.Close()
	select {
	case res := <-d2:
		if res.err != nil || res.code != 0 {
			t.Fatalf("client 2: code=%d err=%v", res.code, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client 2 did not finish")
	}

	close(stop)
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop")
	}

	// Serve owns and closes the relay on return.
	cs, rs := net.Pipe()
	defer cs.Close()
	if err := r.Attach(rs); !errors.Is(err, relay.ErrClosed) {
		t.Fatalf("relay was not closed by Serve: %v", err)
	}
}

func TestServeReturnsWhenRelayDies(t *testing.T) {
	r := fakeGuest(t)
	name := pipeName(t)
	l, err := winpipe.Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- Serve(r, l, stop, ServeOptions{AcceptTimeout: 200 * time.Millisecond})
	}()
	_ = r.Close()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve after relay close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not observe the relay ending")
	}
}
