# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/230_schema_catalog_cluster.pl
#
# Phase 11 (design/shard-phase11.md), increment 1, on three nodes: the catalog of distributed schemas travels with the snapshot, by
# name, to every node; a node that lacks the schema makes the whole push fail and nothing is changed anywhere; set_sharding_mode
# reaches every node and only the map group primary may run it; the planner is not changed yet (a schema table is an ordinary table
# until the later increments), so statements on it work as before.  The rows are made by the node-local delta functions: the product
# does not create a schema group yet.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('sc', 3);
my ($n1, $n2, $n3) = @nodes;

# the schema exists, with its tables, on every node (a distributed schema has its structure everywhere)
for my $n (@nodes)
{
	$n->safe_psql('postgres', q{
		CREATE SCHEMA ten;
		CREATE TABLE ten.a (id int PRIMARY KEY, v text);
		CREATE TABLE ten.b (id int PRIMARY KEY, a_id int);
	});
}
$n2->safe_psql('postgres', q{INSERT INTO ten.a SELECT g, 'row ' || g FROM generate_series(1, 50) g});

# ---------------------------------------------------------------------------
# A. the registration is pushed to every node by name
# ---------------------------------------------------------------------------
my $gid = $n1->safe_psql('postgres', q{SELECT group_id FROM kingres.node WHERE node_id = 2});
is($gid, 1, 'node 2 is the group 1');
my $coloc = $n1->safe_psql('postgres', q{SELECT nextval('kingres.colocation_id_seq')});
$n1->safe_psql('postgres', "SELECT kingres.internal_register_schema('ten', $coloc, $gid)");
$n1->safe_psql('postgres', q{SELECT kingres.internal_register_schema_table('ten.a'), kingres.internal_register_schema_table('ten.b')});
$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
for my $n (@nodes)
{
	my $nm = $n->name;
	is($n->safe_psql('postgres', q{SELECT schema_name || '|' || colocation_id || '|' || group_id || '|' || node_id || '|' || state || '|' || table_count FROM kingres.schemas}),
		"ten|$coloc|1|2|active|2", "$nm: kingres.schemas has the schema, its group and its owner node");
	is($n->safe_psql('postgres', q{SELECT string_agg(relid::text || ':' || method, ',' ORDER BY relid::text) FROM kingres.dist_table}),
		'ten.a:schema,ten.b:schema', "$nm: the two tables are schema tables");
	is($n->safe_psql('postgres', q{SELECT kind || ':' || bucket_count FROM kingres.colocation}), 'schema:1', "$nm: one colocation group of kind schema with one bucket");
	is($n->safe_psql('postgres', q{SELECT group_id || ':' || state FROM kingres.bucket_map WHERE bucket = 0}), '1:active', "$nm: the bucket is in the group of the schema");
	is($n->safe_psql('postgres', q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'ten'::regnamespace}), 1, "$nm: the row of dist_schema names its schema (an OID of this node)");
}
# size is known where the tables are
is($n2->safe_psql('postgres', q{SELECT size_bytes > 0 FROM kingres.schemas}), 't', 'node 2 (the owner) knows the size');
is($n1->safe_psql('postgres', q{SELECT size_bytes IS NULL FROM kingres.schemas}), 't', 'node 1 does not (its tables are shells)');
# the buckets view
is($n3->safe_psql('postgres', q{SELECT string_agg(table_name::text || ':' || bucket || ':' || coalesce(partition_name, '-') || ':' || group_id, ',' ORDER BY table_name::text) FROM kingres.buckets}),
	'ten.a:0:-:1,ten.b:0:-:1', 'kingres.buckets shows a schema table with bucket 0 and no partition');

# the planner is not changed yet: statements on the tables are those of an ordinary table, on the node that has the rows
is($n2->safe_psql('postgres', q{SELECT count(*) FROM ten.a}), 50, 'a statement on a schema table runs as on an ordinary table (the owner has the rows)');
$n1->safe_psql('postgres', q{INSERT INTO ten.b VALUES (1, 1)});
is($n1->safe_psql('postgres', q{SELECT count(*) FROM ten.b}), 1, '... and a write is local');

