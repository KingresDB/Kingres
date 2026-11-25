// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Distribute with related tables (contract 0.32.0, phase 9) on a cluster of three servers: three ordinary tables that composite foreign keys
// link are planned (`listRelatedTables`), previewed, refused when the list is not whole or one of them cannot take the change (and then nothing is
// changed, the keys stay), and distributed together in one task; afterwards the three tables are in one colocation group, hold their rows on every
// server, have their keys again (valid), enforce them, and `Table.operation` is null for each. Needs kingres_shard 0.7 or later in the installation
// of the tests, as TestTableLifecycle does. Run with `scripts/run-integration.ps1 -Packages ./internal/clustertest -Run TestTableLifecycleRelated`.

func TestTableLifecycleRelated(t *testing.T) {
	c := clustertest.FormWith(t, 3, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "tl", "sharding": map[string]any{"enabled": true, "bucketCount": 12}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader := c.Leader()
	tbl := func(name string) string { return "/databases/tl/schemas/public/tables/" + name }
	tlExec(t, leader, `CREATE TABLE customers (tenant_id int, id int, name text, PRIMARY KEY (tenant_id, id))`)
	tlExec(t, leader, `CREATE TABLE orders (tenant_id int, id int, customer_id int, PRIMARY KEY (tenant_id, id),
		CONSTRAINT orders_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id))`)
	tlExec(t, leader, `CREATE TABLE items (tenant_id int, id int, order_id int, qty int, PRIMARY KEY (tenant_id, id),
		CONSTRAINT items_order_fk FOREIGN KEY (tenant_id, order_id) REFERENCES orders (tenant_id, id))`)
	tlExec(t, leader, `INSERT INTO customers SELECT g % 5, g, 'c' || g FROM generate_series(1, 200) g`)
	tlExec(t, leader, `INSERT INTO orders SELECT c.tenant_id, c.id * 10 + n, c.id FROM customers c, generate_series(1, 5) n`)
	tlExec(t, leader, `INSERT INTO items SELECT o.tenant_id, o.id * 10 + n, o.id, n FROM orders o, generate_series(1, 3) n`)

	t.Run("the plan lists the closure, the columns and the keys", func(t *testing.T) {
		st, plan := leader.Do("GET", tbl("customers")+"/related-tables?distributionColumn=tenant_id", nil, nil)
		if st != 200 {
			t.Fatalf("%d %v", st, plan)
		}
		got := []string{}
		for _, r := range plan["tables"].([]any) {
			m := r.(map[string]any)
			if m["distributionColumn"] != "tenant_id" {
				t.Errorf("column of %v: %v", m["table"], m["distributionColumn"])
			}
			got = append(got, m["table"].(string))
		}
		if strings.Join(got, ",") != "orders,items" {
			t.Errorf("tables %v", got)
		}
		if len(plan["keys"].([]any)) != 2 || len(plan["blockers"].([]any)) != 0 {
			t.Errorf("keys and blockers: %v", plan)
		}
		// the same plan from a follower (the read is answered by the leader)
		if st, _ := c.Nodes[1].Do("GET", tbl("items")+"/related-tables?distributionColumn=tenant_id", nil, nil); st != 200 {
			t.Errorf("follower: %d", st)
		}
	})

	t.Run("a list that is not whole is refused and nothing changes", func(t *testing.T) {
		req := map[string]any{"method": "hash", "distributionColumn": "tenant_id", "relatedTables": []any{map[string]any{"schema": "public", "table": "orders"}}}
		st, p := leader.Do("PUT", tbl("customers")+"/distribution", req, nil)
		if st != 422 || p["code"] != "table_not_eligible" || !strings.Contains(fmt.Sprint(p["blockers"]), "related_table_missing") {
			t.Fatalf("%d %v", st, p)
		}
		st, pre := leader.Do("POST", tbl("customers")+"/distribution/preview", map[string]any{"kind": "distribute", "distribute": req}, nil)
		if st != 200 || !strings.Contains(fmt.Sprint(pre["blockers"]), "related_table_missing") {
			t.Fatalf("preview %d %v", st, pre)
		}
		if n := tlCount(t, leader, `SELECT count(*) FROM pg_constraint WHERE conname IN ('orders_customer_fk', 'items_order_fk')`); n != 2 {
			t.Errorf("%d keys", n)
		}
		if n := tlCount(t, leader, `SELECT count(*) FROM kingres.dist_table WHERE relid::text IN ('customers', 'orders', 'items')`); n != 0 {
			t.Errorf("%d tables distributed", n)
		}
	})

	t.Run("a related table that cannot take the change stops the whole set before a key is dropped", func(t *testing.T) {
		tlExec(t, leader, `CREATE FUNCTION items_fn() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`)
		tlExec(t, leader, `CREATE TRIGGER items_t BEFORE INSERT ON items FOR EACH ROW EXECUTE FUNCTION items_fn()`)
		req := map[string]any{"method": "hash", "distributionColumn": "tenant_id", "relatedTables": []any{
			map[string]any{"schema": "public", "table": "orders"}, map[string]any{"schema": "public", "table": "items"}}}
		st, p := leader.Do("PUT", tbl("customers")+"/distribution", req, nil)
		if st != 422 || p["code"] != "table_not_eligible" || !strings.Contains(fmt.Sprint(p["blockers"]), "trigger") {
			t.Fatalf("%d %v", st, p)
		}
		if n := tlCount(t, leader, `SELECT count(*) FROM pg_constraint WHERE conname IN ('orders_customer_fk', 'items_order_fk')`); n != 2 {
			t.Errorf("%d keys", n)
		}
		tlExec(t, leader, `DROP TRIGGER items_t ON items`)
	})

	t.Run("the three tables are distributed together, colocated, with their keys back", func(t *testing.T) {
		req := map[string]any{"method": "hash", "distributionColumn": "tenant_id", "relatedTables": []any{
			map[string]any{"schema": "public", "table": "orders"}, map[string]any{"schema": "public", "table": "items"}}}
		body := leader.Expect(202, "PUT", tbl("customers")+"/distribution", req)
		id := clustertest.TaskID(body)
		got := leader.WaitTask(id, tlTimeout)
		if got["state"] != "done" {
			t.Fatalf("%v", got)
		}
		names := []string{}
		prog, _ := got["progress"].(map[string]any)
		steps, _ := prog["steps"].([]any)
		for _, s := range steps {
			names = append(names, s.(map[string]any)["name"].(string))
		}
		joined := strings.Join(names, "|")
		for _, want := range []string{"Take the foreign keys between the tables out", "Distribute public.customers", "Distribute public.orders", "Distribute public.items", "Add the foreign keys again", "Validate the foreign keys"} {
			if !strings.Contains(joined, want) {
				t.Errorf("step %q is missing from %v", want, names)
			}
		}
		if n := tlCount(t, leader, `SELECT count(DISTINCT colocation_id) FROM kingres.dist_table WHERE relid::text IN ('customers', 'orders', 'items')`); n != 1 {
			t.Errorf("the tables are in %d colocation groups", n)
		}
		if n := tlCount(t, leader, `SELECT count(*) FROM kingres.dist_table WHERE relid::text IN ('customers', 'orders', 'items') AND method = 'hash'`); n != 3 {
			t.Errorf("%d hash tables", n)
		}
		tlSame(t, c, `SELECT count(*) FROM customers`, 200)
		tlSame(t, c, `SELECT count(*) FROM orders`, 1000)
		tlSame(t, c, `SELECT count(*) FROM items`, 3000)
		if n := tlCount(t, leader, `SELECT count(*) FROM pg_constraint WHERE conrelid IN ('public.orders'::regclass, 'public.items'::regclass) AND conname IN ('orders_customer_fk', 'items_order_fk') AND convalidated`); n != 2 {
			t.Errorf("%d valid keys on the distributed tables after the task", n)
		}
		// the keys are enforced again: an item of an order that does not exist is refused
		if _, err := leader.ServicePG("tl").Exec(context.Background(), `INSERT INTO items VALUES (1, 999999, 424242, 1)`); err == nil {
			t.Error("a row that breaks a foreign key was accepted")
		}
		// the indicator is gone with the task
		for _, name := range []string{"customers", "orders", "items"} {
			tb := leader.Expect(200, "GET", tbl(name), nil)
			if tb["operation"] != nil {
				t.Errorf("%s: operation %v after the task", name, tb["operation"])
			}
		}
		if n, what := tlSettled(t, c); n != 0 {
			t.Errorf("%d leftovers: %s", n, what)
		}
	})

	t.Run("a table that is distributed has no plan", func(t *testing.T) {
		if st, _ := leader.Do("GET", tbl("customers")+"/related-tables", nil, nil); st != 422 {
			t.Errorf("status %d", st)
		}
	})
}
