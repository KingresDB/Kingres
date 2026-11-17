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

# C6-04, C6-05: correctness never depends on an event trigger firing.  A session
# that turned the event triggers off, a trigger that is disabled, a database whose
# install script is an earlier one (it has no trigger the access method now uses),
# single-user mode: whatever replaces or drops a file, nothing is left over once
# the transaction has committed, and the table reads.  The access method itself
# notes the files that go away (the drop of the relation that holds them); the
# triggers only do it at the end of the command instead of at the commit.
{
	my @cmds2 = (
		['type change (columnar to columnar)', 'ALTER TABLE x ALTER COLUMN b TYPE bigint'],
		['SET ACCESS METHOD heap (columnar to heap)', 'ALTER TABLE x SET ACCESS METHOD heap'],
		['ADD COLUMN with a volatile default', 'ALTER TABLE x ADD COLUMN c float DEFAULT random()'],
		['DROP TABLE', 'DROP TABLE x'],
		['VACUUM FULL', 'VACUUM FULL x'],
		['TRUNCATE', 'TRUNCATE x'],
	);
	my %modes = (
		'event_triggers = off' => [ 'SET event_triggers = off;', '' ],
		'the triggers disabled' => [ '', '' ],
	);
	foreach my $mode ('event_triggers = off', 'the triggers disabled')
	{
		if ($mode eq 'the triggers disabled')
		{
			q1("ALTER EVENT TRIGGER kingres_columnar_on_drop DISABLE; ALTER EVENT TRIGGER kingres_columnar_check_ddl DISABLE");
		}
		foreach my $c (@cmds2)
		{
			my ($name, $sql) = @$c;
			my $pre = $modes{$mode}[0];
			fresh_x();
			my $uid = q1("SELECT kingres.columnar_identity('x')");
			my $n0 = stripe_rows('x');
			script("$pre $sql;");
			is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), 0,
				"$mode, $name: no stripe row of the old file is left");
			is(orphans(), 0, "$mode, $name: nothing left over");
			unless ($sql =~ /DROP TABLE|TRUNCATE/)
			{
				is(q1('SELECT count(*) FROM x'), 5000, "$mode, $name: the table reads");
			}

			# in a transaction block, committed
			next if $sql =~ /^VACUUM/;
			fresh_x();
			$uid = q1("SELECT kingres.columnar_identity('x')");
			script("$pre BEGIN; $sql; COMMIT;");
			is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), 0,
				"$mode, $name, in a transaction block: no stripe row of the old file is left");

			# and rolled back: the old file is back whole
			fresh_x();
			$uid = q1("SELECT kingres.columnar_identity('x')");
			script("$pre BEGIN; $sql; ROLLBACK;");
			is(stripe_rows('x'), $n0, "$mode, $name, rolled back: the stripe rows of the old file are all there");
			is(q1('SELECT count(*) || \',\' || sum(a) FROM x'), '5000,12502500', "$mode, $name, rolled back: the table reads as before");
		}
		if ($mode eq 'the triggers disabled')
		{
			q1("ALTER EVENT TRIGGER kingres_columnar_on_drop ENABLE ALWAYS; ALTER EVENT TRIGGER kingres_columnar_check_ddl ENABLE ALWAYS");
		}
	}

	# the files of a partitioned table, a schema and an extension-free cascade
	q1("DROP TABLE IF EXISTS pt CASCADE; DROP SCHEMA IF EXISTS sch CASCADE;
		CREATE TABLE pt (a int) PARTITION BY RANGE (a);
		CREATE TABLE pt1 PARTITION OF pt FOR VALUES FROM (0) TO (100000) USING columnar;
		INSERT INTO pt SELECT g FROM generate_series(1, 4000) g;
		CREATE SCHEMA sch; CREATE TABLE sch.s1 (a int) USING columnar; INSERT INTO sch.s1 SELECT g FROM generate_series(1, 4000) g;");
	script("SET event_triggers = off; DROP TABLE pt; DROP SCHEMA sch CASCADE;");
	is(orphans(), 0, 'event_triggers = off: DROP of a partitioned table and DROP SCHEMA CASCADE leave nothing');

	# single-user mode never fires event triggers; the library is loaded as in any server
	fresh_x();
	my $uid = q1("SELECT kingres.columnar_identity('x')");
	$node->stop;
	my $input = "ALTER TABLE x SET ACCESS METHOD heap;\n\n";
	my $ok = PostgreSQL::Test::Utils::run_log([ 'postgres', '--single', '-j', '-D', $node->data_dir, 'postgres' ], '<', \$input);
	ok($ok, 'single-user mode: a change away from columnar runs');
	$node->start;
	is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), 0, 'single-user mode: the old file\'s rows are removed with the commit');
	is(orphans(), 0, 'single-user mode: nothing left over');
	is(q1('SELECT count(*) || \',\' || (SELECT amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.relname = \'x\') FROM x'),
		'5000,heap', 'single-user mode: the table is heap and has its rows');

	# leftover rows can never be read as stripes of a later file: a new file draws an identity
	# that no row of the catalog (live or dead) has, so rows left by a missed cleanup are never
	# found by the reader of another file
	fresh_x();
	$uid = q1("SELECT kingres.columnar_identity('x')");
	q1("INSERT INTO kingres.columnar_stripe
		SELECT rel_uid + 1000003, stripe_id, first_row, row_count, chunk_groups, natts, first_block,
		       block_count, blob_len, raw_bytes, footer_offset, cid_points, col_stats, maintenance
		  FROM kingres.columnar_stripe WHERE rel_uid = $uid");
	my $leftover = $uid + 1000003;
	is(q1("SELECT kingres.columnar_identity('x') <> $leftover"), 't', 'a leftover row of another identity is not the table\'s');
	is(q1('SELECT count(*) FROM x'), 5000, '... and the reader does not take it');
	my @used = map { q1("CREATE TABLE ni$_ (a int) USING columnar; SELECT kingres.columnar_identity('ni$_')") } 1 .. 20;
	is(scalar(grep { $_ == $leftover } @used), 0, '... new files draw identities that the catalog does not use');
	q1("DROP TABLE ni$_") for 1 .. 20;
	q1("SELECT kingres.columnar_reconcile()");
	is(orphans(), 0, '... and the next cleanup removes it');
}

