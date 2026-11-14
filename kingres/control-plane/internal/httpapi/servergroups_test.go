// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// groupEnv is an environment whose Ops is present but has no database: a
// request that passes validation reaches the store and answers 422
// unsupported_operation, which proves how far it got.
func groupEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, func(d *Deps) { d.Ops = &ops.Ops{} })
	e.initialize()
	h := "10.0.0.1"
	e.mem.ServerList = []api.Server{{ID: h, Address: h + ":8091", Hostname: h, Status: "live", ShouldHaveShards: true, Instances: []api.Instance{}}}
	return e
}

func fieldsOf(m map[string]any) map[string]string {
	out := map[string]string{}
	errs, _ := m["errors"].([]any)
	for _, e := range errs {
		f := e.(map[string]any)
		out[f["field"].(string)] = f["code"].(string)
	}
	return out
}

func TestCreateServerGroupValidatesTheBody(t *testing.T) {
	e := groupEnv(t)
	ops := e.token(auth.RoleOps)
	long := make([]byte, 201)
	for i := range long {
		long[i] = 'x'
	}
	for name, tc := range map[string]struct {
		body  any
		field string
	}{
		"missing name":      {map[string]any{}, "/name"},
		"bad pattern":       {map[string]any{"name": "-rack"}, "/name"},
		"slash":             {map[string]any{"name": "a/b"}, "/name"},
		"too long":          {map[string]any{"name": "a123456789012345678901234567890123456789012345678901234567890123"}, "/name"},
		"name not a string": {map[string]any{"name": 5}, "/name"},
		"null name":         {map[string]any{"name": nil}, "/name"},
		"long description":  {map[string]any{"name": "ok", "description": string(long)}, "/description"},
		"unknown member":    {map[string]any{"name": "ok", "color": "red"}, "/color"},
	} {
		m := wantProblem(t, e.do(req{method: "POST", path: "/api/v1/server-groups", bearer: ops, body: tc.body}), 400, "validation_failed")
		if _, ok := fieldsOf(m)[tc.field]; !ok {
			t.Errorf("%s: errors %v do not name %s", name, m["errors"], tc.field)
		}
	}
	// Not a JSON object, empty, wrong media type.
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/server-groups", bearer: ops, body: "[1]"}), 400, "bad_request")
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/server-groups", bearer: ops, body: "null"}), 400, "bad_request")
	// A valid body, with a description of exactly 200 multi-byte characters, passes validation.
	desc := ""
	for i := 0; i < 200; i++ {
		desc += "é"
	}
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/server-groups", bearer: ops, body: map[string]any{"name": "Rack-A_1.x", "description": desc}}), 422, "unsupported_operation")
}

func TestServerGroupOperationsNeedTheOpsRole(t *testing.T) {
	e := groupEnv(t)
	ro := e.token(auth.RoleReadonly)
	body := map[string]any{"name": "rack-a"}
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/server-groups", bearer: ro, body: body}), 403, "forbidden")
	wantProblem(t, e.do(req{method: "DELETE", path: "/api/v1/server-groups/rack-a", bearer: ro}), 403, "forbidden")
	wantProblem(t, e.do(req{method: "PATCH", path: "/api/v1/servers/10.0.0.1", bearer: ro, body: map[string]any{"serverGroup": nil}}), 403, "forbidden")
	// The list needs the read-only role only.
	if w := e.do(req{method: "GET", path: "/api/v1/server-groups", bearer: ro}); w.Code != 200 {
		t.Errorf("list: %d", w.Code)
	}
}

func TestDeleteServerGroupUnknownNameIsNotFound(t *testing.T) {
	e := groupEnv(t)
	// A name that can never exist is 404 without asking the database.
	wantProblem(t, e.do(req{method: "DELETE", path: "/api/v1/server-groups/-bad", bearer: e.token(auth.RoleOps)}), 404, "not_found")
	wantProblem(t, e.do(req{method: "DELETE", path: "/api/v1/server-groups/rack-a", bearer: e.token(auth.RoleOps)}), 422, "unsupported_operation")
}

