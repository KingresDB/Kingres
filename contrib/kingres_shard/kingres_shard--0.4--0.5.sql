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

-- The schema "kingres" is shared with the other KingresDB extensions, which ask for it
-- with "schema = kingres" in their control files, and any of them may be created
-- first.  So it belongs to none of them: whoever made it, it is not a member of this
-- extension, and DROP EXTENSION kingres_shard neither drops it nor fails because other
-- extensions keep objects in it (it stays, empty if this was the last user).  An
-- earlier version made it a member; the update takes it out.
DO $ks$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
                WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_namespace'::pg_catalog.regclass
                  AND d.objid OPERATOR(pg_catalog.=) 'kingres'::pg_catalog.regnamespace
                  AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_extension'::pg_catalog.regclass
                  AND d.refobjid OPERATOR(pg_catalog.=) (SELECT e.oid FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard')
                  AND d.deptype OPERATOR(pg_catalog.=) 'e') THEN
        ALTER EXTENSION kingres_shard DROP SCHEMA kingres;
    END IF;
END
$ks$;

-- ---------------------------------------------------------------------------
-- High availability (design/ha-phase5.md, part C)
-- ---------------------------------------------------------------------------

-- The timeline this server writes WAL on: what tells a node that was promoted in
-- PostgreSQL from the node the catalog named (S9-02).  A node records it in its
-- local_node row when it becomes the map group primary in the catalog; a physical
-- copy carries the row, and when the copy is promoted its timeline is another one.
CREATE FUNCTION kingres.internal_wal_timeline()
    RETURNS pg_catalog.int4
    AS 'MODULE_PATHNAME', 'kingres_internal_wal_timeline'
    LANGUAGE C STABLE
    SET search_path = pg_catalog, kingres, pg_temp;
REVOKE ALL ON FUNCTION kingres.internal_wal_timeline() FROM PUBLIC;

-- The timeline of the node when it became what it is (S9-02).
ALTER TABLE kingres.cluster_secret ADD COLUMN previous bytea, ADD COLUMN previous_at timestamptz;

ALTER TABLE kingres.local_node ADD COLUMN timeline int NOT NULL DEFAULT kingres.internal_wal_timeline();

-- Install the secret of the cluster on this node (the hex form of its bytes).  The
-- secret it replaces is kept as "previous" (S9-03): a node that holds a rotation
-- prepared and was not told that it committed still has the old one, and the node
-- that finishes the rotation proves itself to it with that.  The previous secret is
-- never accepted from anybody; recovery drops it once every primary has been reached
-- with the current one.
CREATE OR REPLACE FUNCTION kingres.internal_apply_secret(secret_hex pg_catalog.text)
    RETURNS pg_catalog.void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_new pg_catalog.bytea := pg_catalog.decode(secret_hex, 'hex');
BEGIN
    UPDATE kingres.cluster_secret c
       SET previous    = CASE WHEN c.secret OPERATOR(pg_catalog.=) v_new THEN c.previous ELSE c.secret END,
           previous_at = CASE WHEN c.secret OPERATOR(pg_catalog.=) v_new THEN c.previous_at
                              ELSE pg_catalog.clock_timestamp() END,
           secret      = v_new;
    IF NOT FOUND THEN
        INSERT INTO kingres.cluster_secret (secret) VALUES (v_new);
    END IF;
END;
$$;

