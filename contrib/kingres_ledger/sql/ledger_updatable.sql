-- ledger_updatable: updatable ledger tables (mode "updatable"): the history
-- table, capture of UPDATE and DELETE, every statement path that changes a
-- row (ON CONFLICT DO UPDATE, MERGE, foreign-key actions, triggers, COPY,
-- session_replication_role, views, ...), verification, ADD COLUMN, proofs of I
-- and D entries, the protection of the history table, the privileges, and the
-- tamper cases an attacker with catalog access can perform.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
SET search_path = public;
\set VERBOSITY terse

DROP ROLE IF EXISTS kl_u_owner;
DROP ROLE IF EXISTS kl_u_other;
CREATE ROLE kl_u_owner;
CREATE ROLE kl_u_other;
GRANT USAGE ON SCHEMA public TO kl_u_owner, kl_u_other;
GRANT CREATE ON SCHEMA public TO kl_u_owner;
GRANT EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) TO kl_u_owner;

-- Report of ledger_verify with the values that change from run to run masked.
CREATE FUNCTION public.ureport() RETURNS TABLE (check_name text, object text, detail text)
LANGUAGE sql AS $$
    SELECT v.check_name,
           regexp_replace(v.object, 'tx [0-9]+', 'tx N', 'g'),
           regexp_replace(regexp_replace(regexp_replace(v.detail, '[0-9a-f]{64}', '<hash>', 'g'),
                                         'tx [0-9]+', 'tx N', 'g'), 'seq [0-9]+', 'seq N', 'g')
    FROM kingres.ledger_verify() v
    ORDER BY 1, 2, 3
$$;

-- The entries of a table, one line each: the ordinal of the transaction, the
-- position in it, the operation and which version of which row it stands for.
CREATE FUNCTION public.uentries(t regclass, key text) RETURNS TABLE (tx int, seq int, op text, version text)
LANGUAGE plpgsql AS $$
BEGIN
    RETURN QUERY EXECUTE format($q$
        SELECT dense_rank() OVER (ORDER BY e.ledger_txid)::int,
               (row_number() OVER (PARTITION BY e.ledger_txid ORDER BY e.seq) - 1)::int,
               e.op::text,
               COALESCE((SELECT 'cur ' || %1$s FROM %2$s r
                          WHERE kingres.ledger_row_hash(r.*) = e.row_hash LIMIT 1),
                        (SELECT 'old ' || %1$s FROM %2$s_ledger_history r
                          WHERE kingres.ledger_history_row_hash(r.*) = e.row_hash LIMIT 1),
                        '?')
          FROM kingres.ledger_entry e JOIN kingres.ledger_table lt USING (table_id)
         WHERE lt.relid = %3$L::regclass
         ORDER BY e.ledger_txid, e.seq $q$, key, t, t);
END $$;

--------------------------------------------------------------------------
-- Enabling, the history table
--------------------------------------------------------------------------
CREATE TABLE inv (id int PRIMARY KEY, item text NOT NULL, qty int, note text);
INSERT INTO inv VALUES (1, 'bolt', 10, NULL), (2, 'nut', 20, 'n'), (3, 'screw', 30, NULL);
SELECT kingres.ledger_enable('inv', 'updatable');

SELECT t.mode, t.history_relid::regclass AS history FROM kingres.ledger_table t WHERE t.relid = 'inv'::regclass;
-- the columns of the history table
SELECT a.attnum, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull, a.atthasdef, a.attidentity
FROM pg_attribute a WHERE a.attrelid = 'inv_ledger_history'::regclass AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum;
-- no constraints, indexes; two triggers, ENABLE ALWAYS
SELECT (SELECT count(*) FROM pg_constraint WHERE conrelid = 'inv_ledger_history'::regclass) AS constraints,
       (SELECT count(*) FROM pg_index WHERE indrelid = 'inv_ledger_history'::regclass) AS indexes;
SELECT tgrelid::regclass, tgname, tgenabled FROM pg_trigger
WHERE tgrelid IN ('inv'::regclass, 'inv_ledger_history'::regclass) AND NOT tgisinternal ORDER BY 1, 2;
-- the existing rows were entered as I
SELECT * FROM uentries('inv', 'r.id') ORDER BY tx, seq;
SELECT * FROM ureport();

-- Not supported, with clear errors.
SELECT kingres.ledger_enable('inv', 'append_only');
SELECT kingres.ledger_enable('inv', 'updatable');
SELECT kingres.ledger_enable('docs', 'updatable');
CREATE TABLE clash (id int, ledger_seq int);
SELECT kingres.ledger_enable('clash', 'updatable');
CREATE TABLE taken (id int);
CREATE TABLE taken_ledger_history (x int);
SELECT kingres.ledger_enable('taken', 'updatable');
CREATE TABLE a_table_with_a_long_name_that_fills_the_whole_identifier_space_xxx (id int);
SELECT kingres.ledger_enable('a_table_with_a_long_name_that_fills_the_whole_identifier_space_xxx', 'updatable');
SELECT count(*) AS leftovers_of_failed_enables FROM kingres.ledger_table WHERE relid IN ('clash'::regclass, 'taken'::regclass);

