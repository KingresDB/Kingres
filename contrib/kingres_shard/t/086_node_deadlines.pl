# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/086_node_deadlines.pl
#
# Review S10-02: a control-plane call has no deadline against a node that accepts the
# session and does not answer.  The invariant: every call of the control plane returns within
# a bound that the README states, and when it gives up it says which node did not answer and
# what it was waiting for.  Two kinds of stuck node are made here, the reviewer's way and a
# second one:
#
#   - a lock: another session holds ACCESS EXCLUSIVE on kingres.map_version (or on
#     kingres.cluster_secret) of a participant for a long time.  A new session of the node waits for
#     it at its first query; the node's own lock_timeout, set for the node session, ends the wait
#     and the answer is "lock not available on node N" (55P03), or, when the deadline of the caller
#     comes first, KS007 "node N (host:port) did not answer within T seconds".  Either names the node;
#   - a backend that does not run: SIGSTOP of the node sessions that the call has cached, so that
#     only the deadline (kingres_shard.node_timeout) can end the wait: KS007, with the statement that
#     was not answered.
#
# For each call the time is asserted as "within the bound plus a margin", never "faster than".
# After the call, with the node free again, nothing is left prepared, check_cluster() is empty and the
# same call, repeated, succeeds.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run;
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_node make_cluster make_standby register_standby wait_catchup
  promote_standby run_sql prepared_count start_psql finish_psql);

my $T = 3;				# kingres_shard.node_timeout
my $MARGIN = 8;			# seconds of margin on a loaded machine
my $B1 = 2 * $T + $MARGIN;	# README, "Bounds": a call that does not change what a stuck node holds
my $B2 = 3 * $T + $MARGIN;	# check_cluster() and promote_node()
my $seq = 0;

sub setup
{
	my $prefix = 'dl' . (++$seq);
	my @n = make_cluster($prefix, 3, replication => 1);
	$_->append_conf('postgresql.conf', "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = 2\n") for @n;
	$_->reload for @n;
	return ($prefix, @n);
}

# Hold a lock on a table of $node for $secs seconds.  The monitoring session is opened first: a
# session that starts while the lock is held waits for it (every new session of a node reads the
# catalog), so nothing can be asked of the node through a new session.
sub hold_lock
{
	my ($node, $table, $secs) = @_;
	my $mon = $node->background_psql('postgres', on_error_stop => 0);
	$mon->query_safe('SELECT 1');
	my $h = start_psql($node, "BEGIN; LOCK TABLE $table IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep($secs);");
	for (my $i = 0; $i < 100; $i++)
	{
		last if $mon->query_safe("SELECT count(*) FROM pg_locks l WHERE l.mode = 'AccessExclusiveLock' AND l.granted AND l.relation = '$table'::regclass AND l.pid <> pg_backend_pid()") ne '0';
		sleep 0.1;
	}
	return { h => $h, mon => $mon };
}

sub release_lock
{
	my ($node, $l) = @_;
	$l->{mon}->query_safe("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE '%pg_sleep%' AND pid <> pg_backend_pid()");
	$l->{mon}->quit;
	eval { finish_psql($l->{h}) };
}

# Run $sql on $node in a session of its own, timed: (seconds, rc, stdout, stderr).
sub timed
{
	my ($node, $sql) = @_;
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($node, "\\set VERBOSITY verbose\n$sql");
	return (time - $t0, $rc, $out, $err);
}

# Warm a session of $node (the call is made once, so that the session has cached its connections to
# the other nodes), stop every client backend of $victim, and run $sql in the same session: the
# cached sessions accept the next statement and never answer.  Afterwards the backends are continued
# and ended.  Returns (seconds, stdout, stderr).
sub timed_stopped
{
	my ($node, $victim, $warm, $sql) = @_;
	my $bg = $node->background_psql('postgres', on_error_stop => 0);
	$bg->query_safe("\\set VERBOSITY verbose");
	$bg->query($warm);
	my $pids = $victim->safe_psql('postgres',
		"SELECT pid FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()");
	my @pids = grep { /^\d+$/ } split /\n/, $pids;
	kill 'STOP', $_ for @pids;
	my $t0 = time;
	$bg->{stderr} = '';
	my ($out) = $bg->query($sql);
	my $elapsed = time - $t0;
	my $err = $bg->{stderr};
	kill 'CONT', $_ for @pids;
	kill 'TERM', $_ for @pids;
	$bg->{stderr} = '';
	eval { $bg->quit };
	sleep 0.5;
	return ($elapsed, $out, $err);
}

