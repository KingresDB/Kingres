-- SPDX-License-Identifier: PostgreSQL
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- ddl_single.sql: pg_regress test of kingres_shard 0.2 (purpose below).
-- DDL on distributed and reference tables on a single node (one shard group):
-- every supported statement just runs, renaming a distributed table renames its
-- bucket partitions, and the statements that would damage the distribution are
-- refused whatever the number of groups.  (Propagation to other nodes is tested
-- by t/012_ddl_propagation.pl.)
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

CREATE TABLE d (id int PRIMARY KEY, a text, b int);
SELECT kingres.create_distributed_table('d', 'id', bucket_count => 4);
CREATE TABLE r (id int PRIMARY KEY, name text);
SELECT kingres.create_reference_table('r');
INSERT INTO d SELECT g, 'a' || g, g FROM generate_series(1, 10) g;
INSERT INTO r VALUES (1, 'one');

-- Supported statements.
ALTER TABLE d ADD COLUMN c int DEFAULT 7;
ALTER TABLE d ALTER COLUMN b SET DEFAULT 5;
ALTER TABLE d ALTER COLUMN b SET NOT NULL;
ALTER TABLE d ALTER COLUMN b DROP NOT NULL;
ALTER TABLE d ALTER COLUMN b DROP DEFAULT;
ALTER TABLE d ADD CONSTRAINT d_b_pos CHECK (b >= 0);
ALTER TABLE d ADD CONSTRAINT d_id_a_key UNIQUE (id, a);
ALTER TABLE d ADD COLUMN rr int;
ALTER TABLE d ADD CONSTRAINT d_rr_fk FOREIGN KEY (rr) REFERENCES r (id);
ALTER TABLE d DROP CONSTRAINT d_b_pos;
ALTER TABLE d RENAME COLUMN a TO a2;
ALTER TABLE d DROP COLUMN c;
CREATE INDEX d_b_idx ON d (b);
DROP INDEX d_b_idx;
GRANT SELECT ON d TO PUBLIC;
REVOKE SELECT ON d FROM PUBLIC;
COMMENT ON TABLE d IS 'a distributed table';
SELECT a2, b FROM d WHERE id = 3;

-- Renaming the table renames the partitions.
ALTER TABLE d RENAME TO d_renamed;
SELECT relname FROM pg_class WHERE relname ~ '^d(_renamed)?(_b[0-9]{4})?$' AND relkind IN ('r', 'p') ORDER BY 1;
SELECT count(*) AS partitions_found FROM kingres.buckets WHERE table_name = 'd_renamed'::regclass AND partition_size IS NOT NULL;
SELECT count(*) FROM d_renamed;
ALTER TABLE d_renamed RENAME TO d;

-- Reference table DDL (the foreign key from d is dropped first: TRUNCATE and DROP need it).
ALTER TABLE d DROP CONSTRAINT d_rr_fk;
ALTER TABLE r ADD COLUMN extra text;
ALTER TABLE r RENAME TO r2;
TRUNCATE r2;
ALTER TABLE r2 RENAME TO r;

-- Refused whatever the number of groups: the distribution column and the
-- bucket partitions.
ALTER TABLE d DROP COLUMN id;
ALTER TABLE d RENAME COLUMN id TO id2;
ALTER TABLE d ALTER COLUMN id TYPE bigint;
ALTER TABLE d_b0000 RENAME TO oops;
DROP TABLE d_b0000;
CREATE TABLE d_b0009 PARTITION OF d FOR VALUES WITH (MODULUS 4, REMAINDER 0);
ALTER TABLE d DETACH PARTITION d_b0000;
CREATE INDEX d_b0000_idx ON d_b0000 (b);
-- A name too long for the partition names.
ALTER TABLE d RENAME TO this_name_is_far_too_long_for_the_partition_suffix_to_fit_in_63_bytes;
SELECT count(*) FROM d;

-- DROP TABLE removes the catalog row.
DROP TABLE r;
SELECT relid::regclass FROM kingres.dist_table ORDER BY 1;
DROP TABLE d CASCADE;
SELECT count(*) AS catalog_rows FROM kingres.dist_table;
DROP EXTENSION kingres_shard;
