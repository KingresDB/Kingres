// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package cli

import (
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

func handleCount(t *testing.T) uint32 {
	t.Helper()
	k := syscall.NewLazyDLL("kernel32.dll")
	getCurrent := k.NewProc("GetCurrentProcess")
	getCount := k.NewProc("GetProcessHandleCount")
	h, _, _ := getCurrent.Call()
	var n uint32
	if r, _, err := getCount.Call(h, uintptr(unsafe.Pointer(&n))); r == 0 {
		t.Fatalf("GetProcessHandleCount: %v", err)
	}
	return n
}

// L9-13: a failure seen once on Windows was a test directory that could not be
// removed at the end of a test ("The directory is not empty"), which is what a
// file that is deleted while a handle to it is open looks like. This test
// counts the handles of the process around runs that read, lock, replace and
// close every kind of file the state commands touch: the count does not grow, so
// no handle of this program is left open when a run returns. (A scanner that
// opens a new file for a moment, which the reviewer thought likelier, is outside
// the process and cannot be tested here.)
func TestStateRunsLeaveNoHandleOpen(t *testing.T) { stateRunsLeaveNoHandleOpen(t, true) }

// The same with the collector off (L16): a handle that only a finalizer closes is a
// leak that the first version of this test could not see, because it collected
// before it counted. With the collector off no finalizer runs, so every handle that a
// run leaves is still open when the count is made.
func TestStateRunsLeaveNoHandleOpenWithTheCollectorOff(t *testing.T) {
	stateRunsLeaveNoHandleOpen(t, false)
}

func stateRunsLeaveNoHandleOpen(t *testing.T, collect bool) {
	x := newExitEnv(t)
	run1 := func() {
		st := filepath.Join(testdir.New(t), "state")
		if code, out, errs := run(t, x.cp("--state", st, "--note", x.oldN)...); code != 0 {
			t.Fatalf("%d %s %s", code, out, errs)
		}
		if code, out, errs := run(t, x.cp("--state", st)...); code != 0 {
			t.Fatalf("%d %s %s", code, out, errs)
		}
		if code, out, errs := run(t, x.co("--old", st)...); code != 0 {
			t.Fatalf("%d %s %s", code, out, errs)
		}
		// a conflict and a refusal, which open the evidence files and the lock
		if code, _, _ := run(t, x.cp("--state", st, "--note", x.oldN)...); code != 1 {
			t.Fatalf("the rollback gave %d", code)
		}
		if code, _, _ := run(t, x.cp("--state", filepath.Join(testdir.New(t), "no", "dir", "s"))...); code != 2 {
			t.Fatalf("the missing directory gave %d", code)
		}
	}
	if !collect {
		defer debug.SetGCPercent(debug.SetGCPercent(-1))
	}
	settled := func() uint32 {
		if collect {
			// the first collection starts the runtime's own threads, which are handles
			runtime.GC()
			time.Sleep(50 * time.Millisecond)
			runtime.GC()
		}
		return handleCount(t)
	}
	run1() // the first run warms up what the runtime keeps
	before := settled()
	for i := 0; i < 60; i++ {
		run1()
	}
	after := settled()
	if after > before+10 {
		t.Fatalf("the handle count of the process grew from %d to %d over 60 runs (collector %v): a handle is left open", before, after, collect)
	}
}
