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

-- The ACL of a new table is the one of the old table and nothing else: the default privileges of the schema
-- (ALTER DEFAULT PRIVILEGES) gave the new table grants that the old one never had.  Every grant is taken back and
-- the owner gets all of them again only when the old table had the default ACL (NULL); otherwise the carried grants, the owner's own among them, follow.
CREATE FUNCTION kingres.internal_reset_acl(p_table regclass, p_default bool)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r pg_catalog.record;
    v_owner pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT ro.rolname INTO v_owner FROM pg_catalog.pg_class c JOIN pg_catalog.pg_roles ro ON ro.oid OPERATOR(pg_catalog.=) c.relowner
     WHERE c.oid OPERATOR(pg_catalog.=) p_table::pg_catalog.oid;
    -- the owner first: on a table with the default ACL (NULL) this makes the entry of the owner explicit and takes it away
    EXECUTE pg_catalog.format('REVOKE ALL ON TABLE %s FROM %I', p_table, v_owner);
    FOR r IN SELECT DISTINCT e.grantee FROM pg_catalog.pg_class c, pg_catalog.aclexplode(c.relacl) e
              WHERE c.oid OPERATOR(pg_catalog.=) p_table::pg_catalog.oid
                AND e.grantee OPERATOR(pg_catalog.<>) c.relowner
    LOOP
        EXECUTE pg_catalog.format('REVOKE ALL ON TABLE %s FROM %s', p_table,
                    CASE WHEN r.grantee OPERATOR(pg_catalog.=) 0 THEN 'PUBLIC'
                         ELSE pg_catalog.quote_ident((SELECT ro.rolname FROM pg_catalog.pg_roles ro WHERE ro.oid OPERATOR(pg_catalog.=) r.grantee)) END);
    END LOOP;
    IF p_default THEN
        EXECUTE pg_catalog.format('GRANT ALL ON TABLE %s TO %I', p_table, v_owner);
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
        n := n + 1; ord := n; scope := 'post';
        stmt := format('SELECT kingres.internal_reset_acl(%L::pg_catalog.regclass, %L::pg_catalog.bool)', qname, c.relacl IS NULL);
        RETURN NEXT;
        -- table privileges: the owner's own entries first, so that the entries other
        -- grantors made find their grant option
        IF c.relacl IS NOT NULL THEN
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
        IF c.relrowsecurity OR c.relforcerowsecurity
           OR EXISTS (SELECT 1 FROM pg_catalog.pg_policy p WHERE p.polrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid) THEN
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

        -- rules on the table are neither carried nor safe to drop silently (an audit rule, an INSTEAD NOTHING rule)
        FOR r IN SELECT rw.rulename FROM pg_catalog.pg_rewrite rw
                  WHERE rw.ev_class OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND rw.rulename OPERATOR(pg_catalog.<>) '_RETURN'
                  ORDER BY rw.rulename
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'rule', r.rulename,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('rule "%s" is not carried to the new table and would be dropped; drop it or turn it into a trigger-free statement of the application', r.rulename)));
        END LOOP;
        -- an index that is not valid cannot be carried as it is: it would be built and made valid
        FOR r IN SELECT ic.relname FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) i.indexrelid
                  WHERE i.indrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND (NOT i.indisvalid OR NOT i.indisready)
                  ORDER BY ic.relname
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'invalid_index', r.relname,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('index "%s" is not valid; drop it or rebuild it first', r.relname)));
        END LOOP;
        -- a CHECK that is not valid: the rows that violate it would stop the copy
        FOR r IN SELECT k.conname FROM pg_catalog.pg_constraint k
                  WHERE k.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND k.contype OPERATOR(pg_catalog.=) 'c' AND NOT k.convalidated
                  ORDER BY k.conname
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'check_not_valid', r.conname,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('check constraint "%s" is not validated; validate it or drop it first', r.conname)));
        END LOOP;
        -- the row type of the table is a column of another table
        FOR r IN SELECT DISTINCT a.attrelid::pg_catalog.regclass::pg_catalog.text AS name, a.attname
                   FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class cl ON cl.oid OPERATOR(pg_catalog.=) a.attrelid
                  WHERE a.attrelid OPERATOR(pg_catalog.<>) relation::pg_catalog.oid AND NOT a.attisdropped AND a.attnum OPERATOR(pg_catalog.>) 0
                    AND cl.relkind IN ('r', 'p', 'm', 'c')
                    AND a.atttypid IN (c.reltype, (SELECT t.typarray FROM pg_catalog.pg_type t WHERE t.oid OPERATOR(pg_catalog.=) c.reltype))
                  ORDER BY 1, 2
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'dependent_object', r.name,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('column "%s" of %s has the row type of the table', r.attname, r.name)));
        END LOOP;
        -- a default that calls nextval() of a sequence that no column of the table owns
        FOR r IN SELECT DISTINCT s.oid::pg_catalog.regclass::pg_catalog.text AS name
                   FROM pg_catalog.pg_attrdef ad
                   JOIN pg_catalog.pg_depend d ON d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_attrdef'::pg_catalog.regclass AND d.objid OPERATOR(pg_catalog.=) ad.oid
                                              AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
                   JOIN pg_catalog.pg_class s ON s.oid OPERATOR(pg_catalog.=) d.refobjid AND s.relkind OPERATOR(pg_catalog.=) 'S'
                  WHERE ad.adrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid
                    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend o
                                     WHERE o.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass AND o.objid OPERATOR(pg_catalog.=) s.oid
                                       AND o.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
                                       AND o.refobjid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND o.deptype IN ('a', 'i'))
                  ORDER BY 1
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'default_sequence_not_owned', r.name,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('a column default calls nextval() of sequence %s, which no column of the table owns; the other nodes do not have it', r.name)));
        END LOOP;
        -- a distribution column whose hash is not the same on every node: an enum value hashes by its OID
        IF kind OPERATOR(pg_catalog.=) 'distribute' AND v_col IS NOT NULL
           AND EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid OPERATOR(pg_catalog.=) a.atttypid
                        WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col AND NOT a.attisdropped
                          AND (t.typtype OPERATOR(pg_catalog.=) 'e' OR (t.typtype OPERATOR(pg_catalog.=) 'd' AND EXISTS (SELECT 1 FROM pg_catalog.pg_type bt WHERE bt.oid OPERATOR(pg_catalog.=) t.typbasetype AND bt.typtype OPERATOR(pg_catalog.=) 'e'))
                               OR t.typcategory OPERATOR(pg_catalog.=) 'A' AND EXISTS (SELECT 1 FROM pg_catalog.pg_type et WHERE et.oid OPERATOR(pg_catalog.=) t.typelem AND et.typtype OPERATOR(pg_catalog.=) 'e'))) THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'enum_distribution_column', v_col::pg_catalog.text,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('column "%s" is an enum: the hash of an enum value depends on the OID of the value, which differs between the nodes', v_col)));
        END IF;
        -- the functions that the copy runs (CHECK, defaults, generated columns, index expressions) must belong to a superuser:
        -- the call runs them as the caller on every node
        FOR r IN SELECT DISTINCT pr.oid::pg_catalog.regprocedure::pg_catalog.text AS name,
                        (SELECT ro.rolname FROM pg_catalog.pg_roles ro WHERE ro.oid OPERATOR(pg_catalog.=) pr.proowner) AS owner
                   FROM pg_catalog.pg_depend d
                   JOIN pg_catalog.pg_proc pr ON pr.oid OPERATOR(pg_catalog.=) d.refobjid
                  WHERE d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_proc'::pg_catalog.regclass
                    AND pr.oid OPERATOR(pg_catalog.>=) 16384
                    AND NOT (SELECT ro.rolsuper FROM pg_catalog.pg_roles ro WHERE ro.oid OPERATOR(pg_catalog.=) pr.proowner)
                    AND ((d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_attrdef'::pg_catalog.regclass
                          AND d.objid IN (SELECT ad.oid FROM pg_catalog.pg_attrdef ad WHERE ad.adrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid))
                         OR (d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_constraint'::pg_catalog.regclass
                             AND d.objid IN (SELECT k.oid FROM pg_catalog.pg_constraint k WHERE k.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid))
                         OR (d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
                             AND d.objid IN (SELECT i.indexrelid FROM pg_catalog.pg_index i WHERE i.indrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid)))
                  ORDER BY 1
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'function_owner_not_superuser', r.name,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('function %s, used by a constraint, default or index of the table, belongs to the role "%s", who is not a superuser; the copy would run it with the rights of the caller on every node', r.name, r.owner)));
        END LOOP;
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
                -- the collations of the columns that do not belong to pg_catalog
                SELECT pg_catalog.string_agg(DISTINCT pg_catalog.format('%I.%I', cn.nspname, co.collname), ',') INTO x
                  FROM pg_catalog.pg_attribute a
                  JOIN pg_catalog.pg_collation co ON co.oid OPERATOR(pg_catalog.=) a.attcollation
                  JOIN pg_catalog.pg_namespace cn ON cn.oid OPERATOR(pg_catalog.=) co.collnamespace
                 WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND NOT a.attisdropped AND a.attnum OPERATOR(pg_catalog.>) 0
                   AND cn.nspname OPERATOR(pg_catalog.<>) 'pg_catalog';
                IF x IS NOT NULL THEN
                    probe := kingres.internal_exec_scalar(nd.node_id,
                        pg_catalog.format('SELECT pg_catalog.string_agg(q, '','' ORDER BY q) FROM pg_catalog.unnest(pg_catalog.string_to_array(%L, '','')) q WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_collation co JOIN pg_catalog.pg_namespace cn ON cn.oid OPERATOR(pg_catalog.=) co.collnamespace WHERE pg_catalog.format(''%%I.%%I'', cn.nspname, co.collname) OPERATOR(pg_catalog.=) q)', x),
                        (SELECT s.setting::pg_catalog.int4 OPERATOR(pg_catalog.*) 2000 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout'));
                    IF probe IS NOT NULL THEN
                        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                   'blocker', 'collation_missing', probe,
                                   pfx OPERATOR(pg_catalog.||) pg_catalog.format('collation(s) %s do not exist on node %s; collations are not synchronized between the nodes', probe, nd.node_id)));
                    END IF;
                END IF;
                -- an owner who is not a superuser needs CREATE on the schema on every node (the new table is handed to him there)
                IF NOT (SELECT ro.rolsuper FROM pg_catalog.pg_roles ro WHERE ro.oid OPERATOR(pg_catalog.=) c.relowner)
                   AND pg_catalog.has_schema_privilege(c.relowner, c.relnamespace, 'CREATE') THEN
                    probe := kingres.internal_exec_scalar(nd.node_id,
                        pg_catalog.format('SELECT CASE WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) %L) AND EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname OPERATOR(pg_catalog.=) %L) THEN pg_catalog.has_schema_privilege(%L, %L, ''CREATE'')::pg_catalog.text ELSE ''f'' END',
                                          (SELECT ro.rolname FROM pg_catalog.pg_roles ro WHERE ro.oid OPERATOR(pg_catalog.=) c.relowner), nsp,
                                          (SELECT ro.rolname FROM pg_catalog.pg_roles ro WHERE ro.oid OPERATOR(pg_catalog.=) c.relowner), nsp),
                        (SELECT s.setting::pg_catalog.int4 OPERATOR(pg_catalog.*) 2000 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout'));
                    IF probe OPERATOR(pg_catalog.=) 'false' OR probe OPERATOR(pg_catalog.=) 'f' THEN
                        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                   'blocker', 'schema_privilege', nsp,
                                   pfx OPERATOR(pg_catalog.||) pg_catalog.format('the owner of the table does not have CREATE on schema "%s" on node %s (schemas and their privileges are not synchronized between the nodes)', nsp, nd.node_id)));
                    END IF;
                END IF;
                -- the names of the new objects must be free on the node
                IF kind OPERATOR(pg_catalog.=) 'distribute' THEN
                    probe := kingres.internal_exec_scalar(nd.node_id,
                        pg_catalog.format('SELECT pg_catalog.string_agg(cl.relname, '','' ORDER BY cl.relname) FROM pg_catalog.pg_class cl JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) cl.relnamespace WHERE ns.nspname OPERATOR(pg_catalog.=) %L AND (cl.relname OPERATOR(pg_catalog.=) %L OR cl.relname LIKE %L OR (pg_catalog.length(cl.relname) OPERATOR(pg_catalog.=) %s AND pg_catalog.left(cl.relname, %s) OPERATOR(pg_catalog.=) %L AND pg_catalog.right(cl.relname, 4) OPERATOR(pg_catalog.~) ''^[0-9]{4}$''))',
                                          nsp, pg_catalog.format('kingres_shadow_%s', relation::pg_catalog.oid),
                                          pg_catalog.format('kingres\_shadow\_%s\_%%', relation::pg_catalog.oid),
                                          pg_catalog.length(rname) OPERATOR(pg_catalog.+) 6, pg_catalog.length(rname) OPERATOR(pg_catalog.+) 2, rname OPERATOR(pg_catalog.||) '_b'),
                        (SELECT s.setting::pg_catalog.int4 OPERATOR(pg_catalog.*) 2000 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout'));
                    IF probe IS NOT NULL THEN
                        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                   'blocker', 'name_in_use', probe,
                                   pfx OPERATOR(pg_catalog.||) pg_catalog.format('relation(s) %s exist on node %s; their names are needed for the new table', probe, nd.node_id)));
                    END IF;
                END IF;
            EXCEPTION WHEN OTHERS OR query_canceled THEN
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
        FOR r IN SELECT a.attname FROM pg_catalog.pg_attribute a
                  WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
                    AND EXISTS (SELECT 1 FROM pg_catalog.unnest(a.attoptions) o WHERE o NOT LIKE 'pg_attver=%') ORDER BY a.attnum
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'warning', 'not_carried', r.attname::pg_catalog.text,
                       pg_catalog.format('the attribute options of column "%s" are not carried to the new table', r.attname)));
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
    kingres.internal_reset_acl(regclass, bool),
    kingres.internal_table_ddl(regclass, name, bool),
    kingres.internal_check_distributable(regclass, name, regclass, int),
    kingres.internal_copy_routed(regclass, name, name, text, name, int, int)
    FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- The rebuild engine of the online table operations (design/shard-phase9.md,
-- "The rebuild engine"), increment 9.3
-- ---------------------------------------------------------------------------

