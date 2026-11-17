-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 11: the tables of a failover (design ha-phase5-part2.md, 3.8) and its exclusion rules.
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.
--
-- Nothing writes these tables yet (the failover task is the next increment): they are here so that the
-- schema, the exclusion and the tests that prove both exist before the code that depends on them.
--
-- Exclusion (replaces the function of migration 0009; the trigger is the same):
--
--   * a rebalance that is pending or running is refused (KX001) while a task of kind server.add,
--     server.remove, server.leave, server.failover or replication.restorePreferredPrimaries is pending,
--     running or paused; and the other way round (KX002) while a rebalance is pending or running;
--   * at most one task of the failover class (server.failover, replication.restorePreferredPrimaries) is
--     unfinished at any time, cluster-wide (KX003);
--   * the record of an instance stop, an instance restart or a config restart (tasks kingresd does not
--     run, written by POST /task/record) is refused (KX004) while an unfinished failover run has a
--     group whose old or new node is that instance.

CREATE TABLE kingres_cp.failover_run (
    task_id         bigint PRIMARY KEY,
    kind            text NOT NULL CHECK (kind IN ('failover', 'restorePreferredPrimaries')),
    trigger         text NOT NULL CHECK (trigger IN ('manual', 'automatic')),
    mode            text,
    server_id       text,
    requested_by    text,
    options         jsonb NOT NULL DEFAULT '{}',
    expected_term   bigint,
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    outcome         text CHECK (outcome IN ('completed', 'partial', 'failed', 'cancelled')),
    counted         boolean NOT NULL DEFAULT false,
    error           text
);

CREATE TABLE kingres_cp.failover_group (
    task_id             bigint NOT NULL REFERENCES kingres_cp.failover_run (task_id) ON DELETE CASCADE,
    group_id            integer NOT NULL,
    seq                 integer NOT NULL,
    state               text NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('pending', 'frozen', 'fenced', 'pinned', 'promoted', 'done', 'failed', 'skipped')),
    old_server          text,
    old_node_id         integer,
    new_server          text,
    new_node_id         integer,
    chosen_standby_id   bigint,
    decision            jsonb,
    data_loss           jsonb,
    term_before         bigint,
    term_after          bigint,
    timeline            bigint,
    lease_wait_ms       integer,
    unreached           jsonb,
    incidents           jsonb,
    old_primary         text,
    pending_sync_catalog boolean NOT NULL DEFAULT false,
    started_at          timestamptz,
    promoted_at         timestamptz,
    finished_at         timestamptz,
    error               text,
    PRIMARY KEY (task_id, group_id)
);

CREATE TABLE kingres_cp.failover_event (
    task_id   bigint NOT NULL REFERENCES kingres_cp.failover_run (task_id) ON DELETE CASCADE,
    seq       integer NOT NULL,
    at        timestamptz NOT NULL DEFAULT now(),
    group_id  integer,
    step      text,
    kind      text NOT NULL,
    detail    jsonb,
    PRIMARY KEY (task_id, seq)
);

-- What the leader last knew of each group's primary (updated by the standby check).
CREATE TABLE kingres_cp.group_replication_state (
    group_id        integer PRIMARY KEY,
    primary_server  text,
    flush_lsn       text,
    sync_streaming  boolean,
    without_sync    boolean,
    seen_at         timestamptz NOT NULL DEFAULT now()
);

-- A standby whose history may have left its primary's (a former primary) is rebuilt with pg_rewind first.
ALTER TABLE kingres_cp.standby ADD COLUMN diverged boolean NOT NULL DEFAULT false;

CREATE OR REPLACE FUNCTION kingres_cp.task_exclusion() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    other text;
BEGIN
    IF NEW.state NOT IN ('pending', 'running', 'paused') THEN
        RETURN NEW;
    END IF;
    -- only a task that becomes active is checked: a new one, or a paused one that is resumed
    IF TG_OP = 'UPDATE' AND NOT (OLD.state = 'paused' AND NEW.state IN ('pending', 'running')) THEN
        RETURN NEW;
    END IF;
    IF NEW.kind = 'rebalance' AND NEW.state IN ('pending', 'running') THEN
        PERFORM kingres_cp.lock_operations();
        SELECT kind INTO other FROM kingres_cp.task
            WHERE kind IN ('server.add', 'server.remove', 'server.leave', 'server.failover', 'replication.restorePreferredPrimaries')
              AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'a rebalance cannot start while a task of kind % is unfinished', other
                USING ERRCODE = 'KX001', DETAIL = other;
        END IF;
    ELSIF NEW.kind IN ('server.add', 'server.remove', 'server.leave', 'server.failover', 'replication.restorePreferredPrimaries') THEN
        PERFORM kingres_cp.lock_operations();
        IF EXISTS (SELECT 1 FROM kingres_cp.task WHERE kind = 'rebalance' AND state IN ('pending', 'running') AND id <> NEW.id) THEN
            RAISE EXCEPTION 'a task of kind % cannot start while a rebalance is running', NEW.kind
                USING ERRCODE = 'KX002', DETAIL = 'rebalance';
        END IF;
        IF NEW.kind IN ('server.failover', 'replication.restorePreferredPrimaries') THEN
            SELECT kind INTO other FROM kingres_cp.task
                WHERE kind IN ('server.failover', 'replication.restorePreferredPrimaries')
                  AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
            IF FOUND THEN
                RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is unfinished', NEW.kind, other
                    USING ERRCODE = 'KX003', DETAIL = other;
            END IF;
        END IF;
    ELSIF NEW.kind IN ('instance.stop', 'instance.restart', 'config.restart') AND NEW.subject_type = 'instance' THEN
        PERFORM kingres_cp.lock_operations();
        IF EXISTS (SELECT 1
                   FROM kingres_cp.failover_run r
                   JOIN kingres_cp.task t ON t.id = r.task_id AND t.state IN ('pending', 'running', 'paused')
                   JOIN kingres_cp.failover_group g ON g.task_id = r.task_id
                       AND g.state NOT IN ('done', 'failed', 'skipped')
                   WHERE NEW.subject_id IN (g.old_node_id::text, g.new_node_id::text)) THEN
            RAISE EXCEPTION 'the instance % belongs to a group that is being failed over', NEW.subject_id
                USING ERRCODE = 'KX004', DETAIL = 'server.failover';
        END IF;
    END IF;
    RETURN NEW;
END $$;
