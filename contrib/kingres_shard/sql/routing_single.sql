-- Full SQL on distributed tables of a one-node cluster.  With a single shard
-- group every bucket is local: statements must run unchanged on the
-- partitions PostgreSQL itself keeps, with no routing involved.
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

CREATE TABLE customers (id int PRIMARY KEY, name text NOT NULL, region text);
CREATE TABLE orders (id int, customer_id int, amount numeric(10,2) NOT NULL DEFAULT 0,
                     PRIMARY KEY (customer_id, id));
SELECT kingres.create_distributed_table('customers', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('orders', 'customer_id', colocate_with => 'customers');

-- A helper to look at plans without depending on costs.
CREATE FUNCTION plan_text(q text) RETURNS text LANGUAGE plpgsql AS $$
DECLARE r text; res text := '';
BEGIN
    FOR r IN EXECUTE 'EXPLAIN (COSTS OFF) ' || q LOOP res := res || r || E'\n'; END LOOP;
    RETURN res;
END $$;

INSERT INTO customers
  SELECT g, 'customer ' || g, CASE WHEN g % 3 = 0 THEN 'north' WHEN g % 3 = 1 THEN 'south' ELSE NULL END
  FROM generate_series(1, 20) g;
INSERT INTO orders
  SELECT g * 10 + o, g, g * 1.5 + o FROM generate_series(1, 20) g, generate_series(1, 3) o;
SELECT count(*) AS customers FROM customers;
SELECT count(*) AS orders FROM orders;

-- Selects, joins and aggregates.
SELECT * FROM customers WHERE id = 5;
SELECT * FROM customers WHERE id IN (1, 2, 3) ORDER BY id;
SELECT c.id, c.name, count(o.id) AS n_orders, sum(o.amount) AS total
  FROM customers c JOIN orders o ON o.customer_id = c.id
  WHERE c.id <= 4 GROUP BY c.id, c.name ORDER BY c.id;
SELECT region, count(*) FROM customers GROUP BY region ORDER BY region NULLS LAST;
SELECT c.id FROM customers c WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.amount > 30)
  ORDER BY c.id LIMIT 5;
WITH big AS (SELECT customer_id, sum(amount) AS s FROM orders GROUP BY customer_id)
  SELECT customer_id, s FROM big WHERE s > 90 ORDER BY customer_id;

-- Writes.
INSERT INTO customers VALUES (100, 'new', 'east') RETURNING id, name;
INSERT INTO customers VALUES (100, 'duplicate', 'east');
INSERT INTO customers VALUES (100, 'upsert', 'west')
  ON CONFLICT (id) DO UPDATE SET name = excluded.name RETURNING id, name, region;
UPDATE customers SET region = 'central' WHERE id = 100 RETURNING id, region;
UPDATE orders SET amount = amount + 1 WHERE customer_id = 1;
SELECT sum(amount) FROM orders WHERE customer_id = 1;
DELETE FROM orders WHERE customer_id = 2 RETURNING id;
DELETE FROM customers WHERE id = 100;
SELECT count(*) FROM customers WHERE id = 100;
INSERT INTO orders (id, customer_id) VALUES (999, 7);
SELECT amount FROM orders WHERE id = 999 AND customer_id = 7;
INSERT INTO orders (id, customer_id, amount) VALUES (1000, 7, NULL);

-- Transactions and savepoints.
BEGIN;
INSERT INTO customers VALUES (200, 'in transaction', NULL);
SELECT count(*) FROM customers WHERE id = 200;
ROLLBACK;
SELECT count(*) FROM customers WHERE id = 200;
BEGIN;
INSERT INTO customers VALUES (201, 'kept', NULL);
SAVEPOINT s1;
INSERT INTO customers VALUES (202, 'dropped', NULL);
ROLLBACK TO s1;
INSERT INTO customers VALUES (203, 'kept too', NULL);
COMMIT;
SELECT id, name FROM customers WHERE id >= 200 ORDER BY id;

-- Prepared statements and parameters, including more than five executions.
PREPARE by_id (int) AS SELECT name FROM customers WHERE id = $1;
EXECUTE by_id(3);
EXECUTE by_id(4);
EXECUTE by_id(5);
EXECUTE by_id(6);
EXECUTE by_id(7);
EXECUTE by_id(8);
EXECUTE by_id(9);
PREPARE put (int, text) AS INSERT INTO customers VALUES ($1, $2, NULL);
EXECUTE put(300, 'prepared');
SELECT name FROM customers WHERE id = 300;

-- COPY goes to the right partitions.
COPY customers (id, name) FROM stdin;
400	copied one
401	copied two
\.
SELECT id, name FROM customers WHERE id >= 400 ORDER BY id;

-- Plans: partition pruning is PostgreSQL's own; no custom scan appears.
SELECT plan_text('SELECT * FROM customers WHERE id = 5') !~ 'Custom Scan' AS no_custom_scan_by_key;
SELECT plan_text('SELECT * FROM customers') !~ 'Custom Scan' AS no_custom_scan_full;
SELECT plan_text('SELECT * FROM customers WHERE id = 5') ~ 'customers_b' AS pruned_to_one_bucket;
SELECT plan_text('SELECT * FROM customers WHERE id = 5') !~ 'customers_b0000.*customers_b0001' AS only_one_partition_scanned;

-- The routing settings are harmless on one node.
SET kingres_shard.enable_routing = off;
SELECT name FROM customers WHERE id = 5;
SET kingres_shard.enable_routing = on;
-- The flag is fixed when the session is opened (a forwarded session is one that
-- another node opened as such), so the test reconnects with the option.
\c -reuse-previous=on "options='-c kingres_shard.forwarded=on'"
SET search_path = public;
SHOW kingres_shard.forwarded;
SELECT name FROM customers WHERE id = 5;
INSERT INTO customers VALUES (500, 'forwarded session', NULL);
SELECT count(*) AS forwarded_session_full_scan FROM customers;
\c -reuse-previous=on "options=''"
SET search_path = public;
SHOW kingres_shard.forwarded;
SELECT name FROM customers WHERE id = 500;

-- Constraints are enforced by PostgreSQL.
INSERT INTO customers VALUES (501, NULL, NULL);
INSERT INTO orders VALUES (1, 1, 1) ;
INSERT INTO orders VALUES (11, 1, 1) ;

-- The buckets view for the table.
SELECT count(*) AS buckets, count(DISTINCT group_id) AS groups, min(node_id) AS node
  FROM kingres.buckets WHERE table_name = 'customers'::regclass;

DROP FUNCTION plan_text(text);
DROP TABLE orders, customers;
DROP EXTENSION kingres_shard;
