-- sql/security.sql
--   Security of the install script (review 2026-11-14: S-A, S-B, and the
--   settings of the routing switches).
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- The role that runs the regression tests has the same name as the extension
-- schema, so "$user" would resolve to it: pin the search path.
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;

-- Every function of the extension pins a search_path that ends with pg_temp,
-- and only the allow-listed functions are SECURITY DEFINER.  The query lists
-- violations, so an empty result is a pass.
WITH ext_functions AS (
    SELECT p.oid, p.oid::regprocedure::text AS fn, p.prosecdef, p.proconfig
      FROM pg_proc p
      JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
                      AND d.deptype = 'e'
      JOIN pg_extension e ON e.oid = d.refobjid
                         AND d.refclassid = 'pg_extension'::regclass
     WHERE e.extname = 'kingres_shard'
)
SELECT fn AS function_without_pinned_search_path
  FROM ext_functions f
 WHERE NOT EXISTS (SELECT 1
                     FROM unnest(coalesce(f.proconfig, ARRAY[]::text[])) c
                    WHERE c ~ '^search_path=.*, pg_temp$')
 ORDER BY fn;

WITH ext_functions AS (
    SELECT p.oid::regprocedure::text AS fn, p.prosecdef
      FROM pg_proc p
      JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
                      AND d.deptype = 'e'
      JOIN pg_extension e ON e.oid = d.refobjid
                         AND d.refclassid = 'pg_extension'::regclass
     WHERE e.extname = 'kingres_shard'
)
SELECT fn AS unexpected_security_definer
  FROM ext_functions
 WHERE prosecdef
   AND fn NOT IN ('kingres.forget_dropped_tables()', 'kingres.internal_capture_trigger()')
 ORDER BY fn;

-- Exactly two functions are SECURITY DEFINER: the guard of dropped tables and the trigger function of the
-- capture of an online table operation (it writes the key of a changed row into a log table that the
-- writer has no privilege on; EXECUTE is revoked from PUBLIC, so only a superuser can make a trigger of it).
SELECT p.oid::regprocedure::text AS security_definer_function
  FROM pg_proc p
  JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e'
  JOIN pg_extension e ON e.oid = d.refobjid AND d.refclassid = 'pg_extension'::regclass
 WHERE e.extname = 'kingres_shard' AND p.prosecdef
 ORDER BY 1;

-- Every function of the table operations is revoked from PUBLIC (the roles are the ones of the install: none
-- can run them without a grant).
SELECT p.oid::regprocedure::text AS function_callable_by_public
  FROM pg_proc p
  JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e'
  JOIN pg_extension e ON e.oid = d.refobjid AND d.refclassid = 'pg_extension'::regclass
 WHERE e.extname = 'kingres_shard'
   AND p.proname IN ('begin_table_operation', 'table_operation_advance', 'cancel_table_operation',
        'resume_table_operations', 'cleanup_table_operations', 'create_distributed_table_concurrently',
        'create_reference_table_concurrently', 'table_change_report', 'check_cluster',
        'internal_capture_trigger', 'internal_capture_truncate', 'internal_op_lock', 'internal_op_unlock', 'internal_op_alive',
        'internal_op_set', 'internal_op_spec', 'internal_op_capture', 'internal_op_copy_step', 'internal_op_clean',
        'internal_op_undo', 'internal_op_request_cancel', 'internal_op_attempt', 'internal_lock_table', 'internal_lock_holders',
        'internal_setting', 'internal_branch_exec', 'internal_copy_chunk', 'internal_copy_routed', 'internal_swap_table',
        'internal_shadow_script', 'internal_choose_colocation', 'internal_replay_chunk', 'internal_retire_table',
        'internal_switch_attempt', 'internal_table_digest', 'internal_begin_table_operation', 'internal_log_key',
        'internal_prevent_xact_block', 'internal_check_table_operations', 'internal_check_cluster_base',
        'internal_table_ddl', 'internal_require_version', 'internal_advance_sequence', 'internal_rename_identity')
   AND has_function_privilege('public', p.oid, 'EXECUTE')
 ORDER BY 1;

-- The count guards the two queries above against an empty scan.
SELECT count(*) > 15 AS functions_were_scanned
  FROM pg_proc p
  JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
                  AND d.deptype = 'e'
  JOIN pg_extension e ON e.oid = d.refobjid
                     AND d.refclassid = 'pg_extension'::regclass
 WHERE e.extname = 'kingres_shard';

-- S-A: an ordinary role must not become superuser through the SECURITY
-- DEFINER event trigger.  The trigger only evaluates the hijacked cast when
-- kingres.dist_table has a row, so distribute a table first.
SELECT kingres.init_cluster('localhost', current_setting('port')::int);
CREATE TABLE sec_t (id int PRIMARY KEY, v text);
SELECT kingres.create_distributed_table('sec_t', 'id', bucket_count => 4);