# C6-05: a database made by an earlier install script (a table_rewrite trigger
# that calls columnar_note_rewrite, a DROP trigger that runs the full reconcile)
# works fully, and its trigger functions can be replaced by the current ones
{
	my $pgc = $ENV{PG_CONFIG} || 'pg_config';
	my $share = `$pgc --sharedir 2>/dev/null`;
	chomp $share;
	my $script = "$share/extension/kingres_columnar--0.1.sql";
	my $text = '';
	if (-f $script) { open(my $fh, '<', $script) or die; local $/; $text = <$fh>; close $fh; }

	q1(q{CREATE FUNCTION kingres.columnar_note_rewrite(rel regclass) RETURNS void
		AS '$libdir/kingres_columnar', 'kingres_columnar_note_rewrite' LANGUAGE C STRICT SECURITY DEFINER
		SET search_path = pg_catalog, kingres, pg_temp;
		CREATE FUNCTION kingres.old_on_rewrite() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER
		SET search_path = pg_catalog, kingres, pg_temp AS $$ BEGIN
		  PERFORM kingres.columnar_note_rewrite(pg_event_trigger_table_rewrite_oid()); END; $$;
		CREATE EVENT TRIGGER old_on_rewrite ON table_rewrite EXECUTE FUNCTION kingres.old_on_rewrite();
		CREATE OR REPLACE FUNCTION kingres.columnar_on_drop() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER
		SET search_path = pg_catalog, kingres, pg_temp AS $$ BEGIN
		  IF EXISTS (SELECT 1 FROM pg_event_trigger_dropped_objects() WHERE object_type IN ('table', 'materialized view')) THEN
		    PERFORM kingres.columnar_reconcile(); END IF; END; $$;});
	foreach my $c (@cmds)
	{
		my ($name, $sql) = @$c;
		next if $sql =~ /^VACUUM/;
		fresh_x();
		my $uid = q1("SELECT kingres.columnar_identity('x')");
		script("$sql;");
		is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), 0, "earlier script, $name: no stripe row of the old file is left");
		is(orphans(), 0, "earlier script, $name: nothing left over");
	}
	SKIP:
	{
		skip 'the install script is not readable here', 3 unless $text =~ /CREATE FUNCTION columnar_on_drop\(\)/;
		foreach my $fn ('columnar_on_drop', 'columnar_check_ddl')
		{
			my ($def) = $text =~ /(CREATE FUNCTION \Q$fn\E\(\).*?\n\$\$;)/s;
			$def =~ s/^CREATE FUNCTION /CREATE OR REPLACE FUNCTION kingres./;
			my ($out, $err) = script($def);
			unlike($err, qr/ERROR/, "the current $fn replaces the old one with CREATE OR REPLACE");
		}
		fresh_x();
		my $uid = q1("SELECT kingres.columnar_identity('x')");
		script("ALTER TABLE x SET ACCESS METHOD heap;");
		is(orphans(), 0, '... and the cleanup still leaves nothing over');
	}
	q1("DROP EVENT TRIGGER old_on_rewrite; DROP FUNCTION kingres.old_on_rewrite(); DROP FUNCTION kingres.columnar_note_rewrite(regclass)");
}

