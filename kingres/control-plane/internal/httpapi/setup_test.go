// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

func readToken(t *testing.T, e *env) string {
	t.Helper()
	b, err := os.ReadFile(e.node.TokenPath())
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestUninitializedNodeGatesEverythingElse(t *testing.T) {
	e := newEnv(t) // a new node is uninitialized
	tok := e.token(auth.RoleAdmin)
	allowed := []string{"/api/v1/healthz", "/api/v1/readyz", "/api/v1/setup/state"}
	for _, p := range allowed {
		if w := e.do(req{method: "GET", path: p}); w.Code != 200 {
			t.Errorf("GET %s while uninitialized = %d, want 200", p, w.Code)
		}
	}
	gated := []struct{ method, path string }{
		{"GET", "/api/v1/cluster"}, {"GET", "/api/v1/databases"}, {"GET", "/api/v1/tasks"},
		{"POST", "/api/v1/auth/login"}, {"GET", "/api/v1/auth/session"}, {"POST", "/api/v1/servers"},
		{"GET", "/api/v1/audit"}, {"GET", "/api/v1/tasks/1"}, {"GET", "/api/v1/database-create-options"},
	}
	for _, g := range gated {
		// Even a valid bearer token does not help: gating comes first.
		w := e.do(req{method: g.method, path: g.path, bearer: tok})
		wantProblem(t, w, 503, "node_uninitialized")
		if w.Header().Get("Retry-After") == "" {
			t.Errorf("%s %s: missing Retry-After", g.method, g.path)
		}
	}
}

func TestSetupStateReflectsNode(t *testing.T) {
	e := newEnv(t)
	m := decode(t, e.do(req{method: "GET", path: "/api/v1/setup/state"}))
	if m["state"] != "uninitialized" || m["setupTokenRequired"] != true || m["localhostOnly"] != true {
		t.Errorf("fresh node: %v", m)
	}
	if m["address"] != "127.0.0.1:8091" || m["hostname"] != "host1" || m["version"] != "test" || m["postgresVersion"] != "18.1" {
		t.Errorf("identity fields: %v", m)
	}
	if m["serverId"] != nil || m["clusterId"] != nil {
		t.Errorf("a fresh node has no cluster identity: %v", m)
	}
	e.initialize()
	m = decode(t, e.do(req{method: "GET", path: "/api/v1/setup/state"}))
	if m["state"] != "standalone" || m["setupTokenRequired"] != false || m["localhostOnly"] != false ||
		m["clusterId"] != "7d1f2c0e-3b2a-4d58-9c51-2f6f0a1b9e11" {
		t.Errorf("standalone node: %v", m)
	}
	// PostgreSQL down: the state endpoint must still work.
	e.mem.Err = store.ErrUnavailable
	m = decode(t, e.do(req{method: "GET", path: "/api/v1/setup/state"}))
	if m["postgresVersion"] != "unknown" {
		t.Errorf("postgresVersion without PostgreSQL = %v", m["postgresVersion"])
	}
}

func TestSetupTokenChecks(t *testing.T) {
	e := newEnv(t)
	tok := readToken(t, e)
	body := map[string]any{"clusterName": "prod"}
	for _, p := range []string{"/api/v1/setup/init-cluster", "/api/v1/setup/join-cluster"} {
		// No token.
		wantProblem(t, e.do(req{method: "POST", path: p, body: body}), 401, "setup_token_invalid")
		// Wrong token.
		wantProblem(t, e.do(req{method: "POST", path: p, body: body, header: map[string]string{"X-Setup-Token": "nope"}}), 401, "setup_token_invalid")
		wantProblem(t, e.do(req{method: "POST", path: p, body: body, header: map[string]string{"X-Setup-Token": tok + "0"}}), 401, "setup_token_invalid")
		// A session is not a setup token.
		wantProblem(t, e.do(req{method: "POST", path: p, body: body, bearer: e.token(auth.RoleAdmin)}), 401, "setup_token_invalid")
		// Right token: authenticated, then the uniform "not yet" response.
		w := e.do(req{method: "POST", path: p, body: body, header: map[string]string{"X-Setup-Token": tok}})
		m := wantProblem(t, w, 422, "unsupported_operation")
		want := "not supported by this server version yet"
		if strings.HasSuffix(p, "init-cluster") {
			want = "pg-bin-dir" // no instance management is configured in these tests
		}
		if !strings.Contains(m["detail"].(string), want) {
			t.Errorf("detail = %v", m["detail"])
		}
	}
	// The token never appears in logs.
	if strings.Contains(e.logs.String(), tok) {
		t.Error("setup token leaked into the logs")
	}
}

func TestSetupTokenAttemptsAreRateLimited(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < setupFailLimit; i++ {
		wantProblem(t, e.do(req{method: "POST", path: "/api/v1/setup/init-cluster", body: map[string]any{},
			header: map[string]string{"X-Setup-Token": "guess"}}), 401, "setup_token_invalid")
	}
	// Now even the right token is refused until the window passes.
	w := e.do(req{method: "POST", path: "/api/v1/setup/init-cluster", body: map[string]any{},
		header: map[string]string{"X-Setup-Token": readToken(t, e)}})
	wantProblem(t, w, 429, "rate_limited")
}

func TestSetupEndpointsAfterInitialization(t *testing.T) {
	e := newEnv(t)
	tok := readToken(t, e)
	e.initialize()
	w := e.do(req{method: "POST", path: "/api/v1/setup/init-cluster", body: map[string]any{},
		header: map[string]string{"X-Setup-Token": tok}})
	wantProblem(t, w, 422, "node_state_invalid")
	// The consumed token file is gone.
	if _, err := os.Stat(e.node.TokenPath()); !os.IsNotExist(err) {
		t.Errorf("token file still exists after initialization: %v", err)
	}
}

// A setup token no longer opens GET /tasks/{id}: the wizard follows the setup
// task through GET /setup/state.
func TestTaskReadDoesNotAcceptSetupToken(t *testing.T) {
	e := newEnv(t)
	tok := readToken(t, e)
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/tasks/1", header: map[string]string{"X-Setup-Token": tok}}), 503, "node_uninitialized")
	e.initialize()
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/tasks/1", header: map[string]string{"X-Setup-Token": tok}}), 401, "unauthenticated")
}

func TestCheckSetupToken(t *testing.T) {
	e := newEnv(t)
	tok := readToken(t, e)
	path := "/api/v1/setup/token-check"
	// Valid: 204, no body, the token is not consumed (it works again).
	for i := 0; i < 3; i++ {
		w := e.do(req{method: "POST", path: path, header: map[string]string{"X-Setup-Token": tok}})
		if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
			t.Fatalf("valid token, call %d: %d %q", i, w.Code, w.Body.String())
		}
	}
	if e.node.State() != setup.Uninitialized {
		t.Error("checking the token must not change the node state")
	}
	// Wrong and missing tokens are setup_token_invalid.
	wantProblem(t, e.do(req{method: "POST", path: path, header: map[string]string{"X-Setup-Token": "nope"}}), 401, "setup_token_invalid")
	wantProblem(t, e.do(req{method: "POST", path: path}), 401, "setup_token_invalid")
	// A wrong token is audited as setupTokenRejected; a valid check is not audited.
	evs := e.audit.Events()
	if len(evs) != 1 || evs[0].Action != "setupTokenRejected" || evs[0].Outcome != audit.OutcomeDenied || evs[0].Actor != "anonymous" {
		t.Errorf("audit events = %+v", evs)
	}
	// Only from a local address while the panel is localhost-only.
	wantProblem(t, e.do(req{method: "POST", path: path, remote: "203.0.113.5:1000", header: map[string]string{"X-Setup-Token": tok}}), 403, "forbidden")
	// Rate limited like the other setup-token paths: failures count.
	for i := 0; i < setupFailLimit; i++ {
		e.do(req{method: "POST", path: path, header: map[string]string{"X-Setup-Token": "guess"}})
	}
	w := e.do(req{method: "POST", path: path, header: map[string]string{"X-Setup-Token": tok}})
	wantProblem(t, w, 429, "rate_limited")
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After")
	}
}

