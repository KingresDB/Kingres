// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// The marker of an import (DX14-01) lives in the schema kingres_transfer of the database of the target.
//
//   - import_marker (nonce, task_id, state, rows, at): one row per import, keyed by a random 128-bit nonce that the task carries (DX15-03), so that a database that was
//     restored or cloned from another cluster cannot hold a row with the key of a new task. A row is deleted after 30 days unless its task is still unfinished (DX16-06).
//   - secret (singleton, k): 32 random bytes that only the service role can read.
//   - mark(nonce, task, rows, tag): the only way for the role of an import to write its row. It runs as the service role and inserts only when tag is
//     sha256(k2 || sha256(k1 || m)) with m = "nonce:task:rows", k1 = sha256(0x31 || secret), k2 = sha256(0x32 || secret): the nested form of an HMAC, which only the control
//     plane can compute (DX15-02, DX16-03). A NULL, a nonce that is not 32 lower-case hex digits, a missing secret or a secret that is not 32 bytes is invalid.
//     The function is not STRICT on purpose: a STRICT function answers NULL to a NULL argument without an error, and the load would go on without a marker.
//   - Both tables have row-level security enabled and no policy (DX16-02): the service role and the function bypass it; the members of pg_read_all_data and of
//     pg_write_all_data, who bypass the privileges of a table but not its row security, see and change no row.
//
// The schema, the tables and the function are created by the service role only, and are checked on every use (and again where the secret is read, DX16-07): a schema that
// exists with another owner, a table with a trigger, a rule, a policy, another column, default, constraint or index, a grant that the control plane did not make, an
// extension that owns one of the objects, or a function with another owner or body makes the import fail closed (DX15-01, DX16-05). The creation takes no lock that a role
// can hold (DX16-04): a first import that races another one retries, and a later one locks the secret table, which only the service role can lock.
const markerTable = "kingres_transfer.import_marker"

const markerRetention = "30 days"

// markFuncBody is the body of kingres_transfer.mark; the check compares it with the catalog.
const markFuncBody = `
DECLARE
  v_k bytea;
BEGIN
  SELECT s.k INTO v_k FROM kingres_transfer.secret s;
  IF p_nonce IS NULL OR p_task IS NULL OR p_rows IS NULL OR p_tag IS NULL OR v_k IS NULL OR pg_catalog.length(v_k) <> 32 OR p_nonce !~ '^[0-9a-f]{32}$' THEN
    RAISE EXCEPTION 'invalid marker';
  END IF;
  IF pg_catalog.sha256(pg_catalog.sha256(pg_catalog.decode('32', 'hex') || v_k) || pg_catalog.sha256(pg_catalog.sha256(pg_catalog.decode('31', 'hex') || v_k) || pg_catalog.convert_to(p_nonce || ':' || p_task::text || ':' || p_rows::text, 'UTF8'))) IS DISTINCT FROM p_tag THEN
    RAISE EXCEPTION 'invalid marker';
  END IF;
  INSERT INTO kingres_transfer.import_marker (nonce, task_id, state, rows) VALUES (p_nonce, p_task, 'committed', p_rows);
END
`

// errMarkerSchema says that the schema of the marker is not the service role's own.
var errMarkerSchema = errors.New("the schema of the marker is not owned by the service role")

