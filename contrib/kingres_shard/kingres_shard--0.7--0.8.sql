-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_shard--0.7--0.8.sql
--   Upgrade of the kingres_shard extension from version 0.7 to 0.8: elasticity (design/shard-phase10.md)
--   and distributed schemas (design/shard-phase11.md).  Two lines of work write this file, each in
--   its own section; keep to your section and do not reorder the sections:
--     * SECTION P10 (elasticity): the bucket layout (every bucket has its own modulus), the placement
--       epoch, and what the later increments of phase 10 add (split, weights and strategies, plans).
--     * SECTION P11 (distributed schemas): added by the phase 11 line of work, after the end of section P10.
--   A function that both lines change (kingres.internal_apply_catalog(), kingres.internal_catalog_snapshot(),
--   kingres.check_cluster()) is defined once, in the section of the line that wrote it first, and the other
--   line edits that definition in place; never create the same function a second time in this file.
--   The result is identical to installing kingres_shard--0.8.sql (the "upgrade" regression test compares
--   the two).  kingres_shard--0.8.sql is the 0.7 install script with this script appended; regenerate it when
--   this file changes.

\echo Use "ALTER EXTENSION kingres_shard UPDATE TO '0.8'" to load this file. \quit

-- ---------------------------------------------------------------------------
-- The version handshake
-- ---------------------------------------------------------------------------

-- internal_check_node_versions(): asks for 0.8.
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

-- ===========================================================================
-- SECTION P10: elasticity (design/shard-phase10.md)
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- P10.1: the bucket layout.  A bucket is a hash partition (MODULUS m, REMAINDER bucket); until now every
-- bucket of a group had the modulus bucket_count.  From 0.8 each bucket has its own modulus, so that one
-- bucket can be split into several without touching the others (design/shard-phase10.md, sections 1.1 to 1.3):
-- the buckets of a group cover each slot 0 .. max_modulus - 1 of hash % max_modulus exactly once.
-- A group that was never split has max_modulus = bucket_count and every modulus = bucket_count, so nothing
-- changes for it.  Nothing in this version splits a bucket; the catalog and the code are ready for it.
-- ---------------------------------------------------------------------------

-- The greatest modulus of a bucket of the group, and the counter of the group's switches (a move or a split
-- raises it in the transaction that changes the partitions: see the placement hash, review finding S4-01).
ALTER TABLE kingres.colocation
    ADD COLUMN max_modulus int,
    ADD COLUMN placement_epoch bigint NOT NULL DEFAULT 0;
-- The two updates fill the new columns of the rows that exist; they must not count the map version up (an install
-- has no rows, an upgrade changes no placement), so the statement triggers are off while they run.
ALTER TABLE kingres.colocation DISABLE TRIGGER colocation_map_version;
UPDATE kingres.colocation SET max_modulus = bucket_count;
ALTER TABLE kingres.colocation ENABLE TRIGGER colocation_map_version;
ALTER TABLE kingres.colocation
    ALTER COLUMN max_modulus SET NOT NULL,
    ADD CONSTRAINT colocation_max_modulus_check CHECK (max_modulus BETWEEN 1 AND 65536);

ALTER TABLE kingres.bucket_map
    ADD COLUMN modulus int;
ALTER TABLE kingres.bucket_map DISABLE TRIGGER bucket_map_map_version;
UPDATE kingres.bucket_map m SET modulus = c.bucket_count
  FROM kingres.colocation c WHERE c.colocation_id = m.colocation_id;
ALTER TABLE kingres.bucket_map ENABLE TRIGGER bucket_map_map_version;
ALTER TABLE kingres.bucket_map
    ALTER COLUMN modulus SET NOT NULL,
    ADD CONSTRAINT bucket_map_modulus_check CHECK (modulus >= 1 AND bucket < modulus);

-- A row that does not say its modulus (an insert of the older form, or a catalog document of a map group
-- that is not upgraded yet) gets the layout of a group that was never split.
CREATE FUNCTION kingres.internal_layout_defaults()
    RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    IF TG_TABLE_NAME OPERATOR(pg_catalog.=) 'colocation' THEN
        NEW.max_modulus := COALESCE(NEW.max_modulus, NEW.bucket_count);
        NEW.placement_epoch := COALESCE(NEW.placement_epoch, 0);
    ELSE
        NEW.modulus := COALESCE(NEW.modulus,
                                (SELECT c.bucket_count FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) NEW.colocation_id),
                                -- a row of a group that does not exist is refused by the foreign key, not by this default
                                (CASE WHEN NEW.bucket OPERATOR(pg_catalog.>) 0 THEN NEW.bucket ELSE 0 END) OPERATOR(pg_catalog.+) 1);
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_layout_defaults() FROM PUBLIC;

CREATE TRIGGER colocation_layout_defaults BEFORE INSERT ON kingres.colocation
    FOR EACH ROW EXECUTE FUNCTION kingres.internal_layout_defaults();
CREATE TRIGGER bucket_map_layout_defaults BEFORE INSERT ON kingres.bucket_map
    FOR EACH ROW EXECUTE FUNCTION kingres.internal_layout_defaults();

-- The modulus is part of the bucket (a view can only gain a column at its end).
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
           END AS partition_size,
           b.modulus
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

-- The modulus and the remainder of a bucket: the partition is FOR VALUES WITH (MODULUS modulus, REMAINDER remainder).
-- No row for a bucket that does not exist.
CREATE FUNCTION kingres.bucket_range(colocation_id int, bucket int)
    RETURNS TABLE (modulus int, remainder int)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT m.modulus, m.bucket FROM kingres.bucket_map m
 WHERE m.colocation_id OPERATOR(pg_catalog.=) bucket_range.colocation_id AND m.bucket OPERATOR(pg_catalog.=) bucket_range.bucket;
$$;
COMMENT ON FUNCTION kingres.bucket_range(int, int) IS
    'The modulus and the remainder of a bucket: its partition is FOR VALUES WITH (MODULUS modulus, REMAINDER remainder)';

-- check_cluster(): the layout.  bucket_map_incomplete is the old finding for a group that was never split; bucket_layout is
-- the finding for any other flaw: the buckets of a group do not cover each slot of hash % max_modulus exactly once, or a
-- partition on a node has other bounds than the row of the bucket map.
CREATE FUNCTION kingres.internal_check_layout()
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    x      pg_catalog.record;
    v_self pg_catalog.int4;
BEGIN
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    FOR x IN
        SELECT c.colocation_id, c.bucket_count, c.max_modulus,
               s.have, s.bad, s.total, s.covered, s.other_modulus
          FROM kingres.colocation c
          CROSS JOIN LATERAL
               (SELECT pg_catalog.count(*) AS have,
                       pg_catalog.count(*) FILTER (WHERE m.bucket OPERATOR(pg_catalog.>=) m.modulus
                                                      OR c.max_modulus OPERATOR(pg_catalog.%) m.modulus OPERATOR(pg_catalog.<>) 0) AS bad,
                       COALESCE(pg_catalog.sum(c.max_modulus OPERATOR(pg_catalog./) m.modulus) FILTER (WHERE m.bucket OPERATOR(pg_catalog.<) m.modulus
                                                      AND c.max_modulus OPERATOR(pg_catalog.%) m.modulus OPERATOR(pg_catalog.=) 0), 0) AS total,
                       (SELECT pg_catalog.count(DISTINCT g) FROM kingres.bucket_map b2
                         CROSS JOIN LATERAL pg_catalog.generate_series(b2.bucket, c.max_modulus OPERATOR(pg_catalog.-) 1, b2.modulus) g
                         WHERE b2.colocation_id OPERATOR(pg_catalog.=) c.colocation_id AND b2.bucket OPERATOR(pg_catalog.<) b2.modulus
                           AND c.max_modulus OPERATOR(pg_catalog.%) b2.modulus OPERATOR(pg_catalog.=) 0) AS covered,
                       pg_catalog.count(*) FILTER (WHERE m.modulus OPERATOR(pg_catalog.<>) c.bucket_count) AS other_modulus
                  FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) c.colocation_id) s
         ORDER BY c.colocation_id
    LOOP
        IF x.max_modulus OPERATOR(pg_catalog.=) x.bucket_count AND x.other_modulus OPERATOR(pg_catalog.=) 0 THEN
            IF x.have OPERATOR(pg_catalog.<>) x.bucket_count THEN
                check_name := 'bucket_map_incomplete'; node_id := v_self;
                object := pg_catalog.format('colocation group %s', x.colocation_id);
                detail := pg_catalog.format('%s of %s buckets are mapped', x.have, x.bucket_count);
                RETURN NEXT;
            END IF;
        ELSIF x.bad OPERATOR(pg_catalog.>) 0 OR x.total OPERATOR(pg_catalog.<>) x.max_modulus OR x.covered OPERATOR(pg_catalog.<>) x.max_modulus THEN
            check_name := 'bucket_layout'; node_id := v_self;
            object := pg_catalog.format('colocation group %s', x.colocation_id);
            detail := pg_catalog.format('the %s buckets do not cover each of the %s slots of the group exactly once (%s buckets with a bad modulus, %s slots covered by the sum of the buckets, %s different slots covered)',
                                        x.have, x.max_modulus, x.bad, x.total, x.covered);
            RETURN NEXT;
        END IF;
    END LOOP;
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_check_layout() FROM PUBLIC;

