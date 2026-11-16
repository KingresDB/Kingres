# contrib/kingres_columnar/t/015_creation_compaction.pl
#
# Two things that must not make a reader fail or a dependency appear where
# nothing changed or nothing older existed.
#
# Part 1 (C3-05): a snapshot older than the creation of a columnar table is
# handed an empty table, as heap does (the table's rows are newer than the
# snapshot).  This covers a new partition, CREATE TABLE AS, ATTACH PARTITION
# of a table created after the snapshot, and a table dropped and created
# again under the same name.  Where the file replaced older content (TRUNCATE,
# a rewriting ALTER TABLE) the serialization failure stays.
#
# Part 2 (C3-04): columnar_compact changes nothing logically.  Under
# SERIALIZABLE it must not abort an unrelated reader, neither through the
# rows it copies nor through the stripes it creates, and it must not hide the
# dependency of a real write made by the same transaction.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colcreate');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar; CREATE TABLE other (x int)');

sub run_in
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	my $out = $s->query($sql);
	my $err = $s->{stderr};
	$s->{stderr} = '';
	$out =~ s/^\s+|\s+$//g;
	return $err ne '' ? "ERROR: $err" : $out;
}

# ------------------------------------------------------------------------
# Part 1: tables created after the snapshot
# ------------------------------------------------------------------------

