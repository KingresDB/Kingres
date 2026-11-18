-- sql/bulk_plans.sql
--   Step 3 and step 4 of phase 2b (design/shard-phase2b.md, 11.1, 11.3, 11.6, 13 and 16.1): which
--   statements are planned as the row router, which stay on the paths of 2a, what the switches do,
--   the settings, and the privilege matrix of INSERT and COPY FROM at the origin.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- The cluster has a second shard group that does not exist as a server: the odd buckets belong to
-- it (as in routing_decisions).  Nothing here is executed against it: the plans are shown, and every
-- statement that would send a row is refused before it does (the checks of the origin).
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

CREATE TABLE t (id int PRIMARY KEY, v text, n int DEFAULT 7, g int GENERATED ALWAYS AS (n * 2) STORED);
CREATE TABLE r (k int PRIMARY KEY);
CREATE TABLE plain (x int, y text);
CREATE TABLE secret (id int, v text);
CREATE FUNCTION bp_imm(int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT $1';
SELECT kingres.create_distributed_table('t', 'id', bucket_count => 8);
SELECT kingres.create_reference_table('r');

-- the roles of the privilege matrix; grants are propagated, so they are made before the second group exists
CREATE ROLE bp_writer;
CREATE ROLE bp_cols;
CREATE ROLE bp_reader;
CREATE ROLE bp_none;
GRANT USAGE ON SCHEMA public TO bp_writer, bp_cols, bp_reader, bp_none;
GRANT INSERT, SELECT ON t TO bp_writer;
GRANT INSERT (id, v) ON t TO bp_cols;
GRANT SELECT ON t TO bp_reader;
GRANT SELECT ON plain TO bp_writer, bp_cols, bp_reader, bp_none;

INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (2, 1, '127.0.0.1', 1, 'primary');
UPDATE kingres.bucket_map SET group_id = 1 WHERE bucket % 2 = 1;

-- a key of a local bucket and of a remote bucket
SELECT min(g) AS lk FROM generate_series(1, 100) g
  WHERE kingres.bucket_for('t'::regclass, g) % 2 = 0 \gset
SELECT min(g) AS rk FROM generate_series(1, 100) g
  WHERE kingres.bucket_for('t'::regclass, g) % 2 = 1 \gset

-- ---------------------------------------------------------------------------
-- The plans
-- ---------------------------------------------------------------------------
\echo -- a multi-row VALUES, INSERT ... SELECT and a computed key are the row router
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b');
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) SELECT x, y FROM plain;
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) SELECT g, 'x' FROM generate_series(1, 3) g;
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES ((random() * 0)::int + 1, 'x');
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b') ON CONFLICT DO NOTHING;
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b') ON CONFLICT (id) DO NOTHING RETURNING id, v;
\echo -- a single row with a constant key is not routed: the remote key is forwarded, the local one is local
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (:rk, 'a');
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (:lk, 'a');
\echo -- a statement that touches no distributed table is not changed
EXPLAIN (COSTS OFF) INSERT INTO plain VALUES (1, 'a'), (2, 'b');
\echo -- a write to a reference table is the fan-out of 2a
EXPLAIN (COSTS OFF) INSERT INTO r VALUES (1), (2);

\echo -- what the router does not take, each with its reason
INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b') ON CONFLICT (id) DO UPDATE SET v = excluded.v;
WITH x AS (SELECT 1 AS id) INSERT INTO t (id, v) SELECT id, 'a' FROM x;
INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b') RETURNING id, (SELECT 1);
INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b') RETURNING old.id, new.id;
INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b') ON CONFLICT (id) WHERE bp_imm(id) > 0 DO NOTHING;
INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b') ON CONFLICT (id) WHERE id > 0 DO NOTHING RETURNING bp_imm(id);
INSERT INTO t (id, g) VALUES (1, 1), (2, 2);

\echo -- the switches: with enable_bulk_routing off the statements are refused as in 2a
SET kingres_shard.enable_bulk_routing = off;
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b');
INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b');
INSERT INTO t (id, v) SELECT g, 'x' FROM generate_series(1, 3) g;
INSERT INTO t (id, v) VALUES ((random() * 0)::int + 1, 'x');
-- statements that were never refused are not affected by the switch
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (:rk, 'a');
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (:lk, 'a');
RESET kingres_shard.enable_bulk_routing;

