// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// pg_stat_statements and plpgsql on a cluster of three servers. Every instance preloads pg_stat_statements; the databases that kingresd makes
// (postgres, kingres_system and the one of the test) carry the extension on every server, also on the servers that were added after the database was
// made; the view answers on every server after a few statements, with the counts of that server. plpgsql is in every database, a function written
// in it is a local object (kingres_shard does not propagate CREATE FUNCTION: each server needs its own copy) and works against a distributed table
// from every server: in a filter, as a function that reads, and as a function that writes.

func psScalar(t *testing.T, n *clustertest.Node, db, q string) string {
	t.Helper()
	var v *string
	for i := 0; ; i++ { // KS001 can be answered while the map changes; the statement is repeated
		err := n.ServicePG(db).QueryRow(context.Background(), "SELECT ("+q+")::text").Scan(&v)
		if err == nil {
			if v == nil {
				return ""
			}
			return *v
		}
		if i >= 30 {
			t.Fatalf("node %d: %s: %v", n.Index, q, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func psExec(t *testing.T, n *clustertest.Node, db, q string) {
	t.Helper()
	if _, err := n.ServicePG(db).Exec(context.Background(), q); err != nil {
		t.Fatalf("node %d: %s: %v", n.Index, q, err)
	}
}

func TestStatStatementsAndPlpgsql(t *testing.T) {
	c := clustertest.FormWith(t, 3, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "ps", "sharding": map[string]any{"enabled": true, "bucketCount": 12}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader := c.Leader()

	t.Run("every instance preloads the library and every database has the extension, on every server", func(t *testing.T) {
		for _, n := range c.Nodes {
			if got := psScalar(t, n, "postgres", `current_setting('shared_preload_libraries')`); !strings.Contains(","+got+",", ",pg_stat_statements,") {
				t.Errorf("node %d: shared_preload_libraries %q", n.Index, got)
			}
			for _, db := range []string{"postgres", "kingres_system", "ps"} {
				if got := psScalar(t, n, db, `SELECT count(*) FROM pg_extension WHERE extname = 'pg_stat_statements'`); got != "1" {
					t.Errorf("node %d: database %s has pg_stat_statements %s times", n.Index, db, got)
				}
				if got := psScalar(t, n, db, `SELECT count(*) FROM pg_extension WHERE extname = 'plpgsql'`); got != "1" {
					t.Errorf("node %d: database %s has plpgsql %s times", n.Index, db, got)
				}
			}
			// the library is loaded, not only listed: the query id is computed
			if got := psScalar(t, n, "ps", `current_setting('compute_query_id')`); got != "auto" && got != "on" {
				t.Errorf("node %d: compute_query_id %q", n.Index, got)
			}
		}
	})

	t.Run("the view of every server counts the statements that ran on that server", func(t *testing.T) {
		for _, n := range c.Nodes {
			psExec(t, n, "ps", `SELECT pg_stat_statements_reset()`)
		}
		for i, n := range c.Nodes {
			for j := 0; j <= i; j++ { // node 1 runs the statement once, node 2 twice, node 3 three times
				psExec(t, n, "ps", `SELECT count(*) AS pgss_marker_cnt FROM pg_class`)
			}
		}
		for i, n := range c.Nodes {
			got := psScalar(t, n, "ps", `SELECT coalesce(sum(calls), 0) FROM pg_stat_statements WHERE query LIKE '%pgss_marker_cnt%' AND query NOT LIKE '%pg_stat_statements%'`)
			if want := strconv.Itoa(i + 1); got != want {
				t.Errorf("node %d: calls of the statement %s, want %s", n.Index, got, want)
			}
			if got := psScalar(t, n, "ps", `SELECT dealloc >= 0 AND stats_reset IS NOT NULL FROM pg_stat_statements_info()`); got != "true" {
				t.Errorf("node %d: pg_stat_statements_info %s", n.Index, got)
			}
		}
	})

	t.Run("a plpgsql function is local to its server and works against a distributed table from every server", func(t *testing.T) {
		psExec(t, leader, "ps", `CREATE TABLE facts (id bigint PRIMARY KEY, g int, v int)`)
		psExec(t, leader, "ps", `INSERT INTO facts SELECT i, i % 7, i FROM generate_series(1, 2100) i`)
		b := leader.Expect(202, "PUT", "/databases/ps/schemas/public/tables/facts/distribution", map[string]any{"method": "hash", "distributionColumn": "id", "mode": "blocking"})
		if got := leader.WaitTask(clustertest.TaskID(b), 6*time.Minute); got["state"] != "done" {
			t.Fatalf("distribute: %v\n%s", got, leader.LogTail(3000))
		}
		fns := []string{
			`CREATE FUNCTION public.cnt_g(p int) RETURNS bigint LANGUAGE plpgsql AS $$ BEGIN RETURN (SELECT count(*) FROM public.facts WHERE g = p); END $$`,
			`CREATE FUNCTION public.is_even(x bigint) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$ BEGIN RETURN x % 2 = 0; END $$`,
			`CREATE FUNCTION public.add_row(i bigint) RETURNS void LANGUAGE plpgsql AS $$ BEGIN INSERT INTO public.facts VALUES (i, 0, 0); END $$`,
		}
		// CREATE FUNCTION is not propagated: made on the leader, it is not on the others
		psExec(t, leader, "ps", fns[0])
		for _, n := range c.Nodes[1:] {
			if got := psScalar(t, n, "ps", `SELECT count(*) FROM pg_proc WHERE proname = 'cnt_g'`); got != "0" {
				t.Errorf("node %d: the function of another server is here (%s): CREATE FUNCTION is propagated now, and this test and the guide must say so", n.Index, got)
			}
		}
		for _, n := range c.Nodes[1:] {
			psExec(t, n, "ps", fns[0])
		}
		for _, n := range c.Nodes {
			psExec(t, n, "ps", fns[1])
			psExec(t, n, "ps", fns[2])
		}
		// 2100 rows, g = i % 7: 300 rows each; 1050 even ids. The answers are the same from every server.
		for _, n := range c.Nodes {
			if got := psScalar(t, n, "ps", `SELECT public.cnt_g(3)`); got != "300" {
				t.Errorf("node %d: cnt_g(3) = %s", n.Index, got)
			}
			if got := psScalar(t, n, "ps", `SELECT count(*) FROM public.facts WHERE public.is_even(id)`); got != "1050" {
				t.Errorf("node %d: rows with an even id %s", n.Index, got)
			}
			if got := psScalar(t, n, "ps", `SELECT sum(public.cnt_g(g)) FROM (SELECT DISTINCT g FROM public.facts) s`); got != "2100" {
				t.Errorf("node %d: sum of cnt_g over the groups %s", n.Index, got)
			}
		}
		// a function that writes: called on every server, the row lands in the distributed table and every server sees it
		for i, n := range c.Nodes {
			for try := 0; ; try++ {
				_, err := n.ServicePG("ps").Exec(context.Background(), `SELECT public.add_row($1)`, int64(900000+i))
				if err == nil {
					break
				}
				if try >= 30 {
					t.Fatalf("node %d: add_row: %v", n.Index, err)
				}
				time.Sleep(300 * time.Millisecond)
			}
		}
		for _, n := range c.Nodes {
			if got := psScalar(t, n, "ps", `SELECT count(*) FROM public.facts WHERE id >= 900000`); got != "3" {
				t.Errorf("node %d: rows written by the function %s", n.Index, got)
			}
		}
	})

	t.Run("the calls of the function are in the view, with the settings of the instance", func(t *testing.T) {
		for _, n := range c.Nodes {
			if got := psScalar(t, n, "ps", `SELECT count(*) FROM pg_stat_statements WHERE query LIKE '%cnt_g%'`); got == "0" {
				t.Errorf("node %d: no statement of the function in pg_stat_statements", n.Index)
			}
			if got := psScalar(t, n, "ps", `current_setting('pg_stat_statements.track')`); got != "top" {
				t.Errorf("node %d: pg_stat_statements.track %q", n.Index, got)
			}
		}
	})
}

// A server that is added gets every extension of the leader's databases in the schema they have on the leader, the schema made as the leader has it
// (name quoted, same owner), not in the schema of the session (pg_catalog): pgcrypto in a schema of its own, owned by another role.
func TestAddedServerGetsExtensionsInTheirSchema(t *testing.T) {
	c := clustertest.FormWith(t, 3, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "ep"})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		psExec(t, leader, "postgres", `CREATE ROLE ext_owner NOLOGIN`)
		psExec(t, leader, "ep", `CREATE SCHEMA "Ext Schema" AUTHORIZATION ext_owner`)
		psExec(t, leader, "ep", `CREATE EXTENSION pgcrypto SCHEMA "Ext Schema"`)
	})
	for _, n := range c.Nodes {
		if got := psScalar(t, n, "ep", `SELECT n.nspname || ':' || r.rolname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace JOIN pg_roles r ON r.oid = n.nspowner WHERE e.extname = 'pgcrypto'`); got != "Ext Schema:ext_owner" {
			t.Errorf("node %d: pgcrypto is in %q", n.Index, got)
		}
		if got := psScalar(t, n, "ep", `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'Ext Schema' AND p.proname = 'gen_random_bytes'`); got != "1" {
			t.Errorf("node %d: gen_random_bytes in the schema: %s", n.Index, got)
		}
		if got := psScalar(t, n, "ep", `length("Ext Schema".gen_random_bytes(8))`); got != "8" {
			t.Errorf("node %d: gen_random_bytes: %s", n.Index, got)
		}
	}
}
