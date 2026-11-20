# contrib/kingres_columnar/t/069_vacuum_merge_safe.pl
#
# C14-02, C14-03, C14-06: the merge of small stripes that VACUUM does is optional work and must
# not harm the VACUUM or the cluster.
#  1. C14-02: a table at the end of its row numbers with many small stripes is still vacuumed (the
#     merge is skipped; before, it failed with "no row numbers left" and so did every VACUUM), and an
#     error in the merge (the fault injection of an assertion build) is a WARNING, not a failed VACUUM.
#  2. C14-03: only stripes smaller than a sixteenth of the stripe size are merged (a stripe of a
#     tenth is left alone), and one VACUUM copies at most kingres_columnar.vacuum_compact_rows rows.
#  3. C14-06: while the merge holds its transaction id, that id is in the snapshot of another
#     session (a lazy VACUUM runs with PROC_IN_VACUUM, which keeps it out of snapshots; the
#     merge clears the flag).  The VACUUM is stopped after the merge by the developer gate.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1 wait_waiting);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

# the test patches the metapage on disk: no page checksum to recompute
my $node = new_node('vms', init => { extra => ['--no-data-checksums'] });

sub stripes { my ($t) = @_; return q1($node, "SELECT count(*) FROM kingres.columnar_stripes('$t')"); }

sub savepoint_loop
{
	my ($t, $n) = @_;
	q1($node, "CREATE TABLE $t (id int, k int, v text) USING columnar; CREATE UNIQUE INDEX ${t}_id ON $t (id); CREATE INDEX ${t}_k ON $t (k); CREATE TABLE ${t}h (id int, k int, v text)");
	q1($node, "DO \$\$ BEGIN
		INSERT INTO $t VALUES (0, 0, 'outer'); INSERT INTO ${t}h VALUES (0, 0, 'outer');
		FOR i IN 1..$n LOOP
		  BEGIN INSERT INTO $t VALUES (i, i % 17, 'row' || i); INSERT INTO ${t}h VALUES (i, i % 17, 'row' || i);
		  EXCEPTION WHEN OTHERS THEN NULL; END;
		END LOOP; END \$\$");
}

sub same_rows
{
	my ($t) = @_;
	return q1($node, "SELECT count(*) FROM (SELECT id, k, v FROM $t EXCEPT SELECT id, k, v FROM ${t}h) s") . '|'
	  . q1($node, "SELECT count(*) FROM (SELECT id, k, v FROM ${t}h EXCEPT SELECT id, k, v FROM $t) s");
}

# ---------------------------------------------------------------------------
# 1. the end of the numbers
# ---------------------------------------------------------------------------
my $max = (0xFFFFFFFE - 1) * 2048;    # KC_MAX_ROW_NUMBER
my @crct;
for my $i (0 .. 255)
{
	my $c = $i;
	for (1 .. 8) { $c = ($c & 1) ? (($c >> 1) ^ 0x82F63B78) : ($c >> 1); }
	$crct[$i] = $c;
}
sub crc32c
{
	my ($data) = @_;
	my $crc = 0xFFFFFFFF;
	$crc = $crct[($crc ^ $_) & 0xFF] ^ ($crc >> 8) for unpack('C*', $data);
	return $crc ^ 0xFFFFFFFF;
}
sub set_next_row
{
	my ($tbl, $value) = @_;
	my $path = $node->data_dir . '/' . q1($node, "SELECT pg_relation_filepath('$tbl')");
	$node->stop;
	open(my $fh, '+<:raw', $path) or die "cannot open $path: $!";
	my $page;
	read($fh, $page, 8192) == 8192 or die 'short read';
	substr($page, 24 + 24, 8) = pack('Q<', $value);
	my $meta = substr($page, 24, 104);
	substr($meta, 52, 4) = pack('L<', 0);
	substr($page, 24 + 52, 4) = pack('L<', crc32c($meta));
	seek($fh, 0, 0);
	print $fh $page;
	close($fh);
	$node->start;
}

savepoint_loop('endt', 200);
cmp_ok(stripes('endt'), '>=', 199, '1: 200 one-row subtransactions made stripes');
set_next_row('endt', $max - 10);
my ($rc, $out, $err) = $node->psql('postgres', "VACUUM endt");
is($rc, 0, '1: a table at the end of its row numbers with many small stripes is vacuumed') or diag($err);
unlike($err, qr/no row numbers left/, '1: and says nothing of the row numbers');
cmp_ok(stripes('endt'), '>=', 199, '1: the merge was skipped (its copies would need numbers there are not)');
is(same_rows('endt'), '0|0', '1: the rows are those of the heap twin');
($rc, $out, $err) = $node->psql('postgres', "VACUUM endt");
is($rc, 0, '1: and again') or diag($err);

# an error in the merge (assertion builds have the fault injection)
my $has_fault = q1($node, "SELECT count(*) FROM pg_settings WHERE name = 'kingres_columnar.fault_point'");
SKIP:
{
	skip 'the fault injection is compiled only in assertion builds', 6 if $has_fault ne '1';
	savepoint_loop('flt', 200);
	($rc, $out, $err) = $node->psql('postgres', "SET kingres_columnar.fault_point = 'alloc'; SET kingres_columnar.fault_nth = 1; VACUUM flt");
	is($rc, 0, '1: an error in the merge does not fail the VACUUM') or diag($err);
	like($err, qr/WARNING:.*merge of the small stripes of "flt" was given up/, '1: it is reported as a warning');
	cmp_ok(stripes('flt'), '>=', 199, '1: the stripes are as they were');
	is(same_rows('flt'), '0|0', '1: the rows are those of the heap twin');
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '1: columnar_check finds nothing');
	($rc, $out, $err) = $node->psql('postgres', "VACUUM flt");
	is($rc, 0, '1: the next VACUUM merges') or diag($err);
}
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('endt')"), 0, '1: the indexes are complete');

