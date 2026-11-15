-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_analyze: ANALYZE samples a table of several stripes by block.  The
-- statistics are compared with the exact values and with the heap table of
-- the same data, within tolerances; no random number is printed.
SET search_path = public;
SET max_parallel_workers_per_gather = 0;

-- 200000 rows, 4 stripes of 50000 rows, 20 chunk groups
CREATE TABLE an_c (id int, grp int, skew int, nl int, txt text, ts timestamptz, f float8) USING columnar;
SELECT kingres.columnar_set_options('an_c', stripe_row_count => 50000, chunk_row_count => 10000);
INSERT INTO an_c
SELECT i, i % 7, CASE WHEN i % 2 = 0 THEN 1 ELSE i END, CASE WHEN i % 10 = 0 THEN NULL ELSE i END,
       'row' || lpad(i::text, 17, '0'), timestamptz '2020-01-01' + i * interval '1 minute', (i % 1000) / 10.0
  FROM generate_series(1, 200000) i;
CREATE TABLE an_h AS SELECT * FROM an_c;
SELECT stripes, chunk_groups, rows FROM kingres.columnar_info('an_c');
-- before ANALYZE nothing is known
SELECT relpages, reltuples FROM pg_class WHERE oid = 'an_c'::regclass;
SELECT count(*) AS stats_rows FROM pg_stats WHERE tablename = 'an_c';

ANALYZE an_c;
ANALYZE an_h;
SELECT relpages > 0 AS has_pages, abs(reltuples - 200000) < 20000 AS reltuples_close,
       abs(reltuples - (SELECT reltuples FROM pg_class WHERE oid = 'an_h'::regclass)) < 20000 AS like_heap
  FROM pg_class WHERE oid = 'an_c'::regclass;
SELECT count(*) AS stats_rows FROM pg_stats WHERE tablename = 'an_c';

-- one line for each column, against the exact values
CREATE VIEW an_stat AS
SELECT s.attname, s.null_frac, s.avg_width, s.n_distinct, s.correlation,
       s.most_common_vals::text::text[] AS mcv, s.most_common_freqs AS mcf, s.histogram_bounds::text::text[] AS hb
  FROM pg_stats s WHERE s.tablename = 'an_c' AND NOT s.inherited;
SELECT attname,
       abs(null_frac - CASE attname WHEN 'nl' THEN 0.1 ELSE 0 END) < 0.02 AS null_frac_ok,
       avg_width AS width,
       CASE attname WHEN 'txt' THEN avg_width = 21 WHEN 'f' THEN avg_width = 8 WHEN 'ts' THEN avg_width = 8 ELSE avg_width = 4 END AS width_ok
  FROM an_stat ORDER BY attname;
-- id is unique and in order
SELECT n_distinct = -1 AS unique_column, correlation > 0.99 AS in_order, array_length(hb, 1) AS bounds,
       hb[1]::int < 1000 AS first_low, hb[101]::int > 199000 AS last_high
  FROM an_stat WHERE attname = 'id';
SELECT min(hb[i + 1]::int - hb[i]::int) > 1000 AND max(hb[i + 1]::int - hb[i]::int) < 3000 AS bounds_evenly_spaced
  FROM an_stat, generate_series(1, 100) i WHERE attname = 'id';
SELECT n_distinct = -1 AS unique_column, correlation > 0.99 AS in_order, array_length(hb, 1) AS bounds FROM an_stat WHERE attname IN ('ts', 'txt') ORDER BY attname;
-- grp: 7 values, each one seventh
SELECT n_distinct AS distinct_values, array_length(mcv, 1) AS common_values, hb IS NULL AS no_histogram,
       (SELECT min(f) > 0.13 AND max(f) < 0.157 FROM unnest(mcf) f) AS frequencies_ok,
       abs(correlation) < 0.5 AS not_in_order
  FROM an_stat WHERE attname = 'grp';
-- skew: half of the rows have one value
SELECT mcv[1] AS most_common, mcf[1] > 0.45 AND mcf[1] < 0.55 AS frequency_ok, array_length(mcv, 1) AS common_values FROM an_stat WHERE attname = 'skew';
-- f: 1000 values
SELECT n_distinct AS distinct_values, array_length(mcv, 1) > 0 AS has_common, abs(correlation) < 0.1 AS not_in_order FROM an_stat WHERE attname = 'f';
-- the statistics are those of a heap table with the same data
SELECT c.attname,
       abs(c.null_frac - h.null_frac) < 0.02 AS null_frac_like_heap,
       c.avg_width = h.avg_width AS width_like_heap,
       CASE WHEN h.n_distinct > 0 THEN c.n_distinct = h.n_distinct
            ELSE c.n_distinct < 0 AND abs(c.n_distinct - h.n_distinct) < 0.05 END AS distinct_like_heap,
       abs(coalesce(c.correlation, 0) - coalesce(h.correlation, 0)) < 0.05 AS correlation_like_heap,
       (c.histogram_bounds IS NULL) = (h.histogram_bounds IS NULL) AS histogram_like_heap
  FROM pg_stats c JOIN pg_stats h ON h.attname = c.attname AND h.tablename = 'an_h' AND NOT h.inherited
 WHERE c.tablename = 'an_c' AND NOT c.inherited ORDER BY c.attname;

