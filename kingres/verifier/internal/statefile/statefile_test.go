// SPDX-License-Identifier: AGPL-3.0-only

package statefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readState(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestCrashAtEveryStep stops the replace after each stage as a crash would
// (the temporary file stays) and checks that the state is then the old file
// or the new one, whole, and that the next replace works and cleans up.
func TestCrashAtEveryStep(t *testing.T) {
	defer func() { step = func(string) error { return nil } }()
	oldData, newData := strings.Repeat("old\n", 500), strings.Repeat("new\n", 700)
	for _, stop := range []string{"written", "synced", "renamed", "dirsynced"} {
		t.Run(stop, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "state")
			if err := os.WriteFile(p, []byte(oldData), 0o644); err != nil {
				t.Fatal(err)
			}
			step = func(s string) error {
				if s == stop {
					return errStop
				}
				return nil
			}
			if err := Replace(p, []byte(newData)); !errors.Is(err, errStop) {
				t.Fatalf("replace did not stop: %v", err)
			}
			got := readState(t, p)
			// Before the rename the old state is there; after it the new one.
			want := oldData
			if stop == "renamed" || stop == "dirsynced" {
				want = newData
			}
			if got != want {
				t.Fatalf("after a stop at %q the state is neither complete old nor complete new (%d bytes)", stop, len(got))
			}
			step = func(string) error { return nil }
			if err := Replace(p, []byte("third\n")); err != nil {
				t.Fatalf("the next run failed: %v", err)
			}
			if readState(t, p) != "third\n" {
				t.Fatal("the next run did not replace the state")
			}
			ents, _ := os.ReadDir(dir)
			if len(ents) != 1 {
				t.Errorf("leftovers after the next run: %d entries", len(ents))
			}
		})
	}
}

func TestReplaceFirstUseAndLongName(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, strings.Repeat("n", 240))
	if err := Replace(p, []byte("x\n")); err != nil {
		t.Fatalf("a 240 byte name: %v", err)
	}
	l, err := Acquire(p, time.Second)
	if err != nil {
		t.Fatalf("lock with a 240 byte name: %v", err)
	}
	l.Release()
}

func TestLockIsExclusiveAndBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state")
	l, err := Acquire(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := Acquire(p, 200*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("a second lock was granted or failed otherwise: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the wait was not bounded")
	}
	l.Release()
	l2, err := Acquire(p, time.Second)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	l2.Release()
}

func TestResolveFollowsLinkAndKeepsIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a link here: %v", err)
	}
	r, err := Resolve(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := Replace(r, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced by a file")
	}
	if readState(t, target) != "new" {
		t.Error("the target was not updated")
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err == nil {
		if _, err := Resolve(dangling); err == nil {
			t.Error("a dangling link was accepted")
		}
	}
}

func TestConflictNeverOverwrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state")
	root := []byte{1, 2, 3, 4, 5, 6}
	a, err := SaveConflict(p, 7, root, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SaveConflict(p, 7, root, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if readState(t, a) != "first" {
		t.Error("evidence was overwritten")
	}
}
