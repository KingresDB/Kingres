-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- init_cluster and create_distributed_table on a single node.
-- (A role named like a schema would capture unqualified names through
-- "$user", so the tests pin the search path.)
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;

-- Nothing works before the cluster is initialized.
SELECT kingres.add_node('localhost', 5999);
SELECT kingres.sync_catalog();
CREATE TABLE early (id int PRIMARY KEY);
SELECT kingres.create_distributed_table('early', 'id');
DROP TABLE early;

SELECT kingres.init_cluster('localhost', current_setting('port')::int);
SELECT kingres.init_cluster('localhost', current_setting('port')::int);
SELECT node_id, group_id, role, is_active, should_have_shards, is_local FROM kingres.nodes;
SELECT version > 0 AS map_version_moved FROM kingres.map_version;

-- Rejected tables.  Each error names what is wrong.
CREATE TABLE bad_pk (id int PRIMARY KEY, k int);
SELECT kingres.create_distributed_table('bad_pk', 'k');
CREATE TABLE bad_unique (id int, k int, CONSTRAINT bad_unique_id_key UNIQUE (id));
SELECT kingres.create_distributed_table('bad_unique', 'k');
CREATE TABLE bad_index (id int, k int);
CREATE UNIQUE INDEX bad_index_id_idx ON bad_index (id);
SELECT kingres.create_distributed_table('bad_index', 'k');
CREATE TABLE no_column (id int);
SELECT kingres.create_distributed_table('no_column', 'nope');
CREATE TABLE unhashable (id int, p point);
SELECT kingres.create_distributed_table('unhashable', 'p');
CREATE TABLE fk_parent (id int PRIMARY KEY);
CREATE TABLE fk_child (id int, parent int REFERENCES fk_parent);
SELECT kingres.create_distributed_table('fk_child', 'id');
SELECT kingres.create_distributed_table('fk_parent', 'id');
CREATE TEMP TABLE temp_t (id int);
SELECT kingres.create_distributed_table('temp_t', 'id');
CREATE VIEW a_view AS SELECT 1 AS id;
SELECT kingres.create_distributed_table('a_view', 'id');
CREATE TABLE already_part (id int) PARTITION BY HASH (id);
SELECT kingres.create_distributed_table('already_part', 'id');
CREATE TABLE trg (id int);
CREATE FUNCTION trg_fn() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END';
CREATE TRIGGER trg_t BEFORE INSERT ON trg FOR EACH ROW EXECUTE FUNCTION trg_fn();
SELECT kingres.create_distributed_table('trg', 'id');
SELECT kingres.create_distributed_table('bad_pk', 'k', bucket_count => 0);
SELECT kingres.create_distributed_table('bad_pk', 'k', bucket_count => 5000);
SELECT count(*) AS distributed_after_errors FROM kingres.dist_table;
DROP VIEW a_view;
DROP TABLE bad_pk, bad_unique, bad_index, no_column, unhashable,
           fk_child, fk_parent, already_part, trg;
DROP FUNCTION trg_fn();

-- A table with a primary key that includes the key, a default, a check
-- constraint and a secondary index.
CREATE TABLE items (
    tenant int NOT NULL,
    id     int NOT NULL,
    name   text DEFAULT 'unnamed',
    qty    int CONSTRAINT items_qty_check CHECK (qty >= 0),
    PRIMARY KEY (tenant, id)
);
CREATE INDEX items_name_idx ON items (name);
SELECT kingres.create_distributed_table('items', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('items', 'id');

SELECT c.relkind, c.relname, pg_get_partkeydef(c.oid) AS partition_key
  FROM pg_class c WHERE c.oid = 'items'::regclass;
SELECT count(*) AS partitions FROM pg_inherits WHERE inhparent = 'items'::regclass;
SELECT pg_get_expr(c.relpartbound, c.oid) AS bound, c.relname
  FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
  WHERE i.inhparent = 'items'::regclass ORDER BY c.relname LIMIT 3;
SELECT conname, contype, pg_get_constraintdef(oid) FROM pg_constraint
  WHERE conrelid = 'items'::regclass ORDER BY conname;
SELECT indexrelid::regclass AS index FROM pg_index
  WHERE indrelid = 'items'::regclass ORDER BY 1::text;
SELECT column_name, data_type, column_default FROM information_schema.columns
  WHERE table_name = 'items' ORDER BY ordinal_position;

SELECT d.relid, d.method, d.dist_column, d.colocation_id, c.bucket_count, c.dist_column_type
  FROM kingres.dist_table d JOIN kingres.colocation c USING (colocation_id);
SELECT b.bucket, b.partition_name, b.group_id, b.node_id, b.state,
       b.partition_size IS NOT NULL AS exists_here
  FROM kingres.buckets b WHERE b.table_name = 'items'::regclass ORDER BY b.bucket;

-- Colocation: explicit, automatic (same type and count), and a new group.
CREATE TABLE orders (id int, item int, PRIMARY KEY (id));
SELECT kingres.create_distributed_table('orders', 'id', colocate_with => 'items');
CREATE TABLE lines (id int, note text);
SELECT kingres.create_distributed_table('lines', 'id', bucket_count => 8);
CREATE TABLE other_count (id int);
SELECT kingres.create_distributed_table('other_count', 'id', bucket_count => 16);
CREATE TABLE bad_count (id int);
SELECT kingres.create_distributed_table('bad_count', 'id', colocate_with => 'items', bucket_count => 4);
CREATE TABLE other_type (id bigint);
SELECT kingres.create_distributed_table('other_type', 'id', bucket_count => 8);
CREATE TABLE text_keyed (id text);
SELECT kingres.create_distributed_table('text_keyed', 'id');
SELECT kingres.create_distributed_table('bad_count', 'id', colocate_with => 'bad_count');
SELECT kingres.create_distributed_table('bad_count', 'id', colocate_with => 'text_keyed');
SELECT d.relid, d.colocation_id, c.bucket_count, c.dist_column_type
  FROM kingres.dist_table d JOIN kingres.colocation c USING (colocation_id)
  ORDER BY d.relid::text;
SELECT colocation_id, count(*) AS buckets, count(DISTINCT group_id) AS groups
  FROM kingres.bucket_map GROUP BY colocation_id ORDER BY 1;

-- A serial column keeps working: its sequence moves to the new table.
CREATE TABLE ser (id serial, k int, PRIMARY KEY (id, k));
SELECT kingres.create_distributed_table('ser', 'id', bucket_count => 4);
SELECT pg_get_serial_sequence('ser', 'id') AS sequence;
INSERT INTO ser (id, k) VALUES (nextval('ser_id_seq'), 1) RETURNING id, k;

-- The table can be used, dropped, and is forgotten by the catalog.
INSERT INTO items VALUES (1, 10, 'a', 1), (1, 11, 'b', 2);
SELECT tableoid::regclass::text ~ '^items_b[0-9]{4}$' AS stored_in_a_bucket,
       substring(tableoid::regclass::text from '_b([0-9]+)$')::int = kingres.bucket_for('items'::regclass, id) AS bucket_matches,
       id FROM items ORDER BY id;
DROP TABLE items, orders, lines, other_count, bad_count, other_type, text_keyed, ser;
SELECT count(*) AS dist_tables_left FROM kingres.dist_table;
DROP EXTENSION kingres_shard;
