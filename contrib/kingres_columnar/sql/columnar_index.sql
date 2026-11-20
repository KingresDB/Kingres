-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_index: B-tree indexes on columnar tables (increment C6b, design 7.8).
-- What is served (a non-unique B-tree: plain, multi-column, expression, partial,
-- INCLUDE, DESC and NULLS FIRST, with a collation, on several types), what is
-- refused and with which message, and that an index scan gives the rows of a heap
-- twin with the same indexes.  Random programs, crashes and standbys are in t/051.
SET search_path = public;
SET enable_bitmapscan = on;
-- the custom scan is a path of its own that enable_seqscan does not turn off: off here, so
-- that the index paths are what the planner has (a section at the end leaves it on)
SET kingres_columnar.enable_custom_scan = off;

CREATE TABLE ci (id int, g int, t text, n numeric, a int[], d date, j jsonb) USING columnar;
SELECT kingres.columnar_set_options('ci', stripe_row_count => 1000, chunk_row_count => 100);
CREATE TABLE ch (LIKE ci);
INSERT INTO ci SELECT i, i % 50, CASE WHEN i % 17 = 0 THEN NULL ELSE 'k' || (i % 300) END, (i % 40) * 1.5,
                      ARRAY[i % 7, i % 3], date '2024-01-01' + (i % 90), jsonb_build_object('v', i % 25)
  FROM generate_series(1, 5000) i;
INSERT INTO ch SELECT * FROM ci;

-- every kind of index the builder serves
CREATE INDEX ci_id ON ci (id);
CREATE INDEX ci_g_t ON ci (g, t DESC NULLS FIRST);
CREATE INDEX ci_expr ON ci ((id % 100), lower(t));
CREATE INDEX ci_part ON ci (id) WHERE g = 3;
CREATE INDEX ci_incl ON ci (n) INCLUDE (t);
CREATE INDEX ci_coll ON ci (t COLLATE "C");
CREATE INDEX ci_arr ON ci (a);
CREATE INDEX ci_d ON ci (d);
CREATE INDEX ci_j ON ci (j);
CREATE INDEX ci_n ON ci (n NULLS FIRST);
CREATE INDEX ch_id ON ch (id);
CREATE INDEX ch_g_t ON ch (g, t DESC NULLS FIRST);
CREATE INDEX ch_expr ON ch ((id % 100), lower(t));
CREATE INDEX ch_part ON ch (id) WHERE g = 3;
CREATE INDEX ch_incl ON ch (n) INCLUDE (t);
CREATE INDEX ch_coll ON ch (t COLLATE "C");
CREATE INDEX ch_arr ON ch (a);
CREATE INDEX ch_d ON ch (d);
CREATE INDEX ch_j ON ch (j);
CREATE INDEX ch_n ON ch (n NULLS FIRST);
ANALYZE ci;
ANALYZE ch;

-- a plan that uses the index, never a bitmap scan or an index-only scan
SET enable_seqscan = off;
EXPLAIN (COSTS OFF) SELECT id FROM ci WHERE id = 1234;
EXPLAIN (COSTS OFF) SELECT id, t FROM ci WHERE g = 7 AND t < 'k5' ORDER BY g, t DESC NULLS FIRST;
EXPLAIN (COSTS OFF) SELECT id FROM ci WHERE id BETWEEN 100 AND 200;
EXPLAIN (COSTS OFF) SELECT n, t FROM ci WHERE n = 3.0;
EXPLAIN (COSTS OFF) SELECT id FROM ci WHERE id % 100 = 5 AND lower(t) = 'k17';
EXPLAIN (COSTS OFF) SELECT id FROM ci WHERE g = 3 AND id < 400;
EXPLAIN (COSTS OFF) SELECT id FROM ci WHERE id = 3 OR id = 4000;
-- index scans answer as the twin does
SELECT string_agg(md5(r), '') = (SELECT string_agg(md5(r), '') FROM (SELECT ch::text r FROM ch WHERE id = 1234 ORDER BY id) q) AS pk_same
  FROM (SELECT ci::text r FROM ci WHERE id = 1234 ORDER BY id) p;
CREATE FUNCTION same_rows(pred text) RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE a text; b text;
BEGIN
  EXECUTE format('SELECT coalesce(md5(string_agg(x::text, '','' ORDER BY x::text)), '''') FROM (SELECT id, g, t, n, a, d, j FROM ci WHERE %s) x', pred) INTO a;
  EXECUTE format('SELECT coalesce(md5(string_agg(x::text, '','' ORDER BY x::text)), '''') FROM (SELECT id, g, t, n, a, d, j FROM ch WHERE %s) x', pred) INTO b;
  RETURN a = b;
