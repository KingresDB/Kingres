-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_shard--0.6--0.7.sql
--   Upgrade of the kingres_shard extension from version 0.6 to 0.7: the table
--   lifecycle of design/shard-phase9.md (increments 9.1 and 9.2).  What changes
--   in the database:
--     * the catalog gains what the lifecycle needs: kingres.dist_table.partition_method
--       and partition_key, kingres.dist_partition (synchronized), the node-local
--       records of table operations (table_operation, table_operation_item,
--       table_operation_lock, table_old_copy), the sequence colocation_id_seq (a
--       colocation id is never reused) and the views table_operations and partitions;
--     * kingres.table_change_report() says what prevents a change of a table, and
--       what it would not carry, without changing anything;
--     * kingres.create_distributed_table() accepts a table that holds rows, and
--       carries its owner, privileges, comments, sequences and foreign keys;
--       kingres.create_reference_table() carries them to the other nodes and allows
--       foreign keys to other reference tables.  Both refuse with the list of the
--       report;
--     * the version handshake kingres.internal_require_version(), and the
--       helpers of the copy;
--     * a bucket move is refused for a colocation group that a table operation
--       claims (kingres.table_operation_lock);
--     * kingres.internal_check_node_versions() asks for 0.7.
--   The result is identical to installing kingres_shard--0.7.sql (the "upgrade"
--   regression test compares the two).  Change both together.

\echo Use "ALTER EXTENSION kingres_shard UPDATE TO '0.7'" to load this file. \quit

-- ---------------------------------------------------------------------------
-- Catalog
-- ---------------------------------------------------------------------------

-- How the user partitioned a distributed table (design, feature 4); NULL for a
-- table that is hash-partitioned only.  Nothing sets it in this version.
ALTER TABLE kingres.dist_table
    ADD COLUMN partition_method text,
    ADD COLUMN partition_key    text,
    ADD CONSTRAINT dist_table_partition_check CHECK (
        (partition_method IS NULL AND partition_key IS NULL)
        OR (method = 'hash' AND partition_method IN ('range', 'list') AND partition_key IS NOT NULL));

COMMENT ON COLUMN kingres.dist_table.partition_method IS
    'NULL, ''range'' or ''list'': how the user partitioned the table below the hash level';
COMMENT ON COLUMN kingres.dist_table.partition_key IS
    'The partition key of the user partitioning as pg_get_partkeydef gives it, without the method';

-- The user partitions of a distributed table.  A row stands for one leaf per
-- bucket (the leaf of bucket b is <partition_name>_b<bbbb> inside <parent>_b<bbbb>).
-- Synchronized with the rest of the catalog.
CREATE TABLE kingres.dist_partition (
    parent         regclass    NOT NULL,
    partition_name name        NOT NULL,
    bound          text        NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT pg_catalog.now(),
    CONSTRAINT dist_partition_pkey PRIMARY KEY (parent, partition_name),
    CONSTRAINT dist_partition_parent_fkey
        FOREIGN KEY (parent) REFERENCES kingres.dist_table (relid) ON DELETE CASCADE
);

CREATE TRIGGER dist_partition_map_version
    AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON kingres.dist_partition
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.map_version_bump();

COMMENT ON TABLE kingres.dist_partition IS
    'The user partitions of the distributed tables: one row per partition, one leaf per bucket';

-- A colocation id is never reused: choose_colocation takes the next value.  The
-- sequence starts above the highest id of the catalog it is created in.
CREATE SEQUENCE kingres.colocation_id_seq;

SELECT pg_catalog.setval('kingres.colocation_id_seq', m, true)
  FROM (SELECT pg_catalog.max(c.colocation_id) AS m FROM kingres.colocation c) x
 WHERE m IS NOT NULL;

COMMENT ON SEQUENCE kingres.colocation_id_seq IS
    'Colocation ids; never reused, so a placement that was seen can never come back under another table';

-- Node-local records of the operations that change a table (design, "Operation
-- record and phases"): never synchronized, readable by superusers only.  The
-- blocking variant of create_distributed_table keeps no record; the rebuild
-- engine of the online variants writes these.
CREATE TABLE kingres.table_operation (
    op_id            bigint GENERATED ALWAYS AS IDENTITY,
    kind             text NOT NULL,
    mode             text NOT NULL,
    phase            text NOT NULL,
    spec             jsonb NOT NULL,
    detail           text,
    colocation_id    int,
    attempts         int NOT NULL DEFAULT 0,
    cancel_requested bool NOT NULL DEFAULT false,
    cleanup_after    timestamptz,
    verified         text,
    started_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    finished_at      timestamptz,
    CONSTRAINT table_operation_pkey PRIMARY KEY (op_id),
    CONSTRAINT table_operation_kind_check
        CHECK (kind IN ('distribute', 'distribute_reference', 'undistribute', 'alter')),
    CONSTRAINT table_operation_mode_check CHECK (mode IN ('online')),
    CONSTRAINT table_operation_phase_check
        CHECK (phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching',
                         'cleaning', 'done', 'failed', 'cancelled')),
    CONSTRAINT table_operation_verified_check CHECK (verified IN ('ok', 'failed', 'skipped'))
);

CREATE TABLE kingres.table_operation_item (
    op_id       bigint   NOT NULL,
    ordinal     int      NOT NULL,
    relid       regclass NOT NULL,
    shadow_name name,
    old_name    name,
    rows_copied bigint   NOT NULL DEFAULT 0,
    rows_total  bigint,
    CONSTRAINT table_operation_item_pkey PRIMARY KEY (op_id, ordinal),
    CONSTRAINT table_operation_item_op_id_fkey
        FOREIGN KEY (op_id) REFERENCES kingres.table_operation (op_id)
);

-- One row per colocation group that an unfinished table operation uses.  A
-- bucket move (internal_begin_move) and a table operation exclude each other
-- through this table and kingres.bucket_move (decision D10).
CREATE TABLE kingres.table_operation_lock (
    colocation_id int    NOT NULL,
    op_id         bigint NOT NULL,
    CONSTRAINT table_operation_lock_pkey PRIMARY KEY (colocation_id),
    CONSTRAINT table_operation_lock_op_id_fkey
        FOREIGN KEY (op_id) REFERENCES kingres.table_operation (op_id)
);

-- The retained old copies of replaced tables and the time after which they may
-- be dropped.
CREATE TABLE kingres.table_old_copy (
    relid         regclass    NOT NULL,
    dropped_after timestamptz NOT NULL,
    CONSTRAINT table_old_copy_pkey PRIMARY KEY (relid)
);

COMMENT ON TABLE kingres.table_operation IS
    'Node-local, never synchronized: the operations that rebuild a table (the online variants)';
COMMENT ON TABLE kingres.table_operation_item IS
    'Node-local, never synchronized: one row per table of an operation';
COMMENT ON TABLE kingres.table_operation_lock IS
    'Node-local, never synchronized: the colocation groups that an unfinished table operation uses';
COMMENT ON TABLE kingres.table_old_copy IS
    'Node-local, never synchronized: replaced tables that are kept until their time has come';

REVOKE ALL ON kingres.table_operation, kingres.table_operation_item,
    kingres.table_operation_lock, kingres.table_old_copy FROM PUBLIC;
REVOKE ALL ON SEQUENCE kingres.colocation_id_seq FROM PUBLIC;

-- What the operations are doing, without the text of errors and without node
-- addresses (the record's detail is for superusers).
CREATE VIEW kingres.table_operations AS
    SELECT o.op_id, o.kind, o.mode, o.phase,
           (SELECT pg_catalog.array_agg(i.relid::pg_catalog.text ORDER BY i.ordinal)
              FROM kingres.table_operation_item i WHERE i.op_id = o.op_id) AS tables,
           (SELECT pg_catalog.sum(i.rows_copied)::pg_catalog.int8
              FROM kingres.table_operation_item i WHERE i.op_id = o.op_id) AS rows_copied,
           (SELECT pg_catalog.sum(i.rows_total)::pg_catalog.int8
              FROM kingres.table_operation_item i WHERE i.op_id = o.op_id) AS rows_total,
           NULL::pg_catalog.int8 AS backlog,
           o.attempts, o.verified, o.started_at, o.updated_at
      FROM kingres.table_operation o;

COMMENT ON VIEW kingres.table_operations IS
    'Table operations with their progress; no error text and no node address';

-- The user partitions with the size of their leaves on this node.
CREATE VIEW kingres.partitions AS
    SELECT p.parent AS table_name,
           p.partition_name,
           d.partition_method AS method,
           p.bound,
           (p.bound = 'DEFAULT') AS is_default,
           (SELECT pg_catalog.count(*)::pg_catalog.int4 FROM kingres.bucket_map b
             WHERE b.colocation_id = d.colocation_id) AS leaf_count,
           (SELECT pg_catalog.sum(pg_catalog.pg_total_relation_size(l.oid))::pg_catalog.int8
              FROM kingres.bucket_map b
              JOIN pg_catalog.pg_class l
                ON l.relnamespace = c.relnamespace
               AND l.relname = pg_catalog.format('%s_b%s', p.partition_name,
                                                 pg_catalog.lpad(b.bucket::pg_catalog.text, 4, '0'))
             WHERE b.colocation_id = d.colocation_id) AS size_here
      FROM kingres.dist_partition p
      JOIN kingres.dist_table d ON d.relid = p.parent
      JOIN pg_catalog.pg_class c ON c.oid = p.parent::pg_catalog.oid;

COMMENT ON VIEW kingres.partitions IS
    'The user partitions of distributed tables; size_here counts the leaves that exist on this node';

GRANT SELECT ON kingres.table_operations, kingres.partitions TO pg_monitor;
GRANT SELECT ON kingres.dist_partition TO PUBLIC;

-- ---------------------------------------------------------------------------
-- The catalog snapshot carries the new columns and the table
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION kingres.internal_catalog_snapshot()
    RETURNS pg_catalog.jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT jsonb_build_object(
    'version', (SELECT m.version FROM kingres.map_version m),
    'term', (SELECT m.term FROM kingres.map_version m),
    'node', COALESCE((SELECT jsonb_agg(to_jsonb(n) ORDER BY n.node_id)
                        FROM kingres.node n), '[]'::pg_catalog.jsonb),
    'colocation', COALESCE((SELECT jsonb_agg(to_jsonb(c) ORDER BY c.colocation_id)
                              FROM kingres.colocation c), '[]'::pg_catalog.jsonb),
    'dist_table', COALESCE((SELECT jsonb_agg(jsonb_build_object(
                                       'schema', ns.nspname,
                                       'name', cl.relname,
                                       'method', d.method,
                                       'dist_column', d.dist_column,
                                       'colocation_id', d.colocation_id,
                                       'partition_method', d.partition_method,
                                       'partition_key', d.partition_key)
                                     ORDER BY d.colocation_id, ns.nspname, cl.relname)
                              FROM kingres.dist_table d
                              JOIN pg_catalog.pg_class cl ON cl.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid
                              JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) cl.relnamespace),
                           '[]'::pg_catalog.jsonb),
    'dist_partition', COALESCE((SELECT jsonb_agg(jsonb_build_object(
                                       'schema', ns.nspname,
                                       'name', cl.relname,
                                       'partition_name', p.partition_name,
                                       'bound', p.bound,
                                       'created_at', p.created_at)
                                     ORDER BY ns.nspname, cl.relname, p.partition_name)
                              FROM kingres.dist_partition p
                              JOIN pg_catalog.pg_class cl ON cl.oid OPERATOR(pg_catalog.=) p.parent::pg_catalog.oid
                              JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) cl.relnamespace),
                           '[]'::pg_catalog.jsonb),
    'bucket_map', COALESCE((SELECT jsonb_agg(to_jsonb(b) ORDER BY b.colocation_id, b.bucket)
                              FROM kingres.bucket_map b), '[]'::pg_catalog.jsonb));
