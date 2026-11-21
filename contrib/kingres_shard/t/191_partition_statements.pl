# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/191_partition_statements.pl: the statements that name a user partition of a distributed table are turned into the partition functions
# (design/shard-phase9.md, increment 9.5, feature 4): CREATE TABLE ... PARTITION OF, ALTER TABLE ... ATTACH / DETACH PARTITION, DROP TABLE.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('ps', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

$n1->safe_psql('postgres', q{
	CREATE TABLE public.s1 (customer int NOT NULL, created date NOT NULL, id int NOT NULL, qty int, PRIMARY KEY (customer, created, id)) PARTITION BY RANGE (created);
	CREATE TABLE public.s1_p1 PARTITION OF public.s1 FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
	CREATE TABLE public.s1_p2 PARTITION OF public.s1 FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
	INSERT INTO public.s1 SELECT g % 9, date '2026-01-01' + (g % 55), g, g % 30 FROM generate_series(1, 300) g;
	SELECT kingres.create_distributed_table('public.s1', 'customer', bucket_count => 4);
	CREATE TABLE public.sl (tenant int NOT NULL, region text NOT NULL, id int NOT NULL, PRIMARY KEY (tenant, region, id)) PARTITION BY LIST (region);
	CREATE TABLE public.sl_a PARTITION OF public.sl FOR VALUES IN ('a');
	SELECT kingres.create_distributed_table('public.sl', 'tenant', bucket_count => 4);
});
sub count_in { my ($t, $w) = @_; return q1($n1, "SELECT count(*) FROM public.$t WHERE $w"); }
sub leaf_total { my ($p) = @_; my $s = 0; $s += q1($_, "SELECT count(*) FROM pg_class WHERE relname ~ '^${p}_b[0-9]{4}\$' AND relkind = 'r'") for @nodes; return $s; }

# CREATE TABLE ... PARTITION OF
my ($rc, $out, $err) = run_sql($n1, q{CREATE TABLE public.s1_p3 PARTITION OF public.s1 FOR VALUES FROM ('2026-03-01') TO ('2026-04-01')});
is($rc, 0, 'CREATE TABLE ... PARTITION OF is turned into create_partition()') or diag($err);
is(leaf_total('s1_p3'), '4', 'four leaves');
is(q1($_, q{SELECT bound FROM kingres.dist_partition WHERE partition_name = 's1_p3'}), q{FROM ('2026-03-01') TO ('2026-04-01')}, 'the bound is in the catalog of ' . $_->name) for @nodes;
($rc, $out, $err) = run_sql($n1, q{CREATE TABLE public.s1_p4 PARTITION OF public.s1 FOR VALUES FROM ('2026-04-01'::date) TO (MAXVALUE)});
is($rc, 0, 'a cast and MAXVALUE are written back') or diag($err);
is(q1($n1, q{SELECT bound FROM kingres.dist_partition WHERE partition_name = 's1_p4'}), q{FROM ('2026-04-01'::date) TO (MAXVALUE)}, 'as they were written');
($rc, $out, $err) = run_sql($n1, q{CREATE TABLE public.sl_b PARTITION OF public.sl FOR VALUES IN ('b', 'c')});
is($rc, 0, 'a LIST partition') or diag($err);
is(q1($n1, q{SELECT bound FROM kingres.dist_partition WHERE partition_name = 'sl_b'}), q{IN ('b', 'c')}, 'with its values');
($rc, $out, $err) = run_sql($n1, q{CREATE TABLE public.sl_def PARTITION OF public.sl DEFAULT});
is($rc, 0, 'a DEFAULT partition') or diag($err);
is(q1($n1, q{SELECT bound FROM kingres.dist_partition WHERE partition_name = 'sl_def'}), 'DEFAULT', 'recorded as DEFAULT');
($rc, $out, $err) = run_sql($n2, q{INSERT INTO public.s1 VALUES (1, '2026-03-05', 70001, 1), (2, '2027-01-01', 70002, 2); INSERT INTO public.sl VALUES (1, 'b', 1), (2, 'zz', 2)});
is($rc, 0, 'writes from another node reach the new partitions') or diag($err);
for my $bad (
	['a partition with sub-partitioning', q{CREATE TABLE public.s1_x PARTITION OF public.s1 FOR VALUES FROM ('2030-01-01') TO ('2031-01-01') PARTITION BY HASH (id)}, 'with its bound only'],
	['a partition with a storage parameter', q{CREATE TABLE public.s1_y PARTITION OF public.s1 FOR VALUES FROM ('2030-01-01') TO ('2031-01-01') WITH (fillfactor = 70)}, 'with its bound only'],
	['a bound with an expression', q{CREATE TABLE public.s1_z PARTITION OF public.s1 FOR VALUES FROM (now()) TO ('2031-01-01')}, 'not supported in the statement'],
	['a partition of a bound that overlaps', q{CREATE TABLE public.s1_w PARTITION OF public.s1 FOR VALUES FROM ('2026-03-10') TO ('2026-03-20')}, 'overlap'],
	)
{
	($rc, $out, $err) = run_sql($n1, $bad->[1]);
	isnt($rc, 0, "$bad->[0] is refused"); like($err, qr/$bad->[2]/, "$bad->[0]: the message") or diag($err);
}
($rc, $out, $err) = run_sql($n2, q{CREATE TABLE public.s1_v PARTITION OF public.s1 FOR VALUES FROM ('2031-01-01') TO ('2032-01-01')});
isnt($rc, 0, 'another node than the map group primary refuses');
is(leaf_total('s1_x') + leaf_total('s1_y') + leaf_total('s1_z') + leaf_total('s1_w') + leaf_total('s1_v'), '0', 'the refusals left no leaf');

