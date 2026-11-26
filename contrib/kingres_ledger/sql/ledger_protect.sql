-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- ledger_protect: the ProcessUtility hook.  Every statement that would switch
-- the ledger off is rejected with SQLSTATE KL002, also for superusers and for
-- the owner of the table; ordinary DDL keeps working.
SET search_path = public;
\set VERBOSITY terse

CREATE TABLE lp (id int PRIMARY KEY, a int, note text);
SELECT kingres.ledger_enable('lp');
INSERT INTO lp VALUES (1, 10, 'x');
CREATE TABLE plain (id int, a int);
CREATE TABLE parent_p (a int) PARTITION BY RANGE (a);
CREATE SCHEMA ledger_s;
CREATE TABLE ledger_s.ls (a int);
SELECT kingres.ledger_enable('ledger_s.ls');
CREATE DOMAIN dflt AS int DEFAULT 5;
CREATE DOMAIN nodflt AS int;
CREATE TABLE cascade_target (id int);

-- Statements that must be rejected.  Each runs in its own subtransaction and
-- its outcome is reported.  A statement that is accepted is an explicit
-- failure: a WARNING line that starts with "FAIL", and an error at the end of
-- the call, so that a wrongly accepted statement cannot hide among the lines
-- of the expected output.  An accepted statement is undone.
CREATE FUNCTION pg_temp.try(stmts text[]) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    stmt text;
    accepted int := 0;
BEGIN
    FOREACH stmt IN ARRAY stmts LOOP
        BEGIN
            EXECUTE stmt;
            RAISE WARNING 'FAIL (accepted, must be rejected): %', stmt;
            accepted := accepted + 1;
            RAISE EXCEPTION 'undo' USING ERRCODE = 'KL999';
        EXCEPTION WHEN SQLSTATE 'KL999' THEN
            NULL;
        WHEN OTHERS THEN
            RAISE NOTICE '% -> % %', stmt, SQLSTATE, SQLERRM;
        END;
    END LOOP;
    IF accepted > 0 THEN
        RAISE EXCEPTION 'FAIL: % statement(s) were accepted that must be rejected', accepted
            USING ERRCODE = 'KL998';
    END IF;
END $$;

-- Rules of the specification, on a table under ledger control.
SELECT pg_temp.try(ARRAY[
    'DROP TABLE lp',
    'DROP TABLE IF EXISTS plain, lp',
    'DROP TABLE lp CASCADE',
    'ALTER TABLE lp DISABLE TRIGGER kingres_ledger_capture',
    'ALTER TABLE lp DISABLE TRIGGER kingres_ledger_block_modify',
    'ALTER TABLE lp DISABLE TRIGGER ALL',
    'ALTER TABLE lp DISABLE TRIGGER USER',
    'ALTER TABLE lp ENABLE TRIGGER kingres_ledger_capture',
    'ALTER TABLE lp ENABLE REPLICA TRIGGER kingres_ledger_capture',
    'ALTER TABLE lp ENABLE ALWAYS TRIGGER kingres_ledger_capture',
    'ALTER TABLE lp ENABLE TRIGGER ALL',
    'ALTER TABLE lp DROP COLUMN a',
    'ALTER TABLE lp ALTER COLUMN a TYPE bigint',
    'ALTER TABLE lp RENAME COLUMN a TO z',
    'ALTER TABLE lp SET SCHEMA ledger_s',
    'ALTER TABLE ledger_s.ls SET SCHEMA public',
    'DROP TRIGGER kingres_ledger_capture ON lp',
    'DROP TRIGGER kingres_ledger_block_modify ON lp',
    'DROP TRIGGER kingres_ledger_block_truncate ON lp',
    'DROP TRIGGER IF EXISTS kingres_ledger_capture ON lp',
    'ALTER TRIGGER kingres_ledger_capture ON lp RENAME TO harmless',
    'CREATE OR REPLACE TRIGGER kingres_ledger_capture AFTER INSERT ON lp FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify()',
    'ALTER TABLE parent_p ATTACH PARTITION lp FOR VALUES FROM (0) TO (10)',
    'CREATE TABLE child_of_lp () INHERITS (lp)',
    'ALTER TABLE plain INHERIT lp',
    'ALTER TABLE lp INHERIT plain',
    'ALTER TABLE lp SET UNLOGGED',
    'CREATE RULE r1 AS ON INSERT TO lp DO INSTEAD NOTHING',
    'DROP SCHEMA ledger_s CASCADE',
    'DROP OWNED BY CURRENT_USER',
    'DROP EXTENSION kingres_ledger',
    'DROP EXTENSION IF EXISTS kingres_ledger CASCADE',
    'DROP SCHEMA kingres CASCADE',
    'ALTER SCHEMA kingres RENAME TO kingres_old']);

