package m3probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func streamHash(id uint16, n int) string {
	h := sha256.New()
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = streamByte(i, streamSeed(id))
	}
	h.Write(buf)
	return hex.EncodeToString(h.Sum(nil))
}

type splitRW struct {
	r      io.Reader
	w      io.Writer
	closer func() error
}

func (s splitRW) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s splitRW) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s splitRW) Close() error                { return s.closer() }

// TestHelperServe is the guest side of TestMeasureHelperProcessStdio: it runs
// the real Serve over the test binary's stdin/stdout.
func TestHelperServe(t *testing.T) {
	if os.Getenv("M3PROBE_HELPER") != "1" {
		return
	}
	rw := splitRW{r: os.Stdin, w: os.Stdout, closer: func() error {
		os.Stdin.Close()
		return os.Stdout.Close()
	}}
	if err := Serve(rw); err != nil {
		panic("helper serve: " + err.Error())
	}
	os.Exit(0)
}

func TestMeasureOverPipe(t *testing.T) {
	host, guest := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(guest) }()

	cfg := MeasureConfig{Nonce: []byte("pipe-run"), PingInterval: time.Millisecond, Timeout: 60 * time.Second}
	rep, err := Measure(host, cfg)
	if err != nil {
		t.Fatalf("measure: %v (errors %v)", err, rep.Errors)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("guest Serve did not stop after Finish/Ack")
	}

	if rep.Version != 1 || rep.NonceSHA256 == "" {
		t.Fatalf("report header = %+v", rep)
	}
	if rep.EchoSamples != defaultEchoCount || rep.EchoMaxUS < rep.EchoP50US || rep.EchoP95US < rep.EchoP50US {
		t.Fatalf("echo metrics = %+v", rep)
	}
	if rep.PingsSent != rep.PingsOK+rep.PingsMissed {
		t.Fatalf("ping counts sent=%d ok=%d missed=%d late=%d", rep.PingsSent, rep.PingsOK, rep.PingsMissed, rep.PingsLate)
	}
	if rep.BulkPingSamples+rep.PingsAfterStream != rep.PingsOK {
		t.Fatalf("bulk samples=%d after=%d ok=%d", rep.BulkPingSamples, rep.PingsAfterStream, rep.PingsOK)
	}
	if rep.BulkPingObserved != (rep.BulkPingSamples > 0) {
		t.Fatalf("bulk observed=%v samples=%d", rep.BulkPingObserved, rep.BulkPingSamples)
	}
	if rep.PingsOK < 1 || !rep.BulkPingObserved {
		t.Fatalf("no bulk ping reply observed: sent=%d ok=%d after=%d late=%d", rep.PingsSent, rep.PingsOK, rep.PingsAfterStream, rep.PingsLate)
	}
	if rep.BulkPingMaxUS < rep.BulkPingP50US || rep.BulkPingP95US < rep.BulkPingP50US {
		t.Fatalf("bulk ping metrics = %+v", rep)
	}
	if len(rep.Streams) != 2 {
		t.Fatalf("streams = %+v", rep.Streams)
	}
	for _, st := range rep.Streams {
		if st.Bytes != defaultStreamBytes || st.Chunks != defaultStreamBytes/StreamChunk {
			t.Fatalf("stream %d = %+v", st.ID, st)
		}
		wantHash := streamHash(st.ID, defaultStreamBytes)
		if st.SentSHA256 != wantHash || st.ReceivedSHA256 != wantHash {
			t.Fatalf("stream %d hash sent=%s received=%s want=%s", st.ID, st.SentSHA256, st.ReceivedSHA256, wantHash)
		}
	}
	if rep.InterleaveSwitches < 2 {
		t.Fatalf("interleave switches = %d", rep.InterleaveSwitches)
	}
	if rep.StreamMs < 0 || rep.AggregateBytesPerS <= 0 {
		t.Fatalf("stream timing = %+v", rep)
	}
}

