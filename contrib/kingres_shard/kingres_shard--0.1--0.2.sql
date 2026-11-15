-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_shard--0.1--0.2.sql
--   Upgrade of the kingres_shard extension from version 0.1 to 0.2: reference
--   tables, the commit-decision log of two-phase commit and its recovery, and
--   the guard that lets propagated DDL drop distributed tables.  The result is
--   identical to installing kingres_shard--0.2.sql (the "upgrade" regression
--   test compares the two).  Design: kingres/docs/design/shard-phase2a.md.
--
-- The text of the changed functions below is copied from the install script by
-- the same hand that changes either file; the test fails if they drift.

\echo Use "ALTER EXTENSION kingres_shard UPDATE TO '0.2'" to load this file. \quit

-- dist_table: the method 'reference' has neither a distribution column nor a
-- colocation group.
ALTER TABLE kingres.dist_table DROP CONSTRAINT dist_table_method_check;
ALTER TABLE kingres.dist_table ALTER COLUMN dist_column DROP NOT NULL;
ALTER TABLE kingres.dist_table ALTER COLUMN colocation_id DROP NOT NULL;
ALTER TABLE kingres.dist_table
    ADD CONSTRAINT dist_table_method_check CHECK (method IN ('hash', 'reference'));
ALTER TABLE kingres.dist_table
    ADD CONSTRAINT dist_table_shape_check CHECK (
        (method = 'hash' AND dist_column IS NOT NULL AND colocation_id IS NOT NULL)
        OR (method = 'reference' AND dist_column IS NULL AND colocation_id IS NULL));

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

-- Changed functions (same text as in the install script).

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

    -- Only the table itself (objsubid 0), not one of its columns, is gone.
    DELETE FROM kingres.dist_table d
     WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) ANY
           (SELECT o.objid
              FROM pg_catalog.pg_event_trigger_dropped_objects() o
             WHERE o.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
               AND o.objsubid OPERATOR(pg_catalog.=) 0);
END;
$$;

CREATE OR REPLACE FUNCTION kingres.internal_table_ddl(p_relid regclass, p_column name)
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

COMMENT ON FUNCTION kingres.sync_catalog() IS
    'Push the catalog of the map group primary to every active node, atomically (two-phase commit)';

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
REVOKE ALL ON FUNCTION
    kingres.internal_check_referenceable(regclass),
    kingres.create_reference_table(regclass),
    kingres.recover_prepared_transactions(),
    kingres.internal_scrub_conn_error(text)
    FROM PUBLIC;

-- The cluster secret: a node of the map group makes it when it is upgraded (the
-- others receive it from sync_catalog(), see the README).
CREATE OR REPLACE FUNCTION kingres.init_cluster(host text, port int)
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

CREATE OR REPLACE FUNCTION kingres.internal_reset_node()
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

SELECT kingres.internal_ensure_secret()
 WHERE EXISTS (SELECT 1 FROM kingres.local_node l JOIN kingres.node n ON n.node_id = l.node_id
                 WHERE n.group_id = 0 AND n.role = 'primary');
