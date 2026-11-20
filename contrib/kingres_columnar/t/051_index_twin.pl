# contrib/kingres_columnar/t/051_index_twin.pl
#
# C6b: B-tree indexes on columnar tables against a heap twin with the same indexes.
# A random program of inserts of every size (also over the stripe size), savepoints
# and rollbacks to them, rolled back transactions, prepared transactions, TRUNCATE,
# VACUUM, VACUUM FULL, REINDEX, a conversion to heap and back, an index made in a
# transaction that holds rows, and crashes with a writer in progress.  After every
# step the same predicates are answered four ways, by an index scan and by a sequential
# (custom) scan, on the columnar table and on the twin, and all four must be the same
# rows; at the end a standby answers the same by its index scans.  Then the
# plans (no bitmap scan, no index-only
# scan), a build that is stopped by a crash, and the format-2 refusal (see t/049).
# The seed is printed; KC_SEED reruns it.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1 rsql check_empty wait_waiting reserve_on);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $seed = $ENV{KC_SEED} // (time() ^ ($$ << 8)) & 0x7fffffff;
srand($seed);
note("seed $seed");

my $node = new_node('ridx', init => { allows_streaming => 1 }, conf => "wal_keep_size = '100MB'\n");
q1($node, q{
	CREATE TABLE c (a int, g int, b text) USING columnar;
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE h (a int, g int, b text);
	CREATE SEQUENCE sq;
	CREATE INDEX c_a ON c (a);  CREATE INDEX h_a ON h (a);
	CREATE INDEX c_gb ON c (g, b DESC NULLS FIRST);  CREATE INDEX h_gb ON h (g, b DESC NULLS FIRST);
	CREATE INDEX c_b ON c (b) WHERE g < 10;  CREATE INDEX h_b ON h (b) WHERE g < 10;
	-- the rows of a predicate, as a hash, by an index scan (index) or a sequential scan (seq)
	CREATE FUNCTION rows_of(tbl text, pred text, how text) RETURNS text LANGUAGE plpgsql AS $$
	DECLARE r text;
	BEGIN
	  PERFORM set_config('kingres_columnar.enable_custom_scan', CASE how WHEN 'index' THEN 'off' ELSE 'on' END, true);
	  PERFORM set_config('enable_seqscan', CASE how WHEN 'index' THEN 'off' ELSE 'on' END, true);
	  PERFORM set_config('enable_indexscan', CASE how WHEN 'index' THEN 'on' ELSE 'off' END, true);
	  PERFORM set_config('enable_bitmapscan', 'off', true);
	  PERFORM set_config('enable_indexonlyscan', 'off', true);
	  EXECUTE format('SELECT count(*) || '':'' || coalesce(md5(string_agg(a || '','' || g || '','' || coalesce(b, ''~''), '';'' ORDER BY a, g, b)), '''') FROM %I WHERE %s', tbl, pred) INTO r;
	  RETURN r;
	END $$;
	CREATE FUNCTION chk(x int) RETURNS text LANGUAGE plpgsql AS $$
	DECLARE p text; preds text[]; res text := ''; v text[];
	BEGIN
	  preds := ARRAY['a = ' || x, 'a BETWEEN ' || x || ' AND ' || (x + 300), 'g = 3 AND b < ''v5''', 'b = ''v17'' AND g < 10',
	                 'a > ' || (x * 2), 'g = 7 AND b >= ''v8'''];
	  FOREACH p IN ARRAY preds LOOP
	    v := ARRAY[rows_of('c', p, 'index'), rows_of('c', p, 'seq'), rows_of('h', p, 'index'), rows_of('h', p, 'seq')];
	    IF v[1] <> v[2] OR v[1] <> v[3] OR v[1] <> v[4] THEN res := res || ' [' || p || ': ' || v[1] || ' ' || v[2] || ' ' || v[3] || ' ' || v[4] || ']'; END IF;
	  END LOOP;
	  RETURN CASE WHEN res = '' THEN 'ok' ELSE res END;
	END $$;
});
$node->backup('bk');
my $standby = PostgreSQL::Test::Cluster->new('ridxs');
$standby->init_from_backup($node, 'bk', has_streaming => 1);
$standby->append_conf('postgresql.conf', "hot_standby = on\n");
$standby->start;

sub ins
{
	my ($n) = @_;
	return "WITH s AS (SELECT nextval('sq')::int a FROM generate_series(1, $n)), i AS (INSERT INTO c SELECT a, a % 20, CASE WHEN a % 31 = 0 THEN NULL ELSE 'v' || (a % 97) END FROM s) INSERT INTO h SELECT a, a % 20, CASE WHEN a % 31 = 0 THEN NULL ELSE 'v' || (a % 97) END FROM s;";
}
sub size { return rand() < 0.15 ? 900 + int(rand(1500)) : 1 + int(rand(80)); }

