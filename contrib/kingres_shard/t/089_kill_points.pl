# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/089_kill_points.pl
#
# Review S10 ("Not covered"): the node that runs a catalog-changing call is killed at every point
# of the commit protocol, for rotate_secret and add_node, as t/064 does for promote_node (and
# t/028 for move_bucket).  After the restart the call is repeated, recovery runs, and the cluster
# converges: nothing prepared, every node holds the same cluster secret (rotate_secret) or the same
# catalog and a registered node exactly once (add_node), check_cluster() is empty and a distributed
# write works.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster keys_of_group run_sql);

my @points = qw(before_prepare after_first_prepare after_prepare after_log after_commit between_commit_prepared);

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

sub secrets
{
	my (@nodes) = @_;
	return join ',', map { $_->safe_psql('postgres', "SELECT encode(sha256(secret), 'hex') FROM kingres.cluster_secret") } @nodes;
}

sub same_secret
{
	my (@nodes) = @_;
	my %h = map { $_->safe_psql('postgres', "SELECT encode(sha256(secret), 'hex') FROM kingres.cluster_secret") => 1 } @nodes;
	return scalar(keys %h) == 1;
}

sub crash_at
{
	my ($n1, $point, $sql) = @_;
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:kill'; $sql");
	isnt($rc, 0, "[$point] node 1 dies at the point");
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
}

# --- rotate_secret ----------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('kr', 3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $rows = 0;
	foreach my $point (@points)
	{
		my $before = secrets($n1, $n2, $n3);
		crash_at($n1, $point, 'SELECT kingres.rotate_secret()');
		# recovery decides what the dead call left; then the call is repeated
		my ($rc, $out, $err) = run_sql($n1, 'SELECT kingres.recover_prepared_transactions()');
		is($rc, 0, "[rotate/$point] recovery runs ($err)");
		($rc, $out, $err) = run_sql($n1, 'SELECT kingres.recover_prepared_transactions()');
		is(prepared($_), '0', "[rotate/$point] nothing is left prepared on " . $_->name) for ($n1, $n2, $n3);
		($rc, $out, $err) = run_sql($n1, 'SELECT kingres.rotate_secret()');
		is($rc, 0, "[rotate/$point] the repeated rotate_secret succeeds ($err)");
		run_sql($n1, 'SELECT kingres.recover_prepared_transactions()');
		ok(same_secret($n1, $n2, $n3), "[rotate/$point] every node holds the same secret");
		isnt(secrets($n1, $n2, $n3), $before, "[rotate/$point] the secret is not the old one");
		is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', "[rotate/$point] check_cluster is empty");
		$rows += 10;
		my ($wrc, $wout, $werr) = run_sql($n1, join("; ", map { "INSERT INTO public.kv VALUES ($_, 'x')" } ($rows - 9 .. $rows)));
		is($wrc, 0, "[rotate/$point] a distributed write works ($werr)");
	}
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), $rows, "[rotate] no row was lost");
}

# --- add_node ------------------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('ka', 3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $i = 0;
	my $rows = 0;
	my $nodes = 3;
	foreach my $point (@points)
	{
		my $x = make_node("ka_x" . (++$i));
		my $add = sprintf("SELECT kingres.add_node('%s', %d)", $x->host, $x->port);
		crash_at($n1, $point, $add);
		run_sql($n1, 'SELECT kingres.recover_prepared_transactions()');
		run_sql($n1, 'SELECT kingres.recover_prepared_transactions()');
		is(prepared($_), '0', "[add_node/$point] nothing is left prepared on " . $_->name) for ($n1, $n2, $n3);
		my $reg = $n1->safe_psql('postgres', sprintf("SELECT count(*) FROM kingres.node WHERE host = '%s' AND port = %d", $x->host, $x->port));
		ok($reg eq '0' || $reg eq '1', "[add_node/$point] the node is registered $reg time(s) after the crash");
		my ($rc, $out, $err) = run_sql($n1, $add);
		ok($rc == 0 || $err =~ /already registered/, "[add_node/$point] the repeated add_node succeeds or says it is registered ($err)");
		run_sql($n1, 'SELECT kingres.recover_prepared_transactions()');
		$nodes++;
		is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), $nodes, "[add_node/$point] the node is registered exactly once");
		is($x->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), $nodes, "[add_node/$point] and the new node holds the same catalog");
		is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', "[add_node/$point] check_cluster is empty");
		is(prepared($x), '0', "[add_node/$point] nothing is prepared on the new node");
		$rows += 10;
		my ($wrc, $wout, $werr) = run_sql($n1, join("; ", map { "INSERT INTO public.kv VALUES ($_, 'x')" } ($rows - 9 .. $rows)));
		is($wrc, 0, "[add_node/$point] a distributed write works ($werr)");
		$x->stop('immediate');
		# the stopped node would make every later catalog call refuse: take it out again
		run_sql($n1, sprintf("SELECT kingres.remove_node(node_id) FROM kingres.node WHERE port = %d", $x->port));
		$nodes--;
	}
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), $rows, "[add_node] no row was lost");
}

done_testing();
