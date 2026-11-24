// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// This file holds what keeps the importing role, and every role that can act as it, away from the objects that the service role works on (review of 14c: C1, M1,
// M3, L6): the check of the importer, the withdrawal of every right to connect, the rollback of prepared transactions, the lock that a service step takes before it
// trusts a table, and the catalog walk that decides whether a table holds code.

// ------------------------------------------------------------------ who may import

// importerSQL says two things about a role: that it exists, and that a role that can log in is a member of it, directly or through others, with the right to act as
// it (SET or INHERIT: a session of such a member acts with the rights of the importer and is not ended by the withdrawal of the importer's own right to connect).
// The creator of a role is a member with ADMIN OPTION only, with neither SET nor INHERIT, so it adds no path; it can grant itself more, and the check finds it at the
// next step. CREATEROLE is not refused: such a role cannot grant CONNECT on a database that the service role owns, and the withdrawal, the end of every
// session and the lock-check-act transaction of each service step close the other paths (review of 14c).
const importerSQL = `
WITH RECURSIVE me AS (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1::pg_catalog.text::pg_catalog.name),
mem(roleid) AS (
    SELECT a.member FROM pg_catalog.pg_auth_members a WHERE a.roleid = (SELECT oid FROM me) AND (a.set_option OR a.inherit_option)
  UNION
    SELECT a.member FROM pg_catalog.pg_auth_members a JOIN mem ON a.roleid = mem.roleid WHERE a.set_option OR a.inherit_option)
SELECT EXISTS (SELECT 1 FROM me),
       EXISTS (SELECT 1 FROM pg_catalog.pg_roles r WHERE r.oid IN (SELECT roleid FROM mem) AND r.rolcanlogin AND NOT r.rolsuper)`

const importerText = "an import is refused for a role that has a member that can log in and can act as it (the creator of the role, which has only ADMIN OPTION, does not count): a session of such a member is not ended when the role's own right to connect is withdrawn; sign in as another role"

// importerRefusal returns "" when the role may import, or the reason.
func importerRefusal(ctx context.Context, q execer, role string) (string, error) {
	var exists, loginMember bool
	if err := q.QueryRow(ctx, importerSQL, role).Scan(&exists, &loginMember); err != nil {
		return "", err
	}
	switch {
	case !exists:
		return "missing", nil
	case loginMember:
		return "members", nil
	}
	return "", nil
}

// newNonce is the random part of the name of a private database and of its marker: a role with CREATEDB cannot make a database under the name of a task that has
// not started.
func newNonce() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ------------------------------------------------------------------ the withdrawal

// withdrawAttempts bounds the loop that ends the sessions of a database.
const withdrawAttempts = 60

// withdrawHook is called by a test at the start of each round of the loop (the round number): it stands in for a session that was in its start-up when the right to
// connect went and that shows up after a round in which no session was found.
var withdrawHook func(ctx context.Context, round int)

