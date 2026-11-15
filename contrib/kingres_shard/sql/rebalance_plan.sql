-- SPDX-License-Identifier: PostgreSQL
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- rebalance_plan.sql: pg_regress test of kingres.rebalance_plan() against
-- hand-built bucket maps (design/shard-phase4.md, "Planning a rebalance").  The
-- catalog tables are filled by hand; nothing here contacts another node (the
-- nodes are on a port that refuses connections, and the plan only tries to read
-- the sizes of the buckets, which it skips when it cannot).
\set SHOW_CONTEXT never
SET search_path = public, pg_temp;
SET kingres_shard.node_connect_timeout = 1;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);

-- A helper that builds the situation: p_groups groups, each with one primary
-- node (node id = group + 1); the buckets of colocation group p_colo come from
-- a list of "group:first-last" ranges.
CREATE FUNCTION public.setup_nodes(p_groups int, p_noshard int[] DEFAULT '{}') RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM kingres.bucket_map;
    DELETE FROM kingres.colocation;
    DELETE FROM kingres.node WHERE node_id > 1;
    FOR g IN 1 .. p_groups - 1 LOOP
        INSERT INTO kingres.node (node_id, group_id, host, port, role)
        VALUES (g + 1, g, '127.0.0.1', g + 1, 'primary');
    END LOOP;
    UPDATE kingres.node SET should_have_shards = NOT (group_id = ANY (p_noshard));
END $$;

CREATE FUNCTION public.setup_colo(p_colo int, p_buckets int, p_layout int[]) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    -- p_layout: the group of every bucket, from bucket 0
    INSERT INTO kingres.colocation VALUES (p_colo, p_buckets, 'int4', 0);
    INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id)
    SELECT p_colo, b - 1, p_layout[b] FROM generate_series(1, p_buckets) b;
END $$;

-- Apply the plan to a copy of the map and report, per colocation group and shard
-- group, the buckets before and after, and whether the plan obeys the rules:
-- every receiving group ends within one bucket of the others, a group that does
-- not receive ends empty, only the surplus moves (the number of moves is the
-- sum of the surplus), and no bucket moves between two groups that both stay
-- within their target.
CREATE FUNCTION public.plan_report() RETURNS TABLE (colocation_id int, group_id int, before int, after int)
LANGUAGE sql AS $$
WITH plan AS (SELECT * FROM kingres.rebalance_plan()),
     groups AS (SELECT DISTINCT m.colocation_id, m.group_id FROM kingres.bucket_map m
                UNION SELECT DISTINCT m.colocation_id, p.target_group FROM kingres.bucket_map m, plan p
                       WHERE p.colocation_id = m.colocation_id)
SELECT g.colocation_id, g.group_id,
       (SELECT count(*)::int FROM kingres.bucket_map m WHERE m.colocation_id = g.colocation_id AND m.group_id = g.group_id),
       (SELECT count(*)::int FROM kingres.bucket_map m WHERE m.colocation_id = g.colocation_id AND m.group_id = g.group_id)
       - (SELECT count(*)::int FROM plan p WHERE p.colocation_id = g.colocation_id AND p.source_group = g.group_id)
       + (SELECT count(*)::int FROM plan p WHERE p.colocation_id = g.colocation_id AND p.target_group = g.group_id)
  FROM groups g ORDER BY 1, 2
$$;

CREATE FUNCTION public.plan_obeys_rules() RETURNS boolean
LANGUAGE plpgsql AS $$
DECLARE
    ok boolean := true;
    r record;
    nrecv int;
