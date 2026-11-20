-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_refusals: what a columnar table does not do, with the SQLSTATE and
-- the message, and the statements that look like writes but touch no row.
SET search_path = public;

-- run one statement; report the SQLSTATE, message and hint of its error, or ok
CREATE FUNCTION kc_try(stmt text) RETURNS TABLE (state text, message text, hint text)
LANGUAGE plpgsql AS $$
BEGIN
    EXECUTE stmt;
    state := '00000'; message := 'ok'; hint := NULL;
    RETURN NEXT;
EXCEPTION WHEN OTHERS THEN
    GET STACKED DIAGNOSTICS state = RETURNED_SQLSTATE, message = MESSAGE_TEXT, hint = PG_EXCEPTION_HINT;
    RETURN NEXT;
END $$;

CREATE TABLE r1 (id int, s text) USING columnar;
INSERT INTO r1 SELECT i, 'x' || i FROM generate_series(1, 100) i;
CREATE TABLE src (id int, s text);
INSERT INTO src VALUES (1, 'one'), (1000, 'thousand');

-- UPDATE, DELETE and MERGE: refused as soon as a row would be written; the message of the
-- statements that fetch the old row first (RETURNING, joins, MERGE) differs, the SQLSTATE does not
UPDATE r1 SET s = 'u' WHERE id = 1;
DELETE FROM r1 WHERE id = 1;
UPDATE r1 SET s = 'u';
DELETE FROM r1;
SELECT state FROM kc_try($$UPDATE r1 SET s = 'u' WHERE id = 1 RETURNING *$$);
SELECT state FROM kc_try($$UPDATE r1 SET s = src.s FROM src WHERE r1.id = src.id$$);
SELECT state FROM kc_try($$DELETE FROM r1 USING src WHERE r1.id = src.id$$);
SELECT state FROM kc_try($$MERGE INTO r1 t USING src s ON t.id = s.id WHEN MATCHED THEN UPDATE SET s = 'm'$$);
SELECT state FROM kc_try($$MERGE INTO r1 t USING src s ON t.id = s.id WHEN MATCHED THEN DELETE$$);
SELECT state FROM kc_try($$MERGE INTO r1 t USING src s ON t.id = s.id
  WHEN MATCHED THEN UPDATE SET s = 'm' WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.s)$$);
-- statements that write no row succeed
UPDATE r1 SET s = 'u' WHERE false;
UPDATE r1 SET s = 'u' WHERE id = -1;
DELETE FROM r1 WHERE id = -1;
SELECT * FROM kc_try($$MERGE INTO r1 t USING src s ON t.id = s.id + 5000 WHEN MATCHED THEN UPDATE SET s = 'm'$$);
SELECT * FROM kc_try($$MERGE INTO r1 t USING src s ON t.id = s.id + 5000 WHEN MATCHED THEN DELETE$$);
-- MERGE that only inserts
MERGE INTO r1 t USING src s ON t.id = s.id + 5000 WHEN NOT MATCHED THEN INSERT VALUES (s.id + 5000, s.s);
MERGE INTO r1 t USING src s ON t.id = s.id + 8000 WHEN MATCHED THEN UPDATE SET s = 'm'
  WHEN NOT MATCHED THEN INSERT VALUES (s.id + 6000, s.s);
SELECT id, s FROM r1 WHERE id > 1000 ORDER BY id;
SELECT count(*) FROM r1;

