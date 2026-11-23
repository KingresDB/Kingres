// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package mapgroup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
)

// The bootstrap of a hard failover of the map group while its server is alive (design ha-phase5-part2.md, "a hard failover of a live leader's server"): the
// old leader's own task asks (Planned.Hard), stops its primary fenced and hands the lease to this server; no proof exists, the standbys are ranked after the stop,
// the loss is judged with the facts the old leader read before the stop, and the old leader's task is the one that is recorded.

func hardRequest() Request {
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10, Hard: true, Facts: &journal.PlannedFacts{SyncStreaming: true, PrimaryPlanned: 1}}
	return rq
}

func hardWorld() *world {
	w := newWorld()
	w.live["s1"] = "live"
	w.oldAlive = true
	// the record of the last check in the system database is old by now: the old leader stopped, and nothing writes it any more
	w.facts = failover.GroupFacts{SyncStreaming: false, LastKnownAge: 10 * time.Minute, PrimaryPlanned: 1}
	return w
}

// No proof is waited for or sent: the promotion carries none, the task that is marked promoted is the old leader's, the pin and the verdict go with it
// (the old leader could not write them: its system database was the stopped primary), and no task is recorded.
func TestAHardFailoverOfTheMapGroupPromotesWithoutAProofAndMarksTheOldLeadersTask(t *testing.T) {
	w := hardWorld()
	r, js := w.runner(t)
	if _, err := r.Run(context.Background(), hardRequest()); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(w.log, "\n"))
	}
	if len(w.promoteReqs) != 1 || w.promoteReqs[0].Checkpoint != "" || w.promoteReqs[0].ClusterState != "" || w.promoteReqs[0].StandbyID != 10 {
		t.Fatalf("the promotion: %+v", w.promoteReqs)
	}
	if w.count("record-task") != 0 || w.count("record-planned 77") != 1 {
		t.Fatalf("the task of the old leader is the one that is marked:\n%s", strings.Join(w.log, "\n"))
	}
	if len(w.plannedPins) != 1 || w.plannedPins[0] == nil || w.plannedPins[0].StandbyID != 10 || w.plannedPins[0].Server != "s2" || w.plannedPins[0].Node != 4 {
		t.Fatalf("the pin that was recorded: %+v", w.plannedPins)
	}
	var dl failover.DataLoss
	if err := json.Unmarshal(w.plannedDL[0], &dl); err != nil || dl.Verdict != failover.VerdictNone {
		t.Fatalf("the verdict that was recorded: %s (%v)", w.plannedDL[0], err)
	}
	if cur, _ := js.Current(); !cur.Recorded || cur.TaskID != 77 {
		t.Fatalf("the journal does not name the task of the old leader: %+v", cur)
	}
	if w.count("demote") != 0 {
		t.Fatalf("the bootstrap must not ask the old server to demote (its task did):\n%s", strings.Join(w.log, "\n"))
	}
}

// A switchover pinned in its own task: the row has its pin already, and the bootstrap passes none.
func TestAPlannedSwitchoverPassesNoPinToTheRecordOfTheTask(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	r, js := w.runner(t)
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10, Checkpoint: "x", ClusterState: "y"}
	n := 0
	base := r.Sleep
	r.Sleep = func(ctx context.Context, d time.Duration) error {
		if n++; n == 2 {
			_ = js.SetProof("0/6000028", "shut down")
		}
		return base(ctx, d)
	}
	if _, err := r.Run(context.Background(), rq); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(w.log, "\n"))
	}
	if len(w.plannedPins) != 1 || w.plannedPins[0] != nil || w.plannedDL[0] != nil {
		t.Fatalf("a switchover passed a pin: %+v %s", w.plannedPins, w.plannedDL)
	}
}

