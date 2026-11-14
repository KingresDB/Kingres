-- sql/routing_decisions.sql
--   Routing decisions that need no second server (review 2026-11-14: finding 1,
--   finding 2, S-D).
--
-- SPDX-License-Identifier: PostgreSQL
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- The cluster has a second shard group that does not exist as a server: the
-- odd buckets belong to it.  A statement for such a bucket is planned as a
-- "Kingres Remote Statement" (EXPLAIN shows it; nothing connects), a
-- statement the router cannot place is refused, and a statement for a local
-- bucket runs here.  Every partition still exists locally, so a statement
-- that is wrongly run here instead of being routed or refused returns rows
-- instead of an error: that is how the tests see a silent wrong answer.
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

CREATE COLLATION ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false);
CREATE TABLE w (name text PRIMARY KEY, v int);
CREATE TABLE wc (name text COLLATE "C" PRIMARY KEY, v int);
CREATE TABLE t (id int PRIMARY KEY, v text);
CREATE TABLE plain (x int);
SELECT kingres.create_distributed_table('w', 'name', bucket_count => 8);
SELECT kingres.create_distributed_table('wc', 'name', bucket_count => 8);
SELECT kingres.create_distributed_table('t', 'id', bucket_count => 8);
INSERT INTO w SELECT 'name' || g, g FROM generate_series(1, 20) g;
INSERT INTO t SELECT g, 'row ' || g FROM generate_series(1, 20) g;

INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (2, 1, 'group1.invalid', 5432, 'primary');
UPDATE kingres.bucket_map SET group_id = 1 WHERE bucket % 2 = 1;

-- A key of a local bucket and of a remote bucket, per table.
SELECT min(k) AS wl FROM (SELECT 'name' || g AS k FROM generate_series(1, 100) g) s
  WHERE kingres.bucket_for('w'::regclass, k) % 2 = 0 \gset
SELECT min(k) AS wr FROM (SELECT 'name' || g AS k FROM generate_series(1, 100) g) s
  WHERE kingres.bucket_for('w'::regclass, k) % 2 = 1 \gset
SELECT min(k) AS wcl FROM (SELECT 'name' || g AS k FROM generate_series(1, 100) g) s
  WHERE kingres.bucket_for('wc'::regclass, k) % 2 = 0 \gset
SELECT min(k) AS wcr FROM (SELECT 'name' || g AS k FROM generate_series(1, 100) g) s
  WHERE kingres.bucket_for('wc'::regclass, k) % 2 = 1 \gset
SELECT min(g) AS tl FROM generate_series(1, 100) g
  WHERE kingres.bucket_for('t'::regclass, g) % 2 = 0 \gset
SELECT min(g) AS tn FROM generate_series(1001, 1100) g
  WHERE kingres.bucket_for('t'::regclass, g) % 2 = 0 \gset
SELECT min(g) AS tr FROM generate_series(1, 100) g
  WHERE kingres.bucket_for('t'::regclass, g) % 2 = 1 \gset

-- ---------------------------------------------------------------------------
-- Finding 2: a key restriction routes only when the comparison uses the
-- collation the key is hashed with.
-- ---------------------------------------------------------------------------
EXPLAIN (COSTS OFF) SELECT * FROM w WHERE name = :'wr';
EXPLAIN (COSTS OFF) SELECT * FROM w WHERE name = :'wl';
EXPLAIN (COSTS OFF) SELECT * FROM wc WHERE name = :'wcr';
EXPLAIN (COSTS OFF) SELECT * FROM wc WHERE name = :'wcl';
-- a different collation: not restricted to one bucket
EXPLAIN (COSTS OFF) SELECT * FROM w WHERE name = :'wr' COLLATE ci;
EXPLAIN (COSTS OFF) SELECT * FROM w WHERE name = :'wl' COLLATE ci;
EXPLAIN (COSTS OFF) SELECT * FROM w WHERE name COLLATE ci = :'wr';
EXPLAIN (COSTS OFF) SELECT * FROM w WHERE name = :'wr' COLLATE "C";
EXPLAIN (COSTS OFF) SELECT * FROM wc WHERE name = :'wcr' COLLATE ci;
EXPLAIN (COSTS OFF) SELECT * FROM wc WHERE name = :'wcl' COLLATE "default";
UPDATE w SET v = 0 WHERE name = :'wr' COLLATE ci;
DELETE FROM w WHERE name = :'wl' COLLATE ci;
SET kingres_shard.enable_routing = off;
SELECT count(*) AS rows_after_refused_delete FROM w;
RESET kingres_shard.enable_routing;

-- ---------------------------------------------------------------------------
-- S-D: statements that reach a distributed table from inside another
-- statement or object are either routed like any statement or refused.
-- ---------------------------------------------------------------------------
-- SQL-language functions that the planner inlines are expanded after the
-- routing decision has been made.
CREATE FUNCTION t_count() RETURNS bigint LANGUAGE sql STABLE
  AS $$ SELECT (SELECT count(*) FROM t) $$;
CREATE FUNCTION t_rows() RETURNS SETOF t LANGUAGE sql STABLE
  AS $$ SELECT * FROM t $$;
CREATE FUNCTION t_by_id(k int) RETURNS SETOF t LANGUAGE sql STABLE
  AS $$ SELECT * FROM t WHERE id = k $$;
