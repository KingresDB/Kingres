# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/096_call_bounds.pl
#
# Review S11-07 and S11-10, and S12-03: a control-plane call has a bound for a node that stops
# answering.  The README says: such a node ends the call within 2 x T + 8 seconds (T =
# kingres_shard.node_timeout), plus node_connect_timeout once for connecting to it.  (The budget of
# 2 x T + 2 seconds for the whole call, which also ended calls against a node that answered every
# statement late but in time, is gone: S12-03.  That a node that answers late but in time never fails a
# call is t/102.)
#
# A node is made silent after its Nth message, or silent from the start, by a forwarder in
# front of it (t/KsProxy.pm).  Each call is run against it and timed; the bound that is asserted is
# the one the README states, with the tolerance the README states (4 seconds, for a loaded machine).
# After the call the node answers again and the same call, repeated, succeeds with nothing left
# prepared.  The case "silent after the 6th message" is the one in which the deadline of a statement
# after the first is the only thing that ends the call (S11-10).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KsProxy;
use KingresShardTest qw(make_node make_standby run_sql prepared_count);

my $T = 3;				# node_timeout
my $C = 2;				# node_connect_timeout
my $TOLERANCE = 4;		# README: "Bounds": seconds of tolerance for a loaded machine
my $BUDGET = 2 * $T + 8;	# what a call waits for a node that stops answering (README)
my $seq = 0;

sub setup
{
	my $prefix = 'cb' . (++$seq);
	my $n1 = make_node("${prefix}_1", extra => "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n");
	my $n2 = make_node("${prefix}_2", extra => "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n");
	my $n3 = make_node("${prefix}_3", tcp => 1, extra => "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n");
	$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
	$n1->safe_psql('postgres', sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
	$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('%s', %d)", $n2->host, $n2->port));
	my $proxy = KsProxy->new(backend_port => $n3->port, host => '127.0.0.1', ctl => "$PostgreSQL::Test::Utils::tmp_check/proxy_$seq");
	$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d)", $proxy->port));
	return ($prefix, $n1, $n2, $n3, $proxy);
}

sub timed
{
	my ($node, $sql) = @_;
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($node, "\\set VERBOSITY verbose\n$sql", timeout => 120);
	return (time - $t0, $rc, $out, $err);
}

my @modes = (
	[ 'silent after the 6th message of a session (a later statement)', { swallow_after => 6 } ],
	[ 'silent from the start of a session (the connection phase)', { swallow_all => 1 } ],
);
my @calls = (
	[ 'sync_catalog', 'SELECT kingres.sync_catalog()' ],
	[ 'rotate_secret', 'SELECT kingres.rotate_secret()' ],
);

my ($prefix, $n1, $n2, $n3, $proxy) = setup();
foreach my $mode (@modes)
{
	my ($mname, $opt) = @$mode;
	foreach my $c (@calls)
	{
		my ($cname, $sql) = @$c;
		# a session of the caller is made first, so that its own connection costs nothing
		$proxy->set(%$opt);
		my ($t, $rc, $out, $err) = timed($n1, $sql);
		my $bound = $BUDGET + $C + $TOLERANCE;
		ok($t <= $bound, "$cname, $mname: returned in " . sprintf('%.1f', $t) . " s (README: at most $BUDGET + $C s, tolerance $TOLERANCE s)");
		isnt($rc, 0, "$cname, $mname: ended with an error, not with a success that skipped the node");
		like($err, qr/KS007|55P03|08001|08006/, "$cname, $mname: with a code that says the node did not answer ($err)");
		my $port = $proxy->port;
		like($err, qr/node 3 \(|127\.0\.0\.1:$port/, "$cname, $mname: and names node 3 ($err)");
		$proxy->set(delay => 0, skip_chunks => 0);
		sleep 1;
		is(prepared_count($n1) + prepared_count($n2) + prepared_count($n3), 0, "$cname, $mname: nothing is left prepared");
		($rc, $out, $err) = run_sql($n1, $sql, timeout => 120);
		is($rc, 0, "$cname, $mname: the repeated call succeeds when the node answers ($err)");
		is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', "$cname, $mname: check_cluster is empty");
	}
}

$proxy->stop;

# --- the node to promote accepts the session and does not answer (S11-09) --------------------------
# The README says KS007 for it; the code answered 08001 or 08006 without a reason.  The standby is
# registered by the address of the forwarder, promoted in PostgreSQL, and then goes silent after the
# first messages of every session: promote_node() must answer KS007 naming it, within the bound, and
# promote it when it answers again.
{
	my $prefix = 'cb_p';
	my $conf = "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n";
	my $m = make_node("${prefix}_m", extra => $conf);
	my $g1 = make_node("${prefix}_g1", replication => 1, extra => $conf);
	$m->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
	$m->safe_psql('postgres', sprintf("SELECT kingres.init_cluster('%s', %d)", $m->host, $m->port));
	$m->safe_psql('postgres', sprintf("SELECT kingres.add_node('%s', %d)", $g1->host, $g1->port));
	my $sb = make_standby($g1, "${prefix}_sb");
	$sb->append_conf('postgresql.conf', "listen_addresses = '127.0.0.1'\nkingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n");
	$sb->append_conf('pg_hba.conf', "host all all 127.0.0.1/32 trust\n");
	$sb->restart;
	my $p = KsProxy->new(backend_port => $sb->port, host => '127.0.0.1', ctl => "$PostgreSQL::Test::Utils::tmp_check/proxy_p");
	my $id = $m->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d, 1, 'standby')", $p->port));
	$g1->stop('immediate');
	$sb->promote;
	$sb->poll_query_until('postgres', 'SELECT NOT pg_is_in_recovery()');
	foreach my $after (2, 4, 6)
	{
		$p->set(swallow_after => $after);
		my ($t, $rc, $out, $err) = timed($m, "SELECT kingres.promote_node($id)");
		ok($t <= 3 * $T + $C + $TOLERANCE, "promote_node, the node to promote silent after $after messages: returned in " . sprintf('%.1f', $t) . " s");
		isnt($rc, 0, "promote_node, silent after $after messages: refused");
		like($err, qr/KS007/, "promote_node, silent after $after messages: with KS007, not 08001/08006 ($err)");
		like($err, qr/node $id \(/, "promote_node, silent after $after messages: naming the node");
	}
	$p->set(swallow_after => '');
	my ($rc, $out, $err) = run_sql($m, "SELECT kingres.promote_node($id)");
	is($rc, 0, 'promote_node succeeds when the node answers again');
	like($out, qr/promoted/, "and says promoted ($out)");
	$p->stop;
}

done_testing();
