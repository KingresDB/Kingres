# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/101_sweep_promote.pl
#
# Review S12-01, for promote_node(): the sweep of t/100 (a forwarder that goes silent after the n-th
# chunk of every session, for every n up to the last chunk of the call) with a node of a failed
# group's cluster behind it, twice: the node behind the forwarder is a participant of the catalog
# change (a primary of another group), and it is the node that is promoted.  A fresh cluster is
# built for every position, because a promotion is done once.  The answer is accepted when it is an
# error and nothing changed, or a success and every catalog is done, or a success with the WARNING
# that names the node (it is in "unreached") and the documented repeat (promote_node again, until
# "already") finishes it.
#
# Two more sequences that the review named, in the same fixture:
#   - promote_node() in a REPEATABLE READ transaction answers 25001 and changes nothing;
#   - after a failover the old standbys of the dead primary block rotate_secret (KS014) until they are
#     removed (or re-pointed and confirmed): the way out that the README gives is run.

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
use KsSweep qw(sweep_call chunk_max sweeps);
use KingresShardTest qw(make_node make_cluster make_standby register_standby run_sql release_node);

my $T = 2;
my $C = 2;
my $conf = "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n";
my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my $seq = 0;

# The fixture: MAP (node 1), the primary of group 1 (node 2) with a standby (node 4), the primary of
# group 2 (node 3).  Node 3 and node 4 are registered by the address of a forwarder each.  The primary
# of group 1 is stopped and its standby promoted in PostgreSQL: promote_node(4) is what remains.
# The ports of the nodes are taken from the harness once and used again for every cluster: it keeps a port
# until the test ends, and a sweep builds a cluster for every position.
my @port = map { PostgreSQL::Test::Cluster::get_free_port() } (1 .. 4);

sub build
{
	my $id = ++$seq;
	my %f;
	$f{m} = make_node("pm$id", port => $port[0], extra => $conf);
	$f{a} = make_node("pa$id", port => $port[1], replication => 1, extra => $conf);
	$f{p} = make_node("pp$id", port => $port[2], tcp => 1, extra => $conf);
	$f{m}->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
	$f{m}->safe_psql('postgres', sprintf("SELECT kingres.init_cluster('%s', %d)", $f{m}->host, $f{m}->port));
	$f{m}->safe_psql('postgres', sprintf("SELECT kingres.add_node('%s', %d)", $f{a}->host, $f{a}->port));
	$f{sb} = make_standby($f{a}, "ps$id", port => $port[3]);
	$f{sb}->append_conf('postgresql.conf', "listen_addresses = '127.0.0.1'\n$conf");
	$f{sb}->append_conf('pg_hba.conf', "host all all 127.0.0.1/32 trust\n");
	$f{sb}->restart;
	$f{pp} = KsProxy->new(backend_port => $f{p}->port, host => '127.0.0.1', ctl => "$tmp/proxy_pp$id");
	$f{sp} = KsProxy->new(backend_port => $f{sb}->port, host => '127.0.0.1', ctl => "$tmp/proxy_sp$id");
	$f{m}->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d)", $f{pp}->port));
	$f{m}->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d, 1, 'standby')", $f{sp}->port));
	$f{a}->stop('immediate');
	$f{sb}->promote;
	$f{sb}->poll_query_until('postgres', 'SELECT NOT pg_is_in_recovery()');
	return \%f;
}

# Stop the cluster of a position and remove its data: a sweep builds a cluster for every position, and the
# data of a dozen of them is gigabytes.
sub teardown
{
	my ($f) = @_;
	$f->{pp}->stop;
	$f->{sp}->stop;
	foreach my $n (grep { defined } @$f{qw(m a p sb)})
	{
		release_node($n);
	}
}

sub call
{
	my ($f, $sql) = @_;
	return run_sql($f->{m}, "\\set VERBOSITY verbose\n$sql", timeout => 120);
}

sub roles
{
	my ($n) = @_;
	my ($rc, $out, $err) = run_sql($n, "SET lock_timeout = '1s'; SELECT string_agg(node_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node");
	return $rc == 0 ? $out : 'locked';
}

# The repeat of the README: promote_node() again, up to ten times, until it answers "already".
sub repeat_promote
{
	my ($f) = @_;
	my @last;
	for (1 .. 10)
	{
		@last = call($f, 'SELECT kingres.promote_node(4)');
		last if $last[0] == 0 && $last[1] =~ /"already"/ && $last[1] =~ /"final": true/;
		sleep 1;
	}
	return @last;
}

# --- the isolation level, once -----------------------------------------------------------------------------
my $silent = !$ENV{KS_SWEEP_SCENARIO} || $ENV{KS_SWEEP_SCENARIO} =~ /silent/;	# the fixed sequences run with the default scenario only