func TestUpdateServerValidation(t *testing.T) {
	e := groupEnv(t)
	tok := e.token(auth.RoleOps)
	for name, tc := range map[string]struct {
		body  any
		field string
	}{
		"empty body object":   {map[string]any{}, "/"},
		"read-only status":    {map[string]any{"status": "live"}, "/status"},
		"read-only with ok":   {map[string]any{"shouldHaveShards": true, "hostname": "x"}, "/hostname"},
		"unknown":             {map[string]any{"color": 1}, "/color"},
		"group not a string":  {map[string]any{"serverGroup": 7}, "/serverGroup"},
		"shards not a bool":   {map[string]any{"shouldHaveShards": "yes"}, "/shouldHaveShards"},
		"shards null":         {map[string]any{"shouldHaveShards": nil}, "/shouldHaveShards"},
		"read-only certified": {map[string]any{"certificateExpiresAt": nil}, "/certificateExpiresAt"},
	} {
		m := wantProblem(t, e.do(req{method: "PATCH", path: "/api/v1/servers/10.0.0.1", bearer: tok, body: tc.body}), 400, "validation_failed")
		if _, ok := fieldsOf(m)[tc.field]; !ok {
			t.Errorf("%s: errors %v do not name %s", name, m["errors"], tc.field)
		}
	}
	wantProblem(t, e.do(req{method: "PATCH", path: "/api/v1/servers/10.0.0.1", bearer: tok}), 400, "bad_request")
	// A valid update of an unknown server is 404; of a known one it reaches the store.
	wantProblem(t, e.do(req{method: "PATCH", path: "/api/v1/servers/10.9.9.9", bearer: tok, body: map[string]any{"serverGroup": nil}}), 404, "not_found")
	wantProblem(t, e.do(req{method: "PATCH", path: "/api/v1/servers/10.0.0.1", bearer: tok, body: map[string]any{"serverGroup": nil}}), 422, "unsupported_operation")
}

// A follower asks the leader for the part of the server list only the leader
// holds and merges it into its own answer.
func TestFollowerTakesServerRecordsFromTheLeader(t *testing.T) {
	fw := &direct{leader: "10.0.0.1"}
	leader, follower := twoServers(t, fw)
	leader.testUser([]string{auth.RoleReadonly})
	seen := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	exp := time.Date(2027, 5, 1, 12, 0, 0, 0, time.UTC)
	grp := "rack-a"
	mk := func(group *string, s *string, pinned bool, cert *time.Time) []api.Server {
		return []api.Server{
			{ID: "10.0.0.1", Address: "10.0.0.1:8091", Status: "live", ServerGroup: group, SeenFingerprint: s, FingerprintPinned: pinned, CertificateExpiresAt: cert, Instances: []api.Instance{}},
			{ID: "10.0.0.2", Address: "10.0.0.2:8091", Status: "live", Instances: []api.Instance{}},
		}
	}
	leader.mem.ServerList = mk(&grp, nil, false, &exp)
	leader.mem.ServerList[1].ServerGroup, leader.mem.ServerList[1].SeenFingerprint, leader.mem.ServerList[1].FingerprintPinned = &grp, &seen, true
	follower.mem.ServerList = mk(nil, nil, false, nil)
	local := "10.0.0.2"
	follower.mem.LocalServer = &local

	w := follower.do(req{method: "GET", path: "/api/v1/servers", bearer: follower.token(auth.RoleReadonly)})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if fw.calls != 1 || fw.lastReq.OperationID != "listServers" {
		t.Fatalf("forwarded %d calls, last %+v", fw.calls, fw.lastReq)
	}
	items := decode(t, w)["items"].([]any)
	s1, s2 := items[0].(map[string]any), items[1].(map[string]any)
	if s1["serverGroup"] != "rack-a" || s1["seenFingerprint"] != nil || s1["fingerprintPinned"] != false || s1["certificateExpiresAt"] != "2027-05-01T12:00:00Z" {
		t.Errorf("server 1 = %v", s1)
	}
	if s2["serverGroup"] != "rack-a" || s2["seenFingerprint"] != seen || s2["fingerprintPinned"] != true || s2["certificateExpiresAt"] != nil {
		t.Errorf("server 2 = %v", s2)
	}
	// getServer takes the same values.
	g := decode(t, follower.do(req{method: "GET", path: "/api/v1/servers/10.0.0.2", bearer: follower.token(auth.RoleReadonly)}))
	if g["serverGroup"] != "rack-a" || g["seenFingerprint"] != seen || g["fingerprintPinned"] != true {
		t.Errorf("getServer = %v", g)
	}
	// The group list is the leader's too.
	leader.mem.Groups = []api.ServerGroup{{Name: "rack-a", Description: "d", ServerIDs: []string{"10.0.0.1", "10.0.0.2"}}}
	gl := decode(t, follower.do(req{method: "GET", path: "/api/v1/server-groups", bearer: follower.token(auth.RoleReadonly)}))
	if items := gl["items"].([]any); len(items) != 1 || items[0].(map[string]any)["description"] != "d" {
		t.Errorf("server groups = %v", gl)
	}
	// A leader that cannot be reached does not make the read fail.
	fw.err = nil
	fw.leader = ""
	w = follower.do(req{method: "GET", path: "/api/v1/servers", bearer: follower.token(auth.RoleReadonly)})
	if w.Code != 200 {
		t.Errorf("unreachable leader: status %d", w.Code)
	}
}

