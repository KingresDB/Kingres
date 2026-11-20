# contrib/kingres_columnar/t/050_reserve_check.pl
#
# C6a, decision 6: columnar_check, columnar_reconcile and VACUUM against the reserved
# stripe row of a writer.
#
#   - a writer that lives (a session with a transaction in progress that reserved row
#     numbers): the check says nothing, the reconcile, the vacuum of the table, the
#     vacuum of the stripe catalog, the fixing check and a compaction do not touch its
#     reserved row (the writer then commits, and its rows are there);
#   - a prepared transaction (it wrote its stripes at PREPARE): the same, until COMMIT
#     PREPARED;
#   - a transaction that aborted: its reserved row is dead, not damage, and the vacuum of
#     the catalog takes it away;
#   - a backend that is killed (SIGKILL: the server restarts): the same;
#   - a reserved row that a COMMITTED transaction left is damage (`reserved_row_left`):
#     made here by hand, reported by the check, and not touched by the reconcile;
#   - a table that is truncated, dropped or converted by the transaction that wrote into
#     it leaves no reserved row.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1 rsql fp same_as_twin tids_ok check_empty reserve_on);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = new_node('rchk');

q1($node, q{
	CREATE TABLE c (a int, b text) USING columnar;
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE ch (a int, b text);
	CREATE TABLE ret (a int, t tid);
	CREATE SEQUENCE sq;
});
sub ins
{
	my ($n) = @_;
	return "WITH s AS (SELECT nextval('sq')::int a FROM generate_series(1, $n)), i AS (INSERT INTO c SELECT a, 'v' || a FROM s RETURNING a, ctid), j AS (INSERT INTO ch SELECT a, 'v' || a FROM s) INSERT INTO ret SELECT a, ctid FROM i;";
}
sub reserved_rows { return q1($node, "SELECT count(*) FROM kingres.columnar_stripe WHERE reserved"); }

# everything an administrator may run, against a writer that is alive
sub maintenance_round
{
	my ($what) = @_;
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, "$what: columnar_check has nothing to report");
	is(q1($node, "SELECT kingres.columnar_check_clean()"), 't', "$what: columnar_check_clean is true");
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check(true)"), 0, "$what: the fixing check reports nothing and removes nothing");
	is(q1($node, "SELECT kingres.columnar_reconcile()"), 0, "$what: columnar_reconcile deletes nothing");
	my ($rc, $out, $err) = $node->psql('postgres', "VACUUM c; VACUUM kingres.columnar_stripe;");
	is($rc, 0, "$what: VACUUM of the table and of the catalog run") or diag($err);
}

# a first batch of committed rows, in a few stripes
my ($rc, $out, $err) = rsql($node, "BEGIN; " . ins(10) . " COMMIT; BEGIN; " . ins(2300) . " COMMIT;");
is($rc, 0, 'committed rows') or diag($err);

