-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- Regression tests for the findings of the independent review of 0.1:
-- rewrites with buffered rows (C1-02, C1-06), stale statistics (C1-03),
-- cursors across RELEASE SAVEPOINT (C1-04), parallel plans (C1-05), the DDL
-- event trigger and CREATE SCHEMA (C1-09), publications (C1-10).
-- Each statement is applied to a heap twin where an answer is compared.
SET search_path = public;
SET client_min_messages = warning;

-- C1-02: rows buffered before a rewrite are committed exactly once, at
-- every subtransaction depth, and a rollback to a savepoint brings back the
-- rows of the old file.
CREATE TABLE rw_c (id int, f int) USING columnar;
CREATE TABLE rw_h (id int, f int);

BEGIN;
INSERT INTO rw_c VALUES (1, 1), (2, 2);
ALTER TABLE rw_c ALTER COLUMN f TYPE bigint;
SELECT count(*), sum(f) FROM rw_c;
COMMIT;
SELECT count(*), sum(f) FROM rw_c;

BEGIN;
INSERT INTO rw_c VALUES (3, 3);
SAVEPOINT a;
INSERT INTO rw_c VALUES (4, 4);
SAVEPOINT b;
ALTER TABLE rw_c ALTER COLUMN f TYPE int;
SELECT count(*), sum(f) FROM rw_c;
ROLLBACK TO b;
SELECT count(*), sum(f) FROM rw_c;
ALTER TABLE rw_c ALTER COLUMN f TYPE numeric;
SELECT count(*), sum(f) FROM rw_c;
RELEASE a;
SELECT count(*), sum(f) FROM rw_c;
COMMIT;
SELECT count(*), sum(f) FROM rw_c;

-- ADD COLUMN with a volatile default rewrites
BEGIN;
INSERT INTO rw_c VALUES (5, 5);
ALTER TABLE rw_c ADD COLUMN r float8 DEFAULT random();
SELECT count(*), count(r) FROM rw_c;
COMMIT;
SELECT count(*), count(r) FROM rw_c;

-- the access method round trip in one transaction
BEGIN;
INSERT INTO rw_c VALUES (6, 6);
ALTER TABLE rw_c SET ACCESS METHOD heap;
ALTER TABLE rw_c SET ACCESS METHOD columnar;
SELECT count(*), sum(f) FROM rw_c;
COMMIT;
SELECT count(*), sum(f) FROM rw_c;

-- C1-06: reading buffered rows after a type change that changes how the
-- values are stored
CREATE TABLE rw2 (id int, f int) USING columnar;
INSERT INTO rw2 VALUES (1, 1);
BEGIN;
INSERT INTO rw2 VALUES (2, 2);
ALTER TABLE rw2 ALTER COLUMN f TYPE text USING f::text;
SELECT id, f FROM rw2 ORDER BY id;
ALTER TABLE rw2 ALTER COLUMN f TYPE int USING f::int;
SELECT id, f FROM rw2 ORDER BY id;
COMMIT;
SELECT id, f FROM rw2 ORDER BY id;

-- TRUNCATE after an outer level inserted rows: allowed, and undone by a
-- rollback to the savepoint
CREATE TABLE tr_c (a int) USING columnar;
BEGIN;
INSERT INTO tr_c VALUES (1);
SAVEPOINT s;
TRUNCATE tr_c;
INSERT INTO tr_c VALUES (2), (3);
SELECT count(*), sum(a) FROM tr_c;
ROLLBACK TO s;
SELECT count(*), sum(a) FROM tr_c;
COMMIT;
SELECT count(*), sum(a) FROM tr_c;
BEGIN;
INSERT INTO tr_c VALUES (7);
SAVEPOINT s;
TRUNCATE tr_c;
INSERT INTO tr_c VALUES (8);
RELEASE s;
COMMIT;
SELECT count(*), sum(a) FROM tr_c;

