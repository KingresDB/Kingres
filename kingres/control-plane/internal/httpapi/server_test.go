// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

func TestHealthAndReadiness(t *testing.T) {
	e := newEnv(t)
	w := e.do(req{method: "GET", path: "/api/v1/healthz"})
	if w.Code != 200 || decode(t, w)["status"] != "ok" {
		t.Fatalf("healthz: %d %s", w.Code, w.Body.String())
	}
	// Uninitialized and without PostgreSQL: still ready, the check is false.
	e.mem.Err = store.ErrUnavailable
	w = e.do(req{method: "GET", path: "/api/v1/readyz"})
	m := decode(t, w)
	if w.Code != 200 || m["status"] != "ready" || m["nodeState"] != "uninitialized" {
		t.Fatalf("readyz uninitialized: %d %s", w.Code, w.Body.String())
	}
	if c := m["checks"].([]any)[0].(map[string]any); c["ok"] != false || c["name"] != "localPostgres" {
		t.Errorf("check = %v", c)
	}
	// Standalone without PostgreSQL: not ready.
	e.initialize()
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/readyz"}), 503, "service_unavailable")
	// With PostgreSQL: ready.
	e.mem.Err = nil
	if w := e.do(req{method: "GET", path: "/api/v1/readyz"}); w.Code != 200 || decode(t, w)["nodeState"] != "standalone" {
		t.Fatalf("readyz standalone: %d %s", w.Code, w.Body.String())
	}
	// Shutting down: not ready, liveness unaffected.
	e.srv.SetReady(false)
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/readyz"}), 503, "service_unavailable")
	if w := e.do(req{method: "GET", path: "/api/v1/healthz"}); w.Code != 200 {
		t.Errorf("healthz while stopping: %d", w.Code)
	}
}

func TestPublicEndpointsAreRateLimited(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < publicRateLimit; i++ {
		if w := e.do(req{method: "GET", path: "/api/v1/healthz"}); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	w := e.do(req{method: "GET", path: "/api/v1/healthz"})
	wantProblem(t, w, 429, "rate_limited")
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After")
	}
}

func TestUnknownRoutesAreProblems(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/v1/nope", "/api/v2/servers", "/api", "/api/", "/api/v1/", "/api/v1/servers/1/extra", "/api/v1/servers/"} {
		m := wantProblem(t, e.do(req{method: "GET", path: p}), 404, "not_found")
		if m["instance"] != strings.TrimSuffix(p, "") && !strings.HasPrefix(p, m["instance"].(string)) {
			t.Errorf("instance %v for path %s", m["instance"], p)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	e := newEnv(t)
	w := e.do(req{method: "DELETE", path: "/api/v1/healthz"})
	wantProblem(t, w, 405, "method_not_allowed")
	if w.Header().Get("Allow") != "GET" {
		t.Errorf("Allow = %q", w.Header().Get("Allow"))
	}
	w = e.do(req{method: "PUT", path: "/api/v1/servers"})
	if got := w.Header().Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow = %q", got)
	}
}

func TestRequestID(t *testing.T) {
	e := newEnv(t)
	hex := regexp.MustCompile(`^[0-9a-f]{24}$`)
	w := e.do(req{method: "GET", path: "/api/v1/healthz"})
	if id := w.Header().Get("X-Request-Id"); !hex.MatchString(id) {
		t.Errorf("generated id = %q", id)
	}
	w = e.do(req{method: "GET", path: "/api/v1/healthz", header: map[string]string{"X-Request-Id": "trace-1.a_b"}})
	if w.Header().Get("X-Request-Id") != "trace-1.a_b" {
		t.Errorf("valid incoming id was not kept: %q", w.Header().Get("X-Request-Id"))
	}
	w = e.do(req{method: "GET", path: "/api/v1/healthz", header: map[string]string{"X-Request-Id": "bad id\twith space"}})
	if id := w.Header().Get("X-Request-Id"); !hex.MatchString(id) {
		t.Errorf("invalid incoming id was kept: %q", id)
	}
	// The id is copied into the problem body.
	w = e.do(req{method: "GET", path: "/api/v1/nope", header: map[string]string{"X-Request-Id": "abc123"}})
	if m := decode(t, w); m["requestId"] != "abc123" {
		t.Errorf("requestId = %v", m["requestId"])
	}
}

