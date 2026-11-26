// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
)

// loginToken signs in as alice and returns a bearer token.
func loginToken(t *testing.T, e *env, user, pass string) string {
	t.Helper()
	w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": user, "password": pass, "issueToken": true}})
	if w.Code != 200 {
		t.Fatalf("login %s: %d %s", user, w.Code, w.Body.String())
	}
	tok, _ := decode(t, w)["token"].(string)
	if tok == "" {
		t.Fatal("no token")
	}
	return tok
}

func (e *env) setUser(name string, u *fakeUser) {
	e.fa.mu.Lock()
	defer e.fa.mu.Unlock()
	if u == nil {
		delete(e.fa.users, name)
		return
	}
	e.fa.users[name] = *u
}

// CP-02: authorization follows the current state of the role, not the state
// at sign-in.
func TestSessionsFollowTheCurrentStateOfTheRole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(e *env)
		want   int
	}{
		{"demoted to readonly", func(e *env) { e.setUser("alice", &fakeUser{"s3cret", []string{auth.RoleReadonly}}) }, http.StatusForbidden},
		{"loses every cluster role", func(e *env) { e.setUser("alice", &fakeUser{"s3cret", nil}) }, http.StatusUnauthorized},
		{"dropped, disabled or expired", func(e *env) { e.setUser("alice", nil) }, http.StatusUnauthorized},
		{"backend unavailable fails closed", func(e *env) { e.fa.mu.Lock(); e.fa.err = auth.ErrUnavailable; e.fa.mu.Unlock() }, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.initialize()
			tok := loginToken(t, e, "alice", "s3cret")
			if w := e.do(req{method: "GET", path: "/api/v1/audit", bearer: tok}); w.Code != 200 {
				t.Fatalf("before the change: %d %s", w.Code, w.Body.String())
			}
			tc.change(e)
			// Within the cache time a change made outside the API may still be unseen;
			// after it, it must be seen.
			e.clock.Advance(auth.DefaultRevalidateTTL + time.Second)
			if w := e.do(req{method: "GET", path: "/api/v1/audit", bearer: tok}); w.Code != tc.want {
				t.Fatalf("after the change: %d %s, want %d", w.Code, w.Body.String(), tc.want)
			}
		})
	}
}

// A change made through the API ends the role's sessions at once, with no
// waiting for the cache; other roles' sessions stay.
func TestRoleChangedEndsItsSessionsAtOnce(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	alice := loginToken(t, e, "alice", "s3cret")
	bob := loginToken(t, e, "bob", "hunter2")
	if w := e.do(req{method: "GET", path: "/api/v1/audit", bearer: alice}); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	e.setUser("alice", &fakeUser{"new-password", []string{auth.RoleAdmin}}) // a password change
	e.srv.RoleChanged("alice")
	if w := e.do(req{method: "GET", path: "/api/v1/audit", bearer: alice}); w.Code != http.StatusUnauthorized {
		t.Fatalf("the session of a changed role still works: %d", w.Code)
	}
	if w := e.do(req{method: "GET", path: "/api/v1/roles", bearer: bob}); w.Code != http.StatusForbidden {
		t.Fatalf("bob (readonly) lost his session or got more: %d", w.Code)
	}
}

// CP-07: the leader checks a forwarded identity against its own catalog.
func TestLeaderChecksForwardedIdentityAgainstTheRole(t *testing.T) {
	fw := &direct{leader: "leader-1"}
	leader, follower := twoServers(t, fw)
	tok := follower.token(auth.RoleAdmin)
	body := map[string]any{"grantee": "bob", "level": "database", "database": "shop", "privileges": []string{"connect"}}
	// The role was demoted on the leader (the follower's cache does not know yet).
	leader.setUser("tester", &fakeUser{"-", []string{auth.RoleReadonly}})
	w := follower.do(req{method: "POST", path: "/api/v1/grants", bearer: tok, body: body})
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Fatalf("a forwarded identity that lost its role was served: %d %s", w.Code, w.Body.String())
	}
	// A role that was dropped on the leader.
	leader.setUser("tester", nil)
	leader.srv.RoleChanged("tester")
	if w := follower.do(req{method: "POST", path: "/api/v1/grants", bearer: tok, body: body}); w.Code != http.StatusUnauthorized {
		t.Fatalf("a forwarded identity of a dropped role was served: %d %s", w.Code, w.Body.String())
	}
}

// CP-06: a user name with control characters is refused before PostgreSQL and
// before the audit log; the audit sink cleans whatever else reaches it.
func TestLoginRefusesControlCharactersInTheUserName(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	before := len(e.audit.Events())
	for i, name := range []string{"root_admin\x00options\x00-c x=y", "a\nb", "a\rb", "a\x1bb", "a b", "a\x7fb", "a" + string(rune(0x2028)) + "b"} {
		w := e.do(req{method: "POST", path: "/api/v1/auth/login", remote: fmt.Sprintf("192.0.2.%d:1", i+1), body: map[string]any{"username": name, "password": "pw"}})
		if w.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d %s", name, w.Code, w.Body.String())
		}
	}
	if got := len(e.audit.Events()); got != before {
		t.Errorf("%d audit events were written for refused user names", got-before)
	}
}
