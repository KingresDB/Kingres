// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/statefile"
)

// holdLock takes the lock of a state file as another run would, and shortens
// the wait of the commands for the length of the test.
func holdLock(t *testing.T, state string) (release func()) {
	t.Helper()
	path, err := statefile.Resolve(state)
	if err != nil {
		t.Fatal(err)
	}
	l, err := statefile.Acquire(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	old := statefile.LockWait
	statefile.LockWait = 300 * time.Millisecond
	return func() {
		statefile.LockWait = old
		l.Release()
	}
}
