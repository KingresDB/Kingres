-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_compact_index: compaction of a table that has indexes (increment C6d).  The
-- copies of the rows have new row numbers, and every index gets an entry for each;
-- the answers of the indexes are those of the table before and after, a unique index
-- still refuses a duplicate, and VACUUM removes the entries of the old numbers.  The
-- sessions that race with a compaction are in t/059 and t/060.
SET search_path = public;

CREATE TABLE cz (id int, g int, v text) USING columnar;
CREATE UNIQUE INDEX cz_id ON cz (id);
CREATE INDEX cz_g ON cz (g, id) WHERE g > 3;
CREATE INDEX cz_e ON cz ((id % 7), v);
-- twenty small stripes
SELECT format('INSERT INTO cz SELECT %s * 100 + n, n %% 10, ''v'' || (%s * 100 + n) FROM generate_series(1, 100) n', i, i) FROM generate_series(0, 19) i \gexec
SELECT count(*) AS stripes_before FROM kingres.columnar_stripes('cz');
CREATE TEMP TABLE before AS SELECT id, g, v FROM cz;

SELECT kingres.columnar_compact('cz', 1000000) > 0 AS compacted;
SELECT count(*) AS stripes_after FROM kingres.columnar_stripes('cz');

-- the table and every index answer as before
SELECT count(*) = (SELECT count(*) FROM before) AS same_count FROM cz;
SELECT count(*) AS rows_not_in_before FROM (SELECT id, g, v FROM cz EXCEPT SELECT id, g, v FROM before) s;
SELECT count(*) AS rows_not_in_table FROM (SELECT id, g, v FROM before EXCEPT SELECT id, g, v FROM cz) s;
SET enable_seqscan = off;
SET enable_bitmapscan = off;
SELECT v FROM cz WHERE id = 1234;
SELECT count(*), sum(id) FROM cz WHERE id BETWEEN 500 AND 1500;
SELECT count(*), sum(id) FROM cz WHERE g > 3 AND id < 700;
SELECT count(*) FROM cz WHERE id % 7 = 3 AND v LIKE 'v1%';
RESET enable_seqscan;
RESET enable_bitmapscan;
SELECT count(*) AS index_entries_missing FROM kingres.columnar_check_indexes('cz');

-- the unique index refuses a duplicate of a copied row, and takes a new key
INSERT INTO cz VALUES (1234, 0, 'again');
INSERT INTO cz VALUES (999999, 0, 'new');
SELECT count(*) AS rows_now FROM cz;

-- a compaction that rolls back leaves the table as it was
INSERT INTO cz SELECT 2000 + n, 1, 'w' FROM generate_series(1, 10) n;
INSERT INTO cz SELECT 3000 + n, 1, 'w' FROM generate_series(1, 10) n;
BEGIN;
SELECT kingres.columnar_compact('cz', 1000000) >= 0 AS compacted_again;
ROLLBACK;
SELECT count(*) AS rows_after_rollback FROM cz;
SELECT count(*) AS index_entries_missing FROM kingres.columnar_check_indexes('cz');
INSERT INTO cz VALUES (2001, 0, 'dup after rollback');

-- VACUUM removes the entries of the numbers that were replaced
VACUUM cz;
SELECT count(*) AS index_entries_missing FROM kingres.columnar_check_indexes('cz');
SET enable_seqscan = off;
SELECT count(*), sum(id) FROM cz WHERE id BETWEEN 1 AND 100000;
RESET enable_seqscan;
SELECT count(*), sum(id) FROM cz WHERE id BETWEEN 1 AND 100000;

-- compaction inside a transaction that wrote rows, and a table with an index made in it
BEGIN;
CREATE TABLE cw (id int, v text) USING columnar;
CREATE UNIQUE INDEX cw_id ON cw (id);
INSERT INTO cw VALUES (1, 'a');
INSERT INTO cw VALUES (2, 'b');
COMMIT;
INSERT INTO cw VALUES (3, 'c');
SELECT kingres.columnar_compact('cw', 1000000);
SELECT * FROM cw ORDER BY id;
INSERT INTO cw VALUES (2, 'dup');

DROP TABLE cz, cw;
