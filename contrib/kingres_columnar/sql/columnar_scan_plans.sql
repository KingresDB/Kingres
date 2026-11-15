-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_scan_plans: the Columnar Scan in EXPLAIN, the columns it projects,
-- the stripes and chunk groups it skips for each form of condition, and that
-- every answer is the answer of the same data in a heap table, with the custom
-- scan off and with skipping off.
SET search_path = public;
SET max_parallel_workers_per_gather = 0;

-- 4 stripes of 5000 rows, 5 chunk groups of 1000 rows in each; id runs from 1 to 20000.
-- nn is NULL in the whole first stripe and in one row of 1000 elsewhere.
CREATE TABLE sp (id int, grp int, nn int, d date, ts timestamptz, txt text,
                 txtc text COLLATE "C", f float8, n numeric, big bigint) USING columnar;
SELECT kingres.columnar_set_options('sp', stripe_row_count => 5000, chunk_row_count => 1000);
INSERT INTO sp
SELECT i, i % 10, CASE WHEN i <= 5000 OR i % 1000 = 7 THEN NULL ELSE i END,
       date '2020-01-01' + i / 10, timestamptz '2020-01-01 00:00:00+00' + i * interval '1 minute',
       'k' || lpad(i::text, 6, '0'), 'k' || lpad(i::text, 6, '0'), i / 4.0, i, i::bigint * 1000000
  FROM generate_series(1, 20000) i;
SELECT stripes, chunk_groups, rows FROM kingres.columnar_info('sp');
CREATE TABLE sp_heap AS SELECT * FROM sp;
ANALYZE sp;
ANALYZE sp_heap;

-- the plan lines of the Columnar Scan, with the byte count reduced to "none" or "some"
CREATE FUNCTION kc_scan(q text) RETURNS SETOF text LANGUAGE plpgsql AS $$
DECLARE l text;
BEGIN
    FOR l IN EXECUTE 'EXPLAIN (analyze, costs off, timing off, summary off, buffers off) ' || q LOOP
        IF l ~ 'Compressed Bytes Read' THEN
            l := regexp_replace(l, 'Read: 0 bytes', 'Read: none');
            l := regexp_replace(l, 'Read: [1-9][0-9]* bytes', 'Read: some');
            RETURN NEXT btrim(l);
        ELSIF l ~ 'Columnar|Custom Scan|Seq Scan|Rows Removed' THEN
            RETURN NEXT btrim(l);
        END IF;
    END LOOP;
END $$;

-- the same condition on the columnar table, on the heap table, with the custom scan off
-- and with skipping off: the number of rows and the sum of id must agree
CREATE FUNCTION kc_agree(cond text) RETURNS TABLE (n bigint, id_sum bigint, agree boolean) LANGUAGE plpgsql AS $$
DECLARE
    r record;
    a record; b record; c record; d record;
BEGIN
    EXECUTE 'SELECT count(*) AS n, coalesce(sum(id), 0) AS s FROM sp WHERE ' || cond INTO a;
    EXECUTE 'SELECT count(*) AS n, coalesce(sum(id), 0) AS s FROM sp_heap WHERE ' || cond INTO b;
    PERFORM set_config('kingres_columnar.enable_custom_scan', 'off', true);
    EXECUTE 'SELECT count(*) AS n, coalesce(sum(id), 0) AS s FROM sp WHERE ' || cond INTO c;
    PERFORM set_config('kingres_columnar.enable_custom_scan', 'on', true);
    PERFORM set_config('kingres_columnar.enable_chunk_skipping', 'off', true);
    EXECUTE 'SELECT count(*) AS n, coalesce(sum(id), 0) AS s FROM sp WHERE ' || cond INTO d;
    PERFORM set_config('kingres_columnar.enable_chunk_skipping', 'on', true);
    n := a.n; id_sum := a.s;
    agree := (a.n, a.s) IS NOT DISTINCT FROM (b.n, b.s) AND (a.n, a.s) IS NOT DISTINCT FROM (c.n, c.s)
             AND (a.n, a.s) IS NOT DISTINCT FROM (d.n, d.s);
    RETURN NEXT;
END $$;

