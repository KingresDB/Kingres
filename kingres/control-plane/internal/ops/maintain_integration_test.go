// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/partsched"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The policy resources and the task partition.maintain against a real instance: a policy is set, read, planned and removed; a run creates the partitions
// ahead and retires the old ones, a second run finds nothing to do, a lock that is held shows as a failed action and the run as partial, a run that
// is interrupted at any step goes on, and the scheduler starts the runs that are due.

var asOf = time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)

// policyOps makes an instance with the system schema (the task of a run is recorded there), a table ev partitioned by day with a partition far in the
// past and one for yesterday, and an Ops whose engine runs on the clock the test controls.
func policyOps(t *testing.T) (*Ops, *pgx.Conn, *task.MemStore, *testClock) {
	t.Helper()
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	if _, err := sysdb.Migrate(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	execAll(t, c,
		`SET TIME ZONE 'UTC'`,
		`ALTER DATABASE postgres SET timezone = 'UTC'`,
		`ALTER DATABASE postgres SET kingres_timepart.lock_retries = 1`,
		`ALTER DATABASE postgres SET kingres_timepart.retire_lock_timeout_ms = 300`,
		`CREATE TABLE public.ev (ts timestamptz NOT NULL, n int) PARTITION BY RANGE (ts)`,
		`CREATE TABLE public.ev_old PARTITION OF public.ev FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2026-01-02 00:00:00+00')`,
		`CREATE TABLE public.ev_yesterday PARTITION OF public.ev FOR VALUES FROM ('2026-01-09 00:00:00+00') TO ('2026-01-10 00:00:00+00')`,
		`INSERT INTO public.ev VALUES ('2026-01-01 05:00:00+00', 1), ('2026-01-09 05:00:00+00', 2)`)
	o := &Ops{Service: testService{in, t}, ServerHost: "s1", Log: slog.New(slog.DiscardHandler)}
	// The tests run for fixed times: the bound of the time of a run is wide here, and tested on its own.
	oldSkew := maintainSkew
	maintainSkew = 100 * 365 * 24 * time.Hour
	t.Cleanup(func() { maintainSkew = oldSkew })
	clk := &testClock{t: time.Now()}
	mem := task.NewMemStoreClock(clk.Now)
	e, err := task.NewEngine(mem, o.Definitions(), task.Options{Holder: "A", Lease: time.Minute, StoreTries: 1, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	o.SetEngine(e)
	return o, c, mem, clk
}

func dailyPolicy() api.PartitionPolicyRequest {
	ret := "P5D"
	return api.PartitionPolicyRequest{PartitionEvery: "P1D", CreateAhead: "P3D", Retention: &ret, RetentionAction: "drop"}
}

func partitions(t *testing.T, c *pgx.Conn) string {
	return scalar(t, c, `SELECT string_agg(c.relname || ' ' || pg_get_expr(c.relpartbound, c.oid), '; ' ORDER BY c.relname)
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'public.ev'::regclass`)
}

func runMaintain(t *testing.T, o *Ops, at time.Time) task.Task {
	t.Helper()
	in, err := o.PrepareMaintain(context.Background(), "admin", "postgres", "public", "ev", &at, "manual")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := o.StartTask(context.Background(), KindMaintain, "table", "postgres/public.ev", in, "admin")
	if err != nil {
		t.Fatal(err)
	}
	o.Wait()
	got, err := o.Engine().Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestIntegrationPartitionPolicyResource(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	// a table without a policy
	_, err := o.GetPartitionPolicy(ctx, "postgres", "public", "ev")
	wantCode(t, err, problem.NotFound)
	// put, then get
	got, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got.PartitionEvery != "P1D" || got.CreateAhead != "P3D" || got.Retention == nil || *got.Retention != "P5D" || got.RetentionAction != "drop" ||
		got.Timezone != "UTC" || got.RunEvery != "PT1H" || !got.Enabled || got.PartitionColumn != "ts" || got.PartitionColumnType != "timestamptz" {
		t.Errorf("policy %+v", got)
	}
	if got.NextRunAt == nil || time.Since(*got.NextRunAt) > time.Minute || got.NextRunAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("a policy that was just set is due now: nextRunAt %v", got.NextRunAt)
	}
	if got.LastRun != nil || got.Health.PartitionsAhead == nil || !strings.HasPrefix(*got.Health.PartitionsAhead, "P") || got.CreatedBy == "" {
		t.Errorf("last run / health / createdBy: %+v", got)
	}
	// replacing it moves the schedule to now again and changes the values
	execAll(t, c, `UPDATE kingres.timepart_policy SET next_run_at = now() + interval '1 day'`)
	rep := dailyPolicy()
	rep.RunEvery = "PT30M"
	rep.ColumnarAfter = nil
	got2, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", rep)
	if err != nil || got2.RunEvery != "PT30M" || got2.NextRunAt == nil || got2.NextRunAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("replace: %+v %v", got2, err)
	}
	// every refusal has its own answer
	bad := func(name string, req api.PartitionPolicyRequest, code problem.Code, fieldPtr string) {
		t.Helper()
		_, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", req)
		p := wantCode(t, err, code)
		if fieldPtr != "" {
			found := false
			for _, e := range p.Errors {
				found = found || e.Field == fieldPtr
			}
			if !found {
				t.Errorf("%s: fields %+v", name, p.Errors)
			}
		}
	}
	bad("a duration that is not ISO 8601", api.PartitionPolicyRequest{PartitionEvery: "1 day"}, problem.ValidationFailed, "/partitionEvery")
	bad("no partitionEvery", api.PartitionPolicyRequest{}, problem.ValidationFailed, "/partitionEvery")
	week := "P1D"
	bad("a retention shorter than a partition", api.PartitionPolicyRequest{PartitionEvery: "P7D", Retention: &week}, problem.ValidationFailed, "/retention")
	bad("a grid that does not exist", api.PartitionPolicyRequest{PartitionEvery: "PT7H"}, problem.ValidationFailed, "/partitionEvery")
	bad("a time zone that does not exist", api.PartitionPolicyRequest{PartitionEvery: "P1D", Timezone: "Mars/Olympus"}, problem.ValidationFailed, "/timezone")
	bad("a retention action that is not one", api.PartitionPolicyRequest{PartitionEvery: "P1D", RetentionAction: "archive"}, problem.ValidationFailed, "/retentionAction")
	// a table that cannot have a policy
	execAll(t, c, `CREATE TABLE public.plain (x int)`, `CREATE TABLE public.byname (k text) PARTITION BY LIST (k)`, `CREATE TABLE public.withdef (d date NOT NULL) PARTITION BY RANGE (d)`,
		`CREATE TABLE public.withdef_d PARTITION OF public.withdef DEFAULT`)
	for _, tb := range []string{"plain", "byname", "withdef"} {
		_, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", tb, api.PartitionPolicyRequest{PartitionEvery: "P1D"})
		wantCode(t, err, problem.PartitionPolicyConflict)
	}
	_, err = o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "nope", api.PartitionPolicyRequest{PartitionEvery: "P1D"})
	wantCode(t, err, problem.NotFound)
	// plan: the actions of a run at a time, from the stored policy and from a policy in the request; nothing changes
	before := partitions(t, c)
	plan, err := o.PlanPartitionPolicy(ctx, "postgres", "public", "ev", api.PlanPartitionPolicyRequest{AsOf: &asOf})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, a := range plan.Actions {
		kinds[a.Kind]++
		if a.State != "planned" || a.Partition == "" {
			t.Errorf("action %+v", a)
		}
	}
	if kinds["create"] < 3 || kinds["drop"] != 1 || !plan.AsOf.Equal(asOf) {
		t.Errorf("plan: %v %+v", kinds, plan)
	}
	hyp := dailyPolicy()
	ahead := "P10D"
	hyp.CreateAhead = ahead
	plan2, err := o.PlanPartitionPolicy(ctx, "postgres", "public", "ev", api.PlanPartitionPolicyRequest{AsOf: &asOf, Policy: &hyp})
	if err != nil || len(plan2.Actions) <= len(plan.Actions) {
		t.Errorf("a longer lookahead plans more: %d, %d (%v)", len(plan2.Actions), len(plan.Actions), err)
	}
	if after := partitions(t, c); after != before {
		t.Errorf("a plan changed the partitions:\n%s\n%s", before, after)
	}
	_, err = o.PlanPartitionPolicy(ctx, "postgres", "public", "ev", api.PlanPartitionPolicyRequest{Policy: &api.PartitionPolicyRequest{PartitionEvery: "weekly"}})
	wantCode(t, err, problem.ValidationFailed)
	// delete
	if err := o.DeletePartitionPolicy(ctx, "postgres", "public", "ev"); err != nil {
		t.Fatal(err)
	}
	_, err = o.GetPartitionPolicy(ctx, "postgres", "public", "ev")
	wantCode(t, err, problem.NotFound)
	wantCode(t, o.DeletePartitionPolicy(ctx, "postgres", "public", "ev"), problem.NotFound)
	if after := partitions(t, c); after != before {
		t.Errorf("the delete of the policy changed the partitions")
	}
	_, err = o.PlanPartitionPolicy(ctx, "postgres", "public", "ev", api.PlanPartitionPolicyRequest{})
	wantCode(t, err, problem.NotFound) // no policy to plan with
}

func TestIntegrationMaintainRun(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	got := runMaintain(t, o, asOf)
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if names := stepNames(got); strings.Join(names, "|") != "Begin the run|Create partitions|Convert partitions|Retire partitions|Finish the run" {
		t.Errorf("steps %v", names)
	}
	parts := partitions(t, c)
	for _, want := range []string{"2026-01-10 00:00:00+00", "2026-01-13 00:00:00+00"} {
		if !strings.Contains(parts, want) {
			t.Errorf("no partition with %s:\n%s", want, parts)
		}
	}
	if strings.Contains(parts, "ev_old") || !strings.Contains(parts, "ev_yesterday") {
		t.Errorf("the retention dropped the wrong ones:\n%s", parts)
	}
	if n := scalar(t, c, `SELECT count(*) FROM public.ev`); n != "1" {
		t.Errorf("rows %s: the dropped partition's row must be gone, the other must stay", n)
	}
	// the run as the API shows it
	page, err := o.ListPartitionPolicyRuns(ctx, "postgres", "public", "ev", "", 10, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("runs %+v %v", page, err)
	}
	r := page.Items[0]
	if r.State != "done" || r.Trigger != "manual" || r.StartedBy != "admin" || r.TaskID == nil || *r.TaskID != got.ID || !r.AsOf.Equal(asOf) || r.FinishedAt == nil || len(r.Actions) != 0 {
		t.Errorf("listed run %+v", r)
	}
	full, err := o.GetPartitionPolicyRun(ctx, "postgres", "public", "ev", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for i, a := range full.Actions {
		kinds[a.Kind]++
		if a.State != "done" || a.Seq != i+1 && a.Seq != i {
			t.Errorf("action %+v", a)
		}
	}
	if kinds["create"] < 3 || kinds["drop"] != 1 {
		t.Errorf("actions %v", kinds)
	}
	if _, err := o.GetPartitionPolicyRun(ctx, "postgres", "public", "ev", r.ID+100); err == nil {
		t.Error("an unknown run was found")
	} else {
		wantCode(t, err, problem.NotFound)
	}
	// the state filter and the policy's last run
	if pg, _ := o.ListPartitionPolicyRuns(ctx, "postgres", "public", "ev", "failed", 10, ""); len(pg.Items) != 0 {
		t.Errorf("failed runs: %+v", pg.Items)
	}
	pol, err := o.GetPartitionPolicy(ctx, "postgres", "public", "ev")
	if err != nil || pol.LastRun == nil || pol.LastRun.ID != r.ID || pol.LastRun.State != "done" {
		t.Errorf("last run %+v %v", pol.LastRun, err)
	}
	if pol.NextRunAt == nil || !pol.NextRunAt.After(asOf) {
		t.Errorf("the next run is after the time of this one: %v", pol.NextRunAt)
	}
	// a second run at the same time has nothing to do
	again := runMaintain(t, o, asOf)
	if again.State != task.Done {
		t.Fatalf("second run %s %+v", again.State, again.Error)
	}
	if parts2 := partitions(t, c); parts2 != parts {
		t.Errorf("a second run changed the partitions")
	}
	second, _ := o.ListPartitionPolicyRuns(ctx, "postgres", "public", "ev", "", 10, "")
	if len(second.Items) != 2 || second.Items[0].ID <= second.Items[1].ID {
		t.Fatalf("runs newest first: %+v", second.Items)
	}
	f2, _ := o.GetPartitionPolicyRun(ctx, "postgres", "public", "ev", second.Items[0].ID)
	if len(f2.Actions) != 0 || f2.State != "done" {
		t.Errorf("the second run: %+v", f2)
	}
	// paging
	one, _ := o.ListPartitionPolicyRuns(ctx, "postgres", "public", "ev", "", 1, "")
	if len(one.Items) != 1 || one.NextCursor == nil {
		t.Fatalf("page 1: %+v", one)
	}
	two, _ := o.ListPartitionPolicyRuns(ctx, "postgres", "public", "ev", "", 1, *one.NextCursor)
	if len(two.Items) != 1 || two.Items[0].ID == one.Items[0].ID || two.NextCursor != nil {
		t.Errorf("page 2: %+v", two)
	}
	// the metrics count the two runs
	if runs, bad, _ := o.PartitionMetrics(); runs != 2 || bad != 0 {
		t.Errorf("metrics: %d runs, %d bad", runs, bad)
	}
	// a run for a table without a policy is refused before a task exists
	execAll(t, c, `SELECT kingres.timepart_unset_policy('public.ev')`)
	_, err = o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", nil, "manual")
	wantCode(t, err, problem.NotFound)
}

// A lock that another session holds stops the retirement of a partition: the action fails with lock_timeout, the step does not fail, the run ends
// partial, and the next run, after the lock is gone, finishes the work.
func TestIntegrationMaintainShowsAFailedActionAndTheNextRunFixesIt(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	holder := second(t, c)
	if _, err := holder.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE public.ev_old IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	got := runMaintain(t, o, asOf)
	if got.State != task.Done {
		t.Fatalf("an action that fails on a lock must not fail the task: %s %+v", got.State, got.Error)
	}
	pg, _ := o.ListPartitionPolicyRuns(ctx, "postgres", "public", "ev", "", 5, "")
	if len(pg.Items) != 1 || pg.Items[0].State != "partial" {
		t.Fatalf("runs %+v", pg.Items)
	}
	run, _ := o.GetPartitionPolicyRun(ctx, "postgres", "public", "ev", pg.Items[0].ID)
	var failed *api.PartitionAction
	for i := range run.Actions {
		if run.Actions[i].State == "failed" {
			failed = &run.Actions[i]
		}
	}
	if failed == nil || failed.Kind != "drop" || failed.Detail == nil || !strings.Contains(*failed.Detail, "lock_timeout") {
		t.Fatalf("the failed action: %+v in %+v", failed, run.Actions)
	}
	if !strings.Contains(partitions(t, c), "ev_old") {
		t.Error("the partition was dropped although it was locked")
	}
	if _, bad, af := o.PartitionMetrics(); bad != 1 || af != 1 {
		t.Errorf("metrics: bad %d, failed actions %d", bad, af)
	}
	pol, _ := o.GetPartitionPolicy(ctx, "postgres", "public", "ev")
	if pol.Health.OK || !hasString(pol.Health.Problems, "lastRunFailed") {
		t.Errorf("health %+v", pol.Health)
	}
	// release the lock: the next run drops it
	if _, err := holder.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	fixed := runMaintain(t, o, asOf.Add(time.Hour))
	if fixed.State != task.Done || strings.Contains(partitions(t, c), "ev_old") {
		t.Errorf("the next run: %s %+v\n%s", fixed.State, fixed.Error, partitions(t, c))
	}
	pg, _ = o.ListPartitionPolicyRuns(ctx, "postgres", "public", "ev", "", 5, "")
	if len(pg.Items) != 2 || pg.Items[0].State != "done" {
		t.Errorf("runs %+v", pg.Items)
	}
}

func hasString(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// A run whose task is interrupted before any step (the process died) goes on at that step when another process takes the task over: the same
// partitions as an uninterrupted run, one run record, no duplicated action.
func TestIntegrationMaintainInterruptedAtEveryStepResumes(t *testing.T) {
	const steps = 5
	for k := 0; k < steps; k++ {
		t.Run(fmt.Sprint("step", k), func(t *testing.T) {
			o, c, mem, clk := policyOps(t)
			ctx := context.Background()
			if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
				t.Fatal(err)
			}
			in, err := o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", &asOf, "manual")
			if err != nil {
				t.Fatal(err)
			}
			// the first process dies when step k is about to run
			dying := o.maintainDefinition()
			runCtx, kill := context.WithCancel(ctx)
			defer kill()
			for i := range dying.Steps {
				i, st := i, dying.Steps[i]
				dying.Steps[i].Run = func(ctx context.Context, env *task.Env) error {
					if i == k {
						kill()
						return ctx.Err()
					}
					return st.Run(ctx, env)
				}
			}
			e1, err := task.NewEngine(mem, []task.Definition{dying}, task.Options{Holder: "A", Lease: time.Minute, StoreTries: 1, Now: clk.Now})
			if err != nil {
				t.Fatal(err)
			}
			tk, err := e1.Submit(ctx, KindMaintain, task.Subject{Type: "table", ID: "postgres/public.ev"}, in, "admin")
			if err != nil {
				t.Fatal(err)
			}
			_ = e1.RunNow(runCtx, tk.ID, nil)
			if mid, _ := e1.Get(ctx, tk.ID); mid.State == task.Done {
				t.Fatal("the dying run finished the task")
			}
			clk.Add(2 * time.Minute) // its lease runs out
			e2, err := task.NewEngine(mem, o.Definitions(), task.Options{Holder: "B", Lease: time.Minute, StoreTries: 1, Now: clk.Now})
			if err != nil {
				t.Fatal(err)
			}
			if ran, err := e2.RunOnce(ctx); !ran || err != nil {
				t.Fatalf("the takeover: %v %v", ran, err)
			}
			got, _ := e2.Get(ctx, tk.ID)
			if got.State != task.Done {
				t.Fatalf("after the takeover: %s %+v", got.State, got.Error)
			}
			parts := partitions(t, c)
			if !strings.Contains(parts, "2026-01-13 00:00:00+00") || strings.Contains(parts, "ev_old") {
				t.Errorf("partitions after the takeover:\n%s", parts)
			}
			if n := scalar(t, c, `SELECT count(*) FROM kingres.timepart_run_data`); n != "1" && k != 0 {
				t.Errorf("%s run records", n)
			}
			final, _ := o.ListPartitionPolicyRuns(ctx, "postgres", "public", "ev", "done", 10, "")
			if len(final.Items) != 1 {
				t.Errorf("done runs: %+v", final.Items)
			}
			// no action twice
			full, _ := o.GetPartitionPolicyRun(ctx, "postgres", "public", "ev", final.Items[0].ID)
			seen := map[string]bool{}
			for _, a := range full.Actions {
				key := a.Kind + " " + a.Partition
				if seen[key] {
					t.Errorf("%s twice", key)
				}
				seen[key] = true
			}
		})
	}
}

// The scheduler asks the instance what is due, starts one task for each table, does not start a second while one is unfinished, and starts again when
// the policy is due again.
func TestIntegrationPartitionSchedulerStartsDueRuns(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s, err := partsched.New(partsched.Config{Backend: PartitionBackend{O: o}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	started, err := s.Tick(ctx)
	if err != nil || len(started) != 1 || started[0].Subject() != "postgres/public.ev" {
		t.Fatalf("first tick: %v %v", started, err)
	}
	o.Wait()
	tasks, _ := o.Engine().List(ctx, 10)
	if len(tasks) != 1 || tasks[0].Kind != KindMaintain || tasks[0].State != task.Done || tasks[0].CreatedBy != "system" {
		t.Fatalf("tasks %+v", tasks)
	}
	pg, _ := o.ListPartitionPolicyRuns(ctx, "postgres", "public", "ev", "", 5, "")
	if len(pg.Items) != 1 || pg.Items[0].Trigger != "schedule" || pg.Items[0].StartedBy != "system" || pg.Items[0].State != "done" {
		t.Fatalf("run %+v", pg.Items)
	}
	// not due again until run_every has passed
	if again, _ := s.Tick(ctx); len(again) != 0 {
		t.Errorf("a policy that ran is not due at once: %v", again)
	}
	now = now.Add(61 * time.Minute)
	if again, _ := s.Tick(ctx); len(again) != 1 {
		t.Errorf("an hour later it is due: %v", again)
	}
	o.Wait()
	// a table whose task is unfinished is skipped: make the policy due and hold a task on the subject
	execAll(t, c, `UPDATE kingres.timepart_policy SET next_run_at = now() - interval '1 minute'`)
	hold, err := o.Engine().Submit(ctx, KindMaintain, task.Subject{Type: "table", ID: "postgres/public.ev"}, maintainParams{Database: "postgres", Schema: "public", Table: "ev", AsOf: asOf.Format(time.RFC3339), Trigger: "manual", Actor: "x"}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Tick(ctx); err != nil || len(got) != 0 {
		t.Errorf("a table with an unfinished task: %v %v", got, err)
	}
	_ = hold
	// the findings for the alerts: the check reads the policies of every database
	checks, _, err := (PartitionBackend{O: o}).Checks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range checks {
		if ch.Database != "postgres" || ch.Schema != "public" || ch.Table != "ev" {
			t.Errorf("check %+v", ch)
		}
	}
}

// second opens another session on the same instance, closed when the test ends.
func second(t *testing.T, c *pgx.Conn) *pgx.Conn {
	t.Helper()
	c2, err := pgx.ConnectConfig(context.Background(), c.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close(context.Background()) })
	return c2
}

// The begin step finds the run that an earlier pass of itself made (the process died after the run was made and before the step was over) and makes no
// second one.
func TestIntegrationMaintainBeginIsIdempotent(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	in, err := o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", &asOf, "manual")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := o.Engine().Submit(ctx, KindMaintain, task.Subject{Type: "table", ID: "postgres/public.ev"}, in, "admin")
	if err != nil {
		t.Fatal(err)
	}
	env := &task.Env{Task: tk}
	for i := 0; i < 3; i++ {
		if err := o.stepMaintainBegin(ctx, env); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if n := scalar(t, c, `SELECT count(*) FROM kingres.timepart_run_data`); n != "1" {
		t.Errorf("%s runs after three passes of the begin step", n)
	}
	if n := scalar(t, c, `SELECT count(*) FROM kingres_cp.partition_run_task WHERE task_id = `+fmt.Sprint(tk.ID)); n != "1" {
		t.Errorf("%s records of the run of the task", n)
	}
	if st := scalar(t, c, `SELECT state FROM kingres.timepart_run_data`); st != "running" {
		t.Errorf("the run is %s: a second begin must not have failed it as interrupted", st)
	}
}
