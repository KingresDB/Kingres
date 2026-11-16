-- timepart_security: who may do what.  The functions run DDL on user tables:
-- a caller must not be able to do through them anything it could not do
-- directly.  Catalog hygiene of every function, ownership checks for a role
-- that is not the owner, a member of the owner role, a role that is
-- not granted EXECUTE, the superuser-only functions, a changed owner, an
-- owner that cannot create in the schema, and a hostile search_path.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

-- Hygiene.  Members of the extension, by kind.
CREATE TEMP VIEW members AS
SELECT p.oid, p.proname, p.prokind, p.prosecdef, p.proconfig, p.proacl, p.pronargs
  FROM pg_proc p
  JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
                  AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
  JOIN pg_extension e ON e.oid = d.refobjid AND e.extname = 'kingres_timepart';
SELECT count(*) > 30 AS functions_found FROM members;
-- Every function sets its search_path with pg_catalog first and pg_temp last,
-- except the two procedures that commit (they hold only qualified names and
-- are superuser only).
SELECT proname, prokind FROM members
 WHERE NOT (coalesce(proconfig, '{}') @> ARRAY['search_path=pg_catalog, kingres, pg_temp']
            OR coalesce(proconfig, '{}') @> ARRAY['search_path=pg_catalog, pg_temp'])
 ORDER BY proname;
-- SECURITY DEFINER is used by the functions that write the policy and
-- history tables for a caller that has no right on them, and by nothing else.
SELECT proname FROM members WHERE prosecdef ORDER BY proname;
-- No function is executable by PUBLIC except the pure helpers and the two
-- DDL steps that run with the caller's own rights.
SELECT proname FROM members
 WHERE has_function_privilege('public', oid, 'EXECUTE') ORDER BY proname;
-- The policy and the history are shown to a role through security_barrier
-- views (the rows of the tables it owns); the base tables and the lock table
-- grant nothing to a role, and no table has row-level security (pg_dump of a
-- role that is not a superuser needs that).
SELECT relname, c.relkind AS kind, c.relrowsecurity AS rls,
       coalesce(c.reloptions::text, '') AS options,
       has_table_privilege('tp_other', c.oid, 'SELECT') AS other_select,
       has_table_privilege('tp_other', c.oid, 'INSERT') AS other_insert
  FROM pg_class c WHERE c.relnamespace = 'kingres'::regnamespace AND c.relname LIKE 'timepart_%' AND c.relkind IN ('r', 'v')
 ORDER BY relname;
-- No object of the extension outside its schema.
SELECT count(*) AS outside_schema FROM pg_depend d
  JOIN pg_extension e ON e.oid = d.refobjid AND e.extname = 'kingres_timepart'
 WHERE d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
   AND d.classid = 'pg_class'::regclass
   AND (SELECT relnamespace FROM pg_class WHERE oid = d.objid) <> 'kingres'::regnamespace;
-- Every function and table of the interface has a comment.
SELECT proname FROM members m
 WHERE proname NOT LIKE 'timepart\_\_%' AND obj_description(oid, 'pg_proc') IS NULL ORDER BY 1;

