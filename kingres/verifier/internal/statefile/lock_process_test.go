// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package statefile

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// Real concurrent processes (L12-02). Goroutines share one process and one
// descriptor table; the lock is a lock between processes, so it is tested between
// processes: the test binary is started several times as a helper, and each
// helper takes the lock many times and, inside it, makes a mark with an
// exclusive create, which two holders cannot both do.

const helperEnv = "KINGRES_LOCK_HELPER"

// TestLockHelperProcess is the body of a helper process; it does nothing in a
// normal run.
func TestLockHelperProcess(t *testing.T) {
	spec := os.Getenv(helperEnv)
	if spec == "" {
		t.Skip("not a helper process")
	}
	parts := strings.Split(spec, "|")
	dir := parts[0]
	rounds, _ := strconv.Atoi(parts[1])
	state := filepath.Join(dir, "state")
	for i := 0; i < rounds; i++ {
		l, err := Acquire(state, 60*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, "acquire:", err)
			os.Exit(3)
		}
		inside, err := os.OpenFile(filepath.Join(dir, "inside"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			// two holders at once
			v, _ := os.OpenFile(filepath.Join(dir, "violations"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
			fmt.Fprintf(v, "%d\n", os.Getpid())
			v.Close()
		} else {
			inside.Close()
		}
		// a read, a pause and a write of a counter: the pattern of a state update, in
		// which a second holder that read the same value would lose an update
		b, _ := os.ReadFile(filepath.Join(dir, "counter"))
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(time.Millisecond)
		_ = os.WriteFile(filepath.Join(dir, "counter"), []byte(strconv.Itoa(n+1)), 0o600)
		if err == nil {
			_ = os.Remove(filepath.Join(dir, "inside"))
		}
		l.Release()
	}
	os.Exit(0)
}

func runHelpers(t *testing.T, dir string, procs, rounds int) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(), helperEnv+"="+dir+"|"+strconv.Itoa(rounds))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("a helper process failed: %v\n%s", err, out)
			}
		}()
	}
	wg.Wait()
}

func checkHelperResult(t *testing.T, dir string, procs, rounds int) {
	t.Helper()
	if v, err := os.ReadFile(filepath.Join(dir, "violations")); err == nil {
		t.Fatalf("two processes held the lock at the same time (%d times):\n%s", strings.Count(string(v), "\n"), v)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "counter"))
	if got, want := strings.TrimSpace(string(b)), strconv.Itoa(procs*rounds); got != want {
		t.Fatalf("the counter is %s, want %s: an update was lost, so two processes held the lock together", got, want)
	}
}

func TestLockExcludesRealProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	const procs, rounds = 4, 25
	dir := testdir.New(t)
	runHelpers(t, dir, procs, rounds)
	checkHelperResult(t, dir, procs, rounds)
}

// The case of the report: a lock file kept with a mode that others can read, in
// the directory of the state, and several processes at once. It repeats, because
// the race was found in some rounds and not in others.
func TestLockExcludesRealProcessesOnALooseLockFile(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	if runtime.GOOS == "windows" {
		t.Skip("a lock file has no mode to loosen on Windows")
	}
	const procs, rounds = 4, 15
	for rep := 0; rep < 6; rep++ {
		dir := testdir.New(t)
		lock := lockName(filepath.Join(dir, "state"))
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lock, 0o644); err != nil {
			t.Fatal(err)
		}
		runHelpers(t, dir, procs, rounds)
		checkHelperResult(t, dir, procs, rounds)
		if st, err := os.Stat(lock); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("the lock file was not tightened in place: %v %v", st, err)
		}
	}
}
