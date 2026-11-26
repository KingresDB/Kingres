// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// catalogNode is a standby row of the sharding catalog.
type catalogNode struct {
	id    int32
	group int32
	host  string
	port  int
}

// catalogStandbys reads the standby rows of the sharding catalog as the node
// holds them.
func catalogStandbys(t *testing.T, n *clustertest.Node) []catalogNode {
	t.Helper()
	ctx := context.Background()
	c := n.ServicePG("kingres_system")
	defer func() { _ = c.Close(ctx) }()
	rows, err := c.Query(ctx, `SELECT node_id, group_id, host, port FROM kingres.node WHERE role = 'standby' ORDER BY node_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []catalogNode
	for rows.Next() {
		var x catalogNode
		if err := rows.Scan(&x.id, &x.group, &x.host, &x.port); err != nil {
			t.Fatal(err)
		}
		out = append(out, x)
	}
	return out
}

// recordNodes reads kingres_cp.standby of the leader: standby id to catalog node id (0: none).
func recordNodes(t *testing.T, leader *clustertest.Node) map[int64]int32 {
	t.Helper()
	ctx := context.Background()
	c := leader.ServicePG("kingres_system")
	defer func() { _ = c.Close(ctx) }()
	rows, err := c.Query(ctx, `SELECT standby_id, coalesce(catalog_node_id, 0) FROM kingres_cp.standby WHERE state <> 'removing'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]int32{}
	for rows.Next() {
		var id int64
		var n int32
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		out[id] = n
	}
	return out
}

