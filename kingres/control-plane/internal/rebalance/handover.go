// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Step 4 of the drain (design shard-phase10.md, 11.3): hand over the map group.
//
// A planned switchover of the map group is a task of the failover class, and a failover task is refused while a rebalance is
// pending or running (migration 0009, KX002). The drain is a rebalance task, so the step cannot run the switchover while it
// runs itself. It does this instead:
//
//  1. It writes a marker (cluster setting drain.mapHandover) that names the drain task, pauses the task (a paused rebalance
//     does not exclude a failover) and, from a background goroutine that waits for the pause, asks for the planned switchover
//     of group 0 on the server (the hook HandOverMapGroup).
//  2. The goroutine records "started" (the switchover is under way) or "refused" (it could not be started, with the problem).
//  3. The switchover ends the old leader: its system database is stopped cleanly and a standby of the map group becomes the
//     primary, so the manager leader is another server. The task rows, the marker and the pause are in the system database
//     and arrive there with the standby.
//  4. The job RunDrainWatch of the new leader resumes the paused drain once the marker says started or refused and no
//     failover task is unfinished (the database refuses the resume while one is). The step runs again from its start: the
//     server no longer hosts the primary of group 0, the step is done and the drain goes on with the standbys. A server that
//     still hosts it (the switchover was rolled back, or never ran) fails the step with the code of the refusal.

// mapHandoverKey is the cluster setting that holds the marker.
const mapHandoverKey = "drain.mapHandover"

// States of the marker.
const (
	HandoverRequested = "requested"
	HandoverStarted   = "started"
	HandoverRefused   = "refused"
)

// handoverGrace is how long a marker that says requested is waited for before the watch takes it as left behind by a leader that
// ended between the pause and the call (nothing was started, or the record of it was lost): the drain is resumed and its step decides.
const handoverGrace = 5 * time.Minute

