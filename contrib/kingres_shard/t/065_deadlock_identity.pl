# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/065_deadlock_identity.pl
#
# Reviews S7-02, S7-03 and S7-10: the identity of a distributed transaction in the
# deadlock detector comes from the extension, not from the application name that a
# client can set.
#
#   - An ordinary role that gives its sessions the application name of a
#     distributed transaction ("ks-dtx:...") cannot make the detector cancel an
#     innocent transaction (S7-02: it used to build a cycle that did not exist).
#   - A malformed label never stops a pass of the detector, and a real deadlock is
#     found while the role floods the nodes with forged labels (S7-03).
#   - A remote session carries the identity that its node registered; only a node
#     session can register one, and only superusers can read it.
#   - A victim mark that did not lead to a cancel of its wait does not rewrite the
#     next cancel of that backend (S7-10).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql wait_waiter);

my ($n1, $n2) = make_cluster('di', 2,
	deadlock_interval => 1, deadlock_confirm => 300,
	extra => "deadlock_timeout = 200ms\n");
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
});
$_->safe_psql('postgres', 'CREATE ROLE mallory LOGIN') for ($n1, $n2);
$n1->safe_psql('postgres', 'GRANT ALL ON public.kv TO mallory');
my ($ka, $ka2) = keys_of_group($n1, 1, 0, 2);
my ($kb, $kb2) = keys_of_group($n1, 1, 1, 2);
$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'init')") for ($ka, $ka2, $kb, $kb2);

my $forged = "SET application_name = 'ks-dtx:1:99999:1000'";

sub session
{
	my ($node, $user) = @_;
	my $s = $node->background_psql('postgres', on_error_stop => 0,
		(defined $user ? (extra_params => [ '-U', $user ]) : ()));
	$s->query_safe("\\set VERBOSITY verbose");
	return $s;
}

sub send_stmt
{
	my ($s, $sql) = @_;
	$s->query_until(qr/sent/, "\\echo sent\n$sql;\n");
}

sub val { my ($n, $k) = @_; return $n->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $k"); }

# ---------------------------------------------------------------------------
# S7-02: a forged label does not build a cycle that does not exist
# ---------------------------------------------------------------------------
{
	my $m2 = session($n2, 'mallory');
	$m2->query_safe($forged);
	$m2->query_safe("BEGIN");
	$m2->query_safe("UPDATE public.kv SET val = 'm2' WHERE id = $kb");
	my $t = session($n1);
	$t->query_safe("BEGIN");
	$t->query_safe("UPDATE public.kv SET val = 't' WHERE id = $ka");
	send_stmt($t, "UPDATE public.kv SET val = 't' WHERE id = $kb");
	ok(wait_waiter($n2, 1), 'forged label: the innocent transaction waits for mallory on node 2');
	my $m1 = session($n1, 'mallory');
	$m1->query_safe($forged);
	$m1->query_safe("BEGIN");
	send_stmt($m1, "UPDATE public.kv SET val = 'm1' WHERE id = $ka");
	ok(wait_waiter($n1, 1), 'forged label: mallory waits for the innocent transaction on node 1');

	# The remote session of the innocent transaction carries the identity of its origin.
	like($n2->safe_psql('postgres',
			"SELECT kingres.internal_dtx_identity(pid) FROM pg_stat_activity WHERE application_name = 'kingres_shard' AND wait_event_type = 'Lock'"),
		qr/^ks-dtx:1:\d+:\d+$/, 'the remote session registered the identity of its origin transaction');
	is($n2->safe_psql('postgres',
			"SELECT count(*) FROM pg_stat_activity WHERE application_name = 'ks-dtx:1:99999:1000' AND kingres.internal_dtx_identity(pid) IS NOT NULL"),
		'0', 'a session with a forged application name has no identity');
	like($n2->safe_psql('postgres',
			"SELECT kingres.internal_dtx_label_of(pid, 2) FROM pg_stat_activity WHERE application_name = 'ks-dtx:1:99999:1000'"),
		qr/^ks-dtx:2:\d+:\d+$/, 'its label is its own, not the forged one');

	my @passes = map { $n1->safe_psql('postgres', 'SELECT kingres.detect_distributed_deadlocks()') } (1 .. 4);
	is(join(',', @passes), '0,0,0,0', 'forged label: the detector cancels nothing');
	like($n1->safe_psql('postgres', "SELECT count(*) FROM pg_locks WHERE NOT granted"), qr/^[1-9]/, 'forged label: the innocent transaction still waits');
	# Release mallory's first lock: the innocent transaction goes on, without an error.
	$m2->query_safe("ROLLBACK");
	$t->query_safe("SELECT 1");
	$t->query_safe("COMMIT");
	unlike($t->{stderr}, qr/ERROR/, 'forged label: the innocent transaction was not cancelled');
	$m1->query_safe("SELECT 1");
	$m1->query_safe("ROLLBACK");
	is(val($n1, $kb), 't', 'forged label: and its work is there');
	$_->quit for ($m1, $m2, $t);
}

# ---------------------------------------------------------------------------
# Who can set or read an identity
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, "SET ROLE mallory; SET kingres_shard.forward_dtx = '1:2:3'");
	isnt($rc, 0, 'an ordinary session cannot register an identity');
	($rc, $out, $err) = run_sql($n1, "SET ROLE mallory; SELECT kingres.internal_dtx_identity(pg_backend_pid())");
	like($err, qr/permission denied/, 'and cannot read one');
	is($n1->safe_psql('postgres', "SELECT kingres.internal_dtx_identity(pg_backend_pid()) IS NULL"), 't',
		'a session that is not a node session has none, superuser or not');
}

