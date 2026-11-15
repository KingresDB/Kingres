-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_shard--0.4--0.5.sql
--   Upgrade of the kingres_shard extension from version 0.4 to 0.5: high
--   availability (design/ha-phase5.md, part C).  What changes in the database:
--     * standby nodes: kingres.standby_status(), and kingres.check_cluster()
--       reports standbys that are not reachable or not streaming (a standby is
--       a node row with role 'standby'; it owns no buckets and every function
--       that goes through the nodes skips it);
--     * kingres.promote_node(): the catalog change of a failover;
--     * the gid of a two-phase transaction names the origin group and the
--       timeline, and kingres.dtx_incident records what recovery rolled back
--       after a failover;
--     * the distributed deadlock detector (kingres.detect_distributed_deadlocks()
--       and its helpers);
--     * kingres.create_restore_point();
--     * kingres.internal_check_node_versions() asks for 0.5.
--   The result is identical to installing kingres_shard--0.5.sql (the "upgrade"
--   regression test compares the two).

\echo Use "ALTER EXTENSION kingres_shard UPDATE TO '0.5'" to load this file. \quit

-- ---------------------------------------------------------------------------
-- High availability (design/ha-phase5.md, part C)
-- ---------------------------------------------------------------------------

-- Node-local, never synchronized: prepared transactions that recovery rolled back
-- because they belonged to an earlier timeline of this group (a failover) and no
-- commit decision was found.  With a synchronous standby these are transactions
-- that never committed; with an asynchronous one a transaction that the lost
-- primary committed and whose decision did not reach the standby can be among
-- them (README, "Failover and prepared transactions").  check_cluster() lists
-- them for 30 days.
CREATE TABLE kingres.dtx_incident (
    gid              text        NOT NULL,
    node_id          int         NOT NULL,
    gid_timeline     int         NOT NULL,
    current_timeline int         NOT NULL,
    detected_at      timestamptz NOT NULL,
    kind             text        NOT NULL,
    CONSTRAINT dtx_incident_pkey PRIMARY KEY (gid)
);

COMMENT ON TABLE kingres.dtx_incident IS
    'Node-local: prepared transactions of an earlier timeline that recovery rolled back without a commit decision; never synchronized';

-- promote_node(node_id): see src/failover.c.
CREATE FUNCTION kingres.promote_node(node_id int)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_promote_node'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.promote_node(int) IS
    'Make a promoted standby the primary of its shard group (map group primary; for group 0 the promoted node itself)';

-- The barrier of create_restore_point() on this node, and the mark of a
-- distributed-deadlock victim (src/ha.c).
CREATE FUNCTION kingres.internal_restore_barrier(op text, ttl_ms int DEFAULT NULL, generation bigint DEFAULT NULL)
    RETURNS jsonb
    AS 'MODULE_PATHNAME', 'kingres_internal_restore_barrier'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_mark_victim(pid int)
    RETURNS bool
    AS 'MODULE_PATHNAME', 'kingres_internal_mark_victim'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

