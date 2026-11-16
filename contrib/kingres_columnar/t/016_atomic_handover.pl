# contrib/kingres_columnar/t/016_atomic_handover.pl
#
# C4-01.  The rows of a subtransaction become rows of its parent for every
# relation or for none, and a transaction that fails while its buffers are
# written leaves nothing behind.  The failures are injected by the developer
# settings kingres_columnar.fault_point and kingres_columnar.fault_nth
# (assertion builds only; the file is skipped otherwise): the nth allocation
# of the write path, the nth relation of a hand-over, or the nth relation of
# a flush fails with "out of memory".  Every n is tried until the statement
# no longer fails, so each allocation and each relation in turn is the one
# that fails, whatever order the relations are visited in.
#
# For the point "alloc" the fault is armed by the last statement of the block
# whose end is the hand-over (arm_in_block), so that every allocation it counts
# is one of the hand-over: those of the INSERTs before it are not counted (C5-07:
# the failing allocation was often one of the INSERTs, and the case then said
# nothing about the hand-over).  A control arms the same fault where no
# subtransaction ends and sees that nothing fails.
#
# Cases: the commit of a subtransaction that wrote several relations (the
# defect: the relations merged before the failing one stayed in the parent
# and were committed); the same with the parent's buffer so full that the
# commit of the child writes stripes; the commit and the PREPARE of a
# transaction whose third relation fails to flush; the abort of a
# subtransaction with buffers in several relations while a cursor holds one
# of them.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colatomic');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
max_prepared_transactions = 10
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

my $has_fault = $node->safe_psql('postgres',
	"SELECT count(*) FROM pg_settings WHERE name = 'kingres_columnar.fault_point'");
if ($has_fault ne '1')
{
	plan skip_all => 'the fault injection is compiled only in assertion builds';
}

sub script
{
	my ($sql) = @_;
	my ($out, $err) = ('', '');

	$node->psql('postgres', $sql, stdout => \$out, stderr => \$err,
		on_error_stop => 0);
	return ($out, $err);
}

my @T = qw(a1 a2 a3 a4 a5);

sub fresh
{
	my ($stripe_rows) = @_;

	my $sql = '';
	foreach my $t (@T)
	{
		$sql .= "DROP TABLE IF EXISTS $t; CREATE TABLE $t (a int, t text) USING columnar;
			SELECT kingres.columnar_set_options('$t', stripe_row_count => $stripe_rows, chunk_row_count => 100);";
	}
	$node->safe_psql('postgres', $sql);
}

# SQL that arms the fault: at once, or (for the allocations of the hand-over
# alone) by the last statement of the block that is about to end
sub arm_before
{
	my ($point, $n) = @_;
	return "SET kingres_columnar.fault_point = '$point'; SET kingres_columnar.fault_nth = $n;";
}

sub arm_in_block
{
	my ($point, $n) = @_;
	return "PERFORM set_config('kingres_columnar.fault_point', '$point', false); PERFORM set_config('kingres_columnar.fault_nth', '$n', false);";
}

# "n1,n2,..." of the row counts, and of the distinct values
sub counts
{
	my $sql = join(" || ',' || ", map { "(SELECT count(*) FROM $_)" } @T);
	my $dsql = join(" || ',' || ", map { "(SELECT count(DISTINCT a) FROM $_)" } @T);
	my $c = $node->safe_psql('postgres', "SELECT $sql");
	my $d = $node->safe_psql('postgres', "SELECT $dsql");
	return ($c, $d);
}

sub check_clean
{
	my ($tag) = @_;
	my $bad = $node->safe_psql('postgres',
		"SELECT count(*) FROM kingres.columnar_check() WHERE check_name <> 'orphan_catalog_rows'");
	is($bad, 0, "$tag: columnar_check finds nothing wrong");
}

# ------------------------------------------------------------------------
# Part 1: the commit of a subtransaction that wrote five relations
# ------------------------------------------------------------------------

