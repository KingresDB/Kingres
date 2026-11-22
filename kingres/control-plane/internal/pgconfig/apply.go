// SPDX-License-Identifier: AGPL-3.0-only

package pgconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

type applyInput struct {
	Changes []change `json:"changes"`
	Actor   string   `json:"actor"`
	Servers []string `json:"servers"`
	Reverts int64    `json:"reverts,omitempty"`
}

// busy reports whether a configuration task (apply or restart) is unfinished.
func (l *Leader) busy(ctx context.Context) bool {
	var n int
	err := l.Sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.task WHERE subject_type = $1 AND state IN ('pending', 'running', 'paused')`, SubjectType).Scan(&n)
	})
	return err == nil && n > 0
}

// StartApply checks a request and starts the task config.apply. Nothing is written before every
// change has passed.
func (l *Leader) StartApply(ctx context.Context, actor string, req api.ConfigChangeRequest, reverts int64) (task.Task, ApplyInfo, error) {
	sn, err := l.snapshot(ctx)
	if err != nil {
		return task.Task{}, ApplyInfo{}, serviceDown(err)
	}
	ck, err := l.check(ctx, sn, req)
	if err != nil {
		return task.Task{}, ApplyInfo{}, wrapProblem(err)
	}
	if err := asProblem(ck.problems); err != nil {
		return task.Task{}, ApplyInfo{}, err
	}
	if req.ExpectedRevision != nil {
		if err := l.checkExpected(ctx, sn, ck.changes, *req.ExpectedRevision); err != nil {
			return task.Task{}, ApplyInfo{}, err
		}
	}
	if l.busy(ctx) {
		return task.Task{}, ApplyInfo{}, problem.New(problem.OperationInProgress, "Another configuration task is still running.")
	}
	sortChanges(ck.changes)
	var servers []string
	for _, s := range sn.servers {
		servers = append(servers, s.ID)
	}
	info := ApplyInfo{Changes: describe(sn.values, ck.changes), Servers: servers, BasedOn: sn.rev}
	t, err := l.StartTask(ctx, KindApply, SubjectType, SubjectApply, applyInput{Changes: ck.changes, Actor: actor, Servers: servers, Reverts: reverts}, actor)
	return t, info, err
}

// checkExpected refuses a request that would overwrite a change the client has not seen: a value it changes was
// changed by a revision later than the one the client saw, and does not hold the wanted value already (P13R-11).
func (l *Leader) checkExpected(ctx context.Context, sn *snapshot, cs []change, expected int64) error {
	for _, c := range cs {
		last, err := l.st.lastRevision(ctx, c.Level, c.Scope, c.Name)
		if err != nil {
			return serviceDown(err)
		}
		if last <= expected {
			continue
		}
		var cur *string
		for _, v := range sn.values {
			if v.Name == c.Name && v.Level == c.Level && v.Scope == c.Scope {
				x := v.Value
				cur = &x
			}
		}
		if (cur == nil) == (c.Value == nil) && (cur == nil || *cur == *c.Value) {
			continue
		}
		return problem.New(problem.ConfigChangeSuperseded, fmt.Sprintf("%s was changed by revision %d after the one you saw (%d); look at it again.", c.Name, last, expected))
	}
	return nil
}

// StartRevert starts the change that puts a value back to what it was before a change.
func (l *Leader) StartRevert(ctx context.Context, actor string, id int64) (task.Task, ApplyInfo, error) {
	row, err := l.st.change(ctx, id)
	if err != nil {
		if errors.Is(err, errNoChange) {
			return task.Task{}, ApplyInfo{}, ErrNoSuchChange
		}
		return task.Task{}, ApplyInfo{}, err
	}
	latest, err := l.st.latestChange(ctx, row.Level, row.Scope, row.Name)
	if err != nil {
		return task.Task{}, ApplyInfo{}, err
	}
	if latest != row.ID {
		return task.Task{}, ApplyInfo{}, problem.New(problem.ConfigChangeSuperseded, "A later change touched the same value; revert that one first.")
	}
	item := api.ConfigChangeItem{Name: row.Name, Level: row.Level, Value: row.Old}
	if row.Level != LevelCluster {
		item.ScopeID = strp(row.Scope)
	}
	return l.StartApply(ctx, actor, api.ConfigChangeRequest{Changes: []api.ConfigChangeItem{item}}, row.ID)
}

func serviceDown(err error) error {
	return problem.New(problem.ServiceUnavailable, "The configuration of the servers cannot be read now: "+firstLine(err.Error())).WithRetryAfter(5)
}

func wrapProblem(err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	return serviceDown(err)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (l *Leader) applySteps(input json.RawMessage) ([]task.Step, error) {
	var in applyInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("pgconfig: the task input is not valid: %w", err)
	}
	steps := []task.Step{
		{Name: "Validate the change", Run: func(ctx context.Context, _ *task.Env) error { return l.stepValidate(ctx, in) }},
		{Name: "Record the change", Run: func(ctx context.Context, env *task.Env) error { return l.stepRecord(ctx, env, in) }},
	}
	for _, s := range in.Servers {
		steps = append(steps, task.Step{Name: "Apply on server " + s, Run: func(ctx context.Context, env *task.Env) error { return l.stepApply(ctx, env, s) }})
	}
	steps = append(steps, task.Step{Name: "Confirm", Run: func(ctx context.Context, env *task.Env) error { return l.stepConfirm(ctx, in) }})
	return steps, nil
}

// stepValidate checks the request again: the state may have moved since it was accepted.
func (l *Leader) stepValidate(ctx context.Context, in applyInput) error {
	if len(in.Changes) == 0 {
		return nil // a task that only brings servers up to date
	}
	sn, err := l.snapshot(ctx)
	if err != nil {
		return err
	}
	var req api.ConfigChangeRequest
	for _, c := range in.Changes {
		if c.Value == nil && !hasStored(sn.values, c.Name, c.Level, c.Scope) {
			continue // the removal is done already (a retry of a task that recorded its change)
		}
		it := api.ConfigChangeItem{Name: c.Name, Level: c.Level, Value: c.Value}
		if c.Level != LevelCluster {
			it.ScopeID = strp(c.Scope)
		}
		req.Changes = append(req.Changes, it)
	}
	ck, err := l.check(ctx, sn, req)
	if err != nil {
		return err
	}
	if len(ck.problems) > 0 {
		return &taskError{code: "validation_failed", msg: ck.problems[0].Message}
	}
	return nil
}

// stepRecord writes the new values and the history in one transaction. It is idempotent: the history
// rows of a task are unique, and a task that recorded its change finds them again.
func (l *Leader) stepRecord(ctx context.Context, env *task.Env, in applyInput) error {
	return l.Sys(ctx, func(c *pgx.Conn) error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		var have int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.config_change WHERE task_id = $1`, env.Task.ID).Scan(&have); err != nil {
			return err
		}
		if have > 0 {
			return nil
		}
		type diff struct {
			c        change
			old, new *string
		}
		var diffs []diff
		for _, ch := range in.Changes {
			var old *string
			var v string
			err := tx.QueryRow(ctx, `SELECT value FROM kingres_cp.config_value WHERE level = $1 AND scope_id = $2 AND name = $3 FOR UPDATE`, ch.Level, ch.Scope, ch.Name).Scan(&v)
			switch {
			case err == nil:
				old = &v
			case errors.Is(err, pgx.ErrNoRows):
			default:
				return err
			}
			if (old == nil) == (ch.Value == nil) && (old == nil || *old == *ch.Value) {
				continue
			}
			diffs = append(diffs, diff{ch, old, ch.Value})
		}
		if len(diffs) == 0 {
			return nil // nothing differs: the servers are only brought up to date
		}
		var rev int64
		if err := tx.QueryRow(ctx, `SELECT nextval('kingres_cp.config_revision')`).Scan(&rev); err != nil {
			return err
		}
		var reverts *int64
		if in.Reverts != 0 {
			reverts = &in.Reverts
		}
		for _, d := range diffs {
			if d.new == nil {
				_, err = tx.Exec(ctx, `DELETE FROM kingres_cp.config_value WHERE level = $1 AND scope_id = $2 AND name = $3`, d.c.Level, d.c.Scope, d.c.Name)
			} else {
				_, err = tx.Exec(ctx, `INSERT INTO kingres_cp.config_value (level, scope_id, name, value, revision, updated_by) VALUES ($1, $2, $3, $4, $5, $6)
					ON CONFLICT (level, scope_id, name) DO UPDATE SET value = EXCLUDED.value, revision = EXCLUDED.revision, updated_by = EXCLUDED.updated_by, updated_at = now()`,
					d.c.Level, d.c.Scope, d.c.Name, *d.new, rev, in.Actor)
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.config_change (revision, task_id, level, scope_id, name, old_value, new_value, changed_by, reverts)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, rev, env.Task.ID, d.c.Level, d.c.Scope, d.c.Name, d.old, d.new, in.Actor, reverts); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	})
}