// syncNames reads synchronous_standby_names of the primary instance of a node.
func syncNames(t *testing.T, n *clustertest.Node) string {
	t.Helper()
	ctx := context.Background()
	c := n.ServicePG("postgres")
	defer func() { _ = c.Close(ctx) }()
	var v string
	if err := c.QueryRow(ctx, `SHOW synchronous_standby_names`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func allRegistered(rec map[int64]int32, want int) bool {
	if len(rec) != want {
		return false
	}
	for _, id := range rec {
		if id == 0 {
			return false
		}
	}
	return true
}

// The standbys kingresd makes are registered in the sharding catalog (kingres_shard
// 0.5, sequence 1): the node id is stored, the primary names the standby in its
// list only then, the catalog's own check shows it streaming and synchronous, and
// the replication view lists the standby under its node id. Lowering the replica
// count takes the name out of the list first and then the row out of the catalog
// (sequence 8), and no group is ever left waiting for a standby that is gone.
func TestStandbysAreRegisteredInTheCatalogAndLeaveIt(t *testing.T) {
	c := formStandbys(t, 3, 1)
	leader := c.Leader()
	until(t, 2*time.Minute, "every standby registered with its node id", func() bool {
		return allRegistered(recordNodes(t, leader), 3) && len(catalogStandbys(t, leader)) == 3
	})
	rec := recordNodes(t, leader)
	byID := map[int32]catalogNode{}
	for _, n := range catalogStandbys(t, leader) {
		byID[n.id] = n
	}
	for sid, nid := range rec {
		if _, ok := byID[nid]; !ok {
			t.Errorf("standby %d has catalog node %d, which the catalog does not have", sid, nid)
		}
	}
	// The replication view lists the standbys under their catalog node ids, with the synchronous state.
	until(t, time.Minute, "the replication view with the node ids of the catalog", func() bool {
		body := leader.Expect(200, "GET", "/replication/topology", nil)
		groups, _ := body["groups"].([]any)
		if len(groups) != 3 {
			return false
		}
		for _, g := range groups {
			sbs, _ := g.(map[string]any)["standbys"].([]any)
			if len(sbs) != 1 {
				return false
			}
			sm := sbs[0].(map[string]any)
			if _, ok := byID[int32(sm["instanceId"].(float64))]; !ok {
				return false
			}
			if ss, _ := sm["syncState"].(string); ss != "quorum" && ss != "sync" {
				return false
			}
		}
		return true
	})
	// Every primary names its standby; the extension's own view agrees.
	for _, n := range c.Nodes {
		until(t, time.Minute, "the primary of "+n.Host+" names its standby", func() bool {
			return strings.HasPrefix(syncNames(t, n), "ANY 1 (kingres_standby_")
		})
	}
	ctx := context.Background()
	sc := leader.ServicePG("kingres_system")
	var bad int
	if err := sc.QueryRow(ctx, `SELECT count(*) FROM kingres.standby_status() WHERE coalesce(problem, '') <> ''`).Scan(&bad); err != nil || bad != 0 {
		t.Errorf("standby_status shows %d problems (err %v)", bad, err)
	}
	_ = sc.Close(ctx)

	// The step that registers is done, not skipped.
	registered := false
	for _, tk := range items(t, leader.Expect(200, "GET", "/tasks?limit=50", nil)) {
		if tk["kind"] != "standby.reconcile" || tk["state"] != "done" {
			continue
		}
		steps, _ := tk["progress"].(map[string]any)["steps"].([]any)
		for _, s := range steps {
			if st := s.(map[string]any); st["name"] == "standby.register_in_catalog" && st["state"] == "done" {
				registered = true
			}
		}
	}
	if !registered {
		t.Error("no finished standby.reconcile task has the step standby.register_in_catalog in the state done")
	}

	// Removal: the replica count goes to zero; the lists empty, the rows leave the catalog, no commit waits.
	if st, body := leader.Do("PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 0}, nil); st != 200 && st != 202 {
		t.Fatalf("lowering the replica count: %d %v", st, body)
	}
	until(t, 3*time.Minute, "the standbys removed from the catalog and the lists", func() bool {
		if len(catalogStandbys(t, leader)) != 0 || len(recordNodes(t, leader)) != 0 {
			return false
		}
		for _, n := range c.Nodes {
			if syncNames(t, n) != "" {
				return false
			}
		}
		return true
	})
	// A commit on every primary still goes through (no list names a standby that is gone).
	for _, n := range c.Nodes {
		p := n.ServicePG("postgres")
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if _, err := p.Exec(cctx, `CREATE TEMP TABLE after_removal AS SELECT 1`); err != nil {
			t.Errorf("a commit on %s after the removal: %v", n.Host, err)
		}
		cancel()
		_ = p.Close(ctx)
	}
}

// A cluster made by the code before the registration existed has ready standbys
// that are named in the lists and are not in the catalog. They are registered by
// the next check by themselves, and the lists keep their names throughout.
func TestStandbysMadeBeforeTheRegistrationAreRegisteredByThemselves(t *testing.T) {
	c := formStandbys(t, 3, 1)
	leader := c.Leader()
	until(t, 2*time.Minute, "the standbys registered", func() bool {
		return allRegistered(recordNodes(t, leader), 3) && len(catalogStandbys(t, leader)) == 3
	})
	ctx := context.Background()
	// Put the cluster back as the earlier code left it: no node id in the records, no row in the catalog.
	sc := leader.ServicePG("kingres_system")
	defer func() { _ = sc.Close(ctx) }()
	for _, n := range catalogStandbys(t, leader) {
		if _, err := sc.Exec(ctx, `SELECT kingres.remove_node($1)`, n.id); err != nil {
			t.Fatalf("removing node %d to recreate the old layout: %v", n.id, err)
		}
	}
	if _, err := sc.Exec(ctx, `UPDATE kingres_cp.standby SET catalog_node_id = NULL`); err != nil {
		t.Fatal(err)
	}
	// Until they are registered again the lists keep the names (they were checked when they went in).
	until(t, 2*time.Minute, "the standbys registered again", func() bool {
		for _, n := range c.Nodes {
			if got := syncNames(t, n); !strings.Contains(got, "kingres_standby_") {
				t.Fatalf("a list lost the name of its standby during the repair: %q", got)
			}
		}
		return allRegistered(recordNodes(t, leader), 3) && len(catalogStandbys(t, leader)) == 3
	})
}