-- Forget the previous cluster secret on this node (S10-04).  A recovery pass that reached every
-- primary with the current secret knows that no node still needs the previous one, and tells
-- the others to forget it: until then a leaked old secret keeps one of its powers (the tag of a
-- prepared transaction's name made with it counts).
CREATE FUNCTION kingres.internal_drop_previous_secret()
    RETURNS pg_catalog.void
    LANGUAGE sql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
UPDATE kingres.cluster_secret SET previous = NULL, previous_at = NULL WHERE previous IS NOT NULL;
$$;
REVOKE ALL ON FUNCTION kingres.internal_drop_previous_secret() FROM PUBLIC;

-- A decision is forgotten only once its participant's standbys have confirmed the commit (S9-01).
ALTER TABLE kingres.dtx_log ADD COLUMN durable bool NOT NULL DEFAULT false;
ALTER TABLE kingres.dtx_log ADD COLUMN exempt bool NOT NULL DEFAULT false;

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

-- Node-local, never synchronized: the participants whose commit decisions this
-- node keeps beyond kingres_shard.decision_retention because the participant's
-- synchronous standbys have not confirmed that its second phase is durable
-- (recovery.c, participant_confirmed).  A row exists while that is so; it is
-- removed when the standbys confirm or the decisions are gone.  check_cluster()
-- reports it as decision_unconfirmed.
CREATE TABLE kingres.dtx_unconfirmed (
    node_id int         NOT NULL,
    since   timestamptz NOT NULL,
    kept    int8        NOT NULL,
    CONSTRAINT dtx_unconfirmed_pkey PRIMARY KEY (node_id)
);

COMMENT ON TABLE kingres.dtx_unconfirmed IS
    'Node-local: participants whose commit decisions are kept without a time limit until their standbys confirm; never synchronized';

-- promote_node(node_id): see src/failover.c.
CREATE FUNCTION kingres.promote_node(node_id int)
    RETURNS jsonb
    AS 'MODULE_PATHNAME', 'kingres_promote_node'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

COMMENT ON FUNCTION kingres.promote_node(int) IS
    'Make a promoted standby the primary of its shard group (map group primary; for group 0 the promoted node itself); resumable; answers {status, term, unreached}';

-- The barrier of create_restore_point() on this node, and the mark of a
-- distributed-deadlock victim (src/ha.c).
CREATE FUNCTION kingres.internal_restore_barrier(op text, ttl_ms int DEFAULT NULL, generation bigint DEFAULT NULL)
    RETURNS jsonb
    AS 'MODULE_PATHNAME', 'kingres_internal_restore_barrier'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_mark_victim(pid int, waitstart bigint)
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
        IF kingres.internal_mark_victim(p_pid, p_waitstart) THEN
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
    kingres.internal_mark_victim(int, bigint),
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

-- The version of the extension that a node reports, asked over a session that claims
-- nothing: a node that runs another version of the library does not accept the claim of
-- a node's own session (review finding S6-06).  NULL when it cannot be asked, '' when
-- the extension is not installed there.  Superusers only.
CREATE FUNCTION kingres.internal_remote_extension_version(p_host text, p_port int)
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_internal_remote_extension_version'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

REVOKE ALL ON FUNCTION kingres.internal_remote_extension_version(text, int) FROM PUBLIC;

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
            -- The node may be reachable and refuse the claim of the session of this node (it runs
            -- another version of the library): ask for the version without a claim, so that the
            -- error names both versions (review finding S6-06).
            v := kingres.internal_remote_extension_version(n.host, n.port);
            IF v IS NULL THEN
                RAISE EXCEPTION 'node % (%:%) cannot be asked for its kingres_shard version: %', n.node_id, n.host, n.port, SQLERRM
                    USING ERRCODE = 'object_not_in_prerequisite_state';
            END IF;
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
    'unconfirmed', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                               'node_id', u.node_id, 'since', u.since, 'kept', u.kept))
                            FROM kingres.dtx_unconfirmed u), '[]'::pg_catalog.jsonb),
    'prepared', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                               'gid', x.gid, 'age', extract(epoch FROM pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) x.prepared),
                               'class', kingres.internal_gid_class(x.gid)))
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
    v_theirs pg_catalog.text;
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
            st := kingres.internal_exec_scalar(n.node_id, 'SELECT kingres.internal_local_state()::pg_catalog.text')::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            -- A node that accepted the session and did not answer within node_timeout (S10-02): the call
            -- does not wait for it again; a cancel by the caller is not swallowed.
            IF SQLSTATE OPERATOR(pg_catalog.=) '57014' THEN
                IF SQLERRM OPERATOR(pg_catalog.!~) 'timed out after' THEN
                    RAISE;
                END IF;
                check_name := 'node_unreachable'; node_id := n.node_id;
                object := pg_catalog.format('%s:%s', n.host, n.port);
                detail := pg_catalog.format('the node did not answer within node_timeout (%s): %s', SQLERRM,
                                            'it accepted the session and was waiting for its answer to the cluster state question; the other checks of this node are skipped');
                RETURN NEXT;
                v_skip := pg_catalog.array_append(v_skip, n.node_id);
                CONTINUE;
            END IF;
            -- A node that runs another version of the library refuses the claim of the session of
            -- this node to come from a node of the cluster, and that looks like a lost connection.
            -- Ask for its version over a session that claims nothing, and say the versions
            -- (review finding S6-06).
            v_theirs := kingres.internal_remote_extension_version(n.host, n.port);
            IF v_theirs IS NOT NULL AND v_theirs IS DISTINCT FROM v_ext THEN
                check_name := 'extension_version_differs'; node_id := n.node_id;
                object := pg_catalog.format('%s:%s', n.host, n.port);
                detail := pg_catalog.format('the node runs %s, this node runs kingres_shard %s; the node did not accept the session of this node, nothing else is compared with it, and run ALTER EXTENSION kingres_shard UPDATE where the version is older',
                                            CASE WHEN v_theirs OPERATOR(pg_catalog.=) '' THEN 'no kingres_shard'
                                                 ELSE 'kingres_shard ' OPERATOR(pg_catalog.||) v_theirs END, v_ext);
            ELSE
                check_name := 'node_unreachable'; node_id := n.node_id;
                object := pg_catalog.format('%s:%s', n.host, n.port); detail := SQLERRM;
            END IF;
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
            detail := pg_catalog.format('the node runs %s, this node runs kingres_shard %s; nothing else is compared with it, and run ALTER EXTENSION kingres_shard UPDATE where the version is older',
                                        COALESCE('kingres_shard ' OPERATOR(pg_catalog.||) (st ->> 'ext_version'),
                                                 'a kingres_shard that does not report its version'), v_ext);
            RETURN NEXT;
            v_skip := pg_catalog.array_append(v_skip, n.node_id);
            CONTINUE;
        END IF;

        -- Prepared transactions that recovery rolled back after a failover of the group
        -- without finding a commit decision (see README, "Failover and prepared
        -- transactions"): the operator decides whether any of them was committed elsewhere.
        FOR x IN SELECT i.gid, i.detected_at, i.gid_timeline, i.current_timeline, i.kind
                   FROM pg_catalog.jsonb_to_recordset(st -> 'incidents')
                        AS i(gid pg_catalog.text, detected_at pg_catalog.timestamptz, gid_timeline pg_catalog.int4, current_timeline pg_catalog.int4, kind pg_catalog.text)
                  ORDER BY i.detected_at
        LOOP
            IF x.kind OPERATOR(pg_catalog.=) 'participant_lost' THEN
                -- The origin committed it, and the primary that the participant group has now has no trace of its part.
                check_name := 'participant_lost_committed_transaction'; node_id := n.node_id; object := x.gid;
                detail := pg_catalog.format('found out at %s: the transaction was committed by its origin, and this node (the primary of its group after a failover) has no trace of its part; the changes it made on this group are lost',
                                            x.detected_at);
            ELSE
            check_name := 'prepared_rolled_back_after_failover'; node_id := n.node_id; object := x.gid;
            detail := pg_catalog.format('rolled back at %s: it belonged to timeline %s and had no commit decision on timeline %s; if the old primary had committed it, other nodes may have too',
                                        x.detected_at, x.gid_timeline, x.current_timeline);
            END IF;
            RETURN NEXT;
        END LOOP;

        -- Decisions kept without a time limit because the participant's standbys have not
        -- confirmed its second phase (S9-01): they are what brings a part back after a
        -- failover of that participant, and they are not forgotten before it is confirmed.
        FOR x IN SELECT u.node_id AS pnode, u.since AS detected_at, u.kept
                   FROM pg_catalog.jsonb_to_recordset(st -> 'unconfirmed')
                        AS u(node_id pg_catalog.int4, since pg_catalog.timestamptz, kept pg_catalog.int8)
                  ORDER BY u.since
        LOOP
            check_name := 'decision_unconfirmed'; node_id := n.node_id;
            object := pg_catalog.format('node %s', x.pnode);
            detail := pg_catalog.format('since %s this node keeps %s commit decision(s) for node %s: its synchronous standbys have not confirmed that its commits are durable, so a failover of it now would bring its part back prepared and recovery needs the decision; they are kept, with no time limit, until the standbys confirm. Start the standby, or take its name out of the primary''s synchronous_standby_names (then the retention applies and the weaker case of the README holds)',
                                        x.detected_at, x.kept, x.pnode);
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
        -- A name of the form of the extension's own whose tag does not verify (S7-09): a role prepared it,
        -- no node of this cluster did, and recovery leaves it alone.
        FOR x IN SELECT p.gid FROM pg_catalog.jsonb_to_recordset(st -> 'prepared') AS p(gid text, class text)
                  WHERE p.class OPERATOR(pg_catalog.=) 'forged' ORDER BY p.gid
        LOOP
            check_name := 'foreign_prepared_transaction'; node_id := n.node_id; object := x.gid;
            detail := 'a prepared transaction with the name of one of ours that does not carry a valid tag: either no node of this cluster made it, or it was prepared in the moments around a kingres.rotate_secret() (which refuses while transactions are in doubt, but cannot stop one that prepares at that instant); recovery leaves it alone. If it is the cluster''s own, resolve it by hand on this node: COMMIT PREPARED if its gid has a row in kingres.dtx_log on the origin, ROLLBACK PREPARED if it has none';
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


-- ---------------------------------------------------------------------------
-- Review S7: the catalog term (src/term.c)
-- ---------------------------------------------------------------------------

-- The term is raised by every promote_node(); every catalog push carries it
-- and a node refuses a push of an older term (KS002).  superseded_by_term is
-- node-local: the newer term that this node, acting as the map group primary,
-- was refused with; while it is above the term the node changes no catalog.
ALTER TABLE kingres.map_version
    ADD COLUMN term bigint NOT NULL DEFAULT 0,
    ADD COLUMN superseded_by_term bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN kingres.map_version.term IS
    'Catalog term: raised by every promote_node(), carried by every catalog push and remote transaction';
COMMENT ON COLUMN kingres.map_version.superseded_by_term IS
    'Node-local, never synchronized: the newer term this node was refused with while it acted as the map group primary; 0 when not superseded';

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
                                       'colocation_id', d.colocation_id)
                                     ORDER BY d.colocation_id, ns.nspname, cl.relname)
                              FROM kingres.dist_table d
                              JOIN pg_catalog.pg_class cl ON cl.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid
                              JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) cl.relnamespace),
                           '[]'::pg_catalog.jsonb),
    'bucket_map', COALESCE((SELECT jsonb_agg(to_jsonb(b) ORDER BY b.colocation_id, b.bucket)
                              FROM kingres.bucket_map b), '[]'::pg_catalog.jsonb));
