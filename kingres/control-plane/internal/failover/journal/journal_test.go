// SPDX-License-Identifier: AGPL-3.0-only

package journal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheJournalSurvivesARestartAndKeepsItsFirstPin(t *testing.T) {
	dir := tempDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Gate() || s.Active() {
		t.Fatal("a server without a journal must have its gate open")
	}
	if _, err := s.Begin("b1", "s1", 7, "manual", "admin", false); err != nil {
		t.Fatal(err)
	}
	if s.Gate() || !s.Active() {
		t.Fatal("the gate must be closed while a bootstrap is in progress")
	}
	if err := s.Done(StepDecide); err != nil {
		t.Fatal(err)
	}
	if p, err := s.SetPin(Pin{StandbyID: 3, Server: "s2", Node: 4}); err != nil || p.StandbyID != 3 {
		t.Fatalf("%v %v", p, err)
	}
	if p, _ := s.SetPin(Pin{StandbyID: 9, Server: "s3", Node: 5}); p.StandbyID != 3 {
		t.Fatalf("a second pin replaced the first: %v", p)
	}
	if err := s.Event(StepPin, "pinned", map[string]any{"standby": 3}); err != nil {
		t.Fatal(err)
	}
	// kingresd restarts: the journal is read first and the same bootstrap goes on.
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Active() || !r.IsDone(StepDecide) || r.IsDone(StepPin) {
		t.Fatal("the steps of the journal were not kept")
	}
	j, err := r.Begin("b2", "s1", 7, "manual", "admin", false)
	if err != nil || j.ID != "b1" || j.Pin == nil || j.Pin.StandbyID != 3 || len(j.Events) != 1 {
		t.Fatalf("a restart must resume the bootstrap in progress: %+v %v", j, err)
	}
	if err := r.Done(StepGateOpen); err != nil {
		t.Fatal(err)
	}
	if r.Active() || !r.Gate() {
		t.Fatal("the gate must open at B8")
	}
}

func TestAJournalOfANewerTermRefusesAndOfAnOlderOneIsReplaced(t *testing.T) {
	s, _ := Open(tempDir(t))
	if _, err := s.Begin("a", "s1", 9, "automatic", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin("b", "s1", 8, "manual", "x", false); !errors.Is(err, ErrNewerTerm) {
		t.Fatalf("got %v", err)
	}
	_ = s.Done(StepGateOpen)
	j, err := s.Begin("c", "s2", 10, "manual", "x", true)
	if err != nil || j.ID != "c" || !j.AllowDataLoss || len(j.Done) != 0 {
		t.Fatalf("%+v %v", j, err)
	}
}

func TestAJournalThatCannotBeReadIsAnError(t *testing.T) {
	dir := tempDir(t)
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("a damaged journal was ignored")
	}
}

func TestUndoForgetsAStep(t *testing.T) {
	s, _ := Open(tempDir(t))
	_, _ = s.Begin("a", "s1", 3, "manual", "x", false)
	_ = s.Done(StepLease)
	_ = s.Done(StepPromote)
	if err := s.Undo(StepLease, StepPromote); err != nil || s.IsDone(StepLease) || s.IsDone(StepPromote) {
		t.Fatalf("%v", err)
	}
}