// buildRequest renders the complete configuration of one server from the stored values.
func (l *Leader) buildRequest(ctx context.Context, server string) (ApplyRequest, error) {
	vals, err := l.st.values(ctx)
	if err != nil {
		return ApplyRequest{}, err
	}
	servers, err := l.st.servers(ctx)
	if err != nil {
		return ApplyRequest{}, err
	}
	rev, err := l.st.revision(ctx)
	if err != nil {
		return ApplyRequest{}, err
	}
	removed, err := l.st.removedDatabaseNames(ctx)
	if err != nil {
		return ApplyRequest{}, err
	}
	group := ""
	for _, s := range servers {
		if s.ID == server {
			group = s.Group
		}
	}
	req := ApplyRequest{Revision: rev, Values: map[string]string{}, Databases: map[string]map[string]string{}, Removed: removed,
		Replicas: l.st.setting(ctx, "replica.default", 1)}
	byName := map[string][]Override{}
	for _, v := range vals {
		if Dropped(v.Name) {
			// A stored value of a name that has become managed or refused is not sent: the operator removes it
			// (the preview of its removal is accepted). It must not keep any server from being configured.
			l.Log.Warn("a stored configuration value is no longer settable and is not applied", "parameter", v.Name, "level", v.Level, "scope", v.Scope)
			continue
		}
		if v.Level == LevelDatabase {
			if req.Databases[v.Scope] == nil {
				req.Databases[v.Scope] = map[string]string{}
			}
			req.Databases[v.Scope][v.Name] = v.Value
			continue
		}
		byName[v.Name] = append(byName[v.Name], Override{Level: v.Level, Scope: v.Scope, Value: v.Value})
	}
	for name, os := range byName {
		if o, ok := Pick(os, server, group); ok {
			req.Values[name] = o.Value
		}
	}
	return req, nil
}

