# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/232_schema_distribute_cluster.pl
#
# Phase 11 (design/shard-phase11.md), increment 3: kingres.distribute_schema() of an empty schema on three nodes,
# the choice of the group (9.1), the limit of schemas, the sharding mode (10), and the atomicity of the creation
# on every node (8.1, 7.10).
#
#   A. the mode "row" refuses; the mode "both" allows
#   B. an empty schema is created on every node with its owner and registered on every node; the groups are chosen
#      by the fewest schemas, the lowest group on a tie; an explicit group; the setting of the session
#   C. what is refused: a bad group, a schema with objects, a system schema, a schema twice, another node than the map
#      group primary, an ordinary role, a read-only mix
#   D. a node that cannot create the schema fails the whole call and nothing is left on any node
#   E. the limit kingres_shard.max_distributed_schemas
#   F. the catalogs of the nodes agree (the delta functions raise the version by the same amount everywhere)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('sd', 3);
my ($n1, $n2, $n3) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

for my $n (@nodes)
{
	q1($n, q{CREATE ROLE tenant_a LOGIN; CREATE ROLE tenant_b LOGIN;});
}

# the schema list as one string on a node: name:group:owner:tables
sub schemas_on
{
	my ($n) = @_;
	return q1($n, q{SELECT coalesce(string_agg(schema_name || ':' || group_id || ':' || owner || ':' || table_count, ',' ORDER BY schema_name), '') FROM kingres.schemas});
}

# ---------------------------------------------------------------------------
# A. the mode
# ---------------------------------------------------------------------------
q1($n1, q{CREATE SCHEMA a1 AUTHORIZATION tenant_a});
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema('a1')});
	isnt($rc, 0, 'in the mode row distribute_schema is refused');
	like($err, qr/not available in the sharding mode "row"/, '... and the message names the mode') or diag($err);
	like($err, qr/set_sharding_mode/, '... and the hint names the function');
}
q1($n1, q{SELECT kingres.set_sharding_mode('both')});

# ---------------------------------------------------------------------------
# B. empty schemas
# ---------------------------------------------------------------------------
my $c1 = q1($n1, q{SELECT kingres.distribute_schema('a1')});
like($c1, qr/^\d+$/, 'distribute_schema returns the colocation id');
for my $n (@nodes)
{
	my $nm = $n->name;
	is(q1($n, q{SELECT nspowner::regrole FROM pg_namespace WHERE nspname = 'a1'}), 'tenant_a', "$nm: the schema exists with its owner");
	is(schemas_on($n), 'a1:0:tenant_a:0', "$nm: the first schema is on the first group (a tie goes to the lowest group), no table yet");
	is(q1($n, q{SELECT kind || ':' || bucket_count FROM kingres.colocation WHERE colocation_id = } . $c1), 'schema:1', "$nm: one colocation group of kind schema");
}
for my $s (qw(a2 a3 a4))
{
	q1($n1, "CREATE SCHEMA $s AUTHORIZATION tenant_b");
	q1($n1, "SELECT kingres.distribute_schema('$s')");
}
for my $n (@nodes)
{
	is(schemas_on($n), 'a1:0:tenant_a:0,a2:1:tenant_b:0,a3:2:tenant_b:0,a4:0:tenant_b:0',
		$n->name . ': the next schemas go to the group with the fewest schemas (1, 2, then 0 again)');
}
q1($n1, q{CREATE SCHEMA a5; SELECT kingres.distribute_schema('a5', 2)});
is(q1($n3, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'a5'}), 2, 'an explicit group');
{
	q1($n1, q{CREATE SCHEMA a6});
	my $old = q1($n1, q{SHOW kingres_shard.schema_placement_group});
	is($old, '-1', 'the setting of the session starts at -1');
	my ($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.schema_placement_group = 1; SELECT kingres.distribute_schema('a6')});
	is($rc, 0, 'distribute_schema with the group of the session setting') or diag($err);
	is(q1($n1, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'a6'}), 1, '... goes to that group');
}

