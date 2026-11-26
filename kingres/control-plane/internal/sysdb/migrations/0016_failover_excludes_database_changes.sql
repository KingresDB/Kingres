-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 16: a failover is refused while a database is created or dropped or a table is distributed, and those operations
-- are refused while a failover is unfinished (review F3-04): the catalogs of the databases are what the failover changes
-- node by node, and a database that appears or goes between the check of the task and the end of it leaves them half moved.
-- The rest of the function is the one of migration 0014. Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

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
            -- the other direction of KX004 (F2-09): an instance stop or restart, or a config restart, that is already
            -- running makes the instances of a server unavailable under the failover's hands
            SELECT kind INTO other FROM kingres_cp.task
                WHERE kind IN ('instance.stop', 'instance.restart', 'config.restart')
                  AND state IN ('pending', 'running') AND id <> NEW.id LIMIT 1;
            IF FOUND THEN
                RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is running', NEW.kind, other
                    USING ERRCODE = 'KX005', DETAIL = other;
            END IF;
            SELECT kind INTO other FROM kingres_cp.task
                WHERE kind IN ('server.failover', 'replication.restorePreferredPrimaries')
                  AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
            IF FOUND THEN
                RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is unfinished', NEW.kind, other
                    USING ERRCODE = 'KX003', DETAIL = other;
            END IF;
            -- F3-04: a database that is created, dropped or distributed meanwhile would be found by the steps after
            -- the point of no return with nodes the check did not see
            SELECT kind INTO other FROM kingres_cp.task
                WHERE kind IN ('database.create', 'database.drop', 'table.distribute')
                  AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
            IF FOUND THEN
                RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is unfinished', NEW.kind, other
                    USING ERRCODE = 'KX006', DETAIL = other;
            END IF;
        END IF;
    ELSIF NEW.kind IN ('database.create', 'database.drop', 'table.distribute') THEN
        PERFORM kingres_cp.lock_operations();
        SELECT kind INTO other FROM kingres_cp.task
            WHERE kind IN ('server.failover', 'replication.restorePreferredPrimaries')
              AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is unfinished', NEW.kind, other
                USING ERRCODE = 'KX007', DETAIL = other;
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