-- The bounds of every bucket partition on every node against the bucket map.
CREATE FUNCTION kingres.internal_check_layout_partitions()
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    n pg_catalog.record;
    r pg_catalog.jsonb;
    e pg_catalog.jsonb;
    v_bucket pg_catalog.int4;
    v_want   pg_catalog.text;
    v_group  pg_catalog.int4;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.method OPERATOR(pg_catalog.=) 'hash') THEN
        RETURN;
    END IF;
    FOR n IN SELECT nd.node_id AS id, nd.group_id AS grp FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active ORDER BY nd.node_id LOOP
        BEGIN
            r := kingres.internal_node_exec(n.id,
                'SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_array(d.relid::pg_catalog.regclass::pg_catalog.text, c.relname::pg_catalog.text, pg_catalog.pg_get_expr(c.relpartbound, c.oid))), ''[]''::pg_catalog.jsonb)::pg_catalog.text '
                'FROM kingres.dist_table d JOIN pg_catalog.pg_inherits i ON i.inhparent OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid '
                'JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.inhrelid '
                'WHERE d.method OPERATOR(pg_catalog.=) ''hash'' AND c.relispartition AND c.relname OPERATOR(pg_catalog.~) ''_b[0-9]+$''');
            r := (((r OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0))::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            CONTINUE;
        END;
        FOR e IN SELECT x FROM pg_catalog.jsonb_array_elements(r) x LOOP
            v_bucket := (pg_catalog.regexp_match(e OPERATOR(pg_catalog.->>) 1, '_b([0-9]+)$'))[1]::pg_catalog.int4;
            SELECT pg_catalog.format('FOR VALUES WITH (modulus %s, remainder %s)', m.modulus, m.bucket), m.group_id
              INTO v_want, v_group
              FROM kingres.bucket_map m
              JOIN kingres.dist_table d ON d.colocation_id OPERATOR(pg_catalog.=) m.colocation_id
             WHERE d.relid OPERATOR(pg_catalog.=) (e OPERATOR(pg_catalog.->>) 0)::pg_catalog.regclass AND m.bucket OPERATOR(pg_catalog.=) v_bucket;
            -- a partition of a bucket that the group of the node does not own is partition_misplaced, said by the base check
            IF FOUND AND v_group OPERATOR(pg_catalog.=) n.grp AND (e OPERATOR(pg_catalog.->>) 2) IS DISTINCT FROM v_want THEN
                check_name := 'bucket_layout'; node_id := n.id; object := (e OPERATOR(pg_catalog.->>) 1);
                detail := pg_catalog.format('the partition has the bounds "%s" but the bucket map says "%s"', e OPERATOR(pg_catalog.->>) 2, v_want);
                RETURN NEXT;
            END IF;
        END LOOP;
    END LOOP;
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_check_layout_partitions() FROM PUBLIC;

CREATE OR REPLACE FUNCTION kingres.check_cluster(p_prepared_older_than interval DEFAULT interval '5 minutes')
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    -- the base check counts the buckets of a group against its bucket_count; the layout check does it for groups with split buckets too
    RETURN QUERY SELECT b.check_name, b.node_id, b.object, b.detail FROM kingres.internal_check_cluster_base(p_prepared_older_than) b
                  WHERE b.check_name OPERATOR(pg_catalog.<>) 'bucket_map_incomplete';
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_layout() t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_layout_partitions() t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_splits() t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_table_operations() t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_partitions() t;
END;
$$;

-- The functions that counted buckets with bucket_count, or used it as the modulus of every bucket: they take the number of buckets and the
-- modulus of the bucket from the bucket map.


CREATE OR REPLACE FUNCTION kingres.move_bucket(colocation_id int, bucket int, target_group int)
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
    v_modulus  pg_catalog.int4;
    v_tables   pg_catalog.jsonb;
    v_info     pg_catalog.jsonb;
    v_id       pg_catalog.int8;
    v_name     pg_catalog.text;
    v_stage    pg_catalog.int4 := 0;	-- 1: recorded, 2: switching, 3: switched
    v_thr      pg_catalog.int8;
    v_stall    pg_catalog.timestamptz;
    v_prefixes pg_catalog.jsonb;
    v_sw_ms    pg_catalog.int4;
    v_delay    pg_catalog.int4;
    v_nt       pg_catalog.int4;
    v_node_ms  pg_catalog.int4;
    v_srcrow   pg_catalog.record;
    v_tgtrow   pg_catalog.record;
    v_maj_src  pg_catalog.text;
    v_maj_tgt  pg_catalog.text;
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
    v_max_attempts pg_catalog.int4;
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
    v_max_attempts := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.move_switch_attempts');

    -- Read-only checks first: a move that cannot start leaves nothing behind.
    PERFORM kingres.internal_check_node_versions();
    SELECT (SELECT pg_catalog.count(*) FROM kingres.bucket_map x WHERE x.colocation_id OPERATOR(pg_catalog.=) c.colocation_id)::pg_catalog.int4 INTO v_count
      FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_colo;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'colocation group % does not exist', v_colo USING ERRCODE = 'undefined_object';
    END IF;
    -- the modulus of the bucket: the bucket count of a group that was never split, a modulus of its own after a split
    SELECT m.group_id, m.modulus INTO v_source, v_modulus FROM kingres.bucket_map m
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

    -- The rows travel by logical replication between the two nodes, and a cluster runs one
    -- PostgreSQL major.
    v_maj_src := kingres.internal_exec_scalar(v_src, 'SELECT (pg_catalog.current_setting(''server_version_num'')::pg_catalog.int4 OPERATOR(pg_catalog./) 10000)::pg_catalog.text');
    v_maj_tgt := kingres.internal_exec_scalar(v_tgt, 'SELECT (pg_catalog.current_setting(''server_version_num'')::pg_catalog.int4 OPERATOR(pg_catalog./) 10000)::pg_catalog.text');
    IF v_maj_src IS DISTINCT FROM v_maj_tgt THEN
        RAISE EXCEPTION 'bucket % of colocation group % cannot move from node % (%:%) to node % (%:%): they run different PostgreSQL major versions (% and %)',
                        v_bucket, v_colo, v_src, v_srcrow.host, v_srcrow.port, v_tgt, v_tgtrow.host, v_tgtrow.port, v_maj_src, v_maj_tgt
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'All nodes of a cluster must run the same major version of PostgreSQL.';
    END IF;

    v_info := (kingres.internal_exec_scalar(v_src, pg_catalog.format(
                  'SELECT kingres.internal_move_inspect(''source'', %s, %s, %L::pg_catalog.jsonb)::pg_catalog.text',
                  v_bucket, v_modulus, v_tables::pg_catalog.text)))::pg_catalog.jsonb;
    PERFORM kingres.internal_exec_scalar(v_tgt, pg_catalog.format(
                  'SELECT kingres.internal_move_inspect(''target'', %s, %s, %L::pg_catalog.jsonb)::pg_catalog.text',
                  v_bucket, v_modulus, v_tables::pg_catalog.text));
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
            'SELECT kingres.internal_create_move_tables(%s, %s, %s, %L::pg_catalog.jsonb)', v_id, v_bucket, v_modulus, v_info::pg_catalog.text));
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
                    -- The copy needs the source; a source that is gone must fail the move now, not
                    -- leave it waiting for a copy that cannot finish (the other branch talks to the
                    -- source at every round, this one did not).
                    PERFORM kingres.internal_exec_scalar(v_src, 'SELECT 1');
                    -- A table whose copy is finished and whose slot is gone from the source can
                    -- never finish: fail the move (it is undone) once that has lasted long enough
                    -- for a worker that is dropping its slot in the ordinary way to be done.
                    v_prefixes := kingres.internal_exec_scalar(v_tgt, pg_catalog.format(
                                      'SELECT kingres.internal_move_sync_prefixes(%L)::pg_catalog.text', v_name))::pg_catalog.jsonb;
                    IF v_prefixes <> '[]'::pg_catalog.jsonb
                       AND kingres.internal_exec_scalar(v_src, pg_catalog.format(
                               'SELECT kingres.internal_move_slots_missing(%L::pg_catalog.jsonb)', v_prefixes::pg_catalog.text))::pg_catalog.int4 > 0 THEN
                        v_stall := COALESCE(v_stall, pg_catalog.clock_timestamp());
                        IF pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) v_stall > pg_catalog.make_interval(secs => GREATEST(10, v_node_ms / 500)) THEN
                            RAISE EXCEPTION 'the table synchronization of bucket % on node % lost its replication slot on node %', v_bucket, v_tgt, v_src
                                USING ERRCODE = 'object_not_in_prerequisite_state',
                                      HINT = 'The target stopped while a table synchronization worker was finishing; the move is undone and can be repeated.';
                        END IF;
                    ELSE
                        v_stall := NULL;
                    END IF;
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
            -- A cancel that arrived since the last look: the switch was not entered (S5-05).
            SELECT b.phase INTO v_phase FROM kingres.bucket_move b WHERE b.move_id OPERATOR(pg_catalog.=) v_id;
            IF v_phase OPERATOR(pg_catalog.<>) 'switching' THEN
                RAISE EXCEPTION 'bucket move % was cancelled', v_id USING ERRCODE = 'KS003';
            END IF;
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
                    -- A transaction that holds the tables for ever must not keep the move trying for ever
                    -- (review finding S4-04): after the limit the move fails, with the reason and the
                    -- sessions in the way in its record, and can be started again.
                    IF v_attempts OPERATOR(pg_catalog.>=) v_max_attempts THEN
                        RAISE EXCEPTION 'bucket move % gave up after % attempts to switch: %', v_id, v_attempts, v_msg
                            USING ERRCODE = 'lock_not_available';
                    END IF;
                    -- Back off: 0.2 s, 0.4 s, ... up to 10 s, and a little at random.
                    PERFORM pg_catalog.pg_sleep(LEAST(10.0, 0.2 OPERATOR(pg_catalog.*) pg_catalog.power(2.0, v_attempts OPERATOR(pg_catalog.-) 1))
                                                OPERATOR(pg_catalog.+) pg_catalog.random() OPERATOR(pg_catalog.*) 0.3);
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


CREATE OR REPLACE FUNCTION kingres.rebalance_plan(max_moves int DEFAULT NULL)
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
        SELECT c.colocation_id,
               -- the number of buckets: after a split it is more than the base bucket_count
               (SELECT pg_catalog.count(*) FROM kingres.bucket_map x WHERE x.colocation_id OPERATOR(pg_catalog.=) c.colocation_id)::pg_catalog.int4 AS bucket_count
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


CREATE OR REPLACE FUNCTION kingres.internal_new_colocation(p_type regtype, p_collation oid, p_buckets int, p_like int DEFAULT NULL)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_id     pg_catalog.int4 := pg_catalog.nextval('kingres.colocation_id_seq')::pg_catalog.int4;
    v_old    pg_catalog.int4;
    v_oldmax pg_catalog.int4;
    v_groups pg_catalog.int4[];
BEGIN
    IF p_like IS NOT NULL THEN
        SELECT c.bucket_count, c.max_modulus INTO v_old, v_oldmax FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) p_like;
    END IF;
    IF v_old IS NOT NULL AND p_buckets OPERATOR(pg_catalog.=) v_old AND v_oldmax OPERATOR(pg_catalog.<>) v_old THEN
        -- The group it is like has split buckets: the new group has the same buckets, with the same moduli and owners.
        INSERT INTO kingres.colocation (colocation_id, bucket_count, dist_column_type, dist_collation, max_modulus)
        VALUES (v_id, p_buckets, p_type, p_collation, v_oldmax);
        INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id, modulus)
        SELECT v_id, m.bucket, m.group_id, m.modulus FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) p_like;
        RETURN v_id;
    END IF;
    INSERT INTO kingres.colocation (colocation_id, bucket_count, dist_column_type, dist_collation) VALUES (v_id, p_buckets, p_type, p_collation);
    -- The placement of a group that was never split is copied bucket by bucket (the new count is a multiple of the old one);
    -- a group that has split buckets has no bucket g % count, so the placement is made as for a group of its own.
    IF v_old IS NOT NULL AND v_oldmax OPERATOR(pg_catalog.=) v_old AND p_buckets OPERATOR(pg_catalog.%) v_old OPERATOR(pg_catalog.=) 0 THEN
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

-- ---------------------------------------------------------------------------
-- P10.3: capacity weights, the plan of a rebalance with strategies (design/shard-phase10.md, sections 4 and 5).
-- ---------------------------------------------------------------------------

-- The capacity of the group whose primary this node is.  The control plane keeps the weight per server and writes it to every node
-- of the server, as it does should_have_shards.
ALTER TABLE kingres.node
    ADD COLUMN weight float8 NOT NULL DEFAULT 1,
    ADD CONSTRAINT node_weight_check CHECK (weight > 0 AND weight <= 1000);

-- A node row that does not say its weight (a catalog document of a map group that is not upgraded yet) has the weight 1.
CREATE OR REPLACE FUNCTION kingres.internal_layout_defaults()
    RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    IF TG_TABLE_NAME OPERATOR(pg_catalog.=) 'colocation' THEN
        NEW.max_modulus := COALESCE(NEW.max_modulus, NEW.bucket_count);
        NEW.placement_epoch := COALESCE(NEW.placement_epoch, 0);
    ELSIF TG_TABLE_NAME OPERATOR(pg_catalog.=) 'node' THEN
        NEW.weight := COALESCE(NEW.weight, 1);
    ELSE
        NEW.modulus := COALESCE(NEW.modulus,
                                (SELECT c.bucket_count FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) NEW.colocation_id),
                                -- a row of a group that does not exist is refused by the foreign key, not by this default
                                (CASE WHEN NEW.bucket OPERATOR(pg_catalog.>) 0 THEN NEW.bucket ELSE 0 END) OPERATOR(pg_catalog.+) 1);
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER node_layout_defaults BEFORE INSERT ON kingres.node
    FOR EACH ROW EXECUTE FUNCTION kingres.internal_layout_defaults();

-- The smallest cost of a bucket by_disk_size counts (an empty bucket is not free).
-- (the setting kingres_shard.min_bucket_cost is defined by the library)

-- kingres.set_node_property(node, should_have_shards, weight): NULL leaves a property as it is.  The two-argument form stays (the control plane
-- asks for it by its signature).
CREATE FUNCTION kingres.set_node_property(node_id int, should_have_shards bool, weight float8)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_node pg_catalog.int4 := node_id;
    v_flag pg_catalog.bool := should_have_shards;
    v_w    pg_catalog.float8 := weight;
BEGIN
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    IF v_flag IS NULL AND v_w IS NULL THEN
        RAISE EXCEPTION 'set_node_property needs should_have_shards or weight' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF v_w IS NOT NULL AND (v_w OPERATOR(pg_catalog.<=) 0 OR v_w OPERATOR(pg_catalog.>) 1000 OR v_w IS NOT DISTINCT FROM 'NaN'::pg_catalog.float8) THEN
        RAISE EXCEPTION 'the weight must be above 0 and at most 1000, not %', v_w USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE kingres.node n
       SET should_have_shards = COALESCE(v_flag, n.should_have_shards), weight = COALESCE(v_w, n.weight)
     WHERE n.node_id OPERATOR(pg_catalog.=) v_node;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'node % does not exist', v_node USING ERRCODE = 'undefined_object';
    END IF;
    PERFORM kingres.sync_catalog();
END;
$$;
COMMENT ON FUNCTION kingres.set_node_property(int, bool, float8) IS
    'Change whether a node accepts buckets and the weight of its group in a rebalance (NULL leaves a property as it is); map group primary only; atomic';
REVOKE ALL ON FUNCTION kingres.set_node_property(int, bool, float8) FROM PUBLIC;

-- What a plan reads, as one document: {"groups": [...], "items": [...]}.  A group is every shard group that owns a bucket or has an
-- active primary; "avail" says it has an active primary, "recv" that it may receive buckets (avail, should_have_shards, not excluded),
-- "w" is the weight of its primary, "extra" the load of other databases for a receiver.  An item is one bucket of the selected
-- colocation groups' map: "owner" is where it is going to be (the target of a move that is running), "phys" where its partitions are now,
-- "mov" whether a plan may move it (active, not moving, of a selected colocation group), "cost" what it counts for in the strategy and
-- "size" the bytes of its partitions (by_disk_size).
CREATE FUNCTION kingres.internal_plan_inputs(p_strategy text, p_extra jsonb, p_exclude int[], p_colos int[])
    RETURNS jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_groups pg_catalog.jsonb;
    v_items  pg_catalog.jsonb;
    v_min    pg_catalog.int8;
    v_sizes  pg_catalog.jsonb := '{}';
    g        pg_catalog.record;
    v_node   pg_catalog.int4;
