-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 12: the mark of a move is kept with its row (review ST-05).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- ext_mark is the highest move id the extension had when the loop took its mark, before the call. A holder
-- that takes over a row whose extension id was not written yet finds the extension's record of the call as
-- the record of the bucket above this mark, with no comparison of two clocks. NULL for a row made before
-- this migration, or before the mark was taken.
ALTER TABLE kingres_cp.rebalance_move ADD COLUMN ext_mark bigint;