-- what the planner estimates from them
CREATE FUNCTION an_rows(q text) RETURNS int LANGUAGE plpgsql AS $$
DECLARE l text;
BEGIN
    FOR l IN EXECUTE 'EXPLAIN ' || q LOOP
        IF l ~ 'rows=' THEN RETURN substring(l FROM 'rows=([0-9]+)')::int; END IF;
    END LOOP;
END $$;
SELECT an_rows('SELECT * FROM an_c') BETWEEN 180000 AND 220000 AS all_rows,
       an_rows('SELECT * FROM an_c WHERE grp = 3') BETWEEN 25000 AND 32000 AS one_seventh,
       an_rows('SELECT * FROM an_c WHERE id < 1000') BETWEEN 500 AND 2500 AS first_thousand,
       an_rows('SELECT * FROM an_c WHERE nl IS NULL') BETWEEN 16000 AND 24000 AS nulls,
       an_rows('SELECT * FROM an_c WHERE skew = 1') BETWEEN 90000 AND 110000 AS half;

-- a second ANALYZE gives the same kind of numbers; ANALYZE of some columns only
ANALYZE an_c;
SELECT abs(reltuples - 200000) < 20000 AS reltuples_close FROM pg_class WHERE oid = 'an_c'::regclass;
ANALYZE an_c (grp, nl);
SELECT abs(reltuples - 200000) < 20000 AS reltuples_close FROM pg_class WHERE oid = 'an_c'::regclass;
SELECT count(*) AS stats_rows FROM pg_stats WHERE tablename = 'an_c';
-- a smaller statistics target gives a smaller histogram
SET default_statistics_target = 10;
ANALYZE an_c;
SELECT array_length(hb, 1) AS bounds FROM an_stat WHERE attname = 'id';
RESET default_statistics_target;
ALTER TABLE an_c ALTER COLUMN id SET STATISTICS 200;
ANALYZE an_c;
SELECT array_length(hb, 1) AS bounds FROM an_stat WHERE attname = 'id';
DROP VIEW an_stat;

-- a table that is read completely: the statistics are exact
CREATE TABLE an_s (id int, g int, w text) USING columnar;
INSERT INTO an_s SELECT i, i % 5, repeat('w', 3000) || i FROM generate_series(1, 500) i;
ANALYZE an_s;
SELECT reltuples FROM pg_class WHERE oid = 'an_s'::regclass;
-- (the width of w is that of the values as they are read: a heap table counts the compressed width)
SELECT attname, null_frac, avg_width, n_distinct FROM pg_stats WHERE tablename = 'an_s' ORDER BY attname;
-- extended statistics are built from the sample
CREATE STATISTICS an_s_st (ndistinct, dependencies, mcv) ON id, g FROM an_s;
ANALYZE an_s;
SELECT statistics_name, n_distinct IS NOT NULL AS has_ndistinct, dependencies IS NOT NULL AS has_dependencies,
       most_common_vals IS NOT NULL AS has_mcv
  FROM pg_stats_ext WHERE tablename = 'an_s';
-- the table grows and is analyzed again
INSERT INTO an_s SELECT i, 1, 'x' FROM generate_series(1, 1000) i;
ANALYZE an_s;
SELECT reltuples FROM pg_class WHERE oid = 'an_s'::regclass;

-- a partitioned table: the partitions and the parent
CREATE TABLE an_p (id int, v int) PARTITION BY RANGE (id);
CREATE TABLE an_p1 PARTITION OF an_p FOR VALUES FROM (0) TO (50000) USING columnar;
CREATE TABLE an_p2 PARTITION OF an_p FOR VALUES FROM (50000) TO (100000);
INSERT INTO an_p SELECT i, i % 10 FROM generate_series(0, 99999) i;
ANALYZE an_p;
SELECT tablename, inherited, n_distinct FROM pg_stats WHERE tablename LIKE 'an_p%' AND attname = 'v' ORDER BY tablename, inherited;
SELECT relname, abs(reltuples - CASE relname WHEN 'an_p' THEN 100000 ELSE 50000 END) < 5000 AS reltuples_close
  FROM pg_class WHERE relname IN ('an_p1', 'an_p2') ORDER BY relname;

-- an empty table and a table of NULLs
CREATE TABLE an_e (a int, b text) USING columnar;
ANALYZE an_e;
SELECT reltuples, relpages FROM pg_class WHERE oid = 'an_e'::regclass;
SELECT count(*) AS stats_rows FROM pg_stats WHERE tablename = 'an_e';
INSERT INTO an_e SELECT NULL, NULL FROM generate_series(1, 100);
ANALYZE an_e;
SELECT reltuples FROM pg_class WHERE oid = 'an_e'::regclass;
SELECT attname, null_frac, avg_width, n_distinct FROM pg_stats WHERE tablename = 'an_e' ORDER BY attname;
-- ANALYZE of a table with a dropped column and an added column
ALTER TABLE an_e DROP COLUMN b;
ALTER TABLE an_e ADD COLUMN c int DEFAULT 5;
ANALYZE an_e;
SELECT attname, null_frac, n_distinct, most_common_vals::text AS common FROM pg_stats WHERE tablename = 'an_e' ORDER BY attname;

DROP TABLE an_c, an_h, an_s, an_p, an_e;
DROP FUNCTION an_rows(text);