func TestAccessLog(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	tok := e.token(auth.RoleAdmin)
	e.do(req{method: "GET", path: "/api/v1/tasks?limit=5", bearer: tok, header: map[string]string{"X-Request-Id": "log-1"}})
	logs := e.logs.String()
	for _, want := range []string{"msg=request", "request_id=log-1", "path=/api/v1/tasks", "status=200", "operation=listTasks", "actor=tester"} {
		if !strings.Contains(logs, want) {
			t.Errorf("access log lacks %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, tok) || strings.Contains(logs, "limit=5") {
		t.Error("access log leaked the credential or the query string")
	}
}

func TestPanicRecovery(t *testing.T) {
	e := newEnv(t)
	h := e.srv.chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	m := wantProblem(t, w, 500, "internal_error")
	if strings.Contains(w.Body.String(), "boom") {
		t.Error("panic value leaked into the response")
	}
	if m["requestId"] == "" || m["requestId"] == nil {
		t.Error("500 problem lacks the request id")
	}
	if !strings.Contains(e.logs.String(), "panic in handler") || !strings.Contains(e.logs.String(), "boom") {
		t.Error("panic was not logged")
	}
}

func TestUnknownQueryParameter(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	tok := e.token(auth.RoleReadonly)
	m := wantProblem(t, e.do(req{method: "GET", path: "/api/v1/servers?bogus=1", bearer: tok}), 400, "validation_failed")
	errs := m["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["field"] != "bogus" {
		t.Errorf("errors = %v", errs)
	}
	// Unknown parameters on public operations are rejected as well.
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/healthz?x=1"}), 400, "validation_failed")
}

func TestAuditOfMutatingRequests(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	admin := e.token(auth.RoleAdmin)
	ro := e.token(auth.RoleReadonly)

	e.do(req{method: "POST", path: "/api/v1/databases", bearer: admin, body: map[string]any{"name": "shop"},
		header: map[string]string{"X-Request-Id": "r-1"}})
	e.do(req{method: "DELETE", path: "/api/v1/databases/shop", bearer: ro})
	e.do(req{method: "GET", path: "/api/v1/databases", bearer: admin})                      // not mutating
	e.do(req{method: "POST", path: "/api/v1/databases", body: map[string]any{"name": "x"}}) // unauthenticated

	evs := e.audit.Events()
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(evs), evs)
	}
	if ev := evs[0]; ev.Action != "createDatabase" || ev.Actor != "tester" || ev.Outcome != audit.OutcomeFailure ||
		ev.RequestID != "r-1" || ev.Source != "cli" || ev.Details["operationId"] != "createDatabase" || ev.Details["status"] != 422 {
		t.Errorf("event 0 = %+v", ev)
	}
	if ev := evs[1]; ev.Action != "dropDatabase" || ev.Outcome != audit.OutcomeDenied || ev.ResourceType != "database" || ev.ResourceID != "shop" {
		t.Errorf("event 1 = %+v", ev)
	}
}

func TestStoreErrorsBecomeProblems(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	tok := e.token(auth.RoleAdmin)
	e.mem.Err = store.ErrUnavailable
	w := e.do(req{method: "GET", path: "/api/v1/servers", bearer: tok})
	wantProblem(t, w, 503, "service_unavailable")
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After")
	}
	e.mem.Err = http.ErrAbortHandler // any unexpected error
	m := wantProblem(t, e.do(req{method: "GET", path: "/api/v1/servers", bearer: tok}), 500, "internal_error")
	if strings.Contains(m["detail"].(string), "abort") {
		t.Error("internal error detail leaks the cause")
	}
}

