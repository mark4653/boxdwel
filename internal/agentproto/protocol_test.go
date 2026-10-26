package agentproto

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func mustSize(t *testing.T, rows, cols uint16) []byte {
	t.Helper()
	p, err := Size(rows, cols)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func frames(t *testing.T) []Frame {
	t.Helper()
	return []Frame{
		{Type: TypeHello, Control: 7},
		{Type: TypeReady, Control: 7, Payload: ReadyPayload()},
		{Type: TypePing, Control: 8, Payload: NoncePayload(42)},
		{Type: TypePong, Control: 8, Payload: NoncePayload(42)},
		{Type: TypeOpen, Session: 1, Payload: mustSize(t, 24, 80)},
		{Type: TypeOpened, Session: 1, Payload: mustSize(t, 24, 80)},
		{Type: TypeInput, Session: 1, Payload: []byte("echo hi\n")},
		{Type: TypeOutput, Session: 1, Payload: []byte("hi\n")},
		{Type: TypeResize, Session: 1, Payload: mustSize(t, 40, 100)},
		{Type: TypeClose, Session: 1},
		{Type: TypeExited, Session: 1, Payload: ExitedPayload(ReasonClosed, 3)},
		{Type: TypeError, Session: 1, Payload: ErrorPayload(CodeProtocol, "bad")},
		{Type: TypeError, Control: 4, Payload: ErrorPayload(CodeProtocol, "bad")},
	}
}

func TestRoundTrip(t *testing.T) {
	for _, want := range frames(t) {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, want); err != nil {
			t.Fatalf("%d: %v", want.Type, err)
		}
		if want.Type == TypeReady {
			if _, _, _, err := ParseReady(want.Payload); err != nil {
				t.Fatal(err)
			}
		}
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("%d: %v", want.Type, err)
		}
		if got.Type != want.Type || got.Control != want.Control || got.Session != want.Session ||
			!bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("frame %d: got %+v want %+v", want.Type, got, want)
		}
	}
}

func header(version byte, typ uint8, control, session, length uint16) []byte {
	return []byte{Magic0, Magic1, version, typ,
		byte(control >> 8), byte(control), byte(session >> 8), byte(session),
		byte(length >> 8), byte(length)}
}

func TestFrameErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		want error
	}{
		{"magic", []byte{0x01, 0x07, 0, 0, 0, 0, 0, 0, 0, 0}, ErrBadMagic},
		{"version", header(2, 1, 7, 0, 0), ErrBadVersion},
		{"length before alloc", header(1, 7, 0, 1, 4097), ErrBadPayload},
		{"unknown type", header(1, 99, 0, 1, 0), ErrBadType},
		{"hello with session", header(1, 1, 0, 1, 0), ErrBadShape},
		{"input without session", append(header(1, 7, 0, 0, 1), 1), ErrBadShape},
		{"truncated header", header(1, 1, 0, 0, 0)[:9], ErrTruncated},
		{"truncated payload", append(header(1, 7, 0, 1, 4), 1, 2), ErrTruncated},
		{"empty input", header(1, 7, 0, 1, 0), ErrBadPayload},
		{"output too big", append(header(1, 8, 0, 1, 1025), bytes.Repeat([]byte{1}, 1025)...), ErrBadPayload},
		{"short exited", append(header(1, 11, 0, 1, 4), 0, 0, 0, 0), ErrBadPayload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(tc.raw))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestExperimentalFrameRejected(t *testing.T) {
	// The first M3 probe frame starts with version 1 and type Hello (7).
	_, err := ReadFrame(bytes.NewReader([]byte{0x01, 0x07, 0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x00, 0x01}))
	if !errors.Is(err, ErrBadMagic) {
		t.Fatalf("got %v", err)
	}
}

func TestDirection(t *testing.T) {
	if err := (Frame{Type: TypeOutput, Session: 1}).FromHost(); err == nil {
		t.Fatal("output from host must fail")
	}
	if err := (Frame{Type: TypeInput, Session: 1}).FromGuest(); err == nil {
		t.Fatal("input from guest must fail")
	}
	if err := (Frame{Type: TypeError, Control: 1}).FromHost(); err == nil {
		t.Fatal("error from host must fail")
	}
	if err := (Frame{Type: TypeError, Control: 1}).FromGuest(); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadHelpers(t *testing.T) {
	if _, _, err := ParseSize([]byte{0, 0, 0, 0}); err == nil {
		t.Fatal("zero size must fail")
	}
	if _, err := Size(MaxDimension+1, 1); err == nil {
		t.Fatal("oversize dimension must fail")
	}
	if _, _, err := ParseExited([]byte{9, 0, 0, 0, 0}); err == nil {
		t.Fatal("bad reason must fail")
	}
	reason, code, err := ParseExited(ExitedPayload(ReasonShellExit, -1))
	if err != nil || reason != ReasonShellExit || code != -1 {
		t.Fatalf("reason=%d code=%d err=%v", reason, code, err)
	}
	if _, _, err := ParseError(nil); err == nil {
		t.Fatal("empty error must fail")
	}
	_, msg, err := ParseError(ErrorPayload(CodeTimeout, string(bytes.Repeat([]byte{'x'}, 400))))
	if err != nil || len(msg) != MaxMessage {
		t.Fatalf("message length %d err=%v", len(msg), err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestShortWrite(t *testing.T) {
	err := WriteFrame(shortWriter{}, Frame{Type: TypeHello, Control: 1})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("got %v", err)
	}
}
