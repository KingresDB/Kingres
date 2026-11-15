# contrib/kingres_sketch/t/004_parallel.pl
#
# Partial aggregation: the combine, serial and deserial functions of the
# aggregates, through PostgreSQL's own parallel aggregation and through
# partitionwise aggregation of a partitioned table.  This is independent of
# kingres_shard.
#
# For each aggregate the same query is run serially, with a forced parallel
# plan (and a check that the plan is a Gather over Partial aggregates), and,
# on a partitioned table, with partitionwise aggregation (and a check that the
# plan has partial aggregates below an Append).  The answers are
#   hll:      identical (the same bytes),
#   topn:     within the guarantees (frequency - max_error <= true <= frequency,
#             max_error <= N / k) of the exact frequencies,
#   tdigest:  count, min and max exact; the rank error of the quantiles within
#             0.01 (body) and 0.002 (tails) at compression 100.
# The hash aggregate and the sorted aggregate strategies are both used.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: PostgreSQL

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('parallel');
$node->init;
$node->append_conf('postgresql.conf', "max_worker_processes = 8\nmax_parallel_workers = 8\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');

my $rows = 200000;
$node->safe_psql('postgres', <<"EOS");
SET search_path = public, kingres_sketch;
SELECT setseed(0.31);
CREATE TABLE t AS
  SELECT id, id % 10 AS g, (random() * 50000)::bigint AS v,
         exp(random() * ln(5000))::int AS item, (random() * 1000)::float8 + (id % 10) * 100 AS x
    FROM generate_series(1, $rows) id;
CREATE TABLE pt (id int, g int, v bigint, item int, x float8) PARTITION BY RANGE (g);
CREATE TABLE pt0 PARTITION OF pt FOR VALUES FROM (0) TO (3);
CREATE TABLE pt1 PARTITION OF pt FOR VALUES FROM (3) TO (6);
CREATE TABLE pt2 PARTITION OF pt FOR VALUES FROM (6) TO (10);
INSERT INTO pt SELECT * FROM t;
ALTER TABLE t SET (parallel_workers = 4);
ALTER TABLE pt0 SET (parallel_workers = 2);
ALTER TABLE pt1 SET (parallel_workers = 2);
ALTER TABLE pt2 SET (parallel_workers = 2);
ANALYZE t; ANALYZE pt;
-- a table of stored sketches, for the union aggregates
CREATE TABLE r AS
  SELECT g, id / 2000 AS b, hll_add_agg(v) AS h, topn_add_agg(item) AS n, tdigest_agg(x) AS d
    FROM t GROUP BY g, id / 2000;
ALTER TABLE r SET (parallel_workers = 4);
ANALYZE r;
EOS

my $serial = "SET max_parallel_workers_per_gather = 0; SET enable_partitionwise_aggregate = off;";
my $parallel = "SET max_parallel_workers_per_gather = 4; SET parallel_setup_cost = 0; "
  . "SET parallel_tuple_cost = 0; SET min_parallel_table_scan_size = 0; "
  . "SET min_parallel_index_scan_size = 0; SET parallel_leader_participation = off; "
  . "SET enable_partitionwise_aggregate = off;";
my $partwise = "SET max_parallel_workers_per_gather = 0; SET enable_partitionwise_aggregate = on;";
my $hashagg = "SET enable_hashagg = on; SET enable_sort = off;";
my $sortagg = "SET enable_hashagg = off;";

sub sql
{
	my ($settings, $sql) = @_;
	return $node->safe_psql('postgres', "SET search_path = public, kingres_sketch; $settings $sql");
}

sub plan_of
{
	my ($settings, $sql) = @_;
	return sql($settings, "EXPLAIN (COSTS OFF) $sql");
}

# the aggregates and the queries (scalar and grouped), over a table
my %agg = (
	hll => 'hll_add_agg(v)::bytea',
	hll_count => 'approx_count_distinct(v)',
	hll_p12 => 'approx_count_distinct(v, 12)',
);

# ---- hll: identical results ----
foreach my $tab ('t', 'pt')
{
	foreach my $name (sort keys %agg)
	{
		my $a = $agg{$name};
		my $scalar = "SELECT $a FROM $tab";
		my $grouped = "SELECT g, $a FROM $tab GROUP BY g ORDER BY g";
		foreach my $strategy ($hashagg, $sortagg)
		{
			my $base_s = sql($serial . $strategy, $scalar);
			my $base_g = sql($serial . $strategy, $grouped);
			if ($tab eq 't')
			{
				like(plan_of($parallel . $strategy, $scalar), qr/Gather.*Partial Aggregate/s,
					"$name scalar: the forced plan is a Gather over a partial aggregate");
				like(plan_of($parallel . $strategy, $grouped), qr/Gather.*Partial (Hash|Group)Aggregate/s,
					"$name grouped: the forced plan is a Gather over a partial aggregate");
				is(sql($parallel . $strategy, $scalar), $base_s, "$name scalar: parallel equals serial");
				is(sql($parallel . $strategy, $grouped), $base_g, "$name grouped: parallel equals serial");
			}
			else
			{
				like(plan_of($partwise . $strategy, $scalar), qr/Finalize Aggregate.*Append.*Partial Aggregate/s,
					"$name scalar on a partitioned table: partial aggregates below an Append");
				is(sql($partwise . $strategy, $scalar), $base_s,
					"$name scalar: partitionwise equals non-partitioned serial");
				is(sql($partwise . $strategy, $grouped), $base_g,
					"$name grouped by the partition key: partitionwise equals serial");
				is(sql($parallel . $strategy, $scalar), $base_s,
					"$name scalar on a partitioned table: parallel equals serial");
			}
		}
	}
}

# the union aggregate over stored sketches, parallel and serial
my $uscalar = "SELECT hll_union_agg(h)::bytea FROM r";
my $ugrouped = "SELECT g, hll_union_agg(h)::bytea FROM r GROUP BY g ORDER BY g";
like(plan_of($parallel, $uscalar), qr/Gather.*Partial Aggregate/s, 'hll_union_agg: parallel plan');
is(sql($parallel, $uscalar), sql($serial, $uscalar), 'hll_union_agg scalar: parallel equals serial');
is(sql($parallel . $sortagg, $ugrouped), sql($serial . $sortagg, $ugrouped), 'hll_union_agg grouped: parallel equals serial');
is(sql($serial, $uscalar), sql($serial, "SELECT hll_add_agg(v)::bytea FROM t"),
	'the union of the stored sketches equals the sketch of the rows');

# ---- topn: within the guarantees ----
# The check returns the number of counters that break a guarantee, for a
# sketch expression over table $tab, per group when $grouped.
sub topn_violations
{
	my ($settings, $tab, $grouped, $k) = @_;
	my $gcol = $grouped ? 'g' : '0';
	my $gby = $grouped ? 'GROUP BY g' : '';
	my $gl = $grouped ? 'g, ' : '';
	return sql($settings, <<"EOS");
WITH sk AS (SELECT $gcol AS g, topn_add_agg(item, $k) AS t FROM $tab $gby),
     truth AS (SELECT $gcol AS g, item::text AS item, count(*) AS c FROM $tab GROUP BY ${gl}item),
     tot AS (SELECT $gcol AS g, count(*) AS n FROM $tab $gby),
     l AS (SELECT sk.g, i.item, i.frequency, i.max_error FROM sk, LATERAL topn_list(sk.t) i)
SELECT (SELECT count(*) FROM l LEFT JOIN truth USING (g, item)
         WHERE truth.c IS NULL OR NOT (l.frequency - l.max_error <= truth.c AND truth.c <= l.frequency))
     + (SELECT count(*) FROM l JOIN tot USING (g) WHERE l.max_error > tot.n / $k)
     + (SELECT count(*) FROM truth JOIN tot USING (g)
         WHERE truth.c > tot.n / $k AND NOT EXISTS (SELECT 1 FROM l WHERE l.g = truth.g AND l.item = truth.item))
     + (SELECT count(*) FROM sk JOIN tot USING (g) WHERE topn_total(sk.t) <> tot.n);
EOS
}

foreach my $k (50, 500)
{
	foreach my $mode (['serial', $serial], ['parallel', $parallel])
	{
		is(topn_violations($mode->[1], 't', 0, $k), '0', "topn k=$k scalar, $mode->[0]: guarantees hold");
		is(topn_violations($mode->[1] . $sortagg, 't', 1, $k), '0', "topn k=$k grouped (sorted), $mode->[0]: guarantees hold");
		is(topn_violations($mode->[1] . $hashagg, 't', 1, $k), '0', "topn k=$k grouped (hashed), $mode->[0]: guarantees hold");
	}
	is(topn_violations($partwise, 'pt', 0, $k), '0', "topn k=$k scalar, partitionwise: guarantees hold");
	is(topn_violations($partwise, 'pt', 1, $k), '0', "topn k=$k grouped by the partition key, partitionwise: guarantees hold");
}
like(plan_of($parallel, "SELECT topn_add_agg(item) FROM t"), qr/Gather.*Partial Aggregate/s, 'topn: parallel plan');
like(plan_of($partwise, "SELECT topn_add_agg(item) FROM pt"), qr/Finalize Aggregate.*Append.*Partial Aggregate/s,
	'topn: partitionwise plan');

# the union aggregate over stored summaries: counters keep their guarantees
# (the stored sketches use capacity 1000)
foreach my $mode (['serial', $serial], ['parallel', $parallel])
{
	is(sql($mode->[1], <<"EOS"), '0', "topn_union_agg over stored summaries, $mode->[0]: guarantees hold");
WITH sk AS (SELECT topn_union_agg(n) AS t FROM r),
     truth AS (SELECT item::text AS item, count(*) AS c FROM t GROUP BY item),
     l AS (SELECT i.item, i.frequency, i.max_error FROM sk, LATERAL topn_list(sk.t) i)
SELECT (SELECT count(*) FROM l LEFT JOIN truth USING (item)
         WHERE truth.c IS NULL OR NOT (l.frequency - l.max_error <= truth.c AND truth.c <= l.frequency))
     + (SELECT count(*) FROM l WHERE l.max_error > $rows / 1000)
     + (SELECT count(*) FROM truth WHERE truth.c > $rows / 1000
         AND NOT EXISTS (SELECT 1 FROM l WHERE l.item = truth.item));
EOS
}

# ---- tdigest and approx_percentile: within the rank error ----
sub td_error
{
	my ($settings) = @_;
	return sql($settings, <<"EOS");
WITH d AS (SELECT tdigest_agg(x) AS d FROM t),
     est AS (SELECT q, tdigest_quantile(d.d, q) AS val
               FROM d, (SELECT unnest(ARRAY[0.001, 0.01, 0.1, 0.5, 0.9, 0.99, 0.999]) AS q) qs),
     cnt AS (SELECT q, (SELECT count(*) FROM t WHERE x < est.val)::float8 AS lo,
                    (SELECT count(*) FROM t WHERE x <= est.val)::float8 AS hi FROM est),
     err AS (SELECT q, CASE WHEN lo / $rows <= q AND q <= hi / $rows THEN 0
                            ELSE least(abs(q - lo / $rows), abs(q - hi / $rows)) END AS e FROM cnt)
SELECT max(e) FILTER (WHERE q BETWEEN 0.02 AND 0.98), max(e) FILTER (WHERE q NOT BETWEEN 0.02 AND 0.98) FROM err;
EOS
}

foreach my $mode (['serial', $serial], ['parallel', $parallel])
{
	my ($body, $tail) = split /\|/, td_error($mode->[1]);
	cmp_ok($body, '<=', 0.01, "tdigest_agg, $mode->[0]: body rank error $body");
	cmp_ok($tail, '<=', 0.002, "tdigest_agg, $mode->[0]: tail rank error $tail");
}

# approx_percentile: one aggregate per fraction, in the same query as the others
foreach my $mode (['serial', $serial], ['parallel', $parallel], ['partitionwise', $partwise])
{
	my $tab = $mode->[0] eq 'partitionwise' ? 'pt' : 't';
	my $r = sql($mode->[1], <<"EOS");
WITH e AS (SELECT approx_percentile(0.5, x) AS m, approx_percentile(0.99, x) AS p99,
                  approx_percentile(0.01, x) AS p01, tdigest_count(tdigest_agg(x)) AS n,
                  tdigest_min(tdigest_agg(x)) AS mn, tdigest_max(tdigest_agg(x)) AS mx FROM $tab)
SELECT e.n = $rows AND e.mn = (SELECT min(x) FROM t) AND e.mx = (SELECT max(x) FROM t)
   AND abs((SELECT count(*) FROM t WHERE x <= e.m)::float8 / $rows - 0.5) < 0.01
   AND abs((SELECT count(*) FROM t WHERE x <= e.p99)::float8 / $rows - 0.99) < 0.002
   AND abs((SELECT count(*) FROM t WHERE x <= e.p01)::float8 / $rows - 0.01) < 0.002
  FROM e;
EOS
	is($r, 't', "approx_percentile and tdigest_agg, $mode->[0]: exact count, min, max and within the rank error");
}

# grouped approx_percentile and tdigest_agg: per group
foreach my $mode (['serial', $serial], ['parallel', $parallel], ['partitionwise', $partwise])
{
	foreach my $strategy ($hashagg, $sortagg)
	{
		my $tab = $mode->[0] eq 'partitionwise' ? 'pt' : 't';
		my $r = sql($mode->[1] . $strategy, <<"EOS");
SELECT count(*) FROM (
  SELECT g, approx_percentile(0.5, x) AS m, tdigest_count(tdigest_agg(x)) AS n
    FROM $tab GROUP BY g) e
 WHERE e.n <> $rows / 10
    OR abs((SELECT count(*) FROM t WHERE t.g = e.g AND t.x <= e.m)::float8 / ($rows / 10) - 0.5) >= 0.01;
EOS
		is($r, '0', "approx_percentile grouped, $mode->[0]: every group within the rank error");
	}
}
like(plan_of($parallel, "SELECT approx_percentile(0.5, x) FROM t"), qr/Gather.*Partial Aggregate/s,
	'approx_percentile: parallel plan');
like(plan_of($partwise, "SELECT tdigest_agg(x) FROM pt"), qr/Finalize Aggregate.*Append.*Partial Aggregate/s,
	'tdigest_agg: partitionwise plan');

# the union aggregate over stored digests
foreach my $mode (['serial', $serial], ['parallel', $parallel])
{
	my $r = sql($mode->[1], <<"EOS");
WITH u AS (SELECT tdigest_union_agg(d) AS d FROM r)
SELECT tdigest_count(d) = $rows AND tdigest_min(d) = (SELECT min(x) FROM t) AND tdigest_max(d) = (SELECT max(x) FROM t)
   AND abs((SELECT count(*) FROM t WHERE x <= tdigest_quantile(u.d, 0.5))::float8 / $rows - 0.5) < 0.01
  FROM u;
EOS
	is($r, 't', "tdigest_union_agg over stored digests, $mode->[0]: exact count, min, max, median within the rank error");
}

# an aggregate with no rows and with a filter that matches none, in a parallel plan
is(sql($parallel, "SELECT hll_cardinality(hll_add_agg(v)), approx_count_distinct(v), tdigest_count(tdigest_agg(x)), "
	. "topn_total(topn_add_agg(item)), approx_percentile(0.5, x) IS NULL FROM t WHERE id < 0"),
	'0|0|0|0|t', 'no rows in a parallel plan: empty sketches and 0');

$node->stop;
done_testing();
