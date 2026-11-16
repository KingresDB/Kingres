# contrib/kingres_columnar/t/023_rewrite_cleanup.pl
#
# C5-04, C5-05 and C5-06.
#
# C5-04: the stripe rows of a file that a command replaced are removed by that
# command, and only those: the cost does not depend on how many columnar tables
# exist.  The defect: every ALTER TABLE of any table scanned pg_class and read
# block 0 of every columnar table (35 ms at 5000 tables, a restore of 3000
# columnar tables 6 times slower than without the trigger).  Deterministic test:
# an ALTER TABLE of an unrelated table deletes no stripe row at all, not even
# one that is already an orphan; the rows of the replaced file are gone when the
# command ends, in its transaction, and come back with a rollback.
#
# C5-05: two commands on unrelated tables do not block or fail each other, and a
# command, VACUUM or columnar_reconcile() that finds a row that another session
# deleted or is deleting is not an error ("tuple concurrently deleted").
#
# C5-06: a stripe catalog of the earlier shape (without the column
# "maintenance") is refused with the exact statement that repairs it without
# losing a row, and the statement works; the hint never tells anyone to drop
# the extension (which drops the tables).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colcleanup');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub q1
{
	return $node->safe_psql('postgres', $_[0]);
}

sub script
{
	my ($sql) = @_;
	my ($out, $err) = ('', '');

	$node->psql('postgres', $sql, stdout => \$out, stderr => \$err, on_error_stop => 0);
	return ($out, $err);
}

sub orphans
{
	return q1("SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'orphan_catalog_rows'");
}

# the number of stripe rows of the file of a table (visible now)
sub stripe_rows
{
	return q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('$_[0]')");
}

sub fresh_x
{
	q1("DROP TABLE IF EXISTS x CASCADE; DROP MATERIALIZED VIEW IF EXISTS mv CASCADE;
		CREATE TABLE x (a int, b int) USING columnar;
		SELECT kingres.columnar_set_options('x', stripe_row_count => 1000, chunk_row_count => 100);
		INSERT INTO x SELECT g, g FROM generate_series(1, 5000) g;");
}

# ------------------------------------------------------------------------
# 1. What each command that replaces a file leaves behind: nothing
# ------------------------------------------------------------------------
my @cmds = (
	['VACUUM FULL', 'VACUUM FULL x'],
	['a rewriting ALTER TABLE (type change)', 'ALTER TABLE x ALTER COLUMN b TYPE bigint'],
	['ADD COLUMN with a volatile default', 'ALTER TABLE x ADD COLUMN c float DEFAULT random()'],
	['SET ACCESS METHOD heap', 'ALTER TABLE x SET ACCESS METHOD heap'],
	['TRUNCATE', 'TRUNCATE x'],
	['DROP TABLE', 'DROP TABLE x'],
);
foreach my $c (@cmds)
{
	my ($name, $sql) = @$c;
	fresh_x();
	is(orphans(), 0, "$name: nothing left over before the command");
	my $uid = q1("SELECT kingres.columnar_identity('x')");
	my $n0 = q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid");
	cmp_ok($n0, '>=', 5, "$name: the table has $n0 stripe rows");
	q1($sql);
	is(orphans(), 0, "$name: columnar_check has nothing to report after the command");
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), 0,
		"$name: no stripe row of the old file is left");

	next if $sql =~ /^VACUUM/;		# VACUUM cannot run in a transaction block

	# the same in a transaction that is rolled back: the old file is back whole
	fresh_x();
	$uid = q1("SELECT kingres.columnar_identity('x')");
	script("BEGIN; $sql; ROLLBACK;");
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), $n0,
		"$name, rolled back: the stripe rows of the old file are all there");
	is(q1('SELECT count(*) || \',\' || sum(a) FROM x'), '5000,12502500', "$name, rolled back: the table reads as before");
	is(orphans(), 0, "$name, rolled back: nothing left over");
}

# a session that turned the event triggers off leaves nothing either: the
# access method knows which files its rewrites replaced
{
	fresh_x();
	my $uid = q1("SELECT kingres.columnar_identity('x')");
	script("SET event_triggers = off; ALTER TABLE x ALTER COLUMN b TYPE bigint;");
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), 0,
		'event triggers off: the rewrite removed the old file\'s rows with its own commit');
	is(orphans(), 0, 'event triggers off: nothing left over');
}

# a materialized view
{
	q1("DROP MATERIALIZED VIEW IF EXISTS mv; CREATE MATERIALIZED VIEW mv USING columnar AS SELECT g AS a FROM generate_series(1, 3000) g");
	my $uid = q1("SELECT kingres.columnar_identity('mv')");
	q1("REFRESH MATERIALIZED VIEW mv");
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), 0, 'REFRESH MATERIALIZED VIEW: the old file\'s rows are removed');
	is(orphans(), 0, 'REFRESH MATERIALIZED VIEW: nothing left over');
	is(q1('SELECT count(*) FROM mv'), 3000, 'REFRESH MATERIALIZED VIEW: the view reads');
}

