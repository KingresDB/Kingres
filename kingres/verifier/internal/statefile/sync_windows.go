// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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

// What MoveFileEx says (L16-06). Its documentation lists no error codes. What was
// measured on Windows 11 with the target of the rename (TestRenameErrorCodes):
//
//   - the target has the read-only attribute: ERROR_ACCESS_DENIED (5), for ever;
//   - the target is a directory: ERROR_ACCESS_DENIED (5), for ever;
//   - the target is open by another program: ERROR_ACCESS_DENIED (5) too, whether
//     that program shares delete or not (L18-03), and it ends when the program lets go.
//
// Code 5 alone therefore cannot tell a handle from a permanent refusal, so what
// can be recognised without a handle is looked at first: the read-only attribute
// and a directory at the name are named at once, and only what is left of code 5
// is tried again for renameWait, together with ERROR_SHARING_VIOLATION (32) and
// ERROR_LOCK_VIOLATION (33), which ReplaceFile and CreateFile document for a file
// that another program holds and which a handle of a program can still produce.

// readOnlyOrDir says, for the target of a refused rename, whether the refusal is a
// property of the target (the read-only attribute, a directory) and not of a
// program that has it open: that never ends by waiting. It returns the words of the
// error, each with the advice that fits (a directory has no attribute to clear).
func readOnlyOrDir(to string) string {
	name, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return ""
	}
	attr, err := syscall.GetFileAttributes(name)
	if err != nil {
		return ""
	}
	switch {
	case attr&syscall.FILE_ATTRIBUTE_DIRECTORY != 0:
		return "the name is a directory, so a file cannot be put there: move or remove the directory, or name another file; nothing was changed"
	case attr&syscall.FILE_ATTRIBUTE_READONLY != 0:
		return "the file is read-only (the read-only attribute is set), so it cannot be replaced: clear the attribute or name another file; nothing was changed"
	}
	return ""
}

// rename replaces the target and asks for the move to be written through to
// disk before it returns: Windows has no way to flush a directory, and this is
// the flag that gives the same guarantee. A refusal that is a property of the
// target (readOnlyOrDir) is named at once. A refusal that a program holding the
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
		if what := readOnlyOrDir(to); what != "" {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: fmt.Errorf("%w: %s", e, what)}
		}
		if time.Now().After(deadline) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: fmt.Errorf("%w; another program may have the file open (a backup, a virus scanner, an indexer, a viewer), tried for %s: close it and run again, nothing was changed", e, renameWait)}
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