// markerStructSQL is true when everything in the schema is what ensureMarker made. $1 is the body of the function. The two temporary tables hold the expected
// definitions, so that the constraints are compared as this server prints them.
const markerStructSQL = `
WITH me AS (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user),
     ns AS (SELECT oid, nspowner, nspacl FROM pg_catalog.pg_namespace WHERE nspname = 'kingres_transfer'),
     tabs AS (SELECT c.oid FROM pg_catalog.pg_class c, ns WHERE c.relnamespace = ns.oid AND c.relkind = 'r')
SELECT (SELECT nspowner FROM ns) = (SELECT oid FROM me)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c, ns WHERE c.relnamespace = ns.oid AND c.relkind NOT IN ('r', 'i', 't') )
  AND (SELECT count(*) FROM pg_catalog.pg_class c, ns WHERE c.relnamespace = ns.oid AND c.relkind = 'r' AND c.relname IN ('import_marker', 'secret')) = 2
  AND (SELECT count(*) FROM tabs) = 2
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c, ns, me WHERE c.relnamespace = ns.oid AND c.relowner <> me.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c, ns WHERE c.relnamespace = ns.oid AND (c.relhastriggers OR c.relhasrules OR c.relispartition OR c.relkind = 'p'))
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid, ns WHERE c.relnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite r JOIN pg_catalog.pg_class c ON c.oid = r.ev_class, ns WHERE c.relnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_policy p JOIN pg_catalog.pg_class c ON c.oid = p.polrelid, ns WHERE c.relnamespace = ns.oid)
  AND (SELECT string_agg(pg_catalog.pg_get_expr(adbin, adrelid), ',' ORDER BY adnum) FROM pg_catalog.pg_attrdef WHERE adrelid = 'kingres_transfer.import_marker'::pg_catalog.regclass) = 'now()'
  AND (SELECT string_agg(pg_catalog.pg_get_expr(adbin, adrelid), ',' ORDER BY adnum) FROM pg_catalog.pg_attrdef WHERE adrelid = 'kingres_transfer.secret'::pg_catalog.regclass) = 'true'
  AND (SELECT coalesce(string_agg(pg_catalog.pg_get_constraintdef(oid), ';' ORDER BY pg_catalog.pg_get_constraintdef(oid)), '') FROM pg_catalog.pg_constraint WHERE conrelid = 'kingres_transfer.import_marker'::pg_catalog.regclass AND contype IN ('c', 'p', 'u', 'f', 'x', 't'))
    = (SELECT coalesce(string_agg(pg_catalog.pg_get_constraintdef(oid), ';' ORDER BY pg_catalog.pg_get_constraintdef(oid)), '') FROM pg_catalog.pg_constraint WHERE conrelid = 'pg_temp.mk_expect_marker'::pg_catalog.regclass AND contype IN ('c', 'p', 'u', 'f', 'x', 't'))
  AND (SELECT coalesce(string_agg(pg_catalog.pg_get_constraintdef(oid), ';' ORDER BY pg_catalog.pg_get_constraintdef(oid)), '') FROM pg_catalog.pg_constraint WHERE conrelid = 'kingres_transfer.secret'::pg_catalog.regclass AND contype IN ('c', 'p', 'u', 'f', 'x', 't'))
    = (SELECT coalesce(string_agg(pg_catalog.pg_get_constraintdef(oid), ';' ORDER BY pg_catalog.pg_get_constraintdef(oid)), '') FROM pg_catalog.pg_constraint WHERE conrelid = 'pg_temp.mk_expect_secret'::pg_catalog.regclass AND contype IN ('c', 'p', 'u', 'f', 'x', 't'))
  AND (SELECT count(*) FROM pg_catalog.pg_index i WHERE i.indrelid IN (SELECT oid FROM tabs)) = 2
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid IN (SELECT oid FROM tabs) AND (NOT i.indisprimary OR i.indexprs IS NOT NULL OR i.indpred IS NOT NULL))
  AND NOT EXISTS (SELECT 1 FROM ns, pg_catalog.aclexplode(ns.nspacl) x WHERE x.grantee <> ns.nspowner AND NOT (x.grantee = 0 AND x.privilege_type = 'USAGE'))
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c, ns, pg_catalog.aclexplode(c.relacl) x WHERE c.relnamespace = ns.oid AND x.grantee <> c.relowner)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_proc p, ns, pg_catalog.aclexplode(p.proacl) x WHERE p.pronamespace = ns.oid AND x.grantee <> p.proowner AND NOT (x.grantee = 0 AND x.privilege_type = 'EXECUTE'))
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_extension e, ns WHERE e.extnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.deptype = 'e' AND ((d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objid IN (SELECT oid FROM tabs))
        OR (d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND d.objid IN (SELECT p.oid FROM pg_catalog.pg_proc p, ns WHERE p.pronamespace = ns.oid))))
  AND (SELECT count(*) FROM pg_catalog.pg_proc p, ns WHERE p.pronamespace = ns.oid) = 1
  AND EXISTS (SELECT 1 FROM pg_catalog.pg_proc p, ns, me WHERE p.pronamespace = ns.oid AND p.proname = 'mark' AND p.proowner = me.oid AND p.prosecdef
              AND p.prosrc = $1 AND p.proconfig = ARRAY['search_path=pg_catalog', 'lock_timeout=10s']::text[] AND p.prolang = (SELECT oid FROM pg_catalog.pg_language WHERE lanname = 'plpgsql'))
  AND (SELECT string_agg(attname || ':' || atttypid::pg_catalog.text || ':' || attnotnull::pg_catalog.text || ':' || attcollation::pg_catalog.text || ':' || attidentity::pg_catalog.text || ':' || attgenerated::pg_catalog.text, ',' ORDER BY attnum) FROM pg_catalog.pg_attribute WHERE attrelid = 'kingres_transfer.import_marker'::pg_catalog.regclass AND attnum > 0 AND NOT attisdropped)
    = (SELECT string_agg(attname || ':' || atttypid::pg_catalog.text || ':' || attnotnull::pg_catalog.text || ':' || attcollation::pg_catalog.text || ':' || attidentity::pg_catalog.text || ':' || attgenerated::pg_catalog.text, ',' ORDER BY attnum) FROM pg_catalog.pg_attribute WHERE attrelid = 'pg_temp.mk_expect_marker'::pg_catalog.regclass AND attnum > 0 AND NOT attisdropped)
  AND (SELECT string_agg(attname || ':' || atttypid::pg_catalog.text || ':' || attnotnull::pg_catalog.text || ':' || attcollation::pg_catalog.text || ':' || attidentity::pg_catalog.text || ':' || attgenerated::pg_catalog.text, ',' ORDER BY attnum) FROM pg_catalog.pg_attribute WHERE attrelid = 'kingres_transfer.secret'::pg_catalog.regclass AND attnum > 0 AND NOT attisdropped)
    = (SELECT string_agg(attname || ':' || atttypid::pg_catalog.text || ':' || attnotnull::pg_catalog.text || ':' || attcollation::pg_catalog.text || ':' || attidentity::pg_catalog.text || ':' || attgenerated::pg_catalog.text, ',' ORDER BY attnum) FROM pg_catalog.pg_attribute WHERE attrelid = 'pg_temp.mk_expect_secret'::pg_catalog.regclass AND attnum > 0 AND NOT attisdropped)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid IN (SELECT oid FROM tabs) AND a.attacl IS NOT NULL)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c WHERE c.oid IN (SELECT oid FROM tabs) AND c.reloptions IS NOT NULL)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid IN (SELECT oid FROM tabs) OR i.inhparent IN (SELECT oid FROM tabs))
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_publication_rel r WHERE r.prrelid IN (SELECT oid FROM tabs))
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_publication_namespace o, ns WHERE o.pnnspid = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_operator o, ns WHERE o.oprnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_collation o, ns WHERE o.collnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_conversion o, ns WHERE o.connamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_statistic_ext o, ns WHERE o.stxnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_opclass o, ns WHERE o.opcnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_opfamily o, ns WHERE o.opfnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_ts_config o, ns WHERE o.cfgnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_ts_dict o, ns WHERE o.dictnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_ts_parser o, ns WHERE o.prsnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_ts_template o, ns WHERE o.tmplnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_type t, ns WHERE t.typnamespace = ns.oid AND t.typtype NOT IN ('c', 'b') AND t.typrelid = 0)
`

