// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ledgerstate_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerstate"
)

// The tests of the fixes of the review of the anchoring fixes (P7R-11).

func acceptSetup(t *testing.T) (dir string, f *ledgerstate.File, db ledgerstate.State) {
	t.Helper()
	dir = t.TempDir()
	f, err := ledgerstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.RecordCheckpoint("o", 9, rootA)
	db = ledgerstate.State{Logs: map[string]ledgerstate.Mark{"o": {Size: 4, Root: "04"}}}
	return dir, f, db
}

// The copy of the record that an acceptance replaces must be written before anything changes: when it cannot be, the record on disk and
// in memory stay as they were, and the answer says so (M23).
func TestAnAcceptanceThatCannotKeepTheOldRecordChangesNothing(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory that the user cannot write to")
	}
	dir, f, db := acceptSetup(t)
	before, err := os.ReadFile(filepath.Join(dir, ledgerstate.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	err = f.Accept(db, ledgerstate.RestoreNote{Reason: "r", Actor: "a"})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("Accept: %v", err)
	}
	if s := f.Snapshot(); s.Generation != 0 || s.Restore != nil || s.Logs["o"].Size != 9 {
		t.Errorf("the record in memory changed although the copy could not be kept: %+v", s)
	}
	_ = os.Chmod(dir, 0o700)
	after, _ := os.ReadFile(filepath.Join(dir, ledgerstate.FileName))
	if string(after) != string(before) {
		t.Error("the record on disk changed")
	}
}

// The copy is the exact bytes of the replaced record, mode 0600, with no temporary file left behind (it is written whole and renamed).
func TestTheCopyOfTheReplacedRecordIsTheExactRecordAndLeavesNoTemporaryFile(t *testing.T) {
	dir, f, db := acceptSetup(t)
	before, _ := os.ReadFile(filepath.Join(dir, ledgerstate.FileName))
	if err := f.Accept(db, ledgerstate.RestoreNote{Reason: "r", Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, ledgerstate.FileName+".before-restore-1")
	got, err := os.ReadFile(keep)
	if err != nil || string(got) != string(before) {
		t.Fatalf("the copy: %v, %d bytes (want %d)", err, len(got), len(before))
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(keep); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", st.Mode().Perm())
		}
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".before-restore-") || strings.HasPrefix(e.Name(), ".published-") {
			t.Errorf("a temporary file is left: %s", e.Name())
		}
	}
}

// A copy of an earlier acceptance is never written over (a record that was lost and made again from the database starts at generation 0
// and would reach the same generation number); the new copy gets a suffix.
func TestAnEarlierCopyOfAReplacedRecordIsNotWrittenOver(t *testing.T) {
	dir, f, db := acceptSetup(t)
	old := filepath.Join(dir, ledgerstate.FileName+".before-restore-1")
	if err := os.WriteFile(old, []byte("THE COPY OF AN EARLIER ACCEPTANCE"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, ledgerstate.FileName))
	if err := f.Accept(db, ledgerstate.RestoreNote{Reason: "r", Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(old); string(got) != "THE COPY OF AN EARLIER ACCEPTANCE" {
		t.Errorf("the earlier copy was written over: %q", got)
	}
	if got, _ := os.ReadFile(old + ".1"); string(got) != string(before) {
		t.Errorf("the new copy is not the replaced record: %q", got)
	}
}