BEGIN
    SELECT count(DISTINCT group_id) INTO nrecv FROM kingres.node WHERE should_have_shards AND role = 'primary';
    FOR r IN
        SELECT c.colocation_id, c.bucket_count FROM kingres.colocation c
    LOOP
        -- the receivers end at floor or ceil of buckets / receivers, the others at 0
        IF EXISTS (SELECT 1 FROM public.plan_report() p
                    WHERE p.colocation_id = r.colocation_id
                      AND ((EXISTS (SELECT 1 FROM kingres.node n WHERE n.group_id = p.group_id AND n.should_have_shards)
                            AND p.after NOT IN (r.bucket_count / nrecv, (r.bucket_count + nrecv - 1) / nrecv))
                        OR (NOT EXISTS (SELECT 1 FROM kingres.node n WHERE n.group_id = p.group_id AND n.should_have_shards)
                            AND p.after <> 0))) THEN
            ok := false;
        END IF;
        -- minimal: the number of moves is the number of buckets that have to leave
        IF (SELECT count(*) FROM kingres.rebalance_plan() p WHERE p.colocation_id = r.colocation_id)
           <> (SELECT coalesce(sum(greatest(p.before - p.after, 0)), 0) FROM public.plan_report() p WHERE p.colocation_id = r.colocation_id) THEN
            ok := false;
        END IF;
        -- a source never receives, a target never gives
        IF EXISTS (SELECT 1 FROM kingres.rebalance_plan() a, kingres.rebalance_plan() b
                    WHERE a.colocation_id = r.colocation_id AND b.colocation_id = r.colocation_id
                      AND a.source_group = b.target_group) THEN
            ok := false;
        END IF;
        -- no bucket twice
        IF EXISTS (SELECT 1 FROM kingres.rebalance_plan() p WHERE p.colocation_id = r.colocation_id
                    GROUP BY p.bucket HAVING count(*) > 1) THEN
            ok := false;
        END IF;
    END LOOP;
    RETURN ok;
END $$;

-- A single group: nothing to do.
SELECT public.setup_nodes(1);
SELECT public.setup_colo(1, 4, ARRAY[0,0,0,0]);
SELECT * FROM kingres.rebalance_plan();

-- A new empty group next to a full one: half of the buckets move.
SELECT public.setup_nodes(2);
SELECT public.setup_colo(1, 8, ARRAY[0,0,0,0,0,0,0,0]);
SELECT colocation_id, bucket, source_group, target_group FROM kingres.rebalance_plan();
SELECT * FROM public.plan_report();
SELECT public.plan_obeys_rules();

-- Already even: no moves.
DELETE FROM kingres.bucket_map;
DELETE FROM kingres.colocation;
SELECT public.setup_colo(1, 8, ARRAY[0,1,0,1,0,1,0,1]);
SELECT count(*) AS moves FROM kingres.rebalance_plan();

-- Three groups, one of them empty: both others give the same.
SELECT public.setup_nodes(3);
SELECT public.setup_colo(1, 12, ARRAY[0,0,0,0,0,0,1,1,1,1,1,1]);
SELECT colocation_id, bucket, source_group, target_group FROM kingres.rebalance_plan();
SELECT * FROM public.plan_report();
SELECT public.plan_obeys_rules();

-- A remainder goes to the groups that already hold the most: 7 buckets on 3
-- groups end as 3, 2, 2.
SELECT public.setup_nodes(3);
SELECT public.setup_colo(1, 7, ARRAY[0,0,0,0,0,1,2]);
SELECT colocation_id, bucket, source_group, target_group FROM kingres.rebalance_plan();
SELECT * FROM public.plan_report();
SELECT public.plan_obeys_rules();
-- Not the first group: 7 buckets, group 2 holds 4, groups 0 and 1 hold 2 and 1.
SELECT public.setup_nodes(3);
SELECT public.setup_colo(1, 7, ARRAY[2,2,2,2,0,0,1]);
SELECT * FROM public.plan_report();
SELECT public.plan_obeys_rules();

-- A draining group (should_have_shards = false) is emptied completely.
SELECT public.setup_nodes(3, ARRAY[1]);
SELECT public.setup_colo(1, 12, ARRAY[0,0,0,1,1,1,1,1,1,2,2,2]);
SELECT colocation_id, bucket, source_group, target_group FROM kingres.rebalance_plan();
SELECT * FROM public.plan_report();
SELECT public.plan_obeys_rules();