func TestMeasureRejectsWrongIDStream(t *testing.T) {
	host, guest := net.Pipe()
	go func() {
		defer guest.Close()
		f, err := ReadFrame(guest)
		if err != nil {
			return
		}
		WriteFrame(guest, Frame{Version: Version, Type: TypeReady, ID: ControlID, Payload: f.Payload})
		starts := 0
		for {
			f, err := ReadFrame(guest)
			if err != nil {
				return
			}
			switch f.Type {
			case TypeEcho:
				WriteFrame(guest, Frame{Version: Version, Type: TypeEchoReply, ID: ControlID, Payload: f.Payload})
			case TypeStart:
				starts++
				if starts == 2 {
					p := make([]byte, StreamChunk)
					for i := range p {
						p[i] = streamByte(i, streamSeed(LastStreamID))
					}
					WriteFrame(guest, Frame{Version: Version, Type: TypeStream, ID: FirstStreamID, Payload: p})
				}
			}
		}
	}()
	rep, err := Measure(host, MeasureConfig{Nonce: []byte("swap-run"), EchoCount: 1, StreamBytes: 8 << 10, Timeout: 10 * time.Second})
	if !errors.Is(err, ErrBadPayload) {
		t.Fatalf("err = %v, report %+v", err, rep)
	}
}

func TestMeasureWrongNonce(t *testing.T) {
	host, guest := net.Pipe()
	// A fake guest that reflects the nonce with an extra byte: the host must
	// reject the mismatch instead of continuing.
	go func() {
		f, err := ReadFrame(guest)
		if err != nil {
			return
		}
		WriteFrame(guest, Frame{Version: Version, Type: TypeReady, ID: ControlID, Payload: append(f.Payload, 'x')})
		ReadFrame(guest)
		guest.Close()
	}()
	rep, err := Measure(host, MeasureConfig{Nonce: []byte("host-nonce"), Timeout: 5 * time.Second})
	if !errors.Is(err, ErrBadPayload) {
		t.Fatalf("err = %v, report %+v", err, rep)
	}
	if len(rep.Errors) == 0 {
		t.Fatal("expected a recorded error")
	}
}

type blockWire struct {
	ch   chan struct{}
	once sync.Once
}

func newBlockWire() *blockWire { return &blockWire{ch: make(chan struct{})} }

func (b *blockWire) Read(p []byte) (int, error)  { <-b.ch; return 0, io.EOF }
func (b *blockWire) Write(p []byte) (int, error) { return len(p), nil }
func (b *blockWire) Close() error {
	b.once.Do(func() { close(b.ch) })
	return nil
}

func TestMeasureTimeoutReleasesBlockedRead(t *testing.T) {
	begin := time.Now()
	_, err := Measure(newBlockWire(), MeasureConfig{Nonce: []byte("blocked"), Timeout: 150 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want timeout", err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("timeout release took %s", d)
	}
}

type scriptedWire struct {
	r *bytes.Reader
	w bytes.Buffer
}

func (s *scriptedWire) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *scriptedWire) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s *scriptedWire) Close() error                { return nil }

func helloFrame(nonce string) []byte {
	return append(rawHeader(Version, byte(TypeHello), 0, len(nonce)), []byte(nonce)...)
}

