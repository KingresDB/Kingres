# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/271_columnar_refusals.pl: what a columnar distributed table refuses (design/analytics-phase12-storage.md, 9.3, 9.7, increments S2 and S3).
#   * the online variants of the table lifecycle (they capture changes with a row trigger, which columnar does not support) are
#     refused by the report, before anything is made;
#   * a bucket move that would carry a columnar relation is refused in "preparing" when the installed kingres_columnar (0.1) cannot decode
#     its changes, as a group, with nothing left behind, and works after the upgrade to 0.2;
#   * a split of a columnar table is refused with the words of the design;
# Needs kingres_columnar built and installed into the same installation as kingres_shard.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2) = make_cluster('cr', 2, logical => 1, extra => "shared_preload_libraries = 'kingres_shard,kingres_columnar'\n");
sub qs { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }
$_->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar') for ($n1, $n2);

# the SQLSTATE and the message of a statement that fails
sub state_of
{
	my ($sql) = @_;
	my ($rc, undef, $err) = run_sql($n1, "\\set VERBOSITY verbose\nSELECT $sql");
	return ($err =~ /ERROR:\s+(\w{5}):\s+([^\n]*)/) ? ($1, $2) : ('none', $err);
}
sub leftovers
{
	my ($n) = @_;
	return qs($n, q{SELECT (SELECT count(*) FROM pg_subscription) || ',' || (SELECT count(*) FROM pg_replication_slots) || ',' || (SELECT count(*) FROM pg_publication)
	                || ',' || (SELECT count(*) FROM pg_class WHERE obj_description(oid, 'pg_class') LIKE 'kingres_shard bucket move%')});
}
sub bucket_group { my ($c, $b) = @_; return qs($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $c AND bucket = $b"); }

qs($n1, q{
	CREATE TABLE public.cc (id int PRIMARY KEY, v text) USING columnar;
	INSERT INTO public.cc SELECT g, 'c' || g FROM generate_series(1, 400) g;
});

# the online variants of the lifecycle are refused by the report
my $codes = qs($n1, q{SELECT coalesce(string_agg(severity || ':' || code, ',' ORDER BY code), '') FROM kingres.table_change_report('distribute', 'public.cc', '{"column":"id","mode":"online"}')});
like($codes, qr/blocker:columnar_online/, 'the report: the online distribution of a columnar table is a blocker');
my ($st, $msg) = state_of(q{kingres.create_distributed_table_concurrently('public.cc', 'id', bucket_count => 8)});
is($st, '0A000', 'create_distributed_table_concurrently refuses with feature_not_supported');
like($msg, qr/online variants capture changes with a row trigger.*"columnar".*blocking variant/s, '... and says to use the blocking variant');
is(qs($n1, 'SELECT count(*) FROM kingres.table_operation') . ',' . qs($n1, 'SELECT count(*) FROM kingres.dist_table'), '0,0', 'no operation was started and nothing was distributed');
qs($n1, "SELECT kingres.create_distributed_table('public.cc', 'id', bucket_count => 8)");
for my $call (q{kingres.alter_distributed_table_concurrently('public.cc', bucket_count => 4)}, q{kingres.undistribute_table_concurrently('public.cc')})
{
	($st, $msg) = state_of($call);
	is($st, '0A000', (split /\(/, $call)[0] . ' refuses');
	like($msg, qr/"columnar".*blocking variant/s, '... naming the blocking variant');
}
is(qs($n1, 'SELECT count(*) FROM kingres.table_operation'), '0', 'no operation is left');
qs($n1, "SELECT kingres.alter_distributed_table('public.cc', bucket_count => 4)");
is(qs($n2, 'SELECT count(*) || \',\' || sum(id) FROM public.cc'), '400,80200', 'the blocking variant works: the table is whole');

# a split of a columnar table is refused with the words of the design
my $C = qs($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.cc'::regclass");
($st, $msg) = state_of("kingres.split_bucket($C, 1, 2)");
is($st, '0A000', 'split_bucket of a columnar table: feature_not_supported');
like($msg, qr/^bucket split refused: table "public\.cc" is columnar; a split needs a primary key, row locks and update triggers/, '... with the words of the design');

# every relation of a bucket is looked at, not the first: a leaf of another access method than columnar stops the move even when a
# columnar leaf sorts before it (review P12S-05).  A second table access method over the heap handler stands for it.
$_->safe_psql('postgres', 'CREATE ACCESS METHOD heap2 TYPE TABLE HANDLER heap_tableam_handler') for ($n1, $n2);
qs($n1, q{
	CREATE TABLE public.up2 (id int, d date, v text, PRIMARY KEY (id, d)) PARTITION BY RANGE (d);
	CREATE TABLE public.up2_a PARTITION OF public.up2 FOR VALUES FROM ('2020-01-01') TO ('2021-01-01') USING columnar;
	CREATE TABLE public.up2_b PARTITION OF public.up2 FOR VALUES FROM ('2021-01-01') TO ('2022-01-01') USING heap2;
	INSERT INTO public.up2 SELECT g, CASE WHEN g % 2 = 0 THEN '2020-06-01'::date ELSE '2021-06-01'::date END, 'u' || g FROM generate_series(1, 100) g;
	SELECT kingres.create_distributed_table('public.up2', 'id', bucket_count => 3);
});
{
	my $U2 = qs($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.up2'::regclass");
	my $from2 = qs($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $U2 AND bucket = 0");
	my ($st2, $msg2) = state_of("kingres.move_bucket($U2, 0, " . (1 - $from2) . ")");
	is($st2, '0A000', 'a bucket with a columnar leaf and a leaf of another access method is refused');
	like($msg2, qr/^bucket move refused: relation "public\.up2_b_b0000" uses the access method "heap2", which a bucket move does not carry$/, '... naming the leaf of the other access method');
}

# a bucket move refuses a columnar relation when the installed kingres_columnar cannot decode its changes (version 0.1), in
# preparing, before anything is made.  (With kingres_columnar 0.2 the move works: t/274.)
my ($m1, $m2) = make_cluster('co', 2, logical => 1, extra => "shared_preload_libraries = 'kingres_shard,kingres_columnar'\n");
$_->safe_psql('postgres', q{CREATE EXTENSION kingres_columnar VERSION '0.1'}) for ($m1, $m2);
is(qs($m1, q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_columnar'}), '0.1', 'the second cluster has kingres_columnar 0.1');
qs($m1, q{
	CREATE TABLE public.cc (id int, v text) USING columnar;
	INSERT INTO public.cc SELECT g, 'c' || g FROM generate_series(1, 400) g;
	SELECT kingres.create_distributed_table('public.cc', 'id', bucket_count => 4);
	CREATE TABLE public.hh (id int PRIMARY KEY, v text);
	INSERT INTO public.hh SELECT g, 'h' || g FROM generate_series(1, 100) g;
	SELECT kingres.create_distributed_table('public.hh', 'id', colocate_with => 'public.cc');
});
sub state_m
{
	my ($sql) = @_;
	my (undef, undef, $err) = run_sql($m1, "\\set VERBOSITY verbose\nSELECT $sql");
	return ($err =~ /ERROR:\s+(\w{5}):\s+([^\n]*)\n(.*)/s) ? ($1, $2, $3) : ('none', $err, '');
}
sub leftovers_m
{
	my ($n) = @_;
	return qs($n, q{SELECT (SELECT count(*) FROM pg_subscription) || ',' || (SELECT count(*) FROM pg_replication_slots) || ',' || (SELECT count(*) FROM pg_publication)
	                || ',' || (SELECT count(*) FROM pg_class WHERE obj_description(oid, 'pg_class') LIKE 'kingres_shard bucket move%')});
}
my $MC = qs($m1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.cc'::regclass");
my $before = qs($m1, "SELECT string_agg(bucket || ':' || group_id, ',' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = $MC");
my $from = qs($m1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $MC AND bucket = 1");
my ($mst, $mmsg, $mrest) = state_m("kingres.move_bucket($MC, 1, " . (1 - $from) . ")");
is($mst, '0A000', 'move_bucket with kingres_columnar 0.1: feature_not_supported');
like($mmsg, qr/^bucket move refused: relation "public\.cc_b0001" has columnar storage, and the installed kingres_columnar cannot decode its changes$/, '... with the words of the design and the name of the partition');
like($mrest, qr/HINT:\s+Move buckets of this table after kingres_columnar is upgraded to a version that supports logical decoding \(0\.2\), or convert the table to row storage/, '... and its hint');
is(qs($m1, "SELECT string_agg(bucket || ':' || group_id, ',' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = $MC"), $before, 'the bucket map is as it was');
is(leftovers_m($m1) . ';' . leftovers_m($m2), '0,0,0,0;0,0,0,0', 'nothing is left behind (subscriptions, slots, publications, copy tables)');
is(qs($m1, "SELECT count(*) FROM kingres.bucket_move WHERE phase NOT IN ('done', 'failed', 'cancelled')"), '0', 'and no move is in progress');
# a heap table of the group does not move alone: the group is refused as a whole, with the name of the columnar relation
($mst, $mmsg) = state_m("kingres.move_bucket($MC, 1, " . (1 - $from) . ")");
like($mmsg, qr/relation "public\.cc_b0001" has columnar storage/, 'a group of a columnar and a heap table is refused as a whole');
# upgraded, the same call works
qs($_, q{ALTER EXTENSION kingres_columnar UPDATE TO '0.2'}) for ($m1, $m2);
my ($rc, undef, $err) = run_sql($m1, "SELECT kingres.move_bucket($MC, 1, " . (1 - $from) . ")");
is($rc, 0, 'after ALTER EXTENSION kingres_columnar UPDATE the bucket moves') or diag($err);

is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for ($n1, $n2, $m1, $m2);
done_testing();
