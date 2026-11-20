// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"strings"
	"testing"
)

// restoreWorld is a cluster after a switchover: the primary of group 1 runs on s2 (node 4), its preferred server is s1 (node 1, the lowest
// id), where a standby streams from it. The leader is s3; the other groups are at home.
func restoreWorld() *world {
	w := gracefulWorld()
	w.oldSrv, w.self = "s2", "s3"
	w.live["s1"] = "live"
	w.nodes = []Node{
		{ID: 0, Group: 0, Host: "s3", Port: 5400, Role: "primary"},
		{ID: 1, Group: 1, Host: "s1", Port: 5401, Role: "standby"},
		{ID: 4, Group: 1, Host: "s2", Port: 5500, Role: "primary"},
		{ID: 5, Group: 1, Host: "s3", Port: 5501, Role: "standby"},
		{ID: 2, Group: 2, Host: "s3", Port: 5402, Role: "primary"},
	}
	w.standbys = []StandbyRec{
		{ID: 10, Group: 1, Server: "s1", InstanceID: 101, Port: 5401, State: "ready", CatalogNode: 1},
		{ID: 11, Group: 1, Server: "s3", InstanceID: 101, Port: 5501, State: "ready", CatalogNode: 5},
	}
	w.pos = map[string]Position{
		"s1": {Server: "s1", Group: 1, InRecovery: true, Timeline: 1, ReceiveLsn: "0/5000000", ReplayLsn: "0/5000000", SystemIdentifier: "9", ReceiverState: "streaming"},
		"s3": {Server: "s3", Group: 1, InRecovery: true, Timeline: 1, ReceiveLsn: "0/5000000", ReplayLsn: "0/5000000", SystemIdentifier: "9", ReceiverState: "streaming"},
	}
	return w
}

func restoreInputOf(t *testing.T, w *world, only ...int32) Input {
	t.Helper()
	s := &Service{Runner: w.runner()}
	in, err := s.restoreInput(context.Background(), "tester", w.nodes, only)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// The groups that are not at home are found, with the server that runs each and the preferred server as the target; the others are left.
func TestRestoreFindsTheGroupsThatAreNotAtHome(t *testing.T) {
	w := restoreWorld()
	in := restoreInputOf(t, w)
	if in.Mode != ModeRestore || len(in.Groups) != 1 || in.Groups[0] != 1 || in.Olds[1] != "s2" || in.Targets[1] != "s1" || len(in.Skipped) != 0 {
		t.Fatalf("%+v", in)
	}
	if in2 := restoreInputOf(t, w, 2); len(in2.Groups) != 0 || len(in2.Skipped) != 0 {
		t.Errorf("a group at home is not restored and not reported: %+v", in2)
	}
}

// A group whose preferred server has no ready standby is skipped with the reason, and so is the map group away from home.
func TestRestoreSkipsWhatItCannotRestore(t *testing.T) {
	w := restoreWorld()
	w.standbys[0].State = "rebuilding"
	w.nodes[0].Host = "s2" // the map group runs away from its preferred server
	w.nodes = append(w.nodes, Node{ID: -1, Group: 0, Host: "s1", Port: 5399, Role: "standby"})
	in := restoreInputOf(t, w)
	reasons := map[int32]string{}
	for _, sk := range in.Skipped {
		reasons[sk.Group] = sk.Reason
	}
	if len(in.Groups) != 0 || reasons[1] != "preferredNotReady" || reasons[0] != "mapGroup" {
		t.Fatalf("%+v", in)
	}
}

// The restore hands the group back with the steps of a switchover, from the server that took it over.
func TestRestoreHandsAGroupBackToItsPreferredServer(t *testing.T) {
	w := restoreWorld()
	r := w.runner()
	w.fakeClock(r)
	in := restoreInputOf(t, w)
	if name, err := runAll(t, r, in); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, strings.Join(w.log, "\n"))
	}
	mustOrder(t, w, "quiesce s2", "mark s2")
	mustOrder(t, w, "mark s2", "replayed? s1")
	mustOrder(t, w, "replayed? s1", "demote-fast s2")
	mustOrder(t, w, "demote-fast s2", "verify s1")
	mustOrder(t, w, "verify s1", "promote s1")
	mustOrder(t, w, "promote s1", "promote_node kingres_system 1")
	if g := w.rows.groups[1]; g.State != "done" || g.OldServer != "s2" || g.ChosenStandby != 10 {
		t.Errorf("group row %+v", g)
	}
	if w.rows.outcome != "completed" {
		t.Errorf("outcome %q", w.rows.outcome)
	}
}