BEGIN
    IF p_strategy IS NULL OR p_strategy NOT IN ('by_bucket_count', 'by_disk_size') THEN
        RAISE EXCEPTION 'unknown rebalance strategy "%" (by_bucket_count or by_disk_size)', p_strategy
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_extra IS NOT NULL AND pg_catalog.jsonb_typeof(p_extra) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'extra_load must be a JSON object of group id to load' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    v_min := (SELECT s.setting::pg_catalog.int8 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.min_bucket_cost') * 1024;

    SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
               'id', x.group_id, 'avail', x.node_id IS NOT NULL,
               'recv', x.node_id IS NOT NULL AND x.should AND NOT (x.group_id OPERATOR(pg_catalog.=) ANY (COALESCE(p_exclude, '{}'::pg_catalog.int4[]))),
               'w', COALESCE(x.weight, 1),
               'extra', CASE WHEN x.node_id IS NOT NULL AND x.should AND NOT (x.group_id OPERATOR(pg_catalog.=) ANY (COALESCE(p_exclude, '{}'::pg_catalog.int4[])))
                             THEN COALESCE((p_extra OPERATOR(pg_catalog.->>) x.group_id::pg_catalog.text)::pg_catalog.float8, 0) ELSE 0 END)
               ORDER BY x.group_id), '[]'::pg_catalog.jsonb)
      INTO v_groups
      FROM (SELECT gr.group_id,
                   (SELECT n.node_id FROM kingres.node n WHERE n.group_id OPERATOR(pg_catalog.=) gr.group_id AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active) AS node_id,
                   COALESCE((SELECT n.should_have_shards FROM kingres.node n WHERE n.group_id OPERATOR(pg_catalog.=) gr.group_id AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active), false) AS should,
                   (SELECT n.weight FROM kingres.node n WHERE n.group_id OPERATOR(pg_catalog.=) gr.group_id AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active) AS weight
              FROM (SELECT n.group_id FROM kingres.node n UNION SELECT m.group_id FROM kingres.bucket_map m) gr) x;

    IF p_strategy OPERATOR(pg_catalog.=) 'by_disk_size' THEN
        PERFORM kingres.internal_require_superuser();
        -- the sizes of the bucket partitions of every table of every colocation group, from the node that owns them now
        FOR g IN SELECT DISTINCT m.group_id FROM kingres.bucket_map m ORDER BY m.group_id LOOP
            v_node := kingres.internal_group_node(g.group_id);
            IF v_node IS NULL THEN
                CONTINUE;           -- a group without a primary is not planned
            END IF;
            BEGIN
                v_sizes := v_sizes OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object(g.group_id::pg_catalog.text,
                    (kingres.internal_node_exec(v_node,
                        'SELECT COALESCE(pg_catalog.jsonb_object_agg(s.k, s.sz), ''{}''::pg_catalog.jsonb)::pg_catalog.text FROM ('
                        'SELECT d.colocation_id::pg_catalog.text OPERATOR(pg_catalog.||) '':'' OPERATOR(pg_catalog.||) b.bucket::pg_catalog.text AS k, '
                        'pg_catalog.sum(b.partition_size)::pg_catalog.int8 AS sz '
                        'FROM kingres.buckets b JOIN kingres.dist_table d ON d.relid OPERATOR(pg_catalog.=) b.table_name '
                        'WHERE b.partition_size IS NOT NULL GROUP BY 1) s',
                        10000) OPERATOR(pg_catalog.->) 0 OPERATOR(pg_catalog.->>) 0)::pg_catalog.jsonb);
            EXCEPTION WHEN OTHERS THEN
                RAISE EXCEPTION 'the sizes of the buckets of shard group % cannot be read (%): a plan by disk size does not guess', g.group_id, SQLERRM
                    USING ERRCODE = 'object_not_in_prerequisite_state';
            END;
        END LOOP;
    END IF;

    SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
               'c', q.colocation_id, 'b', q.bucket, 'owner', q.owner, 'phys', q.group_id,
               'mov', q.mov, 'size', q.size,
               'cost', CASE WHEN p_strategy OPERATOR(pg_catalog.=) 'by_disk_size' THEN (CASE WHEN COALESCE(q.size, 0) OPERATOR(pg_catalog.>) v_min THEN q.size ELSE v_min END) ELSE 1 END)
               ORDER BY q.colocation_id, q.bucket), '[]'::pg_catalog.jsonb)
      INTO v_items
      FROM (SELECT m.colocation_id, m.bucket, m.group_id,
                   COALESCE((SELECT b.target_group FROM kingres.bucket_move b
                              WHERE b.colocation_id OPERATOR(pg_catalog.=) m.colocation_id AND b.bucket OPERATOR(pg_catalog.=) m.bucket
                                AND b.phase IN ('preparing', 'copying', 'catching_up', 'switching')), m.group_id) AS owner,
                   m.state OPERATOR(pg_catalog.=) 'active' AND (p_colos IS NULL OR m.colocation_id OPERATOR(pg_catalog.=) ANY (p_colos)) AS mov,
                   (v_sizes OPERATOR(pg_catalog.->) m.group_id::pg_catalog.text OPERATOR(pg_catalog.->>) (m.colocation_id::pg_catalog.text OPERATOR(pg_catalog.||) ':' OPERATOR(pg_catalog.||) m.bucket::pg_catalog.text))::pg_catalog.int8 AS size
              FROM kingres.bucket_map m) q;
    RETURN pg_catalog.jsonb_build_object('groups', v_groups, 'items', v_items);
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_plan_inputs(text, jsonb, int[], int[]) FROM PUBLIC;

-- Where the groups stand: the load of every group, its utilization (load plus the load of other databases, per unit of weight)
-- and how far a receiver is from the ideal (design section 4.4).  The ideal counts the load of the groups that are drained as work
-- to be distributed.  "available" says the group has an active primary; a group without one is neither a receiver nor a source.
CREATE FUNCTION kingres.group_load(strategy text DEFAULT 'by_bucket_count', extra_load jsonb DEFAULT NULL, exclude_groups int[] DEFAULT NULL)
    RETURNS TABLE (group_id int, receiving bool, weight float8, buckets int, load bigint, utilization float8, deviation float8, available bool)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_in    pg_catalog.jsonb := kingres.internal_plan_inputs(strategy, extra_load, exclude_groups, NULL);
    v_total pg_catalog.float8;
    v_wsum  pg_catalog.float8;
    v_ideal pg_catalog.float8;
BEGIN
    SELECT COALESCE(pg_catalog.sum(i.cost) FILTER (WHERE gr.avail), 0)::pg_catalog.float8 OPERATOR(pg_catalog.+)
           COALESCE((SELECT pg_catalog.sum(x.extra) FROM pg_catalog.jsonb_to_recordset(v_in OPERATOR(pg_catalog.->) 'groups') AS x(recv bool, extra float8) WHERE x.recv), 0)
      INTO v_total
      FROM pg_catalog.jsonb_to_recordset(v_in OPERATOR(pg_catalog.->) 'items') AS i(owner int, cost int8)
      JOIN pg_catalog.jsonb_to_recordset(v_in OPERATOR(pg_catalog.->) 'groups') AS gr(id int, avail bool) ON gr.id OPERATOR(pg_catalog.=) i.owner;
    SELECT COALESCE(pg_catalog.sum(x.w), 0) INTO v_wsum
      FROM pg_catalog.jsonb_to_recordset(v_in OPERATOR(pg_catalog.->) 'groups') AS x(recv bool, w float8) WHERE x.recv;
    v_ideal := CASE WHEN v_wsum OPERATOR(pg_catalog.>) 0 THEN v_total OPERATOR(pg_catalog./) v_wsum ELSE 0 END;
    RETURN QUERY
    SELECT gr.id, gr.recv, gr.w, COALESCE(l.n, 0)::pg_catalog.int4, COALESCE(l.load, 0)::pg_catalog.int8,
           CASE WHEN gr.recv THEN (COALESCE(l.load, 0) OPERATOR(pg_catalog.+) gr.extra) OPERATOR(pg_catalog./) gr.w END,
           CASE WHEN gr.recv AND v_ideal OPERATOR(pg_catalog.>) 0
                THEN pg_catalog.abs((COALESCE(l.load, 0) OPERATOR(pg_catalog.+) gr.extra) OPERATOR(pg_catalog./) gr.w OPERATOR(pg_catalog.-) v_ideal) OPERATOR(pg_catalog./) v_ideal
                WHEN gr.recv THEN 0 END,
           gr.avail
      FROM pg_catalog.jsonb_to_recordset(v_in OPERATOR(pg_catalog.->) 'groups') AS gr(id int, avail bool, recv bool, w float8, extra float8)
      LEFT JOIN (SELECT i.owner, pg_catalog.count(*) AS n, pg_catalog.sum(i.cost) AS load
                   FROM pg_catalog.jsonb_to_recordset(v_in OPERATOR(pg_catalog.->) 'items') AS i(owner int, cost int8) GROUP BY i.owner) l ON l.owner OPERATOR(pg_catalog.=) gr.id
     ORDER BY gr.id;
END;
$$;
COMMENT ON FUNCTION kingres.group_load(text, jsonb, int[]) IS
    'The load, utilization and deviation of every shard group under a rebalance strategy (by_bucket_count or by_disk_size); read only';

-- The plan of a rebalance, by simulation (design section 4.5).  Deterministic for a given state.  Moves are planned over the buckets of
-- every colocation group together (the load of a group is the sum over all colocation groups), a bucket is planned at most once, and a bucket
-- that is moving counts at its target.  A plan stops when every receiver is within the threshold (default 0 for by_bucket_count, 0.1 for
-- by_disk_size) and no group that may not hold shards still holds a bucket; with drain_only only the drain moves are planned.
CREATE FUNCTION kingres.internal_plan_simulate(v_in jsonb, strategy text, threshold float8, max_moves int, drain_only bool)
    RETURNS TABLE (seq int, colocation_id int, bucket int, source_group int, target_group int, cost bigint, size_bytes bigint, reason text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_thr    pg_catalog.float8 := threshold;
    v_max    pg_catalog.int4;
    g_id     pg_catalog.int4[];
    g_w      pg_catalog.float8[];
    g_extra  pg_catalog.float8[];
    g_load   pg_catalog.float8[];
    g_recv   pg_catalog.bool[];
    g_avail  pg_catalog.bool[];
    g_cand   pg_catalog.bool[];
    i_c      pg_catalog.int4[];
    i_b      pg_catalog.int4[];
    i_owner  pg_catalog.int4[];
    i_mov    pg_catalog.bool[];
    i_cost   pg_catalog.int8[];
    i_size   pg_catalog.int8[];
    ng       pg_catalog.int4;
    ni       pg_catalog.int4;
    k        pg_catalog.int4;
    j        pg_catalog.int4;
    s        pg_catalog.int4;
    t        pg_catalog.int4;
    best     pg_catalog.int4;
    v_seq    pg_catalog.int4 := 0;
    v_total  pg_catalog.float8;
    v_wsum   pg_catalog.float8;
    v_ideal  pg_catalog.float8;
    v_us     pg_catalog.float8;
    v_ut     pg_catalog.float8;
    v_drain  pg_catalog.bool;
    v_dev    pg_catalog.bool;
    v_found  pg_catalog.bool;
    v_u      pg_catalog.float8;
    v_best_u pg_catalog.float8;
    v_plan   pg_catalog.jsonb := '[]'::pg_catalog.jsonb;
BEGIN
    IF v_thr IS NULL THEN
        v_thr := CASE WHEN strategy OPERATOR(pg_catalog.=) 'by_disk_size' THEN 0.1 ELSE 0 END;
    END IF;
    IF v_thr OPERATOR(pg_catalog.<) 0 OR v_thr IS NOT DISTINCT FROM 'NaN'::pg_catalog.float8 THEN
        RAISE EXCEPTION 'the threshold must not be negative' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF max_moves IS NOT NULL AND max_moves OPERATOR(pg_catalog.<) 0 THEN
        RAISE EXCEPTION 'max_moves must not be negative' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT pg_catalog.array_agg(x.id ORDER BY x.id), pg_catalog.array_agg(x.w ORDER BY x.id), pg_catalog.array_agg(x.extra ORDER BY x.id),
           pg_catalog.array_agg(x.recv ORDER BY x.id), pg_catalog.array_agg(x.avail ORDER BY x.id)
      INTO g_id, g_w, g_extra, g_recv, g_avail
      FROM pg_catalog.jsonb_to_recordset(v_in OPERATOR(pg_catalog.->) 'groups') AS x(id int, avail bool, recv bool, w float8, extra float8);
    SELECT pg_catalog.array_agg(x.c ORDER BY x.ord), pg_catalog.array_agg(x.b ORDER BY x.ord), pg_catalog.array_agg(x.owner ORDER BY x.ord),
           pg_catalog.array_agg(x.mov ORDER BY x.ord), pg_catalog.array_agg(x.cost ORDER BY x.ord), pg_catalog.array_agg(x.size ORDER BY x.ord)
      INTO i_c, i_b, i_owner, i_mov, i_cost, i_size
      FROM ROWS FROM (pg_catalog.jsonb_to_recordset(v_in OPERATOR(pg_catalog.->) 'items') AS (c int, b int, owner int, mov bool, cost int8, size int8)) WITH ORDINALITY AS x(c, b, owner, mov, cost, size, ord);
    ng := COALESCE(pg_catalog.cardinality(g_id), 0);
    ni := COALESCE(pg_catalog.cardinality(i_c), 0);
    IF ng OPERATOR(pg_catalog.=) 0 OR ni OPERATOR(pg_catalog.=) 0 THEN
        RETURN;
    END IF;
    v_max := CASE WHEN COALESCE(max_moves, ni) OPERATOR(pg_catalog.<) ni THEN COALESCE(max_moves, ni) ELSE ni END;
    g_load := pg_catalog.array_fill(0::pg_catalog.float8, ARRAY[ng]);
    g_cand := pg_catalog.array_fill(true, ARRAY[ng]);
    FOR k IN 1 .. ni LOOP
        j := pg_catalog.array_position(g_id, i_owner[k]);
        IF j IS NOT NULL AND g_avail[j] THEN
            g_load[j] := g_load[j] OPERATOR(pg_catalog.+) i_cost[k];
        END IF;
    END LOOP;

    WHILE v_seq OPERATOR(pg_catalog.<) v_max LOOP
        v_total := 0; v_wsum := 0;
        FOR j IN 1 .. ng LOOP
            IF g_avail[j] THEN v_total := v_total OPERATOR(pg_catalog.+) g_load[j]; END IF;
            IF g_recv[j] THEN v_total := v_total OPERATOR(pg_catalog.+) g_extra[j]; v_wsum := v_wsum OPERATOR(pg_catalog.+) g_w[j]; END IF;
        END LOOP;
        -- the source: the drain source with the greatest load that still holds a bucket it may move
        s := NULL; v_drain := false;
        FOR j IN 1 .. ng LOOP
            IF g_avail[j] AND NOT g_recv[j] AND g_cand[j] AND EXISTS (SELECT 1 FROM pg_catalog.generate_series(1, ni) q WHERE i_owner[q] OPERATOR(pg_catalog.=) g_id[j] AND i_mov[q])
               AND (s IS NULL OR g_load[j] OPERATOR(pg_catalog.>) g_load[s]) THEN
                s := j; v_drain := true;
            END IF;
        END LOOP;
        IF v_wsum OPERATOR(pg_catalog.=) 0 THEN
            IF s IS NOT NULL THEN
                RAISE EXCEPTION 'no shard group can receive buckets' USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Set should_have_shards on a node with kingres.set_node_property().';
            END IF;
            EXIT;
        END IF;
        v_ideal := v_total OPERATOR(pg_catalog./) v_wsum;
        IF s IS NULL THEN
            EXIT WHEN drain_only;
            v_dev := false;
            FOR j IN 1 .. ng LOOP
                IF g_recv[j] AND v_ideal OPERATOR(pg_catalog.>) 0
                   AND pg_catalog.abs((g_load[j] OPERATOR(pg_catalog.+) g_extra[j]) OPERATOR(pg_catalog./) g_w[j] OPERATOR(pg_catalog.-) v_ideal) OPERATOR(pg_catalog./) v_ideal OPERATOR(pg_catalog.>) v_thr THEN
                    v_dev := true;
                END IF;
            END LOOP;
            EXIT WHEN NOT v_dev;
            -- the receiver with the greatest utilization that is still a candidate and holds a bucket it may move
            v_best_u := NULL;
            FOR j IN 1 .. ng LOOP
                IF g_recv[j] AND g_cand[j] AND EXISTS (SELECT 1 FROM pg_catalog.generate_series(1, ni) q WHERE i_owner[q] OPERATOR(pg_catalog.=) g_id[j] AND i_mov[q]) THEN
                    v_u := (g_load[j] OPERATOR(pg_catalog.+) g_extra[j]) OPERATOR(pg_catalog./) g_w[j];
                    IF v_best_u IS NULL OR v_u OPERATOR(pg_catalog.>) v_best_u THEN
                        s := j; v_best_u := v_u;
                    END IF;
                END IF;
            END LOOP;
            EXIT WHEN s IS NULL;
        END IF;
        -- the target: the receiver other than the source with the least utilization (ties: the lowest group id)
        t := NULL; v_best_u := NULL;
        FOR j IN 1 .. ng LOOP
            IF g_recv[j] AND j OPERATOR(pg_catalog.<>) s THEN
                v_u := (g_load[j] OPERATOR(pg_catalog.+) g_extra[j]) OPERATOR(pg_catalog./) g_w[j];
                IF v_best_u IS NULL OR v_u OPERATOR(pg_catalog.<) v_best_u THEN
                    t := j; v_best_u := v_u;
                END IF;
            END IF;
        END LOOP;
        IF t IS NULL THEN
            IF v_drain THEN
                RAISE EXCEPTION 'no shard group can receive buckets' USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Set should_have_shards on a node with kingres.set_node_property().';
            END IF;
            g_cand[s] := false;
            CONTINUE;
        END IF;
        -- the bucket: by decreasing cost (ties: the higher bucket number, then the higher colocation id); a bucket of a receiver must
        -- bring the two utilizations closer
        best := NULL;
        v_us := (g_load[s] OPERATOR(pg_catalog.+) g_extra[s]) OPERATOR(pg_catalog./) g_w[s];
        v_ut := (g_load[t] OPERATOR(pg_catalog.+) g_extra[t]) OPERATOR(pg_catalog./) g_w[t];
        FOR k IN 1 .. ni LOOP
            CONTINUE WHEN NOT i_mov[k] OR i_owner[k] OPERATOR(pg_catalog.<>) g_id[s];
            IF NOT v_drain AND NOT (pg_catalog.abs((v_us OPERATOR(pg_catalog.-) (i_cost[k] OPERATOR(pg_catalog./) g_w[s])) OPERATOR(pg_catalog.-) (v_ut OPERATOR(pg_catalog.+) (i_cost[k] OPERATOR(pg_catalog./) g_w[t])))
                                    OPERATOR(pg_catalog.<) pg_catalog.abs(v_us OPERATOR(pg_catalog.-) v_ut)) THEN
                CONTINUE;
            END IF;
            IF best IS NULL OR (i_cost[k], i_b[k], i_c[k]) OPERATOR(pg_catalog.>) (i_cost[best], i_b[best], i_c[best]) THEN
                best := k;
            END IF;
        END LOOP;
        IF best IS NULL THEN
            g_cand[s] := false;
            CONTINUE;
        END IF;
        v_seq := v_seq OPERATOR(pg_catalog.+) 1;
        v_plan := v_plan OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object(
                      'seq', v_seq, 'c', i_c[best], 'b', i_b[best], 'source', g_id[s], 'target', g_id[t], 'cost', i_cost[best],
                      'size', CASE WHEN strategy OPERATOR(pg_catalog.=) 'by_disk_size' THEN i_size[best] END,
                      'reason', CASE WHEN v_drain THEN 'drain' ELSE 'balance' END));
        g_load[s] := g_load[s] OPERATOR(pg_catalog.-) i_cost[best];
        g_load[t] := g_load[t] OPERATOR(pg_catalog.+) i_cost[best];
        i_owner[best] := g_id[t];
        i_mov[best] := false;
    END LOOP;

    RETURN QUERY
    SELECT p.seq, p.c, p.b, p.source, p.target, p.cost, p.size, p.reason
      FROM pg_catalog.jsonb_to_recordset(v_plan) AS p(seq int, c int, b int, source int, target int, cost int8, size int8, reason text)
     ORDER BY p.seq;
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_plan_simulate(jsonb, text, float8, int, bool) FROM PUBLIC;

