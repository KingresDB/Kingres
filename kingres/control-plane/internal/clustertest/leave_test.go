// SPDX-License-Identifier: PostgreSQL

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
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

const leaveURL = "/this-server/leave-cluster"

// formManually forms a cluster of n servers like clustertest.Form, but hands
// back the setup token every server had before it joined.
func formManually(t *testing.T, n int, before func(leader *clustertest.Node)) (*clustertest.Cluster, []string) {
	t.Helper()
	c := clustertest.New(t, n)
	tokens := make([]string, n)
	for i, nd := range c.Nodes {
		nd.Start()
		tokens[i] = nd.SetupToken()
	}
	c.Leader().InitCluster("leave")
	if before != nil {
		before(c.Leader())
	}
	for i := 1; i < n; i++ {
		nd := c.Nodes[i]
		if i == 1 {
			if st, body := nd.JoinFrom(c.Leader(), ""); st != 202 {
				t.Fatalf("join: %d %v", st, body)
			}
			nd.WaitSetup(8 * time.Minute)
		} else {
			st, body := c.Leader().AddServer(nd, tokens[i], "")
			if st != 202 {
				t.Fatalf("add: %d %v", st, body)
			}
			c.Leader().WaitTaskDone(clustertest.TaskID(body), 8*time.Minute)
		}
		if !nd.Login(clustertest.AdminUser, clustertest.AdminPass) {
			t.Fatalf("no sign-in on node %d", i)
		}
	}
	return c, tokens
}

