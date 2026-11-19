// SPDX-License-Identifier: AGPL-3.0-only

package journal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTheJournalSurvivesARestartAndKeepsItsFirstPin(t *testing.T) {
	dir := t.TempDir()
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
	s, _ := Open(t.TempDir())
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
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("a damaged journal was ignored")
	}
}
