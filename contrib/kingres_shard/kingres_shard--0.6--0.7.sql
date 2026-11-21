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
    ADD COLUMN reloptions       text[],
    ADD CONSTRAINT dist_table_partition_check CHECK (
        (partition_method IS NULL AND partition_key IS NULL)
        OR (method = 'hash' AND partition_method IN ('range', 'list') AND partition_key IS NOT NULL));

COMMENT ON COLUMN kingres.dist_table.partition_method IS
    'NULL, ''range'' or ''list'': how the user partitioned the table below the hash level';
COMMENT ON COLUMN kingres.dist_table.reloptions IS
    'The storage parameters of the table (fillfactor, autovacuum_*, ...) as pg_class.reloptions gives them: a partitioned table cannot have them, so they are applied to the bucket partitions, and carried to every new table that replaces this one';

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
           (pg_catalog.upper(pg_catalog.btrim(p.bound)) = 'DEFAULT') AS is_default,
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
                                       'partition_key', d.partition_key,
                                       'reloptions', d.reloptions)
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
                       partition_method pg_catalog.text, partition_key pg_catalog.text, reloptions pg_catalog.text[])
    LOOP
        rid := pg_catalog.to_regclass(pg_catalog.format('%I.%I', r.schema, r.name));
        IF rid IS NULL THEN
            RAISE EXCEPTION 'distributed table %.% does not exist on this node', r.schema, r.name
                USING ERRCODE = 'undefined_table';
        END IF;
        INSERT INTO kingres.dist_table (relid, method, dist_column, colocation_id,
                                        partition_method, partition_key, reloptions)
        VALUES (rid, r.method, r.dist_column, r.colocation_id, r.partition_method, r.partition_key, r.reloptions);
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

-- The code of non-superusers that a copy of the table would run with the rights of the caller.  The copy (and the
-- replay, and the key capture) evaluates constraints, defaults, generated columns and index expressions, and every
-- function behind them.  This follows the dependencies from the table, its columns' types, defaults, constraints and
-- indexes to every function, operator, operator class, cast, domain (its constraints and default), range type and
-- composite type they reach, and returns the user-defined functions owned by a role that is not a superuser.  Code of
-- a superuser is trusted: whatever it calls, it called.  The default hash and btree operator classes of the column
-- types are followed too (the routing hashes every distribution key).
CREATE FUNCTION kingres.internal_untrusted_functions(p_relid regclass)
    RETURNS TABLE (func text, owner name, via text)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
WITH RECURSIVE c AS (
    SELECT 'pg_catalog.pg_proc'::pg_catalog.regclass::pg_catalog.oid AS proc,
           'pg_catalog.pg_operator'::pg_catalog.regclass::pg_catalog.oid AS oper,
           'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid AS typ,
           'pg_catalog.pg_opclass'::pg_catalog.regclass::pg_catalog.oid AS opclass,
           'pg_catalog.pg_opfamily'::pg_catalog.regclass::pg_catalog.oid AS opfamily,
           'pg_catalog.pg_constraint'::pg_catalog.regclass::pg_catalog.oid AS con,
           'pg_catalog.pg_attrdef'::pg_catalog.regclass::pg_catalog.oid AS attrdef,
           'pg_catalog.pg_class'::pg_catalog.regclass::pg_catalog.oid AS cls,
           'pg_catalog.pg_amproc'::pg_catalog.regclass::pg_catalog.oid AS amproc,
           'pg_catalog.pg_amop'::pg_catalog.regclass::pg_catalog.oid AS amop,
           'pg_catalog.pg_cast'::pg_catalog.regclass::pg_catalog.oid AS cast
), seed(classid, objid, via) AS (
    SELECT c.typ, a.atttypid, pg_catalog.format('column "%s"', a.attname)
      FROM c, pg_catalog.pg_attribute a
     WHERE a.attrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
    UNION
    SELECT c.attrdef, ad.oid, 'a column default' FROM c, pg_catalog.pg_attrdef ad WHERE ad.adrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid
    UNION
    SELECT c.con, k.oid, pg_catalog.format('constraint "%s"', k.conname) FROM c, pg_catalog.pg_constraint k WHERE k.conrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid
    UNION
    SELECT c.cls, i.indexrelid, pg_catalog.format('index "%s"', i.indexrelid::pg_catalog.regclass) FROM c, pg_catalog.pg_index i WHERE i.indrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid
    UNION
    SELECT c.opclass, o, 'an index operator class' FROM c, pg_catalog.pg_index i, pg_catalog.unnest(i.indclass::pg_catalog.oid[]) o WHERE i.indrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid
    UNION
    SELECT c.opclass, oc.oid, pg_catalog.format('the operator class of column "%s"', a.attname)
      FROM c, pg_catalog.pg_attribute a JOIN pg_catalog.pg_opclass oc ON oc.opcintype OPERATOR(pg_catalog.=) a.atttypid AND oc.opcdefault
     WHERE a.attrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
), walk(classid, objid, via) AS (
    SELECT s.classid, s.objid, s.via FROM seed s WHERE s.objid OPERATOR(pg_catalog.>=) 16384
    UNION
    SELECT e.classid, e.objid, w.via
      FROM walk w CROSS JOIN c CROSS JOIN LATERAL (
            SELECT d.refclassid AS classid, d.refobjid AS objid
              FROM pg_catalog.pg_depend d
             WHERE d.classid OPERATOR(pg_catalog.=) w.classid AND d.objid OPERATOR(pg_catalog.=) w.objid
               AND d.refclassid IN (c.proc, c.oper, c.typ, c.opclass, c.opfamily, c.con, c.attrdef, c.amproc, c.amop, c.cast)
            UNION ALL
            SELECT c.con, k.oid FROM pg_catalog.pg_constraint k WHERE w.classid OPERATOR(pg_catalog.=) c.typ AND k.contypid OPERATOR(pg_catalog.=) w.objid
            UNION ALL
            SELECT c.cls, t.typrelid FROM pg_catalog.pg_type t WHERE w.classid OPERATOR(pg_catalog.=) c.typ AND t.oid OPERATOR(pg_catalog.=) w.objid AND t.typrelid OPERATOR(pg_catalog.<>) 0
            UNION ALL
            SELECT c.opfamily, oc.opcfamily FROM pg_catalog.pg_opclass oc WHERE w.classid OPERATOR(pg_catalog.=) c.opclass AND oc.oid OPERATOR(pg_catalog.=) w.objid
            UNION ALL
            SELECT c.amproc, ap.oid FROM pg_catalog.pg_amproc ap WHERE w.classid OPERATOR(pg_catalog.=) c.opfamily AND ap.amprocfamily OPERATOR(pg_catalog.=) w.objid
            UNION ALL
            SELECT c.amop, ao.oid FROM pg_catalog.pg_amop ao WHERE w.classid OPERATOR(pg_catalog.=) c.opfamily AND ao.amopfamily OPERATOR(pg_catalog.=) w.objid
            UNION ALL
            SELECT c.proc, ca.castfunc FROM pg_catalog.pg_cast ca WHERE w.classid OPERATOR(pg_catalog.=) c.cast AND ca.oid OPERATOR(pg_catalog.=) w.objid AND ca.castfunc OPERATOR(pg_catalog.<>) 0
           ) e
     WHERE e.objid OPERATOR(pg_catalog.>=) 16384
)
SELECT DISTINCT pr.oid::pg_catalog.regprocedure::pg_catalog.text, ro.rolname, w.via
  FROM walk w JOIN pg_catalog.pg_proc pr ON pr.oid OPERATOR(pg_catalog.=) w.objid
  JOIN pg_catalog.pg_roles ro ON ro.oid OPERATOR(pg_catalog.=) pr.proowner
 WHERE w.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_proc'::pg_catalog.regclass::pg_catalog.oid
   AND pr.oid OPERATOR(pg_catalog.>=) 16384 AND NOT ro.rolsuper
 ORDER BY 1;
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
        stmt := format('CREATE TABLE %s (%s)%s', qtarget, coldefs, COALESCE(NULLIF(' ' || kingres.internal_with_clause(kingres.internal_reloptions_of(p_relid)), ' '), ''));
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
        SELECT replace(pg_get_indexdef(i.indexrelid), ' ON ONLY ', ' ON ') AS def, ic.relname AS iname
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
    v_dv      pg_catalog.text := COALESCE(options OPERATOR(pg_catalog.->>) 'dependent_views', pg_catalog.current_setting('kingres_shard.dependent_views', true), 'refuse');
    v_vb      pg_catalog.text;
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
        IF c.relispartition THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'partitioned_table', qname,
                       pfx OPERATOR(pg_catalog.||) 'a partition cannot be distributed on its own: distribute the table it belongs to'));
        ELSIF c.relkind OPERATOR(pg_catalog.=) 'p' AND kind OPERATOR(pg_catalog.=) 'distribute_reference' THEN
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'partitioned_table', qname,
                       pfx OPERATOR(pg_catalog.||) 'a partitioned table cannot be a reference table'));
        ELSIF c.relkind OPERATOR(pg_catalog.=) 'p' THEN
            -- a table the user partitioned (RANGE or LIST, one level, ordinary partitions): hash on top, the user's partitions below
            IF (SELECT t.partstrat FROM pg_catalog.pg_partitioned_table t WHERE t.partrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid)
               NOT IN ('r', 'l') THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'partitioned_table', qname,
                           pfx OPERATOR(pg_catalog.||) 'only tables partitioned by RANGE or LIST can be distributed'));
            END IF;
            IF v_mode OPERATOR(pg_catalog.=) 'online' THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'not_available_yet', qname,
                           pfx OPERATOR(pg_catalog.||) 'a partitioned table is distributed by the blocking create_distributed_table() in this version'));
            END IF;
            FOR r IN SELECT pc.oid, pc.relname, pc.relkind, pc.reloptions, pc.reltablespace, pc.relpersistence
                       FROM pg_catalog.pg_inherits i JOIN pg_catalog.pg_class pc ON pc.oid OPERATOR(pg_catalog.=) i.inhrelid
                      WHERE i.inhparent OPERATOR(pg_catalog.=) relation::pg_catalog.oid ORDER BY pc.relname
            LOOP
                IF r.relkind OPERATOR(pg_catalog.=) 'p' THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'subpartitioned', r.relname,
                               pfx OPERATOR(pg_catalog.||) pg_catalog.format('partition "%s" is itself partitioned; one level of partitions is supported', r.relname)));
                    CONTINUE;
                ELSIF r.relkind OPERATOR(pg_catalog.<>) 'r' THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'foreign_partition', r.relname,
                               pfx OPERATOR(pg_catalog.||) pg_catalog.format('partition "%s" is not an ordinary table', r.relname)));
                    CONTINUE;
                END IF;
                IF pg_catalog.octet_length(r.relname) OPERATOR(pg_catalog.+) 6 OPERATOR(pg_catalog.>) 63 THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'name_too_long', r.relname,
                               pfx OPERATOR(pg_catalog.||) pg_catalog.format('partition name "%s" is too long: leaf names append 6 characters (_bNNNN)', r.relname)));
                END IF;
                IF r.reloptions IS NOT NULL
                   OR EXISTS (SELECT 1 FROM pg_catalog.pg_index x WHERE x.indrelid OPERATOR(pg_catalog.=) r.oid
                                AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits ih WHERE ih.inhrelid OPERATOR(pg_catalog.=) x.indexrelid))
                   OR EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k WHERE k.conrelid OPERATOR(pg_catalog.=) r.oid AND k.conislocal AND k.contype OPERATOR(pg_catalog.<>) 'n')
                   OR EXISTS (SELECT 1 FROM pg_catalog.pg_attrdef d JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) d.adrelid AND a.attnum OPERATOR(pg_catalog.=) d.adnum
                               JOIN pg_catalog.pg_attribute pa ON pa.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND pa.attname OPERATOR(pg_catalog.=) a.attname
                               LEFT JOIN pg_catalog.pg_attrdef pd ON pd.adrelid OPERATOR(pg_catalog.=) pa.attrelid AND pd.adnum OPERATOR(pg_catalog.=) pa.attnum
                              WHERE d.adrelid OPERATOR(pg_catalog.=) r.oid
                                AND pg_catalog.pg_get_expr(d.adbin, d.adrelid) IS DISTINCT FROM pg_catalog.pg_get_expr(pd.adbin, pd.adrelid)) THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'partition_local_object', r.relname,
                               pfx OPERATOR(pg_catalog.||) pg_catalog.format('partition "%s" has a storage parameter, an index, a constraint or a default of its own that the table does not have', r.relname)));
                END IF;
                IF r.reltablespace OPERATOR(pg_catalog.<>) c.reltablespace THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'tablespace', r.relname,
                               pfx OPERATOR(pg_catalog.||) pg_catalog.format('partition "%s" is in another tablespace than the table', r.relname)));
                END IF;
                IF pg_catalog.pg_get_expr((SELECT x.relpartbound FROM pg_catalog.pg_class x WHERE x.oid OPERATOR(pg_catalog.=) r.oid), r.oid) OPERATOR(pg_catalog.=) 'DEFAULT' THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'warning', 'default_partition', r.relname,
                               pg_catalog.format('partition "%s" is the default partition: attaching a new partition scans it in every bucket on every node', r.relname)));
                END IF;
            END LOOP;
            IF v_col IS NOT NULL
               AND (SELECT pg_catalog.pg_get_partkeydef(relation::pg_catalog.oid)) ~ pg_catalog.format('[(, ]"?%s"?[), ]', v_col::pg_catalog.text) THEN
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'warning', 'distribution_column_is_partition_key', v_col::pg_catalog.text,
                           'the distribution column is also the partition key: rows of one interval spread over the buckets, and a tenant''s rows are not colocated'));
            END IF;
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
        IF kingres.internal_reloptions_of(relation) IS NOT NULL THEN
            -- carried: applied to the bucket partitions of a distributed table, to the new table of an undistribute and to a reference table;
            -- the parameters of the TOAST table are written toast.<name> (P9C-08)
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'note', 'storage_parameters', qname,
                       pg_catalog.format('the storage parameters (%s) are carried to the bucket partitions of the new table', pg_catalog.array_to_string(kingres.internal_reloptions_of(relation), ', '))));
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
                    AND v_dv OPERATOR(pg_catalog.<>) 'recreate'
                  ORDER BY 2
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'dependent_object', r.name,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('%s %s depends on the table; the table is replaced by a new one',
                           CASE r.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' ELSE 'object' END, r.name)));
        END LOOP;
        -- a view on a partition of the table: the partitions are replaced too, and the views are recreated for the table only (P9C-14)
        FOR r IN SELECT DISTINCT cl.relkind, cl.oid::pg_catalog.regclass::pg_catalog.text AS name, i.inhrelid::pg_catalog.regclass::pg_catalog.text AS part
                   FROM pg_catalog.pg_inherits i
                   JOIN pg_catalog.pg_depend d ON d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass AND d.refobjid OPERATOR(pg_catalog.=) i.inhrelid
                   JOIN pg_catalog.pg_rewrite rw ON d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass AND rw.oid OPERATOR(pg_catalog.=) d.objid
                   JOIN pg_catalog.pg_class cl ON cl.oid OPERATOR(pg_catalog.=) rw.ev_class
                  WHERE i.inhparent OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND c.relkind OPERATOR(pg_catalog.=) 'p'
                    AND rw.ev_class OPERATOR(pg_catalog.<>) i.inhrelid
                  ORDER BY 2
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'dependent_object', r.name,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('%s %s depends on the partition %s; the partitions are replaced by new ones',
                           CASE r.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' ELSE 'object' END, r.name, r.part)));
        END LOOP;
        IF v_dv OPERATOR(pg_catalog.=) 'recreate' THEN
            FOR v_vb IN SELECT b.detail FROM kingres.internal_view_blockers(ARRAY[relation::pg_catalog.text], false) b LOOP
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'dependent_object', qname, pfx OPERATOR(pg_catalog.||) v_vb));
            END LOOP;
        END IF;
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
        -- the code that the copy runs (constraints, defaults, generated columns, index expressions, domains, operators, operator
        -- classes, casts, range and composite types, and the functions behind them) must belong to a superuser: the call runs it
        -- with the rights of the caller on every node
        FOR r IN SELECT u.func AS name, u.owner AS owner, u.via AS via FROM kingres.internal_untrusted_functions(relation) u ORDER BY 1
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'function_owner_not_superuser', r.name,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('function %s, reached from %s, belongs to the role "%s", who is not a superuser; the copy would run it with the rights of the caller on every node', r.name, r.via, r.owner)));
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
                ELSIF EXISTS (SELECT 1 FROM kingres.bucket_move m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id
                                 AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching')) THEN
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'group_busy', v_dt.colocation_id::pg_catalog.text,
                               pg_catalog.format('colocation group %s is busy: a bucket is being moved', v_dt.colocation_id)));
                END IF;
            END IF;
            IF v_dv OPERATOR(pg_catalog.=) 'recreate' THEN
                FOR v_vb IN SELECT b.detail FROM kingres.internal_view_blockers(
                              (SELECT pg_catalog.array_agg(comp.relid::pg_catalog.text) FROM kingres.internal_fk_component(relation, COALESCE((options OPERATOR(pg_catalog.->>) 'cascade_via_foreign_keys')::pg_catalog.bool, false)) comp), true,
                              CASE WHEN kind OPERATOR(pg_catalog.=) 'undistribute' THEN kingres.internal_group_node(COALESCE((options OPERATOR(pg_catalog.->>) 'target_group')::pg_catalog.int4, 0)) END) b LOOP
                    rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                               'blocker', 'dependent_object', qname, pfx OPERATOR(pg_catalog.||) v_vb));
                END LOOP;
            END IF;
            -- the tables go through DROP TABLE: a view (or anything else) that uses one of them stops it, on this node or another
            FOR r IN SELECT DISTINCT c2.oid::pg_catalog.regclass::pg_catalog.text AS name, comp.relid::pg_catalog.text AS dep
                       FROM kingres.internal_fk_component(relation, COALESCE((options OPERATOR(pg_catalog.->>) 'cascade_via_foreign_keys')::pg_catalog.bool, false)) comp
                       JOIN pg_catalog.pg_depend d ON d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass AND d.refobjid OPERATOR(pg_catalog.=) comp.relid::pg_catalog.oid
                       JOIN pg_catalog.pg_rewrite rw ON d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass AND rw.oid OPERATOR(pg_catalog.=) d.objid
                       JOIN pg_catalog.pg_class c2 ON c2.oid OPERATOR(pg_catalog.=) rw.ev_class
                      WHERE c2.oid OPERATOR(pg_catalog.<>) comp.relid::pg_catalog.oid
                        AND v_dv OPERATOR(pg_catalog.<>) 'recreate'
                      ORDER BY 1
            LOOP
                rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                           'blocker', 'dependent_object', r.name,
                           pg_catalog.format('%s depends on table %s, which is dropped by the change; drop it first', r.name, r.dep)));
            END LOOP;
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
            -- the group that the table would join is used by no operation and no move either (P9C-06)
            IF v_cwith IS NOT NULL THEN
                SELECT d.colocation_id INTO v_group FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_cwith;
                IF v_group IS NOT NULL AND v_group OPERATOR(pg_catalog.<>) v_dt.colocation_id THEN
                    x := kingres.internal_group_busy(v_group);
                    IF x IS NOT NULL THEN
                        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                                   'blocker', 'group_busy', v_group::pg_catalog.text, pfx OPERATOR(pg_catalog.||) x));
                    END IF;
                END IF;
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

    -- a table that is rebuilt is copied into a new table, whose constraints, defaults and indexes run on every node with the rights of the
    -- caller: the code of a role that is not a superuser that they reach is refused here too (P9B-01)
    IF kind IN ('undistribute', 'alter') AND is_dist AND v_dt.partition_method IS NOT NULL THEN
        rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                   'blocker', 'not_available_yet', qname,
                   pfx OPERATOR(pg_catalog.||) 'a user-partitioned table cannot be undistributed or altered in this version'));
    END IF;
    IF kind IN ('undistribute', 'alter') AND is_dist THEN
        -- every table that the call takes with it, not the named table only: the tables of the foreign key component (undistribute,
        -- with cascade_via_foreign_keys) or of the colocation group (alter, with cascade_to_colocated) are copied the same way (P9C-04)
        FOR r IN SELECT m.relid::pg_catalog.text AS tbl, (m.relid OPERATOR(pg_catalog.=) relation) AS named, u.func AS name, u.owner AS owner, u.via AS via
                   FROM (SELECT relation AS relid
                         UNION
                         SELECT comp.relid FROM kingres.internal_fk_component(relation, true) comp
                          WHERE kind OPERATOR(pg_catalog.=) 'undistribute'
                            AND COALESCE((options OPERATOR(pg_catalog.->>) 'cascade_via_foreign_keys')::pg_catalog.bool, false)
                         UNION
                         SELECT d.relid FROM kingres.dist_table d
                          WHERE kind OPERATOR(pg_catalog.=) 'alter' AND d.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND d.method OPERATOR(pg_catalog.=) 'hash'
                            AND COALESCE((options OPERATOR(pg_catalog.->>) 'cascade_to_colocated')::pg_catalog.bool, false)) m
                  CROSS JOIN LATERAL kingres.internal_untrusted_functions(m.relid) u
                  ORDER BY 2 DESC, 1, 3
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'function_owner_not_superuser', r.name,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('function %s, reached from %s%s, belongs to the role "%s", who is not a superuser; the copy would run it with the rights of the caller on every node',
                                                                     r.name, r.via, CASE WHEN r.named THEN '' ELSE pg_catalog.format(' (table %s)', r.tbl) END, r.owner)));
        END LOOP;
    END IF;
    -- the replay and the copy of the online forms write with ON CONFLICT and no conflict target, which PostgreSQL does not accept for a
    -- deferrable unique or primary key constraint (P9C-07)
    IF v_mode OPERATOR(pg_catalog.=) 'online' AND kind IN ('distribute', 'undistribute', 'alter') THEN
        FOR r IN SELECT k.conname FROM pg_catalog.pg_constraint k
                  WHERE k.conrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND k.contype IN ('u', 'p') AND k.condeferrable
                  ORDER BY k.conname
        LOOP
            rep := rep OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_array(
                       'blocker', 'deferrable_unique', r.conname,
                       pfx OPERATOR(pg_catalog.||) pg_catalog.format('constraint "%s" is DEFERRABLE; the online variant writes with ON CONFLICT, which PostgreSQL does not accept for a deferrable constraint; use the blocking variant', r.conname)));
        END LOOP;
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
-- The user partitioning of a table: the method and key (as the shadow's bucket partitions repeat them), then one row per partition
-- with its bound without the words FOR VALUES ("DEFAULT" for a default partition).  No rows for a table that is not partitioned.
CREATE FUNCTION kingres.internal_partition_layout(p_relid regclass)
    RETURNS TABLE (partby pg_catalog.text, ord pg_catalog.int4, name pg_catalog.name, bound pg_catalog.text)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.pg_get_partkeydef(p_relid::pg_catalog.oid),
       (pg_catalog.row_number() OVER (ORDER BY pc.relname))::pg_catalog.int4,
       pc.relname,
       pg_catalog.regexp_replace(pg_catalog.pg_get_expr(pc.relpartbound, pc.oid), '^FOR VALUES ', '')
  FROM pg_catalog.pg_inherits i
  JOIN pg_catalog.pg_class pc ON pc.oid OPERATOR(pg_catalog.=) i.inhrelid
 WHERE i.inhparent OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid
   AND EXISTS (SELECT 1 FROM pg_catalog.pg_partitioned_table t WHERE t.partrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid)
 ORDER BY pc.relname;
$$;

-- The catalog rows of a user-partitioned table that was just made distributed (p_relid: the new table; the layout was read before the swap).
CREATE FUNCTION kingres.internal_set_partitioning(p_relid regclass, p_partby text, p_names text[], p_bounds text[])
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    UPDATE kingres.dist_table d
       SET partition_method = pg_catalog.lower(pg_catalog.split_part(p_partby, ' ', 1)),
           partition_key = pg_catalog.substr(p_partby, pg_catalog.strpos(p_partby, ' ') OPERATOR(pg_catalog.+) 1)
     WHERE d.relid OPERATOR(pg_catalog.=) p_relid;
    INSERT INTO kingres.dist_partition (parent, partition_name, bound)
    SELECT p_relid, n.name, p_bounds[n.ord]
      FROM pg_catalog.unnest(p_names) WITH ORDINALITY AS n(name, ord);
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_partition_layout(regclass), kingres.internal_set_partitioning(regclass, text, text[], text[]) FROM PUBLIC;

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
    kingres.internal_untrusted_functions(regclass),
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
CREATE FUNCTION kingres.internal_shadow_script(p_relid regclass, p_column name, p_colocation int, p_group int, p_partition_base text DEFAULT NULL)
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

-- The same, and the first value of the first row that the statement returns.
CREATE FUNCTION kingres.internal_branch_scalar(p_node int, p_sql text)
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_internal_branch_scalar'
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
    SET extra_float_digits = 3
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
            USING NEW;
    ELSIF TG_OP = 'DELETE' THEN
        EXECUTE pg_catalog.format('INSERT INTO %s (op, %s) SELECT ''D'', %s', v_log, v_cols, v_new)
            USING OLD;
    ELSE
        v_old := pg_catalog.replace(v_new, '$1', '$2');
        EXECUTE pg_catalog.format('INSERT INTO %s (op, %s) SELECT ''U'', %s UNION ALL SELECT ''D'', %s WHERE (%s) IS DISTINCT FROM (%s)',
                                  v_log, v_cols, v_new, v_old, v_old, v_new)
            USING NEW, OLD;
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

-- The name of a type with its domains taken away: the log of the capture stores the keys in the base type, so that
-- no CHECK of a domain runs in the capture trigger (which runs with the rights of the extension's owner).
CREATE FUNCTION kingres.internal_base_type(p_type oid, p_typmod int)
    RETURNS text
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t pg_catalog.record;
BEGIN
    LOOP
        SELECT y.typtype, y.typbasetype, y.typtypmod INTO t FROM pg_catalog.pg_type y WHERE y.oid OPERATOR(pg_catalog.=) p_type;
        EXIT WHEN t.typtype OPERATOR(pg_catalog.<>) 'd';
        p_type := t.typbasetype;
        IF t.typtypmod OPERATOR(pg_catalog.<>) -1 THEN
            p_typmod := t.typtypmod;
        END IF;
    END LOOP;
    RETURN pg_catalog.format_type(p_type, p_typmod);
END;
$$;

-- The row identity of the log: the columns of the primary key, or of the first unique index over
-- NOT NULL columns (no predicate, no expression).
CREATE FUNCTION kingres.internal_log_key(p_relid regclass)
    RETURNS TABLE (key_names text[], key_types text[])
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY k.ord),
       pg_catalog.array_agg(kingres.internal_base_type(a.atttypid, a.atttypmod) ORDER BY k.ord)
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
    v_id    pg_catalog.int8;
    v_dv    pg_catalog.text;
    v_kind  pg_catalog.text := kind;
    v_opts  pg_catalog.jsonb := COALESCE(options, '{}'::pg_catalog.jsonb);
    v_self  pg_catalog.int4;
    v_ms    pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_prevent_xact_block('kingres.begin_table_operation()');
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    IF v_kind NOT IN ('distribute', 'distribute_reference', 'undistribute', 'alter') THEN
        RAISE EXCEPTION 'table operation "%" is not available in this version', v_kind
            USING ERRCODE = 'feature_not_supported',
                  HINT = 'This version has distribute, distribute_reference, undistribute and alter.';
    END IF;
    v_dv := COALESCE(v_opts OPERATOR(pg_catalog.->>) 'dependent_views', pg_catalog.current_setting('kingres_shard.dependent_views', true), 'refuse');
    v_opts := v_opts OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object('dependent_views', v_dv);
    -- the blockers of the report are refused here, in the session of the caller: nothing is recorded
    PERFORM kingres.internal_raise_blockers(v_kind, relation, v_opts OPERATOR(pg_catalog.||) '{"mode": "online"}'::pg_catalog.jsonb);
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') * 4000;
    v_id := kingres.internal_exec_scalar(v_self,
        pg_catalog.format('SELECT kingres.%s(%L, %L::pg_catalog.oid::pg_catalog.regclass, %L::pg_catalog.jsonb)',
                          CASE WHEN v_kind IN ('undistribute', 'alter') THEN 'internal_begin_dist_operation' ELSE 'internal_begin_table_operation' END,
                          v_kind, relation::pg_catalog.oid, v_opts::pg_catalog.text), v_ms)::pg_catalog.int8;
    -- the setting is the operation's: a session that finishes it later does not change what it does with the views
    PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('UPDATE kingres.table_operation SET spec = spec || pg_catalog.jsonb_build_object(''dependent_views'', %L::pg_catalog.text) WHERE op_id = %s RETURNING 1', v_dv, v_id));
    RETURN v_id;
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
    v_exprs := (SELECT pg_catalog.string_agg(pg_catalog.format('($1).%I', v_keys[g]), ', ' ORDER BY g)
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
                                         (v_spec OPERATOR(pg_catalog.->>) 'target_node')::pg_catalog.int4, v_keys, v_last,
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
    v_target   pg_catalog.int4;
    r          pg_catalog.record;
    i          pg_catalog.int4;
BEGIN
    SELECT o.spec INTO v_spec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    v_target := (v_spec OPERATOR(pg_catalog.->>) 'target_node')::pg_catalog.int4;
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
        FOR r IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active
                    AND (v_target IS NULL OR n.node_id OPERATOR(pg_catalog.=) v_target) ORDER BY n.node_id LOOP
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
                                        v_target, NULL, NULL, NULL,
                                        pg_catalog.format('(%s) IN (SELECT %s FROM %s WHERE seq OPERATOR(pg_catalog.=) ANY (%L::pg_catalog.int8[]))',
                                                          v_kcols, v_lcols, v_log, v_seqs),
                                        false);

    -- 3. the entries are done
    EXECUTE pg_catalog.format('DELETE FROM %s WHERE seq OPERATOR(pg_catalog.=) ANY (%L::pg_catalog.int8[])', v_log, v_seqs);
    RETURN v_n;
