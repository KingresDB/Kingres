-- contrib/kingres_columnar/perf/shapes.sql
--
-- The four data shapes of the size and scan-speed measurement (perf.sh).  Each
-- shape is first built as a plain heap table "src_<shape>" from fixed seeds,
-- and perf.sh then copies it into a heap table and into one columnar table
-- per codec, so every variant stores exactly the same rows.
--
-- Variables (psql -v): ts_rows, wide_rows, rtext_rows, rep_rows.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

\set ON_ERROR_STOP 1
SELECT setseed(0.42);

-- 1. A time series of 20 columns: the timestamp ascends, the device id has a
--    low cardinality, a few float8 measurements (one with NULLs), a few
--    low-cardinality text labels, small integers, a flag, and a note that is
--    mostly NULL.
CREATE TABLE src_ts AS
SELECT timestamp '2024-01-01' + g * interval '1 second'              AS t,
       (g % 500)::int                                                AS dev,
       round((20 + 10 * random())::numeric, 3)::float8               AS m1,
       random() * 100                                                AS m2,
       (g % 1000) / 10.0::float8                                     AS m3,
       random()                                                      AS m4,
       CASE WHEN g % 10 = 0 THEN NULL ELSE random() * 1000 END       AS m5,
       (ARRAY['eu-west', 'eu-east', 'us-east', 'us-west', 'ap-south'])[1 + g % 5]  AS l1,
       (ARRAY['rack-a', 'rack-b', 'rack-c'])[1 + (g / 7) % 3]       AS l2,
       (ARRAY['ok', 'ok', 'ok', 'degraded', 'down'])[1 + (g / 3) % 5] AS l3,
       (ARRAY['v1.2', 'v1.3', 'v2.0'])[1 + (g / 100000) % 3]        AS l4,
       (g % 17)::int                                                 AS i1,
       (g % 1013)::int                                               AS i2,
       (random() * 100000)::int                                      AS i3,
       (g / 1000)::int                                               AS i4,
       (g % 3 = 0)                                                   AS b1,
       (g % 5)::smallint                                             AS st,
       g::bigint                                                     AS v1,
       round((random() * 1000)::numeric, 2)                          AS n1,
       CASE WHEN g % 50 = 0 THEN 'maintenance window ' || (g / 50) END AS note
  FROM generate_series(1, :ts_rows) g;

-- 2. A wide table: 100 columns of ints, floats and short text.
SELECT 'CREATE TABLE src_wide AS SELECT '
       || string_agg(CASE i % 3
                       WHEN 0 THEN format('((g * %s) %% 1000)::int AS c%s', i, i)
                       WHEN 1 THEN format('((g * %s) %% 100000) / 7.0::float8 AS c%s', i, i)
                       ELSE format('substr(md5((g * %s)::text), 1, 8) AS c%s', i, i)
                     END, ', ' ORDER BY i)
       || format(' FROM generate_series(1, %s) g', :wide_rows)
  FROM generate_series(1, 100) i
\gexec

-- 3. Random text: md5 strings, which do not repeat.
CREATE TABLE src_rtext AS
SELECT g AS id,
       md5(g::text) AS s1,
       md5((g * 7)::text) || md5((g * 13)::text) AS s2
  FROM generate_series(1, :rtext_rows) g;

-- 4. Highly repetitive data: long runs of the same key, constant labels.
CREATE TABLE src_rep AS
SELECT g AS id,
       (g / 100000)::int AS k,
       (g % 10)::int AS v,
       'status-ok-and-nothing-to-report' AS label,
       (g % 2 = 0) AS flag,
       'constant' AS tag
  FROM generate_series(1, :rep_rows) g;

ANALYZE src_ts;
ANALYZE src_wide;
ANALYZE src_rtext;
ANALYZE src_rep;