$$;

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
        -- Plans of statements on the table were made with the old catalog.
        PERFORM kingres.internal_invalidate_relation(rid);
    END LOOP;

    INSERT INTO kingres.bucket_map
        SELECT * FROM pg_catalog.jsonb_populate_recordset(NULL::kingres.bucket_map,
                                    snapshot OPERATOR(pg_catalog.->) 'bucket_map');

    -- A node that was superseded and now follows the newer catalog is no longer
    -- superseded.
    UPDATE kingres.map_version
       SET version = (snapshot OPERATOR(pg_catalog.->>) 'version')::pg_catalog.int8,
           term = new_term,
           superseded_by_term = CASE WHEN new_term OPERATOR(pg_catalog.>=) superseded_by_term
                                     THEN 0 ELSE superseded_by_term END;
END;
$$;

-- What a node says about itself for promote_node(): which node it believes it
-- is, the catalog it holds, whether it is in recovery, and how many catalog
-- changes are prepared on it and unresolved (a prepared transaction holds a
-- row lock on kingres.map_version; every catalog change writes it).
CREATE FUNCTION kingres.internal_promotion_state(OUT local_node int, OUT term bigint,
                                                 OUT version bigint, OUT in_recovery bool,
                                                 OUT pending int, OUT blockers jsonb)
    RETURNS record
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
-- Prepared transactions that hold a lock on kingres.map_version that conflicts with a catalog change
-- (anything above ACCESS SHARE, which only readers take and which blocks nobody): the ones a
-- promotion has to wait for, with the class of their gid so that a caller can tell a transaction of
-- the cluster, which recovery resolves, from one that is somebody else's (S9-04).
WITH b AS (
    SELECT DISTINCT x.gid AS gid, kingres.internal_gid_class(x.gid) AS class
      FROM pg_catalog.pg_locks k
      JOIN pg_catalog.pg_locks t
        ON t.virtualtransaction OPERATOR(pg_catalog.=) k.virtualtransaction
       AND t.locktype OPERATOR(pg_catalog.=) 'transactionid' AND t.pid IS NULL
      JOIN pg_catalog.pg_prepared_xacts x
        ON x.transaction OPERATOR(pg_catalog.=) t.transactionid
     WHERE k.locktype OPERATOR(pg_catalog.=) 'relation'
       AND k.pid IS NULL
       AND k.mode OPERATOR(pg_catalog.<>) 'AccessShareLock'
       AND k.database OPERATOR(pg_catalog.=) (SELECT d.oid FROM pg_catalog.pg_database d
                                               WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database())
       AND k.relation OPERATOR(pg_catalog.=) 'kingres.map_version'::pg_catalog.regclass
)
SELECT (SELECT l.node_id FROM kingres.local_node l),
       (SELECT m.term FROM kingres.map_version m),
       (SELECT m.version FROM kingres.map_version m),
       pg_catalog.pg_is_in_recovery(),
       (SELECT pg_catalog.count(*)::pg_catalog.int4 FROM b),
       COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('gid', b.gid, 'class',
                        CASE b.class WHEN 'tagged' THEN 'cluster' WHEN 'untagged' THEN 'cluster' ELSE 'foreign' END)
                        ORDER BY b.gid) FROM b), '[]'::pg_catalog.jsonb);
