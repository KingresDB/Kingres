-- sql/utility_guard.sql
--   Utility statements on distributed tables (review 2026-11-14, finding 3).
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- With one shard group every statement works as before.  Once there are two
-- groups, a statement that would change a distributed table on this node only
-- is refused with a "not supported on distributed tables yet" error, while the
-- per-node maintenance commands (VACUUM, ANALYZE, REINDEX, ...) still work.
-- The second group below does not exist as a server: nothing connects to it,
-- the statements are refused (or accepted) before any remote work.
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

CREATE SCHEMA ugs;
CREATE TYPE ug_color AS ENUM ('red', 'green');
CREATE TABLE ug (id int PRIMARY KEY, v text);
CREATE TABLE ugs.other (id int PRIMARY KEY);
CREATE TABLE ug_c (id int PRIMARY KEY, c ug_color);
CREATE TABLE ug_plain (id int PRIMARY KEY);
SELECT kingres.create_distributed_table('ug', 'id', bucket_count => 4);
SELECT kingres.create_distributed_table('ugs.other', 'id', colocate_with => 'ug');
SELECT kingres.create_distributed_table('ug_c', 'id', colocate_with => 'ug');
INSERT INTO ug SELECT g, 'row ' || g FROM generate_series(1, 10) g;

-- One shard group: everything works as before.
ALTER TABLE ug ADD COLUMN x int;
CREATE INDEX ug_x_idx ON ug (x);
COMMENT ON TABLE ug IS 'one group';
COPY ug (id) FROM '/dev/null';
VACUUM ug;
ANALYZE ug;
REINDEX TABLE ug;
DROP INDEX ug_x_idx;
ALTER TABLE ug DROP COLUMN x;
-- Dropping a column does not make the table forget that it is distributed.
SELECT count(*) AS still_distributed FROM kingres.dist_table
  WHERE relid = 'ug'::regclass;
TRUNCATE ug;
SELECT count(*) AS rows_after_truncate FROM ug;
INSERT INTO ug SELECT g, 'row ' || g FROM generate_series(1, 10) g;
-- Whatever the number of groups, the distribution column and the partitions
-- are not for ad-hoc DDL.
ALTER TABLE ug DROP COLUMN id;
ALTER TABLE ug RENAME COLUMN id TO key;
ALTER TABLE ug ALTER COLUMN id TYPE bigint;
ALTER TABLE ug DETACH PARTITION ug_b0000;
DROP TABLE ug_b0000;

-- A second shard group.
INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (2, 1, 'group1.invalid', 5432, 'primary');
UPDATE kingres.bucket_map SET group_id = 1 WHERE bucket % 2 = 1;

-- Refused, or propagated: with fake group nodes that cannot be reached, a statement that is propagated fails to connect and applies nothing.
\set VERBOSITY terse
TRUNCATE ug;
\set VERBOSITY default
TRUNCATE ug, ug_plain;
TRUNCATE ug_b0000;
\set VERBOSITY terse
ALTER TABLE ug ADD COLUMN x int;
\set VERBOSITY default
\set VERBOSITY terse
ALTER TABLE ug RENAME TO ug_renamed;
\set VERBOSITY default
ALTER TABLE ug SET SCHEMA ugs;
\set VERBOSITY terse
ALTER TABLE ug OWNER TO CURRENT_USER;
\set VERBOSITY default
ALTER TABLE ug ENABLE ROW LEVEL SECURITY;
ALTER INDEX ug_pkey RENAME TO ug_pkey2;
\set VERBOSITY terse
CREATE INDEX ug_v_idx ON ug (v);
\set VERBOSITY default
\set VERBOSITY terse
CREATE UNIQUE INDEX ug_v_uidx ON ug (id, v);
\set VERBOSITY default
CREATE INDEX ug_b_idx ON ug_b0000 (v);
\set VERBOSITY terse
DROP TABLE ug;
\set VERBOSITY default
DROP TABLE ug_plain, ug;
DROP INDEX ug_pkey;
DROP SCHEMA ugs CASCADE;
DROP TYPE ug_color CASCADE;
COPY ug (id) FROM '/dev/null';
CREATE TRIGGER ug_trg BEFORE INSERT ON ug FOR EACH ROW EXECUTE FUNCTION suppress_redundant_updates_trigger();
CREATE RULE ug_rule AS ON INSERT TO ug DO ALSO NOTIFY ug;
CREATE POLICY ug_pol ON ug USING (true);
CREATE STATISTICS ug_stat ON id, v FROM ug;
CREATE TABLE ug_child () INHERITS (ug);
CREATE TABLE ug_part PARTITION OF ug FOR VALUES WITH (MODULUS 4, REMAINDER 0);
CREATE TABLE ug_part2 (id int, v text);
ALTER TABLE ug ATTACH PARTITION ug_part2 FOR VALUES WITH (MODULUS 4, REMAINDER 0);
-- A statement inside a function is refused as well.
\set VERBOSITY terse
DO $$ BEGIN EXECUTE 'TRUNCATE ug'; END $$;
\set VERBOSITY default
-- Nothing was applied (routing is switched off to count the local partitions).
SET kingres_shard.enable_routing = off;
SELECT count(*) AS rows_still_there FROM ug;
RESET kingres_shard.enable_routing;
SELECT relname FROM pg_class WHERE relname IN ('ug_renamed', 'ug_child', 'ug_part', 'ug_part2') ORDER BY 1;
SELECT count(*) AS dist_tables FROM kingres.dist_table;

-- Allowed: statements on other tables and per-node maintenance.
TRUNCATE ug_plain;
ALTER TABLE ug_plain ADD COLUMN y int;
CREATE INDEX ug_plain_y_idx ON ug_plain (y);
DROP TABLE ug_plain;
VACUUM ug;
VACUUM (ANALYZE) ug;
ANALYZE ug;
REINDEX TABLE ug;
REINDEX INDEX ug_pkey;
BEGIN;
LOCK TABLE ug IN ACCESS SHARE MODE;
ROLLBACK;
EXPLAIN (COSTS OFF) SELECT * FROM ug WHERE id = 1;
\set VERBOSITY terse
COMMENT ON TABLE ug IS 'per-node metadata';
\set VERBOSITY default

-- The per-node escape hatch for an administrator who repeats the statement on
-- every node: with routing off nothing is checked.
SET kingres_shard.enable_routing = off;
TRUNCATE ug;
ALTER TABLE ug ADD COLUMN x int;
ALTER TABLE ug DROP COLUMN x;
RESET kingres_shard.enable_routing;

-- Back to one group: DDL works again, and dropping a table forgets it.
DELETE FROM kingres.node WHERE node_id = 2;
UPDATE kingres.bucket_map SET group_id = 0;
DROP TABLE ug;
SELECT count(*) AS dist_tables_left FROM kingres.dist_table;
DROP SCHEMA ugs CASCADE;
SELECT count(*) AS dist_tables_after_schema_drop FROM kingres.dist_table;
DROP TABLE ug_c;
DROP TYPE ug_color;
DROP EXTENSION kingres_shard;
