# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/175_distribute_hostile_columns.pl
#
# Review P2F-07 (mutant M11 survived): t/159 distributes its tables empty, so the column list of the copy that create_distributed_table
# makes of a table with rows (copy_routed.c) was never given a name that needs quoting.  A populated table whose columns are named
# with quotes, a blank, a reserved word, a comment and a semicolon is distributed, and the cluster holds the single node's rows.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql prepared_count);
use KingresShardCompare;

my $c = KingresShardCompare->new('dh', extra => "kingres_shard.enable_partial_aggregation = on\n");
my ($n1, $n2, $n3) = $c->nodes;

my $create = q{CREATE TABLE public."H""t s" (id int PRIMARY KEY, "a b" text, "c;--" int, "d*/" text, "select" int, "x""y" text, "tail" text)};
$c->on_both($create);
my $ins = q{INSERT INTO public."H""t s" SELECT g, 'it''s ' || g, g % 7, E'back\\slash ' || (g % 5), g, 'q"' || g, NULL FROM generate_series(1, 400) g};
$c->on_both($ins);
$c->on_cluster(q{SELECT kingres.create_distributed_table('public."H""t s"', 'id', bucket_count => 12)});
$c->on_every_node('ANALYZE');
$c->on_single('ANALYZE');
is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.dist_table WHERE relid = 'public."H""t s"'::regclass}), 1, 'the populated table is distributed');
for my $nd ($n1, $n2, $n3)
{
	is($nd->safe_psql('postgres', q{SELECT count(*) FROM public."H""t s"}), 400, $nd->name . ': it holds its 400 rows');
}
$c->compare_tables('public."H""t s"', name => 'the rows of the table after the distribution', order_by => 'id');
$c->compare('a scan of the hostile columns', q{SELECT id, "a b", "c;--", "d*/", "select", "x""y", "tail" FROM public."H""t s" WHERE id % 37 = 0 ORDER BY id}, ordered => 1, plans => ['pushed', 'scan'], profiles => 0);
$c->compare('an aggregate over the hostile columns', q{SELECT "c;--", count(*), max("a b"), min("x""y") FROM public."H""t s" GROUP BY "c;--" ORDER BY 1}, ordered => 1, plans => ['pushed', 'scan'], profiles => 0);
is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for ($n1, $n2, $n3);

done_testing();