# a rewrite that fails removes nothing, and leaves nothing noted
{
	fresh_x();
	my $uid = q1("SELECT kingres.columnar_identity('x')");
	my $n0 = stripe_rows('x');
	my ($out, $err) = script("ALTER TABLE x ALTER COLUMN b TYPE int2 USING (b / (b - 4000))");
	like($err, qr/division by zero/, 'a rewrite that fails halfway reports the failure');
	is(stripe_rows('x'), $n0, '... and removed no stripe row');
	is(q1('SELECT count(*) FROM x'), 5000, '... the table reads as before');
	q1("ALTER TABLE x ALTER COLUMN b TYPE bigint");
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), 0, 'the next rewrite removes the right rows');
	is(orphans(), 0, '... and nothing is left over');
}

# a table of the same transaction rewritten twice
{
	fresh_x();
	script("BEGIN; ALTER TABLE x ALTER COLUMN b TYPE bigint; ALTER TABLE x ALTER COLUMN b TYPE numeric; INSERT INTO x VALUES (1, 1); COMMIT;");
	is(orphans(), 0, 'two rewrites in one transaction: nothing left over');
	is(q1('SELECT count(*) FROM x'), 5001, 'two rewrites in one transaction: the table reads');
}

# ------------------------------------------------------------------------
# 2. An ALTER TABLE of an unrelated table does not look at the stripe catalog
# ------------------------------------------------------------------------
{
	fresh_x();
	q1("DROP TABLE IF EXISTS ha, hb; CREATE TABLE ha (a int); CREATE TABLE hb (a int)");
	# an orphan row, as an older database or a restore with event triggers off leaves
	q1("INSERT INTO kingres.columnar_stripe
		SELECT rel_uid + 1000003, stripe_id, first_row, row_count, chunk_groups, natts, first_block,
		       block_count, blob_len, raw_bytes, footer_offset, cid_points, col_stats, maintenance
		  FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('x') LIMIT 1");
	is(orphans(), 1, 'one orphan row made by hand');
	q1("ALTER TABLE ha ADD COLUMN z int; ALTER TABLE hb ADD COLUMN y int DEFAULT 5");
	is(orphans(), 1, 'ALTER TABLE of unrelated tables does not touch the catalog: the orphan row is still there');
	q1("SELECT kingres.columnar_reconcile()");
	is(orphans(), 0, 'columnar_reconcile() and VACUUM still remove it');
	q1("INSERT INTO kingres.columnar_stripe
		SELECT rel_uid + 1000003, stripe_id, first_row, row_count, chunk_groups, natts, first_block,
		       block_count, blob_len, raw_bytes, footer_offset, cid_points, col_stats, maintenance
		  FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('x') LIMIT 1");
	q1("VACUUM x");
	is(orphans(), 0, 'VACUUM of a columnar table removes the orphan row');
}

# ------------------------------------------------------------------------
# 3. Concurrency: unrelated commands neither block nor fail each other, and a
#    row that another session deleted is not an error
# ------------------------------------------------------------------------
{
	fresh_x();
	q1("DROP TABLE IF EXISTS x2, ha, hb; CREATE TABLE ha (a int); CREATE TABLE hb (a int);
		CREATE TABLE x2 (a int, b int) USING columnar; INSERT INTO x2 SELECT g, g FROM generate_series(1, 3000) g;");

	# session A holds a rewrite of x open
	my $a = $node->background_psql('postgres', on_error_stop => 1);
	$a->query_safe("BEGIN");
	$a->query_safe("ALTER TABLE x ALTER COLUMN b TYPE bigint");

	# others run meanwhile; lock_timeout makes a wait an error, not a hang
	my ($out, $err) = script("SET lock_timeout = '20s';
		ALTER TABLE ha ADD COLUMN z int;
		ALTER TABLE hb ADD COLUMN y int;
		ALTER TABLE x2 ALTER COLUMN b TYPE bigint;
		SELECT 'DONE';");
	unlike($err, qr/ERROR|timeout/, 'commands on unrelated tables run while a rewrite is open: no wait, no error');
	like($out, qr/DONE/, '... and they complete');
	($out, $err) = script("SET lock_timeout = '20s'; SELECT kingres.columnar_reconcile(); VACUUM x2; SELECT 'DONE'");
	unlike($err, qr/ERROR|timeout/, 'columnar_reconcile() and VACUUM run while a rewrite is open: no wait, no error');
	$a->query_safe("COMMIT");
	$a->quit;
	is(q1('SELECT count(*) FROM x'), 5000, 'the rewrite committed');
	is(orphans(), 0, 'nothing is left over');
}

{
	# the same leftover row deleted by two sessions at once
	fresh_x();
	q1("INSERT INTO kingres.columnar_stripe
		SELECT rel_uid + 1000003, stripe_id, first_row, row_count, chunk_groups, natts, first_block,
		       block_count, blob_len, raw_bytes, footer_offset, cid_points, col_stats, maintenance
		  FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('x') LIMIT 1");
	my $a = $node->background_psql('postgres', on_error_stop => 1);
	$a->query_safe("BEGIN");
	is($a->query_safe("SELECT kingres.columnar_reconcile()"), 1, 'session A deletes the leftover row and stays open');
	my ($out, $err) = script("SET lock_timeout = '20s'; SELECT 'R=' || kingres.columnar_reconcile(); VACUUM x; SELECT 'DONE'");
	unlike($err, qr/ERROR|timeout/, 'session B finds the row being deleted: no error, no wait');
	like($out, qr/R=0/, '... and leaves it to A');
	$a->query_safe("COMMIT");
	$a->quit;
	is(orphans(), 0, 'the row is gone');

	# and the other way round: A rolls back, the row stays for the next cleanup
	q1("INSERT INTO kingres.columnar_stripe
		SELECT rel_uid + 1000003, stripe_id, first_row, row_count, chunk_groups, natts, first_block,
		       block_count, blob_len, raw_bytes, footer_offset, cid_points, col_stats, maintenance
		  FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('x') LIMIT 1");
	$a = $node->background_psql('postgres', on_error_stop => 1);
	$a->query_safe("BEGIN");
	$a->query_safe("SELECT kingres.columnar_reconcile()");
	($out, $err) = script("SELECT 'R=' || kingres.columnar_reconcile()");
	like($out, qr/R=0/, 'while A has not ended, B deletes nothing');
	$a->query_safe("ROLLBACK");
	$a->quit;
	is(orphans(), 1, 'A rolled back: the row is still there');
	q1("SELECT kingres.columnar_reconcile()");
	is(orphans(), 0, 'and the next cleanup removes it');
}

# ------------------------------------------------------------------------
# 4. A stripe catalog of the earlier shape (13 columns)
# ------------------------------------------------------------------------
{
	q1("DROP TABLE IF EXISTS old1, old2");
	q1("CREATE TABLE old1 (a int, b text) USING columnar;
		SELECT kingres.columnar_set_options('old1', stripe_row_count => 1000, chunk_row_count => 100);
		INSERT INTO old1 SELECT g, 'v' || g FROM generate_series(1, 5010) g;
		CREATE TABLE old2 (a int) USING columnar; INSERT INTO old2 SELECT generate_series(1, 20);");
	my $want = q1("SELECT count(*) || ',' || sum(a) FROM old1");
	is($want, '5010,12552555', 'the table before the catalog is made old');

	# the earlier shape: the same columns without "maintenance"
	q1("ALTER EXTENSION kingres_columnar DROP TABLE kingres.columnar_stripe;
		ALTER TABLE kingres.columnar_stripe RENAME TO columnar_stripe_new;
		ALTER INDEX kingres.columnar_stripe_pkey RENAME TO columnar_stripe_new_pkey;
		CREATE TABLE kingres.columnar_stripe (
		    rel_uid bigint NOT NULL, stripe_id bigint NOT NULL, first_row bigint NOT NULL,
		    row_count int NOT NULL, chunk_groups int NOT NULL, natts smallint NOT NULL,
		    first_block bigint NOT NULL, block_count int NOT NULL, blob_len bigint NOT NULL,
		    raw_bytes bigint NOT NULL, footer_offset bigint NOT NULL, cid_points bytea NOT NULL,
		    col_stats bytea NOT NULL, CONSTRAINT columnar_stripe_pkey PRIMARY KEY (rel_uid, stripe_id));
		INSERT INTO kingres.columnar_stripe
		  SELECT rel_uid, stripe_id, first_row, row_count, chunk_groups, natts, first_block, block_count,
		         blob_len, raw_bytes, footer_offset, cid_points, col_stats
		    FROM kingres.columnar_stripe_new;
		DROP TABLE kingres.columnar_stripe_new;");
	is(q1("SELECT count(*) FROM pg_attribute WHERE attrelid = 'kingres.columnar_stripe'::regclass AND attnum > 0 AND NOT attisdropped"),
		13, 'the catalog now has the 13 columns of the earlier build');

	my $hint;
	foreach my $stmt ('SELECT count(*) FROM old1', 'INSERT INTO old2 VALUES (21)', 'VACUUM old1',
		'SELECT count(*) FROM old2', 'ALTER TABLE old1 SET ACCESS METHOD heap')
	{
		my ($out, $err) = script($stmt);
		like($err, qr/the stripe catalog has 13 columns, this build uses 14/, "$stmt: refused, not read wrongly");
		unlike($err, qr/drop and create the extension/i, "$stmt: the hint does not tell anyone to drop the extension");
		($hint) = $err =~ /run: (ALTER TABLE \S+ ADD COLUMN maintenance boolean NOT NULL DEFAULT false)\./ unless $hint;
	}
	is($hint, 'ALTER TABLE kingres.columnar_stripe ADD COLUMN maintenance boolean NOT NULL DEFAULT false',
		'the hint names the statement that repairs the catalog');
	like(q1("SELECT detail FROM kingres.columnar_check() WHERE check_name = 'unreadable' AND relation = 'old1'::regclass"),
		qr/13 columns/, 'columnar_check reports the table as unreadable, with the reason');

	q1($hint);
	is(q1("SELECT count(*) || ',' || sum(a) FROM old1"), $want, 'after the statement of the hint every row of the table is there');
	is(q1("SELECT count(*) FROM old2"), 20, '... and of the other table');
	q1("INSERT INTO old1 VALUES (5011, 'new'); VACUUM old1");
	is(q1("SELECT count(*) FROM old1"), 5011, '... and the tables can be written and vacuumed');
	is(q1("SELECT kingres.columnar_compact('old1', 2000)") >= 0 ? 'ok' : 'no', 'ok', '... and compacted');
	is(q1("SELECT count(*) FROM kingres.columnar_check()"), 0, '... and the check is clean');
}


# ------------------------------------------------------------------------
# 5. Isolation levels and roles with a rewrite (gaps the earlier reviews named):
#    SERIALIZABLE with DDL in the transaction, READ ONLY DEFERRABLE, and a
#    session that runs as a replica.  Each runs against a heap twin.
# ------------------------------------------------------------------------
{
	my $mk = "DROP TABLE IF EXISTS xh, xc;
		CREATE TABLE xh (a int, b int); INSERT INTO xh SELECT g, g FROM generate_series(1, 3000) g;
		CREATE TABLE xc (a int, b int) USING columnar; INSERT INTO xc SELECT g, g FROM generate_series(1, 3000) g;";
	foreach my $level ('SERIALIZABLE', 'REPEATABLE READ', 'READ COMMITTED')
	{
		my %r;
		q1($mk);
		foreach my $t (qw(xh xc))
		{
			my ($out, $err) = script("
BEGIN ISOLATION LEVEL $level;
SELECT 'A=' || count(*) || ',' || sum(b) FROM $t;
ALTER TABLE $t ALTER COLUMN b TYPE bigint;
INSERT INTO $t VALUES (3001, 3001);
SELECT 'B=' || count(*) || ',' || sum(b) FROM $t;
ALTER TABLE $t ADD COLUMN c float DEFAULT random();
SELECT 'C=' || count(*) || ',' || sum(b) FROM $t;
COMMIT;
SELECT 'D=' || count(*) || ',' || sum(b) FROM $t;");
			unlike($err, qr/ERROR/, "$level, $t: DDL that rewrites, then reads, in one transaction: no error") or diag($err);
			$r{$t} = $out;
		}
		like($r{xc}, qr/A=3000,4501500\s+B=3001,4504501\s+C=3001,4504501\s+D=3001,4504501/,
			"$level: the columnar table sees its rows through every rewrite of the transaction");
		is($r{xc}, $r{xh}, "$level: and gives what its heap twin gives");
	}
	q1($mk);
	foreach my $t (qw(xh xc))
	{
		my ($out, $err) = script("
BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY DEFERRABLE;
SELECT 'R=' || count(*) || ',' || sum(b) FROM $t;
COMMIT;");
		like($out, qr/R=3000,4501500/, "READ ONLY DEFERRABLE read of $t");
		unlike($err, qr/ERROR/, "READ ONLY DEFERRABLE read of $t: no error");
	}

	# a session that is a replica: triggers off, event triggers still on
	fresh_x();
	my $uid = q1("SELECT kingres.columnar_identity('x')");
	my ($out, $err) = script("SET session_replication_role = replica; ALTER TABLE x ALTER COLUMN b TYPE bigint; INSERT INTO x VALUES (1, 1);");
	unlike($err, qr/ERROR/, 'a replica session: a rewrite and an insert');
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), 0, 'a replica session: the rewrite removed the old file\'s rows');
	is(orphans(), 0, 'a replica session: nothing is left over');
}

$node->stop;
done_testing();