-- ADD COLUMN: only nullable columns without a default.
SELECT pg_temp.try(ARRAY[
    'ALTER TABLE lp ADD COLUMN c1 int DEFAULT 5',
    'ALTER TABLE lp ADD COLUMN c2 int NOT NULL',
    'ALTER TABLE lp ADD COLUMN c3 int GENERATED ALWAYS AS (a + 1) STORED',
    'ALTER TABLE lp ADD COLUMN c4 int GENERATED ALWAYS AS IDENTITY',
    'ALTER TABLE lp ADD COLUMN c5 dflt',
    'ALTER TABLE lp ADD COLUMN c6 int CHECK (c6 > 0)',
    'ALTER TABLE lp ADD COLUMN c7 int UNIQUE',
    'ALTER TABLE lp ALTER COLUMN a SET EXPRESSION AS (1)']);

-- The ledger catalog tables: any DDL.
SELECT pg_temp.try(ARRAY[
    'ALTER TABLE kingres.ledger_entry ADD COLUMN x int',
    'ALTER TABLE kingres.ledger_chain ADD COLUMN x int',
    'ALTER TABLE kingres.ledger_chain DISABLE TRIGGER ALL',
    'DROP TABLE kingres.ledger_chain',
    'CREATE TRIGGER evil2 AFTER INSERT ON kingres.ledger_chain FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify()',
    'ALTER TABLE kingres.ledger_entry DISABLE TRIGGER ALL',
    'ALTER TABLE kingres.ledger_entry DROP CONSTRAINT ledger_entry_pkey',
    'ALTER TABLE kingres.ledger_blocks RENAME TO blocks_old',
    'ALTER TABLE kingres.ledger_blocks RENAME COLUMN height TO h',
    'ALTER TABLE kingres.ledger_blocks RENAME CONSTRAINT ledger_blocks_pkey TO pk',
    'ALTER INDEX kingres.ledger_blocks_pkey RENAME TO pk',
    'ALTER TABLE kingres.ledger_tx SET SCHEMA public',
    'ALTER TABLE kingres.ledger_pending OWNER TO CURRENT_USER',
    'DROP TABLE kingres.ledger_blocks',
    'DROP TABLE kingres.ledger_blocks CASCADE',
    'DROP INDEX kingres.ledger_entry_row_hash_idx',
    'CREATE INDEX ON kingres.ledger_blocks (closed_at)',
    'CREATE TRIGGER evil AFTER INSERT ON kingres.ledger_tx FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify()',
    'DROP TRIGGER kingres_ledger_block_modify ON kingres.ledger_blocks',
    'DROP TRIGGER kingres_ledger_block_truncate ON kingres.ledger_blocks',
    'ALTER TRIGGER kingres_ledger_block_modify ON kingres.ledger_blocks RENAME TO x',
    'CREATE RULE r2 AS ON INSERT TO kingres.ledger_entry DO INSTEAD NOTHING',
    'CREATE POLICY p1 ON kingres.ledger_entry USING (false)',
    'ALTER TABLE kingres.ledger_entry ENABLE ROW LEVEL SECURITY']);

-- The functions and the extension.
SELECT pg_temp.try(ARRAY[
    'CREATE OR REPLACE FUNCTION kingres.ledger_capture() RETURNS trigger LANGUAGE plpgsql AS ''BEGIN RETURN NULL; END''',
    'CREATE OR REPLACE FUNCTION kingres.ledger_build_block() RETURNS bigint LANGUAGE sql AS ''SELECT NULL::bigint''',
    'ALTER FUNCTION kingres.ledger_capture() SECURITY INVOKER',
    'ALTER FUNCTION kingres.ledger_capture() SET search_path = public',
    'ALTER FUNCTION kingres.ledger_capture() SET SCHEMA public',
    'DROP FUNCTION kingres.ledger_capture()',
    'DROP AGGREGATE kingres.ledger_merkle_root(bytea)',
    'ALTER EXTENSION kingres_ledger DROP TABLE kingres.ledger_entry',
    'ALTER EXTENSION kingres_ledger SET SCHEMA public']);

-- DROP TYPE ... CASCADE would drop the column of a ledger table.
SELECT pg_temp.try(ARRAY[
    'DROP TYPE color CASCADE',
    'DROP TYPE color',
    'DROP TYPE _color CASCADE']);

-- The same rules apply to the owner of the table.
SET ROLE kl_owner;
SELECT pg_temp.try(ARRAY[
    'DROP TABLE with_enum',
    'ALTER TABLE with_enum DISABLE TRIGGER ALL',
    'ALTER TABLE with_enum DROP COLUMN c',
    'DROP TRIGGER kingres_ledger_capture ON with_enum',
    'ALTER TABLE with_enum ADD COLUMN extra int DEFAULT 1']);
RESET ROLE;

-- The state is unchanged: the ledger tables still exist, verification is clean.
SELECT count(*) AS problems FROM kingres.ledger_verify();
SELECT count(*) AS armed_triggers FROM pg_trigger
WHERE tgname = 'kingres_ledger_capture' AND tgenabled = 'A';

