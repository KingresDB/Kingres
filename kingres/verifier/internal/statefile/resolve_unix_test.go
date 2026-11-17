// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package statefile

import (
	"os"
	"path/filepath"
	"testing"
)

// L12-06: ".." after a link is the parent of the link's target, in the check and
// in the name the state is stored at, for a relative name and an absolute one.
func TestDotDotAfterALinkMeansTheParentOfTheTarget(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "a", "b")
	mkdir(t, target, 0o700)
	mkdir(t, filepath.Join(base, "work"), 0o700)
	if err := os.Symlink(target, filepath.Join(base, "work", "sl")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "a", "s3") // the parent of the target, not the parent of the link
	abs := filepath.Join(base, "work") + "/sl/../s3"
	got, err := Resolve(abs)
	if err != nil || got != want {
		t.Fatalf("absolute: %q %v, want %q", got, err, want)
	}
	t.Chdir(filepath.Join(base, "work"))
	got, err = Resolve("sl/../s3")
	if err != nil || got != want {
		t.Fatalf("relative: %q %v, want %q", got, err, want)
	}
	// the check looks at the directory the name leads to: open it up and the
	// relative name is refused, as the absolute one is
	if err := os.Chmod(filepath.Join(base, "a"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate("sl/../s3"); err == nil {
		t.Fatal("the relative name with .. after a link was checked at the wrong directory")
	}
	if err := CheckPrivate(abs); err == nil {
		t.Fatal("the absolute name with .. after a link was checked at the wrong directory")
	}
}