func TestReadEndpointsWithSeedData(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	tok := e.token(auth.RoleAdmin)
	e.mem.DBs = []api.Database{{Name: "shop"}, {Name: "app"}}
	e.mem.TaskList = []api.Task{{ID: 1}, {ID: 2}, {ID: 3}}
	for i := 0; i < 7; i++ {
		e.mem.RoleList = append(e.mem.RoleList, api.Role{Name: "role" + string(rune('a'+i)), CanLogin: i%2 == 0})
	}

	t.Run("pagination walks all pages", func(t *testing.T) {
		var names []string
		cursor := ""
		for i := 0; i < 10; i++ {
			p := "/api/v1/roles?limit=3"
			if cursor != "" {
				p += "&cursor=" + cursor
			}
			m := decode(t, e.do(req{method: "GET", path: p, bearer: tok}))
			for _, it := range m["items"].([]any) {
				names = append(names, it.(map[string]any)["name"].(string))
			}
			if m["nextCursor"] == nil {
				break
			}
			cursor = m["nextCursor"].(string)
		}
		if len(names) != 7 {
			t.Errorf("walked %d roles: %v", len(names), names)
		}
	})
	t.Run("newest first tasks with keyset cursor", func(t *testing.T) {
		m := decode(t, e.do(req{method: "GET", path: "/api/v1/tasks?limit=2", bearer: tok}))
		items := m["items"].([]any)
		if len(items) != 2 || items[0].(map[string]any)["id"].(float64) != 3 || m["nextCursor"] == nil {
			t.Fatalf("page 1: %v", m)
		}
		m = decode(t, e.do(req{method: "GET", path: "/api/v1/tasks?limit=2&cursor=" + m["nextCursor"].(string), bearer: tok}))
		if items := m["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"].(float64) != 1 || m["nextCursor"] != nil {
			t.Fatalf("page 2: %v", m)
		}
	})
	t.Run("filters", func(t *testing.T) {
		m := decode(t, e.do(req{method: "GET", path: "/api/v1/roles?canLogin=false", bearer: tok}))
		if n := len(m["items"].([]any)); n != 3 {
			t.Errorf("canLogin=false returned %d roles", n)
		}
		m = decode(t, e.do(req{method: "GET", path: "/api/v1/roles?search=ROLEC", bearer: tok}))
		if n := len(m["items"].([]any)); n != 1 {
			t.Errorf("search returned %d roles", n)
		}
	})
	t.Run("invalid parameters", func(t *testing.T) {
		for _, q := range []string{"/api/v1/roles?limit=0", "/api/v1/roles?limit=501", "/api/v1/roles?limit=x",
			"/api/v1/roles?canLogin=maybe", "/api/v1/tasks?state=weird", "/api/v1/tasks?kind=nope",
			"/api/v1/grants?table=t", "/api/v1/audit?from=yesterday", "/api/v1/audit?outcome=meh"} {
			wantProblem(t, e.do(req{method: "GET", path: q, bearer: tok}), 400, "validation_failed")
		}
		m := wantProblem(t, e.do(req{method: "GET", path: "/api/v1/roles?limit=0&canLogin=maybe", bearer: tok}), 400, "validation_failed")
		if n := len(m["errors"].([]any)); n != 2 {
			t.Errorf("both problems should be reported, got %d", n)
		}
		wantProblem(t, e.do(req{method: "GET", path: "/api/v1/roles?cursor=garbage", bearer: tok}), 400, "validation_failed")
		wantProblem(t, e.do(req{method: "GET", path: "/api/v1/servers/" + strings.Repeat("h", 256), bearer: tok}), 400, "validation_failed")
	})
	t.Run("not found", func(t *testing.T) {
		for _, p := range []string{"/api/v1/servers/no-such-host", "/api/v1/databases/none", "/api/v1/databases/none/tables",
			"/api/v1/databases/none/ledger", "/api/v1/roles/none", "/api/v1/tasks/99",
			"/api/v1/databases/shop/schemas/public/tables/none", "/api/v1/this-server/instances/5",
			"/api/v1/databases/shop/colocation-groups/9/buckets", "/api/v1/databases/none/bucket-distribution"} {
			wantProblem(t, e.do(req{method: "GET", path: p, bearer: tok}), 404, "not_found")
		}
	})
	t.Run("static cluster roles", func(t *testing.T) {
		m := decode(t, e.do(req{method: "GET", path: "/api/v1/cluster-roles", bearer: tok}))
		items := m["items"].([]any)
		if len(items) != 4 || items[0].(map[string]any)["name"] != "kingres_admin" {
			t.Errorf("cluster roles = %v", items)
		}
	})
	t.Run("audit list", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			e.do(req{method: "POST", path: "/api/v1/databases", bearer: tok, body: map[string]any{}})
		}
		m := decode(t, e.do(req{method: "GET", path: "/api/v1/audit?limit=2&action=createDatabase", bearer: tok}))
		items := m["items"].([]any)
		if len(items) != 2 || items[0].(map[string]any)["id"].(float64) != 3 || m["nextCursor"] == nil {
			t.Fatalf("audit page: %v", m)
		}
		m = decode(t, e.do(req{method: "GET", path: "/api/v1/audit?limit=2&action=createDatabase&cursor=" + m["nextCursor"].(string), bearer: tok}))
		if len(m["items"].([]any)) != 1 || m["nextCursor"] != nil {
			t.Fatalf("audit page 2: %v", m)
		}
	})
}

