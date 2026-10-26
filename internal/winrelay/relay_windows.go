//go:build windows

package winrelay

import (
	"errors"
	"sync"
	"time"

	"github.com/mark4653/boxdwel/internal/relay"
	"github.com/mark4653/boxdwel/internal/winpipe"
)

// ServeOptions bounds the accept loop.
type ServeOptions struct {
	AcceptTimeout time.Duration
}

// Serve accepts verified pipe clients and attaches each stream to r until stop
// closes, the relay ends, or the listener fails. Serve owns both r and l: on
// every return it closes the listener, closes the relay, and joins the accept
// watcher and relay monitor. The caller must not use r or l afterwards;
// accepted streams are owned by the relay.
func Serve(r *relay.Relay, l *winpipe.Listener, stop <-chan struct{}, opt ServeOptions) error {
	if opt.AcceptTimeout <= 0 {
		opt.AcceptTimeout = 500 * time.Millisecond
	}

	relayDone := make(chan struct{})
	var relayErr error
	go func() {
		relayErr = r.Wait()
		close(relayDone)
	}()

	done := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		select {
		case <-stop:
		case <-relayDone:
		case <-done:
			return
		}
		_ = l.Close()
	}()
	defer func() {
		close(done)
		_ = l.Close()
		watcher.Wait()
		_ = r.Close()
		<-relayDone
	}()

	for {
		select {
		case <-stop:
			return nil
		case <-relayDone:
			return relayErr
		default:
		}
		s, err := l.Accept(opt.AcceptTimeout)
		if err != nil {
			if errors.Is(err, winpipe.ErrTimeout) {
				continue
			}
			select {
			case <-stop:
				return nil
			case <-relayDone:
				return relayErr
			default:
			}
			if errors.Is(err, winpipe.ErrClosed) {
				return nil
			}
			return err
		}
		if err := r.Attach(s); err != nil {
			// On a rejected attach the stream is still caller-owned.
			_ = s.Close()
			if errors.Is(err, relay.ErrClosed) {
				return nil
			}
			continue
		}
	}
}