-- Coordinated restore point.  On the map group primary: raise the barrier on
-- every primary (a transaction that is about to commit with two-phase commit
-- waits there), wait until every transaction that had passed the barrier has
-- finished its second phase on every node, create the restore point on every
-- primary, lower the barrier and check that it was up the whole time.
CREATE FUNCTION kingres.create_restore_point(name text)
    RETURNS TABLE (node_id int, group_id int, restore_lsn pg_lsn)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    n         pg_catalog.record;
    v_nodes   pg_catalog.int4[];
    v_gens    pg_catalog.int8[] := '{}'::pg_catalog.int8[];
    v_ttl     pg_catalog.int4;
    v_wait    pg_catalog.int4;
    v_deadline pg_catalog.timestamptz;
    v_busy    pg_catalog.bool;
    v_st      pg_catalog.jsonb;
    v_lsns    pg_catalog.text[] := '{}'::pg_catalog.text[];
    v_groups  pg_catalog.int4[] := '{}'::pg_catalog.int4[];
    v_lost    pg_catalog.int4[] := '{}'::pg_catalog.int4[];
    i         pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    IF name IS NULL OR pg_catalog.length(name) = 0 OR pg_catalog.octet_length(name) >= 64 THEN
        RAISE EXCEPTION 'the name of a restore point must have 1 to 63 bytes'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- Taking restore points one after the other, with the barrier up: nobody else
    -- may change the cluster in between (a node added or a primary promoted).
    LOCK TABLE kingres.map_version IN SHARE MODE;
    v_wait := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name = 'kingres_shard.node_timeout');
    v_ttl := (v_wait * 2 + 10) * 1000;
    SELECT pg_catalog.array_agg(nd.node_id ORDER BY nd.node_id), pg_catalog.array_agg(nd.group_id ORDER BY nd.node_id)
      INTO v_nodes, v_groups
      FROM kingres.node nd WHERE nd.is_active AND nd.role = 'primary';

    BEGIN
        FOR i IN 1 .. pg_catalog.array_length(v_nodes, 1) LOOP
            v_st := kingres.internal_exec_scalar(v_nodes[i],
                pg_catalog.format('SELECT kingres.internal_restore_barrier(''on'', %s)::pg_catalog.text', v_ttl),
                v_wait * 1000)::pg_catalog.jsonb;
            v_gens := pg_catalog.array_append(v_gens, (v_st ->> 'generation')::pg_catalog.int8);
        END LOOP;
        PERFORM kingres.internal_fault_point('restore_barrier_up');

        -- Nothing new can start now; wait for what had started.
        v_deadline := pg_catalog.clock_timestamp() + pg_catalog.make_interval(secs => v_wait);
        LOOP
            v_busy := false;
            FOR i IN 1 .. pg_catalog.array_length(v_nodes, 1) LOOP
                v_st := kingres.internal_exec_scalar(v_nodes[i],
                    'SELECT kingres.internal_restore_barrier(''state'')::pg_catalog.text', v_wait * 1000)::pg_catalog.jsonb;
                IF (v_st ->> 'inflight')::pg_catalog.int4 > 0 THEN
                    v_busy := true;
                END IF;
            END LOOP;
            EXIT WHEN NOT v_busy;
            IF pg_catalog.clock_timestamp() > v_deadline THEN
                RAISE EXCEPTION 'distributed transactions did not finish their second phase within % seconds', v_wait
                    USING ERRCODE = 'lock_not_available',
                          HINT = 'Resolve the stuck transactions (kingres.recover_prepared_transactions()) and try again.';
            END IF;
            PERFORM pg_catalog.pg_sleep(0.02);
        END LOOP;
        PERFORM kingres.internal_fault_point('restore_all_quiet');

        FOR i IN 1 .. pg_catalog.array_length(v_nodes, 1) LOOP
            v_lsns := pg_catalog.array_append(v_lsns, kingres.internal_exec_scalar(v_nodes[i],
                pg_catalog.format('SELECT pg_catalog.pg_create_restore_point(%L)::pg_catalog.text', name), v_wait * 1000));
        END LOOP;
    EXCEPTION WHEN OTHERS THEN
        -- Lower what was raised; the barrier expires by itself when a node cannot be reached.
        FOR i IN 1 .. COALESCE(pg_catalog.array_length(v_gens, 1), 0) LOOP
            BEGIN
                PERFORM kingres.internal_exec_scalar(v_nodes[i],
                    pg_catalog.format('SELECT kingres.internal_restore_barrier(''off'', NULL, %s)::pg_catalog.text', v_gens[i]), 5000);
            EXCEPTION WHEN OTHERS THEN
                NULL;
            END;
        END LOOP;
        RAISE;
    END;

    FOR i IN 1 .. pg_catalog.array_length(v_nodes, 1) LOOP
        v_st := kingres.internal_exec_scalar(v_nodes[i],
            pg_catalog.format('SELECT kingres.internal_restore_barrier(''off'', NULL, %s)::pg_catalog.text', v_gens[i]),
            v_wait * 1000)::pg_catalog.jsonb;
        IF NOT (v_st ->> 'held')::pg_catalog.bool THEN
            v_lost := pg_catalog.array_append(v_lost, v_nodes[i]);
        END IF;
    END LOOP;
    IF pg_catalog.array_length(v_lost, 1) > 0 THEN
        RAISE EXCEPTION 'the barrier of node(s) % expired before the restore points were made; the restore point "%" is not consistent', v_lost, name
            USING ERRCODE = 'lock_not_available',
                  HINT = 'Do not use the restore point; run kingres.create_restore_point() again.';
    END IF;

    FOR i IN 1 .. pg_catalog.array_length(v_nodes, 1) LOOP
        node_id := v_nodes[i];
        group_id := v_groups[i];
        restore_lsn := v_lsns[i]::pg_catalog.pg_lsn;
        RETURN NEXT;
    END LOOP;
END;
$$;

COMMENT ON FUNCTION kingres.create_restore_point(text) IS
    'Create the restore point on every primary with no distributed transaction half way through its commit (map group primary, superuser)';

-- ---------------------------------------------------------------------------
-- Distributed deadlock detection
-- ---------------------------------------------------------------------------