// The facts of the old leader's stop are what the verdict uses: the record in the system database is old (no check can follow the stop), and a candidate that
// used it would turn a failover of a group that streamed a synchronous standby into one that needs a decision.
func TestTheLossOfAHardFailoverIsJudgedWithTheFactsOfTheStop(t *testing.T) {
	w := hardWorld()
	r, _ := w.runner(t)
	if _, err := r.Run(context.Background(), hardRequest()); err != nil {
		t.Fatalf("a group that streamed a synchronous standby when it stopped is not a loss: %v", err)
	}
	// and without the synchronous standby the loss needs the acceptance of the operator
	w = hardWorld()
	r, js := w.runner(t)
	rq := hardRequest()
	rq.Planned.Facts.SyncStreaming = false
	_, err := r.Run(context.Background(), rq)
	var pe *failover.PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a pause for a loss that was not accepted: %v", err)
	}
	if !strings.Contains(pe.Reason, "Send the request to server s2") {
		t.Fatalf("the pause must name the server that takes the repeated request: %s", pe.Reason)
	}
	if w.count("promote ") != 0 || len(w.alerts) == 0 || w.alerts[0] != "failoverNeedsDecision" {
		t.Fatalf("promoted, or no alert:\n%s %v", strings.Join(w.log, "\n"), w.alerts)
	}
	// the repeated request of an operator carries the acceptance, and a plain request is taken here because the journal of a hard failover is in progress
	again := req()
	again.AllowDataLoss = true
	if _, err := r.Decide(context.Background(), again); err != nil {
		t.Fatalf("the repeated request: %v", err)
	}
	if cur, _ := js.Current(); !cur.AllowDataLoss {
		t.Fatalf("the acceptance was not taken: %+v", cur)
	}
	if _, err := r.Run(context.Background(), again); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(w.log, "\n"))
	}
	if w.count("promote s2") != 1 {
		t.Fatalf("not promoted after the acceptance:\n%s", strings.Join(w.log, "\n"))
	}
}

// A request for a live old server that is not the old leader's own (no Planned) is refused as before, also when a journal of a planned switchover (not a hard
// failover) is in progress: only a hard failover takes the repeated request.
func TestOnlyAHardFailoverTakesTheRepeatedRequestForALiveOldServer(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	r, js := w.runner(t)
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	if _, err := r.Decide(context.Background(), rq); err != nil {
		t.Fatal(err)
	}
	_, err := r.Decide(context.Background(), req())
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != "unsupported_operation" {
		t.Fatalf("a plain request while a switchover is in progress: %v", err)
	}
	if r.HardInProgress("s1") {
		t.Fatal("a planned switchover is not a hard failover")
	}
	_ = js
	// a hard failover for another old server is not taken either
	w2 := hardWorld()
	r2, _ := w2.runner(t)
	if _, err := r2.Decide(context.Background(), hardRequest()); err != nil {
		t.Fatal(err)
	}
	if r2.HardInProgress("s9") || !r2.HardInProgress("s1") {
		t.Fatal("HardInProgress names the old server of the journal")
	}
}

// A hard failover carries no proof, and an automatic start for a live server is refused as before.
func TestAHardRequestWithAProofIsRefused(t *testing.T) {
	w := hardWorld()
	r, js := w.runner(t)
	_ = js
	var ref *Refusal
	rq := hardRequest()
	rq.Trigger = "automatic"
	if _, err := r.Decide(context.Background(), rq); !errors.As(err, &ref) || ref.Code != "conflict" {
		t.Fatalf("an automatic hard request: %v", err)
	}
}

// The standby that the old leader named is not the choice: the old primary was stopped and the positions have settled, so the bootstrap ranks them all. Here the
// standby the old leader named (11, on s3) is behind the one on this server, and this server's is pinned (a planned switchover, which cannot be proven for any
// other, would refuse).
func TestAHardFailoverRanksTheStandbysAfterTheStop(t *testing.T) {
	w := hardWorld()
	r, js := w.runner(t)
	rq := hardRequest()
	rq.Planned.StandbyID = 11
	if _, err := r.Run(context.Background(), rq); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(w.log, "\n"))
	}
	if cur, _ := js.Current(); cur.Pin == nil || cur.Pin.StandbyID != 10 {
		t.Fatalf("pin %+v", cur.Pin)
	}
}

// When another standby is better the lease goes to it and it continues with the same journal: the hard flag and the facts travel with the journal.
func TestAHardFailoverHandsToABetterCandidateWithItsFacts(t *testing.T) {
	w := hardWorld()
	w.pos["s3"] = failover.Position{Server: "s3", InRecovery: true, Timeline: 1, ReceiveLsn: "0/6000000", ReplayLsn: "0/6000000", SystemIdentifier: "9"}
	r, _ := w.runner(t)
	res, err := r.Run(context.Background(), hardRequest())
	if err != nil || res.HandedTo != "s3" {
		t.Fatalf("%+v %v\n%s", res, err, strings.Join(w.log, "\n"))
	}
	if w.count("lease-transfer s2 to s3") != 1 || w.count("continue s3") != 1 || w.count("promote ") != 0 {
		t.Fatalf("\n%s", strings.Join(w.log, "\n"))
	}
}

