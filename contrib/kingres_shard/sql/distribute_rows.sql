-- sql/distribute_rows.sql
--   Phase 9, increments 9.1 and 9.2 (design/shard-phase9.md): the report of what
--   prevents a change of a table (one statement per code), and
--   create_distributed_table / create_reference_table of a table that holds rows
--   on a one-node cluster (the copy, the counts, owner, privileges, comments,
--   identity columns and sequences, foreign keys to a reference table, unique
--   and other indexes).  The tests with several nodes, faults and interleavings
--   are t/140.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;

-- Before the cluster is initialized there is no map group to run on.
CREATE TABLE early (id int PRIMARY KEY);
SELECT severity, code, detail FROM kingres.table_change_report('distribute', 'early', '{"column":"id"}');
SELECT kingres.init_cluster('localhost', current_setting('port')::int);
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'early', '{"column":"id"}');
SELECT * FROM kingres.table_change_report('no_such_kind', 'early');
DROP TABLE early;

-- One statement per code of a distribution ---------------------------------------------
-- a table that cannot be distributed at all
CREATE VIEW a_view AS SELECT 1 AS id;
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'a_view', '{"column":"id"}');
CREATE TEMP TABLE temp_t (id int);
SELECT severity, code FROM kingres.table_change_report('distribute', 'temp_t', '{"column":"id"}');
CREATE UNLOGGED TABLE unlogged_t (id int);
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'unlogged_t', '{"column":"id"}');
CREATE TABLE part_t (id int) PARTITION BY RANGE (id);
CREATE TABLE part_t_1 PARTITION OF part_t FOR VALUES FROM (0) TO (10);
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'part_t', '{"column":"id"}');
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'part_t_1', '{"column":"id"}');
CREATE TABLE inh_p (id int);
CREATE TABLE inh_c () INHERITS (inh_p);
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'inh_p', '{"column":"id"}');
CREATE TABLE stor_t (id int) WITH (fillfactor = 70);
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'stor_t', '{"column":"id"}');
CREATE TABLE rls_t (id int);
ALTER TABLE rls_t ENABLE ROW LEVEL SECURITY;
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'rls_t', '{"column":"id"}');
CREATE TABLE excl_t (id int, EXCLUDE USING btree (id WITH =));
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'excl_t', '{"column":"id"}');
CREATE TABLE trg_t (id int);
CREATE FUNCTION trg_fn() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END';
CREATE TRIGGER trg_a BEFORE INSERT ON trg_t FOR EACH ROW EXECUTE FUNCTION trg_fn();
CREATE TRIGGER trg_b AFTER DELETE ON trg_t FOR EACH ROW EXECUTE FUNCTION trg_fn();
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'trg_t', '{"column":"id"}');
-- a shared catalog lives in a tablespace of its own
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'pg_database', '{"column":"oid"}');
-- what depends on the table
CREATE TABLE dep_t (id int PRIMARY KEY, v int);
CREATE VIEW dep_v AS SELECT id FROM dep_t;
CREATE MATERIALIZED VIEW dep_mv AS SELECT v FROM dep_t;
CREATE FUNCTION dep_f(r dep_t) RETURNS int LANGUAGE sql AS 'SELECT $1.v';
SET client_min_messages = error;
CREATE PUBLICATION dep_pub FOR TABLE dep_t;
RESET client_min_messages;
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'dep_t', '{"column":"id"}');
-- columns and keys
CREATE TABLE col_t (id int, p point, long_name_of_a_table_that_is_near_the_limit_of_sixty_three int);
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'col_t', '{"column":"nope"}');
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'col_t', '{"column":"p"}');
CREATE TABLE a_very_long_table_name_that_is_at_the_limit_of_sixty_three_chars (id int);
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'a_very_long_table_name_that_is_at_the_limit_of_sixty_three_chars', '{"column":"id"}');
CREATE TABLE key_t (id int PRIMARY KEY, k int, UNIQUE (k));
CREATE UNIQUE INDEX key_t_idx ON key_t (id) WHERE k > 0;
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'key_t', '{"column":"k"}');
CREATE TABLE used_t (id int);
CREATE TABLE used_t_b0003 (x int);
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'used_t', '{"column":"id"}');
-- an online copy needs a key
CREATE TABLE nokey_t (id int, v int);
CREATE TABLE nullkey_t (id int, v int, UNIQUE (id));
CREATE TABLE okkey_t (id int NOT NULL, v int, UNIQUE (id));
SELECT code, object FROM kingres.table_change_report('distribute', 'nokey_t', '{"column":"id","mode":"online"}');
SELECT code, object FROM kingres.table_change_report('distribute', 'nullkey_t', '{"column":"id","mode":"online"}');
SELECT code, object FROM kingres.table_change_report('distribute', 'okkey_t', '{"column":"id","mode":"online"}');
-- what is not carried, and what is good to know
CREATE TABLE nc_t (id int PRIMARY KEY, v text);
ALTER TABLE nc_t ALTER COLUMN v SET STORAGE EXTERNAL;
ALTER TABLE nc_t ALTER COLUMN id SET STATISTICS 50;
CREATE STATISTICS nc_t_stat ON id, v FROM nc_t;
CREATE INDEX nc_t_idx ON nc_t (v);
CLUSTER nc_t USING nc_t_idx;
ALTER TABLE nc_t REPLICA IDENTITY FULL;
INSERT INTO nc_t SELECT g, 'v' || g FROM generate_series(1, 100) g;
ANALYZE nc_t;
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'nc_t', '{"column":"id"}');

