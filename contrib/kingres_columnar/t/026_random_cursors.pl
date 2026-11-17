# contrib/kingres_columnar/t/026_random_cursors.pl
#
# C6-07, C7-02.  A seeded random comparison with a heap twin of what cursors,
# savepoints, exception blocks and writes do together, under real memory
# pressure, with the operations that reach the paths of the write side that a
# plain load never reaches.  Every episode is one transaction in which the same
# statements are run against columnar tables and heap tables: inserts of random
# width and number, inserts from the table itself, savepoints, rollbacks to them
# and releases, blocks that fail, cursors with partial fetches, and checks.  The
# rows a cursor returns, taken over its whole life, and every check of the tables
# must be those of the heap twins.
#
# Three profiles of episode:
#   normal    narrow and wide rows under an allowance of 2 MB (the original test)
#   pressure  an allowance of 1 MB and rows of up to 300 kB, so that memory pressure
#             writes stripes while subtransactions commit and cursors are open; a
#             module of this test (t/kcprobe/kcprobe.c, loaded before the access
#             method, so its callbacks run after the access method's) runs
#             statements at the end of every subtransaction (between the two phases
#             of the hand-over of the rows to the parent) and at the pre-commit
#             (after the access method has written its buffers): rows written to the
#             same table and to others, and a COPY into a table made in the
#             transaction, which is what reaches the guards that keep a prepared
#             hand-over from being flushed
#   ceiling   nested savepoints of wide rows until the write buffers reach their
#             ceiling: the error is the answer, and the comparison is skipped
#
# What the file reaches is counted, not assumed, and the two parts are held to
# different standards (C8-07: a test that fails without a defect is worse than no
# test).  A scripted prelude takes each path of the write side that the file claims
# to reach once, by construction, with the exact statements that reach it (write-
# through after the commit-time flush and in the window, memory-pressure flushes, a
# committing subtransaction's own flush, the hand-over's flush, the flush of a full
# stripe, a flush refused because a hand-over was prepared, the ceiling of a write,
# the ceiling of a hand-over), and the file fails when the prelude does not take one:
# that is a change of the code and not of a seed.  The random part then compares with
# the heap twin, and the counters of its episodes are reported at the end, with a
# note for a path that this run's random episodes did not take: it says whether the
# generator still reaches the paths on average, and it is not asserted per run,
# because with a random seed an assertion on a count would fail one run in thirty.
#
# The seed is random for each run, printed, and replayed with KC_FUZZ_SEED; the
# number of episodes is KC_FUZZ_EPISODES (default 90).  A failure prints the
# script of the episode that differed.
#
# Needs a compiler and the server's headers for the probe module (KcProbe.pm: a
# machine without a compiler skips the file with a message that names the module,
# a build that fails fails the file).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcProbe qw(build_module);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $dir = build_module('kcprobe');
plan skip_all => 'the probe module (t/kcprobe/kcprobe.c) could not be built here: it needs a C compiler and the server headers'
  unless defined $dir;

my $seed = defined $ENV{KC_FUZZ_SEED} ? $ENV{KC_FUZZ_SEED} : (int(time) ^ ($$ << 10)) & 0x7fffffff;
my $episodes = defined $ENV{KC_FUZZ_EPISODES} ? $ENV{KC_FUZZ_EPISODES} : 90;
diag("KC_FUZZ_SEED=$seed KC_FUZZ_EPISODES=$episodes (replay with KC_FUZZ_SEED=$seed)");
note("seed $seed");
srand($seed);

my $node = PostgreSQL::Test::Cluster->new('colrandcur');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = '$dir/kcprobe,kingres_columnar'\n");
$node->start;
$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE TABLE p (id int, pad text) USING columnar;
SELECT kingres.columnar_set_options('p', stripe_row_count => 1000, chunk_row_count => 100);
CREATE TABLE h (id int, pad text);
CREATE TABLE q (id int, pad text) USING columnar;
CREATE TABLE hq (id int, pad text);
CREATE FUNCTION fetchlog(cur text, k int, tag text) RETURNS int LANGUAGE plpgsql AS $$
DECLARE c refcursor := cur; r record; n int := 0;
BEGIN
  LOOP
    EXIT WHEN n >= k;
    FETCH c INTO r;
    EXIT WHEN NOT FOUND;
    INSERT INTO fl VALUES (tag, r.id);
    n := n + 1;
  END LOOP;
  RETURN n;