END;
$$;

-- A chunk of the replay in a session of its own.  A unique index other than the key can fail a chunk: a value that moved
-- from one row to another (the first row's entry is in a later chunk) is held by the old row of the destination.  Every
-- row that holds a stale value is in the log (it changed), so the whole backlog replayed in one chunk cannot conflict
-- with itself: the chunk is repeated with all entries.
CREATE FUNCTION kingres.internal_replay_run(p_self int, p_op bigint, p_batch int, p_ms int, p_pre text DEFAULT '')
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    RETURN kingres.internal_exec_scalar(p_self, p_pre OPERATOR(pg_catalog.||) pg_catalog.format('SELECT kingres.internal_replay_chunk(%s, %s)', p_op, p_batch), p_ms);
EXCEPTION WHEN unique_violation THEN
    RETURN kingres.internal_exec_scalar(p_self, p_pre OPERATOR(pg_catalog.||) pg_catalog.format('SELECT kingres.internal_replay_chunk(%s, %s)', p_op, 2147483647), p_ms);
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
    -- table privileges and column privileges (a role may hold only the second: REVOKE ALL on the table takes both)
    FOR r IN SELECT DISTINCT s.grantee
               FROM (SELECT x.grantee FROM pg_catalog.pg_class c, pg_catalog.aclexplode(c.relacl) x
                      WHERE c.oid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND x.grantee OPERATOR(pg_catalog.<>) c.relowner
                     UNION
                     SELECT x.grantee FROM pg_catalog.pg_attribute a, pg_catalog.pg_class c, pg_catalog.aclexplode(a.attacl) x
                      WHERE a.attrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND a.attacl IS NOT NULL
                        AND c.oid OPERATOR(pg_catalog.=) a.attrelid AND x.grantee OPERATOR(pg_catalog.<>) c.relowner) s
              WHERE s.grantee OPERATOR(pg_catalog.<>) 0
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
    v_views    pg_catalog.jsonb;
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
    -- the views that use the table follow it to the old copy by their OIDs: they are dropped here and made again on the new table
    v_views := kingres.internal_views_capture(ARRAY[v_rel::pg_catalog.text], false, COALESCE(v_rec.spec OPERATOR(pg_catalog.->>) 'dependent_views', 'refuse'));
    PERFORM kingres.internal_views_drop(v_views);
    v_old := kingres.internal_swap_table(p_op, v_rel, v_col, (v_rec.spec OPERATOR(pg_catalog.->>) 'colocation_id')::pg_catalog.int4,
                                         kingres.internal_setting('kingres_shard.table_cleanup_delay')::pg_catalog.int4);
    PERFORM kingres.internal_views_recreate(v_views);
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
    IF (v_rec.spec OPERATOR(pg_catalog.->>) 'src') OPERATOR(pg_catalog.=) 'dist' THEN
        RETURN kingres.internal_op_clean_dist(p_op);
    END IF;
    SELECT * INTO v_item FROM kingres.table_operation_item i WHERE i.op_id OPERATOR(pg_catalog.=) p_op AND i.ordinal OPERATOR(pg_catalog.=) 1;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_new := pg_catalog.format('%I.%I', v_rec.spec OPERATOR(pg_catalog.->>) 'schema', v_rec.spec OPERATOR(pg_catalog.->>) 'table');
    v_old := pg_catalog.format('kingres.%I', v_item.old_name);
    v_ref := v_rec.kind OPERATOR(pg_catalog.=) 'distribute_reference';
    PERFORM kingres.internal_fault_point('rebuild_verify');

    IF NOT pg_catalog.current_setting('kingres_shard.rebuild_verify')::pg_catalog.bool THEN
        UPDATE kingres.table_operation o SET verified = 'skipped', detail = 'kingres_shard.rebuild_verify is off' WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    ELSIF v_item.old_name IS NULL OR pg_catalog.to_regclass(v_old) IS NULL THEN
        UPDATE kingres.table_operation o SET verified = 'skipped', detail = 'the old copy is gone' WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
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
            UPDATE kingres.table_operation o SET verified = 'skipped',
                   detail = pg_catalog.format('the table was written to after the switch (%s rows are newer than the swap); the comparison with the old copy is not possible, and it differs: %s', v_newer, v_why)
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
    PERFORM kingres.internal_op_set(p_op, 'done', (SELECT o.detail FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op AND o.verified OPERATOR(pg_catalog.=) 'skipped'));
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
    IF (v_spec OPERATOR(pg_catalog.->>) 'src') OPERATOR(pg_catalog.=) 'dist' THEN
        RETURN kingres.internal_op_undo_dist(p_op, p_final, p_reason);
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
    v_dist     pg_catalog.bool;
    v_nodes    pg_catalog.int4[];
    v_pre      pg_catalog.text;
    v_node     pg_catalog.int4;
    v_rsum     pg_catalog.int8;
    v_tgt      pg_catalog.int4;
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
            -- a distributed source is followed on every node, each reading the partitions of its own
            v_dist := (v_spec OPERATOR(pg_catalog.->>) 'src') OPERATOR(pg_catalog.=) 'dist';
            v_tgt := (v_spec OPERATOR(pg_catalog.->>) 'target_node')::pg_catalog.int4;
            IF v_dist THEN
                v_pre := 'SET LOCAL kingres_shard.enable_routing = off; ';
                SELECT pg_catalog.array_agg(nd.node_id ORDER BY nd.node_id) INTO v_nodes FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active;
            ELSE
                v_pre := '';
                v_nodes := ARRAY[v_self];
            END IF;
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
                          WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active
                            AND (v_tgt IS NULL OR nd.node_id OPERATOR(pg_catalog.=) v_tgt) ORDER BY nd.node_id
                LOOP
                    v_res := kingres.internal_exec_scalar(n.node_id,
                        pg_catalog.format('SELECT pg_catalog.count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace s ON s.oid OPERATOR(pg_catalog.=) c.relnamespace WHERE s.nspname OPERATOR(pg_catalog.=) %L AND c.relname OPERATOR(pg_catalog.=) %L',
                                          v_spec OPERATOR(pg_catalog.->>) 'schema', v_spec OPERATOR(pg_catalog.->>) 'shadow'), v_ms);
                    IF v_res OPERATOR(pg_catalog.=) '0' THEN
                        -- the script is read in a session of its own: reading the sequences of the table here would
                        -- leave a lock in this transaction that the swap (which renames them) would wait for for ever
                        v_script := kingres.internal_exec_scalar(v_self,
                            pg_catalog.format('SELECT kingres.internal_shadow_script(%L::pg_catalog.oid::pg_catalog.regclass, %L::pg_catalog.name, %L::pg_catalog.int4, %L::pg_catalog.int4, %L::pg_catalog.text)',
                                              v_spec OPERATOR(pg_catalog.->>) 'relid', v_spec OPERATOR(pg_catalog.->>) 'column',
                                              v_spec OPERATOR(pg_catalog.->>) 'colocation_id', n.group_id,
                                              CASE WHEN v_dist THEN v_spec OPERATOR(pg_catalog.->>) 'shadow' END), v_ms);
                        PERFORM kingres.internal_node_exec(n.node_id, v_script, v_chunk_ms);
                    END IF;
                END LOOP;
                PERFORM kingres.internal_fault_point('rebuild_after_shadow');
                FOREACH v_node IN ARRAY v_nodes LOOP
                    IF v_dist AND v_node OPERATOR(pg_catalog.<>) v_self THEN
                        PERFORM kingres.internal_op_mirror(v_op, v_node);
                    END IF;
                    PERFORM kingres.internal_exec_scalar(v_node, v_pre OPERATOR(pg_catalog.||) pg_catalog.format('SELECT kingres.internal_op_capture(%s)', v_op), v_chunk_ms);
                END LOOP;
                PERFORM kingres.internal_fault_point('rebuild_after_capture');
                PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_set(%s, ''copying'', NULL)', v_op), v_ms);

            ELSIF v_phase OPERATOR(pg_catalog.=) 'copying' THEN
                LOOP
                    v_read := 0;
                    FOREACH v_node IN ARRAY v_nodes LOOP
                        v_res := kingres.internal_exec_scalar(v_node, v_pre OPERATOR(pg_catalog.||) pg_catalog.format('SELECT kingres.internal_op_copy_step(%s)', v_op), v_chunk_ms);
                        v_read := v_read OPERATOR(pg_catalog.+) ((v_res::pg_catalog.jsonb) OPERATOR(pg_catalog.->>) 'read')::pg_catalog.int8;
                    END LOOP;
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
                    v_rsum := 0;
                    v_backlog := 0;
                    FOREACH v_node IN ARRAY v_nodes LOOP
                        v_res := kingres.internal_replay_run(v_node, v_op, v_batch, v_chunk_ms, v_pre);
                        v_rsum := v_rsum OPERATOR(pg_catalog.+) v_res::pg_catalog.int4;
                        v_backlog := v_backlog OPERATOR(pg_catalog.+) kingres.internal_exec_scalar(v_node, pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', v_spec OPERATOR(pg_catalog.->>) 'log'), v_ms)::pg_catalog.int8;
                    END LOOP;
                    EXIT WHEN v_backlog OPERATOR(pg_catalog.<=) v_thr;
                    EXIT WHEN v_rsum OPERATOR(pg_catalog.=) 0;
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
                        v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.%s(%s)', CASE WHEN v_dist THEN 'internal_switch_attempt_dist' ELSE 'internal_switch_attempt' END, v_op),
                                                              v_sw_ms OPERATOR(pg_catalog.+) v_ms * 2);
                        v_switched := true;
                    EXCEPTION WHEN OTHERS OR query_canceled THEN
                        -- (the wait of the call for the attempt that runs out is a query_canceled)
                        GET STACKED DIAGNOSTICS v_state = RETURNED_SQLSTATE, v_diag = PG_EXCEPTION_DETAIL;
                        -- a switch that committed although the answer was lost is a switch
                        SELECT o.phase INTO v_phase FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
                        IF v_phase IN ('cleaning', 'done') THEN
                            v_switched := true;
                        ELSIF v_state OPERATOR(pg_catalog.=) '55000'
                              AND EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op AND o.cancel_requested) THEN
                            -- the attempt saw the request to stop: the loop below sees it too
                            NULL;
                        ELSIF v_state OPERATOR(pg_catalog.<>) '55P03' AND v_state OPERATOR(pg_catalog.<>) '23505' THEN
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
                    FOREACH v_node IN ARRAY v_nodes LOOP
                        v_res := kingres.internal_replay_run(v_node, v_op, v_batch, v_chunk_ms, v_pre);
                    END LOOP;
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
                SELECT o.detail INTO v_detail FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op AND o.verified OPERATOR(pg_catalog.=) 'skipped';
                IF FOUND THEN
                    RAISE WARNING 'the verification of table operation % was not done: %', v_op, COALESCE(v_detail, 'no reason was recorded')
                        USING HINT = 'Compare the old copy (kingres.table_operations names it) with the new table before it is dropped.';
                END IF;
            END IF;
        END LOOP;
    EXCEPTION WHEN OTHERS OR query_canceled THEN
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
    EXCEPTION WHEN OTHERS OR query_canceled THEN
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
    -- the old copies of an operation on a distributed table are on every node: the others are dropped from here
    FOR r IN SELECT o.op_id, o.spec, o.verified FROM kingres.table_operation o
              WHERE (o.spec OPERATOR(pg_catalog.->>) 'src') OPERATOR(pg_catalog.=) 'dist' AND o.phase IN ('cleaning', 'done')
                AND NOT COALESCE((o.spec OPERATOR(pg_catalog.->>) 'old_dropped')::pg_catalog.bool, false)
                AND (v_force OR (o.cleanup_after IS NOT NULL AND o.cleanup_after OPERATOR(pg_catalog.<=) pg_catalog.clock_timestamp() AND o.verified IS DISTINCT FROM 'failed'))
              ORDER BY o.op_id
    LOOP
        IF NOT v_force AND r.verified IS NULL THEN
            CONTINUE;
        END IF;
        PERFORM kingres.internal_fault_point('rebuild_cleanup');
        DECLARE
            e   pg_catalog.record;
            v_self pg_catalog.int4 := (SELECT l.node_id FROM kingres.local_node l);
            v_all pg_catalog.bool := true;
        BEGIN
            FOR e IN SELECT x.key AS node_id, x.value #>> '{}' AS old FROM pg_catalog.jsonb_each(r.spec OPERATOR(pg_catalog.->) 'old_copies') x LOOP
                IF e.node_id::pg_catalog.int4 OPERATOR(pg_catalog.<>) v_self THEN
                    BEGIN
                        PERFORM kingres.internal_node_exec(e.node_id::pg_catalog.int4, pg_catalog.format(
                            'SET LOCAL kingres_shard.enable_routing = off; DROP TABLE IF EXISTS %s; DELETE FROM kingres.table_old_copy WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c WHERE c.oid = relid::pg_catalog.oid)', e.old));
                    EXCEPTION WHEN OTHERS OR query_canceled THEN
                        v_all := false;
                    END;
                END IF;
            END LOOP;
            IF v_all THEN
                UPDATE kingres.table_operation o SET spec = o.spec OPERATOR(pg_catalog.||) '{"old_dropped": true}'::pg_catalog.jsonb WHERE o.op_id OPERATOR(pg_catalog.=) r.op_id;
                n := n OPERATOR(pg_catalog.+) 1;
            END IF;
        END;
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
    EXCEPTION WHEN OTHERS OR query_canceled THEN
        -- what did not switch is undone (an operation that switched answers 'switched')
        BEGIN
            PERFORM kingres.internal_exec_scalar(v_self,
                pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''failed'', %L)', v_op, SQLERRM), v_ms);
        EXCEPTION WHEN OTHERS OR query_canceled THEN
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
    EXCEPTION WHEN OTHERS OR query_canceled THEN
        BEGIN
            PERFORM kingres.internal_exec_scalar(v_self,
                pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''failed'', %L)', v_op, SQLERRM), v_ms);
        EXCEPTION WHEN OTHERS OR query_canceled THEN
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
    kingres.internal_shadow_script(regclass, name, int, int, text),
    kingres.internal_swap_table(bigint, regclass, name, int, int),
    kingres.internal_copy_chunk(regclass, name, name, text, name, int, int, text[], text[], bigint, text, bool),
    kingres.internal_branch_exec(int, text),
    kingres.internal_branch_scalar(int, text),
    kingres.internal_capture_trigger(),
    kingres.internal_capture_truncate(),
    kingres.internal_lock_holders(regclass),
    kingres.internal_setting(text),
    kingres.internal_lock_table(regclass, text),
    kingres.internal_op_set(bigint, text, text),
    kingres.internal_op_spec(bigint, jsonb),
    kingres.internal_log_key(regclass),
    kingres.internal_base_type(oid, int),
    kingres.internal_begin_table_operation(text, regclass, jsonb),
    kingres.begin_table_operation(text, regclass, jsonb),
    kingres.internal_op_capture(bigint),
    kingres.internal_op_copy_step(bigint),
    kingres.internal_replay_chunk(bigint, int),
    kingres.internal_replay_run(int, bigint, int, int, text),
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

    FOR o IN SELECT t.op_id, t.phase, t.updated_at, t.verified FROM kingres.table_operation t WHERE NOT (t.spec OPERATOR(pg_catalog.?) 'mirror') ORDER BY t.op_id LOOP
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

-- ---- user partitions: where the leaves are, and what check_cluster() says about them ----

-- The leaves of one user partition: the node and the bucket of each, from the catalog (the leaf is <partition>_b<NNNN> in the schema of the table).
CREATE FUNCTION kingres.partition_leaves(parent regclass, partition_name text)
    RETURNS TABLE (node_id int, bucket int, leaf text)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT n.node_id, m.bucket,
       pg_catalog.format('%I.%I', s.nspname, p.partition_name OPERATOR(pg_catalog.||) '_b' OPERATOR(pg_catalog.||) pg_catalog.lpad(m.bucket::pg_catalog.text, 4, '0'))
  FROM kingres.dist_partition p
  JOIN kingres.dist_table d ON d.relid OPERATOR(pg_catalog.=) p.parent
  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) p.parent::pg_catalog.oid
  JOIN pg_catalog.pg_namespace s ON s.oid OPERATOR(pg_catalog.=) c.relnamespace
  JOIN kingres.bucket_map m ON m.colocation_id OPERATOR(pg_catalog.=) d.colocation_id
  JOIN kingres.node n ON n.group_id OPERATOR(pg_catalog.=) m.group_id AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active
 WHERE p.parent OPERATOR(pg_catalog.=) partition_leaves.parent AND p.partition_name OPERATOR(pg_catalog.=) partition_leaves.partition_name
 ORDER BY m.bucket;
$$;
COMMENT ON FUNCTION kingres.partition_leaves(regclass, text) IS 'The node, bucket and name of every leaf of a user partition of a distributed table';

-- dist_partition_mismatch: a leaf that the catalog says exists is missing on the node that owns its bucket (made by hand outside the extension, or a failed recovery)
CREATE FUNCTION kingres.internal_check_partitions()
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    n pg_catalog.record;
    r pg_catalog.jsonb;
    x pg_catalog.record;