$$;

REVOKE ALL ON FUNCTION kingres.internal_promotion_state() FROM PUBLIC;

-- The catalog term of this node and whether it has been superseded.
CREATE VIEW kingres.catalog_status AS
    SELECT m.term, m.superseded_by_term,
           (m.superseded_by_term OPERATOR(pg_catalog.>) m.term) AS superseded,
           m.version AS map_version
      FROM kingres.map_version m;

COMMENT ON VIEW kingres.catalog_status IS
    'The catalog term of this node; superseded is true on an old map group primary that learned of a newer term';

-- ---------------------------------------------------------------------------
-- Review S7: the identity of a distributed transaction (src/ha.c)
-- ---------------------------------------------------------------------------

-- The identity that a node session proved and registered for its remote
-- transaction (origin node, origin backend, start of the origin transaction),
-- as the label "ks-dtx:<node>:<pid>:<start>", or NULL for a backend that is not
-- such a session.  The identity is kept in shared memory by the extension
-- itself, set only by a session that proved to come from a node of the cluster
-- (forward_auth.c): a client cannot choose it, which the label in
-- application_name allowed (S7-02, S7-03).  Superusers only.
CREATE FUNCTION kingres.internal_dtx_identity(pid int)
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_internal_dtx_identity'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

REVOKE ALL ON FUNCTION kingres.internal_dtx_identity(int) FROM PUBLIC;

-- The label of the distributed transaction a backend works for: the identity of
-- a node session, and for every other backend its own: this node, its pid, the
-- start of its transaction (what pg_stat_activity says, which no client can set).
-- The application_name is not looked at.
CREATE OR REPLACE FUNCTION kingres.internal_dtx_label_of(p_pid int, p_node int)
    RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT COALESCE(kingres.internal_dtx_identity(p_pid),
                CASE WHEN a.xact_start IS NOT NULL
                     THEN pg_catalog.format('ks-dtx:%s:%s:%s', p_node, a.pid,
                                            (extract(epoch FROM a.xact_start) * 1000000)::pg_catalog.int8)
                END)
  FROM pg_catalog.pg_stat_activity a
 WHERE a.pid = p_pid;
