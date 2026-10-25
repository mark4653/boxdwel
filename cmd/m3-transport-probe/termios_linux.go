//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, req, uintptr(arg), 0, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	return ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t)) == nil
}

// makeRaw sets 115200 8N1 raw with no echo and no flow control, and returns
// the previous settings so the caller can restore them.
func makeRaw(fd uintptr) (*syscall.Termios, error) {
	var t syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	old := t
	t.Iflag = 0
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag = syscall.B115200 | syscall.CS8 | syscall.CREAD | syscall.CLOCAL
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, syscall.TCSETS, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return &old, nil
}

func restoreTermios(fd uintptr, t *syscall.Termios) error {
	return ioctl(fd, syscall.TCSETS, unsafe.Pointer(t))
}
