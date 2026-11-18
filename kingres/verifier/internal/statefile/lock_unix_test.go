// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package statefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// L12-02: a lock file is never removed and never replaced while it can be held.
// Replacing it let two runs hold "the" lock together (each locked a different
// file), and a late writer then moved the state backwards. A lock file that is
// ours with a loose mode is locked first, as it is, and its mode is made 0600 in
// place; one of another owner is refused and left where it is.

func TestLockFileWithALooseModeIsLockedInPlaceAndTightened(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o660, 0o666, 0o604, 0o602} {
		t.Run(mode.String(), func(t *testing.T) {
			state := filepath.Join(testdir.New(t), "state")
			lock := lockName(state)
			if err := os.WriteFile(lock, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(lock, mode); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(lock)
			l, err := Acquire(state, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Release()
			after, err := os.Stat(lock)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Fatal("the lock file was replaced: a run that held the old one holds nothing now")
			}
			if after.Mode().Perm() != 0o600 {
				t.Fatalf("the lock file has mode %04o after the lock was taken, want 0600", after.Mode().Perm())
			}
			// Another run is kept out by the very same file.
			if _, err := Acquire(state, 150*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
				t.Fatalf("a second lock on the file that is held: %v", err)
			}
		})
	}
}

// Whoever keeps the old file open holds the lock on the file the auditor locks:
// the auditor waits for them, and does not walk around them.
func TestLockHeldByAnotherDescriptorBlocksARunWhateverTheMode(t *testing.T) {
	state := filepath.Join(testdir.New(t), "state")
	lock := lockName(state)
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lock, 0o666); err != nil {
		t.Fatal(err)
	}
	old, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(old.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(state, 200*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("a lock held on the file of mode 0666 did not hold: %v", err)
	}
	old.Close()
	l, err := Acquire(state, time.Second)
	if err != nil {
		t.Fatalf("after it was released: %v", err)
	}
	l.Release()
}

func TestLockFileOfAnotherOwnerIsRefusedAndKept(t *testing.T) {
	state := filepath.Join(testdir.New(t), "state")
	lock := lockName(state)
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(lock)
	prev := ownerOf
	t.Cleanup(func() { ownerOf = prev })
	ownerOf = func(p string, st fs.FileInfo) (int, bool) {
		if p == lock {
			return selfUID() + 1, true
		}
		return prev(p, st)
	}
	_, err := Acquire(state, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "remove it by hand") {
		t.Fatalf("a lock file of another owner: %v", err)
	}
	after, serr := os.Stat(lock)
	if serr != nil || !os.SameFile(before, after) {
		t.Fatal("the lock file of another owner was removed or replaced")
	}
}

// A lock file removed by hand between the open and the lock: the name leads to
// another file, and the lock is the one on the file the name leads to now.
func TestLockIsOnTheFileTheNameLeadsToAfterTheLock(t *testing.T) {
	state := filepath.Join(testdir.New(t), "state")
	lock := lockName(state)
	first := true
	beforeLock = func() {
		if first {
			first = false
			_ = os.Remove(lock)
		}
	}
	t.Cleanup(func() { beforeLock = func() {} })
	l, err := Acquire(state, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := Acquire(state, 150*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("the lock does not keep another run out: %v", err)
	}
}
