# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/201_plan_disk_weights.pl: the plan of a rebalance by disk size, the weight of a node and a drain (design/shard-phase10.md, sections 4 and 5;
# increment P10.3), on a cluster of three groups with real data.
#
#   A. kingres.group_load('by_disk_size') is what the nodes really hold (each bucket counts at least min_bucket_cost)
#   B. plan_rebalance('by_disk_size'): the planned buckets are the large ones, size_bytes is the real size, and executing the plan moves the groups
#      closer to the ideal; no data is lost
#   C. set_node_property() with a weight: every node has it, the old two-argument form still works, and the weight changes the plan
#   D. a drain: should_have_shards = false and a plan with drain moves only; executing it empties the group
#   E. a plan by disk size does not guess: with a group that does not answer it fails and names the group, a plan by bucket count still works
#   F. who may plan

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @nodes = make_cluster('pdw', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

q1($n1, q{
	CREATE TABLE public.s1 (id int PRIMARY KEY, payload text);
	ALTER TABLE public.s1 ALTER COLUMN payload SET STORAGE EXTERNAL;
	SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 8);
});
my $C = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.s1'::regclass});
# buckets 0, 1, 3 and 6 are large (about 750 rows of 3 kB), the others small
q1($n1, q{
	INSERT INTO public.s1
	SELECT g, CASE WHEN kingres.bucket_for('public.s1'::regclass, g) IN (0, 1, 3, 6)
	               THEN (SELECT string_agg(md5(random()::text || g::text || i::text), '') FROM generate_series(1, 94) i) ELSE 'small' END
	  FROM generate_series(1, 6000) g});
q1($_, q{CHECKPOINT}) for @nodes;
my $rows = q1($n1, q{SELECT count(*) || ',' || sum(id) FROM public.s1});
is($rows, '6000,18003000', 'the table is loaded');