$$;

-- The edges of every active primary.  An edge that is not well formed (a label
-- that is not "ks-dtx:" and three numbers) is left out and logged, once per
-- pass and node: one bad entry never stops the pass.
CREATE OR REPLACE FUNCTION kingres.internal_collect_wait_edges()
    RETURNS jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    n   pg_catalog.record;
    v_all pg_catalog.jsonb := '[]'::pg_catalog.jsonb;
    v_e   pg_catalog.jsonb;
    v_good pg_catalog.jsonb;
    v_bad pg_catalog.int4;
BEGIN
    FOR n IN SELECT nd.node_id FROM kingres.node nd
              WHERE nd.is_active AND nd.role = 'primary' ORDER BY nd.node_id
    LOOP
        BEGIN
            v_e := COALESCE(kingres.internal_exec_scalar(n.node_id,
                'SELECT kingres.internal_wait_edges()::pg_catalog.text', 10000)::pg_catalog.jsonb,
                '[]'::pg_catalog.jsonb);
            SELECT COALESCE(pg_catalog.jsonb_agg(e.x) FILTER (WHERE e.ok), '[]'::pg_catalog.jsonb),
                   (pg_catalog.count(*) FILTER (WHERE NOT e.ok))::pg_catalog.int4
              INTO v_good, v_bad
              FROM (SELECT x, COALESCE(
                           pg_catalog.jsonb_typeof(x) = 'object'
                           AND (x ->> 'waiter') ~ '^ks-dtx:[0-9]{1,9}:[0-9]{1,9}:[0-9]{1,18}$'
                           AND (x ->> 'blocker') ~ '^ks-dtx:[0-9]{1,9}:[0-9]{1,9}:[0-9]{1,18}$'
                           AND (x ->> 'node') ~ '^[0-9]{1,9}$'
                           AND (x ->> 'waiter_pid') ~ '^[0-9]{1,9}$'
                           AND (x ->> 'waitstart') ~ '^[0-9]{1,18}$', false) AS ok
                      FROM pg_catalog.jsonb_array_elements(v_e) x) e;
            IF v_bad > 0 THEN
                RAISE LOG 'kingres_shard deadlock detector: node % sent % wait-for entries that are not well formed; they were ignored',
                    n.node_id, v_bad;
            END IF;
            v_all := v_all || v_good;
        EXCEPTION WHEN OTHERS THEN
            RAISE LOG 'kingres_shard deadlock detector: node % could not be asked: %', n.node_id, SQLERRM;
        END;
    END LOOP;
    RETURN v_all;
END;
$$;

-- The decision of the detector on two readings; an edge whose label is not well
-- formed is ignored here as well (the readings may come from anywhere, and a
-- cast of a bad label must never stop the pass; S7-03).
CREATE OR REPLACE FUNCTION kingres.internal_deadlock_victims(p_first jsonb, p_second jsonb)
    RETURNS TABLE (victim text, victim_node int, victim_pid int, victim_waitstart bigint, cycle text[])
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
WITH RECURSIVE
first_ok AS (
    SELECT x FROM pg_catalog.jsonb_array_elements(p_first) x
     WHERE pg_catalog.jsonb_typeof(x) = 'object'
       AND (x ->> 'waiter') ~ '^ks-dtx:[0-9]{1,9}:[0-9]{1,9}:[0-9]{1,18}$'
       AND (x ->> 'blocker') ~ '^ks-dtx:[0-9]{1,9}:[0-9]{1,9}:[0-9]{1,18}$'
       AND (x ->> 'node') ~ '^[0-9]{1,9}$' AND (x ->> 'waiter_pid') ~ '^[0-9]{1,9}$'
       AND (x ->> 'waitstart') ~ '^[0-9]{1,18}$'
       AND COALESCE(x ->> 'blocker_pid', '0') ~ '^[0-9]{1,9}$'),
second_ok AS (
    SELECT x FROM pg_catalog.jsonb_array_elements(p_second) x
     WHERE pg_catalog.jsonb_typeof(x) = 'object'
       AND (x ->> 'waiter') ~ '^ks-dtx:[0-9]{1,9}:[0-9]{1,9}:[0-9]{1,18}$'
       AND (x ->> 'blocker') ~ '^ks-dtx:[0-9]{1,9}:[0-9]{1,9}:[0-9]{1,18}$'
       AND (x ->> 'node') ~ '^[0-9]{1,9}$' AND (x ->> 'waiter_pid') ~ '^[0-9]{1,9}$'
       AND (x ->> 'waitstart') ~ '^[0-9]{1,18}$'
       AND COALESCE(x ->> 'blocker_pid', '0') ~ '^[0-9]{1,9}$'),
