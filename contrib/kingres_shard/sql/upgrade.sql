-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- upgrade.sql: pg_regress test of kingres_shard 0.8 (purpose below).
-- ALTER EXTENSION kingres_shard UPDATE from 0.1 (through 0.2 to 0.6), from 0.2, from 0.3, from 0.4, from 0.5 from 0.6 and from 0.7 to 0.8 must give exactly the
-- catalog that installing 0.8 gives, and keep the data of the catalog tables.
SET search_path = public, pg_temp;

-- A description of every object of the extension that the two paths could
-- make differ: columns (with defaults, identity, generation, collation, storage and
-- column privileges), constraints, indexes, views (with their definitions),
-- relations (kind, persistence, owner, options), sequences (with their parameters),
-- functions (with the digest of their body, owner and the objects they depend
-- on), triggers, event triggers (with their tags and owner), policies, comments,
-- privileges, the schema's owner and privileges, the configuration tables the
-- extension dumps, and the members of the extension (review finding S6-10).
CREATE FUNCTION public.upg_snapshot() RETURNS SETOF text
LANGUAGE plpgsql AS $$
BEGIN
RETURN QUERY
  SELECT 'column ' || c.relname || '.' || a.attnum || ' ' || a.attname || ' '
         || pg_catalog.format_type(a.atttypid, a.atttypmod)
         || ' notnull=' || a.attnotnull
         || ' default=' || coalesce(pg_catalog.pg_get_expr(d.adbin, d.adrelid), '-')
    FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
    LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
   WHERE c.relnamespace = 'kingres'::regnamespace AND c.relkind IN ('r', 'v')
  UNION ALL
  SELECT 'constraint ' || conrelid::regclass || ' ' || conname || ' ' || pg_catalog.pg_get_constraintdef(oid)
    FROM pg_catalog.pg_constraint WHERE connamespace = 'kingres'::regnamespace
  UNION ALL
  SELECT 'index ' || pg_catalog.pg_get_indexdef(i.indexrelid)
    FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indrelid
   WHERE c.relnamespace = 'kingres'::regnamespace
  UNION ALL
  SELECT 'function ' || p.oid::regprocedure || ' ' || pg_catalog.pg_get_function_result(p.oid)
         || ' lang=' || l.lanname || ' secdef=' || p.prosecdef || ' strict=' || p.proisstrict
         || ' volatile=' || p.provolatile::text || ' parallel=' || p.proparallel::text
         || ' config=' || coalesce(p.proconfig::text, '-')
         || ' acl=' || coalesce(p.proacl::text, '-')
         || ' src=' || md5(coalesce(p.prosrc, '')) || ' bin=' || coalesce(p.probin, '-')
    FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_language l ON l.oid = p.prolang
   WHERE p.pronamespace = 'kingres'::regnamespace
  UNION ALL
  SELECT 'trigger ' || t.tgrelid::regclass || ' ' || pg_catalog.pg_get_triggerdef(t.oid)
    FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid
   WHERE c.relnamespace = 'kingres'::regnamespace AND NOT t.tgisinternal
  UNION ALL
  SELECT 'event trigger ' || evtname || ' ' || evtevent || ' ' || evtfoid::regproc || ' ' || evtenabled::text
    FROM pg_catalog.pg_event_trigger WHERE evtfoid::regproc::text LIKE 'kingres.%'
  UNION ALL
  SELECT 'comment ' || pg_catalog.pg_describe_object(classoid, objoid, objsubid) || ' ' || description
    FROM pg_catalog.pg_description
   WHERE (classoid = 'pg_class'::regclass AND objoid IN (SELECT oid FROM pg_catalog.pg_class WHERE relnamespace = 'kingres'::regnamespace))
      OR (classoid = 'pg_proc'::regclass AND objoid IN (SELECT oid FROM pg_catalog.pg_proc WHERE pronamespace = 'kingres'::regnamespace))
  UNION ALL
  SELECT 'acl ' || c.relname || ' ' || coalesce(c.relacl::text, '-')
         || ' rls=' || c.relrowsecurity || '/' || c.relforcerowsecurity
    FROM pg_catalog.pg_class c
   WHERE c.relnamespace = 'kingres'::regnamespace AND c.relkind IN ('r', 'v')
  UNION ALL
  SELECT 'member ' || pg_catalog.pg_describe_object(d.classid, d.objid, d.objsubid)
    FROM pg_catalog.pg_depend d
   WHERE d.refclassid = 'pg_extension'::regclass
     AND d.refobjid = (SELECT oid FROM pg_catalog.pg_extension WHERE extname = 'kingres_shard')
     AND d.deptype = 'e'
  UNION ALL
  SELECT 'view ' || c.relname || ' ' || pg_catalog.pg_get_viewdef(c.oid, true)
         || ' options=' || coalesce(c.reloptions::text, '-')
    FROM pg_catalog.pg_class c
   WHERE c.relnamespace = 'kingres'::regnamespace AND c.relkind = 'v'
  UNION ALL
  SELECT 'relation ' || c.relname || ' kind=' || c.relkind::text || ' persistence=' || c.relpersistence::text
         || ' owner=' || c.relowner::regrole || ' options=' || coalesce(c.reloptions::text, '-')
    FROM pg_catalog.pg_class c
   WHERE c.relnamespace = 'kingres'::regnamespace AND c.relkind IN ('r', 'v', 'S', 'i')
  UNION ALL
  SELECT 'column details ' || c.relname || '.' || a.attname || ' identity=' || a.attidentity::text
         || ' generated=' || a.attgenerated::text || ' collation=' || a.attcollation::regcollation
         || ' storage=' || a.attstorage::text || ' acl=' || coalesce(a.attacl::text, '-')
    FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
   WHERE c.relnamespace = 'kingres'::regnamespace AND c.relkind IN ('r', 'v')
  UNION ALL
  SELECT 'sequence ' || s.seqrelid::regclass || ' type=' || s.seqtypid::regtype || ' start=' || s.seqstart
         || ' increment=' || s.seqincrement || ' max=' || s.seqmax || ' min=' || s.seqmin
         || ' cache=' || s.seqcache || ' cycle=' || s.seqcycle
    FROM pg_catalog.pg_sequence s JOIN pg_catalog.pg_class c ON c.oid = s.seqrelid
   WHERE c.relnamespace = 'kingres'::regnamespace
  UNION ALL
  SELECT 'policy ' || p.polrelid::regclass || ' ' || p.polname || ' cmd=' || p.polcmd::text
         || ' permissive=' || p.polpermissive || ' roles=' || p.polroles::text
         || ' using=' || coalesce(pg_catalog.pg_get_expr(p.polqual, p.polrelid), '-')
         || ' check=' || coalesce(pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid), '-')
    FROM pg_catalog.pg_policy p JOIN pg_catalog.pg_class c ON c.oid = p.polrelid
   WHERE c.relnamespace = 'kingres'::regnamespace
  UNION ALL
  SELECT 'function owner ' || p.oid::regprocedure || ' ' || p.proowner::regrole
    FROM pg_catalog.pg_proc p WHERE p.pronamespace = 'kingres'::regnamespace
  UNION ALL
  SELECT 'function depends ' || p.oid::regprocedure || ' -> '
         || pg_catalog.pg_describe_object(d.refclassid, d.refobjid, d.refobjsubid) || ' ' || d.deptype::text
    FROM pg_catalog.pg_depend d
    JOIN pg_catalog.pg_proc p ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
   WHERE p.pronamespace = 'kingres'::regnamespace AND d.refclassid <> 'pg_extension'::regclass
  UNION ALL
  SELECT 'event trigger details ' || evtname || ' tags=' || coalesce(evttags::text, '-')
         || ' owner=' || evtowner::regrole
    FROM pg_catalog.pg_event_trigger WHERE evtfoid::regproc::text LIKE 'kingres.%'
  UNION ALL
  SELECT 'schema kingres owner=' || n.nspowner::regrole || ' acl=' || coalesce(n.nspacl::text, '-')
    FROM pg_catalog.pg_namespace n WHERE n.nspname = 'kingres'
  UNION ALL
  SELECT 'type ' || t.typname || ' kind=' || t.typtype::text
    FROM pg_catalog.pg_type t
   WHERE t.typnamespace = 'kingres'::regnamespace AND t.typtype <> 'c' AND t.typcategory <> 'A'
  UNION ALL
  SELECT 'dumped configuration ' || coalesce(extconfig::regclass[]::text, '-') || ' ' || coalesce(extcondition::text, '-')
    FROM pg_catalog.pg_extension WHERE extname = 'kingres_shard';
