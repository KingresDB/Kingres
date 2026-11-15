-- SPDX-License-Identifier: PostgreSQL
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_shard--0.3.sql
--   Install script of the kingres_shard extension, version 0.3: the cluster
--   catalog, the functions that maintain it, the introspection views, and the
--   movement of buckets between shard groups (kingres.move_bucket, the
--   rebalance planner and the cluster check).
--   Installing this script and upgrading with kingres_shard--0.1--0.2.sql and
--   kingres_shard--0.2--0.3.sql must give identical catalogs (the "upgrade"
--   regression test compares them).  This file is the 0.2 install script
--   followed by the body of the 0.2--0.3 upgrade script: change both together.
--   Design: kingres/docs/design/shard-mvp.md, shard-phase2a.md, shard-phase4.md.
\echo Use "CREATE EXTENSION kingres_shard" to load this file. \quit

-- The schema is created here, not by CREATE EXTENSION, so that it is a member
-- of the extension and disappears with it.  Every object is qualified.
CREATE SCHEMA kingres;

-- ---------------------------------------------------------------------------
-- Catalog tables
-- ---------------------------------------------------------------------------

-- One row per PostgreSQL instance that takes part in the cluster.
CREATE TABLE kingres.node (
    node_id            int  NOT NULL,
    group_id           int  NOT NULL,
    host               text NOT NULL,
    port               int  NOT NULL,
    role               text NOT NULL,
    is_active          bool NOT NULL DEFAULT true,
    should_have_shards bool NOT NULL DEFAULT true,
    server_group       text,
    CONSTRAINT node_pkey PRIMARY KEY (node_id),
    CONSTRAINT node_host_port_key UNIQUE (host, port),
    CONSTRAINT node_role_check CHECK (role IN ('primary', 'standby')),
    CONSTRAINT node_group_id_check CHECK (group_id >= 0),
    CONSTRAINT node_port_check CHECK (port BETWEEN 1 AND 65535)
);
-- Two primaries in one shard group are impossible (review finding 4).  The
-- lock that the catalog-changing functions take serializes them; this index
-- is what makes a missed lock an error instead of a corrupt catalog.
CREATE UNIQUE INDEX node_primary_per_group_idx
    ON kingres.node (group_id) WHERE role = 'primary';

-- Tables with the same colocation_id have the same bucket count, the same
-- distribution column type and the same bucket placement.
CREATE TABLE kingres.colocation (
    colocation_id    int    NOT NULL,
    bucket_count     int    NOT NULL,
    dist_column_type regtype NOT NULL,
    dist_collation   oid    NOT NULL DEFAULT 0,
    CONSTRAINT colocation_pkey PRIMARY KEY (colocation_id),
    CONSTRAINT colocation_bucket_count_check
        CHECK (bucket_count BETWEEN 1 AND 4096)
);

-- method 'hash': a distributed table (dist_column and colocation_id are set);
-- method 'reference': a table with a full copy on every node (neither is set).
CREATE TABLE kingres.dist_table (
    relid         regclass NOT NULL,
    method        text     NOT NULL,
    dist_column   name,
    colocation_id int,
    CONSTRAINT dist_table_pkey PRIMARY KEY (relid),
    CONSTRAINT dist_table_method_check CHECK (method IN ('hash', 'reference')),
    CONSTRAINT dist_table_shape_check CHECK (
        (method = 'hash' AND dist_column IS NOT NULL AND colocation_id IS NOT NULL)
        OR (method = 'reference' AND dist_column IS NULL AND colocation_id IS NULL)),
    CONSTRAINT dist_table_colocation_id_fkey
        FOREIGN KEY (colocation_id) REFERENCES kingres.colocation
);

CREATE TABLE kingres.bucket_map (
    colocation_id int  NOT NULL,
    bucket        int  NOT NULL,
    group_id      int  NOT NULL,
    state         text NOT NULL DEFAULT 'active',
    CONSTRAINT bucket_map_pkey PRIMARY KEY (colocation_id, bucket),
    CONSTRAINT bucket_map_colocation_id_fkey
        FOREIGN KEY (colocation_id) REFERENCES kingres.colocation,
    CONSTRAINT bucket_map_bucket_check CHECK (bucket >= 0),
    CONSTRAINT bucket_map_group_id_check CHECK (group_id >= 0),
    CONSTRAINT bucket_map_state_check
        CHECK (state IN ('active', 'moving', 'orphan'))
);

-- Single-row table: the version of the whole map.  Every statement that
-- changes node, colocation, dist_table or bucket_map increments it.
CREATE TABLE kingres.map_version (
    version bigint NOT NULL
);
CREATE UNIQUE INDEX map_version_single_row_idx ON kingres.map_version ((true));
INSERT INTO kingres.map_version VALUES (0);

-- Single-row, node-local table (never synchronized): which node this is.
CREATE TABLE kingres.local_node (
    node_id int NOT NULL
);
CREATE UNIQUE INDEX local_node_single_row_idx ON kingres.local_node ((true));

-- Node-local, never synchronized: the commit decisions of this node as the
-- origin of two-phase transactions.  A row is written by the committing local
-- transaction itself, so it is durable exactly when the commit is; its
-- presence means "commit".  Recovery removes it when the prepared transaction
-- is gone from its target node.
CREATE TABLE kingres.dtx_log (
    gid         text        NOT NULL,
    node_id     int         NOT NULL,
    prepared_at timestamptz NOT NULL,
    CONSTRAINT dtx_log_pkey PRIMARY KEY (gid)
);
CREATE INDEX dtx_log_node_idx ON kingres.dtx_log (node_id);

COMMENT ON TABLE kingres.dtx_log IS
    'Node-local: commit decisions of two-phase transactions that originated here; never synchronized';
-- Node-local, never synchronized: nodes that add_node() is about to prepare a
-- transaction on, recorded (and committed) before the prepare, so that recovery
-- can find a prepared transaction on a node that the aborted add_node() never
-- registered.  Rows are removed by recovery.
CREATE TABLE kingres.pending_node (
    host       text        NOT NULL,
    port       int         NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT pending_node_pkey PRIMARY KEY (host, port)
);

COMMENT ON TABLE kingres.pending_node IS
    'Node-local: addresses add_node() prepared a transaction on before registering them; never synchronized';

COMMENT ON TABLE kingres.node IS
    'Every PostgreSQL instance of the cluster, with its shard group and role';
COMMENT ON TABLE kingres.colocation IS
    'Colocation groups: bucket count and distribution column type shared by their tables';
COMMENT ON TABLE kingres.dist_table IS
    'Distributed tables; relid is a node-local OID, synchronization transfers names';
COMMENT ON TABLE kingres.bucket_map IS
    'Owner shard group of every bucket of every colocation group';
COMMENT ON TABLE kingres.map_version IS
    'Version of the whole map; incremented by statement triggers';
COMMENT ON TABLE kingres.local_node IS
    'Node-local: the node_id of this instance; never synchronized';

-- ---------------------------------------------------------------------------
-- map_version maintenance
-- ---------------------------------------------------------------------------

CREATE FUNCTION kingres.map_version_bump()
    RETURNS trigger
    AS 'MODULE_PATHNAME', 'kingres_map_version_bump'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.map_version_bump() IS
    'Statement trigger: increments map_version and invalidates the backend caches';

CREATE TRIGGER node_map_version
    AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON kingres.node
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.map_version_bump();
CREATE TRIGGER colocation_map_version
    AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON kingres.colocation
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.map_version_bump();
CREATE TRIGGER dist_table_map_version
    AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON kingres.dist_table
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.map_version_bump();
CREATE TRIGGER bucket_map_map_version
    AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON kingres.bucket_map
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.map_version_bump();

-- Forget a distributed table when it is dropped, so that the catalog never
-- holds an OID that no longer exists.  DDL is not propagated in phase 1; see
-- the README.  On a cluster with several shard groups a drop that reaches a
-- distributed table is refused instead (the ProcessUtility hook refuses the
-- direct ones; this trigger is the backstop for drops that cascade, such as
-- DROP SCHEMA ... CASCADE or DROP TYPE ... CASCADE).
--
-- This is the only SECURITY DEFINER function of the extension, and it has to
-- be: whoever may drop a table (its owner) has no privilege on the catalog
-- tables of the extension, but the catalog row must go with the table.  It
-- is safe because
--   * search_path ends with pg_temp, and every type, cast and operator it
--     uses is qualified with pg_catalog, so nothing a user created in a
--     schema of their own (or in their temporary schema) is ever resolved
--     (review S-A);
--   * it takes no argument, and it only deletes the rows of tables that
--     the DROP statement itself reports as dropped.
CREATE FUNCTION kingres.forget_dropped_tables()
    RETURNS event_trigger
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    dropped pg_catalog.oid[];
    hit     pg_catalog.regclass;
BEGIN
    SELECT pg_catalog.array_agg(o.objid) INTO dropped
      FROM pg_catalog.pg_event_trigger_dropped_objects() o
     WHERE o.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass;
    IF dropped IS NULL THEN
        RETURN;
    END IF;

    -- A drop that reaches a distributed table (also by dropping one of its
    -- columns through a cascade) would act on this node only.  The DROP TABLE
    -- statements that the extension propagates to every node are authorized
    -- by the ProcessUtility hook (kingres.internal_ddl_authorized()); a
    -- superuser who repeats a statement on every node by hand switches the
    -- check off with kingres_shard.enable_routing (a superuser-only setting).
    IF pg_catalog.current_setting('kingres_shard.enable_routing', true) IS DISTINCT FROM 'off'
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

    -- Only the table itself (objsubid 0), not one of its columns, is gone.
    DELETE FROM kingres.dist_table d
     WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) ANY
           (SELECT o.objid
              FROM pg_catalog.pg_event_trigger_dropped_objects() o
             WHERE o.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
               AND o.objsubid OPERATOR(pg_catalog.=) 0);
END;
$$;

CREATE EVENT TRIGGER kingres_forget_dropped_tables
    ON sql_drop
    EXECUTE FUNCTION kingres.forget_dropped_tables();

-- ---------------------------------------------------------------------------
-- Hashing
-- ---------------------------------------------------------------------------

CREATE FUNCTION kingres.version()
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_version'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.version() IS 'The KingresDB product version';

CREATE FUNCTION kingres.bucket_for(relation regclass, value anyelement)
    RETURNS int
    AS 'MODULE_PATHNAME', 'kingres_bucket_for_relation'
    LANGUAGE C STABLE
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.bucket_for(regclass, anyelement) IS
    'Bucket of a distribution key value of a distributed table';

CREATE FUNCTION kingres.bucket_for(colocation_id int, value anyelement)
    RETURNS int
    AS 'MODULE_PATHNAME', 'kingres_bucket_for_colocation'
    LANGUAGE C STABLE
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.bucket_for(int, anyelement) IS
    'Bucket of a distribution key value in a colocation group';

-- ---------------------------------------------------------------------------
-- Views
-- ---------------------------------------------------------------------------

CREATE VIEW kingres.nodes AS
    SELECT n.node_id, n.group_id, n.host, n.port, n.role, n.is_active,
           n.should_have_shards, n.server_group,
           (l.node_id IS NOT NULL) AS is_local
      FROM kingres.node n
      LEFT JOIN kingres.local_node l ON l.node_id = n.node_id;

COMMENT ON VIEW kingres.nodes IS
    'kingres.node plus whether the row is the node this server is';

CREATE VIEW kingres.buckets AS
    SELECT d.relid AS table_name,
           b.bucket,
           p.partition_name,
           b.group_id,
           n.node_id,
           b.state,
           CASE WHEN lp.part IS NOT NULL
                THEN pg_catalog.pg_total_relation_size(lp.part)
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

COMMENT ON VIEW kingres.buckets IS
    'Every bucket of every distributed table with its owner; partition_size is set only where the partition exists on this node';

-- ---------------------------------------------------------------------------
-- Catalog snapshot and replacement (used by synchronization)
-- ---------------------------------------------------------------------------

-- The whole catalog as one jsonb document.  Distributed tables are written by
-- schema-qualified name because OIDs differ between nodes.
CREATE FUNCTION kingres.internal_catalog_snapshot()
    RETURNS pg_catalog.jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT jsonb_build_object(
    'version', (SELECT m.version FROM kingres.map_version m),
    'node', COALESCE((SELECT jsonb_agg(to_jsonb(n) ORDER BY n.node_id)
                        FROM kingres.node n), '[]'::pg_catalog.jsonb),
    'colocation', COALESCE((SELECT jsonb_agg(to_jsonb(c) ORDER BY c.colocation_id)
                              FROM kingres.colocation c), '[]'::pg_catalog.jsonb),
    'dist_table', COALESCE((SELECT jsonb_agg(jsonb_build_object(
                                       'schema', ns.nspname,
                                       'name', cl.relname,
                                       'method', d.method,
                                       'dist_column', d.dist_column,
                                       'colocation_id', d.colocation_id)
                                     ORDER BY d.colocation_id, ns.nspname, cl.relname)
                              FROM kingres.dist_table d
                              JOIN pg_catalog.pg_class cl ON cl.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid
                              JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) cl.relnamespace),
                           '[]'::pg_catalog.jsonb),
    'bucket_map', COALESCE((SELECT jsonb_agg(to_jsonb(b) ORDER BY b.colocation_id, b.bucket)
                              FROM kingres.bucket_map b), '[]'::pg_catalog.jsonb));
$$;

-- Replace the synchronized part of the catalog by a snapshot.  The version is
-- set last, from the snapshot, because the statement triggers increment it
-- while the tables are rewritten.  local_node is never touched.
--
-- This function is called by the catalog reload of a node that found its
-- bucket map stale, and that reload runs as the bootstrap superuser inside the
-- session of an ordinary user (the catalog tables belong to the extension
-- owner).  So nothing here may resolve a name through the search_path or the
-- temporary schema of that session (review S-B): the function pins its own
-- search_path (pg_temp last), and every type, cast, function and operator that
-- a user could shadow is written with its pg_catalog (or kingres) schema.  The
-- C caller invokes it by OID, so no name is looked up in the session either.
CREATE FUNCTION kingres.internal_apply_catalog(snapshot pg_catalog.jsonb)
    RETURNS pg_catalog.void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r   pg_catalog.record;
    rid pg_catalog.regclass;
