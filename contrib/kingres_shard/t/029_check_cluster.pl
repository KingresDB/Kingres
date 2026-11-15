# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# kingres.check_cluster() (design/shard-phase4.md, "Checking a cluster"): a clean
# cluster reports nothing; each kind of inconsistency, made by hand, is reported
# with its check name and node.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @nodes = make_cluster('cc', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.t (id int PRIMARY KEY, v int);
	SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 6);
});

# The problems as "check:node" words, sorted.
sub problems
{
	my ($older) = @_;
	$older //= '5 minutes';
	return $n1->safe_psql('postgres', qq{
		SELECT coalesce(string_agg(check_name || ':' || node_id, ' ' ORDER BY check_name, node_id), '')
		  FROM kingres.check_cluster('$older')});
}

# Statements that change one node only, as an administrator would by hand.
sub raw { my ($node, $sql) = @_; $node->safe_psql('postgres', "SET kingres_shard.enable_routing = off;\n$sql"); }

is(problems(), '', 'a clean cluster has no problem');

# The catalog of a node differs.
$n2->safe_psql('postgres', "UPDATE kingres.node SET server_group = 'rack-x' WHERE node_id = 3");
is(problems(), 'catalog_differs:2', 'a catalog that differs from the map group\'s');
$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
is(problems(), '', 'sync_catalog repairs it');

# A partition missing on its owner (detached by hand): the table is still there.
my $b = 1;
my $owner = $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b");
my $on = $nodes[$owner];
raw($on, "ALTER TABLE public.t DETACH PARTITION public.t_b0001");
is(problems(), 'partition_missing:' . ($owner + 1) . ' stray_bucket_table:' . ($owner + 1),
	'a bucket partition detached by hand: missing, and its table is a stray');
raw($on, "ALTER TABLE public.t ATTACH PARTITION public.t_b0001 FOR VALUES WITH (MODULUS 6, REMAINDER 1)");
is(problems(), '', 'attached again');

# A partition on a node that does not own the bucket.
my $other = $nodes[($owner + 1) % 3];
raw($other, "CREATE TABLE public.t_b0001 PARTITION OF public.t FOR VALUES WITH (MODULUS 6, REMAINDER 1)");
is(problems(), 'partition_misplaced:' . ((($owner + 1) % 3) + 1), 'a partition of a bucket that belongs elsewhere');
raw($other, "DROP TABLE public.t_b0001");
is(problems(), '', 'dropped again');

# Leftovers of a move.
$n3->safe_psql('postgres', 'CREATE TABLE kingres.orphan_99_1 (a int)');
is(problems(), 'orphan_table:3', 'an orphan table that no move accounts for');
$n3->safe_psql('postgres', 'DROP TABLE kingres.orphan_99_1');
$n2->safe_psql('postgres', "CREATE TABLE public.t_b0007 (id int PRIMARY KEY, v int)");
is(problems(), 'stray_bucket_table:2', 'a table with the name of a bucket partition that is not one');
$n2->safe_psql('postgres', "DROP TABLE public.t_b0007");
$n2->safe_psql('postgres', "SELECT pg_create_logical_replication_slot('ks_move_77', 'pgoutput');
	CREATE PUBLICATION ks_move_77 FOR TABLE kingres.move_marker;
	CREATE SUBSCRIPTION ks_move_77 CONNECTION 'host=/nonexistent dbname=x' PUBLICATION p WITH (connect = false)");
is(problems(), 'publication:2 replication_slot:2 subscription:2', 'the slot, publication and subscription of a move that does not exist');
$n2->safe_psql('postgres', "ALTER SUBSCRIPTION ks_move_77 SET (slot_name = NONE); DROP SUBSCRIPTION ks_move_77; DROP PUBLICATION ks_move_77;
	SELECT pg_drop_replication_slot('ks_move_77')");
is(problems(), '', 'dropped again');

# A prepared transaction that nobody resolved.
$n3->safe_psql('postgres', "BEGIN; CREATE TABLE public.zz (a int); PREPARE TRANSACTION 'zz'");
is(problems(), '', 'a young prepared transaction is not reported');
is(problems('0 seconds'), 'prepared_transaction:3', 'an old one is');
$n3->safe_psql('postgres', "ROLLBACK PREPARED 'zz'");

# The records of moves against the state of the buckets.
$n1->safe_psql('postgres', "UPDATE kingres.bucket_map SET state = 'moving' WHERE colocation_id = 1 AND bucket = 2");
like(problems(), qr/bucket_moving_without_move:1/, 'a bucket marked moving with no move');
$n1->safe_psql('postgres', "UPDATE kingres.bucket_map SET state = 'active'; SELECT kingres.sync_catalog()");
$n1->safe_psql('postgres', "INSERT INTO kingres.bucket_move (colocation_id, bucket, source_group, target_group, phase, started_at, updated_at)
	VALUES (1, 2, 0, 1, 'copying', now(), now())");
is(problems(), 'move_without_moving_bucket:1', 'a running move whose bucket is not moving');
$n1->safe_psql('postgres', "DELETE FROM kingres.bucket_move");
is(problems(), '', 'removed');

# An unreachable node.
$n3->stop('immediate');
is(problems(), 'node_unreachable:3', 'a node that does not answer');
$n3->start;
is(problems(), '', 'back again');

# Not on a node other than the map group primary; not for an ordinary role.
my ($rc, $out, $err) = run_sql($n2, 'SELECT * FROM kingres.check_cluster()');
like($err, qr/map group primary/, 'check_cluster runs on the map group primary');
$n1->safe_psql('postgres', 'CREATE ROLE plain LOGIN');
($rc, $out, $err) = run_sql($n1, 'SET ROLE plain; SELECT * FROM kingres.check_cluster()');
like($err, qr/permission denied/, 'and is for superusers');

done_testing();