-- The mark of a live operation: the session that runs it holds a lock that nothing
-- else takes; whoever finds it held leaves the operation alone.
CREATE FUNCTION kingres.internal_op_lock(p_op bigint)
    RETURNS bool
    AS 'MODULE_PATHNAME', 'kingres_internal_op_lock'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_op_unlock(p_op bigint)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_op_unlock'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_op_alive(p_op bigint)
    RETURNS bool
    AS 'MODULE_PATHNAME', 'kingres_internal_op_alive'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

-- The functions that commit in sessions of their own cannot run in a transaction block.
CREATE FUNCTION kingres.internal_prevent_xact_block(p_what text)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_prevent_xact_block'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

-- The colocation group of a new table and its bucket count; a new group has its catalog
-- rows made in the calling transaction.
CREATE FUNCTION kingres.internal_choose_colocation(p_type regtype, p_collation oid,
                                                   p_colocate_with regclass DEFAULT NULL,
                                                   p_buckets int DEFAULT NULL)
    RETURNS jsonb
    AS 'MODULE_PATHNAME', 'kingres_internal_choose_colocation'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

-- The statements that create the shadow of a table on the node of a group.
CREATE FUNCTION kingres.internal_shadow_script(p_relid regclass, p_column name, p_colocation int, p_group int)
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_internal_shadow_script'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

-- The swap of the switch (see rebuild.c); returns the retained old copy.
CREATE FUNCTION kingres.internal_swap_table(p_op bigint, p_relid regclass, p_column name,
                                            p_colocation int, p_cleanup_delay int)
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_internal_swap_table'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

-- A statement on another node in the remote branch of the current transaction (the replay deletes keys
-- in the same transaction that writes their rows).
CREATE FUNCTION kingres.internal_branch_exec(p_node int, p_sql text)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_branch_exec'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

-- One chunk of the copy or of the replay (see copy_routed.c).
CREATE FUNCTION kingres.internal_copy_chunk(p_source regclass, p_dest_schema name, p_dest_table name,
                                            p_dest_kind text, p_key_column name, p_colocation int,
                                            p_node int, p_order_cols text[], p_after text[],
                                            p_batch bigint, p_where text, p_conflict_nothing bool)
    RETURNS jsonb
    AS 'MODULE_PATHNAME', 'kingres_internal_copy_chunk'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

-- ---- capture ----

-- The trigger function of the capture (design, "Capture"): it writes the key of the old and of the
-- new row of the source into the log table, in the transaction of the writer.  It is SECURITY
-- DEFINER because the writer has no privilege on the log table; it inserts key columns of the row
-- that is being changed and nothing else, takes the names of the log and of the columns from the
-- arguments of the trigger (made by the extension; EXECUTE is revoked from PUBLIC, so no one else
-- can make a trigger of it) and refuses a log that is not one of the extension's.
CREATE FUNCTION kingres.internal_capture_trigger()
    RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_log  pg_catalog.text := TG_ARGV[0];
    v_cols pg_catalog.text := TG_ARGV[1];
    v_new  pg_catalog.text := TG_ARGV[2];
    v_old  pg_catalog.text;
BEGIN
    IF v_log !~ '^kingres\.tlog_[0-9]+_[0-9]+$' THEN
        RAISE EXCEPTION 'the capture trigger of table "%" has no log table of table operations', TG_TABLE_NAME
            USING ERRCODE = 'internal_error';
    END IF;
    IF TG_OP = 'INSERT' THEN
        EXECUTE pg_catalog.format('INSERT INTO %s (op, %s) SELECT ''I'', %s', v_log, v_cols, v_new)
            USING pg_catalog.to_jsonb(NEW);
    ELSIF TG_OP = 'DELETE' THEN
        EXECUTE pg_catalog.format('INSERT INTO %s (op, %s) SELECT ''D'', %s', v_log, v_cols, v_new)
            USING pg_catalog.to_jsonb(OLD);
    ELSE
        v_old := pg_catalog.replace(v_new, '$1', '$2');
        EXECUTE pg_catalog.format('INSERT INTO %s (op, %s) SELECT ''U'', %s UNION ALL SELECT ''D'', %s WHERE (%s) IS DISTINCT FROM (%s)',
                                  v_log, v_cols, v_new, v_old, v_old, v_new)
            USING pg_catalog.to_jsonb(NEW), pg_catalog.to_jsonb(OLD);
    END IF;
    RETURN NULL;
END;
$$;

-- A replay cannot follow a TRUNCATE (it reads the current row of a key), so it is refused.
CREATE FUNCTION kingres.internal_capture_truncate()
    RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION 'table "%" is being rebuilt; TRUNCATE is not allowed until it finishes', TG_TABLE_NAME
        USING ERRCODE = 'object_not_in_prerequisite_state',
              HINT = 'Wait for the table operation to finish, or cancel it with kingres.cancel_table_operation().';
END;
$$;

-- ---- small helpers ----

-- A setting as a number in its own unit (current_setting() answers with the unit, "10s").
CREATE FUNCTION kingres.internal_setting(p_name text)
    RETURNS bigint
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT s.setting::pg_catalog.int8 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) p_name;
$$;

-- The sessions that hold a lock on a table, for the text of a refusal.
CREATE FUNCTION kingres.internal_lock_holders(p_rel regclass)
    RETURNS text
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.string_agg(s.h, '; ' ORDER BY s.h)
  FROM (SELECT pg_catalog.format('pid %s (%s, transaction since %s, %s)', a.pid, a.usename,
                                 a.xact_start, pg_catalog.left(a.query, 80)) AS h
          FROM (SELECT DISTINCT l.pid FROM pg_catalog.pg_locks l
                 WHERE l.relation OPERATOR(pg_catalog.=) p_rel::pg_catalog.oid AND l.granted AND l.pid IS NOT NULL
                   AND l.pid OPERATOR(pg_catalog.<>) pg_catalog.pg_backend_pid()) x
          JOIN pg_catalog.pg_stat_activity a ON a.pid OPERATOR(pg_catalog.=) x.pid
        UNION ALL
        SELECT pg_catalog.format('prepared transaction "%s" (%s, prepared %s)', p.gid, p.owner, p.prepared)
          FROM pg_catalog.pg_prepared_xacts p
         WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_locks l
                        WHERE l.relation OPERATOR(pg_catalog.=) p_rel::pg_catalog.oid AND l.granted AND l.pid IS NULL
                          AND l.virtualtransaction IN (SELECT t.virtualtransaction FROM pg_catalog.pg_locks t
                                                        WHERE t.locktype OPERATOR(pg_catalog.=) 'transactionid'
                                                          AND t.transactionid OPERATOR(pg_catalog.=) p.transaction))) s;
$$;

-- A lock on a user table by the rule of the design ("Locks"): try without waiting for a while, then
-- queue once for at most kingres_shard.rebuild_lock_timeout.  A refusal names the holders.
CREATE FUNCTION kingres.internal_lock_table(p_rel regclass, p_mode text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_end  pg_catalog.timestamptz;
    v_hold pg_catalog.text;
BEGIN
    v_end := pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+)
             pg_catalog.make_interval(secs => (kingres.internal_setting('kingres_shard.rebuild_switch_timeout')::pg_catalog.int4 OPERATOR(pg_catalog./) 4) OPERATOR(pg_catalog./) 1000.0);
    LOOP
        BEGIN
            EXECUTE pg_catalog.format('LOCK TABLE %s IN %s MODE NOWAIT', p_rel, p_mode);
            RETURN;
        EXCEPTION WHEN lock_not_available THEN
            NULL;
        END;
        EXIT WHEN pg_catalog.clock_timestamp() OPERATOR(pg_catalog.>=) v_end;
        PERFORM pg_catalog.pg_sleep(0.005 OPERATOR(pg_catalog.+) pg_catalog.random() OPERATOR(pg_catalog.*) 0.015);
    END LOOP;
    PERFORM pg_catalog.set_config('lock_timeout', pg_catalog.current_setting('kingres_shard.rebuild_lock_timeout'), true);
    BEGIN
        EXECUTE pg_catalog.format('LOCK TABLE %s IN %s MODE', p_rel, p_mode);
    EXCEPTION WHEN lock_not_available THEN
        v_hold := kingres.internal_lock_holders(p_rel);
        RAISE EXCEPTION 'could not lock table % in % mode', p_rel, p_mode
            USING ERRCODE = 'lock_not_available',
                  DETAIL = pg_catalog.format('Held by: %s', COALESCE(v_hold, 'a session that could not be read')),
                  HINT = 'The operation tries again; a long transaction on the table delays it.';
    END;
END;
$$;

-- The record of an operation: its phase and what it waits for.  Runs in a session of its own (through
-- internal_node_exec) so that it is committed at once.  A finished operation does not change, and an
-- operation that was asked to stop does not enter its switch.
CREATE FUNCTION kingres.internal_op_set(p_op bigint, p_phase text, p_detail text)
    RETURNS void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
UPDATE kingres.table_operation o
   SET phase = p_phase, detail = p_detail, updated_at = pg_catalog.clock_timestamp(),
       finished_at = CASE WHEN p_phase IN ('done', 'failed', 'cancelled') THEN pg_catalog.clock_timestamp() ELSE o.finished_at END
 WHERE o.op_id OPERATOR(pg_catalog.=) p_op
   AND o.phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching', 'cleaning')
   AND NOT (p_phase OPERATOR(pg_catalog.=) 'switching' AND o.cancel_requested);
$$;

-- What an operation decided or learned, merged into its spec.
CREATE FUNCTION kingres.internal_op_spec(p_op bigint, p_patch jsonb)
    RETURNS void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
UPDATE kingres.table_operation o
   SET spec = o.spec OPERATOR(pg_catalog.||) p_patch, updated_at = pg_catalog.clock_timestamp()
 WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
$$;

-- The row identity of the log: the columns of the primary key, or of the first unique index over
-- NOT NULL columns (no predicate, no expression).
CREATE FUNCTION kingres.internal_log_key(p_relid regclass)
    RETURNS TABLE (key_names text[], key_types text[])
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY k.ord),
       pg_catalog.array_agg(pg_catalog.format_type(a.atttypid, a.atttypmod) ORDER BY k.ord)
  FROM (SELECT i.indrelid, i.indkey
          FROM pg_catalog.pg_index i
         WHERE i.indrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND i.indisunique AND i.indpred IS NULL AND i.indisvalid
           AND NOT EXISTS (SELECT 1 FROM pg_catalog.unnest(i.indkey::pg_catalog.int2[]) kk(attno)
                            WHERE kk.attno OPERATOR(pg_catalog.=) 0
                               OR NOT (SELECT a2.attnotnull FROM pg_catalog.pg_attribute a2
                                        WHERE a2.attrelid OPERATOR(pg_catalog.=) i.indrelid AND a2.attnum OPERATOR(pg_catalog.=) kk.attno))
         ORDER BY i.indisprimary DESC, i.indexrelid LIMIT 1) x
  CROSS JOIN LATERAL pg_catalog.unnest(x.indkey::pg_catalog.int2[]) WITH ORDINALITY k(attno, ord)
  JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) x.indrelid AND a.attnum OPERATOR(pg_catalog.=) k.attno
 HAVING pg_catalog.count(*) OPERATOR(pg_catalog.>) 0;
$$;

-- ---- the record of an operation ----

