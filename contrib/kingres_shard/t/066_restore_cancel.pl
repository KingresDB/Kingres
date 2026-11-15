# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/066_restore_cancel.pl
#
# Review S7-08: the barrier of create_restore_point() is lowered on every way out of
# the function.  A transaction is held between its local commit and its second phase,
# so that the restore point waits behind it with the barrier up on every primary; then
# the caller is cancelled (statement_timeout, pg_cancel_backend), terminated, or fails,
# and the barrier must be down on every node long before its time to live (2 x
# node_timeout + 10 seconds: 18 seconds here).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql wait_for_fault_file
  start_psql finish_psql);

my ($n1, $n2) = make_cluster('rl', 2, extra => "kingres_shard.node_timeout = 4\n");
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
});
my @ka = keys_of_group($n1, 1, 0, 4);
my @kb = keys_of_group($n1, 1, 1, 4);
my $tmp = $PostgreSQL::Test::Utils::tmp_check;

sub active
{
	my ($node) = @_;
	return $node->safe_psql('postgres', "SELECT kingres.internal_restore_barrier('state')->>'active'");
}

# A transaction that holds the restore point back: committed on the origin, second phase not sent.
sub hold
{
	my ($key_a, $key_b, $name) = @_;
	my $gate = "$tmp/gate_$name";
	append_to_file($gate, 'x');
	my $t = start_psql($n1,
		"SET kingres_shard.fault_injection = 'after_commit:wait:$gate'; BEGIN; "
		  . "INSERT INTO public.kv VALUES ($key_a, 'h'); INSERT INTO public.kv VALUES ($key_b, 'h'); COMMIT;");
	ok(wait_for_fault_file("$gate.reached", 60), "[$name] a transaction is between its local commit and its second phase");
	return ($t, $gate);
}

sub both_down
{
	my ($what) = @_;
	# The time to live is 18 seconds: a barrier that is down within 6 seconds did not wait for it.
	for my $n ($n1, $n2)
	{
		my $down = 0;
		for (my $i = 0; $i < 60 && !$down; $i++)
		{
			$down = (active($n) eq 'false');
			select(undef, undef, undef, 0.1) unless $down;
		}
		ok($down, "[$what] the barrier is down on node " . ($n == $n1 ? 1 : 2) . " within 6 seconds");
	}
}

# --- 1. statement_timeout -------------------------------------------------------
{
	my ($t, $gate) = hold($ka[0], $kb[0], 'st');
	my ($rc, $out, $err) = run_sql($n1, "SET statement_timeout = '2s'; SELECT * FROM kingres.create_restore_point('rp_timeout')");
	isnt($rc, 0, '[statement_timeout] the restore point is cancelled');
	like($err, qr/statement timeout/, '[statement_timeout] by the timeout');
	both_down('statement_timeout');
	unlink $gate;
	finish_psql($t);
}

# --- 2. pg_cancel_backend -------------------------------------------------------
{
	my ($t, $gate) = hold($ka[1], $kb[1], 'pc');
	my $rp = start_psql($n1, "SELECT * FROM kingres.create_restore_point('rp_cancel')");
	ok($n1->poll_query_until('postgres', "SELECT (kingres.internal_restore_barrier('state')->>'active')::bool"),
		'[pg_cancel_backend] the barrier is up while the restore point waits');
	$n1->safe_psql('postgres',
		"SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE query LIKE '%create_restore_point(''rp_cancel'')%' AND pid <> pg_backend_pid()");
	my ($o, $e) = finish_psql($rp);
	like($e, qr/canceling statement due to user request/, '[pg_cancel_backend] the caller was cancelled');
	both_down('pg_cancel_backend');
	unlink $gate;
	finish_psql($t);
}

# --- 3. pg_terminate_backend ----------------------------------------------------
{
	my ($t, $gate) = hold($ka[2], $kb[2], 'pt');
	my $rp = start_psql($n1, "SELECT * FROM kingres.create_restore_point('rp_term')");
	ok($n1->poll_query_until('postgres', "SELECT (kingres.internal_restore_barrier('state')->>'active')::bool"),
		'[pg_terminate_backend] the barrier is up while the restore point waits');
	$n1->safe_psql('postgres',
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE '%create_restore_point(''rp_term'')%' AND pid <> pg_backend_pid()");
	finish_psql($rp);
	both_down('pg_terminate_backend');
	unlink $gate;
	finish_psql($t);
}

# --- 4. a commit is not held up afterwards --------------------------------------
{
	my $t0 = time;
	$n2->safe_psql('postgres', "BEGIN; INSERT INTO public.kv VALUES (" . ($kb[3]) . ", 'after'); INSERT INTO public.kv VALUES (" . ($ka[3]) . ", 'after'); COMMIT");
	cmp_ok(time - $t0, '<', 8, 'a distributed commit right after the cancels is not held up behind a barrier');
}

done_testing();
