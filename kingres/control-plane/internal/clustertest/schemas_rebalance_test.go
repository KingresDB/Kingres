// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/rebalance"
)

// A rebalance of a database that has hash buckets and a distributed schema: the buckets move, the schema stays where it is (the
// plan of this version moves buckets of hash groups only; the schema counts in the load of its group), a rebalance in flight says
// that its move is one of a hash group, and a schema cannot be moved while the rebalance runs.
func TestRebalanceWithADistributedSchema(t *testing.T) {
	c := clustertest.FormWithExtra(t, 3, []string{"-rebalance-retry-backoff", "1s"}, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "rbs", "sharding": map[string]any{"enabled": true, "mode": "both", "bucketCount": 16}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		sc := leader.ServicePG("rbs")
		shExec(t, sc, `CREATE TABLE items (id bigint PRIMARY KEY, val text)`)
		shExec(t, sc, `CREATE SCHEMA ten`)
		shExec(t, sc, `CREATE TABLE ten.t (id bigint PRIMARY KEY, v text)`)
		shExec(t, sc, `INSERT INTO ten.t SELECT g, 'x' FROM generate_series(1, 1000) g`)
		leader.WaitTaskDone(clustertest.TaskID(leader.Expect(202, "PUT", "/databases/rbs/schemas/public/tables/items/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})), 2*time.Minute)
		shExec(t, sc, `INSERT INTO items SELECT g, 'row' FROM generate_series(1, 500) g`)
	})
	leader := c.Leader()
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 0})
	// a schema is distributed when the servers know each other (the extension asks them)
	leader.WaitTaskDone(clustertest.TaskID(leader.Expect(202, "PUT", shPath("rbs", "ten")+"/distribution", nil)), 3*time.Minute)

	gate := filepath.Join(t.TempDir(), "gate")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	shSetFault(t, c, "rbs", "move_after_catchup:wait:"+gate)
	defer shSetFault(t, c, "rbs", "")
	body := leader.Expect(202, "POST", "/rebalance", nil)
	id := clustertest.TaskID(body)
	until(t, 3*time.Minute, "a bucket move to be held", func() bool { _, err := os.Stat(gate + ".reached"); return err == nil })

	// the move in flight is the move of a bucket of a hash group
	st := rbStatus(leader)
	moves := anyList(st["inFlightMoves"])
	if st["state"] != "running" || len(moves) != 1 {
		t.Fatalf("status with a move in flight: %v", st)
	}
	if m := moves[0].(map[string]any); m["kind"] != "hash" || m["schemaName"] != nil {
		t.Errorf("the move in flight = %v", m)
	}
	// a schema is not moved while a rebalance runs
	stc, b := leader.Do("POST", shPath("rbs", "ten")+"/moves", map[string]any{"targetShardGroupId": 1}, nil)
	shProblem(t, stc, b, 409, "rebalance_in_progress")
	_ = os.Remove(gate)
	rep := waitReport(t, leader, id, 6*time.Minute)
	if rep["outcome"] != "completed" {
		t.Fatalf("report: %v", rep)
	}
	// the schema is where it was, intact, and the rest of the cluster is consistent
	if d := shDistribution(t, leader, "rbs", "ten"); d == nil || d["shardGroupId"] != float64(0) || d["state"] != "active" {
		t.Errorf("distribution of the schema after the rebalance = %v", d)
	}
	shRowsEverywhere(t, c, "rbs", "ten.t", 1000)
	shRowsEverywhere(t, c, "rbs", "items", 500)
	shCheckClean(t, leader, "rbs")
	dst := leader.Expect(200, "GET", "/databases/rbs/bucket-distribution", nil)
	var schemas float64
	for _, s := range anyList(dst["servers"]) {
		schemas += s.(map[string]any)["primarySchemas"].(float64)
	}
	if schemas != 1 {
		t.Errorf("primarySchemas = %v in %v", schemas, dst)
	}
	// the rebalance is over: the schema moves again
	mv := leader.Expect(202, "POST", shPath("rbs", "ten")+"/moves", map[string]any{"targetShardGroupId": 2})
	leader.WaitTaskDone(clustertest.TaskID(mv), 4*time.Minute)
	shRowsEverywhere(t, c, "rbs", "ten.t", 1000)

	// what a status says of a move of a schema: the group of a schema is found by its colocation id
	conn := leader.ServicePG("rbs")
	defer func() { _ = conn.Close(context.Background()) }()
	d := shDistribution(t, leader, "rbs", "ten")
	pg := &rebalance.PG{Conns: nodeConns{leader}}
	name, err := pg.SchemaOf(context.Background(), "rbs", int32(d["colocationId"].(float64)))
	if err != nil || name == nil || *name != "ten" {
		t.Errorf("SchemaOf(schema group) = %v, %v", name, err)
	}
	if name, err := pg.SchemaOf(context.Background(), "rbs", 99999); err != nil || name != nil {
		t.Errorf("SchemaOf(no such group) = %v, %v", name, err)
	}
	hash := fmt.Sprint(shInt(t, conn, `SELECT min(colocation_id) FROM kingres.colocation WHERE kind = 'hash'`))
	var hc int32
	_, _ = fmt.Sscan(hash, &hc)
	if name, err := pg.SchemaOf(context.Background(), "rbs", hc); err != nil || name != nil {
		t.Errorf("SchemaOf(hash group) = %v, %v", name, err)
	}
}

// nodeConns opens the service connections of a node, as the rebalance's Conns.
type nodeConns struct{ n *clustertest.Node }

func (c nodeConns) ServiceConn(_ context.Context, db string) (*pgx.Conn, error) {
	return c.n.ServicePG(db), nil
}
