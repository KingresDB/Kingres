// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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
//
// The rule of the lock file (L12-02): a lock file is never removed and never
// replaced while it can be held, because two runs that each replace it hold two
// different files and so hold "the" lock together. A lock file that belongs to
// another user is refused (the run says how to remove it by hand). One that is
// ours is locked first, as it is, and only then is its mode made 0600, in place,
// on the descriptor that holds the lock (fchmod); nobody who keeps the old file
// open can hold it against us, since the lock is on the very file they hold. After
// the lock is taken the name is looked at again: when it no longer leads to the
// file that was locked (somebody removed the file by hand), the lock is dropped
// and the new file is locked instead.
func Acquire(path string, wait time.Duration) (*Lock, error) {
	name := lockName(path)
	deadline := time.Now().Add(wait)
	for attempt := 0; attempt < 8; attempt++ {
		f, err := safefile.OpenNoFollow(name, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if uid, known := ownerOf(name, st); known && uid != selfUID() {
			_ = f.Close()
			return nil, fmt.Errorf("the lock file %s is owned by user %d, not by the user running this command (%d): it is not removed or replaced by this program, because a lock file that is replaced while another run may hold it lets two runs hold the lock together; if no kingres-verify is running, remove it by hand (rm %s) and run again", name, uid, selfUID(), name)
		}
		beforeLock()
		if err := lockWait(f, deadline); err != nil {
			_ = f.Close()
			return nil, err
		}
		if now, err := os.Lstat(name); err != nil || !os.SameFile(st, now) {
			_ = f.Close() // the name leads to another file now: lock that one
			continue
		}
		if st.Mode().Perm()&0o077 != 0 {
			if err := f.Chmod(0o600); err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("the lock file %s is accessible to group or others (mode %04o) and its mode cannot be tightened: %w", name, st.Mode().Perm(), err)
			}
		}
		return &Lock{f}, nil
	}
	return nil, fmt.Errorf("the lock file %s keeps changing under this run", name)
}

// beforeLock is called between the open of the lock file and the lock; a test
// uses it to change the file under a run. Production code never sets it.
var beforeLock = func() {}

// lockWait takes flock on f, polling until the deadline.
func lockWait(f *os.File, deadline time.Time) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) // #nosec G115 -- a descriptor
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		if time.Now().After(deadline) {
			return ErrLockTimeout
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Release drops the lock. The lock file stays: removing it would let a
// waiting run lock a file that no longer has a name.
func (l *Lock) Release() { _ = l.f.Close() }
