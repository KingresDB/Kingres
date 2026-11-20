-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_shard--0.7--0.8.sql
--   Upgrade of the kingres_shard extension from version 0.7 to 0.8.  Version 0.8 is written by two phases
--   at the same time, and each has its own clearly marked section in this file and in kingres_shard--0.8.sql:
--     * phase 10 (elasticity): its section is marked "PHASE 10" (not here yet at the time of writing);
--     * phase 11 (distributed schemas, design/shard-phase11.md): the section marked "PHASE 11" below.
--   Keep the sections separate: the integrator joins them, and the "upgrade" regression test compares the result
--   with a fresh install of 0.8.
--
\echo Use "ALTER EXTENSION kingres_shard UPDATE TO '0.8'" to load this file. \quit

-- =========================================================================
-- PHASE 11 (distributed schemas), increment 1: catalog, views, inventory   [begin]
--
-- This section belongs to phase 11 (design/shard-phase11.md, sections 5.2 to
-- 5.4 and 10).  It is the same text in kingres_shard--0.7--0.8.sql and at the
-- end of kingres_shard--0.8.sql.  Nothing in it creates a schema group by
-- itself: the functions that do (distribute_schema, the hook for CREATE
-- SCHEMA) are later increments.
-- =========================================================================

-- A colocation group is of kind 'hash' (the buckets of hash-distributed tables)
-- or 'schema' (one bucket: the tables of one distributed schema, all in one shard group).
ALTER TABLE kingres.colocation
    ADD COLUMN kind text NOT NULL DEFAULT 'hash',
    ADD CONSTRAINT colocation_kind_check CHECK (kind IN ('hash', 'schema')),
    ALTER COLUMN dist_column_type DROP NOT NULL,
    ADD CONSTRAINT colocation_kind_shape_check CHECK (
        (kind = 'hash' AND dist_column_type IS NOT NULL)
        OR (kind = 'schema' AND dist_column_type IS NULL AND bucket_count = 1));