--------------------------------------------------------------------------
-- INSERT, UPDATE, DELETE
--------------------------------------------------------------------------
INSERT INTO inv VALUES (4, 'washer', 40, NULL);
UPDATE inv SET qty = 11 WHERE id = 1;
DELETE FROM inv WHERE id = 3;
SELECT * FROM uentries('inv', 'r.id') WHERE tx > 1 ORDER BY tx, seq;
SELECT id, item, qty, ledger_op, ledger_txid IS NOT NULL AS has_tx, ledger_seq IS NOT NULL AS has_seq
FROM inv_ledger_history ORDER BY id;
-- every history row has the hash of its D entry
SELECT count(*) AS history_rows, count(e.*) AS with_entry
FROM inv_ledger_history h
LEFT JOIN kingres.ledger_entry e ON e.ledger_txid = h.ledger_txid AND e.seq = h.ledger_seq
     AND e.op = 'D' AND e.row_hash = kingres.ledger_history_row_hash(h.*);
SELECT count(*) AS problems FROM kingres.ledger_verify();

-- An UPDATE that changes nothing still writes D and I.
UPDATE inv SET qty = qty WHERE id = 2;
SELECT op, version FROM uentries('inv', 'r.id') ORDER BY tx DESC, seq LIMIT 2;
-- Several changes to one row in one transaction.
BEGIN;
UPDATE inv SET qty = 21 WHERE id = 2;
UPDATE inv SET qty = 22, note = 'm' WHERE id = 2;
DELETE FROM inv WHERE id = 2;
INSERT INTO inv VALUES (2, 'nut', 23, 'again');
COMMIT;
SELECT op, version FROM uentries('inv', 'r.id') ORDER BY tx DESC, seq LIMIT 7;
-- Rollback and savepoints leave nothing.
SELECT count(*) AS history_before FROM inv_ledger_history \gset
BEGIN;
UPDATE inv SET qty = 0;
DELETE FROM inv WHERE id = 1;
ROLLBACK;
BEGIN;
UPDATE inv SET qty = 99 WHERE id = 1;
SAVEPOINT s1;
UPDATE inv SET qty = 98 WHERE id = 1;
DELETE FROM inv WHERE id = 4;
ROLLBACK TO s1;
SAVEPOINT s2;
UPDATE inv SET note = 'sp' WHERE id = 1;
RELEASE s2;
COMMIT;
SELECT id, qty, note FROM inv ORDER BY id;
SELECT count(*) - :history_before AS history_rows_added_by_the_savepoint_transaction FROM inv_ledger_history;
SELECT count(*) AS problems FROM kingres.ledger_verify();
-- Rows are locked, not changed, by SELECT ... FOR UPDATE.
SELECT count(*) AS entries_before FROM kingres.ledger_entry \gset
SELECT id FROM inv ORDER BY id FOR UPDATE;
SELECT count(*) - :entries_before AS entries_added_by_locking FROM kingres.ledger_entry;
-- TRUNCATE is rejected.
TRUNCATE inv;
TRUNCATE inv, taken;
SELECT count(*) AS problems FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- Every statement path that changes a row
--------------------------------------------------------------------------
-- INSERT ... ON CONFLICT DO UPDATE / DO NOTHING
INSERT INTO inv VALUES (1, 'bolt', 5, 'conflict') ON CONFLICT (id) DO UPDATE SET qty = inv.qty + EXCLUDED.qty;
INSERT INTO inv VALUES (1, 'bolt', 5, 'nothing') ON CONFLICT (id) DO NOTHING;
INSERT INTO inv VALUES (5, 'rivet', 1, NULL) ON CONFLICT (id) DO UPDATE SET qty = 1000;
SELECT id, qty FROM inv WHERE id IN (1, 5) ORDER BY id;
SELECT count(*) AS problems FROM kingres.ledger_verify();
-- MERGE: update, delete and insert in one statement
MERGE INTO inv t
USING (VALUES (1, 'bolt', 1), (4, 'washer', 0), (6, 'pin', 6), (7, 'cotter', 7)) AS s(id, item, qty)
ON t.id = s.id
WHEN MATCHED AND s.qty = 0 THEN DELETE
WHEN MATCHED THEN UPDATE SET qty = s.qty, note = 'merged'
WHEN NOT MATCHED THEN INSERT (id, item, qty) VALUES (s.id, s.item, s.qty);
SELECT id, item, qty, note FROM inv ORDER BY id;
SELECT count(*) AS problems FROM kingres.ledger_verify();
-- UPDATE ... FROM, DELETE ... USING, RETURNING
UPDATE inv SET qty = s.q FROM (VALUES (6, 66), (7, 77)) s(i, q) WHERE inv.id = s.i RETURNING inv.id, inv.qty;
DELETE FROM inv USING (VALUES (7)) s(i) WHERE inv.id = s.i RETURNING inv.id;
SELECT count(*) AS problems FROM kingres.ledger_verify();
-- Through an updatable view
CREATE VIEW inv_v AS SELECT * FROM inv WHERE id < 100;
UPDATE inv_v SET qty = qty + 1 WHERE id = 6;
DELETE FROM inv_v WHERE id = 6;
INSERT INTO inv_v VALUES (8, 'view', 8, NULL);
SELECT count(*) AS problems FROM kingres.ledger_verify();
-- COPY
COPY inv FROM stdin;
9	copied	9	\N
10	copied	10	x
\.
SELECT count(*) AS problems FROM kingres.ledger_verify();
-- session_replication_role = replica does not skip the triggers
SET session_replication_role = replica;
UPDATE inv SET qty = 91 WHERE id = 9;
DELETE FROM inv WHERE id = 10;
INSERT INTO inv VALUES (11, 'replica', 11, NULL);
RESET session_replication_role;
SELECT count(*) AS problems FROM kingres.ledger_verify();
-- A prepared transaction
BEGIN;
UPDATE inv SET qty = qty + 1 WHERE id = 9;
PREPARE TRANSACTION 'kl_u_prepared';
COMMIT PREPARED 'kl_u_prepared';
SELECT count(*) AS problems FROM kingres.ledger_verify();

