-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- plan_strategies.sql: pg_regress test of kingres.plan_rebalance(), kingres.group_load() and the weight of a node
-- (design/shard-phase10.md, sections 4 and 5; increment P10.3) against hand-built bucket maps.  Nothing here contacts another node: the
-- plan by disk size is tested through kingres.internal_plan_simulate(), which takes the sizes as data (the TAP test t/201 reads real ones).
\set SHOW_CONTEXT never
SET search_path = public, pg_temp;
SET kingres_shard.node_connect_timeout = 1;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster(btrim(split_part(current_setting('unix_socket_directories'), ',', 1)), current_setting('port')::int);

-- p_groups shard groups 0 .. p_groups - 1, one primary node each (node id = group + 1); the groups in p_noshard do not accept buckets;
-- p_weights gives the weight of the groups from group 0 on.
CREATE FUNCTION public.setup_nodes(p_groups int, p_noshard int[] DEFAULT '{}', p_weights float8[] DEFAULT '{}') RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM kingres.bucket_move;
    DELETE FROM kingres.bucket_map;
    DELETE FROM kingres.colocation;
    DELETE FROM kingres.node WHERE node_id > 1;
    FOR g IN 1 .. p_groups - 1 LOOP
        INSERT INTO kingres.node (node_id, group_id, host, port, role)
        VALUES (g + 1, g, '127.0.0.1', g + 1, 'primary');
    END LOOP;
    UPDATE kingres.node SET should_have_shards = NOT (group_id = ANY (p_noshard)), weight = 1, is_active = true;
    UPDATE kingres.node SET weight = p_weights[group_id + 1] WHERE p_weights[group_id + 1] IS NOT NULL;
END $$;

-- p_layout: "group:count" ranges, one colocation group of p_buckets buckets filled from bucket 0 in the order of the list
CREATE FUNCTION public.setup_colo(p_colo int, p_buckets int, p_ranges text[]) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    r text; b int := 0; n int;
BEGIN
    INSERT INTO kingres.colocation VALUES (p_colo, p_buckets, 'int4', 0);
    FOREACH r IN ARRAY p_ranges LOOP
        n := split_part(r, ':', 2)::int;
        INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id)
        SELECT p_colo, b + i, split_part(r, ':', 1)::int FROM generate_series(0, n - 1) i;
        b := b + n;
    END LOOP;
END $$;

-- the buckets per group after the plan is applied to the map: "group:count" in group order
CREATE FUNCTION public.applied(p_plan jsonb) RETURNS text
LANGUAGE sql AS $$
WITH moved AS (SELECT (x->>'c')::int AS c, (x->>'b')::int AS b, (x->>'target')::int AS tgt FROM jsonb_array_elements(p_plan) x),
     final AS (SELECT coalesce(mv.tgt, m.group_id) AS g FROM kingres.bucket_map m LEFT JOIN moved mv ON mv.c = m.colocation_id AND mv.b = m.bucket)
SELECT string_agg(g || ':' || n, ' ' ORDER BY g) FROM (SELECT g, count(*) AS n FROM final GROUP BY g) s
$$;
CREATE FUNCTION public.plan_json(p_strategy text DEFAULT 'by_bucket_count', p_threshold float8 DEFAULT NULL, p_max int DEFAULT NULL,
                                 p_drain_only bool DEFAULT false, p_extra jsonb DEFAULT NULL, p_exclude int[] DEFAULT NULL, p_colos int[] DEFAULT NULL) RETURNS jsonb
LANGUAGE sql AS $$
SELECT coalesce(jsonb_agg(jsonb_build_object('c', colocation_id, 'b', bucket, 'source', source_group, 'target', target_group, 'reason', reason) ORDER BY seq), '[]')
  FROM kingres.plan_rebalance(p_strategy, p_threshold, p_max, p_drain_only, p_extra, p_exclude, p_colos)
$$;

-- ---------------------------------------------------------------------------
-- Example 1 of the design: 128 buckets on three groups (43, 43, 42), a fourth group empty
-- ---------------------------------------------------------------------------
SELECT public.setup_nodes(4);
SELECT public.setup_colo(1, 128, ARRAY['0:43', '1:43', '2:42']);
SELECT * FROM kingres.group_load();
SELECT count(*) AS moves_threshold_0, public.applied(public.plan_json('by_bucket_count', 0)) AS final FROM kingres.plan_rebalance('by_bucket_count', 0);
SELECT count(*) AS moves_default_threshold FROM kingres.plan_rebalance();
SELECT count(*) AS moves_threshold_01, public.applied(public.plan_json('by_bucket_count', 0.1)) AS final FROM kingres.plan_rebalance('by_bucket_count', 0.1);
SELECT count(*) AS moves_threshold_01_capped FROM kingres.plan_rebalance('by_bucket_count', 0.1, 10);
SELECT seq, colocation_id, bucket, source_group, target_group, cost, size_bytes, reason FROM kingres.plan_rebalance('by_bucket_count', 0, 6);

