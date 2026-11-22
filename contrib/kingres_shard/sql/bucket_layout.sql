-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- sql/bucket_layout.sql
--   The bucket layout of version 0.8 (design/shard-phase10.md, sections 1.1 to 1.3): every bucket has its own
--   modulus, the buckets of a group cover each slot of hash % max_modulus exactly once, and the bucket of a key is
--   found through the catalog and not by hash % bucket_count.
--
--   1. kingres.bucket_for() agrees with the partition PostgreSQL really stores a row in, for tables whose
--      partitions have several moduli (a bucket split in two, a split in four, a child split again).
--   2. kingres.bucket_range(), the view kingres.buckets and the layout check (bucket_layout, bucket_map_incomplete).
--   3. The rows of the catalog that do not say their modulus (an insert of the older form, a catalog document of a
--      map group that is not upgraded) get the layout of a group that was never split.
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;

-- Hash partitioned tables with mixed moduli, made as a split makes them: bucket (m, r) is replaced by
-- (2m, r) and (2m, r + m); (m, r) by four buckets (4m, r + i*m); a child is split again.
--   t_two:   4 buckets, bucket 2 split in two:                     0:4 1:4 2:8 6:8 3:4
--   t_four:  4 buckets, bucket 1 split in four:                    0:4 2:4 3:4 1:16 5:16 9:16 13:16
--   t_deep:  4 buckets, 2 split in two, then 6 (a child) in two:    0:4 1:4 3:4 2:8 6:16 14:16
--   t_plain: 6 buckets, never split (the modulus is not a power of two):  0..5 of modulus 6
CREATE TABLE t_two (k int4) PARTITION BY HASH (k);
CREATE TABLE t_four (k int4) PARTITION BY HASH (k);
CREATE TABLE t_deep (k int4) PARTITION BY HASH (k);
CREATE TABLE t_plain (k int4) PARTITION BY HASH (k);
CREATE FUNCTION mkp(t text, b int, m int) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES WITH (MODULUS %s, REMAINDER %s)', t || '_b' || lpad(b::text, 4, '0'), t, m, b);
END $$;
SELECT mkp('t_two', 0, 4), mkp('t_two', 1, 4), mkp('t_two', 2, 8), mkp('t_two', 6, 8), mkp('t_two', 3, 4);
SELECT mkp('t_four', 0, 4), mkp('t_four', 2, 4), mkp('t_four', 3, 4), mkp('t_four', 1, 16), mkp('t_four', 5, 16), mkp('t_four', 9, 16), mkp('t_four', 13, 16);
SELECT mkp('t_deep', 0, 4), mkp('t_deep', 1, 4), mkp('t_deep', 3, 4), mkp('t_deep', 2, 8), mkp('t_deep', 6, 16), mkp('t_deep', 14, 16);
SELECT mkp('t_plain', b, 6) FROM generate_series(0, 5) b;

-- Their catalog rows.  The 4-argument insert of the older form for the group that was never split.
INSERT INTO kingres.colocation VALUES (1, 4, 'int4', 0, 8, 0);
INSERT INTO kingres.colocation VALUES (2, 4, 'int4', 0, 16, 0);
INSERT INTO kingres.colocation VALUES (3, 4, 'int4', 0, 16, 0);
INSERT INTO kingres.colocation VALUES (4, 6, 'int4', 0);
INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id, modulus) VALUES
  (1, 0, 0, 4), (1, 1, 1, 4), (1, 2, 0, 8), (1, 6, 1, 8), (1, 3, 0, 4),
  (2, 0, 0, 4), (2, 2, 0, 4), (2, 3, 1, 4), (2, 1, 0, 16), (2, 5, 1, 16), (2, 9, 0, 16), (2, 13, 1, 16),
  (3, 0, 0, 4), (3, 1, 1, 4), (3, 3, 1, 4), (3, 2, 0, 8), (3, 6, 1, 16), (3, 14, 0, 16);
INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id) SELECT 4, b, b % 2 FROM generate_series(0, 5) b;
SELECT colocation_id, bucket_count, max_modulus, placement_epoch FROM kingres.colocation ORDER BY 1;
SELECT colocation_id, count(*) AS buckets, min(modulus) AS min_modulus, max(modulus) AS max_modulus
  FROM kingres.bucket_map GROUP BY 1 ORDER BY 1;

INSERT INTO t_two SELECT g FROM generate_series(-3000, 3000) g;
INSERT INTO t_four SELECT g FROM generate_series(-3000, 3000) g;
INSERT INTO t_deep SELECT g FROM generate_series(-3000, 3000) g;
INSERT INTO t_plain SELECT g FROM generate_series(-3000, 3000) g;
INSERT INTO t_two VALUES (NULL), (2147483647), (-2147483648);
INSERT INTO t_four VALUES (NULL), (2147483647), (-2147483648);
INSERT INTO t_deep VALUES (NULL), (2147483647), (-2147483648);

-- 1. For every table: rows stored, rows whose partition differs from the bucket that kingres.bucket_for() names,
--    and the number of different buckets that hold rows (every bucket does).
SELECT 't_two' AS tbl, count(*) AS total,
       count(*) FILTER (WHERE kingres.bucket_for(1, k) IS DISTINCT FROM
                        substring(tableoid::regclass::text from '_b([0-9]+)$')::int) AS mismatches,
       count(DISTINCT tableoid) AS partitions_with_rows
  FROM t_two