// A group that fails before its promotion is rolled back and recorded failed, the task goes on, and the outcome is partial: failing back
// is not urgent, the report says what happened.
func TestRestoreGoesOnWhenAGroupFailsBeforeItsPromotion(t *testing.T) {
	w := restoreWorld()
	w.verifyAnswers = []bool{false}
	r := w.runner()
	w.fakeClock(r)
	in := restoreInputOf(t, w)
	if name, err := runAll(t, r, in); err != nil {
		t.Fatalf("a failed group stopped the run at %s: %v", name, err)
	}
	g := w.rows.groups[1]
	if g.State != "failed" || !strings.Contains(g.Error, CodeStandbyBehind) {
		t.Fatalf("group row %+v", g)
	}
	if w.index("promote s1") >= 0 {
		t.Error("the standby that is behind was promoted")
	}
	if w.index("resume s2") < 0 || w.index("unquiesce s2") < 0 {
		t.Errorf("the group was not rolled back:\n%s", strings.Join(w.log, "\n"))
	}
	if w.rows.outcome != "partial" {
		t.Errorf("outcome %q", w.rows.outcome)
	}
}

// The groups that a restore leaves alone are in the report as skipped.
func TestRestoreRecordsTheSkippedGroups(t *testing.T) {
	w := restoreWorld()
	r := w.runner()
	w.fakeClock(r)
	in := restoreInputOf(t, w)
	in.Skipped = []SkippedGroup{{Group: 7, Reason: "preferredNotReady", Text: "no standby"}}
	if name, err := runAll(t, r, in); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if g, ok := w.rows.groups[7]; !ok || g.State != "skipped" || !strings.Contains(g.Error, "preferredNotReady") {
		t.Errorf("skipped group %+v %v", g, ok)
	}
	if w.rows.outcome != "completed" {
		t.Errorf("a skipped group makes the run partial: %q", w.rows.outcome)
	}
}

// A failure after the promotion is the task's: it is not turned into a skip.
func TestRestoreDoesNotSkipAFailureAfterThePromotion(t *testing.T) {
	w := restoreWorld()
	w.rows.groups[1] = &GroupRow{Group: 1, State: "promoted"}
	sc := &stepCtx{r: w.runner(), in: Input{Mode: ModeRestore, Olds: map[int32]string{1: "s2"}}, rows: w.rows, group: 1}
	if skip := sc.restoreContinues(context.Background(), fail(CodePromoteTimeout, true, "x")); skip != nil {
		t.Fatal("a failure of a promoted group was turned into a skip")
	}
}

// The phase of a group of a switchover is the step it is at.
func TestPhaseOfTheStepsOfASwitchover(t *testing.T) {
	steps := func(done int) []stepProgress {
		var out []stepProgress
		for i, k := range switchoverSteps {
			st := "pending"
			if i < done {
				st = "done"
			}
			out = append(out, stepProgress{Name: StepName(1, k), State: st})
		}
		return out
	}
	want := map[string]string{StepFreeze: "checking", StepChoose: "choosing", StepQuiesce: "quiescing", StepDrain: "waitingForTransactions",
		StepStopClean: "stopping", StepVerify: "verifying", StepLease: "waitingForLease", StepHandOver: "promoting", StepPromote: "promoting",
		StepCatalog: "followingCatalog", StepRepoint: "repointing", StepClients: "finishing"}
	for i, k := range switchoverSteps {
		if w, ok := want[k]; ok {
			if got := phaseOfSteps(steps(i), 1, "x"); got != w {
				t.Errorf("at %q: %q, want %q", k, got, w)
			}
		}
	}
	if got := phaseOfSteps(steps(len(switchoverSteps)), 1, "fallback"); got != "fallback" {
		t.Errorf("all done: %q", got)
	}
	if got := phaseOfSteps(steps(0), 2, "fallback"); got != "fallback" {
		t.Errorf("another group's steps: %q", got)
	}
}

