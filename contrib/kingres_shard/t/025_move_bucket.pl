# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Moving a bucket (design/shard-phase4.md, "The move"): the happy path.  A bucket
# with rows in two colocated tables that have a foreign key between them moves
# from one shard group to another; every row is returned exactly once from every
# node before, during and after, the partitions are attached on the target, the
# source keeps orphan tables until the cleanup, and nothing of the replication is
# left behind.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql move_state);

my @nodes = make_cluster('mv', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;

$n1->safe_psql('postgres', q{
	CREATE TABLE public.orders (id int PRIMARY KEY, customer int NOT NULL, note text);
	CREATE TABLE public.items (id int NOT NULL, order_id int NOT NULL, qty int NOT NULL,
	                           PRIMARY KEY (id, order_id));
	SELECT kingres.create_distributed_table('public.orders', 'id', bucket_count => 6);
	SELECT kingres.create_distributed_table('public.items', 'order_id', colocate_with => 'public.orders');
	ALTER TABLE public.items ADD CONSTRAINT items_order_fk FOREIGN KEY (order_id) REFERENCES public.orders (id);
});

# 60 orders and 120 items, written one row at a time through node 1 (a
# statement of several buckets is not supported yet).
{
	my $sql = '';
	$sql .= "INSERT INTO public.orders VALUES ($_, $_ % 7, 'order $_');\n" for 1 .. 60;
	$sql .= "INSERT INTO public.items VALUES ($_, ($_ - 1) % 60 + 1, $_ % 5 + 1);\n" for 1 .. 120;
	$n1->safe_psql('postgres', $sql);
}

sub totals
{
	my ($node) = @_;
	return $node->safe_psql('postgres', q{
		SELECT (SELECT count(*) FROM public.orders) || ',' || (SELECT sum(id) FROM public.orders)
		       || ',' || (SELECT count(*) FROM public.items) || ',' || (SELECT sum(id) FROM public.items)
		       || ',' || (SELECT count(*) FROM public.orders o JOIN public.items i ON i.order_id = o.id)});
}

sub same_everywhere
{
	my ($label, $want) = @_;
	is(totals($_), $want, "$label: orders and items are exactly once from " . $_->name) for @nodes;
}

my $want = '60,1830,120,7260,120';
same_everywhere('before the move', $want);

# The bucket of order 1 and the shard group it is on; any other group is the target.
my $bucket = $n1->safe_psql('postgres', 'SELECT kingres.bucket_for(1, 1)');
my $source = $n1->safe_psql('postgres',
	"SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket");
my $target = ($source + 1) % 3;
my $src_node = $nodes[$source];
my $tgt_node = $nodes[$target];
my $rows_in_bucket = $n1->safe_psql('postgres', qq{
	SELECT count(*) FROM public.orders WHERE kingres.bucket_for(1, id) = $bucket});
cmp_ok($rows_in_bucket, '>', 0, "bucket $bucket (group $source) holds orders");
my $part = sprintf('orders_b%04d', $bucket);
my $part2 = sprintf('items_b%04d', $bucket);

sub partition_state
{
	my ($node, $name) = @_;
	return $node->safe_psql('postgres',
		"SELECT coalesce((SELECT relispartition::text FROM pg_class WHERE oid = to_regclass('public.$name')), 'absent')");
}

is(partition_state($src_node, $part), 'true', 'the partition is attached on the source');
is(partition_state($tgt_node, $part), 'absent', 'the target has no such table');

my @versions_before = map { $_->safe_psql('postgres', 'SELECT version FROM kingres.map_version') } @nodes;

# Only the map group primary moves buckets, and only a superuser.
{
	my ($rc, $out, $err) = run_sql($n2, "SELECT kingres.move_bucket(1, $bucket, $target)");
	like($err, qr/map group primary|permission denied/, 'a move is refused on another node');
	$n1->safe_psql('postgres', 'CREATE ROLE mover LOGIN');
	($rc, $out, $err) = run_sql($n1, "SET ROLE mover; SELECT kingres.move_bucket(1, $bucket, $target)");
	like($err, qr/permission denied/, 'a move is refused to an ordinary role');
	$n1->safe_psql('postgres', 'DROP ROLE mover');
}

# The move.
my $id = $n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $bucket, $target)");
like($id, qr/^\d+$/, "move_bucket returned the id of the move ($id)");
like(move_state($n1, $id), qr/^cleaning\|/, 'the move is in the cleaning phase');
is($n1->safe_psql('postgres', "SELECT source_group || '>' || target_group || ' bucket ' || bucket FROM kingres.bucket_move WHERE move_id = $id"),
	"$source>$target bucket $bucket", 'the record names the groups and the bucket');
is($n1->safe_psql('postgres', "SELECT replication_dropped FROM kingres.bucket_move WHERE move_id = $id"), 't',
	'the replication was dropped');

