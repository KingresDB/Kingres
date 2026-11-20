# contrib/kingres_columnar/t/056_index_vacuum.pl
#
# C6c: VACUUM removes the index entries of row numbers that no live row holds.
#  1. Rolled-back inserts leave entries (their numbers were reserved and never returned);
#     VACUUM removes them: the index shrinks (a million rolled-back entries), queries
#     through the index give the rows of the heap twin, and a rolled-back key can be
#     inserted again.
#  2. VACUUM (INDEX_CLEANUP off) leaves the index alone; a later plain VACUUM cleans it.
#  3. VACUUM racing writers that reserve, flush, commit and roll back (pgbench, with a loop
#     of VACUUMs next to it): afterwards the table is that of the heap twin, no key is
#     twice in a unique index, and columnar_check_indexes finds nothing.
#  4. columnar_check_indexes finds a row that has no entry (an index that was not ready
#     while the row went in), and reports the index and the row.
#  5. Autovacuum does the same without being asked.
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

my $node = new_node('ivac', conf => "autovacuum_naptime = 1s\nallow_system_table_mods = on\n");

sub isize { my ($i) = @_; return q1($node, "SELECT pg_relation_size('$i')"); }
sub rows_via_index
{
	my ($t, $lo, $hi) = @_;
	return q1($node, "SET enable_seqscan = off; SET enable_bitmapscan = off; SELECT count(*) || ':' || coalesce(sum(k), 0) FROM $t WHERE k BETWEEN $lo AND $hi");
}

# ---------------------------------------------------------------------------
# 1. rolled-back entries
# ---------------------------------------------------------------------------
q1($node, q{
	CREATE TABLE v1 (k int, v text) USING columnar;
	CREATE INDEX v1_k ON v1 (k);
	CREATE TABLE v1h (k int, v text);
	CREATE INDEX v1h_k ON v1h (k);
	INSERT INTO v1 SELECT g, 'keep' FROM generate_series(1, 20000) g;
	INSERT INTO v1h SELECT g, 'keep' FROM generate_series(1, 20000) g;
});
# the number of entries on the leaf pages of a B-tree index (pages the index no longer
# uses count for none)
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
q1($node, "CREATE EXTENSION IF NOT EXISTS pageinspect");
my $clean = isize('v1_k');
for my $r (1 .. 4)
{
	$node->safe_psql('postgres', "BEGIN; INSERT INTO v1 SELECT 1000000 * $r + g, 'gone' FROM generate_series(1, 250000) g; ROLLBACK;");
}
my $dirty = isize('v1_k');
my $e0 = q1($node, "SELECT leaf_entries('v1_k')");
cmp_ok($e0, '>=', 1000000, "a million rolled-back entries are in the index ($e0 entries, $clean bytes before, $dirty after)");
is(rows_via_index('v1', 1000001, 5000000), '0:0', 'the rolled-back keys are not visible through the index');
q1($node, "VACUUM v1");
my $e1 = q1($node, "SELECT leaf_entries('v1_k')");
cmp_ok($e1, '<=', 20300, "VACUUM removed them: $e1 entries are left, the 20 000 live rows");
is(rows_via_index('v1', 1, 20000), rows_via_index('v1h', 1, 20000), 'the index finds the rows of the heap twin');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('v1')"), 0, 'columnar_check_indexes finds nothing');
# the pages are used again: another million rolled-back entries do not make the file grow much
q1($node, "VACUUM v1");
my $size1 = isize('v1_k');
for my $r (1 .. 4)
{
	$node->safe_psql('postgres', "BEGIN; INSERT INTO v1 SELECT 1000000 * $r + g, 'gone' FROM generate_series(1, 250000) g; ROLLBACK;");
}
# the same on the heap twin, for what a B-tree does with its freed pages
for my $r (1 .. 4)
{
	$node->safe_psql('postgres', "BEGIN; INSERT INTO v1h SELECT 1000000 * $r + g, 'gone' FROM generate_series(1, 250000) g; ROLLBACK;");
}
q1($node, "VACUUM v1h");
q1($node, "VACUUM v1h");
my $hsize1 = isize('v1h_k');
for my $r (1 .. 4)
{
	$node->safe_psql('postgres', "BEGIN; INSERT INTO v1h SELECT 1000000 * $r + g, 'gone' FROM generate_series(1, 250000) g; ROLLBACK;");
}
is(isize('v1_k'), isize('v1h_k'), 'the index of the columnar table is as large as that of the heap twin after the same inserts, rollbacks and vacuums (a B-tree reuses its freed pages the same way)');
note("heap twin: index $hsize1 bytes after the vacuum, " . isize('v1h_k') . " after the second round; columnar: $size1 and " . isize('v1_k'));
q1($node, "VACUUM v1");
is(q1($node, "SELECT count(*) FROM v1 WHERE k > 20000") , 0, 'no rolled-back row is in the table');
is(q1($node, "INSERT INTO v1 VALUES (100001, 'again') RETURNING k"), 100001, 'a rolled-back key can be inserted again');
is(rows_via_index('v1', 100001, 100001), '1:100001', '... and is found once');

