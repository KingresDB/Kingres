# contrib/kingres_sketch/t/003_accuracy_tdigest.pl
#
# Accuracy of tdigest_quantile on four distributions of 10^6 values.
#
# Distributions: uniform, normal, log-normal, and "ties", a mixture in which
# half of the values are drawn from 100 repeated values.  For compression 50,
# 100 and 500, and for q in 0.001, 0.01, 0.1, 0.5, 0.9, 0.99 and 0.999, the
# rank error of the value v = quantile(q) is the distance from q to the
# interval [P(x < v), P(x <= v)] of the exact fractions of the data (so a tie
# is not an error).  The digest must meet the thresholds below, which were set
# from the first measured values (the largest error over the quantiles of a
# kind, for the digest of one backend and for the union of 16 digests built on
# a partition of the rows) with a margin of 2.  The "ties" distribution has
# larger errors because between two repeated values (each worth 0.5% of the
# data) no value has a rank near q: the data themselves are that coarse.
# min, max and count are exact, and the number of centroids is at most the
# compression.  The seed is fixed, so the data are the same on every run.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('tdigest_accuracy');
$node->init;
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');

my $n = 1000000;
my @qs = (0.001, 0.01, 0.1, 0.5, 0.9, 0.99, 0.999);
my @deltas = (50, 100, 500);
my @dist_names = ('', 'uniform', 'normal', 'log-normal', 'ties');

# Thresholds on the rank error: by compression and by kind of distribution,
# for the quantiles at or below 0.01 and at or above 0.99 (tails) and for the
# others (body).
my %tail_limit = (
	continuous => { 50 => 0.0075, 100 => 0.0019, 500 => 0.00013 },
	ties => { 50 => 0.0085, 100 => 0.0081, 500 => 0.0008 });
my %body_limit = (
	continuous => { 50 => 0.0065, 100 => 0.0021, 500 => 0.00022 },
	ties => { 50 => 0.0120, 100 => 0.0026, 500 => 0.0021 });

# One table per distribution, so a scan reads only its own rows.  A digest is
# built by one backend (no parallel workers) so that it does not depend on
# how many workers happen to be available.
$node->safe_psql('postgres', <<"EOS");
SELECT setseed(0.7);
CREATE TABLE dist1 AS SELECT id, random()::float8 AS x FROM generate_series(1, $n) id;
CREATE TABLE dist2 AS
  SELECT id, (sqrt(-2 * ln(1 - u1)) * cos(2 * pi() * u2))::float8 AS x
    FROM (SELECT id, random() AS u1, random() AS u2 FROM generate_series(1, $n) id) t;
CREATE TABLE dist3 AS SELECT id, exp(x)::float8 AS x FROM dist2;
CREATE TABLE dist4 AS
  SELECT id, (CASE WHEN u2 < 0.5 THEN floor(u1 * 100) ELSE u1 * 100 END)::float8 AS x
    FROM (SELECT id, random() AS u1, random() AS u2 FROM generate_series(1, $n) id) t;
ANALYZE;
CREATE TABLE dg (d int, delta int, source text, t kingres_sketch.tdigest);
EOS
foreach my $delta (@deltas)
{
	foreach my $d (1 .. 4)
	{
		$node->safe_psql('postgres', <<"EOS");
SET max_parallel_workers_per_gather = 0;
SET search_path = public, kingres_sketch;
INSERT INTO dg SELECT $d, $delta, 'single', tdigest_agg(x, $delta) FROM dist$d;
INSERT INTO dg SELECT $d, $delta, 'merged16', tdigest_union_agg(p)
  FROM (SELECT tdigest_agg(x, $delta) AS p FROM dist$d GROUP BY id % 16) q;
EOS
	}
}

# The rank errors of one stored digest over the rows of its distribution:
# one line per q, "q|error".
sub rank_errors
{
	my ($d, $delta, $source) = @_;
	my @ix = (0 .. $#qs);
	my $vals = join(', ', map { "tdigest_quantile(dg.t, $qs[$_]) AS v$_" } @ix);
	my $lo = join(', ', map { "count(*) FILTER (WHERE x < v$_) AS lo$_" } @ix);
	my $hi = join(', ', map { "count(*) FILTER (WHERE x <= v$_) AS hi$_" } @ix);
	my $err = join(' UNION ALL ', map {
		my $q = $qs[$_];
		"SELECT ${q}::float8 AS q, CASE WHEN lo${_}::float8 / $n <= $q AND $q <= hi${_}::float8 / $n THEN 0 "
		  . "ELSE least(abs($q - lo${_}::float8 / $n), abs($q - hi${_}::float8 / $n)) END AS e FROM c"
	} @ix);
	return <<"EOS";
SET search_path = public, kingres_sketch;
WITH v AS MATERIALIZED (SELECT $vals FROM dg WHERE d = $d AND delta = $delta AND source = '$source'),
     c AS (SELECT $lo, $hi FROM dist$d, v)
$err
EOS
}

my $report = "tdigest rank error: distribution|compression|source|tails max|body max|centroids\n";
foreach my $d (1 .. 4)
{
	my $kind = $d == 4 ? 'ties' : 'continuous';
	foreach my $delta (@deltas)
	{
		foreach my $source ('single', 'merged16')
		{
			my ($tail, $body) = (0, 0);
			foreach my $row (split /\n/, $node->safe_psql('postgres', rank_errors($d, $delta, $source)))
			{
				my ($q, $e) = split /\|/, $row;
				if ($q <= 0.01 || $q >= 0.99) { $tail = $e if $e > $tail; }
				else { $body = $e if $e > $body; }
			}
			my $info = $node->safe_psql('postgres', <<"EOS");
SET search_path = public, kingres_sketch;
SELECT get_byte(t::bytea, 8) + 256 * get_byte(t::bytea, 9),
       tdigest_count(t) = $n AND tdigest_min(t) = (SELECT min(x) FROM dist$d)
       AND tdigest_max(t) = (SELECT max(x) FROM dist$d)
  FROM dg WHERE d = $d AND delta = $delta AND source = '$source';
EOS
			my ($centroids, $exact) = split /\|/, $info;
			my $label = "$dist_names[$d], compression $delta, $source";
			$report .= "$dist_names[$d]|$delta|$source|$tail|$body|$centroids\n";
			cmp_ok($tail, '<=', $tail_limit{$kind}{$delta}, "$label: tail rank error $tail");
			cmp_ok($body, '<=', $body_limit{$kind}{$delta}, "$label: body rank error $body");
			cmp_ok($centroids, '<=', $delta, "$label: $centroids centroids");
			is($exact, 't', "$label: min, max and count are exact");
		}
	}
}
diag($report);

# approx_percentile returns the same value as the digest of one backend
my $ap = $node->safe_psql('postgres', <<'EOS');
SET max_parallel_workers_per_gather = 0;
SET search_path = public, kingres_sketch;
SELECT (SELECT approx_percentile(0.99, x) FROM dist3)
     = (SELECT tdigest_quantile(t, 0.99) FROM dg WHERE d = 3 AND delta = 100 AND source = 'single');
EOS
is($ap, 't', 'approx_percentile equals tdigest_quantile of tdigest_agg');

$node->stop;
done_testing();
