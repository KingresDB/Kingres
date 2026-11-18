-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 13: the exclusion of a stop or restart during a failover covers every instance of the server, and
-- a config restart (review FO-11).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.
--
-- Migration 0011 keyed the rule (KX004) on the subject of the task, the first instance of the server, while a
-- stop or a restart acts on every primary the server runs (a server can run two after a failover), and the
-- task of a configuration restart has no instance as its subject at all. Now: an instance task is refused when
-- the server of its node is the old or the new server of a group whose failover is unfinished; a config.restart
-- is refused while any failover group is unfinished, cluster-wide (restarts are rare and a failover short).

CREATE OR REPLACE FUNCTION kingres_cp.task_exclusion() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    other text;
    host text;
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
    ELSIF NEW.kind = 'config.restart' OR (NEW.kind IN ('instance.stop', 'instance.restart') AND NEW.subject_type = 'instance') THEN
        PERFORM kingres_cp.lock_operations();
        host := NULL;
        IF NEW.kind <> 'config.restart' AND NEW.subject_id ~ '^[0-9]+$' AND to_regclass('kingres.node') IS NOT NULL THEN
            EXECUTE 'SELECT host FROM kingres.node WHERE node_id = $1' INTO host USING NEW.subject_id::integer;
        END IF;
        IF EXISTS (SELECT 1
                   FROM kingres_cp.failover_run r
                   JOIN kingres_cp.task t ON t.id = r.task_id AND t.state IN ('pending', 'running', 'paused')
                   JOIN kingres_cp.failover_group g ON g.task_id = r.task_id
                       AND g.state NOT IN ('done', 'failed', 'skipped')
                   WHERE NEW.kind = 'config.restart'
                      OR NEW.subject_id IN (g.old_node_id::text, g.new_node_id::text)
                      OR (host IS NOT NULL AND host IN (g.old_server, g.new_server))) THEN
            RAISE EXCEPTION 'the % of % belongs to a group that is being failed over', NEW.kind, NEW.subject_id
                USING ERRCODE = 'KX004', DETAIL = 'server.failover';
        END IF;
    END IF;
    RETURN NEW;
END $$;
