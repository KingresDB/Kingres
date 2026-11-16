// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package statefile

import (
	"errors"
	"os"
	"syscall"
	"time"
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

// Acquire takes the lock, polling for at most wait.
func Acquire(path string, wait time.Duration) (*Lock, error) {
	f, err := os.OpenFile(lockName(path), os.O_RDWR|os.O_CREATE, 0o644) // #nosec G304 -- next to the state
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