-- Foreign keys: ON DELETE CASCADE, ON UPDATE CASCADE and SET NULL actions
-- change rows of an updatable table without a statement that names it.
CREATE TABLE fk_parent (id int PRIMARY KEY);
INSERT INTO fk_parent VALUES (1), (2), (3), (4);
CREATE TABLE fk_child (id int PRIMARY KEY, p_cascade int REFERENCES fk_parent ON DELETE CASCADE ON UPDATE CASCADE,
                       p_null int REFERENCES fk_parent ON DELETE SET NULL ON UPDATE SET NULL, v text);
INSERT INTO fk_child VALUES (1, 1, 1, 'a'), (2, 2, 2, 'b'), (3, 3, 3, 'c'), (4, 4, 4, 'd');
SELECT kingres.ledger_enable('fk_child', 'updatable');
UPDATE fk_parent SET id = 20 WHERE id = 2;
DELETE FROM fk_parent WHERE id = 3;
DELETE FROM fk_parent WHERE id = 4;
SELECT * FROM fk_child ORDER BY id;
SELECT count(*) AS history_rows FROM fk_child_ledger_history;
SELECT count(*) AS problems FROM kingres.ledger_verify();

-- Triggers on the table that change the row before and after the capture
CREATE TABLE trg (id int PRIMARY KEY, val text, note text);
SELECT kingres.ledger_enable('trg', 'updatable');
CREATE FUNCTION trg_before() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.id = 99 THEN RETURN NULL; END IF;
        RETURN OLD;
    END IF;
    NEW.val := NEW.val || '!';
    RETURN NEW;
END $$;
CREATE TRIGGER trg_before BEFORE INSERT OR UPDATE OR DELETE ON trg
    FOR EACH ROW EXECUTE FUNCTION trg_before();
-- An AFTER trigger that sorts before the capture trigger and one that sorts
-- after it; both update the row again.
CREATE FUNCTION trg_after() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.note IS DISTINCT FROM TG_ARGV[0] THEN
        UPDATE trg SET note = TG_ARGV[0] WHERE id = NEW.id AND note IS DISTINCT FROM TG_ARGV[0];
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER a_first AFTER INSERT OR UPDATE ON trg FOR EACH ROW WHEN (NEW.id < 50) EXECUTE FUNCTION trg_after('first');
CREATE TRIGGER z_last AFTER INSERT OR UPDATE ON trg FOR EACH ROW WHEN (NEW.id >= 50) EXECUTE FUNCTION trg_after('last');
INSERT INTO trg VALUES (1, 'one', NULL), (50, 'fifty', NULL), (99, 'ninety-nine', NULL);
UPDATE trg SET val = 'uno' WHERE id = 1;
UPDATE trg SET val = 'cincuenta' WHERE id = 50;
DELETE FROM trg WHERE id = 99;
DELETE FROM trg WHERE id = 1;
SELECT id, val, note FROM trg ORDER BY id;
SELECT count(*) AS problems FROM kingres.ledger_verify();
SELECT op, count(*) FROM uentries('trg', 'r.id') GROUP BY op ORDER BY op;

