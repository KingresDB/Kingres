-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_types: every family of built-in types, NULLs in every position,
-- wide rows, very large values and many columns.  Each table is compared
-- with the same data in a heap table, in both directions.
SET search_path = public;
CREATE TYPE ty_comp AS (a int, b text, c float8);
CREATE TYPE ty_nested AS (k int, inner_c ty_comp, arr int[]);
CREATE TYPE ty_enum AS ENUM ('red', 'green', 'blue', 'a longer enum label');
CREATE DOMAIN ty_dom AS int CHECK (VALUE > 0);
CREATE DOMAIN ty_domarr AS text[];

CREATE TABLE ty_heap (
    id int,
    c_bool bool, c_int2 int2, c_int4 int4, c_int8 int8,
    c_f4 float4, c_f8 float8, c_num numeric, c_num2 numeric(12, 3), c_money money,
    c_char "char", c_bpchar char(6), c_vchar varchar(30), c_text text, c_name name,
    c_bytea bytea,
    c_date date, c_time time, c_timetz timetz, c_ts timestamp, c_tstz timestamptz, c_iv interval,
    c_uuid uuid, c_inet inet, c_cidr cidr, c_mac macaddr, c_mac8 macaddr8,
    c_bit bit(7), c_vbit varbit(20),
    c_json json, c_jsonb jsonb, c_xml xml, c_oid oid, c_regclass regclass, c_xid8 xid8, c_lsn pg_lsn,
    c_tsv tsvector, c_tsq tsquery,
    c_i4r int4range, c_tsr tsrange, c_dr daterange, c_nr numrange, c_mr int4multirange,
    c_ia int4[], c_ta text[], c_na numeric[], c_2d int[], c_ja jsonb[],
    c_comp ty_comp, c_nest ty_nested, c_comparr ty_comp[],
    c_enum ty_enum, c_dom ty_dom, c_domarr ty_domarr,
    c_point point, c_line line, c_lseg lseg, c_box box, c_path path, c_poly polygon, c_circle circle,
    c_never_null int, c_always_null text
);

INSERT INTO ty_heap
SELECT g,
    g % 3 = 0, (g % 60000 - 30000)::int2, g * 7919, g::int8 * 1000003 * (CASE WHEN g % 2 = 0 THEN -1 ELSE 1 END),
    g / 8.0, g / 7.0, (g::numeric * g * g) / 13, (g * 1.337)::numeric(12, 3), (g * 12.34)::numeric::money,
    chr(33 + g % 90)::"char", 'ab' || g % 100, 'vc ' || g, 'text ' || repeat('t', g % 40) || g, 'nm' || g % 50,
    decode(lpad(to_hex(g), 8, '0'), 'hex') || (CASE WHEN g % 4 = 0 THEN ''::bytea ELSE '\x00ff'::bytea END),
    date '2000-01-01' + g, time '00:00:01' + g * interval '17 second', timetz '04:05:06+03' + g * interval '1 minute',
    timestamp '2001-02-03 04:05:06' + g * interval '1 hour 7 second',
    timestamptz '2001-02-03 04:05:06+00' + g * interval '3 hours 11 microsecond',
    make_interval(years => g % 5, months => g % 13, days => g % 29, secs => g / 3.0),
    ('00000000-0000-4000-8000-' || lpad(to_hex(g), 12, '0'))::uuid,
    ('10.' || g % 256 || '.' || (g / 256) % 256 || '.1/' || (16 + g % 17))::inet,
    ('10.' || g % 256 || '.0.0/16')::cidr,
    ('08:00:2b:01:' || lpad(to_hex(g % 256), 2, '0') || ':03')::macaddr,
    ('08:00:2b:ff:fe:01:' || lpad(to_hex(g % 256), 2, '0') || ':03')::macaddr8,
    (g % 128)::bit(7), (('x' || lpad(to_hex(g), 5, '0'))::bit(20)),
    ('{"k": ' || g || ', "s": "v' || g || '"}')::json,
    jsonb_build_object('k', g, 'arr', jsonb_build_array(1, 'two', NULL, g % 3 = 0), 'nested', jsonb_build_object('x', g / 2.0)),
    ('<r a="' || g || '"><c>t' || g % 10 || '</c></r>')::xml,
    g::oid, (CASE WHEN g % 2 = 0 THEN 'pg_class' ELSE 'pg_type' END)::regclass, g::text::xid8, ('0/' || to_hex(g * 4096))::pg_lsn,
    to_tsvector('simple', 'the quick fox ' || g || ' jumps'), to_tsquery('simple', 'fox & w' || g % 10),
    int4range(g, g + 10), tsrange(timestamp '2020-01-01' + g * interval '1 hour', timestamp '2020-01-01' + g * interval '1 hour' + interval '3 days'),
    daterange(date '2020-01-01' + g, NULL), numrange(g, g + 0.5, '[]'), int4multirange(int4range(g, g + 2), int4range(g + 5, g + 9)),
    ARRAY[g, g + 1, NULL, g % 5], ARRAY['a' || g, NULL, '', 'with "quote" and, comma'], ARRAY[g / 3.0, NULL],
    ARRAY[[g, 1], [2, g]], ARRAY['{"a": 1}'::jsonb, NULL],
    ROW(g, 'comp' || g, g / 9.0)::ty_comp, ROW(g, ROW(g, NULL, 0.5)::ty_comp, ARRAY[g, NULL])::ty_nested,
    ARRAY[ROW(g, 'x', 1.0)::ty_comp, NULL, ROW(NULL, NULL, NULL)::ty_comp],
    (enum_range(NULL::ty_enum))[1 + g % 4], 1 + g % 1000, ARRAY['d' || g, NULL]::ty_domarr,
    point(g, g / 2.0), line(point(g, 0), point(0, g + 1)), lseg(point(g, 1), point(2, g)), box(point(g, g), point(0, 0)),
    ('((' || g || ',1),(2,' || g || '),(3,3))')::path, ('((0,0),(' || g || ',1),(2,' || g + 5 || '))')::polygon, circle(point(g, g), 1 + g % 5),
    g, NULL
