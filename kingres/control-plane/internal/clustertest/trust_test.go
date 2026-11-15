// SPDX-License-Identifier: PostgreSQL

package clustertest_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

// clientKeyOf is what a server that saw the password of a role at sign-in holds:
// the SCRAM ClientKey under the role's verifier on the node.
func clientKeyOf(t *testing.T, n *clustertest.Node, role, password string) []byte {
	t.Helper()
	var verifier string
	if err := n.ServicePG("postgres").QueryRow(context.Background(),
		`SELECT rolpassword FROM pg_catalog.pg_authid WHERE rolname = $1`, role).Scan(&verifier); err != nil {
		t.Fatal(err)
	}
	p, err := scram.Parse(verifier)
	if err != nil {
		t.Fatal(err)
	}
	key, err := p.ClientKey(password)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// CP2-01 / CP-07: a certificate of the cluster is not enough to act as a role.
// A member that never saw the password of kingres_admin cannot forward as it,
// whatever it claims; a proof made with the real password works once, for its
// request only.
func TestForwardedIdentityNeedsTheProofOfASession(t *testing.T) {
	c := sharedCluster(t)
	leader, member := c.Leader(), c.Nodes[1]
	pair := loadPair(t, member)
	forward := func(req internalapi.ForwardRequest) (int, map[string]any) {
		return internalCall(t, leader, pair, "1", "POST", "/forward", req)
	}
	admin := internalapi.ForwardIdentity{Username: clustertest.AdminUser, ClusterRoles: []string{"kingres_admin"}}
	listTasks := func() internalapi.ForwardRequest {
		return internalapi.ForwardRequest{OperationID: "listTasks", Identity: admin, RequestID: "trust-test", Source: "cli"}
	}
	sign := func(key []byte, req *internalapi.ForwardRequest, to string, at time.Time) {
		p, err := fwdproof.Sign(key, req.ProofMessage(fwdproof.KindForward, member.Host, to), at)
		if err != nil {
			t.Fatal(err)
		}
		req.Proof = &p
	}

	// The class: naming an existing role, with a valid certificate, authorizes nothing.
	if st, out := forward(listTasks()); st != 401 || out["error"] != "identity_unproven" {
		t.Errorf("no proof: %d %v", st, out)
	}
	req := listTasks()
	sign(clientKeyOf(t, leader, "early", markerPassword), &req, leader.Host, time.Now()) // a proof of ANOTHER role's session
	if st, out := forward(req); st != 401 {
		t.Errorf("a proof made with another role's password: %d %v", st, out)
	}
	req = listTasks()
	sign(make([]byte, 32), &req, leader.Host, time.Now())
	if st, out := forward(req); st != 401 {
		t.Errorf("a proof made without the password: %d %v", st, out)
	}

	// A real session's proof is accepted, once, for that request, to that server, now.
	key := clientKeyOf(t, leader, clustertest.AdminUser, clustertest.AdminPass)
	req = listTasks()
	sign(key, &req, leader.Host, time.Now())
	replay := req
	if st, out := forward(req); st != 200 {
		t.Fatalf("a valid proof: %d %v", st, out)
	}
	if st, out := forward(replay); st != 401 {
		t.Errorf("a replayed proof: %d %v", st, out)
	}
	req = listTasks()
	sign(key, &req, leader.Host, time.Now())
	req.OperationID, req.Query = "listAlerts", ""
	if st, out := forward(req); st != 401 {
		t.Errorf("a proof used for another operation: %d %v", st, out)
	}
	req = listTasks()
	sign(key, &req, c.Nodes[2].Host, time.Now()) // made for another receiver
	if st, out := forward(req); st != 401 {
		t.Errorf("a proof made for another server: %d %v", st, out)
	}
	req = listTasks()
	sign(key, &req, leader.Host, time.Now().Add(-time.Hour))
	if st, out := forward(req); st != 401 {
		t.Errorf("an old proof: %d %v", st, out)
	}

	// A cluster-wide read (gather) goes to every member, so it never carries a proof (the
	// proof reveals the ClientKey to its receiver): a gather call with one is refused, and one
	// without the leader's assertion is too (see TestMemberCannotGatherAsAnotherUser).
	greq := internalapi.ForwardRequest{OperationID: "listAlerts", Identity: admin}
	if st, out := internalCall(t, leader, pair, "1", "POST", "/gather", greq); st != 401 || out["error"] != "identity_unproven" {
		t.Errorf("gather without an assertion: %d %v", st, out)
	}
	p, _ := fwdproof.Sign(key, greq.ProofMessage(fwdproof.KindGather, member.Host, leader.Host), time.Now())
	greq.Proof = &p
	if st, out := internalCall(t, leader, pair, "1", "POST", "/gather", greq); st != 400 {
		t.Errorf("gather with a proof: %d %v", st, out)
	}

	// Through the normal path (the member forwards a real session) everything keeps working.
	if !member.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Fatal("the administrator cannot sign in on the member")
	}
	member.Expect(200, "GET", "/tasks", nil)
	mkRole(t, member, "forwarded_ok", "Forwarded-Ok-Password-1", "kingres_readonly")
}

