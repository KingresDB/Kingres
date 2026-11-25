-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_shard--0.2.sql
--   Install script of the kingres_shard extension, version 0.2: the cluster
--   catalog, the functions that maintain it and the introspection views.
--   Installing this script and upgrading from 0.1 with kingres_shard--0.1--0.2.sql
--   must give identical catalogs (the "upgrade" regression test compares them).
--   Design: kingres/docs/design/shard-mvp.md and shard-phase2a.md.

\echo Use "CREATE EXTENSION kingres_shard" to load this file. \quit

-- The schema of the extension must not be one that a role that is not a superuser holds or can create objects in: that role would own what the service role, a
-- superuser, reads and runs.  The check is the same in every script of the extension, the install scripts of the older versions and the upgrade scripts too (a
-- schema that was made, or given away, after the install is met by ALTER EXTENSION ... UPDATE).  A schema that does not exist yet is made by the script.
DO $kgchk$
DECLARE
    v_schema pg_catalog.text := 'kingres';
    v_owner  pg_catalog.name;
BEGIN
    SELECT r.rolname INTO v_owner
      FROM pg_catalog.pg_namespace n JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) n.nspowner
     WHERE n.nspname OPERATOR(pg_catalog.=) v_schema AND NOT r.rolsuper;
    IF v_owner IS NOT NULL THEN
        RAISE EXCEPTION 'the schema % already exists and is owned by %, which is not a superuser', v_schema, v_owner
            USING ERRCODE = 'insufficient_privilege',
                  HINT = 'Drop the schema, or make a superuser its owner, before CREATE EXTENSION or ALTER EXTENSION ... UPDATE.';
    END IF;
    IF EXISTS (SELECT 1
                 FROM pg_catalog.pg_namespace n,
                      LATERAL pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
                WHERE n.nspname OPERATOR(pg_catalog.=) v_schema
                  AND a.privilege_type OPERATOR(pg_catalog.=) 'CREATE'
                  AND (a.grantee OPERATOR(pg_catalog.=) 0
                       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles g WHERE g.oid OPERATOR(pg_catalog.=) a.grantee AND g.rolsuper))) THEN
        RAISE EXCEPTION 'the schema % grants CREATE to a role that is not a superuser (or to PUBLIC)', v_schema
            USING ERRCODE = 'insufficient_privilege',
                  HINT = 'Revoke CREATE on the schema before CREATE EXTENSION or ALTER EXTENSION ... UPDATE.';
    END IF;
END
$kgchk$;

-- The schema is created here, not by CREATE EXTENSION, so that it is a member
-- of the extension and disappears with it.  Every object is qualified.
-- Not CREATE SCHEMA IF NOT EXISTS: PostgreSQL refuses that in a script when the schema
-- exists and is not a member of the extension, and another KingresDB extension may have made it.
DO $ks$ BEGIN IF pg_catalog.to_regnamespace('kingres') IS NULL THEN CREATE SCHEMA kingres; END IF; END $ks$;

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