// tempDir is a directory that is removed without failing the test when the machine still holds a file in it (an indexer on Windows).
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "kg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// F4-06. A bootstrap that did not reach the promotion can be given up (the gate opens, a new one may begin); one that promoted cannot.
func TestABootstrapThatDidNotPromoteCanBeGivenUp(t *testing.T) {
	s, err := Open(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin("a", "s1", 3, "automatic", "", false); err != nil {
		t.Fatal(err)
	}
	if !s.Active() || s.Gate() {
		t.Fatal("a bootstrap in progress keeps the gate closed")
	}
	if err := s.Abandon("test"); err != nil {
		t.Fatal(err)
	}
	if s.Active() || !s.Gate() {
		t.Fatal("an abandoned bootstrap is still active")
	}
	j, err := s.Begin("b", "s1", 3, "automatic", "", false)
	if err != nil || j.ID != "b" || !s.Active() {
		t.Fatalf("a new bootstrap of the same term did not begin: %+v %v", j, err)
	}
	if err := s.Done(StepPromote); err != nil {
		t.Fatal(err)
	}
	if err := s.Abandon("late"); !errors.Is(err, ErrPromoted) || !s.Active() {
		t.Fatalf("a promoted bootstrap was given up: %v", err)
	}
}

func TestWhenABootstrapIsStuck(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	j := Journal{Term: 5, StartedAt: now.Add(-time.Minute), Done: map[string]time.Time{}}
	if stuck, _ := Stuck(j, 5, now, 15*time.Minute); stuck {
		t.Error("a young bootstrap of the current term is not stuck")
	}
	if stuck, _ := Stuck(j, 6, now, 15*time.Minute); !stuck {
		t.Error("a bootstrap of an older term is stuck")
	}
	j.StartedAt = now.Add(-20 * time.Minute)
	if stuck, _ := Stuck(j, 5, now, 15*time.Minute); !stuck {
		t.Error("an old bootstrap that did not promote is stuck")
	}
	j.Done[StepPromote] = now
	if stuck, _ := Stuck(j, 6, now, 15*time.Minute); stuck {
		t.Error("a promoted bootstrap is never given up")
	}
}

// A planned switchover marks the journal once, before a standby is pinned; the proof is recorded once and a different one is refused.
func TestThePlannedFieldsAndTheProofOfAJournal(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProof("0/1", "shut down"); err == nil {
		t.Fatal("a proof for a bootstrap that is not planned")
	}
	if _, err := s.Begin("b", "s1", 3, "manual", "op", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPlanned(Planned{TaskID: 9, StandbyID: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPlanned(Planned{TaskID: 9, StandbyID: 4}); err != nil {
		t.Fatalf("the same again: %v", err)
	}
	if _, err := s.SetPlanned(Planned{TaskID: 10, StandbyID: 4}); err == nil {
		t.Fatal("another planned switchover on the same journal")
	}
	if err := s.SetProof("0/6000028", "shut down"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProof("0/6000028", "shut down"); err != nil {
		t.Fatalf("the same proof again: %v", err)
	}
	if err := s.SetProof("0/7000000", "shut down"); err == nil {
		t.Fatal("another proof replaced the first")
	}
	// It survives a restart and is part of the copy that Current returns.
	s2, err := Open(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	if cur, _ := s2.Current(); cur.Planned == nil || cur.Planned.Checkpoint != "0/6000028" || cur.Planned.TaskID != 9 {
		t.Fatalf("after a restart: %+v", cur.Planned)
	}
	// A standby pinned: too late to mark a bootstrap as planned.
	s3, _ := Open(t.TempDir())
	_, _ = s3.Begin("c", "s1", 3, "manual", "op", false)
	_, _ = s3.SetPin(Pin{StandbyID: 1, Server: "s2"})
	if _, err := s3.SetPlanned(Planned{TaskID: 1, StandbyID: 1}); err == nil {
		t.Fatal("planned after the pin")
	}
}

// F5-03. The intent to promote is recorded before the call: from then on the bootstrap cannot be given up as if nothing had been promoted, unless the caller
// knows that nothing was (AbandonUnpromoted); a recorded promotion refuses both.
func TestAPromotionThatWasBegunCannotBeGivenUpByTheOrdinaryCall(t *testing.T) {
	s, _ := Open(t.TempDir())
	_, _ = s.Begin("b", "s1", 3, "manual", "op", false)
	if s.PromotionStarted() {
		t.Fatal("an intent that was not recorded")
	}
	if err := s.PromotionIntent(); err != nil {
		t.Fatal(err)
	}
	if err := s.PromotionIntent(); err != nil || !s.PromotionStarted() {
		t.Fatalf("the intent is recorded once: %v", err)
	}
	if err := s.Abandon("x"); !errors.Is(err, ErrPromoted) || !s.Active() {
		t.Fatalf("the ordinary call after the intent: %v", err)
	}
	if err := s.AbandonUnpromoted("x"); err != nil || s.Active() {
		t.Fatalf("a caller that knows nothing was promoted: %v", err)
	}
	s2, _ := Open(t.TempDir())
	_, _ = s2.Begin("b", "s1", 3, "manual", "op", false)
	_ = s2.PromotionIntent()
	_ = s2.Done(StepPromote)
	if err := s2.AbandonUnpromoted("x"); !errors.Is(err, ErrPromoted) {
		t.Fatalf("a recorded promotion: %v", err)
	}
}

// F6-02. The intent to promote is refused for a bootstrap that was given up: a runner that was past its last look at the journal cannot begin a promotion for it.
func TestThePromotionIntentIsRefusedAfterTheAbandon(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin("b1", "s1", 3, "manual", "alice", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Abandon("an operator"); err != nil {
		t.Fatal(err)
	}
	if err := s.PromotionIntent(); !errors.Is(err, ErrAbandoned) || s.PromotionStarted() {
		t.Fatalf("the intent after the abandon: %v (started %v)", err, s.PromotionStarted())
	}
}

// A hard failover of a live leader's server marks the journal as hard, with the facts of the old leader's stop; both survive a restart and a copy, the flag is part of the
// identity of the request (the same task as a planned switchover is another request), and a copy does not share the facts with the journal.
func TestTheHardFlagAndTheFactsOfAPlannedJournal(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin("b", "s1", 3, "manual", "op", true); err != nil {
		t.Fatal(err)
	}
	f := &PlannedFacts{SyncStreaming: true, LastKnownLsn: "0/5000000", PrimaryPlanned: 1, AsyncDatabases: []string{"a"}}
	if _, err := s.SetPlanned(Planned{TaskID: 9, StandbyID: 4, Hard: true, Facts: f}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPlanned(Planned{TaskID: 9, StandbyID: 4, Hard: true, Facts: f}); err != nil {
		t.Fatalf("the same again: %v", err)
	}
	if _, err := s.SetPlanned(Planned{TaskID: 9, StandbyID: 4}); err == nil {
		t.Fatal("the same task as a planned switchover was taken for the hard failover that began")
	}
	cur, _ := s.Current()
	cur.Planned.Facts.SyncStreaming = false
	if again, _ := s.Current(); !again.Planned.Facts.SyncStreaming {
		t.Fatal("a copy shares the facts with the journal")
	}
	s2, err := Open(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s2.Current()
	if got.Planned == nil || !got.Planned.Hard || got.Planned.Facts == nil || !got.Planned.Facts.SyncStreaming || got.Planned.Facts.LastKnownLsn != "0/5000000" ||
		got.Planned.Facts.PrimaryPlanned != 1 || len(got.Planned.Facts.AsyncDatabases) != 1 || !got.AllowDataLoss {
		t.Fatalf("after a restart: %+v", got.Planned)
	}
}
