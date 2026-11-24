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
)

// Phase 11, design 9.9, on a real cluster of three servers (extension 0.9): a drain moves the distributed schemas of the server with its buckets and ends only
// when none is left on it, a schema that cannot move refuses the drain before a task exists, and a rebalance by disk size moves a schema as an item of its plan.

// sdOwner is the server and the shard group that hold the distributed schema.
func sdOwner(t *testing.T, n *clustertest.Node, schema string) (string, float64) {
	t.Helper()
	d := skDistribution(t, n, schema)
	if d == nil {
		t.Fatalf("schema %s is not distributed", schema)
	}
	host, _ := d["serverId"].(string)
	g, _ := d["shardGroupId"].(float64)
	return host, g
}

// sdSchemaMoves are the moves of a plan or of a report that are distributed schemas, by name.
func sdSchemaMoves(moves any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, m := range anyList(moves) {
		mm := m.(map[string]any)
		if mm["kind"] == "schema" {
			out[fmt.Sprint(mm["schemaName"])] = mm
		}
	}
	return out
}

func TestDrainOfAServerThatHostsADistributedSchema(t *testing.T) {
	const rows = 2000
	c := clustertest.FormWithExtra(t, 3, []string{"-rebalance-retry-backoff", "1s"}, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "sk", "sharding": map[string]any{"enabled": true, "mode": "both", "bucketCount": 8}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		sc := leader.ServicePG("sk")
		if _, err := sc.Exec(context.Background(), `CREATE TABLE items (id bigint PRIMARY KEY, val text)`); err != nil {
			t.Fatal(err)
		}
		body = leader.Expect(202, "PUT", "/databases/sk/schemas/public/tables/items/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
		leader.WaitTaskDone(clustertest.TaskID(body), 2*time.Minute)
		if _, err := sc.Exec(context.Background(), `INSERT INTO items SELECT g, 'row' FROM generate_series(1, 2000) g`); err != nil {
			t.Fatal(err)
		}
	})
	l := c.Leader()
	// No standbys: building them restarts instances, which is noise for what this test looks at.
	l.Expect(200, "PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 0})
	until(t, 3*time.Minute, "no standby task to run", func() bool {
		for _, st := range []string{"pending", "running"} {
			if len(anyList(l.Expect(200, "GET", "/tasks?kind=standby.reconcile&state="+st, nil)["items"])) > 0 {
				return false
			}
		}
		return true
	})
	p10Cleanup(t, c, "3s")
	victim := c.Nodes[2]

	// the buckets of the hash table are spread over the servers first, before there is a schema to be moved by it
	rep := waitReport(t, l, clustertest.TaskID(l.Expect(202, "POST", "/rebalance", nil)), 10*time.Minute)
	if rep["outcome"] != "completed" {
		t.Fatalf("the first rebalance: %v", rep)
	}
	until(t, 2*time.Minute, "the first rebalance to have ended", func() bool { return rbStatus(l)["state"] == "idle" })
	if primaryBuckets(t, l)[victim.Host] == 0 {
		t.Fatalf("the victim owns no bucket: %v", primaryBuckets(t, l))
	}
	var victimGroup int
	if err := l.ServicePG("sk").QueryRow(context.Background(), `SELECT group_id FROM kingres.node WHERE host = $1 AND role = 'primary' AND is_active`, victim.Host).Scan(&victimGroup); err != nil {
		t.Fatal(err)
	}

	// two schemas on the victim's group, and a third with a table that no move can copy
	for _, s := range []string{"dr_a", "dr_b"} {
		sdLocal(t, victim, `CREATE SCHEMA `+s)
		sdLocal(t, victim, `CREATE TABLE `+s+`.t (id bigint PRIMARY KEY, v text)`)
		sdLocal(t, victim, `INSERT INTO `+s+`.t SELECT g, repeat('r', 100) FROM generate_series(1, $1) g`, rows)
		l.WaitTaskDone(clustertest.TaskID(l.Expect(202, "PUT", "/databases/sk/schemas/"+s+"/distribution", map[string]any{"shardGroupId": victimGroup})), 3*time.Minute)
		if host, _ := sdOwner(t, l, s); host != victim.Host {
			t.Fatalf("schema %s is on %s, want %s", s, host, victim.Host)
		}
	}
	sdLocal(t, victim, `CREATE SCHEMA dr_c`)
	sdLocal(t, victim, `CREATE TABLE dr_c.t (a int, b text)`)
	sdLocal(t, victim, `INSERT INTO dr_c.t SELECT g, 'k' FROM generate_series(1, 50) g`)
	l.WaitTaskDone(clustertest.TaskID(l.Expect(202, "PUT", "/databases/sk/schemas/dr_c/distribution", map[string]any{"shardGroupId": victimGroup})), 3*time.Minute)

	t.Run("a schema that cannot be moved is a blocker of the preview and refuses the drain before a task exists", func(t *testing.T) {
		pv := l.Expect(200, "GET", "/servers/"+victim.Host+"/drain-preview", nil)
		if pv["canDrain"] != false {
			t.Fatalf("preview: %v", pv)
		}
		var named bool
		for _, b := range anyList(pv["blockers"]) {
			m := b.(map[string]any)
			if m["code"] == "unprocessable" && strings.Contains(fmt.Sprint(m["detail"]), "dr_c") {
				named = true
			}
		}
		if !named {
			t.Fatalf("no blocker names the schema: %v", pv["blockers"])
		}
		st, p := l.Do("POST", "/servers/"+victim.Host+"/drain", map[string]any{}, nil)
		if st != 422 || p["code"] != "unprocessable" || !strings.Contains(fmt.Sprint(p["detail"]), "dr_c") {
			t.Fatalf("the drain was not refused: %d %v", st, p)
		}
		if tasks := anyList(l.Expect(200, "GET", "/tasks?kind=rebalance&limit=1&state=running", nil)["items"]); len(tasks) != 0 {
			t.Errorf("a refused drain left a task: %v", tasks)
		}
		if got := l.Servers()[victim.Host]["shouldHaveShards"]; got != true {
			t.Errorf("a refused drain changed the flag of the server: %v", got)
		}
		// the operator says what the extension needs for a copy; the drain is then possible
		skExec(t, c, "sk", `ALTER TABLE dr_c.t REPLICA IDENTITY FULL`)
		pv = l.Expect(200, "GET", "/servers/"+victim.Host+"/drain-preview", nil)
		if pv["canDrain"] != true {
			t.Fatalf("preview after the identity was set: %v", pv)
		}
	})

	t.Run("the preview lists the schemas as items of the plan", func(t *testing.T) {
		pv := l.Expect(200, "GET", "/servers/"+victim.Host+"/drain-preview", nil)
		got := sdSchemaMoves(pv["moves"])
		for _, s := range []string{"dr_a", "dr_b", "dr_c"} {
			m := got[s]
			if m == nil || m["reason"] != "drain" || m["fromServerId"] != victim.Host {
				t.Errorf("schema %s in the plan: %v", s, m)
			}
		}
		for _, m := range anyList(pv["moves"]) {
			if mm := m.(map[string]any); mm["kind"] != "schema" && mm["schemaName"] != nil {
				t.Errorf("a bucket names a schema: %v", mm)
			}
		}
	})

	var id any
	t.Run("a drain moves the schemas with the buckets, and ends with the server holding neither", func(t *testing.T) {
		id = clustertest.TaskID(l.Expect(202, "POST", "/servers/"+victim.Host+"/drain", map[string]any{}))
		l.WaitTaskDone(id, 20*time.Minute)
		for _, s := range []string{"dr_a", "dr_b", "dr_c"} {
			host, g := sdOwner(t, l, s)
			if host == victim.Host || host == "" || g == float64(victimGroup) {
				t.Errorf("schema %s is still on the drained server: %s group %v", s, host, g)
			}
		}
		for s, want := range map[string]int64{"dr_a": rows, "dr_b": rows} {
			if got := skInt(t, l, "sk", `SELECT count(*) FROM `+s+`.t`); got != want {
				t.Errorf("rows of %s.t after the drain: %d, want %d", s, got, want)
			}
		}
		if got := skInt(t, l, "sk", `SELECT count(*) FROM dr_c.t`); got != 50 {
			t.Errorf("rows of dr_c.t after the drain: %d", got)
		}
		// the server owns no bucket, no schema, and no move away from it is waiting to be cleaned
		if got := skInt(t, l, "sk", `SELECT count(*) FROM kingres.bucket_map m JOIN kingres.node n ON n.group_id = m.group_id AND n.role = 'primary' AND n.is_active AND n.host = $1`, victim.Host); got != 0 {
			t.Errorf("the drained server owns %d buckets or schemas", got)
		}
		if got := skInt(t, l, "sk", `SELECT count(*) FROM kingres.bucket_move WHERE phase NOT IN ('done', 'failed', 'cancelled')`); got != 0 {
			t.Errorf("%d moves are not finished", got)
		}
		r := l.Expect(200, "GET", fmt.Sprintf("/rebalance/reports/%v", id), nil)
		if r["trigger"] != "drain" || r["outcome"] != "completed" {
			t.Fatalf("report: %v", r)
		}
		moved := sdSchemaMoves(r["moves"])
		for _, s := range []string{"dr_a", "dr_b", "dr_c"} {
			m := moved[s]
			if m == nil || m["outcome"] != "done" || m["reason"] != "drain" || m["fromServerId"] != victim.Host {
				t.Errorf("report entry of schema %s: %v", s, m)
			}
		}
		for _, m := range anyList(r["moves"]) {
			if mm := m.(map[string]any); mm["kind"] != "schema" && (mm["kind"] != nil || mm["schemaName"] != nil) {
				t.Errorf("a bucket carries the members of a schema: %v", mm)
			}
		}
		if cc := l.Expect(200, "GET", "/databases/sk/schemas/dr_a", nil); cc["distribution"] == nil {
			t.Errorf("dr_a lost its distribution: %v", cc)
		}
	})

	t.Run("a rebalance by disk size moves a schema as an item of its plan", func(t *testing.T) {
		l.Expect(200, "PATCH", "/servers/"+victim.Host, map[string]any{"shouldHaveShards": true})
		// larger than the least size a bucket counts (1 MiB), so that a schema is the greatest item of the plan
		for _, s := range []string{"dr_a", "dr_b"} {
			skExec(t, c, "sk", `INSERT INTO `+s+`.t SELECT g, repeat('r', 100) FROM generate_series($1::int, 60000) g`, rows+1)
		}
		plan := l.Expect(200, "POST", "/rebalance/plan", map[string]any{"strategy": "byDiskSize", "moveLimit": 500})
		if plan["needed"] != true {
			t.Fatalf("plan: %v", plan)
		}
		planned := sdSchemaMoves(plan["moves"])
		if len(planned) == 0 {
			t.Fatalf("no schema among the items of the plan: %v", plan["moves"])
		}
		for s, m := range planned {
			if s != "dr_a" && s != "dr_b" {
				t.Errorf("an unexpected schema in the plan: %v", m)
			}
			if m["reason"] != "balance" {
				t.Errorf("schema %s in the plan: %v", s, m)
			}
		}
		before := map[string]string{}
		for s := range planned {
			before[s], _ = sdOwner(t, l, s)
		}
		rb := l.Expect(202, "POST", "/rebalance", map[string]any{"strategy": "byDiskSize"})
		rid := clustertest.TaskID(rb)
		rep := waitReport(t, l, rid, 20*time.Minute)
		if rep["outcome"] != "completed" {
			t.Fatalf("the rebalance: %v", rep)
		}
		done := 0
		for s, m := range sdSchemaMoves(rep["moves"]) {
			if m["outcome"] != "done" {
				t.Errorf("schema %s: %v", s, m)
				continue
			}
			done++
			if host, _ := sdOwner(t, l, s); host == before[s] {
				t.Errorf("schema %s did not leave %s", s, host)
			}
		}
		if done == 0 {
			t.Fatalf("no schema was moved by the rebalance: %v", rep["moves"])
		}
		for _, s := range []string{"dr_a", "dr_b"} {
			if got := skInt(t, l, "sk", `SELECT count(*) FROM `+s+`.t`); got != 60000 {
				t.Errorf("rows of %s.t after the rebalance: %d", s, got)
			}
		}
	})
}

// sdLocal runs a statement on the given server, where a local schema has to be made: the rows of a schema decide the shard group it is distributed to,
// so a schema meant for the group of the victim is made on the victim and not on the leader.
func sdLocal(t *testing.T, n *clustertest.Node, sql string, args ...any) {
	t.Helper()
	conn := n.ServicePG("sk")
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
