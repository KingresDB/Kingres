// SPDX-License-Identifier: AGPL-3.0-only

package statefile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// ---- L18-01: the file the run was told to use is never a leftover ----

func TestRemoveLeftoversNeverRemovesTheFileTheRunWasToldToUse(t *testing.T) {
	dir := testdir.New(t)
	named := []string{
		".kingres-state-0123456789ab-7.tmp",
		".kingres-evidence-0123456789ab-8.tmp",
	}
	for _, n := range named {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("held"), 0o600); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(dir, ".kingres-state-ffffffffffff-1.tmp")
		if err := os.WriteFile(other, []byte("crash"), 0o600); err != nil {
			t.Fatal(err)
		}
		RemoveLeftovers(p)
		if got, err := os.ReadFile(p); err != nil || string(got) != "held" {
			t.Errorf("%s was removed by the cleanup of a run that was told to use it: %v", n, err)
		}
		if _, err := os.Lstat(other); err == nil {
			t.Errorf("a real leftover was kept beside %s", n)
		}
		_ = os.Remove(p)
	}
}

func TestCheckNameRefusesTheNamesOfTheTemporaryFiles(t *testing.T) {
	for _, n := range []string{".kingres-state-0123456789ab-7.tmp", ".kingres-evidence-abcdef012345-123.tmp"} {
		err := CheckName(filepath.Join("some", "dir", n))
		if err == nil || !strings.Contains(err.Error(), "another name") || !strings.Contains(err.Error(), n) {
			t.Errorf("%s: %v", n, err)
		}
	}
	for _, n := range []string{"state", ".kingres-state.lock", ".kingres-state-0123456789ab-7.tmp.bak", ".kingres-state-0123456789AB-7.tmp", "notes.tmp"} {
		if err := CheckName(filepath.Join("some", "dir", n)); err != nil {
			t.Errorf("%s was refused: %v", n, err)
		}
	}
}

// ---- L18-07: the file system that cannot link flushes the directory too ----

func TestEvidenceCreatedInPlaceIsFlushedAndAFailedFlushIsReported(t *testing.T) {
	oldOps, oldLink := ops, linkFn
	defer func() { ops, linkFn = oldOps, oldLink }()
	linkFn = func(string, string) error { return errors.New("link: operation not supported") }

	// the order: the directory is flushed after the file is written
	var log []string
	ops = recordOps{&log}
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	root := bytes.Repeat([]byte{4}, 32)
	if _, err := SaveConflict(p, 3, root, []byte("evidence\n")); err != nil {
		t.Fatal(err)
	}
	if n := len(log); n == 0 || log[n-1] != "syncdir" {
		t.Fatalf("the directory was not flushed after the file was created in place: %v", log)
	}

	// a failed flush is reported as the other paths do, with the file in place
	ops = syncDirFails{osOps{}}
	root2 := bytes.Repeat([]byte{5}, 32)
	sv, err := SaveConflict(p, 3, root2, []byte("evidence two\n"))
	var dse *DirSyncError
	if !errors.As(err, &dse) {
		t.Fatalf("the error is %v", err)
	}
	if sv.Path != ConflictPath(p, 3, root2) || readState(t, sv.Path) != "evidence two\n" {
		t.Errorf("the file is not in place: %+v", sv)
	}
	if left := tmpFiles(t, dir); len(left) != 0 {
		t.Errorf("temporary files %v", left)
	}
}
