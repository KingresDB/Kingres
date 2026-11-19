# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/143_distribute_insel.pl: the colocated INSERT ... SELECT of step 5 of phase 2b and the blocking
# create_distributed_table() of phase 9 (increment 9.2) meet in two places.
#
#   A. a prepared (cached, generic) INSERT ... SELECT made while its source is an ordinary table is
#      executed after that table was swapped for a distributed one: the plan is made again (the swap
#      invalidates it) and ships the statement whole, or, if the invalidation was missed, the second
#      defence of step 5 (the signature of the relations of the plan) raises the retryable error and
#      the repeat works; in no case is a row lost or stored twice.
#   B. a colocated INSERT ... SELECT into a table that is being distributed waits for the EXCLUSIVE lock
#      like any writer (readers of the source go on), and after the commit it goes on against the new
#      table: its rows are in the new table, in the buckets of their keys.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_waiter start_psql finish_psql wait_for_fault_file);

my ($n1, $n2, $n3) = make_cluster('di', 3);
my @nodes = ($n1, $n2, $n3);
my $KS = qr/KS001|bucket map|map changed|no longer what it was|could not open relation|does not exist/;

$n1->safe_psql('postgres', q{
	CREATE TABLE public.seed (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.seed', 'id', bucket_count => 12);
	INSERT INTO public.seed SELECT g, 's' || g FROM generate_series(1, 200) g;
	CREATE TABLE public.sink (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.sink', 'id', colocate_with => 'public.seed');
	CREATE TABLE public.srcp (id int PRIMARY KEY, v text);
	INSERT INTO public.srcp SELECT g, 'p' || g FROM generate_series(1, 120) g;
});

# ---------------------------------------------------------------------------
# A. a cached plan across the swap of its source
# ---------------------------------------------------------------------------
{
	my $s = $n1->background_psql('postgres', on_error_stop => 0);
	$s->query_safe(q{PREPARE pa AS INSERT INTO public.sink SELECT id, v FROM public.srcp ON CONFLICT DO NOTHING});
	$s->query_safe('EXECUTE pa') for (1 .. 6);
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.sink'), '120', 'before the swap the statement copies the rows of the ordinary table');
	$s->query_safe('DELETE FROM public.sink');
	$s->query_safe('SET plan_cache_mode = force_generic_plan');
	$s->query_safe('EXECUTE pa');
	$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.srcp', 'id', colocate_with => 'public.sink')");
	$n1->safe_psql('postgres', 'DELETE FROM public.sink');
	$s->{stderr} = '';
	$s->query('EXECUTE pa');
	my $e = $s->{stderr};
	$s->{stderr} = '';
	if ($e =~ /ERROR/)
	{
		like($e, $KS, 'the plan made before the swap fails with a retryable error');
		$s->query('EXECUTE pa');
		is($s->{stderr}, '', '... and works when repeated');
	}
	else
	{
		pass('the plan was made again after the swap and ran');
	}
	for my $n (@nodes)
	{
		is($n->safe_psql('postgres', q{SELECT count(*) || ',' || sum(id) FROM public.sink}), '120,7260', 'the sink holds the 120 rows once, seen from ' . $n->name);
	}
	my $same = $n1->safe_psql('postgres', q{SELECT (SELECT count(*) FROM (SELECT id, v FROM public.sink EXCEPT SELECT id, v FROM public.srcp) a)
	                                         + (SELECT count(*) FROM (SELECT id, v FROM public.srcp EXCEPT SELECT id, v FROM public.sink) b)});
	is($same, '0', 'and they are the rows of the source, which is distributed now');
	like($n1->safe_psql('postgres', q{EXPLAIN (COSTS OFF) INSERT INTO public.sink SELECT id, v FROM public.srcp}), qr/colocated INSERT|Pushed|Fragment|Kingres/i,
		'the statement is planned as a distributed one now');
	$s->quit;
}

# ---------------------------------------------------------------------------
# B. a colocated INSERT ... SELECT into the table that is being distributed
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.tgt (id int PRIMARY KEY, v text);
	INSERT INTO public.tgt SELECT g, 't' || g FROM generate_series(1, 10) g;
});
{
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_insel_$$";
	append_to_file($gate, 'x');
	my $creator = start_psql($n1,
		"SET kingres_shard.fault_injection = 'distribute_after_shadow:wait:$gate'; "
		  . "SELECT kingres.create_distributed_table('public.tgt', 'id', colocate_with => 'public.seed');");
	ok(wait_for_fault_file("$gate.reached", 60), 'the creation of the target is held after its shadow');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.seed'), '200', 'the distributed source is read meanwhile');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.tgt'), '10', 'and the table that is being distributed is read');
	my $writer = start_psql($n1, "INSERT INTO public.tgt SELECT id + 1000, v FROM public.seed;");
	ok(wait_waiter($n1, 1), 'the INSERT ... SELECT into the table waits like any writer');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.tgt'), '10', 'its rows are not there yet');
	unlink $gate;
	my ($out, $err) = finish_psql($creator);
	is($err, '', 'the creation succeeded');
	($out, $err) = finish_psql($writer);
	is($err, '', 'the statement that waited went on against the new table') or diag($err);
	for my $n (@nodes)
	{
		is($n->safe_psql('postgres', 'SELECT count(*) FROM public.tgt'), '210', 'the target has its 10 rows and the 200 inserted, seen from ' . $n->name);
	}
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.tgt t WHERE t.id > 1000 AND NOT EXISTS (SELECT 1 FROM public.seed s WHERE s.id = t.id - 1000)"), '0',
		'every inserted row has its source row');
	is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for @nodes;
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', 'check_cluster() is clean');
}

done_testing();
