-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- sketch_io: text and bytea input and output of the three types, and what
-- happens to values that are cut off, too long or corrupted.
SET search_path = public, kingres_sketch;

-- helpers that write the bytes of a value: little-endian integers and doubles
CREATE FUNCTION pg_temp.le(i bigint, n int) RETURNS text LANGUAGE sql IMMUTABLE AS
$$ SELECT string_agg(lpad(to_hex(((i >> (8 * b)) & 255)::int), 2, '0'), '' ORDER BY b)
     FROM generate_series(0, n - 1) b $$;
CREATE FUNCTION pg_temp.lef(x float8) RETURNS text LANGUAGE sql IMMUTABLE AS
$$ SELECT string_agg(substr(encode(float8send(x), 'hex'), 2 * i + 1, 2), '' ORDER BY 7 - i)
     FROM generate_series(0, 7) i $$;
-- a top-n value: capacity, total, counters (item length, count, error, item in hex)
CREATE FUNCTION pg_temp.tn(k bigint, total bigint, n bigint, entries text) RETURNS text LANGUAGE sql IMMUTABLE AS
$$ SELECT '\x4b010200' || pg_temp.le(k, 4) || pg_temp.le(total, 8) || pg_temp.le(n, 4) || entries $$;
CREATE FUNCTION pg_temp.tne(len bigint, count bigint, err bigint, itemhex text) RETURNS text LANGUAGE sql IMMUTABLE AS
$$ SELECT pg_temp.le(len, 2) || pg_temp.le(count, 8) || pg_temp.le(err, 8) || itemhex $$;
-- a t-digest value: compression, centroid count, total, min, max, centroids
CREATE FUNCTION pg_temp.td(c bigint, n bigint, total bigint, mn float8, mx float8, cents text) RETURNS text LANGUAGE sql IMMUTABLE AS
$$ SELECT '\x4b010300' || pg_temp.le(c, 4) || pg_temp.le(n, 4) || pg_temp.le(total, 8)
          || pg_temp.lef(mn) || pg_temp.lef(mx) || cents $$;
CREATE FUNCTION pg_temp.tdc(mean float8, w bigint) RETURNS text LANGUAGE sql IMMUTABLE AS
$$ SELECT pg_temp.lef(mean) || pg_temp.le(w, 8) $$;

-- text input of hll
SELECT name, (e).sql_error_code AS code, (e).message, (e).detail
  FROM (SELECT name, pg_input_error_info(v, 'kingres_sketch.hll') AS e
          FROM (VALUES
            ('not hex', 'abc'),
            ('no prefix', '4b010104'),
            ('odd digits', '\x4b01010'),
            ('bad digit', '\x4b0101zz'),
            ('empty', '\x'),
            ('short', '\x4b0101'),
            ('bad magic', '\x4c010104'),
            ('format 2', '\x4b020104'),
            ('format 0', '\x4b000104'),
            ('kind topn', '\x4b010204'),
            ('precision 3', '\x4b010103'),
            ('precision 19', '\x4b010113'),
            ('sparse, not sorted', '\x4b0101044100000001000000'),
            ('sparse, same register', '\x4b0101040100000002000000'),
            ('sparse, rank 0', '\x4b01010400000000'),
            ('sparse, rank 62', '\x4b0101043e000000'),
            ('sparse, register 16', '\x4b01010401040000'),
            ('sparse, 5 bytes', '\x4b0101040100000000'),
            ('dense, all zero', '\x4b010104000000000000000000000000'),
            ('dense, 11 bytes', '\x4b0101044110000000000000000000'),
            ('dense, rank 62', '\x4b0101043e1000000000000000000000'),
            ('too long', '\x' || repeat('00', 200000))
          ) t(name, v)) s
 ORDER BY name;
SELECT v, pg_input_is_valid(v, 'kingres_sketch.hll') AS valid
  FROM (VALUES ('\x4b010104'), ('\X4B010104'), ('\x4b01010401000000'), ('\x4b010104411000000000000000000000')) t(v);

-- text and binary round trip; the output is lower-case hex
SELECT '\x4B01010E'::hll, '\x4b01010401000000'::hll::bytea, ('\x4b010104411000000000000000000000'::hll)::text;
SELECT hll_cardinality('\x4b010104411000000000000000000000'), hll_precision('\x4b010104411000000000000000000000');

