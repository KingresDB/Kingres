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


# ------------------------------------------------------------------------
# C5-01.  The memory allowance holds whatever the width of the rows and
# however they are grouped into statements and subtransactions.  The defect:
# the flush at the commit of a subtransaction asked for a number of rows
# (max(chunk_row_count, 1000)) and never for bytes, so a loop of exception
# blocks that wrote rows of 100 kB kept chunk_row_count rows in memory whatever
# write_buffer_memory said (954 MB against 128 MB in the review).
#
# The bound (README, "Memory"): the buffers of the running subtransaction are
# written as soon as the buffers together are over the allowance, so what is
# held is at most the allowance and the row being written.  What enclosing
# subtransactions hold cannot be written by an inner one: it counts, and the
# inner level writes its own buffers once they are half of the allowance, so an
# open level that holds rows adds less than half of it.  The measure is the
# memory contexts of the buffers, sampled after every statement and every
# block, so it includes the allocator's unused part of a block.
# ------------------------------------------------------------------------

$node->safe_psql('postgres', q{
CREATE FUNCTION kcmem() RETURNS bigint LANGUAGE sql AS
$$ SELECT coalesce(sum(total_bytes), 0)::bigint FROM pg_backend_memory_contexts
    WHERE name IN ('kingres_columnar rows', 'kingres_columnar write buffers') $$;
CREATE FUNCTION kcnote() RETURNS bool LANGUAGE sql AS
$$ SELECT set_config('kc.peak', greatest(coalesce(nullif(current_setting('kc.peak', true), '')::bigint, 0), kcmem())::text, false) IS NOT NULL $$;
});

my $MB = 1024 * 1024;
my $ALLOW = 8;			# MB
my $PAD = "repeat('x', 100000)";

sub wide_fresh
{
	my ($tables) = @_;

	foreach my $t (@$tables)
	{
		$node->safe_psql('postgres', "DROP TABLE IF EXISTS $t; CREATE TABLE $t (id int, pad text) USING columnar;");
	}
}