END $$;});

sub run
{
	my ($sql) = @_;
	my ($out, $err) = ('', '');

	$node->psql('postgres', $sql, stdout => \$out, stderr => \$err, on_error_stop => 0);
	return ($out, $err);
}

# how often each path was taken, over all episodes of the random part
my %paths;
my $PATHS_SQL = "SELECT 'PATHS ' || string_agg(path || '=' || hits, ' ' ORDER BY path) FROM kingres.columnar_write_paths();";

sub count_paths
{
	my ($out, $into) = @_;
	if ($out =~ /^PATHS (.*)$/m)
	{
		foreach my $kv (split / /, $1)
		{
			my ($k, $v) = split /=/, $kv;
			$into->{$k} += $v;
		}
	}
}

# ------------------------------------------------------------------------
# A targeted case: a cursor opened on buffered rows, then enough writes to ask
# for a flush under memory pressure.  The cursor must return the rows it began
# with, once each; the table must have all the rows.
# ------------------------------------------------------------------------
{
	my $script = sub
	{
		my ($t) = @_;
		return "
SET kingres_columnar.write_buffer_memory = '1MB';
CREATE TEMP TABLE fl (tag text, id int);
BEGIN;
INSERT INTO $t SELECT g, repeat('a', 1000) FROM generate_series(1, 600) g;
DECLARE c CURSOR FOR SELECT id FROM $t;
SELECT fetchlog('c', 100, 'c');
INSERT INTO $t SELECT g + 1000, repeat('b', 1000) FROM generate_series(1, 2500) g;
SELECT fetchlog('c', 100, 'c');
INSERT INTO $t SELECT g + 5000, repeat('c', 1000) FROM generate_series(1, 2500) g;
SELECT fetchlog('c', 1000000, 'c');
CLOSE c;
SELECT 'CUR=' || count(*) || ':' || sum(id) || ':' || count(DISTINCT id) FROM fl;
SELECT 'TAB=' || count(*) || ':' || sum(id) || ':' || count(DISTINCT id) FROM $t;
COMMIT;
SELECT 'FIN=' || count(*) || ':' || sum(id) FROM $t;";
	};
	$node->safe_psql('postgres', 'TRUNCATE p, h');
	my ($po, $pe) = run($script->('p'));
	my ($ho, $he) = run($script->('h'));
	unlike($pe, qr/ERROR/, 'targeted: a cursor on buffered rows, then writes under memory pressure: no error');
	my ($pc) = $po =~ /CUR=(\S+)/;
	my ($hc) = $ho =~ /CUR=(\S+)/;
	is($pc, $hc, 'targeted: the cursor returned the rows it began with, once each, as the heap twin\'s did');
	is($pc, '600:180300:600', '... 600 rows');
	my ($pt) = $po =~ /TAB=(\S+)/;
	my ($ht) = $ho =~ /TAB=(\S+)/;
	is($pt, $ht, 'targeted: the table has all the rows, as the heap twin has');
	my ($pf) = $po =~ /FIN=(\S+)/;
	my ($hf) = $ho =~ /FIN=(\S+)/;
	is($pf, $hf, 'targeted: and after the commit');
}

# ------------------------------------------------------------------------
# Parallel query while the leader holds buffered rows: the answer is the heap
# twin's or an error that names the parallel worker, never a count that misses
# the rows only the leader has (the scan refuses to run in a worker)
# ------------------------------------------------------------------------
{
	$node->safe_psql('postgres', 'TRUNCATE p, h');
	$node->safe_psql('postgres', "INSERT INTO p SELECT g, repeat('z', 100) FROM generate_series(1, 20000) g; INSERT INTO h SELECT g, repeat('z', 100) FROM generate_series(1, 20000) g");
	my $script = sub
	{
		my ($t) = @_;
		return "
SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; SET min_parallel_table_scan_size = 0;
SET max_parallel_workers_per_gather = 2; SET debug_parallel_query = on;
BEGIN;
INSERT INTO $t SELECT g + 100000, repeat('y', 100) FROM generate_series(1, 700) g;
SELECT 'PAR=' || count(*) || ':' || sum(id) FROM $t;
SELECT 'PAR2=' || count(*) FROM $t WHERE id % 3 = 0;
COMMIT;";
	};
	my ($po, $pe) = run($script->('p'));
	my ($ho, $he) = run($script->('h'));
	my ($hc) = $ho =~ /PAR=(\S+)/;
	my ($pc) = $po =~ /PAR=(\S+)/;
	if ($pe =~ /ERROR/)
	{
		like($pe, qr/parallel worker/, 'parallel query with buffered rows in the leader: refused with an error that names the parallel worker');
	}
	else
	{
		is($pc, $hc, 'parallel query with buffered rows in the leader: the answer is the heap twin\'s');
		my ($pc2) = $po =~ /PAR2=(\S+)/;
		my ($hc2) = $ho =~ /PAR2=(\S+)/;
		is($pc2, $hc2, '... also with a filter');
	}
	unlike($po . $pe, qr/TRAP|server closed/, '... and the server stays up');
}

