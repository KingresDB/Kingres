// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package partsched starts the runs of the time-partitioning policies (phase 12, analytics-phase12-storage.md section 10.6). It runs as two jobs of the
// scheduler of the manager leader (internal/scheduler): every Interval it asks which policies are due and starts a task partition.maintain for each
// one that has no unfinished task; every AlertEvery it reads the checks of kingres_timepart and raises or clears the alerts partitionsAheadLow and
// partitionPolicyFailed.
//
// The scheduler holds no durable state. What is due is in the policy (next_run_at), what is running is in the task table, and after a change of the
// leader the new leader asks again. The clock is injected so that tests control time.
package partsched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// Names of the jobs.
const (
	JobSchedule = "partition.schedule"
	JobAlerts   = "partition.alerts"
)

// The alert kinds of this package (contract 0.17.0).
const (
	AlertAheadLow     = "partitionsAheadLow"
	AlertPolicyFailed = "partitionPolicyFailed"
)

// ClockError is returned by Backend.Start for a policy whose scheduled time is not credible: the clock of the leader differs from the one of the instance, or
// it jumped far ahead of the last run of the policy. The scheduler does not count it as a failure at every tick: it says so once and raises one alert for the policy.
type ClockError struct{ Msg string }

func (e *ClockError) Error() string { return e.Msg }

// BlockedError is returned by Backend.Start for a policy that cannot run until an administrator changes something (the owner of the table is not trusted). Like
// ClockError it is one alert, not a failure at every tick.
type BlockedError struct{ Msg string }

func (e *BlockedError) Error() string { return e.Msg }

// Due is a policy that is due.
type Due struct{ Database, Schema, Table string }

// Subject is the id of the task of a due policy, database/schema.table.
func (d Due) Subject() string { return d.Database + "/" + d.Schema + "." + d.Table }

// Check is one finding of the check of the policies: the problem is the name of kingres_timepart (ahead_low, last_run_failed, ...).
type Check struct {
	Database, Schema, Table string
	Problem, Detail         string
}

// Backend is what the scheduler needs of the cluster.
type Backend interface {
	// Due returns the policies that are due at asOf, in every database that has kingres_timepart.
	Due(ctx context.Context, asOf time.Time) ([]Due, error)
	// Start starts a run for a due policy. It reports false, without an error, when a task of the table is unfinished.
	Start(ctx context.Context, d Due, asOf time.Time) (bool, error)
	// Checks returns the findings of kingres.timepart_check in every database that has kingres_timepart, and the databases that exist but could not be read this time: such a
	// database says nothing, so the alerts of its tables are neither raised nor cleared. An alert of a database that is gone, or has no kingres_timepart any more, is cleared.
	Checks(ctx context.Context) (checks []Check, unread []string, err error)
}

// OpenAlert is an alert that is open.
type OpenAlert struct{ Kind, Subject string }

// Alerts raises and clears alerts.
type Alerts interface {
	Open(ctx context.Context, kinds []string) ([]OpenAlert, error)
	Raise(ctx context.Context, kind, severity, subject, message string) error
	Clear(ctx context.Context, kind, subject string) error
}

// Config configures the scheduler.
type Config struct {
	Backend Backend
	Alerts  Alerts
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Interval is partition_scheduler_interval (default 60 s); AlertEvery how often the alerts are evaluated (default 5 min).
	Interval, AlertEvery time.Duration
	Log                  *slog.Logger
}

// Scheduler starts the runs.
type Scheduler struct {
	cfg Config
	// ticks, started and failures count the passes, the tasks started and the policies that could not be started (tests, metrics).
	ticks, started, failures atomic.Int64
	// issues are the policies of the last tick that Start refused with a ClockError or a BlockedError, by policy; mu guards it.
	mu     sync.Mutex
	issues map[Due]string
}

// New builds a scheduler.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Backend == nil {
		return nil, errors.New("partsched: a backend is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.AlertEvery <= 0 {
		cfg.AlertEvery = 5 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Scheduler{cfg: cfg}, nil
}

// Jobs returns the jobs for the scheduler of the leader.
func (s *Scheduler) Jobs() []scheduler.Job {
	jobs := []scheduler.Job{{Name: JobSchedule, Every: s.cfg.Interval, Jitter: 0.1, Run: func(ctx context.Context, _ scheduler.Term) error {
		_, err := s.Tick(ctx)
		return err
	}}}
	if s.cfg.Alerts != nil {
		jobs = append(jobs, scheduler.Job{Name: JobAlerts, Every: s.cfg.AlertEvery, Jitter: 0.1, Run: func(ctx context.Context, _ scheduler.Term) error {
			return s.CheckAlerts(ctx)
		}})
	}
	return jobs
}

