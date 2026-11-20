-- sql/pushdown_plans.sql
--   Step 5 of phase 2b (design/shard-phase2b.md, 6, 9.1, 11.7, 12, 13 and 16.1): the P6 part of
--   pushdown_plans.  Which INSERT ... SELECT statements are shipped whole (the plan is the
--   fan-out write with its "Pushed" line), which are not and why (the DEBUG1 message of the
--   planner), what the switch does, and what a statement outside the path gets (the row router).
--   Later steps add the plans of the other paths to this file.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- The cluster has a second shard group that does not exist as a server: the odd buckets belong to
-- it (as in bulk_plans).  Nothing here is executed against it: the plans are shown.
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

-- The plan of a statement, one line per row, with the placement number (a hash of the map) hidden.
CREATE FUNCTION plan_of(q text) RETURNS SETOF text LANGUAGE plpgsql AS $$
DECLARE l text;
BEGIN
  FOR l IN EXECUTE 'EXPLAIN (COSTS OFF) ' || q LOOP
    RETURN NEXT regexp_replace(l, 'place=-?[0-9]+', 'place=N');
  END LOOP;
END $$;

-- Is the statement shipped whole, or planned as something else?
CREATE FUNCTION verdict(q text) RETURNS text LANGUAGE plpgsql AS $$
DECLARE l text; r text := 'other';
BEGIN
  FOR l IN EXECUTE 'EXPLAIN (COSTS OFF) ' || q LOOP
    IF l LIKE '%Kingres Fan-out Write%' THEN r := 'shipped'; END IF;
    IF l LIKE '%Kingres Row Router%' THEN r := 'router'; END IF;
  END LOOP;
  RETURN r;
END $$;

-- ---------------------------------------------------------------------------
-- The plans of shipped statements (12)
-- ---------------------------------------------------------------------------
\echo -- a copy, a join with a colocated table and with a reference table, a left join
SELECT * FROM plan_of('INSERT INTO d (id, v) SELECT id, v FROM s');
SELECT * FROM plan_of('INSERT INTO d (id, v) SELECT s.id, o.w FROM s JOIN o ON o.id = s.id');
SELECT * FROM plan_of('INSERT INTO d (id, v) SELECT s.id, r.label FROM s JOIN r ON r.k = s.g');
SELECT * FROM plan_of('INSERT INTO d (id, v) SELECT s.id, o.w FROM s LEFT JOIN o ON o.id = s.id');
\echo -- grouping by the key, DISTINCT with the key, ON CONFLICT, RETURNING
SELECT * FROM plan_of('INSERT INTO d (id, n) SELECT id, count(*) FROM s GROUP BY id');
SELECT * FROM plan_of('INSERT INTO d (id, v) SELECT DISTINCT id, v FROM s');
SELECT * FROM plan_of('INSERT INTO d (id, v) SELECT id, v FROM s ON CONFLICT DO NOTHING');
SELECT * FROM plan_of('INSERT INTO d (id, v, n) SELECT id, v, n FROM s ON CONFLICT (id) DO UPDATE SET n = d.n + excluded.n WHERE d.n < 5');
SELECT * FROM plan_of('INSERT INTO d (id, v) SELECT id, v FROM s RETURNING id, n');

-- ---------------------------------------------------------------------------
-- Shipped or not (verdict): every rule of 6.1, 6.3, 6.4, 9.1 and 11.7 with its statement
-- ---------------------------------------------------------------------------
\echo -- shipped
SELECT verdict('INSERT INTO d (id, v) SELECT id, v FROM s') AS copy,
       verdict('INSERT INTO d (id, v) SELECT s.id, o.w FROM s, o WHERE o.id = s.id') AS where_equality,
       verdict('INSERT INTO d (id, v) SELECT s.id, o.w FROM s JOIN o USING (id)') AS using_join,
       verdict('INSERT INTO d (id, v) SELECT s.id, t.v FROM s JOIN s t ON t.id = s.id') AS self_join,
       verdict('INSERT INTO d (v, id) SELECT v, id FROM s') AS other_order,
       verdict('INSERT INTO d (id, v) SELECT id, ''lit'' FROM s') AS literal,
       verdict('INSERT INTO d (id) SELECT id FROM s ON CONFLICT (id) DO UPDATE SET v = ''x''') AS do_update,
       verdict('INSERT INTO d (id) SELECT s.id FROM s LEFT JOIN o ON o.id = s.id') AS left_join,
       verdict('INSERT INTO d (id) SELECT s.id FROM o RIGHT JOIN s ON o.id = s.id') AS right_join_preserved_key,
       verdict('INSERT INTO d (id) SELECT o.id FROM o RIGHT JOIN s ON o.id = s.id') AS right_join_nullable_key;
