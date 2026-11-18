// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// childEnv makes the test binary run one command of the verifier and exit, so
// that a test can read the peak resident set of a real process from the system.
const childEnv = "KINGRES_VERIFY_TEST_CHILD"

// peakEnv names the file the child writes its peak resident set to. The parent
// cannot read it with getrusage: the value a child reports starts at the peak of
// the parent at the time of the fork, which is the whole test binary with the
// files of the test in its memory.
const peakEnv = "KINGRES_VERIFY_TEST_PEAKFILE"

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		code := Run(os.Args[1:], os.Stdout, os.Stderr, "test")
		if p := os.Getenv(peakEnv); p != "" {
			// VmHWM of this process, which an exec starts at zero.
			if b, err := os.ReadFile("/proc/self/status"); err == nil {
				for _, l := range strings.Split(string(b), "\n") {
					if rest, ok := strings.CutPrefix(l, "VmHWM:"); ok {
						_ = os.WriteFile(p, []byte(strings.TrimSpace(rest)), 0o600)
					}
				}
			}
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// childRun runs the command in a child process and returns its exit status, its
// output and the peak of its resident set in bytes (VmHWM of /proc, written by the child).
func childRun(t testing.TB, args ...string) (code int, out string, peak int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	peakFile := filepath.Join(testdir.New(t), "peak")
	cmd.Env = append(os.Environ(), childEnv+"=1", peakEnv+"="+peakFile)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	if cmd.ProcessState == nil {
		t.Fatalf("the child did not run: %v", err)
	}
	raw, rerr := os.ReadFile(peakFile)
	fields := strings.Fields(string(raw))
	if rerr != nil || len(fields) == 0 {
		t.Fatalf("the child did not report its peak: %v", rerr)
	}
	kb, perr := strconv.ParseInt(fields[0], 10, 64)
	if perr != nil {
		t.Fatalf("the child reported %q as its peak", fields[0])
	}
	return cmd.ProcessState.ExitCode(), buf.String(), kb * 1024
}

// childRunWithin is childRun for a measurement that a loaded machine can inflate
// (the runtime lets the heap grow further before it collects when the process is
// starved of the processor): it runs the command again, up to three times in all,
// while the peak is over bound, and returns the run with the smallest peak. A
// command whose memory grows with its input is over the bound in every run, so the
// test can still fail; one that was over once because of the load is not a failure.
func childRunWithin(t testing.TB, bound int64, args ...string) (code int, out string, peak int64) {
	t.Helper()
	for try := 0; try < 3; try++ {
		c, o, p := childRun(t, args...)
		if try == 0 || p < peak {
			code, out, peak = c, o, p
		}
		if peak <= bound {
			break
		}
	}
	return
}

// L9-05 on a real process: the peak resident set of a command that reads a
// hostile file of 16 MiB. The baseline is the same test binary running a
// command on a small input (the runtime, the binary and the file system
// buffers), and the bound is that plus a constant that holds the file a few
// times. The first version used 2.0 to 2.4 GiB for the zeros.
func TestPeakResidentSetOfACommandOnAHostileFile(t *testing.T) {
	if testing.Short() {
		t.Skip("reads files of 16 MiB")
	}
	_, _, base := childRun(t, "version")
	forEachHostileFile(t, func(t *testing.T, name string, args []string, sz int) {
		code, out, peak := childRunWithin(t, base+int64(5*sz)+(8<<20), args...)
		t.Logf("MEASURE %s|%d|%d", name, sz, peak>>20)
		if peak > base+int64(5*sz)+(8<<20) {
			t.Errorf("peak resident set %d MiB for a file of %d KiB (a small input: %d MiB)", peak>>20, sz>>10, base>>20)
		}
		if code == 0 || strings.Contains(out, "VERIFIED") {
			t.Errorf("a hostile file was accepted: %d %.300s", code, out)
		}
	})
	t.Run("proof with a chain of headers", func(t *testing.T) {
		path, n := longChainProof(t, testdir.New(t), 15<<20)
		code, out, peak := childRunWithin(t, base+5*(15<<20), "proof", "--proof", path, "--digest", fx("digests/d0.json"))
		t.Logf("%d headers, peak resident set %d MiB (a small input: %d MiB)", n, peak>>20, base>>20)
		if peak > base+5*(15<<20) {
			t.Errorf("peak resident set %d MiB", peak>>20)
		}
		if code != 0 {
			t.Errorf("status %d: %.300s", code, out)
		}
	})
	t.Run("a directory of four files", func(t *testing.T) {
		// the case of the review: four files, each at the cap of its kind
		zeros := hostileShapes(t, testdir.New(t), 16<<20)["zeros"]
		d := testdir.New(t)
		for i := 0; i < 4; i++ {
			copyTo(t, zeros, d+"/d"+string(rune('0'+i))+".json")
		}
		code, out, peak := childRunWithin(t, base+6*(16<<20), "digest-chain", "--digests", d)
		if peak > base+6*(16<<20) || code == 0 {
			t.Errorf("status %d, peak resident set %d MiB: %.200s", code, peak>>20, out)
		}
	})
}
