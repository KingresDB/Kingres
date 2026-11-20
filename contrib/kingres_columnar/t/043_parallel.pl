# contrib/kingres_columnar/t/043_parallel.pl
#
# The parallel Columnar Scan (C5, design 8.4; the design calls this test
# 005_parallel, a number the corruption test already has).
#
#   1. The same queries over a columnar table of 80 stripes and over its heap
#      twin give the same rows, with 0, 1 and 4 workers, with and without the
#      leader taking part, with and without debug_parallel_query: count, sums,
#      a checksum of every row, skipping, projection, a join of the table with
#      itself.
#   2. The workers really scan: EXPLAIN (ANALYZE, BUFFERS) shows the workers
#      that read stripes, their counts add up to the stripes of the table, and
#      the skipped stripes are counted across them.
#   3. A rescan: the parallel scan as the inner side of a nested loop is read
#      again for every outer row and gives the same answer.
#   4. A writer commits while parallel queries run: every query sees whole
#      transactions, no row twice, no row missing.
#   5. The leader's own rows: written in the transaction before the query, in
#      the savepoint that runs it, in an enclosing savepoint (no parallel plan),
#      and a plan made earlier that meets them (an error, never a wrong answer).
#   6. CREATE TABLE AS, a cursor, INSERT ... SELECT from the same table.
#   7. A hot standby gives the same answers.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use IPC::Run;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $primary = PostgreSQL::Test::Cluster->new('colpar');
$primary->init(allows_streaming => 1);
$primary->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
max_connections = 40
max_worker_processes = 24
max_parallel_workers = 16
autovacuum = off
});
$primary->start;

my $STRIPES = 80;
$primary->safe_psql('postgres', q{
	CREATE EXTENSION kingres_columnar;
	CREATE TABLE c (id bigint, g int, v numeric(12,2), t text, d date) USING columnar;
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 250);
	CREATE TABLE h (id bigint, g int, v numeric(12,2), t text, d date);
	CREATE TABLE s5 (g int);
	INSERT INTO s5 SELECT generate_series(1, 5);
});
for my $part (0 .. 7)
{
	my ($lo, $hi) = ($part * 10000 + 1, ($part + 1) * 10000);
	my $ins = qq{SELECT i, i % 10, (i % 997) * 1.25, CASE WHEN i % 13 = 0 THEN NULL ELSE 'r' || i || repeat('z', i % 30) END,
	                    date '2021-01-01' + (i % 500) FROM generate_series($lo, $hi) i};
	$primary->safe_psql('postgres', "INSERT INTO c $ins; INSERT INTO h $ins;");
}
$primary->safe_psql('postgres', 'ANALYZE c; ANALYZE h; ANALYZE s5;');
is($primary->safe_psql('postgres', "SELECT stripes FROM kingres.columnar_info('c')"),
	$STRIPES, "the table has $STRIPES stripes");

# ---------------------------------------------------------------------------
# 1. Same answers
# ---------------------------------------------------------------------------

# Queries over %T (the table); each prints one line.
my @queries = (
	q{SELECT 'q1 ' || count(*) || ' ' || sum(id) || ' ' || sum(g) || ' ' || sum(v) || ' ' || sum(length(t)) || ' ' || min(d) || ' ' || max(d) FROM %T},
	q{SELECT 'q2 ' || sum(hashtext(x::text)) FROM %T x},
	q{SELECT 'q3 ' || count(*) || ' ' || sum(id) FROM %T WHERE id BETWEEN 30000 AND 41000},
	q{SELECT 'q4 ' || sum(g) || ' ' || count(t) FROM %T WHERE g % 3 = 0},
	q{SELECT 'q5 ' || string_agg(g || ':' || n, ',' ORDER BY g) FROM (SELECT g, count(*) n FROM %T GROUP BY g) s},
	q{SELECT 'q6 ' || count(*) || ' ' || sum(a.id) FROM %T a JOIN %T b USING (id) WHERE a.g = 3},
	q{SELECT 'q7 ' || count(*) FROM %T WHERE t IS NULL},
);

sub script_for
{
	my ($table, $gucs) = @_;
	my $s = "SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; SET min_parallel_table_scan_size = 0;\n$gucs\n";
	for my $q (@queries)
	{
		(my $x = $q) =~ s/%T/$table/g;
		$s .= "$x;\n";
	}
	return $s;
}

# Run a script on a node; returns stdout and stderr.
sub run_script
{
	my ($node, $sql) = @_;
	my ($out, $err) = ('', '');
	IPC::Run::run([ 'psql', '-X', '-At', '-q', '-d', $node->connstr('postgres') ],
		'<', \$sql, '>', \$out, '2>', \$err);
	return ($out, $err);
}

