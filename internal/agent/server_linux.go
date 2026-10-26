// Package agent is the Linux guest side of the product protocol: the session
// manager and real PTY shells. See docs/agent-protocol.md.
package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mark4653/boxdwel/internal/agentproto"
)

const (
	defaultWriteTimeout = 10 * time.Second
	defaultInputTimeout = 5 * time.Second
	defaultExitWait     = 3 * time.Second
	defaultExitDrain    = 500 * time.Millisecond
	defaultExitDrainMax = 30 * time.Second
	inputQueueFrames    = 8
)

type options struct {
	writeTimeout time.Duration
	inputTimeout time.Duration
	exitWait     time.Duration
	exitDrain    time.Duration
	exitDrainMax time.Duration
}

type session struct {
	id       uint16
	pty      *pty
	in       chan []byte
	done     chan struct{}
	readDone chan struct{}

	stopOnce sync.Once
	doneOnce sync.Once
	readOnce sync.Once

	// frameMu serializes this session's Output sends with its final frame, so
	// Error/Exited is always the last frame for the session.
	frameMu   sync.Mutex
	finalSent bool

	mu       sync.Mutex
	reason   uint8
	errored  bool
	closing  bool
	reaped   bool
	waitCh   chan struct{}
	waitCode int32
	waitErr  error
}

// stop kills the shell once and releases its PTY, including a blocked input
// write and pending output read.
func (s *session) stop(reason uint8) {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.reason = reason
		s.mu.Unlock()
		s.closeDone()
		if !s.isReaped() {
			s.pty.kill()
		}
		s.pty.closeMaster()
	})
}

func (s *session) closeDone() { s.doneOnce.Do(func() { close(s.done) }) }

func (s *session) closePTY() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.pty.closeMaster()
}

func (s *session) exited() bool {
	select {
	case <-s.waitCh:
		return true
	default:
		return false
	}
}

func (s *session) isReaped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reaped
}

func (s *session) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

func (s *session) end() (uint8, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason, !s.errored
}

func (s *session) markErrored() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errored {
		return false
	}
	s.errored = true
	return true
}

// outputFrame sends one Output frame unless the final frame already went out.
func (s *session) outputFrame(send func() error) error {
	s.frameMu.Lock()
	defer s.frameMu.Unlock()
	if s.finalSent {
		return errSessionFinal
	}
	return send()
}

// finalFrame sets the barrier and sends the one final frame. Later Output is
// dropped, so Error/Exited cannot be followed by data for this session.
func (s *session) finalFrame(send func()) bool {
	s.frameMu.Lock()
	if s.finalSent {
		s.frameMu.Unlock()
		return false
	}
	s.finalSent = true
	s.frameMu.Unlock()
	send()
	return true
}

// reap waits for the single owner of cmd.Wait.
func (s *session) reap(timeout time.Duration) (int32, error) {
	select {
	case <-s.waitCh:
		return s.waitCode, s.waitErr
	case <-time.After(timeout):
		return -1, fmt.Errorf("shell did not exit within %s", timeout)
	}
}

type server struct {
	conn io.ReadWriteCloser
	opt  options

	writeMu sync.Mutex
	errMu   sync.Mutex
	err     error
	errSet  bool

	mu      sync.Mutex
	active  map[uint16]*session
	retired map[uint16]struct{}
	wg      sync.WaitGroup
}

// Serve runs the guest side against one byte stream that unblocks pending I/O
// on Close (a pollable os.File or a net.Conn). It owns rw and always closes
// it; a clean EOF at a frame boundary returns nil.
func Serve(rw io.ReadWriteCloser) error {
	return serve(rw, options{
		writeTimeout: defaultWriteTimeout,
		inputTimeout: defaultInputTimeout,
		exitWait:     defaultExitWait,
		exitDrain:    defaultExitDrain,
		exitDrainMax: defaultExitDrainMax,
	})
}

