-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_basic: create, insert (single row, multi-row, INSERT SELECT, COPY,
-- CREATE TABLE AS), the codecs, transactions, savepoints, visibility,
-- cursors, TRUNCATE and prepared transactions.
SET search_path = public;
CREATE TABLE b1 (id int, name text, v float8) USING columnar;
SELECT count(*) FROM b1;
SELECT * FROM kingres.columnar_info('b1');

INSERT INTO b1 VALUES (1, 'one', 1.5);
INSERT INTO b1 VALUES (2, 'two', 2.5), (3, NULL, NULL), (NULL, 'four', 4.5);
SELECT * FROM b1 ORDER BY id;
INSERT INTO b1 SELECT i, 'n' || i, i / 4.0 FROM generate_series(10, 20) i;
SELECT count(*), sum(id), count(name), count(v) FROM b1;
-- uncommitted writes are flushed at commit; autocommit statements are complete
SELECT stripes, rows FROM kingres.columnar_info('b1');

-- COPY FROM STDIN: text, csv and a NULL marker
COPY b1 FROM STDIN;
100	copied	0.25
101	\N	\N
\N	\N	7
\.
COPY b1 (id, name) FROM STDIN WITH (FORMAT csv);
200,"with, comma"
201,
\.
SELECT * FROM b1 WHERE id >= 100 OR id IS NULL ORDER BY id, name;
SELECT count(*) FROM b1;
COPY (SELECT id, name FROM b1 WHERE id >= 100 ORDER BY id) TO STDOUT;
COPY b1 (id, v) TO STDOUT WITH (FORMAT csv, HEADER);

-- CREATE TABLE AS and SELECT INTO
CREATE TABLE b2 USING columnar AS SELECT i AS id, i % 7 AS m, 'x' || i AS s FROM generate_series(1, 1000) i;
SELECT count(*), sum(id), count(DISTINCT m), min(s), max(s) FROM b2;
SELECT a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'b2'::regclass;
CREATE TABLE b3 USING columnar AS SELECT * FROM b2 WITH NO DATA;
SELECT count(*) FROM b3;
INSERT INTO b3 SELECT * FROM b2 WHERE m = 3;
SELECT count(*) FROM b3;
SET default_table_access_method = columnar;
CREATE TABLE b4 AS SELECT i FROM generate_series(1, 5) i;
SELECT i INTO b5 FROM generate_series(1, 5) i;
RESET default_table_access_method;
SELECT c.relname, a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam
 WHERE c.relname IN ('b4', 'b5') ORDER BY 1;
DROP TABLE b4, b5;

-- each codec, set by the table, by the setting and by columnar_set_options;
-- the same data comes back whatever the codec
CREATE TABLE b_ref AS
SELECT i AS id, 'row ' || i || repeat('abc', i % 10) AS txt, (i % 100) / 7.0 AS f,
       CASE WHEN i % 5 = 0 THEN NULL ELSE i % 13 END AS n
  FROM generate_series(1, 5000) i;
CREATE TABLE b_codec (id int, txt text, f float8, n int) USING columnar;
SELECT compression, compression_level FROM kingres.columnar_options('b_codec');
SELECT kingres.columnar_set_options('b_codec', compression => 'none');
INSERT INTO b_codec SELECT * FROM b_ref WHERE id <= 1250;
SELECT kingres.columnar_set_options('b_codec', compression => 'pglz');
INSERT INTO b_codec SELECT * FROM b_ref WHERE id > 1250 AND id <= 2500;
SELECT kingres.columnar_set_options('b_codec', compression => 'lz4');
INSERT INTO b_codec SELECT * FROM b_ref WHERE id > 2500 AND id <= 3750;
SELECT kingres.columnar_set_options('b_codec', compression => 'zstd', compression_level => 9);
INSERT INTO b_codec SELECT * FROM b_ref WHERE id > 3750;
SELECT compression, compression_level, stripe_row_count, chunk_row_count FROM kingres.columnar_options('b_codec');
SELECT stripes, rows FROM kingres.columnar_info('b_codec');
SELECT count(*) AS only_in_columnar FROM (SELECT * FROM b_codec EXCEPT ALL SELECT * FROM b_ref) x;
SELECT count(*) AS only_in_heap FROM (SELECT * FROM b_ref EXCEPT ALL SELECT * FROM b_codec) x;

