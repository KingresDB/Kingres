-- sql/merge_plans.sql
--   Phase 12, MERGE on distributed tables (design/analytics-phase12-queries.md, section 7):
--   which MERGE statements are decided to run on the shard groups, which are refused and with
--   what message (the message names the condition that is missing), what the switch does.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- The cluster has a second shard group that does not exist as a server: the odd buckets belong
-- to it (as in pushdown_plans).  Nothing here is executed against it: the plans are shown.
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

CREATE TABLE s (id int PRIMARY KEY, v text, n int, g int);
CREATE TABLE d (id int PRIMARY KEY, v text, n int DEFAULT 7);
CREATE TABLE o (id int PRIMARY KEY, w text);
CREATE TABLE away (id int PRIMARY KEY, w text);
CREATE TABLE r (k int PRIMARY KEY, label text);
CREATE TABLE plain (x int, y text);
CREATE TABLE ser (id int PRIMARY KEY, a text, q serial);
SELECT kingres.create_distributed_table('s', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('d', 'id', colocate_with => 's');
SELECT kingres.create_distributed_table('o', 'id', colocate_with => 's');
SELECT kingres.create_distributed_table('away', 'id', bucket_count => 4);
SELECT kingres.create_distributed_table('ser', 'id', colocate_with => 's');
SELECT kingres.create_reference_table('r');

INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (2, 1, '127.0.0.1', 1, 'primary');
UPDATE kingres.bucket_map SET group_id = 1 WHERE bucket % 2 = 1;
UPDATE kingres.bucket_map SET group_id = 1 WHERE colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'away'::regclass) AND bucket % 2 = 0;

-- What the planner does with a statement: the line of its plan that names the path, or the error.
CREATE FUNCTION merge_verdict(q text) RETURNS text LANGUAGE plpgsql AS $$
DECLARE l text; r text := 'other'; dt text;
BEGIN
  FOR l IN EXECUTE 'EXPLAIN (COSTS OFF) ' || q LOOP
    IF l ~ 'Kingres|Pushed|Remote SQL|Nodes' THEN
      r := CASE WHEN r = 'other' THEN '' ELSE r || ' ## ' END
           || regexp_replace(regexp_replace(l, '^ *', ''), 'place=-?[0-9]+', 'place=N');
    END IF;
  END LOOP;
  RETURN r;
EXCEPTION WHEN OTHERS THEN
  GET STACKED DIAGNOSTICS dt = PG_EXCEPTION_DETAIL;
  RETURN 'ERROR: ' || SQLERRM || coalesce(' | ' || nullif(dt, ''), '');
END $$;

-- ---------------------------------------------------------------------------
-- With the switch off the statement meets the refusals of phase 2a.
-- ---------------------------------------------------------------------------
SET kingres_shard.enable_distributed_merge = off;
SELECT merge_verdict('MERGE INTO d USING s ON d.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v');
RESET kingres_shard.enable_distributed_merge;
SHOW kingres_shard.enable_distributed_merge;
SHOW kingres_shard.merge_max_source_bytes;

-- ---------------------------------------------------------------------------
-- The refusals of 7.2: each names the missing condition, before any group runs anything.
-- ---------------------------------------------------------------------------
-- a WITH clause
SELECT merge_verdict('WITH c AS (SELECT 1 AS x) MERGE INTO d USING s ON d.id = s.id WHEN MATCHED THEN DELETE');
-- the distribution column is updated
SELECT merge_verdict('MERGE INTO d USING s ON d.id = s.id WHEN MATCHED THEN UPDATE SET id = s.id + 1');
-- the ON clause does not equate the key with something of the source
SELECT merge_verdict('MERGE INTO d USING plain p ON d.n = p.x WHEN MATCHED THEN DELETE');
SELECT merge_verdict('MERGE INTO d USING (SELECT x FROM plain) p ON d.id > p.x WHEN MATCHED THEN DELETE');
-- an INSERT action whose key is not the key the statement is routed by
SELECT merge_verdict('MERGE INTO d USING s ON d.id = s.id WHEN NOT MATCHED THEN INSERT (id, v) VALUES (s.id + 1, s.v)');
SELECT merge_verdict('MERGE INTO d USING plain p ON d.id = p.x WHEN NOT MATCHED THEN INSERT (id, v) VALUES (p.x + 1, p.y)');
-- no value for the key in the INSERT list
SELECT merge_verdict('MERGE INTO d USING plain p ON d.id = p.x WHEN NOT MATCHED THEN INSERT (v) VALUES (p.y)');
-- a volatile value: each group would compute its own
SELECT merge_verdict('MERGE INTO d USING s ON d.id = s.id WHEN MATCHED THEN UPDATE SET v = random()::text');
SELECT merge_verdict('MERGE INTO ser USING plain p ON ser.id = p.x WHEN NOT MATCHED THEN INSERT (id, a) VALUES (p.x, p.y)');
-- a sub-select in an expression
SELECT merge_verdict('MERGE INTO d USING s ON d.id = s.id WHEN MATCHED AND d.n > (SELECT max(x) FROM plain) THEN DELETE');
-- an expression that is not built in and immutable
SELECT merge_verdict('MERGE INTO d USING plain p ON d.id = p.x WHEN MATCHED THEN UPDATE SET n = pg_backend_pid()');
-- USING is a join: the source is not one relation
SELECT merge_verdict('MERGE INTO d USING plain p JOIN plain q ON p.x = q.x ON d.id = p.x WHEN MATCHED THEN DELETE');
-- USING is the target itself, joined on something other than the key
SELECT merge_verdict('MERGE INTO d USING d dd ON d.n = dd.n WHEN MATCHED THEN DELETE');

