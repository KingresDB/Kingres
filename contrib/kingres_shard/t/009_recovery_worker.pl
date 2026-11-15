# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The background recovery worker (design/shard-phase2a.md, section 1,
# "Recovery"): the launcher starts a worker per database every
# kingres_shard.recovery_interval seconds, and the worker resolves the prepared
# transactions of the node without anyone calling the SQL function.  The
# crash windows are the ones of 008_twophase.pl.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster keys_of_group run_sql prepared_count
  wait_prepared_count);

# Node 1 recovers by itself every second.
my ($n1, $n2, $n3) = make_cluster('rw', 3, recovery_interval => 1);

$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
});
my %k;
$k{$_} = [ keys_of_group($n1, 1, $_, 12) ] for (0 .. 2);
my $next = 0;
sub fresh { return $k{ $_[0] }[$next] }
sub count_of
{
	return $n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $_[0]");
}

is($n1->safe_psql('postgres',
		"SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'kingres_shard recovery launcher'"),
	'1', 'the recovery launcher is running');

sub crash_scenario
{
	my ($point, @keys) = @_;
	my ($rc, $out, $err) = run_sql($n1,
		"SET kingres_shard.fault_injection = '$point:kill';\n"
		  . "BEGIN; INSERT INTO public.kv VALUES ($keys[0], 'a'); INSERT INTO public.kv VALUES ($keys[1], 'b'); INSERT INTO public.kv VALUES ($keys[2], 'c'); COMMIT;");
	isnt($rc, 0, "the origin dies at $point");
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
}

# Crash before the local commit: the worker rolls both back.
{
	my @keys = (fresh(0), fresh(1), fresh(2));
	crash_scenario('after_prepare', @keys);
	ok(wait_prepared_count($n2, 0), 'the worker resolved node 2 (rolled back)');
	ok(wait_prepared_count($n3, 0), 'the worker resolved node 3 (rolled back)');
	is(count_of($_), '0', "key $_ is not there") for @keys;
	$next++;
}

# Crash after the local commit: the worker commits both.
{
	my @keys = (fresh(0), fresh(1), fresh(2));
	crash_scenario('after_commit', @keys);
	ok(wait_prepared_count($n2, 0), 'the worker resolved node 2 (committed)');
	ok(wait_prepared_count($n3, 0), 'the worker resolved node 3 (committed)');
	is(count_of($_), '1', "key $_ is there") for @keys;
	ok($n1->poll_query_until('postgres', 'SELECT count(*) = 0 FROM kingres.dtx_log'),
		'the worker removed the logged decisions');
	$next++;
}

# A target node that was down comes back: the worker finishes the commit.
{
	my @keys = (fresh(0), fresh(1), fresh(2));
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_w_$$";
	append_to_file($gate, 'x');
	my $h = $n1->background_psql('postgres', on_error_stop => 0);
	$h->query_until(qr/started/,
		"SET kingres_shard.fault_injection = 'after_commit:wait:$gate';\n"
		  . "BEGIN; INSERT INTO public.kv VALUES ($keys[0], 'a'); INSERT INTO public.kv VALUES ($keys[1], 'b'); INSERT INTO public.kv VALUES ($keys[2], 'c');\n"
		  . "\\echo started\nCOMMIT;\n");
	ok(wait_prepared_count($n3, 1), 'prepared on node 3');
	$n3->stop('immediate');
	unlink $gate;
	$h->query('SELECT 1');
	$h->quit;
	sleep 3;					# several worker passes while node 3 is down
	is(count_of($keys[1]), '1', 'the part on node 2 is committed');
	$n3->start;
	ok(wait_prepared_count($n3, 0), 'the worker committed the transaction on node 3 after it came back');
	is(count_of($keys[2]), '1', 'the part on node 3 is there');
	$next++;
}

# The worker is controlled by the setting, which a reload changes.
{
	$n1->append_conf('postgresql.conf', "kingres_shard.recovery_interval = 0\n");
	$n1->reload;
	sleep 2;
	my @keys = (fresh(0), fresh(1), fresh(2));
	crash_scenario('after_prepare', @keys);
	sleep 4;
	is(prepared_count($n2), '1', 'with recovery_interval = 0 nothing is resolved by itself');
	$n1->append_conf('postgresql.conf', "kingres_shard.recovery_interval = 1\n");
	$n1->reload;
	ok(wait_prepared_count($n2, 0), 'after a reload with 1 second the worker resolves it');
	ok(wait_prepared_count($n3, 0), 'on node 3 too');
	$next++;
}

# A standby does not run recovery.
{
	my $p = PostgreSQL::Test::Cluster->new('rwp');
	$p->init(allows_streaming => 1);
	$p->append_conf('postgresql.conf',
		"shared_preload_libraries = 'kingres_shard'\nmax_prepared_transactions = 20\nkingres_shard.recovery_interval = 1\n");
	$p->start;
	$p->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
	$p->safe_psql('postgres',
		sprintf("SELECT kingres.init_cluster('%s', %d)", $p->host, $p->port));
	$p->backup('b');
	my $s = PostgreSQL::Test::Cluster->new('rws');
	$s->init_from_backup($p, 'b', has_streaming => 1);
	$s->append_conf('postgresql.conf', "hot_standby = on\n");
	$s->start;
	$p->wait_for_catchup($s);
	is($s->safe_psql('postgres', 'SELECT pg_is_in_recovery()'), 't', 'the second node is a standby');
	is($s->safe_psql('postgres',
			"SELECT count(*) FROM pg_stat_activity WHERE backend_type LIKE 'kingres_shard recovery%'"),
		'0', 'a standby runs no recovery launcher');
	my ($rc, $out, $err) = run_sql($s, 'SELECT kingres.recover_prepared_transactions()');
	like($err, qr/cannot run on a standby|read-only|permission denied/, 'the function is refused on a standby');
	is($p->safe_psql('postgres',
			"SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'kingres_shard recovery launcher'"),
		'1', 'the primary runs the launcher');
}

done_testing();