func serve(rw io.ReadWriteCloser, opt options) error {
	s := &server{
		conn:    rw,
		opt:     opt,
		active:  make(map[uint16]*session),
		retired: make(map[uint16]struct{}),
	}
	defer func() {
		_ = rw.Close()
		s.shutdown()
	}()

	// readEnd classifies a receive-loop error: a peer close, including one
	// that first surfaced as a closed-pipe write, is a clean end. A timeout
	// or any other transport failure is reported.
	readEnd := func(err error) error {
		terr := s.transportErr()
		if err == io.EOF {
			// A peer close at a frame boundary is clean unless a write really
			// failed (for example a timeout) instead of a closed pipe.
			if terr != nil && !peerGone(terr) {
				return terr
			}
			return nil
		}
		// A read error is never a clean end, even if the read side was closed
		// by a write failure.
		if terr != nil {
			return terr
		}
		return err
	}

	f, err := agentproto.ReadFrame(rw)
	if err != nil {
		return readEnd(err)
	}
	if err := f.FromHost(); err != nil {
		return err
	}
	if f.Type != agentproto.TypeHello {
		return fmt.Errorf("%w: first frame %d", agentproto.ErrBadShape, f.Type)
	}
	if err := s.send(agentproto.Frame{
		Type:    agentproto.TypeReady,
		Control: f.Control,
		Payload: agentproto.ReadyPayload(),
	}); err != nil {
		return err
	}

	for {
		f, err := agentproto.ReadFrame(rw)
		if err != nil {
			return readEnd(err)
		}
		if err := f.FromHost(); err != nil {
			_ = s.sendError(0, agentproto.CodeProtocol, err.Error())
			return err
		}
		switch f.Type {
		case agentproto.TypePing:
			if err := s.send(agentproto.Frame{Type: agentproto.TypePong, Control: f.Control, Payload: f.Payload}); err != nil {
				return err
			}
		case agentproto.TypeOpen:
			if err := s.open(f); err != nil {
				_ = s.sendError(0, agentproto.CodeProtocol, err.Error())
				return err
			}
		case agentproto.TypeInput:
			if err := s.input(f.Session, f.Payload); err != nil {
				_ = s.sendError(0, agentproto.CodeProtocol, err.Error())
				return err
			}
		case agentproto.TypeResize:
			if err := s.resize(f.Session, f.Payload); err != nil {
				_ = s.sendError(0, agentproto.CodeProtocol, err.Error())
				return err
			}
		case agentproto.TypeClose:
			if err := s.close(f.Session); err != nil {
				_ = s.sendError(0, agentproto.CodeProtocol, err.Error())
				return err
			}
		default:
			return fmt.Errorf("%w: unexpected %d", agentproto.ErrBadType, f.Type)
		}
	}
}

func (s *server) abort(err error) {
	s.errMu.Lock()
	first := !s.errSet
	if first {
		s.err = err
		s.errSet = true
	}
	s.errMu.Unlock()
	if first {
		_ = s.conn.Close()
	}
}

func (s *server) transportErr() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// send writes one frame. A blocked write is bounded by the write timeout,
// which closes the whole connection so no goroutine waits forever.
func (s *server) send(f agentproto.Frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.transportErr(); err != nil {
		return err
	}
	timer := time.AfterFunc(s.opt.writeTimeout, func() {
		s.abort(fmt.Errorf("agent: wire write timed out after %s", s.opt.writeTimeout))
	})
	err := agentproto.WriteFrame(s.conn, f)
	timer.Stop()
	if err != nil {
		s.abort(err)
		return err
	}
	return nil
}

func (s *server) sendError(session uint16, code uint8, message string) error {
	return s.send(agentproto.Frame{
		Type:    agentproto.TypeError,
		Session: session,
		Payload: agentproto.ErrorPayload(code, message),
	})
}

func (s *server) lookup(id uint16) (*session, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.active[id]; ok {
		return sess, true, false
	}
	_, retired := s.retired[id]
	return nil, false, retired
}

