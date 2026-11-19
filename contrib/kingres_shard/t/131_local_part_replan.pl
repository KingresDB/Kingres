# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/131_local_part_replan.pl
#
# The one-shot mark of the local part (fanout.c) is consumed by the first planning of the statement that SPI
# plans.  If SPI planned the statement a second time before it ran (an invalidated plan), the second plan would be
# the plan of a statement of this session and would act on the other groups too.  Result of the investigation:
# no path of SPI does that for these users: SPI_execute_with_args makes a one-shot plan (planned once, in
# _SPI_execute_plan, with no revalidation) and SPI_cursor_open_with_args builds the plan once (GetCachedPlan
# builds a new plan once; validity is checked only when a plan is reused), and a concurrent ANALYZE that commits
# between the planning and the start does not change that (the test holds the statement in that gap with the fault
# point local_part_planned and runs it, and the planner hook was seen to run once per statement).  As a defence
# in depth a second planning of the armed text before the execution starts is refused with KS001 (fanout.c,
# kingres_shard_local_part_take; ExecutorStart closes the window, so a function that replays the text during the
# execution is still planned and routed, t/132).  This test is the regression gate of the gap.
#
#   A. a fan-out UPDATE, B. a fan-out DELETE, C. the local batch of a router INSERT, D. the local part of a
#   router INSERT ... SELECT, E. the local cursor of a closed query (the fragment scan refuses a plan that holds a scan).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql);

my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my @nodes = make_cluster('lr', 3);
my ($n1, $n2, $n3) = @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.t (id int PRIMARY KEY, b int);
	SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 12);
	INSERT INTO public.t SELECT g, 0 FROM generate_series(1, 120) g;
});
$_->safe_psql('postgres', 'ANALYZE') for @nodes;

# the sum of b and the number of rows, read from every group directly
sub direct
{
	my ($sql) = @_;
	my $sum = 0;
	$sum += $_->safe_psql('postgres', "SET kingres_shard.enable_routing = off; $sql") for @nodes;
	return $sum;
}

# run $stmt on node 1 held at local_part_planned; ANALYZE the table in another session; release
my $gn = 0;
sub replanned
{
	my ($stmt) = @_;
	my $gate = "$tmp/gate_lr_" . $gn++;
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, "SET kingres_shard.fault_injection = 'local_part_planned:wait:$gate'; $stmt");
	ok(wait_for_fault_file("$gate.reached", 60), "the statement waits after the planning of its local part");
	$n1->safe_psql('postgres', 'ANALYZE public.t');
	unlink $gate;
	return finish_psql($sess);
}

# A. a fan-out UPDATE
my ($out, $err) = replanned('UPDATE public.t SET b = b + 1');
is(direct('SELECT coalesce(sum(b), 0) FROM public.t'), 120, 'A: after an invalidation between the planning and the start, the UPDATE added 1 to every row once') or diag("$out\n$err");
is(direct('SELECT count(*) FROM public.t WHERE b <> 1'), 0, 'A: no row was updated twice or left alone');

# B. a fan-out DELETE
($out, $err) = replanned('DELETE FROM public.t WHERE id > 100');
is(direct('SELECT count(*) FROM public.t'), 100, 'B: the DELETE removed exactly the rows it names') or diag("$out\n$err");

# C. the local batch of a router insert
($out, $err) = replanned("INSERT INTO public.t SELECT g, 5 FROM generate_series(200, 260) g");
is(direct('SELECT count(*) FROM public.t WHERE id >= 200'), 61, 'C: the router inserted every row once') or diag("$out\n$err");
is(direct('SELECT count(*) FROM public.t WHERE id >= 200 AND b = 5'), 61, 'C: with its values');

# D. a multi-row VALUES
($out, $err) = replanned("INSERT INTO public.t VALUES (300, 7), (301, 7), (302, 7), (303, 7), (304, 7), (305, 7)");
is(direct('SELECT count(*) FROM public.t WHERE id >= 300'), 6, 'D: a multi-row INSERT stored every row once') or diag("$out\n$err");

# E. the closed query: its local cursor
$n1->safe_psql('postgres', 'ANALYZE');
($out, $err) = replanned('SELECT id, count(*) FROM public.t GROUP BY id');
my @rows = grep { /^\d+\|/ } split /\n/, $out;
ok(scalar(@rows) == 100 + 61 + 6 && scalar(keys %{{ map { (split /\|/)[0] => 1 } @rows }}) == scalar(@rows),
	'E: a closed query whose local plan was invalidated returns every row once, or fails') or diag("$out\n$err");

is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;
done_testing();
