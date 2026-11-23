// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

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

// The tests of the hard failover of the map group while its server is alive (design ha-phase5-part2.md, "a hard failover of a live leader's server").

func mapHardInput() Input {
	return Input{Mode: ModeHard, ServerID: "s1", Groups: []int32{0}, Trigger: "manual", RequestedBy: "tester", Comment: "map", MapLive: true}
}

// runThrough runs the steps in order up to and including the step with this name; the steps of a skip are passed over.
func runThrough(t *testing.T, r *Runner, in Input, last string) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	steps, err := r.steps(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		err := s.Run(context.Background(), &task.Env{Task: task.Task{ID: 1, Input: raw}})
		var sk *task.SkipError
		if err != nil && !errors.As(err, &sk) {
			return s.Name, err
		}
		if s.Name == last {
			return "", nil
		}
	}
	return "", nil
}

// The order that makes the failover safe: the bootstrap of the candidate starts first (it waits for the lease), then the old primary is fenced by one
// call (no renewal, the marker, an immediate stop, the lease handed to the candidate). Nothing is quiesced, marked or stopped cleanly: a hard failover does
// not wait for sessions or for a proof.
func TestAHardFailoverOfTheMapGroupStartsTheBootstrapThenFencesTheOldPrimary(t *testing.T) {
	w := mapWorld()
	r := w.runner()
	w.fakeClock(r)
	if name, err := runThrough(t, r, mapHardInput(), StepName(0, StepStop)); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, strings.Join(w.log, "\n"))
	}
	start, demote := w.index("map-start s2"), w.index("demote s1")
	if start < 0 || demote < 0 || start > demote {
		t.Fatalf("the bootstrap must start before the old primary is fenced (start %d, demote %d):\n%s", start, demote, strings.Join(w.log, "\n"))
	}
	for _, c := range []string{"quiesce", "mark ", "demote-fast", "map-proof", "lease-fence"} {
		if w.index(c) >= 0 {
			t.Errorf("a hard failover made %q:\n%s", c, strings.Join(w.log, "\n"))
		}
	}
	if len(w.mapStarts) != 1 {
		t.Fatalf("starts: %+v", w.mapStarts)
	}
	p := w.mapStarts[0].Planned
	if p == nil || !p.Hard || p.StandbyID != 10 || p.TaskID != 1 || p.Facts == nil || !p.Facts.SyncStreaming || w.mapStarts[0].Server != "s1" {
		t.Fatalf("the request of the bootstrap: %+v %+v", w.mapStarts[0], p)
	}
	if len(w.immediate) != 1 {
		t.Fatalf("immediate stops: %+v", w.immediate)
	}
	d := w.immediate[0]
	if d.Group != 0 || d.Mode != "immediate" || d.TransferTo != "s2" || d.Candidate != "s2" || d.ExpectedTerm != 5 {
		t.Fatalf("the fencing call: %+v", d)
	}
}

// Nothing is changed before the loss is judged: a verdict that the operator did not accept stops the task with the old leader running. With the
// acceptance of the operator the same request goes on, and the candidate gets it.
func TestAHardFailoverOfTheMapGroupJudgesTheLossBeforeAnythingIsStopped(t *testing.T) {
	w := mapWorld()
	w.facts.SyncStreaming = false // the group streamed no synchronous standby: commits may be lost
	r := w.runner()
	w.fakeClock(r)
	name, err := runThrough(t, r, mapHardInput(), StepName(0, StepStop))
	var pe *PauseError
	if !errors.As(err, &pe) || name != StepName(0, StepStop) {
		t.Fatalf("want a pause at the stop: %s %v", name, err)
	}
	if w.index("map-start") >= 0 || w.index("demote") >= 0 {
		t.Fatalf("something was started or stopped before the loss was accepted:\n%s", strings.Join(w.log, "\n"))
	}
	in := mapHardInput()
	in.AllowDataLoss = true
	w2 := mapWorld()
	w2.facts.SyncStreaming = false
	r2 := w2.runner()
	w2.fakeClock(r2)
	if name, err := runThrough(t, r2, in, StepName(0, StepStop)); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, strings.Join(w2.log, "\n"))
	}
	if len(w2.mapStarts) != 1 || !w2.mapStarts[0].AllowDataLoss || w2.mapStarts[0].Planned.Facts.SyncStreaming {
		t.Fatalf("the acceptance and the facts must reach the candidate: %+v", w2.mapStarts)
	}
}

