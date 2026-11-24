// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// P2, the positive case (the negative one is TestADeadOnlyCandidateDeclaredLostDoesNotReleaseTheOldLeader). The map group has two standbys. The old leader hands the
// map group to one of them (the candidate); the candidate takes the lease and dies before it promotes. The old leader's primary is stopped and held back for a server that
// cannot answer. The operator declares the candidate lost (abandonMapGroupBootstrap with lostServerId), to any server: the second standby shows that nothing was promoted, the
// old leader ends its hold and holds the map group again, the REST API answers on the old leader again, and the second standby can take the map group over in a new
// switchover. Nothing acknowledged is lost.
func TestADeadCandidateDeclaredLostWithASecondStandbyReleasesTheOldLeaderAndTheOtherStandbyTakesOver(t *testing.T) {
	c, port, pool := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 2})
	waitStandbys(t, c, 2)
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
	// The writers use the servers that stay alive; a row that the dead server's groups cannot take is not acknowledged and not counted.
	w := startFoWriters([]*clustertest.Node{leader, third}, port, pool)
	until(t, 2*time.Minute, "rows acknowledged before the hand-over", func() bool { return w.count() > 40 })
	defer func() {
		if t.Failed() {
			t.Logf("old leader log: %s", quietLog(leader.LogTail(60000)))
			t.Logf("candidate log: %s", quietLog(nw.LogTail(30000)))
			t.Logf("other standby log: %s", quietLog(third.LogTail(60000)))
		}
	}()

	nw.SetFaults(fault.Spec{FailoverPoint: "promote:unavailable", FailoverMode: "unavailable"})
	_ = mapHardRequest(t, leader, nw, true)
	mapStopped(t, leader)
	until(t, 3*time.Minute, "the candidate to hold the lease and wait for its promotion", func() bool {
		s, b := nw.Do("GET", "/failover", nil, nil)
		return s == 200 && b["state"] == "running"
	})
	declare := func() (int, map[string]any) {
		return third.Do("POST", "/failover/map/abandon", map[string]any{"reason": "the candidate is lost", "lostServerId": nw.Host}, nil)
	}
	// While the candidate answers, it is not lost: the declaration is refused and nothing changes.
	if st, body := declare(); st != 409 {
		t.Fatalf("a declaration about a server that answers: %d %v, want 409", st, body)
	}
	foKillServer(nw)

	// The server that stays sees the candidate dead after a while; until then it answers that the server is heard.
	var res map[string]any
	until(t, 6*time.Minute, "the declaration that the candidate is lost to release the old leader", func() bool {
		st, body := declare()
		t.Logf("declaration: %d %v", st, body)
		if st == 200 && body["resumedServerId"] == leader.Host {
			res = body
			return true
		}
		return false
	})
	if res["abandoned"] == true {
		t.Errorf("a bootstrap that none of the servers held was reported given up: %v", res)
	}
	until(t, 6*time.Minute, "the old leader to hold the map group again", func() bool {
		hosts, _ := primaryOf(t, third)
		return hosts[0] == leader.Host
	})
	until(t, 3*time.Minute, "the REST API of the old leader", func() bool {
		st, _ := leader.Do("GET", "/cluster", nil, nil)
		return st == 200
	})
	if st, _ := third.Do("GET", "/cluster", nil, nil); st != 200 {
		t.Errorf("the REST API of the other standby answers %d", st)
	}
	// What was declared lost stays a refusal for a server that is heard, and the repeat of the declaration finds nothing held back.
	if st, body := declare(); st != 200 || body["resumedServerId"] != nil {
		t.Errorf("the repeated declaration: %d %v, want 200 and nothing resumed", st, body)
	}

	// The other standby takes the map group over.
	id := mapSwitchoverRequest(t, leader, third)
	task := foWaitTask(t, third, id, 12*time.Minute, "done", "failed")
	if task["state"] != "done" {
		t.Fatalf("the switchover to the other standby ended %v: %v", task["state"], task["error"])
	}
	hosts, _ := primaryOf(t, third)
	if hosts[0] != third.Host {
		t.Fatalf("the primary of the map group is on %s, want %s", hosts[0], third.Host)
	}
	if body := third.Expect(200, "GET", "/cluster", nil); body["managerLeader"] == nil || body["managerLeader"].(map[string]any)["serverId"] != third.Host {
		t.Errorf("the manager leader is %v, want %s", body["managerLeader"], third.Host)
	}
	// The dead server's own groups are failed over, and nothing acknowledged is lost.
	st, resp := third.DoThroughRestart("POST", "/servers/"+nw.Host+"/failover", map[string]any{"mode": "hard", "comment": "the lost candidate"}, nil)
	if st != 202 {
		t.Fatalf("failover of the lost server: %d %v", st, resp)
	}
	if ft := foWaitTask(t, third, clustertest.TaskID(resp), 10*time.Minute, "done", "failed"); ft["state"] != "done" {
		t.Fatalf("the failover of the lost server ended %v: %v", ft["state"], ft["error"])
	}
	before := w.count()
	until(t, 5*time.Minute, "writes after the end: "+fmt.Sprint(strings.TrimSpace(w.lastError())), func() bool { return w.count() > before+30 })
	acked := w.Stop()
	for _, n := range []*clustertest.Node{leader, third} {
		have := foReadIDs(t, n, port, pool)
		for rid := range acked {
			if !have[rid] {
				t.Errorf("acknowledged row %d is missing when read through %s", rid, n.Host)
				break
			}
		}
	}
}
