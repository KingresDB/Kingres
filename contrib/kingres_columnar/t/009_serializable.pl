# contrib/kingres_columnar/t/009_serializable.pl
#
# SERIALIZABLE isolation against heap twins and against a model.
#
# A columnar table takes part in serializable snapshot isolation by what it
# does in three places: a scan takes a relation-level predicate lock before it
# reads the stripe catalog; a write checks for conflicts when a row is
# buffered and again when the buffer becomes a stripe (a reader whose lock came
# in between is found then); a scan that skips a stripe because its writer had
# not committed before the snapshot reports the dependency, as a heap scan
# does for a tuple it skips.  The last one needs the top-level transaction of
# the writer, and the stripe row carries the id of the subtransaction that
# flushed it (C3-01), so the writer's rows must reach the table in every way a
# stripe can be written: at commit, by size at top level, by size or by memory
# inside a savepoint, a nested savepoint, an exception block, and in a
# subtransaction that is rolled back.
#
# Part 1: fixed scenarios.  Each is run against tables that are heap,
# columnar, and mixed, one statement at a time in a fixed order, and the set of
# transactions that commit must be that of the heap-only run (the error texts
# are not compared: the serialization failure may be raised by another
# statement of the same transaction).  Every case proves its own precondition
# from the stripe catalog of the writer's own session: how many stripes the
# writer's rows are in at that point, and how many of them were written by a
# subtransaction (written_by_xmin is not the transaction's own id), so that a
# case cannot silently test something else (C3-06).
#
# Part 2: random schedules of three transactions, seeded, run on columnar and
# on heap tables.  The set of transactions that committed must be explained
# by some serial order, computed from a model of what each statement read.
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

# The stripes the running transaction wrote (stripe 1 is the committed one of
# the setup) and how many of those were written by a subtransaction.
my $PROBE = q{SELECT 'S=' || count(*) FILTER (WHERE stripe_id > 1) || ' sub=' ||
  count(*) FILTER (WHERE stripe_id > 1 AND written_by_xmin::text::bigint <>
                   (pg_current_xact_id()::text::bigint & 4294967295))
  FROM kingres.columnar_stripes('@T@')};
# From another session: what is committed.
my $PROBE_ADM = q{SELECT 'S=' || count(*) FILTER (WHERE stripe_id > 1)
  FROM kingres.columnar_stripes('@T@')};

