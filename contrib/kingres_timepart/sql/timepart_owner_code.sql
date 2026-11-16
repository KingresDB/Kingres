-- SPDX-License-Identifier: AGPL-3.0-only
-- timepart_owner_code: where the code of a table owner runs when the
-- maintenance executes DDL on the owner's table (K12T-02).
--
-- DDL makes PostgreSQL run code that the owner wrote.  Run in the session of a
-- superuser, that code can leave things behind that outlive the action and run
-- later with the superuser's rights.  There is no complete way to clean up
-- from SQL, so the superuser's session does not run the code of an owner that
-- is not a superuser unless the driver acknowledges that with
-- kingres_timepart.trust_table_owners = on; the owner drives its own table
-- from its own session instead.  This test shows each: the refusal, what the
-- acknowledgement gives away (so the README can say so, and a change that
-- starts to clean up some of it does not go unnoticed), and the owner's own
-- session.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

SET client_min_messages = warning;
DROP ROLE IF EXISTS tp_c_owner, tp_c_super;
RESET client_min_messages;
CREATE ROLE tp_c_owner LOGIN;
CREATE ROLE tp_c_super LOGIN SUPERUSER;
CREATE SCHEMA tpc AUTHORIZATION tp_c_owner;
GRANT USAGE ON SCHEMA kingres TO tp_c_owner;
GRANT EXECUTE ON FUNCTION
    kingres.timepart_set_policy(regclass, interval, interval, interval, text, interval, text[], jsonb, text, interval, boolean)
  TO tp_c_owner;
SELECT current_user AS su \gset
CREATE TABLE tpc.log (n serial, what text, cu name, su name);
GRANT ALL ON tpc.log TO tp_c_owner;
GRANT ALL ON SEQUENCE tpc.log_n_seq TO tp_c_owner;
CREATE TABLE tpc.arm (armed boolean);
ALTER TABLE tpc.arm OWNER TO tp_c_owner;
CREATE TABLE tpc.dtab (i int);
ALTER TABLE tpc.dtab OWNER TO tp_c_owner;

-- The code of the owner: it records who it is, and one function, once armed,
-- leaves every kind of thing behind that a session can hold.
SET ROLE tp_c_owner;
CREATE FUNCTION tpc.leak(p text) RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
BEGIN
    INSERT INTO tpc.log (what, cu, su) VALUES (p, current_user, session_user);
    RETURN true;
END $$;
CREATE FUNCTION tpc.dtrig() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM tpc.leak('deferred trigger'); RETURN NULL; END $$;
CREATE CONSTRAINT TRIGGER d AFTER INSERT ON tpc.dtab
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION tpc.dtrig();
CREATE FUNCTION tpc.leave() RETURNS void LANGUAGE plpgsql VOLATILE AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM tpc.arm) OR to_regclass('pg_temp.pg_roles') IS NOT NULL THEN
        RETURN;
    END IF;
    BEGIN
        -- a view of the name of a catalog view, in the schema that is searched first
        EXECUTE 'CREATE VIEW pg_temp.pg_roles AS SELECT * FROM pg_catalog.pg_roles WHERE tpc.leak(''shadow view'')';
    EXCEPTION WHEN OTHERS THEN
        RETURN;                 -- inside an operation where PostgreSQL refuses: try again later
    END;
    PERFORM set_config('statement_timeout', '1234', false);
    EXECUTE 'PREPARE tpc_stmt AS SELECT 1';
    EXECUTE 'DECLARE tpc_cur CURSOR WITH HOLD FOR SELECT tpc.leak(''hold cursor'')';
    PERFORM pg_advisory_lock(7424);
    EXECUTE 'LISTEN tpc_chan';
    EXECUTE 'INSERT INTO tpc.dtab VALUES (1)';
END $$;
CREATE FUNCTION tpc.f_leave(x int) RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN PERFORM tpc.leave(); RETURN x; END $$;
CREATE FUNCTION tpc.f_spy(x int) RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN PERFORM tpc.leak('index expression'); RETURN x; END $$;
CREATE TABLE tpc.t1 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE INDEX ON tpc.t1 (tpc.f_spy(5));
CREATE TABLE tpc.t2 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE INDEX ON tpc.t2 (tpc.f_leave(5));
CREATE TABLE tpc.t4 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE INDEX ON tpc.t4 (tpc.f_spy(5));
SELECT kingres.timepart_set_policy(t::regclass, '1 day', create_ahead => '1 day')
  FROM unnest(ARRAY['tpc.t1', 'tpc.t2', 'tpc.t4']) t;
