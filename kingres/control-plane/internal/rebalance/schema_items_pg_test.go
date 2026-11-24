// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// Tests of the distributed schemas of phase 11 as items of the plan, of the run and of the drain, on a stand-in of the extension's catalog. The stand-in is
// recorded as the extension's own by trustStub, as for every test of this package that needs the gate to let the control plane through; the real extension
// is exercised by the cluster tests (internal/clustertest, TestDrainOfAServerThatHostsADistributedSchema).

const (
	schemaA = "tenant_a"
	schemaB = `Ten"ant B` // a name that needs quoting
)

// schemaStub makes the catalog of a cluster of three servers in the database the guard environment maps every user database to: a hash group with
// buckets on groups 1 and 2, and two distributed schemas on group 1.
func schemaStub(t *testing.T, e *guardEnv) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	stub := e.in.Connect(t, "postgres")
	for _, q := range []string{
		`CREATE SCHEMA ` + schemaA,
		`CREATE SCHEMA "Ten""ant B"`,
		`CREATE TABLE kingres.colocation (colocation_id int PRIMARY KEY, kind text NOT NULL)`,
		`CREATE TABLE kingres.bucket_map (colocation_id int, bucket int, group_id int, state text NOT NULL DEFAULT 'active')`,
		`CREATE TABLE kingres.node (node_id int, group_id int, host text, port int, role text, is_active bool, should_have_shards bool)`,
		`CREATE TABLE kingres.dist_schema (nsp regnamespace PRIMARY KEY, colocation_id int)`,
		`CREATE TABLE kingres.bucket_move (move_id bigserial PRIMARY KEY, colocation_id int, bucket int, source_group int, target_group int, phase text, detail text, started_at timestamptz DEFAULT now())`,
		`CREATE TABLE kingres.stub_schemas (group_id int, schema_name text, colocation_id int, state text, size_bytes bigint, blocker text)`,
		`CREATE TABLE kingres.call_log (fn text, arg text, target int)`,
		`INSERT INTO kingres.colocation VALUES (1, 'hash'), (5, 'schema'), (6, 'schema')`,
		`INSERT INTO kingres.bucket_map VALUES (1, 0, 1), (1, 1, 1), (1, 2, 2), (5, 0, 1), (6, 0, 1)`,
		`INSERT INTO kingres.node VALUES (1, 1, '10.0.0.1', 5432, 'primary', true, true), (2, 2, '10.0.0.2', 5432, 'primary', true, true), (3, 3, '10.0.0.3', 5432, 'primary', true, true)`,
		`INSERT INTO kingres.dist_schema VALUES ('` + schemaA + `'::regnamespace, 5), ('"Ten""ant B"'::regnamespace, 6)`,
		`INSERT INTO kingres.stub_schemas VALUES (1, '` + schemaA + `', 5, 'active', 1000, NULL), (1, 'Ten"ant B', 6, 'active', 2000, NULL)`,
		// the plan of a drain: every bucket of an excluded group goes to group 3, schemas included, as the extension plans them
		`CREATE FUNCTION kingres.plan_rebalance(strategy text, threshold float8, max_moves int, drain_only bool, extra_load jsonb, exclude_groups int[], colocation_ids int[])
		   RETURNS TABLE (seq int, colocation_id int, bucket int, source_group int, target_group int, cost bigint, size_bytes bigint, reason text) LANGUAGE sql AS $$
		   SELECT (row_number() OVER (ORDER BY m.colocation_id, m.bucket))::int, m.colocation_id, m.bucket, m.group_id, 3, 1::bigint, NULL::bigint, 'drain'::text
		     FROM kingres.bucket_map m WHERE m.group_id = ANY (exclude_groups) $$`,
		`CREATE FUNCTION kingres.schemas_on_group(p_group int)
		   RETURNS TABLE (schema_name text, colocation_id int, state text, size_bytes bigint, blocker text) LANGUAGE sql AS $$
		   SELECT s.schema_name, s.colocation_id, s.state, s.size_bytes, s.blocker FROM kingres.stub_schemas s WHERE s.group_id = p_group ORDER BY s.schema_name $$`,
		`CREATE FUNCTION kingres.move_schema(nsp regnamespace, target_group int) RETURNS bigint LANGUAGE sql AS $$
		   INSERT INTO kingres.call_log VALUES ('move_schema', nsp::text, target_group) RETURNING 1::bigint $$`,
		`CREATE FUNCTION kingres.move_bucket(colocation_id int, bucket int, target_group int) RETURNS bigint LANGUAGE sql AS $$
		   INSERT INTO kingres.call_log VALUES ('move_bucket', colocation_id || '/' || bucket, target_group) RETURNING 1::bigint $$`,
	} {
		if _, err := stub.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	trustStub(t, stub)
	return stub
}

