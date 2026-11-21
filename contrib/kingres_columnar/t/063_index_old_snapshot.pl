# contrib/kingres_columnar/t/063_index_old_snapshot.pl
#
# C13-01: an index that is built or rebuilt while a snapshot older than a compaction lives has
# entries for the rows of the stripes that snapshot still sees (the stripes the compaction
# replaced), as heap indexes recently dead tuples.  Without them such a snapshot, reading
# through the index, finds none of its rows while its sequential scan finds all of them.
#  1. CREATE INDEX after the compaction.
#  2. REINDEX after the compaction and a VACUUM (the old stripes are not removable while the
#     snapshot lives).
#  3. CREATE UNIQUE INDEX after the compaction: the copies repeat the keys of the replaced rows,
#     which is no duplicate (they are not alive).
#  4. A partial index and an index with an expression.
#  5. After the old snapshot ends a VACUUM removes the entries of the replaced rows; the index
#     answers as the table does; columnar_check_indexes finds nothing.
#  6. A parallel build (the leader indexes the replaced rows).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = new_node('cios', conf => "max_parallel_maintenance_workers = 4\n");

sub fresh
{
	my ($t) = @_;
	q1($node, "CREATE TABLE $t (k int, g int, v text) USING columnar");
	q1($node, "INSERT INTO $t SELECT $_ * 20000 + g, g % 7, 'v' || g FROM generate_series(1, 20000) g") for 0 .. 3;
}

# one reader session: a snapshot taken before the compaction, queries through the index and by a scan
sub reader
{
	my ($t) = @_;
	my $s = $node->background_psql('postgres', on_error_stop => 0);
	# the snapshot is taken by a statement that does not open the table: the session holds no lock on the table or its indexes
	$s->query("BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1;");
	return ($s, 80000);
}

sub via_index
{
	my ($s, $sql) = @_;
	$s->query("SET enable_indexscan = on; SET enable_seqscan = off; SET enable_bitmapscan = off; SET kingres_columnar.enable_custom_scan = off;");
	my $r = $s->query($sql);
	chomp $r;
	return $r;
}

sub via_scan
{
	my ($s, $sql) = @_;
	$s->query("SET enable_indexscan = off; SET enable_seqscan = on; SET kingres_columnar.enable_custom_scan = on;");
	my $r = $s->query($sql);
	chomp $r;
	return $r;
}

# 1. CREATE INDEX after the compaction
fresh('c1');
my ($a, $n0) = reader('c1');
q1($node, "SELECT kingres.columnar_compact('c1', 1000000)");
q1($node, "CREATE INDEX c1_k ON c1 (k)");
my $q = "SELECT count(*) || ':' || sum(k) FROM c1 WHERE k < 500";
is(via_index($a, $q), via_scan($a, $q), '1: CREATE INDEX after a compaction: the older snapshot finds the same rows through the index as by a scan');
like(via_index($a, $q), qr/^[1-9][0-9]*:[0-9]+$/, '1: ... and they are rows');
is(q1($node, "SET enable_seqscan = off; SET enable_bitmapscan = off; SET kingres_columnar.enable_custom_scan = off; SELECT count(*) || ':' || sum(k) FROM c1 WHERE k < 500"),
   q1($node, "SET enable_indexscan = off; SELECT count(*) || ':' || sum(k) FROM c1 WHERE k < 500"), '1: a new snapshot answers as before');
$a->query("COMMIT;");
$a->quit;

# 2. REINDEX after the compaction and a VACUUM
fresh('c2');
q1($node, "CREATE INDEX c2_k ON c2 (k)");
($a, $n0) = reader('c2');
q1($node, "SELECT kingres.columnar_compact('c2', 1000000)");
q1($node, "VACUUM c2");
q1($node, "REINDEX INDEX c2_k");
$q = "SELECT count(*) || ':' || sum(k) FROM c2 WHERE k < 500";
is(via_index($a, $q), via_scan($a, $q), '2: REINDEX after a compaction and a VACUUM: the older snapshot finds its rows through the index');
like(via_index($a, $q), qr/^[1-9][0-9]*:[0-9]+$/, '2: ... and they are rows');
$a->query("COMMIT;");
$a->quit;