-- foreign keys ------------------------------------------------------------------------------
CREATE TABLE plain_parent (id int PRIMARY KEY);
CREATE TABLE fk_plain (id int PRIMARY KEY, p int REFERENCES plain_parent (id));
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'fk_plain', '{"column":"id"}');
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'plain_parent', '{"column":"id"}');
SELECT severity, code, object FROM kingres.table_change_report('distribute_reference', 'fk_plain');
CREATE TABLE ref_parent (id int PRIMARY KEY);
SELECT kingres.create_reference_table('ref_parent');
CREATE TABLE fk_ref (id int PRIMARY KEY, p int REFERENCES ref_parent (id));
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'fk_ref', '{"column":"id"}');
SELECT severity, code, object FROM kingres.table_change_report('distribute_reference', 'fk_ref');
CREATE TABLE dist_parent (id int PRIMARY KEY, g int);
SELECT kingres.create_distributed_table('dist_parent', 'id', bucket_count => 8);
CREATE TABLE fk_dist (id int PRIMARY KEY, p int REFERENCES dist_parent (id));
-- another group (the default has 128 buckets) or the wrong column
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'fk_dist', '{"column":"id"}');
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'fk_dist', '{"column":"id","bucket_count":8}');
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'fk_dist', '{"column":"id","colocate_with":"public.dist_parent"}');
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'fk_dist', '{"column":"p","colocate_with":"public.dist_parent"}');
SELECT severity, code, object FROM kingres.table_change_report('distribute_reference', 'fk_dist');
CREATE TABLE fk_self (id int PRIMARY KEY, parent int REFERENCES fk_self (id));
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'fk_self', '{"column":"id"}');
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'fk_self', '{"column":"parent"}');
-- a distributed table, and a reference table, are not distributed again
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'dist_parent', '{"column":"id"}');
SELECT severity, code, object FROM kingres.table_change_report('distribute_reference', 'ref_parent');

-- a group in use ---------------------------------------------------------------------------
INSERT INTO kingres.table_operation (kind, mode, phase, spec, started_at, updated_at)
  VALUES ('distribute', 'online', 'copying', '{}', now(), now());
INSERT INTO kingres.table_operation_lock (colocation_id, op_id)
  SELECT d.colocation_id, 1 FROM kingres.dist_table d WHERE d.relid = 'dist_parent'::regclass;
CREATE TABLE busy_t (id int PRIMARY KEY);
SELECT severity, code FROM kingres.table_change_report('distribute', 'busy_t', '{"column":"id","colocate_with":"public.dist_parent"}');
SELECT kingres.create_distributed_table('busy_t', 'id', colocate_with => 'dist_parent');
SELECT code FROM kingres.table_change_report('alter', 'dist_parent', '{"bucket_count":16}');
SELECT code FROM kingres.table_change_report('undistribute', 'dist_parent');
DELETE FROM kingres.table_operation_lock;
DELETE FROM kingres.table_operation;
DROP TABLE busy_t;