// withdrawLocal takes away the right of every role to connect to the private database of this server, ends every session of it and rolls back the prepared
// transactions that it holds. The order matters: the right goes first (a session that starts afterwards is refused), the sessions are ended next, and the
// database is looked at again until no session of a role that is not a superuser is there (a session that was in its start-up when the right went is found by
// the second look). It fails when sessions remain after the bound.
func (o *Ops) withdrawLocal(ctx context.Context, c *pgx.Conn, name string, db, role sqlx.Ident) error {
	if err := run(ctx, c, stmt().Text("REVOKE CONNECT ON DATABASE").ID(db).Text("FROM PUBLIC")); err != nil {
		return err
	}
	if err := run(ctx, c, stmt().Text("REVOKE CONNECT ON DATABASE").ID(db).Text("FROM").ID(role)); err != nil {
		return err
	}
	// Every other role that holds the right.
	rows, err := c.Query(ctx, `SELECT r.rolname::pg_catalog.text FROM pg_catalog.pg_database d, pg_catalog.aclexplode(d.datacl) a JOIN pg_catalog.pg_roles r ON r.oid = a.grantee
		WHERE d.datname = $1::pg_catalog.text::pg_catalog.name AND a.privilege_type = 'CONNECT' AND NOT r.rolsuper`, name)
	if err != nil {
		return err
	}
	var holders []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		holders = append(holders, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, h := range holders {
		id, err := sqlx.NewIdent(h)
		if err != nil {
			return err
		}
		if err := run(ctx, c, stmt().Text("REVOKE CONNECT ON DATABASE").ID(db).Text("FROM").ID(id)); err != nil {
			return err
		}
	}
	quiet := 0
	for i := 0; i < withdrawAttempts; i++ {
		if h := withdrawHook; h != nil {
			h(ctx, i)
		}
		if _, err := c.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend(pid) FROM pg_catalog.pg_stat_activity WHERE datname = $1::pg_catalog.text AND pid <> pg_catalog.pg_backend_pid()`, name); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		var left int
		if err := c.QueryRow(ctx, `SELECT pg_catalog.count(*)::pg_catalog.int4 FROM pg_catalog.pg_stat_activity a LEFT JOIN pg_catalog.pg_roles r ON r.rolname = a.usename
			WHERE a.datname = $1::pg_catalog.text AND a.pid <> pg_catalog.pg_backend_pid() AND a.backend_type = 'client backend' AND NOT coalesce(r.rolsuper, false)`, name).Scan(&left); err != nil {
			return err
		}
		if left == 0 {
			quiet++
			if quiet >= 2 {
				return o.rollbackPreparedLocal(ctx, c, name)
			}
			continue
		}
		quiet = 0
	}
	return errors.New("sessions of the private database remain after the withdrawal")
}

// rollbackPreparedLocal rolls back every prepared transaction of the database (as the service role, in the database: PostgreSQL finishes a prepared transaction only
// from its own database). A prepared transaction keeps its locks and keeps the database in use, so a rename and a drop would fail for ever.
func (o *Ops) rollbackPreparedLocal(ctx context.Context, c *pgx.Conn, name string) error {
	rows, err := c.Query(ctx, `SELECT gid::pg_catalog.text FROM pg_catalog.pg_prepared_xacts WHERE database = $1::pg_catalog.text`, name)
	if err != nil {
		return err
	}
	var gids []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return err
		}
		gids = append(gids, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(gids) == 0 {
		return nil
	}
	d, err := o.Service.ServiceConn(ctx, name)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close(context.WithoutCancel(ctx)) }()
	for _, g := range gids {
		lit, err := sqlx.QuoteLiteral(g)
		if err != nil {
			return err
		}
		if _, err := d.Exec(ctx, "ROLLBACK PREPARED "+strings.TrimSpace(lit)); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------------ the lock before the service role acts

// lockTrusted runs in a transaction of the service role. It takes ACCESS EXCLUSIVE on the table (a rename, a drop or a swap with a view needs that lock, so none can
// happen while the transaction is open), checks that the name still names the relation that is locked and that it is an ordinary or partitioned table, and then
// asks the catalog whether the table holds code (tableTrusted). The caller acts on the table in the same transaction, so what was checked is what is acted on.
func lockTrusted(ctx context.Context, tx pgx.Tx, schema, table, owner string, extSchema map[string]string) error {
	rel := qualifiedText(schema, table)
	if rel == "" {
		return errors.New("a table name is not valid")
	}
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '60s'"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+rel+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return err
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_locks l ON l.locktype = 'relation' AND l.relation = c.oid AND l.pid = pg_catalog.pg_backend_pid()
			AND l.granted AND l.mode = 'AccessExclusiveLock' AND l.database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())
		WHERE c.oid = pg_catalog.to_regclass($1::pg_catalog.text) AND c.relkind IN ('r', 'p'))`, rel).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s.%s is not the table that was locked", schema, table)
	}
	return tableTrusted(ctx, tx, schema, table, owner, extSchema)
}

// withTrustedTable opens a transaction on a service connection, locks and checks the table, and runs f in it.
func withTrustedTable(ctx context.Context, c *pgx.Conn, schema, table, owner string, extSchema map[string]string, f func(tx pgx.Tx) error) error {
	return inTx(ctx, c, func(tx pgx.Tx) error {
		if err := lockTrusted(ctx, tx, schema, table, owner, extSchema); err != nil {
			return &untrustedErr{err}
		}
		return f(tx)
	})
}

// untrustedErr marks a failure of the check, as opposed to a failure of the call that follows it.
type untrustedErr struct{ err error }

func (e *untrustedErr) Error() string { return e.err.Error() }
func (e *untrustedErr) Unwrap() error { return e.err }

// wrapTrust turns the failure of a locked step into the error of a task: a failed check is table_not_eligible, anything else is what SafeError says.
func wrapTrust(err error, schema, table string) error {
	if err == nil {
		return nil
	}
	var u *untrustedErr
	if errors.As(err, &u) {
		return untrustedError(u.err)
	}
	var te *transfer.Error
	if errors.As(err, &te) {
		return err
	}
	return transfer.SafeError(err).ForTarget(schema, table)
}