// Tick asks what is due and starts a run for each policy that has no unfinished task. It returns the policies whose task it started. A policy that
// cannot be started is logged and counted; the others still start, and the next tick asks again.
func (s *Scheduler) Tick(ctx context.Context) ([]Due, error) {
	s.ticks.Add(1)
	now := s.cfg.Now()
	due, err := s.cfg.Backend.Due(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("partsched: asking what is due: %w", err)
	}
	var started []Due
	issues := map[Due]string{}
	defer func() {
		s.mu.Lock()
		for d, msg := range issues {
			if s.issues[d] != msg {
				s.cfg.Log.Warn("a partition policy is not run until something changes", "table", d.Subject(), "reason", msg)
			}
		}
		s.issues = issues
		s.mu.Unlock()
	}()
	for _, d := range due {
		if ctx.Err() != nil {
			return started, ctx.Err()
		}
		ok, err := s.cfg.Backend.Start(ctx, d, now)
		var ce *ClockError
		var be *BlockedError
		if errors.As(err, &ce) || errors.As(err, &be) {
			issues[d] = err.Error()
			continue
		}
		if err != nil {
			s.failures.Add(1)
			s.cfg.Log.Warn("a partition policy could not be started", "table", d.Subject(), "error", err)
			continue
		}
		if ok {
			s.started.Add(1)
			started = append(started, d)
		}
	}
	return started, nil
}

// Counts returns the number of passes, tasks started and policies that failed to start.
func (s *Scheduler) Counts() (ticks, started, failures int64) {
	return s.ticks.Load(), s.started.Load(), s.failures.Load()
}

// alertKey names an alert of a table.
func alertKey(kind string, c Check) OpenAlert {
	return OpenAlert{Kind: kind, Subject: "database:" + api.SubjectEscape(c.Database) + "|tableschema:" + api.SubjectEscape(c.Schema) + "|tablename:" + api.SubjectEscape(c.Table)}
}

// CheckAlerts reads the findings and raises partitionsAheadLow for a policy whose last partition ends before the floor and partitionPolicyFailed for a
// policy whose last run failed (or was partial twice in a row: the backend only reports that); an open alert of these kinds without a finding is
// cleared.
func (s *Scheduler) CheckAlerts(ctx context.Context) error {
	if s.cfg.Alerts == nil {
		return nil
	}
	checks, unread, err := s.cfg.Backend.Checks(ctx)
	if err != nil {
		return fmt.Errorf("partsched: reading the checks: %w", err)
	}
	want := map[OpenAlert]string{}
	s.mu.Lock()
	for d, msg := range s.issues {
		want[alertKey(AlertPolicyFailed, Check{Database: d.Database, Schema: d.Schema, Table: d.Table})] = fmt.Sprintf("The partitioning policy of %s.%s in database %s is not run: %s", d.Schema, d.Table, d.Database, msg)
	}
	s.mu.Unlock()
	for _, c := range checks {
		switch c.Problem {
		case "ahead_low":
			want[alertKey(AlertAheadLow, c)] = fmt.Sprintf("The last partition of %s.%s in database %s ends before the lookahead floor: %s", c.Schema, c.Table, c.Database, c.Detail)
		case "last_run_failed":
			want[alertKey(AlertPolicyFailed, c)] = fmt.Sprintf("The last run of the partitioning policy of %s.%s in database %s did not succeed: %s", c.Schema, c.Table, c.Database, c.Detail)
		}
	}
	open, err := s.cfg.Alerts.Open(ctx, []string{AlertAheadLow, AlertPolicyFailed})
	if err != nil {
		return fmt.Errorf("partsched: reading the open alerts: %w", err)
	}
	keys := make([]OpenAlert, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Kind+keys[i].Subject < keys[j].Kind+keys[j].Subject })
	var first error
	for _, k := range keys {
		if err := s.cfg.Alerts.Raise(ctx, k.Kind, "warning", k.Subject, want[k]); err != nil && first == nil {
			first = err
		}
	}
	unreadDB := map[string]bool{}
	for _, d := range unread {
		unreadDB[d] = true
	}
	for _, o := range open {
		if _, ok := want[o]; ok {
			continue
		}
		if unreadDB[subjectDatabase(o.Subject)] {
			continue // its database exists and could not be read this time: the alert stays as it is
		}
		if err := s.cfg.Alerts.Clear(ctx, o.Kind, o.Subject); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// subjectDatabase is the database named by the first segment of the stored subject of a partition alert.
func subjectDatabase(subject string) string {
	first, _, _ := strings.Cut(subject, "|")
	return api.SubjectUnescape(strings.TrimPrefix(first, "database:"))
}
