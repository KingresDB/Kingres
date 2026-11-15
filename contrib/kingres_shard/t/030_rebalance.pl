# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# A full rebalance (design/shard-phase4.md, "Tests required"): a third node is
# added to a cluster of two, the plan of kingres.rebalance_plan() is run move by
# move, the counts end up as the rules say; then a node is drained with
# should_have_shards = false and removed.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster run_sql);

my @nodes = make_cluster('rb', 2, logical => 1);
my ($n1, $n2) = @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.orders (id int PRIMARY KEY, customer int NOT NULL);
	CREATE TABLE public.items (id int NOT NULL, order_id int NOT NULL, PRIMARY KEY (id, order_id));
	SELECT kingres.create_distributed_table('public.orders', 'id', bucket_count => 12);
	SELECT kingres.create_distributed_table('public.items', 'order_id', colocate_with => 'public.orders');
	ALTER TABLE public.items ADD CONSTRAINT items_order_fk FOREIGN KEY (order_id) REFERENCES public.orders (id);
	CREATE TABLE public.second (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.second', 'id', bucket_count => 6);
});
{
	my $sql = '';
	$sql .= "INSERT INTO public.orders VALUES ($_, $_ % 11);\n" for 1 .. 120;
	$sql .= "INSERT INTO public.items VALUES ($_, $_);\n" for 1 .. 120;
	$sql .= "INSERT INTO public.second VALUES ($_, 'x$_');\n" for 1 .. 30;
	$n1->safe_psql('postgres', $sql);
}
my $want = '120,7260,120,7260,30';

sub totals
{
	my ($node) = @_;
	return $node->safe_psql('postgres', q{
		SELECT (SELECT count(*) FROM public.orders) || ',' || (SELECT sum(id) FROM public.orders)
		       || ',' || (SELECT count(*) FROM public.items) || ',' || (SELECT sum(order_id) FROM public.items)
		       || ',' || (SELECT count(*) FROM public.second)});
}

sub counts
{
	return $n1->safe_psql('postgres', q{
		SELECT string_agg(colocation_id || '/' || group_id || '=' || n, ' ' ORDER BY colocation_id, group_id)
		  FROM (SELECT colocation_id, group_id, count(*) n FROM kingres.bucket_map GROUP BY 1, 2) s});
}

# Run the plan, move by move, as the control plane's task will.
sub run_plan
{
	my ($max) = @_;
	my $arg = defined $max ? $max : '';
	my @moves = split /\n/, $n1->safe_psql('postgres',
		"SELECT colocation_id || ' ' || bucket || ' ' || target_group FROM kingres.rebalance_plan($arg)");
	for my $m (@moves)
	{
		my ($c, $b, $t) = split / /, $m;
		$n1->safe_psql('postgres', "SELECT kingres.move_bucket($c, $b, $t)");
	}
	return scalar @moves;
}

is(counts(), '1/0=6 1/1=6 2/0=3 2/1=3', 'two groups, half of the buckets each');
is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.rebalance_plan()'), '0', 'nothing to rebalance yet');

# A third node.
my $n3 = make_node('rb3', logical => 1);
my $id3 = $n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('%s', %d)", $n3->host, $n3->port));
is($id3, '3', 'node 3 joined');
is(counts(), '1/0=6 1/1=6 2/0=3 2/1=3', 'the new group owns no bucket yet');
@nodes = ($n1, $n2, $n3);

my $plan_rows = $n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.rebalance_plan()');
is($plan_rows, '6', 'the plan moves 4 + 2 buckets');
is($n1->safe_psql('postgres', 'SELECT count(*) FILTER (WHERE size_bytes IS NOT NULL) > 0 FROM kingres.rebalance_plan()'), 't', 'the plan knows the sizes');
is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.rebalance_plan(2)'), '2', 'max_moves limits the plan');
is(run_plan(2), 2, 'two moves of the plan are made');
is(run_plan(), 4, 'and the rest of it');
is(counts(), '1/0=4 1/1=4 1/2=4 2/0=2 2/1=2 2/2=2', 'every group owns a third of the buckets');
is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.rebalance_plan()'), '0', 'the plan is empty after the rebalance');
is(totals($_), $want, 'every row exactly once from ' . $_->name) for @nodes;
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
is($n1->safe_psql('postgres', q{SELECT coalesce(string_agg(check_name || ' ' || node_id || ' ' || object, E'\n'), '') FROM kingres.check_cluster()}), '', 'check_cluster is clean');

# New writes land where the map says.
$n3->safe_psql('postgres', "INSERT INTO public.second VALUES (1001, 'new')");
is(totals($n1), '120,7260,120,7260,31', 'a write after the rebalance is visible');
$n3->safe_psql('postgres', "DELETE FROM public.second WHERE id = 1001");

# Drain node 2 and remove it.
$n1->safe_psql('postgres', 'SELECT kingres.set_node_property(2, false)');
is($n1->safe_psql('postgres', 'SELECT should_have_shards FROM kingres.node WHERE node_id = 2'), 'f', 'node 2 no longer receives shards');
for my $n (@nodes)
{
	is($n->safe_psql('postgres', 'SELECT should_have_shards FROM kingres.node WHERE node_id = 2'), 'f', 'every node knows it: ' . $n->name);
}
my ($rc, $out, $err) = run_sql($n1, 'SELECT kingres.remove_node(2)');
like($err, qr/owns \d+ buckets/, 'a node that still owns buckets cannot be removed');
($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket(1, 0, 1)");
like($err, qr/does not accept buckets|already on/, 'a move into a draining group is refused');
my $moved = run_plan();
cmp_ok($moved, '>=', 6, "draining moves the buckets of node 2 ($moved)");
is(counts(), '1/0=6 1/2=6 2/0=3 2/2=3', 'group 1 is empty');
is(totals($_), $want, 'every row exactly once from ' . $_->name) for @nodes;
($rc, $out, $err) = run_sql($n1, 'SELECT kingres.remove_node(2)');
like($err, qr/takes part in bucket move/, 'the node cannot go while the old partitions wait to be dropped');
is($n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)'), $moved, 'cleanup finishes every move');
$n1->safe_psql('postgres', 'SELECT kingres.remove_node(2)');
is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), '2', 'node 2 is gone from the catalog');
is(totals($_), $want, 'the data is whole from ' . $_->name) for ($n1, $n3);
is($n1->safe_psql('postgres', q{SELECT coalesce(string_agg(check_name || ' ' || node_id || ' ' || object, E'\n'), '') FROM kingres.check_cluster()}), '', 'check_cluster is clean at the end');
is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.rebalance_plan()'), '0', 'and so is the plan');

done_testing();
