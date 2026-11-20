# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/196_storage_parameters.pl: the storage parameters of a table (fillfactor, autovacuum_*, ...) are carried (design/shard-phase9.md, increment 9.9).
# A partitioned table cannot have them, so a distributed table has them on its bucket partitions and kingres.dist_table.reloptions records them:
# every table that replaces it, and every copy that a bucket move makes, has them again.
#
#   A. create_distributed_table (blocking and online): the bucket partitions have them, the catalog of every node records them
#   B. alter (blocking and online): the new bucket partitions have them
#   C. undistribute (blocking and online): the ordinary table has them; reference tables keep them and carry them
#   D. a bucket move: the copy on the target has them
#   E. a table without storage parameters is as before; the partitions of a user-partitioned table still refuse them

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('sp', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

my $OPTS = '{fillfactor=70,autovacuum_enabled=false,autovacuum_vacuum_scale_factor=0.05}';
my $WITH = 'WITH (fillfactor = 70, autovacuum_enabled = false, autovacuum_vacuum_scale_factor = 0.05)';

sub new_table
{
	my ($name, $with) = @_;
	$n1->safe_psql('postgres', qq{
		CREATE TABLE public.$name (id int PRIMARY KEY, customer int NOT NULL, note text) } . ($with // '') . qq{;
		INSERT INTO public.$name SELECT g, g % 11, 'n' || g FROM generate_series(1, 300) g;
	});
}
# the options of every bucket partition on every node: one distinct value, or the empty string
sub bucket_opts
{
	my ($name) = @_;
	return join('|', map { q1($_, "SELECT coalesce(string_agg(DISTINCT coalesce(reloptions::text, '-'), ','), '') FROM pg_class WHERE relname ~ '^${name}_b[0-9]{4}\$' AND relkind = 'r'") } @nodes);
}
sub bucket_count_on_nodes { my ($name) = @_; my $s = 0; $s += q1($_, "SELECT count(*) FROM pg_class WHERE relname ~ '^${name}_b[0-9]{4}\$' AND relkind = 'r'") for @nodes; return $s; }
sub recorded { my ($name) = @_; return join('|', map { q1($_, "SELECT coalesce((SELECT reloptions::text FROM kingres.dist_table WHERE relid = 'public.$name'::regclass), 'none')") } @nodes); }
sub rows { my ($name) = @_; return q1($n1, "SELECT count(*) || ',' || sum(id) FROM public.$name"); }
sub own_opts { my ($n, $name) = @_; return q1($n, "SELECT coalesce((SELECT reloptions::text FROM pg_class WHERE oid = to_regclass('public.$name')), '-')"); }

# ---------------------------------------------------------------------------
# A. create
# ---------------------------------------------------------------------------
new_table('s1', $WITH);
my $want = rows('s1');
my $rep = q1($n1, q{SELECT string_agg(severity || ':' || code, ',') FROM kingres.table_change_report('distribute', 'public.s1', '{"column": "id"}')});
is($rep, 'note:storage_parameters', 'A: the report notes the storage parameters, it does not refuse them');
my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 6)});
is($rc, 0, 'A: create_distributed_table of a table with storage parameters') or diag($err);
is(bucket_count_on_nodes('s1'), '6', 'A: six bucket partitions');
is(bucket_opts('s1'), join('|', ($OPTS) x 3), 'A: every bucket partition has them, on every node');
is(recorded('s1'), join('|', ($OPTS) x 3), 'A: the catalog of every node records them');
is(rows('s1'), $want, 'A: and every row is there');
new_table('s2', $WITH);
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table_concurrently('public.s2', 'id', bucket_count => 6)});
is($rc, 0, 'A: create_distributed_table_concurrently') or diag($err);
is(bucket_opts('s2'), join('|', ($OPTS) x 3), 'A: online: every bucket partition has them');
is(recorded('s2'), join('|', ($OPTS) x 3), 'A: online: recorded on every node');
# a second table of the same group, made later, from a table without parameters: nothing
new_table('s3');
run_sql($n1, q{SELECT kingres.create_distributed_table('public.s3', 'id', colocate_with => 'public.s1')});
is(bucket_opts('s3'), join('|', ('-') x 3), 'A: a table without storage parameters has none');
is(recorded('s3'), join('|', ('none') x 3), 'A: and records none');

