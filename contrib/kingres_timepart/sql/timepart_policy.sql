-- timepart_policy: setting, replacing and removing a policy, what it
-- refuses, the plan, due policies, the view, renames and drops of the
-- table, and the checks.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

CREATE TABLE tp.pol (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);

-- Tables a policy refuses.
CREATE TABLE tp.p_plain (ts timestamptz);
SELECT kingres.timepart_set_policy('tp.p_plain', '1 day');
CREATE TABLE tp.p_list (ts timestamptz NOT NULL) PARTITION BY LIST (ts);
SELECT kingres.timepart_set_policy('tp.p_list', '1 day');
CREATE TABLE tp.p_int (n int NOT NULL) PARTITION BY RANGE (n);
SELECT kingres.timepart_set_policy('tp.p_int', '1 day');
CREATE TABLE tp.p_def (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.p_def_default PARTITION OF tp.p_def DEFAULT;
SELECT kingres.timepart_set_policy('tp.p_def', '1 day');
CREATE TABLE tp.p_date (d date NOT NULL) PARTITION BY RANGE (d);
SELECT kingres.timepart_set_policy('tp.p_date', '6 hours');

-- Arguments a policy refuses.
SELECT kingres.timepart_set_policy('tp.pol', '5 hours');
SELECT kingres.timepart_set_policy('tp.pol', '1 day', timezone => 'Mars/Olympus');
SELECT kingres.timepart_set_policy('tp.pol', '1 day', create_ahead => '-1 day');
SELECT kingres.timepart_set_policy('tp.pol', '1 day', run_every => '0');
SELECT kingres.timepart_set_policy('tp.pol', '1 day', retention => '7 days', retention_action => 'truncate');
SELECT kingres.timepart_set_policy('tp.pol', '1 week', retention => '3 days');
SELECT kingres.timepart_set_policy('tp.pol', '1 day', retention => '0');
SELECT kingres.timepart_set_policy('tp.pol', '1 day', retention => '30 days', columnar_after => '31 days');
SELECT kingres.timepart_set_policy('tp.pol', '1 day', columnar_options => '[1]');
SELECT kingres.timepart_set_policy('tp.pol', '1 day', columnar_sort => '{}');
SELECT count(*) AS policies FROM kingres.timepart_policy;

-- A policy.
SELECT kingres.timepart_set_policy('tp.pol', '1 day', create_ahead => '3 days', retention => '10 days');
SELECT schema_name, table_name, partition_every, timezone, create_ahead, retention,
       retention_action, columnar_after, run_every, enabled, table_owner, created_by
  FROM kingres.timepart_policy;
-- Replace it: the creator stays, the rest changes.
SELECT kingres.timepart_set_policy('tp.pol', '1 week', create_ahead => '14 days', retention => '60 days',
                                   retention_action => 'detach', columnar_after => '30 days',
                                   columnar_sort => '{ts,id}', columnar_options => '{"compression": "zstd"}',
                                   timezone => 'Europe/Berlin', run_every => '2 hours', enabled => false);
SELECT schema_name, table_name, partition_every, timezone, create_ahead, retention,
       retention_action, columnar_after, columnar_sort, columnar_options, run_every, enabled,
       created_by, updated_at >= created_at AS updated
  FROM kingres.timepart_policy;
SELECT count(*) AS policies FROM kingres.timepart_policy;

-- Due policies: an enabled one whose next_run_at has passed.
SELECT * FROM kingres.timepart_due('2000-01-01');
SELECT * FROM kingres.timepart_due('2100-01-01');
SELECT kingres.timepart_set_policy('tp.pol', '1 day', create_ahead => '3 days', retention => '10 days');
SELECT * FROM kingres.timepart_due('2100-01-01');
SELECT schema_name, table_name, last_run_state, enabled FROM kingres.timepart_policies;

-- The plan at a given time: a table with no partitions gets the cell of
-- as_of and those ahead of it.
SELECT * FROM kingres.timepart_plan('tp.pol', '2026-03-10 05:30 UTC');
-- With partitions: the missing ones from the first existing one on, none
-- older than the retention boundary, and the old ones retired.
SELECT kingres.create_time_partitions('tp.pol', '1 day', '2026-03-10 00:00 UTC', '2026-02-25 00:00 UTC');
SELECT seq, kind, partition_name, range_from, range_to FROM kingres.timepart_plan('tp.pol', '2026-03-10 05:30 UTC');
-- Planning is a pure function of the catalog and the time: nothing changed.
SELECT count(*) AS partitions FROM tp.parts('tp.pol');
SELECT count(*) AS runs FROM kingres.timepart_run WHERE table_name = 'pol';
-- The plan at a time before the data: nothing to retire.
SELECT kind, count(*) FROM kingres.timepart_plan('tp.pol', '2026-02-26 05:30 UTC') GROUP BY kind ORDER BY kind;

-- Partitions with other bounds are reported and left alone.
CREATE TABLE tp.pol2 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.pol2_odd PARTITION OF tp.pol2 FOR VALUES FROM ('2026-03-03 06:00 UTC') TO ('2026-03-04 06:00 UTC');
CREATE TABLE tp.pol2_ok PARTITION OF tp.pol2 FOR VALUES FROM ('2026-03-05 00:00 UTC') TO ('2026-03-06 00:00 UTC');
SELECT kingres.timepart_set_policy('tp.pol2', '1 day', create_ahead => '1 day');
SELECT seq, kind, partition_name, range_from, range_to, reason
  FROM kingres.timepart_plan('tp.pol2', '2026-03-05 10:00 UTC');
-- A plan on a table without a policy.
SELECT * FROM kingres.timepart_plan('tp.p_plain');
SELECT * FROM kingres.timepart_plan('tp.p_def');

-- The policy follows a rename of the table, and a move to another schema,
-- and goes away with the table; the history of runs stays.
CREATE TABLE tp.ren (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.ren', '1 month');
\o /dev/null
CALL kingres.timepart_run('tp.ren', '2026-03-10 UTC');
\o
ALTER TABLE tp.ren RENAME TO ren2;
SELECT schema_name, table_name FROM kingres.timepart_policy WHERE table_name LIKE 'ren%';
CREATE SCHEMA tp_moved;
ALTER TABLE tp.ren2 SET SCHEMA tp_moved;
SELECT schema_name, table_name FROM kingres.timepart_policy WHERE table_name LIKE 'ren%';
\o /dev/null
CALL kingres.timepart_run('tp_moved.ren2', '2026-03-11 UTC');
\o
SELECT count(*) AS runs_of_moved_table FROM kingres.timepart_run WHERE table_name = 'ren2';
DROP TABLE tp_moved.ren2;
SELECT count(*) AS policies_left FROM kingres.timepart_policy WHERE table_name LIKE 'ren%';
-- the history is kept, under the last name of the table
SELECT count(*) AS history_kept FROM kingres.timepart_run WHERE table_name = 'ren2';
SELECT count(*) AS history_under_old_name FROM kingres.timepart_run WHERE table_name = 'ren';
-- Renaming another table does not touch the policy of this one.
CREATE TABLE tp.other (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tp.other RENAME TO other2;
SELECT count(*) AS policies FROM kingres.timepart_policy WHERE table_name = 'pol';
-- A rename onto the name of another policy's table cannot happen: the name
-- is taken.  A table dropped and created again with the same name has no
-- policy.
DROP TABLE tp.other2;
CREATE TABLE tp.pol3 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.pol3', '1 day');
DROP TABLE tp.pol3;
CREATE TABLE tp.pol3 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT * FROM kingres.timepart_plan('tp.pol3');

-- unset: the policy goes, the history stays.
SELECT kingres.timepart_unset_policy('tp.pol3');
SELECT kingres.timepart_set_policy('tp.pol3', '1 day');
SELECT kingres.timepart_unset_policy('tp.pol3');
SELECT count(*) AS policies FROM kingres.timepart_policy WHERE table_name = 'pol3';
SELECT kingres.timepart_unset_policy('tp.p_plain');

-- Checks, at a time of our choice.
SELECT * FROM kingres.timepart_check(as_of => '2026-03-10 05:30 UTC');
-- the last partition of tp.pol ends 2026-03-11: more than half of 3 days is
-- not ahead at 2026-03-10; asking for less passes it
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-10 05:30 UTC');
CREATE TABLE tp.chk_def (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.chk_def', '1 day');
CREATE TABLE tp.chk_def_default PARTITION OF tp.chk_def DEFAULT;
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-10 05:30 UTC') WHERE table_name = 'chk_def';
-- an orphan: the schema was renamed, so the policy names a table that is gone
CREATE SCHEMA tp_old;
CREATE TABLE tp_old.gone (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp_old.gone', '1 day');
ALTER SCHEMA tp_old RENAME TO tp_new;
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-10 05:30 UTC') WHERE table_name = 'gone';
DROP SCHEMA tp_new CASCADE;
-- the table is gone with its schema, and with it the policy
SELECT count(*) AS orphans FROM kingres.timepart_check() WHERE problem = 'policy_orphan';

-- The partitions of a table with their format.
SELECT partition, range_from, range_to, access_method, attached FROM kingres.timepart_partitions('tp.pol');