RESET ROLE;
-- a table of a superuser, whose code is trusted as the session's own
CREATE TABLE tpc.t3 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tpc.t3 OWNER TO tp_c_super;
SELECT kingres.timepart_set_policy('tpc.t3', '1 day', create_ahead => '1 day');
TRUNCATE tpc.log;

-- ---- the superuser's session does not run the code of an owner ----
-- with the setting off, with a value that is not a boolean, and with none (the
-- empty value is what a session has when nobody has set it)
\o /dev/null
SET kingres_timepart.trust_table_owners = off;
CALL kingres.timepart_run('tpc.t1', '2026-05-20 UTC');
SET kingres_timepart.trust_table_owners = 'maybe';
CALL kingres.timepart_run('tpc.t1', '2026-05-20 UTC');
SET kingres_timepart.trust_table_owners = '';
CALL kingres.timepart_run('tpc.t1', '2026-05-20 UTC');
\o
SELECT r.state, left(r.error, 100) AS error FROM kingres.timepart_run r WHERE schema_name = 'tpc' AND table_name = 't1' ORDER BY run_id;
SELECT count(*) AS actions FROM kingres.timepart_run_action a JOIN kingres.timepart_run r USING (run_id) WHERE r.schema_name = 'tpc' AND r.table_name = 't1';
SELECT count(*) AS statements_of_the_owner FROM tpc.log;
SELECT count(*) AS partitions FROM pg_inherits WHERE inhparent = 'tpc.t1'::regclass;
-- the helper itself refuses as well
SELECT kingres.timepart__exec_as('tp_c_owner'::regrole::oid, ARRAY['SELECT 1']);
-- a table of a superuser is run, as its owner
\o /dev/null
CALL kingres.timepart_run('tpc.t3', '2026-05-20 UTC');
\o
SELECT r.state FROM kingres.timepart_run r WHERE schema_name = 'tpc' AND table_name = 't3';
SELECT DISTINCT pg_get_userbyid(c.relowner) AS partition_owner
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'tpc.t3'::regclass;

-- ---- what the acknowledgement gives away ----
-- The session of a superuser that trusts its owners runs the code of the owner
-- and has everything the code left, afterwards.
SET kingres_timepart.trust_table_owners = on;
INSERT INTO tpc.arm VALUES (true);
\o /dev/null
CALL kingres.timepart_run('tpc.t2', '2026-05-20 UTC');
\o
SELECT r.state FROM kingres.timepart_run r WHERE schema_name = 'tpc' AND table_name = 't2';
-- an object in the temporary schema, which is searched first for relations
SELECT relname, pg_get_userbyid(relowner) AS owner FROM pg_class WHERE relnamespace = pg_my_temp_schema() AND relkind = 'v';
-- a setting of the session
SHOW statement_timeout;
RESET statement_timeout;
-- a prepared statement, a cursor that outlives the transaction, a lock of the
-- session, a channel that is listened to
SELECT name FROM pg_prepared_statements;
SELECT name, is_holdable FROM pg_cursors;
SELECT locktype, objid FROM pg_locks WHERE pid = pg_backend_pid() AND locktype = 'advisory';
SELECT pg_listening_channels();
-- the cursor ran the owner's function at commit with the rights of the
-- superuser, and so does every query that uses the view; a deferred trigger
-- ran as the owner
SELECT count(*) > 0 AS the_view_answered FROM pg_roles WHERE true;
SELECT what, cu = :'su' AS as_the_superuser, count(*) > 0 AS ran
  FROM tpc.log WHERE what <> 'index expression' GROUP BY what, cu = :'su' ORDER BY 1, 2;
