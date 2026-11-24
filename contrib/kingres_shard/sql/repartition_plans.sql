-- sql/repartition_plans.sql
--   Phase 12 (design/analytics-phase12-queries.md, 4.3, 4.7, 4.12 and 14.1): the plans of the repartitioned join.  Which statements
--   get the Kingres Fragment Scan with "Pushed: repartitioned join" (the rows of the join come back, or a partial aggregation above
--   it), which role each table has, which statements do not and why (the DEBUG1 message of the planner), and what the switches do.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- The cluster has a second shard group that does not exist as a server: the odd buckets belong to it.  Nothing here is executed
-- against it: the plans are shown.
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

CREATE TABLE o (id int PRIMARY KEY, cust int, amount int, note text);
CREATE TABLE cu (id int PRIMARY KEY, region int, name text);
CREATE TABLE it (id int PRIMARY KEY, ord int, sku int);
CREATE TABLE far (id int PRIMARY KEY, w int);
CREATE TABLE r (k int PRIMARY KEY, label text);
CREATE TABLE plain (x int, y text);
SELECT kingres.create_distributed_table('o', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('cu', 'id', colocate_with => 'o');
SELECT kingres.create_distributed_table('it', 'id', colocate_with => 'o');
SELECT kingres.create_distributed_table('far', 'id', bucket_count => 4);
SELECT kingres.create_reference_table('r');

INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (2, 1, '127.0.0.1', 1, 'primary');
UPDATE kingres.bucket_map SET group_id = 1 WHERE bucket % 2 = 1;

-- The plan of a statement, one line per row, with the placement number (a hash of the map) hidden.
CREATE FUNCTION plan_of(q text) RETURNS SETOF text LANGUAGE plpgsql AS $$
DECLARE l text;
BEGIN
  FOR l IN EXECUTE 'EXPLAIN (COSTS OFF) ' || q LOOP
    RETURN NEXT regexp_replace(l, 'place=-?[0-9]+', 'place=N');
  END LOOP;
END $$;

-- What is pushed: the "Pushed" line of the plan, or "none"
CREATE FUNCTION pushed(q text) RETURNS text LANGUAGE plpgsql AS $$
DECLARE l text; r text := 'none';
BEGIN
  FOR l IN EXECUTE 'EXPLAIN (COSTS OFF) ' || q LOOP
    IF l LIKE '%Pushed:%' THEN r := btrim(l); END IF;
  END LOOP;
  RETURN r;
END $$;


-- The reason line of EXPLAIN (VERBOSE): "Repartition: not used: ...", or "-"
CREATE FUNCTION why(q text) RETURNS text LANGUAGE plpgsql AS $$
DECLARE l text; r text := '-';
BEGIN
  FOR l IN EXECUTE 'EXPLAIN (COSTS OFF, VERBOSE) ' || q LOOP
    IF l LIKE '%Repartition: not used:%' THEN r := btrim(l); END IF;
  END LOOP;
  RETURN r;
END $$;

SET jit = off;
SET kingres_shard.enable_repartition_join = force;
SET kingres_shard.enable_partial_aggregation = on;

-- ---------------------------------------------------------------------------
-- The role of each table
-- ---------------------------------------------------------------------------
\echo -- o is moved to the groups of cu (the other way round, the same); a filter on each side
SELECT * FROM plan_of('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust');
SELECT * FROM plan_of('SELECT cu.id, o.id FROM cu JOIN o ON o.cust = cu.id WHERE cu.region = 3 AND o.amount > 5');
\echo -- the join condition in WHERE; a reference table beside; three tables, one of them moved
SELECT pushed('SELECT o.id, cu.name FROM o, cu WHERE cu.id = o.cust');
SELECT pushed('SELECT o.id, r.label FROM o JOIN cu ON cu.id = o.cust JOIN r ON r.k = cu.region');
SELECT pushed('SELECT o.id, cu.name, it.sku FROM o JOIN cu ON cu.id = o.cust JOIN it ON it.id = cu.id');
\echo -- the table that is moved is of another colocation group: its rows go by the layout of the group that stays
SELECT pushed('SELECT far.id, o.id FROM far JOIN o ON o.cust = far.id');
SELECT pushed('SELECT o.id, far.w FROM o JOIN far ON far.id = o.cust');
\echo -- a LEFT JOIN to the moved table
SELECT * FROM plan_of('SELECT cu.id, o.id FROM cu LEFT JOIN o ON o.cust = cu.id');

\echo -- a table with no column to be routed by is sent whole to every group (broadcast): an expression, an inequality, columns that are not keys
SELECT * FROM plan_of('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust + 1');
SELECT pushed('SELECT o.id, cu.id FROM o JOIN cu ON cu.region < o.amount');
SELECT pushed('SELECT o.id, it.id FROM o JOIN it ON it.sku = o.amount');
\echo -- a LEFT JOIN: only the nullable table can be sent whole; a table that can be routed is routed
SELECT * FROM plan_of('SELECT o.id, cu.name FROM o LEFT JOIN cu ON cu.id = o.cust');
SELECT pushed('SELECT o.id, cu.name FROM o LEFT JOIN cu ON cu.id = o.cust + 1 AND o.amount > 3');
\echo -- three tables, two aligned, one sent whole; a reference table beside
SELECT pushed('SELECT o.id, it.sku, cu.name FROM o JOIN it ON it.id = o.id JOIN cu ON cu.id = o.cust + 1');
SELECT pushed('SELECT o.id, r.label, cu.name FROM o JOIN cu ON cu.id = o.cust + 1 JOIN r ON r.k = cu.region');
\echo -- an aggregate above a broadcast join
SELECT * FROM plan_of('SELECT cu.region, count(*), sum(o.amount) FROM o JOIN cu ON cu.id = o.cust + 1 GROUP BY cu.region');


-- ---------------------------------------------------------------------------
-- Aggregates above the join: every group aggregates its rows of the join, this node combines
-- ---------------------------------------------------------------------------
\echo -- a grouped aggregate, an aggregate without grouping, HAVING, ORDER BY with LIMIT
SELECT * FROM plan_of('SELECT cu.region, count(*), sum(o.amount) FROM o JOIN cu ON cu.id = o.cust GROUP BY cu.region');
SELECT pushed('SELECT count(*), max(cu.id) FROM o JOIN cu ON cu.id = o.cust');
SELECT pushed('SELECT cu.region, count(*) FROM o JOIN cu ON cu.id = o.cust GROUP BY cu.region HAVING count(*) > 10');
SELECT pushed('SELECT o.note, sum(o.amount) FROM o JOIN cu ON cu.id = o.cust GROUP BY o.note ORDER BY 2 DESC LIMIT 3');
\echo -- an aggregate that is not split stays above the rows of the join
SELECT pushed('SELECT cu.region, count(DISTINCT o.note) FROM o JOIN cu ON cu.id = o.cust GROUP BY cu.region');
SET kingres_shard.enable_partial_aggregation = off;
SELECT pushed('SELECT cu.region, count(*) FROM o JOIN cu ON cu.id = o.cust GROUP BY cu.region');
SET kingres_shard.enable_partial_aggregation = on;

-- ---------------------------------------------------------------------------
-- The switches
-- ---------------------------------------------------------------------------
SET kingres_shard.enable_repartition_join = off;
SELECT pushed('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust');
SET kingres_shard.enable_repartition_join = on;
SELECT pushed('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust');
SET kingres_shard.repartition_max_bytes = 1024;
SELECT pushed('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust');
RESET kingres_shard.repartition_max_bytes;
SET jit = off;
SET kingres_shard.enable_repartition_join = force;
SET kingres_shard.enable_pushdown = off;
SELECT pushed('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust');
RESET kingres_shard.enable_pushdown;

-- ---------------------------------------------------------------------------
-- The statements that are not repartitioned, and why (DEBUG1 of the planner): the complete scan answers them
-- ---------------------------------------------------------------------------
SET client_min_messages = debug1;
\echo -- an outer join other than one LEFT JOIN to a table
SELECT pushed('SELECT o.id, cu.name FROM o FULL JOIN cu ON cu.id = o.cust');
SELECT pushed('SELECT cu.id, o.id FROM cu LEFT JOIN o ON o.cust = cu.id LEFT JOIN it ON it.id = cu.id');
\echo -- the tables that stay are not aligned with each other: a broadcast needs them aligned
SELECT pushed('SELECT o.id, it.id, cu.id FROM o CROSS JOIN it LEFT JOIN cu ON cu.id = o.cust');
SELECT pushed('SELECT o.id, it.id, cu.id FROM o, it, cu');
\echo -- the ON clause of a LEFT JOIN does not align the tables that stay with each other
SELECT pushed('SELECT o.id, it.id, cu.id FROM o CROSS JOIN it LEFT JOIN cu ON cu.region = o.id AND cu.region = it.id');
SELECT pushed('SELECT o.id, it.id, cu.id FROM o CROSS JOIN it LEFT JOIN cu ON o.id = it.id AND cu.region = o.id');
\echo -- a table that is not distributed
SELECT pushed('SELECT o.id, plain.y FROM o JOIN plain ON plain.x = o.cust');
RESET client_min_messages;
\echo -- a sub-select in the statement
SELECT pushed('SELECT o.id FROM o JOIN cu ON cu.id = o.cust WHERE o.amount > (SELECT avg(amount) FROM o)');

-- ---------------------------------------------------------------------------
-- EXPLAIN (VERBOSE) names the reason when a statement that was a candidate is not repartitioned (design 4.12)
-- ---------------------------------------------------------------------------
\echo -- the statements of the section above, one reason each ("-" when the plan has no such line)
SELECT why('SELECT o.id, cu.name FROM o FULL JOIN cu ON cu.id = o.cust');
SELECT why('SELECT cu.id, o.id FROM cu LEFT JOIN o ON o.cust = cu.id LEFT JOIN it ON it.id = cu.id');
\echo -- a statement planned after one that had a reason does not inherit it (a local table beside a distributed one, on the other group)
SELECT id, why('SELECT o.id, plain.y FROM o JOIN plain ON plain.x = o.id WHERE o.id = ' || id) FROM generate_series(1, 6) id;
SELECT why('SELECT o.id, it.id, cu.id FROM o CROSS JOIN it LEFT JOIN cu ON cu.id = o.cust');
SELECT why('SELECT o.id, it.id, cu.id FROM o, it, cu');
SELECT why('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust + 1');
SELECT why('SELECT o.id, plain.y FROM o JOIN plain ON plain.x = o.cust');
\echo -- the setting is off; a small bound, where the scan is cheaper by cost
SET kingres_shard.enable_repartition_join = off;
SELECT why('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust');
SET kingres_shard.enable_repartition_join = on;
SET kingres_shard.repartition_max_bytes = 1024;
SELECT why('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust');
RESET kingres_shard.repartition_max_bytes;
SET kingres_shard.enable_repartition_join = force;
\echo -- not a candidate (one distributed table), or repartitioned: no line
SELECT why('SELECT o.id FROM o WHERE o.amount > 3');
SELECT why('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust');
\echo -- the used form (no VERBOSE) names the moved table, its key and the placement
SELECT * FROM plan_of('SELECT o.id, cu.name FROM o JOIN cu ON cu.id = o.cust') LIMIT 3;

-- ---------------------------------------------------------------------------
-- Clean up
-- ---------------------------------------------------------------------------
RESET jit;
RESET kingres_shard.enable_repartition_join;
RESET kingres_shard.enable_partial_aggregation;
UPDATE kingres.bucket_map SET group_id = 0;
DELETE FROM kingres.node WHERE node_id = 2;
DROP TABLE o, cu, it, far, r, plain;
DROP FUNCTION plan_of(text);
DROP FUNCTION pushed(text);
DROP FUNCTION why(text);
DROP EXTENSION kingres_shard;
