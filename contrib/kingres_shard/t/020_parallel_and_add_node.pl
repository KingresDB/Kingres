# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/020_parallel_and_add_node.pl
#
# Review findings S2-08 and S2-07.
#
# S2-08: a distributed scan or a routed statement never runs inside a parallel
# worker.  A PARALLEL SAFE function that reads a distributed table is refused
# in the worker with a clear error (it would use a connection and a snapshot
# of its own and miss the writes of the leader's transaction); a PARALLEL
# RESTRICTED one runs in the leader and gives the serial answer.
#
# S2-07: add_node() while reference tables are being written: the writers wait
# for the registration and no write is lost on the new node.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster make_node run_sql prepared_count wait_prepared_count);

# ---------------------------------------------------------------------------
# S2-08
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('pw', 3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.t2 (id int PRIMARY KEY, a int);
		SELECT kingres.create_distributed_table('public.t2', 'id', bucket_count => 12);
	});
	my $fn = q{
		CREATE FUNCTION public.fpar(x int) RETURNS bigint LANGUAGE plpgsql PARALLEL SAFE
		AS $$ BEGIN RETURN (SELECT count(*) FROM public.t2 WHERE a >= x); END $$;
		CREATE FUNCTION public.frestr(x int) RETURNS bigint LANGUAGE plpgsql PARALLEL RESTRICTED
		AS $$ BEGIN RETURN (SELECT count(*) FROM public.t2 WHERE a >= x); END $$;
	};
	$_->safe_psql('postgres', $fn) for ($n1, $n2, $n3);
	$n1->safe_psql('postgres',
		join('', map { "INSERT INTO public.t2 VALUES ($_, $_ % 5);\n" } (1 .. 30)));

	my $force = q{
		SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0;
		SET min_parallel_table_scan_size = 0; SET max_parallel_workers_per_gather = 3;
		SET debug_parallel_query = on;
	};
	my $tx = "BEGIN;\nINSERT INTO public.t2 VALUES (1001, 0);\nINSERT INTO public.t2 VALUES (1002, 0);\nINSERT INTO public.t2 VALUES (1003, 0);\n";

	my ($rc, $out, $err) = run_sql($n1, $tx . q{SELECT sum(public.frestr(0)) FROM generate_series(1, 20) g; ROLLBACK;});
	is($out, '660', 'serial answer of the PARALLEL RESTRICTED function inside the transaction (33 rows x 20)');

	($rc, $out, $err) = run_sql($n1, $force . $tx . q{SELECT sum(public.frestr(0)) FROM generate_series(1, 20) g; ROLLBACK;});
	is($err, '', 'a PARALLEL RESTRICTED function under forced parallel query runs');
	is($out, '660', 'S2-08: and gives the serial answer (it ran in the leader)');

	($rc, $out, $err) = run_sql($n1, $force . $tx . q{SELECT sum(public.fpar(0)) FROM generate_series(1, 20) g; ROLLBACK;});
	isnt($rc, 0, 'S2-08: a PARALLEL SAFE function that reads a distributed table is refused in a worker');
	like($err, qr/parallel worker/, 'S2-08: with an error that names the cause');
	like($err, qr/PARALLEL RESTRICTED/, 'S2-08: and tells the user what to do');

	# Plain statements are never planned with a parallel plan under the extension.
	($rc, $out, $err) = run_sql($n1, $force . q{SELECT count(*) FROM public.t2});
	is($out, '30', 'a distributed scan under forced parallel settings');
	($rc, $out, $err) = run_sql($n1, $force . q{EXPLAIN (COSTS OFF) SELECT count(*) FROM public.t2 WHERE a = 1});
	unlike($out, qr/Gather/, 'the plan of a statement on a distributed table has no Gather');
}

# ---------------------------------------------------------------------------
# S2-07
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('an', 3);
	my $n4 = make_node('an4');
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.r (id int PRIMARY KEY, v int);
		INSERT INTO public.r VALUES (1, 1);
		SELECT kingres.create_reference_table('public.r');
	});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');

	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_an_$$";
	append_to_file($gate, 'x');
	my $adder = $n1->background_psql('postgres', on_error_stop => 0);
	$adder->query_until(qr/started/,
		"SET kingres_shard.fault_injection = 'after_prepare:wait:$gate';\n\\echo started\n"
		  . sprintf("SELECT kingres.add_node('%s', %d);\n", $n4->host, $n4->port));
	ok(wait_prepared_count($n4, 1), 'add_node has prepared the new node and waits');

	# A write on node 2 while the node is being added: it waits for the registration.
	my $writer = $n2->background_psql('postgres', on_error_stop => 0);
	$writer->query_until(qr/started/, "\\echo started\nINSERT INTO public.r VALUES (888, 8);\n");
	select(undef, undef, undef, 1.0);
	is($n3->safe_psql('postgres', 'SELECT count(*) FROM public.r WHERE id = 888'), '0',
		'the write waits (it is not visible while add_node is in progress)');
	unlink $gate;
	$adder->query('SELECT 1');
	$adder->quit;
	my ($wout) = $writer->query('SELECT 1');
	my $werr = $writer->{stderr};
	$writer->quit;

	# Either the write was refused (it was planned before the node existed: retry),
	# or it reached every node including the new one: never a write that misses the new node.
	my @counts = map { $_->safe_psql('postgres', 'SELECT count(*) FROM public.r WHERE id = 888') } ($n1, $n2, $n3, $n4);
	is(join(',', @counts), '0,0,0,0', 'S2-07: the write planned with the old node list did not commit anywhere')
	  if $werr =~ /ERROR/;
	is(join(',', @counts), '1,1,1,1', 'S2-07: the write reached the new node too')
	  unless $werr =~ /ERROR/;
	like($werr, qr/set of nodes changed/i, 'a refused write says why') if $werr =~ /ERROR/;
	is($n4->safe_psql('postgres', 'SELECT count(*) FROM public.r'), $n1->safe_psql('postgres', 'SELECT count(*) FROM public.r'),
		'the new node has the same rows of r as node 1');

	# After the registration, writes go to four nodes.
	$n3->safe_psql('postgres', 'INSERT INTO public.r VALUES (889, 9)');
	is($n4->safe_psql('postgres', 'SELECT count(*) FROM public.r WHERE id = 889'), '1', 'a later write reaches the new node');
	is(prepared_count($_), '0', "nothing is left prepared on " . $_->name) for ($n1, $n2, $n3, $n4);
}

done_testing();
