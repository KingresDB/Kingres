-- ledger_review: regression tests for the findings L-01 to L-12 of the
-- independent review kingres/docs/reviews/2026-11-14-kingres-ledger-0.1.md.
-- Every block states the finding it reproduces.  The blocks use the state
-- that the earlier tests of this suite left behind (roles kl_owner and
-- kl_other, the public.vreport() helper), so they run last.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
SET search_path = public;
\set VERBOSITY terse

DROP ROLE IF EXISTS kl_auditor;
CREATE ROLE kl_auditor;
GRANT USAGE, CREATE ON SCHEMA public TO kl_owner;

-- Statements that must be rejected: an accepted statement is an explicit
-- failure line, and is undone.
CREATE FUNCTION pg_temp.try(stmts text[]) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    stmt text;
BEGIN
    FOREACH stmt IN ARRAY stmts LOOP
        BEGIN
            EXECUTE stmt;
            RAISE NOTICE 'FAIL (accepted, must be rejected): %', stmt;
            RAISE EXCEPTION 'undo' USING ERRCODE = 'KL999';
        EXCEPTION WHEN SQLSTATE 'KL999' THEN
            NULL;
        WHEN OTHERS THEN
            RAISE NOTICE 'rejected: % -> % %', stmt, SQLSTATE, SQLERRM;
        END;
    END LOOP;
END $$;

--------------------------------------------------------------------------
-- L-01: a column named like the row alias must not break anything
--------------------------------------------------------------------------
CREATE TABLE r_x (x int, y text);
CREATE TABLE r_t (t int, y text);
CREATE TABLE r_names (kl_r int, r text, e int, h text, n int, l text);
CREATE TABLE r_json (x jsonb, t jsonb);
SELECT kingres.ledger_enable('r_x');
SELECT kingres.ledger_enable('r_t');
SELECT kingres.ledger_enable('r_names');
SELECT kingres.ledger_enable('r_json');
INSERT INTO r_x VALUES (1, 'a'), (2, 'b');
INSERT INTO r_t VALUES (1, 'a');
INSERT INTO r_names VALUES (1, 'a', 2, 'c', 4, 'z');
INSERT INTO r_json VALUES ('{"a": 1}', '[2]');
-- Rows that existed before ledger_enable are entered through the same path.
CREATE TABLE r_pre_x (x int, t int);
INSERT INTO r_pre_x VALUES (7, 8);
SELECT kingres.ledger_enable('r_pre_x');
-- The hash of a row does not depend on the way the row is written down.
SELECT kingres.ledger_row_hash(q.*) = kingres.ledger_row_hash(q.*)
       AS whole_row_by_star FROM r_x q LIMIT 1;
SELECT count(*) AS entries_of_r_x FROM kingres.ledger_entry e
JOIN kingres.ledger_table t USING (table_id) WHERE t.relid = 'r_x'::regclass;
SELECT count(*) AS problems FROM kingres.ledger_verify();

-- One broken table is reported as a finding and does not stop the others.
CREATE TABLE r_broken (a int, z text);
SELECT kingres.ledger_enable('r_broken');
INSERT INTO r_broken VALUES (1, 'row');
CREATE TABLE r_other (a int, z text);
SELECT kingres.ledger_enable('r_other');
INSERT INTO r_other VALUES (1, 'row');
UPDATE pg_attribute SET atttypid = 'aclitem'::regtype
WHERE attrelid = 'r_broken'::regclass AND attname = 'z';
SELECT * FROM public.vreport();
UPDATE pg_attribute SET atttypid = 'text'::regtype
WHERE attrelid = 'r_broken'::regclass AND attname = 'z';
SELECT count(*) AS problems_after_repair FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- L-02: no child of any kind, and a bypass is detected
--------------------------------------------------------------------------
CREATE TABLE r_led (id int PRIMARY KEY, v text);
SELECT kingres.ledger_enable('r_led');
INSERT INTO r_led VALUES (1, 'real');
CREATE TABLE r_plain (id int, v text);
CREATE TABLE r_plain2 (id int, v text);
CREATE TABLE r_ptab (id int) PARTITION BY RANGE (id);
CREATE FOREIGN DATA WRAPPER r_fdw;
CREATE SERVER r_srv FOREIGN DATA WRAPPER r_fdw;
SELECT pg_temp.try(ARRAY[
    'CREATE TABLE r_child () INHERITS (r_led)',
    'CREATE FOREIGN TABLE r_forged () INHERITS (r_led) SERVER r_srv',
    'CREATE TABLE r_part PARTITION OF r_led DEFAULT',
    'CREATE FOREIGN TABLE r_fpart PARTITION OF r_led DEFAULT SERVER r_srv',
    'ALTER TABLE r_plain INHERIT r_led',
    'ALTER TABLE r_led INHERIT r_plain',
    'ALTER TABLE r_ptab ATTACH PARTITION r_led DEFAULT',
    'ALTER TABLE r_led ATTACH PARTITION r_plain2 DEFAULT']);
