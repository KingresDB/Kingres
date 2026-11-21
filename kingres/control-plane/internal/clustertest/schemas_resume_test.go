// SPDX-License-Identifier: AGPL-3.0-only

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
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// The tasks of the schemas, resumed after the leader's kingresd was killed at a step boundary (the crash points of the task engine,
// package fault): the task goes on from the step that was not recorded, and a step that must not run twice is not run twice.

// shKillAt ends the leader's kingresd at crash point `point` of step `step` of the next task that runs, starts the request, brings
// the leader back and returns the finished task. The request is made by start (it returns the 202 body).
func shKillAt(t *testing.T, leader *clustertest.Node, kind, point string, step int, start func() (int, map[string]any)) map[string]any {
	t.Helper()
	leader.SetFaults(fault.Spec{TaskExitPoint: point, TaskExitStep: step})
	st, body := start()
	until(t, 5*time.Minute, "the leader's kingresd to end at "+point+" of step "+fmt.Sprint(step), func() bool {
		st, _ := leader.Do("GET", "/healthz", nil, nil)
		return st != 200
	})
	leader.Kill() // reaps the process
	leader.Heal()
	leader.Start()
	until(t, 3*time.Minute, "the restarted leader accepts a sign-in", func() bool { return leader.Login(clustertest.AdminUser, clustertest.AdminPass) })
	// the process can end before the answer is written: the task is then the newest of its kind
	if st != 202 {
		list := leader.Expect(200, "GET", "/tasks?kind="+kind+"&limit=1", nil)
		items := anyList(list["items"])
		if len(items) != 1 {
			t.Fatalf("the request answered %d %v and no task of kind %s exists", st, body, kind)
		}
		body = items[0].(map[string]any)
	}
	return leader.WaitTask(clustertest.TaskID(body), 8*time.Minute)
}

// shMake makes a local schema with a table that has a key, with rows, on the first server.
func shMake(t *testing.T, c *clustertest.Cluster, schema string, rows int) {
	t.Helper()
	lc := c.Leader().ServicePG("sh")
	defer func() { _ = lc.Close(context.Background()) }()
	shExec(t, lc, `CREATE SCHEMA `+schema)
	shExec(t, lc, `CREATE TABLE `+schema+`.t (id bigint PRIMARY KEY, v text)`)
	shExec(t, lc, `INSERT INTO `+schema+`.t SELECT g, repeat('r', 100) FROM generate_series(1, $1) g`, rows)
}

func shDistributeNow(t *testing.T, leader *clustertest.Node, schema string) {
	t.Helper()
	leader.WaitTaskDone(clustertest.TaskID(leader.Expect(202, "PUT", shPath("sh", schema)+"/distribution", nil)), 3*time.Minute)
}

