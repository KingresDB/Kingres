// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/partsched"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The tests of the fixes of the independent review of phase 12 (P12C-01 to P12C-16) that need a real instance.

// P12C-01: the time of a run is the time of the server. A request, or a clock of the leader that jumped, cannot make a run for 2100 that drops every
// partition and parks the schedule there.
func TestIntegrationAManualRunMustBeForTheTimeOfTheServer(t *testing.T) {
	o, c, _, _ := policyOps(t)
	maintainSkew = 5 * time.Minute // policyOps made it wide for the tests of fixed times; here it is the real one
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	before := partitions(t, c)
	far := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, at := range map[string]time.Time{"2100": far, "ten minutes ahead": time.Now().Add(10 * time.Minute), "ten minutes back": time.Now().Add(-10 * time.Minute)} {
		at := at
		_, err := o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", &at, "manual")
		p := wantCode(t, err, problem.ValidationFailed)
		if len(p.Errors) != 1 || p.Errors[0].Field != "/asOf" {
			t.Errorf("%s: %+v", name, p.Errors)
		}
		// the scheduler goes through the same check: a leader whose clock jumped starts nothing
		started, err := o.StartMaintain(ctx, "postgres", "public", "ev", at)
		if started || err == nil {
			t.Errorf("%s: the scheduler started a run (%v, %v)", name, started, err)
		}
	}
	if after := partitions(t, c); after != before {
		t.Errorf("a refused run changed the partitions")
	}
	soon := time.Now().Add(2 * time.Minute)
	if _, err := o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", &soon, "manual"); err != nil {
		t.Errorf("two minutes of skew is allowed: %v", err)
	}
	got := runMaintain(t, o, time.Now())
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	// the schedule moved by run_every from now, not to the far future
	if ok := scalar(t, c, `SELECT next_run_at < now() + interval '2 hours' AND next_run_at > now() FROM kingres.timepart_policy`); ok != "true" {
		t.Errorf("next_run_at is not within the hour after the run")
	}
}

