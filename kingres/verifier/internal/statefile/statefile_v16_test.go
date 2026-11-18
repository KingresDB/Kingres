// SPDX-License-Identifier: AGPL-3.0-only

package statefile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// ---- L16-01: the evidence of a weak state is two files at most ----

func sameText(have []byte) ([]byte, bool) { return have, true }

func TestWeakConflictKeepsOneFileForEachKindOfConflict(t *testing.T) {
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	held := bytes.Repeat([]byte{6}, 32)
	// merge: a file is "the same checkpoint" when it holds the same bytes
	same := func(note string) func([]byte) ([]byte, bool) {
		return func(have []byte) ([]byte, bool) { return have, string(have) == note }
	}
	roll, err := SaveWeakConflict(p, WeakRollback, 700, held, []byte("old genuine\n"), same("old genuine\n"))
	if err != nil || roll.Existing || roll.Path != WeakConflictPath(p, WeakRollback, 700, held) {
		t.Fatalf("%+v %v", roll, err)
	}
	// a replay of the same old checkpoint is the one already saved
	if again, err := SaveWeakConflict(p, WeakRollback, 700, held, []byte("old genuine\n"), same("old genuine\n")); err != nil || !again.Existing || again.Path != roll.Path {
		t.Fatalf("%+v %v", again, err)
	}
	// the fork has a file of its own: the replay did not take its place
	fork, err := SaveWeakConflict(p, WeakFork, 700, held, []byte("the fork\n"), same("the fork\n"))
	if err != nil || fork.Existing || fork.Path == roll.Path {
		t.Fatalf("%+v %v", fork, err)
	}
	// another checkpoint of a kind that has its file is not saved, and the error says why
	for _, n := range []string{"another fork\n", "a third\n"} {
		sv, err := SaveWeakConflict(p, WeakFork, 700, held, []byte(n), same(n+"x"))
		if !errors.Is(err, ErrSlotTaken) || sv.Existing || sv.Path != fork.Path {
			t.Fatalf("%q: %+v %v", n, sv, err)
		}
	}
	if readState(t, fork.Path) != "the fork\n" || readState(t, roll.Path) != "old genuine\n" {
		t.Fatal("a file was rewritten")
	}
	// a file that someone put at a name is not the evidence, and is not overwritten
	if err := os.WriteFile(roll.Path, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveWeakConflict(p, WeakRollback, 700, held, []byte("old genuine\n"), same("old genuine\n")); !errors.Is(err, ErrSlotTaken) {
		t.Fatalf("a planted file counted as the evidence: %v", err)
	}
	if readState(t, roll.Path) != "planted" {
		t.Fatal("the planted file was overwritten")
	}
	// another held state, other files; an unknown kind is refused
	if c, err := SaveWeakConflict(p, WeakFork, 800, held, []byte("x\n"), sameText); err != nil || c.Path == fork.Path {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := SaveWeakConflict(p, "other", 800, held, []byte("x\n"), sameText); err == nil {
		t.Fatal("a third kind of file was made")
	}
	// the bound: whatever is served, the directory holds the two files of this state
	// and the files of the other state
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) != 3 {
		t.Fatalf("files: %v", names)
	}
}

// ---- L16-04: leftovers of every name of the program ----

func plant(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRemoveLeftoversRemovesEveryTemporaryFileOfTheProgramAndNothingElse(t *testing.T) {
	dir := testdir.New(t)
	state := filepath.Join(dir, "state")
	gone := []string{
		plant(t, dir, ".kingres-state-0123456789ab-1.tmp"),
		plant(t, dir, ".kingres-state-ffffffffffff-4294967295.tmp"),
		plant(t, dir, ".kingres-evidence-0123456789ab-22.tmp"),
		plant(t, dir, ".kingres-evidence-abcdefabcdef-0.tmp"),
	}
	kept := []string{
		plant(t, dir, ".kingres-state-0123456789ab-.tmp"),
		plant(t, dir, ".kingres-state-0123456789ab-1x.tmp"),
		plant(t, dir, ".kingres-state-0123456789ab1-1.tmp"),
		plant(t, dir, ".kingres-state-0123456789a-1.tmp"),
		plant(t, dir, ".kingres-state-0123456789AB-2.tmp"),
		plant(t, dir, ".kingres-state-0123456789ab-1.tmp.old"),
		plant(t, dir, ".kingres-other-0123456789ab-1.tmp"),
		plant(t, dir, "kingres-state-0123456789ab-1.tmp"),
		plant(t, dir, ".kingres-state.lock"),
		plant(t, dir, "state"),
		plant(t, dir, "state.conflict-7-00"),
		plant(t, dir, "x.tmp"),
	}
	// a directory with such a name, with a file inside, is left alone
	d := filepath.Join(dir, ".kingres-state-0123456789ab-999.tmp")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, d, plant(t, d, "inner"))
	// an empty one too, which a plain removal would take
	empty := filepath.Join(dir, ".kingres-state-0123456789ab-997.tmp")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, empty)
	// and so is a link with such a name
	l := filepath.Join(dir, ".kingres-evidence-0123456789ab-998.tmp")
	if err := os.Symlink(kept[len(kept)-1], l); err == nil {
		kept = append(kept, l)
	}
	RemoveLeftovers(state)
	for _, p := range gone {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s is a temporary file of the program and is still there", filepath.Base(p))
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s is not a temporary file of the program and was removed", filepath.Base(p))
		}
	}
}

