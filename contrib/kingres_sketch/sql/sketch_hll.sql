-- sketch_hll: the type hll, its functions and aggregates.
SET search_path = public, kingres_sketch;

-- empty sketches, precisions, errors
SELECT hll_empty(), hll_empty(4)::bytea, hll_empty(18)::bytea, hll_precision(hll_empty(12)),
       hll_is_empty(hll_empty()), hll_cardinality(hll_empty(16));
SELECT hll_empty(3);
SELECT hll_empty(19);
SELECT hll_empty(NULL) IS NULL AS null_precision;

-- a few elements: sparse bytes at precision 4 (4 bytes of header, 4 per register)
SELECT hll_add(hll_add(hll_empty(4), 1), 2)::bytea AS two_elements;
SELECT hll_add(hll_empty(4), NULL::int)::bytea AS null_element_ignored;
SELECT hll_add(NULL, 1) IS NULL AS null_sketch;
SELECT hll_add_hash(hll_empty(4), 0)::bytea AS hash_0, hll_add_hash(hll_empty(4), -1)::bytea AS hash_minus_1;

-- the dense form starts at 3 non-zero registers of precision 4 (4 * 3 is not less than 12)
SELECT length(hll_add_agg(i, 4)::bytea) AS len, hll_cardinality(hll_add_agg(i, 4)) AS card
  FROM generate_series(1, 2) i;
SELECT length(hll_add_agg(i, 4)::bytea) AS len, hll_cardinality(hll_add_agg(i, 4)) AS card
  FROM generate_series(1, 30) i;

-- estimates, exact for small sets and within the error for larger ones
SELECT n, hll_cardinality(s) AS card
  FROM (SELECT n, (SELECT hll_add_agg(i) FROM generate_series(1, n) i) AS s
          FROM unnest(ARRAY[0, 1, 2, 10, 100, 1000, 10000, 100000]) n) t
 ORDER BY n;
SELECT approx_count_distinct(i), approx_count_distinct(i, 12), approx_count_distinct(i, 18)
  FROM generate_series(1, 50000) i;

-- the hash: one value for equal integers of every width, text and varchar alike
SELECT hll_hash(42::int2) = hll_hash(42::int4) AS int2_int4,
       hll_hash(42::int4) = hll_hash(42::int8) AS int4_int8,
       hll_hash('kingres'::text) = hll_hash('kingres'::varchar) AS text_varchar,
       hll_hash(0.0::float8) = hll_hash('-0'::float8) AS zero_signs;
SELECT hll_hash(42), hll_hash('kingres'::text), hll_hash(true), hll_hash(1.5::numeric),
       hll_hash('123e4567-e89b-12d3-a456-426614174000'::uuid);
SELECT hll_hash('{}'::json);
SELECT hll_hash(NULL::int) IS NULL AS null_hash;

-- elements of other types
SELECT approx_count_distinct(x) FROM (VALUES ('a'), ('b'), ('a'), (NULL)) t(x);
SELECT approx_count_distinct(x::text::date) FROM (VALUES ('2026-01-01'), ('2026-01-02'), ('2026-01-01')) t(x);
SELECT approx_count_distinct(x) FROM (VALUES (1.0::numeric), (1.00::numeric), (2.5::numeric)) t(x);

-- NULLs are ignored; no rows give an empty sketch and 0
SELECT hll_cardinality(hll_add_agg(x)), approx_count_distinct(x)
  FROM (VALUES (NULL::int), (NULL), (5), (5)) t(x);
SELECT hll_add_agg(x) AS sketch, hll_precision(hll_add_agg(x, 10)) AS p10,
       hll_union_agg(NULL::hll) AS union_empty, approx_count_distinct(x) AS cnt
  FROM generate_series(1, 0) x;
SELECT hll_cardinality(hll_union_agg(s))
  FROM (VALUES (NULL::hll), (hll_add(hll_empty(), 1)), (NULL)) t(s);

-- precision of an aggregate: repeated by every row of a group
SELECT hll_precision(hll_add_agg(i, 10)) FROM generate_series(1, 100) i;
SELECT hll_add_agg(i, 10 + i % 2) FROM generate_series(1, 100) i;
SELECT hll_add_agg(i, 3) FROM generate_series(1, 100) i;

-- union: commutative, associative, idempotent, equal to the sketch of the union of the data
SELECT hll_union(a, b)::bytea = hll_union(b, a)::bytea AS commutative,
       hll_union(a, a)::bytea = a::bytea AS idempotent,
       hll_union(a, b)::bytea = (SELECT hll_add_agg(i) FROM generate_series(1, 7000) i)::bytea AS equals_whole
  FROM (SELECT (SELECT hll_add_agg(i) FROM generate_series(1, 5000) i) AS a,
               (SELECT hll_add_agg(i) FROM generate_series(3000, 7000) i) AS b) t;
