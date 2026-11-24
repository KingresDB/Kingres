// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// P13R-06: the restart of the configuration code goes through the instance lifecycle: the instance that holds the
// system database is "restarting" meanwhile, a start or stop is refused, and the state ends with the restart.
func TestConfigRestartUsesTheInstanceLifecycle(t *testing.T) {
	ctx := context.Background()
	o := &Ops{Instances: &lifeFake{running: true}, Log: slog.New(slog.DiscardHandler)}
	end, err := o.BeginConfigRestart(ctx, cluster.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := o.InstanceState(ctx); st != StateRestarting {
		t.Errorf("state %s during the restart, want %s", st, StateRestarting)
	}
	if !o.LocalInstanceDown() {
		t.Error("the system database is down during the restart")
	}
	var p *problem.Problem
	if _, err := o.beginOp(ctx, "stop"); !errors.As(err, &p) || p.Code != problem.OperationInProgress {
		t.Errorf("a stop during the restart: %v", err)
	}
	if _, err := o.BeginConfigRestart(ctx, cluster.NodeID); !errors.As(err, &p) || p.Code != problem.OperationInProgress {
		t.Errorf("a second restart: %v", err)
	}
	end(ctx, errors.New("did not start"))
	if st, _ := o.InstanceState(ctx); st != StateRunning {
		t.Errorf("state %s after the restart", st)
	}
	if o.LocalInstanceDown() {
		t.Error("down after the restart ended")
	}
}

// A stopped instance is not started by a configuration restart (the operator stopped it), and an instance that
// does not hold the system database is the manager's business alone.
func TestConfigRestartOfAStoppedOrAnotherInstance(t *testing.T) {
	ctx := context.Background()
	o := &Ops{Instances: &lifeFake{running: false}, Log: slog.New(slog.DiscardHandler)}
	var p *problem.Problem
	if _, err := o.BeginConfigRestart(ctx, cluster.NodeID); !errors.As(err, &p) || p.Code != problem.InstanceAlreadyStopped {
		t.Errorf("a stopped instance: %v", err)
	}
	end, err := o.BeginConfigRestart(ctx, cluster.NodeID+99)
	if err != nil {
		t.Fatal(err)
	}
	end(ctx, nil)
	if st, _ := o.InstanceState(ctx); st == StateRestarting {
		t.Error("another instance must not change the lifecycle state")
	}
}