END
$$;

-- A table distributed the way the older versions did it: this test creates tables in
-- versions 0.1 to 0.4, whose SQL objects the library of 0.8 no longer drives
-- (create_distributed_table refuses a database whose objects are older than the library),
-- so the partitions and the catalog rows are made by hand.  p_table is a table that is
-- PARTITION BY HASH on its first column and has no partitions yet.
CREATE FUNCTION public.upg_register(p_table regclass, p_buckets int) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    v_col  name;
    v_id   int;
    v_name text := (SELECT relname FROM pg_catalog.pg_class WHERE oid = p_table);
BEGIN
    SELECT a.attname INTO v_col FROM pg_catalog.pg_attribute a
     WHERE a.attrelid = p_table AND a.attnum = 1;
    FOR b IN 0 .. p_buckets - 1 LOOP
        EXECUTE format('CREATE TABLE public.%I PARTITION OF %s FOR VALUES WITH (MODULUS %s, REMAINDER %s)',
                       format('%s_b%s', v_name, lpad(b::text, 4, '0')), p_table, p_buckets, b);
    END LOOP;
    SELECT coalesce(max(colocation_id), 0) + 1 INTO v_id FROM kingres.colocation;
    INSERT INTO kingres.colocation (colocation_id, bucket_count, dist_column_type, dist_collation)
         VALUES (v_id, p_buckets, 'int4', 0);
    INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id)
         SELECT v_id, g, 0 FROM generate_series(0, p_buckets - 1) g;
    INSERT INTO kingres.dist_table (relid, method, dist_column, colocation_id)
         VALUES (p_table, 'hash', v_col, v_id);
