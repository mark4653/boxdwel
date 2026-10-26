// Package agentproto is the product wire format shared by the Windows relay
// and the Linux guest agent. The M3 measurement protocol is separate.
package agentproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Magic0     = 0x42 // 'B'
	Magic1     = 0x77 // 'w'
	Version    = 1
	HeaderSize = 10

	MaxPayload     = 4096
	MaxOutputChunk = 1024
	MaxMessage     = 255
	MaxSessions    = 2
	MaxDimension   = 500

	nonceSize  = 8
	readySize  = 6
	sizeBytes  = 4
	exitedSize = 5
)

// Type is a product message type.
type Type uint8

const (
	TypeHello  Type = 1
	TypeReady  Type = 2
	TypePing   Type = 3
	TypePong   Type = 4
	TypeOpen   Type = 5
	TypeOpened Type = 6
	TypeInput  Type = 7
	TypeOutput Type = 8
	TypeResize Type = 9
	TypeClose  Type = 10
	TypeExited Type = 11
	TypeError  Type = 12
)

// Exited reasons.
const (
	ReasonShellExit uint8 = 0
	ReasonClosed    uint8 = 1
)

// Error codes.
const (
	CodeProtocol     uint8 = 1
	CodeSessionLimit uint8 = 2
	CodeSpawn        uint8 = 3
	CodeTimeout      uint8 = 4
	CodeInput        uint8 = 5
)

var (
	ErrBadMagic        = errors.New("agentproto: not a product frame")
	ErrBadVersion      = errors.New("agentproto: unsupported version")
	ErrBadType         = errors.New("agentproto: unknown type")
	ErrBadShape        = errors.New("agentproto: bad header shape")
	ErrBadPayload      = errors.New("agentproto: bad payload")
	ErrPayloadTooLarge = errors.New("agentproto: payload exceeds limit")
	ErrTruncated       = errors.New("agentproto: truncated frame")
)

// Frame is one wire frame.
type Frame struct {
	Type    Type
	Control uint16
	Session uint16
	Payload []byte
}

// bounds returns the control scope and payload size range for a type. It is
// checked from the header, before any payload is read or allocated.
func bounds(t Type) (control bool, min, max int, err error) {
	switch t {
	case TypeHello:
		return true, 0, 0, nil
	case TypeReady:
		return true, readySize, readySize, nil
	case TypePing, TypePong:
		return true, nonceSize, nonceSize, nil
	case TypeOpen, TypeOpened, TypeResize:
		return false, sizeBytes, sizeBytes, nil
	case TypeInput:
		return false, 1, MaxPayload, nil
	case TypeOutput:
		return false, 1, MaxOutputChunk, nil
	case TypeClose:
		return false, 0, 0, nil
	case TypeExited:
		return false, exitedSize, exitedSize, nil
	case TypeError:
		return false, 1, 1 + MaxMessage, nil
	default:
		return false, 0, 0, fmt.Errorf("%w: %d", ErrBadType, t)
	}
}

func (f Frame) header() error {
	control, min, max, err := bounds(f.Type)
	if err != nil {
		return err
	}
	switch f.Type {
	case TypeError:
		if f.Control != 0 && f.Session != 0 {
			return fmt.Errorf("%w: error has both ids", ErrBadShape)
		}
	default:
		if control && f.Session != 0 {
			return fmt.Errorf("%w: %d needs control scope", ErrBadShape, f.Type)
		}
		if !control && (f.Control != 0 || f.Session == 0) {
			return fmt.Errorf("%w: %d needs a session", ErrBadShape, f.Type)
		}
	}
	if len(f.Payload) < min || len(f.Payload) > max {
		return fmt.Errorf("%w: %d with %d bytes", ErrBadPayload, f.Type, len(f.Payload))
	}
	if f.Type == TypeExited {
		if f.Payload[0] != ReasonShellExit && f.Payload[0] != ReasonClosed {
			return fmt.Errorf("%w: exited reason %d", ErrBadPayload, f.Payload[0])
		}
	}
	return nil
}

