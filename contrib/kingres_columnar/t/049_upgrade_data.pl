# contrib/kingres_columnar/t/049_upgrade_data.pl
#
# C6a, decision 5: a data directory written by the code of 0.1 is carried across the
# update to 0.2.  The 0.1 code is the commit bfda9e8842 of the repository, built into a
# private installation that the test is pointed at with KC_OLD_INSTALL (the path of the
# installation; without it the file is skipped, because building an older tree is not
# something a test run can do).  The same server version runs both: the data
# directory is initialised and written by the installation of 0.1, the server is
# stopped, and the installation under test starts on the same data directory:
#
#   1. with the new library and the SQL objects of 0.1 (before ALTER EXTENSION
#      UPDATE: the catalog still has 14 columns): the tables read and are written,
#      a transaction prepared by the old code is committed by the new one;
#   2. after ALTER EXTENSION kingres_columnar UPDATE: the same rows with the same
#      TIDs, the tables written and vacuumed and compacted; the files of 0.1 are
#      format version 2 and never reserve whatever the setting says; a table that is
#      rewritten (VACUUM FULL) becomes version 3 and reserves; a new table reserves;
#      columnar_check is clean;
#   3. the 0.1 library cannot read a file of version 3 (it refuses it by its format
#      version), which is the point of the number.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(q1 rsql reserve_on);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $old = $ENV{KC_OLD_INSTALL};
plan skip_all => 'KC_OLD_INSTALL (an installation of the 0.1 code, commit bfda9e8842) is not set'
  unless defined $old && -x "$old/bin/pg_ctl";

my $node = PostgreSQL::Test::Cluster->new('upgd', install_path => $old);
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_columnar'\nmax_prepared_transactions = 10\nautovacuum = off\n");
$node->start;
is(q1($node, "SHOW server_version_num") =~ /^\d+$/ ? 1 : 0, 1, 'the installation of 0.1 runs');
$node->safe_psql('postgres', "CREATE EXTENSION kingres_columnar");
is(q1($node, "SELECT extversion FROM pg_extension WHERE extname = 'kingres_columnar'"), '0.1', 'the old installation makes the extension at 0.1');

# --- written by the code of 0.1 -------------------------------------------------
q1($node, q{
	CREATE TABLE t1 (a int, b text) USING columnar;
	SELECT kingres.columnar_set_options('t1', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE t2 (a int, b text) USING columnar;
	INSERT INTO t1 SELECT g, 'old' || g FROM generate_series(1, 2500) g;
	BEGIN; INSERT INTO t1 SELECT g, 'sp' || g FROM generate_series(2501, 2520) g;
	  SAVEPOINT s; INSERT INTO t1 SELECT g, 'sp' || g FROM generate_series(2521, 2530) g; RELEASE s; COMMIT;
	INSERT INTO t2 SELECT g, repeat('w', 200) FROM generate_series(1, 3000) g;
	BEGIN; INSERT INTO t1 SELECT g, 'prepared' || g FROM generate_series(3001, 3040) g; PREPARE TRANSACTION 'by_old';
});
my $digest = "SELECT count(*) || '/' || sum(a) || '/' || md5(string_agg(a::text || ':' || b || ':' || ctid::text, ',' ORDER BY a)) FROM ";
my $t1_before = q1($node, $digest . "t1");
my $t2_before = q1($node, $digest . "t2");
is(q1($node, "SELECT count(*) FROM t1"), 2530, 'written by 0.1: 2530 rows are committed (the prepared transaction is not)');
my $stripes_before = q1($node, "SELECT string_agg(first_row || ':' || row_count, ' ' ORDER BY stripe_id) FROM kingres.columnar_stripes('t1')");
$node->stop;

# --- the new code, the SQL objects of 0.1 ----------------------------------------
delete $node->{_install_path};
$node->start;
is(q1($node, "SELECT extversion FROM pg_extension WHERE extname = 'kingres_columnar'"), '0.1', 'the new library starts on the data directory, the extension is still 0.1');
is(q1($node, "SELECT count(*) FROM pg_attribute WHERE attrelid = 'kingres.columnar_stripe'::regclass AND attnum > 0 AND NOT attisdropped"), 14, '... its catalog has the 14 columns of 0.1');
is(q1($node, $digest . "t1"), $t1_before, 'the new library reads t1 as 0.1 wrote it, with the same TIDs');
is(q1($node, $digest . "t2"), $t2_before, '... and t2');
is(q1($node, "SELECT count(*) FROM pg_prepared_xacts"), 1, 'the transaction prepared by 0.1 is there');
q1($node, "COMMIT PREPARED 'by_old'");
is(q1($node, "SELECT count(*) FROM t1"), 2570, '... and the new code commits it');
q1($node, "INSERT INTO t1 SELECT g, 'new' || g FROM generate_series(4001, 4100) g");
is(q1($node, "SELECT count(*) FROM t1"), 2670, 'the new code writes into the table of 0.1 (catalog of 0.1)');
{
	my ($rc, $out, $err) = rsql($node, "BEGIN; INSERT INTO t1 VALUES (5000, 'setting on, catalog of 0.1'); COMMIT;");
	is($rc, 0, 'with the setting on and the catalog of 0.1 nothing is reserved, and nothing fails') or diag($err);
}
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, 'columnar_check is clean before the update');
my $t1_mid = q1($node, $digest . "t1");

