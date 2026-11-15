# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/068_participant_failover.pl
#
# Review S7-07, the part that no setting can prevent: a participant whose standby
# is asynchronous (or was away) can fail over and lose its part of a transaction
# that its origin committed.  The extension has to notice it afterwards: every
# participant writes the identifier of the distributed transaction into
# kingres.dtx_applied in the transaction it prepares, the origin keeps its decisions
# for kingres_shard.decision_retention seconds when the participant's group has a
# standby, and when the group's primary has changed the origin compares them with
# the new primary's marks in its next recovery pass.  A transaction that the new
# primary has no trace of is recorded in kingres.dtx_incident (participant_lost)
# and reported by check_cluster().

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group make_standby register_standby
  wait_catchup promote_standby);

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

my $seq = 0;

# A cluster whose node 3 (group 2) has an asynchronous standby; one distributed
# transaction from node 1 commits; node 3 fails over to its standby.
sub scenario
{
	my ($caught_up) = @_;
	my $prefix = 'pf' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s3 = make_standby($n3, "${prefix}_s3");
	my $id = register_standby($n1, $s3, 2);
	wait_catchup($n3, $s3);
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kc) = keys_of_group($n1, 1, 2, 1);
	$s3->stop unless $caught_up;
	my $marks0 = $n3->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_applied');
	my $log0 = $n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log');
	$n1->safe_psql('postgres', "BEGIN; INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	is(prepared($n3), '0', "[$prefix] the transaction committed on the participant");
	is($n3->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_applied') - $marks0, 1, "[$prefix] which holds its mark");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log') - $log0, 1, "[$prefix] and the origin kept the decision");
	if ($caught_up)
	{
		wait_catchup($n3, $s3);
	}
	$n3->stop('immediate');
	$s3->start unless $caught_up;
	promote_standby($s3);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	return ($prefix, $n1, $n3, $s3, $ka, $kc);
}

# --- the participant lost its part ---------------------------------------------------
{
	my ($prefix, $n1, $n3, $s3, $ka, $kc) = scenario(0);
	is($s3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $kc"), '0', "[$prefix] the new primary of the participant group has no trace of the transaction");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $ka"), '1', "[$prefix] the origin's part is committed");
	# promote_node resolves what the old primary left (a pass of recovery that looks at the promoted node too): it found the loss.
	is($n1->safe_psql('postgres', "SELECT kind || ':' || node_id FROM kingres.dtx_incident"), "participant_lost:4",
		"[$prefix] the next recovery pass of the origin records the loss, against the new primary");
	like($n1->safe_psql('postgres', "SELECT detail FROM kingres.check_cluster() WHERE check_name = 'participant_lost_committed_transaction'"),
		qr/no trace of its part/, "[$prefix] and check_cluster reports it");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log WHERE node_id = 3'), '0', "[$prefix] the decisions about the old primary are forgotten after the look");
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_incident'), '1', "[$prefix] a second pass does not record it twice");
}

# --- the participant's standby had everything -----------------------------------------
{
	my ($prefix, $n1, $n3, $s3, $ka, $kc) = scenario(1);
	is($s3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $kc"), '1', "[$prefix] the new primary has the part");
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_incident'), '0', "[$prefix] no incident: the mark came with the part");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'participant_lost_committed_transaction'"), '0',
		"[$prefix] and check_cluster says nothing");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log WHERE node_id = 3'), '0', "[$prefix] the decisions about the old primary were forgotten");
}

# --- nothing is kept when the group has no standby --------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('pg', 3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kc) = keys_of_group($n1, 1, 2, 1);
	$n1->safe_psql('postgres', "BEGIN; INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '0', 'without a standby the decision is forgotten at the first pass');
	# An ordinary role cannot write a mark.
	$n3->safe_psql('postgres', 'CREATE ROLE mallory LOGIN');
	my ($rc, $out, $err) = (0, '', '');
	$n3->psql('postgres', "SET ROLE mallory; SELECT kingres.internal_dtx_applied('x')", stdout => \$out, stderr => \$err);
	like($err, qr/permission denied/, 'an ordinary role cannot write a mark');
}

done_testing();
