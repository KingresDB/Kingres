-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 3: identifiers of ledger verification records and their tasks
-- (README, "Inclusion proofs and verification records").
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- One row per record of kingres.ledger_verification of a user database: the
-- control plane's own identifier (id, allocated in the order of the
-- extension's ids) and the task that ran the verification (task_id, null for a
-- run the extension recorded without a task). chain_id keeps the pairing from
-- applying to a database that was dropped and created again under the same
-- name (its extension ids start again at 1).
CREATE TABLE kingres_cp.ledger_verification (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_name  text    NOT NULL,
    chain_id       text    NOT NULL,
    shard_group_id integer NOT NULL DEFAULT 0,
    extension_id   bigint  NOT NULL,
    task_id        bigint,
    recorded_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (database_name, chain_id, shard_group_id, extension_id)
);
CREATE INDEX ledger_verification_database_idx ON kingres_cp.ledger_verification (database_name, id DESC);
