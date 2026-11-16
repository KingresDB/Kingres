# contrib/kingres_columnar/t/021_table_creation.pl
#
# C5-02.  A snapshot that could have seen content of a table before the table's
# file was replaced never reads an empty table in place of that content: it
# reads the rows or gets the serialization failure.  The defect: a table that
# came from another access method took its creation (table_xid) from the
# transaction that last wrote its pg_class row, which any GRANT, new owner,
# storage option or ADD COLUMN moves forward, so a snapshot that had seen the
# rows read an empty table after such a command and the error after none.
#
# The rule now: a table's creation is recorded only when it is known for
# certain (made by the command or the transaction that fills it, or empty so
# that nothing can have been seen); otherwise it is unknown (0) and the reader
# asks the catalog whether the table existed for the snapshot, which no later
# command can change.  Heap itself answers an old snapshot with an empty table
# after its own rewrites; the extension is stricter on purpose for content it
# replaces, and every scenario below runs against a heap twin that shows what
# heap answers.
#
# Also here: the creating transactions of the metapage (table_xid and
# created_xid) are frozen by an ordinary VACUUM, a metapage that holds 0 for
# table_xid (a file of an older build) is read by the old rule, and the creation
# follows the content through every replacement of the file.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use Fcntl qw(:DEFAULT :seek);
use File::Path qw(make_path);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('coltabcreate');
$node->init(extra => ['--no-data-checksums']);
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
allow_in_place_tablespaces = on
});
$node->start;
$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE ROLE ktowner;
CREATE TABLE other (x int);
INSERT INTO other VALUES (1);
CREATE TABLESPACE kts LOCATION '';
});

sub run_in
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	my $out = $s->query($sql);
	my $err = $s->{stderr};
	$s->{stderr} = '';
	$out =~ s/^\s+|\s+$//g;
	$err =~ s/\s+/ /g;
	return $err ne '' ? "ERROR: $err" : $out;
}

# One scenario: $before runs and commits, then an older snapshot is taken (a
# REPEATABLE READ transaction that reads an unrelated table), then every
# command runs in a transaction of its own, then the older snapshot reads.
# "@CONVERT@" is the change of access method for the columnar run and a
# rewriting type change for the heap run (heap's own rewrite).
sub old_snapshot
{
	my ($am, $before, $commands, $read) = @_;

	my @cmds = map { s/\@CONVERT\@/$am eq 'heap' ? 'ALTER TABLE x ALTER COLUMN a TYPE bigint' : 'ALTER TABLE x SET ACCESS METHOD columnar'/ger } @$commands;
	$before =~ s/\@AM\@/$am/g;
	$node->safe_psql('postgres', 'DROP TABLE IF EXISTS x CASCADE; DROP TABLE IF EXISTS xp CASCADE; DROP MATERIALIZED VIEW IF EXISTS mv CASCADE');
	$node->safe_psql('postgres', $before) if $before ne '';
	my $s1 = $node->background_psql('postgres', on_error_stop => 0);
	run_in($s1, 'BEGIN ISOLATION LEVEL REPEATABLE READ');
	run_in($s1, 'SELECT count(*) FROM other');
	foreach my $c (@cmds)
	{
		$c =~ s/\@AM\@/$am/g;
		$node->safe_psql('postgres', $c);
	}
	my $r = run_in($s1, $read);
	run_in($s1, 'ROLLBACK');
	$s1->quit;
	return $r;
}

my $ROWS = 'CREATE TABLE x (a int); INSERT INTO x VALUES (1), (2), (3)';
my $ERR = qr/could not read columnar table "x": it was rewritten or truncated after this snapshot was taken/;

# ------------------------------------------------------------------------
# 1. The table existed for the snapshot, with rows: whatever happened to its
#    pg_class row since, the conversion gives the error and never an empty
#    table (the defect gave 0 after each command of the first group).
# ------------------------------------------------------------------------
foreach my $u ('SELECT 1', 'GRANT SELECT ON x TO PUBLIC', 'ALTER TABLE x OWNER TO ktowner',
	'ALTER TABLE x SET (fillfactor = 60)', 'ALTER TABLE x SET (autovacuum_enabled = false)',
	'ALTER TABLE x ADD COLUMN c int', 'ALTER TABLE x ENABLE ROW LEVEL SECURITY',
	'ALTER TABLE x REPLICA IDENTITY FULL', 'ANALYZE x', 'VACUUM x',
	"COMMENT ON TABLE x IS 'c'", 'ALTER TABLE x ALTER COLUMN a SET STATISTICS 100')
{
	my $heap = old_snapshot('heap', $ROWS, [$u, '@CONVERT@'], 'SELECT count(*) FROM x');
	my $col = old_snapshot('columnar', $ROWS, [$u, '@CONVERT@'], 'SELECT count(*) FROM x');
	is($heap, '0', "heap twin after '$u' and a rewrite: heap answers an empty table (its own rewrite)");
	like($col, $ERR, "rows existed for the snapshot, '$u', then the conversion: the error, not an empty table");
}