BEGIN
    DELETE FROM kingres.bucket_map;
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
                       dist_column pg_catalog.name, colocation_id pg_catalog.int4)
    LOOP
        rid := pg_catalog.to_regclass(pg_catalog.format('%I.%I', r.schema, r.name));
        IF rid IS NULL THEN
            RAISE EXCEPTION 'distributed table %.% does not exist on this node', r.schema, r.name
                USING ERRCODE = 'undefined_table';
        END IF;
        INSERT INTO kingres.dist_table VALUES (rid, r.method, r.dist_column, r.colocation_id);
    END LOOP;

    INSERT INTO kingres.bucket_map
        SELECT * FROM pg_catalog.jsonb_populate_recordset(NULL::kingres.bucket_map,
                                    snapshot OPERATOR(pg_catalog.->) 'bucket_map');

    UPDATE kingres.map_version
       SET version = (snapshot OPERATOR(pg_catalog.->>) 'version')::pg_catalog.int8;
END;
$$;

CREATE FUNCTION kingres.internal_set_local_node(p_node_id pg_catalog.int4)
    RETURNS pg_catalog.void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DELETE FROM kingres.local_node;
INSERT INTO kingres.local_node VALUES (p_node_id);
$$;

-- Forget the cluster on a node that was removed from it.
CREATE FUNCTION kingres.internal_reset_node()
    RETURNS void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DELETE FROM kingres.local_node;
DELETE FROM kingres.bucket_map;
DELETE FROM kingres.dist_table;
DELETE FROM kingres.colocation;
DELETE FROM kingres.cluster_secret;
DELETE FROM kingres.node;
UPDATE kingres.map_version SET version = 0;
$$;

-- ---------------------------------------------------------------------------
-- Node registry
-- ---------------------------------------------------------------------------

-- Register the local node as node 1 of group 0, the map group.
CREATE FUNCTION kingres.init_cluster(host text, port int)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    -- The cluster lock of every catalog-changing function (review finding 4): the
    -- same relation and mode as kingres_shard_lock_catalog() takes in C.
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM kingres.local_node)
       OR EXISTS (SELECT 1 FROM kingres.node) THEN
        RAISE EXCEPTION 'the cluster is already initialized on this node'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    INSERT INTO kingres.node (node_id, group_id, host, port, role)
        VALUES (1, 0, host, port, 'primary');
    INSERT INTO kingres.local_node VALUES (1);
    PERFORM kingres.internal_ensure_secret();
END;
$$;

COMMENT ON FUNCTION kingres.init_cluster(text, int) IS
    'Register this node as node 1 of the map group (group 0)';

CREATE FUNCTION kingres.add_node(host text, port int,
                                  group_id int DEFAULT NULL,
                                  role text DEFAULT 'primary')
    RETURNS int
    AS 'MODULE_PATHNAME', 'kingres_add_node'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.add_node(text, int, int, text) IS
    'Register a node (map group primary only) and copy the catalog to it';

CREATE FUNCTION kingres.remove_node(node_id int)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_remove_node'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.remove_node(int) IS
    'Unregister a node whose shard group owns no buckets';

CREATE FUNCTION kingres.sync_catalog()
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_sync_catalog'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.sync_catalog() IS
    'Push the catalog of the map group primary to every active node, atomically (two-phase commit)';

-- ---------------------------------------------------------------------------
-- Distributed tables
-- ---------------------------------------------------------------------------

-- Check that a table can become distributed by the given column; return the
-- type and collation of the column.  Errors name the offending object.
CREATE FUNCTION kingres.internal_check_distributable(p_relid regclass, p_column name)
    RETURNS TABLE (column_type pg_catalog.oid, column_collation pg_catalog.oid)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    c        pg_catalog.pg_class%ROWTYPE;
    a        pg_catalog.pg_attribute%ROWTYPE;
    idx      pg_catalog.record;
    other    pg_catalog.text;
    nonempty pg_catalog.bool;
BEGIN
    SELECT * INTO c FROM pg_class WHERE oid = p_relid;
    IF EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid = p_relid) THEN
        RAISE EXCEPTION 'table "%" is already distributed', c.relname
            USING ERRCODE = 'duplicate_object';
    END IF;
    IF c.relkind = 'p' OR c.relispartition THEN
        RAISE EXCEPTION 'cannot distribute table "%": partitioned tables and partitions are not supported yet', c.relname
            USING ERRCODE = 'feature_not_supported';
    ELSIF c.relkind <> 'r' THEN
        RAISE EXCEPTION 'cannot distribute "%": it is not an ordinary table', c.relname
            USING ERRCODE = 'wrong_object_type';
    END IF;
    IF c.relpersistence <> 'p' THEN
        RAISE EXCEPTION 'cannot distribute table "%": temporary and unlogged tables are not supported', c.relname
            USING ERRCODE = 'feature_not_supported';
    END IF;

    SELECT * INTO a FROM pg_attribute
     WHERE attrelid = p_relid AND attname = p_column
       AND attnum > 0 AND NOT attisdropped;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'column "%" of table "%" does not exist', p_column, c.relname
            USING ERRCODE = 'undefined_column';
    END IF;

    IF EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = p_relid OR inhparent = p_relid) THEN
        RAISE EXCEPTION 'cannot distribute table "%": inheritance is not supported', c.relname
            USING ERRCODE = 'feature_not_supported';
    END IF;
    IF c.reloptions IS NOT NULL THEN
        RAISE EXCEPTION 'cannot distribute table "%": storage parameters are not supported yet', c.relname
            USING ERRCODE = 'feature_not_supported';
    END IF;
    IF c.relrowsecurity OR EXISTS (SELECT 1 FROM pg_policy WHERE polrelid = p_relid) THEN
        RAISE EXCEPTION 'cannot distribute table "%": row-level security is not supported yet', c.relname
            USING ERRCODE = 'feature_not_supported';
    END IF;

    SELECT k.conname INTO other FROM pg_constraint k
     WHERE k.contype = 'f' AND (k.conrelid = p_relid OR k.confrelid = p_relid)
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'cannot distribute table "%": foreign key constraint "%" is not supported yet', c.relname, other
            USING ERRCODE = 'feature_not_supported';
    END IF;
    SELECT k.conname INTO other FROM pg_constraint k
     WHERE k.contype = 'x' AND k.conrelid = p_relid
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'cannot distribute table "%": exclusion constraint "%" is not supported', c.relname, other
            USING ERRCODE = 'feature_not_supported';
    END IF;
    SELECT t.tgname INTO other FROM pg_trigger t
     WHERE t.tgrelid = p_relid AND NOT t.tgisinternal
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'cannot distribute table "%": trigger "%" is not supported yet', c.relname, other
            USING ERRCODE = 'feature_not_supported';
    END IF;

    FOR idx IN
        SELECT ic.relname AS index_name, i.indkey::pg_catalog.int2[] AS cols, k.contype
          FROM pg_index i
          JOIN pg_class ic ON ic.oid = i.indexrelid
          LEFT JOIN pg_constraint k
                 ON k.conindid = i.indexrelid AND k.conrelid = p_relid
                AND k.contype IN ('p', 'u')
         WHERE i.indrelid = p_relid AND i.indisunique
         ORDER BY ic.relname
    LOOP
        IF NOT (a.attnum = ANY (idx.cols)) THEN
            RAISE EXCEPTION 'cannot distribute table "%": % "%" does not include the distribution column "%"',
                c.relname,
                CASE idx.contype WHEN 'p' THEN 'primary key constraint'
                                 WHEN 'u' THEN 'unique constraint'
                                 ELSE 'unique index' END,
                idx.index_name, p_column
                USING ERRCODE = 'invalid_table_definition',
                      HINT = 'A unique or primary key constraint of a distributed table must include the distribution column.';
        END IF;
    END LOOP;

    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %s)', p_relid) INTO nonempty;
    IF nonempty THEN
        RAISE EXCEPTION 'table "%" is not empty', c.relname
            USING ERRCODE = 'feature_not_supported',
                  DETAIL = 'Converting a table that already holds data is not supported yet.';
    END IF;

    column_type := a.atttypid;
    column_collation := a.attcollation;
    RETURN NEXT;
END;
$$;

-- The statements that recreate a table as a hash-partitioned parent with the
-- same columns, defaults, constraints and indexes.  scope says where a
-- statement runs: local_pre (before the old table is dropped, local node
-- only), remote_pre (other nodes only) or both.  Sequences owned by columns
-- are detached from the old table so that dropping it keeps them.
CREATE FUNCTION kingres.internal_table_ddl(p_relid regclass, p_column name)
    RETURNS TABLE (ord pg_catalog.int4, scope pg_catalog.text, stmt pg_catalog.text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    c        pg_catalog.pg_class%ROWTYPE;
    nsp      pg_catalog.text;
    qname    pg_catalog.text;
    coldefs  pg_catalog.text;
    r        pg_catalog.record;
    n        pg_catalog.int4 := 0;
    posts    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    p        pg_catalog.text;
    owner    pg_catalog.text;
BEGIN
    SELECT * INTO c FROM pg_class WHERE oid = p_relid;
    SELECT nspname INTO nsp FROM pg_namespace WHERE oid = c.relnamespace;
    qname := format('%I.%I', nsp, c.relname);

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
               a.attname,
               format_type(sq.seqtypid, NULL) AS seqtype
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
        stmt := format('CREATE SEQUENCE IF NOT EXISTS %s AS %s', r.qseq, r.seqtype);
        RETURN NEXT;
        n := n + 1; ord := n; scope := 'local_pre';
        stmt := format('ALTER SEQUENCE %s OWNED BY NONE', r.qseq);
        RETURN NEXT;
        posts := posts || format('ALTER SEQUENCE %s OWNED BY %s.%I', r.qseq, qname, r.attname);
    END LOOP;

    SELECT string_agg(
             format('%I %s%s%s%s%s', a.attname, format_type(a.atttypid, a.atttypmod),
                    CASE WHEN a.attcollation <> t.typcollation
                         THEN ' COLLATE ' || format('%I.%I', cn.nspname, co.collname)
                         ELSE '' END,
                    CASE a.attidentity WHEN 'a' THEN ' GENERATED ALWAYS AS IDENTITY'
                                       WHEN 'd' THEN ' GENERATED BY DEFAULT AS IDENTITY'
                                       ELSE '' END,
                    CASE WHEN a.attgenerated = 's'
                         THEN format(' GENERATED ALWAYS AS (%s) STORED', pg_get_expr(ad.adbin, ad.adrelid))
                         WHEN a.attgenerated = 'v'
                         THEN format(' GENERATED ALWAYS AS (%s) VIRTUAL', pg_get_expr(ad.adbin, ad.adrelid))
                         WHEN ad.adbin IS NOT NULL AND a.attidentity = ''
                         THEN ' DEFAULT ' || pg_get_expr(ad.adbin, ad.adrelid)
                         ELSE '' END,
                    CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END),
             ', ' ORDER BY a.attnum)
      INTO coldefs
      FROM pg_attribute a
      JOIN pg_type t ON t.oid = a.atttypid
      LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
      LEFT JOIN pg_collation co ON co.oid = a.attcollation
      LEFT JOIN pg_namespace cn ON cn.oid = co.collnamespace
     WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped;

    n := n + 1; ord := n; scope := 'both';
    -- A NULL column means a reference table: a plain table, a full copy per node.
    IF p_column IS NULL THEN
        stmt := format('CREATE TABLE %s (%s)', qname, coldefs);
    ELSE
        stmt := format('CREATE TABLE %s (%s) PARTITION BY HASH (%I)', qname, coldefs, p_column);
    END IF;
    RETURN NEXT;

    FOR r IN
        SELECT k.conname, pg_get_constraintdef(k.oid) AS def
          FROM pg_constraint k
         WHERE k.conrelid = p_relid AND k.contype IN ('p', 'u', 'c')
         ORDER BY CASE k.contype WHEN 'p' THEN 0 WHEN 'u' THEN 1 ELSE 2 END, k.conname
    LOOP
        n := n + 1; ord := n; scope := 'both';
        stmt := format('ALTER TABLE %s ADD CONSTRAINT %I %s', qname, r.conname, r.def);
        RETURN NEXT;
    END LOOP;
    -- Foreign keys of the table itself (not the copies in its partitions): they
    -- are added on a new node after every table exists there.
    FOR r IN
        SELECT k.conname, pg_get_constraintdef(k.oid) AS def
          FROM pg_constraint k
         WHERE k.conrelid = p_relid AND k.contype = 'f' AND k.conparentid = 0
         ORDER BY k.conname
    LOOP
        n := n + 1; ord := n; scope := 'fk';
        stmt := format('ALTER TABLE %s ADD CONSTRAINT %I %s', qname, r.conname, r.def);
        RETURN NEXT;
    END LOOP;


    FOR r IN
        SELECT pg_get_indexdef(i.indexrelid) AS def
          FROM pg_index i
         WHERE i.indrelid = p_relid
           AND NOT EXISTS (SELECT 1 FROM pg_constraint k
                            WHERE k.conindid = i.indexrelid AND k.conrelid = p_relid
                              AND k.contype IN ('p', 'u', 'x'))
         ORDER BY i.indexrelid
    LOOP
        n := n + 1; ord := n; scope := 'both';
        stmt := r.def;
        RETURN NEXT;
    END LOOP;

    FOREACH p IN ARRAY posts LOOP
        n := n + 1; ord := n; scope := 'both';
        stmt := p;
        RETURN NEXT;
    END LOOP;

    SELECT rolname INTO owner FROM pg_roles WHERE oid = c.relowner;
    IF owner IS DISTINCT FROM current_user THEN
        n := n + 1; ord := n; scope := 'both';
        stmt := format('ALTER TABLE %s OWNER TO %I', qname, owner);
        RETURN NEXT;
    END IF;
END;
$$;

CREATE FUNCTION kingres.create_distributed_table(relation regclass,
                                                  dist_column name,
                                                  colocate_with regclass DEFAULT NULL,
                                                  bucket_count int DEFAULT NULL)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_create_distributed_table'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.create_distributed_table(regclass, name, regclass, int) IS
    'Turn an empty table into a hash-partitioned distributed table (map group primary only)';

-- ---------------------------------------------------------------------------
-- Reference tables, two-phase commit recovery, DDL propagation (version 0.2)
-- ---------------------------------------------------------------------------

-- Check that a table can become a reference table.  Errors name the offending
-- object.
CREATE FUNCTION kingres.internal_check_referenceable(p_relid regclass)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    c     pg_catalog.pg_class%ROWTYPE;
    other pg_catalog.text;
BEGIN
    SELECT * INTO c FROM pg_class WHERE oid = p_relid;
    IF EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid = p_relid) THEN
        RAISE EXCEPTION 'table "%" is already distributed or a reference table', c.relname
            USING ERRCODE = 'duplicate_object';
    END IF;
    IF c.relkind = 'p' OR c.relispartition THEN
        RAISE EXCEPTION 'cannot make table "%" a reference table: partitioned tables and partitions are not supported yet', c.relname
            USING ERRCODE = 'feature_not_supported';
    ELSIF c.relkind <> 'r' THEN
        RAISE EXCEPTION 'cannot make "%" a reference table: it is not an ordinary table', c.relname
            USING ERRCODE = 'wrong_object_type';
    END IF;
    IF c.relpersistence <> 'p' THEN
        RAISE EXCEPTION 'cannot make table "%" a reference table: temporary and unlogged tables are not supported', c.relname
            USING ERRCODE = 'feature_not_supported';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = p_relid OR inhparent = p_relid) THEN
        RAISE EXCEPTION 'cannot make table "%" a reference table: inheritance is not supported', c.relname
            USING ERRCODE = 'feature_not_supported';
    END IF;
    IF c.reloptions IS NOT NULL THEN
        RAISE EXCEPTION 'cannot make table "%" a reference table: storage parameters are not supported yet', c.relname
            USING ERRCODE = 'feature_not_supported';
    END IF;
    IF c.relrowsecurity OR EXISTS (SELECT 1 FROM pg_policy WHERE polrelid = p_relid) THEN
        RAISE EXCEPTION 'cannot make table "%" a reference table: row-level security is not supported yet', c.relname
            USING ERRCODE = 'feature_not_supported';
    END IF;
    SELECT k.conname INTO other FROM pg_constraint k
     WHERE k.contype = 'f' AND (k.conrelid = p_relid OR k.confrelid = p_relid)
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'cannot make table "%" a reference table: foreign key constraint "%" is not supported yet', c.relname, other
            USING ERRCODE = 'feature_not_supported',
                  HINT = 'Create the reference table first and add the foreign key afterwards with ALTER TABLE.';
    END IF;
    SELECT k.conname INTO other FROM pg_constraint k
     WHERE k.contype = 'x' AND k.conrelid = p_relid
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'cannot make table "%" a reference table: exclusion constraint "%" is not supported', c.relname, other
            USING ERRCODE = 'feature_not_supported';
    END IF;
    SELECT t.tgname INTO other FROM pg_trigger t
     WHERE t.tgrelid = p_relid AND NOT t.tgisinternal
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'cannot make table "%" a reference table: trigger "%" is not supported yet', c.relname, other
            USING ERRCODE = 'feature_not_supported';
    END IF;
