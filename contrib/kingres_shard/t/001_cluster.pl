# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Three-node cluster: node registry, catalog synchronization and the
# placement of the partitions of a distributed table (design/shard-mvp.md,
# "Tests required for phase 1", multi-node part 1).

use strict;
use warnings FATAL => 'all';
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

# All nodes preload the extension and trust each other over their sockets.
sub make_node
{
	my ($name) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init;
	$node->append_conf('postgresql.conf',
		"shared_preload_libraries = 'kingres_shard'");
	$node->start;
	return $node;
}

my @nodes = map { make_node("n$_") } (1 .. 3);
my ($n1, $n2, $n3) = @nodes;

# The tests create tables in public: a role named like a schema (the OS user
# here is often "kingres") would capture unqualified names otherwise.
$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres',
	sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));

is($n1->safe_psql('postgres', 'SELECT node_id, group_id FROM kingres.nodes'),
	'1|0', 'init_cluster registers node 1 in the map group');

my ($rc, $stdout, $stderr) = $n1->psql('postgres',
	sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
like($stderr, qr/already initialized/, 'init_cluster twice is an error');

# Add the other two nodes.
is( $n1->safe_psql('postgres',
		sprintf("SELECT kingres.add_node('%s', %d)", $n2->host, $n2->port)),
	'2', 'add_node returns node id 2');
is( $n1->safe_psql('postgres',
		sprintf("SELECT kingres.add_node('%s', %d)", $n3->host, $n3->port)),
	'3', 'add_node returns node id 3');

# add_node refuses a duplicate address and a non-map-group caller.
($rc, $stdout, $stderr) = $n1->psql('postgres',
	sprintf("SELECT kingres.add_node('%s', %d)", $n2->host, $n2->port));
like($stderr, qr/already registered as node 2/, 'duplicate node is refused');
($rc, $stdout, $stderr) = $n2->psql('postgres',
	sprintf("SELECT kingres.add_node('%s', %d)", 'elsewhere', 5999));
like($stderr, qr/only run on the map group primary/,
	'add_node on a node that is not the map group primary is refused');

# Every node has the same catalog, and its own local node id.
sub catalog_text
{
	my ($node) = @_;
	return $node->safe_psql('postgres',
		'SELECT kingres.internal_catalog_snapshot()::text');
}

sub assert_catalogs_equal
{
	my ($label) = @_;
	my $c1 = catalog_text($n1);
	is(catalog_text($n2), $c1, "$label: catalog of node 2 equals node 1");
	is(catalog_text($n3), $c1, "$label: catalog of node 3 equals node 1");
}

assert_catalogs_equal('after add_node');
is( $n2->safe_psql('postgres',
		'SELECT node_id FROM kingres.local_node'),
	'2', 'node 2 knows it is node 2');
is( $n3->safe_psql('postgres',
		'SELECT node_id FROM kingres.local_node'),
	'3', 'node 3 knows it is node 3');
is( $n3->safe_psql('postgres',
		'SELECT string_agg(node_id || \':\' || group_id, \',\' ORDER BY node_id) FROM kingres.nodes'),
	'1:0,2:1,3:2', 'each node is its own shard group');
is( $n3->safe_psql('postgres',
		'SELECT node_id FROM kingres.nodes WHERE is_local'),
	'3', 'the nodes view marks the local node');

# A distributed table: 12 buckets over 3 groups, 4 buckets each.
$n1->safe_psql('postgres',
	'CREATE TABLE public.users (id int, name text, PRIMARY KEY (id))');
$n1->safe_psql('postgres',
	"SELECT kingres.create_distributed_table('public.users', 'id', bucket_count => 12)");

assert_catalogs_equal('after create_distributed_table');

my $map = $n1->safe_psql('postgres',
	"SELECT string_agg(bucket || ':' || group_id, ',' ORDER BY bucket) FROM kingres.bucket_map");
is( $map,
	'0:0,1:1,2:2,3:0,4:1,5:2,6:0,7:1,8:2,9:0,10:1,11:2',
	'buckets are assigned round-robin in group order');

for my $i (0 .. 2)
{
	my $node = $nodes[$i];
	my $group = $i;    # node i+1 is group i
	my $label = "node " . ($i + 1);

	is( $node->safe_psql('postgres',
			"SELECT relkind FROM pg_class WHERE oid = 'public.users'::regclass"),
		'p', "$label has the partitioned parent table");
	is( $node->safe_psql('postgres',
			"SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.users'::regclass"),
		'1', "$label has the table in its catalog");

	# The partitions that exist are exactly the buckets the map gives to
	# this node's group.
	my $have = $node->safe_psql('postgres',
		"SELECT coalesce(string_agg(substring(c.relname from '_b([0-9]+)\$')::int::text, ',' ORDER BY substring(c.relname from '_b([0-9]+)\$')::int), '')
		   FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		  WHERE i.inhparent = 'public.users'::regclass");
	my $want = $node->safe_psql('postgres',
		"SELECT string_agg(bucket::text, ',' ORDER BY bucket)
		   FROM kingres.bucket_map WHERE group_id = $group");
	is($have, $want, "$label has the partitions of the buckets of group $group");

	# The introspection view agrees about what is local.
	is( $node->safe_psql('postgres',
			"SELECT count(*) FROM kingres.buckets WHERE partition_size IS NOT NULL"),
		'4', "$label sees four local partitions in kingres.buckets");

	# PostgreSQL itself keeps the primary key: the unique index exists.
	is( $node->safe_psql('postgres',
			"SELECT count(*) FROM pg_index WHERE indrelid = 'public.users'::regclass AND indisprimary"),
		'1', "$label has the primary key on the parent");
}

# A second table colocated with the first has the same placement.
$n1->safe_psql('postgres',
	'CREATE TABLE public.orders (id int PRIMARY KEY, user_id int)');
$n1->safe_psql('postgres',
	"SELECT kingres.create_distributed_table('public.orders', 'id', colocate_with => 'public.users')");
is( $n2->safe_psql('postgres',
		"SELECT count(*) FROM pg_inherits WHERE inhparent = 'public.orders'::regclass"),
	'4', 'a colocated table is placed like its colocation partner');
is( $n1->safe_psql('postgres',
		"SELECT count(DISTINCT colocation_id) FROM kingres.dist_table"),
	'1', 'both tables are in one colocation group');
assert_catalogs_equal('after the second table');

# A table whose primary key lacks the key is refused, and nothing is created
# on any node.
$n1->safe_psql('postgres',
	'CREATE TABLE public.bad (id int PRIMARY KEY, k int)');
($rc, $stdout, $stderr) = $n1->psql('postgres',
	"SELECT kingres.create_distributed_table('public.bad', 'k')");
like($stderr, qr/primary key constraint "bad_pkey" does not include the distribution column "k"/,
	'a primary key without the distribution column is refused');
is( $n2->safe_psql('postgres',
		"SELECT to_regclass('public.bad') IS NULL"),
	't', 'the refused table does not exist on another node');

# A node in a new group owns no buckets and can be removed again.
my $n4 = make_node('n4');
is( $n1->safe_psql('postgres',
		sprintf("SELECT kingres.add_node('%s', %d)", $n4->host, $n4->port)),
	'4', 'a fourth node can be added after tables exist');
is( $n4->safe_psql('postgres',
		"SELECT (SELECT relkind::text FROM pg_class WHERE oid = 'public.users'::regclass) || ':' || (SELECT count(*) FROM pg_inherits WHERE inhparent = 'public.users'::regclass)"),
	'p:0', 'the new node gets the parent of each distributed table, with no partitions');
assert_catalogs_equal('after adding node 4');
is( catalog_text($n4), catalog_text($n1), 'the new node has the same catalog');
is( $n1->safe_psql('postgres',
		"SELECT count(*) FROM kingres.bucket_map WHERE group_id = 3"),
	'0', 'a new group owns no buckets');

($rc, $stdout, $stderr) = $n1->psql('postgres',
	"SELECT kingres.remove_node(2)");
like($stderr, qr/its shard group 1 owns 4 buckets/,
	'a node whose group owns buckets cannot be removed');
($rc, $stdout, $stderr) = $n1->psql('postgres',
	"SELECT kingres.remove_node(1)");
like($stderr, qr/cannot be removed/, 'the map group primary cannot be removed');

$n1->safe_psql('postgres', 'SELECT kingres.remove_node(4)');
is( $n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'),
	'3', 'remove_node unregisters the node');
is( $n4->safe_psql('postgres',
		'SELECT (SELECT count(*) FROM kingres.local_node) + (SELECT count(*) FROM kingres.node)'),
	'0', 'the removed node forgot the cluster');
assert_catalogs_equal('after remove_node');

# sync_catalog repairs a node whose catalog was damaged.
$n3->safe_psql('postgres', 'DELETE FROM kingres.bucket_map');
isnt(catalog_text($n3), catalog_text($n1), 'a damaged catalog differs');
$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
assert_catalogs_equal('after sync_catalog');

# The catalog sync is not atomic: a failure on one node aborts the local
# transaction when the remote commit fails.  Stop node 3 and try to change
# the catalog.
$n3->stop;
($rc, $stdout, $stderr) = $n1->psql('postgres',
	'SELECT kingres.sync_catalog()');
isnt($rc, 0, 'sync_catalog fails while a node is down');
like($stderr, qr/could not connect to node/, 'the error names the unreachable node');
$n3->start;

foreach my $n (@nodes, $n4)
{
	$n->stop;
}

done_testing();
