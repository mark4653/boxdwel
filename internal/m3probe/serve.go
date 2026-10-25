package m3probe

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
	"time"
)

type streamState struct {
	id        uint16
	remaining int
	sent      int
	hash      hash.Hash
}

type server struct {
	send     func(Type, uint16, []byte) error
	close    func()
	mu       sync.Mutex
	streams  map[uint16]*streamState
	started  map[uint16]bool
	order    []uint16
	active   int
	err      error
	stop     chan struct{}
	pump     sync.Once
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// Serve runs the guest side: wait for Hello, reflect its nonce in Ready, serve
// streams, and finish cleanly on Finish/Ack. It takes ownership of rw and
// always closes it; on every exit it closes the wire first so a blocked pump
// write is released, then joins the pump. Fail closed: no resync.
func Serve(rw io.ReadWriteCloser) (ret error) {
	var (
		closeOnce sync.Once
		writeMu   sync.Mutex
	)
	closeWire := func() { closeOnce.Do(func() { _ = rw.Close() }) }
	send := func(t Type, id uint16, p []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return WriteFrame(rw, Frame{Version: Version, Type: t, ID: id, Payload: p})
	}
	s := &server{
		send:    send,
		close:   closeWire,
		streams: make(map[uint16]*streamState),
		started: make(map[uint16]bool),
		stop:    make(chan struct{}),
	}
	defer func() {
		closeWire()
		s.stopPump()
		if s.sendErr() != nil && ret == nil {
			ret = s.sendErr()
		}
	}()

	f, err := ReadFrame(rw)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if f.Type != TypeHello || f.ID != ControlID {
		return fmt.Errorf("%w: first frame type %d", ErrBadState, f.Type)
	}
	if len(f.Payload) == 0 || len(f.Payload) > MaxNonceBytes {
		return fmt.Errorf("%w: nonce length %d", ErrBadPayload, len(f.Payload))
	}
	if err := send(TypeReady, ControlID, f.Payload); err != nil {
		return err
	}

	for {
		f, err := ReadFrame(rw)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch f.Type {
		case TypeEcho:
			if f.ID != ControlID {
				return fmt.Errorf("%w: echo id %d", ErrBadID, f.ID)
			}
			if err := send(TypeEchoReply, ControlID, f.Payload); err != nil {
				return err
			}
		case TypeStart:
			if err := s.start(f); err != nil {
				return err
			}
		case TypeFinish:
			if f.ID != ControlID || len(f.Payload) != 0 {
				return fmt.Errorf("%w: finish id %d payload %d", ErrBadPayload, f.ID, len(f.Payload))
			}
			s.mu.Lock()
			active := s.active
			s.mu.Unlock()
			if active != 0 {
				return fmt.Errorf("%w: finish with %d active streams", ErrBadState, active)
			}
			s.stopPump()
			if err := send(TypeAck, ControlID, nil); err != nil {
				return err
			}
			return nil
		default:
			return fmt.Errorf("%w: guest received type %d", ErrBadType, f.Type)
		}
	}
}

func (s *server) start(f Frame) error {
	if f.ID < FirstStreamID || f.ID > LastStreamID {
		return fmt.Errorf("%w: %d", ErrBadID, f.ID)
	}
	n, err := decodeStartBytes(f.Payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.started[f.ID] {
		s.mu.Unlock()
		return fmt.Errorf("%w: stream %d started twice", ErrBadState, f.ID)
	}
	s.started[f.ID] = true
	s.streams[f.ID] = &streamState{id: f.ID, remaining: n, hash: sha256.New()}
	s.order = append(s.order, f.ID)
	s.active++
	s.mu.Unlock()
	s.pump.Do(func() {
		s.wg.Add(1)
		go s.pumpLoop()
	})
	return nil
}

// pumpLoop writes active streams round robin so both ids interleave on one
// wire; Echo replies still interleave because each frame takes the write lock.
func (s *server) pumpLoop() {
	defer s.wg.Done()
	turn := 0
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		s.mu.Lock()
		var active []*streamState
		for _, id := range s.order {
			if st := s.streams[id]; st != nil && st.remaining > 0 {
				active = append(active, st)
			}
		}
		var st *streamState
		if len(active) > 0 {
			st = active[turn%len(active)]
			turn++
		}
		s.mu.Unlock()
		if st == nil {
			select {
			case <-s.stop:
				return
			case <-time.After(time.Millisecond):
				continue
			}
		}
		n := st.remaining
		if n > StreamChunk {
			n = StreamChunk
		}
		chunk := make([]byte, n)
		for i := range chunk {
			chunk[i] = streamByte(st.sent+i, streamSeed(st.id))
		}
		if err := s.send(TypeStream, st.id, chunk); err != nil {
			s.fail(err)
			s.close()
			return
		}
		st.hash.Write(chunk)
		st.sent += n
		st.remaining -= n
		done := st.remaining == 0
		total := st.sent
		var sum [32]byte
		if done {
			copy(sum[:], st.hash.Sum(nil))
			// Mark the stream complete before StreamEnd is on the wire, so a
			// Finish that follows the host's StreamEnd cannot see it active.
			s.mu.Lock()
			delete(s.streams, st.id)
			s.active--
			s.mu.Unlock()
			if err := s.send(TypeStreamEnd, st.id, encodeStreamEnd(total, sum)); err != nil {
				s.fail(err)
				s.close()
				return
			}
		}
	}
}

func (s *server) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *server) sendErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *server) stopPump() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}
