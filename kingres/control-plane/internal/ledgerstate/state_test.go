// SPDX-License-Identifier: AGPL-3.0-only

package ledgerstate_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerstate"
)

var rootA, rootB = []byte{1, 2, 3}, []byte{4, 5, 6}

func TestTheRecordRefusesASmallerTreeAndAnotherRootForAPublishedSize(t *testing.T) {
	dir := t.TempDir()
	f, err := ledgerstate.Open(dir)
	if err != nil || f.Initialized() {
		t.Fatalf("%v %v", err, f.Initialized())
	}
	if err := f.RecordCheckpoint("o", 5, rootA); err != nil {
		t.Fatal(err)
	}
	if err := f.Allow("o", 4, rootA); !errors.Is(err, ledgerstate.ErrBehind) {
		t.Errorf("a smaller tree: %v", err)
	}
	if err := f.Allow("o", 5, rootB); !errors.Is(err, ledgerstate.ErrFork) {
		t.Errorf("another root: %v", err)
	}
	if err := f.Allow("o", 5, rootA); err != nil {
		t.Errorf("the same tree: %v", err)
	}
	if err := f.Allow("o", 6, rootB); err != nil || f.Allow("other", 1, rootB) != nil {
		t.Errorf("a larger tree and another log are free")
	}
	// it survives a restart, and a restore of the database does not touch it (it is a file of its own)
	g, _ := ledgerstate.Open(dir)
	if !g.Initialized() || g.Snapshot().Logs["o"].Size != 5 {
		t.Errorf("not kept: %+v", g.Snapshot())
	}
	if filepath.Separator == '/' {
		if st, _ := os.Stat(filepath.Join(dir, ledgerstate.FileName)); st.Mode().Perm()&0o077 != 0 {
			t.Errorf("mode %v", st.Mode())
		}
	}
}

func TestADamagedRecordRefusesEverythingAndIsNotTakenForAnEmptyOne(t *testing.T) {
	dir := t.TempDir()
	f, _ := ledgerstate.Open(dir)
	_ = f.RecordCheckpoint("o", 5, rootA)
	p := filepath.Join(dir, ledgerstate.FileName)
	b, _ := os.ReadFile(p)
	b[10] ^= 1
	_ = os.WriteFile(p, b, 0o600)
	g, _ := ledgerstate.Open(dir)
	if !g.Damaged() || g.Initialized() {
		t.Fatal("a changed file was accepted")
	}
	if err := g.Allow("o", 6, rootA); !errors.Is(err, ledgerstate.ErrDamaged) {
		t.Errorf("Allow: %v", err)
	}
	if err := g.RecordCheckpoint("o", 6, rootA); !errors.Is(err, ledgerstate.ErrDamaged) {
		t.Errorf("Record: %v", err)
	}
	// the operator's explicit act
	if err := g.Replace(ledgerstate.State{}); err != nil || g.Damaged() {
		t.Errorf("Replace: %v", err)
	}
}

func TestMergeIsMonotoneAndAForkIsRefused(t *testing.T) {
	a, _ := ledgerstate.Open(t.TempDir())
	_ = a.RecordCheckpoint("o", 5, rootA)
	_ = a.RecordEvent(0, []byte("e0"))
	b, _ := ledgerstate.Open(t.TempDir())
	_ = b.RecordCheckpoint("o", 9, rootB)
	_ = b.RecordCheckpoint("p", 2, rootA)
	_ = b.RecordEvent(0, []byte("e0"))
	_ = b.RecordEvent(1, []byte("e1"))
	if err := a.Merge(b.Snapshot()); err != nil {
		t.Fatal(err)
	}
	s := a.Snapshot()
	if s.Logs["o"].Size != 9 || s.Logs["p"].Size != 2 || s.Events != 2 {
		t.Errorf("%+v", s)
	}
	// an older copy changes nothing
	old, _ := ledgerstate.Open(t.TempDir())
	_ = old.RecordCheckpoint("o", 3, rootA)
	if err := a.Merge(old.Snapshot()); err != nil || a.Snapshot().Logs["o"].Size != 9 {
		t.Errorf("an older copy: %v", err)
	}
	// the same size with another root is a fork and nothing is merged
	c, _ := ledgerstate.Open(t.TempDir())
	_ = c.RecordCheckpoint("o", 9, rootA)
	_ = c.RecordCheckpoint("q", 1, rootA)
	if err := a.Merge(c.Snapshot()); !errors.Is(err, ledgerstate.ErrFork) || a.Snapshot().Logs["q"].Size != 0 {
		t.Errorf("a fork: %v %+v", err, a.Snapshot())
	}
	if _, err := ledgerstate.Union(a.Snapshot(), c.Snapshot()); !errors.Is(err, ledgerstate.ErrFork) {
		t.Errorf("Union: %v", err)
	}
}

func TestEventsAreNotAppendedAfterAnOlderDatabase(t *testing.T) {
	f, _ := ledgerstate.Open(t.TempDir())
	_ = f.RecordEvent(0, []byte("e0"))
	_ = f.RecordEvent(1, []byte("e1"))
	if err := f.AllowEvent(1, []byte("e0")); !errors.Is(err, ledgerstate.ErrBehind) {
		t.Errorf("fewer events: %v", err)
	}
	if err := f.AllowEvent(2, []byte("other")); !errors.Is(err, ledgerstate.ErrFork) {
		t.Errorf("another last event: %v", err)
	}
	if err := f.AllowEvent(2, []byte("e1")); err != nil {
		t.Errorf("the same events: %v", err)
	}
	if err := f.RecordEvent(1, []byte("another e1")); !errors.Is(err, ledgerstate.ErrFork) {
		t.Errorf("a second note for number 1: %v", err)
	}
}
