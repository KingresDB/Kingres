-- SPDX-License-Identifier: AGPL-3.0-only
-- timepart_privilege: code that a table owner supplies (an index expression,
-- a default, a check constraint, a generated column, a trigger) while a
-- superuser runs the maintenance of the owner's policy.
--
-- DDL on a table makes PostgreSQL run code of the owner (an index expression
-- is evaluated when a partition is attached).  A run executes the DDL as the
-- owner, but a role switched to with SET ROLE in a superuser's session can
-- return to the superuser with RESET ROLE, or with set_config('role', ...)
-- from a function that is declared IMMUTABLE and calls a VOLATILE one.  A run
-- must execute the DDL where that is refused, and the code of the owner must
-- see the owner as current_user in every statement.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

SET client_min_messages = warning;
DROP ROLE IF EXISTS tp_p_mallory;
RESET client_min_messages;
CREATE ROLE tp_p_mallory NOLOGIN;
CREATE SCHEMA tpp AUTHORIZATION tp_p_mallory;
GRANT USAGE ON SCHEMA kingres TO tp_p_mallory;
GRANT EXECUTE ON FUNCTION
    kingres.timepart_set_policy(regclass, interval, interval, interval, text, interval, text[], jsonb, text, interval, boolean)
  TO tp_p_mallory;
CREATE TABLE tpp.log (n serial, what text, cu name, su name, is_super boolean, retried text);
GRANT ALL ON tpp.log TO tp_p_mallory;
GRANT ALL ON SEQUENCE tpp.log_n_seq TO tp_p_mallory;
GRANT CREATE ON SCHEMA tpp TO tp_p_mallory;

SET ROLE tp_p_mallory;
-- What the code of the owner does when it runs: it records who it is and tries
-- to become the session user by every means PostgreSQL has.
CREATE FUNCTION tpp.spy(p_what text) RETURNS void LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    v_retried text;
BEGIN
    INSERT INTO tpp.log (what, cu, su, is_super)
    SELECT p_what, current_user, session_user,
           (SELECT r.rolsuper FROM pg_roles r WHERE r.rolname = current_user);
    BEGIN
        RESET ROLE;
        v_retried := 'reset role worked';
    EXCEPTION WHEN OTHERS THEN
        v_retried := 'reset role refused: ' || sqlerrm;
    END;
    BEGIN
        PERFORM set_config('role', 'none', false);
        v_retried := v_retried || '; set_config role worked';
    EXCEPTION WHEN OTHERS THEN
        v_retried := v_retried || '; set_config role refused';
    END;
    BEGIN
        EXECUTE format('SET SESSION AUTHORIZATION %I', session_user);
        v_retried := v_retried || '; session authorization worked';
    EXCEPTION WHEN OTHERS THEN
        v_retried := v_retried || '; session authorization refused';
    END;
    INSERT INTO tpp.log (what, cu, su, is_super, retried)
    SELECT p_what || ' afterwards', current_user, session_user,
           (SELECT r.rolsuper FROM pg_roles r WHERE r.rolname = current_user),
           v_retried;
END $$;
-- The code that PostgreSQL runs while it makes and attaches partitions:
-- constant-argument calls in index expressions (evaluated when an index is
-- looked at again after the attach), a default, a check constraint, a stored
-- generated column and a trigger.
CREATE FUNCTION tpp.f_index(x int) RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN PERFORM tpp.spy('index expression'); RETURN x; END $$;
CREATE FUNCTION tpp.f_check(x timestamptz) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN PERFORM tpp.spy('check constraint'); RETURN true; END $$;
CREATE FUNCTION tpp.f_default() RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN PERFORM tpp.spy('default'); RETURN 1; END $$;
CREATE FUNCTION tpp.f_gen(x int) RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN PERFORM tpp.spy('generated column'); RETURN x + 1; END $$;
CREATE FUNCTION tpp.f_trigger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM tpp.spy('trigger'); RETURN NEW; END $$;

CREATE TABLE tpp.t (ts timestamptz NOT NULL, v int DEFAULT tpp.f_default(), g int GENERATED ALWAYS AS (tpp.f_gen(v)) STORED)
  PARTITION BY RANGE (ts);