BEGIN
    IF pg_catalog.to_regclass('kingres.dist_partition') IS NULL OR NOT EXISTS (SELECT 1 FROM kingres.dist_partition) THEN
        RETURN;
    END IF;
    FOR n IN SELECT nd.node_id AS id, nd.group_id AS grp FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active ORDER BY nd.node_id LOOP
        BEGIN
            r := kingres.internal_node_exec(n.id,
                'SELECT COALESCE(pg_catalog.jsonb_agg(c.relname), ''[]''::pg_catalog.jsonb) FROM pg_catalog.pg_class c WHERE c.relkind OPERATOR(pg_catalog.=) ''r'' AND c.relispartition AND c.relname OPERATOR(pg_catalog.~) ''_b[0-9]{4}$''');
            r := (r OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            CONTINUE;
        END;
        FOR x IN SELECT p.parent::pg_catalog.text AS tbl, l.leaf AS leaf, l.bucket AS bucket
                   FROM kingres.dist_partition p
                   CROSS JOIN LATERAL kingres.partition_leaves(p.parent, p.partition_name) l
                  WHERE l.node_id OPERATOR(pg_catalog.=) n.id
                  ORDER BY p.parent::pg_catalog.text, p.partition_name, l.bucket
        LOOP
            -- the name of the leaf as it is in pg_class: parse_ident takes quotes, doubled quotes and dots in a name apart (P9C-11)
            IF NOT (r OPERATOR(pg_catalog.?) (pg_catalog.parse_ident(x.leaf))[pg_catalog.cardinality(pg_catalog.parse_ident(x.leaf))]) THEN
                check_name := 'dist_partition_mismatch'; node_id := n.id; object := x.leaf;
                detail := pg_catalog.format('the leaf of bucket %s of a partition of %s is missing on the node that owns the bucket', x.bucket, x.tbl);
                RETURN NEXT;
            END IF;
        END LOOP;
    END LOOP;
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_check_partitions() FROM PUBLIC;

CREATE FUNCTION kingres.check_cluster(p_prepared_older_than interval DEFAULT interval '5 minutes')
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    RETURN QUERY SELECT b.check_name, b.node_id, b.object, b.detail FROM kingres.internal_check_cluster_base(p_prepared_older_than) b;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_table_operations() t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_partitions() t;
END;
$$;

COMMENT ON FUNCTION kingres.check_cluster(interval) IS
    'One row per inconsistency of the cluster (catalogs, partitions against the bucket map, leftovers of moves and of table operations, old prepared transactions); none when consistent';

REVOKE ALL ON FUNCTION
    kingres.check_cluster(interval),
    kingres.internal_check_cluster_base(interval),
    kingres.internal_check_table_operations()
    FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- Increment 9.4: undistribute (design/shard-phase9.md, "Feature 2")
-- ---------------------------------------------------------------------------

-- The tables that an undistribute takes with it: the table itself, or with cascade the connected component of the tables of the
-- catalog that foreign keys link to it (a key from a distributed table to a reference table links them too).
CREATE FUNCTION kingres.internal_fk_component(p_relid regclass, p_cascade bool)
    RETURNS TABLE (relid regclass, method text, colocation_id int)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
WITH RECURSIVE comp(oid) AS (
    SELECT p_relid::pg_catalog.oid
    UNION
    SELECT CASE WHEN k.conrelid OPERATOR(pg_catalog.=) c.oid THEN k.confrelid ELSE k.conrelid END
      FROM comp c
      JOIN pg_catalog.pg_constraint k ON k.contype OPERATOR(pg_catalog.=) 'f' AND k.conparentid OPERATOR(pg_catalog.=) 0
                                     AND k.conrelid OPERATOR(pg_catalog.<>) k.confrelid
                                     AND (k.conrelid OPERATOR(pg_catalog.=) c.oid OR k.confrelid OPERATOR(pg_catalog.=) c.oid)
     WHERE p_cascade
       AND EXISTS (SELECT 1 FROM kingres.dist_table d
                    WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) CASE WHEN k.conrelid OPERATOR(pg_catalog.=) c.oid THEN k.confrelid ELSE k.conrelid END)
)
SELECT d.relid, d.method, d.colocation_id
  FROM comp c JOIN kingres.dist_table d ON d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) c.oid
 ORDER BY (d.method OPERATOR(pg_catalog.=) 'reference'), d.relid::pg_catalog.oid;
$$;

-- Run a statement on a node: here, or in the branch of the current transaction on another node.
CREATE FUNCTION kingres.internal_run_on(p_node int, p_sql text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    IF p_node OPERATOR(pg_catalog.=) (SELECT l.node_id FROM kingres.local_node l) THEN
        EXECUTE p_sql;
    ELSE
        PERFORM kingres.internal_branch_exec(p_node, p_sql);
    END IF;
END;
$$;

-- The blocking undistribute: one transaction, the caller's.  Writers of the tables wait for the whole copy (EXCLUSIVE on every
-- node), readers do not.  A distributed table is copied through the distributed scan into an ordinary shadow on the target node,
-- the distributed table is dropped on every node, and the shadow takes its name; a reference table stays on the target node (the
-- same table, so views, grants and keys that depend on it stay) and is dropped on the others.
CREATE FUNCTION kingres.undistribute_table(relation regclass, cascade_via_foreign_keys bool DEFAULT false, target_group int DEFAULT NULL)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    SET client_min_messages = warning
    AS $$
DECLARE
    v_views   pg_catalog.jsonb;
    v_ver     pg_catalog.text;
    v_self    pg_catalog.int4;
    v_tgroup  pg_catalog.int4 := COALESCE(target_group, 0);
    v_target  pg_catalog.int4;
    v_routing pg_catalog.text := COALESCE(pg_catalog.current_setting('kingres_shard.enable_routing', true), 'on');
    t         pg_catalog.record;
    nd        pg_catalog.record;
    s         pg_catalog.record;
    x         pg_catalog.text;
    v_ddl     pg_catalog.jsonb;
    v_hash    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_ref     pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_groups  pg_catalog.int4[] := ARRAY[]::pg_catalog.int4[];
    v_cols    pg_catalog.text;
    v_old     pg_catalog.int8;
    v_new     pg_catalog.int8;
    v_qname   pg_catalog.text;
    v_shadow  pg_catalog.text;
    v_nsp     pg_catalog.text;
    g         pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT e.extversion INTO v_ver FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard';
    PERFORM kingres.internal_require_version(v_ver);
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    PERFORM kingres.internal_require_read_committed('kingres.undistribute_table()');
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_raise_blockers('undistribute', relation,
        pg_catalog.jsonb_strip_nulls(pg_catalog.jsonb_build_object('cascade_via_foreign_keys', cascade_via_foreign_keys, 'target_group', target_group)));
    v_target := kingres.internal_group_node(v_tgroup);
    IF v_target IS NULL THEN
        RAISE EXCEPTION 'shard group % has no active primary', v_tgroup USING ERRCODE = 'invalid_parameter_value';
    END IF;

    -- the tables, and no group of them that a move or an operation uses
    FOR t IN SELECT c.relid, c.method, c.colocation_id FROM kingres.internal_fk_component(relation, cascade_via_foreign_keys) c LOOP
        IF t.method OPERATOR(pg_catalog.=) 'hash' THEN
            v_hash := v_hash || t.relid::pg_catalog.text;
            IF NOT (t.colocation_id OPERATOR(pg_catalog.=) ANY (v_groups)) THEN
                v_groups := v_groups || t.colocation_id;
            END IF;
        ELSE
            v_ref := v_ref || t.relid::pg_catalog.text;
        END IF;
    END LOOP;

    -- what the new tables need, from the old ones, before they are gone
    SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('t', h.x, 'scope', d.scope, 'ord', d.ord, 'stmt', d.stmt) ORDER BY h.i, d.ord)
      INTO v_ddl
      FROM pg_catalog.unnest(v_hash) WITH ORDINALITY h(x, i)
      CROSS JOIN LATERAL kingres.internal_table_ddl(h.x::pg_catalog.regclass, NULL, true) d;

    -- writers wait, on every node, in node order
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        FOREACH x IN ARRAY (v_hash || v_ref) LOOP
            PERFORM kingres.internal_run_on(nd.node_id, pg_catalog.format('LOCK TABLE %s IN EXCLUSIVE MODE', x));
        END LOOP;
    END LOOP;

    PERFORM kingres.internal_fault_point('undistribute_after_lock');

    -- the shadows of the distributed tables on the target node, and the rows into them through the distributed scan
    FOREACH x IN ARRAY v_hash LOOP
        FOR t IN SELECT e->>'stmt' AS stmt FROM pg_catalog.jsonb_array_elements(v_ddl) e
                  WHERE (e->>'t') OPERATOR(pg_catalog.=) x AND (e->>'scope') IN ('remote_pre', 'local_pre', 'both')
                  ORDER BY (e->>'ord')::pg_catalog.int4 LOOP
            PERFORM kingres.internal_run_on(v_target, t.stmt);
        END LOOP;
        SELECT n.nspname INTO v_nsp FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
         WHERE c.oid OPERATOR(pg_catalog.=) x::pg_catalog.regclass::pg_catalog.oid;
        v_shadow := pg_catalog.format('%I.%I', v_nsp, pg_catalog.format('kingres_shadow_%s', x::pg_catalog.regclass::pg_catalog.oid));
        SELECT pg_catalog.string_agg(pg_catalog.quote_ident(a.attname), ', ' ORDER BY a.attnum) INTO v_cols
          FROM pg_catalog.pg_attribute a
         WHERE a.attrelid OPERATOR(pg_catalog.=) x::pg_catalog.regclass::pg_catalog.oid AND a.attnum OPERATOR(pg_catalog.>) 0
           AND NOT a.attisdropped AND a.attgenerated OPERATOR(pg_catalog.=) '';
        EXECUTE pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', x) INTO v_old;
        IF v_target OPERATOR(pg_catalog.=) v_self THEN
            EXECUTE pg_catalog.format('INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s', v_shadow, v_cols, v_cols, x);
            EXECUTE pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', v_shadow) INTO v_new;
        ELSE
            -- a session of another node runs a statement on its own partitions only, so the rows are read here (through the
            -- distributed scan) and sent to the target node, in its branch of this transaction
            v_new := kingres.internal_copy_routed(x::pg_catalog.regclass, v_nsp::pg_catalog.name,
                         pg_catalog.format('kingres_shadow_%s', x::pg_catalog.regclass::pg_catalog.oid)::pg_catalog.name, 'one_node', NULL, NULL, v_target);
        END IF;
        -- the rows that were read are the rows that were stored
        IF v_new IS DISTINCT FROM v_old THEN
            RAISE EXCEPTION 'the copy of table % is not complete: the table holds % rows, % were stored', x, v_old, v_new
                USING ERRCODE = 'data_exception';
        END IF;
    END LOOP;

    PERFORM kingres.internal_fault_point('undistribute_after_copy');
    v_views := kingres.internal_views_capture(v_hash || v_ref, true, pg_catalog.current_setting('kingres_shard.dependent_views', true), v_target);

    -- from here the statements of the extension are written by hand on every node
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    PERFORM kingres.internal_views_drop(v_views);
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        IF nd.node_id OPERATOR(pg_catalog.<>) v_self THEN
            PERFORM kingres.internal_branch_exec(nd.node_id, 'SET LOCAL kingres_shard.enable_routing = off');
        END IF;
        IF pg_catalog.cardinality(v_hash) OPERATOR(pg_catalog.>) 0 THEN
            PERFORM kingres.internal_run_on(nd.node_id, pg_catalog.format('DROP TABLE %s', pg_catalog.array_to_string(v_hash, ', ')));
        END IF;
        IF nd.node_id OPERATOR(pg_catalog.<>) v_target AND pg_catalog.cardinality(v_ref) OPERATOR(pg_catalog.>) 0 THEN
            PERFORM kingres.internal_run_on(nd.node_id, pg_catalog.format('DROP TABLE %s', pg_catalog.array_to_string(v_ref, ', ')));
        END IF;
    END LOOP;
    -- the catalog: the tables are not in it any more, a group without a table is gone (its id is not used again)
    DELETE FROM kingres.dist_table d WHERE d.relid::pg_catalog.oid::pg_catalog.regclass::pg_catalog.text OPERATOR(pg_catalog.=) ANY (v_ref);
    FOREACH g IN ARRAY v_groups LOOP
        IF NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) g)
           AND NOT EXISTS (SELECT 1 FROM kingres.bucket_move m WHERE m.colocation_id OPERATOR(pg_catalog.=) g) THEN
            DELETE FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) g;
            DELETE FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) g;
        END IF;
    END LOOP;

    PERFORM kingres.internal_fault_point('undistribute_after_drop');
    -- the shadows take the names, then the keys, then the sequence values, privileges and comments
    FOREACH x IN ARRAY ARRAY['swap', 'fk', 'post'] LOOP
        FOR t IN SELECT e->>'stmt' AS stmt FROM pg_catalog.jsonb_array_elements(v_ddl) e
                  WHERE CASE x WHEN 'post' THEN (e->>'scope') IN ('post', 'post_local') ELSE (e->>'scope') OPERATOR(pg_catalog.=) x END
                  ORDER BY (SELECT h.i FROM pg_catalog.unnest(v_hash) WITH ORDINALITY h(y, i) WHERE h.y OPERATOR(pg_catalog.=) (e->>'t')), (e->>'ord')::pg_catalog.int4 LOOP
            PERFORM kingres.internal_run_on(v_target, t.stmt);
        END LOOP;
    END LOOP;
    PERFORM kingres.internal_views_recreate(v_views);
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_routing, true);
    PERFORM kingres.sync_catalog();
    PERFORM kingres.internal_fault_point('undistribute_after_swap');
END;
$$;

COMMENT ON FUNCTION kingres.undistribute_table(regclass, bool, int) IS
    'Turn a distributed or reference table (with the tables its foreign keys link to it, when asked) back into an ordinary table on one node; writers wait for the copy';

REVOKE ALL ON FUNCTION
    kingres.internal_fk_component(regclass, bool),
    kingres.internal_run_on(int, text),
    kingres.undistribute_table(regclass, bool, int)
    FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- Increment 9.4: alter_distributed_table (design/shard-phase9.md, "Feature 3")
-- ---------------------------------------------------------------------------

-- A new colocation group (its id is never used again) with p_buckets buckets.  When p_like is given and p_buckets is a multiple of
-- its bucket count, new bucket b goes to the group that owns old bucket (b mod count): the rows of the old bucket b mod count are the
-- rows of the new bucket b, so a rebuild moves no row between nodes.  Otherwise the buckets are spread round-robin over the
-- groups that should hold shards, as create_distributed_table() does.
CREATE FUNCTION kingres.internal_new_colocation(p_type regtype, p_collation oid, p_buckets int, p_like int DEFAULT NULL)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_id     pg_catalog.int4 := pg_catalog.nextval('kingres.colocation_id_seq')::pg_catalog.int4;
    v_old    pg_catalog.int4;
    v_groups pg_catalog.int4[];
BEGIN
    INSERT INTO kingres.colocation VALUES (v_id, p_buckets, p_type, p_collation);
    IF p_like IS NOT NULL THEN
        SELECT c.bucket_count INTO v_old FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) p_like;
    END IF;
    IF v_old IS NOT NULL AND p_buckets OPERATOR(pg_catalog.%) v_old OPERATOR(pg_catalog.=) 0 THEN
        INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id)
        SELECT v_id, g, (SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) p_like AND m.bucket OPERATOR(pg_catalog.=) (g OPERATOR(pg_catalog.%) v_old))
          FROM pg_catalog.generate_series(0, p_buckets OPERATOR(pg_catalog.-) 1) g;
    ELSE
        SELECT pg_catalog.array_agg(DISTINCT n.group_id ORDER BY n.group_id) INTO v_groups
          FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND n.should_have_shards;
        IF v_groups IS NULL THEN
            RAISE EXCEPTION 'no shard group can hold buckets' USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'Register a node with kingres.add_node() or set should_have_shards.';
        END IF;
        INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id)
        SELECT v_id, g, v_groups[1 OPERATOR(pg_catalog.+) (g OPERATOR(pg_catalog.%) pg_catalog.cardinality(v_groups))]
          FROM pg_catalog.generate_series(0, p_buckets OPERATOR(pg_catalog.-) 1) g;
    END IF;
    RETURN v_id;
END;
$$;

-- On this node: the bucket partitions of a distributed table and their indexes get names that carry the OID of the table, so that the
-- partitions of its replacement can have the names these have now (they are dropped with the table).
CREATE FUNCTION kingres.internal_retire_partitions(p_parent regclass)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r pg_catalog.record;
    i pg_catalog.record;
    k pg_catalog.int4;
    v_suffix pg_catalog.text;
BEGIN
    FOR r IN SELECT c.oid AS poid, n.nspname, c.relname FROM pg_catalog.pg_inherits h
               JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) h.inhrelid
               JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
              WHERE h.inhparent OPERATOR(pg_catalog.=) p_parent::pg_catalog.oid ORDER BY c.relname
    LOOP
        v_suffix := pg_catalog.substring(r.relname, '_b[0-9]{4}$');
        k := 0;
        FOR i IN SELECT ic.relname FROM pg_catalog.pg_index x JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) x.indexrelid
                  WHERE x.indrelid OPERATOR(pg_catalog.=) r.poid ORDER BY x.indexrelid
        LOOP
            k := k OPERATOR(pg_catalog.+) 1;
            EXECUTE pg_catalog.format('ALTER INDEX %I.%I RENAME TO %I', r.nspname, i.relname,
                                      pg_catalog.format('kingres_old_%s%s_i%s', p_parent::pg_catalog.oid, v_suffix, k));
        END LOOP;
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I RENAME TO %I', r.nspname, r.relname, pg_catalog.format('kingres_old_%s%s', p_parent::pg_catalog.oid, v_suffix));
    END LOOP;
END;
$$;

-- The blocking alter: one transaction, the caller's.  See "Feature 3" of the design.  Writers of the tables wait (EXCLUSIVE on every
-- node), readers do not.
CREATE FUNCTION kingres.alter_distributed_table(relation regclass, distribution_column name DEFAULT NULL, bucket_count int DEFAULT NULL,
                                                colocate_with text DEFAULT NULL, cascade_to_colocated bool DEFAULT false)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    SET client_min_messages = warning
    AS $$
