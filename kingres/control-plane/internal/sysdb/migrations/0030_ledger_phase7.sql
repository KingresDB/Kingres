-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 30: the ledger of phase 7, signing keys, events and the logs they sign
-- (design ledger-phase7.md, 3.2, 3.3, 6.7 and 8.2; steps 3 and 4 of the plan).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.
-- The number 30 is a range of its own (the migrations 15 to 29 belong to the failover work); the runner
-- applies every migration it has not recorded, so the gap is allowed.

-- The singleton row of the ledger of the cluster: its identity, the term of the manager leader (the
-- fencing token of every write of the scheduler and of the key tasks, 6.7), the settings of publication
-- and anchoring, and what the key tasks keep between two steps.
CREATE TABLE kingres_cp.ledger_cluster (
    -- always true: the table has one row
    id                           boolean     PRIMARY KEY DEFAULT true CHECK (id),
    -- 32 random bytes, public; null until the first signing key is created (3.2.1)
    cluster_id                   bytea       CHECK (cluster_id IS NULL OR length(cluster_id) = 32),
    -- the leader's term: every leader increments it when it takes over; a write names the term it holds
    term                         bigint      NOT NULL DEFAULT 0 CHECK (term >= 0),
    publication_enabled          boolean     NOT NULL DEFAULT false,
    -- null: the default of the daemon (its server address and "kingres-ledger"), written when the first key is made
    origin_prefix                text,
    public_url                   text,
    publication_interval_seconds integer     NOT NULL DEFAULT 60 CHECK (publication_interval_seconds BETWEEN 10 AND 3600),
    anchor_interval_seconds      integer     NOT NULL DEFAULT 3600 CHECK (anchor_interval_seconds BETWEEN 60 AND 86400),
    anchor_minimum               integer     NOT NULL DEFAULT 1 CHECK (anchor_minimum BETWEEN 1 AND 8),
    anchor_required              boolean     NOT NULL DEFAULT false,
    -- the time of the first signed checkpoint; once set the origin prefix cannot change (4.2)
    first_checkpoint_at          timestamptz,
    -- the number of the last change of the key set: a change names the number it read (compare and swap),
    -- so two tasks can never start from one stored set (duty of the caller of ledgerkeys)
    keys_revision                bigint      NOT NULL DEFAULT 0,
    -- the trust file the daemon published at the last change of the key set (public); compared with the file
    -- built from the key set after every load (duty K3-10 of ledgerkeys)
    trust_file                   text,
    -- the servers that were given the private keys at the last distribution (server ids)
    holders                      text[]      NOT NULL DEFAULT '{}',
    -- the operator was told to make a backup of the key directory and has not said that it was made
    backup_advised               boolean     NOT NULL DEFAULT false,
    -- the key ring was replaced after the loss of every key (9.2)
    discontinuity                boolean     NOT NULL DEFAULT false
);
INSERT INTO kingres_cp.ledger_cluster (id) VALUES (true);

-- The signing keys of the cluster (public information only: a private key is never stored here, 3.2.2).
CREATE TABLE kingres_cp.ledger_signing_key (
    -- SHA-256 of the public key, lower-case hexadecimal
    fingerprint       text        PRIMARY KEY CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    public_key        bytea       NOT NULL UNIQUE CHECK (length(public_key) = 32),
    state             text        NOT NULL CHECK (state IN ('next', 'active', 'retired', 'revoked')),
    -- 'file' (the private key is a file on the holders) or 'command' (an external signer)
    mode              text        NOT NULL DEFAULT 'file' CHECK (mode IN ('file', 'command')),
    label             text        NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL,
    activated_at      timestamptz,
    -- on the older of two active keys: when its overlap ends
    overlap_ends      timestamptz,
    retired_at        timestamptz,
    revoked_at        timestamptz,
    compromised_since timestamptz,
    -- for a retired key: the largest tree it may still sign; null is unbounded
    max_tree_size     bigint      CHECK (max_tree_size IS NULL OR max_tree_size >= 1),
    reason            text        NOT NULL DEFAULT '',
    -- the task that made the key, so that a repeated step finds the key it made
    task_id           bigint
);

-- The signed events of the key history, numbered from 0 without gaps (3.2.4).
CREATE TABLE kingres_cp.ledger_event (
    seq        bigint      PRIMARY KEY CHECK (seq >= 0),
    type       text        NOT NULL,
    -- the fingerprint the event is about, null for events about no key
    subject    text,
    -- the signed note
    note       bytea       NOT NULL,
    created_at timestamptz NOT NULL
);
-- one event of a type per key: a repeated step finds its event instead of making a second one
CREATE UNIQUE INDEX ledger_event_subject ON kingres_cp.ledger_event (type, subject) WHERE subject IS NOT NULL;

-- The logs and their checkpoints (4.1, 4.5, 8.2). The collector of the next step writes them; they exist now so
-- that the retirement of a key can bound it by the largest tree it ever signed.
CREATE TABLE kingres_cp.ledger_log (
    log_id          bigserial   PRIMARY KEY,
    kind            text        NOT NULL CHECK (kind IN ('chain', 'aggregate')),
    database_name   text,
    shard_group_id  integer,
    chain_id        bytea,
    epoch           integer     NOT NULL DEFAULT 1,
    origin          text        NOT NULL UNIQUE,
    state           text        NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'diverged', 'closed')),
    size            bigint      NOT NULL DEFAULT 0,
    root            bytea,
    frontier        bytea,
    head_block_hash bytea,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE kingres_cp.ledger_checkpoint (
    log_id    bigint      NOT NULL REFERENCES kingres_cp.ledger_log (log_id) ON DELETE CASCADE,
    size      bigint      NOT NULL,
    root      bytea       NOT NULL,
    note      bytea       NOT NULL,
    signed_at timestamptz NOT NULL,
    anchored  boolean     NOT NULL DEFAULT false,
    PRIMARY KEY (log_id, size)
);