stable_edges AS (
    SELECT DISTINCT a.node, a.waiter_pid, a.waitstart, a.waiter, a.blocker
      FROM pg_catalog.jsonb_to_recordset((SELECT COALESCE(pg_catalog.jsonb_agg(x), '[]'::pg_catalog.jsonb) FROM first_ok))
           AS a(node int, waiter_pid int, waitstart int8, waiter text, blocker text, blocker_pid int)
      JOIN pg_catalog.jsonb_to_recordset((SELECT COALESCE(pg_catalog.jsonb_agg(x), '[]'::pg_catalog.jsonb) FROM second_ok))
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

-- A victim mark that the victim's own error did not consume is removed by the
-- detector after the cancel (S7-10): the cancel did not reach the wait.
CREATE FUNCTION kingres.internal_unmark_victim(pid int)
    RETURNS bool
    AS 'MODULE_PATHNAME', 'kingres_internal_unmark_victim'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

REVOKE ALL ON FUNCTION kingres.internal_unmark_victim(int) FROM PUBLIC;

CREATE OR REPLACE FUNCTION kingres.internal_cancel_waiter(p_pid int, p_waitstart bigint)
    RETURNS bool
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_ok pg_catalog.bool := false;
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_locks l
                WHERE l.pid = p_pid AND NOT l.granted
                  AND (extract(epoch FROM l.waitstart) * 1000000)::pg_catalog.int8 = p_waitstart) THEN
        IF kingres.internal_mark_victim(p_pid, p_waitstart) THEN
            v_ok := pg_catalog.pg_cancel_backend(p_pid);
            -- The victim's error consumes the mark; one that is still there after a
            -- moment means that the cancel found no wait to end.
            PERFORM pg_catalog.pg_sleep(0.1);
            IF kingres.internal_unmark_victim(p_pid) THEN
                RETURN false;
            END IF;
        END IF;
    END IF;
    RETURN v_ok;
END;
$$;

-- ---------------------------------------------------------------------------
-- Review S7-08: the barrier of create_restore_point() falls with its caller
-- ---------------------------------------------------------------------------

-- Tell this session that it has raised the barrier of node p_node with the given
-- generation (0: lowered).  When the transaction of this session aborts (a cancel,
-- a timeout, an error) the session lowers every barrier it still owes, over the
-- control sessions it holds (src/bucket_move.c).  A node also lowers a barrier whose
-- raising session has ended (src/ha.c), so a terminated caller or a lost
-- connection does not leave it up either.
CREATE FUNCTION kingres.internal_note_barrier(p_node int, p_generation bigint)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_note_barrier'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

REVOKE ALL ON FUNCTION kingres.internal_note_barrier(int, bigint) FROM PUBLIC;

CREATE OR REPLACE FUNCTION kingres.create_restore_point(name text)
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
            -- The calling session now owes this barrier a lowering, whatever way it ends (S7-08).
            PERFORM kingres.internal_note_barrier(v_nodes[i], (v_st ->> 'generation')::pg_catalog.int8);
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
                PERFORM kingres.internal_note_barrier(v_nodes[i], 0);
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
        PERFORM kingres.internal_note_barrier(v_nodes[i], 0);
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


-- ---------------------------------------------------------------------------
-- Review S7-09: the identifier of a two-phase transaction carries a tag
-- ---------------------------------------------------------------------------

-- Node-local, never synchronized: the moment this node came to the version that
-- tags the identifiers.  A prepared transaction with an identifier of the older
-- form (no tag) that was prepared before it is resolved as before; one prepared
-- after it is not the extension's (src/recovery.c).
CREATE TABLE kingres.dtx_epoch (
    old_format_until timestamptz NOT NULL
);
CREATE UNIQUE INDEX dtx_epoch_single_row_idx ON kingres.dtx_epoch ((true));
INSERT INTO kingres.dtx_epoch VALUES (pg_catalog.now());

COMMENT ON TABLE kingres.dtx_epoch IS
    'Node-local: prepared transactions with an untagged identifier are resolved only when prepared before this time; never synchronized';

-- The tag of an identifier (the first 16 hex digits of an HMAC of the name with
-- the cluster secret), and what kind of name a gid is: 'tagged' (the tag
-- verifies), 'forged' (the form of a tagged name, the tag does not verify),
-- 'untagged' (the older forms) or 'none'.  Superusers only.
CREATE FUNCTION kingres.internal_gid_tag(body text)
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_internal_gid_tag'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION kingres.internal_gid_class(gid text)
    RETURNS text
    AS 'MODULE_PATHNAME', 'kingres_internal_gid_class'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

REVOKE ALL ON FUNCTION kingres.internal_gid_tag(text), kingres.internal_gid_class(text) FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- Review S7-07: a participant that failed over and lost its part is found out
-- ---------------------------------------------------------------------------

-- Node-local, replicated with the node: the identifiers of the distributed
-- transactions that this node took part in, written by the participant's own
-- transaction just before its PREPARE (so a row is there exactly when the part
-- is committed, and a standby that lost the commit does not have it).  The origin
-- looks here when the primary of a participant group changed (src/recovery.c).
-- Recovery removes them after kingres_shard.decision_retention seconds and a minute.
CREATE TABLE kingres.dtx_applied (
    gid        text        NOT NULL,
    applied_at timestamptz NOT NULL,
    CONSTRAINT dtx_applied_pkey PRIMARY KEY (gid)
);

COMMENT ON TABLE kingres.dtx_applied IS
    'Node-local: distributed transactions this node prepared a part of, written in that part; the origin compares it after a failover of this group';

