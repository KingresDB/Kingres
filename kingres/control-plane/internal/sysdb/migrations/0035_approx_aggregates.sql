-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 35: the approximate-aggregates setting of a database (phase 12, task database.approximateAggregates).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- All three columns have defaults, so a row written before this migration reads as "off".
ALTER TABLE kingres_cp.database_setting
    ADD COLUMN approx_enabled        boolean NOT NULL DEFAULT false,
    ADD COLUMN approx_count_distinct text    NOT NULL DEFAULT 'exact' CHECK (approx_count_distinct IN ('exact', 'approximate')),
    ADD COLUMN approx_error_rate     real    CHECK (approx_error_rate BETWEEN 0.0005 AND 0.1);