# ---------------------------------------------------------------------------
# S7-03: malformed labels never stop the detector; a real deadlock is found in a flood
# ---------------------------------------------------------------------------
{
	my $edge = sub {
		my ($node, $pid, $start, $waiter, $blocker) = @_;
		return qq({"node": $node, "waiter_pid": $pid, "waitstart": $start, "waiter": "$waiter", "blocker": "$blocker", "blocker_pid": 1});
	};
	my ($A, $B) = ('ks-dtx:1:100:1000', 'ks-dtx:2:200:2000');
	my @good = ($edge->(2, 21, 50, $A, $B), $edge->(1, 12, 60, $B, $A));
	my @bad = ($edge->(2, 31, 70, 'ks-dtx:1:1:abc', 'ks-dtx:1:2:5'), $edge->(1, 32, 71, 'ks-dtx:1:2:5', 'ks-dtx:1:1:abc'),
		$edge->(1, 33, 72, 'ks-dtx:1:3', 'x'), qq({"node": "x"}), '"text"', '7');
	my ($rc, $out, $err) = run_sql($n1,
		"SELECT victim FROM kingres.internal_deadlock_victims('[" . join(',', @good, @bad) . "]'::jsonb, '[" . join(',', @good, @bad) . "]'::jsonb)");
	is($rc, 0, 'made up: malformed labels and entries do not raise an error');
	is($out, $B, 'made up: and the real cycle in the same readings still has its victim');

	# The flood: mallory's sessions with forged names wait across the nodes while two innocent
	# transactions deadlock.
	my @mal;
	for my $k ([ $n1, $ka2 ], [ $n2, $kb2 ])
	{
		my ($node, $key) = @$k;
		my $m = session($node, 'mallory');
		$m->query_safe($forged);
		$m->query_safe("BEGIN");
		$m->query_safe("UPDATE public.kv SET val = 'f' WHERE id = $key");
		push @mal, $m;
		my $w = session($node, 'mallory');
		$w->query_safe("SET application_name = 'ks-dtx:1:1:abc'");
		$w->query_safe("BEGIN");
		send_stmt($w, "UPDATE public.kv SET val = 'f2' WHERE id = $key");
		push @mal, $w;
	}
	my $t1 = session($n1);
	my $t2 = session($n2);
	$t1->query_safe("BEGIN");
	$t1->query_safe("UPDATE public.kv SET val = 't1' WHERE id = $ka");
	$t2->query_safe("BEGIN");
	$t2->query_safe("UPDATE public.kv SET val = 't2' WHERE id = $kb");
	send_stmt($t1, "UPDATE public.kv SET val = 't1' WHERE id = $kb");
	ok(wait_waiter($n2, 2), 'flood: the first innocent transaction waits on node 2');
	send_stmt($t2, "UPDATE public.kv SET val = 't2' WHERE id = $ka");
	$t1->query_safe("SELECT 1");
	$t2->query("ROLLBACK");
	like($t2->{stderr}, qr/40P01: distributed deadlock detected/, 'flood: the real deadlock is resolved (the younger transaction is the victim)');
	unlike($t1->{stderr}, qr/ERROR/, 'flood: the other one goes on');
	$t1->query_safe("COMMIT");
	# Clean up the flood.
	$_->query("ROLLBACK") for @mal;
	$_->quit for (@mal, $t1, $t2);
	unlike(PostgreSQL::Test::Utils::slurp_file($n1->logfile), qr/invalid input syntax/, 'the detector never failed on a label');
}

# ---------------------------------------------------------------------------
# S7-10: a mark that missed its wait does not rewrite a later cancel
# ---------------------------------------------------------------------------
{
	my $s = session($n1);
	$s->query_safe("SET statement_timeout = '700ms'");
	my $pid = $s->query_safe("SELECT pg_backend_pid()");
	$pid =~ s/\s+//g;
	# A mark for a wait that this backend is not in (and never was).
	is($n1->safe_psql('postgres', "SELECT kingres.internal_mark_victim($pid, 12345)"), 't', 'a mark is made for a wait that does not exist');
	$s->query("SELECT pg_sleep(30)");
	like($s->{stderr}, qr/57014: canceling statement due to statement timeout/, 'S7-10: the statement timeout is reported as what it is');
	unlike($s->{stderr}, qr/distributed deadlock/, 'S7-10: not as a distributed deadlock');
	$s->quit;
}

done_testing();
