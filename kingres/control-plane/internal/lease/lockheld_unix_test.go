// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package lease

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLockHeldSeesAnotherHolder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "watchdog.1.lock")
	if LockHeld(p) {
		t.Fatal("a file that does not exist is not held")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if LockHeld(p) {
		t.Fatal("an unlocked file is not held")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if !LockHeld(p) {
		t.Fatal("a file locked by another descriptor must be seen as held")
	}
	_ = f.Close()
	if LockHeld(p) {
		t.Fatal("the lock was given up")
	}
}
