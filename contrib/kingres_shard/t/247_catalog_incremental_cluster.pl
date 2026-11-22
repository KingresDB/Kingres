# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/247_catalog_incremental_cluster.pl
#
# Phase 11, step 12 (b): the incremental cache of the catalog, on three nodes.  A backend makes the entry of a schema table (or a distributed
# schema) when a statement asks for it, and the trigger of a catalog change invalidates the distributed tables that the change touches, not
# every one; kingres_shard.catalog_incremental = off is the fallback that invalidates every table.  The counters are
# kingres.internal_catalog_stats() (loads of the cache, lazy lookups, relcache invalidations sent for distributed tables).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(sleep);
use KingresShardTest qw(make_cluster run_sql start_psql finish_psql);

plan skip_all => 'tests the incremental cache itself (KS_CATALOG_FULL=1 turns it off)' if $ENV{KS_CATALOG_FULL};

my $nsch = 30;
my @nodes = make_cluster('ci', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});
q1($n1, q{CREATE TABLE public.ref (id int PRIMARY KEY); SELECT kingres.create_reference_table('public.ref')});
my $sql = '';
for my $i (1 .. $nsch)
{
	$sql .= "CREATE SCHEMA tenant_$i;\n";
	$sql .= "CREATE TABLE tenant_$i.t$_ (id int PRIMARY KEY, v text);\n" for 1 .. 2;
}
q1($n1, $sql);
is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE method = 'schema'}), $nsch * 2, 'the schema tables are registered');

# A. a new backend makes the entries it needs, not one for every schema table of the database
{
	my $out = q1($n2, q{SELECT count(*) FROM tenant_1.t1; SELECT lazy_lookups FROM kingres.internal_catalog_stats()});
	my ($cnt, $lazy) = split /\n/, $out;
	is($cnt, 0, 'a statement on a schema table of another node answers');
	cmp_ok($lazy, '<=', 6, "the backend asked the catalog for $lazy entries, not for the $nsch schemas and " . $nsch * 2 . " tables");
	cmp_ok($lazy, '>=', 1, '... but it did ask');
}
# the answers of one session: a table that is a schema table, one that is not, one that appears later
{
	my $out = q1($n1, q{
		SELECT count(*) FROM public.ref;
		CREATE TABLE public.plain (id int);
		INSERT INTO public.plain VALUES (1), (2);
		SELECT count(*) FROM public.plain;
		CREATE SCHEMA late_s;
		CREATE TABLE late_s.t (id int PRIMARY KEY);
		INSERT INTO late_s.t VALUES (1), (2), (3);
		SELECT count(*) FROM late_s.t;
		SELECT count(*) FROM tenant_2.t1;
	});
	is($out, "0\n2\n3\n0", 'a reference table, a local table, a table of a schema made in the same session, a schema table: each answers');
	is(q1($n2, q{SELECT count(*) FROM late_s.t}), 3, '... and another node sees the table of the schema that was made');
	is(q1($n3, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'late_s'::regnamespace}), 1, '... and the schema is in the catalog of the third node');
}
# a long session whose cache says "not a schema table" for a table that becomes one (the table is local to node 3, the schema is held by node 1's group)
{
	my $g3 = q1($n1, q{SELECT group_id FROM kingres.node WHERE node_id = (SELECT node_id FROM kingres.local_node)});
	q1($n1, q{CREATE TABLE public.plain3 (id int); INSERT INTO public.plain3 VALUES (1), (2)});
	q1($n1, "SET kingres_shard.schema_placement_group = $g3; CREATE SCHEMA mover_s");
	my $h = $n1->background_psql('postgres', on_error_stop => 0);
	my $first = $h->query('SELECT count(*) FROM public.plain3');
	sleep(1);
	my ($rc, $o, $e) = run_sql($n1, q{ALTER TABLE public.plain3 SET SCHEMA mover_s});
	is($rc, 0, 'the table moves into the schema') or diag($e);
	my $second = $h->query('SELECT count(*) FROM mover_s.plain3');
	$h->quit;
	is("$first,$second", '2,2', 'a session that had the table as a plain table answers for it as a schema table');
	is(q1($n2, q{SELECT count(*) FROM mover_s.plain3}), 2, '... and so does another node');
}

