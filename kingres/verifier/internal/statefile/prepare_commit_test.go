// SPDX-License-Identifier: AGPL-3.0-only

package statefile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L14-04. ReplaceIfUnchanged writes and flushes the temporary file first and only
// then reads the state again, so that the time between that read and the rename
// is the rename and not the whole write. A writer that ignores the lock and
// writes while this one is still writing its temporary file is therefore seen.
func TestReplaceIfUnchangedLooksAfterTheTemporaryFileIsWritten(t *testing.T) {
	defer func() { step = func(string) error { return nil } }()
	dir := t.TempDir()
	p := filepath.Join(dir, "state")
	if err := os.WriteFile(p, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{"written", "synced"} {
		step = func(s string) error {
			if s == at { // a writer that ignores the lock, in the middle of the write
				return os.WriteFile(p, []byte("rival-"+at), 0o600) // not errStop: returns nil on success
			}
			return nil
		}
		err := ReplaceIfUnchanged(p, bytes.Repeat([]byte("new"), 1000), []byte("v1"), true, 1<<20)
		if !errors.Is(err, ErrStateChanged) {
			t.Fatalf("a rival that wrote at %q went unseen: %v", at, err)
		}
		if readState(t, p) != "rival-"+at {
			t.Fatalf("a rival that wrote at %q was overwritten", at)
		}
		if left := tmpFiles(t, dir); len(left) != 0 {
			t.Fatalf("the temporary file stays after a refusal: %v", left)
		}
		if err := os.WriteFile(p, []byte("v1"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The order of the file system calls: the read of the state is between the flush of
// the temporary file and the rename, and nothing but the rename and the flush of
// the directory follows it.
func TestReplaceIfUnchangedReadsBetweenTheFlushAndTheRename(t *testing.T) {
	defer func() { step = func(string) error { return nil } }()
	var log []string
	old := ops
	ops = recordOps{&log}
	defer func() { ops = old }()
	p := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(p, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	step = func(s string) error {
		log = append(log, "step:"+s)
		return nil
	}
	rival := func() { // what a rival that writes just before the rename does
		_ = os.WriteFile(p, []byte("late-rival"), 0o600)
	}
	ops = renameHook{recordOps{&log}, func() { log = append(log, "after-read"); rival() }}
	if err := ReplaceIfUnchanged(p, []byte("v2"), []byte("v1"), true, 1<<10); err != nil {
		t.Fatal(err)
	}
	want := "create write step:written sync close step:synced after-read rename step:renamed syncdir step:dirsynced"
	if got := strings.Join(log, " "); got != want {
		t.Fatalf("the order is\n%s\nwant\n%s", got, want)
	}
	// what is left of the window is exactly the time between the read and the
	// rename: a writer inside it is overwritten (and nothing closes that without a lock)
	if readState(t, p) != "v2" {
		t.Fatal("the rename did not happen")
	}
}

// renameHook runs a function just before the rename.
type renameHook struct {
	fileOps
	before func()
}

func (r renameHook) Rename(from, to string) error {
	r.before()
	return r.fileOps.Rename(from, to)
}

// L14-09. A new evidence file is written to a temporary name and linked into
// place: a stop between the create and the write leaves no file at the name.
func TestEvidenceFileIsNeverEmptyAtItsName(t *testing.T) {
	defer func() { step = func(string) error { return nil } }()
	dir := t.TempDir()
	p := filepath.Join(dir, "state")
	root := bytes.Repeat([]byte{3}, 32)
	name := ConflictPath(p, 9, root)
	for _, stop := range []string{"evidence-written"} {
		step = func(s string) error {
			if s == stop {
				return errStop
			}
			return nil
		}
		if _, err := SaveConflict(p, 9, root, []byte("the note\n")); !errors.Is(err, errStop) {
			t.Fatalf("stop at %s: %v", stop, err)
		}
		if _, err := os.Lstat(name); err == nil {
			t.Fatalf("a stop at %s left a file at the name of the evidence", stop)
		}
		if len(tmpFiles(t, dir)) != 1 {
			t.Fatalf("the stop leaves its temporary file, as a crash does: %v", tmpFiles(t, dir))
		}
	}
	// the next run removes the leftover and takes the first name
	step = func(string) error { return nil }
	sv, err := SaveConflict(p, 9, root, []byte("the note\n"))
	if err != nil || sv.Path != name || sv.Existing {
		t.Fatalf("the next run: %+v %v", sv, err)
	}
	if len(tmpFiles(t, dir)) != 0 || readState(t, name) != "the note\n" {
		t.Fatalf("leftovers %v", tmpFiles(t, dir))
	}
	// a stop after the link: the file is whole
	other := bytes.Repeat([]byte{4}, 32)
	step = func(s string) error {
		if s == "evidence-linked" {
			return errStop
		}
		return nil
	}
	if _, err := SaveConflict(p, 9, other, []byte("second\n")); !errors.Is(err, errStop) {
		t.Fatalf("%v", err)
	}
	if readState(t, ConflictPath(p, 9, other)) != "second\n" {
		t.Fatal("a file at the name that is not whole")
	}
}

// A file system that cannot link gets the older way, and the evidence is saved.
func TestEvidenceFileWhereTheFileSystemCannotLink(t *testing.T) {
	old := linkFn
	linkFn = func(string, string) error { return errors.New("link: operation not supported") }
	defer func() { linkFn = old }()
	dir := t.TempDir()
	p := filepath.Join(dir, "state")
	root := bytes.Repeat([]byte{5}, 32)
	sv, err := SaveConflict(p, 1, root, []byte("n\n"))
	if err != nil || readState(t, sv.Path) != "n\n" {
		t.Fatalf("%+v %v", sv, err)
	}
	if len(tmpFiles(t, dir)) != 0 {
		t.Fatalf("temporary files left: %v", tmpFiles(t, dir))
	}
	// the name that exists is still not overwritten
	if sv2, err := SaveConflict(p, 1, root, []byte("other\n")); err != nil || sv2.Path == sv.Path {
		t.Fatalf("%+v %v", sv2, err)
	}
}

// L14-02: one evidence file for a held state, whatever is served against it.
func TestWeakConflictKeepsOneFileForTheHeldState(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state")
	held := bytes.Repeat([]byte{6}, 32)
	any := func([]byte) bool { return true }
	a, err := SaveWeakConflict(p, 700, held, []byte("first\n"), any)
	if err != nil || a.Existing || a.Path != WeakConflictPath(p, 700, held) {
		t.Fatalf("%+v %v", a, err)
	}
	for _, n := range []string{"second\n", "third\n", "first\n"} {
		b, err := SaveWeakConflict(p, 700, held, []byte(n), any)
		if err != nil || !b.Existing || b.Path != a.Path {
			t.Fatalf("%q: %+v %v", n, b, err)
		}
	}
	if readState(t, a.Path) != "first\n" {
		t.Fatal("the file was rewritten")
	}
	// another held state, another file
	c, err := SaveWeakConflict(p, 800, held, []byte("x\n"), any)
	if err != nil || c.Path == a.Path || c.Existing {
		t.Fatalf("%+v %v", c, err)
	}
	// a file at the name that is not a note of the log is not evidence
	if err := os.WriteFile(c.Path, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveWeakConflict(p, 800, held, []byte("x\n"), func(b []byte) bool { return string(b) != "planted" }); err == nil {
		t.Fatal("a planted file counted as the evidence")
	}
	if readState(t, c.Path) != "planted" {
		t.Fatal("the planted file was overwritten")
	}
}
