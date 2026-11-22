# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/248_add_node_batches_cluster.pl
#
# Phase 11, step 12 (a): add_node() makes the distributed schemas on the new node in batches, each a transaction of its own, so that no
# transaction holds more locks than kingres_shard.add_node_batch_statements allows.  The new node here has a lock table so small that one
# transaction cannot make the schemas of the cluster; a repeated call picks up what an earlier one finished; a schema that reads a reference
# table is made in the last transaction; nothing of the new node is in the catalog before the last transaction commits.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(sleep);
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql);

my $nsch = $ENV{KS_BATCH_SCHEMAS} // 150;
my @nodes = make_cluster('ab', 3);
my ($n1, $n2, $n3) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});
q1($n1, q{CREATE TABLE public.ref (id int PRIMARY KEY, name text); SELECT kingres.create_reference_table('public.ref'); INSERT INTO public.ref VALUES (1, 'r1')});
my $sql = '';
for my $i (1 .. $nsch)
{
	$sql .= "CREATE SCHEMA tenant_$i;\n";
	$sql .= "CREATE TABLE tenant_$i.t$_ (id serial PRIMARY KEY, v text);\nCREATE INDEX ON tenant_$i.t$_ (v);\n" for 1 .. 3;
}
q1($n1, $sql);
# a schema with rows, a sequence, a view, a function; an empty one; one that reads the reference table (made in the last transaction)
q1($n1, q{
	CREATE SCHEMA data_s;
	CREATE SEQUENCE data_s.sq START 50;
	CREATE TABLE data_s.t (id serial PRIMARY KEY, v text);
	CREATE VIEW data_s.vw AS SELECT id, v FROM data_s.t WHERE id > 0;
	CREATE FUNCTION data_s.f(x int) RETURNS int LANGUAGE sql AS 'SELECT x + 1';
	INSERT INTO data_s.t (v) SELECT 'row ' || g FROM generate_series(1, 9) g;
	CREATE SCHEMA empty_s;
	CREATE SCHEMA fk_s;
	CREATE TABLE fk_s.t (id int PRIMARY KEY, r int REFERENCES public.ref (id));
	CREATE VIEW fk_s.vw AS SELECT t.id, ref.name FROM fk_s.t JOIN public.ref ON ref.id = t.r;
	INSERT INTO fk_s.t VALUES (1, 1);
});
my $nschemas = $nsch + 3;
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema}), $nschemas, 'the schemas are distributed');

# the new node: a lock table that cannot hold the schemas of the cluster in one transaction
my $n4 = PostgreSQL::Test::Cluster->new('ab4');
$n4->init;
$n4->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_shard'\nmax_prepared_transactions = 5\nmax_locks_per_transaction = 32\nmax_connections = 10\n"
	. "max_wal_senders = 0\nmax_worker_processes = 4\nautovacuum_max_workers = 1\nautovacuum = off\n");
$n4->start;
$n4->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
my $host = $n4->host;
my $port = $n4->port;
my $add = "SELECT kingres.add_node('$host', $port)";

# everything the nodes of the cluster can see of the new node and of the schemas: the same on every old node, equal before and after a failed call
sub old_nodes_view
{
	return join('|', map { q1($_, q{SELECT (SELECT version FROM kingres.map_version) || ':' || (SELECT count(*) FROM kingres.node) || ':' || (SELECT count(*) FROM kingres.dist_schema) || ':' || (SELECT count(*) FROM kingres.dist_table)}) } @nodes)
		. '|' . q1($n1, q{SELECT count(*) FROM kingres.check_cluster()});
}

sub n4_state
{
	return q1($n4, q{SELECT (SELECT count(*) FROM kingres.add_node_progress) || ':' || (SELECT count(*) FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname NOT IN ('public', 'information_schema', 'kingres')) || ':' || (SELECT count(*) FROM kingres.local_node)});
}

