# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/102_slow_nodes.pl
#
# Review S12-03 and S12-02: a node that answers every statement late, but within node_timeout, is a
# working node.  The budget of 2 x node_timeout + 2 seconds per node and call (S11-07) ended calls
# against such a node with KS007, which the README says never to repeat against that node: a control
# plane would have fenced a working node.  There is no budget any more: every statement of a call
# waits for its node at most node_timeout, and the README states the bound of a call as the number of
# statements that it sends to a node times node_timeout.
#
# Here every call that the README names (promote_node is t/103) is run against one, two and four
# nodes that answer every statement after 0.5 and after 0.9 x node_timeout (a forwarder that holds the
# answer until that time has passed since the statement was forwarded, t/KsProxy.pm).  The call must
# succeed, without a WARNING and without KS007, the work must be done on every node, nothing may be
# left prepared, and the time must stay within the formula of the README: the sum over the slow nodes
# of (statements sent to the node) x node_timeout, plus the work on the nodes that answer.  The
# statements are counted by the forwarder.  The last two cells copy a large table.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KsProxy;
use KingresShardTest qw(make_node run_sql);

my $long = $ENV{KINGRES_LONG_TESTS} ? 1 : 0;
# node_timeout: 2 s in the long run; 3 s in the default run, whose cells answer at 0.9 x node_timeout and so
# leave 0.3 s to a machine that is busy (at 2 s the 0.2 s that were left were lost when four tests ran at once)
my $T = $long ? 2 : 3;						# node_timeout
my $C = 2;						# node_connect_timeout
my $TOLERANCE = 4;				# README: tolerance of the tests, for a loaded machine
my $conf = "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n";
my $tmp = $PostgreSQL::Test::Utils::tmp_check;

# --- the cluster: MAP, four nodes behind a forwarder each, and a sixth node for add_node/remove_node ---
my $n1 = make_node('sl_1', extra => $conf);
$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres', sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
my (@nodes, @proxies);
foreach my $i (2 .. 5)
{
	my $n = make_node("sl_$i", tcp => 1, extra => $conf);
	my $p = KsProxy->new(backend_port => $n->port, host => '127.0.0.1', ctl => "$tmp/proxy_sl$i");
	$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d)", $p->port));
	push @nodes, $n;
	push @proxies, $p;
}
my $n6 = make_node('sl_6', tcp => 1, extra => $conf);
my $p6 = KsProxy->new(backend_port => $n6->port, host => '127.0.0.1', ctl => "$tmp/proxy_sl6");
my @all = ($n1, @nodes, $n6);

sub call
{
	my ($sql) = @_;
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($n1, "\\set VERBOSITY verbose\n$sql", timeout => 300);
	return (time - $t0, $rc, $out, $err);
}

sub qn
{
	my ($n, $sql) = @_;
	my ($rc, $out, $err) = run_sql($n, "SET lock_timeout = '1s'; $sql");
	return $rc == 0 ? $out : 'locked';
}

sub fast
{
	$_->set(swallow_after => '', delay_turn => 0, turn_log => '') for (@proxies, $p6);
}

sub prepared_total
{
	my $t = 0;
	$t += $_->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts') for @all;
	return $t;
}

# the longest time that the forwarder held a statement before it let the answer through (it logs it)
sub natural_max
{
	my ($file) = @_;
	my $m = 0;
	if (open(my $h, '<', $file)) { while (my $line = <$h>) { $m = $1 if $line =~ /^\d+ [0-9.]+ ([0-9.]+)$/ && $1 > $m; } close $h; }
	return $m;
}

sub statements
{
	my ($file) = @_;
	my $n = 0;
	if (open(my $h, '<', $file)) { while (my $line = <$h>) { $n++; } close $h; }
	return $n;
}

# The calls.  slow: which forwarders are slow for k slow nodes (the first k of the list, by name);
# prepare/cleanup run with every node fast; done: the state after the call.
my $p6port = $p6->port;
my $rows = 'public.sl_t';
my $xid = 4000000000;
my %call;

