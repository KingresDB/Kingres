# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/022_timeouts_and_worker.pl
#
# Review findings S2-11 and S2-12.
#
# S2-11: the recovery worker is immune to per-database settings that would make
# it fail or stall (default_transaction_isolation = repeatable read,
# lock_timeout, a read-only default).
#
# S2-12: a participant that stops answering COMMIT PREPARED is given up on
# after kingres_shard.node_timeout: the committing session returns, the
# transaction stays prepared and recovery commits it later; the other
# participants are not delayed.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time);
use KingresShardTest qw(make_cluster keys_of_group run_sql prepared_count wait_prepared_count);

# ---------------------------------------------------------------------------
# S2-11: settings stored for the database.
# ---------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('wk', 2, recovery_interval => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 8);
	});
	my ($ka) = keys_of_group($n1, 1, 0, 1);
	my ($kb) = keys_of_group($n1, 1, 1, 1);
	$n1->safe_psql('postgres', q{
		ALTER DATABASE postgres SET default_transaction_isolation = 'repeatable read';
		ALTER DATABASE postgres SET lock_timeout = '1ms';
		ALTER DATABASE postgres SET idle_in_transaction_session_timeout = '1ms';
	});
	my ($rc, $out, $err) = run_sql($n1,
		"SET kingres_shard.fault_injection = 'after_commit:kill';\n"
		  . "BEGIN ISOLATION LEVEL READ COMMITTED; INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kb, 'b'); COMMIT;");
	isnt($rc, 0, 'the origin dies after its local commit');
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	ok(wait_prepared_count($n2, 0), 'S2-11: the worker resolves the prepared transaction although the database has hostile settings');
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $kb"), '1', 'and committed it (the origin had committed)');
	$n1->safe_psql('postgres', q{
		ALTER DATABASE postgres RESET default_transaction_isolation;
		ALTER DATABASE postgres RESET lock_timeout;
		ALTER DATABASE postgres RESET idle_in_transaction_session_timeout;
	});
}

# ---------------------------------------------------------------------------
# S2-12: a participant that does not answer COMMIT PREPARED.
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('to', 3, extra => "kingres_shard.node_timeout = 3\n");
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	my ($k0) = keys_of_group($n1, 1, 0, 1);
	my ($k1) = keys_of_group($n1, 1, 1, 1);
	my ($k2) = keys_of_group($n1, 1, 2, 1);

	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_to_$$";
	append_to_file($gate, 'x');
	my $h = $n1->background_psql('postgres', on_error_stop => 0);
	$h->query_until(qr/started/,
		"SET kingres_shard.fault_injection = 'after_commit:wait:$gate';\n"
		  . "BEGIN; INSERT INTO public.kv VALUES ($k0, 'a'); INSERT INTO public.kv VALUES ($k1, 'b'); INSERT INTO public.kv VALUES ($k2, 'c');\n"
		  . "\\echo started\nCOMMIT;\n");
	ok(wait_prepared_count($n2, 1), 'prepared on node 2');
	ok(wait_prepared_count($n3, 1), 'prepared on node 3');

	# Node 2 stops answering: its backend that serves the origin is stopped.
	my @pids = split /\n/, $n2->safe_psql('postgres',
		q{SELECT pid FROM pg_stat_activity WHERE application_name = 'kingres_shard' AND pid <> pg_backend_pid()});
	is(scalar(@pids), 1, 'one backend of node 2 serves the origin');
	kill 'STOP', @pids;

	my $t0 = time;
	unlink $gate;
	$h->query('SELECT 1');
	my $elapsed = time - $t0;
	$h->quit;
	cmp_ok($elapsed, '<', 20, "S2-12: the committing session returned after $elapsed s although node 2 does not answer");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k0"), '1', 'the local part is committed');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k2"), '1',
		'S2-12: the participant after the silent one was committed (it was not delayed)')
	  or diag('prepared on node 3: ' . prepared_count($n3));
	is(prepared_count($n2), '1', 'the silent participant stays prepared');

	kill 'CONT', @pids;
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	ok(wait_prepared_count($n2, 0), 'once node 2 answers again the transaction is resolved (by the late COMMIT PREPARED or by recovery)');
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k1"), '1', 'node 2 has its row');
	is(prepared_count($_), '0', 'nothing is left prepared on ' . $_->name) for ($n1, $n2, $n3);
}

done_testing();
