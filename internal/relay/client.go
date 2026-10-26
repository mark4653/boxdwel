package relay

import (
	"fmt"
	"io"
	"sync"

	"github.com/mark4653/boxdwel/internal/agentproto"
)

// client is one terminal stream. local is the client's session id; guest is
// the relay-allocated id, which is never sent to the client.
type client struct {
	r      *Relay
	stream io.ReadWriteCloser
	out    chan agentproto.Frame
	done   chan struct{}

	openedCh    chan struct{}
	guestDoneCh chan struct{}
	openedOnce  sync.Once
	finalOnce   sync.Once

	mu        sync.Mutex
	local     uint16
	guest     uint16
	rows      uint16
	cols      uint16
	opened    bool
	dead      bool
	closing   bool
	guestDone bool
	workers   int
	inFlight  int

	stopOnce sync.Once
	slotOnce sync.Once
}

func newClient(r *Relay, stream io.ReadWriteCloser) *client {
	return &client{
		r:           r,
		stream:      stream,
		out:         make(chan agentproto.Frame, r.opt.clientQueue),
		done:        make(chan struct{}),
		openedCh:    make(chan struct{}),
		guestDoneCh: make(chan struct{}),
		guestDone:   true, // no guest session until the handshake assigns one
		workers:     2,
	}
}

func (c *client) localID() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.local
}

func (c *client) guestID() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.guest
}

func (c *client) isOpened() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opened
}

func (c *client) isDead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

func (c *client) isClosing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closing || c.dead
}

func (c *client) sessionOver() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.guestDone
}

// stale reports a session whose queued guest input should be discarded.
func (c *client) stale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead || c.closing || c.guestDone
}

func (c *client) markOpened() {
	c.openedOnce.Do(func() {
		c.mu.Lock()
		c.opened = true
		c.mu.Unlock()
		close(c.openedCh)
	})
}

func (c *client) markGuestDone() {
	c.finalOnce.Do(func() {
		c.mu.Lock()
		c.guestDone = true
		c.mu.Unlock()
		close(c.guestDoneCh)
		c.maybeRelease()
	})
}

func (c *client) reserveInput() (ok, over bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead || c.closing || c.guestDone {
		return false, false
	}
	if c.inFlight >= maxClientInputFrames {
		return false, true
	}
	c.inFlight++
	return true, false
}

func (c *client) inputDone() {
	c.mu.Lock()
	if c.inFlight > 0 {
		c.inFlight--
	}
	c.mu.Unlock()
	c.maybeRelease()
}

func (c *client) releaseSlot() {
	c.slotOnce.Do(func() {
		select {
		case <-c.r.slots:
		default:
		}
	})
}

// maybeRelease exposes the slot only after the guest session is over, both
// client workers (and the stream) are done, and every queued guest input for
// this client has been written or dropped. Otherwise stale input could occupy
// the shared guest queue and make a later client fail with a full queue.
func (c *client) maybeRelease() {
	c.mu.Lock()
	done := c.guestDone && c.workers == 0 && c.inFlight == 0
	c.mu.Unlock()
	if done {
		c.releaseSlot()
	}
}

func (c *client) workerDone() {
	c.mu.Lock()
	if c.workers > 0 {
		c.workers--
	}
	c.mu.Unlock()
	c.maybeRelease()
}

func (c *client) enqueue(f agentproto.Frame) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.out <- f:
		return true
	default:
		return false
	}
}

// forwardClose sends at most one Close for the guest session. Duplicate client
// Close frames or a later disconnect do not add more control frames.
func (c *client) forwardClose() error {
	c.mu.Lock()
	if c.closing || c.guest == 0 || c.guestDone {
		c.mu.Unlock()
		return nil
	}
	c.closing = true
	g := c.guest
	c.mu.Unlock()

	ack, err := c.r.sendGuest(agentproto.Frame{Type: agentproto.TypeClose, Session: g}, true, c, false, true)
	if err != nil {
		return err
	}
	c.r.wg.Add(1)
	go c.r.awaitClosed(c, ack)
	return nil
}

// stop ends this client stream. withClose asks the guest to close the session
// once; a final frame from the guest suppresses that. The returned error is
// the caller's to fail the relay with, outside stopOnce.
func (c *client) stop(withClose bool) error {
	var stopErr error
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.dead = true
		c.mu.Unlock()
		close(c.done)
		_ = c.stream.Close()
		c.r.forget(c)
		if withClose {
			stopErr = c.forwardClose()
		}
		c.maybeRelease()
	})
	return stopErr
}

func (c *client) stopAndFail(withClose bool) {
	if err := c.stop(withClose); err != nil {
		c.r.failIf(err)
	}
}