$$;

-- A document without dist_partition and without the two columns comes from a map
-- group primary that is not upgraded yet: the tables then have no user partitions.
-- The colocation ids of the document are remembered by the sequence, so that a
-- node that becomes the map group primary later never hands out an id that was used.
CREATE OR REPLACE FUNCTION kingres.internal_apply_catalog(snapshot pg_catalog.jsonb)
    RETURNS pg_catalog.void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r        pg_catalog.record;
    rid      pg_catalog.regclass;
    cur_term pg_catalog.int8;
    new_term pg_catalog.int8;
    v_max    pg_catalog.int4;
BEGIN
    -- A node that holds a newer catalog refuses an older one, whoever sends it
    -- (a map group primary that was replaced and restarted; S7-06).  The sender
    -- reads the term out of the detail.
    SELECT m.term INTO cur_term FROM kingres.map_version m;
    new_term := COALESCE((snapshot OPERATOR(pg_catalog.->>) 'term')::pg_catalog.int8, 0);
    IF new_term OPERATOR(pg_catalog.<) cur_term THEN
        RAISE EXCEPTION 'the catalog term % is older than the term % of this node', new_term, cur_term
            USING ERRCODE = 'KS002',
                  DETAIL = pg_catalog.format('kingres-term=%s', cur_term),
                  HINT = 'The sender holds a catalog that has been superseded by a promotion.';
    END IF;

    DELETE FROM kingres.bucket_map;
    DELETE FROM kingres.dist_partition;
    DELETE FROM kingres.dist_table;
    DELETE FROM kingres.colocation;
    DELETE FROM kingres.node;

    INSERT INTO kingres.node
        SELECT * FROM pg_catalog.jsonb_populate_recordset(NULL::kingres.node,
                                    snapshot OPERATOR(pg_catalog.->) 'node');
    INSERT INTO kingres.colocation
        SELECT * FROM pg_catalog.jsonb_populate_recordset(NULL::kingres.colocation,
                                    snapshot OPERATOR(pg_catalog.->) 'colocation');

    FOR r IN SELECT * FROM pg_catalog.jsonb_to_recordset(snapshot OPERATOR(pg_catalog.->) 'dist_table')
                  AS x(schema pg_catalog.text, name pg_catalog.text, method pg_catalog.text,
                       dist_column pg_catalog.name, colocation_id pg_catalog.int4,
                       partition_method pg_catalog.text, partition_key pg_catalog.text)
    LOOP
        rid := pg_catalog.to_regclass(pg_catalog.format('%I.%I', r.schema, r.name));
        IF rid IS NULL THEN
            RAISE EXCEPTION 'distributed table %.% does not exist on this node', r.schema, r.name
                USING ERRCODE = 'undefined_table';
        END IF;
        INSERT INTO kingres.dist_table (relid, method, dist_column, colocation_id,
                                        partition_method, partition_key)
        VALUES (rid, r.method, r.dist_column, r.colocation_id, r.partition_method, r.partition_key);
        -- Plans of statements on the table were made with the old catalog.
        PERFORM kingres.internal_invalidate_relation(rid);
    END LOOP;

    FOR r IN SELECT * FROM pg_catalog.jsonb_to_recordset(
                              COALESCE(snapshot OPERATOR(pg_catalog.->) 'dist_partition', '[]'::pg_catalog.jsonb))
                  AS x(schema pg_catalog.text, name pg_catalog.text, partition_name pg_catalog.name,
                       bound pg_catalog.text, created_at pg_catalog.timestamptz)
    LOOP
        rid := pg_catalog.to_regclass(pg_catalog.format('%I.%I', r.schema, r.name));
        IF rid IS NULL THEN
            RAISE EXCEPTION 'distributed table %.% does not exist on this node', r.schema, r.name
                USING ERRCODE = 'undefined_table';
        END IF;
        INSERT INTO kingres.dist_partition (parent, partition_name, bound, created_at)
        VALUES (rid, r.partition_name, r.bound, COALESCE(r.created_at, pg_catalog.now()));
    END LOOP;

    INSERT INTO kingres.bucket_map
        SELECT * FROM pg_catalog.jsonb_populate_recordset(NULL::kingres.bucket_map,
                                    snapshot OPERATOR(pg_catalog.->) 'bucket_map');

    SELECT pg_catalog.max(c.colocation_id) INTO v_max FROM kingres.colocation c;
    IF v_max IS NOT NULL
       AND v_max OPERATOR(pg_catalog.>) (SELECT s.last_value FROM kingres.colocation_id_seq s) THEN
        PERFORM pg_catalog.setval('kingres.colocation_id_seq', v_max, true);
    END IF;

    -- A node that was superseded and now follows the newer catalog is no longer
    -- superseded.
    UPDATE kingres.map_version
       SET version = (snapshot OPERATOR(pg_catalog.->>) 'version')::pg_catalog.int8,
           term = new_term,
           superseded_by_term = CASE WHEN new_term OPERATOR(pg_catalog.>=) superseded_by_term
                                     THEN 0 ELSE superseded_by_term END;
END;
$$;

-- ---------------------------------------------------------------------------
-- The version handshake, and the exclusion of bucket moves and table operations
-- ---------------------------------------------------------------------------