// What belongs to another user is left alone where the system says who owns it.
func TestRemoveLeftoversLeavesAFileOfAnotherUser(t *testing.T) {
	dir := testdir.New(t)
	foreign := plant(t, dir, ".kingres-state-0123456789ab-1.tmp")
	ours := plant(t, dir, ".kingres-state-0123456789ab-2.tmp")
	prev := ownerOf
	defer func() { ownerOf = prev }()
	ownerOf = func(p string, st fs.FileInfo) (int, bool) {
		if p == foreign {
			return selfUID() + 1, true
		}
		return selfUID(), true
	}
	RemoveLeftovers(filepath.Join(dir, "state"))
	if _, err := os.Lstat(foreign); err != nil {
		t.Error("a file of another user was removed")
	}
	if _, err := os.Lstat(ours); err == nil {
		t.Error("a file of ours was left")
	}
}

// The case of the review: a crash in the extension of an evidence file leaves a
// temporary file that is named after the evidence file, and a run of the state
// removes it.
func TestCrashInTheExtensionOfAnEvidenceFileLeavesAFileTheNextRunRemoves(t *testing.T) {
	defer func() { step = func(string) error { return nil } }()
	dir := testdir.New(t)
	state := filepath.Join(dir, "state")
	root := bytes.Repeat([]byte{9}, 32)
	name := ConflictPath(state, 5, root)
	if _, err := SaveConflict(state, 5, root, []byte("note\n")); err != nil {
		t.Fatal(err)
	}
	step = func(s string) error {
		if s == "written" {
			return errStop
		}
		return nil
	}
	merge := func(have []byte) ([]byte, bool) { return append(append([]byte(nil), have...), "more\n"...), true }
	if _, err := SaveConflictMerging(state, 5, root, []byte("note\n"), merge); !errors.Is(err, errStop) {
		t.Fatalf("%v", err)
	}
	left := tmpFiles(t, dir)
	if len(left) != 1 || !strings.HasPrefix(left[0], ".kingres-state-"+tag(name)+"-") {
		t.Fatalf("the crash leaves %v, want one file named after the evidence file", left)
	}
	step = func(string) error { return nil }
	RemoveLeftovers(state)
	if left := tmpFiles(t, dir); len(left) != 0 {
		t.Fatalf("still there: %v", left)
	}
}

// ---- L16-05: a failed flush of the directory after the file is in place ----

type syncDirFails struct{ fileOps }

func (s syncDirFails) SyncDir(dir string) error {
	_ = s.fileOps.SyncDir(dir)
	return errors.New("injected flush failure")
}