# A. one batch for everything: the lock table of the new node is exhausted, the call fails and the node is not registered
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.add_node_batch_statements = 100000; $add");
	isnt($rc, 0, 'one batch of every schema fails on a node with a lock table this small');
	like($err, qr/out of shared memory|max_locks_per_transaction/, '... for the locks') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM kingres.node}), 3, '... and the node is not registered');
	is(n4_state(), '0:0:0', '... and the node holds no schema, no progress and no node row');
	is(prepared_count($n4), 0, '... and no prepared transaction');
}

# B. batches: a failure after the first batch leaves it done and the node unregistered
my $first;
{
	my $before = old_nodes_view();
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.add_node_batch_statements = 60; SET kingres_shard.fault_injection = 'add_node_batch_done:error'; $add");
	isnt($rc, 0, 'a call that fails after its first batch');
	is(old_nodes_view(), $before, '... and nothing changed for the nodes of the cluster (map version, nodes, schemas, tables, check_cluster)');
	is(q1($n1, q{SELECT count(*) FROM kingres.node}), 3, '... leaves the node out of the catalog');
	my ($progress, $schemas, $local) = split /:/, n4_state();
	cmp_ok($progress, '>', 0, "... with the $progress schemas of the first batch complete on the node");
	cmp_ok($progress, '<', $nschemas, '... and not all of them');
	is($schemas, $progress, '... which are the schemas of the progress rows');
	is($local, 0, '... and the node is no node of a cluster yet');
	$first = $progress;
	# the node cannot be used by the cluster: nothing of its catalog is there
	is(q1($n4, q{SELECT count(*) FROM kingres.dist_schema}), 0, '... it has no catalog of the cluster');
}

# C. between the calls the cluster changes: a schema changes and a schema goes
{
	q1($n1, q{ALTER TABLE data_s.t ADD COLUMN extra int});
	q1($n1, q{DROP SCHEMA empty_s CASCADE});
	q1($n1, q{CREATE SCHEMA tenant_new; CREATE TABLE tenant_new.t1 (id int PRIMARY KEY)});
}

# D. the repeated call: the finished batches are kept, what changed is made again, every schema is on the node, the node is registered
{
	my $gate = "$tmp/gate_batch";
	append_to_file($gate, 'x');
	my $call = start_psql($n1, "SET kingres_shard.add_node_batch_statements = 60; SET kingres_shard.fault_injection = 'add_node_batch_done:wait:$gate'; $add");
	ok(wait_for_fault_file("$gate.reached"), 'the repeated call waits after a batch, with the node half made');
	{
		# The cluster is used while the batches run: no statement reaches the new node, which is not a node of the cluster yet.
		my $nodes_now = q1($n1, q{SELECT count(*) FROM kingres.node});
		is($nodes_now, 3, 'meanwhile the node is not in the catalog');
		my $ins = join('', map { "INSERT INTO tenant_$_.t1 VALUES (1000, 'meanwhile');\n" } 1 .. $nsch);
		my ($rc2, $o2, $e2) = run_sql($n1, "SET lock_timeout = '20s';\n$ins");
		is($rc2, 0, 'meanwhile a statement on every schema is answered by the nodes of the cluster') or diag($e2);
		is(q1($n1, q{SELECT count(*) FROM tenant_3.t1 WHERE id = 1000}), 1, '... and is visible');
		is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'meanwhile check_cluster() finds nothing wrong and does not know the node');
		is(q1($n4, q{SELECT coalesce(sum(n_tup_ins + n_tup_upd + n_tup_del), 0) FROM pg_stat_user_tables WHERE schemaname LIKE 'tenant\_%'}), 0, '... and the new node received none of the rows');
		is(q1($n4, q{SELECT count(*) FROM kingres.node}), 0, '... it has no catalog: no statement is routed by it either');
	}
	unlink $gate;
	my ($callout, $err) = finish_psql($call);
	my $rc = $err =~ /ERROR/ ? 1 : 0;
	is($rc, 0, 'the repeated call succeeds') or diag($err);
	like($err, qr/add_node: \d+ distributed schemas are on the node in \d+ batches \((\d+) were made by an earlier call, (\d+) are made in the last transaction\)/, '... and says what it kept');
	my ($kept, $late) = $err =~ /\((\d+) were made by an earlier call, (\d+) are made/;
	cmp_ok($kept // 0, '>', 0, "... $kept schemas of the earlier call were kept");
	is($late // -1, 1, '... and the schema that reads the reference table was made in the last transaction');
	is(q1($n1, q{SELECT count(*) FROM kingres.node}), 4, 'the node is registered');
	is(q1($n4, q{SELECT count(*) FROM kingres.add_node_progress}), 0, '... and the progress is gone');
	is(q1($n4, q{SELECT count(*) FROM kingres.dist_schema}), $nsch + 3, 'the new node has the catalog of the schemas (one went, one came)');
	is(q1($n4, q{SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'tenant\_%'}), $nsch + 1, '... and exactly the schemas of the cluster (one came between the calls)');
	is(q1($n4, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'empty_s'}), 0, '... the schema that was dropped is gone from the node');
	is(q1($n4, q{SELECT count(*) FROM pg_attribute WHERE attrelid = 'data_s.t'::regclass AND attname = 'extra' AND NOT attisdropped}), 1, '... a schema that changed has its new structure');
	is(q1($n4, q{SELECT count(*) FROM kingres.dist_table WHERE method = 'schema'}), q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE method = 'schema'}), '... every schema table is registered');
	is(q1($n4, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM data_s.t}), 0, 'the tables of a schema are empty shells on the new node (no rows copied)');
	is(q1($n4, q{SELECT count(*) FROM pg_class WHERE relnamespace = 'data_s'::regnamespace AND relname IN ('sq', 'vw', 't', 't_pkey', 't_id_seq')}), 5, '... with the objects of the schema: sequence, view, table, indexes');
	is(q1($n4, q{SELECT data_s.f(1)}), 2, '... and the function');
	is(q1($n4, q{SELECT count(*) FROM pg_constraint WHERE conrelid = 'fk_s.t'::regclass AND contype = 'f'}), 1, 'the schema made in the last transaction has its foreign key to the reference table');
	is(q1($n4, q{SELECT count(*) FROM pg_class WHERE relnamespace = 'fk_s'::regnamespace AND relname = 'vw'}), 1, '... and its view');
	is(prepared_count($_), 0, $_->name . ': no prepared transaction') for (@nodes, $n4);
}