// ReadFrame reads one frame and validates the header before allocating or
// waiting for a payload. A clean EOF at a frame boundary returns io.EOF;
// partial headers or payloads return ErrTruncated.
func ReadFrame(r io.Reader) (Frame, error) {
	var magic [2]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		if err == io.EOF {
			return Frame{}, io.EOF
		}
		return Frame{}, fmt.Errorf("%w: magic: %w", ErrTruncated, err)
	}
	if magic[0] != Magic0 || magic[1] != Magic1 {
		return Frame{}, fmt.Errorf("%w: magic %#x %#x", ErrBadMagic, magic[0], magic[1])
	}
	var fixed [2]byte // version, type
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return Frame{}, fmt.Errorf("%w: header: %w", ErrTruncated, err)
	}
	if fixed[0] != Version {
		return Frame{}, fmt.Errorf("%w: %#x", ErrBadVersion, fixed[0])
	}
	f := Frame{Type: Type(fixed[1])}
	if _, _, _, err := bounds(f.Type); err != nil {
		return Frame{}, err
	}
	var ids [6]byte // control, session, length
	if _, err := io.ReadFull(r, ids[:]); err != nil {
		return Frame{}, fmt.Errorf("%w: header: %w", ErrTruncated, err)
	}
	f.Control = binary.BigEndian.Uint16(ids[0:2])
	f.Session = binary.BigEndian.Uint16(ids[2:4])
	n := int(binary.BigEndian.Uint16(ids[4:6]))

	control, min, max, _ := bounds(f.Type)
	switch f.Type {
	case TypeError:
		if f.Control != 0 && f.Session != 0 {
			return Frame{}, fmt.Errorf("%w: error has both ids", ErrBadShape)
		}
	default:
		if control && f.Session != 0 {
			return Frame{}, fmt.Errorf("%w: %d needs control scope", ErrBadShape, f.Type)
		}
		if !control && (f.Control != 0 || f.Session == 0) {
			return Frame{}, fmt.Errorf("%w: %d needs a session", ErrBadShape, f.Type)
		}
	}
	if n < min || n > max {
		return Frame{}, fmt.Errorf("%w: %d with %d bytes", ErrBadPayload, f.Type, n)
	}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return Frame{}, fmt.Errorf("%w: payload %d/%d: %w", ErrTruncated, n, n, err)
		}
	}
	if err := f.header(); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// WriteFrame writes one validated frame in a single Write call and reports a
// short write.
func WriteFrame(w io.Writer, f Frame) error {
	if err := f.header(); err != nil {
		return err
	}
	buf := make([]byte, HeaderSize+len(f.Payload))
	buf[0], buf[1], buf[2], buf[3] = Magic0, Magic1, Version, byte(f.Type)
	binary.BigEndian.PutUint16(buf[4:6], f.Control)
	binary.BigEndian.PutUint16(buf[6:8], f.Session)
	binary.BigEndian.PutUint16(buf[8:10], uint16(len(f.Payload)))
	copy(buf[HeaderSize:], f.Payload)
	n, err := w.Write(buf)
	if err != nil {
		return fmt.Errorf("agentproto: write: %w", err)
	}
	if n != len(buf) {
		return fmt.Errorf("agentproto: write: %w", io.ErrShortWrite)
	}
	return nil
}

// FromHost rejects frames the guest may not receive.
func (f Frame) FromHost() error {
	switch f.Type {
	case TypeHello, TypePing, TypeOpen, TypeInput, TypeResize, TypeClose:
		return nil
	default:
		return fmt.Errorf("%w: %d from host", ErrBadType, f.Type)
	}
}

// FromGuest rejects frames the host may not receive.
func (f Frame) FromGuest() error {
	switch f.Type {
	case TypeReady, TypePong, TypeOpened, TypeOutput, TypeExited, TypeError:
		return nil
	default:
		return fmt.Errorf("%w: %d from guest", ErrBadType, f.Type)
	}
}

// Size is a terminal dimension payload.
func Size(rows, cols uint16) ([]byte, error) {
	if rows < 1 || rows > MaxDimension || cols < 1 || cols > MaxDimension {
		return nil, fmt.Errorf("%w: size %dx%d", ErrBadPayload, rows, cols)
	}
	p := make([]byte, sizeBytes)
	binary.BigEndian.PutUint16(p[0:2], rows)
	binary.BigEndian.PutUint16(p[2:4], cols)
	return p, nil
}