-- projection
SELECT * FROM kc_scan('SELECT * FROM sp');
SELECT * FROM kc_scan('SELECT count(*) FROM sp');
SELECT * FROM kc_scan('SELECT id FROM sp');
SELECT * FROM kc_scan('SELECT id, txt FROM sp WHERE grp = 3');
SELECT * FROM kc_scan('SELECT sum(f) FROM sp WHERE n > 19990');
SELECT * FROM kc_scan('SELECT max(id) FROM sp GROUP BY grp ORDER BY grp') WHERE kc_scan ~ 'Projected';
SELECT * FROM kc_scan('SELECT t FROM sp t WHERE t.id = 1');
SELECT * FROM kc_scan('SELECT tableoid::regclass, id FROM sp WHERE id = 1');
EXPLAIN (costs off) SELECT id, d FROM sp WHERE grp = 3;
EXPLAIN (costs off, verbose) SELECT id, d FROM sp WHERE grp = 3;
EXPLAIN (costs off, format json) SELECT id FROM sp WHERE id = 5;
SELECT count(*) FROM sp WHERE tableoid = 'sp'::regclass;
-- the custom scan is a choice of the planner: off, the access method scan is used
SET kingres_columnar.enable_custom_scan = off;
EXPLAIN (costs off) SELECT id FROM sp WHERE id = 5;
SELECT count(*), sum(id), sum(n) FROM sp WHERE id < 100;
RESET kingres_columnar.enable_custom_scan;
-- no worker is used: the scan is not parallel
SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; SET min_parallel_table_scan_size = 0;
SET max_parallel_workers_per_gather = 4;
EXPLAIN (costs off) SELECT count(*) FROM sp;
EXPLAIN (costs off) SELECT count(*), sum(f) FROM sp WHERE grp = 1;
SELECT count(*) FROM sp WHERE grp = 1;
RESET parallel_setup_cost; RESET parallel_tuple_cost; RESET min_parallel_table_scan_size;
SET max_parallel_workers_per_gather = 0;

-- skipping for every form of condition
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = 12345');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE 12345 = id');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id < 3000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id <= 5000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id <= 5001');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id > 19000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id >= 19001');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id >= 20000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id > 20000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id < 1');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE 3000 > id');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id <> 5');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id != 12345');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id BETWEEN 5001 AND 5010');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id BETWEEN 4500 AND 5500');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id < 100 AND id > 200');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id < 100 OR id > 19900');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id < 100 OR grp = 5');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id < 100 OR id + 1 = 12345');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id < 100 AND id + 1 = 12345');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE NOT (id < 15000)');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE NOT (id < 15000 OR id > 16000)');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE NOT (id <> 5)');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = ANY (ARRAY[1, 10000, 20000])');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id IN (1, 2, 3)');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id IN (7000, 7001, 12000)');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id IN (100000, 200000)');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id IN (NULL, 5)');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = ANY (ARRAY[]::int[])');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = ANY (NULL::int[])');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id NOT IN (1, 2, 3)');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = ALL (ARRAY[5, 5])');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = NULL');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id IS NULL');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id IS NOT NULL');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE nn IS NULL');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE nn IS NOT NULL');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE nn IS NULL AND id > 10000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE nn IS NULL AND id > 10000 AND id < 11000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE nn = 3');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE nn < 6000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE nn IS NULL OR nn > 19999');

-- cross-type constants: the column keeps its type
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = 12345::bigint');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id < 5000000000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = 5000000000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = 12345::int2');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = 12345.0');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = 12345::numeric');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id::bigint = 12345');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id < 100.5::float8');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE big = 5000000000');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE big > 19999999999::int8 AND id > 1');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE big > 19999999');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE f > 4999.5');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE f = 5000.25');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE f = 5000.25::float4');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE f = ''NaN''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE f < 0');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE n = 5');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE n < 0.5');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE n > 19999.5');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE d = ''2020-02-01''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE d = ''2020-02-01''::timestamptz');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE d >= ''2025-01-01''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE ts = ''2020-01-01 00:05:00+00''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE ts >= ''2020-01-10 00:00:00+00''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE ts >= ''2020-01-10 00:00:00+00''::timestamp');

-- strings: the C collation has statistics, the others do not
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE txtc = ''k012345''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE txtc < ''k005000''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE txtc > ''k019999''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE txtc LIKE ''k0123%''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE txtc COLLATE "C" = ''k012345''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE txtc COLLATE "POSIX" = ''k012345''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE txt COLLATE "C" = ''k012345''');
-- the default collation of the database: statistics exist only if it is C or POSIX
SELECT (SELECT count(*) FROM kc_scan('SELECT count(*) FROM sp WHERE txt = ''k012345''') WHERE kc_scan ~ 'Skip Conditions')
       = (SELECT (datcollate IN ('C', 'POSIX'))::int FROM pg_database WHERE datname = current_database()) AS default_collation_rule;

