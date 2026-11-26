-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 1: the state of the control plane (design: control-plane-2.md, D2).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

CREATE TABLE kingres_cp.task (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind             text        NOT NULL,
    state            text        NOT NULL CHECK (state IN ('pending', 'running', 'paused', 'failed', 'done')),
    subject_type     text        NOT NULL,
    subject_id       text        NOT NULL,
    input            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    progress         jsonb       NOT NULL DEFAULT '{"percent": 0, "steps": []}'::jsonb,
    error            jsonb,
    cancel_requested boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    created_by       text        NOT NULL,
    lease_holder     text,
    lease_expires_at timestamptz
);
-- One unfinished task per subject (the database enforces what the engine checks).
CREATE UNIQUE INDEX task_one_active_per_subject
    ON kingres_cp.task (subject_type, subject_id)
    WHERE state IN ('pending', 'running', 'paused');
CREATE INDEX task_state_idx ON kingres_cp.task (state, id);

CREATE TABLE kingres_cp.database_setting (
    database_name           text PRIMARY KEY,
    sharding_enabled        boolean     NOT NULL,
    bucket_count            integer     CHECK (bucket_count BETWEEN 1 AND 4096),
    durability              text        NOT NULL CHECK (durability IN ('async', 'sync', 'syncApply')),
    ledger_mode             text        NOT NULL CHECK (ledger_mode IN ('off', 'optIn')),
    ledger_digest_interval  integer     CHECK (ledger_digest_interval BETWEEN 60 AND 86400),
    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by              text        NOT NULL
);

CREATE TABLE kingres_cp.cluster_setting (
    key        text PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE kingres_cp.server_group (
    name        text PRIMARY KEY,
    description text NOT NULL DEFAULT ''
);

CREATE TABLE kingres_cp.server (
    server_id         text PRIMARY KEY,
    server_group      text REFERENCES kingres_cp.server_group (name) ON DELETE RESTRICT,
    joined_at         timestamptz NOT NULL DEFAULT now(),
    version           text,
    last_heartbeat_at timestamptz
);

CREATE TABLE kingres_cp.alert (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    severity        text        NOT NULL CHECK (severity IN ('critical', 'warning', 'info')),
    kind            text        NOT NULL,
    subject         text        NOT NULL,
    message         text        NOT NULL,
    raised_at       timestamptz NOT NULL DEFAULT now(),
    cleared_at      timestamptz,
    acknowledged_at timestamptz,
    acknowledged_by text
);
-- One open alert per kind and subject.
CREATE UNIQUE INDEX alert_one_open ON kingres_cp.alert (kind, subject) WHERE cleared_at IS NULL;

CREATE TABLE kingres_cp.network_rule (
    revision   bigint      NOT NULL,
    position   integer     NOT NULL,
    rule       jsonb       NOT NULL,
    PRIMARY KEY (revision, position)
);
CREATE TABLE kingres_cp.network_revision (
    only_row     boolean PRIMARY KEY DEFAULT true CHECK (only_row),
    revision     bigint      NOT NULL,
    tls_required boolean     NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by text        NOT NULL
);
