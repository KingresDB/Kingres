# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/061_sync_decision.pl
#
# Review S7-01: the second phase of a distributed transaction is sent only when
# the decision is known to be durable where the group's configuration says it
# must be.  PostgreSQL ends the wait for the synchronous standby with a warning
# when the backend is cancelled or terminated, and the commit completes; the
# origin used to send COMMIT PREPARED anyway, and after a failover the
# participants had a transaction that the promoted standby knew nothing about.
#
# The origin is node 2, the primary of group 1, with a synchronous standby that is
# stopped, so that the commit waits.  The wait is ended by a cancel, by a
# termination of the backend, or by a fast shutdown of the origin; each time the
# participant must stay prepared, and after the failover the promoted standby
# (which never had the commit) makes recovery roll the participant back, with a
# record in dtx_incident.  Another scenario heals the standby instead: recovery
# commits the participant once the decision is confirmed.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group make_standby
  register_standby wait_catchup set_sync promote_standby
  start_psql finish_psql);

my $seq = 0;

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

# A cluster whose node 2 has a synchronous standby that is stopped, and a
# distributed commit of node 2 that waits for it.
sub waiting_commit
{
	my $prefix = 'sd' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, "${prefix}_s2");
	my $id = register_standby($n1, $s2, 1);
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kb) = keys_of_group($n1, 1, 1, 1);
	set_sync($n2, $s2);
	wait_catchup($n2, $s2);
	$s2->stop;
	my $tx = start_psql($n2,
		"BEGIN; INSERT INTO public.kv VALUES ($kb, 'b'); INSERT INTO public.kv VALUES ($ka, 'a'); COMMIT;");
	ok($n2->poll_query_until('postgres',
			"SELECT count(*) = 1 FROM pg_stat_activity WHERE wait_event = 'SyncRep'"),
		"[$prefix] the commit waits for the synchronous standby");
	is(prepared($n1), '1', "[$prefix] the participant has prepared");
	return ($prefix, $n1, $n2, $n3, $s2, $id, $ka, $kb, $tx);
}

# After the wait was ended without the standby: the participant is still prepared
# and nothing is visible there.
sub check_deferred
{
	my ($prefix, $n1, $ka) = @_;
	is(prepared($n1), '1', "[$prefix] the participant is still prepared (no COMMIT PREPARED was sent)");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $ka"), '0',
		"[$prefix] its row is not visible");
}

# Failover: the standby never had the commit; recovery rolls the participant back.
sub failover_and_resolve
{
	my ($prefix, $n1, $n2, $s2, $id, $ka, $kb) = @_;
	$n2->stop('immediate');
	$s2->start;
	promote_standby($s2);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	$s2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	$s2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared($n1), '0', "[$prefix] after the failover nothing is left prepared");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($ka, $kb)"), '0',
		"[$prefix] the transaction is rolled back everywhere: the promoted standby does not know it, nobody committed it");
	is($s2->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_incident'), '1',
		"[$prefix] and the rollback of the earlier-timeline transaction is recorded");
}

# --- 1. cancel ---------------------------------------------------------------
{
	my ($prefix, $n1, $n2, $n3, $s2, $id, $ka, $kb, $tx) = waiting_commit();
	$n2->safe_psql('postgres',
		"SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep'");
	my ($out, $err) = finish_psql($tx);
	like($err, qr/canceling wait for synchronous replication/, "[$prefix] PostgreSQL warns about the cancelled wait");
	like($err, qr/second phase was not sent/, "[$prefix] and the extension says that the outcome is left to recovery");
	check_deferred($prefix, $n1, $ka);
	failover_and_resolve($prefix, $n1, $n2, $s2, $id, $ka, $kb);
}

# --- 2. termination of the backend ------------------------------------------
{
	my ($prefix, $n1, $n2, $n3, $s2, $id, $ka, $kb, $tx) = waiting_commit();
	$n2->safe_psql('postgres',
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep'");
	finish_psql($tx);
	ok($n2->poll_query_until('postgres',
			"SELECT count(*) = 0 FROM pg_stat_activity WHERE wait_event = 'SyncRep'"),
		"[$prefix] the terminated backend is gone");
	check_deferred($prefix, $n1, $ka);
	failover_and_resolve($prefix, $n1, $n2, $s2, $id, $ka, $kb);
}

# --- 3. shutdown of the origin during the wait ---------------------------------
{
	my ($prefix, $n1, $n2, $n3, $s2, $id, $ka, $kb, $tx) = waiting_commit();
	$n2->stop('fast');
	finish_psql($tx);
	check_deferred($prefix, $n1, $ka);
	failover_and_resolve($prefix, $n1, $n2, $s2, $id, $ka, $kb);
}

# --- 4. the standby comes back: recovery commits once the decision is confirmed ---
{
	my ($prefix, $n1, $n2, $n3, $s2, $id, $ka, $kb, $tx) = waiting_commit();
	$n2->safe_psql('postgres',
		"SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep'");
	finish_psql($tx);
	check_deferred($prefix, $n1, $ka);
	# While the standby is away recovery must not commit: the decision is not confirmed.
	$n2->safe_psql('postgres', 'SET kingres_shard.node_timeout = 1; SELECT kingres.recover_prepared_transactions()');
	is(prepared($n1), '1', "[$prefix] recovery does not commit while the standby has not confirmed the decision");
	$s2->start;
	wait_catchup($n2, $s2);
	is($n2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '1',
		"[$prefix] once the standby has the decision, recovery commits the participant");
	is(prepared($n1), '0', "[$prefix] nothing is left prepared");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($ka, $kb)"), '2',
		"[$prefix] the transaction is committed everywhere");
}

# --- 5. a healthy standby: the second phase is sent at once --------------------
{
	my $prefix = 'sd' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, "${prefix}_s2");
	register_standby($n1, $s2, 1);
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kb) = keys_of_group($n1, 1, 1, 1);
	set_sync($n2, $s2);
	wait_catchup($n2, $s2);
	$n2->safe_psql('postgres',
		"BEGIN; INSERT INTO public.kv VALUES ($kb, 'b'); INSERT INTO public.kv VALUES ($ka, 'a'); COMMIT;");
	is(prepared($n1), '0', "[$prefix] with a healthy standby the participant is committed when COMMIT returns");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($ka, $kb)"), '2',
		"[$prefix] and the rows are visible");
}

done_testing();
