//go:build linux

package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

const (
	// Not exported by syscall on linux/amd64.
	cflagCRTSCTS = 0x80000000
	cflagCBAUD   = 0x100f
)

type winsize struct{ Row, Col, Xpixel, Ypixel uint16 }

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// pty is one shell on a pseudo-terminal. The master is non-blocking and
// registered with the Go poller, so Close releases pending reads and writes.
type pty struct {
	master   *os.File
	cmd      *exec.Cmd
	closeOne sync.Once
}

func openFd(path string) (int, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	return fd, nil
}

func newPollable(fd int, name string) (*os.File, error) {
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("%s: bad descriptor", name)
	}
	return f, nil
}

// startShell opens a PTY and runs /bin/sh -i with a fixed environment and
// working directory.
func startShell(rows, cols uint16) (*pty, error) {
	fd, err := openFd("/dev/ptmx")
	if err != nil {
		return nil, err
	}
	var unlock int32
	if err := ioctl(uintptr(fd), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("unlock ptmx: %w", err)
	}
	var n uint32
	if err := ioctl(uintptr(fd), syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("pty number: %w", err)
	}
	ws := winsize{Row: rows, Col: cols}
	if err := ioctl(uintptr(fd), syscall.TIOCSWINSZ, unsafe.Pointer(&ws)); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("initial size: %w", err)
	}
	master, err := newPollable(fd, "/dev/ptmx")
	if err != nil {
		return nil, err
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", n)
	slaveFD, err := syscall.Open(slavePath, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, err
	}
	slave := os.NewFile(uintptr(slaveFD), slavePath)
	if slave == nil {
		syscall.Close(slaveFD)
		master.Close()
		return nil, errors.New("pty slave: bad descriptor")
	}

	cmd := exec.Command("/bin/sh", "-i")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Dir = "/"
	cmd.Env = []string{
		"TERM=xterm-256color",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + guestHome(),
		"SHELL=/bin/sh",
		"PWD=/",
	}
	// Setsid plus Setctty gives the shell a controlling terminal; Ctty 0 is
	// the slave because it is the child's stdin/stdout/stderr.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		return nil, err
	}
	slave.Close()
	return &pty{master: master, cmd: cmd}, nil
}

func guestHome() string {
	if os.Geteuid() == 0 {
		return "/root"
	}
	return "/tmp"
}

// write writes one input chunk and reports short writes.
func (p *pty) write(chunk []byte) error {
	n, err := p.master.Write(chunk)
	if err != nil {
		return err
	}
	if n != len(chunk) {
		return io.ErrShortWrite
	}
	return nil
}

// resize applies terminal dimensions through SyscallConn so the poller state
// is not disturbed.
func (p *pty) resize(rows, cols uint16) error {
	ws := winsize{Row: rows, Col: cols}
	rc, err := p.master.SyscallConn()
	if err != nil {
		return err
	}
	var ioErr error
	if err := rc.Control(func(fd uintptr) {
		ioErr = ioctl(fd, syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
	}); err != nil {
		return err
	}
	return ioErr
}

func (p *pty) foreground() int {
	rc, err := p.master.SyscallConn()
	if err != nil {
		return -1
	}
	var pgrp int32
	var ioErr error
	_ = rc.Control(func(fd uintptr) {
		ioErr = ioctl(fd, syscall.TIOCGPGRP, unsafe.Pointer(&pgrp))
	})
	if ioErr != nil {
		return -1
	}
	return int(pgrp)
}

// kill signals the foreground job (which may ignore HUP) and then the shell.
// The shell signal goes through os.Process, which returns ErrProcessDone
// after Wait, so a late Close cannot signal a reused shell pid.
// shortcut: the foreground group is read and signalled without pidfd, so a
// reused group id in that gap could be hit; move to pidfd/cgroup isolation
// when the session must be a security sandbox. No other processes are searched.
func (p *pty) kill() {
	if p.cmd.Process == nil {
		return
	}
	fg := p.foreground()
	_ = p.cmd.Process.Kill()
	if fg > 0 && fg != p.cmd.Process.Pid {
		_ = syscall.Kill(-fg, syscall.SIGKILL)
	}
}

func (p *pty) closeMaster() {
	p.closeOne.Do(func() { _ = p.master.Close() })
}

// wait reaps the shell once and returns its exit code.
func (p *pty) wait() (int32, error) {
	err := p.cmd.Wait()
	if p.cmd.ProcessState == nil {
		return -1, err
	}
	return int32(p.cmd.ProcessState.ExitCode()), nil
}

// OpenSerial opens path as a raw 115200 8N1 device with no flow control. The
// descriptor is pollable, so Close releases a pending Read.
func OpenSerial(path string) (*os.File, error) {
	fd, err := openFd(path)
	if err != nil {
		return nil, err
	}
	if err := setRaw(uintptr(fd)); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return newPollable(fd, path)
}

func setRaw(fd uintptr) error {
	var t syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t)); err != nil {
		return fmt.Errorf("tcgetattr: %w", err)
	}
	t.Iflag = 0
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag &^= syscall.CSTOPB | syscall.PARENB | syscall.PARODD | syscall.CSIZE | cflagCRTSCTS | cflagCBAUD
	t.Cflag |= syscall.CS8 | syscall.CREAD | syscall.CLOCAL | syscall.B115200
	t.Ispeed, t.Ospeed = syscall.B115200, syscall.B115200
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	return ioctl(fd, syscall.TCSETS, unsafe.Pointer(&t))
}
