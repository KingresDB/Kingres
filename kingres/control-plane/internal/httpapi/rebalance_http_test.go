// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/rebalance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// fakeRebalancer answers like the service does, with fixed data.
type fakeRebalancer struct {
	started   rebalance.Options
	stopped   int
	startErr  error
	statusVal api.RebalanceStatus
}

func i64(n int64) *int64   { return &n }
func str(s string) *string { return &s }

func (f *fakeRebalancer) Status(context.Context) (api.RebalanceStatus, error) {
	return f.statusVal, nil
}
func (f *fakeRebalancer) Preview(_ context.Context, req api.RebalancePlanRequest) (api.RebalancePlan, error) {
	return api.RebalancePlan{Needed: true, Totals: api.PlanTotals{MoveCount: 2, TotalBytes: i64(4096)}, Truncated: false,
		Moves:         []api.PlannedMove{{DatabaseName: "shop", ColocationID: 1, Bucket: 3, FromShardGroupID: 1, ToShardGroupID: 3, FromServerID: "db1", ToServerID: "db3", EstimatedSizeBytes: i64(2048), Rank: 1000}},
		ServerImpacts: []api.ServerImpact{{ServerID: "db3", PrimaryBucketsBefore: 0, PrimaryBucketsAfter: 2}}, Warnings: []string{}}, nil
}
func (f *fakeRebalancer) Start(_ context.Context, _ string, o rebalance.Options) (task.Task, error) {
	f.started = o
	if f.startErr != nil {
		return task.Task{}, f.startErr
	}
	return task.Task{ID: 118, Kind: "rebalance", State: task.Pending, SubjectType: "cluster", SubjectID: "cluster",
		Steps: []task.StepState{{Name: rebalance.StepCheck, State: task.StepPending}}, CreatedAt: time.Now(), UpdatedAt: time.Now(), CreatedBy: "alice"}, nil
}
func (f *fakeRebalancer) Stop(context.Context, string) error { f.stopped++; return nil }
func (f *fakeRebalancer) Reports(context.Context, int, string) (api.RebalanceReportPage, error) {
	return api.RebalanceReportPage{Items: []api.RebalanceReportSummary{report(101)}}, nil
}
func (f *fakeRebalancer) Report(_ context.Context, id int64) (api.RebalanceReport, error) {
	if id != 101 {
		return api.RebalanceReport{}, problem.New(problem.NotFound, "No rebalance report.")
	}
	return api.RebalanceReport{RebalanceReportSummary: report(101),
		PlacementBefore: []api.PlacementEntry{{DatabaseName: "shop", ServerID: "db1", PrimaryBuckets: 4}},
		PlacementAfter:  []api.PlacementEntry{{DatabaseName: "shop", ServerID: "db1", PrimaryBuckets: 2}, {DatabaseName: "shop", ServerID: "db3", PrimaryBuckets: 2}},
		Moves: []api.ReportMove{
			{DatabaseName: "shop", ColocationID: 1, Bucket: 3, FromShardGroupID: 1, ToShardGroupID: 3, FromServerID: "db1", ToServerID: "db3", SizeBytes: i64(2048), Outcome: "done", Attempts: 1, DurationSeconds: 1.5},
			{DatabaseName: "shop", ColocationID: 1, Bucket: 4, FromShardGroupID: 1, ToShardGroupID: 3, Outcome: "skipped", SkipReason: str("stopped"), Attempts: 1, Error: str("cancelled")},
		}}, nil
}

func report(id int64) api.RebalanceReportSummary {
	return api.RebalanceReportSummary{ID: id, Outcome: "completed", StartedAt: time.Unix(1000, 0).UTC(), FinishedAt: time.Unix(1060, 0).UTC(),
		Options: &api.RebalanceOptions{MaxConcurrentMoves: 1}, Totals: api.ReportTotals{MovesTotal: 2, MovesDone: 1, MovesSkipped: 1, BytesMoved: 2048, DurationSeconds: 60}}
}