# E. the new node serves every schema: reads and writes from it reach the owner
{
	is(q1($n4, q{SELECT count(*) FROM data_s.t}), 9, 'a read from the new node reaches the rows of the owner');
	q1($n4, q{INSERT INTO data_s.t (v) VALUES ('from the new node')});
	is(q1($n1, q{SELECT count(*) FROM data_s.t}), 10, 'a write from the new node reaches the owner');
	is(q1($n4, q{SELECT count(*) FROM fk_s.vw}), 1, 'the view that reads the reference table answers on the new node');
	is(q1($n4, q{SELECT count(*) FROM tenant_new.t1}), 0, 'the schema that was made between the calls answers');
}

# F. the last transaction checks that the node holds every schema: one that went missing after its batch stops the registration
{
	my $n5 = PostgreSQL::Test::Cluster->new('ab5');
	$n5->init;
	$n5->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_shard'\nmax_prepared_transactions = 5\n");
	$n5->start;
	$n5->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
	my $gate = "$tmp/gate_staged";
	append_to_file($gate, 'x');
	my $h = start_psql($n1, "SET kingres_shard.fault_injection = 'add_node_staged:wait:$gate'; SELECT kingres.add_node('" . $n5->host . "', " . $n5->port . ")");
	ok(wait_for_fault_file("$gate.reached"), 'the call waits after its batches');
	q1($n5, q{DROP SCHEMA tenant_3 CASCADE});
	unlink $gate;
	my ($out, $err) = finish_psql($h);
	like($err, qr/does not hold every distributed schema/, 'the registration is refused: a schema of the cluster is missing on the node') or diag($out . $err);
	is(q1($n1, q{SELECT count(*) FROM kingres.node}), 4, '... and the node is not registered');
	is(prepared_count($_), 0, $_->name . ': no prepared transaction') for (@nodes, $n4, $n5);
	$n5->stop('immediate');
}
$n4->stop('immediate');

done_testing();