-- The distributed transaction a backend of this node works for: the label that
-- the origin gave the remote session ("ks-dtx:<node>:<pid>:<start>", set as its
-- application name), or, for any other backend, the backend itself as an origin.
CREATE FUNCTION kingres.internal_dtx_label_of(p_pid int, p_node int)
    RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT CASE WHEN a.application_name LIKE 'ks-dtx:%' THEN a.application_name
            WHEN a.xact_start IS NOT NULL
            THEN pg_catalog.format('ks-dtx:%s:%s:%s', p_node, a.pid,
                                   (extract(epoch FROM a.xact_start) * 1000000)::pg_catalog.int8)
       END
  FROM pg_catalog.pg_stat_activity a
 WHERE a.pid = p_pid;
$$;

-- The edges "distributed transaction A waits for distributed transaction B" that
-- this node sees: one per (waiting backend, backend that blocks it), for waits
-- older than p_min_wait_ms (default deadlock_timeout: a younger wait is not
-- looked at, and PostgreSQL has already looked at a local deadlock by then).
CREATE FUNCTION kingres.internal_wait_edges(p_min_wait_ms int DEFAULT NULL)
    RETURNS jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_node pg_catalog.int4;
    v_min  pg_catalog.interval;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT l.node_id INTO v_node FROM kingres.local_node l;
    v_min := pg_catalog.make_interval(secs =>
        COALESCE(p_min_wait_ms,
                 (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name = 'deadlock_timeout'))
        / 1000.0);
    RETURN COALESCE((
        SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                   'node', v_node,
                   'waiter_pid', w.pid,
                   'waitstart', (extract(epoch FROM w.waitstart) * 1000000)::pg_catalog.int8,
                   'waiter', lw.label,
                   'blocker', lb.label,
                   'blocker_pid', bp.pid))
          FROM (SELECT DISTINCT ON (l.pid) l.pid, l.waitstart
                  FROM pg_catalog.pg_locks l
                 WHERE NOT l.granted AND l.pid IS NOT NULL AND l.waitstart IS NOT NULL
                   AND l.waitstart < pg_catalog.clock_timestamp() - v_min
                 ORDER BY l.pid, l.waitstart) w
         CROSS JOIN LATERAL pg_catalog.unnest(pg_catalog.pg_blocking_pids(w.pid)) AS bp(pid)
         CROSS JOIN LATERAL (SELECT kingres.internal_dtx_label_of(w.pid, v_node)) lw(label)
         CROSS JOIN LATERAL (SELECT kingres.internal_dtx_label_of(bp.pid, v_node)) lb(label)
         WHERE lw.label IS NOT NULL AND lb.label IS NOT NULL AND lw.label <> lb.label),
        '[]'::pg_catalog.jsonb);
END;
$$;

-- Cancel the backend p_pid of this node if it is still in the wait that started
-- at p_waitstart (microseconds since the Unix epoch): its error says that a
-- distributed deadlock was detected (SQLSTATE 40P01).
CREATE FUNCTION kingres.internal_cancel_waiter(p_pid int, p_waitstart bigint)
    RETURNS bool
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_locks l
                WHERE l.pid = p_pid AND NOT l.granted
                  AND (extract(epoch FROM l.waitstart) * 1000000)::pg_catalog.int8 = p_waitstart) THEN
        IF kingres.internal_mark_victim(p_pid) THEN
            RETURN pg_catalog.pg_cancel_backend(p_pid);
        END IF;
    END IF;
    RETURN false;
END;
$$;

-- The wait-for edges of every active primary, as one jsonb array (a node that
-- cannot be asked contributes nothing: a cycle needs every one of its edges).
CREATE FUNCTION kingres.internal_collect_wait_edges()
    RETURNS jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    n   pg_catalog.record;
    v_all pg_catalog.jsonb := '[]'::pg_catalog.jsonb;
    v_e   pg_catalog.text;
BEGIN
    FOR n IN SELECT nd.node_id FROM kingres.node nd
              WHERE nd.is_active AND nd.role = 'primary' ORDER BY nd.node_id
    LOOP
        BEGIN
            v_e := kingres.internal_exec_scalar(n.node_id,
                'SELECT kingres.internal_wait_edges()::pg_catalog.text', 10000);
            v_all := v_all || COALESCE(v_e::pg_catalog.jsonb, '[]'::pg_catalog.jsonb);
        EXCEPTION WHEN OTHERS THEN
            RAISE LOG 'kingres_shard deadlock detector: node % could not be asked: %', n.node_id, SQLERRM;
        END;
    END LOOP;
    RETURN v_all;
END;
$$;

