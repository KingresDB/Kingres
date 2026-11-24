// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package extguard

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

func TestClassifyOrder(t *testing.T) {
	good := row{Name: "kingres_ledger", Version: "0.3", Schema: "kingres", ExtOwnerSuper: true, SchemaOwnerSuper: true}
	info, err := classify(good)
	if err != nil || !info.Present || info.Version != "0.3" || info.Schema != "kingres" || info.Name != "kingres_ledger" {
		t.Fatalf("a clean row is installed: %+v %v", info, err)
	}
	cases := []struct {
		name string
		mut  func(*row)
		want Reason
	}{
		{"extension owner", func(r *row) { r.ExtOwnerSuper = false }, ReasonExtensionOwner},
		{"schema owner", func(r *row) { r.SchemaOwnerSuper = false }, ReasonSchemaOwner},
		{"create grant", func(r *row) { r.CreateGrant = true }, ReasonSchemaCreate},
		{"foreign object", func(r *row) { r.Foreign = 2; r.FirstForeign = "view kingres.x" }, ReasonForeignObject},
		{"the first failing check wins", func(r *row) { r.ExtOwnerSuper, r.CreateGrant, r.Foreign = false, true, 1 }, ReasonExtensionOwner},
	}
	for _, c := range cases {
		r := good
		c.mut(&r)
		info, err := classify(r)
		u, ok := IsUntrusted(err)
		if !ok || u.Reason != c.want || !errors.Is(err, ErrUntrusted) {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !info.Present || info.Schema != "kingres" {
			t.Fatalf("%s: an untrusted extension still reports that it is there: %+v", c.name, info)
		}
		if errors.Is(err, ErrAbsent) {
			t.Fatalf("%s: untrusted is not absent", c.name)
		}
	}
}

func TestQueryShape(t *testing.T) {
	// Every name in the statement is qualified, so that a search_path that is not fixed cannot change what it means.
	for _, bad := range []string{" FROM pg_extension", " FROM pg_namespace", " JOIN pg_roles", " FROM pg_depend", "to_regclass", "count(*)::"} {
		if strings.Contains(verifySQL, bad) {
			t.Errorf("the statement uses an unqualified %q", bad)
		}
	}
	for _, need := range []string{"rolsuper", "nspowner", "nspacl", "deptype = 'e'", "pg_describe_object", "'CREATE'", "a.grantee = 0"} {
		if !strings.Contains(verifySQL, need) {
			t.Errorf("the statement has no %q", need)
		}
	}
}

// countingQ counts the statements that go to the database.
type countingQ struct {
	q Querier
	n atomic.Int64
}

func (c *countingQ) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n.Add(1)
	return c.q.Query(ctx, sql, args...)
}

// tenantDB is a database that the tenant role owns, with the schemas of the tests, opened as the service role with a fixed search_path.
type tenantDB struct {
	su, tenant *pgx.Conn
	in         *pgtest.Instance
}

