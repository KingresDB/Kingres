-- Catalog creation, constraints, map_version triggers and GUCs.
CREATE EXTENSION kingres_shard;

SELECT c.relname
  FROM pg_class c
  WHERE c.relnamespace = 'kingres'::regnamespace AND c.relkind = 'r'
  ORDER BY 1;

SELECT conrelid::regclass AS tbl, conname, contype
  FROM pg_constraint
  WHERE connamespace = 'kingres'::regnamespace AND contype <> 'n'
  ORDER BY 1::text, 2;

-- Nothing is created outside the kingres schema.
SELECT count(*) AS tables_outside_kingres
  FROM pg_depend d
  JOIN pg_extension e ON e.oid = d.refobjid AND d.refclassid = 'pg_extension'::regclass
  JOIN pg_class c ON c.oid = d.objid AND d.classid = 'pg_class'::regclass
  WHERE e.extname = 'kingres_shard' AND c.relnamespace <> 'kingres'::regnamespace;

-- The map version starts at zero and every changing statement increments it.
SELECT version FROM kingres.map_version;
INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (1, 0, 'a.example', 5432, 'primary'), (2, 1, 'b.example', 5432, 'primary');
SELECT version FROM kingres.map_version;
UPDATE kingres.node SET is_active = false WHERE node_id = 2;
SELECT version FROM kingres.map_version;
INSERT INTO kingres.colocation VALUES (1, 8, 'int4', 0);
SELECT version FROM kingres.map_version;
INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id)
  SELECT 1, g, g % 2 FROM generate_series(0, 7) g;
SELECT version FROM kingres.map_version;
DELETE FROM kingres.node WHERE node_id = 2;
SELECT version FROM kingres.map_version;
-- A statement that changes no row still counts: the trigger is per statement.
DELETE FROM kingres.node WHERE node_id = 99;
SELECT version FROM kingres.map_version;

-- Constraints.
INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (3, 0, 'a.example', 5432, 'primary');
INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (3, 0, 'c.example', 5432, 'leader');
INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (3, -1, 'c.example', 5432, 'primary');
INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (3, 0, 'c.example', 70000, 'primary');
INSERT INTO kingres.colocation VALUES (2, 0, 'int4', 0);
INSERT INTO kingres.colocation VALUES (2, 4097, 'int4', 0);
INSERT INTO kingres.bucket_map VALUES (9, 0, 0);
INSERT INTO kingres.bucket_map VALUES (1, 0, 0);
INSERT INTO kingres.bucket_map VALUES (1, 2, 0, 'broken');
-- the bucket layout of 0.8: a bucket is below its modulus, a modulus divides max_modulus of the group
INSERT INTO kingres.bucket_map VALUES (1, 20, 0, 'active');
INSERT INTO kingres.bucket_map VALUES (1, 0, 0, 'active', 0);
INSERT INTO kingres.colocation VALUES (2, 4, 'int4', 0, 0);
INSERT INTO kingres.colocation VALUES (2, 4, 'int4', 0, 65537);
INSERT INTO kingres.dist_table VALUES ('pg_class'::regclass, 'range', 'oid', 1);
INSERT INTO kingres.dist_table VALUES ('pg_class'::regclass, 'hash', 'oid', 9);
INSERT INTO kingres.map_version VALUES (5);
INSERT INTO kingres.local_node VALUES (1);
INSERT INTO kingres.local_node VALUES (2);

-- The introspection views.
SELECT node_id, group_id, is_local FROM kingres.nodes ORDER BY node_id;
SELECT count(*) AS buckets_of_undistributed_tables FROM kingres.buckets;

-- The settings the extension defines.
SELECT name, setting, unit, context
  FROM pg_settings WHERE name LIKE 'kingres_shard.%' ORDER BY name;
SET kingres_shard.default_bucket_count = 0;
SET kingres_shard.default_bucket_count = 4097;
SET kingres_shard.nonexistent = 1;

DROP EXTENSION kingres_shard;
-- The schema is shared with the other KingresDB extensions and belongs to none of them:
-- it stays, empty, when the extension goes.
SELECT count(*) AS schema_left FROM pg_namespace WHERE nspname = 'kingres';
SELECT (SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace)
     + (SELECT count(*) FROM pg_proc WHERE pronamespace = 'kingres'::regnamespace) AS objects_left;
DROP SCHEMA kingres;