# The map on every node.
for my $node (@nodes)
{
	is($node->safe_psql('postgres',
			"SELECT group_id || '/' || state FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket"),
		"$target/active", "node " . $node->name . " has the bucket on group $target");
}
my @versions_after = map { $_->safe_psql('postgres', 'SELECT version FROM kingres.map_version') } @nodes;
for my $i (0 .. 2)
{
	cmp_ok($versions_after[$i], '>', $versions_before[$i], "the map version rose on node " . ($i + 1));
}
is($versions_after[1], $versions_after[0], 'node 2 has the version of the map group');
is($versions_after[2], $versions_after[0], 'node 3 has the version of the map group');

# The partitions.
is(partition_state($tgt_node, $part), 'true', 'orders: the partition is attached on the target');
is(partition_state($tgt_node, $part2), 'true', 'items: the partition is attached on the target');
is(partition_state($src_node, $part), 'absent', 'orders: the partition is no longer on the source');
is(partition_state($src_node, $part2), 'absent', 'items: the partition is no longer on the source');
my $orphans = $src_node->safe_psql('postgres',
	"SELECT string_agg(relname, ',' ORDER BY relname) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan\\_$id\\_%'");
is($orphans, "orphan_${id}_1,orphan_${id}_2", 'the source keeps an orphan table for each table');
is($src_node->safe_psql('postgres', "SELECT count(*) FROM pg_constraint WHERE contype = 'f' AND conrelid IN
		(SELECT oid FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan\\_%')"), '0',
	'the orphan tables keep no foreign key');

same_everywhere('after the move', $want);

# Nothing of the replication is left on any node.
for my $node (@nodes)
{
	my $n = $node->name;
	is($node->safe_psql('postgres', 'SELECT count(*) FROM pg_replication_slots'), '0', "no slot on $n");
	is($node->safe_psql('postgres', 'SELECT count(*) FROM pg_subscription'), '0', "no subscription on $n");
	is($node->safe_psql('postgres', 'SELECT count(*) FROM pg_publication'), '0', "no publication on $n");
	is($node->safe_psql('postgres', 'SELECT count(*) FROM pg_replication_origin'), '0', "no origin on $n");
	is($node->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'), '0', "nothing prepared on $n");
	is($node->safe_psql('postgres', 'SELECT count(*) FROM kingres.move_marker'), '0', "no marker row on $n");
}
is($n1->safe_psql('postgres', q{SELECT coalesce(string_agg(check_name || ' ' || node_id || ' ' || object || ' ' || detail, E'
'), '') FROM kingres.check_cluster()}), '', 'check_cluster finds nothing');

# Writes go to the new owner and are visible everywhere.
{
	my ($key) = $n1->safe_psql('postgres', qq{
		SELECT id FROM generate_series(1000, 2000) id WHERE kingres.bucket_for(1, id) = $bucket LIMIT 1});
	$n2->safe_psql('postgres', "INSERT INTO public.orders VALUES ($key, 1, 'after the move')");
	$n3->safe_psql('postgres', "INSERT INTO public.items VALUES (500, $key, 3)");
	is($tgt_node->safe_psql('postgres', "SELECT count(*) FROM public.$part WHERE id = $key"), '1',
		'a row of the moved bucket is stored on the new owner');
	is($_->safe_psql('postgres', "SELECT count(*) FROM public.orders WHERE id = $key"), '1',
		'and read from ' . $_->name) for @nodes;
	$n1->safe_psql('postgres', "DELETE FROM public.items WHERE order_id = $key; DELETE FROM public.orders WHERE id = $key");
}
same_everywhere('after writes to the moved bucket', $want);

# A second move of the same bucket works, and cleaning waits for the delay.
is($n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets()'), '0', 'cleanup keeps the orphans during the delay');
is(partition_state($src_node, $part), 'absent', 'still no partition on the source');
is($src_node->safe_psql('postgres', "SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan\\_%'"),
	'2', 'the orphans are still there');
is($n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)'), '1', 'a forced cleanup finishes the move');
is(move_state($n1, $id), 'done|', 'the move is done');
is($src_node->safe_psql('postgres', "SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan\\_%'"),
	'0', 'the orphan tables are gone');
is($n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)'), '0', 'cleanup is repeatable');

same_everywhere('after the cleanup', $want);

# Back again to where it came from: the names are free once the orphans are gone.
my $id2 = $n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $bucket, $source)");
is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket"),
	"$source", 'the bucket is back on its first group');
same_everywhere('after the move back', $want);
is($n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)'), '1', 'cleanup of the second move');
is($n1->safe_psql('postgres', q{SELECT coalesce(string_agg(check_name || ' ' || node_id || ' ' || object || ' ' || detail, E'
'), '') FROM kingres.check_cluster()}), '', 'check_cluster finds nothing at the end');

done_testing();