# every way of making the content, then converting it
foreach my $case (
	['CREATE TABLE AS', 'CREATE TABLE x AS SELECT g AS a FROM generate_series(1, 3) g'],
	['SELECT INTO', 'SELECT g AS a INTO x FROM generate_series(1, 3) g'],
	['a table filled with many rows', 'CREATE TABLE x (a int); INSERT INTO x SELECT generate_series(1, 3000)'])
{
	my ($name, $before) = @$case;
	my $col = old_snapshot('columnar', $before, ['@CONVERT@'], 'SELECT count(*) FROM x');
	like($col, $ERR, "$name before the snapshot, converted after it: the error");
}

# the chain heap, columnar, heap, columnar, every step after the snapshot
{
	my $col = old_snapshot('columnar', $ROWS,
		['ALTER TABLE x SET ACCESS METHOD columnar', 'ALTER TABLE x SET ACCESS METHOD heap',
		 'ALTER TABLE x SET ACCESS METHOD columnar', 'GRANT SELECT ON x TO PUBLIC',
		 'ALTER TABLE x SET ACCESS METHOD heap', 'ALTER TABLE x SET ACCESS METHOD columnar'],
		'SELECT count(*) FROM x');
	like($col, $ERR, 'chain heap, columnar, heap, columnar, heap, columnar: rows existed, the error');
}

# the replacements of the file of a table that came from heap keep the answer
foreach my $then ('TRUNCATE x', 'VACUUM FULL x', 'ALTER TABLE x ADD COLUMN b float DEFAULT random()',
	'ALTER TABLE x ALTER COLUMN a TYPE bigint', 'ALTER TABLE x SET ACCESS METHOD heap')
{
	my $col = old_snapshot('columnar', $ROWS, ['@CONVERT@', 'GRANT SELECT ON x TO PUBLIC', $then, 'INSERT INTO x (a) VALUES (9)'],
		'SELECT count(*) FROM x');
	if ($then =~ /SET ACCESS METHOD heap/)
	{
		# heap's own rewrite of the table: an empty table, as heap answers
		is($col, '0', "converted, then '$then': the table is heap again and answers as heap does after its rewrite");
	}
	else
	{
		like($col, $ERR, "converted, then '$then': rows existed for the snapshot, the error");
	}
}

# a materialized view refreshed after the snapshot
{
	my $before = 'CREATE MATERIALIZED VIEW mv AS SELECT g AS a FROM generate_series(1, 3) g';
	my $heap = old_snapshot('heap', $before, ['REFRESH MATERIALIZED VIEW mv'], 'SELECT count(*) FROM mv');
	my $col = old_snapshot('columnar', 'CREATE MATERIALIZED VIEW mv USING columnar AS SELECT g AS a FROM generate_series(1, 3) g',
		['REFRESH MATERIALIZED VIEW mv'], 'SELECT count(*) FROM mv');
	is($heap, '3', 'heap twin: the refresh writes frozen rows, so the older snapshot reads the new contents');
	like($col, qr/could not read columnar table "mv"/, 'a columnar materialized view refreshed after the snapshot: the error, rows existed');
	$col = old_snapshot('columnar', '', ['CREATE MATERIALIZED VIEW mv USING columnar AS SELECT g AS a FROM generate_series(1, 3) g',
		'REFRESH MATERIALIZED VIEW mv'], 'SELECT count(*) FROM mv');
	is($col, '0', 'a columnar materialized view made and refreshed after the snapshot: empty');
}

# SET TABLESPACE copies the file: the snapshot still reads the rows, as heap's
{
	my $heap = old_snapshot('heap', 'CREATE TABLE x (a int) USING @AM@; INSERT INTO x VALUES (1), (2), (3)',
		['ALTER TABLE x SET TABLESPACE kts'], 'SELECT count(*) FROM x');
	my $col = old_snapshot('columnar', 'CREATE TABLE x (a int) USING @AM@; INSERT INTO x VALUES (1), (2), (3)',
		['ALTER TABLE x SET TABLESPACE kts'], 'SELECT count(*) FROM x');
	is($heap, '3', 'heap twin: SET TABLESPACE keeps the rows of an older snapshot');
	is($col, $heap, 'columnar: SET TABLESPACE keeps the rows of an older snapshot');
}

