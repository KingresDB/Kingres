# contrib/kingres_columnar/t/039_alias_am.pl
#
# C11-02.  A second access method made over the extension's handler
# (CREATE ACCESS METHOD columnar2 TYPE TABLE HANDLER kingres.columnar_handler) is a columnar
# access method for every read and write.  The extension used to recognise its relations by
# the NAME of the access method in some places (the list of tables of the check, the list of
# live files of the reconciliation, the drop note, the planner's path, the event trigger that
# refuses foreign keys), so that a table of the second access method was not looked at by the
# check, and the reconciliation, which saw no live file for it, deleted every stripe row of it
# when anything was vacuumed (silent loss of all the rows).
#
# One definition now: an access method is the extension's when its handler is (kc_am_class).
#
#   1. a table of the second access method is listed by the check: a damaged one is reported by
#      name, and a healthy one makes columnar_check_clean() true;
#   2. VACUUM of another table, columnar_reconcile() and columnar_check(true) delete no row of it
#      (the rows compared before and after, and the stripes);
#   3. the planner uses the Columnar Scan for it, and a foreign key on it is refused as for
#      "columnar" (the event trigger tests the handler);
#   4. a rewrite of such a table (to heap and back, VACUUM FULL) keeps the rows and the stripe
#      rows of the old file go away;
#   5. a relation whose access method cannot be classified makes the cleanup do nothing, and
#      says so: an access method that another session created and has not committed, with a
#      table of it in the same transaction (the dirty scan of pg_class sees the table, the
#      catalog does not see the access method).  C12-01: that is a visible outcome: the check
#      returns a not_checked row with the reason and columnar_check_clean() is false (it was
#      true, with an orphan row present), columnar_reconcile() answers NULL with a WARNING
#      (not 0), and VACUUM stays quiet; the same for a columnar file whose metapage is damaged.
#   6. C12-03: an access method over a second SQL function with the extension's C entry point
#      is columnar for the DDL guard too (foreign keys, AFTER ROW triggers, publications).
#   7. C12-05 (a mutant that never cleared the classification cache survived): a second session
#      that has classified an access method sees a handler that was replaced.
#   8. C12-04: a table of an access method whose handler returns a copy of the routine refuses
#      writes (it used to accept them and drop the rows at commit).
#   9. C12-06: the handler of another access method raises an error: the check and the
#      reconciliation stand down and say so, they do not fail.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

use FindBin;
use lib $FindBin::RealBin;
use KcProbe qw(build_module);

my $pdir = build_module('kcprobe');

my $BLCKSZ = 8192;

my $node = PostgreSQL::Test::Cluster->new('colalias');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_columnar'\nautovacuum = off\nlog_min_messages = log\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub q1
{
	my ($sql) = @_;
	my $r = eval { $node->safe_psql('postgres', $sql) };
	die "t/039: q1 failed running [$sql]: $@" if $@;
	return $r;
}

sub bgq
{
	my ($s, $sql) = @_;
	my $r = eval { $s->query_safe($sql) };
	die "t/039: a background session failed running [$sql]: $@" if $@;
	$r =~ s/\s+//g;
	return $r;
}

sub fp
{
	my ($t) = @_;
	return q1("SELECT count(*) || '|' || coalesce(sum(a), 0) || '|' || coalesce(md5(string_agg(a::text || b, ',' ORDER BY a)), '') FROM $t");
}

sub stripes
{
	my ($t) = @_;
	return q1("SELECT count(*) FROM kingres.columnar_stripes('$t')");
}