CREATE ROLE ks_sec_attacker LOGIN;
CREATE ROLE ks_sec_victim LOGIN;

SET ROLE ks_sec_attacker;
CREATE TEMP TABLE zz (a int);
CREATE FUNCTION pg_temp.evil(x anyelement) RETURNS bool LANGUAGE plpgsql AS $$
BEGIN
    ALTER ROLE ks_sec_victim SUPERUSER;
    RETURN true;
END $$;
CREATE DOMAIN pg_temp.oid AS pg_catalog.oid CHECK (pg_temp.evil(VALUE));
CREATE DOMAIN pg_temp.text AS pg_catalog.text CHECK (pg_temp.evil(VALUE));
CREATE DOMAIN pg_temp.regclass AS pg_catalog.regclass CHECK (pg_temp.evil(VALUE));
DROP TABLE zz;
RESET ROLE;

SELECT rolname, rolsuper AS became_superuser
  FROM pg_roles WHERE rolname = 'ks_sec_victim';

-- Ordinary roles can still use the schema.
SET ROLE ks_sec_attacker;
SELECT count(*) AS still_works FROM pg_catalog.pg_tables WHERE tablename = 'sec_t';
RESET ROLE;
SELECT kingres.internal_catalog_snapshot() IS NOT NULL AS snapshot_works;

-- The routing switches.  kingres_shard.forwarded is fixed for the life of a
-- session (it is set by the connection options of a forwarded session), and
-- kingres_shard.enable_routing is for administrators: with it off a
-- statement is no longer checked.
SET ROLE ks_sec_attacker;
SET kingres_shard.forwarded = on;
RESET kingres_shard.forwarded;
SET kingres_shard.enable_routing = off;
SHOW kingres_shard.forwarded;
SHOW kingres_shard.enable_routing;
RESET ROLE;

-- The table lifecycle (0.7): its functions and records are for superusers.
-- Every function the phase made is closed to PUBLIC (the query lists the open ones).
SELECT p.oid::regprocedure AS function_open_to_public
  FROM pg_proc p
 WHERE p.pronamespace = 'kingres'::regnamespace
   AND p.proname IN ('table_change_report', 'internal_raise_blockers', 'internal_require_version',
                     'internal_has_hash_support', 'internal_advance_sequence', 'internal_rename_identity',
                     'internal_table_ddl', 'internal_check_distributable', 'internal_check_referenceable',
                     'internal_copy_routed')
   AND (p.proacl IS NULL OR EXISTS (SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee = 0))
 ORDER BY 1::text;
SELECT count(*) = 10 AS all_ten_were_found
  FROM pg_proc p
 WHERE p.pronamespace = 'kingres'::regnamespace
   AND p.proname IN ('table_change_report', 'internal_raise_blockers', 'internal_require_version',
                     'internal_has_hash_support', 'internal_advance_sequence', 'internal_rename_identity',
                     'internal_table_ddl', 'internal_check_distributable', 'internal_check_referenceable',
                     'internal_copy_routed');
CREATE ROLE ks_sec_monitor;
GRANT pg_monitor TO ks_sec_monitor;
SET ROLE ks_sec_attacker;
SELECT * FROM kingres.table_operation;
SELECT * FROM kingres.table_operation_item;
SELECT * FROM kingres.table_operation_lock;
SELECT * FROM kingres.table_old_copy;
SELECT * FROM kingres.table_operations;
SELECT * FROM kingres.partitions;
SELECT nextval('kingres.colocation_id_seq');
SELECT * FROM kingres.table_change_report('distribute', 'sec_t', '{"column":"id"}');
SELECT kingres.internal_copy_routed('sec_t', 'public', 'sec_t', 'every_node');
SELECT kingres.internal_require_version('0.8');
SELECT kingres.create_distributed_table('sec_t', 'id');
SELECT kingres.create_reference_table('sec_t');
RESET ROLE;
-- the records show progress to pg_monitor, without error text and without a node address
SET ROLE ks_sec_monitor;
SELECT count(*) AS operations_seen FROM kingres.table_operations;
SELECT count(*) AS partitions_seen FROM kingres.partitions;
SELECT * FROM kingres.table_operation;
RESET ROLE;
SELECT column_name FROM information_schema.columns
 WHERE table_schema = 'kingres' AND table_name = 'table_operations' ORDER BY ordinal_position;
-- the catalog tables of a table's partitions are readable like the rest of the catalog
SET ROLE ks_sec_attacker;
SELECT count(*) AS partitions_catalog_readable FROM kingres.dist_partition;
RESET ROLE;
DROP ROLE ks_sec_monitor;

DROP TABLE sec_t;
DROP OWNED BY ks_sec_attacker;
DROP ROLE ks_sec_attacker;
DROP ROLE ks_sec_victim;
DROP EXTENSION kingres_shard;
