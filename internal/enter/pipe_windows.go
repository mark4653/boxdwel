//go:build windows

package enter

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	pipeAccessDuplex   = 0x00000003
	pipeFirstInstance  = 0x00080000
	pipeRejectRemote   = 0x00000008
	pipeOverlapped     = 0x40000000
	errorPipeConnected = syscall.Errno(535)
	errorIoPending     = syscall.Errno(997)
	errorHandleEOF     = syscall.Errno(38)
	errorBrokenPipe    = syscall.Errno(109)
	waitObject0        = 0
	waitTimeout        = 258
	waitFailed         = 0xFFFFFFFF
	waitInfinite       = 0xFFFFFFFF
)

type securityAttributes struct {
	Length             uint32
	SecurityDescriptor uintptr
	InheritHandle      int32
}

type overlapped struct {
	Internal     uintptr
	InternalHigh uintptr
	Offset       uint32
	OffsetHigh   uint32
	Event        syscall.Handle
}

var (
	advapi32             = syscall.NewLazyDLL("advapi32.dll")
	procOpenProcessToken = advapi32.NewProc("OpenProcessToken")
	procGetTokenInfo     = advapi32.NewProc("GetTokenInformation")
	procSidToString      = advapi32.NewProc("ConvertSidToStringSidW")
	procSDDLToSD         = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetCurrentProc   = kernel32.NewProc("GetCurrentProcess")
	procCreateNamedPipe  = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe = kernel32.NewProc("ConnectNamedPipe")
	procCreateEvent      = kernel32.NewProc("CreateEventW")
	procCloseHandle      = kernel32.NewProc("CloseHandle")
	procWaitForSingle    = kernel32.NewProc("WaitForSingleObject")
	procCancelIoEx       = kernel32.NewProc("CancelIoEx")
	procGetOverlapped    = kernel32.NewProc("GetOverlappedResult")
	procReadFile         = kernel32.NewProc("ReadFile")
	procWriteFile        = kernel32.NewProc("WriteFile")
)

func currentUserSID() (string, error) {
	const tokenQuery = 0x0008
	const tokenUser = 1
	var tok syscall.Token
	process, _, _ := procGetCurrentProc.Call()
	if r, _, err := procOpenProcessToken.Call(process, tokenQuery, uintptr(unsafe.Pointer(&tok))); r == 0 {
		return "", fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer tok.Close()
	var size uint32
	procGetTokenInfo.Call(uintptr(tok), tokenUser, 0, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return "", fmt.Errorf("GetTokenInformation: empty token user")
	}
	buf := make([]byte, size)
	if r, _, err := procGetTokenInfo.Call(uintptr(tok), tokenUser, uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&size))); r == 0 {
		return "", fmt.Errorf("GetTokenInformation: %w", err)
	}
	sid := *(**syscall.SID)(unsafe.Pointer(&buf[0]))
	var str *uint16
	if r, _, err := procSidToString.Call(uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&str))); r == 0 {
		return "", fmt.Errorf("ConvertSidToStringSid: %w", err)
	}
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(str)))
	n := 0
	for *(*uint16)(unsafe.Add(unsafe.Pointer(str), uintptr(n)*unsafe.Sizeof(uint16(0)))) != 0 {
		n++
	}
	return syscall.UTF16ToString(unsafe.Slice(str, n)), nil
}

// consolePipe is the single-session lock plus an overlapped serial byte stream.
// Creation does not wait for a peer; Accept performs the bounded overlapped connect.
type consolePipe struct {
	handle   syscall.Handle
	mu       sync.Mutex
	closed   bool
	inflight sync.WaitGroup
	closeOne sync.Once
	closeErr error
}

func createConsolePipe(name string) (*consolePipe, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	sddl := fmt.Sprintf(`D:P(A;;GA;;;SY)(A;;GA;;;%s)`, sid)
	sddlPtr, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return nil, err
	}
	var sd uintptr
	if r, _, callErr := procSDDLToSD.Call(uintptr(unsafe.Pointer(sddlPtr)), 1, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
		return nil, fmt.Errorf("security descriptor: %w", callErr)
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	sa := securityAttributes{Length: uint32(unsafe.Sizeof(securityAttributes{})), SecurityDescriptor: sd}
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, _, callErr := procCreateNamedPipe.Call(uintptr(unsafe.Pointer(namePtr)),
		uintptr(pipeAccessDuplex|pipeFirstInstance|pipeOverlapped), uintptr(pipeRejectRemote), 1,
		uintptr(64*1024), uintptr(64*1024), 0, uintptr(unsafe.Pointer(&sa)))
	if syscall.Handle(h) == syscall.InvalidHandle {
		return nil, fmt.Errorf("named pipe %s unavailable (another session may be active): %w", name, callErr)
	}
	return &consolePipe{handle: syscall.Handle(h)}, nil
}

