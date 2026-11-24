// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Draining a server (design shard-phase10.md, section 11).
//
// Departure from the design, on purpose: a drain is a task of kind "rebalance" (subject cluster/cluster) whose
// options say trigger "drain", and not a task of its own kind. Everything that already treats a rebalance as the
// operation that excludes server operations (the database triggers, the stop, the resume, the report, the
// alert, the failover's pause) then holds for a drain without a second copy of it. The task has the extra steps
// that only a drain runs (they are skipped by a rebalance): the flag is set first, the move is the rebalance's
// loop with drainOnly, the end waits until the server owns no bucket and has none arriving, and the removal,
// when asked for, is made by the "server.remove" task after the drain task has ended (the two exclude each other).
// Standby relocation and the hand-over of the map group (steps 4 and 5 of the design) are not part of this
// increment: a server that hosts the map group primary is refused with server_hosts_map_group, and a drain that
// would leave fewer servers than replicaCount + 1 is refused with replicas_cannot_be_kept.

// Drain step names (shown to users); the first of a rebalance's steps is shared.
const (
	StepDrainFlag     = "Stop new buckets from landing on the server"
	StepDrainEmpty    = "Wait until the server owns no bucket"
	StepDrainStandbys = "Relocate the standbys hosted on the server"
	StepDrainEnd      = "Remove the server"
)

// DrainRequest is the body of drainServer.
type DrainRequest struct {
	RemoveWhenDone      bool
	MaxConcurrentMoves  int
	AcceptFewerReplicas bool
	AcknowledgeLocal    bool
}

// nodeRow is one row of the shard catalog.
type nodeRow struct {
	ID     int32
	Group  int32
	Host   string
	Role   string
	Active bool
	Should bool
}

// readNodes reads kingres.node from the system database (where the extension is installed).
func (s *Service) readNodes(ctx context.Context) ([]nodeRow, error) {
	var out []nodeRow
	err := s.Backend.with(ctx, sysdb.Database, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT node_id, group_id, host, role, is_active, should_have_shards FROM kingres.node ORDER BY node_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n nodeRow
			if err := rows.Scan(&n.ID, &n.Group, &n.Host, &n.Role, &n.Active, &n.Should); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	})
	return out, err
}

// DrainFacts is what the drain's checks need to know, read once.
type DrainFacts struct {
	Servers       []string
	ReplicaCount  int
	HostsMapGroup bool
	// Receivers are the other servers that hold an active primary and have the flag.
	Receivers []string
	// StandbyGroups are the groups whose standby runs on the server.
	StandbyGroups []int32
	// PrimaryGroups are the groups whose primary runs on the server.
	PrimaryGroups []int32
	// Degraded are groups of the server (primary here) that have fewer active standbys than the replica count.
	Degraded []int32
}

// Facts reads the topology for server id; the problem is 404 for a server that is not in the catalog.
func (s *Service) drainFacts(ctx context.Context, id string) (DrainFacts, error) {
	var f DrainFacts
	nodes, err := s.readNodes(ctx)
	if err != nil {
		return f, err
	}
	hosts := map[string]bool{}
	standbys := map[int32]int{}
	for _, n := range nodes {
		hosts[n.Host] = true
		if n.Role == "standby" && n.Active {
			standbys[n.Group]++
		}
	}
	if !hosts[id] {
		return f, problem.New(problem.NotFound, "Server "+id+" does not exist.")
	}
	for h := range hosts {
		f.Servers = append(f.Servers, h)
	}
	sort.Strings(f.Servers)
	for _, c := range standbys {
		f.ReplicaCount = max(f.ReplicaCount, c)
	}
	recv := map[string]bool{}
	for _, n := range nodes {
		switch {
		case n.Host == id && n.Role == "primary":
			f.PrimaryGroups = append(f.PrimaryGroups, n.Group)
			if n.Group == 0 {
				f.HostsMapGroup = true
			}
			if f.ReplicaCount > 0 && standbys[n.Group] < f.ReplicaCount {
				f.Degraded = append(f.Degraded, n.Group)
			}
		case n.Host == id && n.Role == "standby":
			f.StandbyGroups = append(f.StandbyGroups, n.Group)
		case n.Host != id && n.Role == "primary" && n.Active && n.Should:
			recv[n.Host] = true
		}
	}
	for h := range recv {
		f.Receivers = append(f.Receivers, h)
	}
	sort.Strings(f.Receivers)
	return f, nil
}

