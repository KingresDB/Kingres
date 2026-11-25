// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The flag and the weight of a server are written to every database but the private database of an import: its catalog is the task's own, and a kept one is stale
// for ever. The two writers ask walkDatabases; a mutant of it that lists every database fails this test.
func TestTheFlagAndTheWeightWalkLeaveOutThePrivateDatabase(t *testing.T) {
	_, c, o := newGuardInstance(t)
	makePrivate(t, o, c)
	pgExecAll(t, c, "create database shop", "create database "+privDB[:len(privDB)-1]+"x")
	dbs, err := o.walkDatabases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(dbs, privDB) {
		t.Fatalf("the walk lists the private database: %v", dbs)
	}
	if !slices.Contains(dbs, "shop") || !slices.Contains(dbs, privDB[:len(privDB)-1]+"x") {
		t.Fatalf("the walk leaves out a database of a client: %v", dbs)
	}
}

// While an import runs, the flag and the weight of a server cannot change (409 operation_in_progress): the private database is left out of the walks, so it would keep
// the old value and the final database would have buckets on a server that the cluster means to empty. A change of the group is not affected.
func TestUpdateServerIsRefusedWhileAnImportRuns(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	c := in.Connect(t, "postgres")
	if _, err := sysdb.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	pgExecAll(t, c, "insert into kingres_cp.server (server_id) values ('10.0.0.1')", "insert into kingres_cp.server_group (name) values ('rack-a')")
	o := &Ops{Service: testService{in, t}}
	place := ServerPlacement{ShouldHaveShards: true, Weight: 1}
	off, w2 := false, 2.0
	for _, st := range []string{"pending", "running", "paused"} {
		var id int64
		if err := c.QueryRow(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ('database.import', $1, 'database', 'shop', 't') RETURNING id`, st).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for name, req := range map[string]api.ServerUpdateRequest{"the flag": {ShouldHaveShards: &off}, "the weight": {Weight: &w2}} {
			var p *problem.Problem
			if err := o.UpdateServer(ctx, "10.0.0.1", place, req); !errors.As(err, &p) || p.Code != problem.OperationInProgress {
				t.Errorf("%s of a server with an import %s: %v, want operation_in_progress", name, st, err)
			}
		}
		g := "rack-a"
		if err := o.UpdateServer(ctx, "10.0.0.1", place, api.ServerUpdateRequest{ServerGroupSet: true, ServerGroup: &g}); err != nil {
			t.Errorf("a change of the group while an import is %s: %v", st, err)
		}
		if err := o.UpdateServer(ctx, "10.0.0.1", place, api.ServerUpdateRequest{ShouldHaveShards: &place.ShouldHaveShards}); err != nil {
			t.Errorf("a value equal to the current one changes nothing: %v", err)
		}
		if _, err := c.Exec(ctx, `DELETE FROM kingres_cp.task WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	// an import that has ended does not block: the change goes on to the catalogs (the test instance has none, so it ends without a refusal)
	pgExecAll(t, c, "insert into kingres_cp.task (kind, state, subject_type, subject_id, created_by) values ('database.import', 'done', 'database', 'shop', 't'), ('database.import', 'failed', 'database', 'shop2', 't')")
	var p *problem.Problem
	if err := o.UpdateServer(ctx, "10.0.0.1", place, api.ServerUpdateRequest{Weight: &w2}); errors.As(err, &p) && p.Code == problem.OperationInProgress {
		t.Errorf("a finished import blocks the weight: %v", err)
	}
}
