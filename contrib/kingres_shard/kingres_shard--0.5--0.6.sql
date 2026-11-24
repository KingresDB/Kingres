-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_shard--0.5--0.6.sql
--   Upgrade of the kingres_shard extension from version 0.5 to 0.6.  Version 0.6
--   is phase 2b (design/shard-phase2b.md, section 14): the nodes of a cluster
--   must run the same fragment protocol, so the version is raised.  Phase 2b
--   adds no table, view, function or column, and no object of the catalog
--   changes.  The one statement below only makes the node version check of a
--   bucket move ask for 0.6 instead of 0.5 (its text names the version);
--   the function is not new.
--   The result is identical to installing kingres_shard--0.6.sql (the "upgrade"
--   regression test compares the two).

\echo Use "ALTER EXTENSION kingres_shard UPDATE TO '0.6'" to load this file. \quit

-- The schema of the extension must not be one that a role that is not a superuser holds or can create objects in: that role would own what the service role, a
-- superuser, reads and runs.  The check is the same in every script of the extension, the install scripts of the older versions and the upgrade scripts too (a
-- schema that was made, or given away, after the install is met by ALTER EXTENSION ... UPDATE).  A schema that does not exist yet is made by the script.
DO $kgchk$
DECLARE
    v_schema pg_catalog.text := 'kingres';
    v_owner  pg_catalog.name;
BEGIN
    SELECT r.rolname INTO v_owner
      FROM pg_catalog.pg_namespace n JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) n.nspowner
     WHERE n.nspname OPERATOR(pg_catalog.=) v_schema AND NOT r.rolsuper;
    IF v_owner IS NOT NULL THEN
        RAISE EXCEPTION 'the schema % already exists and is owned by %, which is not a superuser', v_schema, v_owner
            USING ERRCODE = 'insufficient_privilege',
                  HINT = 'Drop the schema, or make a superuser its owner, before CREATE EXTENSION or ALTER EXTENSION ... UPDATE.';
    END IF;
    IF EXISTS (SELECT 1
                 FROM pg_catalog.pg_namespace n,
                      LATERAL pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
                WHERE n.nspname OPERATOR(pg_catalog.=) v_schema
                  AND a.privilege_type OPERATOR(pg_catalog.=) 'CREATE'
                  AND (a.grantee OPERATOR(pg_catalog.=) 0
                       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles g WHERE g.oid OPERATOR(pg_catalog.=) a.grantee AND g.rolsuper))) THEN
        RAISE EXCEPTION 'the schema % grants CREATE to a role that is not a superuser (or to PUBLIC)', v_schema
            USING ERRCODE = 'insufficient_privilege',
                  HINT = 'Revoke CREATE on the schema before CREATE EXTENSION or ALTER EXTENSION ... UPDATE.';
    END IF;
END
$kgchk$;

-- internal_check_node_versions(): asks for 0.6.
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
        IF v IS DISTINCT FROM '0.6' THEN
            RAISE EXCEPTION 'node % (%:%) has kingres_shard %, a bucket move needs 0.6 on every node',
                n.node_id, n.host, n.port, COALESCE(NULLIF(v, ''), '(not installed)')
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Run ALTER EXTENSION kingres_shard UPDATE on that node, after the new library is loaded there.';
        END IF;
    END LOOP;
END;
$$;
