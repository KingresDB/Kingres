// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package statefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// recordOps wraps the real file operations and records the order in which
// Replace uses them, so that the two flushes that make a state survive a power
// loss are tested by where they stand and not by their effect (which no test
// can see).
type recordOps struct {
	log *[]string
}

type recordFile struct {
	tempFile
	log *[]string
}

func (r recordOps) CreateTemp(dir, pattern string) (tempFile, error) {
	f, err := osOps{}.CreateTemp(dir, pattern)
	*r.log = append(*r.log, "create")
	return recordFile{f, r.log}, err
}

func (r recordOps) Rename(from, to string) error {
	*r.log = append(*r.log, "rename")
	return osOps{}.Rename(from, to)
}

func (r recordOps) SyncDir(dir string) error {
	*r.log = append(*r.log, "syncdir")
	return osOps{}.SyncDir(dir)
}

func (f recordFile) Write(b []byte) (int, error) {
	*f.log = append(*f.log, "write")
	return f.tempFile.Write(b)
}

func (f recordFile) Sync() error {
	*f.log = append(*f.log, "sync")
	return f.tempFile.Sync()
}

func (f recordFile) Close() error {
	*f.log = append(*f.log, "close")
	return f.tempFile.Close()
}

// L9-11: the data is flushed before the rename, and the directory after it. A
// version without the first can leave an empty state after a crash; without the
// second, the old state comes back.
func TestStateFileReplaceFlushesInTheOrderThatSurvivesAPowerLoss(t *testing.T) {
	var log []string
	old := ops
	ops = recordOps{&log}
	defer func() { ops = old }()
	p := filepath.Join(testdir.New(t), "state")
	if err := Replace(p, []byte("data\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(log, " "), "create write sync close rename syncdir"; got != want {
		t.Fatalf("the order of the operations is %q, want %q", got, want)
	}
	if readState(t, p) != "data\n" {
		t.Error("the state is not the data")
	}
}

func TestStateFileNewStateIsPrivateAndAnOldModeIsKept(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not permissions on Windows")
	}
	dir := testdir.New(t)
	fresh := filepath.Join(dir, "fresh")
	if err := Replace(fresh, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(fresh); st.Mode().Perm() != 0o600 {
		t.Errorf("a new state has mode %v, want 0600", st.Mode().Perm())
	}
	kept := filepath.Join(dir, "kept")
	if err := os.WriteFile(kept, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(kept, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Replace(kept, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(kept); st.Mode().Perm() != 0o640 {
		t.Errorf("a replaced state has mode %v, want the 0640 it had", st.Mode().Perm())
	}
}

// L9-07: the evidence of a fork is never claimed saved when it is not.
func TestStateFileConflictEvidenceIsSavedOnlyWhenItIsThere(t *testing.T) {
	p := filepath.Join(testdir.New(t), "state")
	root := make([]byte, 32)
	for i := range root {
		root[i] = byte(i + 1)
	}
	a, err := SaveConflict(p, 7, root, []byte("first"))
	if err != nil || a.Existing {
		t.Fatalf("first save: %+v %v", a, err)
	}
	if !strings.Contains(a.Path, fmt.Sprintf("%x", root)) {
		t.Errorf("the name %q does not carry the whole root", a.Path)
	}
	// The same evidence again: already there.
	b, err := SaveConflict(p, 7, root, []byte("first"))
	if err != nil || !b.Existing || b.Path != a.Path {
		t.Fatalf("same content: %+v %v", b, err)
	}
	// Other content under the same name never overwrites and is not "saved" at
	// that name: it goes to the next name, and the first file is as it was.
	c, err := SaveConflict(p, 7, root, []byte("second"))
	if err != nil || c.Existing || c.Path == a.Path {
		t.Fatalf("other content: %+v %v", c, err)
	}
	if readState(t, a.Path) != "first" || readState(t, c.Path) != "second" {
		t.Error("evidence was overwritten or not written")
	}
	// A file that someone planted at the name is not the evidence.
	if err := os.WriteFile(a.Path, []byte("PLANTED"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := SaveConflict(p, 7, root, []byte("first"))
	if err != nil || d.Existing || d.Path == a.Path || d.Path == c.Path {
		t.Fatalf("a planted file counted as the evidence: %+v %v", d, err)
	}
	if readState(t, a.Path) != "PLANTED" {
		t.Error("the planted file was overwritten")
	}
	// A long name for the state still gets a name that fits.
	long := filepath.Join(testdir.New(t), strings.Repeat("n", 240))
	if e, err := SaveConflict(long, 7, root, []byte("x")); err != nil || len(filepath.Base(e.Path)) > 255 {
		t.Fatalf("long state name: %+v %v", e, err)
	}
}

func TestStateFileConflictGivesUpAfterTheNamesAreTaken(t *testing.T) {
	p := filepath.Join(testdir.New(t), "state")
	root := make([]byte, 32)
	for i := 0; i < maxConflictNames; i++ {
		if _, err := SaveConflict(p, 1, root, []byte(fmt.Sprint("note ", i))); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if _, err := SaveConflict(p, 1, root, []byte("one too many")); !errors.Is(err, ErrConflictNames) {
		t.Fatalf("after the names were taken: %v", err)
	}
	// ...but evidence that is already there is still recognised.
	if s, err := SaveConflict(p, 1, root, []byte("note 3")); err != nil || !s.Existing {
		t.Fatalf("known evidence: %+v %v", s, err)
	}
}

func TestStateFileConflictThroughALinkIsNotFollowed(t *testing.T) {
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	root := make([]byte, 32)
	target := filepath.Join(testdir.New(t), "elsewhere")
	if err := os.Symlink(target, ConflictPath(p, 3, root)); err != nil {
		t.Skipf("no link here: %v", err)
	}
	s, err := SaveConflict(p, 3, root, []byte("evidence"))
	if err != nil || s.Existing {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("the link was followed and its target created")
	}
	if readState(t, s.Path) != "evidence" {
		t.Error("the evidence is not in the file named")
	}
}

// L9-08: the lock is not opened through a link.
func TestStateFileLockIsNotOpenedThroughALink(t *testing.T) {
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	target := filepath.Join(testdir.New(t), "created-by-the-lock")
	if err := os.Symlink(target, lockName(p)); err != nil {
		t.Skipf("no link here: %v", err)
	}
	l, err := Acquire(p, time.Second)
	if err == nil {
		l.Release()
		t.Fatal("the lock was taken through a link")
	}
	if !errors.Is(err, safefile.ErrLink) {
		t.Errorf("error %v, want a link refusal", err)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("a file was created where the link points")
	}
}

func TestStateFileLockIsNotOpenedOnADirectory(t *testing.T) {
	p := filepath.Join(testdir.New(t), "state")
	if err := os.Mkdir(lockName(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if l, err := Acquire(p, time.Second); err == nil {
		l.Release()
		t.Fatal("the lock was taken on a directory")
	}
}

// L9-12: two names of one file in one directory take one lock.
func TestStateFileTwoNamesInOneDirectoryTakeOneLock(t *testing.T) {
	dir := testdir.New(t)
	a, b := filepath.Join(dir, "state"), filepath.Join(dir, "other-name")
	l, err := Acquire(a, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := Acquire(b, 100*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("a second name took a second lock: %v", err)
	}
}

func TestStateFileCheckPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		if err := CheckPrivate(filepath.Join(testdir.New(t), "state")); err != nil {
			t.Fatal(err)
		}
		t.Skip("permissions of a Windows folder are not read")
	}
	dir := testdir.New(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "state")
	if err := CheckPrivate(p); err != nil {
		t.Fatalf("a private directory: %v", err)
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(p); err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("a group writable directory: %v", err)
	}
	if err := os.Chmod(dir, 0o703); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(p); err == nil {
		t.Fatal("a directory others can write was accepted")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(p); err == nil || !strings.Contains(err.Error(), "the file") {
		t.Fatalf("a state file others can write: %v", err)
	}
}
