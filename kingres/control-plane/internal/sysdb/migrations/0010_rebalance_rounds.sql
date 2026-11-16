-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 10: rounds and the outcome "cancelled" of a rebalance run (review RB-07, RB-08).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- A run that was stopped and is started again by a person begins a new round: the attempts and the skips
-- of the earlier rounds do not count for the new one (a person who starts again has decided that the cause
-- is gone; the checks before each move find out when it is not), and their rows stay as the history of the
-- report. round 0 is the first.
ALTER TABLE kingres_cp.rebalance_run  ADD COLUMN round integer NOT NULL DEFAULT 0;
ALTER TABLE kingres_cp.rebalance_move ADD COLUMN round integer NOT NULL DEFAULT 0;

-- A stopped run that a person ends for good (cancel of the paused task), and a run cancelled while it ran,
-- is neither completed nor failed.
ALTER TABLE kingres_cp.rebalance_run DROP CONSTRAINT rebalance_run_outcome_check;
ALTER TABLE kingres_cp.rebalance_run ADD CONSTRAINT rebalance_run_outcome_check
    CHECK (outcome IN ('completed', 'stopped', 'failed', 'cancelled'));