END
$$;

-- Install 0.8 directly.
CREATE EXTENSION kingres_shard;
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
CREATE TABLE public.upg_direct AS SELECT line FROM public.upg_snapshot() line;
SELECT count(*) > 100 AS direct_snapshot_is_not_trivial FROM public.upg_direct;
DROP EXTENSION kingres_shard;

-- Install 0.1, use it, upgrade.
CREATE EXTENSION kingres_shard VERSION '0.1';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);
-- Until ALTER EXTENSION UPDATE has run, the functions of the table lifecycle refuse with a
-- clear message instead of a missing function (reference tables arrived in 0.2, so only the first function exists).
CREATE TABLE public.upg_early (id int PRIMARY KEY);
SELECT kingres.create_distributed_table('public.upg_early', 'id');
DROP TABLE public.upg_early;
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text) PARTITION BY HASH (id);
SELECT public.upg_register('public.upg_t', 4);
INSERT INTO public.upg_t VALUES (1, 'one'), (2, 'two');

ALTER EXTENSION kingres_shard UPDATE;
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
CREATE TABLE public.upg_upgraded AS SELECT line FROM public.upg_snapshot() line;

-- No difference in either direction.
SELECT 'only in direct install' AS side, line FROM
  (SELECT line FROM public.upg_direct EXCEPT SELECT line FROM public.upg_upgraded) x
UNION ALL
SELECT 'only in upgrade', line FROM
  (SELECT line FROM public.upg_upgraded EXCEPT SELECT line FROM public.upg_direct) y
ORDER BY 1, 2;

-- The data survived and the new objects work.
SELECT method, dist_column, colocation_id FROM kingres.dist_table;
SELECT count(*) FROM kingres.bucket_map;
SELECT count(*) FROM public.upg_t;
SELECT count(*) FROM kingres.dtx_log;
SELECT kingres.recover_prepared_transactions();
-- The shape constraint of 0.2: a hash table needs its column and group, a
-- reference table has neither.
UPDATE kingres.dist_table SET dist_column = NULL;
BEGIN;
UPDATE kingres.dist_table SET method = 'reference', dist_column = NULL, colocation_id = NULL;
SELECT method, dist_column, colocation_id FROM kingres.dist_table;
ROLLBACK;

-- 0.3 and 0.4: the records of bucket moves exist, empty, and the new functions work on
-- a catalog that came from 0.1.
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT count(*) AS moves FROM kingres.bucket_move;
SELECT count(*) AS markers FROM kingres.move_marker;
SELECT count(*) AS plan_rows FROM kingres.rebalance_plan();
SELECT count(*) AS cluster_problems FROM kingres.check_cluster();
SELECT kingres.resume_bucket_moves();
SELECT kingres.cleanup_moved_buckets();