-- The plan of a rebalance for the state of the cluster now.
CREATE FUNCTION kingres.plan_rebalance(strategy text DEFAULT 'by_bucket_count', threshold float8 DEFAULT NULL, max_moves int DEFAULT NULL,
                                        drain_only bool DEFAULT false, extra_load jsonb DEFAULT NULL, exclude_groups int[] DEFAULT NULL,
                                        colocation_ids int[] DEFAULT NULL)
    RETURNS TABLE (seq int, colocation_id int, bucket int, source_group int, target_group int, cost bigint, size_bytes bigint, reason text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    RETURN QUERY
    SELECT p.seq, p.colocation_id, p.bucket, p.source_group, p.target_group, p.cost, p.size_bytes, p.reason
      FROM kingres.internal_plan_simulate(kingres.internal_plan_inputs(strategy, extra_load, exclude_groups, colocation_ids),
                                          strategy, threshold, max_moves, drain_only) p;
END;
$$;
COMMENT ON FUNCTION kingres.plan_rebalance(text, float8, int, bool, jsonb, int[], int[]) IS
    'The plan of a rebalance by simulation: by_bucket_count or by_disk_size, a threshold, drain_only, the load of other databases, groups to treat as not receiving; read only';

-- ---------------------------------------------------------------------------
-- P10.4: several moves at once (design/shard-phase10.md, sections 6.3 and 8.5).  Moves of different buckets never needed anything from the
-- extension to run together (t/202 proves it, with readers running); what the scheduler of the control plane still lacks is one call that says
-- how far every unfinished move is.
-- ---------------------------------------------------------------------------

-- The unfinished moves with the sizes that say how far each is: the bytes of the bucket's partitions on the source and the bytes of the
-- standalone copy on the target (both read from the nodes; NULL when a node does not answer).  percent follows the design: preparing 0, copying the
-- target's size over the source's (at most 89), catching_up 90, switching 95, cleaning 100.  Superusers only (it asks the nodes).
CREATE FUNCTION kingres.move_progress()
    RETURNS TABLE (move_id bigint, colocation_id int, bucket int, source_group int, target_group int, phase text, detail text,
                   started_at timestamptz, updated_at timestamptz, source_bytes bigint, target_bytes bigint, percent int)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    m        pg_catalog.record;
    v_node   pg_catalog.int4;
    v_src    pg_catalog.int8;
    v_tgt    pg_catalog.int8;
    v_pct    pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR m IN SELECT b.* FROM kingres.bucket_move b
              WHERE b.phase IN ('preparing', 'copying', 'catching_up', 'switching', 'cleaning') ORDER BY b.move_id
    LOOP
        v_src := NULL; v_tgt := NULL;
        v_node := kingres.internal_group_node(m.source_group);
        IF v_node IS NOT NULL THEN
            BEGIN
                v_src := (kingres.internal_node_exec(v_node,
                    pg_catalog.format('SELECT COALESCE(pg_catalog.sum(b.partition_size), 0)::pg_catalog.int8 FROM kingres.buckets b JOIN kingres.dist_table d ON d.relid OPERATOR(pg_catalog.=) b.table_name WHERE d.colocation_id OPERATOR(pg_catalog.=) %s AND b.bucket OPERATOR(pg_catalog.=) %s AND b.partition_size IS NOT NULL',
                                      m.colocation_id, m.bucket), 10000) OPERATOR(pg_catalog.->) 0 OPERATOR(pg_catalog.->>) 0)::pg_catalog.int8;
            EXCEPTION WHEN OTHERS THEN
                v_src := NULL;
            END;
        END IF;
        v_node := kingres.internal_group_node(m.target_group);
        IF v_node IS NOT NULL THEN
            BEGIN
                v_tgt := (kingres.internal_node_exec(v_node,
                    pg_catalog.format('SELECT COALESCE(pg_catalog.sum(pg_catalog.pg_total_relation_size(c.oid)), 0)::pg_catalog.int8 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_description d ON d.objoid OPERATOR(pg_catalog.=) c.oid AND d.classoid OPERATOR(pg_catalog.=) ''pg_catalog.pg_class''::pg_catalog.regclass WHERE c.relkind OPERATOR(pg_catalog.=) ''r'' AND d.description OPERATOR(pg_catalog.=) ''kingres_shard bucket move %s''',
                                      m.move_id), 10000) OPERATOR(pg_catalog.->) 0 OPERATOR(pg_catalog.->>) 0)::pg_catalog.int8;
            EXCEPTION WHEN OTHERS THEN
                v_tgt := NULL;
            END;
        END IF;
        v_pct := CASE m.phase
                   WHEN 'preparing' THEN 0
                   WHEN 'copying' THEN CASE WHEN COALESCE(v_src, 0) OPERATOR(pg_catalog.>) 0 AND v_tgt IS NOT NULL
                                            THEN (CASE WHEN v_tgt * 100 / v_src OPERATOR(pg_catalog.>) 89 THEN 89 ELSE (v_tgt * 100 / v_src)::pg_catalog.int4 END) ELSE 0 END
                   WHEN 'catching_up' THEN 90
                   WHEN 'switching' THEN 95
                   ELSE 100 END;
        move_id := m.move_id; colocation_id := m.colocation_id; bucket := m.bucket;
        source_group := m.source_group; target_group := m.target_group; phase := m.phase; detail := m.detail;
        started_at := m.started_at; updated_at := m.updated_at; source_bytes := v_src; target_bytes := v_tgt; percent := v_pct;
        RETURN NEXT;
    END LOOP;
END;
$$;
COMMENT ON FUNCTION kingres.move_progress() IS
    'The unfinished bucket moves with the bytes of the bucket on the source and of its copy on the target, and a percent by phase; superusers only (it asks the nodes)';
REVOKE ALL ON FUNCTION kingres.move_progress() FROM PUBLIC;

-- What a drain waits for, one row per shard group: the buckets the map gives it, the buckets that are on their way to it (unfinished moves with the group as
-- target) and away from it, and whether the group is drained: it may not receive buckets, owns none and none is arriving.  A group with a bucket that is
-- leaving still owns it until the switch.  Read only.
CREATE FUNCTION kingres.drain_status()
    RETURNS TABLE (group_id int, receiving bool, available bool, buckets int, arriving int, leaving int, drained bool)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
WITH grp AS (
    SELECT n.group_id,
           pg_catalog.bool_or(n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active) AS avail,
           pg_catalog.bool_or(n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND n.should_have_shards) AS recv
      FROM kingres.node n GROUP BY n.group_id
    UNION
    SELECT m.group_id, false, false FROM kingres.bucket_map m WHERE NOT EXISTS (SELECT 1 FROM kingres.node n WHERE n.group_id OPERATOR(pg_catalog.=) m.group_id)
),
mv AS (
    SELECT b.source_group, b.target_group FROM kingres.bucket_move b WHERE b.phase IN ('preparing', 'copying', 'catching_up', 'switching')
)
SELECT g.group_id, COALESCE(g.recv, false), COALESCE(g.avail, false),
       (SELECT pg_catalog.count(*) FROM kingres.bucket_map m WHERE m.group_id OPERATOR(pg_catalog.=) g.group_id)::pg_catalog.int4,
       (SELECT pg_catalog.count(*) FROM mv WHERE mv.target_group OPERATOR(pg_catalog.=) g.group_id)::pg_catalog.int4,
       (SELECT pg_catalog.count(*) FROM mv WHERE mv.source_group OPERATOR(pg_catalog.=) g.group_id)::pg_catalog.int4,
       NOT COALESCE(g.recv, false)
         AND NOT EXISTS (SELECT 1 FROM kingres.bucket_map m WHERE m.group_id OPERATOR(pg_catalog.=) g.group_id)
         AND NOT EXISTS (SELECT 1 FROM mv WHERE mv.target_group OPERATOR(pg_catalog.=) g.group_id)
  FROM (SELECT DISTINCT ON (x.group_id) x.* FROM grp x ORDER BY x.group_id) g
 ORDER BY g.group_id
$$;
COMMENT ON FUNCTION kingres.drain_status() IS
    'Per shard group: the buckets it owns, the buckets arriving and leaving, and whether it is drained (may not receive, owns none, none arriving); read only';

-- ---------------------------------------------------------------------------
-- P10.5: splitting a bucket (design/shard-phase10.md, sections 1 to 3).
--
-- split_bucket(colocation, bucket, parts) replaces the partition of one bucket, in every table of the colocation group, by `parts` partitions of a
-- larger modulus, in place (the children stay on the group of the bucket).  The copy is online: the children are standalone tables that a row trigger on the old
-- partition keeps current in the writer's own transaction; a backfill copies what was there; the switch (writers wait, readers wait only for the detach)
-- replaces the partitions on the owning node in one transaction, and the catalog follows on the map group primary.
--
-- Differences from the design text, on purpose: the record is its own table (kingres.bucket_split) and not columns of bucket_move, so that the
-- recovery of moves is not touched; the switch is one transaction on the owning node and the catalog change comes after it (the children are on the same group as
-- the bucket, so every router that still has the old layout reaches the right group, and the epoch makes a scan that overlaps it retry), recovery decides from
-- the partitions on the owner; and a user-partitioned table, a column that is generated or an identity, a table without primary key and a table that is not heap are refused.
-- ---------------------------------------------------------------------------

ALTER TABLE kingres.bucket_map DROP CONSTRAINT bucket_map_state_check;
ALTER TABLE kingres.bucket_map ADD CONSTRAINT bucket_map_state_check CHECK (state IN ('active', 'moving', 'splitting', 'orphan'));

-- Map group primary only, never synchronized: the record of every split of a bucket.
CREATE TABLE kingres.bucket_split (
    split_id         bigint      GENERATED ALWAYS AS IDENTITY,
    colocation_id    int         NOT NULL,
    bucket           int         NOT NULL,
    group_id         int         NOT NULL,
    modulus          int         NOT NULL,
    parts            int         NOT NULL,
    verify           text        NOT NULL DEFAULT 'count',
    phase            text        NOT NULL,
    detail           text,
    started_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    finished_at      timestamptz,
    cancel_requested bool        NOT NULL DEFAULT false,
    cleanup_after    timestamptz,
    rows_done        bigint      NOT NULL DEFAULT 0,
    rows_total       bigint,
    CONSTRAINT bucket_split_pkey PRIMARY KEY (split_id),
    CONSTRAINT bucket_split_phase_check CHECK (phase IN ('preparing', 'copying', 'catching_up', 'switching', 'cleaning', 'done', 'failed', 'cancelled')),
    CONSTRAINT bucket_split_parts_check CHECK (parts IN (2, 4, 8, 16)),
    CONSTRAINT bucket_split_verify_check CHECK (verify IN ('count', 'full', 'none'))
);
-- One split per colocation group at a time.
CREATE UNIQUE INDEX bucket_split_unfinished_idx ON kingres.bucket_split (colocation_id)
    WHERE phase IN ('preparing', 'copying', 'catching_up', 'switching');
COMMENT ON TABLE kingres.bucket_split IS
    'Map group primary only, never synchronized: the record of every split of a bucket';
REVOKE ALL ON kingres.bucket_split FROM PUBLIC;

-- One statement of the extension's own DDL (a trigger on a bucket partition, which the utility hook refuses for a user).
CREATE FUNCTION kingres.internal_ddl(sql text)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_ddl'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;
REVOKE ALL ON FUNCTION kingres.internal_ddl(text) FROM PUBLIC;

-- The live mark of a split: held by the session that runs it (resume_bucket_splits() leaves such a split alone).
CREATE FUNCTION kingres.internal_split_lock(p_split bigint)
    RETURNS bool
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$ SELECT pg_catalog.pg_try_advisory_lock(pg_catalog.hashtext('kingres_split'), p_split::pg_catalog.int4) $$;
CREATE FUNCTION kingres.internal_split_alive(p_split bigint)
    RETURNS bool
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    IF pg_catalog.pg_try_advisory_lock(pg_catalog.hashtext('kingres_split'), p_split::pg_catalog.int4) THEN
        PERFORM pg_catalog.pg_advisory_unlock(pg_catalog.hashtext('kingres_split'), p_split::pg_catalog.int4);
        RETURN false;
    END IF;
    RETURN true;
END;
$$;

-- A hook for the lines of work that must keep some colocation groups whole (a group that holds a distributed schema is never split): NULL when the group
-- may be split, else the reason.
CREATE FUNCTION kingres.internal_split_allowed(p_colocation int)
    RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$ SELECT NULL::pg_catalog.text $$;

-- On the owning node: what is split, table by table (in the order of the foreign keys), and what refuses it.
CREATE FUNCTION kingres.internal_split_inspect(p_colocation int, p_bucket int)
    RETURNS pg_catalog.jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t      pg_catalog.record;
    v_rel  pg_catalog.regclass;
    v_part pg_catalog.regclass;
    v_pk   pg_catalog.text[];
    v_cols pg_catalog.text[];
    v_dist pg_catalog.name;
    v_pm   pg_catalog.text;
    v_kind pg_catalog."char";
    v_am   pg_catalog.oid;
    v_opts pg_catalog.text[];
    v_rows pg_catalog.float8;
    v_out  pg_catalog.jsonb := '[]'::pg_catalog.jsonb;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR t IN SELECT * FROM kingres.internal_move_tables(p_colocation) ORDER BY ord LOOP
        v_rel := pg_catalog.format('%I.%I', t.schema_name, t.table_name)::pg_catalog.regclass;
        SELECT d.dist_column, d.partition_method INTO v_dist, v_pm FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_rel;
        IF v_pm IS NOT NULL THEN
            RAISE EXCEPTION 'table % is partitioned by the user: a split of its buckets is not supported yet', v_rel USING ERRCODE = 'feature_not_supported';
        END IF;
        v_part := pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, kingres.internal_bucket_part_name(t.table_name, p_bucket)));
        IF v_part IS NULL THEN
            RAISE EXCEPTION 'the partition of bucket % of table % is not on this node', p_bucket, v_rel USING ERRCODE = 'object_not_in_prerequisite_state';
        END IF;
        SELECT c.relkind, c.relam, c.reloptions, c.reltuples INTO v_kind, v_am, v_opts, v_rows FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) v_part::pg_catalog.oid;
        IF v_kind OPERATOR(pg_catalog.<>) 'r' THEN
            RAISE EXCEPTION 'the partition % is itself partitioned: a split of its bucket is not supported yet', v_part USING ERRCODE = 'feature_not_supported';
        END IF;
        IF v_am OPERATOR(pg_catalog.<>) (SELECT a.oid FROM pg_catalog.pg_am a WHERE a.amname OPERATOR(pg_catalog.=) 'heap') THEN
            RAISE EXCEPTION 'table % does not use the heap access method: a bucket of such a table cannot be split', v_rel USING ERRCODE = 'feature_not_supported';
        END IF;
        SELECT pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY k.ord) INTO v_pk
          FROM pg_catalog.pg_index i
          CROSS JOIN LATERAL pg_catalog.unnest(i.indkey::pg_catalog.int2[]) WITH ORDINALITY k(attnum, ord)
          JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) i.indrelid AND a.attnum OPERATOR(pg_catalog.=) k.attnum
         WHERE i.indrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid AND i.indisprimary;
        IF v_pk IS NULL THEN
            RAISE EXCEPTION 'table % has no primary key: a split matches the rows of the children by key', v_rel USING ERRCODE = 'object_not_in_prerequisite_state';
        END IF;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid AND a.attnum OPERATOR(pg_catalog.>) 0
                      AND NOT a.attisdropped AND (a.attgenerated OPERATOR(pg_catalog.<>) '' OR a.attidentity OPERATOR(pg_catalog.<>) '')) THEN
            RAISE EXCEPTION 'table % has a generated or an identity column: a bucket of such a table cannot be split yet', v_rel USING ERRCODE = 'feature_not_supported';
        END IF;
        SELECT pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY a.attnum) INTO v_cols
          FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped;
        v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object(
                    'ord', t.ord, 'schema', t.schema_name, 'table', t.table_name,
                    'part', kingres.internal_bucket_part_name(t.table_name, p_bucket), 'dist', v_dist,
                    'pk', pg_catalog.to_jsonb(v_pk), 'cols', pg_catalog.to_jsonb(v_cols),
                    'reloptions', COALESCE(pg_catalog.to_jsonb(v_opts), 'null'::pg_catalog.jsonb),
                    'rows', CASE WHEN v_rows OPERATOR(pg_catalog.>) 0 THEN v_rows::pg_catalog.int8 ELSE 0 END));
    END LOOP;
    RETURN v_out;