# ------------------------------------------------------------------------
# The ceiling of a hand-over (scripted: random statements do not reach it).
# Allowance 1 MB, so a ceiling of 2 MB.  Three levels hold 1.0, 0.5 and 0.5 MB
# (each below what writing is worth); a cursor holds their rows, so the fourth
# level's row cannot be merged into the third level's buffer nor written; the
# write itself passes (the ceiling is checked before a row is added) and the
# release of the level is refused: it would leave 2.11 MB in the parent.
# ------------------------------------------------------------------------
my %scripted;
{
	$node->safe_psql('postgres', q{
DROP TABLE IF EXISTS c;
CREATE TABLE c (id int, pad text) USING columnar;
SELECT kingres.columnar_set_options('c', stripe_row_count => 1000000);});
	my ($out, $err) = run("
SET kingres_columnar.write_buffer_memory = '1MB';
BEGIN;
INSERT INTO c SELECT g, repeat('x', 100000) FROM generate_series(1, 10) g;
SAVEPOINT s1;
INSERT INTO c SELECT g, repeat('x', 100000) FROM generate_series(1, 5) g;
SAVEPOINT s2;
INSERT INTO c SELECT g, repeat('x', 100000) FROM generate_series(1, 5) g;
DECLARE cur CURSOR FOR SELECT count(*) FROM c;
FETCH 1 FROM cur;
SAVEPOINT s3;
INSERT INTO c SELECT g, repeat('x', 100000) FROM generate_series(1, 1) g;
RELEASE s3;
ROLLBACK;
$PATHS_SQL");
	like($err, qr/would hold \d+ bytes after this subtransaction, more than the limit of 2097152 bytes/,
		'scripted: the release of a level that would leave more than twice the allowance is refused');
	count_paths($out, \%scripted);
	cmp_ok($scripted{ceiling_handover} // 0, '==', 1, '... and the path counter saw it');
}

# ------------------------------------------------------------------------
# The scripted prelude: every path the file names, taken once by construction.
# Each script runs in a session of its own (the counters are the session's), on
# columnar tables only; the result of each is checked against the number of rows it
# wrote.  The sizes are those that reach the path with the allowance of 1 MB and the
# stripe size of 1000 rows of table p (the numbers were measured; the accounting is
# deterministic).
# ------------------------------------------------------------------------
{
	my $set = "SET kingres_columnar.write_buffer_memory = '1MB';\n";
	my @prelude = (
		[ 'size_flush', "BEGIN;
INSERT INTO p SELECT g, repeat('f', 20) FROM generate_series(1, 3500) g;
COMMIT;" ],
		[ 'pressure_flush', "BEGIN;
INSERT INTO p SELECT g, repeat('a', 1000) FROM generate_series(1, 3000) g;
COMMIT;" ],
		[ 'own_flush', "BEGIN;
INSERT INTO p VALUES (1, 'a');
DO \$\$ BEGIN BEGIN INSERT INTO p SELECT g + 10, repeat('b', 280000) FROM generate_series(1, 2) g; EXCEPTION WHEN raise_exception THEN NULL; END; END \$\$;
COMMIT;" ],
		[ 'handover_flush', "BEGIN;
INSERT INTO p SELECT g, repeat('c', 20) FROM generate_series(1, 900) g;
SAVEPOINT s;
INSERT INTO p SELECT g + 5000, repeat('c', 20) FROM generate_series(1, 200) g;
RELEASE s;
COMMIT;" ],
		[ 'late_commit', "BEGIN;
SET LOCAL kcprobe.commit_sql = 'INSERT INTO q SELECT g, \$\$w\$\$ FROM generate_series(1, 3) g';
INSERT INTO p VALUES (1, 'a');
COMMIT;" ],
		# the window between the two phases of a hand-over: a COPY into a table made in the
		# transaction meets a prepared buffer (the script of t/022): late_window and prepared_skip
		[ 'late_window', "BEGIN;
CREATE TABLE r (id int, pad text) USING columnar;
SELECT kingres.columnar_set_options('r', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO r SELECT g, 'x' FROM generate_series(1, 999) g;
SAVEPOINT s;
INSERT INTO r VALUES (10001, 'x');
SET kcprobe.sql = 'COPY r (id) FROM PROGRAM \$\$seq 20001 20007\$\$';
RELEASE s;
SET kcprobe.sql = '';
COMMIT;" ],
	);
	foreach my $pr (@prelude)
	{
		my ($path, $sql) = @$pr;
		$node->safe_psql('postgres', 'TRUNCATE p, h, q; DROP TABLE IF EXISTS r');
		my ($out, $err) = run($set . $sql . "\n" . $PATHS_SQL);
		unlike($err, qr/ERROR|TRAP|server closed/, "prelude: the statements for $path run without an error");
		my %one;
		count_paths($out, \%one);
		$scripted{$_} += $one{$_} foreach keys %one;
		cmp_ok($one{$path} // 0, '>', 0, "prelude: $path was taken (" . ($one{$path} // 0) . " times)");
	}
	# the window script reaches the guard that keeps prepared buffers from being flushed too
	cmp_ok($scripted{prepared_skip} // 0, '>', 0, 'prelude: prepared_skip was taken (the guard of a prepared hand-over)');
	$node->safe_psql('postgres', 'DROP TABLE IF EXISTS r');

	# the ceiling of a write: nested savepoints of wide rows that nothing can write
	{
		$node->safe_psql('postgres', 'TRUNCATE p, h, q');
		my $sql = "BEGIN;\n"
			. join('', map { "SAVEPOINT s$_;\nINSERT INTO p SELECT g + $_ * 10, repeat('x', 250000) FROM generate_series(1, 2) g;\n" } 1 .. 10)
			. "ROLLBACK;\n";
		my ($out, $err) = run($set . $sql . $PATHS_SQL);
		like($err, qr/the write buffers of columnar tables hold \d+ bytes, more than the limit of 2097152 bytes/, 'prelude: nested savepoints of wide rows reach the limit of the write buffers');
		my %one;
		count_paths($out, \%one);
		$scripted{$_} += $one{$_} foreach keys %one;
		cmp_ok($one{ceiling_write} // 0, '>', 0, 'prelude: ceiling_write was taken (' . ($one{ceiling_write} // 0) . ' times)');
	}
}

# ------------------------------------------------------------------------
# The random episodes
# ------------------------------------------------------------------------
my @widths = (20, 300, 4000, 30000);
my @pwidths = (300, 3000, 40000, 120000, 300000);
my $differences = 0;
my $refused = 0;
my $checks = 0;
my %profiles;

# the statement each probe setting runs, over the tables of the episode
my @probe_sql = (
	"INSERT INTO q SELECT g, repeat(\$\$w\$\$, 50) FROM generate_series(1, 3) g; INSERT INTO hq SELECT g, repeat(\$\$w\$\$, 50) FROM generate_series(1, 3) g",
	"INSERT INTO p VALUES (-1, \$\$w\$\$); INSERT INTO h VALUES (-1, \$\$w\$\$)",
	"COPY r (id) FROM PROGRAM \$\$seq 1 20\$\$; COPY hr (id) FROM PROGRAM \$\$seq 1 20\$\$",
	"INSERT INTO r SELECT g, repeat(\$\$v\$\$, 200) FROM generate_series(1, 30) g; INSERT INTO hr SELECT g, repeat(\$\$v\$\$, 200) FROM generate_series(1, 30) g",
);

sub episode
{
	my ($n, $profile) = @_;
	my @sql;
	my @sp;							# open savepoints, by number
	my @curs;						# open cursors: [ name, depth when opened ]
	my $ncur = 0;
	my $nsp = 0;
	my $next = 1;
	my $chk = 0;
	my $pressure = $profile ne 'normal';
	my $steps = $profile eq 'ceiling' ? 6 + int(rand(8)) : 8 + int(rand(18));
	my %drained;
	# Rows a cursor returns without ORDER BY come in the order of the table's stripes
	# and buffers, which is not the order heap has (memory pressure writes the largest
	# buffer first), so cursors are compared as sets, and only when every fetch of the
	# cursor is still logged at the end: a ROLLBACK TO a savepoint older than a fetch
	# takes the log of that fetch away (not the position of the cursor).
	my %fetched_at;					# cursor number => depths at which it was fetched
	my %tainted;
	my $maxsp = $pressure ? 6 : 3;
	my $maxcur = $pressure ? 3 : 4;
	my @w = $pressure ? @pwidths : @widths;
	my $budget = $pressure ? 700000 : 300000;

	push @sql, "SET kingres_columnar.write_buffer_memory = '" . ($pressure ? '1MB' : '2MB') . "';";
	push @sql, "DROP TABLE IF EXISTS r, hr;";
	push @sql, "TRUNCATE p, h, q, hq;";
	push @sql, "CREATE TEMP TABLE fl (tag text, id int);";
	push @sql, "BEGIN;";
	# a table made in the transaction: a COPY into it flushes it (the bulk-insert end)
	push @sql, "CREATE TABLE r (id int, pad text) USING columnar; CREATE TABLE hr (id int, pad text);";
	if ($profile eq 'pressure')
	{
		# the probe writes at the end of every subtransaction and at the pre-commit
		push @sql, "SET LOCAL kcprobe.sql = '" . $probe_sql[int(rand(@probe_sql))] . "';" if rand() < 0.6;
		push @sql, "SET LOCAL kcprobe.commit_sql = '" . $probe_sql[int(rand(@probe_sql))] . "';" if rand() < 0.6;
	}
	my $both = sub
	{
		my ($f) = @_;
		my $pair = rand() < 0.75 ? ['p', 'h'] : ['r', 'hr'];
		return join(' ', map { $f->($_) } @$pair);
	};
	my $check = sub
	{
		$chk++;
		my $one = sub { my ($t) = @_; return "(SELECT count(*) || ':' || coalesce(sum(id), 0) || ':' || coalesce(sum(length(pad)), 0) FROM $t)"; };
		push @sql, "SELECT 'CHK$chk p=' || " . join(" || '/' || ", map { $one->($_) } qw(p r q)) . " || ' h=' || " . join(" || '/' || ", map { $one->($_) } qw(h hr hq)) . ";";
	};
	my $drain = sub
	{
		my ($c) = @_;
		$drained{$c->[0]} = 1;
		push @{ $fetched_at{$c->[0]} }, scalar(@sp);
		push @sql, "SELECT fetchlog('cp$c->[0]', 1000000, 'cp$c->[0]'), fetchlog('ch$c->[0]', 1000000, 'ch$c->[0]');";
		push @sql, "CLOSE cp$c->[0]; CLOSE ch$c->[0];";
	};

	for my $s (1 .. $steps)
	{
		my $r = rand();
		if ($profile eq 'ceiling')
		{
			# deeper and deeper, each level a few hundred kB: the ceiling ends the episode
			my $width = 250000;
			my $rows = 1 + int(rand(2));
			my $base = $next;
			$next += $rows;
			$nsp++;
			push @sp, $nsp;
			push @sql, "SAVEPOINT s$nsp;";
			push @sql, $both->(sub { "INSERT INTO $_[0] SELECT g + $base, repeat('x', $width) FROM generate_series(1, $rows) g;" });
			next;
		}
		# (as the first step of an episode the memory is empty, so the child's commit is
		# what writes the big buffer; later the memory pressure writes it before that)
		if ($pressure && ($s == 1 ? rand() < 0.5 : rand() < 0.12))
		{
			# a subtransaction whose commit has something to do: one big child (half the
			# allowance and more, which the commit writes itself), or a child that writes
			# the table made in the transaction (so that a COPY into it, run between the
			# phases of the hand-over, meets a prepared buffer)
			my $base = $next;
			if (rand() < 0.5)
			{
				# the parent holds a row of the table first: only then is the child's buffer
				# merged into the parent's, and a big one written by the child itself
				$next += 3;
				push @sql, "INSERT INTO p VALUES ($base, \$\$a\$\$); INSERT INTO h VALUES ($base, \$\$a\$\$);";
				push @sql, "DO \$\$ BEGIN BEGIN "
					. join(' ', map { "INSERT INTO $_ SELECT g + $base, repeat('b', 280000) FROM generate_series(1, 2) g;" } 'p', 'h')
					. " EXCEPTION WHEN raise_exception THEN NULL; END; END \$\$;";
			}
			else
			{
				$next += 5;
				push @sql, "DO \$\$ BEGIN BEGIN "
					. join(' ', map { "INSERT INTO $_ SELECT g + $base, repeat('r', 100) FROM generate_series(1, 5) g;" } 'r', 'hr')
					. " EXCEPTION WHEN raise_exception THEN NULL; END; END \$\$;";
			}
		}
		elsif ($r < 0.30)
		{
			my $width = $w[int(rand(@w))];
			my $rows = 1 + int(rand(int($budget / $width) < 1500 ? int($budget / $width) : 1500));
			my $base = $next;
			$next += $rows;
			push @sql, $both->(sub { "INSERT INTO $_[0] SELECT g + $base, repeat('x', $width) FROM generate_series(1, $rows) g;" });
		}
		elsif ($r < 0.38)
		{
			push @sql, "INSERT INTO p SELECT id + 1000000, pad FROM p WHERE id % 5 = 0; INSERT INTO h SELECT id + 1000000, pad FROM h WHERE id % 5 = 0;";
		}
		elsif ($r < 0.48 && @sp < $maxsp)
		{
			$nsp++;
			push @sp, $nsp;
			push @sql, "SAVEPOINT s$nsp;";
		}
		elsif ($r < 0.56 && @sp)
		{
			my $k = int(rand(@sp));				# index into @sp
			push @sql, "ROLLBACK TO s$sp[$k];";
			# the fetches made after that savepoint was made are not logged any more
			foreach my $cn (keys %fetched_at)
			{
				$tainted{$cn} = 1 if grep { $_ > $k } @{ $fetched_at{$cn} };
			}
			# the subtransactions after s$sp[$k] are gone, and the cursors opened since it was made
			splice(@sp, $k + 1);
			@curs = grep { $_->[1] <= $k } @curs;
		}
		elsif ($r < 0.62 && @sp)
		{
			my $k = int(rand(@sp));
			push @sql, "RELEASE s$sp[$k];";
			splice(@sp, $k);
		}
		elsif ($r < 0.72)
		{
			my $width = $w[int(rand(@w))];
			my $rows = 1 + int(rand(int(($budget * 2 / 3) / $width) < 800 ? int(($budget * 2 / 3) / $width) : 800));
			my $base = $next;
			$next += $rows;
			my $fail = rand() < 0.5 ? 'true' : 'false';
			push @sql, "DO \$\$ BEGIN BEGIN "
				. join(' ', map { "INSERT INTO $_ SELECT g + $base, repeat('e', $width) FROM generate_series(1, $rows) g;" } 'p', 'h')
				. " IF $fail THEN RAISE EXCEPTION 'x'; END IF; EXCEPTION WHEN raise_exception THEN NULL; END; END \$\$;";
		}
		elsif ($r < 0.80 && @curs < $maxcur)
		{
			$ncur++;
			push @curs, [ $ncur, scalar(@sp) ];
			push @sql, "DECLARE cp$ncur CURSOR FOR SELECT id FROM p; DECLARE ch$ncur CURSOR FOR SELECT id FROM h;";
		}
		elsif ($r < 0.90 && @curs)
		{
			my $c = $curs[int(rand(@curs))];
			my $k = 1 + int(rand(400));
			push @{ $fetched_at{$c->[0]} }, scalar(@sp);
			push @sql, "SELECT fetchlog('cp$c->[0]', $k, 'cp$c->[0]'), fetchlog('ch$c->[0]', $k, 'ch$c->[0]');";
		}
		elsif ($r < 0.94 && @curs)
		{
			my $i = int(rand(@curs));
			my ($c) = splice(@curs, $i, 1);
			$drain->($c);
		}
		else
		{
			$check->();
		}
	}
	$check->();
	foreach my $c (@curs) { $drain->($c); }
	$check->();
	my $commit = rand() < 0.7;
	push @sql, $commit ? "COMMIT;" : "ROLLBACK;";
	my $fin = sub { my ($t) = @_; return "(SELECT count(*) || ':' || coalesce(sum(id), 0) || ':' || coalesce(md5(string_agg(id || '/' || length(pad), ',' ORDER BY id, length(pad))), '') FROM $t)"; };
	push @sql, "SELECT 'FIN p=' || " . join(" || '/' || ", map { $fin->($_) } qw(p q)) . " || ' h=' || " . join(" || '/' || ", map { $fin->($_) } qw(h hq)) . ";";
	push @sql, "SELECT 'LOG ' || tag || ' ' || count(*) || ':' || sum(id) || ':' || md5(string_agg(id::text, ',' ORDER BY id)) FROM fl GROUP BY tag ORDER BY tag;";
	push @sql, $PATHS_SQL;
	delete @drained{ keys %tainted };
	return (join("\n", @sql), \%drained);
}

for my $n (1 .. $episodes)
{
	my $x = rand();
	my $profile = $x < 0.40 ? 'normal' : $x < 0.88 ? 'pressure' : 'ceiling';
	$profiles{$profile}++;
	my ($script, $drained) = episode($n, $profile);
	my ($out, $err) = run($script);
	count_paths($out, \%paths);
	if ($ENV{KC_FUZZ_TRACE})
	{
		my ($pl) = $out =~ /^PATHS (.*)$/m;
		diag("episode $n ($profile): " . ($script =~ /repeat\('b', 280000\)/ ? 'BIG ' : '') . ($pl // 'no paths line') . ($err =~ /ERROR/ ? ' ERROR' : ''));
	}
	if ($err =~ /write buffers of columnar tables/)
	{
		# the limit of the buffers' memory is an answer the heap twin does not have
		$refused++;
		next;
	}
	if ($err =~ /ERROR/)
	{
		$differences++;
		diag("episode $n ($profile) failed:\n$err\n--- script ---\n$script");
		next;
	}
	while ($out =~ /^CHK(\d+) p=(\S+) h=(\S+)/mg) { $checks++; $differences++, diag("episode $n ($profile) check $1: columnar $2, heap $3\n$script") if $2 ne $3; }
	if ($out =~ /^FIN p=(\S+) h=(\S+)/m) { $checks++; $differences++, diag("episode $n ($profile) final: columnar $1, heap $2\n$script") if $1 ne $2; }
	my %log;
	while ($out =~ /^LOG c([ph])(\d+) (\S+)/mg) { $log{$2}{$1} = $3; }
	foreach my $c (grep { $drained->{$_} } keys %log)
	{
		$checks++;
		if (($log{$c}{p} // 'none') ne ($log{$c}{h} // 'none'))
		{
			$differences++;
			diag("episode $n ($profile) cursor $c: columnar " . ($log{$c}{p} // 'none') . ", heap " . ($log{$c}{h} // 'none') . "\n$script");
		}
	}
}
diag('profiles: ' . join(' ', map { "$_=$profiles{$_}" } sort keys %profiles));
diag('paths taken: ' . join(' ', map { "$_=" . ($paths{$_} // 0) } sort grep { $_ ne 'bytes_held' && $_ ne 'orphans_kept' } keys %paths));

is($differences, 0, "$episodes episodes of savepoints, cursors and writes under memory pressure: no difference from the heap twin in $checks comparisons");
cmp_ok($refused, '<=', int($episodes * 0.4), "... and at most 40 percent of the episodes ran into the limit of the buffers' memory ($refused; the ceiling profile does by design)");
cmp_ok($checks, '>=', $episodes, '... and the episodes did compare something');

# the paths the file claims to reach: each is taken by the prelude (asserted above, by
# construction); how often the random episodes of this run took it is reported, because it
# says whether the generator still reaches the paths on average, and is not asserted: the
# count of a path that is rare in a random run (prepared_skip: 0 in about one run in thirty)
# is a property of the seed and not of the code
foreach my $path (qw(late_commit late_window pressure_flush own_flush handover_flush size_flush prepared_skip ceiling_write))
{
	my $hits = $paths{$path} // 0;
	my $by = $scripted{$path} // 0;
	diag("path $path: the prelude $by times, the random episodes $hits times" . ($hits == 0 ? ' (REPORT: the random part of this run did not take it)' : ''));
	cmp_ok($by, '>', 0, "path $path is taken by the scripted prelude ($by times; the random part took it $hits times)");
}

$node->stop;
done_testing();
