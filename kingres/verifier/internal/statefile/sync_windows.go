// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package statefile

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
)

var (
	kernel32   = syscall.NewLazyDLL("kernel32.dll")
	procLockEx = kernel32.NewProc("LockFileEx")
	procMove   = kernel32.NewProc("MoveFileExW")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusive       = 0x2
	errLockViolation        = syscall.Errno(33)
)

// renameWait is how long a rename that Windows refuses because another program
// has the file open is tried again (L14-05): a scanner or a backup that reads
// the state without sharing delete holds it for a moment, and the run has
// verified a checkpoint it would otherwise lose. A test shortens it.
var renameWait = 3 * time.Second

const (
	errAccessDenied     = syscall.Errno(5)
	errSharingViolation = syscall.Errno(32)
)

// rename replaces the target and asks for the move to be written through to
// disk before it returns: Windows has no way to flush a directory, and this is
// the flag that gives the same guarantee. A refusal that a program holding the
// target open causes (access denied, a sharing or a lock violation) is tried
// again every 20 ms for renameWait, and then the error says that another program
// may hold the file.
func rename(from, to string) error {
	f, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	const replaceExisting, writeThrough = 0x1, 0x8
	deadline := time.Now().Add(renameWait)
	for {
		r, _, e := procMove.Call(uintptr(unsafe.Pointer(f)), uintptr(unsafe.Pointer(t)), replaceExisting|writeThrough)
		if r != 0 {
			return nil
		}
		busy := e == errAccessDenied || e == errSharingViolation || e == errLockViolation
		if !busy {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: e}
		}
		if time.Now().After(deadline) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: fmt.Errorf("%w; another program may have the file open without sharing delete (a backup, a virus scanner, an indexer, a viewer), tried for %s: close it and run again, nothing was changed", e, renameWait)}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func syncDir(string) error { return nil }

// Lock is an exclusive lock on the lock file of a state.
type Lock struct{ f *os.File }

// Acquire takes the lock, polling for at most wait. The lock file is opened
// without following a link and must be a regular file (safefile.OpenNoFollow).
// See lockName for what the lock covers.
func Acquire(path string, wait time.Duration) (*Lock, error) {
	f, err := safefile.OpenNoFollow(lockName(path), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		var ol syscall.Overlapped
		r, _, e := procLockEx.Call(f.Fd(), lockfileExclusive|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
		if r != 0 {
			return &Lock{f}, nil
		}
		if e != errLockViolation {
			_ = f.Close()
			return nil, e
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Release drops the lock (closing the handle releases it).
func (l *Lock) Release() { _ = l.f.Close() }