-- Record an operation (phase preparing) and claim its colocation group, in a session of its own, under
-- the cluster lock: the checks of the report, the version handshake, the choice of the group (a new
-- group is made here and announced to the nodes), the log key, and what undoing needs to know.
-- Idempotent for an operation of the same request_id.
CREATE FUNCTION kingres.internal_begin_table_operation(p_kind text, p_relid regclass, p_options jsonb)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_opts    pg_catalog.jsonb := COALESCE(p_options, '{}'::pg_catalog.jsonb) OPERATOR(pg_catalog.||) '{"mode": "online"}'::pg_catalog.jsonb;
    v_req     pg_catalog.text := p_options OPERATOR(pg_catalog.->>) 'request_id';
    v_col     pg_catalog.name := p_options OPERATOR(pg_catalog.->>) 'column';
    v_cwith   pg_catalog.regclass := NULLIF(p_options OPERATOR(pg_catalog.->>) 'colocate_with', '')::pg_catalog.oid::pg_catalog.regclass;
    v_buckets pg_catalog.int4 := (p_options OPERATOR(pg_catalog.->>) 'bucket_count')::pg_catalog.int4;
    v_id      pg_catalog.int8;
    v_type    pg_catalog.oid;
    v_coll    pg_catalog.oid;
    v_ch      pg_catalog.jsonb;
    v_group   pg_catalog.int4;
    v_nb      pg_catalog.int4;
    v_created pg_catalog.bool := false;
    v_keys    pg_catalog.text[];
    v_ktypes  pg_catalog.text[];
    v_seqs    pg_catalog.text[];
    v_nsp     pg_catalog.text;
    v_rname   pg_catalog.text;
    v_other   pg_catalog.int8;
    v_ver     pg_catalog.text;
    v_total   pg_catalog.float8;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    IF p_kind NOT IN ('distribute', 'distribute_reference') THEN
        RAISE EXCEPTION 'table operation "%" is not available in this version', p_kind
            USING ERRCODE = 'feature_not_supported';
    END IF;
    IF v_req IS NOT NULL THEN
        SELECT o.op_id INTO v_id FROM kingres.table_operation o
         WHERE o.spec OPERATOR(pg_catalog.->>) 'request_id' OPERATOR(pg_catalog.=) v_req
           AND o.phase NOT IN ('failed', 'cancelled');
        IF FOUND THEN
            RETURN v_id;
        END IF;
    END IF;

    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    SELECT i.op_id INTO v_other FROM kingres.table_operation_item i
      JOIN kingres.table_operation o ON o.op_id OPERATOR(pg_catalog.=) i.op_id
     WHERE i.relid OPERATOR(pg_catalog.=) p_relid AND o.phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching');
    IF FOUND THEN
        RAISE EXCEPTION 'table "%" is already being rebuilt by table operation %', p_relid, v_other
            USING ERRCODE = 'object_in_use', HINT = 'See kingres.table_operations.';
    END IF;
    SELECT e.extversion INTO v_ver FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard';
    PERFORM kingres.internal_require_version(v_ver);
    PERFORM kingres.internal_raise_blockers(p_kind, p_relid, v_opts);

    SELECT n.nspname, c.relname INTO v_nsp, v_rname
      FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
     WHERE c.oid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid;
    SELECT k.key_names, k.key_types INTO v_keys, v_ktypes FROM kingres.internal_log_key(p_relid) k;
    IF v_keys IS NULL THEN
        RAISE EXCEPTION 'table "%" has no primary key or unique index over NOT NULL columns', p_relid
            USING ERRCODE = 'feature_not_supported',
                  HINT = 'Use the blocking variant, kingres.create_distributed_table().';
    END IF;
    SELECT pg_catalog.array_agg(pg_catalog.format('%I.%I', sn.nspname, s.relname) ORDER BY a.attnum) INTO v_seqs
      FROM pg_catalog.pg_depend d
      JOIN pg_catalog.pg_class s ON s.oid OPERATOR(pg_catalog.=) d.objid AND s.relkind OPERATOR(pg_catalog.=) 'S'
      JOIN pg_catalog.pg_namespace sn ON sn.oid OPERATOR(pg_catalog.=) s.relnamespace
      JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) d.refobjid AND a.attnum OPERATOR(pg_catalog.=) d.refobjsubid
     WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
       AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
       AND d.refobjid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND d.deptype OPERATOR(pg_catalog.=) 'a';

    IF p_kind OPERATOR(pg_catalog.=) 'distribute' THEN
        SELECT a.atttypid, a.attcollation INTO v_type, v_coll FROM pg_catalog.pg_attribute a
         WHERE a.attrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col
           AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped;
        IF NOT (v_col::pg_catalog.text OPERATOR(pg_catalog.=) ANY (v_keys)) THEN
            RAISE EXCEPTION 'the distribution column "%" is not part of the key of table "%"', v_col, p_relid
                USING ERRCODE = 'invalid_table_definition';
        END IF;
        v_ch := kingres.internal_choose_colocation(v_type::pg_catalog.regtype, v_coll, v_cwith, v_buckets);
        v_group := (v_ch OPERATOR(pg_catalog.->>) 'colocation_id')::pg_catalog.int4;
        v_nb := (v_ch OPERATOR(pg_catalog.->>) 'bucket_count')::pg_catalog.int4;
        v_created := (v_ch OPERATOR(pg_catalog.->>) 'created')::pg_catalog.bool;

        -- the group must be free of other operations and of moves
        SELECT l.op_id INTO v_other FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) v_group;
        IF FOUND THEN
            RAISE EXCEPTION 'colocation group % is busy: table operation % uses it', v_group, v_other
                USING ERRCODE = 'object_in_use';
        END IF;
        IF EXISTS (SELECT 1 FROM kingres.bucket_move m
                    WHERE m.colocation_id OPERATOR(pg_catalog.=) v_group
                      AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching'))
           OR EXISTS (SELECT 1 FROM kingres.bucket_map b
                       WHERE b.colocation_id OPERATOR(pg_catalog.=) v_group AND b.state OPERATOR(pg_catalog.<>) 'active') THEN
            RAISE EXCEPTION 'colocation group % is busy: a bucket of it is being moved', v_group
                USING ERRCODE = 'object_in_use';
        END IF;
    END IF;

    v_total := (SELECT c.reltuples FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid);
    INSERT INTO kingres.table_operation (kind, mode, phase, spec, colocation_id, started_at, updated_at)
    VALUES (p_kind, 'online', 'preparing',
            pg_catalog.jsonb_build_object(
                'relid', p_relid::pg_catalog.oid, 'schema', v_nsp, 'table', v_rname,
                'shadow', pg_catalog.format('kingres_shadow_%s', p_relid::pg_catalog.oid),
                'column', v_col, 'colocation_id', v_group, 'bucket_count', v_nb, 'new_group', v_created,
                'dest_kind', CASE WHEN p_kind OPERATOR(pg_catalog.=) 'distribute' THEN 'placement' ELSE 'every_node' END,
                'keys', pg_catalog.to_jsonb(v_keys), 'key_types', pg_catalog.to_jsonb(v_ktypes),
                'sequences', pg_catalog.to_jsonb(COALESCE(v_seqs, ARRAY[]::pg_catalog.text[])),
                'request_id', v_req, 'last', NULL::pg_catalog.jsonb, 'validate', '[]'::pg_catalog.jsonb),
            v_group, pg_catalog.clock_timestamp(), pg_catalog.clock_timestamp())
    RETURNING op_id INTO v_id;
    UPDATE kingres.table_operation o
       SET spec = o.spec OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object('log', pg_catalog.format('kingres.tlog_%s_1', v_id))
     WHERE o.op_id OPERATOR(pg_catalog.=) v_id;
    INSERT INTO kingres.table_operation_item (op_id, ordinal, relid, shadow_name, rows_total)
    VALUES (v_id, 1, p_relid, pg_catalog.format('kingres_shadow_%s', p_relid::pg_catalog.oid), v_total::pg_catalog.int8);
    IF v_group IS NOT NULL THEN
        INSERT INTO kingres.table_operation_lock (colocation_id, op_id) VALUES (v_group, v_id);
    END IF;
    IF v_created THEN
        -- the group is announced to every node (it holds no table yet)
        PERFORM kingres.sync_catalog();
    END IF;
    RETURN v_id;
END;
$$;

-- Start an online table operation: the checks of the report, the claim of the colocation group and the
-- record, committed at once.  The phases are run by table_operation_advance().
CREATE FUNCTION kingres.begin_table_operation(kind text, relation regclass, options jsonb DEFAULT '{}'::jsonb)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_kind  pg_catalog.text := kind;
    v_opts  pg_catalog.jsonb := COALESCE(options, '{}'::pg_catalog.jsonb);
    v_self  pg_catalog.int4;
    v_ms    pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_prevent_xact_block('kingres.begin_table_operation()');
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    IF v_kind NOT IN ('distribute', 'distribute_reference') THEN
        RAISE EXCEPTION 'table operation "%" is not available in this version', v_kind
            USING ERRCODE = 'feature_not_supported',
                  HINT = 'This version has distribute and distribute_reference.';
    END IF;
    -- the blockers of the report are refused here, in the session of the caller: nothing is recorded
    PERFORM kingres.internal_raise_blockers(v_kind, relation, v_opts OPERATOR(pg_catalog.||) '{"mode": "online"}'::pg_catalog.jsonb);
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') * 4000;
    RETURN kingres.internal_exec_scalar(v_self,
        pg_catalog.format('SELECT kingres.internal_begin_table_operation(%L, %L::pg_catalog.oid::pg_catalog.regclass, %L::pg_catalog.jsonb)',
                          v_kind, relation::pg_catalog.oid, v_opts::pg_catalog.text), v_ms)::pg_catalog.int8;
END;
$$;

-- ---- capture, copy and replay (each runs in a session of its own and commits at once) ----

-- The log table and the triggers on the source, under a lock that excludes writers while they
-- are made.  Every step checks what exists.
CREATE FUNCTION kingres.internal_op_capture(p_op bigint)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_spec  pg_catalog.jsonb;
    v_rel   pg_catalog.regclass;
    v_log   pg_catalog.text;
    v_keys  pg_catalog.text[];
    v_types pg_catalog.text[];
    v_cols  pg_catalog.text;
    v_exprs pg_catalog.text;
    v_defs  pg_catalog.text;
    v_trg   pg_catalog.text := pg_catalog.format('kingres_capture_%s', p_op);
    v_ntr   pg_catalog.text := pg_catalog.format('kingres_notrunc_%s', p_op);
    i       pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT o.spec INTO v_spec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    v_rel := (v_spec OPERATOR(pg_catalog.->>) 'relid')::pg_catalog.oid::pg_catalog.regclass;
    v_log := v_spec OPERATOR(pg_catalog.->>) 'log';
    SELECT pg_catalog.array_agg(x) INTO v_keys FROM pg_catalog.jsonb_array_elements_text(v_spec OPERATOR(pg_catalog.->) 'keys') x;
    SELECT pg_catalog.array_agg(x) INTO v_types FROM pg_catalog.jsonb_array_elements_text(v_spec OPERATOR(pg_catalog.->) 'key_types') x;
    PERFORM kingres.internal_lock_table(v_rel, 'SHARE ROW EXCLUSIVE');
    IF pg_catalog.to_regclass(v_log) IS NULL THEN
        v_defs := '';
        FOR i IN 1 .. pg_catalog.cardinality(v_keys) LOOP
            v_defs := v_defs OPERATOR(pg_catalog.||) pg_catalog.format(', k%s %s NOT NULL', i, v_types[i]);
        END LOOP;
        EXECUTE pg_catalog.format('CREATE TABLE %s (seq pg_catalog.int8 GENERATED ALWAYS AS IDENTITY PRIMARY KEY, op pg_catalog."char" NOT NULL%s)', v_log, v_defs);
        EXECUTE pg_catalog.format('REVOKE ALL ON %s FROM PUBLIC', v_log);
    END IF;
    v_cols := (SELECT pg_catalog.string_agg(pg_catalog.format('k%s', g), ', ' ORDER BY g) FROM pg_catalog.generate_series(1, pg_catalog.cardinality(v_keys)) g);
    v_exprs := (SELECT pg_catalog.string_agg(pg_catalog.format('($1 OPERATOR(pg_catalog.->>) %L)::%s', v_keys[g], v_types[g]), ', ' ORDER BY g)
                  FROM pg_catalog.generate_series(1, pg_catalog.cardinality(v_keys)) g);
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t WHERE t.tgrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid AND t.tgname OPERATOR(pg_catalog.=) v_trg) THEN
        EXECUTE pg_catalog.format('CREATE TRIGGER %I AFTER INSERT OR UPDATE OR DELETE ON %s FOR EACH ROW EXECUTE FUNCTION kingres.internal_capture_trigger(%L, %L, %L)',
                                  v_trg, v_rel, v_log, v_cols, v_exprs);
        EXECUTE pg_catalog.format('ALTER TABLE %s ENABLE ALWAYS TRIGGER %I', v_rel, v_trg);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t WHERE t.tgrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid AND t.tgname OPERATOR(pg_catalog.=) v_ntr) THEN
        EXECUTE pg_catalog.format('CREATE TRIGGER %I BEFORE TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION kingres.internal_capture_truncate()', v_ntr, v_rel);
        EXECUTE pg_catalog.format('ALTER TABLE %s ENABLE ALWAYS TRIGGER %I', v_rel, v_ntr);
    END IF;
END;
$$;