# the partitions are not tables
($rc, $out, $err) = run_sql($n1, q{SELECT count(*) FROM public.s1_p3});
isnt($rc, 0, 'a partition cannot be queried by its name'); like($err, qr/does not exist/, 'it does not exist');

# DETACH and ATTACH
my $p2 = count_in('s1', q{created >= '2026-02-01' AND created < '2026-03-01'});
($rc, $out, $err) = run_sql($n1, q{ALTER TABLE public.s1 DETACH PARTITION public.s1_p2});
is($rc, 0, 'ALTER TABLE ... DETACH PARTITION is turned into detach_partition()') or diag($err);
is(q1($n1, q{SELECT count(*) FROM public.s1_p2}), $p2, 'the detached partition is a table with its rows');
($rc, $out, $err) = run_sql($n1, q{ALTER TABLE public.s1 ATTACH PARTITION public.s1_p2 FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')});
is($rc, 0, 'ALTER TABLE ... ATTACH PARTITION is turned into attach_partition()') or diag($err);
is(count_in('s1', q{created >= '2026-02-01' AND created < '2026-03-01'}), $p2, 'the rows are in the table again');
($rc, $out, $err) = run_sql($n1, q{ALTER TABLE public.s1 DETACH PARTITION public.s1_p1 CONCURRENTLY});
isnt($rc, 0, 'DETACH ... CONCURRENTLY is refused');
$n1->safe_psql('postgres', q{CREATE TABLE public.plain (id int)});
($rc, $out, $err) = run_sql($n1, q{ALTER TABLE public.s1 ATTACH PARTITION public.plain FOR VALUES FROM ('2040-01-01') TO ('2041-01-01')});
isnt($rc, 0, 'ATTACH of an ordinary table is refused'); like($err, qr/ATTACH PARTITION of a table that is not a distributed table/, 'with the statement\'s own message');

($rc, $out, $err) = run_sql($n1, q{DROP TABLE public.s1_p4 CASCADE});
isnt($rc, 0, 'DROP ... CASCADE of a partition is left to PostgreSQL, which refuses it');
is(q1($n1, q{SELECT count(*) FROM kingres.dist_partition WHERE partition_name = 's1_p4'}), '1', 'and the partition is still there');
# DROP TABLE
my $cnt = count_in('s1', 'true') - count_in('s1', q{created >= '2026-03-01' AND created < '2026-04-01'});
($rc, $out, $err) = run_sql($n1, q{DROP TABLE public.s1_p3});
is($rc, 0, 'DROP TABLE of a partition is turned into drop_partition()') or diag($err);
is(leaf_total('s1_p3'), '0', 'its leaves are gone');
is(count_in('s1', 'true'), $cnt, 'with its rows');
($rc, $out, $err) = run_sql($n1, q{DROP TABLE IF EXISTS public.s1_nothing});
is($rc, 0, 'DROP TABLE IF EXISTS of a name that is nothing is harmless') or diag($err);
($rc, $out, $err) = run_sql($n1, q{DROP TABLE public.s1_nothing});
isnt($rc, 0, 'DROP TABLE of a name that is nothing fails as PostgreSQL does');
($rc, $out, $err) = run_sql($n1, q{DROP TABLE public.s1_p4, public.sl_b});
isnt($rc, 0, 'a DROP of several tables that names partitions is left to PostgreSQL, which refuses it');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'check_cluster() finds nothing');
is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for @nodes;
done_testing();