// markerFinalSQL is the part of the check that needs the row security and the secret row: it runs after them.
const markerFinalSQL = `
WITH ns AS (SELECT oid FROM pg_catalog.pg_namespace WHERE nspname = 'kingres_transfer')
SELECT (SELECT count(*) FROM pg_catalog.pg_class c, ns WHERE c.relnamespace = ns.oid AND c.relkind = 'r' AND c.relrowsecurity) = 2
  AND (SELECT count(*) FROM kingres_transfer.secret) = 1
  AND (SELECT pg_catalog.length(k) FROM kingres_transfer.secret) = 32
`

// verifyMarkerStruct makes the two temporary tables of the expected definitions and runs the structural check: owner, tables, triggers, rules, policies, columns,
// constraints, indexes, grants, objects of the schema and the function. It reads and runs nothing of the objects. ensureMarker runs it before any write to an object
// that existed before its transaction (DX17-01).
func verifyMarkerStruct(ctx context.Context, tx pgx.Tx) (bool, error) {
	for _, s := range []string{
		"CREATE TEMP TABLE mk_expect_marker (nonce text PRIMARY KEY CHECK (length(nonce) = 32), task_id bigint NOT NULL, state text NOT NULL CHECK (state IN ('committed', 'aborted')), rows bigint NOT NULL, at timestamptz NOT NULL DEFAULT now()) ON COMMIT DROP",
		"CREATE TEMP TABLE mk_expect_secret (singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton), k bytea NOT NULL) ON COMMIT DROP",
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			return false, err
		}
	}
	var ok bool
	if err := tx.QueryRow(ctx, markerStructSQL, markFuncBody).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// verifyMarkerFinal checks the row security and the secret row.
