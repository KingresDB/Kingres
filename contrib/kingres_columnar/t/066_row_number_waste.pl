# contrib/kingres_columnar/t/066_row_number_waste.pl
#
# C13-02: the row numbers of an indexed table are not used up by aborted or interleaved writers.
# A table with an index reserves row numbers for a writer when it inserts a row.  Before this
# change a writer reserved a whole stripe's worth (150 000 by default) and an aborted one gave
# nothing back, so a role with INSERT could end the table (8.8 x 10^12 numbers) with about 59
# million aborted statements, in minutes.  Now:
#  1. a reservation starts small (kingres_columnar.reserve_min_rows, 64) and grows in place
#     while the buffer fills, when it is still the end of the numbers;
#  2. the unused tail of an aborted writer's reservation is given back at that backend's next
#     reservation when it is still the end of the numbers (never the numbers of rows that
#     were put in the buffer: their index entries name them);
#  3. what is not given back is at most the small reservation.
# Measured here: the next row number after aborted one-row inserts by one session, by two
# interleaved sessions, and after interleaved commits; the stripes of a bulk load; the warning
# at 90 percent and the error at the end of the numbers, and the recovery.
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

# the test patches the metapage on disk: no page checksum to recompute
my $node = new_node('rwaste', init => { extra => ['--no-data-checksums'] });
q1($node, q{
	CREATE TABLE w (id int, k int) USING columnar;
	CREATE INDEX w_k ON w (k);
	INSERT INTO w VALUES (0, 0);
});

sub next_row { return q1($node, "SELECT meta_next_row('w')"); }
my $base = next_row();

# ---------------------------------------------------------------------------
# 1. one session: aborted one-row inserts
# ---------------------------------------------------------------------------
q1($node, "DO \$\$ BEGIN FOR i IN 1..3000 LOOP BEGIN INSERT INTO w VALUES (i, i); RAISE EXCEPTION 'x'; EXCEPTION WHEN OTHERS THEN NULL; END; END LOOP; END \$\$");
my $used1 = next_row() - $base;
cmp_ok($used1, '<', 3000 * 3, "1: 3000 aborted subtransactions of one session used $used1 row numbers (about one each, the row's own number: the unused tail went back; before it was 150 000 each)");
# and as autocommit statements that fail
for my $i (1 .. 300)
{
	$node->psql('postgres', "INSERT INTO w SELECT g, 1 / (g - 2) FROM generate_series(1, 2) g");    # the first row is inserted, the second fails the statement
}
my $used1b = next_row() - $base - $used1;
# each psql is a new session, which cannot give back what the one before it dropped: at most the small reservation each
cmp_ok($used1b, '<', 300 * 70, "1: 300 failed statements, each in a session of its own, used $used1b");

# the numbers of rows that were put in are never given back: an index entry names them.  An aborted
# insert, then another: the second has a number after the first's, and the key of the aborted row
# finds nothing through the index (a number given back would be the second row's, and the stale
# entry would find it with the wrong key)
{
	# one session: the tail of the aborted reservation is given back by its next reservation
	my $out = q1($node, "BEGIN; INSERT INTO w VALUES (700001, 700001) RETURNING ctid; ROLLBACK; INSERT INTO w VALUES (700002, 700002) RETURNING ctid");
	my ($t1, $t2) = $out =~ /\((\d+,\d+)\)/g;
	isnt($t2, $t1, "1: the row inserted after an aborted one has another TID ($t1 aborted, $t2 committed)");
	is(q1($node, "SET enable_seqscan = off; SET enable_bitmapscan = off; SET kingres_columnar.enable_custom_scan = off; SELECT count(*) FROM w WHERE k = 700001"), 0,
		'1: the key of the aborted row finds nothing through the index');
	is(q1($node, "SET enable_seqscan = off; SET enable_bitmapscan = off; SET kingres_columnar.enable_custom_scan = off; SELECT id FROM w WHERE k = 700002"), 700002,
		'1: and the committed row is found by its key');
}

# ---------------------------------------------------------------------------
# 2. two sessions that interleave: nothing can be given back
# ---------------------------------------------------------------------------
my ($a, $b) = ($node->background_psql('postgres', on_error_stop => 0), $node->background_psql('postgres', on_error_stop => 0));
my $n2 = next_row();
for my $i (1 .. 300)
{
	$a->query("BEGIN; INSERT INTO w VALUES ($i, 1);");
	$b->query("BEGIN; INSERT INTO w VALUES ($i, 2);");
	$a->query("ROLLBACK;");
	$b->query("ROLLBACK;");
}
my $used2 = next_row() - $n2;
cmp_ok($used2, '<', 600 * 70, "2: 600 aborted transactions of two interleaved sessions used $used2 row numbers (at most the reservation each)");