-- 0.7: a table that was distributed before the upgrade is known to the new functions,
-- and the numbers of colocation groups are never used twice.
SELECT severity, code FROM kingres.table_change_report('alter', 'public.upg_t', '{"bucket_count":8}');
SELECT severity, code FROM kingres.table_change_report('distribute', 'public.upg_t', '{"column":"id"}');
SELECT count(*) AS partitions_of_user_tables FROM kingres.dist_partition;
SELECT count(*) AS operations FROM kingres.table_operation;
CREATE TABLE public.upg_t2 (id int PRIMARY KEY, v text);
INSERT INTO public.upg_t2 VALUES (7, 'seven'), (8, 'eight');
SELECT kingres.create_distributed_table('public.upg_t2', 'id', bucket_count => 6);
SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.upg_t2'::regclass;
SELECT count(*) FROM public.upg_t2;
DROP TABLE public.upg_t2;
DELETE FROM kingres.bucket_map WHERE colocation_id = 2;
DELETE FROM kingres.colocation WHERE colocation_id = 2;
CREATE TABLE public.upg_t3 (id int PRIMARY KEY);
SELECT kingres.create_distributed_table('public.upg_t3', 'id', bucket_count => 6);
SELECT colocation_id AS never_reused FROM kingres.dist_table WHERE relid = 'public.upg_t3'::regclass;
DROP TABLE public.upg_t3;
DELETE FROM kingres.bucket_map WHERE colocation_id = 3;
DELETE FROM kingres.colocation WHERE colocation_id = 3;
-- a map group primary that is not upgraded sends a catalog without the new keys
BEGIN;
SELECT kingres.internal_apply_catalog(kingres.internal_catalog_snapshot() - 'dist_partition');
SELECT count(*) AS tables_after_old_snapshot FROM kingres.dist_table;
ROLLBACK;
-- the version handshake of the new functions
SELECT kingres.internal_require_version('0.8');
DO $$ BEGIN PERFORM kingres.internal_require_version('0.6'); EXCEPTION WHEN object_not_in_prerequisite_state THEN RAISE NOTICE '%', regexp_replace(SQLERRM, '\([^)]*\)', '(address)'); END $$;

DROP TABLE public.upg_t;
DROP EXTENSION kingres_shard;

-- The steps from 0.2 to 0.3 and from 0.3 to 0.4 on their own, with a bucket map that is not trivial.
CREATE EXTENSION kingres_shard VERSION '0.2';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text) PARTITION BY HASH (id);
SELECT public.upg_register('public.upg_t', 4);
INSERT INTO public.upg_t VALUES (3, 'three');
ALTER EXTENSION kingres_shard UPDATE TO '0.3';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
ALTER EXTENSION kingres_shard UPDATE TO '0.4';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
ALTER EXTENSION kingres_shard UPDATE TO '0.5';
ALTER EXTENSION kingres_shard UPDATE TO '0.6';
ALTER EXTENSION kingres_shard UPDATE TO '0.7';
ALTER EXTENSION kingres_shard UPDATE TO '0.8';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
DROP TABLE public.upg_upgraded;
CREATE TABLE public.upg_upgraded AS SELECT line FROM public.upg_snapshot() line;
SELECT 'only in direct install' AS side, line FROM
  (SELECT line FROM public.upg_direct EXCEPT SELECT line FROM public.upg_upgraded) x
UNION ALL
SELECT 'only in upgrade', line FROM
  (SELECT line FROM public.upg_upgraded EXCEPT SELECT line FROM public.upg_direct) y
ORDER BY 1, 2;
SELECT count(*) FROM public.upg_t;
SELECT count(*) FROM kingres.bucket_map;
DROP TABLE public.upg_t;
DROP EXTENSION kingres_shard;

