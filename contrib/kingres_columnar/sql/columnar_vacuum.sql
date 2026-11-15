-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: PostgreSQL
--
-- columnar_vacuum: VACUUM statistics and truncation, columnar_compact,
-- columnar_check, the counters autovacuum reads, and VACUUM FULL.
SET search_path = public;

-- the statistics before and after a VACUUM
CREATE TABLE v1 (id int, s text) USING columnar;
SELECT relpages, reltuples FROM pg_class WHERE oid = 'v1'::regclass;
SELECT pg_relation_size('v1') / 8192 AS pages_empty \gset
SELECT :pages_empty AS pages_empty;
VACUUM v1;
SELECT relpages, reltuples FROM pg_class WHERE oid = 'v1'::regclass;
INSERT INTO v1 SELECT i, repeat('x', 50) || i FROM generate_series(1, 20000) i;
SELECT relpages, reltuples FROM pg_class WHERE oid = 'v1'::regclass;
SELECT pg_relation_size('v1') / 8192 AS pages_loaded \gset
SELECT :pages_loaded > :pages_empty AS grew;
VACUUM v1;
SELECT relpages = :pages_loaded AS relpages_is_file_size, reltuples FROM pg_class WHERE oid = 'v1'::regclass;
VACUUM (ANALYZE) v1;
SELECT reltuples FROM pg_class WHERE oid = 'v1'::regclass;

-- the tail of the file that no stripe uses is cut off: here after a load that rolled back
BEGIN;
INSERT INTO v1 SELECT i, repeat('y', 50) || i FROM generate_series(1, 200000) i;
ROLLBACK;
SELECT pg_relation_size('v1') / 8192 AS pages_garbage \gset
SELECT :pages_garbage > :pages_loaded * 3 AS garbage_in_file;
VACUUM (TRUNCATE false) v1;
SELECT pg_relation_size('v1') / 8192 = :pages_garbage AS not_truncated;
VACUUM v1;
SELECT pg_relation_size('v1') / 8192 = :pages_loaded AS back_to_loaded_size;
SELECT count(*), sum(id) FROM v1;
SELECT stripes, rows FROM kingres.columnar_info('v1');
-- garbage that is followed by committed data stays until VACUUM FULL
BEGIN;
INSERT INTO v1 SELECT i, repeat('y', 50) || i FROM generate_series(1, 100000) i;
ROLLBACK;
INSERT INTO v1 SELECT i, repeat('z', 50) || i FROM generate_series(1, 10) i;
SELECT pg_relation_size('v1') / 8192 AS pages_middle \gset
VACUUM v1;
SELECT pg_relation_size('v1') / 8192 = :pages_middle AS middle_kept;
VACUUM FULL v1;
SELECT pg_relation_size('v1') / 8192 < :pages_middle AS shrunk, pg_relation_size('v1') / 8192 <= :pages_loaded + 1 AS about_loaded_size;
SELECT count(*), sum(id) FROM v1;
VACUUM (FULL, ANALYZE) v1;
SELECT reltuples FROM pg_class WHERE oid = 'v1'::regclass;
-- an empty table
CREATE TABLE v0 (a int) USING columnar;
VACUUM v0;
VACUUM FULL v0;
VACUUM (ANALYZE) v0;
SELECT relpages, reltuples FROM pg_class WHERE oid = 'v0'::regclass;
TRUNCATE v1;
VACUUM v1;
SELECT relpages, reltuples, pg_relation_size('v1') / 8192 = :pages_empty AS empty_file FROM pg_class WHERE oid = 'v1'::regclass;
DROP TABLE v0, v1;