// ------------------------------------------------------------------ the catalog walk (M3)

// A table that the importing role made may hold code that runs when the service role reads or changes it: a function, an operator, a cast, an operator class, a
// trigger, a rule, a policy. The code may sit behind a CHECK, a default, an index, a statistics object, a domain of a column, a type, a partition bound. The walk
// starts at the table (and every table under it) and follows pg_depend to everything that is reached; PostgreSQL does not record a dependency on an object that
// it pins (every builtin one), so the builtin functions are looked at as text by tableTrusted.
//
// A reached object that carries code is accepted only when a superuser owns it or it is a member of an extension of the closed list that an expression may use;
// an object that has no owner column (a cast, a rule, a policy, a trigger, a text-search object) is accepted only as a member of such an extension.

// ownedBySuper and memberOfExt are the two tests of the walk, as SQL text for one object.
func ownedBySuper(owner string) string {
	return "EXISTS (SELECT 1 FROM pg_catalog.pg_roles o WHERE o.oid = (" + owner + ") AND o.rolsuper)"
}

const memberOfExt = `EXISTS (SELECT 1 FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_extension e ON e.oid = d.refobjid JOIN pg_catalog.pg_roles er ON er.oid = e.extowner
   WHERE d.classid = r.classid AND d.objid = r.objid AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e' AND er.rolsuper
     AND e.extname = ANY ($2::pg_catalog.text[]))`

// ledgerTrigger accepts the capture trigger that the ledger extension puts on a table: a trigger whose function is a member of the ledger extension, which a superuser owns.
const ledgerTrigger = `(r.classid = 'pg_catalog.pg_trigger'::pg_catalog.regclass AND EXISTS (SELECT 1 FROM pg_catalog.pg_trigger tg JOIN pg_catalog.pg_depend d3 ON d3.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND d3.objid = tg.tgfoid
   AND d3.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d3.deptype = 'e' JOIN pg_catalog.pg_extension e3 ON e3.oid = d3.refobjid
   JOIN pg_catalog.pg_roles er3 ON er3.oid = e3.extowner WHERE tg.oid = r.objid AND e3.extname = 'kingres_ledger' AND er3.rolsuper))`

