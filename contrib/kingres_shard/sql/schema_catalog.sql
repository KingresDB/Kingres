-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- schema_catalog.sql: phase 11 (distributed schemas), increment 1 (design/shard-phase11.md, 5.2 to 5.4, 10 and 15.1).
-- The catalog objects and their checks, the delta functions, kingres.schemas and kingres.buckets on hand-built rows,
-- schema_inventory for every kind of object, and the transitions of set_sharding_mode.  Nothing here creates a schema
-- group through the product (that is a later increment): the rows are made by the node-local delta functions.
\set SHOW_CONTEXT never
SET search_path = public, pg_temp;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);

-- ---------------------------------------------------------------------------
-- The shape of the catalog
-- ---------------------------------------------------------------------------
SELECT attname, format_type(atttypid, atttypmod) AS type, attnotnull
  FROM pg_attribute WHERE attrelid = 'kingres.colocation'::regclass AND attnum > 0 AND NOT attisdropped ORDER BY attnum;
SELECT attname, format_type(atttypid, atttypmod) AS type, attnotnull, pg_get_expr(d.adbin, d.adrelid) AS default
  FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
 WHERE a.attrelid = 'kingres.dist_schema'::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;
SELECT * FROM kingres.sharding_settings;
SELECT conrelid::regclass AS tbl, conname, pg_get_constraintdef(oid) AS def FROM pg_constraint
 WHERE conrelid IN ('kingres.colocation'::regclass, 'kingres.dist_table'::regclass, 'kingres.dist_schema'::regclass, 'kingres.sharding_settings'::regclass)
   AND contype = 'c' ORDER BY 1::text, 2;

-- The checks of kind and method.
INSERT INTO kingres.colocation (colocation_id, bucket_count, dist_column_type, kind) VALUES (50, 2, NULL, 'schema');
INSERT INTO kingres.colocation (colocation_id, bucket_count, dist_column_type, kind) VALUES (50, 1, 'int4', 'schema');
INSERT INTO kingres.colocation (colocation_id, bucket_count, dist_column_type, kind) VALUES (50, 4, NULL, 'hash');
INSERT INTO kingres.colocation (colocation_id, bucket_count, dist_column_type, kind) VALUES (50, 4, 'int4', 'bogus');
INSERT INTO kingres.dist_table VALUES ('pg_class'::regclass, 'schema', 'oid', 1);
INSERT INTO kingres.dist_table VALUES ('pg_class'::regclass, 'schema', NULL, NULL);
INSERT INTO kingres.sharding_settings VALUES ('row', false);
UPDATE kingres.sharding_settings SET mode = 'bogus';
UPDATE kingres.sharding_settings SET mode = 'row', distribute_new_schemas = true;

-- ---------------------------------------------------------------------------
-- The delta functions on a schema made here
-- ---------------------------------------------------------------------------
CREATE SCHEMA tenant_a;
CREATE TABLE tenant_a.t1 (id int PRIMARY KEY, v text);
CREATE TABLE tenant_a.t2 (id int, ref int) PARTITION BY RANGE (id);
CREATE TABLE tenant_a.t2_p1 PARTITION OF tenant_a.t2 FOR VALUES FROM (0) TO (100);
CREATE VIEW tenant_a.v1 AS SELECT id FROM tenant_a.t1;
SELECT version AS v0 FROM kingres.map_version \gset

-- a schema that does not exist, a bad group, a table of a schema that is not distributed
SELECT kingres.internal_register_schema('no_such_schema', 7, 0);
SELECT kingres.internal_register_schema('tenant_a', 7, -1);
SELECT kingres.internal_register_schema_table('tenant_a.t1');
SELECT kingres.internal_set_schema_group(1, 0, 'active');

SELECT kingres.internal_register_schema('tenant_a', 7, 0);
SELECT kingres.internal_register_schema('tenant_a', 8, 0);
SELECT kingres.internal_register_schema_table('tenant_a.v1');
SELECT kingres.internal_register_schema_table('tenant_a.t1');
SELECT kingres.internal_register_schema_table('tenant_a.t2');
SELECT kingres.internal_register_schema_table('tenant_a.t2_p1');
SELECT version > :v0 AS version_counted FROM kingres.map_version;

