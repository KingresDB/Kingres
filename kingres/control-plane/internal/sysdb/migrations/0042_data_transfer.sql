-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 42: the staged files of the data transfer (phase 14a: table CSV export and import).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- One row per staged file, an export or an upload. The row is the truth about who may have the bytes and for how long; the file
-- (transfer-<transfer_id>.kdx under the transfer directory of server_id) is the truth about the bytes. Nothing a client sends is ever
-- part of a path: the name of the file comes from transfer_id only.
CREATE TABLE kingres_cp.data_transfer (
    transfer_id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind           text        NOT NULL CHECK (kind IN ('tableExport', 'databaseExport', 'upload')),
    state          text        NOT NULL CHECK (state IN ('receiving', 'building', 'ready', 'failed', 'deleted', 'expired', 'lost')),
    -- The server whose disk holds the file: the manager leader that created it.
    server_id      text        NOT NULL,
    -- The role that made it, by name and by OID: a role that was dropped and made again under the same name has another OID and
    -- does not inherit the files. The only role that may read, download or use the file (a delete is also open to any administrator).
    created_by     text        NOT NULL,
    created_by_oid bigint      NOT NULL,
    database_name  text,
    schema_name    text,
    table_name     text,
    size_bytes     bigint      CHECK (size_bytes IS NULL OR size_bytes >= 0),
    declared_bytes bigint      CHECK (declared_bytes IS NULL OR declared_bytes >= 0),
    -- Bytes of the staging quota that this file holds now; grows while an export is written, is the declared size for an upload.
    reserved_bytes bigint      NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    sha256         text        CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$'),
    format         text        CHECK (format IS NULL OR format IN ('csv', 'csv.gz', 'archive/1')),
    -- The result of an export (rows, consistency, options used) or, later, the report of a validation. Never values of the data.
    report         jsonb,
    -- The key that encrypts the file: the id of the master key and the data key wrapped under it, never the key itself.
    key_id         text,
    wrapped_key    bytea,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- The last time bytes arrived (an upload) or were written (an export); the janitor deletes an upload that received nothing for 48 hours.
    updated_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    -- Set when the last byte of a download was served.
    downloaded_at  timestamptz,
    -- The task that builds or uses the file.
    task_id        bigint
);
CREATE INDEX data_transfer_owner ON kingres_cp.data_transfer (created_by_oid, transfer_id DESC);
CREATE INDEX data_transfer_server ON kingres_cp.data_transfer (server_id, state);
CREATE INDEX data_transfer_expiry ON kingres_cp.data_transfer (state, expires_at);