-- the same count of moves as the plan of version 0.3 over generated maps (one colocation group, equal weights)
CREATE FUNCTION public.compare_with_phase4() RETURNS TABLE (maps int, same_moves int, same_counts int)
LANGUAGE plpgsql AS $$
DECLARE
    i int; ng int; nb int; mv4 int; mv10 int; c4 text; c10 text; nmaps int := 0; nsame int := 0; ncount int := 0;
BEGIN
    FOR i IN 1 .. 40 LOOP
        ng := 2 + i % 4;
        nb := 6 + (i * 7) % 35;
        PERFORM public.setup_nodes(ng);
        INSERT INTO kingres.colocation VALUES (1, nb, 'int4', 0);
        INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id)
        SELECT 1, b, (abs(hashint4(b * 31 + i)) % (1 + i % ng)) FROM generate_series(0, nb - 1) b;
        SELECT count(*) INTO mv4 FROM kingres.rebalance_plan();
        SELECT count(*) INTO mv10 FROM kingres.plan_rebalance('by_bucket_count', 0);
        SELECT string_agg(n::text, ',' ORDER BY n DESC, g) INTO c4
          FROM (SELECT g, count(*) AS n FROM (SELECT coalesce((SELECT p.target_group FROM kingres.rebalance_plan() p WHERE p.bucket = m.bucket), m.group_id) AS g FROM kingres.bucket_map m) f GROUP BY g) s;
        SELECT string_agg(n::text, ',' ORDER BY n DESC, g) INTO c10
          FROM (SELECT g, count(*) AS n FROM (SELECT coalesce((SELECT p.target_group FROM kingres.plan_rebalance('by_bucket_count', 0) p WHERE p.bucket = m.bucket), m.group_id) AS g FROM kingres.bucket_map m) f GROUP BY g) s;
        nmaps := nmaps + 1;
        IF mv4 = mv10 THEN nsame := nsame + 1; END IF;
        IF c4 IS NOT DISTINCT FROM c10 THEN ncount := ncount + 1; END IF;
    END LOOP;
    RETURN QUERY SELECT nmaps, nsame, ncount;
END $$;
SELECT * FROM public.compare_with_phase4();

-- ---------------------------------------------------------------------------
-- Example 2: by disk size (the sizes are data of the simulation); then the same cluster by bucket count
-- ---------------------------------------------------------------------------
CREATE FUNCTION public.sim_sizes(p_sizes text, p_threshold float8, p_strategy text DEFAULT 'by_disk_size') RETURNS TABLE (seq int, c int, b int, source int, target int, cost bigint, reason text)
LANGUAGE sql AS $$
-- p_sizes: "group:size,size,..." per group separated by ;  (sizes in GB)
WITH items AS (
  SELECT g.n AS grp, row_number() OVER (ORDER BY g.n, s.ord) - 1 AS b, (s.sz::bigint * 1073741824) AS size
    FROM (SELECT (split_part(x, ':', 1))::int AS n, split_part(x, ':', 2) AS list FROM unnest(string_to_array(p_sizes, ';')) x) g,
         LATERAL unnest(string_to_array(g.list, ',')) WITH ORDINALITY s(sz, ord) WHERE s.sz <> ''),
doc AS (
  SELECT jsonb_build_object(
    'groups', (SELECT jsonb_agg(jsonb_build_object('id', n, 'avail', true, 'recv', true, 'w', 1, 'extra', 0) ORDER BY n) FROM generate_series(0, 2) n),
    'items', (SELECT jsonb_agg(jsonb_build_object('c', 1, 'b', b, 'owner', grp, 'phys', grp, 'mov', true, 'size', size,
                     'cost', CASE WHEN p_strategy = 'by_disk_size' THEN size ELSE 1 END) ORDER BY b) FROM items)) AS d)