// The plan of a drain holds the distributed schemas of the server's group as items with their names; the item of a bucket has none.
func TestThePlanNamesTheSchemaItems(t *testing.T) {
	e, _ := newGuardEnv(t)
	schemaStub(t, e)
	b := &PG{Conns: e}
	moves, err := b.Plan(context.Background(), []string{"postgres"}, Options{DrainOnly: true, DrainServer: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		colo, bucket int32
		schema       string
	}{{1, 0, ""}, {1, 1, ""}, {5, 0, schemaA}, {6, 0, schemaB}}
	if len(moves) != len(want) {
		t.Fatalf("plan: %+v", moves)
	}
	for i, w := range want {
		m := moves[i]
		if m.Colocation != w.colo || m.Bucket != w.bucket || m.Schema != w.schema || m.From != 1 || m.To != 3 || m.Reason != "drain" {
			t.Errorf("item %d: %+v, want colocation %d bucket %d schema %q", i, m, w.colo, w.bucket, w.schema)
		}
	}
	// the other server holds one bucket and no schema: the join with dist_schema adds no name and no row
	moves, err = b.Plan(context.Background(), []string{"postgres"}, Options{DrainOnly: true, DrainServer: "10.0.0.2"})
	if err != nil || len(moves) != 1 || moves[0].Schema != "" {
		t.Fatalf("the plan of another server: %+v %v", moves, err)
	}
}

// The run of a schema item calls the extension's schema move with the schema's name (quoted when it needs it) and the target; a bucket still
// calls move_bucket. Both are made with the fence of the task run, so a run that lost its lease calls neither (TestRunMoveOfASupersededRunCallsNothing).
func TestARunOfASchemaItemCallsMoveSchema(t *testing.T) {
	e, lease := newGuardEnv(t)
	stub := schemaStub(t, e)
	ctx := fence.With(context.Background(), fence.Token{Task: e.id, Epoch: lease.Epoch})
	b := &PG{Conns: e, GuardHold: 30 * time.Second}
	for _, m := range []Move{
		{Database: "postgres", Colocation: 5, Bucket: 0, From: 1, To: 3, Schema: schemaA},
		{Database: "postgres", Colocation: 6, Bucket: 0, From: 1, To: 3, Schema: schemaB},
		{Database: "postgres", Colocation: 1, Bucket: 1, From: 1, To: 3},
	} {
		if err := b.RunMove(ctx, m, 0); err != nil {
			t.Fatalf("%+v: %v", m, err)
		}
	}
	rows, err := stub.Query(context.Background(), `SELECT fn, arg, target FROM kingres.call_log`)
	if err != nil {
		t.Fatal(err)
	}
	type call struct {
		fn, arg string
		target  int
	}
	var got []call
	for rows.Next() {
		var c call
		if err := rows.Scan(&c.fn, &c.arg, &c.target); err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	rows.Close()
	want := []call{{"move_schema", schemaA, 3}, {"move_schema", `"Ten""ant B"`, 3}, {"move_bucket", "1/1", 3}}
	if len(got) != len(want) {
		t.Fatalf("calls: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A run that lost its lease starts no schema move either: nothing is called and the call is not recorded as an attempt.
func TestARunOfASchemaItemWithoutTheLeaseCallsNothing(t *testing.T) {
	e, lease := newGuardEnv(t)
	stub := schemaStub(t, e)
	e.expire(t)
	if c, err := e.st.Claim(context.Background(), "B", time.Hour); err != nil || c == nil {
		t.Fatalf("B: %v %v", c, err)
	}
	ctx := fence.With(context.Background(), fence.Token{Task: e.id, Epoch: lease.Epoch})
	b := &PG{Conns: e}
	err := b.RunMove(ctx, Move{Database: "postgres", Colocation: 5, Bucket: 0, From: 1, To: 3, Schema: schemaA}, 0)
	if err == nil {
		t.Fatal("a run without the lease moved a schema")
	}
	var n int
	if err := stub.QueryRow(context.Background(), `SELECT count(*) FROM kingres.call_log`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("calls made without the lease: %d %v", n, err)
	}
}

// The name of the schema is kept with the row of the move (and of a skipped one) and read back, so the status and the report name it after a restart.
func TestTheSchemaOfAMoveIsKeptWithItsRow(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := fence.With(context.Background(), fence.Token{Task: e.id, Epoch: lease.Epoch})
	rows := &PGRows{Conns: e, TaskID: e.id}
	if _, err := rows.CreateRun(ctx, Options{DrainOnly: true}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rows.StartMove(ctx, Move{Database: "shop", Colocation: 6, Bucket: 0, From: 1, To: 3, Schema: schemaB}, "s1", "s3", 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := rows.AddSkipped(ctx, Move{Database: "shop", Colocation: 5, Bucket: 0, From: 1, To: 3, Schema: schemaA}, "s1", "s3", SkipTargetDiskLow, "full"); err != nil {
		t.Fatal(err)
	}
	if _, err := rows.StartMove(ctx, Move{Database: "shop", Colocation: 1, Bucket: 4, From: 1, To: 3}, "s1", "s3", 1, 0); err != nil {
		t.Fatal(err)
	}
	all, err := rows.Moves(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("%v %d", err, len(all))
	}
	if all[0].Move.Schema != schemaB || all[1].Move.Schema != schemaA || all[2].Move.Schema != "" {
		t.Fatalf("schemas read back: %q %q %q", all[0].Move.Schema, all[1].Move.Schema, all[2].Move.Schema)
	}
	rep := reportMoves(all)
	if rep[0].Kind != "schema" || rep[0].SchemaName == nil || *rep[0].SchemaName != schemaB || rep[2].Kind != "" {
		t.Fatalf("report: %+v", rep)
	}
}

// A distributed schema is a bucket of the server for the end of a drain: bucketsOn counts it, a move away that is not cleaned up yet counts too, and the
// step that waits for the server to empty fails naming the schemas while one remains, and ends when none does.
func TestADrainDoesNotEndWhileASchemaRemainsOnTheServer(t *testing.T) {
	e, _ := newGuardEnv(t)
	stub := schemaStub(t, e)
	ctx := context.Background()
	s := &Service{Conns: e, Backend: &PG{Conns: e}, DrainWait: time.Nanosecond}
	n, err := s.bucketsOn(ctx, "10.0.0.1")
	if err != nil || n != 4 {
		t.Fatalf("bucketsOn: %d %v (two buckets and two schemas)", n, err)
	}
	if note := s.schemaNote(ctx, "10.0.0.1"); note != ", 2 of them distributed schemas" {
		t.Fatalf("note %q", note)
	}
	// the buckets of the hash group have left; the schemas have not
	if _, err := stub.Exec(ctx, `UPDATE kingres.bucket_map SET group_id = 3 WHERE colocation_id = 1`); err != nil {
		t.Fatal(err)
	}
	if n, err = s.bucketsOn(ctx, "10.0.0.1"); err != nil || n != 2 {
		t.Fatalf("with only the schemas left: %d %v", n, err)
	}
	err = s.stepDrainEmpty(ctx, nil, nil, Options{DrainServer: "10.0.0.1"})
	if err == nil || codeOf(t, err) != problem.ServerOwnsBuckets {
		t.Fatalf("the drain ended with schemas on the server: %v", err)
	}
	if !strings.Contains(err.Error(), "2 of them distributed schemas") {
		t.Errorf("the refusal does not say that schemas remain: %v", err)
	}
	// one schema moved, the other is still being cleaned: the old copy keeps the server from being empty
	if _, err := stub.Exec(ctx, `UPDATE kingres.bucket_map SET group_id = 3 WHERE colocation_id = 5`); err != nil {
		t.Fatal(err)
	}
	if _, err := stub.Exec(ctx, `UPDATE kingres.bucket_map SET group_id = 3 WHERE colocation_id = 6`); err != nil {
		t.Fatal(err)
	}
	if _, err := stub.Exec(ctx, `INSERT INTO kingres.bucket_move (colocation_id, bucket, source_group, target_group, phase) VALUES (6, 0, 1, 3, 'cleaning')`); err != nil {
		t.Fatal(err)
	}
	if n, err = s.bucketsOn(ctx, "10.0.0.1"); err != nil || n != 1 {
		t.Fatalf("a schema in cleaning: %d %v", n, err)
	}
	if _, err := stub.Exec(ctx, `UPDATE kingres.bucket_move SET phase = 'done'`); err != nil {
		t.Fatal(err)
	}
	if err := s.stepDrainEmpty(ctx, nil, nil, Options{DrainServer: "10.0.0.1"}); err != nil {
		t.Fatalf("an empty server: %v", err)
	}
}

// The schemas on a server are read from the groups whose primary runs on it, with what keeps each from moving; the blockers that follow refuse the drain.
func TestTheSchemasOnAServerAndTheirBlockers(t *testing.T) {
	e, _ := newGuardEnv(t)
	stub := schemaStub(t, e)
	ctx := context.Background()
	s := &Service{Conns: e, Backend: &PG{Conns: e}}
	got, err := s.schemasOn(ctx, "10.0.0.1")
	if err != nil || len(got) != 2 {
		t.Fatalf("schemas: %+v %v", got, err)
	}
	if got[0].Schema != schemaB || got[1].Schema != schemaA || got[0].Group != 1 {
		t.Fatalf("schemas: %+v", got)
	}
	if bl := schemaBlockers(got); len(bl) != 0 {
		t.Fatalf("schemas that can move are blockers: %+v", bl)
	}
	if got, err = s.schemasOn(ctx, "10.0.0.2"); err != nil || len(got) != 0 {
		t.Fatalf("a server with no schema: %+v %v", got, err)
	}
	if _, err := stub.Exec(ctx, `UPDATE kingres.stub_schemas SET blocker = 'schema_pinned: schema "tenant_a" holds the ledger table(s) t on shard group 1' WHERE schema_name = 'tenant_a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := stub.Exec(ctx, `UPDATE kingres.stub_schemas SET blocker = 'table x.t is not logged: it is not replicated' WHERE schema_name <> 'tenant_a'`); err != nil {
		t.Fatal(err)
	}
	if got, err = s.schemasOn(ctx, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	bl := schemaBlockers(got)
	if len(bl) != 2 {
		t.Fatalf("blockers: %+v", bl)
	}
	codes := map[string]bool{}
	for _, b := range bl {
		codes[b.Code] = true
	}
	if !codes[string(problem.SchemaPinned)] || !codes[string(problem.Unprocessable)] {
		t.Fatalf("codes: %+v", bl)
	}
}
