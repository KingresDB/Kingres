# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/006_ddl_and_concurrency.pl
#   Review 2026-11-14, finding 3 (utility statements on distributed tables on
#   a cluster with several shard groups must not be applied to one node only)
#   and finding 4 (catalog-changing functions are serialized by one lock and
#   two primaries in one group are impossible).

use strict;
use warnings FATAL => 'all';
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

sub make_node
{
	my ($name) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init;
	$node->append_conf('postgresql.conf',
		"shared_preload_libraries = 'kingres_shard'
max_prepared_transactions = 20
kingres_shard.recovery_interval = 0");
	$node->start;
	return $node;
}

my @nodes = map { make_node("d$_") } (1 .. 3);
my ($n1, $n2, $n3) = @nodes;
# Nodes that join later.
my @spare = map { make_node("e$_") } (1 .. 4);

$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres',
	sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
for my $n ($n2, $n3)
{
	$n1->safe_psql('postgres',
		sprintf("SELECT kingres.add_node('%s', %d)", $n->host, $n->port));
}

sub run_sql
{
	my ($node, $sql, %extra) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql('postgres', $sql, stdout => \$stdout,
		stderr => \$stderr, %extra);
	return ($rc, $stdout, $stderr);
}

# ---------------------------------------------------------------------------
# Finding 4.
# ---------------------------------------------------------------------------
my ($e1, $e2, $e3, $e4) = @spare;

# The schema makes two primaries in one group impossible.
{
	my ($rc, $out, $err) = run_sql($n1,
		"INSERT INTO kingres.node (node_id, group_id, host, port, role) VALUES (90, 0, 'dup.invalid', 5432, 'primary')");
	like($err, qr/duplicate key value violates unique constraint/,
		'finding 4: a second primary in a group is a constraint violation');
	($rc, $out, $err) = run_sql($n1,
		"INSERT INTO kingres.node (node_id, group_id, host, port, role) VALUES (90, 0, 'standby.invalid', 5432, 'standby')");
	is($err, '', 'finding 4: standbys are not limited');
	$n1->safe_psql('postgres', 'DELETE FROM kingres.node WHERE node_id = 90');
}

# Every catalog-changing function holds the cluster lock until the end of its
# transaction.
sub lock_held
{
	return $n1->safe_psql('postgres', q{
		SELECT count(*) FROM pg_locks
		 WHERE locktype = 'relation' AND relation = 'kingres.map_version'::regclass
		   AND mode = 'ExclusiveLock' AND granted});
}
for my $case (
	[ 'sync_catalog', 'SELECT kingres.sync_catalog()' ],
	[ 'create_distributed_table', "CREATE TABLE public.lk (id int PRIMARY KEY); SELECT kingres.create_distributed_table('public.lk', 'id', bucket_count => 3)" ],
	[ 'add_node', sprintf("SELECT kingres.add_node('%s', %d)", $e4->host, $e4->port) ],
  )
{
	my $s = $n1->background_psql('postgres', on_error_stop => 0);
	$s->query_safe("BEGIN;\n$case->[1];\nSELECT 'held';");
	is(lock_held(), '1', "finding 4: $case->[0] holds the cluster lock until the transaction ends");
	$s->query_safe('ROLLBACK;');
	$s->quit;
	is(lock_held(), '0', "finding 4: and releases it at rollback ($case->[0])");
}
# remove_node on a spare that was added by the rolled back add_node: nothing
# remains of it.
is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), '3',
	'finding 4: rolled back functions left no node behind');

