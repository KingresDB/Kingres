# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Hook chaining (coding-standards.md, 1.7): kingres_shard installs a planner
# hook and an executor hook; pg_stat_statements and auto_explain use the same
# hooks.  Both load orders must work, and the other extensions must see the
# statements that kingres_shard routes.

use strict;
use warnings FATAL => 'all';
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

sub make_node
{
	my ($name, $libs) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init;
	$node->append_conf('postgresql.conf', qq{
shared_preload_libraries = '$libs'
max_prepared_transactions = 20
auto_explain.log_min_duration = 0
auto_explain.log_analyze = on
pg_stat_statements.track = all
});
	$node->start;
	return $node;
}

# Node a loads the other extensions first, node b loads kingres_shard first.
my $a = make_node('ha', 'pg_stat_statements,auto_explain,kingres_shard');
my $b = make_node('hb', 'kingres_shard,auto_explain,pg_stat_statements');

for my $n ($a, $b)
{
	$n->safe_psql('postgres', 'CREATE EXTENSION pg_stat_statements');
}
$a->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$a->safe_psql('postgres',
	sprintf("SELECT kingres.init_cluster('%s', %d)", $a->host, $a->port));
$a->safe_psql('postgres',
	sprintf("SELECT kingres.add_node('%s', %d)", $b->host, $b->port));
$b->safe_psql('postgres', 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements');

$a->safe_psql('postgres', q{
	CREATE TABLE public.t (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 4);
});

# Keys of each group.
my %key;
for my $g (0, 1)
{
	$key{$g} = $a->safe_psql('postgres', qq{
		SELECT k FROM generate_series(1, 1000) k
		 WHERE (SELECT m.group_id FROM kingres.bucket_map m
		         WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, k)) = $g
		 LIMIT 1});
}

$a->safe_psql('postgres', 'SELECT pg_stat_statements_reset()');
$b->safe_psql('postgres', 'SELECT pg_stat_statements_reset()');

# Each node writes and reads a key of the other node: both go through the
# routing hooks and through the other extensions' hooks.
$a->safe_psql('postgres', "INSERT INTO public.t VALUES ($key{1}, 'one')");
$b->safe_psql('postgres', "INSERT INTO public.t VALUES ($key{0}, 'zero')");
is($a->safe_psql('postgres', "SELECT v FROM public.t WHERE id = $key{1}"), 'one',
	'node a (other extensions first) routes a read');
is($b->safe_psql('postgres', "SELECT v FROM public.t WHERE id = $key{0}"), 'zero',
	'node b (kingres_shard first) routes a read');
is($a->safe_psql('postgres', "SELECT v FROM public.t WHERE id = $key{0}"), 'zero',
	'node a reads its local row');

for my $n ($a, $b)
{
	my $name = $n->name;
	is( $n->safe_psql('postgres',
			"SELECT sum(calls) >= 2 FROM pg_stat_statements WHERE query LIKE '%public.t%'"),
		't', "pg_stat_statements on $name counted the statements on the distributed table");
	is( $n->safe_psql('postgres',
			"SELECT sum(rows) >= 2 FROM pg_stat_statements WHERE query LIKE '%FROM public.t%'"),
		't', "pg_stat_statements on $name counted the returned rows");
}

# auto_explain logged the plan of the routed statement on the originating node.
my $log = slurp_file($a->logfile);
like($log, qr/Custom Scan \(Kingres Remote Statement\)/,
	'auto_explain on node a logged the remote statement plan');
$log = slurp_file($b->logfile);
like($log, qr/Custom Scan \(Kingres Remote Statement\)/,
	'auto_explain on node b logged the remote statement plan');

$a->stop;
$b->stop;
done_testing();
