# contrib/kingres_columnar/t/047_reserve_concurrent.pl
#
# C6a: sessions that reserve row numbers at the same time.  Two and four sessions
# (pgbench, each transaction inserting a random number of rows, some of them over the
# stripe size, a tenth rolled back) against a heap twin: afterwards the rows are those
# of the twin, no two rows share a TID, every row has the TID its insert returned,
# the number ranges of the stripes do not overlap, no reserved row is left, and the
# check is clean.  Then the rule that matters for an index, with sessions that hold a
# reservation open at a point the developer setting names: a number that a live holder
# has reserved is never given to another row; a holder that is stopped between its
# reservation and its first row (point 2) and one that already wrote its pages (point 3)
# make no difference to a writer that commits meanwhile, and after the holders commit all
# the TIDs are different.
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

my $node = new_node('rconc', conf => "kingres_columnar.reserve_min_rows = 0
");
my $KEY = 778;

q1($node, q{
	CREATE TABLE c (a int, b text) USING columnar;
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE ch (a int, b text);
	CREATE TABLE ret (a int, t tid);
	CREATE SEQUENCE sq;
	-- the row number a TID stands for
	CREATE FUNCTION rownum(t tid) RETURNS bigint LANGUAGE sql IMMUTABLE AS
	  $$ SELECT ((t::text)::point)[0]::bigint * 2048 + ((t::text)::point)[1]::bigint - 1 $$;
});

# the stripes' number ranges do not overlap; the rows are in the stripes
sub ranges_ok
{
	my ($what, $tbl) = @_;
	my ($overlap, $sum, $rows) = split /\|/, q1($node, qq{
		SELECT (SELECT count(*) FROM (SELECT first_row, row_count, lead(first_row) OVER (ORDER BY first_row) nf
		                                FROM kingres.columnar_stripes('$tbl')) s WHERE first_row + row_count > nf),
		       (SELECT coalesce(sum(row_count), 0) FROM kingres.columnar_stripes('$tbl')),
		       (SELECT count(*) FROM $tbl)});
	is($overlap, 0, "$what: no two stripes have overlapping row numbers");
	is($sum, $rows, "$what: the stripes hold the $rows rows");
}

# ---------------------------------------------------------------------------
# pgbench, 2 and 4 sessions
# ---------------------------------------------------------------------------
my $script = $node->basedir . '/reserve.pgb';
append_to_file($script, q{\set n random(1, 1500)
\set w random(1, 10)
SET kingres_columnar.reserve_row_numbers = on;
BEGIN;
WITH s AS (SELECT nextval('sq')::int a FROM generate_series(1, :n)),
 i AS (INSERT INTO c SELECT a, 'v' || a FROM s RETURNING a, ctid),
 j AS (INSERT INTO ch SELECT a, 'v' || a FROM s)
 INSERT INTO ret SELECT a, ctid FROM i;
\if :w = 1
ROLLBACK;
\else
COMMIT;
\endif
});

for my $clients (2, 4)
{
	$node->command_ok([ 'pgbench', '-n', '-c', $clients, '-j', $clients, '-t', 25, '-f', $script, 'postgres' ],
		"$clients sessions reserve and insert at once");
	same_as_twin($node, 'c', 'ch', "$clients sessions");
	my $n = tids_ok($node, 'c', 'ret', "$clients sessions");
	ranges_ok("$clients sessions", 'c');
	check_empty($node, "$clients sessions");
	cmp_ok($n, '>', 1000 * $clients, "$clients sessions: $n rows in the table");
}

# ---------------------------------------------------------------------------
# holders
# ---------------------------------------------------------------------------
q1($node, q{
	CREATE TABLE hc (a int, b text) USING columnar;
	SELECT kingres.columnar_set_options('hc', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE hh (a int, b text);
	CREATE TABLE hret (a int, t tid);
});
sub hins
{
	my ($lo, $hi, $tag) = @_;
	return "WITH s AS (SELECT g a, '$tag' || g b FROM generate_series($lo, $hi) g), i AS (INSERT INTO hc SELECT a, b FROM s RETURNING a, ctid), j AS (INSERT INTO hh SELECT a, b FROM s) INSERT INTO hret SELECT a, ctid FROM i;";
}

for my $point (2, 3)
{
	my $base = $point * 100000;
	my $ctl = $node->background_psql('postgres');
	$ctl->query_safe("SELECT pg_advisory_lock($KEY, 0);");
	my $holder = $node->background_psql('postgres');
	# the holder: at point 2 it reserves and stops before its first row is put; at
	# point 3 it has written its pages and stops before it updates its catalog row
	my $hsql = $point == 2 ? "BEGIN; " . hins($base + 1, $base + 5, "holder") : "BEGIN; " . hins($base + 1, $base + 5, "holder") . " COMMIT;";
	$holder->query_until(qr/^go$/m, reserve_on() . "SET kingres_columnar.reserve_gate = $KEY; SET kingres_columnar.reserve_gate_at = $point;\n\\echo go\n$hsql\n");
	ok(wait_waiting($node, 1), "point $point: the holder waits");

	my $held_to = q1($node, "SELECT meta_next_row('hc')");
	cmp_ok($held_to, '>=', 1000, "point $point: the holder has taken numbers from the metapage (the next is $held_to)");
	# writers that commit while the holder lives
	my $bad = 0;
	for my $k (1 .. 12)
	{
		my ($rc, $out, $err) = rsql($node, "BEGIN; " . hins($base + 1000 * $k, $base + 1000 * $k + (7 * $k), "w$k") . " COMMIT;");
		if ($rc != 0) { $bad++; diag($err); }
	}
	is($bad, 0, "point $point: twelve writers commit while the holder lives");
	my $min = q1($node, "SELECT min(rownum(t)) FROM hret WHERE a > $base + 1000");
	cmp_ok($min, '>=', $held_to, "point $point: no row of the writers has a number that the holder reserved (smallest $min, the holder took up to $held_to)");
	is(q1($node, "SELECT count(DISTINCT t::text) FROM hret"), q1($node, "SELECT count(*) FROM hret"), "point $point: the TIDs of the writers are different");

	# let the holder go on
	$ctl->query_safe("SELECT pg_advisory_unlock($KEY, 0);");
	# at point 2 the holder is released inside its INSERT and commits; at point 3 inside its COMMIT
	$holder->query_safe("COMMIT;") if $point == 2;
	$holder->quit;
	$ctl->quit;
	same_as_twin($node, 'hc', 'hh', "point $point, after the holder");
	tids_ok($node, 'hc', 'hret', "point $point, after the holder");
	ranges_ok("point $point, after the holder", 'hc');
	check_empty($node, "point $point, after the holder");
	my $hmax = q1($node, "SELECT max(rownum(t)) FROM hret WHERE a BETWEEN $base + 1 AND $base + 5");
	cmp_ok($hmax, '<', $held_to, "point $point: the holder's rows are in the numbers it reserved");
}

$node->stop;
done_testing();
