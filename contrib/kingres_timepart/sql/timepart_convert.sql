-- timepart_convert: conversion of a partition to columnar storage and back
-- (design 10.4), by hand (convert_partition, convert_old_partitions) and by a
-- policy (columnar_after, timepart_run).  Every refusal with its message, the
-- checks that the copy is whole, the privileges and comments that the new
-- partition carries, who may convert, hostile names and search paths.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

SET client_min_messages = warning;
CREATE EXTENSION kingres_columnar;
RESET client_min_messages;
SELECT extversion FROM pg_extension WHERE extname = 'kingres_timepart';
SET client_min_messages = warning;
DROP ROLE IF EXISTS tpv_owner, tpv_other, tpv_member, tpv_noinherit, tpv_nocall, tpv_su;
RESET client_min_messages;
CREATE ROLE tpv_owner NOLOGIN;
CREATE ROLE tpv_other NOLOGIN;
CREATE ROLE tpv_member NOLOGIN IN ROLE tpv_owner;
CREATE ROLE tpv_noinherit NOLOGIN NOINHERIT IN ROLE tpv_owner;
CREATE ROLE tpv_su NOLOGIN SUPERUSER;
CREATE SCHEMA tpv;
GRANT USAGE ON SCHEMA tpv TO tpv_owner, tpv_other, tpv_member, tpv_noinherit;
GRANT CREATE ON SCHEMA tpv TO tpv_owner;
GRANT USAGE ON SCHEMA kingres TO tpv_owner, tpv_other, tpv_member, tpv_noinherit;
GRANT EXECUTE ON FUNCTION
    kingres.timepart_set_policy(regclass, interval, interval, interval, text, interval, text[], jsonb, text, interval, boolean),
    kingres.create_time_partitions(regclass, interval, timestamptz, timestamptz, name, text)
  TO tpv_owner;

-- Helpers: a digest of every row of a table (rows compared as text), the
-- number of chunk groups a query skips, and the partitions with their method.
CREATE FUNCTION tpv.digest(rel regclass) RETURNS text LANGUAGE plpgsql AS $$
DECLARE r text;
BEGIN
    EXECUTE format('SELECT count(*) || '' rows, '' || coalesce(md5(string_agg(t::text, ''|'' ORDER BY t::text)), ''-'') FROM %s t', rel) INTO r;
    RETURN r;
END $$;
CREATE FUNCTION tpv.skipped(q text) RETURNS text LANGUAGE plpgsql AS $$
DECLARE l text; r text := '';
BEGIN
    FOR l IN EXECUTE 'EXPLAIN (ANALYZE, COSTS off, TIMING off, SUMMARY off, BUFFERS off) ' || q LOOP
        IF l LIKE '%Chunk Groups Skipped%' THEN r := btrim(l); END IF;
    END LOOP;
    RETURN r;
END $$;
CREATE FUNCTION tpv.acl(rel regclass) RETURNS text LANGUAGE sql STABLE AS $$
    SELECT coalesce(regexp_replace(relacl::text, relowner::regrole::text, 'OWNER', 'g'), 'default') FROM pg_class WHERE oid = rel
$$;
CREATE FUNCTION tpv.methods(rel regclass) RETURNS TABLE (part name, am name, bounds text)
LANGUAGE sql STABLE AS $$
    SELECT c.relname, a.amname, regexp_replace(pg_get_expr(c.relpartbound, c.oid), '^FOR VALUES ', '')
      FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_am a ON a.oid = c.relam
     WHERE i.inhparent = rel ORDER BY c.relname
$$;
CREATE FUNCTION tpv.actions(p_table name) RETURNS TABLE (kind text, part name, state text, detail text)
LANGUAGE sql STABLE AS $$
    SELECT a.kind, a.partition_name, a.state, regexp_replace(a.detail, ', compression ratio.*$', '')
      FROM kingres.timepart_run_action a
     WHERE a.run_id = (SELECT max(r.run_id) FROM kingres.timepart_run r WHERE r.schema_name = 'tpv' AND r.table_name = p_table)
     ORDER BY a.seq
$$;

