//go:build windows

// Package winpipe provides the secure Windows named-pipe transport for the
// host relay: a first-instance listening namespace, overlapped byte streams
// whose Close releases pending I/O, and a client that verifies the server
// process before use.
package winpipe

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	// ErrClosed is returned when the listener or stream is already closed.
	ErrClosed = errors.New("winpipe: closed")
	// ErrTimeout is returned when an accept or connect exceeds its bound.
	ErrTimeout = errors.New("winpipe: timeout")
)

const (
	pipeAccessDuplex  = 0x00000003
	pipeFirstInstance = 0x00080000
	pipeRejectRemote  = 0x00000008
	pipeOverlapped    = 0x40000000
	maxInstances      = 4 // two active clients + accepting + pending

	errorPipeConnected = syscall.Errno(535)
	errorPipeBusy      = syscall.Errno(231)
	errorIoPending     = syscall.Errno(997)
	errorHandleEOF     = syscall.Errno(38)
	errorBrokenPipe    = syscall.Errno(109)
	waitObject0        = 0
	waitTimeout        = 258
	waitFailed         = 0xFFFFFFFF
	waitInfinite       = 0xFFFFFFFF

	genericRead     = 0x80000000
	genericWrite    = 0x40000000
	openExisting    = 3
	fileFlagOverlap = 0x40000000
	processQueryLim = 0x1000
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
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	advapi32              = syscall.NewLazyDLL("advapi32.dll")
	procCreateNamedPipe   = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe  = kernel32.NewProc("ConnectNamedPipe")
	procCreateEvent       = kernel32.NewProc("CreateEventW")
	procWaitForSingle     = kernel32.NewProc("WaitForSingleObject")
	procCancelIoEx        = kernel32.NewProc("CancelIoEx")
	procGetOverlapped     = kernel32.NewProc("GetOverlappedResult")
	procReadFile          = kernel32.NewProc("ReadFile")
	procWriteFile         = kernel32.NewProc("WriteFile")
	procCloseHandle       = kernel32.NewProc("CloseHandle")
	procCreateFile        = kernel32.NewProc("CreateFileW")
	procWaitNamedPipe     = kernel32.NewProc("WaitNamedPipeW")
	procGetPipeServerPID  = kernel32.NewProc("GetNamedPipeServerProcessId")
	procQueryImage        = kernel32.NewProc("QueryFullProcessImageNameW")
	procGetCurrentProcess = kernel32.NewProc("GetCurrentProcess")
	procOpenProcessToken  = advapi32.NewProc("OpenProcessToken")
	procGetTokenInfo      = advapi32.NewProc("GetTokenInformation")
	procSidToString       = advapi32.NewProc("ConvertSidToStringSidW")
	procSDDLToSD          = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
)

func currentUserSID() (string, error) {
	const tokenQuery = 0x0008
	const tokenUser = 1
	var tok syscall.Token
	process, _, _ := procGetCurrentProcess.Call()
	if r, _, err := procOpenProcessToken.Call(process, tokenQuery, uintptr(unsafe.Pointer(&tok))); r == 0 {
		return "", fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer tok.Close()
	return tokenUserSID(tok)
}

func tokenUserSID(tok syscall.Token) (string, error) {
	const tokenUser = 1
	var size uint32
	procGetTokenInfo.Call(uintptr(tok), tokenUser, 0, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return "", errors.New("GetTokenInformation: empty token user")
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

// Stream is one overlapped named-pipe byte stream. Close cancels pending I/O
// and waits for the operations before releasing the handle.
type Stream struct {
	handle syscall.Handle

	mu       sync.Mutex
	closed   bool
	inflight sync.WaitGroup
	closeOne sync.Once
	closeErr error
}

func (s *Stream) doIO(write bool, b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	s.inflight.Add(1)
	ev, _, evErr := procCreateEvent.Call(0, 0, 0, 0)
	if ev == 0 {
		s.inflight.Done()
		s.mu.Unlock()
		return 0, fmt.Errorf("CreateEvent: %w", evErr)
	}
	event := syscall.Handle(ev)
	ov := overlapped{Event: event}
	var n uint32
	var r uintptr
	var callErr error
	if write {
		r, _, callErr = procWriteFile.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&b[0])),
			uintptr(len(b)), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&ov)))
	} else {
		r, _, callErr = procReadFile.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&b[0])),
			uintptr(len(b)), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&ov)))
	}
	s.mu.Unlock()
	defer s.inflight.Done()
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
			procCancelIoEx.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&ov)))
			var done uint32
			procGetOverlapped.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&done)), 1)
			if wait == waitTimeout {
				return 0, fmt.Errorf("%w: pipe write", ErrTimeout)
			}
			return 0, fmt.Errorf("pipe wait failed: %#x", wait)
		}
		if r, _, err := procGetOverlapped.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&n)), 0); r == 0 {
			return 0, fmt.Errorf("GetOverlappedResult: %w", err)
		}
		return int(n), nil
	case errorHandleEOF, errorBrokenPipe:
		return 0, io.EOF
	default:
		return 0, callErr
	}
}

