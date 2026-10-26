// Package relay connects one guest wire to up to two terminal client streams.
// It is platform-neutral: the caller supplies byte streams and owns their
// transport (named pipes, sockets, serial files).
package relay

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/mark4653/boxdwel/internal/agentproto"
)

var (
	// ErrBusy means two clients are already attached.
	ErrBusy = errors.New("relay: two clients already attached")
	// ErrClosed means the relay is shutting down.
	ErrClosed = errors.New("relay: closed")
)

const (
	maxClients = 2
	// maxClientInputFrames bounds one client's queued guest input so a flood
	// cannot consume the shared queue or delay control frames.
	maxClientInputFrames = 16

	defaultHandshakeTimeout = 5 * time.Second
	defaultWriteTimeout     = 5 * time.Second
	defaultPingInterval     = 5 * time.Second
	defaultPongTimeout      = 2 * time.Second
	defaultConfirmTimeout   = 5 * time.Second
	defaultClientQueue      = 64
	defaultGuestQueue       = 64
	defaultControlQueue     = 8
)

var errGuestBusy = errors.New("relay: guest write queue full")

type options struct {
	handshakeTimeout time.Duration
	writeTimeout     time.Duration
	pingInterval     time.Duration
	pongTimeout      time.Duration
	confirmTimeout   time.Duration
	clientQueue      int
	guestQueue       int
	controlQueue     int
}

func defaults() options {
	return options{
		handshakeTimeout: defaultHandshakeTimeout,
		writeTimeout:     defaultWriteTimeout,
		pingInterval:     defaultPingInterval,
		pongTimeout:      defaultPongTimeout,
		confirmTimeout:   defaultConfirmTimeout,
		clientQueue:      defaultClientQueue,
		guestQueue:       defaultGuestQueue,
		controlQueue:     defaultControlQueue,
	}
}

type sendAck struct{ err error }

// guestReq is one frame queued for the guest writer. ack is signalled after
// the write attempt (not at enqueue), so confirmation timers start at the
// actual send.
type guestReq struct {
	frame  agentproto.Frame
	owner  *client
	budget bool
	ack    chan sendAck
}

type session struct {
	client     *client
	opened     bool
	rows, cols uint16
}

// Relay owns one guest connection and its clients.
type Relay struct {
	guest io.ReadWriteCloser
	opt   options

	slots chan struct{}

	mu        sync.Mutex
	closed    bool
	clients   map[*client]struct{}
	byGuest   map[uint16]*session
	retired   map[uint16]struct{}
	nextGuest uint16

	guestCtrl chan guestReq
	guestData chan guestReq

	done    chan struct{}
	stopOne sync.Once
	causeMu sync.Mutex
	cause   error
	caused  bool

	pongMu   sync.Mutex
	pongCtrl uint16
	pongCh   chan uint64

	wg sync.WaitGroup
}

// New performs the guest Hello/Ready handshake and starts the relay.
func New(guest io.ReadWriteCloser) (*Relay, error) {
	return newRelay(guest, defaults())
}

func newRelay(guest io.ReadWriteCloser, opt options) (*Relay, error) {
	r := &Relay{
		guest:     guest,
		opt:       opt,
		slots:     make(chan struct{}, maxClients),
		clients:   make(map[*client]struct{}),
		byGuest:   make(map[uint16]*session),
		retired:   make(map[uint16]struct{}),
		nextGuest: 1,
		guestCtrl: make(chan guestReq, opt.controlQueue),
		guestData: make(chan guestReq, opt.guestQueue),
		done:      make(chan struct{}),
	}
	if err := r.guestHandshake(); err != nil {
		_ = guest.Close()
		return nil, err
	}
	r.wg.Add(3)
	go r.guestLoop()
	go r.guestWriter()
	go r.pingLoop()
	return r, nil
}

