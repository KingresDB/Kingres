// SPDX-License-Identifier: AGPL-3.0-only

package statefile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// These tests came from a mutation run of the package. They pin what a failed
// replace leaves behind, which temporary files a replace removes, the exact
// names and the content test of the evidence, and the wait of the lock.

// faultOps fails one operation of Replace, and records the others.
type faultOps struct {
	failAt string
	log    *[]string
}

type faultFile struct {
	tempFile
	o faultOps
}

func (o faultOps) CreateTemp(dir, pattern string) (tempFile, error) {
	f, err := osOps{}.CreateTemp(dir, pattern)
	return faultFile{f, o}, err
}

func (o faultOps) Rename(from, to string) error {
	if o.failAt == "rename" {
		return errors.New("rename failed")
	}
	return osOps{}.Rename(from, to)
}

func (o faultOps) SyncDir(dir string) error {
	if o.failAt == "syncdir" {
		return errors.New("syncdir failed")
	}
	return osOps{}.SyncDir(dir)
}

func (f faultFile) Write(b []byte) (int, error) {
	if f.o.failAt == "write" {
		return 0, errors.New("write failed")
	}
	return f.tempFile.Write(b)
}

func (f faultFile) Sync() error {
	if f.o.failAt == "sync" {
		return errors.New("sync failed")
	}
	return f.tempFile.Sync()
}

func (f faultFile) Close() error {
	*f.o.log = append(*f.o.log, "close")
	if f.o.failAt == "close" {
		_ = f.tempFile.Close()
		return errors.New("close failed")
	}
	return f.tempFile.Close()
}

func tmpFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestReplaceThatFailsLeavesTheOldStateAndNoTemporaryFile(t *testing.T) {
	for _, at := range []string{"write", "sync", "close", "rename"} {
		t.Run(at, func(t *testing.T) {
			dir := testdir.New(t)
			p := filepath.Join(dir, "state")
			if err := os.WriteFile(p, []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var log []string
			old := ops
			ops = faultOps{failAt: at, log: &log}
			defer func() { ops = old }()
			if err := Replace(p, []byte("new\n")); err == nil || !strings.Contains(err.Error(), at+" failed") {
				t.Fatalf("Replace: %v", err)
			}
			if got := readState(t, p); got != "old\n" {
				t.Errorf("the state is %q", got)
			}
			if left := tmpFiles(t, dir); len(left) != 0 {
				t.Errorf("temporary files left: %v", left)
			}
			if at == "write" || at == "sync" {
				if len(log) == 0 || log[len(log)-1] != "close" {
					t.Errorf("the temporary file was not closed after a failed %s: %v", at, log)
				}
			}
		})
	}
	// a failed flush of the directory comes after the rename: the new state is there, and the error is reported
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	var log []string
	old := ops
	ops = faultOps{failAt: "syncdir", log: &log}
	defer func() { ops = old }()
	if err := Replace(p, []byte("new\n")); err == nil || !strings.Contains(err.Error(), "syncdir failed") {
		t.Fatalf("Replace: %v", err)
	}
	if readState(t, p) != "new\n" || len(tmpFiles(t, dir)) != 0 {
		t.Error("after a failed flush of the directory")
	}
}

// A stop that stands for a crash leaves the temporary file, as a crash does; any
// other error removes it.
func TestReplaceStoppedLikeACrashKeepsTheTemporaryFile(t *testing.T) {
	defer func() { step = func(string) error { return nil } }()
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	step = func(s string) error {
		if s == "written" {
			return errStop
		}
		return nil
	}
	if err := Replace(p, []byte("new\n")); !errors.Is(err, errStop) {
		t.Fatal(err)
	}
	if len(tmpFiles(t, dir)) != 1 {
		t.Fatalf("a crash leaves one temporary file: %v", tmpFiles(t, dir))
	}
	step = func(s string) error {
		if s == "synced" {
			return errors.New("not a crash")
		}
		return nil
	}
	if err := Replace(p, []byte("new\n")); err == nil {
		t.Fatal("no error")
	}
	if len(tmpFiles(t, dir)) != 0 {
		t.Fatalf("another error leaves none (the leftover of the crash is removed first): %v", tmpFiles(t, dir))
	}
}

// The next replace removes the temporary files that this program makes, whatever
// the name they were made under (L16-04), and nothing else.
func TestReplaceRemovesOnlyItsOwnLeftovers(t *testing.T) {
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	pre := tmpPrefix(p)
	other := tmpPrefix(filepath.Join(dir, "another"))
	files := map[string]bool{
		pre + "123.tmp":                       false, // a leftover of this state: removed
		pre + "456.tmp":                       false,
		pre + "123.txt":                       true,  // not a temporary file
		pre + ".tmp":                          true,  // no number: the program never makes this name
		other + "123.tmp":                     false, // the leftover of an evidence file: removed too
		"state.tmp":                           true,
		strings.TrimSuffix(pre, "-") + ".tmp": true,
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Replace(p, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	for name, kept := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != kept {
			t.Errorf("%s: kept=%v, want %v", name, err == nil, kept)
		}
	}
}

func TestDefaultLockWaitIsTenSeconds(t *testing.T) {
	if LockWait != 10*time.Second {
		t.Fatalf("%v", LockWait)
	}
}

func TestConflictNamesAndTheContentTest(t *testing.T) {
	dir := testdir.New(t)
	state := filepath.Join(dir, "state")
	root := bytes.Repeat([]byte{7}, 32)
	note := []byte("the note\n")
	base := ConflictPath(state, 5, root)
	// the first names are base, base-2, base-3 ...
	for i := 1; i <= 8; i++ {
		want := base
		if i > 1 {
			want = fmt.Sprintf("%s-%d", base, i)
		}
		sv, err := SaveConflict(state, 5, root, append([]byte(fmt.Sprintf("other %d\n", i)), note...))
		if err != nil || sv.Path != want || sv.Existing {
			t.Fatalf("name %d: %+v %v, want %s", i, sv, err, want)
		}
		if st, err := os.Stat(want); err != nil || (st.Mode().Perm() != 0o600 && os.PathSeparator == '/') {
			t.Fatalf("%s: %v %v", want, st, err)
		}
	}
	// all eight are taken by other content: an error that says the last one is not this note
	_, err := SaveConflict(state, 5, root, note)
	if !errors.Is(err, ErrConflictNames) || !strings.Contains(err.Error(), "is not this note") || !strings.Contains(err.Error(), "8") {
		t.Fatalf("every name taken: %v", err)
	}
	// the same note again is found at its name, not saved twice
	again, err := SaveConflict(state, 5, root, []byte("other 3\n"+string(note)))
	if err != nil || !again.Existing || again.Path != fmt.Sprintf("%s-%d", base, 3) {
		t.Fatalf("the same note: %+v %v", again, err)
	}
	// a file that starts with the note and has more in it is not the note: the content is compared whole
	dir2 := testdir.New(t)
	state2 := filepath.Join(dir2, "state")
	first := ConflictPath(state2, 5, root)
	if err := os.WriteFile(first, append(bytes.Clone(note), 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	sv, err := SaveConflict(state2, 5, root, note)
	if err != nil || sv.Existing || sv.Path != first+"-2" {
		t.Fatalf("a file with the note and more: %+v %v", sv, err)
	}
}

func TestConflictPathShortensLongNamesAtOneHundredBytes(t *testing.T) {
	root := make([]byte, 32)
	for _, c := range []struct {
		n    int
		long bool
	}{{100, false}, {101, true}} {
		p := ConflictPath(filepath.Join("d", strings.Repeat("n", c.n)), 1, root)
		short := strings.Contains(filepath.Base(p), "state-")
		if short != c.long {
			t.Errorf("a name of %d bytes: %s", c.n, filepath.Base(p))
		}
	}
}

// An unreachable lock is waited for, not given up at once.
func TestLockWaitsForItsDeadline(t *testing.T) {
	dir := testdir.New(t)
	state := filepath.Join(dir, "state")
	l, err := Acquire(state, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	start := time.Now()
	if _, err := Acquire(state, 300*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("%v", err)
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("gave up after %v, before the wait of 300 ms", el)
	}
}
