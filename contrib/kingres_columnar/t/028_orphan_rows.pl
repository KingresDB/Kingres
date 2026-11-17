# contrib/kingres_columnar/t/028_orphan_rows.pl
#
# C7-05.  Stripe rows left behind by a file that went away where nothing could
# delete them: a leak, never an emptied table, and a way to find and remove them
# that is safe against everything that is going on.
#
# Two ways to leave rows (both with event triggers off, so that no trigger does the
# work at the end of the command):
#   1. a callback of another module (loaded before the access method, so that its
#      pre-commit runs after the access method's) drops the table: the access
#      method's own cleanup has run and the transaction is about to commit, when
#      nothing can delete a row.  The access method keeps the file and the next
#      transaction of the same backend that reaches its pre-commit deletes the rows
#      (the same comparison as the transaction's own cleanup, so a file that is in
#      use keeps its rows); a backend that ends first leaves them.
#   2. a session that did not load the library (the server runs without
#      shared_preload_libraries, the extension still installed) drops a columnar
#      table: no hook runs there at all.
# What remains is found by kingres.columnar_check() (check_name
# orphan_catalog_rows, with the count) and removed by columnar_check(true), which
# reads the identities of the live files with a dirty snapshot of pg_class, so the
# file of a table that another session is creating, uncommitted, keeps its rows.
#
# Needs a compiler and the server's headers for the probe module; skipped with the
# reason where it cannot be built.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcProbe qw(build_module);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $dir = build_module('kcprobe');
plan skip_all => 'the probe module (t/kcprobe/kcprobe.c) could not be built here: it needs a C compiler and the server headers'
  unless defined $dir;

my $node = PostgreSQL::Test::Cluster->new('colorphan');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = '$dir/kcprobe,kingres_columnar'\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub q1 { return $node->safe_psql('postgres', $_[0]); }

sub script
{
	my ($sql) = @_;
	my ($out, $err) = ('', '');

	$node->psql('postgres', $sql, stdout => \$out, stderr => \$err, on_error_stop => 0);
	return ($out, $err);
}

sub orphans
{
	return q1("SELECT coalesce(sum((regexp_match(detail, '^(\\d+) '))[1]::int), 0) FROM kingres.columnar_check() WHERE check_name = 'orphan_catalog_rows'");
}

