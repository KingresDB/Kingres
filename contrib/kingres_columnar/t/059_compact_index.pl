# contrib/kingres_columnar/t/059_compact_index.pl
#
# C6d: compaction of a table that has indexes, with other sessions.
#  1. An older snapshot (REPEATABLE READ, begun before the compaction) that reads the table
#     by an index after the compaction commits finds all its rows: the entries of the old
#     row numbers are still in the index and the old stripes are still visible to it.
#     (A rebuild of the index would have lost them; this is why the indexes are not rebuilt.)
#     A snapshot taken after the compaction finds the copies; after the older snapshot ends
#     and a VACUUM the entries of the old numbers are gone.
#  2. A loop of compactions and vacuums next to pgbench writers of overlapping keys (a
#     unique index, a heap twin written in the same subtransaction) and readers by the index:
#     no key twice, the same winners as the twin, every reader count matches the table,
#     columnar_check_indexes finds nothing.
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
use POSIX ();
use Test::More;

my $node = new_node('cmpi');

sub leaf_fn
{
	q1($node, q{CREATE FUNCTION leaf_entries(ix regclass) RETURNS bigint LANGUAGE plpgsql AS $$
	DECLARE n bigint := 0; t bigint; p int;
	BEGIN
	  FOR p IN 1 .. (pg_relation_size(ix) / 8192 - 1) LOOP
	    BEGIN
	      SELECT live_items INTO t FROM bt_page_stats(ix::text, p) WHERE type = 'l';
	      n := n + coalesce(t, 0);
	    EXCEPTION WHEN OTHERS THEN NULL;
	    END;
	  END LOOP;
	  RETURN n;
	END $$;});
}
leaf_fn();

# ---------------------------------------------------------------------------
# 1. an older snapshot
# ---------------------------------------------------------------------------
q1($node, q{CREATE TABLE s1 (k int, v text) USING columnar; CREATE UNIQUE INDEX s1_k ON s1 (k);});
q1($node, "INSERT INTO s1 SELECT $_ * 100 + g, 'old' FROM generate_series(1, 100) g") for 0 .. 9;
is(q1($node, "SELECT count(*) FROM kingres.columnar_stripes('s1')"), 10, 'ten small stripes');
my $old = $node->background_psql('postgres', on_error_stop => 0);
$old->query("BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) FROM s1;");
my $e0 = q1($node, "SELECT leaf_entries('s1_k')");
my $sum1 = q1($node, "SELECT sum(k) FROM s1");
is(q1($node, "SELECT kingres.columnar_compact('s1', 1000000) > 0"), 't', 'compaction with the older snapshot open');
is(q1($node, "SELECT count(*) FROM kingres.columnar_stripes('s1')"), 1, '... the ten stripes are one');
my $e1 = q1($node, "SELECT leaf_entries('s1_k')");
cmp_ok($e1, '>', $e0, "the indexes got entries for the copies and keep the old ones ($e0 index tuples before, $e1 after: entries of one key share a tuple until a page splits)");
my $cnt = $old->query("SET enable_seqscan = off; SET enable_bitmapscan = off; SELECT count(*) || ':' || sum(k) FROM s1 WHERE k BETWEEN 1 AND 100000;");
chomp $cnt;
is($cnt, "1000:$sum1", 'the older snapshot finds all its rows through the index');
is(q1($node, "SET enable_seqscan = off; SET enable_bitmapscan = off; SELECT count(*) FROM s1 WHERE k BETWEEN 1 AND 100000"), 1000, 'a new snapshot finds the copies through the index');
q1($node, "VACUUM s1");
cmp_ok(q1($node, "SELECT leaf_entries('s1_k')"), '>=', $e1, 'a VACUUM keeps the old entries while the older snapshot lives');
$old->query("COMMIT;");
$old->quit;
q1($node, "VACUUM s1");
cmp_ok(q1($node, "SELECT leaf_entries('s1_k')"), '<=', $e0 + 20, 'after the older snapshot ended a VACUUM removed the entries of the old numbers');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('s1')"), 0, 'the index is complete');
like(q1($node, "SELECT count(*) FROM s1"), qr/^1000$/, 'the table still has its 1000 rows');


# 1b: a writer of a unique key while a compaction is open: the rows are still the table's rows
q1($node, q{CREATE TABLE s2 (k int, v text) USING columnar; CREATE UNIQUE INDEX s2_k ON s2 (k);});
q1($node, "INSERT INTO s2 SELECT $_ * 100 + g, 'old' FROM generate_series(1, 100) g") for 0 .. 5;
my $open = $node->background_psql('postgres', on_error_stop => 0);
$open->query("BEGIN; SELECT kingres.columnar_compact('s2', 1000000);");
my ($rc, $out, $err) = $node->psql('postgres', "SET lock_timeout = '1s'; INSERT INTO s2 VALUES (150, 'dup')");
like($err, qr/duplicate key/, '1b: a duplicate of a row of a stripe the open compaction deleted is refused at once');
unlike($err, qr/lock timeout/, '1b: ... without waiting for the compaction');
($rc, $out, $err) = $node->psql('postgres', "SET lock_timeout = '1s'; INSERT INTO s2 VALUES (99999, 'new')");
is($rc, 0, '1b: a new key is taken meanwhile') or diag($err);
$open->query("ROLLBACK;");
($rc, $out, $err) = $node->psql('postgres', "INSERT INTO s2 VALUES (250, 'dup')");
like($err, qr/duplicate key/, '1b: after the compaction rolled back the rows are still found');
q1($node, "SELECT kingres.columnar_compact('s2', 1000000)");
($rc, $out, $err) = $node->psql('postgres', "INSERT INTO s2 VALUES (350, 'dup')");
like($err, qr/duplicate key/, '1b: after the compaction committed the copies are found');
$open->quit;

