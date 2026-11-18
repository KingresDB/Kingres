// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package lease

import (
	"os"
	"syscall"
)

// LockHeld says whether another process holds the exclusive lock of the file (the watchdog of an instance holds its lock
// as long as it runs): a failover asks it before it counts an old primary as covered by a watchdog (F2-13). The lock
// is taken and given back at once; a file that does not exist or cannot be opened is "not held".
func LockHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
