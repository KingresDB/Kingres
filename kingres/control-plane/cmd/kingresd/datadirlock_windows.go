// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package main

import (
	"errors"
	"syscall"
)

// errorSharingViolation is ERROR_SHARING_VIOLATION: another handle of the file is open without sharing.
const errorSharingViolation syscall.Errno = 32

// lockFile opens the file without sharing: a second open fails while the first is held. The handle is not inheritable. Only the sharing violation means "in use":
// another error (access denied, a bad path) is the error itself.
func lockFile(path string) (func(), error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, errDataDirInUse
		}
		return nil, err
	}
	return func() { _ = syscall.CloseHandle(h) }, nil
}
