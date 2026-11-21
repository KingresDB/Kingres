// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

// F4-03. A promote step that began and failed (the call did not answer) is past the point of no return: the standby may be promoted, so
// the hook neither resumes the old primary nor releases a lease. A step before the promotion is rolled back as before.
func TestOnFailDoesNotRollBackAGroupWhoseFailedStepIsFromThePromotionOn(t *testing.T) {
	raw, _ := json.Marshal(gracefulInput())
	for _, c := range []struct {
		step     string
		rollback bool
	}{
		{StepName(1, StepVerify), true},
		{StepName(1, StepHandOver), true},
		{StepName(1, StepPromote), false},
		{StepName(1, StepCatalog), false},
		{StepName(2, StepPromote), true}, // another group's step says nothing about group 1
	} {
		w := gracefulWorld()
		r := w.runner()
		w.rows.groups[1] = &GroupRow{Group: 1, State: "pinned", NewServer: "s2"}
		r.OnFail(nil)(context.Background(), task.Task{ID: 1, Input: raw, Error: &task.Error{Code: CodePromoteTimeout, Step: c.step, Message: "x"}})
		got := w.index("resume s1") >= 0 || w.index("unquiesce s1") >= 0
		if got != c.rollback {
			t.Errorf("failed in %q: rolled back %v, want %v\n%s", c.step, got, c.rollback, strings.Join(w.log, "; "))
		}
	}
}

// F4-03, end to end: the promote call does not answer three times, the task fails, and the hook leaves the old primary stopped.
func TestAPromoteThatDoesNotAnswerIsNotRolledBack(t *testing.T) {
	w := gracefulWorld()
	for i := 0; i < 3; i++ {
		w.promoteErr["s2#"+string(rune('0'+i))] = ErrUnreachable
	}
	r := w.runner()
	w.fakeClock(r)
	in := gracefulInput()
	name, err := runAll(t, r, in)
	failedWith(t, w, err, CodePromoteTimeout)
	if name != StepName(1, StepPromote) {
		t.Fatalf("failed in %q", name)
	}
	raw, _ := json.Marshal(in)
	r.OnFail(nil)(context.Background(), task.Task{ID: 1, Input: raw, Error: &task.Error{Code: CodePromoteTimeout, Step: name}})
	if w.index("resume") >= 0 || w.index("unquiesce s1") >= 0 {
		t.Errorf("the old primary was resumed after a promote that may have taken effect:\n%s", strings.Join(w.log, "\n"))
	}
}

// F4-07. The call that promotes carries the shutdown checkpoint and the state, so that the callee makes the proof again; a failover that
// is not a switchover carries none.
func TestThePromoteCallOfASwitchoverCarriesTheProof(t *testing.T) {
	w := gracefulWorld()
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, gracefulInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(w.promoteReqs) == 0 || w.promoteReqs[0].Checkpoint != "0/6000028" || w.promoteReqs[0].ClusterState != "shut down" {
		t.Fatalf("%+v", w.promoteReqs)
	}
	w2 := newWorld()
	r2 := w2.runner()
	w2.fakeClock(r2)
	if name, err := runAll(t, r2, hardInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(w2.promoteReqs) == 0 || w2.promoteReqs[0].Checkpoint != "" {
		t.Fatalf("a failover carried a checkpoint: %+v", w2.promoteReqs)
	}
}

// F4-07. A callee that refuses the proof at the promotion (the old primary wrote more after the first proof, or the standby is behind)
// has promoted nothing: the switchover rolls back, with the code of the refusal.
func TestAProofRefusedAtThePromotionRollsBack(t *testing.T) {
	for _, c := range []struct {
		refuse *CallError
		code   string
	}{
		{&CallError{Status: 409, Code: "unprovable", Message: "KS006"}, CodeSwitchoverUnproven},
		{&CallError{Status: 409, Code: "standby_behind", Message: "behind"}, CodeStandbyBehind},
	} {
		w := gracefulWorld()
		w.promoteRefuse = c.refuse
		r := w.runner()
		w.fakeClock(r)
		name, err := runAll(t, r, gracefulInput())
		failedWith(t, w, err, c.code)
		if name != StepName(1, StepPromote) {
			t.Errorf("failed in %q", name)
		}
		if w.index("promote s2") >= 0 && w.index("promote-refused-proof") < 0 {
			t.Error("promoted")
		}
		if w.index("resume s1") < 0 || w.index("unquiesce s1") < 0 {
			t.Errorf("the refusal was not rolled back:\n%s", strings.Join(w.log, "\n"))
		}
	}
}

// F4-08. The steps before the promotion end when a cancel arrives; the promotion and the steps after it never do.
func TestOnlyTheStepsBeforeThePromotionAreInterruptible(t *testing.T) {
	w := gracefulWorld()
	r := w.runner()
	raw, _ := json.Marshal(gracefulInput())
	steps, err := r.steps(raw)
	if err != nil {
		t.Fatal(err)
	}
	past := false
	for _, s := range steps {
		if s.Name == StepName(1, StepPromote) {
			past = true
		}
		if s.Name == StepCheck || s.Name == StepWriteReport {
			if s.Interruptible {
				t.Errorf("%q is interruptible", s.Name)
			}
			continue
		}
		if s.Interruptible == past {
			t.Errorf("%q: interruptible %v, past the promotion %v", s.Name, s.Interruptible, past)
		}
	}
}

// F4-17. The rules that no test pinned: the stop must release the lease; a failure of the mark that is not "no primary here" and a stale
// term at the stop roll back; the mark and the stop carry the term of the run; a standby that is not ready is not a candidate; the rollback
// releases the lease of the new server when it is not the leader's; an unknown count of prepared transactions is not a reason to wait.
func TestAStopThatDoesNotReleaseTheLeaseIsNotClean(t *testing.T) {
	w := gracefulWorld()
	w.notReleased = true
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeSwitchoverUnproven)
	if name != StepName(1, StepStopClean) || w.index("promote s") >= 0 {
		t.Errorf("%s\n%s", name, strings.Join(w.log, "\n"))
	}
}