#variable_conflict use_variable
DECLARE
    v_ro      pg_catalog.jsonb;
    v_views   pg_catalog.jsonb;
    v_ver      pg_catalog.text;
    v_self     pg_catalog.int4;
    v_routing  pg_catalog.text := COALESCE(pg_catalog.current_setting('kingres_shard.enable_routing', true), 'on');
    v_opts     pg_catalog.jsonb;
    v_dt       kingres.dist_table%ROWTYPE;
    v_old      kingres.colocation%ROWTYPE;
    v_to       kingres.dist_table%ROWTYPE;
    v_cwith    pg_catalog.regclass;
    v_col      pg_catalog.name;
    v_type     pg_catalog.oid;
    v_coll     pg_catalog.oid;
    v_n        pg_catalog.int4;
    v_new      pg_catalog.int4;
    v_same     pg_catalog.bool;
    v_set      pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_ddl      pg_catalog.jsonb;
    v_nsp      pg_catalog.text;
    v_shadow   pg_catalog.text;
    v_oldrows  pg_catalog.int8;
    v_rows     pg_catalog.int8;
    nd         pg_catalog.record;
    t          pg_catalog.record;
    x          pg_catalog.text;
    y          pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT e.extversion INTO v_ver FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard';
    PERFORM kingres.internal_require_version(v_ver);
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    PERFORM kingres.internal_require_read_committed('kingres.alter_distributed_table()');
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    v_opts := pg_catalog.jsonb_strip_nulls(pg_catalog.jsonb_build_object('distribution_column', distribution_column, 'bucket_count', bucket_count,
                  'colocate_with', colocate_with, 'cascade_to_colocated', cascade_to_colocated));
    PERFORM kingres.internal_raise_blockers('alter', relation, v_opts);
    SELECT * INTO v_dt FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) relation;
    SELECT * INTO v_old FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id;
    IF colocate_with IS NOT NULL AND colocate_with NOT IN ('default', 'none') THEN
        v_cwith := pg_catalog.to_regclass(colocate_with);
        SELECT * INTO v_to FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_cwith;
    END IF;

    -- the tables that change: this one, or all of its group
    IF cascade_to_colocated THEN
        SELECT pg_catalog.array_agg(d.relid::pg_catalog.text ORDER BY d.relid::pg_catalog.oid) INTO v_set
          FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND d.method OPERATOR(pg_catalog.=) 'hash';
    ELSE
        v_set := ARRAY[relation::pg_catalog.text];
    END IF;
    v_col := COALESCE(distribution_column, v_dt.dist_column);
    IF distribution_column IS NOT NULL AND distribution_column OPERATOR(pg_catalog.<>) v_dt.dist_column THEN
        -- every table of the set needs a column of that name and the type of the first
        SELECT a.atttypid, a.attcollation INTO v_type, v_coll FROM pg_catalog.pg_attribute a
         WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col AND NOT a.attisdropped;
        FOREACH x IN ARRAY v_set LOOP
            IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) x::pg_catalog.regclass::pg_catalog.oid
                              AND a.attname OPERATOR(pg_catalog.=) v_col AND a.atttypid OPERATOR(pg_catalog.=) v_type AND NOT a.attisdropped) THEN
                RAISE EXCEPTION 'table % has no column "%" of type %', x, v_col, pg_catalog.format_type(v_type, NULL) USING ERRCODE = 'undefined_column';
            END IF;
        END LOOP;
    ELSE
        v_type := v_old.dist_column_type::pg_catalog.oid;
        v_coll := v_old.dist_collation;
    END IF;
    v_n := COALESCE(bucket_count, v_old.bucket_count);

    -- what has to happen
    IF v_cwith IS NOT NULL THEN
        IF v_to.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND v_col OPERATOR(pg_catalog.=) v_dt.dist_column AND v_n OPERATOR(pg_catalog.=) v_old.bucket_count THEN
            RETURN;     -- already there
        END IF;
        SELECT c.dist_column_type::pg_catalog.oid, c.dist_collation, c.bucket_count INTO v_type, v_coll, v_new FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_to.colocation_id;
        IF v_type IS DISTINCT FROM (SELECT a.atttypid FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col) THEN
            RAISE EXCEPTION 'cannot colocate with table "%": the distribution column types differ', v_cwith USING ERRCODE = 'datatype_mismatch';
        END IF;
        IF bucket_count IS NOT NULL AND bucket_count OPERATOR(pg_catalog.<>) v_new THEN
            RAISE EXCEPTION 'bucket_count % differs from the % buckets of table "%"', bucket_count, v_new, v_cwith USING ERRCODE = 'invalid_parameter_value';
        END IF;
        v_n := v_new;
        v_new := v_to.colocation_id;
        -- a group with the same placement: the catalog changes, the tables do not
        SELECT NOT EXISTS (SELECT 1 FROM kingres.bucket_map a FULL JOIN kingres.bucket_map b
                              ON a.bucket OPERATOR(pg_catalog.=) b.bucket AND b.colocation_id OPERATOR(pg_catalog.=) v_to.colocation_id
                           WHERE a.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND (b.bucket IS NULL OR a.group_id OPERATOR(pg_catalog.<>) b.group_id))
               AND v_col OPERATOR(pg_catalog.=) v_dt.dist_column AND v_n OPERATOR(pg_catalog.=) v_old.bucket_count
          INTO v_same;
    ELSE
        v_new := NULL;
        IF v_col OPERATOR(pg_catalog.=) v_dt.dist_column AND v_n OPERATOR(pg_catalog.=) v_old.bucket_count THEN
            v_same := true;                 -- a catalog change only: the table leaves its group, or joins the default one
            IF colocate_with IS NULL THEN
                RETURN;
            END IF;
        ELSE
            v_same := false;
        END IF;
    END IF;

    IF v_same THEN
        -- ---- the catalog only ----
        IF v_new IS NULL THEN
            IF colocate_with OPERATOR(pg_catalog.=) 'default' THEN
                SELECT c.colocation_id INTO v_new FROM kingres.colocation c
                 WHERE c.dist_column_type OPERATOR(pg_catalog.=) v_old.dist_column_type AND c.dist_collation OPERATOR(pg_catalog.=) v_old.dist_collation
                   AND c.bucket_count OPERATOR(pg_catalog.=) v_old.bucket_count AND c.colocation_id OPERATOR(pg_catalog.<>) v_dt.colocation_id
                   AND kingres.internal_group_busy(c.colocation_id) IS NULL
                   AND NOT (SELECT pg_catalog.bool_or(true) FROM kingres.bucket_map a JOIN kingres.bucket_map b ON a.bucket OPERATOR(pg_catalog.=) b.bucket
                             WHERE a.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND b.colocation_id OPERATOR(pg_catalog.=) c.colocation_id
                               AND a.group_id OPERATOR(pg_catalog.<>) b.group_id) IS TRUE
                 ORDER BY c.colocation_id LIMIT 1;
                IF v_new IS NULL THEN
                    RETURN;     -- no other group of the same kind and placement
                END IF;
            ELSE
                v_new := kingres.internal_new_colocation(v_old.dist_column_type, v_old.dist_collation, v_old.bucket_count, v_dt.colocation_id);
            END IF;
        END IF;
        UPDATE kingres.dist_table d SET colocation_id = v_new WHERE d.relid::pg_catalog.text OPERATOR(pg_catalog.=) ANY (v_set);
        IF NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id)
           AND NOT EXISTS (SELECT 1 FROM kingres.bucket_move m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id) THEN
            DELETE FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id;
            DELETE FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id;
        END IF;
        PERFORM kingres.sync_catalog();
        RETURN;
    END IF;

    -- ---- a rebuild ----
    IF v_new IS NULL AND colocate_with OPERATOR(pg_catalog.=) 'default' THEN
        SELECT c.colocation_id INTO v_new FROM kingres.colocation c
         WHERE c.dist_column_type OPERATOR(pg_catalog.=) v_type::pg_catalog.regtype AND c.dist_collation OPERATOR(pg_catalog.=) v_coll AND c.bucket_count OPERATOR(pg_catalog.=) v_n
           AND c.colocation_id OPERATOR(pg_catalog.<>) v_dt.colocation_id
           AND kingres.internal_group_busy(c.colocation_id) IS NULL
         ORDER BY c.colocation_id LIMIT 1;
    END IF;
    IF v_new IS NULL THEN
        v_new := kingres.internal_new_colocation(v_type::pg_catalog.regtype, v_coll, v_n, v_dt.colocation_id);
    END IF;

    -- what the new tables need, from the old ones, before they are gone
    SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('t', h.x, 'scope', d.scope, 'ord', d.ord, 'stmt', d.stmt) ORDER BY h.i, d.ord)
      INTO v_ddl
      FROM pg_catalog.unnest(v_set) WITH ORDINALITY h(x, i)
      CROSS JOIN LATERAL kingres.internal_table_ddl(h.x::pg_catalog.regclass, v_col, true) d;

    -- writers wait, on every node, in node order
    FOR nd IN SELECT n.node_id, n.group_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        FOREACH x IN ARRAY v_set LOOP
            PERFORM kingres.internal_run_on(nd.node_id, pg_catalog.format('LOCK TABLE %s IN EXCLUSIVE MODE', x));
        END LOOP;
    END LOOP;

    PERFORM kingres.internal_fault_point('alter_after_lock');
    -- the old partitions give their names away, the shadows are made with the final names of their partitions, the rows go to their buckets
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    FOR nd IN SELECT n.node_id, n.group_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        IF nd.node_id OPERATOR(pg_catalog.<>) v_self THEN
            PERFORM kingres.internal_branch_exec(nd.node_id, 'SET LOCAL kingres_shard.enable_routing = off');
        END IF;
        FOREACH x IN ARRAY v_set LOOP
            PERFORM kingres.internal_run_on(nd.node_id, pg_catalog.format('SELECT kingres.internal_retire_partitions(%L::pg_catalog.regclass)', x));
            PERFORM kingres.internal_run_on(nd.node_id,
                kingres.internal_shadow_script(x::pg_catalog.regclass, v_col, v_new, nd.group_id));
        END LOOP;
    END LOOP;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'on', true);
    FOREACH x IN ARRAY v_set LOOP
        SELECT n.nspname INTO v_nsp FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
         WHERE c.oid OPERATOR(pg_catalog.=) x::pg_catalog.regclass::pg_catalog.oid;
        EXECUTE pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', x) INTO v_oldrows;
        v_rows := kingres.internal_copy_routed(x::pg_catalog.regclass, v_nsp::pg_catalog.name,
                      pg_catalog.format('kingres_shadow_%s', x::pg_catalog.regclass::pg_catalog.oid)::pg_catalog.name, 'placement', v_col, v_new);
        IF v_rows IS DISTINCT FROM v_oldrows THEN
            RAISE EXCEPTION 'the copy of table % is not complete: the table holds % rows, % were stored', x, v_oldrows, v_rows USING ERRCODE = 'data_exception';
        END IF;
    END LOOP;

    PERFORM kingres.internal_fault_point('alter_after_copy');
    -- the swap
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    v_views := kingres.internal_views_capture(v_set, true, pg_catalog.current_setting('kingres_shard.dependent_views', true));
    -- the storage parameters of the old tables, to record them for the new ones
    SELECT pg_catalog.jsonb_object_agg(x, pg_catalog.to_jsonb(kingres.internal_reloptions_of(x::pg_catalog.regclass))) INTO v_ro FROM pg_catalog.unnest(v_set) x;
    PERFORM kingres.internal_views_drop(v_views);
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        FOR t IN SELECT e->>'stmt' AS stmt FROM pg_catalog.jsonb_array_elements(v_ddl) e WHERE (e->>'scope') OPERATOR(pg_catalog.=) 'local_pre'
                  ORDER BY (SELECT h.i FROM pg_catalog.unnest(v_set) WITH ORDINALITY h(y, i) WHERE h.y OPERATOR(pg_catalog.=) (e->>'t')), (e->>'ord')::pg_catalog.int4 LOOP
            PERFORM kingres.internal_run_on(nd.node_id, t.stmt);
        END LOOP;
        PERFORM kingres.internal_run_on(nd.node_id, pg_catalog.format('DROP TABLE %s', pg_catalog.array_to_string(v_set, ', ')));
    END LOOP;
    PERFORM kingres.internal_fault_point('alter_after_drop');
    FOREACH y IN ARRAY ARRAY['swap', 'fk', 'post'] LOOP
        FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
            FOR t IN SELECT e->>'stmt' AS stmt FROM pg_catalog.jsonb_array_elements(v_ddl) e
                      WHERE CASE y WHEN 'post' THEN (e->>'scope') IN ('post', 'post_local') AND ((e->>'scope') OPERATOR(pg_catalog.=) 'post' OR nd.node_id OPERATOR(pg_catalog.=) v_self)
                                   ELSE (e->>'scope') OPERATOR(pg_catalog.=) y END
                      ORDER BY (SELECT h.i FROM pg_catalog.unnest(v_set) WITH ORDINALITY h(y, i) WHERE h.y OPERATOR(pg_catalog.=) (e->>'t')), (e->>'ord')::pg_catalog.int4 LOOP
                PERFORM kingres.internal_run_on(nd.node_id, t.stmt);
            END LOOP;
        END LOOP;
    END LOOP;
    PERFORM kingres.internal_views_recreate(v_views);
    -- the catalog: the new tables, and no group without a table
    FOREACH x IN ARRAY v_set LOOP
        INSERT INTO kingres.dist_table (relid, method, dist_column, colocation_id, reloptions)
        VALUES (x::pg_catalog.regclass, 'hash', v_col, v_new,
                CASE WHEN pg_catalog.jsonb_typeof(v_ro OPERATOR(pg_catalog.->) x) OPERATOR(pg_catalog.=) 'array' THEN ARRAY(SELECT pg_catalog.jsonb_array_elements_text(v_ro OPERATOR(pg_catalog.->) x)) END);
    END LOOP;
    IF NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id)
       AND NOT EXISTS (SELECT 1 FROM kingres.bucket_move m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id) THEN
        DELETE FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id;
        DELETE FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id;
    END IF;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_routing, true);
    PERFORM kingres.sync_catalog();
    PERFORM kingres.internal_fault_point('alter_after_swap');
END;
$$;

COMMENT ON FUNCTION kingres.alter_distributed_table(regclass, name, int, text, bool) IS
    'Change the distribution column, the bucket count or the colocation group of a distributed table (and, when asked, of the tables colocated with it); writers wait for the copy';

REVOKE ALL ON FUNCTION
    kingres.internal_new_colocation(regtype, oid, int, int),
    kingres.internal_retire_partitions(regclass),
    kingres.alter_distributed_table(regclass, name, int, text, bool)
    FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- Increment 9.4, part two: the rebuild engine for a source that is a distributed table
-- (undistribute_table_concurrently and alter_distributed_table_concurrently)
--
-- The engine of 9.3 follows one plain table on the map group primary.  A distributed source has its rows on every node, so the capture, the
-- copy and the replay run on every node, each on the partitions of that node: the operation is mirrored on every other node by a row of its
-- own in kingres.table_operation (spec 'mirror', the OID of the table on that node), and the functions of 9.3 run there unchanged, in a
-- session that reads only the partitions of the node (kingres_shard.enable_routing = off).  The map group primary drives the phases,
-- takes the locks of the switch on every node, and swaps in one transaction.
-- ---------------------------------------------------------------------------

-- Whether a request to alter a table changes the catalog only (no rows move): the same distribution column and bucket count, and the
-- group left (none, default) or a group of the same placement.
CREATE FUNCTION kingres.internal_alter_is_catalog_only(p_relid regclass, p_column name, p_buckets int, p_cwith text)
    RETURNS bool
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_dt  kingres.dist_table%ROWTYPE;
    v_old kingres.colocation%ROWTYPE;
    v_to  kingres.dist_table%ROWTYPE;
BEGIN
    SELECT * INTO v_dt FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) p_relid;
    SELECT * INTO v_old FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id;
    IF COALESCE(p_column, v_dt.dist_column) OPERATOR(pg_catalog.<>) v_dt.dist_column THEN
        RETURN false;
    END IF;
    IF p_cwith IS NULL OR p_cwith IN ('default', 'none') THEN
        RETURN COALESCE(p_buckets, v_old.bucket_count) OPERATOR(pg_catalog.=) v_old.bucket_count;
    END IF;
    SELECT * INTO v_to FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) pg_catalog.to_regclass(p_cwith);
    IF v_to.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id THEN
        RETURN true;
    END IF;
    RETURN COALESCE(p_buckets, v_old.bucket_count) OPERATOR(pg_catalog.=) v_old.bucket_count
       AND (SELECT c.bucket_count FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_to.colocation_id) OPERATOR(pg_catalog.=) v_old.bucket_count
       AND NOT EXISTS (SELECT 1 FROM kingres.bucket_map a LEFT JOIN kingres.bucket_map b ON b.bucket OPERATOR(pg_catalog.=) a.bucket AND b.colocation_id OPERATOR(pg_catalog.=) v_to.colocation_id
                        WHERE a.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND (b.bucket IS NULL OR a.group_id OPERATOR(pg_catalog.<>) b.group_id));
END;
$$;

-- Record an operation on a distributed table (phase preparing) and claim the colocation groups it uses, in a session of its own, under the
-- cluster lock.  Like internal_begin_table_operation(), for the kinds undistribute and alter.
CREATE FUNCTION kingres.internal_begin_dist_operation(p_kind text, p_relid regclass, p_options jsonb)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_variable
DECLARE
    v_opts    pg_catalog.jsonb := COALESCE(p_options, '{}'::pg_catalog.jsonb) OPERATOR(pg_catalog.||) '{"mode": "online"}'::pg_catalog.jsonb;
    v_req     pg_catalog.text := p_options OPERATOR(pg_catalog.->>) 'request_id';
    v_dt      kingres.dist_table%ROWTYPE;
    v_old     kingres.colocation%ROWTYPE;
    v_to      kingres.dist_table%ROWTYPE;
    v_col     pg_catalog.name;
    v_type    pg_catalog.oid;
    v_coll    pg_catalog.oid;
    v_n       pg_catalog.int4;
    v_new     pg_catalog.int4;
    v_created pg_catalog.bool := false;
    v_cwith   pg_catalog.text := p_options OPERATOR(pg_catalog.->>) 'colocate_with';
    v_cwid    pg_catalog.regclass;
    v_target  pg_catalog.int4;
    v_id      pg_catalog.int8;
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
    IF p_kind NOT IN ('undistribute', 'alter') THEN
        RAISE EXCEPTION 'table operation "%" is not on a distributed table', p_kind USING ERRCODE = 'feature_not_supported';
    END IF;
    IF v_req IS NOT NULL THEN
        SELECT o.op_id INTO v_id FROM kingres.table_operation o
         WHERE o.spec OPERATOR(pg_catalog.->>) 'request_id' OPERATOR(pg_catalog.=) v_req AND o.phase NOT IN ('failed', 'cancelled');
        IF FOUND THEN
            RETURN v_id;
        END IF;
    END IF;
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    SELECT i.op_id INTO v_other FROM kingres.table_operation_item i JOIN kingres.table_operation o ON o.op_id OPERATOR(pg_catalog.=) i.op_id
     WHERE i.relid OPERATOR(pg_catalog.=) p_relid AND o.phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching');
    IF FOUND THEN
        RAISE EXCEPTION 'table "%" is already being rebuilt by table operation %', p_relid, v_other USING ERRCODE = 'object_in_use', HINT = 'See kingres.table_operations.';
    END IF;
    SELECT e.extversion INTO v_ver FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard';
    PERFORM kingres.internal_require_version(v_ver);
    PERFORM kingres.internal_raise_blockers(p_kind, p_relid, v_opts);
    SELECT * INTO v_dt FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) p_relid;
    IF NOT FOUND OR v_dt.method OPERATOR(pg_catalog.<>) 'hash' THEN
        RAISE EXCEPTION 'table "%" is not a distributed table', p_relid USING ERRCODE = 'wrong_object_type',
              HINT = 'A reference table is undistributed with the blocking undistribute_table().';
    END IF;
    SELECT * INTO v_old FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id;
    SELECT n.nspname, c.relname INTO v_nsp, v_rname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
     WHERE c.oid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid;
    SELECT k.key_names, k.key_types INTO v_keys, v_ktypes FROM kingres.internal_log_key(p_relid) k;
    IF v_keys IS NULL THEN
        RAISE EXCEPTION 'table "%" has no primary key or unique index over NOT NULL columns', p_relid USING ERRCODE = 'feature_not_supported',
              HINT = 'Use the blocking variant.';
    END IF;
    -- a key between tables of the catalog is carried to the new tables by the blocking variants, which take the whole component
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k JOIN kingres.dist_table d ON d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) CASE WHEN k.conrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid THEN k.confrelid ELSE k.conrelid END
                WHERE k.contype OPERATOR(pg_catalog.=) 'f' AND k.conparentid OPERATOR(pg_catalog.=) 0 AND k.conrelid OPERATOR(pg_catalog.<>) k.confrelid
                  AND (k.conrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid OR k.confrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid)
                  AND d.method OPERATOR(pg_catalog.=) 'hash') THEN
        RAISE EXCEPTION 'table "%" is linked to other distributed tables by foreign keys', p_relid USING ERRCODE = 'feature_not_supported',
              HINT = 'Use the blocking variant (with cascade) for tables that are linked to each other.';
    END IF;
    SELECT pg_catalog.array_agg(pg_catalog.format('%I.%I', sn.nspname, s.relname) ORDER BY a.attnum) INTO v_seqs
      FROM pg_catalog.pg_depend d
      JOIN pg_catalog.pg_class s ON s.oid OPERATOR(pg_catalog.=) d.objid AND s.relkind OPERATOR(pg_catalog.=) 'S'
      JOIN pg_catalog.pg_namespace sn ON sn.oid OPERATOR(pg_catalog.=) s.relnamespace
      JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) d.refobjid AND a.attnum OPERATOR(pg_catalog.=) d.refobjsubid
     WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
       AND d.refobjid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND d.deptype OPERATOR(pg_catalog.=) 'a';

    IF p_kind OPERATOR(pg_catalog.=) 'undistribute' THEN
        v_target := kingres.internal_group_node(COALESCE((p_options OPERATOR(pg_catalog.->>) 'target_group')::pg_catalog.int4, 0));
        IF v_target IS NULL THEN
            RAISE EXCEPTION 'shard group % has no active primary', COALESCE((p_options OPERATOR(pg_catalog.->>) 'target_group')::pg_catalog.int4, 0) USING ERRCODE = 'invalid_parameter_value';
        END IF;
        v_col := NULL;
        v_new := NULL;
    ELSE
        v_col := COALESCE((p_options OPERATOR(pg_catalog.->>) 'distribution_column')::pg_catalog.name, v_dt.dist_column);
        v_n := COALESCE((p_options OPERATOR(pg_catalog.->>) 'bucket_count')::pg_catalog.int4, v_old.bucket_count);
        IF COALESCE((p_options OPERATOR(pg_catalog.->>) 'cascade_to_colocated')::pg_catalog.bool, false)
           AND EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND d.relid OPERATOR(pg_catalog.<>) p_relid) THEN
            RAISE EXCEPTION 'the group of table "%" has other tables', p_relid USING ERRCODE = 'feature_not_supported',
                  HINT = 'Use the blocking variant to change the tables of a group together.';
        END IF;
        SELECT a.atttypid, a.attcollation INTO v_type, v_coll FROM pg_catalog.pg_attribute a
         WHERE a.attrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col AND NOT a.attisdropped;
        IF NOT (v_col::pg_catalog.text OPERATOR(pg_catalog.=) ANY (v_keys)) THEN
            RAISE EXCEPTION 'the distribution column "%" is not part of the key of table "%"', v_col, p_relid USING ERRCODE = 'invalid_table_definition';
        END IF;
        IF v_cwith IS NOT NULL AND v_cwith NOT IN ('default', 'none') THEN
            v_cwid := pg_catalog.to_regclass(v_cwith);
            SELECT * INTO v_to FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_cwid;
            SELECT c.dist_column_type::pg_catalog.oid, c.dist_collation, c.bucket_count INTO v_type, v_coll, v_n FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_to.colocation_id;
            IF v_type IS DISTINCT FROM (SELECT a.atttypid FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND a.attname OPERATOR(pg_catalog.=) v_col) THEN
                RAISE EXCEPTION 'cannot colocate with table "%": the distribution column types differ', v_cwid USING ERRCODE = 'datatype_mismatch';
            END IF;
            v_new := v_to.colocation_id;
        ELSIF v_cwith OPERATOR(pg_catalog.=) 'default' THEN
            SELECT c.colocation_id INTO v_new FROM kingres.colocation c
             WHERE c.dist_column_type OPERATOR(pg_catalog.=) v_type::pg_catalog.regtype AND c.dist_collation OPERATOR(pg_catalog.=) v_coll AND c.bucket_count OPERATOR(pg_catalog.=) v_n
               AND c.colocation_id OPERATOR(pg_catalog.<>) v_dt.colocation_id
               AND kingres.internal_group_busy(c.colocation_id) IS NULL
             ORDER BY c.colocation_id LIMIT 1;
        END IF;
        IF v_new IS NULL THEN
            v_new := kingres.internal_new_colocation(v_type::pg_catalog.regtype, v_coll, v_n, v_dt.colocation_id);
            v_created := true;
        END IF;
    END IF;
    -- the groups must be free of other operations and of moves
    FOR v_other IN SELECT g FROM pg_catalog.unnest(ARRAY[v_dt.colocation_id, v_new]) g WHERE g IS NOT NULL LOOP
        IF EXISTS (SELECT 1 FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) v_other)
           OR EXISTS (SELECT 1 FROM kingres.bucket_move m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_other AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching'))
           OR EXISTS (SELECT 1 FROM kingres.bucket_map b WHERE b.colocation_id OPERATOR(pg_catalog.=) v_other AND b.state OPERATOR(pg_catalog.<>) 'active') THEN
            RAISE EXCEPTION 'colocation group % is busy: a table operation or a bucket move uses it', v_other USING ERRCODE = 'object_in_use';
        END IF;
    END LOOP;

    v_total := (SELECT c.reltuples FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid);
    INSERT INTO kingres.table_operation (kind, mode, phase, spec, colocation_id, started_at, updated_at)
    VALUES (p_kind, 'online', 'preparing',
            pg_catalog.jsonb_build_object(
                'src', 'dist', 'relid', p_relid::pg_catalog.oid, 'schema', v_nsp, 'table', v_rname,
                'shadow', pg_catalog.format('kingres_shadow_%s', p_relid::pg_catalog.oid),
                'column', v_col, 'colocation_id', v_new, 'old_colocation_id', v_dt.colocation_id, 'old_column', v_dt.dist_column,
                'bucket_count', v_n, 'new_group', v_created,
                'dest_kind', CASE WHEN p_kind OPERATOR(pg_catalog.=) 'undistribute' THEN 'one_node' ELSE 'placement' END,
                'target_node', v_target,
                'keys', pg_catalog.to_jsonb(v_keys), 'key_types', pg_catalog.to_jsonb(v_ktypes),
                'sequences', pg_catalog.to_jsonb(COALESCE(v_seqs, ARRAY[]::pg_catalog.text[])),
                'request_id', v_req, 'last', NULL::pg_catalog.jsonb, 'validate', '[]'::pg_catalog.jsonb),
            v_dt.colocation_id, pg_catalog.clock_timestamp(), pg_catalog.clock_timestamp())
    RETURNING op_id INTO v_id;
    UPDATE kingres.table_operation o
       SET spec = o.spec OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object('log', pg_catalog.format('kingres.tlog_%s_1', v_id))
     WHERE o.op_id OPERATOR(pg_catalog.=) v_id;
    INSERT INTO kingres.table_operation_item (op_id, ordinal, relid, shadow_name, rows_total)
    VALUES (v_id, 1, p_relid, pg_catalog.format('kingres_shadow_%s', p_relid::pg_catalog.oid), v_total::pg_catalog.int8);
    INSERT INTO kingres.table_operation_lock (colocation_id, op_id) VALUES (v_dt.colocation_id, v_id);
    IF v_new IS NOT NULL THEN
        INSERT INTO kingres.table_operation_lock (colocation_id, op_id) VALUES (v_new, v_id);
    END IF;
    IF v_created THEN
        PERFORM kingres.sync_catalog();
    END IF;
    RETURN v_id;
END;
$$;

-- The operation on another node: a row of its own (the node-local functions of the engine read their spec from it, with the OID of the table
-- there), made in a session of that node that commits at once.  Idempotent.
CREATE FUNCTION kingres.internal_op_mirror(p_op bigint, p_node int)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rec kingres.table_operation%ROWTYPE;
    v_q   pg_catalog.text;
BEGIN
    SELECT * INTO v_rec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    v_q := pg_catalog.format('%I.%I', v_rec.spec OPERATOR(pg_catalog.->>) 'schema', v_rec.spec OPERATOR(pg_catalog.->>) 'table');
    PERFORM kingres.internal_node_exec(p_node, pg_catalog.format(
        'INSERT INTO kingres.table_operation (op_id, kind, mode, phase, spec, started_at, updated_at) OVERRIDING SYSTEM VALUE '
        'SELECT %s, %L, ''online'', ''copying'', %L::pg_catalog.jsonb OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object(''relid'', %L::pg_catalog.regclass::pg_catalog.oid, ''mirror'', true, ''last'', NULL), '
        'pg_catalog.clock_timestamp(), pg_catalog.clock_timestamp() ON CONFLICT (op_id) DO NOTHING; '
        'INSERT INTO kingres.table_operation_item (op_id, ordinal, relid, shadow_name) VALUES (%s, 1, %L::pg_catalog.regclass, %L) ON CONFLICT DO NOTHING',
        p_op, v_rec.kind, v_rec.spec::pg_catalog.text, v_q, p_op, v_q, v_rec.spec OPERATOR(pg_catalog.->>) 'shadow'));
END;
$$;

-- On this node: the partitions of a shadow take the names that partitions of the table have (the old ones were renamed out of the way),
-- and so do their indexes, which carry the name of the partition.
CREATE FUNCTION kingres.internal_adopt_shadow_partitions(p_nsp name, p_shadow name, p_final name)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r pg_catalog.record;
    i pg_catalog.record;
    v_new pg_catalog.text;
BEGIN
    FOR r IN SELECT c.oid AS poid, c.relname FROM pg_catalog.pg_inherits h JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) h.inhrelid
              WHERE h.inhparent OPERATOR(pg_catalog.=) pg_catalog.format('%I.%I', p_nsp, p_shadow)::pg_catalog.regclass::pg_catalog.oid ORDER BY c.relname
    LOOP
        v_new := p_final OPERATOR(pg_catalog.||) pg_catalog.substring(r.relname, pg_catalog.length(p_shadow::pg_catalog.text) OPERATOR(pg_catalog.+) 1);
        FOR i IN SELECT ic.relname FROM pg_catalog.pg_index x JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) x.indexrelid WHERE x.indrelid OPERATOR(pg_catalog.=) r.poid ORDER BY x.indexrelid LOOP
            IF pg_catalog.left(i.relname, pg_catalog.length(r.relname)) OPERATOR(pg_catalog.=) r.relname THEN
                EXECUTE pg_catalog.format('ALTER INDEX %I.%I RENAME TO %I', p_nsp, i.relname, v_new OPERATOR(pg_catalog.||) pg_catalog.substring(i.relname, pg_catalog.length(r.relname) OPERATOR(pg_catalog.+) 1));
            END IF;
        END LOOP;
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I RENAME TO %I', p_nsp, r.relname, v_new);
    END LOOP;
