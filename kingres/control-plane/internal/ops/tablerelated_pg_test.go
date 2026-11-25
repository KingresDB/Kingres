// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// Tests of distribute with related tables and of the operation indicator against a real catalog (PostgreSQL 18 or 19): the closure of the foreign keys,
// the drop and the add of the keys, and the records of the extension. The schema kingres is a stand-in with the tables these functions read; it is recorded
// as the extension of a superuser so that the guard lets the control plane read it, as the other tests of this package do.

func relatedStubInstance(t *testing.T) (*Ops, *pgx.Conn) {
	t.Helper()
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	pgExecAll(t, c,
		"CREATE SCHEMA kingres",
		"CREATE TABLE kingres.dist_table (relid regclass PRIMARY KEY)",
		"CREATE TABLE kingres.table_operation (op_id bigint GENERATED ALWAYS AS IDENTITY, kind text NOT NULL, mode text NOT NULL DEFAULT 'online', phase text NOT NULL, spec jsonb NOT NULL DEFAULT '{}', started_at timestamptz NOT NULL DEFAULT now())",
		"CREATE TABLE kingres.table_operation_item (op_id bigint NOT NULL, ordinal int NOT NULL, relid regclass NOT NULL, rows_copied bigint NOT NULL DEFAULT 0, rows_total bigint)",
		`INSERT INTO pg_catalog.pg_extension (oid, extname, extowner, extnamespace, extrelocatable, extversion)
		 SELECT pg_catalog.pg_nextoid('pg_catalog.pg_extension'::pg_catalog.regclass, 'oid', 'pg_catalog.pg_extension_oid_index'::pg_catalog.regclass),
		        'kingres_shard', (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user), 'kingres'::pg_catalog.regnamespace, false, '0.9'`,
		`INSERT INTO pg_catalog.pg_depend SELECT 'pg_catalog.pg_class'::pg_catalog.regclass, c.oid, 0, 'pg_catalog.pg_extension'::pg_catalog.regclass, e.oid, 0, 'e'
		   FROM pg_catalog.pg_class c, pg_catalog.pg_extension e WHERE c.relnamespace = 'kingres'::pg_catalog.regnamespace AND c.relkind IN ('r', 'v', 'p') AND e.extname = 'kingres_shard'`,
		"CREATE TABLE refs (id int PRIMARY KEY)",
		"CREATE TABLE customers (tenant_id int, id int, ref_id int, PRIMARY KEY (tenant_id, id), CONSTRAINT customers_ref_fk FOREIGN KEY (ref_id) REFERENCES refs (id))",
		"CREATE TABLE orders (tenant_id int, id int, customer_id int, PRIMARY KEY (tenant_id, id), CONSTRAINT orders_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id))",
		"CREATE TABLE items (tenant_id int, id int, order_id int, CONSTRAINT items_order_fk FOREIGN KEY (tenant_id, order_id) REFERENCES orders (tenant_id, id))",
		"CREATE TABLE lonely (id int PRIMARY KEY)",
		"INSERT INTO kingres.dist_table VALUES ('refs'::regclass)",
	)
	return &Ops{Service: pgSvc{in}}, c
}

func TestRelatedGraphAndPlanOnARealCatalog(t *testing.T) {
	o, _ := relatedStubInstance(t)
	ctx := context.Background()
	plan, err := o.ListRelatedTables(ctx, "postgres", "public", "customers", "tenant_id")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range plan.Tables {
		names = append(names, r.Table)
		if r.DistributionColumn == nil || *r.DistributionColumn != "tenant_id" {
			t.Errorf("%s column %v", r.Table, r.DistributionColumn)
		}
	}
	if strings.Join(names, ",") != "orders,items" {
		t.Errorf("tables %v", names)
	}
	if len(plan.Keys) != 2 || len(plan.Blockers) != 0 {
		t.Errorf("keys %d blockers %+v", len(plan.Keys), plan.Blockers)
	}
	// the key to the distributed table refs stays, with a warning, and the closure does not go through refs
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "related_table_distributed" {
		t.Errorf("warnings %+v", plan.Warnings)
	}
	lone, err := o.ListRelatedTables(ctx, "postgres", "public", "lonely", "id")
	if err != nil || len(lone.Tables) != 0 || len(lone.Keys) != 0 {
		t.Errorf("lonely %+v %v", lone, err)
	}
	if _, err := o.ListRelatedTables(ctx, "postgres", "public", "refs", ""); err == nil {
		t.Error("a distributed table has no plan")
	}
	if _, err := o.ListRelatedTables(ctx, "postgres", "public", "nothing", ""); err == nil {
		t.Error("a table that does not exist has a plan")
	}
}