// stepApply brings one server to the stored configuration. It tries once: a server that cannot be reached is
// recorded as waiting and the task goes on to the next server, so that a stopped server in the middle neither
// keeps the later servers from the change nor keeps every later change out (P13R-03). The leader's convergence
// loop brings the waiting server up to date when it answers again.
func (l *Leader) stepApply(ctx context.Context, env *task.Env, server string) error {
	req, err := l.buildRequest(ctx, server)
	if err != nil {
		return err
	}
	var res ApplyResponse
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = l.callServer(cctx, server, CallApply, req, &res)
	cancel()
	if err == nil {
		st := serverState{Server: server, Revision: res.Revision, State: res.State, Detail: res.Detail, Errors: map[string]string{}}
		for _, p := range res.Parameters {
			if p.State == StateFailed {
				st.Errors[p.Name] = p.Error
			}
		}
		if res.State == StateFailed {
			// The server kept its previous file: the revision it holds is not the new one.
			if old, ok := l.currentRevision(ctx, server); ok {
				st.Revision = old
			}
		}
		return l.st.saveState(ctx, st)
	}
	if !errors.Is(err, peerclient.ErrUnreachable) {
		// The server answered with an error: it is failed, the others go on.
		l.Log.Warn("a server could not apply the configuration", "server", server, "error", err)
		return l.st.saveState(ctx, serverState{Server: server, State: StateFailed, Errors: map[string]string{"(server)": firstLine(err.Error())}})
	}
	l.Log.Warn("a server cannot be reached; it gets the configuration when it answers again", "server", server, "error", err)
	_ = env.SetDetail(ctx, fmt.Sprintf("Server %s cannot be reached; it gets the change when it returns.", server))
	return l.st.saveState(ctx, serverState{Server: server, Revision: 0, State: StateWaiting, Detail: "the server cannot be reached"})
}

