-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 31: the publication of the ledger, tiles and the digests of every group (design ledger-phase7.md, 4.5, 4.7, 5.4 and 8.2;
-- steps 5 and 6 of the plan). Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- A tile or an entry bundle of a log (level -1 is an entry bundle, as in ledgerlog.EntryLevel). A published tile is never replaced: the
-- primary key makes a second write fail, and the trigger refuses an update (a superuser can still change a row; signatures, anchors and
-- mirrors tell, 4.5).
CREATE TABLE kingres_cp.ledger_tile (
    log_id     bigint      NOT NULL REFERENCES kingres_cp.ledger_log (log_id),
    level      integer     NOT NULL CHECK (level BETWEEN -1 AND 63),
    idx        bigint      NOT NULL CHECK (idx >= 0),
    width      integer     NOT NULL CHECK (width BETWEEN 1 AND 256),
    data       bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (log_id, level, idx, width)
);

CREATE FUNCTION kingres_cp.ledger_tile_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'a published tile is never changed' USING ERRCODE = 'KC001';
END
$$;
CREATE TRIGGER ledger_tile_no_update BEFORE UPDATE ON kingres_cp.ledger_tile
    FOR EACH ROW EXECUTE FUNCTION kingres_cp.ledger_tile_immutable();

-- What a log row needs beyond migration 30: why it stopped, which aggregate leaf closed it, the leaf of the aggregate that is being
-- appended (an aggregate leaf holds a time, so a crash between its tiles and its frontier must append the same bytes again, the duty
-- "the leaves must be reproducible" of ledgerlog), and when the group was first unreadable.
ALTER TABLE kingres_cp.ledger_log
    ADD COLUMN detail          text        NOT NULL DEFAULT '',
    ADD COLUMN closed_at       timestamptz,
    ADD COLUMN retired_in_seq  bigint,
    ADD COLUMN pending_entry   bytea,
    ADD COLUMN unread_since    timestamptz,
    ADD COLUMN signed_at       timestamptz;
-- one chain log is open for a chain at a time; epochs follow each other
CREATE UNIQUE INDEX ledger_log_open_chain ON kingres_cp.ledger_log (chain_id) WHERE kind = 'chain' AND state <> 'closed';
CREATE UNIQUE INDEX ledger_log_aggregate ON kingres_cp.ledger_log (database_name) WHERE kind = 'aggregate' AND state <> 'closed';
CREATE INDEX ledger_log_database ON kingres_cp.ledger_log (database_name, kind);

-- A digest of every group of a database at one moment (5.4 step 5): what generateLedgerDigest returns and listLedgerDigests lists.
-- The per-chain documents are the digests of the extension, byte for byte.
CREATE TABLE kingres_cp.ledger_digest_view (
    digest_id       bigserial   PRIMARY KEY,
    database_name   text        NOT NULL,
    generated_at    timestamptz NOT NULL,
    generated_by    text        NOT NULL,
    hash_version    integer     NOT NULL DEFAULT 1,
    chains          jsonb       NOT NULL,
    complete        boolean     NOT NULL,
    uncovered       integer[]   NOT NULL DEFAULT '{}',
    aggregate_size  bigint,
    aggregate_hash  bytea
);
CREATE INDEX ledger_digest_view_database ON kingres_cp.ledger_digest_view (database_name, digest_id DESC);