// The wait for the lease of a hard failover is the one of a planned switchover (the old leader's fencing call is the thing waited for), not the 25 s of a dead
// leader's lapse: a lease that is refused for forty seconds is waited out.
func TestAHardFailoverWaitsLongerForTheLeaseThanAFailoverOfADeadLeader(t *testing.T) {
	w := hardWorld()
	w.fenceRefusals = 40
	r, _ := w.runner(t)
	if _, err := r.Run(context.Background(), hardRequest()); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(w.log[:10], "\n"))
	}
	if w.count("lease-fence s2") != 41 {
		t.Fatalf("%d fence rounds", w.count("lease-fence s2"))
	}
	// and a dead leader's bootstrap still gives up after the lapse
	d := newWorld()
	d.fenceRefusals = 40
	rd, _ := d.runner(t)
	_, err := rd.Run(context.Background(), req())
	var fe *failover.Error
	if !errors.As(err, &fe) || fe.Code != failover.CodeLeaseStillHeld {
		t.Fatalf("a dead leader whose lease stays held: %v", err)
	}
}

// A crash of kingresd at any step boundary of a hard failover is followed by the rest from the journal: one promotion, one mark of the old leader's task.
func TestAHardFailoverSurvivesACrashAtEveryStepBoundary(t *testing.T) {
	for _, step := range journal.Order {
		for _, where := range []string{"before", "after"} {
			point := step + ":" + where
			t.Run(point, func(t *testing.T) {
				w := hardWorld()
				r, js := w.runner(t)
				crashed := false
				r.Fault = func(p string) error {
					if p == point && !crashed {
						crashed = true
						return errors.New("kingresd ended")
					}
					return nil
				}
				_, err := r.Run(context.Background(), hardRequest())
				if step != journal.StepDecide && err == nil {
					t.Fatalf("the fault at %s did not stop the run", point)
				}
				if _, _, err := r.Resume(context.Background()); err != nil {
					t.Fatalf("resume: %v\n%s", err, strings.Join(w.log, "\n"))
				}
				if w.count("promote s2") != 1 {
					t.Fatalf("%d promotions:\n%s", w.count("promote s2"), strings.Join(w.log, "\n"))
				}
				if w.count("record-task") != 0 {
					t.Fatal("a task was recorded")
				}
				if !js.IsDone(journal.StepGateOpen) {
					t.Fatal("the gate is not open")
				}
			})
		}
	}
}

// The standby that the request named is ranked as the ordinary groups rank it: it is chosen although another is better, and being behind the best is a loss that the
// operator has to accept (the verdict), after the stop as before it.
func TestAHardFailoverRanksTheStandbyTheRequestNamed(t *testing.T) {
	w := hardWorld() // the standby on s2 (this server) is ahead of the one on s3
	r, _ := w.runner(t)
	rq := hardRequest()
	rq.Planned.Target = "s3"
	_, err := r.Run(context.Background(), rq)
	var pe *failover.PauseError
	if !errors.As(err, &pe) || !strings.Contains(pe.Reason, "could lose data") {
		t.Fatalf("the named standby is behind the best: want a pause for the loss, got %v", err)
	}
	if w.count("promote ") != 0 {
		t.Fatal("promoted")
	}
	// accepted: the lease goes to the named standby's server, which continues with the journal
	w = hardWorld()
	r, _ = w.runner(t)
	rq = hardRequest()
	rq.Planned.Target = "s3"
	rq.AllowDataLoss = true
	res, err := r.Run(context.Background(), rq)
	if err != nil || res.HandedTo != "s3" {
		t.Fatalf("%+v %v\n%s", res, err, strings.Join(w.log, "\n"))
	}
}

// M3. A group 0 row that is not there to be marked is a pause with an alert and an event, not a bootstrap that goes on as if the task were finished.
func TestAMissingRowOfTheOldLeadersTaskPausesTheBootstrap(t *testing.T) {
	w := hardWorld()
	w.plannedErr = ErrNoPlannedRow
	r, js := w.runner(t)
	_, err := r.Run(context.Background(), hardRequest())
	var pe *failover.PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a pause: %v", err)
	}
	if len(w.alerts) == 0 || w.alerts[len(w.alerts)-1] != "failoverNeedsDecision" {
		t.Fatalf("alerts %v", w.alerts)
	}
	cur, _ := js.Current()
	found := false
	for _, e := range cur.Events {
		if e.Kind == "plannedRowMissing" {
			found = true
		}
	}
	if !found || cur.Recorded {
		t.Fatalf("event %v recorded %v", found, cur.Recorded)
	}
}
