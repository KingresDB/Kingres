// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// P2, the positive case (the one-standby case is TestADeadOnlyCandidateIsReleasedByTheLocalCommand). The map group has two standbys, on four servers. The old
// leader hands the map group to one of them (the candidate); the candidate takes the lease and dies before it promotes. The old leader's primary is stopped and held back for
// a server that cannot answer. The operator declares the candidate lost (abandonMapGroupBootstrap with lostServerId), to a server that is not the leader: the second
// standby shows that nothing was promoted, the old leader ends its hold and runs its primary again, the task of the hand-over ends, and the hard failover to the other standby, which promotes (it can reach a majority
// of the other shard groups) after the operator asks again (or by itself, when the task goes on). The REST API answers again, and nothing acknowledged is lost.
func TestADeadCandidateDeclaredLostWithASecondStandbyReleasesTheOldLeaderAndTheOtherStandbyTakesOver(t *testing.T) {
	c, port, pool := foClusterOf(t, 4, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 2})
	until(t, 10*time.Minute, "two streaming standbys in every group of the four servers", func() bool {
		if st, _ := leader.Do("GET", "/replication/topology", nil, nil); st == 503 {
			return false
		}
		top := topology(t, leader)
		if len(top) < len(c.Nodes) {
			return false
		}
		for _, v := range top {
			if len(v) != 2 || streamingCount(v) != 2 {
				return false
			}
		}
		return true
	})
	until(t, 4*time.Minute, "both standbys of every group registered in the catalog of the database fo", func() bool {
		fc := leader.ServicePG("fo")
		defer func() { _ = fc.Close(context.Background()) }()
		var n int
		return fc.QueryRow(context.Background(), `SELECT count(*) FROM kingres.node WHERE role = 'standby'`).Scan(&n) == nil && n >= 2*len(c.Nodes)
	})
	views := topology(t, leader)[0]
	if len(views) != 2 {
		t.Fatalf("the map group has %d standbys, want 2: %v", len(views), topology(t, leader))
	}
	nw, third := nodeOf(c, views[0].server), nodeOf(c, views[1].server)
	if nw == nil || third == nil || nw == leader || third == leader || nw == third {
		t.Fatalf("the standbys of the map group are on %q and %q", views[0].server, views[1].server)
	}
	var survivors []*clustertest.Node
	for _, n := range c.Nodes {
		if n != nw {
			survivors = append(survivors, n)
		}
	}
	// The writers use the servers that stay alive; a row that a group of the dead server cannot take is not acknowledged and not counted.
	w := startFoWriters(survivors, port, pool)
	until(t, 2*time.Minute, "rows acknowledged before the hand-over", func() bool { return w.count() > 40 })
	defer func() {
		if t.Failed() {
			t.Logf("old leader log: %s", quietLog(leader.LogTail(60000)))
			t.Logf("candidate log: %s", quietLog(nw.LogTail(20000)))
			t.Logf("other standby log: %s", quietLog(third.LogTail(60000)))
		}
	}()

	// the old leader keeps the keys of the administrators on disk, so that it can check the operator's declaration while its system database is stopped
	until(t, 2*time.Minute, "the cache of the administrators' keys on the old leader", func() bool {
		b, err := os.ReadFile(filepath.Join(leader.DataDir, "failover", "admin-keys.json"))
		return err == nil && strings.Contains(string(b), clustertest.AdminUser)
	})
	nw.SetFaults(fault.Spec{FailoverPoint: "promote:unavailable", FailoverMode: "unavailable"})
	id := mapHardRequest(t, leader, nw, true)
	mapStopped(t, leader)
	until(t, 3*time.Minute, "the candidate to hold the lease and wait for its promotion", func() bool {
		s, b := nw.Do("GET", "/failover", nil, nil)
		return s == 200 && b["state"] == "running"
	})
	// the declaration is the operator's: his session on the other standby signs it for the old leader (a member's word is not taken)
	until(t, 3*time.Minute, "the operator to sign in on the other standby", func() bool { return third.Login(clustertest.AdminUser, clustertest.AdminPass) })
	declare := func() (int, map[string]any) {
		return third.Do("POST", "/failover/map/abandon", map[string]any{"reason": "the candidate is lost", "lostServerId": nw.Host}, nil)
	}
	// While the candidate answers, it is not lost: the declaration is refused and nothing changes.
	if st, body := declare(); st != 409 {
		t.Fatalf("a declaration about a server that answers: %d %v, want 409", st, body)
	}
	foKillServer(nw)

	// The server that stays sees the candidate dead after a while; until then it answers that the server is heard.
	until(t, 6*time.Minute, "the declaration that the candidate is lost to release the old leader", func() bool {
		st, body := declare()
		t.Logf("declaration: %d %v", st, body)
		return st == 200 && body["resumedServerId"] == leader.Host
	})

	// The task that handed the map group to the lost candidate ends: it is given up (no_candidate: the requested standby is not an eligible candidate any more), or it goes on
	// with the other standby by itself. In the first case the operator asks again, naming the other standby; the old leader runs its primary of the map group meanwhile.
	first := foWaitTask(t, third, id, 12*time.Minute, "done", "failed")
	if first["state"] == "failed" {
		if e, _ := first["error"].(map[string]any); e == nil || e["code"] != "no_candidate" {
			t.Fatalf("the hand-over to the lost candidate failed with %v, want no_candidate", first["error"])
		}
		until(t, 5*time.Minute, "the old leader to run its primary of the map group under its lease and answer", func() bool {
			hosts, _ := primaryOf(t, third)
			s, _ := leader.Do("GET", "/cluster", nil, nil)
			var ls failover.State
			code, err := leader.InternalCall(leader, "GET", "/failover/state?group=0", nil, &ls)
			return hosts[0] == leader.Host && s == 200 && err == nil && code == 200 && ls.Holds && len(ls.OutOfRecovery()) == 1
		})
		id = mapHardRequest(t, leader, third, true)
	}
	// The other standby takes the map group over: the task ends done on the new leader.
	mapHardDone(t, leader, third, id)
	hosts, _ := primaryOf(t, third)
	if hosts[0] != third.Host {
		t.Fatalf("the primary of the map group is on %s, want %s", hosts[0], third.Host)
	}
	body := third.Expect(200, "GET", "/cluster", nil)
	if ml, _ := body["managerLeader"].(map[string]any); ml == nil || ml["serverId"] != third.Host {
		t.Fatalf("the manager leader is %v, want %s", body["managerLeader"], third.Host)
	}
	var st failover.State
	until(t, 3*time.Minute, "the old leader's server to answer the new leader", func() bool {
		code, err := third.InternalCall(leader, "GET", "/failover/state?group=0", nil, &st)
		return err == nil && code == 200
	})
	if len(st.OutOfRecovery()) != 0 {
		t.Errorf("the old leader's instance of the map group is a primary: %+v", st)
	}
	rep := third.Expect(200, "GET", "/failover/reports/"+fmt.Sprint(id), nil)
	if gs, _ := rep["groups"].([]any); len(gs) != 1 {
		t.Fatalf("report: %v", rep)
	} else if g := gs[0].(map[string]any); g["toServerId"] != third.Host || g["fromServerId"] != leader.Host {
		t.Errorf("report of the map group: %v", g)
	}
	// The REST API of every server that is alive answers again.
	for _, n := range survivors {
		until(t, 3*time.Minute, "the REST API of "+n.Host, func() bool { s, _ := n.Do("GET", "/cluster", nil, nil); return s == 200 })
	}
	// The groups of the dead server are failed over, the writers go on, and nothing acknowledged is lost.
	hs, resp := third.DoThroughRestart("POST", "/servers/"+nw.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true, "comment": "the lost candidate"}, nil)
	if hs != 202 {
		t.Fatalf("failover of the lost server: %d %v", hs, resp)
	}
	if ft := foWaitTask(t, third, clustertest.TaskID(resp), 10*time.Minute, "done", "failed"); ft["state"] != "done" {
		t.Fatalf("the failover of the lost server ended %v: %v", ft["state"], ft["error"])
	}
	before := w.count()
	until(t, 5*time.Minute, "writes after the end", func() bool { return w.count() > before+30 })
	acked := w.Stop()
	for _, n := range survivors {
		if n == leader { // the old leader's entry point needs the map group, and its instance is the rebuilding old primary (as in mapAckedExcept)
			continue
		}
		have := foReadIDs(t, n, port, pool)
		for rid := range acked {
			if !have[rid] {
				t.Errorf("acknowledged row %d is missing when read through %s", rid, n.Host)
				break
			}
		}
	}
}
