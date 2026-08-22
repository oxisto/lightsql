//go:build windows

package wal

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// The Windows equivalent of flock. LockFileEx is reached through the lazy DLL
// machinery in syscall rather than through golang.org/x/sys, so that installing
// lightsql still pulls in nothing at all -- the promise the root go.mod makes
// and TestNoRuntimeDependencies enforces.
var (
	kernel32    = syscall.NewLazyDLL("kernel32.dll")
	lockFileEx  = kernel32.NewProc("LockFileEx")
	errLockFail = errors.New("LockFileEx failed")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
	errLockViolation        = syscall.Errno(33)
)

// tryLock takes a non-blocking exclusive lock on the whole file.
//
// Windows releases it when the handle closes, including when the process dies,
// so this has the same property that makes the Unix side leave nothing stale
// behind after a crash.
func tryLock(f *os.File) (bool, error) {
	var overlapped syscall.Overlapped
	r, _, err := lockFileEx.Call(
		f.Fd(),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		// The whole file, however long it becomes.
		^uintptr(0), ^uintptr(0),
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r != 0 {
		return true, nil
	}
	if errno, ok := err.(syscall.Errno); ok {
		switch errno {
		case errLockViolation, syscall.ERROR_IO_PENDING:
			return false, nil
		}
		return false, errno
	}
	return false, errLockFail
}