END;
$$;

CREATE FUNCTION kingres.create_reference_table(relation regclass)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_create_reference_table'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.create_reference_table(regclass) IS
    'Make a table a reference table: a full copy on every node (map group primary only)';

CREATE FUNCTION kingres.recover_prepared_transactions()
    RETURNS int
    AS 'MODULE_PATHNAME', 'kingres_recover_prepared_transactions'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.recover_prepared_transactions() IS
    'Commit or roll back the prepared transactions this node originated; returns how many';

-- True while the ProcessUtility hook of the extension runs a statement that it
-- propagates to every node (or runs on behalf of another node): the drop guard
-- of forget_dropped_tables() lets such a drop through.
CREATE FUNCTION kingres.internal_ddl_authorized()
    RETURNS bool
    AS 'MODULE_PATHNAME', 'kingres_internal_ddl_authorized'
    LANGUAGE C STABLE
    SET search_path = pg_catalog, kingres, pg_temp;


-- The text of a connection error with the values of the options that name key
-- material removed (what the extension does to every connection error); for
-- the tests of that rule, superusers only.
CREATE FUNCTION kingres.internal_scrub_conn_error(message text)
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_internal_scrub_conn_error'
    LANGUAGE C STRICT STABLE
    SET search_path = pg_catalog, kingres, pg_temp;
-- ---------------------------------------------------------------------------
-- Privileges
-- ---------------------------------------------------------------------------

-- Administration and the internal helpers are for superusers (or roles they
-- grant execute to): they rewrite the catalog and open connections.
-- internal_catalog_snapshot() stays executable by everyone: it only reads the
-- catalog, and a node that finds its bucket map stale reads it from the map
-- group as an ordinary user.
REVOKE ALL ON FUNCTION
    kingres.internal_apply_catalog(jsonb),
    kingres.internal_set_local_node(int),
    kingres.internal_reset_node(),
    kingres.internal_check_distributable(regclass, name),
    kingres.internal_table_ddl(regclass, name),
    kingres.init_cluster(text, int),
    kingres.add_node(text, int, int, text),
    kingres.remove_node(int),
    kingres.sync_catalog(),
    kingres.create_distributed_table(regclass, name, regclass, int),
    kingres.internal_check_referenceable(regclass),
    kingres.create_reference_table(regclass),
    kingres.recover_prepared_transactions(),
    kingres.internal_scrub_conn_error(text)
    FROM PUBLIC;

GRANT USAGE ON SCHEMA kingres TO PUBLIC;
GRANT SELECT ON kingres.node, kingres.colocation, kingres.dist_table,
    kingres.bucket_map, kingres.map_version, kingres.local_node,
    kingres.nodes, kingres.buckets TO PUBLIC;

-- ---------------------------------------------------------------------------
-- The cluster secret (review finding S2-04)
-- ---------------------------------------------------------------------------

-- Single-row table: the random value from which the nodes derive the tokens
-- that prove a node-to-node session (src/forward_auth.c).  Made by
-- init_cluster(), copied to a new node by add_node() and by sync_catalog(),
-- never part of the catalog snapshot.  No role has any privilege on it: the
-- extension's C code reads it with the heap API.
CREATE TABLE kingres.cluster_secret (
    secret bytea NOT NULL,
    CONSTRAINT cluster_secret_length_check CHECK (pg_catalog.length(secret) >= 32)
);
CREATE UNIQUE INDEX cluster_secret_single_row_idx ON kingres.cluster_secret ((true));
REVOKE ALL ON kingres.cluster_secret FROM PUBLIC;

COMMENT ON TABLE kingres.cluster_secret IS
    'The cluster secret behind the tokens of node-to-node sessions; no privilege is granted on it to anyone';

-- Make the secret if this node has none.
CREATE FUNCTION kingres.internal_ensure_secret()
    RETURNS pg_catalog.void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
INSERT INTO kingres.cluster_secret
SELECT pg_catalog.decode(pg_catalog.replace(pg_catalog.gen_random_uuid()::pg_catalog.text
                                            OPERATOR(pg_catalog.||)
                                            pg_catalog.gen_random_uuid()::pg_catalog.text,
                                            '-', ''), 'hex')
 WHERE NOT EXISTS (SELECT 1 FROM kingres.cluster_secret);
$$;

-- Install the secret of the cluster on this node (the hex form of its bytes).
CREATE FUNCTION kingres.internal_apply_secret(secret_hex pg_catalog.text)
    RETURNS pg_catalog.void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DELETE FROM kingres.cluster_secret;
INSERT INTO kingres.cluster_secret VALUES (pg_catalog.decode(secret_hex, 'hex'));
$$;

-- The token this node would accept from a session of the role on the node with
-- that id; for the tests of the rule, superusers only.
CREATE FUNCTION kingres.internal_forward_token(role_name text, node_id int)
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_internal_forward_token'
    LANGUAGE C STRICT STABLE
    SET search_path = pg_catalog, kingres, pg_temp;

REVOKE ALL ON FUNCTION
    kingres.internal_ensure_secret(),
    kingres.internal_apply_secret(text),
    kingres.internal_forward_token(text, int)
    FROM PUBLIC;


-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

-- On the map group primary only, never synchronized: the record of every move
-- of a bucket.  A phase is recorded before the action it announces, and every
-- action is written so that it can be repeated (see move_bucket()).
--   cancel_requested     cancel_bucket_move() asked the running move to stop
--   cleanup_after        when the orphan tables of a finished switch may go
--   replication_dropped  the subscription, slot and publication are gone
--   sub_oid              the subscription on the target (its table sync slots on the
--                        source are named after it)
CREATE TABLE kingres.bucket_move (
    move_id             bigint      GENERATED ALWAYS AS IDENTITY,
    colocation_id       int         NOT NULL,
    bucket              int         NOT NULL,
    source_group        int         NOT NULL,
    target_group        int         NOT NULL,
    phase               text        NOT NULL,
    detail              text,
    slot_name           text,
    started_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    finished_at         timestamptz,
    cancel_requested    bool        NOT NULL DEFAULT false,
    cleanup_after       timestamptz,
    replication_dropped bool        NOT NULL DEFAULT false,
    sub_oid             oid,
    CONSTRAINT bucket_move_pkey PRIMARY KEY (move_id),
    CONSTRAINT bucket_move_phase_check CHECK (phase IN
        ('preparing', 'copying', 'catching_up', 'switching',
         'cleaning', 'done', 'failed', 'cancelled')),
    CONSTRAINT bucket_move_groups_check CHECK (source_group <> target_group)
);
-- One move per bucket at a time.
CREATE UNIQUE INDEX bucket_move_unfinished_idx
    ON kingres.bucket_move (colocation_id, bucket)
    WHERE phase IN ('preparing', 'copying', 'catching_up', 'switching');

COMMENT ON TABLE kingres.bucket_move IS
    'Map group primary only, never synchronized: the record of every move of a bucket between shard groups';

-- Node-local, never synchronized: rows that a move inserts on the source after
-- it has locked the bucket's tables.  They are published with the bucket; when
-- one has arrived on the target, every earlier change of the bucket has.
CREATE TABLE kingres.move_marker (
    move_id    bigint      NOT NULL,
    seq        bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT move_marker_pkey PRIMARY KEY (move_id, seq)
);

COMMENT ON TABLE kingres.move_marker IS
    'Node-local: replication markers of bucket moves; never synchronized';

-- ---------------------------------------------------------------------------
-- C functions
-- ---------------------------------------------------------------------------

-- Run SQL on a node in a session of its own and return its rows as a JSON array
-- of arrays of text.  Superusers only; bounded and interruptible.
CREATE FUNCTION kingres.internal_node_exec(node_id int, sql text, timeout_ms int DEFAULT NULL)
    RETURNS jsonb
    AS 'MODULE_PATHNAME', 'kingres_internal_node_exec'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_switch_bucket(move_id bigint, switch_timeout_ms int, cleanup_delay_s int)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_switch_bucket'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_attach_bucket(parent regclass, bucket int, bucket_count int)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_attach_bucket'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_detach_bucket(parent regclass, bucket int, orphan text)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_detach_bucket'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_subscription_conninfo(host text, port int)
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_internal_subscription_conninfo'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

-- The mark of a live move: the session that runs it holds a lock that nothing
-- else takes; whoever finds it held leaves the move alone.
CREATE FUNCTION kingres.internal_move_lock(move_id bigint)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_move_lock'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_move_unlock(move_id bigint)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_move_unlock'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_move_alive(move_id bigint)
    RETURNS bool
    AS 'MODULE_PATHNAME', 'kingres_internal_move_alive'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_fault_point(point text)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_fault_point'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

-- ---------------------------------------------------------------------------
-- Small helpers
-- ---------------------------------------------------------------------------

CREATE FUNCTION kingres.internal_require_superuser()
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    IF NOT (SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.rolname OPERATOR(pg_catalog.=) current_user) THEN
        RAISE EXCEPTION 'permission denied: this function is for superusers'
            USING ERRCODE = 'insufficient_privilege',
                  DETAIL = 'Moving buckets creates publications, subscriptions and replication slots on the nodes.';
    END IF;
END;
$$;

-- This node must be the map group primary.
CREATE FUNCTION kingres.internal_require_map_group()
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1
                     FROM kingres.local_node l
                     JOIN kingres.node n ON n.node_id OPERATOR(pg_catalog.=) l.node_id
                    WHERE n.group_id OPERATOR(pg_catalog.=) 0 AND n.role OPERATOR(pg_catalog.=) 'primary') THEN
        RAISE EXCEPTION 'this function can only run on the map group primary'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
END;
$$;

-- The first value of the first row of what a statement returns on a node.
CREATE FUNCTION kingres.internal_exec_scalar(p_node int, p_sql text, p_timeout_ms int DEFAULT NULL)
    RETURNS text
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT (kingres.internal_node_exec(p_node, p_sql, p_timeout_ms) OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0;
$$;

-- The node id of the active primary of a shard group, or NULL.
CREATE FUNCTION kingres.internal_group_node(p_group int)
    RETURNS int
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT n.node_id FROM kingres.node n
 WHERE n.group_id OPERATOR(pg_catalog.=) p_group AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active;
$$;

-- The tables of a colocation group in the order in which their bucket
-- partitions are attached: a table comes after the tables it references, so
-- that the foreign keys of an attached partition find their referenced rows.
CREATE FUNCTION kingres.internal_move_tables(p_colocation int)
    RETURNS TABLE (ord int, schema_name text, table_name text)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r        pg_catalog.record;
    picked   pg_catalog.oid[] := ARRAY[]::pg_catalog.oid[];
    n        pg_catalog.int4 := 0;
    total    pg_catalog.int4;
    progress pg_catalog.bool;
BEGIN
    SELECT pg_catalog.count(*) INTO total
      FROM kingres.dist_table d
     WHERE d.colocation_id OPERATOR(pg_catalog.=) p_colocation AND d.method OPERATOR(pg_catalog.=) 'hash';
    WHILE n < total LOOP
        progress := false;
        FOR r IN
            SELECT d.relid::pg_catalog.oid AS reloid, ns.nspname, c.relname
              FROM kingres.dist_table d
              JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid
              JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) c.relnamespace
             WHERE d.colocation_id OPERATOR(pg_catalog.=) p_colocation AND d.method OPERATOR(pg_catalog.=) 'hash'
               AND NOT (d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) ANY (picked))
             ORDER BY ns.nspname, c.relname
        LOOP
            IF NOT EXISTS (SELECT 1
                             FROM pg_catalog.pg_constraint k
                             JOIN kingres.dist_table d2 ON d2.relid::pg_catalog.oid OPERATOR(pg_catalog.=) k.confrelid
                            WHERE k.conrelid OPERATOR(pg_catalog.=) r.reloid
                              AND k.contype OPERATOR(pg_catalog.=) 'f'
                              AND k.conparentid OPERATOR(pg_catalog.=) 0
                              AND d2.colocation_id OPERATOR(pg_catalog.=) p_colocation
                              AND k.confrelid OPERATOR(pg_catalog.<>) r.reloid
                              AND NOT (k.confrelid OPERATOR(pg_catalog.=) ANY (picked))) THEN
                n := n + 1;
                picked := picked OPERATOR(pg_catalog.||) r.reloid;
                ord := n;
                schema_name := r.nspname;
                table_name := r.relname;
                RETURN NEXT;
                progress := true;
            END IF;
        END LOOP;
        IF NOT progress THEN
            RAISE EXCEPTION 'the tables of colocation group % have a cycle of foreign keys', p_colocation
                USING ERRCODE = 'feature_not_supported',
                      HINT = 'Buckets of such a group cannot be moved yet.';
        END IF;
    END LOOP;
