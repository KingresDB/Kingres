// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package statefile

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The reported case (L10-06): a directory another user can write holds the
// auditor's private directory. Another account renames it and puts a link or
// a directory of its own in its place, and the state it reads is not the
// auditor's. Every component of the path is checked, not only the last
// directory. A second account is simulated: a directory of mode 0777 is what
// another account can change, and ownerOf puts another owner on a path.

func mkdir(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// realOwner is the owner function of the program, kept so that the fakes of a
// test do not pile up on each other.
var realOwner = ownerOf

func fakeOwner(t *testing.T, owners map[string]int) {
	t.Helper()
	ownerOf = func(p string, st fs.FileInfo) (int, bool) {
		if uid, ok := owners[p]; ok {
			return uid, true
		}
		return realOwner(p, st)
	}
	t.Cleanup(func() { ownerOf = realOwner })
}

func TestCheckPrivateLooksAtEveryComponent(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// shared is writable by anybody and is above the private directory.
	shared := filepath.Join(base, "shared")
	mkdir(t, filepath.Join(shared, "auditor"), 0o700)
	mkdir(t, shared, 0o777)
	err = CheckPrivate(filepath.Join(shared, "auditor", "state"))
	if err == nil || !strings.Contains(err.Error(), shared) || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("a private directory below a directory anybody can write: %v", err)
	}
	// The same with the sticky bit: an entry that is not another user's cannot be
	// renamed or removed by them.
	if err := os.Chmod(shared, 0o777|fs.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(filepath.Join(shared, "auditor", "state")); err != nil {
		t.Fatalf("below a sticky directory: %v", err)
	}
	// A directory that holds the state is held to more: sticky is not enough.
	mkdir(t, filepath.Join(base, "holder"), 0o777|fs.ModeSticky)
	if err := CheckPrivate(filepath.Join(base, "holder", "state")); err == nil {
		t.Fatal("a sticky directory that anybody can write holds the state")
	}
	// A directory of another owner on the way.
	mkdir(t, filepath.Join(base, "o", "p"), 0o700)
	fakeOwner(t, map[string]int{filepath.Join(base, "o"): selfUID() + 1})
	if err := CheckPrivate(filepath.Join(base, "o", "p", "state")); err == nil || !strings.Contains(err.Error(), "owned by user") {
		t.Fatalf("a directory of another owner above the state: %v", err)
	}
}

func TestCheckPrivateFollowsLinks(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	priv := filepath.Join(base, "priv")
	mkdir(t, priv, 0o700)
	// A link to a private directory, in a private directory: fine.
	if err := os.Symlink(priv, filepath.Join(base, "ok-link")); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(filepath.Join(base, "ok-link", "state")); err != nil {
		t.Fatalf("a link in a private directory: %v", err)
	}
	// The second form of the report: the name of the state is a link, in a
	// directory another user can change; they re-point it to an older state.
	open := filepath.Join(base, "open")
	mkdir(t, open, 0o777)
	if err := os.WriteFile(filepath.Join(priv, "state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(priv, "state"), filepath.Join(open, "st-link")); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(filepath.Join(open, "st-link")); err == nil || !strings.Contains(err.Error(), open) {
		t.Fatalf("a link that lives where others can re-point it: %v", err)
	}
	// A link of another owner in a private directory.
	other := filepath.Join(base, "other-link")
	if err := os.Symlink(priv, other); err != nil {
		t.Fatal(err)
	}
	fakeOwner(t, map[string]int{other: selfUID() + 1})
	if err := CheckPrivate(filepath.Join(other, "state")); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("a link of another owner: %v", err)
	}
	// The way back: .. after a link is the parent of the target, as the system takes it.
	if err := CheckPrivate(base + "/ok-link/../priv/state"); err != nil {
		t.Fatalf("a path with ..: %v", err)
	}
	// A loop of links ends.
	_ = os.Symlink(filepath.Join(base, "loop2"), filepath.Join(base, "loop1"))
	_ = os.Symlink(filepath.Join(base, "loop1"), filepath.Join(base, "loop2"))
	if err := CheckPrivate(filepath.Join(base, "loop1", "state")); err == nil {
		t.Fatal("a loop of links was accepted")
	}
}

func TestCheckPrivateModesOfTheHolderAndTheState(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode os.FileMode
		ok   bool
	}{
		{0o700, true}, {0o750, true},
		{0o711, true}, // others may only look a name up; the state is 0600 and nothing else is kept here
		{0o770, false}, {0o707, false}, {0o777, false}, {0o722, false},
	} {
		d := filepath.Join(base, "d"+c.mode.String())
		mkdir(t, d, c.mode)
		err := CheckPrivate(filepath.Join(d, "state"))
		if (err == nil) != c.ok {
			t.Errorf("mode %04o: %v, want ok=%v", c.mode, err, c.ok)
		}
	}
	// A state of another owner, and one that others can write.
	d := filepath.Join(base, "files")
	mkdir(t, d, 0o700)
	st := filepath.Join(d, "state")
	if err := os.WriteFile(st, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(st); err != nil {
		t.Fatal(err)
	}
	fakeOwner(t, map[string]int{st: selfUID() + 1})
	if err := CheckPrivate(st); err == nil || !strings.Contains(err.Error(), "the file") {
		t.Fatalf("a state of another owner: %v", err)
	}
}

// L10-12: the lock file is the auditor's. A lock file with a loose mode or
// another owner is replaced by one with mode 0600, so that whoever kept the old
// one open (or holds its lock for ever) no longer blocks the auditor.
func TestLockFileIsPrivateAndReplaced(t *testing.T) {
	d := t.TempDir()
	state := filepath.Join(d, "state")
	lock := lockName(state)
	// Mode 0666: held by somebody with a descriptor of the old file.
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lock, 0o666); err != nil {
		t.Fatal(err)
	}
	old, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := syscall.Flock(int(old.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(state, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("a lock file with mode 0666 held by another process blocks the auditor: %v", err)
	}
	l.Release()
	st, err := os.Stat(lock)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the lock file was not made again with mode 0600: %v %v", st, err)
	}
	// Another owner: replaced the first time, kept after.
	n := 0
	prev := ownerOf
	t.Cleanup(func() { ownerOf = prev })
	ownerOf = func(p string, st fs.FileInfo) (int, bool) {
		if p == lock {
			if n++; n == 1 {
				return selfUID() + 1, true
			}
		}
		return prev(p, st)
	}
	l, err = Acquire(state, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("a lock file of another owner: %v", err)
	}
	l.Release()
	if n < 2 {
		t.Fatalf("the owner was looked at %d time(s), want a second look at the new file", n)
	}
}

// The path is looked at again after the lock is taken, and refused when the name
// leads elsewhere.
func TestAcquireCheckedLooksAgain(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	mkdir(t, a, 0o700)
	mkdir(t, b, 0o700)
	link := filepath.Join(base, "link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	typed := filepath.Join(link, "state")
	resolved, err := Resolve(typed)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := AcquireChecked(typed, resolved, time.Second, true); err != nil {
		t.Fatal(err)
	} else {
		l.Release()
	}
	// The link is moved to the other directory after the first resolution.
	_ = os.Remove(link)
	if err := os.Symlink(b, link); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireChecked(typed, resolved, time.Second, true); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a name that leads elsewhere after the lock: %v", err)
	}
	// And the private check is made again: the directory is opened up.
	if err := os.Chmod(b, 0o777); err != nil {
		t.Fatal(err)
	}
	resolved, _ = Resolve(typed)
	if _, err := AcquireChecked(typed, resolved, time.Second, true); err == nil {
		t.Fatal("a directory that others can write was accepted after the lock")
	}
	if l, err := AcquireChecked(typed, resolved, time.Second, false); err != nil {
		t.Fatalf("--allow-shared-state-dir: %v", err)
	} else {
		l.Release()
	}
}
