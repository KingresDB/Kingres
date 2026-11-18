// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// Races against a second process that changes a file or a path under a run, as
// loops. The model of the README is that a path component that the check
// accepted can be changed only by the auditor or root; the loops use the
// auditor's own account for the swapper, which is the worst case for the check
// and the only one a test can make with one account.

const swapperEnv = "KINGRES_SWAPPER_SPEC"

// TestSwapperProcess is the body of the swapper child: it flips a name between
// two choices as fast as it can, until the stop file appears. It does nothing in
// a normal run.
//
// The spec is JSON: {"kind":"file","name":..,"a":..,"b":..,"stop":..} renames
// a or b over name (the content of a file changes between two readings);
// {"kind":"link","name":..,"a":..,"b":..,"stop":..} points the symbolic link
// name at a and then at b.
func TestSwapperProcess(t *testing.T) {
	spec := os.Getenv(swapperEnv)
	if spec == "" {
		t.Skip("not a swapper")
	}
	var s struct{ Kind, Name, A, B, Stop string }
	if err := json.Unmarshal([]byte(spec), &s); err != nil {
		os.Exit(99)
	}
	tmp := s.Name + ".swap"
	for i := 0; ; i++ {
		if _, err := os.Stat(s.Stop); err == nil {
			os.Exit(0)
		}
		pick := s.A
		if i%2 == 1 {
			pick = s.B
		}
		switch s.Kind {
		case "file":
			data, err := os.ReadFile(pick)
			if err == nil && os.WriteFile(tmp, data, 0o600) == nil {
				_ = os.Rename(tmp, s.Name) // on Windows this can fail while a reader has the file: try again
			}
		case "link":
			_ = os.Remove(tmp)
			if os.Symlink(pick, tmp) == nil {
				_ = os.Rename(tmp, s.Name)
			}
		}
	}
}

