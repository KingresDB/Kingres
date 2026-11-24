# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/276_schema_rebalance_plan.pl
#
# Phase 11 (design/shard-phase11.md 9.9): a distributed schema is one more item of the plan of a rebalance and of a drain.
#
#   A. plan_rebalance() plans the schemas of an unbalanced cluster (by count), the moves it plans balance the groups, a schema that a ledger table pins is
#      not an item and counts as fixed load, a plan by disk size knows the sizes of the schemas
#   B. the drain: drain_only lists every schema of the group with reason 'drain', schemas_on_group() lists them with their sizes and what would keep one
#      from moving, a new schema is not placed on a group that may not hold shards, a group that owns a schema cannot be removed until it is moved away

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @nodes = make_cluster('rp', 3, logical => 1,
	extra => "shared_preload_libraries = 'kingres_shard,kingres_ledger'\nkingres_ledger.block_interval = 0\nkingres_shard.move_cleanup_delay = 1\n");
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

q1($_, q{CREATE EXTENSION kingres_ledger}) for @nodes;
q1($n1, q{SELECT kingres.set_sharding_mode('schema', true)});
my @groups = split /\n/, q1($n1, q{SELECT group_id FROM kingres.node ORDER BY node_id});
my ($g1, $g2, $g3) = @groups;

# six schemas, every one on the first group; s1 is the big one, s2 holds a ledger table
q1($n1, qq{SET kingres_shard.schema_placement_group = $g1; CREATE SCHEMA s$_}) for 1 .. 6;
q1($n1, q{CREATE TABLE s1.t (id int PRIMARY KEY, v text)});
q1($n1, q{INSERT INTO s1.t SELECT g, repeat('x', 200) FROM generate_series(1, 20000) g});
for my $i (2 .. 6)
{
	q1($n1, qq{CREATE TABLE s$i.t (id int PRIMARY KEY, v text); INSERT INTO s$i.t SELECT g, 'row' || g FROM generate_series(1, 50) g});
}
my $owner2 = q1($n1, q{SELECT n.node_id FROM kingres.node n WHERE n.group_id = } . $g1 . q{ AND n.role = 'primary'});
my ($ownernode) = grep { q1($_, q{SELECT node_id FROM kingres.local_node}) == $owner2 } @nodes;
q1($ownernode, q{SELECT kingres.ledger_enable('s2.t')});
my %coloc = map { split /\|/ } split /\n/, q1($n1, q{SELECT nsp::regnamespace::text || '|' || colocation_id FROM kingres.dist_schema});
my $pinned = $coloc{s2};

# ---------------------------------------------------------------------------
# A. the plan
# ---------------------------------------------------------------------------
my $plan = q1($n1, q{SELECT string_agg(colocation_id || ':' || source_group || '>' || target_group || ':' || reason, ' ' ORDER BY seq) FROM kingres.plan_rebalance()});
ok(length($plan // ''), 'the plan of an unbalanced cluster holds moves of schemas') or diag('plan: ' . ($plan // 'NULL'));
is(q1($n1, qq{SELECT count(*) FROM kingres.plan_rebalance() WHERE colocation_id = $pinned}), 0, 'A: the schema that holds a ledger table is not an item of the plan');
is(q1($n1, qq{SELECT count(*) FROM kingres.plan_rebalance(colocation_ids => ARRAY[$pinned])}), 0, 'A: ... not even when it is the only colocation group the plan may touch (while another schema of the same group would be moved)');
is(q1($n1, qq{SELECT count(*) FROM kingres.plan_rebalance(colocation_ids => ARRAY[$coloc{s3}])}), 1, 'A: a schema that nothing pins is');
is(q1($n1, qq{SELECT count(*) FROM kingres.plan_rebalance() WHERE colocation_id NOT IN (SELECT colocation_id FROM kingres.dist_schema)}), 0, 'A: every item is a schema of the cluster');
is(q1($n1, qq{SELECT count(*) FROM kingres.plan_rebalance() WHERE source_group <> $g1}), 0, 'A: they all leave the group that holds them');
is(q1($n1, q{SELECT count(*) FROM kingres.plan_rebalance() WHERE bucket <> 0}), 0, 'A: the item of a schema is its bucket 0');
{
	my $load = q1($n1, q{SELECT string_agg(group_id || ':' || load, ' ' ORDER BY group_id) FROM kingres.group_load()});
	is($load, "$g1:6 $g2:0 $g3:0", 'A: the load of the groups counts the six schemas, the pinned one included');
}
# the moves of the plan make the groups balanced, one after the other, as the scheduler does; the pinned schema stays where it is
{
	for (1 .. 8)
	{
		my $m = q1($n1, q{SELECT colocation_id || ' ' || bucket || ' ' || target_group FROM kingres.plan_rebalance(max_moves => 1)});
		last if $m eq '';
		my ($c, $b, $t) = split / /, $m;
		my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($c, $b, $t)");
		is($rc, 0, "A: the move of colocation group $c to group $t works") or diag($err);
	}
	is(q1($n1, q{SELECT count(*) FROM kingres.plan_rebalance()}), 0, 'A: after the moves the plan is empty');
	is(q1($n1, q{SELECT string_agg(g::text, ' ' ORDER BY g) FROM (SELECT count(*) g FROM kingres.bucket_map GROUP BY group_id) x}), '2 2 2', 'A: the schemas are spread, two on each group (the pinned one stays and keeps one companion)')
	  or diag(q1($n1, q{SELECT string_agg(colocation_id || '@' || group_id, ' ') FROM kingres.bucket_map}));
	is(q1($n1, qq{SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $pinned}), $g1, 'A: the pinned schema is still on its group');
	is(q1($_, q{SELECT count(*) FROM s1.t}), 20000, 'A: ' . $_->name . ' reads the rows of the big schema after the moves') for @nodes;
	is(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || coalesce(object, ''), '; '), '') FROM kingres.check_cluster()}), '', 'A: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# B. the drain
