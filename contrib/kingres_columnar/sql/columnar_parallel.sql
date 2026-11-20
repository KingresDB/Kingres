-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_parallel: the plans of the parallel Columnar Scan (C5, design 8.4):
-- where it is offered and where it is not, and that every answer is the answer
-- of the same rows in a heap table.  The runs of workers (who read which
-- stripe, a writer in the middle, a standby) are in t/043_parallel.pl.
SET search_path = public;
SET parallel_setup_cost = 0;
SET parallel_tuple_cost = 0;
SET min_parallel_table_scan_size = 0;
SET max_parallel_workers_per_gather = 3;
SET parallel_leader_participation = on;

-- 24 stripes of 1000 rows
CREATE TABLE pc (id int, grp int, v float8, t text) USING columnar;
SELECT kingres.columnar_set_options('pc', stripe_row_count => 1000, chunk_row_count => 250);
INSERT INTO pc SELECT i, i % 10, i / 4.0, 'k' || i FROM generate_series(1, 24000) i;
CREATE TABLE pc_heap AS SELECT * FROM pc;
-- one stripe
CREATE TABLE pc1 (id int, v int) USING columnar;
INSERT INTO pc1 SELECT i, i FROM generate_series(1, 500) i;
ANALYZE pc;
ANALYZE pc_heap;
ANALYZE pc1;

-- the partial path under a Gather, with projection and skipping in the node
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc;
EXPLAIN (COSTS OFF) SELECT sum(v) FROM pc WHERE id > 20000;
EXPLAIN (COSTS OFF) SELECT grp, count(*) FROM pc GROUP BY grp;
-- a table of one stripe has nobody to share it with
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc1;
-- the number of workers follows the table option, and never exceeds the stripes - 1
ALTER TABLE pc SET (parallel_workers = 2);
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc;
ALTER TABLE pc SET (parallel_workers = 30);
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc;
ALTER TABLE pc RESET (parallel_workers);
CREATE TABLE pc3 (id int, v int) USING columnar;
SELECT kingres.columnar_set_options('pc3', stripe_row_count => 1000, chunk_row_count => 250);
INSERT INTO pc3 SELECT i, i FROM generate_series(1, 2500) i;
ANALYZE pc3;
ALTER TABLE pc3 SET (parallel_workers = 30);
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc3;
SELECT count(*), sum(v) FROM pc3;
-- off: the access method scan is not parallel, so no worker is used
SET kingres_columnar.enable_custom_scan = off;
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc;
RESET kingres_columnar.enable_custom_scan;
SET max_parallel_workers_per_gather = 0;
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc;
SET max_parallel_workers_per_gather = 3;

