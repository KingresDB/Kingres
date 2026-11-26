#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# Regenerates kingres/verifier/testdata/fixtures from a real KingresDB server
# with the kingres_ledger extension. Run it inside WSL2 (distribution
# Ubuntu-24.04, user kingres):
#
#   wsl.exe -d Ubuntu-24.04 -u kingres -- bash /mnt/c/.../kingres/verifier/scripts/gen-fixtures.sh
#
# It starts a throwaway server in a new directory under
# $KINGRES_TEST_WORK/verifier-test/ (default ~/kw-verifier) on a free port in 57300-57399, builds the
# ledgers, exports the fixtures and removes the server. It only reads the
# installed binaries; it builds and installs nothing.
#
# Environment: KINGRES_BIN (default ~/kingres-work/install/bin), KINGRES_TEST_WORK
# (where the throwaway server lives, default ~/kw-verifier),
# FIXTURE_DIR (default ../testdata/fixtures next to this script).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="${KINGRES_BIN:-$HOME/kingres-work/install/bin}"
OUT="${FIXTURE_DIR:-$SCRIPT_DIR/../testdata/fixtures}"
RUN="run-$(date +%Y%m%d-%H%M%S)-$$"
WORK="${KINGRES_TEST_WORK:-$HOME/kw-verifier}/verifier-test/$RUN"
DATA="$WORK/data"
SOCK="$WORK/sock"

[ -x "$BIN/postgres" ] || { echo "no $BIN/postgres: the server is not built yet" >&2; exit 1; }

PORT=""
for p in $(seq 57300 57399); do
  if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then PORT="$p"; break; fi
done
[ -n "$PORT" ] || { echo "no free port in 57300-57399" >&2; exit 1; }

