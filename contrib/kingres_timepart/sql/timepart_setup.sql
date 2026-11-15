-- timepart_setup: install the extension, the roles and the helpers used by
-- the other tests, and look at what the install script created.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

SET client_min_messages = warning;
-- Roles are global: remove those of an earlier run that did not finish.
DROP ROLE IF EXISTS tp_owner, tp_other, tp_member, tp_noinherit, tp_nogrant;
DROP ROLE IF EXISTS "Own'er""; DROP TABLE tp.canary; --";
CREATE EXTENSION kingres_timepart;
RESET client_min_messages;
\set VERBOSITY default
\set SHOW_CONTEXT never
SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;

-- What the install script created, by kind.
SELECT extversion FROM pg_extension WHERE extname = 'kingres_timepart';
SELECT count(*) FILTER (WHERE c.relkind = 'r') AS tables,
       count(*) FILTER (WHERE c.relkind = 'v') AS views,
       count(*) FILTER (WHERE c.relkind = 'S') AS sequences
  FROM pg_depend d JOIN pg_class c ON c.oid = d.objid
 WHERE d.classid = 'pg_class'::regclass AND d.deptype IN ('e', 'a')
   AND d.refobjid = (SELECT oid FROM pg_extension WHERE extname = 'kingres_timepart')
   AND c.relkind IN ('r', 'v', 'S')
   AND d.refclassid = 'pg_extension'::regclass;
SELECT evtname, evtevent, evtenabled FROM pg_event_trigger
 WHERE evtname LIKE 'kingres_timepart%' ORDER BY evtname;
-- the tables are dumped with the extension configuration
SELECT c.relname, c.relkind AS kind
  FROM pg_extension e, unnest(e.extconfig) AS t(oid), pg_class c
 WHERE e.extname = 'kingres_timepart' AND c.oid = t.oid
 ORDER BY 1;

-- Roles of the tests.
CREATE ROLE tp_owner NOLOGIN;
CREATE ROLE tp_other NOLOGIN;
CREATE ROLE tp_member NOLOGIN IN ROLE tp_owner;
CREATE ROLE tp_noinherit NOLOGIN NOINHERIT IN ROLE tp_owner;
CREATE ROLE tp_nogrant NOLOGIN;
CREATE SCHEMA tp;
CREATE SCHEMA tpo AUTHORIZATION tp_owner;
GRANT USAGE ON SCHEMA kingres TO tp_owner, tp_other, tp_member, tp_noinherit, tp_nogrant;
GRANT EXECUTE ON FUNCTION
    kingres.create_time_partitions(regclass, interval, timestamptz, timestamptz, name, text),
    kingres.drop_old_time_partitions(regclass, timestamptz, boolean, text),
    kingres.timepart_set_policy(regclass, interval, interval, interval, text, interval, text[], jsonb, text, interval, boolean),
    kingres.timepart_unset_policy(regclass),
    kingres.timepart_plan(regclass, timestamptz),
    kingres.timepart_partitions(regclass),
    kingres.timepart_check(interval, timestamptz)
  TO tp_owner, tp_other, tp_member, tp_noinherit;

-- Helpers: partitions of a table with their bounds, gaps between partitions,
-- and the count of rows per partition.
CREATE FUNCTION tp.parts(rel regclass)
RETURNS TABLE (part name, bounds text)
LANGUAGE sql STABLE AS $$
    SELECT c.relname, regexp_replace(pg_get_expr(c.relpartbound, c.oid), '^FOR VALUES ', '')
      FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
     WHERE i.inhparent = rel
     ORDER BY (SELECT min(x.lo) FROM kingres.timepart__children(rel) x WHERE x.part_oid = c.oid) NULLS LAST, c.relname
$$;
CREATE FUNCTION tp.gaps(rel regclass)
RETURNS TABLE (gap_from timestamptz, gap_to timestamptz)
LANGUAGE sql STABLE AS $$
    SELECT prev_hi, lo FROM (
        SELECT lo, lag(hi) OVER (ORDER BY lo) AS prev_hi
          FROM kingres.timepart__children(rel) WHERE NOT is_default) g
     WHERE prev_hi IS NOT NULL AND prev_hi <> lo
$$;
-- The actions of the latest run of a table.
CREATE FUNCTION tp.last_actions(p_schema name, p_table name)
RETURNS TABLE (kind text, partition_name name, state text, detail text)
LANGUAGE sql STABLE AS $$
    SELECT a.kind, a.partition_name, a.state, left(a.detail, 90)
      FROM kingres.timepart_run_action a
     WHERE a.run_id = (SELECT max(r.run_id) FROM kingres.timepart_run r
                        WHERE r.schema_name = p_schema AND r.table_name = p_table)
     ORDER BY a.seq
$$;
CREATE FUNCTION tp.last_run(p_schema name, p_table name)
RETURNS TABLE (state text, trigger text, error text)
LANGUAGE sql STABLE AS $$
    SELECT r.state, r.trigger, left(r.error, 100)
      FROM kingres.timepart_run r
     WHERE r.schema_name = p_schema AND r.table_name = p_table
     ORDER BY r.run_id DESC LIMIT 1
$$;
