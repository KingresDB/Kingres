// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package main

import (
	"os"
	"syscall"
)

// takeLock takes an exclusive lock that lives as long as the process; false when
// another process holds it.
func takeLock(path string) bool {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = f.Close()
		return false
	}
	return true // f stays open (and locked) until the process ends
}

// alive reports whether a process with this pid exists (signal 0 sends nothing).
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