-- row locks
SELECT * FROM r1 WHERE id = 1 FOR UPDATE;
SELECT * FROM kc_try($$SELECT * FROM r1 FOR UPDATE$$);
SELECT * FROM kc_try($$SELECT * FROM r1 FOR NO KEY UPDATE$$);
SELECT * FROM kc_try($$SELECT * FROM r1 FOR SHARE$$);
SELECT * FROM kc_try($$SELECT * FROM r1 FOR KEY SHARE$$);
SELECT * FROM kc_try($$SELECT * FROM r1 FOR UPDATE SKIP LOCKED$$);
SELECT * FROM kc_try($$SELECT * FROM r1 FOR UPDATE NOWAIT$$);
SELECT * FROM kc_try($$SELECT * FROM r1 JOIN src USING (id) FOR UPDATE OF src$$);
SELECT * FROM kc_try($$SELECT * FROM r1 JOIN src USING (id) FOR UPDATE OF r1$$);
SELECT * FROM kc_try($$SELECT * FROM r1 JOIN src USING (id) FOR SHARE$$);
-- locking no row is fine, and so is locking another table
SELECT * FROM r1 WHERE id = -1 FOR UPDATE;
SELECT * FROM kc_try($$SELECT * FROM r1 JOIN src USING (id) WHERE r1.id < 0 FOR UPDATE OF src$$);
SELECT * FROM kc_try($$SELECT * FROM src WHERE id IN (SELECT id FROM r1) FOR UPDATE$$);
LOCK TABLE r1 IN ACCESS EXCLUSIVE MODE NOWAIT;
BEGIN;
LOCK TABLE r1 IN ACCESS EXCLUSIVE MODE;
SELECT count(*) FROM r1;
COMMIT;

-- ON CONFLICT: with no unique index there is nothing to infer, DO NOTHING without a target works
INSERT INTO r1 VALUES (1, 'dup') ON CONFLICT DO NOTHING;
SELECT count(*) FROM r1 WHERE id = 1;
INSERT INTO r1 VALUES (1, 'dup') ON CONFLICT (id) DO NOTHING;
INSERT INTO r1 VALUES (1, 'dup') ON CONFLICT (id) DO UPDATE SET s = 'y';
INSERT INTO r1 VALUES (1, 'dup') ON CONFLICT DO UPDATE SET s = 'y';
INSERT INTO r1 VALUES (1, 'dup') ON CONFLICT ON CONSTRAINT r1_pkey DO NOTHING;
INSERT INTO r1 VALUES (7777, 'ret') RETURNING id, s;

-- indexes, primary keys and unique constraints
CREATE INDEX r1_i ON r1 (id);
DROP INDEX r1_i;
CREATE INDEX r1_hash ON r1 USING hash (id);
CREATE INDEX r1_brin ON r1 USING brin (id);
CREATE UNIQUE INDEX r1_u ON r1 (id);
CREATE INDEX CONCURRENTLY r1_ic ON r1 (id);
-- the refused concurrent build leaves nothing behind
SELECT count(*) AS invalid_left FROM pg_index WHERE indrelid = 'r1'::regclass;
CREATE TABLE r2 (id int PRIMARY KEY, s text) USING columnar;
DROP TABLE r2;
CREATE TABLE r2 (id int UNIQUE, s text) USING columnar;
DROP TABLE r2;
CREATE TABLE r2 (id int, s text, EXCLUDE USING btree (id WITH =)) USING columnar;
CREATE TABLE r2 (id int, s text) USING columnar;
ALTER TABLE r2 ADD PRIMARY KEY (id);
ALTER TABLE r2 DROP CONSTRAINT r2_pkey;
ALTER TABLE r2 ADD UNIQUE (id);
ALTER TABLE r2 DROP CONSTRAINT r2_id_key;
ALTER TABLE r2 ADD EXCLUDE USING btree (id WITH =);
ALTER TABLE r2 ADD CONSTRAINT r2_u UNIQUE USING INDEX r1_nonexistent;
SELECT count(*) AS indexes_on_columnar FROM pg_index WHERE indrelid IN ('r1'::regclass, 'r2'::regclass);
-- a heap table with a primary key or another B-tree index becomes columnar (its indexes are rebuilt)
CREATE TABLE r3 (id int PRIMARY KEY, s text);
ALTER TABLE r3 SET ACCESS METHOD columnar;
CREATE TABLE r3b (id int, s text);
CREATE INDEX r3b_i ON r3b (id);
ALTER TABLE r3b SET ACCESS METHOD columnar;
DROP INDEX r3b_i;
ALTER TABLE r3b SET ACCESS METHOD columnar;
SELECT a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'r3b'::regclass;
CREATE TABLE r3c (LIKE r3 INCLUDING ALL) USING columnar;
DROP TABLE r3c;
CREATE TABLE r3c (LIKE r3 INCLUDING DEFAULTS INCLUDING CONSTRAINTS) USING columnar;
SELECT a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'r3c'::regclass;
CREATE MATERIALIZED VIEW mv USING columnar AS SELECT id, s FROM src;
CREATE UNIQUE INDEX mv_u ON mv (id);
REFRESH MATERIALIZED VIEW CONCURRENTLY mv;
REFRESH MATERIALIZED VIEW mv;
SELECT count(*) FROM mv;
-- no index, no CLUSTER
CLUSTER r1;
CLUSTER r1 USING r1_i;
VACUUM FULL r1;
REINDEX TABLE r1;