-- Data-modifying CTEs, and a domain and an enum column
CREATE DOMAIN posint AS int CHECK (VALUE > 0);
CREATE TABLE typ2 (id int PRIMARY KEY, c color, d posint, tags text[]);
INSERT INTO typ2 VALUES (1, 'red', 5, '{a,b}'), (2, 'green', 6, NULL);
SELECT kingres.ledger_enable('typ2', 'updatable');
WITH u AS (UPDATE typ2 SET d = d + 1 WHERE id = 1 RETURNING id)
UPDATE typ2 SET c = 'red' WHERE id = 2 AND EXISTS (SELECT 1 FROM u);
WITH d AS (DELETE FROM typ2 WHERE id = 1 RETURNING *)
INSERT INTO typ2 SELECT id + 10, c, d, tags FROM d;
UPDATE typ2 SET d = 0 WHERE id = 11;
SELECT id, c, d FROM typ2 ORDER BY id;
SELECT count(*) AS problems FROM kingres.ledger_verify();
SELECT count(*) AS history_rows FROM typ2_ledger_history;
-- Many types, values stored out of line, NULLs: the history row hashes like the entry
CREATE TABLE typ (id int PRIMARY KEY, n numeric(12,2), ts timestamptz, d date, b boolean, big text, big2 text,
                  ba bytea, j jsonb, arr int[], u uuid, f float8, iv interval, bits varbit, c char(5), e text);
ALTER TABLE typ ALTER COLUMN big SET STORAGE EXTERNAL;
SELECT kingres.ledger_enable('typ', 'updatable');
INSERT INTO typ
SELECT g, g * 1.25, '2026-11-14 10:20:30.123456+00', '2026-11-14', g % 2 = 0,
       (SELECT string_agg(md5(i::text), '') FROM generate_series(1, 2000) i),
       repeat('compressible ', 5000), '\xdeadbeef', '{"a": [1, 2, {"b": null}]}', ARRAY[1, 2, NULL, 4],
       '123e4567-e89b-12d3-a456-426614174000', CASE g WHEN 1 THEN 'NaN'::float8 ELSE -0.0 END,
       '1 year 2 days 03:04:05', B'1011', 'ab', ''
FROM generate_series(1, 3) g;
UPDATE typ SET n = n + 1, e = NULL WHERE id < 3;
UPDATE typ SET big = big || 'x' WHERE id = 3;
DELETE FROM typ WHERE id = 1;
UPDATE typ SET j = NULL, arr = NULL, ba = '\x', c = NULL WHERE id = 2;
SELECT count(*) AS problems FROM kingres.ledger_verify();
SELECT count(*) AS history_rows, bool_and(kingres.ledger_history_row_hash(h.*) IN
       (SELECT row_hash FROM kingres.ledger_entry WHERE op = 'D')) AS all_hash_to_a_d_entry
FROM typ_ledger_history h;