func TestAuditActionFilterUsesTheContractEnum(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	tok := e.token(auth.RoleAdmin)
	for _, a := range []string{"login", "loginFailed", "setupTokenRejected", "autoFailover", "createDatabase"} {
		if w := e.do(req{method: "GET", path: "/api/v1/audit?action=" + a, bearer: tok}); w.Code != 200 {
			t.Errorf("action=%s: %d %s", a, w.Code, w.Body.String())
		}
	}
	for _, a := range []string{"database.create", "getHealth", "bogus"} {
		wantProblem(t, e.do(req{method: "GET", path: "/api/v1/audit?action=" + a, bearer: tok}), 400, "validation_failed")
	}
}

// Only the operations of the AuditAction enum are audited; for example
// previewRebalancePlan and createInclusionProof are POSTs that are not.
func TestOnlyAuditActionsAreAudited(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	tok := e.token(auth.RoleAdmin)
	e.do(req{method: "POST", path: "/api/v1/rebalance/plan", bearer: tok, body: map[string]any{}})
	e.do(req{method: "POST", path: "/api/v1/databases/x/ledger/inclusion-proofs", bearer: tok, body: map[string]any{}})
	if n := len(e.audit.Events()); n != 0 {
		t.Fatalf("%d events for non-audited operations: %+v", n, e.audit.Events())
	}
	e.do(req{method: "POST", path: "/api/v1/query", bearer: tok, body: map[string]any{}}) // executeQuery is audited
	if evs := e.audit.Events(); len(evs) != 1 || evs[0].Action != "executeQuery" {
		t.Errorf("events = %+v", evs)
	}
	// Every recorded action is a value of the enum.
	for _, ev := range e.audit.Events() {
		if !contract.IsAuditAction(ev.Action) {
			t.Errorf("action %q is not in the contract enum", ev.Action)
		}
	}
}

func TestMethodNotAllowedBeforeAuthentication(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	// No credentials at all: still 405, not 401.
	w := e.do(req{method: "DELETE", path: "/api/v1/servers"})
	wantProblem(t, w, 405, "method_not_allowed")
	if w.Header().Get("Allow") != "GET, POST" {
		t.Errorf("Allow = %q", w.Header().Get("Allow"))
	}
}
