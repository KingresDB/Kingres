// SPDX-License-Identifier: AGPL-3.0-only
// Portions Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
)

// The proof and the verification records are real operations; the guard
// applies their roles from the contract (kingres_readonly) before the handler.
func TestLedgerProofOperationsAreGuardedByRole(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	stats := e.token(auth.RoleStats)
	ro := e.token(auth.RoleReadonly)
	impl := e.srv.handlers()
	calls := []struct{ id, method, path string }{
		{"createInclusionProof", "POST", "/api/v1/databases/shop/ledger/inclusion-proofs"},
		{"listLedgerVerifications", "GET", "/api/v1/databases/shop/ledger/verifications"},
		{"getLedgerVerification", "GET", "/api/v1/databases/shop/ledger/verifications/1"},
	}
	for _, c := range calls {
		if _, ok := impl[c.id]; !ok {
			t.Errorf("%s has no handler", c.id)
		}
		body := map[string]any{}
		if w := e.do(req{method: c.method, path: c.path, body: body}); w.Code != 401 {
			t.Errorf("%s unauthenticated: %d", c.id, w.Code)
		}
		wantProblem(t, e.do(req{method: c.method, path: c.path, bearer: stats, body: body}), 403, "forbidden")
		// Authorized: the handler runs; this test server manages no instance.
		wantProblem(t, e.do(req{method: c.method, path: c.path, bearer: ro, body: body}), 422, "unsupported_operation")
	}
}

// The proof is answered where the user is (it reads the row as the signed-in
// role); the verification records are answered by the leader, which owns the
// system database that gives them their identifiers.
func TestLedgerProofForwardingClassification(t *testing.T) {
	want := map[string]bool{"createInclusionProof": false, "listLedgerVerifications": true, "getLedgerVerification": true}
	for _, op := range contract.Operations() {
		if w, ok := want[op.ID]; ok {
			if got := forwardsToLeader(op); got != w {
				t.Errorf("%s: forwarded = %v, want %v", op.ID, got, w)
			}
			delete(want, op.ID)
		}
	}
	if len(want) != 0 {
		t.Errorf("operations not in the contract: %v", want)
	}
}
