# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/192_partition_catalog.pl: kingres.partition_leaves(), kingres.partitions and the check of check_cluster() for the leaves of user partitions
# (design/shard-phase9.md, increment 9.5, feature 4: what phase 12 can rely on).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my ($n1, $n2, $n3) = make_cluster('pc', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

$n1->safe_psql('postgres', q{
	CREATE TABLE public.cl (customer int NOT NULL, created date NOT NULL, id int NOT NULL, qty int, PRIMARY KEY (customer, created, id)) PARTITION BY RANGE (created);
	CREATE TABLE public.cl_a PARTITION OF public.cl FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
	CREATE TABLE public.cl_b PARTITION OF public.cl FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
	INSERT INTO public.cl SELECT g % 13, date '2026-01-01' + (g % 50), g, g FROM generate_series(1, 500) g;
	SELECT kingres.create_distributed_table('public.cl', 'customer', bucket_count => 6);
});

# partition_leaves: one row per bucket, on the node that owns the bucket, and the leaf is there
is(q1($n1, q{SELECT count(*) FROM kingres.partition_leaves('public.cl', 'cl_a')}), '6', 'six leaves');
is(q1($n1, q{SELECT count(DISTINCT bucket) FROM kingres.partition_leaves('public.cl', 'cl_a')}), '6', 'one per bucket');
is(q1($n1, q{SELECT count(*) FROM kingres.partition_leaves('public.cl', 'cl_a') l JOIN kingres.bucket_map m ON m.bucket = l.bucket AND m.colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.cl'::regclass)
              JOIN kingres.node n ON n.node_id = l.node_id AND n.group_id = m.group_id}), '6', 'each on the node of the group that owns its bucket');
is(q1($n1, q{SELECT count(*) FROM kingres.partition_leaves('public.cl', 'nothing')}), '0', 'a partition that does not exist has no leaves');
is(q1($n1, q{SELECT count(DISTINCT leaf) FROM kingres.partition_leaves('public.cl', 'cl_a') WHERE leaf ~ '^public\.cl_a_b[0-9]{4}$'}), '6', 'with their names');
for my $n (@nodes)
{
	my $id = q1($n, q{SELECT node_id FROM kingres.local_node});
	my $expected = q1($n1, "SELECT count(*) FROM kingres.partition_leaves('public.cl', 'cl_a') WHERE node_id = $id");
	is(q1($n, q{SELECT count(*) FROM pg_class WHERE relname ~ '^cl_a_b[0-9]{4}$' AND relkind = 'r'}), $expected, 'node ' . $n->name . ' has the leaves that the catalog lists for it');
	is(q1($n, q{SELECT size_here > 0 FROM kingres.partitions WHERE table_name = 'public.cl'::regclass AND partition_name = 'cl_a'}), 't', 'kingres.partitions shows its size on ' . $n->name);
	is(q1($n, q{SELECT leaf_count FROM kingres.partitions WHERE table_name = 'public.cl'::regclass AND partition_name = 'cl_b'}), '6', 'and the count of leaves');
}
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'dist_partition_mismatch'}), '0', 'check_cluster() finds no mismatch');

# a leaf that is gone from its node is found, with the node and the leaf
my ($node, $leaf) = split /\|/, q1($n1, q{SELECT node_id || '|' || leaf FROM kingres.partition_leaves('public.cl', 'cl_b') WHERE node_id = 2 ORDER BY bucket LIMIT 1});
is($node, '2', 'node 2 owns a leaf');
my ($rc, $out, $err) = run_sql($n2, "SET kingres_shard.enable_routing = off; DROP TABLE $leaf");
is($rc, 0, 'a leaf is dropped by hand') or diag($err);
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'dist_partition_mismatch'}), '1', 'check_cluster() finds one mismatch');
is(q1($n1, q{SELECT node_id || '|' || object FROM kingres.check_cluster() WHERE check_name = 'dist_partition_mismatch'}), "2|$leaf", 'on the right node, naming the leaf');
done_testing();