-- ---------------------------------------------------------------------------
-- The settings
-- ---------------------------------------------------------------------------
SELECT name, setting, unit, min_val, max_val, context
  FROM pg_settings
 WHERE name IN ('kingres_shard.enable_bulk_routing', 'kingres_shard.enable_copy_routing',
                'kingres_shard.insert_batch_rows', 'kingres_shard.insert_batch_bytes')
 ORDER BY name;
SET kingres_shard.insert_batch_rows = 0;
SET kingres_shard.insert_batch_rows = 100001;
SET kingres_shard.insert_batch_bytes = '8kB';
SET kingres_shard.insert_batch_bytes = '65MB';
SET kingres_shard.insert_batch_rows = 100000;
SET kingres_shard.insert_batch_bytes = '16kB';
RESET kingres_shard.insert_batch_rows;
RESET kingres_shard.insert_batch_bytes;

-- ---------------------------------------------------------------------------
-- COPY FROM: what is refused (before the data is read) and why
-- ---------------------------------------------------------------------------
COPY t (id, v) FROM STDIN (FREEZE);
1	a
\.
COPY t (id, v) FROM STDIN WHERE id > 1;
1	a
\.
COPY t (id, v) FROM STDIN (ON_ERROR ignore);
1	a
\.
COPY t (id, v) FROM STDIN (ON_ERROR stop);
1	a
\.
COPY t (id, v) FROM STDIN (REJECT_LIMIT 5, ON_ERROR ignore);
1	a
\.
COPY t (id, v) FROM STDIN (LOG_VERBOSITY verbose);
1	a
\.
COPY t FROM STDIN (FORMAT csv, QUOTE 'ab');
\.
COPY t (id, g) FROM STDIN;
\.
COPY r FROM STDIN;
1
\.
COPY t_b0001 FROM STDIN;
1	a
\.
SET kingres_shard.enable_copy_routing = off;
COPY t (id, v) FROM STDIN;
1	a
\.
RESET kingres_shard.enable_copy_routing;
\echo -- COPY TO is not changed
COPY (SELECT 1) TO STDOUT;

-- ---------------------------------------------------------------------------
-- The privilege matrix: the executor checks INSERT (and the columns) at the origin before the
-- router sends anything, for INSERT and for COPY FROM
-- ---------------------------------------------------------------------------
\echo -- INSERT
SET ROLE bp_writer;
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b');
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) SELECT x, y FROM plain;
RESET ROLE;
SET ROLE bp_cols;
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b');
EXPLAIN (COSTS OFF) INSERT INTO t (id, v, n) VALUES (1, 'a', 1), (2, 'b', 2);
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b') RETURNING id;
RESET ROLE;
SET ROLE bp_reader;
EXPLAIN (COSTS OFF) INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b');
INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b');
INSERT INTO t (id, v) SELECT x, y FROM plain;
RESET ROLE;
SET ROLE bp_none;
INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b');
INSERT INTO t (id, v) SELECT x, y FROM plain;
RESET ROLE;
-- a reader of the source who may not read it
SET ROLE bp_writer;
INSERT INTO t (id, v) SELECT id, v FROM secret;
RESET ROLE;

\echo -- COPY FROM
SET ROLE bp_reader;
COPY t (id, v) FROM STDIN;
1	a
\.
RESET ROLE;
SET ROLE bp_none;
COPY t (id, v) FROM STDIN;
1	a
\.
RESET ROLE;
SET ROLE bp_cols;
COPY t (id, v, n) FROM STDIN;
1	a	1
\.
RESET ROLE;
SET ROLE bp_writer;
COPY t (id, v) FROM '/nonexistent/file';
COPY t (id, v) FROM PROGRAM 'true';
RESET ROLE;
-- read-only transactions
BEGIN READ ONLY;
INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b');
ROLLBACK;
BEGIN READ ONLY;
COPY t (id, v) FROM STDIN;
1	a
\.
ROLLBACK;

-- ---------------------------------------------------------------------------
-- Clean up
-- ---------------------------------------------------------------------------
-- nothing that was refused left a row in the partitions of this node
SET kingres_shard.enable_routing = off;
SELECT count(*) AS rows_left FROM t;
RESET kingres_shard.enable_routing;

UPDATE kingres.bucket_map SET group_id = 0;
DELETE FROM kingres.node WHERE node_id = 2;
DROP TABLE t, r, plain, secret;
DROP FUNCTION bp_imm(int);
DROP OWNED BY bp_writer, bp_cols, bp_reader, bp_none;
DROP ROLE bp_writer, bp_cols, bp_reader, bp_none;
DROP EXTENSION kingres_shard;