func startSwapper(t *testing.T, kind, name, a, b string) (stop func()) {
	t.Helper()
	stopFile := filepath.Join(testdir.New(t), "stop")
	spec, _ := json.Marshal(map[string]string{"kind": kind, "name": name, "a": a, "b": b, "stop": stopFile})
	cmd := exec.Command(os.Args[0], "-test.run=^TestSwapperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), swapperEnv+"="+string(spec))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		_ = os.WriteFile(stopFile, nil, 0o600)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
}

// A proof file that is swapped between the two readings. Both files verify when
// read whole, so the only reason for a failure is that the two readings differ,
// and that must be reported as such and never as VERIFIED.
func TestProofFileSwappedBetweenTheTwoReadingsIsNeverVerified(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a process")
	}
	orig, err := os.ReadFile(fx("proofs/acc5.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := testdir.New(t)
	mk := func(name, v string) string {
		body := `{"zz": "` + v + `", ` + strings.TrimPrefix(string(orig), "{")
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := mk("a.json", "A"), mk("b.json", "B")
	target := filepath.Join(dir, "proof.json")
	if err := os.WriteFile(target, mustRead(t, a), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := fx("digests/d5.json")
	for _, p := range []string{a, b} {
		if code, out, _ := run(t, "proof", "--proof", p, "--digest", digest); code != 0 {
			t.Fatalf("each file verifies alone: %d %s", code, out)
		}
	}
	stop := startSwapper(t, "file", target, a, b)
	defer stop()
	var verified, changed, other int
	deadline := time.Now().Add(15 * time.Second)
	runs := 0
	if runtime.GOOS == "windows" {
		deadline = time.Now().Add(8 * time.Second)
	}
	for time.Now().Before(deadline) && runs < 4000 {
		runs++
		code, out, errs := run(t, "proof", "--proof", target, "--digest", digest)
		switch {
		case code == 0 && strings.Contains(out, "VERIFIED"):
			verified++
		case code == 1 && strings.Contains(out, "not the same on the second reading"):
			changed++
			if strings.Contains(out, "VERIFIED") {
				t.Fatalf("VERIFIED printed for a file that changed between the readings:\n%s", out)
			}
		case (code == 2 || code == 1 && (strings.Contains(out, "another process") || strings.Contains(out, "Access is denied"))) && runtime.GOOS == "windows":
			// The swapper held the file at the moment of the open: Windows answers an
			// open that meets a rename over the file with a sharing violation or with
			// "Access is denied" (seen in a run of the whole module).
			// It is a refusal, never VERIFIED, which is what this test is about.
			other++
		default:
			t.Fatalf("status %d for a file that is one of two verifying files:\n%s%s", code, out, errs)
		}
	}
	stop()
	t.Logf("%d runs against a swapper: %d verified, %d refused for a change between the readings, %d other", runs, verified, changed, other)
	if verified == 0 {
		t.Errorf("no run verified: the loop proves nothing")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The directory of a state swapped under a run: the name of the directory is a
// link that the swapper points at one private directory and then at another that
// holds a state of its own. Every run must be coherent: the state of a directory
// only ever goes forward, whichever directory the run used, and a rollback
// against the larger state is still found.
func TestStateDirectorySwappedBetweenPrivateDirectoriesStaysCoherent(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a process")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the private-path check is not made on Windows, and a link needs a privilege there")
	}
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	all := leaves(700)
	root := testdir.New(t)
	base, _ := filepath.EvalSymlinks(testdir.New(t))
	big, small := filepath.Join(base, "big"), filepath.Join(base, "small")
	for _, d := range []string{big, small} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	publishLog(t, root, s, all, 700)
	if code, out, _ := run(t, stateArgs(tr, root, filepath.Join(big, "state"))...); code != 0 {
		t.Fatal(out)
	}
	publishLog(t, root, s, all, 300)
	if code, out, _ := run(t, stateArgs(tr, root, filepath.Join(small, "state"))...); code != 0 {
		t.Fatal(out)
	}
	bigState := fileText(t, filepath.Join(big, "state"))
	link := filepath.Join(base, "sl")
	if err := os.Symlink(big, link); err != nil {
		t.Fatal(err)
	}
	stop := startSwapper(t, "link", link, big, small)
	defer stop()
	publishLog(t, root, s, all, 500)
	counts := map[int]int{}
	for i := 0; i < 400; i++ {
		code, out, errs := run(t, stateArgs(tr, root, filepath.Join(link, "state"))...)
		counts[code]++
		if code != 0 && code != 1 && code != 2 {
			t.Fatalf("status %d\n%s%s", code, out, errs)
		}
		if code == 0 && strings.Contains(out, "FAILED") {
			t.Fatalf("status 0 with a failure: %s", out)
		}
	}
	stop()
	if fileText(t, filepath.Join(big, "state")) != bigState {
		t.Fatal("the state of the larger directory changed: a rollback or a fork moved it")
	}
	if n := stateSize(t, filepath.Join(small, "state")); n != 300 && n != 500 {
		t.Fatalf("the state of the other directory is at %d, expected 300 or 500", n)
	}
	for _, d := range []string{big, small} {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			if strings.Contains(e.Name(), ".kingres-state-") {
				t.Errorf("a temporary file was left in %s: %s", d, e.Name())
			}
		}
	}
	t.Logf("400 runs against a swapper: statuses %v", counts)
}

// The same swap towards a directory that others can write. The rule is that such
// a directory is refused when the check finds it; a swap between the check and
// the open needs somebody who may change a component the check required to be
// the auditor's, so one account can only measure how often the window is hit.
// What must hold either way: a state is never written to a directory that the
// check found open at the time of the second look after the lock, and every file
// that is written is whole.
func TestStateDirectorySwappedTowardsAnOpenDirectoryIsRefusedOrWhole(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a process")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the private-path check is not made on Windows")
	}
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	all := leaves(700)
	root := testdir.New(t)
	base, _ := filepath.EvalSymlinks(testdir.New(t))
	priv, open := filepath.Join(base, "priv"), filepath.Join(base, "open")
	if err := os.Mkdir(priv, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "sl")
	if err := os.Symlink(priv, link); err != nil {
		t.Fatal(err)
	}
	publishLog(t, root, s, all, 300)
	stop := startSwapper(t, "link", link, priv, open)
	defer stop()
	counts := map[int]int{}
	for i := 0; i < 400; i++ {
		code, out, errs := run(t, stateArgs(tr, root, filepath.Join(link, "state"))...)
		counts[code]++
		if code != 0 && code != 2 {
			t.Fatalf("status %d\n%s%s", code, out, errs)
		}
	}
	stop()
	for _, d := range []string{priv, open} {
		if _, err := os.Stat(filepath.Join(d, "state")); err == nil {
			if stateSize(t, filepath.Join(d, "state")) != 300 {
				t.Errorf("the state in %s is not whole", d)
			}
		}
	}
	_, inOpen := os.Stat(filepath.Join(open, "state"))
	t.Logf("400 runs against a swapper toward an open directory: statuses %v; a state landed in the open directory: %v", counts, inOpen == nil)
}