// MapHandover is the marker of a hand-over in progress.
type MapHandover struct {
	TaskID  int64  `json:"taskId"`
	Server  string `json:"server"`
	State   string `json:"state"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// Age is how long ago the marker was written, by the clock of the database (not stored in the value).
	Age time.Duration `json:"-"`
}

// HandoverStore keeps the marker; the default keeps it in the system database.
type HandoverStore interface {
	Get(ctx context.Context) (*MapHandover, error)
	Put(ctx context.Context, m MapHandover) error
	Clear(ctx context.Context) error
}

type sysHandover struct{ s *Service }

func (h sysHandover) Get(ctx context.Context) (*MapHandover, error) {
	var m MapHandover
	err := h.s.sys(ctx, func(c *pgx.Conn) error {
		var raw []byte
		var age float64
		if err := c.QueryRow(ctx, `SELECT value, extract(epoch FROM clock_timestamp() - updated_at)::float8 FROM kingres_cp.cluster_setting WHERE key = $1`, mapHandoverKey).Scan(&raw, &age); err != nil {
			return err
		}
		m.Age = time.Duration(age * float64(time.Second))
		return json.Unmarshal(raw, &m)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (h sysHandover) Put(ctx context.Context, m MapHandover) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return h.s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, mapHandoverKey, b)
		return err
	})
}

func (h sysHandover) Clear(ctx context.Context) error {
	return h.s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `DELETE FROM kingres_cp.cluster_setting WHERE key = $1`, mapHandoverKey)
		return err
	})
}

func (s *Service) handoverStore() HandoverStore {
	if s.Handover != nil {
		return s.Handover
	}
	return sysHandover{s}
}

// handoverAction is what the step does on entry.
type handoverAction int

const (
	// handoverDone: the server does not host the map group primary (any more).
	handoverDone handoverAction = iota
	// handoverRequest: ask for the switchover and pause.
	handoverRequest
	// handoverFail: the switchover was refused or did not take place; the step fails with the code and the text.
	handoverFail
)

// decideHandover is the decision of the step, from the marker (nil when there is none), the id of the drain task and whether the
// server hosts the primary of group 0. A marker of another task is the leftover of an earlier drain and counts for nothing.
func decideHandover(m *MapHandover, taskID int64, hosts bool) (handoverAction, string, string) {
	if !hosts {
		return handoverDone, "", ""
	}
	if m == nil || m.TaskID != taskID {
		return handoverRequest, "", ""
	}
	switch m.State {
	case HandoverRefused:
		code, msg := m.Code, m.Message
		if code == "" {
			code = string(problem.ServerHostsMapGroup)
		}
		if msg == "" {
			msg = "The planned switchover of the map group was refused."
		}
		return handoverFail, code, msg
	case HandoverRequested:
		// a leader that ended between the record and the pause, or a drain resumed after the grace time: the request was not made (or its
		// record was lost), so it is made again
		return handoverRequest, "", ""
	case HandoverStarted:
		return handoverFail, string(problem.ServerHostsMapGroup),
			"The planned switchover of the map group did not complete: the server still runs its primary. Read the failover report, then start the drain again."
	}
	return handoverRequest, "", ""
}

// hostsMapGroup says whether the server runs the active primary of group 0.
func (s *Service) hostsMapGroup(ctx context.Context, server string) (bool, error) {
	if s.HostsMapGroup != nil {
		return s.HostsMapGroup(ctx, server)
	}
	nodes, err := s.readNodes(ctx)
	if err != nil {
		return false, err
	}
	for _, n := range nodes {
		if n.Group == 0 && n.Role == "primary" && n.Active && n.Host == server {
			return true, nil
		}
	}
	return false, nil
}

// stepDrainMapGroup is step 4 of the drain.
func (s *Service) stepDrainMapGroup(ctx context.Context, env *task.Env, rows *PGRows, o Options) error {
	if o.DrainServer == "" {
		return task.Skip("not a drain")
	}
	if s.HandOverMapGroup == nil {
		return task.Skip("this build has no planned switchover of the map group")
	}
	hosts, err := s.hostsMapGroup(ctx, o.DrainServer)
	if err != nil {
		return err
	}
	st := s.handoverStore()
	m, err := st.Get(ctx)
	if err != nil {
		return err
	}
	taskID := env.Task.ID
	act, code, msg := decideHandover(m, taskID, hosts)
	switch act {
	case handoverDone:
		if m != nil && m.TaskID == taskID {
			_ = st.Clear(ctx)
		}
		_ = env.SetDetail(ctx, "The map group does not run on "+o.DrainServer)
		return nil
	case handoverFail:
		_ = st.Clear(ctx)
		return problem.New(problem.Code(code), msg)
	}
	if err := st.Put(ctx, MapHandover{TaskID: taskID, Server: o.DrainServer, State: HandoverRequested}); err != nil {
		return err
	}
	text := "The map group runs on " + o.DrainServer + ": it is handed over to a standby on another server; the drain goes on when the new manager leader has taken over."
	if rows != nil {
		if err := rows.Settle(ctx, OutcomeStopped, "safetyRule", text, nil); err != nil {
			return err
		}
	}
	id, server, actor := taskID, o.DrainServer, env.Task.CreatedBy
	f := func() { s.handOverAfter(id, server, actor) }
	if s.Background != nil {
		s.Background(f)
	} else {
		go f()
	}
	return task.Pause(text)
}

// handOverAfter waits for the drain task to be paused (the switchover is refused while it runs), asks for the planned switchover of
// the map group and records what came of the request.
func (s *Service) handOverAfter(taskID int64, server, actor string) {
	bg := context.Background()
	if s.BGFn != nil {
		bg = s.BGFn()
	}
	st := s.handoverStore()
	refuse := func(code, msg string) {
		if err := st.Put(bg, MapHandover{TaskID: taskID, Server: server, State: HandoverRefused, Code: code, Message: msg}); err != nil {
			s.log().Warn("drain: recording the refusal of the hand-over of the map group", "error", err)
		}
	}
	paused := false
	for i := 0; i < 240 && bg.Err() == nil; i++ {
		if eng := s.Engine(); eng != nil {
			if t, err := eng.Get(bg, taskID); err == nil && t.State == task.Paused {
				paused = true
				break
			}
		}
		select {
		case <-bg.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	if bg.Err() != nil {
		return
	}
	if !paused {
		refuse(string(problem.OperationInProgress), "The drain did not pause, so the planned switchover of the map group could not start.")
		return
	}
	if err := s.HandOverMapGroup(bg, actor, server); err != nil {
		code, msg := string(problem.ServerHostsMapGroup), "The planned switchover of the map group could not be started; the log of kingresd has the reason."
		var p *problem.Problem
		if errors.As(err, &p) {
			code, msg = string(p.Code), p.Detail
		} else {
			s.log().Warn("drain: the planned switchover of the map group could not be started", "server", server, "error", err)
		}
		refuse(code, msg)
		return
	}
	s.audit(bg, "rebalance.mapGroupHandover", taskID, map[string]any{"server": server})
	if err := st.Put(bg, MapHandover{TaskID: taskID, Server: server, State: HandoverStarted}); err != nil {
		// The switchover runs; the watch resumes the drain after the grace time even without the record of it.
		s.log().Warn("drain: recording the start of the hand-over of the map group", "error", err)
	}
}

// ResumeDrain is one round of the watch: it resumes the drain that paused to hand over the map group, once the marker says that the
// request was made or refused and no failover is unfinished. It does nothing on a server that is not the manager leader. It reports
// whether it resumed a drain.
func (s *Service) ResumeDrain(ctx context.Context) (bool, error) {
	if s.HandOverMapGroup == nil || s.IsLeader != nil && !s.IsLeader(ctx) {
		return false, nil
	}
	cur, err := s.active(ctx)
	if err != nil || cur == nil || cur.State != task.Paused {
		return false, err
	}
	m, err := s.handoverStore().Get(ctx)
	if err != nil || m == nil || m.TaskID != cur.ID {
		return false, err
	}
	if !handoverReady(m) {
		return false, nil
	}
	run, err := (&PGRows{Conns: s.Conns, TaskID: cur.ID}).Run(ctx)
	if err != nil {
		return false, err
	}
	if run.Options.Trigger != TriggerDrain || run.Options.DrainServer == "" {
		return false, nil
	}
	eng := s.Engine()
	if eng == nil {
		return false, nil
	}
	err = s.sys(ctx, func(c *pgx.Conn) error { return ResumeRun(ctx, c, cur.ID, run.Options) })
	var x *task.ExcludedError
	switch {
	case errors.As(err, &x), errors.Is(err, task.ErrNotPaused):
		// the switchover is not over yet
		return false, nil
	case err != nil:
		return false, err
	}
	s.run(eng, cur.ID)
	s.audit(ctx, "rebalance.resumed", cur.ID, map[string]any{"actor": "system", "reason": "the map group was handed over"})
	return true, nil
}

// handoverReady says whether the watch may try to resume the drain for the marker.
func handoverReady(m *MapHandover) bool {
	switch m.State {
	case HandoverStarted, HandoverRefused:
		return true
	case HandoverRequested:
		return m.Age > handoverGrace
	}
	return false
}

// RunDrainWatch is ResumeDrain every few seconds until ctx ends.
func (s *Service) RunDrainWatch(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := s.ResumeDrain(ctx); err != nil && ctx.Err() == nil {
			s.log().Debug("drain: the hand-over of the map group cannot be looked at now", "error", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.drainPoll()):
		}
	}
}
