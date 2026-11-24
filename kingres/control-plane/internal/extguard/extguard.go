// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package extguard decides whether a KingresDB extension in a user database can be trusted before the service role (a superuser) reads or calls anything in
// its schema.
//
// The service role opens sessions in databases whose owners and roles with CREATE on the database are not trusted. A name such as kingres.ledger_pending tells nothing
// about who made the object: a database owner can create the schema kingres, a view in it and a function that the view calls, and a superuser session that reads the
// view then runs that function as a superuser. Testing for the object with to_regclass or to_regprocedure, or for the extension by name only, does not help. Verify
// decides, with one query, that the extension is there and that nothing but the extension and other extensions owned by a superuser can be in its schema:
//
//  1. a row of pg_extension exists and its owner is a superuser;
//  2. the schema of the extension is owned by a superuser;
//  3. the ACL of the schema grants CREATE to no role that is not a superuser (and not to PUBLIC);
//  4. every relation, function, type, operator and other schema object in the schema is a member of an extension (pg_depend, deptype 'e') whose owner is a superuser.
//     The extensions kingres_shard, kingres_ledger, kingres_timepart and kingres_columnar share the schema kingres, so the members of any of them are fine.
//
// The result is Installed, Absent (no row of pg_extension; not an error) or Untrusted (an *UntrustedError). A caller that must have the extension uses Require.
// A caller that can do without it treats an untrusted extension like an absent one and says so (a refusal, or a "not readable" state, and the alert
// extensionSchemaUntrusted that the monitor raises).
//
// The checks are about the catalog at the moment of the query. Nothing a role that is not a superuser can do afterwards changes the result: it owns neither
// the schema nor an object of it, and it has no CREATE on the schema. A Guard caches the answers for the lifetime of one transaction (or one pass of a loop), so
// that a busy loop does not run the query on every statement; make a new Guard per transaction or pass, never keep one for the life of the process.
//
// The session that runs the query must have a fixed search_path (cluster.SafeSession): the query qualifies every name it uses, but an operator or function that
// the caller runs after it is only as safe as the session.
package extguard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// The extensions of KingresDB. Callers name them with these constants.
const (
	Shard    = "kingres_shard"
	Ledger   = "kingres_ledger"
	Timepart = "kingres_timepart"
	Columnar = "kingres_columnar"
	Sketch   = "kingres_sketch"
)

// All lists every extension of KingresDB, in the order the monitor checks them.
var All = []string{Shard, Ledger, Timepart, Columnar, Sketch}

// Querier is what Verify needs: *pgx.Conn, pgx.Tx and *pgxpool.Pool all have it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Reason says which check failed.
type Reason string

const (
	// ReasonExtensionOwner: the owner of the extension is not a superuser.
	ReasonExtensionOwner Reason = "extensionOwner"
	// ReasonSchemaOwner: the owner of the schema of the extension is not a superuser.
	ReasonSchemaOwner Reason = "schemaOwner"
	// ReasonSchemaCreate: a role that is not a superuser, or PUBLIC, has CREATE on the schema.
	ReasonSchemaCreate Reason = "schemaCreate"
	// ReasonForeignObject: the schema holds an object that is not a member of an extension owned by a superuser.
	ReasonForeignObject Reason = "foreignObject"
)

// Info describes an extension that Verify found. Present is false when there is no row of pg_extension (Absent).
type Info struct {
	Name    string
	Present bool
	Version string
	Schema  string
}

// UntrustedError says that the extension exists and its schema cannot be trusted.
type UntrustedError struct {
	Extension string
	Schema    string
	Reason    Reason
	// Detail names the first object that does not belong, for the log and the alert; it is a catalog description of the object.
	Detail string
}

