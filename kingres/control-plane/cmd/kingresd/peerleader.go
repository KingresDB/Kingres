// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// peerAnswer is what one member says to GET /leader: the leader it knows, and whether it is itself held back (its instance is stopped or waits for its
// group lease), in which case what it names is only what it remembers and is not counted.
type peerAnswer struct {
	Leader   string `json:"leader"`
	HeldBack bool   `json:"heldBack"`
}

// agreedLeader is the leader that the members name to a server that cannot read its own catalog, or "". The members are `total` in all (this server
// among them): a majority of them must be accounted for, this server counting as one, by answers that name a leader, come from a server that is not
// itself held back and are not errors; every counted answer must name the same leader. A server that does not answer, answers that it is held back,
// or names nobody is not counted, so a minority on the far side of a partition, or servers that remember an old leader, cannot make a caller the leader.
// Nothing is kept between calls: a leader that changed is seen at the next call.
func agreedLeader(ctx context.Context, peers []string, total int, ask func(ctx context.Context, peer string) (peerAnswer, error)) string {
	// The members are asked side by side (phase 5 review L4): one that does not answer costs one timeout for the call, not one per member.
	answers := make([]peerAnswer, len(peers))
	ok := make([]bool, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if a, err := ask(cctx, p); err == nil {
				answers[i], ok[i] = a, true
			}
		}()
	}
	wg.Wait()
	counted := 1 // this server
	leader := ""
	for i := range peers {
		a := answers[i]
		if !ok[i] || a.HeldBack || a.Leader == "" {
			continue
		}
		if leader != "" && a.Leader != leader {
			return ""
		}
		leader = a.Leader
		counted++
	}
	if leader == "" || counted < total/2+1 {
		return ""
	}
	return leader
}

// peerLeaderOf asks the other servers of the peer directory who the manager leader is (GET /leader, which any peer may ask), for the calls of policy
// LeaderOrHeldBack on a server that cannot read its own catalog: the old primary of a failover is rebuilt while its instance is stopped, and the end of
// a switchover is made to it. See agreedLeader for what counts.
func peerLeaderOf(d internalDeps) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		if d.members == nil || d.members.Peers == nil || d.members.Client == nil {
			return ""
		}
		var others []string
		total := 0
		for _, p := range d.members.Peers.All() {
			total++
			if p.ServerID != d.serverHost {
				others = append(others, p.ServerID)
			}
		}
		return agreedLeader(ctx, others, total, func(ctx context.Context, peer string) (peerAnswer, error) {
			var out peerAnswer
			err := d.members.Client.Do(ctx, peer, http.MethodGet, "/leader", nil, &out)
			return out, err
		})
	}
}