// serialOf reads the serial number (hex) of the node's server certificate.
func serialOf(t *testing.T, n *clustertest.Node) string {
	t.Helper()
	crt, err := pki.ParseCertPEM(readFile(t, n.DataDir, "pki", "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return pki.SerialHex(crt.SerialNumber)
}

func revokedOn(t *testing.T, observer *clustertest.Node, as *clustertest.Node) []string {
	t.Helper()
	st, out := internalCall(t, observer, loadPair(t, as), "1", "GET", "/revoked", nil)
	if st != 200 {
		t.Fatalf("GET /revoked on node %d: %d %v", observer.Index, st, out)
	}
	var serials []string
	for _, s := range anyList(out["serials"]) {
		serials = append(serials, s.(string))
	}
	return serials
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// waitUninitialized waits until the node answers /setup/state as uninitialized.
func waitUninitialized(t *testing.T, n *clustertest.Node, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		st, body := n.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""})
		if st == 200 && body["state"] == "uninitialized" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %d did not become uninitialized: %d %v\n%s", n.Index, st, body, n.LogTail(3000))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func auditActions(t *testing.T, n *clustertest.Node) string {
	t.Helper()
	return string(readFile(t, n.DataDir, "audit.jsonl"))
}

// sessionToken signs in as the role and returns its bearer token on node n.
func sessionToken(t *testing.T, n *clustertest.Node, user, pass string) string {
	t.Helper()
	st, body := n.Do("POST", "/auth/login", map[string]any{"username": user, "password": pass, "issueToken": true}, map[string]string{"Authorization": ""})
	if st != 200 {
		t.Fatalf("login %s: %d %v", user, st, body)
	}
	return body["token"].(string)
}

func TestLeaveClusterFollowerLeavesThroughItsOwnAPIAndJoinsAgain(t *testing.T) {
	c, tokens := formManually(t, 3, nil)
	leader, mid, leaving := c.Nodes[0], c.Nodes[1], c.Nodes[2]
	oldPair := loadPair(t, leaving)
	serial := serialOf(t, leaving)
	leaving.Expect(200, "GET", "/server-groups", nil) // opens its connection to the leader before it leaves
	mkRole(t, leader, "leave_ops", "Leave-Ops-Password-1", "kingres_ops")

	// The role must be an administrator.
	opsTok := sessionToken(t, leaving, "leave_ops", "Leave-Ops-Password-1")
	if st, out := leaving.Do("POST", leaveURL, nil, map[string]string{"Authorization": "Bearer " + opsTok}); st != 403 {
		t.Errorf("leave as kingres_ops: %d %v", st, out)
	}
	if st, _ := leaving.Do("POST", leaveURL, nil, map[string]string{"Authorization": "Bearer nonsense"}); st != 401 {
		t.Errorf("leave without a session: %d", st)
	}
	// A body that does not match the contract is a 400 and changes nothing.
	for _, body := range []any{map[string]any{"force": "yes"}, map[string]any{"serverId": mid.Host}} {
		if st, out := leaving.Do("POST", leaveURL, body, nil); st != 400 {
			t.Errorf("leave with body %v: %d %v", body, st, out)
		}
	}
	// The leader cannot leave: it hosts the map group (with force too).
	for _, body := range []any{nil, map[string]any{"force": true}} {
		st, out := leader.Do("POST", leaveURL, body, nil)
		if st != 409 || out["code"] != "server_hosts_map_group" {
			t.Errorf("the leader leaves (%v): %d %v", body, st, out)
		}
	}
	if got := leader.Servers(); len(got) != 3 {
		t.Fatalf("refusals changed the cluster: %v", got)
	}

	// The follower leaves through its own API.
	st, accepted, hdr := leaving.DoH("POST", leaveURL, nil, nil)
	if st != 202 {
		t.Fatalf("leave: %d %v\n%s", st, accepted, leaving.LogTail(3000))
	}
	id := clustertest.TaskID(accepted)
	if accepted["kind"] != "server.leave" || hdr.Get("Location") != fmt.Sprintf("/api/v1/tasks/%v", int64(id.(float64))) {
		t.Errorf("task %v, Location %q", accepted, hdr.Get("Location"))
	}
	if subj, _ := accepted["subject"].(map[string]any); subj["type"] != "server" || subj["id"] != leaving.Host {
		t.Errorf("subject %v", accepted["subject"])
	}
	// While it is still a member, the leaving server follows the task to the leader.
	if st, got := leaving.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil); st != 200 || got["kind"] != "server.leave" {
		t.Errorf("the leaving server's GET /tasks/{id}: %d %v", st, got)
	}
	// A second leave, and a removal, of the same server while the task runs.
	if st, out := leaving.Do("POST", leaveURL, nil, nil); st != 409 || out["code"] != "operation_in_progress" {
		t.Errorf("second leave: %d %v", st, out)
	}
	if st, out := leader.Do("DELETE", "/servers/"+leaving.Host, nil, nil); st != 409 || out["code"] != "operation_in_progress" {
		t.Errorf("removeServer during a leave: %d %v", st, out)
	}

	// The task is recorded done on the leader.
	done := leader.WaitTaskDone(id, 5*time.Minute)
	if done["kind"] != "server.leave" {
		t.Errorf("task: %v", done)
	}
	for _, n := range []*clustertest.Node{leader, mid} {
		if got := n.Servers(); len(got) != 2 || got[leaving.Host] != nil {
			t.Errorf("node %d still lists %v", n.Index, got)
		}
	}
	// Its certificate is revoked on the leader and distributed.
	for _, n := range []*clustertest.Node{leader, mid} {
		if !contains(revokedOn(t, n, mid), serial) {
			t.Errorf("the serial of the leaving server is not in the revocation list of node %d", n.Index)
		}
		if st, out := internalCall(t, n, oldPair, "1", "GET", "/state", nil); st != 401 || out["error"] != "certificate_refused" {
			t.Errorf("node %d accepted the certificate of a server that left: %d %v", n.Index, st, out)
		}
	}
	// The leaving server resets itself: uninitialized, instance and key gone, a new token.
	waitUninitialized(t, leaving, 90*time.Second)
	if _, err := os.Stat(filepath.Join(leaving.DataDir, "instances", "1")); err == nil {
		t.Error("the server that left still has its instance")
	}
	if _, err := os.Stat(filepath.Join(leaving.DataDir, "pki", "server.crt")); err == nil {
		t.Error("the server that left still has its certificate")
	}
	if _, err := os.Stat(filepath.Join(leaving.DataDir, "pki", "server.key")); err == nil {
		t.Error("the server that left still has its key")
	}
	if _, err := os.Stat(filepath.Join(leaving.DataDir, "leave-pending.json")); err == nil {
		t.Error("the record of the leave is still there")
	}
	newToken := leaving.SetupToken()
	if newToken == "" || newToken == tokens[2] {
		t.Errorf("the setup token after leaving is %q (before: %q)", newToken, tokens[2])
	}
	// What a client sees now: the task is on the leader; this server says it is uninitialized.
	if st, out := leaving.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil); st != 503 || out["code"] != "node_uninitialized" {
		t.Errorf("GET /tasks/{id} on the server that left: %d %v", st, out)
	}
	// Both audit files have the entry; the leaving server's audit file survived the reset.
	if a := auditActions(t, leaving); !strings.Contains(a, `"action":"leaveCluster"`) || !strings.Contains(a, `"outcome":"success"`) {
		t.Errorf("the audit file of the server that left has no leaveCluster entry:\n%s", a)
	}
	body := leader.Expect(200, "GET", "/audit?action=leaveCluster&limit=50", nil)
	phases := map[string]int{}
	items, _ := body["items"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		d, _ := m["details"].(map[string]any)
		if m["outcome"] == "success" && m["resourceId"] == leaving.Host {
			phases[fmt.Sprint(d["phase"])]++
		}
	}
	if phases["accepted"] != 1 || phases["completed"] != 1 {
		t.Errorf("the leader's audit of the leave (accepted, completed): %v in %v", phases, items)
	}

	// It can join again, with the new setup token.
	if st, out := leaving.JoinFrom(leader, ""); st != 202 {
		t.Fatalf("joining again: %d %v", st, out)
	}
	leaving.WaitSetup(8 * time.Minute)
	if !leaving.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Fatal("no sign-in after joining again")
	}
	// A forwarded read through the rejoined server works at once: its connections to the leader are new
	// (a connection opened with the deleted certificate would be refused by the leader).
	if st, out := leaving.Do("GET", "/server-groups", nil, nil); st != 200 {
		t.Errorf("a leader-bound read through the server that joined again: %d %v", st, out)
	}
	for _, n := range c.Nodes {
		if got := n.Servers(); len(got) != 3 {
			t.Errorf("node %d after joining again: %v", n.Index, got)
		}
	}
	if serialOf(t, leaving) == serial {
		t.Error("the server got its old certificate back")
	}
}

