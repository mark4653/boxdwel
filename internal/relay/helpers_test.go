package relay

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mark4653/boxdwel/internal/agentproto"
)

type testClient struct {
	t     *testing.T
	conn  net.Conn
	local uint16
}

func attachClient(t *testing.T, r *Relay, local, rows, cols uint16) *testClient {
	t.Helper()
	cs, rs := net.Pipe()
	if err := r.Attach(rs); err != nil {
		t.Fatalf("attach: %v", err)
	}
	c := &testClient{t: t, conn: cs, local: local}
	t.Cleanup(func() { _ = cs.Close() })

	c.send(agentproto.Frame{Type: agentproto.TypeHello, Control: 1})
	f := c.recv(5 * time.Second)
	if f.Type != agentproto.TypeReady || f.Control != 1 {
		t.Fatalf("want Ready, got %+v", f)
	}
	if sessions, payload, output, err := agentproto.ParseReady(f.Payload); err != nil || sessions != 1 ||
		payload != agentproto.MaxPayload || output != agentproto.MaxOutputChunk {
		t.Fatalf("ready %d/%d/%d err=%v", sessions, payload, output, err)
	}
	p, err := agentproto.Size(rows, cols)
	if err != nil {
		t.Fatal(err)
	}
	c.send(agentproto.Frame{Type: agentproto.TypeOpen, Session: local, Payload: p})
	f = c.recv(5 * time.Second)
	if f.Type != agentproto.TypeOpened || f.Session != local {
		t.Fatalf("want Opened %d, got %+v", local, f)
	}
	return c
}

func (c *testClient) send(f agentproto.Frame) {
	c.t.Helper()
	if err := c.conn.SetWriteDeadline(time.Now().Add(20 * time.Second)); err != nil {
		c.t.Fatal(err)
	}
	if err := agentproto.WriteFrame(c.conn, f); err != nil {
		c.t.Fatalf("send %d: %v", f.Type, err)
	}
}

func (c *testClient) recv(d time.Duration) agentproto.Frame {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		c.t.Fatal(err)
	}
	f, err := agentproto.ReadFrame(c.conn)
	if err != nil {
		c.t.Fatalf("recv: %v", err)
	}
	return f
}

func (c *testClient) input(s string) {
	c.t.Helper()
	c.send(agentproto.Frame{Type: agentproto.TypeInput, Session: c.local, Payload: []byte(s)})
}

func (c *testClient) resize(rows, cols uint16) {
	c.t.Helper()
	p, err := agentproto.Size(rows, cols)
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(agentproto.Frame{Type: agentproto.TypeResize, Session: c.local, Payload: p})
}

func (c *testClient) closeSession() {
	c.t.Helper()
	c.send(agentproto.Frame{Type: agentproto.TypeClose, Session: c.local})
}

func (c *testClient) outputUntil(marker string) string {
	c.t.Helper()
	var out strings.Builder
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f := c.recv(20 * time.Second)
		switch f.Type {
		case agentproto.TypeOutput:
			if f.Session != c.local {
				c.t.Fatalf("output for session %d", f.Session)
			}
			out.Write(f.Payload)
			if strings.Contains(out.String(), marker) {
				return out.String()
			}
		case agentproto.TypeError:
			code, msg, _ := agentproto.ParseError(f.Payload)
			c.t.Fatalf("session error %d: %s", code, msg)
		default:
			c.t.Fatalf("unexpected frame %d while waiting for %q", f.Type, marker)
		}
	}
	c.t.Fatalf("marker %q not seen", marker)
	return ""
}

func (c *testClient) exited() (uint8, int32) {
	c.t.Helper()
	for {
		f := c.recv(20 * time.Second)
		if f.Type == agentproto.TypeOutput {
			continue
		}
		if f.Type != agentproto.TypeExited || f.Session != c.local {
			c.t.Fatalf("want Exited %d, got %+v", c.local, f)
		}
		reason, code, err := agentproto.ParseExited(f.Payload)
		if err != nil {
			c.t.Fatal(err)
		}
		return reason, code
	}
}

func (c *testClient) expectClosed(d time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(d)
	buf := make([]byte, 1)
	for {
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return // already closed
		}
		if _, err := c.conn.Read(buf); err == nil {
			continue
		} else {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				c.t.Fatalf("stream still open after %s", d)
			}
			return
		}
	}
}

func (c *testClient) ping(control uint16, nonce uint64) {
	c.t.Helper()
	c.send(agentproto.Frame{Type: agentproto.TypePing, Control: control, Payload: agentproto.NoncePayload(nonce)})
	for {
		f := c.recv(5 * time.Second)
		if f.Type == agentproto.TypeOutput {
			continue
		}
		if f.Type != agentproto.TypePong || f.Control != control || !bytes.Equal(f.Payload, agentproto.NoncePayload(nonce)) {
			c.t.Fatalf("want local Pong, got %+v", f)
		}
		return
	}
}

// attachRetry attaches a client, retrying while the previous session is still
// being retired by the guest.
func attachRetry(t *testing.T, r *Relay, local, rows, cols uint16, within time.Duration) *testClient {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		cs, rs := net.Pipe()
		if err := r.Attach(rs); err == nil {
			c := &testClient{t: t, conn: cs, local: local}
			t.Cleanup(func() { _ = cs.Close() })
			c.send(agentproto.Frame{Type: agentproto.TypeHello, Control: 1})
			f := c.recv(5 * time.Second)
			if f.Type != agentproto.TypeReady {
				t.Fatalf("want Ready, got %+v", f)
			}
			p, _ := agentproto.Size(rows, cols)
			c.send(agentproto.Frame{Type: agentproto.TypeOpen, Session: local, Payload: p})
			f = c.recv(5 * time.Second)
			if f.Type != agentproto.TypeOpened || f.Session != local {
				t.Fatalf("want Opened, got %+v", f)
			}
			return c
		}
		_ = cs.Close()
		if time.Now().After(deadline) {
			t.Fatal("relay stayed busy")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
