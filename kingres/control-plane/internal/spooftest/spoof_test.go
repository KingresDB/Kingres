// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package spooftest holds the test of the schema spoof: what a tenant (the owner of a database, or a role with CREATE on it) can plant in a database must never be
// executed by the service role, a superuser. The package has no code of its own.
package spooftest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerround"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store/pgstore"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// kingresNames are the relations of the schema kingres that the control plane reads; the tenant makes a view of each of them that writes a marker, and a function
// of each function that it calls.
// map_version and local_node are left out on purpose: the library of kingres_shard takes the catalog to be present when the six tables node, colocation,// dist_table, bucket_map, map_version and local_node exist in the schema kingres of any database, and then scans them; a view of a tenant that completes the list// makes the backend fail with a segmentation fault (see the security note).
var kingresNames = []string{
	"node", "bucket_map", "colocation", "dist_table", "dist_schema", "dist_partition", "sharding_settings",
	"ledger_blocks", "ledger_digest", "ledger_verification", "ledger_status", "ledger_table", "ledger_pending", "ledger_chain", "timepart_policy_x",
	"ledger_client_key", "ledger_client_key_revocation",
}

type env struct {
	in     *pgtest.Instance
	admin  *pgx.Conn
	tenant map[string]*pgx.Conn
}

// svc is the service role's connector: the startup parameters pin the search path, as the daemon's do.
type svc struct{ in *pgtest.Instance }

func (s svc) ServiceConn(ctx context.Context, db string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(s.in.DSN(db))
	if err != nil {
		return nil, err
	}
	cluster.SafeSession(cfg)
	return pgx.ConnectConfig(ctx, cfg)
}

// unpinned is the connection that the control plane used to open in a user database: the database's own settings apply.
func (s svc) unpinned(ctx context.Context, db string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(s.in.DSN(db))
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cfg)
}

func exec(t *testing.T, c *pgx.Conn, sql string) {
	t.Helper()
	start := time.Now()
	if _, err := c.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%.120s: %v (after %v, closed: %v)", sql, err, time.Since(start), c.IsClosed())
	}
	if d := time.Since(start); d > time.Second {
		t.Logf("slow statement (%v): %.80s", d, sql)
	}
}