# ------------------------------------------------------------------------
# 2. Nothing existed for the snapshot, or provably nothing it could see: the
#    empty table is the right answer, whatever commands came in between.
# ------------------------------------------------------------------------
my @empty = (
	['the table is made after the snapshot, filled, granted, converted',
	 '', ['CREATE TABLE x (a int)', 'INSERT INTO x VALUES (1), (2), (3)', 'GRANT SELECT ON x TO PUBLIC', 'ALTER TABLE x OWNER TO ktowner', '@CONVERT@']],
	['CREATE TABLE AS after the snapshot, converted',
	 '', ['CREATE TABLE x AS SELECT g AS a FROM generate_series(1, 3) g', '@CONVERT@', 'INSERT INTO x VALUES (4)']],
	['SELECT INTO after the snapshot, converted',
	 '', ['SELECT g AS a INTO x FROM generate_series(1, 3) g', 'ALTER TABLE x ADD COLUMN c int', '@CONVERT@']],
	['created and converted in one transaction',
	 '', ['BEGIN; CREATE TABLE x (a int); INSERT INTO x VALUES (1), (2); ALTER TABLE x SET ACCESS METHOD columnar; COMMIT']],
	['created, converted and rewritten in one transaction',
	 '', ['BEGIN; CREATE TABLE x (a int); INSERT INTO x VALUES (1), (2); ALTER TABLE x SET ACCESS METHOD columnar; ALTER TABLE x ALTER COLUMN a TYPE bigint; TRUNCATE x; INSERT INTO x VALUES (3); COMMIT']],
	['created in a subtransaction and converted',
	 '', ['BEGIN; SAVEPOINT s; CREATE TABLE x (a int); INSERT INTO x VALUES (1); RELEASE s; ALTER TABLE x SET ACCESS METHOD columnar; COMMIT']],
	['an empty table that existed before the snapshot, converted',
	 'CREATE TABLE x (a int)', ['@CONVERT@', 'INSERT INTO x VALUES (1)']],
	['rows deleted and vacuumed away before the snapshot, converted',
	 'CREATE TABLE x (a int); INSERT INTO x VALUES (1), (2); DELETE FROM x; VACUUM x', ['@CONVERT@', 'INSERT INTO x VALUES (1)']],
	['the heap table truncated after the snapshot (heap says empty), converted',
	 $ROWS, ['TRUNCATE x', '@CONVERT@']],
	['CREATE TABLE ... USING after the snapshot',
	 '', ['CREATE TABLE x (a int) USING @AM@', 'INSERT INTO x VALUES (1), (2)']],
	['CREATE TABLE AS ... USING after the snapshot',
	 '', ['CREATE TABLE x USING @AM@ AS SELECT g AS a FROM generate_series(1, 3) g']],
	['a table dropped and made again after the snapshot, converted',
	 $ROWS, ['DROP TABLE x', 'CREATE TABLE x (a int)', 'INSERT INTO x VALUES (1)', '@CONVERT@']],
	['a partition made after the snapshot, converted, filled, truncated',
	 '', ['CREATE TABLE xp (a bigint) PARTITION BY LIST (a)', 'CREATE TABLE x (a bigint)', 'INSERT INTO x VALUES (1)',
	      '@CONVERT@', 'ALTER TABLE xp ATTACH PARTITION x FOR VALUES IN (1, 2, 3)', 'INSERT INTO xp VALUES (2)', 'TRUNCATE x']],
);
foreach my $e (@empty)
{
	my ($name, $before, $cmds) = @$e;
	my $read = $name =~ /partition/ ? 'SELECT count(*) FROM xp' : 'SELECT count(*) FROM x';
	my $heap = old_snapshot('heap', $before, [@$cmds], $read);
	my $col = old_snapshot('columnar', $before, [@$cmds], $read);
	is($heap, '0', "$name: the heap twin answers an empty table");
	is($col, '0', "$name: the older snapshot reads an empty table");
}

# ------------------------------------------------------------------------
# 3. The creation recorded in the metapage, source by source
# ------------------------------------------------------------------------
sub xids
{
	my ($t) = @_;
	return $node->safe_psql('postgres', "SELECT kingres.columnar_table_xid('$t')::text || ',' || kingres.columnar_creating_xid('$t')::text");
}

sub is_normal { return $_[0] > 2 }