-- Tables of the owner.
GRANT USAGE ON SCHEMA tpo TO tp_other, tp_member, tp_noinherit, tp_nogrant;
CREATE TABLE tpo.o1 (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tpo.o1 OWNER TO tp_owner;
GRANT SELECT ON tpo.o1 TO tp_other;
SET ROLE tp_owner;
SELECT kingres.create_time_partitions('tpo.o1', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('tpo.o1', '1 day', create_ahead => '2 days', retention => '3 days');
SELECT kind, partition_name FROM kingres.timepart_plan('tpo.o1', '2026-03-10 UTC') ORDER BY seq LIMIT 3;
SELECT count(*) AS partitions FROM kingres.timepart_partitions('tpo.o1');
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-03 UTC');
SELECT kingres.drop_old_time_partitions('tpo.o1', '2026-03-02 UTC');
-- The owner sees the rows of its own tables in the policy and history tables,
-- and nothing else.
SELECT count(*) AS visible_policies FROM kingres.timepart_policy;
SELECT count(*) AS visible_runs FROM kingres.timepart_run;
SELECT count(*) AS visible_locks FROM kingres.timepart_lock;
-- The administration functions are not for the owner, even with EXECUTE.
RESET ROLE;
GRANT EXECUTE ON FUNCTION kingres.timepart_begin_run(regclass, timestamptz, text),
                          kingres.timepart_due(timestamptz),
                          kingres.timepart_finish_run(bigint),
                          kingres.timepart_prune_runs(name, name, timestamptz) TO tp_owner;
GRANT EXECUTE ON PROCEDURE kingres.timepart_run(regclass, timestamptz, text[], bigint),
                           kingres.timepart_run_phase(bigint, text) TO tp_owner;
SET ROLE tp_owner;
SELECT kingres.timepart_begin_run('tpo.o1', '2026-03-10 UTC');
SELECT * FROM kingres.timepart_due('2100-01-01 UTC');
SELECT kingres.timepart_finish_run(1);
SELECT kingres.timepart_prune_runs('tpo', 'o1', '2026-03-10 UTC');
CALL kingres.timepart_run('tpo.o1', '2026-03-10 UTC');
CALL kingres.timepart_run_phase(1, 'create');
SELECT kingres.timepart_run_action(1, 'create');
RESET ROLE;
-- Without EXECUTE the function cannot even be called.
REVOKE EXECUTE ON FUNCTION kingres.timepart_begin_run(regclass, timestamptz, text),
                           kingres.timepart_due(timestamptz),
                           kingres.timepart_finish_run(bigint),
                           kingres.timepart_prune_runs(name, name, timestamptz) FROM tp_owner;
REVOKE EXECUTE ON PROCEDURE kingres.timepart_run(regclass, timestamptz, text[], bigint),
                            kingres.timepart_run_phase(bigint, text) FROM tp_owner;
SET ROLE tp_nogrant;
SELECT kingres.create_time_partitions('tpo.o1', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('tpo.o1', '1 day');
SELECT kingres.timepart_begin_run('tpo.o1');
RESET ROLE;

-- A role that is not the owner, with EXECUTE granted, can do none of it.
SET ROLE tp_other;
SELECT kingres.create_time_partitions('tpo.o1', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
SELECT kingres.drop_old_time_partitions('tpo.o1', '2026-03-05 UTC');
SELECT kingres.timepart_set_policy('tpo.o1', '1 day');
SELECT kingres.timepart_unset_policy('tpo.o1');
SELECT * FROM kingres.timepart_plan('tpo.o1', '2026-03-10 UTC');
-- Reading the list of partitions needs SELECT on the table, which it has here.
SELECT count(*) AS partitions FROM kingres.timepart_partitions('tpo.o1');
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-03 UTC');
RESET ROLE;
REVOKE SELECT ON tpo.o1 FROM tp_other;
SET ROLE tp_other;
SELECT count(*) AS partitions FROM kingres.timepart_partitions('tpo.o1');
RESET ROLE;
-- Nothing changed: the policy is the owner's, the partitions are the same.
SELECT table_owner, retention FROM kingres.timepart_policy WHERE schema_name = 'tpo';
SELECT count(*) AS partitions FROM tp.parts('tpo.o1');

-- A superuser session that does SET ROLE is judged as the role it set: the
-- policy functions are not a way around SET ROLE.
SET ROLE tp_other;
RESET ROLE;
SELECT current_user, session_user = current_user AS session_is_super;
SET ROLE tp_other;
SELECT kingres.timepart_set_policy('tpo.o1', '1 day');
RESET ROLE;
SET ROLE tp_owner;
SELECT kingres.timepart_set_policy('tpo.o1', '1 day', create_ahead => '2 days', retention => '3 days');
RESET ROLE;

-- A member of the owner role that inherits its privileges is the owner for
-- these functions; a member that does not inherit is not, until it does
-- SET ROLE.
SET ROLE tp_member;
SELECT kingres.timepart_set_policy('tpo.o1', '1 day', create_ahead => '2 days', retention => '3 days');
RESET ROLE;
SET ROLE tp_noinherit;
SELECT kingres.timepart_set_policy('tpo.o1', '1 day');
SELECT kingres.create_time_partitions('tpo.o1', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
SET ROLE tp_owner;
SELECT kingres.timepart_set_policy('tpo.o1', '1 day', create_ahead => '2 days', retention => '3 days');
RESET ROLE;
SELECT created_by FROM kingres.timepart_policy WHERE schema_name = 'tpo';

-- A run by the superuser executes the DDL as the owner of the table: the
-- partitions it creates are the owner's.
\o /dev/null
CALL kingres.timepart_run('tpo.o1', '2026-03-05 UTC');
\o
SELECT state FROM tp.last_run('tpo', 'o1');
SELECT c.relname, pg_get_userbyid(c.relowner) AS owner
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = 'tpo.o1'::regclass ORDER BY 1;
-- The role that is the owner can rename its table; the policy follows
-- although the role cannot touch the policy table.
SET ROLE tp_owner;
ALTER TABLE tpo.o1 RENAME TO o1b;
RESET ROLE;
SELECT schema_name, table_name FROM kingres.timepart_policy WHERE schema_name = 'tpo';
ALTER TABLE tpo.o1b RENAME TO o1;
-- Another role renaming a table of its own does not change this policy.
CREATE TABLE tp.theirs (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tp.theirs OWNER TO tp_other;
GRANT CREATE ON SCHEMA tp TO tp_other;
SET ROLE tp_other;
ALTER TABLE tp.theirs RENAME TO theirs2;
RESET ROLE;
SELECT count(*) AS policies FROM kingres.timepart_policy;
DROP TABLE tp.theirs2;

-- The owner changed after the policy was set: the run refuses to go on, and
-- says why, until someone with the right to do so sets the policy again.
ALTER TABLE tpo.o1 OWNER TO tp_other;
\o /dev/null
CALL kingres.timepart_run('tpo.o1', '2026-03-12 UTC');
\o
SELECT * FROM tp.last_run('tpo', 'o1');
SELECT count(*) AS actions FROM tp.last_actions('tpo', 'o1');
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-12 UTC') WHERE problem = 'last_run_failed' AND table_name = 'o1';
-- A new owner may set the policy again (it needs ownership); the old owner may not.
SET ROLE tp_owner;
SELECT kingres.timepart_set_policy('tpo.o1', '1 day');
RESET ROLE;
GRANT CREATE ON SCHEMA tpo TO tp_other;
SET ROLE tp_other;
SELECT kingres.timepart_set_policy('tpo.o1', '1 day', create_ahead => '2 days', retention => '3 days');
RESET ROLE;
SELECT table_owner, created_by FROM kingres.timepart_policy WHERE schema_name = 'tpo';
\o /dev/null
CALL kingres.timepart_run('tpo.o1', '2026-03-12 UTC');
\o
SELECT * FROM tp.last_run('tpo', 'o1');
-- The partitions made earlier still belong to the old owner, and the new owner
-- cannot drop them: PostgreSQL does not move them with the table.
SELECT kind, partition_name, state, detail FROM tp.last_actions('tpo', 'o1') ORDER BY kind, partition_name;
SELECT c.relname, pg_get_userbyid(c.relowner) AS owner
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = 'tpo.o1'::regclass ORDER BY 1;

-- An owner that cannot create tables in the schema: the action fails with
-- PostgreSQL's own error, and the run goes on to the next one.
CREATE TABLE tp.o2 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tp.o2 OWNER TO tp_owner;
GRANT USAGE ON SCHEMA tp TO tp_owner;
SELECT kingres.timepart_set_policy('tp.o2', '1 day', create_ahead => '1 day');
\o /dev/null
CALL kingres.timepart_run('tp.o2', '2026-03-05 UTC');
\o
SELECT * FROM tp.last_run('tp', 'o2');
SELECT * FROM tp.last_actions('tp', 'o2');
GRANT CREATE ON SCHEMA tp TO tp_owner;
\o /dev/null
CALL kingres.timepart_run('tp.o2', '2026-03-05 UTC');
\o
SELECT * FROM tp.last_run('tp', 'o2');
SELECT count(*) AS partitions FROM tp.parts('tp.o2');
REVOKE CREATE ON SCHEMA tp FROM tp_owner;

-- A hostile search_path of the caller: objects in the caller's schema with
-- the names of catalogs, functions and operators are never picked up.
CREATE TABLE tpo.pg_class (x int);
CREATE TABLE tpo.pg_inherits (x int);
CREATE FUNCTION tpo.format(text, text) RETURNS text LANGUAGE sql AS $$ SELECT 'hijacked' $$;
CREATE FUNCTION tpo.lower(text) RETURNS text LANGUAGE sql AS $$ SELECT 'hijacked' $$;
CREATE FUNCTION tpo.to_regclass(text) RETURNS regclass LANGUAGE sql AS $$ SELECT NULL::regclass $$;
CREATE FUNCTION tpo.quote_ident(text) RETURNS text LANGUAGE sql AS $$ SELECT 'hijacked' $$;
CREATE TEMP TABLE pg_namespace (x int);
CREATE TABLE tpo.h1 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tpo.h1 OWNER TO tp_owner;
GRANT CREATE ON SCHEMA tpo TO tp_owner;
SET ROLE tp_owner;
SET search_path = tpo, pg_temp, pg_catalog, public;
SELECT kingres.create_time_partitions('tpo.h1', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('tpo.h1', '1 day', create_ahead => '1 day');
SELECT kind, partition_name FROM kingres.timepart_plan('tpo.h1', '2026-03-05 UTC') ORDER BY seq LIMIT 2;
SELECT kingres.drop_old_time_partitions('tpo.h1', '2026-03-02 UTC');
RESET ROLE;
RESET search_path;
SET search_path = tpo, pg_temp, pg_catalog, public;
\o /dev/null
CALL kingres.timepart_run('tpo.h1', '2026-03-05 UTC');
\o
RESET search_path;
SELECT * FROM tp.last_run('tpo', 'h1');
SELECT part FROM tp.parts('tpo.h1');

-- The identity is current_user, not the session user.  A function of a role,
-- SECURITY DEFINER, called in a session of a superuser runs with the
-- privileges of that role only; the policy functions have to judge that role,
-- and not the superuser who logged in.
CREATE TABLE tpo.victim (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tpo.victim OWNER TO tp_owner;
SELECT kingres.create_time_partitions('tpo.victim', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
SET ROLE tp_owner;
SELECT kingres.timepart_set_policy('tpo.victim', '1 day', create_ahead => '1 day', retention => '5 days');
RESET ROLE;
GRANT USAGE, CREATE ON SCHEMA tp TO tp_other;
GRANT USAGE ON SCHEMA tpo TO tp_other;
SET ROLE tp_other;
CREATE FUNCTION tp.evil_fn() RETURNS int LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    PERFORM kingres.timepart_set_policy('tpo.victim'::regclass, '1 day', '1 day', '1 day');
    RETURN 1;
END $$;
CREATE FUNCTION tp.evil_unset() RETURNS int LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    PERFORM kingres.timepart_unset_policy('tpo.victim'::regclass);
    RETURN 1;
END $$;
CREATE FUNCTION tp.evil_plan() RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    RETURN (SELECT count(*) FROM kingres.timepart_plan('tpo.victim'::regclass, '2026-03-10 UTC'));
END $$;
SELECT tp.evil_fn();
RESET ROLE;
-- The same function in the session of a superuser (a view, a report): refused.
SELECT tp.evil_fn();
SELECT tp.evil_unset();
SELECT tp.evil_plan();
SELECT retention, created_by FROM kingres.timepart_policy WHERE table_name = 'victim';
-- The ownership check follows the role, not the login: the superuser may
-- act on the table when it acts as itself.
SELECT kingres.timepart_set_policy('tpo.victim', '1 day', create_ahead => '1 day', retention => '5 days');
DROP FUNCTION tp.evil_fn(), tp.evil_unset(), tp.evil_plan();

-- A role writes the policy table directly only for tables it owns, and the
-- columns the extension decides are pinned.
SET ROLE tp_other;
-- the owner and the creator are shown from the catalog and cannot be written
INSERT INTO kingres.timepart_policy (schema_name, table_name, parent_oid, partition_every, table_owner, created_by)
VALUES ('tpo', 'victim', 0, '1 day', 'tp_other', 'tp_other');
INSERT INTO kingres.timepart_policy (schema_name, table_name, parent_oid, partition_every)
VALUES ('tpo', 'victim', 0, '1 day');
UPDATE kingres.timepart_policy SET retention = '1 day' WHERE table_name = 'victim';
DELETE FROM kingres.timepart_policy WHERE table_name = 'victim';
SELECT count(*) AS other_sees FROM kingres.timepart_policy;
RESET ROLE;
SELECT retention FROM kingres.timepart_policy WHERE table_name = 'victim';
SET ROLE tp_owner;
UPDATE kingres.timepart_policy SET retention = '4 days', table_owner = 'tp_other', created_by = 'tp_other',
       parent_oid = 1, created_at = '2000-01-01' WHERE table_name = 'victim';
UPDATE kingres.timepart_policy SET retention = '4 days',
       parent_oid = 1, created_at = '2000-01-01' WHERE table_name = 'victim';
SELECT retention, table_owner, created_by, parent_oid = 'tpo.victim'::regclass::oid AS hint_pinned,
       created_at > '2020-01-01' AS created_at_kept FROM kingres.timepart_policy WHERE table_name = 'victim';
UPDATE kingres.timepart_policy SET retention = '1 microsecond' WHERE table_name = 'victim';
UPDATE kingres.timepart_policy SET timezone = 'Mars/Olympus' WHERE table_name = 'victim';
UPDATE kingres.timepart_policy SET schema_name = 'tp', table_name = 'canary' WHERE table_name = 'victim';
INSERT INTO kingres.timepart_policy (schema_name, table_name, parent_oid, partition_every)
VALUES ('tp', 'canary', 0, '1 day');
DELETE FROM kingres.timepart_policy WHERE table_name = 'victim';
SELECT count(*) AS policies_of_owner FROM kingres.timepart_policy;
RESET ROLE;
-- A role may also make the policy of its own table by writing the row.
SET ROLE tp_owner;
INSERT INTO kingres.timepart_policy (schema_name, table_name, parent_oid, partition_every, create_ahead)
VALUES ('tpo', 'victim', 0, '1 day', '1 day');
SELECT table_owner, created_by, retention FROM kingres.timepart_policy WHERE table_name = 'victim';
RESET ROLE;
DROP TABLE tpo.victim;

-- K12-13: the detached partitions listed are the ones the caller may read, and
-- a table that took the name of a detached partition is not described.
CREATE TABLE tpo.dt (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tpo.dt OWNER TO tp_owner;
SET ROLE tp_owner;
SELECT kingres.create_time_partitions('tpo.dt', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('tpo.dt', '1 day', create_ahead => '1 day', retention => '2 days', retention_action => 'detach');
RESET ROLE;
\o /dev/null
CALL kingres.timepart_run('tpo.dt', '2026-03-06 UTC');
\o
SET ROLE tp_owner;
SELECT partition, attached FROM kingres.timepart_partitions('tpo.dt') WHERE NOT attached ORDER BY 1;
DROP TABLE tpo.dt_p20260301;
RESET ROLE;
GRANT CREATE ON SCHEMA tpo TO tp_other;
SET ROLE tp_other;
CREATE TABLE tpo.dt_p20260301 (x int);
INSERT INTO tpo.dt_p20260301 SELECT g FROM generate_series(1, 1000) g;
RESET ROLE;
SET ROLE tp_owner;
SELECT partition, attached FROM kingres.timepart_partitions('tpo.dt') WHERE NOT attached ORDER BY 1;
RESET ROLE;
DROP TABLE tpo.dt_p20260301;
DROP TABLE tpo.dt;

-- K12R-01: the policy and the history are reached through views whose
-- condition judges current_user, and nothing else.  Two tables with a policy
-- and a run each, of two roles.
CREATE TABLE tpo.mine (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tpo.mine OWNER TO tp_owner;
CREATE TABLE tp.theirs3 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tp.theirs3 OWNER TO tp_other;
SET ROLE tp_owner;
SELECT kingres.timepart_set_policy('tpo.mine', '1 day', create_ahead => '1 day');
RESET ROLE;
SET ROLE tp_other;
SELECT kingres.timepart_set_policy('tp.theirs3', '1 day', create_ahead => '1 day');
RESET ROLE;
GRANT CREATE ON SCHEMA tpo TO tp_owner;
\o /dev/null
CALL kingres.timepart_run('tpo.mine', '2026-03-05 UTC');
CALL kingres.timepart_run('tp.theirs3', '2026-03-05 UTC');
\o
-- Each role sees its own policy, runs and actions; the superuser sees all.
SET ROLE tp_other;
SELECT table_name FROM kingres.timepart_policy WHERE table_name IN ('mine', 'theirs3') ORDER BY 1;
SELECT DISTINCT table_name FROM kingres.timepart_run WHERE table_name IN ('mine', 'theirs3') ORDER BY 1;
SELECT count(DISTINCT r.table_name) AS tables_of_actions
  FROM kingres.timepart_run_action a JOIN kingres.timepart_run r USING (run_id)
 WHERE r.table_name IN ('mine', 'theirs3');
SELECT table_name FROM kingres.timepart_policies WHERE table_name IN ('mine', 'theirs3') ORDER BY 1;
-- The base tables are not reachable.
SELECT count(*) FROM kingres.timepart_policy_data;
SELECT count(*) FROM kingres.timepart_run_data;
SELECT count(*) FROM kingres.timepart_run_action_data;
INSERT INTO kingres.timepart_run_data (schema_name, table_name, as_of, trigger, state, started_by, table_owner_oid, table_owner)
VALUES ('tp', 'theirs3', now(), 'manual', 'done', 'tp_other', 0, 'tp_other');
INSERT INTO kingres.timepart_run (schema_name, table_name, as_of, trigger, state, started_by)
VALUES ('tp', 'theirs3', now(), 'manual', 'done', 'tp_other');
DELETE FROM kingres.timepart_run;
-- A function of the caller in a WHERE clause does not see the rows the view
-- hides: it is called for the rows of the caller only.
CREATE FUNCTION tp.spy(anyelement) RETURNS boolean LANGUAGE plpgsql AS $$
BEGIN RAISE NOTICE 'spy saw %', $1; RETURN true; END $$;
SELECT count(*) AS seen FROM kingres.timepart_policy WHERE tp.spy(table_name);
SELECT count(*) AS seen FROM kingres.timepart_policy WHERE table_name = 'mine' AND tp.spy(table_name);
SELECT count(*) AS seen FROM kingres.timepart_run WHERE tp.spy(table_name);
SELECT count(*) AS seen FROM kingres.timepart_run_action a WHERE tp.spy(a.partition_name) AND a.partition_name <> '';
SELECT count(*) AS seen FROM kingres.timepart_policies WHERE tp.spy(table_name);
SELECT count(*) AS seen FROM kingres.timepart_policy p JOIN kingres.timepart_run r USING (schema_name, table_name) WHERE tp.spy(r.error);
-- Writes through the view: a policy for a table of another role, in every
-- form, and an update that would move a row to a table of another role.
INSERT INTO kingres.timepart_policy (schema_name, table_name, parent_oid, partition_every)
VALUES ('tpo', 'mine', 0, '1 day')
ON CONFLICT (schema_name, table_name) DO UPDATE SET retention = '1 day';
INSERT INTO kingres.timepart_policy (schema_name, table_name, parent_oid, partition_every)
VALUES ('tpo', 'mine', 0, '1 day')
ON CONFLICT (schema_name, table_name) DO NOTHING;
UPDATE kingres.timepart_policy SET schema_name = 'tpo', table_name = 'mine' WHERE table_name = 'theirs3';
DELETE FROM kingres.timepart_policy WHERE table_name = 'mine';
RESET ROLE;
DROP FUNCTION tp.spy(anyelement);
SELECT table_name, retention FROM kingres.timepart_policy WHERE table_name IN ('mine', 'theirs3') ORDER BY 1;
-- A SECURITY DEFINER function of the role, called in the session of a
-- superuser, sees what the role sees, and cannot write what it may not.
SET ROLE tp_other;
CREATE FUNCTION tp.evil_view() RETURNS text LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    RETURN (SELECT string_agg(table_name, ',' ORDER BY table_name) FROM kingres.timepart_policy
             WHERE table_name IN ('mine', 'theirs3'))
        || ' / ' || (SELECT count(*) FROM kingres.timepart_run WHERE table_name = 'mine')
        || ' / ' || (SELECT count(*) FROM kingres.timepart_run_action a JOIN kingres.timepart_run r USING (run_id) WHERE r.table_name = 'mine');
END $$;
CREATE FUNCTION tp.evil_write() RETURNS int LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    UPDATE kingres.timepart_policy SET retention = '1 day' WHERE table_name = 'mine';
    DELETE FROM kingres.timepart_policy WHERE table_name = 'mine';
    INSERT INTO kingres.timepart_policy (schema_name, table_name, parent_oid, partition_every, retention)
    VALUES ('tpo', 'mine', 0, '1 day', '1 day') ON CONFLICT (schema_name, table_name) DO UPDATE SET retention = '1 day';
    RETURN 1;
END $$;
RESET ROLE;
SELECT tp.evil_view();
SELECT tp.evil_write();
SELECT table_name, retention FROM kingres.timepart_policy WHERE table_name IN ('mine', 'theirs3') ORDER BY 1;
DROP FUNCTION tp.evil_view(), tp.evil_write();
-- A role that reads every table (a backup role) reads the base tables as
-- they are, with row security off, which is what pg_dump does.
CREATE ROLE tp_backup NOLOGIN IN ROLE pg_read_all_data;
SET ROLE tp_backup;
SET row_security = off;
SELECT (SELECT count(*) FROM kingres.timepart_policy_data) >= 2 AS reads_policies,
       (SELECT count(*) FROM kingres.timepart_run_data) >= 2 AS reads_runs,
       (SELECT count(*) FROM kingres.timepart_run_action_data) >= 2 AS reads_actions;
SELECT count(*) AS backup_role_sees_in_views FROM kingres.timepart_policy;
INSERT INTO kingres.timepart_policy_data (schema_name, table_name, parent_oid, partition_every, table_owner, created_by)
VALUES ('tpo', 'zz', 0, '1 day', 'tp_backup', 'tp_backup');
RESET row_security;
RESET ROLE;
DROP ROLE tp_backup;
DROP TABLE tpo.mine, tp.theirs3;

-- Cleanup of the roles of the tests.
DROP TABLE tpo.o1, tpo.h1, tpo.pg_class, tpo.pg_inherits, tp.o2;
DROP SCHEMA tpo CASCADE;
REVOKE CREATE ON SCHEMA tp FROM tp_member, tp_other;
DROP OWNED BY tp_owner, tp_other, tp_member, tp_noinherit, tp_nogrant;
DROP ROLE tp_owner, tp_other, tp_member, tp_noinherit, tp_nogrant;
DROP OWNED BY "Own'er""; DROP TABLE tp.canary; --";
DROP ROLE "Own'er""; DROP TABLE tp.canary; --";
SELECT count(*) AS leftover_roles FROM pg_roles WHERE rolname LIKE 'tp\_%' OR rolname LIKE 'Own%';
