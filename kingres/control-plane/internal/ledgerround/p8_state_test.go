// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerround

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerstate"
)

func openState(t *testing.T) *ledgerstate.File {
	t.Helper()
	f, err := ledgerstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// withState makes the collector of e keep the record f.
func (e *env) withState(f *ledgerstate.File) *env {
	e.published = f
	e.restart()
	return e
}

// P8-01, the reviewer's sequence. A live publisher publishes aggregate sizes 1 to 3. The system database is restored from a backup that holds
// size 1 only, while the chains have moved on, and the restored database is given to a collector: it must sign nothing (no checkpoint, no event),
// raise the critical alert, and leave the restored database as it is. Before the record existed the same sequence signed a second root for
// sizes 2 and 3 with no alert.
func TestARestoredDatabaseThatIsBehindWhatWasPublishedSignsNothing(t *testing.T) {
	f := openState(t)
	a, b := newChain(1, 3), newChain(40, 2)
	live := newEnv(t, a, b).withState(f)
	live.round()
	a.add(2)
	live.advance(time.Minute)
	live.round()
	b.add(1)
	live.advance(time.Minute)
	live.round()
	liveSize := live.aggregateLog().Size
	if liveSize < 3 {
		t.Fatalf("the live aggregate is at %d", liveSize)
	}
	liveCP, _ := live.store.LatestCheckpoint(bg, live.aggregateLog().ID)

	// the restored database: the same history up to its first round, on its own store, a clock that has run differently
	a2, b2 := newChain(1, 3), newChain(40, 2)
	restored := newEnv(t, a2, b2)
	restored.round()
	restoredSize := restored.aggregateLog().Size
	restoredLogs := len(restored.logs())
	a2.add(2)
	b2.add(1) // the chains have moved on
	restored.withState(f)
	restored.advance(7 * time.Hour)
	restored.alerts = nil
	res := restored.round()
	if res.SignErr == nil || !errors.Is(res.SignErr, ErrPublishedState) {
		t.Fatalf("a restored database was given to the publisher and the round said %v", res.SignErr)
	}
	if !restored.hasAlert(AlertPublishedKind + "/raise/critical/" + AlertPublishedSubject) {
		t.Errorf("no critical alert: %v", restored.alerts)
	}
	if got := restored.aggregateLog().Size; got != restoredSize || len(restored.logs()) != restoredLogs {
		t.Errorf("the restored database was written: aggregate %d (was %d)", got, restoredSize)
	}
	for _, g := range []int32{0, 1} {
		if cp, err := restored.store.LatestCheckpoint(bg, restored.chainLog(g).ID); err != nil || cp.Size > 5 {
			t.Errorf("group %d checkpoint %v %v", g, cp.Size, err)
		}
	}
	// the live record is untouched: size and root of the live aggregate are what the state holds
	if m := f.Snapshot().Logs[live.aggregateLog().Origin]; m.Size != liveSize || m.Root != hexOf(liveCP.Root) {
		t.Errorf("the record of what was published: %+v, the live aggregate: %d %x", m, liveSize, liveCP.Root)
	}
	// a retirement (a drop) and a re-signing are refused too
	if err := restored.coll.RetireDatabase(bg, "shop", ReasonDropped, true); !errors.Is(err, ErrCannotRetire) {
		t.Errorf("a retirement on a restored database: %v", err)
	}
	// the database comes back to what was published (the live one is given): the alert clears and the rounds go on
	live.alerts = nil
	live.published = f
	live.restart()
	live.advance(time.Minute)
	live.round()
	if !live.hasAlert(AlertPublishedKind+"/clear/") && live.hasAlert(AlertPublishedKind+"/raise/") {
		t.Errorf("alerts %v", live.alerts)
	}
}

// P8-01: the record is lost on this server (a host restored from a backup of the key directory, a new holder) and the database is a restored one: a
// server that is about to lead asks the other holders first, so the publisher is still refused. With no holder to ask, the database is all there
// is to go on, and a log line says that it is taken as published.
func TestTheRecordIsAskedForFromTheOtherHoldersWhenItIsLost(t *testing.T) {
	f := openState(t)
	a, b := newChain(1, 3), newChain(40, 2)
	live := newEnv(t, a, b).withState(f)
	live.round()
	a.add(2)
	live.advance(time.Minute)
	live.round()

	a2, b2 := newChain(1, 3), newChain(40, 2)
	restored := newEnv(t, a2, b2)
	restored.round()
	a2.add(2)
	b2.add(1)
	lost := openState(t) // this server has no record
	restored.pull = func(context.Context) (ledgerstate.State, bool) { return f.Snapshot(), true }
	restored.withState(lost)
	restored.advance(3 * time.Hour)
	res := restored.round()
	if !errors.Is(res.SignErr, ErrPublishedState) {
		t.Fatalf("with the record of another holder: %v", res.SignErr)
	}
	if !lost.Initialized() {
		t.Error("the record pulled from the holder was not kept")
	}
	// no holder answers: the database is taken as the starting point (the limit that the design states)
	alone := newEnv(t, newChain(1, 3), newChain(40, 2))
	alone.round()
	alone.withState(openState(t))
	alone.advance(time.Minute)
	if res := alone.round(); res.SignErr != nil {
		t.Fatalf("a first start with no record and no holder: %v", res.SignErr)
	}
}

// P8-01: the events. The database holds fewer events than were published, or another note at the last number: no event is appended and nothing
// is signed.
func TestAnEventNumberThatWasPublishedIsNeverGivenOtherContent(t *testing.T) {
	f := openState(t)
	a := newChain(1, 3)
	e := newEnv(t, a).withState(f)
	e.store.Events = [][]byte{[]byte("event 0\n"), []byte("event 1\n")}
	if err := f.Replace(ledgerstate.State{Logs: map[string]ledgerstate.Mark{}, Events: 2, EventHash: ledgerstate.EventHashOf([]byte("event 1\n"))}); err != nil {
		t.Fatal(err)
	}
	e.round() // consistent: fine
	// restored: one event only
	e.store.Events = [][]byte{[]byte("event 0\n")}
	res := e.round()
	if !errors.Is(res.SignErr, ErrPublishedState) {
		t.Fatalf("fewer events than were published: %v", res.SignErr)
	}
	// forked: two events, the second one is another note
	e.store.Events = [][]byte{[]byte("event 0\n"), []byte("another event 1\n")}
	if res := e.round(); !errors.Is(res.SignErr, ErrPublishedState) {
		t.Fatalf("another note at the last number: %v", res.SignErr)
	}
	// the key service asks before it appends one
	if err := e.coll.CheckPublished(bg); !errors.Is(err, ErrPublishedState) {
		t.Fatalf("CheckPublished: %v", err)
	}
	e.store.Events = [][]byte{[]byte("event 0\n"), []byte("event 1\n"), []byte("event 2\n")}
	if err := e.coll.CheckPublished(bg); err != nil {
		t.Fatalf("a database that is ahead is fine: %v", err)
	}
}

var _ = ledgerpub.StateActive
