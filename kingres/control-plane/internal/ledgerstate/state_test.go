// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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

// P7A-14: a record that would be larger than a file may be is not written (a file that is larger is read as damaged at the next start, and a
// damaged record refuses every signature); the record on the disk stays the last one that was written, and nothing is signed until it can be.
func TestARecordThatWouldBeTooLargeIsNotWrittenAndNothingIsAllowedUntilItIs(t *testing.T) {
	dir := t.TempDir()
	f, _ := ledgerstate.Open(dir)
	if err := f.RecordCheckpoint("o1", 5, rootA); err != nil {
		t.Fatal(err)
	}
	defer ledgerstate.SetMaxFile(300)()
	var err error
	for i := 0; i < 20 && err == nil; i++ {
		err = f.RecordCheckpoint("origin-number-"+string(rune('a'+i)), 7, rootB)
	}
	if err == nil {
		t.Fatal("a record larger than the file limit was written")
	}
	// the file on the disk is the last good one, and it opens
	g, _ := ledgerstate.Open(dir)
	if g.Damaged() || !g.Initialized() {
		t.Fatalf("the file on the disk is damaged or gone: damaged %v", g.Damaged())
	}
	// the memory is current and the file is behind: no signature is allowed until the write succeeds
	if err := f.Allow("o1", 6, rootA); !errors.Is(err, ledgerstate.ErrNotWritten) {
		t.Errorf("Allow while the record cannot be written: %v", err)
	}
	if err := f.AllowEvent(0, nil); !errors.Is(err, ledgerstate.ErrNotWritten) {
		t.Errorf("AllowEvent: %v", err)
	}
}

// P7A-11: a write that failed is made again before the next signature is allowed.
func TestAWriteThatFailedIsMadeAgainBeforeTheNextSignatureIsAllowed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, _ := ledgerstate.Open(dir)
	if err := f.RecordCheckpoint("o", 5, rootA); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := f.RecordCheckpoint("o", 6, rootB); err == nil {
		t.Fatal("a write into a directory that is gone succeeded")
	}
	if err := f.Allow("o", 7, rootA); !errors.Is(err, ledgerstate.ErrNotWritten) {
		t.Fatalf("Allow: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.Allow("o", 7, rootA); err != nil {
		t.Fatalf("Allow after the directory is back: %v", err)
	}
	g, _ := ledgerstate.Open(dir)
	if g.Snapshot().Logs["o"].Size != 6 {
		t.Errorf("what was recorded is not on the disk: %+v", g.Snapshot())
	}
}

// P7A-02: the acceptance of a restore.
func TestTheAcceptanceOfARestoreReplacesTheRecordKeepsTheOldOneAndWinsTheMerge(t *testing.T) {
	dir := t.TempDir()
	f, _ := ledgerstate.Open(dir)
	_ = f.RecordCheckpoint("o", 9, rootA)
	_ = f.RecordEvent(0, []byte("e0"))
	_ = f.RecordEvent(1, []byte("e1"))
	old := f.Snapshot()
	db := ledgerstate.State{Logs: map[string]ledgerstate.Mark{"o": {Size: 4, Root: "04"}}, Events: 1, EventHash: ledgerstate.EventHashOf([]byte("e0"))}
	if err := f.Accept(db, ledgerstate.RestoreNote{Reason: "r", Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	cur := f.Snapshot()
	if cur.Generation != 1 || cur.Logs["o"].Size != 4 || cur.Events != 1 || cur.Restore == nil || cur.Restore.Reason != "r" {
		t.Fatalf("after Accept: %+v", cur)
	}
	if _, err := os.Stat(filepath.Join(dir, ledgerstate.FileName+".before-restore-1")); err != nil {
		t.Errorf("the old record was not kept: %v", err)
	}
	// it survives a restart with its note, and the confirmation file is read as the format says
	g, _ := ledgerstate.Open(dir)
	if g.Snapshot().Generation != 1 || g.Snapshot().Restore == nil || g.Damaged() {
		t.Fatalf("after a restart: %+v", g.Snapshot())
	}
	if err := g.ClearRestore(); err != nil || g.Snapshot().Restore != nil {
		t.Errorf("ClearRestore: %v", err)
	}
	// a holder with the old record takes the new one; the new one is not lowered by the old
	h, _ := ledgerstate.Open(t.TempDir())
	_ = h.Merge(old)
	if err := h.Merge(cur); err != nil || h.Snapshot().Logs["o"].Size != 4 || h.Snapshot().Generation != 1 {
		t.Errorf("the holder: %v %+v", err, h.Snapshot())
	}
	if err := h.Merge(old); err != nil || h.Snapshot().Logs["o"].Size != 4 {
		t.Errorf("the old record took the floor back: %v %+v", err, h.Snapshot())
	}
	// the confirmation depends on both states and on nothing else
	c1 := ledgerstate.Confirmation(old, db)
	if len(c1) != 16 || c1 != ledgerstate.Confirmation(old, db) {
		t.Errorf("confirmation %q", c1)
	}
	db2 := ledgerstate.State{Logs: map[string]ledgerstate.Mark{"o": {Size: 5, Root: "05"}}, Events: 1, EventHash: db.EventHash}
	if c1 == ledgerstate.Confirmation(old, db2) || c1 == ledgerstate.Confirmation(cur, db) {
		t.Error("the confirmation does not depend on the situation")
	}
	// the file of the confirmation
	p := filepath.Join(dir, ledgerstate.AcceptFileName)
	_ = os.WriteFile(p, []byte(" "+c1+" \nthe backup of Monday\nsecond line\n"), 0o600)
	if tok, reason := g.AcceptToken(); tok != c1 || reason != "the backup of Monday second line" {
		t.Errorf("token %q reason %q", tok, reason)
	}
	_ = os.WriteFile(p, []byte(c1+"\ncafé\n"), 0o600)
	if _, reason := g.AcceptToken(); reason != "" {
		t.Errorf("a reason that is not ASCII was taken: %q", reason)
	}
	g.ConsumeAcceptToken()
	if tok, _ := g.AcceptToken(); tok != "" {
		t.Error("the file was not consumed")
	}
}