-- bytea casts: validating, SQLSTATE 22023
CREATE FUNCTION pg_temp.try(b bytea, t text) RETURNS text LANGUAGE plpgsql AS
$$ BEGIN
     EXECUTE format('SELECT %L::bytea::%s', b, t);
     RETURN 'valid';
   EXCEPTION WHEN OTHERS THEN
     RETURN SQLSTATE || ': ' || SQLERRM;
   END $$;
SELECT pg_temp.try('\x4b010104', 'kingres_sketch.hll'), pg_temp.try('\x4b0101', 'kingres_sketch.hll'),
       pg_temp.try('\x4c010104', 'kingres_sketch.hll'), pg_temp.try('\x4b020104', 'kingres_sketch.hll'),
       pg_temp.try('\x4b010104ff', 'kingres_sketch.hll'), pg_temp.try('\x', 'kingres_sketch.topn');
SELECT pg_temp.try(decode(repeat('00', 200000), 'hex'), 'kingres_sketch.hll');

-- text input of topn: one valid value and its variations
SELECT pg_temp.tn(3, 1, 1, pg_temp.tne(1, 1, 0, '61'))::topn::bytea AS valid_one_item,
       (SELECT item FROM topn_list(pg_temp.tn(3, 1, 1, pg_temp.tne(1, 1, 0, '61'))::topn)) AS item;
SELECT name, (e).sql_error_code AS code, (e).message, (e).detail
  FROM (SELECT name, pg_input_error_info(v, 'kingres_sketch.topn') AS e
          FROM (VALUES
            ('too short', '\x4b0102'),
            ('header only', '\x4b010200'),
            ('format 2', '\x4b020200' || pg_temp.le(3, 4) || pg_temp.le(0, 8) || pg_temp.le(0, 4)),
            ('reserved byte', '\x4b010201' || pg_temp.le(3, 4) || pg_temp.le(0, 8) || pg_temp.le(0, 4)),
            ('capacity 0', pg_temp.tn(0, 0, 0, '')),
            ('capacity 100001', pg_temp.tn(100001, 0, 0, '')),
            ('negative total', pg_temp.tn(3, -1, 0, '')),
            ('count above capacity', pg_temp.tn(1, 2, 2, pg_temp.tne(1, 1, 0, '61') || pg_temp.tne(1, 1, 0, '62'))),
            ('count 2^32 with no bytes', pg_temp.tn(3, 1, 4294967295, '')),
            ('counter cut off', pg_temp.tn(3, 1, 1, '0100')),
            ('item length cut off', pg_temp.tn(3, 1, 1, pg_temp.tne(5, 1, 0, '61'))),
            ('item length 1025', pg_temp.tn(3, 1, 1, pg_temp.tne(1025, 1, 0, repeat('61', 1025)))),
            ('count 0', pg_temp.tn(3, 1, 1, pg_temp.tne(1, 0, 0, '61'))),
            ('negative count', pg_temp.tn(3, 1, 1, pg_temp.tne(1, -1, 0, '61'))),
            ('error equal to count', pg_temp.tn(3, 2, 1, pg_temp.tne(1, 1, 1, '61'))),
            ('negative error', pg_temp.tn(3, 2, 1, pg_temp.tne(1, 1, -1, '61'))),
            ('count above total', pg_temp.tn(3, 1, 1, pg_temp.tne(1, 2, 0, '61'))),
            ('counts above total', pg_temp.tn(3, 2, 2, pg_temp.tne(1, 2, 0, '61') || pg_temp.tne(1, 1, 0, '62'))),
            ('error above total / capacity', pg_temp.tn(3, 5, 1, pg_temp.tne(1, 5, 2, '61'))),
            ('item with a zero byte', pg_temp.tn(3, 1, 1, pg_temp.tne(1, 1, 0, '00'))),
            ('same item twice', pg_temp.tn(3, 2, 2, pg_temp.tne(1, 1, 0, '61') || pg_temp.tne(1, 1, 0, '61'))),
            ('counts ascending', pg_temp.tn(3, 3, 2, pg_temp.tne(1, 1, 0, '61') || pg_temp.tne(1, 2, 0, '62'))),
            ('equal counts, items descending', pg_temp.tn(3, 2, 2, pg_temp.tne(1, 1, 0, '62') || pg_temp.tne(1, 1, 0, '61'))),
            ('trailing byte', pg_temp.tn(3, 1, 1, pg_temp.tne(1, 1, 0, '61') || '00'))
          ) t(name, v)) s
 ORDER BY name;