// The facts that the candidate judges with are the leader's, as of the stop: a record of the last check that is older than the bound says "no
// synchronous standby" whatever it holds, and the candidate must not be told that one streamed.
func TestTheFactsOfAHardFailoverAreAsOfTheStop(t *testing.T) {
	w := mapWorld()
	w.facts.LastKnownAge = time.Hour
	in := mapHardInput()
	in.AllowDataLoss = true
	r := w.runner()
	w.fakeClock(r)
	if name, err := runThrough(t, r, in, StepName(0, StepStop)); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if f := w.mapStarts[0].Planned.Facts; f == nil || f.SyncStreaming || f.PrimaryPlanned != 1 {
		t.Fatalf("facts %+v", f)
	}
}

// The step does not end with the context of the task: the system database of the old leader is the instance it just stopped, its task lease cannot be renewed
// and the engine cancels the step ten seconds later; the step waits for the candidate to promote all the same.
func TestTheStopOfAHardFailoverOfTheMapGroupOutlivesTheContextOfTheStep(t *testing.T) {
	w := mapWorld()
	w.dbDownAfterStop = true
	w.hardLate = 3
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAllCancelledAtTheStop(t, w, r, func() Input { in := mapHardInput(); return in }()); err != nil {
		// the steps after the stop run on the new leader; only the stop step is the subject
		if name == StepName(0, StepStop) {
			t.Fatalf("the stop ended with the context: %v\n%s", err, strings.Join(w.log, "\n"))
		}
	}
	if w.count("state s2") < 3 {
		t.Fatalf("the candidate was not waited for:\n%s", strings.Join(w.log, "\n"))
	}
}

// A candidate that never promotes is not an error to hide: the step ends with promote_timeout after the bound (nothing can be recorded; the bootstrap
// waits for a decision).
func TestAHardFailoverWhoseCandidateNeverPromotesEndsAtTheBound(t *testing.T) {
	w := mapWorld()
	w.noPromoteOnProof = true
	r := w.runner()
	w.fakeClock(r)
	r.MapVerdictWait = 5 * time.Second
	name, err := runThrough(t, r, mapHardInput(), StepName(0, StepStop))
	failedWith(t, w, err, CodePromoteTimeout)
	if name != StepName(0, StepStop) {
		t.Fatalf("failed in %q", name)
	}
}

// A failure before the old primary is fenced changes nothing: the bootstrap that did not start stops the task there, and an old primary that cannot be
// fenced (an error answer comes before the renewal ends) gives the bootstrap up.
func TestAHardFailoverThatCannotStartOrFenceChangesNothing(t *testing.T) {
	w := mapWorld()
	w.mapStartErr = errors.New("the candidate cannot start")
	r := w.runner()
	w.fakeClock(r)
	_, err := runThrough(t, r, mapHardInput(), StepName(0, StepStop))
	failedWith(t, w, err, CodeTargetUnavailable)
	if w.index("demote") >= 0 {
		t.Fatalf("the old primary was fenced although the bootstrap did not start:\n%s", strings.Join(w.log, "\n"))
	}

	w = mapWorld()
	w.oldRefusesDem = &CallError{Status: 503, Code: "unavailable", Message: "the marker cannot be written"}
	r = w.runner()
	w.fakeClock(r)
	_, err = runThrough(t, r, mapHardInput(), StepName(0, StepStop))
	failedWith(t, w, err, CodeTargetUnavailable)
	if w.count("map-abandon s2") != 1 {
		t.Fatalf("the bootstrap was not given up:\n%s", strings.Join(w.log, "\n"))
	}

	w = mapWorld()
	w.oldRefusesDem = &CallError{Status: 409, Code: "stale_term", Message: "another failover"}
	r = w.runner()
	w.fakeClock(r)
	_, err = runThrough(t, r, mapHardInput(), StepName(0, StepStop))
	failedWith(t, w, err, CodeSuperseded)
	if w.count("map-abandon s2") != 1 {
		t.Fatalf("the bootstrap was not given up after a stale term:\n%s", strings.Join(w.log, "\n"))
	}
}

