# contrib/kingres_columnar/t/046_reserve_crash.pl
#
# C6a, decision 6 and the crash table of design 7.2: an immediate stop (a crash) at
# each point of a reservation, then a restart.  The points are the ones the developer
# settings kingres_columnar.reserve_gate and reserve_gate_at name; the writer waits
# there, in a session of its own, until the server is stopped under it:
#
#   0  rows are buffered (the reserved catalog row is there, nothing else)
#   1  the numbers are taken from the metapage, the catalog row is not inserted
#   2  the reserved catalog row is inserted, no row is put in the buffer
#   3  the pages of the stripe are written, the reserved row is not yet updated
#   4  the reserved row is updated to the stripe's row, the tail is not given back
#   P  PREPARE TRANSACTION done, the server stopped, COMMIT PREPARED (and ROLLBACK
#      PREPARED) after the restart
#
# After each restart: no row that was committed is lost (a transaction committed just
# before the crash, with the TIDs its inserts returned), no row that was not committed
# is there, no two rows share a TID, the reserved row of the dead backend is invisible
# and is removed by the vacuum of the stripe catalog (the path that removes the rows of
# aborted stripes), columnar_check has nothing to report (a reserved row of a dead
# backend is not damage), and the table takes new rows whose TIDs are new.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1 rsql fp same_as_twin tids_ok check_empty wait_waiting reserve_on);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = new_node('rcrash');
my $KEY = 777;

q1($node, q{
	CREATE TABLE c (a int, b text) USING columnar;
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE ch (a int, b text);
	CREATE TABLE ret (a int, t tid);
});

my $next = 1;
# n rows of the next ids into the table and the twin, the TIDs into ret
sub ins
{
	my ($n) = @_;
	my $lo = $next;
	$next += $n;
	my $hi = $next - 1;
	return "WITH s AS (SELECT g a, 't' || g b FROM generate_series($lo, $hi) g), i AS (INSERT INTO c SELECT a, b FROM s RETURNING a, ctid), j AS (INSERT INTO ch SELECT a, b FROM s) INSERT INTO ret SELECT a, ctid FROM i;";
}
# the same rows in the twin only: for a transaction that is going to be lost
sub twin_rows { my $n = shift; $next += $n; }

sub crash_and_restart
{
	my ($bg, $ctl) = @_;
	# the WAL of the writer is on disk when the server is stopped: what the writer did
	# is what recovery meets, not what happened to be flushed
	q1($node, "SELECT pg_switch_wal()");
	$node->stop('immediate');
	eval { $bg->quit } if $bg;
	eval { $ctl->quit } if $ctl;
	$node->start;
}

# the state after a restart
sub after_restart
{
	my ($what, $lost_rows_hint) = @_;
	same_as_twin($node, 'c', 'ch', $what);
	tids_ok($node, 'c', 'ret', $what);
	check_empty($node, $what);
	# the dead rows of the writer's transaction are heap's to remove
	my ($rc, $out, $err) = $node->psql('postgres', "VACUUM (VERBOSE) kingres.columnar_stripe");
	my ($removed) = $err =~ /tuples: (\d+) removed/;
	return $removed // 0;
}

# new rows after the restart: unique TIDs, and no TID of an old row
sub write_more
{
	my ($what) = @_;
	my $before = q1($node, "SELECT coalesce(max(ctid::text), '') FROM c");
	my ($rc, $out, $err) = rsql($node, 'BEGIN; ' . ins(25) . ' COMMIT;');
	is($rc, 0, "$what: the table takes new rows") or diag($err);
	tids_ok($node, 'c', 'ret', "$what, new rows");
	same_as_twin($node, 'c', 'ch', "$what, new rows");
}

# baseline: committed rows
my ($rc, $out, $err) = rsql($node, 'BEGIN; ' . ins(50) . ' COMMIT;');
is($rc, 0, 'baseline committed') or diag($err);

