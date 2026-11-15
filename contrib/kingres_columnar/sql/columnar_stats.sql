-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: PostgreSQL
--
-- columnar_stats: the minimum, maximum and null count kept for each chunk
-- group and stripe, the rules under which they are kept, and what
-- columnar_info and columnar_stripes report.
SET search_path = public;
SET max_parallel_workers_per_gather = 0;

-- the Columnar lines of the plan of a query; the byte count is "none" or "some"
CREATE FUNCTION kc_scan(q text) RETURNS SETOF text LANGUAGE plpgsql AS $$
DECLARE l text;
BEGIN
    FOR l IN EXECUTE 'EXPLAIN (analyze, costs off, timing off, summary off, buffers off) ' || q LOOP
        IF l ~ 'Compressed Bytes Read' THEN
            RETURN NEXT btrim(regexp_replace(regexp_replace(l, 'Read: 0 bytes', 'Read: none'), 'Read: [1-9][0-9]* bytes', 'Read: some'));
        ELSIF l ~ 'Skipped' THEN
            RETURN NEXT btrim(l);
        END IF;
    END LOOP;
END $$;

-- the rows a query returns and the chunk groups (of 3) and stripes (of 1) it skips
CREATE FUNCTION kc_skip(tbl text, cond text) RETURNS TABLE (n bigint, stripes_skipped text, groups_skipped text) LANGUAGE plpgsql AS $$
DECLARE l text;
BEGIN
    EXECUTE format('SELECT count(*) FROM %s WHERE %s', tbl, cond) INTO n;
    FOR l IN EXECUTE format('EXPLAIN (analyze, costs off, timing off, summary off, buffers off) SELECT count(*) FROM %s WHERE %s', tbl, cond) LOOP
        IF l ~ 'Stripes Skipped' THEN stripes_skipped := regexp_replace(btrim(l), '^.*: ', ''); END IF;
        IF l ~ 'Chunk Groups Skipped' THEN groups_skipped := regexp_replace(btrim(l), '^.*: ', ''); END IF;
    END LOOP;
    RETURN NEXT;
END $$;

