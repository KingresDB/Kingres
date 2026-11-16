// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package statefile

import (
	"os"
	"syscall"
	"time"
	"unsafe"
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

// rename replaces the target and asks for the move to be written through to
// disk before it returns: Windows has no way to flush a directory, and this is
// the flag that gives the same guarantee.
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
	r, _, e := procMove.Call(uintptr(unsafe.Pointer(f)), uintptr(unsafe.Pointer(t)), replaceExisting|writeThrough)
	if r == 0 {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: e}
	}
	return nil
}

func syncDir(string) error { return nil }

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