my $prepared = 0;
my %steps;
my $bad = 0;
my $N = $ENV{KC_STEPS} // 45;
for my $s (1 .. $N)
{
	my $p = rand();
	my ($kind, $sql);
	if ($p < 0.20) { $kind = 'insert'; $sql = ins(size()); }
	elsif ($p < 0.35)
	{
		$kind = 'savepoints';
		$sql = "BEGIN; " . ins(size()) . " SAVEPOINT a; " . ins(size()) . " SAVEPOINT b; " . ins(1 + int(rand(30))) . " ROLLBACK TO b; "
		  . ins(1 + int(rand(30))) . " RELEASE a; " . ins(1 + int(rand(30))) . " COMMIT;";
	}
	elsif ($p < 0.42)
	{
		$kind = 'rollback';
		$sql = "BEGIN; " . ins(size()) . " SAVEPOINT a; " . ins(5) . " RELEASE a; ROLLBACK;";
	}
	elsif ($p < 0.52)
	{
		$kind = 'prepared';
		$sql = "BEGIN; " . ins(size()) . " PREPARE TRANSACTION 'p$s';";
	}
	elsif ($p < 0.56) { $kind = 'truncate'; $sql = "TRUNCATE c; TRUNCATE h;"; }
	elsif ($p < 0.62) { $kind = 'vacuum'; $sql = "VACUUM c; VACUUM h;"; }
	elsif ($p < 0.68) { $kind = 'vacuum full'; $sql = "VACUUM FULL c;"; }
	elsif ($p < 0.74) { $kind = 'reindex'; $sql = "REINDEX TABLE c;"; }
	elsif ($p < 0.78) { $kind = 'to heap and back'; $sql = "ALTER TABLE c SET ACCESS METHOD heap; " . ins(10) . " ALTER TABLE c SET ACCESS METHOD columnar;"; }
	elsif ($p < 0.88)
	{
		$kind = 'index in a transaction';
		$sql = "BEGIN; " . ins(size()) . " CREATE INDEX x$s ON c (g, a); CREATE INDEX hx$s ON h (g, a); " . ins(size())
		  . " SET LOCAL kingres_columnar.enable_custom_scan = off; SET LOCAL enable_seqscan = off; SELECT count(*) FROM c WHERE g = 4 AND a > 10; DROP INDEX x$s; DROP INDEX hx$s; COMMIT;";
	}
	else { $kind = 'crash'; }
	$steps{$kind}++;
	note("step $s: $kind");

	if ($kind eq 'crash')
	{
		# a writer in progress (rows buffered, index entries written, nothing committed), then the server stops
		my $bg = $node->background_psql('postgres');
		$bg->query_safe(reserve_on() . "BEGIN; " . ins(300) . " " . ins(50));
		q1($node, "SELECT pg_switch_wal()");
		$node->stop('immediate');
		eval { $bg->quit };
		$node->start;
	}
	else
	{
		my ($rc, $out, $err) = $node->psql('postgres', $sql, on_error_stop => 1);
		if ($rc != 0) { $bad++; diag("step $s ($kind) failed (seed $seed): $err\n$sql"); last; }
		if ($kind eq 'prepared')
		{
			q1($node, (rand() < 0.6 ? 'COMMIT' : 'ROLLBACK') . " PREPARED 'p$s'");
			$prepared++;
		}
	}
	my $max = q1($node, "SELECT coalesce(max(a), 0) FROM h");
	my $x = $max > 0 ? 1 + int(rand($max)) : 1;
	my $r = q1($node, "SELECT chk($x)");
	my $same = q1($node, "SELECT (SELECT count(*) FROM c) = (SELECT count(*) FROM h)");
	if ($r ne 'ok' || $same ne 't')
	{
		$bad++;
		fail("step $s ($kind): $r (same count: $same) (seed $seed)");
		diag($sql // 'crash');
		diag(q1($node, "SET kingres_columnar.enable_custom_scan = off; SET enable_seqscan = off; CREATE TEMP TABLE ix AS SELECT a FROM c WHERE a > " . ($x * 2) . "; SET kingres_columnar.enable_custom_scan = on; SET enable_seqscan = on; SET enable_indexscan = off; SELECT stripes_str('c') || ' missing: ' || coalesce((SELECT string_agg(a || ':' || ctid::text, ' ' ORDER BY a) FROM (SELECT a, ctid FROM c WHERE a > " . ($x * 2) . " AND a NOT IN (SELECT a FROM ix) LIMIT 8) q), '-')"));
		last;
	}
}
is($bad, 0, "$N random steps (" . join(', ', map { "$_ $steps{$_}" } sort keys %steps) . "): the same rows by index and sequential scan, on the columnar table and its twin");
check_empty($node, 'after the program');
is(q1($node, "SELECT count(*) FROM pg_index WHERE NOT indisvalid"), 0, 'no invalid index');

# the standby answers by its index scans as the primary does
$node->wait_for_catchup($standby);
{
	my $max = q1($node, "SELECT coalesce(max(a), 0) FROM h");
	my $x = $max > 0 ? 1 + int(rand($max)) : 1;
	is(q1($standby, "SELECT chk($x)"), 'ok', 'the standby: index and sequential scans give the same rows as its heap twin');
	is(q1($standby, "SELECT rows_of('c', 'a BETWEEN $x AND " . ($x + 500) . "', 'index')"), q1($node, "SELECT rows_of('c', 'a BETWEEN $x AND " . ($x + 500) . "', 'index')"), 'the standby and the primary give the same rows by index scan');
}

# plans: no bitmap scan, no index-only scan, an index scan and an ordered one
{
	my @p;
	for my $q ('SELECT a FROM c WHERE a = 5', 'SELECT a FROM c WHERE a BETWEEN 5 AND 5000', 'SELECT a FROM c ORDER BY a LIMIT 3', 'SELECT g, b FROM c WHERE g = 3')
	{
		my $out = $node->safe_psql('postgres', "SET enable_seqscan = off; SET kingres_columnar.enable_custom_scan = off; SET enable_bitmapscan = on; SET enable_indexonlyscan = on; EXPLAIN (COSTS OFF) $q");
		push @p, $out;
	}
	my $all = join("\n", @p);
	unlike($all, qr/Bitmap|Index Only/, 'with every scan type allowed the plans of a columnar table have no bitmap scan and no index-only scan');
	like($all, qr/Index Scan/, '... they have index scans');
	my $par = $node->safe_psql('postgres', "SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; SET min_parallel_index_scan_size = 0; SET min_parallel_table_scan_size = 0; SET enable_seqscan = off; SET kingres_columnar.enable_custom_scan = off; EXPLAIN (COSTS OFF) SELECT a FROM c WHERE a > 5");
	unlike($par, qr/Gather|Parallel/, '... and none is parallel');
}

# a build that a crash stops: nothing half-made is left, the next build works
{
	q1($node, "TRUNCATE c; TRUNCATE h; " . ins(8000));
	my $bg = $node->background_psql('postgres');
	$bg->query_until(qr/^go$/m, "\\echo go\nCREATE INDEX cx ON c (b, a);\n");
	$node->stop('immediate');
	eval { $bg->quit };
	$node->start;
	my $state = q1($node, "SELECT coalesce((SELECT indisvalid::text FROM pg_index WHERE indexrelid = to_regclass('cx')), 'absent')");
	like($state, qr/^(absent|true)$/, "after a crash during CREATE INDEX the index is absent or complete ($state)");
	q1($node, "DROP INDEX IF EXISTS cx; CREATE INDEX cx ON c (b, a)");
	is(q1($node, "SELECT chk(5)"), 'ok', 'the index made after the crash answers as the twin');
	check_empty($node, 'after the crash during a build');
}

# a parallel build: workers share out the stripes of the table
{
	q1($node, "TRUNCATE c; TRUNCATE h; " . ins(30000));
	my ($rc, $out, $err) = $node->psql('postgres', "SET client_min_messages = debug1; SET min_parallel_table_scan_size = 0; SET max_parallel_maintenance_workers = 3; SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; CREATE INDEX cpar ON c (b, a);", on_error_stop => 1);
	is($rc, 0, 'a parallel CREATE INDEX on a table of 30 stripes') or diag($err);
	like($err, qr/parallel worker/, '... the build asked for parallel workers');
	is(q1($node, "SELECT chk(500)"), 'ok', '... and the index answers as the twin does');
	is(q1($node, "SET kingres_columnar.enable_custom_scan = off; SET enable_seqscan = off; SELECT count(*) FROM c WHERE b > chr(117)"), q1($node, "SELECT count(*) FROM h WHERE b > chr(117)"), '... with every row');
	q1($node, "DROP INDEX cpar");
}

my $log = slurp_file($node->logfile);
unlike($log, qr/TRAP:|PANIC:(?!.*terminating)/, 'no assertion failure and no PANIC in the server log');
$standby->stop;
$node->stop;
done_testing();
