// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The restore of the preferred primaries (design ha-phase5-part2.md, 5.5): after a failed server has come back and its standbys are ready
// again, every group whose primary is not on its preferred server is handed back by a switchover, one group at a time. It is the steps of
// a switchover over other groups: the old primary of a group is the server that took it over, the target is the preferred server.

// RestoreRequest is the body of restorePreferredPrimaries (contract RestorePreferredPrimariesRequest); every member is optional.
type RestoreRequest struct {
	ShardGroupIDs []int32 `json:"shardGroupIds"`
	DryRun        bool    `json:"dryRun"`
}

// ResolveRestore turns a request into the input of the task: the groups that are not on their preferred server and whose preferred server
// has a ready standby, with the server that runs each now (Olds) and the target (Targets); the other groups are Skipped with the reason.
// The preferred server of a group is the server of its first node by id (the extension keeps the node ids across a promotion, so the home
// node stays the first).
func (s *Service) ResolveRestore(ctx context.Context, actor string, req RestoreRequest) (Input, error) {
	var errs []problem.FieldError
	seen := map[int32]bool{}
	for i, g := range req.ShardGroupIDs {
		if g < 0 || seen[g] {
			errs = append(errs, problem.FieldError{Field: "/shardGroupIds/" + strconv.Itoa(i), Message: "must be a group id, once", Code: "out_of_range"})
		}
		seen[g] = true
	}
	if len(errs) > 0 {
		return Input{}, problem.Validation("The restore request is invalid.", errs...)
	}
	nodes, err := s.Runner.Cat.Nodes(ctx, SystemDB)
	if err != nil {
		return Input{}, problem.New(problem.ServiceUnavailable, "The catalog cannot be read now.").WithRetryAfter(5)
	}
	return s.restoreInput(ctx, actor, nodes, req.ShardGroupIDs)
}

// restoreInput is ResolveRestore after the request is checked.
func (s *Service) restoreInput(ctx context.Context, actor string, nodes []Node, only []int32) (Input, error) {
	home := map[int32]Node{}    // the first node of the group by id
	primary := map[int32]Node{} // the node that is the primary now
	for _, n := range nodes {
		if h, ok := home[n.Group]; !ok || n.ID < h.ID {
			home[n.Group] = n
		}
		if n.Role == "primary" {
			primary[n.Group] = n
		}
	}
	want := map[int32]bool{}
	for _, g := range only {
		if _, ok := home[g]; !ok {
			return Input{}, problem.New(problem.Unprocessable, fmt.Sprintf("There is no shard group %d.", g))
		}
		want[g] = true
	}
	groups := make([]int32, 0, len(home))
	for g := range home {
		if len(only) == 0 || want[g] {
			groups = append(groups, g)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	in := Input{Mode: ModeRestore, Targets: map[int32]string{}, Olds: map[int32]string{}, Trigger: "manual", RequestedBy: actor}
	for _, g := range groups {
		h, p := home[g], primary[g]
		if p.Host == "" || p.Host == h.Host {
			continue // at home (or without a primary: nothing to hand over)
		}
		recs, err := s.Runner.Plan.Standbys(ctx, g)
		if err != nil {
			return Input{}, problem.New(problem.ServiceUnavailable, "The standbys cannot be read now.").WithRetryAfter(5)
		}
		ready := false
		for _, st := range recs {
			if st.Server == h.Host && st.State == "ready" && st.CatalogNode != 0 {
				ready = true
			}
		}
		if !ready {
			in.Skipped = append(in.Skipped, SkippedGroup{Group: g, Reason: "preferredNotReady",
				Text: fmt.Sprintf("The preferred server %s has no ready standby of shard group %d (it was never rebuilt, or its server is not live).", h.Host, g)})
			continue
		}
		in.Groups = append(in.Groups, g)
		in.Olds[g], in.Targets[g] = p.Host, h.Host
	}
	return in, nil
}

// StartRestore checks a request and starts the task, answers the plan for a dry run, or resumes a restore that waits for an operator. It
// returns exactly one of the task and the plan.
func (s *Service) StartRestore(ctx context.Context, actor string, req RestoreRequest) (*task.Task, *Plan, error) {
	eng := s.Engine()
	if eng == nil {
		return nil, nil, problem.New(problem.ServiceUnavailable, "The task engine is not running yet.").WithRetryAfter(5)
	}
	a, err := s.active(ctx)
	if err != nil {
		return nil, nil, problem.New(problem.ServiceUnavailable, "The system database is not available.").WithRetryAfter(5)
	}
	if a != nil && (a.State != task.Paused || a.Server != SubjectRestoreID) {
		return nil, nil, problem.New(problem.FailoverInProgress, "A failover or a restore is unfinished (task "+strconv.FormatInt(a.ID, 10)+"); wait for it to end.")
	}
	if a != nil && !req.DryRun {
		// A restore that waits for an operator goes on with the groups it has: what it decided at its start is kept.
		return s.resumeRestore(ctx, eng, a)
	}
	in, err := s.ResolveRestore(ctx, actor, req)
	if err != nil {
		return nil, nil, err
	}
	if len(in.Groups) == 0 {
		if req.DryRun {
			p, perr := s.Plan(ctx, in)
			return nil, &p, perr
		}
		return nil, nil, problem.New(problem.Unprocessable, "No shard group needs to be restored: every group is on its preferred server, or its preferred server has no ready standby ("+skippedText(in)+").")
	}
	if ref := s.Runner.Preflight(ctx, in); ref != nil {
		if req.DryRun {
			p, perr := s.Plan(ctx, in)
			return nil, &p, perr
		}
		return nil, nil, refusalProblem(ref)
	}
	if req.DryRun {
		p, err := s.Plan(ctx, in)
		return nil, &p, err
	}
	t, err := eng.Submit(ctx, KindRestore, task.Subject{Type: SubjectRestore, ID: SubjectRestoreID}, in, actor)
	if errors.Is(err, task.ErrSubjectBusy) {
		return nil, nil, problem.New(problem.FailoverInProgress, "A restore of the preferred primaries is already running.")
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

func skippedText(in Input) string {
	if len(in.Skipped) == 0 {
		return "none skipped"
	}
	out := ""
	for i, sk := range in.Skipped {
		if i > 0 {
			out += "; "
		}
		out += fmt.Sprintf("group %d: %s", sk.Group, sk.Reason)
	}
	return out
}

// resumeRestore goes on with a restore that is paused, with the input it was started with.
func (s *Service) resumeRestore(ctx context.Context, eng *task.Engine, a *activeRun) (*task.Task, *Plan, error) {
	err := s.sys(ctx, func(c *pgx.Conn) error {
		tag, err := c.Exec(ctx, `UPDATE kingres_cp.task SET state = 'pending', cancel_requested = false, updated_at = clock_timestamp()
			WHERE id = $1 AND state = 'paused'`, a.ID)
		if err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && (pe.Code == "KX001" || pe.Code == "KX002" || pe.Code == "KX003" || pe.Code == "KX006") {
				return &task.ExcludedError{Kind: KindRestore, Other: pe.Detail}
			}
			return err
		}
		if tag.RowsAffected() != 1 {
			return task.ErrNotPaused
		}
		return nil
	})
	var x *task.ExcludedError
	switch {
	case errors.Is(err, task.ErrNotPaused):
		return nil, nil, problem.New(problem.FailoverInProgress, "A restore is already running.")
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