// plant makes everything of the tenant in database db: the marker table, the function that writes a marker, the schema evil with functions, an aggregate and
// views that shadow what an unqualified statement of the control plane uses, the schema kingres with a view for every relation and a function for every
// function that the control plane reads, and the search path of the database.
func (e *env) plant(t *testing.T, db string, schemas ...string) {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(e.in.DSN(db))
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = "tenant", "tenant-pw"
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	e.tenant[db] = c
	exec(t, c, "CREATE SCHEMA ts")
	exec(t, c, "CREATE TABLE ts.marker (what text)")
	exec(t, c, "CREATE FUNCTION ts.spoof(w text) RETURNS boolean LANGUAGE sql AS 'INSERT INTO ts.marker VALUES (w) RETURNING true'")
	exec(t, c, "CREATE SCHEMA evil")
	exec(t, c, "CREATE FUNCTION evil.to_regclass(text) RETURNS regclass LANGUAGE sql AS 'SELECT CASE WHEN ts.spoof(''evil.to_regclass'') THEN NULL::regclass END'")
	exec(t, c, "CREATE FUNCTION evil.to_regprocedure(text) RETURNS regprocedure LANGUAGE sql AS 'SELECT CASE WHEN ts.spoof(''evil.to_regprocedure'') THEN NULL::regprocedure END'")
	exec(t, c, "CREATE FUNCTION evil.cnt(bigint) RETURNS bigint LANGUAGE sql AS 'SELECT CASE WHEN ts.spoof(''evil.count'') THEN $1 + 1 END'")
	exec(t, c, "CREATE AGGREGATE evil.count(*) (SFUNC = evil.cnt, STYPE = bigint, INITCOND = '0')")
	exec(t, c, "CREATE VIEW evil.pg_attribute AS SELECT attrelid, attnum, attname, atttypid, attisdropped, attnotnull FROM pg_catalog.pg_attribute WHERE ts.spoof('evil.pg_attribute')")
	for _, v := range []string{"pg_class", "pg_namespace", "pg_extension", "pg_database", "pg_index"} {
		exec(t, c, "CREATE VIEW evil."+v+" AS SELECT * FROM pg_catalog."+v+" WHERE ts.spoof('evil."+v+"')")
	}
	for _, s := range schemas {
		exec(t, c, "CREATE SCHEMA IF NOT EXISTS "+s)
		for _, n := range kingresNames {
			cols := "1::bigint AS ledger_txid, 1::int AS group_id, 'x'::text AS host, 1::bigint AS version, 1::int AS node_id, 1::int AS bucket_count, 1::int AS colocation_id"
			if n == "ledger_digest" {
				// the columns that the digest operation reads, so that the statements are valid and the function in them is what stops the planted code
				cols = "1::bigint AS digest_id, now() AS generated_at, 'x'::name AS generated_by, 1::bigint AS height, NULL::bytea AS block_hash, '{}'::jsonb AS digest"
			}
			exec(t, c, "CREATE VIEW "+s+"."+n+" AS SELECT "+cols+" WHERE ts.spoof('"+s+"."+n+"')")
		}
		exec(t, c, "CREATE FUNCTION "+s+".ledger_head() RETURNS TABLE (chain_id bytea, height bigint, block_hash bytea, closed_at_us bigint) LANGUAGE sql AS 'SELECT NULL::bytea, 1::bigint, NULL::bytea, 1::bigint WHERE ts.spoof(''"+s+".ledger_head'')'")
		exec(t, c, "CREATE FUNCTION "+s+".ledger_blocks_entries(bigint, integer) RETURNS TABLE (height bigint, entry bytea) LANGUAGE sql AS 'SELECT 1::bigint, NULL::bytea WHERE ts.spoof(''"+s+".ledger_blocks_entries'')'")
		exec(t, c, "CREATE FUNCTION "+s+".columnar_info(regclass) RETURNS bigint LANGUAGE sql AS 'SELECT 1::bigint WHERE ts.spoof(''"+s+".columnar_info'')'")
		exec(t, c, "CREATE FUNCTION "+s+".columnar_options(regclass) RETURNS bigint LANGUAGE sql AS 'SELECT 1::bigint WHERE ts.spoof(''"+s+".columnar_options'')'")
		exec(t, c, "CREATE FUNCTION "+s+".ledger_generate_digest() RETURNS jsonb LANGUAGE sql AS 'SELECT ''{}''::jsonb WHERE ts.spoof(''"+s+".ledger_generate_digest'')'")
		exec(t, c, "CREATE FUNCTION "+s+".self_test() RETURNS text LANGUAGE sql AS 'SELECT ''x'' WHERE ts.spoof(''"+s+".self_test'')'")
	}
	exec(t, e.admin, "ALTER DATABASE "+db+" SET search_path = evil, pg_catalog")
}

