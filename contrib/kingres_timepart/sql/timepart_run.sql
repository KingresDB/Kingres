-- timepart_run: runs with a clock under the test's control: creation ahead,
-- idempotence, interrupted and repeated runs, name conflicts, partitions
-- made by hand, a default partition with rows, the conversion step without
-- kingres_columnar, daylight saving, naive keys, failed runs, history.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: PostgreSQL

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

-- A first run: partitions from the cell of as_of to three days ahead.
CREATE TABLE tp.r1 (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.r1', '1 day', create_ahead => '3 days', retention => '5 days', run_every => '1 hour');
\o /dev/null
CALL kingres.timepart_run('tp.r1', '2026-03-10 05:30 UTC');
\o
SELECT * FROM tp.last_run('tp', 'r1');
SELECT * FROM tp.last_actions('tp', 'r1');
SELECT * FROM tp.parts('tp.r1');
SELECT * FROM tp.gaps('tp.r1');
-- The run's own columns, and the next run time computed from as_of.
SELECT r.as_of, r.trigger, r.state, r.finished_at IS NOT NULL AS finished, r.error
  FROM kingres.timepart_run r WHERE r.table_name = 'r1';
SELECT next_run_at FROM kingres.timepart_policy WHERE table_name = 'r1';
SELECT * FROM kingres.timepart_due('2026-03-10 06:29 UTC') WHERE table_name = 'r1';
SELECT * FROM kingres.timepart_due('2026-03-10 06:30 UTC') WHERE table_name = 'r1';

-- The same run again does nothing: the plan is empty.
SELECT count(*) AS planned FROM kingres.timepart_plan('tp.r1', '2026-03-10 05:30 UTC');
\o /dev/null
CALL kingres.timepart_run('tp.r1', '2026-03-10 05:30 UTC');
\o
SELECT * FROM tp.last_run('tp', 'r1');
SELECT count(*) AS actions FROM tp.last_actions('tp', 'r1');
SELECT count(*) AS partitions FROM tp.parts('tp.r1');
-- Two days later: new partitions, and the old ones retired.
\o /dev/null
CALL kingres.timepart_run('tp.r1', '2026-03-12 23:30 UTC');
\o
SELECT * FROM tp.last_actions('tp', 'r1');
SELECT * FROM tp.parts('tp.r1');
SELECT * FROM tp.gaps('tp.r1');
-- A long pause: the table catches up from the last partition; the cells in
-- between are older than the retention boundary and are not created.
\o /dev/null
CALL kingres.timepart_run('tp.r1', '2026-06-01 12:00 UTC');
\o
SELECT * FROM tp.last_actions('tp', 'r1') WHERE kind = 'create';
SELECT kind, count(*) FROM tp.last_actions('tp', 'r1') GROUP BY kind ORDER BY kind;
SELECT * FROM tp.parts('tp.r1');

-- A run in pieces, and a run that stopped half way: begin and the create
-- phase only; the next run (in this session, which still holds the lock of
-- the unfinished one) marks it interrupted and does what is left.
CREATE TABLE tp.r2 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.r2', '1 day', create_ahead => '2 days', retention => '3 days');
SELECT kingres.create_time_partitions('tp.r2', '1 day', '2026-03-10 00:00 UTC', '2026-03-09 00:00 UTC');
SELECT kingres.timepart_begin_run('tp.r2', '2026-03-10 12:00 UTC', 'manual') AS run_id \gset first_
CALL kingres.timepart_run_phase(:first_run_id, 'create');
SELECT state, finished_at IS NULL AS open FROM kingres.timepart_run WHERE run_id = :first_run_id;
SELECT kind, partition_name, state FROM kingres.timepart_run_action WHERE run_id = :first_run_id ORDER BY seq;
-- (this session has not finished the run; a second begin takes over)
SELECT kingres.timepart_begin_run('tp.r2', '2026-03-10 12:00 UTC', 'manual') AS run_id \gset second_
SELECT run_id = :first_run_id AS same_run, state, error FROM kingres.timepart_run WHERE run_id IN (:first_run_id, :second_run_id) ORDER BY run_id;
CALL kingres.timepart_run_phase(:second_run_id, 'create');
CALL kingres.timepart_run_phase(:second_run_id, 'convert');
CALL kingres.timepart_run_phase(:second_run_id, 'retire');
-- Phases that are repeated do nothing more.
CALL kingres.timepart_run_phase(:second_run_id, 'create');
CALL kingres.timepart_run_phase(:second_run_id, 'retire');
SELECT kingres.timepart_finish_run(:second_run_id);
SELECT kingres.timepart_finish_run(:second_run_id);
SELECT state, error FROM kingres.timepart_run WHERE run_id = :second_run_id;
SELECT kind, partition_name, state FROM kingres.timepart_run_action WHERE run_id = :second_run_id ORDER BY seq;
SELECT * FROM tp.parts('tp.r2');
-- A finished run takes no more work.
CALL kingres.timepart_run_phase(:second_run_id, 'create');
SELECT count(*) AS actions FROM kingres.timepart_run_action WHERE run_id = :second_run_id;
CALL kingres.timepart_run_phase(:second_run_id, 'sideways');
SELECT kingres.timepart_finish_run(0);
-- The run records the backend that made it, so that a later begin can tell a live
-- run from an abandoned one.
SELECT backend_pid = pg_backend_pid() AS own_backend, backend_start IS NOT NULL AS has_start
  FROM kingres.timepart_run WHERE run_id = :second_run_id;

-- Procedures that commit cannot run inside a transaction block.
BEGIN;
CALL kingres.timepart_run('tp.r2', '2026-03-10 12:00 UTC');
ROLLBACK;
SELECT count(*) AS runs_after_rollback FROM kingres.timepart_run WHERE table_name = 'r2';

-- A name that is taken: that action fails and the run goes on; the run ends
-- "partial".  The next run, once the name is free, finishes the work.
CREATE TABLE tp.r3 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.r3_p20260311 (x int);
SELECT kingres.timepart_set_policy('tp.r3', '1 day', create_ahead => '2 days');
\o /dev/null
CALL kingres.timepart_run('tp.r3', '2026-03-10 05:00 UTC');
\o
SELECT * FROM tp.last_run('tp', 'r3');
SELECT * FROM tp.last_actions('tp', 'r3');
SELECT * FROM tp.parts('tp.r3');
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-10 05:00 UTC') WHERE table_name = 'r3';
DROP TABLE tp.r3_p20260311;
\o /dev/null
CALL kingres.timepart_run('tp.r3', '2026-03-10 05:00 UTC');
\o
SELECT * FROM tp.last_run('tp', 'r3');
SELECT * FROM tp.last_actions('tp', 'r3');
SELECT * FROM tp.parts('tp.r3');
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-10 05:00 UTC') WHERE table_name = 'r3';

-- Partitions made by hand with other bounds: reported, left alone, and the
-- cells around them are created; the existing ones are never changed.
CREATE TABLE tp.r4 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.r4_odd PARTITION OF tp.r4 FOR VALUES FROM ('2026-03-09 18:00 UTC') TO ('2026-03-10 18:00 UTC');
CREATE TABLE tp.r4_ok PARTITION OF tp.r4 FOR VALUES FROM ('2026-03-07 00:00 UTC') TO ('2026-03-08 00:00 UTC');
SELECT kingres.timepart_set_policy('tp.r4', '1 day', create_ahead => '3 days');
\o /dev/null
CALL kingres.timepart_run('tp.r4', '2026-03-10 05:00 UTC');
\o
SELECT * FROM tp.last_run('tp', 'r4');
SELECT * FROM tp.last_actions('tp', 'r4');
SELECT * FROM tp.parts('tp.r4');
SELECT * FROM tp.gaps('tp.r4');
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-10 05:00 UTC') WHERE table_name = 'r4';

-- A default partition created after the policy.  Where it holds no row of
-- a range to create, the attach succeeds.  Where it holds rows of the
-- range, PostgreSQL refuses the attach: that action fails with its error, the
-- run ends "partial", the other partitions are created, and the run does not
-- loop or retry; the next run tries again.
CREATE TABLE tp.r5 (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.r5', '1 day', create_ahead => '3 days');
CREATE TABLE tp.r5_default PARTITION OF tp.r5 DEFAULT;
INSERT INTO tp.r5 VALUES (1, '2026-03-12 10:00 UTC'), (2, '2026-03-15 10:00 UTC');
SELECT tableoid::regclass AS part, id FROM tp.r5 ORDER BY id;
\o /dev/null
CALL kingres.timepart_run('tp.r5', '2026-03-10 05:00 UTC');
\o
SELECT * FROM tp.last_run('tp', 'r5');
SELECT * FROM tp.last_actions('tp', 'r5');
SELECT * FROM tp.parts('tp.r5');
SELECT count(*) AS actions_in_run FROM kingres.timepart_run_action a
 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'r5');
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-10 05:00 UTC') WHERE table_name = 'r5' ORDER BY problem;
-- The rows are untouched.
SELECT tableoid::regclass AS part, id FROM tp.r5 ORDER BY id;
-- Move the rows out of the way; the next run succeeds.
CREATE TEMP TABLE r5_rows AS SELECT * FROM tp.r5_default;
DELETE FROM tp.r5_default;
\o /dev/null
CALL kingres.timepart_run('tp.r5', '2026-03-10 05:00 UTC');
\o
SELECT * FROM tp.last_run('tp', 'r5');
SELECT * FROM tp.last_actions('tp', 'r5');
INSERT INTO tp.r5 SELECT * FROM r5_rows;
SELECT tableoid::regclass AS part, id FROM tp.r5 ORDER BY id;

-- The conversion step: nothing to convert yet, then a partition older than
-- columnar_after; without kingres_columnar the step reports it and skips,
-- and the run is "done".
CREATE TABLE tp.r6 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.r6', '1 day', create_ahead => '1 day', columnar_after => '2 days');
SELECT kingres.create_time_partitions('tp.r6', '1 day', '2026-03-05 00:00 UTC', '2026-03-01 00:00 UTC');
\o /dev/null
CALL kingres.timepart_run('tp.r6', '2026-03-10 05:00 UTC');
\o
SELECT * FROM tp.last_run('tp', 'r6');
SELECT kind, partition_name, state, detail FROM tp.last_actions('tp', 'r6') WHERE kind = 'convert';
SELECT kind, count(*) FROM tp.last_actions('tp', 'r6') GROUP BY kind ORDER BY kind;
SELECT seq, kind, partition_name FROM kingres.timepart_plan('tp.r6', '2026-03-10 05:00 UTC') WHERE kind = 'convert';
CALL kingres.convert_partition('tp.r6', 'tp.r6_p20260301');
CALL kingres.convert_old_partitions('tp.r6', '2026-03-03 UTC');
SELECT NOT EXISTS (SELECT 1 FROM kingres.timepart_run_action WHERE kind = 'convert' AND state = 'failed') AS no_convert_failure;

-- Daylight saving over a run: daily partitions in Berlin, a run the day
-- before the spring change and another after it.  The cells follow the
-- local calendar and nothing overlaps or is missing.
CREATE TABLE tp.r7 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.r7', '1 day', create_ahead => '3 days', timezone => 'Europe/Berlin');
\o /dev/null
CALL kingres.timepart_run('tp.r7', '2026-03-27 12:00 UTC');
CALL kingres.timepart_run('tp.r7', '2026-03-30 12:00 UTC');
\o
SELECT * FROM tp.parts('tp.r7');
SELECT * FROM tp.gaps('tp.r7');
-- The autumn change, from an empty table.
CREATE TABLE tp.r8 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.r8', '1 day', create_ahead => '3 days', timezone => 'Europe/Berlin');
\o /dev/null
CALL kingres.timepart_run('tp.r8', '2026-10-24 12:00 UTC');
\o
SELECT * FROM tp.parts('tp.r8');
-- Hours across the change: six-hour partitions are six hours long.
CREATE TABLE tp.r9 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.r9', '6 hours', create_ahead => '1 day', timezone => 'Europe/Berlin');
\o /dev/null
CALL kingres.timepart_run('tp.r9', '2026-03-28 20:00 UTC');
\o
SELECT part, bounds FROM tp.parts('tp.r9');
SELECT * FROM tp.gaps('tp.r9');

-- Naive keys: the policy's zone says which wall clock "now" is.  22:00 UTC
-- on 31 March is 1 April in Tokyo.
CREATE TABLE tp.r10 (d date NOT NULL) PARTITION BY RANGE (d);
SELECT kingres.timepart_set_policy('tp.r10', '1 month', create_ahead => '1 day', timezone => 'Asia/Tokyo');
\o /dev/null
CALL kingres.timepart_run('tp.r10', '2026-03-31 22:00 UTC');
\o
SELECT * FROM tp.parts('tp.r10');
CREATE TABLE tp.r11 (ts timestamp NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.r11', '12 hours', create_ahead => '12 hours', timezone => 'America/New_York');
\o /dev/null
CALL kingres.timepart_run('tp.r11', '2026-03-10 03:00 UTC');
\o
SELECT * FROM tp.parts('tp.r11');
SELECT range_from, range_to FROM kingres.timepart_plan('tp.r11', '2026-03-10 03:00 UTC');

-- A run that fails as a whole: too many cells for one run.
CREATE TABLE tp.r12 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.r12', '1 hour', create_ahead => '2 years');
\o /dev/null
CALL kingres.timepart_run('tp.r12', '2026-03-10 05:00 UTC');
\o
SELECT * FROM tp.last_run('tp', 'r12');
SELECT count(*) AS partitions FROM tp.parts('tp.r12');
SELECT * FROM kingres.timepart_check('1 hour', '2026-03-10 05:00 UTC') WHERE table_name = 'r12';
SELECT schema_name, table_name, last_run_state FROM kingres.timepart_policies WHERE table_name IN ('r1', 'r3', 'r12') ORDER BY table_name;
-- Fix the policy and run again.
SELECT kingres.timepart_set_policy('tp.r12', '1 day', create_ahead => '2 days');
\o /dev/null
CALL kingres.timepart_run('tp.r12', '2026-03-10 05:00 UTC');
\o
SELECT * FROM tp.last_run('tp', 'r12');
SELECT count(*) AS partitions FROM tp.parts('tp.r12');

-- A run needs a policy and a table; the run phases need a superuser.
CREATE TABLE tp.r13 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CALL kingres.timepart_run('tp.r13', '2026-03-10 UTC');
SELECT kingres.timepart_begin_run('tp.r1', '2026-03-10 UTC', 'cron');
CALL kingres.timepart_run('tp.r1', '2026-03-10 UTC', '{create,explode}');
CALL kingres.timepart_run('tp.r1', 'infinity');

-- Phases can be chosen: only the retirement.
CREATE TABLE tp.r14 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.r14', '1 day', create_ahead => '1 day', retention => '2 days');
SELECT kingres.create_time_partitions('tp.r14', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');
\o /dev/null
CALL kingres.timepart_run('tp.r14', '2026-03-05 12:00 UTC', '{retire}');
\o
SELECT kind, partition_name, state FROM tp.last_actions('tp', 'r14');
SELECT count(*) AS partitions FROM tp.parts('tp.r14');

-- Pruning: a run is kept for 90 days from the time of the newest, and at
-- most 1000 runs per table.
INSERT INTO kingres.timepart_run (schema_name, table_name, as_of, trigger, state, started_by, finished_at)
SELECT 'tp', 'pruned', timestamptz '2026-01-01 UTC' + g * interval '1 minute', 'sql', 'done', 'x', now()
  FROM generate_series(1, 1005) g;
INSERT INTO kingres.timepart_run (schema_name, table_name, as_of, trigger, state, started_by, finished_at)
VALUES ('tp', 'pruned', '2025-06-01 UTC', 'sql', 'done', 'x', now()),
       ('tp', 'pruned_other', '2025-06-01 UTC', 'sql', 'done', 'x', now());
INSERT INTO kingres.timepart_run (schema_name, table_name, as_of, trigger, state, started_by)
VALUES ('tp', 'pruned', '2024-06-01 UTC', 'sql', 'running', 'x');
SELECT kingres.timepart_prune_runs('tp', 'pruned', '2026-01-02 UTC') AS deleted;
SELECT count(*) AS kept, count(*) FILTER (WHERE state = 'running') AS running,
       min(as_of) > '2025-12-31' AS none_old_but_the_running FROM kingres.timepart_run
 WHERE table_name = 'pruned' AND state <> 'running';
SELECT count(*) AS other_table_kept FROM kingres.timepart_run WHERE table_name = 'pruned_other';
-- The actions of a deleted run go with it.
SELECT count(*) AS orphan_actions FROM kingres.timepart_run_action a
 WHERE NOT EXISTS (SELECT 1 FROM kingres.timepart_run r WHERE r.run_id = a.run_id);

-- User code runs during a run: the index of the parent is copied to every new
-- partition, and an index expression that calls a function evaluates it for
-- a constant argument.  A driver that sets statement_timeout and issues each
-- action as a statement of its own gets that action failed after the limit
-- instead of a session that waits for as long as the function sleeps.
CREATE TABLE tp.r15 (ts timestamptz NOT NULL, v int) PARTITION BY RANGE (ts);
CREATE FUNCTION tp.slowfn(int) RETURNS int LANGUAGE plpgsql IMMUTABLE
  AS $$ BEGIN PERFORM pg_sleep(2); RETURN $1; END $$;
CREATE INDEX r15_slow_idx ON tp.r15 (ts, (tp.slowfn(1)));
SELECT kingres.timepart_set_policy('tp.r15', '1 day', create_ahead => '2 days');
SELECT kingres.timepart_begin_run('tp.r15', '2026-03-10 UTC', 'manual') AS run_id \gset slow_
SELECT kingres.timepart_action_timeout();
SET kingres_timepart.action_timeout_ms = 500;
SELECT kingres.timepart_action_timeout();
SELECT set_config('statement_timeout', kingres.timepart_action_timeout()::text || 'ms', false);
SELECT kingres.timepart_run_action(:slow_run_id, 'create');
SELECT kingres.timepart_run_action(:slow_run_id, 'create');
RESET statement_timeout;
RESET kingres_timepart.action_timeout_ms;
SELECT kingres.timepart_run_action(:slow_run_id, 'create');
SELECT kingres.timepart_finish_run(:slow_run_id);
SELECT kind, partition_name, state, left(detail, 40) AS detail FROM kingres.timepart_run_action WHERE run_id = :slow_run_id ORDER BY seq;
SELECT state FROM kingres.timepart_run WHERE run_id = :slow_run_id;
SELECT count(*) AS partitions FROM tp.parts('tp.r15');
-- a cancel by an operator is not an action's failure: only the timeout is
SELECT kingres.timepart__timed_out() AS timed_out_now;
