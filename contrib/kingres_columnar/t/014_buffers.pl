# contrib/kingres_columnar/t/014_buffers.pl
#
# The write buffers of a transaction: the hold of a scan on them (C3-02) and
# the size of a stripe that merged buffers become (C3-03).
#
# Part 1.  A scan of a table that has buffered rows holds the buffers still
# (no flush) while it reads them.  When a query is abandoned by an error that
# a subtransaction catches, the scan never ends, and the hold must be given
# back anyway or no stripe is written by size for the rest of the transaction
# and memory grows without a bound.  Every way a scan can end is exercised:
# an exception block, ROLLBACK TO, a cursor whose FETCH fails, a cursor
# opened inside the subtransaction that fails, an error in the Columnar Scan
# node and in the plain access method scan, and a healthy cursor that must
# keep its hold across a caught error of another statement.
#
# Part 2.  Buffers of committed subtransactions are merged into their
# parent's, and the merged buffer can hold more rows than a stripe may.  The
# writer must cut it into stripes of at most the table's stripe size, and
# never write a stripe the reader refuses.  The limit is reached with small
# numbers: the stripe size option of the table, and the developer setting
# kingres_columnar.max_stripe_rows that lowers the compiled limit.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colbuf');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

# Run a script in one session, errors do not stop it; returns stdout.
sub script
{
	my ($sql) = @_;
	my ($out, $err) = ('', '');

	$node->psql('postgres', $sql, stdout => \$out, stderr => \$err,
		on_error_stop => 0);
	return ($out, $err);
}