# ---------------------------------------------------------------------------
# B. a node that lacks the schema: the push fails everywhere
# ---------------------------------------------------------------------------
for my $n ($n1, $n2) { $n->safe_psql('postgres', 'CREATE SCHEMA ten2; CREATE TABLE ten2.x (id int PRIMARY KEY)'); }
my $coloc2 = $n1->safe_psql('postgres', q{SELECT nextval('kingres.colocation_id_seq')});
$n1->safe_psql('postgres', "SELECT kingres.internal_register_schema('ten2', $coloc2, 0)");
$n1->safe_psql('postgres', q{SELECT kingres.internal_register_schema_table('ten2.x')});
my ($rc, $out, $err) = run_sql($n1, 'SELECT kingres.sync_catalog()');
isnt($rc, 0, 'the push fails when a node does not have a distributed schema');
like($err, qr/distributed (schema ten2|table ten2\.x) does not exist on this node/, '... and the error names it') or diag($err);
for my $n ($n2, $n3)
{
	is($n->safe_psql('postgres', q{SELECT string_agg(schema_name, ',' ORDER BY schema_name) FROM kingres.schemas}), 'ten', $n->name . ': nothing of the failed push is there');
}
# make it whole: the schema on the third node, then the push works
$n3->safe_psql('postgres', 'CREATE SCHEMA ten2; CREATE TABLE ten2.x (id int PRIMARY KEY)');
$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
for my $n (@nodes)
{
	is($n->safe_psql('postgres', q{SELECT string_agg(schema_name || ':' || group_id, ',' ORDER BY schema_name) FROM kingres.schemas}), 'ten:1,ten2:0', $n->name . ': both schemas after the repair');
}
# the versions are equal on every node after the push
my %ver = map { $_->name => $_->safe_psql('postgres', 'SELECT version FROM kingres.map_version') } @nodes;
is(scalar(keys %{{ map { $_ => 1 } values %ver }}), 1, 'the map version is the same on every node') or diag(join(', ', map { "$_=$ver{$_}" } sort keys %ver));

# ---------------------------------------------------------------------------
# C. the sharding mode reaches every node, and only the map group primary may change it
# ---------------------------------------------------------------------------
is($n3->safe_psql('postgres', q{SELECT mode || ':' || distribute_new_schemas FROM kingres.sharding_settings}), 'row:false', 'every database starts in mode row');
($rc, $out, $err) = run_sql($n2, q{SELECT kingres.set_sharding_mode('both', true)});
isnt($rc, 0, 'node 2 is not the map group primary: it cannot change the mode');
like($err, qr/map group primary/, '... and the error says why');
$n1->safe_psql('postgres', q{SELECT kingres.set_sharding_mode('both', true)});
for my $n (@nodes)
{
	is($n->safe_psql('postgres', q{SELECT mode || ':' || distribute_new_schemas FROM kingres.sharding_settings}), 'both:true', $n->name . ': the mode reached the node');
}
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.set_sharding_mode('row')});
isnt($rc, 0, 'with distributed schemas the mode cannot go back to row');
like($err, qr/distributed schemas exist/, '... and the error says so');
is($n1->safe_psql('postgres', q{SELECT mode FROM kingres.sharding_settings}), 'both', '... and the mode is unchanged');

# ---------------------------------------------------------------------------
# D. the registration is undone on every node
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{SELECT kingres.internal_unregister_schema('ten2')});
$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
for my $n (@nodes)
{
	is($n->safe_psql('postgres', q{SELECT string_agg(schema_name, ',') FROM kingres.schemas}), 'ten', $n->name . ': one schema left');
}
is($n3->safe_psql('postgres', q{SELECT count(*) FROM kingres.colocation}), 1, 'the colocation group of the schema that went is gone');
for my $n (@nodes) { is(prepared_count($n), 0, 'nothing is prepared on ' . $n->name); }

done_testing();
