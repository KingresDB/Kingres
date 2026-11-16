# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/070_participant_durability.pl
#
# Review S8-01 and S8-08: the origin of a distributed transaction treats the
# PREPARE TRANSACTION of a participant as done only when the participant has
# confirmed that the record is durable on its own synchronous standby
# (kingres.internal_sync_confirm).  PostgreSQL ends the wait of a PREPARE
# TRANSACTION or a COMMIT PREPARED for a cancel, a termination or a shutdown
# with a warning and a result of success, which the origin cannot see; the part of
# a committed transaction on a participant that then failed over was gone, and the
# client had been told COMMIT.
#
# The participant is node 3 (group 2) with a synchronous standby.  Every way in which
# the wait can end early is made on the participant: the cancel of its backend, the
# termination, a fast shutdown, the emptied list of standbys (the documented weaker
# case), the unbounded wait (S8-08, bounded by node_timeout), the cancelled wait of
# the second phase, and the COMMIT PREPARED of recovery.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group make_standby
  register_standby wait_catchup set_sync promote_standby wait_for_fault_file
  start_psql finish_psql);

my $seq = 0;

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

sub setup
{
	my $prefix = 'pd' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s3 = make_standby($n3, "${prefix}_s3");
	my $id3 = register_standby($n1, $s3, 2);
	set_sync($n3, $s3);
	wait_catchup($n3, $s3);
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kc) = keys_of_group($n1, 1, 2, 1);
	return ($prefix, $n1, $n3, $s3, $id3, $ka, $kc);
}

sub waiting_query
{
	my ($n) = @_;
	ok($n->poll_query_until('postgres',
			"SELECT count(*) = 1 FROM pg_stat_activity WHERE wait_event = 'SyncRep'"),
		'a backend of the participant waits for its synchronous standby');
	return $n->safe_psql('postgres', "SELECT query FROM pg_stat_activity WHERE wait_event = 'SyncRep'");
}

# A distributed commit of node 1 with the standby of the participant stopped.
sub stalled_commit
{
	my ($n1, $s3, $ka, $kc, $timeout) = @_;
	$s3->stop;
	return start_psql($n1,
		"SET kingres_shard.node_timeout = $timeout; BEGIN; INSERT INTO public.kv VALUES ($ka, 'a'); "
		  . "INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
}

# Nothing of the transaction is committed anywhere, nothing is left prepared once
# the standby is back and recovery has run, and no incident is recorded.
sub check_aborted
{
	my ($prefix, $n1, $n3, $s3, $ka, $kc) = @_;
	$s3->start;
	wait_catchup($n3, $s3);
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared($n3), '0', "[$prefix] nothing is left prepared on the participant");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($ka, $kc)"), '0',
		"[$prefix] no part of the transaction is committed anywhere");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_incident'), '0', "[$prefix] no incident is recorded");
}

# --- 1. the cancel of the participant's wait: the origin refuses the commit ------
{
	my ($prefix, $n1, $n3, $s3, $id3, $ka, $kc) = setup();
	my $tx = stalled_commit($n1, $s3, $ka, $kc, 20);
	like(waiting_query($n3), qr/PREPARE TRANSACTION/, "[$prefix] the PREPARE of the participant waits for its standby");
	$n3->safe_psql('postgres', "SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep'");
	my ($out, $err) = finish_psql($tx);
	like($err, qr/did not confirm that its part of the transaction is durable/,
		"[$prefix] the client is told that the participant did not confirm");
	unlike($err, qr/^COMMIT/m, "[$prefix] and not COMMIT");
	check_aborted($prefix, $n1, $n3, $s3, $ka, $kc);
}

# --- 2. nothing cancels the wait: node_timeout bounds it (S8-08) ------------------
{
	my ($prefix, $n1, $n3, $s3, $id3, $ka, $kc) = setup();
	my $tx = stalled_commit($n1, $s3, $ka, $kc, 2);
	my ($out, $err) = finish_psql($tx);
	like($err, qr/did not confirm that its part of the transaction is durable/,
		"[$prefix] the stalled commit ends with an error by itself");
	like($err, qr/kingres_shard\.node_timeout/, "[$prefix] and the error names the setting that bounded it");
	check_aborted($prefix, $n1, $n3, $s3, $ka, $kc);
}

# --- 3. the termination of the participant's backend -----------------------------
{
	my ($prefix, $n1, $n3, $s3, $id3, $ka, $kc) = setup();
	my $tx = stalled_commit($n1, $s3, $ka, $kc, 20);
	waiting_query($n3);
	$n3->safe_psql('postgres', "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep'");
	my ($out, $err) = finish_psql($tx);
	like($err, qr/could not (prepare|confirm)/, "[$prefix] the client gets an error");
	check_aborted($prefix, $n1, $n3, $s3, $ka, $kc);
}

# --- 4. a fast shutdown of the participant during the wait -----------------------
{
	my ($prefix, $n1, $n3, $s3, $id3, $ka, $kc) = setup();
	my $tx = stalled_commit($n1, $s3, $ka, $kc, 20);
	waiting_query($n3);
	$n3->stop('fast');
	my ($out, $err) = finish_psql($tx);
	like($err, qr/could not (prepare|confirm)|connection/, "[$prefix] the client gets an error");
	$n3->start;
	check_aborted($prefix, $n1, $n3, $s3, $ka, $kc);
}

