# contrib/kingres_columnar/t/044_reserve_tids.pl
#
# C6a: the TID of a row exists when the insert returns it (design 5.3, 7.8) and is
# the TID the row has afterwards, whatever happens to its buffer, and no two live
# rows ever share one.  With the developer setting kingres_columnar.reserve_row_numbers
# on, against a heap twin:
#
#   1. a generator: random transactions of random inserts (narrow and wide rows,
#      small batches and batches over the stripe size, COPY), savepoints, releases
#      and rollbacks to them, reads that cause a flush, memory pressure under a 1 MB
#      allowance, COMMIT, ROLLBACK and PREPARE TRANSACTION with COMMIT or ROLLBACK
#      PREPARED; after every transaction the whole table is checked: the rows are
#      those of the twin, no two share a TID, every row has the TID its insert
#      returned, no reserved row is left, columnar_check is clean;
#   2. write-through (rows written by a callback of another module after the commit
#      flush, kcprobe.commit_sql): the TIDs returned equal the TIDs after the commit;
#   3. COPY and CREATE TABLE AS: unique TIDs, in the order of the rows;
#   4. the unused tail of a reservation is given back when it is the end of the range,
#      and the gap stays when it is not; the next row never reuses a number;
#   5. the same transactions with the setting off make the stripes and the row
#      numbers 0.1 made (the reservation changes nothing when it is off).
#
# Needs a compiler and the server's headers for the probe module (part 2).  The
# seed of the generator is printed; KC_SEED reruns it.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcProbe qw(build_module);
use KcReserve qw(new_node q1 rsql fp same_as_twin tids_ok check_empty stripes_str reserve_on);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $dir = build_module('kcprobe');
my $node = new_node('rtids', preload => "$dir/kcprobe,kingres_columnar");

my $seed = $ENV{KC_SEED} // (time() ^ ($$ << 8)) & 0x7fffffff;
srand($seed);
note("seed $seed");