func TestSchemaTasksResumeAfterTheLeaderIsKilled(t *testing.T) {
	c := shCluster(t)
	leader := c.Leader()
	n := 0
	next := func(prefix string) string { n++; return fmt.Sprintf("%s_%d", prefix, n) }

	t.Run("distribute", func(t *testing.T) {
		for _, pt := range []struct {
			point string
			step  int
		}{
			{"step_started", 0}, {"step_started", 1}, {"step_started", 2}, {"step_ran", 2}, {"step_started", 3}, {"step_ran", 3},
		} {
			schema := next("rd")
			t.Run(fmt.Sprintf("%s of step %d", pt.point, pt.step), func(t *testing.T) {
				shMake(t, c, schema, 200)
				task := shKillAt(t, leader, "schema.distribute", pt.point, pt.step, func() (int, map[string]any) {
					return leader.Do("PUT", shPath("sh", schema)+"/distribution", nil, nil)
				})
				if task["state"] != "done" {
					t.Fatalf("task after the takeover: %v %v", task["state"], task["error"])
				}
				d := shDistribution(t, leader, "sh", schema)
				if d == nil {
					t.Fatalf("no distribution after the task")
				}
				// the schema was distributed once: one row of the catalog and the same rows everywhere
				cc := leader.ServicePG("sh")
				defer func() { _ = cc.Close(context.Background()) }()
				if got := shInt(t, cc, `SELECT count(*) FROM kingres.dist_schema s JOIN pg_namespace n ON n.oid = s.nsp::oid WHERE n.nspname = $1`, schema); got != 1 {
					t.Errorf("%d rows of dist_schema for %s", got, schema)
				}
				shRowsEverywhere(t, c, "sh", schema+".t", 200)
				shCheckClean(t, leader, "sh")
				for i, s := range shSteps(task) {
					if !strings.HasSuffix(s, "=done") && !strings.HasSuffix(s, "=skipped") {
						t.Errorf("step %d of the task is %s", i, s)
					}
				}
			})
		}
	})

	t.Run("move", func(t *testing.T) {
		target := int32(1)
		for _, pt := range []struct {
			point string
			step  int
		}{
			{"step_started", 0}, {"step_started", 1}, {"step_started", 2}, {"step_ran", 2}, {"step_started", 3}, {"step_started", 4},
		} {
			schema := next("rm")
			t.Run(fmt.Sprintf("%s of step %d", pt.point, pt.step), func(t *testing.T) {
				shMake(t, c, schema, 5000)
				shDistributeNow(t, leader, schema)
				want := target
				task := shKillAt(t, leader, "schema.move", pt.point, pt.step, func() (int, map[string]any) {
					return leader.Do("POST", shPath("sh", schema)+"/moves", map[string]any{"targetShardGroupId": want}, nil)
				})
				if task["state"] != "done" {
					t.Fatalf("task after the takeover: %v %v\n%s", task["state"], task["error"], leader.LogTail(2500))
				}
				d := shDistribution(t, leader, "sh", schema)
				if d == nil || d["shardGroupId"] != float64(want) || d["state"] != "active" {
					t.Errorf("distribution after the move = %v", d)
				}
				shRowsEverywhere(t, c, "sh", schema+".t", 5000)
			})
		}
	})

	t.Run("a move that is running when the leader is killed is followed after the restart", func(t *testing.T) {
		schema := next("rmid")
		shMake(t, c, schema, 20000)
		shDistributeNow(t, leader, schema)
		gate := filepath.Join(t.TempDir(), "gate")
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		shSetFault(t, c, "sh", "move_after_catchup:wait:"+gate)
		defer shSetFault(t, c, "sh", "")
		body := leader.Expect(202, "POST", shPath("sh", schema)+"/moves", map[string]any{"targetShardGroupId": 2})
		until(t, 3*time.Minute, "the move to be caught up", func() bool {
			det, _ := shTask(t, leader, clustertest.TaskID(body))["detail"].(string)
			return strings.HasPrefix(det, "catching_up")
		})
		leader.Kill()
		leader.Start()
		until(t, 3*time.Minute, "the restarted leader accepts a sign-in", func() bool { return leader.Login(clustertest.AdminUser, clustertest.AdminPass) })
		// the move session of the extension is still held at its fault point; let it go
		_ = os.Remove(gate)
		task := leader.WaitTask(clustertest.TaskID(body), 10*time.Minute)
		if task["state"] != "done" {
			t.Fatalf("the task after the restart: %v %v\n%s", task["state"], task["error"], leader.LogTail(2500))
		}
		if d := shDistribution(t, leader, "sh", schema); d == nil || d["shardGroupId"] != float64(2) {
			t.Errorf("distribution = %v", d)
		}
		shRowsEverywhere(t, c, "sh", schema+".t", 20000)
	})

	t.Run("undistribute", func(t *testing.T) {
		for _, pt := range []struct {
			point string
			step  int
		}{
			{"step_started", 0}, {"step_started", 1}, {"step_started", 2}, {"step_ran", 2}, {"step_started", 3}, {"step_ran", 3},
		} {
			schema := next("ru")
			t.Run(fmt.Sprintf("%s of step %d", pt.point, pt.step), func(t *testing.T) {
				shMake(t, c, schema, 300)
				shDistributeNow(t, leader, schema)
				// move it first, so that the step "Move first" has work
				task := shKillAt(t, leader, "schema.undistribute", pt.point, pt.step, func() (int, map[string]any) {
					return leader.Do("DELETE", shPath("sh", schema)+"/distribution?shardGroupId=1", nil, nil)
				})
				if task["state"] != "done" {
					t.Fatalf("task after the takeover: %v %v\n%s", task["state"], task["error"], leader.LogTail(2500))
				}
				// a local schema on the second server is not the first server's: it is gone from what the leader lists
				if st, b := leader.Do("GET", shPath("sh", schema), nil, nil); st == 200 && b["distribution"] != nil {
					t.Errorf("still distributed: %v", b["distribution"])
				}
				// the schema is on the second server alone, with its rows
				conn := c.Nodes[1].ServicePG("sh")
				defer func() { _ = conn.Close(context.Background()) }()
				if got := shInt(t, conn, `SELECT count(*) FROM `+schema+`.t`); got != 300 {
					t.Errorf("the owner reads %d rows", got)
				}
				for _, other := range []*clustertest.Node{c.Nodes[0], c.Nodes[2]} {
					oc := other.ServicePG("sh")
					if shInt(t, oc, `SELECT count(*) FROM pg_namespace WHERE nspname = $1`, schema) != 0 {
						t.Errorf("node %d still has the schema", other.Index)
					}
					_ = oc.Close(context.Background())
				}
			})
		}
	})
}