# Two add_node calls at the same time without a group: different groups.
{
	local $PostgreSQL::Test::Utils::timeout_default = 30;
	my $a = $n1->background_psql('postgres', on_error_stop => 0);
	my $b = $n1->background_psql('postgres', on_error_stop => 0);
	$a->query_safe(sprintf("BEGIN; SELECT kingres.add_node('%s', %d);", $e1->host, $e1->port));
	$b->query_until(qr/started/, sprintf(
			"\\echo started\nSELECT kingres.add_node('%s', %d) AS second;\n",
			$e2->host, $e2->port));
	ok($n1->poll_query_until('postgres',
			q{SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%add_node%'},
			'1'),
		'finding 4: the second add_node waits for the first');
	$a->query_safe('COMMIT;');
	$b->query_until(qr/finished/, "\\echo finished\n");
	$a->quit;
	$b->quit;
	is($n1->safe_psql('postgres',
			"SELECT count(DISTINCT group_id) || ':' || count(*) FROM kingres.node WHERE role = 'primary'"),
		'5:5', 'finding 4: concurrent add_node calls got different groups');
}

# Two add_node calls for the same group at the same time: one wins.
{
	local $PostgreSQL::Test::Utils::timeout_default = 30;
	my $a = $n1->background_psql('postgres', on_error_stop => 0);
	my $b = $n1->background_psql('postgres', on_error_stop => 0);
	$a->query_safe(sprintf("BEGIN; SELECT kingres.add_node('%s', %d, 9);", $e3->host, $e3->port));
	$b->query_until(qr/started/, sprintf(
			"\\echo started\nSELECT kingres.add_node('%s', %d, 9);\n",
			$e4->host, $e4->port));
	ok($n1->poll_query_until('postgres',
			q{SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%add_node%'},
			'1'),
		'finding 4: the second add_node for the same group waits');
	$a->query_safe('COMMIT;');
	$b->query_until(qr/finished/, "\\echo finished\n");
	like($b->{stderr}, qr/group 9 already has a primary/,
		'finding 4: the second add_node for the same group fails cleanly');
	$a->quit;
	$b->quit;
	is($n1->safe_psql('postgres',
			"SELECT count(*) FROM kingres.node WHERE group_id = 9 AND role = 'primary'"),
		'1', 'finding 4: the group has exactly one primary');
}


# ---------------------------------------------------------------------------
# Finding 3.
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 6);
});
# One row per group, written by the node that owns it.
my %key;
for my $g (0 .. 2)
{
	$key{$g} = $n1->safe_psql('postgres', qq{
		SELECT k FROM generate_series(1, 1000) k
		 WHERE (SELECT m.group_id FROM kingres.bucket_map m
		         WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, k)) = $g
		 LIMIT 1});
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($key{$g}, 'row-$g')");
}

sub rows_everywhere
{
	return join ',', map {
		$nodes[$_]->safe_psql('postgres',
			"SELECT val FROM public.kv WHERE id = $key{$_}")
	} 0 .. 2;
}
is(rows_everywhere(), 'row-0,row-1,row-2', 'one row per node before the DDL tests');

# Since phase 2a the DDL statements of the list are propagated to every node
# when they run on the map group primary (t/012_ddl_propagation.pl), and refused
# on any other node, where they would change the table there only.  COPY FROM
# is refused everywhere: its rows would land on the partitions of one node.
for my $case (
	[ 'TRUNCATE', 'TRUNCATE public.kv' ],
	[ 'ALTER TABLE', 'ALTER TABLE public.kv ADD COLUMN extra int' ],
	[ 'DROP TABLE', 'DROP TABLE public.kv' ],
	[ 'CREATE INDEX', 'CREATE INDEX kv_val_idx ON public.kv (val)' ],
  )
{
	my ($rc, $out, $err) = run_sql($n3, $case->[1]);
	like($err, qr/$case->[0] on a distributed or reference table must run on the map group primary/,
		"finding 3: $case->[0] on " . $n3->name . ' is refused');
	like($err, qr/HINT:  Run it on node 1/, "the hint names the map group primary");
}
for my $n ($n1, $n3)
{
	my ($rc, $out, $err) = run_sql($n, "COPY public.kv FROM '/dev/null'");
	like($err, qr/COPY FROM is not supported on distributed tables yet/,
		"finding 3: COPY FROM on " . $n->name . ' is refused');
}
is(rows_everywhere(), 'row-0,row-1,row-2',
	'finding 3: no row was removed from any node by the refused statements');
is($n2->safe_psql('postgres',
		"SELECT count(*) FROM pg_attribute WHERE attrelid = 'public.kv'::regclass AND attname = 'extra'"),
	'0', 'finding 3: no column was added anywhere');
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.dist_table"), '1',
	'finding 3: the table is still distributed');

for my $sql ('VACUUM public.kv', 'ANALYZE public.kv', 'REINDEX TABLE public.kv',
	'VACUUM (ANALYZE) public.kv')
{
	my ($rc, $out, $err) = run_sql($n2, $sql);
	is($err, '', "finding 3: the per-node maintenance command $sql works");
}

# A superuser who repeats a statement on every node can switch routing off.
for my $n (@nodes)
{
	$n->safe_psql('postgres',
		'SET kingres_shard.enable_routing = off; TRUNCATE public.kv');
}
is(join(',', map { $_->safe_psql('postgres', 'SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.kv') } @nodes),
	'0,0,0', 'with routing off a superuser can repeat a statement on every node');

foreach my $n (@nodes, @spare)
{
	$n->stop;
}

done_testing();
