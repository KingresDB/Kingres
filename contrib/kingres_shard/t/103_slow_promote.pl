# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/103_slow_promote.pl
#
# Review S12-03 and S12-02, for promote_node(): the cells of t/102 (one, two and four nodes that
# answer every statement after 0.5 and after 0.9 x node_timeout) with the node that is promoted and
# the primaries of three other groups behind a forwarder each.  The slow nodes are, in this order:
# the node to promote, then the other groups' primaries.  A fresh cluster is built for every cell,
# because a promotion is done once.  The call must answer "promoted" with nothing in "unreached", no
# WARNING and no KS007, every catalog must show the promoted node as the primary of its group, the
# repeat must answer "already", and the time must stay within the formula of the README: the sum over
# the slow nodes of (statements sent to the node) x node_timeout, plus the work on the other nodes.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use File::Path ();
use Time::HiRes qw(time sleep);
use KsProxy;
use KingresShardTest qw(make_node make_standby run_sql release_node);

my $long = $ENV{KINGRES_LONG_TESTS} ? 1 : 0;
# node_timeout: 2 s in the long run; 3 s in the default run, whose cells answer at 0.9 x node_timeout and so
# leave 0.3 s to a machine that is busy (at 2 s the 0.2 s that were left were lost when four tests ran at once)
my $T = $long ? 2 : 3;						# node_timeout
my $C = 2;
my $TOLERANCE = 4;
my $conf = "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n";
my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my $seq = 0;

# MAP (node 1), group 1: a primary (node 2) with a standby (node 6), groups 2, 3 and 4: one primary
# each (nodes 3, 4, 5).  Every node but MAP and the failed primary is registered by the address of a
# forwarder.  The primary of group 1 is stopped and its standby promoted in PostgreSQL.
# The ports of the nodes are taken from the harness once and used again for every cluster (it keeps a port
# until the test ends).
my @port = map { PostgreSQL::Test::Cluster::get_free_port() } (1 .. 6);

sub build
{
	my $id = ++$seq;
	my %f;
	$f{m} = make_node("qm$id", port => $port[0], extra => $conf);
	$f{a} = make_node("qa$id", port => $port[1], replication => 1, extra => $conf);
	$f{m}->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
	$f{m}->safe_psql('postgres', sprintf("SELECT kingres.init_cluster('%s', %d)", $f{m}->host, $f{m}->port));
	$f{m}->safe_psql('postgres', sprintf("SELECT kingres.add_node('%s', %d)", $f{a}->host, $f{a}->port));
	$f{part} = [];
	$f{ppx} = [];
	foreach my $i (1 .. 3)
	{
		my $n = make_node("qp${i}_$id", port => $port[1 + $i], tcp => 1, extra => $conf);
		my $p = KsProxy->new(backend_port => $n->port, host => '127.0.0.1', ctl => "$tmp/proxy_q${i}_$id");
		$f{m}->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d)", $p->port));
		push @{ $f{part} }, $n;
		push @{ $f{ppx} }, $p;
	}
	$f{sb} = make_standby($f{a}, "qs$id", port => $port[5]);
	$f{sb}->append_conf('postgresql.conf', "listen_addresses = '127.0.0.1'\n$conf");
	$f{sb}->append_conf('pg_hba.conf', "host all all 127.0.0.1/32 trust\n");
	$f{sb}->restart;
	$f{sp} = KsProxy->new(backend_port => $f{sb}->port, host => '127.0.0.1', ctl => "$tmp/proxy_qs$id");
	$f{m}->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d, 1, 'standby')", $f{sp}->port));
	$f{a}->stop('immediate');
	$f{sb}->promote;
	$f{sb}->poll_query_until('postgres', 'SELECT NOT pg_is_in_recovery()');
	return \%f;
}

# Stop the cluster of a cell and remove its data (seven nodes of about a hundred megabytes each).
sub teardown
{
	my ($f) = @_;
	$_->stop for ($f->{sp}, @{ $f->{ppx} });
	foreach my $n ($f->{m}, $f->{a}, $f->{sb}, @{ $f->{part} })
	{
		release_node($n);
	}
}