-- One chunk of the copy: the rows after the last key, to their places in the shadow.  The key of the last
-- row and the number of rows are recorded in the same transaction as the chunk.
CREATE FUNCTION kingres.internal_op_copy_step(p_op bigint)
    RETURNS jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_spec pg_catalog.jsonb;
    v_keys pg_catalog.text[];
    v_last pg_catalog.text[];
    v_res  pg_catalog.jsonb;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT o.spec INTO v_spec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    SELECT pg_catalog.array_agg(x) INTO v_keys FROM pg_catalog.jsonb_array_elements_text(v_spec OPERATOR(pg_catalog.->) 'keys') x;
    IF pg_catalog.jsonb_typeof(v_spec OPERATOR(pg_catalog.->) 'last') OPERATOR(pg_catalog.=) 'array' THEN
        SELECT pg_catalog.array_agg(x) INTO v_last FROM pg_catalog.jsonb_array_elements_text(v_spec OPERATOR(pg_catalog.->) 'last') x;
    END IF;
    PERFORM kingres.internal_fault_point('rebuild_copy_chunk');
    v_res := kingres.internal_copy_chunk((v_spec OPERATOR(pg_catalog.->>) 'relid')::pg_catalog.oid::pg_catalog.regclass,
                                         (v_spec OPERATOR(pg_catalog.->>) 'schema')::pg_catalog.name,
                                         (v_spec OPERATOR(pg_catalog.->>) 'shadow')::pg_catalog.name,
                                         v_spec OPERATOR(pg_catalog.->>) 'dest_kind',
                                         (v_spec OPERATOR(pg_catalog.->>) 'column')::pg_catalog.name,
                                         (v_spec OPERATOR(pg_catalog.->>) 'colocation_id')::pg_catalog.int4,
                                         NULL, v_keys, v_last,
                                         pg_catalog.current_setting('kingres_shard.rebuild_batch_rows')::pg_catalog.int8,
                                         NULL, true);
    UPDATE kingres.table_operation_item i
       SET rows_copied = i.rows_copied OPERATOR(pg_catalog.+) (v_res OPERATOR(pg_catalog.->>) 'read')::pg_catalog.int8
     WHERE i.op_id OPERATOR(pg_catalog.=) p_op AND i.ordinal OPERATOR(pg_catalog.=) 1;
    UPDATE kingres.table_operation o
       SET spec = o.spec OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object('last', v_res OPERATOR(pg_catalog.->) 'last'),
           updated_at = pg_catalog.clock_timestamp()
     WHERE o.op_id OPERATOR(pg_catalog.=) p_op AND pg_catalog.jsonb_typeof(v_res OPERATOR(pg_catalog.->) 'last') OPERATOR(pg_catalog.=) 'array';
    RETURN v_res;
END;
$$;

-- One chunk of the replay (design, "Replay"): for each key that the next entries of the log name,
-- the destination is made equal to the current row of the source: the key is deleted at the
-- destination (a group, or every node), then the row, if it exists, is inserted; then the entries
-- are removed.  The deletes at other nodes commit at once, the rest is in the caller's transaction;
-- a chunk that is repeated gives the same result.  Returns the number of entries it took.
CREATE FUNCTION kingres.internal_replay_chunk(p_op bigint, p_batch int)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_spec     pg_catalog.jsonb;
    v_log      pg_catalog.text;
    v_keys     pg_catalog.text[];
    v_types    pg_catalog.text[];
    v_col      pg_catalog.text;
    v_coloc    pg_catalog.int4;
    v_shadow   pg_catalog.text;
    v_nsp      pg_catalog.text;
    v_seqs     pg_catalog.int8[];
    v_n        pg_catalog.int4;
    v_self     pg_catalog.int4;
    v_ucols    pg_catalog.text;
    v_cond     pg_catalog.text;
    v_unnest   pg_catalog.text;
    v_aggs     pg_catalog.text;
    v_lcols    pg_catalog.text;
    v_kcols    pg_catalog.text;
    v_distidx  pg_catalog.int4;
    v_tmpl     pg_catalog.text;
    v_dest     pg_catalog.text;
    r          pg_catalog.record;
    i          pg_catalog.int4;
BEGIN
    SELECT o.spec INTO v_spec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    v_log := v_spec OPERATOR(pg_catalog.->>) 'log';
    EXECUTE pg_catalog.format('SELECT COALESCE(pg_catalog.array_agg(s.seq), ARRAY[]::pg_catalog.int8[]) FROM (SELECT l.seq FROM %s l ORDER BY l.seq LIMIT %s) s',
                              v_log, p_batch) INTO v_seqs;
    v_n := pg_catalog.cardinality(v_seqs);
    PERFORM kingres.internal_fault_point('rebuild_replay_chunk');
    IF v_n OPERATOR(pg_catalog.=) 0 THEN
        RETURN 0;
    END IF;
    SELECT pg_catalog.array_agg(x) INTO v_keys FROM pg_catalog.jsonb_array_elements_text(v_spec OPERATOR(pg_catalog.->) 'keys') x;
    SELECT pg_catalog.array_agg(x) INTO v_types FROM pg_catalog.jsonb_array_elements_text(v_spec OPERATOR(pg_catalog.->) 'key_types') x;
    v_col := v_spec OPERATOR(pg_catalog.->>) 'column';
    v_coloc := (v_spec OPERATOR(pg_catalog.->>) 'colocation_id')::pg_catalog.int4;
    v_nsp := v_spec OPERATOR(pg_catalog.->>) 'schema';
    v_shadow := pg_catalog.format('%I.%I', v_nsp, v_spec OPERATOR(pg_catalog.->>) 'shadow');
    SELECT l.node_id INTO v_self FROM kingres.local_node l;

    v_ucols := (SELECT pg_catalog.string_agg(pg_catalog.format('%I', v_keys[g]), ', ' ORDER BY g) FROM pg_catalog.generate_series(1, pg_catalog.cardinality(v_keys)) g);
    v_cond := (SELECT pg_catalog.string_agg(pg_catalog.format('d.%I OPERATOR(pg_catalog.=) u.%I', v_keys[g], v_keys[g]), ' AND ' ORDER BY g)
                 FROM pg_catalog.generate_series(1, pg_catalog.cardinality(v_keys)) g);
    -- inside the format of the template the placeholders are doubled once more
    v_unnest := (SELECT pg_catalog.string_agg(pg_catalog.format('%%L::%s[]', v_types[g]), ', ' ORDER BY g)
                   FROM pg_catalog.generate_series(1, pg_catalog.cardinality(v_keys)) g);
    v_aggs := (SELECT pg_catalog.string_agg(pg_catalog.format('pg_catalog.array_agg(k.k%s)', g), ', ' ORDER BY g)
                 FROM pg_catalog.generate_series(1, pg_catalog.cardinality(v_keys)) g);
    v_lcols := (SELECT pg_catalog.string_agg(pg_catalog.format('k%s', g), ', ' ORDER BY g) FROM pg_catalog.generate_series(1, pg_catalog.cardinality(v_keys)) g);
    v_kcols := v_ucols;
    v_tmpl := pg_catalog.format('DELETE FROM %s d USING unnest(%s) AS u(%s) WHERE %s',
                                v_shadow, v_unnest, v_ucols, v_cond);

    -- 1. the keys are removed from the destination
    IF v_col IS NOT NULL THEN
        v_distidx := pg_catalog.array_position(v_keys, v_col);
        FOR r IN EXECUTE pg_catalog.format(
                    'SELECT m.group_id AS grp, pg_catalog.format(%L, %s) AS stmt '
                    'FROM (SELECT DISTINCT %s FROM %s WHERE seq OPERATOR(pg_catalog.=) ANY (%L::pg_catalog.int8[])) k '
                    'JOIN kingres.bucket_map m ON m.colocation_id OPERATOR(pg_catalog.=) %s '
                    'AND m.bucket OPERATOR(pg_catalog.=) kingres.bucket_for(%s, k.k%s) GROUP BY m.group_id',
                    v_tmpl, v_aggs, v_lcols, v_log, v_seqs, v_coloc, v_coloc, v_distidx)
        LOOP
            IF (SELECT n.node_id FROM kingres.node n WHERE n.group_id OPERATOR(pg_catalog.=) r.grp AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active) OPERATOR(pg_catalog.=) v_self THEN
                EXECUTE r.stmt;
            ELSE
                PERFORM kingres.internal_branch_exec(kingres.internal_group_node(r.grp), r.stmt);
            END IF;
        END LOOP;
    ELSE
        EXECUTE pg_catalog.format(
                    'SELECT pg_catalog.format(%L, %s) FROM (SELECT DISTINCT %s FROM %s WHERE seq OPERATOR(pg_catalog.=) ANY (%L::pg_catalog.int8[])) k',
                    v_tmpl, v_aggs, v_lcols, v_log, v_seqs) INTO v_dest;
        FOR r IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
            IF r.node_id OPERATOR(pg_catalog.=) v_self THEN
                EXECUTE v_dest;
            ELSE
                PERFORM kingres.internal_branch_exec(r.node_id, v_dest);
            END IF;
        END LOOP;
    END IF;

    -- 2. the rows that exist now are inserted
    PERFORM kingres.internal_copy_chunk((v_spec OPERATOR(pg_catalog.->>) 'relid')::pg_catalog.oid::pg_catalog.regclass,
                                        v_nsp::pg_catalog.name, (v_spec OPERATOR(pg_catalog.->>) 'shadow')::pg_catalog.name,
                                        v_spec OPERATOR(pg_catalog.->>) 'dest_kind', v_col::pg_catalog.name, v_coloc,
                                        NULL, NULL, NULL, NULL,
                                        pg_catalog.format('(%s) IN (SELECT %s FROM %s WHERE seq OPERATOR(pg_catalog.=) ANY (%L::pg_catalog.int8[]))',
                                                          v_kcols, v_lcols, v_log, v_seqs),
                                        false);

    -- 3. the entries are done
    EXECUTE pg_catalog.format('DELETE FROM %s WHERE seq OPERATOR(pg_catalog.=) ANY (%L::pg_catalog.int8[])', v_log, v_seqs);
    RETURN v_n;
END;
$$;

-- ---- the switch ----

-- Put the old table out of the way, in the transaction of the swap (design, "Switch", step 4): the
-- capture triggers and the log table are dropped, the foreign keys are let go (the new table has
-- them; the retained copy must not hold up the tables it referred to), the indexes (with their
-- constraints) and the sequences of identity columns get names that carry the OID of the table,
-- and the table is renamed, moved to the schema kingres and made unreadable by PUBLIC.
CREATE FUNCTION kingres.internal_retire_table(p_relid regclass, p_op bigint)
    RETURNS regclass
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_oid  pg_catalog.oid := p_relid::pg_catalog.oid;
    v_nsp  pg_catalog.text;
    v_name pg_catalog.text;
    v_new  pg_catalog.text := pg_catalog.format('kingres_old_%s', p_relid::pg_catalog.oid);
    r      pg_catalog.record;
    k      pg_catalog.int4 := 0;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT n.nspname, c.relname INTO v_nsp, v_name
      FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
     WHERE c.oid OPERATOR(pg_catalog.=) v_oid;
    FOR r IN SELECT t.tgname FROM pg_catalog.pg_trigger t
              WHERE t.tgrelid OPERATOR(pg_catalog.=) v_oid
                AND (t.tgname LIKE 'kingres\_capture\_%' OR t.tgname LIKE 'kingres\_notrunc\_%')
    LOOP
        EXECUTE pg_catalog.format('DROP TRIGGER %I ON %s', r.tgname, p_relid);
    END LOOP;
    EXECUTE pg_catalog.format('DROP TABLE IF EXISTS kingres.tlog_%s_1', p_op);
    FOR r IN SELECT c.conname FROM pg_catalog.pg_constraint c
              WHERE c.conrelid OPERATOR(pg_catalog.=) v_oid AND c.contype OPERATOR(pg_catalog.=) 'f' AND c.conparentid OPERATOR(pg_catalog.=) 0
    LOOP
        EXECUTE pg_catalog.format('ALTER TABLE %s DROP CONSTRAINT %I', p_relid, r.conname);
    END LOOP;
    FOR r IN SELECT ic.relname FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) i.indexrelid
              WHERE i.indrelid OPERATOR(pg_catalog.=) v_oid ORDER BY i.indexrelid
    LOOP
        k := k OPERATOR(pg_catalog.+) 1;
        EXECUTE pg_catalog.format('ALTER INDEX %I.%I RENAME TO %I', v_nsp, r.relname, pg_catalog.format('kingres_old_%s_i%s', v_oid, k));
    END LOOP;
    FOR r IN SELECT a.attnum, pg_catalog.pg_get_serial_sequence(pg_catalog.format('%I.%I', v_nsp, v_name), a.attname::pg_catalog.text) AS seq
               FROM pg_catalog.pg_attribute a
              WHERE a.attrelid OPERATOR(pg_catalog.=) v_oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped AND a.attidentity OPERATOR(pg_catalog.<>) ''
    LOOP
        EXECUTE pg_catalog.format('ALTER SEQUENCE %s RENAME TO %I', r.seq, pg_catalog.format('kingres_old_%s_s%s', v_oid, r.attnum));
    END LOOP;
    EXECUTE pg_catalog.format('ALTER TABLE %I.%I RENAME TO %I', v_nsp, v_name, v_new);
    EXECUTE pg_catalog.format('ALTER TABLE %I.%I SET SCHEMA kingres', v_nsp, v_new);
    EXECUTE pg_catalog.format('REVOKE ALL ON kingres.%I FROM PUBLIC', v_new);
    -- (a rename does not change privileges: whoever could read the table could read the copy)
    FOR r IN SELECT DISTINCT x.grantee FROM pg_catalog.pg_class c, pg_catalog.aclexplode(c.relacl) x
              WHERE c.oid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND x.grantee OPERATOR(pg_catalog.<>) c.relowner AND x.grantee OPERATOR(pg_catalog.<>) 0
    LOOP
        EXECUTE pg_catalog.format('REVOKE ALL ON kingres.%I FROM %I', v_new, (SELECT ro.rolname FROM pg_catalog.pg_roles ro WHERE ro.oid OPERATOR(pg_catalog.=) r.grantee));
    END LOOP;
    RETURN pg_catalog.format('kingres.%I', v_new)::pg_catalog.regclass;
END;
$$;