-- the codec from the setting, at table creation; "auto" resolves to a real codec
SET kingres_columnar.compression = 'pglz';
CREATE TABLE b_c_pglz USING columnar AS SELECT * FROM b_ref;
SET kingres_columnar.compression = 'none';
CREATE TABLE b_c_none USING columnar AS SELECT * FROM b_ref;
SET kingres_columnar.compression = 'lz4';
SET kingres_columnar.compression_level = 12;
CREATE TABLE b_c_lz4 USING columnar AS SELECT * FROM b_ref;
SET kingres_columnar.compression = 'zstd';
SET kingres_columnar.compression_level = 19;
CREATE TABLE b_c_zstd USING columnar AS SELECT * FROM b_ref;
RESET kingres_columnar.compression;
RESET kingres_columnar.compression_level;
CREATE TABLE b_c_auto USING columnar AS SELECT * FROM b_ref;
SELECT 'pglz' AS t, compression, compression_level FROM kingres.columnar_options('b_c_pglz')
UNION ALL SELECT 'none', compression, compression_level FROM kingres.columnar_options('b_c_none')
UNION ALL SELECT 'lz4', compression, compression_level FROM kingres.columnar_options('b_c_lz4')
UNION ALL SELECT 'zstd', compression, compression_level FROM kingres.columnar_options('b_c_zstd')
UNION ALL SELECT 'auto', compression, compression_level FROM kingres.columnar_options('b_c_auto');
SELECT t, count(*) AS only_in_columnar FROM
  (SELECT 'pglz' AS t, * FROM (SELECT * FROM b_c_pglz EXCEPT ALL SELECT * FROM b_ref) a
   UNION ALL SELECT 'none', * FROM (SELECT * FROM b_c_none EXCEPT ALL SELECT * FROM b_ref) a
   UNION ALL SELECT 'lz4', * FROM (SELECT * FROM b_c_lz4 EXCEPT ALL SELECT * FROM b_ref) a
   UNION ALL SELECT 'zstd', * FROM (SELECT * FROM b_c_zstd EXCEPT ALL SELECT * FROM b_ref) a
   UNION ALL SELECT 'auto', * FROM (SELECT * FROM b_c_auto EXCEPT ALL SELECT * FROM b_ref) a) x
 GROUP BY t ORDER BY t;
-- compressible data takes less space on disk than raw data
SELECT i.bytes_on_disk < i.bytes_uncompressed AS compressed
  FROM kingres.columnar_info('b_c_zstd') i;
SELECT i.bytes_on_disk >= i.bytes_uncompressed AS not_compressed
  FROM kingres.columnar_info('b_c_none') i;
DROP TABLE b_c_pglz, b_c_none, b_c_lz4, b_c_zstd, b_c_auto, b_codec;

-- options errors
SELECT kingres.columnar_set_options('b1', compression => 'gzip');
SELECT kingres.columnar_set_options('b1', compression_level => 0);
SELECT kingres.columnar_set_options('b1', stripe_row_count => 999);
SELECT kingres.columnar_set_options('b1', chunk_row_count => 99);
SELECT kingres.columnar_set_options('b1', stripe_row_count => 1000, chunk_row_count => 2000);
SELECT kingres.columnar_set_options('b_ref');
SELECT * FROM kingres.columnar_options('b_ref');

-- rollback: nothing of an aborted transaction is visible, later work is
CREATE TABLE tx (id int, s text) USING columnar;
INSERT INTO tx VALUES (1, 'committed');
BEGIN;
INSERT INTO tx SELECT i, 'rolled back' FROM generate_series(100, 5000) i;
SELECT count(*) FROM tx;
ROLLBACK;
SELECT count(*), min(s) FROM tx;
INSERT INTO tx VALUES (2, 'after rollback');
SELECT * FROM tx ORDER BY id;

-- savepoints: rollback to, release, nested
BEGIN;
INSERT INTO tx VALUES (10, 'level 0');
SAVEPOINT s1;
INSERT INTO tx VALUES (11, 'level 1');
SAVEPOINT s2;
INSERT INTO tx VALUES (12, 'level 2');
SELECT id FROM tx WHERE id >= 10 ORDER BY id;
ROLLBACK TO s2;
SELECT id FROM tx WHERE id >= 10 ORDER BY id;
INSERT INTO tx VALUES (13, 'level 2 again');
SAVEPOINT s3;
INSERT INTO tx VALUES (14, 'level 3');
RELEASE s3;
RELEASE s2;
SELECT id FROM tx WHERE id >= 10 ORDER BY id;
ROLLBACK TO s1;
SELECT id FROM tx WHERE id >= 10 ORDER BY id;
INSERT INTO tx VALUES (15, 'after rollback to s1');
COMMIT;
SELECT id, s FROM tx ORDER BY id;

