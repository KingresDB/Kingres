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
CREATE FUNCTION kingres.internal_check_layout_partitions(p_skip int[] DEFAULT '{}')
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
    v_colo   pg_catalog.int4;
    v_busy   pg_catalog.int4[];
BEGIN
    IF NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.method OPERATOR(pg_catalog.=) 'hash') THEN
        RETURN;
    END IF;
    -- P10R-12: between the commit of the owner's switch and the catalog change of a split (normally milliseconds, longer when a node is down) the partitions have the new bounds and the
    -- map the old ones, and that is no damage: a colocation group with an unfinished split is not compared (a split that stopped is said by split_stalled)
    v_busy := COALESCE((SELECT pg_catalog.array_agg(b.colocation_id) FROM kingres.bucket_split b WHERE b.phase IN ('preparing', 'copying', 'catching_up', 'switching')), '{}'::pg_catalog.int4[]);
    FOR n IN SELECT nd.node_id AS id, nd.group_id AS grp FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active
                AND NOT (nd.node_id OPERATOR(pg_catalog.=) ANY (p_skip)) ORDER BY nd.node_id LOOP
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
            SELECT pg_catalog.format('FOR VALUES WITH (modulus %s, remainder %s)', m.modulus, m.bucket), m.group_id, m.colocation_id
              INTO v_want, v_group, v_colo
              FROM kingres.bucket_map m
              JOIN kingres.dist_table d ON d.colocation_id OPERATOR(pg_catalog.=) m.colocation_id
             WHERE d.relid OPERATOR(pg_catalog.=) (e OPERATOR(pg_catalog.->>) 0)::pg_catalog.regclass AND m.bucket OPERATOR(pg_catalog.=) v_bucket;
            -- a partition of a bucket that the group of the node does not own is partition_misplaced, said by the base check
            IF FOUND AND v_group OPERATOR(pg_catalog.=) n.grp AND (e OPERATOR(pg_catalog.->>) 2) IS DISTINCT FROM v_want
               AND NOT (v_colo OPERATOR(pg_catalog.=) ANY (v_busy)) THEN
                check_name := 'bucket_layout'; node_id := n.id; object := (e OPERATOR(pg_catalog.->>) 1);
                detail := pg_catalog.format('the partition has the bounds "%s" but the bucket map says "%s"', e OPERATOR(pg_catalog.->>) 2, v_want);
                RETURN NEXT;
            END IF;
        END LOOP;
    END LOOP;
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_check_layout_partitions(int[]) FROM PUBLIC;

-- On the source node of a move: the bucket partitions of the tables must have the bounds that the bucket map gives the bucket (P10R-01).  A node whose partitions are not the map's
-- (a split that was settled wrongly, a manual change) must not have its bucket copied: the target would attach the partition with the map's bounds and the rows of the other
-- children would be left behind.  Returns NULL, or a text that says what differs.
CREATE FUNCTION kingres.internal_move_bounds(p_tables pg_catalog.jsonb, p_bucket int, p_modulus int)
    RETURNS pg_catalog.text
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t      pg_catalog.record;
    v_bnd  pg_catalog.text;
    v_want pg_catalog.text := pg_catalog.format('FOR VALUES WITH (modulus %s, remainder %s)', p_modulus, p_bucket);
BEGIN
    FOR t IN SELECT x.schema_name, x.table_name FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text) LOOP
        SELECT pg_catalog.pg_get_expr(c.relpartbound, c.oid) INTO v_bnd FROM pg_catalog.pg_class c
         WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, kingres.internal_bucket_part_name(t.table_name, p_bucket)))::pg_catalog.oid;
        IF v_bnd IS DISTINCT FROM v_want THEN
            RETURN pg_catalog.format('the partition of bucket %s of %I.%I has the bounds "%s" and the bucket map says "%s"', p_bucket, t.schema_name, t.table_name, COALESCE(v_bnd, 'none'), v_want);
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_move_bounds(jsonb, int, int) FROM PUBLIC;

-- P10R-11: the check of the objects that table operations leave leaves out the nodes that the base check found not answering, as the layout and split checks do: each call to a node that
-- does not answer costs a node_timeout, and check_cluster() has a bound (README, "Bounds").
DROP FUNCTION kingres.internal_check_table_operations();
CREATE FUNCTION kingres.internal_check_table_operations(p_skip int[] DEFAULT '{}')
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
    FOR n IN SELECT nd.node_id AS id FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active AND NOT (nd.node_id OPERATOR(pg_catalog.=) ANY (p_skip)) ORDER BY nd.node_id LOOP
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
REVOKE ALL ON FUNCTION kingres.internal_check_table_operations(int[]) FROM PUBLIC;

CREATE OR REPLACE FUNCTION kingres.check_cluster(p_prepared_older_than interval DEFAULT interval '5 minutes')
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    b      pg_catalog.record;
    v_skip pg_catalog.int4[] := '{}';