SELECT p.seq, p.colocation_id, p.bucket, p.source_group, p.target_group, p.cost, p.reason
  FROM doc, kingres.internal_plan_simulate(doc.d, p_strategy, p_threshold, NULL, false) p
$$;
-- A holds 12 8 6 4 GB (buckets 0-3), B 10 5 3 2 (4-7), C is empty
SELECT seq, b AS bucket, source, target, cost / 1073741824 AS gb, reason FROM public.sim_sizes('0:12,8,6,4;1:10,5,3,2', 0.15);
SELECT seq, b AS bucket, source, target, cost AS buckets, reason FROM public.sim_sizes('0:12,8,6,4;1:10,5,3,2', 0.15, 'by_bucket_count');

-- ---------------------------------------------------------------------------
-- Example 3: a drain with weights
-- ---------------------------------------------------------------------------
SELECT public.setup_nodes(3, ARRAY[2], ARRAY[2, 1, 1]);
SELECT public.setup_colo(1, 128, ARRAY['0:43', '1:43', '2:42']);
SELECT * FROM kingres.group_load();
SELECT count(*) AS drain_moves, public.applied(public.plan_json()) AS final, count(*) FILTER (WHERE reason = 'drain') AS drain_reasons FROM kingres.plan_rebalance();
SELECT public.setup_nodes(3, ARRAY[2]);
SELECT public.setup_colo(1, 128, ARRAY['0:43', '1:43', '2:42']);
SELECT count(*) AS drain_moves_equal_weights, public.applied(public.plan_json()) AS final FROM kingres.plan_rebalance();
-- drain_only: the drain moves and no balancing (group 0 holds 8 and group 1 none: the plan balances after the drain, the drain-only plan does not)
SELECT public.setup_nodes(3, ARRAY[2]);
SELECT public.setup_colo(1, 12, ARRAY['0:8', '2:4']);
SELECT count(*) AS plan_moves, count(*) FILTER (WHERE reason = 'balance') AS balance_moves FROM kingres.plan_rebalance();
SELECT count(*) AS drain_only_moves, count(*) FILTER (WHERE reason = 'balance') AS balance_moves FROM kingres.plan_rebalance(drain_only => true);
-- a what-if drain with exclude_groups: group 1 is treated as not receiving
SELECT public.setup_nodes(3);
SELECT public.setup_colo(1, 12, ARRAY['0:4', '1:4', '2:4']);
SELECT count(*) AS moves_without_exclusion FROM kingres.plan_rebalance();
SELECT seq, source_group, target_group, reason FROM kingres.plan_rebalance(exclude_groups => ARRAY[1]);
SELECT public.applied(public.plan_json(p_exclude => ARRAY[1])) AS final;
SELECT group_id, receiving, buckets FROM kingres.group_load(exclude_groups => ARRAY[1]);

-- the load of other databases (extra_load): group 0 carries 8 more in another database
SELECT group_id, receiving, load, utilization, deviation FROM kingres.group_load(extra_load => '{"0": 8}');
SELECT seq, bucket, source_group, target_group FROM kingres.plan_rebalance(extra_load => '{"0": 8}', threshold => 0);
SELECT public.applied(public.plan_json(p_extra => '{"0": 8}', p_threshold => 0)) AS final_with_extra_load;
-- the load of other databases is part of the ideal: utilizations 6, 4 and 4 with an ideal of 4.67 are inside a window of 0.3
SELECT public.setup_nodes(3);
SELECT public.setup_colo(1, 12, ARRAY['0:4', '1:4', '2:4']);
SELECT count(*) AS moves_inside_the_window FROM kingres.plan_rebalance(threshold => 0.3, extra_load => '{"0": 2}');
SELECT count(*) AS moves_without_the_extra_load FROM kingres.plan_rebalance(threshold => 0.3);
-- a drain always moves its buckets, even when the move does not bring the two utilizations closer (group 2 holds one bucket, the others ten)
SELECT public.setup_nodes(3, ARRAY[2]);
SELECT public.setup_colo(1, 21, ARRAY['0:10', '1:10', '2:1']);
SELECT seq, source_group, target_group, reason FROM kingres.plan_rebalance();
-- a move must bring the utilizations closer, per unit of weight: group 0 (weight 1) holds 2 buckets (utilization 2), group 1 (weight 2) holds 3 (1.5);
-- a bucket from 0 to 1 would give 1 and 2, which is farther apart, so there is nothing to move
SELECT public.setup_nodes(2, '{}', ARRAY[1, 2]);
SELECT public.setup_colo(1, 5, ARRAY['0:2', '1:3']);
SELECT count(*) AS moves_that_do_not_help FROM kingres.plan_rebalance();
SELECT * FROM kingres.group_load();
-- weights decide too: group 2 counts for three
SELECT public.setup_nodes(3, '{}', ARRAY[1, 1, 3]);
SELECT public.setup_colo(1, 15, ARRAY['0:5', '1:5', '2:5']);
SELECT public.applied(public.plan_json()) AS final_with_weight_3;