-- The cycles of confirmed waits and the victim of each (the decision of the
-- detector, apart from the reading of the nodes, so that it can be tested with
-- edges that are made up).  p_first and p_second are two readings of the edges
-- (kingres.internal_wait_edges() of every node, one array); a cycle counts only
-- if it is made of edges that are the very same wait in both readings (the same
-- backend, in the wait that began at the same moment, blocked by the same
-- transaction) and its edges come from at least two nodes.  The victim is the
-- youngest transaction of the cycle (the latest start; the label breaks a tie),
-- and the wait to cancel is its wait in the cycle.
CREATE FUNCTION kingres.internal_deadlock_victims(p_first jsonb, p_second jsonb)
    RETURNS TABLE (victim text, victim_node int, victim_pid int, victim_waitstart bigint, cycle text[])
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
WITH RECURSIVE
stable_edges AS (
    SELECT DISTINCT a.node, a.waiter_pid, a.waitstart, a.waiter, a.blocker
      FROM pg_catalog.jsonb_to_recordset(p_first)
           AS a(node int, waiter_pid int, waitstart int8, waiter text, blocker text, blocker_pid int)
      JOIN pg_catalog.jsonb_to_recordset(p_second)
           AS b(node int, waiter_pid int, waitstart int8, waiter text, blocker text, blocker_pid int)
        USING (node, waiter_pid, waitstart, waiter, blocker, blocker_pid)
     WHERE a.waiter <> a.blocker),
walk(start, cur, path, nodes, pids, starts) AS (
    SELECT e.waiter, e.blocker, ARRAY[e.waiter], ARRAY[e.node], ARRAY[e.waiter_pid], ARRAY[e.waitstart]
      FROM stable_edges e
    UNION ALL
    SELECT w.start, e.blocker, w.path || w.cur, w.nodes || e.node, w.pids || e.waiter_pid, w.starts || e.waitstart
      FROM walk w JOIN stable_edges e ON e.waiter = w.cur
     WHERE w.cur <> w.start AND NOT (w.cur = ANY (w.path))
       AND pg_catalog.cardinality(w.path) < 32),
cycles AS (
    SELECT w.path, w.nodes, w.pids, w.starts
      FROM walk w
     WHERE w.cur = w.start
       AND w.start = (SELECT pg_catalog.min(v) FROM pg_catalog.unnest(w.path) v)
       AND (SELECT pg_catalog.count(DISTINCT nn) FROM pg_catalog.unnest(w.nodes) nn) > 1),
victims AS (
    SELECT c.*,
           (SELECT v FROM pg_catalog.unnest(c.path) v
             ORDER BY pg_catalog.split_part(v, ':', 4)::pg_catalog.int8 DESC, v DESC LIMIT 1) AS vic
      FROM cycles c)
SELECT DISTINCT ON (vv.vic)
       vv.vic, vv.nodes[i.pos], vv.pids[i.pos], vv.starts[i.pos], vv.path
  FROM victims vv
  CROSS JOIN LATERAL (SELECT pg_catalog.array_position(vv.path, vv.vic) AS pos) i
 ORDER BY vv.vic;
$$;

-- One pass of the detector (the recovery worker calls it on the map group
-- primary every kingres_shard.deadlock_check_interval seconds).  Returns the
-- number of transactions it cancelled.
--
-- A wait-for graph made of edges that were read at different moments on
-- different nodes can contain a cycle that never existed (A waited for B on
-- one node a moment before B stopped waiting for C on another).  So the nodes
-- are read twice, kingres_shard.deadlock_confirm_delay apart, and a cycle is
-- acted on only when it is made of edges that are the very same wait in both
-- readings (internal_deadlock_victims).  A wait is one uninterrupted period,
-- so each of those edges existed throughout the interval between the readings,
-- and all of them existed at the same time: a deadlock, which nothing resolves
-- by itself.  The wait of the victim that is part of the cycle is cancelled
-- where it is (on its origin node, or on the node whose lock it waits for: the
-- error goes back to the origin all the same).
CREATE FUNCTION kingres.detect_distributed_deadlocks()
    RETURNS int
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_first   pg_catalog.jsonb;
    v_second  pg_catalog.jsonb;
    v_delay   pg_catalog.int4;
    c         pg_catalog.record;
    v_done    pg_catalog.int4 := 0;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    v_first := kingres.internal_collect_wait_edges();
    IF pg_catalog.jsonb_array_length(v_first) < 2 THEN
        RETURN 0;
    END IF;
    -- Without the second reading: is there a cycle at all?
    IF NOT EXISTS (SELECT 1 FROM kingres.internal_deadlock_victims(v_first, v_first)) THEN
        RETURN 0;
    END IF;

    v_delay := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name = 'kingres_shard.deadlock_confirm_delay');
    PERFORM pg_catalog.pg_sleep(v_delay / 1000.0);
    v_second := kingres.internal_collect_wait_edges();

    FOR c IN SELECT * FROM kingres.internal_deadlock_victims(v_first, v_second)
    LOOP
        IF kingres.internal_exec_scalar(c.victim_node,
               pg_catalog.format('SELECT kingres.internal_cancel_waiter(%s, %s)::pg_catalog.text',
                                 c.victim_pid, c.victim_waitstart), 10000)::pg_catalog.bool THEN
            v_done := v_done + 1;
            RAISE LOG 'kingres_shard deadlock detector: cancelled % in the cycle %', c.victim, c.cycle;
        END IF;
    END LOOP;
    RETURN v_done;