-- The step from 0.3 on its own: the 0.3 install script, a table, a node that has a
-- secret and a bucket map, then the upgrade.
CREATE EXTENSION kingres_shard VERSION '0.3';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text) PARTITION BY HASH (id);
SELECT public.upg_register('public.upg_t', 4);
INSERT INTO public.upg_t VALUES (4, 'four');
SELECT count(*) AS secret_rows FROM kingres.cluster_secret;
ALTER EXTENSION kingres_shard UPDATE TO '0.4';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
ALTER EXTENSION kingres_shard UPDATE TO '0.5';
ALTER EXTENSION kingres_shard UPDATE TO '0.6';
ALTER EXTENSION kingres_shard UPDATE TO '0.7';
ALTER EXTENSION kingres_shard UPDATE TO '0.8';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT count(*) AS secret_rows_after FROM kingres.cluster_secret;
DROP TABLE public.upg_upgraded;
CREATE TABLE public.upg_upgraded AS SELECT line FROM public.upg_snapshot() line;
SELECT 'only in direct install' AS side, line FROM
  (SELECT line FROM public.upg_direct EXCEPT SELECT line FROM public.upg_upgraded) x
UNION ALL
SELECT 'only in upgrade', line FROM
  (SELECT line FROM public.upg_upgraded EXCEPT SELECT line FROM public.upg_direct) y
ORDER BY 1, 2;
SELECT count(*) FROM public.upg_t;
SELECT count(*) FROM kingres.bucket_map;
DROP TABLE public.upg_t;
DROP EXTENSION kingres_shard;
-- The step from 0.4 on its own: a node with a secret, a bucket map and a decision of
-- the 0.4 kind in dtx_log, then the upgrade; the new objects work on what 0.4 made.
CREATE EXTENSION kingres_shard VERSION '0.4';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text) PARTITION BY HASH (id);
SELECT public.upg_register('public.upg_t', 4);
INSERT INTO public.upg_t VALUES (5, 'five');
INSERT INTO kingres.dtx_log VALUES ('kingres_1_1_1_1', 1, now());
ALTER EXTENSION kingres_shard UPDATE TO '0.5';
ALTER EXTENSION kingres_shard UPDATE TO '0.6';
ALTER EXTENSION kingres_shard UPDATE TO '0.7';
ALTER EXTENSION kingres_shard UPDATE TO '0.8';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
DROP TABLE public.upg_upgraded;
CREATE TABLE public.upg_upgraded AS SELECT line FROM public.upg_snapshot() line;
SELECT 'only in direct install' AS side, line FROM
  (SELECT line FROM public.upg_direct EXCEPT SELECT line FROM public.upg_upgraded) x
UNION ALL
SELECT 'only in upgrade', line FROM
  (SELECT line FROM public.upg_upgraded EXCEPT SELECT line FROM public.upg_direct) y
ORDER BY 1, 2;
SELECT count(*) FROM public.upg_t;
SELECT count(*) AS decisions_kept FROM kingres.dtx_log;
SELECT count(*) AS incidents FROM kingres.dtx_incident;
SELECT count(*) AS standbys FROM kingres.standby_status();
SELECT count(*) AS cluster_problems FROM kingres.check_cluster();
SELECT kingres.detect_distributed_deadlocks();
DROP TABLE public.upg_t;
DROP EXTENSION kingres_shard;

-- The step from 0.5 on its own: 0.6 adds no object, so a node of 0.5 with a cluster, data and
-- a decision in dtx_log ends, through 0.6, 0.7 and 0.8, in the catalog a fresh 0.8 gives, and keeps everything it had.
CREATE EXTENSION kingres_shard VERSION '0.5';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text) PARTITION BY HASH (id);
SELECT public.upg_register('public.upg_t', 4);
INSERT INTO public.upg_t VALUES (6, 'six');
INSERT INTO kingres.dtx_log VALUES ('kingres_1_1_1_1', 1, now());
SELECT count(*) AS secret_rows FROM kingres.cluster_secret;
ALTER EXTENSION kingres_shard UPDATE TO '0.6';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
ALTER EXTENSION kingres_shard UPDATE TO '0.7';
ALTER EXTENSION kingres_shard UPDATE TO '0.8';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
DROP TABLE public.upg_upgraded;
CREATE TABLE public.upg_upgraded AS SELECT line FROM public.upg_snapshot() line;
SELECT 'only in direct install' AS side, line FROM
  (SELECT line FROM public.upg_direct EXCEPT SELECT line FROM public.upg_upgraded) x
UNION ALL
SELECT 'only in upgrade', line FROM
  (SELECT line FROM public.upg_upgraded EXCEPT SELECT line FROM public.upg_direct) y