func (r *Relay) guestHandshake() error {
	if err := writeTimed(r.guest, r.guest, agentproto.Frame{
		Type: agentproto.TypeHello, Control: 1,
	}, r.opt.writeTimeout); err != nil {
		return fmt.Errorf("relay: guest hello: %w", err)
	}
	f, err := readTimed(r.guest, r.guest, r.opt.handshakeTimeout)
	if err != nil {
		return fmt.Errorf("relay: guest ready: %w", err)
	}
	if f.Type != agentproto.TypeReady || f.Control != 1 {
		return fmt.Errorf("relay: guest ready: unexpected frame %d", f.Type)
	}
	sessions, payload, output, err := agentproto.ParseReady(f.Payload)
	if err != nil {
		return fmt.Errorf("relay: guest ready: %w", err)
	}
	if sessions < maxClients || payload < agentproto.MaxPayload || output < agentproto.MaxOutputChunk {
		return fmt.Errorf("relay: guest limits %d/%d/%d too small", sessions, payload, output)
	}
	return nil
}

// Attach registers one client stream. It returns ErrBusy when two clients are
// attached and ErrClosed after Close. On error the caller still owns stream.
func (r *Relay) Attach(stream io.ReadWriteCloser) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	select {
	case r.slots <- struct{}{}:
	default:
		r.mu.Unlock()
		return ErrBusy
	}
	// The slot, the map entry, and wg.Add are all under the same gate that
	// fail uses to set closed and snapshot; Wait therefore cannot pass this
	// Add. No Add happens after closed is set.
	c := newClient(r, stream)
	r.clients[c] = struct{}{}
	r.wg.Add(2)
	r.mu.Unlock()
	go c.readLoop()
	go c.writeLoop()
	return nil
}

// Close stops the relay and every client. It is idempotent.
func (r *Relay) Close() error {
	r.fail(nil)
	return nil
}

// Wait blocks until the relay stops and returns its cause (nil after Close).
func (r *Relay) Wait() error {
	<-r.done
	r.wg.Wait()
	r.causeMu.Lock()
	defer r.causeMu.Unlock()
	return r.cause
}

func (r *Relay) fail(err error) {
	r.causeMu.Lock()
	if !r.caused {
		r.caused = true
		r.cause = err
	}
	r.causeMu.Unlock()
	r.stopOne.Do(func() {
		// Decide closed and snapshot under the same lock that Attach uses to
		// add clients, so no Add can happen after the snapshot. I/O teardown
		// stays outside the lock.
		r.mu.Lock()
		r.closed = true
		clients := make([]*client, 0, len(r.clients))
		for c := range r.clients {
			clients = append(clients, c)
		}
		r.mu.Unlock()
		close(r.done)
		_ = r.guest.Close()
		for _, c := range clients {
			_ = c.stop(false)
		}
	})
}

// failIf ignores a shutdown race: a send failure while the relay is already
// stopping is not a second cause.
func (r *Relay) failIf(err error) {
	if err == nil || errors.Is(err, ErrClosed) {
		return
	}
	select {
	case <-r.done:
		return
	default:
	}
	r.fail(err)
}

func (r *Relay) forget(c *client) {
	r.mu.Lock()
	delete(r.clients, c)
	r.mu.Unlock()
}

func (r *Relay) allocGuest() (uint16, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nextGuest == 0 {
		return 0, errors.New("relay: guest session ids exhausted")
	}
	id := r.nextGuest
	r.nextGuest++
	return id, nil
}

func (r *Relay) register(guest uint16, c *client, rows, cols uint16) {
	r.mu.Lock()
	r.byGuest[guest] = &session{client: c, rows: rows, cols: cols}
	r.mu.Unlock()
}

func (r *Relay) unregister(guest uint16) {
	r.mu.Lock()
	delete(r.byGuest, guest)
	r.retired[guest] = struct{}{}
	r.mu.Unlock()
}

func (r *Relay) sendGuest(f agentproto.Frame, control bool, owner *client, budget, needAck bool) (chan sendAck, error) {
	req := guestReq{frame: f, owner: owner, budget: budget}
	if needAck {
		req.ack = make(chan sendAck, 1)
	}
	queue := r.guestData
	if control {
		queue = r.guestCtrl
	}
	select {
	case <-r.done:
		return nil, ErrClosed
	default:
	}
	select {
	case queue <- req:
		return req.ack, nil
	default:
		return nil, errGuestBusy
	}
}