UNION ALL
SELECT 't_four', count(*),
       count(*) FILTER (WHERE kingres.bucket_for(2, k) IS DISTINCT FROM
                        substring(tableoid::regclass::text from '_b([0-9]+)$')::int),
       count(DISTINCT tableoid)
  FROM t_four
UNION ALL
SELECT 't_deep', count(*),
       count(*) FILTER (WHERE kingres.bucket_for(3, k) IS DISTINCT FROM
                        substring(tableoid::regclass::text from '_b([0-9]+)$')::int),
       count(DISTINCT tableoid)
  FROM t_deep
UNION ALL
SELECT 't_plain', count(*),
       count(*) FILTER (WHERE kingres.bucket_for(4, k) IS DISTINCT FROM
                        substring(tableoid::regclass::text from '_b([0-9]+)$')::int),
       count(DISTINCT tableoid)
  FROM t_plain;

-- 2. bucket_range and the layout check.
SELECT * FROM kingres.bucket_range(1, 2);
SELECT * FROM kingres.bucket_range(1, 6);
SELECT * FROM kingres.bucket_range(2, 13);
SELECT * FROM kingres.bucket_range(4, 5);
SELECT count(*) AS no_such_bucket FROM kingres.bucket_range(1, 7);
SELECT * FROM kingres.internal_check_layout();

-- the view
INSERT INTO kingres.dist_table VALUES ('t_two'::regclass, 'hash', 'k', 1);
SELECT bucket, modulus, partition_name, group_id FROM kingres.buckets ORDER BY bucket;
DELETE FROM kingres.dist_table;

-- a gap: bucket 6 (slots 6 of 8) is missing
DELETE FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = 6;
SELECT check_name, object, detail FROM kingres.internal_check_layout();
SELECT min(k) AS gapkey FROM t_two_b0006 \gset
SELECT kingres.bucket_for(1, :gapkey);
INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id, modulus) VALUES (1, 6, 1, 8);
SELECT * FROM kingres.internal_check_layout();
-- an overlap: bucket 2 of modulus 4 covers the slots 2 and 6, which buckets 2 and 6 of modulus 8 cover
UPDATE kingres.bucket_map SET modulus = 4 WHERE colocation_id = 1 AND bucket = 2;
SELECT check_name, object FROM kingres.internal_check_layout();
UPDATE kingres.bucket_map SET modulus = 8 WHERE colocation_id = 1 AND bucket = 2;
-- a modulus that is not a divisor of max_modulus
UPDATE kingres.bucket_map SET modulus = 6 WHERE colocation_id = 1 AND bucket = 2;
SELECT check_name, object FROM kingres.internal_check_layout();
UPDATE kingres.bucket_map SET modulus = 8 WHERE colocation_id = 1 AND bucket = 2;
-- an overlap and a gap that cancel in the sum of the slots: buckets 4 5 6 7 0 1 of modulus 8 and bucket 2 of modulus 4 (slots 2 and 6):
-- slot 6 is covered twice and slot 3 not at all, and the buckets cover exactly 8 slots in all
INSERT INTO kingres.colocation VALUES (5, 4, 'int4', 0, 8, 0);
INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id, modulus) VALUES (5, 4, 0, 8), (5, 5, 0, 8), (5, 6, 0, 8), (5, 7, 0, 8), (5, 0, 0, 8), (5, 1, 0, 8), (5, 2, 0, 4);
SELECT check_name, object FROM kingres.internal_check_layout();
DELETE FROM kingres.bucket_map WHERE colocation_id = 5;
DELETE FROM kingres.colocation WHERE colocation_id = 5;
-- a group that was never split and lacks a bucket keeps the old finding
DELETE FROM kingres.bucket_map WHERE colocation_id = 4 AND bucket = 5;
SELECT check_name, object, detail FROM kingres.internal_check_layout();
INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id) VALUES (4, 5, 1);
SELECT * FROM kingres.internal_check_layout();

-- 3. A catalog document of a map group that is not upgraded (no max_modulus, no modulus, no placement_epoch):
--    the colocation gets the layout of a group that was never split.
SELECT kingres.internal_apply_catalog(jsonb_build_object(
  'version', 100, 'term', 0,
  'node', '[]'::jsonb,
  'colocation', '[{"colocation_id": 7, "bucket_count": 4, "dist_column_type": "integer", "dist_collation": 0}]'::jsonb,
  'dist_table', '[]'::jsonb,
  'bucket_map', '[{"colocation_id": 7, "bucket": 0, "group_id": 0, "state": "active"}, {"colocation_id": 7, "bucket": 1, "group_id": 0, "state": "active"},
                  {"colocation_id": 7, "bucket": 2, "group_id": 0, "state": "active"}, {"colocation_id": 7, "bucket": 3, "group_id": 0, "state": "active"}]'::jsonb));
SELECT colocation_id, bucket_count, max_modulus, placement_epoch FROM kingres.colocation;
SELECT bucket, modulus FROM kingres.bucket_map ORDER BY bucket;
SELECT * FROM kingres.internal_check_layout();
-- and a document of this version keeps what it says
SELECT kingres.internal_apply_catalog(jsonb_set(kingres.internal_catalog_snapshot(), '{colocation,0}',
       (kingres.internal_catalog_snapshot() -> 'colocation' -> 0) || '{"max_modulus": 8, "placement_epoch": 5}'));
SELECT colocation_id, bucket_count, max_modulus, placement_epoch FROM kingres.colocation;

DROP EXTENSION kingres_shard CASCADE;
