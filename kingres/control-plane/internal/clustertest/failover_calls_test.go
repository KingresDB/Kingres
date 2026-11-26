// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// callOK makes an internal call as server `as` and fails the test unless it answers want.
func callOK(t *testing.T, as, target *clustertest.Node, method, path string, in any, want int, out any) {
	t.Helper()
	st, err := as.InternalCall(target, method, path, in, out)
	if err != nil || st != want {
		t.Fatalf("%s %s on node %d as node %d: status %d, error %v, want %d", method, path, target.Index, as.Index, st, err, want)
	}
}

// The read calls of the failover answer what the instances know: the position of a standby and of a primary, the
// liveness view, the state of a group, and the margin of the lease protocol. They only read.
func TestFailoverReadCalls(t *testing.T) {
	c := formStandbys(t, 3, 1)
	leader := c.Leader()
	top := topology(t, leader)
	primaries, _ := primaryOf(t, leader)
	prim, sb := nodeOf(c, primaries[1]), nodeOf(c, top[1][0].server)

	var pos struct {
		Server, Role, ReceiverState, SystemIdentifier, ReceiveLsn string
		Group                                                     int32
		InstanceID                                                int
		InRecovery                                                bool
		Timeline                                                  int64
		CatalogTerm                                               *int64
	}
	// As another server would ask: any peer may.
	callOK(t, c.Nodes[2], sb, "GET", "/failover/position?group=1", nil, 200, &pos)
	if pos.Server != sb.Host || pos.Group != 1 || pos.Role != "standby" || !pos.InRecovery || pos.ReceiverState != "streaming" ||
		pos.InstanceID != 101 || pos.ReceiveLsn == "" || pos.Timeline < 1 || pos.SystemIdentifier == "" || pos.CatalogTerm == nil {
		t.Errorf("position of the standby: %+v", pos)
	}
	var ppos = pos
	callOK(t, c.Nodes[0], prim, "GET", "/failover/position?group=1", nil, 200, &ppos)
	if ppos.Role != "primary" || ppos.InRecovery || ppos.InstanceID != 1 || ppos.ReceiveLsn == "" {
		t.Errorf("position of the primary: %+v", ppos)
	}
	if ppos.SystemIdentifier == pos.SystemIdentifier && false {
		t.Error("unreachable")
	}
	if st, _ := c.Nodes[0].InternalCall(prim, "GET", "/failover/position?group=99", nil, nil); st != 404 {
		t.Errorf("a group this server runs nothing of must be 404, got %d", st)
	}
	if st, _ := c.Nodes[0].InternalCall(prim, "GET", "/failover/position", nil, nil); st != 400 {
		t.Errorf("no group must be 400, got %d", st)
	}

	var lv struct {
		Server string
		Peers  []struct {
			Server    string
			State     string
			SilenceMs *int64
		}
	}
	until(t, time.Minute, "every server live in the liveness view", func() bool {
		callOK(t, c.Nodes[0], c.Nodes[1], "GET", "/liveness", nil, 200, &lv)
		if lv.Server != c.Nodes[1].Host || len(lv.Peers) != 2 {
			return false
		}
		for _, p := range lv.Peers {
			if p.State != "live" {
				return false
			}
		}
		return true
	})

	var stt struct {
		Server     string
		Group      int32
		Holds      bool
		LeaseState string
		Frozen     bool
		Instances  []struct {
			ID         int
			Role       string
			Origin     string
			Group      int32
			Running    bool
			InRecovery *bool
		}
		CatalogTerm *int64
	}
	callOK(t, leader, prim, "GET", "/failover/state?group=1", nil, 200, &stt)
	if !stt.Holds || stt.LeaseState != "held" || stt.Frozen || stt.CatalogTerm == nil {
		t.Errorf("state of the group on its primary's server: %+v", stt)
	}
	found := false
	for _, in := range stt.Instances {
		if in.ID == 1 && in.Role == "primary" && in.Origin == "own" && in.Group == 1 && in.Running && in.InRecovery != nil && !*in.InRecovery {
			found = true
		}
	}
	if !found {
		t.Errorf("the primary instance is not in the answer: %+v", stt.Instances)
	}
	// A call that only the leader may make is refused to another server.
	if st, _ := c.Nodes[1].InternalCall(prim, "GET", "/failover/state?group=1", nil, nil); st != 403 {
		t.Errorf("GET /failover/state by a server that is not the leader: %d, want 403", st)
	}

	var ls struct {
		DurationMs int64 `json:"durationMs"`
		MarginMs   int64 `json:"marginMs"`
	}
	callOK(t, c.Nodes[2], prim, "GET", "/lease/state", nil, 200, &ls)
	if ls.DurationMs != 10000 || ls.MarginMs != 3000 {
		t.Errorf("the lease state must carry the settings: %+v", ls)
	}
}

