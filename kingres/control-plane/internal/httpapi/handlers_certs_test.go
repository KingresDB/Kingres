// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// certMembers records the certificate calls and answers as configured.
type certMembers struct {
	fakeMembers
	renew, revoke []string
	err           error
}

func (c *certMembers) RenewServerCertificates(_ context.Context, actor, id string) (task.Task, error) {
	c.renew = append(c.renew, actor+":"+id)
	if c.err != nil {
		return task.Task{}, c.err
	}
	return task.Task{ID: 31, Kind: "certificate.renew", State: task.Pending, SubjectType: "server", SubjectID: id, CreatedBy: actor}, nil
}

func (c *certMembers) RevokeServerCertificates(_ context.Context, actor, id string) (task.Task, error) {
	c.revoke = append(c.revoke, actor+":"+id)
	if c.err != nil {
		return task.Task{}, c.err
	}
	return task.Task{ID: 32, Kind: "certificate.revoke", State: task.Pending, SubjectType: "server", SubjectID: id, CreatedBy: actor}, nil
}

var _ Members = (*certMembers)(nil)
var _ = api.Server{}

func TestCertificateOperationsAreTasksForAdministrators(t *testing.T) {
	m := &certMembers{}
	e := newEnv(t, func(d *Deps) { d.Members = m })
	e.initialize()
	for _, tc := range []struct{ path, kind string }{
		{"/api/v1/servers/10.0.0.12/certificates/renewals", "certificate.renew"},
		{"/api/v1/servers/10.0.0.12/certificates/revocations", "certificate.revoke"},
	} {
		// kingres_ops is not enough.
		if w := e.do(req{method: "POST", path: tc.path, bearer: e.token(auth.RoleOps)}); w.Code != http.StatusForbidden {
			t.Errorf("%s as ops: %d", tc.path, w.Code)
		}
		w := e.do(req{method: "POST", path: tc.path, bearer: e.token(auth.RoleAdmin)})
		if w.Code != http.StatusAccepted || w.Header().Get("Location") == "" {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		var got api.Task
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Kind != tc.kind || got.Subject == nil || got.Subject.Type != "server" || got.Subject.ID != "10.0.0.12" {
			t.Errorf("task %+v", got)
		}
	}
	if len(m.renew) != 1 || len(m.revoke) != 1 {
		t.Errorf("calls %v %v", m.renew, m.revoke)
	}
}

// The problems of the membership code reach the client unchanged, among them the
// dedicated 503 of an operation that needs the CA key on a leader without it.
func TestCertificateOperationsAnswerTheirProblems(t *testing.T) {
	for _, tc := range []struct {
		err    *problem.Problem
		status int
		code   string
	}{
		{problem.New(problem.CertificateAuthorityUnavailable, "no CA key"), 503, "certificate_authority_unavailable"},
		{problem.New(problem.CertificateRevoked, "revoked"), 409, "certificate_revoked"},
		{problem.New(problem.NotFound, "no server"), 404, "not_found"},
		{problem.New(problem.OperationInProgress, "busy"), 409, "operation_in_progress"},
	} {
		m := &certMembers{err: tc.err}
		e := newEnv(t, func(d *Deps) { d.Members = m })
		e.initialize()
		for _, path := range []string{"/api/v1/servers/db2/certificates/renewals", "/api/v1/servers/db2/certificates/revocations"} {
			wantProblem(t, e.do(req{method: "POST", path: path, bearer: e.token(auth.RoleAdmin)}), tc.status, tc.code)
		}
	}
}

func TestCertificateOperationsAreForwardedToTheLeaderAndAudited(t *testing.T) {
	fw := &direct{leader: "leader-1"}
	m := &certMembers{}
	leader := newEnv(t, func(d *Deps) { d.Members = m })
	leader.initialize()
	fw.target = leader.srv
	leader.testUser([]string{auth.RoleAdmin})
	follower := newEnv(t, func(d *Deps) { d.Forwarder = fw })
	follower.initialize()
	w := follower.do(req{method: "POST", path: "/api/v1/servers/db2/certificates/revocations", bearer: follower.token(auth.RoleAdmin)})
	if w.Code != http.StatusAccepted || fw.calls != 1 || len(m.revoke) != 1 {
		t.Fatalf("status %d, forwarded %d, leader calls %v", w.Code, fw.calls, m.revoke)
	}
	var n int
	for _, ev := range leader.audit.Events() {
		if ev.Action == "revokeServerCertificates" {
			n++
			if ev.ResourceType != "server" || ev.ResourceID != "db2" || ev.Details["forwardedBy"] != "follower-1" {
				t.Errorf("event %+v", ev)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d audit events", n)
	}
}
