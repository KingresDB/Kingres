// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// serversAs lists the servers as seen by node n.
func serversAs(t *testing.T, n *clustertest.Node) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for id, s := range n.Servers() {
		out[id] = s
	}
	return out
}

// Defect 2: a server that joined has a joinedAt and an uptime in every answer,
// and its status does not depend on which server is asked or when.
func TestJoinedServersHaveJoinedAtUptimeAndOneStatus(t *testing.T) {
	c := sharedCluster(t)
	first := serversAs(t, c.Leader())
	if len(first) != 3 {
		t.Fatalf("servers: %v", first)
	}
	for _, n := range c.Nodes {
		for round := 0; round < 3; round++ {
			got := serversAs(t, n)
			for id, s := range got {
				if s["joinedAt"] == nil {
					t.Errorf("node %d, server %s: joinedAt is null", n.Index, id)
				}
				if up, ok := s["uptimeSeconds"].(float64); !ok || up < 0 {
					t.Errorf("node %d, server %s: uptimeSeconds %v", n.Index, id, s["uptimeSeconds"])
				}
				if s["status"] != first[id]["status"] || s["joinedAt"] != first[id]["joinedAt"] {
					t.Errorf("node %d, server %s: status %v joinedAt %v, the leader's answer has %v %v",
						n.Index, id, s["status"], s["joinedAt"], first[id]["status"], first[id]["joinedAt"])
				}
			}
		}
	}
}

// Defect 9 and 8, CP2-05: the fingerprint of a server's own certificate is on its
// own panel, a join must pin one (or say it does not), and the setup task says
// which fingerprint it checked.
func TestJoinPinFlowFromThePanels(t *testing.T) {
	c := clustertest.New(t, 2)
	for _, n := range c.Nodes {
		n.Start()
	}
	leader, joiner := c.Leader(), c.Nodes[1]

	// An uninitialized server tells whoever is allowed to set it up the fingerprint it presents.
	st, state := joiner.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""})
	if st != 200 || state["internalFingerprint"] != joiner.BootstrapFingerprint() {
		t.Errorf("internalFingerprint of an uninitialized server: %d %v (want %s)", st, state["internalFingerprint"], joiner.BootstrapFingerprint())
	}
	leader.InitCluster("pinflow")
	// A member's panel shows the fingerprint of its cluster certificate to a signed-in user.
	cert, err := pki.ParseCertPEM(readFile(t, leader.DataDir, "pki", "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	want := pki.Fingerprint(cert.Raw)
	if info := leader.Expect(200, "GET", "/this-server", nil); info["internalFingerprint"] != want {
		t.Errorf("internalFingerprint of a member: %v (want %s)", info["internalFingerprint"], want)
	}
	if st, state := leader.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""}); st != 200 || state["internalFingerprint"] != nil {
		t.Errorf("a member shows no fingerprint on the public state: %d %v", st, state["internalFingerprint"])
	}

	join := func(extra map[string]any, header map[string]string) (int, map[string]any) {
		body := map[string]any{"existingNodeAddress": leader.RestAddr(),
			"admin":     map[string]any{"username": clustertest.AdminUser, "password": clustertest.AdminPass},
			"dataPaths": map[string]any{"dataDirectory": joiner.DataDir}}
		for k, v := range extra {
			body[k] = v
		}
		h := map[string]string{"X-Setup-Token": joiner.SetupToken(), "Authorization": ""}
		for k, v := range header {
			h[k] = v
		}
		return joiner.Do("POST", "/setup/join-cluster", body, h)
	}
	// No pin and no explicit acceptance: refused before anything is sent; so is a malformed pin.
	for name, tc := range map[string]struct {
		extra  map[string]any
		header map[string]string
	}{
		"no pin":              {nil, nil},
		"allowUnpinned false": {map[string]any{"allowUnpinned": false}, nil},
		"malformed body pin":  {map[string]any{"expectedFingerprint": "12:34"}, nil},
		"malformed header":    {nil, map[string]string{"X-Kingres-Pin-Sha256": "zz"}},
	} {
		if st, out := join(tc.extra, tc.header); st != 400 || out["code"] != "validation_failed" {
			t.Errorf("join with %s: %d %v", name, st, out)
		}
	}
	if st, state := joiner.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""}); st != 200 || state["setupTask"] != nil {
		t.Errorf("a refused join started a task: %d %v", st, state["setupTask"])
	}
	// With the pin read from the leader's panel, the join works and the task records it.
	if st, out := join(map[string]any{"expectedFingerprint": want}, nil); st != 202 {
		t.Fatalf("pinned join: %d %v", st, out)
	}
	joiner.WaitSetup(8 * time.Minute)
	_, after := joiner.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""})
	task, _ := after["setupTask"].(map[string]any)
	if d, _ := task["detail"].(string); !strings.Contains(d, "pinned") || !strings.Contains(d, pki.FormatFingerprint(want)) {
		t.Errorf("setupTask.detail = %q, want the pinned fingerprint %s", d, want)
	}
}

// Defect 5: a server started without its internal API answers the operations that
// need the other servers with a code of their own, not "not implemented".
func TestServerWithoutInternalAPIAnswersADedicatedCode(t *testing.T) {
	c := clustertest.New(t, 1)
	n := c.Leader()
	n.Extra = []string{"-internal-listen="}
	n.Start()
	n.InitCluster("nointernal")
	for _, tc := range []struct{ method, path string }{
		{"POST", "/this-server/leave-cluster"},
		{"POST", "/servers"},
		{"DELETE", "/servers/10.9.9.9"},
	} {
		var body any
		if tc.path == "/servers" {
			body = map[string]any{"address": "10.9.9.9:8091", "setupToken": "x", "allowUnpinned": true}
		}
		st, out := n.Do(tc.method, tc.path, body, nil)
		if st != 409 || code(out) != "internal_api_disabled" {
			t.Errorf("%s %s: %d %v (want 409 internal_api_disabled)", tc.method, tc.path, st, out)
		}
	}
}