-- One attempt of the switch (design, "Switch"), in a session of its own, in one transaction: the
-- cluster lock, the lock of the source (writers wait, readers go on), the replay of the rest of the
-- log, the proof that nothing is left, the lock that the rename needs, and the swap.  An attempt that
-- cannot get a lock in time, or whose replay takes too long, fails with lock_not_available and
-- leaves everything as it was.
CREATE FUNCTION kingres.internal_switch_attempt(p_op bigint)
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rec      kingres.table_operation%ROWTYPE;
    v_rel      pg_catalog.regclass;
    v_log      pg_catalog.text;
    v_col      pg_catalog.name;
    v_deadline pg_catalog.timestamptz;
    v_batch    pg_catalog.int4 := pg_catalog.current_setting('kingres_shard.rebuild_batch_rows')::pg_catalog.int4;
    v_n        pg_catalog.int4;
    v_cnt      pg_catalog.int8;
    v_valid    pg_catalog.text[];
    v_left     pg_catalog.int4;
    v_old      pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT * INTO v_rec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'table operation % does not exist', p_op USING ERRCODE = 'undefined_object';
    END IF;
    IF v_rec.phase OPERATOR(pg_catalog.<>) 'switching' THEN
        RAISE EXCEPTION 'table operation % is in phase "%", not switching', p_op, v_rec.phase
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF v_rec.cancel_requested THEN
        -- not query_canceled: a PL/pgSQL handler for OTHERS does not catch that one
        RAISE EXCEPTION 'table operation % was asked to stop', p_op USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    v_rel := (v_rec.spec OPERATOR(pg_catalog.->>) 'relid')::pg_catalog.oid::pg_catalog.regclass;
    v_log := v_rec.spec OPERATOR(pg_catalog.->>) 'log';
    v_col := v_rec.spec OPERATOR(pg_catalog.->>) 'column';
    v_deadline := pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+)
                  pg_catalog.make_interval(secs => kingres.internal_setting('kingres_shard.rebuild_switch_timeout')::pg_catalog.int4 OPERATOR(pg_catalog./) 1000.0);
    PERFORM kingres.internal_fault_point('rebuild_before_switch');

    -- the catalog lock first (the order of every catalog change), then the table
    PERFORM pg_catalog.set_config('lock_timeout', pg_catalog.current_setting('kingres_shard.rebuild_lock_timeout'), true);
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_lock_table(v_rel, 'EXCLUSIVE');
    PERFORM kingres.internal_fault_point('rebuild_switch_after_lock');

    -- the rest of the log, replayed after the lock was granted: no writer can add to it now
    LOOP
        v_n := kingres.internal_replay_chunk(p_op, v_batch);
        EXIT WHEN v_n OPERATOR(pg_catalog.=) 0;
        IF pg_catalog.clock_timestamp() OPERATOR(pg_catalog.>) v_deadline THEN
            EXECUTE pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', v_log) INTO v_left;
            RAISE EXCEPTION 'the final replay of table % did not finish in time: % entries are left', v_rel, v_left
                USING ERRCODE = 'lock_not_available',
                      HINT = 'The writers of the table wait meanwhile; the operation tries again.';
        END IF;
    END LOOP;
    EXECUTE pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', v_log) INTO v_cnt;
    IF v_cnt OPERATOR(pg_catalog.>) 0 THEN
        RAISE EXCEPTION 'the log of table % is not empty after the final replay (% entries)', v_rel, v_cnt
            USING ERRCODE = 'internal_error';
    END IF;
    PERFORM kingres.internal_fault_point('rebuild_switch_after_final_replay');

    PERFORM kingres.internal_lock_table(v_rel, 'ACCESS EXCLUSIVE');
    SELECT pg_catalog.array_agg(c.conname::pg_catalog.text ORDER BY c.conname) INTO v_valid
      FROM pg_catalog.pg_constraint c
     WHERE c.conrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid AND c.contype OPERATOR(pg_catalog.=) 'f'
       AND c.conparentid OPERATOR(pg_catalog.=) 0 AND c.convalidated;
    PERFORM kingres.internal_op_spec(p_op, pg_catalog.jsonb_build_object('validate', pg_catalog.to_jsonb(COALESCE(v_valid, ARRAY[]::pg_catalog.text[]))));
    -- what is left of the time bounds the waits of the swap
    PERFORM pg_catalog.set_config('lock_timeout',
        GREATEST(1, pg_catalog.floor(EXTRACT(epoch FROM (v_deadline OPERATOR(pg_catalog.-) pg_catalog.clock_timestamp())) OPERATOR(pg_catalog.*) 1000))::pg_catalog.int4::pg_catalog.text, true);
    v_old := kingres.internal_swap_table(p_op, v_rel, v_col, (v_rec.spec OPERATOR(pg_catalog.->>) 'colocation_id')::pg_catalog.int4,
                                         kingres.internal_setting('kingres_shard.table_cleanup_delay')::pg_catalog.int4);
    RETURN v_old;
END;
$$;

-- ---- verification and cleaning ----

-- The number of rows of a table (of this node's partitions, when it is a distributed table) and the
-- sum of a hash of every row in text form: the same on every node for equal rows, whatever their
-- order.  The text of a row does not depend on the settings of the session that asks.
CREATE FUNCTION kingres.internal_table_digest(p_rel regclass, p_xid xid8 DEFAULT NULL)
    RETURNS TABLE (n_rows bigint, n_hash numeric, n_newer bigint)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    SET DateStyle = 'ISO, YMD'
    SET IntervalStyle = 'postgres'
    SET extra_float_digits = 3
    SET bytea_output = 'hex'
    SET lc_monetary = 'C'
    SET TimeZone = 'UTC'
    SET kingres_shard.enable_routing = 'off'
    AS $$
BEGIN
    -- n_newer: the rows that a transaction after p_xid (the swap of the operation on this node) wrote
    RETURN QUERY EXECUTE pg_catalog.format('SELECT pg_catalog.count(*), COALESCE(pg_catalog.sum(pg_catalog.hashtextextended(t::pg_catalog.text, 0)::pg_catalog.numeric), 0), pg_catalog.count(*) FILTER (WHERE t.xmin::pg_catalog.text::pg_catalog.int8 OPERATOR(pg_catalog.>) %L::pg_catalog.int8) FROM %s t',
                                           COALESCE((p_xid::pg_catalog.text::pg_catalog.numeric OPERATOR(pg_catalog.%) 4294967296)::pg_catalog.int8, 9223372036854775807), p_rel);
END;
$$;

-- The cleaning phase (design, "Verification after the switch"): the old copy is compared with the new
-- table, the foreign keys that the switch added NOT VALID are validated (writers are admitted
-- meanwhile), the claim of the colocation group is released and the operation is done.  A difference
-- keeps the old copy and is reported by the answer 'failed: ...'.  The old copy itself is dropped by
-- cleanup_table_operations() when its time has come.
CREATE FUNCTION kingres.internal_op_clean(p_op bigint)
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rec    kingres.table_operation%ROWTYPE;
    v_item   kingres.table_operation_item%ROWTYPE;
    v_new    pg_catalog.text;
    v_old    pg_catalog.text;
    v_self   pg_catalog.int4;
    v_orows  pg_catalog.int8;
    v_ohash  pg_catalog.numeric;
    v_rows   pg_catalog.int8 := 0;
    v_hash   pg_catalog.numeric := 0;
    v_newer  pg_catalog.int8 := 0;
    v_r      pg_catalog.int8;
    v_h      pg_catalog.numeric;
    v_nw     pg_catalog.int8;
    v_xid    pg_catalog.text;
    v_res    pg_catalog.text;
    v_ref    pg_catalog.bool;
    v_ok     pg_catalog.bool := true;
    v_why    pg_catalog.text;
    v_names  pg_catalog.text[];
    nd       pg_catalog.record;
    nm       pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT * INTO v_rec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'table operation % does not exist', p_op USING ERRCODE = 'undefined_object';
    END IF;
    IF v_rec.phase OPERATOR(pg_catalog.=) 'done' THEN
        RETURN 'done';
    END IF;
    IF v_rec.phase OPERATOR(pg_catalog.<>) 'cleaning' THEN
        RAISE EXCEPTION 'table operation % is in phase "%", not cleaning', p_op, v_rec.phase
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    SELECT * INTO v_item FROM kingres.table_operation_item i WHERE i.op_id OPERATOR(pg_catalog.=) p_op AND i.ordinal OPERATOR(pg_catalog.=) 1;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_new := pg_catalog.format('%I.%I', v_rec.spec OPERATOR(pg_catalog.->>) 'schema', v_rec.spec OPERATOR(pg_catalog.->>) 'table');
    v_old := pg_catalog.format('kingres.%I', v_item.old_name);
    v_ref := v_rec.kind OPERATOR(pg_catalog.=) 'distribute_reference';
    PERFORM kingres.internal_fault_point('rebuild_verify');

    IF NOT pg_catalog.current_setting('kingres_shard.rebuild_verify')::pg_catalog.bool THEN
        UPDATE kingres.table_operation o SET verified = 'skipped' WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    ELSIF v_item.old_name IS NULL OR pg_catalog.to_regclass(v_old) IS NULL THEN
        UPDATE kingres.table_operation o SET verified = 'skipped' WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    ELSE
        SELECT d.n_rows, d.n_hash INTO v_orows, v_ohash FROM kingres.internal_table_digest(v_old::pg_catalog.regclass) d;
        FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
            v_xid := v_rec.spec OPERATOR(pg_catalog.->) 'swap_xid' OPERATOR(pg_catalog.->>) nd.node_id::pg_catalog.text;
            IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
                SELECT d.n_rows, d.n_hash, d.n_newer INTO v_r, v_h, v_nw
                  FROM kingres.internal_table_digest(v_new::pg_catalog.regclass, v_xid::pg_catalog.xid8) d;
            ELSE
                v_res := kingres.internal_exec_scalar(nd.node_id,
                    pg_catalog.format('SELECT d.n_rows::pg_catalog.text OPERATOR(pg_catalog.||) '','' OPERATOR(pg_catalog.||) d.n_hash::pg_catalog.text OPERATOR(pg_catalog.||) '','' OPERATOR(pg_catalog.||) d.n_newer::pg_catalog.text FROM kingres.internal_table_digest(%L::pg_catalog.regclass, %L::pg_catalog.xid8) d', v_new, v_xid));
                v_r := pg_catalog.split_part(v_res, ',', 1)::pg_catalog.int8;
                v_h := pg_catalog.split_part(v_res, ',', 2)::pg_catalog.numeric;
                v_nw := pg_catalog.split_part(v_res, ',', 3)::pg_catalog.int8;
            END IF;
            v_newer := v_newer OPERATOR(pg_catalog.+) v_nw;
            IF v_ref THEN
                IF v_r IS DISTINCT FROM v_orows OR v_h IS DISTINCT FROM v_ohash THEN
                    v_ok := false;
                    v_why := pg_catalog.format('node %s holds %s rows (the old copy has %s) or a different content', nd.node_id, v_r, v_orows);
                END IF;
            ELSE
                v_rows := v_rows OPERATOR(pg_catalog.+) v_r;
                v_hash := v_hash OPERATOR(pg_catalog.+) v_h;
            END IF;
        END LOOP;
        IF NOT v_ref AND (v_rows IS DISTINCT FROM v_orows OR v_hash IS DISTINCT FROM v_ohash) THEN
            v_ok := false;
            v_why := pg_catalog.format('the new table holds %s rows (the old copy has %s) or a different content', v_rows, v_orows);
        END IF;
        IF NOT v_ok AND v_newer OPERATOR(pg_catalog.>) 0 THEN
            -- the table was written to after the swap: the old copy cannot be compared with it
            UPDATE kingres.table_operation o SET verified = 'skipped', detail = 'the table was written to after the switch; the comparison with the old copy is not possible'
             WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
        ELSIF NOT v_ok THEN
            UPDATE kingres.table_operation o SET verified = 'failed', detail = v_why, updated_at = pg_catalog.clock_timestamp()
             WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
            RETURN pg_catalog.format('failed: %s', v_why);
        ELSE
            UPDATE kingres.table_operation o SET verified = 'ok' WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
        END IF;
    END IF;

    -- the foreign keys that the swap added NOT VALID
    SELECT pg_catalog.array_agg(x) INTO v_names FROM pg_catalog.jsonb_array_elements_text(v_rec.spec OPERATOR(pg_catalog.->) 'validate') x;
    IF v_names IS NOT NULL THEN
        PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
        FOREACH nm IN ARRAY v_names LOOP
            FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
                IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
                    EXECUTE pg_catalog.format('ALTER TABLE %s VALIDATE CONSTRAINT %I', v_new, nm);
                ELSE
                    PERFORM kingres.internal_node_exec(nd.node_id,
                        pg_catalog.format('SET LOCAL kingres_shard.enable_routing = off; ALTER TABLE %s VALIDATE CONSTRAINT %I', v_new, nm));
                END IF;
            END LOOP;
        END LOOP;
    END IF;
    PERFORM kingres.internal_fault_point('rebuild_cleanup');

    DELETE FROM kingres.table_operation_lock l WHERE l.op_id OPERATOR(pg_catalog.=) p_op;
    PERFORM kingres.internal_op_set(p_op, 'done', NULL);
    RETURN 'done';
END;
$$;

-- ---- undoing an operation that did not switch ----

CREATE FUNCTION kingres.internal_op_request_cancel(p_op bigint)
    RETURNS void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
UPDATE kingres.table_operation o SET cancel_requested = true, updated_at = pg_catalog.clock_timestamp()
 WHERE o.op_id OPERATOR(pg_catalog.=) p_op AND o.phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching');
$$;

CREATE FUNCTION kingres.internal_op_attempt(p_op bigint, p_detail text)
    RETURNS void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
UPDATE kingres.table_operation o SET attempts = o.attempts OPERATOR(pg_catalog.+) 1, detail = p_detail,
       updated_at = pg_catalog.clock_timestamp()
 WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
$$;

-- Undo an operation that has not switched: the capture triggers and the log on the source, the shadow
-- tables (and the sequences made for them) on every node, the claim and a colocation group that the
-- operation made.  Every step checks what exists.  A node that cannot be reached keeps its leftovers: they
-- are named in the spec (cleanup_pending) and removed by a later call.  Runs in a session of its own.
-- Returns 'undone', 'incomplete' (a node was missed), or 'switched' (nothing to undo).
CREATE FUNCTION kingres.internal_op_undo(p_op bigint, p_final text, p_reason text)
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rec    kingres.table_operation%ROWTYPE;
    v_spec   pg_catalog.jsonb;
    v_rel    pg_catalog.oid;
    v_self   pg_catalog.int4;
    v_nsp    pg_catalog.text;
    v_shadow pg_catalog.text;
    v_failed pg_catalog.int4[] := ARRAY[]::pg_catalog.int4[];
    v_seqs   pg_catalog.text[];
    v_group  pg_catalog.int4;
    v_drop   pg_catalog.bool := false;
    nd       pg_catalog.record;
    t        pg_catalog.record;
    s        pg_catalog.text;
    i        pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT * INTO v_rec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'table operation % does not exist', p_op USING ERRCODE = 'undefined_object';
    END IF;
    IF v_rec.phase IN ('cleaning', 'done') THEN
        RETURN 'switched';
    END IF;
    v_spec := v_rec.spec;
    IF v_rec.phase IN ('failed', 'cancelled')
       AND pg_catalog.jsonb_array_length(COALESCE(v_spec OPERATOR(pg_catalog.->) 'cleanup_pending', '[]'::pg_catalog.jsonb)) OPERATOR(pg_catalog.=) 0 THEN
        RETURN 'undone';
    END IF;
    UPDATE kingres.table_operation o SET cancel_requested = true WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_rel := (v_spec OPERATOR(pg_catalog.->>) 'relid')::pg_catalog.oid;
    v_nsp := v_spec OPERATOR(pg_catalog.->>) 'schema';
    v_shadow := v_spec OPERATOR(pg_catalog.->>) 'shadow';
    v_group := (v_spec OPERATOR(pg_catalog.->>) 'colocation_id')::pg_catalog.int4;
    SELECT pg_catalog.array_agg(x) INTO v_seqs FROM pg_catalog.jsonb_array_elements_text(v_spec OPERATOR(pg_catalog.->) 'sequences') x;

    -- 1. the capture on the source
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) v_rel) THEN
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_trigger g WHERE g.tgrelid OPERATOR(pg_catalog.=) v_rel
                      AND g.tgname IN (pg_catalog.format('kingres_capture_%s', p_op), pg_catalog.format('kingres_notrunc_%s', p_op))) THEN
            -- dropping a trigger needs the strongest lock: wait for a long holder a while (a prepared transaction that
            -- the operation could not get past), then leave the rest to resume_table_operations()
            FOR i IN 1 .. 12 LOOP
                BEGIN
                    PERFORM kingres.internal_lock_table(v_rel::pg_catalog.regclass, 'ACCESS EXCLUSIVE');
                    EXIT;
                EXCEPTION WHEN lock_not_available THEN
                    IF i OPERATOR(pg_catalog.=) 12 THEN
                        RAISE;
                    END IF;
                END;
            END LOOP;
            FOR t IN SELECT g.tgname FROM pg_catalog.pg_trigger g WHERE g.tgrelid OPERATOR(pg_catalog.=) v_rel
                        AND g.tgname IN (pg_catalog.format('kingres_capture_%s', p_op), pg_catalog.format('kingres_notrunc_%s', p_op))
            LOOP
                EXECUTE pg_catalog.format('DROP TRIGGER %I ON %s', t.tgname, v_rel::pg_catalog.regclass);
            END LOOP;
        END IF;
    END IF;
    EXECUTE pg_catalog.format('DROP TABLE IF EXISTS kingres.tlog_%s_1', p_op);
    PERFORM kingres.internal_fault_point('rebuild_undo_after_triggers');

    -- 2. the shadows
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        BEGIN
            IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
                EXECUTE pg_catalog.format('DROP TABLE IF EXISTS %I.%I CASCADE', v_nsp, v_shadow);
            ELSE
                PERFORM kingres.internal_node_exec(nd.node_id, pg_catalog.format('DROP TABLE IF EXISTS %I.%I CASCADE', v_nsp, v_shadow));
                IF v_seqs IS NOT NULL THEN
                    FOREACH s IN ARRAY v_seqs LOOP
                        PERFORM kingres.internal_node_exec(nd.node_id, pg_catalog.format('DROP SEQUENCE IF EXISTS %s', s));
                    END LOOP;
                END IF;
            END IF;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            v_failed := v_failed OPERATOR(pg_catalog.||) nd.node_id;
        END;
    END LOOP;
    PERFORM kingres.internal_fault_point('rebuild_undo_after_shadows');

    -- 3. the claim, and the group that the operation made
    DELETE FROM kingres.table_operation_lock l WHERE l.op_id OPERATOR(pg_catalog.=) p_op;
    IF COALESCE((v_spec OPERATOR(pg_catalog.->>) 'new_group')::pg_catalog.bool, false) AND v_group IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) v_group) THEN
        v_drop := true;
    END IF;

    UPDATE kingres.table_operation o
       SET phase = p_final, detail = p_reason, updated_at = pg_catalog.clock_timestamp(), finished_at = pg_catalog.clock_timestamp(),
           spec = o.spec OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object('cleanup_pending', pg_catalog.to_jsonb(v_failed))
     WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    IF v_drop THEN
        BEGIN
            DELETE FROM kingres.bucket_map b WHERE b.colocation_id OPERATOR(pg_catalog.=) v_group;
            DELETE FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_group;
            PERFORM kingres.sync_catalog();
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            -- a node that cannot be reached cannot be told: the empty group stays (it holds no table; the next table
            -- of the same kind takes it) and the undo goes on
            RAISE WARNING 'the colocation group % that table operation % made could not be removed everywhere: %', v_group, p_op, SQLERRM;
        END;
    END IF;
    RETURN CASE WHEN pg_catalog.cardinality(v_failed) OPERATOR(pg_catalog.=) 0 THEN 'undone' ELSE 'incomplete' END;