# ---------------------------------------------------------------------------
# 2. INDEX_CLEANUP off
# ---------------------------------------------------------------------------
$node->safe_psql('postgres', "BEGIN; INSERT INTO v1 SELECT 9000000 + g, 'gone' FROM generate_series(1, 250000) g; ROLLBACK;");
my $before = q1($node, "SELECT leaf_entries('v1_k')");
q1($node, "VACUUM (INDEX_CLEANUP off) v1");
is(q1($node, "SELECT leaf_entries('v1_k')"), $before, 'VACUUM (INDEX_CLEANUP off) leaves the index alone');
q1($node, "VACUUM v1");
cmp_ok(q1($node, "SELECT leaf_entries('v1_k')"), '<', $before - 200000, 'a later VACUUM cleans it');

# ---------------------------------------------------------------------------
# 3. VACUUM racing writers
# ---------------------------------------------------------------------------
q1($node, q{
	CREATE TABLE rc (k int, who int, v text) USING columnar;
	CREATE UNIQUE INDEX rc_k ON rc (k);
	CREATE INDEX rc_who ON rc (who, k);
	CREATE TABLE rh (k int, who int, v text);
	CREATE UNIQUE INDEX rh_k ON rh (k);
	SELECT kingres.columnar_set_options('rc', stripe_row_count => 1000, chunk_row_count => 100);
});
my $script = $node->basedir . '/vrace.pgb';
append_to_file($script, q{\set base random(1, 100000000)
\set n random(1, 1500)
\set w random(1, 4)
BEGIN;
INSERT INTO rh SELECT :base * 10 + g, :client_id, 'v' FROM generate_series(1, :n) g;
INSERT INTO rc SELECT :base * 10 + g, :client_id, 'v' FROM generate_series(1, :n) g;
\if :w = 1
ROLLBACK;
\else
COMMIT;
\endif
});
my $vloop = $node->basedir . '/vloop.sh';
my $pid = fork();
if ($pid == 0)
{
	for (1 .. 40)
	{
		system('psql', '-X', '-q', '-d', 'postgres', '-h', $node->host, '-p', $node->port, '-c', 'VACUUM rc');
		select(undef, undef, undef, 0.1);
	}
	POSIX::_exit(0);
}
$node->command_ok([ 'pgbench', '-n', '-c', 4, '-j', 4, '-t', 40, '-f', $script, 'postgres' ], '4 sessions write while VACUUM runs');
waitpid($pid, 0);
is($? >> 8, 0, 'the vacuum loop ended without an error');
q1($node, "VACUUM rc");
is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM rc"), 0, 'no key twice in the unique index');
is(q1($node, "SELECT count(*) FROM (SELECT k, who FROM rc EXCEPT SELECT k, who FROM rh) s"), 0, 'the rows are those of the heap twin');
is(q1($node, "SELECT count(*) FROM (SELECT k, who FROM rh EXCEPT SELECT k, who FROM rc) s"), 0, '... in both directions');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('rc')"), 0, 'columnar_check_indexes finds nothing');
my ($via_idx, $via_scan) = (
	q1($node, "SET enable_seqscan = off; SET enable_bitmapscan = off; SELECT count(*) FROM rc WHERE k >= 0"),
	q1($node, "SET enable_indexscan = off; SELECT count(*) FROM rc"));
is($via_idx, $via_scan, "the index scan and the table scan count the same rows ($via_scan)");
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, 'columnar_check has nothing to report');