func (e *UntrustedError) Error() string {
	s := fmt.Sprintf("the schema %q of the extension %s cannot be trusted (%s)", e.Schema, e.Extension, e.Reason)
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// Is makes errors.Is(err, ErrUntrusted) true for every *UntrustedError.
func (e *UntrustedError) Is(target error) bool { return target == ErrUntrusted }

var (
	// ErrUntrusted matches every *UntrustedError.
	ErrUntrusted = errors.New("extension schema is not trusted")
	// ErrAbsent is what Require returns when the extension is not installed.
	ErrAbsent = errors.New("extension is not installed")
)

// IsUntrusted reports whether err is an *UntrustedError and returns it.
func IsUntrusted(err error) (*UntrustedError, bool) {
	var u *UntrustedError
	if errors.As(err, &u) {
		return u, true
	}
	return nil, false
}

// verifySQL runs the four checks for the extensions named in $1. One row per extension that has a pg_extension row. A member is an object with an 'e'
// dependency on an extension that a superuser owns; indexes, TOAST tables and the types that belong to a relation or an array are parts of their relation and
// are not looked at on their own.
const verifySQL = verifyHead + verifyTail

// verifyHead selects the extensions named in $1 (the CTE e). preInstallHead selects a schema named in $1 that an extension is about to be created in, as a
// row with no extension (owned by a superuser, so that only the checks of the schema remain). Both go with verifyTail.
const verifyHead = `
WITH e AS (
  SELECT x.oid, x.extname, x.extversion, x.extowner, x.extnamespace
    FROM pg_catalog.pg_extension x
   WHERE x.extname = ANY ($1::pg_catalog.text[])`

const preInstallHead = `
WITH e AS (
  SELECT 0::pg_catalog.oid AS oid, 'install'::pg_catalog.text AS extname, ''::pg_catalog.text AS extversion,
         (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolsuper ORDER BY r.oid LIMIT 1) AS extowner, n.oid AS extnamespace
    FROM pg_catalog.pg_namespace n
   WHERE n.nspname = $1::pg_catalog.text`

const verifyTail = `
), ns AS (
  -- The schemas that an extension populates.  Its own (extnamespace) is one of them, except that pg_catalog is never searched for foreign objects: it holds
  -- the objects of PostgreSQL itself.  kingres_shard is created with SCHEMA pg_catalog and its script makes the schema kingres, so for it the schema kingres is
  -- always one of them, whatever schema it was created in.  prim marks the schema that Info reports.
  SELECT e.extname, n.oid AS nsoid, n.nspname, n.nspowner, n.nspacl, (e.extname <> 'kingres_shard' OR n.nspname = 'kingres') AS prim
    FROM e JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
   WHERE NOT (e.extname = 'kingres_shard' AND e.extnamespace = 'pg_catalog'::pg_catalog.regnamespace)
  UNION
  SELECT e.extname, n.oid, n.nspname, n.nspowner, n.nspacl, true
    FROM e JOIN pg_catalog.pg_namespace n ON n.nspname = 'kingres'
   WHERE e.extname = 'kingres_shard'
), objs AS (
  SELECT ns.extname, 'pg_catalog.pg_class'::pg_catalog.regclass AS classid, c.oid
    FROM ns JOIN pg_catalog.pg_class c ON c.relnamespace = ns.nsoid WHERE c.relkind NOT IN ('i', 'I', 't')
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_proc'::pg_catalog.regclass, p.oid
    FROM ns JOIN pg_catalog.pg_proc p ON p.pronamespace = ns.nsoid
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_type'::pg_catalog.regclass, t.oid
    FROM ns JOIN pg_catalog.pg_type t ON t.typnamespace = ns.nsoid WHERE t.typrelid = 0 AND NOT (t.typelem <> 0 AND t.typcategory = 'A')
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_operator'::pg_catalog.regclass, o.oid
    FROM ns JOIN pg_catalog.pg_operator o ON o.oprnamespace = ns.nsoid
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_opclass'::pg_catalog.regclass, o.oid
    FROM ns JOIN pg_catalog.pg_opclass o ON o.opcnamespace = ns.nsoid
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_opfamily'::pg_catalog.regclass, o.oid
    FROM ns JOIN pg_catalog.pg_opfamily o ON o.opfnamespace = ns.nsoid
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_collation'::pg_catalog.regclass, o.oid
    FROM ns JOIN pg_catalog.pg_collation o ON o.collnamespace = ns.nsoid
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_conversion'::pg_catalog.regclass, o.oid
    FROM ns JOIN pg_catalog.pg_conversion o ON o.connamespace = ns.nsoid
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_statistic_ext'::pg_catalog.regclass, o.oid
    FROM ns JOIN pg_catalog.pg_statistic_ext o ON o.stxnamespace = ns.nsoid
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_ts_config'::pg_catalog.regclass, o.oid
    FROM ns JOIN pg_catalog.pg_ts_config o ON o.cfgnamespace = ns.nsoid
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_ts_dict'::pg_catalog.regclass, o.oid
    FROM ns JOIN pg_catalog.pg_ts_dict o ON o.dictnamespace = ns.nsoid
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_ts_parser'::pg_catalog.regclass, o.oid
    FROM ns JOIN pg_catalog.pg_ts_parser o ON o.prsnamespace = ns.nsoid
  UNION ALL SELECT ns.extname, 'pg_catalog.pg_ts_template'::pg_catalog.regclass, o.oid
    FROM ns JOIN pg_catalog.pg_ts_template o ON o.tmplnamespace = ns.nsoid
), foreign_all AS (
  SELECT o.extname, o.classid, o.oid
    FROM objs o
   WHERE NOT EXISTS (
         SELECT 1
           FROM pg_catalog.pg_depend d
           JOIN pg_catalog.pg_extension m ON m.oid = d.refobjid
           JOIN pg_catalog.pg_roles r ON r.oid = m.extowner
          WHERE d.classid = o.classid AND d.objid = o.oid AND d.objsubid = 0
            AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e' AND r.rolsuper)
     AND NOT EXISTS (
         -- a sequence that belongs to a column of a table that is a member: part of that table
         SELECT 1
           FROM pg_catalog.pg_depend d
           JOIN pg_catalog.pg_depend d2 ON d2.classid = d.refclassid AND d2.objid = d.refobjid
           JOIN pg_catalog.pg_extension m ON m.oid = d2.refobjid
           JOIN pg_catalog.pg_roles r ON r.oid = m.extowner
          WHERE o.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.classid = o.classid AND d.objid = o.oid
            AND d.deptype IN ('a', 'i') AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
            AND d2.deptype = 'e' AND d2.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND r.rolsuper)
), runtime AS (
  -- What kingres_shard makes in the schema kingres while a bucket moves, splits or drains, and what it leaves there until the operation is cleaned up; the
  -- objects are not members of the extension.  They are tables and sequences of fixed name patterns (the orphan of a move or of a split, the old copy of a
  -- table that was rebuilt, the log of a table operation, their partitions and identity sequences) and the trigger functions of a split, which the extension
  -- makes SECURITY DEFINER and gives to the owner of the table.  Nothing else is accepted: no view, no type, no operator, no other function, and no table or
  -- function whose name is one that the control plane or the C library reads.  The owner does not matter: a role that is not a superuser cannot create or
  -- rename anything in the schema (checks 2 and 3), and the owner of a table that moved can drop it, or can be left with the orphan when its last distributed
  -- table is gone.  Only the schema kingres counts, whichever schema the extension is in.
  SELECT f.extname, f.classid, f.oid
    FROM foreign_all f JOIN pg_catalog.pg_class c ON f.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND c.oid = f.oid
   WHERE c.relnamespace = (SELECT n.oid FROM pg_catalog.pg_namespace n WHERE n.nspname = 'kingres')
     AND ((c.relkind IN ('r', 'p') AND c.relname ~ '^(orphan_s?[0-9]+_[0-9]+|kingres_old_[0-9]+|tlog_[0-9]+_[0-9]+)([A-Za-z0-9_]*)$')
      OR (c.relkind = 'S' AND EXISTS (
            SELECT 1 FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_class t ON t.oid = d.refobjid
             WHERE d.classid = f.classid AND d.objid = f.oid AND d.deptype IN ('a', 'i') AND d.refclassid = f.classid
               AND t.relnamespace = c.relnamespace AND t.relkind IN ('r', 'p')
               AND t.relname ~ '^(orphan_s?[0-9]+_[0-9]+|kingres_old_[0-9]+|tlog_[0-9]+_[0-9]+)([A-Za-z0-9_]*)$')))
  UNION ALL
  -- The trigger function of a split is the one that the extension makes: SECURITY DEFINER, language plpgsql, search_path = pg_catalog, pg_temp.  The owner of
  -- the table owns it and could change the attributes of its own function (SECURITY INVOKER, another search_path), so they are part of the test.
  SELECT f.extname, f.classid, f.oid
    FROM foreign_all f JOIN pg_catalog.pg_proc p ON f.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND p.oid = f.oid
   WHERE p.pronamespace = (SELECT n.oid FROM pg_catalog.pg_namespace n WHERE n.nspname = 'kingres')
     AND p.proname ~ '^split_capture_[0-9]+_[0-9]+$' AND p.prorettype = 'pg_catalog.trigger'::pg_catalog.regtype AND p.prokind = 'f'
     AND p.prosecdef AND p.proconfig = ARRAY['search_path=pg_catalog, pg_temp']::pg_catalog.text[]
     AND p.prolang = (SELECT l.oid FROM pg_catalog.pg_language l WHERE l.lanname = 'plpgsql')
), foreign_objs AS (
  SELECT f.extname, f.classid, f.oid FROM foreign_all f
   WHERE NOT EXISTS (SELECT 1 FROM runtime r WHERE r.classid = f.classid AND r.oid = f.oid)
)
SELECT e.extname, e.extversion,
       (pg_catalog.array_agg(ns.nspname ORDER BY ns.prim DESC, ns.nspname))[1],
       COALESCE((SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.oid = e.extowner), false),
       pg_catalog.bool_and(COALESCE((SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.oid = ns.nspowner), false)),
       pg_catalog.bool_or(EXISTS (SELECT 1 FROM pg_catalog.aclexplode(COALESCE(ns.nspacl, pg_catalog.acldefault('n', ns.nspowner))) a
                WHERE a.privilege_type = 'CREATE'
                  AND (a.grantee = 0 OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles g WHERE g.oid = a.grantee AND g.rolsuper)))),
       (SELECT count(*) FROM foreign_objs f WHERE f.extname = e.extname),
       COALESCE((SELECT pg_catalog.pg_describe_object(f.classid, f.oid, 0) FROM foreign_objs f WHERE f.extname = e.extname
                  ORDER BY pg_catalog.pg_describe_object(f.classid, f.oid, 0) LIMIT 1), '')
  FROM e JOIN ns ON ns.extname = e.extname
 GROUP BY e.extname, e.extversion, e.extowner
 ORDER BY e.extname`

// row is one result row of verifySQL.
type row struct {
	Name, Version, Schema string
	ExtOwnerSuper         bool
	SchemaOwnerSuper      bool
	CreateGrant           bool
	Foreign               int64
	FirstForeign          string
}

// classify turns a row into the answer: the first failing check, in the order of the list in the package comment.
func classify(r row) (Info, error) {
	info := Info{Name: r.Name, Present: true, Version: r.Version, Schema: r.Schema}
	bad := func(reason Reason, detail string) (Info, error) {
		return info, &UntrustedError{Extension: r.Name, Schema: r.Schema, Reason: reason, Detail: detail}
	}
	switch {
	case !r.ExtOwnerSuper:
		return bad(ReasonExtensionOwner, "the extension is not owned by a superuser")
	case !r.SchemaOwnerSuper:
		return bad(ReasonSchemaOwner, "the schema is not owned by a superuser")
	case r.CreateGrant:
		return bad(ReasonSchemaCreate, "a role that is not a superuser, or PUBLIC, has CREATE on the schema")
	case r.Foreign > 0:
		return bad(ReasonForeignObject, fmt.Sprintf("%d object(s) of the schema are not members of an extension, first: %s", r.Foreign, r.FirstForeign))
	}
	return info, nil
}

// Result is the answer for one extension: Err is nil or an *UntrustedError.
type Result struct {
	Info Info
	Err  error
}

// VerifyAll checks the named extensions with one query and returns one entry per name, in the order given: Installed (Present, nil error), Absent (not Present,
// nil error) or Untrusted (the *UntrustedError; Present is true). An error of the query itself is returned as the second value.
func VerifyAll(ctx context.Context, q Querier, names ...string) ([]Result, error) {
	rows, err := q.Query(ctx, querySQL, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.Name, &r.Version, &r.Schema, &r.ExtOwnerSuper, &r.SchemaOwnerSuper, &r.CreateGrant, &r.Foreign, &r.FirstForeign); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	found := map[string]Result{}
	for _, r := range all {
		info, cerr := classify(r)
		found[r.Name] = Result{Info: info, Err: cerr}
	}
	out := make([]Result, 0, len(names))
	for _, n := range names {
		if r, ok := found[n]; ok {
			out = append(out, r)
		} else {
			out = append(out, Result{Info: Info{Name: n}})
		}
	}
	return out, nil
}

// Verify checks one extension. It returns the Info of an installed extension, Info{Present: false} when there is no row of pg_extension (not an error), and an
// *UntrustedError when the checks fail (Info then has Present set and the schema). An error of the query itself is returned as it is; callers treat it as
// "could not be read", never as "absent".
func Verify(ctx context.Context, q Querier, ext string) (Info, error) {
	rs, err := VerifyAll(ctx, q, ext)
	if err != nil {
		return Info{Name: ext}, err
	}
	return rs[0].Info, rs[0].Err
}

// Require is Verify for code that must have the extension: an absent one is ErrAbsent (wrapped with the name).
func Require(ctx context.Context, q Querier, ext string) (Info, error) {
	info, err := Verify(ctx, q, ext)
	if err != nil {
		return info, err
	}
	if !info.Present {
		return info, fmt.Errorf("%w: %s", ErrAbsent, ext)
	}
	return info, nil
}

// Guard caches the answers of Verify for the life of one transaction or one pass of a loop. It is safe for concurrent use. The answers Installed, Absent and
// Untrusted are cached; an error of the query is not.
type Guard struct {
	q  Querier
	mu sync.Mutex
	m  map[string]Result
}

// NewGuard returns a Guard that asks q.
func NewGuard(q Querier) *Guard { return &Guard{q: q, m: map[string]Result{}} }

// Verify is the cached extguard.Verify.
func (g *Guard) Verify(ctx context.Context, ext string) (Info, error) {
	g.mu.Lock()
	r, ok := g.m[ext]
	g.mu.Unlock()
	if ok {
		return r.Info, r.Err
	}
	info, err := Verify(ctx, g.q, ext)
	if err != nil {
		if _, untrusted := IsUntrusted(err); !untrusted {
			return info, err
		}
	}
	g.mu.Lock()
	g.m[ext] = Result{Info: info, Err: err}
	g.mu.Unlock()
	return info, err
}

// Require is the cached extguard.Require.
func (g *Guard) Require(ctx context.Context, ext string) (Info, error) {
	info, err := g.Verify(ctx, ext)
	if err != nil {
		return info, err
	}
	if !info.Present {
		return info, fmt.Errorf("%w: %s", ErrAbsent, ext)
	}
	return info, nil
}

// Usable is the question most callers ask: is the extension there and trustworthy? An untrusted extension is (false, the *UntrustedError) so that the caller
// can say why; an absent one is (false, nil).
func (g *Guard) Usable(ctx context.Context, ext string) (bool, error) {
	info, err := g.Verify(ctx, ext)
	return info.Present && err == nil, err
}

// querySQL is the statement that VerifyAll runs; the tests replace it to show that each check is needed (mutants).
var querySQL = verifySQL

// ObjectExtension names the extension that defines an object of the schema kingres, from its name with or without the schema ("kingres.ledger_blocks",
// "timepart_policy"): the ledger objects are ledger_*, the time partitioning ones timepart_* and the two functions create_time_partitions and
// drop_old_time_partitions, the columnar ones columnar_*; everything else is the sharding extension's.
func ObjectExtension(name string) string {
	name = strings.TrimPrefix(name, "kingres.")
	switch {
	case strings.HasPrefix(name, "ledger_"):
		return Ledger
	case strings.HasPrefix(name, "timepart_"), name == "create_time_partitions", name == "drop_old_time_partitions":
		return Timepart
	case strings.HasPrefix(name, "columnar_"):
		return Columnar
	}
	return Shard
}

// preInstallSQL is the statement of PreInstall; the tests replace it (mutants).
var preInstallSQL = preInstallHead + verifyTail

// PreInstall is the check before CREATE EXTENSION or ALTER EXTENSION UPDATE: the schema that the extension is created in must not exist, or must be owned by a
// superuser, grant CREATE to no role that is not a superuser and hold nothing but members of extensions owned by a superuser. A schema that a tenant made in
// advance would otherwise become the schema of the extension (a control file with "schema = ..." uses an existing schema), and the tenant would own what the
// service role runs. The result is an *UntrustedError (with Extension set to ext) or nil; a schema that does not exist is nil.
func PreInstall(ctx context.Context, q Querier, ext, schema string) error {
	rows, err := q.Query(ctx, preInstallSQL, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.Name, &r.Version, &r.Schema, &r.ExtOwnerSuper, &r.SchemaOwnerSuper, &r.CreateGrant, &r.Foreign, &r.FirstForeign); err != nil {
			return err
		}
		r.Name = ext
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, r := range all {
		if _, err := classify(r); err != nil {
			return err
		}
	}
	return nil
}

// ExtensionSchema returns the schema that the control file of the extension names ("schema = ..."), as pg_available_extension_versions shows it, for the
// default version: "" when the extension is not available or the control file names none (the install script then makes its own schema, which PreInstall of
// the name "kingres" covers for kingres_shard).
func ExtensionSchema(ctx context.Context, q Querier, ext string) (string, error) {
	rows, err := q.Query(ctx, `SELECT coalesce(v.schema, '') FROM pg_catalog.pg_available_extension_versions v
		JOIN pg_catalog.pg_available_extensions a ON a.name = v.name AND a.default_version = v.version WHERE v.name = $1`, ext)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var s string
	for rows.Next() {
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
	}
	return s, rows.Err()
}

// PreInstallExtension runs PreInstall for the schema that ext is created in (its control file's schema; the install script of kingres_shard makes "kingres"
// itself, so that is its schema).
func PreInstallExtension(ctx context.Context, q Querier, ext string) error {
	s, err := ExtensionSchema(ctx, q, ext)
	if err != nil {
		return err
	}
	if s == "" && ext == Shard {
		s = "kingres"
	}
	if s == "" {
		return nil
	}
	return PreInstall(ctx, q, ext, s)
}

// IsKingres reports whether name is one of the extensions of KingresDB (All). The other extensions of a database (pgcrypto, hstore, ...) live in schemas that
// the tenant may own and that the control plane never reads, so they are not subject to the checks.
func IsKingres(name string) bool {
	for _, n := range All {
		if n == name {
			return true
		}
	}
	return false
}

// StatusSQL is a statement without parameters for the places that can only send text (a read-only statement run on another server): it returns no row when
// the extension is absent and one row (extversion, reason) otherwise, with reason "" for a trusted extension and the Reason of the first failed check
// for an untrusted one. ext must be one of All.
func StatusSQL(ext string) (string, error) {
	if !IsKingres(ext) {
		return "", fmt.Errorf("extguard: %q is not an extension of KingresDB", ext)
	}
	inner := strings.Replace(verifySQL, "$1::pg_catalog.text[]", "ARRAY['"+ext+"']::pg_catalog.text[]", 1)
	return `SELECT v.extversion, CASE WHEN NOT v.eo THEN '` + string(ReasonExtensionOwner) + `' WHEN NOT v.so THEN '` + string(ReasonSchemaOwner) +
		`' WHEN v.cg THEN '` + string(ReasonSchemaCreate) + `' WHEN v.fc > 0 THEN '` + string(ReasonForeignObject) + `' ELSE '' END
  FROM (` + inner + `) AS v(extname, extversion, nspname, eo, so, cg, fc, ff)`, nil
}
