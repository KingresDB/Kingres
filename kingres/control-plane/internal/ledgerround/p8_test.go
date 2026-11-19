// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
)

// P8-03: the system instance still has the database, or it comes back before the time is up: nothing ends, and the alert that warned is cleared.
func TestAHealthyDatabaseWhoseGroupsAnsweredNoDatabaseIsNotClosed(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	e.round()
	e.src.noDB = true // exists answers true for "shop": the instance has it
	for i := 0; i < 5; i++ {
		e.advance(DefaultMissingFor)
		if err := e.coll.jobRound(bg, 5); err != nil {
			t.Fatal(err)
		}
	}
	if len(eventsOf(e.keys)) != 0 || e.chainLog(0).State != ledgerpub.StateActive {
		t.Fatalf("a database that the system instance has was closed: %v", eventsOf(e.keys))
	}
	// the groups answer again once, then the database is absent from the instance and they say "no database" again: the run starts anew
	e.src.noDB = false
	e.advance(61 * time.Second)
	_ = e.coll.jobRound(bg, 5)
	e.src.noDB = true
	e.exists = func(string) (bool, error) { return false, nil }
	e.advance(61 * time.Second)
	_ = e.coll.jobRound(bg, 5)
	e.advance(61 * time.Second)
	e.src.noDB = false
	_ = e.coll.jobRound(bg, 5)
	if !e.hasAlert(AlertUnread + "/clear/critical/database:shop") {
		t.Errorf("the warning was not cleared: %v", e.alerts)
	}
	e.src.noDB = true
	e.advance(DefaultMissingFor - time.Minute) // the clock of "missing" started again with the new run of answers
	_ = e.coll.jobRound(bg, 5)
	if len(eventsOf(e.keys)) != 0 {
		t.Fatal("an interrupted run of answers counted as one")
	}
}

// P8-03: a chain whose log was closed (here for database-missing) comes back with the same chain id: the log is not opened again and the new blocks are
// not published in silence; a critical alert says so, and nothing is signed for them.
func TestAChainThatComesBackAfterItsLogWasClosedIsReportedLoudly(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	e.round()
	e.src.noDB = true
	e.exists = func(string) (bool, error) { return false, nil }
	e.advance(time.Minute)
	_ = e.coll.jobRound(bg, 5)
	e.advance(DefaultMissingFor)
	_ = e.coll.jobRound(bg, 5)
	if e.chainLog(0).State != ledgerpub.StateClosed {
		t.Fatal("the log was not closed")
	}
	size, events := e.chainLog(0).Size, len(eventsOf(e.keys))
	e.src.noDB = false
	a.add(3)
	e.alerts = nil
	e.advance(time.Minute)
	e.round()
	if !e.hasAlert(AlertDiverged + "/raise/critical/database:shop|group:0") {
		t.Fatalf("the return of a closed chain raised no alert: %v", e.alerts)
	}
	if l := e.chainLog(0); l.Size != size || l.State != ledgerpub.StateClosed || len(eventsOf(e.keys)) != events {
		t.Errorf("something was published for the returned chain: %+v", l)
	}
}

// P8-04: two groups that swap their answers are a divergence of both with critical alerts; nothing is closed, no event is signed.
func TestTwoGroupsThatSwapTheirAnswersDivergeAndAreNotRetired(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	e.round()
	e.src.chains[0], e.src.chains[1] = b, a
	e.alerts = nil
	e.advance(time.Minute)
	e.round()
	for g := int32(0); g < 2; g++ {
		if l := e.chainLog(g); l.State != ledgerpub.StateDiverged {
			t.Errorf("group %d: %s %q", g, l.State, l.Detail)
		}
	}
	if len(eventsOf(e.keys)) != 0 {
		t.Errorf("events were signed: %v", eventsOf(e.keys))
	}
	if !e.hasAlert(AlertDiverged+"/raise/critical/database:shop|group:0") || !e.hasAlert(AlertDiverged+"/raise/critical/database:shop|group:1") {
		t.Errorf("alerts: %v", e.alerts)
	}
}