my ($truth, $terr) = run_script($primary, script_for('h', 'SET max_parallel_workers_per_gather = 0;'));
is($terr, '', 'the heap twin answers without errors');
my ($serial) = run_script($primary, script_for('c', 'SET max_parallel_workers_per_gather = 0;'));
is($serial, $truth, 'serial scan of the columnar table: the heap twin\'s answers');

my @variants;
for my $w (0, 1, 4)
{
	for my $lead ('on', 'off')
	{
		for my $dbg ('off', 'on')
		{
			push @variants,
			  [ "workers $w, leader $lead, debug_parallel_query $dbg",
				"SET max_parallel_workers_per_gather = $w; SET parallel_leader_participation = $lead; SET debug_parallel_query = $dbg;" ];
		}
	}
}
sub check_variants
{
	my ($node, $label) = @_;
	for my $v (@variants)
	{
		my ($out, $err) = run_script($node, script_for('c', $v->[1]));
		is($out, $truth, "$label: $v->[0]: the heap twin's answers")
		  or diag($err);
		is($err, '', "$label: $v->[0]: no error");
	}
}
check_variants($primary, 'primary');

# ---------------------------------------------------------------------------
# 2. The workers really scan
# ---------------------------------------------------------------------------

my $par = "SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; SET min_parallel_table_scan_size = 0; "
  . "SET max_parallel_workers_per_gather = 4; SET parallel_leader_participation = off;";