{
	$node->safe_psql('postgres', 'DROP TABLE IF EXISTS c1, c2, c3, c4, c5');
	# made by CREATE: this transaction
	$node->safe_psql('postgres', 'CREATE TABLE c1 (a int) USING columnar; INSERT INTO c1 VALUES (1)');
	my ($t, $c) = split /,/, xids('c1');
	ok(is_normal($t) && $t == $c, "CREATE TABLE: the creation of the table is the creating transaction ($t, $c)");

	# every replacement of the file carries the creation of the table
	foreach my $cmd ('TRUNCATE c1', 'VACUUM FULL c1', 'ALTER TABLE c1 ALTER COLUMN a TYPE bigint',
		'ALTER TABLE c1 ADD COLUMN b float DEFAULT random()', 'ALTER TABLE c1 SET TABLESPACE kts')
	{
		my ($t0) = split /,/, xids('c1');
		$node->safe_psql('postgres', $cmd);
		my ($t1, $c1) = split /,/, xids('c1');
		is($t1, $t0, "after '$cmd' the creation of the table is the same ($t0)");
		cmp_ok($c1, '>', 2, "after '$cmd' the file has a creating transaction of its own") unless $cmd =~ /TABLESPACE|VACUUM FULL/;
	}

	# CREATE TABLE AS ... USING columnar
	$node->safe_psql('postgres', 'CREATE TABLE c2 USING columnar AS SELECT g AS a FROM generate_series(1, 5) g');
	($t, $c) = split /,/, xids('c2');
	ok(is_normal($t) && $t == $c, "CREATE TABLE AS USING columnar: the creating transaction ($t, $c)");

	# a heap table converted: unknown (0) when it holds rows from an earlier transaction ...
	$node->safe_psql('postgres', 'CREATE TABLE c3 (a int); INSERT INTO c3 VALUES (1)');
	$node->safe_psql('postgres', 'ALTER TABLE c3 SET ACCESS METHOD columnar');
	($t, $c) = split /,/, xids('c3');
	is($t, 0, 'a heap table with rows converted: the creation is not known (0), the catalog tells what a snapshot saw');
	ok(is_normal($c), 'the file itself has a creating transaction');
	# ... and the same after any chain of replacements, until the table is empty and made anew
	$node->safe_psql('postgres', 'VACUUM FULL c3; ALTER TABLE c3 ALTER COLUMN a TYPE bigint; INSERT INTO c3 VALUES (2)');
	is((split /,/, xids('c3'))[0], 0, 'the unknown creation follows the content through VACUUM FULL and a rewrite');
	$node->safe_psql('postgres', 'TRUNCATE c3; INSERT INTO c3 VALUES (3)');
	is((split /,/, xids('c3'))[0], 0, 'and through TRUNCATE');
	$node->safe_psql('postgres', 'ALTER TABLE c3 SET ACCESS METHOD heap; ALTER TABLE c3 SET ACCESS METHOD columnar');
	is((split /,/, xids('c3'))[0], 0, 'heap and back again, with rows: still unknown');

	# ... an empty heap table, or one made in the same transaction: known
	$node->safe_psql('postgres', 'CREATE TABLE c4 (a int); ALTER TABLE c4 SET ACCESS METHOD columnar');
	($t, $c) = split /,/, xids('c4');
	ok(is_normal($t) && $t == $c, "an empty heap table converted: nothing could have been seen, creation = the conversion ($t, $c)");
	$node->safe_psql('postgres', 'BEGIN; CREATE TABLE c5 (a int); INSERT INTO c5 VALUES (1); ALTER TABLE c5 SET ACCESS METHOD columnar; COMMIT');
	($t, $c) = split /,/, xids('c5');
	ok(is_normal($t) && $t == $c, "a heap table made in the same transaction converted: creation = this transaction ($t, $c)");
}