func verifyMarkerFinal(ctx context.Context, tx pgx.Tx) (bool, error) {
	var ok bool
	if err := tx.QueryRow(ctx, markerFinalSQL).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// verifyMarker is the whole check, for a transaction that only reads (the load step).
func verifyMarker(ctx context.Context, tx pgx.Tx) (bool, error) {
	ok, err := verifyMarkerStruct(ctx, tx)
	if err != nil || !ok {
		return false, err
	}
	return verifyMarkerFinal(ctx, tx)
}

// markerRaceCodes are the codes that a creation that raced another one ends with; the whole creation is tried again.
var markerRaceCodes = map[string]bool{"42P06": true, "42P07": true, "42723": true, "42710": true, "23505": true, "40P01": true, "40001": true, "XX000": true}

var errMarkerNotVerified = errors.New("the marker objects are not what the control plane makes")

// ensureMarker makes the schema, the tables and the function with the service role and checks them; it fails closed when anything is not the service role's own.
func (o *Ops) ensureMarker(ctx context.Context, db, _ string) error {
	// Markers of tasks that are still unfinished are never pruned (DX16-06).
	open := []int64{}
	if err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM kingres_cp.task WHERE kind = 'table.import' AND state IN ('pending', 'running', 'paused')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			open = append(open, id)
		}
		return rows.Err()
	}); err != nil {
		return wrap(err)
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = o.ensureMarkerOnce(ctx, db, open)
		var pe *pgconn.PgError
		if err == nil || !errors.As(err, &pe) || !markerRaceCodes[pe.Code] {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(100*(attempt+1)) * time.Millisecond):
		}
	}
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, errMarkerSchema), errors.Is(err, errMarkerNotVerified):
		return transfer.Errorf(transfer.CodeForbidden, false, "the schema kingres_transfer of the database exists and is not what the control plane makes (owner, tables, triggers, constraints, grants or function differ); an operator must remove it before an import is possible")
	case errors.As(err, &pe) && pe.Code == "55P03":
		return transfer.Errorf(transfer.CodeSessionLost, true, "the marker of the database is busy; repeat the operation")
	}
	return wrap(err)
}

func (o *Ops) ensureMarkerOnce(ctx context.Context, db string, open []int64) error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	return o.asService(ctx, db, func(tx pgx.Tx) error {
		for _, s := range []string{"SET LOCAL search_path = pg_catalog", "SET LOCAL lock_timeout = '10s'"} {
			if _, e := tx.Exec(ctx, s); e != nil {
				return e
			}
		}
		var foreign bool
		if e := tx.QueryRow(ctx, `SELECT coalesce((SELECT n.nspowner <> r.oid FROM pg_catalog.pg_namespace n, pg_catalog.pg_roles r WHERE n.nspname = 'kingres_transfer' AND r.rolname = current_user), false)`).Scan(&foreign); e != nil {
			return e
		}
		if foreign {
			return errMarkerSchema
		}
		// A lock that only the service role can take (a role has no privilege on the table); the first creation relies on the retry of ensureMarker.
		var have bool
		if e := tx.QueryRow(ctx, `SELECT pg_catalog.to_regclass('kingres_transfer.secret') IS NOT NULL`).Scan(&have); e != nil {
			return e
		}
		if have {
			if _, e := tx.Exec(ctx, "LOCK TABLE kingres_transfer.secret IN SHARE ROW EXCLUSIVE MODE"); e != nil {
				return e
			}
		}
		// A fault point of the tests, between the owner test and the creation: the window that a database owner could use.
		_ = importFault("beforeMarkerCreate", nil)
		for _, s := range []string{
			"CREATE SCHEMA IF NOT EXISTS kingres_transfer",
			"CREATE TABLE IF NOT EXISTS " + markerTable + " (nonce text PRIMARY KEY CHECK (length(nonce) = 32), task_id bigint NOT NULL, state text NOT NULL CHECK (state IN ('committed', 'aborted')), rows bigint NOT NULL, at timestamptz NOT NULL DEFAULT now())",
			"CREATE TABLE IF NOT EXISTS kingres_transfer.secret (singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton), k bytea NOT NULL)",
		} {
			if _, e := tx.Exec(ctx, s); e != nil {
				return e
			}
		}
		var exists bool
		if e := tx.QueryRow(ctx, `SELECT pg_catalog.to_regprocedure('kingres_transfer.mark(text, bigint, bigint, bytea)') IS NOT NULL`).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			if _, e := tx.Exec(ctx, "CREATE FUNCTION kingres_transfer.mark(p_nonce text, p_task bigint, p_rows bigint, p_tag bytea) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog SET lock_timeout = '10s' AS $f$"+markFuncBody+"$f$"); e != nil {
				return e
			}
		}
		// Verify first: nothing is altered or written on an object that existed before this transaction until the structure is the one the service role made.
		verified, e := verifyMarkerStruct(ctx, tx)
		if e != nil {
			return e
		}
		if !verified {
			return errMarkerNotVerified
		}
		for _, s := range []string{
			"ALTER TABLE " + markerTable + " ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE kingres_transfer.secret ENABLE ROW LEVEL SECURITY",
		} {
			if _, e := tx.Exec(ctx, s); e != nil {
				return e
			}
		}
		// The first run, or a restore that left the table empty, makes the secret; a secret that is there is never replaced.
		if _, e := tx.Exec(ctx, "INSERT INTO kingres_transfer.secret (k) VALUES ($1) ON CONFLICT DO NOTHING", key); e != nil {
			return e
		}
		if final, e := verifyMarkerFinal(ctx, tx); e != nil {
			return e
		} else if !final {
			return errMarkerNotVerified
		}
		// Only after the check, so that nothing is granted or written on an object that the service role did not make.
		for _, s := range []string{
			"REVOKE ALL ON TABLE " + markerTable + " FROM PUBLIC",
			"REVOKE ALL ON TABLE kingres_transfer.secret FROM PUBLIC",
			"GRANT USAGE ON SCHEMA kingres_transfer TO PUBLIC",
			"GRANT EXECUTE ON FUNCTION kingres_transfer.mark(text, bigint, bigint, bytea) TO PUBLIC",
		} {
			if _, e := tx.Exec(ctx, s); e != nil {
				return e
			}
		}
		_, e = tx.Exec(ctx, "DELETE FROM "+markerTable+" WHERE at < now() - interval '"+markerRetention+"' AND task_id <> ALL($1::bigint[])", open)
		return e
	})
}

