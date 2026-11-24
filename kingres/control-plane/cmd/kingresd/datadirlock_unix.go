// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package main

import (
	"os"
	"syscall"
)

// lockFile takes the exclusive lock of the file, without waiting; the lock ends with the process or with the returned function. The descriptor is not inherited by the
// instances that kingresd starts (Go opens files close-on-exec), so they do not keep the lock after kingresd ended.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errDataDirInUse
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
