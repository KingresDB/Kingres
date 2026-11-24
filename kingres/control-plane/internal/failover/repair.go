// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The repair of what a switchover leaves behind (review F4-02). A switchover changes three things on the server of the old primary that
// are files and survive its restart: the hold rules (and the backup of the real ones), the relinquished marker, and the wait marker. The task
// undoes them in one pass (rollbackSwitchover), which cannot reach a server that is down, and a leader that changes or a task that ends
// in an odd way does not undo them at all. Repair is the loop that does, whatever happened to the task: when no failover is unfinished, a
// group whose catalog primary is held (the writes are stopped, or its primary was given up) for longer than Grace is resumed.

// RepairPorts is what the loop reads and does.
type RepairPorts interface {
	// Primaries lists the groups (the map group excluded) with the server that runs the primary in the catalog.
	Primaries(ctx context.Context) (map[int32]string, error)
	State(ctx context.Context, server string, group int32) (State, error)
	// Term is the catalog term now: the undo calls carry it, so that a callee that has seen a newer one refuses (F4-09).
	Term(ctx context.Context) (int64, error)
	Resume(ctx context.Context, server string, group int32, term int64) error
	Unquiesce(ctx context.Context, server string, group int32, term int64) error
	// Unfinished says that a failover or restore task is pending, running or paused.
	Unfinished(ctx context.Context) (bool, error)
	Log(msg string, kv ...any)
}

// Repair is the loop on the manager leader.
type Repair struct {
	P        RepairPorts
	Interval time.Duration // default 15 s
	Grace    time.Duration // how long a hold is seen with no task before it is undone (default 60 s)
	Now      func() time.Time

	mu    sync.Mutex
	since map[int32]time.Time
}

func (r *Repair) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Step is one look. It returns the groups it repaired.
func (r *Repair) Step(ctx context.Context) ([]int32, error) {
	r.mu.Lock()
	if r.since == nil {
		r.since = map[int32]time.Time{}
	}
	r.mu.Unlock()
	busy, err := r.P.Unfinished(ctx)
	if err != nil {
		return nil, err
	}
	if busy {
		// A task owns the holds; what it leaves is repaired when it has ended.
		r.mu.Lock()
		r.since = map[int32]time.Time{}
		r.mu.Unlock()
		return nil, nil
	}
	prim, err := r.P.Primaries(ctx)
	if err != nil {
		return nil, err
	}
	var done []int32
	for g, server := range prim {
		st, serr := r.P.State(ctx, server, g)
		if serr != nil {
			continue // a server that does not answer is looked at again
		}
		held := st.Quiesced || st.Relinquished
		r.mu.Lock()
		first, seen := r.since[g]
		if !held {
			delete(r.since, g)
			r.mu.Unlock()
			continue
		}
		if !seen {
			r.since[g] = r.now()
			r.mu.Unlock()
			continue
		}
		r.mu.Unlock()
		if r.now().Sub(first) < orDur(r.Grace, time.Minute) {
			continue
		}
		// Look at the tasks again just before: a task that began meanwhile owns the hold.
		if busy, err := r.P.Unfinished(ctx); err != nil || busy {
			continue
		}
		r.P.Log("repairing a group that a switchover left held", "group", g, "server", server, "quiesced", st.Quiesced, "relinquished", st.Relinquished)
		term, terr := r.P.Term(ctx)
		if terr != nil {
			continue
		}
		// POST /failover/resume is leader-only on purpose (it is not "leader or held back"): it starts a primary, which a server whose instance waits
		// for its lease must not do on the word of the peers alone. A server that is held back answers 503, the group stays in r.since (it is deleted
		// only after both calls succeeded) and the next round of Run repeats the repair.
		rerr := errors.Join(r.P.Resume(ctx, server, g, term), r.P.Unquiesce(ctx, server, g, term))
		if rerr != nil {
			r.P.Log(fmt.Sprintf("the repair of group %d did not finish; it is repeated", g), "error", rerr.Error())
			continue
		}
		r.mu.Lock()
		delete(r.since, g)
		r.mu.Unlock()
		done = append(done, g)
	}
	return done, nil
}

// Run loops until ctx ends.
func (r *Repair) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := r.Step(ctx); err != nil && ctx.Err() == nil {
			r.P.Log("the repair of switchovers cannot read the cluster", "error", err.Error())
		}
		select {
		case <-ctx.Done():
		case <-time.After(orDur(r.Interval, 15*time.Second)):
		}
	}
}