-- LIKE copies the columns only and is fine.
CREATE TABLE r_like (LIKE r_led INCLUDING ALL);
DROP TABLE r_like;
-- A child that got there some other way is reported.
CREATE TABLE r_kid (id int, v text);
INSERT INTO pg_inherits (inhrelid, inhparent, inhseqno, inhdetachpending)
VALUES ('r_kid'::regclass, 'r_led'::regclass, 1, false);
UPDATE pg_class SET relhassubclass = true WHERE oid = 'r_led'::regclass;
SELECT * FROM public.vreport();
DELETE FROM pg_inherits WHERE inhrelid = 'r_kid'::regclass;
UPDATE pg_class SET relhassubclass = false WHERE oid = 'r_led'::regclass;
DROP TABLE r_kid;
SELECT count(*) AS problems_after_cleanup FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- L-04: ledger_enable is not executable by PUBLIC
--------------------------------------------------------------------------
SET ROLE kl_owner;
CREATE TABLE r_mine (id int PRIMARY KEY, note text);
SELECT kingres.ledger_enable('r_mine');
RESET ROLE;
SELECT count(*) AS r_mine_registered FROM kingres.ledger_table
WHERE relid = 'r_mine'::regclass;
-- An administrator grants it; the role still needs to own the table.
GRANT EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) TO kl_owner, kl_other;
SET ROLE kl_other;
SELECT kingres.ledger_enable('r_mine');
RESET ROLE;
SET ROLE kl_owner;
SELECT kingres.ledger_enable('r_mine');
INSERT INTO r_mine VALUES (1, 'x');
RESET ROLE;
REVOKE EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) FROM kl_owner, kl_other;

--------------------------------------------------------------------------
-- L-09 and L-08: who can read what, and who can scan the chain
--------------------------------------------------------------------------
SET ROLE kl_other;
SELECT count(*) AS entry_rows_visible FROM kingres.ledger_entry;
SELECT count(*) AS pending_rows_visible FROM kingres.ledger_pending;
SELECT count(*) AS tx_rows_visible FROM kingres.ledger_tx;
SELECT count(*) > 0 AS blocks_readable FROM kingres.ledger_blocks;
SELECT count(*) > 0 AS tables_readable FROM kingres.ledger_table;
SELECT kingres.ledger_verify_digest('{}');
SELECT * FROM kingres.ledger_verify();
RESET ROLE;
-- The documented grants of an auditor role.
GRANT SELECT ON kingres.ledger_entry, kingres.ledger_pending, kingres.ledger_tx
    TO kl_auditor;
GRANT EXECUTE ON FUNCTION kingres.ledger_verify(bigint, bigint),
    kingres.ledger_verify_digest(jsonb),
    kingres.ledger_inclusion_proof(regclass, bytea) TO kl_auditor;
SET ROLE kl_auditor;
SELECT count(*) > 0 AS auditor_reads_entries FROM kingres.ledger_entry;
SELECT count(*) AS auditor_problems FROM kingres.ledger_verify();
SELECT kingres.ledger_verify_digest('{}') AS auditor_verify_digest;
SELECT kingres.ledger_build_block();
RESET ROLE;
REVOKE ALL ON kingres.ledger_entry, kingres.ledger_pending, kingres.ledger_tx
    FROM kl_auditor;
REVOKE EXECUTE ON FUNCTION kingres.ledger_verify(bigint, bigint),
    kingres.ledger_verify_digest(jsonb),
    kingres.ledger_inclusion_proof(regclass, bytea) FROM kl_auditor;

--------------------------------------------------------------------------
-- L-05: ledger_enable needs READ COMMITTED
--------------------------------------------------------------------------
CREATE TABLE r_rc (id int);
INSERT INTO r_rc VALUES (1);
BEGIN ISOLATION LEVEL REPEATABLE READ;
SELECT count(*) AS rows_seen FROM r_rc;
SELECT kingres.ledger_enable('r_rc');
ROLLBACK;
BEGIN ISOLATION LEVEL SERIALIZABLE;
SELECT kingres.ledger_enable('r_rc');
ROLLBACK;
SELECT count(*) AS r_rc_registered FROM kingres.ledger_table WHERE relid = 'r_rc'::regclass;
BEGIN ISOLATION LEVEL READ COMMITTED;
SELECT kingres.ledger_enable('r_rc');
COMMIT;
SELECT count(*) AS r_rc_registered FROM kingres.ledger_table WHERE relid = 'r_rc'::regclass;