sub roles
{
	my ($n) = @_;
	my ($rc, $out, $err) = run_sql($n, "SET lock_timeout = '1s'; SELECT string_agg(node_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node");
	return $rc == 0 ? $out : 'locked';
}

sub statements
{
	my ($file) = @_;
	my $n = 0;
	if (open(my $h, '<', $file)) { while (my $line = <$h>) { $n++; } close $h; }
	return $n;
}

my @report;
note('cells with two and four slow nodes and at 0.5 x node_timeout are skipped: set KINGRES_LONG_TESTS=1 to run them') if !$long;
foreach my $factor ($long ? (0.5, 0.9) : (0.9))
{
	foreach my $k ($long ? (1, 2, 4) : (1))
	{
		my $f = build();
		my @slow = ($f->{sp}, @{ $f->{ppx} })[0 .. $k - 1];
		my @logs;
		foreach my $i (0 .. $#slow)
		{
			my $log = "$tmp/turns_promote_${k}_" . ($factor * 10) . "_$i";
			unlink $log;
			$slow[$i]->set(delay_turn => $factor * $T, skip_turns => 2, turn_log => $log);
			push @logs, $log;
		}
		my $t0 = time;
		my ($rc, $out, $err) = run_sql($f->{m}, "\\set VERBOSITY verbose\nSELECT kingres.promote_node(6)", timeout => 600);
		my $t = time - $t0;
		$_->set(delay_turn => 0, turn_log => '') for ($f->{sp}, @{ $f->{ppx} });
		my @stmts = map { statements($_) } @logs;
		my $total = 0;
		$total += $_ for @stmts;
		my $label = sprintf('promote_node, %d slow node%s at %.1f x node_timeout', $k, $k == 1 ? '' : 's', $factor);
		is($rc, 0, "$label: the call succeeds ($err)");
		unlike($err, qr/KS007/, "$label: without KS007");
		my @warn = ($err =~ /WARNING:[^
]*/g);
		ok(!grep({ $_ !~ /node 2 / } @warn), "$label: and with no warning but those about the failed primary (node 2): " . scalar(@warn));
		like($out, qr/"status": "promoted"/, "$label: and answers promoted ($out)");
		like($out, qr/"unreached": \[2\]/, "$label: with only the failed primary unreached");
		ok(!grep({ $_ == 0 } @stmts), "$label: every slow node was asked (" . join('+', @stmts) . " statements)");
		my $bound = $total * $T + $TOLERANCE;
		ok($t <= $bound, sprintf('%s: %.1f s is within %d statements x node_timeout (%d s) + %d s', $label, $t, $total, $T, $TOLERANCE));
		ok($t >= $total * $factor * $T * 0.8 - 1, sprintf('%s: and the nodes were slow: %.1f s for %d statements of %.1f s', $label, $t, $total, $factor * $T));
		my $want = '1:primary,2:standby,3:primary,4:primary,5:primary,6:primary';
		is(roles($f->{m}), $want, "$label: MAP has the promoted node as the primary");
		is(roles($_), $want, "$label: so has a participant") for @{ $f->{part} };
		is(roles($f->{sb}), $want, "$label: and the promoted node itself");
		my $prep = 0;
		$prep += $_->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts') for ($f->{m}, $f->{sb}, @{ $f->{part} });
		is($prep, 0, "$label: nothing is left prepared");
		($rc, $out, $err) = run_sql($f->{m}, 'SELECT kingres.promote_node(6)');
		like($out, qr/"status": "already"/, "$label: the repeat answers already ($out)");
		is($f->{m}->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster() WHERE check_name NOT IN (\'standby_unreachable\')'), '0', "$label: check_cluster() shows only the failed primary");
		push @report, sprintf('promote_node | %d | %.1f | %s | %.1f s (bound %d s)', $k, $factor, join('+', @stmts), $t, $bound);
		teardown($f);
	}
}
diag("call | slow nodes | x node_timeout | statements to the slow nodes | time\n" . join("\n", @report));
done_testing();
