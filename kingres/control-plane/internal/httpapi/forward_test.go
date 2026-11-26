// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// direct is a Forwarder whose "leader" is another Server in the same process:
// Forward calls its ServeForwarded, as the internal API does after it has
// verified the peer's certificate.
type direct struct {
	leader  string
	self    bool
	err     error
	target  *Server
	calls   int
	lastReq internalapi.ForwardRequest
}

func (d *direct) Leader(context.Context) (string, bool, error) { return d.leader, d.self, d.err }
func (d *direct) Forward(ctx context.Context, _ string, req internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
	d.calls++
	d.lastReq = req
	return d.target.ServeForwarded(ctx, "follower-1", req)
}

func twoServers(t *testing.T, fw *direct) (leader, follower *env) {
	t.Helper()
	leader = newEnv(t)
	leader.initialize()
	fw.target = leader.srv
	leader.testUser([]string{auth.RoleAdmin}) // the leader knows the roles of the forwarded identities
	follower = newEnv(t, func(d *Deps) { d.Forwarder = fw })
	follower.initialize()
	return leader, follower
}

func TestMutationOnAFollowerIsForwardedAndAuditedOnce(t *testing.T) {
	fw := &direct{leader: "leader-1"}
	leader, follower := twoServers(t, fw)
	body := map[string]any{"grantee": "bob", "level": "database", "database": "shop", "privileges": []string{"connect"}}
	got := follower.do(req{method: "POST", path: "/api/v1/grants", bearer: follower.token(auth.RoleAdmin), body: body})
	want := leader.do(req{method: "POST", path: "/api/v1/grants", bearer: leader.token(auth.RoleAdmin), body: body})
	if fw.calls != 1 {
		t.Fatalf("forward calls = %d", fw.calls)
	}
	if got.Code != want.Code || got.Code == http.StatusInternalServerError {
		t.Errorf("relayed status = %d, the leader's own = %d", got.Code, want.Code)
	}
	if got.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
		t.Errorf("content type %q vs %q", got.Header().Get("Content-Type"), want.Header().Get("Content-Type"))
	}
	if fw.lastReq.OperationID != "createGrant" || fw.lastReq.Identity.Username != "tester" || fw.lastReq.Source != "cli" {
		t.Errorf("forward request = %+v", fw.lastReq)
	}
	// Audited once, by the leader, with the forwarding server named.
	if n := len(follower.audit.Events()); n != 0 {
		t.Errorf("the follower audited %d events", n)
	}
	var forwardedEvents int
	for _, ev := range leader.audit.Events() {
		if ev.Action == "createGrant" && ev.Details["forwardedBy"] == "follower-1" {
			forwardedEvents++
			if ev.Actor != "tester" {
				t.Errorf("actor = %q", ev.Actor)
			}
		}
	}
	if forwardedEvents != 1 {
		t.Errorf("leader has %d forwarded createGrant events: %+v", forwardedEvents, leader.audit.Events())
	}
}

func TestForwardingKeepsTheCallersAuthorization(t *testing.T) {
	fw := &direct{leader: "leader-1"}
	_, follower := twoServers(t, fw)
	// A read-only user is refused by the follower itself; nothing is forwarded.
	w := follower.do(req{method: "POST", path: "/api/v1/grants", bearer: follower.token(auth.RoleReadonly), body: map[string]any{}})
	if w.Code != http.StatusForbidden || fw.calls != 0 {
		t.Errorf("status %d, forwarded %d times", w.Code, fw.calls)
	}
	// No credential at all: same.
	if w := follower.do(req{method: "POST", path: "/api/v1/grants", body: map[string]any{}}); w.Code != http.StatusUnauthorized || fw.calls != 0 {
		t.Errorf("anonymous: %d, forwarded %d", w.Code, fw.calls)
	}
}

