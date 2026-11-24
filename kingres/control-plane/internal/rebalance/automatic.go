// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// Automatic rebalancing (design shard-phase10.md, section 8): a job on the manager leader that starts a run by
// itself when the placement has been out of balance long enough and nothing makes a move unsafe, pauses a run
// when a safety rule fails, and resumes it when the rules have held again. The decision is a pure function
// (Evaluate) of settings and facts read from the system database, so that a new leader does not restart the
// timers: the moment the imbalance was first seen, the last topology change and the end of the last
// automatic run are all rows of kingres_cp.

// Outcomes of an evaluation (RebalanceStatus.automatic.lastEvaluation.outcome).
const (
	EvalStarted     = "started"
	EvalNothingToDo = "nothingToDo"
	EvalBlocked     = "blocked"
	EvalSettling    = "settling"
)

// Rule names (design 8.3), as they appear in blockedBy.
const (
	RuleWindow      = "window"
	RuleFailover    = "failover"
	RuleDegraded    = "groupDegraded"
	RuleNoPrimary   = "groupUnavailable"
	RuleServers     = "servers"
	RuleFailedMove  = "moveFailed"
	RuleConsistency = "consistency"
	RuleOperator    = "operatorStopped"
	RuleRunning     = "running"
)

// Window is the time of day in which an automatic run may start (from and to as HH:MM; from later than to
// wraps over midnight).
type Window struct {
	From     string `json:"from"`
	To       string `json:"to"`
	TimeZone string `json:"timeZone"`
}

// Contains reports whether t is inside the window; a window that cannot be read never contains anything.
func (w *Window) Contains(t time.Time) bool {
	if w == nil {
		return true
	}
	loc := time.UTC
	if w.TimeZone != "" {
		l, err := time.LoadLocation(w.TimeZone)
		if err != nil {
			return false
		}
		loc = l
	}
	from, ok1 := minutesOf(w.From)
	to, ok2 := minutesOf(w.To)
	if !ok1 || !ok2 {
		return false
	}
	lt := t.In(loc)
	now := lt.Hour()*60 + lt.Minute()
	switch {
	case from == to:
		return true
	case from < to:
		return now >= from && now < to
	default:
		return now >= from || now < to
	}
}

func minutesOf(s string) (int, bool) {
	var h, m int
	if n, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || n != 2 || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// AutoSettings are the settings of the automatic mode (ClusterSettings.rebalance.automatic).
type AutoSettings struct {
	Enabled                    bool
	Strategy                   string
	Threshold                  float64
	CheckIntervalSeconds       int
	SettleSeconds              int
	SettleAfterTopologySeconds int
	MinIntervalSeconds         int
	FailoverSettleSeconds      int
	Window                     *Window
}

// DefaultAutoSettings are the defaults of design 8.2 and 8.3; the mode is off.
func DefaultAutoSettings() AutoSettings {
	return AutoSettings{Strategy: ByBucketCount, Threshold: 0.15, CheckIntervalSeconds: 60, SettleSeconds: 300,
		SettleAfterTopologySeconds: 600, MinIntervalSeconds: 3600, FailoverSettleSeconds: 600}
}

// ReadAutoSettings reads the settings from the system database.
func ReadAutoSettings(ctx context.Context, c *pgx.Conn) (AutoSettings, error) {
	out := DefaultAutoSettings()
	rows, err := c.Query(ctx, `SELECT key, value FROM kingres_cp.cluster_setting WHERE key LIKE 'rebalance.automatic.%'`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return out, err
		}
		out.apply(strings.TrimPrefix(k, "rebalance.automatic."), v)
	}
	return out, rows.Err()
}