// No standby, no stop: a group without a ready standby is refused with the old leader running.
func TestAHardFailoverOfTheMapGroupWithoutAStandbyStopsNothing(t *testing.T) {
	w := mapWorld()
	for i := range w.standbys {
		w.standbys[i].State = "pending"
	}
	r := w.runner()
	w.fakeClock(r)
	_, err := runThrough(t, r, mapHardInput(), StepName(0, StepStop))
	var ref *Error
	if !errors.As(err, &ref) || ref.Code != "no_replica_available" {
		t.Fatalf("want the refusal of the check, got %v", err)
	}
	if w.index("map-start") >= 0 || w.index("demote") >= 0 {
		t.Fatalf("\n%s", strings.Join(w.log, "\n"))
	}
	w = mapWorld()
	w.posErr["s2"], w.posErr["s3"] = errors.New("down"), errors.New("down")
	r = w.runner()
	w.fakeClock(r)
	_, err = runThrough(t, r, mapHardInput(), StepName(0, StepStop))
	failedWith(t, w, err, CodeNoCandidate)
	if w.index("demote") >= 0 {
		t.Fatalf("the old primary was stopped with no standby that can be read:\n%s", strings.Join(w.log, "\n"))
	}
}

// The new leader's engine runs again the steps of the map group that the old leader could not record: the standby is promoted and pinned in the row, and
// the stop, the lease, the choice, the hand-over and the promotion skip; nothing is started or stopped a second time.
func TestTheNewLeaderRunsAgainTheStepsOfAHardFailoverOfTheMapGroup(t *testing.T) {
	w := mapWorld()
	r := w.runner()
	w.fakeClock(r)
	in := mapHardInput()
	if name, err := runThrough(t, r, in, StepName(0, StepStop)); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	mark := len(w.log)
	// what the bootstrap's B7 writes into the row of the old leader's task
	term := int64(6)
	w.rows.mu.Lock()
	g := w.rows.groups[0]
	g.State, g.TermAfter, g.ChosenStandby, g.NewServer, g.NewNode = "promoted", &term, 10, "s2", 4
	w.rows.mu.Unlock()
	raw, _ := json.Marshal(in)
	steps, _ := r.steps(raw)
	for _, s := range steps {
		switch s.Name {
		case StepName(0, StepStop), StepName(0, StepLease), StepName(0, StepChoose), StepName(0, StepHandOver), StepName(0, StepPromote):
			err := s.Run(context.Background(), &task.Env{Task: task.Task{ID: 1, Input: raw}})
			var sk *task.SkipError
			if !errors.As(err, &sk) {
				t.Errorf("%s did not skip on the new leader: %v", s.Name, err)
			}
		}
	}
	after := strings.Join(w.log[mark:], "\n")
	for _, c := range []string{"map-start", "demote", "lease-fence", "lease-holder", "promote s", "freeze"} {
		if strings.Contains(after, c) {
			t.Errorf("a re-run on the new leader made %q:\n%s", c, after)
		}
	}
	if g := w.rows.groups[0]; g.State != "promoted" || g.ChosenStandby != 10 {
		t.Errorf("the choice step changed the row of a promoted group: %+v", g)
	}
}

// The map group is the last group of a hard failover that includes it for a live leader's server, and of nothing else that is hard.
func TestTheMapGroupIsTheLastGroupOfAHardFailoverOfALiveLeadersServer(t *testing.T) {
	live := Input{Mode: ModeHard, Groups: []int32{0, 2, 5}, MapLive: true}
	if got := mapLast(live); len(got) != 3 || got[0] != 2 || got[1] != 5 || got[2] != 0 {
		t.Fatalf("%v", got)
	}
	if got := mapLast(Input{Mode: ModeHard, Groups: []int32{0, 2, 5}}); got[0] != 0 {
		t.Fatalf("a task that the bootstrap of a dead leader recorded keeps its order: %v", got)
	}
}