--------------------------------------------------------------------------
-- L-06: extension members are identified by membership, and cannot be
-- renamed, replaced or handed to another owner
--------------------------------------------------------------------------
DO $$
BEGIN
    BEGIN
        ALTER FUNCTION kingres.ledger_capture() RENAME TO ledger_capture_x;
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'rename rejected: % %', SQLSTATE, SQLERRM;
        RETURN;
    END;
    -- The rename went through: the old hook stops recognising ledger tables.
    BEGIN
        DROP TABLE r_led;
        RAISE NOTICE 'FAIL: DROP TABLE of a ledger table succeeded after the rename';
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'drop rejected: % %', SQLSTATE, SQLERRM;
    END;
    RAISE EXCEPTION 'undo' USING ERRCODE = 'KL999';
EXCEPTION WHEN SQLSTATE 'KL999' THEN
    NULL;
END $$;
SELECT pg_temp.try(ARRAY[
    'ALTER FUNCTION kingres.ledger_capture() RENAME TO ledger_capture_x',
    'ALTER FUNCTION kingres.ledger_block_modify() RENAME TO other_name',
    'ALTER FUNCTION kingres.ledger_verify(bigint, bigint) RENAME TO ledger_verify_x',
    'ALTER FUNCTION kingres.ledger_capture() OWNER TO kl_owner',
    'ALTER EVENT TRIGGER kingres_ledger_guard_end DISABLE',
    'ALTER EVENT TRIGGER kingres_ledger_guard_end ENABLE REPLICA',
    'ALTER EVENT TRIGGER kingres_ledger_guard_end RENAME TO other_name',
    'ALTER EVENT TRIGGER kingres_ledger_guard_end OWNER TO kl_owner',
    'DROP EVENT TRIGGER kingres_ledger_guard_end',
    'GRANT EXECUTE ON FUNCTION kingres.ledger_capture() TO PUBLIC',
    'GRANT EXECUTE ON FUNCTION kingres.ledger_register_table(regclass) TO kl_other',
    'GRANT INSERT ON kingres.ledger_entry TO kl_other',
    'GRANT ALL ON kingres.ledger_blocks TO kl_other',
    'REVOKE SELECT ON kingres.ledger_blocks FROM CURRENT_USER',
    'REVOKE ALL ON kingres.ledger_entry FROM CURRENT_USER']);
