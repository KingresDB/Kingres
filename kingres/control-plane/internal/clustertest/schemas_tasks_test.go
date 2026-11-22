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

// The schema tasks (phase 11, step 13) where they meet other work and failures: the tables a move cannot copy, the cancel of a move
// before and after its switch, a task that excludes a rebalance (the exclusion of a failover is the trigger test of internal/rebalance), and the tasks resumed after the leader was killed.
// Three servers with real instances; the groups are 0 (the leader, which is the map group), 1 and 2.

func skExec(t *testing.T, c *clustertest.Cluster, db, sql string, args ...any) {
	t.Helper()
	conn := c.Leader().ServicePG(db)
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func skInt(t *testing.T, n *clustertest.Node, db, sql string, args ...any) int64 {
	t.Helper()
	conn := n.ServicePG(db)
	defer func() { _ = conn.Close(context.Background()) }()
	var v int64
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// skMake makes a local schema with a table that has a key and rows, on the leader.
func skMake(t *testing.T, c *clustertest.Cluster, schema string, rows int) {
	t.Helper()
	skExec(t, c, "sk", `CREATE SCHEMA `+schema)
	skExec(t, c, "sk", `CREATE TABLE `+schema+`.t (id bigint PRIMARY KEY, v text)`)
	skExec(t, c, "sk", `INSERT INTO `+schema+`.t SELECT g, repeat('r', 100) FROM generate_series(1, $1) g`, rows)
}

func skDistribution(t *testing.T, n *clustertest.Node, schema string) map[string]any {
	t.Helper()
	d, _ := n.Expect(200, "GET", "/databases/sk/schemas/"+schema, nil)["distribution"].(map[string]any)
	return d
}

func skDistribute(t *testing.T, l *clustertest.Node, schema string) {
	t.Helper()
	l.WaitTaskDone(clustertest.TaskID(l.Expect(202, "PUT", "/databases/sk/schemas/"+schema+"/distribution", nil)), 3*time.Minute)
}

func skDetail(t *testing.T, l *clustertest.Node, id any) string {
	t.Helper()
	d, _ := l.Expect(200, "GET", fmt.Sprintf("/tasks/%v", id), nil)["detail"].(string)
	return d
}

func skNames(p map[string]any) []string {
	var out []string
	for _, e := range anyList(p["errors"]) {
		out = append(out, fmt.Sprint(e.(map[string]any)["field"]))
	}
	return out
}

// skKillAt ends the leader's kingresd at crash point `point` of step `step` of the next task of the kind, restarts it and returns
// the finished task. start makes the request.
func skKillAt(t *testing.T, leader *clustertest.Node, kind, point string, step int, start func() (int, map[string]any)) map[string]any {
	t.Helper()
	leader.SetFaults(fault.Spec{TaskExitPoint: point, TaskExitStep: step})
	st, body := start()
	until(t, 5*time.Minute, "the leader's kingresd to end at "+point+" of step "+fmt.Sprint(step), func() bool {
		s, _ := leader.Do("GET", "/healthz", nil, nil)
		return s != 200
	})
	leader.Kill()
	leader.Heal()
	leader.Start()
	until(t, 3*time.Minute, "the restarted leader accepts a sign-in", func() bool { return leader.Login(clustertest.AdminUser, clustertest.AdminPass) })
	if st != 202 { // the process can end before the answer is written: the task is the newest of its kind
		items := anyList(leader.Expect(200, "GET", "/tasks?kind="+kind+"&limit=1", nil)["items"])
		if len(items) != 1 {
			t.Fatalf("the request answered %d %v and no task of kind %s exists", st, body, kind)
		}
		body = items[0].(map[string]any)
	}
	return leader.WaitTask(clustertest.TaskID(body), 8*time.Minute)
}

func TestSchemaTasksOnACluster(t *testing.T) {
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
	// No standbys: building them restarts instances and rewrites access rules, which is noise for a move that waits for its nodes.
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

	t.Run("a table that a move cannot copy is named, by the move and by the move first of a make local", func(t *testing.T) {
		skExec(t, c, "sk", `CREATE SCHEMA kn`)
		// a primary key does not help a table whose replica identity is NOTHING
		skExec(t, c, "sk", `CREATE TABLE kn.nothing (id bigint PRIMARY KEY, v text)`)
		skExec(t, c, "sk", `ALTER TABLE kn.nothing REPLICA IDENTITY NOTHING`)
		skExec(t, c, "sk", `CREATE TABLE kn.nokey (a int, b text)`)
		skExec(t, c, "sk", `CREATE TABLE kn.fine (id bigint PRIMARY KEY)`)
		skExec(t, c, "sk", `INSERT INTO kn.nothing SELECT g, 'n' FROM generate_series(1, 50) g`)
		skExec(t, c, "sk", `INSERT INTO kn.nokey SELECT g, 'k' FROM generate_series(1, 50) g`)
		skDistribute(t, l, "kn")
		d := l.Expect(200, "GET", "/databases/sk/schemas/kn", nil)
		if got := strings.Join(toStrings(d["tablesWithoutReplicaIdentity"]), ","); got != "nokey,nothing" {
			t.Fatalf("tables without a replica identity: %q", got)
		}
		// 1. a move
		st, p := l.Do("POST", "/databases/sk/schemas/kn/moves", map[string]any{"targetShardGroupId": 1}, nil)
		if st != 422 || strings.Join(skNames(p), ",") != "nokey,nothing" {
			t.Fatalf("move: %d %v", st, p)
		}
		// 2. the move first of a make local is a move: it is refused before a task exists
		st, p = l.Do("DELETE", "/databases/sk/schemas/kn/distribution?shardGroupId=1", nil, nil)
		if st != 422 || strings.Join(skNames(p), ",") != "nokey,nothing" {
			t.Fatalf("make local with a move first: %d %v", st, p)
		}
		if tasks := anyList(l.Expect(200, "GET", "/tasks?kind=schema.undistribute&limit=5", nil)["items"]); len(tasks) != 0 {
			t.Errorf("a refused make local left %d tasks", len(tasks))
		}
		// 3. asked for, the replica identity is set and the move goes through
		body := l.Expect(202, "POST", "/databases/sk/schemas/kn/moves", map[string]any{"targetShardGroupId": 1, "setReplicaIdentityFull": true})
		l.WaitTaskDone(clustertest.TaskID(body), 6*time.Minute)
		if d := skDistribution(t, l, "kn"); d == nil || d["shardGroupId"] != float64(1) {
			t.Fatalf("distribution after the move: %v", d)
		}
		for _, tbl := range []string{"nothing", "nokey"} {
			if skInt(t, c.Nodes[1], "sk", `SELECT count(*) FROM pg_class WHERE oid = $1::regclass AND relreplident = 'f'`, "kn."+tbl) != 1 {
				t.Errorf("kn.%s has no full replica identity on the new owner", tbl)
			}
		}
		if got := skInt(t, c.Nodes[1], "sk", `SELECT count(*) FROM kn.nothing`); got != 50 {
			t.Errorf("rows of kn.nothing on the owner: %d", got)
		}
	})

	t.Run("a move is cancelled before its switch, and the task says the schema is unchanged", func(t *testing.T) {
		skMake(t, c, "cb", 3000)
		skDistribute(t, l, "cb")
		from := skDistribution(t, l, "cb")["shardGroupId"]
		to := 1
		if from == float64(1) {
			to = 2
		}
		reached, release := p10Gate(t, c, "sk", "move_after_catchup")
		defer release()
		body := l.Expect(202, "POST", "/databases/sk/schemas/cb/moves", map[string]any{"targetShardGroupId": to})
		id := clustertest.TaskID(body)
		reached(3 * time.Minute)
		until(t, time.Minute, "the task to show the phase", func() bool { return strings.HasPrefix(skDetail(t, l, id), "catching_up") })
		if tk := l.Expect(200, "GET", fmt.Sprintf("/tasks/%v", id), nil); tk["cancellable"] != true {
			t.Errorf("a move that is catching up is not cancellable: %v", tk)
		}
		skExec(t, c, "sk", `INSERT INTO cb.t VALUES (7000001, 'during')`)
		l.Expect(200, "POST", fmt.Sprintf("/tasks/%v/cancel", id), nil)
		until(t, time.Minute, "the move to be flagged as cancelled", func() bool {
			return skInt(t, l, "sk", `SELECT count(*) FROM kingres.bucket_move WHERE cancel_requested`) > 0
		})
		release()
		task := l.WaitTask(id, 3*time.Minute)
		e, _ := task["error"].(map[string]any)
		if task["state"] != "failed" || e == nil || e["code"] != "cancelled" || !strings.Contains(fmt.Sprint(e["message"]), "unchanged") {
			t.Fatalf("task after the cancel: %v", task)
		}
		d := skDistribution(t, l, "cb")
		if d["state"] != "active" || d["shardGroupId"] != from || d["movingToShardGroupId"] != nil {
			t.Errorf("distribution after the cancel: %v", d)
		}
		if skInt(t, l, "sk", `SELECT count(*) FROM cb.t WHERE id = 7000001`) != 1 {
			t.Errorf("the row written during the held move is gone")
		}
	})

	t.Run("after the switch a move is not cancelled: it is refused, and the task ends done", func(t *testing.T) {
		skMake(t, c, "ca", 3000)
		skDistribute(t, l, "ca")
		from := skDistribution(t, l, "ca")["shardGroupId"]
		to := 1
		if from == float64(1) {
			to = 2
		}
		reached, release := p10Gate(t, c, "sk", "move_after_switch")
		defer release()
		id := clustertest.TaskID(l.Expect(202, "POST", "/databases/sk/schemas/ca/moves", map[string]any{"targetShardGroupId": to}))
		reached(3 * time.Minute)
		for end := time.Now().Add(time.Minute); l.Expect(200, "GET", fmt.Sprintf("/tasks/%v", id), nil)["cancellable"] != false; time.Sleep(500 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("the task never said that it cannot be cancelled: %v\nmoves: %v", l.Expect(200, "GET", fmt.Sprintf("/tasks/%v", id), nil), skMoves(t, l))
			}
		}
		st, b := l.Do("POST", fmt.Sprintf("/tasks/%v/cancel", id), nil, nil)
		if st != 409 || b["code"] != "task_not_cancellable" {
			t.Fatalf("a cancel after the switch: %d %v", st, b)
		}
		release()
		l.WaitTaskDone(id, 3*time.Minute)
		if d := skDistribution(t, l, "ca"); d == nil || d["shardGroupId"] != float64(to) || d["state"] != "active" {
			t.Errorf("distribution: %v", d)
		}
		if got := skInt(t, l, "sk", `SELECT count(*) FROM ca.t`); got != 3000 {
			t.Errorf("rows after the move: %d", got)
		}
	})

	t.Run("a schema move excludes a rebalance while it runs", func(t *testing.T) {
		skMake(t, c, "ex", 3000)
		skDistribute(t, l, "ex")
		from := skDistribution(t, l, "ex")["shardGroupId"]
		to := 1
		if from == float64(1) {
			to = 2
		}
		reached, release := p10Gate(t, c, "sk", "move_after_catchup")
		defer release()
		id := clustertest.TaskID(l.Expect(202, "POST", "/databases/sk/schemas/ex/moves", map[string]any{"targetShardGroupId": to}))
		reached(3 * time.Minute)
		if st, b := l.Do("POST", "/rebalance", nil, nil); st != 409 || b["code"] != "operation_in_progress" {
			t.Errorf("a rebalance while a schema moves: %d %v", st, b)
		}
		// a second schema task of the same kind is another matter (the subject is another schema); a make local of the moving schema is refused
		if st, b := l.Do("DELETE", "/databases/sk/schemas/ex/distribution", nil, nil); st != 409 || b["code"] != "schema_move_in_progress" {
			t.Errorf("make local while it moves: %d %v", st, b)
		}
		release()
		l.WaitTaskDone(id, 3*time.Minute)
		// the refusals leave nothing behind: a rebalance task of the refused kind does not exist
		if items := anyList(l.Expect(200, "GET", "/tasks?kind=rebalance&limit=5", nil)["items"]); len(items) != 0 {
			t.Errorf("a refused rebalance left %d tasks", len(items))
		}
	})

	t.Run("a move that is running when the leader is killed is followed after the restart", func(t *testing.T) {
		skMake(t, c, "kl", 20000)
		skDistribute(t, l, "kl")
		from := skDistribution(t, l, "kl")["shardGroupId"]
		to := 1
		if from == float64(1) {
			to = 2
		}
		reached, release := p10Gate(t, c, "sk", "move_after_catchup")
		defer release()
		id := clustertest.TaskID(l.Expect(202, "POST", "/databases/sk/schemas/kl/moves", map[string]any{"targetShardGroupId": to}))
		reached(3 * time.Minute)
		until(t, time.Minute, "the task to show the phase", func() bool { return strings.HasPrefix(skDetail(t, l, id), "catching_up") })
		before := l.Expect(200, "GET", fmt.Sprintf("/tasks/%v", id), nil)["updatedAt"]
		l.Kill()
		l.Start()
		until(t, 3*time.Minute, "the restarted leader accepts a sign-in", func() bool { return l.Login(clustertest.AdminUser, clustertest.AdminPass) })
		// The move session of the extension is still held at its fault point. It is let go only after the new run of the step has met
		// the move that the dead leader left (it records its phase again, which moves updatedAt): a move that had ended by then would
		// be found on its target, and the test would not tell a run that follows the move from one that starts it again.
		until(t, 3*time.Minute, "the new run of the step to follow the move", func() bool {
			return l.Expect(200, "GET", fmt.Sprintf("/tasks/%v", id), nil)["updatedAt"] != before
		})
		release()
		task := l.WaitTask(id, 10*time.Minute)
		if task["state"] != "done" {
			t.Fatalf("the task after the restart: %v %v\n%s", task["state"], task["error"], l.LogTail(2500))
		}
		if d := skDistribution(t, l, "kl"); d == nil || d["shardGroupId"] != float64(to) || d["state"] != "active" {
			t.Errorf("distribution = %v", d)
		}
		if got := skInt(t, l, "sk", `SELECT count(*) FROM kl.t`); got != 20000 {
			t.Errorf("rows after the move: %d", got)
		}
	})

	t.Run("the step that distributes is not run twice after the leader was killed in it", func(t *testing.T) {
		for _, pt := range []struct {
			name  string
			point string
		}{{"before the effect", "step_started"}, {"after the effect", "step_ran"}} {
			schema := "rd_" + strings.ReplaceAll(pt.name, " ", "_")
			t.Run(pt.name, func(t *testing.T) {
				skMake(t, c, schema, 200)
				task := skKillAt(t, l, "schema.distribute", pt.point, 2, func() (int, map[string]any) {
					return l.Do("PUT", "/databases/sk/schemas/"+schema+"/distribution", nil, nil)
				})
				if task["state"] != "done" {
					t.Fatalf("task after the takeover: %v %v\n%s", task["state"], task["error"], l.LogTail(2500))
				}
				if skDistribution(t, l, schema) == nil {
					t.Fatalf("no distribution after the task")
				}
				if got := skInt(t, l, "sk", `SELECT count(*) FROM kingres.dist_schema s JOIN pg_namespace n ON n.oid = s.nsp::oid WHERE n.nspname = $1`, schema); got != 1 {
					t.Errorf("%d rows of dist_schema for %s", got, schema)
				}
				for _, n := range c.Nodes {
					if got := skInt(t, n, "sk", `SELECT count(*) FROM `+schema+`.t`); got != 200 {
						t.Errorf("node %d reads %d rows", n.Index, got)
					}
				}
			})
		}
	})
}

func toStrings(v any) []string {
	var out []string
	for _, e := range anyList(v) {
		out = append(out, fmt.Sprint(e))
	}
	return out
}

// skMoves lists the records of the moves of the database, for the message of a test that times out.
func skMoves(t *testing.T, l *clustertest.Node) string {
	t.Helper()
	conn := l.ServicePG("sk")
	defer func() { _ = conn.Close(context.Background()) }()
	rows, err := conn.Query(context.Background(), `SELECT move_id, colocation_id, phase, coalesce(detail, '') FROM kingres.bucket_move ORDER BY move_id`)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, colo int64
		var phase, detail string
		if err := rows.Scan(&id, &colo, &phase, &detail); err != nil {
			return err.Error()
		}
		out = append(out, fmt.Sprintf("%d/%d %s %q", id, colo, phase, detail))
	}
	return strings.Join(out, "; ")
}