# the cost of a bucket as the node that owns it sees it: the size of its partitions, at least the minimum
sub real_costs
{
	my $min = 1048576;		# kingres_shard.min_bucket_cost, 1 MB by default
	my %cost;
	for my $n (@nodes)
	{
		for my $line (split /\n/, q1($n, q{
			SELECT b || '=' || (CASE WHEN sz > 1048576 THEN sz ELSE 1048576 END)
			  FROM (SELECT (regexp_match(c.relname, '_b([0-9]+)$'))[1] AS b, sum(pg_total_relation_size(c.oid)) AS sz
			          FROM pg_class c WHERE c.relname ~ '^s1_b[0-9]{4}$' AND c.relkind = 'r' AND c.relispartition GROUP BY 1) x}))
		{
			my ($b, $v) = split /=/, $line;
			$cost{$b + 0} = $v;
		}
	}
	return %cost;
}
sub group_of { my ($b) = @_; return q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"); }
sub loads { return q1($n1, q{SELECT string_agg(group_id || ':' || load, ' ' ORDER BY group_id) FROM kingres.group_load('by_disk_size')}); }
sub max_dev { return q1($n1, q{SELECT round(max(deviation)::numeric, 3) FROM kingres.group_load('by_disk_size')}); }

# ---------------------------------------------------------------------------
# A. the load of a group
# ---------------------------------------------------------------------------
my %cost = real_costs();
is(scalar(keys %cost), 8, 'A: eight buckets have partitions');
my %expect;
$expect{group_of($_)} += $cost{$_} for keys %cost;
my %got = map { split /:/ } split / /, loads();
is($got{$_}, $expect{$_}, "A: group $_ has the load that its node holds, bucket by bucket") for sort keys %expect;
is(q1($n1, q{SELECT string_agg(group_id || ':' || buckets, ' ' ORDER BY group_id) FROM kingres.group_load('by_disk_size')}), '0:3 1:3 2:2', 'A: and the buckets per group');
cmp_ok($cost{0}, '>', 2 * 1048576, 'A: a large bucket is larger than two minimum costs');
is($cost{2}, 1048576, 'A: a small bucket counts as the minimum');
my $dev0 = max_dev();
cmp_ok($dev0, '>', 0.1, "A: the groups are far from the ideal ($dev0)");

# ---------------------------------------------------------------------------
# B. the plan by disk size
# ---------------------------------------------------------------------------
my @plan = split /\n/, q1($n1, q{SELECT bucket || '|' || source_group || '|' || target_group || '|' || cost || '|' || coalesce(size_bytes::text, 'null') || '|' || reason FROM kingres.plan_rebalance('by_disk_size') ORDER BY seq});
cmp_ok(scalar(@plan), '>=', 1, 'B: the plan has a move');
cmp_ok(scalar(@plan), '<=', 3, 'B: and only a few');
for my $m (@plan)
{
	my ($b, $s, $t, $c, $sz, $r) = split /[|]/, $m;
	is($c, $cost{$b}, "B: the planned bucket $b costs what its node holds");
	is($r, 'balance', "B: the move of bucket $b is a balance move");
	cmp_ok($c, '>', 1048576, "B: and bucket $b is a large one (the plan moves what makes a difference)");
	like($sz, qr/^\d+$/, "B: the size of bucket $b is given");
	is($s, group_of($b), "B: bucket $b is planned from the group that owns it");
}
is(q1($n1, q{SELECT count(*) FROM kingres.plan_rebalance('by_disk_size', 0.1, 1)}), '1', 'B: max_moves caps the plan');
is(q1($n1, q{SELECT count(*) FROM kingres.plan_rebalance('by_disk_size', 100)}), '0', 'B: a threshold that every group meets gives an empty plan');
for my $m (@plan)
{
	my ($b, $s, $t) = split /[|]/, $m;
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $t)");
	is($rc, 0, "B: the planned move of bucket $b to group $t") or diag($err);
}
q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
my $dev1 = max_dev();
cmp_ok($dev1, '<', $dev0, "B: the groups are closer to the ideal after the plan ($dev0 before, $dev1 after)");
is(q1($n1, q{SELECT count(*) || ',' || sum(id) FROM public.s1}), $rows, 'B: every row is there');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'B: check_cluster() finds nothing');

# ---------------------------------------------------------------------------
# C. the weight of a node
# ---------------------------------------------------------------------------
q1($n1, q{SELECT kingres.set_node_property(2, NULL, 4)});
is(q1($_, q{SELECT weight FROM kingres.node WHERE node_id = 2}), '4', 'C: the weight is on every node') for @nodes;
is(q1($n1, q{SELECT should_have_shards FROM kingres.node WHERE node_id = 2}), 't', 'C: and the flag is as it was');
q1($n1, q{SELECT kingres.set_node_property(2, true)});
is(q1($n1, q{SELECT weight FROM kingres.node WHERE node_id = 2}), '4', 'C: the two-argument form leaves the weight alone');
my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.set_node_property(2, NULL, 0)});
isnt($rc, 0, 'C: a weight of 0 is refused');
($rc, $out, $err) = run_sql($n2, q{SELECT kingres.set_node_property(2, NULL, 2)});
isnt($rc, 0, 'C: and the map group primary only may set it');
like(q1($n1, q{SELECT string_agg(group_id || ':' || weight, ' ' ORDER BY group_id) FROM kingres.group_load('by_disk_size')}), qr/^0:1 1:4 2:1$/, 'C: group_load shows the weights');
# with a weight of 4 the group of node 2 is the one that should hold most
my @plan_w = split /\n/, q1($n1, q{SELECT target_group FROM kingres.plan_rebalance('by_disk_size', 0) ORDER BY seq});
cmp_ok(scalar(grep { $_ eq '1' } @plan_w), '>=', 1, 'C: with the weight the plan moves toward group 1');
q1($n1, q{SELECT kingres.set_node_property(2, NULL, 1)});
is(q1($n3, q{SELECT weight FROM kingres.node WHERE node_id = 2}), '1', 'C: the weight is set back');

