// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
)

// record writes an event into the audit log of one server of the test cluster at
// the given second (every server has a clock of its own).
func (s *gsrv) record(t *testing.T, sec int, action string) {
	t.Helper()
	s.env.clock.mu.Lock()
	s.env.clock.t = time.Date(2026, 11, 14, 11, 0, sec, 0, time.UTC)
	s.env.clock.mu.Unlock()
	if _, err := s.env.audit.Append(context.Background(), audit.Event{Actor: "alice", Source: "cli", Action: action, Outcome: audit.OutcomeSuccess}); err != nil {
		t.Fatal(err)
	}
}

func auditActions(items []any) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.(map[string]any)["action"].(string))
	}
	return out
}

// CP2-13: an administrator who reads the audit log from any server sees the
// actions that the other servers wrote (the leader writes every forwarded
// administrative action), merged in time order, with ids that do not collide.
func TestAuditListIsClusterWideAndPaged(t *testing.T) {
	c := newGCluster(t, []map[int]string{{}, {}, {}}, time.Second)
	s1, s2, s3 := c.servers[0], c.servers[1], c.servers[2]
	s1.record(t, 1, "createRole")
	s2.record(t, 2, "createGrant") // written by the leader in real life
	s3.record(t, 3, "dropRole")
	s2.record(t, 4, "createDatabase")
	s1.record(t, 5, "updateRole")

	want := []string{"updateRole", "createDatabase", "dropRole", "createGrant", "createRole"}
	code, body := s3.get("/api/v1/audit?limit=100", auth.RoleAdmin)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if got := auditActions(body["items"].([]any)); !equal(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	if body["partial"] != false {
		t.Errorf("partial = %v", body["partial"])
	}
	ids := map[float64]bool{}
	for _, it := range body["items"].([]any) {
		e := it.(map[string]any)
		ids[e["id"].(float64)] = true
		if e["serverId"] == nil {
			t.Errorf("event without serverId: %v", e)
		}
	}
	if len(ids) != 5 {
		t.Errorf("ids collide: %v", ids)
	}

	// Paging through the merged log returns every event once, whichever server is asked.
	var got []string
	path := "/api/v1/audit?limit=2"
	for pages := 0; pages < 10; pages++ {
		code, body = s2.get(path, auth.RoleAdmin)
		if code != http.StatusOK {
			t.Fatalf("status %d: %v", code, body)
		}
		got = append(got, auditActions(body["items"].([]any))...)
		next, _ := body["nextCursor"].(string)
		if next == "" {
			break
		}
		path = "/api/v1/audit?limit=2&cursor=" + next
	}
	if !equal(got, want) {
		t.Errorf("paged actions = %v, want %v", got, want)
	}
}

func TestAuditListSaysWhichServersDidNotAnswer(t *testing.T) {
	c := newGCluster(t, []map[int]string{{}, {}, {}}, time.Second)
	c.servers[0].record(t, 1, "createRole")
	c.servers[1].record(t, 2, "createGrant")
	c.servers[2].down = true
	code, body := c.servers[0].get("/api/v1/audit", auth.RoleAdmin)
	if code != http.StatusOK || body["partial"] != true {
		t.Fatalf("status %d partial %v: %v", code, body["partial"], body)
	}
	missing := body["missingServers"].([]any)
	if len(missing) != 1 || missing[0].(map[string]any)["serverId"] != "s3" {
		t.Errorf("missingServers = %v", missing)
	}
	if got := auditActions(body["items"].([]any)); !equal(got, []string{"createGrant", "createRole"}) {
		t.Errorf("actions = %v", got)
	}
}

// A gathered read is authorized by the answering server itself: a role that may
// not read the audit log does not get it by asking through another server.
func TestAuditListStaysAdminOnly(t *testing.T) {
	c := newGCluster(t, []map[int]string{{}, {}}, time.Second)
	if code, _ := c.servers[0].get("/api/v1/audit", auth.RoleOps); code != http.StatusForbidden {
		t.Errorf("ops read the audit log: %d", code)
	}
}

// CP2-13 (second part): a state-changing operation is not run while the audit
// log cannot take its record.
func TestMutationIsRefusedWhileTheAuditLogCannotBeWritten(t *testing.T) {
	e := newEnv(t, func(*Deps) {})
	id := "s1"
	if err := e.node.Transition(setup.Clustered, "7d1f2c0e-3b2a-4d58-9c51-2f6f0a1b9e11", "prod", &id); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"grantee": "bob", "level": "database", "database": "shop", "privileges": []string{"connect"}}
	e.audit.SetFailure(errors.New("disk full"))
	w := e.do(req{method: "POST", path: "/api/v1/grants", bearer: e.token(auth.RoleAdmin), body: body})
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "audit log") {
		t.Fatalf("status = %d, want 503 about the audit log: %s", w.Code, w.Body)
	}
	// Reads are not blocked: the administrator must be able to see what is wrong.
	if w := e.do(req{method: "GET", path: "/api/v1/databases", bearer: e.token(auth.RoleReadonly)}); w.Code == http.StatusServiceUnavailable {
		t.Errorf("a read was refused because of the audit log: %s", w.Body)
	}
	e.audit.SetFailure(nil)
	w = e.do(req{method: "POST", path: "/api/v1/grants", bearer: e.token(auth.RoleAdmin), body: body})
	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("still refused after the log works again: %s", w.Body)
	}
}

// CP2-17: the log is read from the directory the server has now, not from the
// one it had at start-up.
func TestLogsFollowAMovedLogDirectory(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 9, 0, 0, 0, time.Local)
	before, after := t.TempDir(), t.TempDir()
	writeLog(t, before, t0, map[int]string{1: "before the move"})
	writeLog(t, after, t0, map[int]string{2: "after the move"})
	dir := before
	e := newEnv(t, func(d *Deps) { d.LogDir = func() string { return dir } })
	id := "s1"
	if err := e.node.Transition(setup.Clustered, "7d1f2c0e-3b2a-4d58-9c51-2f6f0a1b9e11", "prod", &id); err != nil {
		t.Fatal(err)
	}
	read := func() []string {
		w := e.do(req{method: "GET", path: "/api/v1/this-server/logs", bearer: e.token(auth.RoleOps)})
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		items, _ := out["items"].([]any)
		return messages(items)
	}
	if got := read(); !equal(got, []string{"before the move"}) {
		t.Fatalf("before = %v", got)
	}
	dir = after
	if got := read(); !equal(got, []string{"after the move"}) {
		t.Errorf("after = %v", got)
	}
}
