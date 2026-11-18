# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/111_two_nodes.pl
#
# S13-05: two nodes fail in one call.  rotate_secret() runs against a cluster in which two participants are behind
# forwarders (t/KsProxy.pm); one goes silent after chunk a of its sessions and the other after chunk b, for a and b
# taken from the positions of the sweep of t/100 (a sample by default: the first, one in each phase of the call, the
# last; every position with KINGRES_LONG_TESTS=1, which also adds the closed connection to the silent one).
# The standard of a control call (README): it never returns success for work that was not done on a node without
# saying so.  An answer is accepted when
#   - it is an error, and the secret is the old one on every node (or, after one recovery pass, the old one or the new
#     one everywhere), never a mixture;
#   - it is a success without a WARNING, and the secret is the new one on every node, nothing is prepared;
#   - it is a success with a WARNING that names a node that was left, and after one recovery pass the secret is the
#     new one on every node and nothing is prepared.
# and it ends within twice the README's bound for one node that stops answering.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KsProxy;
use KsSweep qw(sample_positions chunk_max settle);
use KingresShardTest qw(make_node run_sql);

my $T = 2;
my $C = 2;
my $BOUND = 2 * (2 * $T + 8 + $C);
my $long = $ENV{KINGRES_LONG_TESTS} ? 1 : 0;
my $conf = "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n";
my $tmp = $PostgreSQL::Test::Utils::tmp_check;

my $n1 = make_node('tn_1', extra => $conf);
my $n2 = make_node('tn_2', tcp => 1, extra => $conf);
my $n3 = make_node('tn_3', tcp => 1, extra => $conf);
$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres', sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
my $p2 = KsProxy->new(backend_port => $n2->port, host => '127.0.0.1', ctl => "$tmp/proxy_tn2");
my $p3 = KsProxy->new(backend_port => $n3->port, host => '127.0.0.1', ctl => "$tmp/proxy_tn3");
$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d)", $_->port)) for ($p2, $p3);
my @nodes = ($n1, $n2, $n3);
my $ports = join('|', $p2->port, $p3->port);

sub call
{
	my ($sql) = @_;
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($n1, "\\set VERBOSITY verbose\n$sql", timeout => 180);
	return (time - $t0, $rc, $out, $err);
}

sub secrets
{
	my @h = map { $_->safe_psql('postgres', q{SET lock_timeout = '1s'; SELECT encode(sha256(secret), 'hex') FROM kingres.cluster_secret}) } @nodes;
	return @h;
}

sub prepared_total
{
	my $t = 0;
	$t += $_->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts') for @nodes;
	return $t;
}

sub open_all
{
	$_->set(swallow_after => '', close_after => '', delay => 0, delay_turn => 0) for ($p2, $p3);
}

# the positions: the sweep's positions of one participant, from a clean run
open_all();
my $log = "$tmp/tn_chunks";
my $tlog = "$log.text";
unlink $log, $tlog;
$p2->set(chunk_log => $log, text_log => $tlog);
{
	my ($t, $rc, $out, $err) = call('SELECT kingres.rotate_secret()');
	is($rc, 0, "the clean run succeeds ($err)");
}
open_all();
my $max = chunk_max($log);
my @positions = @{ sample_positions($max, $tlog, 0) };
if (!$long && @positions > 4)
{
	my $n = @positions;
	my %pick = map { $_ => 1 } (0, int($n / 3), int(2 * $n / 3), $n - 1);
	@positions = @positions[ sort { $a <=> $b } keys %pick ];
}
@positions = (0 .. $max) if $long;
diag("rotate_secret sends up to $max chunks to a participant; positions: @positions");
my @modes = $long ? ('swallow_after', 'close_after') : ('swallow_after');

foreach my $mode (@modes)
{
	foreach my $a (@positions)
	{
		foreach my $b (@positions)
		{
			my $label = "rotate_secret, node 2 $mode $a, node 3 $mode $b";
			call('SELECT kingres.recover_prepared_transactions()');
			my @old = secrets();
			open_all();
			$p2->set($mode => $a);
			$p3->set($mode => $b);
			my ($t, $rc, $out, $err) = call('SELECT kingres.rotate_secret()');
			open_all();
			settle({ nodes => \@nodes });
			my @s = secrets();
			my %distinct = map { $_ => 1 } @s;
			my $same = keys(%distinct) == 1;
			my $new = $same && $s[0] ne $old[0];
			my $warn = $err =~ /WARNING/;
			if ($rc != 0)
			{
				ok($same && !$new, "$label: an error leaves the old secret on every node (error: " . (split /\n/, $err)[0] . ')')
				  or diag("old @old; now @s");
			}
			elsif (!$warn)
			{
				ok($new, "$label: a success without a warning is done on every node");
				is(prepared_total(), 0, "$label: and leaves nothing prepared");
			}
			else
			{
				like($err, qr/:(?:$ports)\b/, "$label: a success with a warning names a node that was left");
			}
			cmp_ok($t, '<=', $BOUND, sprintf("$label: ended in %.1f s (bound %d s)", $t, $BOUND));
			if ($rc != 0 || $warn || prepared_total() > 0)
			{
				my ($t2, $rc2, $out2, $err2) = call('SELECT kingres.recover_prepared_transactions()');
				is($rc2, 0, "$label: the recovery pass runs ($err2)");
				@s = secrets();
				%distinct = map { $_ => 1 } @s;
				my $same2 = keys(%distinct) == 1;
				ok($same2, "$label: after the recovery pass there is no mixture of secrets") or diag("old @old; now @s");
				is(prepared_total(), 0, "$label: after the recovery pass nothing is prepared");
				ok($s[0] ne $old[0], "$label: and the call that said success is done") if $rc == 0;
			}
		}
	}
}

$p2->stop;
$p3->stop;
done_testing();