func (a *AutoSettings) apply(key string, v []byte) {
	var n int
	var f float64
	var s string
	var b bool
	switch key {
	case "enabled":
		if json.Unmarshal(v, &b) == nil {
			a.Enabled = b
		}
	case "strategy":
		if json.Unmarshal(v, &s) == nil && (s == ByBucketCount || s == ByDiskSize) {
			a.Strategy = s
		}
	case "threshold":
		if json.Unmarshal(v, &f) == nil {
			a.Threshold = f
		}
	case "checkIntervalSeconds":
		if json.Unmarshal(v, &n) == nil && n > 0 {
			a.CheckIntervalSeconds = n
		}
	case "settleSeconds":
		if json.Unmarshal(v, &n) == nil && n >= 0 {
			a.SettleSeconds = n
		}
	case "settleAfterTopologySeconds":
		if json.Unmarshal(v, &n) == nil && n >= 0 {
			a.SettleAfterTopologySeconds = n
		}
	case "minIntervalSeconds":
		if json.Unmarshal(v, &n) == nil && n >= 0 {
			a.MinIntervalSeconds = n
		}
	case "failoverSettleSeconds":
		if json.Unmarshal(v, &n) == nil && n >= 0 {
			a.FailoverSettleSeconds = n
		}
	case "window":
		var w Window
		if string(v) == "null" {
			a.Window = nil
		} else if json.Unmarshal(v, &w) == nil {
			a.Window = &w
		}
	}
}

// AutoFacts are what Evaluate decides on.
type AutoFacts struct {
	Now time.Time
	// PlanNeeded is whether the plan with the automatic strategy and threshold has a move.
	PlanNeeded bool
	// HeldSince is when PlanNeeded first held without a break; nil when it was not recorded yet.
	HeldSince *time.Time
	// LastTopologyChange is the newest event of the servers (join, removal, flag, weight, failover).
	LastTopologyChange *time.Time
	// LastAutomaticEnd is when the last automatic run ended.
	LastAutomaticEnd *time.Time
	// WaiveMinInterval is true for a server that waits for a rebalance or a drain flag.
	WaiveMinInterval bool
	// Blocked are the safety rules that fail now (see Rule*).
	Blocked []string
}

// Evaluation is the verdict.
type Evaluation struct {
	Outcome   string
	BlockedBy []string
}

// Evaluate decides whether an automatic run starts now (design 8.2). The rules are tried in an order that
// makes the cheapest and most informative answer first: a closed window or a failing safety rule is "blocked",
// a balanced cluster is "nothingToDo", an imbalance that is too young or too soon after a change is "settling".
func Evaluate(s AutoSettings, f AutoFacts) Evaluation {
	if !s.Enabled {
		return Evaluation{Outcome: EvalNothingToDo}
	}
	if !s.Window.Contains(f.Now) {
		return Evaluation{Outcome: EvalBlocked, BlockedBy: []string{RuleWindow}}
	}
	if !f.PlanNeeded {
		return Evaluation{Outcome: EvalNothingToDo}
	}
	if len(f.Blocked) > 0 {
		by := append([]string(nil), f.Blocked...)
		sort.Strings(by)
		return Evaluation{Outcome: EvalBlocked, BlockedBy: by}
	}
	settle := time.Duration(s.SettleSeconds) * time.Second
	if f.HeldSince == nil || f.Now.Sub(*f.HeldSince) < settle {
		return Evaluation{Outcome: EvalSettling}
	}
	if f.LastTopologyChange != nil && f.Now.Sub(*f.LastTopologyChange) < time.Duration(s.SettleAfterTopologySeconds)*time.Second {
		return Evaluation{Outcome: EvalSettling}
	}
	if f.LastAutomaticEnd != nil && !f.WaiveMinInterval && f.Now.Sub(*f.LastAutomaticEnd) < time.Duration(s.MinIntervalSeconds)*time.Second {
		return Evaluation{Outcome: EvalSettling}
	}
	return Evaluation{Outcome: EvalStarted}
}

// ClusterFacts are the conditions of design 8.3 as the job reads them.
type ClusterFacts struct {
	Now time.Time
	// FailoverUnfinished is true while a failover or switchover task is unfinished.
	FailoverUnfinished bool
	// LastPrimaryChange is the newest change of a primary, nil when none is known.
	LastPrimaryChange *time.Time
	// ReplicaCount and the groups with their active standbys and primaries.
	ReplicaCount      int
	GroupsNoPrimary   []int32
	GroupsFewStandbys []int32
	// GroupsLagging are the groups with a standby that is behind its primary by more than maxStandbyLagBytes (design 8.3: a group without
	// a healthy standby), or whose lag cannot be read.
	GroupsLagging []int32
	// ServersNotLive are servers that are dead or suspect.
	ServersNotLive []string
	// ServerTaskUnfinished is true while a server.add, server.remove, replica-count or restore task is unfinished.
	ServerTaskUnfinished bool
	// FailedMoveUnacknowledged is true while a failed move of the last automatic run has not been acknowledged.
	FailedMoveUnacknowledged bool
	// CheckError is the refusal of the cluster check (consistency or prerequisites), "" when it passes.
	CheckError string
}