END;
$$;

-- On the owning node, in one transaction: the children (standalone tables with the check that spares the scan of the later attach) and the triggers that keep
-- them current.  Every write of the old partition, whoever makes it, also makes the same change in the child that covers the row.
CREATE FUNCTION kingres.internal_split_prepare(p_split bigint, p_bucket int, p_modulus int, p_parts int, p_inspect pg_catalog.jsonb, p_lock_ms int)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    el       pg_catalog.jsonb;
    j        pg_catalog.int4;
    r        pg_catalog.int4;
    v_new    pg_catalog.int4 := p_modulus OPERATOR(pg_catalog.*) p_parts;
    v_parent pg_catalog.text;
    v_part   pg_catalog.text;
    v_child  pg_catalog.text;
    v_with   pg_catalog.text;
    v_fn     pg_catalog.text;
    v_cols   pg_catalog.text;
    v_vals   pg_catalog.text;
    v_set    pg_catalog.text;
    v_pkl    pg_catalog.text;
    v_pkm    pg_catalog.text;
    v_dist   pg_catalog.text;
    v_del    pg_catalog.text;
    v_ins    pg_catalog.text;
    v_trunc  pg_catalog.text;
    v_body   pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM pg_catalog.set_config('lock_timeout', p_lock_ms::pg_catalog.text, true);
    FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x LOOP
        v_parent := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'table');
        v_part := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'part');
        v_dist := pg_catalog.quote_ident(el OPERATOR(pg_catalog.->>) 'dist');
        v_with := CASE WHEN pg_catalog.jsonb_typeof(el OPERATOR(pg_catalog.->) 'reloptions') OPERATOR(pg_catalog.=) 'array'
                       THEN kingres.internal_with_clause(ARRAY(SELECT pg_catalog.jsonb_array_elements_text(el OPERATOR(pg_catalog.->) 'reloptions'))) ELSE '' END;
        SELECT pg_catalog.string_agg(pg_catalog.quote_ident(c), ', '), pg_catalog.string_agg('NEW.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident(c), ', '),
               pg_catalog.string_agg(pg_catalog.format('%I = EXCLUDED.%I', c, c), ', ')
          INTO v_cols, v_vals, v_set FROM pg_catalog.jsonb_array_elements_text(el OPERATOR(pg_catalog.->) 'cols') c;
        SELECT pg_catalog.string_agg(pg_catalog.quote_ident(c), ', '), pg_catalog.string_agg(pg_catalog.format('%I = OLD.%I', c, c), ' AND ')
          INTO v_pkl, v_pkm FROM pg_catalog.jsonb_array_elements_text(el OPERATOR(pg_catalog.->) 'pk') c;
        v_del := ''; v_ins := ''; v_trunc := '';
        FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
            r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
            v_child := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', pg_catalog.format('%s_s%s_%s', el OPERATOR(pg_catalog.->>) 'table', p_split, r));
            EXECUTE pg_catalog.format('CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES INCLUDING STORAGE INCLUDING COMMENTS) %s', v_child, v_parent, v_with);
            EXECUTE pg_catalog.format('ALTER TABLE %s ADD CONSTRAINT ks_bucket_check CHECK (pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass::pg_catalog.oid, %s, %s, %s))',
                                      v_child, v_parent, v_new, r, v_dist);
            EXECUTE pg_catalog.format('COMMENT ON TABLE %s IS %L', v_child, 'kingres_shard bucket split ' OPERATOR(pg_catalog.||) p_split::pg_catalog.text);
            v_del := v_del OPERATOR(pg_catalog.||) pg_catalog.format(E'        IF pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass::pg_catalog.oid, %s, %s, OLD.%s) THEN DELETE FROM %s WHERE %s; END IF;\n',
                                                                    v_parent, v_new, r, v_dist, v_child, v_pkm);
            v_ins := v_ins OPERATOR(pg_catalog.||) pg_catalog.format(E'        IF pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass::pg_catalog.oid, %s, %s, NEW.%s) THEN INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s; END IF;\n',
                                                                    v_parent, v_new, r, v_dist, v_child, v_cols, v_vals, v_pkl, v_set);
            v_trunc := v_trunc OPERATOR(pg_catalog.||) CASE WHEN v_trunc OPERATOR(pg_catalog.=) '' THEN '' ELSE ', ' END OPERATOR(pg_catalog.||) v_child;
        END LOOP;
        v_fn := pg_catalog.format('split_capture_%s_%s', p_split, el OPERATOR(pg_catalog.->>) 'ord');
        v_body := pg_catalog.format(E'BEGIN\n    IF TG_OP = ''TRUNCATE'' THEN\n        TRUNCATE %s;\n        RETURN NULL;\n    END IF;\n    IF TG_OP IN (''UPDATE'', ''DELETE'') THEN\n%s    END IF;\n    IF TG_OP IN (''INSERT'', ''UPDATE'') THEN\n%s    END IF;\n    RETURN NULL;\nEND', v_trunc, v_del, v_ins);
        EXECUTE pg_catalog.format('CREATE FUNCTION kingres.%I() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS %L', v_fn, v_body);
        PERFORM kingres.internal_ddl(pg_catalog.format('CREATE TRIGGER ks_split_capture_row AFTER INSERT OR UPDATE OR DELETE ON %s FOR EACH ROW EXECUTE FUNCTION kingres.%I()', v_part, v_fn));
        PERFORM kingres.internal_ddl(pg_catalog.format('ALTER TABLE %s ENABLE ALWAYS TRIGGER ks_split_capture_row', v_part));
        PERFORM kingres.internal_ddl(pg_catalog.format('CREATE TRIGGER ks_split_capture_truncate AFTER TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION kingres.%I()', v_part, v_fn));
        PERFORM kingres.internal_ddl(pg_catalog.format('ALTER TABLE %s ENABLE ALWAYS TRIGGER ks_split_capture_truncate', v_part));
    END LOOP;
END;
$$;

-- On the owning node, in a transaction of its own: one batch of the backfill of one table.  The rows after p_last (a JSON object of the key) in key order, those that
-- no writer holds, are locked FOR SHARE (a writer waits for the batch, the copy cannot resurrect a row that a delete removes) and copied into the child that covers
-- them; a row that is there already is left.  Returns {n: rows taken, inserted: rows that were not in the children, last: the last key, skipped: rows in the range that
-- a writer held}.
CREATE FUNCTION kingres.internal_split_batch(p_split bigint, p_bucket int, p_modulus int, p_parts int, p_el pg_catalog.jsonb, p_last pg_catalog.jsonb, p_batch int)
    RETURNS pg_catalog.jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_new    pg_catalog.int4 := p_modulus OPERATOR(pg_catalog.*) p_parts;
    v_parent pg_catalog.text := pg_catalog.format('%I.%I', p_el OPERATOR(pg_catalog.->>) 'schema', p_el OPERATOR(pg_catalog.->>) 'table');
    v_part   pg_catalog.text := pg_catalog.format('%I.%I', p_el OPERATOR(pg_catalog.->>) 'schema', p_el OPERATOR(pg_catalog.->>) 'part');
    v_dist   pg_catalog.text := pg_catalog.quote_ident(p_el OPERATOR(pg_catalog.->>) 'dist');
    v_pkl    pg_catalog.text;
    v_pkr    pg_catalog.text;
    v_cond   pg_catalog.text;
    v_sql    pg_catalog.text;
    v_ctes   pg_catalog.text := '';
    v_sum    pg_catalog.text := '';
    j        pg_catalog.int4;
    r        pg_catalog.int4;
    v_child  pg_catalog.text;
    v_res    pg_catalog.record;
    v_skip   pg_catalog.int8;
    v_upper  pg_catalog.text := '';
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT pg_catalog.string_agg(pg_catalog.quote_ident(c), ', '), pg_catalog.string_agg('r.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident(c), ', ')
      INTO v_pkl, v_pkr FROM pg_catalog.jsonb_array_elements_text(p_el OPERATOR(pg_catalog.->) 'pk') c;
    v_cond := CASE WHEN p_last IS NULL THEN 'true'
                   ELSE pg_catalog.format('(%s) OPERATOR(pg_catalog.>) (SELECT %s FROM pg_catalog.jsonb_populate_record(NULL::%s, %L::pg_catalog.jsonb) r)', v_pkl, v_pkr, v_part, p_last::pg_catalog.text) END;
    FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
        r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
        v_child := pg_catalog.format('%I.%I', p_el OPERATOR(pg_catalog.->>) 'schema', pg_catalog.format('%s_s%s_%s', p_el OPERATOR(pg_catalog.->>) 'table', p_split, r));
        v_ctes := v_ctes OPERATOR(pg_catalog.||) pg_catalog.format(', i%s AS (INSERT INTO %s SELECT * FROM b WHERE pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass::pg_catalog.oid, %s, %s, %s) ON CONFLICT DO NOTHING RETURNING 1)',
                                                                  j, v_child, v_parent, v_new, r, v_dist);
        v_sum := v_sum OPERATOR(pg_catalog.||) pg_catalog.format(' OPERATOR(pg_catalog.+) (SELECT pg_catalog.count(*) FROM i%s)', j);
    END LOOP;
    v_sql := pg_catalog.format('WITH b AS (SELECT * FROM %s WHERE %s ORDER BY %s LIMIT %s FOR SHARE SKIP LOCKED)%s SELECT (SELECT pg_catalog.count(*) FROM b) AS n, (0::pg_catalog.int8%s) AS ins, (SELECT pg_catalog.to_jsonb(x) FROM (SELECT %s FROM b ORDER BY %s DESC LIMIT 1) x) AS lastk',
                               v_part, v_cond, v_pkl, p_batch, v_ctes, v_sum, v_pkl, v_pkl);
    EXECUTE v_sql INTO v_res;
    -- the rows of the range that a writer held and the batch skipped
    IF v_res.n OPERATOR(pg_catalog.=) p_batch AND v_res.lastk IS NOT NULL THEN
        v_upper := pg_catalog.format(' AND (%s) OPERATOR(pg_catalog.<=) (SELECT %s FROM pg_catalog.jsonb_populate_record(NULL::%s, %L::pg_catalog.jsonb) r)', v_pkl, v_pkr, v_part, v_res.lastk::pg_catalog.text);
    END IF;
    EXECUTE pg_catalog.format('SELECT pg_catalog.count(*) FROM %s WHERE %s%s', v_part, v_cond, v_upper) INTO v_skip;
    RETURN pg_catalog.jsonb_build_object('n', v_res.n, 'inserted', v_res.ins, 'last', v_res.lastk,
                                         'skipped', CASE WHEN v_skip OPERATOR(pg_catalog.>) v_res.n THEN v_skip OPERATOR(pg_catalog.-) v_res.n ELSE 0 END);
END;
$$;

-- On the owning node, in one transaction: lock the tables, verify, drop the triggers, replace the old partitions by the children.  Writers of the tables on this node wait from the lock
-- to the commit (SHARE); readers wait for the detach only.  An error (a lock that is not got within p_lock_ms, a verification that fails) rolls everything back.
CREATE FUNCTION kingres.internal_split_switch(p_split bigint, p_bucket int, p_modulus int, p_parts int, p_inspect pg_catalog.jsonb, p_lock_ms int, p_verify text)
    RETURNS pg_catalog.jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    el       pg_catalog.jsonb;
    j        pg_catalog.int4;
    r        pg_catalog.int4;
    v_new    pg_catalog.int4 := p_modulus OPERATOR(pg_catalog.*) p_parts;
    v_parent pg_catalog.text;
    v_part   pg_catalog.text;
    v_child  pg_catalog.text;
    v_dist   pg_catalog.text;
    v_pkm    pg_catalog.text;
    v_old    pg_catalog.int8;
    v_kids   pg_catalog.int8;
    v_n      pg_catalog.int8;
    v_target pg_catalog.text;
    v_rows   pg_catalog.int8 := 0;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM pg_catalog.set_config('lock_timeout', p_lock_ms::pg_catalog.text, true);
    FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x LOOP
        EXECUTE pg_catalog.format('LOCK TABLE ONLY %I.%I, ONLY %I.%I IN SHARE MODE', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'table',
                                  el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'part');
    END LOOP;
    -- with the writers out the children must equal the partition: count, bring them in line once if they differ, count again
    IF p_verify IN ('count', 'full') THEN
        FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x LOOP
            v_parent := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'table');
            v_part := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'part');
            v_dist := pg_catalog.quote_ident(el OPERATOR(pg_catalog.->>) 'dist');
            SELECT pg_catalog.string_agg(pg_catalog.format('c.%I OPERATOR(pg_catalog.=) p.%I', k, k), ' AND ') INTO v_pkm
              FROM pg_catalog.jsonb_array_elements_text(el OPERATOR(pg_catalog.->) 'pk') k;
            FOR v_n IN 1 .. 2 LOOP
                EXECUTE pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', v_part) INTO v_old;
                v_kids := 0;
                FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
                    r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
                    v_child := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', pg_catalog.format('%s_s%s_%s', el OPERATOR(pg_catalog.->>) 'table', p_split, r));
                    EXECUTE pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', v_child) INTO v_target;
                    v_kids := v_kids OPERATOR(pg_catalog.+) v_target::pg_catalog.int8;
                END LOOP;
                -- 'full' brings the children in line with the partition even when the counts agree (a child can miss a row and hold a stray one)
                EXIT WHEN v_kids OPERATOR(pg_catalog.=) v_old AND (p_verify OPERATOR(pg_catalog.=) 'count' OR v_n OPERATOR(pg_catalog.=) 2);
                IF v_n OPERATOR(pg_catalog.=) 2 THEN
                    RAISE EXCEPTION 'the children of bucket % of table % hold % rows and the partition holds %', p_bucket, v_parent, v_kids, v_old
                        USING ERRCODE = 'data_exception';
                END IF;
                FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
                    r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
                    v_child := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', pg_catalog.format('%s_s%s_%s', el OPERATOR(pg_catalog.->>) 'table', p_split, r));
                    EXECUTE pg_catalog.format('DELETE FROM %s c WHERE NOT EXISTS (SELECT 1 FROM %s p WHERE %s)', v_child, v_part, v_pkm);
                    EXECUTE pg_catalog.format('INSERT INTO %s SELECT p.* FROM %s p WHERE pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass::pg_catalog.oid, %s, %s, p.%s) ON CONFLICT DO NOTHING',
                                              v_child, v_part, v_parent, v_new, r, v_dist);
                END LOOP;
            END LOOP;
            v_rows := v_rows OPERATOR(pg_catalog.+) v_old;
        END LOOP;
    END IF;
    -- the triggers and their functions go (the old partition must never write into a child again), the old partitions leave, the children take their place
    FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x LOOP
        v_part := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'part');
        PERFORM kingres.internal_ddl(pg_catalog.format('DROP TRIGGER ks_split_capture_row ON %s', v_part));
        PERFORM kingres.internal_ddl(pg_catalog.format('DROP TRIGGER ks_split_capture_truncate ON %s', v_part));
        EXECUTE pg_catalog.format('DROP FUNCTION kingres.%I()', pg_catalog.format('split_capture_%s_%s', p_split, el OPERATOR(pg_catalog.->>) 'ord'));
    END LOOP;
    FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x ORDER BY (x OPERATOR(pg_catalog.->>) 'ord')::pg_catalog.int4 DESC LOOP
        PERFORM kingres.internal_detach_bucket(pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'table')::pg_catalog.regclass, p_bucket,
                                               pg_catalog.format('orphan_s%s_%s', p_split, el OPERATOR(pg_catalog.->>) 'ord'));
    END LOOP;
    FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x ORDER BY (x OPERATOR(pg_catalog.->>) 'ord')::pg_catalog.int4 LOOP
        FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
            r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
            v_child := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', pg_catalog.format('%s_s%s_%s', el OPERATOR(pg_catalog.->>) 'table', p_split, r));
            EXECUTE pg_catalog.format('ALTER TABLE %s RENAME TO %I', v_child, kingres.internal_bucket_part_name(el OPERATOR(pg_catalog.->>) 'table', r));
            PERFORM kingres.internal_attach_bucket(pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'table')::pg_catalog.regclass, r, v_new);
        END LOOP;
    END LOOP;
    RETURN pg_catalog.jsonb_build_object('rows', v_rows);