sub fresh_x
{
	q1("DROP TABLE IF EXISTS x CASCADE;
		CREATE TABLE x (a int, b int) USING columnar;
		SELECT kingres.columnar_set_options('x', stripe_row_count => 1000, chunk_row_count => 100);
		INSERT INTO x SELECT g, g FROM generate_series(1, 5000) g;");
	q1("SELECT kingres.columnar_reconcile()");
}

my $ORPH = "SELECT 'ORPH=' || coalesce(sum((regexp_match(detail, '^(\\d+) '))[1]::int), 0) FROM kingres.columnar_check() WHERE check_name = 'orphan_catalog_rows';";
my $CLEANED = "SELECT 'CLEANED=' || hits FROM kingres.columnar_write_paths() WHERE path = 'orphans_cleaned';";

# ------------------------------------------------------------------------
# 1. A table dropped by another module's pre-commit callback, in the same session
#    and in a session that ends
# ------------------------------------------------------------------------
foreach my $stmt (['DROP TABLE x', 'DROP TABLE'], ['ALTER TABLE x ALTER COLUMN b TYPE bigint', 'a rewriting ALTER TABLE'],
	['ALTER TABLE x SET ACCESS METHOD heap', 'SET ACCESS METHOD heap'])
{
	my ($sql, $name) = @$stmt;
	fresh_x();
	is(orphans(), 0, "$name: nothing left over before");
	my ($out, $err) = script("SET event_triggers = off;
BEGIN; SET LOCAL kcprobe.commit_sql = '$sql'; SELECT 1; COMMIT;
$ORPH
SELECT 1;
$ORPH
SELECT 1;
$CLEANED");
	unlike($err, qr/ERROR/, "$name at the pre-commit of another module, event triggers off: no error");
	like($out, qr/ORPH=5\b.*ORPH=0\b/s, "$name: the five stripe rows of the old file are left by the commit and removed by the next transaction of the session");
	like($out, qr/CLEANED=5\b/, "... counted: five rows");
	is(orphans(), 0, "$name: columnar_check has nothing to report afterwards");

	# the session ends before another transaction: the rows stay, are reported and removed
	fresh_x();
	script("SET event_triggers = off; BEGIN; SET LOCAL kcprobe.commit_sql = '$sql'; SELECT 1; COMMIT;");
	is(orphans(), 5, "$name: a session that ends right after the commit leaves the five rows, and columnar_check reports them");
	my ($rows) = q1("SELECT count(*) FROM kingres.columnar_check(true) WHERE check_name = 'orphan_catalog_rows'");
	is($rows, 1, "$name: columnar_check(true) reports them once, with their count");
	is(orphans(), 0, "$name: ... and has removed them");
}

# a table that is not dropped after all: the commit deletes nothing (the rollback of a
# statement in a block, and a drop that fails)
{
	fresh_x();
	script("SET event_triggers = off;
BEGIN; SET LOCAL kcprobe.commit_sql = 'SELECT 1'; DO \$\$ BEGIN BEGIN DROP TABLE x; RAISE EXCEPTION 'no'; EXCEPTION WHEN raise_exception THEN NULL; END; END \$\$; COMMIT; SELECT 1; SELECT 1;");
	is(q1('SELECT count(*) FROM x'), 5000, 'a drop rolled back in a block, then commits and later transactions: the table keeps its rows');
	is(orphans(), 0, '... and nothing is reported');
}

# ------------------------------------------------------------------------
# 2. columnar_check(true) against a table that another session is creating
# ------------------------------------------------------------------------
{
	fresh_x();
	my $b = $node->background_psql('postgres', on_error_stop => 1);
	$b->query_safe("BEGIN");
	$b->query_safe("CREATE TABLE y (a int) USING columnar");
	$b->query_safe("COPY y FROM PROGRAM \$\$seq 1 3000\$\$");
	# the COPY ended with the flush of the table (it was made in this transaction): its
	# stripe rows are in the catalog, uncommitted
	q1("SELECT * FROM kingres.columnar_check(true)");
	q1("SELECT kingres.columnar_reconcile()");
	$b->query_safe("COMMIT");
	is(q1("SELECT count(*) FROM y"), 3000, 'columnar_check(true) and columnar_reconcile() run while a table is created: its rows are all there when it commits');
	cmp_ok(q1("SELECT count(*) FROM kingres.columnar_stripes('y')"), '>=', 1, '... in stripes that the check did not take away');
	is(orphans(), 0, '... and nothing is reported');
	$b->quit;
	q1("DROP TABLE y");
}

# ------------------------------------------------------------------------
# 3. A session without the library drops a table
# ------------------------------------------------------------------------
{
	fresh_x();
	$node->stop;
	$node->append_conf('postgresql.conf', "shared_preload_libraries = ''\n");
	$node->start;
	my ($out, $err) = script("SET event_triggers = off; DROP TABLE x;");
	unlike($err, qr/ERROR/, 'DROP TABLE of a columnar table in a server without the library: works');
	$node->stop;
	$node->append_conf('postgresql.conf', "shared_preload_libraries = '$dir/kcprobe,kingres_columnar'\n");
	$node->start;
	is(orphans(), 5, 'the five stripe rows of the dropped table are left, and columnar_check reports them');
	q1("SELECT * FROM kingres.columnar_check(true)");
	is(orphans(), 0, '... columnar_check(true) removes them');
	q1("CREATE TABLE z (a int) USING columnar; INSERT INTO z SELECT generate_series(1, 2500)");
	is(q1('SELECT count(*) FROM z'), 2500, '... and a table made afterwards is not touched by any of it');
}

$node->stop;
done_testing();
