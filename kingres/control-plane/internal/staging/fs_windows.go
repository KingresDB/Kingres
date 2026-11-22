// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build windows

package staging

import (
	"errors"
	"io/fs"
	"syscall"
	"unsafe"
)

const noFollowFlag = 0

// modesEnforced says whether file modes and owners mean something on this system. They do not on Windows; the directory ACL is the
// operator's to set.
const modesEnforced = false

func spaceOf(dir string) (Space, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return Space{}, err
	}
	var avail, total, free uint64
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	r, _, e := proc.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return Space{}, e
	}
	return Space{Total: total, Free: avail}, nil
}

func isDiskFullOS(err error) bool {
	var en syscall.Errno
	if errors.As(err, &en) {
		return en == 112 || en == 39 // ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL
	}
	return false
}

func ownedByUs(fs.FileInfo) bool { return true }
