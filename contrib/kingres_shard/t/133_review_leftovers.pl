# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/133_review_leftovers.pl
#
# What the review of steps 3 and 4 left (P2C-07, P2C-01) and the step 5 path against type modifiers:
#   A. char(n): an over-length value fails as on a single node, trailing blanks are fine;
#   B. COPY into a table with row-level security (turned on by hand with routing off) is refused;
#   C. the colocated INSERT ... SELECT (step 5, shipped whole) with keys whose type has a modifier
#      (timestamp(0), numeric(8,2), varchar(8)) gives the twin's rows; a source key of a wider modifier
#      than the target's is compared with the twin too (the group stores the rounded value in its own partition).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('rl');
my ($n1, $n2, $n3) = $c->nodes;
my $single = $c->single;
my @nodes = ($n1, $n2, $n3);

sub table
{
	my ($create, $dist) = @_;
	$c->on_cluster($create . ($dist // ''));
	$c->on_single($create);
}

# A. char(n)
table(q{CREATE TABLE public.ch (k char(5), v text)}, q{; SELECT kingres.create_distributed_table('public.ch', 'k', bucket_count => 12)});
$c->compare_dml('char(5): values with blanks', q{INSERT INTO public.ch VALUES ('ab', 'a'), ('abc  ', 'b'), ('abcde', 'c'), ('x', 'd')});
$c->compare_dml('char(5): too long', q{INSERT INTO public.ch VALUES ('abc', 'a'), ('abcdefgh', 'b'), ('xy', 'c')},
	expect_error => qr/value too long for type character\(5\)/, single_fails => 1);
$c->compare_dml('char(5): too long through a select', q{INSERT INTO public.ch SELECT 'toolong' || g, 'z' FROM generate_series(1, 5) g},
	expect_error => qr/value too long for type character\(5\)/, single_fails => 1);
$c->compare_tables('public.ch', order_by => '1, 2', name => 'char(5): the rows');

# B. COPY and row-level security (the check of COPY FROM in the router)
table(q{CREATE TABLE public.rls_t (id int PRIMARY KEY, v text)}, q{; SELECT kingres.create_distributed_table('public.rls_t', 'id', bucket_count => 12)});
$_->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; ALTER TABLE public.rls_t ENABLE ROW LEVEL SECURITY}) for @nodes;
my ($cc) = KingresShardCompare::copy_in($n1, 'public.rls_t', "1\ta\n2\tb\n3\tc\n4\td\n", columns => ['id', 'v']);
isnt($cc, 0, 'B: COPY into a table with row-level security is refused');
my (undef, undef, $cerr) = KingresShardCompare::copy_in($n1, 'public.rls_t', "5\ta\n", columns => ['id', 'v']);
like($cerr, qr/COPY FROM not supported with row-level security/, 'B: with the message of PostgreSQL');
$_->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; ALTER TABLE public.rls_t DISABLE ROW LEVEL SECURITY}) for @nodes;

# C. the colocated INSERT ... SELECT with keys that have a modifier
my @types = ([ 'ts0', 'timestamp(0)', q{timestamp '2024-03-10 10:00:00' + (g || ' sec')::interval + interval '0.7 sec'} ],
	[ 'n82', 'numeric(8,2)', q{g + 0.005} ], [ 'vc8', 'varchar(8)', q{'k' || g} ]);
for my $ty (@types)
{
	my ($name, $type, $gen) = @$ty;
	table("CREATE TABLE public.sa_$name (k $type PRIMARY KEY, v int)", "; SELECT kingres.create_distributed_table('public.sa_$name', 'k', bucket_count => 12)");
	table("CREATE TABLE public.sb_$name (k $type PRIMARY KEY, v int)", "; SELECT kingres.create_distributed_table('public.sb_$name', 'k', colocate_with => 'public.sa_$name')");
	$c->compare_dml("$name: fill the source", "INSERT INTO public.sa_$name SELECT $gen, g FROM generate_series(1, 40) g");
	my $stmt = "INSERT INTO public.sb_$name SELECT k, v FROM public.sa_$name";
	my ($rc, $plan) = run_sql($n1, "EXPLAIN (COSTS OFF) $stmt");
	like($plan, qr/Fan-out Write/, "$name: the statement is shipped whole (step 5)");
	$c->compare_dml("$name: INSERT SELECT", $stmt);
	$c->compare_tables("public.sb_$name", order_by => '1', name => "$name: the rows of the target");
	$c->compare_dml("$name: INSERT SELECT with a join", "INSERT INTO public.sb_$name SELECT a.k, a.v + 1 FROM public.sa_$name a JOIN public.sa_$name b2 ON b2.k = a.k ON CONFLICT DO NOTHING");
	$c->compare_tables("public.sb_$name", order_by => '1', name => "$name: the rows after the join");
}
# a source key with more digits than the target's: the stored value of the target is rounded
{
	my $ok = eval {
		table(q{CREATE TABLE public.wa (k numeric(10,4) PRIMARY KEY, v int)}, q{; SELECT kingres.create_distributed_table('public.wa', 'k', bucket_count => 12)});
		table(q{CREATE TABLE public.wb (k numeric(8,2) PRIMARY KEY, v int)}, q{; SELECT kingres.create_distributed_table('public.wb', 'k', colocate_with => 'public.wa')});
		1;
	};
	SKIP:
	{
		skip 'the two modifiers are not colocated', 3 unless $ok;
		$c->compare_dml('wider source: fill', q{INSERT INTO public.wa SELECT g + 0.0049, g FROM generate_series(1, 30) g});
		$c->compare_dml('wider source: INSERT SELECT rounds the key like a single node', q{INSERT INTO public.wb SELECT k, v FROM public.wa});
		$c->compare_tables('public.wb', order_by => '1', name => 'wider source: the rows of the target');
	}
}

done_testing();
