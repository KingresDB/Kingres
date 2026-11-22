# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/236_schema_with_elasticity_cluster.pl
#
# The two lines of work of version 0.8 together (design/shard-phase10.md, phase 11 design 5.2 and 9.9): distributed schemas (phase 11) in a
# cluster that has the bucket layout, the weights, the plans and the split of phase 10, on three nodes.
#
#   A. the catalog carries both lines: the layout columns of phase 10 and the kind of phase 11, equal on every node after a push, and a
#      document of an older map group (no new column at all) is accepted
#   B. a group that holds a distributed schema is never split (internal_split_allowed, split_bucket), a hash group is
#   C. the plans (plan_rebalance, rebalance_plan) do not move the bucket of a schema; check_cluster has nothing to say about a schema
#   D. the buckets view has the modulus last and shows a schema table with one bucket; a split of a hash group and a distributed schema live together

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('el', 3);
my ($n1, $n2, $n3) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});
q1($n1, q{CREATE SCHEMA ea});
q1($n1, q{CREATE TABLE ea.t (id int PRIMARY KEY, v text)});
q1($n1, q{CREATE TABLE public.h (id int PRIMARY KEY, v int)});
q1($n1, q{SELECT kingres.create_distributed_table('public.h', 'id', bucket_count => 4)});
q1($n1, q{INSERT INTO public.h SELECT g, g FROM generate_series(1, 200) g});
my $cs = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'ea'::regnamespace});
my $ch = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.h'::regclass});
isnt($cs, $ch, 'the schema and the hash table are two colocation groups');

# ---------------------------------------------------------------------------
# A. the catalog
# ---------------------------------------------------------------------------
q1($n1, q{SELECT kingres.set_node_property(2, null, 2.5)});
q1($n1, q{SELECT kingres.sync_catalog()});
for my $n (@nodes)
{
	my $nm = $n->name;
	is(q1($n, q{SELECT string_agg(colocation_id || ':' || kind || ':' || bucket_count || ':' || max_modulus || ':' || placement_epoch, ',' ORDER BY colocation_id) FROM kingres.colocation}),
		join(',', map { $_ == $ch ? "$ch:hash:4:4:0" : "$cs:schema:1:1:0" } sort { $a <=> $b } ($ch, $cs)), "$nm: both kinds of group with the layout columns of phase 10");
	is(q1($n, q{SELECT string_agg(colocation_id || '.' || bucket || '/' || modulus || ':' || group_id, ',' ORDER BY colocation_id, bucket) FROM kingres.bucket_map}),
		q1($n1, q{SELECT string_agg(colocation_id || '.' || bucket || '/' || modulus || ':' || group_id, ',' ORDER BY colocation_id, bucket) FROM kingres.bucket_map}), "$nm: the bucket map with its moduli is the map group's");
	is(q1($n, q{SELECT string_agg(node_id || ':' || weight, ',' ORDER BY node_id) FROM kingres.node}), '1:1,2:2.5,3:1', "$nm: the weight of a node came with the push");
}
# a document of a map group that does not have the new columns: the defaults of both lines fill them
{
	my ($rc, $out, $err) = run_sql($n3, q{
		BEGIN;
		SELECT kingres.internal_apply_catalog((kingres.internal_catalog_snapshot()
		  - 'dist_schema' - 'sharding_settings')
		  || jsonb_build_object('colocation', (SELECT jsonb_agg(c - 'kind' - 'max_modulus' - 'placement_epoch') FROM jsonb_array_elements(kingres.internal_catalog_snapshot() -> 'colocation') c WHERE c ->> 'kind' = 'hash'),
		                        'bucket_map', (SELECT jsonb_agg(b - 'modulus') FROM jsonb_array_elements(kingres.internal_catalog_snapshot() -> 'bucket_map') b WHERE (b ->> 'colocation_id')::int = } . $ch . q{),
		                        'node', (SELECT jsonb_agg(x - 'weight') FROM jsonb_array_elements(kingres.internal_catalog_snapshot() -> 'node') x),
		                        'dist_table', (SELECT jsonb_agg(d) FROM jsonb_array_elements(kingres.internal_catalog_snapshot() -> 'dist_table') d WHERE d ->> 'method' = 'hash')));
		SELECT string_agg(colocation_id || ':' || kind || ':' || max_modulus || ':' || placement_epoch, ',' ORDER BY colocation_id) FROM kingres.colocation;
		SELECT string_agg(modulus::text, ',' ORDER BY bucket) FROM kingres.bucket_map;
		SELECT string_agg(weight::text, ',' ORDER BY node_id) FROM kingres.node;
		ROLLBACK;
	});
	is($rc, 0, 'a catalog document without the columns of either line is accepted') or diag($err);
	like($out, qr/^$ch:hash:4:0\n4,4,4,4\n1,1,1$/m, '... and the defaults of both lines fill the rows') or diag($out);
}

