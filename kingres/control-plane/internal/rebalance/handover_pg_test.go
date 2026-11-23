// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func TestTheMarkerOfAHandoverIsKeptInTheSystemDatabase(t *testing.T) {
	e, _ := newGuardEnv(t)
	ctx := context.Background()
	st := sysHandover{&Service{Conns: e}}
	if m, err := st.Get(ctx); err != nil || m != nil {
		t.Fatalf("no marker yet: %v %v", m, err)
	}
	if err := st.Put(ctx, MapHandover{TaskID: 9, Server: "a", State: HandoverRefused, Code: "no_replica_available", Message: "none"}); err != nil {
		t.Fatal(err)
	}
	m, err := st.Get(ctx)
	if err != nil || m == nil || m.TaskID != 9 || m.Server != "a" || m.State != HandoverRefused || m.Code != "no_replica_available" || m.Message != "none" {
		t.Fatalf("marker %+v %v", m, err)
	}
	// the age is the clock of the database
	if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.cluster_setting SET updated_at = now() - interval '10 minutes' WHERE key = $1`, mapHandoverKey); err != nil {
		t.Fatal(err)
	}
	if m, _ := st.Get(ctx); m.Age < 9*time.Minute || m.Age > 11*time.Minute {
		t.Fatalf("age %v", m.Age)
	}
	if err := st.Put(ctx, MapHandover{TaskID: 10, State: HandoverStarted}); err != nil {
		t.Fatal(err)
	}
	if m, _ := st.Get(ctx); m.TaskID != 10 || m.Age > time.Minute {
		t.Fatalf("a second put replaces the first: %+v", m)
	}
	if err := st.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if m, err := st.Get(ctx); err != nil || m != nil {
		t.Fatalf("cleared: %v %v", m, err)
	}
}

// resumeEnv is a paused drain in the system database (the task of guardEnv, paused), its run row, and a service whose engine can run it.
func resumeEnv(t *testing.T, mark *MapHandover) (*Service, *guardEnv) {
	t.Helper()
	e, lease := newGuardEnv(t)
	ctx := context.Background()
	if _, err := e.sys.Exec(ctx, `INSERT INTO kingres_cp.rebalance_run (task_id, options, started_at, outcome, stopped_by) VALUES ($1, '{"trigger":"drain","drainServer":"a"}', clock_timestamp(), 'stopped', 'safetyRule')`, e.id); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Pause(ctx, e.id, lease, "handing over the map group"); err != nil {
		t.Fatal(err)
	}
	def := task.Definition{Kind: Kind, Steps: []task.Step{{Name: "x", Run: func(context.Context, *task.Env) error { return nil }}}}
	// the engine runs in the background on a session of its own: a session is not shared between goroutines
	eng, err := task.NewEngine(task.NewPGStore(e.in.Connect(t, sysdb.Database)), []task.Definition{def}, task.Options{Holder: "B", Lease: time.Minute, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Conns: e, Engine: func() *task.Engine { return eng }, Poll: time.Millisecond,
		HandOverMapGroup: func(context.Context, string, string) error { return nil }}
	s.BGFn = func() context.Context { return ctx }
	if mark != nil {
		mark.TaskID = e.id
		if err := (sysHandover{s}).Put(ctx, *mark); err != nil {
			t.Fatal(err)
		}
	}
	return s, e
}

func stateOf(t *testing.T, e *guardEnv) string {
	t.Helper()
	var st string
	if err := e.sys.QueryRow(context.Background(), `SELECT state FROM kingres_cp.task WHERE id = $1`, e.id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// The new leader resumes the paused drain once the marker says the switchover was asked for; the drain runs on.
func TestTheWatchResumesTheDrainWhenTheSwitchoverWasAskedFor(t *testing.T) {
	s, e := resumeEnv(t, &MapHandover{State: HandoverStarted, Server: "a"})
	ok, err := s.ResumeDrain(context.Background())
	if err != nil || !ok {
		t.Fatalf("resumed %v: %v", ok, err)
	}
	defer func() {
		if t.Failed() {
			var st, er string
			_ = e.sys.QueryRow(context.Background(), `SELECT state, coalesce(error::text, '') FROM kingres_cp.task WHERE id = $1`, e.id).Scan(&st, &er)
			t.Logf("task state %s error %s", st, er)
		}
	}()
	waitFor(t, "the drain to run on", func() bool { return stateOf(t, e) == "done" })
	var outcome *string
	_ = e.sys.QueryRow(context.Background(), `SELECT outcome FROM kingres_cp.rebalance_run WHERE task_id = $1`, e.id).Scan(&outcome)
	if outcome != nil {
		t.Errorf("the run is still marked %q", *outcome)
	}
}

func TestTheWatchResumesAfterARefusalToo(t *testing.T) {
	s, e := resumeEnv(t, &MapHandover{State: HandoverRefused, Code: "no_replica_available", Server: "a"})
	if ok, err := s.ResumeDrain(context.Background()); err != nil || !ok {
		t.Fatalf("resumed %v: %v", ok, err)
	}
	waitFor(t, "the drain to run on", func() bool { return stateOf(t, e) == "done" })
}

// The database refuses the resume while a failover is unfinished; the watch waits and the drain stays paused.
func TestTheWatchWaitsForTheFailoverToEnd(t *testing.T) {
	s, e := resumeEnv(t, &MapHandover{State: HandoverStarted, Server: "a"})
	ctx := context.Background()
	if _, err := e.sys.Exec(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ('server.failover', 'paused', 'server', 'a', 'admin')`); err != nil {
		t.Fatalf("the failover task: %v", err)
	}
	if ok, err := s.ResumeDrain(ctx); err != nil || ok {
		t.Fatalf("resumed %v while a failover is unfinished: %v", ok, err)
	}
	if st := stateOf(t, e); st != "paused" {
		t.Fatalf("the drain is %s", st)
	}
	if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.task SET state = 'done' WHERE kind = 'server.failover'`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ResumeDrain(ctx); err != nil || !ok {
		t.Fatalf("resumed %v after the failover: %v", ok, err)
	}
}

// What the watch leaves alone: a marker that was just written (the switchover is not asked for yet), a marker of another task, no
// marker, a server that is not the leader, a paused rebalance that is not a drain.
func TestTheWatchLeavesOtherPausedRebalancesAlone(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		mark   *MapHandover
		leader bool
		opts   string
	}{
		"requested a moment ago": {&MapHandover{State: HandoverRequested}, true, ""},
		"another task":           {&MapHandover{State: HandoverStarted}, true, "other"},
		"no marker":              {nil, true, ""},
		"not the leader":         {&MapHandover{State: HandoverStarted}, false, ""},
		"not a drain":            {&MapHandover{State: HandoverStarted}, true, "manual"},
	} {
		t.Run(name, func(t *testing.T) {
			s, e := resumeEnv(t, c.mark)
			if c.opts == "other" {
				if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.cluster_setting SET value = jsonb_set(value, '{taskId}', '999') WHERE key = $1`, mapHandoverKey); err != nil {
					t.Fatal(err)
				}
			}
			if c.opts == "manual" {
				if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET options = '{"trigger":"manual"}' WHERE task_id = $1`, e.id); err != nil {
					t.Fatal(err)
				}
			}
			if !c.leader {
				s.IsLeader = func(context.Context) bool { return false }
			}
			if ok, err := s.ResumeDrain(ctx); err != nil || ok {
				t.Fatalf("resumed %v: %v", ok, err)
			}
			if st := stateOf(t, e); st != "paused" {
				t.Fatalf("the task is %s", st)
			}
		})
	}
}

// A request that never reached the switchover (the leader ended after the record and before the call) is resumed after the grace time.
func TestTheWatchResumesAMarkerThatWasLeftBehind(t *testing.T) {
	s, e := resumeEnv(t, &MapHandover{State: HandoverRequested, Server: "a"})
	ctx := context.Background()
	if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.cluster_setting SET updated_at = now() - interval '6 minutes' WHERE key = $1`, mapHandoverKey); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ResumeDrain(ctx); err != nil || !ok {
		t.Fatalf("resumed %v: %v", ok, err)
	}
}