END;
$$;

-- On this node: what an undo leaves nothing of: the capture on the table, the log, the shadow, and the mirror of the operation.  Idempotent.
CREATE FUNCTION kingres.internal_node_undo(p_op bigint, p_nsp name, p_table name, p_shadow name)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rel pg_catalog.regclass := pg_catalog.to_regclass(pg_catalog.format('%I.%I', p_nsp, p_table));
    t     pg_catalog.record;
    i     pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF v_rel IS NOT NULL
       AND EXISTS (SELECT 1 FROM pg_catalog.pg_trigger g WHERE g.tgrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid
                      AND g.tgname IN (pg_catalog.format('kingres_capture_%s', p_op), pg_catalog.format('kingres_notrunc_%s', p_op))) THEN
        FOR i IN 1 .. 12 LOOP
            BEGIN
                PERFORM kingres.internal_lock_table(v_rel, 'ACCESS EXCLUSIVE');
                EXIT;
            EXCEPTION WHEN lock_not_available THEN
                IF i OPERATOR(pg_catalog.=) 12 THEN
                    RAISE;
                END IF;
            END;
        END LOOP;
        FOR t IN SELECT g.tgname FROM pg_catalog.pg_trigger g WHERE g.tgrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid
                    AND g.tgname IN (pg_catalog.format('kingres_capture_%s', p_op), pg_catalog.format('kingres_notrunc_%s', p_op))
        LOOP
            EXECUTE pg_catalog.format('DROP TRIGGER %I ON %s', t.tgname, v_rel);
        END LOOP;
    END IF;
    EXECUTE pg_catalog.format('DROP TABLE IF EXISTS kingres.tlog_%s_1', p_op);
    EXECUTE pg_catalog.format('DROP TABLE IF EXISTS %I.%I CASCADE', p_nsp, p_shadow);
    DELETE FROM kingres.table_operation_item i2 WHERE i2.op_id OPERATOR(pg_catalog.=) p_op
       AND EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op AND o.spec OPERATOR(pg_catalog.?) 'mirror');
    DELETE FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op AND o.spec OPERATOR(pg_catalog.?) 'mirror';
END;
$$;

-- The undo of an operation on a distributed table: what internal_op_undo() does for a plain one, on every node.
CREATE FUNCTION kingres.internal_op_undo_dist(p_op bigint, p_final text, p_reason text)
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rec    kingres.table_operation%ROWTYPE;
    v_spec   pg_catalog.jsonb;
    v_failed pg_catalog.int4[] := ARRAY[]::pg_catalog.int4[];
    v_group  pg_catalog.int4;
    v_drop   pg_catalog.bool := false;
    nd       pg_catalog.record;
BEGIN
    SELECT * INTO v_rec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op FOR UPDATE;
    v_spec := v_rec.spec;
    UPDATE kingres.table_operation o SET cancel_requested = true WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    v_group := (v_spec OPERATOR(pg_catalog.->>) 'colocation_id')::pg_catalog.int4;
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        BEGIN
            PERFORM kingres.internal_exec_scalar(nd.node_id,
                pg_catalog.format('SET LOCAL kingres_shard.enable_routing = off; SELECT kingres.internal_node_undo(%s, %L, %L, %L)',
                                  p_op, v_spec OPERATOR(pg_catalog.->>) 'schema', v_spec OPERATOR(pg_catalog.->>) 'table', v_spec OPERATOR(pg_catalog.->>) 'shadow'));
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            v_failed := v_failed OPERATOR(pg_catalog.||) nd.node_id;
        END;
    END LOOP;
    PERFORM kingres.internal_fault_point('rebuild_undo_after_shadows');
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
            RAISE WARNING 'the colocation group % that table operation % made could not be removed everywhere: %', v_group, p_op, SQLERRM;
        END;
    END IF;
    RETURN CASE WHEN pg_catalog.cardinality(v_failed) OPERATOR(pg_catalog.=) 0 THEN 'undone' ELSE 'incomplete' END;
END;
$$;

-- One attempt of the switch for a distributed source (the counterpart of internal_switch_attempt()): the cluster lock, the lock of the table
-- on every node (writers wait, readers go on), the replay of what is left of every node's log in sessions of their own after the locks were
-- granted, the proof that the logs are empty, the lock that the rename needs, and the swap on every node in this transaction, which the commit
-- protocol prepares and commits on every node.  An attempt that cannot get a lock in time, or whose replay takes too long, fails with
-- lock_not_available and leaves everything as it was.
CREATE FUNCTION kingres.internal_switch_attempt_dist(p_op bigint)
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_variable
DECLARE
    v_ro       pg_catalog.text[];
    v_views    pg_catalog.jsonb;
    v_rec      kingres.table_operation%ROWTYPE;
    v_spec     pg_catalog.jsonb;
    v_rel      pg_catalog.regclass;
    v_q        pg_catalog.text;
    v_log      pg_catalog.text;
    v_col      pg_catalog.name;
    v_nsp      pg_catalog.text;
    v_tbl      pg_catalog.text;
    v_shadow   pg_catalog.text;
    v_new      pg_catalog.int4;
    v_self     pg_catalog.int4;
    v_target   pg_catalog.int4;
    v_deadline pg_catalog.timestamptz;
    v_batch    pg_catalog.int4 := pg_catalog.current_setting('kingres_shard.rebuild_batch_rows')::pg_catalog.int4;
    v_ms       pg_catalog.int4 := GREATEST(kingres.internal_setting('kingres_shard.node_timeout')::pg_catalog.int4 OPERATOR(pg_catalog.*) 6000, 600000);
    v_lt       pg_catalog.text := pg_catalog.current_setting('kingres_shard.rebuild_lock_timeout');
    v_routing  pg_catalog.text := COALESCE(pg_catalog.current_setting('kingres_shard.enable_routing', true), 'on');
    v_valid    pg_catalog.text[];
    v_ddl      pg_catalog.jsonb;
    v_res      pg_catalog.text;
    v_cnt      pg_catalog.int8;
    v_has      pg_catalog.bool;
    v_old      pg_catalog.text;
    v_oldmap   pg_catalog.text;
    v_oldcopies pg_catalog.jsonb := '{}'::pg_catalog.jsonb;
    v_xids     pg_catalog.jsonb := '{}'::pg_catalog.jsonb;
    v_xid      pg_catalog.text;
    v_cleanup  pg_catalog.int4 := kingres.internal_setting('kingres_shard.table_cleanup_delay')::pg_catalog.int4;
    nd         pg_catalog.record;
    t          pg_catalog.record;
    y          pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT * INTO v_rec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'table operation % does not exist', p_op USING ERRCODE = 'undefined_object';
    END IF;
    IF v_rec.phase OPERATOR(pg_catalog.<>) 'switching' THEN
        RAISE EXCEPTION 'table operation % is in phase "%", not switching', p_op, v_rec.phase USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF v_rec.cancel_requested THEN
        RAISE EXCEPTION 'table operation % was asked to stop', p_op USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    v_spec := v_rec.spec;
    v_rel := (v_spec OPERATOR(pg_catalog.->>) 'relid')::pg_catalog.oid::pg_catalog.regclass;
    v_nsp := v_spec OPERATOR(pg_catalog.->>) 'schema';
    v_tbl := v_spec OPERATOR(pg_catalog.->>) 'table';
    v_shadow := v_spec OPERATOR(pg_catalog.->>) 'shadow';
    v_q := pg_catalog.format('%I.%I', v_nsp, v_tbl);
    v_log := v_spec OPERATOR(pg_catalog.->>) 'log';
    v_col := v_spec OPERATOR(pg_catalog.->>) 'column';
    v_new := (v_spec OPERATOR(pg_catalog.->>) 'colocation_id')::pg_catalog.int4;
    v_target := (v_spec OPERATOR(pg_catalog.->>) 'target_node')::pg_catalog.int4;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_deadline := pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+)
                  pg_catalog.make_interval(secs => kingres.internal_setting('kingres_shard.rebuild_switch_timeout')::pg_catalog.int4 OPERATOR(pg_catalog./) 1000.0);
    PERFORM kingres.internal_fault_point('rebuild_before_switch');
    v_ro := kingres.internal_reloptions_of(v_rel);

    -- the catalog lock first (the order of every catalog change), then the tables, in node order
    PERFORM pg_catalog.set_config('lock_timeout', v_lt, true);
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
            PERFORM kingres.internal_lock_table(v_rel, 'EXCLUSIVE');
        ELSE
            PERFORM kingres.internal_branch_exec(nd.node_id, pg_catalog.format('SET LOCAL lock_timeout = %L; LOCK TABLE %s IN EXCLUSIVE MODE', v_lt, v_q));
        END IF;
    END LOOP;
    PERFORM kingres.internal_fault_point('rebuild_switch_after_lock');

    -- what is left of the logs, replayed after the locks were granted: no writer can add to them now
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        LOOP
            v_res := kingres.internal_replay_run(nd.node_id, p_op, v_batch, v_ms, 'SET LOCAL kingres_shard.enable_routing = off; ');
            EXIT WHEN v_res::pg_catalog.int4 OPERATOR(pg_catalog.=) 0;
            IF pg_catalog.clock_timestamp() OPERATOR(pg_catalog.>) v_deadline THEN
                RAISE EXCEPTION 'the final replay of table % did not finish in time on node %', v_rel, nd.node_id
                    USING ERRCODE = 'lock_not_available', HINT = 'The writers of the table wait meanwhile; the operation tries again.';
            END IF;
        END LOOP;
        v_cnt := kingres.internal_exec_scalar(nd.node_id, pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', v_log), v_ms)::pg_catalog.int8;
        IF v_cnt OPERATOR(pg_catalog.>) 0 THEN
            RAISE EXCEPTION 'the log of table % is not empty after the final replay on node % (% entries)', v_rel, nd.node_id, v_cnt USING ERRCODE = 'internal_error';
        END IF;
    END LOOP;
    PERFORM kingres.internal_fault_point('rebuild_switch_after_final_replay');

    -- the views are read before ACCESS EXCLUSIVE is taken on the nodes: reading a view takes a lock on its tables
    v_views := kingres.internal_views_capture(ARRAY[v_q], true, COALESCE(v_spec OPERATOR(pg_catalog.->>) 'dependent_views', 'refuse'), v_target);
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
            PERFORM kingres.internal_lock_table(v_rel, 'ACCESS EXCLUSIVE');
        ELSE
            PERFORM kingres.internal_branch_exec(nd.node_id, pg_catalog.format('SET LOCAL lock_timeout = %L; LOCK TABLE %s IN ACCESS EXCLUSIVE MODE', v_lt, v_q));
        END IF;
    END LOOP;
    SELECT pg_catalog.array_agg(c.conname::pg_catalog.text ORDER BY c.conname) INTO v_valid
      FROM pg_catalog.pg_constraint c
     WHERE c.conrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid AND c.contype OPERATOR(pg_catalog.=) 'f' AND c.conparentid OPERATOR(pg_catalog.=) 0 AND c.convalidated;
    PERFORM kingres.internal_op_spec(p_op, pg_catalog.jsonb_build_object('validate', pg_catalog.to_jsonb(COALESCE(v_valid, ARRAY[]::pg_catalog.text[]))));

    -- the statements of the new table, read while the old one is whole (sequence states are the final ones)
    SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('scope', d.scope, 'ord', d.ord, 'stmt', d.stmt) ORDER BY d.ord)
      INTO v_ddl FROM kingres.internal_table_ddl(v_rel, v_col, true) d;
    PERFORM kingres.internal_fault_point('rebuild_switch_before_swap');

    -- the swap, on every node, with the statements of the extension off
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    -- the views that use the table follow it to the old copy by their OIDs: they are dropped on every node and made again on the new table
    PERFORM kingres.internal_views_drop(v_views);
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        v_has := v_target IS NULL OR nd.node_id OPERATOR(pg_catalog.=) v_target;
        IF nd.node_id OPERATOR(pg_catalog.<>) v_self THEN
            PERFORM kingres.internal_branch_exec(nd.node_id, 'SET LOCAL kingres_shard.enable_routing = off');
        END IF;
        PERFORM pg_catalog.set_config('lock_timeout', v_lt, true);
        IF v_has THEN
            FOR t IN SELECT e->>'stmt' AS stmt FROM pg_catalog.jsonb_array_elements(v_ddl) e WHERE (e->>'scope') OPERATOR(pg_catalog.=) 'local_pre' ORDER BY (e->>'ord')::pg_catalog.int4 LOOP
                PERFORM kingres.internal_run_on(nd.node_id, t.stmt);
            END LOOP;
        END IF;
        PERFORM kingres.internal_run_on(nd.node_id, pg_catalog.format('SELECT kingres.internal_retire_partitions(%L::pg_catalog.regclass)', v_q));
        IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
            v_old := kingres.internal_retire_table(v_rel, p_op)::pg_catalog.text;
        ELSE
            v_old := kingres.internal_branch_scalar(nd.node_id, pg_catalog.format('SELECT kingres.internal_retire_table(%L::pg_catalog.regclass, %s)::pg_catalog.text', v_q, p_op));
        END IF;
        -- the name with its schema (a regclass written under the search_path of the function has none)
        v_old := 'kingres.' OPERATOR(pg_catalog.||) pg_catalog.regexp_replace(v_old, '^kingres\.', '');
        v_oldcopies := v_oldcopies OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object(nd.node_id::pg_catalog.text, v_old);
        PERFORM kingres.internal_run_on(nd.node_id, pg_catalog.format(
            'INSERT INTO kingres.table_old_copy (relid, dropped_after) VALUES (%L::pg_catalog.regclass, pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+) pg_catalog.make_interval(secs => %s))', v_old, v_cleanup));
        IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
            v_oldmap := v_old;
        END IF;
        IF v_has THEN
            IF v_target IS NULL THEN
                PERFORM kingres.internal_run_on(nd.node_id, pg_catalog.format('SELECT kingres.internal_adopt_shadow_partitions(%L, %L, %L)', v_nsp, v_shadow, v_tbl));
            END IF;
            FOR t IN SELECT e->>'stmt' AS stmt FROM pg_catalog.jsonb_array_elements(v_ddl) e WHERE (e->>'scope') OPERATOR(pg_catalog.=) 'swap' ORDER BY (e->>'ord')::pg_catalog.int4 LOOP
                PERFORM kingres.internal_run_on(nd.node_id, t.stmt);
            END LOOP;
            -- the keys come NOT VALID: no scan under the locks; the cleaning phase validates them while writers are admitted
            FOR t IN SELECT e->>'stmt' AS stmt FROM pg_catalog.jsonb_array_elements(v_ddl) e WHERE (e->>'scope') OPERATOR(pg_catalog.=) 'fk' ORDER BY (e->>'ord')::pg_catalog.int4 LOOP
                PERFORM kingres.internal_run_on(nd.node_id, CASE WHEN t.stmt LIKE '%NOT VALID%' THEN t.stmt ELSE t.stmt OPERATOR(pg_catalog.||) ' NOT VALID' END);
            END LOOP;
            FOR t IN SELECT e->>'stmt' AS stmt FROM pg_catalog.jsonb_array_elements(v_ddl) e
                      WHERE (e->>'scope') OPERATOR(pg_catalog.=) 'post' OR ((e->>'scope') OPERATOR(pg_catalog.=) 'post_local' AND nd.node_id OPERATOR(pg_catalog.=) v_self)
                      ORDER BY (e->>'ord')::pg_catalog.int4 LOOP
                PERFORM kingres.internal_run_on(nd.node_id, t.stmt);
            END LOOP;
        END IF;
        IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
            v_xid := pg_catalog.pg_current_xact_id()::pg_catalog.text;
        ELSE
            v_xid := kingres.internal_branch_scalar(nd.node_id, 'SELECT pg_catalog.pg_current_xact_id()::pg_catalog.text');
        END IF;
        v_xids := v_xids OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object(nd.node_id::pg_catalog.text, v_xid);
        IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
            PERFORM kingres.internal_fault_point('rebuild_switch_after_local_swap');
        END IF;
    END LOOP;
    PERFORM kingres.internal_views_recreate(v_views);
    PERFORM kingres.internal_fault_point('rebuild_switch_after_remote_swap');

    -- the catalog: the old table is out, the new one is in
    DELETE FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_rel;
    IF v_target IS NULL THEN
        INSERT INTO kingres.dist_table (relid, method, dist_column, colocation_id, reloptions)
        VALUES (pg_catalog.to_regclass(v_q), 'hash', v_col, v_new, v_ro);
    END IF;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_routing, true);
    UPDATE kingres.table_operation o
       SET spec = o.spec OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object('swap_xid', v_xids, 'old_copies', v_oldcopies),
           phase = 'cleaning', detail = NULL,
           cleanup_after = pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+) pg_catalog.make_interval(secs => v_cleanup),
           updated_at = pg_catalog.clock_timestamp()
     WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    UPDATE kingres.table_operation_item i SET old_name = pg_catalog.split_part(v_oldmap, '.', 2) WHERE i.op_id OPERATOR(pg_catalog.=) p_op;
    PERFORM kingres.sync_catalog();
    PERFORM kingres.internal_fault_point('rebuild_switch_after_prepare');
    RETURN v_oldmap;
END;
$$;

-- The cleaning phase for a distributed source (the counterpart of internal_op_clean()): the old copies, on every node, are compared with the new
-- tables; the keys that the swap added NOT VALID are validated; the claims, the mirrors on the other nodes and a group without a table go.
CREATE FUNCTION kingres.internal_op_clean_dist(p_op bigint)
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rec    kingres.table_operation%ROWTYPE;
    v_spec   pg_catalog.jsonb;
    v_self   pg_catalog.int4;
    v_target pg_catalog.int4;
    v_q      pg_catalog.text;
    v_orows  pg_catalog.int8 := 0;
    v_ohash  pg_catalog.numeric := 0;
    v_rows   pg_catalog.int8 := 0;
    v_hash   pg_catalog.numeric := 0;
    v_newer  pg_catalog.int8 := 0;
    v_res    pg_catalog.text;
    v_ok     pg_catalog.bool := true;
    v_why    pg_catalog.text;
    v_names  pg_catalog.text[];
    v_oldgrp pg_catalog.int4;
    v_ms     pg_catalog.int4 := GREATEST(kingres.internal_setting('kingres_shard.node_timeout')::pg_catalog.int4 OPERATOR(pg_catalog.*) 12000, 600000);
    nd       pg_catalog.record;
    nm       pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT * INTO v_rec FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op FOR UPDATE;
    v_spec := v_rec.spec;
    v_target := (v_spec OPERATOR(pg_catalog.->>) 'target_node')::pg_catalog.int4;
    v_oldgrp := (v_spec OPERATOR(pg_catalog.->>) 'old_colocation_id')::pg_catalog.int4;
    v_q := pg_catalog.format('%I.%I', v_spec OPERATOR(pg_catalog.->>) 'schema', v_spec OPERATOR(pg_catalog.->>) 'table');
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    PERFORM kingres.internal_fault_point('rebuild_verify');

    IF NOT pg_catalog.current_setting('kingres_shard.rebuild_verify')::pg_catalog.bool THEN
        UPDATE kingres.table_operation o SET verified = 'skipped', detail = 'kingres_shard.rebuild_verify is off' WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    ELSIF NOT (v_spec OPERATOR(pg_catalog.?) 'old_copies') THEN
        UPDATE kingres.table_operation o SET verified = 'skipped', detail = 'the old copy is gone' WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
    ELSE
        FOR nd IN SELECT e.key AS node_id, e.value #>> '{}' AS old FROM pg_catalog.jsonb_each(v_spec OPERATOR(pg_catalog.->) 'old_copies') e ORDER BY e.key::pg_catalog.int4 LOOP
            v_res := kingres.internal_exec_scalar(nd.node_id::pg_catalog.int4,
                pg_catalog.format('SELECT d.n_rows::pg_catalog.text OPERATOR(pg_catalog.||) '','' OPERATOR(pg_catalog.||) d.n_hash::pg_catalog.text FROM kingres.internal_table_digest(%L::pg_catalog.regclass) d', nd.old), v_ms);
            v_orows := v_orows OPERATOR(pg_catalog.+) pg_catalog.split_part(v_res, ',', 1)::pg_catalog.int8;
            v_ohash := v_ohash OPERATOR(pg_catalog.+) pg_catalog.split_part(v_res, ',', 2)::pg_catalog.numeric;
        END LOOP;
        FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active
                     AND (v_target IS NULL OR n.node_id OPERATOR(pg_catalog.=) v_target) ORDER BY n.node_id LOOP
            v_res := kingres.internal_exec_scalar(nd.node_id,
                pg_catalog.format('SELECT d.n_rows::pg_catalog.text OPERATOR(pg_catalog.||) '','' OPERATOR(pg_catalog.||) d.n_hash::pg_catalog.text OPERATOR(pg_catalog.||) '','' OPERATOR(pg_catalog.||) d.n_newer::pg_catalog.text FROM kingres.internal_table_digest(%L::pg_catalog.regclass, %L::pg_catalog.xid8) d',
                                  v_q, v_spec OPERATOR(pg_catalog.->) 'swap_xid' OPERATOR(pg_catalog.->>) nd.node_id::pg_catalog.text), v_ms);
            v_rows := v_rows OPERATOR(pg_catalog.+) pg_catalog.split_part(v_res, ',', 1)::pg_catalog.int8;
            v_hash := v_hash OPERATOR(pg_catalog.+) pg_catalog.split_part(v_res, ',', 2)::pg_catalog.numeric;
            v_newer := v_newer OPERATOR(pg_catalog.+) pg_catalog.split_part(v_res, ',', 3)::pg_catalog.int8;
        END LOOP;
        IF v_rows IS DISTINCT FROM v_orows OR v_hash IS DISTINCT FROM v_ohash THEN
            v_ok := false;
            v_why := pg_catalog.format('the new table holds %s rows (the old copy has %s) or a different content', v_rows, v_orows);
        END IF;
        IF NOT v_ok AND v_newer OPERATOR(pg_catalog.>) 0 THEN
            UPDATE kingres.table_operation o SET verified = 'skipped',
                   detail = pg_catalog.format('the table was written to after the switch (%s rows are newer than the swap); the comparison with the old copy is not possible, and it differs: %s', v_newer, v_why)
             WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
        ELSIF NOT v_ok THEN
            UPDATE kingres.table_operation o SET verified = 'failed', detail = v_why, updated_at = pg_catalog.clock_timestamp() WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
            RETURN pg_catalog.format('failed: %s', v_why);
        ELSE
            UPDATE kingres.table_operation o SET verified = 'ok' WHERE o.op_id OPERATOR(pg_catalog.=) p_op;
        END IF;
    END IF;

    SELECT pg_catalog.array_agg(x) INTO v_names FROM pg_catalog.jsonb_array_elements_text(v_spec OPERATOR(pg_catalog.->) 'validate') x;
    IF v_names IS NOT NULL THEN
        FOREACH nm IN ARRAY v_names LOOP
            FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active
                         AND (v_target IS NULL OR n.node_id OPERATOR(pg_catalog.=) v_target) ORDER BY n.node_id LOOP
                PERFORM kingres.internal_node_exec(nd.node_id,
                    pg_catalog.format('SET LOCAL kingres_shard.enable_routing = off; ALTER TABLE %s VALIDATE CONSTRAINT %I', v_q, nm), v_ms);
            END LOOP;
        END LOOP;
    END IF;
    PERFORM kingres.internal_fault_point('rebuild_cleanup');

    DELETE FROM kingres.table_operation_lock l WHERE l.op_id OPERATOR(pg_catalog.=) p_op;
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND n.node_id OPERATOR(pg_catalog.<>) v_self ORDER BY n.node_id LOOP
        BEGIN
            PERFORM kingres.internal_node_exec(nd.node_id, pg_catalog.format(
                'DELETE FROM kingres.table_operation_item WHERE op_id = %s AND EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id = %s AND o.spec ? ''mirror''); DELETE FROM kingres.table_operation WHERE op_id = %s AND spec ? ''mirror''', p_op, p_op, p_op));
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            RAISE WARNING 'the mirror of table operation % on node % could not be removed: %', p_op, nd.node_id, SQLERRM;
        END;
    END LOOP;
    -- the group the table left (alter) or lived in (undistribute) goes when no table is in it
    IF v_oldgrp IS NOT NULL AND NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) v_oldgrp)
       AND NOT EXISTS (SELECT 1 FROM kingres.bucket_move m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_oldgrp)
       AND NOT EXISTS (SELECT 1 FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) v_oldgrp) THEN
        BEGIN
            DELETE FROM kingres.bucket_map b WHERE b.colocation_id OPERATOR(pg_catalog.=) v_oldgrp;
            DELETE FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_oldgrp;
            PERFORM kingres.sync_catalog();
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            RAISE WARNING 'the colocation group % could not be removed everywhere: %', v_oldgrp, SQLERRM;
        END;
    END IF;
    PERFORM kingres.internal_op_set(p_op, 'done', (SELECT o.detail FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) p_op AND o.verified OPERATOR(pg_catalog.=) 'skipped'));
    RETURN 'done';