# commits that interleave
my $n3 = next_row();
for my $i (1 .. 300)
{
	$a->query("BEGIN; INSERT INTO w VALUES ($i, 3);");
	$b->query("BEGIN; INSERT INTO w VALUES ($i, 4);");
	$a->query("COMMIT;");
	$b->query("COMMIT;");
}
my $used3 = next_row() - $n3;
cmp_ok($used3, '<', 600 * 70, "3: 600 committed transactions of two interleaved sessions used $used3 row numbers for 600 rows");
$a->quit;
$b->quit;

# ---------------------------------------------------------------------------
# 3. the stripes of a load are not made small by the small first reservation
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE bulk (id int, v text) USING columnar; CREATE UNIQUE INDEX bulk_id ON bulk (id)");
q1($node, "INSERT INTO bulk SELECT g, md5(g::text) FROM generate_series(1, 20000) g");
is(q1($node, "SELECT count(*) FROM kingres.columnar_stripes('bulk')"), 1, '4: a load of 20 000 rows by one session is one stripe');
q1($node, "INSERT INTO bulk SELECT g, md5(g::text) FROM generate_series(100001, 400000) g");
cmp_ok(q1($node, "SELECT count(*) FROM kingres.columnar_stripes('bulk')"), '<=', 4, '4: a load of 300 000 rows is a few stripes: ' . q1($node, "SELECT stripes_str('bulk')"));
is(q1($node, "SELECT count(*) FROM bulk"), 320000, '4: all the rows are there');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('bulk')"), 0, '4: the index is complete');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '4: columnar_check finds nothing');

# two loaders at once
my ($l1, $l2) = ($node->background_psql('postgres', on_error_stop => 0), $node->background_psql('postgres', on_error_stop => 0));
$l1->query("BEGIN; INSERT INTO bulk SELECT g, 'a' FROM generate_series(1000001, 1010000) g;");
$l2->query("BEGIN; INSERT INTO bulk SELECT g, 'b' FROM generate_series(2000001, 2010000) g;");
$l1->query("INSERT INTO bulk SELECT g, 'a' FROM generate_series(1010001, 1030000) g;");
$l2->query("INSERT INTO bulk SELECT g, 'b' FROM generate_series(2010001, 2030000) g;");
$l1->query("COMMIT;");
$l2->query("COMMIT;");
$l1->quit;
$l2->quit;
is(q1($node, "SELECT count(*) FROM bulk WHERE id > 1000000"), 60000, '4: two loaders at once: all rows are there');
is(q1($node, "SELECT count(*) - count(DISTINCT id) FROM bulk"), 0, '4: no key twice');
is(q1($node, "SELECT count(*) - count(DISTINCT ctid::text) FROM bulk"), 0, '4: no TID twice (two reservations never overlap)');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '4: columnar_check finds nothing');

# ---------------------------------------------------------------------------
# 4. the end of the numbers: the warning at 90 percent, the error, the recovery
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
q1($node, "CREATE TABLE endt (id int, k int) USING columnar; CREATE INDEX endt_k ON endt (k); INSERT INTO endt VALUES (1, 1)");
set_next_row('endt', int($max * 0.91));
my ($rc, $out, $err) = $node->psql('postgres', "INSERT INTO endt VALUES (2, 2)");
is($rc, 0, '5: an insert after 90 percent of the numbers is used still works');
like($err, qr/WARNING:.*has used 9\d percent of its row numbers/, '5: and warns that the numbers are running out');
like($err, qr/VACUUM FULL|SET ACCESS METHOD/, '5: with the way out');
($rc, $out, $err) = $node->psql('postgres', "INSERT INTO endt VALUES (3, 3); INSERT INTO endt VALUES (4, 4); BEGIN; INSERT INTO endt VALUES (5, 5); COMMIT");
is(scalar(() = $err =~ /WARNING:.*row numbers/g), 1, '5: the warning is given once in a session');
set_next_row('endt', $max - 10);
($rc, $out, $err) = $node->psql('postgres', "INSERT INTO endt SELECT g, g FROM generate_series(1, 100) g");
isnt($rc, 0, '5: an insert that needs more numbers than are left fails');
like($err, qr/has no row numbers left/, '5: with a clear message');
like($err, qr/VACUUM FULL|SET ACCESS METHOD/, '5: and the way out');
($rc, $out, $err) = $node->psql('postgres', "VACUUM FULL endt");
is($rc, 0, '5: VACUUM FULL rewrites the table') or diag($err);
($rc, $out, $err) = $node->psql('postgres', "INSERT INTO endt SELECT g, g FROM generate_series(1, 100) g");
is($rc, 0, '5: and the inserts work again') or diag($err);
cmp_ok(q1($node, "SELECT meta_next_row('endt')"), '<', 10000, '5: from small numbers');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('endt')"), 0, '5: the index is complete');

$node->stop;
done_testing();