SELECT name, pg_input_is_valid(v, 'kingres_sketch.topn') AS valid
  FROM (VALUES
    ('empty', pg_temp.tn(3, 0, 0, '')),
    ('two items', pg_temp.tn(3, 3, 2, pg_temp.tne(1, 2, 0, '62') || pg_temp.tne(1, 1, 0, '61'))),
    ('empty item', pg_temp.tn(3, 1, 1, pg_temp.tne(0, 1, 0, ''))),
    ('longest item', pg_temp.tn(3, 1, 1, pg_temp.tne(1024, 1, 0, repeat('61', 1024)))),
    ('error = total / capacity', pg_temp.tn(2, 5, 1, pg_temp.tne(1, 5, 2, '61')))
  ) t(name, v) ORDER BY name;

-- text input of tdigest
SELECT pg_temp.td(100, 2, 3, 1, 5, pg_temp.tdc(1, 1) || pg_temp.tdc(3.5, 2))::tdigest::bytea AS valid,
       tdigest_count(pg_temp.td(100, 2, 3, 1, 5, pg_temp.tdc(1, 1) || pg_temp.tdc(3.5, 2))::tdigest);
SELECT name, (e).sql_error_code AS code, (e).message, (e).detail
  FROM (SELECT name, pg_input_error_info(v, 'kingres_sketch.tdigest') AS e
          FROM (VALUES
            ('too short', '\x4b0103'),
            ('format 2', '\x4b020300' || repeat('00', 32)),
            ('kind 4 is no value', '\x4b010400' || pg_temp.le(100, 4) || repeat('00', 36)),
            ('reserved byte', '\x4b010301' || pg_temp.le(100, 4) || repeat('00', 28)),
            ('compression 9', pg_temp.td(9, 0, 0, 0, 0, '')),
            ('compression 10001', pg_temp.td(10001, 0, 0, 0, 0, '')),
            ('too many centroids', pg_temp.td(10, 23, 23, 0, 1, repeat(pg_temp.tdc(0.5, 1), 23))),
            ('centroid count does not fit the length', pg_temp.td(100, 3, 3, 0, 1, pg_temp.tdc(0.5, 1))),
            ('trailing bytes', pg_temp.td(100, 1, 1, 0, 1, pg_temp.tdc(0.5, 1) || '00')),
            ('empty with a total', pg_temp.td(100, 0, 1, 0, 0, '')),
            ('centroids with no total', pg_temp.td(100, 1, 0, 0, 1, pg_temp.tdc(0.5, 1))),
            ('empty with a minimum', pg_temp.td(100, 0, 0, 1, 0, '')),
            ('negative total', pg_temp.td(100, 1, -1, 0, 1, pg_temp.tdc(0.5, 1))),
            ('weights do not add up', pg_temp.td(100, 2, 5, 0, 1, pg_temp.tdc(0.2, 1) || pg_temp.tdc(0.5, 1))),
            ('weight 0', pg_temp.td(100, 2, 1, 0, 1, pg_temp.tdc(0.2, 0) || pg_temp.tdc(0.5, 1))),
            ('negative weight', pg_temp.td(100, 2, 1, 0, 1, pg_temp.tdc(0.2, -1) || pg_temp.tdc(0.5, 2))),
            ('weights overflow', pg_temp.td(100, 2, 9223372036854775807, 0, 1,
                                           pg_temp.tdc(0.2, 9223372036854775807) || pg_temp.tdc(0.5, 1))),
            ('means not sorted', pg_temp.td(100, 2, 2, 0, 1, pg_temp.tdc(0.7, 1) || pg_temp.tdc(0.5, 1))),
            ('mean below minimum', pg_temp.td(100, 1, 1, 0.6, 1, pg_temp.tdc(0.5, 1))),
            ('mean above maximum', pg_temp.td(100, 1, 1, 0, 0.4, pg_temp.tdc(0.5, 1))),
            ('minimum above maximum', pg_temp.td(100, 1, 1, 2, 1, pg_temp.tdc(1.5, 1))),
            ('negative zero minimum', pg_temp.td(100, 1, 1, '-0'::float8, 1, pg_temp.tdc(0.5, 1))),
            ('negative zero maximum', pg_temp.td(100, 1, 1, -1, '-0'::float8, pg_temp.tdc(-0.5, 1))),
            ('negative zero mean', pg_temp.td(100, 1, 1, -1, 1, pg_temp.tdc('-0'::float8, 1))),
            ('NaN mean', pg_temp.td(100, 1, 1, 0, 1, pg_temp.tdc('NaN', 1))),
            ('infinite mean', pg_temp.td(100, 1, 1, 0, 1, pg_temp.tdc('Infinity', 1))),
            ('NaN maximum', pg_temp.td(100, 1, 1, 0, 'NaN', pg_temp.tdc(0.5, 1))),
            ('infinite minimum', pg_temp.td(100, 1, 1, '-Infinity', 1, pg_temp.tdc(0.5, 1))),
            ('too long', '\x' || repeat('00', 400000))
          ) t(name, v)) s
 ORDER BY name;
