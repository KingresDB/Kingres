# contrib/kingres_columnar/t/020_limits_flags_check.pl
#
# The tests that the fourth review found missing (C4-05) and the report of
# columnar_check after a rewrite (C4-06).
#
# Part 1 (C4-06): columnar_check reports nothing after VACUUM FULL, a
# rewriting ALTER TABLE, a change of access method in either direction,
# TRUNCATE, DROP and a compaction; the stripe rows of the replaced file are
# removed in the transaction that replaced it, and come back with a rollback.
#
# Part 2 (C4-05): the reader's limit on the rows of a stripe.  The limit is
# the writer's own, KC_MAX_STRIPE_ROWS (10 000 000): a catalog row at the limit
# gets as far as the stripe and is refused by the checks against its header,
# one above it is refused from the catalog row alone, before anything is
# allocated from the count.
#
# Part 3 (C4-05): a stripe catalog of another shape is refused with a clear
# error, never read with a guess.
#
# Part 4 (C4-05): the maintenance flag.  Compaction copies rows through a
# buffer of its own, so no hand-over of a subtransaction's buffers can mix its
# rows with a user's: the stripes compaction writes are all maintenance
# stripes, every other stripe is not, whatever the subtransactions did.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('collimits');
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

sub check_report
{
	return $node->safe_psql('postgres',
		"SELECT coalesce(string_agg(check_name || ' ' || coalesce(relation::text, '-') || ' ' || detail, '; '), '') FROM kingres.columnar_check()");
}

# ------------------------------------------------------------------------
# Part 1: columnar_check after the commands that replace a file
# ------------------------------------------------------------------------

