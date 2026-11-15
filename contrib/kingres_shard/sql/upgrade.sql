-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- upgrade.sql: pg_regress test of kingres_shard 0.5 (purpose below).
-- ALTER EXTENSION kingres_shard UPDATE from 0.1 (through 0.2, 0.3 and 0.4), from 0.2, from 0.3 and from 0.4 to 0.5 must give exactly the
-- catalog that installing 0.5 gives, and keep the data of the catalog tables.
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

-- Install 0.5 directly.
CREATE EXTENSION kingres_shard;
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
CREATE TABLE public.upg_direct AS SELECT line FROM public.upg_snapshot() line;
SELECT count(*) > 100 AS direct_snapshot_is_not_trivial FROM public.upg_direct;
DROP EXTENSION kingres_shard;

-- Install 0.1, use it, upgrade.
CREATE EXTENSION kingres_shard VERSION '0.1';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text);
SELECT kingres.create_distributed_table('public.upg_t', 'id', bucket_count => 4);
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

DROP TABLE public.upg_t;
DROP EXTENSION kingres_shard;

-- The steps from 0.2 to 0.3 and from 0.3 to 0.4 on their own, with a bucket map that is not trivial.
CREATE EXTENSION kingres_shard VERSION '0.2';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text);
SELECT kingres.create_distributed_table('public.upg_t', 'id', bucket_count => 4);
INSERT INTO public.upg_t VALUES (3, 'three');
ALTER EXTENSION kingres_shard UPDATE TO '0.3';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
ALTER EXTENSION kingres_shard UPDATE TO '0.4';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
ALTER EXTENSION kingres_shard UPDATE TO '0.5';
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
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text);
SELECT kingres.create_distributed_table('public.upg_t', 'id', bucket_count => 4);
INSERT INTO public.upg_t VALUES (4, 'four');
SELECT count(*) AS secret_rows FROM kingres.cluster_secret;
ALTER EXTENSION kingres_shard UPDATE TO '0.4';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
ALTER EXTENSION kingres_shard UPDATE TO '0.5';
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
CREATE TABLE public.upg_t (id int PRIMARY KEY, v text);
SELECT kingres.create_distributed_table('public.upg_t', 'id', bucket_count => 4);
INSERT INTO public.upg_t VALUES (5, 'five');
INSERT INTO kingres.dtx_log VALUES ('kingres_1_1_1_1', 1, now());
ALTER EXTENSION kingres_shard UPDATE TO '0.5';
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

DROP TABLE public.upg_direct, public.upg_upgraded;
DROP FUNCTION public.upg_snapshot();
