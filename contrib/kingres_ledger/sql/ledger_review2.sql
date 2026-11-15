-- ledger_review2: regression tests for the findings L2-01 to L2-05 and L2-08
-- of the independent review kingres/docs/reviews/2026-11-14-kingres-ledger-0.2.md
-- that a single server can show.  Every block states the finding it
-- reproduces.  (L2-03 and L2-06 need workers and are in t/010_review2.pl,
-- L2-07 is in kingres/verifier.)  The test is self-contained: it makes its own
-- roles and tables, and it needs the setting of ledger_setup (workers off).
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
SET search_path = public;
\set VERBOSITY terse
SET client_min_messages = warning;
CREATE EXTENSION IF NOT EXISTS kingres_ledger;
RESET client_min_messages;

DROP ROLE IF EXISTS kl2_alice;
DROP ROLE IF EXISTS kl2_bob;
DROP ROLE IF EXISTS kl2_carol;
DROP ROLE IF EXISTS kl2_dflt;
DROP ROLE IF EXISTS kl2_auditor;
CREATE ROLE kl2_alice;
CREATE ROLE kl2_bob;
CREATE ROLE kl2_carol;
CREATE ROLE kl2_dflt;
CREATE ROLE kl2_auditor;
GRANT USAGE, CREATE ON SCHEMA public TO kl2_alice, kl2_bob, kl2_carol;
GRANT EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) TO kl2_alice;

-- The report of ledger_verify with the values that change from run to run masked.
CREATE FUNCTION public.r2report() RETURNS TABLE (check_name text, object text, detail text)
LANGUAGE sql AS $$
    SELECT v.check_name, v.object,
           regexp_replace(regexp_replace(regexp_replace(v.detail, '[0-9a-f]{64}', '<hash>', 'g'),
                                         'tx [0-9]+', 'tx N', 'g'), 'seq [0-9]+', 'seq N', 'g')
    FROM kingres.ledger_verify() v
    ORDER BY 1, 2, 3
$$;

-- Who can do what on a history table, one line per grantee and privilege.
CREATE FUNCTION public.r2acl(t regclass) RETURNS TABLE (grantee text, privilege text, grantor text)
LANGUAGE sql AS $$
    SELECT CASE a.grantee WHEN 0 THEN 'PUBLIC'
                          WHEN c.relowner THEN '<history owner>'
                          ELSE a.grantee::regrole::text END,
           a.privilege_type,
           CASE a.grantor WHEN c.relowner THEN '<history owner>' ELSE a.grantor::regrole::text END
    FROM pg_class c, aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
    WHERE c.oid = t
    ORDER BY 1, 2
$$;

SET ROLE kl2_alice;
CREATE TABLE r2_acc (id int PRIMARY KEY, v text);
RESET ROLE;
SELECT kingres.ledger_enable('r2_acc', 'updatable');
INSERT INTO r2_acc VALUES (1, 'one');
SELECT count(*) AS problems_at_start FROM r2report();

