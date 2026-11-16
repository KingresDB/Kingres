# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/075_decision_kept.pl
#
# Review S9-01: a commit decision is never forgotten while the participant that it
# names might still come back with the transaction prepared.  The second phase of
# a participant (COMMIT PREPARED) that its standby did not confirm leaves the
# transaction committed everywhere; if the participant then fails over to a standby
# that has the PREPARE and not the COMMIT PREPARED, recovery must commit the part
# again, however long after the commit that is.  Until the fix the decision row
# went after decision_retention and recovery rolled the part back, with no record.
#
# decision_retention is 3 seconds here; every scenario waits until the decision is
# older than that (a poll on the age of the row, not a sleep) before it acts.
#
# 1. the review's script: one unconfirmed participant, a second one that confirmed;
#    the unconfirmed one fails over after the retention
# 2. every node restarted and the retention shortened to nothing: the decision stays
#    until the standby confirms, then it goes
# 3. the origin itself fails over between the commit and the participant's failover
# 4. a prepared transaction with no decision that is older than the retention is
#    rolled back and recorded (the second line of defence, for a participant that
#    requires no confirmation)

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

# Three groups; the standbys of groups 1 and 2 are the synchronous ones of their primaries.
sub setup
{
	my $prefix = 'dk' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, replication => 1,
		extra => "kingres_shard.decision_retention = 3\n");
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, "${prefix}_s2");
	my $id2 = register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	wait_catchup($n2, $s2);
	my $s3 = make_standby($n3, "${prefix}_s3");
	my $id3 = register_standby($n1, $s3, 2);
	set_sync($n3, $s3);
	wait_catchup($n3, $s3);
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kb) = keys_of_group($n1, 1, 1, 1);
	my ($kc) = keys_of_group($n1, 1, 2, 1);
	return { prefix => $prefix, n1 => $n1, n2 => $n2, n3 => $n3, s2 => $s2, s3 => $s3,
		id2 => $id2, id3 => $id3, ka => $ka, kb => $kb, kc => $kc };
}

# A transaction of $origin that writes in the three groups and whose COMMIT PREPARED on
# node 3 is cancelled while the standby of node 3 is down: committed everywhere, the
# part of node 3 not confirmed.  The standby of node 2 is up, so node 2 confirms.
sub unconfirmed_commit
{
	my ($c, $origin, @keys) = @_;
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_$c->{prefix}";
	append_to_file($gate, 'x');
	my $sql = "SET kingres_shard.node_timeout = 20; SET kingres_shard.fault_injection = 'after_log:wait:$gate'; BEGIN; "
	  . join(' ', map { "INSERT INTO public.kv VALUES ($_, 'v1');" } @keys) . " COMMIT;";
	my $tx = start_psql($origin, $sql);
	ok(wait_for_fault_file("$gate.reached", 60), "[$c->{prefix}] the origin is between the PREPARE and its decision");
	$c->{s3}->stop;
	unlink $gate;
	ok($c->{n3}->poll_query_until('postgres',
			"SELECT count(*) = 1 FROM pg_stat_activity WHERE wait_event = 'SyncRep' AND query LIKE 'COMMIT PREPARED%'"),
		"[$c->{prefix}] the COMMIT PREPARED of node 3 waits for its standby");
	$c->{n3}->safe_psql('postgres', "SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep'");
	my ($out, $err) = finish_psql($tx);
	like($err, qr/has not confirmed the commit/, "[$c->{prefix}] the client is told that node 3's standby has not confirmed");
	# The decision rows of this transaction, by node.
	for my $p (2, 3)
	{
		$c->{"g$p"} = $origin->safe_psql('postgres',
			"SELECT gid FROM kingres.dtx_log WHERE node_id = $p ORDER BY prepared_at DESC LIMIT 1");
	}
	return;
}

# Wait until every decision row of $node is older than the retention.
sub age_decisions
{
	my ($node, $prefix) = @_;
	ok($node->poll_query_until('postgres',
			"SELECT count(*) > 0 AND bool_and(prepared_at < now() - interval '4 seconds') FROM kingres.dtx_log"),
		"[$prefix] the decisions are older than decision_retention");
}

sub logged { my ($node, $gid) = @_; return $node->safe_psql('postgres', "SELECT count(*) FROM kingres.dtx_log WHERE gid = '$gid'"); }

sub checks
{
	my ($node, $name) = @_;
	return $node->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name = '$name'");
}

sub failover
{
	my ($map, $old, $standby, $id) = @_;
	$old->stop('immediate');
	$standby->start unless $standby->{_pid};
	promote_standby($standby);
	set_sync($standby);
	$map->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	$map->safe_psql('postgres', "SELECT kingres.promote_node($id)");
}

