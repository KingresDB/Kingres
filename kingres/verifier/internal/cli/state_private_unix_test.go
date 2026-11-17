// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L10-06: the reported case. The state is in a private directory below a
// directory that another user can change. That user renames the directory, puts
// a link of its own in its place that leads to an empty private directory of
// the auditor, and the publisher serves the genuine older checkpoint: the run
// ended with VERIFIED and status 0 although the real state was at a larger size.
// Now the first use is refused, and so is every form of the redirect.
func TestRedirectedStateIsRefused(t *testing.T) {
	x := newExitEnv(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(base, "shared")
	for _, d := range []string{shared, filepath.Join(shared, "auditor"), filepath.Join(base, "empty")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	// 1. The first use below the shared directory is refused, naming it.
	st := filepath.Join(shared, "auditor", "state")
	code, out, errs := run(t, x.cp("--state", st)...)
	if code != 2 || strings.Contains(out, "VERIFIED") || !strings.Contains(errs, "group or others") || !strings.Contains(errs, shared) {
		t.Fatalf("below a shared directory: %d\n%s%s", code, out, errs)
	}
	if _, err := os.Stat(st); err == nil {
		t.Fatal("a state was written below a shared directory")
	}
	// 2. The real state is at 700, in the auditor's own place.
	real := filepath.Join(base, "mine", "state")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := run(t, x.cp("--state", real)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	// 3. The other user's link replaces the directory: the genuine size-300 note
	// is a rollback of the real state, and must not end with 0.
	if err := os.RemoveAll(filepath.Join(shared, "auditor")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "empty"), filepath.Join(shared, "auditor")); err != nil {
		t.Fatal(err)
	}
	code, out, errs = run(t, x.cp("--note", x.oldN, "--state", filepath.Join(shared, "auditor", "state"))...)
	if code != 2 || strings.Contains(out, "VERIFIED") {
		t.Fatalf("a redirected directory: %d\n%s%s", code, out, errs)
	}
	// 4. The second form: the name of the state is a link in the shared directory.
	link := filepath.Join(shared, "st-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	code, out, errs = run(t, x.cp("--note", x.oldN, "--state", link)...)
	if code != 2 || strings.Contains(out, "VERIFIED") {
		t.Fatalf("a link in a shared directory: %d\n%s%s", code, out, errs)
	}
	// 5. The real state was not touched by any of it.
	if stateSize(t, real) != 700 {
		t.Fatalf("the real state moved to %d", stateSize(t, real))
	}
	// 6. --allow-shared-state-dir keeps its meaning: accepted, with its note.
	code, out, errs = run(t, x.cp("--note", x.oldN, "--state", link, "--allow-shared-state-dir")...)
	if !strings.Contains(out, "accepted by --allow-shared-state-dir") {
		t.Fatalf("allow-shared: %d\n%s%s", code, out, errs)
	}
	if code != 1 || strings.Contains(out, "VERIFIED") {
		t.Fatalf("with the flag the rollback is found as before: %d\n%s%s", code, out, errs)
	}
}