--------------------------------------------------------------------------
-- L2-01: the statement that the extension runs on the history table must not
-- open a window in which other statements are not checked.  Event triggers
-- of the superuser that act on the second firing of ALTER TABLE (the first
-- is the statement of the user, the second used to be the extension's own),
-- and event triggers that act on every firing at the start and the end.
--------------------------------------------------------------------------
SELECT current_user AS extension_owner \gset
CREATE FUNCTION public.r2_evil() RETURNS event_trigger LANGUAGE plpgsql AS $$
DECLARE n text := coalesce(current_setting('r2.n', true), '');
BEGIN
  IF tg_tag = 'ALTER TABLE' THEN
    IF n = '' THEN
      PERFORM set_config('r2.n', '1', true);
    ELSIF n = '1' THEN
      PERFORM set_config('r2.n', '2', true);
      EXECUTE 'grant insert on kingres.ledger_entry to kl2_bob';
      EXECUTE 'alter table kingres.ledger_entry owner to kl2_bob';
      EXECUTE 'create or replace function kingres.ledger_hook_active() returns boolean language sql as $f$ select true $f$';
    END IF;
  END IF;
END $$;
CREATE EVENT TRIGGER aaa_r2_evil ON ddl_command_end EXECUTE FUNCTION public.r2_evil();
ALTER TABLE r2_acc ADD COLUMN extra9 text;
DROP EVENT TRIGGER aaa_r2_evil;
SELECT has_table_privilege('kl2_bob', 'kingres.ledger_entry', 'insert') AS bob_can_write_the_entries,
       (SELECT relowner::regrole::text = :'extension_owner' FROM pg_class WHERE oid = 'kingres.ledger_entry'::regclass) AS entries_still_owned_by_the_extension_owner,
       (SELECT l.lanname FROM pg_proc p JOIN pg_language l ON l.oid = p.prolang
         WHERE p.oid = 'kingres.ledger_hook_active()'::regprocedure) AS hook_active_is_still_c;

-- An event trigger that tries on every firing, at the start and at the end:
-- every attempt is a normal statement and is rejected, and nothing is
-- accepted that is not accepted outside the statement.
CREATE FUNCTION public.r2_evil_always() RETURNS event_trigger LANGUAGE plpgsql AS $$
DECLARE stmt text;
BEGIN
  IF tg_tag = 'ALTER TABLE' THEN
    FOREACH stmt IN ARRAY ARRAY[
        'grant insert on kingres.ledger_entry to kl2_bob',
        'alter table kingres.ledger_entry owner to kl2_bob',
        'create or replace function kingres.ledger_hook_active() returns boolean language sql as $f$ select true $f$',
        'alter table kingres.ledger_entry disable trigger all',
        'grant select on r2_acc_ledger_history to kl2_bob'] LOOP
      BEGIN
        EXECUTE stmt;
        RAISE NOTICE '% %: ACCEPTED %', tg_event, tg_tag, stmt;
      EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE '% %: rejected % %', tg_event, tg_tag, SQLSTATE, left(stmt, 40);
      END;
    END LOOP;
  END IF;
END $$;
CREATE EVENT TRIGGER aaa_r2_evil_start ON ddl_command_start EXECUTE FUNCTION public.r2_evil_always();
CREATE EVENT TRIGGER aaa_r2_evil_end ON ddl_command_end EXECUTE FUNCTION public.r2_evil_always();
ALTER TABLE r2_acc ADD COLUMN extra8 text;
DROP EVENT TRIGGER aaa_r2_evil_start;
DROP EVENT TRIGGER aaa_r2_evil_end;
SELECT has_table_privilege('kl2_bob', 'kingres.ledger_entry', 'insert') AS bob_can_write_the_entries,
       has_table_privilege('kl2_bob', 'r2_acc_ledger_history', 'select') AS bob_reads_the_history;
-- the history table followed both ADD COLUMNs, and the ledger verifies
SELECT string_agg(attname, ',' ORDER BY attnum) AS history_columns
FROM pg_attribute WHERE attrelid = 'r2_acc_ledger_history'::regclass AND attnum > 0 AND NOT attisdropped;
INSERT INTO r2_acc VALUES (2, 'two', 'x', 'y');
UPDATE r2_acc SET extra9 = 'nine' WHERE id = 2;
SELECT count(*) AS problems_after_add_column FROM r2report();
DROP FUNCTION public.r2_evil();
DROP FUNCTION public.r2_evil_always();

--------------------------------------------------------------------------
-- L2-02: the history table follows the owner of the table
--------------------------------------------------------------------------
SELECT * FROM r2acl('r2_acc_ledger_history');
ALTER TABLE r2_acc OWNER TO kl2_bob;
SET ROLE kl2_bob;
UPDATE r2_acc SET v = 'bob-secret-2' WHERE id = 1;
RESET ROLE;
SELECT * FROM r2acl('r2_acc_ledger_history');
SELECT has_table_privilege('kl2_alice', 'r2_acc_ledger_history', 'select') AS former_owner_reads_the_history,
       has_table_privilege('kl2_bob', 'r2_acc_ledger_history', 'select') AS new_owner_reads_the_history;
SET ROLE kl2_alice;
SELECT v FROM r2_acc_ledger_history;
RESET ROLE;
SET ROLE kl2_bob;
SELECT v FROM r2_acc_ledger_history ORDER BY ledger_txid;
RESET ROLE;
-- REASSIGN OWNED fires no event trigger
REASSIGN OWNED BY kl2_bob TO kl2_carol;
SELECT * FROM r2acl('r2_acc_ledger_history');
SELECT has_table_privilege('kl2_bob', 'r2_acc_ledger_history', 'select') AS former_owner_reads_the_history,
       has_table_privilege('kl2_carol', 'r2_acc_ledger_history', 'select') AS new_owner_reads_the_history;
-- with event triggers off the hook still moves the privileges
SET event_triggers = off;
ALTER TABLE r2_acc OWNER TO kl2_alice;
RESET event_triggers;
SELECT * FROM r2acl('r2_acc_ledger_history');
-- nobody can grant anything on the history table
GRANT SELECT ON r2_acc_ledger_history TO kl2_bob;
GRANT SELECT (v) ON r2_acc_ledger_history TO kl2_bob;
GRANT ALL ON r2_acc_ledger_history TO kl2_bob;
SELECT count(*) AS problems_after_owner_changes FROM r2report();

-- GRANT ... ON ALL TABLES IN SCHEMA cannot be told apart by the hook; the
-- event trigger takes the privileges back at the end of the statement.
GRANT SELECT ON ALL TABLES IN SCHEMA public TO kl2_bob;
SELECT * FROM r2acl('r2_acc_ledger_history');
SELECT count(*) AS problems_after_grant_all_tables FROM r2report();

-- Default privileges of the extension owner must not open a history table.
ALTER DEFAULT PRIVILEGES FOR ROLE :"extension_owner" IN SCHEMA public GRANT SELECT ON TABLES TO PUBLIC, kl2_dflt;
SET ROLE kl2_alice;
CREATE TABLE r2_dflt (id int PRIMARY KEY, v text);
RESET ROLE;
SELECT kingres.ledger_enable('r2_dflt', 'updatable');
SELECT * FROM r2acl('r2_dflt_ledger_history');
ALTER DEFAULT PRIVILEGES FOR ROLE :"extension_owner" IN SCHEMA public REVOKE SELECT ON TABLES FROM PUBLIC, kl2_dflt;
SELECT count(*) AS problems_with_default_privileges FROM r2report();

-- ledger_verify reports a history table whose privileges are not those of the
-- rule (a privilege added by a catalog edit, a privilege missing, an owner of
-- the table that cannot read it, a column privilege).
UPDATE pg_class SET relacl = relacl || makeaclitem('kl2_bob'::regrole, relowner, 'SELECT', false)
WHERE oid = 'r2_acc_ledger_history'::regclass;
SELECT * FROM r2report();
UPDATE pg_class SET relacl = NULL WHERE oid = 'r2_dflt_ledger_history'::regclass;
SELECT * FROM r2report();
UPDATE pg_attribute SET attacl = ARRAY[makeaclitem('kl2_carol'::regrole,
           (SELECT relowner FROM pg_class WHERE oid = 'r2_acc_ledger_history'::regclass), 'SELECT', false)]
WHERE attrelid = 'r2_acc_ledger_history'::regclass AND attname = 'v';
SELECT * FROM r2report();
-- any DDL command repairs them (the event trigger runs the same rule)
COMMENT ON TABLE r2_acc IS 'r2';
SELECT * FROM r2acl('r2_acc_ledger_history');
SELECT * FROM r2acl('r2_dflt_ledger_history');
SELECT count(*) AS problems_after_repair FROM r2report();

--------------------------------------------------------------------------
-- L2-04: what PUBLIC can read of the records, and of every object of 0.2
--------------------------------------------------------------------------
SELECT kingres.ledger_verify_recorded() AS vid \gset
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest_made;
SET ROLE kl2_bob;
SELECT count(*) FROM kingres.ledger_verification;
SELECT problems FROM kingres.ledger_verification;
SELECT count(*) > 0 AS public_reads_the_digests FROM kingres.ledger_digest;
SELECT count(digest_id) > 0 AS public_reads_the_columns_without_a_role_name FROM kingres.ledger_digest;
SELECT generated_by FROM kingres.ledger_digest;
SELECT count(*) FROM kingres.ledger_status;
SELECT count(*) FROM kingres.ledger_entry;
SELECT count(*) FROM kingres.ledger_pending;
SELECT count(*) FROM kingres.ledger_tx;
SELECT state FROM kingres.ledger_worker_state();
SELECT kingres.ledger_history_acl_sync('r2_acc', 'r2_acc_ledger_history');
-- L2-08: a role without EXECUTE cannot add rows to the records
SELECT kingres.ledger_generate_digest();
SELECT kingres.ledger_verify_recorded();
RESET ROLE;
-- an auditor is given exactly what the README says
GRANT SELECT ON kingres.ledger_verification TO kl2_auditor;
SET ROLE kl2_auditor;
SELECT count(*) > 0 AS auditor_reads_the_verifications FROM kingres.ledger_verification;
RESET ROLE;
-- every column of every table of the extension that PUBLIC can read
SELECT c.relname, a.attname
FROM pg_class c
JOIN pg_depend d ON d.classid = 'pg_class'::regclass AND d.objid = c.oid
                AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
JOIN pg_extension e ON e.oid = d.refobjid AND e.extname = 'kingres_ledger'
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
WHERE c.relkind IN ('r', 'v')
  AND (has_column_privilege('public', c.oid, a.attnum, 'select'))
ORDER BY 1, a.attnum;
-- the functions of the extension that PUBLIC can execute and that read or
-- write anything outside their arguments
SELECT p.proname
FROM pg_proc p
JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
                AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
WHERE has_function_privilege('public', p.oid, 'execute')
  AND p.proname IN ('ledger_history_acl_sync', 'ledger_history_sync', 'ledger_worker_state',
                    'ledger_lock_table', 'ledger_record_row', 'ledger_capture')
ORDER BY 1;

--------------------------------------------------------------------------
-- L2-05: the caches of a transaction survive a rolled back ledger_enable
--------------------------------------------------------------------------
BEGIN;
CREATE TABLE r2_spt (id int);
SAVEPOINT s1;
SELECT kingres.ledger_enable('r2_spt', 'updatable');
INSERT INTO r2_spt VALUES (1);
UPDATE r2_spt SET id = 2;
ROLLBACK TO s1;
SELECT kingres.ledger_enable('r2_spt', 'updatable');
INSERT INTO r2_spt VALUES (5);
UPDATE r2_spt SET id = 6;
DELETE FROM r2_spt;
COMMIT;
SELECT count(*) AS history_rows FROM r2_spt_ledger_history;
SELECT count(*) AS problems_after_savepoint FROM r2report();

-- The same in an exception block, with an append-only table that is enabled
-- a second time and then used, and with the order of the lookups reversed.
DO $$
BEGIN
  CREATE TABLE r2_blk (id int PRIMARY KEY, v text);
  BEGIN
    PERFORM kingres.ledger_enable('r2_blk', 'updatable');
    INSERT INTO r2_blk VALUES (1, 'a');
    UPDATE r2_blk SET v = 'b';
    RAISE EXCEPTION 'undo' USING ERRCODE = 'KL999';
  EXCEPTION WHEN SQLSTATE 'KL999' THEN
    NULL;
  END;
  PERFORM kingres.ledger_enable('r2_blk', 'updatable');
  INSERT INTO r2_blk VALUES (7, 'x');
  UPDATE r2_blk SET v = 'y' WHERE id = 7;
  DELETE FROM r2_blk;
END $$;
SELECT count(*) AS history_rows FROM r2_blk_ledger_history;
SELECT count(*) AS problems_after_exception_block FROM r2report();

-- Subtransactions that commit keep what they did.
BEGIN;
CREATE TABLE r2_keep (id int PRIMARY KEY, v text);
SAVEPOINT s1;
SELECT kingres.ledger_enable('r2_keep', 'updatable');
INSERT INTO r2_keep VALUES (1, 'a');
RELEASE SAVEPOINT s1;
UPDATE r2_keep SET v = 'b';
SAVEPOINT s2;
UPDATE r2_keep SET v = 'c';
ROLLBACK TO s2;
UPDATE r2_keep SET v = 'd';
COMMIT;
SELECT v FROM r2_keep;
SELECT count(*) AS history_rows FROM r2_keep_ledger_history;
SELECT count(*) AS problems_after_release FROM r2report();

--------------------------------------------------------------------------
-- Not examined by the review: COPY ... FREEZE into a ledger table
--------------------------------------------------------------------------
-- rows loaded frozen, then the table is put under ledger control
BEGIN;
CREATE TABLE r2_frz (id int, v text);
COPY r2_frz FROM STDIN FREEZE;
1	one
2	two
3	three
\.
SELECT kingres.ledger_enable('r2_frz', 'updatable');
COMMIT;
-- the table is under ledger control first, then loaded frozen
BEGIN;
CREATE TABLE r2_frz2 (id int, v text);
SELECT kingres.ledger_enable('r2_frz2', 'updatable');
COPY r2_frz2 FROM STDIN FREEZE;
1	one
\.
COMMIT;
SELECT count(*) AS rows_in_r2_frz2 FROM r2_frz2;
SELECT count(*) AS rows_after_copy_freeze FROM r2_frz;
UPDATE r2_frz SET v = v || '!' WHERE id = 2;
DELETE FROM r2_frz WHERE id = 3;
SELECT count(*) AS history_rows FROM r2_frz_ledger_history;
SELECT count(*) AS problems_after_copy_freeze FROM r2report();
BEGIN;
CREATE TABLE r2_frz_ao (id int, v text);
SELECT kingres.ledger_enable('r2_frz_ao');
COPY r2_frz_ao FROM STDIN FREEZE;
1	one
2	two
\.
COMMIT;
SELECT count(*) AS problems_after_copy_freeze_append_only FROM r2report();

--------------------------------------------------------------------------
-- Hostile settings of a role or a database apply to the functions that the
-- extension runs for other roles (L2-03, the part that does not need a worker):
-- every function still works.
--------------------------------------------------------------------------
SET standard_conforming_strings = off;
SET escape_string_warning = off;
SET plpgsql.extra_errors = 'all';
SET plpgsql.extra_warnings = 'all';
SET plpgsql.variable_conflict = 'use_column';
SET check_function_bodies = off;
SET transform_null_equals = on;
SET quote_all_identifiers = on;
SET DateStyle = 'SQL, DMY';
SET IntervalStyle = 'sql_standard';
SET TimeZone = 'Asia/Kolkata';
SET extra_float_digits = -5;
SET bytea_output = 'escape';
SET default_transaction_deferrable = on;
SET default_table_access_method = 'heap';
SET row_security = on;
SET search_path = pg_temp;
CREATE TABLE public.r2_hostile (id int PRIMARY KEY, v text);
SELECT kingres.ledger_enable('public.r2_hostile', 'updatable');
INSERT INTO public.r2_hostile VALUES (1, 'a');
UPDATE public.r2_hostile SET v = 'b';
COMMENT ON TABLE public.r2_hostile IS 'hostile';
ALTER TABLE public.r2_hostile ADD COLUMN w text;
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest;
SELECT count(*) AS problems_with_hostile_settings FROM kingres.ledger_verify();
SELECT kingres.ledger_verify_recorded() IS NOT NULL AS recorded;
SELECT count(*) AS history_rows FROM public.r2_hostile_ledger_history;
RESET ALL;
SET search_path = public;
SELECT count(*) AS problems_after_hostile_settings FROM r2report();
