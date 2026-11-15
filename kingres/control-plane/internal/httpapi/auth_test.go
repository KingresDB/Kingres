// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

func TestLoginSuccessSetsSecureCookieAndCSRF(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "alice", "password": "s3cret"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["username"] != "alice" || m["csrfToken"] == nil || m["csrfToken"] == "" {
		t.Fatalf("unexpected body: %v", m)
	}
	if m["token"] != nil {
		t.Errorf("token must be null without issueToken, got %v", m["token"])
	}
	roles := m["clusterRoles"].([]any)
	if len(roles) != 1 || roles[0] != "kingres_admin" {
		t.Errorf("clusterRoles = %v", roles)
	}
	var ck *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("no session cookie")
	}
	if !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" || ck.Value == "" {
		t.Errorf("cookie attributes: %+v", ck)
	}
	// The cookie value is the credential, not the CSRF token.
	if ck.Value == m["csrfToken"] {
		t.Error("cookie value equals the CSRF token")
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	// The session works.
	w2 := e.do(req{method: "GET", path: "/api/v1/auth/session", cookie: ck.Value})
	if w2.Code != 200 || decode(t, w2)["csrfToken"] != m["csrfToken"] {
		t.Fatalf("session: %d %s", w2.Code, w2.Body.String())
	}
}

func TestLoginIssueToken(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "alice", "password": "s3cret", "issueToken": true}})
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	tok, _ := decode(t, w)["token"].(string)
	if tok == "" {
		t.Fatal("no token issued")
	}
	// The bearer token authenticates and needs no CSRF header for POST.
	w2 := e.do(req{method: "POST", path: "/api/v1/auth/logout", bearer: tok})
	if w2.Code != http.StatusNoContent {
		t.Fatalf("logout with bearer: %d %s", w2.Code, w2.Body.String())
	}
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/auth/session", bearer: tok}), 401, "unauthenticated")
}

func TestLoginFailures(t *testing.T) {
	tests := []struct {
		name   string
		body   any
		ctype  string
		mod    func(*env)
		status int
		code   string
	}{
		{"wrong password", map[string]any{"username": "alice", "password": "nope"}, "", nil, 401, "invalid_credentials"},
		{"unknown user", map[string]any{"username": "mallory", "password": "x"}, "", nil, 401, "invalid_credentials"},
		{"no cluster role", map[string]any{"username": "nobody", "password": "pw"}, "", nil, 403, "forbidden"},
		{"backend down", map[string]any{"username": "alice", "password": "s3cret"}, "", func(e *env) { e.fa.err = auth.ErrUnavailable }, 503, "service_unavailable"},
		{"missing password", map[string]any{"username": "alice"}, "", nil, 400, "validation_failed"},
		{"empty username", map[string]any{"username": "", "password": "x"}, "", nil, 400, "validation_failed"},
		{"unknown field", `{"username":"alice","password":"s3cret","admin":true}`, "", nil, 400, "bad_request"},
		{"not json", `{"username":`, "", nil, 400, "bad_request"},
		{"wrong type", `{"username":1,"password":"x"}`, "", nil, 400, "bad_request"},
		{"trailing data", `{"username":"alice","password":"s3cret"} {}`, "", nil, 400, "bad_request"},
		{"empty body", ``, "", nil, 400, "bad_request"},
		{"form content type", `username=alice&password=s3cret`, "application/x-www-form-urlencoded", nil, 400, "bad_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.initialize()
			if tc.mod != nil {
				tc.mod(e)
			}
			r := req{method: "POST", path: "/api/v1/auth/login", body: tc.body}
			if tc.body == "" {
				r.body = []byte{}
			}
			if tc.ctype != "" {
				r.header = map[string]string{"Content-Type": tc.ctype}
			}
			w := e.do(r)
			wantProblem(t, w, tc.status, tc.code)
			for _, c := range w.Result().Cookies() {
				if c.Name == CookieName {
					t.Error("a failed login must not set a session cookie")
				}
			}
		})
	}
}

