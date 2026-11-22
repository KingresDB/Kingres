# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/207_check_cluster_bound.pl: the time of kingres.check_cluster() with nodes that do not answer (design/shard-phase10.md, 21.10; review P10R-11).
#
# Three ways for a node not to answer, each with one and two such nodes:
#   - its sessions that the caller has cached are stopped (SIGSTOP): the next statement is accepted and never answered, so the wait is node_timeout (T);
#   - the node is down: the connection is refused at once;
#   - its postmaster is stopped (SIGSTOP): a new connection is accepted by the kernel and never answered, so the wait is node_connect_timeout (C), and the
#     base check connects twice to a node, so a node costs 2 x C (the reviewer's measurement, 10 s at C = 5, was 3.35 x T at T = 3).
# In every case a row node_unreachable names the node, the nodes that answer are still checked, and the time is within the bound that the README gives.
# The checks of phase 10 (layout, splits) and of table operations ask no node that the base check found not answering: they add nothing per node.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster run_sql);

my $T = 3;
my $C = 2;
my @nodes = make_cluster('cb', 4, replication => 1);
$_->append_conf('postgresql.conf', "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n") for @nodes;
$_->reload for @nodes;
my ($map, @others) = @nodes;
$map->safe_psql('postgres', q{CREATE TABLE public.t (id int PRIMARY KEY); INSERT INTO public.t SELECT generate_series(1, 100); SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 8)});

sub postmaster_pid { my ($n) = @_; my ($pid) = split /\n/, slurp_file($n->data_dir . '/postmaster.pid'); return $pid; }

# Warm a session of $node (its connections to the others are cached), stop every client backend of the victims, run $sql in the same session.
sub timed_stopped
{
	my ($node, $victims, $warm, $sql) = @_;
	my $bg = $node->background_psql('postgres', on_error_stop => 0);
	$bg->query($warm);
	my @pids;
	for my $v (@$victims)
	{
		my $pids = $v->safe_psql('postgres', "SELECT pid FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()");
		push @pids, grep { /^\d+$/ } split /\n/, $pids;
	}
	kill 'STOP', $_ for @pids;
	my $t0 = time;
	my ($out) = $bg->query($sql);
	my $elapsed = time - $t0;
	kill 'CONT', $_ for @pids;
	kill 'TERM', $_ for @pids;
	eval { $bg->quit };
	sleep 0.5;
	return ($elapsed, $out);
}

# A new session that asks while the postmasters of the victims are stopped.
sub timed_postmaster_stopped
{
	my ($node, $victims, $sql) = @_;
	my @pids = map { postmaster_pid($_) } @$victims;
	kill 'STOP', $_ for @pids;
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($node, $sql);
	my $elapsed = time - $t0;
	kill 'CONT', $_ for @pids;
	sleep 0.5;
	return ($elapsed, $out);
}

my $margin = 4;		# seconds, for a loaded machine (the sessions case)
my $tight = 1.5;	# seconds: the postmaster case is 2 x C, and a check that asked the node a third time (the table operations check before P10R-11) takes 3 x C
my $sql = 'SELECT * FROM kingres.check_cluster()';

# the cached sessions of nodes 3 and 4 do not answer: T for each
my ($t1, $out1) = timed_stopped($map, [ $others[1] ], $sql, $sql);
diag("sessions stopped, one node: $t1 s");
cmp_ok($t1, '<=', $T + $margin, "one node whose sessions do not answer: $t1 s (T + $margin)");
like($out1, qr/^node_unreachable\|3\|/m, 'it is reported');
my ($t2, $out2) = timed_stopped($map, [ $others[1], $others[2] ], $sql, $sql);
diag("sessions stopped, two nodes: $t2 s");
cmp_ok($t2, '<=', 2 * $T + $margin, "two nodes: $t2 s (2 x T + $margin)");
like($out2, qr/^node_unreachable\|3\|/m, 'the first is reported');
like($out2, qr/^node_unreachable\|4\|/m, 'and the second');

# the postmaster of node 3, then of nodes 3 and 4, is stopped: a new connection is not answered, C for each connect, two connects for a node
my ($p1, $pout1) = timed_postmaster_stopped($map, [ $others[1] ], $sql);
diag("postmaster stopped, one node: $p1 s");
cmp_ok($p1, '<=', 2 * $C + $tight, "one node whose postmaster is stopped: $p1 s (2 x C + $tight)");
like($pout1, qr/^node_unreachable\|3\|/m, 'it is reported');
my ($p2, $pout2) = timed_postmaster_stopped($map, [ $others[1], $others[2] ], $sql);
diag("postmaster stopped, two nodes: $p2 s");
cmp_ok($p2, '<=', 2 * (2 * $C) + 2 * $tight, "two nodes: $p2 s (2 x 2 x C + 2 x $tight)");
like($pout2, qr/^node_unreachable\|3\|/m, 'the first is reported');
like($pout2, qr/^node_unreachable\|4\|/m, 'and the second');
is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', 'check_cluster() is empty when every node answers again');

# a node that is down: refused at once
$others[1]->stop('immediate');
my $t0 = time;
my ($rc, $dout, $derr) = run_sql($map, $sql);
my $down = time - $t0;
diag("node down: $down s");
cmp_ok($down, '<=', $T + $margin, "a node that is down: $down s");
like($dout, qr/^node_unreachable\|3\|/m, 'it is reported');

done_testing();