func (s *Stream) Read(b []byte) (int, error)  { return s.doIO(false, b) }
func (s *Stream) Write(b []byte) (int, error) { return s.doIO(true, b) }

// Close cancels this stream's pending I/O, waits for it, then closes the
// handle. Only the handle owned by this stream is touched.
func (s *Stream) Close() error {
	s.closeOne.Do(func() {
		s.mu.Lock()
		s.closed = true
		procCancelIoEx.Call(uintptr(s.handle), 0)
		s.mu.Unlock()
		s.inflight.Wait()
		if r, _, err := procCloseHandle.Call(uintptr(s.handle)); r == 0 {
			s.closeErr = err
		}
	})
	return s.closeErr
}

// Server is one server instance of the pipe namespace.
type Server struct {
	*Stream
}

func newServer(name string, first bool) (*Server, error) {
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
	// PIPE_REJECT_REMOTE_CLIENTS belongs to dwPipeMode, not dwOpenMode;
	// CreateNamedPipe rejects an unsupported dwOpenMode flag.
	openMode := uintptr(pipeAccessDuplex | pipeOverlapped)
	if first {
		openMode |= pipeFirstInstance
	}
	pipeMode := uintptr(pipeRejectRemote)
	h, _, callErr := procCreateNamedPipe.Call(uintptr(unsafe.Pointer(namePtr)),
		openMode, pipeMode, uintptr(maxInstances),
		uintptr(64*1024), uintptr(64*1024), 0, uintptr(unsafe.Pointer(&sa)))
	if syscall.Handle(h) == syscall.InvalidHandle {
		return nil, fmt.Errorf("named pipe %s unavailable: %w", name, callErr)
	}
	return &Server{Stream: &Stream{handle: syscall.Handle(h)}}, nil
}

// Accept waits for one client with a bounded overlapped connect. On timeout
// or failure the instance is left for the caller to close.
func (s *Server) Accept(timeout time.Duration) error {
	ev, _, evErr := procCreateEvent.Call(0, 0, 0, 0)
	if ev == 0 {
		return fmt.Errorf("CreateEvent: %w", evErr)
	}
	event := syscall.Handle(ev)
	defer procCloseHandle.Call(ev)
	ov := overlapped{Event: event}

	// Issue the non-blocking connect under mu so Close's CancelIoEx always has
	// an operation to cancel; otherwise Close could wait for a late connect
	// until the accept timeout.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.inflight.Add(1)
	connected := false
	r, _, callErr := procConnectNamedPipe.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&ov)))
	if r != 0 {
		connected = true
	} else if callErr == errorPipeConnected {
		connected = true
	} else if callErr != errorIoPending {
		s.mu.Unlock()
		s.inflight.Done()
		return fmt.Errorf("ConnectNamedPipe: %w", callErr)
	}
	s.mu.Unlock()
	defer s.inflight.Done()
	if connected {
		return nil
	}
	wait, _, _ := procWaitForSingle.Call(ev, uintptr(timeout.Milliseconds()))
	switch wait {
	case waitObject0:
	case waitTimeout, waitFailed:
		procCancelIoEx.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&ov)))
		var done uint32
		procGetOverlapped.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&done)), 1)
		if wait == waitTimeout {
			return fmt.Errorf("%w: accept after %s", ErrTimeout, timeout)
		}
		return fmt.Errorf("named pipe wait failed: %#x", wait)
	default:
		procCancelIoEx.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&ov)))
		var done uint32
		procGetOverlapped.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&done)), 1)
		return fmt.Errorf("unexpected named pipe wait result: %#x", wait)
	}
	var n uint32
	if r, _, err := procGetOverlapped.Call(uintptr(s.handle), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&n)), 0); r == 0 {
		return fmt.Errorf("GetOverlappedResult: %w", err)
	}
	return nil
}

// Listener owns the pipe namespace. It registers the accepting instance and
// the accept wait group under the same lock Close uses, and creates the next
// pending instance before returning an accepted stream.
type Listener struct {
	name string

	acceptMu  sync.Mutex
	mu        sync.Mutex
	pending   *Server
	accepting *Server
	closed    bool
	accepts   sync.WaitGroup
	closeOnce sync.Once
}

// Listen creates the first-instance server. A second listener on the same name
// fails rather than sharing the namespace.
func Listen(name string) (*Listener, error) {
	if err := validPipeName(name); err != nil {
		return nil, err
	}
	first, err := newServer(name, true)
	if err != nil {
		return nil, err
	}
	return &Listener{name: name, pending: first}, nil
}

