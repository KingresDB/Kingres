-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- sql/bucket_lookup.sql
--   The bucket of a key: one place computes it (design/shard-phase2b.md, 4.7).
--
-- The expected file of this test was recorded from the code as it was BEFORE
-- the bucket lookup was moved into kingres_shard_hash_of_value() and
-- kingres_shard_bucket_for_hash(), and the bucket numbers in it are the
-- numbers that code computed.  Section 1 lists, for every supported key type,
-- the bucket of NULL and of boundary values, of values that hash differently
-- by collation (text, with a nondeterministic collation too) and of values that
-- are equal in several forms (an interval of 1 day and of 24 hours, a numeric
-- with trailing zeros, a bpchar with trailing blanks, a float -0), and checks
-- each against the partition PostgreSQL really stores the row in.  Section 2
-- shows values of another type than the column (the width of an integer, a
-- varchar for a text column) and the refusal of a type with no suitable hash
-- function.  Section 3 shows the bucket the planner chooses for a constant:
-- the number is in the plan.  A change of any bucket changes this file.
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

-- ---------------------------------------------------------------------------
-- 3 (set up first): the buckets the planner chooses for a constant.  The odd
-- buckets belong to a second group that does not exist as a server, so a
-- statement for such a bucket is a "Kingres Remote Statement" that names it,
-- and one for an even bucket reads the partition here.
-- ---------------------------------------------------------------------------
CREATE COLLATION ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false);
CREATE TABLE p_int4 (id int4 PRIMARY KEY, v text);
CREATE TABLE p_int8 (id int8 PRIMARY KEY, v text);
CREATE TABLE p_text (id text PRIMARY KEY, v text);
CREATE TABLE p_textc (id text COLLATE "C" PRIMARY KEY, v text);
CREATE TABLE p_uuid (id uuid PRIMARY KEY, v text);
CREATE TABLE p_date (id date PRIMARY KEY, v text);
CREATE TABLE p_numeric (id numeric PRIMARY KEY, v text);
CREATE TABLE p_tstz (id timestamptz PRIMARY KEY, v text);
CREATE TABLE p_float8 (id float8 PRIMARY KEY, v text);
SELECT kingres.create_distributed_table('p_int4', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('p_int8', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('p_text', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('p_textc', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('p_uuid', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('p_date', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('p_numeric', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('p_tstz', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('p_float8', 'id', bucket_count => 8);
INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (2, 1, '127.0.0.1', 1, 'primary');
UPDATE kingres.bucket_map SET group_id = 1 WHERE bucket % 2 = 1;

-- ---------------------------------------------------------------------------
-- 1: every supported key type, against the real partitions.  A case is a
-- type, a number of buckets, a collation and the values (as text; NULL is a
-- NULL key).  The test table kv is only the list of cases.
-- ---------------------------------------------------------------------------
CREATE TYPE pair AS (a int, b text);
CREATE TABLE kv (id int PRIMARY KEY, typ text, nb int, coll text, lits text[]);
INSERT INTO kv VALUES
  (101, 'int2', 7, NULL, ARRAY['0','1','-1','32767','-32768','12345',NULL]),
  (102, 'int4', 7, NULL, ARRAY['0','1','-1','2147483647','-2147483648','42','1000003',NULL]),
  (103, 'int4', 16, NULL, ARRAY['0','1','-1','2147483647','-2147483648','42','1000003',NULL]),
  (104, 'int8', 7, NULL, ARRAY['0','1','-1','9223372036854775807','-9223372036854775808','4294967296','-4294967297','1000003000009',NULL]),
  (105, 'int8', 128, NULL, ARRAY['0','1','2','3','127','128','129','9223372036854775807',NULL]),
  (106, 'oid', 7, NULL, ARRAY['0','1','2147483648','4294967295',NULL]),
  (107, 'bool', 7, NULL, ARRAY['true','false',NULL]),
  (108, '"char"', 7, NULL, ARRAY['a','Z','0','~',NULL]),
  (109, 'name', 7, 'C', ARRAY['','a','abc','Alpha_1',NULL]),
  (110, 'text', 7, 'default', ARRAY['','a','A','abc','ABC','Abc','key-1','hello world','abc ',repeat('x', 300),repeat('ab', 2000),NULL]),
  (111, 'text', 7, 'C', ARRAY['','a','A','abc','ABC','Abc','key-1','hello world','abc ',repeat('x', 300),repeat('ab', 2000),NULL]),
  (112, 'text', 7, 'POSIX', ARRAY['','a','A','abc','ABC','Abc','key-1','hello world','abc ',repeat('x', 300),repeat('ab', 2000),NULL]),
  (113, 'text', 7, 'ci', ARRAY['','a','A','abc','ABC','Abc','key-1','hello world','abc ',repeat('x', 300),repeat('ab', 2000),NULL]),
  (114, 'varchar', 7, 'default', ARRAY['','a','A','abc','ABC','key-1','hello world',NULL]),
  (115, 'bpchar', 7, 'default', ARRAY['','abc','abc  ','abc   x',' ','A',NULL]),
  (116, 'bytea', 7, NULL, ARRAY['\x','\x00','\x00ff','\xdeadbeef','\xffffffffffffffff',NULL]),
  (117, 'uuid', 7, NULL, ARRAY['00000000-0000-0000-0000-000000000000','ffffffff-ffff-ffff-ffff-ffffffffffff','123e4567-e89b-12d3-a456-426614174000',NULL]),
  (118, 'date', 7, NULL, ARRAY['2020-01-01','1970-01-01','0001-01-01','2000-02-29','infinity','-infinity',NULL]),
  (119, 'time', 7, NULL, ARRAY['00:00:00','12:34:56.789','23:59:59.999999','24:00:00',NULL]),
  (120, 'timetz', 7, NULL, ARRAY['00:00:00+00','12:34:56+05:30','23:59:59.999999-12',NULL]),
  (121, 'timestamp', 7, NULL, ARRAY['1970-01-01 00:00:00','2020-02-29 12:34:56.789012','0001-01-01 00:00:00','infinity','-infinity',NULL]),
  (122, 'timestamptz', 7, NULL, ARRAY['1970-01-01 00:00:00+00','2020-02-29 12:34:56.789012+03','0001-01-01 00:00:00+00','infinity','-infinity',NULL]),
  (123, 'interval', 7, NULL, ARRAY['0','1 day','24 hours','30 days','1 month','1 year 2 mons 3 days 04:05:06.789','-1 day',NULL]),
  (124, 'numeric', 7, NULL, ARRAY['0','0.0','1','1.0','1.50','-1','12345678901234567890.123456789','NaN','1e100','0.000000001',NULL]),
  (125, 'float4', 7, NULL, ARRAY['0','-0','1.5','NaN','Infinity','-Infinity','3.4e38','1e-45',NULL]),
  (126, 'float8', 7, NULL, ARRAY['0','-0','1.5','NaN','Infinity','-Infinity','1e308','5e-324',NULL]),
  (127, 'inet', 7, NULL, ARRAY['0.0.0.0','127.0.0.1','192.168.1.1/24','::1','2001:db8::/32',NULL]),
  (128, 'cidr', 7, NULL, ARRAY['10.0.0.0/8','192.168.0.0/16','::/0',NULL]),
  (129, 'macaddr', 7, NULL, ARRAY['00:00:00:00:00:00','08:00:2b:01:02:03','ff:ff:ff:ff:ff:ff',NULL]),
  (130, 'macaddr8', 7, NULL, ARRAY['00:00:00:00:00:00:00:00','08:00:2b:01:02:03:04:05',NULL]),
  (131, 'pg_lsn', 7, NULL, ARRAY['0/0','16/B374D848','FFFFFFFF/FFFFFFFF',NULL]),
  (132, 'tid', 7, NULL, ARRAY['(0,0)','(1,2)','(4294967295,65535)',NULL]),
  (133, 'xid8', 7, NULL, ARRAY['0','1','18446744073709551615',NULL]),
  (134, 'jsonb', 7, NULL, ARRAY['{}','[]','null','{"a": 1}','{"a": 1, "b": [1, 2, {"c": "d"}]}','"text"','1.50','1.5',NULL]),
  (135, 'int4[]', 7, NULL, ARRAY['{}','{1}','{1,2,3}','{NULL}','{{1,2},{3,4}}',NULL]),
  (136, 'text[]', 7, 'default', ARRAY['{}','{a}','{a,b}','{"a b","c,d"}','{NULL,x}',NULL]),
  (137, 'int4range', 7, NULL, ARRAY['(,)','empty','[1,10)','[1,)','(,5]',NULL]),
  (138, 'int4multirange', 7, NULL, ARRAY['{}','{[1,3),[5,8)}',NULL]),
  (139, 'pair', 7, NULL,ARRAY['(1,x)','(,)','(2,"a b")','(2147483647,)',NULL]),
  (140, 'oidvector', 7, NULL, ARRAY['','0','0 1 2',NULL]);

-- The groups of the cases (the colocation rows are what bucket_for() reads).
INSERT INTO kingres.colocation
  SELECT id, nb, typ::regtype,
         coalesce((SELECT oid FROM pg_collation WHERE collname = coll LIMIT 1), 0)
    FROM kv;
-- The partitioned tables: the real routing of PostgreSQL for the same type,
-- collation and number of buckets.
SELECT format('CREATE TABLE pt%s (k %s%s, ord int) PARTITION BY HASH (k)',
              id, typ, CASE WHEN coll IS NULL THEN '' ELSE format(' COLLATE %I', coll) END)
  FROM kv ORDER BY id \gexec
SELECT format('CREATE TABLE pt%s_b%s PARTITION OF pt%s FOR VALUES WITH (MODULUS %s, REMAINDER %s)',
              id, r, id, nb, r)
  FROM kv, generate_series(0, nb - 1) r ORDER BY id, r \gexec
CREATE TABLE res (id int, ord int, bucket int, phys int);
SELECT format('INSERT INTO pt%s SELECT l::%s, o FROM kv, unnest(lits) WITH ORDINALITY u(l, o) WHERE kv.id = %s',
              id, typ, id)
  FROM kv ORDER BY id \gexec
SELECT format('INSERT INTO res SELECT %s, o, kingres.bucket_for(%s, l::%s), NULL FROM kv, unnest(lits) WITH ORDINALITY u(l, o) WHERE kv.id = %s',
              id, id, typ, id)
  FROM kv ORDER BY id \gexec
SELECT format('UPDATE res SET phys = (SELECT substring(tableoid::regclass::text from ''_b([0-9]+)$'')::int FROM pt%s WHERE pt%s.ord = res.ord) WHERE res.id = %s',
              id, id, id)
  FROM kv ORDER BY id \gexec

-- The bucket of every value, in the order of the list of its case.
SELECT kv.id, kv.typ, kv.nb, coalesce(kv.coll, '-') AS coll,
       string_agg(coalesce(res.bucket::text, 'x'), ' ' ORDER BY res.ord) AS buckets
  FROM kv JOIN res ON res.id = kv.id GROUP BY kv.id, kv.typ, kv.nb, kv.coll ORDER BY kv.id;
-- Each equals the partition PostgreSQL stored the row in.
SELECT count(*) AS values_checked,
       count(*) FILTER (WHERE bucket IS DISTINCT FROM phys) AS mismatches
  FROM res;
SELECT kv.id, kv.typ, count(DISTINCT res.bucket) AS buckets_hit
  FROM kv JOIN res ON res.id = kv.id WHERE kv.id IN (103, 105, 110) GROUP BY kv.id, kv.typ ORDER BY kv.id;
-- Values that are equal in several forms land in one bucket.
SELECT bool_and(same) AS equal_values_share_a_bucket FROM (
  SELECT (SELECT count(DISTINCT bucket) = 1 FROM res WHERE id = 123 AND ord IN (2, 3)) AS same   -- 1 day, 24 hours
  UNION ALL
  SELECT (SELECT count(DISTINCT bucket) = 1 FROM res WHERE id = 123 AND ord IN (4, 5))           -- 30 days, 1 month
  UNION ALL
  SELECT (SELECT count(DISTINCT bucket) = 1 FROM res WHERE id = 124 AND ord IN (1, 2))           -- 0, 0.0
  UNION ALL
  SELECT (SELECT count(DISTINCT bucket) = 1 FROM res WHERE id = 124 AND ord IN (3, 4))           -- 1, 1.0
  UNION ALL
  SELECT (SELECT count(DISTINCT bucket) = 1 FROM res WHERE id = 115 AND ord IN (2, 3))           -- abc, 'abc  '
  UNION ALL
  SELECT (SELECT count(DISTINCT bucket) = 1 FROM res WHERE id = 125 AND ord IN (1, 2))           -- float4 0, -0
  UNION ALL
  SELECT (SELECT count(DISTINCT bucket) = 1 FROM res WHERE id = 126 AND ord IN (1, 2))           -- float8 0, -0
  UNION ALL
  SELECT (SELECT count(DISTINCT bucket) = 1 FROM res WHERE id = 113 AND ord IN (4, 5, 6))        -- abc, ABC, Abc under the nondeterministic collation
) s;
SELECT (SELECT count(DISTINCT bucket) FROM res WHERE id = 111 AND ord IN (4, 5, 6)) AS deterministic_collation_distinguishes_case;
-- A NULL key is in bucket 0, as for the partitions.
SELECT count(*) AS null_keys, count(*) FILTER (WHERE bucket = 0 AND phys = 0) AS in_bucket_0
  FROM res JOIN kv ON kv.id = res.id WHERE res.ord = cardinality(kv.lits) AND kv.lits[res.ord] IS NULL;

-- ---------------------------------------------------------------------------
-- 2: a value of another type than the column.  The hash function of the
-- operator family of the column is used for it; a type the family does not
-- contain is refused.
-- ---------------------------------------------------------------------------
SELECT kingres.bucket_for(104, 5::int2) AS i2, kingres.bucket_for(104, 5::int4) AS i4, kingres.bucket_for(104, 5::int8) AS i8,
       kingres.bucket_for(104, (-5)::int2) AS m2, kingres.bucket_for(104, (-5)::int4) AS m4, kingres.bucket_for(104, (-5)::int8) AS m8,
       kingres.bucket_for(104, 2147483648::int8) AS big8;
SELECT kingres.bucket_for(102, 5::int2) AS i2, kingres.bucket_for(102, 5::int4) AS i4, kingres.bucket_for(102, 5::int8) AS i8,
       kingres.bucket_for(101, 5::int2) AS s2, kingres.bucket_for(101, 5::int4) AS s4, kingres.bucket_for(101, 5::int8) AS s8;
SELECT kingres.bucket_for(110, 'abc'::varchar) = kingres.bucket_for(110, 'abc'::text) AS varchar_for_text,
       kingres.bucket_for(110, 'abc'::varchar) AS abc_for_text,
       kingres.bucket_for(114, 'abc'::text) AS text_for_varchar,
       kingres.bucket_for(110, 'abc'::name) AS name_for_text,
       kingres.bucket_for(109, 'abc'::text) AS text_for_name;
SELECT kingres.bucket_for(126, 1.5::float4) AS f4_for_f8, kingres.bucket_for(126, 1.5::float8) AS f8_for_f8,
       kingres.bucket_for(125, 1.5::float8) AS f8_for_f4, kingres.bucket_for(125, 1.5::float4) AS f4_for_f4;
SELECT kingres.bucket_for(118, '2020-01-01'::date) AS date_for_date,
       kingres.bucket_for(122, '2020-01-01 00:00:00+00'::timestamptz) AS tstz_for_tstz;
SELECT kingres.bucket_for(118, '2020-01-01'::timestamp) AS ts_for_date;
SELECT kingres.bucket_for(121, '2020-01-01'::date) AS date_for_ts;
SELECT kingres.bucket_for(110, 'abc'::bpchar);
SELECT kingres.bucket_for(102, 'abc'::text);
SELECT kingres.bucket_for(104, 1.5::numeric);
SELECT kingres.bucket_for(124, 1::int4);
SELECT kingres.bucket_for(117, 'abc'::text);
SELECT kingres.bucket_for(113, 'abc'::text) AS ci_lower, kingres.bucket_for(113, 'ABC'::text) AS ci_upper,
       kingres.bucket_for(110, 'abc'::text) AS default_lower, kingres.bucket_for(110, 'ABC'::text) AS default_upper;

-- ---------------------------------------------------------------------------
-- 3: the bucket of a constant in the plan.  A remote statement names the
-- bucket; a local one reads the partition (its name ends in the bucket).
-- ---------------------------------------------------------------------------
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 1;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 2;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 3;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 4;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 5;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 6;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 2147483647;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = -2147483648;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 0;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE 5 = id;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 2 + 3;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 5::int8;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 5::int2;
EXPLAIN (COSTS OFF) SELECT v FROM p_int8 WHERE id = 1;
EXPLAIN (COSTS OFF) SELECT v FROM p_int8 WHERE id = 2;
EXPLAIN (COSTS OFF) SELECT v FROM p_int8 WHERE id = 3;
EXPLAIN (COSTS OFF) SELECT v FROM p_int8 WHERE id = 4;
EXPLAIN (COSTS OFF) SELECT v FROM p_int8 WHERE id = 5::int8;
EXPLAIN (COSTS OFF) SELECT v FROM p_int8 WHERE id = 5::int2;
EXPLAIN (COSTS OFF) SELECT v FROM p_int8 WHERE id = 9223372036854775807;
EXPLAIN (COSTS OFF) SELECT v FROM p_int8 WHERE id = -9223372036854775808;
EXPLAIN (COSTS OFF) SELECT v FROM p_int8 WHERE id = 4294967296;
EXPLAIN (COSTS OFF) SELECT v FROM p_text WHERE id = 'a';
EXPLAIN (COSTS OFF) SELECT v FROM p_text WHERE id = 'b';
EXPLAIN (COSTS OFF) SELECT v FROM p_text WHERE id = 'abc';
EXPLAIN (COSTS OFF) SELECT v FROM p_text WHERE id = 'ABC';
EXPLAIN (COSTS OFF) SELECT v FROM p_text WHERE id = '';
EXPLAIN (COSTS OFF) SELECT v FROM p_text WHERE id = 'abc'::varchar;
EXPLAIN (COSTS OFF) SELECT v FROM p_text WHERE id = 'abc' COLLATE "C";
EXPLAIN (COSTS OFF) SELECT v FROM p_text WHERE id = 'abc' COLLATE ci;
EXPLAIN (COSTS OFF) SELECT v FROM p_textc WHERE id = 'a';
EXPLAIN (COSTS OFF) SELECT v FROM p_textc WHERE id = 'b';
EXPLAIN (COSTS OFF) SELECT v FROM p_textc WHERE id = 'abc';
EXPLAIN (COSTS OFF) SELECT v FROM p_textc WHERE id = 'ABC';
EXPLAIN (COSTS OFF) SELECT v FROM p_uuid WHERE id = '00000000-0000-0000-0000-000000000000';
EXPLAIN (COSTS OFF) SELECT v FROM p_uuid WHERE id = 'ffffffff-ffff-ffff-ffff-ffffffffffff';
EXPLAIN (COSTS OFF) SELECT v FROM p_uuid WHERE id = '123e4567-e89b-12d3-a456-426614174000';
EXPLAIN (COSTS OFF) SELECT v FROM p_date WHERE id = '2020-01-01';
EXPLAIN (COSTS OFF) SELECT v FROM p_date WHERE id = '2020-01-02';
EXPLAIN (COSTS OFF) SELECT v FROM p_date WHERE id = '1970-01-01';
EXPLAIN (COSTS OFF) SELECT v FROM p_date WHERE id = 'infinity';
EXPLAIN (COSTS OFF) SELECT v FROM p_numeric WHERE id = 1;
EXPLAIN (COSTS OFF) SELECT v FROM p_numeric WHERE id = 1.0;
EXPLAIN (COSTS OFF) SELECT v FROM p_numeric WHERE id = 2;
EXPLAIN (COSTS OFF) SELECT v FROM p_numeric WHERE id = 1.5;
EXPLAIN (COSTS OFF) SELECT v FROM p_numeric WHERE id = 3::int8;
EXPLAIN (COSTS OFF) SELECT v FROM p_tstz WHERE id = '2020-01-01 00:00:00+00';
EXPLAIN (COSTS OFF) SELECT v FROM p_tstz WHERE id = '2020-01-01 00:00:01+00';
EXPLAIN (COSTS OFF) SELECT v FROM p_tstz WHERE id = '2020-01-01 03:30:00+03:30';
EXPLAIN (COSTS OFF) SELECT v FROM p_float8 WHERE id = 1;
EXPLAIN (COSTS OFF) SELECT v FROM p_float8 WHERE id = 1.5;
EXPLAIN (COSTS OFF) SELECT v FROM p_float8 WHERE id = 2;
EXPLAIN (COSTS OFF) SELECT v FROM p_float8 WHERE id = 'NaN';
EXPLAIN (COSTS OFF) SELECT v FROM p_float8 WHERE id = '-0';
EXPLAIN (COSTS OFF) SELECT v FROM p_float8 WHERE id = 0;
-- Writes are placed by the same bucket.
EXPLAIN (COSTS OFF) INSERT INTO p_int4 VALUES (1, 'a');
EXPLAIN (COSTS OFF) INSERT INTO p_int4 VALUES (2, 'a');
EXPLAIN (COSTS OFF) UPDATE p_int8 SET v = 'b' WHERE id = 3;
EXPLAIN (COSTS OFF) DELETE FROM p_text WHERE id = 'abc';
-- A key that is a parameter is decided when the value is known.
PREPARE pk (int4) AS SELECT v FROM p_int4 WHERE id = $1;
EXPLAIN (COSTS OFF) EXECUTE pk(1);
EXPLAIN (COSTS OFF) EXECUTE pk(2);
EXPLAIN (COSTS OFF) EXECUTE pk(1);
EXPLAIN (COSTS OFF) EXECUTE pk(2);
EXPLAIN (COSTS OFF) EXECUTE pk(1);
EXPLAIN (COSTS OFF) EXECUTE pk(2);
EXPLAIN (COSTS OFF) EXECUTE pk(1);
EXPLAIN (COSTS OFF) EXECUTE pk(2);
-- Several keys of the same statement.
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 1 AND id = 1;
EXPLAIN (COSTS OFF) SELECT v FROM p_int4 WHERE id = 1 OR id = 3;
-- the plan of the scan (step 7 answers an aggregate by partial aggregation; that is tested in t/139)
SET kingres_shard.enable_partial_aggregation = off;
EXPLAIN (COSTS OFF) SELECT count(*) FROM p_int4 WHERE id IN (1, 2);
RESET kingres_shard.enable_partial_aggregation;
EXPLAIN (COSTS OFF) SELECT a.v FROM p_int4 a JOIN p_int8 b ON a.id = b.id WHERE a.id = 1 AND b.id = 1;
EXPLAIN (COSTS OFF) SELECT a.v FROM p_int4 a JOIN p_int4 b ON a.v = b.v WHERE a.id = 1 AND b.id = 1;
EXPLAIN (COSTS OFF) SELECT a.v FROM p_int4 a JOIN p_int4 b ON a.v = b.v WHERE a.id = 1 AND b.id = 3;

-- Clean up: the second group goes away, so the tables drop locally.
UPDATE kingres.bucket_map SET group_id = 0;
DELETE FROM kingres.node WHERE node_id = 2;
SELECT format('DROP TABLE pt%s', id) FROM kv ORDER BY id \gexec
DROP TABLE kv, res;
DROP TYPE pair;
DROP TABLE p_int4, p_int8, p_text, p_textc, p_uuid, p_date, p_numeric, p_tstz, p_float8;
DROP COLLATION ci;
DROP EXTENSION kingres_shard;