--------------------------------------------------------------------------
-- ADD COLUMN reaches the history table
--------------------------------------------------------------------------
ALTER TABLE inv ADD COLUMN extra text;
ALTER TABLE inv ADD COLUMN extra2 numeric(5,1), ADD COLUMN extra3 int[];
SELECT a.attnum, a.attname, format_type(a.atttypid, a.atttypmod)
FROM pg_attribute a WHERE a.attrelid = 'inv_ledger_history'::regclass AND a.attnum > 4 AND NOT a.attisdropped
ORDER BY a.attnum;
UPDATE inv SET extra = 'new', extra2 = 1.5, extra3 = '{1}' WHERE id = 1;
DELETE FROM inv WHERE id = 5;
SELECT id, extra, extra2, extra3 FROM inv_ledger_history ORDER BY ledger_txid DESC, ledger_seq LIMIT 3;
SELECT count(*) AS problems FROM kingres.ledger_verify();
-- Rejected: a default, NOT NULL, the history columns' names, a column of an unsupported type
ALTER TABLE inv ADD COLUMN bad int DEFAULT 1;
ALTER TABLE inv ADD COLUMN bad int NOT NULL;
ALTER TABLE inv ADD COLUMN ledger_op text;
ALTER TABLE inv ADD COLUMN ledger_txid bigint;
ALTER TABLE inv ADD COLUMN bad pair;
CREATE TYPE upair AS (x int, y int);
ALTER TABLE inv ADD COLUMN bad upair;
SELECT count(*) AS problems FROM kingres.ledger_verify();
-- A transaction that adds a column and writes in the same transaction
BEGIN;
ALTER TABLE inv ADD COLUMN extra4 text;
UPDATE inv SET extra4 = 'same tx' WHERE id = 1;
DELETE FROM inv WHERE id = 6;
COMMIT;
SELECT count(*) AS problems FROM kingres.ledger_verify();
-- ... and one that rolls back
BEGIN;
ALTER TABLE inv ADD COLUMN extra5 text;
ROLLBACK;
SELECT count(*) AS history_has_extra5 FROM pg_attribute WHERE attrelid = 'inv_ledger_history'::regclass AND attname = 'extra5';
SELECT count(*) AS problems FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- Protection of the history table and of the updatable table
--------------------------------------------------------------------------
\set VERBOSITY sqlstate
-- Rows of the history table cannot be changed, even by a superuser.
UPDATE inv_ledger_history SET qty = 0;
DELETE FROM inv_ledger_history;
TRUNCATE inv_ledger_history;
INSERT INTO inv_ledger_history (id, item) VALUES (1000, 'forged');
-- DDL on it
DROP TABLE inv_ledger_history;
DROP TABLE inv_ledger_history CASCADE;
ALTER TABLE inv_ledger_history ADD COLUMN y int;
ALTER TABLE inv_ledger_history DROP COLUMN qty;
ALTER TABLE inv_ledger_history RENAME COLUMN qty TO q;
ALTER TABLE inv_ledger_history RENAME TO other_name;
ALTER TABLE inv_ledger_history SET SCHEMA pg_catalog;
ALTER TABLE inv_ledger_history OWNER TO kl_u_owner;
ALTER TABLE inv_ledger_history ALTER COLUMN qty TYPE bigint;
ALTER TABLE inv_ledger_history DISABLE TRIGGER ALL;
ALTER TABLE inv_ledger_history DISABLE TRIGGER kingres_ledger_block_modify;
ALTER TABLE inv_ledger_history ENABLE TRIGGER kingres_ledger_block_modify;
ALTER TABLE inv_ledger_history SET UNLOGGED;
ALTER TABLE inv_ledger_history ADD PRIMARY KEY (id, ledger_txid);
ALTER TABLE inv_ledger_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE inv_ledger_history INHERIT inv;
CREATE INDEX ON inv_ledger_history (id);
CREATE TRIGGER x AFTER INSERT ON inv_ledger_history FOR EACH ROW EXECUTE FUNCTION trg_after('x');
DROP TRIGGER kingres_ledger_block_modify ON inv_ledger_history;
CREATE RULE r AS ON INSERT TO inv_ledger_history DO INSTEAD NOTHING;
CREATE POLICY p ON inv_ledger_history USING (true);
CREATE TABLE child_of_history () INHERITS (inv_ledger_history);
CREATE VIEW hist_v AS SELECT * FROM inv_ledger_history;
DROP VIEW hist_v;
-- privileges: only SELECT can be granted
GRANT INSERT ON inv_ledger_history TO kl_u_other;
GRANT ALL ON inv_ledger_history TO kl_u_other;
REVOKE ALL ON inv_ledger_history FROM CURRENT_USER;
-- DROP of what holds it
DROP SCHEMA public CASCADE;
DROP OWNED BY CURRENT_USER;
DROP TABLE inv;
DROP TABLE inv CASCADE;
ALTER TABLE inv DROP COLUMN extra;
ALTER TABLE inv DISABLE TRIGGER kingres_ledger_capture;
ALTER TABLE inv ENABLE TRIGGER kingres_ledger_capture;
ALTER TABLE inv ENABLE REPLICA TRIGGER kingres_ledger_capture;
DROP TRIGGER kingres_ledger_capture ON inv;
ALTER TRIGGER kingres_ledger_capture ON inv RENAME TO other;
CREATE OR REPLACE TRIGGER kingres_ledger_capture AFTER INSERT ON inv FOR EACH ROW EXECUTE FUNCTION trg_after('x');
ALTER TABLE inv RENAME COLUMN qty TO q;
ALTER TABLE inv ALTER COLUMN qty TYPE bigint;
ALTER TABLE inv SET UNLOGGED;
CREATE RULE r AS ON UPDATE TO inv DO INSTEAD NOTHING;
ALTER TABLE inv INHERIT trg;
CREATE TABLE child_of_inv () INHERITS (inv);
-- ENABLE ALWAYS is a trigger state change too
ALTER TABLE inv ENABLE ALWAYS TRIGGER kingres_ledger_capture;
-- Internal functions: no grants, no direct calls
GRANT EXECUTE ON FUNCTION kingres.ledger_history_sync(regclass, regclass) TO kl_u_other;
SET ROLE kl_u_other;
SELECT kingres.ledger_history_sync('inv', 'inv_ledger_history');
RESET ROLE;
\set VERBOSITY terse
SELECT count(*) AS problems FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- Privileges: who can write the table, who can read the history
--------------------------------------------------------------------------
CREATE TABLE priv_t (id int PRIMARY KEY, v text);
ALTER TABLE priv_t OWNER TO kl_u_owner;
SET ROLE kl_u_owner;
SELECT kingres.ledger_enable('priv_t', 'updatable');
INSERT INTO priv_t VALUES (1, 'a'), (2, 'b');
UPDATE priv_t SET v = 'a2' WHERE id = 1;
DELETE FROM priv_t WHERE id = 2;
SELECT id, v, ledger_op FROM priv_t_ledger_history ORDER BY id;
GRANT SELECT, INSERT, UPDATE, DELETE ON priv_t TO kl_u_other;
SELECT count(*) FROM kingres.ledger_entry;
RESET ROLE;
SELECT relacl FROM pg_class WHERE oid = 'priv_t_ledger_history'::regclass;
SELECT pg_get_userbyid(relowner) AS history_owner_is_the_extension_owner_not_the_table_owner
FROM pg_class WHERE oid = 'priv_t_ledger_history'::regclass;
SET ROLE kl_u_other;
UPDATE priv_t SET v = 'by other' WHERE id = 1;
SELECT count(*) FROM priv_t_ledger_history;
RESET ROLE;
SELECT user_name FROM kingres.ledger_pending ORDER BY ledger_txid DESC LIMIT 1;
GRANT SELECT ON priv_t_ledger_history TO kl_u_other;
SET ROLE kl_u_other;
SELECT count(*) AS other_can_read_after_the_grant FROM priv_t_ledger_history;
RESET ROLE;
SELECT count(*) AS problems FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- Blocks, digests, proofs of I and D entries
--------------------------------------------------------------------------
SELECT kingres.ledger_build_block() IS NOT NULL AS block_built;
SELECT kingres.ledger_generate_digest() AS dig \gset
SELECT kingres.ledger_verify_digest(:'dig'::jsonb) AS digest_ok;
-- an I proof and a D proof for the same row version
CREATE TABLE pr (id int PRIMARY KEY, v text);
SELECT kingres.ledger_enable('pr', 'updatable');
INSERT INTO pr VALUES (1, 'first');
UPDATE pr SET v = 'second' WHERE id = 1;
SELECT kingres.ledger_generate_digest() AS dig \gset
SELECT kingres.ledger_history_row_hash(h.*) AS oldhash, t.table_id
FROM pr_ledger_history h, kingres.ledger_table t WHERE t.relid = 'pr'::regclass \gset
SELECT kingres.ledger_inclusion_proof('pr', :'oldhash'::bytea) AS pi \gset
SELECT kingres.ledger_inclusion_proof('pr', :'oldhash'::bytea, 'D') AS pd \gset
SELECT :'pi'::jsonb ->> 'op' AS i_op, :'pd'::jsonb ->> 'op' AS d_op;
SELECT kingres.ledger_verify_proof(:'pi'::jsonb, :'dig'::jsonb) AS i_proof,
       kingres.ledger_verify_proof(:'pd'::jsonb, :'dig'::jsonb) AS d_proof;