func TestListServersOnTheLeaderDoesNotForward(t *testing.T) {
	fw := &direct{leader: "me", self: true}
	_, follower := twoServers(t, fw)
	follower.do(req{method: "GET", path: "/api/v1/servers", bearer: follower.token(auth.RoleReadonly)})
	if fw.calls != 0 {
		t.Errorf("the leader forwarded %d reads to itself", fw.calls)
	}
}

func TestLeaderServesOnlyTheListedForwardedReads(t *testing.T) {
	fw := &direct{leader: "x"}
	leader, _ := twoServers(t, fw)
	id := internalapi.ForwardIdentity{Username: "bob", ClusterRoles: []string{auth.RoleReadonly}}
	for op, want := range map[string]bool{"listServers": true, "listServerGroups": true, "listDatabases": false, "getServer": false} {
		pp := map[string]string{}
		if op == "getServer" {
			pp["serverId"] = "10.0.0.1"
		}
		_, err := leader.srv.ServeForwarded(t.Context(), "p", internalapi.ForwardRequest{OperationID: op, PathParams: pp, Identity: id})
		if (err == nil) != want {
			t.Errorf("%s: accepted = %v, want %v (%v)", op, err == nil, want, err)
		}
	}
}

func TestLocalServerCertificateExpiryComesFromItsFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	crt, _, err := pki.NewBootstrapCert(pki.Names{CommonName: "10.0.0.1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pki"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pki", pki.ServerCertFile), crt, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := pki.ParseCertPEM(crt)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(d *Deps) { d.DataDir = dir })
	e.initialize()
	local := "10.0.0.1"
	e.mem.LocalServer = &local
	e.mem.ServerList = []api.Server{
		{ID: "10.0.0.1", Instances: []api.Instance{}, Status: "live"},
		{ID: "10.0.0.2", Instances: []api.Instance{}, Status: "live"},
	}
	items := decode(t, e.do(req{method: "GET", path: "/api/v1/servers", bearer: e.token(auth.RoleReadonly)}))["items"].([]any)
	if got := items[0].(map[string]any)["certificateExpiresAt"]; got != c.NotAfter.UTC().Format(time.RFC3339) {
		t.Errorf("local certificateExpiresAt = %v, want %v", got, c.NotAfter.UTC().Format(time.RFC3339))
	}
	if got := items[1].(map[string]any)["certificateExpiresAt"]; got != nil {
		t.Errorf("remote certificateExpiresAt = %v, want null", got)
	}
	// Without the file the value is null, not an error.
	if err := os.Remove(filepath.Join(dir, "pki", pki.ServerCertFile)); err != nil {
		t.Fatal(err)
	}
	items = decode(t, e.do(req{method: "GET", path: "/api/v1/servers", bearer: e.token(auth.RoleReadonly)}))["items"].([]any)
	if got := items[0].(map[string]any)["certificateExpiresAt"]; got != nil {
		t.Errorf("without the file: %v", got)
	}
}

func TestServersAnswerHasTheThreeNewProperties(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	h := "10.0.0.1"
	e.mem.ServerList = []api.Server{{ID: h, Instances: []api.Instance{}, Status: "live"}}
	items := decode(t, e.do(req{method: "GET", path: "/api/v1/servers", bearer: e.token(auth.RoleReadonly)}))["items"].([]any)
	s := items[0].(map[string]any)
	for _, k := range []string{"certificateExpiresAt", "seenFingerprint", "fingerprintPinned"} {
		if _, ok := s[k]; !ok {
			t.Errorf("property %s is missing", k)
		}
	}
	if s["fingerprintPinned"] != false {
		t.Errorf("fingerprintPinned = %v", s["fingerprintPinned"])
	}
	_ = http.StatusOK
}
