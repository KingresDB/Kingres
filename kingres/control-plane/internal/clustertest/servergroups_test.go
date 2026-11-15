// SPDX-License-Identifier: PostgreSQL

package clustertest_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

func memberIDs(g map[string]any) []string {
	var out []string
	for _, id := range g["serverIds"].([]any) {
		out = append(out, id.(string))
	}
	return out
}

func groupByName(n *clustertest.Node, name string) map[string]any {
	n.Do("GET", "/server-groups", nil, nil)
	_, body := n.Do("GET", "/server-groups", nil, nil)
	items, _ := body["items"].([]any)
	for _, it := range items {
		if m := it.(map[string]any); m["name"] == name {
			return m
		}
	}
	return nil
}

// eventually polls until f is true (the catalog reaches the other servers a
// moment after the leader changed it).
func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// Server groups are changed through any server (a follower forwards to the
// leader) and every server shows them.
func TestServerGroupsAcrossServers(t *testing.T) {
	c := sharedCluster(t)
	leader, f1, f2 := c.Nodes[0], c.Nodes[1], c.Nodes[2]
	target := f2.Host

	for _, n := range c.Nodes {
		if _, body := n.Do("GET", "/server-groups", nil, nil); len(body["items"].([]any)) != 0 {
			t.Fatalf("node %d: groups before: %v", n.Index, body)
		}
	}
	// Create one group on the leader and one through a follower.
	g := leader.Expect(201, "POST", "/server-groups", map[string]any{"name": "rack-a", "description": "Row 1"})
	if g["name"] != "rack-a" || len(g["serverIds"].([]any)) != 0 {
		t.Errorf("created %v", g)
	}
	f1.Expect(201, "POST", "/server-groups", map[string]any{"name": "rack-b"})
	if st, o := f2.Do("POST", "/server-groups", map[string]any{"name": "rack-a"}, nil); st != 409 || o["code"] != "already_exists" {
		t.Errorf("duplicate through a follower: %d %v", st, o)
	}
	for _, n := range c.Nodes {
		if groupByName(n, "rack-a") == nil || groupByName(n, "rack-b") == nil {
			t.Errorf("node %d does not list both groups", n.Index)
		}
	}

	// Assign a server by a request to a FOLLOWER (forwarded to the leader).
	out := f1.Expect(200, "PATCH", "/servers/"+target, map[string]any{"serverGroup": "rack-a"})
	if out["id"] != target || out["serverGroup"] != "rack-a" {
		t.Errorf("updated server %v", out)
	}
	for _, n := range c.Nodes {
		if got := n.Servers()[target]["serverGroup"]; got != "rack-a" {
			t.Errorf("node %d: serverGroup of %s = %v", n.Index, target, got)
		}
		if got := n.Expect(200, "GET", "/servers/"+target, nil)["serverGroup"]; got != "rack-a" {
			t.Errorf("node %d: getServer serverGroup = %v", n.Index, got)
		}
		if m := memberIDs(groupByName(n, "rack-a")); len(m) != 1 || m[0] != target {
			t.Errorf("node %d: members of rack-a: %v", n.Index, m)
		}
		if m := memberIDs(groupByName(n, "rack-b")); len(m) != 0 {
			t.Errorf("node %d: members of rack-b: %v", n.Index, m)
		}
	}
	// An equal value is a no-op.
	f2.Expect(200, "PATCH", "/servers/"+target, map[string]any{"serverGroup": "rack-a"})

	// Refusals, relayed from the leader.
	st, o := f2.Do("DELETE", "/server-groups/rack-a", nil, nil)
	if st != 409 || o["code"] != "server_group_not_empty" || !strings.Contains(o["detail"].(string), target) {
		t.Errorf("delete non-empty through a follower: %d %v", st, o)
	}
	if groupByName(leader, "rack-a") == nil {
		t.Error("the refused delete removed the group")
	}
	if st, o := f1.Do("PATCH", "/servers/"+target, map[string]any{"serverGroup": "missing"}, nil); st != 422 || o["code"] != "unprocessable" {
		t.Errorf("unknown group: %d %v", st, o)
	} else if errs, _ := o["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "/serverGroup" {
		t.Errorf("unknown group errors: %v", o["errors"])
	}
	if st, o := f1.Do("PATCH", "/servers/10.9.9.9", map[string]any{"serverGroup": nil}, nil); st != 404 {
		t.Errorf("unknown server: %d %v", st, o)
	}
	// A read-only role is refused by the server it asked (nothing is forwarded).
	st, login := f1.Do("POST", "/auth/login", map[string]any{"username": "early", "password": markerPassword, "issueToken": true}, map[string]string{"Authorization": ""})
	if st != 200 {
		t.Fatalf("login of the read-only role: %d %v", st, login)
	}
	ro := map[string]string{"Authorization": "Bearer " + login["token"].(string)}
	if st, _ := f1.Do("POST", "/server-groups", map[string]any{"name": "ro"}, ro); st != 403 {
		t.Errorf("readonly create: %d", st)
	}
	if st, _ := f1.Do("PATCH", "/servers/"+target, map[string]any{"serverGroup": nil}, ro); st != 403 {
		t.Errorf("readonly update: %d", st)
	}
	if st, _ := f1.Do("DELETE", "/server-groups/rack-b", nil, ro); st != 403 {
		t.Errorf("readonly delete: %d", st)
	}
	if st, _ := f1.Do("GET", "/server-groups", nil, ro); st != 200 {
		t.Errorf("readonly list: %d", st)
	}

	// shouldHaveShards through a follower reaches the catalog of every server.
	out = f2.Expect(200, "PATCH", "/servers/"+target, map[string]any{"shouldHaveShards": false})
	if out["shouldHaveShards"] != false || out["serverGroup"] != "rack-a" {
		t.Errorf("after shouldHaveShards=false: %v", out)
	}
	for _, n := range c.Nodes {
		eventually(t, "shouldHaveShards=false on node "+n.Host, func() bool { return n.Servers()[target]["shouldHaveShards"] == false })
	}
	f1.Expect(200, "PATCH", "/servers/"+target, map[string]any{"shouldHaveShards": true})
	for _, n := range c.Nodes {
		eventually(t, "shouldHaveShards=true on node "+n.Host, func() bool { return n.Servers()[target]["shouldHaveShards"] == true })
	}

	// Unassign, then the group can be deleted (through a follower).
	out = f2.Expect(200, "PATCH", "/servers/"+target, map[string]any{"serverGroup": nil})
	if out["serverGroup"] != nil {
		t.Errorf("unassigned server %v", out)
	}
	for _, n := range c.Nodes {
		if n.Servers()[target]["serverGroup"] != nil {
			t.Errorf("node %d still shows a group for %s", n.Index, target)
		}
		if m := memberIDs(groupByName(n, "rack-a")); len(m) != 0 {
			t.Errorf("node %d: members of rack-a: %v", n.Index, m)
		}
	}
	f2.Expect(204, "DELETE", "/server-groups/rack-a", nil)
	f1.Expect(204, "DELETE", "/server-groups/rack-b", nil)
	for _, n := range c.Nodes {
		if _, body := n.Do("GET", "/server-groups", nil, nil); len(body["items"].([]any)) != 0 {
			t.Errorf("node %d: groups after the deletes: %v", n.Index, body)
		}
	}

	// The leader audited every request once, naming the forwarding server for
	// the forwarded ones; the followers audited none of them.
	for _, tc := range []struct {
		action           string
		success, viaPeer int // successes by the administrator, and those that arrived through a follower
	}{{"createServerGroup", 2, 1}, {"deleteServerGroup", 2, 2}, {"updateServer", 5, 5}} {
		var ok, forwarded int
		a := leader.Expect(200, "GET", "/audit?action="+tc.action+"&limit=200", nil)
		for _, it := range a["items"].([]any) {
			m := it.(map[string]any)
			if m["outcome"] != "success" || m["actor"] != clustertest.AdminUser {
				continue
			}
			ok++
			if d, _ := m["details"].(map[string]any); d["forwardedBy"] != nil {
				forwarded++
			}
		}
		if ok != tc.success || forwarded != tc.viaPeer {
			t.Errorf("leader audited %d successful %s events (%d forwarded), want %d (%d)", ok, tc.action, forwarded, tc.success, tc.viaPeer)
		}
	}
	for _, n := range []*clustertest.Node{f1, f2} {
		a := n.Expect(200, "GET", "/audit?action=createServerGroup&limit=200", nil)
		for _, it := range a["items"].([]any) {
			// Only the refused attempt of the read-only role is audited where it was made.
			// The list is the cluster's: only the events this follower wrote count.
			if m := it.(map[string]any); m["serverId"] == n.Host && m["outcome"] != "denied" {
				t.Errorf("the follower %d audited a forwarded request: %v", n.Index, m)
			}
		}
	}
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The properties of Server that come from the certificates and the first
// contact are seen from every server, and the setup task of a server that
// joined is done.
func TestServerCertificateAndFingerprintProperties(t *testing.T) {
	c := sharedCluster(t)
	for _, n := range c.Nodes {
		servers := n.Servers()
		for _, m := range c.Nodes {
			s := servers[m.Host]
			exp, _ := s["certificateExpiresAt"].(string)
			if exp == "" {
				t.Errorf("node %d: certificateExpiresAt of %s is null", n.Index, m.Host)
			} else if ts, err := time.Parse(time.RFC3339, exp); err != nil || ts.Before(time.Now().Add(24*time.Hour)) {
				t.Errorf("node %d: certificateExpiresAt of %s = %q", n.Index, m.Host, exp)
			}
			seen, _ := s["seenFingerprint"].(string)
			switch {
			case m != c.Nodes[2] && s["seenFingerprint"] != nil:
				// The creator has no first contact; a server that joined by its own
				// request presented nothing to the leader (it checked the leader's
				// certificate, which is in its own setup task).
				t.Errorf("node %d: %s has seenFingerprint %v", n.Index, m.Host, s["seenFingerprint"])
			case m == c.Nodes[2] && !hex64.MatchString(seen):
				t.Errorf("node %d: seenFingerprint of %s = %v", n.Index, m.Host, s["seenFingerprint"])
			}
			if s["fingerprintPinned"] != false {
				t.Errorf("node %d: fingerprintPinned of %s = %v (trust on first use)", n.Index, m.Host, s["fingerprintPinned"])
			}
		}
	}
	// The servers that joined have a finished setup task; the creator too.
	for _, n := range c.Nodes[:2] {
		_, body := n.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""})
		tk, _ := body["setupTask"].(map[string]any)
		if tk == nil || tk["state"] != "done" || tk["progressPercent"] != float64(100) {
			t.Errorf("node %d: setupTask = %v", n.Index, body["setupTask"])
		}
	}
	_, b3 := c.Nodes[2].Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""})
	// A server the leader added never accepted a setup request: null.
	if b3["setupTask"] != nil {
		t.Errorf("node 2: setupTask of a server added by the leader = %v, want null", b3["setupTask"])
	}
}