-- the four-argument form is for I entries, the five-argument form says which
SELECT kingres.ledger_verify_proof(:'pi'::jsonb, :'dig'::jsonb, :table_id, :'oldhash'::bytea) AS i_with_4_args,
       kingres.ledger_verify_proof(:'pd'::jsonb, :'dig'::jsonb, :table_id, :'oldhash'::bytea) AS d_with_4_args,
       kingres.ledger_verify_proof(:'pd'::jsonb, :'dig'::jsonb, :table_id, :'oldhash'::bytea, 'D') AS d_with_5_args,
       kingres.ledger_verify_proof(:'pi'::jsonb, :'dig'::jsonb, :table_id, :'oldhash'::bytea, 'D') AS i_expected_as_d,
       kingres.ledger_verify_proof(:'pd'::jsonb, :'dig'::jsonb, :table_id, :'oldhash'::bytea, 'I') AS d_expected_as_i;
-- the operation is covered by the hashes
SELECT kingres.ledger_verify_proof(jsonb_set(:'pi'::jsonb, '{op}', '"D"'), :'dig'::jsonb) AS i_altered_to_d,
       kingres.ledger_verify_proof(jsonb_set(:'pd'::jsonb, '{op}', '"I"'), :'dig'::jsonb) AS d_altered_to_i,
       kingres.ledger_verify_proof(jsonb_set(:'pd'::jsonb, '{op}', '"X"'), :'dig'::jsonb) AS d_altered_to_x,
       kingres.ledger_verify_proof(jsonb_set(:'pd'::jsonb, '{op}', '"DD"'), :'dig'::jsonb) AS op_too_long,
       kingres.ledger_verify_proof(jsonb_set(:'pd'::jsonb, '{op}', '7'), :'dig'::jsonb) AS op_not_a_string,
       kingres.ledger_verify_proof(:'pd'::jsonb - 'op', :'dig'::jsonb) AS op_missing,
       kingres.ledger_verify_proof(jsonb_set(:'pd'::jsonb, '{format}', '2'), :'dig'::jsonb) AS format_2,
       kingres.ledger_verify_proof(:'pd'::jsonb - 'format', :'dig'::jsonb) AS format_missing;