# ---------------------------------------------------------------------------
# C. refusals
# ---------------------------------------------------------------------------
q1($n1, q{CREATE SCHEMA c1});
for my $case (
	[ 'a group that does not exist', q{SELECT kingres.distribute_schema('c1', 77)}, qr/shard group 77 cannot hold a schema/ ],
	[ 'a schema twice', q{SELECT kingres.distribute_schema('a1')}, qr/schema "a1" is already distributed/ ],
	[ 'a system schema', q{SELECT kingres.distribute_schema('pg_catalog')}, qr/system schema, the schema public, or the schema of an extension/ ],
	[ 'the schema public', q{SELECT kingres.distribute_schema('public')}, qr/system schema, the schema public/ ],
	[ 'the schema of this extension', q{SELECT kingres.distribute_schema('kingres')}, qr/system schema, the schema public/ ],
  )
{
	my ($rc, $out, $err) = run_sql($n1, $case->[1]);
	isnt($rc, 0, "refused: $case->[0]");
	like($err, $case->[2], "... the message: $case->[0]") or diag($err);
}
q1($n1, q{CREATE SCHEMA full1; CREATE TABLE full1.t (id int); CREATE MATERIALIZED VIEW full1.mv AS SELECT 1 AS x});
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema('full1')});
	isnt($rc, 0, 'a schema with a materialized view is refused');
	like($err, qr/cannot be distributed: materialized view full1.mv/, '... the message names the object') or diag($err);
	q1($n1, q{DROP SCHEMA full1 CASCADE});
	q1($n1, q{CREATE SCHEMA full2; CREATE AGGREGATE full2.ag(int) (sfunc = int4pl, stype = int)});
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema('full2')});
	like($err, qr/cannot be distributed: aggregate full2.ag/, 'an aggregate is refused') or diag($err);
	q1($n1, q{DROP SCHEMA full2 CASCADE});
}
{
	q1($n2, q{CREATE SCHEMA c2});
	my ($rc, $out, $err) = run_sql($n2, q{SELECT kingres.distribute_schema('c2')});
	isnt($rc, 0, 'a node that is not the map group primary is refused');
	like($err, qr/map group/, '... and the message names the map group') or diag($err);
	q1($n2, q{DROP SCHEMA c2});
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema('c1')}, extra_params => [ '-U', 'tenant_a' ]);
	isnt($rc, 0, 'an ordinary role is refused');
	like($err, qr/permission denied|superuser/, '... for lack of privilege') or diag($err);
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema(null)});
	isnt($rc, 0, 'a schema that is NULL is refused');
}

# ---------------------------------------------------------------------------
# D. a node that cannot create the schema
# ---------------------------------------------------------------------------
my $before = q1($n1, q{SELECT count(*) FROM kingres.dist_schema});
q1($n3, q{CREATE SCHEMA clash});
q1($n1, q{CREATE SCHEMA clash});
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema('clash')});
	isnt($rc, 0, 'a node that has a schema of that name fails the whole call');
	like($err, qr/schema "clash" already exists/, '... with the error of that node') or diag($err);
}
for my $n (@nodes)
{
	is(q1($n, q{SELECT count(*) FROM kingres.dist_schema}), $before, $n->name . ': nothing of the failed call is registered');
	is(prepared_count($n), 0, 'nothing is prepared on ' . $n->name);
}
is(q1($n2, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'clash'}), 0, 'the node that did not fail has not got the schema either');
q1($n3, q{DROP SCHEMA clash});
q1($n1, q{SELECT kingres.distribute_schema('clash')});
is(q1($n3, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'clash'::regnamespace}), 1, 'after the clash is removed the call works');

# ---------------------------------------------------------------------------
# E. the limit
# ---------------------------------------------------------------------------
{
	my $now = q1($n1, q{SELECT count(*) FROM kingres.dist_schema});
	q1($n1, "ALTER SYSTEM SET kingres_shard.max_distributed_schemas = $now");
	$n1->reload;
	q1($n1, q{CREATE SCHEMA e1});
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema('e1')});
	isnt($rc, 0, 'at the limit another schema is refused');
	like($err, qr/already has $now distributed schemas, the limit of kingres_shard.max_distributed_schemas/, '... the message names the limit') or diag($err);
	q1($n1, 'ALTER SYSTEM RESET kingres_shard.max_distributed_schemas');
	$n1->reload;
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema('e1')});
	is($rc, 0, 'with the limit raised again it works') or diag($err);
}

# ---------------------------------------------------------------------------
# F. the nodes agree
# ---------------------------------------------------------------------------
{
	my %list = map { $_->name => schemas_on($_) } @nodes;
	is(scalar(keys %{{ map { $_ => 1 } values %list }}), 1, 'every node lists the same distributed schemas') or diag(join("\n", map { "$_: $list{$_}" } sort keys %list));
	my %ver = map { $_->name => q1($_, 'SELECT version FROM kingres.map_version') } @nodes;
	is(scalar(keys %{{ map { $_ => 1 } values %ver }}), 1, 'the map version is the same on every node') or diag(join(', ', map { "$_=$ver{$_}" } sort keys %ver));
	my %col = map { $_->name => q1($_, q{SELECT string_agg(colocation_id || kind, ',' ORDER BY colocation_id) FROM kingres.colocation}) } @nodes;
	is(scalar(keys %{{ map { $_ => 1 } values %col }}), 1, 'every node has the same colocation groups');
	for my $n (@nodes) { is(prepared_count($n), 0, 'nothing is prepared on ' . $n->name); }
}

done_testing();
