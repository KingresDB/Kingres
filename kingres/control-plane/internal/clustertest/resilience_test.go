// SPDX-License-Identifier: PostgreSQL

package clustertest_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// stepStates returns the step states of a task.
func stepStates(task map[string]any) []string {
	prog, _ := task["progress"].(map[string]any)
	steps, _ := prog["steps"].([]any)
	out := make([]string, len(steps))
	for i, s := range steps {
		m, _ := s.(map[string]any)
		out[i], _ = m["state"].(string)
	}
	return out
}

// TestJoinInterruptedAtEveryStepResumes kills the leader's process (SIGKILL, as
// a crash would) while each step of server.add runs and checks that the task
// resumes after the restart and the server ends up a complete member.
func TestJoinInterruptedAtEveryStepResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	const steps = 7
	for step := 0; step < steps; step++ {
		t.Run(fmt.Sprintf("step%d", step), func(t *testing.T) {
			c := clustertest.New(t, 2)
			leader, n1 := c.Nodes[0], c.Nodes[1]
			leader.Start()
			n1.Start()
			leader.InitCluster("crash")
			mkRole(t, leader, "early", markerPassword, "kingres_readonly")
			st, add := leader.AddServer(n1, n1.SetupToken(), "")
			if st != 202 {
				t.Fatalf("add: %d %v", st, add)
			}
			id := clustertest.TaskID(add)
			// Kill the leader as soon as the step runs (or the task is past it).
			deadline := time.Now().Add(2 * time.Minute)
			for time.Now().Before(deadline) {
				_, task := leader.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
				ss := stepStates(task)
				if len(ss) > step && (ss[step] == "running" || ss[step] == "done") || task["state"] == "done" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			leader.Kill()
			leader.Start()
			if !leader.Login(clustertest.AdminUser, clustertest.AdminPass) {
				// The service connection needs the instance; it is already running.
				time.Sleep(3 * time.Second)
				if !leader.Login(clustertest.AdminUser, clustertest.AdminPass) {
					t.Fatalf("cannot sign in after the restart\n%s", c.Dump())
				}
			}
			final := leader.WaitTask(id, 5*time.Minute)
			if final["state"] != "done" {
				t.Fatalf("the task did not resume at step %d: %v %v\n%s", step, final["state"], final["error"], c.Dump())
			}
			if got := leader.Servers(); len(got) != 2 {
				t.Errorf("servers after the resumed join: %v", got)
			}
			// The new server is a full member: clustered, with the role, certificate-trusted.
			if !n1.Login(clustertest.AdminUser, clustertest.AdminPass) {
				t.Fatalf("the administrator cannot sign in on the new server\n%s", c.Dump())
			}
			if got := count(t, n1, "SELECT count(*) FROM pg_roles WHERE rolname = 'early'"); got != 1 {
				t.Errorf("the role is missing on the new server")
			}
			if st, state := n1.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""}); st != 200 || state["state"] != "clustered" {
				t.Errorf("new server state: %d %v", st, state["state"])
			}
		})
	}
}

// TestFailedJoinBeforeRegistrationCleansUp makes a join fail before the
// registration (the new server's setup token is fine but its instance port is
// taken) and checks that the cluster is unchanged and the certificate revoked.
func TestFailedJoinBeforeRegistrationCleansUp(t *testing.T) {
	c := clustertest.New(t, 2)
	leader, n1 := c.Nodes[0], c.Nodes[1]
	leader.Start()
	n1.Start()
	leader.InitCluster("cleanup")
	// Occupy the new server's PostgreSQL port so that creating its instance fails.
	ln, err := listen(n1)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	st, add := leader.AddServer(n1, n1.SetupToken(), "")
	if st != 202 {
		t.Fatalf("add: %d %v", st, add)
	}
	final := leader.WaitTask(clustertest.TaskID(add), 3*time.Minute)
	if final["state"] != "failed" {
		t.Fatalf("the task ended %v; it must fail (the port is taken)", final["state"])
	}
	if got := leader.Servers(); len(got) != 1 {
		t.Errorf("the cluster changed: %v", got)
	}
	// The clean-up runs after the task is recorded as failed: wait for it. The
	// pending server is forgotten and its certificate revoked.
	var n int
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if err := leader.ServicePG("kingres_system").QueryRow(context.Background(), "SELECT count(*) FROM kingres_cp.server").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
	}
	if n != 1 {
		t.Errorf("the server table has %d rows; want only the leader\n%s", n, c.Dump())
	}
	revoked := string(readFile(t, leader.DataDir, "pki", "revoked.json"))
	if !strings.Contains(revoked, `"serials":{"`) {
		t.Errorf("no certificate was revoked: %s", revoked)
	}
	// The new server discarded what it created and can join again once the port is free.
	st2, state := n1.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""})
	if st2 != 200 || state["state"] != "uninitialized" {
		t.Errorf("new server state: %d %v", st2, state["state"])
	}
	_ = ln.Close()
	st3, add2 := leader.AddServer(n1, n1.SetupToken(), "")
	if st3 != 202 {
		t.Fatalf("add again: %d %v", st3, add2)
	}
	leader.WaitTaskDone(clustertest.TaskID(add2), 6*time.Minute)
}

