-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 50: the tasks of the distributed schemas (contract 0.15.0, phase 11) and a failover exclude each other, as the
-- creation of a database and a failover do (migration 0016, review F3-04): a schema that is distributed, moved or made local
-- while the catalogs are being failed over node by node would be found by the steps after the point of no return with nodes the
-- check did not see. It is a second trigger, with a function of its own, so that it does not depend on the text of the function
-- of migration 0016 (the range 0050 to 0059 is reserved for the schemas; another line of work replaces task_exclusion()).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.
--
--   a task of kind schema.distribute, schema.move or schema.undistribute is refused (SQLSTATE KX007) while a failover is
--   pending, running or paused;
--   a failover (or the restore of preferred primaries) is refused (KX006) while one of those tasks is unfinished.

CREATE FUNCTION kingres_cp.task_exclusion_schema() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    other text;
BEGIN
    IF NEW.state NOT IN ('pending', 'running', 'paused') THEN
        RETURN NEW;
    END IF;
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
    ELSIF NEW.kind IN ('server.failover', 'replication.restorePreferredPrimaries') THEN
        PERFORM kingres_cp.lock_operations();
        SELECT kind INTO other FROM kingres_cp.task
            WHERE kind IN ('schema.distribute', 'schema.move', 'schema.undistribute')
              AND state IN ('pending', 'running', 'paused') AND id <> NEW.id LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is unfinished', NEW.kind, other
                USING ERRCODE = 'KX006', DETAIL = other;
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER task_exclusion_schema BEFORE INSERT OR UPDATE OF state ON kingres_cp.task
    FOR EACH ROW EXECUTE FUNCTION kingres_cp.task_exclusion_schema();
