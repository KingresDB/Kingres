# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/062_participant_sync.pl
#
# Review S7-07: a participant's PREPARE TRANSACTION, and the COMMIT PREPARED or
# ROLLBACK PREPARED that follows, are acknowledged by the participant's own
# synchronous standby whatever synchronous_commit the participant's role, database
# or server says.  The participant is node 3 (group 2) with a synchronous standby;
# its role setting is "off", which used to let PREPARE and COMMIT PREPARED return
# without the standby.  The standby is stopped at the moment under test, and the
# participant's backend must wait for it.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group make_standby
  register_standby wait_catchup set_sync wait_for_fault_file
  start_psql finish_psql);

my $seq = 0;

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

sub setup
{
	my $prefix = 'ps' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s3 = make_standby($n3, "${prefix}_s3");
	register_standby($n1, $s3, 2);
	set_sync($n3, $s3);
	wait_catchup($n3, $s3);
	# The role of the transaction asks for asynchronous commits on the participant.
	$n3->safe_psql('postgres', 'ALTER ROLE CURRENT_USER SET synchronous_commit = off');
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kc) = keys_of_group($n1, 1, 2, 1);
	return ($prefix, $n1, $n3, $s3, $ka, $kc);
}

# The query of the participant's backend that waits for its standby.
sub waiting_query
{
	my ($n) = @_;
	ok($n->poll_query_until('postgres',
			"SELECT count(*) = 1 FROM pg_stat_activity WHERE wait_event = 'SyncRep'"),
		'a backend of the participant waits for its synchronous standby');
	return $n->safe_psql('postgres', "SELECT query FROM pg_stat_activity WHERE wait_event = 'SyncRep'");
}

# --- 1. PREPARE TRANSACTION waits for the participant's standby ------------------
{
	my ($prefix, $n1, $n3, $s3, $ka, $kc) = setup();
	$s3->stop;
	my $tx = start_psql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	like(waiting_query($n3), qr/PREPARE TRANSACTION/, "[$prefix] the PREPARE of the participant waits although its role says off");
	$s3->start;
	wait_catchup($n3, $s3);
	my ($out, $err) = finish_psql($tx);
	is($err, '', "[$prefix] the commit goes through once the standby has the PREPARE");
	is(prepared($n1) . prepared($n3), '00', "[$prefix] nothing is left prepared");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($ka, $kc)"), '2', "[$prefix] both rows are committed");
}

# --- 2. COMMIT PREPARED of the origin's second phase waits for the participant's standby -
{
	my ($prefix, $n1, $n3, $s3, $ka, $kc) = setup();
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_${prefix}";
	append_to_file($gate, 'x');
	my $tx = start_psql($n1,
		"SET kingres_shard.fault_injection = 'after_log:wait:$gate'; BEGIN; "
		  . "INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	ok(wait_for_fault_file("$gate.reached", 60), "[$prefix] the origin is between the PREPARE and its decision");
	$s3->stop;
	unlink $gate;
	like(waiting_query($n3), qr/COMMIT PREPARED/, "[$prefix] the COMMIT PREPARED of the participant waits for its standby");
	is(prepared($n3), '1', "[$prefix] the participant is still prepared while it waits");
	$s3->start;
	wait_catchup($n3, $s3);
	finish_psql($tx);
	is(prepared($n3), '0', "[$prefix] committed once the standby has it");
}

# --- 3. the COMMIT PREPARED of recovery waits for the participant's standby ----
{
	my ($prefix, $n1, $n3, $s3, $ka, $kc) = setup();
	my $tx = start_psql($n1,
		"SET kingres_shard.fault_injection = 'after_commit:kill'; BEGIN; "
		  . "INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	finish_psql($tx);
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	is(prepared($n3), '1', "[$prefix] the participant is prepared, its origin crashed after the decision");
	$s3->stop;
	my $rec = start_psql($n1, 'SELECT kingres.recover_prepared_transactions()');
	like(waiting_query($n3), qr/COMMIT PREPARED/, "[$prefix] the COMMIT PREPARED of recovery waits for the participant's standby");
	$s3->start;
	wait_catchup($n3, $s3);
	my ($out) = finish_psql($rec);
	is($out, "1\n", "[$prefix] recovery committed it once the standby had it");
	is(prepared($n3), '0', "[$prefix] nothing is left prepared");
}

done_testing();
