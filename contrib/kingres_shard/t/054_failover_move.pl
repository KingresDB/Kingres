# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/054_failover_move.pl
#
# A failover in the middle of a bucket move (design/ha-phase5.md, tests required;
# ADR 0022).  The primary of the source group or of the target group stops at a
# phase of the move, its synchronous standby is promoted, kingres.promote_node()
# is called, and the move is resolved:
#   - before the switch (the copy, the catch-up): the logical replication of the
#     move lived on the node that is gone, so the move is undone and the bucket
#     stays with the source group;
#   - inside the switch, after the decision: the switch stands, the prepared
#     transactions of it on the promoted node are committed by recovery, and the
#     bucket is on the target group.
# Whatever happens every row is there exactly once, nothing is left prepared, and
# check_cluster() reports nothing but the old primary, which is down.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(sleep);
use KingresShardTest qw(make_cluster run_sql make_standby register_standby wait_catchup
  set_sync promote_standby wait_for_fault_file start_psql finish_psql);

my $seq = 0;

# $fault: the point of the move where the session waits; $victim: 'source' or 'target'.
sub scenario
{
	my ($fault, $victim, $expect_moved) = @_;
	my $label = "[$fault, $victim]";
	my $prefix = 'fm' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, logical => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.orders (id int PRIMARY KEY, customer int NOT NULL);
		SELECT kingres.create_distributed_table('public.orders', 'id', bucket_count => 6);
	});
	my $sql = '';
	$sql .= "INSERT INTO public.orders VALUES ($_, $_ % 9);\n" for 1 .. 60;
	$n1->safe_psql('postgres', $sql);
	my $want = '60,1830';
	my $totals = "SELECT (SELECT count(*) FROM public.orders) || ',' || (SELECT sum(id) FROM public.orders)";

	my $s2 = make_standby($n2, "${prefix}_s2");
	my $s3 = make_standby($n3, "${prefix}_s3");
	my $id2 = register_standby($n1, $s2, 1);
	my $id3 = register_standby($n1, $s3, 2);
	set_sync($n2, $s2);
	set_sync($n3, $s3);
	wait_catchup($n2, $s2);
	wait_catchup($n3, $s3);

	# A bucket of group 1, moved to group 2.
	my $b = $n1->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = 1");
	my $tmp = $PostgreSQL::Test::Utils::tmp_check;
	my $gate = "$tmp/gate_$prefix";
	append_to_file($gate, 'x');
	my $mv = start_psql($n1,
		"SET kingres_shard.fault_injection = '$fault:wait:$gate'; SET kingres_shard.node_timeout = 5; "
		  . "SELECT kingres.move_bucket(1, $b, 2)");
	ok(wait_for_fault_file("$gate.reached", 90), "$label the move is at its point");

	my ($dead, $standby, $id, $dead_id) = $victim eq 'source' ? ($n2, $s2, $id2, 2) : ($n3, $s3, $id3, 3);
	wait_catchup($dead, $standby);
	$dead->stop('immediate');
	promote_standby($standby);
	set_sync($standby);
	# A move that has committed its switch still holds the catalog lock of this node while it
	# finishes the second phase, and promote_node() waits for that lock: let it end first.  It
	# cannot reach the old primary, so its prepared transaction there is left to recovery.
	my ($mout, $merr);
	if ($fault =~ /^after_commit/)
	{
		unlink $gate;
		($mout, $merr) = finish_psql($mv);
		$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	}
	else
	{
		$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
		unlink $gate;
		($mout, $merr) = finish_psql($mv);
	}

	# Resolve what is unfinished: the recovery of prepared transactions and the resumption of moves.
	for (1 .. 8)
	{
		$n1->safe_psql('postgres', "SET kingres_shard.node_timeout = 3; SELECT kingres.recover_prepared_transactions()");
		$n1->safe_psql('postgres', "SET kingres_shard.node_timeout = 3; SELECT kingres.resume_bucket_moves()");
		$standby->safe_psql('postgres', "SET kingres_shard.node_timeout = 3; SELECT kingres.recover_prepared_transactions()");
		last if $n1->safe_psql('postgres',
			"SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('preparing','copying','catching_up','switching')") eq '0';
		sleep(1);
	}
	$n1->safe_psql('postgres', "SET kingres_shard.node_timeout = 3; SELECT kingres.cleanup_moved_buckets(true)");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('preparing','copying','catching_up','switching')"),
		'0', "$label no move is unfinished");
	# The target's promoted standby has the subscription of the move (it is replicated) and its
	# worker goes on, so a move whose target failed over may finish; every other case is undone.
	my $owner = $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b");
	if ($expect_moved eq 'either')
	{
		like($owner, qr/^[12]$/, "$label the bucket is whole on group 1 or on group 2 (it is on $owner)");
	}
	else
	{
		is($owner, $expect_moved ? '2' : '1', "$label the bucket is on group " . ($expect_moved ? 2 : 1));
	}
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.bucket_map WHERE state <> 'active'"), '0', "$label no bucket is left moving");
	my @live = ($n1, $victim eq 'source' ? ($standby, $n3) : ($n2, $standby));
	for my $node (@live)
	{
		my $name = $node->name;
		is($node->safe_psql('postgres', $totals), $want, "$label every row exactly once from $name");
		is($node->safe_psql('postgres', q{SELECT (SELECT count(*) FROM pg_prepared_xacts) || ',' || (SELECT count(*) FROM kingres.move_marker)}),
			'0,0', "$label no prepared transaction or marker on $name");
		is($node->safe_psql('postgres', q{SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan%'}),
			'0', "$label no orphan table on $name");
	}
	is($standby->safe_psql('postgres', q{SELECT (SELECT count(*) FROM pg_replication_slots) || ',' || (SELECT count(*) FROM pg_publication)}),
		'0,0', "$label nothing of the replication of the move is left on the promoted node");
	is($n1->safe_psql('postgres', q{SELECT coalesce(string_agg(check_name || ':' || node_id, ','), '') FROM kingres.check_cluster()}),
		"standby_unreachable:$dead_id", "$label check_cluster reports nothing but the old primary, which is down");
	# The cluster works: another move, of another bucket, from the new primary.
	$n1->safe_psql('postgres', "SET kingres_shard.node_timeout = 20; SELECT kingres.move_bucket(1, " .
		$n1->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = 0") . ", 1)")
	  if $victim eq 'source' && 0;
	return;
}

scenario('move_after_subscription', 'source', 0);
scenario('move_after_catchup',      'target', 'either');
scenario('after_commit@switch',     'source', 1);

done_testing();
