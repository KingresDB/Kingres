// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// The table lifecycle (contract 0.14.0, increment 9.6): a table that has rows is distributed, rebuilt and made plain again through the REST API, the
// online copy goes on while a writer on another server works, a table that cannot take the change is refused with its blockers, a cancel leaves
// nothing behind, the partitions of a user-partitioned table are created, listed, detached and dropped, and the plain views that use a table are
// made again on the new one with dependentViews recreate. A cluster of three servers; every database object is made through the leader.
// Needs kingres_shard 0.7 with the increments 9.3 to 9.9 in the PostgreSQL installation of the tests.

const tlTimeout = 6 * time.Minute

func tlTask(t *testing.T, n *clustertest.Node, body map[string]any) map[string]any {
	t.Helper()
	return n.WaitTask(clustertest.TaskID(body), tlTimeout)
}

func tlCount(t *testing.T, n *clustertest.Node, q string) int64 {
	t.Helper()
	c := n.ServicePG("tl")
	var v int64
	var err error
	for i := 0; i < 30; i++ { // KS001 can be answered while a switch commits; the statement is repeated
		if err = c.QueryRow(context.Background(), q).Scan(&v); err == nil {
			return v
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s on node %d: %v", q, n.Index, err)
	return 0
}

func tlExec(t *testing.T, n *clustertest.Node, q string, args ...any) {
	t.Helper()
	if _, err := n.ServicePG("tl").Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func tlSame(t *testing.T, c *clustertest.Cluster, q string, want int64) {
	t.Helper()
	for _, n := range c.Nodes {
		if got := tlCount(t, n, q); got != want {
			t.Errorf("node %d: %s = %d, want %d", n.Index, q, got, want)
		}
	}
}

// leftovers counts what an operation that did not finish must not leave: shadow tables, capture logs, unfinished operations.
func tlLeftovers(t *testing.T, c *clustertest.Cluster) int64 {
	var total int64
	for _, n := range c.Nodes {
		total += tlCount(t, n, `SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\_shadow\_%' OR relname LIKE 'tlog\_%'`)
	}
	return total + tlCount(t, c.Leader(), `SELECT count(*) FROM kingres.table_operation WHERE phase NOT IN ('done', 'failed', 'cancelled')`)
}

func TestTableLifecycle(t *testing.T) {
	c := clustertest.FormWith(t, 3, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "tl", "sharding": map[string]any{"enabled": true, "bucketCount": 12}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader := c.Leader()
	tbl := func(name string) string { return "/databases/tl/schemas/public/tables/" + name }

	t.Run("a table with rows is distributed, blocking and online", func(t *testing.T) {
		tlExec(t, leader, `CREATE TABLE blk (id bigint PRIMARY KEY, customer int, note text)`)
		tlExec(t, leader, `INSERT INTO blk SELECT g, g % 17, 'n' FROM generate_series(1, 3000) g`)
		b := leader.Expect(202, "PUT", tbl("blk")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id", "mode": "blocking"})
		if got := tlTask(t, leader, b); got["state"] != "done" {
			t.Fatalf("blocking: %v", got)
		}
		tlSame(t, c, `SELECT count(*) FROM blk`, 3000)
		// online, with a writer on another server
		tlExec(t, leader, `CREATE TABLE onl (id bigint PRIMARY KEY, customer int, v int)`)
		tlExec(t, leader, `INSERT INTO onl SELECT g, g % 17, 0 FROM generate_series(1, 20000) g`)
		var stop atomic.Bool
		var written atomic.Int64
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := c.Nodes[1].ServicePG("tl")
			id := int64(1000000)
			for !stop.Load() {
				id++
				for try := 0; try < 50; try++ { // the change of the map is answered with a retryable error
					if _, err := w.Exec(context.Background(), `INSERT INTO onl VALUES ($1, 1, 1)`, id); err == nil {
						written.Add(1)
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		b = leader.Expect(202, "PUT", tbl("onl")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id", "mode": "online"})
		got := tlTask(t, leader, b)
		stop.Store(true)
		wg.Wait()
		if got["state"] != "done" {
			t.Fatalf("online: %v", got)
		}
		names := []string{}
		prog, _ := got["progress"].(map[string]any)
		steps, _ := prog["steps"].([]any)
		for _, s := range steps {
			names = append(names, s.(map[string]any)["name"].(string))
		}
		if !strings.Contains(strings.Join(names, "|"), "Copy rows") || !strings.Contains(strings.Join(names, "|"), "Switch") {
			t.Errorf("an online task has the steps of the online copy, got %v", names)
		}
		tlSame(t, c, `SELECT count(*) FROM onl`, 20000+written.Load())
		if n := tlLeftovers(t, c); n != 0 {
			t.Errorf("%d leftovers", n)
		}
	})

	t.Run("a table that cannot take the change is refused with its blockers", func(t *testing.T) {
		tlExec(t, leader, `CREATE TABLE trg (id bigint PRIMARY KEY, v int)`)
		tlExec(t, leader, `CREATE FUNCTION trg_fn() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`)
		tlExec(t, leader, `CREATE TRIGGER trg_t BEFORE INSERT ON trg FOR EACH ROW EXECUTE FUNCTION trg_fn()`)
		st, p := leader.Do("PUT", tbl("trg")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id"}, nil)
		if st != 422 || p["code"] != "table_not_eligible" {
			t.Fatalf("%d %v", st, p)
		}
		bl, _ := p["blockers"].([]any)
		if len(bl) == 0 {
			t.Errorf("no blockers: %v", p)
		}
		st, pv := leader.Do("POST", tbl("trg")+"/distribution/preview", map[string]any{"kind": "distribute", "distribute": map[string]any{"method": "hash", "distributionColumn": "id"}}, nil)
		if st != 200 {
			t.Fatalf("preview: %d %v", st, pv)
		}
		if b, _ := pv["blockers"].([]any); len(b) == 0 {
			t.Errorf("the preview says the table is eligible: %v", pv)
		}
		if c := tlCount(t, leader, `SELECT count(*) FROM kingres.dist_table WHERE relid = 'trg'::regclass`); c != 0 {
			t.Errorf("a refused table was distributed")
		}
	})

	t.Run("alter and undistribute, online and blocking", func(t *testing.T) {
		b := leader.Expect(202, "PATCH", tbl("onl")+"/distribution", map[string]any{"bucketCount": 24, "mode": "online"})
		if got := tlTask(t, leader, b); got["state"] != "done" {
			t.Fatalf("alter online: %v", got)
		}
		if n := tlCount(t, leader, `SELECT bucket_count FROM kingres.colocation WHERE colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'onl'::regclass)`); n != 24 {
			t.Errorf("bucket count %d", n)
		}
		b = leader.Expect(202, "PATCH", tbl("blk")+"/distribution", map[string]any{"bucketCount": 6, "mode": "blocking"})
		if got := tlTask(t, leader, b); got["state"] != "done" {
			t.Fatalf("alter blocking: %v", got)
		}
		st, p := leader.Do("PATCH", tbl("blk")+"/distribution", map[string]any{}, nil)
		if st != 400 {
			t.Errorf("an alter that changes nothing: %d %v", st, p)
		}
		for _, name := range []string{"onl", "blk"} {
			before := tlCount(t, leader, `SELECT count(*) FROM `+name)
			b = leader.Expect(202, "DELETE", tbl(name)+"/distribution?mode=blocking", nil)
			if got := tlTask(t, leader, b); got["state"] != "done" {
				t.Fatalf("undistribute %s: %v", name, got)
			}
			if after := tlCount(t, leader, `SELECT count(*) FROM `+name); after != before {
				t.Errorf("%s: %d rows before, %d after", name, before, after)
			}
			if n := tlCount(t, leader, `SELECT count(*) FROM kingres.dist_table WHERE relid = '`+name+`'::regclass`); n != 0 {
				t.Errorf("%s is still in the catalog", name)
			}
		}
	})

	t.Run("a cancel leaves nothing behind", func(t *testing.T) {
		tlExec(t, leader, `CREATE TABLE cnc (id bigint PRIMARY KEY, v int)`)
		tlExec(t, leader, `INSERT INTO cnc SELECT g, 0 FROM generate_series(1, 20000) g`)
		b := leader.Expect(202, "PUT", tbl("cnc")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id", "mode": "online"})
		id := clustertest.TaskID(b)
		time.Sleep(300 * time.Millisecond)
		st, _ := leader.Do("POST", fmt.Sprintf("/tasks/%v/cancel", id), nil, nil)
		got := leader.WaitTask(id, tlTimeout)
		switch {
		case st == 409 && got["state"] == "done":
			// finished before the cancel: nothing to undo
		case got["state"] == "failed":
			if e, _ := got["error"].(map[string]any); e == nil || e["code"] != "cancelled" {
				t.Errorf("a cancelled task ends with code cancelled: %v", got["error"])
			}
			if n := tlLeftovers(t, c); n != 0 {
				t.Errorf("%d leftovers after a cancel", n)
			}
			if n := tlCount(t, leader, `SELECT count(*) FROM kingres.dist_table WHERE relid = 'cnc'::regclass`); n != 0 {
				t.Errorf("a cancelled table is distributed")
			}
		case got["state"] == "done":
		default:
			t.Errorf("cancel: status %d, task %v", st, got)
		}
		if n := tlCount(t, leader, `SELECT count(*) FROM cnc`); n != 20000 {
			t.Errorf("rows after the cancel: %d", n)
		}
	})

	t.Run("the switch waits for a long transaction and shows why", func(t *testing.T) {
		tlExec(t, leader, `CREATE TABLE hld (id bigint PRIMARY KEY, v int)`)
		tlExec(t, leader, `INSERT INTO hld SELECT g, 0 FROM generate_series(1, 1000) g`)
		holder := leader.ServicePG("tl")
		ctx := context.Background()
		if _, err := holder.Exec(ctx, `BEGIN`); err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(ctx, `LOCK TABLE hld IN ROW EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		b := leader.Expect(202, "PUT", tbl("hld")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id", "mode": "online"})
		id := clustertest.TaskID(b)
		time.Sleep(25 * time.Second)
		_, cur := leader.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if cur["state"] == "done" {
			t.Errorf("the task finished while a transaction held the table: %v", cur)
		}
		if _, err := holder.Exec(ctx, `ROLLBACK`); err != nil {
			t.Fatal(err)
		}
		if got := leader.WaitTask(id, tlTimeout); got["state"] != "done" {
			t.Fatalf("after the transaction ended: %v", got)
		}
		tlSame(t, c, `SELECT count(*) FROM hld`, 1000)
	})

	t.Run("the partitions of a user-partitioned table", func(t *testing.T) {
		tlExec(t, leader, `CREATE TABLE ev (id bigint NOT NULL, at date NOT NULL, v int, PRIMARY KEY (id, at)) PARTITION BY RANGE (at)`)
		tlExec(t, leader, `CREATE TABLE ev_a PARTITION OF ev FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`)
		tlExec(t, leader, `INSERT INTO ev SELECT g, date '2026-01-01' + (g % 28), 1 FROM generate_series(1, 500) g`)
		b := leader.Expect(202, "PUT", tbl("ev")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id", "mode": "blocking"})
		if got := tlTask(t, leader, b); got["state"] != "done" {
			t.Fatalf("distribute: %v", got)
		}
		b = leader.Expect(202, "POST", tbl("ev")+"/partitions", map[string]any{"name": "ev_b", "bound": "FROM ('2026-02-01') TO ('2026-03-01')"})
		if got := tlTask(t, leader, b); got["state"] != "done" {
			t.Fatalf("create: %v", got)
		}
		st, p := leader.Do("POST", tbl("ev")+"/partitions", map[string]any{"name": "ev_b", "bound": "FROM ('2026-02-01') TO ('2026-03-01')"}, nil)
		if st != 409 || p["code"] != "already_exists" {
			t.Errorf("a partition that exists: %d %v", st, p)
		}
		list := leader.Expect(200, "GET", tbl("ev")+"/partitions", nil)
		if items, _ := list["items"].([]any); len(items) != 2 {
			t.Errorf("partitions: %v", list)
		}
		b = leader.Expect(202, "POST", tbl("ev")+"/partitions/ev_b/detach", nil)
		if got := tlTask(t, leader, b); got["state"] != "done" {
			t.Fatalf("detach: %v", got)
		}
		if n := tlCount(t, leader, `SELECT count(*) FROM kingres.dist_table WHERE relid = 'ev_b'::regclass`); n != 1 {
			t.Errorf("the detached partition is a distributed table: %d", n)
		}
		b = leader.Expect(202, "POST", tbl("ev")+"/partitions/attach", map[string]any{"table": map[string]any{"schema": "public", "table": "ev_b"}, "bound": "FROM ('2026-02-01') TO ('2026-03-01')"})
		if got := tlTask(t, leader, b); got["state"] != "done" {
			t.Fatalf("attach: %v", got)
		}
		b = leader.Expect(202, "DELETE", tbl("ev")+"/partitions/ev_b", nil)
		if got := tlTask(t, leader, b); got["state"] != "done" {
			t.Fatalf("drop: %v", got)
		}
		if st, _ := leader.Do("DELETE", tbl("ev")+"/partitions/ev_b", nil, nil); st != 404 {
			t.Errorf("drop of a partition that is gone: %d", st)
		}
		tlSame(t, c, `SELECT count(*) FROM ev`, 500)
	})

	t.Run("dependent views", func(t *testing.T) {
		tlExec(t, leader, `CREATE TABLE vw (id bigint PRIMARY KEY, v int)`)
		tlExec(t, leader, `INSERT INTO vw SELECT g, g FROM generate_series(1, 100) g`)
		tlExec(t, leader, `CREATE VIEW vw_v AS SELECT id, v FROM vw WHERE v > 10`)
		tlExec(t, leader, `COMMENT ON VIEW vw_v IS 'a view'`)
		st, p := leader.Do("PUT", tbl("vw")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id"}, nil)
		if st != 422 || p["code"] != "table_not_eligible" {
			t.Fatalf("the default refuses: %d %v", st, p)
		}
		b := leader.Expect(202, "PUT", tbl("vw")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id", "mode": "blocking", "dependentViews": "recreate"})
		if got := tlTask(t, leader, b); got["state"] != "done" {
			t.Fatalf("recreate: %v", got)
		}
		if n := tlCount(t, leader, `SELECT count(*) FROM vw_v`); n != 90 {
			t.Errorf("the view answers %d rows, want 90", n)
		}
		if n := tlCount(t, leader, `SELECT count(*) FROM pg_description WHERE objoid = 'vw_v'::regclass AND description = 'a view'`); n != 1 {
			t.Errorf("the comment of the view is gone")
		}
	})
}