-- 3 chunk groups of 100 rows in one stripe, every column ordered so that the groups do not overlap
CREATE TABLE st_int (id int, i2 int2, i4 int, i8 bigint, neg int, nl int) USING columnar;
SELECT kingres.columnar_set_options('st_int', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO st_int SELECT i, (i - 150)::int2, i * 10, i::bigint * 100000000000, -i,
       CASE WHEN i BETWEEN 101 AND 200 THEN NULL ELSE i END FROM generate_series(1, 300) i;
SELECT stripes, chunk_groups, rows FROM kingres.columnar_info('st_int');
SELECT * FROM kc_skip('st_int', 'id = 250');
SELECT * FROM kc_skip('st_int', 'i2 = -100');
SELECT * FROM kc_skip('st_int', 'i2 < -100');
SELECT * FROM kc_skip('st_int', 'neg = -250');
SELECT * FROM kc_skip('st_int', 'neg < -250');
SELECT * FROM kc_skip('st_int', 'neg > -50');
SELECT * FROM kc_skip('st_int', 'neg BETWEEN -150 AND -101');
SELECT * FROM kc_skip('st_int', 'i8 > 29900000000000');
-- a chunk group that holds only NULLs is skipped by every comparison and by IS NOT NULL
SELECT * FROM kc_skip('st_int', 'nl = 150');
SELECT * FROM kc_skip('st_int', 'nl > 0');
SELECT * FROM kc_skip('st_int', 'nl < 1000');
SELECT * FROM kc_skip('st_int', 'nl IS NOT NULL');
SELECT * FROM kc_skip('st_int', 'nl IS NULL');
SELECT * FROM kc_skip('st_int', 'nl = ANY (ARRAY[5, 150, 250])');
SELECT * FROM kc_skip('st_int', 'nl = 5 OR nl = 250');
SELECT * FROM kc_skip('st_int', 'nl IS NULL OR nl > 250');
SELECT * FROM kc_skip('st_int', 'id IS NULL');
SELECT * FROM kc_skip('st_int', 'id IS NOT NULL');

-- a table whose stripe holds only NULLs in a column: the stripe is skipped
CREATE TABLE st_null (id int, nl int, nl_txt text COLLATE "C") USING columnar;
INSERT INTO st_null SELECT i, NULL, NULL FROM generate_series(1, 300) i;
SELECT * FROM kc_skip('st_null', 'nl = 5');
SELECT * FROM kc_skip('st_null', 'nl IS NOT NULL');
SELECT * FROM kc_skip('st_null', 'nl IS NULL');
SELECT * FROM kc_skip('st_null', 'nl_txt > ''a''');
SELECT * FROM kc_skip('st_null', 'nl_txt IS NULL');
SELECT * FROM kc_skip('st_null', 'id = 5');

-- text: values of more than 64 bytes turn the statistics of their chunk group off
CREATE TABLE st_txt (id int, t text COLLATE "C", v varchar(200) COLLATE "C") USING columnar;
SELECT kingres.columnar_set_options('st_txt', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO st_txt SELECT i, CASE WHEN i = 150 THEN repeat('m', 100) ELSE 'k' || lpad(i::text, 5, '0') END,
       CASE WHEN i = 150 THEN repeat('m', 64) ELSE 'k' || lpad(i::text, 5, '0') END FROM generate_series(1, 300) i;
SELECT * FROM kc_skip('st_txt', 't = ''k00250''');
SELECT * FROM kc_skip('st_txt', 't = ''k00150''');
SELECT * FROM kc_skip('st_txt', 't = ''zzz''');
SELECT * FROM kc_skip('st_txt', 't = repeat(''m'', 100)');
SELECT * FROM kc_skip('st_txt', 't < ''a''');
SELECT * FROM kc_skip('st_txt', 'v = ''k00250''');
SELECT * FROM kc_skip('st_txt', 'v = repeat(''m'', 64)');
SELECT * FROM kc_skip('st_txt', 'v = ''zzz''');
SELECT * FROM kc_skip('st_txt', 'id = 250');
-- every chunk group has statistics: the stripe is skipped as a whole
CREATE TABLE st_txt2 (id int, t text COLLATE "C") USING columnar;
INSERT INTO st_txt2 SELECT i, 'k' || lpad(i::text, 5, '0') FROM generate_series(1, 300) i;
SELECT * FROM kc_skip('st_txt2', 't = ''zzz''');
SELECT * FROM kc_skip('st_txt2', 't = ''k00250''');
-- one chunk group with a long value: neither the chunk group nor its stripe has statistics
CREATE TABLE st_txt3 (id int, t text COLLATE "C") USING columnar;
INSERT INTO st_txt3 SELECT i, CASE WHEN i = 1 THEN repeat('m', 100) ELSE 'k' || lpad(i::text, 5, '0') END FROM generate_series(1, 300) i;
SELECT * FROM kc_skip('st_txt3', 't = ''zzz''');
-- the collation of the column decides: C and POSIX have statistics, a language collation has none
CREATE TABLE st_coll (id int, c text COLLATE "C", p text COLLATE "POSIX", d text) USING columnar;
SELECT kingres.columnar_set_options('st_coll', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO st_coll SELECT i, 'k' || lpad(i::text, 5, '0'), 'k' || lpad(i::text, 5, '0'), 'k' || lpad(i::text, 5, '0') FROM generate_series(1, 300) i;
SELECT * FROM kc_skip('st_coll', 'c = ''k00250''');
SELECT * FROM kc_skip('st_coll', 'p = ''k00250''');
SELECT * FROM kc_skip('st_coll', 'c COLLATE "POSIX" = ''k00250''');
SELECT * FROM kc_skip('st_coll', 'p COLLATE "C" = ''k00250''');
SELECT (SELECT groups_skipped FROM kc_skip('st_coll', 'd = ''k00250''')) =
       (SELECT CASE WHEN datcollate IN ('C', 'POSIX') THEN '2 of 3' ELSE '0 of 3' END FROM pg_database WHERE datname = current_database()) AS default_collation_rule;
SELECT count(*) AS same_answers FROM st_coll WHERE c = d OR c <> d;

-- floating point: NaN is the largest value, infinities are values; -0 and 0 are equal
CREATE TABLE st_f (id int, f8 float8, f4 float4, n numeric) USING columnar;
SELECT kingres.columnar_set_options('st_f', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO st_f SELECT i,
       CASE WHEN i = 150 THEN 'NaN' WHEN i = 250 THEN 'Infinity' WHEN i = 20 THEN '-Infinity' WHEN i = 21 THEN '-0' ELSE i::float8 END,
       CASE WHEN i = 150 THEN 'NaN' WHEN i = 250 THEN 'Infinity' WHEN i = 20 THEN '-Infinity' WHEN i = 21 THEN '-0' ELSE i::float4 END,
       CASE WHEN i = 150 THEN 'NaN' ELSE i::numeric END FROM generate_series(1, 300) i;
SELECT * FROM kc_skip('st_f', 'f8 > 1e300');
SELECT * FROM kc_skip('st_f', 'f8 = ''NaN''');
SELECT * FROM kc_skip('st_f', 'f8 = ''Infinity''');
SELECT * FROM kc_skip('st_f', 'f8 = ''-Infinity''');
SELECT * FROM kc_skip('st_f', 'f8 < -1e300');
SELECT * FROM kc_skip('st_f', 'f8 = 0');
SELECT * FROM kc_skip('st_f', 'f8 < 0');
SELECT * FROM kc_skip('st_f', 'f8 <= 0');
SELECT * FROM kc_skip('st_f', 'f8 BETWEEN 100.5 AND 120');
SELECT * FROM kc_skip('st_f', 'f8 > 300');
SELECT * FROM kc_skip('st_f', 'f4 = ''NaN''');
SELECT * FROM kc_skip('st_f', 'f4 > 299');
SELECT * FROM kc_skip('st_f', 'n = ''NaN''');
SELECT * FROM kc_skip('st_f', 'n > 1000');
SELECT * FROM kc_skip('st_f', 'n < 100');
SELECT * FROM kc_skip('st_f', 'f8 = 5::float4');

-- dates and timestamps, with infinity
CREATE TABLE st_ts (id int, d date, ts timestamp, tz timestamptz, iv interval, t time) USING columnar;
SELECT kingres.columnar_set_options('st_ts', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO st_ts SELECT i, CASE WHEN i = 250 THEN 'infinity' WHEN i = 50 THEN '-infinity' ELSE date '2000-01-01' + i END,
       CASE WHEN i = 250 THEN 'infinity' WHEN i = 50 THEN '-infinity' ELSE timestamp '2000-01-01 00:00:00' + i * interval '1 hour' END,
       CASE WHEN i = 250 THEN 'infinity' WHEN i = 50 THEN '-infinity' ELSE timestamptz '2000-01-01 00:00:00+00' + i * interval '1 hour' END,
       i * interval '1 day 1 second', time '00:00:00' + i * interval '3 minute'
  FROM generate_series(1, 300) i;
SELECT * FROM kc_skip('st_ts', 'd = ''2000-05-01''');
SELECT * FROM kc_skip('st_ts', 'd = ''infinity''');
SELECT * FROM kc_skip('st_ts', 'd > ''2001-01-01''');
SELECT * FROM kc_skip('st_ts', 'd < ''1999-01-01''');
SELECT * FROM kc_skip('st_ts', 'ts < ''2000-01-01 00:00:00''');
SELECT * FROM kc_skip('st_ts', 'ts = ''2000-01-09 05:00:00''');
SELECT * FROM kc_skip('st_ts', 'ts > ''2000-01-12 00:00:00''');
SELECT * FROM kc_skip('st_ts', 'tz = ''2000-01-09 05:00:00+00''');
SELECT * FROM kc_skip('st_ts', 'tz > ''2000-01-12 00:00:00+00''');
SELECT * FROM kc_skip('st_ts', 'tz = ''2000-01-09 05:00:00+00''::date');
SELECT * FROM kc_skip('st_ts', 'iv > ''250 days''');
SELECT * FROM kc_skip('st_ts', 't > ''14:30:00''');
SELECT * FROM kc_skip('st_ts', 'd < now() - interval ''50 years''');

-- the other types: a value from the last chunk group.  Statistics exist for the base types with a
-- B-tree ordering; not for arrays and ranges, and a boolean test (b = true) is simplified to b by the
-- planner.  The values of "char" repeat in every chunk group, so no chunk group can be skipped.
CREATE TABLE st_ty (id int, b bool, c "char", nm name, bt bytea, u uuid, ip inet, cd cidr, mac macaddr, bit bit(12), vb varbit, o oid, m money,
                    l pg_lsn, tt timetz, bp char(8) COLLATE "C", j jsonb, tv tsvector, a int[], r int4range) USING columnar;
SELECT kingres.columnar_set_options('st_ty', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO st_ty SELECT i, i > 150, chr(32 + i % 90)::"char", 'n' || lpad(i::text, 5, '0'), decode(lpad(to_hex(i), 6, '0'), 'hex'),
       ('00000000-0000-0000-0000-' || lpad(to_hex(i), 12, '0'))::uuid, ('10.0.' || i / 256 || '.' || i % 256)::inet, ('10.' || i / 256 || '.' || i % 256 || '.0/24')::cidr,
       ('08:00:2b:00:' || lpad(to_hex(i / 256), 2, '0') || ':' || lpad(to_hex(i % 256), 2, '0'))::macaddr,
       i::bit(12), (i::bit(12))::varbit, i::oid, (i * 100)::numeric::money, ('0/' || to_hex(i * 4096))::pg_lsn,
       ('00:00:00+00'::timetz + i * interval '1 minute'), lpad(i::text, 8, '0'), jsonb_build_object('k', i), to_tsvector('simple', 'w' || i), ARRAY[i], int4range(i, i + 1)
  FROM generate_series(1, 300) i;
CREATE FUNCTION kc_ty(col text) RETURNS TABLE (column_name text, matched bigint, groups_skipped text) LANGUAGE plpgsql AS $$
DECLARE lit text;
BEGIN
    EXECUTE format('SELECT quote_literal(%I::text) FROM st_ty WHERE id = 250', col) INTO lit;
    column_name := col;
    SELECT s.n, s.groups_skipped INTO matched, groups_skipped
      FROM kc_skip('st_ty', format('%I = %s::%s', col, lit, (SELECT format_type(atttypid, atttypmod) FROM pg_attribute
                                                                  WHERE attrelid = 'st_ty'::regclass AND attname = col))) s;
    RETURN NEXT;
END $$;
SELECT t.* FROM (SELECT attname FROM pg_attribute WHERE attrelid = 'st_ty'::regclass AND attnum > 0 ORDER BY attnum) a, LATERAL kc_ty(a.attname) t;

-- what columnar_info and columnar_stripes report
CREATE TABLE inf (a int, b text) USING columnar;
SELECT * FROM kingres.columnar_info('inf');
SELECT * FROM kingres.columnar_stripes('inf');
SELECT kingres.columnar_set_options('inf', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO inf SELECT i, repeat('abc', 10) || i FROM generate_series(1, 2500) i;
SELECT stripes, chunk_groups, rows, bytes_on_disk > 0 AS on_disk, bytes_uncompressed > bytes_on_disk AS smaller,
       round(compression_ratio::numeric, 1) > 1 AS ratio_above_one,
       abs(compression_ratio - bytes_uncompressed::float8 / bytes_on_disk) < 1e-9 AS ratio_is_the_quotient
  FROM kingres.columnar_info('inf');
SELECT stripe_id, first_row, row_count, chunk_groups, bytes_on_disk > 0 AS on_disk, bytes_uncompressed > 0 AS raw,
       written_by_xmin::text::bigint > 0 AS has_xmin
  FROM kingres.columnar_stripes('inf') ORDER BY stripe_id;
SELECT sum(row_count) AS rows, sum(chunk_groups) AS groups, count(DISTINCT stripe_id) AS stripes,
       sum(bytes_on_disk) = (SELECT bytes_on_disk FROM kingres.columnar_info('inf')) AS bytes_add_up
  FROM kingres.columnar_stripes('inf');
-- stripes written by one statement carry the same transaction, and the next statement adds new ones
INSERT INTO inf SELECT i, 'x' FROM generate_series(1, 10) i;
SELECT stripes, rows FROM kingres.columnar_info('inf');
SELECT count(DISTINCT written_by_xmin::text) AS distinct_writers FROM kingres.columnar_stripes('inf');
-- rows that still wait in the write buffer of the transaction are not a stripe yet
BEGIN;
INSERT INTO inf SELECT i, 'y' FROM generate_series(1, 7) i;
SELECT stripes, rows FROM kingres.columnar_info('inf');
ROLLBACK;
SELECT stripes, rows FROM kingres.columnar_info('inf');
SELECT count(*) AS stripes_listed FROM kingres.columnar_stripes('inf');
-- a table that was emptied
TRUNCATE inf;
SELECT * FROM kingres.columnar_info('inf');
SELECT count(*) FROM kingres.columnar_stripes('inf');

DROP TABLE st_int, st_null, st_txt, st_txt2, st_txt3, st_coll, st_f, st_ts, st_ty, inf;
DROP FUNCTION kc_scan(text), kc_skip(text, text), kc_ty(text);
SELECT count(*) AS catalog_rows FROM kingres.columnar_stripe;