// The plan of a switchover names the steps, the chosen standby, the pause of the writes and a verdict of none.
func TestThePlanOfASwitchover(t *testing.T) {
	w := gracefulWorld()
	r := w.runner()
	w.fakeClock(r)
	p, err := (&Service{Runner: r}).Plan(context.Background(), gracefulInput())
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanProceed || p.Mode != ModeGraceful || p.Kind != "failover" || p.ServerID == nil || len(p.Groups) != 1 {
		t.Fatalf("%+v", p)
	}
	g := p.Groups[0]
	if g.ToServerID == nil || *g.ToServerID != "s2" || g.WritesPausedSeconds == nil || g.DataLoss.Verdict != VerdictNone || len(g.Steps) != len(switchoverStepNames()) {
		t.Fatalf("%+v", g)
	}
	// A refusal of the request is a blocker of the plan, not an error.
	w.live["s1"] = "suspect"
	p, err = (&Service{Runner: r}).Plan(context.Background(), gracefulInput())
	if err != nil || p.CanProceed || len(p.Blockers) == 0 || p.Blockers[0].Code != "node_state_invalid" {
		t.Fatalf("%+v %v", p, err)
	}
}

// The plan of a restore has no server, says where each group comes from and goes to.
func TestThePlanOfARestore(t *testing.T) {
	w := restoreWorld()
	r := w.runner()
	w.fakeClock(r)
	p, err := (&Service{Runner: r}).Plan(context.Background(), restoreInputOf(t, w))
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanProceed || p.Kind != "restorePreferredPrimaries" || p.ServerID != nil || p.Mode != ModeRestore || len(p.Groups) != 1 {
		t.Fatalf("%+v", p)
	}
	if g := p.Groups[0]; g.FromServerID != "s2" || g.ToServerID == nil || *g.ToServerID != "s1" {
		t.Fatalf("%+v", g)
	}
}

// A standby that does not stream is not offered by the plan of a switchover: the best standby that does is.
func TestThePlanOfASwitchoverSkipsAStandbyThatDoesNotStream(t *testing.T) {
	w := gracefulWorld()
	p := w.pos["s2"]
	p.ReceiverState = ""
	w.pos["s2"] = p
	r := w.runner()
	w.fakeClock(r)
	plan, err := (&Service{Runner: r}).Plan(context.Background(), gracefulInput())
	if err != nil || len(plan.Groups) != 1 || plan.Groups[0].ToServerID == nil || *plan.Groups[0].ToServerID != "s3" {
		t.Fatalf("%+v %v", plan, err)
	}
}

// A group that a restore leaves alone is in the plan with the reason, as it is in the report: no target, no candidates, no steps.
func TestThePlanOfARestoreListsTheSkippedGroups(t *testing.T) {
	w := restoreWorld()
	r := w.runner()
	w.fakeClock(r)
	in := restoreInputOf(t, w)
	in.Skipped = []SkippedGroup{{Group: 0, Reason: "mapGroup", Text: "The planned switchover of the map group is not supported yet."},
		{Group: 7, Reason: "preferredNotReady", Text: "The preferred server has no ready standby."}}
	in.Olds[0], in.Olds[7] = "s3", "s2"
	p, err := (&Service{Runner: r}).Plan(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanProceed || len(p.Groups) != 3 {
		t.Fatalf("%+v", p)
	}
	// ordered by group
	if p.Groups[0].ShardGroupID != 0 || p.Groups[1].ShardGroupID != 1 && p.Groups[1].ShardGroupID != 7 {
		t.Fatalf("order: %+v", p.Groups)
	}
	for _, g := range p.Groups {
		if g.ShardGroupID == 1 {
			continue
		}
		if g.Skipped == nil || g.Skipped.Reason == "" || g.Skipped.Message == "" || g.ToServerID != nil || len(g.Steps) != 0 || len(g.Candidates) != 0 || !strings.Contains(g.DataLoss.Message, "Skipped (") {
			t.Errorf("a skipped group: %+v", g)
		}
	}
}