func TestLeaveRefusalsBucketsAndForgedInternalCalls(t *testing.T) {
	c := sharedCluster(t)
	prepareTable(t, c)
	leader, mid, last := c.Nodes[0], c.Nodes[1], c.Nodes[2]

	// Both followers hold buckets: 409 server_owns_buckets, with and without force, naming the database.
	for _, n := range []*clustertest.Node{mid, last} {
		for _, body := range []any{nil, map[string]any{"force": true}} {
			st, out := n.Do("POST", leaveURL, body, nil)
			if st != 409 || out["code"] != "server_owns_buckets" || !strings.Contains(fmt.Sprint(out["detail"]), "after_db") {
				t.Errorf("node %d leaves (%v): %d %v", n.Index, body, st, out)
			}
		}
		// removeServer says the same with the contract's code.
		st, out := leader.Do("DELETE", "/servers/"+n.Host, nil, nil)
		if st != 409 || out["code"] != "server_owns_buckets" || !strings.Contains(fmt.Sprint(out["detail"]), "after_db") {
			t.Errorf("removeServer %s: %d %v", n.Host, st, out)
		}
	}
	// removeServer: the map group's server and an unknown one.
	if st, out := leader.Do("DELETE", "/servers/"+leader.Host, nil, nil); st != 409 || out["code"] != "server_hosts_map_group" {
		t.Errorf("removeServer of the leader: %d %v", st, out)
	}
	if st, out := leader.Do("DELETE", "/servers/nowhere.example", nil, nil); st != 404 || out["code"] != "not_found" {
		t.Errorf("removeServer of an unknown server: %d %v", st, out)
	}

	// A forged internal call: server X (mid) names server Y (last). The body has no
	// field for it, and a call without the name acts for the caller only.
	pair := loadPair(t, mid)
	for _, body := range []map[string]any{
		{"actor": clustertest.AdminUser, "serverId": last.Host},
		{"actor": clustertest.AdminUser, "server": last.Host},
		{"actor": clustertest.AdminUser, "subject": map[string]any{"type": "server", "id": last.Host}},
	} {
		if st, out := internalCall(t, leader, pair, "1", "POST", "/leave-cluster", body); st != 400 {
			t.Errorf("forged leave %v: %d %v", body, st, out)
		}
	}
	// The valid call by X is decided for X: it owns buckets, so it is refused for X.
	st, out := internalCall(t, leader, pair, "1", "POST", "/leave-cluster", map[string]any{"actor": clustertest.AdminUser})
	if st != 409 || out["error"] != "server_owns_buckets" {
		t.Errorf("leave call by a server that owns buckets: %d %v", st, out)
	}
	// Only the leader answers; a follower refuses.
	if st, out := internalCall(t, last, pair, "1", "POST", "/leave-cluster", map[string]any{"actor": clustertest.AdminUser}); st != 409 || out["error"] != "not_leader" {
		t.Errorf("leave call to a server that is not the leader: %d %v", st, out)
	}
	// A server cannot read the status of a task that is not its own leave task.
	if st, out := internalCall(t, leader, pair, "1", "POST", "/leave-status", map[string]any{"taskId": 1}); st != 404 {
		t.Errorf("leave status of someone else's task: %d %v", st, out)
	}
	// No certificate, no call.
	if st, _ := internalCall(t, leader, nil, "1", "POST", "/leave-cluster", map[string]any{"actor": "x"}); st != 401 {
		t.Errorf("leave call without a certificate: %d", st)
	}
	if got := leader.Servers(); len(got) != 3 {
		t.Errorf("refusals changed the cluster: %v", got)
	}
}