-- ---------------------------------------------------------------------------
-- M1, colocated (7.3): the statement runs whole on every group of the target.
-- ---------------------------------------------------------------------------
SELECT merge_verdict('MERGE INTO d USING s ON d.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v');
SELECT merge_verdict('MERGE INTO d USING s ON d.id = s.id WHEN MATCHED AND s.n > 3 THEN DELETE WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (s.id, s.v, s.n)');
SELECT merge_verdict('MERGE INTO d USING s JOIN o ON s.id = o.id ON d.id = s.id WHEN MATCHED THEN UPDATE SET v = o.w');
-- only reference tables in USING, and an action on the target rows that no source row matches
SELECT merge_verdict('MERGE INTO d USING r ON d.n = r.k WHEN MATCHED THEN UPDATE SET v = r.label WHEN NOT MATCHED BY SOURCE THEN DELETE');
SELECT merge_verdict('MERGE INTO d USING s ON d.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v RETURNING merge_action(), d.id');

-- ---------------------------------------------------------------------------
-- M3, routed source (7.4): the source is read here and each row goes to the group of its key.
-- ---------------------------------------------------------------------------
SELECT merge_verdict('MERGE INTO d USING plain p ON d.id = p.x WHEN NOT MATCHED THEN INSERT (id, v) VALUES (p.x, p.y)');
SELECT merge_verdict('MERGE INTO d USING (VALUES (1, ''a''), (2, ''b'')) AS v(k, t) ON d.id = v.k WHEN MATCHED THEN UPDATE SET v = v.t WHEN NOT MATCHED THEN INSERT (id, v) VALUES (v.k, v.t)');
-- a table of another colocation group, a distributed table joined on a column other than its key,
-- a reference table that must give rows to a NOT MATCHED action
SELECT merge_verdict('MERGE INTO d USING away ON d.id = away.id WHEN MATCHED THEN DELETE');
SELECT merge_verdict('MERGE INTO d USING s ON d.id = s.g WHEN MATCHED THEN DELETE');
SELECT merge_verdict('MERGE INTO d USING r ON d.id = r.k WHEN NOT MATCHED THEN INSERT (id, v) VALUES (r.k, r.label)');
-- an aggregate as the source, with an action that no group may skip
SELECT merge_verdict('MERGE INTO d USING (SELECT g, count(*) AS c FROM s GROUP BY g) q ON d.id = q.g WHEN MATCHED THEN UPDATE SET n = q.c WHEN NOT MATCHED BY SOURCE THEN DELETE');
-- the key expression is an expression of the source
SELECT merge_verdict('MERGE INTO d USING plain p ON d.id = p.x + 1 WHEN MATCHED THEN DELETE');
-- with a bound parameter
PREPARE mp(int) AS MERGE INTO d USING plain p ON d.id = p.x WHEN MATCHED AND p.x > $1 THEN DELETE;
SELECT merge_verdict('EXECUTE mp(3)');
DEALLOCATE mp;
-- the switch of the setting
SET kingres_shard.enable_distributed_merge = off;
SELECT merge_verdict('MERGE INTO d USING plain p ON d.id = p.x WHEN MATCHED THEN DELETE');
RESET kingres_shard.enable_distributed_merge;

-- ---------------------------------------------------------------------------
-- Clean up
-- ---------------------------------------------------------------------------
UPDATE kingres.bucket_map SET group_id = 0;
DELETE FROM kingres.node WHERE node_id = 2;
DROP TABLE s, d, o, away, r, plain, ser;
DROP FUNCTION merge_verdict(text);
DROP EXTENSION kingres_shard;
