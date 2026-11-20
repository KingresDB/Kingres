# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/138_no_gain_plans.pl
#
# The cost model of the fragment scan (step 6, review round, part C).  A closed query over ONE distributed table that
# returns about as many rows as the table gives after its restrictions (a grouping or a DISTINCT by a unique
# key) gains nothing from being run by the groups: the scan already ships only the restricted rows.  The
# measurement that led to the rule: a GROUP BY by the primary key of 3 million rows was 25 percent slower through the
# groups.  The fragment scan is therefore not offered for it, and is offered for every query that reduces the
# rows or joins two distributed tables.  Both directions are tested, from two nodes, with the answers compared
# with the scan's:
#
#   shipped  : grouping or DISTINCT by a key that repeats (every key four times, and twice), with a restriction,
#              a join of two distributed tables whatever it returns;
#   not shipped: grouping or DISTINCT by a unique key, with and without a restriction, with a primary key that
#              is the distribution key.
#
# The test fails for the mutant that drops the rule (the first group is then shipped) and for the one that
# always applies it (the second group is then not shipped).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @n = make_cluster('ng', 3);
my ($n1, $n2) = @n;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.w4 (id int, c int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.w4', 'id', bucket_count => 12);
	CREATE TABLE public.w2 (id int, c int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.w2', 'id', colocate_with => 'public.w4');
	CREATE TABLE public.u1 (id int PRIMARY KEY, v int);
	SELECT kingres.create_distributed_table('public.u1', 'id', colocate_with => 'public.w4');
	INSERT INTO public.w4 SELECT g, c FROM generate_series(1, 2000) g, generate_series(1, 4) c;
	INSERT INTO public.w2 SELECT g, c FROM generate_series(1, 2000) g, generate_series(1, 2) c;
	INSERT INTO public.u1 SELECT g, g % 7 FROM generate_series(1, 8000) g;
});
$_->safe_psql('postgres', 'ANALYZE') for @n;

my $FS = 'Kingres Fragment Scan';
my $DS = 'Kingres Distributed Scan';

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

sub sorted_rows
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . $sql);
	diag($err) if $rc != 0;
	return join("\n", sort split /\n/, $out);
}

my @shipped = (
	['a grouping by a key that repeats four times', q{SELECT id, count(*) FROM public.w4 GROUP BY id}],
	['a grouping by a key that repeats twice', q{SELECT id, count(*), max(c) FROM public.w2 GROUP BY id}],
	['a DISTINCT of a key that repeats', q{SELECT DISTINCT id FROM public.w4}],
	['a restricted grouping by a key that repeats', q{SELECT id, max(c) FROM public.w4 WHERE id < 500 GROUP BY id}],
	['a join of two distributed tables that returns a row per match', q{SELECT x.id, y.c FROM public.u1 x JOIN public.w2 y ON x.id = y.id WHERE x.id < 300}],
	['a join with a grouping', q{SELECT x.id, count(*) FROM public.w4 x JOIN public.w2 y ON x.id = y.id GROUP BY x.id}],
	['a self join of a table with a unique key (two references, so the rule of one table does not apply)', q{SELECT x.id, y.v FROM public.u1 x JOIN public.u1 y ON y.id = x.id WHERE x.id < 300}],
);
my @scan = (
	['a grouping by the unique key', q{SELECT id, count(*) FROM public.u1 GROUP BY id}],
	['a DISTINCT of the unique key', q{SELECT DISTINCT id FROM public.u1}],
	['a restricted grouping by the unique key', q{SELECT id, max(v) FROM public.u1 WHERE id < 500 GROUP BY id}],
	['a restricted DISTINCT of the unique key', q{SELECT DISTINCT id, v FROM public.u1 WHERE v = 3}],
);

for my $node ($n1, $n2)
{
	my $nm = $node->name;
	for my $t (@shipped)
	{
		my ($what, $sql) = @$t;
		like(plan_of($node, $sql), qr/\Q$FS\E/, "$nm: $what is run by the groups");
		is(sorted_rows($node, $sql), sorted_rows($node, $sql, 'SET kingres_shard.enable_pushdown = off; '), "$nm: ... and the answer is the scan's");
	}
	for my $t (@scan)
	{
		my ($what, $sql) = @$t;
		my $p = plan_of($node, $sql);
		unlike($p, qr/\Q$FS\E/, "$nm: $what is not run by the groups (nothing is gained)");
		like($p, qr/\Q$DS\E/, "$nm: ... the scan answers") or diag((run_sql($node, "SET client_min_messages = debug1; EXPLAIN (COSTS OFF) $sql"))[2]);
		is(sorted_rows($node, $sql), sorted_rows($node, $sql, 'SET kingres_shard.enable_pushdown = off; '), "$nm: ... and the answer is the same");
	}
}

# a query that is not shipped for no gain says so, and the switch that forces nothing is not needed: the reason is the plan's
done_testing();