-- A function of the name that the frame once used, left in the temporary
-- schema, is in nobody's way: the frame has a name of its own every time.
CREATE FUNCTION pg_temp.timepart_exec(p text[]) RETURNS void LANGUAGE sql AS 'SELECT 1';
SELECT kingres.timepart_set_policy('tpc.t3', '1 day', create_ahead => '3 days');
\o /dev/null
CALL kingres.timepart_run('tpc.t4', '2026-05-21 UTC');
\o
SELECT r.state, r.error FROM kingres.timepart_run r WHERE schema_name = 'tpc' AND table_name = 't4' ORDER BY run_id DESC LIMIT 1;
DROP FUNCTION pg_temp.timepart_exec(text[]);
-- the cleanup that the session needs
DEALLOCATE tpc_stmt;
CLOSE tpc_cur;
SELECT pg_advisory_unlock_all();
UNLISTEN *;
DROP VIEW pg_temp.pg_roles;
SELECT count(*) AS left_in_the_temporary_schema FROM pg_class WHERE relnamespace = pg_my_temp_schema() AND relkind <> 'i';
TRUNCATE tpc.log;
DELETE FROM tpc.arm;
RESET kingres_timepart.trust_table_owners;

-- ---- the owner drives its own table, from its own session ----
-- Its code runs there, as it, with nothing but its own rights; what it leaves
-- is in its own session.  The superuser's session is not involved.
SELECT kingres.timepart_grant_driver('tp_c_owner');
SELECT kingres.timepart_set_policy('tpc.t4', '1 day', create_ahead => '2 days');
\c - tp_c_owner
SELECT current_user = session_user AS plain_session,
       (SELECT rolsuper FROM pg_roles WHERE rolname = current_user) AS superuser;
SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
SELECT schema_name, table_name FROM kingres.timepart_due('2030-01-01 UTC') ORDER BY 2;
\o /dev/null
CALL kingres.timepart_run('tpc.t4', '2026-05-22 UTC');
\o
SELECT r.state, r.trigger, r.started_by FROM kingres.timepart_run r WHERE schema_name = 'tpc' AND table_name = 't4' ORDER BY run_id DESC LIMIT 1;
SELECT count(*) AS partitions FROM pg_inherits WHERE inhparent = 'tpc.t4'::regclass;
SELECT what, cu, su, count(*) > 0 AS ran FROM tpc.log GROUP BY what, cu, su ORDER BY 1;
-- the same steps one by one, as a driver of the owner would
SELECT kingres.timepart_begin_run('tpc.t4', '2026-05-23 UTC') AS run_id \gset
SELECT kingres.timepart_run_action(:run_id, 'create');
CALL kingres.timepart_run_phase(:run_id, 'create');
CALL kingres.timepart_run_phase(:run_id, 'retire');
SELECT kingres.timepart_finish_run(:run_id);
SELECT r.state FROM kingres.timepart_run r WHERE run_id = :run_id;
-- not for tables that are not its own, and not for what is the superuser's
SELECT kingres.timepart_begin_run('tpc.t3', '2026-05-23 UTC');
CALL kingres.timepart_run('tpc.t3', '2026-05-23 UTC');
SELECT kingres.timepart_prune_runs('tpc', 't4', '2026-05-23 UTC');
SELECT kingres.timepart_adopt();
SELECT kingres.timepart_grant_driver('tp_c_owner');
SELECT kingres.timepart__exec_as('tp_c_owner'::regrole::oid, ARRAY['SELECT 1']);
\c - :su
SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

-- ---- the owner is a superuser: nothing is to be protected from it ----
-- (done above for t3) and a superuser that drives a table as itself needs no
-- frame: it is the owner
ALTER TABLE tpc.t1 OWNER TO :"su";
SELECT kingres.timepart_set_policy('tpc.t1', '1 day', create_ahead => '1 day');
\o /dev/null
CALL kingres.timepart_run('tpc.t1', '2026-05-24 UTC');
\o
SELECT r.state FROM kingres.timepart_run r WHERE schema_name = 'tpc' AND table_name = 't1' ORDER BY run_id DESC LIMIT 1;

-- Cleanup.
SELECT kingres.timepart_revoke_driver('tp_c_owner');
DROP SCHEMA tpc CASCADE;
DELETE FROM kingres.timepart_run_data WHERE schema_name = 'tpc';
DROP OWNED BY tp_c_owner, tp_c_super;
DROP ROLE tp_c_owner, tp_c_super;
