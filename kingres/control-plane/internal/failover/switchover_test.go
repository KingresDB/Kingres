// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// gracefulWorld is the world of a switchover: the old primary's server is alive and holds its lease, and both standbys stream.
func gracefulWorld() *world {
	w := newWorld()
	w.oldAlive, w.oldWritable, w.oldHolds = true, true, true
	w.live["s1"] = "live"
	w.markReplayed = true
	for _, s := range []string{"s2", "s3"} {
		p := w.pos[s]
		p.ReceiverState = "streaming"
		w.pos[s] = p
	}
	return w
}

func gracefulInput() Input {
	return Input{Mode: ModeGraceful, ServerID: "s1", Groups: []int32{1}, Trigger: "manual", RequestedBy: "tester"}
}

func failedWith(t *testing.T, w *world, err error, code string) {
	t.Helper()
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != code {
		t.Fatalf("want %s, got %v\n%s", code, err, strings.Join(w.log, "\n"))
	}
}

// The writes stop, the new primary replays the mark, the old primary stops cleanly and releases the lease, the proof is made, and only
// then is the standby promoted (T12).
func TestSwitchoverStopsCleanlyVerifiesThenPromotes(t *testing.T) {
	w := gracefulWorld()
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, gracefulInput()); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, strings.Join(w.log, "\n"))
	}
	mustOrder(t, w, "quiesce s1", "mark s1")
	mustOrder(t, w, "mark s1", "replayed? s2")
	mustOrder(t, w, "replayed? s2", "demote-fast s1")
	mustOrder(t, w, "demote-fast s1", "verify s2")
	mustOrder(t, w, "verify s2", "lease-fence s2")
	mustOrder(t, w, "lease-fence s2", "promote s2")
	mustOrder(t, w, "promote s2", "promote_node kingres_system 4")
	mustOrder(t, w, "promote_node kingres_system 4", "unquiesce s1")
	if w.index("resume") >= 0 || w.index("lease-release") >= 0 {
		t.Errorf("a switchover that completed was rolled back:\n%s", strings.Join(w.log, "\n"))
	}
	if w.index("demote s1") >= 0 {
		t.Error("the old primary was stopped with an immediate stop")
	}
	if g := w.rows.groups[1]; g.State != "done" || g.ChosenStandby != 10 {
		t.Errorf("group row %+v", g)
	}
	if w.rows.outcome != "completed" {
		t.Errorf("outcome %q", w.rows.outcome)
	}
}

// A standby that does not hold what the old primary wrote is never promoted: the proof stays false, the switchover rolls back, the old
// primary is resumed and the writes are let in again (T12b).
func TestSwitchoverRollsBackWhenTheProofStaysFalse(t *testing.T) {
	w := gracefulWorld()
	w.verifyAnswers = []bool{false}
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeStandbyBehind)
	if name != StepName(1, StepVerify) {
		t.Errorf("failed in %q", name)
	}
	if w.index("promote s") >= 0 {
		t.Fatal("promoted")
	}
	mustOrder(t, w, "verify s2", "resume s1")
	if w.index("unquiesce s1") < 0 || w.index("lease-release s2") < 0 {
		t.Errorf("the rollback is incomplete:\n%s", strings.Join(w.log, "\n"))
	}
}

// A proof that settles after a few tries goes on.
func TestSwitchoverProofMayTakeAFewTries(t *testing.T) {
	w := gracefulWorld()
	w.verifyAnswers = []bool{false, false, true}
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, gracefulInput()); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, strings.Join(w.log, "\n"))
	}
	if w.count("verify s2") != 3 {
		t.Errorf("%d proofs", w.count("verify s2"))
	}
}

// KS006: the proof can never be made; the switchover does not turn into a failover.
func TestSwitchoverThatCannotBeProvenRollsBack(t *testing.T) {
	w := gracefulWorld()
	w.verifyErr = &CallError{Status: 409, Code: "unprovable", Message: "KS006"}
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeSwitchoverUnproven)
	if w.index("promote s") >= 0 || w.index("resume s1") < 0 {
		t.Errorf("\n%s", strings.Join(w.log, "\n"))
	}
}

// An old primary whose state is not "shut down" was not stopped cleanly: the new primary is not even asked.
func TestSwitchoverWithAnUncleanStopIsNotVerified(t *testing.T) {
	w := gracefulWorld()
	w.stopState = "in production"
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeSwitchoverUnproven)
	if name != StepName(1, StepStopClean) {
		t.Errorf("failed in %q", name)
	}
	if w.index("verify s2") >= 0 || w.index("promote s") >= 0 {
		t.Errorf("\n%s", strings.Join(w.log, "\n"))
	}
	if w.index("resume s1") < 0 {
		t.Error("the old primary was not resumed")
	}
}

// The state is read again before the proof: a stop that looked clean and does not any more is not verified.
func TestSwitchoverRereadsTheStateBeforeTheProof(t *testing.T) {
	w := gracefulWorld()
	w.stopStates = []string{"shut down", "in production"}
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeSwitchoverUnproven)
	if name != StepName(1, StepVerify) || w.index("verify s2") >= 0 {
		t.Errorf("%s: %s", name, strings.Join(w.log, "; "))
	}
}

// The standby with the most WAL is not chosen when it does not stream from the primary.
func TestSwitchoverSkipsAStandbyThatDoesNotStream(t *testing.T) {
	w := gracefulWorld()
	p := w.pos["s2"]
	p.ReceiverState = "stopped"
	w.pos["s2"] = p
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, gracefulInput()); err != nil {
		t.Fatalf("%s: %v: %s", name, err, strings.Join(w.log, "; "))
	}
	if w.index("promote s3") < 0 || w.index("promote s2") >= 0 {
		t.Errorf("%s", strings.Join(w.log, "; "))
	}
}

