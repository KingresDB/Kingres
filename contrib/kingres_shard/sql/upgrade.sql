-- SPDX-License-Identifier: PostgreSQL
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- upgrade.sql: pg_regress test of kingres_shard 0.2 (purpose below).
-- ALTER EXTENSION kingres_shard UPDATE from 0.1 to 0.2 must give exactly the
-- catalog that installing 0.2 gives, and keep the data of the catalog tables.
SET search_path = public, pg_temp;

-- A description of every object of the extension that the two paths could
-- make differ: columns, constraints, indexes, functions (with the digest of
-- their body), triggers, event triggers, comments and privileges.
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
    FROM pg_catalog.pg_class c
   WHERE c.relnamespace = 'kingres'::regnamespace AND c.relkind IN ('r', 'v')
  UNION ALL
  SELECT 'member ' || pg_catalog.pg_describe_object(d.classid, d.objid, d.objsubid)
    FROM pg_catalog.pg_depend d
   WHERE d.refclassid = 'pg_extension'::regclass
     AND d.refobjid = (SELECT oid FROM pg_catalog.pg_extension WHERE extname = 'kingres_shard')
     AND d.deptype = 'e';
END
$$;

-- Install 0.2 directly.
CREATE EXTENSION kingres_shard;
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
CREATE TABLE public.upg_direct AS SELECT line FROM public.upg_snapshot() line;
SELECT count(*) > 100 AS direct_snapshot_is_not_trivial FROM public.upg_direct;
DROP EXTENSION kingres_shard;

-- Install 0.1, use it, upgrade.
CREATE EXTENSION kingres_shard VERSION '0.1';
SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard';
SELECT kingres.init_cluster('localhost', current_setting('port')::int);
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

DROP TABLE public.upg_t;
DROP EXTENSION kingres_shard;
DROP TABLE public.upg_direct, public.upg_upgraded;
DROP FUNCTION public.upg_snapshot();