END;
$$;

-- The name of the bucket partition of a table.
CREATE FUNCTION kingres.internal_bucket_part_name(p_table text, p_bucket int)
    RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT p_table OPERATOR(pg_catalog.||) '_b' OPERATOR(pg_catalog.||) pg_catalog.lpad(p_bucket::pg_catalog.text, 4, '0');
$$;

-- ---------------------------------------------------------------------------
-- set_node_property
-- ---------------------------------------------------------------------------

CREATE FUNCTION kingres.set_node_property(node_id int, should_have_shards bool)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_node pg_catalog.int4 := node_id;
    v_flag pg_catalog.bool := should_have_shards;
BEGIN
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    UPDATE kingres.node n SET should_have_shards = v_flag WHERE n.node_id OPERATOR(pg_catalog.=) v_node;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'node % does not exist', v_node USING ERRCODE = 'undefined_object';
    END IF;
    PERFORM kingres.sync_catalog();
END;
$$;

COMMENT ON FUNCTION kingres.set_node_property(int, bool) IS
    'Change whether a node accepts buckets (should_have_shards); map group primary only; atomic';

-- ---------------------------------------------------------------------------
-- The plan of a rebalance
-- ---------------------------------------------------------------------------

-- The moves that even out the number of buckets per shard group, in the order
-- they should run (design/shard-phase4.md, "Planning a rebalance").  Per
-- colocation group: the receiving groups (the primary has should_have_shards)
-- get bucket_count / receivers buckets each, the remainder one each to the
-- receivers that already hold the most; every other group is drained.  Only
-- the surplus of a group is moved, and only to groups below their target, so
-- the plan moves the fewest buckets.  A bucket that is itself being moved is
-- left alone.  The order interleaves colocation groups and target groups: the
-- n-th bucket into a group of every colocation group comes before the
-- (n+1)-th of any.
CREATE FUNCTION kingres.rebalance_plan(max_moves int DEFAULT NULL)
    RETURNS TABLE (colocation_id int, bucket int, source_group int, target_group int, size_bytes bigint)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_max     pg_catalog.int4 := max_moves;
    v_receivers pg_catalog.int4;
    r         pg_catalog.record;
    v_sizes   pg_catalog.jsonb := '{}'::pg_catalog.jsonb;
    v_node    pg_catalog.int4;
    v_is_super pg_catalog.bool;
    v_plan    pg_catalog.jsonb;
BEGIN
    IF v_max IS NOT NULL AND v_max < 0 THEN
        RAISE EXCEPTION 'max_moves must not be negative' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT pg_catalog.count(DISTINCT n.group_id) INTO v_receivers
      FROM kingres.node n
     WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND n.should_have_shards;
    IF v_receivers = 0 THEN
        RAISE EXCEPTION 'no shard group accepts buckets'
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'Set should_have_shards on a node with kingres.set_node_property().';
    END IF;

    WITH recv AS (
        SELECT DISTINCT n.group_id
          FROM kingres.node n
         WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND n.should_have_shards
    ),
    colo AS (
        SELECT c.colocation_id, c.bucket_count
          FROM kingres.colocation c
         WHERE EXISTS (SELECT 1 FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) c.colocation_id)
    ),
    groups AS (
        SELECT c.colocation_id, g.group_id
          FROM colo c
         CROSS JOIN (SELECT recv.group_id FROM recv
                      UNION SELECT m.group_id FROM kingres.bucket_map m) g
    ),
    cnt AS (
        SELECT g.colocation_id, g.group_id,
               (SELECT pg_catalog.count(*) FROM kingres.bucket_map m
                 WHERE m.colocation_id OPERATOR(pg_catalog.=) g.colocation_id
                   AND m.group_id OPERATOR(pg_catalog.=) g.group_id)::pg_catalog.int4 AS cnt,
               EXISTS (SELECT 1 FROM recv WHERE recv.group_id OPERATOR(pg_catalog.=) g.group_id) AS is_recv
          FROM groups g
    ),
    ranked AS (
        SELECT cnt.*, c.bucket_count,
               pg_catalog.row_number() OVER (PARTITION BY cnt.colocation_id
                                             ORDER BY cnt.is_recv DESC, cnt.cnt DESC, cnt.group_id) AS rn
          FROM cnt JOIN colo c ON c.colocation_id OPERATOR(pg_catalog.=) cnt.colocation_id
    ),
    tgt AS (
        SELECT ranked.colocation_id, ranked.group_id, ranked.cnt,
               CASE WHEN NOT ranked.is_recv THEN 0
                    ELSE ranked.bucket_count / v_receivers
                         + CASE WHEN ranked.rn <= ranked.bucket_count % v_receivers THEN 1 ELSE 0 END
               END AS target
          FROM ranked
    ),
    surplus AS (
        SELECT m.colocation_id, m.bucket, m.group_id AS source_group,
               pg_catalog.row_number() OVER (PARTITION BY m.colocation_id, m.group_id ORDER BY m.bucket DESC) AS k,
               t.cnt - t.target AS surplus
          FROM kingres.bucket_map m
          JOIN tgt t ON t.colocation_id OPERATOR(pg_catalog.=) m.colocation_id
                    AND t.group_id OPERATOR(pg_catalog.=) m.group_id
         WHERE m.state OPERATOR(pg_catalog.=) 'active' AND t.cnt > t.target
    ),
    give AS (
        SELECT s.colocation_id, s.bucket, s.source_group,
               pg_catalog.row_number() OVER (PARTITION BY s.colocation_id ORDER BY s.k, s.source_group, s.bucket) AS i
          FROM surplus s WHERE s.k <= s.surplus
    ),
    slots AS (
        SELECT t.colocation_id, t.group_id AS target_group, s.n,
               pg_catalog.row_number() OVER (PARTITION BY t.colocation_id ORDER BY s.n, t.group_id) AS j
          FROM tgt t
         CROSS JOIN LATERAL pg_catalog.generate_series(1, t.target - t.cnt) AS s(n)
         WHERE t.target > t.cnt
    ),
    pairs AS (
        SELECT gv.colocation_id, gv.bucket, gv.source_group, sl.target_group,
               pg_catalog.row_number() OVER (PARTITION BY gv.colocation_id, sl.target_group ORDER BY gv.bucket) AS rnk
          FROM give gv
          JOIN slots sl ON sl.colocation_id OPERATOR(pg_catalog.=) gv.colocation_id AND sl.j OPERATOR(pg_catalog.=) gv.i
    )
    SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
               'colocation_id', p.colocation_id, 'bucket', p.bucket, 'source_group', p.source_group,
               'target_group', p.target_group, 'rnk', p.rnk)), '[]'::pg_catalog.jsonb)
      INTO v_plan
      FROM pairs p;

    -- The size of the bucket's partitions on the source, where the caller may
    -- ask (a superuser) and the node answers; NULL otherwise.
    SELECT r2.rolsuper INTO v_is_super FROM pg_catalog.pg_roles r2 WHERE r2.rolname OPERATOR(pg_catalog.=) current_user;
    IF v_is_super THEN
        FOR r IN SELECT DISTINCT x.source_group
                   FROM pg_catalog.jsonb_to_recordset(v_plan) AS x(source_group int) LOOP
            v_node := kingres.internal_group_node(r.source_group);
            CONTINUE WHEN v_node IS NULL;
            BEGIN
                v_sizes := v_sizes OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object(
                    r.source_group::pg_catalog.text,
                    COALESCE((kingres.internal_node_exec(v_node,
                        'SELECT COALESCE(pg_catalog.jsonb_object_agg(s.k, s.sz), ''{}''::pg_catalog.jsonb)::pg_catalog.text FROM ('
                        'SELECT d.colocation_id::pg_catalog.text OPERATOR(pg_catalog.||) '':'' OPERATOR(pg_catalog.||) b.bucket::pg_catalog.text AS k, '
                        'pg_catalog.sum(b.partition_size)::pg_catalog.int8 AS sz '
                        'FROM kingres.buckets b JOIN kingres.dist_table d ON d.relid OPERATOR(pg_catalog.=) b.table_name '
                        'WHERE b.partition_size IS NOT NULL GROUP BY 1) s',
                        10000) OPERATOR(pg_catalog.->) 0 OPERATOR(pg_catalog.->>) 0)::pg_catalog.jsonb,
                        '{}'::pg_catalog.jsonb));
            EXCEPTION WHEN OTHERS THEN
                NULL;	-- size_bytes stays NULL for this group
            END;
        END LOOP;
    END IF;

    RETURN QUERY
    SELECT p.colocation_id, p.bucket, p.source_group, p.target_group,
           (v_sizes OPERATOR(pg_catalog.->) p.source_group::pg_catalog.text
                    OPERATOR(pg_catalog.->>) (p.colocation_id::pg_catalog.text OPERATOR(pg_catalog.||) ':' OPERATOR(pg_catalog.||) p.bucket::pg_catalog.text))::pg_catalog.int8
      FROM pg_catalog.jsonb_to_recordset(v_plan) AS p(colocation_id int, bucket int, source_group int, target_group int, rnk int)
     ORDER BY p.rnk, p.colocation_id, p.target_group, p.bucket
     LIMIT v_max;
END;
$$;

COMMENT ON FUNCTION kingres.rebalance_plan(int) IS
    'The moves that even out the number of buckets per shard group, in the order to run them; changes nothing';

-- ---------------------------------------------------------------------------
-- What a node says about itself (inspection before a move, check_cluster)
-- ---------------------------------------------------------------------------

-- The checks a node must pass to take part in a move, as its role (source or
-- target); returns, for the source, what the target needs to know about each
-- table.  Raises an error that names the setting or the table.
CREATE FUNCTION kingres.internal_move_inspect(p_role text, p_bucket int, p_bucket_count int, p_tables jsonb)
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
            v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object(
                'schema_name', t.schema_name, 'table_name', t.table_name, 'replident', v_ident);
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

-- On the target: the standalone tables that receive the copy.  Each has the
-- columns, defaults, constraints and indexes of the parent, and the check that
-- is the partition constraint, so that attaching it later needs no scan; it is
-- NOT attached to the parent, so no scan of the parent reads it while the copy
-- is incomplete.  p_tables is the answer of internal_move_inspect on the source.
CREATE FUNCTION kingres.internal_create_move_tables(p_move bigint, p_bucket int, p_bucket_count int, p_tables jsonb)
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
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR t IN SELECT x.schema_name, x.table_name, x.replident
               FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text, replident text)
    LOOP
        v_part := kingres.internal_bucket_part_name(t.table_name, p_bucket);
        v_parent := pg_catalog.format('%I.%I', t.schema_name, t.table_name)::pg_catalog.regclass;
        SELECT d.dist_column INTO v_col FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_parent;
        SELECT r.rolname INTO v_owner FROM pg_catalog.pg_class c JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) c.relowner
         WHERE c.oid OPERATOR(pg_catalog.=) v_parent::pg_catalog.oid;
        IF pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, v_part)) IS NOT NULL THEN
            CONTINUE;	-- made by an earlier attempt of this move
        END IF;
        EXECUTE pg_catalog.format('CREATE TABLE %I.%I (LIKE %I.%I INCLUDING ALL)',
                                  t.schema_name, v_part, t.schema_name, t.table_name);
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ADD CONSTRAINT ks_bucket_check CHECK '
                                  '(pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass, %s, %s, %I))',
                                  t.schema_name, v_part, v_parent::pg_catalog.text, p_bucket_count, p_bucket, v_col);
        IF t.replident OPERATOR(pg_catalog.=) 'f' THEN
            EXECUTE pg_catalog.format('ALTER TABLE %I.%I REPLICA IDENTITY FULL', t.schema_name, v_part);
        END IF;
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I OWNER TO %I', t.schema_name, v_part, v_owner);
        EXECUTE pg_catalog.format('COMMENT ON TABLE %I.%I IS %L', t.schema_name, v_part,
                                  'kingres_shard bucket move ' OPERATOR(pg_catalog.||) p_move::pg_catalog.text);
    END LOOP;
END;
$$;

-- On the target: drop the standalone tables of a move (only those that carry
-- its comment and are not partitions of anything).
CREATE FUNCTION kingres.internal_drop_move_tables(p_move bigint, p_bucket int, p_tables jsonb)
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
                    WHERE c.oid OPERATOR(pg_catalog.=) v_oid AND NOT c.relispartition AND c.relkind OPERATOR(pg_catalog.=) 'r')
           AND pg_catalog.obj_description(v_oid, 'pg_class') IS NOT DISTINCT FROM
               ('kingres_shard bucket move ' OPERATOR(pg_catalog.||) p_move::pg_catalog.text) THEN
            EXECUTE pg_catalog.format('DROP TABLE %I.%I', t.schema_name, v_part);
            n := n + 1;
        END IF;
    END LOOP;
    RETURN n;
END;
$$;

-- On the source: the publication of the bucket's partitions (and the marker
-- rows of the move), and the logical slot.
CREATE FUNCTION kingres.internal_create_move_publication(p_move bigint, p_bucket int, p_tables jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_name  pg_catalog.text := pg_catalog.format('ks_move_%s', p_move);
    v_list  pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT pg_catalog.string_agg(pg_catalog.format('%I.%I', x.schema_name,
                                                   kingres.internal_bucket_part_name(x.table_name, p_bucket)), ', ')
      INTO v_list
      FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text);
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_publication p WHERE p.pubname OPERATOR(pg_catalog.=) v_name) THEN
        EXECUTE pg_catalog.format('DROP PUBLICATION %I', v_name);
    END IF;
    EXECUTE pg_catalog.format('CREATE PUBLICATION %I FOR TABLE %s, kingres.move_marker WHERE (move_id = %s) '
                              'WITH (publish = ''insert, update, delete, truncate'', publish_via_partition_root = false)',
                              v_name, v_list, p_move);
END;
$$;

CREATE FUNCTION kingres.internal_create_move_slot(p_name text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_replication_slots s WHERE s.slot_name OPERATOR(pg_catalog.=) p_name) THEN
        PERFORM pg_catalog.pg_create_logical_replication_slot(p_name, 'pgoutput');
    END IF;
END;
$$;

-- On the target: the subscription, with the initial copy, on the slot made
-- before.  The connection string is built here, from this node's own
-- kingres_shard.node_conninfo.  synchronous_commit = on: after the switch the
-- subscription is gone and nothing can replay what a crash would lose.
CREATE FUNCTION kingres.internal_create_move_subscription(p_name text, p_host text, p_port int)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_subscription s WHERE s.subname OPERATOR(pg_catalog.=) p_name) THEN
        RETURN;
    END IF;
    EXECUTE pg_catalog.format('CREATE SUBSCRIPTION %I CONNECTION %L PUBLICATION %I '
                              'WITH (connect = true, create_slot = false, slot_name = %L, copy_data = true, '
                              'enabled = true, synchronous_commit = ''on'', streaming = ''off'', '
                              'binary = false, origin = ''any'', disable_on_error = false)',
                              p_name, kingres.internal_subscription_conninfo(p_host, p_port), p_name, p_name);
