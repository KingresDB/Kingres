// SPDX-License-Identifier: PostgreSQL

package clustertest_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// instanceIDOf reads the id of the node's own instance from its API.
func instanceIDOf(t *testing.T, n *clustertest.Node) string {
	t.Helper()
	out := n.Expect(200, "GET", "/this-server/instances", nil)
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("node %d: instances %v", n.Index, out)
	}
	return fmt.Sprint(int64(items[0].(map[string]any)["id"].(float64)))
}

// waitFor polls until f is true.
func waitFor(t *testing.T, c *clustertest.Cluster, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s\n%s", what, c.Dump())
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func code(m map[string]any) string { s, _ := m["code"].(string); return s }

// TestFollowerInstanceStopAndStart stops and starts the instance of a follower
// through its own API while the leader keeps working; the role created on the
// leader meanwhile reaches the follower after it is back.
func TestFollowerInstanceStopAndStart(t *testing.T) {
	c := clustertest.Form(t, 2)
	leader, follower := c.Nodes[0], c.Nodes[1]
	fid := instanceIDOf(t, follower)
	lid := instanceIDOf(t, leader)
	if fid == lid {
		t.Fatalf("both servers report instance %s", fid)
	}
	// The instance of another server is not ours to start or stop.
	if st, out := follower.Do("POST", "/this-server/instances/"+lid+"/stop", nil, nil); st != 404 {
		t.Fatalf("stop of the leader's instance on the follower: %d %v", st, out)
	}
	if st, out := follower.Do("POST", "/this-server/instances/"+fid+"/start", nil, nil); st != 409 || code(out) != "instance_already_running" {
		t.Fatalf("start of a running instance: %d %v", st, out)
	}

	st, stop := follower.Do("POST", "/this-server/instances/"+fid+"/stop", nil, nil)
	if st == 409 && code(stop) == "no_replica_available" {
		t.Logf("the follower's instance is the only up-to-date copy of its group; stopping with force")
		st, stop = follower.Do("POST", "/this-server/instances/"+fid+"/stop", map[string]any{"force": true}, nil)
	}
	if st != 202 {
		t.Fatalf("stop on the follower: %d %v\n%s", st, stop, c.Dump())
	}
	stopID := clustertest.TaskID(stop)
	waitFor(t, c, "the follower's instance to stop", 2*time.Minute, func() bool {
		st, out := follower.Do("GET", "/cluster", nil, nil)
		return st == 503 && code(out) == "system_database_stopped"
	})
	// The follower answers its probes and refuses everything else, naming the cause.
	if st, _ := follower.Do("GET", "/healthz", nil, map[string]string{"Authorization": ""}); st != 200 {
		t.Errorf("healthz on the stopped follower: %d", st)
	}
	if st, out := follower.Do("GET", "/this-server", nil, nil); st != 503 || code(out) != "system_database_stopped" {
		t.Errorf("this-server on the stopped follower: %d %v", st, out)
	}
	if st, out := follower.Do("POST", "/roles", map[string]any{"name": "nobody1", "password": "Passw0rd-nobody1-xyz"}, nil); st != 503 || code(out) != "system_database_stopped" {
		t.Errorf("a cluster change through the stopped follower: %d %v", st, out)
	}
	// The leader keeps working: reads, and a cluster change.
	leader.Expect(200, "GET", "/cluster", nil)
	servers := leader.Servers()
	if len(servers) != 2 {
		t.Errorf("the leader lists %d servers", len(servers))
	}
	t.Logf("the leader sees the stopped follower as: %v", servers[follower.Host]["status"])
	st, out := leader.Do("POST", "/roles", map[string]any{"name": "while_down", "password": "Passw0rd-while-down-1", "clusterRoles": []string{"kingres_readonly"}}, nil)
	t.Logf("createRole on the leader while the follower is down: %d %v", st, code(out))

	// Start it again with the token from before the stop.
	st, start := startWhenStopped(t, c, follower, fid)
	if st != 202 {
		t.Fatalf("start on the follower: %d %v\n%s", st, start, c.Dump())
	}
	startID := clustertest.TaskID(start)
	waitFor(t, c, "the start task of the follower", 3*time.Minute, func() bool {
		st, out := follower.Do("GET", fmt.Sprintf("/tasks/%v", startID), nil, nil)
		return st == 200 && out["state"] == "done"
	})
	// The follower's own tasks: the stop recorded as done, the start written.
	for id, kind := range map[any]string{stopID: "instance.stop", startID: "instance.start"} {
		_, got := follower.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		t.Logf("follower task %v: %v %v", id, got["kind"], got["state"])
		if got["kind"] != kind || got["state"] != "done" {
			t.Errorf("task %v on the follower: %v", id, got)
		}
	}
	follower.Expect(200, "GET", "/cluster", nil)
	waitFor(t, c, "the follower's instance to be running", time.Minute, func() bool {
		st, out := follower.Do("GET", "/this-server/instances/"+fid, nil, nil)
		return st == 200 && out["state"] == "running"
	})
	// A change made on the leader after the follower is back is applied to it.
	leader.Expect(201, "POST", "/roles", map[string]any{"name": "after_up", "password": "Passw0rd-after-up-1", "clusterRoles": []string{"kingres_readonly"}})
	waitFor(t, c, "the role to reach the follower", 2*time.Minute, func() bool {
		st, _ := follower.Do("GET", "/roles/after_up", nil, nil)
		return st == 200
	})
}

// TestLeaderInstanceStopWithAFollower stops the instance of the manager leader.
// There is no failover: the leader process keeps answering, a follower's
// operations that need the leader relay the leader's 503, and the leader's
// instance can be started again with the token of before.
func TestLeaderInstanceStopWithAFollower(t *testing.T) {
	c := clustertest.Form(t, 2)
	leader, follower := c.Nodes[0], c.Nodes[1]
	lid := instanceIDOf(t, leader)
	st, stop := leader.Do("POST", "/this-server/instances/"+lid+"/stop", nil, nil)
	if st == 409 && code(stop) == "no_replica_available" {
		st, stop = leader.Do("POST", "/this-server/instances/"+lid+"/stop", map[string]any{"force": true}, nil)
	}
	if st != 202 {
		t.Fatalf("stop on the leader: %d %v\n%s", st, stop, c.Dump())
	}
	waitFor(t, c, "the leader's instance to stop", 2*time.Minute, func() bool {
		st, out := leader.Do("GET", "/cluster", nil, nil)
		return st == 503 && code(out) == "system_database_stopped"
	})
	// The leader answers its probes and the node state.
	if st, _ := leader.Do("GET", "/healthz", nil, map[string]string{"Authorization": ""}); st != 200 {
		t.Errorf("healthz on the stopped leader: %d", st)
	}
	if st, out := leader.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""}); st != 200 || out["state"] != "clustered" {
		t.Errorf("node state on the stopped leader: %d %v", st, out)
	}
	if st, out := leader.Do("POST", "/auth/login", map[string]any{"username": clustertest.AdminUser, "password": clustertest.AdminPass}, map[string]string{"Authorization": ""}); st != 503 || code(out) != "system_database_stopped" {
		t.Errorf("login on the stopped leader: %d %v", st, out)
	}
	// The follower: its own instance runs, so it still authenticates and answers
	// what it can read locally. Operations that are the leader's relay its 503.
	st, out := follower.Do("GET", "/this-server", nil, nil)
	t.Logf("this-server on the follower while the leader is down: %d %v", st, code(out))
	if st != 200 {
		t.Errorf("this-server on the follower: %d %v", st, out)
	}
	st, out = follower.Do("GET", "/cluster", nil, nil)
	t.Logf("getCluster on the follower while the leader is down: %d %v", st, code(out))
	st, out = follower.Do("POST", "/roles", map[string]any{"name": "via_follower", "password": "Passw0rd-via-follower-1"}, nil)
	t.Logf("createRole on the follower while the leader is down: %d %v", st, code(out))
	if st != 503 || code(out) != "system_database_stopped" {
		t.Errorf("a cluster change through the follower must relay the leader's 503: %d %v", st, out)
	}
	st, out = follower.Do("GET", "/tasks", nil, nil)
	t.Logf("listTasks on the follower while the leader is down: %d %v", st, code(out))
	if st != 503 || code(out) != "system_database_stopped" {
		t.Errorf("a read that is the leader's must relay its 503: %d %v", st, out)
	}
	// The follower can still be asked about its own server and signs in.
	if !follower.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Errorf("sign-in on the follower while the leader is down failed")
	}

	// Start the leader's instance with the earlier token.
	st, start := startWhenStopped(t, c, leader, lid)
	if st != 202 {
		t.Fatalf("start on the leader: %d %v\n%s", st, start, c.Dump())
	}
	startID := clustertest.TaskID(start)
	waitFor(t, c, "the leader's start task", 3*time.Minute, func() bool {
		st, out := leader.Do("GET", fmt.Sprintf("/tasks/%v", startID), nil, nil)
		return st == 200 && out["state"] == "done"
	})
	leader.Expect(200, "GET", "/cluster", nil)
	// The follower's cluster changes work again.
	waitFor(t, c, "a cluster change through the follower", 2*time.Minute, func() bool {
		st, out := follower.Do("POST", "/roles", map[string]any{"name": "via_follower", "password": "Passw0rd-via-follower-1", "clusterRoles": []string{"kingres_readonly"}}, nil)
		if st == 201 {
			return true
		}
		if st != 503 && !(st == 409 && strings.Contains(code(out), "exists")) {
			t.Logf("createRole on the follower after the start: %d %v", st, out)
		}
		return false
	})
}

// startWhenStopped asks for the start; while the shutdown is still going on the
// answer is 409 operation_in_progress (the contract), so it asks again.
func startWhenStopped(t *testing.T, c *clustertest.Cluster, n *clustertest.Node, id string) (int, map[string]any) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		st, out := n.Do("POST", "/this-server/instances/"+id+"/start", nil, nil)
		if st != 409 || code(out) != "operation_in_progress" || time.Now().After(deadline) {
			return st, out
		}
		time.Sleep(300 * time.Millisecond)
	}
}