func (r *Relay) guestWriter() {
	defer r.wg.Done()
	for {
		select {
		case <-r.done:
			return
		default:
		}
		var req guestReq
		select {
		case req = <-r.guestCtrl:
		default:
			select {
			case req = <-r.guestCtrl:
			case req = <-r.guestData:
			case <-r.done:
				return
			}
		}
		r.writeGuest(req)
	}
}

func (r *Relay) writeGuest(req guestReq) {
	if req.budget && req.owner != nil {
		defer req.owner.inputDone()
	}
	if req.budget && req.owner != nil && req.owner.stale() {
		// The session is closing: do not forward its queued input.
		if req.ack != nil {
			req.ack <- sendAck{}
		}
		return
	}
	err := writeTimed(r.guest, r.guest, req.frame, r.opt.writeTimeout)
	if req.ack != nil {
		req.ack <- sendAck{err: err}
	}
	if err != nil {
		r.fail(fmt.Errorf("relay: guest write: %w", err))
	}
}

func (r *Relay) guestLoop() {
	defer r.wg.Done()
	for {
		f, err := agentproto.ReadFrame(r.guest)
		if err != nil {
			r.fail(fmt.Errorf("relay: guest read: %w", err))
			return
		}
		if err := f.FromGuest(); err != nil {
			r.fail(err)
			return
		}
		switch f.Type {
		case agentproto.TypeOpened:
			r.onOpened(f)
		case agentproto.TypeOutput:
			r.route(f)
		case agentproto.TypeExited:
			r.onFinal(f)
		case agentproto.TypeError:
			if f.Session == 0 {
				r.fail(errors.New("relay: guest control error"))
				return
			}
			r.onFinal(f)
		case agentproto.TypePong:
			r.onPong(f)
		default:
			r.fail(fmt.Errorf("relay: unexpected guest frame %d", f.Type))
			return
		}
	}
}

// lookup returns the session for a guest id: (session, false) for a retired
// id, or (nil, false) when the id was never allocated.
func (r *Relay) lookup(guest uint16) (*session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byGuest[guest]; ok {
		return s, true
	}
	if _, ok := r.retired[guest]; ok {
		return nil, true
	}
	return nil, false
}

func (r *Relay) onOpened(f agentproto.Frame) {
	s, known := r.lookup(f.Session)
	if !known {
		r.fail(fmt.Errorf("relay: opened for unknown guest session %d", f.Session))
		return
	}
	if s == nil {
		return // late for a retired id
	}
	if s.opened {
		r.fail(fmt.Errorf("relay: duplicate opened for guest session %d", f.Session))
		return
	}
	rows, cols, err := agentproto.ParseSize(f.Payload)
	if err != nil || rows != s.rows || cols != s.cols {
		r.fail(fmt.Errorf("relay: opened size mismatch for guest session %d", f.Session))
		return
	}
	s.opened = true
	c := s.client
	c.markOpened()
	if c.isDead() {
		return
	}
	if !c.enqueue(agentproto.Frame{Type: agentproto.TypeOpened, Session: c.localID(), Payload: f.Payload}) {
		if err := c.stop(true); err != nil {
			r.failIf(err)
		}
	}
}

func (r *Relay) route(f agentproto.Frame) {
	s, known := r.lookup(f.Session)
	if !known {
		r.fail(fmt.Errorf("relay: frame for unknown guest session %d", f.Session))
		return
	}
	if s == nil || s.client.isDead() {
		return
	}
	if !s.opened {
		r.fail(fmt.Errorf("relay: output before opened for guest session %d", f.Session))
		return
	}
	if !s.client.enqueue(agentproto.Frame{Type: f.Type, Session: s.client.localID(), Payload: f.Payload}) {
		if err := s.client.stop(true); err != nil {
			r.failIf(err)
		}
	}
}

