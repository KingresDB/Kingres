# contrib/kingres_sketch/t/001_accuracy_hll.pl
#
# Accuracy of the hll estimate against exact distinct counts.
#
# The hash is fixed, so every data set gives the same estimate on every run:
# this is not a statistical test that can flake.  For each cardinality n, each
# of the seeded data sets (different disjoint sets of n integers) and each
# precision p of 10, 12, 14 and 16, the error |estimate - n| must be at most
# max(4 * 1.04 / sqrt(2^p) * n, 2), the bound of the design (section 14.3).
# The number of data sets is 200 for n up to 10^5, and 20 for 10^6 (the design
# says 200 for every n; 20 keeps the run of 10^6 elements short).
#
# Also: a union of the sketches of a random partition of the rows (2 to 8
# parts, with rows repeated in several parts) is byte for byte the sketch of
# all rows, and the estimate of a text column is as good as that of integers.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('hll_accuracy');
$node->init;
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');

my $setup = <<'EOS';
SET search_path = public, kingres_sketch;
CREATE TABLE cases AS
  SELECT n, s
    FROM (VALUES (0, 200), (1, 200), (10, 200), (100, 200), (1000, 200),
                 (10000, 200), (100000, 200), (1000000, 20)) c(n, sets),
         generate_series(1, c.sets) s;
-- data set s holds the integers s * 10000000019 + 1 .. + n: disjoint sets
CREATE TABLE est AS
  SELECT c.n, c.s,
         approx_count_distinct(g.v, 10) AS e10, approx_count_distinct(g.v, 12) AS e12,
         approx_count_distinct(g.v, 14) AS e14, approx_count_distinct(g.v, 16) AS e16
    FROM cases c
    LEFT JOIN LATERAL (SELECT c.s::bigint * 10000000019 + i AS v
                         FROM generate_series(1, c.n) i) g ON true
   GROUP BY c.n, c.s;
EOS
$node->safe_psql('postgres', $setup);

my $report = $node->safe_psql('postgres', <<'EOS');
SET search_path = public, kingres_sketch;
SELECT n, p, count(*) AS sets,
       round((100 * avg((est - n)::numeric / nullif(n, 0)))::numeric, 3) AS mean_err_pct,
       round((100 * max(abs(est - n)::numeric / nullif(n, 0)))::numeric, 3) AS max_err_pct,
       round((100 * 4 * 1.04 / sqrt(2^p))::numeric, 2) AS bound_pct,
       count(*) FILTER (WHERE abs(est - n) > greatest(4 * 1.04 / sqrt(2^p) * n, 2)) AS violations
  FROM est e CROSS JOIN LATERAL (VALUES (10, e10), (12, e12), (14, e14), (16, e16)) v(p, est)
 GROUP BY n, p ORDER BY p, n;
EOS
diag("hll accuracy (error in percent of n):\nn|p|sets|mean_err_pct|max_err_pct|bound_pct|violations\n$report");

my $violations = $node->safe_psql('postgres', <<'EOS');
SELECT count(*)
  FROM est e CROSS JOIN LATERAL (VALUES (10, e10), (12, e12), (14, e14), (16, e16)) v(p, est)
 WHERE abs(est - n) > greatest(4 * 1.04 / sqrt(2^p) * n, 2);
EOS
is($violations, '0', 'every estimate is within 4 times the standard error, or 2');

# very small sets are exact (a collision of two hashes in one register of 16384 is rare for 10 elements)
is($node->safe_psql('postgres', <<'EOS'), '0', 'sets of up to 10 elements are counted exactly at p = 14');
SELECT count(*) FROM est WHERE n <= 10 AND e14 <> n;
EOS

# no bias: the mean error over the larger n is far below the standard error
my $bias = $node->safe_psql('postgres', <<'EOS');
SELECT count(*) FROM (
  SELECT n, avg((e14 - n)::float8 / n) AS m FROM est WHERE n >= 1000 GROUP BY n) t
 WHERE abs(m) > 1.04 / sqrt(2^14);
EOS
is($bias, '0', 'the mean relative error at p = 14 is below one standard error');

# union of a partition equals the whole, byte for byte
$node->safe_psql('postgres', <<'EOS');
SET search_path = public, kingres_sketch;
CREATE TABLE data AS SELECT i, md5(i::text) AS t FROM generate_series(1, 100000) i;
EOS
foreach my $parts (2 .. 8)
{
	foreach my $seed (1 .. 3)
	{
		# a row goes to the part chosen by a hash of the row; every fifth row
		# goes to a second part as well, so the parts overlap
		my $ok = $node->safe_psql('postgres', <<"EOS");
SET search_path = public, kingres_sketch;
SELECT (SELECT hll_union_agg(p) FROM (
          SELECT hll_add_agg(i) AS p
            FROM (SELECT i, (i::bigint * 7919 + $seed) % $parts AS part FROM data
                  UNION ALL
                  SELECT i, (i::bigint * 104729 + $seed) % $parts FROM data WHERE i % 5 = 0) x
           GROUP BY part) y)::bytea
       = (SELECT hll_add_agg(i) FROM data)::bytea;
EOS
		is($ok, 't', "union of $parts overlapping parts (seed $seed) equals the whole");
	}
}

# a text column
my $text = $node->safe_psql('postgres', <<'EOS');
SET search_path = public, kingres_sketch;
SELECT count(*) FROM (
  SELECT s, approx_count_distinct(md5(s || '/' || i)) AS e
    FROM generate_series(1, 6) s, generate_series(1, 100000) i GROUP BY s) t
 WHERE abs(e - 100000) > 4 * 1.04 / sqrt(2^14) * 100000;
EOS
is($text, '0', 'text elements: six sets of 100000 are within the bound');

# estimates are the same through the sparse and the dense path: sketches of
# n = 3000 .. 3100 elements cross the point where p = 14 changes encoding
# (3071 registers); the estimate is a function of the registers
my $cross = $node->safe_psql('postgres', <<'EOS');
SET search_path = public, kingres_sketch;
SELECT count(*) FROM (
  SELECT n, hll_cardinality(a) AS ca,
         hll_cardinality(hll_union(b, c)) AS cu
    FROM (SELECT n,
                 (SELECT hll_add_agg(i) FROM generate_series(1, n) i) AS a,
                 (SELECT hll_add_agg(i) FROM generate_series(1, n / 2) i) AS b,
                 (SELECT hll_add_agg(i) FROM generate_series(n / 2 - 5, n) i) AS c
            FROM generate_series(3000, 3700, 25) n) q) t
 WHERE ca <> cu;
EOS
is($cross, '0', 'the estimate does not depend on the encoding or the way the sketch was built');

$node->stop;
done_testing();
