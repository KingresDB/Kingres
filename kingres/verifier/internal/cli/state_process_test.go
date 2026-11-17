// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// L12-02 with the real command and real processes, on every system. The test
// binary is started many times as the program, each run advancing one state to a
// different size at the same moment; a watcher reads the state while they run.
// Whatever the interleaving the state ends at the largest size and never went
// down, which is what the lock and the second look before the write are for.

const stateHelperEnv = "KINGRES_STATE_HELPER_ARGS"

// TestStateHelperProcess is the body of a child; it does nothing in a normal run.
func TestStateHelperProcess(t *testing.T) {
	spec := os.Getenv(stateHelperEnv)
	if spec == "" {
		t.Skip("not a helper process")
	}
	var args []string
	if err := json.Unmarshal([]byte(spec), &args); err != nil {
		os.Exit(99)
	}
	os.Exit(Run(args, nopWriter{}, os.Stderr, "test"))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestStateAcrossRealProcessesNeverMovesBackwards(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	all := leaves(320)
	root := t.TempDir()
	var notes []string
	for size := 301; size <= 320; size++ {
		notes = append(notes, publishLog(t, root, s, all, size))
	}
	files := make([]string, len(notes))
	for i, n := range notes {
		files[i] = writeNote(t, n)
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	publishLog(t, root, s, all, 300)
	if code, out, errs := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	initial := fileText(t, state)
	// eight processes at once (a process is costly to start on Windows)
	files = files[len(files)-8:]
	rounds := 8
	if runtime.GOOS == "windows" {
		rounds = 3
	}
	for round := 0; round < rounds; round++ {
		if err := os.WriteFile(state, []byte(initial), 0o600); err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" {
			// a lock file kept from a time the directory was shared, in loose mode
			lock := filepath.Join(dir, ".kingres-state.lock")
			_ = os.Remove(lock)
			if err := os.WriteFile(lock, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(lock, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		var stop atomic.Bool
		var wentDown atomic.Int64
		var wg sync.WaitGroup
		watcher := make(chan struct{})
		go func() {
			defer close(watcher)
			if runtime.GOOS == "windows" {
				return // a reader of the state can make a rename fail there (README); only the end is checked
			}
			last := uint64(300)
			for !stop.Load() {
				if b, err := os.ReadFile(state); err == nil {
					var rec struct {
						Size uint64 `json:"size"`
					}
					if json.Unmarshal(b, &rec) == nil {
						if rec.Size < last {
							wentDown.Store(int64(rec.Size)<<20 | int64(last))
						}
						last = rec.Size
					}
				}
				time.Sleep(time.Millisecond)
			}
		}()
		codes := make([]int, len(files))
		msgs := make([]string, len(files))
		for i := range files {
			wg.Add(1)
			go func() {
				defer wg.Done()
				args, _ := json.Marshal(stateArgs(tr, root, state, "--note", files[len(files)-1-i]))
				cmd := exec.Command(os.Args[0], "-test.run=^TestStateHelperProcess$", "-test.count=1")
				cmd.Env = append(os.Environ(), stateHelperEnv+"="+string(args))
				out, err := cmd.CombinedOutput()
				msgs[i] = string(out)
				if ee, ok := err.(*exec.ExitError); ok {
					codes[i] = ee.ExitCode()
				} else if err != nil {
					codes[i] = -1
				}
			}()
		}
		wg.Wait()
		stop.Store(true)
		<-watcher
		if v := wentDown.Load(); v != 0 {
			t.Fatalf("round %d: the state went down from %d to %d while the runs were going", round, v&(1<<20-1), v>>20)
		}
		for i, c := range codes {
			if c != 0 && c != 1 {
				t.Fatalf("round %d: process %d ended with status %d: %s", round, i, c, msgs[i])
			}
		}
		if got := stateSize(t, state); got != 320 {
			t.Fatalf("round %d: the state ended at %d, not 320: a higher state was lost", round, got)
		}
		if runtime.GOOS != "windows" {
			if st, err := os.Stat(filepath.Join(dir, ".kingres-state.lock")); err != nil || st.Mode().Perm() != 0o600 {
				t.Fatalf("round %d: the lock file is %v %v, want mode 0600", round, st, err)
			}
		}
	}
}