END;
$$;

-- ---- running the phases ----

-- Run an online operation up to the end of the phase to_phase (shadow, copying, catching_up, switching
-- or cleaning) and return the phase it is in.  Every phase can be called again after a failure or a
-- crash: it checks what exists.  The session that runs it holds the live mark of the operation.
-- Each step that must outlive this call commits in a session of its own (internal_node_exec on this
-- node), so a call that dies leaves a record that another session can continue from, or undo.
-- 'switching' returns with the detail set when the attempts of one call are used up (the caller calls
-- again).  Progress is in kingres.table_operations.
CREATE FUNCTION kingres.table_operation_advance(op_id bigint, to_phase text, timeout_ms int DEFAULT NULL)
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_op       pg_catalog.int8 := op_id;
    v_to       pg_catalog.text := to_phase;
    v_phases   pg_catalog.text[] := ARRAY['preparing', 'shadow', 'copying', 'catching_up', 'switching', 'cleaning', 'done'];
    v_stop     pg_catalog.int4;
    v_rec      kingres.table_operation%ROWTYPE;
    v_self     pg_catalog.int4;
    v_ms       pg_catalog.int4;
    v_chunk_ms pg_catalog.int4;
    v_deadline pg_catalog.timestamptz;
    v_phase    pg_catalog.text;
    v_cancel   pg_catalog.bool;
    v_spec     pg_catalog.jsonb;
    v_res      pg_catalog.text;
    v_script   pg_catalog.text;
    v_read     pg_catalog.int8;
    v_batch    pg_catalog.int4 := pg_catalog.current_setting('kingres_shard.rebuild_batch_rows')::pg_catalog.int4;
    v_thr      pg_catalog.int8 := pg_catalog.current_setting('kingres_shard.rebuild_drain_threshold')::pg_catalog.int8;
    v_max      pg_catalog.int4 := pg_catalog.current_setting('kingres_shard.rebuild_switch_max_attempts')::pg_catalog.int4;
    v_sw_ms    pg_catalog.int4 := kingres.internal_setting('kingres_shard.rebuild_switch_timeout')::pg_catalog.int4;
    v_backlog  pg_catalog.int8;
    v_prev     pg_catalog.int8 := -1;
    v_still    pg_catalog.int4 := 0;
    v_att      pg_catalog.int4;
    v_detail   pg_catalog.text;
    v_diag     pg_catalog.text;
    v_state    pg_catalog.text;
    v_switched pg_catalog.bool;
    n          pg_catalog.record;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    IF pg_catalog.current_setting('transaction_isolation') OPERATOR(pg_catalog.<>) 'read committed' THEN
        RAISE EXCEPTION 'kingres.table_operation_advance() must run in a READ COMMITTED transaction'
            USING ERRCODE = 'invalid_transaction_state',
                  DETAIL = 'It follows the progress of other sessions and nodes with statements that must see their latest commits.';
    END IF;
    IF v_to NOT IN ('shadow', 'copying', 'catching_up', 'switching', 'cleaning') THEN
        RAISE EXCEPTION 'unknown phase "%"', v_to
            USING ERRCODE = 'invalid_parameter_value',
                  HINT = 'Use shadow, copying, catching_up, switching or cleaning.';
    END IF;
    SELECT * INTO v_rec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'table operation % does not exist', v_op USING ERRCODE = 'undefined_object';
    END IF;
    IF v_rec.phase IN ('done', 'failed', 'cancelled') THEN
        RETURN v_rec.phase;
    END IF;
    IF NOT kingres.internal_op_lock(v_op) THEN
        RAISE EXCEPTION 'table operation % is being run by another session', v_op
            USING ERRCODE = 'object_in_use';
    END IF;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') * 1000;
    v_chunk_ms := GREATEST(v_ms * 6, 600000);
    v_stop := pg_catalog.array_position(v_phases, v_to) OPERATOR(pg_catalog.+) 1;
    IF timeout_ms IS NOT NULL THEN
        v_deadline := pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+) pg_catalog.make_interval(secs => timeout_ms OPERATOR(pg_catalog./) 1000.0);
    END IF;

    BEGIN
        LOOP
            SELECT o.phase, o.cancel_requested, o.spec INTO v_phase, v_cancel, v_spec
              FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
            EXIT WHEN v_phase IN ('done', 'failed', 'cancelled');
            IF v_cancel THEN
                PERFORM kingres.internal_exec_scalar(v_self,
                    pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''cancelled'', %L)', v_op, 'cancelled on request'), v_chunk_ms);
                PERFORM kingres.internal_op_unlock(v_op);
                RAISE EXCEPTION 'table operation % was cancelled', v_op USING ERRCODE = 'query_canceled';
            END IF;
            EXIT WHEN pg_catalog.array_position(v_phases, v_phase) OPERATOR(pg_catalog.>=) v_stop;
            IF v_deadline IS NOT NULL AND pg_catalog.clock_timestamp() OPERATOR(pg_catalog.>) v_deadline THEN
                EXIT;
            END IF;

            IF v_phase IN ('preparing', 'shadow') THEN
                -- the shadow on every node, then the capture on the source
                PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_set(%s, ''shadow'', NULL)', v_op), v_ms);
                PERFORM kingres.internal_fault_point('rebuild_after_preparing');
                FOR n IN SELECT nd.node_id, nd.group_id FROM kingres.node nd
                          WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active ORDER BY nd.node_id
                LOOP
                    v_res := kingres.internal_exec_scalar(n.node_id,
                        pg_catalog.format('SELECT pg_catalog.count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace s ON s.oid OPERATOR(pg_catalog.=) c.relnamespace WHERE s.nspname OPERATOR(pg_catalog.=) %L AND c.relname OPERATOR(pg_catalog.=) %L',
                                          v_spec OPERATOR(pg_catalog.->>) 'schema', v_spec OPERATOR(pg_catalog.->>) 'shadow'), v_ms);
                    IF v_res OPERATOR(pg_catalog.=) '0' THEN
                        -- the script is read in a session of its own: reading the sequences of the table here would
                        -- leave a lock in this transaction that the swap (which renames them) would wait for for ever
                        v_script := kingres.internal_exec_scalar(v_self,
                            pg_catalog.format('SELECT kingres.internal_shadow_script(%L::pg_catalog.oid::pg_catalog.regclass, %L::pg_catalog.name, %L::pg_catalog.int4, %L::pg_catalog.int4)',
                                              v_spec OPERATOR(pg_catalog.->>) 'relid', v_spec OPERATOR(pg_catalog.->>) 'column',
                                              v_spec OPERATOR(pg_catalog.->>) 'colocation_id', n.group_id), v_ms);
                        PERFORM kingres.internal_node_exec(n.node_id, v_script, v_chunk_ms);
                    END IF;
                END LOOP;
                PERFORM kingres.internal_fault_point('rebuild_after_shadow');
                PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_capture(%s)', v_op), v_chunk_ms);
                PERFORM kingres.internal_fault_point('rebuild_after_capture');
                PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_set(%s, ''copying'', NULL)', v_op), v_ms);

            ELSIF v_phase OPERATOR(pg_catalog.=) 'copying' THEN
                LOOP
                    v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_copy_step(%s)', v_op), v_chunk_ms);
                    v_read := ((v_res::pg_catalog.jsonb) OPERATOR(pg_catalog.->>) 'read')::pg_catalog.int8;
                    EXIT WHEN v_read OPERATOR(pg_catalog.=) 0;
                    EXIT WHEN EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op AND o.cancel_requested);
                    EXIT WHEN v_deadline IS NOT NULL AND pg_catalog.clock_timestamp() OPERATOR(pg_catalog.>) v_deadline;
                END LOOP;
                CONTINUE WHEN v_read OPERATOR(pg_catalog.<>) 0;
                PERFORM kingres.internal_fault_point('rebuild_after_copy');
                PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_set(%s, ''catching_up'', NULL)', v_op), v_ms);

            ELSIF v_phase OPERATOR(pg_catalog.=) 'catching_up' THEN
                -- replay until the backlog is under the threshold after a chunk
                v_prev := -1;
                v_still := 0;
                LOOP
                    v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_replay_chunk(%s, %s)', v_op, v_batch), v_chunk_ms);
                    v_backlog := kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', v_spec OPERATOR(pg_catalog.->>) 'log'), v_ms)::pg_catalog.int8;
                    EXIT WHEN v_backlog OPERATOR(pg_catalog.<=) v_thr;
                    EXIT WHEN v_res::pg_catalog.int4 OPERATOR(pg_catalog.=) 0;
                    IF v_prev OPERATOR(pg_catalog.>=) 0 AND v_backlog OPERATOR(pg_catalog.>=) v_prev THEN
                        v_still := v_still OPERATOR(pg_catalog.+) 1;
                        IF v_still OPERATOR(pg_catalog.>=) 5 THEN
                            PERFORM kingres.internal_exec_scalar(v_self,
                                pg_catalog.format('SELECT kingres.internal_op_set(%s, ''catching_up'', %L)', v_op,
                                                  pg_catalog.format('the backlog does not shrink: %s entries', v_backlog)), v_ms);
                            v_still := 0;
                        END IF;
                    ELSE
                        v_still := 0;
                    END IF;
                    v_prev := v_backlog;
                    EXIT WHEN EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op AND o.cancel_requested);
                    EXIT WHEN v_deadline IS NOT NULL AND pg_catalog.clock_timestamp() OPERATOR(pg_catalog.>) v_deadline;
                END LOOP;
                CONTINUE WHEN v_backlog OPERATOR(pg_catalog.>) v_thr;
                PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_set(%s, ''switching'', NULL)', v_op), v_ms);

            ELSIF v_phase OPERATOR(pg_catalog.=) 'switching' THEN
                v_att := 0;
                v_switched := false;
                LOOP
                    v_att := v_att OPERATOR(pg_catalog.+) 1;
                    BEGIN
                        v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_switch_attempt(%s)', v_op),
                                                              v_sw_ms OPERATOR(pg_catalog.+) v_ms * 2);
                        v_switched := true;
                    EXCEPTION WHEN OTHERS THEN
                        GET STACKED DIAGNOSTICS v_state = RETURNED_SQLSTATE, v_diag = PG_EXCEPTION_DETAIL;
                        -- a switch that committed although the answer was lost is a switch
                        SELECT o.phase INTO v_phase FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
                        IF v_phase IN ('cleaning', 'done') THEN
                            v_switched := true;
                        ELSIF v_state OPERATOR(pg_catalog.=) '55000'
                              AND EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op AND o.cancel_requested) THEN
                            -- the attempt saw the request to stop: the loop below sees it too
                            NULL;
                        ELSIF v_state OPERATOR(pg_catalog.<>) '55P03' THEN
                            RAISE;
                        ELSE
                            v_detail := pg_catalog.format('attempt %s: %s%s', v_att, SQLERRM,
                                                          CASE WHEN v_diag IS NULL THEN '' ELSE ' (' OPERATOR(pg_catalog.||) v_diag OPERATOR(pg_catalog.||) ')' END);
                            PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_attempt(%s, %L)', v_op, v_detail), v_ms);
                        END IF;
                    END;
                    EXIT WHEN v_switched;
                    EXIT WHEN v_att OPERATOR(pg_catalog.>=) v_max;
                    EXIT WHEN EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op AND o.cancel_requested);
                    PERFORM pg_catalog.pg_sleep(0.2 OPERATOR(pg_catalog.+) pg_catalog.random() OPERATOR(pg_catalog.*) 0.5);
                    -- the log grew while the attempt waited
                    v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_replay_chunk(%s, %s)', v_op, v_batch), v_chunk_ms);
                END LOOP;
                IF v_switched THEN
                    PERFORM kingres.internal_fault_point('rebuild_after_switch');
                    CONTINUE;
                END IF;
                IF EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op AND o.cancel_requested) THEN
                    CONTINUE;
                END IF;
                -- the operation stays in switching with its detail; the caller calls again
                EXIT;

            ELSIF v_phase OPERATOR(pg_catalog.=) 'cleaning' THEN
                v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_clean(%s)', v_op), v_chunk_ms * 2);
                IF v_res LIKE 'failed:%' THEN
                    RAISE EXCEPTION 'verification of table operation % failed: %', v_op, pg_catalog.substr(v_res, 9)
                        USING ERRCODE = 'data_corrupted',
                              HINT = 'The old copy is kept; the data was not rolled back.';
                END IF;
            END IF;
        END LOOP;
    EXCEPTION WHEN OTHERS THEN
        PERFORM kingres.internal_op_unlock(v_op);
        RAISE;
    END;
    PERFORM kingres.internal_op_unlock(v_op);
    RETURN (SELECT o.phase FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op);