END $$;
SELECT same_rows('id = 1234'), same_rows('id BETWEEN 100 AND 700'), same_rows('g = 7 AND t < ''k5'''),
       same_rows('t IS NULL AND g < 10'), same_rows('n = 3.0'), same_rows('id % 100 = 5 AND lower(t) = ''k17'''),
       same_rows('g = 3 AND id < 400'), same_rows('id = 3 OR id = 4000'), same_rows('t = ''k17'''),
       same_rows('a = ARRAY[3,1]'), same_rows('d = date ''2024-02-10'''), same_rows('j = ''{"v": 4}''::jsonb'),
       same_rows('n IS NULL'), same_rows('id > 4990');
SELECT count(*) AS by_index FROM ci WHERE id BETWEEN 100 AND 700;
-- ORDER BY comes from the index (no Sort), also backward
EXPLAIN (COSTS OFF) SELECT id FROM ci ORDER BY id LIMIT 3;
EXPLAIN (COSTS OFF) SELECT id FROM ci ORDER BY id DESC LIMIT 3;
SELECT id FROM ci ORDER BY id DESC LIMIT 3;
-- a merge join over two index scans, and a nested loop with an index scan inside
SET enable_hashjoin = off;
SET enable_nestloop = off;
EXPLAIN (COSTS OFF) SELECT count(*) FROM ci x JOIN ch y ON x.id = y.id;
SELECT count(*) FROM ci x JOIN ch y ON x.id = y.id;
RESET enable_nestloop;
SET enable_mergejoin = off;
SET enable_hashjoin = off;
SELECT count(*) FROM (SELECT id FROM ch WHERE id < 300) y JOIN ci x ON x.id = y.id;
RESET enable_mergejoin;
RESET enable_hashjoin;
-- bitmap scans cannot be made (a columnar TID has 2048 offsets, a bitmap holds fewer)
SET enable_indexscan = off;
EXPLAIN (COSTS OFF) SELECT id FROM ci WHERE id = 1234;
RESET enable_indexscan;
RESET enable_seqscan;

-- what is refused
CREATE UNIQUE INDEX ci_u ON ci (id);
ALTER TABLE ci ADD PRIMARY KEY (id);
ALTER TABLE ci ADD UNIQUE (g, id);
CREATE INDEX ci_hash ON ci USING hash (id);
CREATE INDEX ci_brin ON ci USING brin (id);
CREATE INDEX ci_gin ON ci USING gin (a);
CREATE INDEX CONCURRENTLY ci_conc ON ci (id);
REINDEX INDEX CONCURRENTLY ci_id;
REINDEX TABLE CONCURRENTLY ci;
CREATE TABLE ci_pk (id int PRIMARY KEY) USING columnar;
CLUSTER ci USING ci_id;
-- no leftovers of the refused statements
SELECT count(*) AS indexes FROM pg_index WHERE indrelid = 'ci'::regclass;
SELECT count(*) AS invalid FROM pg_index WHERE NOT indisvalid OR NOT indisready;

-- REINDEX and the rewrites keep the indexes right
REINDEX TABLE ci;
SELECT same_rows('id BETWEEN 100 AND 700');
VACUUM FULL ci;
SELECT same_rows('id BETWEEN 100 AND 700'), same_rows('t = ''k17'''), same_rows('id = 77');
TRUNCATE ci;
SELECT count(*) AS after_truncate FROM ci WHERE id > 0;
INSERT INTO ci SELECT i, i % 50, 'k' || (i % 300), 1.5, ARRAY[1], date '2024-01-01', '{}' FROM generate_series(1, 3000) i;
TRUNCATE ch;
INSERT INTO ch SELECT * FROM ci;
SELECT same_rows('id BETWEEN 100 AND 700'), same_rows('g = 3 AND id < 400');
-- VACUUM leaves the dead entries of aborted writers in the index and the fetch filters them
BEGIN;
INSERT INTO ci SELECT i, 7, 'rolled back', 0, ARRAY[0], date '2024-03-03', '{}' FROM generate_series(10001, 10300) i;
ROLLBACK;
VACUUM ci;
SET enable_seqscan = off;
SELECT count(*) AS rolled_back_rows FROM ci WHERE id > 10000;
SELECT same_rows('id > 2900');
RESET enable_seqscan;
-- convert to heap and back: the indexes are rebuilt by the rewrite
ALTER TABLE ci SET ACCESS METHOD heap;
SELECT same_rows('id BETWEEN 100 AND 700');
ALTER TABLE ci SET ACCESS METHOD columnar;
SET enable_seqscan = off;
SELECT same_rows('id BETWEEN 100 AND 700'), same_rows('t = ''k17'''), same_rows('g = 3 AND id < 400');
RESET enable_seqscan;
SELECT * FROM kingres.columnar_check();