# ---------------------------------------------------------------------------
# a live writer
# ---------------------------------------------------------------------------
{
	my $w = $node->background_psql('postgres');
	$w->query_safe(reserve_on() . "BEGIN; " . ins(40) . " SAVEPOINT x; " . ins(5) . " RELEASE x;");
	cmp_ok(q1($node, "SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction'"), '>=', 1, 'a writer is in progress, with its rows buffered and its reserved catalog row inserted');
	maintenance_round('live writer');
	$w->query_safe("COMMIT;");
	$w->quit;
	same_as_twin($node, 'c', 'ch', 'after the live writer');
	tids_ok($node, 'c', 'ret', 'after the live writer');
	check_empty($node, 'after the live writer');

	# a compaction while a writer lives: it copies the committed stripes into new ones
	# (with new row numbers: it does not keep TIDs, C6d), the writer's reservation is
	# not in its way, and the writer commits its rows with the numbers it reserved
	$w = $node->background_psql('postgres');
	$w->query_safe(reserve_on() . "BEGIN; " . ins(20));
	is(q1($node, "SELECT kingres.columnar_compact('c', 100000) >= 0"), 't', 'a compaction runs while a writer lives');
	$w->query_safe("COMMIT;");
	$w->quit;
	same_as_twin($node, 'c', 'ch', 'after a compaction and a live writer');
	is(q1($node, "SELECT count(DISTINCT ctid::text) = count(*) FROM c"), 't', '... no two rows share a TID');
	check_empty($node, 'after a compaction and a live writer');
	# the rows of the compaction have new numbers: the TIDs recorded earlier are stale now
	q1($node, "TRUNCATE ret; INSERT INTO ret SELECT a, ctid FROM c");

	# the stripe catalog is rewritten (VACUUM FULL, CLUSTER) while a writer holds a
	# reservation: the writer holds no lock on the catalog between its insert of the
	# reserved row and its flush, so the row moves, and the flush finds it by its key
	$w = $node->background_psql('postgres');
	$w->query_safe(reserve_on() . "BEGIN; " . ins(15) . " SAVEPOINT x; " . ins(5));
	($rc, $out, $err) = $node->psql('postgres', "VACUUM FULL kingres.columnar_stripe");
	is($rc, 0, 'VACUUM FULL of the stripe catalog while a writer holds reserved rows') or diag($err);
	($rc, $out, $err) = $node->psql('postgres', "CLUSTER kingres.columnar_stripe USING columnar_stripe_pkey");
	is($rc, 0, 'CLUSTER of the stripe catalog while a writer holds reserved rows') or diag($err);
	$w->query_safe("RELEASE x; COMMIT;");
	$w->quit;
	pass('the writer commits: it finds its reserved rows by their keys after they moved');
	same_as_twin($node, 'c', 'ch', 'after the catalog was rewritten under a writer');
	tids_ok($node, 'c', 'ret', 'after the catalog was rewritten under a writer');
	check_empty($node, 'after the catalog was rewritten under a writer');
}

# ---------------------------------------------------------------------------
# a prepared transaction
# ---------------------------------------------------------------------------
{
	($rc, $out, $err) = rsql($node, "BEGIN; " . ins(60) . " SAVEPOINT x; " . ins(7) . " RELEASE x; PREPARE TRANSACTION 'chk1';");
	is($rc, 0, 'a transaction is prepared') or diag($err);
	maintenance_round('prepared transaction');
	q1($node, "COMMIT PREPARED 'chk1'");
	same_as_twin($node, 'c', 'ch', 'after COMMIT PREPARED');
	tids_ok($node, 'c', 'ret', 'after COMMIT PREPARED');
	check_empty($node, 'after COMMIT PREPARED');
}

# ---------------------------------------------------------------------------
# an aborted transaction
# ---------------------------------------------------------------------------
{
	q1($node, "VACUUM kingres.columnar_stripe");
	($rc, $out, $err) = rsql($node, "BEGIN; " . ins(30) . " ROLLBACK;");
	is($rc, 0, 'a writer rolls back') or diag($err);
	is(reserved_rows(), 0, 'its reserved row is not seen (an aborted row is dead)');
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, 'columnar_check: the reserved row of an aborted transaction is not damage');
	($rc, $out, $err) = $node->psql('postgres', "VACUUM (VERBOSE) kingres.columnar_stripe");
	my ($removed) = $err =~ /tuples: (\d+) removed/;
	cmp_ok($removed // 0, '>=', 1, "the vacuum of the catalog removed its dead row(s) ($removed)");
	same_as_twin($node, 'c', 'ch', 'after the aborted writer');
	check_empty($node, 'after the aborted writer');
}

