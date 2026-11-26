-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 36: which task and which role made a run of a time-partitioning policy (phase 12, task partition.maintain).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- The run itself is a row of kingres_timepart in the database of the table (kingres.timepart_run). The extension knows the role of the session that
-- drove it, which is always the service role; this row says which task drove it and which administrator, or the scheduler, asked.
CREATE TABLE kingres_cp.partition_run_task (
    database_name text   NOT NULL,
    run_id        bigint NOT NULL,
    task_id       bigint NOT NULL,
    started_by    text   NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (database_name, run_id)
);
CREATE INDEX partition_run_task_task ON kingres_cp.partition_run_task (task_id);