FROM generate_series(1, 3000) g;

-- NULLs in every position: each column but the last two gets NULL in the rows
-- whose number has a column-specific remainder; one extra row has only NULLs and row 7 has no id
DO $$
DECLARE
    r record;
    n int := 0;
BEGIN
    FOR r IN SELECT attname FROM pg_attribute
              WHERE attrelid = 'ty_heap'::regclass AND attnum > 1 AND NOT attisdropped
                AND attname NOT IN ('c_never_null', 'c_always_null') ORDER BY attnum
    LOOP
        n := n + 1;
        EXECUTE format('UPDATE ty_heap SET %I = NULL WHERE id %% %s = %s', r.attname, 5 + n % 7, n % 5);
    END LOOP;
END $$;
UPDATE ty_heap SET id = NULL WHERE id = 7;
INSERT INTO ty_heap (id) VALUES (NULL);
SELECT count(*) AS heap_rows,
       count(c_always_null) AS always_null_count, count(c_never_null) AS never_null_count
  FROM ty_heap;
-- every column has NULLs and non-NULLs, except the two named ones
SELECT count(*) AS columns_without_null_or_value FROM
  (SELECT (j.key), count(*) FILTER (WHERE j.value = 'null') AS nulls, count(*) AS n
     FROM ty_heap h, jsonb_each(to_jsonb(h)) j GROUP BY j.key) s
 WHERE (nulls = 0 OR nulls = n) AND key NOT IN ('c_never_null', 'c_always_null');

-- small stripes and chunk groups so the table has many of both
SET kingres_columnar.stripe_row_count = 1000;
SET kingres_columnar.chunk_row_count = 100;
CREATE TABLE ty_col USING columnar AS SELECT * FROM ty_heap;
RESET kingres_columnar.stripe_row_count;
RESET kingres_columnar.chunk_row_count;
SELECT stripes, chunk_groups, rows FROM kingres.columnar_info('ty_col');

-- rows compared as text (every type, one comparison), in both directions
SELECT count(*) AS columnar_only FROM
  (SELECT t::text FROM ty_col t EXCEPT ALL SELECT t::text FROM ty_heap t) x;
SELECT count(*) AS heap_only FROM
  (SELECT t::text FROM ty_heap t EXCEPT ALL SELECT t::text FROM ty_col t) x;