sub fresh_table
{
	my ($rows) = @_;

	$node->safe_psql('postgres', "
		DROP TABLE IF EXISTS pn;
		CREATE TABLE pn (a int) USING columnar;
		SELECT kingres.columnar_set_options('pn', stripe_row_count => $rows,
											chunk_row_count => 100);");
}

# ------------------------------------------------------------------------
# Part 1: holds
# ------------------------------------------------------------------------

# 10 rows are buffered, a failing scan is caught, then 5000 rows are
# inserted: with stripes of 1000 rows five stripes must exist before COMMIT.
my $LOAD = q{INSERT INTO pn SELECT generate_series(1, 5000);
SELECT 'STRIPES=' || count(*) FROM kingres.columnar_stripes('pn');};

my %cases = (
	'ROLLBACK TO after an error' => q{
SAVEPOINT s; SELECT 1/(a-a) FROM pn; ROLLBACK TO s;},
	'exception block' => q{
DO $$ BEGIN BEGIN PERFORM 1/(a-a) FROM pn; EXCEPTION WHEN division_by_zero THEN NULL; END; END $$;},
	'exception block in a loop' => q{
DO $$ BEGIN FOR i IN 1..20 LOOP BEGIN PERFORM 1/(a-a) FROM pn;
  EXCEPTION WHEN division_by_zero THEN NULL; END; END LOOP; END $$;},
	'error in a qual' => q{
SAVEPOINT s; SELECT * FROM pn WHERE 1/(a-a) = 1; ROLLBACK TO s;},
	'cursor whose FETCH fails' => q{
DECLARE cur CURSOR FOR SELECT 1/(a-a) FROM pn;
SAVEPOINT s; FETCH 1 FROM cur; ROLLBACK TO s; CLOSE cur;},
	'failed cursor left open' => q{
DECLARE cur CURSOR FOR SELECT 1/(a-a) FROM pn;
SAVEPOINT s; FETCH 1 FROM cur; ROLLBACK TO s;},
	'cursor opened inside the failed subtransaction' => q{
SAVEPOINT s; DECLARE cur CURSOR FOR SELECT a FROM pn;
SELECT 1/(a-a) FROM pn; ROLLBACK TO s;},
	'nested savepoints' => q{
SAVEPOINT s1; SAVEPOINT s2; SELECT 1/(a-a) FROM pn; ROLLBACK TO s1; RELEASE s1;},
);

foreach my $custom (qw(on off))
{
	foreach my $name (sort keys %cases)
	{
		fresh_table(1000);
		my ($out) = script("
SET kingres_columnar.enable_custom_scan = $custom;
BEGIN;
INSERT INTO pn SELECT generate_series(1, 10);
$cases{$name}
$LOAD
COMMIT;");
		like($out, qr/STRIPES=5\b/,
			"$name (custom scan $custom): the load after the caught error flushes by size");
		is($node->safe_psql('postgres', 'SELECT count(*) FROM pn'), 5010,
			"$name (custom scan $custom): no row lost");
	}
}

# the error really is raised inside the Columnar Scan node in the custom
# scan cases, so that the case proves what it says
{
	fresh_table(1000);
	my ($out) = script("
SET kingres_columnar.enable_custom_scan = on;
EXPLAIN (COSTS OFF) SELECT 1/(a-a) FROM pn;");
	like($out, qr/Columnar Scan/, 'the failing query is planned with the Columnar Scan');
	($out) = script("
SET kingres_columnar.enable_custom_scan = off;
EXPLAIN (COSTS OFF) SELECT 1/(a-a) FROM pn;");
	unlike($out, qr/Columnar Scan/, 'and with a plain scan when the custom scan is off');
}

# A healthy cursor keeps its hold across a caught error of another statement:
# its rows are not flushed from under it, none is lost or repeated.
{
	fresh_table(1000);
	my ($out) = script("
BEGIN;
INSERT INTO pn SELECT generate_series(1, 10);
DECLARE cur CURSOR FOR SELECT a FROM pn ORDER BY a;
FETCH 4 FROM cur;
SAVEPOINT s; SELECT 1/(a-a) FROM pn; ROLLBACK TO s; RELEASE s;
INSERT INTO pn SELECT generate_series(11, 3000);
SELECT 'OPEN_STRIPES=' || count(*) FROM kingres.columnar_stripes('pn');
FETCH ALL FROM cur;
CLOSE cur;
INSERT INTO pn SELECT generate_series(3001, 5000);
SELECT 'CLOSED_STRIPES=' || count(*) FROM kingres.columnar_stripes('pn');
COMMIT;");
	like($out, qr/OPEN_STRIPES=0\b/, 'a healthy cursor holds the buffers across the caught error');
	like($out, qr/CLOSED_STRIPES=5\b/, 'and gives them back when it is closed');
	is($node->safe_psql('postgres', 'SELECT count(*), sum(a) FROM pn'),
		'5000|12502500', 'every row is there once');
}

# The leak of the hold made the transaction's memory grow without a bound.
{
	$node->safe_psql('postgres', "
		DROP TABLE IF EXISTS pn;
		CREATE TABLE pn (a int, t text) USING columnar;
		SELECT kingres.columnar_set_options('pn', stripe_row_count => 10000,
											chunk_row_count => 1000);");
	my ($out) = script("
SET kingres_columnar.write_buffer_memory = '16MB';
BEGIN;
INSERT INTO pn SELECT 1, 'x';
SAVEPOINT s; SELECT 1/(a-a) FROM pn; ROLLBACK TO s; RELEASE s;
INSERT INTO pn SELECT g, repeat('y', 100) FROM generate_series(1, 1500000) g;
SELECT 'STRIPES=' || count(*) FROM kingres.columnar_stripes('pn');
SELECT 'CTXBYTES=' || sum(total_bytes) FROM pg_backend_memory_contexts;
COMMIT;");
	my ($nstripes) = $out =~ /STRIPES=(\d+)/;
	cmp_ok($nstripes, '>=', 100, 'the load was written out in stripes as it went');
	my ($bytes) = $out =~ /CTXBYTES=(\d+)/;
	cmp_ok($bytes, '<', 64 * 1024 * 1024,
		"the backend's memory stays bounded after a caught error ($bytes bytes)");
}

# No resource of ours was left behind when a transaction ended.
{
	my $log = slurp_file($node->logfile);
	unlike($log, qr/resource was not closed/, 'no leaked hold reported at the end of a transaction');
}

# ------------------------------------------------------------------------
# Part 2: merged buffers and the stripe limit
# ------------------------------------------------------------------------

sub stripe_summary
{
	my ($rel) = @_;
	return $node->safe_psql('postgres',
		"SELECT count(*) || '|' || coalesce(max(row_count), 0) || '|' || coalesce(sum(row_count), 0)
		   FROM kingres.columnar_stripes('$rel')");
}

# 30 subtransactions of 500 rows are merged into one parent buffer, far more
# than a stripe of 1000 rows: they must come out as stripes of at most 1000.
{
	fresh_table(1000);
	my ($out) = script("
BEGIN;
INSERT INTO pn VALUES (0);
DO \$\$ BEGIN FOR i IN 1..30 LOOP BEGIN
  INSERT INTO pn SELECT g FROM generate_series(1, 500) g;
EXCEPTION WHEN division_by_zero THEN NULL; END; END LOOP; END \$\$;
SELECT 'BEFORE=' || count(*) FROM kingres.columnar_stripes('pn');
INSERT INTO pn VALUES (1);
SELECT 'MID=' || count(*) || '|' || max(row_count) FROM kingres.columnar_stripes('pn');
COMMIT;");
	like($out, qr/BEFORE=0\b/, 'the merged rows are still buffered (nothing flushed by the merge)');
	like($out, qr/MID=15\|1000\b/, 'the next write cuts them into full stripes of 1000 rows and keeps the tail');
	is(stripe_summary('pn'), '16|1000|15002', 'at commit: 16 stripes, none over 1000 rows, no row lost');
	is($node->safe_psql('postgres', 'SELECT count(*), sum(a) FROM pn'), '15002|3757501',
		'every row reads back');
}

# The same, with the memory flush and a merged buffer that is the only one.
{
	fresh_table(1000);
	my ($out) = script("
BEGIN;
DO \$\$ BEGIN FOR i IN 1..8 LOOP BEGIN
  INSERT INTO pn SELECT g FROM generate_series(1, 700) g;
EXCEPTION WHEN division_by_zero THEN NULL; END; END LOOP; END \$\$;
COMMIT;");
	is(stripe_summary('pn'), '6|1000|5600', 'commit of a merged buffer of 5600 rows: stripes of at most 1000');
}

# A subtransaction that is rolled back after its parent merged an earlier one
# leaves nothing of itself in the parent.
{
	fresh_table(1000);
	my ($out) = script("
BEGIN;
INSERT INTO pn VALUES (0);
SAVEPOINT a; INSERT INTO pn SELECT g FROM generate_series(1, 2500) g; RELEASE a;
SAVEPOINT b; INSERT INTO pn SELECT g FROM generate_series(1, 4000) g; ROLLBACK TO b;
COMMIT;");
	is(stripe_summary('pn'), '3|1000|2501', 'a rolled back subtransaction adds nothing');
}

# The limit and the reader.  The developer setting lowers the writer's limit
# and the size above which columnar_check reports a stripe; stripes of
# 5000 rows are written, then the limit goes down to 1000.
{
	fresh_table(5000);
	$node->safe_psql('postgres', 'INSERT INTO pn SELECT generate_series(1, 20000)');
	is(stripe_summary('pn'), '4|5000|20000', 'four stripes of 5000 rows');

	my $over = $node->safe_psql('postgres', "
		SET kingres_columnar.max_stripe_rows = 1000;
		SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'stripe_over_row_limit'");
	is($over, 4, 'columnar_check reports the stripes above the limit');
	is($node->safe_psql('postgres', "
		SET kingres_columnar.max_stripe_rows = 1000;
		SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'stripe_over_row_limit' AND detail LIKE '%VACUUM FULL%'"),
		4, 'and says how to write them again');
	is($node->safe_psql('postgres', 'SELECT count(*), sum(a) FROM pn'), '20000|200010000',
		'such a stripe is still readable');

	# the writer keeps to the lowered limit, also for buffers that grew
	$node->safe_psql('postgres', "
		SET kingres_columnar.max_stripe_rows = 1000;
		VACUUM FULL pn");
	is(stripe_summary('pn'), '20|1000|20000', 'VACUUM FULL writes stripes within the limit');
	is($node->safe_psql('postgres', "
		SET kingres_columnar.max_stripe_rows = 1000;
		SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'stripe_over_row_limit'"),
		0, 'columnar_check has nothing to report afterwards');
	is($node->safe_psql('postgres', 'SELECT count(*), sum(a) FROM pn'), '20000|200010000',
		'rows unchanged');

	# a load into a table whose stripe size is above the limit is cut at the limit
	$node->safe_psql('postgres', "
		SET kingres_columnar.max_stripe_rows = 2000;
		INSERT INTO pn SELECT g FROM generate_series(1, 9000) g");
	is($node->safe_psql('postgres', "SELECT max(row_count) FROM kingres.columnar_stripes('pn') WHERE stripe_id > 20"),
		2000, 'a table with a larger stripe size writes stripes at the lowered limit');

	# the setting is for the superuser and cannot make a table unreadable
	$node->safe_psql('postgres', 'CREATE ROLE plain_user LOGIN');
	my ($rc, $o, $e) = $node->psql('postgres',
		'SET ROLE plain_user; SET kingres_columnar.max_stripe_rows = 1000',
		on_error_stop => 1, stdout => \my $so, stderr => \my $se);
	like($se, qr/permission denied/, 'an ordinary user cannot set the limit');
	($rc, $o, $e) = $node->psql('postgres',
		'SET kingres_columnar.max_stripe_rows = 20000000',
		on_error_stop => 1, stdout => \$so, stderr => \$se);
	like($se, qr/is outside the valid range/, 'the limit cannot be raised above the compiled one');
}

$node->stop;
done_testing();