// ParseSize decodes a size payload.
func ParseSize(p []byte) (rows, cols uint16, err error) {
	if len(p) != sizeBytes {
		return 0, 0, fmt.Errorf("%w: size needs %d bytes", ErrBadPayload, sizeBytes)
	}
	rows, cols = binary.BigEndian.Uint16(p[0:2]), binary.BigEndian.Uint16(p[2:4])
	if rows < 1 || rows > MaxDimension || cols < 1 || cols > MaxDimension {
		return 0, 0, fmt.Errorf("%w: size %dx%d", ErrBadPayload, rows, cols)
	}
	return rows, cols, nil
}

// ReadyPayload reports the guest limits.
func ReadyPayload() []byte {
	p := make([]byte, readySize)
	p[0], p[1] = byte(MaxSessions), 0
	binary.BigEndian.PutUint16(p[2:4], MaxPayload)
	binary.BigEndian.PutUint16(p[4:6], MaxOutputChunk)
	return p
}

// ParseReady decodes and validates the guest limits.
func ParseReady(p []byte) (sessions, payload, output int, err error) {
	if len(p) != readySize {
		return 0, 0, 0, fmt.Errorf("%w: ready needs %d bytes", ErrBadPayload, readySize)
	}
	sessions = int(p[0])
	payload = int(binary.BigEndian.Uint16(p[2:4]))
	output = int(binary.BigEndian.Uint16(p[4:6]))
	if p[1] != 0 {
		return 0, 0, 0, fmt.Errorf("%w: ready reserved %d", ErrBadPayload, p[1])
	}
	if sessions < 1 || sessions > MaxSessions {
		return 0, 0, 0, fmt.Errorf("%w: ready sessions %d", ErrBadPayload, sessions)
	}
	if payload < 1 || payload > MaxPayload {
		return 0, 0, 0, fmt.Errorf("%w: ready payload %d", ErrBadPayload, payload)
	}
	if output < 1 || output > MaxOutputChunk {
		return 0, 0, 0, fmt.Errorf("%w: ready output %d", ErrBadPayload, output)
	}
	return sessions, payload, output, nil
}

// NoncePayload encodes a ping nonce.
func NoncePayload(nonce uint64) []byte {
	p := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(p, nonce)
	return p
}

// ParseNonce decodes a ping nonce.
func ParseNonce(p []byte) (uint64, error) {
	if len(p) != nonceSize {
		return 0, fmt.Errorf("%w: nonce needs %d bytes", ErrBadPayload, nonceSize)
	}
	return binary.BigEndian.Uint64(p), nil
}

// ExitedPayload encodes a session end.
func ExitedPayload(reason uint8, code int32) []byte {
	p := make([]byte, exitedSize)
	p[0] = reason
	binary.BigEndian.PutUint32(p[1:], uint32(code))
	return p
}

// ParseExited decodes a session end.
func ParseExited(p []byte) (reason uint8, code int32, err error) {
	if len(p) != exitedSize {
		return 0, 0, fmt.Errorf("%w: exited needs %d bytes", ErrBadPayload, exitedSize)
	}
	if p[0] != ReasonShellExit && p[0] != ReasonClosed {
		return 0, 0, fmt.Errorf("%w: reason %d", ErrBadPayload, p[0])
	}
	return p[0], int32(binary.BigEndian.Uint32(p[1:])), nil
}

// ErrorPayload encodes an error code and message.
func ErrorPayload(code uint8, message string) []byte {
	if len(message) > MaxMessage {
		message = message[:MaxMessage]
	}
	p := make([]byte, 1+len(message))
	p[0] = code
	copy(p[1:], message)
	return p
}

// ParseError decodes an error payload.
func ParseError(p []byte) (code uint8, message string, err error) {
	if len(p) == 0 || len(p) > 1+MaxMessage {
		return 0, "", fmt.Errorf("%w: error needs 1..%d bytes", ErrBadPayload, 1+MaxMessage)
	}
	return p[0], string(p[1:]), nil
}
