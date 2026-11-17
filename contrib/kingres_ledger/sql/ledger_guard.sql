-- ledger_guard: a row of a ledger table exists only because the code of the
-- extension wrote it (L9-01).  The eight tables of the ledger against every
-- way of writing them, as a member of pg_write_all_data (the role that
-- bypasses table privileges), as a role with nothing, as a superuser, and as a
-- superuser who turned allow_manual_write on.  Every statement that is
-- allowed is undone (an exception rolls its block back), so the ledger is the
-- same at the end and ledger_verify finds nothing.  Works in a database of
-- its own.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
SET search_path = public;
\set VERBOSITY terse
SELECT current_database() AS orig_db \gset
DROP DATABASE IF EXISTS kl_guard_t;
DROP ROLE IF EXISTS kl_g_member;
DROP ROLE IF EXISTS kl_g_plain;
CREATE ROLE kl_g_member IN ROLE pg_write_all_data;
CREATE ROLE kl_g_plain;
CREATE DATABASE kl_guard_t;
\c kl_guard_t
SET search_path = public;
\set VERBOSITY terse
CREATE EXTENSION kingres_ledger;
GRANT USAGE ON SCHEMA kingres TO kl_g_member, kl_g_plain;
GRANT CREATE ON SCHEMA public TO kl_g_member;

-- An honest ledger: a table, a block, a digest, a recorded verification.
CREATE TABLE docs (id int PRIMARY KEY, body text);
SELECT kingres.ledger_enable('docs');
INSERT INTO docs VALUES (1, 'a'), (2, 'b');
SELECT kingres.ledger_build_block() AS block;
INSERT INTO docs VALUES (3, 'c');
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest;
SELECT kingres.ledger_verify_recorded() IS NOT NULL AS recorded;
SELECT count(*) AS findings FROM kingres.ledger_verify();

-- What each table needs for a statement that parses; the guard acts before
-- any value is looked at.
CREATE TABLE g_spec (ord int, tbl text, cols text, vals text, c1 text);
INSERT INTO g_spec VALUES
 (1, 'ledger_table', 'relid, mode', '''docs''::regclass, ''append_only''', 'mode'),
 (2, 'ledger_chain', 'chain_id', 'sha256(''x'')', 'chain_id'),
 (3, 'ledger_entry', 'ledger_txid, seq, table_id, op, row_hash', '900001, 0, 1, ''I'', sha256(''x'')', 'seq'),
 (4, 'ledger_pending', 'ledger_txid, user_name', '900001, ''x''', 'user_name'),
 (5, 'ledger_tx', 'ledger_txid, block_height, tx_index, entry_count, merkle_root, user_name', '900001, 0, 5, 1, sha256(''x''), ''x''', 'user_name'),
 (6, 'ledger_blocks', 'height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at, block_hash', '900, 1, sha256(''a''), sha256(''b''), 1, now(), sha256(''c'')', 'tx_count'),
 (7, 'ledger_digest', 'generated_at, generated_by, height, block_hash, digest', 'now(), ''x'', 0, sha256(''c''), ''{}''', 'height'),
 (8, 'ledger_verification', 'started_at, started_by, from_height, state', 'now(), ''x'', 0, ''clean''', 'from_height');

-- Views and SECURITY DEFINER functions of the member, one per table.
DO $$
DECLARE s record;
BEGIN
    FOR s IN SELECT * FROM g_spec ORDER BY ord LOOP
        EXECUTE format('CREATE VIEW public.g_v_%s AS SELECT * FROM kingres.%I', s.tbl, s.tbl);
        EXECUTE format('ALTER VIEW public.g_v_%s OWNER TO kl_g_member', s.tbl);
        EXECUTE format('GRANT ALL ON public.g_v_%s TO kl_g_plain', s.tbl);
        EXECUTE format('CREATE FUNCTION public.g_f_%s() RETURNS void LANGUAGE sql SECURITY DEFINER AS %L',
                       s.tbl, format('INSERT INTO kingres.%I (%s) VALUES (%s)', s.tbl, s.cols, s.vals));
        EXECUTE format('ALTER FUNCTION public.g_f_%s() OWNER TO kl_g_member', s.tbl);
        EXECUTE format('GRANT EXECUTE ON FUNCTION public.g_f_%s() TO kl_g_plain', s.tbl);
    END LOOP;
