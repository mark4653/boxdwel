//go:build windows

package enter

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const (
	enableProcessedOutput = 0x0001
	enableVTProcessing    = 0x0004
	codePageUTF8          = 65001
)

type consoleState struct {
	inMode, outMode uint32
	inCP, outCP     uint32
}

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode  = kernel32.NewProc("SetConsoleMode")
	procGetConsoleCP    = kernel32.NewProc("GetConsoleCP")
	procGetConsoleOutCP = kernel32.NewProc("GetConsoleOutputCP")
	procSetConsoleCP    = kernel32.NewProc("SetConsoleCP")
	procSetConsoleOutCP = kernel32.NewProc("SetConsoleOutputCP")
)

// enableRawConsole requires an interactive console, switches to raw input with
// VT output and UTF-8, and returns the exact state needed for restoration.
func enableRawConsole() (*consoleState, error) {
	st := &consoleState{}
	hin := syscall.Handle(os.Stdin.Fd())
	hout := syscall.Handle(os.Stdout.Fd())
	if err := syscall.GetConsoleMode(hin, &st.inMode); err != nil {
		return nil, fmt.Errorf("interactive console required (stdin): %w", err)
	}
	if err := syscall.GetConsoleMode(hout, &st.outMode); err != nil {
		return nil, fmt.Errorf("interactive console required (stdout): %w", err)
	}
	inCP, _, _ := procGetConsoleCP.Call()
	outCP, _, _ := procGetConsoleOutCP.Call()
	if inCP == 0 || outCP == 0 {
		return nil, fmt.Errorf("console code page unavailable: in=%d out=%d", inCP, outCP)
	}
	st.inCP, st.outCP = uint32(inCP), uint32(outCP)
	rawIn := rawInputMode(st.inMode)
	if err := setConsoleMode(hin, rawIn); err != nil {
		return nil, fmt.Errorf("set input console mode: %w", err)
	}
	if err := setConsoleMode(hout, st.outMode|enableProcessedOutput|enableVTProcessing); err != nil {
		return nil, errors.Join(fmt.Errorf("set output console mode: %w", err), st.restore())
	}
	if err := setCodePage(procSetConsoleCP, codePageUTF8); err != nil {
		return nil, errors.Join(err, st.restore())
	}
	if err := setCodePage(procSetConsoleOutCP, codePageUTF8); err != nil {
		return nil, errors.Join(err, st.restore())
	}
	return st, nil
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

// restore puts back modes and code pages; all failures are reported to the caller.
func (s *consoleState) restore() error {
	var first error
	if err := setConsoleMode(syscall.Handle(os.Stdin.Fd()), s.inMode); err != nil {
		first = err
	}
	if err := setConsoleMode(syscall.Handle(os.Stdout.Fd()), s.outMode); err != nil && first == nil {
		first = err
	}
	if err := setCodePage(procSetConsoleCP, s.inCP); err != nil && first == nil {
		first = err
	}
	if err := setCodePage(procSetConsoleOutCP, s.outCP); err != nil && first == nil {
		first = err
	}
	return first
}