func (l *Leader) currentRevision(ctx context.Context, server string) (int64, bool) {
	var st StateResponse
	c, cancel := context.WithTimeout(ctx, l.GatherTimeout)
	defer cancel()
	if err := l.callServer(c, server, CallState, StateRequest{}, &st); err != nil {
		return 0, false
	}
	return st.Revision, true
}

func (l *Leader) stepConfirm(ctx context.Context, in applyInput) error {
	states, err := l.st.states(ctx)
	if err != nil {
		return err
	}
	var ok, pend, failed []string
	for _, s := range in.Servers {
		st := states[s]
		switch st.State {
		case StateFailed:
			var parts []string
			for _, n := range sortedKeys(st.Errors) {
				parts = append(parts, n+": "+st.Errors[n])
			}
			failed = append(failed, s+" ("+strings.Join(parts, "; ")+")")
		case StatePendingRestart:
			pend = append(pend, s)
		default:
			ok = append(ok, s)
		}
	}
	if len(failed) > 0 {
		return &taskError{code: "config_incomplete", retry: true, msg: fmt.Sprintf("applied on %s; waiting for a restart on %s; failed on %s", list(ok), list(pend), strings.Join(failed, ", "))}
	}
	return nil
}

func list(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// ApplyInfo describes an accepted change for the audit log.
type ApplyInfo struct {
	// Changes are the checked changes: name, level, scope, the stored value before and the normalized value after.
	Changes []map[string]any
	// Servers are the servers the task brings to the change.
	Servers []string
	// BasedOn is the configuration revision the request was checked against; the revision the change gets is recorded
	// with the task (the history row of its task id).
	BasedOn int64
}

// describe lists the checked changes with the value each replaces. The values are those PostgreSQL shows to every
// database user: a parameter that is hidden, managed or refused can never be in an accepted request (a removal of
// a stored value of such a name has no value to show).
func describe(stored []valueRow, cs []change) []map[string]any {
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		d := map[string]any{"name": c.Name, "level": c.Level, "scopeId": c.Scope, "oldValue": nil, "newValue": nil}
		for _, v := range stored {
			if v.Name == c.Name && v.Level == c.Level && v.Scope == c.Scope && !Dropped(c.Name) {
				d["oldValue"] = v.Value
			}
		}
		if c.Value != nil {
			d["newValue"] = *c.Value
		}
		out = append(out, d)
	}
	return out
}

// Attempted describes the changes of a request that was not accepted for the audit log: the names and levels only,
// never a value (the value of a refused parameter may be a command or a secret).
func Attempted(req api.ConfigChangeRequest) []map[string]any {
	out := make([]map[string]any, 0, len(req.Changes))
	for i, c := range req.Changes {
		if i >= MaxChanges {
			break
		}
		scope := ""
		if c.ScopeID != nil && c.Level != LevelCluster {
			scope = *c.ScopeID
		}
		name := strings.ToLower(c.Name)
		if len(name) > 63 {
			name = name[:63]
		}
		out = append(out, map[string]any{"name": name, "level": c.Level, "scopeId": scope, "remove": c.Value == nil})
	}
	return out
}

// taskError is a failure of a step that carries the code the task is recorded with (task.Coded): the design
// documents validation_failed and config_incomplete, which a client branches on (P13R-15).
type taskError struct {
	code  string
	retry bool
	msg   string
}

func (e *taskError) Error() string       { return e.code + ": " + e.msg }
func (e *taskError) TaskCode() string    { return e.code }
func (e *taskError) TaskRetryable() bool { return e.retry }
