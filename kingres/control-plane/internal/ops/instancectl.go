// SPDX-License-Identifier: PostgreSQL

package ops

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Start, stop and restart of the local instance (README "Starting and
// stopping the instance"). The system database of this server is hosted by the
// instance, so the task of a stop is recorded as done before the shutdown and
// the task of a start is kept in memory until the database is back.

// Task kinds of the instance operations (KindInstanceRestart is in databases.go).
const (
	KindInstanceStart = "instance.start"
	KindInstanceStop  = "instance.stop"
)

// States of the local instance (the contract's Instance.state).
const (
	StateRunning    = "running"
	StateStarting   = "starting"
	StateStopping   = "stopping"
	StateStopped    = "stopped"
	StateRestarting = "restarting"
)

// LifecycleControl is what start, stop and restart need from the instance
// manager (*instance.Manager). Ops.Instances must implement it for these
// operations to be available.
type LifecycleControl interface {
	Running(ctx context.Context, id int) (bool, error)
	Start(ctx context.Context, id int) error
	Stop(ctx context.Context, id int) error
	Restart(ctx context.Context, id int) error
}

// downProbeEvery bounds how often a stopped instance is looked at again to
// notice that an operator started it on the machine.
const downProbeEvery = 2 * time.Second

// lifecycle is the in-memory state of the local instance's start and stop.
type lifecycle struct {
	mu sync.Mutex
	// mem is the operation in flight (starting, stopping, restarting), or
	// stopped after a stop on purpose; "" follows the process.
	mem string
	// down is true from just before a stop's shutdown until the instance and
	// its system database are back after a start: the HTTP layer answers 503
	// system_database_stopped meanwhile.
	down     bool
	probedAt time.Time
	// reserved is a task id taken from the sequence while the database was up,
	// for the start task that follows the stop.
	reserved int64
	lastID   int64
	// pending are finished start tasks not written to the database yet.
	pending []task.Task
}

func (l *lifecycle) set(mem string, down bool) {
	l.mu.Lock()
	l.mem, l.down = mem, down
	l.mu.Unlock()
}

// clearIf resets the state when it is still mem (an operation that ended must
// not undo the state a later one set).
func (l *lifecycle) clearIf(mem string) {
	l.mu.Lock()
	if l.mem == mem {
		l.mem, l.down = "", false
	}
	l.mu.Unlock()
}

func (o *Ops) lifecycleCtl() (LifecycleControl, error) {
	l, ok := o.Instances.(LifecycleControl)
	if !ok || l == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server is not configured to manage a PostgreSQL instance (pg-bin-dir).")
	}
	return l, nil
}

// stateLocked derives the state from the in-memory state and whether the
// process runs.
func (l *lifecycle) stateLocked(up bool) string {
	switch l.mem {
	case StateStarting, StateStopping, StateRestarting:
		return l.mem
	}
	if up {
		if l.mem == StateStopped {
			// An operator started it on the machine.
			l.mem, l.down = "", false
		}
		return StateRunning
	}
	return StateStopped
}

// InstanceState returns the state of the local instance.
func (o *Ops) InstanceState(ctx context.Context) (string, error) {
	lc, err := o.lifecycleCtl()
	if err != nil {
		return "", err
	}
	up, err := lc.Running(ctx, cluster.NodeID)
	if err != nil {
		return "", problem.New(problem.ServiceUnavailable, "The state of the instance cannot be read.").WithRetryAfter(5)
	}
	o.life.mu.Lock()
	defer o.life.mu.Unlock()
	return o.life.stateLocked(up), nil
}

// LocalInstanceDown reports whether the local instance was stopped on purpose
// (or is being stopped or started) so that every authenticated operation but
// the start answers 503 system_database_stopped. It follows the process: when
// an operator starts the instance on the machine it becomes false again.
func (o *Ops) LocalInstanceDown() bool {
	o.life.mu.Lock()
	down, mem, probed := o.life.down, o.life.mem, o.life.probedAt
	if !down {
		o.life.mu.Unlock()
		return false
	}
	if mem != StateStopped || time.Since(probed) < downProbeEvery {
		o.life.mu.Unlock()
		return true
	}
	o.life.probedAt = time.Now()
	o.life.mu.Unlock()
	lc, err := o.lifecycleCtl()
	if err != nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if up, err := lc.Running(ctx, cluster.NodeID); err == nil && up {
		o.life.mu.Lock()
		if o.life.mem == StateStopped {
			o.life.mem, o.life.down = "", false
		}
		down = o.life.down
		o.life.mu.Unlock()
		return down
	}
	return true
}

// StartMayUseStaleRoles reports whether startThisServerInstance may be
// authorized from the cluster roles of the last successful check: only while
// the local instance is verifiably not running (stopped on purpose or not),
// because then the roles cannot be asked of PostgreSQL. Never for another
// operation (CP-02), never while the instance runs.
func (o *Ops) StartMayUseStaleRoles(ctx context.Context) bool {
	if o.LocalInstanceDown() {
		return true
	}
	st, err := o.InstanceState(ctx)
	return err == nil && (st == StateStopped || st == StateStarting)
}

// beginOp checks the state against the operation and enters the state of the
// operation in one step. op is "start", "stop" or "restart". The returned
// function undoes it (when the task cannot be created).
func (o *Ops) beginOp(ctx context.Context, op string) (undo func(), err error) {
	lc, err := o.lifecycleCtl()
	if err != nil {
		return nil, err
	}
	up, rerr := lc.Running(ctx, cluster.NodeID)
	if rerr != nil {
		return nil, problem.New(problem.ServiceUnavailable, "The state of the instance cannot be read.").WithRetryAfter(5)
	}
	o.life.mu.Lock()
	defer o.life.mu.Unlock()
	st := o.life.stateLocked(up)
	prevMem, prevDown := o.life.mem, o.life.down
	busy := problem.New(problem.OperationInProgress, "Another start, stop or restart of this instance is running.")
	switch op {
	case "stop":
		switch st {
		case StateStopped, StateStopping:
			return nil, problem.New(problem.InstanceAlreadyStopped, "The instance is already stopped (or being stopped).")
		case StateStarting, StateRestarting:
			return nil, busy
		}
		// down at once: from accepting the stop on, the database may end the
		// connections of a request at any moment, and such a request must be
		// answered system_database_stopped, not as a store failure (defect 4).
		o.life.mem, o.life.down = StateStopping, true
	case "start":
		switch st {
		case StateRunning, StateStarting, StateRestarting:
			return nil, problem.New(problem.InstanceAlreadyRunning, "The instance is already running (or being started).")
		case StateStopping:
			return nil, busy
		}
		o.life.mem, o.life.down = StateStarting, true
	case "restart":
		switch st {
		case StateStopped, StateStopping:
			return nil, problem.New(problem.InstanceAlreadyStopped, "The instance is stopped; use startThisServerInstance.")
		case StateStarting, StateRestarting:
			return nil, busy
		}
		o.life.mem, o.life.down = StateRestarting, true
	default:
		return nil, errors.New("ops: unknown instance operation")
	}
	return func() { o.life.set(prevMem, prevDown) }, nil
}
