-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_unique: unique indexes, primary keys and unique constraints on columnar
-- tables (increment C6c), what is refused, and index vacuum.  The sessions that race
-- are in t/053 to t/056.
SET search_path = public;

CREATE TABLE u (id int, v text) USING columnar;
SELECT kingres.columnar_set_options('u', stripe_row_count => 1000, chunk_row_count => 100);
CREATE UNIQUE INDEX u_id ON u (id);
INSERT INTO u VALUES (1, 'a'), (2, 'b');
-- a duplicate of a committed row, of a row of this transaction, inside one statement
INSERT INTO u VALUES (1, 'dup');
BEGIN;
INSERT INTO u VALUES (3, 'c');
INSERT INTO u VALUES (3, 'again');
ROLLBACK;
INSERT INTO u VALUES (4, 'x'), (4, 'y');
INSERT INTO u SELECT 5, 'z' FROM generate_series(1, 2);
COPY u (id, v) FROM STDIN;
6	one
6	two
\.
SELECT id FROM u ORDER BY id;
-- the failed statements left nothing: a key of a failed statement is free
INSERT INTO u VALUES (3, 'c'), (4, 'x'), (5, 'z'), (6, 'one');
SELECT id, v FROM u ORDER BY id;
-- NULLs are not equal
INSERT INTO u VALUES (NULL, 'n1'), (NULL, 'n2');
SELECT count(*) AS nulls FROM u WHERE id IS NULL;
-- a savepoint that rolled back frees its key, one that was released holds it
BEGIN;
SAVEPOINT s;
INSERT INTO u VALUES (10, 'in savepoint');
ROLLBACK TO s;
INSERT INTO u VALUES (10, 'again');
SAVEPOINT t;
INSERT INTO u VALUES (11, 'x');
RELEASE t;
INSERT INTO u VALUES (11, 'dup of a released savepoint');
ROLLBACK;
SELECT count(*) AS ten_or_eleven FROM u WHERE id IN (10, 11);
-- a PL/pgSQL exception block
DO $$ BEGIN
  INSERT INTO u VALUES (20, 'a');
  BEGIN
    INSERT INTO u VALUES (20, 'b');
  EXCEPTION WHEN unique_violation THEN RAISE NOTICE 'caught';
  END;
END $$;
SELECT count(*) AS twenty FROM u WHERE id = 20;
-- the unique index answers by index scan too
SET enable_seqscan = off;
SET kingres_columnar.enable_custom_scan = off;
SELECT v FROM u WHERE id = 20;
RESET enable_seqscan;
RESET kingres_columnar.enable_custom_scan;

-- a unique index made over duplicates fails with the server's message and leaves nothing
CREATE TABLE ud (id int) USING columnar;
INSERT INTO ud VALUES (1), (2), (2);
CREATE UNIQUE INDEX ud_id ON ud (id);
SELECT count(*) AS indexes_left FROM pg_index WHERE indrelid = 'ud'::regclass;
DELETE FROM ud WHERE false;
TRUNCATE ud;
INSERT INTO ud VALUES (1), (2);
CREATE UNIQUE INDEX ud_id ON ud (id);
-- a unique index made while another transaction holds rows of the table waits for it (see t/053)