// P12C-02: a run that fails before it could finish moves the schedule all the same, so that the scheduler does not start a task and a run at every tick for ever.
func TestIntegrationAFailedBeginMovesTheScheduleAndNothingStorms(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	execAll(t, c, `CREATE ROLE app_owner NOLOGIN`)
	allowOwners(t, c, true) // the owner is not a superuser: the setting is on, so that begin_run is what refuses
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	execAll(t, c, `ALTER TABLE public.ev OWNER TO app_owner`) // begin_run records the run as failed (owner_changed)
	got := runMaintain(t, o, time.Now())
	if got.State != task.Failed || got.Error == nil || got.Error.Code != string(problem.PartitionPolicyConflict) || !strings.Contains(got.Error.Message, "owner_changed") {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if st := scalar(t, c, `SELECT state FROM kingres.timepart_run_data`); st != "failed" {
		t.Errorf("run state %s", st)
	}
	if ok := scalar(t, c, `SELECT next_run_at > now() + interval '30 minutes' FROM kingres.timepart_policy_data`); ok != "true" {
		t.Fatal("the failed run did not move next_run_at: the policy is due again at once")
	}
	s, _ := partsched.New(partsched.Config{Backend: PartitionBackend{O: o}})
	for i := 0; i < 5; i++ {
		if started, err := s.Tick(ctx); err != nil || len(started) != 0 {
			t.Fatalf("tick %d: %v %v", i, started, err)
		}
	}
	if n := scalar(t, c, `SELECT count(*) FROM kingres.timepart_run_data`); n != "1" {
		t.Errorf("%s runs after five ticks", n)
	}
	tasks, _ := o.Engine().List(ctx, 20)
	if len(tasks) != 1 {
		t.Errorf("%d tasks after five ticks", len(tasks))
	}
}

// P12C-02: an error of a phase marks the run failed and closes it (schedule, pruning), and the scheduler leaves the table alone for a while.
func TestIntegrationAFailedStepClosesTheRun(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	execAll(t, c, `ALTER FUNCTION kingres.timepart_run_action(bigint, text) RENAME TO timepart_run_action_x`) // a phase now raises
	got := runMaintain(t, o, time.Now())
	if got.State != task.Failed || got.Error == nil || got.Error.Step != stepMaintainCreate {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if st := scalar(t, c, `SELECT state || ' ' || (error IS NOT NULL) || ' ' || (finished_at IS NOT NULL) FROM kingres.timepart_run_data`); st != "failed true true" {
		t.Errorf("the run: %s", st)
	}
	if ok := scalar(t, c, `SELECT next_run_at > now() + interval '30 minutes' FROM kingres.timepart_policy_data`); ok != "true" {
		t.Error("the schedule did not move")
	}
	if m, bad, _ := o.PartitionMetrics(); m != 1 || bad != 1 {
		t.Errorf("metrics %d %d", m, bad)
	}
	execAll(t, c, `UPDATE kingres.timepart_policy_data SET next_run_at = now() - interval '1 minute'`) // due again
	started, err := o.StartMaintain(ctx, "postgres", "public", "ev", time.Now())
	if err != nil || started {
		t.Errorf("a table whose last run failed a minute ago is left alone: %v %v", started, err)
	}
	old := maintainBackoff
	maintainBackoff = 0
	defer func() { maintainBackoff = old }()
	if started, err := o.StartMaintain(ctx, "postgres", "public", "ev", time.Now()); err != nil || !started {
		t.Errorf("after the back-off the policy is started again: %v %v", started, err)
	}
	o.Wait()
}

// P12C-02: a task that ends while its run is going (cancelled, a failure that no step saw) closes the run through the hook of the kind.
func TestIntegrationOnFailClosesARunThatIsStillGoing(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	in, err := o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", nil, "manual")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := o.Engine().Submit(ctx, KindMaintain, task.Subject{Type: "table", ID: "postgres/public.ev"}, in, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := o.stepMaintainBegin(ctx, &task.Env{Task: tk}); err != nil {
		t.Fatal(err)
	}
	if st := scalar(t, c, `SELECT state FROM kingres.timepart_run_data`); st != "running" {
		t.Fatalf("state %s", st)
	}
	tk.Error = &task.Error{Code: task.CodeCanceled, Message: "cancelled by an operator"}
	o.maintainOnFail(ctx, tk)
	if st := scalar(t, c, `SELECT state || ' ' || error FROM kingres.timepart_run_data`); !strings.HasPrefix(st, "failed ") || !strings.Contains(st, "cancelled by an operator") {
		t.Errorf("run %s", st)
	}
	if ok := scalar(t, c, `SELECT next_run_at > now() + interval '30 minutes' FROM kingres.timepart_policy_data`); ok != "true" {
		t.Error("the schedule did not move")
	}
	o.maintainOnFail(ctx, tk) // a run that is closed is left as it is
}

// P12C-08: a run that the instance does not have is not "done": the step fails and says why (a task taken over by a server whose instance does not hold the run).
func TestIntegrationAMissingRunFailsTheStep(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	in, _ := o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", nil, "manual")
	tk, err := o.Engine().Submit(ctx, KindMaintain, task.Subject{Type: "table", ID: "postgres/public.ev"}, in, "admin")
	if err != nil {
		t.Fatal(err)
	}
	env := &task.Env{Task: tk}
	if err := o.stepMaintainBegin(ctx, env); err != nil {
		t.Fatal(err)
	}
	execAll(t, c, `DELETE FROM kingres.timepart_run_data`)
	for name, f := range map[string]func() error{
		"create": func() error { return o.stepMaintainPhase(ctx, env, "create") },
		"finish": func() error { return o.stepMaintainFinish(ctx, env) },
	} {
		if err := f(); err == nil || !strings.Contains(err.Error(), "not in the instance of this server") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// P12C-11: the reads never install an extension; without kingres_timepart there is no policy, no run and an empty list.
func TestIntegrationReadsDoNotInstallTheExtension(t *testing.T) {
	o, c := storageOps(t)
	ctx := context.Background()
	execAll(t, c, `CREATE TABLE public.ev (ts timestamptz NOT NULL) PARTITION BY RANGE (ts)`)
	installed := func() string {
		return scalar(t, c, `SELECT count(*) FROM pg_extension WHERE extname IN ('kingres_timepart', 'kingres_columnar')`)
	}
	_, err := o.GetPartitionPolicy(ctx, "postgres", "public", "ev")
	if p := wantCode(t, err, problem.NotFound); !strings.Contains(p.Detail, "not installed") {
		t.Errorf("detail %q", p.Detail)
	}
	page, err := o.ListPartitionPolicyRuns(ctx, "postgres", "public", "ev", "", 10, "")
	if err != nil || len(page.Items) != 0 {
		t.Errorf("runs %+v %v", page, err)
	}
	_, err = o.GetPartitionPolicyRun(ctx, "postgres", "public", "ev", 1)
	wantCode(t, err, problem.NotFound)
	_, err = o.PlanPartitionPolicy(ctx, "postgres", "public", "ev", api.PlanPartitionPolicyRequest{})
	wantCode(t, err, problem.NotFound)
	_, err = o.PlanPartitionPolicy(ctx, "postgres", "public", "ev", api.PlanPartitionPolicyRequest{Policy: &api.PartitionPolicyRequest{PartitionEvery: "P1D"}})
	wantCode(t, err, problem.ExtensionMissing)
	wantCode(t, o.DeletePartitionPolicy(ctx, "postgres", "public", "ev"), problem.NotFound)
	if n := installed(); n != "0" {
		t.Fatalf("a read installed %s extensions", n)
	}
	// a write installs
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", api.PartitionPolicyRequest{PartitionEvery: "P1D"}); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, c, `SELECT count(*) FROM pg_extension WHERE extname = 'kingres_timepart'`); n != "1" {
		t.Errorf("put did not install kingres_timepart")
	}
}

// P12C-13 and P12C-14: a policy cannot be replaced or removed while a run of it is unfinished, however many tasks came after that one.
func TestIntegrationPutAndDeleteAreRefusedDuringARun(t *testing.T) {
	o, _, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	in, _ := o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", nil, "manual")
	if _, err := o.Engine().Submit(ctx, KindMaintain, task.Subject{Type: "table", ID: "postgres/public.ev"}, in, "admin"); err != nil {
		t.Fatal(err)
	}
	// more than two hundred newer tasks of other subjects
	for i := 0; i < 250; i++ {
		_, err := o.Engine().Submit(ctx, KindSetStorage, task.Subject{Type: "table", ID: "postgres/public.x" + string(rune('a'+i%26)) + string(rune('a'+i/26))}, storageParams{Database: "postgres", Schema: "public", Table: "t", AccessMethod: "heap"}, "u")
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy())
	wantCode(t, err, problem.OperationInProgress)
	wantCode(t, o.DeletePartitionPolicy(ctx, "postgres", "public", "ev"), problem.OperationInProgress)
	_, err = o.PrepareSetStorage(ctx, "admin", "postgres", "public", "ev", "ev_old", api.TableStorageRequest{AccessMethod: "columnar"})
	wantCode(t, err, problem.OperationInProgress)
}

// P12C-12: a partition alert and the raw 404 say the limit of this version; checked on the problem.
func TestIntegrationATableOfAnotherInstanceIsNotFoundWithTheReason(t *testing.T) {
	o, _, _, _ := policyOps(t)
	ctx := context.Background()
	if err := o.ensureExtensions(ctx, "postgres", []string{ExtTimepart}, false); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func() error{
		"get": func() error { _, err := o.GetPartitionPolicy(ctx, "postgres", "public", "nowhere"); return err },
		"put": func() error {
			_, err := o.PutPartitionPolicy(ctx, "a", "postgres", "public", "nowhere", dailyPolicy())
			return err
		},
		"plain": func() error {
			_, err := o.PrepareSetStorage(ctx, "a", "postgres", "public", "nowhere", "", api.TableStorageRequest{AccessMethod: "columnar"})
			return err
		},
		"maintain": func() error {
			_, err := o.PrepareMaintain(ctx, "a", "postgres", "public", "nowhere", nil, "manual")
			return err
		},
	} {
		err := f()
		p := wantCode(t, err, problem.NotFound)
		if !strings.Contains(p.Detail, "instance of the manager leader") {
			t.Errorf("%s: %q", name, p.Detail)
		}
	}
}
