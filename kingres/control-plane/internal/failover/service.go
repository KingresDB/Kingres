// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// SubjectType is the subject type of the task of a failover (contract table of task kinds).
const SubjectType = "server"

// Service is the failover as the HTTP layer uses it: the request, the plan, the
// status and the reports. It runs on the manager leader.
type Service struct {
	Runner *Runner
	Conns  Conns
	// Engine returns the task engine, nil before it exists.
	Engine func() *task.Engine
	// Background runs f in the background, waited for at shutdown; BGFn returns the
	// context of background runs.
	Background func(f func())
	BGFn       func() context.Context
	Audit      func(ctx context.Context, ev audit.Event)
	Log        *slog.Logger
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Definitions returns the task definition.
func (s *Service) Definitions() []task.Definition {
	return []task.Definition{s.Runner.Definition(s.Runner.OnFail(func(ctx context.Context, id int64, outcome, text string) {
		rows := &PGRows{Conns: s.Conns, TaskID: id}
		if err := rows.Abandon(ctx, outcome, text); err != nil {
			s.log().Warn("failover: closing the record of a run", "task", id, "error", err)
		}
	}))}
}

func (s *Service) sys(ctx context.Context, f func(c *pgx.Conn) error) error {
	c, err := s.Conns.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return f(c)
}

func refusalProblem(r *Refusal) error {
	return problem.New(problem.Code(r.Code), r.Message)
}

// Resolve turns a request into the input of the task (or a problem).
func (s *Service) Resolve(ctx context.Context, actor, server string, req Request) (Input, error) {
	var errs []problem.FieldError
	if req.Mode != ModeHard && req.Mode != ModeGraceful {
		errs = append(errs, problem.FieldError{Field: "/mode", Message: "must be hard or graceful", Code: "unknown_value"})
	}
	if len(req.Comment) > 500 {
		errs = append(errs, problem.FieldError{Field: "/comment", Message: "at most 500 characters", Code: "out_of_range"})
	}
	seen := map[int32]bool{}
	for i, g := range req.ShardGroupIDs {
		if g < 0 || seen[g] {
			errs = append(errs, problem.FieldError{Field: "/shardGroupIds/" + strconv.Itoa(i), Message: "must be a group id, once", Code: "out_of_range"})
		}
		seen[g] = true
	}
	targets := map[int32]string{}
	for i, t := range req.Targets {
		if _, dup := targets[t.ShardGroupID]; dup || t.ServerID == "" {
			errs = append(errs, problem.FieldError{Field: "/targets/" + strconv.Itoa(i), Message: "one target per group, with a server", Code: "out_of_range"})
		}
		targets[t.ShardGroupID] = t.ServerID
	}
	if len(errs) > 0 {
		return Input{}, problem.Validation("The failover request is invalid.", errs...)
	}
	nodes, err := s.Runner.Cat.Nodes(ctx, SystemDB)
	if err != nil {
		return Input{}, problem.New(problem.ServiceUnavailable, "The catalog cannot be read now.").WithRetryAfter(5)
	}
	known := false
	for _, n := range nodes {
		if n.Host == server {
			known = true
		}
	}
	if !known {
		return Input{}, problem.New(problem.NotFound, "No server "+server+" in the cluster.")
	}
	groups := GroupsOn(nodes, server)
	if len(req.ShardGroupIDs) > 0 {
		groups = append([]int32(nil), req.ShardGroupIDs...)
		sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	}
	for g := range targets {
		if !seen[g] && len(req.ShardGroupIDs) > 0 {
			return Input{}, problem.New(problem.Unprocessable, fmt.Sprintf("A target names shard group %d, which the request does not fail over.", g))
		}
	}
	return Input{Mode: req.Mode, ServerID: server, Groups: groups, Targets: targets, AllowDataLoss: req.AllowDataLoss,
		Comment: req.Comment, Trigger: "manual", RequestedBy: actor}, nil
}

// Plan reads the state and answers what the request would do. The positions are read
// now, while the old primary may still be writing, so the verdict is a first
// assessment; the task reads them again after the old primary is fenced and decides.
func (s *Service) Plan(ctx context.Context, in Input) (Plan, error) {
	r := s.Runner
	out := Plan{GeneratedAt: time.Now().UTC(), Kind: "failover", Mode: in.Mode, ServerID: &in.ServerID, Blockers: []Blocker{}, Groups: []GroupPlan{}}
	if ref := r.Preflight(ctx, in); ref != nil {
		out.Blockers = append(out.Blockers, Blocker{Code: ref.Code, ShardGroupID: ref.Group, Message: ref.Message})
	}
	nodes, _ := r.Cat.Nodes(ctx, SystemDB)
	for _, g := range in.Groups {
		gp := GroupPlan{ShardGroupID: g, IsMapGroup: g == 0, FromServerID: in.ServerID, Candidates: []CandidateOut{}, MovesTheManager: g == 0,
			Steps: stepNames(), DataLoss: DataLoss{Verdict: VerdictUnknown, AsyncDatabases: []string{}, UnreadableServerIDs: []string{}}}
		recs, err := r.Plan.Standbys(ctx, g)
		if err != nil {
			return out, err
		}
		var ready []StandbyRec
		for _, st := range recs {
			if st.State == "ready" {
				ready = append(ready, st)
			}
		}
		if len(ready) == 0 {
			out.Groups = append(out.Groups, gp)
			continue
		}
		reads := make([]Candidate, 0, len(ready))
		for _, st := range ready {
			cctx, cancel := context.WithTimeout(ctx, orDur(r.PositionTimeout, 5*time.Second))
			pos, err := r.Peers.Position(cctx, st.Server, g)
			cancel()
			cd := Candidate{StandbyID: st.ID, Server: st.Server, Readable: err == nil, Pos: pos, Preferred: r.Plan.Preferred(ctx, g, st.Server), RingDist: r.Plan.Ring(in.ServerID, st.Server)}
			if err != nil {
				cd.Err = err.Error()
			}
			reads = append(reads, cd)
		}
		facts, err := r.Plan.Facts(ctx, g)
		if err != nil {
			return out, err
		}
		ch := Choose(ChooseInput{Candidates: reads, Target: in.Targets[g], Facts: facts})
		d, l := marshalDecision(ch, facts, reads)
		_ = json.Unmarshal(d, &gp.Candidates)
		_ = json.Unmarshal(l, &gp.DataLoss)
		if ch.Chosen != nil {
			to := ch.Chosen.Server
			gp.ToServerID = &to
			est := int64(30)
			gp.EstimatedSeconds = &est
		}
		if !Accepts(ch.Verdict, in.AllowDataLoss) {
			gg := g
			out.Blockers = append(out.Blockers, Blocker{Code: string(problem.FailoverNeedsDecision), ShardGroupID: &gg, Message: ch.Message})
		}
		out.Groups = append(out.Groups, gp)
	}
	_ = nodes
	out.CanProceed = len(out.Blockers) == 0
	return out, nil
}

func stepNames() []string {
	out := []string{StepCheck}
	out = append(out, groupSteps...)
	return append(out, StepWriteReport)
}

// Start checks a request and starts the task, or answers the plan for a dry run, or
// resumes a task that waits for an operator. It returns exactly one of the task and
// the plan.
func (s *Service) Start(ctx context.Context, actor, server string, req Request) (*task.Task, *Plan, error) {
	eng := s.Engine()
	if eng == nil {
		return nil, nil, problem.New(problem.ServiceUnavailable, "The task engine is not running yet.").WithRetryAfter(5)
	}
	in, err := s.Resolve(ctx, actor, server, req)
	if err != nil {
		return nil, nil, err
	}
	a, err := s.active(ctx)
	if err != nil {
		return nil, nil, problem.New(problem.ServiceUnavailable, "The system database is not available.").WithRetryAfter(5)
	}
	if a != nil && a.State != task.Paused {
		return nil, nil, problem.New(problem.FailoverInProgress, "A failover is running (task "+strconv.FormatInt(a.ID, 10)+"); wait for it to end.")
	}
	if a != nil && a.Server != server {
		return nil, nil, problem.New(problem.FailoverInProgress, "A failover of server "+a.Server+" waits for an operator (task "+strconv.FormatInt(a.ID, 10)+"); resume or cancel it first.")
	}
	if a == nil {
		if ref := s.Runner.Preflight(ctx, in); ref != nil {
			if req.DryRun {
				p, perr := s.Plan(ctx, in)
				return nil, &p, perr
			}
			return nil, nil, refusalProblem(ref)
		}
	}
	if req.DryRun {
		p, err := s.Plan(ctx, in)
		return nil, &p, err
	}
	if !in.AllowDataLoss && a == nil {
		p, err := s.Plan(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		if !p.CanProceed {
			detail := "This failover could lose data; repeat the request with allowDataLoss set to true if you accept it, or ask for the plan with dryRun."
			for _, b := range p.Blockers {
				detail += fmt.Sprintf(" Shard group %s: %s", groupText(b.ShardGroupID), b.Message)
			}
			return nil, nil, problem.New(problem.FailoverNeedsDecision, detail)
		}
	}
	if a != nil {
		return s.resume(ctx, eng, a, actor, in)
	}
	t, err := eng.Submit(ctx, KindFailover, task.Subject{Type: SubjectType, ID: server}, in, actor)
	if errors.Is(err, task.ErrSubjectBusy) {
		return nil, nil, problem.New(problem.FailoverInProgress, "A failover of this server is already running.")
	}
	var x *task.ExcludedError
	if errors.As(err, &x) {
		return nil, nil, x.Problem()
	}
	if err != nil {
		return nil, nil, err
	}
	s.run(eng, t.ID)
	return &t, nil, nil
}

func groupText(g *int32) string {
	if g == nil {
		return "-"
	}
	return strconv.Itoa(int(*g))
}

type activeRun struct {
	ID     int64
	State  task.State
	Server string
}

func (s *Service) active(ctx context.Context) (*activeRun, error) {
	var a activeRun
	err := s.sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT id, state, subject_id FROM kingres_cp.task WHERE kind = $1 AND state IN ('pending', 'running', 'paused')
			ORDER BY id DESC LIMIT 1`, KindFailover).Scan(&a.ID, &a.State, &a.Server)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

// resume goes on with a task that waits for an operator, with the options of this
// request (the acceptance of a loss, the targets). The change of the task from paused
// to pending and the change of the options are one transaction.
func (s *Service) resume(ctx context.Context, eng *task.Engine, a *activeRun, actor string, in Input) (*task.Task, *Plan, error) {
	opts, _ := json.Marshal(in)
	err := s.sys(ctx, func(c *pgx.Conn) error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		tag, err := tx.Exec(ctx, `UPDATE kingres_cp.task SET state = 'pending', cancel_requested = false, updated_at = clock_timestamp()
			WHERE id = $1 AND state = 'paused'`, a.ID)
		if err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && (pe.Code == "KX001" || pe.Code == "KX002" || pe.Code == "KX003") {
				return &task.ExcludedError{Kind: KindFailover, Other: pe.Detail}
			}
			return err
		}
		if tag.RowsAffected() != 1 {
			return task.ErrNotPaused
		}
		if _, err := tx.Exec(ctx, `UPDATE kingres_cp.failover_run SET options = $2 WHERE task_id = $1`, a.ID, opts); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	var x *task.ExcludedError
	switch {
	case errors.Is(err, task.ErrNotPaused):
		return nil, nil, problem.New(problem.FailoverInProgress, "A failover is already running.")
	case errors.As(err, &x):
		return nil, nil, x.Problem()
	case err != nil:
		return nil, nil, err
	}
	s.run(eng, a.ID)
	t, gerr := eng.Get(ctx, a.ID)
	if gerr != nil {
		return nil, nil, gerr
	}
	return &t, nil, nil
}

func (s *Service) run(eng *task.Engine, id int64) {
	bg := context.Background()
	if s.BGFn != nil {
		bg = s.BGFn()
	}
	f := func() {
		if err := eng.RunNow(bg, id, nil); err != nil && bg.Err() == nil {
			s.log().Error("failover task ended with an error", "task", id, "error", err)
		}
	}
	if s.Background != nil {
		s.Background(f)
		return
	}
	go f()
}

// ---------------------------------------------------------------------------------
// The read side: status and reports.

type runSummary struct {
	TaskID      int64
	Trigger     string
	Mode        string
	Server      string
	RequestedBy string
	Outcome     string
	StartedAt   time.Time
	FinishedAt  *time.Time
	Error       string
	TaskState   string
	Options     []byte
}

func (s *Service) runs(ctx context.Context, c *pgx.Conn, where string, args ...any) ([]runSummary, error) {
	rows, err := c.Query(ctx, `SELECT r.task_id, r.trigger, coalesce(r.mode, ''), coalesce(r.server_id, ''), coalesce(r.requested_by, ''), coalesce(r.outcome, ''),
		r.started_at, r.finished_at, coalesce(r.error, ''), coalesce(t.state, ''), r.options
		FROM kingres_cp.failover_run r LEFT JOIN kingres_cp.task t ON t.id = r.task_id WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runSummary
	for rows.Next() {
		var x runSummary
		if err := rows.Scan(&x.TaskID, &x.Trigger, &x.Mode, &x.Server, &x.RequestedBy, &x.Outcome, &x.StartedAt, &x.FinishedAt, &x.Error, &x.TaskState, &x.Options); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func summaryOf(c *pgx.Conn, ctx context.Context, x runSummary) (ReportSummary, error) {
	out := ReportSummary{ID: x.TaskID, Kind: "failover", Trigger: x.Trigger, Outcome: x.Outcome, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt, RequestedBy: x.RequestedBy}
	if x.Server != "" {
		out.ServerID = &x.Server
	}
	if x.Error != "" {
		out.Error = &x.Error
	}
	switch {
	case x.TaskState == "paused":
		out.Outcome = "paused"
	case x.Outcome == "" && x.TaskState == "failed":
		out.Outcome = "failed"
	case x.Outcome == "":
		out.Outcome = "partial"
	}
	rows, err := c.Query(ctx, `SELECT state, data_loss FROM kingres_cp.failover_group WHERE task_id = $1`, x.TaskID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var dl []byte
		if err := rows.Scan(&state, &dl); err != nil {
			return out, err
		}
		out.Totals.GroupsTotal++
		switch state {
		case "done":
			out.Totals.GroupsDone++
		case "failed":
			out.Totals.GroupsFailed++
		case "skipped":
			out.Totals.GroupsSkipped++
		}
		var d DataLoss
		if json.Unmarshal(dl, &d) == nil && d.Verdict != "" && d.Verdict != VerdictNone {
			out.DataLossPossible = true
		}
	}
	end := time.Now()
	if x.FinishedAt != nil {
		end = *x.FinishedAt
	}
	out.Totals.DurationSeconds = int64(end.Sub(x.StartedAt).Seconds())
	return out, rows.Err()
}

// Status implements getFailoverStatus.
func (s *Service) Status(ctx context.Context) (Status, error) {
	out := Status{State: "idle", NeedsDecision: []Blocker{}, Groups: []GroupProgress{}}
	err := s.sys(ctx, func(c *pgx.Conn) error {
		var id int64
		var state string
		err := c.QueryRow(ctx, `SELECT id, state FROM kingres_cp.task WHERE kind = $1 AND state IN ('pending', 'running', 'paused') ORDER BY id DESC LIMIT 1`, KindFailover).Scan(&id, &state)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			runs, err := s.runs(ctx, c, `r.task_id = $1`, id)
			if err != nil {
				return err
			}
			out.State = "running"
			if state == "paused" {
				out.State = "paused"
			}
			out.TaskID = &id
			k := "failover"
			out.Kind = &k
			if len(runs) == 1 {
				x := runs[0]
				out.Trigger, out.ServerID, out.StartedAt = &x.Trigger, &x.Server, &x.StartedAt
			}
			if t, gerr := s.Engine().Get(ctx, id); gerr == nil {
				out.Cancellable = Cancellable(t)
				if state == "paused" && t.Detail != "" {
					d := t.Detail
					out.PausedReason = &d
				}
			}
			groups, err := s.groupProgress(ctx, c, id)
			if err != nil {
				return err
			}
			out.Groups = groups
			if state == "paused" {
				for _, g := range groups {
					if g.State != "done" && g.Message != nil {
						gg := g.ShardGroupID
						out.NeedsDecision = append(out.NeedsDecision, Blocker{Code: "failoverNeedsDecision", ShardGroupID: &gg, Message: *g.Message})
					}
				}
			}
		}
		last, err := s.runs(ctx, c, `r.finished_at IS NOT NULL AND ($1::bigint = 0 OR r.task_id <> $1) ORDER BY r.task_id DESC LIMIT 1`, orID(out.TaskID))
		if err != nil {
			return err
		}
		if len(last) == 1 {
			sm, err := summaryOf(c, ctx, last[0])
			if err != nil {
				return err
			}
			out.LastReport = &sm
			if out.State == "idle" && sm.Outcome == "failed" {
				out.State = "failed"
			}
		}
		return nil
	})
	return out, err
}

func orID(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Service) groupProgress(ctx context.Context, c *pgx.Conn, id int64) ([]GroupProgress, error) {
	rows, err := c.Query(ctx, `SELECT group_id, state, coalesce(old_server, ''), new_server, nullif(error, '') FROM kingres_cp.failover_group WHERE task_id = $1 ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupProgress
	for rows.Next() {
		var g GroupProgress
		if err := rows.Scan(&g.ShardGroupID, &g.State, &g.FromServerID, &g.ToServerID, &g.Message); err != nil {
			return nil, err
		}
		ph := phaseOf(g.State)
		g.Phase = &ph
		out = append(out, g)
	}
	return out, rows.Err()
}

func phaseOf(state string) string {
	switch state {
	case "pending":
		return "checking"
	case "frozen":
		return "waitingForLease"
	case "fenced":
		return "choosing"
	case "pinned":
		return "promoting"
	case "promoted":
		return "followingCatalog"
	}
	return "finishing"
}

// Reports implements listFailoverReports. The cursor is the id of the last report of
// the previous page.
func (s *Service) Reports(ctx context.Context, limit int, cursor string) (ReportPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	var before int64 = 1<<62 - 1
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 1 {
			return ReportPage{}, problem.New(problem.BadRequest, "The cursor is not valid.")
		}
		before = n
	}
	page := ReportPage{Items: []ReportSummary{}}
	err := s.sys(ctx, func(c *pgx.Conn) error {
		runs, err := s.runs(ctx, c, `(r.finished_at IS NOT NULL OR t.state = 'paused') AND r.task_id < $1 ORDER BY r.task_id DESC LIMIT $2`, before, limit+1)
		if err != nil {
			return err
		}
		for i, x := range runs {
			if i == limit {
				next := strconv.FormatInt(runs[limit-1].TaskID, 10)
				page.NextCursor = &next
				break
			}
			sm, err := summaryOf(c, ctx, x)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, sm)
		}
		return nil
	})
	return page, err
}

// Report implements getFailoverReport.
func (s *Service) Report(ctx context.Context, id int64) (Report, error) {
	var out Report
	err := s.sys(ctx, func(c *pgx.Conn) error {
		runs, err := s.runs(ctx, c, `r.task_id = $1`, id)
		if err != nil {
			return err
		}
		if len(runs) != 1 {
			return problem.New(problem.NotFound, "No failover report "+strconv.FormatInt(id, 10)+".")
		}
		x := runs[0]
		if out.ReportSummary, err = summaryOf(c, ctx, x); err != nil {
			return err
		}
		var in Input
		_ = json.Unmarshal(x.Options, &in)
		out.Mode = x.Mode
		out.Options = Input2{Mode: in.Mode, ShardGroupIDs: in.Groups, AllowDataLoss: in.AllowDataLoss, Comment: in.Comment}
		for g, srv := range in.Targets {
			out.Options.Targets = append(out.Options.Targets, TargetRequest{ShardGroupID: g, ServerID: srv})
		}
		sort.Slice(out.Options.Targets, func(i, j int) bool { return out.Options.Targets[i].ShardGroupID < out.Options.Targets[j].ShardGroupID })
		out.Groups, out.Events = []GroupReport{}, []Event{}
		rows, err := c.Query(ctx, `SELECT `+groupCols+` FROM kingres_cp.failover_group WHERE task_id = $1 ORDER BY seq`, id)
		if err != nil {
			return err
		}
		var grows []GroupRow
		for rows.Next() {
			g, err := scanGroup(rows)
			if err != nil {
				rows.Close()
				return err
			}
			grows = append(grows, g)
		}
		rows.Close()
		for _, g := range grows {
			out.Groups = append(out.Groups, groupReport(g))
		}
		ev, err := c.Query(ctx, `SELECT at, group_id, coalesce(step, ''), kind, coalesce(detail::text, '') FROM kingres_cp.failover_event WHERE task_id = $1 ORDER BY seq`, id)
		if err != nil {
			return err
		}
		defer ev.Close()
		for ev.Next() {
			var e Event
			var detail string
			if err := ev.Scan(&e.At, &e.ShardGroupID, &e.Step, &e.Kind, &detail); err != nil {
				return err
			}
			e.Message = detail
			out.Events = append(out.Events, e)
		}
		return ev.Err()
	})
	return out, err
}

func groupReport(g GroupRow) GroupReport {
	r := GroupReport{ShardGroupID: g.Group, FromServerID: g.OldServer, TermBefore: g.TermBefore, TermAfter: g.TermAfter, Timeline: g.Timeline, LeaseWaitMs: g.LeaseWaitMs,
		Candidates: []CandidateOut{}, UnreachedServerIDs: []string{}, Incidents: []IncidentOut{}, DataLoss: DataLoss{Verdict: VerdictUnknown, AsyncDatabases: []string{}, UnreadableServerIDs: []string{}}}
	switch g.State {
	case "done", "failed", "skipped":
		r.Outcome = g.State
	default:
		r.Outcome = "pending"
	}
	if g.NewServer != "" {
		r.ToServerID = &g.NewServer
	}
	if g.Error != "" {
		r.Error = &g.Error
	}
	if g.StartedAt != nil {
		end := time.Now()
		if g.FinishedAt != nil {
			end = *g.FinishedAt
		}
		r.DurationSeconds = int64(end.Sub(*g.StartedAt).Seconds())
	}
	_ = json.Unmarshal(g.Decision, &r.Candidates)
	_ = json.Unmarshal(g.DataLoss, &r.DataLoss)
	_ = json.Unmarshal(g.Incidents, &r.Incidents)
	var un []string
	if json.Unmarshal(g.Unreached, &un) == nil {
		r.UnreachedServerIDs = append(r.UnreachedServerIDs, un...)
	}
	op := OldPrimary{State: "waitingForServer"}
	switch g.OldPrimary {
	case "rebuildNotScheduled":
		op.State = "notScheduled"
		m := "The old primary is not turned into a standby again by this version of kingresd; it stays stopped and fenced."
		op.Message = &m
	case "waitingForServer", "":
	default:
		op.State = g.OldPrimary
	}
	r.OldPrimary = op
	return r
}