-- Every active primary must report extension version p_version; the first that
-- does not is named.  The functions of the table lifecycle call it before they
-- change anything (review finding S4-05).
CREATE FUNCTION kingres.internal_require_version(p_version text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    n pg_catalog.record;
    v pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR n IN SELECT nd.node_id, nd.host, nd.port FROM kingres.node nd
              WHERE nd.is_active AND nd.role OPERATOR(pg_catalog.=) 'primary' ORDER BY nd.node_id
    LOOP
        IF n.node_id OPERATOR(pg_catalog.=) (SELECT l.node_id FROM kingres.local_node l) THEN
            -- this node: no session needed (a node does not always listen on the address it is registered with)
            SELECT COALESCE((SELECT e.extversion FROM pg_catalog.pg_extension e
                              WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard'), '') INTO v;
        ELSE
        BEGIN
            v := kingres.internal_exec_scalar(n.node_id,
                'SELECT COALESCE((SELECT e.extversion FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) ''kingres_shard''), '''')',
                (SELECT s.setting::pg_catalog.int4 OPERATOR(pg_catalog.*) 2000 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout'));
        EXCEPTION WHEN OTHERS THEN
            -- The node may be reachable and refuse the claim of the session of this node (it
            -- runs another version of the library): ask without a claim, so that the error
            -- names both versions.
            v := kingres.internal_remote_extension_version(n.host, n.port);
            IF v IS NULL THEN
                RAISE EXCEPTION 'node % (%:%) cannot be asked for its kingres_shard version: %', n.node_id, n.host, n.port, SQLERRM
                    USING ERRCODE = 'object_not_in_prerequisite_state';
            END IF;
        END;
        END IF;
        IF v IS DISTINCT FROM p_version THEN
            RAISE EXCEPTION 'node % (%:%) has kingres_shard %, a table operation needs % on every node',
                n.node_id, n.host, n.port, COALESCE(NULLIF(v, ''), '(not installed)'), p_version
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Run ALTER EXTENSION kingres_shard UPDATE on that node, after the new library is loaded there.';
        END IF;
    END LOOP;
END;
$$;

-- internal_check_node_versions(): asks for 0.7.
CREATE OR REPLACE FUNCTION kingres.internal_check_node_versions()
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    n pg_catalog.record;
    v pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR n IN SELECT nd.node_id, nd.host, nd.port FROM kingres.node nd
              WHERE nd.is_active AND nd.role OPERATOR(pg_catalog.=) 'primary' ORDER BY nd.node_id
    LOOP
        BEGIN
            v := kingres.internal_exec_scalar(n.node_id,
                'SELECT COALESCE((SELECT e.extversion FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) ''kingres_shard''), '''')', 20000);
        EXCEPTION WHEN OTHERS THEN
            -- The node may be reachable and refuse the claim of the session of this node (it runs
            -- another version of the library): ask for the version without a claim, so that the
            -- error names both versions (review finding S6-06).
            v := kingres.internal_remote_extension_version(n.host, n.port);
            IF v IS NULL THEN
                RAISE EXCEPTION 'node % (%:%) cannot be asked for its kingres_shard version: %', n.node_id, n.host, n.port, SQLERRM
                    USING ERRCODE = 'object_not_in_prerequisite_state';
            END IF;
        END;
        IF v IS DISTINCT FROM '0.7' THEN
            RAISE EXCEPTION 'node % (%:%) has kingres_shard %, a bucket move needs 0.7 on every node',
                n.node_id, n.host, n.port, COALESCE(NULLIF(v, ''), '(not installed)')
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Run ALTER EXTENSION kingres_shard UPDATE on that node, after the new library is loaded there.';
        END IF;
    END LOOP;
END;
$$;

-- internal_begin_move(): a colocation group that a table operation claims is
-- refused (decision D10); the rest is as before.
CREATE OR REPLACE FUNCTION kingres.internal_begin_move(p_colocation int, p_bucket int, p_target int)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_source pg_catalog.int4;
    v_state  pg_catalog.text;
    v_count  pg_catalog.int4;
    v_id     pg_catalog.int8;
    v_op     pg_catalog.int8;
BEGIN
    PERFORM kingres.internal_require_superuser();
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    PERFORM kingres.internal_check_node_versions();

    SELECT c.bucket_count INTO v_count FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) p_colocation;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'colocation group % does not exist', p_colocation USING ERRCODE = 'undefined_object';
    END IF;
    SELECT l.op_id INTO v_op FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) p_colocation;
    IF FOUND THEN
        RAISE EXCEPTION 'colocation group % is used by table operation %', p_colocation, v_op
            USING ERRCODE = 'object_in_use',
                  DETAIL = 'A bucket cannot move while a table operation copies the tables of its group.',
                  HINT = 'Wait for the operation to finish, or cancel it; see kingres.table_operations.';
    END IF;
    SELECT m.group_id, m.state INTO v_source, v_state FROM kingres.bucket_map m
     WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation AND m.bucket OPERATOR(pg_catalog.=) p_bucket;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'bucket % of colocation group % does not exist (it has % buckets)', p_bucket, p_colocation, v_count
            USING ERRCODE = 'undefined_object';
    END IF;
    IF EXISTS (SELECT 1 FROM kingres.bucket_move m
                WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation AND m.bucket OPERATOR(pg_catalog.=) p_bucket
                  AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching')) THEN
        RAISE EXCEPTION 'bucket % of colocation group % is already being moved', p_bucket, p_colocation
            USING ERRCODE = 'object_in_use',
                  HINT = 'See kingres.bucket_move; a move can be cancelled with kingres.cancel_bucket_move().';
    END IF;
    IF v_state OPERATOR(pg_catalog.<>) 'active' THEN
        RAISE EXCEPTION 'bucket % of colocation group % is in state "%"', p_bucket, p_colocation, v_state
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF p_target OPERATOR(pg_catalog.=) v_source THEN
        RAISE EXCEPTION 'bucket % of colocation group % is already on shard group %', p_bucket, p_colocation, p_target
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF kingres.internal_group_node(p_target) IS NULL OR kingres.internal_group_node(v_source) IS NULL THEN
        RAISE EXCEPTION 'shard group % or % has no active primary', v_source, p_target
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM kingres.node n
                    WHERE n.group_id OPERATOR(pg_catalog.=) p_target AND n.role OPERATOR(pg_catalog.=) 'primary'
                      AND n.is_active AND n.should_have_shards) THEN
        RAISE EXCEPTION 'shard group % does not accept buckets (should_have_shards is false)', p_target
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'Use kingres.set_node_property() to change it.';
    END IF;

    INSERT INTO kingres.bucket_move (colocation_id, bucket, source_group, target_group, phase,
                                     detail, started_at, updated_at)
    VALUES (p_colocation, p_bucket, v_source, p_target, 'preparing', 'starting',
            pg_catalog.clock_timestamp(), pg_catalog.clock_timestamp())
    RETURNING move_id INTO v_id;
    UPDATE kingres.bucket_move m SET slot_name = pg_catalog.format('ks_move_%s', v_id) WHERE m.move_id OPERATOR(pg_catalog.=) v_id;
    UPDATE kingres.bucket_map m SET state = 'moving'
     WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation AND m.bucket OPERATOR(pg_catalog.=) p_bucket;
    PERFORM kingres.sync_catalog();
    RETURN v_id;
END;
$$;

-- ---------------------------------------------------------------------------
-- Helpers of the copy
-- ---------------------------------------------------------------------------

-- Whether a type has a default hash operator class with an extended hash
-- function (what a distribution column needs).
CREATE FUNCTION kingres.internal_has_hash_support(p_type regtype)
    RETURNS bool
    AS 'MODULE_PATHNAME', 'kingres_internal_has_hash_support'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

-- Give the sequence of a column the values that the copied rows need: at least the
-- largest (smallest, for a descending sequence) value of the column on this node,
-- and at least the state p_last/p_called of the sequence the table had before.  A
-- sequence is never moved backwards.
CREATE FUNCTION kingres.internal_advance_sequence(p_seq regclass, p_table regclass, p_column name,
                                                  p_last bigint DEFAULT NULL, p_called bool DEFAULT false)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_inc    pg_catalog.int8;
    v_min    pg_catalog.int8;
    v_max    pg_catalog.int8;
    v_last   pg_catalog.int8;
    v_called pg_catalog.bool;
    v_m      pg_catalog.int8;
    v_want   pg_catalog.int8;
BEGIN
    SELECT s.seqincrement, s.seqmin, s.seqmax INTO v_inc, v_min, v_max
      FROM pg_catalog.pg_sequence s WHERE s.seqrelid OPERATOR(pg_catalog.=) p_seq;
    EXECUTE pg_catalog.format('SELECT last_value, is_called FROM %s', p_seq) INTO v_last, v_called;
    IF v_inc OPERATOR(pg_catalog.>) 0 THEN
        EXECUTE pg_catalog.format('SELECT pg_catalog.max(%I)::pg_catalog.int8 FROM %s', p_column, p_table) INTO v_m;
    ELSE
        EXECUTE pg_catalog.format('SELECT pg_catalog.min(%I)::pg_catalog.int8 FROM %s', p_column, p_table) INTO v_m;
    END IF;
    IF p_last IS NOT NULL AND p_called THEN
        IF v_m IS NULL OR (v_inc OPERATOR(pg_catalog.>) 0 AND p_last OPERATOR(pg_catalog.>) v_m)
                       OR (v_inc OPERATOR(pg_catalog.<) 0 AND p_last OPERATOR(pg_catalog.<) v_m) THEN
            v_m := p_last;
        END IF;
    END IF;
    IF v_m IS NULL OR v_m OPERATOR(pg_catalog.<) v_min OR v_m OPERATOR(pg_catalog.>) v_max THEN
        RETURN;
    END IF;
    v_want := v_m;
    IF v_called AND ((v_inc OPERATOR(pg_catalog.>) 0 AND v_last OPERATOR(pg_catalog.>=) v_want)
                  OR (v_inc OPERATOR(pg_catalog.<) 0 AND v_last OPERATOR(pg_catalog.<=) v_want)) THEN
        RETURN;
    END IF;
    PERFORM pg_catalog.setval(p_seq, v_want, true);
END;
$$;

-- Give the sequence of an identity column the name it had on the table that is
-- replaced.
CREATE FUNCTION kingres.internal_rename_identity(p_table regclass, p_column name, p_new_name name)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_seq pg_catalog.text;
BEGIN
    v_seq := pg_catalog.pg_get_serial_sequence(p_table::pg_catalog.text, p_column);
    IF v_seq IS NOT NULL AND v_seq OPERATOR(pg_catalog.<>) pg_catalog.format('%I.%I',
         (SELECT n.nspname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
           WHERE c.oid OPERATOR(pg_catalog.=) p_table), p_new_name) THEN
        EXECUTE pg_catalog.format('ALTER SEQUENCE %s RENAME TO %I', v_seq, p_new_name);
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- The statements that recreate a table
-- ---------------------------------------------------------------------------

-- The rows of a table to a destination that is described, not looked up in the
-- catalog (a table that is being built): through the batch sender of the row
-- router, each row to the group that owns its bucket ('placement', by key_column,
-- in the colocation group colocation_id), to every active primary ('every_node')
-- or to one node ('one_node').  The destination table must exist on those nodes.
-- Returns the number of rows the destination acknowledged, and fails unless every
-- row that was read was stored.  Superusers only.
CREATE FUNCTION kingres.internal_copy_routed(source regclass, dest_schema name, dest_table name,
                                             dest_kind text, key_column name DEFAULT NULL,
                                             colocation_id int DEFAULT NULL, node_id int DEFAULT NULL)
    RETURNS bigint
    AS 'MODULE_PATHNAME', 'kingres_internal_copy_routed'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

-- internal_table_ddl() grows a third argument and new scopes, so it is replaced.
DROP FUNCTION kingres.internal_table_ddl(regclass, name);

-- The statements that recreate a table as a hash-partitioned parent with the
-- same columns, defaults, constraints and indexes (a plain table, for a NULL
-- column).  scope says where a statement runs:
--   remote_pre  other nodes only, before the table
--   local_pre   this node only, before the table (the old table still exists)
--   both        every node, the table itself (and, for an existing table that a
--               node learns about later, its owner)
--   swap        every node, after the rows are copied, with the old table gone
--               on this node: the names of the shadow take the place of the
--               names of the table (p_shadow only), and the sequences owned by
--               columns are moved to it
--   fk          every node, after the swap: the foreign keys of the table
--   post        every node, after the swap: sequence values, privileges, comments
--   post_local  this node only: the state of the identity sequences of the old table
-- With p_shadow the table is created as kingres_shadow_<oid> (constraints and
-- indexes under names that carry the oid), so that the old table can be read
-- while it is copied; without it the table is created under its own name.
-- Sequences owned by columns are detached from the old table so that dropping
-- it keeps them.
CREATE FUNCTION kingres.internal_table_ddl(p_relid regclass, p_column name, p_shadow bool DEFAULT false)
    RETURNS TABLE (ord pg_catalog.int4, scope pg_catalog.text, stmt pg_catalog.text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    c        pg_catalog.pg_class%ROWTYPE;
    nsp      pg_catalog.text;
    qname    pg_catalog.text;
    shname   pg_catalog.text;
    qtarget  pg_catalog.text;
    coldefs  pg_catalog.text;
    r        pg_catalog.record;
    q        pg_catalog.record;
    n        pg_catalog.int4 := 0;
    swaps    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    posts    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    locals   pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    p        pg_catalog.text;
    owner    pg_catalog.text;
    k        pg_catalog.int4 := 0;
    cname    pg_catalog.text;
    iname    pg_catalog.text;
    idef     pg_catalog.text;
    idseq    pg_catalog.text;
    state    pg_catalog.record;
BEGIN
    SELECT * INTO c FROM pg_class WHERE oid = p_relid;
    SELECT nspname INTO nsp FROM pg_namespace WHERE oid = c.relnamespace;
    qname := format('%I.%I', nsp, c.relname);
    shname := format('kingres_shadow_%s', p_relid::pg_catalog.oid);
    qtarget := CASE WHEN p_shadow THEN format('%I.%I', nsp, shname) ELSE qname END;

    IF p_column IS NOT NULL AND octet_length(c.relname) + 6 > 63 THEN
        RAISE EXCEPTION 'table name "%" is too long', c.relname
            USING ERRCODE = 'invalid_name',
                  DETAIL = 'Bucket partition names append 6 characters (_bNNNN) to the table name.';
    END IF;

    n := n + 1; ord := n; scope := 'remote_pre';
    stmt := format('CREATE SCHEMA IF NOT EXISTS %I', nsp);
    RETURN NEXT;

    FOR r IN
        SELECT format('%I.%I', sn.nspname, s.relname) AS qseq,
               (SELECT ro.rolname FROM pg_roles ro WHERE ro.oid = s.relowner) AS seqowner,
               a.attname,
               format_type(sq.seqtypid, NULL) AS seqtype,
               sq.seqstart, sq.seqincrement, sq.seqmax, sq.seqmin, sq.seqcache, sq.seqcycle
          FROM pg_depend d
          JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
          JOIN pg_namespace sn ON sn.oid = s.relnamespace
          JOIN pg_sequence sq ON sq.seqrelid = s.oid
          JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
         WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
           AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
           AND d.refobjid = p_relid AND d.deptype = 'a'
         ORDER BY a.attnum
    LOOP
        n := n + 1; ord := n; scope := 'remote_pre';
        stmt := format('CREATE SEQUENCE IF NOT EXISTS %s AS %s INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s %s',
                       r.qseq, r.seqtype, r.seqincrement, r.seqmin, r.seqmax, r.seqstart, r.seqcache,
                       CASE WHEN r.seqcycle THEN 'CYCLE' ELSE 'NO CYCLE' END);
        RETURN NEXT;
        IF r.seqowner IS DISTINCT FROM current_user THEN
            -- a sequence must have the owner of the table it belongs to
            n := n + 1; ord := n; scope := 'remote_pre';
            stmt := format('ALTER SEQUENCE %s OWNER TO %I', r.qseq, r.seqowner);
            RETURN NEXT;
        END IF;
        n := n + 1; ord := n; scope := 'local_pre';
        stmt := format('ALTER SEQUENCE %s OWNED BY NONE', r.qseq);
        RETURN NEXT;
        swaps := swaps || format('ALTER SEQUENCE %s OWNED BY %s.%I', r.qseq, qname, r.attname);
        posts := posts || format('SELECT kingres.internal_advance_sequence(%L::pg_catalog.regclass, %L::pg_catalog.regclass, %L)',
                                 r.qseq, qname, r.attname);
    END LOOP;

    -- The identity columns' sequences belong to the table (an internal dependency),
    -- so the new table makes its own: with the options of the old one, and under the
    -- old name after the swap.
    SELECT string_agg(
             format('%I %s%s%s%s%s', a.attname, format_type(a.atttypid, a.atttypmod),
                    CASE WHEN a.attcollation <> t.typcollation
                         THEN ' COLLATE ' || format('%I.%I', cn.nspname, co.collname)
                         ELSE '' END,
                    CASE a.attidentity
                         WHEN 'a' THEN ' GENERATED ALWAYS AS IDENTITY'
                         WHEN 'd' THEN ' GENERATED BY DEFAULT AS IDENTITY'
                         ELSE '' END
                    || CASE WHEN a.attidentity <> '' THEN
                         (SELECT format(' (INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s %s)',
                                        sq.seqincrement, sq.seqmin, sq.seqmax, sq.seqstart, sq.seqcache,
                                        CASE WHEN sq.seqcycle THEN 'CYCLE' ELSE 'NO CYCLE' END)
                            FROM pg_sequence sq
                           WHERE sq.seqrelid = pg_get_serial_sequence(qname, a.attname::pg_catalog.text)::pg_catalog.regclass)
                         ELSE '' END,
                    CASE WHEN a.attgenerated = 's'
                         THEN format(' GENERATED ALWAYS AS (%s) STORED', pg_get_expr(ad.adbin, ad.adrelid))
                         WHEN a.attgenerated = 'v'
                         THEN format(' GENERATED ALWAYS AS (%s) VIRTUAL', pg_get_expr(ad.adbin, ad.adrelid))
                         WHEN ad.adbin IS NOT NULL AND a.attidentity = ''
                         THEN ' DEFAULT ' || pg_get_expr(ad.adbin, ad.adrelid)
                         ELSE '' END,
                    CASE WHEN a.attnotnull THEN coalesce(format(' CONSTRAINT %I NOT NULL', nn.conname), ' NOT NULL') ELSE '' END),
             ', ' ORDER BY a.attnum)
      INTO coldefs
      FROM pg_attribute a
      JOIN pg_type t ON t.oid = a.atttypid
      LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
      LEFT JOIN pg_collation co ON co.oid = a.attcollation
      LEFT JOIN pg_namespace cn ON cn.oid = co.collnamespace
      LEFT JOIN pg_constraint nn ON nn.conrelid = a.attrelid AND nn.contype = 'n' AND nn.conkey[1] = a.attnum
     WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped;

    n := n + 1; ord := n; scope := 'both';
    -- A NULL column means a reference table: a plain table, a full copy per node.
    IF p_column IS NULL THEN
        stmt := format('CREATE TABLE %s (%s)', qtarget, coldefs);
    ELSE
        stmt := format('CREATE TABLE %s (%s) PARTITION BY HASH (%I)', qtarget, coldefs, p_column);
    END IF;
    RETURN NEXT;

    -- The identity columns of the new table: their sequences get the old names and
    -- the values the rows need.
    FOR r IN
        SELECT a.attname, a.attnum
          FROM pg_attribute a
         WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped AND a.attidentity <> ''
         ORDER BY a.attnum
    LOOP
        idseq := pg_get_serial_sequence(qname, r.attname::pg_catalog.text);
        IF p_shadow THEN
            swaps := swaps || format('SELECT kingres.internal_rename_identity(%L::pg_catalog.regclass, %L, %L)',
                                     qtarget, r.attname, split_part(idseq, '.', 2));
        END IF;
        posts := posts || format('SELECT kingres.internal_advance_sequence(pg_catalog.pg_get_serial_sequence(%L, %L)::pg_catalog.regclass, %L::pg_catalog.regclass, %L)',
                                 qname, r.attname, qname, r.attname);
        EXECUTE format('SELECT last_value, is_called FROM %s', idseq) INTO state;
        locals := locals || format('SELECT kingres.internal_advance_sequence(pg_catalog.pg_get_serial_sequence(%L, %L)::pg_catalog.regclass, %L::pg_catalog.regclass, %L, %s, %L)',
                                   qname, r.attname, qname, r.attname, state.last_value, state.is_called);
    END LOOP;

    FOR r IN
        SELECT k2.conname, pg_get_constraintdef(k2.oid) AS def, k2.contype
          FROM pg_constraint k2
         WHERE k2.conrelid = p_relid AND k2.contype IN ('p', 'u', 'c')
         ORDER BY CASE k2.contype WHEN 'p' THEN 0 WHEN 'u' THEN 1 ELSE 2 END, k2.conname
    LOOP
        n := n + 1; ord := n; scope := 'both';
        IF p_shadow AND r.contype IN ('p', 'u') THEN
            k := k + 1;
            cname := format('kingres_shadow_%s_c%s', p_relid::pg_catalog.oid, k);
            stmt := format('ALTER TABLE %s ADD CONSTRAINT %I %s', qtarget, cname, r.def);
            swaps := swaps || format('ALTER TABLE %s RENAME CONSTRAINT %I TO %I', qtarget, cname, r.conname);
        ELSE
            stmt := format('ALTER TABLE %s ADD CONSTRAINT %I %s', qtarget, r.conname, r.def);
        END IF;
        RETURN NEXT;
    END LOOP;

    -- Foreign keys of the table itself (not the copies in its partitions): they are
    -- added after every table exists, on the new table once it has its name.
    FOR r IN
        SELECT k2.conname, pg_get_constraintdef(k2.oid) AS def
          FROM pg_constraint k2
         WHERE k2.conrelid = p_relid AND k2.contype = 'f' AND k2.conparentid = 0
         ORDER BY k2.conname
    LOOP
        n := n + 1; ord := n; scope := 'fk';
        stmt := format('ALTER TABLE %s ADD CONSTRAINT %I %s', qname, r.conname, r.def);
        RETURN NEXT;
    END LOOP;

    k := 0;
    FOR r IN
        SELECT pg_get_indexdef(i.indexrelid) AS def, ic.relname AS iname
          FROM pg_index i
          JOIN pg_class ic ON ic.oid = i.indexrelid
         WHERE i.indrelid = p_relid
           AND NOT EXISTS (SELECT 1 FROM pg_constraint k2
                            WHERE k2.conindid = i.indexrelid AND k2.conrelid = p_relid
                              AND k2.contype IN ('p', 'u', 'x'))
         ORDER BY i.indexrelid
    LOOP
        n := n + 1; ord := n; scope := 'both';
        IF p_shadow THEN
            k := k + 1;
            iname := format('kingres_shadow_%s_i%s', p_relid::pg_catalog.oid, k);
            idef := replace(r.def, format(' INDEX %I ON %s USING ', r.iname, qname),
                                   format(' INDEX %I ON %s USING ', iname, qtarget));
            IF idef = r.def THEN
                RAISE EXCEPTION 'cannot rewrite the definition of index "%": %', r.iname, r.def
                    USING ERRCODE = 'internal_error';
            END IF;
            stmt := idef;
            swaps := swaps || format('ALTER INDEX %I.%I RENAME TO %I', nsp, iname, r.iname);
        ELSE
            stmt := r.def;
        END IF;
        RETURN NEXT;
    END LOOP;

    SELECT rolname INTO owner FROM pg_roles WHERE oid = c.relowner;
    IF owner IS DISTINCT FROM current_user THEN
        n := n + 1; ord := n; scope := 'both';
        stmt := format('ALTER TABLE %s OWNER TO %I', qtarget, owner);
        RETURN NEXT;
    END IF;

    -- After the copy: the names of the shadow's constraints, indexes and identity
    -- sequences become the old names, then the table takes its name, then the
    -- sequences that columns own move to it.
    IF p_shadow THEN
        FOR p IN SELECT s FROM unnest(swaps) s WHERE s LIKE 'ALTER TABLE % RENAME CONSTRAINT %'
                                                  OR s LIKE 'ALTER INDEX % RENAME TO %'
                                                  OR s LIKE 'SELECT kingres.internal_rename_identity%'
        LOOP
            n := n + 1; ord := n; scope := 'swap';
            stmt := p;
            RETURN NEXT;
        END LOOP;
        n := n + 1; ord := n; scope := 'swap';
        stmt := format('ALTER TABLE %s RENAME TO %I', qtarget, c.relname);
        RETURN NEXT;
    END IF;
    FOR p IN SELECT s FROM unnest(swaps) s WHERE s LIKE 'ALTER SEQUENCE % OWNED BY %'
    LOOP
        n := n + 1; ord := n;
        scope := CASE WHEN p_shadow THEN 'swap' ELSE 'both' END;
        stmt := p;
        RETURN NEXT;
    END LOOP;

    -- Sequence values, then the privileges and comments of the old table.
    FOREACH p IN ARRAY posts LOOP
        n := n + 1; ord := n; scope := 'post';
        stmt := p;
        RETURN NEXT;
    END LOOP;
    FOREACH p IN ARRAY locals LOOP
        n := n + 1; ord := n; scope := 'post_local';
        stmt := p;
        RETURN NEXT;
    END LOOP;

    IF p_shadow OR p_column IS NULL THEN
        -- table privileges: the owner's own entries first, so that the entries other
        -- grantors made find their grant option
        IF c.relacl IS NOT NULL THEN
            n := n + 1; ord := n; scope := 'post';
            stmt := format('REVOKE ALL ON TABLE %s FROM PUBLIC, %I', qname, owner);
            RETURN NEXT;
            FOR q IN
                SELECT x.grantor, x.grantee, x.is_grantable,
                       string_agg(x.privilege_type, ', ' ORDER BY x.privilege_type) AS privs
                  FROM aclexplode(c.relacl) x
                 GROUP BY x.grantor, x.grantee, x.is_grantable
                 ORDER BY (x.grantor = c.relowner) DESC, x.grantor, x.grantee, x.is_grantable
            LOOP
                IF q.grantor <> c.relowner THEN
                    n := n + 1; ord := n; scope := 'post';
                    stmt := format('SET LOCAL ROLE %I', (SELECT rolname FROM pg_roles WHERE oid = q.grantor));
                    RETURN NEXT;
                END IF;
                n := n + 1; ord := n; scope := 'post';
                stmt := format('GRANT %s ON TABLE %s TO %s%s', q.privs, qname,
                               CASE WHEN q.grantee = 0 THEN 'PUBLIC'
                                    ELSE format('%I', (SELECT rolname FROM pg_roles WHERE oid = q.grantee)) END,
                               CASE WHEN q.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END);
                RETURN NEXT;
                IF q.grantor <> c.relowner THEN
                    n := n + 1; ord := n; scope := 'post';
                    stmt := 'RESET ROLE';
                    RETURN NEXT;
                END IF;
            END LOOP;
        END IF;
        -- column privileges
        FOR r IN
            SELECT a.attname, a.attacl FROM pg_attribute a
             WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped AND a.attacl IS NOT NULL
             ORDER BY a.attnum
        LOOP
            FOR q IN
                SELECT x.grantor, x.grantee, x.is_grantable,
                       string_agg(format('%s (%I)', x.privilege_type, r.attname), ', ' ORDER BY x.privilege_type) AS privs
                  FROM aclexplode(r.attacl) x
                 GROUP BY x.grantor, x.grantee, x.is_grantable
                 ORDER BY (x.grantor = c.relowner) DESC, x.grantor, x.grantee, x.is_grantable
            LOOP
                IF q.grantor <> c.relowner THEN
                    n := n + 1; ord := n; scope := 'post';
                    stmt := format('SET LOCAL ROLE %I', (SELECT rolname FROM pg_roles WHERE oid = q.grantor));
                    RETURN NEXT;
                END IF;
                n := n + 1; ord := n; scope := 'post';
                stmt := format('GRANT %s ON TABLE %s TO %s%s', q.privs, qname,
                               CASE WHEN q.grantee = 0 THEN 'PUBLIC'
                                    ELSE format('%I', (SELECT rolname FROM pg_roles WHERE oid = q.grantee)) END,
                               CASE WHEN q.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END);
                RETURN NEXT;
                IF q.grantor <> c.relowner THEN
                    n := n + 1; ord := n; scope := 'post';
                    stmt := 'RESET ROLE';
                    RETURN NEXT;
                END IF;
            END LOOP;
        END LOOP;
        -- comments: the table, its columns, its constraints and its indexes
        FOR r IN
            SELECT d.objsubid, d.description FROM pg_description d
             WHERE d.classoid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objoid = p_relid
             ORDER BY d.objsubid
        LOOP
            n := n + 1; ord := n; scope := 'post';
            IF r.objsubid = 0 THEN
                stmt := format('COMMENT ON TABLE %s IS %L', qname, r.description);
            ELSE
                stmt := format('COMMENT ON COLUMN %s.%I IS %L', qname,
                               (SELECT a.attname FROM pg_attribute a WHERE a.attrelid = p_relid AND a.attnum = r.objsubid),
                               r.description);
            END IF;
            RETURN NEXT;
        END LOOP;
        FOR r IN
            SELECT k2.conname, d.description FROM pg_constraint k2
              JOIN pg_description d ON d.classoid = 'pg_catalog.pg_constraint'::pg_catalog.regclass AND d.objoid = k2.oid
             WHERE k2.conrelid = p_relid AND k2.contype IN ('p', 'u', 'c', 'f') AND k2.conparentid = 0
             ORDER BY k2.conname
        LOOP
            n := n + 1; ord := n; scope := 'post';
            stmt := format('COMMENT ON CONSTRAINT %I ON %s IS %L', r.conname, qname, r.description);
            RETURN NEXT;
        END LOOP;
        FOR r IN
            SELECT ic.relname, d.description FROM pg_index i
              JOIN pg_class ic ON ic.oid = i.indexrelid
              JOIN pg_description d ON d.classoid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objoid = i.indexrelid
             WHERE i.indrelid = p_relid
             ORDER BY ic.relname
        LOOP
            n := n + 1; ord := n; scope := 'post';
            stmt := format('COMMENT ON INDEX %I.%I IS %L', nsp, r.relname, r.description);
            RETURN NEXT;
        END LOOP;
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- The report of what a change of a table would meet
-- ---------------------------------------------------------------------------

-- What prevents a change of a table (severity 'blocker'), what the change would
-- not carry over ('warning') and what is good to know ('note'), without changing
-- anything.  kind is distribute, distribute_reference, undistribute, alter,
-- create_partition, attach_partition, detach_partition or drop_partition; options
-- holds what the call would be given: column, colocate_with, bucket_count and
-- mode ('online') for a distribution; cascade_via_foreign_keys and target_group
-- for undistribute; distribution_column, bucket_count, colocate_with and
-- cascade_to_colocated for alter.  The report never stops at the first problem.
-- The codes are listed in the README of the extension.  For superusers (or roles
-- they grant execute to): it asks the other nodes for the functions and roles the
-- table uses.
CREATE FUNCTION kingres.table_change_report(kind text, relation regclass,
                                            options jsonb DEFAULT '{}'::jsonb)
    RETURNS TABLE (severity text, code text, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_variable
DECLARE
    rep       pg_catalog.jsonb := '[]'::pg_catalog.jsonb;
    c         pg_catalog.pg_class%ROWTYPE;
    nsp       pg_catalog.text;
    rname     pg_catalog.text;
    qname     pg_catalog.text;
    pfx       pg_catalog.text;
    v_col     pg_catalog.name := options OPERATOR(pg_catalog.->>) 'column';
    v_mode    pg_catalog.text := options OPERATOR(pg_catalog.->>) 'mode';
    v_cwith   pg_catalog.regclass;
    v_buckets pg_catalog.int4 := (options OPERATOR(pg_catalog.->>) 'bucket_count')::pg_catalog.int4;
    v_attnum  pg_catalog.int2;
    v_atttype pg_catalog.oid;
    v_group   pg_catalog.int4;
    v_dt      kingres.dist_table%ROWTYPE;
    r         pg_catalog.record;
    x         pg_catalog.text;
    names     pg_catalog.text[];
    nd        pg_catalog.record;
    probe     pg_catalog.text;
    local_id  pg_catalog.int4;
    is_dist   pg_catalog.bool;
    pos       pg_catalog.int4;
    ok        pg_catalog.bool;
BEGIN
    IF kind IS NULL OR kind NOT IN ('distribute', 'distribute_reference', 'undistribute', 'alter',
                                    'create_partition', 'attach_partition', 'detach_partition',
                                    'drop_partition') THEN
        RAISE EXCEPTION 'unknown kind "%"', kind USING ERRCODE = 'invalid_parameter_value',
              HINT = 'Use distribute, distribute_reference, undistribute, alter, create_partition, attach_partition, detach_partition or drop_partition.';
    END IF;
    SELECT * INTO c FROM pg_catalog.pg_class k WHERE k.oid OPERATOR(pg_catalog.=) relation::pg_catalog.oid;
    SELECT n.nspname INTO nsp FROM pg_catalog.pg_namespace n WHERE n.oid OPERATOR(pg_catalog.=) c.relnamespace;
    rname := c.relname;
    qname := pg_catalog.format('%I.%I', nsp, rname);
    pfx := CASE kind WHEN 'distribute' THEN pg_catalog.format('cannot distribute table "%s": ', rname)
                     WHEN 'distribute_reference' THEN pg_catalog.format('cannot make table "%s" a reference table: ', rname)
                     ELSE pg_catalog.format('cannot change table "%s": ', rname) END;
    SELECT * INTO v_dt FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) relation;
    is_dist := FOUND;
    IF options OPERATOR(pg_catalog.->>) 'colocate_with' IS NOT NULL
       AND options OPERATOR(pg_catalog.->>) 'colocate_with' NOT IN ('default', 'none') THEN
        v_cwith := pg_catalog.regclassin(pg_catalog.textout((options OPERATOR(pg_catalog.->>) 'colocate_with')::pg_catalog.text));
    END IF;
    SELECT l.node_id INTO local_id FROM kingres.local_node l;

    -- Where the statement runs.
    BEGIN
        PERFORM kingres.internal_require_map_group();
    EXCEPTION WHEN OTHERS THEN
        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                   'blocker', 'not_on_map_group', qname, SQLERRM));
    END;

    IF kind IN ('distribute', 'distribute_reference') THEN
        IF is_dist THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'already_distributed', qname,
                       CASE kind WHEN 'distribute' THEN pg_catalog.format('table "%s" is already distributed', rname)
                                 ELSE pg_catalog.format('table "%s" is already distributed or a reference table', rname) END));
            RETURN QUERY SELECT e OPERATOR(pg_catalog.->>) 0, e OPERATOR(pg_catalog.->>) 1, e OPERATOR(pg_catalog.->>) 2, e OPERATOR(pg_catalog.->>) 3
                           FROM pg_catalog.jsonb_array_elements(rep) WITH ORDINALITY AS t(e, o) ORDER BY o;
            RETURN;
        END IF;
        IF c.relkind NOT IN ('r', 'p') THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'not_ordinary_table', qname,
                       CASE kind WHEN 'distribute' THEN pg_catalog.format('cannot distribute "%s": it is not an ordinary table', rname)
                                 ELSE pg_catalog.format('cannot make "%s" a reference table: it is not an ordinary table', rname) END));
            RETURN QUERY SELECT e OPERATOR(pg_catalog.->>) 0, e OPERATOR(pg_catalog.->>) 1, e OPERATOR(pg_catalog.->>) 2, e OPERATOR(pg_catalog.->>) 3
                           FROM pg_catalog.jsonb_array_elements(rep) WITH ORDINALITY AS t(e, o) ORDER BY o;
            RETURN;
        END IF;
        IF c.relkind OPERATOR(pg_catalog.=) 'p' OR c.relispartition THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'partitioned_table', qname,
                       pfx OPERATOR(pg_catalog.||) 'partitioned tables and partitions are not supported yet'));
        END IF;
        IF c.relpersistence OPERATOR(pg_catalog.<>) 'p' THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'temporary_or_unlogged', qname,
                       pfx OPERATOR(pg_catalog.||) 'temporary and unlogged tables are not supported'));
        END IF;
        IF c.relkind OPERATOR(pg_catalog.=) 'r'
           AND EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i
                        WHERE i.inhrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid
                           OR i.inhparent OPERATOR(pg_catalog.=) relation::pg_catalog.oid) THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'inheritance', qname, pfx OPERATOR(pg_catalog.||) 'inheritance is not supported'));
        END IF;
        IF c.reloptions IS NOT NULL THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'storage_parameters', qname,
                       pfx OPERATOR(pg_catalog.||) 'storage parameters are not supported yet'));
        END IF;
        IF c.relrowsecurity OR EXISTS (SELECT 1 FROM pg_catalog.pg_policy p WHERE p.polrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid) THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'row_level_security', qname,
                       pfx OPERATOR(pg_catalog.||) 'row-level security is not supported yet'));
        END IF;
        FOR r IN SELECT k.conname FROM pg_catalog.pg_constraint k
                  WHERE k.contype OPERATOR(pg_catalog.=) 'x' AND k.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid
                  ORDER BY k.conname
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'exclusion_constraint', r.conname,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('exclusion constraint "%s" is not supported', r.conname)));
        END LOOP;
        FOR r IN SELECT t.tgname FROM pg_catalog.pg_trigger t
                  WHERE t.tgrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND NOT t.tgisinternal
                  ORDER BY t.tgname
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'trigger', r.tgname,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('trigger "%s" is not supported yet', r.tgname)));
        END LOOP;

        -- tablespaces are not synchronized between the nodes
        IF c.reltablespace OPERATOR(pg_catalog.<>) 0 THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'tablespace', qname,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('the table is in tablespace "%s"; tablespaces are not synchronized between the nodes',
                                                                      (SELECT s.spcname FROM pg_catalog.pg_tablespace s WHERE s.oid OPERATOR(pg_catalog.=) c.reltablespace))));
        END IF;
        FOR r IN SELECT ic.relname, s.spcname FROM pg_catalog.pg_index i
                   JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) i.indexrelid
                   JOIN pg_catalog.pg_tablespace s ON s.oid OPERATOR(pg_catalog.=) ic.reltablespace
                  WHERE i.indrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND ic.reltablespace OPERATOR(pg_catalog.<>) 0
                  ORDER BY ic.relname
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'tablespace', r.relname,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('index "%s" is in tablespace "%s"; tablespaces are not synchronized between the nodes', r.relname, r.spcname)));
        END LOOP;

        -- a ledger table, or the history table of one
        IF pg_catalog.to_regclass('kingres.ledger_table') IS NOT NULL THEN
            EXECUTE pg_catalog.format('SELECT EXISTS (SELECT 1 FROM kingres.ledger_table t WHERE t.relid OPERATOR(pg_catalog.=) %L::pg_catalog.regclass%s)',
                       relation,
                       CASE WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
                                          WHERE a.attrelid OPERATOR(pg_catalog.=) 'kingres.ledger_table'::pg_catalog.regclass
                                            AND a.attname OPERATOR(pg_catalog.=) 'history_relid' AND NOT a.attisdropped)
                            THEN pg_catalog.format(' OR t.history_relid OPERATOR(pg_catalog.=) %L::pg_catalog.regclass', relation)
                            ELSE '' END)
               INTO ok;
            IF ok THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'ledger_table', qname,
                           pfx OPERATOR(pg_catalog.||) 'the table is a ledger table (or the history table of one); its capture trigger and its protection are the integrity mechanism and a copy would start a new history'));
            END IF;
        END IF;

        -- objects that depend on the table and would stay bound to the old one
        FOR r IN SELECT DISTINCT cl.relkind, cl.oid::pg_catalog.regclass::pg_catalog.text AS name
                   FROM pg_catalog.pg_depend d
                   JOIN pg_catalog.pg_rewrite rw ON rw.oid OPERATOR(pg_catalog.=) d.objid
                   JOIN pg_catalog.pg_class cl ON cl.oid OPERATOR(pg_catalog.=) rw.ev_class
                  WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass
                    AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
                    AND d.refobjid OPERATOR(pg_catalog.=) relation::pg_catalog.oid
                    AND rw.ev_class OPERATOR(pg_catalog.<>) relation::pg_catalog.oid
                  ORDER BY 2
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'dependent_object', r.name,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('%s %s depends on the table; the table is replaced by a new one',
                           CASE r.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' ELSE 'object' END, r.name)));
        END LOOP;
        FOR r IN SELECT DISTINCT p.oid::pg_catalog.regprocedure::pg_catalog.text AS name
                   FROM pg_catalog.pg_depend d
                   JOIN pg_catalog.pg_proc p ON p.oid OPERATOR(pg_catalog.=) d.objid
                  WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_proc'::pg_catalog.regclass
                    AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_type'::pg_catalog.regclass
                    AND d.refobjid OPERATOR(pg_catalog.=) c.reltype
                  ORDER BY 1
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'dependent_object', r.name,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('function %s uses the row type of the table', r.name)));
        END LOOP;
        FOR r IN SELECT p.pubname FROM pg_catalog.pg_publication_rel pr
                   JOIN pg_catalog.pg_publication p ON p.oid OPERATOR(pg_catalog.=) pr.prpubid
                  WHERE pr.prrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid
                  ORDER BY p.pubname
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'dependent_object', r.pubname,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('publication "%s" publishes the table', r.pubname)));
        END LOOP;

        -- foreign keys
        FOR r IN SELECT k.oid, k.conname, k.conrelid, k.confrelid, k.conkey, k.confkey
                   FROM pg_catalog.pg_constraint k
                  WHERE k.contype OPERATOR(pg_catalog.=) 'f' AND k.conparentid OPERATOR(pg_catalog.=) 0
                    AND (k.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid
                         OR k.confrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid)
                  ORDER BY k.conname
        LOOP
            IF r.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid THEN
                -- a key from the table
                IF r.confrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid THEN
                    -- to itself: for a distributed table the key must use the distribution column on both sides
                    IF kind OPERATOR(pg_catalog.=) 'distribute' AND v_col IS NOT NULL THEN
                        SELECT a.attnum INTO v_attnum FROM pg_catalog.pg_attribute a
                         WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col AND NOT a.attisdropped;
                        ok := false;
                        FOR pos IN 1 .. pg_catalog.cardinality(r.conkey) LOOP
                            IF r.conkey[pos] OPERATOR(pg_catalog.=) v_attnum AND r.confkey[pos] OPERATOR(pg_catalog.=) v_attnum THEN
                                ok := true;
                            END IF;
                        END LOOP;
                        IF NOT ok THEN
                            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                       'blocker', 'foreign_key_not_distribution_column', r.conname,
                                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('foreign key "%s" does not use the distribution column "%s" on both sides', r.conname, v_col)));
                        END IF;
                    END IF;
                ELSE
                    SELECT * INTO v_dt FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) r.confrelid::pg_catalog.regclass;
                    IF NOT FOUND THEN
                        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                   'blocker', 'foreign_key_plain_table', r.conname,
                                   pfx OPERATOR(pg_catalog.||) pg_catalog.format('foreign key "%s" references ordinary table "%s"; a key to an ordinary table is not supported',
                                                                                 r.conname, r.confrelid::pg_catalog.regclass)));
                    ELSIF v_dt.method OPERATOR(pg_catalog.=) 'hash' THEN
                        IF kind OPERATOR(pg_catalog.=) 'distribute_reference' THEN
                            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                       'blocker', 'foreign_key_other_group', r.conname,
                                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('foreign key "%s" references distributed table "%s"; a reference table can only reference reference tables',
                                                                                     r.conname, r.confrelid::pg_catalog.regclass)));
                        ELSE
                            -- the group the table will be in
                            v_group := NULL;
                            IF v_cwith IS NOT NULL THEN
                                SELECT d.colocation_id INTO v_group FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_cwith;
                            ELSIF v_col IS NOT NULL THEN
                                SELECT co.colocation_id INTO v_group FROM kingres.colocation co
                                 WHERE co.dist_column_type::pg_catalog.oid OPERATOR(pg_catalog.=)
                                           (SELECT a.atttypid FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col AND NOT a.attisdropped)
                                   AND co.dist_collation OPERATOR(pg_catalog.=)
                                           (SELECT a.attcollation FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col AND NOT a.attisdropped)
                                   AND co.bucket_count OPERATOR(pg_catalog.=) COALESCE(v_buckets, pg_catalog.current_setting('kingres_shard.default_bucket_count')::pg_catalog.int4)
                                 ORDER BY co.colocation_id LIMIT 1;
                            END IF;
                            IF v_group IS DISTINCT FROM v_dt.colocation_id THEN
                                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                           'blocker', 'foreign_key_other_group', r.conname,
                                           pfx OPERATOR(pg_catalog.||) pg_catalog.format('foreign key "%s" references distributed table "%s" of another colocation group; pass colocate_with => ''%s''',
                                                                                         r.conname, r.confrelid::pg_catalog.regclass, r.confrelid::pg_catalog.regclass)));
                            ELSIF v_col IS NOT NULL THEN
                                SELECT a.attnum INTO v_attnum FROM pg_catalog.pg_attribute a
                                 WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col AND NOT a.attisdropped;
                                ok := false;
                                FOR pos IN 1 .. pg_catalog.cardinality(r.conkey) LOOP
                                    IF r.conkey[pos] OPERATOR(pg_catalog.=) v_attnum
                                       AND r.confkey[pos] OPERATOR(pg_catalog.=)
                                           (SELECT a.attnum FROM pg_catalog.pg_attribute a
                                             WHERE a.attrelid OPERATOR(pg_catalog.=) r.confrelid AND a.attname OPERATOR(pg_catalog.=) v_dt.dist_column AND NOT a.attisdropped) THEN
                                        ok := true;
                                    END IF;
                                END LOOP;
                                IF NOT ok THEN
                                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                               'blocker', 'foreign_key_not_distribution_column', r.conname,
                                               pfx OPERATOR(pg_catalog.||) pg_catalog.format('foreign key "%s" does not use the distribution columns of both tables', r.conname)));
                                END IF;
                            END IF;
                        END IF;
                    END IF;
                END IF;
            ELSE
                -- a key to the table, from another table
                IF EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) r.conrelid::pg_catalog.regclass) THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'referenced_by_distributed_table', r.conname,
                               pfx OPERATOR(pg_catalog.||) pg_catalog.format('foreign key "%s" of table "%s" references the table',
                                                                             r.conname, r.conrelid::pg_catalog.regclass)));
                ELSE
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'referenced_by_plain_table', r.conname,
                               pfx OPERATOR(pg_catalog.||) pg_catalog.format('foreign key "%s" of ordinary table "%s" references the table; a key from an ordinary table is not supported',
                                                                             r.conname, r.conrelid::pg_catalog.regclass)));
                END IF;
            END IF;
        END LOOP;

        -- the column and the keys that must contain it
        IF kind OPERATOR(pg_catalog.=) 'distribute' AND v_col IS NOT NULL THEN
            SELECT a.attnum, a.atttypid INTO v_attnum, v_atttype FROM pg_catalog.pg_attribute a
             WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col
               AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped;
            IF NOT FOUND THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'column_missing', v_col::pg_catalog.text,
                           pg_catalog.format('column "%s" of table "%s" does not exist', v_col, rname)));
            ELSE
                IF NOT kingres.internal_has_hash_support(v_atttype::pg_catalog.regtype) THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'no_hash_support', v_col::pg_catalog.text,
                               pg_catalog.format('column "%s" of type %s cannot be a distribution column: the type has no default hash operator class with an extended hash function',
                                                 v_col, pg_catalog.format_type(v_atttype, NULL))));
                END IF;
                FOR r IN SELECT ic.relname AS index_name, i.indkey::pg_catalog.int2[] AS cols, k.contype
                           FROM pg_catalog.pg_index i
                           JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) i.indexrelid
                           LEFT JOIN pg_catalog.pg_constraint k
                                  ON k.conindid OPERATOR(pg_catalog.=) i.indexrelid AND k.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid
                                 AND k.contype IN ('p', 'u')
                          WHERE i.indrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND i.indisunique
                          ORDER BY ic.relname
                LOOP
                    IF NOT (v_attnum OPERATOR(pg_catalog.=) ANY (r.cols)) THEN
                        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                   'blocker', 'unique_without_distribution_column', r.index_name,
                                   pg_catalog.format('cannot distribute table "%s": %s "%s" does not include the distribution column "%s"; a unique or primary key constraint of a distributed table must include the distribution column',
                                       rname,
                                       CASE r.contype WHEN 'p' THEN 'primary key constraint' WHEN 'u' THEN 'unique constraint' ELSE 'unique index' END,
                                       r.index_name, v_col)));
                    END IF;
                END LOOP;
            END IF;
            IF pg_catalog.octet_length(rname) OPERATOR(pg_catalog.+) 6 OPERATOR(pg_catalog.>) 63 THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'name_too_long', qname,
                           pg_catalog.format('table name "%s" is too long: bucket partition names append 6 characters (_bNNNN) to the table name', rname)));
            END IF;
            FOR r IN SELECT cl.relname FROM pg_catalog.pg_class cl
                      WHERE cl.relnamespace OPERATOR(pg_catalog.=) c.relnamespace
                        AND ((pg_catalog.left(cl.relname, pg_catalog.length(rname) + 2) = (rname || '_b')
                              AND pg_catalog.length(cl.relname) = pg_catalog.length(rname) + 6
                              AND pg_catalog.right(cl.relname, 4) ~ '^[0-9]{4}$')
                             OR cl.relname = pg_catalog.format('kingres_shadow_%s', relation::pg_catalog.oid))
                      ORDER BY cl.relname
            LOOP
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'name_in_use', r.relname,
                           pg_catalog.format('relation "%s" exists; its name is needed for a bucket partition of the table', r.relname)));
            END LOOP;
        END IF;

        -- the key of an online copy
        IF v_mode OPERATOR(pg_catalog.=) 'online' AND NOT EXISTS (
               SELECT 1 FROM pg_catalog.pg_index i
                WHERE i.indrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND i.indisunique AND i.indpred IS NULL AND i.indisvalid
                  AND NOT EXISTS (SELECT 1 FROM pg_catalog.unnest(i.indkey::pg_catalog.int2[]) kk(attno)
                                   WHERE kk.attno OPERATOR(pg_catalog.=) 0
                                      OR NOT (SELECT a.attnotnull FROM pg_catalog.pg_attribute a
                                               WHERE a.attrelid OPERATOR(pg_catalog.=) i.indrelid AND a.attnum OPERATOR(pg_catalog.=) kk.attno))) THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'no_key_for_online', qname,
                       pfx OPERATOR(pg_catalog.||) 'the online variant needs a primary key or a unique index over NOT NULL columns; use the blocking variant'));
        END IF;

        -- the group the table would join must not be in use
        IF v_cwith IS NOT NULL THEN
            SELECT d.colocation_id INTO v_group FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_cwith;
            IF v_group IS NOT NULL THEN
                IF EXISTS (SELECT 1 FROM kingres.bucket_move m
                            WHERE m.colocation_id OPERATOR(pg_catalog.=) v_group
                              AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching')) THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'group_busy', v_group::pg_catalog.text,
                               pg_catalog.format('colocation group %s is busy: a bucket is being moved', v_group)));
                ELSIF EXISTS (SELECT 1 FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) v_group) THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'group_busy', v_group::pg_catalog.text,
                               pg_catalog.format('colocation group %s is busy: a table operation uses it', v_group)));
                END IF;
            END IF;
        END IF;

        -- what the other nodes must have: the roles of the table and the functions of its defaults
        SELECT pg_catalog.array_agg(DISTINCT s.ro) INTO names FROM (
            SELECT (SELECT r2.rolname FROM pg_catalog.pg_roles r2 WHERE r2.oid OPERATOR(pg_catalog.=) c.relowner) AS ro
            UNION
            SELECT (SELECT r2.rolname FROM pg_catalog.pg_roles r2 WHERE r2.oid OPERATOR(pg_catalog.=) e.grantee)
              FROM pg_catalog.aclexplode(c.relacl) e WHERE e.grantee OPERATOR(pg_catalog.<>) 0
            UNION
            SELECT (SELECT r2.rolname FROM pg_catalog.pg_roles r2 WHERE r2.oid OPERATOR(pg_catalog.=) e.grantor)
              FROM pg_catalog.aclexplode(c.relacl) e
            UNION
            SELECT (SELECT r2.rolname FROM pg_catalog.pg_roles r2 WHERE r2.oid OPERATOR(pg_catalog.=) e.grantee)
              FROM pg_catalog.pg_attribute a, pg_catalog.aclexplode(a.attacl) e
             WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attacl IS NOT NULL AND e.grantee OPERATOR(pg_catalog.<>) 0
            UNION
            SELECT (SELECT r2.rolname FROM pg_catalog.pg_roles r2 WHERE r2.oid OPERATOR(pg_catalog.=) e.grantor)
              FROM pg_catalog.pg_attribute a, pg_catalog.aclexplode(a.attacl) e
             WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attacl IS NOT NULL
        ) s WHERE s.ro IS NOT NULL;
        SELECT pg_catalog.string_agg(DISTINCT pr.oid::pg_catalog.regprocedure::pg_catalog.text, E'\n') INTO x
          FROM pg_catalog.pg_depend d
          JOIN pg_catalog.pg_attrdef ad ON ad.oid OPERATOR(pg_catalog.=) d.objid
          JOIN pg_catalog.pg_proc pr ON pr.oid OPERATOR(pg_catalog.=) d.refobjid
         WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_attrdef'::pg_catalog.regclass
           AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_proc'::pg_catalog.regclass
           AND ad.adrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid
           AND pr.pronamespace OPERATOR(pg_catalog.<>) 'pg_catalog'::pg_catalog.regnamespace;
        FOR nd IN SELECT n.node_id, n.host, n.port FROM kingres.node n
                   WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active
                     AND n.node_id OPERATOR(pg_catalog.<>) COALESCE(local_id, 0)
                   ORDER BY n.node_id
        LOOP
            BEGIN
                IF names IS NOT NULL THEN
                    probe := kingres.internal_exec_scalar(nd.node_id,
                        pg_catalog.format('SELECT pg_catalog.string_agg(q, '','' ORDER BY q) FROM pg_catalog.unnest(%L::pg_catalog.text[]) q WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) q)', names::pg_catalog.text),
                        (SELECT s.setting::pg_catalog.int4 OPERATOR(pg_catalog.*) 2000 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout'));
                    IF probe IS NOT NULL THEN
                        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                   'blocker', 'role_missing', probe,
                                   pfx OPERATOR(pg_catalog.||) pg_catalog.format('role(s) %s do not exist on node %s; roles are not synchronized between the nodes', probe, nd.node_id)));
                    END IF;
                END IF;
                IF x IS NOT NULL THEN
                    probe := kingres.internal_exec_scalar(nd.node_id,
                        pg_catalog.format('SELECT pg_catalog.string_agg(q, '','' ORDER BY q) FROM pg_catalog.unnest(pg_catalog.string_to_array(%L, E''\n'')) q WHERE pg_catalog.to_regprocedure(q) IS NULL', x),
                        (SELECT s.setting::pg_catalog.int4 OPERATOR(pg_catalog.*) 2000 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout'));
                    IF probe IS NOT NULL THEN
                        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                   'blocker', 'default_function_missing', probe,
                                   pfx OPERATOR(pg_catalog.||) pg_catalog.format('function(s) %s used by column defaults do not exist on node %s; functions are not synchronized between the nodes', probe, nd.node_id)));
                    END IF;
                END IF;
            EXCEPTION WHEN OTHERS THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'node_unreachable', nd.node_id::pg_catalog.text,
                           pg_catalog.format('node %s (%s:%s) could not be asked: %s', nd.node_id, nd.host, nd.port, SQLERRM)));
            END;
        END LOOP;

        -- what is not carried
        FOR r IN SELECT s.stxname FROM pg_catalog.pg_statistic_ext s
                  WHERE s.stxrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid ORDER BY s.stxname
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'warning', 'not_carried', r.stxname,
                       pg_catalog.format('statistics object "%s" is not carried to the new table', r.stxname)));
        END LOOP;
        FOR r IN SELECT a.attname FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid OPERATOR(pg_catalog.=) a.atttypid
                  WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
                    AND (a.attstorage OPERATOR(pg_catalog.<>) t.typstorage OR a.attcompression OPERATOR(pg_catalog.<>) ''
                         OR a.attstattarget IS NOT NULL)
                  ORDER BY a.attnum
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'warning', 'not_carried', r.attname::pg_catalog.text,
                       pg_catalog.format('the storage, compression or statistics target of column "%s" is not carried to the new table', r.attname)));
        END LOOP;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND i.indisclustered) THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'warning', 'not_carried', qname, 'CLUSTER ON is not carried to the new table'));
        END IF;
        IF c.relreplident OPERATOR(pg_catalog.<>) 'd' THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'warning', 'not_carried', qname, 'the replica identity is not carried to the new table'));
        END IF;

        IF c.reltuples OPERATOR(pg_catalog.>) 0 AND kind OPERATOR(pg_catalog.=) 'distribute' THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'note', 'rows', qname,
                       pg_catalog.format('the table holds about %s rows; writers wait while they are copied', c.reltuples::pg_catalog.int8)));
        END IF;

    ELSIF kind OPERATOR(pg_catalog.=) 'undistribute' THEN
        IF NOT is_dist THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'not_distributed', qname, pg_catalog.format('table "%s" is not a distributed or reference table', rname)));
        ELSE
            IF options OPERATOR(pg_catalog.->>) 'target_group' IS NOT NULL
               AND NOT EXISTS (SELECT 1 FROM kingres.node n
                                WHERE n.group_id OPERATOR(pg_catalog.=) (options OPERATOR(pg_catalog.->>) 'target_group')::pg_catalog.int4
                                  AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active) THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'target_group_invalid', options OPERATOR(pg_catalog.->>) 'target_group',
                           pg_catalog.format('shard group %s has no active primary', options OPERATOR(pg_catalog.->>) 'target_group')));
            END IF;
            IF COALESCE((options OPERATOR(pg_catalog.->>) 'cascade_via_foreign_keys')::pg_catalog.bool, false) IS NOT TRUE THEN
                FOR r IN SELECT k.conname, k.conrelid, k.confrelid FROM pg_catalog.pg_constraint k
                          WHERE k.contype OPERATOR(pg_catalog.=) 'f' AND k.conparentid OPERATOR(pg_catalog.=) 0
                            AND k.conrelid OPERATOR(pg_catalog.<>) k.confrelid
                            AND (k.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid OR k.confrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid)
                            AND EXISTS (SELECT 1 FROM kingres.dist_table d
                                         WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=)
                                               CASE WHEN k.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid THEN k.confrelid ELSE k.conrelid END)
                          ORDER BY k.conname
                LOOP
                    -- a key from a distributed table to a reference table is kept
                    IF NOT (r.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND v_dt.method OPERATOR(pg_catalog.=) 'hash'
                            AND EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) r.confrelid AND d.method OPERATOR(pg_catalog.=) 'reference')) THEN
                        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                   'blocker', 'foreign_key_linked', r.conname,
                                   pg_catalog.format('foreign key "%s" links "%s" and "%s"; undistribute them together with cascade_via_foreign_keys => true, or drop the key',
                                                     r.conname, r.conrelid::pg_catalog.regclass, r.confrelid::pg_catalog.regclass)));
                    END IF;
                END LOOP;
            END IF;
            IF v_dt.colocation_id IS NOT NULL THEN
                IF EXISTS (SELECT 1 FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id) THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'group_busy', v_dt.colocation_id::pg_catalog.text,
                               pg_catalog.format('colocation group %s is busy: a table operation uses it', v_dt.colocation_id)));
                END IF;
            END IF;
        END IF;

    ELSIF kind OPERATOR(pg_catalog.=) 'alter' THEN
        IF NOT is_dist OR v_dt.method OPERATOR(pg_catalog.<>) 'hash' THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'not_distributed', qname, pg_catalog.format('table "%s" is not a distributed table', rname)));
        ELSE
            v_col := options OPERATOR(pg_catalog.->>) 'distribution_column';
            IF v_col IS NOT NULL THEN
                SELECT a.attnum, a.atttypid INTO v_attnum, v_atttype FROM pg_catalog.pg_attribute a
                 WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col
                   AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped;
                IF NOT FOUND THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'column_missing', v_col::pg_catalog.text,
                               pg_catalog.format('column "%s" of table "%s" does not exist', v_col, rname)));
                ELSE
                    IF NOT kingres.internal_has_hash_support(v_atttype::pg_catalog.regtype) THEN
                        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                   'blocker', 'no_hash_support', v_col::pg_catalog.text,
                                   pg_catalog.format('column "%s" of type %s cannot be a distribution column', v_col, pg_catalog.format_type(v_atttype, NULL))));
                    END IF;
                    FOR r IN SELECT ic.relname AS index_name, i.indkey::pg_catalog.int2[] AS cols
                               FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) i.indexrelid
                              WHERE i.indrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND i.indisunique ORDER BY ic.relname
                    LOOP
                        IF NOT (v_attnum OPERATOR(pg_catalog.=) ANY (r.cols)) THEN
                            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                       'blocker', 'unique_without_distribution_column', r.index_name,
                                       pg_catalog.format('unique index "%s" does not include the new distribution column "%s"', r.index_name, v_col)));
                        END IF;
                    END LOOP;
                END IF;
            END IF;
            IF (options OPERATOR(pg_catalog.->>) 'bucket_count') IS NOT NULL AND (v_buckets < 1 OR v_buckets > 4096) THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'bucket_count_invalid', v_buckets::pg_catalog.text, 'bucket_count must be between 1 and 4096'));
            END IF;
            IF v_cwith IS NOT NULL AND NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_cwith AND d.method OPERATOR(pg_catalog.=) 'hash') THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'colocate_with_not_distributed', v_cwith::pg_catalog.text,
                           pg_catalog.format('table "%s" is not a distributed table', v_cwith)));
            END IF;
            IF COALESCE((options OPERATOR(pg_catalog.->>) 'cascade_to_colocated')::pg_catalog.bool, false) IS NOT TRUE THEN
                FOR r IN SELECT k.conname, k.conrelid, k.confrelid FROM pg_catalog.pg_constraint k
                          WHERE k.contype OPERATOR(pg_catalog.=) 'f' AND k.conparentid OPERATOR(pg_catalog.=) 0
                            AND k.conrelid OPERATOR(pg_catalog.<>) k.confrelid
                            AND (k.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid OR k.confrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid)
                            AND EXISTS (SELECT 1 FROM kingres.dist_table d
                                         WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=)
                                               CASE WHEN k.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid THEN k.confrelid ELSE k.conrelid END
                                           AND d.method OPERATOR(pg_catalog.=) 'hash' AND d.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id)
                          ORDER BY k.conname
                LOOP
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'foreign_key_colocation', r.conname,
                               pg_catalog.format('foreign key "%s" depends on the colocation of "%s" and "%s"; pass cascade_to_colocated => true or drop the key',
                                                 r.conname, r.conrelid::pg_catalog.regclass, r.confrelid::pg_catalog.regclass)));
                END LOOP;
            END IF;
            IF EXISTS (SELECT 1 FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id) THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'group_busy', v_dt.colocation_id::pg_catalog.text,
                           pg_catalog.format('colocation group %s is busy: a table operation uses it', v_dt.colocation_id)));
            ELSIF EXISTS (SELECT 1 FROM kingres.bucket_move m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id
                             AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching')) THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'group_busy', v_dt.colocation_id::pg_catalog.text,
                           pg_catalog.format('colocation group %s is busy: a bucket is being moved', v_dt.colocation_id)));
            END IF;
        END IF;

    ELSE
        -- create_partition, attach_partition, detach_partition, drop_partition
        IF NOT is_dist OR v_dt.method OPERATOR(pg_catalog.<>) 'hash' THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'not_distributed', qname, pg_catalog.format('table "%s" is not a distributed table', rname)));
        END IF;
        IF options OPERATOR(pg_catalog.->>) 'partition' IS NOT NULL
           AND pg_catalog.octet_length(options OPERATOR(pg_catalog.->>) 'partition') OPERATOR(pg_catalog.+) 6 OPERATOR(pg_catalog.>) 63 THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'name_too_long', options OPERATOR(pg_catalog.->>) 'partition',
                       pg_catalog.format('partition name "%s" is too long: leaf names append 6 characters (_bNNNN)', options OPERATOR(pg_catalog.->>) 'partition')));
        END IF;
        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                   'blocker', 'not_available_yet', qname, 'user partitions of distributed tables are not available in this version'));
    END IF;

    RETURN QUERY SELECT e OPERATOR(pg_catalog.->>) 0, e OPERATOR(pg_catalog.->>) 1, e OPERATOR(pg_catalog.->>) 2, e OPERATOR(pg_catalog.->>) 3
                   FROM pg_catalog.jsonb_array_elements(rep) WITH ORDINALITY AS t(e, o) ORDER BY o;