END;
$$;

-- On the owning node: whatever a split that did not switch left (the triggers, their functions, the children); safe to run again.
CREATE FUNCTION kingres.internal_split_undo(p_split bigint, p_colocation int, p_bucket int, p_modulus int, p_parts int)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t      pg_catalog.record;
    v_part pg_catalog.regclass;
    j      pg_catalog.int4;
    r      pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR t IN SELECT * FROM kingres.internal_move_tables(p_colocation) ORDER BY ord LOOP
        v_part := pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, kingres.internal_bucket_part_name(t.table_name, p_bucket)));
        IF v_part IS NOT NULL THEN
            PERFORM kingres.internal_ddl(pg_catalog.format('DROP TRIGGER IF EXISTS ks_split_capture_row ON %s', v_part));
            PERFORM kingres.internal_ddl(pg_catalog.format('DROP TRIGGER IF EXISTS ks_split_capture_truncate ON %s', v_part));
        END IF;
        EXECUTE pg_catalog.format('DROP FUNCTION IF EXISTS kingres.%I()', pg_catalog.format('split_capture_%s_%s', p_split, t.ord));
        FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
            r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
            EXECUTE pg_catalog.format('DROP TABLE IF EXISTS %I.%I', t.schema_name, pg_catalog.format('%s_s%s_%s', t.table_name, p_split, r));
        END LOOP;
    END LOOP;
END;
$$;

-- On the owning node: 'switched' when the partition of the bucket has the modulus of the children (the switch committed), else 'not'.
CREATE FUNCTION kingres.internal_split_state(p_colocation int, p_bucket int, p_newmod int)
    RETURNS text
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t      pg_catalog.record;
    v_bnd  pg_catalog.text;
BEGIN
    SELECT * INTO t FROM kingres.internal_move_tables(p_colocation) ORDER BY ord LIMIT 1;
    SELECT pg_catalog.pg_get_expr(c.relpartbound, c.oid) INTO v_bnd FROM pg_catalog.pg_class c
     WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, kingres.internal_bucket_part_name(t.table_name, p_bucket)))::pg_catalog.oid;
    RETURN CASE WHEN v_bnd IS NOT NULL AND v_bnd OPERATOR(pg_catalog.~) pg_catalog.format('modulus %s, ', p_newmod) THEN 'switched' ELSE 'not' END;
END;
$$;

-- On the owning node: the old partitions of a finished split.
CREATE FUNCTION kingres.internal_split_drop_orphans(p_split bigint)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    x pg_catalog.record;
    n pg_catalog.int4 := 0;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR x IN SELECT c.relname FROM pg_catalog.pg_class c
              WHERE c.relnamespace OPERATOR(pg_catalog.=) 'kingres'::pg_catalog.regnamespace AND c.relkind OPERATOR(pg_catalog.=) 'r'
                AND c.relname OPERATOR(pg_catalog.~) pg_catalog.format('^orphan_s%s_[0-9]+$', p_split)
    LOOP
        EXECUTE pg_catalog.format('DROP TABLE kingres.%I', x.relname);
        n := n OPERATOR(pg_catalog.+) 1;
    END LOOP;
    RETURN n;
END;
$$;

-- On the map group primary (called through a session of its own, so that each change is committed at once).
CREATE FUNCTION kingres.internal_split_begin(p_colocation int, p_bucket int, p_parts int, p_verify text, p_total bigint)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_group  pg_catalog.int4;
    v_mod    pg_catalog.int4;
    v_state  pg_catalog.text;
    v_id     pg_catalog.int8;
    v_op     pg_catalog.int8;