-- stable functions and parameters
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE ts > now()');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE ts < now() - interval ''100 years''');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE ts < now()');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE d > current_date');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = (SELECT 12345)');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = length(repeat(''x'', 12345))');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = (random() * 0)::int');
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = grp');
SET plan_cache_mode = force_generic_plan;
PREPARE q_eq (int) AS SELECT count(*) FROM sp WHERE id = $1;
SELECT * FROM kc_scan('EXECUTE q_eq(12345)');
SELECT * FROM kc_scan('EXECUTE q_eq(7)');
SELECT * FROM kc_scan('EXECUTE q_eq(NULL)');
EXECUTE q_eq(12345);
EXECUTE q_eq(1);
EXECUTE q_eq(-1);
EXECUTE q_eq(NULL);
PREPARE q_in (int[]) AS SELECT count(*) FROM sp WHERE id = ANY ($1);
SELECT * FROM kc_scan('EXECUTE q_in(ARRAY[1, 20000])');
EXECUTE q_in(ARRAY[1, 20000]);
EXECUTE q_in(ARRAY[0, 20001]);
PREPARE q_rng (int, int) AS SELECT count(*), sum(id) FROM sp WHERE id BETWEEN $1 AND $2;
EXECUTE q_rng(100, 200);
EXECUTE q_rng(5000, 5001);
EXECUTE q_rng(200, 100);
EXECUTE q_rng(NULL, 100);
SET plan_cache_mode = force_custom_plan;
EXECUTE q_eq(12345);
SELECT * FROM kc_scan('EXECUTE q_eq(12345)');
RESET plan_cache_mode;
DEALLOCATE ALL;

-- the switches
SET kingres_columnar.enable_chunk_skipping = off;
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = 12345');
RESET kingres_columnar.enable_chunk_skipping;
SET kingres_columnar.enable_custom_scan = off;
SELECT * FROM kc_scan('SELECT count(*) FROM sp WHERE id = 12345');
RESET kingres_columnar.enable_custom_scan;

-- the answers do not depend on skipping or on the scan (count, sum of id, all four agree)
SELECT cond, a.* FROM unnest(ARRAY[
  'id = 12345', 'id = 0', 'id = 20000', 'id <= 1', 'id < 1', 'id >= 20000', 'id > 20000', 'id <> 5',
  'id BETWEEN 4999 AND 5001', 'id < 100 OR id > 19900', 'NOT (id < 15000)', 'NOT (id = 5)',
  'id = ANY (ARRAY[1, 5000, 5001, 20000])', 'id IN (NULL, 5)', 'id NOT IN (1, 2, 3)', 'id = ANY (NULL::int[])',
  'nn IS NULL', 'nn IS NOT NULL', 'nn IS NULL AND id > 15000', 'nn = 3', 'nn < 6000', 'nn >= 20000',
  'id = 12345::bigint', 'id < 5000000000', 'id::numeric = 5', 'id = 5.0', 'big = 5000000000', 'big > 19999999999',
  'f > 4999.5', 'f = 5000.25', 'f < 0', 'n = 5', 'n < 0.5', 'n > 19999.5', 'n IN (1.0, 2, 20000)',
  'd = ''2020-02-01''', 'd >= ''2025-01-01''', 'd < ''2020-01-01''', 'ts = ''2020-01-01 00:05:00+00''', 'ts >= ''2020-01-10 00:00:00+00''',
  'ts < now()', 'ts > now()', 'd BETWEEN current_date - 10000 AND current_date',
  'txtc = ''k012345''', 'txtc < ''k005000''', 'txtc > ''k019999''', 'txtc LIKE ''k0123%''', 'txt = ''k012345''', 'txt < ''k000100''',
  'id = grp', 'id % 1000 = 0', 'grp = 3 AND id < 5000', 'grp = 3 OR id > 19990', 'false', 'true'
]) AS cond, LATERAL kc_agree(cond) a ORDER BY cond COLLATE "C";

