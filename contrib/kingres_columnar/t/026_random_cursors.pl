# contrib/kingres_columnar/t/026_random_cursors.pl
#
# C6-07.  A seeded, deterministic comparison with a heap twin of what cursors,
# savepoints, exception blocks and writes do together.  Every episode is one
# transaction in which the same statements are run against a columnar table and
# a heap table: inserts of random width and number (under a small allowance, so
# that memory pressure writes stripes while cursors are open), inserts from the
# table itself, savepoints, rollbacks to them and releases, blocks that fail,
# cursors with partial fetches, and checks.  The rows a cursor returns, taken
# over its whole life, and every check of the table must be those of the heap
# twin.  The seed is printed (and can be set with KC_FUZZ_SEED).
#
# It is the test that finds a buffer that is flushed under an open cursor (the
# cursor then misses rows or reads some twice): no deterministic test of the
# earlier rounds did.  A targeted case of the same defect comes first.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $seed = defined $ENV{KC_FUZZ_SEED} ? $ENV{KC_FUZZ_SEED} : 20261117;
my $episodes = defined $ENV{KC_FUZZ_EPISODES} ? $ENV{KC_FUZZ_EPISODES} : 70;
diag("KC_FUZZ_SEED=$seed KC_FUZZ_EPISODES=$episodes");
srand($seed);

my $node = PostgreSQL::Test::Cluster->new('colrandcur');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
});
$node->start;
$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE TABLE p (id int, pad text) USING columnar;
SELECT kingres.columnar_set_options('p', stripe_row_count => 1000, chunk_row_count => 100);
CREATE TABLE h (id int, pad text);
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
# The random episodes
# ------------------------------------------------------------------------
my @widths = (20, 300, 4000, 30000);
my $differences = 0;
my $refused = 0;
my $checks = 0;

