// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

func member(id int32, role, state string, lag *int64) api.ReplicationMember {
	return api.ReplicationMember{InstanceID: id, Role: role, State: state, LagBytes: lag}
}

func i64(v int64) *int64 { return &v }

func TestSoleUpToDateCopy(t *testing.T) {
	group := func(p *api.ReplicationMember, standbys ...api.ReplicationMember) api.ReplicationTopology {
		return api.ReplicationTopology{Groups: []api.ShardGroup{{ID: 0, Primary: p, Standbys: standbys}}}
	}
	pri := func(id int32) *api.ReplicationMember { m := member(id, "primary", "running", nil); return &m }
	for _, tc := range []struct {
		name string
		topo api.ReplicationTopology
		id   int32
		want bool
	}{
		{"one server, no replicas", group(pri(1)), 1, false},
		{"primary with a caught-up standby", group(pri(1), member(2, "standby", "running", i64(0))), 1, false},
		{"primary whose only standby is behind", group(pri(1), member(2, "standby", "running", i64(50<<20))), 1, true},
		{"primary whose standby has an unknown lag", group(pri(1), member(2, "standby", "running", nil)), 1, true},
		{"primary whose standby is stopped", group(pri(1), member(2, "standby", "stopped", i64(0))), 1, true},
		{"the lagging standby itself", group(pri(1), member(2, "standby", "running", i64(50<<20))), 2, false},
		{"a standby that is the only copy (no primary)", group(nil, member(2, "standby", "running", i64(0)), member(3, "standby", "stopped", nil)), 2, true},
		{"two caught-up standbys", group(pri(1), member(2, "standby", "running", i64(10)), member(3, "standby", "running", i64(0))), 1, false},
		{"another group", group(pri(1), member(2, "standby", "running", i64(99<<20))), 9, false},
	} {
		if got := SoleUpToDateCopy(tc.topo, tc.id); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

type lifeFake struct {
	mu      sync.Mutex
	running bool
	err     error
}

func (f *lifeFake) Running(context.Context, int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, f.err
}
func (f *lifeFake) Start(context.Context, int) error             { return nil }
func (f *lifeFake) Stop(context.Context, int) error              { return nil }
func (f *lifeFake) Restart(context.Context, int) error           { return nil }
func (f *lifeFake) WriteHBA(int, []instance.HBARule, bool) error { return nil }
func (f *lifeFake) Reload(context.Context, int) error            { return nil }
func (f *lifeFake) set(running bool)                             { f.mu.Lock(); f.running = running; f.mu.Unlock() }

// The conflicts of the contract, by state and operation.
func TestBeginOpConflicts(t *testing.T) {
	type tc struct {
		state string
		op    string
		want  problem.Code // "" = allowed
	}
	for _, c := range []tc{
		{StateRunning, "stop", ""}, {StateRunning, "start", problem.InstanceAlreadyRunning}, {StateRunning, "restart", ""},
		{StateStopped, "stop", problem.InstanceAlreadyStopped}, {StateStopped, "start", ""}, {StateStopped, "restart", problem.InstanceAlreadyStopped},
		{StateStopping, "stop", problem.InstanceAlreadyStopped}, {StateStopping, "start", problem.OperationInProgress}, {StateStopping, "restart", problem.InstanceAlreadyStopped},
		{StateStarting, "stop", problem.OperationInProgress}, {StateStarting, "start", problem.InstanceAlreadyRunning}, {StateStarting, "restart", problem.OperationInProgress},
		{StateRestarting, "stop", problem.OperationInProgress}, {StateRestarting, "start", problem.InstanceAlreadyRunning}, {StateRestarting, "restart", problem.OperationInProgress},
	} {
		f := &lifeFake{running: c.state == StateRunning}
		o := &Ops{Instances: f, Log: slog.New(slog.DiscardHandler)}
		switch c.state {
		case StateStopping, StateStarting, StateRestarting:
			o.life.set(c.state, false)
		}
		undo, err := o.beginOp(context.Background(), c.op)
		var p *problem.Problem
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s %s: refused: %v", c.op, c.state, err)
		case c.want != "" && (!errors.As(err, &p) || p.Code != c.want):
			t.Errorf("%s %s: got %v, want %s", c.op, c.state, err, c.want)
		case c.want == "":
			if st, _ := o.InstanceState(context.Background()); st == c.state {
				t.Errorf("%s %s: the state did not change", c.op, c.state)
			}
			undo()
			if st, _ := o.InstanceState(context.Background()); st != c.state && c.state != StateStopped && c.state != StateRunning {
				t.Errorf("%s %s: undo left %s", c.op, c.state, st)
			}
		}
	}
}

// An operator who starts the instance on the machine ends the stopped state; a
// crash does not make the API answer system_database_stopped.
func TestDownFollowsTheProcess(t *testing.T) {
	f := &lifeFake{running: false}
	o := &Ops{Instances: f, Log: slog.New(slog.DiscardHandler)}
	if o.LocalInstanceDown() {
		t.Error("a crash is not a stop on purpose")
	}
	o.life.set(StateStopped, true)
	if !o.LocalInstanceDown() {
		t.Fatal("stopped on purpose")
	}
	f.set(true)
	o.life.mu.Lock()
	o.life.probedAt = o.life.probedAt.Add(-downProbeEvery * 2)
	o.life.mu.Unlock()
	if o.LocalInstanceDown() {
		t.Error("an operator started the instance, the stopped state must end")
	}
	if st, _ := o.InstanceState(context.Background()); st != StateRunning {
		t.Errorf("state %s", st)
	}
	// Without a lifecycle-capable manager nothing is down and nothing is allowed.
	none := &Ops{}
	if none.LocalInstanceDown() || none.StartMayUseStaleRoles(context.Background()) {
		t.Error("an Ops without an instance manager must not gate anything")
	}
}

// Defect 4: from the moment a stop or restart is accepted every authenticated
// operation must be answered system_database_stopped; before, the window in
// which the database ended connections showed up as 503 service_unavailable.
func TestStopAndRestartGateFromTheStart(t *testing.T) {
	for _, op := range []string{"stop", "restart"} {
		f := &lifeFake{running: true}
		o := &Ops{Instances: f, Log: slog.New(slog.DiscardHandler)}
		if o.LocalInstanceDown() {
			t.Fatal("a running instance is not down")
		}
		undo, err := o.beginOp(context.Background(), op)
		if err != nil {
			t.Fatal(err)
		}
		if !o.LocalInstanceDown() {
			t.Errorf("%s accepted but the instance is not reported down", op)
		}
		undo()
		if o.LocalInstanceDown() {
			t.Errorf("%s undone but the instance is still reported down", op)
		}
	}
	// The restart ends: the gate opens again.
	o := &Ops{Instances: &lifeFake{running: true}, Log: slog.New(slog.DiscardHandler)}
	if _, err := o.beginOp(context.Background(), "restart"); err != nil {
		t.Fatal(err)
	}
	o.life.clearIf(StateRestarting)
	if o.LocalInstanceDown() {
		t.Error("the gate stayed closed after the restart")
	}
}