// CP2-02: kingres_ops cannot add a server. Before the fix an ops-level account
// that ran its own kingresd could have the leader sign a certificate of the
// cluster for it and send it every role's SCRAM verifier.
func TestOpsCannotAddAServer(t *testing.T) {
	c := clustertest.New(t, 2)
	for _, n := range c.Nodes {
		n.Start()
	}
	leader, candidate := c.Leader(), c.Nodes[1]
	leader.InitCluster("addrole")
	mkRole(t, leader, "ops_adder", "Ops-Adder-Password-1", "kingres_ops")
	opsTok := sessionToken(t, leader, "ops_adder", "Ops-Adder-Password-1")
	pin := candidate.BootstrapFingerprint()

	st, out := leader.Do("POST", "/servers", map[string]any{"address": candidate.RestAddr(), "setupToken": candidate.SetupToken(), "expectedFingerprint": pin},
		map[string]string{"Authorization": "Bearer " + opsTok})
	if st != 403 {
		t.Fatalf("addServer as kingres_ops: %d %v (the leader must not issue a certificate for it)", st, out)
	}
	if got := leader.Servers(); len(got) != 1 {
		t.Errorf("a refused add changed the cluster: %v", got)
	}
	// Nothing reached the candidate: no certificate of the cluster was installed.
	if st, state := candidate.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""}); st != 200 || state["state"] != "uninitialized" {
		t.Errorf("the candidate after the refused add: %d %v", st, state)
	}
	// An administrator can.
	st, add := leader.AddServer(candidate, candidate.SetupToken(), pin)
	if st != 202 {
		t.Fatalf("addServer as administrator: %d %v", st, add)
	}
	leader.WaitTaskDone(clustertest.TaskID(add), 8*time.Minute)
}

// Unconfirmed item "internal exec has no statement timeout": the statement runs
// under a statement_timeout the server sets, whatever the caller does.
func TestInternalExecRunsUnderAStatementTimeout(t *testing.T) {
	c := sharedCluster(t)
	leader, member := c.Leader(), c.Nodes[1]
	st, out := internalCall(t, member, loadPair(t, leader), "1", "POST", "/exec", map[string]any{"database": "postgres", "sql": "SHOW statement_timeout"})
	if st != 200 {
		t.Fatalf("exec: %d %v", st, out)
	}
	rows, _ := out["rows"].([]any)
	if len(rows) != 1 || rows[0].([]any)[0] != "15min" {
		t.Errorf("statement_timeout of the internal exec = %v, want 15min", out["rows"])
	}
}