# --- 5. the list of standbys is emptied during the wait: the documented weaker case -
{
	my ($prefix, $n1, $n3, $s3, $id3, $ka, $kc) = setup();
	my $tx = stalled_commit($n1, $s3, $ka, $kc, 20);
	waiting_query($n3);
	set_sync($n3);
	my ($out, $err) = finish_psql($tx);
	is($err, '', "[$prefix] with nothing required of the node the commit goes through, as a commit would");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($ka, $kc)"), '2',
		"[$prefix] and both parts are committed");
}

# --- 6. the cancel of the second phase on the participant -------------------------
{
	my ($prefix, $n1, $n3, $s3, $id3, $ka, $kc) = setup();
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_${prefix}";
	append_to_file($gate, 'x');
	my $tx = start_psql($n1,
		"SET kingres_shard.node_timeout = 20; SET kingres_shard.fault_injection = 'after_log:wait:$gate'; BEGIN; "
		  . "INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	ok(wait_for_fault_file("$gate.reached", 60), "[$prefix] the origin is between the PREPARE and its decision");
	$s3->stop;
	unlink $gate;
	like(waiting_query($n3), qr/COMMIT PREPARED/, "[$prefix] the COMMIT PREPARED of the participant waits for its standby");
	$n3->safe_psql('postgres', "SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep'");
	my ($out, $err) = finish_psql($tx);
	like($err, qr/has not confirmed the commit/, "[$prefix] the client is told that the standby of the participant has not confirmed");
	# The participant fails over to its standby, which has the PREPARE and not the commit.
	$n3->stop('immediate');
	$s3->start;
	promote_standby($s3);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id3)");
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared($s3), '0', "[$prefix] recovery commits it again: the decision was kept");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($ka, $kc)"), '2',
		"[$prefix] no part of the committed transaction is lost");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_incident'), '0', "[$prefix] and there is nothing to record");
}

# --- 7. the COMMIT PREPARED of recovery: the same question ---------------------------
{
	my ($prefix, $n1, $n3, $s3, $id3, $ka, $kc) = setup();
	my $tx = start_psql($n1,
		"SET kingres_shard.fault_injection = 'after_commit:kill'; BEGIN; "
		  . "INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	finish_psql($tx);
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	is(prepared($n3), '1', "[$prefix] the participant is prepared, its origin crashed after the decision");
	my $gid = $n3->safe_psql('postgres', "SELECT gid FROM pg_prepared_xacts");
	$s3->stop;
	my $rec = start_psql($n1, 'SET kingres_shard.node_timeout = 2; SELECT kingres.recover_prepared_transactions()');
	like(waiting_query($n3), qr/COMMIT PREPARED/, "[$prefix] the COMMIT PREPARED of recovery waits for the participant's standby");
	$n3->safe_psql('postgres', "SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep'");
	my ($out, $err) = finish_psql($rec);
	like($out, qr/^1$/m, "[$prefix] recovery committed the part on the participant");
	like(slurp_file($n1->logfile), qr/has not confirmed the commit; the decision is kept/,
		"[$prefix] and noted that the standby had not confirmed it");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.dtx_log WHERE gid = '$gid'"), '1', "[$prefix] the decision is still there");
}

# --- 8. a smart shutdown during the commit wait of the origin (S8-08) --------------------
# The node waits for its standby, and a postmaster that is shutting down refuses the
# connection of that standby, so the wait cannot end: that is how PostgreSQL behaves, not
# a fault of the extension.  A fast shutdown (or a cancel) ends it, and the second phase is
# left to recovery as in t/061.
{
	my ($prefix, $n1, $n3, $s3, $id3, $ka, $kc) = setup();
	$s3->stop;
	# Node 3 is the origin here: its own commit waits for its standby.
	my $tx = start_psql($n3,
		"BEGIN; INSERT INTO public.kv VALUES ($kc, 'c'); INSERT INTO public.kv VALUES ($ka, 'a'); COMMIT;");
	waiting_query($n3);
	system_or_bail('pg_ctl', '-D', $n3->data_dir, '-m', 'smart', '-W', 'stop');
	$s3->start;
	$s3->wait_for_log(qr/the database system is shutting down/);
	pass("[$prefix] the standby is refused by the node that is shutting down");
	sleep 3;
	ok(-f $n3->data_dir . '/postmaster.pid', "[$prefix] the node is still up after the smart shutdown was asked for");
	ok($tx->{h}->pumpable, "[$prefix] the commit still waits: the standby cannot connect to a postmaster that is shutting down");
	$n3->stop('fast');
	finish_psql($tx);
	$n3->start;
	wait_catchup($n3, $s3);
	$n3->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared($n1), '0', "[$prefix] after the restart recovery commits the participant");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($ka, $kc)"), '2',
		"[$prefix] and both parts are committed");
}

done_testing();