# 1c: a partial index gets entries only for the rows it covers
q1($node, q{CREATE TABLE s3 (k int, v text) USING columnar; CREATE INDEX s3_all ON s3 (k); CREATE INDEX s3_even ON s3 (k) WHERE k % 2 = 0;});
q1($node, "INSERT INTO s3 SELECT $_ * 100 + g, 'old' FROM generate_series(1, 100) g") for 0 .. 9;
q1($node, "SELECT kingres.columnar_compact('s3', 1000000)");
q1($node, "VACUUM s3");
my ($ea, $ee) = (q1($node, "SELECT leaf_entries('s3_all')"), q1($node, "SELECT leaf_entries('s3_even')"));
cmp_ok($ee, '<=', 0.6 * $ea, "the partial index has entries for its half of the rows only ($ee against $ea)");
# ---------------------------------------------------------------------------
# 2. compaction next to writers and readers
# ---------------------------------------------------------------------------
q1($node, q{
	CREATE TABLE c (k int, who int, v text) USING columnar;
	CREATE UNIQUE INDEX c_k ON c (k);
	CREATE INDEX c_who ON c (who, k);
	CREATE TABLE h (k int, who int, v text);
	CREATE UNIQUE INDEX h_k ON h (k);
});
my $script = $node->basedir . '/ukeys.pgb';
append_to_file($script, q{\set k random(1, 2500)
DO $$ BEGIN
  BEGIN
    INSERT INTO h VALUES (:k, :client_id, 'x');
    INSERT INTO c VALUES (:k, :client_id, 'x');
  EXCEPTION WHEN unique_violation THEN NULL;
  END;
END $$;
});
my $rscript = $node->basedir . '/readers.pgb';
append_to_file($rscript, q{SET enable_seqscan = off;
SET enable_bitmapscan = off;
SELECT count(*) FILTER (WHERE k > 0) FROM c WHERE k BETWEEN 1 AND 2500;
});
my $pid = fork();
if ($pid == 0)
{
	for (1 .. 25)
	{
		system('psql', '-X', '-q', '-d', 'postgres', '-h', $node->host, '-p', $node->port, '-c',
			"SELECT kingres.columnar_compact('c', 100000)", '-c', 'VACUUM c');
		select(undef, undef, undef, 0.1);
	}
	POSIX::_exit(0);
}
$node->command_ok([ 'pgbench', '-n', '-c', 4, '-j', 4, '-t', 250, '-f', $script, 'postgres' ], '4 writers of overlapping keys while compactions and vacuums run');
waitpid($pid, 0);
is($? >> 8, 0, 'the loop of compactions and vacuums ended without an error');
q1($node, "SELECT kingres.columnar_compact('c', 100000)");
is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM c"), 0, 'no key twice');
is(q1($node, "SELECT count(*) FROM (SELECT k, who FROM c EXCEPT SELECT k, who FROM h) s"), 0, 'the same winners as the heap twin');
is(q1($node, "SELECT count(*) FROM (SELECT k, who FROM h EXCEPT SELECT k, who FROM c) s"), 0, '... in both directions');
my ($via_idx, $via_scan) = (
	q1($node, "SET enable_seqscan = off; SET enable_bitmapscan = off; SELECT count(*) FROM c WHERE k >= 0"),
	q1($node, "SET enable_indexscan = off; SELECT count(*) FROM c"));
is($via_idx, $via_scan, "the index scan and the table scan agree ($via_scan rows)");
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('c')"), 0, 'columnar_check_indexes finds nothing');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, 'columnar_check has nothing to report');

# readers by the index while compactions run: every count is the number of rows (no writer)
$pid = fork();
if ($pid == 0)
{
	for (1 .. 15)
	{
		system('psql', '-X', '-q', '-d', 'postgres', '-h', $node->host, '-p', $node->port, '-c',
			"INSERT INTO c SELECT 100000 * " . $_ . " + g, 0, 'z' FROM generate_series(1, 200) g", '-c',
			"SELECT kingres.columnar_compact('c', 100000)");
	}
	POSIX::_exit(0);
}
my $bad = 0;
my $n0 = q1($node, "SELECT count(*) FROM c WHERE k <= 2500");
my $rd = $node->background_psql('postgres', on_error_stop => 0);
$rd->query("SET enable_seqscan = off; SET enable_bitmapscan = off;");
for (1 .. 150)
{
	$rd->{stderr} = '';
	my $r = $rd->query("SELECT count(*) FROM c WHERE k <= 2500;");
	$bad++ if $r !~ /\b$n0\b/ || $rd->{stderr} ne '';
}
waitpid($pid, 0);
is($bad, 0, 'index readers got the same count every time while compactions ran');
$rd->quit;
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('c')"), 0, 'columnar_check_indexes finds nothing at the end');
is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM c"), 0, 'no key twice at the end');
$node->stop;
done_testing();