SELECT t_count();
SELECT count(*) FROM t_rows();
SELECT * FROM t_by_id(:tl);
SELECT * FROM t_by_id(:tr);
SELECT 1 AS other_statements_unaffected;
-- not inlined: the body is a statement of its own
CREATE FUNCTION t_v(k int) RETURNS text LANGUAGE sql STABLE
  AS $$ SELECT v FROM t WHERE id = k $$;
SELECT t_v(:tl);
CREATE FUNCTION t_count_pl() RETURNS bigint LANGUAGE plpgsql
  AS $$ BEGIN RETURN (SELECT count(*) FROM t); END $$;
CREATE FUNCTION t_one_pl(k int) RETURNS text LANGUAGE plpgsql
  AS $$ BEGIN RETURN (SELECT v FROM t WHERE id = k); END $$;
SELECT t_count_pl();
SELECT t_one_pl(:tl);
DO $$ BEGIN PERFORM count(*) FROM t; END $$;
DO $$ BEGIN EXECUTE 'SELECT count(*) FROM t'; END $$;
-- views and rules
CREATE VIEW t_view AS SELECT * FROM t;
SELECT count(*) FROM t_view;
SELECT * FROM t_view WHERE id = :tl;
CREATE RULE plain_to_t AS ON INSERT TO plain DO ALSO INSERT INTO t VALUES (NEW.x, 'rule');
INSERT INTO plain VALUES (:tn);
CREATE FUNCTION plain_trg() RETURNS trigger LANGUAGE plpgsql
  AS $$ BEGIN PERFORM count(*) FROM t; RETURN NEW; END $$;
CREATE TRIGGER plain_trg_i BEFORE INSERT ON plain FOR EACH ROW EXECUTE FUNCTION plain_trg();
DROP RULE plain_to_t ON plain;
INSERT INTO plain VALUES (1);
-- cursors, CTAS, COPY, EXPLAIN ANALYZE, SELECT INTO
BEGIN;
DECLARE c1 CURSOR FOR SELECT * FROM t;
ROLLBACK;
BEGIN;
DECLARE c2 CURSOR FOR SELECT * FROM t WHERE id = :tl;
FETCH 1 FROM c2;
ROLLBACK;
CREATE TABLE ctas1 AS SELECT * FROM t;
CREATE TABLE ctas2 AS SELECT * FROM t WHERE id = :tl;
SELECT count(*) AS ctas2_rows FROM ctas2;
SELECT * INTO ctas3 FROM t;
COPY (SELECT * FROM t) TO stdout;
COPY t TO stdout;
COPY (SELECT id FROM t WHERE id = :tl) TO stdout;
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) SELECT * FROM t;
EXPLAIN (COSTS OFF) SELECT * FROM t WHERE id = :tr;
-- prepared statements under every plan_cache_mode
PREPARE p1 (int) AS SELECT v FROM t WHERE id = $1;
EXECUTE p1(:tl);
SET plan_cache_mode = force_custom_plan;
EXECUTE p1(:tl);
SET plan_cache_mode = force_generic_plan;
EXECUTE p1(:tl);
SET plan_cache_mode = auto;
EXECUTE p1(:tl);
EXECUTE p1(:tl);
EXECUTE p1(:tl);
EXECUTE p1(:tl);
EXECUTE p1(:tl);
EXECUTE p1(:tl);
EXECUTE p1(:tl);
-- a parallel worker may not forward a statement
CREATE FUNCTION t_par(k int) RETURNS bigint LANGUAGE plpgsql STABLE PARALLEL SAFE
  AS $$ BEGIN RETURN (SELECT count(*) FROM t WHERE id = k); END $$;
SET debug_parallel_query = on;
SELECT t_par(:tl);
SELECT t_par(:tr);
RESET debug_parallel_query;
-- SPI from C goes through the same hook: pg_catalog functions that run
-- queries on their own are not distributed-table aware, so only the
-- statements above matter.

-- ---------------------------------------------------------------------------
-- Finding 1: in a forwarded session a statement runs only if every bucket it
-- can touch is local; otherwise KS001.  The flag is set by the connection
-- options of a forwarded session and cannot be changed later.
-- ---------------------------------------------------------------------------
\c -reuse-previous=on "options='-c kingres_shard.forwarded=on'"
SET search_path = public;
SHOW kingres_shard.forwarded;
SELECT v FROM t WHERE id = :tl;
SELECT * FROM t;
SELECT v FROM t WHERE id = :tr;
SELECT count(*) FROM t WHERE id = :tl OR id = :tr;
UPDATE t SET v = 'x';
DELETE FROM t;
INSERT INTO t VALUES (:tl + 1000, 'a'), (:tr + 1000, 'b');
SELECT count(*) FROM t, w WHERE t.id = :tl;
SELECT t_count();
SELECT * FROM t_by_id(:tl);
\set VERBOSITY sqlstate
SELECT * FROM t;
\set VERBOSITY default
SET kingres_shard.forwarded = off;

-- A forwarded session of a one-group cluster runs anything: every bucket is
-- local.
UPDATE kingres.bucket_map SET group_id = 0;
DELETE FROM kingres.node WHERE node_id = 2;
SELECT count(*) AS all_rows_local FROM t;
SELECT count(*) AS all_rows_local_w FROM w WHERE name = :'wr' COLLATE ci;

DROP VIEW t_view;
DROP TABLE ctas2, plain;
DROP FUNCTION t_count(), t_rows(), t_by_id(int), t_v(int), t_count_pl(),
  t_one_pl(int), plain_trg(), t_par(int);
DROP TABLE w, wc, t;
DROP COLLATION ci;
DROP EXTENSION kingres_shard;
