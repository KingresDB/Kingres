# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/039_node_timeouts.pl
#
# Review findings S3-07, S3-12, S3-15 and S3-16, and the audit of the commands
# the extension sends to other nodes.
#
# S3-07: the wait for a node is bounded all the way: with the postmaster of a
# participant stopped (not only its backend), the committing session returns
# after node_timeout plus the bounded cancel, not when the node resumes.
# S3-12: the recovery worker's session bounds reach the node (one command per
# message) and no pass logs an error.
# S3-16: node_timeout and node_connect_timeout stored for the database do not
# change what the worker sends.
# S3-15: a transaction that prepared remote participants waits for synchronous
# standbys whatever synchronous_commit says.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time);
use KingresShardTest qw(make_cluster keys_of_group run_sql prepared_count wait_prepared_count);

# Every process of a node: the postmaster and its children.
sub node_pids
{
	my ($node) = @_;
	my $pm = $node->{_pid};
	my @kids = split ' ', scalar(readpipe("ps -o pid= --ppid $pm"));
	return ($pm, @kids);
}

# ---------------------------------------------------------------------------
# S3-07: the postmaster of a participant is stopped.
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('pc', 3, extra => "kingres_shard.node_timeout = 3\n");
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	my ($k0) = keys_of_group($n1, 1, 0, 1);
	my ($k1) = keys_of_group($n1, 1, 1, 1);
	my ($k2) = keys_of_group($n1, 1, 2, 1);

	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_pc_$$";
	append_to_file($gate, 'x');
	my $h = $n1->background_psql('postgres', on_error_stop => 0, timeout => 60);
	$h->query_until(qr/started/,
		"SET kingres_shard.fault_injection = 'after_commit:wait:$gate';\n"
		  . "BEGIN; INSERT INTO public.kv VALUES ($k0, 'a'); INSERT INTO public.kv VALUES ($k1, 'b'); INSERT INTO public.kv VALUES ($k2, 'c');\n"
		  . "\\echo started\nCOMMIT;\n");
	ok(wait_prepared_count($n2, 1), 'prepared on node 2');
	ok(wait_prepared_count($n3, 1), 'prepared on node 3');

	# Node 2 freezes completely: the postmaster does not answer either.
	my @pids = node_pids($n2);
	kill 'STOP', @pids;

	my $t0 = time;
	unlink $gate;
	my $ok = eval { $h->query('SELECT 1'); 1 };
	my $elapsed = time - $t0;
	kill 'CONT', @pids;
	$h->quit if $ok;
	ok($ok, 'the committing session returned');
	cmp_ok($elapsed, '<', 20,
		sprintf('S3-07: it returned after %.1f s although the whole of node 2 is stopped (node_timeout is 3)', $elapsed));
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k0"), '1', 'the local part is committed');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k2"), '1',
		'the participant after the frozen one was committed');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	ok(wait_prepared_count($n2, 0), 'the frozen participant is resolved once it answers');
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k1"), '1', 'with its row');
}

# ---------------------------------------------------------------------------
# S3-12 and S3-16: what the recovery worker sends to a node.
# ---------------------------------------------------------------------------
{
	my $conf = "kingres_shard.node_timeout = 3\nkingres_shard.node_connect_timeout = 4\n"
	  . "log_statement = 'all'\nlog_line_prefix = '%m [%p] %a '\n";
	my ($n1, $n2) = make_cluster('wl', 2, recovery_interval => 1, extra => $conf);
	# Settings stored for the database that would change what the worker does.
	$_->safe_psql('postgres', q{
		ALTER DATABASE postgres SET kingres_shard.node_timeout = 1;
		ALTER DATABASE postgres SET kingres_shard.node_connect_timeout = 1;
	}) for ($n1, $n2);
	my $off1 = -s $n1->logfile;
	my $off2 = -s $n2->logfile;
	my $want = qr/\[\d+\] kingres_shard LOG:  execute <unnamed>: SET statement_timeout = 3000\n/;
	my $count = sub {
		my ($node, $off) = @_;
		my $log = slurp_file($node->logfile, $off);
		return scalar(() = $log =~ /$want/g);
	};
	my $passes = 0;
	for (my $i = 0; $i < 600; $i++)
	{
		$passes = $count->($n2, $off2);
		last if $passes >= 3;
		select(undef, undef, undef, 0.1);
	}
	cmp_ok($passes, '>=', 3, 'S3-12: the worker bound its sessions on node 2 in three passes (statement_timeout = node_timeout)');
	for my $n ($n1, $n2)
	{
		my $off = $n == $n1 ? $off1 : $off2;
		my $log = slurp_file($n->logfile, $off);
		like($log, qr/\[\d+\] kingres_shard LOG:  execute <unnamed>: SET lock_timeout = 3000\n/,
			'S3-12: and lock_timeout, on ' . $n->name);
		unlike($log, qr/SET (statement|lock)_timeout = 1000\b/,
			'S3-16: the node_timeout stored for the database did not reach ' . $n->name);
		unlike($log, qr/cannot insert multiple commands/, 'S3-12: no pass logs a refused command on ' . $n->name);
		my @errors = $log =~ /^.* (?:ERROR|FATAL|PANIC):  .*$/mg;
		is(scalar(@errors), 0, 'S3-12: no error at all in the log of ' . $n->name . ' during the passes')
		  or diag(join("\n", @errors));
	}
}

# ---------------------------------------------------------------------------
# S3-15: a multi-node commit waits for the synchronous standby of the origin
# although synchronous_commit is off.
# ---------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('sr', 2, logical => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	my ($k0) = keys_of_group($n1, 1, 0, 1);
	my ($k1) = keys_of_group($n1, 1, 1, 1);

	$n1->backup('sr_backup');
	my $sb = PostgreSQL::Test::Cluster->new('sr_standby');
	$sb->init_from_backup($n1, 'sr_backup', has_streaming => 1);
	$sb->start;
	$n1->append_conf('postgresql.conf', "synchronous_standby_names = 'sr_standby'\n");
	$n1->reload;
	ok($n1->poll_query_until('postgres', q{SELECT count(*) = 1 FROM pg_stat_replication WHERE sync_state = 'sync'}),
		'the standby is the synchronous one');
	$sb->stop;

	my $h = $n1->background_psql('postgres', on_error_stop => 0, timeout => 60);
	$h->query_until(qr/started/,
		"SET synchronous_commit = off;\n"
		  . "BEGIN; INSERT INTO public.kv VALUES ($k0, 'a'); INSERT INTO public.kv VALUES ($k1, 'b');\n"
		  . "\\echo started\nCOMMIT;\n");
	my $waiting = 0;
	for (my $i = 0; $i < 200 && !$waiting; $i++)
	{
		$waiting = $n1->safe_psql('postgres', q{SELECT count(*) FROM pg_stat_activity WHERE wait_event = 'SyncRep'});
		select(undef, undef, undef, 0.1) unless $waiting;
	}
	is($waiting, '1', 'S3-15: the commit waits for the synchronous standby although synchronous_commit is off');
	is(prepared_count($n2), '1', 'and the participant is still only prepared (the decision is not out before it is safe)');

	$sb->start;
	ok($n1->poll_query_until('postgres', q{SELECT count(*) = 0 FROM pg_stat_activity WHERE wait_event = 'SyncRep'}),
		'the commit completes once the standby is back');
	$h->quit;
	ok(wait_prepared_count($n2, 0), 'the participant is committed');
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k1"), '1', 'with its row');
}

done_testing();
