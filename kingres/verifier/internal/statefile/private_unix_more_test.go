// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package statefile

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// More of the private check, from a mutation run of the package.

func TestOwnerOfReadsTheRealOwner(t *testing.T) {
	d := t.TempDir()
	st, err := os.Stat(d)
	if err != nil {
		t.Fatal(err)
	}
	uid, ok := ownerOf(d, st)
	if !ok || uid != os.Geteuid() {
		t.Fatalf("ownerOf: %d %v, this user is %d", uid, ok, os.Geteuid())
	}
	if selfUID() != os.Geteuid() {
		t.Fatalf("selfUID %d", selfUID())
	}
}

func TestCheckPrivateAtLinkLimits(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(base, "real"), 0o700)
	prev := filepath.Join(base, "real")
	// a chain of links, each pointing at the one before
	chain := func(n int) string {
		last := prev
		for i := 1; i <= n; i++ {
			l := filepath.Join(base, "l"+strconv.Itoa(n)+"-"+strconv.Itoa(i))
			if err := os.Symlink(last, l); err != nil {
				t.Fatal(err)
			}
			last = l
		}
		return last
	}
	if err := CheckPrivate(filepath.Join(chain(40), "state")); err != nil {
		t.Errorf("%d links: %v", maxLinks, err)
	}
	if err := CheckPrivate(filepath.Join(chain(41), "state")); err == nil || !strings.Contains(err.Error(), "too many links") {
		t.Errorf("%d links: %v", maxLinks+1, err)
	}
}

func TestCheckPrivateOfARelativePath(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(base, "d"), 0o700)
	mkdir(t, filepath.Join(base, "open"), 0o777)
	t.Chdir(base)
	if err := CheckPrivate("d/state"); err != nil {
		t.Errorf("a relative path in a private directory: %v", err)
	}
	if err := CheckPrivate("open/state"); err == nil {
		t.Error("a relative path in a directory others can write")
	}
	if err := CheckPrivate("./d/../d/state"); err != nil {
		t.Errorf("a relative path with . and ..: %v", err)
	}
	// a missing directory is an error, a missing file is not
	if err := CheckPrivate("missing/state"); err == nil {
		t.Error("a missing directory")
	}
	if err := CheckPrivate("d/state"); err != nil {
		t.Errorf("a file that does not exist yet: %v", err)
	}
	// something that is not a directory in the middle of the path
	if err := os.WriteFile("file", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate("file/state"); err == nil {
		t.Error("a file used as a directory")
	}
}

func TestCheckPrivateOwnersOfLinksAndHolders(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	priv := filepath.Join(base, "priv")
	mkdir(t, priv, 0o700)
	l := filepath.Join(base, "link")
	if err := os.Symlink(priv, l); err != nil {
		t.Fatal(err)
	}
	// a link of root is trusted, a link of this user is, a link of another is not
	for uid, ok := range map[int]bool{0: true, selfUID(): true, selfUID() + 1: false} {
		fakeOwner(t, map[string]int{l: uid})
		err := CheckPrivate(filepath.Join(l, "state"))
		if (err == nil) != ok {
			t.Errorf("a link owned by %d: %v, want ok=%v", uid, err, ok)
		}
	}
	// a directory above the holder owned by root is trusted, one owned by another is not
	for uid, ok := range map[int]bool{0: true, selfUID() + 1: false} {
		fakeOwner(t, map[string]int{base: uid})
		if err := CheckPrivate(filepath.Join(priv, "state")); (err == nil) != ok {
			t.Errorf("a parent owned by %d: %v, want ok=%v", uid, err, ok)
		}
	}
	// the holder itself must be this user's, root is not enough, and the message says which directory it is
	fakeOwner(t, map[string]int{priv: 0})
	if err := CheckPrivate(filepath.Join(priv, "state")); err == nil || !strings.Contains(err.Error(), "holds the state") {
		t.Errorf("a holder owned by root: %v", err)
	}
	// and the message of a holder that others can write says so too
	fakeOwner(t, nil)
	mkdir(t, filepath.Join(base, "h"), 0o777|fs.ModeSticky)
	if err := CheckPrivate(filepath.Join(base, "h", "state")); err == nil || !strings.Contains(err.Error(), "holds the state") {
		t.Errorf("a sticky holder that others can write: %v", err)
	}
}

func TestCheckPrivateModesOfTheState(t *testing.T) {
	d := t.TempDir()
	mkdir(t, d, 0o700)
	st := filepath.Join(d, "state")
	if err := os.WriteFile(st, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode os.FileMode
		ok   bool
	}{{0o600, true}, {0o644, true}, {0o640, true}, {0o601, true}, {0o604, true}, {0o620, false}, {0o602, false}, {0o666, false}, {0o660, false}} {
		if err := os.Chmod(st, c.mode); err != nil {
			t.Fatal(err)
		}
		if err := CheckPrivate(st); (err == nil) != c.ok {
			t.Errorf("a state of mode %04o: %v, want ok=%v", c.mode, err, c.ok)
		}
	}
	_ = fs.ModeSticky
}

func TestSyncDirReportsAMissingDirectory(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "no-such-directory")); err == nil {
		t.Fatal("no error for a directory that does not exist")
	}
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("a directory: %v", err)
	}
}