CREATE INDEX ON tpp.t (tpp.f_index(5));
CREATE INDEX ON tpp.t (ts, tpp.f_index(6));
ALTER TABLE tpp.t ADD CONSTRAINT t_check CHECK (tpp.f_check(ts));
CREATE TRIGGER t_trg BEFORE INSERT ON tpp.t FOR EACH ROW EXECUTE FUNCTION tpp.f_trigger();
CREATE TRIGGER t_trg_stmt BEFORE INSERT ON tpp.t FOR EACH STATEMENT EXECUTE FUNCTION tpp.f_trigger();
SELECT kingres.timepart_set_policy('tpp.t', '1 day', create_ahead => '3 days', retention => '3 days');
RESET ROLE;
TRUNCATE tpp.log;

-- A superuser runs the maintenance: partitions are created and attached, then
-- the old ones are detached and dropped.
\o /dev/null
CALL kingres.timepart_run('tpp.t', '2026-03-10 UTC');
CALL kingres.timepart_run('tpp.t', '2026-03-16 UTC');
\o
SELECT state, error FROM kingres.timepart_run WHERE table_name = 't' ORDER BY run_id;
SELECT a.kind, a.state, count(*) AS n FROM kingres.timepart_run_action a JOIN kingres.timepart_run r USING (run_id)
 WHERE r.table_name = 't' GROUP BY a.kind, a.state ORDER BY 1, 2;

-- Every statement of the owner's code, whatever PostgreSQL ran it for, ran as
-- the owner and never as the superuser, and none could change that.
SELECT what, cu, is_super, retried, count(*) > 0 AS ran
  FROM tpp.log GROUP BY what, cu, is_super, retried ORDER BY 1, 2, 3, 4;
SELECT count(*) FILTER (WHERE is_super) AS statements_as_a_superuser,
       count(*) FILTER (WHERE cu <> 'tp_p_mallory') AS statements_as_another_role,
       count(*) FILTER (WHERE retried LIKE '%worked%') AS escapes
  FROM tpp.log;
-- the partitions are the owner's
SELECT DISTINCT pg_get_userbyid(c.relowner) AS partition_owner
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'tpp.t'::regclass;
-- nothing is left behind in the temporary schema, and no role owns anything
-- of the extension
SELECT count(*) AS temp_functions_left FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname LIKE 'pg\_temp\_%' AND p.proname LIKE 'timepart_exec%';
SELECT count(*) AS functions_owned_by_the_owner FROM pg_proc
 WHERE pronamespace = 'kingres'::regnamespace AND proowner = 'tp_p_mallory'::regrole;

-- A statement that fails inside the helper (the owner has no CREATE on the
-- schema of the table) is an action that failed, and the function that was
-- made for it goes with the subtransaction.
CREATE SCHEMA tpq;
GRANT USAGE ON SCHEMA tpq TO tp_p_mallory;
CREATE TABLE tpq.t (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tpq.t OWNER TO tp_p_mallory;
SET ROLE tp_p_mallory;
SELECT kingres.timepart_set_policy('tpq.t', '1 day', create_ahead => '1 day');
RESET ROLE;
\o /dev/null
CALL kingres.timepart_run('tpq.t', '2026-03-10 UTC');
\o
SELECT r.state, a.kind, a.state, left(a.detail, 60) AS detail
  FROM kingres.timepart_run r JOIN kingres.timepart_run_action a USING (run_id) WHERE r.table_name = 't' AND r.schema_name = 'tpq';
SELECT count(*) AS temp_functions_left FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname LIKE 'pg\_temp\_%' AND p.proname LIKE 'timepart_exec%';
DROP TABLE tpq.t;
DROP SCHEMA tpq;

-- A call of the helper by hand, for a role that does not exist and for one
-- that does, is refused or runs as that role.
SELECT kingres.timepart__exec_as(0, ARRAY['SELECT 1']);
CREATE TABLE tpp.who (r name, s name);
GRANT ALL ON tpp.who TO tp_p_mallory;
SELECT kingres.timepart__exec_as('tp_p_mallory'::regrole::oid,
       ARRAY['INSERT INTO tpp.who SELECT current_user, session_user', 'RESET ROLE']);
SELECT kingres.timepart__exec_as('tp_p_mallory'::regrole::oid,
       ARRAY['INSERT INTO tpp.who SELECT current_user, session_user']);
SELECT r AS current_user_in_the_helper, s = session_user AS session_user_unchanged FROM tpp.who;

-- Cleanup.
DROP TABLE tpp.t;
DROP SCHEMA tpp CASCADE;
DROP OWNED BY tp_p_mallory;
DROP ROLE tp_p_mallory;
