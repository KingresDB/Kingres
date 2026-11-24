-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026 RooyeKhat Media Co
-- kingres_cp 44: the distributed schema that a move of a rebalance carries (phase 11, design shard-phase11.md section 9.9).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- The name of the distributed schema when the move is a schema move (the one bucket of the schema's colocation group); null for a bucket.
ALTER TABLE kingres_cp.rebalance_move ADD COLUMN schema_name text;
