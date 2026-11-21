# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/234_schema_modes_cluster.pl
#
# Phase 11 (design/shard-phase11.md), increment 3, section 10 and 7.2: what the sharding mode of the database allows,
# what a new schema becomes, the settings of the session, and the limit, on two nodes.
#
#   A. mode row (the default of every database): no distribution of schemas, a new schema is local
#   B. mode schema: create_distributed_table is refused, a reference table is allowed, distribute_schema works
#   C. mode both: both kinds; a table of a distributed schema can be neither a hash nor a reference table
#   D. the default for new schemas, and what a session may change (kingres_shard.distribute_new_schemas)
#   E. CREATE SCHEMA AUTHORIZATION, the group of the session, the limit of schemas

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('sm', 2);
my ($n1, $n2) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

sub fails
{
	my ($n, $sql, $re, $name, %opt) = @_;
	my ($rc, $out, $err) = run_sql($n, $sql, %opt);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "... message: $name") or diag($err);
}

for my $n (@nodes) { q1($n, q{CREATE ROLE alice LOGIN; GRANT CREATE ON DATABASE postgres TO alice}); }

# ---------------------------------------------------------------------------
# A. mode row
# ---------------------------------------------------------------------------
is(q1($n1, q{SELECT mode || ':' || distribute_new_schemas FROM kingres.sharding_settings}), 'row:false', 'a database starts in the mode row');
q1($n1, q{CREATE SCHEMA r1; CREATE TABLE r1.t (id int PRIMARY KEY)});
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema}), 0, 'in the mode row a new schema is local');
is(q1($n2, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'r1'}), 0, '... and exists on this node only');
q1($n1, q{SET kingres_shard.distribute_new_schemas = on; CREATE SCHEMA r_on});
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema}), 0, 'in the mode row even the session setting on does not distribute a schema');
q1($n1, q{DROP SCHEMA r_on});
q1($n1, q{CREATE TABLE public.h1 (id int PRIMARY KEY, v int); SELECT kingres.create_distributed_table('public.h1', 'id', bucket_count => 4)});
is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE method = 'hash'}), 1, 'a hash table can be made in the mode row');
fails($n1, q{SELECT kingres.set_sharding_mode('schema')}, qr/hash-distributed tables exist/, 'the mode schema while a hash table exists');
q1($n1, q{SELECT kingres.undistribute_table('public.h1')});

# ---------------------------------------------------------------------------
# B. mode schema
# ---------------------------------------------------------------------------
q1($n1, q{SELECT kingres.set_sharding_mode('schema')});
is(q1($n2, q{SELECT mode FROM kingres.sharding_settings}), 'schema', 'the mode reached every node');
q1($n1, q{CREATE TABLE public.h2 (id int PRIMARY KEY)});
fails($n1, q{SELECT kingres.create_distributed_table('public.h2', 'id')}, qr/create_distributed_table is not available in the sharding mode "schema"/, 'create_distributed_table in the mode schema');
q1($n1, q{CREATE TABLE public.rf (id int PRIMARY KEY, name text); SELECT kingres.create_reference_table('public.rf')});
is(q1($n2, q{SELECT method FROM kingres.dist_table WHERE relid = 'public.rf'::regclass}), 'reference', 'a reference table is allowed in the mode schema');
q1($n1, q{CREATE MATERIALIZED VIEW r1.mv AS SELECT 1 AS x});
fails($n1, q{SELECT kingres.distribute_schema('r1')}, qr/materialized view/, 'a schema with a materialized view is not distributed');
q1($n1, q{DROP MATERIALIZED VIEW r1.mv});
q1($n1, q{DROP TABLE r1.t});
q1($n1, q{SELECT kingres.distribute_schema('r1')});
is(q1($n2, q{SELECT schema_name FROM kingres.schemas}), 'r1', 'distribute_schema works in the mode schema, on every node');
fails($n1, q{SELECT kingres.set_sharding_mode('row')}, qr/distributed schemas exist/, 'back to row with a distributed schema');
fails($n1, q{SELECT kingres.set_sharding_mode('row', true)}, qr/cannot be distributed by default in mode "row"/, 'a default for new schemas in the mode row');

