//go:build windows

package winpipe

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"
)

const (
	enableProcessedInput  = 0x0001
	enableLineInput       = 0x0002
	enableEchoInput       = 0x0004
	enableWindowInput     = 0x0008
	enableMouseInput      = 0x0010
	enableQuickEdit       = 0x0040
	enableExtendedFlags   = 0x0080
	enableVTInput         = 0x0200
	enableProcessedOutput = 0x0001
	enableVTProcessing    = 0x0004
	codePageUTF8          = 65001
	minDimension          = 1
	maxDimension          = 500
)

type coord struct{ X, Y int16 }

type smallRect struct{ Left, Top, Right, Bottom int16 }

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

type consoleState struct {
	inMode, outMode uint32
	inCP, outCP     uint32
}

var (
	procSetConsoleMode   = kernel32.NewProc("SetConsoleMode")
	procGetConsoleCP     = kernel32.NewProc("GetConsoleCP")
	procGetConsoleOutCP  = kernel32.NewProc("GetConsoleOutputCP")
	procSetConsoleCP     = kernel32.NewProc("SetConsoleCP")
	procSetConsoleOutCP  = kernel32.NewProc("SetConsoleOutputCP")
	procGetScreenBufInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

// ValidSize reports whether a terminal dimension is accepted by the product
// protocol. There is no implicit fallback size.
func ValidSize(rows, cols uint16) error {
	if rows < minDimension || rows > maxDimension || cols < minDimension || cols > maxDimension {
		return fmt.Errorf("terminal size %dx%d outside %d..%d", rows, cols, minDimension, maxDimension)
	}
	return nil
}

// Console is an interactive Windows console with raw input, VT output and
// UTF-8 enabled. Restore must be called on every path.
type Console struct {
	in, out syscall.Handle
	state   consoleState
}

// EnableConsole requires an interactive console. It saves the exact modes and
// code pages, then switches to raw input with VT and UTF-8 output.
func EnableConsole() (*Console, error) {
	c := &Console{in: syscall.Handle(os.Stdin.Fd()), out: syscall.Handle(os.Stdout.Fd())}
	if err := syscall.GetConsoleMode(c.in, &c.state.inMode); err != nil {
		return nil, fmt.Errorf("interactive console required (stdin): %w", err)
	}
	if err := syscall.GetConsoleMode(c.out, &c.state.outMode); err != nil {
		return nil, fmt.Errorf("interactive console required (stdout): %w", err)
	}
	inCP, _, _ := procGetConsoleCP.Call()
	outCP, _, _ := procGetConsoleOutCP.Call()
	if inCP == 0 || outCP == 0 {
		return nil, fmt.Errorf("console code page unavailable: in=%d out=%d", inCP, outCP)
	}
	c.state.inCP, c.state.outCP = uint32(inCP), uint32(outCP)
	if err := setConsoleMode(c.in, rawInputMode(c.state.inMode)); err != nil {
		return nil, fmt.Errorf("set input console mode: %w", err)
	}
	if err := setConsoleMode(c.out, c.state.outMode|enableProcessedOutput|enableVTProcessing); err != nil {
		return nil, errors.Join(fmt.Errorf("set output console mode: %w", err), c.Restore())
	}
	if err := setCodePage(procSetConsoleCP, codePageUTF8); err != nil {
		return nil, errors.Join(err, c.Restore())
	}
	if err := setCodePage(procSetConsoleOutCP, codePageUTF8); err != nil {
		return nil, errors.Join(err, c.Restore())
	}
	return c, nil
}

// Size returns the visible console window rows and columns. A non-interactive
// stdout or an out-of-range window is an error.
func (c *Console) Size() (uint16, uint16, error) {
	var info consoleScreenBufferInfo
	if r, _, err := procGetScreenBufInfo.Call(uintptr(c.out), uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0, 0, fmt.Errorf("GetConsoleScreenBufferInfo: %w", err)
	}
	rows := info.Window.Bottom - info.Window.Top + 1
	cols := info.Window.Right - info.Window.Left + 1
	if rows < minDimension || cols < minDimension {
		return 0, 0, fmt.Errorf("console window %dx%d has no visible cells", rows, cols)
	}
	if err := ValidSize(uint16(rows), uint16(cols)); err != nil {
		return 0, 0, err
	}
	return uint16(rows), uint16(cols), nil
}

// Input and Output are the raw console streams; the caller writes guest bytes
// unchanged (VT/UTF-8).
func (c *Console) Input() io.Reader  { return os.Stdin }
func (c *Console) Output() io.Writer { return os.Stdout }

// Restore puts back the saved modes and code pages. The first failure is
// returned and restoration continues for the remaining settings.
func (c *Console) Restore() error {
	var first error
	if err := setConsoleMode(c.in, c.state.inMode); err != nil {
		first = err
	}
	if err := setConsoleMode(c.out, c.state.outMode); err != nil && first == nil {
		first = err
	}
	if err := setCodePage(procSetConsoleCP, c.state.inCP); err != nil && first == nil {
		first = err
	}
	if err := setCodePage(procSetConsoleOutCP, c.state.outCP); err != nil && first == nil {
		first = err
	}
	return first
}

func setConsoleMode(h syscall.Handle, mode uint32) error {
	if r, _, err := procSetConsoleMode.Call(uintptr(h), uintptr(mode)); r == 0 {
		return err
	}
	return nil
}

func setCodePage(proc *syscall.LazyProc, cp uint32) error {
	if r, _, err := proc.Call(uintptr(cp)); r == 0 {
		return fmt.Errorf("set console code page %d: %w", cp, err)
	}
	return nil
}

// rawInputMode computes raw interactive input: no line editing, echo, mouse or
// window events, with VT input and extended flags. Kept identical to the M2
// console behavior.
func rawInputMode(mode uint32) uint32 {
	return (mode &^ uint32(enableProcessedInput|enableLineInput|enableEchoInput|enableQuickEdit|
		enableMouseInput|enableWindowInput)) | enableExtendedFlags | enableVTInput
}
