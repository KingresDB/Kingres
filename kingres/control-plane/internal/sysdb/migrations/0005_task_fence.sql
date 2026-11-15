-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 5: the lease epoch of a task (fencing token).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- Every claim of a task (first claim or takeover of an expired lease) increments the epoch. Every write
-- of the holder names the epoch it was given, so a run that lost its lease cannot write even when the
-- same holder id claims the task again (a second run in the same process, a restart).
ALTER TABLE kingres_cp.task ADD COLUMN lease_epoch bigint NOT NULL DEFAULT 0;

-- Who asked for a digest. The extension records the role that ran the function, which is always the
-- service role (kingresd runs it on behalf of the caller), so the control plane remembers the caller:
-- generatedBy of the contract is "the role that requested it, or system for scheduled digests".
CREATE TABLE kingres_cp.ledger_digest_actor (
    database_name text   NOT NULL,
    chain_id      text   NOT NULL,
    digest_id     bigint NOT NULL,
    actor         text   NOT NULL,
    PRIMARY KEY (database_name, chain_id, digest_id)
);