------------------------------------------------------------------------
-- 0. A publication that would take in the columnar partition (columnar
--    tables are not replicated logically yet; the publication has to be there
--    first, since a columnar table cannot be put in one)
------------------------------------------------------------------------
CREATE SCHEMA tpp;
CREATE TABLE tpp.pr (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpp.pr_a PARTITION OF tpp.pr FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
SET client_min_messages = error;
CREATE PUBLICATION pub_all FOR ALL TABLES;
RESET client_min_messages;
CALL kingres.convert_partition('tpp.pr', 'tpp.pr_a');
DROP PUBLICATION pub_all;
SET client_min_messages = error;
CREATE PUBLICATION pub_schema FOR TABLES IN SCHEMA tpp;
RESET client_min_messages;
CALL kingres.convert_partition('tpp.pr', 'tpp.pr_a');
DROP PUBLICATION pub_schema;
SET client_min_messages = error;
CREATE PUBLICATION pub_parent FOR TABLE tpp.pr;
RESET client_min_messages;
CALL kingres.convert_partition('tpp.pr', 'tpp.pr_a');
DROP PUBLICATION pub_parent;
DROP SCHEMA tpp CASCADE;

------------------------------------------------------------------------
-- 1. One partition to columnar, with every kind of value
------------------------------------------------------------------------
CREATE TABLE tpv.ev (
    id    bigint NOT NULL,
    ts    timestamptz NOT NULL,
    dev   int,
    val   numeric(12,3),
    flag  boolean,
    tags  text[],
    doc   jsonb,
    note  text,
    dbl   float8 GENERATED ALWAYS AS (dev * 2.5) STORED,
    CONSTRAINT ev_val_check CHECK (val IS NULL OR val > -1000000)
) PARTITION BY RANGE (ts);
COMMENT ON COLUMN tpv.ev.note IS 'free text';
SELECT kingres.create_time_partitions('tpv.ev', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
INSERT INTO tpv.ev (id, ts, dev, val, flag, tags, doc, note)
SELECT i, '2026-03-01 UTC'::timestamptz + (i % 5) * interval '1 day' + (i * 37 % 86400) * interval '1 second',
       CASE WHEN i % 11 = 0 THEN NULL ELSE i % 13 END,
       CASE WHEN i % 7 = 0 THEN NULL ELSE i * 1.125 END,
       CASE WHEN i % 5 = 0 THEN NULL ELSE i % 2 = 0 END,
       CASE WHEN i % 3 = 0 THEN NULL ELSE ARRAY['a' || i, 'b', NULL] END,
       CASE WHEN i % 4 = 0 THEN NULL ELSE jsonb_build_object('i', i, 'k', ARRAY[i, i + 1]) END,
       CASE WHEN i % 17 = 0 THEN NULL WHEN i % 19 = 0 THEN repeat('long text ', 500) ELSE 'row ' || i END
  FROM generate_series(1, 5000) i;
ALTER TABLE tpv.ev OWNER TO tpv_owner;
ALTER TABLE tpv.ev_p20260301 OWNER TO tpv_owner;
ALTER TABLE tpv.ev_p20260302 OWNER TO tpv_owner;
ALTER TABLE tpv.ev_p20260303 OWNER TO tpv_owner;
ALTER TABLE tpv.ev_p20260304 OWNER TO tpv_owner;
ALTER TABLE tpv.ev_p20260305 OWNER TO tpv_owner;
GRANT SELECT ON tpv.ev_p20260302 TO tpv_other WITH GRANT OPTION;
GRANT INSERT ON tpv.ev_p20260302 TO PUBLIC;
GRANT UPDATE (note) ON tpv.ev_p20260302 TO tpv_other;
COMMENT ON TABLE tpv.ev_p20260302 IS 'the second day';
SELECT tpv.digest('tpv.ev_p20260302') AS heap_digest \gset
SELECT tpv.digest('tpv.ev') AS table_digest \gset
SELECT * FROM tpv.methods('tpv.ev');

CALL kingres.convert_partition('tpv.ev', 'tpv.ev_p20260302', 'columnar', '{dev,ts}', '{"compression": "pglz", "compression_level": 5, "chunk_row_count": 200}');
SELECT * FROM tpv.methods('tpv.ev');
-- the rows are the same, in the table and in the partition, and the partition is
-- where it was: same name, same bounds, owner, comment and privileges
SELECT tpv.digest('tpv.ev_p20260302') = :'heap_digest' AS partition_same_rows,
       tpv.digest('tpv.ev') = :'table_digest' AS table_same_rows;
SELECT count(*) AS rows_in_partition, (SELECT i.rows FROM kingres.columnar_info('tpv.ev_p20260302') i) AS rows_in_stripes FROM tpv.ev_p20260302;
SELECT relname, relowner::regrole AS owner, relkind, relispartition, obj_description(oid, 'pg_class') AS comment
  FROM pg_class WHERE oid = 'tpv.ev_p20260302'::regclass;
SELECT tpv.acl('tpv.ev_p20260302');
SELECT attname, attacl FROM pg_attribute WHERE attrelid = 'tpv.ev_p20260302'::regclass AND attacl IS NOT NULL;
SELECT a.attname, col_description(a.attrelid, a.attnum) AS comment FROM pg_attribute a
 WHERE a.attrelid = 'tpv.ev_p20260302'::regclass AND a.attnum > 0 AND NOT a.attisdropped AND col_description(a.attrelid, a.attnum) IS NOT NULL;
SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'tpv.ev_p20260302'::regclass ORDER BY conname;
SELECT * FROM kingres.columnar_options('tpv.ev_p20260302');
-- no helper object is left behind
SELECT count(*) AS leftovers FROM pg_class WHERE relname LIKE '%\_\_kc' AND relnamespace = 'tpv'::regnamespace;
SELECT count(*) AS helper_checks FROM pg_constraint WHERE conname = 'timepart_convert_bound';
-- the parent still routes rows, the partition constraint is the old one
INSERT INTO tpv.ev (id, ts, dev, val) VALUES (5001, '2026-03-01 12:00 UTC', 1, 1), (5002, '2026-03-03 12:00 UTC', 1, 1);
SELECT tableoid::regclass, count(*) FROM tpv.ev WHERE id > 5000 GROUP BY 1 ORDER BY 1;
INSERT INTO tpv.ev (id, ts, dev, val) VALUES (5003, '2026-03-02 12:00 UTC', 1, 1);
SELECT count(*) AS inserted_into_columnar FROM tpv.ev_p20260302 WHERE id = 5003;
DELETE FROM tpv.ev WHERE id = 5003;
-- a statement that matches no row of a columnar partition succeeds
UPDATE tpv.ev SET note = 'x' WHERE id = 99999;
DELETE FROM tpv.ev WHERE id = 99999;
-- the same data, sorted by the partition column: skipping works on it
CALL kingres.convert_partition('tpv.ev', 'tpv.ev_p20260303', options => '{"chunk_row_count": 100}');
SELECT tpv.skipped('SELECT count(*) FROM tpv.ev_p20260303 WHERE ts >= ''2026-03-03 06:00 UTC'' AND ts < ''2026-03-03 07:00 UTC''') AS sorted_by_ts;
CALL kingres.convert_partition('tpv.ev', 'tpv.ev_p20260304', sort_by => '{dev,id}', options => '{"chunk_row_count": 100}');
SELECT tpv.skipped('SELECT count(*) FROM tpv.ev_p20260304 WHERE dev = 3') AS sorted_by_dev;
SELECT tpv.skipped('SELECT count(*) FROM tpv.ev_p20260303 WHERE dev = 3') AS not_sorted_by_dev;
SELECT * FROM tpv.methods('tpv.ev');

-- back to heap, a change that columnar refuses, and to columnar again
SELECT tpv.digest('tpv.ev_p20260302') AS columnar_digest \gset
CALL kingres.convert_partition('tpv.ev', 'tpv.ev_p20260302', 'heap');
SELECT * FROM tpv.methods('tpv.ev') WHERE part = 'ev_p20260302';
SELECT tpv.digest('tpv.ev_p20260302') = :'columnar_digest' AS back_same_rows;
SELECT tpv.acl('tpv.ev_p20260302'), obj_description('tpv.ev_p20260302'::regclass, 'pg_class') AS comment;
UPDATE tpv.ev SET note = 'changed' WHERE id = 6;
SELECT note FROM tpv.ev WHERE id = 6;
UPDATE tpv.ev SET note = 'refused' WHERE id = 8;
DELETE FROM tpv.ev WHERE id = 8;
CALL kingres.convert_partition('tpv.ev', 'tpv.ev_p20260302');
SELECT note FROM tpv.ev WHERE id = 6;

-- convert_old_partitions: those whose upper bound is at or before the time,
-- oldest first, the ones already columnar left alone
CALL kingres.convert_old_partitions('tpv.ev', '2026-03-02 UTC');
SELECT * FROM tpv.methods('tpv.ev');
CALL kingres.convert_old_partitions('tpv.ev', '2026-03-04 UTC', 'heap');
SELECT part, am FROM tpv.methods('tpv.ev');
CALL kingres.convert_old_partitions('tpv.ev', '2026-03-04 UTC', 'heap');
CALL kingres.convert_old_partitions('tpv.ev', '2026-03-06 UTC');
SELECT part, am FROM tpv.methods('tpv.ev');
SELECT tpv.digest('tpv.ev') AS now_digest \gset
SELECT :'now_digest' <> :'table_digest' AS the_table_has_the_new_rows;
SELECT (SELECT count(*) FROM tpv.ev) = 5002 AS all_rows_there;

------------------------------------------------------------------------
-- 2. Refusals, each with its message
------------------------------------------------------------------------
CREATE TABLE tpv.rf (id int, ts timestamptz NOT NULL, note text) PARTITION BY RANGE (ts);
CREATE TABLE tpv.rf_a PARTITION OF tpv.rf FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE tpv.rf_b PARTITION OF tpv.rf FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
CREATE TABLE tpv.rf_d PARTITION OF tpv.rf DEFAULT;
CREATE TABLE tpv.rf_sub PARTITION OF tpv.rf FOR VALUES FROM ('2026-03-01') TO ('2026-04-01') PARTITION BY RANGE (ts);
CREATE TABLE tpv.rf_sub1 PARTITION OF tpv.rf_sub FOR VALUES FROM ('2026-03-01') TO ('2026-03-15');
CREATE TABLE tpv.other (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpv.other_a PARTITION OF tpv.other FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE tpv.plain (id int);
INSERT INTO tpv.rf_a VALUES (1, '2026-01-02', 'one'), (2, '2026-01-03', 'two');

-- what is asked
CALL kingres.convert_partition('tpv.rf', 'tpv.other_a');
CALL kingres.convert_partition('tpv.rf', 'tpv.plain');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_sub');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_d');
CALL kingres.convert_partition('tpv.plain', 'tpv.rf_a');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', 'btree');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', 'nonexistent');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', 'heap');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', sort_by => '{nope}');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', sort_by => '{}');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', sort_by => ARRAY[NULL]);
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', options => '{"compression": "gzip"}');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', options => '{"compression_level": 99}');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', options => '{"compression_level": 2.5}');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', options => '{"stripe_row_count": "many"}');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', options => '{"level": 3}');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', options => '[1]');
CALL kingres.convert_partition(NULL, 'tpv.rf_a');
CALL kingres.convert_partition('tpv.rf', NULL);
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', NULL);
CALL kingres.convert_old_partitions('tpv.rf', NULL);
CALL kingres.convert_old_partitions('tpv.rf', 'infinity');
CALL kingres.convert_old_partitions('tpv.rf', '2026-03-01 UTC', 'btree');
CALL kingres.convert_old_partitions('tpv.plain', '2026-03-01 UTC');
-- options belong to columnar tables: a heap target with options
CALL kingres.convert_partition('tpv.ev', 'tpv.ev_p20260301', 'columnar');
CALL kingres.convert_partition('tpv.ev', 'tpv.ev_p20260301', 'heap', options => '{"compression": "none"}');
CALL kingres.convert_partition('tpv.ev', 'tpv.ev_p20260301', 'heap');