--------------------------------------------------------------------------
-- Allowed operations
--------------------------------------------------------------------------
-- Ordinary tables are not touched by the hook.
ALTER TABLE plain ADD COLUMN b int DEFAULT 1;
ALTER TABLE plain DROP COLUMN b;
CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON plain FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
ALTER TABLE plain DISABLE TRIGGER kingres_ledger_capture;
DROP TRIGGER kingres_ledger_capture ON plain;
DROP TABLE plain;

-- Harmless changes of a ledger table.
ALTER TABLE lp ADD COLUMN extra1 text;
ALTER TABLE lp ADD COLUMN extra2 nodflt NULL;
ALTER TABLE lp ALTER COLUMN a SET DEFAULT 7;
ALTER TABLE lp ADD CONSTRAINT lp_a_check CHECK (a > 0) NOT VALID;
CREATE INDEX lp_a_idx ON lp (a);
DROP INDEX lp_a_idx;
CREATE TRIGGER user_trigger AFTER INSERT ON lp FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
SELECT pg_temp.try(ARRAY['ALTER TABLE lp DISABLE TRIGGER user_trigger', 'DROP DOMAIN nodflt CASCADE']);
DROP TRIGGER user_trigger ON lp;
ALTER TABLE lp SET (fillfactor = 90);
ALTER TABLE lp OWNER TO CURRENT_USER;
ALTER TABLE lp RENAME TO lp2;
ALTER TABLE lp2 RENAME TO lp;

-- A new nullable column does not change the hashes of existing rows, and new
-- rows are captured with the new column.
SELECT count(*) AS problems_after_add_column FROM kingres.ledger_verify();
INSERT INTO lp (id, a, note, extra1) VALUES (2, 20, 'y', 'with extra');
INSERT INTO lp (id, a, note) VALUES (3, 30, 'z');
SELECT count(*) AS problems_after_inserts FROM kingres.ledger_verify();
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest_ok;
SELECT count(*) AS problems_after_block FROM kingres.ledger_verify();

-- Types that ledger columns use: an extension that provides the type cannot
-- be dropped with CASCADE, and an enum label, which is part of the row hash,
-- cannot be renamed; adding a label is harmless.
CREATE EXTENSION citext;
CREATE TABLE ci (a citext);
SELECT kingres.ledger_enable('ci');
INSERT INTO ci VALUES ('Mixed');
SELECT pg_temp.try(ARRAY['DROP EXTENSION citext CASCADE',
                         'ALTER TYPE color RENAME VALUE ''red'' TO ''crimson''']);
ALTER TYPE color ADD VALUE 'blue';
ALTER TYPE color RENAME TO colour;
ALTER TYPE colour RENAME TO color;
SELECT count(*) AS problems_after_type_changes FROM kingres.ledger_verify();

-- A table enabled in the transaction that tries to drop it.
BEGIN;
CREATE TABLE in_tx (a int);
SELECT kingres.ledger_enable('in_tx');
SELECT pg_temp.try(ARRAY['DROP TABLE in_tx', 'ALTER TABLE in_tx DISABLE TRIGGER ALL',
                         'DROP EXTENSION kingres_ledger', 'DROP SCHEMA public CASCADE']);
ROLLBACK;

--------------------------------------------------------------------------
-- Catalog checks of the extension itself
--------------------------------------------------------------------------
-- Every SQL or PL/pgSQL function and every SECURITY DEFINER function has a
-- fixed search_path.
SELECT p.proname AS function_without_search_path
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname = 'kingres' AND p.prokind = 'f'
  AND (p.prosecdef OR p.prolang <> (SELECT oid FROM pg_language WHERE lanname = 'c'))
  AND NOT COALESCE(p.proconfig::text LIKE '%search_path=%', false)
ORDER BY 1;
-- Every object of the extension is in schema kingres and named ledger_*.
SELECT count(*) AS members_outside_schema
FROM pg_depend d
JOIN pg_extension e ON e.oid = d.refobjid AND d.refclassid = 'pg_extension'::regclass
WHERE e.extname = 'kingres_ledger' AND d.deptype = 'e'
  AND ((d.classid = 'pg_proc'::regclass AND
        (SELECT pronamespace FROM pg_proc WHERE oid = d.objid) <> 'kingres'::regnamespace)
    OR (d.classid = 'pg_class'::regclass AND
        (SELECT relnamespace FROM pg_class WHERE oid = d.objid) <> 'kingres'::regnamespace));
SELECT count(*) AS members_not_named_ledger
FROM pg_depend d
JOIN pg_extension e ON e.oid = d.refobjid AND d.refclassid = 'pg_extension'::regclass
WHERE e.extname = 'kingres_ledger' AND d.deptype = 'e'
  AND ((d.classid = 'pg_proc'::regclass AND
        (SELECT proname FROM pg_proc WHERE oid = d.objid) NOT LIKE 'ledger\_%')
    OR (d.classid = 'pg_class'::regclass AND
        (SELECT relname FROM pg_class WHERE oid = d.objid) NOT LIKE 'ledger\_%'));
