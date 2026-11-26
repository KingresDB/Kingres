-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- sketch_hash: the hash of an element depends on the value only, never on an
-- object identifier, so a sketch is the same in another database or after a
-- restore.  Enums hash by label, containers by their elements, and the types
-- whose hash is an OID are refused.
SET search_path = public, kingres_sketch;

CREATE TYPE colr AS ENUM ('r', 'g', 'b');
-- the same labels in another order and another type: other OIDs
CREATE TYPE colr2 AS ENUM ('x', 'b', 'g', 'r');
CREATE TYPE colr_range AS RANGE (subtype = colr);
CREATE TYPE colr2_range AS RANGE (subtype = colr2);
CREATE TYPE pt AS (a int, c colr, t text);
CREATE TYPE pt2 AS (a int, c colr2, t text);
CREATE DOMAIN dcolr AS colr;

-- enum: by label
SELECT hll_hash('r'::colr) = hll_hash('r'::colr2) AS same_label_other_type,
       hll_hash('r'::colr) <> hll_hash('g'::colr) AS other_label,
       hll_hash('r'::dcolr) = hll_hash('r'::colr) AS domain_is_base;
SELECT hll_cardinality(hll_add(hll_add(hll_empty(), 'r'::colr), 'r'::colr)) AS one_value;

-- arrays: by dimensions and elements
SELECT hll_hash(ARRAY['r', 'g']::colr[]) = hll_hash(ARRAY['r', 'g']::colr2[]) AS enum_array,
       hll_hash(ARRAY['r', 'g']::colr[]) <> hll_hash(ARRAY['g', 'r']::colr[]) AS order_matters,
       hll_hash('{{1,2},{3,4}}'::int[]) <> hll_hash('{1,2,3,4}'::int[]) AS dimensions_matter,
       hll_hash('{1,NULL}'::int[]) <> hll_hash('{1}'::int[]) AS null_element,
       hll_hash('{}'::int[]) <> hll_hash('{NULL}'::int[]) AS empty_array,
       hll_hash(ARRAY[ARRAY['r']]::colr[]) = hll_hash(ARRAY[ARRAY['r']]::colr2[]) AS nested_enum_array;

-- ranges and multiranges
SELECT hll_hash('[g,g]'::colr_range) = hll_hash('[g,g]'::colr2_range) AS enum_range,
       hll_hash('[r,g]'::colr_range) <> hll_hash('[g,g]'::colr_range) AS other_enum_range,
       hll_hash('[1,5)'::int4range) = hll_hash('[1,4]'::int4range) AS canonical_range,
       hll_hash('[1,5)'::int4range) <> hll_hash('[1,6)'::int4range) AS other_range,
       hll_hash('empty'::int4range) <> hll_hash('[1,2)'::int4range) AS empty_range,
       hll_hash('(,5)'::int4range) <> hll_hash('(,6)'::int4range) AS unbounded,
       hll_hash('{[1,3),[5,7)}'::int4multirange) <> hll_hash('{[1,3)}'::int4multirange) AS multirange;

-- composite types and records
SELECT hll_hash(ROW(1, 'r', 'x')::pt) = hll_hash(ROW(1, 'r', 'x')::pt2) AS enum_in_composite,
       hll_hash(ROW(1, 'r', 'x')::pt) <> hll_hash(ROW(1, 'g', 'x')::pt) AS field_matters,
       hll_hash(ROW(1, NULL, 'x')::pt) <> hll_hash(ROW(1, 'r', 'x')::pt) AS null_field,
       hll_hash(ROW(1, 'r'::colr)) = hll_hash(ROW(1, 'r'::colr2)) AS anonymous_record,
       hll_hash(ARRAY[ROW(1, 'r', 'x')::pt]) = hll_hash(ARRAY[ROW(1, 'r', 'x')::pt2]) AS composite_array;

-- equal values are one element
SELECT hll_hash('a'::char(3)) = hll_hash('a'::char(5)) AS bpchar,
       hll_hash(1.5::numeric) = hll_hash(1.50::numeric) AS numeric_scale,
       hll_hash(42::int2) = hll_hash(42::int8) AS integers;

-- a sketch built from an enum column counts by label
CREATE TABLE ev (c colr, c2 colr2);
INSERT INTO ev VALUES ('r', 'r'), ('g', 'g'), ('b', 'b'), ('r', 'x');
SELECT (SELECT hll_add_agg(c)::bytea FROM ev) = (SELECT hll_add_agg(c2)::bytea FROM ev WHERE c2 <> 'x') AS same_bytes_by_label;
SELECT hll_cardinality(hll_add_agg(c)), approx_count_distinct(c2), approx_count_distinct(ARRAY[c]) FROM ev;

