// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// fakeLeaver is a Members that can also leave.
type fakeLeaver struct {
	calls []leaveCall
	err   error
}

type leaveCall struct {
	actor string
	force bool
}

func (f *fakeLeaver) AddServer(context.Context, string, api.AddServerRequest, string) (task.Task, error) {
	return task.Task{}, nil
}
func (f *fakeLeaver) RemoveServer(context.Context, string, string) (task.Task, error) {
	return task.Task{}, nil
}
func (f *fakeLeaver) LeaveCluster(_ context.Context, actor string, force bool) (task.Task, error) {
	f.calls = append(f.calls, leaveCall{actor, force})
	if f.err != nil {
		return task.Task{}, f.err
	}
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	return task.Task{ID: 41, Kind: "server.leave", State: task.Pending, SubjectType: "server", SubjectID: "db2", CreatedAt: now, UpdatedAt: now,
		CreatedBy: actor, Steps: []task.StepState{{Name: "Check that the server may leave", State: task.StepPending}}}, nil
}

func leaveEnv(t *testing.T, f *fakeLeaver) *env {
	t.Helper()
	e := newEnv(t, func(d *Deps) { d.Members = f })
	e.initialize()
	return e
}

const leavePath = "/api/v1/this-server/leave-cluster"

func TestLeaveClusterAcceptsATaskAndAuditsIt(t *testing.T) {
	f := &fakeLeaver{}
	e := leaveEnv(t, f)
	admin := e.token(auth.RoleAdmin)

	w := e.do(req{method: "POST", path: leavePath, bearer: admin})
	if w.Code != http.StatusAccepted || w.Header().Get("Location") != "/api/v1/tasks/41" {
		t.Fatalf("no body: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	m := decode(t, w)
	if m["kind"] != "server.leave" || m["subject"].(map[string]any)["type"] != "server" {
		t.Errorf("task: %v", m)
	}
	if w := e.do(req{method: "POST", path: leavePath, bearer: admin, body: map[string]any{"force": true}}); w.Code != 202 {
		t.Errorf("force: %d %s", w.Code, w.Body.String())
	}
	if w := e.do(req{method: "POST", path: leavePath, bearer: admin, body: map[string]any{"force": false}}); w.Code != 202 {
		t.Errorf("force false: %d", w.Code)
	}
	if len(f.calls) != 3 || f.calls[0].force || !f.calls[1].force || f.calls[2].force || f.calls[0].actor != "tester" {
		t.Errorf("calls: %+v", f.calls)
	}
	evs := e.audit.Events()
	if len(evs) != 3 || evs[0].Action != "leaveCluster" || evs[0].Outcome != audit.OutcomeSuccess || evs[1].Details["force"] != true {
		t.Errorf("audit: %+v", evs)
	}
}

func TestLeaveClusterRefusalsAreRelayed(t *testing.T) {
	f := &fakeLeaver{err: problem.New(problem.ServerOwnsBuckets, "buckets")}
	e := leaveEnv(t, f)
	admin := e.token(auth.RoleAdmin)
	wantProblem(t, e.do(req{method: "POST", path: leavePath, bearer: admin}), 409, "server_owns_buckets")
	f.err = problem.New(problem.NodeStateInvalid, "dead")
	wantProblem(t, e.do(req{method: "POST", path: leavePath, bearer: admin}), 422, "node_state_invalid")
	evs := e.audit.Events()
	if len(evs) != 2 || evs[0].Outcome != audit.OutcomeFailure {
		t.Errorf("a refused leave is audited as a failure: %+v", evs)
	}
}

func TestLeaveClusterNeedsAdminAndValidBody(t *testing.T) {
	f := &fakeLeaver{}
	e := leaveEnv(t, f)
	for _, role := range []string{auth.RoleOps, auth.RoleReadonly, auth.RoleStats} {
		if w := e.do(req{method: "POST", path: leavePath, bearer: e.token(role)}); w.Code != 403 {
			t.Errorf("%s: %d", role, w.Code)
		}
	}
	if w := e.do(req{method: "POST", path: leavePath}); w.Code != 401 {
		t.Errorf("no credentials: %d", w.Code)
	}
	admin := e.token(auth.RoleAdmin)
	for name, body := range map[string]any{
		"wrong type":      map[string]any{"force": "yes"},
		"unknown field":   map[string]any{"serverId": "db3"},
		"not an object":   `[true]`,
		"trailing data":   `{"force":true} {}`,
		"truncated":       `{"force":`,
		"unknown + force": map[string]any{"force": true, "x": 1},
	} {
		wantProblem(t, e.do(req{method: "POST", path: leavePath, bearer: admin, body: body}), 400, "bad_request")
		_ = name
	}
	wantProblem(t, e.do(req{method: "POST", path: leavePath, bearer: admin, body: `{"force":true}`, header: map[string]string{"Content-Type": "text/plain"}}), 400, "bad_request")
	if len(f.calls) != 0 {
		t.Errorf("a refused request reached the membership logic: %+v", f.calls)
	}
}

func TestLeaveClusterOnAServerWithoutMembersOrUninitialized(t *testing.T) {
	e := newEnv(t) // no Members
	e.initialize()
	wantProblem(t, e.do(req{method: "POST", path: leavePath, bearer: e.token(auth.RoleAdmin)}), 422, "unsupported_operation")

	// Members that cannot leave.
	e = newEnv(t, func(d *Deps) { d.Members = memberStub{} })
	e.initialize()
	wantProblem(t, e.do(req{method: "POST", path: leavePath, bearer: e.token(auth.RoleAdmin)}), 422, "unsupported_operation")

	// An uninitialized server answers 503 node_uninitialized: what a client
	// sees once the server has reset itself.
	e = newEnv(t, func(d *Deps) { d.Members = &fakeLeaver{} })
	wantProblem(t, e.do(req{method: "POST", path: leavePath}), 503, "node_uninitialized")
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/tasks/41"}), 503, "node_uninitialized")
}

// memberStub is a Members that is not a Leaver.
type memberStub struct{}

func (memberStub) AddServer(context.Context, string, api.AddServerRequest, string) (task.Task, error) {
	return task.Task{}, nil
}
func (memberStub) RemoveServer(context.Context, string, string) (task.Task, error) {
	return task.Task{}, nil
}

func (f *fakeLeaver) RenewServerCertificates(context.Context, string, string) (task.Task, error) {
	return task.Task{}, nil
}
func (f *fakeLeaver) RevokeServerCertificates(context.Context, string, string) (task.Task, error) {
	return task.Task{}, nil
}

func (memberStub) RenewServerCertificates(context.Context, string, string) (task.Task, error) {
	return task.Task{}, nil
}
func (memberStub) RevokeServerCertificates(context.Context, string, string) (task.Task, error) {
	return task.Task{}, nil
}
