-- timepart_names: hostile and awkward names of schemas, tables, columns and
-- roles go through every statement the extension builds, quoted; names near
-- the 63-byte limit; what happens to generated partition names at the limit.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

-- Things that must survive: a table that an injected statement would drop,
-- and a marker table that it would create.
CREATE TABLE tp.canary (id int);
INSERT INTO tp.canary VALUES (1);

-- Quotes, a semicolon, an SQL comment, a dot and upper case, in the schema,
-- the table and the column.  Table and schema names go through create,
-- the policy, the plan, a run that creates and retires, and the checks.
CREATE SCHEMA "Sch ""ema""; DROP TABLE tp.canary; --";
CREATE TABLE "Sch ""ema""; DROP TABLE tp.canary; --"."Evil ""Table"" '; DROP TABLE tp.canary; --"
    ("Ti""me'; DROP TABLE tp.canary; --" timestamptz NOT NULL, "Id" int DEFAULT 1, CONSTRAINT "bad;name" CHECK ("Id" > 0))
    PARTITION BY RANGE ("Ti""me'; DROP TABLE tp.canary; --");
CREATE INDEX "idx ""x""" ON "Sch ""ema""; DROP TABLE tp.canary; --"."Evil ""Table"" '; DROP TABLE tp.canary; --" ("Id");
SELECT kingres.create_time_partitions(
    '"Sch ""ema""; DROP TABLE tp.canary; --"."Evil ""Table"" ''; DROP TABLE tp.canary; --"',
    '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = '"Sch ""ema""; DROP TABLE tp.canary; --"."Evil ""Table"" ''; DROP TABLE tp.canary; --"'::regclass
 ORDER BY 1;
INSERT INTO "Sch ""ema""; DROP TABLE tp.canary; --"."Evil ""Table"" '; DROP TABLE tp.canary; --" VALUES ('2026-03-02 UTC');
SELECT count(*) AS rows FROM "Sch ""ema""; DROP TABLE tp.canary; --"."Evil ""Table"" '; DROP TABLE tp.canary; --";
SELECT kingres.timepart_set_policy(
    '"Sch ""ema""; DROP TABLE tp.canary; --"."Evil ""Table"" ''; DROP TABLE tp.canary; --"',
    '1 day', create_ahead => '2 days', retention => '2 days');
\o /dev/null
CALL kingres.timepart_run('"Sch ""ema""; DROP TABLE tp.canary; --"."Evil ""Table"" ''; DROP TABLE tp.canary; --"', '2026-03-05 12:00 UTC');
\o
SELECT state FROM kingres.timepart_run WHERE table_name LIKE 'Evil%';
SELECT a.kind, a.partition_name, a.state FROM kingres.timepart_run_action a JOIN kingres.timepart_run r USING (run_id)
 WHERE r.table_name LIKE 'Evil%' ORDER BY a.run_id, a.seq;
SELECT schema_name, table_name FROM kingres.timepart_policy WHERE table_name LIKE 'Evil%';
SELECT problem FROM kingres.timepart_check('1 hour', '2026-03-05 12:00 UTC') WHERE table_name LIKE 'Evil%';
SELECT count(*) AS canary_rows FROM tp.canary;
SELECT count(*) AS stray_tables FROM pg_class WHERE relname LIKE '%DROP TABLE%' AND relkind = 'r' AND relispartition = false
   AND relnamespace NOT IN (SELECT oid FROM pg_namespace WHERE nspname LIKE 'Sch%');
-- Rename of the table with those names: the policy follows.
ALTER TABLE "Sch ""ema""; DROP TABLE tp.canary; --"."Evil ""Table"" '; DROP TABLE tp.canary; --" RENAME TO "Renamed'; DROP TABLE tp.canary; --";
SELECT table_name FROM kingres.timepart_policy WHERE table_name LIKE 'Renamed%';
-- The columns of the policy table survive the quotes in a plan.
SELECT kind, partition_name FROM kingres.timepart_plan('"Sch ""ema""; DROP TABLE tp.canary; --"."Renamed''; DROP TABLE tp.canary; --"', '2026-03-20 12:00 UTC') LIMIT 2;

-- Upper case, reserved words, a dot and a newline in a table name.
CREATE TABLE tp."MixedCase" (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp."select" (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp."a.b" (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.table_with_newline_x (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE tp.table_with_newline_x RENAME TO "new
line";
SELECT kingres.timepart_set_policy('tp."MixedCase"', '1 month', create_ahead => '1 day');
SELECT kingres.timepart_set_policy('tp."select"', '1 month', create_ahead => '1 day');
SELECT kingres.timepart_set_policy('tp."a.b"', '1 month', create_ahead => '1 day');
SELECT kingres.timepart_set_policy('tp."new
line"', '1 month', create_ahead => '1 day');
\o /dev/null
CALL kingres.timepart_run('tp."MixedCase"', '2026-03-05 UTC');
CALL kingres.timepart_run('tp."select"', '2026-03-05 UTC');
CALL kingres.timepart_run('tp."a.b"', '2026-03-05 UTC');
CALL kingres.timepart_run('tp."new
line"', '2026-03-05 UTC');
\o
SELECT r.table_name, r.state, a.partition_name
  FROM kingres.timepart_run r JOIN kingres.timepart_run_action a USING (run_id)
 WHERE r.table_name IN ('MixedCase', 'select', 'a.b', E'new\nline') ORDER BY r.table_name;
SELECT count(*) AS partitions_of_a_b FROM pg_inherits WHERE inhparent = 'tp."a.b"'::regclass;

-- Names at the limit.  A table name of 63 bytes: the partition names are the
-- name cut so that the whole name is 63 bytes, with the suffix whole.
CREATE TABLE tp.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT length('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa') AS table_name_length;
SELECT kingres.create_time_partitions('tp.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', '1 day', '2026-03-02 UTC', '2026-03-01 UTC');
SELECT c.relname, length(c.relname) AS chars, octet_length(c.relname) AS bytes
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = 'tp.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'::regclass ORDER BY 1;
-- Hourly partitions have a longer suffix and cut the name more.
CREATE TABLE tp.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', '12 hours', '2026-03-01 12:00 UTC', '2026-03-01 UTC');
SELECT c.relname, octet_length(c.relname) AS bytes
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = 'tp.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'::regclass ORDER BY 1;
-- Multi-byte characters are cut whole: 31 two-byte letters are 62 bytes.
CREATE TABLE tp.ééééééééééééééééééééééééééééééé (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.ééééééééééééééééééééééééééééééé', '1 month', '2026-03-02 UTC', '2026-03-01 UTC');
SELECT c.relname, length(c.relname) AS chars, octet_length(c.relname) AS bytes
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = 'tp.ééééééééééééééééééééééééééééééé'::regclass ORDER BY 1;
-- Two tables whose names differ only in the part that is cut get the same
-- partition names.  The second one's run reports the conflict and neither
-- table is changed by it.
CREATE TABLE tp.cccccccccccccccccccccccccccccccccccccccccccccccccccccccc1 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tp.cccccccccccccccccccccccccccccccccccccccccccccccccccccccc2 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('tp.cccccccccccccccccccccccccccccccccccccccccccccccccccccccc1', '1 day', create_ahead => '1 day');
SELECT kingres.timepart_set_policy('tp.cccccccccccccccccccccccccccccccccccccccccccccccccccccccc2', '1 day', create_ahead => '1 day');
\o /dev/null
CALL kingres.timepart_run('tp.cccccccccccccccccccccccccccccccccccccccccccccccccccccccc1', '2026-03-05 UTC');
CALL kingres.timepart_run('tp.cccccccccccccccccccccccccccccccccccccccccccccccccccccccc2', '2026-03-05 UTC');
\o
SELECT r.table_name, r.state, a.partition_name, a.state AS action_state, left(a.detail, 40) AS detail
  FROM kingres.timepart_run r JOIN kingres.timepart_run_action a USING (run_id)
 WHERE r.table_name LIKE 'cccc%' ORDER BY r.run_id, a.seq;
SELECT c.relname, octet_length(c.relname) AS bytes
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = 'tp.cccccccccccccccccccccccccccccccccccccccccccccccccccccccc2'::regclass;

-- A hostile role as the owner of the table: the run switches to it with a
-- quoted name, and the partitions are owned by it.
CREATE ROLE "Own'er""; DROP TABLE tp.canary; --" NOLOGIN;
CREATE SCHEMA hostile_owner;
CREATE TABLE hostile_owner.t (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE hostile_owner.t OWNER TO "Own'er""; DROP TABLE tp.canary; --";
GRANT USAGE, CREATE ON SCHEMA hostile_owner TO "Own'er""; DROP TABLE tp.canary; --";
SELECT kingres.timepart_set_policy('hostile_owner.t', '1 day', create_ahead => '1 day');
SELECT table_owner = 'Own''er"; DROP TABLE tp.canary; --' AS owner_recorded FROM kingres.timepart_policy WHERE schema_name = 'hostile_owner';
\o /dev/null
CALL kingres.timepart_run('hostile_owner.t', '2026-03-05 UTC');
\o
SELECT r.state, a.state AS action_state, a.detail FROM kingres.timepart_run r JOIN kingres.timepart_run_action a USING (run_id) WHERE r.schema_name = 'hostile_owner';
SELECT c.relname, pg_get_userbyid(c.relowner) = 'Own''er"; DROP TABLE tp.canary; --' AS owned_by_the_owner
  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = 'hostile_owner.t'::regclass ORDER BY 1;
SELECT count(*) AS canary_rows FROM tp.canary;
