package m3probe

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// startupPreamble is the exact 58 bytes captured from the first M3 Windows run
// in serial-console.log (firmware text-mode setup: ESC[2J, ESC[01;01H, ESC[=3h
// repeats). Source:
//
//	a local M3 diagnostics run (serial-console.log)
//
// This is a startup-precondition fixture only: it is not a full COM2 capture
// and contains no payload data. The real fix is expected to be a connection-time
// gate, not a parser relaxation; the PS/host seam that applies such a gate is
// not proven here because no Windows runtime is available.
const (
	startupPreambleHex    = "1b5b324a1b5b30313b3031481b5b3d33681b5b324a1b5b30313b3031481b5b324a1b5b30313b3031481b5b3d33681b5b324a1b5b30313b303148"
	startupPreambleSHA256 = "609a4e420949c513f360951512668dbf1a5a470e67b50b868edaf5113effa2c0"
)

type startupWire struct {
	pending []byte
	rw      io.ReadWriteCloser
}

func (w *startupWire) Read(p []byte) (int, error) {
	if len(w.pending) > 0 {
		n := copy(p, w.pending)
		w.pending = w.pending[n:]
		return n, nil
	}
	return w.rw.Read(p)
}

func (w *startupWire) Write(p []byte) (int, error) { return w.rw.Write(p) }
func (w *startupWire) Close() error                { return w.rw.Close() }

func startupConfig(nonce string) MeasureConfig {
	return MeasureConfig{
		Nonce:        []byte(nonce),
		EchoCount:    4,
		StreamBytes:  8 << 10,
		Timeout:      10 * time.Second,
		PingInterval: time.Millisecond,
	}
}

// TestMeasureStartupReplay pins the startup-precondition contract: boot bytes
// on the wire are a protocol error, not silently skipped or resynced.
func TestMeasureStartupReplay(t *testing.T) {
	preamble, err := hex.DecodeString(startupPreambleHex)
	if err != nil {
		t.Fatal(err)
	}
	if len(preamble) != 58 {
		t.Fatalf("startup fixture length = %d, want 58", len(preamble))
	}
	if got := sha256.Sum256(preamble); hex.EncodeToString(got[:]) != startupPreambleSHA256 {
		t.Fatalf("startup fixture hash mismatch: %x", got)
	}

	host, guest := net.Pipe()
	go func() { _ = Serve(guest) }()
	wire := &startupWire{pending: append([]byte(nil), preamble...), rw: host}
	_, err = Measure(wire, startupConfig("startup-replay"))
	if !errors.Is(err, ErrBadVersion) || !strings.Contains(err.Error(), "0x1b") {
		t.Fatalf("err = %v, want ErrBadVersion 0x1b (boot bytes must not be skipped)", err)
	}
}

// TestMeasureWithoutStartupPreamble is the control: with no prefix the same
// local Serve peer measures normally, so the replay failure is startup-order
// specific, not a parser/measurement defect.
func TestMeasureWithoutStartupPreamble(t *testing.T) {
	host, guest := net.Pipe()
	go func() { _ = Serve(guest) }()
	rep, err := Measure(host, startupConfig("no-prefix"))
	if err != nil {
		t.Fatalf("no-prefix measure: %v", err)
	}
	if len(rep.Streams) != 2 {
		t.Fatalf("streams = %+v", rep.Streams)
	}
}