END;
$$;

-- The online variants for a distributed table.  A request that changes the catalog only (no rows move) is the blocking function's: nothing to
-- follow.  A table that is linked to other distributed tables by keys, and a group of several tables to change together, are the blocking
-- variants' (they carry the keys between the tables).
CREATE FUNCTION kingres.undistribute_table_concurrently(relation regclass, cascade_via_foreign_keys bool DEFAULT false, target_group int DEFAULT NULL)
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
    PERFORM kingres.internal_prevent_xact_block('kingres.undistribute_table_concurrently()');
    v_op := kingres.begin_table_operation('undistribute', relation,
                pg_catalog.jsonb_strip_nulls(pg_catalog.jsonb_build_object('cascade_via_foreign_keys', cascade_via_foreign_keys, 'target_group', target_group)));
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') * 6000;
    BEGIN
        v_phase := kingres.table_operation_advance(v_op, 'cleaning');
    EXCEPTION WHEN OTHERS OR query_canceled THEN
        BEGIN
            PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''failed'', %L)', v_op, SQLERRM), v_ms);
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            NULL;
        END;
        RAISE;
    END;
    IF v_phase OPERATOR(pg_catalog.<>) 'done' THEN
        SELECT o.detail INTO v_detail FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
        PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''failed'', %L)', v_op, COALESCE(v_detail, 'did not finish')), v_ms);
        RAISE EXCEPTION 'table operation % did not finish: it stopped in phase %', v_op, v_phase
            USING ERRCODE = 'object_not_in_prerequisite_state', DETAIL = v_detail;
    END IF;
    RETURN v_op;
END;
$$;

CREATE FUNCTION kingres.alter_distributed_table_concurrently(relation regclass, distribution_column name DEFAULT NULL, bucket_count int DEFAULT NULL,
                                                             colocate_with text DEFAULT NULL, cascade_to_colocated bool DEFAULT false)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_variable
DECLARE
    v_op     pg_catalog.int8;
    v_phase  pg_catalog.text;
    v_self   pg_catalog.int4;
    v_ms     pg_catalog.int4;
    v_detail pg_catalog.text;
BEGIN
    PERFORM kingres.internal_prevent_xact_block('kingres.alter_distributed_table_concurrently()');
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    IF pg_catalog.to_regclass('kingres.dist_table') IS NOT NULL
       AND EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) relation AND d.method OPERATOR(pg_catalog.=) 'hash')
       AND kingres.internal_alter_is_catalog_only(relation, distribution_column, bucket_count, colocate_with) THEN
        -- no rows move: nothing to follow
        PERFORM kingres.alter_distributed_table(relation, distribution_column, bucket_count, colocate_with, cascade_to_colocated);
        RETURN NULL;
    END IF;
    v_op := kingres.begin_table_operation('alter', relation,
                pg_catalog.jsonb_strip_nulls(pg_catalog.jsonb_build_object('distribution_column', distribution_column, 'bucket_count', bucket_count,
                    'colocate_with', colocate_with, 'cascade_to_colocated', cascade_to_colocated)));
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') * 6000;
    BEGIN
        v_phase := kingres.table_operation_advance(v_op, 'cleaning');
    EXCEPTION WHEN OTHERS OR query_canceled THEN
        BEGIN
            PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''failed'', %L)', v_op, SQLERRM), v_ms);
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            NULL;
        END;
        RAISE;
    END;
    IF v_phase OPERATOR(pg_catalog.<>) 'done' THEN
        SELECT o.detail INTO v_detail FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
        PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''failed'', %L)', v_op, COALESCE(v_detail, 'did not finish')), v_ms);
        RAISE EXCEPTION 'table operation % did not finish: it stopped in phase %', v_op, v_phase
            USING ERRCODE = 'object_not_in_prerequisite_state', DETAIL = v_detail;
    END IF;
    RETURN v_op;
END;
$$;

COMMENT ON FUNCTION kingres.undistribute_table_concurrently(regclass, bool, int) IS
    'The online variant of undistribute_table for a distributed table: writers keep working; returns the id of the operation when the table is an ordinary table';
COMMENT ON FUNCTION kingres.alter_distributed_table_concurrently(regclass, name, int, text, bool) IS
    'The online variant of alter_distributed_table for one table: writers keep working; returns the id of the operation (NULL when only the catalog changed)';

REVOKE ALL ON FUNCTION
    kingres.internal_alter_is_catalog_only(regclass, name, int, text),
    kingres.internal_begin_dist_operation(text, regclass, jsonb),
    kingres.internal_op_mirror(bigint, int),
    kingres.internal_adopt_shadow_partitions(name, name, name),
    kingres.internal_node_undo(bigint, name, name, name),
    kingres.internal_op_undo_dist(bigint, text, text),
    kingres.internal_switch_attempt_dist(bigint),
    kingres.internal_op_clean_dist(bigint),
    kingres.undistribute_table_concurrently(regclass, bool, int),
    kingres.alter_distributed_table_concurrently(regclass, name, int, text, bool)
    FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- User partitions of a distributed table (increment 9.5, step 3): create, drop, detach and attach.  Each runs on the map group primary, in one
-- two-phase transaction on every node, under the lock of the catalog, and is refused while a move or an operation uses the group.
-- ---------------------------------------------------------------------------

-- On this node: the work of a partition operation for the buckets of this node's group.  The names are built here from validated pieces.
--   create  : a leaf <name>_bNNNN of every bucket partition, attached with the bound
--   drop    : the leaves are dropped
--   detach  : the leaves of <name> move from the bucket partitions of <parent> to the new hash parent <name> (made by make_parent)
--   attach  : the bucket partitions of the distributed table <name> move to the bucket partitions of <parent> as leaves, with the bound
--   make_parent : the hash parent <name> of a partition that is detached (the key columns, indexes and constraints of <parent>; p_bound is the column)
--   drop_parent : the shell of the table that was attached
CREATE FUNCTION kingres.internal_leaf_op(p_op text, p_nsp name, p_parent name, p_name name, p_bound text, p_buckets int4[])
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    b      pg_catalog.int4;
    bp     pg_catalog.text;
    lf     pg_catalog.text;
    own    pg_catalog.text;
    pb     pg_catalog.text;
    clause pg_catalog.text;
BEGIN
    clause := CASE WHEN pg_catalog.upper(pg_catalog.btrim(p_bound)) OPERATOR(pg_catalog.=) 'DEFAULT' THEN 'DEFAULT' ELSE 'FOR VALUES ' OPERATOR(pg_catalog.||) p_bound END;
    IF p_op OPERATOR(pg_catalog.=) 'make_parent' THEN
        EXECUTE pg_catalog.format('CREATE TABLE %I.%I (LIKE %I.%I INCLUDING ALL) PARTITION BY HASH (%I)', p_nsp, p_name, p_nsp, p_parent, p_bound);
        SELECT pg_catalog.pg_get_userbyid(c.relowner) INTO own FROM pg_catalog.pg_class c
         WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.format('%I.%I', p_nsp, p_parent)::pg_catalog.regclass::pg_catalog.oid;
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I OWNER TO %I', p_nsp, p_name, own);
        -- the privileges and the comment of the table go to the new table too (P9C-10)
        PERFORM kingres.internal_copy_acl_comment(pg_catalog.format('%I.%I', p_nsp, p_parent)::pg_catalog.regclass,
                                                  pg_catalog.format('%I.%I', p_nsp, p_name)::pg_catalog.regclass);
        RETURN;
    ELSIF p_op OPERATOR(pg_catalog.=) 'drop_parent' THEN
        EXECUTE pg_catalog.format('DROP TABLE %I.%I', p_nsp, p_name);
        RETURN;
    END IF;
    FOREACH b IN ARRAY p_buckets LOOP
        bp := pg_catalog.format('%I.%I', p_nsp, p_parent OPERATOR(pg_catalog.||) '_b' OPERATOR(pg_catalog.||) pg_catalog.lpad(b::pg_catalog.text, 4, '0'));
        lf := pg_catalog.format('%I.%I', p_nsp, p_name OPERATOR(pg_catalog.||) '_b' OPERATOR(pg_catalog.||) pg_catalog.lpad(b::pg_catalog.text, 4, '0'));
        IF p_op OPERATOR(pg_catalog.=) 'create' THEN
            EXECUTE pg_catalog.format('CREATE TABLE %s (LIKE %s INCLUDING ALL)', lf, bp);
            SELECT pg_catalog.pg_get_userbyid(c.relowner) INTO own FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) bp::pg_catalog.regclass::pg_catalog.oid;
            EXECUTE pg_catalog.format('ALTER TABLE %s OWNER TO %I', lf, own);
            EXECUTE pg_catalog.format('ALTER TABLE %s ATTACH PARTITION %s %s', bp, lf, clause);
        ELSIF p_op OPERATOR(pg_catalog.=) 'drop' THEN
            EXECUTE pg_catalog.format('DROP TABLE IF EXISTS %s', lf);
        ELSIF p_op OPERATOR(pg_catalog.=) 'detach' THEN
            SELECT pg_catalog.pg_get_expr(c.relpartbound, c.oid) INTO pb FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) bp::pg_catalog.regclass::pg_catalog.oid;
            EXECUTE pg_catalog.format('ALTER TABLE %s DETACH PARTITION %s', bp, lf);
            EXECUTE pg_catalog.format('ALTER TABLE %I.%I ATTACH PARTITION %s %s', p_nsp, p_name, lf, pb);
        ELSIF p_op OPERATOR(pg_catalog.=) 'attach' THEN
            EXECUTE pg_catalog.format('ALTER TABLE %I.%I DETACH PARTITION %s', p_nsp, p_name, lf);
            EXECUTE pg_catalog.format('ALTER TABLE %s ATTACH PARTITION %s %s', bp, lf, clause);
        ELSE
            RAISE EXCEPTION 'unknown partition operation "%"', p_op;
        END IF;
    END LOOP;
END;
$$;

-- The checks every partition function makes, and its context: the table, its schema, its group, the idle group.
CREATE FUNCTION kingres.internal_partition_context(p_parent regclass, OUT nsp name, OUT rname name, OUT colocation int4, OUT dist_column name)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_dt kingres.dist_table%ROWTYPE;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    PERFORM kingres.internal_require_version((SELECT e.extversion FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard'));
    PERFORM kingres.internal_require_read_committed('the partition functions');
    SET LOCAL lock_timeout = '5s';
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    SELECT * INTO v_dt FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) p_parent;
    IF NOT FOUND OR v_dt.partition_method IS NULL THEN
        RAISE EXCEPTION 'table "%" is not a user-partitioned distributed table', p_parent USING ERRCODE = 'wrong_object_type';
    END IF;
    IF EXISTS (SELECT 1 FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id)
       OR EXISTS (SELECT 1 FROM kingres.bucket_move m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching'))
       OR EXISTS (SELECT 1 FROM kingres.bucket_map b WHERE b.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND b.state OPERATOR(pg_catalog.<>) 'active') THEN
        RAISE EXCEPTION 'colocation group % is busy: a table operation or a bucket move uses it', v_dt.colocation_id USING ERRCODE = 'object_in_use';
    END IF;
    SELECT n.nspname, c.relname INTO nsp, rname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
     WHERE c.oid OPERATOR(pg_catalog.=) p_parent::pg_catalog.oid;
    colocation := v_dt.colocation_id;
    dist_column := v_dt.dist_column;
END;
$$;

-- Run a leaf operation on every node, for the buckets that its group owns.
CREATE FUNCTION kingres.internal_leaf_everywhere(p_op text, p_colocation int4, p_nsp name, p_parent name, p_name name, p_bound text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    nd        pg_catalog.record;
    v_buckets pg_catalog.int4[];
    v_routing pg_catalog.text := COALESCE(pg_catalog.current_setting('kingres_shard.enable_routing', true), 'on');
BEGIN
    IF p_bound IS NOT NULL AND p_op IN ('create', 'attach') AND (p_bound ~ '[;]|--|/[*]' OR NOT (pg_catalog.upper(pg_catalog.btrim(p_bound)) OPERATOR(pg_catalog.=) 'DEFAULT' OR p_bound ~* '^ *(FROM|IN|WITH) *[(]')) THEN
        RAISE EXCEPTION 'invalid partition bound "%"', p_bound USING ERRCODE = 'invalid_parameter_value',
              HINT = 'Give the text that follows FOR VALUES: FROM (...) TO (...), IN (...), or DEFAULT.';
    END IF;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    FOR nd IN SELECT n.node_id, n.group_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active ORDER BY n.node_id LOOP
        SELECT pg_catalog.array_agg(m.bucket ORDER BY m.bucket) INTO v_buckets FROM kingres.bucket_map m
         WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation AND m.group_id OPERATOR(pg_catalog.=) nd.group_id;
        IF p_op IN ('make_parent', 'drop_parent') THEN
            v_buckets := ARRAY[]::pg_catalog.int4[];
        ELSIF v_buckets IS NULL THEN
            CONTINUE;
        END IF;
        IF nd.node_id OPERATOR(pg_catalog.<>) (SELECT l.node_id FROM kingres.local_node l) THEN
            PERFORM kingres.internal_branch_exec(nd.node_id, 'SET LOCAL kingres_shard.enable_routing = off; SET LOCAL lock_timeout = ''5s''');
        END IF;
        PERFORM kingres.internal_run_on(nd.node_id, pg_catalog.format('SELECT kingres.internal_leaf_op(%L, %L, %L, %L, %L, %L::pg_catalog.int4[])',
                                                                     p_op, p_nsp, p_parent, p_name, p_bound, v_buckets));
    END LOOP;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_routing, true);
END;
$$;

CREATE FUNCTION kingres.create_partition(parent regclass, partition_name text, bound text, if_not_exists bool DEFAULT false)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    SET client_min_messages = warning
    AS $$
DECLARE
    c pg_catalog.record;
BEGIN
    SELECT * INTO c FROM kingres.internal_partition_context(parent);
    PERFORM kingres.internal_fault_point('partition_after_lock');
    IF partition_name IS NULL OR pg_catalog.octet_length(partition_name) OPERATOR(pg_catalog.>) 57 THEN
        RAISE EXCEPTION 'partition name "%" is too long or missing: leaf names append 6 characters (_bNNNN), at most 57 bytes', partition_name USING ERRCODE = 'invalid_name';
    END IF;
    IF EXISTS (SELECT 1 FROM kingres.dist_partition p WHERE p.parent OPERATOR(pg_catalog.=) create_partition.parent AND p.partition_name OPERATOR(pg_catalog.=) create_partition.partition_name) THEN
        IF if_not_exists THEN
            RETURN;
        END IF;
        RAISE EXCEPTION 'partition "%" of table "%" already exists', partition_name, parent USING ERRCODE = 'duplicate_table';
    END IF;
    IF pg_catalog.to_regclass(pg_catalog.format('%I.%I', c.nsp, partition_name)) IS NOT NULL THEN
        RAISE EXCEPTION 'relation "%" already exists', partition_name USING ERRCODE = 'duplicate_table';
    END IF;
    PERFORM kingres.internal_leaf_everywhere('create', c.colocation, c.nsp, c.rname, partition_name::pg_catalog.name, bound);
    PERFORM kingres.internal_fault_point('partition_after_leaves');
    INSERT INTO kingres.dist_partition (parent, partition_name, bound) VALUES (parent, partition_name, bound);
    PERFORM kingres.sync_catalog();
END;
$$;

CREATE FUNCTION kingres.drop_partition(parent regclass, partition_name text, missing_ok bool DEFAULT false)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    SET client_min_messages = warning
    AS $$
DECLARE
    c pg_catalog.record;
BEGIN
    SELECT * INTO c FROM kingres.internal_partition_context(parent);
    PERFORM kingres.internal_fault_point('partition_after_lock');
    IF NOT EXISTS (SELECT 1 FROM kingres.dist_partition p WHERE p.parent OPERATOR(pg_catalog.=) drop_partition.parent AND p.partition_name OPERATOR(pg_catalog.=) drop_partition.partition_name) THEN
        IF missing_ok THEN
            RETURN;
        END IF;
        RAISE EXCEPTION 'partition "%" of table "%" does not exist', partition_name, parent USING ERRCODE = 'undefined_table';
    END IF;
    PERFORM kingres.internal_leaf_everywhere('drop', c.colocation, c.nsp, c.rname, partition_name::pg_catalog.name, NULL);
    PERFORM kingres.internal_fault_point('partition_after_leaves');
    DELETE FROM kingres.dist_partition p WHERE p.parent OPERATOR(pg_catalog.=) drop_partition.parent AND p.partition_name OPERATOR(pg_catalog.=) drop_partition.partition_name;
    PERFORM kingres.sync_catalog();
END;
$$;

-- The partition becomes a distributed table of its own (hash-partitioned, in the group of the table); no row moves.
CREATE FUNCTION kingres.detach_partition(parent regclass, partition_name text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    SET client_min_messages = warning
    AS $$
DECLARE
    c pg_catalog.record;
BEGIN
    SELECT * INTO c FROM kingres.internal_partition_context(parent);
    PERFORM kingres.internal_fault_point('partition_after_lock');
    IF NOT EXISTS (SELECT 1 FROM kingres.dist_partition p WHERE p.parent OPERATOR(pg_catalog.=) detach_partition.parent AND p.partition_name OPERATOR(pg_catalog.=) detach_partition.partition_name) THEN
        RAISE EXCEPTION 'partition "%" of table "%" does not exist', partition_name, parent USING ERRCODE = 'undefined_table';
    END IF;
    IF pg_catalog.to_regclass(pg_catalog.format('%I.%I', c.nsp, partition_name)) IS NOT NULL THEN
        RAISE EXCEPTION 'relation "%" already exists', partition_name USING ERRCODE = 'duplicate_table';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k WHERE k.contype OPERATOR(pg_catalog.=) 'f'
                AND (k.conrelid OPERATOR(pg_catalog.=) parent::pg_catalog.oid OR k.confrelid OPERATOR(pg_catalog.=) parent::pg_catalog.oid) AND k.conparentid OPERATOR(pg_catalog.=) 0) THEN
        RAISE EXCEPTION 'table "%" has foreign keys: a partition cannot be detached in this version', parent USING ERRCODE = 'feature_not_supported';
    END IF;
    PERFORM kingres.internal_leaf_everywhere('make_parent', c.colocation, c.nsp, c.rname, partition_name::pg_catalog.name, c.dist_column::pg_catalog.text);
    PERFORM kingres.internal_leaf_everywhere('detach', c.colocation, c.nsp, c.rname, partition_name::pg_catalog.name, NULL);
    PERFORM kingres.internal_fault_point('partition_after_leaves');
    DELETE FROM kingres.dist_partition p WHERE p.parent OPERATOR(pg_catalog.=) detach_partition.parent AND p.partition_name OPERATOR(pg_catalog.=) detach_partition.partition_name;
    INSERT INTO kingres.dist_table (relid, method, dist_column, colocation_id)
    VALUES (pg_catalog.to_regclass(pg_catalog.format('%I.%I', c.nsp, partition_name)), 'hash', c.dist_column, c.colocation);
    PERFORM kingres.sync_catalog();
END;
$$;

-- A distributed table of the group of the parent (same distribution column) becomes a partition of it: its bucket partitions become leaves.
CREATE FUNCTION kingres.attach_partition(parent regclass, partition regclass, bound text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    SET client_min_messages = warning
    AS $$
DECLARE
    c        pg_catalog.record;
    v_dt     kingres.dist_table%ROWTYPE;
    v_name   pg_catalog.name;
    v_nsp    pg_catalog.name;
    v_cnt    pg_catalog.int8;
    v_own    pg_catalog.text;
BEGIN
    SELECT * INTO c FROM kingres.internal_partition_context(parent);
    PERFORM kingres.internal_fault_point('partition_after_lock');
    SELECT * INTO v_dt FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) partition;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'table "%" is not a distributed table of the group of "%"', partition, parent USING ERRCODE = 'wrong_object_type',
              HINT = 'Create the partition with create_partition() and insert the rows into the parent.';
    END IF;
    IF v_dt.method OPERATOR(pg_catalog.<>) 'hash' OR v_dt.colocation_id OPERATOR(pg_catalog.<>) c.colocation OR v_dt.dist_column OPERATOR(pg_catalog.<>) c.dist_column OR v_dt.partition_method IS NOT NULL THEN
        RAISE EXCEPTION 'table "%" must be a distributed table of the colocation group of "%", on the same distribution column, and not partitioned', partition, parent USING ERRCODE = 'wrong_object_type';
    END IF;
    SELECT n.nspname, cl.relname INTO v_nsp, v_name FROM pg_catalog.pg_class cl JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) cl.relnamespace
     WHERE cl.oid OPERATOR(pg_catalog.=) partition::pg_catalog.oid;
    IF v_nsp OPERATOR(pg_catalog.<>) c.nsp THEN
        RAISE EXCEPTION 'the partition must be in the schema of the table' USING ERRCODE = 'invalid_schema_name';
    END IF;
    -- A bucket move makes the leaves again from the definition of the table and of its partitions: an index, a constraint, a default, a
    -- storage parameter, an owner or privileges that the table has of its own would be lost at the first move (P9C-03)
    SELECT pg_catalog.string_agg(m.what, '; ' ORDER BY m.what) INTO v_own FROM kingres.internal_attach_differences(parent, partition) m(what);
    IF v_own IS NOT NULL THEN
        RAISE EXCEPTION 'table "%" has objects of its own that table "%" does not have: %', partition, parent, v_own
            USING ERRCODE = 'feature_not_supported',
                  DETAIL = 'A bucket move creates the leaves of a partition from the definition of the partitioned table, so they would be lost.',
                  HINT = 'Give the table the same indexes and constraints, owner and privileges as the partitioned table, or create the partition with create_partition() and copy the rows.';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k WHERE k.contype OPERATOR(pg_catalog.=) 'f'
                AND (k.conrelid OPERATOR(pg_catalog.=) partition::pg_catalog.oid OR k.confrelid OPERATOR(pg_catalog.=) partition::pg_catalog.oid) AND k.conparentid OPERATOR(pg_catalog.=) 0) THEN
        RAISE EXCEPTION 'table "%" has foreign keys: it cannot be attached in this version', partition USING ERRCODE = 'feature_not_supported';
    END IF;
    IF pg_catalog.octet_length(v_name) OPERATOR(pg_catalog.>) 57 THEN
        RAISE EXCEPTION 'partition name "%" is too long', v_name USING ERRCODE = 'invalid_name';
    END IF;
    PERFORM kingres.internal_leaf_everywhere('attach', c.colocation, c.nsp, c.rname, v_name, bound);
    PERFORM kingres.internal_leaf_everywhere('drop_parent', c.colocation, c.nsp, c.rname, v_name, NULL);
    PERFORM kingres.internal_fault_point('partition_after_leaves');
    DELETE FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) partition;
    INSERT INTO kingres.dist_partition (parent, partition_name, bound) VALUES (parent, v_name, bound);
    PERFORM kingres.sync_catalog();
