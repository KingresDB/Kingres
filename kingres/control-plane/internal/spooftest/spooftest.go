// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package spooftest builds the database of the attack that extguard stops: a database that a tenant owns, with no KingresDB extension in it and a schema kingres
// that the tenant made. The schema has a view for every catalog name that the control plane reads and a function for every call it makes; each of them writes a row
// to public.spoof_marker when it runs. The service role is a superuser, so a row in the table proves that code of the tenant ran as a superuser.
//
// A test opens the instance with New, runs the operation of the control plane on the database spoofdb through ServiceConn (a connection of the service role with the
// search path fixed, as cluster.Service gives it) and asserts with NoMarkers that nothing of the tenant ran.
package spooftest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// Database is the name of the database that the tenant owns.
const Database = "spoofdb"

// View is a name the control plane reads in the schema kingres and the columns it reads.
type View struct{ Name, Columns string }

// Views are the catalog names read from the schema kingres by the paths that extguard gates. map_version is left out on purpose: kingres_shard is preloaded and its
// catalog loader (catalog.c) takes the six names node, colocation, dist_table, bucket_map, map_version and local_node for the tables of its catalog when all of them
// exist in the schema kingres of any database; a tenant that makes all six as views makes the backend that runs the next DDL there fail with a segmentation fault
// (the instance restarts). The bug is in the extension and is reported with the change; the test must not trigger it.
var Views = []View{
	{"node", "node_id int, group_id int, host text, port int, role text, is_active boolean, should_have_shards boolean, weight float8, server_group text"},
	{"dist_table", "relid regclass, method text, colocation_id int, partition_method text"},
	{"ledger_table", "relid regclass, signature_policy text"},
	{"schemas", "schema_name text, colocation_id int, group_id int, state text, node_id int, size_bytes bigint, moving_to_group_id int"},
	{"sharding_settings", "mode text"},
	{"colocation", "colocation_id int"},
	{"bucket_map", "colocation_id int, bucket int, group_id int, state text, modulus int"},
	{"bucket_move", "move_id bigint, phase text, detail text, colocation_id int, source_group int, target_group int"},
	{"bucket_split", "split_id bigint, phase text, detail text, rows_done bigint, rows_total bigint, colocation_id int"},
	{"partitions", "table_name regclass, partition_name name, method text, bound text, is_default boolean, leaf_count int, size_here bigint"},
	{"dist_partition", "parent regclass, partition_name name"},
	{"local_node", "node_id int"},
	{"table_operation", "op_id bigint, kind text, phase text, spec jsonb, detail text, verified text"},
	{"table_operations", "op_id bigint, rows_copied bigint, rows_total bigint"},
	{"catalog_status", "term bigint, superseded boolean"},
	{"timepart_policy", "schema_name name, table_name name, updated_at timestamptz, columnar_after interval, run_every interval, create_ahead interval, partition_every interval, retention interval, retention_action text, columnar_sort text[], columnar_options jsonb, timezone text, enabled boolean, next_run_at timestamptz, created_by text"},
	{"timepart_run", "run_id bigint, schema_name name, table_name name, state text, started_at timestamptz, finished_at timestamptz, as_of timestamptz"},
	{"timepart_run_data", "run_id bigint, state text, error text, finished_at timestamptz"},
	{"timepart_run_action", "run_id bigint, seq int, kind text"},
	{"timepart_run_action_data", "run_id bigint, state text"},
}

// Func is a function the control plane calls: Args is the argument list in the form of to_regprocedure, Returns what the call is
// read as.
type Func struct{ Name, Args, Returns string }