q1($node, q{
	CREATE TABLE r (a int, b text) USING columnar;
	SELECT kingres.columnar_set_options('r', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE h (a int, b text);
	CREATE TABLE ret (a int, t tid);
	CREATE SEQUENCE sq;
});

# ---------------------------------------------------------------------------
# 1. the generator
# ---------------------------------------------------------------------------
my $prepared = 0;
my $stmts_total = 0;

# one insert statement of n rows with b as wide as w bytes (0: short), the TIDs
# kept in ret, the same rows in the twin
sub insert_stmt
{
	my ($n, $w) = @_;
	my $b = $w ? "repeat('y', $w) || a" : "repeat('x', a % 37) || a";
	return qq{WITH s AS (SELECT nextval('sq')::int a FROM generate_series(1, $n)),
	 i AS (INSERT INTO r SELECT a, $b FROM s RETURNING a, ctid),
	 j AS (INSERT INTO h SELECT a, $b FROM s)
	 INSERT INTO ret SELECT a, ctid FROM i;};
}

sub gen_txn
{
	my ($t) = @_;
	my @sql;
	my @stack;						# open savepoints
	my $next_sp = 1;
	my $nstmt = 2 + int(rand(7));

	push @sql, 'BEGIN;';
	for (my $k = 0; $k < $nstmt; $k++)
	{
		my $p = rand();

		if ($p < 0.50)
		{
			my $n = rand() < 0.12 ? 1000 + int(rand(1500)) : 1 + int(rand(60));
			push @sql, insert_stmt($n, 0);
		}
		elsif ($p < 0.58)
		{
			# wide rows under a small allowance: the flush that memory asks for
			push @sql, "SET LOCAL kingres_columnar.write_buffer_memory = '1MB';";
			push @sql, insert_stmt(400 + int(rand(150)), 2900);
		}
		elsif ($p < 0.70)
		{
			my $name = 'sp' . $next_sp++;
			push @stack, $name;
			push @sql, "SAVEPOINT $name;";
		}
		elsif ($p < 0.80 && @stack)
		{
			my $name = pop @stack;
			push @sql, "RELEASE $name;";
		}
		elsif ($p < 0.88 && @stack)
		{
			# ROLLBACK TO keeps the savepoint (and the ones inside it are gone)
			my $i = int(rand(scalar @stack));
			push @sql, "ROLLBACK TO $stack[$i];";
			splice(@stack, $i + 1);
		}
		elsif (!@stack)
		{
			# a read that flushes the buffers of the transaction (a parallel plan)
			push @sql, "SET LOCAL debug_parallel_query = on; SELECT count(*) > 0 FROM r; SET LOCAL debug_parallel_query = off;";
		}
	}
	my $end = rand();
	my $mode;
	if ($end < 0.70) { push @sql, 'COMMIT;'; $mode = 'commit'; }
	elsif ($end < 0.88) { push @sql, 'ROLLBACK;'; $mode = 'rollback'; }
	else
	{
		push @sql, "PREPARE TRANSACTION 'g$t';";
		$mode = 'prepare';
	}
	return ($mode, join("\n", @sql));
}

my $N = 60;
my $failed_txn = 0;
for my $t (1 .. $N)
{
	my ($mode, $script) = gen_txn($t);
	my ($rc, $out, $err) = rsql($node, $script);
	if ($rc != 0)
	{
		$failed_txn++;
		diag("transaction $t failed (seed $seed): $err\n$script");
		last;
	}
	if ($mode eq 'prepare')
	{
		my $fin = rand() < 0.6 ? 'COMMIT' : 'ROLLBACK';
		q1($node, "$fin PREPARED 'g$t'");
		$prepared++;
	}
	my ($same, $n, $dist, $match, $res) = split /\|/, q1($node, q{
		SELECT fp('r') = fp('h'),
		       (SELECT count(*) FROM r),
		       (SELECT count(DISTINCT ctid::text) FROM r),
		       (SELECT count(*) FROM r JOIN ret USING (a) WHERE r.ctid::text = ret.t::text),
		       (SELECT count(*) FROM kingres.columnar_stripe WHERE reserved)});
	my $check = q1($node, "SELECT count(*) FROM kingres.columnar_check()");
	my $ok = ($same eq 't' && $n == $dist && $n == $match && $res == 0 && $check == 0);
	if (!$ok)
	{
		fail("transaction $t ($mode) (seed $seed): twin=$same rows=$n distinct_tids=$dist tid_matches=$match reserved=$res check=$check");
		diag(q1($node, "SELECT string_agg(check_name || ' ' || coalesce(detail, ''), '; ') FROM kingres.columnar_check()"));
		diag($script);
		$failed_txn++;
		last;
	}
}
is($failed_txn, 0, "$N random transactions ($prepared prepared): after each one the rows are those of the twin, no two share a TID, every row has the TID its insert returned, no reserved row is left, columnar_check is clean");
cmp_ok(q1($node, "SELECT count(*) FROM r"), '>', 0, 'the table holds rows');
cmp_ok(q1($node, "SELECT count(*) FROM kingres.columnar_stripes('r')"), '>', 3, 'in several stripes');

# The generator reaches the paths it is about (counted in one session, with the same
# statements): a flush by size, by memory pressure, by a committing savepoint, by a read.
{
	my ($rc, $out, $err) = rsql($node, "BEGIN;\n" . insert_stmt(2200, 0) .
		"\nSET LOCAL kingres_columnar.write_buffer_memory = '1MB';\n" . insert_stmt(520, 2900) .
		"\nSAVEPOINT x;\n" . insert_stmt(30, 0) . "\nRELEASE x;\n" .
		"SET LOCAL debug_parallel_query = on; SELECT count(*) > 0 FROM r; SET LOCAL debug_parallel_query = off;\n" .
		insert_stmt(5, 0) . "\nCOMMIT;\n" .
		"SELECT string_agg(path || '=' || hits, ' ' ORDER BY path) FROM kingres.columnar_write_paths() WHERE hits > 0 AND path IN ('size_flush', 'pressure_flush', 'own_flush', 'reserved', 'tail_returned');");
	is($rc, 0, 'a transaction with every kind of flush commits') or diag($err);
	like($out, qr/own_flush=\d+ pressure_flush=\d+ reserved=\d+ size_flush=\d+ tail_returned=\d+/,
		'it took the paths of the generator: size flush, memory pressure, own flush of a savepoint, reservations, tails given back');
	tids_ok($node, 'r', 'ret', 'every kind of flush');
	same_as_twin($node, 'r', 'h', 'every kind of flush');
	check_empty($node, 'every kind of flush');
}

# ---------------------------------------------------------------------------
# 2. write-through: rows written after the commit-time flush
# ---------------------------------------------------------------------------
q1($node, q{
	CREATE TABLE w (a int, b text) USING columnar;
	CREATE TABLE wh (a int, b text);
	CREATE TABLE wret (a int, t tid);
	CREATE SEQUENCE wsq;
});
{
	# rows of the transaction itself (buffered, flushed at commit) and rows a callback
	# inserts after that (written through): the TIDs of both are those afterwards
	my $late = q{WITH s AS (SELECT nextval(''wsq'')::int a FROM generate_series(1, 7)), i AS (INSERT INTO w SELECT a, ''late'' || a FROM s RETURNING a, ctid), j AS (INSERT INTO wh SELECT a, ''late'' || a FROM s) INSERT INTO wret SELECT a, ctid FROM i};
	my ($rc, $out, $err) = rsql($node, qq{
		BEGIN;
		WITH s AS (SELECT nextval('wsq')::int a FROM generate_series(1, 30)), i AS (INSERT INTO w SELECT a, 'own' || a FROM s RETURNING a, ctid), j AS (INSERT INTO wh SELECT a, 'own' || a FROM s) INSERT INTO wret SELECT a, ctid FROM i;
		SET LOCAL kcprobe.commit_sql = '$late';
		SELECT 1;
		COMMIT;
		SELECT hits FROM kingres.columnar_write_paths() WHERE path = 'late_commit';});
	is($rc, 0, 'a transaction whose callback writes rows after the commit flush commits') or diag($err);
	cmp_ok((split /\n/, $out)[-1], '>', 0, '... through the write-through path (late_commit)');
	is(q1($node, "SELECT count(*) FROM w"), 37, 'its 30 rows and the 7 written through are there');
	tids_ok($node, 'w', 'wret', 'write-through');
	same_as_twin($node, 'w', 'wh', 'write-through');
	check_empty($node, 'write-through');

	# the same in a subtransaction commit (between the two phases of the hand-over)
	($rc, $out, $err) = rsql($node, qq{
		BEGIN;
		SAVEPOINT x;
		WITH s AS (SELECT nextval('wsq')::int a FROM generate_series(1, 5)), i AS (INSERT INTO w SELECT a, 'sub' || a FROM s RETURNING a, ctid), j AS (INSERT INTO wh SELECT a, 'sub' || a FROM s) INSERT INTO wret SELECT a, ctid FROM i;
		SET LOCAL kcprobe.commit_sql = '$late';
		RELEASE x;
		COMMIT;});
	is($rc, 0, 'a release of a savepoint with a callback that writes') or diag($err);
	tids_ok($node, 'w', 'wret', 'write-through after a release');
	same_as_twin($node, 'w', 'wh', 'write-through after a release');
}

# ---------------------------------------------------------------------------
# 3. COPY and CREATE TABLE AS
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE cp (a int, b text) USING columnar; SELECT kingres.columnar_set_options('cp', stripe_row_count => 1000, chunk_row_count => 100)");
{
	my ($rc, $out, $err) = rsql($node, q{COPY cp (a) FROM PROGRAM 'seq 1 2600';});
	is($rc, 0, 'COPY of 2600 rows with the setting on') or diag($err);
	my ($n, $dist, $inorder) = split /\|/, q1($node, q{
		SELECT count(*), count(DISTINCT ctid::text),
		       (SELECT count(*) FROM (SELECT ctid, lag(ctid) OVER (ORDER BY a) l FROM cp) x WHERE ctid > l)
		  FROM cp});
	is("$n/$dist/$inorder", '2600/2600/2599', 'COPY: 2600 rows, 2600 TIDs, in the order of the rows');
	($rc, $out, $err) = rsql($node, q{CREATE TABLE ctas USING columnar AS SELECT g a, 'c' || g b FROM generate_series(1, 2100) g;});
	is($rc, 0, 'CREATE TABLE AS into columnar with the setting on') or diag($err);
	is(q1($node, "SELECT count(*) || '/' || count(DISTINCT ctid::text) FROM ctas"), '2100/2100', 'CREATE TABLE AS: 2100 rows, 2100 TIDs');
	check_empty($node, 'COPY and CREATE TABLE AS');

	# a CREATE TABLE AS whose query is parallel writes in the leader while the executor is
	# in parallel mode, where a reserved row could not be updated (heap forbids it): its
	# rows do not reserve, and its buffers are written by size and by memory as ever
	q1($node, "CREATE TABLE psrc AS SELECT g AS id, repeat('x', 1000) AS pad FROM generate_series(1, 9000) g; ALTER TABLE psrc SET (parallel_workers = 2); ANALYZE psrc;");
	($rc, $out, $err) = rsql($node, q{SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; SET min_parallel_table_scan_size = 0; SET max_parallel_workers_per_gather = 2;
		SET kingres_columnar.write_buffer_memory = '1MB'; SET kingres_columnar.stripe_row_count = 1000;
		CREATE TABLE pctas USING columnar AS SELECT id, pad FROM psrc;
		SELECT 'RESERVED=' || coalesce((SELECT hits FROM kingres.columnar_write_paths() WHERE path = 'reserved'), 0), 'PRESSURE=' || coalesce((SELECT hits FROM kingres.columnar_write_paths() WHERE path = 'pressure_flush'), 0);});
	is($rc, 0, 'a CREATE TABLE AS with a parallel query and the setting on') or diag($err);
	like($out, qr/RESERVED=0\|PRESSURE=[1-9]/, '... reserved nothing, and memory wrote stripes while it ran');
	is(q1($node, "SELECT count(*) || '/' || count(DISTINCT ctid::text) FROM pctas"), '9000/9000', '... 9000 rows, 9000 TIDs');
	check_empty($node, 'a CREATE TABLE AS with a parallel query');
}

# ---------------------------------------------------------------------------
# 4. the tail of a reservation: given back at the end of the range, a gap otherwise
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE tl (a int, b text) USING columnar; SELECT kingres.columnar_set_options('tl', stripe_row_count => 1000, chunk_row_count => 100); CREATE TABLE tlret (a int, t tid);");
{
	is(q1($node, "SELECT meta_next_row('tl')"), 0, 'a new table: the next row number is 0');
	rsql($node, "INSERT INTO tl SELECT g, 'a' FROM generate_series(1, 10) g;");
	is(q1($node, "SELECT meta_next_row('tl')"), 10, 'ten rows, one transaction: the tail of the reservation was given back (next number 10)');
	is(stripes_str($node, 'tl'), '(0,10)', '... one stripe of ten rows');

	# a holder in the middle: the first session reserves, the second reserves after it
	# and ends first, so its tail is the end of the range and goes back; the first
	# one's is not and stays
	my $bg = $node->background_psql('postgres');
	$bg->query_safe(reserve_on() . "BEGIN; INSERT INTO tl SELECT g, 'holder' FROM generate_series(101, 105) g;");
	is(q1($node, "SELECT meta_next_row('tl')"), 1010, 'a holder in progress has taken 1000 numbers (next 1010)');
	rsql($node, "INSERT INTO tl SELECT g, 'other' FROM generate_series(201, 203) g;");
	is(q1($node, "SELECT meta_next_row('tl')"), 1013, 'a second writer took the 1000 after it and gave back all but 3 (next 1013)');
	$bg->query_safe("COMMIT;");
	$bg->quit;
	is(q1($node, "SELECT meta_next_row('tl')"), 1013, 'the holder committed: its tail is not the end of the range, so the gap stays (next 1013)');
	is(stripes_str($node, 'tl'), '(0,10) (10,5) (1010,3)', 'the stripes: 10 rows, the holder\'s 5 at the start of its 1000 numbers, the other writer\'s 3 after them');
	is(q1($node, "SELECT count(*) || '/' || count(DISTINCT ctid::text) FROM tl"), '18/18', '18 rows, 18 TIDs');
	# the next row never reuses a number that a row had or that is in a gap
	rsql($node, "INSERT INTO tl VALUES (301, 'next');");
	is(q1($node, "SELECT min(ctid::text) FROM tl WHERE a = 301"), '(0,1014)', 'the next row has the number 1013, after the gap: nobody has the numbers 15 to 1009 again');
	check_empty($node, 'tails');
}

# ---------------------------------------------------------------------------
# 4b. buffers that a scan holds, a savepoint released over an open cursor, ADD COLUMN
# ---------------------------------------------------------------------------
q1($node, q{
	CREATE TABLE x (a int, b text) USING columnar;
	SELECT kingres.columnar_set_options('x', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE xh (a int, b text);
	CREATE TABLE xret (a int, t tid);
	CREATE SEQUENCE xsq;
});
sub xins
{
	my ($n) = @_;
	return qq{WITH s AS (SELECT nextval('xsq')::int a FROM generate_series(1, $n)),
	 i AS (INSERT INTO x (a, b) SELECT a, 'x' || a FROM s RETURNING a, ctid),
	 j AS (INSERT INTO xh (a, b) SELECT a, 'x' || a FROM s)
	 INSERT INTO xret SELECT a, ctid FROM i;};
}
{
	# INSERT ... SELECT from the table itself: the scan holds the buffer of the earlier
	# command, the new rows go to a buffer of their own with its own reservation
	my ($rc, $out, $err) = rsql($node, "BEGIN; " . xins(300) . qq{
		WITH i AS (INSERT INTO x (a, b) SELECT a + 100000, b FROM x RETURNING a, ctid),
		 j AS (INSERT INTO xh (a, b) SELECT a + 100000, b FROM xh)
		 INSERT INTO xret SELECT a, ctid FROM i;
		COMMIT;});
	is($rc, 0, 'INSERT ... SELECT from the table itself') or diag($err);
	is(q1($node, "SELECT count(*) FROM x"), 600, '... 600 rows');
	tids_ok($node, 'x', 'xret', 'self insert');
	same_as_twin($node, 'x', 'xh', 'self insert');

	# the setting changed inside a transaction: a row goes into a buffer made under the
	# same setting, so the rows written while it is on have their numbers, whatever was
	# written before it was switched on (and the rows before have none: a TID of no meaning)
	($rc, $out, $err) = $node->psql('postgres', "BEGIN; SET kingres_columnar.reserve_row_numbers = off; "
		. "INSERT INTO x (a, b) VALUES (900001, 'off'), (900002, 'off'); INSERT INTO xh (a, b) VALUES (900001, 'off'), (900002, 'off'); "
		. "SET kingres_columnar.reserve_row_numbers = on; " . xins(15) . " SET kingres_columnar.reserve_row_numbers = off; "
		. "INSERT INTO x (a, b) VALUES (900003, 'off'); INSERT INTO xh (a, b) VALUES (900003, 'off'); "
		. "SET kingres_columnar.reserve_row_numbers = on; " . xins(15) . " COMMIT;", on_error_stop => 1);
	is($rc, 0, 'the setting switched off and on inside one transaction') or diag($err);
	is(q1($node, "SELECT count(*) FROM x x1 JOIN xret r USING (a) WHERE x1.ctid::text = r.t::text"), q1($node, "SELECT count(*) FROM xret"), '... every row written while it was on has the TID its insert returned');
	same_as_twin($node, 'x', 'xh', 'the setting switched in a transaction');
	# the rows written while it was off have their TIDs now; later checks keep them
	q1($node, "INSERT INTO xret SELECT a, ctid FROM x WHERE a >= 900000 AND a NOT IN (SELECT a FROM xret)");
	is(q1($node, "SELECT count(DISTINCT ctid::text) = count(*) FROM x"), 't', '... no two rows share a TID');

	# a subtransaction that wrote without reserving, under a parent that reserved: the
	# rows of the two are never merged into one buffer (that would have 1100 rows under a
	# reservation of 1000 numbers)
	($rc, $out, $err) = $node->psql('postgres', "BEGIN; SET kingres_columnar.reserve_row_numbers = on; " . xins(900)
		. " SAVEPOINT s; SET LOCAL kingres_columnar.reserve_row_numbers = off; "
		. "INSERT INTO x (a, b) SELECT g, 'off' FROM generate_series(910001, 910200) g; INSERT INTO xh (a, b) SELECT g, 'off' FROM generate_series(910001, 910200) g; "
		. "RELEASE s; SET kingres_columnar.reserve_row_numbers = on; " . xins(3) . " COMMIT;", on_error_stop => 1);
	is($rc, 0, 'a child that does not reserve under a parent that does') or diag($err);
	is(q1($node, "SELECT count(*) FROM x x1 JOIN xret r USING (a) WHERE x1.ctid::text = r.t::text"), q1($node, "SELECT count(*) FROM xret"), '... the rows that reserved have their TIDs');
	same_as_twin($node, 'x', 'xh', 'a child that does not reserve');
	q1($node, "INSERT INTO xret SELECT a, ctid FROM x WHERE a >= 900000 AND a NOT IN (SELECT a FROM xret)");
	is(q1($node, "SELECT count(DISTINCT ctid::text) = count(*) FROM x"), 't', '... no two rows share a TID');
	check_empty($node, 'a child that does not reserve');

	# a savepoint released while a cursor reads its rows: the buffer cannot be written
	# by the subtransaction and becomes the parent's, with its reservation and its
	# reserved catalog row (inserted by the subtransaction); the parent writes it
	($rc, $out, $err) = rsql($node, "BEGIN; SAVEPOINT s; " . xins(20) . qq{
		DECLARE cur CURSOR FOR SELECT a FROM x;
		FETCH 1 FROM cur;
		RELEASE s;
		} . xins(20) . qq{ FETCH 1 FROM cur; COMMIT;});
	is($rc, 0, 'a savepoint released over an open cursor') or diag($err);
	tids_ok($node, 'x', 'xret', 'cursor over a released savepoint');
	same_as_twin($node, 'x', 'xh', 'cursor over a released savepoint');
	check_empty($node, 'cursor over a released savepoint');

	# the table gets a column between two inserts of one transaction
	($rc, $out, $err) = rsql($node, "BEGIN; " . xins(5) . qq{ ALTER TABLE x ADD COLUMN z int DEFAULT 7; ALTER TABLE xh ADD COLUMN z int DEFAULT 7;} . xins(5) . " COMMIT;");
	is($rc, 0, 'ADD COLUMN between two inserts') or diag($err);
	tids_ok($node, 'x', 'xret', 'ADD COLUMN');
	same_as_twin($node, 'x', 'xh', 'ADD COLUMN');
	is(q1($node, "SELECT count(*) FROM x WHERE z = 7"), q1($node, "SELECT count(*) FROM x"), '... the new column has its default in every row');
	check_empty($node, 'ADD COLUMN');
}

# ---------------------------------------------------------------------------
# 5. the setting off: the stripes and the numbers of 0.1
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE off1 (a int, b text) USING columnar; SELECT kingres.columnar_set_options('off1', stripe_row_count => 1000, chunk_row_count => 100)");
$node->psql('postgres', q{
	BEGIN;
	INSERT INTO off1 SELECT g, 'o' FROM generate_series(1, 2500) g;
	SAVEPOINT s;
	INSERT INTO off1 SELECT g, 'p' FROM generate_series(1, 40) g;
	RELEASE s;
	COMMIT;
	INSERT INTO off1 VALUES (1, 'q');});
is(stripes_str($node, 'off1'), '(0,1000) (1000,1000) (2000,540) (2540,1)', 'setting off: stripes as before (the savepoint rows merge into the parent, row numbers dense)');
is(q1($node, "SELECT count(*) FROM kingres.columnar_stripe WHERE reserved"), 0, '... and no reserved row');
is(q1($node, "SELECT meta_next_row('off1')"), 2541, '... and the metapage hands out the next number after the last row');

$node->stop;
done_testing();
