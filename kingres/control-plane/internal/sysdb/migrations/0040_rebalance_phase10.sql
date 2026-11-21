-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026 RooyeKhat Media Co
-- kingres_cp 40: what phase 10 adds to a rebalance run (design shard-phase10.md, sections 4 and 6.5).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- Why a move was planned (balance or drain) and what the plan counted for the bucket (1, or its bytes).
ALTER TABLE kingres_cp.rebalance_move ADD COLUMN reason text;
ALTER TABLE kingres_cp.rebalance_move ADD COLUMN cost   bigint;
