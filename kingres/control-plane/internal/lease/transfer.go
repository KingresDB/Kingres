// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"errors"
	"fmt"
	"time"
)

// The two additions of the failover to the lease protocol (design
// ha-phase5-part2.md, 3.1.1). Both only make the current holder hand a lease over
// earlier than it would run out; neither lets anybody else take it. The rule of the
// safety argument stays: a grantor never grants a lease to a holder other than the
// current one before receipt + duration + margin, except when the current holder
// itself says so.

// Transfer makes `to` the holder of the group's lease for a fresh duration from
// now, when caller is the holder of record (even when its lease has not run out):
// the hand-over is atomic at this grantor, there is no instant at which the group
// has no holder, so the old holder cannot be granted it again. Any other caller is
// refused (Granted false, the holder named). The change is on disk before the
// answer, as every change of holder is; a failure to record it refuses the transfer.
//
// Whether the caller may still act under the lease is its own business: it calls
// this only when no primary runs under the lease (a clean stop, or a group it
// holds as the fence holder).
func (g *Grantor) Transfer(group int32, caller, to string, duration time.Duration) (Reply, error) {
	if caller == "" || to == "" || duration <= 0 {
		return Reply{}, errors.New("lease: a caller, a target and a positive duration are required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clk.Mono()
	cur, have := g.grants[group]
	if !have || cur.holder != caller {
		who := ""
		var wait int64
		if have {
			who = cur.holder
			if until := cur.expiry + g.margin; now < until {
				wait = (until - now).Milliseconds() + 1
			}
		}
		return Reply{Granted: false, Holder: who, WaitMs: wait}, nil
	}
	old := cur
	g.grants[group] = grant{holder: to, expiry: now + duration, duration: duration}
	if err := g.persistLocked(); err != nil {
		g.grants[group] = old
		return Reply{}, fmt.Errorf("lease: recording the transfer: %w", err)
	}
	return Reply{Granted: true, Holder: to}, nil
}

// Release deletes the grant of the group when caller is its holder of record: the
// lease is free at once. It is for a holder that has confirmed that no primary runs
// under the lease and that the fence marker is on disk. Any other caller is
// refused. A group with no grant answers Granted true (nothing to release; the
// caller's aim is reached).
func (g *Grantor) Release(group int32, caller string) (Reply, error) {
	if caller == "" {
		return Reply{}, errors.New("lease: a caller is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cur, have := g.grants[group]
	if !have {
		return Reply{Granted: true}, nil
	}
	if cur.holder != caller {
		return Reply{Granted: false, Holder: cur.holder}, nil
	}
	delete(g.grants, group)
	if err := g.persistLocked(); err != nil {
		g.grants[group] = cur
		return Reply{}, fmt.Errorf("lease: recording the release: %w", err)
	}
	return Reply{Granted: true}, nil
}

// Margin is the margin this grantor waits after a lease ran out.
func (g *Grantor) Margin() time.Duration { return g.margin }