// Accept returns one connected server stream. The accepting instance and the
// wait-group Add are registered under the listener lock, so Close cannot miss
// the operation or return before it is joined. The next pending instance is
// created before the connect; if it cannot be created the listener fails
// closed instead of losing the namespace.
func (l *Listener) Accept(timeout time.Duration) (*Server, error) {
	l.acceptMu.Lock()
	defer l.acceptMu.Unlock()

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, ErrClosed
	}
	cur := l.pending
	if cur == nil {
		l.closed = true
		l.mu.Unlock()
		return nil, errors.New("winpipe: listener namespace lost")
	}
	l.pending = nil
	l.accepting = cur
	l.accepts.Add(1)
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.accepting == cur {
			l.accepting = nil
		}
		l.mu.Unlock()
		l.accepts.Done()
	}()

	next, err := newServer(l.name, false)
	if err != nil {
		_ = cur.Close()
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		return nil, fmt.Errorf("winpipe: cannot keep pipe namespace (fail closed): %w", err)
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = next.Close()
		_ = cur.Close()
		return nil, ErrClosed
	}
	l.pending = next
	l.mu.Unlock()

	if err := cur.Accept(timeout); err != nil {
		_ = cur.Close()
		return nil, err
	}
	return cur, nil
}

// Close stops the namespace: pending and accepting instances are canceled, the
// accept calls are joined, and every repeated Close waits for the same full
// teardown (sync.Once joins concurrent and later callers).
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		pending := l.pending
		l.pending = nil
		accepting := l.accepting
		l.mu.Unlock()
		if pending != nil {
			_ = pending.Close()
		}
		if accepting != nil {
			_ = accepting.Close()
		}
		l.accepts.Wait()
	})
	return nil
}

func validPipeName(name string) error {
	const prefix = `\\.\pipe\`
	if len(name) <= len(prefix) || !strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
		return fmt.Errorf("winpipe: %q is not a local named-pipe path", name)
	}
	rest := name[len(prefix):]
	if rest == "" || strings.ContainsAny(rest, `\/`) || strings.Contains(rest, "..") {
		return fmt.Errorf("winpipe: invalid local pipe name %q", name)
	}
	return nil
}

func validExpectedExe(path string) error {
	if path == "" {
		return errors.New("winpipe: expected server executable is required")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("winpipe: expected server executable must be absolute: %q", path)
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `//`) {
		return fmt.Errorf("winpipe: UNC/device path is not a trusted server executable: %q", path)
	}
	return nil
}

// OpenClient opens name with overlapped I/O and verifies the server identity
// before any protocol frame is exchanged. expectedExe is the caller-trusted
// executable path; a different server process or user is an error.
func OpenClient(name, expectedExe string, timeout time.Duration) (*Stream, error) {
	if err := validPipeName(name); err != nil {
		return nil, err
	}
	if err := validExpectedExe(expectedExe); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var h syscall.Handle
	for {
		r, _, callErr := procCreateFile.Call(uintptr(unsafe.Pointer(namePtr)),
			uintptr(genericRead|genericWrite), 0, 0, uintptr(openExisting), uintptr(fileFlagOverlap), 0)
		h = syscall.Handle(r)
		if h != syscall.InvalidHandle {
			break
		}
		if callErr != errorPipeBusy {
			return nil, fmt.Errorf("open %s: %w", name, callErr)
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, fmt.Errorf("%w: open %s", ErrTimeout, name)
		}
		wait, _, _ := procWaitNamedPipe.Call(uintptr(unsafe.Pointer(namePtr)), uintptr(left.Milliseconds()))
		if wait == 0 {
			return nil, fmt.Errorf("%w: open %s", ErrTimeout, name)
		}
	}
	if err := verifyServer(h, expectedExe); err != nil {
		procCloseHandle.Call(uintptr(h))
		return nil, err
	}
	return &Stream{handle: h}, nil
}

func verifyServer(h syscall.Handle, expectedExe string) error {
	var pid uint32
	if r, _, err := procGetPipeServerPID.Call(uintptr(h), uintptr(unsafe.Pointer(&pid))); r == 0 {
		return fmt.Errorf("GetNamedPipeServerProcessId: %w", err)
	}
	proc, err := syscall.OpenProcess(processQueryLim, false, pid)
	if err != nil {
		return fmt.Errorf("OpenProcess %d: %w", pid, err)
	}
	defer syscall.CloseHandle(proc)

	const maxPath = 32768
	buf := make([]uint16, maxPath)
	size := uint32(len(buf))
	if r, _, callErr := procQueryImage.Call(uintptr(proc), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return fmt.Errorf("QueryFullProcessImageName: %w", callErr)
	}
	serverExe := syscall.UTF16ToString(buf[:size])
	if !samePath(serverExe, expectedExe) {
		return fmt.Errorf("winpipe: pipe server executable %q does not match %q", serverExe, expectedExe)
	}

	var tok syscall.Token
	if r, _, callErr := procOpenProcessToken.Call(uintptr(proc), 0x0008, uintptr(unsafe.Pointer(&tok))); r == 0 {
		return fmt.Errorf("OpenProcessToken(server): %w", callErr)
	}
	defer tok.Close()
	serverSID, err := tokenUserSID(tok)
	if err != nil {
		return fmt.Errorf("server user: %w", err)
	}
	currentSID, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("current user: %w", err)
	}
	if !strings.EqualFold(serverSID, currentSID) {
		return errors.New("winpipe: pipe server user does not match the current user")
	}
	return nil
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return strings.EqualFold(a, b)
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

// SelfPath is the executable path a client should trust for a pipe server
// started from the same binary.
func SelfPath() (string, error) { return os.Executable() }
