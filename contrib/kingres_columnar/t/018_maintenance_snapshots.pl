# contrib/kingres_columnar/t/018_maintenance_snapshots.pl
#
# C4-02.  No operation changes which rows a transaction's snapshot sees, for
# that transaction or any other.  The defect: a REPEATABLE READ or SERIALIZABLE
# transaction that called columnar_compact read, afterwards, rows committed
# after its snapshot (3 rows, then 4), and under SERIALIZABLE committed with no
# dependency.  The compaction read with a rule that is right for deciding what
# may be dropped (everything committed) and wrote the copies as the stripes of
# its own transaction, which the transaction sees in full.
#
# Every scenario runs against a heap twin (the compaction is then a no-op) and
# against a columnar table; the observations must be equal.  Sessions are
# interleaved the way the review's reproduction did: snapshot, a concurrent
# commit, the maintenance command, then reads.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colmaint');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

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

sub fresh
{
	my ($am) = @_;

	$node->safe_psql('postgres', "
		DROP TABLE IF EXISTS ca; DROP TABLE IF EXISTS obs;
		CREATE TABLE ca (v int) USING $am;
		INSERT INTO ca VALUES (1);
		INSERT INTO ca VALUES (2);
		INSERT INTO ca VALUES (3);
		CREATE TABLE obs (before_n int, after_n int);");
}

# what the compaction is, for each twin
sub compact_sql
{
	my ($am) = @_;
	return $am eq 'columnar' ? "SELECT kingres.columnar_compact('ca', 100)" : 'SELECT 1';
}

sub obs_of
{
	my ($out) = @_;
	return defined $out ? $out : '';
}

# ------------------------------------------------------------------------
# The review's case, at both isolation levels: the transaction's own reads
# before and after, then a commit
# ------------------------------------------------------------------------

foreach my $iso ('REPEATABLE READ', 'SERIALIZABLE')
{
	my %res;
	foreach my $am (qw(heap columnar))
	{
		fresh($am);
		my $s1 = $node->background_psql('postgres', on_error_stop => 0);
		my @log;
		run_in($s1, "BEGIN ISOLATION LEVEL $iso");
		push @log, 'before=' . run_in($s1, 'SELECT count(*) FROM ca');
		$node->safe_psql('postgres', 'INSERT INTO ca VALUES (100)');
		push @log, 'compact=' . (run_in($s1, compact_sql($am)) =~ /ERROR/ ? 'ERR' : 'ok');
		push @log, 'after=' . run_in($s1, 'SELECT count(*) || \'/\' || sum(v) FROM ca');
		run_in($s1, 'INSERT INTO obs SELECT 3, count(*) FROM ca');
		push @log, 'commit=' . (run_in($s1, 'COMMIT') =~ /ERROR/ ? 'ERR' : 'ok');
		push @log, 'obs=' . $node->safe_psql('postgres', 'SELECT before_n || \',\' || after_n FROM obs');
		push @log, 'latest=' . $node->safe_psql('postgres', 'SELECT count(*) FROM ca');
		$s1->quit;
		$res{$am} = join(' ', @log);
	}
	is($res{columnar}, $res{heap}, "$iso: the compacting transaction sees what its snapshot sees ($res{heap})");
	like($res{columnar}, qr/after=3\/6/, "$iso: three rows after the compaction, not four");
}

# the same with the compaction at the very start and the concurrent commit
# after it: the transaction must not see the later row either way
foreach my $iso ('REPEATABLE READ', 'SERIALIZABLE')
{
	my %res;
	foreach my $am (qw(heap columnar))
	{
		fresh($am);
		my $s1 = $node->background_psql('postgres', on_error_stop => 0);
		my @log;
		run_in($s1, "BEGIN ISOLATION LEVEL $iso");
		push @log, 'compact=' . (run_in($s1, compact_sql($am)) =~ /ERROR/ ? 'ERR' : 'ok');
		$node->safe_psql('postgres', 'INSERT INTO ca VALUES (100)');
		push @log, 'after=' . run_in($s1, 'SELECT count(*) FROM ca');
		run_in($s1, 'COMMIT');
		push @log, 'latest=' . $node->safe_psql('postgres', 'SELECT count(*) FROM ca');
		$s1->quit;
		$res{$am} = join(' ', @log);
	}
	is($res{columnar}, $res{heap}, "$iso: compaction first, commit after: equal to the heap twin ($res{heap})");
}

# ------------------------------------------------------------------------
# Other snapshots, before and after the compacting transaction commits
# ------------------------------------------------------------------------

foreach my $iso ('REPEATABLE READ', 'SERIALIZABLE')
{
	my %res;
	foreach my $am (qw(heap columnar))
	{
		fresh($am);
		my $old = $node->background_psql('postgres', on_error_stop => 0);
		my $mid = $node->background_psql('postgres', on_error_stop => 0);
		my $cmp = $node->background_psql('postgres', on_error_stop => 0);
		my @log;
		run_in($old, "BEGIN ISOLATION LEVEL $iso");
		push @log, 'old0=' . run_in($old, 'SELECT count(*) FROM ca');
		$node->safe_psql('postgres', 'INSERT INTO ca VALUES (100)');	# committed after the old snapshot
		run_in($cmp, "BEGIN ISOLATION LEVEL $iso");
		push @log, 'cmp0=' . run_in($cmp, 'SELECT count(*) FROM ca');
		$node->safe_psql('postgres', 'INSERT INTO ca VALUES (200)');	# committed after the compactor's snapshot
		run_in($mid, "BEGIN ISOLATION LEVEL $iso");
		push @log, 'mid0=' . run_in($mid, 'SELECT count(*) FROM ca');	# sees both
		push @log, 'compact=' . (run_in($cmp, compact_sql($am)) =~ /ERROR/ ? 'ERR' : 'ok');
		push @log, 'old1=' . run_in($old, 'SELECT count(*) FROM ca');
		push @log, 'mid1=' . run_in($mid, 'SELECT count(*) FROM ca');
		push @log, 'cmp1=' . run_in($cmp, 'SELECT count(*) FROM ca');
		push @log, 'commit=' . (run_in($cmp, 'COMMIT') =~ /ERROR/ ? 'ERR' : 'ok');
		push @log, 'old2=' . run_in($old, 'SELECT count(*) FROM ca');
		push @log, 'mid2=' . run_in($mid, 'SELECT count(*) FROM ca');
		push @log, 'fresh=' . $node->safe_psql('postgres', 'SELECT count(*) FROM ca');
		run_in($old, 'COMMIT'); run_in($mid, 'COMMIT');
		push @log, 'later=' . $node->safe_psql('postgres', 'SELECT count(*) FROM ca');
		$_->quit foreach ($old, $mid, $cmp);
		$res{$am} = join(' ', @log);
	}
	is($res{columnar}, $res{heap}, "$iso: older, newer and later snapshots around a compaction ($res{heap})");
}

# ------------------------------------------------------------------------
# Another transaction compacted the same stripes since the snapshot: heap has
# nothing to conflict with, columnar must not copy the rows twice
# ------------------------------------------------------------------------

foreach my $iso ('REPEATABLE READ', 'SERIALIZABLE')
{
	fresh('columnar');
	my $s1 = $node->background_psql('postgres', on_error_stop => 0);
	run_in($s1, "BEGIN ISOLATION LEVEL $iso");
	run_in($s1, 'SELECT count(*) FROM ca');
	$node->safe_psql('postgres', "SELECT kingres.columnar_compact('ca', 100)");
	my $r = run_in($s1, compact_sql('columnar'));
	like($r, qr/could not serialize access/, "$iso: stripes replaced since the snapshot: a serialization failure");
	run_in($s1, 'ROLLBACK');
	is($node->safe_psql('postgres', 'SELECT count(*) FROM ca'), 3, "$iso: no row doubled");
	is($node->safe_psql('postgres', "SELECT count(*) FROM kingres.columnar_stripes('ca')"), 1, "$iso: one stripe");
	$s1->quit;
}

# ------------------------------------------------------------------------
# READ COMMITTED: the transaction reads what is committed, as heap does, and
# a cursor opened before the compaction keeps reading what it started with
# ------------------------------------------------------------------------

{
	my %res;
	foreach my $am (qw(heap columnar))
	{
		fresh($am);
		my $s1 = $node->background_psql('postgres', on_error_stop => 0);
		my @log;
		run_in($s1, 'BEGIN');
		push @log, 'before=' . run_in($s1, 'SELECT count(*) FROM ca');
		run_in($s1, 'DECLARE c CURSOR FOR SELECT v FROM ca ORDER BY v');
		run_in($s1, 'FETCH 1 FROM c');
		$node->safe_psql('postgres', 'INSERT INTO ca VALUES (100)');
		push @log, 'compact=' . (run_in($s1, compact_sql($am)) =~ /ERROR/ ? 'ERR' : 'ok');
		push @log, 'after=' . run_in($s1, 'SELECT count(*) FROM ca');
		my $fetched = run_in($s1, 'FETCH ALL FROM c');
		$fetched =~ s/\s+/,/g;
		push @log, "cursor=$fetched";
		run_in($s1, 'COMMIT');
		$s1->quit;
		$res{$am} = join(' ', @log);
	}
	is($res{columnar}, $res{heap}, "READ COMMITTED: statements and an older cursor around a compaction ($res{heap})");
}

# ------------------------------------------------------------------------
# SERIALIZABLE write skew around a compaction: the outcome of the heap twin
# ------------------------------------------------------------------------

foreach my $am (qw(heap columnar))
{
	fresh($am);
	$node->safe_psql('postgres', 'CREATE TABLE IF NOT EXISTS cb (x int); TRUNCATE cb');
}
{
	my %res;
	foreach my $am (qw(heap columnar))
	{
		fresh($am);
		$node->safe_psql('postgres', 'TRUNCATE cb');
		my $s1 = $node->background_psql('postgres', on_error_stop => 0);
		my $s2 = $node->background_psql('postgres', on_error_stop => 0);
		run_in($s1, 'BEGIN ISOLATION LEVEL SERIALIZABLE');
		run_in($s2, 'BEGIN ISOLATION LEVEL SERIALIZABLE');
		run_in($s1, 'SELECT count(*) FROM ca');
		run_in($s2, 'SELECT count(*) FROM cb');
		$node->safe_psql('postgres', 'INSERT INTO ca VALUES (100)');
		run_in($s1, compact_sql($am));
		run_in($s1, 'INSERT INTO cb VALUES (1)');
		run_in($s2, 'INSERT INTO ca VALUES (200)');
		my $a = run_in($s1, 'COMMIT') =~ /ERROR/ ? 'A' : 'C';
		my $b = run_in($s2, 'COMMIT') =~ /ERROR/ ? 'A' : 'C';
		$res{$am} = "1=$a 2=$b";
		$_->quit foreach ($s1, $s2);
	}
	is($res{columnar}, $res{heap}, "SERIALIZABLE: write skew around a compaction ends as with heap ($res{heap})");
}

# ------------------------------------------------------------------------
# The rewriting paths read with their own rule and write stripes: a rewrite
# inside a transaction that has a snapshot, a VACUUM FULL, and a conversion,
# each against a snapshot older than a concurrent commit.  Columnar must give
# the heap twin's answer, or refuse with a serialization failure.
# ------------------------------------------------------------------------

foreach my $cmd ('ALTER TABLE ca ALTER COLUMN v TYPE bigint', 'ALTER TABLE ca ADD COLUMN w float DEFAULT random()')
{
	my %res;
	foreach my $am (qw(heap columnar))
	{
		fresh($am);
		my $s1 = $node->background_psql('postgres', on_error_stop => 0);
		my @log;
		run_in($s1, 'BEGIN ISOLATION LEVEL REPEATABLE READ');
		push @log, 'before=' . run_in($s1, 'SELECT count(*) FROM ca');
		$node->safe_psql('postgres', 'INSERT INTO ca VALUES (100)');
		my $r = run_in($s1, $cmd);
		push @log, 'ddl=' . ($r =~ /ERROR/ ? 'ERR' : 'ok');
		push @log, 'after=' . (run_in($s1, 'SELECT count(*) FROM ca') =~ s/^ERROR.*/ERR/r);
		run_in($s1, 'ROLLBACK');
		$s1->quit;
		$res{$am} = join(' ', @log);
	}
	if ($res{columnar} =~ /ddl=ERR/)
	{
		pass("$cmd after a snapshot and a concurrent commit: refused ($res{columnar})");
	}
	else
	{
		is($res{columnar}, $res{heap}, "$cmd after a snapshot and a concurrent commit: the heap twin ($res{heap})");
	}
}

{
	my %res;
	foreach my $am (qw(heap columnar))
	{
		fresh($am);
		my $s1 = $node->background_psql('postgres', on_error_stop => 0);
		my @log;
		# the snapshot is taken on another table: a read of ca would hold the
		# lock VACUUM FULL waits for
		run_in($s1, 'BEGIN ISOLATION LEVEL REPEATABLE READ');
		run_in($s1, 'SELECT count(*) FROM obs');
		$node->safe_psql('postgres', 'INSERT INTO ca VALUES (100)');
		$node->safe_psql('postgres', 'VACUUM FULL ca');
		push @log, 'after=' . (run_in($s1, 'SELECT count(*) FROM ca') =~ s/^ERROR.*/ERR/r);
		run_in($s1, 'ROLLBACK');
		$s1->quit;
		$res{$am} = join(' ', @log);
	}
	if ($res{columnar} =~ /after=ERR/)
	{
		pass("VACUUM FULL after a snapshot and a concurrent commit: a serialization failure for the old snapshot");
	}
	else
	{
		is($res{columnar}, $res{heap}, "VACUUM FULL after a snapshot and a concurrent commit: the heap twin ($res{heap})");
	}
}

$node->stop;
done_testing();
