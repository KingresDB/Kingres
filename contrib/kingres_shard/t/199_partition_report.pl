# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/199_partition_report.pl: table_change_report for the four partition operations.
#
#   A. a valid operation on a user-partitioned distributed table has no blocker (the report once refused every one of them with
#      not_available_yet, which stopped the control plane before the call was made)
#   B. an invalid one is still refused: a table that is not distributed, a name that leaves no room for _bNNNN
#   C. what the report allows, the call does

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster);

my ($n1, $n2, $n3) = make_cluster('pr', 3, logical => 1);

$n1->safe_psql('postgres', q{
	CREATE TABLE public.ev (customer int NOT NULL, created date NOT NULL, id int NOT NULL, PRIMARY KEY (customer, created, id)) PARTITION BY RANGE (created);
	CREATE TABLE public.ev_a PARTITION OF public.ev FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
	INSERT INTO public.ev SELECT g % 7, date '2026-01-01' + (g % 28), g FROM generate_series(1, 100) g;
	SELECT kingres.create_distributed_table('public.ev', 'customer', bucket_count => 6);
	CREATE TABLE public.plain (id int);
});

sub report
{
	my ($kind, $rel, $opts) = @_;
	return $n1->safe_psql('postgres',
		qq{SELECT coalesce(string_agg(code, ',' ORDER BY code), '') FROM kingres.table_change_report('$kind', '$rel'::regclass, '$opts'::jsonb) WHERE severity = 'blocker'});
}

# A
for my $kind (qw(create_partition attach_partition detach_partition drop_partition))
{
	is(report($kind, 'public.ev', '{"partition":"ev_b"}'), '', "$kind: no blocker for a valid operation");
}

# B
$n1->safe_psql('postgres', q{CREATE TABLE public.hashed (id int PRIMARY KEY); SELECT kingres.create_distributed_table('public.hashed', 'id', bucket_count => 6)});
is(report('create_partition', 'public.hashed', '{"partition":"p"}'), 'not_distributed', 'a distributed table that is not user-partitioned is refused');
is(report('create_partition', 'public.plain', '{"partition":"p"}'), 'not_distributed', 'a table that is not distributed is refused');
is(report('create_partition', 'public.ev', '{"partition":"' . ('x' x 60) . '"}'), 'name_too_long', 'a name without room for the leaf suffix is refused');

# C
$n1->safe_psql('postgres', q{SELECT kingres.create_partition('public.ev', 'ev_b', 'FROM (''2026-02-01'') TO (''2026-03-01'')')});
is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.dist_partition WHERE parent = 'public.ev'::regclass}), '2', 'the call that the report allowed works');

done_testing();