END $$;

CREATE TABLE g_results (who text, form text, tbl text, outcome text);
GRANT ALL ON g_results TO PUBLIC;
GRANT SELECT ON g_spec TO PUBLIC;

-- Runs every form on every table as the current role.  A statement that is
-- allowed is rolled back by an exception of its own.
CREATE FUNCTION public.g_run(who text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    s record;
    f record;
    res text;
BEGIN
    FOR s IN SELECT * FROM g_spec ORDER BY ord LOOP
        FOR f IN SELECT * FROM (VALUES
            (1, 'insert', format('INSERT INTO kingres.%I (%s) VALUES (%s)', s.tbl, s.cols, s.vals)),
            (2, 'on conflict', format('INSERT INTO kingres.%I (%s) VALUES (%s) ON CONFLICT DO NOTHING', s.tbl, s.cols, s.vals)),
            (3, 'merge', format('MERGE INTO kingres.%I t USING (SELECT 1 AS x) u ON false WHEN NOT MATCHED THEN INSERT (%s) VALUES (%s)', s.tbl, s.cols, s.vals)),
            (4, 'view of the member', format('INSERT INTO public.g_v_%s (%s) VALUES (%s)', s.tbl, s.cols, s.vals)),
            (5, 'definer function of the member', format('SELECT public.g_f_%s()', s.tbl)),
            (6, 'delete', format('DELETE FROM kingres.%I WHERE false', s.tbl)),
            (7, 'update', format('UPDATE kingres.%I SET %I = %I WHERE false', s.tbl, s.c1, s.c1)),
            (8, 'select for update', format('SELECT 1 FROM kingres.%I FOR UPDATE', s.tbl))
        ) AS v(n, form, sql) ORDER BY n LOOP
            BEGIN
                EXECUTE f.sql;
                RAISE EXCEPTION 'undo' USING ERRCODE = 'U0001';
            EXCEPTION
                WHEN SQLSTATE 'U0001' THEN res := 'allowed';
                WHEN OTHERS THEN
                    res := CASE WHEN SQLERRM LIKE 'cannot write to table%' OR SQLERRM LIKE 'cannot lock rows%' THEN 'refused by the guard'
                                WHEN SQLSTATE = 'KL001' THEN 'refused as append-only'
                                WHEN SQLSTATE = '42501' THEN 'permission denied'
                                ELSE SQLSTATE || ' ' || SQLERRM END;
            END;
            INSERT INTO public.g_results VALUES (who, f.form, s.tbl, res);
        END LOOP;
    END LOOP;
END $$;

-- The member of pg_write_all_data, the role with nothing, a superuser.
SET ROLE kl_g_member;
SELECT public.g_run('member');
RESET ROLE;
SET ROLE kl_g_plain;
SELECT public.g_run('plain role');
RESET ROLE;
SELECT public.g_run('superuser');
-- A superuser who asked for it; and the member and the plain role, who cannot
-- ask (the setting is the superuser's), and a superuser acting as the member.
SET kingres_ledger.allow_manual_write = on;
SELECT public.g_run('superuser, manual write on');
SET ROLE kl_g_member;
SELECT public.g_run('member in that session');
RESET ROLE;
RESET kingres_ledger.allow_manual_write;

SELECT who, form, outcome, count(*) AS tables
  FROM g_results GROUP BY who, form, outcome
 ORDER BY CASE who WHEN 'member' THEN 1 WHEN 'plain role' THEN 2 WHEN 'superuser' THEN 3
                   WHEN 'superuser, manual write on' THEN 4 ELSE 5 END,
          CASE form WHEN 'insert' THEN 1 WHEN 'on conflict' THEN 2 WHEN 'merge' THEN 3
                    WHEN 'view of the member' THEN 4 WHEN 'definer function of the member' THEN 5
                    WHEN 'delete' THEN 6 WHEN 'update' THEN 7 ELSE 8 END, outcome;

-- Nothing was left behind, and the chain verifies.
SELECT count(*) AS findings FROM kingres.ledger_verify();
SELECT count(*) AS rows_of_ledger_table FROM kingres.ledger_table;

-- The messages.
SET ROLE kl_g_member;
INSERT INTO kingres.ledger_blocks VALUES (900, 1, sha256('a'), sha256('b'), 1, now(), sha256('c'));
DELETE FROM kingres.ledger_pending;
RESET ROLE;
INSERT INTO kingres.ledger_pending (ledger_txid, user_name) VALUES (900001, 'x');
COPY kingres.ledger_pending (ledger_txid, user_name) FROM PROGRAM 'echo 900001 x';

-- The setting is the superuser's, and for one session: it cannot be given a
-- value anywhere else.
SHOW kingres_ledger.allow_manual_write;
SET ROLE kl_g_member;
SET kingres_ledger.allow_manual_write = on;
RESET ROLE;
ALTER ROLE kl_g_member SET kingres_ledger.allow_manual_write = on;
ALTER ROLE ALL SET kingres_ledger.allow_manual_write = on;
ALTER DATABASE kl_guard_t SET kingres_ledger.allow_manual_write = on;
ALTER SYSTEM SET kingres_ledger.allow_manual_write = on;
SELECT set_config('kingres_ledger.allow_manual_write', 'on', false);
SELECT current_setting('kingres_ledger.allow_manual_write');
RESET kingres_ledger.allow_manual_write;
-- Turning it off needs no care, and it is off in a new session.
ALTER ROLE kl_g_member SET kingres_ledger.allow_manual_write = off;
ALTER ROLE kl_g_member RESET kingres_ledger.allow_manual_write;
SELECT count(*) AS settings_stored
  FROM pg_db_role_setting WHERE setconfig::text LIKE '%allow_manual_write%';

-- L10-01, L10-02: there is no function that takes the content of a write.
SELECT count(*) AS internal_write_functions FROM pg_proc WHERE proname = 'ledger_internal_write';

-- pg_maintain (L9-14) changes no row of the chain, but may take any lock on any
-- table: a lock that is held for the whole transaction stops the block builder
-- and every reader.  The ledger refuses the locks that block readers or writers
-- to a role that the lock would have been allowed to; a role with nothing gets
-- the usual error, a lock that blocks nobody (ACCESS SHARE) is allowed, and
-- VACUUM works.
CREATE ROLE kl_g_maint IN ROLE pg_maintain;
GRANT USAGE ON SCHEMA kingres TO kl_g_maint;
SET ROLE kl_g_maint;
BEGIN; LOCK TABLE kingres.ledger_blocks IN ACCESS EXCLUSIVE MODE; ROLLBACK;
BEGIN; LOCK TABLE kingres.ledger_chain IN SHARE ROW EXCLUSIVE MODE; ROLLBACK;
BEGIN; LOCK TABLE kingres.ledger_blocks IN ACCESS SHARE MODE; ROLLBACK;
BEGIN; LOCK TABLE kingres.ledger_blocks IN ROW EXCLUSIVE MODE; ROLLBACK;
VACUUM kingres.ledger_blocks;
SET ROLE kl_g_member;
BEGIN; LOCK TABLE kingres.ledger_blocks IN EXCLUSIVE MODE; ROLLBACK;
SET ROLE kl_g_plain;
BEGIN; LOCK TABLE kingres.ledger_blocks IN ACCESS EXCLUSIVE MODE; ROLLBACK;
RESET ROLE;
BEGIN; LOCK TABLE kingres.ledger_blocks IN ACCESS EXCLUSIVE MODE; ROLLBACK;

-- The honest flows, with the setting off: the same as before.
SHOW kingres_ledger.allow_manual_write;
INSERT INTO docs VALUES (4, 'd');
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest;
SELECT kingres.ledger_verify_recorded() IS NOT NULL AS recorded;
CREATE TABLE docs2 (id int, body text);
INSERT INTO docs2 VALUES (1, 'x');
SELECT kingres.ledger_enable('docs2', 'updatable');
UPDATE docs2 SET body = 'y';
DELETE FROM docs2;
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT count(*) AS findings FROM kingres.ledger_verify();

\c :orig_db
SET search_path = public;
DROP DATABASE kl_guard_t;
DROP ROLE kl_g_member;
DROP ROLE kl_g_plain;
DROP ROLE kl_g_maint;