// TestClusterWideTaskWaitsForADownServer stops one server's kingresd, creates
// a database and checks that the task waits, names the server, and finishes
// when the server is back; the database then exists on every server.
func TestClusterWideTaskWaitsForADownServer(t *testing.T) {
	c := sharedCluster(t)
	down := c.Nodes[2]
	down.Stop()
	t.Cleanup(func() {
		if !down.Running() {
			down.Start()
		}
	})
	body := c.Leader().Expect(202, "POST", "/databases", map[string]any{"name": "down_db", "sharding": map[string]any{"enabled": true, "bucketCount": 16}})
	id := clustertest.TaskID(body)
	want := "Create the database on server " + down.Host
	deadline := time.Now().Add(2 * time.Minute)
	waiting := false
	for time.Now().Before(deadline) && !waiting {
		_, task := c.Leader().Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		prog, _ := task["progress"].(map[string]any)
		if prog["currentStep"] == want {
			waiting = true
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !waiting {
		t.Fatalf("the task did not reach %q\n%s", want, c.Leader().LogTail(3000))
	}
	time.Sleep(6 * time.Second) // it keeps waiting: it neither fails nor skips the server
	_, task := c.Leader().Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
	if task["state"] != "running" {
		t.Fatalf("the task is %v while a server is down; it must wait", task["state"])
	}
	if !strings.Contains(c.Leader().Log(), "task waits for a server") || !strings.Contains(c.Leader().Log(), down.Host) {
		t.Error("the log does not say which server the task waits for")
	}
	down.Start()
	if !down.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Fatalf("cannot sign in on the restarted server\n%s", down.LogTail(2000))
	}
	final := c.Leader().WaitTask(id, 4*time.Minute)
	if final["state"] != "done" {
		t.Fatalf("the task ended %v: %v\n%s", final["state"], final["error"], c.Leader().LogTail(3000))
	}
	for _, n := range c.Nodes {
		if got := count(t, n, "SELECT count(*) FROM pg_database WHERE datname = 'down_db'"); got != 1 {
			t.Errorf("node %d: down_db: %d", n.Index, got)
		}
	}
}

// TestNetworkRulesReachEveryServer checks that replacing the network rules runs
// on every server (one step per server).
func TestNetworkRulesReachEveryServer(t *testing.T) {
	c := sharedCluster(t)
	cur := c.Leader().Expect(200, "GET", "/settings/network-access", nil)
	rules, _ := cur["rules"].([]any)
	rules = append([]any{map[string]any{"type": "host", "databases": []string{"all"}, "users": []string{"all"}, "address": "10.9.8.0/24", "method": "scram-sha-256"}}, rules...)
	body := c.Leader().Expect(202, "PUT", "/settings/network-access", map[string]any{"revision": cur["revision"], "tlsRequired": cur["tlsRequired"], "rules": rules})
	task := c.Leader().WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
	if got := stepStates(task); len(got) != 4 {
		t.Errorf("the task has %d steps (%v); want the two local ones and one per other server", len(got), got)
	}
	for _, n := range c.Nodes {
		hba := string(readFile(t, n.DataDir, "instances", "1", "pgdata", "pg_hba.conf"))
		if !strings.Contains(hba, "10.9.8.0/24") {
			t.Errorf("node %d: pg_hba.conf lacks the new rule", n.Index)
		}
		if !strings.Contains(hba, "cert map=kingres_nodes") {
			t.Errorf("node %d: pg_hba.conf lost the certificate rules for the other servers:\n%s", n.Index, hba)
		}
	}
}

func listen(n *clustertest.Node) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", n.PGPort))
}

// TestRoleChangeReachesAServerThatWasDown stops one server, creates a role
// (the contract's answer is synchronous), and checks that the stopped server
// gets the role when it is back, from the task that waited for it.
func TestRoleChangeReachesAServerThatWasDown(t *testing.T) {
	c := sharedCluster(t)
	down := c.Nodes[2]
	down.Stop()
	t.Cleanup(func() {
		if !down.Running() {
			down.Start()
		}
	})
	started := time.Now()
	c.Leader().Expect(201, "POST", "/roles", map[string]any{"name": "catchup", "password": "Catch-Up-Password-5", "clusterRoles": []string{"kingres_readonly"}})
	if d := time.Since(started); d > 90*time.Second {
		t.Errorf("the role operation took %v with a server down", d)
	}
	if got := count(t, c.Nodes[1], "SELECT count(*) FROM pg_roles WHERE rolname = 'catchup'"); got != 1 {
		t.Errorf("the running server has the role: %d", got)
	}
	down.Start()
	if !down.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Fatalf("cannot sign in on the restarted server\n%s", down.LogTail(2000))
	}
	deadline := time.Now().Add(2 * time.Minute)
	for count(t, down, "SELECT count(*) FROM pg_roles WHERE rolname = 'catchup'") != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the role never reached the server that was down\n%s", c.Leader().LogTail(3000))
		}
		time.Sleep(time.Second)
	}
	// The housekeeping task is not part of the contract's task kinds and is hidden.
	body := c.Leader().Expect(200, "GET", "/tasks?limit=100", nil)
	items, _ := body["items"].([]any)
	for _, it := range items {
		if m, _ := it.(map[string]any); strings.HasPrefix(fmt.Sprint(m["kind"]), "replicate.") {
			t.Errorf("the task API lists the housekeeping task: %v", m)
		}
	}
}
