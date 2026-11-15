# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/050_failover_twophase.pl
#
# Prepared transactions across a failover (design/ha-phase5.md, part C, item 3).
# The origin of a two-phase transaction is the primary of group 1 (node 2); it is
# stopped at each point of the commit protocol, its standby is promoted,
# kingres.promote_node() is called, and the recovery of the new primary resolves
# what the old one left prepared on the other nodes:
#
#   with a synchronous standby the outcome is always atomic: a transaction that
#   the old primary had committed (a decision was acknowledged by the standby
#   before any participant was told to commit) is committed everywhere, any
#   other is rolled back everywhere;
#
#   with an asynchronous standby that did not have the decision, a transaction
#   that the old primary committed and partly told its participants about is
#   rolled back on the participants that were still prepared: the one case that
#   ends with a half-committed transaction, which kingres.dtx_incident records.

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

my $seq = 0;

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

# One run: the origin (node 2) stops at $point; returns the nodes and the keys.
sub scenario
{
	my ($point, %opt) = @_;
	my $prefix = 'fo' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, "${prefix}_s2");
	my $id = register_standby($n1, $s2, 1);
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kb) = keys_of_group($n1, 1, 1, 1);
	my ($kc) = keys_of_group($n1, 1, 2, 1);
	if ($opt{sync})
	{
		set_sync($n2, $s2);
	}
	wait_catchup($n2, $s2);
	# A transaction that is already committed everywhere: the baseline.
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES (" . ($ka + 500000) . ", 'base')");
	if ($opt{lag})
	{
		# The standby stops receiving: whatever the origin does from here is not on it.
		$s2->stop;
	}

	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_${prefix}";
	append_to_file($gate, 'x');
	my $tx = start_psql($n2,
		"SET kingres_shard.fault_injection = '$point:wait:$gate'; BEGIN; "
		  . "INSERT INTO public.kv VALUES ($kb, 'b'); INSERT INTO public.kv VALUES ($ka, 'a'); "
		  . "INSERT INTO public.kv VALUES ($kc, 'c'); COMMIT;");
	ok(wait_for_fault_file("$gate.reached", 60), "[$point] the origin is at the point");
	$n2->stop('immediate');
	unlink $gate;
	my ($o, $e) = finish_psql($tx);
	if ($opt{lag})
	{
		$s2->start;
	}
	promote_standby($s2);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	return ($n1, $n3, $s2, $ka, $kb, $kc, $id);
}

sub resolve
{
	my ($s2) = @_;
	# The promoted node resolves what its group originated: more than once is harmless.
	my $r = $s2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	$s2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	return $r;
}

# ---------------------------------------------------------------------------
# A synchronous standby: always atomic.
# ---------------------------------------------------------------------------
for my $case (
	[ 'before_prepare',          0, 0 ],
	[ 'after_first_prepare',     0, 1 ],
	[ 'after_prepare',           0, 2 ],
	[ 'after_log',               0, 2 ],
	[ 'after_commit',            1, 2 ],
	[ 'between_commit_prepared', 1, 2 ])
{
	my ($point, $committed, $prepared_before) = @$case;
	my ($n1, $n3, $s2, $ka, $kb, $kc) = scenario($point, sync => 1);
	is(prepared($n1) + prepared($n3), $prepared_before > 1 && $point eq 'between_commit_prepared' ? 1 : $prepared_before,
		"[$point] participants hold the prepared transactions the point implies")
	  if $point ne 'between_commit_prepared' && $point ne 'after_commit';
	resolve($s2);
	is(prepared($n1) . prepared($n3) . prepared($s2), '000', "[$point] nothing is left prepared");
	my $got = $n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($ka, $kb, $kc)");
	is($got, $committed ? '3' : '0', "[$point] sync standby: the transaction is " . ($committed ? 'committed everywhere' : 'rolled back everywhere'));
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($ka, $kb, $kc)"), $committed ? '3' : '0',
		"[$point] and a scan from another node agrees");
	my $incidents = $s2->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_incident');
	is($incidents, $committed ? '0' : (($point eq 'before_prepare') ? '0' : ($point eq 'after_first_prepare' ? '1' : '2')),
		"[$point] the rollbacks of earlier-timeline transactions are recorded");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'prepared_rolled_back_after_failover'"),
		$committed ? '0' : (($point eq 'before_prepare') ? '0' : ($point eq 'after_first_prepare' ? '1' : '2')),
		"[$point] check_cluster shows them");
	# The new primary originates transactions of the new timeline that nobody confuses with the old.
	$n1->safe_psql('postgres', "BEGIN; INSERT INTO public.kv VALUES (" . ($kb + 700000) . ", 'n'); INSERT INTO public.kv VALUES (" . ($ka + 700000) . ", 'n'); COMMIT");
	is(prepared($n1) . prepared($n3) . prepared($s2), '000', "[$point] a new two-node transaction commits on the new primary");
}

# ---------------------------------------------------------------------------
# An asynchronous standby that does not have the decision.
# ---------------------------------------------------------------------------
{
	my ($n1, $n3, $s2, $ka, $kb, $kc) = scenario('between_commit_prepared', lag => 1);
	resolve($s2);
	is(prepared($n1) . prepared($n3), '00', 'async: nothing is left prepared');
	my $a = $n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $ka");
	my $c = $n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $kc");
	my $b = $n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $kb");
	is($b, '0', 'async: the local part of the lost primary is not on the standby');
	is($a + $c, 1, 'async: the participant that had been told to commit kept the row, the other was rolled back: half committed');
	is($s2->safe_psql('postgres', "SELECT count(*) FROM kingres.dtx_incident"), '1',
		'async: and the rolled-back transaction is recorded, so that an operator finds out');
	like($n1->safe_psql('postgres', "SELECT detail FROM kingres.check_cluster() WHERE check_name = 'prepared_rolled_back_after_failover'"),
		qr/timeline 1 and had no commit decision on timeline 2/, 'async: check_cluster says what happened');
}

done_testing();