-- the other kinds ------------------------------------------------------------------------
SELECT severity, code, object FROM kingres.table_change_report('undistribute', 'plain_parent');
SELECT severity, code, object FROM kingres.table_change_report('undistribute', 'ref_parent');
SELECT severity, code, object FROM kingres.table_change_report('undistribute', 'dist_parent', '{"target_group":9}');
SELECT severity, code, object FROM kingres.table_change_report('alter', 'plain_parent', '{"bucket_count":16}');
SELECT severity, code, object FROM kingres.table_change_report('alter', 'dist_parent', '{"bucket_count":5000,"distribution_column":"nope"}');
SELECT severity, code, object FROM kingres.table_change_report('alter', 'dist_parent', '{"distribution_column":"g"}');
SELECT severity, code, object FROM kingres.table_change_report('alter', 'dist_parent', '{"colocate_with":"public.plain_parent"}');
SELECT severity, code, object FROM kingres.table_change_report('create_partition', 'dist_parent', '{"partition":"p1"}');
SELECT severity, code, object FROM kingres.table_change_report('drop_partition', 'plain_parent', '{"partition":"a_partition_name_that_is_much_too_long_to_get_six_more_characters_"}');

-- the report and the checks of the create functions agree -------------------------------
SELECT kingres.create_distributed_table('trg_t', 'id');
SELECT kingres.create_distributed_table('key_t', 'k');
SELECT kingres.create_distributed_table('dep_t', 'id');
SELECT kingres.create_distributed_table('col_t', 'p');
SELECT kingres.create_reference_table('fk_plain');
SELECT kingres.create_reference_table('fk_dist');
SELECT kingres.create_distributed_table('stor_t', 'id');
SELECT count(*) AS distributed_after_refusals FROM kingres.dist_table;

-- A table that holds rows -------------------------------------------------------------------
CREATE ROLE p9_owner;
CREATE ROLE p9_reader;
CREATE ROLE p9_writer;
CREATE ROLE p9_grantor;
CREATE TABLE items (
    tenant int NOT NULL,
    id     int NOT NULL,
    name   text DEFAULT 'unnamed',
    qty    int CONSTRAINT items_qty_check CHECK (qty >= 0),
    ser    serial,
    ident  bigint GENERATED ALWAYS AS IDENTITY (START WITH 100 INCREMENT BY 5),
    calc   int GENERATED ALWAYS AS (qty * 2) STORED,
    ref    int REFERENCES ref_parent (id),
    PRIMARY KEY (tenant, id)
);
CREATE INDEX items_name_idx ON items (name);
CREATE UNIQUE INDEX items_part_idx ON items (id, tenant) WHERE qty > 10;
INSERT INTO ref_parent SELECT g FROM generate_series(1, 5) g;
INSERT INTO items (tenant, id, name, qty, ref)
  SELECT g % 7, g, CASE WHEN g % 10 = 0 THEN NULL ELSE 'n' || g END, g % 50, 1 + g % 5
    FROM generate_series(1, 2000) g;
ALTER TABLE items OWNER TO p9_owner;
GRANT SELECT ON items TO p9_reader;
GRANT UPDATE (name), SELECT (qty) ON items TO p9_writer;
GRANT INSERT, UPDATE ON items TO p9_grantor WITH GRANT OPTION;
SET ROLE p9_grantor;
GRANT INSERT ON items TO p9_writer;
RESET ROLE;
COMMENT ON TABLE items IS 'the items';
COMMENT ON COLUMN items.name IS 'the name';
COMMENT ON CONSTRAINT items_qty_check ON items IS 'qty is not negative';
COMMENT ON CONSTRAINT items_pkey ON items IS 'the key';
COMMENT ON INDEX items_name_idx IS 'by name';
SELECT setval('items_ser_seq', 5000);

-- What the source looks like, object by object: columns, constraints, indexes, owner,
-- privileges, comments, sequences.  The same text is taken again after the copy.
CREATE FUNCTION fingerprint(t regclass) RETURNS SETOF text LANGUAGE sql AS $$
  SELECT 'column ' || a.attnum || ' ' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
         || ' notnull=' || a.attnotnull || ' identity=' || a.attidentity::text || ' generated=' || a.attgenerated::text
         || ' default=' || coalesce(pg_get_expr(d.adbin, d.adrelid), '-')
         || ' acl=' || coalesce(a.attacl::text, '-') || ' comment=' || coalesce(col_description(t, a.attnum), '-')
    FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
   WHERE a.attrelid = t AND a.attnum > 0 AND NOT a.attisdropped
  UNION ALL
  SELECT 'constraint ' || c.conname || ' ' || pg_get_constraintdef(c.oid) || ' comment=' || coalesce(obj_description(c.oid, 'pg_constraint'), '-')
    FROM pg_constraint c WHERE c.conrelid = t AND c.conparentid = 0
  UNION ALL
  SELECT 'index ' || ic.relname || ' ' || replace(pg_get_indexdef(i.indexrelid), ' ONLY ', ' ') || ' comment=' || coalesce(obj_description(i.indexrelid, 'pg_class'), '-')
    FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid WHERE i.indrelid = t
  UNION ALL
  SELECT 'table owner=' || c.relowner::regrole || ' acl=' || coalesce(c.relacl::text, '-') || ' comment=' || coalesce(obj_description(t, 'pg_class'), '-')
    FROM pg_class c WHERE c.oid = t
  UNION ALL
  SELECT 'sequence ' || s.seqrelid::regclass || ' type=' || s.seqtypid::regtype || ' start=' || s.seqstart || ' inc=' || s.seqincrement
         || ' min=' || s.seqmin || ' max=' || s.seqmax || ' cache=' || s.seqcache || ' cycle=' || s.seqcycle
         || ' owner=' || coalesce((SELECT d.refobjid::regclass || '.' || d.refobjsubid || '/' || d.deptype::text FROM pg_depend d
                                    WHERE d.classid = 'pg_class'::regclass AND d.objid = s.seqrelid
                                      AND d.refclassid = 'pg_class'::regclass LIMIT 1), '-')
    FROM pg_sequence s
   WHERE s.seqrelid IN (SELECT d.objid FROM pg_depend d WHERE d.classid = 'pg_class'::regclass
                         AND d.refclassid = 'pg_class'::regclass AND d.refobjid = t AND d.deptype IN ('a', 'i'))