// SafetyBlockers are the rules of the table in design 8.3 that fail for the facts, by name.
func SafetyBlockers(s AutoSettings, f ClusterFacts) []string {
	var out []string
	if f.FailoverUnfinished || f.LastPrimaryChange != nil && f.Now.Sub(*f.LastPrimaryChange) < time.Duration(s.FailoverSettleSeconds)*time.Second {
		out = append(out, RuleFailover)
	}
	if len(f.GroupsNoPrimary) > 0 {
		out = append(out, RuleNoPrimary)
	}
	if f.ReplicaCount > 0 && (len(f.GroupsFewStandbys) > 0 || len(f.GroupsLagging) > 0) {
		out = append(out, RuleDegraded)
	}
	if len(f.ServersNotLive) > 0 || f.ServerTaskUnfinished {
		out = append(out, RuleServers)
	}
	if f.FailedMoveUnacknowledged {
		out = append(out, RuleFailedMove)
	}
	if f.CheckError != "" {
		out = append(out, RuleConsistency)
	}
	return out
}

// ---- the job

const autoStateKey = "autostate.evaluation"

// autoState is what the job keeps between rounds, in kingres_cp.cluster_setting (so that a new leader goes on).
type autoState struct {
	HeldSince    *time.Time `json:"heldSince,omitempty"`
	RulesOKSince *time.Time `json:"rulesOkSince,omitempty"`
	At           time.Time  `json:"at"`
	Outcome      string     `json:"outcome"`
	BlockedBy    []string   `json:"blockedBy"`
}