const reachHead = `
WITH RECURSIVE tree(oid) AS (
    SELECT pg_catalog.to_regclass($1::pg_catalog.text)::pg_catalog.oid
  UNION
    SELECT i.inhrelid FROM pg_catalog.pg_inherits i JOIN tree t ON i.inhparent = t.oid),
seed(classid, objid) AS (
    SELECT 'pg_catalog.pg_class'::pg_catalog.regclass::pg_catalog.oid, oid FROM tree
  UNION ALL SELECT 'pg_catalog.pg_attrdef'::pg_catalog.regclass::pg_catalog.oid, ad.oid FROM pg_catalog.pg_attrdef ad JOIN tree t ON ad.adrelid = t.oid
  UNION ALL SELECT 'pg_catalog.pg_constraint'::pg_catalog.regclass::pg_catalog.oid, c.oid FROM pg_catalog.pg_constraint c JOIN tree t ON c.conrelid = t.oid
  UNION ALL SELECT 'pg_catalog.pg_class'::pg_catalog.regclass::pg_catalog.oid, i.indexrelid FROM pg_catalog.pg_index i JOIN tree t ON i.indrelid = t.oid
  UNION ALL SELECT 'pg_catalog.pg_statistic_ext'::pg_catalog.regclass::pg_catalog.oid, s.oid FROM pg_catalog.pg_statistic_ext s JOIN tree t ON s.stxrelid = t.oid
  UNION ALL SELECT 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid, a.atttypid FROM pg_catalog.pg_attribute a JOIN tree t ON a.attrelid = t.oid WHERE a.attnum > 0 AND NOT a.attisdropped
  UNION ALL SELECT 'pg_catalog.pg_trigger'::pg_catalog.regclass::pg_catalog.oid, g.oid FROM pg_catalog.pg_trigger g JOIN tree t ON g.tgrelid = t.oid WHERE NOT g.tgisinternal
  UNION ALL SELECT 'pg_catalog.pg_policy'::pg_catalog.regclass::pg_catalog.oid, p.oid FROM pg_catalog.pg_policy p JOIN tree t ON p.polrelid = t.oid
  UNION ALL SELECT 'pg_catalog.pg_rewrite'::pg_catalog.regclass::pg_catalog.oid, w.oid FROM pg_catalog.pg_rewrite w JOIN tree t ON w.ev_class = t.oid),
reach(classid, objid) AS (
    SELECT classid, objid FROM seed
  UNION
    SELECT x.classid, x.objid FROM reach r CROSS JOIN LATERAL (
        SELECT d.refclassid, d.refobjid FROM pg_catalog.pg_depend d
         WHERE d.classid = r.classid AND d.objid = r.objid AND d.deptype <> 'e' AND d.refclassid <> 'pg_catalog.pg_extension'::pg_catalog.regclass
        UNION ALL
        SELECT 'pg_catalog.pg_constraint'::pg_catalog.regclass::pg_catalog.oid, c.oid FROM pg_catalog.pg_constraint c
         WHERE r.classid = 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid AND c.contypid = r.objid
        -- A composite type has no dependency record on the class row that holds its attributes: the row is reached here, and its attributes then lead to their types.
        UNION ALL
        SELECT 'pg_catalog.pg_class'::pg_catalog.regclass::pg_catalog.oid, ty.typrelid FROM pg_catalog.pg_type ty
         WHERE r.classid = 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid AND ty.oid = r.objid AND ty.typtype = 'c' AND ty.typrelid <> 0
        -- An array reaches its element; a range reaches its subtype, its operator class and its canonical and difference functions; a multirange reaches its range.
        UNION ALL
        SELECT 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid, ty.typelem FROM pg_catalog.pg_type ty
         WHERE r.classid = 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid AND ty.oid = r.objid AND ty.typelem <> 0
        UNION ALL
        SELECT 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid, g.rngsubtype FROM pg_catalog.pg_range g
         WHERE r.classid = 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid AND g.rngtypid = r.objid
        UNION ALL
        SELECT 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid, g.rngtypid FROM pg_catalog.pg_range g
         WHERE r.classid = 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid AND g.rngmultitypid = r.objid
        UNION ALL
        SELECT 'pg_catalog.pg_opclass'::pg_catalog.regclass::pg_catalog.oid, g.rngsubopc FROM pg_catalog.pg_range g
         WHERE r.classid = 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid AND g.rngtypid = r.objid
        UNION ALL
        SELECT 'pg_catalog.pg_proc'::pg_catalog.regclass::pg_catalog.oid, f.fn FROM pg_catalog.pg_range g CROSS JOIN LATERAL (VALUES (g.rngcanonical::pg_catalog.oid), (g.rngsubdiff::pg_catalog.oid)) f(fn)
         WHERE r.classid = 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid AND g.rngtypid = r.objid AND f.fn <> 0) x(classid, objid))
SELECT pg_catalog.pg_describe_object(r.classid, r.objid, 0)
FROM reach r
WHERE r.objid <> 0 AND (`

func reachSQL() string {
	own := func(class, col, tbl string) string {
		return "(r.classid = '" + class + "'::pg_catalog.regclass AND NOT (" + ownedBySuper("SELECT "+col+" FROM "+tbl+" WHERE oid = r.objid") + " OR " + memberOfExt + "))"
	}
	noOwner := []string{"pg_cast", "pg_rewrite", "pg_policy", "pg_trigger", "pg_event_trigger", "pg_transform", "pg_conversion", "pg_ts_dict", "pg_ts_template", "pg_ts_config",
		"pg_ts_parser", "pg_foreign_data_wrapper", "pg_foreign_server", "pg_amop", "pg_amproc"}
	var no []string
	for _, c := range noOwner {
		no = append(no, "'pg_catalog."+c+"'::pg_catalog.regclass")
	}
	return reachHead + strings.Join([]string{
		own("pg_catalog.pg_proc", "proowner", "pg_catalog.pg_proc"),
		own("pg_catalog.pg_operator", "oprowner", "pg_catalog.pg_operator"),
		own("pg_catalog.pg_opclass", "opcowner", "pg_catalog.pg_opclass"),
		own("pg_catalog.pg_opfamily", "opfowner", "pg_catalog.pg_opfamily"),
		own("pg_catalog.pg_language", "lanowner", "pg_catalog.pg_language"),
		"(r.classid = 'pg_catalog.pg_type'::pg_catalog.regclass AND EXISTS (SELECT 1 FROM pg_catalog.pg_type t WHERE t.oid = r.objid AND t.typtype = 'b' AND t.typcategory <> 'A' AND NOT (" +
			ownedBySuper("t.typowner") + " OR " + memberOfExt + ")))",
		"(r.classid IN (" + strings.Join(no, ", ") + ") AND NOT " + memberOfExt + " AND NOT " + ledgerTrigger + ")",
		"(r.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND EXISTS (SELECT 1 FROM pg_catalog.pg_class c WHERE c.oid = r.objid AND c.relkind IN ('v', 'm', 'f')))",
	}, "\n  OR ") + "\n) LIMIT 1"
}