END;
$$;

COMMENT ON FUNCTION kingres.create_partition(regclass, text, text, bool) IS 'Create a user partition of a distributed table: one leaf per bucket (map group primary, superuser)';
COMMENT ON FUNCTION kingres.drop_partition(regclass, text, bool) IS 'Drop a user partition of a distributed table with its leaves';
COMMENT ON FUNCTION kingres.detach_partition(regclass, text) IS 'Turn a user partition into a distributed table of its own; no row moves';
COMMENT ON FUNCTION kingres.attach_partition(regclass, regclass, text) IS 'Make a distributed table of the group a user partition of another; no row moves';

REVOKE ALL ON FUNCTION
    kingres.internal_leaf_op(text, name, name, name, text, int4[]),
    kingres.internal_partition_context(regclass),
    kingres.internal_leaf_everywhere(text, int4, name, name, name, text)
    FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- User-partitioned tables and the bucket move (increment 9.5, step 5): the bucket partition is a tree; the copy on the target is the same tree
-- ---------------------------------------------------------------------------

-- The storage parameters of an ordinary table as a WITH (...) list: its own, and those of its TOAST table written toast.<name>; NULL when
-- it has none (P9C-08: the parameters of the TOAST table are in the TOAST table's reloptions, not in the table's).
CREATE FUNCTION kingres.internal_table_options(p_oid oid)
    RETURNS text[]
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT NULLIF(COALESCE(c.reloptions, ARRAY[]::pg_catalog.text[])
              OPERATOR(pg_catalog.||) COALESCE((SELECT pg_catalog.array_agg('toast.' OPERATOR(pg_catalog.||) o ORDER BY o)
                                                  FROM pg_catalog.unnest(t.reloptions) o), ARRAY[]::pg_catalog.text[]),
              ARRAY[]::pg_catalog.text[])
  FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) c.reltoastrelid
 WHERE c.oid OPERATOR(pg_catalog.=) p_oid;
$$;
REVOKE ALL ON FUNCTION kingres.internal_table_options(oid) FROM PUBLIC;

-- The shape of a user-partitioned bucket partition: the key and one entry per leaf (name, bound as PostgreSQL writes it, access method).  '{}' for a plain one.
CREATE FUNCTION kingres.internal_move_tree(p_oid oid)
    RETURNS jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.jsonb_build_object('reloptions', pg_catalog.to_jsonb(kingres.internal_table_options(c.oid))) OPERATOR(pg_catalog.||) CASE WHEN c.relkind OPERATOR(pg_catalog.=) 'p'
            THEN pg_catalog.jsonb_build_object(
                     'partby', pg_catalog.pg_get_partkeydef(c.oid),
                     'leaves', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                                                'name', lc.relname, 'bound', pg_catalog.pg_get_expr(lc.relpartbound, lc.oid), 'am', COALESCE(a.amname, 'heap'))
                                            ORDER BY lc.relname)
                                           FROM pg_catalog.pg_inherits i JOIN pg_catalog.pg_class lc ON lc.oid OPERATOR(pg_catalog.=) i.inhrelid
                                           LEFT JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) lc.relam
                                          WHERE i.inhparent OPERATOR(pg_catalog.=) c.oid), '[]'::pg_catalog.jsonb))
            ELSE '{}'::pg_catalog.jsonb END
  FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) p_oid;
$$;
REVOKE ALL ON FUNCTION kingres.internal_move_tree(oid) FROM PUBLIC;

CREATE OR REPLACE FUNCTION kingres.internal_move_inspect(p_role text, p_bucket int, p_bucket_count int, p_tables jsonb)
    RETURNS jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t         pg_catalog.record;
    v_ntab    pg_catalog.int4 := pg_catalog.jsonb_array_length(p_tables);
    v_sync    pg_catalog.int4 := LEAST(pg_catalog.current_setting('max_sync_workers_per_subscription')::pg_catalog.int4, v_ntab + 1);
    v_free    pg_catalog.int4;
    v_node    pg_catalog.int4;
    v_out     pg_catalog.jsonb := '[]'::pg_catalog.jsonb;
    v_part    pg_catalog.text;
    v_oid     pg_catalog.oid;
    v_ident   pg_catalog.text;
    v_pk      pg_catalog.bool;
    v_col     pg_catalog.text;
    v_origins pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT l.node_id INTO v_node FROM kingres.local_node l;

    IF pg_catalog.current_setting('wal_level') OPERATOR(pg_catalog.<>) 'logical' THEN
        RAISE EXCEPTION 'wal_level is "%" on node %, a bucket move needs wal_level = logical', pg_catalog.current_setting('wal_level'), v_node
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'Set wal_level = logical on every node and restart them.';
    END IF;
    IF pg_catalog.current_setting('max_logical_replication_workers')::pg_catalog.int4 < 1 + v_sync THEN
        RAISE EXCEPTION 'max_logical_replication_workers is %, node % needs at least %',
            pg_catalog.current_setting('max_logical_replication_workers'), v_node, 1 + v_sync
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'Raise max_logical_replication_workers (and max_worker_processes) and restart the node.';
    END IF;

    IF p_role OPERATOR(pg_catalog.=) 'source' THEN
        SELECT pg_catalog.current_setting('max_replication_slots')::pg_catalog.int4 - pg_catalog.count(*)
          INTO v_free FROM pg_catalog.pg_replication_slots;
        IF v_free < 1 + v_sync THEN
            RAISE EXCEPTION 'node % has % free replication slots (max_replication_slots = %), a move needs %',
                v_node, v_free, pg_catalog.current_setting('max_replication_slots'), 1 + v_sync
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Raise max_replication_slots and restart the node, or finish the moves that hold slots.';
        END IF;
        SELECT pg_catalog.current_setting('max_wal_senders')::pg_catalog.int4 - pg_catalog.count(*)
          INTO v_free FROM pg_catalog.pg_stat_replication;
        IF v_free < 1 + v_sync THEN
            RAISE EXCEPTION 'node % has % free WAL senders (max_wal_senders = %), a move needs %',
                v_node, v_free, pg_catalog.current_setting('max_wal_senders'), 1 + v_sync
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Raise max_wal_senders and restart the node.';
        END IF;
    ELSE
        v_origins := pg_catalog.current_setting('max_active_replication_origins', true);
        IF v_origins IS NOT NULL THEN
            SELECT v_origins::pg_catalog.int4 - pg_catalog.count(*) INTO v_free FROM pg_catalog.pg_replication_origin;
            IF v_free < 1 + v_sync THEN
                RAISE EXCEPTION 'node % has % free replication origins (max_active_replication_origins = %), a move needs %',
                    v_node, v_free, v_origins, 1 + v_sync
                    USING ERRCODE = 'object_not_in_prerequisite_state',
                          HINT = 'Raise max_active_replication_origins and restart the node.';
            END IF;
        END IF;
    END IF;

    FOR t IN SELECT x.schema_name, x.table_name
               FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text)
    LOOP
        v_part := kingres.internal_bucket_part_name(t.table_name, p_bucket);
        v_oid := pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, v_part));
        IF p_role OPERATOR(pg_catalog.=) 'source' THEN
            IF v_oid IS NULL OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                                             WHERE c.oid OPERATOR(pg_catalog.=) v_oid AND c.relispartition) THEN
                RAISE EXCEPTION 'bucket partition "%.%" is not attached on node %', t.schema_name, v_part, v_node
                    USING ERRCODE = 'object_not_in_prerequisite_state';
            END IF;
            SELECT c.relreplident::pg_catalog.text INTO v_ident FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) v_oid;
            SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_index i
                            WHERE i.indrelid OPERATOR(pg_catalog.=) v_oid AND i.indisprimary) INTO v_pk;
            IF v_ident OPERATOR(pg_catalog.=) 'n' OR (v_ident OPERATOR(pg_catalog.=) 'd' AND NOT v_pk) THEN
                RAISE EXCEPTION 'table "%.%" has no replica identity', t.schema_name, t.table_name
                    USING ERRCODE = 'object_not_in_prerequisite_state',
                          DETAIL = 'Logical replication needs a primary key or REPLICA IDENTITY FULL to replicate updates and deletes of the bucket.',
                          HINT = pg_catalog.format('Add a primary key, or set REPLICA IDENTITY FULL on the bucket partitions of %I.%I on the node that has them (ALTER TABLE is not propagated for it: superuser, kingres_shard.enable_routing = off).', t.schema_name, t.table_name);
            END IF;
            IF v_ident OPERATOR(pg_catalog.=) 'i' THEN
                RAISE EXCEPTION 'table "%.%" uses REPLICA IDENTITY USING INDEX, which a bucket move does not support yet', t.schema_name, t.table_name
                    USING ERRCODE = 'feature_not_supported',
                          HINT = 'Use a primary key or REPLICA IDENTITY FULL.';
            END IF;
            SELECT a.attname INTO v_col FROM pg_catalog.pg_attribute a
             WHERE a.attrelid OPERATOR(pg_catalog.=) v_oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
               AND a.attidentity OPERATOR(pg_catalog.<>) '' LIMIT 1;
            IF FOUND THEN
                RAISE EXCEPTION 'column "%" of table "%.%" is an identity column, which a bucket move does not support yet', v_col, t.schema_name, t.table_name
                    USING ERRCODE = 'feature_not_supported';
            END IF;
            v_out := v_out OPERATOR(pg_catalog.||) (pg_catalog.jsonb_build_object(
                'schema_name', t.schema_name, 'table_name', t.table_name, 'replident', v_ident)
                OPERATOR(pg_catalog.||) kingres.internal_move_tree(v_oid));
        ELSE
            IF v_oid IS NOT NULL THEN
                RAISE EXCEPTION 'relation "%.%" already exists on node %', t.schema_name, v_part, v_node
                    USING ERRCODE = 'duplicate_table',
                          HINT = 'It may be left over from an earlier move; see kingres.check_cluster().';
            END IF;
            IF pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, t.table_name)) IS NULL THEN
                RAISE EXCEPTION 'table "%.%" does not exist on node %', t.schema_name, t.table_name, v_node
                    USING ERRCODE = 'undefined_table';
            END IF;
            v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object(
                'schema_name', t.schema_name, 'table_name', t.table_name);
        END IF;
    END LOOP;
    RETURN v_out;
END;
$$;


CREATE OR REPLACE FUNCTION kingres.internal_create_move_tables(p_move bigint, p_bucket int, p_bucket_count int, p_tables jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t       pg_catalog.record;
    v_part  pg_catalog.text;
    v_col   pg_catalog.text;
    v_owner pg_catalog.text;
    v_parent pg_catalog.regclass;
    l       pg_catalog.record;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR t IN SELECT x.schema_name, x.table_name, x.replident, x.partby, x.leaves, x.reloptions
               FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text, replident text, partby text, leaves jsonb, reloptions text[])
    LOOP
        v_part := kingres.internal_bucket_part_name(t.table_name, p_bucket);
        v_parent := pg_catalog.format('%I.%I', t.schema_name, t.table_name)::pg_catalog.regclass;
        SELECT d.dist_column INTO v_col FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_parent;
        SELECT r.rolname INTO v_owner FROM pg_catalog.pg_class c JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) c.relowner
         WHERE c.oid OPERATOR(pg_catalog.=) v_parent::pg_catalog.oid;
        IF pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, v_part)) IS NOT NULL THEN
            CONTINUE;	-- made by an earlier attempt of this move
        END IF;
        -- LIKE does not copy the storage parameters: they are those of the bucket partition on the source
        EXECUTE pg_catalog.format('CREATE TABLE %I.%I (LIKE %I.%I INCLUDING ALL)%s %s',
                                  t.schema_name, v_part, t.schema_name, t.table_name,
                                  CASE WHEN t.partby IS NOT NULL THEN ' PARTITION BY ' OPERATOR(pg_catalog.||) t.partby ELSE '' END,
                                  kingres.internal_with_clause(t.reloptions));
        IF t.partby IS NOT NULL THEN
            -- a user-partitioned bucket partition: the copy is the same tree, a leaf per user partition with its bound and its access method
            FOR l IN SELECT y.name, y.bound, y.am FROM pg_catalog.jsonb_to_recordset(t.leaves) AS y(name text, bound text, am text) ORDER BY y.name LOOP
                EXECUTE pg_catalog.format('CREATE TABLE %I.%I PARTITION OF %I.%I %s USING %I', t.schema_name, l.name, t.schema_name, v_part, l.bound, l.am);
            END LOOP;
        END IF;
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ADD CONSTRAINT ks_bucket_check CHECK '
                                  '(pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass, %s, %s, %I))',
                                  t.schema_name, v_part, v_parent::pg_catalog.text, p_bucket_count, p_bucket, v_col);
        IF t.replident OPERATOR(pg_catalog.=) 'f' THEN
            EXECUTE pg_catalog.format('ALTER TABLE %I.%I REPLICA IDENTITY FULL', t.schema_name, v_part);
        END IF;
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I OWNER TO %I', t.schema_name, v_part, v_owner);
        IF t.partby IS NOT NULL THEN
            FOR l IN SELECT y.name FROM pg_catalog.jsonb_to_recordset(t.leaves) AS y(name text) LOOP
                EXECUTE pg_catalog.format('ALTER TABLE %I.%I OWNER TO %I', t.schema_name, l.name, v_owner);
            END LOOP;
        END IF;
        EXECUTE pg_catalog.format('COMMENT ON TABLE %I.%I IS %L', t.schema_name, v_part,
                                  'kingres_shard bucket move ' OPERATOR(pg_catalog.||) p_move::pg_catalog.text);
    END LOOP;
END;
$$;


CREATE OR REPLACE FUNCTION kingres.internal_drop_move_tables(p_move bigint, p_bucket int, p_tables jsonb)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t       pg_catalog.record;
    v_part  pg_catalog.text;
    v_oid   pg_catalog.oid;
    n       pg_catalog.int4 := 0;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR t IN SELECT x.schema_name, x.table_name
               FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text)
    LOOP
        v_part := kingres.internal_bucket_part_name(t.table_name, p_bucket);
        v_oid := pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, v_part));
        CONTINUE WHEN v_oid IS NULL;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                    WHERE c.oid OPERATOR(pg_catalog.=) v_oid AND NOT c.relispartition AND c.relkind IN ('r', 'p'))
           AND pg_catalog.obj_description(v_oid, 'pg_class') IS NOT DISTINCT FROM
               ('kingres_shard bucket move ' OPERATOR(pg_catalog.||) p_move::pg_catalog.text) THEN
            EXECUTE pg_catalog.format('DROP TABLE %I.%I', t.schema_name, v_part);
            n := n + 1;
        END IF;
    END LOOP;
    RETURN n;
END;
$$;


CREATE OR REPLACE FUNCTION kingres.internal_create_move_publication(p_move bigint, p_bucket int, p_tables jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_name  pg_catalog.text := pg_catalog.format('ks_move_%s', p_move);
    v_list  pg_catalog.text;
    v_root  pg_catalog.bool;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT pg_catalog.string_agg(pg_catalog.format('%I.%I', x.schema_name,
                                                   kingres.internal_bucket_part_name(x.table_name, p_bucket)), ', ')
      INTO v_list
      FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text);
    -- a user-partitioned bucket partition is published under its own name, not under the names of its leaves
    SELECT EXISTS (SELECT 1 FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text)
                     JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass(pg_catalog.format('%I.%I', x.schema_name, kingres.internal_bucket_part_name(x.table_name, p_bucket)))::pg_catalog.oid
                    WHERE c.relkind OPERATOR(pg_catalog.=) 'p') INTO v_root;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_publication p WHERE p.pubname OPERATOR(pg_catalog.=) v_name) THEN
        EXECUTE pg_catalog.format('DROP PUBLICATION %I', v_name);
    END IF;
    EXECUTE pg_catalog.format('CREATE PUBLICATION %I FOR TABLE %s, kingres.move_marker WHERE (move_id = %s) '
                              'WITH (publish = ''insert, update, delete, truncate'', publish_via_partition_root = %s)',
                              v_name, v_list, p_move, CASE WHEN v_root THEN 'true' ELSE 'false' END);
END;
$$;


CREATE OR REPLACE FUNCTION kingres.internal_drop_orphans(p_move bigint)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r pg_catalog.record;
    n pg_catalog.int4 := 0;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SET LOCAL lock_timeout = '5s';
    FOR r IN SELECT c.relname FROM pg_catalog.pg_class c
              WHERE c.relnamespace OPERATOR(pg_catalog.=) 'kingres'::pg_catalog.regnamespace
                AND c.relkind IN ('r', 'p')
                AND NOT c.relispartition
                AND pg_catalog.starts_with(c.relname, pg_catalog.format('orphan_%s_', p_move))
              ORDER BY c.relname
    LOOP
        EXECUTE pg_catalog.format('DROP TABLE kingres.%I', r.relname);
        n := n + 1;
    END LOOP;
    RETURN n;
END;
$$;


-- the size of a bucket partition is the size of its leaves when it is partitioned
CREATE OR REPLACE VIEW kingres.buckets AS
    SELECT d.relid AS table_name,
           b.bucket,
           p.partition_name,
           b.group_id,
           n.node_id,
           b.state,
           CASE WHEN lp.part IS NOT NULL
                THEN (SELECT pg_catalog.sum(pg_catalog.pg_total_relation_size(pt.relid))::pg_catalog.int8 FROM pg_catalog.pg_partition_tree(lp.part) pt WHERE pt.isleaf)
           END AS partition_size
      FROM kingres.dist_table d
      JOIN kingres.bucket_map b ON b.colocation_id = d.colocation_id
      JOIN pg_catalog.pg_class c ON c.oid = d.relid::oid
      JOIN pg_catalog.pg_namespace ns ON ns.oid = c.relnamespace
      CROSS JOIN LATERAL
           (SELECT pg_catalog.format('%s_b%s', c.relname,
                                     pg_catalog.lpad(b.bucket::text, 4, '0'))
                   AS partition_name) p
      LEFT JOIN kingres.node n
             ON n.group_id = b.group_id AND n.role = 'primary' AND n.is_active
      LEFT JOIN LATERAL
           (SELECT pg_catalog.to_regclass(
                       pg_catalog.quote_ident(ns.nspname) || '.' ||
                       pg_catalog.quote_ident(p.partition_name)) AS part) lp
             ON true;

-- ---------------------------------------------------------------------------
-- Increment 9.8: dependent plain views are recreated on the new table (kingres_shard.dependent_views = 'recreate')
-- ---------------------------------------------------------------------------

-- On this node: the plain views that depend on the tables, directly or through other views, in the order in which they can be created, with
-- everything that recreates them (definition, columns, owner, options, privileges, comments).  "blockers" names what cannot be recreated:
-- a materialized view, a rule other than a view's own, a trigger on a view, a privilege on a column of a view, a function that uses the row type
-- of a view, a publication of a view.
CREATE FUNCTION kingres.internal_view_capture(p_rels regclass[])
    RETURNS jsonb
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_tabs   pg_catalog.oid[];
    v_front  pg_catalog.oid[];
    v_new    pg_catalog.oid[];
    v_all    pg_catalog.oid[] := ARRAY[]::pg_catalog.oid[];
    v_done   pg_catalog.oid[] := ARRAY[]::pg_catalog.oid[];
    v_pick   pg_catalog.oid;
    v_blk    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_views  pg_catalog.jsonb := '[]'::pg_catalog.jsonb;
    c        pg_catalog.record;
    q        pg_catalog.text;
BEGIN
    SELECT pg_catalog.array_agg(x::pg_catalog.oid) INTO v_tabs FROM pg_catalog.unnest(p_rels) x;
    v_front := v_tabs;
    LOOP
        SELECT pg_catalog.array_agg(DISTINCT cl.oid) INTO v_new
          FROM pg_catalog.pg_depend d
          JOIN pg_catalog.pg_rewrite rw ON d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass AND rw.oid OPERATOR(pg_catalog.=) d.objid
          JOIN pg_catalog.pg_class cl ON cl.oid OPERATOR(pg_catalog.=) rw.ev_class
         WHERE d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
           AND d.refobjid OPERATOR(pg_catalog.=) ANY (v_front)
           AND cl.oid OPERATOR(pg_catalog.<>) d.refobjid
           AND NOT (cl.oid OPERATOR(pg_catalog.=) ANY (v_tabs)) AND NOT (cl.oid OPERATOR(pg_catalog.=) ANY (v_all));
        EXIT WHEN v_new IS NULL;
        v_all := v_all OPERATOR(pg_catalog.||) v_new;
        v_front := v_new;
    END LOOP;

    FOR c IN SELECT cl.oid, cl.relkind, cl.relname, n.nspname, cl.reltype FROM pg_catalog.pg_class cl JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) cl.relnamespace
              WHERE cl.oid OPERATOR(pg_catalog.=) ANY (v_all) ORDER BY n.nspname, cl.relname
    LOOP
        q := pg_catalog.format('%I.%I', c.nspname, c.relname);
        IF c.relkind OPERATOR(pg_catalog.<>) 'v' THEN
            v_blk := v_blk OPERATOR(pg_catalog.||) pg_catalog.format('%s %s depends on the table and is not a plain view',
                         CASE c.relkind WHEN 'm' THEN 'materialized view' ELSE 'object' END, q);
            CONTINUE;
        END IF;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite rw WHERE rw.ev_class OPERATOR(pg_catalog.=) c.oid AND rw.rulename OPERATOR(pg_catalog.<>) '_RETURN') THEN
            v_blk := v_blk OPERATOR(pg_catalog.||) pg_catalog.format('view %s has a rule of its own', q);
        END IF;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t WHERE t.tgrelid OPERATOR(pg_catalog.=) c.oid AND NOT t.tgisinternal) THEN
            v_blk := v_blk OPERATOR(pg_catalog.||) pg_catalog.format('view %s has a trigger', q);
        END IF;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) c.oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND a.attacl IS NOT NULL) THEN
            v_blk := v_blk OPERATOR(pg_catalog.||) pg_catalog.format('view %s has privileges on columns', q);
        END IF;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_proc p ON p.oid OPERATOR(pg_catalog.=) d.objid
                    WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_proc'::pg_catalog.regclass AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_type'::pg_catalog.regclass
                      AND d.refobjid OPERATOR(pg_catalog.=) c.reltype) THEN
            v_blk := v_blk OPERATOR(pg_catalog.||) pg_catalog.format('a function uses the row type of view %s', q);
        END IF;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_publication_rel pr WHERE pr.prrelid OPERATOR(pg_catalog.=) c.oid) THEN
            v_blk := v_blk OPERATOR(pg_catalog.||) pg_catalog.format('a publication publishes view %s', q);
        END IF;
    END LOOP;

    -- the order of creation: a view comes after the views it uses
    WHILE pg_catalog.cardinality(v_done) OPERATOR(pg_catalog.<) pg_catalog.cardinality(v_all) LOOP
        v_pick := NULL;
        SELECT cl.oid INTO v_pick FROM pg_catalog.pg_class cl JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) cl.relnamespace
         WHERE cl.oid OPERATOR(pg_catalog.=) ANY (v_all) AND NOT (cl.oid OPERATOR(pg_catalog.=) ANY (v_done))
           AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_rewrite rw ON d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass AND rw.oid OPERATOR(pg_catalog.=) d.objid
                            WHERE rw.ev_class OPERATOR(pg_catalog.=) cl.oid AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
                              AND d.refobjid OPERATOR(pg_catalog.=) ANY (v_all) AND d.refobjid OPERATOR(pg_catalog.<>) cl.oid AND NOT (d.refobjid OPERATOR(pg_catalog.=) ANY (v_done)))
         ORDER BY n.nspname, cl.relname LIMIT 1;
        IF v_pick IS NULL THEN
            v_blk := v_blk OPERATOR(pg_catalog.||) 'the views depend on each other in a cycle'::pg_catalog.text;
            EXIT;
        END IF;
        v_done := v_done OPERATOR(pg_catalog.||) v_pick;
        SELECT v_views OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object(
                   'schema', n.nspname, 'name', cl.relname,
                   'cols', (SELECT pg_catalog.jsonb_agg(a.attname ORDER BY a.attnum) FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) cl.oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped),
                   'def', pg_catalog.regexp_replace(pg_catalog.pg_get_viewdef(cl.oid, true), ';[[:space:]]*$', ''),
                   'owner', pg_catalog.pg_get_userbyid(cl.relowner),
                   'options', COALESCE(pg_catalog.to_jsonb(cl.reloptions), 'null'::pg_catalog.jsonb),
                   'has_acl', cl.relacl IS NOT NULL,
                   'acl', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                                        'grantee', CASE WHEN x.grantee OPERATOR(pg_catalog.=) 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(x.grantee) END,
                                        'priv', x.privilege_type, 'grant', x.is_grantable) ORDER BY x.grantee, x.privilege_type)
                                      FROM pg_catalog.aclexplode(cl.relacl) x), '[]'::pg_catalog.jsonb),
                   'comment', pg_catalog.obj_description(cl.oid, 'pg_class'),
                   'colcomments', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('col', a.attname, 'comment', pg_catalog.col_description(cl.oid, a.attnum)) ORDER BY a.attnum)
                                              FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) cl.oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
                                               AND pg_catalog.col_description(cl.oid, a.attnum) IS NOT NULL), '[]'::pg_catalog.jsonb)))
          INTO v_views
          FROM pg_catalog.pg_class cl JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) cl.relnamespace WHERE cl.oid OPERATOR(pg_catalog.=) v_pick;
    END LOOP;
    RETURN pg_catalog.jsonb_build_object('views', v_views, 'blockers', pg_catalog.to_jsonb(v_blk));
