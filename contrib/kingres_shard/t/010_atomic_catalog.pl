# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Atomic catalog changes (design/shard-phase2a.md, section 2): sync_catalog(),
# add_node() and create_distributed_table() change every node in one
# two-phase transaction, so a failure or a crash leaves no node with a table or
# a catalog row that another node does not have.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('ac', 3);
my $n4 = make_node('ac4');

sub catalog_text
{
	my ($node) = @_;
	return $node->safe_psql('postgres', 'SELECT kingres.internal_catalog_snapshot()::text');
}

sub assert_catalogs_equal
{
	my ($label, @nodes) = @_;
	my $c1 = catalog_text($nodes[0]);
	is(catalog_text($_), $c1, "$label: catalog of $_->name equals the map group's") for @nodes[ 1 .. $#nodes ];
}

sub table_kind
{
	my ($node, $table) = @_;
	return $node->safe_psql('postgres',
		"SELECT coalesce((SELECT relkind::text FROM pg_class WHERE oid = to_regclass('public.$table')), 'none')");
}

sub dist_rows
{
	my ($node) = @_;
	return $node->safe_psql('postgres', 'SELECT count(*) FROM kingres.dist_table');
}

sub crash
{
	my ($point, $sql) = @_;
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:kill';\n$sql");
	isnt($rc, 0, "the origin dies at $point");
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
}

assert_catalogs_equal('after setup', $n1, $n2, $n3);
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');

# ---------------------------------------------------------------------------
# A failure on the last node undoes the work on the first ones.
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', 'CREATE TABLE public.t1 (id int PRIMARY KEY, v text)');
	$n3->safe_psql('postgres', 'CREATE TABLE public.t1 (other int)');	# in the way
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.t1', 'id', bucket_count => 6)");
	like($err, qr/already exists/, 'create_distributed_table fails when a node cannot create the table');
	is(table_kind($n1, 't1'), 'r', 'the table on the origin is still the plain table');
	is(table_kind($n2, 't1'), 'none', 'node 2 did not keep the table');
	is(dist_rows($_), '0', "no catalog row of the failed table on $_->name") for ($n1, $n2, $n3);
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.colocation'), '0',
		'no colocation group was left behind');
	is(prepared_count($_), '0', "nothing is prepared on $_->name") for ($n1, $n2, $n3);
	assert_catalogs_equal('after the failed create', $n1, $n2, $n3);

	$n3->safe_psql('postgres', 'DROP TABLE public.t1');
	$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.t1', 'id', bucket_count => 6)");
	is(table_kind($_, 't1'), 'p', "the retry created the table on $_->name") for ($n1, $n2, $n3);
	is(dist_rows($_), '1', "one catalog row on $_->name") for ($n1, $n2, $n3);
	assert_catalogs_equal('after the retry', $n1, $n2, $n3);
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
}

# ---------------------------------------------------------------------------
# A crash after the prepare and before the local commit rolls back everywhere.
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', 'CREATE TABLE public.t2 (id int PRIMARY KEY, v text)');
	crash('after_prepare', "SELECT kingres.create_distributed_table('public.t2', 'id', bucket_count => 6)");
	is(prepared_count($n2), '1', 'node 2 holds a prepared transaction');
	is(prepared_count($n3), '1', 'node 3 holds a prepared transaction');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2',
		'recovery rolls both back');
	is(table_kind($n1, 't2'), 'r', 'the origin still has the plain table');
	is(table_kind($n2, 't2'), 'none', 'node 2 has no t2');
	is(table_kind($n3, 't2'), 'none', 'node 3 has no t2');
	is(dist_rows($_), '1', "$_->name still has one catalog row") for ($n1, $n2, $n3);
	assert_catalogs_equal('after rollback by recovery', $n1, $n2, $n3);
}

# ---------------------------------------------------------------------------
# A crash after the local commit is completed by recovery.
# ---------------------------------------------------------------------------
{
	crash('after_commit', "SELECT kingres.create_distributed_table('public.t2', 'id', bucket_count => 6)");
	is(table_kind($n1, 't2'), 'p', 'the origin committed the distributed table');
	is(table_kind($n2, 't2'), 'none', 'node 2 does not show it yet (prepared)');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2',
		'recovery commits both');
	is(table_kind($_, 't2'), 'p', "$_->name has the table") for ($n2, $n3);
	is(dist_rows($_), '2', "$_->name has both catalog rows") for ($n1, $n2, $n3);
	assert_catalogs_equal('after commit by recovery', $n1, $n2, $n3);
	is($n2->safe_psql('postgres', q{SELECT count(*) FROM pg_class WHERE relname ~ '^t2_b[0-9]{4}$' AND relkind = 'r'}),
		$n1->safe_psql('postgres', "SELECT count(*) FILTER (WHERE group_id = 1) FROM kingres.buckets WHERE table_name = 'public.t2'::regclass"),
		'node 2 has exactly the partitions of its buckets');
}

# ---------------------------------------------------------------------------
# add_node: a crash leaves the new node untouched.
# ---------------------------------------------------------------------------
{
	crash('after_prepare', sprintf("SELECT kingres.add_node('%s', %d)", $n4->host, $n4->port));
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '3',
		'recovery rolls back add_node on the three nodes that took part');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), '3', 'the map group did not register the node');
	is($n4->safe_psql('postgres', "SELECT count(*) FROM pg_extension WHERE extname = 'kingres_shard'"), '0',
		'the new node did not keep the extension');
	assert_catalogs_equal('after the rolled back add_node', $n1, $n2, $n3);

	is($n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('%s', %d)", $n4->host, $n4->port)), '4',
		'the node can be added afterwards');
	is($n4->safe_psql('postgres', 'SELECT count(*) FROM kingres.dist_table'), '2',
		'the new node received the distributed tables');
	assert_catalogs_equal('after add_node', $n1, $n2, $n3, $n4);
}

# ---------------------------------------------------------------------------
# sync_catalog: all or nothing.
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	crash('after_prepare',
		"BEGIN; UPDATE kingres.node SET server_group = 'rack-9' WHERE node_id = 3; SELECT kingres.sync_catalog(); COMMIT;");
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '3',
		'recovery rolls back the synchronization');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE server_group = 'rack-9'"), '0',
		'the change is gone on the origin');
	is($_->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE server_group = 'rack-9'"), '0',
		"and on $_->name") for ($n2, $n3, $n4);
	assert_catalogs_equal('after the rolled back sync', $n1, $n2, $n3, $n4);

	$n1->safe_psql('postgres',
		"BEGIN; UPDATE kingres.node SET server_group = 'rack-9' WHERE node_id = 3; SELECT kingres.sync_catalog(); COMMIT;");
	is($_->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE server_group = 'rack-9'"), '1',
		"a clean synchronization reaches $_->name") for ($n2, $n3, $n4);
	assert_catalogs_equal('after a clean sync', $n1, $n2, $n3, $n4);
}

done_testing();