-- A function that only has the name of an extension function does not
-- satisfy the verification.
CREATE FUNCTION kingres.ledger_capture(int) RETURNS int LANGUAGE sql
AS 'SELECT 1';
UPDATE pg_trigger SET tgfoid = 'kingres.ledger_capture(int)'::regprocedure
WHERE tgrelid = 'r_led'::regclass AND tgname = 'kingres_ledger_capture';
SELECT * FROM public.vreport() WHERE object = 'public.r_led';
UPDATE pg_trigger SET tgfoid = 'kingres.ledger_capture()'::regprocedure
WHERE tgrelid = 'r_led'::regclass AND tgname = 'kingres_ledger_capture';
DROP FUNCTION kingres.ledger_capture(int);
SELECT count(*) AS problems_after_repair FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- L-07: dependency cascades, generated columns
--------------------------------------------------------------------------
CREATE FUNCTION r_dbl(int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT $1 * 2';
CREATE TABLE r_gen (id int, g int GENERATED ALWAYS AS (r_dbl(id)) STORED, v text);
DO $$
BEGIN
    BEGIN
        PERFORM kingres.ledger_enable('r_gen');
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'enable rejected: % %', SQLSTATE, SQLERRM;
        RETURN;
    END;
    BEGIN
        DROP FUNCTION r_dbl(int) CASCADE;
        RAISE NOTICE 'FAIL: the column of a ledger table was dropped by a cascade';
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'cascade rejected: % %', SQLSTATE, SQLERRM;
    END;
    RAISE EXCEPTION 'undo' USING ERRCODE = 'KL999';
EXCEPTION WHEN SQLSTATE 'KL999' THEN
    NULL;
END $$;
SELECT pg_temp.try(ARRAY['ALTER TABLE r_led ADD COLUMN gc int GENERATED ALWAYS AS (id * 2) STORED']);
-- A cascade through a domain built on the type of a ledger column.
CREATE TYPE r_base AS ENUM ('a', 'b');
CREATE DOMAIN r_dom AS r_base;
CREATE TABLE r_domcol (id int, d r_dom);
SELECT kingres.ledger_enable('r_domcol');
SELECT pg_temp.try(ARRAY['DROP TYPE r_base CASCADE', 'DROP DOMAIN r_dom CASCADE']);
-- A cascade through an extension that provides a domain's base type.
SELECT count(*) AS columns_of_r_domcol FROM pg_attribute
WHERE attrelid = 'r_domcol'::regclass AND attnum > 0 AND NOT attisdropped;

--------------------------------------------------------------------------
-- L-10: no error where false is the answer
--------------------------------------------------------------------------
SELECT kingres.ledger_verify_digest(jsonb_build_object(
           'height', 9223372036854775807, 'block_hash', repeat('00', 32))) AS max_height;
SELECT kingres.ledger_verify_digest(jsonb_build_object(
           'height', 1e30, 'block_hash', repeat('00', 32))) AS huge_height;
SELECT kingres.ledger_verify_digest(jsonb_build_object(
           'height', 1.5, 'block_hash', repeat('00', 32))) AS fractional_height;
SELECT kingres.ledger_verify_digest(jsonb_build_object(
           'height', -1, 'block_hash', repeat('00', 32))) AS negative_height;

--------------------------------------------------------------------------
-- L-11: canonical integers in proofs, ADD COLUMN types, documented
-- limitations
--------------------------------------------------------------------------
SELECT kingres.ledger_generate_digest() IS NOT NULL AS block_built;
SELECT kingres.ledger_generate_digest() AS dg \gset
SELECT kingres.ledger_inclusion_proof('r_led', kingres.ledger_row_hash(q.*)) AS pf
FROM r_led q WHERE id = 1 \gset
SELECT kingres.ledger_verify_proof(:'pf'::jsonb, :'dg'::jsonb) AS original;
SELECT f.field, f.delta,
       kingres.ledger_verify_proof(
           jsonb_set(:'pf'::jsonb, ARRAY[f.field], to_jsonb((:'pf'::jsonb ->> f.field)::numeric + f.delta)),
           :'dg'::jsonb) AS accepted
FROM (VALUES ('entry_index', 0.4), ('entry_index', 0.5), ('entry_index', 1e-10),
             ('tx_index', 0.4), ('seq', 0.3), ('table_id', 0.4),
             ('ledger_txid', 0.2), ('entry_count', 0.4)) f(field, delta)
ORDER BY 1, 2;
SELECT kingres.ledger_verify_proof(
           jsonb_set(:'pf'::jsonb, '{entry_index}', to_jsonb(1e400::numeric)),
           :'dg'::jsonb) AS out_of_range;
SELECT kingres.ledger_verify_proof(:'pf'::jsonb,
           jsonb_set(:'dg'::jsonb, '{height}',
                     to_jsonb((:'dg'::jsonb ->> 'height')::numeric + 0.4))) AS fractional_digest_height;

-- ADD COLUMN passes the same type checks as ledger_enable.
CREATE TYPE r_pair AS (a int, b int);
CREATE TYPE r_color AS ENUM ('red', 'green');
SELECT pg_temp.try(ARRAY[
    'ALTER TABLE r_led ADD COLUMN c_pair r_pair',
    'ALTER TABLE r_led ADD COLUMN c_pair_arr r_pair[]',
    'ALTER TABLE r_led ADD COLUMN c_color_arr r_color[]',
    'ALTER TABLE r_led ADD COLUMN c_acl aclitem',
    'ALTER TABLE r_led ADD COLUMN c_acl_arr aclitem[]']);
ALTER TABLE r_led ADD COLUMN c_ok text;
ALTER TABLE r_led ADD COLUMN c_arr int[];
INSERT INTO r_led VALUES (2, 'two', 'ok', ARRAY[1, 2]);
SELECT count(*) AS problems_after_add_column FROM kingres.ledger_verify();

-- Documented limitation: the ledger follows the table by OID, not by name.
-- A renamed ledger table keeps verifying while a fresh table with the old
-- name is what applications read.
ALTER TABLE r_led RENAME TO r_led_old;
CREATE TABLE r_led (id int PRIMARY KEY, v text);
INSERT INTO r_led VALUES (99, 'not in the ledger');
SELECT count(*) AS problems_with_fresh_table_under_old_name FROM kingres.ledger_verify();
SELECT t.relid::regclass AS ledger_table FROM kingres.ledger_table t
WHERE t.relid = 'r_led_old'::regclass;
DROP TABLE r_led;
ALTER TABLE r_led_old RENAME TO r_led;

-- A trigger of the extension on a table that is not registered (a restored
-- database that kept the triggers but not the ledger) is reported.
CREATE TABLE r_unreg (a int);
CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON r_unreg
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture('424242');
SELECT * FROM public.vreport() WHERE object = 'public.r_unreg';
DROP TABLE r_unreg;

--------------------------------------------------------------------------
-- L-04: what enabling a table blocks, as the README states it
--------------------------------------------------------------------------
DROP ROLE IF EXISTS r_tmp;
CREATE ROLE r_tmp;
CREATE SCHEMA r_sch AUTHORIZATION r_tmp;
CREATE TABLE r_parent (id int PRIMARY KEY);
INSERT INTO r_parent VALUES (1), (2);
CREATE TABLE r_sch.r_child (id int, pid int REFERENCES r_parent ON DELETE CASCADE);
ALTER TABLE r_sch.r_child OWNER TO r_tmp;
SELECT kingres.ledger_enable('r_sch.r_child');
INSERT INTO r_sch.r_child VALUES (1, 1);
SELECT pg_temp.try(ARRAY[
    'DELETE FROM r_parent WHERE id = 1',
    'TRUNCATE r_parent',
    'DROP TABLE r_sch.r_child',
    'DROP SCHEMA r_sch CASCADE',
    'DROP OWNED BY r_tmp',
    'DROP ROLE r_tmp']);
-- A parent row that no ledger row refers to can go.
DELETE FROM r_parent WHERE id = 2;
-- The way to retire the role: hand its tables to another role first.
REASSIGN OWNED BY r_tmp TO kl_owner;
DROP ROLE r_tmp;
SELECT pg_get_userbyid(relowner) AS owner_after_reassign FROM pg_class WHERE oid = 'r_sch.r_child'::regclass;
SELECT count(*) AS problems_after_reassign FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- A parallel query does not break anything: the functions that read tables
-- are not parallel safe, the pure ones are.
--------------------------------------------------------------------------
SET debug_parallel_query = on;
SELECT count(*) AS rows_hashed FROM r_x q WHERE kingres.ledger_row_hash(q.*) IS NOT NULL;
SELECT count(*) AS problems_in_parallel_mode FROM kingres.ledger_verify();
SELECT kingres.ledger_verify_proof(:'pf'::jsonb, :'dg'::jsonb) AS proof_in_parallel_mode;
RESET debug_parallel_query;

--------------------------------------------------------------------------
-- A table that is hostile to verification, and tampering next to it: the
-- other tables are still checked.
--------------------------------------------------------------------------
SELECT count(*) AS problems_before FROM kingres.ledger_verify();
UPDATE pg_trigger SET tgenabled = 'D'
WHERE tgrelid = 'r_other'::regclass AND tgname LIKE 'kingres_ledger%';
\c
SET search_path = public;
INSERT INTO r_other VALUES (2, 'smuggled');
UPDATE pg_trigger SET tgenabled = 'A'
WHERE tgrelid = 'r_other'::regclass AND tgname LIKE 'kingres_ledger%';
UPDATE pg_attribute SET atttypid = 'aclitem'::regtype
WHERE attrelid = 'r_broken'::regclass AND attname = 'z';
SELECT * FROM public.vreport();
UPDATE pg_attribute SET atttypid = 'text'::regtype
WHERE attrelid = 'r_broken'::regclass AND attname = 'z';
UPDATE pg_trigger SET tgenabled = 'D'
WHERE tgrelid = 'r_other'::regclass AND tgname LIKE 'kingres_ledger%';
\c
SET search_path = public;
DELETE FROM r_other WHERE a = 2;
UPDATE pg_trigger SET tgenabled = 'A'
WHERE tgrelid = 'r_other'::regclass AND tgname LIKE 'kingres_ledger%';
SELECT count(*) AS problems_after_cleanup FROM kingres.ledger_verify();

-- REINDEX of the ledger catalog tables, also CONCURRENTLY, changes nothing
-- that the ledger covers.
REINDEX TABLE kingres.ledger_entry;
REINDEX TABLE CONCURRENTLY kingres.ledger_blocks;
SELECT count(*) AS problems_after_reindex FROM kingres.ledger_verify();