-- TABLESAMPLE
SELECT count(*) FROM r1 TABLESAMPLE SYSTEM (50);
SELECT count(*) FROM r1 TABLESAMPLE BERNOULLI (50) REPEATABLE (1);

-- TID scans: a row of a columnar table cannot be fetched by its TID (the
-- Columnar Scan filters on the synthetic ctid instead, so switch it off)
SET kingres_columnar.enable_custom_scan = off;
SET enable_seqscan = off;
SELECT state FROM kc_try($$SELECT count(*) FROM r1 WHERE ctid = '(0,6)'$$);
RESET kingres_columnar.enable_custom_scan;
RESET enable_seqscan;

-- temporary and unlogged tables
CREATE TEMP TABLE t_tmp (a int) USING columnar;
CREATE TEMPORARY TABLE t_tmp USING columnar AS SELECT 1 AS a;
CREATE UNLOGGED TABLE t_unl (a int) USING columnar;
CREATE UNLOGGED TABLE t_unl USING columnar AS SELECT 1 AS a;
ALTER TABLE r1 SET UNLOGGED;
CREATE TEMP TABLE t_heap (a int);
ALTER TABLE t_heap SET ACCESS METHOD columnar;
CREATE UNLOGGED TABLE t_heap_unl (a int);
ALTER TABLE t_heap_unl SET ACCESS METHOD columnar;
ALTER TABLE t_heap_unl SET LOGGED;
ALTER TABLE t_heap_unl SET ACCESS METHOD columnar;
SELECT a.amname, c.relpersistence FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 't_heap_unl'::regclass;
SET default_table_access_method = columnar;
CREATE TEMP TABLE t_tmp2 (a int);
CREATE TABLE t_def (a int);
RESET default_table_access_method;
SELECT a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 't_def'::regclass;
DROP TABLE t_def, t_heap_unl;

-- foreign keys, both directions
CREATE TABLE pk_heap (id int PRIMARY KEY);
CREATE TABLE fk_col (id int REFERENCES pk_heap (id)) USING columnar;
CREATE TABLE fk_col (id int) USING columnar;
ALTER TABLE fk_col ADD FOREIGN KEY (id) REFERENCES pk_heap (id);
ALTER TABLE fk_col ADD CONSTRAINT fk_col_fk FOREIGN KEY (id) REFERENCES pk_heap (id) NOT VALID;
CREATE TABLE fk_heap (id int REFERENCES r1 (id));
CREATE TABLE fk_heap (id int);
ALTER TABLE fk_heap ADD FOREIGN KEY (id) REFERENCES r1 (id);
SELECT count(*) AS foreign_keys FROM pg_constraint WHERE contype = 'f' AND conrelid IN ('fk_col'::regclass, 'fk_heap'::regclass);
DROP TABLE fk_col, fk_heap, pk_heap;

