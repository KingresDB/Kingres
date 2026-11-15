-- SPDX-License-Identifier: PostgreSQL
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- reference.sql: pg_regress test of kingres_shard 0.2 (purpose below).
-- Reference tables on a single node: creation, catalog rows, DML, refusals.
-- (The multi-node behavior is tested by t/011_reference.pl.)
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

CREATE TABLE r1 (id int PRIMARY KEY, name text, n numeric DEFAULT 0);
INSERT INTO r1 VALUES (1, 'a', 1.5), (2, 'b', NULL);
SELECT kingres.create_reference_table('r1');
SELECT relid::regclass AS tbl, method, dist_column, colocation_id FROM kingres.dist_table;
SELECT count(*) AS colocation_groups FROM kingres.colocation;

-- Reads and writes work as on an ordinary table.
INSERT INTO r1 (id, name) VALUES (3, 'c') RETURNING *;
UPDATE r1 SET name = upper(name) WHERE id < 3 RETURNING id, name;
DELETE FROM r1 WHERE id = 3 RETURNING id;
SELECT * FROM r1 ORDER BY id;
TRUNCATE r1;
SELECT count(*) FROM r1;

-- A reference table has no buckets.
SELECT kingres.bucket_for('r1'::regclass, 1);
SELECT count(*) AS buckets_of_reference_tables FROM kingres.buckets b WHERE b.table_name = 'r1'::regclass;

-- Refusals.
SELECT kingres.create_reference_table('r1');
CREATE TABLE p (a int) PARTITION BY HASH (a);
SELECT kingres.create_reference_table('p');
CREATE TEMP TABLE tmp1 (a int);
SELECT kingres.create_reference_table('tmp1');
CREATE UNLOGGED TABLE ul (a int);
SELECT kingres.create_reference_table('ul');
CREATE TABLE has_fk (id int PRIMARY KEY, a int REFERENCES r1 (id));
SELECT kingres.create_reference_table('has_fk');
CREATE TABLE has_trg (id int PRIMARY KEY);
CREATE FUNCTION trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER t BEFORE INSERT ON has_trg FOR EACH ROW EXECUTE FUNCTION trg();
SELECT kingres.create_reference_table('has_trg');
CREATE VIEW v1 AS SELECT * FROM r1;
SELECT kingres.create_reference_table('v1');
SELECT kingres.create_reference_table(NULL);
SELECT kingres.create_distributed_table('r1', 'id');

-- A reference table next to a distributed table.
CREATE TABLE d1 (id int PRIMARY KEY, v int);
SELECT kingres.create_distributed_table('d1', 'id', bucket_count => 4);
SELECT kingres.create_distributed_table('d1', 'id', colocate_with => 'r1');
INSERT INTO d1 SELECT g, g FROM generate_series(1, 20) g;
INSERT INTO r1 SELECT g, 'r' || g FROM generate_series(1, 20) g;
SELECT count(*), sum(d1.v) FROM d1 JOIN r1 ON r1.id = d1.id;

-- The catalog is synchronized with the name of the table, not its OID.
SELECT jsonb_path_query_array(kingres.internal_catalog_snapshot(), '$.dist_table[*] ? (@.method == "reference")') AS ref_snapshot;

-- Dropping the table removes it from the catalog.
DROP TABLE r1 CASCADE;
SELECT relid::regclass FROM kingres.dist_table ORDER BY 1;

DROP TABLE has_fk, has_trg, d1, p, ul;
DROP FUNCTION trg();
DROP VIEW IF EXISTS v1;
DROP EXTENSION kingres_shard;
