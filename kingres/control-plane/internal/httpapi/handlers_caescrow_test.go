// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// escrowMembers records the calls of the escrow operations.
type escrowMembers struct {
	fakeMembers
	calls []string
	err   error
}

func (e *escrowMembers) st() api.CaEscrowStatus {
	return api.CaEscrowStatus{Enabled: true, HoldsKey: true, Generation: 4, Recipients: []api.CaEscrowRecipient{}, Message: "m"}
}

func (e *escrowMembers) EscrowStatus(context.Context) (api.CaEscrowStatus, error) {
	e.calls = append(e.calls, "get")
	return e.st(), e.err
}

func (e *escrowMembers) SetEscrow(_ context.Context, on bool) (api.CaEscrowStatus, error) {
	if on {
		e.calls = append(e.calls, "on")
	} else {
		e.calls = append(e.calls, "off")
	}
	return e.st(), e.err
}

func (e *escrowMembers) RotateEscrow(context.Context) (api.CaEscrowStatus, error) {
	e.calls = append(e.calls, "rotate")
	return e.st(), e.err
}

var _ CAEscrow = (*escrowMembers)(nil)

func TestCaEscrowOperationsNeedAnAdministrator(t *testing.T) {
	m := &escrowMembers{}
	e := newEnv(t, func(d *Deps) { d.Members = m })
	e.initialize()
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/certificate-authority/escrow", nil},
		{"PUT", "/api/v1/certificate-authority/escrow", map[string]any{"enabled": true}},
		{"POST", "/api/v1/certificate-authority/escrow/rotations", nil},
	} {
		if w := e.do(req{method: tc.method, path: tc.path, body: tc.body, bearer: e.token(auth.RoleOps)}); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as ops: %d", tc.method, tc.path, w.Code)
		}
	}
	if len(m.calls) != 0 {
		t.Fatalf("an operator without the role reached the escrow: %v", m.calls)
	}
	admin := e.token(auth.RoleAdmin)
	w := e.do(req{method: "GET", path: "/api/v1/certificate-authority/escrow", bearer: admin})
	var got api.CaEscrowStatus
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !got.Enabled || got.Generation != 4 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if w := e.do(req{method: "PUT", path: "/api/v1/certificate-authority/escrow", bearer: admin, body: map[string]any{"enabled": false}}); w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if w := e.do(req{method: "POST", path: "/api/v1/certificate-authority/escrow/rotations", bearer: admin}); w.Code != 200 {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	if len(m.calls) != 3 || m.calls[0] != "get" || m.calls[1] != "off" || m.calls[2] != "rotate" {
		t.Errorf("calls %v", m.calls)
	}
}

func TestSetCaEscrowNeedsTheFlag(t *testing.T) {
	m := &escrowMembers{}
	e := newEnv(t, func(d *Deps) { d.Members = m })
	e.initialize()
	admin := e.token(auth.RoleAdmin)
	wantProblem(t, e.do(req{method: "PUT", path: "/api/v1/certificate-authority/escrow", bearer: admin, body: map[string]any{}}), 400, "validation_failed")
	wantProblem(t, e.do(req{method: "PUT", path: "/api/v1/certificate-authority/escrow", bearer: admin, body: map[string]any{"enabled": "yes"}}), 400, "bad_request")
	if len(m.calls) != 0 {
		t.Fatalf("calls %v", m.calls)
	}
}

func TestCaEscrowProblemsReachTheClient(t *testing.T) {
	for _, tc := range []struct {
		err    *problem.Problem
		status int
		code   string
	}{
		{problem.New(problem.CertificateAuthorityUnavailable, "no CA key"), 503, "certificate_authority_unavailable"},
		{problem.New(problem.Conflict, "escrow is off"), 409, "conflict"},
	} {
		m := &escrowMembers{err: tc.err}
		e := newEnv(t, func(d *Deps) { d.Members = m })
		e.initialize()
		wantProblem(t, e.do(req{method: "POST", path: "/api/v1/certificate-authority/escrow/rotations", bearer: e.token(auth.RoleAdmin)}), tc.status, tc.code)
	}
}

// A server whose Members has no escrow says so; it does not panic.
func TestCaEscrowWithoutSupport(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Members = &fakeMembers{} })
	e.initialize()
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/certificate-authority/escrow", bearer: e.token(auth.RoleAdmin)}), 422, "unsupported_operation")
}

// The escrow lives on the leader: a server that is not the leader forwards all three operations, the read as well.
func TestCaEscrowOperationsAreAnsweredByTheLeader(t *testing.T) {
	for _, id := range []string{"getCaEscrow", "setCaEscrow", "rotateCaEscrow"} {
		if !forwardsToLeader(operationOf(t, id)) {
			t.Errorf("%s is not forwarded to the leader", id)
		}
	}
}