sub named_node
{
	my ($err, $id) = @_;
	return $err =~ /(?:lock not available on node|node) $id \(/ ? 1 : 0;
}

sub no_prepared
{
	my (@nodes) = @_;
	my $n = 0;
	$n += prepared_count($_) for @nodes;
	return $n;
}

# --- 1. a lock on a participant ---------------------------------------------------------------
{
	my ($prefix, $map, $g1, $g2) = setup();
	my $x = make_node("${prefix}_x", replication => 1);
	my $l = hold_lock($g2, 'kingres.map_version', 150);
	my @calls = (
		[ 'sync_catalog', 'SELECT kingres.sync_catalog()' ],
		[ 'rotate_secret', 'SELECT kingres.rotate_secret()' ],
		[ 'remove_node', 'SELECT kingres.remove_node(2)' ],
		[ 'add_node', sprintf("SELECT kingres.add_node('%s', %d)", $x->host, $x->port) ],
	);
	foreach my $c (@calls)
	{
		my ($name, $sql) = @$c;
		my ($t, $rc, $out, $err) = timed($map, $sql);
		ok($t <= $B1, "[$prefix] $name with a lock on a participant returned in $t s (bound $B1)");
		ok($rc != 0 && $err =~ /\b(55P03|KS007)\b/, "[$prefix] $name: refused with 55P03 or KS007 ($err)");
		ok(named_node($err, 3), "[$prefix] $name: the answer names node 3");
	}
	my ($t, $rc, $out, $err) = timed($map, 'SELECT * FROM kingres.check_cluster()');
	ok($t <= $B2, "[$prefix] check_cluster returned in $t s (bound $B2)");
	is($rc, 0, "[$prefix] check_cluster does not fail as a whole");
	like($out, qr/^node_unreachable\|3\|/m, "[$prefix] check_cluster reports node 3 as not answering");
	($t, $rc, $out, $err) = timed($map, 'SELECT kingres.recover_prepared_transactions()');
	ok($t <= $B1, "[$prefix] recover_prepared_transactions returned in $t s (bound $B1)");
	is($rc, 0, "[$prefix] recover_prepared_transactions does not fail as a whole");
	like($err, qr/node 3 \(.*\) is not reachable, skipped/, "[$prefix] and its warning names node 3");
	release_lock($g2, $l);
	is(no_prepared($map, $g1, $g2), 0, "[$prefix] nothing is left prepared on any node");
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), '3', "[$prefix] the catalog did not change");
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', "[$prefix] check_cluster is empty");
	# the same calls, repeated, converge
	foreach my $c (@calls)
	{
		my ($name, $sql) = @$c;
		my ($rc2, $out2, $err2) = run_sql($map, $sql);
		is($rc2, 0, "[$prefix] $name succeeds when repeated ($err2)");
	}
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), '3', "[$prefix] one node removed, one added");
	is(no_prepared($map, $g1, $g2), 0, "[$prefix] still nothing prepared");
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', "[$prefix] check_cluster is empty after the repeats");
	$x->stop('immediate');
}

# --- 2. a lock on kingres.cluster_secret of a participant ------------------------------------
{
	my ($prefix, $map, $g1, $g2) = setup();
	my $l = hold_lock($g2, 'kingres.cluster_secret', 150);
	foreach my $c ([ 'sync_catalog', 'SELECT kingres.sync_catalog()' ], [ 'rotate_secret', 'SELECT kingres.rotate_secret()' ])
	{
		my ($name, $sql) = @$c;
		my ($t, $rc, $out, $err) = timed($map, $sql);
		ok($t <= $B1, "[$prefix] $name with a lock on cluster_secret returned in $t s (bound $B1)");
		ok($rc == 0 || (named_node($err, 3) && $err =~ /\b(55P03|KS007)\b/), "[$prefix] $name: done, or refused naming node 3 ($err)");
	}
	release_lock($g2, $l);
	is(no_prepared($map, $g1, $g2), 0, "[$prefix] nothing is left prepared");
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', "[$prefix] check_cluster is empty");
	my ($rc, $out, $err) = run_sql($map, 'SELECT kingres.rotate_secret()');
	is($rc, 0, "[$prefix] rotate_secret succeeds when repeated ($err)");
}