\echo -- not shipped (the row router takes them)
SELECT verdict('INSERT INTO d (id, v) SELECT id + 1, v FROM s') AS key_computed,
       verdict('INSERT INTO d (id, v) SELECT n, v FROM s') AS key_not_a_key,
       verdict('INSERT INTO d (id, v) SELECT s.id, o.w FROM s JOIN o ON o.id = s.n') AS join_on_other_column,
       verdict('INSERT INTO d (id, v) SELECT s.id, a.w FROM s JOIN away a ON a.id = s.id') AS other_colocation_group,
       verdict('INSERT INTO d (id, v) SELECT count(*), ''x'' FROM s') AS aggregate_without_group_by,
       verdict('INSERT INTO d (id, v) SELECT n, ''x'' FROM s GROUP BY n') AS group_by_not_the_key,
       verdict('INSERT INTO d (id, v) SELECT id, v FROM s LIMIT 5') AS limit_,
       verdict('INSERT INTO d (id, v) SELECT id, v FROM s WHERE id IN (SELECT id FROM o)') AS sub_select,
       verdict('INSERT INTO d (id, v) SELECT id, v FROM (SELECT id, v FROM s) q') AS from_sub_select,
       verdict('INSERT INTO d (id, v) SELECT k, label FROM r') AS only_a_reference_table,
       verdict('INSERT INTO d (id, v) SELECT o.id, o.w FROM r LEFT JOIN o ON o.id = r.k') AS preserved_side_replicated,
       verdict('INSERT INTO d (id, v) SELECT s.id, o.w FROM s FULL JOIN o ON o.id = s.id') AS full_join_key_nullable,
       verdict('INSERT INTO d (id, v) SELECT id, v FROM s UNION ALL SELECT id, w FROM o') AS set_operation,
       verdict('INSERT INTO d (id, v) SELECT x, y FROM plain') AS local_source,
       verdict('INSERT INTO d (id, v) VALUES (1, ''a''), (2, ''b'')') AS values_list,
       verdict('INSERT INTO ser (id, a) SELECT id, v FROM s') AS serial_default,
       verdict('INSERT INTO d (id, v) SELECT id, v || random()::text FROM s') AS volatile_function;

-- ---------------------------------------------------------------------------
-- The switch (13): off, the statement goes to the router, and DO UPDATE is refused
-- ---------------------------------------------------------------------------
SET kingres_shard.enable_colocated_insert_select = off;
SELECT verdict('INSERT INTO d (id, v) SELECT id, v FROM s') AS copy_off;
SELECT * FROM plan_of('INSERT INTO d (id, v) SELECT id, v FROM s');
INSERT INTO d (id, v) SELECT id, v FROM s ON CONFLICT (id) DO UPDATE SET v = excluded.v;
SET kingres_shard.enable_bulk_routing = off;
INSERT INTO d (id, v) SELECT id, v FROM s;
RESET kingres_shard.enable_bulk_routing;
RESET kingres_shard.enable_colocated_insert_select;
SELECT verdict('INSERT INTO d (id, v) SELECT id, v FROM s') AS copy_on_again;
-- with the router off and the path on, the statement is still shipped
SET kingres_shard.enable_bulk_routing = off;
SELECT verdict('INSERT INTO d (id, v) SELECT id, v FROM s') AS copy_without_the_router;
RESET kingres_shard.enable_bulk_routing;

-- ---------------------------------------------------------------------------
-- The reasons (DEBUG1 of the planner): the first rule that failed
-- ---------------------------------------------------------------------------
SET client_min_messages = debug1;
\set VERBOSITY terse
EXPLAIN (COSTS OFF) INSERT INTO d (id, v) SELECT id + 1, v FROM s;
EXPLAIN (COSTS OFF) INSERT INTO d (id, v) SELECT s.id, o.w FROM s JOIN o ON o.id = s.n;
EXPLAIN (COSTS OFF) INSERT INTO d (id, v) SELECT s.id, a.w FROM s JOIN away a ON a.id = s.id;
EXPLAIN (COSTS OFF) INSERT INTO d (id, v) SELECT n, 'x' FROM s GROUP BY n;
EXPLAIN (COSTS OFF) INSERT INTO d (id, v) SELECT id, v FROM s LIMIT 5;
EXPLAIN (COSTS OFF) INSERT INTO d (id, v) SELECT id, v FROM s WHERE id IN (SELECT id FROM o);
EXPLAIN (COSTS OFF) INSERT INTO d (id, v) SELECT x, y FROM plain;
EXPLAIN (COSTS OFF) INSERT INTO ser (id, a) SELECT id, v FROM s;
EXPLAIN (COSTS OFF) INSERT INTO d (id, v) SELECT id, v FROM s ON CONFLICT (id) DO UPDATE SET id = excluded.id;
RESET client_min_messages;
\set VERBOSITY default

-- ---------------------------------------------------------------------------
-- The settings
-- ---------------------------------------------------------------------------
SELECT name, setting, context FROM pg_settings WHERE name = 'kingres_shard.enable_colocated_insert_select';

-- ---------------------------------------------------------------------------
-- Clean up
-- ---------------------------------------------------------------------------
UPDATE kingres.bucket_map SET group_id = 0;
DELETE FROM kingres.node WHERE node_id = 2;
DROP TABLE s, d, o, away, r, plain, ser;
DROP FUNCTION plan_of(text);
DROP FUNCTION verdict(text);
DROP EXTENSION kingres_shard;
