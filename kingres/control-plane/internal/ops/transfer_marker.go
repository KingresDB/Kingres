// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// The marker of an import (DX14-01) lives in the schema kingres_transfer of the database of the target.
//
//   - import_marker (nonce, task_id, state, rows, at): one row per import, keyed by a random 128-bit nonce that the task carries (DX15-03), so that a database that was
//     restored or cloned from another cluster cannot hold a row with the key of a new task. Rows older than 30 days are deleted.
//   - secret (only, k): 32 random bytes that only the service role can read.
//   - mark(nonce, task, rows, tag): the only way for the role of an import to write its row. It runs as the service role and inserts only when tag is the SHA-256
//     of the secret and of "nonce:task:rows", which only the control plane can compute (DX15-02). A role cannot forge, pre-empt or hold the marker of a task.
//
// The schema, the tables and the function are created by the service role only, and are checked on every use: a schema that exists with another owner, a table with a
// trigger, a rule, a policy, another column or another default, or a function with another owner or body makes the import fail closed (DX15-01). The check and
// the creation are made under an advisory lock, so two first imports into a fresh database do not collide.
const markerTable = "kingres_transfer.import_marker"

const markerRetention = "30 days"

// markFuncBody is the body of kingres_transfer.mark; the check compares it with the catalog.
const markFuncBody = `
BEGIN
  IF length(p_nonce) <> 32 OR pg_catalog.sha256((SELECT s.k FROM kingres_transfer.secret s) || pg_catalog.convert_to(p_nonce || ':' || p_task::text || ':' || p_rows::text, 'UTF8')) IS DISTINCT FROM p_tag THEN
    RAISE EXCEPTION 'invalid marker';
  END IF;
  INSERT INTO kingres_transfer.import_marker (nonce, task_id, state, rows) VALUES (p_nonce, p_task, 'committed', p_rows);
END
`

// errMarkerSchema says that the schema of the marker is not the service role's own.
var errMarkerSchema = errors.New("the schema of the marker is not owned by the service role")

// markerVerifySQL is true when everything in the schema is what ensureMarker made.
const markerVerifySQL = `
WITH me AS (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user),
     ns AS (SELECT oid, nspowner FROM pg_catalog.pg_namespace WHERE nspname = 'kingres_transfer')
SELECT (SELECT nspowner FROM ns) = (SELECT oid FROM me)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c, ns WHERE c.relnamespace = ns.oid AND c.relkind NOT IN ('r', 'i', 't') )
  AND (SELECT count(*) FROM pg_catalog.pg_class c, ns WHERE c.relnamespace = ns.oid AND c.relkind = 'r' AND c.relname IN ('import_marker', 'secret')) = 2
  AND (SELECT count(*) FROM pg_catalog.pg_class c, ns WHERE c.relnamespace = ns.oid AND c.relkind = 'r') = 2
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c, ns, me WHERE c.relnamespace = ns.oid AND c.relowner <> me.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c, ns WHERE c.relnamespace = ns.oid AND (c.relrowsecurity OR c.relhastriggers OR c.relhasrules OR c.relispartition OR c.relkind = 'p'))
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid, ns WHERE c.relnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite r JOIN pg_catalog.pg_class c ON c.oid = r.ev_class, ns WHERE c.relnamespace = ns.oid)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_policy p JOIN pg_catalog.pg_class c ON c.oid = p.polrelid, ns WHERE c.relnamespace = ns.oid)
  AND (SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_catalog.pg_attribute WHERE attrelid = 'kingres_transfer.import_marker'::pg_catalog.regclass AND attnum > 0 AND NOT attisdropped) = 'nonce,task_id,state,rows,at'
  AND (SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_catalog.pg_attribute WHERE attrelid = 'kingres_transfer.secret'::pg_catalog.regclass AND attnum > 0 AND NOT attisdropped) = 'singleton,k'
  AND (SELECT string_agg(pg_catalog.pg_get_expr(adbin, adrelid), ',' ORDER BY adnum) FROM pg_catalog.pg_attrdef WHERE adrelid = 'kingres_transfer.import_marker'::pg_catalog.regclass) = 'now()'
  AND (SELECT string_agg(pg_catalog.pg_get_expr(adbin, adrelid), ',' ORDER BY adnum) FROM pg_catalog.pg_attrdef WHERE adrelid = 'kingres_transfer.secret'::pg_catalog.regclass) = 'true'
  AND (SELECT count(*) FROM pg_catalog.pg_proc p, ns WHERE p.pronamespace = ns.oid) = 1
  AND EXISTS (SELECT 1 FROM pg_catalog.pg_proc p, ns, me WHERE p.pronamespace = ns.oid AND p.proname = 'mark' AND p.proowner = me.oid AND p.prosecdef
              AND p.prosrc = $1 AND p.proconfig = ARRAY['search_path=pg_catalog']::text[] AND p.prolang = (SELECT oid FROM pg_catalog.pg_language WHERE lanname = 'plpgsql'))
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_type t, ns WHERE t.typnamespace = ns.oid AND t.typtype NOT IN ('c', 'b') AND t.typrelid = 0)
`