func (s *Service) readAutoState(ctx context.Context, c *pgx.Conn) autoState {
	var st autoState
	var raw []byte
	if err := c.QueryRow(ctx, `SELECT value FROM kingres_cp.cluster_setting WHERE key = $1`, autoStateKey).Scan(&raw); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

func (s *Service) writeAutoState(ctx context.Context, c *pgx.Conn, st autoState) error {
	b, _ := json.Marshal(st)
	_, err := c.Exec(ctx, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, autoStateKey, b)
	return err
}

// AutoStep is one round of the job: it does nothing on a server that is not the manager leader, and nothing
// while the mode is off. It returns the evaluation it made (for tests and the status).
func (s *Service) AutoStep(ctx context.Context) (Evaluation, error) {
	var ev Evaluation
	if s.IsLeader != nil && !s.IsLeader(ctx) {
		return ev, nil
	}
	var set AutoSettings
	var st autoState
	var now time.Time
	err := s.sys(ctx, func(c *pgx.Conn) (err error) {
		if set, err = ReadAutoSettings(ctx, c); err != nil {
			return err
		}
		st = s.readAutoState(ctx, c)
		return c.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	})
	if err != nil || !set.Enabled {
		return ev, err
	}
	o := Resolve(Options{Strategy: set.Strategy, Threshold: &set.Threshold, Trigger: TriggerAutomatic}, DefaultSettings())
	o.Strategy, o.Threshold = set.Strategy, &set.Threshold
	var cur *active
	if cur, err = s.active(ctx); err != nil {
		return ev, err
	}
	dbs, err := s.Backend.Databases(ctx, Options{})
	if err != nil {
		return ev, err
	}
	facts, err := s.clusterFacts(ctx, dbs, set, now)
	if err != nil {
		return ev, err
	}
	blocked := SafetyBlockers(set, facts)
	s.failedAlert(ctx, facts.FailedMoveUnacknowledged)
	next := autoState{At: now}
	switch {
	case cur != nil && cur.State != "paused":
		next.Outcome, next.BlockedBy = EvalNothingToDo, []string{RuleRunning}
		next.HeldSince, next.RulesOKSince = st.HeldSince, nil
		ev = Evaluation{Outcome: EvalNothingToDo, BlockedBy: next.BlockedBy}
	case cur != nil:
		ev, err = s.autoResume(ctx, cur, set, st, blocked, now, &next)
	default:
		ev, err = s.autoStart(ctx, o, set, st, facts, blocked, now, &next)
	}
	if err != nil {
		return ev, err
	}
	next.Outcome, next.BlockedBy = ev.Outcome, ev.BlockedBy
	return ev, s.sys(ctx, func(c *pgx.Conn) error { return s.writeAutoState(ctx, c, next) })
}

// failedAlert raises the alert rebalanceFailed while the mode is suspended by a failed run that nobody has
// acknowledged, and clears it when a person has started a run (design 8.3).
func (s *Service) failedAlert(ctx context.Context, suspended bool) {
	_ = s.sys(ctx, func(c *pgx.Conn) error {
		if suspended {
			_, err := c.Exec(ctx, `INSERT INTO kingres_cp.alert (severity, kind, subject, message, raised_at)
				VALUES ('warning', 'rebalanceFailed', 'cluster', 'The last automatic rebalance failed; automatic rebalancing is suspended until an operator starts a rebalance or looks at the report.', now())
				ON CONFLICT (kind, subject) WHERE cleared_at IS NULL DO NOTHING`)
			return err
		}
		_, err := c.Exec(ctx, `UPDATE kingres_cp.alert SET cleared_at = now() WHERE kind = 'rebalanceFailed' AND cleared_at IS NULL`)
		return err
	})
}

// autoResume goes on with a stopped automatic run once the rules have held for settleSeconds; a run that an
// operator stopped, or a manual run, is left alone.
func (s *Service) autoResume(ctx context.Context, cur *active, set AutoSettings, st autoState, blocked []string, now time.Time, next *autoState) (Evaluation, error) {
	run, err := (&PGRows{Conns: s.Conns, TaskID: cur.ID}).Run(ctx)
	if err != nil {
		return Evaluation{}, err
	}
	if run.Options.Trigger != TriggerAutomatic || run.StoppedBy != "safetyRule" {
		return Evaluation{Outcome: EvalBlocked, BlockedBy: []string{RuleOperator}}, nil
	}
	if len(blocked) > 0 {
		next.RulesOKSince = nil
		return Evaluation{Outcome: EvalBlocked, BlockedBy: blocked}, nil
	}
	since := now
	if st.RulesOKSince != nil {
		since = *st.RulesOKSince
	}
	next.RulesOKSince = &since
	if now.Sub(since) < time.Duration(set.SettleSeconds)*time.Second {
		return Evaluation{Outcome: EvalSettling}, nil
	}
	s.lag.reset()
	if _, err := s.start(ctx, "system", run.Options, false); err != nil {
		return Evaluation{Outcome: EvalBlocked, BlockedBy: []string{RuleFailover}}, nil
	}
	next.RulesOKSince = nil
	return Evaluation{Outcome: EvalStarted}, nil
}

func (s *Service) autoStart(ctx context.Context, o Options, set AutoSettings, st autoState, facts ClusterFacts, blocked []string, now time.Time, next *autoState) (Evaluation, error) {
	plan, err := s.Backend.Plan(ctx, mustDatabases(ctx, s), o)
	if err != nil {
		var p *problem.Problem
		if errors.As(err, &p) {
			return Evaluation{Outcome: EvalBlocked, BlockedBy: []string{RuleConsistency}}, nil
		}
		return Evaluation{}, err
	}
	needed := len(plan) > 0
	// the moment the imbalance was first seen: kept while it holds, forgotten when it does not
	if needed {
		since := now
		if st.HeldSince != nil {
			since = *st.HeldSince
		}
		next.HeldSince = &since
	}
	af := AutoFacts{Now: now, PlanNeeded: needed, HeldSince: next.HeldSince, Blocked: blocked}
	if err := s.sys(ctx, func(c *pgx.Conn) error {
		af.LastTopologyChange, af.LastAutomaticEnd = readTopologyAndLastRun(ctx, c)
		af.WaiveMinInterval = s.waivesMinInterval(ctx)
		return nil
	}); err != nil {
		return Evaluation{}, err
	}
	ev := Evaluate(set, af)
	if ev.Outcome == EvalStarted {
		s.lag.reset()
		if _, err := s.start(ctx, "system", o, false); err != nil {
			var p *problem.Problem
			if errors.As(err, &p) {
				return Evaluation{Outcome: EvalBlocked, BlockedBy: []string{string(p.Code)}}, nil
			}
			return Evaluation{}, err
		}
		next.HeldSince = nil
	}
	return ev, nil
}

func mustDatabases(ctx context.Context, s *Service) []string {
	d, _ := s.Backend.Databases(ctx, Options{})
	return d
}

// readTopologyAndLastRun reads the newest change of the servers and the end of the last automatic run.
func readTopologyAndLastRun(ctx context.Context, c *pgx.Conn) (topology, lastAuto *time.Time) {
	_ = c.QueryRow(ctx, `SELECT greatest(
			(SELECT max(joined_at) FROM kingres_cp.server),
			(SELECT max(updated_at) FROM kingres_cp.task WHERE kind IN ('server.add', 'server.remove', 'server.leave', 'server.failover')),
			(SELECT (value #>> '{}')::timestamptz FROM kingres_cp.cluster_setting WHERE key = 'topology.changedAt'))`).Scan(&topology)
	_ = c.QueryRow(ctx, `SELECT max(finished_at) FROM kingres_cp.rebalance_run WHERE options->>'trigger' = 'automatic'`).Scan(&lastAuto)
	return topology, lastAuto
}

// waivesMinInterval is true while a server waits for its first rebalance or has the drain flag.
func (s *Service) waivesMinInterval(ctx context.Context) bool {
	nodes, err := s.readNodes(ctx)
	if err != nil {
		return false
	}
	for _, n := range nodes {
		if n.Role == "primary" && !n.Should {
			return true
		}
		if s.ServerStatus != nil && n.Role == "primary" && s.ServerStatus(ctx, n.Host) == "pendingRebalance" {
			return true
		}
	}
	return false
}

// clusterFacts reads the conditions of the safety rules.
func (s *Service) clusterFacts(ctx context.Context, dbs []string, set AutoSettings, now time.Time) (ClusterFacts, error) {
	return s.clusterFactsLag(ctx, dbs, set, now, 1)
}

// clusterFactsLag is clusterFacts with the limit of the standby lag multiplied by lagFactor: 1 where a run is started or resumed, lagRunFactor
// inside a run (whose own writes make the standbys lag for a while).
func (s *Service) clusterFactsLag(ctx context.Context, dbs []string, set AutoSettings, now time.Time, lagFactor int64) (ClusterFacts, error) {
	f := ClusterFacts{Now: now}
	nodes, err := s.readNodes(ctx)
	if err != nil {
		return f, err
	}
	standbys := map[int32]int{}
	primaries := map[int32]bool{}
	hosts := map[string]bool{}
	for _, n := range nodes {
		hosts[n.Host] = true
		if n.Active && n.Role == "standby" {
			standbys[n.Group]++
		}
		if n.Active && n.Role == "primary" {
			primaries[n.Group] = true
		}
	}
	for _, c := range standbys {
		f.ReplicaCount = max(f.ReplicaCount, c)
	}
	if f.ReplicaCount > 0 {
		f.GroupsLagging = s.laggingGroups(ctx, primaries, lagFactor)
	}
	for _, n := range nodes {
		if n.Role == "primary" && !primaries[n.Group] {
			f.GroupsNoPrimary = append(f.GroupsNoPrimary, n.Group)
		}
		if n.Role == "primary" && f.ReplicaCount > 0 && standbys[n.Group] < f.ReplicaCount {
			f.GroupsFewStandbys = append(f.GroupsFewStandbys, n.Group)
		}
	}
	if s.ServerStatus != nil {
		for h := range hosts {
			if st := s.ServerStatus(ctx, h); st == "dead" || st == "suspect" {
				f.ServersNotLive = append(f.ServersNotLive, h)
			}
		}
		sort.Strings(f.ServersNotLive)
	}
	err = s.sys(ctx, func(c *pgx.Conn) error {
		var n int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.task WHERE kind IN ('server.failover') AND state IN ('pending', 'running', 'paused')`).Scan(&n); err != nil {
			return err
		}
		f.FailoverUnfinished = n > 0
		if err := c.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.task WHERE kind IN ('server.add', 'server.remove', 'server.leave', 'standby.reconcile', 'replication.restorePreferredPrimaries')
			AND state IN ('pending', 'running', 'paused')`).Scan(&n); err != nil {
			return err
		}
		f.ServerTaskUnfinished = n > 0
		var t *time.Time
		if err := c.QueryRow(ctx, `SELECT max(updated_at) FROM kingres_cp.task WHERE kind = 'server.failover'`).Scan(&t); err == nil {
			f.LastPrimaryChange = t
		}
		var failed bool
		if err := c.QueryRow(ctx, `SELECT coalesce((SELECT r.outcome = 'failed' AND r.finished_at > coalesce(
				(SELECT (value #>> '{}')::timestamptz FROM kingres_cp.cluster_setting WHERE key = 'autostate.acknowledgedAt'), '-infinity')
			FROM kingres_cp.rebalance_run r WHERE r.options->>'trigger' = 'automatic' ORDER BY r.task_id DESC LIMIT 1), false)`).Scan(&failed); err != nil {
			return err
		}
		f.FailedMoveUnacknowledged = failed
		return nil
	})
	if err != nil {
		return f, err
	}
	if len(dbs) > 0 {
		if err := s.Backend.Check(ctx, dbs); err != nil {
			var p *problem.Problem
			if errors.As(err, &p) {
				f.CheckError = p.Detail
			} else {
				f.CheckError = err.Error()
			}
		}
	}
	return f, nil
}