$node->safe_psql('postgres', "
	CREATE TABLE r1 (a int, b text) USING columnar;
	INSERT INTO r1 SELECT g, 'x' FROM generate_series(1, 3000) g;
	INSERT INTO r1 VALUES (0, 'y');
	INSERT INTO r1 VALUES (-1, 'z');");

my @steps = (
	['VACUUM FULL', 'VACUUM FULL r1'],
	['ALTER COLUMN TYPE', 'ALTER TABLE r1 ALTER COLUMN a TYPE bigint'],
	['ADD COLUMN with a volatile default', 'ALTER TABLE r1 ADD COLUMN c float DEFAULT random()'],
	['SET ACCESS METHOD heap', 'ALTER TABLE r1 SET ACCESS METHOD heap'],
	['SET ACCESS METHOD columnar', 'ALTER TABLE r1 SET ACCESS METHOD columnar'],
	['columnar_compact', "SELECT kingres.columnar_compact('r1', 100000)"],
	['TRUNCATE', 'TRUNCATE r1'],
	['DROP TABLE', 'DROP TABLE r1'],
);
foreach my $s (@steps)
{
	my ($name, $sql) = @$s;
	$node->safe_psql('postgres', $sql);
	is(check_report(), '', "columnar_check reports nothing after $name");
	$node->safe_psql('postgres', "INSERT INTO r1 SELECT g, 'x' FROM generate_series(1, 2000) g")
		if $name eq 'TRUNCATE' || $name eq 'columnar_compact';
}

# a rewrite that is rolled back: the rows of the file that stays are still read
{
	$node->safe_psql('postgres', "
		CREATE TABLE r2 (a int) USING columnar;
		INSERT INTO r2 SELECT generate_series(1, 500);");
	my $before = $node->safe_psql('postgres', "SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('r2')");
	script("BEGIN; ALTER TABLE r2 ALTER COLUMN a TYPE bigint; ROLLBACK;");
	is($node->safe_psql('postgres', 'SELECT count(*) FROM r2'), 500, 'a rolled-back rewrite: all rows still read');
	is($node->safe_psql('postgres', "SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('r2')"),
		$before, 'a rolled-back rewrite: the stripe rows are back');
	is(check_report(), '', 'a rolled-back rewrite: nothing to report');
	script("BEGIN; VACUUM FULL r2; ROLLBACK;");
	is($node->safe_psql('postgres', 'SELECT count(*) FROM r2'), 500, 'rows after a refused VACUUM FULL in a block');
}

# ------------------------------------------------------------------------
# Part 2: the reader's limit on the rows of a stripe
# ------------------------------------------------------------------------

$node->safe_psql('postgres', "
	CREATE TABLE lim (a int) USING columnar;
	INSERT INTO lim SELECT generate_series(1, 2000);");
my $uid = $node->safe_psql('postgres', "SELECT kingres.columnar_identity('lim')");

foreach my $n (10000000)
{
	$node->safe_psql('postgres', "UPDATE kingres.columnar_stripe SET row_count = $n WHERE rel_uid = $uid");
	my ($out, $err) = script('SELECT count(*) FROM lim');
	like($err, qr/disagrees with the catalog|do not add up/, "a stripe row count at the limit ($n) reaches the stripe and is refused there");
	unlike($err, qr/stripe catalog row of stripe \d+ is corrupt/, "at the limit the catalog row itself is accepted");
}
foreach my $n (10000001, 40000001, 2147483647)
{
	$node->safe_psql('postgres', "UPDATE kingres.columnar_stripe SET row_count = $n WHERE rel_uid = $uid");
	my ($out, $err) = script('SELECT count(*) FROM lim');
	like($err, qr/the stripe catalog row of stripe \d+ is corrupt/, "a stripe row count of $n is refused from the catalog row, before any allocation");
	($out, $err) = script("SELECT kingres.columnar_compact('lim', 100)");
	like($err, qr/corrupt/, "compaction of a stripe of $n rows is refused");
}
$node->safe_psql('postgres', "UPDATE kingres.columnar_stripe SET row_count = 2000 WHERE rel_uid = $uid");
is($node->safe_psql('postgres', 'SELECT count(*) FROM lim'), 2000, 'the stripe reads again with its true row count');

# the writer cannot be set above the reader: the largest stripe option is the limit
my ($oout, $oerr) = script("SELECT kingres.columnar_set_options('lim', stripe_row_count => 10000001)");
like($oerr, qr/ERROR/, 'the stripe size option cannot exceed the reader\'s limit');

# ------------------------------------------------------------------------
# Part 3: a stripe catalog of another shape
# ------------------------------------------------------------------------

{
	my $cols13 = 'rel_uid bigint NOT NULL, stripe_id bigint NOT NULL, first_row bigint NOT NULL, '
	  . 'row_count int NOT NULL, chunk_groups int NOT NULL, natts smallint NOT NULL, '
	  . 'first_block bigint NOT NULL, block_count int NOT NULL, blob_len bigint NOT NULL, '
	  . 'raw_bytes bigint NOT NULL, footer_offset bigint NOT NULL, cid_points bytea NOT NULL, '
	  . 'col_stats bytea NOT NULL';
	$node->safe_psql('postgres', "
		CREATE TABLE shp (a int) USING columnar;
		INSERT INTO shp SELECT generate_series(1, 100);
		ALTER EVENT TRIGGER kingres_columnar_check_ddl DISABLE;
		ALTER EVENT TRIGGER kingres_columnar_on_drop DISABLE;
		ALTER TABLE kingres.columnar_stripe RENAME TO columnar_stripe_14;
		ALTER TABLE kingres.columnar_stripe_14 RENAME CONSTRAINT columnar_stripe_pkey TO columnar_stripe_14_pkey;
		CREATE TABLE kingres.columnar_stripe ($cols13, CONSTRAINT columnar_stripe_pkey PRIMARY KEY (rel_uid, stripe_id));
		INSERT INTO kingres.columnar_stripe
		  SELECT rel_uid, stripe_id, first_row, row_count, chunk_groups, natts, first_block, block_count,
		         blob_len, raw_bytes, footer_offset, cid_points, col_stats FROM kingres.columnar_stripe_14;");
	my ($out, $err) = script('SELECT count(*) FROM shp');
	like($err, qr/the stripe catalog has 13 columns, this build uses 14/, 'a catalog of the earlier shape (13 columns) is refused with an error that says so');
	unlike($out, qr/^\s*\d+\s*$/m, 'and no count is answered');
	($out, $err) = script('INSERT INTO shp VALUES (1)');
	like($err, qr/the stripe catalog has 13 columns/, 'a write into such a table is refused too');
	$node->safe_psql('postgres', "
		ALTER EVENT TRIGGER kingres_columnar_on_drop DISABLE;
		DROP TABLE kingres.columnar_stripe;
		ALTER TABLE kingres.columnar_stripe_14 RENAME TO columnar_stripe;
		ALTER TABLE kingres.columnar_stripe RENAME CONSTRAINT columnar_stripe_14_pkey TO columnar_stripe_pkey;
		ALTER EVENT TRIGGER kingres_columnar_check_ddl ENABLE ALWAYS;
		ALTER EVENT TRIGGER kingres_columnar_on_drop ENABLE ALWAYS;");
	is($node->safe_psql('postgres', 'SELECT count(*) FROM shp'), 100, 'with the catalog back, the table reads');
}

# ------------------------------------------------------------------------
# Part 4: the maintenance flag after subtransactions and a compaction in one
# transaction: three user rows in two subtransaction levels around a
# compaction of three small stripes
# ------------------------------------------------------------------------

sub flags_of
{
	my ($t) = @_;
	return $node->safe_psql('postgres', "
		SELECT coalesce(string_agg(maintenance || ':' || n, ' ' ORDER BY maintenance DESC), '')
		  FROM (SELECT maintenance, sum(row_count) n FROM kingres.columnar_stripe
		         WHERE rel_uid = kingres.columnar_identity('$t') GROUP BY maintenance) s");
}

foreach my $variant ('plain', 'with a cursor open on the table')
{
	$node->safe_psql('postgres', "
		DROP TABLE IF EXISTS fl; CREATE TABLE fl (a int) USING columnar;
		INSERT INTO fl VALUES (1); INSERT INTO fl VALUES (2); INSERT INTO fl VALUES (3);");
	my $cursor = $variant =~ /cursor/ ? 'DECLARE cc CURSOR FOR SELECT a FROM fl; FETCH 1 FROM cc;' : '';
	my ($out, $err) = script("
BEGIN;
INSERT INTO fl VALUES (100);
$cursor
SAVEPOINT s; INSERT INTO fl VALUES (101);
SELECT kingres.columnar_compact('fl', 100000);
RELEASE s;
SAVEPOINT t; INSERT INTO fl VALUES (102); RELEASE t;
COMMIT;");
	unlike($err, qr/ERROR/, "$variant: no error");
	is(flags_of('fl'), 'true:3 false:3', "$variant: the copies of compaction are maintenance stripes, the three user rows are not");
	is($node->safe_psql('postgres', 'SELECT count(*) || \'/\' || sum(a) FROM fl'), '6/309', "$variant: every row once");
}

$node->stop;
done_testing();