# Run the steps of one scenario on tables of the given kinds.  A step is
# [txn, conn, sql] or [txn, conn, sql, {tab => 'A', re => qr//}]: the second
# form is a precondition, run only when that table is columnar, whose output
# must match.  Returns the outcome of every transaction, "1=C 2=A ...",
# C committed, A aborted.
sub run_scenario
{
	my ($name, $kinds, $steps, $cfg) = @_;
	my $stripe = $cfg->{stripe};
	my $mem = $cfg->{mem} // '128MB';
	my @am = map { $_ eq 'col' ? 'USING columnar' : 'USING heap' } @$kinds;
	my %is_col = (A => $kinds->[0] eq 'col', B => ($kinds->[1] // $kinds->[0]) eq 'col');

	my $sql = "DROP TABLE IF EXISTS t_a, t_b;
		CREATE TABLE t_a (v int, p text) $am[0];
		CREATE TABLE t_b (v int, p text) " . ($am[1] // $am[0]) . ';';
	foreach my $t (qw(A B))
	{
		next unless $is_col{$t} && defined $stripe;
		$sql .= sprintf("SELECT kingres.columnar_set_options('t_%s', stripe_row_count => %d, chunk_row_count => 100);",
			lc $t, $stripe);
	}
	$sql .= "INSERT INTO t_a VALUES (0); INSERT INTO t_b VALUES (0);";
	$node->safe_psql('postgres', $sql);

	my %sess;
	my (%failed, %seen);
	foreach my $st (@$steps)
	{
		my ($txn, $conn, $q, $pre) = @$st;

		next if $pre && !$is_col{$pre->{tab}};
		$seen{$txn} = 1 unless $pre;
		next if $failed{$txn};
		$q =~ s/\@A\@/t_a/g;
		$q =~ s/\@B\@/t_b/g;
		my ($err, $out) = ('', '');
		if ($conn eq 'adm')
		{
			$node->psql('postgres', $q, stderr => \$err, stdout => \$out,
				on_error_stop => 0);
		}
		else
		{
			unless ($sess{$conn})
			{
				$sess{$conn} = $node->background_psql('postgres', on_error_stop => 0);
				$sess{$conn}->query_safe(
					"SET kingres_columnar.write_buffer_memory = '$mem'");
			}
			my $s = $sess{$conn};
			$s->{stderr} = '';
			$out = $s->query($q);
			$err = $s->{stderr};
			$s->{stderr} = '';
		}
		if ($pre)
		{
			$out =~ s/^\s+|\s+$//g;
			like($out, $pre->{re}, "$name: $pre->{what}");
			diag("probe error: $err") if $err ne '';
			next;
		}
		if ($err ne '')
		{
			$failed{$txn} = 1;
			$unexpected++ unless $err =~ /could not serialize access/;
			diag("unexpected error in '$q': $err")
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
	my ($name, $steps, $expect_abort, $cfg) = @_;
	my $ref = run_scenario($name, ['heap', 'heap'], $steps, $cfg);

	like($ref, qr/=A/, "$name: heap aborts one transaction ($ref)")
	  if $expect_abort;
	foreach my $k (@variants)
	{
		my $got = run_scenario("$name @$k", $k, $steps, $cfg);
		is($got, $ref, "$name: @$k gives the heap outcome ($ref)");
	}
}

my $SER = 'BEGIN ISOLATION LEVEL SERIALIZABLE';

# The sizes of a write.  small: one row, flushed at commit.  size: 2500 rows
# into stripes of 1000, two flushed by size and 500 buffered.  exact: 2000
# rows into stripes of 1000, both flushed by size and nothing left to flush at
# commit (the case where the commit-time flush cannot hide a missing
# dependency, C3-01).  mem: 2500 rows
# of 2 000 bytes against write_buffer_memory = 1MB and stripes of 100 000
# rows, flushed by memory only.  dflt: 300 000 rows into tables with the
# default stripe size of 150 000 rows (the case of the review), two stripes.
my %CFG = (
	small => {stripe => 1000},
	size => {stripe => 1000},
	exact => {stripe => 1000},
	mem => {stripe => 100000, mem => '1MB'},
	dflt => {},
);
my %ROWS = (small => 1, size => 2500, exact => 2000, mem => 2500, dflt => 300000);

sub ins
{
	my ($t, $size) = @_;
	my $pad = $size eq 'mem' ? "repeat('x', 2000)" : 'NULL';

	return "INSERT INTO $t VALUES (1)" if $size eq 'small';
	return "INSERT INTO $t SELECT g, $pad FROM generate_series(1, $ROWS{$size}) g";
}

# The precondition of a writer's stripes: flushed when the buffer is full, by
# size or memory (and then by a subtransaction when the write is inside one),
# and nothing at all for a small write.
sub probe
{
	my ($t, $conn, $tab, $size, $in_sub, $what) = @_;
	my $re;

	if ($size eq 'small') { $re = qr/^S=0 sub=0$/ }
	elsif ($size eq 'mem') { $re = $in_sub ? qr/^S=([1-9]\d*) sub=\1$/ : qr/^S=[1-9]\d* sub=0$/ }
	else { $re = $in_sub ? qr/^S=2 sub=2$/ : qr/^S=2 sub=0$/ }
	(my $q = $PROBE) =~ s/\@T\@/\@$tab\@/;
	return [$t, $conn, $q, {tab => $tab, re => $re, what => $what}];
}

sub probe_zero
{
	my ($t, $conn, $tab, $what) = @_;
	(my $q = $PROBE) =~ s/\@T\@/\@$tab\@/;
	return [$t, $conn, $q, {tab => $tab, re => qr/^S=0 sub=0$/, what => $what}];
}

# The statements of a write of $size rows into table $tab by transaction $t in
# the given mode, each with the precondition that proves what it is meant to
# exercise.  Modes: plain (top level), sp (savepoint, released), sp_open
# (savepoint left open), nested (two savepoints), mixed (a small write in the
# outer savepoint, the large one in the inner), exc (an exception block),
# rolled (the savepoint that flushed is rolled back and a small write
# follows).
sub wsteps
{
	my ($t, $c, $tab, $size, $mode) = @_;
	my $tok = "\@$tab\@";
	my $ins = ins($tok, $size);
	my @s;
	my $sub = $mode ne 'plain';

	if ($mode eq 'plain')
	{
		push @s, [$t, $c, $ins], probe($t, $c, $tab, $size, 0, "$size write at top level");
	}
	elsif ($mode eq 'sp' || $mode eq 'sp_open')
	{
		push @s, [$t, $c, 'SAVEPOINT sp'], [$t, $c, $ins],
		  probe($t, $c, $tab, $size, 1, "$size write in a savepoint");
		push @s, [$t, $c, 'RELEASE sp'] if $mode eq 'sp';
	}
	elsif ($mode eq 'nested')
	{
		push @s, [$t, $c, 'SAVEPOINT sa'], [$t, $c, 'SAVEPOINT sb'], [$t, $c, $ins],
		  probe($t, $c, $tab, $size, 1, "$size write in nested savepoints"),
		  [$t, $c, 'RELEASE sb'], [$t, $c, 'RELEASE sa'];
	}
	elsif ($mode eq 'mixed')
	{
		push @s, [$t, $c, 'SAVEPOINT sa'], [$t, $c, ins($tok, 'small')],
		  [$t, $c, 'SAVEPOINT sb'], [$t, $c, $ins],
		  probe($t, $c, $tab, $size, 1, "$size write in the inner of two savepoints"),
		  [$t, $c, 'RELEASE sb'], [$t, $c, 'RELEASE sa'];
	}
	elsif ($mode eq 'exc')
	{
		push @s, [$t, $c, "DO \$\$ BEGIN BEGIN $ins; EXCEPTION WHEN division_by_zero THEN NULL; END; END \$\$"],
		  probe($t, $c, $tab, $size, 1, "$size write in an exception block");
	}
	elsif ($mode eq 'rolled')
	{
		push @s, [$t, $c, 'SAVEPOINT sp'], [$t, $c, $ins],
		  probe($t, $c, $tab, $size, 1, "$size write before the rollback of its savepoint"),
		  [$t, $c, 'ROLLBACK TO sp'],
		  probe_zero($t, $c, $tab, "nothing left after the rollback of the savepoint"),
		  [$t, $c, 'RELEASE sp'], [$t, $c, ins($tok, 'small')];
	}
	else { die "unknown mode $mode" }
	return @s;
}

sub pre_steps
{
	return ([1, 's1', $SER], [1, 's1', 'SELECT 1'],
		[2, 's2', $SER], [2, 's2', 'SELECT 1']);
}

# After the commit of the writer: its small write became a stripe at commit.
sub commit_probe
{
	my ($t, $tab, $size, $mode) = @_;
	$size = 'small' if $mode eq 'rolled';	# the large write was rolled back
	my %want = (small => 1, size => 3, exact => 2, dflt => 2);
	$want{$size}++ if $mode eq 'mixed' && $size eq 'exact';	# the small write of the outer savepoint is a stripe of its own
	(my $q = $PROBE_ADM) =~ s/\@T\@/\@$tab\@/;
	return [$t, 'adm', $q, {tab => $tab, what => "stripes after the commit",
		re => ($size eq 'mem' ? qr/^S=([2-9]|[1-9]\d+)$/ : qr/^S=$want{$size}$/)}];
}

# Write skew over two tables: T1 writes A and reads B, T2 writes B and reads
# A; read first or write first, the writer's rows buffered or flushed by
# size, by memory, in a savepoint of any shape, commit order, PREPARE.
foreach my $size (qw(small size exact mem dflt))
{
	foreach my $timing (qw(before during aftercommit))
	{
		foreach my $order (qw(12 21))
		{
			next if $timing eq 'aftercommit' && $order eq '21';
			foreach my $mode (qw(plain sp sp_open nested mixed exc rolled prepared))
			{
				next if $mode eq 'prepared' && $order eq '21';
				next if $size eq 'dflt' && ($mode ne 'sp' && $mode ne 'plain');
				# the odd shapes once per timing and size, in both orders only
				# while the writer's rows are not yet visible
				next if $mode =~ /^(nested|mixed|exc|rolled)$/ && $timing eq 'aftercommit' && $size ne 'size';
				next if $mode eq 'sp_open' && $timing eq 'aftercommit';
				my $wmode = $mode eq 'prepared' ? 'sp' : $mode;
				my @s = pre_steps();
				my @w1 = wsteps(1, 's1', 'A', $size, $wmode);
				my @w2 = wsteps(2, 's2', 'B', $size, 'plain');
				my @c1 = $mode eq 'prepared'
				  ? ([1, 's1', "PREPARE TRANSACTION 'p1'"],
					 [1, 'adm', "COMMIT PREPARED 'p1'"])
				  : ([1, 's1', 'COMMIT']);
				my @c2 = ([2, 's2', 'COMMIT']);

				if ($timing eq 'before')
				{
					push @s, [2, 's2', 'SELECT count(*) FROM @A@'], @w1, @w2,
					  [1, 's1', 'SELECT count(*) FROM @B@'];
				}
				elsif ($timing eq 'during')
				{
					push @s, @w1, [2, 's2', 'SELECT count(*) FROM @A@'], @w2,
					  [1, 's1', 'SELECT count(*) FROM @B@'];
				}
				else
				{
					push @s, @w1, [1, 's1', 'SELECT count(*) FROM @B@'], @c1,
					  commit_probe(1, 'A', $size, $wmode),
					  [2, 's2', 'SELECT count(*) FROM @A@'], @w2, @c2;
					check_scenario("skew $size $timing $mode", \@s, 1, $CFG{$size});
					next;
				}
				push @s, ($order eq '12' ? (@c1, commit_probe(1, 'A', $size, $wmode), @c2) : (@c2, @c1));
				check_scenario("skew $size $timing $order $mode", \@s, 1, $CFG{$size});
			}
		}
	}
}

# The case of the README: read then write, both orders, on one table.
foreach my $order (qw(12 21))
{
	foreach my $size (qw(small size exact))
	{
		foreach my $mode (qw(plain sp))
		{
			my @c = $order eq '12'
			  ? ([1, 's1', 'COMMIT'], [2, 's2', 'COMMIT'])
			  : ([2, 's2', 'COMMIT'], [1, 's1', 'COMMIT']);
			my @pre = pre_steps();
			check_scenario("one table, read then write $size $mode $order",
				[@pre,
				 [1, 's1', 'SELECT count(*) FROM @A@'],
				 [2, 's2', 'SELECT count(*) FROM @A@'],
				 wsteps(1, 's1', 'A', $size, $mode), wsteps(2, 's2', 'A', $size, $mode), @c],
				1, $CFG{$size});
			check_scenario("one table, write then read $size $mode $order",
				[@pre,
				 wsteps(1, 's1', 'A', $size, $mode), wsteps(2, 's2', 'A', $size, $mode),
				 [1, 's1', 'SELECT count(*) FROM @A@'],
				 [2, 's2', 'SELECT count(*) FROM @A@'], @c],
				1, $CFG{$size});
		}
	}
}

# The read-only anomaly: T3 reads after T2 committed and before T1 commits;
# T1 writes in a savepoint.
foreach my $size (qw(small size exact))
{
	foreach my $mode (qw(plain sp))
	{
		foreach my $when (qw(before during aftercommit))
		{
			my @s = (
				[1, 's1', $SER], [1, 's1', 'SELECT count(*) FROM @A@'],
				[2, 's2', $SER], [2, 's2', ins('@A@', 'small')],
				[2, 's2', 'COMMIT'],
				[3, 's3', 'BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY'],
				[3, 's3', 'SELECT count(*) FROM @A@']);
			push @s, [3, 's3', 'SELECT count(*) FROM @B@'] if $when eq 'before';
			push @s, wsteps(1, 's1', 'B', $size, $mode);
			push @s, [3, 's3', 'SELECT count(*) FROM @B@'] if $when eq 'during';
			push @s, [1, 's1', 'COMMIT'];
			push @s, [3, 's3', 'SELECT count(*) FROM @B@'] if $when eq 'aftercommit';
			push @s, [3, 's3', 'COMMIT'];
			check_scenario("read-only anomaly $size $mode $when", \@s, 1, $CFG{$size});
		}
	}
}

# Rolled back writes leave no dependency of their own, flushed or not.
foreach my $size (qw(small size exact))
{
	check_scenario("rolled back subtransaction $size",
		[pre_steps(),
		 [1, 's1', 'SAVEPOINT sp'], [1, 's1', ins('@A@', $size)],
		 [1, 's1', 'ROLLBACK TO sp'], [2, 's2', 'SELECT count(*) FROM @A@'],
		 [2, 's2', ins('@B@', 'small')], [1, 's1', 'SELECT count(*) FROM @B@'],
		 [1, 's1', 'COMMIT'], [2, 's2', 'COMMIT']], 0, $CFG{$size});
}

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

# ------------------------------------------------------------------------
# Part 2: random schedules of three transactions
# ------------------------------------------------------------------------
#
# Each transaction reads the two tables (count) and writes rows of three
# sizes, plain, in a savepoint, in an exception block, or in a savepoint that
# is rolled back, then commits or rolls back; the statements of the three are
# interleaved at random.  A count that a transaction reads is its snapshot's
# rows plus its own: with the model of what every transaction wrote, the
# committed set is explained when some serial order of it gives every read.

my $NSCHED = $ENV{KC_SSI_SCHEDULES} // 30;
my $SEED = 20261116;

sub gen_txn
{
	my ($id) = @_;
	my @ops;
	my $n = 2 + int(rand(3));
	for my $i (1 .. $n)
	{
		my $r = rand();
		my $tab = rand() < 0.5 ? 'A' : 'B';
		if ($r < 0.4) { push @ops, {op => 'read', tab => $tab} }
		else
		{
			my $size = (qw(small small size exact exact mem))[int(rand(6))];
			my $mode = (qw(plain sp exc rolled))[int(rand(4))];
			push @ops, {op => 'write', tab => $tab, size => $size, mode => $mode};
		}
	}
	return {id => $id, ops => \@ops, commit => (rand() < 0.85)};
}


# Run one schedule on tables of one kind; returns the outcome and the reads.
sub run_schedule
{
	my ($kind, $txns, $order) = @_;
	my @am = ($kind eq 'col') ? ('USING columnar') x 2 : ('USING heap') x 2;
	my $sql = "DROP TABLE IF EXISTS t_a, t_b;
		CREATE TABLE t_a (v int, p text) $am[0];
		CREATE TABLE t_b (v int, p text) $am[1];";
	if ($kind eq 'col')
	{
		$sql .= "SELECT kingres.columnar_set_options('t_a', stripe_row_count => 1000, chunk_row_count => 100);
		SELECT kingres.columnar_set_options('t_b', stripe_row_count => 1000, chunk_row_count => 100);";
	}
	$sql .= "INSERT INTO t_a VALUES (0); INSERT INTO t_b VALUES (0);";
	$node->safe_psql('postgres', $sql);

	my %sess;
	my %pos;		# next op of each transaction (0 = begin)
	my %failed;
	my %mine;		# rows the transaction has written so far, per table
	my %reads;		# list of [table, value, own rows at that time]
	my %committed;

	foreach my $id (@$order)
	{
		my $t = $txns->[$id - 1];
		my $c = "s$id";
		my $p = $pos{$id}++;

		next if $failed{$id};
		$sess{$c} //= do
		{
			my $s = $node->background_psql('postgres', on_error_stop => 0);
			$s->query_safe("SET kingres_columnar.write_buffer_memory = '1MB'");
			$s;
		};
		my $s = $sess{$c};
		my @stmts;
		my $after;		# a code ref run when the statements succeeded
		my $probe_tn;	# a write that must have flushed in a subtransaction

		if ($p == 0)
		{
			@stmts = ($SER, 'SELECT 1');
		}
		elsif ($p <= @{$t->{ops}})
		{
			my $op = $t->{ops}[$p - 1];
			my $tn = 't_' . lc $op->{tab};

			if ($op->{op} eq 'read')
			{
				@stmts = ("SELECT count(*) FROM $tn");
				$after = sub {
					my ($out) = @_;
					$out =~ s/\s+//g;
					push @{$reads{$id}}, [$op->{tab}, $out + 0, $mine{$id}{$op->{tab}} // 0];
				};
			}
			else
			{
				my $size = $op->{size};
				my $ins = ins($tn, $size);
				my $net = $ROWS{$size};

				if ($op->{mode} eq 'plain') { @stmts = ($ins) }
				elsif ($op->{mode} eq 'sp') { @stmts = ('SAVEPOINT x', $ins, 'RELEASE x') }
				elsif ($op->{mode} eq 'exc')
				{
					@stmts = ("DO \$\$ BEGIN BEGIN $ins; EXCEPTION WHEN division_by_zero THEN NULL; END; END \$\$");
				}
				else { @stmts = ('SAVEPOINT x', $ins, 'ROLLBACK TO x', 'RELEASE x'); $net = 0 }
				$after = sub { $mine{$id}{$op->{tab}} += $net };
				$probe_tn = $tn
				  if $kind eq 'col' && $size ne 'small' && $op->{mode} =~ /^(sp|exc)$/;
			}
		}
		elsif ($p == @{$t->{ops}} + 1)
		{
			@stmts = ($t->{commit} ? 'COMMIT' : 'ROLLBACK');
			$after = sub { $committed{$id} = 1 if $t->{commit} };
		}
		else { next }

		my ($err, $out) = ('', '');
		foreach my $q (@stmts)
		{
			$s->{stderr} = '';
			$out = $s->query($q);
			$err = $s->{stderr};
			$s->{stderr} = '';
			last if $err ne '';
		}
		if ($err ne '')
		{
			$failed{$id} = 1;
			$unexpected++ unless $err =~ /could not serialize access/;
			diag("unexpected error in schedule: $err")
			  unless $err =~ /could not serialize access/;
			next;
		}
		$after->($out) if $after;
		if ($probe_tn)
		{
			(my $q = $PROBE) =~ s/\@T\@/$probe_tn/;
			my $o = $s->query($q);
			$o =~ s/^\s+|\s+$//g;
			like($o, qr/^S=\d+ sub=[1-9]\d*$/,
				"schedule write of $probe_tn by $id flushed in a subtransaction");
		}
	}
	foreach my $s (values %sess)
	{
		$s->query('ROLLBACK');
		$s->quit;
	}
	return (\%committed, \%reads, \%mine);
}

# Is there a serial order of the committed transactions in which every read
# is what the model says?  A read of table X by T sees the initial row, the
# net rows of every committed transaction before T in the order, and its own.
sub explained
{
	my ($committed, $reads, $mine) = @_;
	my @ids = sort keys %$committed;

	return 1 unless @ids;
	my @perms = ([]);
	foreach my $id (@ids)
	{
		my @next;
		foreach my $p (@perms)
		{
			for my $i (0 .. @$p)
			{
				my @q = @$p;
				splice(@q, $i, 0, $id);
				push @next, \@q;
			}
		}
		@perms = @next;
	}
	foreach my $perm (@perms)
	{
		my $ok = 1;
		my %before;
		foreach my $id (@$perm)
		{
			foreach my $r (@{$reads->{$id} // []})
			{
				my ($tab, $val, $own) = @$r;
				my $base = 1;
				foreach my $b (keys %before) { $base += $mine->{$b}{$tab} // 0 }
				if ($val != $base + $own) { $ok = 0; last }
			}
			last unless $ok;
			$before{$id} = 1;
		}
		return 1 if $ok;
	}
	return 0;
}

srand($SEED);
my $sched_unexplained = 0;
for my $n (1 .. $NSCHED)
{
	my @txns = map { gen_txn($_) } 1 .. 3;
	# the schedule: each transaction's begin, its operations and its end
	my @slots;
	foreach my $t (@txns) { push @slots, ($t->{id}) x (@{$t->{ops}} + 2) }
	my @order;
	{
		my @pool = @slots;
		while (@pool) { push @order, splice(@pool, int(rand(@pool)), 1) }
	}
	my $text = join('', map { $_ } @order);
	foreach my $kind (qw(heap col))
	{
		my ($committed, $reads, $mine) = run_schedule($kind, \@txns, \@order);
		my $ok = explained($committed, $reads, $mine);
		$sched_unexplained++ unless $ok;
		ok($ok, "schedule $n ($kind, seed $SEED, order $text): the committed set "
			  . join('', sort keys %$committed) . " is serializable");
	}
}
is($sched_unexplained, 0, "all $NSCHED random schedules are serializable");

is($unexpected, 0, 'no error other than a serialization failure');

$node->stop;
done_testing();
