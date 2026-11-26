// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// testService opens connections to the test instance; every database name
// maps to the one database that holds the system schema.
type testService struct {
	in *pgtest.Instance
	t  testing.TB
}

func (s testService) ServiceConn(ctx context.Context, _ string) (*pgx.Conn, error) {
	return pgx.Connect(ctx, s.in.DSN("postgres"))
}

func wantCode(t *testing.T, err error, code problem.Code) *problem.Problem {
	t.Helper()
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != code {
		t.Fatalf("error = %v, want problem %s", err, code)
	}
	return p
}

func TestIntegrationServerGroupsStore(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	c := in.Connect(t, "postgres")
	if _, err := sysdb.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		if _, err := c.Exec(ctx, `INSERT INTO kingres_cp.server (server_id) VALUES ($1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	o := &Ops{Service: testService{in, t}}

	// Create: exact, case-sensitive names; a duplicate is already_exists.
	g, err := o.CreateServerGroup(ctx, api.ServerGroupCreateRequest{Name: "rack-a", Description: "Row 1"})
	if err != nil || g.Name != "rack-a" || len(g.ServerIDs) != 0 {
		t.Fatalf("create: %+v, %v", g, err)
	}
	if _, err := o.CreateServerGroup(ctx, api.ServerGroupCreateRequest{Name: "Rack-A"}); err != nil {
		t.Fatalf("a name that differs in case is another group: %v", err)
	}
	_, err = o.CreateServerGroup(ctx, api.ServerGroupCreateRequest{Name: "rack-a"})
	wantCode(t, err, problem.AlreadyExists)

	// Assign, no-op, move, unassign.
	place := ServerPlacement{ShouldHaveShards: true}
	rack := func(s string) api.ServerUpdateRequest {
		return api.ServerUpdateRequest{ServerGroupSet: true, ServerGroup: &s}
	}
	if err := o.UpdateServer(ctx, "10.0.0.1", place, rack("rack-a")); err != nil {
		t.Fatal(err)
	}
	if err := o.UpdateServer(ctx, "10.0.0.2", place, rack("rack-a")); err != nil {
		t.Fatal(err)
	}
	if err := o.UpdateServer(ctx, "10.0.0.1", place, rack("rack-a")); err != nil {
		t.Fatalf("an equal value is a no-op: %v", err)
	}
	groups, err := o.ListServerGroups(ctx)
	if err != nil || len(groups) != 2 || groups[0].Name != "Rack-A" || groups[1].Name != "rack-a" ||
		strings.Join(groups[1].ServerIDs, ",") != "10.0.0.1,10.0.0.2" || len(groups[0].ServerIDs) != 0 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}

	// Refusals: an unknown group (422 with the field), an unknown server (404).
	p := wantCode(t, o.UpdateServer(ctx, "10.0.0.3", place, rack("nope")), problem.Unprocessable)
	if len(p.Errors) != 1 || p.Errors[0].Field != "/serverGroup" {
		t.Errorf("unknown group errors: %+v", p.Errors)
	}
	wantCode(t, o.UpdateServer(ctx, "10.9.9.9", place, rack("rack-a")), problem.NotFound)
	// Nothing changed on a refusal.
	var gr *string
	if err := c.QueryRow(ctx, `SELECT server_group FROM kingres_cp.server WHERE server_id = '10.0.0.3'`).Scan(&gr); err != nil || gr != nil {
		t.Errorf("server 3 group = %v, %v", gr, err)
	}

	// A group with servers cannot be deleted; the refusal lists them.
	p = wantCode(t, o.DeleteServerGroup(ctx, "rack-a"), problem.ServerGroupNotEmpty)
	if !strings.Contains(p.Detail, "10.0.0.1") || !strings.Contains(p.Detail, "10.0.0.2") {
		t.Errorf("detail = %q", p.Detail)
	}
	wantCode(t, o.DeleteServerGroup(ctx, "missing"), problem.NotFound)

	// Operations in progress: an unfinished add, remove or leave of the server
	// (any of the three kinds), a server that is being removed, a rebalance.
	move := rack("Rack-A")
	for _, kind := range []string{"server.add", "server.remove", "server.leave"} {
		var id int64
		if err := c.QueryRow(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by)
			VALUES ($1, 'running', 'server', '10.0.0.1', 't') RETURNING id`, kind).Scan(&id); err != nil {
			t.Fatal(err)
		}
		wantCode(t, o.UpdateServer(ctx, "10.0.0.1", place, move), problem.OperationInProgress)
		// An equal value changes nothing and is not refused.
		if err := o.UpdateServer(ctx, "10.0.0.1", place, rack("rack-a")); err != nil {
			t.Errorf("%s: no-op refused: %v", kind, err)
		}
		if _, err := c.Exec(ctx, `UPDATE kingres_cp.task SET state = 'done' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	// Another kind of task of the server, or a finished one, does not block.
	if _, err := c.Exec(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by)
		VALUES ('instance.restart', 'running', 'server', '10.0.0.1', 't')`); err != nil {
		t.Fatal(err)
	}
	if err := o.UpdateServer(ctx, "10.0.0.1", place, move); err != nil {
		t.Fatalf("a restart does not block: %v", err)
	}
	if _, err := c.Exec(ctx, `UPDATE kingres_cp.server SET status = 'removing' WHERE server_id = '10.0.0.2'`); err != nil {
		t.Fatal(err)
	}
	wantCode(t, o.UpdateServer(ctx, "10.0.0.2", place, move), problem.OperationInProgress)
	if _, err := c.Exec(ctx, `UPDATE kingres_cp.server SET status = 'active' WHERE server_id = '10.0.0.2'`); err != nil {
		t.Fatal(err)
	}
	var rb int64
	if err := c.QueryRow(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by)
		VALUES ('rebalance', 'running', 'cluster', 'cluster', 't') RETURNING id`).Scan(&rb); err != nil {
		t.Fatal(err)
	}
	wantCode(t, o.UpdateServer(ctx, "10.0.0.2", place, move), problem.RebalanceInProgress)
	if _, err := c.Exec(ctx, `UPDATE kingres_cp.task SET state = 'done' WHERE id = $1`, rb); err != nil {
		t.Fatal(err)
	}
	if err := o.UpdateServer(ctx, "10.0.0.2", place, move); err != nil {
		t.Fatal(err)
	}

	// Unassign everything, then the groups can go.
	null := api.ServerUpdateRequest{ServerGroupSet: true}
	for _, id := range []string{"10.0.0.1", "10.0.0.2"} {
		if err := o.UpdateServer(ctx, id, place, null); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"rack-a", "Rack-A"} {
		if err := o.DeleteServerGroup(ctx, n); err != nil {
			t.Errorf("delete %s: %v", n, err)
		}
	}
	if groups, err := o.ListServerGroups(ctx); err != nil || len(groups) != 0 {
		t.Errorf("groups after the deletes: %+v, %v", groups, err)
	}
}