-- primary keys and unique constraints
CREATE TABLE pk (id int PRIMARY KEY, v text) USING columnar;
INSERT INTO pk VALUES (1, 'a');
INSERT INTO pk VALUES (1, 'b');
INSERT INTO pk VALUES (NULL, 'c');
CREATE TABLE pk2 (a int, b int, v text, UNIQUE (a, b)) USING columnar;
INSERT INTO pk2 VALUES (1, 1, 'x'), (1, 2, 'y'), (2, 1, 'z');
INSERT INTO pk2 VALUES (1, 2, 'dup');
CREATE TABLE pk3 (id int, v text) USING columnar;
INSERT INTO pk3 VALUES (1, 'a'), (2, 'b');
ALTER TABLE pk3 ADD PRIMARY KEY (id);
INSERT INTO pk3 VALUES (2, 'dup');
ALTER TABLE pk3 ADD UNIQUE (v);
INSERT INTO pk3 VALUES (3, 'a');
ALTER TABLE pk3 DROP CONSTRAINT pk3_pkey;
INSERT INTO pk3 VALUES (2, 'c');
-- a partial and an expression unique index
CREATE TABLE pu (id int, g int, t text) USING columnar;
CREATE UNIQUE INDEX pu_part ON pu (id) WHERE g = 1;
CREATE UNIQUE INDEX pu_expr ON pu (lower(t));
INSERT INTO pu VALUES (1, 1, 'A'), (1, 2, 'B'), (2, 2, 'C');
INSERT INTO pu VALUES (1, 1, 'D');
INSERT INTO pu VALUES (9, 9, 'a');

-- refused, with a message that says why
CREATE TABLE ref_to (id int PRIMARY KEY) USING columnar;
CREATE TABLE ref_from (id int REFERENCES ref_to (id));
CREATE TABLE ref_h (id int PRIMARY KEY);
CREATE TABLE ref_c (id int REFERENCES ref_h (id)) USING columnar;
ALTER TABLE pk3 ADD CONSTRAINT pk3_def UNIQUE (id) DEFERRABLE;
ALTER TABLE pk3 ADD CONSTRAINT pk3_def2 UNIQUE (id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE pk3 ADD EXCLUDE USING btree (id WITH =);
INSERT INTO u VALUES (1, 'x') ON CONFLICT DO NOTHING;
INSERT INTO u VALUES (1, 'x') ON CONFLICT (id) DO NOTHING;
INSERT INTO u VALUES (1, 'x') ON CONFLICT (id) DO UPDATE SET v = 'y';
-- row locks and updates stay refused
UPDATE u SET v = 'q' WHERE id = 1;
SELECT * FROM u WHERE id = 1 FOR UPDATE;

-- index vacuum: the entries of rolled back inserts go, the rows stay, the answers do not change
CREATE TABLE iv (id int, v text) USING columnar;
SELECT kingres.columnar_set_options('iv', stripe_row_count => 1000, chunk_row_count => 100);
CREATE INDEX iv_id ON iv (id);
CREATE UNIQUE INDEX iv_u ON iv (v);
INSERT INTO iv SELECT g, 'k' || g FROM generate_series(1, 3000) g;
BEGIN;
INSERT INTO iv SELECT g, 'r' || g FROM generate_series(5001, 8000) g;
ROLLBACK;
SELECT idx_tup_read IS NOT NULL AS stats_ok FROM pg_stat_user_indexes WHERE indexrelname = 'iv_id';
SELECT (SELECT reltuples FROM pg_class WHERE relname = 'iv_id') AS before_vacuum_tuples;
VACUUM iv;
SELECT (SELECT reltuples FROM pg_class WHERE relname = 'iv_id') AS after_vacuum_tuples;
SELECT (SELECT reltuples FROM pg_class WHERE relname = 'iv_u') AS after_vacuum_unique_tuples;
SET enable_seqscan = off;
SET kingres_columnar.enable_custom_scan = off;
SELECT count(*) AS by_index FROM iv WHERE id > 0;
SELECT count(*) AS rolled_back_rows FROM iv WHERE id > 5000;
RESET enable_seqscan;
RESET kingres_columnar.enable_custom_scan;
-- the keys of the rolled back rows are free and the entries are not in the way
INSERT INTO iv SELECT g, 'r' || g FROM generate_series(5001, 5010) g;
SELECT count(*) FROM iv;
VACUUM (INDEX_CLEANUP off) iv;
VACUUM (INDEX_CLEANUP on) iv;
SELECT * FROM kingres.columnar_check();
SELECT * FROM kingres.columnar_check_indexes('iv');
SELECT * FROM kingres.columnar_check_indexes('pu');
