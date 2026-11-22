# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/249_split_verify_types.pl: split_bucket(..., verify => 'full') on a table whose columns have no equality operator (review P12R-01).
#
# The verification writes a row of a child only when its values differ from the partition's; the comparison must not need an operator
# of the column types (json, xml, point, box have none) and must still see a difference.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @nodes = make_cluster('sv', 3, logical => 1, recovery_interval => 2,
	extra => "max_connections = 100\nkingres_shard.node_timeout = 5\nkingres_shard.move_cleanup_delay = 1\n");
my ($n1) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

# xml is a build option of PostgreSQL: the test uses it when it is there
my ($rc) = run_sql($n1, q{SELECT '<a/>'::xml});
my $xml = $rc == 0;
diag($xml ? 'xml is available' : 'this PostgreSQL has no xml: the table has json, point and box columns only');

q1($n1, "CREATE TABLE public.vt (id int PRIMARY KEY, j json, p point, b box, t text" . ($xml ? ", x xml" : '') . ")");
q1($n1, "INSERT INTO public.vt SELECT g, ('{\"k\": ' || g || '}')::json, point(g, g * 2), box(point(g, g), point(g + 1, g + 1)), 'v' || g" . ($xml ? ", ('<n>' || g || '</n>')::xml" : '') . " FROM generate_series(1, 2000) g");
q1($n1, q{SELECT kingres.create_distributed_table('public.vt', 'id', bucket_count => 8)});
my $C = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.vt'::regclass});
sub digest
{
	my ($n) = @_;
	return q1($n, q{SELECT count(*) || ',' || sum(id) || ',' || md5(string_agg(j::text || p::text || b::text || t, ',' ORDER BY id)) FROM public.vt});
}
my $base = digest($n1);

my ($owner) = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 0");
my ($rc2, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, 0, 2, 'full')");
is($rc2, 0, "split_bucket verify 'full' of a table with json, point, box" . ($xml ? ' and xml' : '') . ' columns') or diag($err);
unlike($err, qr/operator does not exist/, '... and no operator is missing');
q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
is(digest($_), $base, 'node ' . $_->name . ' reads the same rows after the split') for @nodes;
is(q1($n1, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $C AND modulus = 16"), 2, '... and the bucket is two buckets of modulus 16');
is(q1($n1, q{SELECT coalesce(string_agg(check_name, ','), '') FROM kingres.check_cluster()}), '', 'check_cluster() finds nothing');

# the float text of the comparison must not depend on the session that calls the function (review P12F-03)
is(q1($n1, q{SELECT p.proconfig::text FROM pg_proc p WHERE p.oid = (SELECT oid FROM pg_proc WHERE proname = 'internal_split_switch' AND pronamespace = 'kingres'::regnamespace)}), '{"search_path=pg_catalog, kingres, pg_temp",extra_float_digits=3}', 'internal_split_switch sets extra_float_digits = 3 itself');

done_testing();