sub episode
{
	my ($n) = @_;
	my @sql;
	my @sp;							# open savepoints, by number
	my @curs;						# open cursors: [ name, depth when opened ]
	my $ncur = 0;
	my $nsp = 0;
	my $next = 1;
	my $chk = 0;
	my $steps = 8 + int(rand(18));
	my %drained;

	push @sql, "SET kingres_columnar.write_buffer_memory = '2MB';";
	push @sql, "TRUNCATE p, h;";
	push @sql, "CREATE TEMP TABLE fl (tag text, id int);";
	push @sql, "BEGIN;";
	my $both = sub { my ($f) = @_; return join(' ', map { $f->($_) } 'p', 'h'); };
	my $check = sub
	{
		$chk++;
		push @sql, "SELECT 'CHK$chk p=' || (SELECT count(*) || ':' || coalesce(sum(id), 0) || ':' || coalesce(sum(length(pad)), 0) FROM p) || ' h=' || (SELECT count(*) || ':' || coalesce(sum(id), 0) || ':' || coalesce(sum(length(pad)), 0) FROM h);";
	};
	my $drain = sub
	{
		my ($c) = @_;
		$drained{$c->[0]} = 1;
		push @sql, "SELECT fetchlog('cp$c->[0]', 1000000, 'cp$c->[0]'), fetchlog('ch$c->[0]', 1000000, 'ch$c->[0]');";
		push @sql, "CLOSE cp$c->[0]; CLOSE ch$c->[0];";
	};

	for my $s (1 .. $steps)
	{
		my $r = rand();
		if ($r < 0.30)
		{
			my $w = $widths[int(rand(@widths))];
			my $rows = 1 + int(rand(int(300000 / $w) < 1500 ? int(300000 / $w) : 1500));
			my $base = $next;
			$next += $rows;
			push @sql, $both->(sub { "INSERT INTO $_[0] SELECT g + $base, repeat('x', $w) FROM generate_series(1, $rows) g;" });
		}
		elsif ($r < 0.38)
		{
			push @sql, $both->(sub { "INSERT INTO $_[0] SELECT id + 1000000, pad FROM $_[0] WHERE id % 5 = 0;" });
		}
		elsif ($r < 0.48 && @sp < 3)
		{
			$nsp++;
			push @sp, $nsp;
			push @sql, "SAVEPOINT s$nsp;";
		}
		elsif ($r < 0.56 && @sp)
		{
			my $k = int(rand(@sp));				# index into @sp
			push @sql, "ROLLBACK TO s$sp[$k];";
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
			my $w = $widths[int(rand(@widths))];
			my $rows = 1 + int(rand(int(200000 / $w) < 800 ? int(200000 / $w) : 800));
			my $base = $next;
			$next += $rows;
			my $fail = rand() < 0.5 ? 'true' : 'false';
			push @sql, "DO \$\$ BEGIN BEGIN "
				. join(' ', map { "INSERT INTO $_ SELECT g + $base, repeat('e', $w) FROM generate_series(1, $rows) g;" } 'p', 'h')
				. " IF $fail THEN RAISE EXCEPTION 'x'; END IF; EXCEPTION WHEN raise_exception THEN NULL; END; END \$\$;";
			$next += 0;
		}
		elsif ($r < 0.80 && @curs < 4)
		{
			$ncur++;
			push @curs, [ $ncur, scalar(@sp) ];
			push @sql, "DECLARE cp$ncur CURSOR FOR SELECT id FROM p; DECLARE ch$ncur CURSOR FOR SELECT id FROM h;";
		}
		elsif ($r < 0.90 && @curs)
		{
			my $c = $curs[int(rand(@curs))];
			my $k = 1 + int(rand(400));
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
	push @sql, "SELECT 'FIN p=' || (SELECT count(*) || ':' || coalesce(sum(id), 0) || ':' || coalesce(md5(string_agg(id || '/' || length(pad), ',' ORDER BY id, length(pad))), '') FROM p) || ' h=' || (SELECT count(*) || ':' || coalesce(sum(id), 0) || ':' || coalesce(md5(string_agg(id || '/' || length(pad), ',' ORDER BY id, length(pad))), '') FROM h);";
	push @sql, "SELECT 'LOG ' || tag || ' ' || count(*) || ':' || sum(id) || ':' || md5(string_agg(id::text, ',' ORDER BY id)) FROM fl GROUP BY tag ORDER BY tag;";
	return (join("\n", @sql), \%drained);
}

for my $n (1 .. $episodes)
{
	my ($script, $drained) = episode($n);
	my ($out, $err) = run($script);
	if ($err =~ /write buffers of columnar tables/)
	{
		# the limit of the buffers' memory is an answer the heap twin does not have
		$refused++;
		next;
	}
	if ($err =~ /ERROR/)
	{
		$differences++;
		diag("episode $n failed:\n$err\n--- script ---\n$script");
		next;
	}
	my %chk;
	while ($out =~ /^CHK(\d+) p=(\S+) h=(\S+)/mg) { $checks++; $differences++, diag("episode $n check $1: columnar $2, heap $3\n$script") if $2 ne $3; }
	if ($out =~ /^FIN p=(\S+) h=(\S+)/m) { $checks++; $differences++, diag("episode $n final: columnar $1, heap $2\n$script") if $1 ne $2; }
	my %log;
	while ($out =~ /^LOG c([ph])(\d+) (\S+)/mg) { $log{$2}{$1} = $3; }
	foreach my $c (grep { $drained->{$_} } keys %log)
	{
		$checks++;
		if (($log{$c}{p} // 'none') ne ($log{$c}{h} // 'none'))
		{
			$differences++;
			diag("episode $n cursor $c: columnar " . ($log{$c}{p} // 'none') . ", heap " . ($log{$c}{h} // 'none') . "\n$script");
		}
	}
}
is($differences, 0, "$episodes episodes of savepoints, cursors and writes under memory pressure: no difference from the heap twin in $checks comparisons");
cmp_ok($refused, '<=', int($episodes / 10), "... and at most a tenth of the episodes ran into the limit of the buffers' memory ($refused)");
cmp_ok($checks, '>=', $episodes * 3, '... and the episodes did compare something');

$node->stop;
done_testing();
