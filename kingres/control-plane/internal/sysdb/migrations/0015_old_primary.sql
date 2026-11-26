-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 15: the old primary after a failover (design ha-phase5-part2.md, 6.2 and 6.5; increment 4).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- Since when the server of a standby that is not ready (the old primary waiting to be rebuilt) has not answered the leader.
-- The leader sets it at the first check that finds the server silent and clears it when the server answers; a standby
-- whose server stays silent for standby-replace-after is removed (in the order of extension sequence 8) and replaced.
ALTER TABLE kingres_cp.standby ADD COLUMN waiting_since timestamptz;

-- What the rebuild of the old primary found and did: {"method": "clean|pgRewind|baseBackup", "divergenceLsn": "...",
-- "oldEndLsn": "...", "divergenceBytes": n, "rewindBytes": n, "message": "..."} (the report of the run reads it).
ALTER TABLE kingres_cp.failover_group ADD COLUMN old_primary_report jsonb;
