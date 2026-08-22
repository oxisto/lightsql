//go:build !unix && !windows

package wal

import (
	"errors"
	"os"
)

// tryLock has no implementation on this platform.
//
// It reports an error rather than pretending to lock. A directory that is
// unlocked because locking is unavailable looks identical to one that is
// unlocked because nothing holds it, and the consequence of getting that wrong
// is a second process silently destroying the first one's writes. An in-memory
// database is unaffected, since it never opens a directory at all.
func tryLock(*os.File) (bool, error) {
	return false, errors.New("locking a database directory is not supported on this platform")
}