-- triggers
CREATE FUNCTION trg_row() RETURNS trigger LANGUAGE plpgsql AS
$$ BEGIN NEW.s := 'by ' || TG_NAME; RETURN NEW; END $$;
CREATE FUNCTION trg_stmt() RETURNS trigger LANGUAGE plpgsql AS
$$ BEGIN RAISE NOTICE '% % % rows %', TG_NAME, TG_WHEN, TG_OP, (SELECT count(*) FROM new_rows); RETURN NULL; END $$;
CREATE FUNCTION trg_stmt_plain() RETURNS trigger LANGUAGE plpgsql AS
$$ BEGIN RAISE NOTICE '% % % %', TG_NAME, TG_WHEN, TG_OP, TG_LEVEL; RETURN NULL; END $$;
CREATE TABLE tg (id int, s text) USING columnar;
CREATE TRIGGER tg_before_row BEFORE INSERT ON tg FOR EACH ROW EXECUTE FUNCTION trg_row();
CREATE TRIGGER tg_before_stmt BEFORE INSERT ON tg FOR EACH STATEMENT EXECUTE FUNCTION trg_stmt_plain();
CREATE TRIGGER tg_after_stmt AFTER INSERT ON tg FOR EACH STATEMENT EXECUTE FUNCTION trg_stmt_plain();
CREATE TRIGGER tg_transition AFTER INSERT ON tg REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION trg_stmt();
CREATE TRIGGER tg_truncate AFTER TRUNCATE ON tg FOR EACH STATEMENT EXECUTE FUNCTION trg_stmt_plain();
INSERT INTO tg VALUES (1, 'a'), (2, 'b');
COPY tg FROM STDIN;
3	c
4	d
\.
SELECT id, s FROM tg ORDER BY id;
CREATE TRIGGER tg_after_row AFTER INSERT ON tg FOR EACH ROW EXECUTE FUNCTION trg_stmt_plain();
CREATE TRIGGER tg_after_row_upd AFTER UPDATE ON tg FOR EACH ROW EXECUTE FUNCTION trg_stmt_plain();
CREATE TRIGGER tg_after_row_del AFTER DELETE ON tg FOR EACH ROW EXECUTE FUNCTION trg_stmt_plain();
CREATE CONSTRAINT TRIGGER tg_constraint AFTER INSERT ON tg FOR EACH ROW EXECUTE FUNCTION trg_stmt_plain();
CREATE TRIGGER tg_before_upd BEFORE UPDATE ON tg FOR EACH ROW EXECUTE FUNCTION trg_row();
CREATE TRIGGER tg_before_del BEFORE DELETE ON tg FOR EACH ROW EXECUTE FUNCTION trg_row();
SELECT tgname FROM pg_trigger WHERE tgrelid = 'tg'::regclass AND NOT tgisinternal ORDER BY tgname;
TRUNCATE tg;
-- a heap table with an AFTER ROW trigger cannot become columnar
CREATE TABLE tg_heap (id int);
CREATE TRIGGER tg_heap_after AFTER INSERT ON tg_heap FOR EACH ROW EXECUTE FUNCTION trg_stmt_plain();
ALTER TABLE tg_heap SET ACCESS METHOD columnar;
DROP TRIGGER tg_heap_after ON tg_heap;
ALTER TABLE tg_heap SET ACCESS METHOD columnar;
-- the trigger of a partitioned parent is cloned to the columnar partition
CREATE TABLE tg_p (id int) PARTITION BY LIST (id);
CREATE TRIGGER tg_p_after AFTER INSERT ON tg_p FOR EACH ROW EXECUTE FUNCTION trg_stmt_plain();
CREATE TABLE tg_p1 PARTITION OF tg_p FOR VALUES IN (1) USING columnar;
CREATE TABLE tg_p2 PARTITION OF tg_p FOR VALUES IN (2);
CREATE TABLE tg_p3 (id int) USING columnar;
ALTER TABLE tg_p ATTACH PARTITION tg_p3 FOR VALUES IN (3);
SELECT c.relname, a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.relname LIKE 'tg_p%' AND c.relkind = 'r' ORDER BY 1;
DROP TABLE tg_p, tg_p3;
DROP TABLE tg, tg_heap;

-- the event triggers fire in every session_replication_role
SET session_replication_role = replica;
CREATE TABLE rr (id int) USING columnar;
CREATE FUNCTION trg_rr() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
CREATE TRIGGER rr_after AFTER INSERT ON rr FOR EACH ROW EXECUTE FUNCTION trg_rr();
CREATE TABLE pk_rr (id int PRIMARY KEY);
CREATE TABLE rr_fk (id int REFERENCES pk_rr (id)) USING columnar;
RESET session_replication_role;
DROP TABLE rr, pk_rr;
DROP FUNCTION trg_rr();

