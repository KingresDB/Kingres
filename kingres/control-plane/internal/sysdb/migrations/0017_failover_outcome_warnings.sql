-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 17: a failover that ended with something the operator should read ends "completedWithWarnings" (review F3-15):
-- a standby that was skipped or does not stream, a list of synchronous standbys that is still empty, prepared transactions
-- that remain, an incident of check_cluster().
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

ALTER TABLE kingres_cp.failover_run DROP CONSTRAINT IF EXISTS failover_run_outcome_check;
ALTER TABLE kingres_cp.failover_run ADD CONSTRAINT failover_run_outcome_check
    CHECK (outcome IN ('completed', 'completedWithWarnings', 'partial', 'failed', 'cancelled'));