# ---------------------------------------------------------------------------
# the points 0 to 4
# ---------------------------------------------------------------------------
for my $point (0 .. 4)
{
	# a transaction that commits just before the crash: its rows are not lost
	($rc, $out, $err) = rsql($node, 'BEGIN; ' . ins(10) . ' COMMIT;');
	is($rc, 0, "point $point: a transaction committed before the crash") or diag($err);

	# nothing dead is left in the stripe catalog from the earlier rounds (and the
	# vacuum is on disk, a crash does not bring the dead rows back): what the vacuum
	# removes after the crash is what the writer left
	q1($node, "VACUUM kingres.columnar_stripe");
	q1($node, "CHECKPOINT");
	my $ctl = $node->background_psql('postgres');
	$ctl->query_safe("SELECT pg_advisory_lock($KEY, 0);");
	my $bg = $node->background_psql('postgres');
	my $sql = ($point == 0)
	  ? "BEGIN; INSERT INTO c VALUES (900000, 'lost'), (900001, 'lost');"
	  : ($point <= 2)
	  ? "BEGIN; INSERT INTO c VALUES (900000, 'lost'), (900001, 'lost');"
	  : "BEGIN; INSERT INTO c VALUES (900000, 'lost'), (900001, 'lost'); COMMIT;";
	my $gate = $point == 0 ? '' : "SET kingres_columnar.reserve_gate = $KEY; SET kingres_columnar.reserve_gate_at = $point;";
	if ($point == 0)
	{
		# stop with the rows only in the buffer: nothing waits
		$bg->query_safe(reserve_on() . $sql);
	}
	else
	{
		$bg->query_until(qr/^go$/m, reserve_on() . $gate . "\n\\echo go\n$sql\n");
		ok(wait_waiting($node, 1), "point $point: the writer waits at its gate");
	}
	if ($point >= 3)
	{
		# the pages are written before the gate of the point 3: a reader of another
		# session sees nothing of the transaction
		is(q1($node, "SELECT count(*) FROM c WHERE a >= 900000"), 0, "point $point: nobody sees the rows of the writer that waits");
	}
	crash_and_restart($bg, $ctl);

	is(q1($node, "SELECT count(*) FROM c WHERE a >= 900000"), 0, "point $point: after the crash the rows of the transaction that did not commit are not there");
	my $removed = after_restart("point $point");
	is($removed, (1, 0, 1, 1, 2)[$point], "point $point: the vacuum of the stripe catalog removed the dead row(s) of the writer ($removed)");
	write_more("point $point");
}

# ---------------------------------------------------------------------------
# P: prepared, crash, commit prepared / rollback prepared
# ---------------------------------------------------------------------------
for my $fate (qw(COMMIT ROLLBACK))
{
	($rc, $out, $err) = rsql($node, "BEGIN; " . ins(1200) . " PREPARE TRANSACTION 'p_$fate';");
	is($rc, 0, "prepared transaction ($fate to come)") or diag($err);
	# the rows are in the twin because ret and ch are heap tables written by the same
	# transaction; they appear with the rest of it at COMMIT PREPARED
	$node->stop('immediate');
	$node->start;
	is(q1($node, "SELECT count(*) FROM pg_prepared_xacts"), 1, "after the crash the prepared transaction is still there");
	is(q1($node, "SELECT count(*) FROM c WHERE a > " . ($next - 1201)), 0, "... and its rows are not visible");
	check_empty($node, "prepared, $fate to come");
	q1($node, "$fate PREPARED 'p_$fate'");
	if ($fate eq 'COMMIT')
	{
		same_as_twin($node, 'c', 'ch', 'commit prepared after a crash');
		tids_ok($node, 'c', 'ret', 'commit prepared after a crash');
	}
	else
	{
		is(q1($node, "SELECT count(*) FROM c WHERE a > " . ($next - 1201)), 0, "rollback prepared after a crash: no row");
		is(q1($node, "SELECT count(*) FROM ch WHERE a > " . ($next - 1201)), 0, "... and none in the twin");
	}
	check_empty($node, "$fate PREPARED after a crash");
	write_more("$fate PREPARED after a crash");
}

$node->stop;
done_testing();