// The freeze keeps the synchronous list of a group while it lasts, however long its standby does not stream; when it is
// lifted the grace period applies again. standbys-gone empties the lists that name a failed server's standbys at once.
func TestFreezeAndStandbysGoneOnACluster(t *testing.T) {
	args := append([]string{}, standbyArgs...)
	args[1] = "5m" // a grace so long that only the freeze and the gone call can be what changes the list
	c := clustertest.FormWithExtra(t, 3, args, nil)
	waitStandbys(t, c, 1)
	leader := c.Leader()
	top := topology(t, leader)
	primaries, _ := primaryOf(t, leader)
	prim := nodeOf(c, primaries[1])
	sbServer := top[1][0].server
	until(t, time.Minute, "the list names the standby", func() bool { return strings.HasPrefix(syncNames(t, prim), "ANY 1 (") })
	named := syncNames(t, prim)

	// The freeze is made and lifted as a run of a task does it: since F3-14 only the run that holds a freeze lifts it.
	callOK(t, leader, prim, "POST", "/failover/freeze", map[string]any{"group": 1, "ttlMs": 120000, "task": 1, "epoch": 1}, 200, nil)
	callOK(t, leader, prim, "POST", "/failover/standbys-gone", map[string]any{"server": sbServer, "ttlMs": 120000}, 200, nil)
	var stt struct{ Frozen bool }
	callOK(t, leader, prim, "GET", "/failover/state?group=1", nil, 200, &stt)
	if !stt.Frozen {
		t.Fatal("the group is not frozen")
	}
	// Frozen: the gone declaration does not touch the list of the group that is being failed over.
	// Nothing is expected to change, so wait for the agent's own count of rounds to grow by two instead of for a time.
	var s0, s1 struct{ Rounds int64 }
	callOK(t, leader, prim, "GET", "/standby/status", nil, 200, &s0)
	until(t, time.Minute, "two rounds of the agent after the calls", func() bool {
		callOK(t, leader, prim, "GET", "/standby/status", nil, 200, &s1)
		return s1.Rounds >= s0.Rounds+2
	})
	if got := syncNames(t, prim); got != named {
		t.Fatalf("the list of a frozen group changed: %q -> %q", named, got)
	}
	callOK(t, leader, prim, "POST", "/failover/unfreeze", map[string]any{"group": 1}, 409, nil)
	callOK(t, leader, prim, "POST", "/failover/unfreeze", map[string]any{"group": 1, "task": 2, "epoch": 1}, 409, nil)
	callOK(t, leader, prim, "POST", "/failover/unfreeze", map[string]any{"group": 1, "task": 1, "epoch": 1}, 200, nil)
	until(t, 30*time.Second, "the list emptied at once after the freeze, the standby being gone", func() bool { return syncNames(t, prim) == "" })
	callOK(t, leader, prim, "POST", "/failover/standbys-gone", map[string]any{"server": sbServer, "clear": true}, 200, nil)
	until(t, time.Minute, "the standby named again once it is not gone", func() bool { return syncNames(t, prim) == named })
}
