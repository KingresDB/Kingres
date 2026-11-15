-- sketch_tdigest: the type tdigest, its functions and aggregates.
SET search_path = public, kingres_sketch;

-- empty digests
SELECT tdigest_empty()::bytea AS empty_default, tdigest_compression(tdigest_empty()),
       tdigest_count(tdigest_empty()), tdigest_min(tdigest_empty()), tdigest_max(tdigest_empty()),
       tdigest_quantile(tdigest_empty(), 0.5), tdigest_rank(tdigest_empty(), 1),
       tdigest_quantile(tdigest_empty(), ARRAY[0.5, 0.9]);
SELECT tdigest_empty(9);
SELECT tdigest_empty(10001);
SELECT tdigest_compression(tdigest_empty(10)), tdigest_compression(tdigest_empty(10000));

-- a small set is kept exactly (one centroid per value): quantiles interpolate
SELECT tdigest_count(d), tdigest_min(d), tdigest_max(d),
       tdigest_quantile(d, 0), tdigest_quantile(d, 0.5), tdigest_quantile(d, 1),
       tdigest_quantile(d, 0.25), tdigest_quantile(d, 0.9),
       tdigest_quantile(d, ARRAY[0, 0.25, 0.5, 0.75, 1, NULL])
  FROM (SELECT tdigest_agg(i::float8) AS d FROM generate_series(1, 100) i) t;
SELECT tdigest_rank(d, 0), tdigest_rank(d, 1), tdigest_rank(d, 50), tdigest_rank(d, 50.5),
       tdigest_rank(d, 100), tdigest_rank(d, 1000), tdigest_rank(d, 'Infinity'), tdigest_rank(d, '-Infinity')
  FROM (SELECT tdigest_agg(i::float8) AS d FROM generate_series(1, 100) i) t;

-- one value, ties
SELECT tdigest_quantile(d, 0.5), tdigest_rank(d, 7), tdigest_rank(d, 6.9), tdigest_rank(d, 7.1)
  FROM (SELECT tdigest_agg(7::float8) AS d FROM generate_series(1, 5)) t;

-- errors and NULLs
SELECT tdigest_quantile(tdigest_empty(), -0.1);
SELECT tdigest_quantile(tdigest_add(tdigest_empty(), 1), 1.1);
SELECT tdigest_quantile(tdigest_add(tdigest_empty(), 1), 'NaN'::float8);
SELECT tdigest_quantile(tdigest_add(tdigest_empty(), 1), ARRAY[0.5, 2]);
SELECT tdigest_add(tdigest_empty(), 'NaN');
SELECT tdigest_add(tdigest_empty(), 'Infinity');
SELECT tdigest_add(tdigest_empty(), '-Infinity');
SELECT tdigest_add(tdigest_empty(), 1, 0);
SELECT tdigest_add(tdigest_empty(), 1, -5);
SELECT tdigest_rank(tdigest_add(tdigest_empty(), 1), 'NaN'::float8);
SELECT tdigest_add(tdigest_empty(), NULL)::bytea = tdigest_empty()::bytea AS null_value_ignored;
SELECT tdigest_add(NULL, 1) IS NULL AS null_digest;
SELECT tdigest_count(tdigest_agg(x)), tdigest_min(tdigest_agg(x))
  FROM (VALUES (1.0::float8), (NULL), (3.0)) t(x);
SELECT tdigest_agg(x) IS NOT NULL AS empty_digest, tdigest_count(tdigest_agg(x)),
       approx_percentile(0.5, x) FROM generate_series(1, 0) x;
SELECT tdigest_agg('NaN'::float8);

-- weights
SELECT tdigest_count(d), tdigest_quantile(d, 0.5), tdigest_min(d), tdigest_max(d)
  FROM (SELECT tdigest_weighted_agg(x::float8, w, 100) AS d FROM (VALUES (1, 1), (2, 1), (3, 8)) t(x, w)) s;
SELECT tdigest_count(tdigest_add(tdigest_add(tdigest_empty(), 5, 3), 6, 4));
SELECT tdigest_count(tdigest_add(tdigest_add(tdigest_empty(), 5, 9223372036854775807), 6, 1));
SELECT tdigest_weighted_agg(1::float8, 0, 100);
SELECT tdigest_count(tdigest_weighted_agg(x::float8, NULL, 100)) FROM generate_series(1, 3) x;

-- compression of an aggregate: repeated by every row of a group
SELECT tdigest_compression(tdigest_agg(i::float8, 20)) FROM generate_series(1, 10) i;
SELECT tdigest_agg(i::float8, 20 + i % 2) FROM generate_series(1, 10) i;
SELECT tdigest_agg(i::float8, 5) FROM generate_series(1, 10) i;

-- -0 and 0 are one value
SELECT tdigest_add(tdigest_empty(), '-0'::float8)::bytea = tdigest_add(tdigest_empty(), 0)::bytea AS zero_signs;

-- union: weights add; the compression must be the same
SELECT tdigest_count(u), tdigest_min(u), tdigest_max(u), tdigest_quantile(u, 0.5)
  FROM (SELECT tdigest_union((SELECT tdigest_agg(i::float8) FROM generate_series(1, 50) i),
                             (SELECT tdigest_agg(i::float8) FROM generate_series(51, 100) i)) AS u) t;
SELECT tdigest_union(tdigest_empty(50), tdigest_empty(100));
SELECT tdigest_union(tdigest_empty(), NULL) IS NULL AS null_union;
SELECT tdigest_count(tdigest_union_agg(d)), tdigest_quantile(tdigest_union_agg(d), 0.5)
  FROM (SELECT tdigest_agg(i::float8) AS d FROM generate_series(1, 90) i GROUP BY i % 3) t;