foreach my $point (qw(merge alloc))
{
	my $failed_at = 0;
	for (my $n = 1; $n <= 120; $n++)
	{
		fresh(100000);
		my $inserts = join(' ', map { "INSERT INTO $_ VALUES (1, repeat('v', 40));" } @T);
		my $in_block = $point eq 'alloc';
		my ($out, $err) = script("
BEGIN;
" . join(' ', map { "INSERT INTO $_ VALUES (0, repeat('v', 40));" } @T) . "
" . ($in_block ? '' : arm_before($point, $n)) . "
DO \$\$ BEGIN BEGIN $inserts " . ($in_block ? arm_in_block($point, $n) : '') . "
EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'CAUGHT: %', sqlerrm; END; END \$\$;
SET kingres_columnar.fault_point = 'off';
COMMIT;");
		my $caught = $err =~ /CAUGHT: out of memory/;
		my ($c, $d) = counts();
		if ($caught)
		{
			$failed_at++;
			is($c, '1,1,1,1,1', "$point #$n: the subtransaction failed, none of its rows is in any relation");
		}
		else
		{
			is($c, '2,2,2,2,2', "$point #$n: no failure, every row is there once");
			is($d, $c, "$point #$n: no value doubled");
			last;
		}
		is($d, $c, "$point #$n: no value doubled");
	}
	cmp_ok($failed_at, '>=', 5, "$point: the failure was injected in at least five places ($failed_at)");
	check_clean("hand-over, $point");
}

# the control: the fault armed the same way where no subtransaction ends counts
# no allocation, so the failures above were made by the hand-over
foreach my $n (1 .. 3)
{
	fresh(100000);
	my ($out, $err) = script("
BEGIN;
" . join(' ', map { "INSERT INTO $_ VALUES (0, repeat('v', 40));" } @T) . "
DO \$\$ BEGIN " . arm_in_block('alloc', $n) . " END \$\$;
SET kingres_columnar.fault_point = 'off';
COMMIT;");
	unlike($err, qr/out of memory/, "control #$n: no allocation is counted outside the hand-over");
	is((counts())[0], '1,1,1,1,1', "control #$n: the rows are committed");
}

# ------------------------------------------------------------------------
# Part 2: the parent's buffer is a stripe, so the commit of the child writes
# it: 999 rows of the parent in each of three relations, the child adds 3
# ------------------------------------------------------------------------

foreach my $point (qw(flush merge alloc))
{
	my $failed_at = 0;
	for (my $n = 1; $n <= 200; $n++)
	{
		fresh(1000);
		my $in_block = $point eq 'alloc';
		my ($out, $err) = script("
BEGIN;
" . join(' ', map { "INSERT INTO $_ SELECT g, 'v' FROM generate_series(1, 999) g;" } @T[0..2]) . "
" . ($in_block ? '' : arm_before($point, $n)) . "
DO \$\$ BEGIN BEGIN " . join(' ', map { "INSERT INTO $_ SELECT g, 'w' FROM generate_series(1000, 1002) g;" } @T[0..2]) .
	($in_block ? ' ' . arm_in_block($point, $n) : '') . "
EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'CAUGHT: %', sqlerrm; END; END \$\$;
SET kingres_columnar.fault_point = 'off';
COMMIT;");
		my $caught = $err =~ /CAUGHT: out of memory/;
		my ($c, $d) = counts();
		my $want = $caught ? '999,999,999,0,0' : '1002,1002,1002,0,0';
		is($c, $want, "full parent, $point #$n: " . ($caught ? 'child failed, parent rows intact' : 'child committed, all rows there'));
		is($d, $c, "full parent, $point #$n: no value lost or doubled");
		if ($caught)
		{
			$failed_at++;
		}
		else
		{
			last;
		}
	}
	cmp_ok($failed_at, '>=', 1, "full parent, $point: the failure was injected ($failed_at places)");
	check_clean("full parent, $point");
}

# ------------------------------------------------------------------------
# Part 3: commit and PREPARE of a transaction that wrote five relations,
# the nth relation fails to flush
# ------------------------------------------------------------------------

foreach my $end ('COMMIT', "PREPARE TRANSACTION 'kc016'")
{
	foreach my $point (qw(flush alloc))
	{
		my $failed_at = 0;
		for (my $n = 1; $n <= 150; $n++)
		{
			fresh(1000);
			my ($out, $err) = script("
BEGIN;
" . join(' ', map { "INSERT INTO $_ SELECT generate_series(1, 50);" } @T) . "
SET kingres_columnar.fault_point = '$point';
SET kingres_columnar.fault_nth = $n;
$end;");
			my $failed = $err =~ /out of memory/;
			$node->safe_psql('postgres', "COMMIT PREPARED 'kc016'")
				if !$failed && $end =~ /PREPARE/;
			my ($c, $d) = counts();
			my $stripes = $node->safe_psql('postgres',
				"SELECT count(*) FROM kingres.columnar_stripes('a1')");
			if ($failed)
			{
				$failed_at++;
				is($c, '0,0,0,0,0', "$end, $point #$n: the transaction failed, no relation has a row");
				is($stripes, 0, "$end, $point #$n: no stripe is visible");
				# the table works afterwards and the leftover pages are never read
				$node->safe_psql('postgres', 'INSERT INTO a1 VALUES (7)');
				is($node->safe_psql('postgres', 'SELECT count(*), min(a) FROM a1'), '1|7',
					"$end, $point #$n: the table takes new rows");
			}
			else
			{
				is($c, '50,50,50,50,50', "$end, $point #$n: no failure, all rows committed");
				last;
			}
		}
		cmp_ok($failed_at, '>=', 5, "$end, $point: failed in at least five places ($failed_at)");
		check_clean("$end, $point");
		is($node->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'), 0,
			"$end, $point: no prepared transaction left");
	}
}

# ------------------------------------------------------------------------
# Part 4: the abort of a subtransaction that has buffers in several
# relations while a cursor reads one of them, with the cursor opened in the
# subtransaction and in its parent
# ------------------------------------------------------------------------

{
	fresh(100000);
	my ($out, $err) = script("
BEGIN;
INSERT INTO a1 VALUES (0);
DECLARE outer_cur CURSOR FOR SELECT a FROM a1 ORDER BY a;
SAVEPOINT s;
" . join(' ', map { "INSERT INTO $_ SELECT generate_series(1, 20);" } @T) . "
DECLARE inner_cur CURSOR FOR SELECT a FROM a2 ORDER BY a;
FETCH 3 FROM inner_cur;
FETCH 1 FROM outer_cur;
ROLLBACK TO s;
INSERT INTO a1 VALUES (100);
FETCH ALL FROM outer_cur;
CLOSE outer_cur;
SELECT 'COUNTS=' || count(*) FROM a1;
COMMIT;");
	like($out, qr/COUNTS=2\b/, 'abort of a subtransaction with a cursor on one of five relations: the parent keeps its rows');
	is((counts())[0], '2,0,0,0,0', 'abort with cursors: no relation has a row of the aborted subtransaction');
	unlike($err, qr/ERROR/, 'abort with cursors: no error');
	check_clean('abort with cursors');
}

# a commit with a cursor open on the parent's buffer: the buffer is not
# merged or flushed under it, and the rows stay readable
{
	fresh(1000);
	my ($out, $err) = script("
BEGIN;
INSERT INTO a1 SELECT generate_series(1, 999);
DECLARE c CURSOR FOR SELECT a FROM a1;
FETCH 2 FROM c;
SAVEPOINT s;
INSERT INTO a1 SELECT generate_series(1000, 1100);
RELEASE s;
FETCH ALL FROM c;
SELECT 'N=' || count(*) FROM a1;
COMMIT;");
	like($out, qr/N=1100\b/, 'commit of a child under a cursor of the parent: all rows readable');
	is((counts())[0], '1100,0,0,0,0', 'commit of a child under a cursor of the parent: all rows committed');
	unlike($err, qr/ERROR/, 'commit of a child under a cursor of the parent: no error');
}

$node->stop;
done_testing();