SELECT (SELECT count(*) FROM ty_col) = (SELECT count(*) FROM ty_heap) AS same_count;
-- the same with typed comparison of the types that have an equality operator
CREATE VIEW ty_cmp_cols AS SELECT 'id, c_bool, c_int2, c_int4, c_int8, c_f4, c_f8, c_num, c_num2, c_money, c_char, c_bpchar, c_vchar, c_text, c_name, c_bytea, c_date, c_time, c_timetz, c_ts, c_tstz, c_iv, c_uuid, c_inet, c_cidr, c_mac, c_mac8, c_bit, c_vbit, c_jsonb, c_oid, c_regclass, c_xid8, c_lsn, c_tsv, c_tsq, c_i4r, c_tsr, c_dr, c_nr, c_mr, c_ia, c_ta, c_na, c_2d, c_ja, c_comp, c_nest, c_comparr, c_enum, c_dom, c_domarr, c_never_null, c_always_null' AS cols;
SELECT cols FROM ty_cmp_cols \gset
SELECT count(*) AS typed_columnar_only FROM (SELECT :cols FROM ty_col EXCEPT ALL SELECT :cols FROM ty_heap) x;
SELECT count(*) AS typed_heap_only FROM (SELECT :cols FROM ty_heap EXCEPT ALL SELECT :cols FROM ty_col) x;
DROP VIEW ty_cmp_cols;
-- one column at a time: where it is NULL, where it is not, and a filter on it
SELECT count(*) AS null_pattern_differences FROM
  (SELECT h.id, h.c_text IS NULL AS hn, c.c_text IS NULL AS cn
     FROM ty_heap h FULL JOIN ty_col c ON c.id = h.id WHERE h.id IS NOT NULL OR c.id IS NOT NULL) x
 WHERE hn IS DISTINCT FROM cn;
SELECT count(*) FILTER (WHERE c_jsonb @> '{"k": 5}') AS jsonb_match, count(*) FILTER (WHERE c_tsv @@ 'fox'::tsquery) AS tsv_match,
       count(*) FILTER (WHERE c_i4r @> 100) AS range_match, count(*) FILTER (WHERE c_comp = ROW(10, 'comp10', 10 / 9.0)::ty_comp) AS comp_match
  FROM ty_col;
SELECT count(*) FILTER (WHERE c_jsonb @> '{"k": 5}') AS jsonb_match, count(*) FILTER (WHERE c_tsv @@ 'fox'::tsquery) AS tsv_match,
       count(*) FILTER (WHERE c_i4r @> 100) AS range_match, count(*) FILTER (WHERE c_comp = ROW(10, 'comp10', 10 / 9.0)::ty_comp) AS comp_match
  FROM ty_heap;
-- a projection of a few columns, a count over a column that is all NULL
SELECT count(c_always_null), count(c_never_null), sum(c_never_null) FROM ty_col;
SELECT c_enum, count(*) FROM ty_col GROUP BY c_enum ORDER BY c_enum;
SELECT c_enum, count(*) FROM ty_heap GROUP BY c_enum ORDER BY c_enum;

-- floating point extremes and special values
CREATE TABLE ty_f_heap (id serial, f4 float4, f8 float8, n numeric, t timestamptz, d date, i8 int8, iv interval);
INSERT INTO ty_f_heap (f4, f8, n, t, d, i8, iv) VALUES
  ('NaN', 'NaN', 'NaN', 'infinity', 'infinity', 9223372036854775807, 'infinity'),
  ('Infinity', 'Infinity', 'Infinity', '-infinity', '-infinity', -9223372036854775808, '-infinity'),
  ('-Infinity', '-Infinity', '-Infinity', '4713-01-01 BC', '4713-01-01 BC', 0, '-178000000 years'),
  (0, '-0'::float8, '0.000000000000000000000000000001', '294276-12-30 12:00:00+00', '5874897-12-31', 1, '178000000 years'),
  ('-0'::float4, 1.7976931348623157e308, '123456789012345678901234567890.123456789', now(), current_date, -1, '1 day 2 hours'),
  (3.4028235e38, 5e-324, '-123456789012345678901234567890.123456789', NULL, NULL, NULL, NULL),
  (1.4e-45, -1.7976931348623157e308, 1e100000, '2000-01-01', '2000-01-01', 42, '00:00:00.000001');