func TestServeRejectsBadFrames(t *testing.T) {
	start := func(id uint16, n int) []byte {
		return append(rawHeader(Version, byte(TypeStart), id, 4), encodeStartBytes(n)...)
	}
	withHello := func(frames ...[]byte) []byte {
		out := helloFrame("n")
		for _, f := range frames {
			out = append(out, f...)
		}
		return out
	}
	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		{"echo before hello", rawHeader(Version, byte(TypeEcho), 0, 0), ErrBadState},
		{"bad hello type", rawHeader(Version, byte(TypeReady), 0, 0), ErrBadState},
		{"empty hello", rawHeader(Version, byte(TypeHello), 0, 0), ErrBadPayload},
		{"long hello", append(rawHeader(Version, byte(TypeHello), 0, MaxNonceBytes+1), make([]byte, MaxNonceBytes+1)...), ErrBadPayload},
		{"echo bad id", withHello(rawHeader(Version, byte(TypeEcho), 1, 0)), ErrBadID},
		{"start bad id", withHello(start(3, 1)), ErrBadID},
		{"start zero bytes", withHello(start(1, 0)), ErrBadPayload},
		{"start short payload", withHello(append(rawHeader(Version, byte(TypeStart), 1, 3), 0, 0, 1)), ErrBadPayload},
		{"duplicate start", withHello(start(1, 1), start(1, 1)), ErrBadState},
		{"unexpected ack", withHello(rawHeader(Version, byte(TypeAck), 0, 0)), ErrBadType},
		{"finish with payload", withHello(append(rawHeader(Version, byte(TypeFinish), 0, 1), 1)), ErrBadPayload},
		{"finish while active", withHello(start(1, MaxStreamBytes), rawHeader(Version, byte(TypeFinish), 0, 0)), ErrBadState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := &scriptedWire{r: bytes.NewReader(tc.raw)}
			if err := Serve(wire); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestServeEOFVsTruncated(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		{"empty clean eof", nil, nil},
		{"eof after hello", helloFrame("n"), nil},
		{"partial header", append(helloFrame("n"), 1, 2, 3), ErrTruncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := &scriptedWire{r: bytes.NewReader(tc.raw)}
			err := Serve(wire)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want clean EOF", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestServeBadFrameWhileBulkBlocked covers the old hang: the pump is blocked
// writing a stream frame, so an invalid frame must close the wire to release
// it instead of waiting forever.
func TestServeBadFrameWhileBulkBlocked(t *testing.T) {
	host, guest := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(guest) }()
	if err := WriteFrame(host, Frame{Version: Version, Type: TypeHello, ID: ControlID, Payload: []byte("n")}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrame(host); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(host, Frame{Version: Version, Type: TypeStart, ID: FirstStreamID, Payload: encodeStartBytes(MaxStreamBytes)}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the pump block on the unread stream write
	if err := WriteFrame(host, Frame{Version: Version, Type: TypeAck, ID: ControlID}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrBadType) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve hung with a blocked pump write")
	}
	host.Close()
}

func TestServeStopsOnDisconnect(t *testing.T) {
	host, guest := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(guest) }()
	if err := WriteFrame(host, Frame{Version: Version, Type: TypeHello, ID: ControlID, Payload: []byte("n")}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrame(host); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := WriteFrame(host, Frame{Version: Version, Type: TypeStart, ID: FirstStreamID, Payload: encodeStartBytes(MaxStreamBytes)}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := ReadFrame(host); err != nil {
		t.Fatalf("first stream frame: %v", err)
	}
	host.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after disconnect")
	}
}

type pipeWire struct {
	r    io.ReadCloser
	w    io.WriteCloser
	once sync.Once
}

func (p *pipeWire) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeWire) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeWire) Close() error {
	p.once.Do(func() {
		p.w.Close()
		p.r.Close()
	})
	return nil
}

func TestMeasureHelperProcessStdio(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperServe")
	cmd.Env = append(os.Environ(), "M3PROBE_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wire := &pipeWire{r: stdout, w: stdin}
	rep, err := Measure(wire, MeasureConfig{
		Nonce:        []byte("stdio-run"),
		EchoCount:    4,
		StreamBytes:  8 << 10,
		Timeout:      30 * time.Second,
		PingInterval: time.Millisecond,
	})
	if err != nil {
		cmd.Wait()
		t.Fatalf("measure: %v (errors %v, stderr %q)", err, rep.Errors, stderr.String())
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		if err != nil {
			t.Fatalf("helper exit: %v (stderr %q)", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("helper did not exit")
	}
	if len(rep.Streams) != 2 || rep.Streams[0].Bytes != 8<<10 {
		t.Fatalf("streams = %+v", rep.Streams)
	}
	if rep.PingsSent != rep.PingsOK+rep.PingsMissed || rep.BulkPingSamples+rep.PingsAfterStream != rep.PingsOK {
		t.Fatalf("ping counts = %+v", rep)
	}
}