// drainRefusal is the first reason that stops a drain of server id (design 11.3, step 1), nil when none does.
func (s *Service) drainRefusal(ctx context.Context, id string, req DrainRequest, f DrainFacts) error {
	if len(f.Servers) <= 1 {
		return problem.New(problem.LastServer, "Server "+id+" is the last server of the cluster; its buckets have nowhere to go.")
	}
	if s.ServerStatus != nil {
		if st := s.ServerStatus(ctx, id); st == "dead" {
			return problem.New(problem.NodeStateInvalid, "Server "+id+" is dead; fail it over first, then remove it.")
		}
	}
	if len(f.Receivers) == 0 {
		return problem.New(problem.NoReceivingServer, "No other server should hold shards and runs a primary; the buckets of "+id+" have nowhere to go.")
	}
	if f.HostsMapGroup {
		return problem.New(problem.ServerHostsMapGroup, "Server "+id+" runs the primary of the map group (the manager leader). Fail the map group over to a standby on another server first.")
	}
	if remaining := len(f.Servers) - 1; remaining < f.ReplicaCount+1 && !req.AcceptFewerReplicas {
		return problem.New(problem.ReplicasCannotBeKept, fmt.Sprintf(
			"The cluster keeps %d standby copies of each group; %d servers would remain, and no server may hold two instances of a group. Lower the replica count first, or accept fewer copies (acceptFewerReplicas).", f.ReplicaCount, remaining))
	}
	if len(f.Degraded) > 0 {
		return problem.New(problem.GroupDegraded, fmt.Sprintf("Shard group %v of server %s has fewer healthy standbys than the replica count; a drain would move buckets of an unprotected group.", f.Degraded, id))
	}
	return nil
}

// DrainPreview is previewServerDrain.
func (s *Service) DrainPreview(ctx context.Context, id string) (api.DrainPreview, error) {
	out := api.DrainPreview{ServerID: id, Moves: []api.PlannedMove{}, StandbyGroups: []int32{}, Blockers: []api.DrainBlocker{}, LocalData: []api.DrainLocalData{}}
	f, err := s.drainFacts(ctx, id)
	if err != nil {
		return out, err
	}
	out.StandbyGroups = append(out.StandbyGroups, f.StandbyGroups...)
	out.CanKeepReplicas = len(f.Servers)-1 >= f.ReplicaCount+1
	if err := s.drainRefusal(ctx, id, DrainRequest{AcceptFewerReplicas: true}, f); err != nil {
		var p *problem.Problem
		if errors.As(err, &p) {
			out.Blockers = append(out.Blockers, api.DrainBlocker{Code: string(p.Code), Detail: p.Detail})
		}
	}
	if !out.CanKeepReplicas {
		out.Blockers = append(out.Blockers, api.DrainBlocker{Code: string(problem.ReplicasCannotBeKept),
			Detail: "Fewer servers would remain than the replica count needs; the request must say acceptFewerReplicas."})
	}
	plan, err := s.Preview(ctx, api.RebalancePlanRequest{RebalanceOptions: api.RebalanceOptions{DrainOnly: true}, ExcludeServerIDs: []string{id}, MoveLimit: 200})
	if err != nil {
		return out, err
	}
	out.Moves, out.MoveCount, out.TotalBytes, out.Warnings = plan.Moves, plan.Totals.MoveCount, plan.Totals.TotalBytes, plan.Warnings
	out.Strategy = plan.Strategy
	out.LocalData = s.localData(ctx, id)
	out.CanDrain = len(out.Blockers) == 0
	return out, nil
}