CREATE TABLE ty_f_col USING columnar AS SELECT * FROM ty_f_heap;
SELECT (SELECT count(*) FROM (SELECT * FROM ty_f_col EXCEPT ALL SELECT * FROM ty_f_heap) x) AS columnar_only,
       (SELECT count(*) FROM (SELECT * FROM ty_f_heap EXCEPT ALL SELECT * FROM ty_f_col) x) AS heap_only;
SELECT id, f4, f8, length(n::text) AS n_len, n < 1 AS n_small, t IS NULL AS t_null, i8 FROM ty_f_col ORDER BY id;
SELECT id, f8 FROM ty_f_col WHERE f8 = 'NaN' OR f8 > 1e300 ORDER BY id;
SELECT count(*) FROM ty_f_col WHERE f8 < 0 AND id IN (SELECT id FROM ty_f_heap WHERE f8 < 0);
SELECT count(*) FROM ty_f_col WHERE f4 = '-0';

-- many columns: 600 columns of several types, 700 rows, on two chunk groups
DO $$
DECLARE
    cols text := '';
    vals text := '';
    k int;
BEGIN
    FOR k IN 1..600 LOOP
        cols := cols || format(', w%s %s', k, (ARRAY['int', 'text', 'float8', 'bool', 'numeric', 'int8', 'bytea'])[1 + k % 7]);
        vals := vals || ', CASE WHEN (g + ' || k || ') % 11 = 0 THEN NULL ELSE ' ||
            replace((ARRAY['(g * K)::int', '(''t'' || g || ''_K'')', '(g / K.0)::float8', '(g % K = 0)',
                       '(g::numeric / K)', '(g::int8 * K * 100000)', 'decode(lpad(to_hex(g * K), 8, ''0''), ''hex'')'])[1 + k % 7], 'K', k::text) || ' END';
    END LOOP;
    EXECUTE 'CREATE TABLE wide_heap (id int' || cols || ')';
    EXECUTE 'INSERT INTO wide_heap SELECT g' || vals || ' FROM generate_series(1, 700) g';
END $$;
SELECT count(*) AS wide_columns FROM pg_attribute WHERE attrelid = 'wide_heap'::regclass AND attnum > 0;
SET kingres_columnar.chunk_row_count = 500;
CREATE TABLE wide_col USING columnar AS SELECT * FROM wide_heap;
RESET kingres_columnar.chunk_row_count;
SELECT (SELECT count(*) FROM (SELECT t::text FROM wide_col t EXCEPT ALL SELECT t::text FROM wide_heap t) x) AS columnar_only,
       (SELECT count(*) FROM (SELECT t::text FROM wide_heap t EXCEPT ALL SELECT t::text FROM wide_col t) x) AS heap_only;
SELECT sum(w7), sum(w9), count(w8), max(w593), count(w600) FROM wide_col;
SELECT sum(w7), sum(w9), count(w8), max(w593), count(w600) FROM wide_heap;
-- a projection of the first and the last column of a wide table
SELECT count(*), sum(id + w600 % 1000) FROM wide_col WHERE id > 100;
SELECT count(*), sum(id + w600 % 1000) FROM wide_heap WHERE id > 100;

-- the largest number of columns of a table
DO $$
BEGIN
    EXECUTE 'CREATE TABLE max_cols (' || (SELECT string_agg('c' || i || ' int', ', ') FROM generate_series(1, 1600) i) || ') USING columnar';
    EXECUTE 'INSERT INTO max_cols SELECT ' || (SELECT string_agg('g + ' || i, ', ') FROM generate_series(1, 1600) i)
            || ' FROM generate_series(1, 3000) g';
END $$;
SELECT count(*), sum(c1), sum(c800), sum(c1600) FROM max_cols;
SELECT count(*), sum(c1600) FROM max_cols WHERE c1 > 2000;
DROP TABLE max_cols;

