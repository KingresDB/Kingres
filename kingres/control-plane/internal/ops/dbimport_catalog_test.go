// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

func TestCatalogDifferences(t *testing.T) {
	w := func(f float64) *float64 { return &f }
	n := func(h string, p int32, r string) catalogNode {
		return catalogNode{Host: h, Port: p, Role: r, Active: true, Should: true}
	}
	with := func(c catalogNode, f func(*catalogNode)) catalogNode { f(&c); return c }
	sys := []catalogNode{n("a", 5432, "standby"), n("b", 5432, "primary"), with(n("c", 5432, "primary"), func(c *catalogNode) { c.Weight = w(2) })}
	for _, tc := range []struct {
		name string
		priv []catalogNode
		want []string // substrings, one for each difference, in the order of the (sorted) answer
	}{
		{"agrees", []catalogNode{n("b", 5432, "primary"), with(n("c", 5432, "primary"), func(c *catalogNode) { c.Weight = w(2) })}, nil},
		{"a failover of a group: the old primary is a standby now", []catalogNode{n("a", 5432, "primary"), n("b", 5432, "primary"), with(n("c", 5432, "primary"), func(c *catalogNode) { c.Weight = w(2) })},
			[]string{"names a:5432 as a primary"}},
		{"a node that is gone", []catalogNode{n("z", 5432, "primary"), n("b", 5432, "primary"), with(n("c", 5432, "primary"), func(c *catalogNode) { c.Weight = w(2) })},
			[]string{"names z:5432 as a primary"}},
		{"another port is another node", []catalogNode{n("b", 5433, "primary"), with(n("c", 5432, "primary"), func(c *catalogNode) { c.Weight = w(2) })},
			[]string{"primary b:5432 of the cluster is not in the private catalog", "names b:5433 as a primary"}},
		{"a standby of the private catalog is not compared", []catalogNode{n("a", 5432, "standby"), with(n("a", 5432, "standby"), func(c *catalogNode) { c.Should = false }), n("b", 5432, "primary"), with(n("c", 5432, "primary"), func(c *catalogNode) { c.Weight = w(2) })}, nil},
		// a group that the private catalog lacks
		{"a group is missing", []catalogNode{n("b", 5432, "primary")}, []string{"primary c:5432 of the cluster is not in the private catalog"}},
		{"empty private catalog", nil, []string{"primary b:5432 of the cluster", "primary c:5432 of the cluster"}},
		// the flags of the nodes that hold buckets
		{"a drained server: the flag differs", []catalogNode{n("b", 5432, "primary"), with(n("c", 5432, "primary"), func(c *catalogNode) { c.Weight = w(2); c.Should = false })},
			[]string{"c:5432 has should_have_shards false (the cluster: true)"}},
		{"the weight differs", []catalogNode{n("b", 5432, "primary"), with(n("c", 5432, "primary"), func(c *catalogNode) { c.Weight = w(3) })},
			[]string{"c:5432 has weight 3 (the cluster: 2)"}},
		{"the weight is missing", []catalogNode{n("b", 5432, "primary"), n("c", 5432, "primary")}, []string{"c:5432 has weight none (the cluster: 2)"}},
		{"the weight is not in the cluster", []catalogNode{with(n("b", 5432, "primary"), func(c *catalogNode) { c.Weight = w(1) }), with(n("c", 5432, "primary"), func(c *catalogNode) { c.Weight = w(2) })},
			[]string{"b:5432 has weight 1 (the cluster: none)"}},
		{"is_active differs", []catalogNode{with(n("b", 5432, "primary"), func(c *catalogNode) { c.Active = false }), with(n("c", 5432, "primary"), func(c *catalogNode) { c.Weight = w(2) })},
			[]string{"b:5432 has is_active false (the cluster: true)"}},
	} {
		got := catalogDifferences(tc.priv, sys)
		if len(got) != len(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if !strings.Contains(got[i], tc.want[i]) {
				t.Errorf("%s: %q does not say %q", tc.name, got[i], tc.want[i])
			}
		}
	}
}