sub explain_analyze
{
	my ($node, $q, $extra) = @_;
	my ($out, $err) = run_script($node,
		"$par\n" . ($extra // '') . "\nEXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF, SUMMARY OFF) $q;\n");
	return ($out, $err);
}

{
	my ($plan, $err) = explain_analyze($primary, 'SELECT count(*), sum(g) FROM c');
	like($plan, qr/Parallel Custom Scan \(Columnar Scan\) on c/, 'the plan has a parallel Columnar Scan');
	my ($launched) = $plan =~ /Workers Launched: (\d+)/;
	cmp_ok($launched // 0, '>=', 2, "workers were launched ($launched)");
	my ($by) = $plan =~ /Columnar Stripes Read By: (.*)/;
	ok(defined $by, 'EXPLAIN ANALYZE says who read the stripes');
	my @w = $by =~ /worker (\d+) (\d+) \((\d+) rows\)/g;
	my ($sum, $rows, $active) = (0, 0, 0);
	while (my ($n, $stripes, $r) = splice(@w, 0, 3))
	{
		$sum += $stripes;
		$rows += $r;
		$active++ if $stripes > 0;
	}
	cmp_ok($active, '>=', 1, "workers read stripes ($by)");
	is($sum, $STRIPES, 'the workers read every stripe once (the leader takes no part)');
	is($rows, 80000, 'and returned every row once');
	like($by, qr/^leader 0 /, 'the leader read none');
	like($plan, qr/Parallel Custom Scan \(Columnar Scan\) on c.*?\n\s+Buffers: shared hit=\d+/s,
		'BUFFERS shows the pages the workers read');

	# two workers read stripes in at least one of a few runs (which worker is
	# fast enough to take a stripe is not decided by the scan)
	my $two = 0;
	for my $try (1 .. 6)
	{
		my ($p) = explain_analyze($primary, 'SELECT sum(length(t)), sum(v), count(DISTINCT t) FROM c');
		my ($b) = $p =~ /Columnar Stripes Read By: (.*)/;
		my $n = () = ($b // '') =~ /worker \d+ [1-9]\d* /g;
		if ($n >= 2) { $two = 1; last; }
	}
	ok($two, 'at least two workers read stripes of one scan');
}

# skipping: a range predicate skips most stripes, counted over all participants
{
	my ($plan) = explain_analyze($primary, 'SELECT count(*), sum(id) FROM c WHERE id BETWEEN 30000 AND 41000');
	my ($skipped, $total) = $plan =~ /Columnar Stripes Skipped: (\d+) of (\d+)/;
	is($total, $STRIPES, 'the stripe total counts the stripes of every participant');
	cmp_ok($skipped, '>=', 60, "the stripes outside the range are skipped ($skipped)");
	like($plan, qr/Columnar Skip Conditions: id/, 'the skip condition is shown in the parallel node');
	my ($out) = run_script($primary, "$par SELECT count(*), sum(id) FROM c WHERE id BETWEEN 30000 AND 41000;");
	is($out, "11001|" . (30000 + 41000) * 11001 / 2 . "\n", 'and the answer is complete');
}

# ---------------------------------------------------------------------------
# 3. A rescan
# ---------------------------------------------------------------------------

{
	my $nl = "SET enable_hashjoin = off; SET enable_mergejoin = off; SET enable_material = off; SET enable_memoize = off;";
	my $q = 'SELECT count(*), sum(c.id), sum(c.v) FROM s5 JOIN c ON c.g = s5.g';
	my ($plan, $err) = explain_analyze($primary, $q, $nl);
	like($plan, qr/Nested Loop.*?Gather.*?Parallel Custom Scan \(Columnar Scan\) on c/s,
		'the parallel scan is the inner side of a nested loop (Gather rescanned)');
	my ($launched) = $plan =~ /Workers Launched: (\d+)/;
	cmp_ok($launched // 0, '>=', 1, 'workers were launched for the inner side');
	my ($by) = $plan =~ /Columnar Stripes Read By: (.*)/;
	my ($loops) = $plan =~ /Gather.*?loops=(\d+)/s;
	my ($sum) = 0;
	$sum += $_ for ($by // '') =~ /(?:leader|worker \d+) (\d+) /g;
	is($sum, 5 * $STRIPES, 'five loops of the outer side read every stripe five times in all');
	my ($out) = run_script($primary, "$par $nl $q;");
	my ($want) = run_script($primary, "SET max_parallel_workers_per_gather = 0; SELECT count(*), sum(h.id), sum(h.v) FROM s5 JOIN h ON h.g = s5.g;");
	is($out, $want, 'the rescanned parallel scan gives the heap twin\'s answer');
	($out) = run_script($primary, "$par SET parallel_leader_participation = on; $nl $q;");
	is($out, $want, '... with the leader taking part');
}

# ---------------------------------------------------------------------------
# 4. A writer commits while parallel queries run
# ---------------------------------------------------------------------------

{
	my $K = 50;
	my $N = 400;
	$primary->safe_psql('postgres', q{
		CREATE TABLE w (id bigint, pad text) USING columnar;
		SELECT kingres.columnar_set_options('w', stripe_row_count => 1000, chunk_row_count => 250);
		INSERT INTO w SELECT i, md5(i::text) FROM generate_series(0, 4999) i;
	});
	my $dir = PostgreSQL::Test::Utils::tempdir;
	my $wsql = '';
	for my $k (0 .. $N - 1)
	{
		my $lo = 5000 + $k * $K;
		my $hi = $lo + $K - 1;
		$wsql .= "BEGIN;\nINSERT INTO w SELECT i, md5(i::text) FROM generate_series($lo, $hi) i;\n";
		$wsql .= ($k % 9 == 8) ? "ROLLBACK;\n" : "COMMIT;\n";
		# a rolled back transaction leaves a hole in the ids, which the check allows for below
	}
	my $rsql = "$par\nSET parallel_leader_participation = on;\n"
	  . "SELECT 'cnt ' || count(*) || ' ' || count(DISTINCT id) || ' ' || sum(id) || ' ' || max(id) FROM w;\n" x 150;
	open(my $fh, '>', "$dir/writer.sql") or die;
	print $fh $wsql;
	close($fh);
	open($fh, '>', "$dir/reader.sql") or die;
	print $fh $rsql;
	close($fh);
	my $wh = IPC::Run::start([ 'psql', '-X', '-At', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $primary->connstr('postgres'), '-f', "$dir/writer.sql" ],
		'>', "$dir/writer.out", '2>', "$dir/writer.err");
	my @rh;
	for my $i (1 .. 2)
	{
		push @rh, IPC::Run::start([ 'psql', '-X', '-At', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $primary->connstr('postgres'), '-f', "$dir/reader.sql" ],
			'>', "$dir/reader$i.out", '2>', "$dir/reader$i.err");
	}
	IPC::Run::finish($wh);
	IPC::Run::finish($_) for @rh;
	is($wh->result(0), 0, 'the writer finished');
	for my $i (1 .. 2)
	{
		is($rh[$i - 1]->result(0), 0, "reader $i finished") or diag(slurp_file("$dir/reader$i.err"));
		my @l = grep { /^cnt / } split /\n/, slurp_file("$dir/reader$i.out");
		is(scalar(@l), 150, "reader $i got all its answers");
		my ($bad, $dup, $nonmono, $prev, $growth) = (0, 0, 0, 0, 0);
		my %seen;
		for my $line (@l)
		{
			my (undef, $n, $d, $sum, $max) = split ' ', $line;
			$dup++ if $n != $d;
			$nonmono++ if $n < $prev;
			$prev = $n;
			$seen{$n} = 1;
			# whole transactions only: 5000 initial rows plus a multiple of K
			$bad++ if ($n - 5000) % $K != 0;
		}
		is("$dup/$nonmono/$bad", '0/0/0', "reader $i: no row twice, counts never go down, whole transactions only");
		cmp_ok(scalar(keys %seen), '>', 1, "reader $i saw the table grow while its queries ran");
	}
	my $committed = 0;
	$committed++ for grep { $_ % 9 != 8 } 0 .. $N - 1;
	is($primary->safe_psql('postgres', 'SELECT count(*) || \'|\' || count(DISTINCT id) FROM w'),
		(5000 + $committed * $K) . '|' . (5000 + $committed * $K), 'the end state has every committed row once');
	is($primary->safe_psql('postgres', "$par SELECT count(*) || '|' || count(DISTINCT id) FROM w"),
		(5000 + $committed * $K) . '|' . (5000 + $committed * $K), '... also read by workers');
}

# ---------------------------------------------------------------------------
# 5. The rows of the leader's transaction
# ---------------------------------------------------------------------------

{
	$primary->safe_psql('postgres', q{CREATE TABLE o (id int, pad text) USING columnar; SELECT kingres.columnar_set_options('o', stripe_row_count => 1000, chunk_row_count => 250); INSERT INTO o SELECT i, 'x' FROM generate_series(1, 5000) i; ANALYZE o;});
	my $q = "SELECT 'n ' || count(*) || ' ' || count(*) FILTER (WHERE id > 10000) || ' ' || coalesce(sum(id), 0) FROM o";
	# the transaction's own rows, then a parallel query
	my ($out, $err) = run_script($primary, "$par
BEGIN;
INSERT INTO o SELECT 10000 + i, 'own' FROM generate_series(1, 700) i;
EXPLAIN (COSTS OFF) SELECT count(*) FROM o;
$q;
SAVEPOINT a;
INSERT INTO o SELECT 20000 + i, 'sub' FROM generate_series(1, 300) i;
$q;
ROLLBACK TO a;
$q;
COMMIT;
$q;
");
	like($out, qr/Gather/, 'own rows: the query has a parallel plan');
	is($err, '', 'own rows: no error');
	my @n = $out =~ /^n (.*)$/mg;
	is($n[0] =~ /^5700 700 / ? 1 : 0, 1, 'the transaction sees its own 700 rows in a parallel scan');
	is($n[1] =~ /^6000 1000 / ? 1 : 0, 1, '... and those of the savepoint that runs the query');
	is($n[2] =~ /^5700 700 / ? 1 : 0, 1, '... and not those of a rolled back savepoint');
	is($n[3] =~ /^5700 700 / ? 1 : 0, 1, '... and after the commit');

	# rows of an enclosing level cannot go to workers: the plan has none, the answer is right
	($out, $err) = run_script($primary, "$par
BEGIN;
INSERT INTO o SELECT 30000 + i, 'outer' FROM generate_series(1, 400) i;
SAVEPOINT b;
INSERT INTO o SELECT 40000 + i, 'inner' FROM generate_series(1, 100) i;
EXPLAIN (COSTS OFF) SELECT count(*) FROM o;
SELECT 'm ' || count(*) FROM o;
ROLLBACK;
");
	unlike($out, qr/Gather/, 'rows of an enclosing savepoint: no parallel plan');
	like($out, qr/^m 6200$/m, '... and the answer has every row');
	is($err, '', '... without an error');

	# a plan made before meets them: an error that says so, not a wrong count
	($out, $err) = run_script($primary, "$par
SET plan_cache_mode = force_generic_plan;
PREPARE pq AS SELECT count(*) FROM o;
EXECUTE pq;
BEGIN;
INSERT INTO o SELECT 50000 + i, 'late' FROM generate_series(1, 10) i;
SAVEPOINT c1;
EXECUTE pq;
ROLLBACK;
EXECUTE pq;
");
	like($err, qr/has rows that this transaction wrote and parallel workers cannot read/,
		'a prepared parallel plan that meets rows of an enclosing savepoint fails with the message');
	like($out, qr/^5700$/m, 'the plan works again once they are gone');
	is(scalar(() = $out =~ /^\d+$/mg), 2, 'and no count was returned with the rows missing');
}

# ---------------------------------------------------------------------------
# 6. CREATE TABLE AS, a cursor, INSERT ... SELECT
# ---------------------------------------------------------------------------

{
	my ($out, $err) = run_script($primary, "$par
CREATE TABLE ctas_c USING columnar AS SELECT id, g, v FROM c WHERE id % 2 = 0;
CREATE TABLE ctas_h AS SELECT id, g, v FROM h WHERE id % 2 = 0;
SELECT 'ctas ' || (SELECT count(*) FROM ctas_c) || ' ' || (SELECT count(*) FROM ctas_h) || ' ' ||
       (SELECT sum(hashtext(x::text)) FROM ctas_c x) || ' ' || (SELECT sum(hashtext(x::text)) FROM ctas_h x);
BEGIN;
INSERT INTO ctas_c SELECT id + 1000000, g, v FROM ctas_c;
SELECT 'ins ' || count(*) FROM ctas_c;
INSERT INTO ctas_c SELECT id + 2000000, g, v FROM ctas_c WHERE id > 1000000;
SELECT 'ins2 ' || count(*) FROM ctas_c;
COMMIT;
BEGIN;
INSERT INTO ctas_c SELECT 9000000 + i, 1, 1 FROM generate_series(1, 50) i;
DECLARE cur CURSOR FOR SELECT id FROM ctas_c WHERE id > 9000000 ORDER BY id;
FETCH 3 FROM cur;
INSERT INTO ctas_c SELECT 9100000 + i, 1, 1 FROM generate_series(1, 50) i;
FETCH ALL FROM cur;
COMMIT;
");
	is($err, '', 'CREATE TABLE AS, INSERT ... SELECT and a cursor: no error');
	my ($c, $hc, $cs, $hs) = $out =~ /^ctas (\d+) (\d+) (-?\d+) (-?\d+)$/m;
	is("$c/$cs", "$hc/$hs", 'CREATE TABLE AS from a parallel scan into a columnar table: the heap twin\'s rows');
	like($out, qr/^ins 80000$/m, 'INSERT ... SELECT from the same table reads its old rows only');
	like($out, qr/^ins2 120000$/m, '... and the rows of the earlier command');
	my @ids = $out =~ /^(9\d{6})$/mg;
	is(scalar(@ids) . '/' . scalar(grep { $_ >= 9100000 } @ids), '50/0', 'the cursor returned its 50 rows and none written after it was opened');
	is($primary->safe_psql('postgres', 'SELECT count(*) FROM kingres.columnar_check()'), '0', 'columnar_check is clean');
}

# ---------------------------------------------------------------------------
# 7. A hot standby
# ---------------------------------------------------------------------------

{
	my $backup = 'parbackup';
	$primary->backup($backup);
	my $standby = PostgreSQL::Test::Cluster->new('colpar_s');
	$standby->init_from_backup($primary, $backup, has_streaming => 1);
	$standby->append_conf('postgresql.conf', "hot_standby = on\nmax_worker_processes = 24\nmax_parallel_workers = 16\n");
	$standby->start;
	$primary->wait_for_catchup($standby);
	check_variants($standby, 'standby');
	my ($plan) = explain_analyze($standby, 'SELECT count(*), sum(g) FROM c');
	my ($launched) = $plan =~ /Workers Launched: (\d+)/;
	cmp_ok($launched // 0, '>=', 1, 'workers were launched on the standby');
	my ($by) = $plan =~ /Columnar Stripes Read By: (.*)/;
	my $sum = 0;
	$sum += $_ for ($by // '') =~ /(?:leader|worker \d+) (\d+) /g;
	is($sum, $STRIPES, 'the standby\'s workers read every stripe once');

	# a load on the primary while a standby query runs in parallel
	$primary->safe_psql('postgres', 'INSERT INTO w SELECT 900000 + i, md5(i::text) FROM generate_series(1, 3000) i;');
	$primary->wait_for_catchup($standby);
	is($standby->safe_psql('postgres', "$par SELECT count(*) || '|' || count(DISTINCT id) FROM w"),
		$primary->safe_psql('postgres', 'SELECT count(*) || \'|\' || count(DISTINCT id) FROM w'),
		'the standby reads the primary\'s new stripes in parallel');
	$standby->stop;
}

# ---------------------------------------------------------------------------
# The log
# ---------------------------------------------------------------------------

is($primary->safe_psql('postgres', 'SELECT count(*) FROM kingres.columnar_check()'), '0', 'columnar_check is clean at the end');
my $log = slurp_file($primary->logfile);
unlike($log, qr/TRAP:|PANIC:/, 'no assertion failure or PANIC in the log');
unlike($log, qr/WARNING:.*leak/, 'no buffer, lock or reference leak warning in the log');
unlike($log, qr/another list of stripes/, 'no participant found another list of stripes');

$primary->stop;
done_testing();