-- TRUNCATE in a subtransaction, after the transaction wrote rows at an outer level
CREATE TABLE tr (id int) USING columnar;
BEGIN;
INSERT INTO tr VALUES (1);
SAVEPOINT s;
TRUNCATE tr;
ROLLBACK;
BEGIN;
SAVEPOINT a;
INSERT INTO tr VALUES (1);
SAVEPOINT b;
TRUNCATE tr;
ROLLBACK;
-- the same level works, and so does a table created in the transaction
BEGIN;
SAVEPOINT a;
INSERT INTO tr VALUES (2);
TRUNCATE tr;
INSERT INTO tr VALUES (3);
COMMIT;
SELECT * FROM tr;
BEGIN;
CREATE TABLE tr2 (id int) USING columnar;
SAVEPOINT s;
INSERT INTO tr2 VALUES (1);
TRUNCATE tr2;
INSERT INTO tr2 VALUES (2);
RELEASE s;
COMMIT;
SELECT * FROM tr2;

-- partitioned tables with a columnar partition
CREATE TABLE pp (id int, v int) PARTITION BY RANGE (id);
CREATE TABLE pp_col PARTITION OF pp FOR VALUES FROM (0) TO (100) USING columnar;
CREATE TABLE pp_heap PARTITION OF pp FOR VALUES FROM (100) TO (200);
INSERT INTO pp SELECT i, i FROM generate_series(0, 199) i;
UPDATE pp SET v = -1 WHERE id = 150;
SELECT * FROM pp WHERE v = -1;
UPDATE pp SET v = -2 WHERE id = 50;
UPDATE pp SET v = -3 WHERE id = 500;
UPDATE pp SET v = -4 WHERE v = 12345;
DELETE FROM pp WHERE id >= 100 AND id < 120;
DELETE FROM pp WHERE v = -1000;
DELETE FROM pp WHERE id = 5;
SELECT * FROM kc_try($$SELECT * FROM pp WHERE id = 5 FOR UPDATE$$);
SELECT * FROM kc_try($$SELECT * FROM pp WHERE id = 150 FOR UPDATE$$);
SELECT count(*) FROM pp;
-- moving a row to a columnar partition works; moving one out of it deletes it there
CREATE TABLE ppm (id int, v int) PARTITION BY RANGE (id);
CREATE TABLE ppm_col PARTITION OF ppm FOR VALUES FROM (0) TO (100) USING columnar;
CREATE TABLE ppm_heap PARTITION OF ppm FOR VALUES FROM (100) TO (200);
INSERT INTO ppm VALUES (150, 1);
UPDATE ppm SET id = 50 WHERE id = 150;
SELECT tableoid::regclass, id FROM ppm;
UPDATE ppm SET id = 150 WHERE id = 50;
SELECT tableoid::regclass, id FROM ppm;
DROP TABLE ppm, pp;

-- options functions on tables that are not columnar
SELECT kingres.columnar_set_options('src', compression => 'none');
SELECT * FROM kingres.columnar_options('src');
SELECT * FROM kingres.columnar_info('src');
SELECT * FROM kingres.columnar_stripes('src');
SELECT kingres.columnar_compact('src');
CREATE TABLE ppo (id int) PARTITION BY RANGE (id);
SELECT * FROM kingres.columnar_info('ppo');
SELECT kingres.columnar_set_options('mv', compression => 'none');
SELECT * FROM kingres.columnar_options('mv');
SELECT kingres.columnar_set_options('no_such_table');
DROP TABLE ppo;

DROP MATERIALIZED VIEW mv;
DROP TABLE r1, r2, r3, r3b, r3c, tr, tr2, src;
DROP FUNCTION kc_try(text), trg_row(), trg_stmt(), trg_stmt_plain();
SELECT count(*) AS catalog_rows FROM kingres.columnar_stripe;