func TestLoginBodyTooLarge(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.MaxBodyBytes = 64 })
	e.initialize()
	body := `{"username":"alice","password":"` + strings.Repeat("x", 500) + `"}`
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: body}), 400, "bad_request")
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t) // limit is 5 per minute
	e.initialize()
	bad := map[string]any{"username": "alice", "password": "wrong"}
	for i := 0; i < 5; i++ {
		wantProblem(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: bad}), 401, "invalid_credentials")
	}
	w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: bad})
	wantProblem(t, w, 429, "rate_limited")
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After")
	}
	// Even the right password is refused while the address is limited; this
	// is what stops password guessing.
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/auth/login",
		body: map[string]any{"username": "alice", "password": "s3cret"}}), 429, "rate_limited")
	calls := e.fa.calls
	// Another address is not affected by the first one's limit.
	if w := e.do(req{method: "POST", path: "/api/v1/auth/login", remote: "198.51.100.7:1",
		body: map[string]any{"username": "bob", "password": "hunter2"}}); w.Code != 200 {
		t.Fatalf("other address: %d %s", w.Code, w.Body.String())
	}
	if e.fa.calls != calls+1 {
		t.Error("rate-limited attempts must not reach PostgreSQL")
	}
	// After the window the address is admitted again.
	e.clock.Advance(61 * time.Second)
	if w := e.do(req{method: "POST", path: "/api/v1/auth/login",
		body: map[string]any{"username": "alice", "password": "s3cret"}}); w.Code != 200 {
		t.Fatalf("after window: %d %s", w.Code, w.Body.String())
	}
}

func TestLoginRateLimitPerUserAcrossAddresses(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	bad := map[string]any{"username": "alice", "password": "wrong"}
	for i := 0; i < 5; i++ {
		addr := "198.51.100." + string(rune('1'+i)) + ":1"
		wantProblem(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: bad, remote: addr}), 401, "invalid_credentials")
	}
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: bad, remote: "203.0.113.9:1"}), 429, "rate_limited")
}

func TestCSRFAndSessionLifecycle(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	cookie, csrf := e.cookieSession(auth.RoleAdmin)

	// A mutating request with the cookie but without or with a wrong CSRF
	// token is refused before the role check and before any handler.
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/databases", cookie: cookie, body: map[string]any{"name": "x"}}), 403, "csrf_token_invalid")
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/databases", cookie: cookie, body: map[string]any{"name": "x"},
		header: map[string]string{"X-CSRF-Token": csrf + "x"}}), 403, "csrf_token_invalid")
	// A read does not need the token.
	if w := e.do(req{method: "GET", path: "/api/v1/databases", cookie: cookie}); w.Code != 200 {
		t.Fatalf("GET with cookie: %d", w.Code)
	}
	// With the token the request reaches the (not implemented) handler.
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/databases", cookie: cookie, body: map[string]any{"name": "x"},
		header: map[string]string{"X-CSRF-Token": csrf}}), 422, "unsupported_operation")
	// Cross-site requests are refused even with a valid token.
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/databases", cookie: cookie, body: map[string]any{"name": "x"},
		header: map[string]string{"X-CSRF-Token": csrf, "Sec-Fetch-Site": "cross-site"}}), 403, "csrf_token_invalid")

	// Logout needs the token too, revokes the session and clears the cookie.
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/auth/logout", cookie: cookie}), 403, "csrf_token_invalid")
	w := e.do(req{method: "POST", path: "/api/v1/auth/logout", cookie: cookie, header: map[string]string{"X-CSRF-Token": csrf}})
	if w.Code != 204 {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout must clear the cookie")
	}
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/auth/session", cookie: cookie}), 401, "unauthenticated")
}

func TestSessionExpiry(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	tok := e.token(auth.RoleReadonly)
	if w := e.do(req{method: "GET", path: "/api/v1/auth/session", bearer: tok}); w.Code != 200 {
		t.Fatalf("fresh session: %d", w.Code)
	}
	e.clock.Advance(61 * time.Minute)
	w := e.do(req{method: "GET", path: "/api/v1/auth/session", bearer: tok})
	wantProblem(t, w, 401, "session_expired")
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 needs WWW-Authenticate")
	}
}