SELECT colocation_id, bucket_count, dist_column_type, kind FROM kingres.colocation ORDER BY 1;
SELECT colocation_id, bucket, group_id, state FROM kingres.bucket_map ORDER BY 1, 2;
SELECT nsp, colocation_id, ship_functions FROM kingres.dist_schema;
SELECT relid, method, dist_column, colocation_id FROM kingres.dist_table ORDER BY relid::text;

-- the views
SELECT schema_name, owner = current_user AS owner_is_me, colocation_id, group_id, node_id, state, table_count, moving_to_group_id,
       size_bytes IS NOT NULL AS size_known
  FROM kingres.schemas;
SELECT table_name, bucket, partition_name, group_id, node_id, state, partition_size IS NULL AS no_partition_size
  FROM kingres.buckets ORDER BY table_name::text;

-- a running move shows its target
INSERT INTO kingres.bucket_move (colocation_id, bucket, source_group, target_group, phase, started_at, updated_at)
  VALUES (7, 0, 0, 1, 'copying', now(), now());
SELECT schema_name, moving_to_group_id FROM kingres.schemas;
DELETE FROM kingres.bucket_move;

-- the group and state of the bucket
SELECT kingres.internal_set_schema_group(7, 0, 'moving');
SELECT group_id, state FROM kingres.bucket_map WHERE colocation_id = 7;
SELECT state FROM kingres.schemas;
SELECT kingres.internal_set_schema_group(7, 0, 'active');

-- the snapshot carries the schema, by name, and applies it again
SELECT jsonb_pretty((kingres.internal_catalog_snapshot() -> 'dist_schema' -> 0) - 'created_at') AS snapshot_dist_schema;
BEGIN;
SELECT kingres.internal_apply_catalog(kingres.internal_catalog_snapshot());
SELECT s.schema_name, s.colocation_id, s.table_count FROM kingres.schemas s;
SELECT count(*) AS dist_tables FROM kingres.dist_table;
ROLLBACK;
-- a snapshot that names a schema this node does not have
BEGIN;
SELECT kingres.internal_apply_catalog(jsonb_set(kingres.internal_catalog_snapshot(), '{dist_schema,0,schema}', '"nowhere"'));
ROLLBACK;

-- a role that is not a superuser
CREATE ROLE ks_p11_low;
GRANT USAGE ON SCHEMA tenant_a TO ks_p11_low;
SET ROLE ks_p11_low;
SELECT kingres.internal_register_schema('tenant_a', 9, 0);
SELECT kingres.internal_unregister_schema('tenant_a');
SELECT kingres.set_sharding_mode('both');
SELECT schema_name, group_id FROM kingres.schemas;
SELECT count(*) FROM kingres.schema_inventory('tenant_a');
RESET ROLE;
-- the functions check the caller themselves too, not only through the privilege
GRANT EXECUTE ON FUNCTION kingres.internal_register_schema(name, int, int), kingres.internal_register_schema_table(regclass),
    kingres.internal_unregister_schema_table(regclass), kingres.internal_unregister_schema(name),
    kingres.internal_set_schema_group(int, int, text), kingres.set_sharding_mode(text, bool) TO ks_p11_low;
SET ROLE ks_p11_low;
SELECT kingres.internal_register_schema('tenant_a', 9, 0);
SELECT kingres.internal_register_schema_table('tenant_a.t1');
SELECT kingres.internal_unregister_schema_table('tenant_a.t1');
SELECT kingres.internal_unregister_schema('tenant_a');
SELECT kingres.internal_set_schema_group(7, 0, 'moving');
SELECT kingres.set_sharding_mode('both');
RESET ROLE;
DROP OWNED BY ks_p11_low;
DROP ROLE ks_p11_low;

-- the unregister of a table and of the schema
SELECT kingres.internal_unregister_schema_table('tenant_a.t2_p1');
SELECT relid FROM kingres.dist_table ORDER BY relid::text;
SELECT kingres.internal_unregister_schema('no_such_schema');
SELECT kingres.internal_unregister_schema('tenant_a');
SELECT count(*) AS left_over FROM kingres.dist_schema;
SELECT count(*) AS left_over FROM kingres.dist_table;
SELECT count(*) AS left_over FROM kingres.bucket_map;
SELECT count(*) AS left_over FROM kingres.colocation;