# ---------------------------------------------------------------------------
q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
my ($busy) = map { $_->[0] } sort { $b->[1] <=> $a->[1] } map { [ $_, q1($n1, qq{SELECT count(*) FROM kingres.bucket_map WHERE group_id = $_}) ] } ($g2, $g3);
my $drain_node = q1($n1, qq{SELECT node_id FROM kingres.node WHERE group_id = $busy AND role = 'primary'});
{
	my @on = split /\n/, q1($n1, qq{SELECT schema_name || ':' || (size_bytes > 0) || ':' || coalesce(blocker, '-') FROM kingres.schemas_on_group($busy) ORDER BY schema_name});
	cmp_ok(scalar(@on), '>=', 1, "B: schemas_on_group($busy) lists the schemas of the group: @on");
	unlike(join(' ', @on), qr/schema_pinned/, 'B: none of them is blocked on this group');
	{
		my $big = q1($n1, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 's1'});
		my $sz = q1($n1, qq{SELECT size_bytes FROM kingres.schemas_on_group($big) WHERE schema_name = 's1'});
		cmp_ok($sz, '>', 4000000, "B: the size of the big schema is the bytes of its tables on its owner ($sz)");
	}
	my @g1s = split /\n/, q1($n1, qq{SELECT schema_name || ':' || coalesce(blocker, '-') FROM kingres.schemas_on_group($g1) ORDER BY schema_name});
	like(join(' ', @g1s), qr/s2:schema_pinned/, 'B: the pinned schema shows its blocker on its group');
}
q1($n1, qq{SELECT kingres.set_node_property($drain_node, false)});
{
	my $d = q1($n1, q{SELECT string_agg(DISTINCT reason, ',') FROM kingres.plan_rebalance(drain_only => true)});
	is($d, 'drain', 'B: the plan of a drain moves the schemas of the group that may not hold shards, with the reason drain');
	is(q1($n1, qq{SELECT count(*) FROM kingres.plan_rebalance(drain_only => true) WHERE source_group <> $busy}), 0, 'B: only from that group');
	is(q1($n1, qq{SELECT count(*) FROM kingres.plan_rebalance(drain_only => true) WHERE target_group = $busy}), 0, 'B: never to it');
	my ($rc, $out, $err) = run_sql($n1, qq{SELECT kingres.remove_node($drain_node)});
	isnt($rc, 0, 'B: a group that owns a schema cannot be removed');
	like($err, qr/owns \d+ buckets/, 'B: ... and the message says it owns buckets') or diag($err);
	q1($n1, q{SET kingres_shard.schema_placement_group = -1; CREATE SCHEMA fresh_a; CREATE SCHEMA fresh_b; CREATE SCHEMA fresh_c});
	is(q1($n1, qq{SELECT count(*) FROM kingres.schemas WHERE schema_name LIKE 'fresh%' AND group_id = $busy}), 0, 'B: a new schema is not placed on the group that may not hold shards');
}
{
	for (1 .. 10)
	{
		my $m = q1($n1, q{SELECT colocation_id || ' ' || bucket || ' ' || target_group FROM kingres.plan_rebalance(drain_only => true, max_moves => 1)});
		last if $m eq '';
		my ($c, $b, $t) = split / /, $m;
		my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($c, $b, $t)");
		is($rc, 0, "B: the drain moves colocation group $c to group $t") or diag($err);
	}
	is(q1($n1, qq{SELECT count(*) FROM kingres.drain_status() WHERE group_id = $busy AND buckets = 0 AND drained}), 1, 'B: the group is drained');
	is(q1($n1, qq{SELECT count(*) FROM kingres.schemas_on_group($busy)}), 0, 'B: and holds no schema');
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	my ($rc, $out, $err) = run_sql($n1, qq{SELECT kingres.remove_node($drain_node)});
	is($rc, 0, 'B: the drained group can be removed') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM s1.t}), 20000, 'B: the rows are all there');
}

done_testing();