END;
$$;

-- On the target: how far the copy is.
CREATE FUNCTION kingres.internal_move_subscription_status(p_name text)
    RETURNS jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT COALESCE(
    (SELECT pg_catalog.jsonb_build_object(
                'enabled', s.subenabled,
                'tables', (SELECT pg_catalog.count(*) FROM pg_catalog.pg_subscription_rel r WHERE r.srsubid OPERATOR(pg_catalog.=) s.oid),
                'ready', (SELECT pg_catalog.count(*) FROM pg_catalog.pg_subscription_rel r
                           WHERE r.srsubid OPERATOR(pg_catalog.=) s.oid AND r.srsubstate OPERATOR(pg_catalog.=) 'r'),
                'errors', COALESCE((SELECT st.apply_error_count + st.sync_error_count
                                      FROM pg_catalog.pg_stat_subscription_stats st
                                     WHERE st.subid OPERATOR(pg_catalog.=) s.oid), 0))
       FROM pg_catalog.pg_subscription s WHERE s.subname OPERATOR(pg_catalog.=) p_name),
    pg_catalog.jsonb_build_object('missing', true));
$$;

-- On the source: the lag of the slot behind the WAL, in bytes, or NULL.
CREATE FUNCTION kingres.internal_move_slot_lag(p_name text)
    RETURNS bigint
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.pg_wal_lsn_diff(pg_catalog.pg_current_wal_lsn(), s.confirmed_flush_lsn)::pg_catalog.int8
  FROM pg_catalog.pg_replication_slots s WHERE s.slot_name OPERATOR(pg_catalog.=) p_name;
$$;

-- On the source: drop the slot (it may still be released by a stopping
-- walsender), the publication and the markers of a move.  Returns true when
-- nothing is left.
CREATE FUNCTION kingres.internal_drop_move_publication(p_move bigint, p_sub_oid oid DEFAULT NULL)
    RETURNS bool
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_name pg_catalog.text := pg_catalog.format('ks_move_%s', p_move);
    r      pg_catalog.record;
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_replication_slots s WHERE s.slot_name OPERATOR(pg_catalog.=) v_name) THEN
        BEGIN
            PERFORM pg_catalog.pg_drop_replication_slot(v_name);
        EXCEPTION WHEN object_in_use THEN
            RETURN false;	-- the walsender has not let go yet
        END;
    END IF;
    -- The table sync slots of the subscription, left when it was dropped without
    -- reaching this node.
    IF p_sub_oid IS NOT NULL THEN
        FOR r IN SELECT s.slot_name FROM pg_catalog.pg_replication_slots s
                  WHERE pg_catalog.starts_with(s.slot_name, pg_catalog.format('pg_%s_sync_', p_sub_oid))
        LOOP
            BEGIN
                PERFORM pg_catalog.pg_drop_replication_slot(r.slot_name);
            EXCEPTION WHEN object_in_use THEN
                RETURN false;
            END;
        END LOOP;
    END IF;
    EXECUTE pg_catalog.format('DROP PUBLICATION IF EXISTS %I', v_name);
    DELETE FROM kingres.move_marker m WHERE m.move_id OPERATOR(pg_catalog.=) p_move;
    RETURN true;
END;
$$;

-- On the source: drop the orphan tables of a move.
CREATE FUNCTION kingres.internal_drop_orphans(p_move bigint)
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
                AND c.relkind OPERATOR(pg_catalog.=) 'r'
                AND pg_catalog.starts_with(c.relname, pg_catalog.format('orphan_%s_', p_move))
              ORDER BY c.relname
    LOOP
        EXECUTE pg_catalog.format('DROP TABLE kingres.%I', r.relname);
        n := n + 1;
    END LOOP;
    RETURN n;
END;
$$;

-- On the target: wait for a marker row (see move_marker); true when it came.
CREATE FUNCTION kingres.internal_marker_seen(p_move bigint, p_seq bigint)
    RETURNS bool
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT EXISTS (SELECT 1 FROM kingres.move_marker m
                WHERE m.move_id OPERATOR(pg_catalog.=) p_move AND m.seq OPERATOR(pg_catalog.=) p_seq);
$$;

-- ---------------------------------------------------------------------------
-- The record of a move
-- ---------------------------------------------------------------------------

-- Record a phase (and what the move waits for).  Runs in a session of its own
-- (through internal_node_exec) so that it is committed at once.  A phase never
-- moves back from a finished one.
CREATE FUNCTION kingres.internal_move_set(p_move bigint, p_phase text, p_detail text)
    RETURNS void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
UPDATE kingres.bucket_move m
   SET phase = p_phase, detail = p_detail, updated_at = pg_catalog.clock_timestamp()
 WHERE m.move_id OPERATOR(pg_catalog.=) p_move
   AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching');
$$;

-- Take the bucket: check everything under the cluster lock, record the move
-- and set the bucket to "moving" on every node, atomically.  Runs in a session
-- of its own.
CREATE FUNCTION kingres.internal_begin_move(p_colocation int, p_bucket int, p_target int)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_source pg_catalog.int4;
    v_state  pg_catalog.text;
    v_count  pg_catalog.int4;
    v_id     pg_catalog.int8;
BEGIN
    PERFORM kingres.internal_require_superuser();
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();

    SELECT c.bucket_count INTO v_count FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) p_colocation;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'colocation group % does not exist', p_colocation USING ERRCODE = 'undefined_object';
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

-- Take a marker round trip: insert a marker row on the source and wait up to
-- p_wait_ms for it to arrive on the target.  True when it did.
CREATE FUNCTION kingres.internal_marker_roundtrip(p_move bigint, p_src_node int, p_tgt_node int, p_wait_ms int)
    RETURNS bool
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_seq      pg_catalog.int8;
    v_deadline pg_catalog.timestamptz := pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+) pg_catalog.make_interval(secs => p_wait_ms / 1000.0);
BEGIN
    v_seq := kingres.internal_exec_scalar(p_src_node, pg_catalog.format(
        'INSERT INTO kingres.move_marker (move_id, seq) VALUES (%s, (extract(epoch FROM pg_catalog.clock_timestamp()) * 1000000)::pg_catalog.int8) RETURNING seq',
        p_move))::pg_catalog.int8;
    LOOP
        IF kingres.internal_exec_scalar(p_tgt_node, pg_catalog.format('SELECT kingres.internal_marker_seen(%s, %s)', p_move, v_seq))::pg_catalog.bool THEN
            RETURN true;
        END IF;
        IF pg_catalog.clock_timestamp() OPERATOR(pg_catalog.>=) v_deadline THEN
            RETURN false;
        END IF;
        PERFORM pg_catalog.pg_sleep(0.02);
    END LOOP;
END;
$$;

-- Drop the subscription of a move on the target: the plain DROP SUBSCRIPTION,
-- which also drops the slots on the publisher; when the publisher cannot be
-- reached, without the slot (which the caller then drops from its side).
CREATE FUNCTION kingres.internal_drop_move_subscription(p_node int, p_name text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    BEGIN
        PERFORM kingres.internal_node_exec(p_node, pg_catalog.format('DROP SUBSCRIPTION IF EXISTS %I', p_name), 120000);
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'kingres_shard: DROP SUBSCRIPTION % failed (%); dropping it without its slot', p_name, SQLERRM;
        PERFORM kingres.internal_node_exec(p_node, pg_catalog.format(
            'ALTER SUBSCRIPTION %I DISABLE; ALTER SUBSCRIPTION %I SET (slot_name = NONE); DROP SUBSCRIPTION %I',
            p_name, p_name, p_name), 120000);
    END;
END;
$$;

-- Tear down the replication of a move on both nodes: the subscription (target),
-- then the slot, publication and markers (source).  Repeatable.  Bounded: a
-- slot that is still held is retried for a few seconds.
CREATE FUNCTION kingres.internal_drop_move_replication(p_move bigint, p_src_node int, p_tgt_node int)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_tries pg_catalog.int4 := 0;
BEGIN
    PERFORM kingres.internal_drop_move_subscription(p_tgt_node, pg_catalog.format('ks_move_%s', p_move));
    -- The markers were replicated to the target too.
    PERFORM kingres.internal_node_exec(p_tgt_node, pg_catalog.format(
        'DELETE FROM kingres.move_marker WHERE move_id = %s', p_move));
    LOOP
        IF kingres.internal_exec_scalar(p_src_node, pg_catalog.format('SELECT kingres.internal_drop_move_publication(%s, %s)', p_move,
                                                              COALESCE((SELECT b.sub_oid::pg_catalog.text FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) p_move), 'NULL')))::pg_catalog.bool THEN
            RETURN;
        END IF;
        v_tries := v_tries + 1;
        IF v_tries > 100 THEN
            RAISE EXCEPTION 'the replication slot ks_move_% of node % is still in use', p_move, p_src_node
                USING ERRCODE = 'object_in_use';
        END IF;
        PERFORM pg_catalog.pg_sleep(0.1);
    END LOOP;
END;
$$;

-- Undo a move that has not switched: the subscription, slot, publication and
-- the standalone tables on the target are dropped, the bucket goes back to
-- "active", the move is recorded as p_final.  Runs in a session of its own.
-- p_owner: the caller is the session that runs the move; otherwise a move that
-- is alive (kingres.internal_move_alive()) is skipped.  Returns 'undone', 'skipped', 'switched' (the
-- switch had committed: the move continues as cleaning) or the phase of a
-- move that is not unfinished.  Every step is repeatable, so a failure in the
-- middle (an unreachable node) leaves the move as it was for the next try.
CREATE FUNCTION kingres.internal_undo_move(p_move bigint, p_reason text, p_final text, p_owner bool)
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    m        kingres.bucket_move%ROWTYPE;
    v_src    pg_catalog.int4;
    v_tgt    pg_catalog.int4;
    v_tables pg_catalog.jsonb;
    v_owner  pg_catalog.text;
    v_group  pg_catalog.int4;
    v_state  pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    IF p_final NOT IN ('failed', 'cancelled') THEN
        RAISE EXCEPTION 'invalid final phase "%"', p_final USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF NOT p_owner AND kingres.internal_move_alive(p_move) THEN
        RETURN 'skipped';
    END IF;
    SELECT * INTO m FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) p_move FOR UPDATE SKIP LOCKED;
    IF NOT FOUND THEN
        RETURN 'skipped';
    END IF;
    IF m.phase NOT IN ('preparing', 'copying', 'catching_up', 'switching') THEN
        RETURN m.phase;
    END IF;

    -- A switch that committed is not undone.
    SELECT b.group_id, b.state INTO v_group, v_state FROM kingres.bucket_map b
     WHERE b.colocation_id OPERATOR(pg_catalog.=) m.colocation_id AND b.bucket OPERATOR(pg_catalog.=) m.bucket;
    IF m.phase OPERATOR(pg_catalog.=) 'switching' AND v_group OPERATOR(pg_catalog.=) m.target_group AND v_state OPERATOR(pg_catalog.=) 'active' THEN
        UPDATE kingres.bucket_move b SET phase = 'cleaning', detail = NULL, updated_at = pg_catalog.clock_timestamp(),
               cleanup_after = pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+) pg_catalog.make_interval(secs => 0)
         WHERE b.move_id OPERATOR(pg_catalog.=) p_move;
        RETURN 'switched';
    END IF;

    v_src := kingres.internal_group_node(m.source_group);
    v_tgt := kingres.internal_group_node(m.target_group);
    IF v_src IS NULL OR v_tgt IS NULL THEN
        RAISE EXCEPTION 'shard group % or % has no active primary: cannot undo move %', m.source_group, m.target_group, p_move
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('schema_name', t.schema_name, 'table_name', t.table_name) ORDER BY t.ord)
      INTO v_tables FROM kingres.internal_move_tables(m.colocation_id) t;

    -- A prepared transaction of a switch that did not commit still holds the
    -- locks of the tables; its resolution (kingres.recover_prepared_transactions(),
    -- which the callers run first, in a session of its own) comes before the undo.

    PERFORM kingres.internal_drop_move_subscription(v_tgt, pg_catalog.format('ks_move_%s', p_move));
    PERFORM kingres.internal_fault_point('move_undo_after_subscription');
    PERFORM kingres.internal_drop_move_replication(p_move, v_src, v_tgt);
    IF v_tables IS NOT NULL THEN
        PERFORM kingres.internal_node_exec(v_tgt, pg_catalog.format(
            'SELECT kingres.internal_drop_move_tables(%s, %s, %L::pg_catalog.jsonb)', p_move, m.bucket, v_tables::pg_catalog.text), 120000);
    END IF;
    PERFORM kingres.internal_fault_point('move_undo_after_tables');

    -- The cluster lock, only for the catalog change.
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    UPDATE kingres.bucket_map b SET state = 'active'
     WHERE b.colocation_id OPERATOR(pg_catalog.=) m.colocation_id AND b.bucket OPERATOR(pg_catalog.=) m.bucket
       AND b.group_id OPERATOR(pg_catalog.=) m.source_group AND b.state OPERATOR(pg_catalog.=) 'moving';
    UPDATE kingres.bucket_move b
       SET phase = p_final, detail = p_reason, replication_dropped = true,
           updated_at = pg_catalog.clock_timestamp(), finished_at = pg_catalog.clock_timestamp()
     WHERE b.move_id OPERATOR(pg_catalog.=) p_move;
    PERFORM kingres.sync_catalog();
    RETURN 'undone';
END;
$$;

