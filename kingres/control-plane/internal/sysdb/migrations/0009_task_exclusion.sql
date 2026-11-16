-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 9: operations that exclude each other are excluded in the database (review RB-10).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.
--
-- The contract says a rebalance and a change of the servers (add, remove, leave, failover) exclude each
-- other. Each side used to read the other's tasks and then insert its own, with nothing between the two,
-- so both could be created. Now the insert of such a task (and the resume of a paused rebalance) runs a
-- check under one advisory lock that every check-then-act of the control plane that depends on the same
-- fact also takes (kingres_cp.lock_operations()), so the check and the write are one atomic step.
--
--   a rebalance that is pending or running is refused (SQLSTATE KX001) while a task of kind server.add,
--   server.remove, server.leave or server.failover is pending, running or paused;
--   a task of one of those kinds is refused (KX002) while a rebalance is pending or running.
--
-- A paused (stopped) rebalance does not exclude the server operations (contract 0.9.0).

CREATE FUNCTION kingres_cp.lock_operations() RETURNS void
LANGUAGE sql AS $$ SELECT pg_advisory_xact_lock(hashtext('kingres_cp.operations')::bigint) $$;

CREATE FUNCTION kingres_cp.task_exclusion() RETURNS trigger
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
            WHERE kind IN ('server.add', 'server.remove', 'server.leave', 'server.failover')
              AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'a rebalance cannot start while a task of kind % is unfinished', other
                USING ERRCODE = 'KX001', DETAIL = other;
        END IF;
    ELSIF NEW.kind IN ('server.add', 'server.remove', 'server.leave', 'server.failover') THEN
        PERFORM kingres_cp.lock_operations();
        IF EXISTS (SELECT 1 FROM kingres_cp.task WHERE kind = 'rebalance' AND state IN ('pending', 'running') AND id <> NEW.id) THEN
            RAISE EXCEPTION 'a task of kind % cannot start while a rebalance is running', NEW.kind
                USING ERRCODE = 'KX002', DETAIL = 'rebalance';
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER task_exclusion BEFORE INSERT OR UPDATE OF state ON kingres_cp.task
    FOR EACH ROW EXECUTE FUNCTION kingres_cp.task_exclusion();
