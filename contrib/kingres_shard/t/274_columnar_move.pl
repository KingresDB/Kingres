# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/274_columnar_move.pl: bucket moves of columnar relations (design/analytics-phase12-storage.md, 9.3 and 11.6, increment S3).
# The copy of a bucket partition is made with the access method of the source (USING columnar) and REPLICA IDENTITY FULL, the
# initial copy is the table-sync COPY of the stripes, the changes made during the move are the inserts that kingres_columnar 0.2
# decodes, and the bucket switches as a heap bucket does.  Cases: a group of a columnar and a heap table; inserts while the move
# runs; the move back; a user-partitioned table with a columnar and a heap leaf; the old copy and check_cluster after the cleanup.
# Needs kingres_columnar built and installed into the same installation as kingres_shard.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count keys_of_group start_psql finish_psql);

my ($n1, $n2) = make_cluster('cmv', 2, logical => 1, extra => "shared_preload_libraries = 'kingres_shard,kingres_columnar'\n");
sub qs { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }
$_->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar') for ($n1, $n2);

qs($n1, q{
	CREATE TABLE public.cc (id int PRIMARY KEY, k int, v text, n numeric, j jsonb, ts timestamptz, b bytea) USING columnar;
	INSERT INTO public.cc
	  SELECT g, g % 13, CASE WHEN g % 17 = 0 THEN NULL ELSE 'row ' || g END, g / 7.0, jsonb_build_object('g', g), '2026-01-01'::timestamptz + g * interval '1 minute',
	         CASE WHEN g % 5 = 0 THEN NULL ELSE decode(md5(g::text), 'hex') END
	    FROM generate_series(1, 3000) g;
	CREATE TABLE public.hh (id int PRIMARY KEY, v text);
	INSERT INTO public.hh SELECT g, 'h' || g FROM generate_series(1, 500) g;
	SELECT kingres.columnar_set_options('public.cc', compression => 'none', stripe_row_count => 30000, chunk_row_count => 7000);
	SELECT kingres.create_distributed_table('public.cc', 'id', bucket_count => 4);
	SELECT kingres.create_distributed_table('public.hh', 'id', colocate_with => 'public.cc');
});
my $C = qs($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.cc'::regclass");
sub group_of { my ($c, $b) = @_; return qs($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $c AND bucket = $b"); }
sub node_of { my ($g) = @_; return $g == 0 ? $n1 : $n2; }
sub digest { my ($t) = @_; return qs($n1, "SELECT count(*) || ':' || md5(string_agg(x::text, '|' ORDER BY id)) FROM public.$t x"); }
sub opts_of
{
	my ($n, $part) = @_;
	return qs($n, qq{SELECT o.compression || '/' || o.stripe_row_count || '/' || o.chunk_row_count FROM kingres.columnar_options('public.$part') o});
}
sub part_state
{
	my ($n, $part) = @_;
	return qs($n, qq{SELECT coalesce((SELECT a.amname || '/' || c.relreplident::text || '/' || c.relispartition::text FROM pg_class c JOIN pg_am a ON a.oid = c.relam
	                                   WHERE c.oid = to_regclass('public.$part')), 'absent')});
}

my $cc0 = digest('cc');
my $hh0 = digest('hh');
my $src = group_of($C, 1);
my $dst = 1 - $src;
is(part_state(node_of($src), 'cc_b0001'), 'columnar/f/true', 'before: the bucket partition is columnar with REPLICA IDENTITY FULL, on the source');
is(part_state(node_of($dst), 'cc_b0001'), 'absent', '... and absent on the target');

# the move of a group of a columnar and a heap table
my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, 1, $dst)");
is($rc, 0, 'move_bucket of a bucket of a columnar and a heap table') or diag($err);
is(group_of($C, 1), $dst, 'the bucket is on the other group');
is(part_state(node_of($dst), 'cc_b0001'), 'columnar/f/true', 'the copy is columnar, with REPLICA IDENTITY FULL, attached');
is(opts_of(node_of($dst), 'cc_b0001'), 'none/30000/7000', 'the copy has the options of the source partition (review P12S-03)');
is(part_state(node_of($dst), 'hh_b0001'), 'heap/d/true', 'the heap table of the group moved as a heap table');
is(digest('cc'), $cc0, 'the columnar table is whole: the same rows, byte for byte, from node 1');
is(digest('hh'), $hh0, 'the heap table is whole');
{
	my $n = node_of($dst);
	is(qs($n, "SELECT count(*) FROM public.cc_b0001") , qs($n1, "SELECT count(*) FROM public.cc WHERE kingres.bucket_for($C, id) = 1"), 'the copy holds every row of the bucket');
	is(qs($n, "SELECT count(*) FROM kingres.columnar_check()"), '0', 'columnar_check finds nothing wrong on the target');
}
is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for ($n1, $n2);
qs($n1, 'SELECT kingres.cleanup_moved_buckets(true)');
is(qs($n1, 'SELECT count(*) FROM kingres.check_cluster()'), '0', 'after the cleanup check_cluster has nothing to say');
is(qs($n1, "SELECT count(*) FROM pg_subscription") . ',' . qs($n2, "SELECT count(*) FROM pg_subscription") . ',' . qs($n1, "SELECT count(*) FROM pg_replication_slots") . ',' . qs($n1, "SELECT count(*) FROM pg_publication"), '0,0,0,0', 'no subscription, slot or publication is left');

# inserts while a bucket moves: every key of the moving bucket, one transaction each
{
	my $b = 2;
	my $from = group_of($C, $b);
	my @keys = split /,/, qs($n1, "SELECT string_agg(k::text, ',' ORDER BY k) FROM (SELECT k FROM generate_series(10000, 14000) k WHERE kingres.bucket_for($C, k) = $b LIMIT 300) s");
	my $list = join(',', @keys);
	my $w = start_psql($n1, "DO \$\$ DECLARE k int; BEGIN FOREACH k IN ARRAY ARRAY[$list] LOOP INSERT INTO public.cc VALUES (k, 1, 'w' || k, k, '{}', now(), NULL); PERFORM pg_sleep(0.02); END LOOP; END \$\$");
	my ($rc2, undef, $err2) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, " . (1 - $from) . ")");
	is($rc2, 0, 'move_bucket while a session inserts rows into the bucket') or diag($err2);
	my ($wo, $we) = finish_psql($w);
	diag("writer: $we") if $we;
	is(group_of($C, $b), 1 - $from, 'the bucket is on the other group');
	is(qs($n1, "SELECT count(*) FROM public.cc WHERE id >= 10000"), scalar(@keys), 'every inserted row is there, once');
	is(qs($n2, "SELECT count(*) FROM public.cc WHERE id >= 10000"), scalar(@keys), '... seen from the other node too');
	is(qs(node_of(1 - $from), "SELECT count(*) FROM public.cc_b000$b WHERE id >= 10000"), scalar(@keys), '... and held by the new owner of the bucket');
}
qs($n1, 'SELECT kingres.cleanup_moved_buckets(true)');

# and back again: the copy of a copy
my $cc1 = digest('cc');
my $now = group_of($C, 1);
($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, 1, " . (1 - $now) . ")");
is($rc, 0, 'the bucket moves back') or diag($err);
is(digest('cc'), $cc1, 'and the table is the same');
is(part_state(node_of(1 - $now), 'cc_b0001'), 'columnar/f/true', 'the partition on the node it came back to is columnar');
qs($n1, 'SELECT kingres.cleanup_moved_buckets(true)');

# a user-partitioned table with a columnar leaf and a heap leaf
qs($n1, q{
	CREATE TABLE public.up (id int, d date, v text, PRIMARY KEY (id, d)) PARTITION BY RANGE (d);
	CREATE TABLE public.up_old PARTITION OF public.up FOR VALUES FROM ('2020-01-01') TO ('2021-01-01') USING columnar;
	SELECT kingres.columnar_set_options('public.up_old', compression => 'none', stripe_row_count => 40000, chunk_row_count => 3000);
	CREATE TABLE public.up_new PARTITION OF public.up FOR VALUES FROM ('2021-01-01') TO ('2022-01-01');
	INSERT INTO public.up SELECT g, CASE WHEN g % 2 = 0 THEN '2020-06-01'::date ELSE '2021-06-01'::date END + (g % 200), 'u' || g FROM generate_series(1, 600) g;
	SELECT kingres.create_distributed_table('public.up', 'id', bucket_count => 5);
});
my $U = qs($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.up'::regclass");
my $up0 = digest('up');
my $ufrom = group_of($U, 0);
($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($U, 0, " . (1 - $ufrom) . ")");
is($rc, 0, 'move_bucket of a user-partitioned bucket with a columnar leaf') or diag($err);
is(group_of($U, 0), 1 - $ufrom, 'the bucket moved');
is(part_state(node_of(1 - $ufrom), 'up_old_b0000'), 'columnar/f/true', 'the columnar leaf arrived columnar');
is(opts_of(node_of(1 - $ufrom), 'up_old_b0000'), 'none/40000/3000', 'the columnar leaf has the options of its source leaf');
is(part_state(node_of(1 - $ufrom), 'up_new_b0000'), 'heap/d/true', 'the heap leaf arrived heap');
is(digest('up'), $up0, 'the user-partitioned table is whole');
qs($n1, 'SELECT kingres.cleanup_moved_buckets(true)');
is(qs($n1, 'SELECT count(*) FROM kingres.check_cluster()'), '0', 'check_cluster has nothing to say at the end');

is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for ($n1, $n2);
done_testing();