-- the drop of a registered schema takes its rows with it
SELECT kingres.internal_register_schema('tenant_a', 7, 0);
SELECT kingres.internal_register_schema_table('tenant_a.t1');
DROP SCHEMA tenant_a CASCADE;
SELECT count(*) AS dist_schemas FROM kingres.dist_schema;
SELECT count(*) AS dist_tables FROM kingres.dist_table;
SELECT count(*) AS buckets FROM kingres.bucket_map;
SELECT count(*) AS colocations FROM kingres.colocation;
-- a schema without tables
CREATE SCHEMA tenant_empty;
SELECT kingres.internal_register_schema('tenant_empty', 7, 0);
DROP SCHEMA tenant_empty;
SELECT count(*) AS dist_schemas FROM kingres.dist_schema;
SELECT count(*) AS colocations FROM kingres.colocation;

-- ---------------------------------------------------------------------------
-- schema_inventory: every kind of object
-- ---------------------------------------------------------------------------
CREATE SCHEMA inv_other;
CREATE TABLE inv_other.plain (id int PRIMARY KEY);
CREATE TABLE inv_other.ref (id int PRIMARY KEY);
INSERT INTO kingres.dist_table VALUES ('inv_other.ref'::regclass, 'reference', NULL, NULL);
CREATE TABLE inv_other.hashed (id int PRIMARY KEY);
INSERT INTO kingres.colocation (colocation_id, bucket_count, dist_column_type) VALUES (60, 4, 'int4');
INSERT INTO kingres.dist_table VALUES ('inv_other.hashed'::regclass, 'hash', 'id', 60);
CREATE TABLE inv_other.parent (id int);

CREATE SCHEMA inv;
CREATE TABLE inv.good (id int PRIMARY KEY, v text, CONSTRAINT good_v CHECK (v <> ''));
CREATE INDEX good_v_idx ON inv.good (v);
CREATE SEQUENCE inv.seq;
CREATE UNLOGGED TABLE inv.unl (id int);
CREATE TABLE inv.part (id int, v int) PARTITION BY RANGE (id);
CREATE TABLE inv.part_1 PARTITION OF inv.part FOR VALUES FROM (0) TO (10);
CREATE TABLE inv.child () INHERITS (inv_other.parent);
CREATE TABLE inv.fk_ok (id int REFERENCES inv.good (id), r int REFERENCES inv_other.ref (id));
CREATE TABLE inv.fk_bad (id int REFERENCES inv_other.plain (id));
CREATE TABLE inv_other.fk_in (id int REFERENCES inv.good (id));
CREATE TABLE inv.rls (id int);
ALTER TABLE inv.rls ENABLE ROW LEVEL SECURITY;
CREATE POLICY pol ON inv.rls USING (true);
CREATE TABLE inv.ruled (id int);
CREATE RULE r1 AS ON INSERT TO inv.ruled DO ALSO NOTHING;
CREATE VIEW inv.v_inside AS SELECT id FROM inv.good;
CREATE VIEW inv.v_outside AS SELECT id FROM inv_other.plain;
CREATE VIEW inv.v_ref AS SELECT id FROM inv_other.ref;
CREATE VIEW inv.v_catalog AS SELECT oid FROM pg_class;
CREATE MATERIALIZED VIEW inv.mv AS SELECT 1 AS x;
CREATE FUNCTION inv.f(int) RETURNS int LANGUAGE sql AS 'SELECT $1';
CREATE PROCEDURE inv.p() LANGUAGE sql AS 'SELECT 1';
CREATE AGGREGATE inv.agg(int) (sfunc = int4pl, stype = int, initcond = '0');
CREATE TYPE inv.color AS ENUM ('red', 'green');
CREATE TYPE inv.pair AS (a int, b int);
CREATE DOMAIN inv.posint AS int CHECK (VALUE > 0);
CREATE TYPE inv.rng AS RANGE (subtype = int);
CREATE FUNCTION inv.trg() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END';
CREATE TRIGGER t_good BEFORE INSERT ON inv.good FOR EACH ROW EXECUTE FUNCTION inv.trg();
CREATE STATISTICS inv.st ON id, v FROM inv.good;
SET client_min_messages = error;
CREATE PUBLICATION inv_pub FOR TABLES IN SCHEMA inv;
RESET client_min_messages;
SELECT object_kind, object_name, supported, reason FROM kingres.schema_inventory('inv') ORDER BY 1, 2;
SELECT object_kind, object_name, supported, reason FROM kingres.schema_inventory('inv_other') ORDER BY 1, 2;