// markerSecret verifies the objects again (the check at the request is not the state of the load step) and reads the secret with the service role.
func (o *Ops) markerSecret(ctx context.Context, db string) ([]byte, error) {
	var k []byte
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		for _, st := range []string{"SET LOCAL search_path = pg_catalog", "SET LOCAL lock_timeout = '10s'"} {
			if _, e := tx.Exec(ctx, st); e != nil {
				return e
			}
		}
		ok, e := verifyMarker(ctx, tx)
		if e != nil {
			return e
		}
		if !ok {
			return errMarkerNotVerified
		}
		return tx.QueryRow(ctx, "SELECT k FROM kingres_transfer.secret").Scan(&k)
	})
	return k, err
}

// markerTag is the proof that the control plane gives with the marker: sha256(k2 || sha256(k1 || m)), m = "nonce:task:rows", k1 = sha256(0x31 || secret),
// k2 = sha256(0x32 || secret).
func markerTag(secret []byte, nonce string, task, rows int64) []byte {
	derive := func(b byte) []byte {
		h := sha256.New()
		h.Write([]byte{b})
		h.Write(secret)
		return h.Sum(nil)
	}
	inner := sha256.New()
	inner.Write(derive('1'))
	inner.Write([]byte(nonce + ":" + strconv.FormatInt(task, 10) + ":" + strconv.FormatInt(rows, 10)))
	outer := sha256.New()
	outer.Write(derive('2'))
	outer.Write(inner.Sum(nil))
	return outer.Sum(nil)
}

// newMarkerNonce is the random 128-bit key of the marker of one import, as 32 hex digits.
func newMarkerNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// decideOutcome settles whether the transaction of a task committed. It is only called for a task that wrote its intent. committed is true when the marker says
// the rows are in; false means a marker "aborted" now exists and the transaction can never commit. err is set when the question could not be answered (the
// lock on the key was not given in a minute: a transaction in doubt holds it), and the outcome is then unknown.
func (o *Ops) decideOutcome(ctx context.Context, p importParams, taskID int64) (committed bool, rows int64, err error) {
	err = o.asService(ctx, p.Database, func(tx pgx.Tx) error {
		for _, s := range []string{"SET LOCAL search_path = pg_catalog", "SET LOCAL lock_timeout = '60s'"} {
			if _, e := tx.Exec(ctx, s); e != nil {
				return e
			}
		}
		if _, e := tx.Exec(ctx, "INSERT INTO "+markerTable+" (nonce, task_id, state, rows) VALUES ($1, $2, 'aborted', -1) ON CONFLICT (nonce) DO NOTHING", p.MarkerNonce, taskID); e != nil {
			return e
		}
		var state string
		if e := tx.QueryRow(ctx, "SELECT state, rows FROM "+markerTable+" WHERE nonce = $1", p.MarkerNonce).Scan(&state, &rows); e != nil {
			return e
		}
		committed = state == "committed"
		return nil
	})
	return committed, rows, err
}
