-- timepart_security: who may do what.  The functions run DDL on user tables:
-- a caller must not be able to do through them anything it could not do
-- directly.  Catalog hygiene of every function, ownership checks for a role
-- that is not the owner, a member of the owner role, a role that is
-- not granted EXECUTE, the superuser-only functions, a changed owner, an
-- owner that cannot create in the schema, and a hostile search_path.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: PostgreSQL

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
-- The tables and the view are not readable or writable by anyone else.
SELECT relname,
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
-- The owner cannot read the policy tables, only use the functions.
SELECT * FROM kingres.timepart_policy;
SELECT * FROM kingres.timepart_run;
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
SELECT kingres.timepart__step(1, 'create');
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

-- Cleanup of the roles of the tests.
DROP TABLE tpo.o1, tpo.h1, tpo.pg_class, tpo.pg_inherits, tp.o2;
DROP SCHEMA tpo CASCADE;
REVOKE CREATE ON SCHEMA tp FROM tp_member, tp_other;
DROP OWNED BY tp_owner, tp_other, tp_member, tp_noinherit, tp_nogrant;
DROP ROLE tp_owner, tp_other, tp_member, tp_noinherit, tp_nogrant;
DROP OWNED BY "Own'er""; DROP TABLE tp.canary; --";
DROP ROLE "Own'er""; DROP TABLE tp.canary; --";
SELECT count(*) AS leftover_roles FROM pg_roles WHERE rolname LIKE 'tp\_%' OR rolname LIKE 'Own%';
