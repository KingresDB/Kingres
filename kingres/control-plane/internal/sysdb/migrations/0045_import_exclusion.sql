-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026 RooyeKhat Media Co
-- kingres_cp 45: a database import excludes a rebalance (and so a drain), the addition of a server and the removal of a server, and they exclude it.
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.
--
-- The private database of an import is left out of every walk that changes a catalog (the failover, the weight and the drain flag of a server, the rebalance), so
-- its catalog follows nothing while the import loads it. A drain that ran meanwhile would end with the server empty in every database but that one, and the final
-- database would have buckets on a server that the cluster means to empty; the same holds for the removal of a server and for a rebalance that moves buckets. A
-- server that is added meanwhile is registered in the private database too (every sharded database gets it), but the private database is made on the servers that
-- the import started with, so the new server would never take the final name while the catalog names it.
--
--   a task of kind database.import is refused (KX007) while a rebalance is pending or running (a drain is a rebalance), or while server.add, server.remove or
--   server.leave is unfinished; a rebalance is refused (KX001) and server.add, server.remove and server.leave are refused (KX006) while a database.import is
--   pending, running or paused.
--
-- It is a function of its own, so that it does not depend on the text of task_exclusion() of migrations 0016 and 0041. A change of the weight or of the flag of a server
-- (PATCH /servers/{id}) is not a task: UpdateServer asks for an unfinished import under the same lock (kingres_cp.lock_operations).

CREATE FUNCTION kingres_cp.task_exclusion_import() RETURNS trigger
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
    IF NEW.kind = 'database.import' THEN
        PERFORM kingres_cp.lock_operations();
        SELECT kind INTO other FROM kingres_cp.task
            WHERE ((kind = 'rebalance' AND state IN ('pending', 'running'))
                OR (kind IN ('server.add', 'server.remove', 'server.leave') AND state IN ('pending', 'running', 'paused')))
              AND id <> NEW.id LIMIT 1;
        IF FOUND THEN
            RAISE EXCEPTION 'a task of kind % cannot start while a task of kind % is unfinished', NEW.kind, other
                USING ERRCODE = 'KX007', DETAIL = other;
        END IF;
    ELSIF NEW.kind = 'rebalance' AND NEW.state IN ('pending', 'running') THEN
        PERFORM kingres_cp.lock_operations();
        IF EXISTS (SELECT 1 FROM kingres_cp.task WHERE kind = 'database.import' AND state IN ('pending', 'running', 'paused') AND id <> NEW.id) THEN
            RAISE EXCEPTION 'a rebalance cannot start while a database import is unfinished'
                USING ERRCODE = 'KX001', DETAIL = 'database.import';
        END IF;
    ELSIF NEW.kind IN ('server.add', 'server.remove', 'server.leave') THEN
        PERFORM kingres_cp.lock_operations();
        IF EXISTS (SELECT 1 FROM kingres_cp.task WHERE kind = 'database.import' AND state IN ('pending', 'running', 'paused') AND id <> NEW.id) THEN
            RAISE EXCEPTION 'a task of kind % cannot start while a database import is unfinished', NEW.kind
                USING ERRCODE = 'KX006', DETAIL = 'database.import';
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER task_exclusion_import BEFORE INSERT OR UPDATE OF state ON kingres_cp.task
    FOR EACH ROW EXECUTE FUNCTION kingres_cp.task_exclusion_import();
