// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package failover

import (
	"context"
	"errors"
	"testing"
	"time"
)

type repairFake struct {
	prim       map[int32]string
	states     map[int32]State
	stateErr   map[int32]error
	busy       []bool // the answers of Unfinished in order (the last repeats)
	busyN      int
	resumeErr  error
	resumed    []int32
	terms      []int64
	unquiesced []int32
}

func (f *repairFake) Primaries(context.Context) (map[int32]string, error) { return f.prim, nil }
func (f *repairFake) State(_ context.Context, _ string, g int32) (State, error) {
	if err := f.stateErr[g]; err != nil {
		return State{}, err
	}
	return f.states[g], nil
}
func (f *repairFake) Term(context.Context) (int64, error) { return 7, nil }
func (f *repairFake) Resume(_ context.Context, _ string, g int32, term int64) error {
	f.terms = append(f.terms, term)
	f.resumed = append(f.resumed, g)
	return f.resumeErr
}
func (f *repairFake) Unquiesce(_ context.Context, _ string, g int32, term int64) error {
	f.terms = append(f.terms, term)
	f.unquiesced = append(f.unquiesced, g)
	return nil
}
func (f *repairFake) Unfinished(context.Context) (bool, error) {
	if len(f.busy) == 0 {
		return false, nil
	}
	n := f.busyN
	f.busyN++
	if n >= len(f.busy) {
		n = len(f.busy) - 1
	}
	return f.busy[n], nil
}
func (f *repairFake) Log(string, ...any) {}

func newRepair(f *repairFake) (*Repair, *time.Time) {
	now := time.Unix(1_000_000, 0)
	return &Repair{P: f, Grace: time.Minute, Now: func() time.Time { return now }}, &now
}

// F4-02. A group whose primary stays held (the hold rules, or the marker of a hand-over) while no failover task exists is resumed, after a
// grace period in which a task that is about to begin can show itself.
func TestRepairResumesAGroupThatStaysHeld(t *testing.T) {
	f := &repairFake{prim: map[int32]string{1: "s1"}, states: map[int32]State{1: {Quiesced: true}}}
	r, now := newRepair(f)
	ctx := context.Background()
	if done, _ := r.Step(ctx); len(done) != 0 || len(f.resumed) != 0 {
		t.Fatal("repaired at the first sight")
	}
	*now = now.Add(30 * time.Second)
	if done, _ := r.Step(ctx); len(done) != 0 {
		t.Fatal("repaired inside the grace period")
	}
	*now = now.Add(31 * time.Second)
	done, err := r.Step(ctx)
	for _, x := range f.terms {
		if x != 7 {
			t.Errorf("an undo call carried the term %d, the catalog's is 7", x)
		}
	}
	if err != nil || len(done) != 1 || done[0] != 1 || len(f.resumed) != 1 || len(f.unquiesced) != 1 {
		t.Fatalf("%v %v resumed %v unquiesced %v", done, err, f.resumed, f.unquiesced)
	}
}

// A marker of a hand-over (the primary was given up and stopped) is a hold as well.
func TestRepairResumesAPrimaryThatWasGivenUp(t *testing.T) {
	f := &repairFake{prim: map[int32]string{2: "s2"}, states: map[int32]State{2: {Relinquished: true}}}
	r, now := newRepair(f)
	_, _ = r.Step(context.Background())
	*now = now.Add(2 * time.Minute)
	if done, _ := r.Step(context.Background()); len(done) != 1 || len(f.resumed) != 1 {
		t.Fatalf("%v", done)
	}
}

// While a task is unfinished nothing is repaired, and what was seen before is forgotten: a task owns its holds.
func TestRepairWaitsForTheTask(t *testing.T) {
	f := &repairFake{prim: map[int32]string{1: "s1"}, states: map[int32]State{1: {Quiesced: true}}, busy: []bool{false, true, false}}
	r, now := newRepair(f)
	ctx := context.Background()
	_, _ = r.Step(ctx)
	*now = now.Add(2 * time.Minute)
	_, _ = r.Step(ctx) // busy: forgets
	*now = now.Add(2 * time.Minute)
	if done, _ := r.Step(ctx); len(done) != 0 || len(f.resumed) != 0 {
		t.Fatal("repaired right after a task ended, with no time to see the hold again")
	}
}

// A task that begins between the look and the repair owns the hold.
func TestRepairLooksAtTheTasksAgainBeforeItActs(t *testing.T) {
	f := &repairFake{prim: map[int32]string{1: "s1"}, states: map[int32]State{1: {Quiesced: true}}, busy: []bool{false, false, true}}
	r, now := newRepair(f)
	ctx := context.Background()
	_, _ = r.Step(ctx)
	*now = now.Add(2 * time.Minute)
	if done, _ := r.Step(ctx); len(done) != 0 || len(f.resumed) != 0 {
		t.Fatalf("a task that began meanwhile was ignored: %v", done)
	}
}

// A server that does not answer, and a repair that fails, are tried again; a group that is not held is never touched.
func TestRepairTriesAgain(t *testing.T) {
	f := &repairFake{prim: map[int32]string{1: "s1", 2: "s2"}, states: map[int32]State{1: {Quiesced: true}, 2: {}},
		stateErr: map[int32]error{}, resumeErr: errors.New("down")}
	r, now := newRepair(f)
	ctx := context.Background()
	_, _ = r.Step(ctx)
	*now = now.Add(2 * time.Minute)
	if done, _ := r.Step(ctx); len(done) != 0 || len(f.resumed) != 1 || f.resumed[0] != 1 {
		t.Fatalf("%v %v", done, f.resumed)
	}
	f.resumeErr = nil
	if done, _ := r.Step(ctx); len(done) != 1 || len(f.resumed) != 2 {
		t.Fatalf("not repeated: %v %v", done, f.resumed)
	}
	f.stateErr[1] = ErrUnreachable
	f.states[1] = State{Quiesced: true}
	n := len(f.resumed)
	_, _ = r.Step(ctx)
	if len(f.resumed) != n {
		t.Fatal("a server that does not answer was repaired")
	}
}