// ensureMarker makes the schema, the tables and the function with the service role and checks them; it fails closed when anything is not the service role's own.
func (o *Ops) ensureMarker(ctx context.Context, db, _ string) error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	var verified bool
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		for _, s := range []string{"SET LOCAL search_path = pg_catalog", "SELECT pg_advisory_xact_lock(7355608)"} {
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
		for _, s := range []string{
			"CREATE SCHEMA IF NOT EXISTS kingres_transfer",
			"CREATE TABLE IF NOT EXISTS " + markerTable + " (nonce text PRIMARY KEY CHECK (length(nonce) = 32), task_id bigint NOT NULL, state text NOT NULL CHECK (state IN ('committed', 'aborted')), rows bigint NOT NULL, at timestamptz NOT NULL DEFAULT now())",
			"CREATE TABLE IF NOT EXISTS kingres_transfer.secret (singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton), k bytea NOT NULL)",
		} {
			if _, e := tx.Exec(ctx, s); e != nil {
				return e
			}
		}
		var have bool
		if e := tx.QueryRow(ctx, `SELECT to_regprocedure('kingres_transfer.mark(text, bigint, bigint, bytea)') IS NOT NULL`).Scan(&have); e != nil {
			return e
		}
		if !have {
			if _, e := tx.Exec(ctx, "CREATE FUNCTION kingres_transfer.mark(p_nonce text, p_task bigint, p_rows bigint, p_tag bytea) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $f$"+markFuncBody+"$f$"); e != nil {
				return e
			}
		}
		if e := tx.QueryRow(ctx, markerVerifySQL, markFuncBody).Scan(&verified); e != nil {
			return e
		}
		if !verified {
			return nil
		}
		// Only after the check, so that nothing is granted or written on an object that the service role did not make.
		for _, s := range []string{
			"REVOKE ALL ON TABLE " + markerTable + " FROM PUBLIC",
			"REVOKE ALL ON TABLE kingres_transfer.secret FROM PUBLIC",
			"GRANT USAGE ON SCHEMA kingres_transfer TO PUBLIC",
			"GRANT EXECUTE ON FUNCTION kingres_transfer.mark(text, bigint, bigint, bytea) TO PUBLIC",
			"DELETE FROM " + markerTable + " WHERE at < now() - interval '" + markerRetention + "'",
		} {
			if _, e := tx.Exec(ctx, s); e != nil {
				return e
			}
		}
		_, e := tx.Exec(ctx, "INSERT INTO kingres_transfer.secret (k) VALUES ($1) ON CONFLICT DO NOTHING", key)
		return e
	})
	if errors.Is(err, errMarkerSchema) || (err == nil && !verified) {
		return transfer.Errorf(transfer.CodeForbidden, false, "the schema kingres_transfer of the database exists and is not what the control plane makes (owner, tables, triggers or function differ); an operator must remove it before an import is possible")
	}
	return wrap(err)
}

// markerSecret reads the secret of the database with the service role.
func (o *Ops) markerSecret(ctx context.Context, db string) ([]byte, error) {
	var k []byte
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT k FROM kingres_transfer.secret").Scan(&k)
	})
	return k, err
}

// markerTag is the proof that the control plane gives with the marker: SHA-256 of the secret and "nonce:task:rows".
func markerTag(secret []byte, nonce string, task, rows int64) []byte {
	h := sha256.New()
	h.Write(secret)
	h.Write([]byte(nonce + ":" + strconv.FormatInt(task, 10) + ":" + strconv.FormatInt(rows, 10)))
	return h.Sum(nil)
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