-- The rest of a move that has switched: tear down the replication, and when
-- the delay is over (or p_force) and both nodes know the new owner, drop the
-- orphan tables of the source and finish.  Returns 'done', 'waiting' or the
-- phase of a move that is not cleaning.  Runs in a session of its own.
CREATE FUNCTION kingres.internal_continue_cleaning(p_move bigint, p_owner bool, p_force bool)
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    m       kingres.bucket_move%ROWTYPE;
    v_src   pg_catalog.int4;
    v_tgt   pg_catalog.int4;
    v_sql   pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    IF NOT p_owner AND kingres.internal_move_alive(p_move) THEN
        RETURN 'skipped';
    END IF;
    SELECT * INTO m FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) p_move FOR UPDATE SKIP LOCKED;
    IF NOT FOUND THEN
        RETURN 'skipped';
    END IF;
    IF m.phase OPERATOR(pg_catalog.<>) 'cleaning' THEN
        RETURN m.phase;
    END IF;
    v_src := kingres.internal_group_node(m.source_group);
    v_tgt := kingres.internal_group_node(m.target_group);
    IF v_src IS NULL OR v_tgt IS NULL THEN
        RETURN 'waiting';
    END IF;

    IF NOT m.replication_dropped THEN
        PERFORM kingres.internal_drop_move_replication(p_move, v_src, v_tgt);
        UPDATE kingres.bucket_move b SET replication_dropped = true, updated_at = pg_catalog.clock_timestamp()
         WHERE b.move_id OPERATOR(pg_catalog.=) p_move;
    END IF;

    IF NOT p_force AND pg_catalog.clock_timestamp() OPERATOR(pg_catalog.<) m.cleanup_after THEN
        RETURN 'waiting';
    END IF;

    -- No transaction of this node may still be prepared on the source or the target:
    -- a participant of the switch that is not committed has not detached (or
    -- attached) yet, and the orphan tables do not exist before it has.
    v_sql := pg_catalog.format('SELECT pg_catalog.count(*) FROM pg_catalog.pg_prepared_xacts WHERE gid LIKE %L',
                               'kingres\_' OPERATOR(pg_catalog.||) (SELECT l.node_id::pg_catalog.text FROM kingres.local_node l) OPERATOR(pg_catalog.||) '\_%');
    IF kingres.internal_exec_scalar(v_src, v_sql, 20000) IS DISTINCT FROM '0'
       OR kingres.internal_exec_scalar(v_tgt, v_sql, 20000) IS DISTINCT FROM '0' THEN
        RETURN 'waiting';
    END IF;

    BEGIN
        PERFORM kingres.internal_node_exec(v_src, pg_catalog.format('SELECT kingres.internal_drop_orphans(%s)', p_move), 60000);
    EXCEPTION WHEN lock_not_available OR query_canceled THEN
        UPDATE kingres.bucket_move b SET detail = 'waiting for readers of the old partitions to finish', updated_at = pg_catalog.clock_timestamp()
         WHERE b.move_id OPERATOR(pg_catalog.=) p_move;
        RETURN 'waiting';
    END;
    PERFORM kingres.internal_fault_point('move_cleanup_after_orphans');
    UPDATE kingres.bucket_move b
       SET phase = 'done', detail = NULL, updated_at = pg_catalog.clock_timestamp(), finished_at = pg_catalog.clock_timestamp()
     WHERE b.move_id OPERATOR(pg_catalog.=) p_move;
    RETURN 'done';
END;
$$;

-- ---------------------------------------------------------------------------
-- move_bucket
-- ---------------------------------------------------------------------------

CREATE FUNCTION kingres.move_bucket(colocation_id int, bucket int, target_group int)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_colo     pg_catalog.int4 := colocation_id;
    v_bucket   pg_catalog.int4 := bucket;
    v_target   pg_catalog.int4 := target_group;
    v_self     pg_catalog.int4;
    v_source   pg_catalog.int4;
    v_src      pg_catalog.int4;
    v_tgt      pg_catalog.int4;
    v_count    pg_catalog.int4;
    v_tables   pg_catalog.jsonb;
    v_info     pg_catalog.jsonb;
    v_id       pg_catalog.int8;
    v_name     pg_catalog.text;
    v_stage    pg_catalog.int4 := 0;	-- 1: recorded, 2: switching, 3: switched
    v_thr      pg_catalog.int8;
    v_sw_ms    pg_catalog.int4;
    v_delay    pg_catalog.int4;
    v_nt       pg_catalog.int4;
    v_node_ms  pg_catalog.int4;
    v_srcrow   pg_catalog.record;
    v_tgtrow   pg_catalog.record;
    v_st       pg_catalog.jsonb;
    v_lag      pg_catalog.int8;
    v_iter     pg_catalog.int4 := 0;
    v_last     pg_catalog.text := '';
    v_now      pg_catalog.text;
    v_cancel   pg_catalog.bool;
    v_phase    pg_catalog.text;
    v_msg      pg_catalog.text;
    v_state    pg_catalog.text;
    v_attempts pg_catalog.int4 := 0;
    v_wait_ms  pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    IF pg_catalog.current_setting('transaction_isolation') OPERATOR(pg_catalog.<>) 'read committed' THEN
        RAISE EXCEPTION 'kingres.move_bucket() must run in a READ COMMITTED transaction'
            USING ERRCODE = 'invalid_transaction_state',
                  DETAIL = 'It follows the progress of other sessions and nodes with statements that must see their latest commits.';
    END IF;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_node_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') * 1000;
    v_thr := (SELECT s.setting::pg_catalog.int8 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.move_catchup_threshold') * 1024;
    v_sw_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.move_switch_timeout');
    v_delay := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.move_cleanup_delay');

    -- Read-only checks first: a move that cannot start leaves nothing behind.
    SELECT c.bucket_count INTO v_count FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_colo;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'colocation group % does not exist', v_colo USING ERRCODE = 'undefined_object';
    END IF;
    SELECT m.group_id INTO v_source FROM kingres.bucket_map m
     WHERE m.colocation_id OPERATOR(pg_catalog.=) v_colo AND m.bucket OPERATOR(pg_catalog.=) v_bucket;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'bucket % of colocation group % does not exist (it has % buckets)', v_bucket, v_colo, v_count
            USING ERRCODE = 'undefined_object';
    END IF;
    IF v_source OPERATOR(pg_catalog.=) v_target THEN
        RAISE EXCEPTION 'bucket % of colocation group % is already on shard group %', v_bucket, v_colo, v_target
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    v_src := kingres.internal_group_node(v_source);
    v_tgt := kingres.internal_group_node(v_target);
    IF v_src IS NULL OR v_tgt IS NULL THEN
        RAISE EXCEPTION 'shard group % or % has no active primary', v_source, v_target
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF EXISTS (SELECT 1 FROM kingres.bucket_move b
                WHERE b.colocation_id OPERATOR(pg_catalog.=) v_colo AND b.bucket OPERATOR(pg_catalog.=) v_bucket
                  AND b.phase IN ('preparing', 'copying', 'catching_up', 'switching')) THEN
        RAISE EXCEPTION 'bucket % of colocation group % is already being moved', v_bucket, v_colo
            USING ERRCODE = 'object_in_use',
                  HINT = 'See kingres.bucket_move; a move can be cancelled with kingres.cancel_bucket_move().';
    END IF;
    SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('schema_name', t.schema_name, 'table_name', t.table_name) ORDER BY t.ord)
      INTO v_tables FROM kingres.internal_move_tables(v_colo) t;
    IF v_tables IS NULL THEN
        RAISE EXCEPTION 'colocation group % has no distributed table', v_colo
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    SELECT n.host, n.port INTO v_srcrow FROM kingres.node n WHERE n.node_id OPERATOR(pg_catalog.=) v_src;
    SELECT n.host, n.port INTO v_tgtrow FROM kingres.node n WHERE n.node_id OPERATOR(pg_catalog.=) v_tgt;

    v_info := (kingres.internal_exec_scalar(v_src, pg_catalog.format(
                  'SELECT kingres.internal_move_inspect(''source'', %s, %s, %L::pg_catalog.jsonb)::pg_catalog.text',
                  v_bucket, v_count, v_tables::pg_catalog.text)))::pg_catalog.jsonb;
    PERFORM kingres.internal_exec_scalar(v_tgt, pg_catalog.format(
                  'SELECT kingres.internal_move_inspect(''target'', %s, %s, %L::pg_catalog.jsonb)::pg_catalog.text',
                  v_bucket, v_count, v_tables::pg_catalog.text));
    PERFORM kingres.internal_fault_point('move_after_precheck');

    -- 1. Preparing: the cluster lock, the record, the state "moving" on every node.
    v_id := kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_begin_move(%s, %s, %s)',
                                                                   v_colo, v_bucket, v_target))::pg_catalog.int8;
    v_name := pg_catalog.format('ks_move_%s', v_id);
    -- The mark of a live move: whoever finds this lock held knows the session is
    -- still running it (resume_bucket_moves() skips it).  It is released when the
    -- function ends, or with the session.
    PERFORM kingres.internal_move_lock(v_id);
    v_stage := 1;

    BEGIN
        PERFORM kingres.internal_fault_point('move_after_preparing');

        -- 2. Copying.
        PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format(
            'SELECT kingres.internal_move_set(%s, ''copying'', ''creating the tables on the target'')', v_id));
        PERFORM kingres.internal_exec_scalar(v_tgt, pg_catalog.format(
            'SELECT kingres.internal_create_move_tables(%s, %s, %s, %L::pg_catalog.jsonb)', v_id, v_bucket, v_count, v_info::pg_catalog.text));
        PERFORM kingres.internal_fault_point('move_after_target_tables');
        PERFORM kingres.internal_exec_scalar(v_src, pg_catalog.format(
            'SELECT kingres.internal_create_move_publication(%s, %s, %L::pg_catalog.jsonb)', v_id, v_bucket, v_tables::pg_catalog.text));
        PERFORM kingres.internal_fault_point('move_after_publication');
        -- The slot waits for the transactions that are running on the source
        -- (they may be long): its wait is bounded by a multiple of node_timeout.
        PERFORM kingres.internal_exec_scalar(v_src, pg_catalog.format('SELECT kingres.internal_create_move_slot(%L)', v_name), v_node_ms * 5);
        PERFORM kingres.internal_fault_point('move_after_slot');
        PERFORM kingres.internal_exec_scalar(v_tgt, pg_catalog.format(
            'SELECT kingres.internal_create_move_subscription(%L, %L, %s)', v_name, v_srcrow.host, v_srcrow.port), v_node_ms * 2);
        PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format(
            'UPDATE kingres.bucket_move SET sub_oid = %s WHERE move_id = %s',
            kingres.internal_exec_scalar(v_tgt, pg_catalog.format('SELECT oid::pg_catalog.int8 FROM pg_catalog.pg_subscription WHERE subname = %L', v_name)), v_id));
        PERFORM kingres.internal_fault_point('move_after_subscription');

        -- 3. Catching up, and 4. switching, until the switch succeeds.
        <<attempts>>
        LOOP
            IF v_attempts OPERATOR(pg_catalog.=) 0 THEN
                PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format(
                    'SELECT kingres.internal_move_set(%s, ''catching_up'', ''waiting for the initial copy'')', v_id));
            END IF;
            LOOP
                SELECT b.cancel_requested INTO v_cancel FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) v_id;
                IF v_cancel THEN
                    RAISE EXCEPTION 'bucket move % was cancelled', v_id USING ERRCODE = 'KS003';
                END IF;
                v_st := kingres.internal_exec_scalar(v_tgt, pg_catalog.format(
                            'SELECT kingres.internal_move_subscription_status(%L)::pg_catalog.text', v_name))::pg_catalog.jsonb;
                IF v_st ? 'missing' THEN
                    RAISE EXCEPTION 'the subscription % disappeared from node %', v_name, v_tgt
                        USING ERRCODE = 'object_not_in_prerequisite_state';
                END IF;
                IF (v_st ->> 'errors')::pg_catalog.int8 > 0 THEN
                    RAISE EXCEPTION 'the replication of bucket % failed on node % (% errors of the apply or table copy workers)',
                        v_bucket, v_tgt, v_st ->> 'errors'
                        USING ERRCODE = 'data_exception',
                              HINT = 'See the log of node ' || v_tgt || '; a row that conflicts with the copy is the usual cause.';
                END IF;
                IF (v_st ->> 'tables')::pg_catalog.int4 > 0 AND (v_st ->> 'ready') OPERATOR(pg_catalog.=) (v_st ->> 'tables') THEN
                    v_lag := kingres.internal_exec_scalar(v_src, pg_catalog.format(
                                 'SELECT kingres.internal_move_slot_lag(%L)', v_name))::pg_catalog.int8;
                    IF v_lag IS NOT NULL AND v_lag <= v_thr THEN
                        EXIT;
                    END IF;
                    -- The slot confirms only what it has sent: a marker proves the replay.
                    IF kingres.internal_marker_roundtrip(v_id, v_src, v_tgt, 1000) THEN
                        EXIT;
                    END IF;
                    v_now := pg_catalog.format('catching up: %s bytes behind', COALESCE(v_lag::pg_catalog.text, '?'));
                ELSE
                    v_now := pg_catalog.format('copying: %s of %s tables ready', v_st ->> 'ready', v_st ->> 'tables');
                END IF;
                IF v_now IS DISTINCT FROM v_last AND v_attempts OPERATOR(pg_catalog.=) 0 THEN
                    PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format(
                        'SELECT kingres.internal_move_set(%s, ''catching_up'', %L)', v_id, v_now));
                    v_last := v_now;
                END IF;
                v_iter := v_iter + 1;
                PERFORM pg_catalog.pg_sleep(LEAST(1.0, 0.05 + v_iter * 0.05));
            END LOOP;
            PERFORM kingres.internal_fault_point('move_after_catchup');

            -- Switching.
            PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format(
                'SELECT kingres.internal_move_set(%s, ''switching'', ''waiting for the write lock of the bucket'')', v_id));
            v_stage := 2;
            PERFORM kingres.internal_fault_point('move_before_switch');
            BEGIN
                v_attempts := v_attempts + 1;
                PERFORM kingres.internal_node_exec(v_self, pg_catalog.format(
                    'SELECT kingres.internal_switch_bucket(%s, %s, %s)', v_id, v_sw_ms, v_delay),
                    v_sw_ms + v_node_ms * 2);
                v_stage := 3;
                EXIT attempts;
            EXCEPTION WHEN OTHERS OR query_canceled THEN
                GET STACKED DIAGNOSTICS v_msg = MESSAGE_TEXT, v_state = RETURNED_SQLSTATE;
                SELECT b.phase INTO v_phase FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) v_id;
                IF v_phase OPERATOR(pg_catalog.=) 'cleaning' THEN
                    v_stage := 3;
                    EXIT attempts;		-- it did commit
                ELSIF v_state IN ('55P03', '40P01', 'KS004')
                      OR (v_state OPERATOR(pg_catalog.=) '57014' AND v_msg ~ 'statement timeout') THEN
                    -- Waited too long for the lock or the replay: nothing was changed.
                    v_stage := 1;
                    PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format(
                        'SELECT kingres.internal_move_set(%s, ''catching_up'', %L)', v_id,
                        'switch attempt ' || v_attempts || ' gave up: ' || v_msg));
                    PERFORM pg_catalog.pg_sleep(0.2 OPERATOR(pg_catalog.+) pg_catalog.random() OPERATOR(pg_catalog.*) 0.5);
                ELSE
                    RAISE;
                END IF;
            END;
        END LOOP attempts;

        PERFORM kingres.internal_fault_point('move_after_switch');
        -- 5. The replication is no longer needed; a failure here does not undo the move.
        BEGIN
            PERFORM kingres.internal_drop_move_replication(v_id, v_src, v_tgt);
            PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format(
                'UPDATE kingres.bucket_move SET replication_dropped = true, updated_at = pg_catalog.clock_timestamp() WHERE move_id = %s', v_id));
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'bucket move %: the bucket has moved, but dropping the replication failed: %; kingres.resume_bucket_moves() repeats it', v_id, SQLERRM;
        END;
        PERFORM kingres.internal_fault_point('move_after_drop_replication');
    EXCEPTION WHEN OTHERS OR query_canceled THEN
        GET STACKED DIAGNOSTICS v_msg = MESSAGE_TEXT, v_state = RETURNED_SQLSTATE;
        SELECT b.phase INTO v_phase FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) v_id;
        IF v_stage < 3 AND v_phase IN ('preparing', 'copying', 'catching_up') THEN
            -- Not switched: undo.  A failure of the undo itself leaves the move for resume_bucket_moves().
            BEGIN
                PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format(
                    'SELECT kingres.internal_undo_move(%s, %L, %L, true)', v_id, v_msg,
                    CASE WHEN v_state OPERATOR(pg_catalog.=) 'KS003' THEN 'cancelled' ELSE 'failed' END), v_node_ms * 4);
            EXCEPTION WHEN OTHERS THEN
                RAISE WARNING 'bucket move %: could not undo it now (%); kingres.resume_bucket_moves() will', v_id, SQLERRM;
            END;
        END IF;
        PERFORM kingres.internal_move_unlock(v_id);
        RAISE;
    END;
    PERFORM kingres.internal_move_unlock(v_id);
    RETURN v_id;
