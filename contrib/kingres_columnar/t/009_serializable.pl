# contrib/kingres_columnar/t/009_serializable.pl
#
# SERIALIZABLE isolation against heap twins.
#
# A columnar table takes part in serializable snapshot isolation by what it
# does in three places: a scan takes a relation-level predicate lock before it
# reads the stripe catalog; a write checks for conflicts when a row is
# buffered and again when the buffer becomes a stripe (a reader whose lock came
# in between is found then); a scan that skips a stripe because its writer had
# not committed before the snapshot reports the dependency, as a heap scan
# does for a tuple it skips.
#
# Every scenario below is run against tables that are heap, columnar, and
# mixed, with one statement at a time in a fixed order, and the set of
# transactions that commit is compared with the heap-only run: it must be the
# same set (the error texts are not compared, the serialization failure may
# be raised by another statement of the same transaction).  The scenarios
# vary where the reader starts relative to the writer's insert (before, while
# the rows are buffered or already flushed by size, after the writer's
# commit), how the writer's rows get into a stripe (a flush at commit, a flush
# forced by the stripe size), subtransactions, PREPARE TRANSACTION, the order
# of the commits, and three transactions (the read-only anomaly).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colssi');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
max_prepared_transactions = 10
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

my $unexpected = 0;

# Run the steps of one scenario on tables of the given kinds; returns the
# outcome of every transaction, "1=C 2=A ...", C committed, A aborted.
sub run_scenario
{
	my ($kinds, $steps) = @_;
	my @am = map { $_ eq 'col' ? 'USING columnar' : 'USING heap' } @$kinds;

	$node->safe_psql('postgres', "
		DROP TABLE IF EXISTS t_a, t_b;
		CREATE TABLE t_a (v int) $am[0];
		CREATE TABLE t_b (v int) " . ($am[1] // $am[0]) . ";
		INSERT INTO t_a VALUES (0);
		INSERT INTO t_b VALUES (0);");

	my %sess;
	my (%failed, %seen);
	foreach my $st (@$steps)
	{
		my ($txn, $conn, $sql) = @$st;

		$seen{$txn} = 1;
		next if $failed{$txn};
		$sql =~ s/\@A\@/t_a/g;
		$sql =~ s/\@B\@/t_b/g;
		my $err = '';
		if ($conn eq 'adm')
		{
			my ($rc, $out, $e) = ('', '', '');
			$node->psql('postgres', $sql, stderr => \$e, on_error_stop => 0);
			$err = $e;
		}
		else
		{
			$sess{$conn} //= $node->background_psql('postgres', on_error_stop => 0);
			my $s = $sess{$conn};
			$s->{stderr} = '';
			$s->query($sql);
			$err = $s->{stderr};
			$s->{stderr} = '';
		}
		if ($err ne '')
		{
			$failed{$txn} = 1;
			$unexpected++ unless $err =~ /could not serialize access/;
			diag("unexpected error in '$sql': $err")
			  unless $err =~ /could not serialize access/;
		}
	}
	foreach my $s (values %sess)
	{
		$s->query("ROLLBACK");
		$s->quit;
	}
	foreach my $gid (split /\n/,
		$node->safe_psql('postgres', 'SELECT gid FROM pg_prepared_xacts'))
	{
		$node->safe_psql('postgres', "ROLLBACK PREPARED '$gid'");
	}
	return join(' ', map { "$_=" . ($failed{$_} ? 'A' : 'C') } sort keys %seen);
}

# The steps of a scenario against twin tables.  Every variant of the table
# kinds must give the outcome of the heap-only run.
my @variants = (['col', 'col'], ['col', 'heap'], ['heap', 'col']);

sub check_scenario
{
	my ($name, $steps, $expect_abort) = @_;
	my $ref = run_scenario(['heap', 'heap'], $steps);

	if ($expect_abort)
	{
		like($ref, qr/=A/, "$name: heap aborts one transaction ($ref)");
	}
	foreach my $k (@variants)
	{
		my $got = run_scenario($k, $steps);
		is($got, $ref, "$name: @$k gives the heap outcome ($ref)");
	}
}

my $SER = 'BEGIN ISOLATION LEVEL SERIALIZABLE';

sub ins
{
	my ($t, $size) = @_;
	return $size eq 'big'
	  ? "INSERT INTO $t SELECT g FROM generate_series(1, 2500) g"
	  : "INSERT INTO $t VALUES (1)";
}

# Write skew over two tables: T1 writes A and reads B, T2 writes B and reads
# A; read first or write first, the writer's rows buffered or flushed by
# size, commit order, with a subtransaction, with PREPARE TRANSACTION.
foreach my $size (qw(small big))
{
	foreach my $timing (qw(before during aftercommit))
	{
		foreach my $order (qw(12 21))
		{
			next if $timing eq 'aftercommit' && $order eq '21';
			foreach my $mode (qw(plain savepoint prepared))
			{
				next if $mode eq 'prepared' && $order eq '21';
				my @s = (
					[1, 's1', "SET kingres_columnar.stripe_row_count = 1000"],
					[2, 's2', "SET kingres_columnar.stripe_row_count = 1000"],
					[1, 's1', $SER], [1, 's1', 'SELECT 1'],
					[2, 's2', $SER], [2, 's2', 'SELECT 1']);
				my @w1 = $mode eq 'savepoint'
				  ? ([1, 's1', 'SAVEPOINT sp'], [1, 's1', ins('@A@', $size)],
					 [1, 's1', 'RELEASE sp'])
				  : ([1, 's1', ins('@A@', $size)]);
				my @c1 = $mode eq 'prepared'
				  ? ([1, 's1', "PREPARE TRANSACTION 'p1'"],
					 [1, 'adm', "COMMIT PREPARED 'p1'"])
				  : ([1, 's1', 'COMMIT']);
				my @c2 = ([2, 's2', 'COMMIT']);

				if ($timing eq 'before')
				{
					push @s, [2, 's2', 'SELECT count(*) FROM @A@'], @w1,
					  [2, 's2', ins('@B@', $size)],
					  [1, 's1', 'SELECT count(*) FROM @B@'];
				}
				elsif ($timing eq 'during')
				{
					push @s, @w1, [2, 's2', 'SELECT count(*) FROM @A@'],
					  [2, 's2', ins('@B@', $size)],
					  [1, 's1', 'SELECT count(*) FROM @B@'];
				}
				else
				{
					push @s, @w1, [1, 's1', 'SELECT count(*) FROM @B@'], @c1,
					  [2, 's2', 'SELECT count(*) FROM @A@'],
					  [2, 's2', ins('@B@', $size)];
					push @s, @c2;
					check_scenario("skew $size $timing $mode", \@s, 1);
					next;
				}
				push @s, ($order eq '12' ? (@c1, @c2) : (@c2, @c1));
				check_scenario("skew $size $timing $order $mode", \@s, 1);
			}
		}
	}
}

# The case of the README: read then write, both orders, on one table.
foreach my $order (qw(12 21))
{
	foreach my $size (qw(small big))
	{
		my @c = $order eq '12'
		  ? ([1, 's1', 'COMMIT'], [2, 's2', 'COMMIT'])
		  : ([2, 's2', 'COMMIT'], [1, 's1', 'COMMIT']);
		my @pre = (
			[1, 's1', "SET kingres_columnar.stripe_row_count = 1000"],
			[2, 's2', "SET kingres_columnar.stripe_row_count = 1000"],
			[1, 's1', $SER], [1, 's1', 'SELECT 1'],
			[2, 's2', $SER], [2, 's2', 'SELECT 1']);
		check_scenario("one table, read then write $size $order",
			[@pre,
			 [1, 's1', 'SELECT count(*) FROM @A@'],
			 [2, 's2', 'SELECT count(*) FROM @A@'],
			 [1, 's1', ins('@A@', $size)], [2, 's2', ins('@A@', $size)], @c], 1);
		check_scenario("one table, write then read $size $order",
			[@pre,
			 [1, 's1', ins('@A@', $size)], [2, 's2', ins('@A@', $size)],
			 [1, 's1', 'SELECT count(*) FROM @A@'],
			 [2, 's2', 'SELECT count(*) FROM @A@'], @c], 1);
	}
}

# The read-only anomaly: T3 reads after T2 committed and before T1 commits.
foreach my $size (qw(small big))
{
	foreach my $when (qw(before during aftercommit))
	{
		my @s = (
			[1, 's1', "SET kingres_columnar.stripe_row_count = 1000"],
			[1, 's1', $SER], [1, 's1', 'SELECT count(*) FROM @A@'],
			[2, 's2', $SER], [2, 's2', ins('@A@', 'small')],
			[2, 's2', 'COMMIT'],
			[3, 's3', 'BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY'],
			[3, 's3', 'SELECT count(*) FROM @A@']);
		push @s, [3, 's3', 'SELECT count(*) FROM @B@'] if $when eq 'before';
		push @s, [1, 's1', ins('@B@', $size)];
		push @s, [3, 's3', 'SELECT count(*) FROM @B@'] if $when eq 'during';
		push @s, [1, 's1', 'COMMIT'];
		push @s, [3, 's3', 'SELECT count(*) FROM @B@'] if $when eq 'aftercommit';
		push @s, [3, 's3', 'COMMIT'];
		check_scenario("read-only anomaly $size $when", \@s, 1);
	}
}

# Rolled back writes leave no dependency of their own.
check_scenario('rolled back subtransaction',
	[[1, 's1', $SER], [1, 's1', 'SELECT 1'], [2, 's2', $SER], [2, 's2', 'SELECT 1'],
	 [1, 's1', 'SAVEPOINT sp'], [1, 's1', ins('@A@', 'small')],
	 [1, 's1', 'ROLLBACK TO sp'], [2, 's2', 'SELECT count(*) FROM @A@'],
	 [2, 's2', ins('@B@', 'small')], [1, 's1', 'SELECT count(*) FROM @B@'],
	 [1, 's1', 'COMMIT'], [2, 's2', 'COMMIT']], 0);

# Unrelated tables never abort each other (the first review's case).
foreach my $kinds (['heap', 'heap'], ['col', 'col'])
{
	$node->safe_psql('postgres', "
		DROP TABLE IF EXISTS u1, u2, u3, u4;
		CREATE TABLE u1 (a int) USING " . ($kinds->[0] eq 'col' ? 'columnar' : 'heap') . ";
		CREATE TABLE u2 (a int) USING " . ($kinds->[1] eq 'col' ? 'columnar' : 'heap') . ";
		CREATE TABLE u3 (a int) USING " . ($kinds->[0] eq 'col' ? 'columnar' : 'heap') . ";
		CREATE TABLE u4 (a int) USING " . ($kinds->[1] eq 'col' ? 'columnar' : 'heap') . ";
		INSERT INTO u1 VALUES (1); INSERT INTO u3 VALUES (1);");
	my $s1 = $node->background_psql('postgres', on_error_stop => 0);
	my $s2 = $node->background_psql('postgres', on_error_stop => 0);
	$s1->query_safe($SER);
	$s1->query_safe('SELECT count(*) FROM u1');
	$s2->query_safe($SER);
	$s2->query_safe('SELECT count(*) FROM u3');
	$s1->query_safe('INSERT INTO u2 VALUES (1)');
	$s2->query_safe('INSERT INTO u4 VALUES (2)');
	$s2->{stderr} = '';
	$s2->query('COMMIT');
	is($s2->{stderr}, '', "unrelated tables @$kinds: first commit succeeds");
	$s1->{stderr} = '';
	$s1->query('COMMIT');
	is($s1->{stderr}, '', "unrelated tables @$kinds: second commit succeeds");
	$s1->quit;
	$s2->quit;
}

is($unexpected, 0, 'no error other than a serialization failure');

$node->stop;
done_testing();
