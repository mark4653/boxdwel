// Package m3probe is the M3 COM2 transport spike: an experimental framed wire
// for local measurement only. It is not the product agent protocol.
package m3probe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Version    = 0x01
	HeaderSize = 6
	// MaxPayload bounds one frame so a low-speed serial line can still carry
	// control replies between data chunks.
	MaxPayload     = 4096
	MaxNonceBytes  = 64
	MaxStreamBytes = 1 << 20
	StreamChunk    = 1024

	ControlID     = 0
	FirstStreamID = 1
	LastStreamID  = 2
)

// Type is an experimental frame type.
type Type uint8

const (
	TypeReady     Type = 1
	TypeEcho      Type = 2
	TypeEchoReply Type = 3
	TypeStart     Type = 4
	TypeStream    Type = 5
	TypeStreamEnd Type = 6
	TypeHello     Type = 7
	TypeFinish    Type = 8
	TypeAck       Type = 9
)

// Frame is one wire frame: 6-byte header plus payload.
type Frame struct {
	Version uint8
	Type    Type
	ID      uint16
	Payload []byte
}

var (
	ErrBadVersion      = errors.New("m3probe: unsupported version")
	ErrBadType         = errors.New("m3probe: unknown or unexpected frame type")
	ErrBadID           = errors.New("m3probe: bad stream id")
	ErrBadPayload      = errors.New("m3probe: bad payload")
	ErrBadState        = errors.New("m3probe: unexpected frame in this state")
	ErrPayloadTooLarge = errors.New("m3probe: payload exceeds limit")
	ErrTruncated       = errors.New("m3probe: truncated frame")
	ErrTimeout         = errors.New("m3probe: deadline exceeded")
)

func (t Type) valid() bool {
	switch t {
	case TypeReady, TypeEcho, TypeEchoReply, TypeStart, TypeStream, TypeStreamEnd,
		TypeHello, TypeFinish, TypeAck:
		return true
	}
	return false
}

// ReadFrame reads and validates one frame. Oversized or malformed frames fail
// closed before allocating payload memory.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, fmt.Errorf("%w: header: %w", ErrTruncated, err)
	}
	f := Frame{
		Version: hdr[0],
		Type:    Type(hdr[1]),
		ID:      binary.BigEndian.Uint16(hdr[2:4]),
	}
	n := int(binary.BigEndian.Uint16(hdr[4:6]))
	if f.Version != Version {
		return Frame{}, fmt.Errorf("%w: %#x", ErrBadVersion, f.Version)
	}
	if !f.Type.valid() {
		return Frame{}, fmt.Errorf("%w: %#x", ErrBadType, uint8(f.Type))
	}
	if n > MaxPayload {
		return Frame{}, fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, n, MaxPayload)
	}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return Frame{}, fmt.Errorf("%w: payload: %w", ErrTruncated, err)
		}
	}
	return f, nil
}

// WriteFrame writes one validated frame in a single Write call and reports a
// partial write as io.ErrShortWrite.
func WriteFrame(w io.Writer, f Frame) error {
	if f.Version != Version {
		return fmt.Errorf("%w: %#x", ErrBadVersion, f.Version)
	}
	if !f.Type.valid() {
		return fmt.Errorf("%w: %#x", ErrBadType, uint8(f.Type))
	}
	if len(f.Payload) > MaxPayload {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(f.Payload), MaxPayload)
	}
	buf := make([]byte, HeaderSize+len(f.Payload))
	buf[0] = f.Version
	buf[1] = byte(f.Type)
	binary.BigEndian.PutUint16(buf[2:4], f.ID)
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(f.Payload)))
	copy(buf[HeaderSize:], f.Payload)
	n, err := w.Write(buf)
	if err != nil {
		return fmt.Errorf("m3probe: write: %w", err)
	}
	if n != len(buf) {
		return fmt.Errorf("m3probe: write: %w", io.ErrShortWrite)
	}
	return nil
}

// streamByte is the fixed non-secret stream pattern; the per-id seed makes a
// frame swapped to the wrong stream id fail the receiver's byte/hash check.
func streamSeed(id uint16) byte { return byte(0x40 + id*0x35) }

func streamByte(offset int, seed byte) byte { return byte((offset + int(seed)) % 256) }

// PingPayloadSize and encodePing give each bulk-phase ping a sequence number
// so a late reply can never be mistaken for the current ping.
const PingPayloadSize = 32

func encodePing(seq uint32) []byte {
	p := make([]byte, PingPayloadSize)
	binary.BigEndian.PutUint32(p[:4], seq)
	for i := 4; i < len(p); i++ {
		p[i] = byte(0x80 + i)
	}
	return p
}

func decodePing(p []byte) (uint32, bool) {
	if len(p) != PingPayloadSize {
		return 0, false
	}
	want := encodePing(0)
	if !bytes.Equal(p[4:], want[4:]) {
		return 0, false
	}
	seq := binary.BigEndian.Uint32(p[:4])
	if seq == 0 {
		return 0, false
	}
	return seq, true
}

func encodeStartBytes(n int) []byte {
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, uint32(n))
	return p
}

func decodeStartBytes(p []byte) (int, error) {
	if len(p) != 4 {
		return 0, fmt.Errorf("%w: start needs 4 bytes, got %d", ErrBadPayload, len(p))
	}
	n := int(binary.BigEndian.Uint32(p))
	if n <= 0 || n > MaxStreamBytes {
		return 0, fmt.Errorf("%w: start bytes %d out of 1..%d", ErrBadPayload, n, MaxStreamBytes)
	}
	return n, nil
}

func encodeStreamEnd(total int, sum [32]byte) []byte {
	p := make([]byte, 40)
	binary.BigEndian.PutUint64(p[:8], uint64(total))
	copy(p[8:], sum[:])
	return p
}

func decodeStreamEnd(p []byte) (int, [32]byte, error) {
	var sum [32]byte
	if len(p) != 40 {
		return 0, sum, fmt.Errorf("%w: stream end needs 40 bytes, got %d", ErrBadPayload, len(p))
	}
	n := binary.BigEndian.Uint64(p[:8])
	if n > MaxStreamBytes {
		return 0, sum, fmt.Errorf("%w: stream end bytes %d > %d", ErrBadPayload, n, MaxStreamBytes)
	}
	copy(sum[:], p[8:])
	return int(n), sum, nil
}