# ---------------------------------------------------------------------------
# B. alter
# ---------------------------------------------------------------------------
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table('public.s1', bucket_count => 12)});
is($rc, 0, 'B: alter_distributed_table') or diag($err);
is(bucket_count_on_nodes('s1'), '12', 'B: twelve bucket partitions');
is(bucket_opts('s1'), join('|', ($OPTS) x 3), 'B: the new bucket partitions have them');
is(recorded('s1'), join('|', ($OPTS) x 3), 'B: still recorded');
is(rows('s1'), $want, 'B: every row is there');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table_concurrently('public.s2', bucket_count => 12)});
is($rc, 0, 'B: alter_distributed_table_concurrently') or diag($err);
is(bucket_count_on_nodes('s2'), '12', 'B: online: twelve bucket partitions');
is(bucket_opts('s2'), join('|', ($OPTS) x 3), 'B: online: the new bucket partitions have them');
is(recorded('s2'), join('|', ($OPTS) x 3), 'B: online: still recorded');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'B: check_cluster() finds nothing');

# ---------------------------------------------------------------------------
# D. a bucket move (before the tables are undistributed)
# ---------------------------------------------------------------------------
{
	my $coloc = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.s1'::regclass});
	my $bucket = q1($n1, "SELECT kingres.bucket_for($coloc, 1)");
	my $source = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $coloc AND bucket = $bucket");
	my $target = ($source + 1) % 3;
	my $part = sprintf('s1_b%04d', $bucket);
	my $id = q1($n1, "SELECT kingres.move_bucket($coloc, $bucket, $target)");
	like($id, qr/^\d+$/, "D: the bucket $bucket moves from group $source to group $target");
	is(q1($nodes[$target], "SELECT reloptions::text FROM pg_class WHERE oid = 'public.$part'::regclass"), $OPTS, 'D: the copy on the target has the storage parameters');
	is(q1($nodes[$target], "SELECT relispartition::text FROM pg_class WHERE oid = 'public.$part'::regclass"), 'true', 'D: and is attached');
	is(bucket_opts('s1'), join('|', ($OPTS) x 3), 'D: every bucket partition on every node still has them');
	is(rows('s1'), $want, 'D: every row is there');
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'D: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# C. undistribute, and a reference table
# ---------------------------------------------------------------------------
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.undistribute_table('public.s1')});
is($rc, 0, 'C: undistribute_table') or diag($err);
is(own_opts($n1, 's1'), $OPTS, 'C: the ordinary table has the storage parameters');
is(rows('s1'), $want, 'C: every row is there');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.undistribute_table_concurrently('public.s2')});
is($rc, 0, 'C: undistribute_table_concurrently') or diag($err);
is(own_opts($n1, 's2'), $OPTS, 'C: online: the ordinary table has them');
is(rows('s2'), $want, 'C: online: every row is there');
new_table('r1', $WITH);
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_reference_table('public.r1')});
is($rc, 0, 'C: create_reference_table of a table with storage parameters') or diag($err);
is(join('|', map { own_opts($_, 'r1') } @nodes), join('|', ($OPTS) x 3), 'C: every copy of the reference table has them');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.undistribute_table('public.r1')});
is($rc, 0, 'C: undistribute of the reference table') or diag($err);
is(own_opts($n1, 'r1'), $OPTS, 'C: the table has them');
new_table('r2', $WITH);
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table_concurrently('public.r2', 'id', bucket_count => 3)});
is($rc, 0, 'C: a distributed table, again') or diag($err);
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table('public.r2', distribution_column => 'id', bucket_count => 3, colocate_with => 'none')});
$n1->safe_psql('postgres', q{SELECT kingres.cleanup_table_operations(true)});
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'C: check_cluster() finds nothing');

# ---------------------------------------------------------------------------
# E. the partitions of a user-partitioned table
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.up (k int NOT NULL, d date NOT NULL, PRIMARY KEY (k, d)) PARTITION BY RANGE (d);
	CREATE TABLE public.up_a PARTITION OF public.up FOR VALUES FROM ('2026-01-01') TO ('2027-01-01') WITH (fillfactor = 70);
});
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.up', 'k')});
isnt($rc, 0, 'E: a partition of a user-partitioned table with a storage parameter is still refused'); like($err, qr/of its own/, 'E: as one that has something of its own');
done_testing();