END;
$$;

-- On this node: drop the captured views, the last first.
CREATE FUNCTION kingres.internal_view_drop(p_doc jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v pg_catalog.record;
BEGIN
    IF p_doc IS NULL OR pg_catalog.jsonb_array_length(COALESCE(p_doc OPERATOR(pg_catalog.->) 'views', '[]'::pg_catalog.jsonb)) OPERATOR(pg_catalog.=) 0 THEN
        RETURN;
    END IF;
    FOR v IN SELECT e.value AS j FROM pg_catalog.jsonb_array_elements(p_doc OPERATOR(pg_catalog.->) 'views') WITH ORDINALITY e(value, ord) ORDER BY e.ord DESC LOOP
        EXECUTE pg_catalog.format('DROP VIEW %I.%I', v.j OPERATOR(pg_catalog.->>) 'schema', v.j OPERATOR(pg_catalog.->>) 'name');
    END LOOP;
END;
$$;

-- On this node: create the captured views again, on the table that has the name of the old one now.
CREATE FUNCTION kingres.internal_view_recreate(p_doc jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v pg_catalog.record;
    a pg_catalog.record;
    q pg_catalog.text;
    o pg_catalog.text;
BEGIN
    IF p_doc IS NULL THEN
        RETURN;
    END IF;
    FOR v IN SELECT e.value AS j FROM pg_catalog.jsonb_array_elements(COALESCE(p_doc OPERATOR(pg_catalog.->) 'views', '[]'::pg_catalog.jsonb)) WITH ORDINALITY e(value, ord) ORDER BY e.ord LOOP
        q := pg_catalog.format('%I.%I', v.j OPERATOR(pg_catalog.->>) 'schema', v.j OPERATOR(pg_catalog.->>) 'name');
        o := NULL;
        IF pg_catalog.jsonb_typeof(v.j OPERATOR(pg_catalog.->) 'options') OPERATOR(pg_catalog.=) 'array' THEN
            SELECT 'WITH (' OPERATOR(pg_catalog.||) pg_catalog.string_agg(x, ', ') OPERATOR(pg_catalog.||) ')' INTO o FROM pg_catalog.jsonb_array_elements_text(v.j OPERATOR(pg_catalog.->) 'options') x;
        END IF;
        EXECUTE pg_catalog.format('CREATE VIEW %s (%s) %s AS %s', q,
                    (SELECT pg_catalog.string_agg(pg_catalog.quote_ident(x), ', ') FROM pg_catalog.jsonb_array_elements_text(v.j OPERATOR(pg_catalog.->) 'cols') x),
                    COALESCE(o, ''), v.j OPERATOR(pg_catalog.->>) 'def');
        EXECUTE pg_catalog.format('ALTER VIEW %s OWNER TO %I', q, v.j OPERATOR(pg_catalog.->>) 'owner');
        -- The ACL of the new view is the one of the old view and nothing else: the default privileges of the schema (ALTER DEFAULT
        -- PRIVILEGES) gave it grants that the old view never had (P9C-05).  Every grant is taken back, whether or not the old view had an
        -- ACL; the owner gets all privileges again when it had none, otherwise the captured entries, the owner's own among them, follow.
        -- A view with no ACL of its own, made where nothing was granted by default, keeps the default ACL (NULL), as the old view had it.
        IF (v.j OPERATOR(pg_catalog.->>) 'has_acl')::pg_catalog.bool
           OR (SELECT c.relacl IS NOT NULL FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) q::pg_catalog.regclass::pg_catalog.oid) THEN
            PERFORM kingres.internal_reset_acl(q::pg_catalog.regclass, NOT (v.j OPERATOR(pg_catalog.->>) 'has_acl')::pg_catalog.bool);
        END IF;
        IF (v.j OPERATOR(pg_catalog.->>) 'has_acl')::pg_catalog.bool THEN
            FOR a IN SELECT e.value ->> 'grantee' AS grantee, e.value ->> 'priv' AS priv, (e.value ->> 'grant')::pg_catalog.bool AS gr
                       FROM pg_catalog.jsonb_array_elements(v.j OPERATOR(pg_catalog.->) 'acl') e(value) LOOP
                EXECUTE pg_catalog.format('GRANT %s ON %s TO %s%s', a.priv, q,
                            CASE WHEN a.grantee OPERATOR(pg_catalog.=) 'PUBLIC' THEN 'PUBLIC' ELSE pg_catalog.quote_ident(a.grantee) END,
                            CASE WHEN a.gr THEN ' WITH GRANT OPTION' ELSE '' END);
            END LOOP;
        END IF;
        IF v.j OPERATOR(pg_catalog.->>) 'comment' IS NOT NULL THEN
            EXECUTE pg_catalog.format('COMMENT ON VIEW %s IS %L', q, v.j OPERATOR(pg_catalog.->>) 'comment');
        END IF;
        FOR a IN SELECT e.value ->> 'col' AS col, e.value ->> 'comment' AS cm FROM pg_catalog.jsonb_array_elements(v.j OPERATOR(pg_catalog.->) 'colcomments') e(value) LOOP
            EXECUTE pg_catalog.format('COMMENT ON COLUMN %s.%I IS %L', q, a.col, a.cm);
        END LOOP;
    END LOOP;
END;
$$;

-- The captured views of every node that holds the tables (or of this node only), as {node id: document}; {} when the setting is "refuse".
-- A view that cannot be recreated stops the operation here.
CREATE FUNCTION kingres.internal_views_capture(p_rels text[], p_all bool, p_mode text, p_only int DEFAULT NULL)
    RETURNS jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    nd    pg_catalog.record;
    v_doc pg_catalog.jsonb;
    v_out pg_catalog.jsonb := '{}'::pg_catalog.jsonb;
    v_self pg_catalog.int4;
    v_lit pg_catalog.text;
BEGIN
    IF p_mode IS DISTINCT FROM 'recreate' THEN
        RETURN v_out;
    END IF;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    SELECT pg_catalog.string_agg(pg_catalog.quote_literal(x), ', ') INTO v_lit FROM pg_catalog.unnest(p_rels) x;
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND (p_all OR n.node_id OPERATOR(pg_catalog.=) v_self) ORDER BY n.node_id LOOP
        IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
            v_doc := kingres.internal_view_capture((SELECT pg_catalog.array_agg(x::pg_catalog.regclass) FROM pg_catalog.unnest(p_rels) x));
        ELSE
            v_doc := kingres.internal_exec_scalar(nd.node_id, pg_catalog.format('SELECT kingres.internal_view_capture(ARRAY[%s]::pg_catalog.regclass[])::pg_catalog.text', v_lit))::pg_catalog.jsonb;
        END IF;
        IF pg_catalog.jsonb_array_length(v_doc OPERATOR(pg_catalog.->) 'blockers') OPERATOR(pg_catalog.>) 0 THEN
            RAISE EXCEPTION 'a view that depends on the table cannot be recreated on node %: %', nd.node_id,
                  (SELECT pg_catalog.string_agg(x, '; ') FROM pg_catalog.jsonb_array_elements_text(v_doc OPERATOR(pg_catalog.->) 'blockers') x)
                  USING ERRCODE = 'feature_not_supported';
        END IF;
        IF p_only IS NOT NULL AND nd.node_id OPERATOR(pg_catalog.<>) p_only AND pg_catalog.jsonb_array_length(v_doc OPERATOR(pg_catalog.->) 'views') OPERATOR(pg_catalog.>) 0 THEN
            RAISE EXCEPTION 'the views on node % cannot be recreated: the table will be on node % only', nd.node_id, p_only USING ERRCODE = 'feature_not_supported';
        END IF;
        IF pg_catalog.jsonb_array_length(v_doc OPERATOR(pg_catalog.->) 'views') OPERATOR(pg_catalog.>) 0 THEN
            v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object(nd.node_id::pg_catalog.text, v_doc);
        END IF;
    END LOOP;
    RETURN v_out;
END;
$$;

CREATE FUNCTION kingres.internal_views_drop(p_docs jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    e pg_catalog.record;
    v_routing pg_catalog.text := COALESCE(pg_catalog.current_setting('kingres_shard.enable_routing', true), 'on');
    v_self pg_catalog.int4 := (SELECT l.node_id FROM kingres.local_node l);
BEGIN
    -- the statements of the extension are off while the views are changed, here and in the branches; the caller's setting comes back
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    FOR e IN SELECT d.key, d.value FROM pg_catalog.jsonb_each(COALESCE(p_docs, '{}'::pg_catalog.jsonb)) d ORDER BY d.key::pg_catalog.int4 LOOP
        IF e.key::pg_catalog.int4 OPERATOR(pg_catalog.=) v_self THEN
            PERFORM kingres.internal_view_drop(e.value);
        ELSE
            PERFORM kingres.internal_run_on(e.key::pg_catalog.int4, pg_catalog.format('SET LOCAL kingres_shard.enable_routing = off; SELECT kingres.internal_view_drop(%L::pg_catalog.jsonb)', e.value::pg_catalog.text));
        END IF;
    END LOOP;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_routing, true);
END;
$$;

CREATE FUNCTION kingres.internal_views_recreate(p_docs jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    e pg_catalog.record;
    v_routing pg_catalog.text := COALESCE(pg_catalog.current_setting('kingres_shard.enable_routing', true), 'on');
    v_self pg_catalog.int4 := (SELECT l.node_id FROM kingres.local_node l);
BEGIN
    -- the statements of the extension are off while the views are changed, here and in the branches; the caller's setting comes back
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    FOR e IN SELECT d.key, d.value FROM pg_catalog.jsonb_each(COALESCE(p_docs, '{}'::pg_catalog.jsonb)) d ORDER BY d.key::pg_catalog.int4 LOOP
        IF e.key::pg_catalog.int4 OPERATOR(pg_catalog.=) v_self THEN
            PERFORM kingres.internal_view_recreate(e.value);
        ELSE
            PERFORM kingres.internal_run_on(e.key::pg_catalog.int4, pg_catalog.format('SET LOCAL kingres_shard.enable_routing = off; SELECT kingres.internal_view_recreate(%L::pg_catalog.jsonb)', e.value::pg_catalog.text));
        END IF;
    END LOOP;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_routing, true);
END;
$$;

-- What the report says about the views of the tables when the setting is "recreate": what cannot be recreated, on which node.
CREATE FUNCTION kingres.internal_view_blockers(p_rels text[], p_all bool, p_only int DEFAULT NULL)
    RETURNS TABLE (detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    nd    pg_catalog.record;
    v_doc pg_catalog.jsonb;
    v_self pg_catalog.int4;
    v_lit pg_catalog.text;
    x     pg_catalog.text;
BEGIN
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    SELECT pg_catalog.string_agg(pg_catalog.quote_literal(r), ', ') INTO v_lit FROM pg_catalog.unnest(p_rels) r;
    FOR nd IN SELECT n.node_id FROM kingres.node n WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND (p_all OR n.node_id OPERATOR(pg_catalog.=) v_self) ORDER BY n.node_id LOOP
        BEGIN
            -- in a session of its own on every node, this one too: the caller's transaction keeps no lock on the table, which the switch needs
            v_doc := kingres.internal_exec_scalar(nd.node_id, pg_catalog.format('SELECT kingres.internal_view_capture(ARRAY[%s]::pg_catalog.regclass[])::pg_catalog.text', v_lit))::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            CONTINUE;
        END;
        FOR x IN SELECT e FROM pg_catalog.jsonb_array_elements_text(v_doc OPERATOR(pg_catalog.->) 'blockers') e LOOP
            detail := pg_catalog.format('node %s: %s', nd.node_id, x);
            RETURN NEXT;
        END LOOP;
        IF p_only IS NOT NULL AND nd.node_id OPERATOR(pg_catalog.<>) p_only AND pg_catalog.jsonb_array_length(v_doc OPERATOR(pg_catalog.->) 'views') OPERATOR(pg_catalog.>) 0 THEN
            detail := pg_catalog.format('node %s: the views on the table cannot be recreated there: the table will be on node %s only', nd.node_id, p_only);
            RETURN NEXT;
        END IF;
    END LOOP;
END;
$$;

REVOKE ALL ON FUNCTION
    kingres.internal_view_capture(regclass[]),
    kingres.internal_view_drop(jsonb),
    kingres.internal_view_recreate(jsonb),
    kingres.internal_views_capture(text[], bool, text, int),
    kingres.internal_views_drop(jsonb),
    kingres.internal_views_recreate(jsonb),
    kingres.internal_view_blockers(text[], bool, int)
    FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- Increment 9.9: the storage parameters of a table are carried
-- ---------------------------------------------------------------------------

-- The storage parameters of a table that is about to be replaced: the recorded ones for a distributed table (its parent cannot have them),
-- the table's own otherwise.
CREATE FUNCTION kingres.internal_reloptions_of(p_relid regclass)
    RETURNS text[]
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT COALESCE((SELECT d.reloptions FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) p_relid),
                kingres.internal_table_options(p_relid::pg_catalog.oid));
$$;

-- WITH (...) for CREATE TABLE, or nothing.
CREATE FUNCTION kingres.internal_with_clause(p_opts text[])
    RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT CASE WHEN p_opts IS NULL OR pg_catalog.cardinality(p_opts) OPERATOR(pg_catalog.=) 0 THEN ''
            ELSE 'WITH (' OPERATOR(pg_catalog.||) pg_catalog.array_to_string(p_opts, ', ') OPERATOR(pg_catalog.||) ')' END;
$$;

REVOKE ALL ON FUNCTION kingres.internal_reloptions_of(regclass), kingres.internal_with_clause(text[]) FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- The fixes for the review of 9.4, 9.5, 9.8 and 9.9 (P9C-01 to P9C-14)
-- ---------------------------------------------------------------------------

-- The blocking calls copy rows (or read the catalogs they decide with) after the lock that stops the writers.  Only READ COMMITTED
-- gives them a snapshot taken after the lock: a transaction that holds one snapshot for its whole life would copy the rows as they
-- were before the lock, and the old table is dropped in the same transaction (P9C-01).
CREATE FUNCTION kingres.internal_require_read_committed(p_call text)
    RETURNS void
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    IF pg_catalog.current_setting('transaction_isolation') OPERATOR(pg_catalog.<>) 'read committed' THEN
        RAISE EXCEPTION '% cannot run in a REPEATABLE READ or SERIALIZABLE transaction', p_call
            USING ERRCODE = 'active_sql_transaction',
                  DETAIL = 'The rows are copied as of the moment the writers are stopped, which a transaction snapshot taken earlier does not show.',
                  HINT = 'Run the call in a READ COMMITTED transaction.';
    END IF;
END;
$$;

-- Why a colocation group cannot be changed or joined now: a table operation holds it, a bucket is being moved, or a bucket is not
-- active.  NULL when it is free.
CREATE FUNCTION kingres.internal_group_busy(p_colocation int)
    RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT CASE
         WHEN EXISTS (SELECT 1 FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) p_colocation)
              THEN pg_catalog.format('colocation group %s is busy: a table operation uses it', p_colocation)
         WHEN EXISTS (SELECT 1 FROM kingres.bucket_move m WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation
                         AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching'))
              THEN pg_catalog.format('colocation group %s is busy: a bucket is being moved', p_colocation)
         WHEN EXISTS (SELECT 1 FROM kingres.bucket_map b WHERE b.colocation_id OPERATOR(pg_catalog.=) p_colocation AND b.state OPERATOR(pg_catalog.<>) 'active')
              THEN pg_catalog.format('colocation group %s is busy: a bucket is not active', p_colocation)
       END;
$$;

-- What a distributed table has of its own that the partitioned table it would be attached to does not have, one text per object: an
-- index, a CHECK constraint (by name and definition), a default, storage parameters, another owner, privileges.  The leaves of a user
-- partition are made again by a bucket move from the definition of the partitioned table, so these would be lost (P9C-03).
CREATE FUNCTION kingres.internal_attach_differences(p_parent regclass, p_part regclass)
    RETURNS SETOF text
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    pc pg_catalog.pg_class%ROWTYPE;
    tc pg_catalog.pg_class%ROWTYPE;
    r  pg_catalog.record;
BEGIN
    SELECT * INTO pc FROM pg_catalog.pg_class k WHERE k.oid OPERATOR(pg_catalog.=) p_parent::pg_catalog.oid;
    SELECT * INTO tc FROM pg_catalog.pg_class k WHERE k.oid OPERATOR(pg_catalog.=) p_part::pg_catalog.oid;
    IF tc.relowner OPERATOR(pg_catalog.<>) pc.relowner THEN
        RETURN NEXT pg_catalog.format('the owner is "%s", not "%s"', pg_catalog.pg_get_userbyid(tc.relowner), pg_catalog.pg_get_userbyid(pc.relowner));
    END IF;
    IF tc.relacl IS NOT NULL AND tc.relacl::pg_catalog.text[] IS DISTINCT FROM pg_catalog.acldefault('r', tc.relowner)::pg_catalog.text[] THEN
        RETURN NEXT 'privileges of its own';
    END IF;
    FOR r IN SELECT ic.relname AS name
               FROM pg_catalog.pg_index x JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) x.indexrelid
              WHERE x.indrelid OPERATOR(pg_catalog.=) tc.oid
                AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_index y
                                 WHERE y.indrelid OPERATOR(pg_catalog.=) pc.oid AND y.indisunique OPERATOR(pg_catalog.=) x.indisunique AND y.indisprimary OPERATOR(pg_catalog.=) x.indisprimary
                                   AND pg_catalog.regexp_replace(pg_catalog.pg_get_indexdef(y.indexrelid), '^.*? USING ', '')
                                       OPERATOR(pg_catalog.=) pg_catalog.regexp_replace(pg_catalog.pg_get_indexdef(x.indexrelid), '^.*? USING ', ''))
              ORDER BY ic.relname
    LOOP
        RETURN NEXT pg_catalog.format('the index "%s"', r.name);
    END LOOP;
    FOR r IN SELECT k.conname AS name
               FROM pg_catalog.pg_constraint k
              WHERE k.conrelid OPERATOR(pg_catalog.=) tc.oid AND k.contype OPERATOR(pg_catalog.=) 'c'
                AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint j
                                 WHERE j.conrelid OPERATOR(pg_catalog.=) pc.oid AND j.contype OPERATOR(pg_catalog.=) 'c' AND j.conname OPERATOR(pg_catalog.=) k.conname
                                   AND pg_catalog.pg_get_constraintdef(j.oid) OPERATOR(pg_catalog.=) pg_catalog.pg_get_constraintdef(k.oid))
              ORDER BY k.conname
    LOOP
        RETURN NEXT pg_catalog.format('the check constraint "%s"', r.name);
    END LOOP;
    FOR r IN SELECT a.attname AS name
               FROM pg_catalog.pg_attrdef d JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) d.adrelid AND a.attnum OPERATOR(pg_catalog.=) d.adnum
               JOIN pg_catalog.pg_attribute pa ON pa.attrelid OPERATOR(pg_catalog.=) pc.oid AND pa.attname OPERATOR(pg_catalog.=) a.attname AND NOT pa.attisdropped
               LEFT JOIN pg_catalog.pg_attrdef pd ON pd.adrelid OPERATOR(pg_catalog.=) pa.attrelid AND pd.adnum OPERATOR(pg_catalog.=) pa.attnum
              WHERE d.adrelid OPERATOR(pg_catalog.=) tc.oid
                AND pg_catalog.pg_get_expr(d.adbin, d.adrelid) IS DISTINCT FROM pg_catalog.pg_get_expr(pd.adbin, pd.adrelid)
              ORDER BY a.attname
    LOOP
        RETURN NEXT pg_catalog.format('a default of its own on column "%s"', r.name);
    END LOOP;
    IF kingres.internal_reloptions_of(p_part) IS DISTINCT FROM kingres.internal_reloptions_of(p_parent) THEN
        RETURN NEXT 'storage parameters of its own';
    END IF;
END;
$$;

-- The privileges and the comment of a table go to another table (a hash parent made like it).  Table privileges only; those of columns
-- are not carried.
CREATE FUNCTION kingres.internal_copy_acl_comment(p_from regclass, p_to regclass)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_acl pg_catalog.aclitem[];
    v_cm  pg_catalog.text;
    a     pg_catalog.record;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT c.relacl INTO v_acl FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) p_from::pg_catalog.oid;
    -- nothing is done for a table with the default ACL that the new one has as well; default privileges of the schema are taken back
    IF v_acl IS NOT NULL OR (SELECT c.relacl IS NOT NULL FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) p_to::pg_catalog.oid) THEN
        PERFORM kingres.internal_reset_acl(p_to, v_acl IS NULL);
    END IF;
    IF v_acl IS NOT NULL THEN
        FOR a IN SELECT CASE WHEN e.grantee OPERATOR(pg_catalog.=) 0 THEN 'PUBLIC' ELSE pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(e.grantee)) END AS grantee,
                        e.privilege_type AS priv, e.is_grantable AS gr
                   FROM pg_catalog.aclexplode(v_acl) e ORDER BY e.grantee, e.privilege_type
        LOOP
            EXECUTE pg_catalog.format('GRANT %s ON TABLE %s TO %s%s', a.priv, p_to, a.grantee, CASE WHEN a.gr THEN ' WITH GRANT OPTION' ELSE '' END);
        END LOOP;
    END IF;
    v_cm := pg_catalog.obj_description(p_from::pg_catalog.oid, 'pg_class');
    IF v_cm IS NOT NULL THEN
        EXECUTE pg_catalog.format('COMMENT ON TABLE %s IS %L', p_to, v_cm);
    END IF;
END;
$$;

REVOKE ALL ON FUNCTION
    kingres.internal_require_read_committed(text),
    kingres.internal_group_busy(int),
    kingres.internal_attach_differences(regclass, regclass),
    kingres.internal_copy_acl_comment(regclass, regclass)
    FROM PUBLIC;