func TestLeaderChecksTheForwardedIdentityAgain(t *testing.T) {
	fw := &direct{leader: "leader-1"}
	leader, _ := twoServers(t, fw)
	ctx := context.Background()
	base := internalapi.ForwardRequest{OperationID: "createGrant", Identity: internalapi.ForwardIdentity{Username: "bob", ClusterRoles: []string{auth.RoleReadonly}}, Body: []byte(`{}`)}
	// A read-only identity cannot run an admin operation even when a server claims it.
	resp, err := leader.srv.ServeForwarded(ctx, "p", base)
	if err != nil || resp.Status != http.StatusForbidden {
		t.Errorf("readonly identity: %d, %v", resp.Status, err)
	}
	for name, mod := range map[string]func(*internalapi.ForwardRequest){
		"unknown operation":   func(r *internalapi.ForwardRequest) { r.OperationID = "doesNotExist" },
		"login is not routed": func(r *internalapi.ForwardRequest) { r.OperationID = "login" },
		"setup is not routed": func(r *internalapi.ForwardRequest) { r.OperationID = "initCluster" },
		"this-server is local": func(r *internalapi.ForwardRequest) {
			r.OperationID = "restartThisServerInstance"
			r.PathParams = map[string]string{"instanceId": "1"}
		},
		"a read that is not leader-bound": func(r *internalapi.ForwardRequest) { r.OperationID = "listRoles" },
		"unknown cluster role":            func(r *internalapi.ForwardRequest) { r.Identity.ClusterRoles = []string{"superuser"} },
		"no user":                         func(r *internalapi.ForwardRequest) { r.Identity.Username = "" },
		"missing path parameter":          func(r *internalapi.ForwardRequest) { r.OperationID = "deleteRole" },
		"extra path parameter":            func(r *internalapi.ForwardRequest) { r.PathParams = map[string]string{"x": "y"} },
		"newline in the query":            func(r *internalapi.ForwardRequest) { r.Query = "a=1\r\nHost: x" },
	} {
		r := base
		mod(&r)
		if _, err := leader.srv.ServeForwarded(ctx, "p", r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestForwardingFailsClosedWhenTheLeaderIsUnknownOrUnreachable(t *testing.T) {
	fw := &direct{err: errors.New("catalog unreadable")}
	_, follower := twoServers(t, fw)
	tok := follower.token(auth.RoleAdmin)
	w := follower.do(req{method: "POST", path: "/api/v1/grants", bearer: tok, body: map[string]any{}})
	wantProblem(t, w, http.StatusServiceUnavailable, "service_unavailable")
	fw.err, fw.leader = nil, ""
	wantProblem(t, follower.do(req{method: "POST", path: "/api/v1/grants", bearer: tok, body: map[string]any{}}), http.StatusServiceUnavailable, "service_unavailable")
	if fw.calls != 0 {
		t.Errorf("forwarded %d calls without a leader", fw.calls)
	}
}

func TestTheLeaderRunsItsOwnRequestsAndLocalOperationsStayLocal(t *testing.T) {
	fw := &direct{leader: "me", self: true}
	_, follower := twoServers(t, fw)
	tok := follower.token(auth.RoleAdmin)
	follower.do(req{method: "POST", path: "/api/v1/grants", bearer: tok, body: map[string]any{}})
	// Operations on this very server, sign-in and reads are never forwarded.
	fw.self = false
	fw.leader = "other"
	follower.do(req{method: "GET", path: "/api/v1/databases", bearer: tok})
	follower.do(req{method: "POST", path: "/api/v1/this-server/instances/1/restart", bearer: tok})
	follower.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "alice", "password": "s3cret"}})
	if fw.calls != 0 {
		t.Errorf("forwarded %d calls that must stay local", fw.calls)
	}
}

// Every operation is either forwarded or answered locally by design; this
// pins the classification so that a new operation cannot silently change it.
func TestForwardedOperationClassification(t *testing.T) {
	local := map[string]bool{}
	for _, op := range contract.Operations() {
		if !forwardsToLeader(op) && op.Mutating() {
			local[op.ID] = true
		}
	}
	for _, id := range []string{"login", "logout", "initCluster", "joinCluster", "checkSetupToken", "executeQuery", "cancelStatement", "createInclusionProof",
		"restartThisServerInstance", "startThisServerInstance", "stopThisServerInstance", "updateThisServerSettings", "leaveCluster", "acknowledgeAlert"} {
		delete(local, id)
	}
	if len(local) != 0 {
		t.Errorf("mutating operations that are not forwarded and not listed as local: %v", local)
	}
}

type fakeMembers struct{}

func (fakeMembers) AddServer(_ context.Context, actor string, _ api.AddServerRequest, _ string) (task.Task, error) {
	return task.Task{ID: 7, Kind: "server.add", State: task.Pending, CreatedBy: actor}, nil
}

func (fakeMembers) RemoveServer(_ context.Context, actor, _ string) (task.Task, error) {
	return task.Task{ID: 8, Kind: "server.remove", State: task.Pending, CreatedBy: actor}, nil
}

// A handler that sets its own audit details must not drop the facts of a
// forwarded request: the audit event keeps forwardedBy and originRequestId
// whatever the handler assigns to c.details.
func TestForwardedAuditKeepsForwardedByWhenTheHandlerSetsDetails(t *testing.T) {
	fw := &direct{leader: "leader-1"}
	leader := newEnv(t, func(d *Deps) { d.Members = fakeMembers{} })
	leader.initialize()
	fw.target = leader.srv
	leader.testUser([]string{auth.RoleAdmin})
	follower := newEnv(t, func(d *Deps) { d.Forwarder = fw })
	follower.initialize()
	body := map[string]any{"address": "db9.example.com:8091", "setupToken": "tok", "allowUnpinned": true}
	w := follower.do(req{method: "POST", path: "/api/v1/servers", bearer: follower.token(auth.RoleAdmin), body: body})
	if w.Code != http.StatusAccepted {
		t.Fatalf("addServer through the follower: %d %s", w.Code, w.Body.String())
	}
	var found int
	for _, ev := range leader.audit.Events() {
		if ev.Action != "addServer" {
			continue
		}
		found++
		if ev.Details["forwardedBy"] != "follower-1" {
			t.Errorf("forwardedBy lost: %+v", ev.Details)
		}
		if ev.Details["originRequestId"] == nil || ev.Details["originRequestId"] == "" {
			t.Errorf("originRequestId lost: %+v", ev.Details)
		}
		if ev.Details["address"] != "db9.example.com:8091" {
			t.Errorf("the handler's own details were lost: %+v", ev.Details)
		}
	}
	if found != 1 {
		t.Errorf("%d addServer events on the leader", found)
	}
}

func (fakeMembers) RenewServerCertificates(_ context.Context, actor, _ string) (task.Task, error) {
	return task.Task{ID: 9, Kind: "certificate.renew", State: task.Pending, CreatedBy: actor}, nil
}

func (fakeMembers) RevokeServerCertificates(_ context.Context, actor, _ string) (task.Task, error) {
	return task.Task{ID: 10, Kind: "certificate.revoke", State: task.Pending, CreatedBy: actor}, nil
}