# runs a DO block body that keeps the peak in the variable peak; returns
# (peak, rows, stripes, rows of the smallest stripe but the last)
sub wide_case
{
	my ($tables, $setup, $body, $after) = @_;

	wide_fresh($tables);
	$after //= '';
	my $t = $tables->[0];
	my ($out, $err) = script("
SET kingres_columnar.write_buffer_memory = '${ALLOW}MB';
BEGIN;
$setup
DO \$\$ DECLARE peak bigint := 0; m bigint; BEGIN
$body
RAISE NOTICE 'PEAK=%', peak; END \$\$;
$after
COMMIT;");
	my ($peak) = $err =~ /PEAK=(\d+)/;
	my $rows = $node->safe_psql('postgres', "SELECT sum(c) FROM (" .
		join(' UNION ALL ', map { "SELECT count(*) c FROM $_" } @$tables) . ") s");
	my $shape = $node->safe_psql('postgres', "
		WITH s AS (SELECT row_number() OVER (ORDER BY stripe_id DESC) rn, row_count FROM kingres.columnar_stripes('$t'))
		SELECT count(*) || ',' || coalesce(min(row_count) FILTER (WHERE rn > 1), 0) FROM s");
	my ($nstripes, $small) = split /,/, $shape;
	unlike($err, qr/ERROR/, "the case runs without an error") or diag($err);
	return ($peak, $rows, $nstripes, $small);
}

my $SAMPLE = 'm := kcmem(); IF m > peak THEN peak := m; END IF;';
my $WITHIN = $ALLOW * $MB + 1 * $MB;		# the allowance, a row and an allocator block

# plain statements
{
	my ($peak, $rows, $n, $small) = wide_case(['tw'], '',
		"FOR i IN 1..400 LOOP INSERT INTO tw VALUES (i, $PAD); $SAMPLE END LOOP;");
	is($rows, 400, 'wide rows, one statement each: no row lost');
	cmp_ok($peak, '<=', $WITHIN, "wide rows, one statement each: peak $peak bytes, within the allowance and a row");
	cmp_ok($small, '>=', 40, "wide rows, one statement each: no stripe but the last under half the allowance ($small rows)");
}

# one statement, sampled inside it
{
	wide_fresh(['tw']);
	my ($out, $err) = script("
SET kingres_columnar.write_buffer_memory = '${ALLOW}MB';
SELECT set_config('kc.peak', '0', false);
INSERT INTO tw SELECT g, $PAD FROM generate_series(1, 300) g WHERE kcnote();
SELECT 'PEAK=' || current_setting('kc.peak');");
	my ($peak) = $out =~ /PEAK=(\d+)/;
	cmp_ok($peak, '<=', $WITHIN, "one INSERT of 300 wide rows: peak $peak bytes, within the allowance and a row");
	is($node->safe_psql('postgres', 'SELECT count(*) FROM tw'), 300, 'one INSERT of 300 wide rows: no row lost');
}

# the loop of exception blocks of the review
{
	my ($peak, $rows, $n, $small) = wide_case(['tw'], '',
		"FOR i IN 1..400 LOOP BEGIN INSERT INTO tw VALUES (i, $PAD); EXCEPTION WHEN division_by_zero THEN NULL; END; $SAMPLE END LOOP;");
	is($rows, 400, 'exception blocks, wide rows: no row lost');
	cmp_ok($peak, '<=', $WITHIN, "exception blocks, wide rows: peak $peak bytes, within the allowance and a row");
	cmp_ok($n, '<=', 12, "exception blocks, wide rows: $n stripes for 400 rows of 100 kB");
	cmp_ok($small, '>=', 40, "exception blocks, wide rows: no stripe but the last under half the allowance ($small rows)");
}

# narrow and wide in one block
{
	my ($peak, $rows, $n, $small) = wide_case(['tw'], '',
		"FOR i IN 1..300 LOOP BEGIN INSERT INTO tw SELECT g, repeat('y', 200) FROM generate_series(1, 10) g;
		  INSERT INTO tw VALUES (i, $PAD); EXCEPTION WHEN division_by_zero THEN NULL; END; $SAMPLE END LOOP;");
	is($rows, 3300, 'narrow and wide rows in each block: no row lost');
	cmp_ok($peak, '<=', $WITHIN, "narrow and wide rows in each block: peak $peak bytes");
	cmp_ok($small, '>=', 40, "narrow and wide rows in each block: no stripe but the last under 40 rows ($small)");
}

# narrow rows in small blocks: the stripes stay as large as the minimum rule says
{
	my ($peak, $rows, $n, $small) = wide_case(['tn'], '',
		"FOR i IN 1..6000 LOOP BEGIN INSERT INTO tn SELECT g, repeat('y', 200) FROM generate_series(1, 7) g;
		  EXCEPTION WHEN division_by_zero THEN NULL; END; $SAMPLE END LOOP;");
	is($rows, 42000, 'narrow rows in blocks of 7: no row lost');
	cmp_ok($peak, '<=', 1.5 * $ALLOW * $MB, "narrow rows in blocks of 7: peak $peak bytes, within 1.5 times the allowance");
	cmp_ok($small, '>=', 1000, "narrow rows in blocks of 7: no stripe but the last under 1000 rows ($small)");
}

# three levels of subtransactions, the outer ones holding little
{
	my ($peak, $rows) = wide_case(['tw'], '',
		"BEGIN INSERT INTO tw VALUES (0, $PAD);
		  BEGIN INSERT INTO tw VALUES (0, $PAD);
		    FOR i IN 1..300 LOOP BEGIN INSERT INTO tw VALUES (i, $PAD); EXCEPTION WHEN division_by_zero THEN NULL; END; $SAMPLE END LOOP;
		  EXCEPTION WHEN division_by_zero THEN NULL; END;
		EXCEPTION WHEN division_by_zero THEN NULL; END;");
	is($rows, 302, 'three levels, wide rows: no row lost');
	cmp_ok($peak, '<=', $WITHIN, "three levels, wide rows: peak $peak bytes");
}

# three levels, the outer one holding almost the allowance: what it holds cannot
# be written by the levels inside, and the middle level adds less than half
{
	my ($peak, $rows) = wide_case(['tw'], "INSERT INTO tw SELECT g, $PAD FROM generate_series(1, 70) g;",
		"BEGIN
		  BEGIN
		    FOR i IN 1..300 LOOP BEGIN INSERT INTO tw VALUES (i, $PAD); EXCEPTION WHEN division_by_zero THEN NULL; END; $SAMPLE END LOOP;
		  EXCEPTION WHEN division_by_zero THEN NULL; END;
		EXCEPTION WHEN division_by_zero THEN NULL; END;");
	is($rows, 370, 'the outer level holds 7 MB: no row lost');
	cmp_ok($peak, '<=', 1.5 * $ALLOW * $MB + 1 * $MB,
		"the outer level holds 7 MB: peak $peak bytes, less than the allowance and half of it more");
}

# a cursor open on the table: the buffer it reads is held, the rows after it are
# written under the allowance
{
	my ($peak, $rows, $n, $small) = wide_case(['tw'],
		"INSERT INTO tw SELECT g, $PAD FROM generate_series(1, 10) g;
		 DECLARE c CURSOR FOR SELECT id FROM tw; FETCH 1 FROM c;",
		"FOR i IN 1..400 LOOP BEGIN INSERT INTO tw VALUES (100 + i, $PAD); EXCEPTION WHEN division_by_zero THEN NULL; END;
		  INSERT INTO tw VALUES (1000 + i, $PAD); $SAMPLE END LOOP;",
		"FETCH ALL FROM c;");
	is($rows, 810, 'a cursor open on the table: no row lost');
	cmp_ok($peak, '<=', $WITHIN + 1 * $MB,
		"a cursor open on the table: peak $peak bytes, the allowance, the 10 rows it holds and a row");
	cmp_ok($small, '>=', 10, "a cursor open on the table: no stripe but the last under 10 rows ($small)");
	wide_fresh(['tw']);
	my ($out) = script("
BEGIN;
INSERT INTO tw SELECT g, $PAD FROM generate_series(1, 10) g;
DECLARE c CURSOR FOR SELECT id FROM tw ORDER BY id;
FETCH 3 FROM c;
SET kingres_columnar.write_buffer_memory = '1MB';
INSERT INTO tw SELECT g, $PAD FROM generate_series(101, 140) g;
FETCH ALL FROM c;
COMMIT;");
	my @ids = $out =~ /^(\d+)$/mg;
	is(join(',', @ids), '1,2,3,4,5,6,7,8,9,10',
		'a cursor open while rows are written and flushed: it returns the rows it began with, once each');
}

# many tables, each holding a little
{
	my @t = map { "tm$_" } 1 .. 24;
	wide_fresh(\@t);
	my ($out, $err) = script("
SET kingres_columnar.write_buffer_memory = '${ALLOW}MB';
BEGIN;
DO \$\$ DECLARE peak bigint := 0; m bigint; BEGIN FOR i IN 1..480 LOOP
  EXECUTE format('INSERT INTO tm%s VALUES (%s, repeat(''x'', 100000))', 1 + i % 24, i); $SAMPLE
END LOOP; RAISE NOTICE 'PEAK=%', peak; END \$\$;
COMMIT;");
	my ($peak) = $err =~ /PEAK=(\d+)/;
	cmp_ok($peak, '<=', $WITHIN, "24 tables, wide rows in turn: peak $peak bytes");
	is($node->safe_psql('postgres', 'SELECT sum(c) FROM (' . join(' UNION ALL ', map { "SELECT count(*) c FROM $_" } @t) . ') s'),
		480, '24 tables, wide rows in turn: no row lost');
}

$node->stop;
done_testing();