-- wide rows: values of several megabytes, compressible and not, TOASTed in the source
CREATE TABLE big_heap (id int, t text, b bytea, j jsonb, ia int[], t2 text);
ALTER TABLE big_heap ALTER COLUMN t2 SET STORAGE EXTERNAL;
INSERT INTO big_heap
SELECT g,
       repeat('compressible text ' || g || ' ', 100000 * g),
       decode(string_agg(md5(i::text || g::text), ''), 'hex'),
       jsonb_build_object('id', g, 'blob', repeat('x', 1000000 * g)),
       ARRAY(SELECT x FROM generate_series(1, 300000 * g) x),
       string_agg(md5(i::text), '')
  FROM generate_series(1, 4) g, generate_series(1, 120000 * g) i GROUP BY g;
INSERT INTO big_heap VALUES (5, NULL, NULL, NULL, NULL, NULL), (6, '', '\x', '{}', '{}', repeat('e', 10));
SELECT id, pg_column_size(t) > 1000 AS t_big, length(t) > 1000000 AS t_long, length(b) >= 1920000 AS b_long,
       pg_column_size(t2) >= length(t2) AS t2_external
  FROM big_heap ORDER BY id;
CREATE TABLE big_col USING columnar AS SELECT * FROM big_heap;
SELECT rows FROM kingres.columnar_info('big_col');
SELECT (SELECT count(*) FROM (SELECT id, md5(t), md5(b), j, ia, md5(t2) FROM big_col EXCEPT ALL SELECT id, md5(t), md5(b), j, ia, md5(t2) FROM big_heap) x) AS columnar_only,
       (SELECT count(*) FROM (SELECT id, md5(t), md5(b), j, ia, md5(t2) FROM big_heap EXCEPT ALL SELECT id, md5(t), md5(b), j, ia, md5(t2) FROM big_col) x) AS heap_only;
SELECT id, length(t), length(b), length(t2), array_length(ia, 1), pg_column_size(t) > 0 AS sized FROM big_col ORDER BY id;
-- big values written by two statements of one transaction
BEGIN;
INSERT INTO big_col SELECT 7, repeat('z', 3000000), NULL, NULL, NULL, repeat('y', 2000000);
INSERT INTO big_col SELECT 8, repeat('z', 3000000), NULL, NULL, NULL, repeat('y', 2000000);
SELECT id, length(t), length(t2) FROM big_col WHERE id >= 7 ORDER BY id;
COMMIT;
SELECT id, length(t), length(t2) FROM big_col WHERE id >= 7 ORDER BY id;
-- the TOASTed values of the heap table, inserted again
INSERT INTO big_col SELECT * FROM big_heap WHERE id <= 2;
SELECT count(*) AS columnar_copy FROM big_col WHERE id <= 2;

-- UTF-8 text, empty strings, long strings and strings with special characters
CREATE TABLE ty_text_heap (s text);
INSERT INTO ty_text_heap VALUES ('plain'), (''), (NULL), ('caf' || chr(233)), (chr(8364) || chr(8364)), (chr(128512)),
  (E'tab\there'), (E'new\nline'), (E'back\\slash'), ('quote''s'), (repeat('long', 5000)), (repeat(chr(955), 2000)),
  ('a' || repeat(' ', 100)), (repeat('b', 100)), (repeat('b', 100) || 'c'), ('ABC'), ('abc'), ('Abc');
CREATE TABLE ty_text_col USING columnar AS SELECT * FROM ty_text_heap;
SELECT (SELECT count(*) FROM (SELECT * FROM ty_text_col EXCEPT ALL SELECT * FROM ty_text_heap) x) AS columnar_only,
       (SELECT count(*) FROM (SELECT * FROM ty_text_heap EXCEPT ALL SELECT * FROM ty_text_col) x) AS heap_only;
SELECT s FROM ty_text_col WHERE length(s) < 12 ORDER BY s COLLATE "C";

DROP TABLE ty_heap, ty_col, ty_f_heap, ty_f_col, wide_heap, wide_col, big_heap, big_col, ty_text_heap, ty_text_col;
DROP TYPE ty_nested; DROP TYPE ty_comp; DROP TYPE ty_enum; DROP DOMAIN ty_dom; DROP DOMAIN ty_domarr;
SELECT count(*) AS catalog_rows FROM kingres.columnar_stripe;