if ($silent)
{
	my $f = build();
	my ($rc, $out, $err) = call($f, "SET default_transaction_isolation = 'repeatable read';\nSELECT kingres.promote_node(4)");
	isnt($rc, 0, 'promote_node() in a REPEATABLE READ transaction is refused');
	like($err, qr/25001/, 'with 25001');
	like($err, qr/must run in a READ COMMITTED transaction/, 'and says why');
	is(roles($f->{m}), '1:primary,2:primary,3:primary,4:standby', 'nothing was changed');
	($rc, $out, $err) = call($f, "SET default_transaction_isolation = 'repeatable read';\nBEGIN ISOLATION LEVEL READ COMMITTED;\nSELECT kingres.promote_node(4);\nCOMMIT");
	is($rc, 0, "the way out: the call in a READ COMMITTED transaction works under that default ($err)");
	teardown($f);
}

# --- the sweeps ------------------------------------------------------------------------------------------------
my %results;
foreach my $which ('participant', 'target')
{
	my $f;
	my $c;
	$c = {
		tmp => $tmp,
		name => "promote_node ($which silent)", run => 'SELECT kingres.promote_node(4)',
		call => sub { return call($f, $_[0]); },
		prepare => sub {
			teardown($f) if $f;
			$f = build();
			$c->{n1} = $f->{m};
			$c->{proxy} = $which eq 'participant' ? $f->{pp} : $f->{sp};
			$c->{nodes} = [ $f->{m}, $f->{p}, $f->{sb} ];
		},
		state => sub { return { m => roles($f->{m}), p => roles($f->{p}), sb => roles($f->{sb}) }; },
		cleanup => sub { },
		repeat => sub { return repeat_promote($f); },
		bound => 2 * $T + 8 + $C,
		left_if_left => 1,
	};
	$results{$which} = sweeps($c);
	teardown($f);
	$f = undef;
}

# --- after a failover the old standbys of the dead primary block rotate_secret (S12, "For the control plane") -----
# The previous secret is kept after a rotation until every standby of the catalog has confirmed it; a
# second rotation is refused (KS014) while it is kept and a standby cannot be reached with the current
# secret.  The dead primary (a standby of the catalog after the promotion) and a standby that still
# streams from it are such standbys.  The way out of the README: take them out with remove_node().
if ($silent)
{
	my ($map, $g1, $g2) = make_cluster('rf', 3, replication => 1, extra => $conf);
	my $sb = make_standby($g1, 'rf_s1');
	my $sb2 = make_standby($g1, 'rf_s2');
	my $id = register_standby($map, $sb, 1);
	my $id2 = register_standby($map, $sb2, 1);
	$g1->wait_for_catchup($sb);
	$g1->wait_for_catchup($sb2);
	my $has_previous = sub { return $map->safe_psql('postgres', 'SELECT previous IS NOT NULL FROM kingres.cluster_secret'); };
	my ($rc, $out, $err) = run_sql($map, 'SELECT kingres.rotate_secret()');
	is($rc, 0, "the first rotation works ($err)");
	$g1->wait_for_catchup($sb);
	$g1->wait_for_catchup($sb2);
	($rc, $out, $err) = run_sql($map, 'SELECT kingres.recover_prepared_transactions()');
	is($has_previous->(), 'f', 'both standbys confirmed it: the previous secret is gone');
	$g1->stop('immediate');
	$sb->promote;
	$sb->poll_query_until('postgres', 'SELECT NOT pg_is_in_recovery()');
	($rc, $out, $err) = run_sql($map, "SELECT kingres.promote_node($id)");
	is($rc, 0, "the failover works ($err)");
	for (1 .. 5)
	{
		last if $out =~ /"status": "already"/;
		($rc, $out, $err) = run_sql($map, "SELECT kingres.promote_node($id)");
	}
	($rc, $out, $err) = run_sql($map, 'SELECT kingres.rotate_secret()');
	is($rc, 0, "the next rotation works: the previous secret was gone ($err)");
	is($has_previous->(), 't', 'and keeps the previous secret now: the dead primary and the stale standby cannot confirm');
	($rc, $out, $err) = run_sql($map, "\\set VERBOSITY verbose\nSELECT kingres.rotate_secret()");
	isnt($rc, 0, 'the one after it is refused');
	like($err, qr/KS014/, "with KS014 ($err)");
	like($err, qr/standby \d+ \(.*has not confirmed the last rotation/, 'naming a standby');
	# the way out: remove the standbys that will not come back
	my $removed = 0;
	for (1 .. 3)
	{
		last if $rc == 0;
		my ($sid) = ($err =~ /standby (\d+) \(/);
		last unless defined $sid;
		my ($rrc, $rout, $rerr) = run_sql($map, "SELECT kingres.remove_node($sid)");
		is($rrc, 0, "remove_node($sid) removes it ($rerr)");
		$removed++;
		($rc, $out, $err) = run_sql($map, "\\set VERBOSITY verbose\nSELECT kingres.rotate_secret()");
	}
	is($rc, 0, "after removing the standbys ($removed) the rotation works ($err)");
	cmp_ok($removed, '>=', 1, 'at least one standby had to be removed');
	$_->stop('immediate', fail_ok => 1) for ($sb, $sb2, $g2);
}

done_testing();