ORDER BY 1, 2;
SELECT count(*) FROM public.upg_t;
SELECT count(*) AS decisions_kept FROM kingres.dtx_log;
SELECT count(*) AS secret_rows_after FROM kingres.cluster_secret;
SELECT kingres.internal_check_node_versions();
DROP TABLE public.upg_t;
DROP EXTENSION kingres_shard;

-- The step from 0.6 on its own: a node with a bucket map and a table with rows, then the
-- upgrade; the table keeps its rows, the new objects work on what 0.6 made, and a
-- table with rows is distributed afterwards.
CREATE EXTENSION kingres_shard VERSION '0.6';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text) PARTITION BY HASH (id);
SELECT public.upg_register('public.upg_t', 4);
INSERT INTO public.upg_t VALUES (6, 'six');
ALTER EXTENSION kingres_shard UPDATE TO '0.7';
ALTER EXTENSION kingres_shard UPDATE TO '0.8';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
DROP TABLE public.upg_upgraded;
CREATE TABLE public.upg_upgraded AS SELECT line FROM public.upg_snapshot() line;
SELECT 'only in direct install' AS side, line FROM
  (SELECT line FROM public.upg_direct EXCEPT SELECT line FROM public.upg_upgraded) x
UNION ALL
SELECT 'only in upgrade', line FROM
  (SELECT line FROM public.upg_upgraded EXCEPT SELECT line FROM public.upg_direct) y
ORDER BY 1, 2;
SELECT count(*) FROM public.upg_t;
SELECT last_value, is_called FROM kingres.colocation_id_seq;
CREATE TABLE public.upg_rows (id int PRIMARY KEY, v text);
INSERT INTO public.upg_rows SELECT g, 'r' || g FROM generate_series(1, 100) g;
SELECT kingres.create_distributed_table('public.upg_rows', 'id', colocate_with => 'public.upg_t');
SELECT count(*), sum(id) FROM public.upg_rows;
SELECT colocation_id FROM kingres.dist_table ORDER BY relid::text;
DROP TABLE public.upg_rows;
DROP TABLE public.upg_t;
DROP EXTENSION kingres_shard;

-- The step from 0.7 on its own (phase 11, distributed schemas): a node with a cluster, a table with rows and a
-- bucket map, then the upgrade; the table keeps its rows, the catalog is the one a fresh 0.8 gives, the new tables
-- start as a database before phase 11 is: mode row, no distributed schema, no schema group.
CREATE EXTENSION kingres_shard VERSION '0.7';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text) PARTITION BY HASH (id);
SELECT public.upg_register('public.upg_t', 4);
INSERT INTO public.upg_t VALUES (7, 'seven');
ALTER EXTENSION kingres_shard UPDATE TO '0.8';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
DROP TABLE public.upg_upgraded;
CREATE TABLE public.upg_upgraded AS SELECT line FROM public.upg_snapshot() line;
SELECT 'only in direct install' AS side, line FROM
  (SELECT line FROM public.upg_direct EXCEPT SELECT line FROM public.upg_upgraded) x
UNION ALL
SELECT 'only in upgrade', line FROM
  (SELECT line FROM public.upg_upgraded EXCEPT SELECT line FROM public.upg_direct) y
ORDER BY 1, 2;
SELECT count(*) FROM public.upg_t;
SELECT mode, distribute_new_schemas FROM kingres.sharding_settings;
SELECT count(*) AS distributed_schemas FROM kingres.dist_schema;
SELECT count(*) AS schemas_listed FROM kingres.schemas;
SELECT kind, count(*) FROM kingres.colocation GROUP BY kind ORDER BY kind;
-- the snapshot carries the new tables and a document without them (an older map group primary) leaves them alone
BEGIN;
SELECT kingres.internal_apply_catalog(kingres.internal_catalog_snapshot());
SELECT count(*) AS tables_after_snapshot FROM kingres.dist_table;
SELECT mode FROM kingres.sharding_settings;
ROLLBACK;
BEGIN;
SELECT kingres.internal_apply_catalog(kingres.internal_catalog_snapshot() - 'dist_schema' - 'sharding_settings');
SELECT mode FROM kingres.sharding_settings;
ROLLBACK;
DROP TABLE public.upg_t;
DROP EXTENSION kingres_shard;

DROP TABLE public.upg_direct, public.upg_upgraded;
DROP FUNCTION public.upg_snapshot();
