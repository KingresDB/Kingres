# contrib/kingres_columnar/t/017_stripe_sizes.pl
#
# C4-03.  The number and size of the stripes of a table depend on the amount
# of data and on the options, not on how the rows were grouped into
# subtransactions or what else the transaction did.  The defect: once the
# buffers that committed subtransactions had merged into their parent used
# up write_buffer_memory, every later row was flushed as a stripe of its own
# (one exception block per batch of 1000 rows gave 14 519 stripes and 113 MB
# for what one INSERT writes as 2 stripes).
#
# The bounds are derived from the options: a table with stripe_row_count S
# and chunk_row_count C gets, from rows written in any grouping, stripes of
# S rows, plus stripes that memory pressure asked for, none of which has
# fewer than max(C, 1000) rows (kc_min_flush_rows), except the last one of
# the transaction.  The memory of the write buffers stays within a small
# multiple of write_buffer_memory.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colsizes');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub script
{
	my ($sql) = @_;
	my ($out, $err) = ('', '');

	$node->psql('postgres', $sql, stdout => \$out, stderr => \$err,
		on_error_stop => 0);
	return ($out, $err);
}

sub fresh
{
	my ($srows, $crows) = @_;

	$node->safe_psql('postgres', "
		DROP TABLE IF EXISTS tm;
		CREATE TABLE tm (a int, b text) USING columnar;
		SELECT kingres.columnar_set_options('tm', stripe_row_count => $srows,
											chunk_row_count => $crows);");
}

# (stripes, rows, smallest stripe except the last, largest)
sub shape
{
	return $node->safe_psql('postgres', "
		WITH s AS (SELECT row_number() OVER (ORDER BY stripe_id DESC) rn, row_count
		             FROM kingres.columnar_stripes('tm'))
		SELECT count(*) || ',' || coalesce(sum(row_count), 0) || ',' ||
		       coalesce(min(row_count) FILTER (WHERE rn > 1), 0) || ',' || coalesce(max(row_count), 0)
		  FROM s");
}

# ------------------------------------------------------------------------
# Memory pressure: 100 blocks of 500 wide rows, 1 MB allowance
# ------------------------------------------------------------------------

my $WIDE = "SELECT g, repeat('x', 200) FROM generate_series(1, 500) g";
my $PEAKS = "SELECT coalesce(sum(total_bytes), 0) INTO m FROM pg_backend_memory_contexts
  WHERE name IN ('kingres_columnar rows', 'kingres_columnar write buffers');";

sub mem_case
{
	my ($tag, $between, $before) = @_;

	fresh(20000, 500);
	my ($out, $err) = script("
SET kingres_columnar.write_buffer_memory = '1MB';
CREATE TEMP TABLE peak (v bigint);
BEGIN;
$before
DO \$\$ DECLARE m bigint; BEGIN FOR i IN 1..100 LOOP
  BEGIN INSERT INTO tm $WIDE; EXCEPTION WHEN division_by_zero THEN NULL; END;
  $between
  $PEAKS
  INSERT INTO peak VALUES (m);
END LOOP; END \$\$;
COMMIT;
SELECT 'PEAK=' || max(v) FROM peak;");
	my ($peak) = $out =~ /PEAK=(\d+)/;
	return ($peak, shape());
}

my ($single_peak, $single_shape);
{
	fresh(20000, 500);
	my ($out) = script("
SET kingres_columnar.write_buffer_memory = '1MB';
CREATE TEMP TABLE peak (v bigint);
BEGIN;
INSERT INTO tm SELECT g, repeat('x', 200) FROM generate_series(1, 50000) g;
COMMIT;");
	$single_shape = shape();
}
my ($sstripes, $srows, $ssmall) = split /,/, $single_shape;
is($srows, 50000, 'one INSERT of 50000 rows: all rows are in stripes');

my %cases = (
	'exception blocks in a loop' => ['', ''],
	'reads of the table between the blocks' => ['PERFORM count(*) FROM tm;', ''],
	'a read of an unrelated table between the blocks' => ['PERFORM count(*) FROM pg_class;', ''],
	'the parent wrote first' => ['', "INSERT INTO tm SELECT g, repeat('x', 200) FROM generate_series(1, 700) g;"],
);
foreach my $name (sort keys %cases)
{
	my ($peak, $shape) = mem_case($name, @{ $cases{$name} });
	my ($n, $rows, $small, $large) = split /,/, $shape;
	my $expect_rows = $name eq 'the parent wrote first' ? 50700 : 50000;
	is($rows, $expect_rows, "$name: no row lost or doubled");
	cmp_ok($n, '<=', $sstripes * 2 + 2,
		"$name: $n stripes against $sstripes for the same rows in one statement");
	cmp_ok($small, '>=', 1000, "$name: no stripe but the last is smaller than max(chunk rows, 1000) (smallest $small)");
	cmp_ok($large, '<=', 20000, "$name: no stripe above the table's stripe size ($large)");
	cmp_ok($peak, '<=', 4 * 1024 * 1024,
		"$name: the write buffers peak at $peak bytes, within four times the 1 MB allowance");
}

# ------------------------------------------------------------------------
# Row-count stripes: the same rows in any grouping give the same stripes
# ------------------------------------------------------------------------

sub rows_in_blocks
{
	my ($block, $total, $nest) = @_;
	my $n = int($total / $block);
	my $body = $nest
		? "BEGIN BEGIN BEGIN INSERT INTO tm SELECT g, 'v' FROM generate_series(1, $block) g;
		   EXCEPTION WHEN division_by_zero THEN NULL; END; EXCEPTION WHEN division_by_zero THEN NULL; END;
		   EXCEPTION WHEN division_by_zero THEN NULL; END;"
		: "BEGIN INSERT INTO tm SELECT g, 'v' FROM generate_series(1, $block) g;
		   EXCEPTION WHEN division_by_zero THEN NULL; END;";
	fresh(1000, 100);
	script("BEGIN;
DO \$\$ BEGIN FOR i IN 1..$n LOOP $body END LOOP; END \$\$;
COMMIT;");
	return shape();
}

foreach my $block (1, 7, 100, 999, 1000, 5500)
{
	foreach my $nest (0, 1)
	{
		next if $block > 100 && $nest;
		next if $block == 1 && $nest;
		my $total = $block * int((5500 + $block - 1) / $block);
		my $shape = rows_in_blocks($block, $total, $nest);
		my $nstripes = int(($total + 999) / 1000);
		my $expect = "$nstripes,$total," . ($nstripes > 1 ? 1000 : 0) . "," . ($total >= 1000 ? 1000 : $total);
		is($shape, $expect, "blocks of $block rows ($total in all)" . ($nest ? ' three levels deep' : '') .
			': full stripes and the rest, as one statement writes them');
	}
}

{
	fresh(1000, 100);
	script("INSERT INTO tm SELECT g, 'v' FROM generate_series(1, 5500) g;");
	is(shape(), '6,5500,1000,1000', 'one INSERT of 5500 rows: the reference shape');
}

# a cursor open on the table keeps its buffers whole: nothing is flushed under
# it, and when it is closed the rows still become stripes of the proper size
{
	fresh(1000, 100);
	my ($out, $err) = script("
BEGIN;
INSERT INTO tm SELECT g, 'v' FROM generate_series(1, 10) g;
DECLARE c CURSOR FOR SELECT a FROM tm;
FETCH 1 FROM c;
DO \$\$ BEGIN FOR i IN 1..55 LOOP BEGIN INSERT INTO tm SELECT g, 'v' FROM generate_series(1, 100) g;
 EXCEPTION WHEN division_by_zero THEN NULL; END; END LOOP; END \$\$;
CLOSE c;
COMMIT;");
	unlike($err, qr/ERROR/, 'blocks under an open cursor: no error');
	my ($n, $rows, $small, $large) = split /,/, shape();
	is($rows, 5510, 'blocks under an open cursor: no row lost');
	cmp_ok($n, '<=', 6, "blocks under an open cursor: $n stripes for 5510 rows");
	cmp_ok($large, '<=', 1000, 'blocks under an open cursor: no stripe above the stripe size');
}

# the visibility of rows whose subtransaction committed into a parent that is
# still open: a stripe written for them is the parent's, and goes with the
# parent.  Heap twin: the same script on a heap table.
foreach my $end ('COMMIT', 'ROLLBACK')
{
	my %res;
	foreach my $am (qw(heap columnar))
	{
		$node->safe_psql('postgres', "DROP TABLE IF EXISTS tv; CREATE TABLE tv (a int) USING $am;"
			. ($am eq 'columnar' ? "SELECT kingres.columnar_set_options('tv', stripe_row_count => 1000, chunk_row_count => 100);" : ''));
		my ($out, $err) = script("
BEGIN;
INSERT INTO tv SELECT generate_series(1, 900);
SAVEPOINT s1;
INSERT INTO tv SELECT generate_series(1001, 1150);
SELECT 'IN1=' || count(*) FROM tv;
SAVEPOINT s2;
INSERT INTO tv SELECT generate_series(2001, 2500);
ROLLBACK TO s2;
INSERT INTO tv SELECT generate_series(3001, 3200);
RELEASE s1;
SELECT 'IN2=' || count(*) || ',' || sum(a) FROM tv;
DO \$\$ BEGIN BEGIN INSERT INTO tv SELECT generate_series(4001, 4900); PERFORM 1/0; EXCEPTION WHEN division_by_zero THEN NULL; END; END \$\$;
SELECT 'IN3=' || count(*) || ',' || sum(a) FROM tv;
$end;
SELECT 'OUT=' || count(*) || ',' || coalesce(sum(a), 0) FROM tv;");
		$res{$am} = join(' ', $out =~ /((?:IN\d|OUT)=\S+)/g);
	}
	is($res{columnar}, $res{heap}, "$end: what the transaction sees and what is left equal the heap twin ($res{heap})");
}

$node->stop;
done_testing();