# ---------------------------------------------------------------------------
# B. a schema group is never split
# ---------------------------------------------------------------------------
is(q1($n1, "SELECT kingres.internal_split_allowed($ch) IS NULL"), 't', 'a hash group may be split');
like(q1($n1, "SELECT kingres.internal_split_allowed($cs)"), qr/holds a distributed schema: it has one bucket and is never split/, 'a group that holds a schema may not');
{
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($cs, 0)");
	isnt($rc, 0, 'split_bucket of a schema group is refused');
	like($err, qr/holds a distributed schema: it has one bucket and is never split/, '... with the reason') or diag($err);
	is(q1($n1, "SELECT count(*) FROM kingres.bucket_split WHERE colocation_id = $cs"), 0, '... and no split was recorded');
}

# ---------------------------------------------------------------------------
# C. plans and checks
# ---------------------------------------------------------------------------
is(q1($n1, "SELECT count(*) FROM kingres.plan_rebalance() WHERE colocation_id = $cs"), 0, 'plan_rebalance does not move the bucket of a schema');
is(q1($n1, "SELECT count(*) FROM kingres.rebalance_plan() WHERE colocation_id = $cs"), 0, 'rebalance_plan does not either');
is(q1($n1, "SELECT count(*) FROM kingres.plan_rebalance('by_disk_size', 0) WHERE colocation_id = $cs"), 0, '... nor by disk size');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster has nothing to say about a cluster with a distributed schema') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));
is(q1($n1, q{SELECT sum(buckets) FROM kingres.drain_status()}), 5, 'drain_status counts the bucket of the schema among the buckets of its group');

# ---------------------------------------------------------------------------
# D. the view, and a split of a hash group next to a schema
# ---------------------------------------------------------------------------
is(q1($n1, q{SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid = 'kingres.buckets'::regclass AND attnum > 0}),
	'table_name,bucket,partition_name,group_id,node_id,state,partition_size,modulus', 'the view keeps the modulus as its last column');
is(q1($n2, q{SELECT bucket || ':' || coalesce(partition_name, '-') || ':' || modulus FROM kingres.buckets WHERE table_name = 'ea.t'::regclass}), '0:-:1', 'a schema table: bucket 0, no partition, modulus 1');
{
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($ch, 0)");
	is($rc, 0, 'a bucket of a hash group is split while a distributed schema exists') or diag($err);
	is(q1($n2, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $ch"), 5, '... the group has five buckets on every node');
	is(q1($n3, q{SELECT count(*) FROM public.h}), 200, '... the rows are all there');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, '... and check_cluster is quiet') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));
	q1($n1, q{INSERT INTO ea.t SELECT g, 'x' FROM generate_series(1, 30) g});
	is(q1($n3, q{SELECT count(*) FROM ea.t}), 30, 'the schema is read from every node as before');
}
for my $n (@nodes) { is(prepared_count($n), 0, 'nothing is prepared on ' . $n->name); }

done_testing();