-- types whose hash depends on an object identifier are refused, also for NULL
CREATE FUNCTION pg_temp.state(sql text) RETURNS text LANGUAGE plpgsql AS
$$ BEGIN
     EXECUTE sql;
     RETURN 'accepted';
   EXCEPTION WHEN OTHERS THEN
     RETURN SQLSTATE;
   END $$;
SELECT t, pg_temp.state(format('SELECT hll_add(hll_empty(4), NULL::%s)', t)) AS hll_add_null,
          pg_temp.state(format('SELECT hll_hash(%s)', v)) AS hll_hash_value
  FROM (VALUES ('oid', '1::oid'), ('regclass', '''pg_class''::regclass'), ('regtype', '''int4''::regtype'),
               ('regproc', '''now''::regproc'), ('oidvector', '''1 2''::oidvector'),
               ('oid[]', 'ARRAY[1::oid]'), ('regclass[]', 'ARRAY[''pg_class''::regclass]'),
               ('colr', '''r''::colr'), ('colr[]', 'ARRAY[''r'']::colr[]'),
               ('colr_range', '''[g,g]''::colr_range'), ('int4', '1'), ('text', '''a''::text')) x(t, v);
SELECT pg_temp.state('SELECT hll_add_agg(c) FROM (SELECT 1::oid c WHERE false) s') AS agg_over_no_rows,
       pg_temp.state('SELECT approx_count_distinct(c) FROM (SELECT 1::oid c) s') AS agg_over_a_row,
       pg_temp.state('SELECT hll_add_agg(ROW(1::int, 1::oid)) FROM (SELECT 1) s') AS composite_with_oid;
SELECT hll_hash(1::oid);
SELECT hll_add(hll_empty(), ROW(1, 'pg_class'::regclass));
SELECT hll_add(hll_empty(), ARRAY[1::oid]);
SELECT hll_hash('{}'::json);

-- every type of pg_catalog that can be an element: accepted, or refused with
-- 0A000 only for a type whose hash is an OID, or has no hash function (42883)
CREATE TABLE type_verdict AS
SELECT t.oid::regtype::text AS typ, t.typname, t.typtype,
       pg_temp.state(format('SELECT hll_add(hll_empty(4), NULL::%s)', t.oid::regtype)) AS verdict
  FROM pg_type t
 WHERE t.typnamespace = 'pg_catalog'::regnamespace AND t.typisdefined
   AND t.typtype IN ('b', 'e', 'r', 'm', 'd')
   AND t.typname NOT LIKE '\_%' AND t.typname <> 'unknown';
SELECT verdict, count(*) > 0 AS any_types FROM type_verdict GROUP BY verdict ORDER BY verdict;
SELECT count(*) > 40 AS many_types_accepted FROM type_verdict WHERE verdict = 'accepted';
-- the refused types are exactly the OID ones
SELECT array_agg(typ ORDER BY typ) FILTER (WHERE typname !~ '^(oid|oidvector|reg.*)$') AS unexpected_refused,
       count(*) FILTER (WHERE typname ~ '^(oid|oidvector|reg.*)$') > 8 AS oid_types_refused
  FROM type_verdict WHERE verdict = '0A000';
-- and the OID types are all refused
SELECT array_agg(typ ORDER BY typ) AS oid_types_not_refused
  FROM type_verdict WHERE typname ~ '^(oid|oidvector|reg.*)$' AND verdict <> '0A000';
-- the same for arrays of every type
CREATE TABLE array_verdict AS
SELECT t.typname, pg_temp.state(format('SELECT hll_add(hll_empty(4), NULL::%s)', t.oid::regtype)) AS verdict
  FROM pg_type t JOIN pg_type e ON e.oid = t.typelem
 WHERE t.typnamespace = 'pg_catalog'::regnamespace AND t.typisdefined AND t.typtype = 'b'
   AND t.typlen = -1 AND t.typname LIKE '\_%' AND e.typtype <> 'c';
SELECT array_agg(typname ORDER BY typname) FILTER (WHERE typname !~ '^_(oid|oidvector|reg.*)$') AS unexpected_refused_arrays
  FROM array_verdict WHERE verdict = '0A000';
SELECT array_agg(typname ORDER BY typname) AS oid_arrays_not_refused
  FROM array_verdict WHERE typname ~ '^_(oid|oidvector|reg.*)$' AND verdict <> '0A000';

DROP TABLE ev, type_verdict, array_verdict;
DROP DOMAIN dcolr;
DROP TYPE pt, pt2, colr_range, colr2_range, colr, colr2;