# C6-07: what a failed rewrite or a rolled-back one leaves behind must not be
# deleted by the cleanup (the table would be emptied), and the cleanup at the end
# of the command has run when the command is over, not at the commit
{
	fresh_x();
	my $n0 = stripe_rows('x');
	script("BEGIN;
DO \$\$ BEGIN BEGIN ALTER TABLE x ALTER COLUMN b TYPE int2 USING (b / (b - 4000));
	EXCEPTION WHEN division_by_zero THEN NULL; END; END \$\$;
ALTER TABLE x ADD COLUMN c int;
COMMIT;");
	is(q1('SELECT count(*) FROM x'), 5000, 'a failed rewrite in a block, then another command and a commit: the table keeps its rows');
	is(orphans(), 0, '... and nothing is left over');

	fresh_x();
	q1("ALTER EVENT TRIGGER kingres_columnar_check_ddl DISABLE; ALTER EVENT TRIGGER kingres_columnar_on_drop DISABLE");
	script("BEGIN; SAVEPOINT s; ALTER TABLE x ALTER COLUMN b TYPE bigint; ROLLBACK TO s; ALTER TABLE x ADD COLUMN c int; COMMIT;");
	is(q1('SELECT count(*) FROM x'), 5000, 'a rewrite rolled back to a savepoint, the triggers disabled, then a commit: the table keeps its rows');
	q1("ALTER EVENT TRIGGER kingres_columnar_check_ddl ENABLE ALWAYS; ALTER EVENT TRIGGER kingres_columnar_on_drop ENABLE ALWAYS");

	fresh_x();
	script("BEGIN; SAVEPOINT s; DROP TABLE x; ROLLBACK TO s; INSERT INTO x VALUES (1, 1); COMMIT;");
	is(q1('SELECT count(*) FROM x'), 5001, 'a DROP rolled back to a savepoint, then a commit: the table keeps its rows');

	# the cleanup at the end of the command: inside the transaction, after the command, nothing is left
	foreach my $c (['ALTER TABLE x ALTER COLUMN b TYPE bigint', 'a rewriting ALTER TABLE'],
		['ALTER TABLE x SET ACCESS METHOD heap', 'SET ACCESS METHOD heap'], ['DROP TABLE x', 'DROP TABLE'])
	{
		my ($sql, $name) = @$c;
		fresh_x();
		my ($out, $err) = script("BEGIN; $sql; SELECT 'O=' || count(*) FROM kingres.columnar_check() WHERE check_name = 'orphan_catalog_rows'; ROLLBACK;");
		like($out, qr/O=0\b/, "$name: when the command is over, inside its transaction, no stripe row of the old file is left");
	}
}

# C7-03: the two defenses of the cleanup, each by a case the other cannot decide.
#
# (1) A note follows the subtransaction that made it: it goes with an abort and
#     becomes the parent's with a release.  Where the file was replaced by a rewrite
#     the table that holds the note is the transient one, which does not exist
#     after a rollback, so the comparison with the file the table has cannot tell
#     "replaced" from "rolled back": only the note's own life does.  A released
#     savepoint inside one that is rolled back (the review's shape, event
#     triggers off so that no command-end cleanup consumes the note first) must
#     leave the rows; a note that stayed with the released savepoint would empty
#     the table.  The same with two releases, and with a single rolled back
#     savepoint; and a release that is followed by a commit must still remove them.
# (2) The comparison: a note of a file that the table still has deletes nothing.
#     Nothing in a rollback can make a note of a file in use that the note's life
#     does not already take away, but a note can be made of a file that is not
#     replaced at all (columnar_note_rewrite, kept for databases with the earlier
#     script, called for a table that is then not rewritten); the commit must
#     delete nothing for it.
{
	foreach my $c (['DROP TABLE x', 'a DROP'], ['ALTER TABLE x ALTER COLUMN b TYPE bigint', 'a rewriting ALTER TABLE'],
		['ALTER TABLE x SET ACCESS METHOD heap', 'SET ACCESS METHOD heap'])
	{
		my ($sql, $name) = @$c;
		foreach my $shape (
			['SAVEPOINT a; SAVEPOINT b; %S%; RELEASE b; ROLLBACK TO a;', 'a released savepoint inside one that is rolled back'],
			['SAVEPOINT a; SAVEPOINT b; SAVEPOINT c; %S%; RELEASE c; RELEASE b; ROLLBACK TO a;', 'two releases, then a rollback to the outermost'],
			['SAVEPOINT a; %S%; ROLLBACK TO a;', 'a rolled back savepoint'])
		{
			my ($t, $sname) = @$shape;
			(my $body = $t) =~ s/%S%/$sql/;
			fresh_x();
			script("SET event_triggers = off; BEGIN; $body COMMIT;");
			is(q1('SELECT count(*) FROM x'), 5000, "$name, $sname, event triggers off: the table keeps its rows");
			cmp_ok(stripe_rows('x'), '>=', 5, '... and its stripe rows');
		}
		# released and committed: the file is gone and so are its rows
		fresh_x();
		my $uid = q1("SELECT kingres.columnar_identity('x')");
		script("SET event_triggers = off; BEGIN; SAVEPOINT a; SAVEPOINT b; $sql; RELEASE b; RELEASE a; COMMIT;");
		is(q1("SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = $uid"), 0, "$name, released twice and committed: the old file's rows are removed at the commit");
		is(orphans(), 0, '... and nothing is left over');
	}

	fresh_x();
	q1(q{CREATE FUNCTION kingres.columnar_note_rewrite(rel regclass) RETURNS void
		AS '$libdir/kingres_columnar', 'kingres_columnar_note_rewrite' LANGUAGE C STRICT SECURITY DEFINER
		SET search_path = pg_catalog, kingres, pg_temp});
	script("BEGIN; SELECT kingres.columnar_note_rewrite('x'); INSERT INTO x VALUES (1, 1); COMMIT;");
	is(q1('SELECT count(*) FROM x'), 5001, 'a file noted and then not replaced (columnar_note_rewrite without a rewrite): the commit deletes nothing');
	cmp_ok(stripe_rows('x'), '>=', 5, '... the stripe rows are still there');
	script("BEGIN; SAVEPOINT a; SELECT kingres.columnar_note_rewrite('x'); RELEASE a; COMMIT;");
	is(q1('SELECT count(*) FROM x'), 5001, '... also when the note was made in a savepoint that was released');
	q1("DROP FUNCTION kingres.columnar_note_rewrite(regclass)");
}

# a stripe row that another session deleted and committed while the cleanup waited for it is
# gone, which is what the cleanup wanted: not an error
{
	fresh_x();
	my $uid = q1("SELECT kingres.columnar_identity('x')");
	my $a = $node->background_psql('postgres', on_error_stop => 1);
	$a->query_safe("BEGIN");
	$a->query_safe("DELETE FROM kingres.columnar_stripe WHERE rel_uid = $uid AND stripe_id = (SELECT min(stripe_id) FROM kingres.columnar_stripe WHERE rel_uid = $uid)");
	my $b = $node->background_psql('postgres', on_error_stop => 0);
	my $bpid = $b->query_safe("SELECT pg_backend_pid()");
	$b->query_until(qr/started/, "\\echo started\nALTER TABLE x SET ACCESS METHOD heap;\n");
	$node->poll_query_until('postgres', "SELECT wait_event_type = 'Lock' FROM pg_stat_activity WHERE pid = $bpid")
		or diag('session B did not start to wait');
	$a->query_safe("COMMIT");
	my $res = $b->query("SELECT 'AFTER=' || count(*) FROM x");
	like($res, qr/AFTER=5000/, 'a row deleted and committed by another session while the cleanup waited for it: the command finishes, the table has its rows');
	$b->quit;
	$a->quit;
	is(orphans(), 0, '... and nothing is left over');
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

# the last: dropping the extension drops the tables and the catalog in one command; the
# files the access method noted have no catalog to be cleaned from, which is not an error
{
	fresh_x();
	my ($out, $err) = script("DROP EXTENSION kingres_columnar CASCADE;");
	unlike($err, qr/ERROR/, 'DROP EXTENSION ... CASCADE with columnar tables: no error');
	is(q1("SELECT count(*) FROM pg_extension WHERE extname = 'kingres_columnar'"), 0, '... and the extension is gone');
	is(q1("SELECT count(*) FROM pg_class WHERE relname = 'x'"), 0, '... with its tables');
}

$node->stop;
done_testing();