BEGIN
    PERFORM kingres.internal_require_superuser();
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    PERFORM kingres.internal_check_node_versions();
    SELECT l.op_id INTO v_op FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) p_colocation;
    IF FOUND THEN
        RAISE EXCEPTION 'colocation group % is used by table operation %', p_colocation, v_op USING ERRCODE = 'object_in_use';
    END IF;
    SELECT m.group_id, m.modulus, m.state INTO v_group, v_mod, v_state FROM kingres.bucket_map m
     WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation AND m.bucket OPERATOR(pg_catalog.=) p_bucket;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'bucket % of colocation group % does not exist', p_bucket, p_colocation USING ERRCODE = 'undefined_object';
    END IF;
    IF v_state OPERATOR(pg_catalog.<>) 'active' THEN
        RAISE EXCEPTION 'bucket % of colocation group % is in state "%"', p_bucket, p_colocation, v_state USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF EXISTS (SELECT 1 FROM kingres.bucket_split s WHERE s.colocation_id OPERATOR(pg_catalog.=) p_colocation AND s.phase IN ('preparing', 'copying', 'catching_up', 'switching')) THEN
        RAISE EXCEPTION 'colocation group % is already being split', p_colocation USING ERRCODE = 'object_in_use',
              HINT = 'See kingres.bucket_split; a split can be cancelled with kingres.cancel_bucket_split().';
    END IF;
    INSERT INTO kingres.bucket_split (colocation_id, bucket, group_id, modulus, parts, verify, phase, detail, started_at, updated_at, rows_total)
    VALUES (p_colocation, p_bucket, v_group, v_mod, p_parts, p_verify, 'preparing', 'starting', pg_catalog.clock_timestamp(), pg_catalog.clock_timestamp(), p_total)
    RETURNING split_id INTO v_id;
    UPDATE kingres.bucket_map m SET state = 'splitting' WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation AND m.bucket OPERATOR(pg_catalog.=) p_bucket;
    PERFORM kingres.sync_catalog();
    RETURN v_id;
END;
$$;

CREATE FUNCTION kingres.internal_split_set(p_split bigint, p_phase text, p_detail text, p_rows bigint DEFAULT NULL)
    RETURNS void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
UPDATE kingres.bucket_split s SET phase = p_phase, detail = p_detail, updated_at = pg_catalog.clock_timestamp(), rows_done = COALESCE(p_rows, s.rows_done)
 WHERE s.split_id OPERATOR(pg_catalog.=) p_split AND s.phase IN ('preparing', 'copying', 'catching_up', 'switching')
$$;

-- The catalog follows the switch: the bucket becomes its children (same group, the larger modulus), the group's max_modulus and epoch go up.  Safe to run again.
CREATE FUNCTION kingres.internal_split_finish(p_split bigint)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    s       pg_catalog.record;
    v_new   pg_catalog.int4;
    v_delay pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    SELECT * INTO s FROM kingres.bucket_split b WHERE b.split_id OPERATOR(pg_catalog.=) p_split FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'split % does not exist', p_split USING ERRCODE = 'undefined_object';
    END IF;
    IF s.phase IN ('cleaning', 'done') THEN
        RETURN;
    END IF;
    v_new := s.modulus OPERATOR(pg_catalog.*) s.parts;
    IF NOT EXISTS (SELECT 1 FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND m.bucket OPERATOR(pg_catalog.=) s.bucket AND m.modulus OPERATOR(pg_catalog.=) v_new) THEN
        DELETE FROM kingres.bucket_map m WHERE m.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND m.bucket OPERATOR(pg_catalog.=) s.bucket;
        INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id, state, modulus)
        SELECT s.colocation_id, s.bucket OPERATOR(pg_catalog.+) (i OPERATOR(pg_catalog.*) s.modulus), s.group_id, 'active', v_new
          FROM pg_catalog.generate_series(0, s.parts OPERATOR(pg_catalog.-) 1) i;
        UPDATE kingres.colocation c
           SET max_modulus = CASE WHEN c.max_modulus OPERATOR(pg_catalog.>) v_new THEN c.max_modulus ELSE v_new END, placement_epoch = c.placement_epoch OPERATOR(pg_catalog.+) 1
         WHERE c.colocation_id OPERATOR(pg_catalog.=) s.colocation_id;
    END IF;
    v_delay := (SELECT x.setting::pg_catalog.int4 FROM pg_catalog.pg_settings x WHERE x.name OPERATOR(pg_catalog.=) 'kingres_shard.move_cleanup_delay');
    UPDATE kingres.bucket_split b SET phase = 'cleaning', detail = NULL, updated_at = pg_catalog.clock_timestamp(),
           cleanup_after = pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+) pg_catalog.make_interval(secs => v_delay)
     WHERE b.split_id OPERATOR(pg_catalog.=) p_split;
    PERFORM kingres.sync_catalog();
END;
$$;

-- A split that ended without switching: the record says why, the bucket is active again.
CREATE FUNCTION kingres.internal_split_fail(p_split bigint, p_msg text, p_cancelled bool)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    s pg_catalog.record;
BEGIN
    PERFORM kingres.internal_require_superuser();
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    SELECT * INTO s FROM kingres.bucket_split b WHERE b.split_id OPERATOR(pg_catalog.=) p_split FOR UPDATE;
    IF NOT FOUND OR s.phase NOT IN ('preparing', 'copying', 'catching_up', 'switching') THEN
        RETURN;
    END IF;
    UPDATE kingres.bucket_split b SET phase = CASE WHEN p_cancelled THEN 'cancelled' ELSE 'failed' END, detail = pg_catalog.left(p_msg, 500),
           updated_at = pg_catalog.clock_timestamp(), finished_at = pg_catalog.clock_timestamp()
     WHERE b.split_id OPERATOR(pg_catalog.=) p_split;
    UPDATE kingres.bucket_map m SET state = 'active'
     WHERE m.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND m.bucket OPERATOR(pg_catalog.=) s.bucket AND m.state OPERATOR(pg_catalog.=) 'splitting';
    PERFORM kingres.sync_catalog();
END;
$$;