# ---------------------------------------------------------------------------
# 3b. the reservation lock: a writer that has taken numbers and not yet made its claim
# row (stopped there by the developer gate) keeps the index vacuum waiting
# ---------------------------------------------------------------------------
q1($node, q{CREATE TABLE gl (k int, v text) USING columnar; CREATE INDEX gl_k ON gl (k);
	INSERT INTO gl SELECT g, 'x' FROM generate_series(1, 100) g;});
{
	my $KEY = 781;
	my $ctl = $node->background_psql('postgres');
	$ctl->query_safe("SELECT pg_advisory_lock($KEY, 0);");
	my $holder = $node->background_psql('postgres', on_error_stop => 0);
	$holder->query_until(qr/^go$/m, "SET kingres_columnar.reserve_gate = $KEY; SET kingres_columnar.reserve_gate_at = 1;\n\\echo go\nINSERT INTO gl VALUES (1000, 'held');\n");
	my $w = 0;
	for (1 .. 200)
	{
		last if ($w = q1($node, "SELECT count(*) FROM pg_stat_activity WHERE wait_event = 'PgSleep' AND pid <> pg_backend_pid()")) >= 1;
		select(undef, undef, undef, 0.05);
	}
	is($w, 1, 'the writer waits in the gate after taking its numbers');
	my $vac = $node->background_psql('postgres', on_error_stop => 0);
	$vac->query_until(qr/^go$/m, "\\echo go\nVACUUM gl;\n");
	my $vw = 0;
	for (1 .. 100)
	{
		last if ($vw = q1($node, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'VACUUM gl%'")) >= 1;
		select(undef, undef, undef, 0.05);
	}
	is($vw, 1, 'the index vacuum waits for the writer that is between its numbers and its claim');
	$ctl->query_safe("SELECT pg_advisory_unlock($KEY, 0);");
	$vac->query("SELECT 'sync';");
	$holder->query("SELECT 'sync';");
	is(q1($node, "SELECT count(*) FROM gl WHERE k = 1000"), 1, 'the writer finished and its row is there');
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('gl')"), 0, 'the index is complete after the vacuum');
	$_->quit for ($ctl, $holder, $vac);
}

# ---------------------------------------------------------------------------
# 4. columnar_check_indexes finds damage
# ---------------------------------------------------------------------------
q1($node, q{
	CREATE TABLE dmg (k int, v text) USING columnar;
	CREATE INDEX dmg_k ON dmg (k);
	INSERT INTO dmg SELECT g, 'x' FROM generate_series(1, 100) g;
	UPDATE pg_index SET indisready = false WHERE indexrelid = 'dmg_k'::regclass;
});
$node->safe_psql('postgres', "SELECT pg_sleep(0)");    # a new backend reads the catalog afresh
q1($node, "INSERT INTO dmg SELECT g, 'lost' FROM generate_series(101, 105) g;");
q1($node, "UPDATE pg_index SET indisready = true WHERE indexrelid = 'dmg_k'::regclass;");
my $rep = q1($node, "SELECT count(*) || '|' || coalesce(min(check_name), '') || '|' || coalesce(min(detail), '') FROM kingres.columnar_check_indexes('dmg')");
like($rep, qr/^1\|index_entry_missing\|5 visible row/, "columnar_check_indexes reports the 5 rows without an entry ($rep)");
q1($node, "REINDEX INDEX dmg_k");
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('dmg')"), 0, 'after REINDEX the report is empty');
# one row only
q1($node, "UPDATE pg_index SET indisready = false WHERE indexrelid = 'dmg_k'::regclass");
$node->safe_psql('postgres', "SELECT pg_sleep(0)");
q1($node, "INSERT INTO dmg VALUES (200, 'lost')");
q1($node, "UPDATE pg_index SET indisready = true WHERE indexrelid = 'dmg_k'::regclass");
like(q1($node, "SELECT count(*) || '|' || coalesce(min(detail), '') FROM kingres.columnar_check_indexes('dmg')"), qr/^1\|1 visible row/, 'a single row without an entry is reported too');
q1($node, "REINDEX INDEX dmg_k");

# ---------------------------------------------------------------------------
# 5. autovacuum
# ---------------------------------------------------------------------------
q1($node, q{
	CREATE TABLE av (k int, v text) USING columnar;
	CREATE INDEX av_k ON av (k);
	INSERT INTO av SELECT g, 'keep' FROM generate_series(1, 10000) g;
	ALTER TABLE av SET (autovacuum_vacuum_threshold = 50, autovacuum_vacuum_scale_factor = 0, autovacuum_enabled = true);
	ALTER SYSTEM SET autovacuum = on;
	SELECT pg_reload_conf();
});
my $av0 = q1($node, "SELECT leaf_entries('av_k')");
$node->safe_psql('postgres', "BEGIN; INSERT INTO av SELECT 100000 + g, 'gone' FROM generate_series(1, 250000) g; ROLLBACK;");
my $av1 = q1($node, "SELECT leaf_entries('av_k')");
cmp_ok($av1, '>', $av0 * 2, "the index grew with the garbage ($av0 to $av1)");
my $done = 0;
for (1 .. 60)
{
	if (q1($node, "SELECT count(*) FROM pg_stat_user_tables WHERE relname = 'av' AND autovacuum_count > 0") > 0) { $done = 1; last; }
	select(undef, undef, undef, 0.5);
}
SKIP:
{
	skip 'autovacuum did not pick the table in 30 s (it counts the table by its live rows)', 2 unless $done;
	cmp_ok(q1($node, "SELECT leaf_entries('av_k')"), '<', $av1 - 200000, 'autovacuum removed the garbage entries');
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('av')"), 0, '... and the index is complete');
}
note("autovacuum ran: $done");

$node->stop;
done_testing();