// autoGuard stops an automatic run when a safety rule of design 8.3 fails (the run pauses with stoppedBy
// safetyRule and the job resumes it when the rules have held for settleSeconds).
func (s *Service) autoGuard(ctx context.Context, _ Options) error {
	var set AutoSettings
	var now time.Time
	if err := s.sys(ctx, func(c *pgx.Conn) (err error) {
		if set, err = ReadAutoSettings(ctx, c); err != nil {
			return err
		}
		return c.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	}); err != nil {
		return err
	}
	dbs, err := s.Backend.Databases(ctx, Options{})
	if err != nil {
		return err
	}
	f, err := s.clusterFactsLag(ctx, dbs, set, now, lagRunFactor)
	if err != nil {
		return err
	}
	// The run writes to the groups, and its standbys lag for a while because of it: inside a run the lag has to exceed twice the limit on
	// lagRunStreak evaluations in a row (one before each move) to count.
	if !s.lag.observe(len(f.GroupsLagging) > 0) {
		f.GroupsLagging = nil
	}
	// a failed move of this run is the loop's own business (it retries and gives up); only the others pause the run
	f.FailedMoveUnacknowledged = false
	if by := SafetyBlockers(set, f); len(by) > 0 {
		return problem.New(problem.Conflict, "An automatic rebalance waits: "+strings.Join(by, ", ")+" ("+ruleText(by[0])+").")
	}
	return nil
}