END;
$$;

COMMENT ON FUNCTION kingres.detect_distributed_deadlocks() IS
    'One pass of the distributed deadlock detector (map group primary, superuser): cancels the youngest transaction of every confirmed cycle';

-- ---------------------------------------------------------------------------
-- Standbys
-- ---------------------------------------------------------------------------

-- On a primary: its walsenders, for the standbys that stream from it.
CREATE FUNCTION kingres.internal_replication_state()
    RETURNS jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
           'application_name', r.application_name,
           'slot_name', s.slot_name,
           'state', r.state,
           'sync_state', r.sync_state,
           'sent_lsn', r.sent_lsn::pg_catalog.text,
           'flush_lsn', r.flush_lsn::pg_catalog.text,
           'replay_lsn', r.replay_lsn::pg_catalog.text,
           'replay_lag_bytes', pg_catalog.pg_wal_lsn_diff(pg_catalog.pg_current_wal_lsn(), r.replay_lsn)::pg_catalog.int8)),
        '[]'::pg_catalog.jsonb)
  FROM pg_catalog.pg_stat_replication r
  LEFT JOIN pg_catalog.pg_replication_slots s ON s.active_pid = r.pid;
$$;

-- On a standby: how it identifies itself to its primary, and what it has received.
CREATE FUNCTION kingres.internal_standby_state()
    RETURNS jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.jsonb_build_object(
    'in_recovery', pg_catalog.pg_is_in_recovery(),
    'application_name', COALESCE(
        pg_catalog.substring(pg_catalog.current_setting('primary_conninfo'), 'application_name=([^ ]+)'),
        NULLIF(pg_catalog.current_setting('cluster_name'), ''), 'walreceiver'),
    'slot_name', pg_catalog.current_setting('primary_slot_name'),
    'receiver_status', (SELECT w.status FROM pg_catalog.pg_stat_wal_receiver w),
    'replay_lsn', pg_catalog.pg_last_wal_replay_lsn()::pg_catalog.text);
$$;