func TestFailedFlushOfTheDirectoryAfterTheRenameIsNotAFailedWrite(t *testing.T) {
	old := ops
	ops = syncDirFails{osOps{}}
	defer func() { ops = old }()
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	if err := os.WriteFile(p, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ReplaceIfUnchanged(p, []byte("v2"), []byte("v1"), true, 1<<10)
	var dse *DirSyncError
	if !errors.As(err, &dse) || !strings.Contains(err.Error(), "directory could not be flushed") || !strings.Contains(err.Error(), "power loss") {
		t.Fatalf("the error is %v", err)
	}
	if readState(t, p) != "v2" {
		t.Errorf("the state is %q: the rename had happened", readState(t, p))
	}
	if left := tmpFiles(t, dir); len(left) != 0 {
		t.Errorf("temporary files %v", left)
	}
}

func TestFailedFlushOfTheDirectoryAfterTheLinkIsNotAFailedSave(t *testing.T) {
	old := ops
	ops = syncDirFails{osOps{}}
	defer func() { ops = old }()
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	root := bytes.Repeat([]byte{4}, 32)
	sv, err := SaveConflict(p, 3, root, []byte("evidence\n"))
	var dse *DirSyncError
	if !errors.As(err, &dse) {
		t.Fatalf("the error is %v", err)
	}
	if sv.Path != ConflictPath(p, 3, root) || readState(t, sv.Path) != "evidence\n" {
		t.Errorf("the file is not in place: %+v", sv)
	}
	if left := tmpFiles(t, dir); len(left) != 0 {
		t.Errorf("temporary files %v", left)
	}
	// the extension of a file: it is in place too
	merge := func(have []byte) ([]byte, bool) { return append(append([]byte(nil), have...), "more\n"...), true }
	sv, err = SaveConflictMerging(p, 3, root, []byte("evidence\n"), merge)
	if !errors.As(err, &dse) || !sv.Existing || !sv.Extended || readState(t, sv.Path) != "evidence\nmore\n" {
		t.Errorf("the extension: %+v %v", sv, err)
	}
}

// ---- L16-07: the evidence file is flushed before it is linked ----

type linkRecorder struct {
	log *[]string
}

func recordLink(log *[]string) func(string, string) error {
	return func(from, to string) error {
		*log = append(*log, "link")
		return os.Link(from, to)
	}
}

func TestEvidenceTemporaryFileIsFlushedBeforeItIsLinked(t *testing.T) {
	var log []string
	old, oldLink := ops, linkFn
	ops = recordOps{&log}
	linkFn = recordLink(&log)
	defer func() { ops, linkFn = old, oldLink }()
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	root := bytes.Repeat([]byte{4}, 32)
	if _, err := SaveConflict(p, 3, root, []byte("evidence\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(log, " "), "create write sync close link syncdir"; got != want {
		t.Fatalf("the order is\n%s\nwant\n%s", got, want)
	}
	// the weak files are written the same way
	log = nil
	if _, err := SaveWeakConflict(p, WeakFork, 3, root, []byte("evidence\n"), sameText); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(log, " "), "create write sync close link syncdir"; got != want {
		t.Fatalf("weak: the order is\n%s\nwant\n%s", got, want)
	}
}

// failSync is a temporary file whose flush fails.
type failSyncOps struct{ fileOps }

type failSyncFile struct{ tempFile }

func (failSyncFile) Sync() error { return errors.New("injected flush failure") }

func (o failSyncOps) CreateTemp(dir, pattern string) (tempFile, error) {
	f, err := o.fileOps.CreateTemp(dir, pattern)
	return failSyncFile{f}, err
}

func TestEvidenceIsNotLinkedWhenItCannotBeFlushed(t *testing.T) {
	old, oldLink := ops, linkFn
	ops = failSyncOps{osOps{}}
	var log []string
	linkFn = recordLink(&log)
	defer func() { ops, linkFn = old, oldLink }()
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	root := bytes.Repeat([]byte{4}, 32)
	if _, err := SaveConflict(p, 3, root, []byte("evidence\n")); err == nil || !strings.Contains(err.Error(), "injected flush failure") {
		t.Fatalf("the error is %v", err)
	}
	if len(log) != 0 {
		t.Errorf("the file was linked although its flush failed: %v", log)
	}
	if _, err := os.Lstat(ConflictPath(p, 3, root)); err == nil {
		t.Error("a file is at the name of the evidence although its flush failed")
	}
	if left := tmpFiles(t, dir); len(left) != 0 {
		t.Errorf("temporary files %v", left)
	}
}