func ruleText(rule string) string {
	switch rule {
	case RuleFailover:
		return "a failover is running or has just ended"
	case RuleDegraded:
		return "a shard group has fewer healthy standbys than the replica count, or a standby lags too far behind"
	case RuleNoPrimary:
		return "a shard group has no primary"
	case RuleServers:
		return "a server is not live or a change of the servers is unfinished"
	case RuleConsistency:
		return "the cluster check reports a problem"
	}
	return "a safety rule does not hold"
}

// RunAuto is the job: AutoStep every checkIntervalSeconds until ctx ends.
func (s *Service) RunAuto(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := s.AutoStep(ctx); err != nil && ctx.Err() == nil {
			s.log().Warn("automatic rebalance: the cluster cannot be evaluated", "error", err)
		}
		d := 60 * time.Second
		_ = s.sys(ctx, func(c *pgx.Conn) error {
			if set, err := ReadAutoSettings(ctx, c); err == nil {
				d = time.Duration(set.CheckIntervalSeconds) * time.Second
			}
			return nil
		})
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
}

// AutoStatus is RebalanceStatus.automatic.
func (s *Service) AutoStatus(ctx context.Context) *api.RebalanceAutomatic {
	var out *api.RebalanceAutomatic
	_ = s.sys(ctx, func(c *pgx.Conn) error {
		set, err := ReadAutoSettings(ctx, c)
		if err != nil {
			return err
		}
		out = &api.RebalanceAutomatic{Enabled: set.Enabled}
		st := s.readAutoState(ctx, c)
		if set.Enabled && !st.At.IsZero() {
			next := st.At.Add(time.Duration(set.CheckIntervalSeconds) * time.Second)
			out.NextCheckAt = &next
			by := st.BlockedBy
			if by == nil {
				by = []string{}
			}
			out.LastEvaluation = &api.AutoEvaluation{At: st.At, Outcome: st.Outcome, BlockedBy: by}
		}
		return nil
	})
	return out
}