BEGIN
    -- the base check counts the buckets of a group against its bucket_count; the layout check does it for groups with split buckets too.
    -- The checks of the layout and of the splits ask every node: they leave out a node that the base check found not answering (each call
    -- to such a node costs a node_timeout, and check_cluster has a bound, README "Bounds").
    FOR b IN SELECT x.check_name, x.node_id, x.object, x.detail FROM kingres.internal_check_cluster_base(p_prepared_older_than) x LOOP
        IF b.check_name OPERATOR(pg_catalog.=) 'node_unreachable' AND b.node_id IS NOT NULL THEN
            v_skip := v_skip OPERATOR(pg_catalog.||) b.node_id;
        END IF;
        IF b.check_name OPERATOR(pg_catalog.<>) 'bucket_map_incomplete' THEN
            check_name := b.check_name; node_id := b.node_id; object := b.object; detail := b.detail;
            RETURN NEXT;
        END IF;
    END LOOP;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_layout() t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_layout_partitions(v_skip) t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_splits(v_skip) t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_ledger(v_skip) t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_table_operations(v_skip) t;
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
    v_kind     pg_catalog.text;
    v_bad      pg_catalog.text;
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
    -- a group that holds a distributed schema (phase 11) has one bucket, which moves in place: the tables are the tables of the schema, the target
    -- already has them (empty), nothing is attached or detached
    SELECT c.kind INTO v_kind FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) v_colo;
    IF v_kind OPERATOR(pg_catalog.=) 'schema' THEN
        SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('schema_name', t.schema_name, 'table_name', t.table_name) ORDER BY t.ord)
          INTO v_tables FROM kingres.internal_schema_move_tables(v_colo) t;
        IF v_tables IS NULL THEN
            RAISE EXCEPTION 'the schema of colocation group % has no table to move', v_colo
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'A schema without a table is not moved by a copy; create its tables first.';
        END IF;
        IF EXISTS (SELECT 1 FROM kingres.bucket_move b WHERE b.colocation_id OPERATOR(pg_catalog.=) v_colo AND b.phase OPERATOR(pg_catalog.=) 'cleaning'
                      AND b.source_group OPERATOR(pg_catalog.=) v_target) THEN
            RAISE EXCEPTION 'the old copy of the schema of colocation group % on shard group % is still being cleaned by an earlier move', v_colo, v_target
                USING ERRCODE = 'object_in_use',
                      HINT = 'Finish the cleaning first: kingres.cleanup_moved_buckets(true).';
        END IF;
    ELSE
    SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('schema_name', t.schema_name, 'table_name', t.table_name) ORDER BY t.ord)
      INTO v_tables FROM kingres.internal_move_tables(v_colo) t;
    IF v_tables IS NULL THEN
        RAISE EXCEPTION 'colocation group % has no distributed table', v_colo
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
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

    IF v_kind OPERATOR(pg_catalog.=) 'schema' THEN
        -- the requirements of design 9.2: a key or a full replica identity on every table, no unlogged table; the shells of the target exist and are empty
        v_bad := kingres.internal_exec_scalar(v_src, pg_catalog.format('SELECT kingres.internal_schema_precheck(%s)', v_colo));
        IF v_bad IS NOT NULL THEN
            RAISE EXCEPTION 'the schema of colocation group % cannot be moved: %', v_colo, v_bad
                USING ERRCODE = 'object_not_in_prerequisite_state';
        END IF;
        v_bad := kingres.internal_exec_scalar(v_tgt, pg_catalog.format('SELECT kingres.internal_schema_shells(%s, true)', v_colo));
        IF v_bad IS NOT NULL THEN
            RAISE EXCEPTION 'table % of the schema is missing or not empty on the target node %', v_bad, v_tgt
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'The tables of a distributed schema exist on every node; the shells are empty. See kingres.check_cluster().';
        END IF;
    ELSE
    v_info := (kingres.internal_exec_scalar(v_src, pg_catalog.format(
                  'SELECT kingres.internal_move_inspect(''source'', %s, %s, %L::pg_catalog.jsonb)::pg_catalog.text',
                  v_bucket, v_modulus, v_tables::pg_catalog.text)))::pg_catalog.jsonb;
    -- the partitions of the source are the bucket map's (a split settled wrongly would leave rows behind)
    v_bad := kingres.internal_exec_scalar(v_src, pg_catalog.format('SELECT kingres.internal_move_bounds(%L::pg_catalog.jsonb, %s, %s)', v_tables::pg_catalog.text, v_bucket, v_modulus));
    IF v_bad IS NOT NULL THEN
        RAISE EXCEPTION 'bucket % of colocation group % cannot be moved: %', v_bucket, v_colo, v_bad
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'See kingres.check_cluster() (bucket_layout) and kingres.bucket_split.';
    END IF;
    PERFORM kingres.internal_exec_scalar(v_tgt, pg_catalog.format(
                  'SELECT kingres.internal_move_inspect(''target'', %s, %s, %L::pg_catalog.jsonb)::pg_catalog.text',
                  v_bucket, v_modulus, v_tables::pg_catalog.text));
    END IF;
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
        IF v_kind OPERATOR(pg_catalog.<>) 'schema' THEN
            PERFORM kingres.internal_exec_scalar(v_tgt, pg_catalog.format(
                'SELECT kingres.internal_create_move_tables(%s, %s, %s, %L::pg_catalog.jsonb)', v_id, v_bucket, v_modulus, v_info::pg_catalog.text));
        END IF;
        PERFORM kingres.internal_fault_point('move_after_target_tables');
        PERFORM kingres.internal_exec_scalar(v_src, pg_catalog.format(
            CASE WHEN v_kind OPERATOR(pg_catalog.=) 'schema'
                 THEN 'SELECT kingres.internal_create_schema_move_publication(%s, %s, %L::pg_catalog.jsonb)'
                 ELSE 'SELECT kingres.internal_create_move_publication(%s, %s, %L::pg_catalog.jsonb)' END,
            v_id, v_bucket, v_tables::pg_catalog.text));
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
           -- a group that holds a distributed schema (phase 11) is moved by move_schema, not by a plan of buckets
           AND c.kind OPERATOR(pg_catalog.=) 'hash'
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
        -- the kind of a group (phase 11): a catalog document of a map group that does not have the column is a hash group
        NEW.kind := COALESCE(NEW.kind, CASE WHEN NEW.dist_column_type IS NULL THEN 'schema' ELSE 'hash' END);
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
                        'WHERE b.partition_size IS NOT NULL GROUP BY 1 '
                        -- the one bucket of a distributed schema is its tables (P10R-09): the size is the tables' own, where the node holds them
                        'UNION ALL SELECT d.colocation_id::pg_catalog.text OPERATOR(pg_catalog.||) '':0'', pg_catalog.sum(pg_catalog.pg_total_relation_size(d.relid))::pg_catalog.int8 '
                        'FROM kingres.dist_table d WHERE d.method OPERATOR(pg_catalog.=) ''schema'' GROUP BY d.colocation_id) s',
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
                   m.state OPERATOR(pg_catalog.=) 'active' AND (p_colos IS NULL OR m.colocation_id OPERATOR(pg_catalog.=) ANY (p_colos))
                   -- the bucket of a distributed schema counts in the load of its group and is not moved by a plan of buckets (phase 11)
                   AND NOT EXISTS (SELECT 1 FROM kingres.colocation sc WHERE sc.colocation_id OPERATOR(pg_catalog.=) m.colocation_id AND sc.kind OPERATOR(pg_catalog.=) 'schema') AS mov,
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
    g_try    pg_catalog.bool[];
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
        -- the target: the receiver other than the source with the least utilization (ties: the lowest group id); when no bucket of the source brings that pair closer
        -- the next receiver in order of utilization is tried, and only when none has a bucket that helps is the source given up for the rest of the plan (P10R-07: with
        -- unequal weights a receiver of a higher utilization can take a bucket that the least utilized one cannot, because its weight is larger)
        g_try := pg_catalog.array_fill(false, ARRAY[ng]);
        best := NULL;
        LOOP
            t := NULL; v_best_u := NULL;
            FOR j IN 1 .. ng LOOP
                IF g_recv[j] AND j OPERATOR(pg_catalog.<>) s AND NOT g_try[j] THEN
                    v_u := (g_load[j] OPERATOR(pg_catalog.+) g_extra[j]) OPERATOR(pg_catalog./) g_w[j];
                    IF v_best_u IS NULL OR v_u OPERATOR(pg_catalog.<) v_best_u THEN
                        t := j; v_best_u := v_u;
                    END IF;
                END IF;
            END LOOP;
            EXIT WHEN t IS NULL;
            -- the bucket: by decreasing cost (ties: the higher bucket number, then the higher colocation id); a bucket of a receiver must
            -- bring the two utilizations closer
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
            EXIT WHEN best IS NOT NULL;
            g_try[t] := true;
        END LOOP;
        IF best IS NULL THEN
            IF v_drain AND NOT EXISTS (SELECT 1 FROM pg_catalog.generate_series(1, ng) q WHERE g_recv[q] AND q OPERATOR(pg_catalog.<>) s) THEN
                RAISE EXCEPTION 'no shard group can receive buckets' USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Set should_have_shards on a node with kingres.set_node_property().';
            END IF;
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
    v_owner  pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM pg_catalog.set_config('lock_timeout', p_lock_ms::pg_catalog.text, true);
    FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x LOOP
        v_parent := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'table');
        -- P10R-02: the children and the capture trigger belong to the owner of the table, as the bucket partitions do, and the trigger function runs with the
        -- owner's rights.  Whoever writes the table writes the old partition and so fires the trigger; the child must take the row whatever rights the writer has
        -- on it (none: the writer has them on the parent), and what the owner's constraints and defaults run as is the owner, never the superuser who splits.
        SELECT r.rolname INTO v_owner FROM pg_catalog.pg_class c JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) c.relowner
         WHERE c.oid OPERATOR(pg_catalog.=) v_parent::pg_catalog.regclass::pg_catalog.oid;
        IF NOT pg_catalog.has_schema_privilege(v_owner, (el OPERATOR(pg_catalog.->>) 'schema'), 'USAGE') THEN
            RAISE EXCEPTION 'the owner % of table % has no USAGE on its schema: the capture trigger of a split runs with the rights of the owner', v_owner, v_parent
                USING ERRCODE = 'insufficient_privilege';
        END IF;
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
            EXECUTE pg_catalog.format('ALTER TABLE %s OWNER TO %I', v_child, v_owner);
            v_del := v_del OPERATOR(pg_catalog.||) pg_catalog.format(E'        IF pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass::pg_catalog.oid, %s, %s, OLD.%s) THEN DELETE FROM %s WHERE %s; END IF;\n',
                                                                    v_parent, v_new, r, v_dist, v_child, v_pkm);
            v_ins := v_ins OPERATOR(pg_catalog.||) pg_catalog.format(E'        IF pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass::pg_catalog.oid, %s, %s, NEW.%s) THEN INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s; END IF;\n',
                                                                    v_parent, v_new, r, v_dist, v_child, v_cols, v_vals, v_pkl, v_set);
            v_trunc := v_trunc OPERATOR(pg_catalog.||) CASE WHEN v_trunc OPERATOR(pg_catalog.=) '' THEN '' ELSE ', ' END OPERATOR(pg_catalog.||) v_child;
        END LOOP;
        v_fn := pg_catalog.format('split_capture_%s_%s', p_split, el OPERATOR(pg_catalog.->>) 'ord');
        v_body := pg_catalog.format(E'BEGIN\n    IF TG_OP = ''TRUNCATE'' THEN\n        TRUNCATE %s;\n        RETURN NULL;\n    END IF;\n    IF TG_OP IN (''UPDATE'', ''DELETE'') THEN\n%s    END IF;\n    IF TG_OP IN (''INSERT'', ''UPDATE'') THEN\n%s    END IF;\n    RETURN NULL;\nEND', v_trunc, v_del, v_ins);
        EXECUTE pg_catalog.format('CREATE FUNCTION kingres.%I() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS %L', v_fn, v_body);
        EXECUTE pg_catalog.format('REVOKE ALL ON FUNCTION kingres.%I() FROM PUBLIC', v_fn);
        EXECUTE pg_catalog.format('ALTER FUNCTION kingres.%I() OWNER TO %I', v_fn, v_owner);
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
-- a writer still holds}.  The rows of the range that the batch did not take (locked by a writer, or gone since the snapshot) are looked at again in a second statement
-- with a new snapshot: those that are free now are copied, those that are gone are forgotten, only those that exist and are still locked count as held.  A row that a
-- writer inserted after the snapshot is not in the range at all (the trigger has put it in its child), so inserts at any key never keep a pass from converging (P10R-03).
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
    v_pkeq   pg_catalog.text;
    v_cond   pg_catalog.text;
    v_upper  pg_catalog.text;
    v_ctes   pg_catalog.text := '';
    v_ctes2  pg_catalog.text := '';
    v_sum    pg_catalog.text := '';
    v_sum2   pg_catalog.text := '';
    j        pg_catalog.int4;
    r        pg_catalog.int4;
    v_child  pg_catalog.text;
    v_res    pg_catalog.record;
    v_res2   pg_catalog.record;
    v_miss   pg_catalog.jsonb;
    v_held   pg_catalog.int8 := 0;
    v_ins    pg_catalog.int8;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT pg_catalog.string_agg(pg_catalog.quote_ident(c), ', '), pg_catalog.string_agg('r.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident(c), ', '),
           pg_catalog.string_agg(pg_catalog.format('b.%I OPERATOR(pg_catalog.=) p.%I', c, c), ' AND ')
      INTO v_pkl, v_pkr, v_pkeq FROM pg_catalog.jsonb_array_elements_text(p_el OPERATOR(pg_catalog.->) 'pk') c;
    v_cond := CASE WHEN p_last IS NULL THEN 'true'
                   ELSE pg_catalog.format('(%s) OPERATOR(pg_catalog.>) (SELECT %s FROM pg_catalog.jsonb_populate_record(NULL::%s, %L::pg_catalog.jsonb) r)', v_pkl, v_pkr, v_part, p_last::pg_catalog.text) END;
    FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
        r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
        v_child := pg_catalog.format('%I.%I', p_el OPERATOR(pg_catalog.->>) 'schema', pg_catalog.format('%s_s%s_%s', p_el OPERATOR(pg_catalog.->>) 'table', p_split, r));
        v_ctes := v_ctes OPERATOR(pg_catalog.||) pg_catalog.format(', i%s AS (INSERT INTO %s SELECT * FROM b WHERE pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass::pg_catalog.oid, %s, %s, %s) ON CONFLICT DO NOTHING RETURNING 1)',
                                                                  j, v_child, v_parent, v_new, r, v_dist);
        v_sum := v_sum OPERATOR(pg_catalog.||) pg_catalog.format(' OPERATOR(pg_catalog.+) (SELECT pg_catalog.count(*) FROM i%s)', j);
        v_ctes2 := v_ctes2 OPERATOR(pg_catalog.||) pg_catalog.format(', i%s AS (INSERT INTO %s SELECT * FROM m WHERE pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass::pg_catalog.oid, %s, %s, %s) ON CONFLICT DO NOTHING RETURNING 1)',
                                                                    j, v_child, v_parent, v_new, r, v_dist);
        v_sum2 := v_sum2 OPERATOR(pg_catalog.||) pg_catalog.format(' OPERATOR(pg_catalog.+) (SELECT pg_catalog.count(*) FROM i%s)', j);
    END LOOP;
    -- the range of the batch ends at its last key when it is full (the rows after it are for the next batch); the rows of the range that the batch did not take
    v_upper := pg_catalog.format('((SELECT pg_catalog.count(*) FROM b) OPERATOR(pg_catalog.<) %s OR (%s) OPERATOR(pg_catalog.<=) (SELECT %s FROM b ORDER BY %s DESC LIMIT 1))', p_batch, v_pkl, v_pkl, v_pkl);
    EXECUTE pg_catalog.format('WITH b AS (SELECT * FROM %s WHERE %s ORDER BY %s LIMIT %s FOR SHARE SKIP LOCKED)%s '
                              'SELECT (SELECT pg_catalog.count(*) FROM b) AS n, (0::pg_catalog.int8%s) AS ins, '
                              '(SELECT pg_catalog.to_jsonb(x) FROM (SELECT %s FROM b ORDER BY %s DESC LIMIT 1) x) AS lastk, '
                              '(SELECT pg_catalog.count(*) FROM %s p WHERE %s AND %s AND NOT EXISTS (SELECT 1 FROM b WHERE %s)) AS nmiss, '
                              '(SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.to_jsonb(y)), ''[]''::pg_catalog.jsonb) FROM (SELECT %s FROM %s p WHERE %s AND %s AND NOT EXISTS (SELECT 1 FROM b WHERE %s) ORDER BY %s LIMIT 200) y) AS miss',
                              v_part, v_cond, v_pkl, p_batch, v_ctes, v_sum, v_pkl, v_pkl,
                              v_part, v_cond, v_upper, v_pkeq,
                              v_pkl, v_part, v_cond, v_upper, v_pkeq, v_pkl)
       INTO v_res;
    v_ins := v_res.ins;
    v_miss := v_res.miss;
    v_held := v_res.nmiss OPERATOR(pg_catalog.-) pg_catalog.jsonb_array_length(v_miss);       -- more than the list holds: all of them count as held
    IF pg_catalog.jsonb_array_length(v_miss) OPERATOR(pg_catalog.>) 0 THEN
        EXECUTE pg_catalog.format('WITH k AS (SELECT * FROM pg_catalog.jsonb_populate_recordset(NULL::%s, %L::pg_catalog.jsonb)), '
                                  'm AS (SELECT p.* FROM %s p WHERE (%s) IN (SELECT %s FROM k) FOR SHARE SKIP LOCKED)%s '
                                  'SELECT (SELECT pg_catalog.count(*) FROM %s p WHERE (%s) IN (SELECT %s FROM k)) AS existing, (SELECT pg_catalog.count(*) FROM m) AS got, (0::pg_catalog.int8%s) AS ins',
                                  v_part, v_miss::pg_catalog.text, v_part, v_pkl, v_pkl, v_ctes2, v_part, v_pkl, v_pkl, v_sum2)
           INTO v_res2;
        v_held := v_held OPERATOR(pg_catalog.+) (v_res2.existing OPERATOR(pg_catalog.-) v_res2.got);
        v_ins := v_ins OPERATOR(pg_catalog.+) v_res2.ins;
    END IF;
    RETURN pg_catalog.jsonb_build_object('n', v_res.n, 'inserted', v_ins, 'last', v_res.lastk, 'skipped', v_held);
END;
$$;

-- On the owning node, the first half of the switch, in the transaction that the second half finishes (the coordinator sends both in one command, with the
-- statement timeout of the switch set between them): the fence, then the tables and the old partitions locked SHARE (writers wait from here to the commit, readers do
-- not).  The fence is an advisory lock that the undo of the split takes too, so that a switch and an undo of one split never run at the same time (P10R-01): whoever
-- comes second waits for the first to end and then looks at what is there.  Returns 'switched' when the partitions are switched already (a call that was retried after its
-- answer was lost, or a recovery got there first), 'locked' otherwise.  A lock that is not got within p_lock_ms, or a deadlock (this session finds it first: its deadlock
-- timeout is short), is an error and the coordinator tries again.
CREATE FUNCTION kingres.internal_split_switch_lock(p_split bigint, p_colocation int, p_bucket int, p_modulus int, p_parts int, p_inspect pg_catalog.jsonb, p_lock_ms int)
    RETURNS pg_catalog.text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    el       pg_catalog.jsonb;
    j        pg_catalog.int4;
    r        pg_catalog.int4;
    v_child  pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM pg_catalog.set_config('lock_timeout', p_lock_ms::pg_catalog.text, true);
    PERFORM pg_catalog.set_config('deadlock_timeout', '100ms', true);
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('kingres_split_switch'), p_split::pg_catalog.int4);
    IF kingres.internal_split_state(p_colocation, p_bucket, p_modulus OPERATOR(pg_catalog.*) p_parts) OPERATOR(pg_catalog.=) 'switched' THEN
        RETURN 'switched';
    END IF;
    -- an undo that ran meanwhile took the children away: there is nothing to switch to
    FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x LOOP
        FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
            r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
            v_child := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', pg_catalog.format('%s_s%s_%s', el OPERATOR(pg_catalog.->>) 'table', p_split, r));
            IF pg_catalog.to_regclass(v_child) IS NULL THEN
                RAISE EXCEPTION 'the children of split % are gone (the split was undone)', p_split USING ERRCODE = 'object_not_in_prerequisite_state';
            END IF;
        END LOOP;
    END LOOP;
    FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x LOOP
        EXECUTE pg_catalog.format('LOCK TABLE ONLY %I.%I, ONLY %I.%I IN SHARE MODE', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'table',
                                  el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'part');
        PERFORM kingres.internal_fault_point('split_switch_after_lock');
    END LOOP;
    RETURN 'locked';
END;
$$;

-- On the owning node, the second half of the switch, in the transaction of the first: with the writers out the children must equal the partition (count, bring them in
-- line once if they differ, count again), the triggers go, the old partitions are detached and the children, renamed, take their place.  Runs under the statement timeout
-- that the coordinator sets between the two halves (move_switch_timeout), so that the verification, the detach and the attach, which all run with the writers waiting,
-- have the bound the design promises (P10R-08).  An error rolls everything back.
CREATE FUNCTION kingres.internal_split_switch(p_split bigint, p_colocation int, p_bucket int, p_modulus int, p_parts int, p_inspect pg_catalog.jsonb, p_verify text)
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
    v_pkl    pg_catalog.text;
    v_set    pg_catalog.text;
    v_cols   pg_catalog.text;
    v_old    pg_catalog.int8;
    v_kids   pg_catalog.int8;
    v_n      pg_catalog.int8;
    v_target pg_catalog.text;
    v_rows   pg_catalog.int8 := 0;
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF kingres.internal_split_state(p_colocation, p_bucket, v_new) OPERATOR(pg_catalog.=) 'switched' THEN
        RETURN pg_catalog.jsonb_build_object('rows', 0, 'already', true);
    END IF;
    IF p_verify IN ('count', 'full') THEN
        FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x LOOP
            v_parent := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'table');
            v_part := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'part');
            v_dist := pg_catalog.quote_ident(el OPERATOR(pg_catalog.->>) 'dist');
            SELECT pg_catalog.string_agg(pg_catalog.format('c.%I OPERATOR(pg_catalog.=) p.%I', k, k), ' AND ') INTO v_pkm
              FROM pg_catalog.jsonb_array_elements_text(el OPERATOR(pg_catalog.->) 'pk') k;
            SELECT pg_catalog.string_agg(pg_catalog.quote_ident(k), ', ') INTO v_pkl FROM pg_catalog.jsonb_array_elements_text(el OPERATOR(pg_catalog.->) 'pk') k;
            SELECT pg_catalog.string_agg(pg_catalog.quote_ident(k), ', ') INTO v_cols FROM pg_catalog.jsonb_array_elements_text(el OPERATOR(pg_catalog.->) 'cols') k;
            -- 'full' makes the children equal in content: a row that exists with other values is brought to the partition's values (P10R-14)
            SELECT pg_catalog.string_agg(pg_catalog.format('%I = EXCLUDED.%I', k, k), ', ') INTO v_set
              FROM pg_catalog.jsonb_array_elements_text(el OPERATOR(pg_catalog.->) 'cols') k
             WHERE NOT (k OPERATOR(pg_catalog.=) ANY (ARRAY(SELECT pg_catalog.jsonb_array_elements_text(el OPERATOR(pg_catalog.->) 'pk'))));
            FOR v_n IN 1 .. 2 LOOP
                EXECUTE pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', v_part) INTO v_old;
                v_kids := 0;
                FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
                    r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
                    v_child := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', pg_catalog.format('%s_s%s_%s', el OPERATOR(pg_catalog.->>) 'table', p_split, r));
                    EXECUTE pg_catalog.format('SELECT pg_catalog.count(*) FROM %s', v_child) INTO v_target;
                    v_kids := v_kids OPERATOR(pg_catalog.+) v_target::pg_catalog.int8;
                END LOOP;
                -- 'full' brings the children in line with the partition even when the counts agree (a child can miss a row, hold a stray one or hold other values)
                EXIT WHEN v_kids OPERATOR(pg_catalog.=) v_old AND (p_verify OPERATOR(pg_catalog.=) 'count' OR v_n OPERATOR(pg_catalog.=) 2);
                IF v_n OPERATOR(pg_catalog.=) 2 THEN
                    RAISE EXCEPTION 'the children of bucket % of table % hold % rows and the partition holds %', p_bucket, v_parent, v_kids, v_old
                        USING ERRCODE = 'data_exception';
                END IF;
                FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
                    r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
                    v_child := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', pg_catalog.format('%s_s%s_%s', el OPERATOR(pg_catalog.->>) 'table', p_split, r));
                    EXECUTE pg_catalog.format('DELETE FROM %s c WHERE NOT EXISTS (SELECT 1 FROM %s p WHERE %s)', v_child, v_part, v_pkm);
                    EXECUTE pg_catalog.format('INSERT INTO %s (%s) SELECT %s FROM %s p WHERE pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass::pg_catalog.oid, %s, %s, p.%s) ON CONFLICT (%s) %s',
                                              v_child, v_cols, v_cols, v_part, v_parent, v_new, r, v_dist, v_pkl,
                                              CASE WHEN v_set IS NULL OR p_verify OPERATOR(pg_catalog.<>) 'full' THEN 'DO NOTHING' ELSE 'DO UPDATE SET ' OPERATOR(pg_catalog.||) v_set END);
                END LOOP;
            END LOOP;
            v_rows := v_rows OPERATOR(pg_catalog.+) v_old;
        END LOOP;
    END IF;
    PERFORM kingres.internal_fault_point('split_switch_after_verify');
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
    PERFORM kingres.internal_fault_point('split_switch_after_detach');
    FOR el IN SELECT x FROM pg_catalog.jsonb_array_elements(p_inspect) x ORDER BY (x OPERATOR(pg_catalog.->>) 'ord')::pg_catalog.int4 LOOP
        FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
            r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
            v_child := pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', pg_catalog.format('%s_s%s_%s', el OPERATOR(pg_catalog.->>) 'table', p_split, r));
            EXECUTE pg_catalog.format('ALTER TABLE %s RENAME TO %I', v_child, kingres.internal_bucket_part_name(el OPERATOR(pg_catalog.->>) 'table', r));
            PERFORM kingres.internal_attach_bucket(pg_catalog.format('%I.%I', el OPERATOR(pg_catalog.->>) 'schema', el OPERATOR(pg_catalog.->>) 'table')::pg_catalog.regclass, r, v_new);
        END LOOP;
    END LOOP;
    PERFORM kingres.internal_fault_point('split_switch_after_attach');
    RETURN pg_catalog.jsonb_build_object('rows', v_rows);
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