SELECT name, pg_input_is_valid(v, 'kingres_sketch.tdigest') AS valid
  FROM (VALUES
    ('empty', pg_temp.td(100, 0, 0, 0, 0, '')),
    ('one centroid', pg_temp.td(100, 1, 7, 1, 2, pg_temp.tdc(1.5, 7))),
    ('equal means', pg_temp.td(100, 2, 2, 1, 1, pg_temp.tdc(1, 1) || pg_temp.tdc(1, 1))),
    ('most centroids', pg_temp.td(10, 22, 22, 0, 1, repeat(pg_temp.tdc(0.5, 1), 22)))
    ,('zero minimum and mean', pg_temp.td(100, 1, 1, 0, 1, pg_temp.tdc(0, 1)))
  ) t(name, v) ORDER BY name;

-- binary input is the same validation
SELECT pg_temp.try('\x4b0103', 'kingres_sketch.tdigest'), pg_temp.try(decode(substr(pg_temp.td(100, 1, 1, 0, 1, pg_temp.tdc(0.5, 1)), 3), 'hex'), 'kingres_sketch.tdigest'),
       pg_temp.try(decode(substr(pg_temp.tn(3, 1, 1, pg_temp.tne(1, 0, 0, '61')), 3), 'hex'), 'kingres_sketch.topn');

CREATE TABLE sketch_io_t (h kingres_sketch.hll, t kingres_sketch.topn, d kingres_sketch.tdigest);
INSERT INTO sketch_io_t VALUES (hll_empty(4), topn_empty(2), tdigest_empty(10));
SELECT hll_cardinality(h), topn_total(t), tdigest_count(d) FROM sketch_io_t;
DROP TABLE sketch_io_t;

-- negative zero is stored as zero: a value with the bit pattern of -0 in a
-- minimum, a maximum, a mean or the fraction of a state is refused, so the
-- bytes of a digest are a function of its content
WITH b AS (SELECT tdigest_to_bytea(tdigest_agg(0.0::float8)) AS v),
     c AS (SELECT set_byte(set_byte(v, 27, 128), 43, 128) AS v FROM b)
SELECT pg_temp.try(v, 'kingres_sketch.tdigest') AS minus_zero_min_and_mean FROM c;
WITH b AS (SELECT tdigest_to_bytea(tdigest_agg(0.0::float8)) AS v)
SELECT pg_temp.try(set_byte(v, 35, 128), 'kingres_sketch.tdigest') AS minus_zero_max FROM b;
SELECT tdigest_to_bytea(tdigest_add(tdigest_empty(), '-0'::float8)) = tdigest_to_bytea(tdigest_add(tdigest_empty(), 0)) AS built_as_zero,
       tdigest_to_bytea(tdigest_agg(x)) = tdigest_to_bytea(tdigest_agg(abs(x))) AS aggregate_as_zero
  FROM (VALUES ('-0'::float8), (0)) v(x);
SELECT sketch_kind(set_byte(set_byte(v, 2, 4), 3, 0) || '\x0000000000000080'::bytea) AS kind4_minus_zero_fraction
  FROM (SELECT tdigest_to_bytea(tdigest_agg(1.0::float8)) AS v) b;
