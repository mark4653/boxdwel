//go:build windows

package enter

import (
	"syscall"
	"unsafe"
)

const fileAttributeReparsePoint = 0x00000400

var procGetFileAttributes = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFileAttributesW")

// isReparse reports the Windows reparse attribute; missing paths are not reparse.
func isReparse(path string) (bool, error) {
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attrs, _, callErr := procGetFileAttributes.Call(uintptr(unsafe.Pointer(ptr)))
	if uint32(attrs) == 0xFFFFFFFF {
		if callErr == syscall.ERROR_FILE_NOT_FOUND || callErr == syscall.ERROR_PATH_NOT_FOUND {
			return false, nil
		}
		return false, callErr
	}
	return attrs&fileAttributeReparsePoint != 0, nil
}