# VACUUM freezes both ids of the metapage, so that neither can name another
# transaction after a lap of the id space.  An ordinary check: no timeout, no
# optional module.  A creation of 0 stays 0.
{
	$node->safe_psql('postgres', 'DROP TABLE IF EXISTS fz1, fz2');
	$node->safe_psql('postgres', 'CREATE TABLE fz1 (a int) USING columnar; INSERT INTO fz1 VALUES (1)');
	$node->safe_psql('postgres', 'CREATE TABLE fz2 (a int); INSERT INTO fz2 VALUES (1)');
	$node->safe_psql('postgres', 'ALTER TABLE fz2 SET ACCESS METHOD columnar');
	$node->safe_psql('postgres', 'SELECT txid_current()');
	$node->safe_psql('postgres', 'ALTER TABLE fz1 ALTER COLUMN a TYPE bigint');	# created_xid differs from table_xid
	my ($t, $c) = split /,/, xids('fz1');
	ok(is_normal($t) && is_normal($c) && $t != $c, "before VACUUM the metapage holds two transaction ids ($t, $c)");
	$node->safe_psql('postgres', 'SELECT txid_current()');
	$node->safe_psql('postgres', 'VACUUM fz1; VACUUM fz2');
	is(xids('fz1'), '2,2', 'VACUUM froze the creation of the table and of the file');
	is(xids('fz2'), '0,2', 'VACUUM froze the file; an unknown creation stays unknown');
	is($node->safe_psql('postgres', "SELECT count(*) FROM fz1"), 1, 'the frozen table reads as before');
	# relfrozenxid is not held back by the ids of the metapage once they are frozen
	cmp_ok($node->safe_psql('postgres', "SELECT age(relfrozenxid) FROM pg_class WHERE relname = 'fz1'"),
		'<', 20, 'relfrozenxid of the table moved on with the freeze');
}

# ------------------------------------------------------------------------
# 4. A metapage that holds 0 for table_xid (a file of an older build) is read
#    by the old rule: a snapshot that cannot see the file's creation and saw the
#    table gets the error, one that can see it reads the rows.  The file is made
#    by editing the metapage of a stopped cluster (its creating transaction is
#    set to one that is newer than the snapshot taken after the restart).
# ------------------------------------------------------------------------
{
	my $page = 8192;
	my @tab;
	for my $i (0 .. 255)
	{
		my $c = $i;
		for (1 .. 8) { $c = ($c & 1) ? (($c >> 1) ^ 0x82F63B78) : ($c >> 1); }
		$tab[$i] = $c;
	}
	my $crc32c = sub {
		my ($data) = @_;
		my $crc = 0xFFFFFFFF;
		$crc = $tab[($crc ^ $_) & 0xFF] ^ ($crc >> 8) for unpack('C*', $data);
		return ($crc ^ 0xFFFFFFFF) & 0xFFFFFFFF;
	};

	$node->safe_psql('postgres', 'DROP TABLE IF EXISTS zx; CREATE TABLE zx (a int) USING columnar; INSERT INTO zx VALUES (1), (2), (3)');
	my $path = $node->data_dir . '/' . $node->safe_psql('postgres', "SELECT pg_relation_filepath('zx')");
	my $cx = $node->safe_psql('postgres', 'SELECT txid_current()') + 6;
	$node->stop;

	sysopen(my $fh, $path, O_RDWR) or die "open $path: $!";
	sysseek($fh, 0, SEEK_SET);
	my $buf = '';
	sysread($fh, $buf, $page) == $page or die "short read";
	my $meta_off = 24;						# SizeOfPageHeaderData
	my $meta = substr($buf, $meta_off, 104);	# sizeof(KcMetaData)
	substr($meta, 48, 4) = pack('V', $cx);		# created_xid: newer than any snapshot taken now
	substr($meta, 56, 4) = pack('V', 1);		# flags: KC_META_REPLACES_CONTENT, so not the first file
	substr($meta, 64, 4) = pack('V', 0);		# table_xid: 0, an older file
	substr($meta, 52, 4) = pack('V', 0);
	substr($meta, 52, 4) = pack('V', $crc32c->($meta));
	substr($buf, $meta_off, 104) = $meta;
	sysseek($fh, 0, SEEK_SET);
	syswrite($fh, $buf) == $page or die "short write";
	close($fh);
	$node->start;

	is($node->safe_psql('postgres', 'SELECT kingres.columnar_table_xid(\'zx\')::text'), '0', 'the edited metapage holds 0 for table_xid');
	my $s1 = $node->background_psql('postgres', on_error_stop => 0);
	run_in($s1, 'BEGIN ISOLATION LEVEL REPEATABLE READ');
	run_in($s1, 'SELECT count(*) FROM other');
	$node->safe_psql('postgres', 'SELECT txid_current()') for 1 .. 9;	# the file's creating id is now a past one, newer than the snapshot
	like(run_in($s1, 'SELECT count(*) FROM zx'), qr/could not read columnar table "zx": it was rewritten or truncated/,
		'table_xid 0: a snapshot that saw the table and cannot see the file gets the error (the old rule)');
	run_in($s1, 'ROLLBACK');
	$s1->quit;
	is($node->safe_psql('postgres', 'SELECT count(*) FROM zx'), '3', 'a snapshot taken now sees the file and reads the rows');
}

$node->stop;
done_testing();