// markers returns what the planted functions wrote.
func (e *env) markers(t *testing.T, db string) []string {
	t.Helper()
	rows, err := e.tenant[db].Query(context.Background(), "SELECT what FROM ts.marker ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *env) clear(t *testing.T, db string) { exec(t, e.tenant[db], "TRUNCATE ts.marker") }

func TestSchemaSpoofIsNeverExecuted(t *testing.T) {
	in := pgtest.Start(t, nil)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		files, _ := filepath.Glob(filepath.Join(in.Root, "1", "pgdata", "log", "*"))
		for _, f := range files {
			if b, err := os.ReadFile(f); err == nil {
				lines := strings.Split(string(b), "\n")
				if len(lines) > 40 {
					lines = lines[len(lines)-40:]
				}
				t.Logf("the log of the server (%s):\n%s", filepath.Base(f), strings.Join(lines, "\n"))
			}
		}
	})
	ctx := context.Background()
	admin := in.Connect(t, "postgres")
	e := &env{in: in, admin: admin, tenant: map[string]*pgx.Conn{}}
	for _, s := range []string{
		"CREATE ROLE tenant LOGIN PASSWORD 'tenant-pw'",
		"CREATE DATABASE kingres_system",
		"CREATE DATABASE nox OWNER tenant",
		"CREATE DATABASE tpx OWNER tenant",
	} {
		exec(t, admin, s)
	}
	sysc := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, sysc); err != nil {
		t.Fatal(err)
	}
	s := svc{in}

	// nox: a database without the extensions, in which the tenant made the schemas kingres and kingres_sketch.
	e.plant(t, "nox", "kingres", "kingres_sketch")
	// tpx: a database that had kingres_timepart installed before the fix and whose schema the tenant got: the schema belongs to the tenant, and so do the
	// views in it.
	tpx := in.Connect(t, "tpx")
	if _, err := tpx.Exec(ctx, "CREATE EXTENSION kingres_timepart"); err != nil {
		t.Skipf("kingres_timepart is not available in this installation: %v", err)
	}
	exec(t, tpx, "ALTER SCHEMA kingres OWNER TO tenant")
	e.plant(t, "tpx", "kingres")

	// The setup works: a session of the old kind (the database's search path applies) does run the planted code as the service role.
	old, err := s.unpinned(ctx, "nox")
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := old.QueryRow(ctx, "SELECT count(*) FROM pg_class").Scan(&n); err != nil {
		t.Fatal(err)
	}
	var has bool
	if err := old.QueryRow(ctx, "SELECT to_regclass('kingres.ledger_pending') IS NOT NULL").Scan(&has); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(ctx, "SELECT min(ledger_txid) FROM kingres.ledger_pending"); err != nil {
		t.Fatal(err)
	}
	_ = old.Close(ctx)
	if m := e.markers(t, "nox"); len(m) == 0 {
		t.Fatal("the planted code did not run in an unpinned session: the test proves nothing")
	} else {
		t.Logf("an unpinned session ran %d planted calls (%s ...)", len(m), strings.Join(m[:min(3, len(m))], ", "))
	}
	e.clear(t, "nox")

	// 1. The monitor loop, which visits every database on a timer.
	r := &monitor.Runner{Src: runnerSrc{s}, Ring: monitor.NewRing(10, 100), Host: "h1", DataDir: t.TempDir(), System: "kingres_system", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.Once(ctx, time.Now())
	page, err := monitor.List(ctx, sysc, "open", "", store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var untrusted []string
	for _, a := range page.Items {
		if a.Kind == "extensionSchemaUntrusted" {
			untrusted = append(untrusted, a.Message)
		}
	}
	if len(untrusted) != 1 || !strings.Contains(untrusted[0], "database tpx") || !strings.Contains(untrusted[0], "kingres_timepart") {
		t.Errorf("the alert extensionSchemaUntrusted for tpx only (nox has no extension): %v", untrusted)
	}

	// 2. The listings and probes of the store.
	ps, err := pgstore.New(pgstore.Options{DSN: in.DSN("kingres_system"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	_, _ = ps.Databases(ctx)
	_, _ = ps.Database(ctx, "nox")
	_, _ = ps.Database(ctx, "tpx")
	_, _ = ps.ClusterSettings(ctx)
	for _, db := range []string{"nox", "tpx"} {
		_, _ = ps.Tables(ctx, db, store.TableFilter{}, store.Page{Limit: 20})
		_, _ = ps.ColocationGroups(ctx, db)
		_, _ = ps.LedgerStatus(ctx, db)
		_, _ = ps.LedgerBlocks(ctx, db, nil, store.Page{Limit: 5})
		_, _ = ps.Buckets(ctx, db, 1, store.BucketFilter{}, store.Page{Limit: 5})
		_, _ = ps.BucketDistribution(ctx, db, nil)
	}
	if d, err := ps.Database(ctx, "tpx"); err != nil || d.Sharding.Enabled {
		t.Errorf("a schema that cannot be trusted switches nothing on: %+v %v", d, err)
	}

	// 3. The ledger reads of a group (head, entries, status, digest).
	rd := &ledgerround.Reader{Conn: func(ctx context.Context, _ int32, db string) (*pgx.Conn, error) { return s.ServiceConn(ctx, db) }}
	for _, db := range []string{"nox", "tpx"} {
		for _, op := range []string{ledgerround.OpHead, ledgerround.OpEntries, ledgerround.OpStatus, ledgerround.OpDigest} {
			body, _ := json.Marshal(ledgerround.ReadRequest{Op: op, Database: db, Max: 5})
			if _, err := rd.Handle(ctx, body); err == nil {
				t.Errorf("%s %s: a ledger that is not there, or whose schema cannot be trusted, was read", db, op)
			}
		}
	}
	src := &ledgerround.ClusterSource{Conn: func(ctx context.Context, db string) (*pgx.Conn, error) { return s.ServiceConn(ctx, db) }, Self: "h1"}
	for _, db := range []string{"nox", "tpx"} {
		g, err := src.Groups(ctx, db)
		if db == "nox" && (err != nil || len(g) != 1) {
			t.Errorf("nox: no extension, one group: %v %v", g, err)
		}
	}

	// 4. The extension operations: the install path and the self test of kingres_sketch.
	o := &ops.Ops{Service: s}
	for _, kind := range []string{ops.ApplyExtensionInstall, ops.ApplyExtensionVerify} {
		for _, ext := range []string{extguard.Sketch, extguard.Timepart} {
			raw, _ := json.Marshal(map[string]any{"database": "nox", "extension": ext, "version": "1.0", "hash": "x"})
			err := o.ApplyLocal(ctx, kind, raw)
			if kind == ops.ApplyExtensionInstall && ext == extguard.Sketch && (err == nil || !strings.Contains(err.Error(), "cannot be trusted")) {
				t.Errorf("CREATE EXTENSION kingres_sketch over the schema that the tenant made must be refused by the control plane before the script runs: %v", err)
			}
		}
	}
	// The ledger operations of the control plane (digests, verifications, client keys): the extension is not there, or its schema cannot be trusted, so they refuse.
	for _, db := range []string{"nox", "tpx"} {
		_, err := o.GenerateDigest(ctx, db, "a")
		t.Logf("%s: GenerateDigest: %v", db, err)
		if err == nil {
			t.Errorf("%s: a digest was made", db)
		}
		// the lists are empty for a database without a ledger; what matters is that nothing of the tenant ran (the markers below)
		if p, err := o.ListDigests(ctx, db, store.Page{Limit: 5}); err == nil && len(p.Items) != 0 {
			t.Errorf("%s: digests were listed", db)
		}
		if p, err := o.ListVerifications(ctx, db, store.Page{Limit: 5}); err == nil && len(p.Items) != 0 {
			t.Errorf("%s: verifications were listed", db)
		}
		if _, err := o.ListClientKeys(ctx, db); err == nil {
			t.Errorf("%s: client keys were listed", db)
		}
	}
	rawT, _ := json.Marshal(map[string]any{"database": "tpx", "extension": extguard.Timepart, "version": "0.2"})
	if err := o.ApplyLocal(ctx, ops.ApplyExtensionInstall, rawT); err == nil || !strings.Contains(err.Error(), "cannot be trusted") {
		t.Errorf("an update of an extension whose schema cannot be trusted: %v", err)
	}
	if err := o.ApplyLocal(ctx, ops.ApplyExtensionVerify, rawT); err == nil || !strings.Contains(err.Error(), "cannot be trusted") {
		t.Errorf("a verification of an extension whose schema cannot be trusted: %v", err)
	}
	nox, err := s.ServiceConn(ctx, "nox")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nox.Close(ctx) }()
	for _, ext := range extguard.All {
		err := extguard.PreInstallExtension(ctx, nox, ext)
		if _, bad := extguard.IsUntrusted(err); !bad && (ext == extguard.Sketch || ext == extguard.Shard || ext == extguard.Ledger) {
			t.Errorf("PreInstall of %s over the schema of the tenant: %v", ext, err)
		}
	}
	var installed int
	if err := nox.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_extension WHERE extname LIKE 'kingres%'").Scan(&installed); err != nil || installed != 0 {
		t.Errorf("nothing was installed over the schema of the tenant: %d %v", installed, err)
	}

	// 5. The system database: when someone can create there, the monitor raises the alert for it (the readers of kingres_cp in it do not check an extension).
	if page, _ := monitor.List(ctx, sysc, "open", "", store.Page{Limit: 50}); func() bool {
		for _, a := range page.Items {
			if strings.Contains(a.Message, "kingres_system") {
				return true
			}
		}
		return false
	}() {
		t.Error("an alert for the system database before anything is wrong with it")
	}
	exec(t, sysc, "GRANT CREATE ON SCHEMA public TO PUBLIC")
	for i := 0; i < 3; i++ {
		r.Once(ctx, time.Now())
	}
	page, err = monitor.List(ctx, sysc, "open", "", store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	sysAlert := false
	for _, a := range page.Items {
		if a.Kind == "extensionSchemaUntrusted" && strings.Contains(a.Message, "database kingres_system") && strings.Contains(a.Message, "schemaCreate") {
			sysAlert = true
		}
	}
	if !sysAlert {
		t.Errorf("no alert extensionSchemaUntrusted for kingres_system: %+v", page.Items)
	}
	// What none of it did: nothing of the tenant was run, in either database.
	for _, db := range []string{"nox", "tpx"} {
		if m := e.markers(t, db); len(m) != 0 {
			t.Errorf("%s: the planted code ran as the service role: %v", db, m)
		}
	}
	_ = errors.New
}

// runnerSrc adapts the connector to the monitor's Source.
type runnerSrc struct{ svc }

func (runnerSrc) InstanceRunning(context.Context) (bool, error) { return true, nil }