END;
$$;

COMMENT ON FUNCTION kingres.move_bucket(int, int, int) IS
    'Move one bucket of a colocation group to another shard group with logical replication; returns the id of its row in kingres.bucket_move (map group primary, superuser)';

-- ---------------------------------------------------------------------------
-- Resumption, cancellation, cleanup
-- ---------------------------------------------------------------------------

CREATE FUNCTION kingres.resume_bucket_moves()
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r      pg_catalog.record;
    v_self pg_catalog.int4;
    v_res  pg_catalog.text;
    n      pg_catalog.int4 := 0;
    v_to   pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    -- Every node runs the recovery worker, only the map group primary has moves.
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

    -- In a session of its own: the work below runs in sessions of their own that
    -- would wait for the rows this transaction deleted.
    BEGIN
        PERFORM kingres.internal_exec_scalar(v_self, 'SELECT kingres.recover_prepared_transactions()', v_to * 4000);
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'kingres_shard: recovery of prepared transactions failed: %', SQLERRM;
    END;

    FOR r IN SELECT b.move_id, b.phase, b.updated_at FROM kingres.bucket_move b
              WHERE b.phase IN ('preparing', 'copying', 'catching_up', 'switching', 'cleaning')
              ORDER BY b.move_id
    LOOP
        BEGIN
            IF r.phase OPERATOR(pg_catalog.=) 'cleaning' THEN
                v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format(
                    'SELECT kingres.internal_continue_cleaning(%s, false, false)', r.move_id), v_to * 4000);
                IF v_res OPERATOR(pg_catalog.=) 'done' THEN
                    n := n + 1;
                END IF;
            ELSE
                -- A switch may still be committing in the session that runs it.
                IF r.phase OPERATOR(pg_catalog.=) 'switching'
                   AND r.updated_at OPERATOR(pg_catalog.>) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) pg_catalog.make_interval(secs => v_to * 2)) THEN
                    CONTINUE;
                END IF;
                v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format(
                    'SELECT kingres.internal_undo_move(%s, %L, ''failed'', false)', r.move_id,
                    'interrupted; undone by kingres.resume_bucket_moves()'), v_to * 4000);
                IF v_res IN ('undone', 'switched') THEN
                    n := n + 1;
                END IF;
            END IF;
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'kingres_shard: could not resume bucket move %: %', r.move_id, SQLERRM;
        END;
    END LOOP;
    RETURN n;
END;
$$;

COMMENT ON FUNCTION kingres.resume_bucket_moves() IS
    'Undo the moves that were interrupted before the switch, finish those that switched; returns how many it resolved';

CREATE FUNCTION kingres.cancel_bucket_move(move_id bigint)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_move   pg_catalog.int8 := move_id;
    v_self   pg_catalog.int4;
    v_phase  pg_catalog.text;
    v_res    pg_catalog.text;
    v_to     pg_catalog.int4;
    v_start  pg_catalog.timestamptz := pg_catalog.clock_timestamp();
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_to := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout');
    SELECT b.phase INTO v_phase FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) v_move;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'bucket move % does not exist', v_move USING ERRCODE = 'undefined_object';
    END IF;
    IF v_phase OPERATOR(pg_catalog.=) 'switching' THEN
        RAISE EXCEPTION 'bucket move % is switching and cannot be cancelled', v_move
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'The switch ends in a moment, either completed or back to catching up; cancel then.';
    ELSIF v_phase NOT IN ('preparing', 'copying', 'catching_up') THEN
        RAISE EXCEPTION 'bucket move % is already %', v_move, v_phase
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    -- In a session of its own (committed at once): flag it, and undo it if
    -- nobody is running it; otherwise the running session undoes it.
    v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format(
        'UPDATE kingres.bucket_move SET cancel_requested = true WHERE move_id = %s AND phase IN (''preparing'', ''copying'', ''catching_up'')', v_move) ||
        '; SELECT kingres.internal_undo_move(' || v_move || ', ''cancelled by kingres.cancel_bucket_move()'', ''cancelled'', false)', v_to * 4000);
    LOOP
        SELECT b.phase INTO v_phase FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) v_move;
        EXIT WHEN v_phase NOT IN ('preparing', 'copying', 'catching_up');
        IF pg_catalog.clock_timestamp() OPERATOR(pg_catalog.>) (v_start OPERATOR(pg_catalog.+) pg_catalog.make_interval(secs => v_to)) THEN
            RAISE NOTICE 'bucket move % has been asked to stop; it is still %', v_move, v_phase;
            RETURN;
        END IF;
        PERFORM pg_catalog.pg_sleep(0.1);
    END LOOP;
END;
$$;

COMMENT ON FUNCTION kingres.cancel_bucket_move(bigint) IS
    'Stop a move that has not reached the switch and undo it (map group primary, superuser)';

CREATE FUNCTION kingres.cleanup_moved_buckets(force bool DEFAULT false)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_force pg_catalog.bool := force;
    r       pg_catalog.record;
    v_self  pg_catalog.int4;
    v_res   pg_catalog.text;
    n       pg_catalog.int4 := 0;
    v_to    pg_catalog.int4;
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
    FOR r IN SELECT b.move_id FROM kingres.bucket_move b WHERE b.phase OPERATOR(pg_catalog.=) 'cleaning' ORDER BY b.move_id
    LOOP
        BEGIN
            v_res := kingres.internal_exec_scalar(v_self, pg_catalog.format(
                'SELECT kingres.internal_continue_cleaning(%s, false, %L::pg_catalog.bool)', r.move_id, v_force::pg_catalog.text), v_to * 4000);
            IF v_res OPERATOR(pg_catalog.=) 'done' THEN
                n := n + 1;
            END IF;
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'kingres_shard: could not clean up bucket move %: %', r.move_id, SQLERRM;
        END;
    END LOOP;
    RETURN n;
END;
$$;

COMMENT ON FUNCTION kingres.cleanup_moved_buckets(bool) IS
    'Drop the orphan tables of the moves whose delay (kingres_shard.move_cleanup_delay) is over, or of all of them with force; returns how many moves are finished';

-- ---------------------------------------------------------------------------
-- The consistency check
-- ---------------------------------------------------------------------------

-- What this node knows about the things a check looks at.
CREATE FUNCTION kingres.internal_local_state()
    RETURNS jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.jsonb_build_object(
    'node_id', (SELECT l.node_id FROM kingres.local_node l),
    'catalog', kingres.internal_catalog_snapshot(),
    -- bucket partitions that are attached
    'partitions', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                                 'schema_name', ns.nspname, 'table_name', p.relname, 'part', c.relname))
                              FROM pg_catalog.pg_inherits i
                              JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.inhrelid
                              JOIN pg_catalog.pg_class p ON p.oid OPERATOR(pg_catalog.=) i.inhparent
                              JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) p.relnamespace
                              JOIN kingres.dist_table d ON d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) p.oid
                             WHERE d.method OPERATOR(pg_catalog.=) 'hash'), '[]'::pg_catalog.jsonb),
    -- tables with the name of a bucket partition that are not partitions
    'standalone', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                                 'schema_name', ns.nspname, 'part', c.relname,
                                 'comment', pg_catalog.obj_description(c.oid, 'pg_class')))
                              FROM pg_catalog.pg_class c
                              JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) c.relnamespace
                             WHERE c.relkind OPERATOR(pg_catalog.=) 'r' AND NOT c.relispartition
                               AND c.relname OPERATOR(pg_catalog.~) '_b[0-9]{4}$'
                               AND EXISTS (SELECT 1 FROM kingres.dist_table d
                                             JOIN pg_catalog.pg_class p ON p.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid
                                            WHERE d.method OPERATOR(pg_catalog.=) 'hash'
                                              AND p.relnamespace OPERATOR(pg_catalog.=) c.relnamespace
                                              AND p.relname OPERATOR(pg_catalog.=) pg_catalog.left(c.relname, -6))), '[]'::pg_catalog.jsonb),
    'orphans', COALESCE((SELECT pg_catalog.jsonb_agg(c.relname)
                           FROM pg_catalog.pg_class c
                          WHERE c.relnamespace OPERATOR(pg_catalog.=) 'kingres'::pg_catalog.regnamespace
                            AND c.relkind OPERATOR(pg_catalog.=) 'r' AND c.relname OPERATOR(pg_catalog.~) '^orphan_[0-9]+_[0-9]+$'), '[]'::pg_catalog.jsonb),
    'slots', COALESCE((SELECT pg_catalog.jsonb_agg(s.slot_name) FROM pg_catalog.pg_replication_slots s), '[]'::pg_catalog.jsonb),
    'subscriptions', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('name', s.subname, 'oid', s.oid::pg_catalog.int8))
                                 FROM pg_catalog.pg_subscription s), '[]'::pg_catalog.jsonb),
    'publications', COALESCE((SELECT pg_catalog.jsonb_agg(p.pubname) FROM pg_catalog.pg_publication p), '[]'::pg_catalog.jsonb),
    'origins', COALESCE((SELECT pg_catalog.jsonb_agg(o.roname) FROM pg_catalog.pg_replication_origin o), '[]'::pg_catalog.jsonb),
    'prepared', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                               'gid', x.gid, 'age', extract(epoch FROM pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) x.prepared)))
                            FROM pg_catalog.pg_prepared_xacts x), '[]'::pg_catalog.jsonb));
$$;