-- The cleanup looks for the rows that are older than decision_retention (plus a minute): an index keeps
-- it from reading the whole table on every pass of the recovery worker (S8-06).
CREATE INDEX dtx_applied_applied_at_idx ON kingres.dtx_applied (applied_at);

CREATE FUNCTION kingres.internal_dtx_applied(gid text)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_dtx_applied'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

-- Executable by every role on purpose: the remote transaction of a participant runs as the role of the
-- user whose statement it is.  The function itself refuses everyone but a node session and a superuser.

-- Does the synchronous standby of this node confirm everything that the node has flushed so far, as its
-- own configuration requires?  The origin of a distributed transaction asks it of a participant right after
-- the participant's PREPARE TRANSACTION and COMMIT PREPARED, whose wait for the standby can end early
-- (cancel, termination, shutdown) with a warning that the origin cannot see (S8-01).  Answers false after
-- timeout_ms milliseconds.  Executable by every role, like internal_dtx_applied, for the same reason.
CREATE FUNCTION kingres.internal_sync_confirm(timeout_ms integer)
    RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_internal_sync_confirm'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

-- Can this node commit at all?  Writes a record and waits for its synchronous standbys to confirm it;
-- promote_node() asks it of a node before it changes anything (S8-02).  Superusers and node sessions only.
CREATE FUNCTION kingres.internal_sync_probe(timeout_ms integer)
    RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_internal_sync_probe'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;
REVOKE ALL ON FUNCTION kingres.internal_sync_probe(integer) FROM PUBLIC;

-- A node that leaves the cluster forgets its catalog term as well: it may join
-- another cluster, whose term is lower than the one it had.
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
UPDATE kingres.map_version SET version = 0, term = 0, superseded_by_term = 0;
$$;

-- A bucket move whose table synchronization lost its slot on the source fails (and is undone) instead of waiting for ever.
-- On the target: the prefixes of the table-sync slots of the tables whose copy is finished
-- (state "f") and that wait for their worker to hand over.  Such a worker needs its slot on the
-- source.  A table-sync worker drops its slot and only then records that it is done; a target
-- that stops between the two (a crash) restarts a worker that asks for a slot that is not there,
-- and no error counter of the subscription counts it (S8, t/074).
CREATE FUNCTION kingres.internal_move_sync_prefixes(p_name text)
    RETURNS jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.format('pg_%s_sync_%s_', s.oid, r.srrelid)), '[]'::pg_catalog.jsonb)
  FROM pg_catalog.pg_subscription s
  JOIN pg_catalog.pg_subscription_rel r ON r.srsubid OPERATOR(pg_catalog.=) s.oid
 WHERE s.subname OPERATOR(pg_catalog.=) p_name AND r.srsubstate OPERATOR(pg_catalog.=) 'f';
$$;

-- On the source: how many of those prefixes have no slot.
CREATE FUNCTION kingres.internal_move_slots_missing(p_prefixes jsonb)
    RETURNS int
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.count(*)::pg_catalog.int4
  FROM pg_catalog.jsonb_array_elements_text(p_prefixes) AS p(prefix)
 WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_replication_slots sl
                    WHERE pg_catalog.starts_with(sl.slot_name, p.prefix));
$$;

REVOKE ALL ON FUNCTION kingres.internal_move_sync_prefixes(text), kingres.internal_move_slots_missing(jsonb) FROM PUBLIC;

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

-- ---------------------------------------------------------------------------
-- The check of a planned switchover (S9-07)
-- ---------------------------------------------------------------------------

-- On the node that hands over, after the application's writes to its group have
-- stopped: write a record into the WAL, flush it, and return its position.  A node
-- that has replayed this position has every commit that was acknowledged before it.
CREATE FUNCTION kingres.switchover_mark()
    RETURNS pg_catalog.pg_lsn
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    IF pg_catalog.pg_is_in_recovery() THEN
        RAISE EXCEPTION 'this node is in recovery: the mark is written by the node that hands over'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    RETURN pg_catalog.pg_logical_emit_message(false, 'kingres_shard', 'switchover', true);
END;
$$;

-- On the node that takes over, before or after pg_promote(): has this node replayed the mark
-- that switchover_mark() returned?  This says only that: replay position >= mark.  It does NOT
-- say that nothing is lost: whatever the old primary wrote after the mark is not covered (S10-01).
-- The proof is switchover_verify() below.
CREATE FUNCTION kingres.switchover_replayed(mark_lsn pg_catalog.pg_lsn)
    RETURNS pg_catalog.bool
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_replayed pg_catalog.pg_lsn;
BEGIN
    v_replayed := pg_catalog.pg_last_wal_replay_lsn();
    IF v_replayed IS NULL THEN
        RETURN false;
    END IF;
    RETURN v_replayed OPERATOR(pg_catalog.>=) mark_lsn;
END;
$$;