func TestCheckSetupTokenAfterInitialization(t *testing.T) {
	e := newEnv(t)
	tok := readToken(t, e)
	e.initialize()
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/setup/token-check", header: map[string]string{"X-Setup-Token": tok}}), 422, "node_state_invalid")
}

func TestLocalhostOnlyBlocksRemoteSetupCalls(t *testing.T) {
	e := newEnv(t)
	tok := readToken(t, e)
	for _, p := range []string{"/api/v1/setup/init-cluster", "/api/v1/setup/join-cluster"} {
		wantProblem(t, e.do(req{method: "POST", path: p, body: map[string]any{}, remote: "198.51.100.9:1", header: map[string]string{"X-Setup-Token": tok}}), 403, "forbidden")
	}
	// A server that listens on all interfaces accepts setup from anywhere.
	e2 := newEnv(t, func(d *Deps) { d.LoopbackOnly = false })
	tok2 := readToken(t, e2)
	wantProblem(t, e2.do(req{method: "POST", path: "/api/v1/setup/init-cluster", body: map[string]any{}, remote: "198.51.100.9:1",
		header: map[string]string{"X-Setup-Token": tok2}}), 422, "unsupported_operation")
}

func TestSetupStateReportsSetupTask(t *testing.T) {
	e := newEnv(t)
	if m := decode(t, e.do(req{method: "GET", path: "/api/v1/setup/state"})); m["setupTask"] != nil {
		t.Errorf("setupTask before any setup request = %v", m["setupTask"])
	}
	if err := e.node.SetSetupTask(api.SetupTaskSummary{ID: 5, Kind: "cluster.init", State: "failed", ProgressPercent: 30,
		Error: &api.TaskError{Code: "step_failed", Message: "no disk"}}); err != nil {
		t.Fatal(err)
	}
	m := decode(t, e.do(req{method: "GET", path: "/api/v1/setup/state"}))
	st := m["setupTask"].(map[string]any)
	if st["id"] != float64(5) || st["state"] != "failed" || st["kind"] != "cluster.init" || st["error"].(map[string]any)["code"] != "step_failed" {
		t.Errorf("setupTask = %v", st)
	}
	// The wizard still sees it after the node left uninitialized.
	e.initialize()
	if m := decode(t, e.do(req{method: "GET", path: "/api/v1/setup/state"})); m["setupTask"] == nil {
		t.Error("setupTask must survive initialization")
	}
}
