-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 18: how many standbys the primary's server knew of for the group at the last check (review F3-02, F3-12).
-- A primary whose server never learned that its group has a standby is not stopped by a lapsed lease (the lease fences only
-- a group that has a standby), so the failover of a server that does not answer must know what its plan carried.
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

ALTER TABLE kingres_cp.group_replication_state ADD COLUMN planned_standbys integer NOT NULL DEFAULT 0;