-- joins: a partial scan on each side (Parallel Hash)
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc a JOIN pc b USING (id);
-- (the side of a join with a heap table that is hashed is the planner's choice, which differs between majors)
SELECT count(*), sum(a.v) FROM pc a JOIN pc_heap b USING (id) WHERE a.id % 7 = 0;

-- partitions: Parallel Append over columnar and heap leaves
CREATE TABLE pp (k int, v int) PARTITION BY RANGE (k);
CREATE TABLE pp_c1 PARTITION OF pp FOR VALUES FROM (0) TO (12000) USING columnar;
CREATE TABLE pp_c2 PARTITION OF pp FOR VALUES FROM (12000) TO (24000) USING columnar;
CREATE TABLE pp_h PARTITION OF pp FOR VALUES FROM (24000) TO (36000);
SELECT kingres.columnar_set_options('pp_c1', stripe_row_count => 1000, chunk_row_count => 250);
SELECT kingres.columnar_set_options('pp_c2', stripe_row_count => 1000, chunk_row_count => 250);
INSERT INTO pp SELECT i, i FROM generate_series(0, 35999) i;
ANALYZE pp;
EXPLAIN (COSTS OFF) SELECT count(*) FROM pp;

-- every answer is the heap twin's, with workers, one worker, none, a single copy
SET parallel_leader_participation = off;
SELECT count(*), sum(id), sum(v), count(DISTINCT t) FROM pc;
SELECT count(*), sum(id), sum(v), count(DISTINCT t) FROM pc_heap;
SELECT grp, count(*), sum(id) FROM pc GROUP BY grp ORDER BY grp;
SELECT grp, count(*), sum(id) FROM pc_heap GROUP BY grp ORDER BY grp;
SELECT count(*), sum(a.v) FROM pc a JOIN pc b USING (id) WHERE a.id % 3 = 0;
SELECT count(*), sum(a.v) FROM pc_heap a JOIN pc_heap b USING (id) WHERE a.id % 3 = 0;
SELECT count(*), sum(v) FROM pp;
SELECT count(*), sum(v) FROM pp_c1;
RESET parallel_leader_participation;
SET max_parallel_workers_per_gather = 1;
SELECT count(*), sum(id) FROM pc WHERE id BETWEEN 5000 AND 9000;
SELECT count(*), sum(id) FROM pc_heap WHERE id BETWEEN 5000 AND 9000;
SET max_parallel_workers_per_gather = 0;
SELECT count(*), sum(id) FROM pc WHERE id BETWEEN 5000 AND 9000;
SET max_parallel_workers_per_gather = 2;
SET debug_parallel_query = on;
SELECT count(*), sum(id) FROM pc WHERE id BETWEEN 5000 AND 9000;
SELECT count(*), sum(v) FROM pp;
EXPLAIN (COSTS OFF) SELECT id FROM pc1 WHERE id = 7;
SELECT id FROM pc1 WHERE id = 7;
RESET debug_parallel_query;

-- the rows this transaction wrote, in the current (sub)transaction, are written as stripes
-- before the workers start: the answer has them
BEGIN;
INSERT INTO pc SELECT 100000 + i, 1, 1, 'new' FROM generate_series(1, 700) i;
SELECT count(*), count(*) FILTER (WHERE id > 100000) FROM pc;
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc;
COMMIT;
BEGIN;
INSERT INTO pc SELECT 100000 + i, 1, 1, 'new' FROM generate_series(1, 700) i;
SAVEPOINT s1;
INSERT INTO pc SELECT 200000 + i, 2, 2, 'sub' FROM generate_series(1, 300) i;
-- rows of the enclosing level cannot be given to workers: the plan has none
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc;
SELECT count(*), count(*) FILTER (WHERE id > 100000) FROM pc;
ROLLBACK TO s1;
-- the level's buffer is empty again, the enclosing level still has its rows
EXPLAIN (COSTS OFF) SELECT count(*) FROM pc;
SELECT count(*), count(*) FILTER (WHERE id > 100000) FROM pc;
RELEASE s1;
COMMIT;
SELECT count(*) FROM pc;

-- a plan made before, with parallel workers, meets rows of an enclosing level
SET plan_cache_mode = force_generic_plan;
PREPARE pcq AS SELECT count(*) FROM pc;
EXECUTE pcq;
BEGIN;
INSERT INTO pc SELECT 300000 + i, 3, 3, 'late' FROM generate_series(1, 10) i;
SAVEPOINT s2;
\set VERBOSITY terse
EXECUTE pcq;
\set VERBOSITY default
ROLLBACK;
EXECUTE pcq;
RESET plan_cache_mode;
DEALLOCATE pcq;

-- other things that need a plain scan are unchanged
EXPLAIN (COSTS OFF) SELECT * FROM pc TABLESAMPLE SYSTEM (10);

-- a parallel-safe function that reads the table in a worker is still refused
CREATE FUNCTION pc_count() RETURNS bigint LANGUAGE sql PARALLEL SAFE AS 'SELECT count(*) FROM pc';
SET debug_parallel_query = on;
\set VERBOSITY terse
SELECT count(pc_count()) FROM pc1;
\set VERBOSITY default
RESET debug_parallel_query;

DROP TABLE pp, pc, pc1, pc3, pc_heap;
