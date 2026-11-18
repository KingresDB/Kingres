-- SPDX-License-Identifier: AGPL-3.0-only
--
-- ledger_paths: the invariant of the guard, and the paths to it (L10-01,
-- L10-02, L10-03, L10-13, L10-14).  No statement that a role can issue writes
-- a ledger table, whatever the role inherits or owns, except through the
-- public functions of the extension doing what they are documented to do, and
-- a superuser acting as itself with kingres_ledger.allow_manual_write on.
-- This test runs the roles that have everything but the superuser attribute
-- (a member of the owner role with INHERIT and no SET, which is how the
-- privileges of an owner are handed out on PostgreSQL 16 and later), the
-- locks and row locks that would stop the block builder, the marker of the
-- extension's own writes at every way out of a statement, the setting of a
-- role that was given the right to set it, the record that a manual write
-- leaves, the clauses of the digest check and the units of the clock limit.
-- Works in a database of its own.
SET search_path = public;
\set VERBOSITY terse
SELECT current_database() AS orig_db \gset
SELECT current_user AS owner_name \gset
DROP DATABASE IF EXISTS kl_paths_t;
DROP ROLE IF EXISTS kl_p_inh;
DROP ROLE IF EXISTS kl_p_maint;
DROP ROLE IF EXISTS kl_p_member;
DROP ROLE IF EXISTS kl_p_gset;
DROP ROLE IF EXISTS kl_p_plain;
CREATE ROLE kl_p_inh LOGIN;
GRANT :"owner_name" TO kl_p_inh WITH INHERIT TRUE, SET FALSE;
CREATE ROLE kl_p_maint LOGIN IN ROLE pg_maintain;
CREATE ROLE kl_p_member LOGIN IN ROLE pg_write_all_data;
CREATE ROLE kl_p_gset LOGIN;
CREATE ROLE kl_p_plain LOGIN;
CREATE DATABASE kl_paths_t;
\c kl_paths_t
SET search_path = public;
\set VERBOSITY terse
CREATE EXTENSION kingres_ledger;
GRANT USAGE ON SCHEMA kingres TO PUBLIC;
GRANT CREATE ON SCHEMA public TO kl_p_inh, kl_p_maint, kl_p_member, kl_p_gset, kl_p_plain;
GRANT SET ON PARAMETER kingres_ledger.allow_manual_write TO kl_p_gset;

CREATE TABLE docs (id int PRIMARY KEY, body text);
GRANT ALL ON docs TO kl_p_plain, kl_p_member;
SELECT kingres.ledger_enable('docs');
INSERT INTO docs VALUES (1, 'a'), (2, 'b');
SELECT kingres.ledger_build_block() AS block;
SELECT count(*) AS findings FROM kingres.ledger_verify();

-- The nine tables, and what each statement needs to parse.
CREATE TABLE p_tables (ord int, tbl text);
INSERT INTO p_tables VALUES (1, 'ledger_table'), (2, 'ledger_chain'), (3, 'ledger_entry'),
 (4, 'ledger_pending'), (5, 'ledger_tx'), (6, 'ledger_blocks'), (7, 'ledger_digest'),
 (8, 'ledger_verification'), (9, 'ledger_removed');
GRANT SELECT ON p_tables TO PUBLIC;
CREATE TABLE p_results (who text, form text, outcome text, n int);
GRANT ALL ON p_results TO PUBLIC;