-- the current version has an I entry and no D entry
SELECT kingres.ledger_row_hash(p.*) AS curhash FROM pr p \gset
SELECT kingres.ledger_inclusion_proof('pr', :'curhash'::bytea, 'D');
SELECT kingres.ledger_inclusion_proof('pr', :'curhash'::bytea) IS NOT NULL AS current_version_has_i_proof;
SELECT kingres.ledger_inclusion_proof('pr', :'curhash'::bytea, 'X');
SELECT kingres.ledger_inclusion_proof('pr', :'curhash'::bytea, NULL);
-- a D entry for an append-only table does not exist
SELECT kingres.ledger_inclusion_proof('docs', kingres.ledger_row_hash(d.*), 'D') FROM docs d LIMIT 1;

-- The headers in "chain": a proof made after more blocks exist carries them;
-- a digest of an earlier block uses only the headers up to its height, the
-- headers beyond it are ignored, a header before it that is altered is not.
INSERT INTO pr VALUES (50, 'later');
SELECT kingres.ledger_build_block() IS NOT NULL AS block_a;
INSERT INTO pr VALUES (51, 'later');
SELECT kingres.ledger_build_block() IS NOT NULL AS block_b;
INSERT INTO pr VALUES (52, 'later');
SELECT kingres.ledger_generate_digest() AS dig_later \gset
SELECT kingres.ledger_inclusion_proof('pr', :'oldhash'::bytea, 'D') AS pd2 \gset
SELECT jsonb_array_length(:'pd2'::jsonb -> 'chain') AS headers_in_chain,
       (:'dig_later'::jsonb ->> 'height')::int - (:'pd2'::jsonb -> 'block' ->> 'height')::int AS tip_minus_block;
SELECT kingres.ledger_verify_proof(:'pd2'::jsonb, :'dig_later'::jsonb) AS against_the_latest_digest,
       kingres.ledger_verify_proof(:'pd2'::jsonb, :'dig'::jsonb) AS against_the_earlier_digest;
-- a digest of the block after the proof's block stops there: later headers are not used
SELECT jsonb_build_object('chain_id', :'dig'::jsonb -> 'chain_id', 'height', :'pd2'::jsonb -> 'chain' -> 0 -> 'height',
                          'block_hash', :'pd2'::jsonb -> 'chain' -> 0 -> 'block_hash') AS dig_mid \gset
SELECT kingres.ledger_verify_proof(:'pd2'::jsonb, :'dig_mid'::jsonb) AS against_the_digest_in_between;
SELECT kingres.ledger_verify_proof(
           jsonb_set(:'pd2'::jsonb, '{chain,1,prev_hash}', to_jsonb(repeat('00', 32))), :'dig_mid'::jsonb) AS altered_beyond_the_digest,
       kingres.ledger_verify_proof(
           jsonb_set(:'pd2'::jsonb, '{chain,1,prev_hash}', to_jsonb(repeat('00', 32))), :'dig_later'::jsonb) AS altered_within_the_latest;
