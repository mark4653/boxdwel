package m3probe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func rawHeader(version, typ uint8, id uint16, length int) []byte {
	b := make([]byte, HeaderSize)
	b[0] = version
	b[1] = typ
	binary.BigEndian.PutUint16(b[2:4], id)
	binary.BigEndian.PutUint16(b[4:6], uint16(length))
	return b
}

func TestFrameRoundTrip(t *testing.T) {
	frames := []Frame{
		{Version: Version, Type: TypeReady, ID: ControlID, Payload: []byte("nonce-1")},
		{Version: Version, Type: TypeEcho, ID: ControlID, Payload: bytes.Repeat([]byte{7}, MaxPayload)},
		{Version: Version, Type: TypeStream, ID: LastStreamID, Payload: nil},
	}
	for _, want := range frames {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, want); err != nil {
			t.Fatalf("write %v: %v", want.Type, err)
		}
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("read %v: %v", want.Type, err)
		}
		if got.Version != want.Version || got.Type != want.Type || got.ID != want.ID || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("round trip = %+v, want %+v", got, want)
		}
	}
}

func TestReadFrameRejectsBadFrames(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		{"bad version", rawHeader(0x02, 1, 0, 0), ErrBadVersion},
		{"bad type", rawHeader(Version, 99, 0, 0), ErrBadType},
		{"oversize payload", rawHeader(Version, byte(TypeEcho), 0, MaxPayload+1), ErrPayloadTooLarge},
		{"truncated header", []byte{Version, 1, 0}, ErrTruncated},
		{"truncated payload", append(rawHeader(Version, byte(TypeEcho), 0, 10), 1, 2, 3), ErrTruncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadFrame(bytes.NewReader(tc.raw)); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

type shortWriter struct{ n int }

func (w shortWriter) Write(p []byte) (int, error) { return w.n, nil }

func TestWriteFrameRejectsAndDetectsShortWrite(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{Version: 0x02, Type: TypeEcho}); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("bad version err = %v", err)
	}
	if err := WriteFrame(&buf, Frame{Version: Version, Type: 99}); !errors.Is(err, ErrBadType) {
		t.Fatalf("bad type err = %v", err)
	}
	big := Frame{Version: Version, Type: TypeEcho, Payload: make([]byte, MaxPayload+1)}
	if err := WriteFrame(&buf, big); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("oversize err = %v", err)
	}
	for _, n := range []int{0, HeaderSize - 1} {
		if err := WriteFrame(shortWriter{n}, Frame{Version: Version, Type: TypeEcho}); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short write %d err = %v", n, err)
		}
	}
}

func TestPingCodec(t *testing.T) {
	for _, seq := range []uint32{1, 2, 4000000000} {
		got, ok := decodePing(encodePing(seq))
		if !ok || got != seq {
			t.Fatalf("ping %d = %d, %v", seq, got, ok)
		}
	}
	if _, ok := decodePing(nil); ok {
		t.Fatal("empty ping accepted")
	}
	if _, ok := decodePing(make([]byte, 31)); ok {
		t.Fatal("short ping accepted")
	}
	bad := encodePing(1)
	bad[8] ^= 1
	if _, ok := decodePing(bad); ok {
		t.Fatal("ping with bad filler accepted")
	}
	if _, ok := decodePing(encodePing(0)); ok {
		t.Fatal("zero sequence accepted")
	}
}

func TestStreamPayloadCodecs(t *testing.T) {
	n, err := decodeStartBytes(encodeStartBytes(1234))
	if err != nil || n != 1234 {
		t.Fatalf("start codec = %d, %v", n, err)
	}
	for _, p := range [][]byte{nil, {1, 2, 3}, encodeStartBytes(0), encodeStartBytes(MaxStreamBytes + 1)} {
		if _, err := decodeStartBytes(p); !errors.Is(err, ErrBadPayload) {
			t.Fatalf("start %v err = %v", p, err)
		}
	}
	var sum [32]byte
	for i := range sum {
		sum[i] = byte(i)
	}
	total, gotSum, err := decodeStreamEnd(encodeStreamEnd(4321, sum))
	if err != nil || total != 4321 || gotSum != sum {
		t.Fatalf("end codec = %d %v %v", total, gotSum, err)
	}
	badEnd := make([]byte, 40)
	binary.BigEndian.PutUint64(badEnd[:8], MaxStreamBytes+1)
	for _, p := range [][]byte{nil, make([]byte, 39), make([]byte, 41), badEnd} {
		if _, _, err := decodeStreamEnd(p); !errors.Is(err, ErrBadPayload) {
			t.Fatalf("end %d bytes err = %v", len(p), err)
		}
	}
}
