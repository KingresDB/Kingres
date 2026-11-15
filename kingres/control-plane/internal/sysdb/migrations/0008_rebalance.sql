-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 8: rebalancing (phase 4 of the control plane; design shard-phase10.md, section 6.5).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- One row per run of the task kind 'rebalance'; its id is the task id, which is also the id of the
-- report of the contract. The row is written by the run that holds the task's lease (every write
-- names the epoch, see internal/rebalance), and outlives the task as the report.
CREATE TABLE kingres_cp.rebalance_run (
    task_id           bigint      PRIMARY KEY,
    trigger           text        NOT NULL DEFAULT 'manual',
    strategy          text        NOT NULL DEFAULT 'byBucketCount',
    options           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    started_at        timestamptz NOT NULL,
    -- null while the run is active; 'stopped' while it is paused by an operator
    outcome           text        CHECK (outcome IN ('completed', 'stopped', 'failed')),
    finished_at       timestamptz,
    -- the operator asked to stop: the run starts no new move and cancels the one in flight
    stop_requested    boolean     NOT NULL DEFAULT false,
    stopped_by        text,
    -- moves of the first plan; the run may find more when it plans again before a move
    planned_moves     integer     NOT NULL DEFAULT 0,
    -- primary buckets per database and server, as a JSON array
    placement_before  jsonb,
    placement_after   jsonb,
    error             text
);

-- One row per move of a run. A move that is tried again keeps its row and counts attempts.
CREATE TABLE kingres_cp.rebalance_move (
    task_id        bigint      NOT NULL REFERENCES kingres_cp.rebalance_run (task_id) ON DELETE CASCADE,
    seq            integer     NOT NULL,
    database_name  text        NOT NULL,
    colocation_id  integer     NOT NULL,
    bucket         integer     NOT NULL,
    from_group     integer     NOT NULL,
    to_group       integer     NOT NULL,
    from_server    text,
    to_server      text,
    -- running, done, failed, cancelled, skipped
    state          text        NOT NULL CHECK (state IN ('running', 'done', 'failed', 'cancelled', 'skipped')),
    attempts       integer     NOT NULL DEFAULT 1,
    -- the extension's own record (kingres.bucket_move.move_id) once the move has one
    move_id        bigint,
    -- preparing, copying, catching_up, switching, cleaning, done, failed, cancelled
    phase          text,
    percent        real        NOT NULL DEFAULT 0,
    size_bytes     bigint,
    started_at     timestamptz NOT NULL,
    finished_at    timestamptz,
    error          text,
    PRIMARY KEY (task_id, seq)
);
CREATE INDEX rebalance_move_key_idx ON kingres_cp.rebalance_move (task_id, database_name, colocation_id, bucket);