END;
$$;

-- Stop an operation that has not switched: its capture and shadows are removed, its claim released.  A
-- session that is running it notices at the end of its chunk and undoes it itself; otherwise it is undone
-- here.  After the switch nothing is undone and a notice says so.
CREATE FUNCTION kingres.cancel_table_operation(op_id bigint)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_op    pg_catalog.int8 := op_id;
    v_phase pg_catalog.text;
    v_self  pg_catalog.int4;
    v_ms    pg_catalog.int4;
    v_res   pg_catalog.text;
    i       pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT o.phase INTO v_phase FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'table operation % does not exist', v_op USING ERRCODE = 'undefined_object';
    END IF;
    IF v_phase IN ('cleaning', 'done') THEN
        RAISE NOTICE 'table operation % has switched: nothing is undone', v_op;
        RETURN;
    END IF;
    IF v_phase IN ('failed', 'cancelled') THEN
        RETURN;
    END IF;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') * 1000;
    PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_request_cancel(%s)', v_op), v_ms);
    IF kingres.internal_op_alive(v_op) THEN
        -- the session that runs it undoes it at the end of its chunk
        FOR i IN 1 .. (v_ms OPERATOR(pg_catalog./) 100) * 4 LOOP
            PERFORM pg_catalog.pg_sleep(0.1);
            SELECT o.phase INTO v_phase FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
            EXIT WHEN v_phase IN ('failed', 'cancelled', 'cleaning', 'done');
        END LOOP;
        IF v_phase NOT IN ('failed', 'cancelled', 'cleaning', 'done') THEN
            RAISE NOTICE 'table operation % was asked to stop; the session that runs it has not yet noticed', v_op;
        END IF;
        RETURN;
    END IF;
    v_res := kingres.internal_exec_scalar(v_self,
        pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''cancelled'', %L)', v_op, 'cancelled on request'), v_ms * 6);
    IF v_res OPERATOR(pg_catalog.=) 'incomplete' THEN
        RAISE NOTICE 'table operation % was cancelled but a node could not be cleaned; kingres.resume_table_operations() removes what is left', v_op;
    END IF;
END;
$$;

-- The recovery worker's pass (and anybody's): operations whose session is gone and that did not
-- switch are undone (an operation is left alone while its session holds the live mark, or has written
-- its record a moment ago: a switch may still be committing); operations that switched are finished
-- (verified, validated, released); undone operations that missed a node are cleaned again.  Prepared
-- transactions are resolved first.  Returns how many it resolved.
CREATE FUNCTION kingres.resume_table_operations()
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r      pg_catalog.record;
    v_self pg_catalog.int4;
    v_res  pg_catalog.text;
    v_to   pg_catalog.int4;
    n      pg_catalog.int4 := 0;
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF pg_catalog.pg_is_in_recovery() THEN
        RETURN 0;
    END IF;
    SELECT l.node_id INTO v_self
      FROM kingres.local_node l JOIN kingres.node nd ON nd.node_id OPERATOR(pg_catalog.=) l.node_id
     WHERE nd.group_id OPERATOR(pg_catalog.=) 0 AND nd.role OPERATOR(pg_catalog.=) 'primary';
    IF v_self IS NULL THEN
        RETURN 0;
    END IF;
    v_to := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout');
    BEGIN
        PERFORM kingres.internal_exec_scalar(v_self, 'SELECT kingres.recover_prepared_transactions()', v_to * 4000);
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'kingres_shard: recovery of prepared transactions failed: %', SQLERRM;
    END;

    FOR r IN SELECT o.op_id, o.phase, o.updated_at,
                    (o.phase IN ('failed', 'cancelled')) AS finished
               FROM kingres.table_operation o
              WHERE o.phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching', 'cleaning')
                 OR (o.phase IN ('failed', 'cancelled')
                     AND pg_catalog.jsonb_array_length(COALESCE(o.spec OPERATOR(pg_catalog.->) 'cleanup_pending', '[]'::pg_catalog.jsonb)) OPERATOR(pg_catalog.>) 0)
              ORDER BY o.op_id
    LOOP
        BEGIN
            IF kingres.internal_op_alive(r.op_id) THEN
                CONTINUE;
            END IF;
            IF r.phase OPERATOR(pg_catalog.=) 'cleaning' THEN
                v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_clean(%s)', r.op_id), v_to * 8000);
                IF v_res OPERATOR(pg_catalog.=) 'done' THEN
                    n := n OPERATOR(pg_catalog.+) 1;
                END IF;
            ELSE
                IF NOT r.finished AND r.updated_at OPERATOR(pg_catalog.>) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) pg_catalog.make_interval(secs => v_to * 2)) THEN
                    CONTINUE;
                END IF;
                v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format(
                    'SELECT kingres.internal_op_undo(%s, CASE WHEN %L THEN (SELECT o.phase FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) %s) ELSE ''failed'' END, %L)',
                    r.op_id, r.finished, r.op_id, 'interrupted; undone by kingres.resume_table_operations()'), v_to * 6000);
                IF v_res IN ('undone', 'switched') THEN
                    n := n OPERATOR(pg_catalog.+) 1;
                END IF;
            END IF;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            RAISE WARNING 'kingres_shard: could not resume table operation %: %', r.op_id, SQLERRM;
        END;
    END LOOP;
    RETURN n;
END;
$$;

-- Drop the old copies whose time has come (all of them with force).  A copy that failed its
-- verification is kept unless force is given.
CREATE FUNCTION kingres.cleanup_table_operations(force bool DEFAULT false)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_force pg_catalog.bool := force;
    r       pg_catalog.record;
    n       pg_catalog.int4 := 0;
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF pg_catalog.pg_is_in_recovery() THEN
        RETURN 0;
    END IF;
    FOR r IN SELECT c.relid::pg_catalog.oid AS oid
               FROM kingres.table_old_copy c
              WHERE (v_force OR c.dropped_after OPERATOR(pg_catalog.<=) pg_catalog.clock_timestamp())
                AND (v_force OR NOT EXISTS (SELECT 1 FROM kingres.table_operation_item i
                                              JOIN kingres.table_operation o ON o.op_id OPERATOR(pg_catalog.=) i.op_id
                                             WHERE i.old_name OPERATOR(pg_catalog.=) (SELECT cl.relname FROM pg_catalog.pg_class cl WHERE cl.oid OPERATOR(pg_catalog.=) c.relid::pg_catalog.oid)
                                               AND o.verified OPERATOR(pg_catalog.=) 'failed'))
              ORDER BY c.dropped_after
    LOOP
        PERFORM kingres.internal_fault_point('rebuild_cleanup');
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_class cl WHERE cl.oid OPERATOR(pg_catalog.=) r.oid) THEN
            EXECUTE pg_catalog.format('DROP TABLE %s', r.oid::pg_catalog.regclass);
        END IF;
        DELETE FROM kingres.table_old_copy c WHERE c.relid::pg_catalog.oid OPERATOR(pg_catalog.=) r.oid;
        n := n OPERATOR(pg_catalog.+) 1;
    END LOOP;
    RETURN n;
END;
$$;

-- ---- the online variants ----

CREATE FUNCTION kingres.create_distributed_table_concurrently(relation regclass, dist_column name,
                                                              colocate_with regclass DEFAULT NULL,
                                                              bucket_count int DEFAULT NULL)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_op     pg_catalog.int8;
    v_phase  pg_catalog.text;
    v_self   pg_catalog.int4;
    v_ms     pg_catalog.int4;
    v_detail pg_catalog.text;