// LaggingGroups are the groups whose lag is above max bytes (design 8.3, maxStandbyLagBytes), ascending. A group whose lag could not be
// read has a negative value in lags and counts as lagging: what cannot be shown to be in step is not trusted. A lag of exactly max is
// allowed.
func LaggingGroups(lags map[int32]int64, max int64) []int32 {
	var out []int32
	for g, l := range lags {
		if l < 0 || l > max {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// standbyLag is the lag of the standbys of the group behind its primary, in bytes, by the hook or the primary of the group.
func (s *Service) standbyLag(ctx context.Context, group int32) (int64, error) {
	if s.StandbyLag != nil {
		return s.StandbyLag(ctx, group)
	}
	if s.Backend == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return s.Backend.StandbyLag(ctx, group)
}

// laggingGroups reads the lag of every group with an active primary and returns the groups that exceed the limit of the cluster
// settings. Nothing is asked when no standby is wanted.
func (s *Service) laggingGroups(ctx context.Context, primaries map[int32]bool, factor int64) []int32 {
	limit := DefaultMaxStandbyLagBytes
	_ = s.sys(ctx, func(c *pgx.Conn) error {
		if set, err := ReadSettings(ctx, c); err == nil {
			limit = set.MaxStandbyLagBytes
		}
		return nil
	})
	if factor > 1 && limit > math.MaxInt64/factor {
		limit = math.MaxInt64
	} else if factor > 1 {
		limit *= factor
	}
	// the groups are asked at the same time: the time is the slowest primary (bounded by standbyLag), not the sum
	lags := map[int32]int64{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for g := range primaries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := s.standbyLag(ctx, g)
			if err != nil {
				s.log().Warn("automatic rebalance: the lag of the standbys of a shard group cannot be read", "group", g, "error", err)
				l = -1
			}
			mu.Lock()
			lags[g] = l
			mu.Unlock()
		}()
	}
	wg.Wait()
	return LaggingGroups(lags, limit)
}

// Inside a run the lag is judged less strictly than at its start (design 8.3): the run's own writes make the standbys lag for a while.
const (
	// lagRunFactor multiplies maxStandbyLagBytes for the evaluations inside a run.
	lagRunFactor = 2
	// lagRunStreak is how many evaluations in a row must find a group over that limit before the guard pauses the run.
	lagRunStreak = 3
)

// lagStreak counts the evaluations of the guard in a row that found a group lagging.
type lagStreak struct {
	mu sync.Mutex
	n  int
}

// observe records one evaluation and says whether the run is to pause now: after lagRunStreak lagging evaluations in a row (the count then
// starts again); a clean evaluation ends the row.
func (l *lagStreak) observe(lagging bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !lagging {
		l.n = 0
		return false
	}
	l.n++
	if l.n >= lagRunStreak {
		l.n = 0
		return true
	}
	return false
}

func (l *lagStreak) reset() { l.mu.Lock(); l.n = 0; l.mu.Unlock() }

// manualLagGate is the rule "group without a healthy standby" for a manual start (design 8.3, marked (M)): a start, or the resumption of a
// stopped run, is refused with group_degraded while a group has a standby that is behind its primary by more than maxStandbyLagBytes
// (the limit of a start, as for the automatic mode), unless the request says force. A drain has its own refusals and the automatic mode its
// own rule; nothing is read when no standby is wanted.
func (s *Service) manualLagGate(ctx context.Context, o Options, drain bool) error {
	if o.Force || drain || o.Trigger != TriggerManual {
		return nil
	}
	nodes, err := s.readNodes(ctx)
	if err != nil {
		return asProblem(err)
	}
	standbys := map[int32]int{}
	primaries := map[int32]bool{}
	replicas := 0
	for _, n := range nodes {
		if n.Active && n.Role == "standby" {
			standbys[n.Group]++
		}
		if n.Active && n.Role == "primary" {
			primaries[n.Group] = true
		}
	}
	for _, c := range standbys {
		replicas = max(replicas, c)
	}
	if replicas == 0 {
		return nil
	}
	if g := s.laggingGroups(ctx, primaries, 1); len(g) > 0 {
		return problem.New(problem.GroupDegraded, fmt.Sprintf(
			"Shard group %v has a standby that is behind its primary by more than maxStandbyLagBytes (or whose lag cannot be read); moves would copy through a group that is not protected. Wait until it has caught up, or start again with force.", g))
	}
	return nil
}
