// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package main

import "syscall"

// lockFile opens the file without sharing: a second open fails while the first is held. The handle is not inheritable.
func lockFile(path string) (func(), error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, errDataDirInUse
	}
	return func() { _ = syscall.CloseHandle(h) }, nil
}