// What the six rebalance operations answer is what the contract describes, and the roles are the contract's.
func TestRebalanceOperationsMatchTheContract(t *testing.T) {
	fake := &fakeRebalancer{statusVal: api.RebalanceStatus{State: "running", Needed: true, Reasons: []string{"serverAdded"}, TaskID: i64(118),
		Options:       &api.RebalanceOptions{MaxConcurrentMoves: 1},
		Progress:      &api.RebalanceProgress{MovesTotal: 4, MovesDone: 1, MovesInFlight: 1, BytesMoved: 2048, Percent: 30},
		InFlightMoves: []api.BucketMove{{DatabaseName: "shop", ColocationID: 1, Bucket: 3, FromShardGroupID: 1, ToShardGroupID: 3, Phase: "copy", Percent: 30, SizeBytes: i64(2048)}},
		LastReport:    func() *api.RebalanceReportSummary { r := report(101); return &r }()}}
	e := newEnv(t, func(d *Deps) { d.Rebalance = fake })
	e.initialize()
	chk := newSchemaChecker(t)
	admin, ops, ro := e.token(auth.RoleAdmin), e.token(auth.RoleOps), e.token(auth.RoleReadonly)

	check := func(op, status string, w interface {
		Bytes() []byte
	}) {
		t.Helper()
		if err := chk.validateResponse(op, status, w.Bytes()); err != nil {
			t.Errorf("%s: %v", op, err)
		}
	}
	w := e.do(req{method: "GET", path: "/api/v1/rebalance", bearer: ro})
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	check("getRebalanceStatus", "200", w.Body)
	w = e.do(req{method: "POST", path: "/api/v1/rebalance/plan", bearer: ro, body: map[string]any{"databases": []string{"shop"}, "moveLimit": 10}})
	if w.Code != 200 {
		t.Fatalf("plan: %d %s", w.Code, w.Body.String())
	}
	check("previewRebalancePlan", "200", w.Body)
	w = e.do(req{method: "GET", path: "/api/v1/rebalance/reports", bearer: ro})
	if w.Code != 200 {
		t.Fatalf("reports: %d %s", w.Code, w.Body.String())
	}
	check("listRebalanceReports", "200", w.Body)
	w = e.do(req{method: "GET", path: "/api/v1/rebalance/reports/101", bearer: ro})
	if w.Code != 200 {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}
	check("getRebalanceReport", "200", w.Body)
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/rebalance/reports/7", bearer: ro}), 404, "not_found")

	// Start: 202 with the Task and a Location; the body is optional; roles.
	w = e.do(req{method: "POST", path: "/api/v1/rebalance", bearer: ops, body: map[string]any{"databases": []string{"shop"}, "maxConcurrentMoves": 2}})
	if w.Code != 202 || w.Header().Get("Location") != "/api/v1/tasks/118" {
		t.Fatalf("start: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if err := chk.validateSchema("Task", w.Body.Bytes()); err != nil {
		t.Errorf("startRebalance: %v", err)
	}
	if len(fake.started.Databases) != 1 || fake.started.MaxConcurrentMoves != 2 {
		t.Errorf("options passed: %+v", fake.started)
	}
	if w := e.do(req{method: "POST", path: "/api/v1/rebalance", bearer: ops}); w.Code != 202 {
		t.Errorf("start without a body: %d %s", w.Code, w.Body.String())
	}
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/rebalance", bearer: ro}), 403, "forbidden")
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/rebalance/stop", bearer: ro}), 403, "forbidden")
	w = e.do(req{method: "POST", path: "/api/v1/rebalance/stop", bearer: ops})
	if w.Code != 200 || fake.stopped != 1 {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	check("stopRebalance", "200", w.Body)
	// A refused start carries the problem code of the refusal.
	fake.startErr = problem.New(problem.ClusterInconsistent, "check_cluster reports stalled_move")
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/rebalance", bearer: admin}), 409, "cluster_inconsistent")
	// The audit log has the start and the stop, with the actor.
	n := 0
	for _, ev := range e.audit.Events() {
		if ev.Action == "startRebalance" || ev.Action == "stopRebalance" {
			n++
		}
	}
	if n < 3 {
		t.Errorf("audit events for the rebalance operations: %d", n)
	}
}