// localData lists the databases in which ordinary (not distributed, not reference) tables exist; their rows
// are per instance and leave with the server (design 11.5). The server's own instance is asked.
func (s *Service) localData(ctx context.Context, id string) []api.DrainLocalData {
	out := []api.DrainLocalData{}
	dbs, err := s.Backend.Databases(ctx, Options{})
	if err != nil {
		return out
	}
	for _, db := range dbs {
		_ = s.Backend.withShard(ctx, db, func(c *pgx.Conn) error {
			var n int
			var size *int64
			// the catalog's node on this server; the table list is the local one of the node that answers
			err := c.QueryRow(ctx, `SELECT count(*), sum(pg_total_relation_size(c.oid))::bigint FROM pg_class c
				JOIN pg_namespace ns ON ns.oid = c.relnamespace
				WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition AND ns.nspname NOT IN ('pg_catalog', 'information_schema', 'kingres', 'kingres_ledger')
				AND ns.nspname !~ '^pg_toast' AND NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid = c.oid)`).Scan(&n, &size)
			if err == nil && n > 0 {
				out = append(out, api.DrainLocalData{DatabaseName: db, TableCount: n, SizeBytes: size})
			}
			return nil
		})
	}
	return out
}

// Drain is drainServer: refuses what can be refused before a task exists, then starts the run.
func (s *Service) Drain(ctx context.Context, actor, id string, req DrainRequest) (task.Task, error) {
	f, err := s.drainFacts(ctx, id)
	if err != nil {
		return task.Task{}, err
	}
	if err := s.drainRefusal(ctx, id, req, f); err != nil {
		return task.Task{}, err
	}
	if loc := s.localData(ctx, id); len(loc) > 0 && !req.AcknowledgeLocal {
		names := make([]string, 0, len(loc))
		for _, l := range loc {
			names = append(names, l.DatabaseName)
		}
		return task.Task{}, problem.New(problem.Unprocessable, "The server holds ordinary tables in the databases "+strings.Join(names, ", ")+
			" that a drain does not move; their data leaves with the server. Say acknowledgeLocalData to go on.")
	}
	o := Options{DrainOnly: true, Trigger: TriggerDrain, DrainServer: id, RemoveWhenDone: req.RemoveWhenDone,
		AcceptFewerReplicas: req.AcceptFewerReplicas, MaxConcurrentMoves: req.MaxConcurrentMoves}
	return s.start(ctx, actor, o, true)
}

// stepDrainFlag sets shouldHaveShards false on the server (a drain only).
func (s *Service) stepDrainFlag(ctx context.Context, _ *task.Env, _ *PGRows, o Options) error {
	if o.DrainServer == "" {
		return task.Skip("not a drain")
	}
	if s.SetShouldHaveShards == nil {
		return errors.New("the drain cannot change the flag of a server on this build")
	}
	return s.SetShouldHaveShards(ctx, o.DrainServer, false)
}

// stepDrainEmpty waits until no bucket is on the server and none is arriving or leaving (a drain only).
func (s *Service) stepDrainEmpty(ctx context.Context, env *task.Env, _ *PGRows, o Options) error {
	if o.DrainServer == "" {
		return task.Skip("not a drain")
	}
	deadline := time.Now().Add(s.drainWait())
	for {
		left, err := s.bucketsOn(ctx, o.DrainServer)
		if err != nil {
			return err
		}
		if left == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return problem.New(problem.ServerOwnsBuckets, "Server "+o.DrainServer+" still owns "+strconv.Itoa(left)+" buckets (or a move away from it has not been cleaned up) after the drain; start it again.")
		}
		_ = env.SetDetail(ctx, fmt.Sprintf("%d buckets are still on %s", left, o.DrainServer))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Service) drainPoll() time.Duration {
	if s.DrainPoll > 0 {
		return s.DrainPoll
	}
	return 3 * time.Second
}

func (s *Service) drainWait() time.Duration {
	if s.DrainWait > 0 {
		return s.DrainWait
	}
	return 30 * time.Minute
}