# The old snapshot is taken first; then the command runs; then the old
# snapshot reads.  Returns what the read answered.
sub old_snapshot
{
	my ($am, $setup, $command, $read) = @_;

	foreach my $s ($setup, $command, $read)
	{
		$s =~ s/\@AM\@/$am/g;
	}
	$node->safe_psql('postgres', "
		DROP TABLE IF EXISTS pt, pt1, pt2, ctas, x, hc, newp, base CASCADE;
		$setup");
	my $s1 = $node->background_psql('postgres', on_error_stop => 0);
	run_in($s1, 'BEGIN ISOLATION LEVEL REPEATABLE READ');
	run_in($s1, 'SELECT count(*) FROM other');
	$node->safe_psql('postgres', $command);
	my $out = run_in($s1, $read);
	run_in($s1, 'ROLLBACK');
	$s1->quit;
	return $out;
}

sub same_as_heap
{
	my ($name, $setup, $command, $read, $expect) = @_;
	my $heap = old_snapshot('heap', $setup, $command, $read);
	my $col = old_snapshot('columnar', $setup, $command, $read);

	is($heap, $expect, "$name: heap answers $expect");
	is($col, $heap, "$name: columnar answers what heap answers");
}

my $PART = q{
CREATE TABLE pt (d date, v int) PARTITION BY RANGE (d);
CREATE TABLE pt1 PARTITION OF pt FOR VALUES FROM ('2026-01-01') TO ('2026-02-01') USING @AM@;
INSERT INTO pt VALUES ('2026-01-05', 1);};

same_as_heap('new partition created and filled', $PART,
	q{CREATE TABLE pt2 PARTITION OF pt FOR VALUES FROM ('2026-02-01') TO ('2026-03-01') USING @AM@;
	  INSERT INTO pt VALUES ('2026-02-05', 2), ('2026-02-06', 3);},
	'SELECT count(*) FROM pt', 1);

same_as_heap('new partition read directly', $PART,
	q{CREATE TABLE pt2 PARTITION OF pt FOR VALUES FROM ('2026-02-01') TO ('2026-03-01') USING @AM@;
	  INSERT INTO pt VALUES ('2026-02-05', 2);},
	'SELECT count(*) FROM pt2', 0);

same_as_heap('CREATE TABLE AS', 'CREATE TABLE base (a int); INSERT INTO base VALUES (1), (2);',
	'CREATE TABLE ctas USING @AM@ AS SELECT * FROM base',
	'SELECT count(*) FROM ctas', 0);

same_as_heap('ATTACH PARTITION of a table created after the snapshot', $PART,
	q{CREATE TABLE newp (d date, v int) USING @AM@;
	  INSERT INTO newp VALUES ('2026-02-10', 9);
	  ALTER TABLE pt ATTACH PARTITION newp FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');},
	'SELECT count(*) FROM pt', 1);

same_as_heap('a table dropped and created again under the same name',
	'CREATE TABLE x (a int) USING @AM@; INSERT INTO x VALUES (1), (2), (3);',
	'DROP TABLE x; CREATE TABLE x (a int) USING @AM@; INSERT INTO x VALUES (10);',
	'SELECT count(*) FROM x', 0);

same_as_heap('a table created, filled and filled again in more transactions',
	'CREATE TABLE base (a int);',
	'CREATE TABLE x (a int) USING @AM@; INSERT INTO x SELECT generate_series(1, 5000); INSERT INTO x VALUES (1);',
	'SELECT count(*) FROM x', 0);

# Where the file replaced older content the serialization failure stays.
foreach my $case (
	['TRUNCATE', 'CREATE TABLE x (a int) USING columnar; INSERT INTO x VALUES (1), (2)',
	 'TRUNCATE x'],
	['SET ACCESS METHOD from heap', 'CREATE TABLE x (a int) USING heap; INSERT INTO x VALUES (1), (2)',
	 'ALTER TABLE x SET ACCESS METHOD columnar'],
	['a rewriting ALTER COLUMN TYPE', 'CREATE TABLE x (a int) USING columnar; INSERT INTO x VALUES (1), (2)',
	 'ALTER TABLE x ALTER COLUMN a TYPE bigint'],
	['TRUNCATE of a table created in the same transaction as its first rows',
	 'CREATE TABLE x (a int) USING columnar; INSERT INTO x VALUES (1), (2)',
	 'BEGIN; INSERT INTO x VALUES (3); TRUNCATE x; INSERT INTO x VALUES (4); COMMIT'])
{
	my ($name, $setup, $command) = @$case;
	my $out = old_snapshot('columnar', $setup, $command, 'SELECT count(*) FROM x');
	like($out, qr/rewritten or truncated after this snapshot/,
		"$name: an older snapshot still gets the serialization failure");
}

# ------------------------------------------------------------------------
# Part 2: compaction under SERIALIZABLE
# ------------------------------------------------------------------------

sub fresh_tables
{
	my ($akind, $bkind) = @_;

	$node->safe_psql('postgres', "
		DROP TABLE IF EXISTS ca, cb;
		CREATE TABLE ca (v int) USING $akind;
		CREATE TABLE cb (v int) USING $bkind;
		INSERT INTO ca VALUES (1);
		INSERT INTO ca VALUES (2);
		INSERT INTO ca VALUES (3);
		INSERT INTO cb VALUES (0);");
}

my $SER = 'BEGIN ISOLATION LEVEL SERIALIZABLE';

foreach my $bkind (qw(heap columnar))
{
	# The case of the review: an application transaction that reads ca and
	# writes cb, another that reads cb, and a compaction of ca by a third.
	foreach my $reread (0, 1)
	{
		fresh_tables('columnar', $bkind);
		my $s1 = $node->background_psql('postgres', on_error_stop => 0);
		my $s2 = $node->background_psql('postgres', on_error_stop => 0);
		my $s3 = $node->background_psql('postgres', on_error_stop => 0);
		my $tag = "compaction by a third transaction, b is $bkind, reader " . ($reread ? 're-reads' : 'reads once');

		run_in($s1, $SER);
		is(run_in($s1, 'SELECT count(*) FROM ca'), 3, "$tag: s1 reads ca");
		run_in($s2, $SER);
		run_in($s2, 'SELECT count(*) FROM cb');
		run_in($s1, 'INSERT INTO cb VALUES (1)');
		run_in($s3, $SER);
		is(run_in($s3, "SELECT kingres.columnar_compact('ca', 100)"), 1, "$tag: the three stripes become one");
		unlike(run_in($s3, 'COMMIT'), qr/ERROR/, "$tag: the compaction commits");
		is(run_in($s1, 'SELECT count(*) FROM ca'), 3, "$tag: s1 still reads three rows") if $reread;
		unlike(run_in($s1, 'COMMIT'), qr/ERROR/, "$tag: s1 commits");
		unlike(run_in($s2, 'COMMIT'), qr/ERROR/, "$tag: s2 commits");
		is($node->safe_psql('postgres', 'SELECT count(*) FROM ca'), 3, "$tag: no row lost");
		is($node->safe_psql('postgres', "SELECT count(*) FROM kingres.columnar_stripes('ca')"), 1,
			"$tag: one stripe now");
		$_->quit foreach ($s1, $s2, $s3);
	}
}

# default_transaction_isolation = serializable for the compacting session
{
	fresh_tables('columnar', 'heap');
	my $s1 = $node->background_psql('postgres', on_error_stop => 0);
	my $s2 = $node->background_psql('postgres', on_error_stop => 0);
	run_in($s1, $SER);
	run_in($s1, 'SELECT count(*) FROM ca');
	run_in($s2, $SER);
	run_in($s2, 'SELECT count(*) FROM cb');
	run_in($s1, 'INSERT INTO cb VALUES (1)');
	$node->safe_psql('postgres', "
		ALTER ROLE CURRENT_USER SET default_transaction_isolation = 'serializable';");
	is($node->safe_psql('postgres', "SELECT kingres.columnar_compact('ca', 100)"), 1,
		'compaction with the serializable default');
	$node->safe_psql('postgres', 'ALTER ROLE CURRENT_USER RESET default_transaction_isolation');
	unlike(run_in($s1, 'COMMIT'), qr/ERROR/, 'reader commits after a compaction with the serializable default');
	unlike(run_in($s2, 'COMMIT'), qr/ERROR/, 'second transaction commits too');
	$_->quit foreach ($s1, $s2);
}

# A real write by the compacting transaction keeps its dependencies.  The
# same transaction writes ca (a row), compacts it, and reads cb; another reads
# ca and writes cb: write skew, which must abort one of them, as with heap
# twins where the compaction is a no-op.
sub skew_with_compaction
{
	my ($akind, $compact) = @_;

	fresh_tables($akind, 'heap');
	my $s1 = $node->background_psql('postgres', on_error_stop => 0);
	my $s2 = $node->background_psql('postgres', on_error_stop => 0);
	my %res;

	# the writer's row is still buffered when the reader reads: only the check
	# at the flush finds the reader's lock, whatever compaction added to the buffer
	run_in($s1, $SER); run_in($s1, 'SELECT 1');
	run_in($s2, $SER); run_in($s2, 'SELECT 1');
	run_in($s2, 'INSERT INTO ca VALUES (4)');
	run_in($s1, 'SELECT count(*) FROM ca');
	run_in($s2, $compact ? "SELECT kingres.columnar_compact('ca', 100)" : 'SELECT 1');
	run_in($s2, 'SELECT count(*) FROM cb');
	run_in($s1, 'INSERT INTO cb VALUES (1)');
	$res{1} = run_in($s1, 'COMMIT') =~ /ERROR/ ? 'A' : 'C';
	$res{2} = run_in($s2, 'COMMIT') =~ /ERROR/ ? 'A' : 'C';
	$_->quit foreach ($s1, $s2);
	return "1=$res{1} 2=$res{2}";
}

my $heap_skew = skew_with_compaction('heap', 0);
like($heap_skew, qr/=A/, "write skew with a heap twin aborts one transaction ($heap_skew)");
is(skew_with_compaction('columnar', 0), $heap_skew, 'columnar without compaction: the heap outcome');
is(skew_with_compaction('columnar', 1), $heap_skew,
	'columnar with a compaction by the writing transaction: the heap outcome');

# the stripe catalog marks what compaction wrote
{
	fresh_tables('columnar', 'heap');
	$node->safe_psql('postgres', "SELECT kingres.columnar_compact('ca', 100)");
	is($node->safe_psql('postgres', "
		SELECT count(*) FILTER (WHERE maintenance) || '/' || count(*)
		  FROM kingres.columnar_stripe WHERE rel_uid = kingres.columnar_identity('ca')"),
		'1/1', 'the compacted stripe is marked as maintenance');
}

$node->stop;
done_testing();
