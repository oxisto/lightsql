//go:build unix

package wal

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes a non-blocking exclusive flock, reporting whether it got one.
//
// syscall.Flock rather than a lock file holding a process id: the kernel
// releases this when the process ends, however it ends, so a crash leaves
// nothing stale. A pid file has to be validated against a live process, and
// the pid may have been reused by then.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, err
	}
}