-- a transaction block, and a transaction isolation level that is not read committed
BEGIN;
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
ROLLBACK;
BEGIN;
CALL kingres.convert_old_partitions('tpv.rf', '2026-03-01 UTC');
ROLLBACK;
SET default_transaction_isolation = 'repeatable read';
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
SET default_transaction_isolation = 'serializable';
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
RESET default_transaction_isolation;

-- the partition has something the copy would not carry (each, then removed)
CREATE TRIGGER own_trg BEFORE INSERT ON tpv.rf_a FOR EACH ROW EXECUTE FUNCTION suppress_redundant_updates_trigger();
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP TRIGGER own_trg ON tpv.rf_a;
CREATE TRIGGER own_stmt BEFORE INSERT ON tpv.rf_a FOR EACH STATEMENT EXECUTE FUNCTION suppress_redundant_updates_trigger();
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP TRIGGER own_stmt ON tpv.rf_a;
CREATE RULE own_rule AS ON INSERT TO tpv.rf_a DO ALSO NOTHING;
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP RULE own_rule ON tpv.rf_a;
ALTER TABLE tpv.rf_a ENABLE ROW LEVEL SECURITY;
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
ALTER TABLE tpv.rf_a DISABLE ROW LEVEL SECURITY;
CREATE POLICY own_pol ON tpv.rf_a USING (true);
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP POLICY own_pol ON tpv.rf_a;
SET client_min_messages = error;
CREATE PUBLICATION pub_a FOR TABLE tpv.rf_a;
RESET client_min_messages;
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP PUBLICATION pub_a;
CREATE VIEW tpv.rf_a_view AS SELECT * FROM tpv.rf_a;
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP VIEW tpv.rf_a_view;
CREATE MATERIALIZED VIEW tpv.rf_a_mv AS SELECT * FROM tpv.rf_a;
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP MATERIALIZED VIEW tpv.rf_a_mv;
CREATE TABLE tpv.refs (id int PRIMARY KEY);
ALTER TABLE tpv.rf_a ADD CONSTRAINT rf_a_uq UNIQUE (id);
CREATE TABLE tpv.child (rid int REFERENCES tpv.rf_a (id));
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', 'columnar');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', 'heap');
DROP TABLE tpv.child;
ALTER TABLE tpv.rf_a DROP CONSTRAINT rf_a_uq;
CREATE SEQUENCE tpv.rf_a_seq OWNED BY tpv.rf_a.id;
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP SEQUENCE tpv.rf_a_seq;
CREATE STATISTICS tpv.rf_a_stats ON id, note FROM tpv.rf_a;
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP STATISTICS tpv.rf_a_stats;
CREATE TABLE tpv.idn (g int GENERATED ALWAYS AS IDENTITY, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpv.idn_a PARTITION OF tpv.idn FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CALL kingres.convert_partition('tpv.idn', 'tpv.idn_a');
CREATE TABLE tpv.gen (ts timestamptz NOT NULL, g int GENERATED ALWAYS AS (1) STORED) PARTITION BY RANGE (ts);
CREATE TABLE tpv.gen_a PARTITION OF tpv.gen FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CALL kingres.convert_partition('tpv.gen', 'tpv.gen_a');
SELECT part, am FROM tpv.methods('tpv.gen');
-- and the check on its own, for what no ordinary statement can make
BEGIN;
UPDATE pg_class SET relpersistence = 'u' WHERE oid = 'tpv.rf_a'::regclass;
SELECT kingres.timepart__convert_refusal('tpv.rf'::regclass::oid, 'tpv.rf_a'::regclass::oid, 'columnar', NULL, NULL);
ROLLBACK;
SELECT kingres.timepart__convert_refusal('tpv.rf'::regclass::oid, 'tpv.rf_a'::regclass::oid, 'columnar', NULL, NULL) IS NULL AS refusal_function_is_clean_again;
-- a partition owned by another role than the table
ALTER TABLE tpv.rf OWNER TO tpv_owner;
ALTER TABLE tpv.rf_b OWNER TO tpv_other;
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_b');
ALTER TABLE tpv.rf OWNER TO CURRENT_USER;
ALTER TABLE tpv.rf_b OWNER TO CURRENT_USER;
-- a ledger table or a distributed table (the catalogs of those extensions, here only their names)
CREATE TABLE kingres.dist_table (relid regclass);
INSERT INTO kingres.dist_table VALUES ('tpv.rf');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP TABLE kingres.dist_table;
CREATE TABLE kingres.ledger_table (relid regclass);
INSERT INTO kingres.ledger_table VALUES ('tpv.rf_a');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP TABLE kingres.ledger_table;
-- the refusals of columnar itself, found before the attach fails on them
CREATE INDEX rf_a_idx ON tpv.rf_a (id);
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP INDEX tpv.rf_a_idx;
CREATE INDEX rf_idx ON tpv.rf (id);
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP INDEX tpv.rf_idx;
ALTER TABLE tpv.rf_a ADD PRIMARY KEY (id);
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
ALTER TABLE tpv.rf_a DROP CONSTRAINT rf_a_pkey;
ALTER TABLE tpv.rf ADD CONSTRAINT rf_out_fk FOREIGN KEY (id) REFERENCES tpv.refs (id) NOT VALID;
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
ALTER TABLE tpv.rf DROP CONSTRAINT rf_out_fk;
CREATE FUNCTION tpv.noop() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END';
CREATE TRIGGER after_row AFTER INSERT ON tpv.rf FOR EACH ROW EXECUTE FUNCTION tpv.noop();
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a', 'heap');
DROP TRIGGER after_row ON tpv.rf;
-- nothing was changed by all of that
SELECT part, am FROM tpv.methods('tpv.rf');
SELECT * FROM tpv.rf_a ORDER BY id;
SELECT count(*) AS leftovers FROM pg_class WHERE relname LIKE '%\_\_kc';

-- a table of the name that the new partition would have
CREATE TABLE tpv.rf_a__kc (x int);
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
DROP TABLE tpv.rf_a__kc;
-- all fine now
CALL kingres.convert_partition('tpv.rf', 'tpv.rf_a');
SELECT part, am FROM tpv.methods('tpv.rf');
SELECT * FROM tpv.rf_a ORDER BY id;

------------------------------------------------------------------------
-- 3. The copy is checked: a copy that loses rows is not swapped in
------------------------------------------------------------------------
CREATE FUNCTION tpv.drop_some() RETURNS trigger LANGUAGE plpgsql
AS 'BEGIN IF NEW.id % 2 = 0 THEN RETURN NULL; END IF; RETURN NEW; END';
CREATE FUNCTION tpv.lossy() RETURNS event_trigger LANGUAGE plpgsql AS $$
DECLARE r record;
BEGIN
    FOR r IN SELECT objid::regclass::text AS n FROM pg_event_trigger_ddl_commands() WHERE command_tag = 'CREATE TABLE' LOOP
        IF r.n LIKE '%\_\_kc' THEN
            EXECUTE format('CREATE TRIGGER lossy BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION tpv.drop_some()', r.n);
        END IF;
    END LOOP;
END $$;
CREATE EVENT TRIGGER tpv_lossy ON ddl_command_end WHEN TAG IN ('CREATE TABLE') EXECUTE FUNCTION tpv.lossy();
SELECT tpv.digest('tpv.ev_p20260301') AS before_lossy \gset
CALL kingres.convert_partition('tpv.ev', 'tpv.ev_p20260301');
SELECT tpv.digest('tpv.ev_p20260301') = :'before_lossy' AS unchanged_after_failed_copy;
SELECT part, am FROM tpv.methods('tpv.ev') WHERE part = 'ev_p20260301';
SELECT count(*) AS leftovers FROM pg_class WHERE relname LIKE '%\_\_kc';
DROP EVENT TRIGGER tpv_lossy;
CALL kingres.convert_partition('tpv.ev', 'tpv.ev_p20260301');
SELECT tpv.digest('tpv.ev_p20260301') = :'before_lossy' AS converted_when_the_copy_is_whole;

------------------------------------------------------------------------
-- 4. Who converts: the owner, from its own session; a superuser for the
--    owner only when the session trusts the owners of its tables
------------------------------------------------------------------------
GRANT EXECUTE ON PROCEDURE
    kingres.convert_partition(regclass, regclass, name, text[], jsonb),
    kingres.convert_old_partitions(regclass, timestamptz, name, text[], jsonb)
  TO tpv_owner, tpv_other, tpv_member, tpv_noinherit;
CREATE TABLE tpv.ow (id int, ts timestamptz NOT NULL, note text) PARTITION BY RANGE (ts);
CREATE TABLE tpv.ow_a PARTITION OF tpv.ow FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE tpv.ow_b PARTITION OF tpv.ow FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
CREATE TABLE tpv.ow_c PARTITION OF tpv.ow FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');
INSERT INTO tpv.ow SELECT i, '2026-01-01 UTC'::timestamptz + (i % 80) * interval '1 day', 'n' || i FROM generate_series(1, 200) i;
ALTER TABLE tpv.ow OWNER TO tpv_owner;
ALTER TABLE tpv.ow_a OWNER TO tpv_owner;
ALTER TABLE tpv.ow_b OWNER TO tpv_owner;
ALTER TABLE tpv.ow_c OWNER TO tpv_owner;
SELECT tpv.digest('tpv.ow') AS ow_digest \gset

-- a role that does not own the table, and a member that does not inherit
SET SESSION AUTHORIZATION tpv_other;
CALL kingres.convert_partition('tpv.ow', 'tpv.ow_a');
CALL kingres.convert_old_partitions('tpv.ow', '2026-03-01 UTC');
RESET SESSION AUTHORIZATION;
SET SESSION AUTHORIZATION tpv_noinherit;
CALL kingres.convert_partition('tpv.ow', 'tpv.ow_a');
RESET SESSION AUTHORIZATION;
-- a member that inherits the owner's rights is not the owner: the owner's
-- statements would run in the member's own session, which only the owner or a
-- superuser may do
SET SESSION AUTHORIZATION tpv_member;
CALL kingres.convert_partition('tpv.ow', 'tpv.ow_a');
RESET SESSION AUTHORIZATION;
-- a role that was not given the right to call
CREATE ROLE tpv_nocall NOLOGIN;
GRANT USAGE ON SCHEMA tpv, kingres TO tpv_nocall;
SET SESSION AUTHORIZATION tpv_nocall;
CALL kingres.convert_partition('tpv.ow', 'tpv.ow_a');
RESET SESSION AUTHORIZATION;
DROP OWNED BY tpv_nocall;
DROP ROLE tpv_nocall;
-- a superuser, for the owner of a table, who is no superuser, and does not trust
SET SESSION AUTHORIZATION tpv_su;
SET kingres_timepart.trust_table_owners = off;
CALL kingres.convert_partition('tpv.ow', 'tpv.ow_a');
RESET kingres_timepart.trust_table_owners;
RESET SESSION AUTHORIZATION;
-- the owner, in its own session
SET SESSION AUTHORIZATION tpv_owner;
CALL kingres.convert_partition('tpv.ow', 'tpv.ow_a');
CALL kingres.convert_old_partitions('tpv.ow', '2026-03-01 UTC', sort_by => '{id}');
SELECT part, am FROM tpv.methods('tpv.ow');
SELECT relname, relowner::regrole AS owner FROM pg_class WHERE oid IN ('tpv.ow_a'::regclass, 'tpv.ow_b'::regclass) ORDER BY 1;
CALL kingres.convert_partition('tpv.ow', 'tpv.ow_a', 'heap');
RESET SESSION AUTHORIZATION;
-- a superuser, for the owner, who is trusted
SET kingres_timepart.trust_table_owners = on;
CALL kingres.convert_partition('tpv.ow', 'tpv.ow_c');
SELECT part, am FROM tpv.methods('tpv.ow');
SELECT relname, relowner::regrole AS owner FROM pg_class WHERE oid IN ('tpv.ow_a'::regclass, 'tpv.ow_c'::regclass) ORDER BY 1;
SELECT tpv.digest('tpv.ow') = :'ow_digest' AS owner_table_same_rows;

------------------------------------------------------------------------
-- 5. Names: quotes, case, dots, long names; and a hostile search path
------------------------------------------------------------------------
CREATE SCHEMA "Odd""Schema";
CREATE TABLE "Odd""Schema"."T;ab ""le" (ts timestamptz NOT NULL, "C'ol" int) PARTITION BY RANGE (ts);
CREATE TABLE "Odd""Schema"."P1; DROP TABLE tpv.plain; --" PARTITION OF "Odd""Schema"."T;ab ""le" FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
INSERT INTO "Odd""Schema"."T;ab ""le" VALUES ('2026-01-05', 1), ('2026-01-06', 2);
CALL kingres.convert_partition('"Odd""Schema"."T;ab ""le"', '"Odd""Schema"."P1; DROP TABLE tpv.plain; --"', sort_by => ARRAY['C''ol']);
SELECT c.relname, a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.relnamespace = '"Odd""Schema"'::regnamespace AND c.relkind = 'r';
SELECT * FROM "Odd""Schema"."T;ab ""le" ORDER BY 2;
SELECT to_regclass('tpv.plain') IS NOT NULL AS plain_is_there;
-- 63 byte names: the name of the new table is cut so that the whole fits
CREATE TABLE tpv.long_names (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpv.aaaaaaaaaabbbbbbbbbbccccccccccddddddddddeeeeeeeeeeffffffffff123 PARTITION OF tpv.long_names FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
INSERT INTO tpv.long_names VALUES ('2026-01-05');
SELECT length('aaaaaaaaaabbbbbbbbbbccccccccccddddddddddeeeeeeeeeeffffffffff123') AS name_length;
CALL kingres.convert_partition('tpv.long_names', 'tpv.aaaaaaaaaabbbbbbbbbbccccccccccddddddddddeeeeeeeeeeffffffffff123');
SELECT part, am FROM tpv.methods('tpv.long_names');
-- a search path with look-alikes of what the code calls
CREATE SCHEMA evil;
CREATE FUNCTION evil.count(anyelement) RETURNS bigint LANGUAGE sql AS $$ SELECT -1::bigint $$;
CREATE FUNCTION evil.quote_ident(text) RETURNS text LANGUAGE sql AS $$ SELECT 'evil'::text $$;
CREATE FUNCTION evil.quote_literal(text) RETURNS text LANGUAGE sql AS $$ SELECT 'evil'::text $$;
CREATE FUNCTION evil.set_config(text, text, boolean) RETURNS text LANGUAGE sql AS $$ SELECT 'evil'::text $$;
CREATE FUNCTION evil.pg_get_userbyid(oid) RETURNS name LANGUAGE sql AS $$ SELECT 'evil'::name $$;
CREATE FUNCTION evil.current_setting(text) RETURNS text LANGUAGE sql AS $$ SELECT 'evil'::text $$;
CREATE TABLE evil.pg_class (oid oid, relname name);
CREATE TABLE tpv.sp (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpv.sp_a PARTITION OF tpv.sp FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
INSERT INTO tpv.sp VALUES (1, '2026-01-05'), (2, '2026-01-06');
GRANT SELECT ON tpv.sp_a TO tpv_other;
SET search_path = evil, public;
CALL kingres.convert_partition('tpv.sp', 'tpv.sp_a');
CALL kingres.convert_partition('tpv.sp', 'tpv.sp_a', 'heap');
CALL kingres.convert_old_partitions('tpv.sp', '2026-03-01 UTC');
RESET search_path;
SELECT part, am FROM tpv.methods('tpv.sp');
SELECT * FROM tpv.sp ORDER BY id;
SELECT tpv.acl('tpv.sp_a');

------------------------------------------------------------------------
-- 6. The policy: columnar_after, columnar_sort, columnar_options, in a run
------------------------------------------------------------------------
CREATE TABLE tpv.cpol (id bigint, ts timestamptz NOT NULL, dev int, note text) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tpv.cpol', '1 day', create_ahead => '1 day', retention => '30 days',
                                   columnar_after => '3 days', columnar_sort => '{dev,ts}',
                                   columnar_options => '{"compression": "pglz", "chunk_row_count": 500}');
SELECT kingres.create_time_partitions('tpv.cpol', '1 day', '2026-03-09 UTC', '2026-03-01 UTC');
INSERT INTO tpv.cpol SELECT i, '2026-03-01 UTC'::timestamptz + (i % 9) * interval '1 day' + (i % 3600) * interval '1 second', i % 7, 'n' || i FROM generate_series(1, 4500) i;
SELECT tpv.digest('tpv.cpol') AS pol_digest \gset
-- the plan names what a run converts; nothing is converted by planning
SELECT seq, kind, partition_name, reason FROM kingres.timepart_plan('tpv.cpol', '2026-03-09 12:00 UTC') WHERE kind = 'convert';
SELECT count(*) AS columnar_before FROM tpv.methods('tpv.cpol') WHERE am = 'columnar';
\o /dev/null
CALL kingres.timepart_run('tpv.cpol', '2026-03-09 12:00 UTC');
\o
SELECT * FROM tp.last_run('tpv', 'cpol');
SELECT * FROM tpv.actions('cpol') WHERE kind = 'convert';
SELECT a.partition_name, a.bytes_before > 0 AS has_before, a.bytes_after > 0 AS has_after
  FROM kingres.timepart_run_action a
 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'cpol') AND a.kind = 'convert' ORDER BY a.seq;
SELECT part, am FROM tpv.methods('tpv.cpol');
SELECT * FROM kingres.columnar_options('tpv.cpol_p20260302');
SELECT tpv.digest('tpv.cpol') = :'pol_digest' AS pol_same_rows;
-- the new partitions were analyzed, the old ones were not
SELECT c.relname, c.reltuples > 0 AS analyzed FROM pg_class c WHERE c.relname IN ('cpol_p20260301', 'cpol_p20260306') ORDER BY 1;
-- a second run has nothing to convert
\o /dev/null
CALL kingres.timepart_run('tpv.cpol', '2026-03-09 12:00 UTC');
\o
SELECT * FROM tpv.actions('cpol') WHERE kind = 'convert';
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-09 12:00 UTC') WHERE table_name = 'cpol';
-- later, one more partition is old enough
\o /dev/null
CALL kingres.timepart_run('tpv.cpol', '2026-03-10 12:00 UTC');
\o
SELECT * FROM tpv.actions('cpol') WHERE kind IN ('convert', 'create');
SELECT part, am FROM tpv.methods('tpv.cpol') WHERE am = 'columnar';
-- a partition that cannot be converted is skipped and the run goes on; the check says so
CREATE INDEX cpol_dev_idx ON tpv.cpol_p20260307 (dev);
\o /dev/null
CALL kingres.timepart_run('tpv.cpol', '2026-03-11 12:00 UTC');
\o
SELECT * FROM tp.last_run('tpv', 'cpol');
SELECT * FROM tpv.actions('cpol') WHERE kind = 'convert';
SELECT problem, left(detail, 150) FROM kingres.timepart_check('1 hour', '2026-03-11 12:00 UTC') WHERE table_name = 'cpol';
DROP INDEX tpv.cpol_dev_idx;
\o /dev/null
CALL kingres.timepart_run('tpv.cpol', '2026-03-11 12:00 UTC');
\o
SELECT * FROM tpv.actions('cpol') WHERE kind = 'convert';
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-11 12:00 UTC') WHERE table_name = 'cpol';
-- a partition that is both old enough to convert and to drop is only dropped
\o /dev/null
CALL kingres.timepart_run('tpv.cpol', '2026-04-20 12:00 UTC');
\o
SELECT kind, part FROM tpv.actions('cpol') WHERE kind IN ('convert', 'drop') ORDER BY 1, 2 LIMIT 5;
SELECT count(*) AS partitions_left FROM tpv.methods('tpv.cpol');
-- the phases one by one
SELECT kingres.timepart_set_policy('tpv.cpol', '1 day', create_ahead => '1 day', columnar_after => '2 days');
SELECT kingres.timepart_begin_run('tpv.cpol', '2026-04-20 12:00 UTC', 'manual') AS run_id \gset
CALL kingres.timepart_run_phase(:run_id, 'create');
CALL kingres.timepart_run_phase(:run_id, 'convert');
SELECT kingres.timepart_run_action(:run_id, 'convert') AS nothing_left;
SELECT kingres.timepart_finish_run(:run_id);
SELECT state FROM kingres.timepart_run WHERE run_id = :run_id;
SELECT count(*) FILTER (WHERE am = 'columnar') AS columnar, count(*) FILTER (WHERE am = 'heap') AS heap FROM tpv.methods('tpv.cpol');
-- invalid options of a policy are refused when it is set
SELECT kingres.timepart_set_policy('tpv.cpol', '1 day', columnar_after => '2 days', columnar_options => '{"compresion": "zstd"}');
SELECT kingres.timepart_set_policy('tpv.cpol', '1 day', columnar_after => '2 days', columnar_options => '{"compression_level": 0}');
SELECT kingres.timepart_set_policy('tpv.cpol', '1 day', columnar_after => '2 days', columnar_options => '{"compression": 5}');
SELECT kingres.timepart_set_policy('tpv.cpol', '1 day', columnar_after => '2 days', columnar_options => '{"stripe_row_count": 10}');
-- a policy with a sort column that is not there: the conversion is refused, the run goes on
SELECT kingres.timepart_set_policy('tpv.cpol', '1 day', create_ahead => '1 day', columnar_after => '1 day', columnar_sort => '{nope}');
\o /dev/null
CALL kingres.timepart_run('tpv.cpol', '2026-04-21 12:00 UTC');
\o
SELECT * FROM tp.last_run('tpv', 'cpol');
SELECT kind, state, detail FROM tpv.actions('cpol') WHERE kind = 'convert' LIMIT 1;
SELECT kingres.timepart_unset_policy('tpv.cpol');

------------------------------------------------------------------------
-- 7. A policy on a table of another role, run by a superuser
------------------------------------------------------------------------
CREATE TABLE tpv.cpow (ts timestamptz NOT NULL, v int) PARTITION BY RANGE (ts);
ALTER TABLE tpv.cpow OWNER TO tpv_owner;
SET SESSION AUTHORIZATION tpv_owner;
SELECT kingres.timepart_set_policy('tpv.cpow', '1 day', create_ahead => '1 day', columnar_after => '2 days');
SELECT kingres.create_time_partitions('tpv.cpow', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
INSERT INTO tpv.cpow SELECT '2026-03-01 UTC'::timestamptz + (i % 5) * interval '1 day', i FROM generate_series(1, 100) i;
RESET SESSION AUTHORIZATION;
SET SESSION AUTHORIZATION tpv_su;
SET kingres_timepart.trust_table_owners = off;
\o /dev/null
CALL kingres.timepart_run('tpv.cpow', '2026-03-05 12:00 UTC');
\o
SELECT * FROM tp.last_run('tpv', 'cpow');
SELECT kind, state, left(detail, 60) FROM tpv.actions('cpow') WHERE kind = 'convert' LIMIT 2;
RESET kingres_timepart.trust_table_owners;
RESET SESSION AUTHORIZATION;
SET kingres_timepart.trust_table_owners = on;
\o /dev/null
CALL kingres.timepart_run('tpv.cpow', '2026-03-05 12:00 UTC');
\o
SELECT * FROM tp.last_run('tpv', 'cpow');
SELECT * FROM tpv.actions('cpow') WHERE kind = 'convert';
SELECT part, am FROM tpv.methods('tpv.cpow');
SELECT count(*) AS pow_rows FROM tpv.cpow;

-- The owner drives its own run, from its own session (timepart_grant_driver),
-- without the acknowledgement: the conversion runs as the owner, and the
-- history and the analyze after it are written for the owner.
CREATE TABLE tpv.cpod (ts timestamptz NOT NULL, v int) PARTITION BY RANGE (ts);
ALTER TABLE tpv.cpod OWNER TO tpv_owner;
SELECT kingres.timepart_grant_driver('tpv_owner');
GRANT EXECUTE ON FUNCTION kingres.timepart_unset_policy(regclass) TO tpv_owner;
SET SESSION AUTHORIZATION tpv_owner;
SET kingres_timepart.trust_table_owners = off;
SELECT kingres.timepart_set_policy('tpv.cpod', '1 day', create_ahead => '1 day', columnar_after => '2 days',
                                   columnar_options => '{"compression": "none"}');
SELECT kingres.create_time_partitions('tpv.cpod', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
INSERT INTO tpv.cpod SELECT '2026-03-01 UTC'::timestamptz + (i % 5) * interval '1 day', i FROM generate_series(1, 100) i;
\o /dev/null
CALL kingres.timepart_run('tpv.cpod', '2026-03-05 12:00 UTC');
\o
SELECT state, trigger, error FROM kingres.timepart_run WHERE table_name = 'cpod' ORDER BY run_id DESC LIMIT 1;
SELECT * FROM tpv.actions('cpod') WHERE kind = 'convert';
SELECT a.partition_name, a.bytes_before > 0 AS has_before, a.bytes_after > 0 AS has_after
  FROM kingres.timepart_run_action a
 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'cpod') AND a.kind = 'convert' ORDER BY a.seq;
SELECT part, am FROM tpv.methods('tpv.cpod');
SELECT c.relname, c.reltuples > 0 AS analyzed FROM pg_class c WHERE c.relname IN ('cpod_p20260301', 'cpod_p20260303') ORDER BY 1;
SELECT count(*) AS cpod_rows FROM tpv.cpod;
SELECT kingres.timepart_unset_policy('tpv.cpod');
RESET kingres_timepart.trust_table_owners;
RESET SESSION AUTHORIZATION;
SELECT kingres.timepart_revoke_driver('tpv_owner');

------------------------------------------------------------------------
-- Clean up
------------------------------------------------------------------------
SET client_min_messages = warning;
DROP SCHEMA tpv CASCADE;
DROP SCHEMA evil CASCADE;
DROP SCHEMA "Odd""Schema" CASCADE;
DROP EXTENSION kingres_columnar;
DROP OWNED BY tpv_owner, tpv_other, tpv_member, tpv_noinherit, tpv_su;
DROP ROLE tpv_owner, tpv_other, tpv_member, tpv_noinherit, tpv_su;
RESET client_min_messages;
SELECT count(*) AS columnar_objects FROM pg_proc WHERE pronamespace = 'kingres'::regnamespace AND proname LIKE 'columnar%';
SELECT count(*) AS policies_left FROM kingres.timepart_policy WHERE table_name IN ('cpol', 'cpow', 'cpod');
