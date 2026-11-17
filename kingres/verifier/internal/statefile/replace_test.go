// SPDX-License-Identifier: AGPL-3.0-only

package statefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// L12-02: a write is made only over the state that the decision was made on. A
// late writer, which decided on an older state, finds another one and writes
// nothing.
func TestReplaceIfUnchangedWritesOnlyOverWhatWasRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state")
	if err := os.WriteFile(p, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	// the state is what was read: replaced
	if err := ReplaceIfUnchanged(p, []byte("v2"), []byte("v1"), true, 1<<10); err != nil {
		t.Fatalf("unchanged state: %v", err)
	}
	if readState(t, p) != "v2" {
		t.Fatal("not replaced")
	}
	// a rival moved it on: the late writer, which read v2, finds v3 and writes nothing
	if err := os.WriteFile(p, []byte("v3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceIfUnchanged(p, []byte("v-late"), []byte("v2"), true, 1<<10); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("a changed state: %v", err)
	}
	if readState(t, p) != "v3" {
		t.Fatal("the late writer overwrote the newer state")
	}
	// it was not there when read and is now
	if err := ReplaceIfUnchanged(p, []byte("x"), nil, false, 1<<10); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("a state that appeared: %v", err)
	}
	// it was there and is gone
	gone := filepath.Join(dir, "gone")
	if err := ReplaceIfUnchanged(gone, []byte("x"), []byte("old"), true, 1<<10); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("a state that vanished: %v", err)
	}
	// first use
	if err := ReplaceIfUnchanged(gone, []byte("first"), nil, false, 1<<10); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if readState(t, gone) != "first" {
		t.Fatal("first use did not write")
	}
}
