-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 32: anchoring of the aggregate checkpoints of the ledger (design ledger-phase7.md, 6.2, 6.6, 6.8 and 8.2; step 10 of the plan).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.
-- The numbers 16 to 29 belong to the failover work, 30 and 31 to steps 3 to 6 of phase 7.

-- The anchor targets: where a signed aggregate checkpoint is stored or time-stamped. Nothing secret is here: the
-- credentials are in a file on the servers (<ledger-keys-dir>/anchor/<id>.json, 6.8) and only the fact that they
-- were set is a column.
CREATE TABLE kingres_cp.ledger_anchor_target (
    id              bigserial   PRIMARY KEY,
    kind            text        NOT NULL CHECK (kind IN ('worm', 'tsa', 'ots')),
    name            text        NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 63),
    enabled         boolean     NOT NULL DEFAULT false,
    -- when the target was last switched on: the clock of "behind" starts there, not at its creation
    enabled_since   timestamptz,
    -- the non-secret configuration, in the spelling of the library of the kind
    config          jsonb       NOT NULL,
    credentials_set boolean     NOT NULL DEFAULT false,
    -- the result of the last test (the checks), null when never tested
    last_test       jsonb,
    -- raised at every change of the configuration or of the credentials, so that a job that built a client
    -- from an older configuration is told so when it writes its result
    revision        bigint      NOT NULL DEFAULT 1,
    created_by      text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- The anchor records: one signed aggregate checkpoint at one target. A record keeps the signed note it was made
-- for (a re-signing of the checkpoint by a rotation of the key must not change the bytes that a retry sends to a
-- store that holds the first ones), the start of its first submission and the generation of its key (corrections
-- 89, 95 and 105), and its evidence as text: the strict parsers of the library accept the canonical bytes only,
-- and jsonb reorders the members of an object (duty 6 of internal/anchor).
CREATE TABLE kingres_cp.ledger_anchor (
    id              bigserial   PRIMARY KEY,
    -- null after the target was deleted; the name and the kind stay
    target_id       bigint      REFERENCES kingres_cp.ledger_anchor_target (id) ON DELETE SET NULL,
    target_name     text        NOT NULL,
    kind            text        NOT NULL CHECK (kind IN ('worm', 'tsa', 'ots')),
    -- the aggregate log, the database it belongs to, and the identifier that is the last element of its origin
    log_id          bigint      NOT NULL,
    database_name   text        NOT NULL,
    aggregate_id    text        NOT NULL CHECK (aggregate_id ~ '^[0-9a-f]{1,128}$'),
    origin          text        NOT NULL,
    size            bigint      NOT NULL CHECK (size >= 1),
    payload_sha256  bytea       NOT NULL CHECK (length(payload_sha256) = 32),
    note            bytea       NOT NULL,
    state           text        NOT NULL DEFAULT 'pending'
                    CHECK (state IN ('pending', 'submitted', 'claimed', 'anchored', 'failed', 'superseded', 'expired')),
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    refusals        integer     NOT NULL DEFAULT 0 CHECK (refusals >= 0),
    retryables      integer     NOT NULL DEFAULT 0 CHECK (retryables >= 0),
    failing_since   timestamptz,
    next_attempt_at timestamptz,
    evidence        text,
    evidence_time   timestamptz,
    anchored_at     timestamptz,
    last_error      text        NOT NULL DEFAULT '',
    -- the machine tag of the last error (the code of the library), shown in the text of an alert
    error_code      text        NOT NULL DEFAULT '',
    -- the alert that the record's failure raised: '' none, else the kind (6.9); a failed or expired record keeps it
    -- until a Retry, so the alerts are derived from the rows and survive a restart and a leader change
    alert           text        NOT NULL DEFAULT '' CHECK (alert IN ('', 'ledgerAnchorFailed', 'ledgerAnchorConflict')),
    created_at      timestamptz NOT NULL,
    -- the start of the first Submit call for the key, written before that call and never changed except with a
    -- new generation (duty 1 of internal/anchor)
    submit_start    timestamptz,
    generation      integer     NOT NULL DEFAULT 0 CHECK (generation BETWEEN 0 AND 1000),
    -- raised by every write of the row: a job writes its result only if the row is as it read it
    version         bigint      NOT NULL DEFAULT 1,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- the first stored evidence of a (target, log, size) wins
    UNIQUE (target_id, log_id, size)
);
CREATE INDEX ledger_anchor_work ON kingres_cp.ledger_anchor (state, next_attempt_at) WHERE state IN ('pending', 'submitted', 'claimed');
CREATE INDEX ledger_anchor_database ON kingres_cp.ledger_anchor (database_name, id DESC);
CREATE INDEX ledger_anchor_target_idx ON kingres_cp.ledger_anchor (target_id, log_id, size DESC);
