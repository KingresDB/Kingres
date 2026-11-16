# contrib/kingres_columnar/t/019_table_age.pl
#
# C4-04.  A table created after a snapshot is empty for that snapshot,
# whatever happens to its file afterwards.  The defect: the rule "the error
# 40001 is only for a file that replaced older content" looked at the last
# file, not at the table, so a table created after the snapshot and then
# truncated, vacuumed full, rewritten or moved to columnar gave the old
# snapshot a serialization failure where heap answers an empty table.
#
# The invariant: an error is for content that existed for the snapshot and is
# gone.  The table's creation (table_xid in the metapage) follows the content
# through every replacement of the file.  Every chain below is run against a
# heap twin and an older snapshot; and the opposite direction stays: content
# that existed before the snapshot and was replaced after it still fails.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('coltableage');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar; CREATE TABLE other (x int); INSERT INTO other VALUES (1)');

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

# An older snapshot is taken first (a REPEATABLE READ transaction that reads
# an unrelated table), then the commands run, each in its own transaction,
# then the older snapshot reads x.  Returns what it answered.
sub old_snapshot
{
	my ($am, $before, $commands, $read) = @_;

	foreach my $s ($before, @$commands, $read)
	{
		$s =~ s/\@AM\@/$am/g;
	}
	$node->safe_psql('postgres', 'DROP TABLE IF EXISTS x CASCADE; DROP TABLE IF EXISTS xp CASCADE');
	$node->safe_psql('postgres', $before) if $before ne '';
	my $s1 = $node->background_psql('postgres', on_error_stop => 0);
	run_in($s1, 'BEGIN ISOLATION LEVEL REPEATABLE READ');
	run_in($s1, 'SELECT count(*) FROM other');
	foreach my $c (@$commands)
	{
		$node->safe_psql('postgres', $c);
	}
	my $r = run_in($s1, $read);
	run_in($s1, 'ROLLBACK');
	$s1->quit;
	return $r;
}

my @chains = (
	['TRUNCATE after creation',
	 ['CREATE TABLE x (a int) USING @AM@', 'INSERT INTO x VALUES (1), (2)', 'TRUNCATE x', 'INSERT INTO x VALUES (3)']],
	['VACUUM FULL after creation',
	 ['CREATE TABLE x (a int) USING @AM@', 'INSERT INTO x VALUES (1), (2)', 'VACUUM FULL x', 'INSERT INTO x VALUES (3)']],
	['rewriting ALTER TABLE after creation',
	 ['CREATE TABLE x (a int) USING @AM@', 'INSERT INTO x VALUES (1), (2)',
	  'ALTER TABLE x ADD COLUMN b float DEFAULT random()', 'INSERT INTO x (a) VALUES (3)']],
	['type change after creation',
	 ['CREATE TABLE x (a int) USING @AM@', 'INSERT INTO x VALUES (1), (2)',
	  'ALTER TABLE x ALTER COLUMN a TYPE bigint', 'INSERT INTO x VALUES (3)']],
	['CREATE TABLE AS, then SET ACCESS METHOD',
	 ['CREATE TABLE x AS SELECT g AS a FROM generate_series(1, 5) g',
	  'ALTER TABLE x SET ACCESS METHOD @AM@', 'INSERT INTO x VALUES (3)']],
	['SELECT INTO, then SET ACCESS METHOD',
	 ['SELECT g AS a INTO x FROM generate_series(1, 5) g',
	  'ALTER TABLE x SET ACCESS METHOD @AM@', 'INSERT INTO x VALUES (3)']],
	['the whole chain: create, truncate, vacuum full, rewrite, heap, columnar again',
	 ['CREATE TABLE x (a int) USING @AM@', 'INSERT INTO x VALUES (1), (2)', 'TRUNCATE x',
	  'INSERT INTO x VALUES (3)', 'VACUUM FULL x', 'ALTER TABLE x ALTER COLUMN a TYPE bigint',
	  'ALTER TABLE x SET ACCESS METHOD heap', 'INSERT INTO x VALUES (4)',
	  'ALTER TABLE x SET ACCESS METHOD @AM@', 'TRUNCATE x', 'INSERT INTO x VALUES (5)']],
	['the chain in one transaction',
	 ['BEGIN; CREATE TABLE x (a int) USING @AM@; INSERT INTO x VALUES (1); TRUNCATE x; INSERT INTO x VALUES (2); ' .
	  'ALTER TABLE x ALTER COLUMN a TYPE bigint; TRUNCATE x; COMMIT']],
	['a partition made, filled and truncated',
	 ['CREATE TABLE xp (a int) PARTITION BY LIST (a)',
	  'CREATE TABLE x PARTITION OF xp FOR VALUES IN (1, 2, 3) USING @AM@',
	  'INSERT INTO xp VALUES (1), (2)', 'TRUNCATE x', 'INSERT INTO xp VALUES (3)']],
);

foreach my $ch (@chains)
{
	my ($name, $cmds) = @$ch;
	my $read = $name =~ /partition/ ? 'SELECT count(*) FROM xp' : 'SELECT count(*) FROM x';
	my $heap = old_snapshot('heap', '', [@$cmds], $read);
	my $col = old_snapshot('columnar', '', [@$cmds], $read);
	is($heap, '0', "$name: the heap twin answers an empty table");
	is($col, $heap, "$name: the older snapshot reads an empty table, not an error");
}

# the opposite direction: content that existed before the snapshot, replaced
# after it, still gives the error (the heap twin has nothing to compare: it
# answers empty, which would hide the rows the snapshot should see)
foreach my $cmd ('TRUNCATE x', 'VACUUM FULL x', 'ALTER TABLE x ADD COLUMN b float DEFAULT random()',
	'ALTER TABLE x SET ACCESS METHOD heap')
{
	my $col = old_snapshot('columnar', 'CREATE TABLE x (a int) USING columnar; INSERT INTO x VALUES (1), (2)',
		[$cmd, 'INSERT INTO x (a) VALUES (3)'], 'SELECT count(*) FROM x');
	if ($cmd =~ /VACUUM FULL/)
	{
		# every row is older than every snapshot: the rewrite keeps what each sees
		is($col, '2', "$cmd of content older than the snapshot: the snapshot's rows");
	}
	elsif ($cmd =~ /heap/)
	{
		# heap's own rewrite of the table: the rows are written by the rewriting
		# transaction, which the older snapshot cannot see, so heap answers an empty
		# table; any other answer (the rows of a later transaction, or an error from
		# the extension) would be wrong
		is($col, '0', "$cmd of content older than the snapshot: the table is heap's again and answers as heap does after a rewrite");
	}
	else
	{
		like($col, qr/could not read columnar table "x": it was rewritten or truncated/,
			"$cmd of content that existed for the snapshot: the serialization failure stays");
	}
}

# a table that existed for the snapshot and was created again under the name
{
	my $col = old_snapshot('columnar', 'CREATE TABLE x (a int) USING columnar; INSERT INTO x VALUES (1)',
		['DROP TABLE x', 'CREATE TABLE x (a int) USING columnar', 'INSERT INTO x VALUES (9)'],
		'SELECT count(*) FROM x');
	is($col, '0', 'a table dropped and created again: the old snapshot sees the new table empty');
}

$node->stop;
done_testing();