SELECT hll_union(hll_empty(10), hll_empty(12));
SELECT hll_union(hll_empty(10), NULL) IS NULL AS null_union;

-- aggregation is independent of how rows are grouped and ordered: sketches
-- of the parts, united, equal the sketch of everything (also with duplicates)
SELECT hll_union_agg(part)::bytea = (SELECT hll_add_agg(i) FROM generate_series(1, 20000) i)::bytea AS parts_equal_whole
  FROM (SELECT hll_add_agg(i) AS part FROM generate_series(1, 20000) i, generate_series(1, 3) d GROUP BY i % 7) t;
SELECT (SELECT hll_add_agg(i ORDER BY i DESC) FROM generate_series(1, 5000) i)::bytea
     = (SELECT hll_add_agg(i) FROM generate_series(1, 5000) i)::bytea AS order_independent;
SELECT (SELECT hll_add_agg(i) FROM generate_series(1, 5000) i)::bytea
     = (SELECT hll_add_agg(DISTINCT i) FROM generate_series(1, 5000) i, generate_series(1, 2))::bytea AS distinct_same;

-- fold: the sketch of a lower precision, exactly
SELECT hll_precision(hll_fold(hll_add_agg(i, 14), 10)) AS p,
       hll_fold(hll_add_agg(i, 14), 10)::bytea = hll_add_agg(i, 10)::bytea AS fold_14_10,
       hll_fold(hll_add_agg(i, 18), 5)::bytea = hll_add_agg(i, 5)::bytea AS fold_18_5,
       hll_fold(hll_add_agg(i, 14), 14)::bytea = hll_add_agg(i, 14)::bytea AS fold_same
  FROM generate_series(1, 30000) i;
SELECT hll_fold(hll_add_agg(i, 14), 4)::bytea = hll_add_agg(i, 4)::bytea AS fold_sparse
  FROM generate_series(1, 5) i;
SELECT hll_fold(hll_empty(10), 12);
SELECT hll_fold(hll_empty(10), 3);
SELECT hll_cardinality(hll_union(hll_fold(hll_add_agg(i, 14), 12), hll_add_agg(i + 1000, 12)))
  FROM generate_series(1, 2000) i;

-- the same registers give the same bytes whatever path built them
SELECT (SELECT hll_add_agg(i) FROM generate_series(1, 4000) i)::bytea
     = (SELECT hll_union(hll_add_agg(i), hll_add_agg(i + 2000)) FROM generate_series(1, 2000) i)::bytea AS agg_vs_union,
       (SELECT hll_add_agg(i) FROM generate_series(1, 300) i)::bytea
     = (SELECT hll_add_hash(h, hll_hash(300)) FROM (SELECT hll_add_agg(i) AS h FROM generate_series(1, 299) i) t)::bytea AS agg_vs_add;

-- the bytea casts are validating
SELECT (h::bytea::hll)::bytea = h::bytea AS cast_round_trip, (h::text::hll)::bytea = h::bytea AS text_round_trip
  FROM (SELECT hll_add_agg(i, 6) AS h FROM generate_series(1, 3) i) t;

-- the aggregates as window functions (running and sliding frames), with FILTER and HAVING
SELECT i, approx_count_distinct(i % 7) OVER (ORDER BY i) AS running,
       hll_cardinality(hll_add_agg(i % 4) OVER (ORDER BY i ROWS BETWEEN 2 PRECEDING AND CURRENT ROW)) AS sliding
  FROM generate_series(1, 9) i;
SELECT i / 10 AS g, approx_count_distinct(i) FILTER (WHERE i % 2 = 0) AS even
  FROM generate_series(1, 20) i GROUP BY i / 10 HAVING approx_count_distinct(i) > 5 ORDER BY g;
-- the state functions cannot be called from SQL
DO $$ BEGIN PERFORM hll_agg_add(NULL, 1); EXCEPTION WHEN undefined_function THEN RAISE NOTICE 'undefined_function'; END $$;

-- a NULL precision is an error in every row, so the answer does not depend on the order of the rows
SELECT hll_precision(hll_add_agg(x, p)) FROM (VALUES ('a', 12), ('b', NULL::int)) v(x, p);
SELECT hll_precision(hll_add_agg(x, p)) FROM (VALUES ('a', NULL::int), ('b', 12)) v(x, p);
SELECT approx_count_distinct(x, p) FROM (VALUES (NULL::text, NULL::int)) v(x, p);
SELECT hll_precision(hll_add_agg(x, 12)) FROM (VALUES ('a'), (NULL)) v(x);
