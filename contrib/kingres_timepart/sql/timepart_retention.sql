-- timepart_retention: dropping and detaching old partitions, a partition that
-- spans the boundary, drop_old_time_partitions by hand, a partition that
-- cannot be dropped, and DETACH ... CONCURRENTLY from a procedure.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: PostgreSQL

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

-- Drop: partitions that end at or before as_of - retention go, with their
-- rows; a partition that ends after the boundary stays whole.
CREATE TABLE tp.t1 (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.t1', '1 day', '2026-03-10 00:00 UTC', '2026-03-01 00:00 UTC');
INSERT INTO tp.t1 SELECT g, timestamptz '2026-03-01 UTC' + (g - 1) * interval '6 hours' FROM generate_series(1, 40) g;
SELECT count(*) AS rows_before FROM tp.t1;
SELECT kingres.timepart_set_policy('tp.t1', '1 day', create_ahead => '1 day', retention => '3 days');
-- at 2026-03-06 12:00 the boundary is 2026-03-03 12:00: the days up to 03-02
-- end before it; 03-03 ends at 03-04 00:00, after it, and stays
SELECT kind, partition_name, range_to FROM kingres.timepart_plan('tp.t1', '2026-03-06 12:00 UTC') WHERE kind <> 'create';
\o /dev/null
CALL kingres.timepart_run('tp.t1', '2026-03-06 12:00 UTC');
\o
SELECT * FROM tp.last_run('tp', 't1');
SELECT kind, partition_name, state FROM tp.last_actions('tp', 't1') WHERE kind <> 'create';
SELECT a.bytes_before > 0 AS size_recorded, a.bytes_after IS NULL AS no_after
  FROM kingres.timepart_run_action a JOIN kingres.timepart_run r USING (run_id)
 WHERE r.table_name = 't1' AND a.partition_name = 't1_p20260301';
SELECT min(ts) AS oldest_row, count(*) AS rows_after FROM tp.t1;
SELECT to_regclass('tp.t1_p20260301') IS NULL AS dropped, to_regclass('tp.t1_p20260303') IS NOT NULL AS kept;
-- The same run again: nothing to retire.
\o /dev/null
CALL kingres.timepart_run('tp.t1', '2026-03-06 12:00 UTC');
\o
SELECT count(*) FILTER (WHERE kind IN ('drop', 'detach')) AS retired_again FROM tp.last_actions('tp', 't1');
-- Later the boundary passes the end of 03-03.
\o /dev/null
CALL kingres.timepart_run('tp.t1', '2026-03-07 00:00 UTC');
\o
SELECT kind, partition_name FROM tp.last_actions('tp', 't1') WHERE kind = 'drop';

-- Detach: the partition stays as a table with its rows, under its name.
CREATE TABLE tp.t2 (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.t2', '1 day', '2026-03-05 00:00 UTC', '2026-03-01 00:00 UTC');
INSERT INTO tp.t2 SELECT g, timestamptz '2026-03-01 UTC' + (g - 1) * interval '12 hours' FROM generate_series(1, 10) g;
SELECT kingres.timepart_set_policy('tp.t2', '1 day', create_ahead => '1 day', retention => '2 days', retention_action => 'detach');
\o /dev/null
CALL kingres.timepart_run('tp.t2', '2026-03-05 06:00 UTC');
\o
SELECT kind, partition_name, state FROM tp.last_actions('tp', 't2') WHERE kind <> 'create';
SELECT * FROM tp.parts('tp.t2');
SELECT c.relname, c.relispartition, (SELECT count(*) FROM tp.t2_p20260301) AS rows_in_first
  FROM pg_class c WHERE c.oid = 'tp.t2_p20260301'::regclass;
SELECT count(*) AS rows_in_parent FROM tp.t2;
SELECT partition, attached, range_from FROM kingres.timepart_partitions('tp.t2') ORDER BY attached, range_from;
-- A second run does not touch the detached tables, and does not create
-- their cells again.
\o /dev/null
CALL kingres.timepart_run('tp.t2', '2026-03-05 06:00 UTC');
\o
SELECT count(*) AS actions FROM tp.last_actions('tp', 't2');
-- If the retention is later removed, the detached partitions do not come
-- back: the grid starts at the first partition that is attached.
SELECT kingres.timepart_set_policy('tp.t2', '1 day', create_ahead => '1 day');
\o /dev/null
CALL kingres.timepart_run('tp.t2', '2026-03-05 06:00 UTC');
\o
SELECT * FROM tp.last_run('tp', 't2');
SELECT kind, partition_name, state, detail FROM tp.last_actions('tp', 't2');
SELECT count(*) AS rows_in_detached FROM tp.t2_p20260301;

-- A partition that spans the boundary: monthly partitions, retention one month.
CREATE TABLE tp.t3 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.t3', '1 month', '2026-04-15 00:00 UTC', '2026-01-10 00:00 UTC');
INSERT INTO tp.t3 VALUES ('2026-03-01 UTC'), ('2026-03-31 23:00 UTC'), ('2026-04-01 UTC');
SELECT kingres.timepart_set_policy('tp.t3', '1 month', create_ahead => '1 day', retention => '1 month');
-- on 2026-04-10 the boundary is 2026-03-10: January and February are gone,
-- March ends at 04-01, after it, and stays whole with rows older than the
-- retention
\o /dev/null
CALL kingres.timepart_run('tp.t3', '2026-04-10 UTC');
\o
SELECT kind, partition_name FROM tp.last_actions('tp', 't3') WHERE kind <> 'create';
SELECT count(*) AS rows_kept FROM tp.t3 WHERE ts < '2026-04-01 UTC';
-- on 2026-05-01 the boundary is 2026-04-01 and March goes
\o /dev/null
CALL kingres.timepart_run('tp.t3', '2026-05-01 UTC');
\o
SELECT kind, partition_name FROM tp.last_actions('tp', 't3') WHERE kind <> 'create';
SELECT count(*) AS rows_left FROM tp.t3;

-- Naive keys.
CREATE TABLE tp.t4 (d date NOT NULL) PARTITION BY RANGE (d);
SELECT kingres.create_time_partitions('tp.t4', '1 month', '2026-04-15 UTC', '2026-01-10 UTC');
SELECT kingres.timepart_set_policy('tp.t4', '1 month', create_ahead => '1 day', retention => '1 month');
\o /dev/null
CALL kingres.timepart_run('tp.t4', '2026-04-15 UTC');
\o
SELECT kind, partition_name FROM tp.last_actions('tp', 't4') WHERE kind <> 'create';
SELECT * FROM tp.parts('tp.t4');
CREATE TABLE tp.t5 (ts timestamp NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.t5', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('tp.t5', '1 day', create_ahead => '1 day', retention => '2 days', timezone => 'Asia/Tokyo');
-- 2026-03-04 20:00 UTC is 2026-03-05 05:00 in Tokyo: the boundary is 03-03 05:00
\o /dev/null
CALL kingres.timepart_run('tp.t5', '2026-03-04 20:00 UTC');
\o
SELECT kind, partition_name FROM tp.last_actions('tp', 't5') WHERE kind <> 'create';

-- drop_old_time_partitions by hand: the bound is inclusive, a number is
-- returned, detach_only keeps the tables, and a default partition is never
-- touched.
CREATE TABLE tp.t6 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.t6', '1 day', '2026-03-06 UTC', '2026-03-01 UTC');
CREATE TABLE tp.t6_default PARTITION OF tp.t6 DEFAULT;
INSERT INTO tp.t6 VALUES ('2025-01-01 UTC');
SHOW lock_timeout;
SELECT kingres.drop_old_time_partitions('tp.t6', '2026-03-02 00:00 UTC');
SHOW lock_timeout;
SELECT part FROM tp.parts('tp.t6') ORDER BY part;
SELECT kingres.drop_old_time_partitions('tp.t6', '2026-03-03 23:59:59 UTC');
SELECT kingres.drop_old_time_partitions('tp.t6', '2026-03-04 UTC', detach_only => true);
SELECT part FROM tp.parts('tp.t6') ORDER BY part;
SELECT to_regclass('tp.t6_p20260303') IS NOT NULL AS detached_table_exists;
SELECT kingres.drop_old_time_partitions('tp.t6', '2100-01-01 UTC');
SELECT part FROM tp.parts('tp.t6');
SELECT count(*) AS default_rows FROM tp.t6_default;
SELECT kingres.drop_old_time_partitions('tp.t6', 'infinity');
SELECT kingres.drop_old_time_partitions('tp.t6', '2026-03-02 UTC', timezone => 'Mars/Olympus');
-- Partitions with bounds off the grid go by their upper bound.
CREATE TABLE tp.t7 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.t7_a PARTITION OF tp.t7 FOR VALUES FROM ('2026-03-01 06:00 UTC') TO ('2026-03-02 18:00 UTC');
CREATE TABLE tp.t7_b PARTITION OF tp.t7 FOR VALUES FROM ('2026-03-02 18:00 UTC') TO ('2026-03-05 00:00 UTC');
SELECT kingres.drop_old_time_partitions('tp.t7', '2026-03-02 18:00 UTC');
SELECT part FROM tp.parts('tp.t7');

-- A partition that cannot be dropped: a view depends on it.  The action
-- fails with the reason, the run is "partial", the other partitions go, and
-- the next run finishes once the view is gone.
CREATE TABLE tp.t8 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.t8', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
CREATE VIEW tp.t8_view AS SELECT * FROM tp.t8_p20260302;
SELECT kingres.timepart_set_policy('tp.t8', '1 day', create_ahead => '1 day', retention => '2 days');
\o /dev/null
CALL kingres.timepart_run('tp.t8', '2026-03-06 UTC');
\o
SELECT * FROM tp.last_run('tp', 't8');
SELECT kind, partition_name, state, detail FROM tp.last_actions('tp', 't8') WHERE kind <> 'create';
SELECT part FROM tp.parts('tp.t8');
-- The failed drop was rolled back whole: the partition is still attached with its rows.
SELECT count(*) AS still_attached FROM pg_inherits WHERE inhrelid = 'tp.t8_p20260302'::regclass;
DROP VIEW tp.t8_view;
\o /dev/null
CALL kingres.timepart_run('tp.t8', '2026-03-06 UTC');
\o
SELECT * FROM tp.last_run('tp', 't8');
SELECT kind, partition_name, state FROM tp.last_actions('tp', 't8') WHERE kind <> 'create';

-- DETACH ... CONCURRENTLY cannot run inside a function or a procedure
-- (design item V14), which is why the retirement does not use it.  The text
-- of the message differs between PostgreSQL 18 and 19, so only the SQLSTATE
-- is shown (25001, active_sql_transaction).
CREATE TABLE tp.t9 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.t9', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
\set VERBOSITY sqlstate
DO $$ BEGIN ALTER TABLE tp.t9 DETACH PARTITION tp.t9_p20260301 CONCURRENTLY; END $$;
CREATE PROCEDURE tp.detach_concurrently() LANGUAGE plpgsql AS $$ BEGIN ALTER TABLE tp.t9 DETACH PARTITION tp.t9_p20260301 CONCURRENTLY; END $$;
CALL tp.detach_concurrently();
\set VERBOSITY default
SELECT count(*) AS still_attached FROM pg_inherits WHERE inhrelid = 'tp.t9_p20260301'::regclass;

-- A partition that the policy does not recognise as its own (bounds off the
-- grid) is reported and never dropped or detached by a run, even when it is
-- older than the retention, and the check names it.  The call by hand takes
-- any partition.
CREATE TABLE tp.t10 (ts timestamptz NOT NULL, v int) PARTITION BY RANGE (ts);
CREATE TABLE tp.t10_hand PARTITION OF tp.t10 FOR VALUES FROM ('2026-03-03 06:00 UTC') TO ('2026-03-04 06:00 UTC');
INSERT INTO tp.t10 VALUES ('2026-03-03 12:00 UTC', 42);
SELECT kingres.timepart_set_policy('tp.t10', '1 day', create_ahead => '1 day', retention => '3 days');
SELECT kind, partition_name FROM kingres.timepart_plan('tp.t10', '2026-03-10 UTC') WHERE kind <> 'create';
\o /dev/null
CALL kingres.timepart_run('tp.t10', '2026-03-10 UTC');
\o
SELECT kind, partition_name, state, detail FROM tp.last_actions('tp', 't10') WHERE kind <> 'create';
SELECT to_regclass('tp.t10_hand') IS NOT NULL AS still_there, count(*) AS rows_kept FROM tp.t10;
SELECT problem, detail FROM kingres.timepart_check('1 hour', '2026-03-10 UTC') WHERE table_name = 't10' ORDER BY problem;
SELECT kingres.drop_old_time_partitions('tp.t10', '2026-03-10 UTC');
SELECT to_regclass('tp.t10_hand') IS NULL AS dropped_by_hand;

-- A partition that a foreign key references cannot be dropped: that action
-- fails with PostgreSQL's reason, every later run is partial until the
-- referencing row is gone, and the check names the partition.
CREATE TABLE tp.t11 (ts timestamptz NOT NULL, id int, PRIMARY KEY (ts, id)) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.t11', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
CREATE TABLE tp.t11_child (ts timestamptz, id int, FOREIGN KEY (ts, id) REFERENCES tp.t11 (ts, id));
INSERT INTO tp.t11 VALUES ('2026-03-01 10:00 UTC', 1), ('2026-03-02 10:00 UTC', 2);
INSERT INTO tp.t11_child VALUES ('2026-03-01 10:00 UTC', 1);
SELECT kingres.timepart_set_policy('tp.t11', '1 day', create_ahead => '1 day', retention => '2 days');
\o /dev/null
CALL kingres.timepart_run('tp.t11', '2026-03-06 UTC');
\o
SELECT * FROM tp.last_run('tp', 't11');
SELECT kind, partition_name, state, left(detail, 52) AS detail FROM tp.last_actions('tp', 't11') WHERE kind <> 'create';
SELECT problem, detail FROM kingres.timepart_check('1 hour', '2026-03-06 UTC') WHERE table_name = 't11' ORDER BY problem;
\o /dev/null
CALL kingres.timepart_run('tp.t11', '2026-03-06 UTC');
\o
SELECT * FROM tp.last_run('tp', 't11');
DELETE FROM tp.t11_child;
\o /dev/null
CALL kingres.timepart_run('tp.t11', '2026-03-06 UTC');
\o
SELECT * FROM tp.last_run('tp', 't11');
SELECT to_regclass('tp.t11_p20260301') IS NULL AS dropped_now;