// Funcs are the calls of the control plane into the schema kingres.
var Funcs = []Func{
	{"set_sharding_mode", "text, boolean", "void"},
	{"schema_inventory", "regnamespace", "TABLE(object_kind text, object_name text, reason text, supported boolean)"},
	{"set_node_property", "integer, boolean", "void"},
	{"set_node_property", "integer, boolean, double precision", "void"},
	{"table_change_report", "text, regclass, jsonb", "TABLE(severity text, code text, object text, detail text)"},
	{"columnar_info", "regclass", "TABLE(compression_ratio float8)"},
	{"plan_rebalance", "text, double precision, integer, boolean, jsonb, integer[], integer[]", "TABLE(seq int)"},
	{"group_load", "text, jsonb, integer[]", "TABLE(group_id int)"},
	{"init_cluster", "text, integer", "void"},
	{"add_node", "text, integer", "int"},
	{"add_node", "text, integer, integer, text", "int"},
	{"remove_node", "integer", "void"},
	{"check_cluster", "", "TABLE(check_name text, node_id int, object text, detail text)"},
	{"rebalance_plan", "", "TABLE(colocation_id int, bucket int, source_group int, target_group int, size_bytes bigint)"},
	{"internal_group_node", "integer", "int"},
	{"internal_schema_pinned", "integer", "text"},
	{"internal_fk_component", "regclass, boolean", "TABLE(x int)"},
	{"internal_exec_scalar", "integer, text", "text"},
	{"internal_exec_scalar", "integer, text, integer", "text"},
	{"distribute_schema", "regnamespace, integer", "void"},
	{"undistribute_schema", "regnamespace", "void"},
	{"move_schema", "regnamespace, integer", "void"},
	{"move_bucket", "integer, integer, integer", "bigint"},
	{"resume_bucket_moves", "", "void"},
	{"cancel_bucket_move", "bigint", "void"},
	{"cleanup_moved_buckets", "", "int"},
	{"cleanup_moved_buckets", "boolean", "int"},
	{"create_reference_table", "regclass", "void"},
	{"create_distributed_table", "regclass, name, regclass, integer", "void"},
	{"undistribute_table", "regclass, boolean, integer", "void"},
	{"alter_distributed_table", "regclass, name, integer, text, boolean", "void"},
	{"begin_table_operation", "text, regclass, jsonb", "bigint"},
	{"table_operation_advance", "bigint, text", "text"},
	{"ledger_enable", "regclass, text", "void"},
	{"recover_prepared_transactions", "", "void"},
	{"sync_catalog", "", "void"},
	{"timepart_due", "timestamptz", "TABLE(schema_name text, table_name text)"},
	{"timepart_check", "", "TABLE(schema_name text, table_name text, problem text, detail text)"},
	{"timepart_unset_policy", "regclass", "void"},
	{"timepart_begin_run", "regclass, timestamptz, text", "bigint"},
	{"timepart_finish_run", "bigint", "void"},
	{"timepart_plan", "regclass, timestamptz", "TABLE(seq int, kind text, partition_name name, range_from timestamptz, range_to timestamptz, reason text)"},
	{"timepart_partitions", "regclass", "TABLE(range_to timestamptz, attached boolean)"},
	{"columnar_set_options", "regclass, text, integer, integer, integer", "void"},
}

// SQL is the script that makes the schema kingres of the tenant, one statement on every line. Every object writes the marker. A view reads a function that returns a table, so that the marker
// is written whatever the outer query asks of it.
func SQL() string {
	var b strings.Builder
	b.WriteString("CREATE TABLE public.spoof_marker (what text, at timestamptz DEFAULT now());\n")
	b.WriteString("CREATE SCHEMA kingres;\n")
	for _, v := range Views {
		fmt.Fprintf(&b, "CREATE FUNCTION kingres.spoof_%[1]s() RETURNS TABLE(%[2]s) LANGUAGE plpgsql AS $f$ BEGIN INSERT INTO public.spoof_marker(what) VALUES ('view %[1]s'); RETURN; END $f$;\n"+
			"CREATE VIEW kingres.%[1]s AS SELECT * FROM kingres.spoof_%[1]s();\n", v.Name, v.Columns)
	}
	for i, f := range Funcs {
		args := f.Args
		body := fmt.Sprintf("INSERT INTO public.spoof_marker(what) VALUES ('function %s #%d');", f.Name, i)
		switch {
		case strings.HasPrefix(f.Returns, "TABLE"):
			fmt.Fprintf(&b, "CREATE FUNCTION kingres.%s(%s) RETURNS %s LANGUAGE plpgsql AS $f$ BEGIN %s RETURN; END $f$;\n", f.Name, args, f.Returns, body)
		case f.Returns == "void":
			fmt.Fprintf(&b, "CREATE FUNCTION kingres.%s(%s) RETURNS void LANGUAGE plpgsql AS $f$ BEGIN %s END $f$;\n", f.Name, args, body)
		default:
			fmt.Fprintf(&b, "CREATE FUNCTION kingres.%s(%s) RETURNS %s LANGUAGE plpgsql AS $f$ BEGIN %s RETURN NULL; END $f$;\n", f.Name, args, f.Returns, body)
		}
	}
	return b.String()
}

// Env is the instance with the database of the tenant.
type Env struct {
	In *pgtest.Instance
}