# B. what a catalog change invalidates
sub invalidations_of
{
	my ($sqlbody, $pre) = @_;
	my $out = q1($n1, ($pre // '') . q{SELECT invalidations AS i0 FROM kingres.internal_catalog_stats() \gset} . "\n" . $sqlbody
		. q{SELECT invalidations - :i0 FROM kingres.internal_catalog_stats();});
	return (split /\n/, $out)[-1];
}
{
	my $dist = q1($n1, q{SELECT count(*) FROM kingres.dist_table});
	my $d1 = invalidations_of(q{CREATE TABLE tenant_3.extra (id int PRIMARY KEY);});
	cmp_ok($d1, '<=', 4, "a CREATE TABLE in one schema sends $d1 invalidations (the database has $dist distributed tables)");
	my $d2 = invalidations_of(q{CREATE TABLE tenant_4.extra (id int PRIMARY KEY);}, q{SET kingres_shard.catalog_incremental = off;});
	cmp_ok($d2, '>=', $dist, "with kingres_shard.catalog_incremental = off every table is invalidated: $d2");
	my $d3 = invalidations_of(q{CREATE TABLE public.plain2 (id int); SELECT kingres.create_reference_table('public.plain2');});
	cmp_ok($d3, '<=', 4, "registering a reference table invalidates that table only: $d3");
	# a change of the placement of one group invalidates the tables of that group, and only them
	my $c6 = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'tenant_6'::regnamespace});
	my $d4 = invalidations_of("UPDATE kingres.bucket_map SET state = state WHERE colocation_id = $c6;");
	cmp_ok($d4, '>=', 2, "a change of the bucket map of a schema invalidates its tables: $d4");
	cmp_ok($d4, '<=', 4, '... and not the others');
	my $d5 = invalidations_of("UPDATE kingres.colocation SET placement_epoch = placement_epoch WHERE colocation_id = $c6;");
	cmp_ok($d5, '>=', 2, "a change of the group row invalidates its tables: $d5");
	cmp_ok($d5, '<=', 4, '... and not the others');
}

# B2. applying a whole catalog (sync_catalog, add_node) writes dist_table with one statement, so the trigger of the catalog changes fires a few times, not once per table
{
	my $out = q1($n2, q{
		BEGIN;
		SELECT n_tup_upd AS u0 FROM pg_stat_xact_user_tables WHERE relid = 'kingres.map_version'::regclass \gset
		SELECT kingres.internal_apply_catalog(kingres.internal_catalog_snapshot());
		SELECT n_tup_upd - :u0 FROM pg_stat_xact_user_tables WHERE relid = 'kingres.map_version'::regclass;
		ROLLBACK;
	});
	my $fired = (split /\n/, $out)[-1];
	cmp_ok($fired, '<=', 30, "applying a catalog of " . ($nsch * 2) . " schema tables fires the trigger of the catalog tables $fired times");
}

# C. the plan of a prepared statement follows a move of its schema
{
	my $own = q1($n1, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'tenant_5'});
	my $target = q1($n1, qq{SELECT min(group_id) FROM kingres.node WHERE group_id <> $own AND role = 'primary'});
	q1($n1, q{INSERT INTO tenant_5.t1 SELECT g, 'x' || g FROM generate_series(1, 7) g});
	my $holder = start_psql($n2, q{SELECT pg_advisory_lock(4242); SELECT pg_sleep(60)});
	for (1 .. 50)
	{
		last if q1($n2, q{SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted}) > 0;
		sleep(0.1);
	}
	my $b = start_psql($n3, q{SET plan_cache_mode = force_generic_plan; PREPARE p AS SELECT count(*) FROM tenant_5.t1; EXECUTE p; EXECUTE p; SELECT pg_advisory_lock(4242); EXECUTE p});
	sleep(1.5);
	q1($n1, "SELECT kingres.move_schema('tenant_5', $target)");
	q1($n2, q{SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE '%pg_advisory_lock(4242)%' AND pid <> pg_backend_pid()});
	my ($out, $err) = finish_psql($b);
	finish_psql($holder);
	my @lines = grep { /^\d+$/ } split /\n/, $out;
	is(join(',', @lines), '7,7,7', 'a prepared statement with a cached generic plan answers from the new owner after the move') or diag($out . $err);
	is(q1($n1, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'tenant_5'}), $target, '... which is the group the schema moved to');
}

# D. the lookups take the primary key from the table they open (review P12R-03)
{
	my $h = $n3->background_psql('postgres', on_error_stop => 0);
	is($h->query('SELECT count(*) FROM tenant_1.t1'), 0, 'D: a backend that has the catalog loaded');
	q1($n1, q{REINDEX (CONCURRENTLY) TABLE kingres.dist_schema; REINDEX (CONCURRENTLY) TABLE kingres.dist_table});
	is($h->query('SELECT count(*) FROM tenant_9.t1'), 0, 'D: after REINDEX CONCURRENTLY of the two tables it still finds a schema table it had not looked up');
	is($h->query(q{INSERT INTO tenant_10.t1 VALUES (7, 'x') RETURNING id}), 7, 'D: ... and routes a write');
	q1($n1, q{ALTER INDEX kingres.dist_table_pkey RENAME TO ks_renamed_pk});
	is($h->query('SELECT count(*) FROM tenant_11.t1'), 0, 'D: an index that has another name is the same index');
	q1($n1, q{ALTER INDEX kingres.ks_renamed_pk RENAME TO dist_table_pkey});
	$h->quit;
	# a table of the extension without its primary key is an error, never "a local table" (in a transaction that is rolled back)
	my ($rc, $out, $err) = run_sql($n1, q{BEGIN; ALTER TABLE kingres.dist_schema DROP CONSTRAINT dist_schema_pkey; SELECT count(*) FROM tenant_12.t1; ROLLBACK});
	like($err, qr/has no primary key/, 'D: without the primary key of dist_schema a statement on a schema table is refused with the reason, not answered from a local copy') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM tenant_12.t1}), 0, 'D: after the rollback it answers again');
}

done_testing();