func (r *Relay) onFinal(f agentproto.Frame) {
	s, known := r.lookup(f.Session)
	if !known {
		r.fail(fmt.Errorf("relay: final for unknown guest session %d", f.Session))
		return
	}
	if s == nil {
		return
	}
	if f.Type == agentproto.TypeExited && !s.opened {
		r.fail(fmt.Errorf("relay: exited before opened for guest session %d", f.Session))
		return
	}
	r.unregister(f.Session)
	c := s.client
	c.markGuestDone()
	if c.isDead() {
		return
	}
	if !c.enqueue(agentproto.Frame{Type: f.Type, Session: c.localID(), Payload: f.Payload}) {
		if err := c.stop(true); err != nil {
			r.failIf(err)
		}
	}
}

func (r *Relay) onPong(f agentproto.Frame) {
	nonce, err := agentproto.ParseNonce(f.Payload)
	if err != nil {
		return
	}
	r.pongMu.Lock()
	ctrl, ch := r.pongCtrl, r.pongCh
	r.pongMu.Unlock()
	if ch == nil || ctrl != f.Control {
		return
	}
	select {
	case ch <- nonce:
	default:
	}
}

func (r *Relay) pingLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.opt.pingInterval)
	defer ticker.Stop()
	var control uint16 = 100
	var nonce uint64
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
		}
		control++
		if control == 0 {
			control = 1
		}
		nonce++
		ch := make(chan uint64, 1)
		r.pongMu.Lock()
		r.pongCtrl, r.pongCh = control, ch
		r.pongMu.Unlock()
		ack, err := r.sendGuest(agentproto.Frame{
			Type:    agentproto.TypePing,
			Control: control,
			Payload: agentproto.NoncePayload(nonce),
		}, true, nil, false, true)
		if err != nil {
			r.fail(fmt.Errorf("relay: ping: %w", err))
			return
		}
		// The response deadline starts only after the Ping was actually
		// written; a slow guest writer must not trigger a false timeout.
		select {
		case a := <-ack:
			if a.err != nil {
				return
			}
		case <-r.done:
			return
		}
		select {
		case got := <-ch:
			if got != nonce {
				r.fail(errors.New("relay: pong nonce mismatch"))
				return
			}
		case <-time.After(r.opt.pongTimeout):
			r.fail(errors.New("relay: pong timeout"))
			return
		case <-r.done:
			return
		}
		r.pongMu.Lock()
		r.pongCh = nil
		r.pongMu.Unlock()
	}
}

func (r *Relay) awaitOpened(c *client, ack chan sendAck) {
	defer r.wg.Done()
	select {
	case a := <-ack:
		if a.err != nil {
			return
		}
	case <-r.done:
		return
	}
	select {
	case <-c.openedCh:
	case <-c.guestDoneCh:
	case <-time.After(r.opt.confirmTimeout):
		r.fail(errors.New("relay: guest did not confirm session open"))
	case <-r.done:
	}
}

func (r *Relay) awaitClosed(c *client, ack chan sendAck) {
	defer r.wg.Done()
	select {
	case a := <-ack:
		if a.err != nil {
			return
		}
	case <-r.done:
		return
	}
	select {
	case <-c.guestDoneCh:
	case <-time.After(r.opt.confirmTimeout):
		r.fail(errors.New("relay: guest did not confirm session close"))
	case <-r.done:
	}
}

// readTimed reads one frame, closing the stream when the deadline passes, and
// waits for the helper goroutine to finish after the close.
func readTimed(r io.Reader, closer io.Closer, d time.Duration) (agentproto.Frame, error) {
	type result struct {
		f   agentproto.Frame
		err error
	}
	ch := make(chan result, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := agentproto.ReadFrame(r)
		ch <- result{f, err}
	}()
	select {
	case res := <-ch:
		return res.f, res.err
	case <-time.After(d):
		_ = closer.Close()
		<-done
		return agentproto.Frame{}, fmt.Errorf("relay: read timeout after %s", d)
	}
}

// writeTimed writes one frame and closes the stream if the write blocks past
// the deadline, releasing the writer.
func writeTimed(w io.Writer, closer io.Closer, f agentproto.Frame, d time.Duration) error {
	timer := time.AfterFunc(d, func() { _ = closer.Close() })
	err := agentproto.WriteFrame(w, f)
	timer.Stop()
	return err
}
