# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/052_distributed_deadlock.pl
#
# Distributed deadlock detection (design/ha-phase5.md, part C, item 4): the
# recovery worker of the map group primary collects the wait-for edges of every
# primary, confirms a cycle by reading them twice, and cancels the youngest
# transaction of the cycle with 40P01 "distributed deadlock detected".
#   - a cycle across two groups and across three groups;
#   - a chain that is no cycle, and a local deadlock, are left alone;
#   - the decision function with made-up edges: a cycle whose edges are not the
#     same waits in two readings, a cycle on one node, disjoint cycles.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql wait_waiter);

my ($n1, $n2, $n3) = make_cluster('dl', 3,
	deadlock_interval => 1, deadlock_confirm => 300,
	extra => "deadlock_timeout = 200ms\n");
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
});
my ($ka, $ka2) = keys_of_group($n1, 1, 0, 2);
my ($kb) = keys_of_group($n1, 1, 1, 1);
my ($kc) = keys_of_group($n1, 1, 2, 1);
$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'init')") for ($ka, $ka2, $kb, $kc);

sub session
{
	my ($node) = @_;
	my $s = $node->background_psql('postgres', on_error_stop => 0);
	$s->query_safe("\\set VERBOSITY verbose");
	return $s;
}

# Send a statement that is going to wait; returns when psql has taken it.
sub send_stmt
{
	my ($s, $sql) = @_;
	$s->query_until(qr/sent/, "\\echo sent\n$sql;\n");
}