-- a join, an aggregate, LIMIT
CREATE TABLE dim AS SELECT g AS grp, 'g' || g AS label FROM generate_series(0, 9) g;
ANALYZE dim;
SET enable_mergejoin = off; SET enable_nestloop = off;
EXPLAIN (costs off) SELECT count(*), sum(id) FROM sp JOIN dim USING (grp) WHERE id < 3000;
SELECT label, count(*), sum(id) FROM sp JOIN dim USING (grp) WHERE id < 3000 GROUP BY label ORDER BY label;
RESET enable_mergejoin; RESET enable_nestloop;
SELECT (SELECT sum(c) FROM (SELECT label, count(*) c FROM sp JOIN dim USING (grp) GROUP BY label) x) AS joined_rows;
EXPLAIN (costs off) SELECT id FROM sp WHERE id > 100 LIMIT 5;
SELECT id FROM sp WHERE id > 100 LIMIT 5;
SELECT * FROM kc_scan('SELECT id FROM sp WHERE id > 100 LIMIT 5');
SELECT id FROM sp WHERE id > 7000 ORDER BY id LIMIT 3 OFFSET 2;
SELECT grp, count(*), min(id), max(id), round(avg(f)::numeric, 2) FROM sp GROUP BY grp ORDER BY grp;
SELECT count(*), count(DISTINCT grp), count(nn) FROM sp;
SELECT count(*) FROM (SELECT id FROM sp WHERE id < 100 UNION ALL SELECT id FROM sp WHERE id > 19900) u;
SELECT count(*) FROM sp WHERE id IN (SELECT id FROM sp_heap WHERE grp = 4 AND id < 100);
SELECT count(*) FROM sp WHERE EXISTS (SELECT 1 FROM sp_heap h WHERE h.id = sp.id AND h.grp = 9 AND h.id > 19000);

-- rescan under a nested loop: the condition changes with each outer row
SET enable_hashjoin = off; SET enable_mergejoin = off; SET enable_material = off;
CREATE TABLE outer_t AS SELECT * FROM (VALUES (1, 100), (2, 7000), (3, 12345), (4, 20000), (5, -5), (6, 7000), (7, NULL)) v(k, x);
EXPLAIN (costs off) SELECT o.k, c.n FROM outer_t o CROSS JOIN LATERAL (SELECT count(*) AS n FROM sp WHERE sp.id = o.x) c ORDER BY o.k;
SELECT o.k, (SELECT count(*) FROM sp WHERE sp.id = o.x) AS c FROM outer_t o ORDER BY o.k;
SELECT o.k, c.n FROM outer_t o CROSS JOIN LATERAL (SELECT count(*) AS n FROM sp WHERE sp.id BETWEEN o.x - 1 AND o.x + 1) c ORDER BY o.k;
SELECT o.k, c.n FROM outer_t o CROSS JOIN LATERAL (SELECT count(*) AS n FROM sp_heap WHERE sp_heap.id BETWEEN o.x - 1 AND o.x + 1) c ORDER BY o.k;
EXPLAIN (costs off) SELECT o.k, s.id FROM outer_t o JOIN sp s ON s.id = o.x ORDER BY o.k;
SELECT o.k, s.id FROM outer_t o JOIN sp s ON s.id = o.x ORDER BY o.k;
SELECT count(*) FROM outer_t o JOIN sp s ON s.id = o.x AND s.grp = o.k;
SELECT count(*) FROM outer_t o JOIN sp_heap s ON s.id = o.x AND s.grp = o.k;
-- the same through a function that runs the inner query once for each outer row
SELECT o.k, a.* FROM outer_t o, LATERAL kc_agree('id = ' || coalesce(o.x::text, 'NULL')) a ORDER BY o.k;
RESET enable_hashjoin; RESET enable_mergejoin; RESET enable_material;

-- a partitioned table with columnar and heap partitions
CREATE TABLE pt (id int, v int) PARTITION BY RANGE (id);
CREATE TABLE pt_c1 PARTITION OF pt FOR VALUES FROM (0) TO (1000) USING columnar;
CREATE TABLE pt_c2 PARTITION OF pt FOR VALUES FROM (1000) TO (2000) USING columnar;
CREATE TABLE pt_h PARTITION OF pt FOR VALUES FROM (2000) TO (3000);
INSERT INTO pt SELECT i, i FROM generate_series(0, 2999) i;
EXPLAIN (costs off) SELECT count(*) FROM pt;
EXPLAIN (costs off) SELECT v FROM pt WHERE id = 1500;
EXPLAIN (costs off) SELECT v FROM pt WHERE id = 1500 OR id = 2500;
SELECT * FROM kc_scan('SELECT count(*) FROM pt WHERE id >= 1500 AND v < 1600');
SELECT count(*), sum(v) FROM pt WHERE id BETWEEN 990 AND 2010;
SELECT tableoid::regclass, count(*) FROM pt GROUP BY 1 ORDER BY 1;

DROP TABLE sp, sp_heap, dim, outer_t, pt;
DROP FUNCTION kc_scan(text), kc_agree(text);