END;
$$;

COMMENT ON FUNCTION kingres.table_change_report(text, regclass, jsonb) IS
    'What prevents a change of a table (blocker), what it would not carry (warning) and what is good to know (note); changes nothing';

-- Raise one error that lists every blocker of the report, or return.  A single
-- blocker is the error message itself; several are counted in the message and
-- listed in the detail.
CREATE FUNCTION kingres.internal_raise_blockers(p_kind text, p_relid regclass, p_options jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rows  pg_catalog.text[];
    v_codes pg_catalog.text[];
    v_n     pg_catalog.int4;
    v_state pg_catalog.text := 'feature_not_supported';
BEGIN
    SELECT pg_catalog.array_agg(t.detail ORDER BY t.o), pg_catalog.array_agg(t.code ORDER BY t.o)
      INTO v_rows, v_codes
      FROM (SELECT r.detail, r.code, pg_catalog.row_number() OVER () AS o
              FROM kingres.table_change_report(p_kind, p_relid, p_options) r
             WHERE r.severity OPERATOR(pg_catalog.=) 'blocker') t;
    v_n := COALESCE(pg_catalog.cardinality(v_rows), 0);
    IF v_n OPERATOR(pg_catalog.=) 0 THEN
        RETURN;
    END IF;
    IF v_n OPERATOR(pg_catalog.=) 1 THEN
        v_state := CASE v_codes[1]
                       WHEN 'already_distributed' THEN 'duplicate_object'
                       WHEN 'not_ordinary_table' THEN 'wrong_object_type'
                       WHEN 'column_missing' THEN 'undefined_column'
                       WHEN 'unique_without_distribution_column' THEN 'invalid_table_definition'
                       WHEN 'name_too_long' THEN 'invalid_name'
                       WHEN 'not_on_map_group' THEN 'object_not_in_prerequisite_state'
                       WHEN 'group_busy' THEN 'object_in_use'
                       ELSE 'feature_not_supported' END;
        RAISE EXCEPTION '%', v_rows[1] USING ERRCODE = v_state;
    END IF;
    RAISE EXCEPTION '% problems prevent the change of table "%"', v_n, p_relid
        USING ERRCODE = 'feature_not_supported',
              DETAIL = (SELECT pg_catalog.string_agg(pg_catalog.format('%s: %s', v_codes[i], v_rows[i]), E'
' ORDER BY i)
                          FROM pg_catalog.generate_subscripts(v_rows, 1) AS i),
              HINT = 'kingres.table_change_report() lists them with the warnings and notes.';
END;
$$;

-- The checks of create_distributed_table() and create_reference_table() are the
-- blockers of the report.  They keep their names; the first returns the type and
-- the collation of the distribution column.
DROP FUNCTION kingres.internal_check_distributable(regclass, name);
CREATE FUNCTION kingres.internal_check_distributable(p_relid regclass, p_column name,
                                                     p_colocate_with regclass DEFAULT NULL,
                                                     p_bucket_count int DEFAULT NULL)
    RETURNS TABLE (column_type pg_catalog.oid, column_collation pg_catalog.oid)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    PERFORM kingres.internal_raise_blockers('distribute', p_relid,
        pg_catalog.jsonb_strip_nulls(pg_catalog.jsonb_build_object('column', p_column,
            'colocate_with', p_colocate_with::pg_catalog.oid::pg_catalog.text, 'bucket_count', p_bucket_count)));
    RETURN QUERY SELECT a.atttypid, a.attcollation FROM pg_catalog.pg_attribute a
                  WHERE a.attrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) p_column
                    AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped;
END;
$$;

CREATE OR REPLACE FUNCTION kingres.internal_check_referenceable(p_relid regclass)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    PERFORM kingres.internal_raise_blockers('distribute_reference', p_relid, '{}'::pg_catalog.jsonb);
END;
$$;

COMMENT ON FUNCTION kingres.create_distributed_table(regclass, name, regclass, int) IS
    'Turn a table, empty or not, into a hash-partitioned distributed table (map group primary only)';

REVOKE ALL ON FUNCTION
    kingres.table_change_report(text, regclass, jsonb),
    kingres.internal_raise_blockers(text, regclass, jsonb),
    kingres.internal_require_version(text),
    kingres.internal_has_hash_support(regtype),
    kingres.internal_advance_sequence(regclass, regclass, name, bigint, bool),
    kingres.internal_rename_identity(regclass, name, name),
    kingres.internal_table_ddl(regclass, name, bool),
    kingres.internal_check_distributable(regclass, name, regclass, int),
    kingres.internal_copy_routed(regclass, name, name, text, name, int, int)
    FROM PUBLIC;