-- one transaction: rows in the buffer, the first index, more rows, a query by the index
CREATE TABLE ct (id int, v text) USING columnar;
CREATE TABLE cth (id int, v text);
BEGIN;
INSERT INTO ct SELECT i, 'a' || i FROM generate_series(1, 700) i;
INSERT INTO cth SELECT i, 'a' || i FROM generate_series(1, 700) i;
CREATE INDEX ct_id ON ct (id);
CREATE INDEX cth_id ON cth (id);
INSERT INTO ct SELECT i, 'b' || i FROM generate_series(701, 900) i;
INSERT INTO cth SELECT i, 'b' || i FROM generate_series(701, 900) i;
SET LOCAL enable_seqscan = off;
EXPLAIN (COSTS OFF) SELECT v FROM ct WHERE id = 850;
SELECT (SELECT string_agg(v, ',' ORDER BY id) FROM ct WHERE id IN (5, 699, 700, 701, 850)) =
       (SELECT string_agg(v, ',' ORDER BY id) FROM cth WHERE id IN (5, 699, 700, 701, 850)) AS in_transaction_same;
SAVEPOINT s;
INSERT INTO ct VALUES (901, 'sp');
INSERT INTO cth VALUES (901, 'sp');
SELECT count(*) AS own_row_by_index FROM ct WHERE id = 901;
ROLLBACK TO s;
SELECT count(*) AS rolled_back_savepoint FROM ct WHERE id = 901;
INSERT INTO ct VALUES (902, 'after');
INSERT INTO cth VALUES (902, 'after');
COMMIT;
SET enable_seqscan = off;
SELECT (SELECT string_agg(v, ',' ORDER BY id) FROM ct WHERE id >= 690) = (SELECT string_agg(v, ',' ORDER BY id) FROM cth WHERE id >= 690) AS after_commit_same;
RESET enable_seqscan;
-- an index created in a subtransaction over rows of an enclosing one is refused
CREATE TABLE cs (id int) USING columnar;
BEGIN;
INSERT INTO cs VALUES (1);
SAVEPOINT s;
CREATE INDEX cs_id ON cs (id);
ROLLBACK;
SELECT * FROM kingres.columnar_check();

-- with the custom scan on the planner chooses by cost between it and the index scans, and the rows are the same
RESET kingres_columnar.enable_custom_scan;
SELECT same_rows('id BETWEEN 100 AND 700'), same_rows('t = ''k17'''), same_rows('g = 3 AND id < 400'), same_rows('id = 77');
EXPLAIN (COSTS OFF) SELECT id FROM ci WHERE g = 7 AND t < 'k5' ORDER BY g, t DESC NULLS FIRST;

-- INSERT ... SELECT from the table itself through its index: the command does not see the rows it inserts
SET kingres_columnar.enable_custom_scan = off;
SET enable_seqscan = off;
BEGIN;
INSERT INTO ct SELECT id + 10000, 'self' FROM ct WHERE id < 5;
SELECT count(*) AS after_self_insert FROM ct WHERE id > 10000;
SELECT count(*) AS own_rows_by_index FROM ct WHERE id BETWEEN 10001 AND 10004;
COMMIT;
RESET enable_seqscan;
RESET kingres_columnar.enable_custom_scan;

-- the command does not see its own rows by the index either (it would not stop otherwise), and
-- a stripe written in the middle of the statement (1000 rows) does not lose the rows it reads
SET kingres_columnar.enable_custom_scan = off;
SET enable_seqscan = off;
CREATE TABLE cx (id int, v text) USING columnar;
SELECT kingres.columnar_set_options('cx', stripe_row_count => 1000, chunk_row_count => 100);
CREATE INDEX cx_id ON cx (id);
BEGIN;
INSERT INTO cx SELECT g, 'first' FROM generate_series(10001, 10004) g;
INSERT INTO cx SELECT id + 1, 'again' FROM cx WHERE id BETWEEN 10001 AND 10010;
SELECT count(*) AS halloween_rows FROM cx WHERE id BETWEEN 10001 AND 10010;
-- a lookup by number of a row of the same command finds nothing
WITH i AS (INSERT INTO cx VALUES (30001, 'w') RETURNING id) SELECT count(*) AS same_command_lookup FROM i JOIN cx ON cx.id = i.id;
SELECT count(*) AS next_command_lookup FROM cx WHERE id = 30001;
SET enable_hashjoin = off;
SET enable_mergejoin = off;
INSERT INTO cx SELECT g, 'small' FROM generate_series(39001, 39300) g;
WITH i AS (INSERT INTO cx SELECT g, 'big' FROM generate_series(40001, 41200) g RETURNING id) SELECT count(*) AS same_command_after_stripe FROM i JOIN cx ON cx.id = i.id;
SELECT count(*) AS earlier_command_rows FROM cx WHERE id BETWEEN 39001 AND 39300;
COMMIT;
RESET enable_hashjoin;
RESET enable_mergejoin;
SELECT count(*) AS next_command_after_stripe FROM cx WHERE id BETWEEN 40001 AND 41200;
INSERT INTO cx SELECT g, 'bulk' FROM generate_series(20001, 20700) g;
INSERT INTO cx SELECT id + 100000, 'copy' FROM cx WHERE id BETWEEN 20001 AND 20700;
SELECT count(*) AS flushed_in_statement FROM cx WHERE id > 120000;
COMMIT;
SELECT count(*) AS after_commit FROM cx WHERE id > 120000;
RESET enable_seqscan;
RESET kingres_columnar.enable_custom_scan;