-- ---------------------------------------------------------------------------
-- Several colocation groups, and a restriction to some of them
-- ---------------------------------------------------------------------------
SELECT public.setup_nodes(3);
SELECT public.setup_colo(1, 6, ARRAY['0:6']);
SELECT public.setup_colo(2, 6, ARRAY['0:6']);
SELECT count(*) AS moves_all, count(DISTINCT colocation_id) AS colocation_groups FROM kingres.plan_rebalance();
SELECT public.applied(public.plan_json()) AS final_all;
SELECT count(*) AS moves_group_2_only, min(colocation_id) AS only_group FROM kingres.plan_rebalance(colocation_ids => ARRAY[2]);
SELECT public.applied(public.plan_json(p_colos => ARRAY[2])) AS final_group_2_only;

-- ---------------------------------------------------------------------------
-- A bucket that is moving counts at its target, and is not planned; a group without a primary is neither source nor receiver
-- ---------------------------------------------------------------------------
SELECT public.setup_nodes(2);
SELECT public.setup_colo(1, 8, ARRAY['0:8']);
UPDATE kingres.bucket_map SET state = 'moving' WHERE bucket IN (6, 7);
INSERT INTO kingres.bucket_move (colocation_id, bucket, source_group, target_group, phase, started_at, updated_at)
SELECT 1, b, 0, 1, 'copying', now(), now() FROM generate_series(6, 7) b;
SELECT * FROM kingres.group_load();
SELECT * FROM kingres.drain_status();
SELECT seq, bucket, source_group, target_group FROM kingres.plan_rebalance();
UPDATE kingres.bucket_map SET state = 'active';
DELETE FROM kingres.bucket_move;
UPDATE kingres.node SET is_active = false WHERE group_id = 1;
SELECT group_id, receiving, available, buckets FROM kingres.group_load();
SELECT count(*) AS moves_to_a_group_without_a_primary FROM kingres.plan_rebalance();
UPDATE kingres.node SET is_active = true WHERE group_id = 1;
-- drained: may not receive, owns nothing, nothing arriving
SELECT public.setup_nodes(3, ARRAY[2]);
SELECT public.setup_colo(1, 8, ARRAY['0:4', '1:4']);
SELECT * FROM kingres.drain_status();
SELECT public.setup_colo(2, 6, ARRAY['2:6']);
SELECT group_id, buckets, drained FROM kingres.drain_status() WHERE group_id = 2;
UPDATE kingres.node SET should_have_shards = false;
SELECT * FROM kingres.plan_rebalance();
UPDATE kingres.node SET should_have_shards = true;

-- ---------------------------------------------------------------------------
-- Refusals and the weight of a node
-- ---------------------------------------------------------------------------
SELECT * FROM kingres.plan_rebalance('by_color');
SELECT * FROM kingres.plan_rebalance(threshold => -1);
SELECT * FROM kingres.plan_rebalance(max_moves => -1);
SELECT * FROM kingres.group_load(extra_load => '[1]');
-- (the successful forms of set_node_property() push the catalog to the other nodes: t/201 tests them)
SELECT kingres.set_node_property(2, NULL, NULL);
SELECT kingres.set_node_property(2, NULL, 0);
SELECT kingres.set_node_property(2, NULL, 1001);
SELECT kingres.set_node_property(2, NULL, 'NaN');
SELECT kingres.set_node_property(99, NULL, 1);
UPDATE kingres.node SET weight = 0 WHERE node_id = 2;
-- a node document of a map group that is not upgraded (no weight) gets the weight 1
SELECT kingres.internal_apply_catalog(jsonb_set(kingres.internal_catalog_snapshot(), '{node}',
       (SELECT jsonb_agg(n - 'weight') FROM jsonb_array_elements(kingres.internal_catalog_snapshot() -> 'node') n)));
SELECT node_id, weight FROM kingres.node ORDER BY node_id;

DROP EXTENSION kingres_shard CASCADE;