-- the other reasons: already distributed, a system schema
SELECT object_kind, object_name, supported, reason FROM kingres.schema_inventory('pg_catalog') WHERE object_kind = 'schema';
SELECT object_kind, object_name, supported, reason FROM kingres.schema_inventory('kingres') WHERE object_kind = 'schema';
SELECT object_kind, object_name, supported, reason FROM kingres.schema_inventory('pg_toast') WHERE object_kind = 'schema';
SELECT kingres.schema_inventory(9999999::oid::regnamespace);
DROP PUBLICATION inv_pub;

-- ---------------------------------------------------------------------------
-- set_sharding_mode: every cell of the table of design section 10
-- ---------------------------------------------------------------------------
DELETE FROM kingres.dist_table;
INSERT INTO kingres.colocation (colocation_id, bucket_count, dist_column_type) VALUES (60, 4, 'int4') ON CONFLICT DO NOTHING;
CREATE SCHEMA mode_s;
SELECT * FROM kingres.sharding_settings;
SELECT kingres.set_sharding_mode('bogus');
SELECT kingres.set_sharding_mode(NULL);
SELECT kingres.set_sharding_mode('row', true);
-- row to both, both to row (nothing distributed)
SELECT kingres.set_sharding_mode('both', true);
SELECT * FROM kingres.sharding_settings;
SELECT kingres.set_sharding_mode('both');
SELECT * FROM kingres.sharding_settings;
SELECT kingres.set_sharding_mode('both', false);
SELECT * FROM kingres.sharding_settings;
SELECT kingres.set_sharding_mode('row');
SELECT * FROM kingres.sharding_settings;
-- row to schema, schema to row, schema to both
SELECT kingres.set_sharding_mode('schema', true);
SELECT * FROM kingres.sharding_settings;
SELECT kingres.set_sharding_mode('row');
SELECT * FROM kingres.sharding_settings;
SELECT kingres.set_sharding_mode('schema');
SELECT kingres.set_sharding_mode('both');
SELECT * FROM kingres.sharding_settings;
-- with a distributed schema: both to row refused, both to schema allowed, schema to row refused
SELECT kingres.internal_register_schema('mode_s', 7, 0);
SELECT object_kind, object_name, supported, reason FROM kingres.schema_inventory('mode_s') WHERE object_kind = 'schema';
SELECT kingres.set_sharding_mode('row');
SELECT kingres.set_sharding_mode('schema');
SELECT kingres.set_sharding_mode('row');
SELECT * FROM kingres.sharding_settings;
-- with a hash table: both to schema and row to schema refused
SELECT kingres.set_sharding_mode('both');
CREATE TABLE mode_h (id int PRIMARY KEY);
INSERT INTO kingres.dist_table VALUES ('mode_h'::regclass, 'hash', 'id', 60);
SELECT kingres.set_sharding_mode('schema');
SELECT * FROM kingres.sharding_settings;
-- the sharding settings travel with the snapshot
SELECT kingres.internal_catalog_snapshot() -> 'sharding_settings' AS settings_in_snapshot;
BEGIN;
UPDATE kingres.sharding_settings SET mode = 'row', distribute_new_schemas = false;
SELECT kingres.internal_apply_catalog(kingres.internal_catalog_snapshot() || jsonb_build_object('sharding_settings', '[{"mode":"schema","distribute_new_schemas":true}]'::jsonb));
SELECT * FROM kingres.sharding_settings;
ROLLBACK;
DELETE FROM kingres.dist_table WHERE relid = 'mode_h'::regclass;
DROP TABLE mode_h;
DELETE FROM kingres.colocation WHERE colocation_id = 60;
SELECT kingres.internal_unregister_schema('mode_s');
SELECT kingres.set_sharding_mode('row');
SELECT * FROM kingres.sharding_settings;
-- not the map group primary
UPDATE kingres.local_node SET node_id = node_id + 100;
SELECT kingres.set_sharding_mode('both');
UPDATE kingres.local_node SET node_id = node_id - 100;