// A standby that has not replayed the mark in time stops the switchover before anything is stopped.
func TestSwitchoverStopsNothingWhenTheStandbyHasNotReplayedTheMark(t *testing.T) {
	w := gracefulWorld()
	w.markReplayed = false
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeStandbyBehind)
	if w.index("demote-fast") >= 0 {
		t.Error("the old primary was stopped")
	}
	if w.index("unquiesce s1") < 0 {
		t.Error("the writes were not let in again")
	}
}

// Sessions that cannot be ended end the switchover before anything else happens.
func TestSwitchoverRollsBackWhenTheWritesCannotBeStopped(t *testing.T) {
	w := gracefulWorld()
	w.quiesceFail = true
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeCannotQuiesce)
	if name != StepName(1, StepQuiesce) || w.index("mark") >= 0 || w.index("unquiesce s1") < 0 {
		t.Errorf("%s\n%s", name, strings.Join(w.log, "\n"))
	}
}

// Sessions that end after a few calls are waited for.
func TestSwitchoverWaitsForSessionsToEnd(t *testing.T) {
	w := gracefulWorld()
	w.quiesceCalls = 3
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, gracefulInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if w.count("quiesce s1") != 4 {
		t.Errorf("%d calls", w.count("quiesce s1"))
	}
}

// Prepared transactions that do not end stop the switchover before the old primary is marked.
func TestSwitchoverRollsBackWhenPreparedTransactionsRemain(t *testing.T) {
	w := gracefulWorld()
	w.oldPrepared = 2
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodePreparedPending)
	if name != StepName(1, StepDrain) || w.index("mark") >= 0 {
		t.Errorf("%s\n%s", name, strings.Join(w.log, "\n"))
	}
}

// The refusals of the request.
func TestSwitchoverRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		mod  func(w *world, in *Input)
		code string
	}{
		{"a server that is not live", func(w *world, in *Input) { w.live["s1"] = "suspect" }, "node_state_invalid"},
		{"a primary that does not hold its lease", func(w *world, in *Input) { w.oldHolds = false }, "node_state_invalid"},
		{"a primary that does not run", func(w *world, in *Input) { w.oldWritable = false }, "node_state_invalid"},
		{"no standby streams", func(w *world, in *Input) {
			for _, s := range []string{"s2", "s3"} {
				p := w.pos[s]
				p.ReceiverState = "stopped"
				w.pos[s] = p
			}
		}, "group_degraded"},
		{"the named target does not stream", func(w *world, in *Input) {
			p := w.pos["s3"]
			p.ReceiverState = ""
			w.pos["s3"] = p
			in.Targets = map[int32]string{1: "s3"}
		}, "group_degraded"},
		{"the map group", func(w *world, in *Input) { in.Groups = []int32{0, 1}; w.nodes[0].Host = "s1" }, "unsupported_operation"},
	} {
		w := gracefulWorld()
		in := gracefulInput()
		c.mod(w, &in)
		r := w.runner()
		ref := r.Preflight(context.Background(), in)
		if ref == nil || ref.Code != c.code {
			t.Errorf("%s: %+v", c.name, ref)
		}
	}
	w := gracefulWorld()
	if ref := w.runner().Preflight(context.Background(), gracefulInput()); ref != nil {
		t.Errorf("a healthy switchover is refused: %+v", ref)
	}
}

// The switchover goes to the named standby.
func TestSwitchoverGoesToTheNamedTarget(t *testing.T) {
	w := gracefulWorld()
	r := w.runner()
	w.fakeClock(r)
	in := gracefulInput()
	in.Targets = map[int32]string{1: "s3"}
	if name, err := runAll(t, r, in); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, strings.Join(w.log, "\n"))
	}
	if w.index("promote s3") < 0 || w.index("promote s2") >= 0 {
		t.Errorf("\n%s", strings.Join(w.log, "\n"))
	}
}

// A switchover cannot be cancelled once the promotion has begun.
func TestSwitchoverIsCancellableOnlyBeforeThePromotion(t *testing.T) {
	steps := func(promote string) task.Task {
		return task.Task{State: task.Running, Steps: []task.StepState{
			{Name: StepName(1, StepStopClean), State: task.StepDone},
			{Name: StepName(1, StepPromote), State: promote},
		}}
	}
	if !Cancellable(steps(task.StepPending)) {
		t.Error("a switchover before its promotion must be cancellable")
	}
	if Cancellable(steps(task.StepRunning)) {
		t.Error("a switchover whose promotion has begun must not be cancellable")
	}
}

// A cancel rolls back the groups that were begun and not promoted, and only those.
func TestOnFailRollsBackASwitchoverThatWasNotPromoted(t *testing.T) {
	w := gracefulWorld()
	r := w.runner()
	in := gracefulInput()
	raw, _ := json.Marshal(in)
	w.rows.groups[1] = &GroupRow{Group: 1, State: "pinned", NewServer: "s2"}
	r.OnFail(nil)(context.Background(), task.Task{ID: 1, Input: raw, Error: &task.Error{Code: task.CodeCanceled, Message: "cancelled"}})
	if w.index("resume s1") < 0 || w.index("unquiesce s1") < 0 {
		t.Errorf("\n%s", strings.Join(w.log, "\n"))
	}
	w2 := gracefulWorld()
	r2 := w2.runner()
	w2.rows.groups[1] = &GroupRow{Group: 1, State: "promoted", NewServer: "s2"}
	r2.OnFail(nil)(context.Background(), task.Task{ID: 1, Input: raw, Error: &task.Error{Message: "failed"}})
	if w2.index("resume") >= 0 {
		t.Errorf("a promoted group was rolled back:\n%s", strings.Join(w2.log, "\n"))
	}
}
