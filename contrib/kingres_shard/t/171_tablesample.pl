# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/171_tablesample.pl
#
# Review P2E-02: TABLESAMPLE of a distributed table returned the unsampled rows of every remote group (the distributed scan asks
# every group for its rows with no sampling, and only this node's partitions were sampled).  The statement is now refused with a
# clear error on every path, from every node, with the pushdown on and off; a reference table and a local table are sampled as on a
# single node.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('ts', 3, extra => "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_join = on\n");
my ($n1) = @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.m (id int, c int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.m', 'id', bucket_count => 12);
	CREATE TABLE public.m2 (id int, c int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.m2', 'id', colocate_with => 'public.m');
	CREATE TABLE public.r (k int PRIMARY KEY, v text);
	SELECT kingres.create_reference_table('public.r');
	INSERT INTO public.m SELECT g, c FROM generate_series(1, 300) g, generate_series(1, 4) c;
	INSERT INTO public.m2 SELECT g, c FROM generate_series(1, 300) g, generate_series(1, 2) c;
	INSERT INTO public.r SELECT g, 'x' FROM generate_series(1, 20) g;
});
$_->safe_psql('postgres', 'CREATE TABLE public.loc (a int); INSERT INTO public.loc SELECT g FROM generate_series(1, 50) g') for @nodes;
$_->safe_psql('postgres', 'ANALYZE') for @nodes;

my @refused = (
	q{SELECT count(*) FROM public.m TABLESAMPLE BERNOULLI (0)},
	q{SELECT count(*) FROM public.m TABLESAMPLE BERNOULLI (50) REPEATABLE (1)},
	q{SELECT count(*) FROM public.m TABLESAMPLE SYSTEM (50)},
	q{SELECT c, count(*) FROM public.m TABLESAMPLE BERNOULLI (50) GROUP BY c},
	q{SELECT id, count(*) FROM public.m TABLESAMPLE BERNOULLI (50) GROUP BY id},
	q{SELECT a.id FROM public.m a TABLESAMPLE BERNOULLI (50) JOIN public.m2 b ON b.id = a.id},
	q{SELECT a.id FROM public.m a JOIN public.m2 b TABLESAMPLE BERNOULLI (50) ON b.id = a.id},
	q{SELECT * FROM (SELECT id FROM public.m TABLESAMPLE BERNOULLI (50)) q},
	q{WITH s AS (SELECT id FROM public.m TABLESAMPLE BERNOULLI (50)) SELECT count(*) FROM s},
	q{SELECT count(*) FROM public.m WHERE id IN (SELECT id FROM public.m2 TABLESAMPLE BERNOULLI (50))},
	q{DELETE FROM public.m2 WHERE id IN (SELECT id FROM public.m TABLESAMPLE BERNOULLI (50))},
	q{INSERT INTO public.loc SELECT id FROM public.m TABLESAMPLE BERNOULLI (50)},
);
for my $nd (@nodes)
{
	for my $sql (@refused)
	{
		my ($rc, $out, $err) = run_sql($nd, $sql);
		isnt($rc, 0, $nd->name . ": refused: $sql");
		like($err, qr/TABLESAMPLE is not supported for distributed table/, '... with the clear error') or diag($err);
	}
}
{
	# every path, with the pushdown off too
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_pushdown = off; $refused[0]");
	like($err, qr/TABLESAMPLE is not supported for distributed table "m"/, 'enable_pushdown = off: the same error');
	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_distributed_scan = off; $refused[1]");
	like($err, qr/TABLESAMPLE|distributed/, 'enable_distributed_scan = off: still an error, never rows of one group');
	# and a prepared statement
	($rc, $out, $err) = run_sql($n1, q{PREPARE ps AS SELECT count(*) FROM public.m TABLESAMPLE BERNOULLI (10); EXECUTE ps});
	like($err, qr/TABLESAMPLE is not supported/, 'a prepared statement is refused');
}
# nothing was changed
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.m2'), 600, 'the DELETE and the INSERT of the list changed nothing');
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.loc'), 50, '... and the local table has its rows');

# what is sampled as on a single node: a reference table, a local table, and a distributed table without TABLESAMPLE
for my $nd (@nodes)
{
	is($nd->safe_psql('postgres', 'SELECT count(*) FROM public.r TABLESAMPLE BERNOULLI (100)'), 20, $nd->name . ': a reference table is sampled (100 percent: every row)');
	is($nd->safe_psql('postgres', 'SELECT count(*) FROM public.r TABLESAMPLE BERNOULLI (0)'), 0, $nd->name . ': and (0 percent) none');
	is($nd->safe_psql('postgres', 'SELECT count(*) FROM public.loc TABLESAMPLE BERNOULLI (100)'), 50, $nd->name . ': a local table is sampled');
	is($nd->safe_psql('postgres', 'SELECT count(*) FROM public.m'), 1200, $nd->name . ': a distributed table without TABLESAMPLE is read whole');
}
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.m WHERE random() < 2'), 1200, 'a random() condition is the way to sample: all rows when it is always true');
my $half = $n1->safe_psql('postgres', 'SELECT count(*) FROM public.m WHERE random() < 0.5');
cmp_ok($half, '>', 300, 'a random() condition of one half keeps a share of the rows (more than 300 of 1200)');
cmp_ok($half, '<', 900, '... and not all of them');
is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;

done_testing();