func (c *client) readLoop() {
	defer c.r.wg.Done()
	defer c.workerDone()
	if err := c.handshake(); err != nil {
		c.stopAndFail(false)
		return
	}
	for {
		f, err := agentproto.ReadFrame(c.stream)
		if err != nil {
			c.stopAndFail(true)
			return
		}
		if err := f.FromHost(); err != nil {
			c.stopAndFail(true)
			return
		}
		if f.Type == agentproto.TypePing {
			if !c.enqueue(agentproto.Frame{Type: agentproto.TypePong, Control: f.Control, Payload: f.Payload}) {
				c.stopAndFail(true)
				return
			}
			continue
		}
		if f.Session != c.localID() {
			c.stopAndFail(true)
			return
		}
		switch f.Type {
		case agentproto.TypeInput, agentproto.TypeResize:
			if c.sessionOver() || c.isClosing() {
				continue
			}
			if !c.isOpened() {
				c.stopAndFail(true)
				return
			}
			if f.Type == agentproto.TypeResize {
				if _, _, err := agentproto.ParseSize(f.Payload); err != nil {
					c.stopAndFail(true)
					return
				}
			}
			ok, over := c.reserveInput()
			if !ok {
				if over {
					c.stopAndFail(true)
					return
				}
				continue
			}
			if _, err := c.r.sendGuest(agentproto.Frame{
				Type: f.Type, Session: c.guestID(), Payload: f.Payload,
			}, false, c, true, false); err != nil {
				c.inputDone()
				c.stopAndFail(true)
				return
			}
		case agentproto.TypeClose:
			if c.sessionOver() {
				continue
			}
			if !c.isOpened() {
				c.stopAndFail(true)
				return
			}
			if err := c.forwardClose(); err != nil {
				c.stopAndFail(true)
				return
			}
		default:
			c.stopAndFail(true)
			return
		}
	}
}

func (c *client) writeLoop() {
	defer c.r.wg.Done()
	defer c.workerDone()
	for {
		select {
		case <-c.done:
			return
		case f := <-c.out:
			if err := writeTimed(c.stream, c.stream, f, c.r.opt.writeTimeout); err != nil {
				c.stopAndFail(true)
				return
			}
			if f.Type == agentproto.TypeExited || (f.Type == agentproto.TypeError && f.Session != 0) {
				_ = c.stream.Close()
				return
			}
		}
	}
}

func (c *client) handshake() error {
	f, err := readTimed(c.stream, c.stream, c.r.opt.handshakeTimeout)
	if err != nil {
		return err
	}
	if err := f.FromHost(); err != nil || f.Type != agentproto.TypeHello {
		return fmt.Errorf("relay: client hello: unexpected frame %d", f.Type)
	}
	ready := agentproto.ReadyPayload()
	ready[0] = 1 // one terminal per client
	if err := writeTimed(c.stream, c.stream, agentproto.Frame{
		Type: agentproto.TypeReady, Control: f.Control, Payload: ready,
	}, c.r.opt.writeTimeout); err != nil {
		return err
	}

	f, err = readTimed(c.stream, c.stream, c.r.opt.handshakeTimeout)
	if err != nil {
		return err
	}
	if err := f.FromHost(); err != nil || f.Type != agentproto.TypeOpen {
		return fmt.Errorf("relay: client open: unexpected frame %d", f.Type)
	}
	rows, cols, err := agentproto.ParseSize(f.Payload)
	if err != nil {
		c.reject(agentproto.CodeProtocol, "invalid terminal size")
		return err
	}
	local := f.Session
	guest, err := c.r.allocGuest()
	if err != nil {
		c.reject(agentproto.CodeSessionLimit, "no session id available")
		return err
	}
	c.mu.Lock()
	c.local = local
	c.rows, c.cols = rows, cols
	c.guest = guest
	c.guestDone = false
	c.mu.Unlock()
	c.r.register(guest, c, rows, cols)

	payload, err := agentproto.Size(rows, cols)
	if err != nil {
		c.r.unregister(guest)
		c.markGuestDone()
		c.reject(agentproto.CodeProtocol, "invalid terminal size")
		return err
	}
	ack, err := c.r.sendGuest(agentproto.Frame{Type: agentproto.TypeOpen, Session: guest, Payload: payload}, true, c, false, true)
	if err != nil {
		c.r.unregister(guest)
		c.markGuestDone()
		c.reject(agentproto.CodeProtocol, "relay busy")
		return err
	}
	c.r.wg.Add(1)
	go c.r.awaitOpened(c, ack)
	return nil
}

// reject writes one control Error before closing; it is only used before the
// guest session has produced output for this client.
func (c *client) reject(code uint8, message string) {
	_ = writeTimed(c.stream, c.stream, agentproto.Frame{
		Type:    agentproto.TypeError,
		Payload: agentproto.ErrorPayload(code, message),
	}, c.r.opt.writeTimeout)
}