# --- 1. the review's script, with a second participant that confirmed --------------------
{
	my $c = setup();
	my ($n1, $n2, $n3, $s3) = @$c{qw(n1 n2 n3 s3)};
	unconfirmed_commit($c, $n1, $c->{ka}, $c->{kb}, $c->{kc});
	age_decisions($n1, $c->{prefix});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(logged($n1, $c->{g2}), '0', "[$c->{prefix}] the confirmed participant's decision was forgotten after the retention");
	is(logged($n1, $c->{g3}), '1', "[$c->{prefix}] the unconfirmed participant's decision is kept after the retention (S9-01)");
	is(checks($n1, 'decision_unconfirmed'), '1', "[$c->{prefix}] check_cluster() reports it");
	# The retention is no limit while it is unconfirmed: again, and with a pass in between.
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(logged($n1, $c->{g3}), '1', "[$c->{prefix}] and a second pass keeps it");

	# The participant fails over to the standby that has the PREPARE and not the commit.
	failover($n1, $n3, $s3, $c->{id3});
	# promote_node runs the recovery of the origin first, which commits the part that came back prepared.
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared($s3), '0', "[$c->{prefix}] recovery commits it again from the kept decision");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($c->{ka}, $c->{kb}, $c->{kc})"), '3',
		"[$c->{prefix}] no part of the committed transaction is lost");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_incident'), '0', "[$c->{prefix}] and there is nothing to record");
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(logged($n1, $c->{g3}), '0', "[$c->{prefix}] the decision goes once the part is committed and its node is not the primary");
	is(checks($n1, 'decision_unconfirmed'), '0', "[$c->{prefix}] and the report is gone");
}

# --- 2. every node restarted, the retention shortened: unconfirmed means kept --------------
{
	my $c = setup();
	my ($n1, $n2, $n3, $s2, $s3) = @$c{qw(n1 n2 n3 s2 s3)};
	unconfirmed_commit($c, $n1, $c->{ka}, $c->{kc});
	$_->stop foreach ($n1, $n2, $n3, $s2);
	$_->start foreach ($n3, $n2, $s2, $n1);
	$n1->append_conf('postgresql.conf', "kingres_shard.decision_retention = 0\n");
	$n1->reload;
	age_decisions($n1, $c->{prefix});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(logged($n1, $c->{g3}), '1', "[$c->{prefix}] after a restart of every node and a retention of 0 the decision is still kept");
	is(checks($n1, 'decision_unconfirmed'), '1', "[$c->{prefix}] and reported");
	$s3->start;
	wait_catchup($n3, $s3);
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(logged($n1, $c->{g3}), '0', "[$c->{prefix}] once the standby confirms, the retention applies and the decision goes");
	is(checks($n1, 'decision_unconfirmed'), '0', "[$c->{prefix}] and the report is gone");
}

# --- 3. the origin fails over between the commit and the participant's failover -----------
{
	my $c = setup();
	my ($n1, $n2, $n3, $s2, $s3) = @$c{qw(n1 n2 n3 s2 s3)};
	# Node 2 is the origin: a row of its own group and one of group 2.
	unconfirmed_commit($c, $n2, $c->{kb}, $c->{kc});
	age_decisions($n2, $c->{prefix});
	$n2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(logged($n2, $c->{g3}), '1', "[$c->{prefix}] the origin keeps the decision past the retention");
	wait_catchup($n2, $s2);
	failover($n1, $n3, $s3, $c->{id3});
	failover($n1, $n2, $s2, $c->{id2});
	is(prepared($s3), '1', "[$c->{prefix}] the part of node 3 is prepared on its new primary");
	$s2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared($s3), '0', "[$c->{prefix}] the new origin commits it from the decision it was sent");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($c->{kb}, $c->{kc})"), '2',
		"[$c->{prefix}] both parts are there");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_incident') . $s2->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_incident'),
		'00', "[$c->{prefix}] and there is nothing to record");
}

# --- 4. no decision and older than the retention: rolled back, and recorded ---------------
{
	my $c = setup();
	my ($n1, $n2, $n3, $s3) = @$c{qw(n1 n2 n3 s3)};
	# The origin dies after the PREPARE of both participants and before its decision.
	my $tx = start_psql($n1,
		"SET kingres_shard.fault_injection = 'after_log:kill'; BEGIN; "
		  . "INSERT INTO public.kv VALUES ($c->{ka}, 'a'); INSERT INTO public.kv VALUES ($c->{kc}, 'c'); COMMIT;");
	finish_psql($tx);
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	is(prepared($n3), '1', "[$c->{prefix}] the participant is prepared and its origin has no decision");
	ok($n3->poll_query_until('postgres', "SELECT count(*) = 1 FROM pg_prepared_xacts WHERE prepared < now() - interval '4 seconds'"),
		"[$c->{prefix}] it is older than the retention");
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared($n3), '0', "[$c->{prefix}] recovery rolls it back (nothing was decided)");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.dtx_incident WHERE kind = 'rolled_back_after_retention'"), '1',
		"[$c->{prefix}] and records that it could not tell a forgotten decision from none");
	is(checks($n1, 'prepared_rolled_back_after_failover'), '1', "[$c->{prefix}] check_cluster() shows it");
}

done_testing();
