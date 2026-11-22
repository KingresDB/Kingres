# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/271_columnar_refusals.pl: what a columnar distributed table refuses (design/analytics-phase12-storage.md, 9.3, 9.7, increment S2).
#   * the online variants of the table lifecycle (they capture changes with a row trigger, which columnar does not support) are
#     refused by the report, before anything is made;
#   * a bucket move that would carry a columnar relation is refused in "preparing", whether the relation is a bucket partition, a table
#     of the colocation group that is not the first, or a leaf of a user-partitioned table; nothing is left behind;
#   * a split of a columnar table is refused with the words of the design;
#   * a heap group moves as before.
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

# a bucket move refuses a columnar bucket partition, in preparing
my $C = qs($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.cc'::regclass");
my $before = qs($n1, "SELECT string_agg(bucket || ':' || group_id, ',' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = $C");
my $src = bucket_group($C, 1);
my $dst = 1 - $src;
($st, $msg) = state_of("kingres.move_bucket($C, 1, $dst)");
is($st, '0A000', 'move_bucket of a columnar bucket: feature_not_supported');
like($msg, qr/^bucket move refused: relation "public\.cc_b0001" has columnar storage/, '... with the words of the design and the name of the partition');
is(qs($n1, "SELECT string_agg(bucket || ':' || group_id, ',' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = $C"), $before, 'the bucket map is as it was');
is(leftovers($n1), '0,0,0,0', 'nothing is left on the source (subscriptions, slots, publications, copy tables)');
is(leftovers($n2), '0,0,0,0', 'nor on the target');
is(qs($n1, "SELECT count(*) FROM kingres.bucket_move WHERE phase NOT IN ('done', 'failed', 'cancelled')") , '0', 'and no move is in progress');

# a heap table colocated with it: the group is refused as a whole, naming the columnar table
qs($n1, q{
	CREATE TABLE public.hh (id int PRIMARY KEY, v text);
	INSERT INTO public.hh SELECT g, 'h' || g FROM generate_series(1, 100) g;
});
qs($n1, "SELECT kingres.create_distributed_table('public.hh', 'id', colocate_with => 'public.cc')");
($st, $msg) = state_of("kingres.move_bucket($C, 1, $dst)");
like($msg, qr/relation "public\.cc_b0001" has columnar storage/, 'a group of a columnar and a heap table is refused, naming the columnar table');

# a heap-only group moves as before
qs($n1, q{
	CREATE TABLE public.hz (id int PRIMARY KEY, v text);
	INSERT INTO public.hz SELECT g, 'z' || g FROM generate_series(1, 200) g;
});
qs($n1, "SELECT kingres.create_distributed_table('public.hz', 'id', bucket_count => 6)");
my $H = qs($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.hz'::regclass");
isnt($H, $C, 'the heap table is in a group of its own');
my $hsrc = bucket_group($H, 2);
my ($rc, undef, $err) = run_sql($n1, "SELECT kingres.move_bucket($H, 2, " . (1 - $hsrc) . ")");
is($rc, 0, 'a heap group still moves') or diag($err);
is(bucket_group($H, 2), 1 - $hsrc, '... and the bucket is on the other group');
is(qs($n2, 'SELECT count(*) FROM public.hz'), '200', '... with every row');

# a leaf of a user-partitioned table that is columnar
qs($n1, q{
	CREATE TABLE public.up (id int, d date, v text, PRIMARY KEY (id, d)) PARTITION BY RANGE (d);
	CREATE TABLE public.up_old PARTITION OF public.up FOR VALUES FROM ('2020-01-01') TO ('2021-01-01') USING columnar;
	CREATE TABLE public.up_new PARTITION OF public.up FOR VALUES FROM ('2021-01-01') TO ('2022-01-01');
	INSERT INTO public.up SELECT g, '2020-06-01'::date + (g % 300), 'u' || g FROM generate_series(1, 300) g;
});
qs($n1, "SELECT kingres.create_distributed_table('public.up', 'id', bucket_count => 5)");
my $U = qs($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.up'::regclass");
my $lo = leftovers($n1) . ';' . leftovers($n2);
($st, $msg) = state_of("kingres.move_bucket($U, 0, " . (1 - bucket_group($U, 0)) . ")");
is($st, '0A000', 'a user-partitioned bucket with a columnar leaf is refused');
like($msg, qr/relation "public\.up_old_b0000" has columnar storage/, '... naming the leaf');
is(leftovers($n1) . ';' . leftovers($n2), $lo, 'nothing is left behind (what the move of the heap group left, the copy that waits for its cleanup, is all there is)');

# a split of a columnar table is refused with the words of the design
($st, $msg) = state_of("kingres.split_bucket($C, 1, 2)");
is($st, '0A000', 'split_bucket of a columnar table: feature_not_supported');
like($msg, qr/^bucket split refused: table "public\.cc" is columnar; a split needs a primary key, row locks and update triggers/, '... with the words of the design');

is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for ($n1, $n2);
done_testing();