q1("CREATE ACCESS METHOD columnar2 TYPE TABLE HANDLER kingres.columnar_handler");
q1("CREATE TABLE a2 (a int, b text) USING columnar2;
	SELECT kingres.columnar_set_options('a2', stripe_row_count => 5000, chunk_row_count => 500);
	INSERT INTO a2 SELECT g, repeat('x', 20) FROM generate_series(1, 60000) g;
	CREATE TABLE a1 (a int, b text) USING columnar;
	INSERT INTO a1 SELECT g, 'y' FROM generate_series(1, 1000) g;");

# ------------------------------------------------------------------------
# 1. the check looks at the table of the second access method
# ------------------------------------------------------------------------
is(q1('SELECT kingres.columnar_check_clean()'), 't', 'a healthy database with a table of the second access method: clean');
my $pre = fp('a2');
my $pre_stripes = stripes('a2');
cmp_ok($pre_stripes, '>', 3, 'the table has several stripes');

# ------------------------------------------------------------------------
# 2. a routine vacuum of another table, the reconciliation and the repairing check delete no row of it
# ------------------------------------------------------------------------
q1('VACUUM a1');
is(fp('a2'), $pre, 'VACUUM of another table: the rows of the table of the second access method are all there');
is(stripes('a2'), $pre_stripes, '... and so are its stripes');
q1('VACUUM a2');
is(fp('a2'), $pre, 'VACUUM of the table itself: the rows are all there');
is(q1('SELECT kingres.columnar_reconcile()'), '0', 'columnar_reconcile() finds nothing to delete');
is(q1("SELECT count(*) FROM kingres.columnar_check(true)"), '0', 'columnar_check(true) reports nothing for a healthy database');
is(fp('a2'), $pre, 'the rows are all there after the reconciliation and the repairing check');
is(stripes('a2'), $pre_stripes, '... and the stripes');

# a table made after, and vacuumed by the vacuum of its own, as the review's steps did
q1("CREATE TABLE a3 (a int, b text) USING columnar2; INSERT INTO a3 SELECT g, 'y' FROM generate_series(1, 50000) g;");
my $pre3 = fp('a3');
q1('VACUUM a1');
is(fp('a3'), $pre3, 'a table made later: VACUUM of another table loses nothing');
q1('VACUUM a3');
is(fp('a3'), $pre3, '... nor does its own VACUUM');
q1('DROP TABLE a3');

# a damaged table of the second access method is named by the check
{
	q1('CHECKPOINT');
	my $path = $node->data_dir . '/' . q1("SELECT pg_relation_filepath('a2')");
	truncate($path, 2 * $BLCKSZ) or die "truncate $path: $!";
	is(q1('SELECT kingres.columnar_check_clean()'), 'f', 'a damaged table of the second access method: not clean');
	is(q1("SELECT count(*) > 0 FROM kingres.columnar_check() WHERE check_name = 'stripe_beyond_file' AND relation = 'a2'::regclass"), 't',
		'... and the check names the table (it used to name no table at all)');
	is(q1("SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'orphan_catalog_rows'"), '0', '... and calls none of its rows orphans');
	q1("SELECT * FROM kingres.columnar_check(true)");
	is(q1('SELECT kingres.columnar_check_clean()'), 't', '... the repairing check removes the stripes that cannot be read, and the table is clean');
	q1('DROP TABLE a2');
}

# ------------------------------------------------------------------------
# 3. the planner's path, and the event trigger's refusals
# ------------------------------------------------------------------------
{
	q1("CREATE TABLE p2 (a int PRIMARY KEY, b text) USING heap; CREATE TABLE c2 (a int, b text) USING columnar2;
		INSERT INTO c2 SELECT g, 'z' FROM generate_series(1, 20000) g;
		INSERT INTO p2 SELECT g, 'p' FROM generate_series(1, 20000) g;");
	like(q1("EXPLAIN (COSTS OFF) SELECT a FROM c2 WHERE a > 19990"), qr/Columnar Scan/, 'the planner uses the Columnar Scan for a table of the second access method');
	my ($ret, $out, $err) = $node->psql('postgres', "ALTER TABLE c2 ADD CONSTRAINT c2_fk FOREIGN KEY (a) REFERENCES p2 (a)");
	like($err, qr/foreign keys on or to columnar tables are not supported/, 'a foreign key on a table of the second access method is refused, as for "columnar"');
	($ret, $out, $err) = $node->psql('postgres', "CREATE PUBLICATION pub2 FOR TABLE c2");
	like($err, qr/columnar tables are not replicated logically yet/, 'so is a publication of it');
	q1('DROP TABLE c2, p2');
}

# ------------------------------------------------------------------------
# 4. rewrites of such a table
# ------------------------------------------------------------------------
{
	q1("CREATE TABLE r2 (a int, b text) USING columnar2; INSERT INTO r2 SELECT g, 'w' FROM generate_series(1, 30000) g;");
	my $want = fp('r2');
	q1('ALTER TABLE r2 SET ACCESS METHOD heap');
	is(fp('r2'), $want, 'to heap: the rows are there');
	is(q1('SELECT kingres.columnar_check_clean()'), 't', '... and the stripe rows of the old file are gone');
	q1('ALTER TABLE r2 SET ACCESS METHOD columnar2');
	is(fp('r2'), $want, 'and back to the second access method: the rows are there');
	q1('VACUUM FULL r2');
	is(fp('r2'), $want, 'VACUUM FULL: the rows are there');
	is(q1('SELECT kingres.columnar_check_clean()'), 't', '... and the check is clean');
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid NOT IN (SELECT kingres.columnar_identity(c.oid) FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE a.amhandler = 'kingres.columnar_handler(internal)'::regprocedure AND c.relkind = 'r')"),
		'0', 'no stripe row is left without a table');
	q1('DROP TABLE r2');
}

# ------------------------------------------------------------------------
# 5. a relation whose access method cannot be classified: the cleanup deletes nothing
# ------------------------------------------------------------------------
{
	q1("CREATE TABLE keep (a int, b text) USING columnar; INSERT INTO keep SELECT g, 'k' FROM generate_series(1, 20000) g;");
	my $want = fp('keep');
	my $open = $node->background_psql('postgres', on_error_stop => 1);
	$open->query_safe("BEGIN; CREATE ACCESS METHOD columnar3 TYPE TABLE HANDLER kingres.columnar_handler; CREATE TABLE u3 (a int, b text) USING columnar3; INSERT INTO u3 VALUES (1, 'x');");
	# stripes with no table: made by hand, as a drop that has left them would
	q1("INSERT INTO kingres.columnar_stripe SELECT 424242424242, stripe_id, first_row, row_count, chunk_groups, natts, first_block, block_count, blob_len, raw_bytes, footer_offset, cid_points, col_stats, maintenance FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('keep') AND stripe_id = (SELECT min(stripe_id) FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('keep'))");
	my $off = -s $node->logfile;
	my ($ret, $out, $err) = $node->psql('postgres', "SELECT kingres.columnar_reconcile() IS NULL");
	is($out, 't', 'with the unclassified relation present the reconciliation answers NULL, not 0: it did nothing');
	like($err, qr/WARNING:  columnar_reconcile did nothing: a relation has an access method that cannot be classified/, '... and says why in a warning');
	like(slurp_file($node->logfile, $off), qr/cleanup skipped: a relation has an access method that cannot be classified/, '... and in the log');
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = 424242424242"), '1', '... the stripe row of no table is still there');
	# C12-01: the check does not say "clean" for a step that did not run
	is(q1('SELECT kingres.columnar_check_clean()'), 'f', 'columnar_check_clean() is false while the orphan step cannot run (it was true)');
	is(q1("SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'not_checked' AND relation IS NULL AND detail ~ 'orphan step could not be done: a relation has an access method that cannot be classified'"), '1',
		'columnar_check() has a not_checked row with the reason');
	is(q1("SELECT count(*) FROM kingres.columnar_check(true) WHERE check_name = 'not_checked' AND detail ~ 'orphan step could not be done'"), '1',
		'... so does columnar_check(true), which deletes nothing');
	is(q1("SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'orphan_catalog_rows'"), '0', '... and it does not claim to have counted orphans');
	($ret, $out, $err) = $node->psql('postgres', "VACUUM keep");
	is($err, '', 'VACUUM stays quiet (the log line is the trace)');
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = 424242424242"), '1', '... and deleted nothing');
	$open->query_safe('ROLLBACK');
	$open->quit;
	is(q1("SELECT string_agg(check_name, ',') FROM kingres.columnar_check()"), 'orphan_catalog_rows', 'once the transaction is over the check reports the orphan row');
	is(q1('SELECT kingres.columnar_check_clean()'), 'f', '... and is not clean');
	is(q1("SELECT kingres.columnar_reconcile()"), '1', 'the reconciliation deletes it (and only it)');
	is(fp('keep'), $want, 'the table that was there is intact');
}

# ------------------------------------------------------------------------
# 5b. C12-01: a columnar file with a damaged metapage makes the identities unknown: the same visible outcome
# ------------------------------------------------------------------------
{
	q1("CREATE TABLE bm (a int, b text) USING columnar; INSERT INTO bm SELECT g, 'm' FROM generate_series(1, 5000) g;");
	q1("INSERT INTO kingres.columnar_stripe SELECT 434343434343, stripe_id, first_row, row_count, chunk_groups, natts, first_block, block_count, blob_len, raw_bytes, footer_offset, cid_points, col_stats, maintenance FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('keep') AND stripe_id = (SELECT min(stripe_id) FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('keep'))");
	q1('CHECKPOINT');
	my $path = $node->data_dir . '/' . q1("SELECT pg_relation_filepath('bm')");
	open(my $fh, '+<', $path) or die "$path: $!";
	binmode($fh);
	seek($fh, 24, 0) or die "seek: $!";
	print $fh "\xff\xff\xff\xff\xff\xff\xff\xff";
	close($fh) or die "close: $!";
	is(q1('SELECT kingres.columnar_check_clean()'), 'f', 'a damaged metapage: columnar_check_clean() is false');
	like(q1("SELECT detail FROM kingres.columnar_check() WHERE check_name = 'not_checked' AND relation IS NULL"), qr/orphan step could not be done: a columnar relation has an unreadable metapage/,
		'... the check says that the orphan step could not be done, and why');
	my ($ret, $out, $err) = $node->psql('postgres', "SELECT kingres.columnar_reconcile() IS NULL");
	is($out, 't', '... columnar_reconcile() answers NULL');
	like($err, qr/WARNING:  columnar_reconcile did nothing: a columnar relation has an unreadable metapage/, '... with a warning');
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = 434343434343"), '1', '... and the orphan row is still there');
	q1('DROP TABLE bm');
	cmp_ok(q1("SELECT kingres.columnar_reconcile()"), '>=', 1, 'with the damaged table gone the reconciliation runs again and deletes the orphan rows');
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = 434343434343"), '0', '... including the one made by hand');
}

# ------------------------------------------------------------------------
# 6. C12-03: a second SQL function over the C symbol: the guard recognises its access method as columnar
# ------------------------------------------------------------------------
{
	q1(q{CREATE FUNCTION kingres.h2(internal) RETURNS table_am_handler AS '$libdir/kingres_columnar', 'kingres_columnar_handler' LANGUAGE C;
		CREATE ACCESS METHOD columnar4 TYPE TABLE HANDLER kingres.h2;
		CREATE TABLE hp (a int PRIMARY KEY);
		CREATE TABLE fk_real (a int) USING columnar;
		CREATE TABLE fk_alias (a int) USING columnar4;
		CREATE TABLE fk_heap (a int);
		CREATE FUNCTION trf() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;});
	foreach my $t ('fk_real', 'fk_alias')
	{
		my ($ret, $out, $err) = $node->psql('postgres', "ALTER TABLE $t ADD FOREIGN KEY (a) REFERENCES hp(a)");
		like($err, qr/foreign keys on or to columnar tables are not supported/, "$t: a foreign key is refused");
		($ret, $out, $err) = $node->psql('postgres', "CREATE TRIGGER tr AFTER INSERT ON $t FOR EACH ROW EXECUTE FUNCTION trf()");
		like($err, qr/row-level AFTER triggers on columnar tables are not supported/, "$t: an AFTER ROW trigger is refused");
		($ret, $out, $err) = $node->psql('postgres', "CREATE PUBLICATION pb_$t FOR TABLE $t");
		like($err, qr/columnar tables are not replicated logically yet/, "$t: a publication of it is refused");
	}
	my ($ret, $out, $err) = $node->psql('postgres', "ALTER TABLE fk_heap ADD FOREIGN KEY (a) REFERENCES hp(a)");
	unlike($err, qr/ERROR/, 'a heap table is not touched by the guard');
	($ret, $out, $err) = $node->psql('postgres', "CREATE PUBLICATION pb_heap FOR TABLE fk_heap");
	unlike($err, qr/ERROR/, '... nor is a publication of it');
	q1('DROP PUBLICATION pb_heap; DROP TABLE fk_real, fk_alias, fk_heap, hp; DROP ACCESS METHOD columnar4; DROP FUNCTION kingres.h2(internal), trf()');
}

# ------------------------------------------------------------------------
# 7. C12-05: a second session that classified an access method sees a handler that was replaced
#    (the cache clears on a change of pg_am or pg_proc).  Another session replaces the handler function of
#    cx (the extension's routine, then a copy of it, which is not the extension's own): the table of cx stops
#    being columnar for the check, whose orphan step then finds its stripe rows without a table; the first
#    session still thinks cx is columnar if the cache is never cleared.
# ------------------------------------------------------------------------
{
	q1(qq{CREATE FUNCTION kingres.hx(internal) RETURNS table_am_handler AS '\$libdir/kingres_columnar', 'kingres_columnar_handler' LANGUAGE C;
		CREATE ACCESS METHOD cx TYPE TABLE HANDLER kingres.hx;
		CREATE TABLE tx (a int, b text) USING cx;
		INSERT INTO tx SELECT g, 'x' FROM generate_series(1, 3000) g;});
	my $b = $node->background_psql('postgres', on_error_stop => 1);
	is(bgq($b, 'SELECT kingres.columnar_check_clean()'), 't', 'the second session classifies cx as columnar (and it is cached there)');
	is(bgq($b, "SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'orphan_catalog_rows'"), '0', '... and sees no orphan');
	q1(qq{CREATE OR REPLACE FUNCTION kingres.hx(internal) RETURNS table_am_handler AS '$pdir/kcprobe', 'kcprobe_am_copy' LANGUAGE C});
	is(bgq($b, "SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'orphan_catalog_rows'"), '1',
		'the handler of cx was replaced by another session: the second session sees it (the stripe rows of tx have no columnar table now)');
	q1(qq{CREATE OR REPLACE FUNCTION kingres.hx(internal) RETURNS table_am_handler AS '\$libdir/kingres_columnar', 'kingres_columnar_handler' LANGUAGE C});
	is(bgq($b, 'SELECT kingres.columnar_check_clean()'), 't', 'replaced back: the second session sees that too');
	$b->quit;
	q1('DROP TABLE tx; DROP ACCESS METHOD cx; DROP FUNCTION kingres.hx(internal)');
}

# ------------------------------------------------------------------------
# 8. C12-04: a handler that returns a copy of the routine: the table has the extension's callbacks and is not
#    recognised by it, so a write would be dropped at commit.  It is refused.
# ------------------------------------------------------------------------
{
	q1(qq{CREATE FUNCTION kingres.hcopy(internal) RETURNS table_am_handler AS '$pdir/kcprobe', 'kcprobe_am_copy' LANGUAGE C;
		CREATE ACCESS METHOD am_copy TYPE TABLE HANDLER kingres.hcopy;
		CREATE TABLE w_copy (a int, b text) USING am_copy;});
	foreach my $sql ("INSERT INTO w_copy SELECT g, 'c' FROM generate_series(1, 5000) g", "INSERT INTO w_copy VALUES (1, 'c')",
		"COPY w_copy (a) FROM PROGRAM 'seq 1 100'", "CREATE TABLE w_ctas USING am_copy AS SELECT g AS a FROM generate_series(1, 100) g")
	{
		my ($ret, $out, $err) = $node->psql('postgres', $sql);
		like($err, qr/cannot insert into table "w_(copy|ctas)": its access method uses the columnar callbacks/, "refused: $sql");
	}
	is(q1('SELECT count(*) FROM w_copy'), '0', 'the table holds no row, and none was accepted');
	q1('DROP TABLE w_copy; DROP TABLE IF EXISTS w_ctas; DROP ACCESS METHOD am_copy; DROP FUNCTION kingres.hcopy(internal)');
	is(q1('SELECT kingres.columnar_check_clean()'), 't', 'the check is clean afterwards');
}

# ------------------------------------------------------------------------
# 9. C12-06: a handler that raises an error (the handler of an access method of another extension): a call
#    of the check or the reconciliation stands down and says so; it does not fail
# ------------------------------------------------------------------------
{
	q1(qq{CREATE FUNCTION kingres.herr(internal) RETURNS table_am_handler AS '$pdir/kcprobe', 'kcprobe_am_error' LANGUAGE C;
		CREATE ACCESS METHOD am_err TYPE TABLE HANDLER kingres.herr;
		CREATE TABLE we (a int) USING am_err;});
	# one session (a fresh one has nothing cached about am_err), the handler fails from the first call on
	my $off = -s $node->logfile;
	my ($ret, $out, $err) = $node->psql('postgres', "SET kcprobe.am_error = on;
SELECT kingres.columnar_check_clean();
SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'not_checked' AND detail ~ 'orphan step could not be done: a relation has an access method that cannot be classified';
SELECT kingres.columnar_reconcile() IS NULL;
VACUUM keep;
SELECT count(*) FROM keep;");
	is($ret, 0, 'the handler of another access method raises an error: no call fails');
	is($out, "f\n1\nt\n20000", '... columnar_check_clean() is false, columnar_check() says that the orphan step could not be done, columnar_reconcile() answers NULL, the rows are all there');
	unlike($err, qr/ERROR/, '... and nothing raised an error');
	like(slurp_file($node->logfile, $off), qr/the handler of access method \d+ raised an error and the access method cannot be classified: kcprobe: the handler of this access method is broken/, '... the log has the error, and VACUUM went on');
	is(q1('SELECT kingres.columnar_check_clean()'), 't', 'without the error the check is clean');
	q1('DROP TABLE we; DROP ACCESS METHOD am_err; DROP FUNCTION kingres.herr(internal)');
}

is(q1('SELECT kingres.columnar_check_clean()'), 't', 'at the end the check is clean');
$node->stop;
done_testing();