# ---------------------------------------------------------------------------
# a killed backend
# ---------------------------------------------------------------------------
{
	$node->append_conf('postgresql.conf', "restart_after_crash = on
");
	$node->reload;
	q1($node, "VACUUM kingres.columnar_stripe");
	q1($node, "CHECKPOINT");
	my $w = $node->background_psql('postgres');
	my $pid = $w->query_safe("SELECT pg_backend_pid();");
	$pid =~ s/\s+//g;
	$w->query_safe(reserve_on() . "BEGIN; " . ins(33));
	q1($node, "SELECT pg_switch_wal()");
	kill 'KILL', $pid;
	eval { $w->quit };
	$node->poll_query_until('postgres', 'SELECT 1', '1') or BAIL_OUT('the server did not come back');
	is(reserved_rows(), 0, 'a killed backend: its reserved row is not seen');
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, 'columnar_check: the reserved row of a killed backend is not damage');
	($rc, $out, $err) = $node->psql('postgres', "VACUUM (VERBOSE) kingres.columnar_stripe");
	my ($removed) = $err =~ /tuples: (\d+) removed/;
	is($removed // 0, 1, 'the vacuum of the catalog removed the dead reserved row of the killed backend');
	same_as_twin($node, 'c', 'ch', 'after the killed backend');
	tids_ok($node, 'c', 'ret', 'after the killed backend');
	check_empty($node, 'after the killed backend');
}

# ---------------------------------------------------------------------------
# a reserved row that a committed transaction left is damage
# ---------------------------------------------------------------------------
{
	q1($node, q{
		INSERT INTO kingres.columnar_stripe
		  (rel_uid, stripe_id, first_row, row_count, chunk_groups, natts, first_block, block_count,
		   blob_len, raw_bytes, footer_offset, cid_points, col_stats, reserved)
		SELECT kingres.columnar_identity('c'), 99999, 50000000, 1000, 0, 2, 0, 0, 0, 0, 0, ''::bytea, ''::bytea, true});
	is(q1($node, "SELECT check_name FROM kingres.columnar_check()"), 'reserved_row_left', 'a committed reserved row is reported (reserved_row_left)');
	is(q1($node, "SELECT kingres.columnar_check_clean()"), 'f', '... and columnar_check_clean is false');
	is(q1($node, "SELECT count(*) FROM c"), q1($node, "SELECT count(*) FROM ch"), '... the table reads as before (readers skip a reserved row)');
	is(q1($node, "SELECT kingres.columnar_reconcile()"), 0, '... columnar_reconcile does not delete it (its table is alive)');
	($rc, $out, $err) = rsql($node, "INSERT INTO c VALUES (-1, 'after the damage')");
	is($rc, 0, '... and the table takes rows') or diag($err);
	q1($node, "DELETE FROM c WHERE FALSE; DELETE FROM kingres.columnar_stripe WHERE reserved");
	q1($node, "DELETE FROM ch WHERE FALSE");
	q1($node, "INSERT INTO ch VALUES (-1, 'after the damage')");
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, 'with the row deleted the check is clean');
}

# ---------------------------------------------------------------------------
# a table written, then truncated, converted or dropped by the same transaction
# ---------------------------------------------------------------------------
for my $stmt ('TRUNCATE t1', 'ALTER TABLE t1 SET ACCESS METHOD heap', 'DROP TABLE t1')
{
	q1($node, "CREATE TABLE t1 (a int, b text) USING columnar; INSERT INTO t1 SELECT g, 'x' FROM generate_series(1, 50) g;");
	($rc, $out, $err) = rsql($node, "BEGIN; INSERT INTO t1 SELECT g, 'y' FROM generate_series(100, 160) g; $stmt; COMMIT;");
	is($rc, 0, "$stmt after an insert that reserved") or diag($err);
	is(reserved_rows(), 0, "$stmt: no reserved row is left");
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, "$stmt: columnar_check has nothing to report");
	is(q1($node, "SELECT kingres.columnar_reconcile()"), 0, "$stmt: nothing is left for the reconcile to delete");
	q1($node, "DROP TABLE IF EXISTS t1");
}
# the same with a savepoint that is released, and one that is rolled back
{
	q1($node, "CREATE TABLE t2 (a int, b text) USING columnar");
	($rc, $out, $err) = rsql($node, "BEGIN; SAVEPOINT s; INSERT INTO t2 SELECT g, 'x' FROM generate_series(1, 20) g; RELEASE s; INSERT INTO t2 VALUES (99, 'top'); TRUNCATE t2; INSERT INTO t2 VALUES (100, 'after'); COMMIT;");
	is($rc, 0, 'TRUNCATE after a released savepoint that reserved') or diag($err);
	is(q1($node, "SELECT string_agg(a::text, ',') FROM t2"), '100', '... only the row after the TRUNCATE is there');
	is(reserved_rows(), 0, '... no reserved row is left');
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '... and the check is clean');
	q1($node, "DROP TABLE t2");
}

my $log = slurp_file($node->logfile);
unlike($log, qr/TRAP:|PANIC:(?!.*terminating)/, 'no assertion failure and no PANIC in the server log');

$node->stop;
done_testing();