-- One row per problem of the cluster: the catalog of each node against the map
-- group's, the partitions that exist against the bucket map, what a move left
-- behind (orphan tables, slots, publications, subscriptions, origins, standalone
-- copies), the state of the buckets against the move records, and prepared
-- transactions older than p_prepared_older_than.  No rows: consistent.  Runs on
-- the map group primary, superuser.
CREATE FUNCTION kingres.check_cluster(p_prepared_older_than interval DEFAULT interval '5 minutes')
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_self   pg_catalog.int4;
    v_cat    pg_catalog.jsonb;
    n        pg_catalog.record;
    st       pg_catalog.jsonb;
    x        pg_catalog.record;
    v_move   pg_catalog.int8;
    v_mrow   kingres.bucket_move%ROWTYPE;
    v_age    pg_catalog.float8 := extract(epoch FROM p_prepared_older_than);
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_cat := kingres.internal_catalog_snapshot();

    -- The map itself.
    FOR x IN SELECT c.colocation_id, c.bucket_count, (SELECT pg_catalog.count(*) FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) c.colocation_id) AS have
               FROM kingres.colocation c
    LOOP
        IF x.have OPERATOR(pg_catalog.<>) x.bucket_count THEN
            check_name := 'bucket_map_incomplete'; node_id := v_self;
            object := pg_catalog.format('colocation group %s', x.colocation_id);
            detail := pg_catalog.format('%s of %s buckets are mapped', x.have, x.bucket_count);
            RETURN NEXT;
        END IF;
    END LOOP;
    FOR x IN SELECT DISTINCT m.group_id, m.colocation_id FROM kingres.bucket_map m
              WHERE kingres.internal_group_node(m.group_id) IS NULL
    LOOP
        check_name := 'group_without_primary'; node_id := v_self;
        object := pg_catalog.format('shard group %s', x.group_id);
        detail := pg_catalog.format('owns buckets of colocation group %s and has no active primary', x.colocation_id);
        RETURN NEXT;
    END LOOP;
    -- Buckets that are moving against the records of moves.
    FOR x IN SELECT m.colocation_id, m.bucket FROM kingres.bucket_map m
              WHERE m.state OPERATOR(pg_catalog.=) 'moving'
                AND NOT EXISTS (SELECT 1 FROM kingres.bucket_move b
                                 WHERE b.colocation_id OPERATOR(pg_catalog.=) m.colocation_id AND b.bucket OPERATOR(pg_catalog.=) m.bucket
                                   AND b.phase IN ('preparing', 'copying', 'catching_up', 'switching'))
    LOOP
        check_name := 'bucket_moving_without_move'; node_id := v_self;
        object := pg_catalog.format('bucket %s of colocation group %s', x.bucket, x.colocation_id);
        detail := 'the bucket is marked "moving" but no move is running';
        RETURN NEXT;
    END LOOP;
    FOR x IN SELECT b.move_id, b.colocation_id, b.bucket, b.phase FROM kingres.bucket_move b
              WHERE b.phase IN ('preparing', 'copying', 'catching_up', 'switching')
                AND NOT EXISTS (SELECT 1 FROM kingres.bucket_map m
                                 WHERE m.colocation_id OPERATOR(pg_catalog.=) b.colocation_id AND m.bucket OPERATOR(pg_catalog.=) b.bucket
                                   AND m.state OPERATOR(pg_catalog.=) 'moving')
    LOOP
        check_name := 'move_without_moving_bucket'; node_id := v_self;
        object := pg_catalog.format('bucket move %s', x.move_id);
        detail := pg_catalog.format('the move is %s but bucket %s of colocation group %s is not marked "moving"', x.phase, x.bucket, x.colocation_id);
        RETURN NEXT;
    END LOOP;

    FOR n IN SELECT nd.node_id, nd.host, nd.port, nd.group_id FROM kingres.node nd
              WHERE nd.is_active AND nd.role OPERATOR(pg_catalog.=) 'primary' ORDER BY nd.node_id
    LOOP
        BEGIN
            st := kingres.internal_exec_scalar(n.node_id, 'SELECT kingres.internal_local_state()::pg_catalog.text', 20000)::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS THEN
            check_name := 'node_unreachable'; node_id := n.node_id;
            object := pg_catalog.format('%s:%s', n.host, n.port); detail := SQLERRM;
            RETURN NEXT;
            CONTINUE;
        END;

        -- The version is not part of the comparison: statements that touch no row
        -- of the catalog (a DROP TABLE runs the drop trigger on the node that
        -- drops) still count it up on that node alone.
        IF (st -> 'catalog') OPERATOR(pg_catalog.-) 'version' IS DISTINCT FROM v_cat OPERATOR(pg_catalog.-) 'version' THEN
            check_name := 'catalog_differs'; node_id := n.node_id;
            object := pg_catalog.format('%s:%s', n.host, n.port);
            detail := pg_catalog.format('the catalog of this node (map version %s) differs from the map group''s (version %s)',
                                        st -> 'catalog' ->> 'version', v_cat ->> 'version');
            RETURN NEXT;
        END IF;

        -- Partitions that must be here and are not.
        FOR x IN
            SELECT ns.nspname AS schema_name, c.relname AS table_name, m.bucket,
                   kingres.internal_bucket_part_name(c.relname, m.bucket) AS part
              FROM kingres.dist_table d
              JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid
              JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) c.relnamespace
              JOIN kingres.bucket_map m ON m.colocation_id OPERATOR(pg_catalog.=) d.colocation_id
             WHERE d.method OPERATOR(pg_catalog.=) 'hash' AND m.group_id OPERATOR(pg_catalog.=) n.group_id
               AND NOT EXISTS (SELECT 1 FROM pg_catalog.jsonb_to_recordset(st -> 'partitions') AS p(schema_name text, table_name text, part text)
                                WHERE p.schema_name OPERATOR(pg_catalog.=) ns.nspname AND p.part OPERATOR(pg_catalog.=)
                                      kingres.internal_bucket_part_name(c.relname, m.bucket))
             ORDER BY 1, 2, 3
        LOOP
            check_name := 'partition_missing'; node_id := n.node_id;
            object := pg_catalog.format('%I.%I', x.schema_name, x.part);
            detail := pg_catalog.format('bucket %s of %I.%I belongs to shard group %s and has no attached partition on this node',
                                        x.bucket, x.schema_name, x.table_name, n.group_id);
            RETURN NEXT;
        END LOOP;

        -- Partitions that are here and belong elsewhere.
        FOR x IN
            SELECT p.schema_name, p.table_name, p.part
              FROM pg_catalog.jsonb_to_recordset(st -> 'partitions') AS p(schema_name text, table_name text, part text)
              JOIN kingres.dist_table d ON d.relid::pg_catalog.oid OPERATOR(pg_catalog.=)
                   pg_catalog.to_regclass(pg_catalog.format('%I.%I', p.schema_name, p.table_name))::pg_catalog.oid
              JOIN kingres.bucket_map m ON m.colocation_id OPERATOR(pg_catalog.=) d.colocation_id
                                       AND kingres.internal_bucket_part_name(p.table_name, m.bucket) OPERATOR(pg_catalog.=) p.part
             WHERE m.group_id OPERATOR(pg_catalog.<>) n.group_id
             ORDER BY 1, 2, 3
        LOOP
            check_name := 'partition_misplaced'; node_id := n.node_id;
            object := pg_catalog.format('%I.%I', x.schema_name, x.part);
            detail := 'the partition is attached on this node, whose shard group does not own the bucket';
            RETURN NEXT;
        END LOOP;

        -- Standalone tables with the name of a bucket partition: copies of a move.
        FOR x IN SELECT p.schema_name, p.part, p.comment
                   FROM pg_catalog.jsonb_to_recordset(st -> 'standalone') AS p(schema_name text, part text, comment text)
        LOOP
            v_move := NULLIF(substring(x.comment FROM '^kingres_shard bucket move ([0-9]+)$'), '')::pg_catalog.int8;
            IF v_move IS NOT NULL AND EXISTS (SELECT 1 FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) v_move
                                                 AND b.phase IN ('preparing', 'copying', 'catching_up', 'switching')) THEN
                CONTINUE;
            END IF;
            check_name := 'stray_bucket_table'; node_id := n.node_id;
            object := pg_catalog.format('%I.%I', x.schema_name, x.part);
            detail := CASE WHEN v_move IS NULL THEN 'a table with the name of a bucket partition that is not attached'
                           ELSE pg_catalog.format('copy of bucket move %s, which is not running', v_move) END;
            RETURN NEXT;
        END LOOP;

        -- Orphan tables.
        FOR x IN SELECT o.relname FROM pg_catalog.jsonb_array_elements_text(st -> 'orphans') AS o(relname)
        LOOP
            v_move := substring(x.relname FROM '^orphan_([0-9]+)_')::pg_catalog.int8;
            SELECT * INTO v_mrow FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) v_move;
            IF FOUND AND v_mrow.phase OPERATOR(pg_catalog.=) 'cleaning'
               AND (v_mrow.cleanup_after IS NULL OR pg_catalog.clock_timestamp() OPERATOR(pg_catalog.<) (v_mrow.cleanup_after OPERATOR(pg_catalog.+) interval '1 hour')) THEN
                CONTINUE;
            END IF;
            check_name := 'orphan_table'; node_id := n.node_id;
            object := pg_catalog.format('kingres.%I', x.relname);
            detail := CASE WHEN NOT FOUND THEN pg_catalog.format('no record of bucket move %s', v_move)
                           WHEN v_mrow.phase OPERATOR(pg_catalog.=) 'cleaning' THEN pg_catalog.format('overdue: bucket move %s should have dropped it at %s', v_move, v_mrow.cleanup_after)
                           ELSE pg_catalog.format('bucket move %s is %s', v_move, v_mrow.phase) END;
            RETURN NEXT;
        END LOOP;

        -- Replication objects of moves.
        FOR x IN
            SELECT k.kind, k.name
              FROM (SELECT 'replication_slot' AS kind, s.name FROM pg_catalog.jsonb_array_elements_text(st -> 'slots') AS s(name)
                    UNION ALL
                    SELECT 'subscription', s.name FROM pg_catalog.jsonb_to_recordset(st -> 'subscriptions') AS s(name text)
                    UNION ALL
                    SELECT 'publication', s.name FROM pg_catalog.jsonb_array_elements_text(st -> 'publications') AS s(name)) k
             WHERE k.name OPERATOR(pg_catalog.~) '^ks_move_[0-9]+$'
        LOOP
            v_move := substring(x.name FROM '^ks_move_([0-9]+)$')::pg_catalog.int8;
            SELECT * INTO v_mrow FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) v_move;
            IF FOUND AND (v_mrow.phase IN ('preparing', 'copying', 'catching_up', 'switching')
                          OR (v_mrow.phase OPERATOR(pg_catalog.=) 'cleaning' AND NOT v_mrow.replication_dropped)) THEN
                CONTINUE;
            END IF;
            check_name := x.kind; node_id := n.node_id; object := x.name;
            detail := CASE WHEN NOT FOUND THEN pg_catalog.format('no record of bucket move %s', v_move)
                           ELSE pg_catalog.format('bucket move %s is %s', v_move, v_mrow.phase) END;
            IF x.kind OPERATOR(pg_catalog.=) 'replication_slot' THEN
                detail := detail OPERATOR(pg_catalog.||) '; the slot holds back WAL on this node until it is dropped';
            END IF;
            RETURN NEXT;
        END LOOP;
        -- Table sync slots of a subscription of a move that is over.
        FOR x IN SELECT sl.name FROM pg_catalog.jsonb_array_elements_text(st -> 'slots') AS sl(name)
                  WHERE sl.name OPERATOR(pg_catalog.~) '^pg_[0-9]+_sync_[0-9]+_[0-9]+$'
                    AND NOT EXISTS (SELECT 1 FROM kingres.bucket_move b
                                     WHERE b.sub_oid::pg_catalog.text OPERATOR(pg_catalog.=) substring(sl.name FROM '^pg_([0-9]+)_')
                                       AND (b.phase IN ('preparing', 'copying', 'catching_up', 'switching')
                                            OR (b.phase OPERATOR(pg_catalog.=) 'cleaning' AND NOT b.replication_dropped)))
        LOOP
            check_name := 'replication_slot'; node_id := n.node_id; object := x.name;
            detail := 'a table synchronization slot of a subscription that no running move owns; it holds back WAL on this node until it is dropped';
            RETURN NEXT;
        END LOOP;
        -- Replication origins whose subscription is gone.
        FOR x IN SELECT o.name FROM pg_catalog.jsonb_array_elements_text(st -> 'origins') AS o(name)
                  WHERE o.name OPERATOR(pg_catalog.~) '^pg_[0-9]+(_[0-9]+)?$'
                    AND NOT EXISTS (SELECT 1 FROM pg_catalog.jsonb_to_recordset(st -> 'subscriptions') AS s(name text, oid bigint)
                                     WHERE s.oid::pg_catalog.text OPERATOR(pg_catalog.=) substring(o.name FROM '^pg_([0-9]+)'))
        LOOP
            check_name := 'replication_origin'; node_id := n.node_id; object := x.name;
            detail := 'the origin belongs to no subscription';
            RETURN NEXT;
        END LOOP;

        -- Prepared transactions that nobody resolved.
        FOR x IN SELECT p.gid, p.age FROM pg_catalog.jsonb_to_recordset(st -> 'prepared') AS p(gid text, age float8)
                  WHERE p.age OPERATOR(pg_catalog.>=) v_age ORDER BY p.gid
        LOOP
            check_name := 'prepared_transaction'; node_id := n.node_id; object := x.gid;
            detail := pg_catalog.format('prepared %s seconds ago; kingres.recover_prepared_transactions() on its origin resolves it', pg_catalog.round(x.age::pg_catalog.numeric, 1));
            RETURN NEXT;
        END LOOP;
    END LOOP;
END;
$$;

COMMENT ON FUNCTION kingres.check_cluster(interval) IS
    'One row per inconsistency of the cluster (catalogs, partitions against the bucket map, leftovers of moves, old prepared transactions); none when consistent';

-- ---------------------------------------------------------------------------
-- Privileges
-- ---------------------------------------------------------------------------

REVOKE ALL ON FUNCTION
    kingres.internal_node_exec(int, text, int),
    kingres.internal_switch_bucket(bigint, int, int),
    kingres.internal_attach_bucket(regclass, int, int),
    kingres.internal_detach_bucket(regclass, int, text),
    kingres.internal_subscription_conninfo(text, int),
    kingres.internal_fault_point(text),
    kingres.internal_move_lock(bigint),
    kingres.internal_move_unlock(bigint),
    kingres.internal_move_alive(bigint),
    kingres.internal_require_superuser(),
    kingres.internal_require_map_group(),
    kingres.internal_exec_scalar(int, text, int),
    kingres.internal_group_node(int),
    kingres.internal_move_tables(int),
    kingres.internal_bucket_part_name(text, int),
    kingres.set_node_property(int, bool),
    kingres.internal_move_inspect(text, int, int, jsonb),
    kingres.internal_create_move_tables(bigint, int, int, jsonb),
    kingres.internal_drop_move_tables(bigint, int, jsonb),
    kingres.internal_create_move_publication(bigint, int, jsonb),
    kingres.internal_create_move_slot(text),
    kingres.internal_create_move_subscription(text, text, int),
    kingres.internal_move_subscription_status(text),
    kingres.internal_move_slot_lag(text),
    kingres.internal_drop_move_publication(bigint, oid),
    kingres.internal_drop_orphans(bigint),
    kingres.internal_marker_seen(bigint, bigint),
    kingres.internal_move_set(bigint, text, text),
    kingres.internal_begin_move(int, int, int),
    kingres.internal_marker_roundtrip(bigint, int, int, int),
    kingres.internal_drop_move_subscription(int, text),
    kingres.internal_drop_move_replication(bigint, int, int),
    kingres.internal_undo_move(bigint, text, text, bool),
    kingres.internal_continue_cleaning(bigint, bool, bool),
    kingres.move_bucket(int, int, int),
    kingres.resume_bucket_moves(),
    kingres.cancel_bucket_move(bigint),
    kingres.cleanup_moved_buckets(bool),
    kingres.internal_local_state(),
    kingres.check_cluster(interval)
    FROM PUBLIC;

GRANT SELECT ON kingres.bucket_move TO PUBLIC;

-- ---------------------------------------------------------------------------
-- The map version rule (design/shard-phase4.md, "Implementation notes")
-- ---------------------------------------------------------------------------

-- The latest committed map version of this node, whatever the snapshot of the
-- calling transaction (it counts every change of the catalog; the scan and the
-- fan-out write compare the placement below instead).
CREATE FUNCTION kingres.internal_map_version()
    RETURNS bigint
    AS 'MODULE_PATHNAME', 'kingres_internal_map_version'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

-- The placement of a colocation group on this node (which group holds each
-- bucket) as one number, from the latest committed state: what a fragment of a
-- distributed scan or a fan-out write compares with its origin.  It changes when a
-- bucket moves, not with the other changes of the catalog.
CREATE FUNCTION kingres.internal_placement_hash(colocation_id int, parent regclass DEFAULT NULL)
    RETURNS bigint
    AS 'MODULE_PATHNAME', 'kingres_internal_placement_hash'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

-- The version is compared across nodes, so it must count catalog changes only:
-- the drop trigger used to delete from kingres.dist_table on every DROP, which
-- counted the version up on the node that dropped even a table that has nothing
-- to do with the cluster.  Same function as before, with the delete only when
-- the drop reaches a table that is distributed.
CREATE OR REPLACE FUNCTION kingres.forget_dropped_tables()
    RETURNS event_trigger
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    dropped pg_catalog.oid[];
    hit     pg_catalog.regclass;
BEGIN
    SELECT pg_catalog.array_agg(o.objid) INTO dropped
      FROM pg_catalog.pg_event_trigger_dropped_objects() o
     WHERE o.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass;
    IF dropped IS NULL THEN
        RETURN;
    END IF;

    -- A drop that reaches a distributed table (also by dropping one of its
    -- columns through a cascade) would act on this node only.  The DROP TABLE
    -- statements that the extension propagates to every node are authorized
    -- by the ProcessUtility hook (kingres.internal_ddl_authorized()); a
    -- superuser who repeats a statement on every node by hand switches the
    -- check off with kingres_shard.enable_routing (a superuser-only setting).
    IF pg_catalog.current_setting('kingres_shard.enable_routing', true) IS DISTINCT FROM 'off'
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
END;
$$;