mkdir -p "$SOCK"
cleanup() {
  "$BIN/pg_ctl" -D "$DATA" -m immediate stop >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

export PATH="$BIN:$PATH"
export PGHOST="$SOCK" PGPORT="$PORT" PGUSER=kingres PGDATABASE=ledgerdb PGCLIENTENCODING=UTF8
# The OS user and the bootstrap superuser are called "kingres", like the
# extension's schema, so the search path is set explicitly and every
# reference to the extension is schema qualified.
export PGOPTIONS="-c search_path=app,pg_catalog"

"$BIN/initdb" -D "$DATA" -U kingres -E UTF8 --locale=C.UTF-8 --auth=trust >/dev/null
"$BIN/pg_ctl" -D "$DATA" -w -l "$WORK/server.log" \
  -o "-p $PORT -c listen_addresses=127.0.0.1 -c unix_socket_directories=$SOCK -c shared_preload_libraries=kingres_ledger -c fsync=off" \
  start >/dev/null

q() { psql -X -q -At -v ON_ERROR_STOP=1 "$@"; }
sql() { q -c "$1"; }

PGDATABASE=postgres sql "CREATE DATABASE ledgerdb"
PGDATABASE=postgres sql "CREATE DATABASE otherdb"

rm -rf "$OUT"
mkdir -p "$OUT/digests" "$OUT/proofs" "$OUT/columns"

# ---- the first ledger ------------------------------------------------------
q <<'SQL'
CREATE EXTENSION kingres_ledger;
CREATE SCHEMA app;
CREATE ROLE "zoé" LOGIN;
GRANT USAGE ON SCHEMA app TO "zoé";

CREATE TABLE app.accounts (
  id int PRIMARY KEY,
  owner text NOT NULL,
  balance numeric(12,2),
  opened timestamptz,
  active boolean,
  token uuid,
  blob bytea,
  big bigint,
  ratio float8,
  note text,
  tags text[],
  born date
);
CREATE TABLE app.events (
  id bigint PRIMARY KEY,
  kind varchar(20) NOT NULL,
  payload jsonb,
  at timestamp,
  small smallint,
  "größe" real,
  span interval,
  addr inet,
  empty text
);
GRANT INSERT, SELECT ON app.accounts, app.events TO "zoé";

CREATE FUNCTION app.fx_cols(rel regclass, row_ctid tid, skip text[] DEFAULT '{}') RETURNS jsonb
LANGUAGE plpgsql AS $f$
DECLARE r record; res jsonb := '[]'; hx text;
BEGIN
  FOR r IN SELECT a.attname::text AS attname, t.typsend::regproc::text AS send
           FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
           WHERE a.attrelid = rel AND a.attnum > 0 AND NOT a.attisdropped
             AND a.attname::text <> ALL (skip)
           ORDER BY a.attnum
  LOOP
    EXECUTE format('SELECT pg_catalog.encode(%s(%I), ''hex'') FROM %s WHERE ctid = $1', r.send, r.attname, rel)
      INTO hx USING row_ctid;
    IF hx IS NOT NULL THEN
      res := res || jsonb_build_array(jsonb_build_object('name', r.attname, 'send_hex', hx));
    END IF;
  END LOOP;
  RETURN res;
END $f$;

SELECT kingres.ledger_enable('app.accounts');
SELECT kingres.ledger_enable('app.events');
SQL

digest() { sql "SELECT kingres.ledger_generate_digest()" > "$OUT/digests/$1.json"; }

# Block 0: three account rows in one transaction, one in another.
sql "INSERT INTO app.accounts(id, owner, balance, opened, active, token, blob, big, ratio, note, tags, born) VALUES
  (1, 'alice', 100.50, '2026-01-02 03:04:05.678901+00', true, 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '\\x00ff10', 9223372036854775807, 3.14159, 'plain', ARRAY['a','b'], '2026-01-02'),
  (2, 'Zoë Müller-Ström', -0.01, '1999-12-31 23:59:59.999999+00', false, NULL, '\\x', -1, 'NaN', 'ünïcode ключ 日本語 😀', ARRAY[]::text[], NULL),
  (3, 'carol', 0, NULL, NULL, NULL, NULL, NULL, NULL, '', NULL, '2000-02-29')"
sql "INSERT INTO app.accounts(id, owner, balance) VALUES (4, 'dave', 12345678.90)"
digest d0
# Proof made at the time of the digest: the block is the digest's block.
sql "SELECT kingres.ledger_inclusion_proof('app.accounts', kingres.ledger_row_hash(a.*)) FROM app.accounts a WHERE id = 1" > "$OUT/proofs/acc1_at_d0.json"

# Block 1: one transaction over two tables, one by another role, one rolled back.
sql "INSERT INTO app.accounts(id, owner, balance, note) VALUES (5, 'erin', 5, 'multi table tx');
     INSERT INTO app.events(id, kind, payload, at, small, \"größe\", span, addr, empty) VALUES
       (1, 'open', '{\"b\": [1, 2, {\"c\": null}], \"a\": \"é\"}', '2026-03-04 05:06:07.89', 32767, 1.5, '1 year 2 mons 3 days 04:05:06.789', '192.168.1.5/32', ''),
       (2, 'close', NULL, NULL, NULL, NULL, '-00:00:01', '::1', NULL)"
sql "BEGIN; SET LOCAL ROLE \"zoé\"; INSERT INTO app.events(id, kind) VALUES (3, 'by zoé'); COMMIT"
sql "BEGIN; INSERT INTO app.events(id, kind) VALUES (99, 'rolled back'); ROLLBACK"
digest d1

# Blocks 2 to 4: a nullable column is added, rows with and without it.
sql "ALTER TABLE app.accounts ADD COLUMN memo text"
sql "INSERT INTO app.accounts(id, owner, memo) VALUES (6, 'frank', 'added column')"
sql "INSERT INTO app.accounts(id, owner) VALUES (7, 'gina')"
digest d2
sql "INSERT INTO app.events(id, kind, payload) VALUES (4, 'later', '[]'), (5, 'later', 'null'), (6, 'later', '\"x\"'), (7, 'later', '1.50'), (8, 'later', '{}')"
digest d3
sql "INSERT INTO app.accounts(id, owner, memo, balance) VALUES (8, 'hank', 'in the last block', 1e9)"
digest d4

# Blocks 5 and 6 are built after d4 so that proofs made now are longer than d4.
sql "INSERT INTO app.events(id, kind) VALUES (9, 'after d4')"
sql "SELECT kingres.ledger_build_block()" >/dev/null
sql "INSERT INTO app.events(id, kind) VALUES (10, 'last')"
digest d5

# ---- an updatable table: updates and deletes, entries I and D ---------------
# The history table keeps every removed version. Blocks 7 to 9: the rows go in;
# one is updated twice and one deleted; one is updated in a later block.
q <<'SQL'
CREATE TABLE app.items (id int PRIMARY KEY, name text NOT NULL, qty int, note text, price numeric(10,2));
SELECT kingres.ledger_enable('app.items', 'updatable');
SQL
sql "INSERT INTO app.items VALUES (1, 'bolt', 10, NULL, 0.25), (2, 'nut', 20, 'ünï', 0.1), (3, 'screw', 30, NULL, NULL), (4, 'washer', 5, 'x', 1)"
digest d6
sql "UPDATE app.items SET qty = qty - 1 WHERE id = 1; DELETE FROM app.items WHERE id = 3"
sql "UPDATE app.items SET qty = qty - 1 WHERE id = 1"
digest d7
sql "UPDATE app.items SET note = 'changed', price = 0.15 WHERE id = 2"
sql "INSERT INTO app.items VALUES (5, 'rivet', 1, NULL, NULL)"
digest d8

# ---- proofs and column descriptions at the end ------------------------------
# label | relation | table id | row hash | columns file | operation
MANIFEST="$WORK/manifest.rows"
: > "$MANIFEST"
export_row() {
  local label="$1" rel="$2" where="$3" cols="$4"
  local tid
  tid=$(sql "SELECT table_id FROM kingres.ledger_table WHERE relid = '$rel'::regclass")
  sql "SELECT kingres.ledger_inclusion_proof('$rel', kingres.ledger_row_hash(r.*)) FROM $rel r WHERE $where" > "$OUT/proofs/$label.json"
  local rh
  rh=$(sql "SELECT encode(kingres.ledger_row_hash(r.*), 'hex') FROM $rel r WHERE $where")
  local colfile=""
  if [ "$cols" = 1 ]; then
    sql "SELECT jsonb_pretty(app.fx_cols('$rel'::regclass, r.ctid)) FROM $rel r WHERE $where" > "$OUT/columns/$label.json"
    colfile="columns/$label.json"
  fi
  printf '%s\t%s\t%s\t%s\t%s\tI\n' "$label" "$rel" "$tid" "$rh" "$colfile" >> "$MANIFEST"
}
# A removed version of a row of an updatable table, from its history table:
# the proof of its D entry (op D) or of the I entry that entered it (op I).
export_old() {
  local label="$1" rel="$2" where="$3" op="$4"
  local hist="${rel}_ledger_history" tid rh
  tid=$(sql "SELECT table_id FROM kingres.ledger_table WHERE relid = '$rel'::regclass")
  rh=$(sql "SELECT encode(kingres.ledger_history_row_hash(h.*), 'hex') FROM $hist h WHERE $where")
  sql "SELECT kingres.ledger_inclusion_proof('$rel', decode('$rh', 'hex'), '$op')" > "$OUT/proofs/$label.json"
  sql "SELECT jsonb_pretty(app.fx_cols('$hist'::regclass, h.ctid, ARRAY['ledger_txid', 'ledger_seq', 'ledger_op'])) FROM $hist h WHERE $where" > "$OUT/columns/$label.json"
  printf '%s\t%s\t%s\t%s\tcolumns/%s.json\t%s\n' "$label" "$rel" "$tid" "$rh" "$label" "$op" >> "$MANIFEST"
}
export_row acc1 app.accounts "id = 1" 1
export_row acc2 app.accounts "id = 2" 1
export_row acc3 app.accounts "id = 3" 1
export_row acc4 app.accounts "id = 4" 1
export_row acc5 app.accounts "id = 5" 1
export_row evt1 app.events "id = 1" 1
export_row evt2 app.events "id = 2" 1
export_row evt3 app.events "id = 3" 1
export_row acc6 app.accounts "id = 6" 1
export_row acc7 app.accounts "id = 7" 1
export_row evt4 app.events "id = 4" 1
export_row evt8 app.events "id = 8" 1
export_row acc8 app.accounts "id = 8" 1
export_row evt9 app.events "id = 9" 1
export_row evt10 app.events "id = 10" 1
# The current rows of the updatable table (I), and its removed versions: each
# removed version has a D proof and an I proof (the entry that entered it).
export_row itm1 app.items "id = 1" 1
export_row itm2 app.items "id = 2" 1
export_row itm4 app.items "id = 4" 1
export_row itm5 app.items "id = 5" 1
# id 1 was (10), (9), is (8): two removals; id 2 was (nut, ünï, 0.1); id 3 was deleted.
export_old itm1_v1_d app.items "id = 1 AND qty = 10" D
export_old itm1_v1_i app.items "id = 1 AND qty = 10" I
export_old itm1_v2_d app.items "id = 1 AND qty = 9" D
export_old itm1_v2_i app.items "id = 1 AND qty = 9" I
export_old itm2_v1_d app.items "id = 2" D
export_old itm2_v1_i app.items "id = 2" I
export_old itm3_deleted_d app.items "id = 3" D
export_old itm3_deleted_i app.items "id = 3" I

# The proof of a row made when its block was the latest.
sql "SELECT kingres.ledger_inclusion_proof('app.accounts', kingres.ledger_row_hash(a.*)) FROM app.accounts a WHERE id = 1" > "$OUT/proofs/acc1_latest.json"

# Block headers: the export query that the README documents.
sql "SELECT json_build_object('height', height, 'hash_version', hash_version,
       'prev_hash', encode(prev_hash, 'hex'), 'tx_merkle_root', encode(tx_merkle_root, 'hex'),
       'tx_count', tx_count, 'closed_at_us', kingres.ledger_timestamptz_us(closed_at),
       'block_hash', encode(block_hash, 'hex'))
     FROM kingres.ledger_blocks ORDER BY height" > "$OUT/blocks.jsonl"

# The server's own answers, for comparison with the verifier.
sql "SELECT kingres.ledger_verify_digest('$(cat "$OUT/digests/d5.json")'::jsonb)" > "$WORK/server_verify_digest.txt"
echo "server ledger_verify_digest(d5): $(cat "$WORK/server_verify_digest.txt")"
sql "SELECT count(*) FROM kingres.ledger_verify()" | sed 's/^/server ledger_verify problems: /'

# ---- a second, separately created database (another chain id) --------------
PGDATABASE=otherdb q <<'SQL'
CREATE EXTENSION kingres_ledger;
CREATE SCHEMA app;
CREATE TABLE app.accounts (id int PRIMARY KEY, owner text NOT NULL);
SELECT kingres.ledger_enable('app.accounts');
INSERT INTO app.accounts VALUES (1, 'alice');
SQL
PGDATABASE=otherdb sql "SELECT kingres.ledger_generate_digest()" > "$OUT/digests/other_chain.json"

# ---- manifest ---------------------------------------------------------------
{
  echo '{"digests": ['
  first=1
  for f in d0 d1 d2 d3 d4 d5 d6 d7 d8; do
    h=$(sql "SELECT ($(printf "'%s'" "$(cat "$OUT/digests/$f.json")")::jsonb->>'height')::int")
    [ $first = 1 ] || echo ','
    first=0
    printf '  {"file": "digests/%s.json", "height": %s}' "$f" "$h"
  done
  echo ''
  echo '],'
  echo '"rows": ['
  first=1
  while IFS=$'\t' read -r label rel tid rh colfile op; do
    [ $first = 1 ] || echo ','
    first=0
    printf '  {"label": "%s", "relation": "%s", "table_id": %s, "row_hash": "%s", "proof": "proofs/%s.json", "columns": "%s", "op": "%s"}' \
      "$label" "$rel" "$tid" "$rh" "$label" "$colfile" "$op"
  done < "$MANIFEST"
  echo ''
  echo '],'
  echo '"blocks": "blocks.jsonl", "other_chain_digest": "digests/other_chain.json"}'
} > "$OUT/manifest.json"

echo "fixtures written to $OUT"
