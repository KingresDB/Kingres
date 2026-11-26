// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package membership

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

func callErr(t *testing.T, err error) *internalapi.CallError {
	t.Helper()
	var ce *internalapi.CallError
	if !errors.As(err, &ce) {
		t.Fatalf("not a call error: %v", err)
	}
	return ce
}

// The leave call carries no server id: a body that names one is refused, so a
// server cannot ask for the removal of another through it.
func TestLeaveCallBodyCannotNameAServer(t *testing.T) {
	r := &Receiver{M: &Manager{Ops: &ops.Ops{}}}
	for _, body := range []string{
		`{"actor":"root","serverId":"db3"}`,
		`{"actor":"root","server":"db3"}`,
		`{"actor":"root","force":true,"subject":"db3"}`,
		`not json`,
		``,
	} {
		_, err := r.Handle(context.Background(), "POST /leave-cluster", "db2", []byte(body))
		if ce := callErr(t, err); ce.Status != http.StatusBadRequest {
			t.Errorf("body %q: %d %s", body, ce.Status, ce.Code)
		}
	}
	// The status call takes a task id only.
	_, err := r.Handle(context.Background(), "POST /leave-status", "db2", []byte(`{"taskId":1,"serverId":"db3"}`))
	if ce := callErr(t, err); ce.Status != http.StatusBadRequest {
		t.Errorf("status body naming a server: %d", ce.Status)
	}
}

func TestLeaveCallNeedsTheActor(t *testing.T) {
	m := &Manager{Ops: &ops.Ops{}}
	if _, err := m.LeaveFromPeer(context.Background(), "db2", wire.LeaveRequest{}); err == nil {
		t.Error("a leave without an actor was accepted")
	} else if p := new(problem.Problem); !errors.As(err, &p) || p.Code != problem.BadRequest {
		t.Errorf("got %v", err)
	}
}

// A server asks about its own leave task and no other: a task of another
// server, of another kind, or that does not exist, all answer 404 alike.
func TestLeaveStatusAnswersOnlyForTheSubjectServer(t *testing.T) {
	step := task.Step{Name: "s", Run: func(context.Context, *task.Env) error { return nil }}
	eng, err := task.NewEngine(task.NewMemStore(), []task.Definition{
		{Kind: KindLeave, Steps: []task.Step{step}}, {Kind: KindRemove, Steps: []task.Step{step}},
	}, task.Options{Holder: "t"})
	if err != nil {
		t.Fatal(err)
	}
	o := &ops.Ops{}
	o.SetEngine(eng)
	m := &Manager{Ops: o}
	ctx := context.Background()
	leave, err := eng.Submit(ctx, KindLeave, task.Subject{Type: "server", ID: "db2"}, leaveInput{ServerID: "db2"}, "root")
	if err != nil {
		t.Fatal(err)
	}
	remove, err := eng.Submit(ctx, KindRemove, task.Subject{Type: "server", ID: "db2"}, removeInput{ServerID: "db2"}, "root")
	if err != nil && !errors.Is(err, task.ErrSubjectBusy) {
		t.Fatal(err)
	}

	st, err := m.LeaveStatus(ctx, "db2", wire.LeaveStatusRequest{TaskID: leave.ID})
	if err != nil || st.State != "pending" {
		t.Errorf("own task: %v %v", st, err)
	}
	for name, c := range map[string]struct {
		peer string
		id   int64
	}{
		"another server's task": {"db3", leave.ID},
		"no such task":          {"db2", 9999},
		"a remove task":         {"db2", remove.ID},
	} {
		_, err := m.LeaveStatus(ctx, c.peer, wire.LeaveStatusRequest{TaskID: c.id})
		if p := new(problem.Problem); !errors.As(err, &p) || p.Code != problem.NotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestProblemOfAPIErrorKeepsTheContractCodes(t *testing.T) {
	for _, code := range []string{"server_owns_buckets", "server_hosts_map_group", "last_server", "rebalance_in_progress",
		"operation_in_progress", "node_state_invalid"} {
		st := 409
		if code == "node_state_invalid" {
			st = 422
		}
		p := problemOfAPIError(&peerclient.APIError{Status: st, Code: code, Message: "m"})
		if string(p.Code) != code || p.Status != st {
			t.Errorf("%s became %s %d", code, p.Code, p.Status)
		}
	}
	if p := problemOfAPIError(&peerclient.APIError{Status: 500, Code: "failed", Message: "m"}); p.Status != 503 {
		t.Errorf("an unknown failure of the leader: %d", p.Status)
	}
}