-- On the owning node: whatever a split that did not switch left (the triggers, their functions, the children); safe to run again.  One transaction, and it takes
-- the fence of the switch first (P10R-01): a switch that is running (its coordinator may be dead) is waited for, within what a switch may take, and then the partitions
-- are looked at again.  Returns 'switched' when they are switched (nothing is undone, the catalog must follow), 'undone' when the split is gone.  The locks of the
-- undo are bounded (P10R-13): a lock that is not got is an error and the caller tries later.
CREATE FUNCTION kingres.internal_split_undo(p_split bigint, p_colocation int, p_bucket int, p_modulus int, p_parts int)
    RETURNS pg_catalog.text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t       pg_catalog.record;
    v_part  pg_catalog.regclass;
    j       pg_catalog.int4;
    r       pg_catalog.int4;
    v_sw_ms pg_catalog.int4 := (SELECT x.setting::pg_catalog.int4 FROM pg_catalog.pg_settings x WHERE x.name OPERATOR(pg_catalog.=) 'kingres_shard.move_switch_timeout');
    v_lock_ms pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    v_lock_ms := CASE WHEN v_sw_ms OPERATOR(pg_catalog./) 10 OPERATOR(pg_catalog.>) 200 THEN v_sw_ms OPERATOR(pg_catalog./) 10 ELSE 200 END;
    PERFORM pg_catalog.set_config('deadlock_timeout', '100ms', true);
    PERFORM pg_catalog.set_config('lock_timeout', (v_sw_ms OPERATOR(pg_catalog.+) v_lock_ms)::pg_catalog.text, true);
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('kingres_split_switch'), p_split::pg_catalog.int4);
    PERFORM pg_catalog.set_config('lock_timeout', v_lock_ms::pg_catalog.text, true);
    IF kingres.internal_split_state(p_colocation, p_bucket, p_modulus OPERATOR(pg_catalog.*) p_parts) OPERATOR(pg_catalog.=) 'switched' THEN
        RETURN 'switched';
    END IF;
    PERFORM kingres.internal_fault_point('split_undo_after_lock');
    FOR t IN SELECT * FROM kingres.internal_move_tables(p_colocation) ORDER BY ord LOOP
        v_part := pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, kingres.internal_bucket_part_name(t.table_name, p_bucket)));
        IF v_part IS NOT NULL THEN
            PERFORM kingres.internal_ddl(pg_catalog.format('DROP TRIGGER IF EXISTS ks_split_capture_row ON %s', v_part));
            PERFORM kingres.internal_ddl(pg_catalog.format('DROP TRIGGER IF EXISTS ks_split_capture_truncate ON %s', v_part));
        END IF;
        EXECUTE pg_catalog.format('DROP FUNCTION IF EXISTS kingres.%I()', pg_catalog.format('split_capture_%s_%s', p_split, t.ord));
    END LOOP;
    PERFORM kingres.internal_fault_point('split_undo_after_triggers');
    FOR t IN SELECT * FROM kingres.internal_move_tables(p_colocation) ORDER BY ord LOOP
        FOR j IN 0 .. p_parts OPERATOR(pg_catalog.-) 1 LOOP
            r := p_bucket OPERATOR(pg_catalog.+) (j OPERATOR(pg_catalog.*) p_modulus);
            EXECUTE pg_catalog.format('DROP TABLE IF EXISTS %I.%I', t.schema_name, pg_catalog.format('%s_s%s_%s', t.table_name, p_split, r));
        END LOOP;
    END LOOP;
    PERFORM kingres.internal_fault_point('split_undo_after_children');
    RETURN 'undone';
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
-- and brings the children in line when they differ; 'full' brings them in line always (key by key, both ways, values included: it costs a pass over the bucket under the lock);
-- 'none' trusts the triggers and the passes.  The batch of 5000 rows, the 10 passes and the 16 parts at most are constants of this function; the attempts of the switch and its
-- bounds are the settings of the bucket move (move_switch_attempts, move_switch_timeout, README "Splitting a bucket").
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
    v_sqlstate pg_catalog.text;
    v_cancel   pg_catalog.bool;
    v_pause    pg_catalog.float8;
    v_blockers pg_catalog.text;
    v_res      pg_catalog.text;
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
    -- P10R-05: the partition of bucket 12288 is named <table>_b12288 by the code of the extension and was named <table>_b1228 by the SQL functions (lpad cuts); no group has such a
    -- bucket, because a group starts with at most 4096 and the names of the buckets of a split are the numbers 0 to 9999 until this limit is lifted
    IF v_bucket OPERATOR(pg_catalog.+) ((v_parts OPERATOR(pg_catalog.-) 1) OPERATOR(pg_catalog.*) v_mod) OPERATOR(pg_catalog.>=) 10000 THEN
        RAISE EXCEPTION 'bucket % of modulus % cannot be split in % parts: the new buckets would be numbered %, and bucket numbers are limited to 9999 (the names of the partitions of larger numbers are not the same in every part of the extension)',
                        v_bucket, v_mod, v_parts, v_bucket OPERATOR(pg_catalog.+) ((v_parts OPERATOR(pg_catalog.-) 1) OPERATOR(pg_catalog.*) v_mod) USING ERRCODE = 'program_limit_exceeded';
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
            ELSIF v_skip OPERATOR(pg_catalog.>) 0 THEN
                -- a writer holds rows: it needs time to end, the passes are not for spinning (P10R-03); the ten passes then last about ten seconds
                PERFORM pg_catalog.pg_sleep(LEAST(2.0, 0.2 OPERATOR(pg_catalog.*) v_pass));
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
                -- one command, one transaction on the owner: the fence and the locks, the statement timeout of the switch (the writers wait from the first lock to the commit,
                -- and that is what move_switch_timeout bounds, P10R-08), then the verification, the detach and the attach
                PERFORM kingres.internal_exec_scalar(v_owner, pg_catalog.format(
                    'SELECT kingres.internal_split_switch_lock(%s, %s, %s, %s, %s, %L::pg_catalog.jsonb, %s); SET LOCAL statement_timeout = ''%sms''; '
                    'SELECT kingres.internal_split_switch(%s, %s, %s, %s, %s, %L::pg_catalog.jsonb, %L)::pg_catalog.text',
                    v_id, v_colo, v_bucket, v_mod, v_parts, v_inspect::pg_catalog.text, v_lock_ms, v_sw_ms,
                    v_id, v_colo, v_bucket, v_mod, v_parts, v_inspect::pg_catalog.text, v_verify), v_sw_ms OPERATOR(pg_catalog.+) v_node_ms * 6);
                v_stage := 3;
                EXIT;
            EXCEPTION WHEN OTHERS OR query_canceled THEN
                GET STACKED DIAGNOSTICS v_err = MESSAGE_TEXT, v_sqlstate = RETURNED_SQLSTATE;
                -- an answer that did not come may be a switch that committed: ask the node what it holds (the fence makes a switch that still runs finish first)
                BEGIN
                    IF kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_state(%s, %s, %s)', v_colo, v_bucket, v_mod OPERATOR(pg_catalog.*) v_parts)) OPERATOR(pg_catalog.=) 'switched' THEN
                        v_stage := 3;
                        EXIT;
                    END IF;
                EXCEPTION WHEN OTHERS THEN
                    RAISE EXCEPTION 'the switch of split % is not known: %', v_id, v_err USING ERRCODE = 'object_not_in_prerequisite_state';
                END;
                -- nothing was changed by a switch that waited too long for a lock, found a deadlock or ran past its time: it is tried again (P10R-04)
                IF (v_sqlstate IN ('55P03', '40P01') OR (v_sqlstate OPERATOR(pg_catalog.=) '57014' AND v_err OPERATOR(pg_catalog.~) 'statement timeout')) AND v_att OPERATOR(pg_catalog.<) v_max_att THEN
                    PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_split_set(%s, ''switching'', %L)', v_id,
                                                          pg_catalog.format('switch attempt %s gave up: %s', v_att, pg_catalog.left(v_err, 200))));
                    PERFORM pg_catalog.pg_sleep(v_pause OPERATOR(pg_catalog.*) (0.5 OPERATOR(pg_catalog.+) pg_catalog.random()));
                    v_pause := CASE WHEN v_pause OPERATOR(pg_catalog.>) 2.5 THEN 5 ELSE v_pause OPERATOR(pg_catalog.*) 2 END;
                ELSIF v_sqlstate = '55P03' THEN
                    BEGIN
                        v_blockers := kingres.internal_exec_scalar(v_owner, pg_catalog.format(
                            'SELECT COALESCE(pg_catalog.string_agg(DISTINCT pg_catalog.format(''pid %%s: %%s'', l.pid, pg_catalog.left(a.query, 60)), ''; ''), '''') FROM pg_catalog.pg_locks l JOIN pg_catalog.pg_stat_activity a ON a.pid OPERATOR(pg_catalog.=) l.pid '
                            'WHERE l.locktype OPERATOR(pg_catalog.=) ''relation'' AND l.granted AND l.pid OPERATOR(pg_catalog.<>) pg_catalog.pg_backend_pid() AND l.mode IN (''RowExclusiveLock'', ''ShareUpdateExclusiveLock'', ''ShareRowExclusiveLock'', ''ExclusiveLock'', ''AccessExclusiveLock'') '
                            'AND l.relation IN (SELECT d.relid::pg_catalog.oid FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) %s)', v_colo));
                    EXCEPTION WHEN OTHERS THEN
                        v_blockers := '';
                    END;
                    RAISE EXCEPTION 'the switch of bucket % did not get its locks in % attempts: %', v_bucket, v_att, v_blockers USING ERRCODE = 'lock_not_available';
                ELSIF v_sqlstate IN ('40P01', '57014') THEN
                    RAISE EXCEPTION 'the switch of bucket % did not finish in % attempts: %', v_bucket, v_att, v_err USING ERRCODE = 'lock_not_available';
                ELSE
                    RAISE EXCEPTION '%', v_err USING ERRCODE = v_sqlstate;
                END IF;
            END;
        END LOOP;
        PERFORM kingres.internal_fault_point('split_after_switch');
        -- 5. The catalog.
        PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_split_finish(%s)', v_id));
        v_stage := 4;
        PERFORM kingres.internal_fault_point('split_after_local');
    EXCEPTION WHEN OTHERS OR query_canceled THEN
        v_err := SQLERRM;
        IF v_stage OPERATOR(pg_catalog.>=) 3 THEN
            -- the partitions are switched: the worker (or the next resume_bucket_splits()) writes the catalog; the live mark goes, so that it can (P10R-06)
            PERFORM pg_catalog.pg_advisory_unlock(pg_catalog.hashtext('kingres_split'), v_id::pg_catalog.int4);
            RAISE;
        END IF;
        BEGIN
            v_res := kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_undo(%s, %s, %s, %s, %s)', v_id, v_colo, v_bucket, v_mod, v_parts), v_sw_ms OPERATOR(pg_catalog.+) v_node_ms * 6);
            IF v_res OPERATOR(pg_catalog.=) 'switched' THEN
                -- the switch committed after all (the undo waited for it): the split is not undone, it is finished
                PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_split_finish(%s)', v_id));
                PERFORM pg_catalog.pg_advisory_unlock(pg_catalog.hashtext('kingres_split'), v_id::pg_catalog.int4);
                RETURN v_id;
            END IF;
            PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_split_fail(%s, %L, %L)', v_id, v_err, v_err OPERATOR(pg_catalog.~) 'was cancelled'));
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            NULL;       -- what could not be undone is left to resume_bucket_splits()
        END;
        PERFORM pg_catalog.pg_advisory_unlock(pg_catalog.hashtext('kingres_split'), v_id::pg_catalog.int4);
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

-- Settles the splits whose session is gone: the undo, which waits for a switch that may still be running and looks at the partitions then, says whether the split is
-- undone (the record says failed) or the partitions are switched (the catalog is written).  Returns how many (P10R-01).
CREATE FUNCTION kingres.resume_bucket_splits()
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    s       pg_catalog.record;
    n       pg_catalog.int4 := 0;
    v_owner pg_catalog.int4;
    v_res   pg_catalog.text;
    v_node_ms pg_catalog.int4 := (SELECT x.setting::pg_catalog.int4 FROM pg_catalog.pg_settings x WHERE x.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') OPERATOR(pg_catalog.*) 1000;
    v_sw_ms pg_catalog.int4 := (SELECT x.setting::pg_catalog.int4 FROM pg_catalog.pg_settings x WHERE x.name OPERATOR(pg_catalog.=) 'kingres_shard.move_switch_timeout');
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    FOR s IN SELECT b.* FROM kingres.bucket_split b WHERE b.phase IN ('preparing', 'copying', 'catching_up', 'switching') ORDER BY b.split_id LOOP
        CONTINUE WHEN kingres.internal_split_alive(s.split_id);
        BEGIN
            v_owner := kingres.internal_group_node(s.group_id);
            CONTINUE WHEN v_owner IS NULL;
            v_res := kingres.internal_exec_scalar(v_owner, pg_catalog.format('SELECT kingres.internal_split_undo(%s, %s, %s, %s, %s)', s.split_id, s.colocation_id, s.bucket, s.modulus, s.parts),
                                                  v_sw_ms OPERATOR(pg_catalog.+) v_node_ms * 6);
            IF v_res OPERATOR(pg_catalog.=) 'switched' THEN
                PERFORM kingres.internal_split_finish(s.split_id);
            ELSE
                PERFORM kingres.internal_split_fail(s.split_id, 'the session of the split ended before it switched', false);
            END IF;
            n := n OPERATOR(pg_catalog.+) 1;
        EXCEPTION WHEN OTHERS THEN
            NULL;       -- a node that does not answer, a lock that is not got: the next pass tries again
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
CREATE FUNCTION kingres.internal_check_splits(p_skip int[] DEFAULT '{}')
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
    FOR n IN SELECT nd.node_id AS id FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active
                AND NOT (nd.node_id OPERATOR(pg_catalog.=) ANY (p_skip)) ORDER BY nd.node_id LOOP
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
    kingres.internal_split_batch(bigint, int, int, int, jsonb, jsonb, int), kingres.internal_split_switch(bigint, int, int, int, int, jsonb, text), kingres.internal_split_switch_lock(bigint, int, int, int, int, jsonb, int),
    kingres.internal_split_undo(bigint, int, int, int, int), kingres.internal_split_state(int, int, int), kingres.internal_split_drop_orphans(bigint),
    kingres.internal_split_begin(int, int, int, text, bigint), kingres.internal_split_set(bigint, text, text, bigint), kingres.internal_split_finish(bigint),
    kingres.internal_split_fail(bigint, text, bool), kingres.internal_split_lock(bigint), kingres.internal_split_alive(bigint), kingres.internal_split_allowed(int),
    kingres.split_bucket(int, int, int, text), kingres.cancel_bucket_split(bigint), kingres.resume_bucket_splits(), kingres.cleanup_bucket_splits(bool),
    kingres.internal_check_splits(int[]) FROM PUBLIC;
-- ===========================================================================
-- END OF SECTION P10
-- ===========================================================================

-- ===========================================================================
-- SECTION P11: distributed schemas (design/shard-phase11.md)
-- ===========================================================================

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
                                       'ship_functions', s.ship_functions)
                                     ORDER BY ns.nspname)
                              FROM kingres.dist_schema s
                              JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) s.nsp::pg_catalog.oid),
                           '[]'::pg_catalog.jsonb),
    'sharding_settings', COALESCE((SELECT jsonb_agg(to_jsonb(g)) FROM kingres.sharding_settings g),
                                  '[]'::pg_catalog.jsonb));
$$;

-- The same function as before, with the two tables (the creation time of a schema is not in the document: every node stamps its own, so it would make the catalogs differ).  A document without them comes from a map
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
           CASE WHEN pg_catalog.current_setting('is_superuser') OPERATOR(pg_catalog.=) 'on' AND EXISTS (SELECT 1 FROM kingres.local_node l
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
           (c.relpersistence OPERATOR(pg_catalog.=) 'p' AND dt.method IS NULL
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
    IF NOT EXISTS (SELECT 1 FROM kingres.dist_schema s WHERE s.nsp OPERATOR(pg_catalog.=) pg_catalog.to_regnamespace(pg_catalog.quote_ident(p_nsp))) THEN
        RAISE EXCEPTION 'schema "%" is not a distributed schema on this node', p_nsp
            USING ERRCODE = 'undefined_object';
    END IF;
    FOR r IN SELECT c.oid::pg_catalog.regclass AS rel
               FROM pg_catalog.pg_class c
              WHERE c.relnamespace OPERATOR(pg_catalog.=) pg_catalog.to_regnamespace(pg_catalog.quote_ident(p_nsp))
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
                       FROM pg_catalog.pg_trigger t WHERE t.tgrelid OPERATOR(pg_catalog.=) p_rel::pg_catalog.oid AND NOT t.tgisinternal
                        AND t.tgname NOT LIKE 'kingres!_ledger!_%' ESCAPE '!'), ''));
$$;

CREATE FUNCTION kingres.internal_table_fingerprint(p_rel regclass)
    RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
SELECT pg_catalog.md5(kingres.internal_table_structure(p_rel));
$$;

GRANT EXECUTE ON FUNCTION kingres.internal_table_structure(regclass), kingres.internal_table_fingerprint(regclass) TO PUBLIC;

-- distribute_schema(nsp, group): the schema becomes a distributed schema of a group.  An empty schema (increment 3):
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
    v_kind   pg_catalog.text;
    v_obj    pg_catalog.text;
    v_reason pg_catalog.text;
    v_row    pg_catalog.bool;
    v_has    pg_catalog.bool := false;
    v_data   pg_catalog.bool := false;
    v_own    pg_catalog.int4;
    v_stmts  pg_catalog.text[];
    v_one    pg_catalog.text;
    v_fp     pg_catalog.text;
    nd       pg_catalog.record;
    tb       pg_catalog.record;
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
    -- what the schema holds: tables, indexes, sequences and views are in pg_class, functions in pg_proc, types in pg_type
    SELECT x.what INTO v_what
      FROM (SELECT 'relation ' || c.relname AS what FROM pg_catalog.pg_class c WHERE c.relnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
            UNION ALL SELECT 'function ' || p.proname FROM pg_catalog.pg_proc p WHERE p.pronamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
            UNION ALL SELECT 'type ' || t.typname FROM pg_catalog.pg_type t WHERE t.typnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
            UNION ALL SELECT 'operator ' || o.oprname FROM pg_catalog.pg_operator o WHERE o.oprnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
            UNION ALL SELECT 'collation ' || co.collname FROM pg_catalog.pg_collation co WHERE co.collnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid) x
     ORDER BY x.what LIMIT 1;
    IF v_what IS NOT NULL THEN
        -- the conversion of a schema that has objects (design 8.1): what the structure generator does not make is refused first
        v_has := true;
        SELECT i.object_kind, i.object_name, i.reason INTO v_kind, v_obj, v_reason
          FROM kingres.schema_inventory(nsp) i WHERE NOT i.supported ORDER BY i.object_kind, i.object_name LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'schema "%" cannot be distributed: % %: %', v_name, v_kind, v_obj, v_reason
                USING ERRCODE = 'feature_not_supported',
                      HINT = 'kingres.schema_inventory() lists every object of the schema and whether it is supported.';
        END IF;
        SELECT y.what INTO v_what
          FROM (SELECT 'operator ' || o.oprname AS what FROM pg_catalog.pg_operator o WHERE o.oprnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
                UNION ALL SELECT 'collation ' || co.collname FROM pg_catalog.pg_collation co WHERE co.collnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
                UNION ALL SELECT 'operator class ' || oc.opcname FROM pg_catalog.pg_opclass oc WHERE oc.opcnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
                UNION ALL SELECT 'operator family ' || opf.opfname FROM pg_catalog.pg_opfamily opf WHERE opf.opfnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
                UNION ALL SELECT 'text search object ' || ts.cfgname FROM pg_catalog.pg_ts_config ts WHERE ts.cfgnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
                UNION ALL SELECT 'text search object ' || td.dictname FROM pg_catalog.pg_ts_dict td WHERE td.dictnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
                UNION ALL SELECT 'conversion ' || cv.conname FROM pg_catalog.pg_conversion cv WHERE cv.connamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid
                UNION ALL SELECT 'table inheritance of ' || ic.relname
                            FROM pg_catalog.pg_inherits ih JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) ih.inhrelid
                            JOIN pg_catalog.pg_class pc ON pc.oid OPERATOR(pg_catalog.=) ih.inhparent
                           WHERE ic.relnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid AND ic.relkind IN ('r', 'p') AND pc.relkind OPERATOR(pg_catalog.=) 'r') y
         ORDER BY y.what LIMIT 1;
        IF v_what IS NOT NULL THEN
            RAISE EXCEPTION 'schema "%" cannot be distributed: % is not supported in a distributed schema yet', v_name, v_what
                USING ERRCODE = 'feature_not_supported';
        END IF;
        -- no write meanwhile: the tables are locked (a read goes on) until the conversion commits
        FOR nd IN SELECT c.oid::pg_catalog.regclass AS rel FROM pg_catalog.pg_class c
                   WHERE c.relnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid AND c.relkind IN ('r', 'p') ORDER BY c.oid
        LOOP
            EXECUTE pg_catalog.format('LOCK TABLE %s IN SHARE ROW EXCLUSIVE MODE', nd.rel);
        END LOOP;
        FOR nd IN SELECT c.oid::pg_catalog.regclass AS rel FROM pg_catalog.pg_class c
                   WHERE c.relnamespace OPERATOR(pg_catalog.=) nsp::pg_catalog.oid AND c.relkind OPERATOR(pg_catalog.=) 'r' ORDER BY c.oid
        LOOP
            EXECUTE pg_catalog.format('SELECT EXISTS (SELECT 1 FROM %s)', nd.rel) INTO v_row;
            IF v_row THEN
                v_data := true;
                EXIT;
            END IF;
        END LOOP;
    END IF;
    v_max := pg_catalog.current_setting('kingres_shard.max_distributed_schemas')::pg_catalog.int4;
    IF (SELECT pg_catalog.count(*) FROM kingres.dist_schema) OPERATOR(pg_catalog.>=) v_max THEN
        RAISE EXCEPTION 'the database already has % distributed schemas, the limit of kingres_shard.max_distributed_schemas', v_max
            USING ERRCODE = 'program_limit_exceeded';
    END IF;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    IF v_data THEN
        -- rows stay where they are: this node's group is the owner (design 8.1); another group is a move afterwards
        SELECT n.group_id INTO v_own FROM kingres.node n WHERE n.node_id OPERATOR(pg_catalog.=) v_self;
        IF v_group IS NOT NULL AND v_group OPERATOR(pg_catalog.<>) v_own THEN
            RAISE EXCEPTION 'schema "%" holds rows on shard group %, not on group %', v_name, v_own, v_group
                USING ERRCODE = 'feature_not_supported',
                      HINT = pg_catalog.format('Distribute the schema in place (kingres.distribute_schema(%L)), then kingres.move_schema() it to group %s.', v_name, v_group);
        END IF;
        v_group := v_own;
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
    v_coloc := pg_catalog.nextval('kingres.colocation_id_seq');
    IF v_has THEN
        v_stmts := kingres.internal_schema_structure(nsp);
    ELSE
        v_stmts := kingres.internal_schema_head(nsp);
    END IF;
    PERFORM kingres.internal_register_schema(v_name, v_coloc, v_group);
    PERFORM kingres.internal_register_schema_tables(v_name);
    FOR nd IN SELECT n.node_id FROM kingres.node n
               WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND n.node_id OPERATOR(pg_catalog.<>) v_self
               ORDER BY n.node_id
    LOOP
        PERFORM kingres.internal_branch_exec(nd.node_id, pg_catalog.format('CREATE SCHEMA %I AUTHORIZATION %I', v_name, v_owner));
        FOREACH v_one IN ARRAY v_stmts LOOP
            PERFORM kingres.internal_branch_exec(nd.node_id, v_one);
        END LOOP;
        PERFORM kingres.internal_branch_exec(nd.node_id, pg_catalog.format('SELECT kingres.internal_register_schema(%L, %s, %s)', v_name, v_coloc, v_group));
        IF v_has THEN
            PERFORM kingres.internal_branch_exec(nd.node_id, pg_catalog.format('SELECT kingres.internal_register_schema_tables(%L)', v_name));
            -- the same structure on the other node, table by table
            FOR tb IN SELECT d.relid::pg_catalog.regclass AS rel FROM kingres.dist_table d
                       WHERE d.colocation_id OPERATOR(pg_catalog.=) v_coloc ORDER BY d.relid::pg_catalog.oid
            LOOP
                v_fp := kingres.internal_branch_scalar(nd.node_id,
                            pg_catalog.format('SELECT kingres.internal_table_fingerprint(%L::pg_catalog.regclass)', tb.rel::pg_catalog.text));
                IF v_fp IS DISTINCT FROM kingres.internal_table_fingerprint(tb.rel) THEN
                    RAISE EXCEPTION 'table % has another structure on node %: the conversion is rolled back', tb.rel, nd.node_id
                        USING ERRCODE = 'object_not_in_prerequisite_state',
                              HINT = 'Something in the table is not carried by the structure of the conversion (for example an index of a partition that has a name of its own).';
                END IF;
            END LOOP;
        END IF;
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
CREATE FUNCTION kingres.internal_schema_delta(op text, nsp name, colocation_id int DEFAULT NULL, group_id int DEFAULT NULL, rel text DEFAULT NULL)
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

-- A group that holds a distributed schema has one bucket and is never split (the hook of phase 10, SECTION P10):
-- the reason, or NULL for a group that may be split.
CREATE OR REPLACE FUNCTION kingres.internal_split_allowed(p_colocation int)
    RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT CASE WHEN EXISTS (SELECT 1 FROM kingres.colocation c
                          WHERE c.colocation_id OPERATOR(pg_catalog.=) p_colocation AND c.kind OPERATOR(pg_catalog.=) 'schema')
            THEN 'colocation group ' OPERATOR(pg_catalog.||) p_colocation OPERATOR(pg_catalog.||) ' holds a distributed schema: it has one bucket and is never split'
       END::pg_catalog.text;
$$;

-- =========================================================================
-- PHASE 11, increment 3                                                      [end]
-- =========================================================================


-- =========================================================================
-- PHASE 11 (distributed schemas), increment 5: move_schema   [begin]
--
-- A distributed schema moves like a bucket (design/shard-phase11.md section 9): the same record (kingres.bucket_move with bucket 0), the same phases, the same
-- replication and the same switch (src/bucket_move.c), except that the tables stay where they are: the target has them, empty, and the copy fills them; the
-- switch changes the owner in the catalog and the sequences go over; the old copy is emptied (cleaning) instead of the orphan tables being dropped.
-- move_bucket() is the one function that runs it; move_schema() is the name of the design.
-- =========================================================================

-- The tables that the replication of the schema carries: the tables that hold rows (a partitioned table holds none; its partitions are the tables).
CREATE FUNCTION kingres.internal_schema_move_tables(p_colocation int)
    RETURNS TABLE (ord int, schema_name text, table_name text)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT (pg_catalog.row_number() OVER (ORDER BY ns.nspname, c.relname))::pg_catalog.int4, ns.nspname::pg_catalog.text, c.relname::pg_catalog.text
  FROM kingres.dist_table d
  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid
  JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) c.relnamespace
 WHERE d.colocation_id OPERATOR(pg_catalog.=) p_colocation AND d.method OPERATOR(pg_catalog.=) 'schema' AND c.relkind OPERATOR(pg_catalog.=) 'r';
$$;

-- The sequences of the schema that have been used or set, with their state (read on the source under the lock of the switch): the last value
-- and whether it has been handed out (setval(s, n, false) leaves the next nextval at n).
CREATE FUNCTION kingres.internal_schema_sequence_state(p_colocation int)
    RETURNS TABLE (seq text, last_value bigint, is_called bool)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r  pg_catalog.record;
    lv pg_catalog.int8;
    ic pg_catalog.bool;
BEGIN
    FOR r IN SELECT pg_catalog.format('%I.%I', ns.nspname, c.relname) AS q, sq.seqstart AS st
               FROM kingres.dist_schema s
               JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) s.nsp::pg_catalog.oid
               JOIN pg_catalog.pg_class c ON c.relnamespace OPERATOR(pg_catalog.=) ns.oid AND c.relkind OPERATOR(pg_catalog.=) 'S'
               JOIN pg_catalog.pg_sequence sq ON sq.seqrelid OPERATOR(pg_catalog.=) c.oid
              WHERE s.colocation_id OPERATOR(pg_catalog.=) p_colocation
              ORDER BY ns.nspname, c.relname
    LOOP
        EXECUTE pg_catalog.format('SELECT last_value, is_called FROM %s', r.q) INTO lv, ic;
        IF ic OR lv OPERATOR(pg_catalog.<>) r.st THEN
            seq := r.q; last_value := lv; is_called := ic;
            RETURN NEXT;
        END IF;
    END LOOP;
END;
$$;

-- On the source: what keeps the schema from being copied by the replication, or NULL.  A table needs a key or a full replica identity (the
-- replication cannot apply an update or a delete otherwise); an unlogged table is not replicated.
CREATE FUNCTION kingres.internal_schema_precheck(p_colocation int)
    RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT COALESCE(kingres.internal_schema_pinned(p_colocation), (SELECT CASE
         WHEN c.relpersistence OPERATOR(pg_catalog.<>) 'p' THEN pg_catalog.format('table %s is not logged: it is not replicated', c.oid::pg_catalog.regclass)
         WHEN c.relreplident OPERATOR(pg_catalog.=) 'n' OR (c.relreplident OPERATOR(pg_catalog.=) 'd'
              AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid OPERATOR(pg_catalog.=) c.oid AND i.indisprimary))
              THEN pg_catalog.format('table %s has no primary key and no full replica identity', c.oid::pg_catalog.regclass)
       END
  FROM kingres.dist_table d
  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid
 WHERE d.colocation_id OPERATOR(pg_catalog.=) p_colocation AND d.method OPERATOR(pg_catalog.=) 'schema' AND c.relkind OPERATOR(pg_catalog.=) 'r'
   AND (c.relpersistence OPERATOR(pg_catalog.<>) 'p' OR c.relreplident OPERATOR(pg_catalog.=) 'n'
        OR (c.relreplident OPERATOR(pg_catalog.=) 'd'
            AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid OPERATOR(pg_catalog.=) c.oid AND i.indisprimary)))
 ORDER BY c.oid
 LIMIT 1));
$$;

-- On a node: the shells of the schema.  p_empty true: the name of the first table that has a row (or NULL when every table is empty);
-- false: every table of the schema is emptied (TRUNCATE, the routing off: the extension's own statement), and the tables are returned.
CREATE FUNCTION kingres.internal_schema_shells(p_colocation int, p_empty bool)
    RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    r      pg_catalog.record;
    v_has  pg_catalog.bool;
    v_rels pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    IF p_empty THEN
        FOR r IN SELECT pg_catalog.format('%I.%I', t.schema_name, t.table_name) AS q FROM kingres.internal_schema_move_tables(p_colocation) t ORDER BY t.ord LOOP
            EXECUTE pg_catalog.format('SELECT EXISTS (SELECT 1 FROM %s)', r.q) INTO v_has;
            IF v_has THEN
                RETURN r.q;
            END IF;
        END LOOP;
        RETURN NULL;
    END IF;
    PERFORM pg_catalog.set_config('lock_timeout', '5s', true);
    SELECT pg_catalog.string_agg(pg_catalog.format('%I.%I', t.schema_name, t.table_name), ', ' ORDER BY t.ord) INTO v_rels
      FROM kingres.internal_schema_move_tables(p_colocation) t;
    IF v_rels IS NOT NULL THEN
        EXECUTE 'TRUNCATE ' OPERATOR(pg_catalog.||) v_rels;
    END IF;
    RETURN v_rels;
END;
$$;

-- On the source: the publication of the schema's tables (the tables that hold rows, by name) and the marker table.
CREATE FUNCTION kingres.internal_create_schema_move_publication(p_move bigint, p_bucket int, p_tables jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_name pg_catalog.text := pg_catalog.format('ks_move_%s', p_move);
    v_list pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT pg_catalog.string_agg(pg_catalog.format('%I.%I', x.schema_name, x.table_name), ', ') INTO v_list
      FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text);
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_publication p WHERE p.pubname OPERATOR(pg_catalog.=) v_name) THEN
        EXECUTE pg_catalog.format('DROP PUBLICATION %I', v_name);
    END IF;
    EXECUTE pg_catalog.format('CREATE PUBLICATION %I FOR TABLE %s, kingres.move_marker WHERE (move_id = %s) '
                              'WITH (publish = ''insert, update, delete, truncate'', publish_via_partition_root = false)',
                              v_name, v_list, p_move);
END;
$$;

-- The undo and the cleaning of a move know the two kinds of group (the copies of the 0.7 functions with a part for a schema).
CREATE OR REPLACE FUNCTION kingres.internal_undo_move(p_move bigint, p_reason text, p_final text, p_owner bool)
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
    -- the shells of the target of a schema move hold what the copy had brought: empty them again (the move did not switch, so the owner is the source)
    IF EXISTS (SELECT 1 FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) m.colocation_id AND c.kind OPERATOR(pg_catalog.=) 'schema') THEN
        PERFORM kingres.internal_node_exec(v_tgt, pg_catalog.format('SELECT kingres.internal_schema_shells(%s, false)', m.colocation_id), 120000);
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


CREATE OR REPLACE FUNCTION kingres.internal_continue_cleaning(p_move bigint, p_owner bool, p_force bool)
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
        IF EXISTS (SELECT 1 FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) m.colocation_id AND c.kind OPERATOR(pg_catalog.=) 'schema') THEN
            -- the old copy of a distributed schema: its tables on the source, emptied (bounded lock wait); not when that group holds the schema again
            IF NOT EXISTS (SELECT 1 FROM kingres.bucket_map bm WHERE bm.colocation_id OPERATOR(pg_catalog.=) m.colocation_id AND bm.bucket OPERATOR(pg_catalog.=) 0
                              AND bm.group_id OPERATOR(pg_catalog.=) m.source_group) THEN
                PERFORM kingres.internal_node_exec(v_src, pg_catalog.format('SELECT kingres.internal_schema_shells(%s, false)', m.colocation_id), 60000);
            END IF;
        ELSE
            PERFORM kingres.internal_node_exec(v_src, pg_catalog.format('SELECT kingres.internal_drop_orphans(%s)', p_move), 60000);
        END IF;
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


-- The name of the design: the schema moves to a shard group, as a bucket does.  Returns the id of its row in kingres.bucket_move.
CREATE FUNCTION kingres.move_schema(nsp regnamespace, target_group int)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_colo pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT s.colocation_id INTO v_colo FROM kingres.dist_schema s WHERE s.nsp OPERATOR(pg_catalog.=) move_schema.nsp;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'schema % is not a distributed schema', nsp::pg_catalog.oid
            USING ERRCODE = 'undefined_object';
    END IF;
    RETURN kingres.move_bucket(v_colo, 0, target_group);
END;
$$;

COMMENT ON FUNCTION kingres.move_schema(regnamespace, int) IS
    'Move a distributed schema to another shard group with logical replication, in place; returns the id of its row in kingres.bucket_move (map group primary, superuser)';

REVOKE ALL ON FUNCTION
    kingres.internal_schema_move_tables(int),
    kingres.internal_schema_sequence_state(int),
    kingres.internal_schema_precheck(int),
    kingres.internal_schema_shells(int, bool),
    kingres.internal_create_schema_move_publication(bigint, int, jsonb),
    kingres.move_schema(regnamespace, int)
    FROM PUBLIC;

-- =========================================================================
-- PHASE 11, increment 5                                                      [end]
-- =========================================================================


-- =========================================================================
-- PHASE 11 (distributed schemas), increment 6: conversion of a schema with data, undistribute_schema   [begin]
--
-- distribute_schema() takes a schema that holds objects and rows (design/shard-phase11.md section 8.1): the node that runs it
-- (the map group primary) keeps the rows and becomes the owner; every other node receives the structure, empty, in the same
-- two-phase transaction.  internal_schema_structure() makes the statements of that structure; undistribute_schema() undoes the
-- conversion (8.2): the owner keeps its schema and its rows, the other nodes drop their shells.
-- =========================================================================

-- The GRANT statements of an access control list (an aclitem[]; the owner's entries first, so that the entries of other
-- grantors find their grant option).  A list that is not null first takes the default privileges of PUBLIC away.
CREATE FUNCTION kingres.internal_acl_statements(p_kind text, p_qname text, p_acl aclitem[], p_owner oid)
    RETURNS text[]
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_out pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    q     pg_catalog.record;
BEGIN
    IF p_acl IS NULL THEN
        RETURN v_out;
    END IF;
    v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.format('REVOKE ALL ON %s %s FROM PUBLIC', p_kind, p_qname);
    FOR q IN
        SELECT x.grantor, x.grantee, x.is_grantable,
               pg_catalog.string_agg(x.privilege_type, ', ' ORDER BY x.privilege_type) AS privs
          FROM pg_catalog.aclexplode(p_acl) x
         GROUP BY x.grantor, x.grantee, x.is_grantable
         ORDER BY (x.grantor OPERATOR(pg_catalog.=) p_owner) DESC, x.grantor, x.grantee, x.is_grantable
    LOOP
        IF q.grantor OPERATOR(pg_catalog.<>) p_owner THEN
            v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.format('SET LOCAL ROLE %I', pg_catalog.pg_get_userbyid(q.grantor));
        END IF;
        v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.format('GRANT %s ON %s %s TO %s%s', q.privs, p_kind, p_qname,
                       CASE WHEN q.grantee OPERATOR(pg_catalog.=) 0 THEN 'PUBLIC' ELSE pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(q.grantee)) END,
                       CASE WHEN q.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END);
        IF q.grantor OPERATOR(pg_catalog.<>) p_owner THEN
            v_out := v_out OPERATOR(pg_catalog.||) 'RESET ROLE'::pg_catalog.text;
        END IF;
    END LOOP;
    RETURN v_out;
END;
$$;

-- The statements that make the structure of a schema (the schema itself exists): types, sequences, functions, tables with their
-- constraints and indexes, partitions, foreign keys, views, the functions that return a row type, triggers; owners, privileges and
-- comments.  The statements are in an order in which they run on an empty schema, and set check_function_bodies off for the
-- functions (their bodies may name tables that come later).  Schema_inventory() has refused what this function does not make.
CREATE FUNCTION kingres.internal_schema_structure(p_nsp regnamespace)
    RETURNS text[]
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_oid    pg_catalog.oid := p_nsp::pg_catalog.oid;
    v_name   pg_catalog.name;
    v_nsprow pg_catalog.pg_namespace%ROWTYPE;
    v_head   pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_types  pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_seqs   pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_fn1    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_fn2    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_tab    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_fk     pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_views  pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_trg    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    r        pg_catalog.record;
    d        pg_catalog.record;
    c        pg_catalog.record;
    qn       pg_catalog.text;
    stmt     pg_catalog.text;
    v_parent pg_catalog.text;
    v_cmt    pg_catalog.text;
BEGIN
    SELECT * INTO v_nsprow FROM pg_catalog.pg_namespace WHERE oid OPERATOR(pg_catalog.=) v_oid;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'schema % does not exist', v_oid USING ERRCODE = 'invalid_schema_name';
    END IF;
    v_name := v_nsprow.nspname;
    v_head := v_head OPERATOR(pg_catalog.||) 'SET LOCAL check_function_bodies = off'::pg_catalog.text;
    v_head := v_head OPERATOR(pg_catalog.||) kingres.internal_schema_head(p_nsp);

    -- types: enums, domains, composite types (in the order they were made, which is the order of their dependencies)
    FOR r IN
        SELECT t.* FROM pg_catalog.pg_type t
         WHERE t.typnamespace OPERATOR(pg_catalog.=) v_oid AND t.typtype IN ('e', 'd', 'c')
           AND NOT (t.typcategory OPERATOR(pg_catalog.=) 'A' AND t.typelem OPERATOR(pg_catalog.<>) 0)
           AND (t.typtype OPERATOR(pg_catalog.<>) 'c'
                OR EXISTS (SELECT 1 FROM pg_catalog.pg_class rc WHERE rc.oid OPERATOR(pg_catalog.=) t.typrelid AND rc.relkind OPERATOR(pg_catalog.=) 'c'))
         ORDER BY t.oid
    LOOP
        qn := pg_catalog.format('%I.%I', v_name, r.typname);
        IF r.typtype OPERATOR(pg_catalog.=) 'e' THEN
            v_types := v_types OPERATOR(pg_catalog.||) pg_catalog.format('CREATE TYPE %s AS ENUM (%s)', qn,
                (SELECT pg_catalog.string_agg(pg_catalog.quote_literal(e.enumlabel), ', ' ORDER BY e.enumsortorder)
                   FROM pg_catalog.pg_enum e WHERE e.enumtypid OPERATOR(pg_catalog.=) r.oid));
        ELSIF r.typtype OPERATOR(pg_catalog.=) 'd' THEN
            stmt := pg_catalog.format('CREATE DOMAIN %s AS %s', qn, pg_catalog.format_type(r.typbasetype, r.typtypmod));
            IF r.typcollation OPERATOR(pg_catalog.<>) 0
               AND r.typcollation OPERATOR(pg_catalog.<>) (SELECT bt.typcollation FROM pg_catalog.pg_type bt WHERE bt.oid OPERATOR(pg_catalog.=) r.typbasetype) THEN
                stmt := stmt OPERATOR(pg_catalog.||) pg_catalog.format(' COLLATE %I.%I',
                            (SELECT cn.nspname FROM pg_catalog.pg_collation co JOIN pg_catalog.pg_namespace cn ON cn.oid OPERATOR(pg_catalog.=) co.collnamespace WHERE co.oid OPERATOR(pg_catalog.=) r.typcollation),
                            (SELECT co.collname FROM pg_catalog.pg_collation co WHERE co.oid OPERATOR(pg_catalog.=) r.typcollation));
            END IF;
            IF r.typdefault IS NOT NULL THEN
                stmt := stmt OPERATOR(pg_catalog.||) ' DEFAULT ' OPERATOR(pg_catalog.||) r.typdefault;
            END IF;
            IF r.typnotnull THEN
                stmt := stmt OPERATOR(pg_catalog.||) ' NOT NULL';
            END IF;
            v_types := v_types OPERATOR(pg_catalog.||) stmt;
            FOR d IN SELECT k.conname, pg_catalog.pg_get_constraintdef(k.oid) AS def FROM pg_catalog.pg_constraint k
                      WHERE k.contypid OPERATOR(pg_catalog.=) r.oid AND k.contype OPERATOR(pg_catalog.=) 'c' ORDER BY k.conname
            LOOP
                v_types := v_types OPERATOR(pg_catalog.||) pg_catalog.format('ALTER DOMAIN %s ADD CONSTRAINT %I %s', qn, d.conname, d.def);
            END LOOP;
        ELSE
            v_types := v_types OPERATOR(pg_catalog.||) pg_catalog.format('CREATE TYPE %s AS (%s)', qn,
                (SELECT pg_catalog.string_agg(pg_catalog.format('%I %s', a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)), ', ' ORDER BY a.attnum)
                   FROM pg_catalog.pg_attribute a
                  WHERE a.attrelid OPERATOR(pg_catalog.=) r.typrelid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped));
        END IF;
        v_types := v_types OPERATOR(pg_catalog.||) pg_catalog.format('ALTER TYPE %s OWNER TO %I', qn, pg_catalog.pg_get_userbyid(r.typowner));
        v_types := v_types OPERATOR(pg_catalog.||) kingres.internal_acl_statements(CASE WHEN r.typtype OPERATOR(pg_catalog.=) 'd' THEN 'DOMAIN' ELSE 'TYPE' END, qn, r.typacl, r.typowner);
        SELECT ds.description INTO v_cmt FROM pg_catalog.pg_description ds
         WHERE ds.classoid OPERATOR(pg_catalog.=) 'pg_catalog.pg_type'::pg_catalog.regclass AND ds.objoid OPERATOR(pg_catalog.=) r.oid AND ds.objsubid OPERATOR(pg_catalog.=) 0;
        IF v_cmt IS NOT NULL THEN
            v_types := v_types OPERATOR(pg_catalog.||) pg_catalog.format('COMMENT ON %s %s IS %L', CASE WHEN r.typtype OPERATOR(pg_catalog.=) 'd' THEN 'DOMAIN' ELSE 'TYPE' END, qn, v_cmt);
        END IF;
    END LOOP;

    -- sequences that no column owns (the others come with their table)
    FOR r IN
        SELECT c2.oid, c2.relname, c2.relowner, c2.relacl, sq.seqtypid, sq.seqstart, sq.seqincrement, sq.seqmax, sq.seqmin, sq.seqcache, sq.seqcycle
          FROM pg_catalog.pg_class c2 JOIN pg_catalog.pg_sequence sq ON sq.seqrelid OPERATOR(pg_catalog.=) c2.oid
         WHERE c2.relnamespace OPERATOR(pg_catalog.=) v_oid AND c2.relkind OPERATOR(pg_catalog.=) 'S'
           AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend dp
                            WHERE dp.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass AND dp.objid OPERATOR(pg_catalog.=) c2.oid
                              AND dp.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass AND dp.deptype IN ('a', 'i'))
         ORDER BY c2.oid
    LOOP
        qn := pg_catalog.format('%I.%I', v_name, r.relname);
        v_seqs := v_seqs OPERATOR(pg_catalog.||) pg_catalog.format('CREATE SEQUENCE %s AS %s INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s %s',
                       qn, pg_catalog.format_type(r.seqtypid, NULL), r.seqincrement, r.seqmin, r.seqmax, r.seqstart, r.seqcache,
                       CASE WHEN r.seqcycle THEN 'CYCLE' ELSE 'NO CYCLE' END);
        v_seqs := v_seqs OPERATOR(pg_catalog.||) pg_catalog.format('ALTER SEQUENCE %s OWNER TO %I', qn, pg_catalog.pg_get_userbyid(r.relowner));
        v_seqs := v_seqs OPERATOR(pg_catalog.||) kingres.internal_acl_statements('SEQUENCE', qn, r.relacl, r.relowner);
        SELECT ds.description INTO v_cmt FROM pg_catalog.pg_description ds
         WHERE ds.classoid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass AND ds.objoid OPERATOR(pg_catalog.=) r.oid AND ds.objsubid OPERATOR(pg_catalog.=) 0;
        IF v_cmt IS NOT NULL THEN
            v_seqs := v_seqs OPERATOR(pg_catalog.||) pg_catalog.format('COMMENT ON SEQUENCE %s IS %L', qn, v_cmt);
        END IF;
    END LOOP;

    -- functions and procedures; one that names the row type of a table or a view in its arguments or result comes after them
    FOR r IN
        SELECT p.oid, p.proname, p.prokind, p.proowner, p.proacl,
               (EXISTS (SELECT 1 FROM pg_catalog.pg_type t JOIN pg_catalog.pg_class rc ON rc.oid OPERATOR(pg_catalog.=) t.typrelid
                         WHERE rc.relkind IN ('r', 'p', 'v')
                           AND t.oid OPERATOR(pg_catalog.=) ANY (ARRAY[p.prorettype] OPERATOR(pg_catalog.||) p.proargtypes::pg_catalog.oid[]
                                                                 OPERATOR(pg_catalog.||) COALESCE(p.proallargtypes, '{}'::pg_catalog.oid[])))) AS needs_rel
          FROM pg_catalog.pg_proc p
         WHERE p.pronamespace OPERATOR(pg_catalog.=) v_oid AND p.prokind IN ('f', 'p')
           AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend dp
                            WHERE dp.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_proc'::pg_catalog.regclass
                              AND dp.objid OPERATOR(pg_catalog.=) p.oid AND dp.deptype IN ('e', 'i'))
         ORDER BY p.oid
    LOOP
        qn := pg_catalog.format('%I.%I(%s)', v_name, r.proname, pg_catalog.pg_get_function_identity_arguments(r.oid));
        stmt := pg_catalog.pg_get_functiondef(r.oid);
        IF NOT r.needs_rel THEN
            v_fn1 := v_fn1 OPERATOR(pg_catalog.||) stmt;
            v_fn1 := v_fn1 OPERATOR(pg_catalog.||) pg_catalog.format('ALTER %s %s OWNER TO %I', CASE r.prokind WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END, qn, pg_catalog.pg_get_userbyid(r.proowner));
            v_fn1 := v_fn1 OPERATOR(pg_catalog.||) kingres.internal_acl_statements(CASE r.prokind WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END, qn, r.proacl, r.proowner);
        ELSE
            v_fn2 := v_fn2 OPERATOR(pg_catalog.||) stmt;
            v_fn2 := v_fn2 OPERATOR(pg_catalog.||) pg_catalog.format('ALTER %s %s OWNER TO %I', CASE r.prokind WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END, qn, pg_catalog.pg_get_userbyid(r.proowner));
            v_fn2 := v_fn2 OPERATOR(pg_catalog.||) kingres.internal_acl_statements(CASE r.prokind WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END, qn, r.proacl, r.proowner);
        END IF;
        SELECT ds.description INTO v_cmt FROM pg_catalog.pg_description ds
         WHERE ds.classoid OPERATOR(pg_catalog.=) 'pg_catalog.pg_proc'::pg_catalog.regclass AND ds.objoid OPERATOR(pg_catalog.=) r.oid AND ds.objsubid OPERATOR(pg_catalog.=) 0;
        IF v_cmt IS NOT NULL THEN
            stmt := pg_catalog.format('COMMENT ON %s %s IS %L', CASE r.prokind WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END, qn, v_cmt);
            IF r.needs_rel THEN v_fn2 := v_fn2 OPERATOR(pg_catalog.||) stmt; ELSE v_fn1 := v_fn1 OPERATOR(pg_catalog.||) stmt; END IF;
        END IF;
    END LOOP;

    -- tables: the tables that are not partitions first (a partitioned table carries its PARTITION BY), then the partitions, parents before children
    FOR r IN
        SELECT c2.oid, c2.relname, c2.relkind, c2.relispartition, c2.relreplident
          FROM pg_catalog.pg_class c2
         WHERE c2.relnamespace OPERATOR(pg_catalog.=) v_oid AND c2.relkind IN ('r', 'p')
         ORDER BY c2.relispartition, c2.oid
    LOOP
        qn := pg_catalog.format('%I.%I', v_name, r.relname);
        IF r.relispartition THEN
            SELECT pg_catalog.format('%I.%I', pn.nspname, pc.relname) INTO v_parent
              FROM pg_catalog.pg_inherits i
              JOIN pg_catalog.pg_class pc ON pc.oid OPERATOR(pg_catalog.=) i.inhparent
              JOIN pg_catalog.pg_namespace pn ON pn.oid OPERATOR(pg_catalog.=) pc.relnamespace
             WHERE i.inhrelid OPERATOR(pg_catalog.=) r.oid;
            stmt := pg_catalog.format('CREATE TABLE %s PARTITION OF %s %s', qn, v_parent,
                        (SELECT pg_catalog.pg_get_expr(c3.relpartbound, c3.oid) FROM pg_catalog.pg_class c3 WHERE c3.oid OPERATOR(pg_catalog.=) r.oid));
            IF r.relkind OPERATOR(pg_catalog.=) 'p' THEN
                stmt := stmt OPERATOR(pg_catalog.||) ' PARTITION BY ' OPERATOR(pg_catalog.||) pg_catalog.pg_get_partkeydef(r.oid);
            END IF;
            v_tab := v_tab OPERATOR(pg_catalog.||) stmt;
            FOR d IN SELECT * FROM kingres.internal_table_ddl(r.oid::pg_catalog.regclass, NULL) ORDER BY ord LOOP
                IF (d.scope OPERATOR(pg_catalog.=) 'post' AND d.stmt NOT LIKE 'SELECT kingres.internal_advance_sequence%')
                   OR (d.scope OPERATOR(pg_catalog.=) 'both' AND d.stmt LIKE 'ALTER TABLE % OWNER TO %') THEN
                    v_tab := v_tab OPERATOR(pg_catalog.||) d.stmt;
                END IF;
            END LOOP;
        ELSE
            FOR d IN SELECT * FROM kingres.internal_table_ddl(r.oid::pg_catalog.regclass, NULL) ORDER BY ord LOOP
                IF d.scope OPERATOR(pg_catalog.=) 'remote_pre' OR d.scope OPERATOR(pg_catalog.=) 'both' THEN
                    stmt := d.stmt;
                    IF r.relkind OPERATOR(pg_catalog.=) 'p' AND d.scope OPERATOR(pg_catalog.=) 'both' AND stmt LIKE 'CREATE TABLE %' THEN
                        stmt := stmt OPERATOR(pg_catalog.||) ' PARTITION BY ' OPERATOR(pg_catalog.||) pg_catalog.pg_get_partkeydef(r.oid);
                    END IF;
                    v_tab := v_tab OPERATOR(pg_catalog.||) stmt;
                ELSIF d.scope OPERATOR(pg_catalog.=) 'fk' THEN
                    v_fk := v_fk OPERATOR(pg_catalog.||) d.stmt;
                ELSIF d.scope OPERATOR(pg_catalog.=) 'post' AND d.stmt NOT LIKE 'SELECT kingres.internal_advance_sequence%' THEN
                    v_tab := v_tab OPERATOR(pg_catalog.||) d.stmt;
                END IF;
            END LOOP;
        END IF;
        IF r.relreplident OPERATOR(pg_catalog.=) 'f' THEN
            v_tab := v_tab OPERATOR(pg_catalog.||) pg_catalog.format('ALTER TABLE %s REPLICA IDENTITY FULL', qn);
        ELSIF r.relreplident OPERATOR(pg_catalog.=) 'n' THEN
            v_tab := v_tab OPERATOR(pg_catalog.||) pg_catalog.format('ALTER TABLE %s REPLICA IDENTITY NOTHING', qn);
        ELSIF r.relreplident OPERATOR(pg_catalog.=) 'i' THEN
            v_tab := v_tab OPERATOR(pg_catalog.||) pg_catalog.format('ALTER TABLE %s REPLICA IDENTITY USING INDEX %I', qn,
                        (SELECT ic.relname FROM pg_catalog.pg_index ix JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) ix.indexrelid
                          WHERE ix.indrelid OPERATOR(pg_catalog.=) r.oid AND ix.indisreplident));
        END IF;
    END LOOP;

    -- views, in the order they were made
    FOR r IN
        SELECT c2.oid, c2.relname, c2.relowner, c2.relacl, c2.reloptions
          FROM pg_catalog.pg_class c2
         WHERE c2.relnamespace OPERATOR(pg_catalog.=) v_oid AND c2.relkind OPERATOR(pg_catalog.=) 'v'
         ORDER BY c2.oid
    LOOP
        qn := pg_catalog.format('%I.%I', v_name, r.relname);
        v_views := v_views OPERATOR(pg_catalog.||) pg_catalog.format('CREATE VIEW %s%s AS %s', qn,
                        CASE WHEN r.reloptions IS NOT NULL THEN ' WITH (' OPERATOR(pg_catalog.||) pg_catalog.array_to_string(r.reloptions, ', ') OPERATOR(pg_catalog.||) ')' ELSE '' END,
                        pg_catalog.regexp_replace(pg_catalog.pg_get_viewdef(r.oid, true), ';\s*$', ''));
        v_views := v_views OPERATOR(pg_catalog.||) pg_catalog.format('ALTER VIEW %s OWNER TO %I', qn, pg_catalog.pg_get_userbyid(r.relowner));
        v_views := v_views OPERATOR(pg_catalog.||) kingres.internal_acl_statements('TABLE', qn, r.relacl, r.relowner);
        SELECT ds.description INTO v_cmt FROM pg_catalog.pg_description ds
         WHERE ds.classoid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass AND ds.objoid OPERATOR(pg_catalog.=) r.oid AND ds.objsubid OPERATOR(pg_catalog.=) 0;
        IF v_cmt IS NOT NULL THEN
            v_views := v_views OPERATOR(pg_catalog.||) pg_catalog.format('COMMENT ON VIEW %s IS %L', qn, v_cmt);
        END IF;
    END LOOP;

    -- triggers (those that a partitioned table's trigger made on its partitions come with the parent)
    FOR r IN
        SELECT g.oid, g.tgname, g.tgenabled, c2.relname
          FROM pg_catalog.pg_trigger g JOIN pg_catalog.pg_class c2 ON c2.oid OPERATOR(pg_catalog.=) g.tgrelid
         WHERE c2.relnamespace OPERATOR(pg_catalog.=) v_oid AND NOT g.tgisinternal AND g.tgparentid OPERATOR(pg_catalog.=) 0
           AND g.tgname NOT LIKE 'kingres!_ledger!_%' ESCAPE '!'
         ORDER BY g.oid
    LOOP
        v_trg := v_trg OPERATOR(pg_catalog.||) pg_catalog.pg_get_triggerdef(r.oid, true);
        IF r.tgenabled OPERATOR(pg_catalog.<>) 'O' THEN
            v_trg := v_trg OPERATOR(pg_catalog.||) pg_catalog.format('ALTER TABLE %I.%I %s TRIGGER %I', v_name, r.relname,
                        CASE r.tgenabled WHEN 'D' THEN 'DISABLE' WHEN 'A' THEN 'ENABLE ALWAYS' ELSE 'ENABLE REPLICA' END, r.tgname);
        END IF;
    END LOOP;

    RETURN v_head OPERATOR(pg_catalog.||) v_types OPERATOR(pg_catalog.||) v_seqs OPERATOR(pg_catalog.||) v_fn1 OPERATOR(pg_catalog.||) v_tab
           OPERATOR(pg_catalog.||) v_fk OPERATOR(pg_catalog.||) v_views OPERATOR(pg_catalog.||) v_fn2 OPERATOR(pg_catalog.||) v_trg
           OPERATOR(pg_catalog.||) 'RESET check_function_bodies'::pg_catalog.text;
END;
$$;

-- undistribute_schema(nsp) (design 8.2): the schema is local again.  The owner keeps its rows; every other node drops the shells (the schema
-- and what is in it) and forgets the schema; nothing is propagated (routing off, the extension's own statement).  The shells must be empty
-- and no move may be running; the check and the change are one two-phase transaction.
CREATE FUNCTION kingres.undistribute_schema(nsp regnamespace)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_variable
DECLARE
    v_name   pg_catalog.name;
    v_coloc  pg_catalog.int4;
    v_group  pg_catalog.int4;
    v_state  pg_catalog.text;
    v_self   pg_catalog.int4;
    v_first  pg_catalog.text;
    v_save   pg_catalog.text;
    v_owner_node pg_catalog.int4;
    nd       pg_catalog.record;
BEGIN
    PERFORM kingres.internal_require_superuser();
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    IF pg_catalog.current_setting('transaction_isolation') OPERATOR(pg_catalog.<>) 'read committed' THEN
        RAISE EXCEPTION 'undistribute_schema must run in a READ COMMITTED transaction'
            USING ERRCODE = 'invalid_transaction_state';
    END IF;
    SELECT ns.nspname INTO v_name FROM pg_catalog.pg_namespace ns WHERE ns.oid OPERATOR(pg_catalog.=) nsp::pg_catalog.oid;
    SELECT s.colocation_id, b.group_id, b.state INTO v_coloc, v_group, v_state
      FROM kingres.dist_schema s
      JOIN kingres.bucket_map b ON b.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND b.bucket OPERATOR(pg_catalog.=) 0
     WHERE s.nsp OPERATOR(pg_catalog.=) nsp;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'schema % is not a distributed schema', COALESCE(v_name::pg_catalog.text, nsp::pg_catalog.oid::pg_catalog.text)
            USING ERRCODE = 'undefined_object';
    END IF;
    IF v_state OPERATOR(pg_catalog.<>) 'active'
       OR EXISTS (SELECT 1 FROM kingres.bucket_move mv
                   WHERE mv.colocation_id OPERATOR(pg_catalog.=) v_coloc AND mv.phase NOT IN ('done', 'failed', 'cancelled')) THEN
        RAISE EXCEPTION 'schema "%" is being moved: wait for the move to end or cancel it', v_name
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_save := pg_catalog.current_setting('kingres_shard.enable_routing');

    -- a schema that holds a ledger table cannot be undistributed (the chain of the ledger is on the owner and no schema operation moves it)
    SELECT n.node_id INTO v_owner_node FROM kingres.node n
     WHERE n.group_id OPERATOR(pg_catalog.=) v_group AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active;
    v_first := CASE WHEN v_owner_node OPERATOR(pg_catalog.=) v_self THEN kingres.internal_schema_pinned(v_coloc)
                    ELSE kingres.internal_branch_scalar(v_owner_node, pg_catalog.format('SELECT kingres.internal_schema_pinned(%s)', v_coloc)) END;
    IF v_first IS NOT NULL THEN
        RAISE EXCEPTION '%', v_first USING ERRCODE = 'object_not_in_prerequisite_state',
              HINT = 'Only the per-shard ledger of a later phase could move a ledger with its schema.';
    END IF;

    -- the shells hold no row
    FOR nd IN SELECT n.node_id, n.group_id FROM kingres.node n
               WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active AND n.group_id OPERATOR(pg_catalog.<>) v_group
               ORDER BY n.node_id
    LOOP
        IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
            v_first := kingres.internal_schema_shells(v_coloc, true);
            PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_save, true);
        ELSE
            v_first := kingres.internal_branch_scalar(nd.node_id, pg_catalog.format('SELECT kingres.internal_schema_shells(%s, true)', v_coloc));
        END IF;
        IF v_first IS NOT NULL THEN
            RAISE EXCEPTION 'table % of schema "%" holds rows on node %: the schema cannot be undistributed', v_first, v_name, nd.node_id
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'A shell holds rows only when something wrote to it with the routing off; empty it, or move the rows, first.';
        END IF;
    END LOOP;

    -- the change, node by node
    FOR nd IN SELECT n.node_id, n.group_id FROM kingres.node n
               WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active
               ORDER BY n.node_id
    LOOP
        IF nd.node_id OPERATOR(pg_catalog.=) v_self THEN
            PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
            PERFORM kingres.internal_unregister_schema(v_name);
            IF nd.group_id OPERATOR(pg_catalog.<>) v_group THEN
                EXECUTE pg_catalog.format('DROP SCHEMA %I CASCADE', v_name);
            END IF;
            PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_save, true);
        ELSE
            PERFORM kingres.internal_branch_exec(nd.node_id, 'SET LOCAL kingres_shard.enable_routing = off');
            PERFORM kingres.internal_branch_exec(nd.node_id, pg_catalog.format('SELECT kingres.internal_unregister_schema(%L)', v_name));
            IF nd.group_id OPERATOR(pg_catalog.<>) v_group THEN
                PERFORM kingres.internal_branch_exec(nd.node_id, pg_catalog.format('DROP SCHEMA %I CASCADE', v_name));
            END IF;
        END IF;
    END LOOP;
END;
$$;

COMMENT ON FUNCTION kingres.internal_acl_statements(text, text, aclitem[], oid) IS
    'The GRANT statements of an access control list';
COMMENT ON FUNCTION kingres.internal_schema_structure(regnamespace) IS
    'The statements that make the structure of a schema on an empty schema of another node (types, sequences, functions, tables, partitions, keys, views, triggers, owners, privileges, comments)';
COMMENT ON FUNCTION kingres.undistribute_schema(regnamespace) IS
    'Make a distributed schema local again: the owner keeps the rows, the other nodes drop their empty shells (map group primary only)';

REVOKE ALL ON FUNCTION
    kingres.internal_acl_statements(text, text, aclitem[], oid),
    kingres.internal_schema_structure(regnamespace),
    kingres.undistribute_schema(regnamespace)
    FROM PUBLIC;

-- =========================================================================
-- PHASE 11, increment 6                                                      [end]
-- =========================================================================


-- =========================================================================
-- PHASE 11 (distributed schemas), step 10: single tables in and out (ALTER TABLE ... SET SCHEMA)   [begin]
--
-- design/shard-phase11.md 7.9 and 8.3.  A table moves across the border of a distributed schema when no row has to move: into a schema whose owner
-- is the group of this node (or when the table is empty), out of a schema (the owner keeps the table and its rows, the other nodes drop their shells),
-- between two schemas of one owner.  The hook (src/utility.c) asks internal_set_schema_check for the mode and the refusals, runs the statement here and
-- the node-local steps below on every other node, in the one two-phase transaction of the statement.
-- =========================================================================

-- The kind of move that ALTER TABLE p_relid SET SCHEMA p_target is, and why it is refused (err), with a hint.
--   mode 0: no distributed schema is involved; 1: into a distributed schema; 2: out of one; 3: from one to another of the same owner.
CREATE FUNCTION kingres.internal_set_schema_check(p_relid oid, p_target oid)
    RETURNS TABLE (mode int, owner_group int, err text, hint text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    c        pg_catalog.pg_class%ROWTYPE;
    v_src    pg_catalog.int4;
    v_dst    pg_catalog.int4;
    v_sg     pg_catalog.int4;
    v_dg     pg_catalog.int4;
    v_local  pg_catalog.int4;
    v_has    pg_catalog.bool;
    r        pg_catalog.record;
    v_other  pg_catalog.oid;
    v_ons    pg_catalog.oid;
    v_ref    pg_catalog.bool;
    v_save   pg_catalog.text := pg_catalog.current_setting('kingres_shard.enable_routing');
BEGIN
    mode := 0; owner_group := NULL; err := NULL; hint := NULL;
    SELECT * INTO c FROM pg_catalog.pg_class WHERE oid OPERATOR(pg_catalog.=) p_relid;
    IF NOT FOUND THEN
        RETURN NEXT;
        RETURN;
    END IF;
    SELECT s.colocation_id, b.group_id INTO v_src, v_sg
      FROM kingres.dist_schema s JOIN kingres.bucket_map b ON b.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND b.bucket OPERATOR(pg_catalog.=) 0
     WHERE s.nsp::pg_catalog.oid OPERATOR(pg_catalog.=) c.relnamespace;
    SELECT s.colocation_id, b.group_id INTO v_dst, v_dg
      FROM kingres.dist_schema s JOIN kingres.bucket_map b ON b.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND b.bucket OPERATOR(pg_catalog.=) 0
     WHERE s.nsp::pg_catalog.oid OPERATOR(pg_catalog.=) p_target;
    IF (v_src IS NULL AND v_dst IS NULL) OR c.relnamespace OPERATOR(pg_catalog.=) p_target THEN
        RETURN NEXT;
        RETURN;
    END IF;
    hint := 'Recreate the object in the other schema.';
    IF EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) p_relid AND d.method OPERATOR(pg_catalog.<>) 'schema') THEN
        err := 'a hash-distributed or reference table cannot be moved into or out of a distributed schema';
        RETURN NEXT;
        RETURN;
    END IF;
    IF c.relkind OPERATOR(pg_catalog.<>) 'r' OR c.relispartition OR c.relhassubclass
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid OPERATOR(pg_catalog.=) p_relid) THEN
        err := 'only an ordinary table (not a partitioned table, a partition or a table with inheritance) can be moved into or out of a distributed schema';
        RETURN NEXT;
        RETURN;
    END IF;
    IF c.relpersistence OPERATOR(pg_catalog.<>) 'p' THEN
        err := 'a temporary or unlogged table cannot be moved into or out of a distributed schema';
        RETURN NEXT;
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_trigger g WHERE g.tgrelid OPERATOR(pg_catalog.=) p_relid AND NOT g.tgisinternal) THEN
        err := 'a table with a trigger cannot be moved into or out of a distributed schema';
        hint := 'Drop the trigger, move the table, and create the trigger again.';
        RETURN NEXT;
        RETURN;
    END IF;
    IF v_src IS NULL THEN
        mode := 1; owner_group := v_dg;
    ELSIF v_dst IS NULL THEN
        mode := 2; owner_group := v_sg;
    ELSE
        mode := 3; owner_group := v_dg;
        IF v_sg OPERATOR(pg_catalog.<>) v_dg THEN
            err := pg_catalog.format('the schemas are held by different shard groups (%s and %s): the rows would have to move', v_sg, v_dg);
            hint := 'Use kingres.move_schema() to bring the two schemas to one group first.';
            RETURN NEXT;
            RETURN;
        END IF;
    END IF;
    -- foreign keys: none may cross the border that the table crosses
    FOR r IN SELECT k.conrelid, k.confrelid, k.conname FROM pg_catalog.pg_constraint k
              WHERE k.contype OPERATOR(pg_catalog.=) 'f' AND (k.conrelid OPERATOR(pg_catalog.=) p_relid OR k.confrelid OPERATOR(pg_catalog.=) p_relid)
    LOOP
        v_other := CASE WHEN r.conrelid OPERATOR(pg_catalog.=) p_relid THEN r.confrelid ELSE r.conrelid END;
        IF v_other OPERATOR(pg_catalog.=) p_relid THEN
            CONTINUE;
        END IF;
        SELECT oc.relnamespace INTO v_ons FROM pg_catalog.pg_class oc WHERE oc.oid OPERATOR(pg_catalog.=) v_other;
        v_ref := EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) v_other AND d.method OPERATOR(pg_catalog.=) 'reference');
        IF mode = 1 AND r.conrelid OPERATOR(pg_catalog.=) p_relid AND (v_ons OPERATOR(pg_catalog.=) p_target OR v_ref) THEN
            CONTINUE;
        ELSIF mode = 3 AND v_ons OPERATOR(pg_catalog.<>) c.relnamespace AND (v_ons OPERATOR(pg_catalog.=) p_target OR v_ref) THEN
            CONTINUE;
        ELSIF mode = 2 AND v_ons OPERATOR(pg_catalog.<>) c.relnamespace THEN
            CONTINUE;
        END IF;
        err := pg_catalog.format('the foreign key "%s" would cross the border of the distributed schema', r.conname);
        hint := 'Drop the foreign key, or move the tables it joins together.';
        RETURN NEXT;
        RETURN;
    END LOOP;
    -- into a schema of another group: the rows must be on the owner, and they are here
    IF mode = 1 THEN
        SELECT n.group_id INTO v_local FROM kingres.local_node l JOIN kingres.node n ON n.node_id OPERATOR(pg_catalog.=) l.node_id;
        IF v_local IS DISTINCT FROM v_dg THEN
            PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
            EXECUTE pg_catalog.format('SELECT EXISTS (SELECT 1 FROM ONLY %s)', p_relid::pg_catalog.regclass) INTO v_has;
            PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_save, true);
            IF v_has THEN
                err := pg_catalog.format('the table holds rows on shard group %s and the schema is held by group %s', v_local, v_dg);
                hint := 'Create the table in the schema and copy the rows with INSERT ... SELECT, or empty the table first.';
                RETURN NEXT;
                RETURN;
            END IF;
        END IF;
    END IF;
    hint := NULL;
    RETURN NEXT;
END;
$$;

-- The statements that make a table of this node as a shell on another node (an ordinary table; the structure generator of the rebuild).
CREATE FUNCTION kingres.internal_table_shell_statements(p_rel regclass)
    RETURNS text[]
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    d       pg_catalog.record;
    v_out   pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_fk    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_ri    pg_catalog."char";
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR d IN SELECT * FROM kingres.internal_table_ddl(p_rel, NULL) ORDER BY ord LOOP
        IF d.scope OPERATOR(pg_catalog.=) 'remote_pre' OR d.scope OPERATOR(pg_catalog.=) 'both' THEN
            v_out := v_out OPERATOR(pg_catalog.||) d.stmt;
        ELSIF d.scope OPERATOR(pg_catalog.=) 'fk' THEN
            v_fk := v_fk OPERATOR(pg_catalog.||) d.stmt;
        ELSIF d.scope OPERATOR(pg_catalog.=) 'post' AND d.stmt NOT LIKE 'SELECT kingres.internal_advance_sequence%' THEN
            v_out := v_out OPERATOR(pg_catalog.||) d.stmt;
        END IF;
    END LOOP;
    SELECT c.relreplident INTO v_ri FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) p_rel::pg_catalog.oid;
    IF v_ri OPERATOR(pg_catalog.=) 'f' THEN
        v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.format('ALTER TABLE %s REPLICA IDENTITY FULL', p_rel);
    ELSIF v_ri OPERATOR(pg_catalog.=) 'n' THEN
        v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.format('ALTER TABLE %s REPLICA IDENTITY NOTHING', p_rel);
    END IF;
    RETURN v_out OPERATOR(pg_catalog.||) v_fk;
END;
$$;

-- Node-local, after a statement that moved a table across the border of a distributed schema: the rows of kingres.dist_table follow
-- the catalog of PostgreSQL (a table that left its schema is forgotten, a table that went to another distributed schema takes that schema's
-- colocation group, a table that came is registered).  Returns the number of rows changed.
CREATE FUNCTION kingres.internal_schema_reconcile()
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    n pg_catalog.int4 := 0;
    k pg_catalog.int4;
    r pg_catalog.record;
BEGIN
    PERFORM kingres.internal_require_superuser();
    DELETE FROM kingres.dist_table d
     WHERE d.method OPERATOR(pg_catalog.=) 'schema'
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c JOIN kingres.dist_schema s ON s.nsp::pg_catalog.oid OPERATOR(pg_catalog.=) c.relnamespace
                        WHERE c.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid);
    GET DIAGNOSTICS k = ROW_COUNT;
    n := n + k;
    UPDATE kingres.dist_table d SET colocation_id = s.colocation_id
      FROM pg_catalog.pg_class c JOIN kingres.dist_schema s ON s.nsp::pg_catalog.oid OPERATOR(pg_catalog.=) c.relnamespace
     WHERE c.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid AND d.method OPERATOR(pg_catalog.=) 'schema' AND d.colocation_id OPERATOR(pg_catalog.<>) s.colocation_id;
    GET DIAGNOSTICS k = ROW_COUNT;
    n := n + k;
    FOR r IN SELECT ns.nspname FROM kingres.dist_schema s JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) s.nsp::pg_catalog.oid LOOP
        n := n + kingres.internal_register_schema_tables(r.nspname);
    END LOOP;
    RETURN n;
END;
$$;

-- Node-local: the shell p_rel (a table of a distributed schema that another group holds) has no row.
CREATE FUNCTION kingres.internal_schema_assert_empty(p_rel text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rel  pg_catalog.regclass := pg_catalog.to_regclass(p_rel);
    v_has  pg_catalog.bool;
    v_save pg_catalog.text := pg_catalog.current_setting('kingres_shard.enable_routing');
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF v_rel IS NULL THEN
        RAISE EXCEPTION 'table % does not exist on this node', p_rel USING ERRCODE = 'undefined_table';
    END IF;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    EXECUTE pg_catalog.format('SELECT EXISTS (SELECT 1 FROM ONLY %s)', v_rel) INTO v_has;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_save, true);
    IF v_has THEN
        RAISE EXCEPTION 'table % holds rows on this node, which is not the owner of its schema: it cannot leave the distributed schema', p_rel
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
END;
$$;

-- Node-local: the shell p_rel is forgotten and dropped (the owner keeps the table and its rows).
CREATE FUNCTION kingres.internal_schema_drop_shell(p_rel text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rel  pg_catalog.regclass := pg_catalog.to_regclass(p_rel);
    v_save pg_catalog.text := pg_catalog.current_setting('kingres_shard.enable_routing');
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF v_rel IS NULL THEN
        RAISE EXCEPTION 'table % does not exist on this node', p_rel USING ERRCODE = 'undefined_table';
    END IF;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    DELETE FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_rel AND d.method OPERATOR(pg_catalog.=) 'schema';
    EXECUTE pg_catalog.format('DROP TABLE %s', v_rel);
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', v_save, true);
END;
$$;

COMMENT ON FUNCTION kingres.internal_set_schema_check(oid, oid) IS
    'The kind of move that ALTER TABLE ... SET SCHEMA is for distributed schemas (0 none, 1 in, 2 out, 3 between two of one owner), and why it is refused';
COMMENT ON FUNCTION kingres.internal_table_shell_statements(regclass) IS
    'The statements that make an ordinary table of this node as an empty shell on another node';
COMMENT ON FUNCTION kingres.internal_schema_reconcile() IS
    'Node-local: the registered tables follow the schemas they are in';
COMMENT ON FUNCTION kingres.internal_schema_assert_empty(text) IS
    'Node-local: the shell has no row (raises otherwise)';
COMMENT ON FUNCTION kingres.internal_schema_drop_shell(text) IS
    'Node-local: forget and drop a shell';

REVOKE ALL ON FUNCTION
    kingres.internal_set_schema_check(oid, oid),
    kingres.internal_table_shell_statements(regclass),
    kingres.internal_schema_reconcile(),
    kingres.internal_schema_assert_empty(text),
    kingres.internal_schema_drop_shell(text)
    FROM PUBLIC;

-- =========================================================================
-- PHASE 11, step 10                                                          [end]
-- =========================================================================

-- =========================================================================
-- PHASE 11 (distributed schemas), step 11: ledger tables   [begin]
--
-- design/shard-phase11.md 11.1.  A ledger table (kingres_ledger) may be in a distributed schema and then lives entirely on the owner: ledger_enable is run on the
-- owner's primary (kingres_ledger refuses it elsewhere), the shells are plain tables, and the schema is pinned: its ledger chain is in the schema kingres of the
-- owner, which no schema operation moves, so the schema cannot be moved or undistributed (and PostgreSQL's ledger hook refuses to drop its tables).
-- =========================================================================

-- Node-local: why the schema of the colocation group cannot move (it holds a ledger table on this node), or NULL.
CREATE FUNCTION kingres.internal_schema_pinned(p_colocation int)
    RETURNS text
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_nsp   pg_catalog.oid;
    v_name  pg_catalog.name;
    v_group pg_catalog.int4;
    v_list  pg_catalog.text;
BEGIN
    IF pg_catalog.to_regclass('kingres.ledger_table') IS NULL THEN
        RETURN NULL;
    END IF;
    SELECT s.nsp::pg_catalog.oid, ns.nspname, b.group_id INTO v_nsp, v_name, v_group
      FROM kingres.dist_schema s
      JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) s.nsp::pg_catalog.oid
      JOIN kingres.bucket_map b ON b.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND b.bucket OPERATOR(pg_catalog.=) 0
     WHERE s.colocation_id OPERATOR(pg_catalog.=) p_colocation;
    IF v_nsp IS NULL THEN
        RETURN NULL;
    END IF;
    EXECUTE 'SELECT pg_catalog.string_agg(c.relname::pg_catalog.text, '', '' ORDER BY c.relname) FROM kingres.ledger_table t '
            'JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) t.relid::pg_catalog.oid WHERE c.relnamespace OPERATOR(pg_catalog.=) $1'
       INTO v_list USING v_nsp;
    IF v_list IS NULL THEN
        RETURN NULL;
    END IF;
    RETURN pg_catalog.format('schema_pinned: schema "%s" holds the ledger table(s) %s on shard group %s; the chain of the ledger is on that group and a schema operation would leave it behind', v_name, v_list, v_group);
END;
$$;

-- Node-local: the tables of this node that kingres_ledger registered and that are in a distributed schema that another group holds, as a JSON array of names.
CREATE FUNCTION kingres.internal_ledger_misplaced()
    RETURNS pg_catalog.jsonb
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_local pg_catalog.int4;
    v_res   pg_catalog.jsonb;
BEGIN
    IF pg_catalog.to_regclass('kingres.ledger_table') IS NULL THEN
        RETURN '[]'::pg_catalog.jsonb;
    END IF;
    SELECT n.group_id INTO v_local FROM kingres.local_node l JOIN kingres.node n ON n.node_id OPERATOR(pg_catalog.=) l.node_id;
    EXECUTE 'SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.format(''%s.%s'', ns.nspname, c.relname) ORDER BY ns.nspname, c.relname), ''[]''::pg_catalog.jsonb) '
            'FROM kingres.ledger_table t JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) t.relid::pg_catalog.oid '
            'JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) c.relnamespace '
            'JOIN kingres.dist_schema s ON s.nsp::pg_catalog.oid OPERATOR(pg_catalog.=) c.relnamespace '
            'JOIN kingres.bucket_map b ON b.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND b.bucket OPERATOR(pg_catalog.=) 0 '
            'WHERE b.group_id OPERATOR(pg_catalog.<>) $1'
       INTO v_res USING v_local;
    RETURN v_res;
END;
$$;

-- check_cluster: a ledger table registered on a node that is not the owner of its schema (ledger_enable was run on a shell).
CREATE FUNCTION kingres.internal_check_ledger(p_skip int[] DEFAULT '{}')
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    n pg_catalog.record;
    r pg_catalog.jsonb;
    e pg_catalog.text;
BEGIN
    IF pg_catalog.to_regclass('kingres.dist_schema') IS NULL THEN
        RETURN;
    END IF;
    FOR n IN SELECT nd.node_id AS id FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active
                AND NOT (nd.node_id OPERATOR(pg_catalog.=) ANY (p_skip)) ORDER BY nd.node_id LOOP
        BEGIN
            r := kingres.internal_node_exec(n.id, 'SELECT kingres.internal_ledger_misplaced()::pg_catalog.text');
            r := (((r OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0))::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            CONTINUE;
        END;
        FOR e IN SELECT x FROM pg_catalog.jsonb_array_elements_text(r) x LOOP
            check_name := 'ledger_on_non_owner'; node_id := n.id; object := e;
            detail := 'the table is a ledger table of this node, and its schema is distributed and held by another shard group: its rows are on the owner, so this ledger covers none of them';
            RETURN NEXT;
        END LOOP;
    END LOOP;
END;
$$;

COMMENT ON FUNCTION kingres.internal_schema_pinned(int) IS 'Node-local: why the schema cannot move (it holds a ledger table here), or NULL';
COMMENT ON FUNCTION kingres.internal_ledger_misplaced() IS 'Node-local: the ledger tables of this node that are in a distributed schema of another group';
REVOKE ALL ON FUNCTION kingres.internal_schema_pinned(int), kingres.internal_ledger_misplaced(), kingres.internal_check_ledger(int[]) FROM PUBLIC;

-- =========================================================================
-- PHASE 11, step 11                                                          [end]
-- =========================================================================

-- =========================================================================
-- PHASE 11 (distributed schemas), the fixes for the review of increments 1 to 6   [begin]
-- =========================================================================

-- The statements that give a schema, on an empty schema of another node, its own privileges, its comment and the default privileges that
-- are set for it (ALTER DEFAULT PRIVILEGES ... IN SCHEMA): the part of the structure that exists whether or not the schema holds objects.
CREATE FUNCTION kingres.internal_schema_head(p_nsp regnamespace)
    RETURNS text[]
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_oid    pg_catalog.oid := p_nsp::pg_catalog.oid;
    ns       pg_catalog.pg_namespace%ROWTYPE;
    v_out    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    v_cmt    pg_catalog.text;
    r        pg_catalog.record;
    q        pg_catalog.record;
BEGIN
    SELECT * INTO ns FROM pg_catalog.pg_namespace WHERE oid OPERATOR(pg_catalog.=) v_oid;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'schema % does not exist', v_oid USING ERRCODE = 'invalid_schema_name';
    END IF;
    v_out := v_out OPERATOR(pg_catalog.||) kingres.internal_acl_statements('SCHEMA', pg_catalog.quote_ident(ns.nspname), ns.nspacl, ns.nspowner);
    SELECT ds.description INTO v_cmt FROM pg_catalog.pg_description ds
     WHERE ds.classoid OPERATOR(pg_catalog.=) 'pg_catalog.pg_namespace'::pg_catalog.regclass AND ds.objoid OPERATOR(pg_catalog.=) v_oid AND ds.objsubid OPERATOR(pg_catalog.=) 0;
    IF v_cmt IS NOT NULL THEN
        v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.format('COMMENT ON SCHEMA %I IS %L', ns.nspname, v_cmt);
    END IF;
    -- default privileges of the schema: for each role that set them, one ALTER DEFAULT PRIVILEGES per grantee
    FOR r IN SELECT a.defaclrole, a.defaclobjtype, a.defaclacl FROM pg_catalog.pg_default_acl a
              WHERE a.defaclnamespace OPERATOR(pg_catalog.=) v_oid ORDER BY a.oid
    LOOP
        FOR q IN SELECT x.grantee, x.is_grantable, pg_catalog.string_agg(x.privilege_type, ', ' ORDER BY x.privilege_type) AS privs
                   FROM pg_catalog.aclexplode(r.defaclacl) x
                  WHERE x.grantor OPERATOR(pg_catalog.=) r.defaclrole
                  GROUP BY x.grantee, x.is_grantable ORDER BY x.grantee, x.is_grantable
        LOOP
            v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA %I GRANT %s ON %s TO %s%s',
                        pg_catalog.pg_get_userbyid(r.defaclrole), ns.nspname, q.privs,
                        CASE r.defaclobjtype WHEN 'r' THEN 'TABLES' WHEN 'S' THEN 'SEQUENCES' WHEN 'f' THEN 'FUNCTIONS' WHEN 'T' THEN 'TYPES' ELSE 'SCHEMAS' END,
                        CASE WHEN q.grantee OPERATOR(pg_catalog.=) 0 THEN 'PUBLIC' ELSE pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(q.grantee)) END,
                        CASE WHEN q.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END);
        END LOOP;
    END LOOP;
    RETURN v_out;
END;
$$;

-- Does one of the roles own a distributed schema, or a table of one?  (REASSIGN OWNED and DROP OWNED are run on this node only.)
CREATE FUNCTION kingres.internal_roles_own_distributed(p_roles oid[])
    RETURNS bool
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT EXISTS (SELECT 1 FROM kingres.dist_schema s JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) s.nsp::pg_catalog.oid
                WHERE n.nspowner OPERATOR(pg_catalog.=) ANY (p_roles))
    OR EXISTS (SELECT 1 FROM kingres.dist_table d JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid
                WHERE c.relowner OPERATOR(pg_catalog.=) ANY (p_roles));
$$;

COMMENT ON FUNCTION kingres.internal_schema_head(regnamespace) IS 'The statements for the privileges, comment and default privileges of a schema';
COMMENT ON FUNCTION kingres.internal_roles_own_distributed(oid[]) IS 'Does one of the roles own a distributed schema or a distributed table?';
REVOKE ALL ON FUNCTION kingres.internal_schema_head(regnamespace), kingres.internal_roles_own_distributed(oid[]) FROM PUBLIC;

-- =========================================================================
-- PHASE 11, the fixes for the review of increments 1 to 6                    [end]
-- =========================================================================

-- ===========================================================================
-- END OF SECTION P11
-- ===========================================================================