SELECT tdigest_union_agg(NULL::tdigest) IS NOT NULL AS empty_union;

-- approx_percentile: the fraction is the first argument and part of the state
SELECT approx_percentile(0.5, i::float8), approx_percentile(0, i::float8),
       approx_percentile(1, i::float8), approx_percentile(0.5, i::float8, 50)
  FROM generate_series(1, 99) i;
SELECT approx_percentile(0.5 + i / 1000.0, i::float8) FROM generate_series(1, 3) i;
SELECT approx_percentile(1.5, i::float8) FROM generate_series(1, 3) i;
SELECT approx_percentile(NULL, i::float8) FROM generate_series(1, 3) i;
SELECT approx_percentile(0.5, i::float8) FROM generate_series(1, 3) i WHERE i > 5;

-- a larger input stays within the number of centroids and the rank error
SELECT tdigest_count(d), tdigest_min(d), tdigest_max(d),
       (get_byte(d::bytea, 8) + 256 * get_byte(d::bytea, 9)) AS centroids,
       round(tdigest_quantile(d, 0.5)::numeric, -2) AS median,
       round(tdigest_quantile(d, 0.99)::numeric, -3) AS p99
  FROM (SELECT tdigest_agg(i::float8) AS d FROM generate_series(1, 100000) i) t;

-- canonical bytes: rebuilding a value from its bytes gives the same bytes
SELECT d::bytea = d::bytea::tdigest::bytea AS rebuilt_same,
       tdigest_union(d, tdigest_empty())::bytea = d::bytea AS union_with_empty
  FROM (SELECT tdigest_agg(i::float8) AS d FROM generate_series(1, 5000) i) t;

-- as a window function
SELECT i, tdigest_count(tdigest_agg(i::float8) OVER (ORDER BY i)) AS running_count,
       approx_percentile(0.5, i::float8) OVER (ORDER BY i) AS running_median
  FROM generate_series(1, 6) i;

-- a NULL compression is an error in every row, so the answer does not depend on the order of the rows
SELECT tdigest_compression(tdigest_agg(x, c)) FROM (VALUES (1.0::float8, 20), (2.0, NULL::int)) v(x, c);
SELECT tdigest_compression(tdigest_agg(x, c)) FROM (VALUES (1.0::float8, NULL::int), (2.0, 20)) v(x, c);
SELECT tdigest_compression(tdigest_weighted_agg(x, 1, c)) FROM (VALUES (1.0::float8, NULL::int), (2.0, 20)) v(x, c);
SELECT approx_percentile(0.5, x, c) FROM (VALUES (1.0::float8, NULL::int), (2.0, 20)) v(x, c);
SELECT approx_percentile(0.5, x, c) FROM (VALUES (1.0::float8, 20), (2.0, NULL::int)) v(x, c);
-- a NULL fraction ignores the row, in every order
SELECT approx_percentile(q, x) FROM (VALUES (NULL::float8, 1.0::float8), (0.5, 3.0), (0.5, 5.0)) v(q, x);
SELECT approx_percentile(q, x) FROM (VALUES (0.5::float8, 3.0::float8), (0.5, 5.0), (NULL, 1.0)) v(q, x);

-- values near the limit of the double type stay inside [min, max]
SELECT tdigest_quantile(d, 0.5) AS q50, tdigest_quantile(d, 0.25) AS q25, tdigest_quantile(d, 0.75) AS q75,
       tdigest_quantile(d, 0) AS q0, tdigest_quantile(d, 1) AS q1, tdigest_rank(d, 0) AS rank0,
       tdigest_rank(d, 1.7e308) AS rank_max, tdigest_rank(d, -1.7e308) AS rank_min
  FROM (SELECT tdigest_agg(x) d FROM (VALUES (-1.7e308::float8), (1.7e308)) v(x)) s;
SELECT bool_and(v >= -1.7e308 AND v <= 1.7e308) AS all_in_range, count(*) AS n
  FROM (SELECT tdigest_quantile(d, q / 100.0) AS v
          FROM (SELECT tdigest_agg(x) d FROM (VALUES (-1.7e308::float8), (-1.7e308), (1.7e308), (1.7e308), (0), (1e300), (-1e300)) v(x)) s,
               generate_series(0, 100) q) t;
SELECT tdigest_quantile(d, 0.5) AS between_huge_values, tdigest_min(d), tdigest_max(d)
  FROM (SELECT tdigest_agg(x, 10) d FROM (VALUES (1.7e308::float8), (1.6e308), (-1.6e308), (-1.7e308)) v(x)) s;
SELECT tdigest_quantile(tdigest_union(a, b), 0.5) IS NOT NULL AS merged_ok
  FROM (SELECT tdigest_add(tdigest_empty(), -1.7e308) a, tdigest_add(tdigest_empty(), 1.7e308) b) s;
SELECT tdigest_count(d), tdigest_quantile(d, 0.5) >= tdigest_min(d) AND tdigest_quantile(d, 0.5) <= tdigest_max(d) AS median_in_range,
       get_byte(d::bytea, 8) AS centroids
  FROM (SELECT tdigest_agg(x, 10) d FROM generate_series(1, 400) i, LATERAL (SELECT CASE WHEN i % 2 = 0 THEN 1.7e308 ELSE -1.7e308 END::float8 AS x) q) s;
