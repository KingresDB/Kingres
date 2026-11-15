-- sketch_topn: the type topn (Space-Saving), its functions and aggregates.
SET search_path = public, kingres_sketch;

SELECT topn_empty()::bytea AS empty_default, topn_capacity(topn_empty()), topn_total(topn_empty());
SELECT topn_capacity(topn_empty(1)), topn_capacity(topn_empty(100000));
SELECT topn_empty(0);
SELECT topn_empty(100001);
SELECT * FROM topn_list(topn_empty());
SELECT * FROM topn_list(NULL::topn);

-- a stream in which the first counters are enough: exact counts, no error
SELECT * FROM topn_list(
  (SELECT topn_add_agg(x, 10) FROM unnest(ARRAY['a','b','a','c','a','b','d']) x));
SELECT topn_total(t), topn_capacity(t)
  FROM (SELECT topn_add_agg(x, 10) AS t FROM unnest(ARRAY['a','b','a',NULL,'a']) x) s;

-- a stream that overflows 3 counters: the takeover rule of Space-Saving
SELECT * FROM topn_list(
  (SELECT topn_add_agg(x, 3) FROM unnest(ARRAY['a','b','a','c','d','a','b','e']) x));
SELECT * FROM topn_list(
  (SELECT topn_add_agg(x, 3) FROM unnest(ARRAY['a','b','a','c','d','a','b','e']) x), 2);
SELECT * FROM topn_list(
  (SELECT topn_add_agg(x, 3) FROM unnest(ARRAY['a','b','a','c','d','a','b','e']) x), 0);
SELECT * FROM topn_list(topn_empty(), -1);

-- capacity 1: one counter whose count is the number of items
SELECT * FROM topn_list((SELECT topn_add_agg(x, 1) FROM generate_series(1, 5) x));

-- items of the accepted types, and the refused ones
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES (1::int2), (1::int2), (2::int2)) t(x)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES (1::int8), (1::int8), (-2::int8)) t(x)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES (1.50::numeric), (1.5::numeric), (2::numeric)) t(x)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES (true), (true), (false)) t(x)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES ('123e4567-e89b-12d3-a456-426614174000'::uuid), ('123e4567-e89b-12d3-a456-426614174000'::uuid)) t(x)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES ('ab'::varchar), ('ab'::varchar), ('x'::name)) t(x)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES ('ab'::char(4)), ('ab'::char(4))) t(x)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES (1.5::float8)) t(x)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES ('2026-01-01'::date)) t(x)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES ('2026-01-01 00:00'::timestamptz)) t(x)));

-- the longest item is 1024 bytes
SELECT length(item), frequency FROM topn_list((SELECT topn_add_agg(repeat('x', 1024))));
SELECT topn_add_agg(repeat('x', 1025));
SELECT * FROM topn_list(topn_add(topn_empty(), ''::text));

-- topn_add on a value
SELECT * FROM topn_list(topn_add(topn_add(topn_add(topn_empty(5), 'a'::text), 'b'::text), 'a'::text));
SELECT topn_add(topn_empty(), NULL::text)::bytea = topn_empty()::bytea AS null_item_ignored;
SELECT topn_add(NULL, 'a'::text) IS NULL AS null_sketch;

-- union adds frequencies; the capacity must be the same
SELECT * FROM topn_list(topn_union(
  (SELECT topn_add_agg(x, 4) FROM unnest(ARRAY['a','a','b','c']) x),
  (SELECT topn_add_agg(x, 4) FROM unnest(ARRAY['a','b','b','d']) x)));
SELECT topn_total(topn_union(
  (SELECT topn_add_agg(x, 4) FROM unnest(ARRAY['a','a','b','c']) x),
  (SELECT topn_add_agg(x, 4) FROM unnest(ARRAY['a','b','b','d']) x)));
SELECT topn_union(topn_empty(3), topn_empty(4));
-- full summaries: an item missing from a full summary gets its smallest count
SELECT * FROM topn_list(topn_union(
  (SELECT topn_add_agg(x, 2) FROM unnest(ARRAY['a','a','a','b','b','c']) x),
  (SELECT topn_add_agg(x, 2) FROM unnest(ARRAY['d','d','d','d','e','e']) x)));

-- the union aggregate; NULLs are ignored; no rows give an empty summary of capacity 1000
SELECT * FROM topn_list(
  (SELECT topn_union_agg(t) FROM (VALUES
     ((SELECT topn_add_agg(x, 4) FROM unnest(ARRAY['a','a','b']) x)),
     (NULL::topn),
     ((SELECT topn_add_agg(x, 4) FROM unnest(ARRAY['a','c']) x))) v(t)));
SELECT topn_capacity(topn_add_agg(x)), topn_total(topn_union_agg(NULL::topn))
  FROM generate_series(1, 0) x;
SELECT topn_add_agg(x, 3 + x % 2) FROM generate_series(1, 4) x;
SELECT topn_add_agg(x, 0) FROM generate_series(1, 4) x;

-- canonical bytes: the same content rebuilt from its bytes is identical, and
-- order of equal counts is by item
SELECT t::bytea = t::bytea::topn::bytea AS rebuilt_same
  FROM (SELECT topn_add_agg(x, 3) AS t FROM unnest(ARRAY['b','a','c','b','a','d','e']) x) s;
SELECT (SELECT topn_add_agg(x, 5) FROM unnest(ARRAY['a','b','c']) x)::bytea
     = (SELECT topn_add_agg(x, 5) FROM unnest(ARRAY['c','b','a']) x)::bytea AS equal_counts_same_bytes;

-- the invariants on a larger stream: count - error <= true <= count, error <= N/k
WITH data AS (SELECT (i * i % 997)::text AS x FROM generate_series(1, 20000) i),
     truth AS (SELECT x, count(*) AS c FROM data GROUP BY x),
     sk AS (SELECT * FROM topn_list((SELECT topn_add_agg(x, 50) FROM data)))
SELECT count(*) AS counters,
       bool_and(frequency - max_error <= c AND c <= frequency) AS bounds_hold,
       bool_and(max_error <= 20000 / 50) AS error_bound_holds
  FROM sk JOIN truth ON truth.x = sk.item;

-- as a window function
SELECT i, topn_total(topn_add_agg(i % 3, 2) OVER (ORDER BY i)) AS running_total
  FROM generate_series(1, 5) i;

-- equal values are one item: char(n) without the padding, numeric without the scale
SELECT * FROM topn_list(topn_add(topn_add(topn_empty(), 'a'::char(3)), 'a'::char(5)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES ('a  '::char(3)), ('a'::char(1)), ('a  b'::char(5))) t(x)));
SELECT * FROM topn_list((SELECT topn_add_agg(x) FROM (VALUES (1.5::numeric), (1.50::numeric), (1.500::numeric), (2.0::numeric), (2::numeric), (0.00::numeric), (0::numeric)) t(x)));
-- a sketch from char(n) values of different lengths equals the sketch of the text without padding
SELECT topn_add_agg(x)::bytea = (SELECT topn_add_agg(rtrim(x::text)) FROM (VALUES ('a'::char(3)), ('a'::char(5))) u(x))::bytea AS same_as_trimmed_text
  FROM (VALUES ('a'::char(3)), ('a'::char(5))) t(x);

-- a NULL capacity is an error in every row, so the answer does not depend on the order of the rows
SELECT topn_capacity(topn_add_agg(x, k)) FROM (VALUES ('a', 5), ('b', NULL::int)) v(x, k);
SELECT topn_capacity(topn_add_agg(x, k)) FROM (VALUES ('a', NULL::int), ('b', 5)) v(x, k);