func TestAFailureOfTheMarkRollsBack(t *testing.T) {
	w := gracefulWorld()
	w.markErr = errors.New("the instance cannot be asked")
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeStandbyBehind)
	if name != StepName(1, StepStopClean) || w.index("demote-fast") >= 0 || w.index("resume s1") < 0 {
		t.Errorf("%s\n%s", name, strings.Join(w.log, "\n"))
	}
}

func TestAStaleTermAtTheStopIsSupersededAndRollsBack(t *testing.T) {
	w := gracefulWorld()
	w.stopErr = &CallError{Status: 409, Code: "stale_term", Message: "newer"}
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeSuperseded)
	if w.index("resume s1") < 0 || w.index("promote s") >= 0 {
		t.Errorf("\n%s", strings.Join(w.log, "\n"))
	}
}

func TestTheMarkAndTheStopCarryTheTermOfTheRun(t *testing.T) {
	w := gracefulWorld()
	want := w.term // the run is term is the one it began with: the promotion raises it
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, gracefulInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(w.markTerms) == 0 || len(w.stopTerms) == 0 {
		t.Fatalf("mark %v stop %v", w.markTerms, w.stopTerms)
	}
	for _, x := range append(append([]int64(nil), w.markTerms...), w.stopTerms...) {
		if x != want {
			t.Errorf("a call carried the term %d, the run's is %d", x, want)
		}
	}
}

func TestAStandbyThatIsNotReadyIsNotACandidate(t *testing.T) {
	w := gracefulWorld()
	w.standbys[0].State = "rebuilding" // s2 has the most WAL
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, gracefulInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if w.index("promote s3") < 0 || w.index("promote s2") >= 0 {
		t.Errorf("\n%s", strings.Join(w.log, "\n"))
	}
}

func TestTheRollbackReleasesTheLeaseOfTheNewServer(t *testing.T) {
	w := gracefulWorld()
	w.verifyAnswers = []bool{false}
	in := gracefulInput()
	in.Targets = map[int32]string{1: "s3"} // not the leader's server
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, in)
	failedWith(t, w, err, CodeStandbyBehind)
	if w.index("lease-release s3") < 0 {
		t.Errorf("the lease of the new server was not released:\n%s", strings.Join(w.log, "\n"))
	}
}

func TestAnUnknownCountOfPreparedTransactionsIsNotWaitedFor(t *testing.T) {
	w := gracefulWorld()
	w.oldPreparedUnknown = true
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, gracefulInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if i := w.index("recover-prepared kingres_system"); i >= 0 && i < w.index("mark s1") {
		t.Error("the drain asked for a recovery although nothing says that a transaction is prepared")
	}
}

