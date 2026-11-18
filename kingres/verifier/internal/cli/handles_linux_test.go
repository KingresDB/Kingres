// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

func fdCount(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	return len(ents)
}

// The Linux counterpart of the Windows handle tests: the descriptors of the process
// do not grow over runs that read, lock, replace and close the files of the state
// commands, with the collector on and with it off (a descriptor that only a finalizer
// closes shows only with it off).
func TestStateRunsLeaveNoDescriptorOpen(t *testing.T) {
	for _, collect := range []bool{true, false} {
		x := newExitEnv(t)
		th := newThief(t)
		run1 := func() {
			walkWeakEvidenceRuns(t, th)
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
			if code, _, _ := run(t, x.cp("--state", st, "--note", x.oldN)...); code != 1 {
				t.Fatalf("the rollback gave %d", code)
			}
		}
		func() {
			if !collect {
				defer debug.SetGCPercent(debug.SetGCPercent(-1))
			}
			settled := func() int {
				if collect {
					runtime.GC()
					time.Sleep(50 * time.Millisecond)
					runtime.GC()
				}
				return fdCount(t)
			}
			run1()
			before := settled()
			for i := 0; i < 40; i++ {
				run1()
			}
			if after := settled(); after > before+5 {
				t.Fatalf("collector %v: the descriptors of the process grew from %d to %d over 40 runs: one is left open", collect, before, after)
			}
		}()
	}
}