-- Every registered standby with its replication state as its primary sees it.
-- "problem" names what is wrong (NULL when the standby streams).
CREATE FUNCTION kingres.standby_status()
    RETURNS TABLE (node_id int, group_id int, host text, port int, primary_node_id int,
                   replication_state text, sync_state text, replay_lsn pg_lsn,
                   replay_lag_bytes bigint, problem text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    n      pg_catalog.record;
    v_rep  pg_catalog.jsonb;
    v_sb   pg_catalog.jsonb;
    v_err  pg_catalog.text;
    v_perr pg_catalog.text;
    m      pg_catalog.jsonb;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    FOR n IN SELECT nd.node_id, nd.group_id, nd.host, nd.port,
                    (SELECT p.node_id FROM kingres.node p
                      WHERE p.group_id = nd.group_id AND p.role = 'primary' AND p.is_active) AS primary_id
               FROM kingres.node nd WHERE nd.role = 'standby' ORDER BY nd.node_id
    LOOP
        node_id := n.node_id; group_id := n.group_id; host := n.host; port := n.port;
        primary_node_id := n.primary_id;
        replication_state := NULL; sync_state := NULL; replay_lsn := NULL; replay_lag_bytes := NULL;
        problem := NULL; detail := NULL;
        v_rep := NULL; v_sb := NULL; v_err := NULL; v_perr := NULL; m := NULL;

        IF n.primary_id IS NULL THEN
            problem := 'standby_without_primary';
            detail := pg_catalog.format('shard group %s has no active primary', n.group_id);
            RETURN NEXT;
            CONTINUE;
        END IF;
        BEGIN
            v_rep := kingres.internal_exec_scalar(n.primary_id,
                'SELECT kingres.internal_replication_state()::pg_catalog.text', 20000)::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS THEN
            v_perr := SQLERRM;
        END;
        BEGIN
            v_sb := kingres.internal_exec_scalar(n.node_id,
                'SELECT kingres.internal_standby_state()::pg_catalog.text', 20000)::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS THEN
            v_err := SQLERRM;
        END;

        IF v_sb IS NOT NULL THEN
            replay_lsn := (v_sb ->> 'replay_lsn')::pg_catalog.pg_lsn;
            IF NOT (v_sb ->> 'in_recovery')::pg_catalog.bool THEN
                replication_state := 'not a standby';
                problem := 'standby_not_in_recovery';
                detail := 'the node is registered as a standby and is not in recovery (promoted, or never was a standby)';
                RETURN NEXT;
                CONTINUE;
            END IF;
            IF v_rep IS NOT NULL THEN
                SELECT x INTO m FROM pg_catalog.jsonb_array_elements(v_rep) x
                 WHERE (COALESCE(v_sb ->> 'slot_name', '') <> '' AND x ->> 'slot_name' = v_sb ->> 'slot_name')
                    OR (COALESCE(v_sb ->> 'slot_name', '') = '' AND (x ->> 'slot_name') IS NULL
                        AND x ->> 'application_name' = v_sb ->> 'application_name')
                 LIMIT 1;
            END IF;
        END IF;

        IF v_perr IS NOT NULL THEN
            replication_state := 'unknown';
            problem := 'node_unreachable';
            detail := pg_catalog.format('the primary of the group (node %s) cannot be asked: %s', n.primary_id, v_perr);
        ELSIF m IS NOT NULL THEN
            replication_state := m ->> 'state';
            sync_state := m ->> 'sync_state';
            replay_lag_bytes := (m ->> 'replay_lag_bytes')::pg_catalog.int8;
            IF m ->> 'state' <> 'streaming' THEN
                problem := 'standby_not_streaming';
                detail := pg_catalog.format('its primary (node %s) reports the state "%s"', n.primary_id, m ->> 'state');
            END IF;
        ELSIF v_sb IS NULL THEN
            replication_state := 'unknown';
            problem := 'standby_unreachable';
            detail := pg_catalog.format('the standby cannot be asked (%s) and its primary (node %s) cannot be matched to it', v_err, n.primary_id);
        ELSE
            replication_state := 'not connected';
            problem := 'standby_not_streaming';
            detail := pg_catalog.format('its primary (node %s) has no walsender for it (receiver status: %s)',
                                        n.primary_id, COALESCE(v_sb ->> 'receiver_status', 'none'));
        END IF;
        RETURN NEXT;
    END LOOP;
END;
$$;

COMMENT ON FUNCTION kingres.standby_status() IS
    'Every registered standby with its replication state as its primary sees it (map group primary, superuser)';

REVOKE ALL ON FUNCTION
    kingres.promote_node(int),
    kingres.create_restore_point(text),
    kingres.internal_restore_barrier(text, int, bigint),
    kingres.internal_mark_victim(int),
    kingres.internal_dtx_label_of(int, int),
    kingres.internal_wait_edges(int),
    kingres.internal_cancel_waiter(int, bigint),
    kingres.internal_collect_wait_edges(),
    kingres.internal_deadlock_victims(jsonb, jsonb),
    kingres.detect_distributed_deadlocks(),
    kingres.internal_replication_state(),
    kingres.internal_standby_state(),
    kingres.standby_status()
    FROM PUBLIC;

-- internal_check_node_versions(): asks for 0.5.
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
            RAISE EXCEPTION 'node % (%:%) cannot be asked for its kingres_shard version: %', n.node_id, n.host, n.port, SQLERRM
                USING ERRCODE = 'object_not_in_prerequisite_state';
        END;
        IF v IS DISTINCT FROM '0.5' THEN
            RAISE EXCEPTION 'node % (%:%) has kingres_shard %, a bucket move needs 0.5 on every node',
                n.node_id, n.host, n.port, COALESCE(NULLIF(v, ''), '(not installed)')
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Run ALTER EXTENSION kingres_shard UPDATE on that node, after the new library is loaded there.';
        END IF;
    END LOOP;
END;
$$;

-- internal_local_state(): adds the incidents of this node.
CREATE OR REPLACE FUNCTION kingres.internal_local_state()
    RETURNS jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.jsonb_build_object(
    'node_id', (SELECT l.node_id FROM kingres.local_node l),
    'server_major', (pg_catalog.current_setting('server_version_num')::pg_catalog.int4 OPERATOR(pg_catalog./) 10000),
    'ext_version', (SELECT e.extversion FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard'),
    'has_secret', EXISTS (SELECT 1 FROM kingres.cluster_secret),
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
    'incidents', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                               'gid', i.gid, 'kind', i.kind, 'detected_at', i.detected_at,
                               'gid_timeline', i.gid_timeline, 'current_timeline', i.current_timeline))
                            FROM kingres.dtx_incident i), '[]'::pg_catalog.jsonb),
    'prepared', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                               'gid', x.gid, 'age', extract(epoch FROM pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) x.prepared)))
                            FROM pg_catalog.pg_prepared_xacts x), '[]'::pg_catalog.jsonb));
$$;