// F4-15. A target names a group of the request, also when the request names no groups (all the groups of the server).
func TestATargetForAGroupTheServerDoesNotRunIsRefused(t *testing.T) {
	w := gracefulWorld()
	s := &Service{Runner: w.runner()}
	_, err := s.Resolve(context.Background(), "t", "s1", Request{Mode: ModeHard, Targets: []TargetRequest{{ShardGroupID: 2, ServerID: "s2"}}})
	if err == nil {
		t.Fatal("a target for a group that the server does not run was accepted")
	}
	if _, err := s.Resolve(context.Background(), "t", "s1", Request{Mode: ModeHard, Targets: []TargetRequest{{ShardGroupID: 1, ServerID: "s2"}}}); err != nil {
		t.Fatalf("a target for a group the server runs: %v", err)
	}
}

// F4-09. The calls that stop the writes and the calls that undo it carry the term of the run, so that a callee that has seen a newer term
// refuses them (they cannot carry the fence of a task run: the repair of a switchover has none).
func TestTheUndoCallsCarryTheTermOfTheRun(t *testing.T) {
	w := gracefulWorld()
	want := w.term
	w.verifyAnswers = []bool{false}
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeStandbyBehind)
	if len(w.undoTerms) < 3 {
		t.Fatalf("quiesce, resume and unquiesce were not all called: %v", w.undoTerms)
	}
	for _, x := range w.undoTerms {
		if x != want {
			t.Errorf("a call carried the term %d, the run's is %d (%v)", x, want, w.undoTerms)
		}
	}
}

// F4-02(c). A switchover that loses the majority after the old primary was stopped does not pause with the group down: it goes back through
// the path of a failure (the writes are let in again, the old primary resumed), a plain failover still pauses.
func TestASwitchoverWithoutAMajorityIsRolledBackNotPaused(t *testing.T) {
	w := gracefulWorld()
	r := w.runner()
	w.fakeClock(r)
	raw, _ := json.Marshal(gracefulInput())
	steps, err := r.steps(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Name == StepName(1, StepLease) {
			w.live["s1"], w.live["s3"] = "dead", "dead" // the leader (s2) hears none of the others now
			runErr := s.Run(context.Background(), &task.Env{Task: task.Task{ID: 1, Input: raw}})
			failedWith(t, w, runErr, CodeQuorumUnavailable)
			break
		}
		if err := s.Run(context.Background(), &task.Env{Task: task.Task{ID: 1, Input: raw}}); err != nil {
			var sk *task.SkipError
			if !errors.As(err, &sk) {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
	if w.index("resume s1") < 0 || w.index("unquiesce s1") < 0 || w.index("promote s") >= 0 {
		t.Errorf("\n%s", strings.Join(w.log, "\n"))
	}
}

// mapWorld is gracefulWorld for the map group: the groups of the nodes, the standbys and the positions are 0, and the server that runs the
// tasks is the old leader's own (the map group is handed over by the leader's task).
func mapWorld() *world {
	w := gracefulWorld()
	w.mapGroup, w.self = true, "s1"
	for i := range w.nodes {
		if w.nodes[i].Group == 1 {
			w.nodes[i].Group = 0
		}
	}
	for i := range w.standbys {
		w.standbys[i].Group = 0
	}
	for s, p := range w.pos {
		p.Group = 0
		w.pos[s] = p
	}
	return w
}

func mapGracefulInput() Input {
	return Input{Mode: ModeGraceful, ServerID: "s1", Groups: []int32{0}, Trigger: "manual", RequestedBy: "tester", Comment: "map"}
}

// The map group is handed over by the old leader's task: the bootstrap of the candidate is started BEFORE the old primary is stopped (it waits for the
// lease that the clean stop releases), and the proof of the stop is sent to it after.
func TestAMapGroupSwitchoverStartsTheBootstrapBeforeTheStopAndSendsTheProofAfter(t *testing.T) {
	w := mapWorld()
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, mapGracefulInput()); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, strings.Join(w.log, "\n"))
	}
	start, mark, demote, proof := w.index("map-start s2"), w.index("mark s1"), w.index("demote-fast s1"), w.index("map-proof s2")
	if start < 0 || mark < 0 || demote < 0 || proof < 0 || !(start < mark && mark < demote && demote < proof) {
		t.Fatalf("the order of the hand-over (start %d, mark %d, demote %d, proof %d):\n%s", start, mark, demote, proof, strings.Join(w.log, "\n"))
	}
	if len(w.mapStarts) != 1 || w.mapStarts[0].Planned == nil || w.mapStarts[0].Planned.StandbyID != 10 || w.mapStarts[0].Planned.TaskID != 1 ||
		w.mapStarts[0].Server != "s1" || w.mapStarts[0].AllowDataLoss {
		t.Fatalf("the request of the bootstrap: %+v", w.mapStarts)
	}
	if len(w.mapProofs) != 1 || w.mapProofs[0].Checkpoint == "" || w.mapProofs[0].ClusterState != "shut down" {
		t.Fatalf("the proof: %+v", w.mapProofs)
	}
}