-- C1-04: a cursor keeps its rows across RELEASE, ROLLBACK TO and a flush
-- caused by the size of the buffer
CREATE TABLE cu_c (a int) USING columnar;
BEGIN;
INSERT INTO cu_c VALUES (1);
SAVEPOINT s;
INSERT INTO cu_c VALUES (2);
DECLARE cc CURSOR FOR SELECT a FROM cu_c ORDER BY a;
RELEASE s;
FETCH ALL FROM cc;
COMMIT;

BEGIN;
INSERT INTO cu_c VALUES (10);
DECLARE c1 CURSOR FOR SELECT a FROM cu_c WHERE a >= 10 ORDER BY a;
SAVEPOINT s;
INSERT INTO cu_c VALUES (11);
ROLLBACK TO s;
INSERT INTO cu_c VALUES (12);
FETCH ALL FROM c1;
COMMIT;

SELECT kingres.columnar_set_options('cu_c', stripe_row_count => 1000, chunk_row_count => 100);
BEGIN;
INSERT INTO cu_c SELECT 100 + g FROM generate_series(1, 300) g;
DECLARE c2 CURSOR FOR SELECT count(*), sum(a) FROM cu_c WHERE a > 100;
INSERT INTO cu_c SELECT 1000 + g FROM generate_series(1, 5000) g;
FETCH ALL FROM c2;
SAVEPOINT s2;
INSERT INTO cu_c SELECT 10000 + g FROM generate_series(1, 2000) g;
DECLARE c3 CURSOR FOR SELECT count(*) FROM cu_c WHERE a > 10000;
RELEASE s2;
FETCH ALL FROM c3;
COMMIT;
SELECT count(*) FROM cu_c;