// bucketsOn counts, over the databases with distributed tables, the buckets that groups with a primary on the
// server own plus the moves that are not finished with them (a bucket in cleaning still has its old copy).
func (s *Service) bucketsOn(ctx context.Context, server string) (int, error) {
	// Strict: a database that cannot be verified is an error here, never a database with no buckets (the drain would report done with buckets still on the server).
	dbs, err := s.Backend.Databases(ctx, Options{Strict: true})
	if err != nil {
		return 0, err
	}
	total := 0
	for _, db := range dbs {
		var n int
		err := s.Backend.withShard(ctx, db, func(c *pgx.Conn) error {
			return c.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM kingres.bucket_map m JOIN kingres.node n ON n.group_id = m.group_id AND n.role = 'primary' AND n.is_active AND n.host = $1)
				+ coalesce((SELECT count(*) FROM kingres.bucket_move mv JOIN kingres.node n ON n.group_id = mv.source_group AND n.role = 'primary' AND n.is_active AND n.host = $1
					WHERE mv.phase IN ('preparing', 'copying', 'catching_up', 'switching', 'cleaning')), 0)`, server).Scan(&n)
		})
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// stepDrainStandbys takes the server out of the ring of the standbys and waits until it hosts none (design 11.3 step 5): the
// reconcile of phase 5 creates the replacements on the servers that remain before it removes the old ones. With
// acceptFewerReplicas a group that cannot be placed anywhere else stays with fewer standbys after the wait; without it the
// step fails and the operator lowers the replica count. A drain without the hook (a build without standby management) skips it.
func (s *Service) stepDrainStandbys(ctx context.Context, env *task.Env, _ *PGRows, o Options) error {
	if o.DrainServer == "" {
		return task.Skip("not a drain")
	}
	if s.RelocateStandbys == nil {
		return task.Skip("this build does not manage standbys")
	}
	deadline := time.Now().Add(s.drainWait())
	for {
		left, err := s.RelocateStandbys(ctx, o.DrainServer)
		if err != nil {
			return err
		}
		if left == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			if o.AcceptFewerReplicas {
				if env != nil {
					_ = env.SetDetail(ctx, fmt.Sprintf("%d standbys stay on %s: no other server can take them", left, o.DrainServer))
				}
				return nil
			}
			return problem.New(problem.ReplicasCannotBeKept, fmt.Sprintf("%d standbys are still on server %s and no other server can take them; lower the replica count or accept fewer copies.", left, o.DrainServer))
		}
		if env != nil {
			_ = env.SetDetail(ctx, fmt.Sprintf("%d standbys are still on %s", left, o.DrainServer))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.drainPoll()):
		}
	}
}

// stepDrainEnd starts the removal of the server after this task has ended (a drain with removeWhenDone only).
func (s *Service) stepDrainEnd(ctx context.Context, env *task.Env, _ *PGRows, o Options) error {
	if o.DrainServer == "" || !o.RemoveWhenDone {
		return task.Skip("the server stays")
	}
	if s.RemoveServer == nil {
		return nil
	}
	id, server, actor := env.Task.ID, o.DrainServer, env.Task.CreatedBy
	f := func() { s.removeAfter(id, server, actor) }
	if s.Background != nil {
		s.Background(f)
	} else {
		go f()
	}
	return nil
}

// removeAfter waits for the drain task to leave the running state (the removal is excluded while it runs) and
// removes the server. A leader that stops in between leaves the server drained and not removed; the operator
// removes it with removeServer.
func (s *Service) removeAfter(taskID int64, server, actor string) {
	bg := context.Background()
	if s.BGFn != nil {
		bg = s.BGFn()
	}
	eng := s.Engine()
	for i := 0; i < 120 && bg.Err() == nil && eng != nil; i++ {
		t, err := eng.Get(bg, taskID)
		if err == nil && t.State == task.Done {
			if _, err := s.RemoveServer(bg, actor, server); err != nil {
				s.log().Warn("drain: removing the drained server", "server", server, "error", err)
			}
			return
		}
		if err == nil && (t.State == task.Failed) {
			return
		}
		select {
		case <-bg.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
