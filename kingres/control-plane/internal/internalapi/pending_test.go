// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package internalapi

import (
	"fmt"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
)

// CP2-01: a server that holds a certificate but is not a member (pending) may
// make none of the calls that act in the cluster; every one of them is refused
// before it is read (POST /forward is gated by the backend, see cmd/kingresd).
func TestPendingServerAuthorizesNothing(t *testing.T) {
	e := newEnv(t)
	e.pending = map[string]bool{"newcomer": true}
	c := cert(t, e.ca, "newcomer")
	pc := e.client(&c)
	for key, body := range map[string]any{
		"/pki/renew":     map[string]any{"csr": "x"},
		"/leave-cluster": map[string]any{"actor": "a"},
		"/leave-status":  map[string]any{"taskId": 1},
		"/add-server":    map[string]any{},
		"/task/record":   map[string]any{},
		"/task/settle":   map[string]any{},
		"/task/reserve":  map[string]any{},
	} {
		st, out, err := e.do(pc, "POST", key, Version, body)
		if err != nil || st != 403 || out["error"] != "not_member" {
			t.Errorf("POST %s as a pending server: %d %v %v", key, st, out, err)
		}
	}
	// The same certificate as a member is accepted.
	delete(e.pending, "newcomer")
	if st, _, _ := e.do(pc, "POST", "/leave-status", Version, map[string]any{"taskId": 1}); st == 403 {
		t.Errorf("a member's call was refused as not_member")
	}
}

// A request whose identity cannot be proven never reaches the backend, for
// forward and for gather; an unreadable verifier is a 503, not a 401.
func TestUnprovenIdentityIsRefusedBeforeTheBackend(t *testing.T) {
	e := newEnv(t)
	c := cert(t, e.ca, "server2")
	pc := e.client(&c)
	req := ForwardRequest{OperationID: "createRole", Identity: ForwardIdentity{Username: "alice", ClusterRoles: []string{"kingres_admin"}}}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fwdproof.ErrInvalid, 401, "identity_unproven"},
		{fwdproof.ErrExpired, 401, "identity_unproven"},
		{fwdproof.ErrReplay, 401, "identity_unproven"},
		{fmt.Errorf("wrapped: %w", fwdproof.ErrUnavailable), 503, "unavailable"},
	} {
		e.unproven = tc.err
		st, out, err := e.do(pc, "POST", "/forward", Version, req)
		if err != nil || st != tc.status || out["error"] != tc.code {
			t.Errorf("forward with %v: %d %v %v", tc.err, st, out, err)
		}
	}
	if len(e.forwarded) != 0 {
		t.Errorf("refused calls reached the backend: %v", e.forwarded)
	}
}

// A gather call goes to every member, so it must never carry a proof (which
// reveals the user's ClientKey to its receiver): a member refuses one, and
// refuses a call without the leader's assertion.
func TestGatherNeverCarriesAProof(t *testing.T) {
	e := newEnv(t)
	c := cert(t, e.ca, "server2")
	pc := e.client(&c)
	gathered := 0
	e.gather = func() { gathered++ }
	id := ForwardIdentity{Username: "alice", ClusterRoles: []string{"kingres_ops"}}
	proof := &fwdproof.Proof{IssuedAt: 1, Nonce: make([]byte, 16), Value: make([]byte, 32)}
	if st, out, err := e.do(pc, "POST", "/gather", Version, ForwardRequest{OperationID: "listLogs", Identity: id, Proof: proof}); err != nil || st != 400 {
		t.Errorf("a gather call with a proof: %d %v %v", st, out, err)
	}
	e.noAssertion = true
	if st, out, err := e.do(pc, "POST", "/gather", Version, ForwardRequest{OperationID: "listLogs", Identity: id}); err != nil || st != 401 || out["error"] != "identity_unproven" {
		t.Errorf("a gather call without an assertion: %d %v %v", st, out, err)
	}
	if gathered != 0 {
		t.Errorf("a refused gather call reached the backend %d times", gathered)
	}
	e.noAssertion = false
	if st, _, _ := e.do(pc, "POST", "/gather", Version, ForwardRequest{OperationID: "listLogs", Identity: id, Assertion: &fwdproof.Assertion{}}); st != 200 || gathered != 1 {
		t.Errorf("a gather call with an accepted assertion: %d, ran %d", st, gathered)
	}
	// Authorizing needs a proof (the backend's check refuses a request without one).
	e.unproven = fwdproof.ErrInvalid
	if st, _, _ := e.do(pc, "POST", "/gather-authorize", Version, ForwardRequest{OperationID: "listLogs", Identity: id}); st != 401 {
		t.Errorf("authorize without a proof: %d", st)
	}
}