# ---------------------------------------------------------------------------
# D. a drain
# ---------------------------------------------------------------------------
q1($n1, q{SELECT kingres.set_node_property(3, false, NULL)});
is(q1($n2, q{SELECT should_have_shards FROM kingres.node WHERE node_id = 3}), 'f', 'D: node 3 does not accept buckets any more');
is(q1($n1, q{SELECT receiving FROM kingres.group_load() WHERE group_id = 2}), 'f', 'D: and group 2 is not a receiver');
my $held = q1($n1, q{SELECT count(*) FROM kingres.bucket_map WHERE group_id = 2});
my $dplan = q1($n1, q{SELECT count(*) FILTER (WHERE reason = 'drain') || ',' || count(*) FILTER (WHERE source_group <> 2 OR target_group = 2) FROM kingres.plan_rebalance('by_disk_size', drain_only => true)});
is($dplan, "$held,0", 'D: a drain-only plan has one move for every bucket of the group and nothing else');
for my $m (split /\n/, q1($n1, q{SELECT bucket || '|' || target_group FROM kingres.plan_rebalance('by_disk_size', drain_only => true) ORDER BY seq}))
{
	my ($b, $t) = split /[|]/, $m;
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $t)");
	is($rc, 0, "D: the bucket $b leaves group 2 for group $t") or diag($err);
}
q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
is(q1($n1, q{SELECT count(*) FROM kingres.bucket_map WHERE group_id = 2}), '0', 'D: group 2 holds no bucket');
is(q1($n1, q{SELECT count(*) FROM kingres.plan_rebalance('by_disk_size', drain_only => true)}), '0', 'D: and nothing is left to drain');
is(q1($n1, q{SELECT count(*) || ',' || sum(id) FROM public.s1}), $rows, 'D: every row is there');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'D: check_cluster() finds nothing');
# the plan of a what-if drain does not need the flag
q1($n1, q{SELECT kingres.set_node_property(3, true, 1)});
is(q1($n1, q{SELECT count(*) FILTER (WHERE reason = 'drain') FROM kingres.plan_rebalance('by_bucket_count', exclude_groups => ARRAY[0])}), q1($n1, q{SELECT count(*) FROM kingres.bucket_map WHERE group_id = 0}), 'D: exclude_groups plans the drain of a group without changing it');

# ---------------------------------------------------------------------------
# F. who may plan (before a node is stopped)
# ---------------------------------------------------------------------------
q1($n1, q{CREATE ROLE eve LOGIN});
($rc, $out, $err) = run_sql($n1, q{SET ROLE eve; SELECT count(*) FROM kingres.plan_rebalance('by_disk_size')});
isnt($rc, 0, 'F: a role that is not a superuser cannot plan by disk size (it reads the nodes)');
like($err, qr/permission denied|must be superuser|superuser/i, 'F: with a permission error');
($rc, $out, $err) = run_sql($n1, q{SET ROLE eve; SELECT count(*) FROM kingres.group_load()});
isnt($rc, 0, 'F: nor can it read the load: the plan reads the records of the moves, which only a superuser may');

# ---------------------------------------------------------------------------
# E. a group that does not answer (group 1 still holds buckets; group 2 was drained)
# ---------------------------------------------------------------------------
$n2->stop('immediate');
($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.node_connect_timeout = 2; SELECT count(*) FROM kingres.plan_rebalance('by_disk_size')});
isnt($rc, 0, 'E: a plan by disk size with a group that does not answer fails');
like($err, qr/shard group 1 cannot be read/, 'E: and names the group');
($rc, $out, $err) = run_sql($n1, q{SELECT count(*) FROM kingres.plan_rebalance('by_bucket_count')});
is($rc, 0, 'E: a plan by bucket count does not need the nodes') or diag($err);

done_testing();