-- The proof that a planned switchover loses nothing (S10-01), on the node that takes over,
-- after the old primary has stopped: the arguments are "Latest checkpoint location" and
-- "Database cluster state" of pg_controldata of the stopped old primary.  True only when the
-- state is exactly 'shut down' and this node's own WAL holds a shutdown checkpoint at the
-- position, replayed as the very last record.  False: not replayed yet, ask again.  An error
-- (KS006): it can never be proven, handle the promotion as a failover.
CREATE FUNCTION kingres.switchover_verify(shutdown_checkpoint pg_catalog.pg_lsn, cluster_state pg_catalog.text)
    RETURNS pg_catalog.bool
    AS 'MODULE_PATHNAME', 'kingres_switchover_verify'
    LANGUAGE C STABLE
    SET search_path = pg_catalog, kingres, pg_temp;

REVOKE ALL ON FUNCTION kingres.switchover_mark(), kingres.switchover_replayed(pg_lsn), kingres.switchover_verify(pg_lsn, text) FROM PUBLIC;
COMMENT ON FUNCTION kingres.switchover_mark() IS
    'Planned switchover: write and flush a WAL record on the old primary and return its position';
COMMENT ON FUNCTION kingres.switchover_replayed(pg_lsn) IS
    'Planned switchover: has this node replayed the mark (only that: nothing is said about what the old primary wrote after it)';
COMMENT ON FUNCTION kingres.switchover_verify(pg_lsn, text) IS
    'Planned switchover: true only when this node has replayed the stopped old primary''s shutdown checkpoint as the last record and the old primary''s state is shut down; false: not yet; error KS006: cannot be proven';

-- The same check as the C functions that change the catalog make (S9-02): the node must be the
-- map group primary in the catalog and in PostgreSQL, and not superseded.
CREATE OR REPLACE FUNCTION kingres.internal_require_map_group()
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_internal_require_map_group'
    LANGUAGE C
    SET search_path = pg_catalog, kingres, pg_temp;

-- The operator's way out when the map group cannot be promoted by the rule of the majority
-- because the groups that were lost with it are half or more of the others (S9-10).  Deliberately
-- awkward: the second argument must be the exact text below, and the catalog term jumps by 1000.
CREATE FUNCTION kingres.promote_map_group_without_majority(node_id int, acknowledgement text)
    RETURNS jsonb
    AS 'MODULE_PATHNAME', 'kingres_promote_node_forced'
    LANGUAGE C STRICT
    SET search_path = pg_catalog, kingres, pg_temp;

REVOKE ALL ON FUNCTION kingres.promote_map_group_without_majority(int, text) FROM PUBLIC;
COMMENT ON FUNCTION kingres.promote_map_group_without_majority(int, text) IS
    'Promote the map group standby that this node is without the rule of the majority, on the operator''s written word that every other node is fenced; the catalog term jumps by 1000';

-- S9: the subscription of a move does not wait for the target's standby (see the install script).
CREATE OR REPLACE FUNCTION kingres.internal_create_move_subscription(p_name text, p_host text, p_port int)
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
                              'enabled = true, synchronous_commit = ''local'', streaming = ''off'', '
                              'binary = false, origin = ''any'', disable_on_error = false)',
                              p_name, kingres.internal_subscription_conninfo(p_host, p_port), p_name, p_name);
END;
$$;

-- Would an element of synchronous_standby_names that reads list_name match a standby that streams
-- under application_name?  What PostgreSQL decides: names compared without regard to case, and
-- the element * matches every standby (S10-03).
CREATE FUNCTION kingres.internal_sync_name_matches(list_name pg_catalog.text, application_name pg_catalog.text)
    RETURNS pg_catalog.bool
    AS 'MODULE_PATHNAME', 'kingres_internal_sync_name_matches'
    LANGUAGE C IMMUTABLE STRICT
    SET search_path = pg_catalog, kingres, pg_temp;
REVOKE ALL ON FUNCTION kingres.internal_sync_name_matches(text, text) FROM PUBLIC;

-- The check of the name of a standby before it goes into synchronous_standby_names
-- (README, "Registering a standby", step 2).  On the primary of the group: does a connected
-- walreceiver that is streaming report this application_name, and does this node's group have
-- a registered standby?  A name that matches nobody makes every commit that writes in the group
-- wait for ever.  pg_stat_replication does not say which registered node a walsender serves, so
-- this answers "some streaming standby of this primary carries the name", which is what the list
-- is matched against.
CREATE FUNCTION kingres.standby_name_matches(standby_name pg_catalog.text)
    RETURNS pg_catalog.bool
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_replication r
                WHERE kingres.internal_sync_name_matches(standby_name, r.application_name)
                  AND r.state OPERATOR(pg_catalog.=) 'streaming')
   AND EXISTS (SELECT 1
                 FROM kingres.local_node l
                 JOIN kingres.node me ON me.node_id OPERATOR(pg_catalog.=) l.node_id
                 JOIN kingres.node s ON s.group_id OPERATOR(pg_catalog.=) me.group_id
                                    AND s.role OPERATOR(pg_catalog.=) 'standby');
$$;

REVOKE ALL ON FUNCTION kingres.standby_name_matches(text) FROM PUBLIC;
COMMENT ON FUNCTION kingres.standby_name_matches(text) IS
    'On the primary of a group: does a streaming standby carry this application_name (the check before the name goes into synchronous_standby_names)';