BEGIN;
SAVEPOINT a;
INSERT INTO tx VALUES (20, 'a');
SAVEPOINT b;
INSERT INTO tx VALUES (21, 'b');
ROLLBACK TO a;
INSERT INTO tx VALUES (22, 'a2');
RELEASE a;
INSERT INTO tx VALUES (23, 'outer');
COMMIT;
SELECT id, s FROM tx WHERE id >= 20 ORDER BY id;

-- a failing statement in a transaction with a savepoint
BEGIN;
INSERT INTO tx VALUES (30, 'before error');
SAVEPOINT e;
INSERT INTO tx SELECT 31, 'x' || (1 / (i - 3)) FROM generate_series(1, 5) i;
ROLLBACK TO e;
INSERT INTO tx VALUES (32, 'after error');
COMMIT;
SELECT id, s FROM tx WHERE id >= 30 ORDER BY id;

-- an exception block of PL/pgSQL is a subtransaction
CREATE FUNCTION tx_exc() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO tx VALUES (40, 'kept');
  BEGIN
    INSERT INTO tx VALUES (41, 'lost');
    PERFORM 1 / 0;
  EXCEPTION WHEN division_by_zero THEN
    INSERT INTO tx VALUES (42, 'handler');
  END;
END $$;
SELECT tx_exc();
SELECT id, s FROM tx WHERE id >= 40 ORDER BY id;

-- an error aborts the whole transaction
BEGIN;
INSERT INTO tx VALUES (50, 'aborted');
SELECT 1 / 0;
COMMIT;
SELECT count(*) FROM tx WHERE id = 50;

-- own rows are visible to later commands of the same transaction
BEGIN;
SELECT count(*) FROM tx WHERE id >= 60;
INSERT INTO tx VALUES (60, 'cmd 1');
SELECT count(*) FROM tx WHERE id >= 60;
INSERT INTO tx VALUES (61, 'cmd 2'), (62, 'cmd 2');
SELECT count(*) FROM tx WHERE id >= 60;
INSERT INTO tx SELECT id + 100, s FROM tx WHERE id >= 60;
SELECT id FROM tx WHERE id >= 60 ORDER BY id;
-- a statement does not see its own output
INSERT INTO tx SELECT id + 1000, s FROM tx;
SELECT count(*) FILTER (WHERE id >= 1000) AS copies, count(*) FILTER (WHERE id >= 1000 AND id < 1100) AS first_gen FROM tx;
COMMIT;
SELECT count(*) FROM tx;
-- a volatile function sees the rows written before the statement that calls it
CREATE FUNCTION tx_count() RETURNS bigint LANGUAGE sql VOLATILE AS 'SELECT count(*) FROM tx';
BEGIN;
INSERT INTO tx VALUES (70, 'f');
SELECT tx_count() - (SELECT count(*) FROM tx) AS difference;
COMMIT;
INSERT INTO tx SELECT * FROM tx;
SELECT count(*) FROM tx;

-- cursors
CREATE TABLE cur (id int, s text) USING columnar;
INSERT INTO cur SELECT i, 'c' || i FROM generate_series(1, 20) i;
BEGIN;
DECLARE c1 CURSOR FOR SELECT id FROM cur ORDER BY id;
FETCH 3 FROM c1;
INSERT INTO cur VALUES (21, 'late');
FETCH 3 FROM c1;
CLOSE c1;
DECLARE c2 SCROLL CURSOR FOR SELECT id, s FROM cur;
FETCH 5 FROM c2;
FETCH BACKWARD 2 FROM c2;
FETCH ABSOLUTE 1 FROM c2;
FETCH LAST FROM c2;
FETCH PRIOR FROM c2;
FETCH FIRST FROM c2;
FETCH RELATIVE 10 FROM c2;
MOVE BACKWARD ALL IN c2;
FETCH NEXT FROM c2;
CLOSE c2;
DECLARE c3 CURSOR WITH HOLD FOR SELECT id FROM cur WHERE id > 15 ORDER BY id;
FETCH 2 FROM c3;
COMMIT;
FETCH 2 FROM c3;
INSERT INTO cur VALUES (22, 'after hold');
FETCH ALL FROM c3;
CLOSE c3;
-- a scroll cursor in a transaction that wrote rows itself
BEGIN;
INSERT INTO cur VALUES (23, 'own');
DECLARE c4 SCROLL CURSOR FOR SELECT id FROM cur WHERE id >= 20 ORDER BY id DESC;
FETCH ALL FROM c4;
FETCH BACKWARD ALL FROM c4;
COMMIT;