func (s *server) open(f agentproto.Frame) error {
	rows, cols, err := agentproto.ParseSize(f.Payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	_, active := s.active[f.Session]
	_, retired := s.retired[f.Session]
	if active || retired {
		s.mu.Unlock()
		return fmt.Errorf("%w: session %d reuse", agentproto.ErrBadShape, f.Session)
	}
	if len(s.active) >= agentproto.MaxSessions {
		s.mu.Unlock()
		return s.sendError(f.Session, agentproto.CodeSessionLimit, "session limit reached")
	}
	p, err := startShell(rows, cols)
	if err != nil {
		s.mu.Unlock()
		return s.sendError(f.Session, agentproto.CodeSpawn, "pty start failed")
	}
	sess := &session{
		id:       f.Session,
		pty:      p,
		in:       make(chan []byte, inputQueueFrames),
		done:     make(chan struct{}),
		waitCh:   make(chan struct{}),
		readDone: make(chan struct{}),
	}
	go func() {
		code, werr := p.wait()
		sess.mu.Lock()
		sess.waitCode, sess.waitErr = code, werr
		sess.reaped = true
		sess.mu.Unlock()
		close(sess.waitCh)
	}()
	s.active[f.Session] = sess
	s.wg.Add(3)
	s.mu.Unlock()

	if err := s.send(agentproto.Frame{Type: agentproto.TypeOpened, Session: f.Session, Payload: f.Payload}); err != nil {
		sess.stop(agentproto.ReasonClosed)
	}
	go s.readLoop(sess)
	go s.inputLoop(sess)
	go s.exitWatcher(sess)
	return nil
}

// exitWatcher bounds the post-exit drain. A read that started before the exit
// is woken with one quiet window; the read loop re-arms it while output keeps
// arriving, so a slow wire can finish the tail. If output never stops, the
// total cap ends the session with Error code 4 rather than a successful
// Exited that would hide the lost bytes.
func (s *server) exitWatcher(sess *session) {
	defer s.wg.Done()
	select {
	case <-sess.waitCh:
	case <-sess.readDone:
		return
	}
	_ = sess.pty.master.SetReadDeadline(time.Now().Add(s.opt.exitDrain))
	timer := time.NewTimer(s.opt.exitDrainMax)
	defer timer.Stop()
	select {
	case <-sess.readDone:
	case <-timer.C:
		if sess.markErrored() {
			sess.finalFrame(func() {
				_ = s.sendError(sess.id, agentproto.CodeTimeout, "output drain timeout")
			})
			sess.stop(agentproto.ReasonClosed)
		}
	}
}

func (s *server) input(id uint16, payload []byte) error {
	sess, ok, retired := s.lookup(id)
	if !ok {
		if retired {
			return nil // a late frame for a closed session
		}
		return fmt.Errorf("%w: input for unknown session %d", agentproto.ErrBadShape, id)
	}
	if !enqueue(sess.in, payload) {
		s.failSession(sess, agentproto.CodeInput, "input queue full")
	}
	return nil
}

func (s *server) resize(id uint16, payload []byte) error {
	rows, cols, err := agentproto.ParseSize(payload)
	if err != nil {
		return err
	}
	sess, ok, retired := s.lookup(id)
	if !ok {
		if retired {
			return nil
		}
		return fmt.Errorf("%w: resize for unknown session %d", agentproto.ErrBadShape, id)
	}
	if sess.isClosing() {
		return nil
	}
	if err := sess.pty.resize(rows, cols); err != nil {
		if sess.isClosing() {
			return nil // raced with session end
		}
		s.failSession(sess, agentproto.CodeProtocol, "pty resize failed")
	}
	return nil
}

func (s *server) close(id uint16) error {
	sess, ok, retired := s.lookup(id)
	switch {
	case ok:
		sess.stop(agentproto.ReasonClosed)
	case retired:
	default:
		return fmt.Errorf("%w: close for unknown session %d", agentproto.ErrBadShape, id)
	}
	return nil
}

// failSession reports an input failure and ends only that session.
func (s *server) failSession(sess *session, code uint8, message string) {
	if sess.markErrored() {
		sess.stop(agentproto.ReasonClosed)
	}
	sess.finalFrame(func() { _ = s.sendError(sess.id, code, message) })
}

func (s *server) inputLoop(sess *session) {
	defer s.wg.Done()
	err := applyInput(sess.in, sess.done, s.opt.inputTimeout, sess.pty.write, func() {
		if sess.markErrored() {
			sess.stop(agentproto.ReasonClosed)
		}
	})
	switch {
	case err == nil:
	case err == errInputTimeout:
		// The timer marked the session errored and released the PTY; report
		// it once from this goroutine instead of the timer.
		sess.finalFrame(func() { _ = s.sendError(sess.id, agentproto.CodeInput, "input write timeout") })
	case sess.isClosing():
	default:
		s.failSession(sess, agentproto.CodeInput, "input write failed")
	}
}

var (
	errInputTimeout = errors.New("agent: input write timeout")
	errSessionFinal = errors.New("agent: session already sent its final frame")
)

// peerGone reports errors that only mean the other side closed the stream.
func peerGone(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed)
}