// reachFinding walks the dependencies of the table and returns the description of the first object that carries code and is not trusted, or "".
func reachFinding(ctx context.Context, tx pgx.Tx, rel string) (string, error) {
	exts := []string{"plpgsql"}
	for e := range exprExtensions {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	var what *string
	err := tx.QueryRow(ctx, reachSQL(), rel, exts).Scan(&what)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if what == nil {
		return "", nil
	}
	return *what, nil
}

// ------------------------------------------------------------------ the tables of a schema, and the ledger

// lockSchemaTables runs in a transaction of the service role, before a call of the extension that reads or moves every table of a schema (distribute_schema): it
// lists every relation of the schema, refuses one that is not a table of the archive's catalog (the SQL of an archive may have made another) or that is not an
// ordinary or partitioned table, locks and checks each of them (lockTrusted), and lists the schema again to see that nothing came in between.
func lockSchemaTables(ctx context.Context, tx pgx.Tx, schema string, known map[string]bool, owner string, extSchema map[string]string) error {
	list := func() ([]string, error) {
		rows, err := tx.Query(ctx, `SELECT c.relname::pg_catalog.text, c.relkind::pg_catalog.text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1::pg_catalog.text AND c.relkind IN ('r', 'p', 'v', 'm', 'f') ORDER BY c.relname::pg_catalog.text`, schema)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n, k string
			if err := rows.Scan(&n, &k); err != nil {
				return nil, err
			}
			if k != "r" && k != "p" {
				return nil, fmt.Errorf("%s.%s is not an ordinary table, and the schema is distributed as it is", schema, n)
			}
			if !known[n] {
				return nil, fmt.Errorf("%s.%s is not a table of the archive's catalog, and the schema is distributed as it is", schema, n)
			}
			names = append(names, n)
		}
		return names, rows.Err()
	}
	names, err := list()
	if err != nil {
		return &untrustedErr{err}
	}
	for _, n := range names {
		if err := lockTrusted(ctx, tx, schema, n, owner, extSchema); err != nil {
			return &untrustedErr{err}
		}
	}
	again, err := list()
	if err != nil {
		return &untrustedErr{err}
	}
	if strings.Join(again, "\x00") != strings.Join(names, "\x00") {
		return &untrustedErr{fmt.Errorf("the tables of schema %s changed while they were locked", schema)}
	}
	return nil
}

// ledgerVerifyLocked runs kingres.ledger_verify() in a transaction that has locked and checked every table that is registered with the ledger (lockTrusted: ACCESS
// EXCLUSIVE, which also holds off a rename, a drop or a swap with a view; the ledger tables of the extension itself are not user tables and are not touched).
// ledger_verify reads the rows of those tables and runs the output functions of their types, so what it reads must be what was checked. It returns the findings.
func ledgerVerifyLocked(ctx context.Context, c *pgx.Conn, owner string, extSchema map[string]string) (int, error) {
	var findings int
	err := inTx(ctx, c, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT n.nspname::pg_catalog.text, c.relname::pg_catalog.text FROM kingres.ledger_table l JOIN pg_catalog.pg_class c ON c.oid = l.relid
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objid = c.oid AND d.deptype = 'e') ORDER BY 1, 2`)
		if err != nil {
			return err
		}
		var rels [][2]string
		for rows.Next() {
			var s, t string
			if err := rows.Scan(&s, &t); err != nil {
				rows.Close()
				return err
			}
			rels = append(rels, [2]string{s, t})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range rels {
			if err := lockTrusted(ctx, tx, r[0], r[1], owner, extSchema); err != nil {
				return &untrustedErr{err}
			}
		}
		return tx.QueryRow(ctx, `SELECT pg_catalog.count(*)::pg_catalog.int4 FROM kingres.ledger_verify()`).Scan(&findings)
	})
	return findings, err
}