func TestRelatedKeysAreDroppedAddedAndValidated(t *testing.T) {
	o, c := relatedStubInstance(t)
	ctx := context.Background()
	plan, err := o.ListRelatedTables(ctx, "postgres", "public", "customers", "tenant_id")
	if err != nil {
		t.Fatal(err)
	}
	var keys []relatedKey
	for _, k := range plan.Keys {
		keys = append(keys, relatedKey{Name: k.Name, Schema: k.Schema, Table: k.Table, RefSchema: k.ReferencesSchema, RefTable: k.ReferencesTable, Def: k.Definition, Validated: true})
	}
	b := pgBackend{o: o}
	p := tableParams{Database: "postgres", Schema: "public", Table: "customers"}
	count := func() int {
		var n int
		if err := c.QueryRow(ctx, "SELECT count(*) FROM pg_constraint WHERE conname IN ('orders_customer_fk', 'items_order_fk')").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := b.DropKeys(ctx, p, keys); err != nil || count() != 0 {
		t.Fatalf("drop: %v, %d keys left", err, count())
	}
	if err := b.DropKeys(ctx, p, keys); err != nil { // a repeated step
		t.Fatalf("drop again: %v", err)
	}
	failed, err := b.AddKeys(ctx, p, keys)
	if err != nil || len(failed) != 0 || count() != 2 {
		t.Fatalf("add: %v %v, %d keys", err, failed, count())
	}
	var valid bool
	if err := c.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conname = 'orders_customer_fk'").Scan(&valid); err != nil || valid {
		t.Errorf("a key is added NOT VALID: %v %v", valid, err)
	}
	if err := b.ValidateKeys(ctx, p, keys); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, "SELECT bool_and(convalidated) FROM pg_constraint WHERE conname IN ('orders_customer_fk', 'items_order_fk')").Scan(&valid); err != nil || !valid {
		t.Errorf("keys are valid after VALIDATE: %v %v", valid, err)
	}
	// a key that cannot be added is named, not fatal
	keys[0].Def = "FOREIGN KEY (nothing) REFERENCES customers (id)"
	_ = b.DropKeys(ctx, p, keys[:1])
	failed, err = b.AddKeys(ctx, p, keys[:1])
	if err != nil || len(failed) != 1 || !strings.Contains(failed[0], keys[0].Name) {
		t.Errorf("a key that does not hold: %v %v", failed, err)
	}
}

func TestTableOperationsReadTheRecordsOfTheExtension(t *testing.T) {
	o, c := relatedStubInstance(t)
	ctx := context.Background()
	pgExecAll(t, c,
		"INSERT INTO kingres.table_operation (kind, phase) VALUES ('distribute', 'copying'), ('alter', 'done'), ('distribute', 'cancelled')",
		"INSERT INTO kingres.table_operation_item (op_id, ordinal, relid, rows_copied, rows_total) VALUES (1, 0, 'orders'::regclass, 25, 100), (2, 0, 'items'::regclass, 5, 5), (3, 0, 'lonely'::regclass, 0, 0)",
	)
	ops := o.TableOperations(ctx, "postgres")
	if len(ops) != 1 {
		t.Fatalf("%+v", ops)
	}
	op := ops["postgres/public.orders"]
	if op.Kind != "distribute" || op.Source != "extension" || op.TaskID != nil || op.State != "copying" || op.Percent == nil || *op.Percent != 25 || op.StartedAt == nil {
		t.Errorf("%+v", op)
	}
}

func TestTableOperationsIgnoreAnExtensionThatCannotBeTrusted(t *testing.T) {
	o, c := relatedStubInstance(t)
	pgExecAll(t, c,
		"INSERT INTO kingres.table_operation (kind, phase) VALUES ('distribute', 'copying')",
		"INSERT INTO kingres.table_operation_item (op_id, ordinal, relid, rows_copied, rows_total) VALUES (1, 0, 'orders'::regclass, 25, 100)",
		// an object in the schema that no extension owns: the guard refuses the schema
		"CREATE VIEW kingres.planted AS SELECT 1 AS x",
	)
	if ops := o.TableOperations(context.Background(), "postgres"); len(ops) != 0 {
		t.Errorf("an untrusted schema was read: %+v", ops)
	}
}
