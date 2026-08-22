package wal

import (
	"os"
	"path/filepath"

	"github.com/oxisto/lightsql/internal/pgerr"
)

// LockName is the file whose lock says a process has this directory open.
const LockName = "LOCK"

// lockDir takes an exclusive lock on the database directory, refusing rather
// than waiting if another process already holds it.
//
// It locks a file of its own rather than the log, and that is the whole point.
// A checkpoint replaces the log by renaming a new file over it, so a lock held
// on the log would end up on an unlinked inode while the next process locked
// the replacement -- and both would believe they had it. That is not a
// hypothetical: a second process shutting down cleanly rewrote the log while
// the first was still running, and every commit the first made afterwards went
// into a file with no name, invisible and lost at exit. Neither process
// reported anything wrong.
//
// The lock is advisory and held by an open descriptor, so the operating system
// drops it when the process dies however it dies. That is what makes a crash
// leave nothing to clean up: there is no stale lock to time out or to override,
// which is the failure mode of a lock file holding a process id.
func lockDir(dir string) (*os.File, error) {
	path := filepath.Join(dir, LockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, pgerr.Newf(pgerr.InternalError, "opening the lock file: %v", err)
	}

	held, err := tryLock(f)
	if err != nil {
		_ = f.Close()
		return nil, pgerr.Newf(pgerr.InternalError, "locking %s: %v", path, err)
	}
	if !held {
		_ = f.Close()
		return nil, pgerr.Newf(pgerr.ObjectInUse,
			"database directory %q is open in another process", dir).
			WithDetail("Only one process may open a lightsql directory at a time.").
			WithHint("Close the other process, or copy the directory to look at it.")
	}
	return f, nil
}

// unlockDir releases the lock. Closing the descriptor is what releases it; the
// file is left behind deliberately, since removing it would race a process that
// is opening the directory at that moment and has the file open but not yet
// locked.
func unlockDir(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}