func TestLeaveRefusedForTheOnlyServer(t *testing.T) {
	c := clustertest.New(t, 1)
	n := c.Leader()
	n.Start()
	n.InitCluster("solo")
	// last_server comes before the map group, in both operations.
	if st, out := n.Do("POST", leaveURL, nil, nil); st != 409 || out["code"] != "last_server" {
		t.Errorf("leave of the only server: %d %v", st, out)
	}
	if st, out := n.Do("POST", leaveURL, map[string]any{"force": true}, nil); st != 409 || out["code"] != "last_server" {
		t.Errorf("forced leave of the only server: %d %v", st, out)
	}
	if st, out := n.Do("DELETE", "/servers/"+n.Host, nil, nil); st != 409 || out["code"] != "last_server" {
		t.Errorf("removeServer of the only server: %d %v", st, out)
	}
	if st, _ := n.Do("GET", "/cluster", nil, nil); st != 200 {
		t.Errorf("the cluster is gone: %d", st)
	}
}

// force lifts the requirement that the cluster is reachable and nothing else.
func TestLeaveForceWhenTheLeaderCannotBeReached(t *testing.T) {
	c, tokens := formManually(t, 3, func(leader *clustertest.Node) {
		// A distributed table that exists before the others join: all its buckets
		// are on the leader, so the other servers hold none.
		mkDB(t, leader, "force_db")
		admin := leader.MustPG(clustertest.AdminUser, clustertest.AdminPass, "force_db")
		if _, err := admin.Exec(context.Background(), "CREATE TABLE ft (id int PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
		body := leader.Expect(202, "PUT", "/databases/force_db/schemas/public/tables/ft/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader, leaving := c.Nodes[0], c.Nodes[2]
	serial := serialOf(t, leaving)

	leader.Stop() // kingresd of the leader; its instance keeps running
	// Without force: refused, still a member.
	st, out := leaving.Do("POST", leaveURL, nil, nil)
	if st != 503 || out["code"] != "service_unavailable" {
		t.Errorf("leave without the leader: %d %v", st, out)
	}
	if st, state := leaving.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""}); st != 200 || state["state"] != "clustered" {
		t.Errorf("state after the refused leave: %d %v", st, state)
	}
	// With force: accepted as a finished task that is not recorded anywhere; the server resets itself.
	st, out = leaving.Do("POST", leaveURL, map[string]any{"force": true}, nil)
	if st != 202 || out["kind"] != "server.leave" || out["state"] != "done" {
		t.Fatalf("forced leave without the leader: %d %v", st, out)
	}
	waitUninitialized(t, leaving, 90*time.Second)
	if tok := leaving.SetupToken(); tok == "" || tok == tokens[2] {
		t.Errorf("setup token %q (before %q)", tok, tokens[2])
	}
	if a := auditActions(t, leaving); !strings.Contains(a, `"action":"leaveCluster"`) {
		t.Errorf("no audit entry on the server that left:\n%s", a)
	}

	// The cluster still lists it, until it is removed from there; its certificate was not revoked.
	leader.Start()
	deadline := time.Now().Add(2 * time.Minute)
	for !leader.Login(clustertest.AdminUser, clustertest.AdminPass) {
		if time.Now().After(deadline) {
			t.Fatal("no sign-in on the restarted leader")
		}
		time.Sleep(time.Second)
	}
	for {
		if got := leader.Servers(); got[leaving.Host] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cluster no longer lists the server that left on its own")
		}
		time.Sleep(time.Second)
	}
	if contains(revokedOn(t, leader, leader), serial) {
		t.Error("the certificate of a server that left alone is revoked without the cluster's action")
	}
	// The checks still run with force: the leader's own server is refused.
	if st, out := leader.Do("POST", leaveURL, map[string]any{"force": true}, nil); st != 409 || out["code"] != "server_hosts_map_group" {
		t.Errorf("forced leave of the leader: %d %v", st, out)
	}
}