BEGIN
    PERFORM kingres.internal_prevent_xact_block('kingres.create_distributed_table_concurrently()');
    v_op := kingres.begin_table_operation('distribute', relation,
                pg_catalog.jsonb_strip_nulls(pg_catalog.jsonb_build_object('column', dist_column,
                    'colocate_with', colocate_with::pg_catalog.oid::pg_catalog.text, 'bucket_count', bucket_count)));
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') * 6000;
    BEGIN
        v_phase := kingres.table_operation_advance(v_op, 'cleaning');
    EXCEPTION WHEN OTHERS THEN
        -- what did not switch is undone (an operation that switched answers 'switched')
        BEGIN
            PERFORM kingres.internal_exec_scalar(v_self,
                pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''failed'', %L)', v_op, SQLERRM), v_ms);
        EXCEPTION WHEN OTHERS THEN
            NULL;
        END;
        RAISE;
    END;
    IF v_phase OPERATOR(pg_catalog.<>) 'done' THEN
        SELECT o.detail INTO v_detail FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
        PERFORM kingres.internal_exec_scalar(v_self,
            pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''failed'', %L)', v_op, COALESCE(v_detail, 'did not finish')), v_ms);
        RAISE EXCEPTION 'table operation % did not finish: it stopped in phase %', v_op, v_phase
            USING ERRCODE = 'object_not_in_prerequisite_state', DETAIL = v_detail;
    END IF;
    RETURN v_op;
END;
$$;

CREATE FUNCTION kingres.create_reference_table_concurrently(relation regclass)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_op     pg_catalog.int8;
    v_phase  pg_catalog.text;
    v_self   pg_catalog.int4;
    v_ms     pg_catalog.int4;
    v_detail pg_catalog.text;
BEGIN
    PERFORM kingres.internal_prevent_xact_block('kingres.create_reference_table_concurrently()');
    v_op := kingres.begin_table_operation('distribute_reference', relation, '{}'::pg_catalog.jsonb);
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') * 6000;
    BEGIN
        v_phase := kingres.table_operation_advance(v_op, 'cleaning');
    EXCEPTION WHEN OTHERS THEN
        BEGIN
            PERFORM kingres.internal_exec_scalar(v_self,
                pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''failed'', %L)', v_op, SQLERRM), v_ms);
        EXCEPTION WHEN OTHERS THEN
            NULL;
        END;
        RAISE;
    END;
    IF v_phase OPERATOR(pg_catalog.<>) 'done' THEN
        SELECT o.detail INTO v_detail FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
        PERFORM kingres.internal_exec_scalar(v_self,
            pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''failed'', %L)', v_op, COALESCE(v_detail, 'did not finish')), v_ms);
        RAISE EXCEPTION 'table operation % did not finish: it stopped in phase %', v_op, v_phase
            USING ERRCODE = 'object_not_in_prerequisite_state', DETAIL = v_detail;
    END IF;
    RETURN v_op;
END;
$$;

COMMENT ON FUNCTION kingres.create_distributed_table_concurrently(regclass, name, regclass, int) IS
    'The online variant of create_distributed_table: writers keep working; returns the id of the operation when the table is distributed';
COMMENT ON FUNCTION kingres.create_reference_table_concurrently(regclass) IS
    'The online variant of create_reference_table: writers keep working; returns the id of the operation when the table is a reference table';
COMMENT ON FUNCTION kingres.begin_table_operation(text, regclass, jsonb) IS
    'Check, claim and record an online table operation; kingres.table_operation_advance() runs its phases';
COMMENT ON FUNCTION kingres.table_operation_advance(bigint, text, int) IS
    'Run an online table operation up to the end of a phase and return the phase it is in';
COMMENT ON FUNCTION kingres.cancel_table_operation(bigint) IS
    'Undo an online table operation that has not switched';
COMMENT ON FUNCTION kingres.resume_table_operations() IS
    'Undo the operations whose session is gone and that did not switch, finish those that switched; returns how many it resolved';
COMMENT ON FUNCTION kingres.cleanup_table_operations(bool) IS
    'Drop the old copies of replaced tables whose time has come; returns how many';

REVOKE ALL ON FUNCTION
    kingres.internal_op_lock(bigint),
    kingres.internal_op_unlock(bigint),
    kingres.internal_op_alive(bigint),
    kingres.internal_prevent_xact_block(text),
    kingres.internal_choose_colocation(regtype, oid, regclass, int),
    kingres.internal_shadow_script(regclass, name, int, int),
    kingres.internal_swap_table(bigint, regclass, name, int, int),
    kingres.internal_copy_chunk(regclass, name, name, text, name, int, int, text[], text[], bigint, text, bool),
    kingres.internal_branch_exec(int, text),
    kingres.internal_capture_trigger(),
    kingres.internal_capture_truncate(),
    kingres.internal_lock_holders(regclass),
    kingres.internal_setting(text),
    kingres.internal_lock_table(regclass, text),
    kingres.internal_op_set(bigint, text, text),
    kingres.internal_op_spec(bigint, jsonb),
    kingres.internal_log_key(regclass),
    kingres.internal_begin_table_operation(text, regclass, jsonb),
    kingres.begin_table_operation(text, regclass, jsonb),
    kingres.internal_op_capture(bigint),
    kingres.internal_op_copy_step(bigint),
    kingres.internal_replay_chunk(bigint, int),
    kingres.internal_retire_table(regclass, bigint),
    kingres.internal_switch_attempt(bigint),
    kingres.internal_table_digest(regclass, xid8),
    kingres.internal_op_clean(bigint),
    kingres.internal_op_request_cancel(bigint),
    kingres.internal_op_attempt(bigint, text),
    kingres.internal_op_undo(bigint, text, text),
    kingres.table_operation_advance(bigint, text, int),
    kingres.cancel_table_operation(bigint),
    kingres.resume_table_operations(),
    kingres.cleanup_table_operations(bool),
    kingres.create_distributed_table_concurrently(regclass, name, regclass, int),
    kingres.create_reference_table_concurrently(regclass)
    FROM PUBLIC;

-- ---- check_cluster() knows the leftovers of table operations ----

-- What is wrong with the table operations (design, "Interaction with the rest of the system"):
--   table_operation_stalled     unfinished, nobody runs it, not written for three node_timeout
--   shadow_table, log_table, capture_trigger
--                               objects of an operation that is done, failed, cancelled or unknown
--   old_table                   an old copy that is overdue by an hour
--   table_verification_failed   the old copy and the new table differ; the old copy is kept
CREATE FUNCTION kingres.internal_check_table_operations()
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_to   pg_catalog.int4;
    n      pg_catalog.record;
    o      pg_catalog.record;
    r      pg_catalog.jsonb;
    x      pg_catalog.text;
    v_oid  pg_catalog.oid;
    v_op   pg_catalog.int8;
    v_live pg_catalog.bool;
BEGIN
    IF pg_catalog.to_regclass('kingres.table_operation') IS NULL THEN
        RETURN;
    END IF;
    v_to := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout');

    FOR o IN SELECT t.op_id, t.phase, t.updated_at, t.verified FROM kingres.table_operation t ORDER BY t.op_id LOOP
        IF o.phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching', 'cleaning')
           AND o.updated_at OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) pg_catalog.make_interval(secs => v_to * 3))
           AND NOT kingres.internal_op_alive(o.op_id) THEN
            check_name := 'table_operation_stalled'; node_id := NULL;
            object := pg_catalog.format('table operation %s', o.op_id);
            detail := pg_catalog.format('in phase %s since %s and nobody runs it; kingres.table_operation_advance() continues it, kingres.cancel_table_operation() undoes it', o.phase, o.updated_at);
            RETURN NEXT;
        END IF;
        IF o.verified OPERATOR(pg_catalog.=) 'failed' THEN
            check_name := 'table_verification_failed'; node_id := NULL;
            object := pg_catalog.format('table operation %s', o.op_id);
            detail := 'the old copy and the new table differ; the old copy is kept as the evidence (kingres.cleanup_table_operations(true) drops it)';
            RETURN NEXT;
        END IF;
    END LOOP;

    FOR o IN SELECT c.relid::pg_catalog.oid AS oid, c.dropped_after FROM kingres.table_old_copy c
              WHERE c.dropped_after OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) interval '1 hour') ORDER BY c.dropped_after LOOP
        check_name := 'old_table'; node_id := NULL;
        object := COALESCE(o.oid::pg_catalog.regclass::pg_catalog.text, o.oid::pg_catalog.text);
        detail := pg_catalog.format('the old copy of a replaced table was due for dropping at %s', o.dropped_after);
        RETURN NEXT;
    END LOOP;

    -- the objects that operations make on the nodes: a shadow is kingres_shadow_<oid of the source>, a log and
    -- the triggers carry the id of the operation
    FOR n IN SELECT nd.node_id AS id FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active ORDER BY nd.node_id LOOP
        BEGIN
            r := kingres.internal_node_exec(n.id,
                'SELECT pg_catalog.jsonb_build_object(''shadows'', COALESCE((SELECT pg_catalog.jsonb_agg(s.nspname OPERATOR(pg_catalog.||) ''.'' OPERATOR(pg_catalog.||) c.relname) '
                'FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace s ON s.oid OPERATOR(pg_catalog.=) c.relnamespace '
                'WHERE c.relname LIKE ''kingres\_shadow\_%'' AND c.relkind IN (''r'', ''p'')), ''[]''::pg_catalog.jsonb), '
                '''logs'', COALESCE((SELECT pg_catalog.jsonb_agg(c.relname) FROM pg_catalog.pg_class c WHERE c.relnamespace OPERATOR(pg_catalog.=) ''kingres''::pg_catalog.regnamespace AND c.relname LIKE ''tlog\_%'' AND c.relkind OPERATOR(pg_catalog.=) ''r''), ''[]''::pg_catalog.jsonb), '
                '''triggers'', COALESCE((SELECT pg_catalog.jsonb_agg(t.tgname) FROM pg_catalog.pg_trigger t WHERE NOT t.tgisinternal AND (t.tgname LIKE ''kingres\_capture\_%'' OR t.tgname LIKE ''kingres\_notrunc\_%'')), ''[]''::pg_catalog.jsonb))');
            r := (r OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            CONTINUE;
        END;
        FOR x IN SELECT e FROM pg_catalog.jsonb_array_elements_text(r OPERATOR(pg_catalog.->) 'shadows') e LOOP
            v_oid := NULLIF(pg_catalog.regexp_replace(x, '^.*kingres_shadow_', ''), '')::pg_catalog.oid;
            IF NOT EXISTS (SELECT 1 FROM kingres.table_operation_item i JOIN kingres.table_operation t ON t.op_id OPERATOR(pg_catalog.=) i.op_id
                            WHERE i.relid::pg_catalog.oid OPERATOR(pg_catalog.=) v_oid
                              AND t.phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching')) THEN
                check_name := 'shadow_table'; node_id := n.id; object := x;
                detail := 'the shadow table of a table operation that is not running';
                RETURN NEXT;
            END IF;
        END LOOP;
        FOR x IN SELECT e FROM pg_catalog.jsonb_array_elements_text(r OPERATOR(pg_catalog.->) 'logs') e LOOP
            v_op := NULLIF(pg_catalog.split_part(x, '_', 2), '')::pg_catalog.int8;
            IF NOT EXISTS (SELECT 1 FROM kingres.table_operation t WHERE t.op_id OPERATOR(pg_catalog.=) v_op
                              AND t.phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching')) THEN
                check_name := 'log_table'; node_id := n.id; object := pg_catalog.format('kingres.%s', x);
                detail := 'the capture log of a table operation that is not running';
                RETURN NEXT;
            END IF;
        END LOOP;
        FOR x IN SELECT e FROM pg_catalog.jsonb_array_elements_text(r OPERATOR(pg_catalog.->) 'triggers') e LOOP
            v_op := NULLIF(pg_catalog.regexp_replace(x, '^kingres_(capture|notrunc)_', ''), '')::pg_catalog.int8;
            IF NOT EXISTS (SELECT 1 FROM kingres.table_operation t WHERE t.op_id OPERATOR(pg_catalog.=) v_op
                              AND t.phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching')) THEN
                check_name := 'capture_trigger'; node_id := n.id; object := x;
                detail := 'a capture trigger of a table operation that is not running';
                RETURN NEXT;
            END IF;
        END LOOP;
    END LOOP;
    RETURN;
END;
$$;

ALTER FUNCTION kingres.check_cluster(interval) RENAME TO internal_check_cluster_base;

CREATE FUNCTION kingres.check_cluster(p_prepared_older_than interval DEFAULT interval '5 minutes')
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    RETURN QUERY SELECT b.check_name, b.node_id, b.object, b.detail FROM kingres.internal_check_cluster_base(p_prepared_older_than) b;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_table_operations() t;
END;
$$;

COMMENT ON FUNCTION kingres.check_cluster(interval) IS
    'One row per inconsistency of the cluster (catalogs, partitions against the bucket map, leftovers of moves and of table operations, old prepared transactions); none when consistent';

REVOKE ALL ON FUNCTION
    kingres.check_cluster(interval),
    kingres.internal_check_cluster_base(interval),
    kingres.internal_check_table_operations()
    FROM PUBLIC;