// The new leader's engine runs the steps that the old leader could not record: the standby is promoted already, so the steps that act on the old
// leader or on the lease skip, and nothing starts a second bootstrap.
func TestTheNewLeaderRunsAgainWhatTheOldLeaderCouldNotRecord(t *testing.T) {
	w := mapWorld()
	r := w.runner()
	w.fakeClock(r)
	in := mapGracefulInput()
	raw, _ := json.Marshal(in)
	steps, err := r.steps(raw)
	if err != nil {
		t.Fatal(err)
	}
	mark := -1
	for _, s := range steps {
		if mark >= 0 && s.Name == StepName(0, StepVerify) {
			break // the steps of the old leader are over: what follows is the new leader's run
		}
		env := &task.Env{Task: task.Task{ID: 1, Input: raw}}
		if err := s.Run(context.Background(), env); err != nil {
			var sk *task.SkipError
			if !errors.As(err, &sk) {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
		if s.Name == StepName(0, StepStopClean) {
			// the old leader stopped; the bootstrap of the candidate promoted the standby and holds the lease
			w.promoted["s2"], w.lease["s2"] = true, true
			mark = len(w.log)
		}
	}
	// The new leader's engine: every step of the map group that is not recorded runs again, the stop first.
	for _, s := range steps {
		if s.Name != StepName(0, StepStopClean) && s.Name != StepName(0, StepVerify) && s.Name != StepName(0, StepLease) && s.Name != StepName(0, StepHandOver) && s.Name != StepName(0, StepPromote) {
			continue
		}
		env := &task.Env{Task: task.Task{ID: 1, Input: raw}}
		err := s.Run(context.Background(), env)
		var sk *task.SkipError
		if !errors.As(err, &sk) {
			t.Errorf("%s did not skip on the new leader: %v", s.Name, err)
		}
	}
	after := strings.Join(w.log[mark:], "\n")
	for _, c := range []string{"map-start", "mark ", "demote", "map-proof", "lease-fence"} {
		if strings.Contains(after, c) {
			t.Errorf("a re-run on the new leader made %q:\n%s", c, after)
		}
	}
}

// A failure before the stop gives the bootstrap up and resumes the old leader.
func TestAFailedMapGroupSwitchoverGivesTheBootstrapUp(t *testing.T) {
	w := mapWorld()
	w.mapStartErr = errors.New("the candidate cannot start")
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, mapGracefulInput())
	if err == nil || name != StepName(0, StepStopClean) {
		t.Fatalf("%s: %v", name, err)
	}
	if w.count("map-abandon s2") != 1 || w.count("resume s1") != 1 {
		t.Fatalf("the rollback:\n%s", strings.Join(w.log, "\n"))
	}
	if w.count("demote ") != 0 {
		t.Fatal("the old leader was stopped although the bootstrap did not start")
	}
}

// The map group is the last group of a switchover or a restore.
func TestTheMapGroupIsTheLastGroupOfAPlannedSwitchover(t *testing.T) {
	in := Input{Mode: ModeGraceful, Groups: []int32{0, 2, 5}}
	if got := mapLast(in); len(got) != 3 || got[0] != 2 || got[1] != 5 || got[2] != 0 {
		t.Fatalf("%v", got)
	}
	hard := Input{Mode: ModeHard, Groups: []int32{0, 2, 5}}
	if got := mapLast(hard); got[0] != 0 {
		t.Fatalf("a hard failover keeps its order: %v", got)
	}
}

// The preflight: the map group is accepted for a planned switchover of the server that runs the task, refused for a hard failover of a live
// server, and refused when the request is not made on the leader's own server.
func TestThePreflightOfTheMapGroup(t *testing.T) {
	w := mapWorld()
	r := w.runner()
	if ref := r.Preflight(context.Background(), mapGracefulInput()); ref != nil {
		t.Fatalf("a planned switchover of the map group: %+v", ref)
	}
	hard := mapGracefulInput()
	hard.Mode = ModeHard
	if ref := r.Preflight(context.Background(), hard); ref == nil || ref.Code != "unsupported_operation" {
		t.Fatalf("a hard failover of a live leader's server: %+v", ref)
	}
	w.self = "s3"
	if ref := w.runner().Preflight(context.Background(), mapGracefulInput()); ref == nil || ref.Code != "unsupported_operation" {
		t.Fatalf("not on the leader's server: %+v", ref)
	}
}

// The standby refuses the proof after the old leader stopped: the candidate reports it, the old leader's task rolls back, the old leader is resumed
// (its lease manager starts the primary, the candidate let go of the lease) and the writes are let in again; nothing was promoted.
func TestARefusedProofAfterTheStopRollsTheMapGroupSwitchoverBack(t *testing.T) {
	w := mapWorld()
	w.mapRefusal = "the standby does not hold everything the old primary wrote"
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, mapGracefulInput())
	failedWith(t, w, err, CodeSwitchoverUnproven)
	if name != StepName(0, StepStopClean) {
		t.Fatalf("the step: %s", name)
	}
	if w.count("resume s1") != 1 || w.count("unquiesce s1") < 1 || w.count("map-abandon s2") != 1 {
		t.Fatalf("the rollback:\n%s", strings.Join(w.log, "\n"))
	}
	if w.count("promote s2") != 0 || w.promoted["s2"] {
		t.Fatal("the standby was promoted although the proof was refused")
	}
}