-- A statement of the caller, tried inside a block that undoes it; the outcome
-- is the first words of the error, or "ran".
CREATE FUNCTION public.p_try(who text, form text, stmt text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE res text;
BEGIN
    BEGIN
        EXECUTE stmt;
        RAISE EXCEPTION 'undo' USING ERRCODE = 'U0001';
    EXCEPTION
        WHEN SQLSTATE 'U0001' THEN res := 'ran';
        WHEN OTHERS THEN
            res := CASE WHEN SQLERRM LIKE 'cannot write to table%' THEN 'refused by the guard'
                        WHEN SQLERRM LIKE 'cannot lock rows%' THEN 'refused by the guard (row lock)'
                        WHEN SQLERRM LIKE 'cannot lock with a mode%' THEN 'refused by the protection (lock)'
                        WHEN SQLERRM LIKE '%cannot run inside a transaction block%' THEN 'refused (transaction block)'
                        WHEN SQLSTATE = 'KL001' THEN 'refused as append-only'
                        WHEN SQLSTATE = '42501' THEN 'permission denied: ' || left(SQLERRM, 60)
                        ELSE SQLSTATE || ' ' || left(SQLERRM, 70) END;
    END;
    INSERT INTO public.p_results VALUES (who, form, res, 1);
END $$;
GRANT EXECUTE ON FUNCTION public.p_try(text, text, text) TO PUBLIC;

-- A SECURITY DEFINER function of a superuser that writes a block.
CREATE FUNCTION public.p_forge_block() RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    INSERT INTO kingres.ledger_blocks (height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at, block_hash)
    VALUES (900, 1, sha256('a'), sha256('b'), 1, now(), sha256('c'))
$$;
GRANT EXECUTE ON FUNCTION public.p_forge_block() TO PUBLIC;

--------------------------------------------------------------------------
-- L10-01, L10-02: the roles that have everything but the attribute
--------------------------------------------------------------------------
-- The owner role is a superuser; kl_p_inh inherits its privileges (EXECUTE on
-- every internal function) and cannot become it.  Every write of a table, and
-- every internal function with arguments of its choice, is refused.
CREATE FUNCTION public.p_run_inh() RETURNS void LANGUAGE plpgsql AS $$
DECLARE t record;
BEGIN
    FOR t IN SELECT tbl FROM p_tables ORDER BY ord LOOP
        PERFORM public.p_try('inheriting member', 'insert ' || t.tbl, format('INSERT INTO kingres.%I DEFAULT VALUES', t.tbl));
        PERFORM public.p_try('inheriting member', 'delete ' || t.tbl, format('DELETE FROM kingres.%I WHERE false', t.tbl));
    END LOOP;
    PERFORM public.p_try('inheriting member', 'register a table it does not own', 'SELECT kingres.ledger_register_table(''docs''::regclass)');
    PERFORM public.p_try('inheriting member', 'history_sync of any two tables', 'SELECT kingres.ledger_history_sync(''docs''::regclass, ''pg_class''::regclass)');
    PERFORM public.p_try('inheriting member', 'history_acl_sync of any two tables', 'SELECT kingres.ledger_history_acl_sync(''docs''::regclass, ''pg_class''::regclass)');
    PERFORM public.p_try('inheriting member', 'ledger_remove_stray', 'SELECT kingres.ledger_remove_stray(''x'')');
    PERFORM public.p_try('inheriting member', 'the setting', 'SET kingres_ledger.allow_manual_write = on');
    PERFORM public.p_try('inheriting member', 'ledger_build_block (documented)', 'SELECT kingres.ledger_build_block()');
    PERFORM public.p_try('inheriting member', 'ledger_verify (documented)', 'SELECT count(*) FROM kingres.ledger_verify()');
END $$;
SELECT count(*) AS blocks_before FROM kingres.ledger_blocks \gset
SELECT count(*) AS entries_before FROM kingres.ledger_entry \gset
\c - kl_p_inh
SET search_path = public;
SELECT public.p_run_inh();
-- It may even set the setting (it inherits the right to set every parameter
-- from the bootstrap superuser), and gets nothing from it: it is not a superuser,
-- and neither is the role outside a function of a superuser.
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_blocks DEFAULT VALUES;
SELECT public.p_forge_block();
RESET kingres_ledger.allow_manual_write;
-- A trigger of its own with the capture function and the identity of another
-- table (it can create it: it inherits EXECUTE on ledger_capture).
CREATE TABLE p_forge (id int);
CREATE TRIGGER p_forge_t AFTER INSERT ON p_forge FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture('1');
INSERT INTO p_forge VALUES (1);
CREATE TABLE p_forge2 (id int);
CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON p_forge2 FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture('999999');
ALTER TABLE p_forge2 ENABLE ALWAYS TRIGGER kingres_ledger_capture;
INSERT INTO p_forge2 VALUES (1);
CREATE TABLE p_bare (id int);
-- A second capture trigger on a registered table, with the identity of another
-- (it is not named like the triggers of the ledger, so the protection lets it be created).
CREATE TRIGGER p_dup AFTER INSERT ON docs FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture('7');
INSERT INTO docs VALUES (999, 'forged entries');
DROP TRIGGER p_dup ON docs;
-- ...or register the table by hand with an identity that was never issued, or
-- before it has the triggers of a ledger table.
SELECT kingres.ledger_register_table('p_forge2'::regclass);
SELECT kingres.ledger_register_table('p_bare'::regclass);
\c - :owner_name
SET search_path = public;
SELECT who, form, outcome FROM p_results WHERE who = 'inheriting member' ORDER BY form;
SELECT (SELECT count(*) FROM kingres.ledger_blocks) - :blocks_before AS new_blocks,
       (SELECT count(*) FROM kingres.ledger_entry) - :entries_before AS new_entries;
SELECT check_name, object FROM kingres.ledger_verify() ORDER BY 1, 2;

-- The superuser, with no setting, with arguments of its choice.
DELETE FROM p_results;
CREATE FUNCTION public.p_run_super() RETURNS void LANGUAGE plpgsql AS $$
DECLARE t record;
BEGIN
    FOR t IN SELECT tbl FROM p_tables ORDER BY ord LOOP
        PERFORM public.p_try('superuser', 'insert ' || t.tbl, format('INSERT INTO kingres.%I DEFAULT VALUES', t.tbl));
    END LOOP;
    PERFORM public.p_try('superuser', 'register a system table', 'SELECT kingres.ledger_register_table(''pg_class''::regclass)');
    PERFORM public.p_try('superuser', 'register a table with no triggers', 'SELECT kingres.ledger_register_table(''p_bare''::regclass)');
    PERFORM public.p_try('superuser', 'register a table with a forged identity', 'SELECT kingres.ledger_register_table(''p_forge2''::regclass)');
    PERFORM public.p_try('superuser', 'remove_stray without the setting', 'SELECT kingres.ledger_remove_stray(''x'')');
    PERFORM public.p_try('superuser', 'insert through the capture trigger of another table', 'INSERT INTO p_forge2 VALUES (1)');
END $$;
SELECT public.p_run_super();
SELECT who, form, outcome FROM p_results ORDER BY form;
SELECT check_name, object FROM kingres.ledger_verify() ORDER BY 1, 2;
-- A registration that is half made: the identity is the next free one (as an enable
-- would take it), the table has the capture trigger and nothing else of a
-- ledger table.  ledger_register_table refuses it, with ledger_table_defects.
-- (the next free identifier of the registry: no table has it yet)
SELECT COALESCE(max(table_id), 0) + 1 AS gap_id FROM kingres.ledger_table \gset
\c - kl_p_inh
SET search_path = public;
CREATE TABLE p_half (id int);
CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON p_half FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture(:'gap_id');
ALTER TABLE p_half ENABLE ALWAYS TRIGGER kingres_ledger_capture;
SELECT kingres.ledger_register_table('p_half'::regclass);
SELECT kingres.ledger_register_table('docs'::regclass);
\c - :owner_name
SET search_path = public;
SELECT count(*) AS registered_by_hand FROM kingres.ledger_table WHERE relid IN ('p_half'::regclass, 'p_forge2'::regclass, 'p_bare'::regclass);
-- No function of the extension is left that takes the content of a write.
SELECT count(*) AS functions_that_take_content
  FROM pg_proc p JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
                  AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
 WHERE p.proname IN ('ledger_internal_write', 'ledger_record_row');

--------------------------------------------------------------------------
-- The setting, in the hands of a role that may set it
--------------------------------------------------------------------------
-- A SECURITY DEFINER function of a superuser writes as that superuser, so the
-- check must look at the user outside it (L10-13): the role that called it, or
-- the role the superuser's session switched to, is not a superuser.
\c - kl_p_gset
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.p_forge_block();
SELECT public.p_try('role with the right to set it', 'its own insert', 'INSERT INTO kingres.ledger_blocks DEFAULT VALUES');
\c - :owner_name
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SET ROLE kl_p_plain;
SELECT public.p_forge_block();
RESET ROLE;
RESET kingres_ledger.allow_manual_write;
-- A function with a SET clause naming the setting is refused when it is created.
CREATE FUNCTION public.p_setclause() RETURNS void LANGUAGE sql SET kingres_ledger.allow_manual_write = on AS 'SELECT 1';
SELECT count(*) AS blocks_changed FROM kingres.ledger_blocks WHERE height >= 900;

--------------------------------------------------------------------------
-- L10-03: what would stop the block builder or the capture triggers
--------------------------------------------------------------------------
-- The builder takes no table lock of its own; the capture triggers take ROW
-- EXCLUSIVE on the queue and the entries.  A lock held by another role that
-- conflicts with them, and a row lock on any of the nine tables, is refused to
-- the roles that could take it (pg_maintain, the members of pg_write_all_data).
DELETE FROM p_results;
CREATE FUNCTION public.p_run_locks(who text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE t record; m text; r text;
BEGIN
    FOR t IN SELECT tbl FROM p_tables ORDER BY ord LOOP
        FOREACH m IN ARRAY ARRAY['ACCESS SHARE', 'ROW SHARE', 'ROW EXCLUSIVE', 'SHARE UPDATE EXCLUSIVE', 'SHARE',
                                 'SHARE ROW EXCLUSIVE', 'EXCLUSIVE', 'ACCESS EXCLUSIVE'] LOOP
            PERFORM public.p_try(who, 'lock ' || m, format('LOCK TABLE kingres.%I IN %s MODE', t.tbl, m));
        END LOOP;
        FOREACH r IN ARRAY ARRAY['FOR UPDATE', 'FOR NO KEY UPDATE', 'FOR SHARE', 'FOR KEY SHARE'] LOOP
            PERFORM public.p_try(who, 'select ' || r, format('SELECT 1 FROM kingres.%I %s', t.tbl, r));
        END LOOP;
    END LOOP;
END $$;
GRANT EXECUTE ON FUNCTION public.p_run_locks(text) TO PUBLIC;
\c - kl_p_maint
SET search_path = public;
BEGIN; SELECT public.p_run_locks('pg_maintain'); COMMIT;
\c - kl_p_member
SET search_path = public;
BEGIN; SELECT public.p_run_locks('pg_write_all_data'); COMMIT;
\c - :owner_name
SET search_path = public;
SELECT who, form, outcome, count(*) AS tables FROM p_results GROUP BY who, form, outcome
 ORDER BY who, CASE WHEN form LIKE 'lock%' THEN 1 ELSE 2 END, form, outcome;
-- CLUSTER and REINDEX hold a lock that conflicts with the writes until the
-- transaction ends: as a statement of their own they are allowed.
\c - kl_p_maint
SET search_path = public;
REINDEX TABLE kingres.ledger_blocks;
BEGIN;
REINDEX TABLE kingres.ledger_blocks;
ROLLBACK;
BEGIN;
CLUSTER kingres.ledger_blocks USING ledger_blocks_pkey;
ROLLBACK;
CREATE FUNCTION public.p_reindex_in_function() RETURNS void LANGUAGE plpgsql AS $$ BEGIN REINDEX TABLE kingres.ledger_pending; END $$;
DO $$ BEGIN PERFORM public.p_reindex_in_function(); EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'refused: %', SQLSTATE; END $$;
REINDEX TABLE CONCURRENTLY kingres.ledger_pending;
-- The capture triggers work for every ordinary role meanwhile.
\c - kl_p_plain
SET search_path = public;
INSERT INTO docs VALUES (50, 'a plain role writes');
\c - :owner_name
SET search_path = public;
SELECT kingres.ledger_build_block() IS NOT NULL AS built;

--------------------------------------------------------------------------
-- L10-13: the marker is lowered at every way out
--------------------------------------------------------------------------
-- After the capture trigger, the builder, the digest, the recorded
-- verification and ledger_enable, in the same transaction, a write is refused.
CREATE FUNCTION public.p_blk() RETURNS text LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        INSERT INTO kingres.ledger_blocks (height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at, block_hash)
        VALUES (901, 1, sha256('a'), sha256('b'), 1, now(), sha256('c'));
        RAISE EXCEPTION 'undo' USING ERRCODE = 'U0001';
    EXCEPTION
        WHEN SQLSTATE 'U0001' THEN RETURN 'WRITTEN';
        WHEN OTHERS THEN RETURN left(SQLERRM, 40);
    END;
END $$;
INSERT INTO docs VALUES (59, 'pending');
BEGIN;
INSERT INTO docs VALUES (60, 'x');
SELECT public.p_blk() AS after_the_capture_trigger;
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT public.p_blk() AS after_the_builder;
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest;
SELECT public.p_blk() AS after_the_digest;
-- The extension refuses a call that would take a lock that comes earlier in
-- the order of the locks than one the transaction holds (L17-04): the builders'
-- lock, the registry's lock, the verifications' lock.
CREATE TABLE p_new (id int);
SELECT kingres.ledger_enable('p_new');
SELECT public.p_blk() AS after_ledger_enable;
INSERT INTO p_new VALUES (1);
SELECT public.p_blk() AS after_a_row_of_the_new_table;
SELECT kingres.ledger_verify_recorded() IS NOT NULL AS recorded;
SELECT public.p_blk() AS after_the_recorded_verification;
COMMIT;
-- After a statement of the extension that failed, in a subtransaction that was
-- rolled back, and in an exception block of a function: a transaction row that
-- exists already makes the builder's own insert fail (unique violation), after
-- the marker was raised.
SELECT txid_current() AS xid \gset
INSERT INTO docs VALUES (70, 'x');
SELECT ledger_txid AS pend FROM kingres.ledger_pending ORDER BY ledger_txid DESC LIMIT 1 \gset
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_tx (ledger_txid, block_height, tx_index, entry_count, merkle_root, user_name)
VALUES (:pend, 99999, 0, 1, sha256('x'), 'x');
RESET kingres_ledger.allow_manual_write;
BEGIN;
SAVEPOINT s1;
SELECT kingres.ledger_build_block();
ROLLBACK TO s1;
SELECT public.p_blk() AS after_a_failed_build_in_a_subtransaction;
SELECT kingres.ledger_build_block();
ROLLBACK TO s1;
CREATE FUNCTION public.p_nested() RETURNS text LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        PERFORM kingres.ledger_build_block();
    EXCEPTION WHEN OTHERS THEN
        NULL;
    END;
    RETURN public.p_blk();
END $$;
SELECT public.p_nested() AS after_a_failed_build_in_an_exception_block;
COMMIT;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_tx WHERE block_height = 99999;
RESET kingres_ledger.allow_manual_write;
SELECT kingres.ledger_build_block() IS NOT NULL AS built_after_the_cleanup;
-- The same for the capture trigger: the entry that it writes exists already.
BEGIN;
SELECT txid_current() AS xid \gset
SET LOCAL kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_entry (ledger_txid, seq, table_id, op, row_hash)
VALUES (:xid, 0, (SELECT table_id FROM kingres.ledger_table WHERE relid = 'docs'::regclass), 'I', sha256('x'));
SET LOCAL kingres_ledger.allow_manual_write = off;
SAVEPOINT s2;
INSERT INTO docs VALUES (80, 'x');
ROLLBACK TO s2;
SELECT public.p_blk() AS after_a_failed_capture_in_a_subtransaction;
ROLLBACK;

--------------------------------------------------------------------------
-- L10-02: what a write by hand leaves
--------------------------------------------------------------------------
-- A record in kingres.ledger_removed (action inserted), which ledger_verify
-- reports for as long as it exists, and which is rolled back with the write.
SELECT count(*) AS records_before FROM kingres.ledger_removed WHERE action = 'inserted' \gset
BEGIN;
SET LOCAL kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
SELECT now(), 'x', height, block_hash, '{}' FROM kingres.ledger_blocks ORDER BY height DESC LIMIT 1;
SELECT count(*) - :records_before AS records_in_the_transaction FROM kingres.ledger_removed WHERE action = 'inserted';
ROLLBACK;
SELECT count(*) - :records_before AS records_after_rollback FROM kingres.ledger_removed WHERE action = 'inserted';
SELECT count(*) AS removed_rows_with_no_record FROM kingres.ledger_removed WHERE reason = '';

-- The clauses of the digest check: a digest document that differs from its row
-- in only one of the three things the check compares.
SELECT height AS dh, block_hash AS dbh FROM kingres.ledger_blocks ORDER BY height DESC LIMIT 1 \gset
SELECT encode(chain_id, 'hex') AS dchain FROM kingres.ledger_chain \gset
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest) VALUES
 (now(), 'x', :dh, :'dbh', jsonb_build_object('height', :dh + 1, 'block_hash', encode(:'dbh', 'hex'), 'chain_id', :'dchain')),
 (now(), 'x', :dh, :'dbh', jsonb_build_object('height', :dh, 'block_hash', encode(sha256('other'), 'hex'), 'chain_id', :'dchain')),
 (now(), 'x', :dh, :'dbh', jsonb_build_object('height', :dh, 'block_hash', encode(:'dbh', 'hex'), 'chain_id', encode(sha256('other'), 'hex'))),
 (now(), 'x', :dh, :'dbh', jsonb_build_object('height', :dh, 'block_hash', encode(:'dbh', 'hex'), 'chain_id', :'dchain'));
RESET kingres_ledger.allow_manual_write;
SELECT d.digest ->> 'height' = d.height::text AS height_ok, d.digest ->> 'block_hash' = encode(d.block_hash, 'hex') AS hash_ok,
       d.digest ->> 'chain_id' = :'dchain' AS chain_ok, v.object IS NOT NULL AS reported
  FROM kingres.ledger_digest d
  LEFT JOIN (SELECT object FROM kingres.ledger_verify() WHERE check_name = 'digest_json') v ON v.object = 'digest ' || d.digest_id
 WHERE d.generated_by = 'x' ORDER BY d.digest_id;
SELECT check_name, object, detail FROM kingres.ledger_verify() WHERE check_name = 'manual_write' ORDER BY object;

-- With the setting on, a superuser deletes the rows that ledger_remove_stray
-- removes, and nothing else.
DELETE FROM p_results;
CREATE FUNCTION public.p_run_deletes() RETURNS void LANGUAGE plpgsql AS $$
DECLARE t record;
BEGIN
    FOR t IN SELECT tbl FROM p_tables ORDER BY ord LOOP
        PERFORM public.p_try('superuser, setting on', 'delete all of ' || t.tbl, format('DELETE FROM kingres.%I', t.tbl));
    END LOOP;
END $$;
SET kingres_ledger.allow_manual_write = on;
SELECT public.p_run_deletes();
RESET kingres_ledger.allow_manual_write;
SELECT form, outcome FROM p_results ORDER BY form;
DELETE FROM p_results;

--------------------------------------------------------------------------
-- L10-13: the units and the boundary of the clock limit
--------------------------------------------------------------------------
-- A valid block closed some minutes ahead, written by hand.
CREATE FUNCTION public.p_ahead(m numeric) RETURNS void LANGUAGE plpgsql AS $$
DECLARE h bigint; p bytea; t timestamptz := clock_timestamp() + m * interval '1 minute';
BEGIN
    SELECT b.height + 1, b.block_hash INTO h, p FROM kingres.ledger_blocks b ORDER BY b.height DESC LIMIT 1;
    INSERT INTO kingres.ledger_blocks VALUES (h, 1, p, sha256(''), 0, t,
        kingres.ledger_block_hash(1::smallint, h, p, sha256(''), 0, t));
END $$;
CREATE FUNCTION public.p_state(stmt text) RETURNS text LANGUAGE plpgsql AS $$
BEGIN EXECUTE stmt; RETURN 'ok'; EXCEPTION WHEN OTHERS THEN RETURN SQLSTATE; END $$;
SET kingres_ledger.max_clock_skew = '5min';
SET kingres_ledger.allow_manual_write = on;
SELECT public.p_ahead(4) \gset
RESET kingres_ledger.allow_manual_write;
-- 4 minutes ahead, limit 5 minutes: no finding for the time, the builder builds.
SELECT count(*) AS findings_for_the_time FROM kingres.ledger_verify() WHERE detail LIKE 'closed_at is later than%';
INSERT INTO docs VALUES (90, 'x');
SELECT kingres.ledger_build_block() IS NOT NULL AS built_4_minutes_behind;
SET kingres_ledger.allow_manual_write = on;
SELECT public.p_ahead(8) \gset
RESET kingres_ledger.allow_manual_write;
-- 8 minutes ahead, limit 5 minutes: reported, and the builder refuses (KL004).
SELECT count(*) AS findings_for_the_time FROM kingres.ledger_verify() WHERE detail LIKE 'closed_at is later than%';
INSERT INTO docs VALUES (91, 'x');
SELECT public.p_state('SELECT kingres.ledger_build_block()') AS build_8_minutes_behind;
SET kingres_ledger.max_clock_skew = '30min';
SELECT count(*) AS findings_for_the_time FROM kingres.ledger_verify() WHERE detail LIKE 'closed_at is later than%';
SELECT kingres.ledger_build_block() IS NOT NULL AS built_with_the_larger_limit;
RESET kingres_ledger.max_clock_skew;

\c :orig_db
SET search_path = public;
DROP DATABASE kl_paths_t;
REVOKE SET ON PARAMETER kingres_ledger.allow_manual_write FROM kl_p_gset;
DROP ROLE kl_p_inh;
DROP ROLE kl_p_maint;
DROP ROLE kl_p_member;
DROP ROLE kl_p_gset;
DROP ROLE kl_p_plain;