// The term that the calls of the map group expect is the one after the groups that ran before it, whatever order the groups were written in: the map group runs last, so
// a group that was promoted before it has raised the term.
func TestTheMapGroupOfAHardFailoverExpectsTheTermAfterTheOtherGroups(t *testing.T) {
	w := newWorld()
	w.rows = newMemRows()
	in := Input{Mode: ModeHard, Groups: []int32{0, 2}, MapLive: true, ServerID: "s1"}
	if err := w.rows.Begin(context.Background(), in, 5, []GroupRow{{Group: 0}, {Group: 2}}); err != nil {
		t.Fatal(err)
	}
	after := int64(6)
	w.rows.groups[2].TermAfter = &after
	run, err := runRowOf(context.Background(), w.rows, in, 0)
	if err != nil || run.ExpectedTerm != 6 {
		t.Fatalf("the map group expects %d (%v), want 6", run.ExpectedTerm, err)
	}
	run, err = runRowOf(context.Background(), w.rows, in, 2)
	if err != nil || run.ExpectedTerm != 5 {
		t.Fatalf("group 2 expects %d (%v), want 5", run.ExpectedTerm, err)
	}
}

// The preflight: a hard failover that includes the map group is accepted on the leader's own server, refused elsewhere, and a hard task that is not the one of a
// live leader's server (an automatic one) is still refused.
func TestThePreflightOfAHardFailoverOfTheMapGroup(t *testing.T) {
	w := mapWorld()
	r := w.runner()
	if ref := r.Preflight(context.Background(), mapHardInput()); ref != nil {
		t.Fatalf("a hard failover of the map group of a live leader's server: %+v", ref)
	}
	notLive := mapHardInput()
	notLive.MapLive = false
	if ref := r.Preflight(context.Background(), notLive); ref == nil || ref.Code != "unsupported_operation" {
		t.Fatalf("a hard task that is not marked: %+v", ref)
	}
	w.self = "s3"
	if ref := w.runner().Preflight(context.Background(), mapHardInput()); ref == nil || ref.Code != "unsupported_operation" || !strings.Contains(ref.Message, "leader") {
		t.Fatalf("not on the leader's server: %+v", ref)
	}
}

// The calls to the old server after the hand-over are bounded for the map group of a hard failover as for a switchover: its instance is the stopped system database.
func TestTheCallsToTheOldServerAreBoundedForAHardFailoverOfTheMapGroup(t *testing.T) {
	w := mapWorld()
	r := w.runner()
	sc := &stepCtx{r: r, in: mapHardInput(), group: 0}
	octx, cancel := sc.oldCtx(context.Background())
	defer cancel()
	if _, ok := octx.Deadline(); !ok {
		t.Fatal("the wait for the old leader's server is not bounded")
	}
	sc.in.MapLive = false
	sc.in.Mode = ModeHard
	octx2, cancel2 := sc.oldCtx(context.Background())
	defer cancel2()
	if _, ok := octx2.Deadline(); ok {
		t.Fatal("the failover of an ordinary group is bounded")
	}
}

// A hard request that includes the map group is the failover of a live leader's server for the task that resolves it; no other request is.
func TestAHardRequestThatIncludesTheMapGroupIsMarkedAsOneOfALiveLeadersServer(t *testing.T) {
	w := mapWorld()
	s := &Service{Runner: w.runner()}
	in, err := s.Resolve(context.Background(), "t", "s1", Request{Mode: ModeHard, ShardGroupIDs: []int32{0}})
	if err != nil || !in.MapLive || len(in.Groups) != 1 || in.Groups[0] != 0 {
		t.Fatalf("hard, the map group: %+v %v", in, err)
	}
	if in, err = s.Resolve(context.Background(), "t", "s1", Request{Mode: ModeHard}); err != nil || !in.MapLive {
		t.Fatalf("hard, all the groups of the server (the map group is one): %+v %v", in, err)
	}
	if in, err = s.Resolve(context.Background(), "t", "s1", Request{Mode: ModeGraceful, ShardGroupIDs: []int32{0}}); err != nil || in.MapLive {
		t.Fatalf("a planned switchover is not marked: %+v %v", in, err)
	}
	w2 := gracefulWorld() // group 1 only
	s2 := &Service{Runner: w2.runner()}
	if in, err = s2.Resolve(context.Background(), "t", "s1", Request{Mode: ModeHard}); err != nil || in.MapLive {
		t.Fatalf("a hard failover of an ordinary group is not marked: %+v %v", in, err)
	}
}