-- Several colocation groups: independent, interleaved so that a receiving group
-- fills evenly (the first bucket into a group of every colocation group comes
-- before the second of any).
SELECT public.setup_nodes(3);
SELECT public.setup_colo(1, 6, ARRAY[0,0,0,0,0,0]);
SELECT public.setup_colo(2, 6, ARRAY[0,0,0,0,0,0]);
SELECT colocation_id, bucket, source_group, target_group FROM kingres.rebalance_plan();
SELECT public.plan_obeys_rules();
-- max_moves cuts the plan after that many moves, in the same order.
SELECT colocation_id, bucket, source_group, target_group FROM kingres.rebalance_plan(3);
SELECT count(*) AS none FROM kingres.rebalance_plan(0);

-- A bucket that is itself being moved is left alone.
UPDATE kingres.bucket_map SET state = 'moving' WHERE colocation_id = 1 AND bucket IN (4, 5);
SELECT colocation_id, bucket, source_group, target_group FROM kingres.rebalance_plan() WHERE colocation_id = 1;
UPDATE kingres.bucket_map SET state = 'active';

-- Two colocation groups with different bucket counts and placements.
SELECT public.setup_nodes(4);
SELECT public.setup_colo(1, 8, ARRAY[0,0,0,0,1,1,2,3]);
SELECT public.setup_colo(2, 5, ARRAY[3,3,3,3,3]);
SELECT public.plan_obeys_rules();
SELECT * FROM public.plan_report();

-- Nothing can receive: an error, not an empty plan.
SELECT public.setup_nodes(2, ARRAY[0, 1]);
SELECT public.setup_colo(1, 4, ARRAY[0,0,1,1]);
SELECT * FROM kingres.rebalance_plan();
SELECT * FROM kingres.rebalance_plan(-1);

-- size_bytes is NULL when the source cannot be read.
SELECT public.setup_nodes(2);
SELECT public.setup_colo(1, 4, ARRAY[0,0,0,0]);
SELECT bucket, size_bytes FROM kingres.rebalance_plan();

-- A role that is not a superuser may ask for the plan (it only reads) and gets
-- no sizes.
CREATE ROLE ks_plan_reader LOGIN;
GRANT USAGE ON SCHEMA public TO ks_plan_reader;
SET ROLE ks_plan_reader;
SELECT count(*) AS plan_for_a_plain_role FROM kingres.rebalance_plan();
SELECT count(*) AS cannot_exec FROM kingres.rebalance_plan() WHERE size_bytes IS NOT NULL;
RESET ROLE;
DROP OWNED BY ks_plan_reader;
DROP ROLE ks_plan_reader;

-- check_cluster sees a node that cannot be reached.
SELECT public.setup_nodes(2);
SELECT public.setup_colo(1, 4, ARRAY[0,0,1,1]);
SELECT check_name, node_id FROM kingres.check_cluster();

-- set_node_property, on a cluster of one node (it synchronizes every node).
DELETE FROM kingres.bucket_map;
DELETE FROM kingres.colocation;
DELETE FROM kingres.node WHERE node_id > 1;
SELECT public.setup_colo(1, 4, ARRAY[0,0,0,0]);
SELECT kingres.set_node_property(1, false);
SELECT node_id, group_id, should_have_shards FROM kingres.node ORDER BY node_id;
SELECT * FROM kingres.rebalance_plan();
SELECT kingres.set_node_property(1, true);
SELECT kingres.set_node_property(99, true);
SELECT node_id, should_have_shards FROM kingres.node ORDER BY node_id;
-- (the version moved, as a catalog change should)
SELECT version > 0 AS version_moved FROM kingres.map_version;
-- A role that is not a superuser may not.
CREATE ROLE ks_plan_reader2 LOGIN;
SET ROLE ks_plan_reader2;
SELECT kingres.set_node_property(1, true);
SELECT kingres.check_cluster();
RESET ROLE;
DROP ROLE ks_plan_reader2;

-- check_cluster on a catalog that is consistent: no rows.
SELECT * FROM kingres.check_cluster();

DROP FUNCTION public.plan_obeys_rules(), public.plan_report(), public.setup_colo(int, int, int[]),
    public.setup_nodes(int, int[]);
DROP EXTENSION kingres_shard;