$call{sync_catalog} = {
	run => 'SELECT kingres.sync_catalog()',
	slow => sub { return (@proxies); },
	prepare => sub { $n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'); },
	done => sub { return 1; },
};
$call{rotate_secret} = {
	run => 'SELECT kingres.rotate_secret()',
	slow => sub { return (@proxies); },
	prepare => sub { $n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'); },
	done => sub {
		my %h = map { $_->safe_psql('postgres', "SELECT encode(sha256(secret), 'hex') FROM kingres.cluster_secret") => 1 } ($n1, @nodes);
		return keys(%h) == 1;
	},
};
foreach my $kind ('distributed', 'reference', 'reference_large')
{
	my $big = $kind eq 'reference_large';
	$call{"create_${kind}_table"} = {
		run => $kind eq 'distributed'
		  ? "SELECT kingres.create_distributed_table('$rows', 'id', bucket_count => 6)"
		  : "SELECT kingres.create_reference_table('$rows')",
		slow => sub { return (@proxies); },
		prepare => sub {
			$n1->safe_psql('postgres', "DROP TABLE IF EXISTS $rows CASCADE");
			$n1->safe_psql('postgres', "CREATE TABLE $rows (id int PRIMARY KEY, val text)");
			$n1->safe_psql('postgres', "INSERT INTO $rows SELECT i, repeat('x', 20) FROM generate_series(1, " . ($big ? 60000 : 300) . ") i")
			  if $kind ne 'distributed';
		},
		done => sub {
			foreach my $n (@nodes)
			{
				return 0 unless qn($n, "SELECT to_regclass('$rows') IS NOT NULL") eq 't';
				return 0 if $kind ne 'distributed' && qn($n, "SELECT count(*) FROM $rows") ne ($big ? 60000 : 300);
			}
			return 1;
		},
		cleanup => sub {
			$n1->safe_psql('postgres', "DROP TABLE IF EXISTS $rows CASCADE");
		},
		large => $big,
	};
}
$call{recover_prepared_transactions} = {
	run => 'SELECT kingres.recover_prepared_transactions()',
	slow => sub { return (@proxies); },
	prepare => sub {
		# a transaction in doubt, with a decision, on every node behind a forwarder
		$n1->safe_psql('postgres', 'DELETE FROM kingres.dtx_log');
		foreach my $i (0 .. $#nodes)
		{
			my $id = $i + 2;
			$nodes[$i]->safe_psql('postgres', 'CREATE TABLE IF NOT EXISTS public.sl_rec (id int)');
			$nodes[$i]->safe_psql('postgres', 'DELETE FROM public.sl_rec');
			my $body = 'kingres_h0_1_999_' . (++$xid) . "_$id";
			my $gid = $body . '_' . $n1->safe_psql('postgres', "SELECT kingres.internal_gid_tag('$body')");
			$nodes[$i]->safe_psql('postgres', "BEGIN; INSERT INTO public.sl_rec VALUES (1); PREPARE TRANSACTION '$gid'");
			$n1->safe_psql('postgres', "INSERT INTO kingres.dtx_log VALUES ('$gid', $id, now())");
		}
	},
	done => sub {
		foreach my $n (@nodes)
		{
			return 0 unless qn($n, 'SELECT count(*) FROM public.sl_rec') eq '1';
		}
		return 1;
	},
};
$call{add_node} = {
	run => "SELECT kingres.add_node('127.0.0.1', $p6port)",
	slow => sub { return ($p6, @proxies); },
	prepare => sub { },
	done => sub { return $n1->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE port = $p6port") eq '1'
		&& qn($n6, 'SELECT count(*) FROM kingres.node') eq $n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'); },
	cleanup => sub {
		my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.remove_node((SELECT node_id FROM kingres.node WHERE port = $p6port))");
		die "cleanup remove_node: $err" if $rc != 0;
	},
};
$call{remove_node} = {
	run => "SELECT kingres.remove_node((SELECT node_id FROM kingres.node WHERE port = $p6port))",
	slow => sub { return ($p6, @proxies); },
	prepare => sub {
		my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.add_node('127.0.0.1', $p6port)");
		die "prepare add_node: $err" if $rc != 0;
	},
	done => sub { return $n1->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE port = $p6port") eq '0'
		&& qn($n6, q{SELECT CASE WHEN to_regclass('kingres.node') IS NULL THEN '0' ELSE (SELECT count(*)::text FROM kingres.node) END}) eq '0'; },
};

my @order = qw(sync_catalog rotate_secret create_distributed_table create_reference_table recover_prepared_transactions add_node remove_node);
my @report;

sub cell
{
	my ($name, $k, $factor) = @_;
	my $c = $call{$name};
	# The claim is about a node whose every answer comes within node_timeout.  The forwarder measures every
	# answer; when the machine stalled and one took node_timeout or more, the premise did not hold in that
	# attempt and the cell is run again (three attempts; the last one counts).
	my ($t, $rc, $out, $err, $nat, @stmts);
	for my $attempt (1 .. 3)
	{
		fast();
		$c->{prepare}->();
		my @slow = ($c->{slow}->())[0 .. $k - 1];
		my @logs;
		foreach my $i (0 .. $#slow)
		{
			my $log = "$tmp/turns_${name}_${k}_" . ($factor * 10) . "_$i";
			unlink $log;
			$slow[$i]->set(delay_turn => $factor * $T, skip_turns => 2, turn_log => $log);
			push @logs, $log;
		}
		($t, $rc, $out, $err) = call($c->{run});
		fast();
		@stmts = map { statements($_) } @logs;
		$nat = 0;
		foreach my $l (@logs) { my $x = natural_max($l); $nat = $x if $x > $nat; }
		last if $nat < $T;
		diag(sprintf('%s, %d slow nodes at %.1f: attempt %d: the machine stalled, an answer took %.2f s (node_timeout %d s): the premise did not hold; again', $name, $k, $factor, $attempt, $nat, $T));
		$c->{cleanup}->() if $c->{cleanup};
		$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	}
	my $total = 0;
	$total += $_ for @stmts;
	my $label = sprintf('%s, %d slow node%s at %.1f x node_timeout', $name, $k, $k == 1 ? '' : 's', $factor);
	is($rc, 0, sprintf('%s: the call succeeds (the slowest answer took %.2f s) (%s)', $label, $nat, $err));
	unlike($err, qr/WARNING|KS007/, "$label: without a warning and without KS007");
	ok(!grep({ $_ == 0 } @stmts), "$label: every slow node was asked (" . join('+', @stmts) . " statements)");
	my $bound = $total * $T + $TOLERANCE;
	ok($t <= $bound, sprintf('%s: %.1f s is within %d statements x node_timeout (%d s) + %d s', $label, $t, $total, $T, $TOLERANCE));
	ok($t >= $total * $factor * $T * 0.8 - 1, sprintf('%s: and the nodes were slow: %.1f s for %d statements of %.1f s', $label, $t, $total, $factor * $T));
	ok($c->{done}->(), "$label: the work is done on every node");
	is(prepared_total(), 0, "$label: nothing is left prepared");
	$c->{cleanup}->() if $c->{cleanup};
	fast();
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', "$label: check_cluster() is empty");
	push @report, sprintf('%s | %d | %.1f | %s | %.1f s (bound %d s)', $name, $k, $factor, join('+', @stmts), $t, $bound);
}

# KS_SLOW_CALLS, KS_SLOW_NODES, KS_SLOW_FACTORS (comma lists) run some of the cells only, for work on one of them
my %want_call = map { $_ => 1 } split /,/, ($ENV{KS_SLOW_CALLS} // '');
# The default run (a few minutes) is the case of the finding: one node that answers every statement at 0.9 x
# node_timeout, for every call, and the copy of 60000 rows.  KINGRES_LONG_TESTS=1 adds one, two and four slow
# nodes at 0.5 and 0.9 x node_timeout (about 22 minutes).
my @want_k = $ENV{KS_SLOW_NODES} ? split(/,/, $ENV{KS_SLOW_NODES}) : ($long ? (1, 2, 4) : (1));
my @want_f = $ENV{KS_SLOW_FACTORS} ? split(/,/, $ENV{KS_SLOW_FACTORS}) : ($long ? (0.5, 0.9) : (0.9));
note('cells with two and four slow nodes and at 0.5 x node_timeout are skipped: set KINGRES_LONG_TESTS=1 to run them') if !$long;
foreach my $name (@order)
{
	next if %want_call && !$want_call{$name};
	foreach my $factor (@want_f)
	{
		cell($name, $_, $factor) for @want_k;
	}
}

# a large table, copied by create_reference_table: many statements, none of them late
foreach my $factor (@want_f)
{
	cell('create_reference_large_table', 1, $factor) if !%want_call || $want_call{create_reference_large_table};
}

diag("call | slow nodes | x node_timeout | statements to the slow nodes | time\n" . join("\n", @report));
done_testing();