-- columnar_compact: stripes below the minimum are merged, the rows stay
CREATE TABLE v2 (id int, s text) USING columnar;
SELECT kingres.columnar_set_options('v2', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO v2 SELECT i, 'a' FROM generate_series(1, 100) i;
INSERT INTO v2 SELECT i, 'b' FROM generate_series(101, 250) i;
INSERT INTO v2 SELECT i, 'c' FROM generate_series(251, 400) i;
INSERT INTO v2 SELECT i, 'd' FROM generate_series(401, 1700) i;
SELECT count(*), sum(id), md5(string_agg(id || s, ',' ORDER BY id)) AS digest FROM v2 \gset
SELECT stripe_id, row_count FROM kingres.columnar_stripes('v2') ORDER BY stripe_id;
SELECT kingres.columnar_compact('v2') AS stripes_written;
SELECT stripe_id, row_count FROM kingres.columnar_stripes('v2') ORDER BY stripe_id;
SELECT count(*) = :count AND sum(id) = :sum AND md5(string_agg(id || s, ',' ORDER BY id)) = :'digest' AS same_content FROM v2;
-- nothing is left below the default minimum (a quarter of the stripe size)
SELECT kingres.columnar_compact('v2') AS stripes_written;
-- a larger minimum merges more; the stripes are filled up to the stripe size
SELECT kingres.columnar_compact('v2', 5000) AS stripes_written;
SELECT stripe_id, row_count, chunk_groups FROM kingres.columnar_stripes('v2') ORDER BY stripe_id;
SELECT count(*) = :count AND sum(id) = :sum AND md5(string_agg(id || s, ',' ORDER BY id)) = :'digest' AS same_content FROM v2;
SELECT kingres.columnar_compact('v2', 1) AS stripes_written;
SELECT kingres.columnar_compact('v2', 0);
SELECT kingres.columnar_compact('v2', -1);
-- the merged stripes are written in the transaction and rolled back with it
INSERT INTO v2 SELECT i, 'e' FROM generate_series(1, 10) i;
BEGIN;
SELECT kingres.columnar_compact('v2', 5000) AS stripes_written;
SELECT stripes, rows FROM kingres.columnar_info('v2');
ROLLBACK;
SELECT stripes, rows FROM kingres.columnar_info('v2');
SELECT count(*), sum(id) FROM v2;
-- the old stripes stay in the file until VACUUM FULL
SELECT kingres.columnar_compact('v2', 5000) AS stripes_written;
SELECT pg_relation_size('v2') / 8192 AS pages_after_compact \gset
VACUUM v2;
SELECT pg_relation_size('v2') / 8192 >= :pages_after_compact - 1 AS not_much_smaller;
VACUUM FULL v2;
SELECT pg_relation_size('v2') / 8192 < :pages_after_compact AS smaller;
SELECT stripe_id, row_count FROM kingres.columnar_stripes('v2') ORDER BY stripe_id;
SELECT count(*), sum(id) FROM v2;
-- a table with one stripe, an empty table and a heap table
CREATE TABLE v2e (a int) USING columnar;
SELECT kingres.columnar_compact('v2e');
INSERT INTO v2e VALUES (1);
SELECT kingres.columnar_compact('v2e');
CREATE TABLE v2h (a int);
SELECT kingres.columnar_compact('v2h');
DROP TABLE v2, v2e, v2h;

-- columnar_check: a catalog row of a file nobody uses, a stripe outside the file, a wrong first page
CREATE TABLE v3 (id int, s text) USING columnar;
SELECT kingres.columnar_set_options('v3', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO v3 SELECT i, 'a' FROM generate_series(1, 1000) i;
INSERT INTO v3 SELECT i, 'b' FROM generate_series(1001, 1500) i;
SELECT * FROM kingres.columnar_check();
INSERT INTO kingres.columnar_stripe VALUES (999999, 1, 0, 1, 1, 1, 1, 1, 1, 1, 0, '\x', '\x');
SELECT check_name, relation, detail FROM kingres.columnar_check();
SELECT check_name, relation, detail FROM kingres.columnar_check(true);
SELECT * FROM kingres.columnar_check();
-- VACUUM removes such rows too
INSERT INTO kingres.columnar_stripe VALUES (999999, 1, 0, 1, 1, 1, 1, 1, 1, 1, 0, '\x', '\x');
INSERT INTO kingres.columnar_stripe VALUES (999999, 2, 0, 1, 1, 1, 1, 1, 1, 1, 0, '\x', '\x');
SELECT count(*) AS orphan_rows FROM kingres.columnar_stripe WHERE filenode = 999999;
VACUUM v3;
SELECT count(*) AS orphan_rows FROM kingres.columnar_stripe WHERE filenode = 999999;
-- a stripe that lies beyond the end of its file
SELECT relfilenode AS v3_filenode FROM pg_class WHERE oid = 'v3'::regclass \gset
UPDATE kingres.columnar_stripe SET first_block = first_block + 100000 WHERE filenode = :v3_filenode AND stripe_id = 2;
SELECT check_name, relation::regclass, regexp_replace(detail, '[0-9]+', 'N', 'g') AS detail FROM kingres.columnar_check();
SELECT count(*) AS rows_before_fix FROM v3 WHERE id <= 1000;
SELECT check_name, relation::regclass FROM kingres.columnar_check(true);
SELECT * FROM kingres.columnar_check();
SELECT count(*) AS rows_after_fix, max(id) FROM v3;
-- the first page of a stripe that is not the stripe's page: reported, not repaired
UPDATE kingres.columnar_stripe SET first_block = first_block + 1 WHERE filenode = :v3_filenode AND stripe_id = 1;
SELECT check_name, relation::regclass, regexp_replace(detail, '[0-9]+', 'N', 'g') AS detail FROM kingres.columnar_check();
SELECT check_name, relation::regclass FROM kingres.columnar_check(true);
UPDATE kingres.columnar_stripe SET first_block = first_block - 1 WHERE filenode = :v3_filenode AND stripe_id = 1;
SELECT * FROM kingres.columnar_check();
SELECT count(*) FROM v3;
DROP TABLE v3;

-- the counters autovacuum reads, flushed to the statistics at once
CREATE TABLE vs (id int, s text) USING columnar;
CREATE TABLE vs_heap (id int, s text);
INSERT INTO vs SELECT i, 'a' FROM generate_series(1, 1000) i;
INSERT INTO vs_heap SELECT i, 'a' FROM generate_series(1, 1000) i;
INSERT INTO vs SELECT i, 'b' FROM generate_series(1, 500) i;
INSERT INTO vs_heap SELECT i, 'b' FROM generate_series(1, 500) i;
SELECT pg_stat_force_next_flush();
SELECT relname, n_tup_ins, n_tup_upd, n_tup_del, n_ins_since_vacuum, n_live_tup, n_dead_tup, n_mod_since_analyze,
       vacuum_count, analyze_count, last_vacuum IS NULL AS never_vacuumed
  FROM pg_stat_user_tables WHERE relname IN ('vs', 'vs_heap') ORDER BY relname;
VACUUM vs;
VACUUM vs_heap;
SELECT pg_stat_force_next_flush();
SELECT relname, n_tup_ins, n_ins_since_vacuum, n_live_tup, n_dead_tup, n_mod_since_analyze,
       vacuum_count, analyze_count, last_vacuum IS NULL AS never_vacuumed
  FROM pg_stat_user_tables WHERE relname IN ('vs', 'vs_heap') ORDER BY relname;
ANALYZE vs;
ANALYZE vs_heap;
SELECT pg_stat_force_next_flush();
SELECT relname, n_ins_since_vacuum, n_mod_since_analyze, vacuum_count, analyze_count, last_analyze IS NULL AS never_analyzed
  FROM pg_stat_user_tables WHERE relname IN ('vs', 'vs_heap') ORDER BY relname;
INSERT INTO vs SELECT i, 'c' FROM generate_series(1, 30) i;
INSERT INTO vs_heap SELECT i, 'c' FROM generate_series(1, 30) i;
BEGIN;
INSERT INTO vs SELECT i, 'd' FROM generate_series(1, 20) i;
INSERT INTO vs_heap SELECT i, 'd' FROM generate_series(1, 20) i;
ROLLBACK;
SELECT pg_stat_force_next_flush();
SELECT relname, n_tup_ins, n_ins_since_vacuum, n_live_tup, n_dead_tup, n_mod_since_analyze
  FROM pg_stat_user_tables WHERE relname IN ('vs', 'vs_heap') ORDER BY relname;
VACUUM (ANALYZE) vs;
SELECT pg_stat_force_next_flush();
SELECT relname, n_ins_since_vacuum, n_live_tup, n_dead_tup, n_mod_since_analyze, vacuum_count, analyze_count
  FROM pg_stat_user_tables WHERE relname = 'vs';
SELECT relname, reltuples FROM pg_class WHERE relname IN ('vs', 'vs_heap') ORDER BY relname;
DROP TABLE vs, vs_heap;

-- VACUUM FULL keeps the data, the options and a consistent catalog
CREATE TABLE vf (id int, s text) USING columnar;
SELECT kingres.columnar_set_options('vf', compression => 'lz4', compression_level => 5, stripe_row_count => 1000, chunk_row_count => 200);
INSERT INTO vf SELECT i, 'r' || i FROM generate_series(1, 2500) i;
INSERT INTO vf SELECT i, 'q' || i FROM generate_series(1, 10) i;
SELECT count(*), sum(id), md5(string_agg(id || s, ',' ORDER BY id, s)) AS digest FROM vf \gset
SELECT stripes, rows FROM kingres.columnar_info('vf');
VACUUM FULL vf;
SELECT stripes, rows FROM kingres.columnar_info('vf');
SELECT count(*) = :count AND sum(id) = :sum AND md5(string_agg(id || s, ',' ORDER BY id, s)) = :'digest' AS same_content FROM vf;
SELECT compression, compression_level, stripe_row_count, chunk_row_count FROM kingres.columnar_options('vf');
-- the rows of the old file go at the next VACUUM of a columnar table
VACUUM vf;
SELECT * FROM kingres.columnar_check();
DROP TABLE vf;
SELECT count(*) AS catalog_rows FROM kingres.columnar_stripe;
