# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/073_sequences.pl
#
# The call sequences of the README ("What the control plane calls") that no other
# file runs end to end:
#   A. a planned switchover with no loss (the old primary stopped cleanly, the standby
#      promoted, the old primary re-added as a standby) and the restoring of the
#      preferred primary (the same again, the other way);
#   B. removing a dead standby that is named in synchronous_standby_names: the commits
#      that touch its group fail by themselves with an error (they do not hang), and
#      work again once the list is changed;
#   C. choosing among several standbys: by the position each has received.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql make_standby
  register_standby wait_catchup set_sync promote_standby);

sub roles
{
	my ($node) = @_;
	return $node->safe_psql('postgres',
		"SELECT string_agg(node_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node");
}

# ---------------------------------------------------------------------------
# A. planned switchover and the way back
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('sa', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, 'sa_s2');
	my $ids2 = register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	wait_catchup($n2, $s2);
	my @k = keys_of_group($n1, 1, 1, 4);
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($k[0], 'before')");

	# Writes of the application have stopped (here: nobody writes).  A clean stop of the
	# old primary sends everything it has to its standby before it ends.
	$n2->stop('fast');
	promote_standby($s2);
	my $out = $n1->safe_psql('postgres', "SELECT kingres.promote_node($ids2)");
	like($out, qr/"status": "promoted"/, '[A] the switchover is a promote_node of the standby');
	like($out, qr/"unreached": \[2\]/, '[A] and the old primary, which is stopped, is listed as not told');
	is($n1->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $k[0]"), 'before', '[A] nothing was lost');
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($k[1], 'new primary')");
	is($s2->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $k[1]"), 'new primary', '[A] and writes go to the new primary');

	# The old primary is re-added as a standby of the new one.
	$n2->enable_streaming($s2);
	$n2->start;
	$s2->wait_for_catchup($n2);
	is($n1->safe_psql('postgres', "SELECT replication_state FROM kingres.standby_status() WHERE node_id = 2"), 'streaming',
		'[A] standby_status shows the old primary streaming from the new one');
	is($n2->safe_psql('postgres', 'SELECT term FROM kingres.catalog_status'), '1', '[A] with the new catalog term');
	is(roles($n2), roles($n1), '[A] and the same roles');

	# The way back: the preferred primary again.
	$s2->stop('fast');
	promote_standby($n2);
	# The list that n2 had as a primary names the standby that is now stopped (precondition 3).
	set_sync($n2);
	$out = $n1->safe_psql('postgres', "SELECT kingres.promote_node(2)");
	like($out, qr/"status": "promoted"/, '[A] restoring the preferred primary is a promote_node of its node id');
	like($out, qr/"term": 2/, '[A] at the next term');
	like($out, qr/"unreached": \[$ids2\]/, '[A] the stopped node is listed');
	is($n2->safe_psql('postgres', 'SELECT node_id FROM kingres.local_node'), '2', '[A] the node knows itself as node 2');
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($k[2], 'back')");
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($k[0], $k[1], $k[2])"), '3',
		'[A] every row is on the preferred primary again');
}

# ---------------------------------------------------------------------------
# B. removing a standby that is dead and named in synchronous_standby_names
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('sb', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s3 = make_standby($n3, 'sb_s3');
	my $id = register_standby($n1, $s3, 2);
	set_sync($n3, $s3);
	wait_catchup($n3, $s3);
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kc) = keys_of_group($n1, 1, 2, 1);
	$s3->stop;
	my ($rc, $out, $err) = run_sql($n1,
		"SET kingres_shard.node_timeout = 2; BEGIN; INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	isnt($rc, 0, '[B] a commit that touches the group of the dead standby fails by itself');
	like($err, qr/did not confirm that its part of the transaction is durable/, '[B] with the reason');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	set_sync($n3);
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	is($rc, 0, '[B] with the list changed on the primary the commit works');
	is($n1->safe_psql('postgres', "SELECT kingres.remove_node($id)"), '', '[B] the standby is then removed from the catalog');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.standby_status()'), '0', '[B] it is gone from standby_status');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name LIKE 'standby%'"), '0',
		'[B] and from check_cluster');
}

# ---------------------------------------------------------------------------
# C. several standbys: the one that has received the most
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('sc', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $sa = make_standby($n2, 'sc_sa');
	my $sb = make_standby($n2, 'sc_sb');
	my $ida = register_standby($n1, $sa, 1);
	my $idb = register_standby($n1, $sb, 1);
	set_sync($n2, $sa, $sb);
	wait_catchup($n2, $sa);
	wait_catchup($n2, $sb);
	$sa->stop;
	my ($k) = keys_of_group($n1, 1, 1, 1);
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($k, 'only sb has it')");
	$n2->stop('immediate');
	# Each standby is asked for the position it has flushed.  A stopped one is asked
	# after it has been started again, and does not have the write.
	$sa->start;
	my $lsn_b = $sb->safe_psql('postgres', 'SELECT pg_last_wal_receive_lsn()');
	my $lsn_a = $sa->safe_psql('postgres', 'SELECT pg_last_wal_receive_lsn()');
	is($sb->safe_psql('postgres', "SELECT pg_wal_lsn_diff('$lsn_b', '$lsn_a') > 0"), 't',
		'[C] the standby that was acknowledging has received more than the one that was away');
	promote_standby($sb);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($idb)");
	is($n1->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $k"), 'only sb has it',
		'[C] promoting that one loses nothing');
}

done_testing();
