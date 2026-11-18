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
#      catalog does not see the access method).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

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
	is(q1("SELECT kingres.columnar_reconcile()"), '0', 'with the unclassified relation present the reconciliation deletes nothing');
	like(slurp_file($node->logfile, $off), qr/cleanup skipped: a relation has an access method that cannot be classified/, '... and says so in the log');
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = 424242424242"), '1', '... the stripe row of no table is still there');
	$open->query_safe('ROLLBACK');
	$open->quit;
	is(q1("SELECT kingres.columnar_reconcile()"), '1', 'once the transaction is over the reconciliation deletes it (and only it)');
	is(fp('keep'), $want, 'the table that was there is intact');
}

is(q1('SELECT kingres.columnar_check_clean()'), 't', 'at the end the check is clean');
$node->stop;
done_testing();