COMMENT ON COLUMN kingres.colocation.kind IS
    '''hash'': the buckets of hash-distributed tables; ''schema'': one bucket, the tables of one distributed schema';

-- method 'schema': a table of a distributed schema (no distribution column; the colocation group is the schema's).
ALTER TABLE kingres.dist_table
    DROP CONSTRAINT dist_table_method_check,
    DROP CONSTRAINT dist_table_shape_check,
    ADD CONSTRAINT dist_table_method_check CHECK (method IN ('hash', 'reference', 'schema')),
    ADD CONSTRAINT dist_table_shape_check CHECK (
        (method = 'hash' AND dist_column IS NOT NULL AND colocation_id IS NOT NULL)
        OR (method = 'reference' AND dist_column IS NULL AND colocation_id IS NULL)
        OR (method = 'schema' AND dist_column IS NULL AND colocation_id IS NOT NULL));

-- One row per distributed schema.  nsp is a node-local OID (like dist_table.relid); the catalog snapshot carries the name.
CREATE TABLE kingres.dist_schema (
    nsp             regnamespace NOT NULL,
    colocation_id   int          NOT NULL,
    created_at      timestamptz  NOT NULL DEFAULT pg_catalog.now(),
    ship_functions  bool         NOT NULL DEFAULT false,
    CONSTRAINT dist_schema_pkey PRIMARY KEY (nsp),
    CONSTRAINT dist_schema_colocation_id_key UNIQUE (colocation_id),
    CONSTRAINT dist_schema_colocation_id_fkey
        FOREIGN KEY (colocation_id) REFERENCES kingres.colocation
);

COMMENT ON TABLE kingres.dist_schema IS
    'The distributed schemas: one row each, with the colocation group (kind schema) that holds its one bucket';

-- One row: how this database shards (design 10).  'row': hash-distributed tables only (every database before 0.8);
-- 'schema': distributed schemas (and reference tables); 'both'.
CREATE TABLE kingres.sharding_settings (
    mode                   text NOT NULL DEFAULT 'row',
    distribute_new_schemas bool NOT NULL DEFAULT false,
    CONSTRAINT sharding_settings_mode_check CHECK (mode IN ('row', 'schema', 'both')),
    CONSTRAINT sharding_settings_default_check CHECK (distribute_new_schemas = false OR mode IN ('schema', 'both'))
);
CREATE UNIQUE INDEX sharding_settings_single_row_idx ON kingres.sharding_settings ((true));
INSERT INTO kingres.sharding_settings DEFAULT VALUES;

COMMENT ON TABLE kingres.sharding_settings IS
    'One row: the sharding mode of the database (row, schema or both) and whether new schemas are distributed by default';

-- Every change of the new tables counts as a change of the map.
CREATE TRIGGER dist_schema_map_version
    AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON kingres.dist_schema
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.map_version_bump();
CREATE TRIGGER sharding_settings_map_version
    AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON kingres.sharding_settings
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.map_version_bump();

-- ---------------------------------------------------------------------------
-- The version handshake asks for 0.8.  This is the version bump of 0.8, the same change as phase 10 makes: when the two
-- sections are joined, one definition of this function is enough.
-- ---------------------------------------------------------------------------
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
        IF v IS DISTINCT FROM '0.8' THEN
            RAISE EXCEPTION 'node % (%:%) has kingres_shard %, a bucket move needs 0.8 on every node',
                n.node_id, n.host, n.port, COALESCE(NULLIF(v, ''), '(not installed)')
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Run ALTER EXTENSION kingres_shard UPDATE on that node, after the new library is loaded there.';
        END IF;
    END LOOP;
END;
$$;

-- ---------------------------------------------------------------------------
-- The catalog snapshot carries the new tables
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
                              FROM kingres.bucket_map b), '[]'::pg_catalog.jsonb),
    'dist_schema', COALESCE((SELECT jsonb_agg(jsonb_build_object(
                                       'schema', ns.nspname,
                                       'colocation_id', s.colocation_id,
                                       'created_at', s.created_at,
                                       'ship_functions', s.ship_functions)
                                     ORDER BY ns.nspname)
                              FROM kingres.dist_schema s
                              JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) s.nsp::pg_catalog.oid),
                           '[]'::pg_catalog.jsonb),
    'sharding_settings', COALESCE((SELECT jsonb_agg(to_jsonb(g)) FROM kingres.sharding_settings g),
                                  '[]'::pg_catalog.jsonb));
$$;

-- The same function as before, with the two tables.  A document without them comes from a map
-- group primary that is not upgraded: the settings then stay as they are and there is no
-- distributed schema.
CREATE OR REPLACE FUNCTION kingres.internal_apply_catalog(snapshot pg_catalog.jsonb)
    RETURNS pg_catalog.void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r        pg_catalog.record;
    rid      pg_catalog.regclass;
    rns      pg_catalog.regnamespace;
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

    DELETE FROM kingres.dist_schema;
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

    -- the distributed schemas, by name: the schema must exist on this node (a structure that was not made here is an error, not a gap)
    FOR r IN SELECT * FROM pg_catalog.jsonb_to_recordset(
                              COALESCE(snapshot OPERATOR(pg_catalog.->) 'dist_schema', '[]'::pg_catalog.jsonb))
                  AS x(schema pg_catalog.text, colocation_id pg_catalog.int4, created_at pg_catalog.timestamptz,
                       ship_functions pg_catalog.bool)
    LOOP
        rns := pg_catalog.to_regnamespace(pg_catalog.quote_ident(r.schema));
        IF rns IS NULL THEN
            RAISE EXCEPTION 'distributed schema % does not exist on this node', r.schema
                USING ERRCODE = 'invalid_schema_name';
        END IF;
        INSERT INTO kingres.dist_schema (nsp, colocation_id, created_at, ship_functions)
        VALUES (rns, r.colocation_id, COALESCE(r.created_at, pg_catalog.now()), COALESCE(r.ship_functions, false));
    END LOOP;

    IF snapshot OPERATOR(pg_catalog.?) 'sharding_settings'
       AND pg_catalog.jsonb_array_length(snapshot OPERATOR(pg_catalog.->) 'sharding_settings') OPERATOR(pg_catalog.>) 0 THEN
        DELETE FROM kingres.sharding_settings;
        INSERT INTO kingres.sharding_settings
            SELECT * FROM pg_catalog.jsonb_populate_recordset(NULL::kingres.sharding_settings,
                                        snapshot OPERATOR(pg_catalog.->) 'sharding_settings');
    END IF;

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

-- The drop of a registered schema takes its rows with it (the event trigger forget_dropped_tables already
-- takes the rows of the dropped tables).
CREATE OR REPLACE FUNCTION kingres.forget_dropped_tables()
    RETURNS event_trigger
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    dropped    pg_catalog.oid[];
    dropped_ns pg_catalog.oid[];
    v_coloc    pg_catalog.int4[];
    hit        pg_catalog.regclass;
BEGIN
    SELECT pg_catalog.array_agg(o.objid) INTO dropped
      FROM pg_catalog.pg_event_trigger_dropped_objects() o
     WHERE o.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass;
    SELECT pg_catalog.array_agg(o.objid) INTO dropped_ns
      FROM pg_catalog.pg_event_trigger_dropped_objects() o
     WHERE o.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_namespace'::pg_catalog.regclass;
    IF dropped IS NULL AND dropped_ns IS NULL THEN
        RETURN;
    END IF;

    -- A drop that reaches a distributed table (also by dropping one of its
    -- columns through a cascade) would act on this node only.  The DROP TABLE
    -- statements that the extension propagates to every node are authorized
    -- by the ProcessUtility hook (kingres.internal_ddl_authorized()); a
    -- superuser who repeats a statement on every node by hand switches the
    -- check off with kingres_shard.enable_routing (a superuser-only setting).
    IF dropped IS NOT NULL
       AND pg_catalog.current_setting('kingres_shard.enable_routing', true) IS DISTINCT FROM 'off'
       AND NOT kingres.internal_ddl_authorized()
       AND (SELECT pg_catalog.count(DISTINCT n.group_id)
              FROM kingres.node n
             WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active) OPERATOR(pg_catalog.>) 1 THEN
        SELECT d.relid INTO hit
          FROM kingres.dist_table d
         WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) ANY (dropped)
         LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'DROP is not supported on distributed tables yet'
                USING ERRCODE = 'feature_not_supported',
                      DETAIL = pg_catalog.format('The statement would drop distributed table %s, or one of its columns, on this node only.', hit),
                      HINT = 'Drop it on every node with kingres_shard.enable_routing = off, as a superuser, if you can keep the nodes consistent yourself.';
        END IF;
    END IF;

    -- Only the table itself (objsubid 0), not one of its columns, is gone; and
    -- only a statement that deletes nothing leaves the version alone.
    IF EXISTS (SELECT 1
                 FROM kingres.dist_table d
                WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) ANY
                      (SELECT o.objid
                         FROM pg_catalog.pg_event_trigger_dropped_objects() o
                        WHERE o.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
                          AND o.objsubid OPERATOR(pg_catalog.=) 0)) THEN
        DELETE FROM kingres.dist_table d
         WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) ANY
               (SELECT o.objid
                  FROM pg_catalog.pg_event_trigger_dropped_objects() o
                 WHERE o.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
                   AND o.objsubid OPERATOR(pg_catalog.=) 0);
    END IF;

    -- A dropped schema that was distributed: its row, its colocation group and its bucket go.
    IF dropped_ns IS NOT NULL THEN
        SELECT pg_catalog.array_agg(s.colocation_id) INTO v_coloc
          FROM kingres.dist_schema s
         WHERE s.nsp::pg_catalog.oid OPERATOR(pg_catalog.=) ANY (dropped_ns);
        IF v_coloc IS NOT NULL THEN
            DELETE FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) ANY (v_coloc);
            DELETE FROM kingres.dist_schema s WHERE s.colocation_id OPERATOR(pg_catalog.=) ANY (v_coloc);
            DELETE FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) ANY (v_coloc);
            DELETE FROM kingres.colocation c
             WHERE c.colocation_id OPERATOR(pg_catalog.=) ANY (v_coloc) AND c.kind OPERATOR(pg_catalog.=) 'schema';
        END IF;
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- The delta functions (design 5.3): node-local changes of the catalog rows of a schema.  They run on every
-- node in the same two-phase transaction as the DDL; every statement counts as a change of the map.
-- ---------------------------------------------------------------------------

-- The schema p_nsp (which must exist on this node) becomes a distributed schema of the group p_group,
-- with the colocation group p_colocation (kind schema, one bucket).
CREATE FUNCTION kingres.internal_register_schema(p_nsp name, p_colocation int, p_group int)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_nsp pg_catalog.regnamespace := pg_catalog.to_regnamespace(pg_catalog.quote_ident(p_nsp));
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF v_nsp IS NULL THEN
        RAISE EXCEPTION 'schema "%" does not exist', p_nsp USING ERRCODE = 'invalid_schema_name';
    END IF;
    IF p_group OPERATOR(pg_catalog.<) 0 THEN
        RAISE EXCEPTION 'shard group % is not valid', p_group USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF EXISTS (SELECT 1 FROM kingres.dist_schema s WHERE s.nsp OPERATOR(pg_catalog.=) v_nsp) THEN
        RAISE EXCEPTION 'schema "%" is already distributed', p_nsp USING ERRCODE = 'duplicate_schema';
    END IF;
    INSERT INTO kingres.colocation (colocation_id, bucket_count, dist_column_type, dist_collation, kind)
    VALUES (p_colocation, 1, NULL, 0, 'schema');
    INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id, state)
    VALUES (p_colocation, 0, p_group, 'active');
    INSERT INTO kingres.dist_schema (nsp, colocation_id) VALUES (v_nsp, p_colocation);
END;
$$;

-- A table (or partition) of a distributed schema becomes a schema table.  The group is the one of the table's schema.
CREATE FUNCTION kingres.internal_register_schema_table(p_relid regclass)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_kind  pg_catalog."char";
    v_nsp   pg_catalog.oid;
    v_coloc pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT c.relkind, c.relnamespace INTO v_kind, v_nsp FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid;
    IF v_kind IS NULL OR v_kind NOT IN ('r', 'p') THEN
        RAISE EXCEPTION '"%" is not an ordinary or partitioned table', p_relid USING ERRCODE = 'wrong_object_type';
    END IF;
    SELECT s.colocation_id INTO v_coloc FROM kingres.dist_schema s WHERE s.nsp::pg_catalog.oid OPERATOR(pg_catalog.=) v_nsp;
    IF v_coloc IS NULL THEN
        RAISE EXCEPTION 'the schema of table "%" is not distributed', p_relid USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    INSERT INTO kingres.dist_table (relid, method, dist_column, colocation_id)
    VALUES (p_relid, 'schema', NULL, v_coloc);
END;
$$;

CREATE FUNCTION kingres.internal_unregister_schema_table(p_relid regclass)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    PERFORM kingres.internal_require_superuser();
    DELETE FROM kingres.dist_table d
     WHERE d.relid OPERATOR(pg_catalog.=) p_relid AND d.method OPERATOR(pg_catalog.=) 'schema';
END;
$$;

-- The schema is local again: its rows, its tables' rows, its bucket and its colocation group go.
CREATE FUNCTION kingres.internal_unregister_schema(p_nsp name)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_nsp   pg_catalog.regnamespace := pg_catalog.to_regnamespace(pg_catalog.quote_ident(p_nsp));
    v_coloc pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT s.colocation_id INTO v_coloc FROM kingres.dist_schema s WHERE s.nsp OPERATOR(pg_catalog.=) v_nsp;
    IF v_coloc IS NULL THEN
        RAISE EXCEPTION 'schema "%" is not distributed', p_nsp USING ERRCODE = 'undefined_object';
    END IF;
    DELETE FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) v_coloc AND d.method OPERATOR(pg_catalog.=) 'schema';
    DELETE FROM kingres.dist_schema s WHERE s.colocation_id OPERATOR(pg_catalog.=) v_coloc;
    DELETE FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_coloc;
    DELETE FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_coloc AND c.kind OPERATOR(pg_catalog.=) 'schema';
END;
$$;

-- The group that holds a schema, and its state (used by a move).
CREATE FUNCTION kingres.internal_set_schema_group(p_colocation int, p_group int, p_state text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF NOT EXISTS (SELECT 1 FROM kingres.colocation c
                    WHERE c.colocation_id OPERATOR(pg_catalog.=) p_colocation AND c.kind OPERATOR(pg_catalog.=) 'schema') THEN
        RAISE EXCEPTION 'colocation group % is not the group of a distributed schema', p_colocation
            USING ERRCODE = 'undefined_object';
    END IF;
    UPDATE kingres.bucket_map m
       SET group_id = p_group, state = p_state
     WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation AND m.bucket OPERATOR(pg_catalog.=) 0;
END;
$$;

COMMENT ON FUNCTION kingres.internal_register_schema(name, int, int) IS
    'Node-local: the schema becomes a distributed schema of the group (colocation group of kind schema, its bucket, its row)';
COMMENT ON FUNCTION kingres.internal_register_schema_table(regclass) IS
    'Node-local: a table of a distributed schema becomes a schema table';
COMMENT ON FUNCTION kingres.internal_unregister_schema_table(regclass) IS
    'Node-local: the table is no longer a schema table';
COMMENT ON FUNCTION kingres.internal_unregister_schema(name) IS
    'Node-local: the schema is local again (its rows, tables, bucket and colocation group are forgotten)';
COMMENT ON FUNCTION kingres.internal_set_schema_group(int, int, text) IS
    'Node-local: the group and state of the one bucket of a schema group';

-- ---------------------------------------------------------------------------
-- Views
-- ---------------------------------------------------------------------------

-- One row per distributed schema.  size_bytes is known where the schema's tables are held: on the node of the owner group;
-- elsewhere it is null (the tables there are empty shells).
CREATE VIEW kingres.schemas AS
    SELECT ns.nspname                                   AS schema_name,
           pg_catalog.pg_get_userbyid(ns.nspowner)      AS owner,
           s.colocation_id,
           b.group_id,
           n.node_id,
           b.state,
           (SELECT pg_catalog.count(*) FROM kingres.dist_table d
             WHERE d.colocation_id OPERATOR(pg_catalog.=) s.colocation_id)::pg_catalog.int4 AS table_count,
           CASE WHEN EXISTS (SELECT 1 FROM kingres.local_node l
                               JOIN kingres.node ln ON ln.node_id OPERATOR(pg_catalog.=) l.node_id
                              WHERE ln.group_id OPERATOR(pg_catalog.=) b.group_id AND ln.role OPERATOR(pg_catalog.=) 'primary')
                THEN (SELECT pg_catalog.sum(pg_catalog.pg_total_relation_size(d.relid))::pg_catalog.int8
                        FROM kingres.dist_table d
                       WHERE d.colocation_id OPERATOR(pg_catalog.=) s.colocation_id)
           END                                          AS size_bytes,
           (SELECT mv.target_group FROM kingres.bucket_move mv
             WHERE mv.colocation_id OPERATOR(pg_catalog.=) s.colocation_id
               AND mv.phase IN ('preparing', 'copying', 'catching_up', 'switching')
             ORDER BY mv.move_id DESC LIMIT 1)          AS moving_to_group_id
      FROM kingres.dist_schema s
      JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) s.nsp::pg_catalog.oid
      JOIN kingres.bucket_map b ON b.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND b.bucket OPERATOR(pg_catalog.=) 0
      LEFT JOIN kingres.node n
             ON n.group_id OPERATOR(pg_catalog.=) b.group_id AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active;

COMMENT ON VIEW kingres.schemas IS
    'The distributed schemas: owner group and node, state, number of tables, size (on the owner node only) and the target of a running move';

-- A schema table shows bucket 0 and no partition: it is an ordinary table, not a bucket partition.
CREATE OR REPLACE VIEW kingres.buckets AS
    SELECT d.relid AS table_name,
           b.bucket,
           CASE WHEN d.method OPERATOR(pg_catalog.<>) 'schema' THEN p.partition_name END AS partition_name,
           b.group_id,
           n.node_id,
           b.state,
           CASE WHEN lp.part IS NOT NULL AND d.method OPERATOR(pg_catalog.<>) 'schema'
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
-- schema_inventory (design 5.4, 7.12, 8.1): every object of the schema, and whether the schema can be distributed with it
-- ---------------------------------------------------------------------------

CREATE FUNCTION kingres.schema_inventory(nsp regnamespace)
    RETURNS TABLE (object_kind text, object_name text, supported bool, reason text)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_oid    pg_catalog.oid := nsp::pg_catalog.oid;
    v_name   pg_catalog.name;
    v_ledger pg_catalog.oid[] := '{}';
    v_hist   pg_catalog.oid[];
BEGIN
    SELECT n.nspname INTO v_name FROM pg_catalog.pg_namespace n WHERE n.oid OPERATOR(pg_catalog.=) v_oid;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'schema with OID % does not exist', v_oid USING ERRCODE = 'invalid_schema_name';
    END IF;

    -- the tables of kingres_ledger (and their history tables), when that extension is installed
    IF pg_catalog.to_regclass('kingres.ledger_table') IS NOT NULL THEN
        EXECUTE 'SELECT COALESCE(pg_catalog.array_agg(x), ''{}'') FROM (SELECT t.relid::pg_catalog.oid AS x FROM kingres.ledger_table t) s'
           INTO v_ledger;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
                    WHERE a.attrelid OPERATOR(pg_catalog.=) 'kingres.ledger_table'::pg_catalog.regclass
                      AND a.attname OPERATOR(pg_catalog.=) 'history_relid' AND NOT a.attisdropped) THEN
            EXECUTE 'SELECT COALESCE(pg_catalog.array_agg(t.history_relid::pg_catalog.oid), ''{}'') FROM kingres.ledger_table t WHERE t.history_relid IS NOT NULL'
               INTO v_hist;
            v_ledger := v_ledger OPERATOR(pg_catalog.||) v_hist;
        END IF;
    END IF;

    RETURN QUERY
    SELECT x.k, x.n, x.s, CASE WHEN x.s THEN NULL ELSE x.r END
      FROM (
    -- the schema itself
    SELECT 'schema'::pg_catalog.text, pg_catalog.quote_ident(v_name)::pg_catalog.text, false,
           'a system schema'::pg_catalog.text
     WHERE v_name OPERATOR(pg_catalog.=) ANY (ARRAY['pg_catalog', 'information_schema', 'kingres', 'pg_toast']::pg_catalog.name[])
        OR v_name OPERATOR(pg_catalog.~~) 'pg\_%'
    UNION ALL
    SELECT 'schema', pg_catalog.quote_ident(v_name), false, 'the schema is already distributed'
     WHERE EXISTS (SELECT 1 FROM kingres.dist_schema s WHERE s.nsp::pg_catalog.oid OPERATOR(pg_catalog.=) v_oid)
    UNION ALL
    -- tables and partitioned tables
    SELECT CASE c.relkind WHEN 'p' THEN 'partitioned table' ELSE 'table' END,
           pg_catalog.quote_ident(v_name) || '.' || pg_catalog.quote_ident(c.relname),
           (c.relpersistence OPERATOR(pg_catalog.=) 'p' AND dt.method IS NULL AND NOT c.oid OPERATOR(pg_catalog.=) ANY (v_ledger)
            AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i
                              JOIN pg_catalog.pg_class pc ON pc.oid OPERATOR(pg_catalog.=) i.inhparent
                             WHERE i.inhrelid OPERATOR(pg_catalog.=) c.oid AND pc.relnamespace OPERATOR(pg_catalog.<>) v_oid)
            AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i
                              JOIN pg_catalog.pg_class cc ON cc.oid OPERATOR(pg_catalog.=) i.inhrelid
                             WHERE i.inhparent OPERATOR(pg_catalog.=) c.oid AND cc.relnamespace OPERATOR(pg_catalog.<>) v_oid)),
           CASE WHEN c.relpersistence OPERATOR(pg_catalog.=) 't' THEN 'a temporary table'
                WHEN c.relpersistence OPERATOR(pg_catalog.=) 'u' THEN 'an unlogged table (it is not replicated and cannot be moved)'
                WHEN dt.method OPERATOR(pg_catalog.=) 'hash' THEN 'a hash-distributed table'
                WHEN dt.method OPERATOR(pg_catalog.=) 'reference' THEN 'a reference table'
                WHEN dt.method OPERATOR(pg_catalog.=) 'schema' THEN 'already a table of a distributed schema'
                WHEN c.oid OPERATOR(pg_catalog.=) ANY (v_ledger) THEN 'a kingres_ledger table'
                WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i
                              JOIN pg_catalog.pg_class pc ON pc.oid OPERATOR(pg_catalog.=) i.inhparent
                             WHERE i.inhrelid OPERATOR(pg_catalog.=) c.oid AND pc.relnamespace OPERATOR(pg_catalog.<>) v_oid)
                     THEN 'it inherits from, or is a partition of, a table of another schema'
                WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i
                              JOIN pg_catalog.pg_class cc ON cc.oid OPERATOR(pg_catalog.=) i.inhrelid
                             WHERE i.inhparent OPERATOR(pg_catalog.=) c.oid AND cc.relnamespace OPERATOR(pg_catalog.<>) v_oid)
                     THEN 'a table of another schema inherits from it or is its partition' END
      FROM pg_catalog.pg_class c
      LEFT JOIN kingres.dist_table dt ON dt.relid OPERATOR(pg_catalog.=) c.oid
     WHERE c.relnamespace OPERATOR(pg_catalog.=) v_oid AND c.relkind IN ('r', 'p')
    UNION ALL
    -- indexes, sequences
    SELECT CASE c.relkind WHEN 'S' THEN 'sequence' ELSE 'index' END,
           pg_catalog.quote_ident(v_name) || '.' || pg_catalog.quote_ident(c.relname), true, NULL
      FROM pg_catalog.pg_class c
     WHERE c.relnamespace OPERATOR(pg_catalog.=) v_oid AND c.relkind IN ('i', 'I', 'S')
    UNION ALL
    -- views: supported unless they read a table or view outside the schema that is not a reference table
    SELECT 'view', pg_catalog.quote_ident(v_name) || '.' || pg_catalog.quote_ident(c.relname),
           NOT EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite rw
                         JOIN pg_catalog.pg_depend dp ON dp.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass
                                                     AND dp.objid OPERATOR(pg_catalog.=) rw.oid
                                                     AND dp.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
                         JOIN pg_catalog.pg_class rc ON rc.oid OPERATOR(pg_catalog.=) dp.refobjid
                         JOIN pg_catalog.pg_namespace rn ON rn.oid OPERATOR(pg_catalog.=) rc.relnamespace
                        WHERE rw.ev_class OPERATOR(pg_catalog.=) c.oid AND rc.oid OPERATOR(pg_catalog.<>) c.oid
                          AND rc.relnamespace OPERATOR(pg_catalog.<>) v_oid
                          AND rn.nspname NOT IN ('pg_catalog', 'information_schema')
                          AND rc.relkind IN ('r', 'p', 'v', 'm', 'f')
                          AND NOT EXISTS (SELECT 1 FROM kingres.dist_table d
                                           WHERE d.relid OPERATOR(pg_catalog.=) rc.oid AND d.method OPERATOR(pg_catalog.=) 'reference')),
           'it reads a table or view of another schema that is not a reference table'
      FROM pg_catalog.pg_class c
     WHERE c.relnamespace OPERATOR(pg_catalog.=) v_oid AND c.relkind OPERATOR(pg_catalog.=) 'v'
    UNION ALL
    -- what is not supported at all (7.12)
    SELECT CASE c.relkind WHEN 'm' THEN 'materialized view' ELSE 'foreign table' END,
           pg_catalog.quote_ident(v_name) || '.' || pg_catalog.quote_ident(c.relname), false,
           CASE c.relkind WHEN 'm' THEN 'a materialized view is not supported in a distributed schema yet'
                          ELSE 'a foreign table is not supported in a distributed schema yet' END
      FROM pg_catalog.pg_class c
     WHERE c.relnamespace OPERATOR(pg_catalog.=) v_oid AND c.relkind IN ('m', 'f')
    UNION ALL
    -- functions and procedures; aggregates and window functions are not produced by the structure generator
    SELECT CASE p.prokind WHEN 'p' THEN 'procedure' WHEN 'a' THEN 'aggregate' WHEN 'w' THEN 'window function' ELSE 'function' END,
           pg_catalog.quote_ident(v_name) || '.' || pg_catalog.quote_ident(p.proname) || '(' || pg_catalog.pg_get_function_identity_arguments(p.oid) || ')',
           p.prokind IN ('f', 'p'),
           CASE WHEN p.prokind IN ('f', 'p') THEN NULL
                ELSE 'an aggregate or window function is not supported in a distributed schema yet' END
      FROM pg_catalog.pg_proc p
     WHERE p.pronamespace OPERATOR(pg_catalog.=) v_oid
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend dp
                        WHERE dp.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_proc'::pg_catalog.regclass
                          AND dp.objid OPERATOR(pg_catalog.=) p.oid AND dp.deptype IN ('e', 'i'))
    UNION ALL
    -- types (a table's row type and the array types are not listed: they follow the table)
    SELECT CASE t.typtype WHEN 'e' THEN 'enum type' WHEN 'd' THEN 'domain' WHEN 'c' THEN 'composite type'
                          WHEN 'r' THEN 'range type' WHEN 'm' THEN 'multirange type' ELSE 'type' END,
           pg_catalog.quote_ident(v_name) || '.' || pg_catalog.quote_ident(t.typname),
           t.typtype IN ('e', 'd', 'c'),
           CASE WHEN t.typtype IN ('e', 'd', 'c') THEN NULL
                ELSE 'a range, multirange or base type is not supported in a distributed schema yet' END
      FROM pg_catalog.pg_type t
     WHERE t.typnamespace OPERATOR(pg_catalog.=) v_oid
       AND NOT (t.typcategory OPERATOR(pg_catalog.=) 'A' AND t.typelem OPERATOR(pg_catalog.<>) 0)
       AND (t.typtype <> 'c' OR EXISTS (SELECT 1 FROM pg_catalog.pg_class rc WHERE rc.oid OPERATOR(pg_catalog.=) t.typrelid AND rc.relkind OPERATOR(pg_catalog.=) 'c'))
       AND t.typtype OPERATOR(pg_catalog.<>) 'm'
    UNION ALL
    -- triggers (not the internal ones that foreign keys make)
    SELECT 'trigger', pg_catalog.quote_ident(v_name) || '.' || pg_catalog.quote_ident(c.relname) || '.' || pg_catalog.quote_ident(g.tgname), true, NULL
      FROM pg_catalog.pg_trigger g JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) g.tgrelid
     WHERE c.relnamespace OPERATOR(pg_catalog.=) v_oid AND NOT g.tgisinternal
    UNION ALL
    -- rules, policies, row-level security, extended statistics, publications
    SELECT 'rule', pg_catalog.quote_ident(v_name) || '.' || pg_catalog.quote_ident(c.relname) || '.' || pg_catalog.quote_ident(rw.rulename), false,
           'a rule is not supported in a distributed schema yet'
      FROM pg_catalog.pg_rewrite rw JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) rw.ev_class
     WHERE c.relnamespace OPERATOR(pg_catalog.=) v_oid AND rw.rulename OPERATOR(pg_catalog.<>) '_RETURN'
    UNION ALL
    SELECT 'row-level security', pg_catalog.quote_ident(v_name) || '.' || pg_catalog.quote_ident(c.relname), false,
           'row-level security and policies are not supported in a distributed schema yet'
      FROM pg_catalog.pg_class c
     WHERE c.relnamespace OPERATOR(pg_catalog.=) v_oid AND c.relkind IN ('r', 'p')
       AND (c.relrowsecurity OR EXISTS (SELECT 1 FROM pg_catalog.pg_policy po WHERE po.polrelid OPERATOR(pg_catalog.=) c.oid))
    UNION ALL
    SELECT 'extended statistics', pg_catalog.quote_ident(v_name) || '.' || pg_catalog.quote_ident(se.stxname), false,
           'extended statistics are not supported in a distributed schema yet'
      FROM pg_catalog.pg_statistic_ext se WHERE se.stxnamespace OPERATOR(pg_catalog.=) v_oid
    UNION ALL
    SELECT 'publication', pg_catalog.quote_ident(pb.pubname), false,
           'a publication that includes the schema or its tables is not supported in a distributed schema yet'
      FROM pg_catalog.pg_publication pb
     WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_publication_namespace pn
                    WHERE pn.pnpubid OPERATOR(pg_catalog.=) pb.oid AND pn.pnnspid OPERATOR(pg_catalog.=) v_oid)
        OR EXISTS (SELECT 1 FROM pg_catalog.pg_publication_rel pr JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) pr.prrelid
                    WHERE pr.prpubid OPERATOR(pg_catalog.=) pb.oid AND c.relnamespace OPERATOR(pg_catalog.=) v_oid)
    UNION ALL
    -- foreign keys that cross the border of the schema; a reference table is the one allowed target
    SELECT 'foreign key', pg_catalog.quote_ident(cn.nspname) || '.' || pg_catalog.quote_ident(c.relname) || '.' || pg_catalog.quote_ident(k.conname),
           (k.confrelid OPERATOR(pg_catalog.=) 0
            OR (rc.relnamespace OPERATOR(pg_catalog.=) v_oid AND c.relnamespace OPERATOR(pg_catalog.=) v_oid)
            OR (c.relnamespace OPERATOR(pg_catalog.=) v_oid AND EXISTS (SELECT 1 FROM kingres.dist_table d
                                                                        WHERE d.relid OPERATOR(pg_catalog.=) k.confrelid
                                                                          AND d.method OPERATOR(pg_catalog.=) 'reference'))),
           CASE WHEN c.relnamespace OPERATOR(pg_catalog.<>) v_oid THEN 'a foreign key from a table of another schema to a table of this schema'
                ELSE 'a foreign key to a table of another schema that is not a reference table' END
      FROM pg_catalog.pg_constraint k
      JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) k.conrelid
      JOIN pg_catalog.pg_namespace cn ON cn.oid OPERATOR(pg_catalog.=) c.relnamespace
      JOIN pg_catalog.pg_class rc ON rc.oid OPERATOR(pg_catalog.=) k.confrelid
     WHERE k.contype OPERATOR(pg_catalog.=) 'f'
       AND (c.relnamespace OPERATOR(pg_catalog.=) v_oid OR rc.relnamespace OPERATOR(pg_catalog.=) v_oid)
       AND (c.relnamespace OPERATOR(pg_catalog.<>) rc.relnamespace)
      ) x (k, n, s, r)
    ORDER BY 1, 2;
END;
$$;

COMMENT ON FUNCTION kingres.schema_inventory(regnamespace) IS
    'Every object of the schema with whether the schema can be distributed with it, and why not (reads the catalog only)';

-- ---------------------------------------------------------------------------
-- set_sharding_mode (design 10): the sharding mode of the database and the default for new schemas
-- ---------------------------------------------------------------------------

CREATE FUNCTION kingres.set_sharding_mode(mode text, distribute_new_schemas bool DEFAULT NULL)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_mode  pg_catalog.text := mode;
    v_dist  pg_catalog.bool := distribute_new_schemas;
    v_cur   pg_catalog.text;
    v_curd  pg_catalog.bool;
BEGIN
    PERFORM kingres.internal_require_superuser();
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    IF v_mode IS NULL OR v_mode NOT IN ('row', 'schema', 'both') THEN
        RAISE EXCEPTION 'invalid sharding mode %: it must be row, schema or both', pg_catalog.quote_nullable(v_mode)
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT g.mode, g.distribute_new_schemas INTO v_cur, v_curd FROM kingres.sharding_settings g;
    IF v_dist IS NULL THEN
        v_dist := v_curd AND v_mode IN ('schema', 'both');
    END IF;
    IF v_dist AND v_mode = 'row' THEN
        RAISE EXCEPTION 'new schemas cannot be distributed by default in mode "row"'
            USING ERRCODE = 'invalid_parameter_value',
                  HINT = 'Choose the mode schema or both first.';
    END IF;
    -- what is in the way of the change
    IF v_mode = 'row' AND v_cur <> 'row'
       AND EXISTS (SELECT 1 FROM kingres.dist_schema) THEN
        RAISE EXCEPTION 'cannot change the sharding mode to "row": distributed schemas exist'
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  DETAIL = (SELECT pg_catalog.string_agg(s.schema_name, ', ' ORDER BY s.schema_name) FROM kingres.schemas s),
                  HINT = 'Make the schemas local first (undistribute_schema).';
    END IF;
    IF v_mode = 'schema' AND v_cur <> 'schema'
       AND EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.method OPERATOR(pg_catalog.=) 'hash') THEN
        RAISE EXCEPTION 'cannot change the sharding mode to "schema": hash-distributed tables exist'
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  DETAIL = (SELECT pg_catalog.string_agg(d.relid::pg_catalog.text, ', ' ORDER BY d.relid::pg_catalog.text)
                              FROM kingres.dist_table d WHERE d.method OPERATOR(pg_catalog.=) 'hash'),
                  HINT = 'Make the tables local first (undistribute_table).';
    END IF;
    UPDATE kingres.sharding_settings g SET mode = v_mode, distribute_new_schemas = v_dist;
    PERFORM kingres.sync_catalog();
END;
$$;

COMMENT ON FUNCTION kingres.set_sharding_mode(text, bool) IS
    'Set the sharding mode of the database (row, schema or both) and whether new schemas are distributed by default; map group primary only; atomic';

-- ---------------------------------------------------------------------------
-- Privileges of this section
-- ---------------------------------------------------------------------------

REVOKE ALL ON FUNCTION
    kingres.internal_register_schema(name, int, int),
    kingres.internal_register_schema_table(regclass),
    kingres.internal_unregister_schema_table(regclass),
    kingres.internal_unregister_schema(name),
    kingres.internal_set_schema_group(int, int, text),
    kingres.set_sharding_mode(text, bool)
    FROM PUBLIC;
GRANT SELECT ON kingres.dist_schema, kingres.sharding_settings, kingres.schemas TO PUBLIC;

-- =========================================================================
-- PHASE 11, increment 1                                                      [end]
-- =========================================================================

-- =========================================================================
-- PHASE 11 (distributed schemas), increment 3: a distributed schema and its DDL   [begin]
--
-- distribute_schema() of an empty schema, the choice of the group, the registration of the tables that a
-- propagated DDL statement creates, and the structure fingerprint (design/shard-phase11.md 5.3, 7.1, 8.1, 9.1).
-- The statements themselves are propagated by the ProcessUtility hook (src/utility.c, src/schema_ddl.c).
-- =========================================================================

-- The group that receives the next distributed schema (design 9.1): the shard group whose primary is active and should
-- have shards, with the fewest distributed schemas; a tie goes to the lowest group id.  (The weight of a group belongs to
-- phase 10 and is not in the catalog of 0.8 yet: the count is not divided by it.)
CREATE FUNCTION kingres.internal_choose_schema_group()
    RETURNS int
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT g.group_id
  FROM (SELECT DISTINCT n.group_id
          FROM kingres.node n
         WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND n.should_have_shards) g
 ORDER BY (SELECT pg_catalog.count(*)
             FROM kingres.dist_schema s
             JOIN kingres.bucket_map b ON b.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND b.bucket OPERATOR(pg_catalog.=) 0
            WHERE b.group_id OPERATOR(pg_catalog.=) g.group_id),
          g.group_id
 LIMIT 1;
$$;

-- Node-local: every ordinary and partitioned table of the schema that is not yet a schema table becomes one.  The hook runs
-- it on every node after a propagated statement that may have created a table (the C function that calls it has checked
-- that the session is a superuser or a session of another node).  Returns the number of tables registered.
CREATE FUNCTION kingres.internal_register_schema_tables(p_nsp name)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r pg_catalog.record;
    n pg_catalog.int4 := 0;
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF NOT EXISTS (SELECT 1 FROM kingres.dist_schema s WHERE s.nsp OPERATOR(pg_catalog.=) p_nsp::pg_catalog.regnamespace) THEN
        RAISE EXCEPTION 'schema "%" is not a distributed schema on this node', p_nsp
            USING ERRCODE = 'undefined_object';
    END IF;
    FOR r IN SELECT c.oid::pg_catalog.regclass AS rel
               FROM pg_catalog.pg_class c
              WHERE c.relnamespace OPERATOR(pg_catalog.=) p_nsp::pg_catalog.regnamespace
                AND c.relkind IN ('r', 'p')
                AND c.relpersistence OPERATOR(pg_catalog.<>) 't'
                AND NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) c.oid)
              ORDER BY c.oid
    LOOP
        PERFORM kingres.internal_register_schema_table(r.rel);
        n := n + 1;
    END LOOP;
    RETURN n;
END;
$$;

-- The structure of a table as one text (and its md5, the fingerprint): columns (name, type, modifier, collation, not null, default, identity,
-- generated), constraints, indexes and storage options, each sorted.  The same table has the same fingerprint on every
-- node; the names of schemas are in it, the OIDs are not.  The search path is fixed so that the definitions print the same.
-- The hook asks every node for it in the session of the user whose statement it is, so the function is executable by
-- PUBLIC; it reads what pg_catalog shows anyway.
CREATE FUNCTION kingres.internal_table_structure(p_rel regclass)
    RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
SELECT pg_catalog.format('%s|%s|%s|%s|%s',
           (SELECT c.relkind::pg_catalog.text || c.relpersistence::pg_catalog.text || COALESCE(c.reloptions::pg_catalog.text, '') FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) p_rel::pg_catalog.oid),
           COALESCE((SELECT pg_catalog.string_agg(pg_catalog.format('%s:%s:%s:%s:%s:%s:%s',
                                a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod),
                                COALESCE((SELECT co.collname FROM pg_catalog.pg_collation co WHERE co.oid OPERATOR(pg_catalog.=) a.attcollation), ''),
                                a.attnotnull, COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid), ''), a.attidentity, a.attgenerated),
                                ',' ORDER BY a.attnum)
                       FROM pg_catalog.pg_attribute a
                       LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid OPERATOR(pg_catalog.=) a.attrelid AND d.adnum OPERATOR(pg_catalog.=) a.attnum
                      WHERE a.attrelid OPERATOR(pg_catalog.=) p_rel::pg_catalog.oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped), ''),
           COALESCE((SELECT pg_catalog.string_agg(k.conname || ' ' || pg_catalog.pg_get_constraintdef(k.oid), ',' ORDER BY k.conname)
                       FROM pg_catalog.pg_constraint k WHERE k.conrelid OPERATOR(pg_catalog.=) p_rel::pg_catalog.oid), ''),
           COALESCE((SELECT pg_catalog.string_agg(pg_catalog.pg_get_indexdef(i.indexrelid), ',' ORDER BY pg_catalog.pg_get_indexdef(i.indexrelid))
                       FROM pg_catalog.pg_index i WHERE i.indrelid OPERATOR(pg_catalog.=) p_rel::pg_catalog.oid), ''),
           COALESCE((SELECT pg_catalog.string_agg(t.tgname, ',' ORDER BY t.tgname)
                       FROM pg_catalog.pg_trigger t WHERE t.tgrelid OPERATOR(pg_catalog.=) p_rel::pg_catalog.oid AND NOT t.tgisinternal), ''));
$$;

CREATE FUNCTION kingres.internal_table_fingerprint(p_rel regclass)
    RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
SELECT pg_catalog.md5(kingres.internal_table_structure(p_rel));
$$;

GRANT EXECUTE ON FUNCTION kingres.internal_table_structure(regclass), kingres.internal_table_fingerprint(regclass) TO PUBLIC;

-- distribute_schema(nsp, group): the schema becomes a distributed schema of a group.  Increment 3 takes the empty schema:
-- it exists on this node, holds no object, and is created on every other node in the same two-phase transaction.  A schema
-- with objects is the conversion of design 8.1 (a later increment).
CREATE FUNCTION kingres.distribute_schema(nsp regnamespace, group_id int DEFAULT NULL)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_variable
DECLARE
    v_name   pg_catalog.name;
    v_owner  pg_catalog.name;
    v_mode   pg_catalog.text;
    v_group  pg_catalog.int4 := group_id;
    v_coloc  pg_catalog.int4;
    v_self   pg_catalog.int4;
    v_max    pg_catalog.int4;
    v_what   pg_catalog.text;
    nd       pg_catalog.record;
BEGIN
    PERFORM kingres.internal_require_superuser();
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    IF pg_catalog.current_setting('transaction_isolation') OPERATOR(pg_catalog.<>) 'read committed' THEN
        RAISE EXCEPTION 'distribute_schema must run in a READ COMMITTED transaction'
            USING ERRCODE = 'invalid_transaction_state';
    END IF;
    SELECT s.mode INTO v_mode FROM kingres.sharding_settings s;
    IF v_mode OPERATOR(pg_catalog.=) 'row' THEN
        RAISE EXCEPTION 'distribute_schema is not available in the sharding mode "row"'
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'Run kingres.set_sharding_mode(''both'') or kingres.set_sharding_mode(''schema'') first.';
    END IF;
    PERFORM kingres.internal_check_node_versions();
    SELECT ns.nspname, pg_catalog.pg_get_userbyid(ns.nspowner) INTO v_name, v_owner
      FROM pg_catalog.pg_namespace ns WHERE ns.oid OPERATOR(pg_catalog.=) nsp::pg_catalog.oid;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'schema % does not exist', nsp::pg_catalog.oid
            USING ERRCODE = 'invalid_schema_name';
    END IF;
    IF v_name OPERATOR(pg_catalog.~~) 'pg\_%' OR v_name IN ('information_schema', 'kingres', 'public')
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_depend dp
                   WHERE dp.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_namespace'::pg_catalog.regclass
                     AND dp.objid OPERATOR(pg_catalog.=) nsp::pg_catalog.oid AND dp.deptype OPERATOR(pg_catalog.=) 'e') THEN
        RAISE EXCEPTION 'schema "%" cannot be distributed: it is a system schema, the schema public, or the schema of an extension', v_name
            USING ERRCODE = 'feature_not_supported';
    END IF;
    IF EXISTS (SELECT 1 FROM kingres.dist_schema s WHERE s.nsp OPERATOR(pg_catalog.=) nsp) THEN
        RAISE EXCEPTION 'schema "%" is already distributed', v_name
            USING ERRCODE = 'duplicate_object';
    END IF;
    -- the schema holds nothing: tables, indexes, sequences and views are in pg_class, functions in pg_proc, types in pg_type
    SELECT x.what INTO v_what
      FROM (SELECT 'relation ' || c.relname AS what FROM pg_catalog.pg_class c WHERE c.relnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
            UNION ALL SELECT 'function ' || p.proname FROM pg_catalog.pg_proc p WHERE p.pronamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
            UNION ALL SELECT 'type ' || t.typname FROM pg_catalog.pg_type t WHERE t.typnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
            UNION ALL SELECT 'operator ' || o.oprname FROM pg_catalog.pg_operator o WHERE o.oprnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
            UNION ALL SELECT 'collation ' || co.collname FROM pg_catalog.pg_collation co WHERE co.collnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid) x
     ORDER BY x.what LIMIT 1;
    IF v_what IS NOT NULL THEN
        RAISE EXCEPTION 'schema "%" holds objects: distributing a schema that is not empty is not supported yet', v_name
            USING ERRCODE = 'feature_not_supported',
                  DETAIL = pg_catalog.format('The first object is %s.', v_what),
                  HINT = 'Distribute the schema while it is empty, then create its objects.';
    END IF;
    v_max := pg_catalog.current_setting('kingres_shard.max_distributed_schemas')::pg_catalog.int4;
    IF (SELECT pg_catalog.count(*) FROM kingres.dist_schema) OPERATOR(pg_catalog.>=) v_max THEN
        RAISE EXCEPTION 'the database already has % distributed schemas, the limit of kingres_shard.max_distributed_schemas', v_max
            USING ERRCODE = 'program_limit_exceeded';
    END IF;
    IF v_group IS NULL THEN
        v_group := NULLIF(pg_catalog.current_setting('kingres_shard.schema_placement_group')::pg_catalog.int4, -1);
    END IF;
    IF v_group IS NULL THEN
        v_group := kingres.internal_choose_schema_group();
    ELSIF NOT EXISTS (SELECT 1 FROM kingres.node n
                       WHERE n.group_id OPERATOR(pg_catalog.=) v_group AND n.role OPERATOR(pg_catalog.=) 'primary'
                         AND n.is_active AND n.should_have_shards) THEN
        RAISE EXCEPTION 'shard group % cannot hold a schema: it has no active primary that should have shards', v_group
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF v_group IS NULL THEN
        RAISE EXCEPTION 'no shard group can hold a schema'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_coloc := pg_catalog.nextval('kingres.colocation_id_seq');
    PERFORM kingres.internal_register_schema(v_name, v_coloc, v_group);
    FOR nd IN SELECT n.node_id FROM kingres.node n
               WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND n.node_id OPERATOR(pg_catalog.<>) v_self
               ORDER BY n.node_id
    LOOP
        PERFORM kingres.internal_branch_exec(nd.node_id, pg_catalog.format('CREATE SCHEMA %I AUTHORIZATION %I', v_name, v_owner));
        PERFORM kingres.internal_branch_exec(nd.node_id, pg_catalog.format('SELECT kingres.internal_register_schema(%L, %s, %s)', v_name, v_coloc, v_group));
    END LOOP;
    RETURN v_coloc;
END;
$$;

COMMENT ON FUNCTION kingres.distribute_schema(regnamespace, int) IS
    'Make an empty schema a distributed schema of a shard group (map group primary only); returns its colocation id';

-- What the origin of a propagated statement asks every other node to do after the statement, in the same transaction:
-- 'register_schema' (the schema that a CREATE SCHEMA made, in a database that distributes new schemas) and 'register_tables'
-- (the tables the statement made).  The session that calls it runs as the role of the user whose statement it is, so the
-- function checks its caller itself (a superuser, or a session that another node opened) and is not revoked from PUBLIC.
CREATE FUNCTION kingres.internal_schema_delta(op text, nsp name, colocation_id int DEFAULT NULL, group_id int DEFAULT NULL)
    RETURNS int
    AS 'MODULE_PATHNAME', 'kingres_internal_schema_delta'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

-- A flag of a distributed schema (design 5.4): ship_functions lets path S run a statement that calls functions of the
-- schema (or any function that is not built in) whole on the owner, which has them all.  The catalog goes to every node.
CREATE FUNCTION kingres.alter_distributed_schema(nsp regnamespace, ship_functions bool DEFAULT NULL)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_variable
BEGIN
    PERFORM kingres.internal_require_superuser();
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    IF NOT EXISTS (SELECT 1 FROM kingres.dist_schema s WHERE s.nsp OPERATOR(pg_catalog.=) nsp) THEN
        RAISE EXCEPTION 'schema % is not a distributed schema', nsp::pg_catalog.oid
            USING ERRCODE = 'undefined_object';
    END IF;
    IF ship_functions IS NOT NULL THEN
        UPDATE kingres.dist_schema s SET ship_functions = alter_distributed_schema.ship_functions WHERE s.nsp OPERATOR(pg_catalog.=) nsp;
    END IF;
    PERFORM kingres.sync_catalog();
END;
$$;

COMMENT ON FUNCTION kingres.alter_distributed_schema(regnamespace, bool) IS
    'Change a flag of a distributed schema (ship_functions); map group primary only';

REVOKE ALL ON FUNCTION
    kingres.internal_choose_schema_group(),
    kingres.internal_register_schema_tables(name),
    kingres.distribute_schema(regnamespace, int),
    kingres.alter_distributed_schema(regnamespace, bool)
    FROM PUBLIC;

-- =========================================================================
-- PHASE 11, increment 3                                                      [end]
-- =========================================================================