// M1. A marker that says requesting, with a failover that began after it and has ended (the record of started could not be made because the
// system database of the old leader stopped), is resumed, and the marker is turned into started so that the step fails if the server still
// runs the map group (the switchover was rolled back).
func TestTheWatchResumesARequestingMarkerWhoseFailoverHasEnded(t *testing.T) {
	s, e := resumeEnv(t, &MapHandover{State: HandoverRequesting, Server: "a", Attempt: "x"})
	ctx := context.Background()
	if ok, err := s.ResumeDrain(ctx); err != nil || ok {
		t.Fatalf("resumed %v with no failover at all: %v", ok, err)
	}
	if _, err := e.sys.Exec(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ('server.failover', 'done', 'server', 'a', 'admin')`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ResumeDrain(ctx); err != nil || !ok {
		t.Fatalf("resumed %v: %v", ok, err)
	}
	if m, _ := (sysHandover{s}).Get(ctx); m == nil || m.State != HandoverStarted || m.Attempt != "x" {
		t.Fatalf("the marker: %+v", m)
	}
}

// M1. A failover that began before the marker does not count.
func TestAFailoverThatEndedBeforeTheMarkerIsNotTheOnesOfTheRequest(t *testing.T) {
	s, e := resumeEnv(t, nil)
	ctx := context.Background()
	if _, err := e.sys.Exec(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ('server.failover', 'done', 'server', 'a', 'admin')`); err != nil {
		t.Fatal(err)
	}
	if err := (sysHandover{s}).Put(ctx, MapHandover{TaskID: e.id, State: HandoverRequesting, Server: "a"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := (sysHandover{s}).Get(ctx); m == nil || m.FailoverEnded {
		t.Fatalf("an older failover counts: %+v", m)
	}
}

// L4. A drain that is cancelled or fails (also while paused for the hand-over) leaves no marker.
func TestTheMarkerIsClearedWhenTheDrainFailsOrIsCancelled(t *testing.T) {
	s, e := resumeEnv(t, &MapHandover{State: HandoverStarted, Server: "a"})
	ctx := context.Background()
	s.onFail(ctx, task.Task{ID: e.id, Error: &task.Error{Code: task.CodeCanceled, Message: "cancelled"}})
	if m, err := (sysHandover{s}).Get(ctx); err != nil || m != nil {
		t.Fatalf("the marker stays after the cancel: %+v %v", m, err)
	}
}
