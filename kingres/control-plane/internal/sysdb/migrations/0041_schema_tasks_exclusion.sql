-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026 RooyeKhat Media Co
-- kingres_cp 41: the tasks of the distributed schemas exclude a failover and a rebalance (contract 0.16.0, phase 11).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.
--
-- A schema that is distributed, moved or made local while the catalogs are being failed over node by node would be found by the
-- steps after the point of no return with nodes the check did not see, as a database that is created meanwhile would
-- (migration 0016, F3-04). A schema move is a bucket move: a rebalance that moves buckets of the same groups at the same time
-- is refused the same way. It is a second trigger with a function of its own, so that it does not depend on the text of
-- task_exclusion() of migration 0016.
--
--   a task of kind schema.distribute, schema.move or schema.undistribute is refused (KX007) while a failover is pending, running
--   or paused; a failover (or the restore of preferred primaries) is refused (KX006) while one of those tasks is unfinished;
--   schema.move and schema.undistribute (which may move the schema first) are refused (KX002) while a rebalance is pending or
--   running, and a rebalance is refused (KX001) while one of those two is unfinished.
--   schema.move and schema.undistribute are refused (KX007) while server.add, server.remove or server.leave is unfinished, and those
--   are refused (KX006) while one of the two is unfinished: a move to a group whose server is being removed or joined must not run.

CREATE FUNCTION kingres_cp.task_exclusion_schema() RETURNS trigger
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
    IF NEW.kind IN ('schema.distribute', 'schema.move', 'schema.undistribute') THEN
        PERFORM kingres_cp.lock_operations();
        SELECT kind INTO other FROM kingres_cp.task
            WHERE kind IN ('server.failover', 'replication.restorePreferredPrimaries')
              AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is unfinished', NEW.kind, other
                USING ERRCODE = 'KX007', DETAIL = other;
        END IF;
        IF NEW.kind IN ('schema.move', 'schema.undistribute') THEN
            SELECT kind INTO other FROM kingres_cp.task
                WHERE kind IN ('server.add', 'server.remove', 'server.leave')
                  AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
            IF FOUND THEN
                RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is unfinished', NEW.kind, other
                    USING ERRCODE = 'KX007', DETAIL = other;
            END IF;
        END IF;
        IF NEW.kind IN ('schema.move', 'schema.undistribute')
           AND EXISTS (SELECT 1 FROM kingres_cp.task WHERE kind = 'rebalance' AND state IN ('pending', 'running') AND id <> NEW.id) THEN
            RAISE EXCEPTION 'a task of kind % cannot start while a rebalance is running', NEW.kind
                USING ERRCODE = 'KX002', DETAIL = 'rebalance';
        END IF;
    ELSIF NEW.kind IN ('server.failover', 'replication.restorePreferredPrimaries') THEN
        PERFORM kingres_cp.lock_operations();
        SELECT kind INTO other FROM kingres_cp.task
            WHERE kind IN ('schema.distribute', 'schema.move', 'schema.undistribute')
              AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is unfinished', NEW.kind, other
                USING ERRCODE = 'KX006', DETAIL = other;
        END IF;
    ELSIF NEW.kind IN ('server.add', 'server.remove', 'server.leave') THEN
        PERFORM kingres_cp.lock_operations();
        SELECT kind INTO other FROM kingres_cp.task
            WHERE kind IN ('schema.move', 'schema.undistribute')
              AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is unfinished', NEW.kind, other
                USING ERRCODE = 'KX006', DETAIL = other;
        END IF;
    ELSIF NEW.kind = 'rebalance' AND NEW.state IN ('pending', 'running') THEN
        PERFORM kingres_cp.lock_operations();
        SELECT kind INTO other FROM kingres_cp.task
            WHERE kind IN ('schema.move', 'schema.undistribute')
              AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'a rebalance cannot start while a task of kind % is unfinished', other
                USING ERRCODE = 'KX001', DETAIL = other;
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER task_exclusion_schema BEFORE INSERT OR UPDATE OF state ON kingres_cp.task
    FOR EACH ROW EXECUTE FUNCTION kingres_cp.task_exclusion_schema();