sub val { my ($n, $k) = @_; return $n->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $k"); }

# ---------------------------------------------------------------------------
# Two groups
# ---------------------------------------------------------------------------
{
	my $t1 = session($n1);
	my $t2 = session($n2);
	$t1->query_safe("BEGIN");
	$t1->query_safe("UPDATE public.kv SET val = 't1' WHERE id = $ka");
	$t2->query_safe("BEGIN");
	$t2->query_safe("UPDATE public.kv SET val = 't2' WHERE id = $kb");
	send_stmt($t1, "UPDATE public.kv SET val = 't1' WHERE id = $kb");
	ok(wait_waiter($n2, 1), 'two groups: T1 (from node 1) waits for T2 on node 2');
	send_stmt($t2, "UPDATE public.kv SET val = 't2' WHERE id = $ka");
	# The detector (a background worker, every second) finds the cycle and cancels the younger one, T2.
	$t1->query_safe("SELECT 1");		# returns when T1's statement has finished
	$t2->query("ROLLBACK");
	like($t2->{stderr}, qr/40P01: distributed deadlock detected/, 'two groups: the younger transaction got 40P01, distributed deadlock detected');
	unlike($t1->{stderr}, qr/ERROR/, 'two groups: the older transaction got no error');
	$t1->query_safe("COMMIT");
	is(join(',', val($n1, $ka), val($n1, $kb)), 't1,t1', 'two groups: the older transaction committed, the younger left nothing');
	$t1->quit;
	$t2->quit;
}

# ---------------------------------------------------------------------------
# Three groups
# ---------------------------------------------------------------------------
{
	my $t1 = session($n1);
	my $t2 = session($n2);
	my $t3 = session($n3);
	$t1->query_safe("BEGIN");
	$t1->query_safe("UPDATE public.kv SET val = 'u1' WHERE id = $ka");
	$t2->query_safe("BEGIN");
	$t2->query_safe("UPDATE public.kv SET val = 'u2' WHERE id = $kb");
	$t3->query_safe("BEGIN");
	$t3->query_safe("UPDATE public.kv SET val = 'u3' WHERE id = $kc");
	send_stmt($t1, "UPDATE public.kv SET val = 'u1' WHERE id = $kb");
	ok(wait_waiter($n2, 1), 'three groups: T1 waits on node 2');
	send_stmt($t2, "UPDATE public.kv SET val = 'u2' WHERE id = $kc");
	ok(wait_waiter($n3, 1), 'three groups: T2 waits on node 3');
	send_stmt($t3, "UPDATE public.kv SET val = 'u3' WHERE id = $ka");
	$t3->query("ROLLBACK");
	like($t3->{stderr}, qr/40P01: distributed deadlock detected/, 'three groups: the youngest transaction of the cycle (T3) is the victim');
	# T2 is released by T3's rollback, then T1 by T2's commit.
	$t2->query_safe("SELECT 1");
	$t2->query_safe("COMMIT");
	$t1->query_safe("SELECT 1");
	$t1->query_safe("COMMIT");
	unlike($t1->{stderr} . $t2->{stderr}, qr/ERROR/, 'three groups: the other two transactions ended without error');
	is(join(',', val($n1, $ka), val($n1, $kb), val($n1, $kc)), 'u1,u1,u2', 'three groups: their work is there, the victim\'s is not');
	$_->quit for ($t1, $t2, $t3);
}

# ---------------------------------------------------------------------------
# No cycle: a chain, and a local deadlock
# ---------------------------------------------------------------------------
{
	my $t1 = session($n1);
	my $t2 = session($n2);
	$t1->query_safe("BEGIN");
	$t1->query_safe("UPDATE public.kv SET val = 'c1' WHERE id = $ka");
	$t2->query_safe("BEGIN");
	send_stmt($t2, "UPDATE public.kv SET val = 'c2' WHERE id = $ka");
	ok(wait_waiter($n1, 1), 'chain: T2 waits for T1');
	# Let the detector read the edges several times (a manual pass is the same function).
	my @cancelled = map { $n1->safe_psql('postgres', 'SELECT kingres.detect_distributed_deadlocks()') } (1 .. 4);
	is(join(',', @cancelled), '0,0,0,0', 'chain: the detector cancels nothing');
	like($n1->safe_psql('postgres', "SELECT count(*) FROM pg_locks WHERE NOT granted"), qr/^[1-9]/, 'chain: T2 is still waiting');
	$t1->query_safe("COMMIT");
	$t2->query_safe("SELECT 1");
	$t2->query_safe("COMMIT");
	unlike($t2->{stderr}, qr/ERROR/, 'chain: T2 went on when T1 committed');
	is(val($n1, $ka), 'c2', 'chain: and wrote its value');
	$t1->quit;
	$t2->quit;
}
{
	# A local deadlock is PostgreSQL's: both transactions on node 1, two keys of group 0.
	my $t1 = session($n1);
	my $t2 = session($n1);
	$t1->query_safe("BEGIN");
	$t1->query_safe("UPDATE public.kv SET val = 'l1' WHERE id = $ka");
	$t2->query_safe("BEGIN");
	$t2->query_safe("UPDATE public.kv SET val = 'l2' WHERE id = $ka2");
	send_stmt($t1, "UPDATE public.kv SET val = 'l1' WHERE id = $ka2");
	ok(wait_waiter($n1, 1), 'local: T1 waits for T2');
	send_stmt($t2, "UPDATE public.kv SET val = 'l2' WHERE id = $ka");
	$t2->query("SELECT 1");
	$t1->query("SELECT 1");
	my $err = $t1->{stderr} . $t2->{stderr};
	like($err, qr/40P01: deadlock detected/, 'local: PostgreSQL resolved a local deadlock itself');
	unlike($err, qr/distributed/, 'local: the detector did not take part');
	$t1->query("ROLLBACK");
	$t2->query("ROLLBACK");
	$t1->quit;
	$t2->quit;
}

# ---------------------------------------------------------------------------
# The decision with made-up edges
# ---------------------------------------------------------------------------
{
	my ($A, $B, $C, $D, $E) = ('ks-dtx:1:100:1000', 'ks-dtx:2:200:2000', 'ks-dtx:3:300:3000', 'ks-dtx:1:400:4000', 'ks-dtx:2:500:5000');
	my $edge = sub {
		my ($node, $pid, $start, $waiter, $blocker) = @_;
		return qq({"node": $node, "waiter_pid": $pid, "waitstart": $start, "waiter": "$waiter", "blocker": "$blocker", "blocker_pid": 1});
	};
	my $victims = sub {
		my ($first, $second) = @_;
		return $n1->safe_psql('postgres',
			"SELECT coalesce(string_agg(victim || '\@' || victim_node || '/' || victim_pid || '/' || victim_waitstart, ' ' ORDER BY victim), '')"
			  . " FROM kingres.internal_deadlock_victims('[" . join(',', @$first) . "]'::jsonb, '[" . join(',', @$second) . "]'::jsonb)");
	};
	my @cyc2 = ($edge->(2, 21, 50, $A, $B), $edge->(1, 12, 60, $B, $A));
	is($victims->(\@cyc2, \@cyc2), "$B\@1/12/60", 'made up: a cycle seen twice has the youngest transaction as its victim, with its wait');
	is($victims->(\@cyc2, [ $edge->(2, 21, 50, $A, $B), $edge->(1, 12, 61, $B, $A) ]), '',
		'made up: a wait that ended and began again between the readings is not the same wait: no victim');
	is($victims->(\@cyc2, [ $edge->(2, 21, 50, $A, $B) ]), '', 'made up: an edge missing from the second reading: no victim');
	is($victims->(\@cyc2, []), '', 'made up: the second reading finds nothing: no victim');
	is($victims->([ $edge->(1, 21, 50, $A, $B), $edge->(1, 12, 60, $B, $A) ], [ $edge->(1, 21, 50, $A, $B), $edge->(1, 12, 60, $B, $A) ]), '',
		'made up: a cycle whose edges all come from one node is PostgreSQL\'s');
	my @cyc3 = ($edge->(2, 21, 50, $A, $B), $edge->(3, 31, 60, $B, $C), $edge->(1, 11, 70, $C, $A));
	is($victims->(\@cyc3, \@cyc3), "$C\@1/11/70", 'made up: a cycle of three');
	my @two = (@cyc2, $edge->(2, 41, 80, $D, $E), $edge->(3, 51, 90, $E, $D));
	is($victims->(\@two, \@two), "$B\@1/12/60 $E\@3/51/90", 'made up: two disjoint cycles have a victim each');
	is($victims->([ $edge->(2, 21, 50, $A, $B), $edge->(1, 12, 60, $B, $C) ], [ $edge->(2, 21, 50, $A, $B), $edge->(1, 12, 60, $B, $C) ]), '',
		'made up: a chain is no cycle');
	# The edges that this node reports for a real wait carry the label and the start of the wait.
	my $t1 = session($n1);
	my $t2 = session($n1);
	$t1->query_safe("BEGIN");
	$t1->query_safe("UPDATE public.kv SET val = 'e1' WHERE id = $ka");
	send_stmt($t2, "UPDATE public.kv SET val = 'e2' WHERE id = $ka");
	ok(wait_waiter($n1, 1), 'edges: a wait exists');
	my $e = $n1->safe_psql('postgres', "SELECT e->>'waiter' || ' -> ' || (e->>'blocker') FROM jsonb_array_elements(kingres.internal_wait_edges(0)) e");
	like($e, qr/^ks-dtx:1:\d+:\d+ -> ks-dtx:1:\d+:\d+$/, 'edges: waiter and blocker are labelled with the node, backend and start of their transactions');
	$t1->query_safe("COMMIT");
	$t2->query_safe("SELECT 1");
	$t1->quit;
	$t2->quit;
}

done_testing();