# 3. CREATE UNIQUE INDEX after the compaction
fresh('c3');
($a, $n0) = reader('c3');
q1($node, "SELECT kingres.columnar_compact('c3', 1000000)");
my ($rc, $out, $err) = $node->psql('postgres', "CREATE UNIQUE INDEX c3_k ON c3 (k)");
is($rc, 0, '3: CREATE UNIQUE INDEX after a compaction is no duplicate: the replaced rows repeat the keys of the copies') or diag($err);
$q = "SELECT count(*) || ':' || sum(k) FROM c3 WHERE k < 500";
is(via_index($a, $q), via_scan($a, $q), '3: the older snapshot finds its rows through the unique index');
($rc, $out, $err) = $node->psql('postgres', "INSERT INTO c3 VALUES (5, 0, 'dup')");
like($err, qr/duplicate key/, '3: the unique index still refuses a duplicate');
$a->query("COMMIT;");
$a->quit;

# 4. a partial index and an expression
fresh('c4');
($a, $n0) = reader('c4');
q1($node, "SELECT kingres.columnar_compact('c4', 1000000)");
q1($node, "CREATE INDEX c4_p ON c4 (k) WHERE g = 3; CREATE INDEX c4_e ON c4 ((k % 1000), g)");
$q = "SELECT count(*) || ':' || sum(k) FROM c4 WHERE g = 3 AND k < 5000";
is(via_index($a, $q), via_scan($a, $q), '4: a partial index built after the compaction serves the older snapshot');
$q = "SELECT count(*) || ':' || sum(k) FROM c4 WHERE k % 1000 = 17";
is(via_index($a, $q), via_scan($a, $q), '4: an expression index too');
$a->query("COMMIT;");
$a->quit;

# 5. after the older snapshot ended
q1($node, "VACUUM c1; VACUUM c2; VACUUM c3; VACUUM c4");
for my $t ('c1', 'c2', 'c3', 'c4')
{
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('$t')"), 0, "5: columnar_check_indexes finds nothing on $t");
}
$q = "SELECT count(*) || ':' || sum(k) FROM c1 WHERE k < 5000";
is(q1($node, "SET enable_seqscan = off; SET enable_bitmapscan = off; SET kingres_columnar.enable_custom_scan = off; $q"),
   q1($node, "SET enable_indexscan = off; $q"), '5: the index answers as the table does after the vacuum');

# 6. a parallel build
fresh('c6');
q1($node, "SELECT kingres.columnar_compact('c6', 1000000)");
q1($node, "INSERT INTO c6 SELECT 900000 + g, 1, 'late' FROM generate_series(1, 20000) g; INSERT INTO c6 SELECT 950000 + g, 2, 'late' FROM generate_series(1, 20000) g");
($a, $n0) = reader('c6');
q1($node, "SELECT kingres.columnar_compact('c6', 1000000)");
q1($node, "SET min_parallel_table_scan_size = 0; SET min_parallel_index_scan_size = 0; SET max_parallel_maintenance_workers = 4; SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; CREATE INDEX c6_k ON c6 (k)");
$q = "SELECT count(*) || ':' || sum(k) FROM c6 WHERE k < 1000000";
is(via_index($a, $q), via_scan($a, $q), '6: a parallel build indexes the replaced rows too');
$a->query("COMMIT;");
$a->quit;

# 7. the compaction and the index in one transaction: the replaced stripes are deleted by the transaction that builds
fresh('c7');
($a, $n0) = reader('c7');
q1($node, "BEGIN; SELECT kingres.columnar_compact('c7', 1000000); CREATE INDEX c7_k ON c7 (k); COMMIT");
$q = "SELECT count(*) || ':' || sum(k) FROM c7 WHERE k < 500";
is(via_index($a, $q), via_scan($a, $q), '7: compaction and CREATE INDEX in one transaction: the older snapshot finds its rows through the index');
like(via_index($a, $q), qr/^[1-9][0-9]*:[0-9]+$/, '7: ... and they are rows');
$a->query("COMMIT;");
$a->quit;

$node->stop;
done_testing();