-- C1-03: statistics of a column whose type changed without a rewrite
CREATE TABLE st_c (a int) USING columnar;
SELECT kingres.columnar_set_options('st_c', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO st_c SELECT generate_series(-5, 2000);
ALTER TABLE st_c ALTER COLUMN a TYPE oid;
SET enable_seqscan = off;
SELECT count(*) FROM st_c WHERE a > 3000000000::oid;
SELECT count(*) FROM st_c WHERE a = 4294967291::oid;
SELECT count(*) FROM st_c WHERE a < 3::oid;
RESET enable_seqscan;
-- a changed collation of a column
CREATE TABLE co_c (t text COLLATE "C") USING columnar;
SELECT kingres.columnar_set_options('co_c', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO co_c SELECT chr(65 + (g % 26)) || g FROM generate_series(1, 3000) g;
-- (the ICU collation where the server has one; on a server built without ICU the same change to another collation, so that this file needs no ICU)
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_collation WHERE collname = 'en-x-icu' AND collprovider = 'i') THEN
        EXECUTE 'ALTER TABLE co_c ALTER COLUMN t TYPE text COLLATE "en-x-icu"';
    ELSE
        EXECUTE 'ALTER TABLE co_c ALTER COLUMN t TYPE text COLLATE "POSIX"';
    END IF;
END $$;
SELECT count(*) FROM co_c WHERE t > 'Z';
ALTER TABLE co_c ALTER COLUMN t TYPE text COLLATE "C";
SELECT count(*) FROM co_c WHERE t > 'Z';
-- a domain over the type
CREATE DOMAIN dint AS int;
CREATE TABLE do_c (a int) USING columnar;
SELECT kingres.columnar_set_options('do_c', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO do_c SELECT generate_series(1, 2000);
ALTER TABLE do_c ALTER COLUMN a TYPE dint;
SELECT count(*) FROM do_c WHERE a > 1990;

-- C1-05: no path over a columnar relation runs in a worker
CREATE TABLE pp (k int, v int) PARTITION BY RANGE (k);
CREATE TABLE pp_h1 PARTITION OF pp FOR VALUES FROM (0) TO (1000000);
CREATE TABLE pp_c PARTITION OF pp FOR VALUES FROM (1000000) TO (2000000) USING columnar;
INSERT INTO pp SELECT g, g FROM generate_series(0, 99999) g;
INSERT INTO pp_c SELECT 1000000 + g, g FROM generate_series(1, 1000) g;
ANALYZE pp;
SET parallel_setup_cost = 0;
SET parallel_tuple_cost = 0;
SET min_parallel_table_scan_size = 0;
SET min_parallel_index_scan_size = 0;
SET max_parallel_workers_per_gather = 3;
SET debug_parallel_query = on;
SET parallel_leader_participation = off;
BEGIN;
INSERT INTO pp_c SELECT 1500000 + g, g FROM generate_series(1, 10) g;
SELECT count(*) FROM pp;
SELECT count(*) FROM pp_c;
SELECT count(*) FROM (SELECT k FROM pp_c UNION ALL SELECT k FROM pp_h1) u;
SET kingres_columnar.enable_custom_scan = off;
SELECT count(*) FROM pp;
SELECT count(*) FROM pp_c;
SELECT count(*) FROM pp_c a JOIN pp_c b ON a.k = b.k;
EXPLAIN (COSTS OFF) SELECT count(*) FROM pp;
RESET kingres_columnar.enable_custom_scan;
COMMIT;
-- a parallel-safe function that reads the table inside a worker is refused
CREATE FUNCTION pp_count() RETURNS bigint LANGUAGE sql PARALLEL SAFE AS 'SELECT count(*) FROM pp_c';
\set VERBOSITY terse
SELECT count(pp_count()) FROM pp_h1;
\set VERBOSITY default
RESET debug_parallel_query;
RESET parallel_leader_participation;
RESET max_parallel_workers_per_gather;
RESET parallel_setup_cost;
RESET parallel_tuple_cost;
RESET min_parallel_table_scan_size;
RESET min_parallel_index_scan_size;

-- C1-09: the event trigger looks only at the command's own objects
CREATE SCHEMA sx
  CREATE TABLE pk1 (a int PRIMARY KEY)
  CREATE TABLE cfk (a int REFERENCES pk1) USING columnar;
SELECT count(*) FROM pg_class WHERE relname IN ('pk1', 'cfk');
CREATE SCHEMA sy
  CREATE TABLE ty (a int) USING columnar
  CREATE TRIGGER tg AFTER INSERT ON ty FOR EACH ROW EXECUTE FUNCTION suppress_redundant_updates_trigger();
SELECT count(*) FROM pg_class WHERE relname = 'ty';
-- other DDL is not affected by whatever exists
CREATE TABLE unrelated (a int);
ALTER TABLE unrelated ADD COLUMN b int;
DROP TABLE unrelated;
CREATE SCHEMA sz CREATE TABLE tz (a int) USING columnar;
SELECT count(*) FROM sz.tz;
DROP SCHEMA sz CASCADE;

-- C1-10: columnar tables are not published (the wal_level warning differs by major)
SET client_min_messages = error;
CREATE TABLE pub_h (a int);
CREATE TABLE pub_c (a int) USING columnar;
CREATE PUBLICATION pub1 FOR TABLE pub_h;
ALTER PUBLICATION pub1 ADD TABLE pub_c;
CREATE PUBLICATION pub2 FOR TABLE pub_c;
CREATE PUBLICATION pub3 FOR ALL TABLES;
-- with a publication in place, a columnar table cannot join it
CREATE TABLE pub_c2 (a int) USING columnar;
CREATE TABLE pub_h2 (a int);
ALTER TABLE pub_h2 SET ACCESS METHOD columnar;
DROP PUBLICATION pub1;
CREATE PUBLICATION pub4 FOR TABLES IN SCHEMA public;
DROP PUBLICATION pub4;
CREATE SCHEMA pubs;
CREATE PUBLICATION pub5 FOR TABLES IN SCHEMA pubs;
CREATE TABLE pubs.c1 (a int) USING columnar;
CREATE TABLE pubs.h1 (a int);
ALTER TABLE pubs.h1 SET ACCESS METHOD columnar;
DROP PUBLICATION pub5;
DROP SCHEMA pubs CASCADE;

-- clean up
DROP TABLE rw_c, rw_h, rw2, tr_c, cu_c, st_c, co_c, do_c, pp, pub_h, pub_c, pub_c2, pub_h2;
DROP FUNCTION pp_count();
DROP DOMAIN dint;