// A candidate that says nothing is waited for, then the task rolls back as well (the lease keeps the two from running together).
func TestACandidateThatNeverAnswersEndsTheMapGroupSwitchoverBySilence(t *testing.T) {
	w := mapWorld()
	w.dbDownAfterStop = true // as in the real system: the rows of the task are in the stopped instance (F5-01)
	w.mapRefusal = ""
	r := w.runner()
	w.fakeClock(r)
	r.MapVerdictWait = 3 * time.Second
	// the fake candidate does not promote: this world answers State with the standby still in recovery
	w.noPromoteOnProof = true
	_, err := runAll(t, r, mapGracefulInput())
	failedWith(t, w, err, CodeSwitchoverUnproven)
	if w.count("resume s1") != 1 {
		t.Fatalf("the old leader was not resumed:\n%s", strings.Join(w.log, "\n"))
	}
}

// F5-01. The clean stop did not release the lease at a majority (the old leader's renewal is back): the task rolls back with the rows unreadable and
// still lets go of the candidate's lease, gives its bootstrap up, and resumes the old leader.
func TestARollbackAfterAStopThatDidNotReleaseTheLeaseStillReleasesTheCandidate(t *testing.T) {
	w := mapWorld()
	w.dbDownAfterStop, w.notReleased = true, true
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, mapGracefulInput())
	failedWith(t, w, err, CodeSwitchoverUnproven)
	if name != StepName(0, StepStopClean) {
		t.Fatalf("the step: %s", name)
	}
	if w.count("lease-release s2") != 1 || w.count("map-abandon s2") != 1 || w.count("resume s1") != 1 {
		t.Fatalf("the rollback with unreadable rows:\n%s", strings.Join(w.log, "\n"))
	}
}

// F5-03. The rollback asks the candidate first: when it answers that it promoted (or may have), nothing is rolled back: no lease is released and the old
// leader is not resumed; the step ends as the hand-over it is. In an ordinary rollback the candidate is asked before the leases are released.
func TestARollbackThatMeetsAPromotedCandidateLeavesTheOldLeaderHeld(t *testing.T) {
	w := mapWorld()
	w.dbDownAfterStop = true
	w.mapRefusal = "the standby could not prove it"
	w.mapAbandonErr = &CallError{Status: 409, Code: "conflict", Message: "it promoted"}
	r := w.runner()
	w.fakeClock(r)
	raw, _ := json.Marshal(mapGracefulInput())
	steps, serr := r.steps(raw)
	if serr != nil {
		t.Fatal(serr)
	}
	for _, s := range steps {
		err := s.Run(context.Background(), &task.Env{Task: task.Task{ID: 1, Input: raw}})
		var sk *task.SkipError
		if err != nil && !errors.As(err, &sk) {
			t.Fatalf("%s should end as a hand-over: %v\n%s", s.Name, err, strings.Join(w.log, "\n"))
		}
		if s.Name == StepName(0, StepStopClean) {
			break // the old leader's task ends here: its database is stopped
		}
	}
	if w.count("map-abandon s2") != 1 || w.count("resume s1") != 0 || w.count("lease-release") != 0 || w.count("unquiesce s1") != 0 {
		t.Fatalf("a rollback ran although the candidate promoted:\n%s", strings.Join(w.log, "\n"))
	}
}

