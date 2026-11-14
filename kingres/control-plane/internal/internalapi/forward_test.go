// SPDX-License-Identifier: PostgreSQL

package internalapi

import "testing"

func TestForwardAnyPeerMayCallTheLeaderOnly(t *testing.T) {
	e := newEnv(t)
	peer := cert(t, e.ca, "server2")
	pc := e.client(&peer)
	body := ForwardRequest{OperationID: "createRole", Identity: ForwardIdentity{Username: "alice", ClusterRoles: []string{"kingres_admin"}}, Body: []byte(`{"name":"x"}`)}
	st, out, err := e.do(pc, "POST", "/forward", Version, body)
	if err != nil || st != 200 || out["status"] != float64(201) {
		t.Fatalf("forward: %d %v %v", st, out, err)
	}
	if len(e.forwarded) != 1 || e.forwarded[0] != "server2:createRole:alice" {
		t.Errorf("backend saw %v (the peer must be the certificate's server id)", e.forwarded)
	}
	// Without a certificate, with another version and with an unknown field: refused.
	if st, out, err := e.do(e.client(nil), "POST", "/forward", Version, body); err == nil && (st != 401 || out["error"] != "no_certificate") {
		t.Errorf("forward without a client certificate: %d %v", st, out)
	}
	if st, _, _ := e.do(pc, "POST", "/forward", "99", body); st != 409 {
		t.Errorf("wrong version: %d", st)
	}
	if st, _, _ := e.do(pc, "POST", "/forward", Version, map[string]any{"operationId": "x", "identity": map[string]any{"username": "a"}, "sql": "drop"}); st != 400 {
		t.Errorf("unknown field: %d", st)
	}
	if st, _, _ := e.do(pc, "POST", "/forward", Version, map[string]any{"operationId": "x"}); st != 400 {
		t.Errorf("no identity: %d", st)
	}
	if st, _, _ := e.do(pc, "GET", "/forward", Version, nil); st != 404 {
		t.Errorf("GET /forward: %d", st)
	}
	// A server that is not the leader refuses, whoever asks.
	e.leader = "someone-else"
	if st, out, _ := e.do(pc, "POST", "/forward", Version, body); st != 409 || out["error"] != "not_leader" {
		t.Errorf("non-leader: %d %v", st, out)
	}
	e.leader = ""
	if st, _, _ := e.do(pc, "POST", "/forward", Version, body); st != 409 {
		t.Errorf("unknown leader: %d", st)
	}
	if len(e.forwarded) != 1 {
		t.Errorf("refused calls reached the backend: %v", e.forwarded)
	}
}