# --- the update ------------------------------------------------------------------
q1($node, "ALTER EXTENSION kingres_columnar UPDATE");
is(q1($node, "SELECT extversion FROM pg_extension WHERE extname = 'kingres_columnar'"), '0.2', 'ALTER EXTENSION UPDATE: version 0.2');
is(q1($node, "SELECT count(*) FROM pg_attribute WHERE attrelid = 'kingres.columnar_stripe'::regclass AND attnum > 0 AND NOT attisdropped"), 15, '... the catalog has the new column');
is(q1($node, $digest . "t1"), $t1_mid, 'after the update t1 has the same rows with the same TIDs');
is(q1($node, $digest . "t2"), $t2_before, '... and t2');
is(q1($node, "SELECT string_agg(first_row || ':' || row_count, ' ' ORDER BY stripe_id) FROM kingres.columnar_stripes('t1')") =~ /^\Q$stripes_before\E / ? 1 : 0, 1, '... and the stripes of 0.1 are still the first stripes of t1');

# a file of 0.1 never reserves
{
	my ($rc, $out, $err) = rsql($node, "BEGIN; INSERT INTO t1 SELECT g, 'x' FROM generate_series(6000, 6009) g; SELECT count(*) FILTER (WHERE ctid::text LIKE '(4294967293,%') FROM t1 WHERE a >= 6000; SELECT count(*) FROM kingres.columnar_stripe WHERE reserved; COMMIT;");
	is($rc, 0, 'the setting on, a table of 0.1') or diag($err);
	is($out, "10\n0", '... its rows have the meaningless TID of a buffered row (nothing is reserved)');
}
q1($node, "INSERT INTO t2 SELECT g, 'more' FROM generate_series(3001, 3100) g");
is(q1($node, "SELECT count(*) FROM t2"), 3100, 'the table of 0.1 is written after the update');
q1($node, "VACUUM t1; VACUUM t2");
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, 'VACUUM, and columnar_check is clean');
is(q1($node, "SELECT kingres.columnar_compact('t2', 1000000) >= 0"), 't', 'a compaction of a table of 0.1');
is(q1($node, "SELECT count(*) FROM t2"), 3100, '... changes no row');

# a new table reserves; a rewritten one becomes version 3 and reserves
{
	my ($rc, $out, $err) = rsql($node, "CREATE TABLE t3 (a int) USING columnar; BEGIN; INSERT INTO t3 SELECT generate_series(1, 5); SELECT count(*) FROM kingres.columnar_stripe WHERE reserved; SELECT min(ctid), max(ctid) FROM t3; COMMIT;");
	is($rc, 0, 'a table made after the update') or diag($err);
	is($out, "1\n(0,1)|(0,5)", '... reserves: TIDs from the number 0');
	q1($node, "VACUUM FULL t1");
	($rc, $out, $err) = rsql($node, "BEGIN; INSERT INTO t1 VALUES (7000, 'after the rewrite'); SELECT count(*) FROM kingres.columnar_stripe WHERE reserved; SELECT count(*) FILTER (WHERE ctid::text LIKE '(4294967293,%') FROM t1 WHERE a = 7000; COMMIT;");
	is($rc, 0, 'a table of 0.1 that VACUUM FULL has rewritten') or diag($err);
	is($out, "1\n0", '... reserves now (its file is version 3)');
}
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, 'columnar_check is clean at the end');
$node->stop;

# --- the old library and a file of version 3 --------------------------------------
$node->{_install_path} = $old;
$node->start;
my ($rc, $out, $err) = $node->psql('postgres', "SELECT count(*) FROM t3");
like($err, qr/format version 3/, 'the library of 0.1 refuses a file of version 3 by its format version');
($rc, $out, $err) = $node->psql('postgres', "SELECT count(*) FROM t2");
like($err, qr/the stripe catalog has 15 columns/, '... and the catalog of 0.2 as well: there is no way back to 0.1 once the extension is updated');
$node->stop;

done_testing();