-- ... but they are parsed: a malformed header beyond the digest makes the proof malformed
SELECT kingres.ledger_verify_proof(jsonb_set(:'pd2'::jsonb, '{chain,2}', '"x"'), :'dig_mid'::jsonb) AS not_an_object_beyond,
       kingres.ledger_verify_proof(jsonb_set(:'pd2'::jsonb, '{chain,2,tx_count}', '1.5'), :'dig_mid'::jsonb) AS fractional_count_beyond,
       kingres.ledger_verify_proof(:'pd2'::jsonb #- '{chain,2,closed_at_us}', :'dig_mid'::jsonb) AS missing_member_beyond;
-- a malformed block_hash in a header is an error; an absent one is allowed
SELECT kingres.ledger_verify_proof(
           jsonb_set(:'pd2'::jsonb, '{chain,0,block_hash}', '"zz"'), :'dig_later'::jsonb) AS malformed_header_hash,
       kingres.ledger_verify_proof(
           jsonb_set(:'pd2'::jsonb, '{chain,0,block_hash}', to_jsonb(repeat('00', 32))), :'dig_later'::jsonb) AS wrong_header_hash,
       kingres.ledger_verify_proof(
           :'pd2'::jsonb #- '{chain,0,block_hash}', :'dig_later'::jsonb) AS absent_header_hash,
       kingres.ledger_verify_proof(
           jsonb_set(:'pd2'::jsonb, '{chain,0,hash_version}', '2'), :'dig_later'::jsonb) AS header_version_2;

--------------------------------------------------------------------------
-- Tamper cases, the way an attacker with catalog access performs them:
-- the triggers are switched off by editing pg_trigger, the session is
-- reconnected, the change is made, and the verification looks at it.  Each
-- case runs in a transaction that is rolled back.
--------------------------------------------------------------------------
SELECT kingres.ledger_build_block() IS NOT NULL AS block_built;
SELECT count(*) AS problems_before FROM kingres.ledger_verify();
\set trig_off 'UPDATE pg_trigger SET tgenabled = ''D'' WHERE tgname LIKE ''kingres_ledger_%'' AND tgrelid IN (SELECT oid FROM pg_class WHERE relname IN (''pr'', ''pr_ledger_history'', ''ledger_entry'', ''ledger_table''))'
\set trig_on 'UPDATE pg_trigger SET tgenabled = ''A'' WHERE tgname LIKE ''kingres_ledger_%'' AND tgrelid IN (SELECT oid FROM pg_class WHERE relname IN (''pr'', ''pr_ledger_history'', ''ledger_entry'', ''ledger_table''))'
INSERT INTO pr VALUES (2, 'two'), (3, 'three');
UPDATE pr SET v = 'two-b' WHERE id = 2;
DELETE FROM pr WHERE id = 3;
SELECT kingres.ledger_build_block() IS NOT NULL AS block_built;
SELECT count(*) AS problems_before FROM kingres.ledger_verify();
:trig_off;
\c
SET search_path = public;
-- 1. a current row changed without entries
BEGIN;
UPDATE pr SET v = 'tampered' WHERE id = 1;
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
-- 2. a current row removed
BEGIN;
DELETE FROM pr WHERE id = 2;
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
-- 3. a history row changed
BEGIN;
UPDATE pr_ledger_history SET v = 'tampered' WHERE id = 3;
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
-- 4. a history row removed
BEGIN;
DELETE FROM pr_ledger_history WHERE id = 3;
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
-- 5. a D entry removed together with its history row (a block that was built
-- has covered it: the transaction root no longer matches)
BEGIN;
DELETE FROM kingres.ledger_entry e USING pr_ledger_history h
 WHERE e.ledger_txid = h.ledger_txid AND e.seq = h.ledger_seq AND h.id = 3;
DELETE FROM pr_ledger_history WHERE id = 3;
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
-- 6. an old version put back as the current row
BEGIN;
INSERT INTO pr SELECT id, v FROM pr_ledger_history WHERE id = 3;
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
-- 7. a history row duplicated
BEGIN;
INSERT INTO pr_ledger_history SELECT * FROM pr_ledger_history WHERE id = 3;
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
-- 8. a history row that points to another entry, and one that claims another operation
BEGIN;
UPDATE pr_ledger_history SET ledger_seq = ledger_seq + 100 WHERE id = 3;
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
BEGIN;
UPDATE pr_ledger_history SET ledger_op = 'I' WHERE id = 3;
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
-- 9. a history row with an invented content (no entry)
BEGIN;
INSERT INTO pr_ledger_history VALUES (77, 'invented', 1, 1, 'D');
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
-- 10. an entry turned from D into I, and from I into D
BEGIN;
UPDATE kingres.ledger_entry e SET op = 'I' FROM pr_ledger_history h
 WHERE e.ledger_txid = h.ledger_txid AND e.seq = h.ledger_seq AND h.id = 3;
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
BEGIN;
UPDATE kingres.ledger_entry e SET op = 'D'
 WHERE e.row_hash = (SELECT kingres.ledger_row_hash(p.*) FROM pr p WHERE id = 1) AND e.op = 'I';
:trig_on;
SELECT * FROM ureport();
ROLLBACK;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
-- 11. a D entry for an append-only table
BEGIN;
UPDATE kingres.ledger_entry e SET op = 'D'
 WHERE e.ledger_txid = (SELECT min(ledger_txid) FROM kingres.ledger_entry WHERE table_id =
                         (SELECT table_id FROM kingres.ledger_table WHERE relid = 'docs'::regclass));
:trig_on;
SELECT check_name, object FROM ureport();
ROLLBACK;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
-- 12. the registry says append-only for a table that has a history table
BEGIN;
UPDATE kingres.ledger_table SET mode = 'append_only', history_relid = NULL WHERE relid = 'pr'::regclass;
:trig_on;
SELECT check_name, object FROM ureport();
ROLLBACK;
\c
SET search_path = public;
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS problems_after FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- The statement-level ways around the capture trigger do not work
--------------------------------------------------------------------------
\set VERBOSITY sqlstate
-- a trigger that is not ENABLE ALWAYS
SELECT count(*) AS plain_triggers FROM pg_trigger t
WHERE t.tgname = 'kingres_ledger_capture' AND t.tgenabled <> 'A';
-- the table cannot lose its history through ALTER TABLE ... ADD COLUMN with a type that cannot be hashed
ALTER TABLE pr ADD COLUMN c pair;
\set VERBOSITY terse
SELECT count(*) AS problems FROM kingres.ledger_verify();
