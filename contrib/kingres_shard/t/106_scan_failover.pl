# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/106_scan_failover.pl
#
# Step 2 of phase 2b, the high-availability part (design/shard-phase2b.md, 4.4,
# 4.6 and 10.3) for the distributed scan:
#
#   A. a transaction reads and writes a group through one node: after a
#      switchover a transaction that has read through the old primary and then
#      meets the new one fails with KS001 instead of combining two states of the
#      group (the rule of 4.4);
#   B. a node that dies while a scan reads it: KS001 with a detail that names it
#      when the group has a standby, the plain connection error when it has none
#      (fragment_error, 4.6); nothing is returned in either case;
#   C. the scan across promote_node when the transaction is new: the statement
#      goes to the new primary and answers completely.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql make_standby
  register_standby wait_catchup set_sync promote_standby wait_for_fault_file
  start_psql finish_psql);

# A cluster of two groups; group 1 has a standby when $standby.
sub setup
{
	my ($prefix, $standby) = @_;
	my ($n1, $n2) = make_cluster($prefix, 2, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 8);
	});
	my @k0 = keys_of_group($n1, 1, 0, 3);
	my @k1 = keys_of_group($n1, 1, 1, 4);
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'x')") for (@k0, @k1);
	my ($s2, $id);
	if ($standby)
	{
		$s2 = make_standby($n2, "${prefix}_s2");
		$id = register_standby($n1, $s2, 1);
		set_sync($n2, $s2);
		wait_catchup($n2, $s2);
	}
	return ($n1, $n2, $s2, $id, scalar(@k0) + scalar(@k1));
}

# ---------------------------------------------------------------------------
# A. one node of a group per transaction
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $s2, $id, $total) = setup('ga', 1);
	my $sess = $n1->background_psql('postgres', on_error_stop => 0);
	$sess->query_safe("\\set VERBOSITY verbose");
	$sess->query_safe("BEGIN");
	is($sess->query_safe("SELECT count(*) FROM public.kv"), $total, 'the transaction reads through the primary of group 1');
	is($sess->query_safe("SELECT count(*) FROM public.kv"), $total, 'and again, through the same node');

	# The switchover, from another session.
	$n2->safe_psql('postgres', 'CHECKPOINT');
	promote_standby($s2);
	set_sync($n2);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");

	my $out = $sess->query("SELECT count(*) FROM public.kv");
	my $err = $sess->{stderr};
	like($err, qr/KS001/, 'the next statement of the transaction fails with the retryable error');
	like($err, qr/already uses node .* of shard group 1/, 'and says that the transaction uses another node of the group');
	unlike($out, qr/^\d+$/m, 'and returns no count');
	$sess->{stderr} = '';
	$sess->query("ROLLBACK");
	is($sess->query_safe("SELECT count(*) FROM public.kv"), $total, 'in a new transaction the scan answers, from the new primary');
	$sess->quit;

	# C. the scan that starts after the change goes to the new primary
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN (" .
			join(',', keys_of_group($n1, 1, 1, 2)) . ")"), 2, 'a pruned scan of the group goes to the new primary');
	is($n2->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), $total, 'and so does a scan from the old primary');
}

# ---------------------------------------------------------------------------
# B. a node that dies while a scan reads it
# ---------------------------------------------------------------------------
for my $standby (1, 0)
{
	my ($n1, $n2, $s2, $id, $total) = setup("gb$standby", $standby);
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_gb$standby";
	append_to_file($gate, 'x');
	# The cursors are declared and the scan waits; the node is killed; the scan goes on.
	my $scan = start_psql($n1,
		"SET kingres_shard.scan_fetch_size = 2; SET kingres_shard.fault_injection = 'scan_after_declare:wait:$gate'; SELECT count(*) FROM public.kv;");
	ok(wait_for_fault_file("$gate.reached", 30), 'the scan waits after its DECLAREs (' . ($standby ? 'a standby is registered' : 'none') . ')');
	$n2->stop('immediate');
	unlink $gate;
	my ($out, $err) = finish_psql($scan);
	unlike($out, qr/^\d+$/m, 'no count is returned');
	if ($standby)
	{
		like($err, qr/failed while a distributed statement was running/, q{a group with a standby: the error is the retryable one (KS001)});
		like($err, qr/failover of its shard group may be in progress/, 'with a detail that says what may be going on');
		like($err, qr/node [^ ]+:\d+ failed while a distributed statement was running/, 'and names the node');
	}
	else
	{
		unlike($err, qr/failed while a distributed statement was running/, q{a group without a standby: the plain error, which a repeat would only meet again});
		like($err, qr/connect|connection|lost|terminat|FATAL|server closed/i, 'a connection error');
	}
}

done_testing();