func TestTheRollbackAsksTheCandidateBeforeItReleasesTheLeasesAndResumes(t *testing.T) {
	w := mapWorld()
	w.dbDownAfterStop = true
	w.mapRefusal = "refused"
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, mapGracefulInput())
	failedWith(t, w, err, CodeSwitchoverUnproven)
	a, l, rs := w.index("map-abandon s2"), w.index("lease-release"), w.index("resume s1")
	if a < 0 || l < 0 || rs < 0 || !(a < l && a < rs) {
		t.Fatalf("the order (abandon %d, release %d, resume %d):\n%s", a, l, rs, strings.Join(w.log, "\n"))
	}
}

// F5-06. On the new leader's engine, a state that cannot be read is not "the map group was not handed over": the step fails and is retried, and nothing is
// started, stopped or rolled back.
func TestAStateThatCannotBeReadIsNotReadAsNotHandedOver(t *testing.T) {
	w := mapWorld()
	r := w.runner()
	w.fakeClock(r)
	raw, _ := json.Marshal(mapGracefulInput())
	steps, err := r.steps(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Name == StepName(0, StepStopClean) {
			break
		}
		if err := s.Run(context.Background(), &task.Env{Task: task.Task{ID: 1, Input: raw}}); err != nil {
			var sk *task.SkipError
			if !errors.As(err, &sk) {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
	// the promotion happened elsewhere and this server cannot read the state of the standby now
	w.promoted["s2"], w.lease["s2"] = true, true
	w.stateDown = map[string]bool{"s2": true}
	mark := len(w.log)
	for _, s := range steps {
		if s.Name != StepName(0, StepStopClean) {
			continue
		}
		err := s.Run(context.Background(), &task.Env{Task: task.Task{ID: 1, Input: raw}})
		var sk *task.SkipError
		if err == nil || errors.As(err, &sk) {
			t.Fatalf("the step went on with a state that could not be read: %v", err)
		}
	}
	after := strings.Join(w.log[mark:], "\n")
	for _, c := range []string{"map-start", "mark ", "demote", "resume", "unquiesce", "lease-release", "map-abandon"} {
		if strings.Contains(after, c) {
			t.Errorf("a failed read started or undid something (%s):\n%s", c, after)
		}
	}
}

// A rollback is never made for a group whose row says promoted.
func TestAGroupThatWasPromotedIsNeverRolledBack(t *testing.T) {
	w := gracefulWorld()
	r := w.runner()
	w.fakeClock(r)
	w.stopState = "in production" // the stop is not clean: the step would roll back
	raw, _ := json.Marshal(gracefulInput())
	steps, _ := r.steps(raw)
	for _, s := range steps {
		if s.Name == StepName(1, StepStopClean) {
			w.rows.mu.Lock()
			w.rows.groups[1].State = "promoted"
			w.rows.mu.Unlock()
			mark := len(w.log)
			err := s.Run(context.Background(), &task.Env{Task: task.Task{ID: 1, Input: raw}})
			failedWith(t, w, err, CodeSwitchoverUnproven)
			if after := strings.Join(w.log[mark:], "\n"); strings.Contains(after, "resume") || strings.Contains(after, "lease-release") {
				t.Fatalf("a promoted group was rolled back:\n%s", after)
			}
			return
		}
		if err := s.Run(context.Background(), &task.Env{Task: task.Task{ID: 1, Input: raw}}); err != nil {
			var sk *task.SkipError
			if !errors.As(err, &sk) {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
	t.Fatal("no stop step")
}

// F5-10. The short wait for the old server's answers is the map group's of a planned switchover; every other group and mode keeps the long wait.
func TestOnlyTheMapGroupOfAPlannedSwitchoverBoundsTheCallsToTheOldServer(t *testing.T) {
	ctx := context.Background()
	has := func(sc *stepCtx) bool {
		c, cancel := sc.oldCtx(ctx)
		defer cancel()
		_, ok := c.Deadline()
		return ok
	}
	r := &Runner{}
	if !has(&stepCtx{r: r, in: Input{Mode: ModeGraceful}, group: 0}) {
		t.Error("the map group of a planned switchover is not bounded")
	}
	if !has(&stepCtx{r: r, in: Input{Mode: ModeRestore}, group: 0}) {
		t.Error("the map group of a restore is not bounded")
	}
	for _, c := range []struct {
		name string
		sc   *stepCtx
	}{
		{"an ordinary group of a switchover", &stepCtx{r: r, in: Input{Mode: ModeGraceful}, group: 2}},
		{"an ordinary group of a hard failover", &stepCtx{r: r, in: Input{Mode: ModeHard}, group: 2}},
		{"the map group of a hard failover", &stepCtx{r: r, in: Input{Mode: ModeHard}, group: 0}},
	} {
		if has(c.sc) {
			t.Errorf("%s is bounded: the fence of last resort would be skipped after 15 s", c.name)
		}
	}
}

// runAllCancelledAtTheStop runs the steps like runAll, with a context that the clean stop cancels (the engine cancels the context of a step whose task lease
// cannot be renewed, which is what the stopped system database of the map group does ten seconds after the stop).
func runAllCancelledAtTheStop(t *testing.T, w *world, r *Runner, in Input) (string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.afterStop = cancel
	raw, _ := json.Marshal(in)
	steps, err := r.steps(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		err := s.Run(ctx, &task.Env{Task: task.Task{ID: 1, Input: raw}})
		var sk *task.SkipError
		if errors.As(err, &sk) {
			continue
		}
		if err != nil {
			return s.Name, err
		}
	}
	return "", nil
}

// The cluster tests of the review of round 2 found it: the context of the step ends soon after the clean stop of the map group, and a step that ends with it
// leaves the old primary stopped and held back, with nobody to roll back. A candidate that never promotes, and a proof that cannot be sent, roll back all the same.
func TestTheRollbackOfTheMapGroupOutlivesTheContextOfTheStep(t *testing.T) {
	w := mapWorld()
	w.dbDownAfterStop, w.noPromoteOnProof = true, true
	r := w.runner()
	w.fakeClock(r)
	r.MapVerdictWait = 3 * time.Second
	_, err := runAllCancelledAtTheStop(t, w, r, mapGracefulInput())
	failedWith(t, w, err, CodeSwitchoverUnproven)
	if w.count("resume s1") != 1 || w.count("map-abandon s2") != 1 || w.count("lease-release s2") != 1 {
		t.Fatalf("no rollback after the context ended:\n%s", strings.Join(w.log, "\n"))
	}
}

func TestAProofThatCannotBeSentRollsTheMapGroupBack(t *testing.T) {
	w := mapWorld()
	w.dbDownAfterStop = true
	w.mapProofErr = &CallError{Status: 409, Code: "conflict", Message: "No planned switchover of the map group is in progress on this server."}
	r := w.runner()
	w.fakeClock(r)
	_, err := runAllCancelledAtTheStop(t, w, r, mapGracefulInput())
	failedWith(t, w, err, CodeTargetUnavailable)
	if w.count("resume s1") != 1 || w.count("map-abandon s2") != 1 {
		t.Fatalf("no rollback after the proof failed:\n%s", strings.Join(w.log, "\n"))
	}
}

// A run of the step after the switchover was given up (its rollback could not be recorded) ends as the rollback it was: switchover_unproven, with the
// old leader resumed, and no second switchover begins.
func TestAMapGroupSwitchoverThatWasGivenUpEndsAsUnprovenWhenTheStepRunsAgain(t *testing.T) {
	w := mapWorld()
	w.mapStartErr = &CallError{Status: 409, Code: "conflict", Message: "The planned switchover of this task was given up; it is not started again."}
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, mapGracefulInput())
	failedWith(t, w, err, CodeSwitchoverUnproven)
	if w.count("demote-fast s1") != 0 || w.count("resume s1") != 1 {
		t.Fatalf("a second switchover began or the old leader was not resumed:\n%s", strings.Join(w.log, "\n"))
	}
}