// Accept waits for the VirtualBox UART client. Timeout and wait failures cancel
// the pending operation and wait for its completion before the event is freed.
func (p *consolePipe) Accept(timeout time.Duration) error {
	ev, _, evErr := procCreateEvent.Call(0, 0, 0, 0)
	if ev == 0 {
		return fmt.Errorf("CreateEvent: %w", evErr)
	}
	event := syscall.Handle(ev)
	ov := overlapped{Event: event}
	connected := false
	r, _, callErr := procConnectNamedPipe.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&ov)))
	if r != 0 {
		connected = true
	} else if callErr == errorPipeConnected {
		connected = true
	} else if callErr != errorIoPending {
		procCloseHandle.Call(ev)
		return fmt.Errorf("ConnectNamedPipe: %w", callErr)
	}
	if !connected {
		wait, _, _ := procWaitForSingle.Call(ev, uintptr(timeout.Milliseconds()))
		if wait == waitTimeout || wait == waitFailed {
			procCancelIoEx.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&ov)))
			var n uint32
			procGetOverlapped.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&n)), 1)
			procCloseHandle.Call(ev)
			if wait == waitTimeout {
				return fmt.Errorf("named pipe accept timed out after %s", timeout)
			}
			return fmt.Errorf("named pipe wait failed: %#x", wait)
		}
		var n uint32
		if r, _, err := procGetOverlapped.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&n)), 0); r == 0 {
			procCloseHandle.Call(ev)
			return fmt.Errorf("GetOverlappedResult: %w", err)
		}
	}
	procCloseHandle.Call(ev)
	return nil
}

func (p *consolePipe) doIO(write bool, b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, errors.New("pipe closed")
	}
	p.inflight.Add(1)
	ev, _, evErr := procCreateEvent.Call(0, 0, 0, 0)
	if ev == 0 {
		p.inflight.Done()
		p.mu.Unlock()
		return 0, fmt.Errorf("CreateEvent: %w", evErr)
	}
	event := syscall.Handle(ev)
	ov := overlapped{Event: event}
	var n uint32
	var r uintptr
	var callErr error
	if write {
		r, _, callErr = procWriteFile.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&b[0])),
			uintptr(len(b)), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&ov)))
	} else {
		r, _, callErr = procReadFile.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&b[0])),
			uintptr(len(b)), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&ov)))
	}
	p.mu.Unlock()
	defer p.inflight.Done()
	defer procCloseHandle.Call(ev)
	defer runtime.KeepAlive(b)
	if r != 0 {
		return int(n), nil
	}
	switch callErr {
	case errorIoPending:
		timeoutMs := uintptr(waitInfinite)
		if write {
			timeoutMs = 30000
		}
		wait, _, _ := procWaitForSingle.Call(ev, timeoutMs)
		if wait != waitObject0 {
			procCancelIoEx.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&ov)))
			var done uint32
			procGetOverlapped.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&done)), 1)
			if wait == waitTimeout {
				return 0, fmt.Errorf("pipe write timed out")
			}
			return 0, fmt.Errorf("pipe wait failed: %#x", wait)
		}
		if r, _, err := procGetOverlapped.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&n)), 0); r == 0 {
			return 0, fmt.Errorf("GetOverlappedResult: %w", err)
		}
		return int(n), nil
	case errorHandleEOF, errorBrokenPipe:
		return 0, io.EOF
	default:
		return 0, callErr
	}
}

func (p *consolePipe) Read(b []byte) (int, error)  { return p.doIO(false, b) }
func (p *consolePipe) Write(b []byte) (int, error) { return p.doIO(true, b) }

// Close cancels pending I/O, waits for those operations to finish, then closes
// the handle so no overlapped memory outlives it.
func (p *consolePipe) Close() error {
	p.closeOne.Do(func() {
		p.mu.Lock()
		p.closed = true
		procCancelIoEx.Call(uintptr(p.handle), 0)
		p.mu.Unlock()
		p.inflight.Wait()
		if r, _, err := procCloseHandle.Call(uintptr(p.handle)); r == 0 {
			p.closeErr = err
		}
	})
	return p.closeErr
}