# --- 3. a node whose backends do not run -------------------------------------------------------
{
	my ($prefix, $map, $g1, $g2) = setup();
	my $x = make_node("${prefix}_x", replication => 1);
	my @calls = (
		[ 'sync_catalog', 'SELECT kingres.sync_catalog()', $B1 ],
		[ 'rotate_secret', 'SELECT kingres.rotate_secret()', $B1 ],
		[ 'remove_node', 'SELECT kingres.remove_node(2)', $B1 ],
		[ 'add_node', sprintf("SELECT kingres.add_node('%s', %d)", $x->host, $x->port), $B1 ],
	);
	foreach my $c (@calls)
	{
		my ($name, $sql, $bound) = @$c;
		my ($t, $out, $err) = timed_stopped($map, $g2, 'SELECT kingres.sync_catalog()', $sql);
		ok($t <= $bound, "[$prefix] $name with the sessions of node 3 stopped returned in $t s (bound $bound)");
		ok($err =~ /KS007/ && named_node($err, 3), "[$prefix] $name: KS007 naming node 3 ($err)");
		like($err, qr/did not answer within $T seconds/, "[$prefix] $name: it says what the deadline was");
	}
	my ($t, $out, $err) = timed_stopped($map, $g2, 'SELECT * FROM kingres.check_cluster()', 'SELECT * FROM kingres.check_cluster()');
	ok($t <= $B2, "[$prefix] check_cluster with the sessions of node 3 stopped returned in $t s (bound $B2)");
	like($out, qr/^node_unreachable\|3\|/m, "[$prefix] check_cluster reports node 3 ($err)");
	($t, $out, $err) = timed_stopped($map, $g2, 'SELECT kingres.sync_catalog()', 'SELECT kingres.recover_prepared_transactions()');
	ok($t <= $B1, "[$prefix] recover_prepared_transactions with the sessions of node 3 stopped returned in $t s (bound $B1)");
	like($err, qr/node 3 \(/, "[$prefix] its warning names node 3 ($err)");
	is(no_prepared($map, $g1, $g2), 0, "[$prefix] nothing is left prepared");
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), '3', "[$prefix] the catalog did not change");
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', "[$prefix] check_cluster is empty");
	foreach my $c (@calls)
	{
		my ($name, $sql) = @$c;
		my ($rc2, $out2, $err2) = run_sql($map, $sql);
		is($rc2, 0, "[$prefix] $name succeeds when repeated ($err2)");
	}
	$x->stop('immediate');
}

# --- 4. promote_node with a participant that does not answer ------------------------------------
foreach my $how ('lock', 'stopped')
{
	my ($prefix, $map, $g1, $g2) = setup();
	my $sb = make_standby($g1, "${prefix}_s");
	my $id = register_standby($map, $sb, 1);
	wait_catchup($g1, $sb);
	$g1->stop('immediate');
	promote_standby($sb);
	my ($t, $rc, $out, $err);
	if ($how eq 'lock')
	{
		my $l = hold_lock($g2, 'kingres.map_version', 150);
		($t, $rc, $out, $err) = timed($map, "SELECT kingres.promote_node($id)");
		release_lock($g2, $l);
	}
	else
	{
		($t, $out, $err) = timed_stopped($map, $g2, 'SELECT kingres.recover_prepared_transactions()', "SELECT kingres.promote_node($id)");
	}
	ok($t <= $B2, "[$prefix/$how] promote_node returned in $t s (bound $B2)");
	like($out, qr/"unreached": \[[0-9, ]*\b3\b/, "[$prefix/$how] promote_node lists node 3 as unreached ($out)");
	like($err, qr/WARNING:.*node 3 \(/s, "[$prefix/$how] and a warning names it ($err)");
	is(no_prepared($map, $sb, $g2), 0, "[$prefix/$how] nothing is left prepared");
	# repeated, with node 3 free again, the promotion is finished everywhere
	my ($rc2, $out2, $err2) = run_sql($map, "SELECT kingres.promote_node($id)");
	is($rc2, 0, "[$prefix/$how] promote_node repeated succeeds ($err2)");
	unlike($out2, qr/"unreached": \[[0-9, ]*\b3\b/, "[$prefix/$how] node 3 is reached now ($out2)");
	is($g2->safe_psql('postgres', "SELECT role FROM kingres.node WHERE node_id = $id"), 'primary', "[$prefix/$how] node 3 learned the promotion");
	is(no_prepared($map, $sb, $g2), 0, "[$prefix/$how] still nothing prepared");
}

# --- 5. a second caller of a catalog call is bounded as every wait (S10-07) -----------------------
{
	my ($prefix, $map, $g1, $g2) = setup();
	my $bg = $map->background_psql('postgres', on_error_stop => 0);
	$bg->query_safe("BEGIN; LOCK TABLE kingres.map_version IN EXCLUSIVE MODE");
	my ($t, $rc, $out, $err) = timed($map, 'SELECT kingres.sync_catalog()');
	ok($t <= $T + $MARGIN, "[$prefix] the second caller waited $t s (node_timeout $T)");
	ok($t >= $T - 0.5, "[$prefix] and not less than node_timeout ($t s)");
	like($err, qr/KS008/, "[$prefix] it is refused with KS008 ($err)");
	like($err, qr/another call that changes the catalog is running/, "[$prefix] and says why");
	$bg->query_safe("ROLLBACK");
	$bg->quit;
	my ($rc2, $out2, $err2) = run_sql($map, 'SELECT kingres.sync_catalog()');
	is($rc2, 0, "[$prefix] the call succeeds when repeated ($err2)");
	is(no_prepared($map, $g1, $g2), 0, "[$prefix] nothing is left prepared");
}

done_testing();
