# contrib/kingres_sketch/t/002_accuracy_topn.pl
#
# The guarantees of the topn summary on Zipf-like streams.
#
# Streams of 10^6 items over 10^5 distinct values, with exponent 0.8, 1.0 and
# 1.2 (an item of rank r has probability proportional to r^-exponent), at
# capacity k = 100 and 1000.  For every counter that a summary returns:
#   frequency - max_error <= true frequency <= frequency,  max_error <= N / k;
# every item with a true frequency above N / k has a counter; the sum of the
# counts is at most N.  The same must hold after merging 2 to 16 partial
# summaries, in different orders.  The top 10 by frequency equals the true top
# 10 for exponent 1.2 at k = 1000.  A seed is fixed, so the data are the same
# on every run.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('topn_accuracy');
$node->init;
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');

my $n = 1000000;
my $distinct = 100000;

# The inverse of the continuous Zipf distribution on [1, distinct]:
# rank = floor(F^-1(u)) with F(x) = (x^(1-s) - 1) / (D^(1-s) - 1), and
# F(x) = ln x / ln D for s = 1.
$node->safe_psql('postgres', <<"EOS");
SELECT setseed(0.4242);
CREATE TABLE stream AS
  SELECT s, id,
         CASE WHEN s = 10 THEN floor(power(1 + u * (power($distinct, 0.2) - 1), 1 / 0.2))::int
              WHEN s = 11 THEN floor(exp(u * ln($distinct)))::int
              ELSE floor(power(1 + u * (power($distinct, -0.2) - 1), 1 / -0.2))::int END AS item
    FROM (SELECT s, id, random() AS u
            FROM (VALUES (10), (11), (12)) e(s), generate_series(1, $n) id) t;
CREATE INDEX ON stream (s, id);
CREATE TABLE truth AS SELECT s, item::text AS item, count(*) AS c FROM stream GROUP BY s, item;
CREATE INDEX ON truth (s, c DESC);
ANALYZE stream; ANALYZE truth;
EOS

# s = 10: exponent 0.8; s = 11: exponent 1.0; s = 12: exponent 1.2
my %exponent = (10 => '0.8', 11 => '1.0', 12 => '1.2');

# A violation check: the SQL returns the number of counters (and items) that
# break one of the guarantees, for a sketch given as a SQL expression.
sub invariant_sql
{
	my ($s, $k, $sketch) = @_;
	return <<"EOS";
SET search_path = public, kingres_sketch;
WITH sk AS (SELECT $sketch AS t),
     l AS (SELECT item, frequency, max_error FROM sk, LATERAL topn_list(sk.t)),
     bad_bounds AS (
       SELECT count(*) AS c FROM l LEFT JOIN truth ON truth.s = $s AND truth.item = l.item
        WHERE truth.c IS NULL OR NOT (l.frequency - l.max_error <= truth.c AND truth.c <= l.frequency)),
     bad_error AS (SELECT count(*) AS c FROM l WHERE l.max_error > $n / $k),
     missing AS (
       SELECT count(*) AS c FROM truth
        WHERE truth.s = $s AND truth.c > $n / $k
          AND NOT EXISTS (SELECT 1 FROM l WHERE l.item = truth.item)),
     sums AS (SELECT sum(frequency) AS f, count(*) AS counters FROM l),
     tot AS (SELECT topn_total(t) AS n, topn_capacity(t) AS k FROM sk)
SELECT bad_bounds.c, bad_error.c, missing.c,
       (sums.f > tot.n)::int, (tot.n <> $n)::int, (tot.k <> $k)::int, (sums.counters > $k)::int
  FROM bad_bounds, bad_error, missing, sums, tot;
EOS
}

my $report = '';
foreach my $s (10, 11, 12)
{
	foreach my $k (100, 1000)
	{
		my $sketch = "(SELECT topn_add_agg(item::text, $k) FROM stream WHERE s = $s)";
		my $r = $node->safe_psql('postgres', invariant_sql($s, $k, $sketch));
		is($r, '0|0|0|0|0|0|0', "exponent $exponent{$s}, k = $k: bounds, error, completeness hold");

		# the same after merging the summaries of 2 to 16 parts
		foreach my $parts (2, 3, 5, 8, 16)
		{
			my $merged = "(SELECT topn_union_agg(p ORDER BY random()) FROM ("
			  . "SELECT topn_add_agg(item::text, $k) AS p FROM stream WHERE s = $s GROUP BY id % $parts) q)";
			my $m = $node->safe_psql('postgres', invariant_sql($s, $k, $merged));
			is($m, '0|0|0|0|0|0|0', "exponent $exponent{$s}, k = $k, merged from $parts parts");
		}

		# sequential topn_union in a random order
		my $chain = "(SELECT topn_union_agg(p ORDER BY random()) FROM ("
		  . "SELECT topn_add_agg(item::text, $k) AS p FROM stream WHERE s = $s GROUP BY (id * 31) % 7) q)";
		my $c = $node->safe_psql('postgres', invariant_sql($s, $k, $chain));
		is($c, '0|0|0|0|0|0|0', "exponent $exponent{$s}, k = $k, merged from 7 parts in random order");

		# how many counters are exact (error 0) and the largest error, for the report
		my $stats = $node->safe_psql('postgres', <<"EOS");
SET search_path = public, kingres_sketch;
SELECT count(*), count(*) FILTER (WHERE max_error = 0), max(max_error), $n / $k
  FROM topn_list($sketch);
EOS
		$report .= "exponent $exponent{$s} k=$k: counters|exact|max_error|N/k = $stats\n";
	}
}
diag($report);

# the top 10 by frequency is the true top 10 for exponent 1.2 at k = 1000
my $top = $node->safe_psql('postgres', <<"EOS");
SET search_path = public, kingres_sketch;
SELECT (SELECT array_agg(item ORDER BY frequency DESC, item)
          FROM (SELECT * FROM topn_list((SELECT topn_add_agg(item::text, 1000) FROM stream WHERE s = 12), 10)) a)
     = (SELECT array_agg(item ORDER BY c DESC, item)
          FROM (SELECT * FROM truth WHERE s = 12 ORDER BY c DESC, item LIMIT 10) b);
EOS
is($top, 't', 'the top 10 of exponent 1.2 at k = 1000 is the true top 10');

# a stream with all items distinct overflows every counter; the guarantees hold
my $distinct_all = $node->safe_psql('postgres', <<'EOS');
SET search_path = public, kingres_sketch;
SELECT count(*), min(frequency), max(frequency), max(max_error)
  FROM topn_list((SELECT topn_add_agg(i, 100) FROM generate_series(1, 100000) i));
EOS
is($distinct_all, '100|1000|1000|999', 'distinct items: 100 counters of 1000, errors below N / k');

$node->stop;
done_testing();