-- check_cluster(): reports standbys and the incidents of the nodes.
CREATE OR REPLACE FUNCTION kingres.check_cluster(p_prepared_older_than interval DEFAULT interval '5 minutes')
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_self   pg_catalog.int4;
    v_cat    pg_catalog.jsonb;
    v_major  pg_catalog.int4;
    v_ext    pg_catalog.text;
    v_skip   pg_catalog.int4[] := '{}'::pg_catalog.int4[];	-- nodes that could not be asked, or that run another version
    n        pg_catalog.record;
    st       pg_catalog.jsonb;
    x        pg_catalog.record;
    v_move   pg_catalog.int8;
    v_mrow   kingres.bucket_move%ROWTYPE;
    v_age    pg_catalog.float8 := extract(epoch FROM p_prepared_older_than);
    -- How long a move or an orphan table may wait for the work that the recovery
    -- worker does (and a person does when it is switched off) before it is a
    -- problem: the worker's pass and the longest wait of one of its steps.
    v_grace  pg_catalog.interval;
    v_ref    pg_catalog.text;
    v_dig    pg_catalog.text;
    v_bad    pg_catalog.int4[];
    v_det    pg_catalog.text[];
    v_try    pg_catalog.int4;
    i        pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_cat := kingres.internal_catalog_snapshot();
    v_major := (pg_catalog.current_setting('server_version_num')::pg_catalog.int4 OPERATOR(pg_catalog./) 10000);
    SELECT e.extversion INTO v_ext FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard';
    v_grace := pg_catalog.make_interval(secs => 2 OPERATOR(pg_catalog.*)
        (SELECT pg_catalog.sum(s.setting::pg_catalog.float8) FROM pg_catalog.pg_settings s
          WHERE s.name IN ('kingres_shard.node_timeout', 'kingres_shard.recovery_interval')));

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
    -- Moves that nobody runs: the session that runs a move holds a mark for as
    -- long as it lives (the recovery worker resumes the move that has none).  A
    -- move that stopped a moment ago, or that is waiting for its cleanup delay,
    -- is not a problem; one that nobody has touched for longer than the grace
    -- time is, and so is a move whose cleaning is overdue.
    FOR x IN SELECT b.move_id, b.phase, b.updated_at, b.cleanup_after FROM kingres.bucket_move b
              WHERE b.phase IN ('preparing', 'copying', 'catching_up', 'switching', 'cleaning')
                AND pg_catalog.clock_timestamp() OPERATOR(pg_catalog.>)
                    (CASE WHEN b.phase OPERATOR(pg_catalog.=) 'cleaning'
                          THEN GREATEST(b.updated_at, COALESCE(b.cleanup_after, b.updated_at))
                          ELSE b.updated_at END OPERATOR(pg_catalog.+) v_grace)
                AND NOT kingres.internal_move_alive(b.move_id)
              ORDER BY b.move_id
    LOOP
        check_name := 'stalled_move'; node_id := v_self;
        object := pg_catalog.format('bucket move %s', x.move_id);
        detail := pg_catalog.format('the move is %s, no session runs it and it was last touched at %s; kingres.resume_bucket_moves() (the recovery worker, when it is enabled) resolves it',
                                    x.phase, x.updated_at);
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
            v_skip := pg_catalog.array_append(v_skip, n.node_id);
            CONTINUE;
        END;

        -- A node that runs another version of the extension answers other questions than this
        -- node asks: say so, and make no comparison with it (a copy that cannot be read is not a
        -- copy that differs; review finding S5-02).
        IF (st ->> 'ext_version') IS DISTINCT FROM v_ext THEN
            check_name := 'extension_version_differs'; node_id := n.node_id;
            object := pg_catalog.format('%s:%s', n.host, n.port);
            detail := pg_catalog.format('the node runs kingres_shard %s, this node %s; nothing else is compared with it, and run ALTER EXTENSION kingres_shard UPDATE where the version is older',
                                        COALESCE(st ->> 'ext_version', 'an older version'), v_ext);
            RETURN NEXT;
            v_skip := pg_catalog.array_append(v_skip, n.node_id);
            CONTINUE;
        END IF;

        -- Prepared transactions that recovery rolled back after a failover of the group
        -- without finding a commit decision (see README, "Failover and prepared
        -- transactions"): the operator decides whether any of them was committed elsewhere.
        FOR x IN SELECT i.gid, i.detected_at, i.gid_timeline, i.current_timeline
                   FROM pg_catalog.jsonb_to_recordset(st -> 'incidents')
                        AS i(gid pg_catalog.text, detected_at pg_catalog.timestamptz, gid_timeline pg_catalog.int4, current_timeline pg_catalog.int4)
                  ORDER BY i.detected_at
        LOOP
            check_name := 'prepared_rolled_back_after_failover'; node_id := n.node_id; object := x.gid;
            detail := pg_catalog.format('rolled back at %s: it belonged to timeline %s and had no commit decision on timeline %s; if the old primary had committed it, other nodes may have too',
                                        x.detected_at, x.gid_timeline, x.current_timeline);
            RETURN NEXT;
        END LOOP;

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

        -- A node without the cluster secret: an ordinary role fails on every statement that needs another node.
        IF (st ->> 'has_secret') OPERATOR(pg_catalog.=) 'false' THEN
            check_name := 'secret_missing'; node_id := n.node_id;
            object := pg_catalog.format('%s:%s', n.host, n.port);
            detail := 'the node holds no cluster secret; statements of ordinary roles that need another node fail until kingres.sync_catalog() is run on the map group primary';
            RETURN NEXT;
        END IF;

        -- One PostgreSQL major per cluster: a node of another major than this one.
        IF (st ->> 'server_major') IS NOT NULL AND (st ->> 'server_major')::pg_catalog.int4 OPERATOR(pg_catalog.<>) v_major THEN
            check_name := 'server_major_differs'; node_id := n.node_id;
            object := pg_catalog.format('%s:%s', n.host, n.port);
            detail := pg_catalog.format('the node runs PostgreSQL %s, this node PostgreSQL %s; all nodes of a cluster must run the same major version',
                                        st ->> 'server_major', v_major);
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
               AND (v_mrow.cleanup_after IS NULL OR pg_catalog.clock_timestamp() OPERATOR(pg_catalog.<) (v_mrow.cleanup_after OPERATOR(pg_catalog.+) v_grace)) THEN
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

    -- The copies of every reference table: the number of rows and an
    -- order-independent digest of them on each active primary node, against
    -- the map group primary's.  A write that is in flight (committed on one
    -- node, not yet on another) shows as a difference once, so a difference is
    -- reported only if it is still there after two more looks.
    FOR x IN SELECT pg_catalog.format('%I.%I', ns.nspname, c.relname) AS qname
               FROM kingres.dist_table d
               JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid
               JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) c.relnamespace
              WHERE d.method OPERATOR(pg_catalog.=) 'reference' ORDER BY 1
    LOOP
        v_try := 0;
        LOOP
            v_try := v_try OPERATOR(pg_catalog.+) 1;
            v_ref := kingres.internal_reference_digest(x.qname);
            v_bad := '{}'::pg_catalog.int4[];
            v_det := '{}'::pg_catalog.text[];
            FOR n IN SELECT nd.node_id FROM kingres.node nd
                      WHERE nd.is_active AND nd.role OPERATOR(pg_catalog.=) 'primary'
                        AND nd.node_id OPERATOR(pg_catalog.<>) v_self
                        AND nd.node_id OPERATOR(pg_catalog.<>) ALL (v_skip) ORDER BY nd.node_id
            LOOP
                BEGIN
                    v_dig := kingres.internal_exec_scalar(n.node_id,
                        pg_catalog.format('SELECT kingres.internal_reference_digest(%L)', x.qname), 60000);
                EXCEPTION WHEN OTHERS THEN
                    -- An unreachable node is reported as such above; a table that is
                    -- not there is a copy that differs.
                    v_dig := CASE WHEN SQLERRM OPERATOR(pg_catalog.~) 'does not exist' THEN 'table missing' END;
                END;
                IF v_dig IS NOT NULL AND v_dig IS DISTINCT FROM v_ref THEN
                    v_bad := pg_catalog.array_append(v_bad, n.node_id);
                    v_det := pg_catalog.array_append(v_det, v_dig);
                END IF;
            END LOOP;
            EXIT WHEN v_bad = '{}'::pg_catalog.int4[] OR v_try OPERATOR(pg_catalog.>=) 3;
            PERFORM pg_catalog.pg_sleep(0.2 OPERATOR(pg_catalog.*) v_try);
        END LOOP;
        FOR i IN 1 .. COALESCE(pg_catalog.array_length(v_bad, 1), 0) LOOP
            check_name := 'reference_table_differs'; node_id := v_bad[i]; object := x.qname;
            detail := pg_catalog.format('rows:digest of this copy are %s, of the map group primary''s %s; the copies of a reference table must be equal',
                                        v_det[i], v_ref);
            RETURN NEXT;
        END LOOP;
    END LOOP;

    -- Standbys: registered, owning nothing; what is wrong with one is reported
    -- with its replication state as its primary sees it (kingres.standby_status()).
    FOR x IN SELECT s.node_id, s.host, s.port, s.problem, s.detail, s.replication_state, s.replay_lag_bytes
               FROM kingres.standby_status() s WHERE s.problem IS NOT NULL ORDER BY s.node_id
    LOOP
        check_name := x.problem; node_id := x.node_id;
        object := pg_catalog.format('%s:%s', x.host, x.port);
        detail := pg_catalog.format('%s (replication state: %s)', x.detail, x.replication_state);
        RETURN NEXT;
    END LOOP;
END;
$$;

