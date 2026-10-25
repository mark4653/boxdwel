//go:build linux

// Command m3-transport-probe is the guest side of the M3 COM2 transport spike.
// It is experimental and not the product agent protocol.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mark4653/boxdwel/internal/m3probe"
)

// wire closes in a fixed order: termios restore first, then the descriptors,
// so Close both releases blocked I/O and leaves the tty as it was.
type wire struct {
	r      io.Reader
	w      io.Writer
	closer []func() error
	once   sync.Once
}

func (w *wire) Read(p []byte) (int, error)  { return w.r.Read(p) }
func (w *wire) Write(p []byte) (int, error) { return w.w.Write(p) }

func (w *wire) Close() error {
	w.once.Do(func() {
		for _, fn := range w.closer {
			_ = fn()
		}
	})
	return nil
}

func main() { os.Exit(run()) }

func run() int {
	dev := flag.String("dev", "/dev/ttyS1", "serial device carrying the wire")
	stdio := flag.Bool("stdio", false, "use stdin/stdout as the wire (bounded local check)")
	deadline := flag.Duration("deadline", 120*time.Second, "global self-deadline (max 120s)")
	flag.Parse()

	if *deadline <= 0 || *deadline > 120*time.Second {
		fmt.Fprintln(os.Stderr, "m3probe: deadline must be within 1s..120s")
		return 2
	}

	var (
		w     *wire
		label string
	)
	if *stdio {
		w = &wire{r: os.Stdin, w: os.Stdout}
		label = "stdio"
		w.closer = append(w.closer, os.Stdin.Close, os.Stdout.Close)
		if isTerminal(os.Stdin.Fd()) {
			old, err := makeRaw(os.Stdin.Fd())
			if err != nil {
				fmt.Fprintf(os.Stderr, "m3probe: raw stdio: %v\n", err)
				return 1
			}
			w.closer = append([]func() error{func() error { return restoreTermios(os.Stdin.Fd(), old) }}, w.closer...)
		}
	} else {
		// O_NONBLOCK lets os.NewFile register the tty with Go's poller, so a
		// Close from the deadline path releases a blocked read/write.
		fd, err := syscall.Open(*dev, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "m3probe: open %s: %v\n", *dev, err)
			return 1
		}
		f := os.NewFile(uintptr(fd), *dev)
		old, err := makeRaw(uintptr(fd))
		if err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "m3probe: raw %s: %v\n", *dev, err)
			return 1
		}
		w = &wire{r: f, w: f, closer: []func() error{
			func() error { return restoreTermios(uintptr(fd), old) },
			f.Close,
		}}
		label = *dev
	}

	var hitDeadline atomic.Bool
	timer := time.AfterFunc(*deadline, func() {
		hitDeadline.Store(true)
		w.Close()
	})
	defer timer.Stop()
	// Last-resort bound if Close ever fails to wake a blocked callback; this
	// is a test probe, not the product agent.
	hard := time.AfterFunc(*deadline+2*time.Second, func() {
		fmt.Fprintln(os.Stderr, "m3probe: hard deadline, exiting")
		os.Exit(1)
	})
	defer hard.Stop()

	fmt.Fprintf(os.Stderr, "m3probe: wire=%s deadline=%s\n", label, deadline.String())

	if err := m3probe.Serve(w); err != nil {
		if hitDeadline.Load() {
			fmt.Fprintln(os.Stderr, "m3probe: guest deadline reached")
		} else {
			fmt.Fprintf(os.Stderr, "m3probe: guest error: %v\n", err)
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "m3probe: guest done")
	return 0
}