// P8-04: a chain that is really replaced (a new id) ends the old log with an event, starts a new log, and raises a critical alert.
func TestAReplacedChainIsAnAlertAndNotOnlyAnEvent(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	e.round()
	e.src.chains[0] = newChain(120, 2)
	e.alerts = nil
	e.advance(time.Minute)
	e.round()
	ev := eventsOf(e.keys)
	if len(ev) != 1 || !strings.Contains(ev[0], "reason=chain-replaced") {
		t.Fatalf("events %v", ev)
	}
	if !e.hasAlert(AlertDiverged + "/raise/critical/database:shop|group:0") {
		t.Errorf("a replacement raised no alert: %v", e.alerts)
	}
}

// P8-08: a diverged chain that is retired by dropping the database stays flagged diverged in the retiring leaf, and its alert is not cleared.
func TestADropDoesNotClearADivergence(t *testing.T) {
	a, b := newChain(1, 10), newChain(40, 4)
	e := newEnv(t, a, b)
	e.round()
	a.blocks = a.blocks[:5] // a rollback
	e.advance(time.Minute)
	e.round()
	if e.chainLog(0).State != ledgerpub.StateDiverged {
		t.Fatal("not diverged")
	}
	e.alerts = nil
	if err := e.coll.RetireDatabase(bg, "shop", ReasonDropped, true); err != nil {
		t.Fatal(err)
	}
	if e.hasAlert(AlertDiverged + "/clear/critical/database:shop|group:0") {
		t.Errorf("the divergence alert was cleared by a drop: %v", e.alerts)
	}
	last := flagsOf(leafAt(t, e, uint64(e.aggregateLog().Size)-1))
	if last[0]&ledgerlog.FlagDiverged == 0 || last[0]&ledgerlog.FlagRetired == 0 {
		t.Errorf("flags of the diverged member in the retiring leaf: %v", last)
	}
}

// P8-09: a tail loss of exactly one block, and an empty answer after blocks were published, stop the log as a divergence (the mutants that the
// review found alive).
func TestALossOfOneBlockAndAnEmptyAnswerAreDivergences(t *testing.T) {
	a, b := newChain(1, 6), newChain(40, 4)
	e := newEnv(t, a, b)
	e.round()
	a.blocks = a.blocks[:len(a.blocks)-1]
	e.advance(time.Minute)
	e.round()
	if l := e.chainLog(0); l.State != ledgerpub.StateDiverged {
		t.Fatalf("one block lost: %s %q", l.State, l.Detail)
	}
	b.blocks = nil // the group answers an empty ledger after blocks were published
	e.advance(time.Minute)
	e.round()
	if l := e.chainLog(1); l.State != ledgerpub.StateDiverged {
		t.Fatalf("an empty answer: %s %q", l.State, l.Detail)
	}
}

// The task of a drop ends the publication first and drops the database a few steps later, so the chain of a database that was just retired for
// "dropped" is still read: that raises no alert for DropGrace, nothing is published for it, and after the grace the return is reported as any other.
func TestTheChainOfADatabaseBeingDroppedRaisesNoAlertForTheGrace(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	e.round()
	if err := e.coll.RetireDatabase(bg, "shop", ReasonDropped, true); err != nil {
		t.Fatal(err)
	}
	size, events := e.chainLog(0).Size, len(eventsOf(e.keys))
	a.add(1)
	e.alerts = nil
	e.advance(time.Minute)
	e.round()
	if e.hasAlert(AlertDiverged + "/raise/critical/database:shop|group:0") {
		t.Fatalf("an alert inside the grace of a drop: %v", e.alerts)
	}
	if l := e.chainLog(0); l.Size != size || l.State != ledgerpub.StateClosed || len(eventsOf(e.keys)) != events {
		t.Errorf("something was published for the chain of a database being dropped: %+v", l)
	}
	e.advance(DropGrace)
	e.round()
	if !e.hasAlert(AlertDiverged + "/raise/critical/database:shop|group:0") {
		t.Fatalf("no alert after the grace: %v", e.alerts)
	}
}