// New starts an instance, makes the role tenant (not a superuser), the database spoofdb that it owns with a table public.t, the schema kingres of SQL() made by
// the tenant, and the system database with its schema (the operations read their settings there). The test is skipped when no PostgreSQL is given.
func New(t *testing.T) *Env {
	t.Helper()
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	admin := in.Connect(t, "postgres")
	for _, s := range []string{
		"CREATE ROLE tenant LOGIN PASSWORD 'tenant-pw'",
		"CREATE DATABASE " + Database + " OWNER tenant",
		"CREATE DATABASE " + sysdb.Database,
	} {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	sys := in.Connect(t, sysdb.Database)
	if _, err := sysdb.Migrate(ctx, sys); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(in.DSN(Database))
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = "tenant", "tenant-pw"
	tc, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting as the tenant: %v", err)
	}
	defer func() { _ = tc.Close(ctx) }()
	if _, err := tc.Exec(ctx, "CREATE TABLE public.t (id int PRIMARY KEY, v text)"); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(strings.TrimSpace(SQL()), "\n") {
		if _, err := tc.Exec(ctx, stmt); err != nil {
			dumpLogs(t, in.Root)
			t.Fatalf("making the schema of the tenant: %v: %s", err, stmt)
		}
	}
	return &Env{In: in}
}

// ServiceConn opens a connection of the service role to db with the search path fixed in the startup parameters, as cluster.Service does.
func (e *Env) ServiceConn(ctx context.Context, db string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(e.In.DSN(db))
	if err != nil {
		return nil, err
	}
	cluster.SafeSession(cfg)
	return pgx.ConnectConfig(ctx, cfg)
}

// Markers lists the rows of the marker table: the objects of the tenant that ran.
func (e *Env) Markers(t testing.TB) []string {
	t.Helper()
	c := e.In.Connect(t, Database)
	rows, err := c.Query(context.Background(), "SELECT what FROM public.spoof_marker ORDER BY at, what")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// NoMarkers fails the test when anything of the tenant ran as the service role.
func (e *Env) NoMarkers(t testing.TB, what string) {
	t.Helper()
	if m := e.Markers(t); len(m) > 0 {
		t.Fatalf("%s: code of the tenant ran as the service role (a superuser): %v", what, m)
	}
}

func dumpLogs(t *testing.T, root string) {
	_ = filepath.Walk(root, func(p string, i os.FileInfo, err error) error {
		if err == nil && !i.IsDir() && filepath.Ext(p) == ".log" {
			b, _ := os.ReadFile(p)
			if len(b) > 3000 {
				b = b[len(b)-3000:]
			}
			t.Logf("%s:\n%s", p, b)
		}
		return nil
	})
}

// NewUntrusted is New for the other attack: the tenant makes the schema kingres first (it owns it), and the service role then installs kingres_shard into it, so the
// extension is there and the schema is not the extension's alone. The tenant also makes an object of its own in the schema. The database has the table public.t and
// the marker table; Markers works as before.
func NewUntrusted(t *testing.T) *Env {
	t.Helper()
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	admin := in.Connect(t, "postgres")
	for _, s := range []string{
		"CREATE ROLE tenant LOGIN PASSWORD 'tenant-pw'",
		"CREATE DATABASE " + Database + " OWNER tenant",
		"CREATE DATABASE " + sysdb.Database,
	} {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	sys := in.Connect(t, sysdb.Database)
	if _, err := sysdb.Migrate(ctx, sys); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(in.DSN(Database))
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = "tenant", "tenant-pw"
	tc, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting as the tenant: %v", err)
	}
	defer func() { _ = tc.Close(ctx) }()
	for _, s := range []string{
		"CREATE TABLE public.t (id int PRIMARY KEY, v text)",
		"CREATE TABLE public.spoof_marker (what text, at timestamptz DEFAULT now())",
		"CREATE SCHEMA kingres",
	} {
		if _, err := tc.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	e := &Env{In: in}
	sc, err := e.ServiceConn(ctx, Database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sc.Close(ctx) }()
	if _, err := sc.Exec(ctx, "CREATE EXTENSION kingres_shard"); err != nil {
		t.Skipf("kingres_shard cannot be installed in this installation: %v", err)
	}
	// an object of the tenant in the schema it owns: a function that is not the extension's and writes the marker when it runs
	if _, err := tc.Exec(ctx, "CREATE FUNCTION kingres.tenant_probe() RETURNS int LANGUAGE sql AS 'INSERT INTO public.spoof_marker(what) VALUES (''probe'') RETURNING 1'"); err != nil {
		t.Fatal(err)
	}
	return e
}
