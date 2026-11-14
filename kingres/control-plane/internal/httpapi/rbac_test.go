// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
)

var paramRE = regexp.MustCompile(`\{([A-Za-z]+)\}`)

// concretePath fills the path parameters of a template with plausible values.
func concretePath(tpl string) string {
	return contract.BasePath + paramRE.ReplaceAllStringFunc(tpl, func(m string) string {
		name := m[1 : len(m)-1]
		if strings.HasSuffix(name, "Id") {
			return "1"
		}
		return "x"
	})
}

// sessionOps are the operations that need a session (any security scheme
// other than none or setup token only).
func sessionOps() []contract.Operation {
	var out []contract.Operation
	for _, op := range contract.Operations() {
		if op.Accepts(contract.SchemeBearer) || op.Accepts(contract.SchemeSessionCookie) {
			out = append(out, op)
		}
	}
	return out
}

// TestRoleMatrix drives every session operation with a token of each cluster
// role and checks that the guard lets exactly the permitted roles through:
// the response is 403 forbidden if and only if the role hierarchy denies it.
func TestRoleMatrix(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	tokens := map[string]string{}
	for _, r := range auth.ClusterRoles {
		tokens[r] = e.token(r)
	}
	ops := sessionOps()
	if len(ops) < 70 {
		t.Fatalf("only %d session operations; the contract has more", len(ops))
	}
	for _, op := range ops {
		for _, role := range auth.ClusterRoles {
			// Logout revokes the token it is called with; use a fresh one.
			tok := tokens[role]
			if op.ID == "logout" {
				tok = e.token(role)
			}
			w := e.do(req{method: op.Method, path: concretePath(op.Path), bearer: tok, body: bodyFor(op)})
			allowed := auth.Allows([]string{role}, op.Role)
			switch {
			case !allowed && w.Code != http.StatusForbidden:
				t.Errorf("%s as %s: status %d, want 403 (needs %s)", op.ID, role, w.Code, op.Role)
			case allowed && (w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized):
				t.Errorf("%s as %s: status %d, want access (needs %s): %s", op.ID, role, w.Code, op.Role, w.Body.String())
			}
			if w.Code == http.StatusForbidden {
				m := decode(t, w)
				if m["code"] != "forbidden" || !strings.Contains(m["detail"].(string), op.Role) {
					t.Errorf("%s as %s: forbidden body %v", op.ID, role, m)
				}
			}
		}
	}
}

func bodyFor(op contract.Operation) any {
	if op.Mutating() {
		return map[string]any{}
	}
	return nil
}

// TestSessionOperationsNeedCredentials checks the 401 for every session
// operation without credentials.
func TestSessionOperationsNeedCredentials(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	for _, op := range sessionOps() {
		w := e.do(req{method: op.Method, path: concretePath(op.Path), body: bodyFor(op)})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials: status %d, want 401", op.ID, w.Code)
		}
	}
}

// TestNotImplementedIsUniform checks that every operation without a handler
// answers 422 unsupported_operation for an authorized caller, and only after
// authentication and authorization.
func TestNotImplementedIsUniform(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	impl := e.srv.handlers()
	admin := e.token(auth.RoleAdmin)
	stats := e.token(auth.RoleStats)
	count := 0
	for _, op := range sessionOps() {
		if _, ok := impl[op.ID]; ok {
			continue
		}
		count++
		path := concretePath(op.Path)
		m := wantProblem(t, e.do(req{method: op.Method, path: path, bearer: admin, body: bodyFor(op)}), 422, "unsupported_operation")
		if !strings.Contains(m["detail"].(string), op.ID) {
			t.Errorf("%s: detail %q does not name the operation", op.ID, m["detail"])
		}
		// Authentication comes before the not-implemented answer ...
		if w := e.do(req{method: op.Method, path: path, body: bodyFor(op)}); w.Code != 401 {
			t.Errorf("%s: unauthenticated status %d, want 401", op.ID, w.Code)
		}
		// ... and so does authorization.
		if op.Role != auth.RoleStats {
			if w := e.do(req{method: op.Method, path: path, bearer: stats, body: bodyFor(op)}); w.Code != 403 {
				t.Errorf("%s: kingres_stats status %d, want 403", op.ID, w.Code)
			}
		}
	}
	if count < 25 {
		t.Errorf("only %d not-implemented operations found", count)
	}
}

func TestHierarchyAllows(t *testing.T) {
	tests := []struct {
		have     []string
		required string
		want     bool
	}{
		{nil, "none", true},
		{nil, auth.RoleStats, false},
		{[]string{auth.RoleStats}, auth.RoleStats, true},
		{[]string{auth.RoleStats}, auth.RoleReadonly, false},
		{[]string{auth.RoleReadonly}, auth.RoleStats, true},
		{[]string{auth.RoleReadonly}, auth.RoleOps, false},
		{[]string{auth.RoleOps}, auth.RoleReadonly, true},
		{[]string{auth.RoleOps}, auth.RoleAdmin, false},
		{[]string{auth.RoleAdmin}, auth.RoleOps, true},
		{[]string{auth.RoleStats, auth.RoleOps}, auth.RoleOps, true},
		{[]string{"postgres", "pg_monitor"}, auth.RoleStats, false},
		{[]string{auth.RoleAdmin}, "unknown_role", false},
	}
	for _, tc := range tests {
		if got := auth.Allows(tc.have, tc.required); got != tc.want {
			t.Errorf("Allows(%v, %q) = %v, want %v", tc.have, tc.required, got, tc.want)
		}
	}
}
