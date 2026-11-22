# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/251_add_node_staging_leftovers.pl: add_node() and what an earlier call left on the new node (reviews P12R-04 and P12R-05).
#
#   A. a call fails after its batches; the last distributed schemas of the cluster are made local; the next call registers the node with
#      none of the schemas and none of the progress rows of the first call
#   B. a role that the new node lacks is an error of the call, with the hint to repeat it, not a schema that is deferred to the last transaction

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('sl', 3);
my ($n1) = @nodes;
sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }
sub new_node
{
	my ($name) = @_;
	my $n = PostgreSQL::Test::Cluster->new($name);
	$n->init;
	$n->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_shard'\nmax_prepared_transactions = 10\n");
	$n->start;
	$n->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
	return $n;
}

q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});
q1($n1, q{CREATE SCHEMA s1; CREATE TABLE s1.t (id int PRIMARY KEY, v text); CREATE SCHEMA s2; CREATE TABLE s2.t (id int PRIMARY KEY)});

# A
my $n4 = new_node('sl4');
{
	my ($rc, $out, $err) = run_sql($n1, sprintf("SET kingres_shard.fault_injection = 'add_node_batch_done:error'; SELECT kingres.add_node('%s', %d)", $n4->host, $n4->port));
	isnt($rc, 0, 'A: a call that fails after its batch');
	is(q1($n4, q{SELECT count(*) FROM pg_namespace WHERE nspname IN ('s1', 's2')}), 2, 'A: the new node holds the two schemas of the batch');
	is(q1($n4, q{SELECT count(*) FROM kingres.add_node_progress}), 2, 'A: ... and their progress rows');
	q1($n1, q{SELECT kingres.undistribute_schema('s1'); SELECT kingres.undistribute_schema('s2')});
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema}), 0, 'A: the cluster has no distributed schema any more');
	($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $n4->host, $n4->port));
	is($rc, 0, 'A: the next call succeeds') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM kingres.node}), 4, 'A: the node is registered');
	is(q1($n4, q{SELECT count(*) FROM pg_namespace WHERE nspname IN ('s1', 's2')}), 0, 'A: the schemas of the first call are gone from the node');
	is(q1($n4, q{SELECT count(*) FROM kingres.add_node_progress}), 0, 'A: ... and so are the progress rows');
	is(prepared_count($_), 0, 'A: ' . $_->name . ': no prepared transaction') for (@nodes, $n4);
}

# B
{
	q1($_, q{CREATE ROLE ghost; CREATE ROLE ghost2}) for (@nodes, $n4);
	q1($n1, q{CREATE SCHEMA s3; CREATE TABLE s3.t (id int PRIMARY KEY); CREATE FUNCTION s3.f() RETURNS int LANGUAGE sql AS 'SELECT 1'; ALTER FUNCTION s3.f() OWNER TO ghost; ALTER DEFAULT PRIVILEGES FOR ROLE ghost IN SCHEMA s3 GRANT SELECT ON TABLES TO ghost2});
	my $n5 = new_node('sl5');
	my ($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $n5->host, $n5->port));
	isnt($rc, 0, 'B: a role that the new node lacks refuses the call');
	# the roles of the schemas are looked up on the node before a schema is made there, so the answer does not depend on the language of its messages (P12F-02)
	like($err, qr/the roles ghost, ghost2 own objects of the distributed schemas or hold privileges on them and do not exist on the new node/, 'B: ... naming the role') or diag($err);
	like($err, qr/HINT:.*Create the roles on the new node .* then repeat add_node\(\)/s, 'B: ... and the hint to create it and repeat the call') or diag($err);
	is(q1($n5, q{SELECT count(*) FROM pg_namespace WHERE nspname = 's3'}), 0, 'B: no schema was made on the node');
	is(q1($n1, q{SELECT count(*) FROM kingres.node}), 4, 'B: the node is not registered');
	q1($n5, q{CREATE ROLE ghost});
	# a role that only a default privilege of the schema names is found the same way
	($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $n5->host, $n5->port));
	isnt($rc, 0, 'B: a role of a default privilege that the node lacks refuses the call');
	like($err, qr/the roles ghost2 own objects/, 'B: ... naming it') or diag($err);
	q1($n5, q{CREATE ROLE ghost2});
	($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $n5->host, $n5->port));
	is($rc, 0, 'B: with the role the call succeeds') or diag($err);
	like($err, qr/1 distributed schemas are on the node in 1 batches \(\d+ were made by an earlier call, 0 are made in the last transaction\)/, 'B: the schema was made in a batch');
	is(q1($n5, q{SELECT s3.f()}), 1, 'B: the function is on the node');
	$n5->stop('immediate');
	$n4->stop('immediate');
}

done_testing();