# ---------------------------------------------------------------------------
# C. mode both
# ---------------------------------------------------------------------------
q1($n1, q{SELECT kingres.set_sharding_mode('both')});
q1($n1, q{SELECT kingres.create_distributed_table('public.h2', 'id', bucket_count => 4)});
is(q1($n2, q{SELECT method FROM kingres.dist_table WHERE relid = 'public.h2'::regclass}), 'hash', 'a hash table in the mode both');
q1($n1, q{CREATE TABLE r1.st (id int PRIMARY KEY)});
fails($n1, q{SELECT kingres.create_distributed_table('r1.st', 'id')}, qr/table "st" is in a distributed schema, which holds only schema tables/, 'a hash table in a distributed schema');
fails($n1, q{SELECT kingres.create_reference_table('r1.st')}, qr/table "st" is in a distributed schema, which holds only schema tables/, 'a reference table in a distributed schema');
is(q1($n2, q{SELECT method FROM kingres.dist_table WHERE relid = 'r1.st'::regclass}), 'schema', 'the table of the schema stayed a schema table');
fails($n1, q{SELECT kingres.set_sharding_mode('schema')}, qr/hash-distributed tables exist/, 'the mode schema while a hash table exists');

# ---------------------------------------------------------------------------
# D. the default of new schemas
# ---------------------------------------------------------------------------
is(q1($n1, q{SELECT distribute_new_schemas FROM kingres.sharding_settings}), 'f', 'the mode both starts with the default off');
q1($n1, q{CREATE SCHEMA d_local});
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'd_local'::regnamespace}), 0, 'with the default off a new schema is local');
{
	my ($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.distribute_new_schemas = on; CREATE SCHEMA d_on});
	is($rc, 0, 'a superuser overrides the default for the session') or diag($err);
	is(q1($n2, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'd_on'::regnamespace}), 1, '... and the schema is distributed on every node');
	($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.distribute_new_schemas = on}, extra_params => [ '-U', 'alice' ]);
	isnt($rc, 0, 'an ordinary role cannot change the setting');
	like($err, qr/permission denied to set parameter/, '... for lack of privilege') or diag($err);
}
q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});
q1($n1, q{CREATE SCHEMA d_default});
is(q1($n2, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'd_default'::regnamespace}), 1, 'with the default on a new schema is distributed');
q1($n1, q{SET kingres_shard.distribute_new_schemas = off; CREATE SCHEMA d_off});
is(q1($n2, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'd_off'}), 0, 'a superuser can opt a schema out for the session');
{
	my ($rc, $out, $err) = run_sql($n1, q{CREATE SCHEMA d_alice}, extra_params => [ '-U', 'alice' ]);
	is($rc, 0, 'an ordinary role creates a schema') or diag($err);
	is(q1($n2, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'd_alice'::regnamespace}), 1, '... which is distributed: it cannot opt out');
}

# ---------------------------------------------------------------------------
# E. AUTHORIZATION, the group of the session, the limit
# ---------------------------------------------------------------------------
q1($n1, q{CREATE SCHEMA AUTHORIZATION alice});
is(q1($n2, q{SELECT schema_name || ':' || owner FROM kingres.schemas WHERE schema_name = 'alice'}), 'alice:alice', 'CREATE SCHEMA AUTHORIZATION: the schema is named like the role');
{
	my $g = q1($n1, q{SELECT group_id FROM kingres.node WHERE node_id = 2});
	q1($n1, "SET kingres_shard.schema_placement_group = $g; CREATE SCHEMA pinned");
	is(q1($n2, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'pinned'}), $g, 'the group of the session setting places a new schema');
	fails($n1, q{SET kingres_shard.schema_placement_group = 55; CREATE SCHEMA nowhere}, qr/shard group 55 cannot hold a schema/, 'a group that cannot hold it');
	is(q1($n2, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'nowhere'}), 0, '... nothing is created');
}
{
	my $now = q1($n1, q{SELECT count(*) FROM kingres.dist_schema});
	q1($n1, "ALTER SYSTEM SET kingres_shard.max_distributed_schemas = $now");
	$n1->reload;
	fails($n1, q{CREATE SCHEMA over_limit}, qr/already has $now distributed schemas, the limit of kingres_shard.max_distributed_schemas/, 'CREATE SCHEMA at the limit');
	is(q1($n1, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'over_limit'}), 0, '... nothing is created here');
	q1($n1, q{SET kingres_shard.distribute_new_schemas = off; CREATE SCHEMA still_local});
	is(q1($n1, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'still_local'}), 1, 'a local schema is not counted');
	q1($n1, 'ALTER SYSTEM RESET kingres_shard.max_distributed_schemas');
	$n1->reload;
}
for my $n (@nodes) { is(prepared_count($n), 0, 'nothing is prepared on ' . $n->name); }

done_testing();
