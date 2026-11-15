# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/016_recovery_ordering.pl
#
# Review findings S2-01 and S2-02 (reviews/2026-11-14-kingres-shard-0.2.md):
#  - recovery must look for the decision row with a snapshot taken after it has
#    established that the origin transaction is over (deterministic window test,
#    then a stress test: recovery loops, also two at once, against concurrent
#    multi-node commits);
#  - the decision must be durable before any COMMIT PREPARED is sent, whatever
#    synchronous_commit says.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run ();
use KingresShardTest qw(make_cluster keys_of_group run_sql prepared_count
  wait_prepared_count);

# ---------------------------------------------------------------------------
# S2-01: a recovery whose snapshot is older than the commit of T.
# ---------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('ro', 2);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 8);
	});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kb) = keys_of_group($n1, 1, 1, 1);

	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_ro_$$";
	append_to_file($gate, 'x');

	# The recovery statement: its snapshot is taken now, its work starts later.
	my $r = $n1->background_psql('postgres', on_error_stop => 0);
	$r->query_until(qr/started/,
		"\\echo started\nSELECT pg_sleep(8), kingres.recover_prepared_transactions();\n");

	# T commits after that snapshot and stops right after its local commit.
	my $t = $n1->background_psql('postgres', on_error_stop => 0);
	$t->query_until(qr/started/,
		"SET kingres_shard.fault_injection = 'after_commit:wait:$gate';\n"
		  . "BEGIN; INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kb, 'b');\n"
		  . "\\echo started\nCOMMIT;\n");
	ok($n1->poll_query_until('postgres', 'SELECT count(*) = 1 FROM kingres.dtx_log'),
		'T has committed locally and logged its decision');
	is(prepared_count($n2), '1', 'T is still prepared on node 2');

	# Let the old-snapshot recovery run while T is held after its local commit.
	my ($out) = $r->query('SELECT 1');
	unlink $gate;
	$t->query('SELECT 1');
	$t->quit;
	$r->quit;
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $kb"), '1',
		'S2-01: the participant of a committed transaction was not rolled back by a recovery with an old snapshot');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $ka"), '1',
		'S2-01: the local side is there too');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared_count($n2), '0', 'nothing stays prepared');
}

# ---------------------------------------------------------------------------
# S2-01 stress: recovery (two runs at once) against concurrent commits.
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('rs', 3, max_prepared => 100,
		extra => "max_connections = 100\n");
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, txn int);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	my $dir = $PostgreSQL::Test::Utils::tmp_check;
	append_to_file("$dir/stress_txn.sql",
		"\\set a random(1, 100000000)\n\\set b random(100000001, 200000000)\n"
		  . "BEGIN;\nINSERT INTO public.kv VALUES (:a, :a);\nINSERT INTO public.kv VALUES (:b, :a);\nCOMMIT;\n");
	append_to_file("$dir/stress_rec.sql", "SELECT kingres.recover_prepared_transactions();\n");
	my @base = ('pgbench', '-n', '-h', $n1->host, '-p', $n1->port);
	my ($o1, $e1, $o2, $e2) = ('', '', '', '');
	my $load = IPC::Run::start([@base, '-c', 6, '-j', 2, '-T', 10, '-f', "$dir/stress_txn.sql", 'postgres'],
		'>', \$o1, '2>', \$e1);
	my $rec = IPC::Run::start([@base, '-c', 2, '-j', 2, '-T', 10, '-f', "$dir/stress_rec.sql", 'postgres'],
		'>', \$o2, '2>', \$e2);
	$load->finish;
	$rec->finish;
	like($o1, qr/number of failed transactions: 0|number of transactions actually processed/,
		'the stress load ran');
	diag($e1) if $e1 =~ /ERROR|WARNING/ && 0;
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared_count($_), '0', "stress: nothing left prepared on @{[$_->name]}") for ($n1, $n2, $n3);
	is($n1->safe_psql('postgres',
			'SELECT count(*) FROM (SELECT txn FROM public.kv GROUP BY txn HAVING count(*) <> 2) s'),
		'0', 'S2-01 stress: no committed transaction lost a participant');
}

# ---------------------------------------------------------------------------
# S2-02: the decision is durable before COMMIT PREPARED is sent.
# ---------------------------------------------------------------------------
{
	my $extra = "wal_writer_delay = 10s\nwal_writer_flush_after = 1GB\n";
	my ($n1, $n2) = make_cluster('sc', 2, extra => $extra);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.d (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 8);
	});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kb) = keys_of_group($n1, 1, 1, 1);
	my ($rc, $out, $err) = run_sql($n1,
		"SET synchronous_commit = off; BEGIN; INSERT INTO public.d VALUES ($ka, 'local'); "
		  . "INSERT INTO public.d VALUES ($kb, 'remote'); COMMIT;");
	is($err, '', 'a two-node commit with synchronous_commit = off');
	$n1->stop('immediate');
	$n1->start;
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.d WHERE id = $kb"), '1',
		'the participant committed');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.d WHERE id = $ka"), '1',
		'S2-02: the origin did not lose the transaction the participants committed');
}

done_testing();