-- Splits one bucket into parts (2, 4, 8 or 16) of a modulus parts times larger, in place: the children stay on the group of the bucket.  Map group primary, superuser,
-- READ COMMITTED.  Returns the id of the record in kingres.bucket_split.  verify 'count' compares the number of rows of every table with its children under the switch lock
-- and brings the children in line when they differ; 'full' brings them in line always (key by key, both ways: it costs a pass over the bucket under the lock); 'none' trusts the triggers and the passes.
CREATE FUNCTION kingres.split_bucket(colocation_id int, bucket int, parts int DEFAULT 2, verify text DEFAULT 'count')
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_colo     pg_catalog.int4 := colocation_id;
    v_bucket   pg_catalog.int4 := bucket;
    v_parts    pg_catalog.int4 := parts;
    v_verify   pg_catalog.text := verify;
    v_self     pg_catalog.int4;
    v_group    pg_catalog.int4;
    v_owner    pg_catalog.int4;
    v_mod      pg_catalog.int4;
    v_state    pg_catalog.text;
    v_why      pg_catalog.text;
    v_inspect  pg_catalog.jsonb;
    v_total    pg_catalog.int8;
    v_id       pg_catalog.int8;
    v_stage    pg_catalog.int4 := 0;
    v_node_ms  pg_catalog.int4;
    v_sw_ms    pg_catalog.int4;
    v_max_att  pg_catalog.int4;
    v_lock_ms  pg_catalog.int4;
    v_batch    pg_catalog.int4 := 5000;
    el         pg_catalog.jsonb;
    r          pg_catalog.jsonb;
    v_last     pg_catalog.jsonb;
    v_pass     pg_catalog.int4;
    v_ins      pg_catalog.int8;
    v_skip     pg_catalog.int8;
    v_done     pg_catalog.int8 := 0;
    v_att      pg_catalog.int4;
    v_err      pg_catalog.text;
    v_cancel   pg_catalog.bool;
    v_pause    pg_catalog.float8;
    v_blockers pg_catalog.text;
    v_converged pg_catalog.bool := false;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    IF pg_catalog.current_setting('transaction_isolation') OPERATOR(pg_catalog.<>) 'read committed' THEN
        RAISE EXCEPTION 'kingres.split_bucket() must run in a READ COMMITTED transaction' USING ERRCODE = 'invalid_transaction_state';
    END IF;
    IF v_parts IS NULL OR v_parts NOT IN (2, 4, 8, 16) THEN
        RAISE EXCEPTION 'parts must be 2, 4, 8 or 16, not %', v_parts USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF v_verify IS NULL OR v_verify NOT IN ('count', 'full', 'none') THEN
        RAISE EXCEPTION 'verify must be count, full or none, not %', v_verify USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_node_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') OPERATOR(pg_catalog.*) 1000;
    v_sw_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.move_switch_timeout');
    v_max_att := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.move_switch_attempts');
    v_lock_ms := CASE WHEN v_sw_ms OPERATOR(pg_catalog./) 10 OPERATOR(pg_catalog.>) 200 THEN v_sw_ms OPERATOR(pg_catalog./) 10 ELSE 200 END;

    -- Read-only checks first: a split that cannot start leaves nothing behind.
    PERFORM kingres.internal_check_node_versions();
    SELECT m.group_id, m.modulus, m.state INTO v_group, v_mod, v_state FROM kingres.bucket_map m
     WHERE m.colocation_id OPERATOR(pg_catalog.=) v_colo AND m.bucket OPERATOR(pg_catalog.=) v_bucket;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'bucket % of colocation group % does not exist', v_bucket, v_colo USING ERRCODE = 'undefined_object';
    END IF;
    IF v_state OPERATOR(pg_catalog.<>) 'active' THEN
        RAISE EXCEPTION 'bucket % of colocation group % is in state "%"', v_bucket, v_colo, v_state USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF v_mod OPERATOR(pg_catalog.*) v_parts OPERATOR(pg_catalog.>) 65536 THEN
        RAISE EXCEPTION 'a bucket of modulus % cannot be split in % parts: the greatest modulus of a group is 65536', v_mod, v_parts USING ERRCODE = 'program_limit_exceeded';
    END IF;
    v_why := kingres.internal_split_allowed(v_colo);
    IF v_why IS NOT NULL THEN
        RAISE EXCEPTION 'colocation group % cannot be split: %', v_colo, v_why USING ERRCODE = 'feature_not_supported';
    END IF;
    v_owner := kingres.internal_group_node(v_group);
    IF v_owner IS NULL THEN
        RAISE EXCEPTION 'shard group % has no active primary', v_group USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    v_inspect := kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_inspect(%s, %s)::pg_catalog.text', v_colo, v_bucket))::pg_catalog.jsonb;
    SELECT COALESCE(pg_catalog.sum((x OPERATOR(pg_catalog.->>) 'rows')::pg_catalog.int8), 0) INTO v_total FROM pg_catalog.jsonb_array_elements(v_inspect) x;

    -- 1. Preparing: the cluster lock, the record, the state "splitting" on every node.
    v_id := kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_split_begin(%s, %s, %s, %L, %s)', v_colo, v_bucket, v_parts, v_verify, v_total))::pg_catalog.int8;
    PERFORM kingres.internal_split_lock(v_id);
    v_stage := 1;
    BEGIN
        PERFORM kingres.internal_fault_point('split_after_preparing');
        -- 2. The children and the triggers (a lock that is not got in time is tried again).
        PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_split_set(%s, ''copying'', ''creating the children and the triggers'')', v_id));
        v_att := 0;
        LOOP
            v_att := v_att OPERATOR(pg_catalog.+) 1;
            BEGIN
                PERFORM kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_prepare(%s, %s, %s, %s, %L::pg_catalog.jsonb, %s)',
                                                      v_id, v_bucket, v_mod, v_parts, v_inspect::pg_catalog.text, v_lock_ms), v_node_ms OPERATOR(pg_catalog.+) v_lock_ms * 2);
                EXIT;
            EXCEPTION WHEN OTHERS THEN
                IF SQLERRM OPERATOR(pg_catalog.~*) 'lock timeout' AND v_att OPERATOR(pg_catalog.<) v_max_att THEN
                    PERFORM pg_catalog.pg_sleep(0.2 OPERATOR(pg_catalog.*) v_att);
                ELSE
                    RAISE;
                END IF;
            END;
        END LOOP;
        PERFORM kingres.internal_fault_point('split_after_triggers');
        -- 3. The backfill, then passes until one finds nothing to copy and nothing held by a writer.
        FOR v_pass IN 1 .. 10 LOOP
            v_ins := 0; v_skip := 0;
            FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(v_inspect) x LOOP
                v_last := NULL;
                LOOP
                    SELECT s.cancel_requested INTO v_cancel FROM kingres.bucket_split s WHERE s.split_id OPERATOR(pg_catalog.=) v_id;
                    IF v_cancel THEN
                        RAISE EXCEPTION 'split % was cancelled', v_id USING ERRCODE = 'KS0C1';	-- not query_canceled: WHEN OTHERS does not catch that one and the undo must run
                    END IF;
                    r := kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_batch(%s, %s, %s, %s, %L::pg_catalog.jsonb, %L::pg_catalog.jsonb, %s)::pg_catalog.text',
                                                      v_id, v_bucket, v_mod, v_parts, el::pg_catalog.text, v_last::pg_catalog.text, v_batch), v_node_ms OPERATOR(pg_catalog.*) 6)::pg_catalog.jsonb;
                    v_ins := v_ins OPERATOR(pg_catalog.+) (r OPERATOR(pg_catalog.->>) 'inserted')::pg_catalog.int8;
                    v_skip := v_skip OPERATOR(pg_catalog.+) (r OPERATOR(pg_catalog.->>) 'skipped')::pg_catalog.int8;
                    v_done := v_done OPERATOR(pg_catalog.+) (r OPERATOR(pg_catalog.->>) 'inserted')::pg_catalog.int8;
                    EXIT WHEN (r OPERATOR(pg_catalog.->>) 'n')::pg_catalog.int4 OPERATOR(pg_catalog.<) v_batch;
                    v_last := r OPERATOR(pg_catalog.->) 'last';
                    PERFORM kingres.internal_fault_point('split_mid_backfill');
                END LOOP;
            END LOOP;
            PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_split_set(%s, %L, %L, %s)', v_id,
                                                  CASE WHEN v_pass OPERATOR(pg_catalog.=) 1 THEN 'copying' ELSE 'catching_up' END,
                                                  pg_catalog.format('pass %s: %s rows copied, %s held by writers', v_pass, v_ins, v_skip), v_done));
            IF v_ins OPERATOR(pg_catalog.=) 0 AND v_skip OPERATOR(pg_catalog.=) 0 THEN
                v_converged := true;
                EXIT;
            END IF;
        END LOOP;
        IF NOT v_converged THEN
            RAISE EXCEPTION 'writers keep rows of bucket % locked, the copy does not converge', v_bucket USING ERRCODE = 'object_not_in_prerequisite_state';
        END IF;
        PERFORM kingres.internal_fault_point('split_after_backfill');
        -- 4. The switch.
        PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_split_set(%s, ''switching'', ''switching the partitions'')', v_id));
        v_stage := 2;
        PERFORM kingres.internal_fault_point('split_before_switch');
        v_att := 0;
        v_pause := 0.2;
        LOOP
            v_att := v_att OPERATOR(pg_catalog.+) 1;
            BEGIN
                PERFORM kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_switch(%s, %s, %s, %s, %L::pg_catalog.jsonb, %s, %L)',
                                                      v_id, v_bucket, v_mod, v_parts, v_inspect::pg_catalog.text, v_lock_ms, v_verify), v_sw_ms OPERATOR(pg_catalog.+) v_node_ms * 6);
                v_stage := 3;
                EXIT;
            EXCEPTION WHEN OTHERS THEN
                v_err := SQLERRM;
                -- an answer that did not come may be a switch that committed: ask the node what it holds
                BEGIN
                    IF kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_state(%s, %s, %s)', v_colo, v_bucket, v_mod OPERATOR(pg_catalog.*) v_parts)) OPERATOR(pg_catalog.=) 'switched' THEN
                        v_stage := 3;
                        EXIT;
                    END IF;
                EXCEPTION WHEN OTHERS THEN
                    RAISE EXCEPTION 'the switch of split % is not known: %', v_id, v_err USING ERRCODE = 'object_not_in_prerequisite_state';
                END;
                IF v_err OPERATOR(pg_catalog.~*) 'lock timeout' AND v_att OPERATOR(pg_catalog.<) v_max_att THEN
                    PERFORM pg_catalog.pg_sleep(v_pause OPERATOR(pg_catalog.*) (0.5 OPERATOR(pg_catalog.+) pg_catalog.random()));
                    v_pause := CASE WHEN v_pause OPERATOR(pg_catalog.>) 2.5 THEN 5 ELSE v_pause OPERATOR(pg_catalog.*) 2 END;
                ELSE
                    IF v_err OPERATOR(pg_catalog.~*) 'lock timeout' THEN
                        BEGIN
                            v_blockers := kingres.internal_exec_scalar(v_owner, pg_catalog.format(
                                'SELECT COALESCE(pg_catalog.string_agg(DISTINCT pg_catalog.format(''pid %%s: %%s'', l.pid, pg_catalog.left(a.query, 60)), ''; ''), '''') FROM pg_catalog.pg_locks l JOIN pg_catalog.pg_stat_activity a ON a.pid OPERATOR(pg_catalog.=) l.pid '
                                'WHERE l.locktype OPERATOR(pg_catalog.=) ''relation'' AND l.granted AND l.pid OPERATOR(pg_catalog.<>) pg_catalog.pg_backend_pid() AND l.mode IN (''RowExclusiveLock'', ''ShareUpdateExclusiveLock'', ''ShareRowExclusiveLock'', ''ExclusiveLock'', ''AccessExclusiveLock'') '
                                'AND l.relation IN (SELECT d.relid::pg_catalog.oid FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) %s)', v_colo));
                        EXCEPTION WHEN OTHERS THEN
                            v_blockers := '';
                        END;
                        RAISE EXCEPTION 'the switch of bucket % did not get its locks in % attempts: %', v_bucket, v_att, v_blockers USING ERRCODE = 'lock_not_available';
                    END IF;
                    RAISE EXCEPTION '%', v_err;
                END IF;
            END;
        END LOOP;
        PERFORM kingres.internal_fault_point('split_after_switch');
        -- 5. The catalog.
        PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_split_finish(%s)', v_id));
        v_stage := 4;
        PERFORM kingres.internal_fault_point('split_after_local');
    EXCEPTION WHEN OTHERS THEN
        v_err := SQLERRM;
        IF v_stage OPERATOR(pg_catalog.>=) 3 THEN
            -- the partitions are switched: the worker (or the next resume_bucket_splits()) writes the catalog
            RAISE;
        END IF;
        BEGIN
            PERFORM kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_undo(%s, %s, %s, %s, %s)', v_id, v_colo, v_bucket, v_mod, v_parts), v_node_ms OPERATOR(pg_catalog.*) 6);
            PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_split_fail(%s, %L, %L)', v_id, v_err, v_err OPERATOR(pg_catalog.~) 'was cancelled'));
        EXCEPTION WHEN OTHERS THEN
            NULL;       -- what could not be undone is left to resume_bucket_splits()
        END;
        RAISE;
    END;
    PERFORM pg_catalog.pg_advisory_unlock(pg_catalog.hashtext('kingres_split'), v_id::pg_catalog.int4);
    RETURN v_id;
END;
$$;
COMMENT ON FUNCTION kingres.split_bucket(int, int, int, text) IS
    'Split one bucket into parts (2, 4, 8 or 16) buckets of a larger modulus, in place and online; map group primary, superuser; returns the id of the record in kingres.bucket_split';

-- Asks a split that has not reached its switch to stop; the session that runs it undoes it between two batches.
CREATE FUNCTION kingres.cancel_bucket_split(split_id bigint)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_phase pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT s.phase INTO v_phase FROM kingres.bucket_split s WHERE s.split_id OPERATOR(pg_catalog.=) cancel_bucket_split.split_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'split % does not exist', cancel_bucket_split.split_id USING ERRCODE = 'undefined_object';
    END IF;
    IF v_phase OPERATOR(pg_catalog.=) 'switching' THEN
        RAISE EXCEPTION 'split % is switching and cannot be cancelled now', cancel_bucket_split.split_id USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF v_phase NOT IN ('preparing', 'copying', 'catching_up') THEN
        RAISE EXCEPTION 'split % is %', cancel_bucket_split.split_id, v_phase USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    UPDATE kingres.bucket_split s SET cancel_requested = true WHERE s.split_id OPERATOR(pg_catalog.=) cancel_bucket_split.split_id;
END;
$$;

-- Settles the splits whose session is gone: one that did not switch is undone, one whose partitions are switched gets its catalog.  Returns how many.
CREATE FUNCTION kingres.resume_bucket_splits()
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    s       pg_catalog.record;
    n       pg_catalog.int4 := 0;
    v_owner pg_catalog.int4;
    v_node_ms pg_catalog.int4 := (SELECT x.setting::pg_catalog.int4 FROM pg_catalog.pg_settings x WHERE x.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') OPERATOR(pg_catalog.*) 1000;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    FOR s IN SELECT b.* FROM kingres.bucket_split b WHERE b.phase IN ('preparing', 'copying', 'catching_up', 'switching') ORDER BY b.split_id LOOP
        CONTINUE WHEN kingres.internal_split_alive(s.split_id);
        BEGIN
            v_owner := kingres.internal_group_node(s.group_id);
            CONTINUE WHEN v_owner IS NULL;
            IF s.phase OPERATOR(pg_catalog.=) 'switching'
               AND kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_state(%s, %s, %s)', s.colocation_id, s.bucket, s.modulus OPERATOR(pg_catalog.*) s.parts)) OPERATOR(pg_catalog.=) 'switched' THEN
                PERFORM kingres.internal_split_finish(s.split_id);
            ELSE
                PERFORM kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_undo(%s, %s, %s, %s, %s)', s.split_id, s.colocation_id, s.bucket, s.modulus, s.parts), v_node_ms OPERATOR(pg_catalog.*) 6);
                PERFORM kingres.internal_split_fail(s.split_id, 'the session of the split ended before it switched', false);
            END IF;
            n := n OPERATOR(pg_catalog.+) 1;
        EXCEPTION WHEN OTHERS THEN
            NULL;       -- a node that does not answer: the next pass tries again
        END;
    END LOOP;
    RETURN n;
END;
$$;

-- Drops the old partitions of the splits that are finished and whose delay is over (all of them with p_force).  Returns how many splits were cleaned.
CREATE FUNCTION kingres.cleanup_bucket_splits(p_force bool DEFAULT false)
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    s pg_catalog.record;
    n pg_catalog.int4 := 0;
    v_owner pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    FOR s IN SELECT b.* FROM kingres.bucket_split b WHERE b.phase OPERATOR(pg_catalog.=) 'cleaning' AND (p_force OR b.cleanup_after OPERATOR(pg_catalog.<=) pg_catalog.clock_timestamp()) ORDER BY b.split_id LOOP
        BEGIN
            v_owner := kingres.internal_group_node(s.group_id);
            CONTINUE WHEN v_owner IS NULL;
            PERFORM kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_drop_orphans(%s)', s.split_id));
            UPDATE kingres.bucket_split b SET phase = 'done', updated_at = pg_catalog.clock_timestamp(), finished_at = pg_catalog.clock_timestamp() WHERE b.split_id OPERATOR(pg_catalog.=) s.split_id;
            n := n OPERATOR(pg_catalog.+) 1;
        EXCEPTION WHEN OTHERS THEN
            NULL;
        END;
    END LOOP;
    RETURN n;
END;
$$;

-- check_cluster(): a split that is not alive and has not moved for twice the node timeout; a capture trigger or a child table of a split that is not running.
CREATE FUNCTION kingres.internal_check_splits()
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    x      pg_catalog.record;
    n      pg_catalog.record;
    r      pg_catalog.jsonb;
    e      pg_catalog.text;
    v_kind pg_catalog.text;
    v_self pg_catalog.int4;
    v_id   pg_catalog.int8;
    v_grace pg_catalog.interval;
BEGIN
    IF pg_catalog.to_regclass('kingres.bucket_split') IS NULL THEN
        RETURN;
    END IF;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_grace := pg_catalog.make_interval(secs => 2 OPERATOR(pg_catalog.*) (SELECT s.setting::pg_catalog.float8 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout'));
    FOR x IN SELECT b.* FROM kingres.bucket_split b WHERE b.phase IN ('preparing', 'copying', 'catching_up', 'switching') AND b.updated_at OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) v_grace) ORDER BY b.split_id LOOP
        IF NOT kingres.internal_split_alive(x.split_id) THEN
            check_name := 'split_stalled'; node_id := v_self; object := pg_catalog.format('bucket split %s', x.split_id);
            detail := pg_catalog.format('the split is %s, no session runs it and it was last touched at %s; kingres.resume_bucket_splits() (the recovery worker) resolves it', x.phase, x.updated_at);
            RETURN NEXT;
        END IF;
    END LOOP;
    FOR n IN SELECT nd.node_id AS id FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active ORDER BY nd.node_id LOOP
        BEGIN
            r := kingres.internal_node_exec(n.id,
                'SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_array(k, nm)), ''[]''::pg_catalog.jsonb)::pg_catalog.text FROM ('
                'SELECT ''trigger'' AS k, p.proname::pg_catalog.text AS nm FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_proc p ON p.oid OPERATOR(pg_catalog.=) t.tgfoid WHERE t.tgname OPERATOR(pg_catalog.=) ''ks_split_capture_row'' '
                'UNION ALL SELECT ''table'', c.relname::pg_catalog.text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_description d ON d.objoid OPERATOR(pg_catalog.=) c.oid AND d.classoid OPERATOR(pg_catalog.=) ''pg_catalog.pg_class''::pg_catalog.regclass '
                'WHERE c.relkind OPERATOR(pg_catalog.=) ''r'' AND NOT c.relispartition AND d.description OPERATOR(pg_catalog.~) ''^kingres_shard bucket split [0-9]+$'') q');
            r := (((r OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0))::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            CONTINUE;
        END;
        FOR v_kind, e IN SELECT q OPERATOR(pg_catalog.->>) 0, q OPERATOR(pg_catalog.->>) 1 FROM pg_catalog.jsonb_array_elements(r) q LOOP
            v_id := NULL;
            IF e OPERATOR(pg_catalog.~) 'split_capture_[0-9]+_' THEN
                v_id := (pg_catalog.regexp_match(e, 'split_capture_([0-9]+)_'))[1]::pg_catalog.int8;
            ELSIF e OPERATOR(pg_catalog.~) '_s[0-9]+_[0-9]+' THEN
                v_id := (pg_catalog.regexp_match(e, '_s([0-9]+)_[0-9]+'))[1]::pg_catalog.int8;
            END IF;
            IF v_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM kingres.bucket_split b WHERE b.split_id OPERATOR(pg_catalog.=) v_id AND b.phase IN ('preparing', 'copying', 'catching_up', 'switching')) THEN
                check_name := CASE WHEN v_kind OPERATOR(pg_catalog.=) 'trigger' THEN 'split_trigger' ELSE 'split_child_table' END;
                node_id := n.id; object := e;
                detail := pg_catalog.format('left by split %s, which is not running', v_id);
                RETURN NEXT;
            END IF;
        END LOOP;
    END LOOP;
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_split_inspect(int, int), kingres.internal_split_prepare(bigint, int, int, int, jsonb, int),
    kingres.internal_split_batch(bigint, int, int, int, jsonb, jsonb, int), kingres.internal_split_switch(bigint, int, int, int, jsonb, int, text),
    kingres.internal_split_undo(bigint, int, int, int, int), kingres.internal_split_state(int, int, int), kingres.internal_split_drop_orphans(bigint),
    kingres.internal_split_begin(int, int, int, text, bigint), kingres.internal_split_set(bigint, text, text, bigint), kingres.internal_split_finish(bigint),
    kingres.internal_split_fail(bigint, text, bool), kingres.internal_split_lock(bigint), kingres.internal_split_alive(bigint), kingres.internal_split_allowed(int),
    kingres.split_bucket(int, int, int, text), kingres.cancel_bucket_split(bigint), kingres.resume_bucket_splits(), kingres.cleanup_bucket_splits(bool),
    kingres.internal_check_splits() FROM PUBLIC;
-- ===========================================================================
-- END OF SECTION P10
-- ===========================================================================

-- ===========================================================================
-- SECTION P11: distributed schemas (design/shard-phase11.md)
-- ===========================================================================

-- ===========================================================================
-- END OF SECTION P11
-- ===========================================================================