// enqueue never blocks the receive loop.
func enqueue(ch chan []byte, payload []byte) bool {
	select {
	case ch <- payload:
		return true
	default:
		return false
	}
}

// applyInput writes queued chunks one at a time. The queue is bounded by the
// session channel, so a stalled PTY never blocks the receive loop or grows
// memory; a write over timeout calls release to unblock it.
func applyInput(in <-chan []byte, done <-chan struct{}, timeout time.Duration, write func([]byte) error, release func()) error {
	for {
		select {
		case <-done:
			return nil
		case chunk := <-in:
			timer := time.AfterFunc(timeout, release)
			err := write(chunk)
			if !timer.Stop() {
				return errInputTimeout
			}
			if err != nil {
				return err
			}
		}
	}
}

func (s *server) readLoop(sess *session) {
	defer s.wg.Done()
	buf := make([]byte, agentproto.MaxOutputChunk)
	for {
		if sess.exited() {
			// Quiet-time drain: each read gets a fresh window, so a slow wire
			// never loses the tail to an absolute deadline.
			_ = sess.pty.master.SetReadDeadline(time.Now().Add(s.opt.exitDrain))
		}
		n, err := sess.pty.master.Read(buf)
		if n > 0 {
			serr := sess.outputFrame(func() error {
				return s.send(agentproto.Frame{Type: agentproto.TypeOutput, Session: sess.id, Payload: buf[:n]})
			})
			if serr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	sess.readOnce.Do(func() { close(sess.readDone) })
	s.finish(sess)
}

func (s *server) finish(sess *session) {
	code, err := sess.reap(s.opt.exitWait)
	if err != nil {
		sess.stop(agentproto.ReasonClosed)
		code, err = sess.reap(s.opt.exitWait)
	}
	sess.closePTY()
	sess.closeDone()

	s.mu.Lock()
	delete(s.active, sess.id)
	s.retired[sess.id] = struct{}{}
	s.mu.Unlock()

	reason, ok := sess.end()
	if !ok {
		return
	}
	sess.finalFrame(func() {
		if err != nil {
			_ = s.sendError(sess.id, agentproto.CodeTimeout, "shell did not exit")
			return
		}
		_ = s.send(agentproto.Frame{
			Type:    agentproto.TypeExited,
			Session: sess.id,
			Payload: agentproto.ExitedPayload(reason, code),
		})
	})
}

// shutdown stops every remaining session and joins the bounded readers.
func (s *server) shutdown() {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.active))
	for _, sess := range s.active {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.stop(agentproto.ReasonClosed)
	}
	s.wg.Wait()
}