// checkImportCatalog and seedImportCatalog on a real instance: the private catalog and the system catalog are tables of the shape of kingres.node.
func TestCheckImportCatalogOnARealInstance(t *testing.T) {
	_, c, o := newGuardInstance(t)
	pgExecAll(t, c, "create database "+sysdb.Database, "create database "+privDB)
	node := "create table kingres.node (node_id int, group_id int, host text, port int, role text, is_active bool default true, should_have_shards bool default true, weight float8)"
	mk := func(db string, rows ...string) {
		d := pgConnect(t, o, db)
		pgExecAll(t, d, "create schema kingres", node)
		for _, r := range rows {
			pgExecAll(t, d, "insert into kingres.node (node_id, group_id, host, port, role) values "+r)
		}
	}
	sys := pgConnect(t, o, sysdb.Database)
	pgExecAll(t, sys, "create schema kingres_cp", "create table kingres_cp.task (kind text, state text)")
	mk(sysdb.Database, "(1, 0, 'h1', 5432, 'standby')", "(2, 0, 'h2', 5432, 'primary')", "(3, 1, 'h3', 5432, 'primary')")
	// the private catalog after a failover of the map group from h1 to h2 that it did not follow
	mk(privDB, "(1, 0, 'h1', 5432, 'primary')", "(2, 1, 'h3', 5432, 'primary')")
	wantLeader := func(what string, err error, inText string) {
		t.Helper()
		var te *transfer.Error
		if !errors.As(err, &te) || te.Code != transfer.CodeLeaderChanged || !strings.Contains(te.Error(), inText) {
			t.Fatalf("%s: %v, want transfer_leader_changed that says %q", what, err, inText)
		}
		if !strings.Contains(te.Error(), "a primary of the cluster changed during the import") && !strings.Contains(te.Error(), "not over") {
			t.Errorf("%s: the message does not say what happened: %v", what, te)
		}
	}
	ctx := context.Background()
	wantLeader("a stale catalog", o.checkImportCatalog(ctx, privDB), "h1:5432")
	// the catalog is as the system catalog says
	d := pgConnect(t, o, privDB)
	pgExecAll(t, d, "update kingres.node set host = 'h2' where node_id = 1")
	if err := o.checkImportCatalog(ctx, privDB); err != nil {
		t.Fatalf("a catalog that agrees: %v", err)
	}
	// a drain or a weight that the private catalog did not get
	pgExecAll(t, sys, "update kingres.node set should_have_shards = false where host = 'h3'")
	wantLeader("a flag", o.checkImportCatalog(ctx, privDB), "should_have_shards")
	pgExecAll(t, sys, "update kingres.node set should_have_shards = true where host = 'h3'", "update kingres.node set weight = 2 where host = 'h2'")
	wantLeader("a weight", o.checkImportCatalog(ctx, privDB), "weight")
	pgExecAll(t, sys, "update kingres.node set weight = null", "update kingres.node set is_active = false where host = 'h3'")
	wantLeader("is_active", o.checkImportCatalog(ctx, privDB), "is_active")
	pgExecAll(t, sys, "update kingres.node set is_active = true")
	if err := o.checkImportCatalog(ctx, privDB); err != nil {
		t.Fatalf("agreeing again: %v", err)
	}
	// a failover that is not over: the standby may be promoted and the system catalog not yet changed, so the comparison cannot be trusted
	for _, st := range []string{"pending", "running", "paused"} {
		pgExecAll(t, sys, "insert into kingres_cp.task values ('server.failover', '"+st+"')")
		wantLeader("a failover "+st, o.checkImportCatalog(ctx, privDB), "not over")
		pgExecAll(t, sys, "delete from kingres_cp.task")
	}
	pgExecAll(t, sys, "insert into kingres_cp.task values ('replication.restorePreferredPrimaries', 'running')")
	wantLeader("a restore of the preferred primaries", o.checkImportCatalog(ctx, privDB), "not over")
	pgExecAll(t, sys, "delete from kingres_cp.task", "insert into kingres_cp.task values ('server.failover', 'done'), ('server.failover', 'failed'), ('rebalance', 'running')")
	if err := o.checkImportCatalog(ctx, privDB); err != nil {
		t.Fatalf("a failover that is over, and a rebalance: %v", err)
	}
	// the system database without a shard catalog is an error of its own, not a reason to call every primary stale
	pgExecAll(t, sys, "alter table kingres.node rename to node_hidden")
	err := o.checkImportCatalog(ctx, privDB)
	var te *transfer.Error
	if !errors.As(err, &te) || te.Code != transfer.CodeVerification || !strings.Contains(te.Error(), "system database has no shard catalog") {
		t.Fatalf("a system database without kingres.node: %v", err)
	}
	pgExecAll(t, sys, "alter table kingres.node_hidden rename to node")
	// a private database that has no catalog (the archive has no sharded tables) is not compared
	pgExecAll(t, d, "drop schema kingres cascade")
	if err := o.checkImportCatalog(ctx, privDB); err != nil {
		t.Fatalf("no catalog: %v", err)
	}
}