func (d *tenantDB) mustSU(t *testing.T, sql string) {
	t.Helper()
	if _, err := d.su.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// startTenant makes a cluster with the role tenant (not a superuser) that owns database tdb, and returns two connections to it: the service role (superuser) with
// search_path pg_catalog fixed in the startup parameters, and the tenant.
func startTenant(t *testing.T) *tenantDB {
	t.Helper()
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	admin := in.Connect(t, "postgres")
	for _, s := range []string{
		"CREATE ROLE tenant LOGIN PASSWORD 'tenant-pw'",
		"CREATE DATABASE tdb OWNER tenant",
	} {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	open := func(user, pw string) *pgx.Conn {
		cfg, err := pgx.ParseConfig(in.DSN("tdb"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.User, cfg.Password = user, pw
		cfg.RuntimeParams["search_path"] = "pg_catalog"
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connecting as %s: %v", user, err)
		}
		t.Cleanup(func() { _ = c.Close(context.Background()) })
		return c
	}
	return &tenantDB{su: open("kingresd", pgtest.Password), tenant: open("tenant", "tenant-pw"), in: in}
}

func (d *tenantDB) install(t *testing.T, ext string) {
	t.Helper()
	if _, err := d.su.Exec(context.Background(), "CREATE EXTENSION "+ext); err != nil {
		var pe interface{ SQLState() string }
		if errors.As(err, &pe) && pe.SQLState() == "0A000" {
			t.Skipf("the extension %s is not available in this installation: %v", ext, err)
		}
		t.Fatalf("CREATE EXTENSION %s: %v", ext, err)
	}
}

func verifyReason(t *testing.T, q Querier, ext string) (Info, Reason) {
	t.Helper()
	info, err := Verify(context.Background(), q, ext)
	if err == nil {
		return info, ""
	}
	u, ok := IsUntrusted(err)
	if !ok {
		t.Fatalf("Verify(%s): %v", ext, err)
	}
	return info, u.Reason
}

func TestVerifyInstalledAbsentAndShared(t *testing.T) {
	d := startTenant(t)
	ctx := context.Background()
	if info, err := Verify(ctx, d.su, Sketch); err != nil || info.Present {
		t.Fatalf("an extension that is not installed is absent, not an error: %+v %v", info, err)
	}
	if _, err := Require(ctx, d.su, Sketch); !errors.Is(err, ErrAbsent) {
		t.Fatalf("Require of an absent extension: %v", err)
	}
	d.install(t, Sketch)
	info, err := Verify(ctx, d.su, Sketch)
	if err != nil || !info.Present || info.Schema != "kingres_sketch" || info.Version == "" {
		t.Fatalf("a clean install is trusted: %+v %v", info, err)
	}
	if _, err := Require(ctx, d.su, Sketch); err != nil {
		t.Fatal(err)
	}
	// Two extensions in one schema (kingres): the members of each are members of an extension, so both are trusted.
	d.install(t, Timepart)
	if tp, err := Verify(ctx, d.su, Timepart); err != nil || tp.Schema != "kingres" {
		t.Fatalf("timepart: %+v %v", tp, err)
	}
	if rs, err := VerifyAll(ctx, d.su, Timepart, Sketch, Shard); err != nil || len(rs) != 3 || !rs[0].Info.Present || !rs[1].Info.Present || rs[2].Info.Present || rs[0].Err != nil || rs[1].Err != nil {
		t.Fatalf("VerifyAll: %+v %v", rs, err)
	}
	if _, err := d.su.Exec(ctx, "CREATE EXTENSION kingres_columnar"); err == nil {
		if col, err := Verify(ctx, d.su, Timepart); err != nil || !col.Present {
			t.Fatalf("a second extension in the schema kingres leaves the first trusted: %+v %v", col, err)
		}
	}
	// The same through a transaction and a pool of one: the Querier is what pgx offers.
	tx, err := d.su.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := Verify(ctx, tx, Sketch); err != nil {
		t.Fatal(err)
	}
}

// The failing cases: every one makes the extension untrusted, with the reason of its check.
func TestVerifyUntrustedCases(t *testing.T) {
	d := startTenant(t)
	d.install(t, Sketch)
	d.install(t, Timepart)
	ctx := context.Background()
	if _, r := verifyReason(t, d.su, Sketch); r != "" {
		t.Fatalf("clean: %s", r)
	}
	// A function that is not a member, put into the schema (here by the superuser: the check is about what is there, not about who put it).
	d.mustSU(t, "CREATE FUNCTION kingres.planted() RETURNS int LANGUAGE sql AS 'SELECT 1'")
	if _, r := verifyReason(t, d.su, Timepart); r != ReasonForeignObject {
		t.Fatalf("a function added to kingres: %q", r)
	}
	// The first extension is then untrusted too: they share the schema; the sketch extension is in another schema and stays trusted.
	if _, r := verifyReason(t, d.su, Sketch); r != "" {
		t.Fatalf("another schema is not affected: %q", r)
	}
	d.mustSU(t, "DROP FUNCTION kingres.planted()")
	if _, r := verifyReason(t, d.su, Timepart); r != "" {
		t.Fatalf("after the removal: %q", r)
	}
	// A view that is not a member.
	d.mustSU(t, "CREATE VIEW kingres_sketch.ledger_pending AS SELECT 1 AS ledger_txid")
	if info, r := verifyReason(t, d.su, Sketch); r != ReasonForeignObject || !info.Present {
		t.Fatalf("a view that is not a member: %q", r)
	}
	d.mustSU(t, "DROP VIEW kingres_sketch.ledger_pending")
	for _, c := range []struct {
		name, bad, undo string
		want            Reason
	}{
		{"a type", "CREATE TYPE kingres_sketch.t AS (a int)", "DROP TYPE kingres_sketch.t", ReasonForeignObject},
		{"a table", "CREATE TABLE kingres_sketch.tab (a int)", "DROP TABLE kingres_sketch.tab", ReasonForeignObject},
		{"a sequence", "CREATE SEQUENCE kingres_sketch.sq", "DROP SEQUENCE kingres_sketch.sq", ReasonForeignObject},
		{"an operator", "CREATE OPERATOR kingres_sketch.### (LEFTARG = int, RIGHTARG = int, FUNCTION = int4pl)", "DROP OPERATOR kingres_sketch.### (int, int)", ReasonForeignObject},
		{"a schema owned by the tenant", "ALTER SCHEMA kingres_sketch OWNER TO tenant", "ALTER SCHEMA kingres_sketch OWNER TO kingresd", ReasonSchemaOwner},
		{"CREATE granted to PUBLIC", "GRANT CREATE ON SCHEMA kingres_sketch TO PUBLIC", "REVOKE CREATE ON SCHEMA kingres_sketch FROM PUBLIC", ReasonSchemaCreate},
		{"CREATE granted to the tenant", "GRANT CREATE ON SCHEMA kingres_sketch TO tenant", "REVOKE CREATE ON SCHEMA kingres_sketch FROM tenant", ReasonSchemaCreate},
		{"an extension owned by a role that is not a superuser", "UPDATE pg_catalog.pg_extension SET extowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = 'tenant') WHERE extname = 'kingres_sketch'",
			"UPDATE pg_catalog.pg_extension SET extowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = 'kingresd') WHERE extname = 'kingres_sketch'", ReasonExtensionOwner},
	} {
		d.mustSU(t, c.bad)
		if _, r := verifyReason(t, d.su, Sketch); r != c.want {
			t.Errorf("%s: reason %q, want %q", c.name, r, c.want)
		}
		d.mustSU(t, c.undo)
		if _, r := verifyReason(t, d.su, Sketch); r != "" {
			t.Fatalf("%s: still untrusted after the undo: %q", c.name, r)
		}
	}
	// Require returns the untrusted error, not absent.
	d.mustSU(t, "GRANT CREATE ON SCHEMA kingres_sketch TO PUBLIC")
	if _, err := Require(ctx, d.su, Sketch); !errors.Is(err, ErrUntrusted) || errors.Is(err, ErrAbsent) {
		t.Fatalf("Require: %v", err)
	}
}

// The attack of the audit, as far as the extension goes: the tenant makes the schema and a view of its own, and the extension is never created. Nothing is
// installed; the schema does not make an extension.
func TestTenantSchemaWithoutExtensionIsAbsent(t *testing.T) {
	d := startTenant(t)
	for _, s := range []string{"CREATE SCHEMA kingres", "CREATE VIEW kingres.ledger_pending AS SELECT 1 AS ledger_txid"} {
		if _, err := d.tenant.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	info, err := Verify(context.Background(), d.su, Ledger)
	if err != nil || info.Present {
		t.Fatalf("a schema of the tenant does not make the extension: %+v %v", info, err)
	}
}

func TestGuardCaches(t *testing.T) {
	d := startTenant(t)
	d.install(t, Sketch)
	cq := &countingQ{q: d.su}
	g := NewGuard(cq)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := g.Verify(ctx, Sketch); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Require(ctx, Sketch); err != nil {
			t.Fatal(err)
		}
		if info, err := g.Verify(ctx, Ledger); err != nil || info.Present {
			t.Fatalf("%+v %v", info, err)
		}
	}
	if n := cq.n.Load(); n != 2 {
		t.Fatalf("two names, two statements; got %d", n)
	}
	// An untrusted answer is cached too (and stays untrusted for the guard even if the catalog changes: the guard is for one transaction).
	d.mustSU(t, "GRANT CREATE ON SCHEMA kingres_sketch TO PUBLIC")
	g2 := NewGuard(cq)
	for i := 0; i < 3; i++ {
		if _, err := g2.Verify(ctx, Sketch); !errors.Is(err, ErrUntrusted) {
			t.Fatalf("%v", err)
		}
	}
	if n := cq.n.Load(); n != 3 {
		t.Fatalf("the untrusted answer is cached: %d statements", n)
	}
	// An error of the query is not cached and is not "absent".
	bad := NewGuard(&failingQ{})
	for i := 0; i < 2; i++ {
		if info, err := bad.Verify(ctx, Sketch); err == nil || info.Present {
			t.Fatalf("%+v %v", info, err)
		}
	}
}

type failingQ struct{ n int }

func (f *failingQ) Query(context.Context, string, ...any) (pgx.Rows, error) {
	f.n++
	return nil, errors.New("connection lost")
}

// Mutants: each check of the statement, removed in turn, lets a case through. The cases are the ones of TestVerifyUntrustedCases; a mutant that is not killed
// would mean that the check is not tested.
func TestMutantsAreKilled(t *testing.T) {
	d := startTenant(t)
	d.install(t, Sketch)
	orig := querySQL
	defer func() { querySQL = orig }()
	cases := []struct {
		name         string
		from, to     string
		bad, undo    string
		wantOnOrigin Reason
	}{
		{"extension owner not checked", "COALESCE((SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.oid = e.extowner), false)", "true",
			"UPDATE pg_catalog.pg_extension SET extowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = 'tenant') WHERE extname = 'kingres_sketch'",
			"UPDATE pg_catalog.pg_extension SET extowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = 'kingresd') WHERE extname = 'kingres_sketch'", ReasonExtensionOwner},
		{"schema owner not checked", "COALESCE((SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.oid = ns.nspowner), false)", "true",
			"ALTER SCHEMA kingres_sketch OWNER TO tenant", "ALTER SCHEMA kingres_sketch OWNER TO kingresd", ReasonSchemaOwner},
		{"CREATE of a role not checked", "a.privilege_type = 'CREATE'", "a.privilege_type = 'NOPE'",
			"GRANT CREATE ON SCHEMA kingres_sketch TO tenant", "REVOKE CREATE ON SCHEMA kingres_sketch FROM tenant", ReasonSchemaCreate},
		{"CREATE of PUBLIC not checked", "a.grantee = 0 OR NOT EXISTS", "a.grantee <> 0 AND NOT EXISTS",
			"GRANT CREATE ON SCHEMA kingres_sketch TO PUBLIC", "REVOKE CREATE ON SCHEMA kingres_sketch FROM PUBLIC", ReasonSchemaCreate},
		{"the grantee that is a superuser is not excused", "g.rolsuper)", "true)",
			"GRANT CREATE ON SCHEMA kingres_sketch TO tenant", "REVOKE CREATE ON SCHEMA kingres_sketch FROM tenant", ReasonSchemaCreate},
		{"relations not looked at", "ns JOIN pg_catalog.pg_class c ON c.relnamespace = ns.nsoid WHERE", "ns JOIN pg_catalog.pg_class c ON c.relnamespace = ns.nsoid WHERE false AND",
			"CREATE VIEW kingres_sketch.v AS SELECT 1", "DROP VIEW kingres_sketch.v", ReasonForeignObject},
		{"functions not looked at", "ns JOIN pg_catalog.pg_proc p ON p.pronamespace = ns.nsoid", "ns JOIN pg_catalog.pg_proc p ON p.pronamespace = ns.nsoid AND false",
			"CREATE FUNCTION kingres_sketch.planted() RETURNS int LANGUAGE sql AS 'SELECT 1'", "DROP FUNCTION kingres_sketch.planted()", ReasonForeignObject},
		{"types not looked at", "WHERE t.typrelid = 0 AND", "WHERE false AND",
			"CREATE DOMAIN kingres_sketch.dom AS int", "DROP DOMAIN kingres_sketch.dom", ReasonForeignObject},
		{"operators not looked at", "ON o.oprnamespace = ns.nsoid", "ON o.oprnamespace = ns.nsoid AND false",
			"CREATE OPERATOR kingres_sketch.### (LEFTARG = int, RIGHTARG = int, FUNCTION = int4pl)", "DROP OPERATOR kingres_sketch.### (int, int)", ReasonForeignObject},
		// Mutants of the other direction: the clean install must stay trusted, so a wrong test of membership shows as an untrusted extension.
		{"membership by the wrong dependency type", "d.deptype = 'e' AND r.rolsuper)\n     AND NOT EXISTS", "d.deptype = 'n' AND r.rolsuper)\n     AND NOT EXISTS", "", "", ""},
		{"membership of an extension of a superuser inverted", "d.deptype = 'e' AND r.rolsuper)\n     AND NOT EXISTS", "d.deptype = 'e' AND NOT r.rolsuper)\n     AND NOT EXISTS", "", "", ""},
		{"foreign objects not counted", "(SELECT count(*) FROM foreign_objs f WHERE f.extname = e.extname)", "0::bigint",
			"CREATE FUNCTION kingres_sketch.planted() RETURNS int LANGUAGE sql AS 'SELECT 1'", "DROP FUNCTION kingres_sketch.planted()", ReasonForeignObject},
	}
	for _, c := range cases {
		querySQL = orig
		if c.bad != "" {
			d.mustSU(t, c.bad)
		}
		if _, r := verifyReason(t, d.su, Sketch); r != c.wantOnOrigin {
			t.Fatalf("%s: the real statement says %q, want %q", c.name, r, c.wantOnOrigin)
		}
		if !strings.Contains(orig, c.from) {
			t.Fatalf("%s: the mutated text is not in the statement: %q", c.name, c.from)
		}
		querySQL = strings.Replace(orig, c.from, c.to, 1)
		if _, r := verifyReason(t, d.su, Sketch); r == c.wantOnOrigin {
			t.Errorf("mutant survived: %s (the case is still reported as %q)", c.name, r)
		}
		querySQL = orig
		if c.undo != "" {
			d.mustSU(t, c.undo)
		}
	}
}

func TestObjectExtensionAndNames(t *testing.T) {
	for name, want := range map[string]string{
		"kingres.node": Shard, "bucket_map": Shard, "kingres.ledger_blocks": Ledger, "ledger_head": Ledger, "kingres.timepart_policy": Timepart,
		"create_time_partitions": Timepart, "kingres.columnar_info": Columnar, "dist_table": Shard,
	} {
		if got := ObjectExtension(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	for _, n := range All {
		if !IsKingres(n) {
			t.Errorf("%s", n)
		}
	}
	if IsKingres("pgcrypto") || IsKingres("") || IsKingres("kingres_other") {
		t.Error("only the extensions of KingresDB")
	}
	if _, err := StatusSQL("pgcrypto"); err == nil {
		t.Error("StatusSQL takes only the extensions of KingresDB (its text carries the name)")
	}
}

// PreInstall: the schema that an extension is created in.
func TestPreInstallAndStatusSQL(t *testing.T) {
	d := startTenant(t)
	ctx := context.Background()
	pre := func() Reason {
		t.Helper()
		err := PreInstall(ctx, d.su, Sketch, "kingres_sketch")
		if err == nil {
			return ""
		}
		u, ok := IsUntrusted(err)
		if !ok {
			t.Fatal(err)
		}
		if u.Extension != Sketch {
			t.Fatalf("the error names the extension: %+v", u)
		}
		return u.Reason
	}
	if r := pre(); r != "" {
		t.Fatalf("a schema that does not exist: %q", r)
	}
	if err := PreInstallExtension(ctx, d.su, Sketch); err != nil {
		t.Fatal(err)
	}
	if s, err := ExtensionSchema(ctx, d.su, Sketch); err != nil || s != "kingres_sketch" {
		t.Fatalf("the control file schema: %q %v", s, err)
	}
	d.mustSU(t, "CREATE SCHEMA kingres_sketch")
	if r := pre(); r != "" {
		t.Fatalf("an empty schema of the superuser: %q", r)
	}
	for _, c := range []struct {
		name, bad, undo string
		want            Reason
	}{
		{"a schema of the tenant", "ALTER SCHEMA kingres_sketch OWNER TO tenant", "ALTER SCHEMA kingres_sketch OWNER TO kingresd", ReasonSchemaOwner},
		{"CREATE for PUBLIC", "GRANT CREATE ON SCHEMA kingres_sketch TO PUBLIC", "REVOKE CREATE ON SCHEMA kingres_sketch FROM PUBLIC", ReasonSchemaCreate},
		{"CREATE for the tenant", "GRANT CREATE ON SCHEMA kingres_sketch TO tenant", "REVOKE CREATE ON SCHEMA kingres_sketch FROM tenant", ReasonSchemaCreate},
		{"an object that is not a member", "CREATE VIEW kingres_sketch.v AS SELECT 1", "DROP VIEW kingres_sketch.v", ReasonForeignObject},
	} {
		d.mustSU(t, c.bad)
		if r := pre(); r != c.want {
			t.Errorf("%s: %q, want %q", c.name, r, c.want)
		}
		d.mustSU(t, c.undo)
	}
	// The same checks through the statement for another server (no parameters): reason "" for a trusted extension, no row for an absent one.
	statusOf := func(ext string) (string, string, bool) {
		sql, err := StatusSQL(ext)
		if err != nil {
			t.Fatal(err)
		}
		var v, reason string
		err = d.su.QueryRow(ctx, sql).Scan(&v, &reason)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		return v, reason, true
	}
	d.mustSU(t, "DROP SCHEMA kingres_sketch")
	if _, _, ok := statusOf(Sketch); ok {
		t.Fatal("absent: no row")
	}
	d.install(t, Sketch)
	for _, c := range []struct {
		bad, undo string
		want      Reason
	}{
		{"", "", ""},
		{"ALTER SCHEMA kingres_sketch OWNER TO tenant", "ALTER SCHEMA kingres_sketch OWNER TO kingresd", ReasonSchemaOwner},
		{"GRANT CREATE ON SCHEMA kingres_sketch TO PUBLIC", "REVOKE CREATE ON SCHEMA kingres_sketch FROM PUBLIC", ReasonSchemaCreate},
		{"CREATE FUNCTION kingres_sketch.planted() RETURNS int LANGUAGE sql AS 'SELECT 1'", "DROP FUNCTION kingres_sketch.planted()", ReasonForeignObject},
	} {
		if c.bad != "" {
			d.mustSU(t, c.bad)
		}
		v, reason, ok := statusOf(Sketch)
		_, vr := verifyReason(t, d.su, Sketch)
		if !ok || v == "" || Reason(reason) != c.want || vr != c.want {
			t.Errorf("%q: StatusSQL %q %q, Verify %q, want %q", c.bad, v, reason, vr, c.want)
		}
		if c.undo != "" {
			d.mustSU(t, c.undo)
		}
	}
}

// Mutants of PreInstall.
func TestPreInstallMutantsAreKilled(t *testing.T) {
	d := startTenant(t)
	d.mustSU(t, "CREATE SCHEMA kingres_sketch")
	orig := preInstallSQL
	defer func() { preInstallSQL = orig }()
	check := func() Reason {
		err := PreInstall(context.Background(), d.su, Sketch, "kingres_sketch")
		if u, ok := IsUntrusted(err); ok {
			return u.Reason
		}
		if err != nil {
			t.Fatal(err)
		}
		return ""
	}
	for _, c := range []struct {
		name, from, to, bad, undo string
		want                      Reason
	}{
		{"owner not checked", "COALESCE((SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.oid = ns.nspowner), false)", "true",
			"ALTER SCHEMA kingres_sketch OWNER TO tenant", "ALTER SCHEMA kingres_sketch OWNER TO kingresd", ReasonSchemaOwner},
		{"CREATE not checked", "a.privilege_type = 'CREATE'", "a.privilege_type = 'NOPE'",
			"GRANT CREATE ON SCHEMA kingres_sketch TO PUBLIC", "REVOKE CREATE ON SCHEMA kingres_sketch FROM PUBLIC", ReasonSchemaCreate},
		{"members not checked", "(SELECT count(*) FROM foreign_objs f WHERE f.extname = e.extname)", "0::bigint",
			"CREATE VIEW kingres_sketch.v AS SELECT 1", "DROP VIEW kingres_sketch.v", ReasonForeignObject},
		{"the schema is not looked up by name", "WHERE n.nspname = $1::pg_catalog.text", "WHERE n.nspname = $1::pg_catalog.text AND false",
			"ALTER SCHEMA kingres_sketch OWNER TO tenant", "ALTER SCHEMA kingres_sketch OWNER TO kingresd", ReasonSchemaOwner},
	} {
		preInstallSQL = orig
		d.mustSU(t, c.bad)
		if r := check(); r != c.want {
			t.Fatalf("%s: the real statement says %q", c.name, r)
		}
		if !strings.Contains(orig, c.from) {
			t.Fatalf("%s: %q is not in the statement", c.name, c.from)
		}
		preInstallSQL = strings.Replace(orig, c.from, c.to, 1)
		if r := check(); r == c.want {
			t.Errorf("mutant survived: %s", c.name)
		}
		preInstallSQL = orig
		d.mustSU(t, c.undo)
	}
}

// kingres_shard is created with SCHEMA pg_catalog and populates the schema kingres: that schema is the one that is checked, never pg_catalog.
func TestShardIsCheckedInTheSchemaKingres(t *testing.T) {
	d := startTenant(t)
	ctx := context.Background()
	d.install(t, Shard)
	info, err := Verify(ctx, d.su, Shard)
	if err != nil || !info.Present || info.Schema != "kingres" {
		t.Fatalf("a real install of kingres_shard is trusted, in the schema kingres: %+v %v", info, err)
	}
	// the other extensions of the schema kingres are members, not foreign
	d.install(t, Timepart)
	if _, err := Verify(ctx, d.su, Shard); err != nil {
		t.Fatalf("with kingres_timepart in the same schema: %v", err)
	}
	if rs, err := VerifyAll(ctx, d.su, Shard, Timepart); err != nil || rs[0].Err != nil || rs[1].Err != nil {
		t.Fatalf("%+v %v", rs, err)
	}
	for _, c := range []struct {
		bad, undo string
		want      Reason
	}{
		{"CREATE VIEW kingres.ledger_pending AS SELECT 1 AS ledger_txid", "DROP VIEW kingres.ledger_pending", ReasonForeignObject},
		{"CREATE FUNCTION kingres.planted() RETURNS int LANGUAGE sql AS 'SELECT 1'", "DROP FUNCTION kingres.planted()", ReasonForeignObject},
		{"ALTER SCHEMA kingres OWNER TO tenant", "ALTER SCHEMA kingres OWNER TO kingresd", ReasonSchemaOwner},
		{"GRANT CREATE ON SCHEMA kingres TO PUBLIC", "REVOKE CREATE ON SCHEMA kingres FROM PUBLIC", ReasonSchemaCreate},
	} {
		d.mustSU(t, c.bad)
		if _, r := verifyReason(t, d.su, Shard); r != c.want {
			t.Errorf("%s: %q, want %q", c.bad, r, c.want)
		}
		d.mustSU(t, c.undo)
		if _, r := verifyReason(t, d.su, Shard); r != "" {
			t.Fatalf("after %s: %q", c.undo, r)
		}
	}
}

func TestSystemDatabaseProblems(t *testing.T) {
	d := startTenant(t)
	ctx := context.Background()
	// the database that the tenant owns is not a system database
	probs, err := SystemDatabaseProblems(ctx, d.su)
	hasOwner := false
	for _, p := range probs {
		hasOwner = hasOwner || p == SystemProblem{"database", ReasonDatabaseOwner}
	}
	if err != nil || !hasOwner {
		t.Fatalf("a database of the tenant: %v %v", probs, err)
	}
	// the system database as the control plane makes it
	sys := startSystem(t, d)
	if probs, err := SystemDatabaseProblems(ctx, sys); err != nil || len(probs) != 0 {
		t.Fatalf("a clean system database: %v %v", probs, err)
	}
	for _, c := range []struct {
		bad, undo string
		want      SystemProblem
	}{
		{"GRANT CREATE ON DATABASE sysd TO PUBLIC", "REVOKE CREATE ON DATABASE sysd FROM PUBLIC", SystemProblem{"database", ReasonDatabaseCreate}},
		{"GRANT CREATE ON DATABASE sysd TO tenant", "REVOKE CREATE ON DATABASE sysd FROM tenant", SystemProblem{"database", ReasonDatabaseCreate}},
		{"GRANT CREATE ON SCHEMA public TO PUBLIC", "REVOKE CREATE ON SCHEMA public FROM PUBLIC", SystemProblem{"public", ReasonSchemaCreate}},
		{"GRANT CREATE ON SCHEMA kingres_cp TO tenant", "REVOKE CREATE ON SCHEMA kingres_cp FROM tenant", SystemProblem{"kingres_cp", ReasonSchemaCreate}},
		{"ALTER SCHEMA kingres OWNER TO tenant", "ALTER SCHEMA kingres OWNER TO kingresd", SystemProblem{"kingres", ReasonSchemaOwner}},
		{"ALTER SCHEMA kingres_cp OWNER TO tenant", "ALTER SCHEMA kingres_cp OWNER TO kingresd", SystemProblem{"kingres_cp", ReasonSchemaOwner}},
	} {
		sysExec(t, sys, c.bad)
		probs, err := SystemDatabaseProblems(ctx, sys)
		found := false
		for _, p := range probs {
			found = found || p == c.want
		}
		if err != nil || !found {
			t.Errorf("%s: %v %v, want %v", c.bad, probs, err, c.want)
		}
		sysExec(t, sys, c.undo)
		if probs, _ := SystemDatabaseProblems(ctx, sys); len(probs) != 0 {
			t.Fatalf("after %s: %v", c.undo, probs)
		}
	}
}

// startSystem makes a database as the control plane makes its system database (owned by the service role, with the schemas kingres and kingres_cp) and returns a
// connection to it.
func startSystem(t *testing.T, d *tenantDB) *pgx.Conn {
	t.Helper()
	d.mustSU(t, "CREATE DATABASE sysd")
	cfg, err := pgx.ParseConfig(d.in.DSN("sysd"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = "pg_catalog"
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	sysExec(t, c, "CREATE SCHEMA kingres; CREATE SCHEMA kingres_cp")
	return c
}

func sysExec(t *testing.T, c *pgx.Conn, sql string) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// What kingres_shard leaves in the schema kingres while a bucket moves, splits or drains (the orphan of a move and of a split, the old copy of a rebuilt table,
// the log of a table operation, the trigger function of a split) is not a member of the extension and is accepted by name, kind and, for the function, attributes;
// nothing else is.
func shardWithRuntimeObjects(t *testing.T) *tenantDB {
	t.Helper()
	d := startTenant(t)
	d.install(t, Shard)
	d.mustSU(t, "CREATE ROLE tenant2 LOGIN")
	d.mustSU(t, "SELECT kingres.init_cluster('127.0.0.1', "+strconv.Itoa(d.in.Port)+")")
	// a distributed table of the tenant: the extension keeps the objects of its moves under this owner
	if _, err := d.tenant.Exec(context.Background(), "CREATE TABLE public.tenant_t (id bigint PRIMARY KEY, v text)"); err != nil {
		t.Fatal(err)
	}
	d.mustSU(t, "SELECT kingres.create_distributed_table('public.tenant_t', 'id')")
	return d
}

// splitFn is the trigger function of a split as the extension makes it (kingres_shard--0.9.sql, internal_split_*).
func splitFn(name string) string {
	return "CREATE FUNCTION kingres." + name + "() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS 'BEGIN RETURN NULL; END'"
}

func TestRuntimeObjectsOfAMoveAreTrusted(t *testing.T) {
	d := shardWithRuntimeObjects(t)
	ctx := context.Background()
	if _, err := Verify(ctx, d.su, Shard); err != nil {
		t.Fatalf("a distributed table: %v", err)
	}
	for _, s := range []string{
		"CREATE TABLE kingres.orphan_5_3 (a int PRIMARY KEY)", // an orphan of a move, owned by the service role
		"CREATE TABLE kingres.orphan_5_3_l1 (a int)",
		"CREATE TABLE kingres.orphan_s2_1 (a int)",
		"CREATE TABLE kingres.kingres_old_16384 (a int, id int GENERATED ALWAYS AS IDENTITY)",
		"CREATE TABLE kingres.tlog_7_1 (seq int8 GENERATED ALWAYS AS IDENTITY PRIMARY KEY, op \"char\")",
		"CREATE TABLE kingres.orphan_9_1 (a int)", "ALTER TABLE kingres.orphan_9_1 OWNER TO tenant", // owned by the owner of the distributed table
		splitFn("split_capture_3_1"), "ALTER FUNCTION kingres.split_capture_3_1() OWNER TO tenant",
	} {
		d.mustSU(t, s)
	}
	if info, err := Verify(ctx, d.su, Shard); err != nil || !info.Present {
		t.Fatalf("a database in the middle of a move and a split: %v", err)
	}
	if err := PreInstall(ctx, d.su, Timepart, "kingres"); err != nil {
		t.Fatalf("PreInstall over the same schema: %v", err)
	}
	// The tenant drops its last distributed table before the cleanup of the move: the orphan stays, owned by the tenant, and the database is still the
	// extension's (it must not disappear from the listings that a drain relies on).
	if _, err := d.tenant.Exec(ctx, "DROP TABLE public.tenant_t"); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, d.su, Shard); err != nil {
		t.Fatalf("an orphan of a role that owns no distributed table any more: %v", err)
	}
	// and an orphan of a role that never owned one
	d.mustSU(t, "CREATE TABLE kingres.orphan_4_4 (a int); ALTER TABLE kingres.orphan_4_4 OWNER TO tenant2")
	if _, err := Verify(ctx, d.su, Shard); err != nil {
		t.Fatalf("an orphan of any owner: %v", err)
	}
}

func TestRuntimeLookingObjectsThatAreNotTheExtensionsAreRefused(t *testing.T) {
	d := shardWithRuntimeObjects(t)
	for _, c := range []struct{ name, bad, undo string }{
		{"a function with the name of the trigger function of a split but another type", "CREATE FUNCTION kingres.split_capture_3_1() RETURNS int LANGUAGE sql AS 'SELECT 1'", "DROP FUNCTION kingres.split_capture_3_1()"},
		{"a function with another name", splitFn("split_capture_x"), "DROP FUNCTION kingres.split_capture_x()"},
		{"a trigger function that is not SECURITY DEFINER", "CREATE FUNCTION kingres.split_capture_3_1() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS 'BEGIN RETURN NULL; END'", "DROP FUNCTION kingres.split_capture_3_1()"},
		{"a trigger function that the owner made SECURITY INVOKER", splitFn("split_capture_3_1") + "; ALTER FUNCTION kingres.split_capture_3_1() SECURITY INVOKER", "DROP FUNCTION kingres.split_capture_3_1()"},
		{"a trigger function with another search_path", splitFn("split_capture_3_1") + "; ALTER FUNCTION kingres.split_capture_3_1() SET search_path = public", "DROP FUNCTION kingres.split_capture_3_1()"},
		{"a trigger function with no search_path", splitFn("split_capture_3_1") + "; ALTER FUNCTION kingres.split_capture_3_1() RESET ALL", "DROP FUNCTION kingres.split_capture_3_1()"},
		{"a trigger function with a setting more", splitFn("split_capture_3_1") + "; ALTER FUNCTION kingres.split_capture_3_1() SET work_mem = '1MB'", "DROP FUNCTION kingres.split_capture_3_1()"},
		{"a function of a name that the control plane reads", "CREATE FUNCTION kingres.ledger_head() RETURNS int LANGUAGE sql AS 'SELECT 1'", "DROP FUNCTION kingres.ledger_head()"},
		{"a view with the name of an orphan", "CREATE VIEW kingres.orphan_1_1 AS SELECT 1 AS a", "DROP VIEW kingres.orphan_1_1"},
		{"a type with the name of an orphan", "CREATE TYPE kingres.orphan_1_2 AS (a int)", "DROP TYPE kingres.orphan_1_2"},
		{"a table named like a table of the ledger", "CREATE TABLE kingres.ledger_pending (ledger_txid bigint)", "DROP TABLE kingres.ledger_pending"},
		{"a table named like a table of the sharding catalog", "CREATE TABLE kingres.node_x (a int)", "DROP TABLE kingres.node_x"},
		{"a sequence with the name of the log of an operation but no table", "CREATE SEQUENCE kingres.tlog_1_1_seq", "DROP SEQUENCE kingres.tlog_1_1_seq"},
	} {
		d.mustSU(t, c.bad)
		if _, r := verifyReason(t, d.su, Shard); r != ReasonForeignObject {
			t.Errorf("%s: %q", c.name, r)
		}
		d.mustSU(t, c.undo)
		if _, r := verifyReason(t, d.su, Shard); r != "" {
			t.Fatalf("%s: still %q after the undo", c.name, r)
		}
	}
}

// The schema kingres is checked for kingres_shard whichever schema the extension is in (the row of pg_extension is moved here to show it).
func TestShardChecksTheSchemaKingresWhateverTheExtensionSchema(t *testing.T) {
	d := shardWithRuntimeObjects(t)
	ctx := context.Background()
	d.mustSU(t, "CREATE SCHEMA elsewhere")
	d.mustSU(t, "UPDATE pg_catalog.pg_extension SET extnamespace = 'elsewhere'::pg_catalog.regnamespace WHERE extname = 'kingres_shard'")
	info, err := Verify(ctx, d.su, Shard)
	if err != nil || info.Schema != "kingres" {
		t.Fatalf("a clean schema elsewhere and a clean kingres: %+v %v", info, err)
	}
	d.mustSU(t, "CREATE VIEW kingres.ledger_pending AS SELECT 1 AS ledger_txid")
	if _, r := verifyReason(t, d.su, Shard); r != ReasonForeignObject {
		t.Fatalf("a view in kingres: %q", r)
	}
	d.mustSU(t, "DROP VIEW kingres.ledger_pending; ALTER SCHEMA kingres OWNER TO tenant")
	if _, r := verifyReason(t, d.su, Shard); r != ReasonSchemaOwner {
		t.Fatalf("kingres of a tenant: %q", r)
	}
	d.mustSU(t, "ALTER SCHEMA kingres OWNER TO kingresd; GRANT CREATE ON SCHEMA elsewhere TO PUBLIC")
	if _, r := verifyReason(t, d.su, Shard); r != ReasonSchemaCreate {
		t.Fatalf("CREATE on the schema of the extension: %q", r)
	}
}

// Mutants of the rule for the run-time objects: each one lets one of the refused cases through, or refuses the accepted ones.
func TestRuntimeRuleMutantsAreKilled(t *testing.T) {
	d := shardWithRuntimeObjects(t)
	orig := querySQL
	defer func() { querySQL = orig }()
	const tablePattern = "c.relname ~ '^(orphan_s?[0-9]+_[0-9]+|kingres_old_[0-9]+|tlog_[0-9]+_[0-9]+)([A-Za-z0-9_]*)$')"
	invoker := splitFn("split_capture_3_1") + "; ALTER FUNCTION kingres.split_capture_3_1() SECURITY INVOKER"
	other := splitFn("split_capture_3_1") + "; ALTER FUNCTION kingres.split_capture_3_1() SET search_path = public"
	cases := []struct {
		name, from, to, bad, undo string
		want                      Reason // what the real statement says with bad in place ("" for the accepted objects)
	}{
		{"every table is accepted", tablePattern, "true)", "CREATE TABLE kingres.ledger_pending (ledger_txid bigint)", "DROP TABLE kingres.ledger_pending", ReasonForeignObject},
		{"every function is accepted", "p.proname ~ '^split_capture_[0-9]+_[0-9]+$' AND p.prorettype = 'pg_catalog.trigger'::pg_catalog.regtype AND", "true AND", "CREATE FUNCTION kingres.ledger_head() RETURNS int LANGUAGE sql AS 'SELECT 1'", "DROP FUNCTION kingres.ledger_head()", ReasonForeignObject},
		{"the type of the function is not looked at", "AND p.prorettype = 'pg_catalog.trigger'::pg_catalog.regtype", "", "CREATE FUNCTION kingres.split_capture_3_1() RETURNS int LANGUAGE sql AS 'SELECT 1'", "DROP FUNCTION kingres.split_capture_3_1()", ReasonForeignObject},
		{"SECURITY DEFINER is not required", "AND p.prosecdef AND", "AND", invoker, "DROP FUNCTION kingres.split_capture_3_1()", ReasonForeignObject},
		{"the search_path is not required", "AND p.proconfig = ARRAY['search_path=pg_catalog, pg_temp']::pg_catalog.text[]", "", other, "DROP FUNCTION kingres.split_capture_3_1()", ReasonForeignObject},
		{"the schema kingres is not required for the run-time objects", "WHERE c.relnamespace = (SELECT n.oid FROM pg_catalog.pg_namespace n WHERE n.nspname = 'kingres')\n     AND (", "WHERE (", "CREATE SCHEMA elsewhere; UPDATE pg_catalog.pg_extension SET extnamespace = 'elsewhere'::pg_catalog.regnamespace WHERE extname = 'kingres_shard'; CREATE TABLE elsewhere.orphan_5_3 (a int)", "UPDATE pg_catalog.pg_extension SET extnamespace = 'pg_catalog'::pg_catalog.regnamespace WHERE extname = 'kingres_shard'; DROP SCHEMA elsewhere CASCADE", ReasonForeignObject},
		{"the schema kingres is not added for kingres_shard", "    FROM e JOIN pg_catalog.pg_namespace n ON n.nspname = 'kingres'\n   WHERE e.extname = 'kingres_shard'", "    FROM e JOIN pg_catalog.pg_namespace n ON n.nspname = 'kingres'\n   WHERE false", "CREATE SCHEMA elsewhere; UPDATE pg_catalog.pg_extension SET extnamespace = 'elsewhere'::pg_catalog.regnamespace WHERE extname = 'kingres_shard'; CREATE VIEW kingres.ledger_pending AS SELECT 1 AS ledger_txid", "UPDATE pg_catalog.pg_extension SET extnamespace = 'pg_catalog'::pg_catalog.regnamespace WHERE extname = 'kingres_shard'; DROP VIEW kingres.ledger_pending; DROP SCHEMA elsewhere", ReasonForeignObject},
		{"the run-time tables are not accepted at all", "c.relkind IN ('r', 'p') AND c.relname ~", "false AND c.relname ~", "CREATE TABLE kingres.orphan_5_3 (a int)", "DROP TABLE kingres.orphan_5_3", ""},
		{"the trigger functions are not accepted at all", "AND p.proname ~ '^split_capture", "AND false AND p.proname ~ '^split_capture", splitFn("split_capture_3_1"), "DROP FUNCTION kingres.split_capture_3_1()", ""},
	}
	for _, c := range cases {
		querySQL = orig
		d.mustSU(t, c.bad)
		if _, r := verifyReason(t, d.su, Shard); r != c.want {
			t.Fatalf("%s: the real statement says %q, want %q", c.name, r, c.want)
		}
		if !strings.Contains(orig, c.from) {
			t.Fatalf("%s: %q is not in the statement", c.name, c.from)
		}
		querySQL = strings.Replace(orig, c.from, c.to, 1)
		if _, r := verifyReason(t, d.su, Shard); r == c.want {
			t.Errorf("mutant survived: %s", c.name)
		}
		querySQL = orig
		d.mustSU(t, c.undo)
	}
}
