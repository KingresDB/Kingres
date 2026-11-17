// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package statefile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
)

func rename(from, to string) error { return os.Rename(from, to) }

// syncDir flushes the directory so that the rename survives a crash: without
// it the old state can come back, which is the safe direction but loses the
// progress.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- the directory of the state
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

// Lock is an exclusive lock on the lock file of a state.
type Lock struct{ f *os.File }

// Acquire takes the lock, polling for at most wait. The lock file is opened
// without following a link and must be a regular file (safefile.OpenNoFollow).
// See lockName for what the lock covers.
func Acquire(path string, wait time.Duration) (*Lock, error) {
	f, err := openLock(lockName(path))
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) // #nosec G115 -- a descriptor
		if err == nil {
			return &Lock{f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Release drops the lock. The lock file stays: removing it would let a
// waiting run lock a file that no longer has a name.
func (l *Lock) Release() { _ = l.f.Close() }

// openLock opens the lock file and makes sure it is the auditor's: owned by the
// user running this command and not accessible to group or others. A lock file
// that is not (left from a time the directory was shared, or made by hand with
// a loose mode) could be held by somebody else for ever, so it is removed and
// made again with mode 0600: whoever kept the old one open holds a file that has
// no name any more. The directory itself was checked by CheckPrivate, so
// nobody else can put a file back.
func openLock(name string) (*os.File, error) {
	for attempt := 0; attempt < 3; attempt++ {
		f, err := safefile.OpenNoFollow(name, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		uid, known := ownerOf(name, st)
		if (!known || uid == selfUID()) && st.Mode().Perm()&0o077 == 0 {
			return f, nil
		}
		_ = f.Close()
		if err := os.Remove(name); err != nil {
			return nil, fmt.Errorf("the lock file %s is not private to the user running this command (owner %d, mode %04o) and cannot be replaced: %w", name, uid, st.Mode().Perm(), err)
		}
	}
	return nil, fmt.Errorf("the lock file %s could not be made private", name)
}