-- TRUNCATE: commit and rollback
CREATE TABLE tr (id int) USING columnar;
INSERT INTO tr SELECT generate_series(1, 100);
TRUNCATE tr;
SELECT count(*) FROM tr;
SELECT stripes, rows FROM kingres.columnar_info('tr');
INSERT INTO tr SELECT generate_series(1, 10);
BEGIN;
TRUNCATE tr;
SELECT count(*) FROM tr;
INSERT INTO tr SELECT generate_series(1, 3);
SELECT count(*) FROM tr;
ROLLBACK;
SELECT count(*), sum(id) FROM tr;
BEGIN;
INSERT INTO tr VALUES (11);
TRUNCATE tr;
INSERT INTO tr VALUES (12);
COMMIT;
SELECT * FROM tr;
BEGIN;
INSERT INTO tr VALUES (13);
TRUNCATE tr;
ROLLBACK;
SELECT * FROM tr;
-- TRUNCATE in a subtransaction, rolled back, and another one released
BEGIN;
SAVEPOINT s;
TRUNCATE tr;
INSERT INTO tr VALUES (14);
ROLLBACK TO s;
SELECT * FROM tr;
TRUNCATE tr;
RELEASE s;
INSERT INTO tr VALUES (15);
COMMIT;
SELECT * FROM tr;

-- prepared transactions: commit and rollback
CREATE TABLE pt (id int, s text) USING columnar;
INSERT INTO pt VALUES (1, 'base');
BEGIN;
INSERT INTO pt SELECT i, 'prepared commit' FROM generate_series(10, 12) i;
PREPARE TRANSACTION 'kc_commit';
SELECT count(*) FROM pt;
COMMIT PREPARED 'kc_commit';
SELECT id, s FROM pt ORDER BY id;
BEGIN;
INSERT INTO pt SELECT i, 'prepared rollback' FROM generate_series(20, 29) i;
PREPARE TRANSACTION 'kc_rollback';
SELECT count(*) FROM pt;
ROLLBACK PREPARED 'kc_rollback';
SELECT count(*) FROM pt;
INSERT INTO pt VALUES (2, 'after');
SELECT id, s FROM pt ORDER BY id;
SELECT count(*) AS leftover_prepared FROM pg_prepared_xacts;

-- a small write buffer: stripes are flushed early, also with savepoints and a rollback to one of them
SET kingres_columnar.write_buffer_memory = '1MB';
CREATE TABLE wb_a (a int, b text) USING columnar;
CREATE TABLE wb_b (a int, b text) USING columnar;
CREATE TABLE wb_c (a int, b text) USING columnar;
BEGIN;
INSERT INTO wb_a SELECT i, repeat('a', 200) FROM generate_series(1, 20000) i;
SAVEPOINT s1;
INSERT INTO wb_b SELECT i, repeat('b', 200) FROM generate_series(1, 20000) i;
INSERT INTO wb_c SELECT i, repeat('c', 200) FROM generate_series(1, 20000) i;
INSERT INTO wb_a SELECT i, repeat('a', 200) FROM generate_series(1, 20000) i;
SAVEPOINT s2;
INSERT INTO wb_b SELECT i, repeat('b', 200) FROM generate_series(1, 20000) i;
ROLLBACK TO s2;
INSERT INTO wb_c SELECT i, repeat('c', 200) FROM generate_series(1, 5000) i;
RELEASE s1;
SELECT (SELECT count(*) FROM wb_a) AS a, (SELECT count(*) FROM wb_b) AS b, (SELECT count(*) FROM wb_c) AS c;
COMMIT;
RESET kingres_columnar.write_buffer_memory;
SELECT (SELECT count(*) FROM wb_a) AS a, (SELECT count(*) FROM wb_b) AS b, (SELECT count(*) FROM wb_c) AS c;
SELECT (SELECT stripes FROM kingres.columnar_info('wb_a')) > 1 AS a_in_several_stripes;
SELECT count(*) AS a_rows, count(DISTINCT a) AS a_distinct, min(b) = max(b) AS same_text FROM wb_a;
SELECT count(*) AS c_rows, sum(a) AS c_sum FROM wb_c;
DROP TABLE wb_a, wb_b, wb_c;

DROP TABLE b1, b2, b3, b_ref, tx, cur, tr, pt;
DROP FUNCTION tx_exc(), tx_count();
SELECT count(*) AS catalog_rows FROM kingres.columnar_stripe;
