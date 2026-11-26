-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 7: configuration from the console (design: config-phase13.md, "Data model").
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- The desired value of a parameter at one level. A row is removed when the level's value is removed.
CREATE TABLE kingres_cp.config_value (
    level       text   NOT NULL CHECK (level IN ('cluster', 'serverGroup', 'server', 'database')),
    -- '' for the cluster, else the server group, the server id or the database name
    scope_id    text   NOT NULL DEFAULT '',
    -- the parameter in lower case
    name        text   NOT NULL,
    -- the validated, normalized text; a refused parameter can never be stored
    value       text   NOT NULL,
    revision    bigint NOT NULL,
    updated_by  text   NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (level, scope_id, name),
    CHECK ((level = 'cluster') = (scope_id = '')),
    CHECK (length(name) BETWEEN 1 AND 63 AND length(value) <= 4096)
);

-- One number per accepted change; never reused. The first change is revision 1.
CREATE SEQUENCE kingres_cp.config_revision;

-- One row per changed (level, scope, name) of an accepted change: the history the page lists and
-- the revert reads.
CREATE TABLE kingres_cp.config_change (
    change_id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    revision    bigint NOT NULL,
    task_id     bigint,
    level       text   NOT NULL,
    scope_id    text   NOT NULL DEFAULT '',
    name        text   NOT NULL,
    -- null: there was no value at this level / the value at this level was removed
    old_value   text,
    new_value   text,
    changed_by  text   NOT NULL,
    changed_at  timestamptz NOT NULL DEFAULT now(),
    reverts     bigint REFERENCES kingres_cp.config_change (change_id),
    -- a resumed step cannot record a change twice
    UNIQUE (task_id, level, scope_id, name)
);
CREATE INDEX config_change_name ON kingres_cp.config_change (name, change_id DESC);

-- What each server reported at its last apply: the states that cannot be derived from the server
-- itself (failed, with PostgreSQL's text).
CREATE TABLE kingres_cp.config_server_state (
    server_id        text PRIMARY KEY,
    applied_revision bigint NOT NULL DEFAULT 0,
    state            text   NOT NULL CHECK (state IN ('applied', 'pendingRestart', 'failed', 'waiting')),
    detail           jsonb  NOT NULL DEFAULT '{}'::jsonb,
    updated_at       timestamptz NOT NULL DEFAULT now()
);