func TestCredentialKindsAreNotInterchangeable(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	cookie, _ := e.cookieSession(auth.RoleAdmin)
	// A cookie value presented as a bearer token would skip CSRF checks.
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/databases", bearer: cookie, body: map[string]any{"name": "x"}}), 401, "unauthenticated")
	tok := e.token(auth.RoleAdmin)
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases", cookie: tok}), 401, "unauthenticated")
	// Malformed Authorization headers.
	for _, h := range []string{"Basic abc", "Bearer", "Bearer  ", "token"} {
		wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases", header: map[string]string{"Authorization": h}}), 401, "unauthenticated")
	}
	// No credentials at all.
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases"}), 401, "unauthenticated")
}

func TestSessionCapacity(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	small := auth.NewStore(time.Hour, 1, e.clock.Now)
	e.srv.deps.Sessions = small
	ok := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "alice", "password": "s3cret"}})
	if ok.Code != 200 {
		t.Fatalf("first login: %d", ok.Code)
	}
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/auth/login", remote: "198.51.100.1:1",
		body: map[string]any{"username": "bob", "password": "hunter2"}}), 503, "service_unavailable")
}

func TestAuditOfAuthenticationEvents(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "alice", "password": "wrong-password-xyz"}})
	e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "nobody", "password": "pw"}})
	e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "alice", "password": "s3cret"},
		header: map[string]string{"X-Request-Id": "req-42"}})
	evs := e.audit.Events()
	if len(evs) != 3 {
		t.Fatalf("events = %d: %+v", len(evs), evs)
	}
	want := []struct{ actor, outcome, action string }{
		{"alice", audit.OutcomeFailure, "loginFailed"}, {"nobody", audit.OutcomeDenied, "loginFailed"}, {"alice", audit.OutcomeSuccess, "login"}}
	for i, w := range want {
		if evs[i].Actor != w.actor || evs[i].Outcome != w.outcome || evs[i].Action != w.action {
			t.Errorf("event %d = %+v, want actor %s outcome %s", i, evs[i], w.actor, w.outcome)
		}
	}
	if evs[2].RequestID != "req-42" || evs[2].ActorAddress != "127.0.0.1" {
		t.Errorf("request id / address not recorded: %+v", evs[2])
	}
	// No secrets anywhere: audit events and logs.
	dump := e.logs.String()
	for _, ev := range evs {
		dump += ev.Action + ev.Actor + ev.ResourceID
		for k, v := range ev.Details {
			dump += k + "=" + strings.TrimSpace(strings.Join([]string{toString(v)}, ""))
		}
	}
	for _, secret := range []string{"wrong-password-xyz", "s3cret"} {
		if strings.Contains(dump, secret) {
			t.Errorf("secret %q leaked into logs or audit events", secret)
		}
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestSessionCarriesClusterIdentityForEveryRole(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	h := "db1.example.com"
	e.mem.LocalServer = &h
	// Even kingres_stats, which reaches nothing else, gets the identity.
	for _, role := range []string{auth.RoleStats, auth.RoleAdmin} {
		tok := e.token(role)
		m := decode(t, e.do(req{method: "GET", path: "/api/v1/auth/session", bearer: tok}))
		if m["clusterId"] != "7d1f2c0e-3b2a-4d58-9c51-2f6f0a1b9e11" || m["clusterName"] != "prod" || m["serverId"] != h {
			t.Errorf("%s: session = %v", role, m)
		}
		if m["csrfToken"] != nil {
			t.Errorf("a bearer session has no csrf token: %v", m["csrfToken"])
		}
	}
	// The CSRF token is fixed for the session: GET /auth/session does not rotate it.
	cookie, csrf := e.cookieSession(auth.RoleReadonly)
	for i := 0; i < 3; i++ {
		if got := decode(t, e.do(req{method: "GET", path: "/api/v1/auth/session", cookie: cookie}))["csrfToken"]; got != csrf {
			t.Fatalf("csrf token changed: %v != %v", got, csrf)
		}
	}
	// A node outside a cluster reports nulls (and keeps working without PostgreSQL).
	e2 := newEnv(t)
	e2.initialize()
	e2.node.Transition(setup.Clustered, "", "", nil)
	e2.mem.Err = store.ErrUnavailable
	m := decode(t, e2.do(req{method: "GET", path: "/api/v1/auth/session", bearer: e2.token(auth.RoleStats)}))
	if m["serverId"] != nil || m["clusterName"] == nil {
		t.Errorf("session without catalog = %v", m)
	}
}
