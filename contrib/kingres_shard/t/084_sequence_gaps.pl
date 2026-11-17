# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/084_sequence_gaps.pl
#
# The answers of the call sequences in the README (what the control plane calls) that no
# other test produces in the situation the sequence names, each with the exact text:
#
#  A. registering a standby: the warning for a standby that cannot be reached, the refusal of
#     add_node when a primary cannot commit, the name check (standby_name_matches), and
#     check_cluster() / standby_status() for a standby that was promoted without the catalog
#  B. promote_node of a standby that does not accept connections; KS005 with its SQLSTATE
#  C. the map group: a node that is not a copy of the primary of group 0, a node registered on
#     another port, a node that holds a newer catalog term, a standby that is still in recovery

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group make_standby register_standby wait_catchup
  set_sync promote_standby run_sql);

# --- A ------------------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('sg1', 3, replication => 1);
	my $s2 = make_standby($n2, 'sg1_s2');
	wait_catchup($n2, $s2);
	$s2->stop;
	my ($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d, 1, 'standby')", $s2->host, $s2->port));
	is($rc, 0, '[A] a standby that cannot be reached is registered');
	like($err, qr/WARNING:  standby \S+:\d+ could not be reached; it is registered anyway/, '[A] with the warning');
	my $id = $out;
	set_sync($n2, $s2);
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.add_node('127.0.0.1', 1)");
	like($err, qr/node 2 cannot commit: its synchronous standbys do not confirm/,
		'[A] add_node is refused at once when a primary cannot commit');
	set_sync($n2);
	$s2->start;
	wait_catchup($n2, $s2);
	is($n2->safe_psql('postgres', sprintf("SELECT kingres.standby_name_matches('%s')", $s2->name)), 't',
		'[A] standby_name_matches: the primary sees a streaming standby under the name');
	is($n2->safe_psql('postgres', sprintf("SELECT kingres.standby_name_matches('%s_typo')", $s2->name)), 'f',
		'[A] a typo matches nobody');
	is($n3->safe_psql('postgres', sprintf("SELECT kingres.standby_name_matches('%s')", $s2->name)), 'f',
		'[A] and a primary whose group has no standby answers false');
	# A standby promoted in PostgreSQL and not by the catalog.
	promote_standby($s2);
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'standby_not_in_recovery' AND node_id = $id"),
		'1', '[A] check_cluster() reports standby_not_in_recovery for a standby that runs as a primary');
	is($n1->safe_psql('postgres', "SELECT problem FROM kingres.standby_status() WHERE node_id = $id"), 'standby_not_in_recovery',
		'[A] and so does standby_status()');
}

# --- B ------------------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('sg2', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, 'sg2_s2');
	my $id = register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	wait_catchup($n2, $s2);
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kb) = keys_of_group($n1, 1, 1, 1);
	$s2->stop;
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($id)");
	like($err, qr/could not connect to node $id \([^)]*\), which is to become the primary of group 1/,
		'[B] promote_node of a standby that does not accept connections');
	($rc, $out, $err) = run_sql($n1,
		"SET kingres_shard.node_timeout = 2; BEGIN; INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kb, 'b'); COMMIT;",
		extra_params => [ '-v', 'VERBOSITY=verbose' ]);
	like($err, qr/ERROR:  KS005: node \S+:\d+ did not confirm that its part of the transaction is durable on its synchronous standby/,
		'[B] a participant whose standby is away: KS005 with that text');
}

# --- D: recovery in a session of a user does not wait for ever for a standby that is away ---
# (the hang of t/073 in the tree build: ROLLBACK PREPARED or COMMIT PREPARED of recovery waits for the
# participant's standby, and a session has no timeout for that wait)
{
	my ($n1, $n2, $n3) = make_cluster('sg4', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s3 = make_standby($n3, 'sg4_s3');
	my $id = register_standby($n1, $s3, 2);
	set_sync($n3, $s3);
	wait_catchup($n3, $s3);
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kc) = keys_of_group($n1, 1, 2, 1);
	my $tx = KingresShardTest::start_psql($n1,
		"SET kingres_shard.fault_injection = 'after_commit:kill'; BEGIN; INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	KingresShardTest::finish_psql($tx);
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	is($n3->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'), '1', '[D] the participant holds the part prepared');
	$s3->stop;
	my ($o, $e, $timed_out) = ('', '', 0);
	my $rc = $n1->psql('postgres', 'SET kingres_shard.node_timeout = 2; SELECT kingres.recover_prepared_transactions()',
		stdout => \$o, stderr => \$e, timeout => 120, timed_out => \$timed_out);
	ok(!$timed_out, '[D] recover_prepared_transactions() returns although the participant\'s standby is away (bounded by node_timeout)');
	is($n3->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'), '0', '[D] and the part was committed on the participant');
	like($o, qr/^1$/m, '[D] and counted as resolved');
}

# --- C ------------------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('sg3', 3, replication => 1);
	my $sm = make_standby($n1, 'sg3_sm');
	my $id = register_standby($n1, $sm, 0);
	wait_catchup($n1, $sm);
	my ($rc, $out, $err) = run_sql($sm, "SELECT kingres.promote_node($id)");
	isnt($rc, 0, '[C] promote_node on the standby of the map group before pg_promote() is refused');
	diag($err);
	like($err, qr/recovery/, '[C] and the error says that the node is in recovery');
	($rc, $out, $err) = run_sql($n3, "SELECT kingres.promote_node($id)");
	like($err, qr/this node is not a copy of the primary of group 0 and cannot become it/,
		'[C] the call went to a node that is not the standby of the map group');
	$n1->stop('immediate');
	promote_standby($sm);
	$sm->safe_psql('postgres', "UPDATE kingres.node SET port = port + 1000 WHERE node_id = $id");
	($rc, $out, $err) = run_sql($sm, "\\set VERBOSITY verbose\nSELECT kingres.promote_node($id)");
	like($err, qr/KS010/, "[C] SQLSTATE KS010: the call went to the wrong node");
	like($err, qr/node $id is registered on port \d+, this server listens on port \d+/,
		'[C] a node registered on another port than the server listens on');
	$sm->safe_psql('postgres', "UPDATE kingres.node SET port = port - 1000 WHERE node_id = $id");
	$n2->safe_psql('postgres', 'UPDATE kingres.map_version SET term = 5');
	($rc, $out, $err) = run_sql($sm, "\\set VERBOSITY verbose\nSELECT kingres.promote_node($id)");
	like($err, qr/KS012/, "[C] SQLSTATE KS012: not a retry");
	like($err, qr/node 2 holds catalog term 5, newer than the 0 of this node/,
		'[C] a primary that holds a newer catalog term: this node must not promote');
}

done_testing();