$$;
CREATE TABLE fp_before AS SELECT line FROM fingerprint('items') line;
SELECT count(*) AS fingerprint_lines FROM fp_before;
CREATE TABLE items_model AS SELECT * FROM items;
SELECT count(nextval('items_ident_seq')) AS identity_values_used_without_rows FROM generate_series(1, 3);
SELECT last_value, is_called FROM items_ser_seq \gset ser_
SELECT last_value, is_called FROM items_ident_seq \gset ident_
SELECT oid AS old_oid FROM pg_class WHERE oid = 'items'::regclass \gset
SELECT severity, code, object FROM kingres.table_change_report('distribute', 'items', '{"column":"id"}');

SELECT kingres.create_distributed_table('items', 'id', bucket_count => 8);

SELECT count(*) AS rows, sum(qty) AS qty, count(DISTINCT id) AS ids FROM items;
SELECT (SELECT count(*) FROM (SELECT * FROM items EXCEPT SELECT * FROM items_model) a) AS only_after,
       (SELECT count(*) FROM (SELECT * FROM items_model EXCEPT SELECT * FROM items) b) AS only_before;
SELECT 'items'::regclass::oid <> :old_oid AS new_table;
SELECT relkind, pg_get_partkeydef(oid) FROM pg_class WHERE oid = 'items'::regclass;
-- every row is in the partition of its own bucket, and nowhere else
SELECT count(*) FILTER (WHERE tableoid::regclass::text = format('items_b%s', lpad(kingres.bucket_for('items'::regclass, id)::text, 4, '0'))) AS in_own_bucket,
       count(*) AS total
  FROM items;
-- the same object by object
CREATE TABLE fp_after AS SELECT line FROM fingerprint('items') line;
SELECT 'only before' AS side, line FROM (SELECT line FROM fp_before EXCEPT SELECT line FROM fp_after) x
UNION ALL
SELECT 'only after', line FROM (SELECT line FROM fp_after EXCEPT SELECT line FROM fp_before) y
ORDER BY 1, 2;
-- the sequences go on where they were
SELECT last_value = :ser_last_value AND is_called = :'ser_is_called' AS serial_state_carried FROM items_ser_seq;
SELECT last_value = :ident_last_value AND is_called = :'ident_is_called' AS identity_state_carried FROM items_ident_seq;
INSERT INTO items (tenant, id, qty, ref) VALUES (1, 9001, 3, 2) RETURNING ser, ident, calc;
-- the keys are enforced on the new table
INSERT INTO items (tenant, id, qty, ref) VALUES (1, 9001, 3, 2);
INSERT INTO items (tenant, id, qty, ref) VALUES (1, 9002, -1, 2);
INSERT INTO items (tenant, id, qty, ref) VALUES (1, 9003, 1, 99);
DELETE FROM ref_parent WHERE id = 1;
-- nothing of the old table or the shadow is left
SELECT relname FROM pg_class WHERE relname LIKE 'kingres\_shadow%' OR relname LIKE 'kingres\_old%';
SELECT count(*) AS dist_tables FROM kingres.dist_table;
SELECT version > 0 AS map_version_moved FROM kingres.map_version;