// CP2-01, gather: the proof of a session reveals the user's ClientKey to whoever
// checks it, so only the leader ever receives one. The members that answer a gather
// call accept an assertion that the leader signed with its own key. A member's own
// key and certificate cannot make one, an assertion captured from a gather call
// cannot be used by the member that received it, and a proof is refused by every
// server that is not the leader.
func TestMemberCannotGatherAsAnotherUser(t *testing.T) {
	c := sharedCluster(t)
	leader, asker, receiver := c.Leader(), c.Nodes[1], c.Nodes[2]
	askerPair, receiverPair := loadPair(t, asker), loadPair(t, receiver)
	admin := internalapi.ForwardIdentity{Username: clustertest.AdminUser, ClusterRoles: []string{"kingres_admin"}}
	key := clientKeyOf(t, leader, clustertest.AdminUser, clustertest.AdminPass)
	newReq := func() internalapi.ForwardRequest {
		return internalapi.ForwardRequest{OperationID: "listLogs", Identity: admin, RequestID: "gather-trust", Source: "panel", Query: "limit=1"}
	}
	gatherAs := func(pair *tls.Certificate, to *clustertest.Node, req internalapi.ForwardRequest) (int, map[string]any) {
		return internalCall(t, to, pair, "1", "POST", "/gather", req)
	}
	authorize := func(pair *tls.Certificate, from, to string, to2 *clustertest.Node, k []byte) (int, map[string]any) {
		req := newReq()
		pr, err := fwdproof.Sign(k, req.ProofMessage(fwdproof.KindGatherAuthorize, from, to), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		req.Proof = &pr
		return internalCall(t, to2, pair, "1", "POST", "/gather-authorize", req)
	}

	// A member's own key and certificate: no assertion, or one signed by itself, is worth nothing.
	if st, out := gatherAs(askerPair, receiver, newReq()); st != 401 {
		t.Errorf("gather without an assertion: %d %v", st, out)
	}
	req := newReq()
	own, err := (&fwdproof.Signer{Load: func() (tls.Certificate, error) { return *askerPair, nil }}).Sign(req.ProofMessage(fwdproof.KindGather, asker.Host, ""))
	if err != nil {
		t.Fatal(err)
	}
	req.Assertion = &own
	if st, out := gatherAs(askerPair, receiver, req); st != 401 {
		t.Errorf("gather with an assertion that the member signed itself: %d %v", st, out)
	}
	// The leader signs only for a proof made with the user's password; a server that is not the
	// leader does not take proofs at all (so none is ever sent to a member).
	if st, out := authorize(askerPair, asker.Host, leader.Host, leader, make([]byte, 32)); st != 401 {
		t.Errorf("authorize with a proof made without the password: %d %v", st, out)
	}
	if st, out := authorize(askerPair, asker.Host, receiver.Host, receiver, key); st != 409 {
		t.Errorf("a proof sent to a member that is not the leader: %d %v", st, out)
	}

	// The legitimate path: the asking server proves the session to the leader and gets an assertion.
	st, raw := authorize(askerPair, asker.Host, leader.Host, leader, key)
	if st != 200 {
		t.Fatalf("authorize: %d %v", st, raw)
	}
	var a fwdproof.Assertion
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &a); err != nil || len(a.Signature) == 0 {
		t.Fatalf("the assertion: %v %v", err, raw)
	}
	req = newReq()
	req.Assertion = &a
	if st, out := gatherAs(askerPair, receiver, req); st != 200 {
		t.Fatalf("gather with the leader's assertion: %d %v", st, out)
	}
	// The member that received it holds the assertion now. It cannot use it: not as the asker
	// (the asking server is bound), not for another request, and not twice.
	if st, out := gatherAs(receiverPair, leader, req); st != 401 {
		t.Errorf("a receiver that replays a captured assertion as itself: %d %v", st, out)
	}
	if st, out := gatherAs(askerPair, receiver, req); st != 401 {
		t.Errorf("a replay of the assertion: %d %v", st, out)
	}
	other := newReq()
	other.OperationID = "listAlerts"
	other.Assertion = &a
	if st, out := gatherAs(askerPair, leader, other); st != 401 {
		t.Errorf("the assertion used for another operation: %d %v", st, out)
	}
}