# ---------------------------------------------------------------------------
# 2. what is merged, and how much at a time
# ---------------------------------------------------------------------------
# 130 one-row stripes and 20 stripes of 10 000 rows (more than a sixteenth of 150 000)
q1($node, "CREATE TABLE mix (id int, k int, v text) USING columnar; CREATE UNIQUE INDEX mix_id ON mix (id); CREATE TABLE mixh (id int, k int, v text)");
q1($node, "DO \$\$ BEGIN
	INSERT INTO mix VALUES (0, 0, 'outer'); INSERT INTO mixh VALUES (0, 0, 'outer');
	FOR i IN 1..130 LOOP
	  BEGIN INSERT INTO mix VALUES (i, i, 'row'); INSERT INTO mixh VALUES (i, i, 'row'); EXCEPTION WHEN OTHERS THEN NULL; END;
	END LOOP; END \$\$");
for my $j (1 .. 20)
{
	my $lo = 1000 + $j * 10000;
	q1($node, "INSERT INTO mix SELECT g, g, 'big' FROM generate_series($lo, $lo + 9999) g; INSERT INTO mixh SELECT g, g, 'big' FROM generate_series($lo, $lo + 9999) g");
}
my $big_before = q1($node, "SELECT count(*) FROM kingres.columnar_stripes('mix') WHERE row_count = 10000");
is($big_before, 20, '2: 20 stripes of 10 000 rows');
q1($node, "VACUUM mix");
is(q1($node, "SELECT count(*) FROM kingres.columnar_stripes('mix') WHERE row_count = 10000"), 20, '2: a VACUUM leaves them alone (only stripes under a sixteenth are merged)');
cmp_ok(q1($node, "SELECT count(*) FROM kingres.columnar_stripes('mix') WHERE row_count < 10000"), '<=', 3, '2: and merged the small ones');
is(same_rows('mix'), '0|0', '2: the rows are those of the heap twin');

# the cap
savepoint_loop('cap', 400);
q1($node, "SET kingres_columnar.vacuum_compact_rows = 100; VACUUM cap");
my $s1 = stripes('cap');
cmp_ok($s1, '>=', 290, "2: with a cap of 100 rows one VACUUM merged about 100 of the 400 stripes ($s1 left)");
cmp_ok($s1, '<=', 310, '2: ... and not all of them');
q1($node, "SET kingres_columnar.vacuum_compact_rows = 100; VACUUM cap");
my $s2 = stripes('cap');
cmp_ok($s2, '<', $s1 - 50, "2: the next VACUUM merges more ($s2 left)");
q1($node, "SET kingres_columnar.vacuum_compact_rows = 100; VACUUM cap");
q1($node, "SET kingres_columnar.vacuum_compact_rows = 100; VACUUM cap");
cmp_ok(stripes('cap'), '>=', 100, '2: below the 128 small stripes the VACUUM merges no more');
is(same_rows('cap'), '0|0', '2: the rows are those of the heap twin');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('cap')"), 0, '2: the indexes are complete');

# ---------------------------------------------------------------------------
# 3. the merge's transaction id is in the snapshots of other sessions
# ---------------------------------------------------------------------------
savepoint_loop('gt', 300);
my $KEY = 783;
my $ctl = $node->background_psql('postgres');
$ctl->query_safe("SELECT pg_advisory_lock($KEY, 0);");
my $vac = $node->background_psql('postgres', on_error_stop => 0);
$vac->query_until(qr/^go$/m, "SET kingres_columnar.reserve_gate = $KEY; SET kingres_columnar.reserve_gate_at = 7;\n\\echo go\nVACUUM gt;\n");
ok(wait_waiting($node, 1), '3: the VACUUM waits after its merge');
my $xid = q1($node, "SELECT backend_xid FROM pg_stat_activity WHERE query LIKE 'VACUUM gt%' AND pid <> pg_backend_pid()");
ok($xid ne '', "3: the merging VACUUM has a transaction id ($xid)");
# a later transaction completes, so that the merge's id is below the snapshot's xmax and has to be listed (an id at or above xmax is running anyway)
q1($node, "SELECT pg_current_xact_id()");
is(q1($node, "SELECT count(*) FROM pg_snapshot_xip(pg_current_snapshot()) x WHERE x::text = '$xid'"), 1,
	'3: and another session sees it as running in its snapshot');
$ctl->query_safe("SELECT pg_advisory_unlock($KEY, 0);");
$vac->query("SELECT 'sync';");
cmp_ok(stripes('gt'), '<=', 5, '3: the merge was done');
is(same_rows('gt'), '0|0', '3: the rows are those of the heap twin');
$_->quit for ($ctl, $vac);

$node->stop;
done_testing();