-- A prepared statement made on the ordinary table is planned again after the swap (it finds the new
-- table by name), in the session that distributes the table and in another one.
CREATE TABLE prep_t (id int PRIMARY KEY, v int);
INSERT INTO prep_t SELECT g, g * 2 FROM generate_series(1, 100) g;
PREPARE prep_q AS SELECT count(*), sum(v) FROM prep_t;
EXECUTE prep_q;
SELECT kingres.create_distributed_table('prep_t', 'id', bucket_count => 4);
EXECUTE prep_q;
EXPLAIN (COSTS OFF) EXECUTE prep_q;
DEALLOCATE prep_q;
DROP TABLE prep_t;

-- a table with rows joins the group of another one; its key to that table is carried
CREATE TABLE orders (id int, parent int REFERENCES dist_parent (id), note text, PRIMARY KEY (id, parent));
INSERT INTO dist_parent SELECT g, g % 3 FROM generate_series(1, 50) g;
INSERT INTO orders SELECT g, g, 'o' || g FROM generate_series(1, 50) g;
SELECT kingres.create_distributed_table('orders', 'parent', colocate_with => 'dist_parent');
SELECT count(*) AS orders FROM orders;
SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'orders'::regclass AND contype = 'f' AND conparentid = 0;
INSERT INTO orders VALUES (51, 99, 'bad');
SELECT tableoid::regclass::text = format('orders_b%s', lpad(kingres.bucket_for('orders'::regclass, parent)::text, 4, '0')) AS in_bucket_of_parent, count(*) FROM orders GROUP BY 1;
-- a key from the table to itself must use the distribution column on both sides: a tree does not
CREATE TABLE tree (id int PRIMARY KEY, parent int REFERENCES tree (id));
INSERT INTO tree SELECT g, nullif(g - 1, 0) FROM generate_series(1, 30) g;
SELECT kingres.create_distributed_table('tree', 'id', bucket_count => 4);
-- the key on the distribution column itself is carried
CREATE TABLE self_key (id int PRIMARY KEY, v int, FOREIGN KEY (id) REFERENCES self_key (id));
INSERT INTO self_key SELECT g, g FROM generate_series(1, 30) g;
SELECT kingres.create_distributed_table('self_key', 'id', bucket_count => 4);
SELECT count(*) AS self_key FROM self_key;
SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'self_key'::regclass AND contype = 'f' AND conparentid = 0;

-- A reference table with a key to another reference table, and the privileges of both
CREATE TABLE ref_child (id int PRIMARY KEY, p int REFERENCES ref_parent (id), v text);
INSERT INTO ref_child SELECT g, 2 + g % 3, 'c' || g FROM generate_series(1, 20) g;
ALTER TABLE ref_child OWNER TO p9_owner;
GRANT SELECT ON ref_child TO p9_reader;
COMMENT ON TABLE ref_child IS 'a reference table with a key';
SELECT kingres.create_reference_table('ref_child');
SELECT count(*) AS ref_child FROM ref_child;
SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'ref_child'::regclass AND contype = 'f';
SELECT relacl, obj_description(oid) FROM pg_class WHERE oid = 'ref_child'::regclass;
INSERT INTO ref_child VALUES (100, 77, 'bad');

-- the keys of a table that cannot be distributed are still refused, with the list
CREATE TABLE multi_bad (id int PRIMARY KEY, k int, p int REFERENCES plain_parent (id));
CREATE TRIGGER trg_m BEFORE INSERT ON multi_bad FOR EACH ROW EXECUTE FUNCTION trg_fn();
SELECT kingres.create_distributed_table('multi_bad', 'k');

-- cleanup
DROP PUBLICATION dep_pub;
DROP MATERIALIZED VIEW dep_mv;
DROP VIEW dep_v, a_view;
DROP FUNCTION dep_f(dep_t);
DROP TABLE multi_bad, ref_child, tree, self_key, orders, items, items_model, fp_before, fp_after,
           dep_t, col_t, key_t, used_t, used_t_b0003, nokey_t, nullkey_t, okkey_t, nc_t,
           fk_plain, plain_parent, fk_ref, fk_dist, fk_self, dist_parent, ref_parent, temp_t, unlogged_t,
           part_t, inh_c, inh_p, stor_t, rls_t, excl_t, trg_t, a_very_long_table_name_that_is_at_the_limit_of_sixty_three_chars;
DROP FUNCTION trg_fn();
DROP FUNCTION fingerprint(regclass);
DROP OWNED BY p9_owner;
DROP ROLE p9_owner, p9_reader, p9_writer, p9_grantor;
DROP EXTENSION kingres_shard;
