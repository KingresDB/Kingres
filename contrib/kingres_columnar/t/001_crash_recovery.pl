# contrib/kingres_columnar/t/001_crash_recovery.pl
#
# Crash recovery of the columnar table access method.
#
# A columnar table keeps its rows of a transaction in a per-backend buffer and
# writes a stripe (pages logged as full-page images by the KingresColumnar
# resource manager, then a row in kingres.columnar_stripe) when the buffer is
# full or at pre-commit.  So after any crash the committed rows, and only they,
# must be there.  Every scenario loads the same rows into a heap twin in the
# same transactions; the twin is the oracle for the count and for a checksum of
# all columns, and the expected count is also computed independently here.
#
# Scenarios: (a) committed work and an open transaction, (b) a crash in the
# middle of a stripe (some stripes of the open transaction already written),
# (c) crash before and after a checkpoint with full_page_writes on and off and
# with wal_compression, (d) prepared transactions across a crash, (e)
# wal_level = minimal with a table created and loaded in one transaction, (f)
# TRUNCATE, ALTER TABLE SET ACCESS METHOD, VACUUM FULL and
# columnar_compact() cut short, (g) a loop of crashes of three kinds, (h) WAL
# consistency checking of the resource manager and pg_waldump.
#
# The kinds of crash: stop('immediate') (SIGQUIT), kill -9 of the postmaster
# and its children (nothing is cleaned up), and kill -9 of one backend (crash
# restart by the postmaster).  Nothing here depends on timing: the code waits
# for log lines and polls for conditions.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use IPC::Run;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

my $CHECKSUM = q{hashtext(id::text || '/' || pad || '/' || coalesce(n::text, '~'))};

# Deterministic rows: ids lo..hi, a text column, a nullable int column.
sub load_sql
{
	my ($lo, $hi) = @_;
	my $sel = "SELECT g, md5(g::text), CASE WHEN g % 7 = 0 THEN NULL ELSE g END "
	  . "FROM generate_series($lo, $hi) g";
	return "INSERT INTO c $sel; INSERT INTO h $sel;";
}

# A committed transaction of one or more consecutive id ranges.
sub load_committed
{
	my ($node, @ranges) = @_;
	my $sql = "BEGIN;\n";
	$sql .= load_sql(@$_) . "\n" for @ranges;
	$sql .= "COMMIT;\n";
	$node->safe_psql('postgres', $sql);
}

# A fresh pair of tables.  Small stripes make a stripe boundary every 1000 rows.
sub reset_tables
{
	my ($node, $stripe_rows) = @_;
	$stripe_rows //= 1000;
	$node->safe_psql('postgres', qq{
		DROP TABLE IF EXISTS c, h;
		CREATE TABLE c (id int, pad text, n int) USING columnar;
		CREATE TABLE h (id int, pad text, n int);
		SELECT kingres.columnar_set_options('c', stripe_row_count => $stripe_rows,
											chunk_row_count => 100);
	});
}

# Count, twin count, checksum equality, duplicate ids, problems reported by
# columnar_check(), and stripes that overlap, include the metapage or reach
# beyond the end of the file (design V7), as one string.
sub state
{
	my ($node) = @_;
	return $node->safe_psql('postgres', qq{
		WITH s AS (SELECT * FROM kingres.columnar_stripe
					WHERE rel_uid = kingres.columnar_identity('c'))
		SELECT format('rows=%s twin=%s same=%s dups=%s check=%s stripes_bad=%s',
			(SELECT count(*) FROM c),
			(SELECT count(*) FROM h),
			(SELECT coalesce(sum($CHECKSUM), 0) FROM c)
			  = (SELECT coalesce(sum($CHECKSUM), 0) FROM h),
			(SELECT count(*) - count(DISTINCT id) FROM c),
			(SELECT count(*) FROM kingres.columnar_check()),
			(SELECT count(*) FROM s a JOIN s b ON a.stripe_id < b.stripe_id
				AND a.first_block < b.first_block + b.block_count
				AND b.first_block < a.first_block + a.block_count)
			+ (SELECT count(*) FROM s
				WHERE first_block < 1
				   OR first_block + block_count > pg_relation_size('c') / 8192));
	});
}

sub verify
{
	my ($node, $rows, $label) = @_;
		is(state($node),
		"rows=$rows twin=$rows same=t dups=0 check=0 stripes_bad=0", $label)
	  or diag($node->safe_psql('postgres', 'SELECT * FROM kingres.columnar_check()'));
}

# The table must still be usable: insert, VACUUM, compact, TRUNCATE, insert.
sub liveness
{
	my ($node, $rows, $label) = @_;
	load_committed($node, [ 900001, 902500 ]);
	$rows += 2500;
	verify($node, $rows, "$label: insert after recovery");
	$node->safe_psql('postgres',
		"VACUUM c; SELECT kingres.columnar_compact('c');");
	verify($node, $rows, "$label: VACUUM and compact after recovery");
	$node->safe_psql('postgres', 'TRUNCATE c, h;');
	verify($node, 0, "$label: TRUNCATE after recovery");
	load_committed($node, [ 1, 1500 ]);
	verify($node, 1500, "$label: reload after TRUNCATE");
}

sub is_gone
{
	my ($pid) = @_;
	return !kill(0, $pid);
}

sub wait_gone
{
	my (@pids) = @_;
	for my $pid (@pids)
	{
		my $n = 0;
		while (!is_gone($pid))
		{
			die "process $pid does not go away" if ++$n > 3000;
			select(undef, undef, undef, 0.05);
		}
	}
}

sub close_sessions
{
	# The server is dead: quit() may complain about the broken pipe.
	for my $s (@_)
	{
		eval { $s->quit };
	}
}

# Crash the whole server: 'immediate' (pg_ctl -m immediate) or 'kill9'
# (SIGKILL to the postmaster and to every child, which is what the OOM killer
# or a power failure leaves behind), then start it again.
sub crash_and_restart
{
	my ($node, $how, @sessions) = @_;

	if ($how eq 'immediate')
	{
		$node->stop('immediate');
	}
	else
	{
		my $pm = $node->{_pid};
		my @kids = grep { /^\d+$/ } split /\s+/, scalar(`pgrep -P $pm`);
		$node->kill9;
		kill 'KILL', @kids;
		wait_gone($pm, @kids);
	}
	close_sessions(@sessions);
	$node->start;
}

# Kill one backend with SIGKILL and wait until the postmaster has finished the
# crash restart and recovery.
sub crash_backend
{
	my ($node, $pid, @sessions) = @_;
	my $off = -s $node->logfile;
	kill 'KILL', $pid;
	$node->wait_for_log(
		qr/all server processes terminated; reinitializing.*database system is ready to accept connections/s,
		$off);
	close_sessions(@sessions);
	$node->poll_query_until('postgres', 'SELECT 1', '1');
}

sub new_node
{
	my ($name, $conf) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init;
	$node->append_conf('postgresql.conf', qq{
shared_preload_libraries = 'kingres_columnar'
max_prepared_transactions = 10
restart_after_crash = on
$conf
});
	$node->start;
	$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
	return $node;
}

# ---------------------------------------------------------------------------
# (a), (b): committed work, then a crash with another transaction open
# ---------------------------------------------------------------------------

my $node = new_node('colcrash', '');

# The resource manager is registered at preload; a server without the library
# could not replay the records.  Make sure the setup is what we think it is.
is($node->safe_psql('postgres', 'SHOW shared_preload_libraries'),
	'kingres_columnar', 'library is preloaded');

# (a) Two committed transactions (the first spans several stripes), then an
# open transaction with rows in the buffer only (default stripe size is far
# larger than its rows, so nothing of it has been written yet).
reset_tables($node, 150000);
load_committed($node, [ 1, 3000 ]);
load_committed($node, [ 3001, 5500 ], [ 5501, 5600 ]);
my $bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN;' . load_sql(10001, 12000));
	is($bg->query_safe('SELECT count(*) FROM c'), '7600',
		'(a) the open transaction sees its own buffered rows');
crash_and_restart($node, 'immediate', $bg);
verify($node, 5600, '(a) after immediate stop with an open transaction');
liveness($node, 5600, '(a)');

# (a) again with kill -9 of everything.
reset_tables($node, 150000);
load_committed($node, [ 1, 4000 ]);
$bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN;' . load_sql(10001, 12000));
crash_and_restart($node, 'kill9', $bg);
verify($node, 4000, '(a) after kill -9 with an open transaction');
liveness($node, 4000, '(a) kill9');

# (b) A crash in the middle of a stripe.  With 1000 rows per stripe the open
# transaction has already written four stripes (their pages are in the WAL and
# in the file, their catalog rows are uncommitted) and holds 500 rows in the
# buffer.  A committed transaction precedes it.
reset_tables($node, 1000);
load_committed($node, [ 1, 2500 ]);
$bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN;' . load_sql(10001, 12000) . load_sql(12001, 14500));
my $inflight = $bg->query_safe(
	q{SELECT count(*) FROM kingres.columnar_stripe
	   WHERE rel_uid = kingres.columnar_identity('c')});
cmp_ok($inflight, '>=', 7,
	'(b) the open transaction has already written stripes');
my $size_before = $node->safe_psql('postgres', "SELECT pg_relation_size('c')");
crash_and_restart($node, 'kill9', $bg);
verify($node, 2500, '(b) kill -9 in the middle of a stripe');
# Pages of the lost stripes are garbage in the file; new stripes must go after
# them without overlapping anything.
load_committed($node, [ 3001, 4200 ]);
verify($node, 3700, '(b) writing after a crash that left unreferenced pages');
cmp_ok($node->safe_psql('postgres', "SELECT pg_relation_size('c')"),
	'>=', $size_before,
	'(b) the relation file was not shortened by recovery (V6)');
liveness($node, 3700, '(b)');

# (b) with a backend killed by SIGKILL (crash restart, not a server restart).
reset_tables($node, 1000);
load_committed($node, [ 1, 2500 ]);
$bg = $node->background_psql('postgres');
my $pid = $bg->query_safe('SELECT pg_backend_pid()');
$bg->query_safe('BEGIN;' . load_sql(10001, 13300));
crash_backend($node, $pid, $bg);
verify($node, 2500, '(b) after SIGKILL of the backend in the middle of a stripe');
liveness($node, 2500, '(b) backend kill');

# A backend killed while it is flushing a stripe at COMMIT time is the hard
# case; it cannot be placed from here, so run commit of a large transaction in
# the background and kill the backend as soon as it is running the COMMIT.
for my $round (1 .. 3)
{
	reset_tables($node, 1000);
	load_committed($node, [ 1, 1500 ]);
	$bg = $node->background_psql('postgres');
	$pid = $bg->query_safe('SELECT pg_backend_pid()');
	$bg->query_safe('BEGIN;' . load_sql(20001, 20900));
	# The buffer holds 900 rows (a stripe is 1000), so COMMIT writes it.
	$bg->{stdin} .= "COMMIT;\n";
	$bg->{run}->pump_nb;
	crash_backend($node, $pid, $bg);
	# The COMMIT may or may not have completed before the kill; both are
	# legal, but the twin and the columnar table must agree.
	my $st = state($node);
	like($st, qr/^rows=(1500|2400) twin=\1 same=t dups=0 check=0 stripes_bad=0$/,
		"(b) kill during COMMIT, round $round: all or nothing");
}

# ---------------------------------------------------------------------------
# (c) before and after a checkpoint; full_page_writes, wal_compression
# ---------------------------------------------------------------------------

my @configs = (
	[ 'fpw on',  "full_page_writes = on\nwal_compression = off" ],
	[ 'fpw off', "full_page_writes = off\nwal_compression = off" ],
	[ 'wal_compression', "full_page_writes = on\nwal_compression = on" ],);

for my $cfg (@configs)
{
	my ($name, $conf) = @$cfg;
	$node->stop;
	$node->append_conf('postgresql.conf', $conf);
	$node->start;

	reset_tables($node, 1000);
	load_committed($node, [ 1, 3500 ]);
	$node->safe_psql('postgres', 'CHECKPOINT');
	load_committed($node, [ 3501, 5000 ]);
	crash_and_restart($node, 'immediate');
	verify($node, 5000, "(c) $name: crash after a checkpoint and a commit");

	load_committed($node, [ 5001, 6200 ]);
	crash_and_restart($node, 'kill9');
	verify($node, 6200, "(c) $name: kill -9 with no checkpoint since");

	# A checkpoint after the commit: everything is flushed, redo has nothing.
	load_committed($node, [ 6201, 8000 ]);
	$node->safe_psql('postgres', 'CHECKPOINT');
	crash_and_restart($node, 'immediate');
	verify($node, 8000, "(c) $name: crash right after the checkpoint");
	liveness($node, 8000, "(c) $name");
}

# ---------------------------------------------------------------------------
# (h) WAL consistency checking of the resource manager, in the same server:
# every replayed page is compared with the image in the record.  A mismatch is
# a PANIC in the log, so the server would not come back.
# ---------------------------------------------------------------------------

$node->stop;
$node->append_conf('postgresql.conf',
	"full_page_writes = on\nwal_compression = off\n"
	  . "wal_consistency_checking = 'KingresColumnar'");
$node->start;
is($node->safe_psql('postgres', 'SHOW wal_consistency_checking'),
	'KingresColumnar', '(h) consistency checking of the resource manager is on');
reset_tables($node, 1000);
load_committed($node, [ 1, 2600 ]);
$bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN;' . load_sql(10001, 12600));
$node->safe_psql('postgres', "CHECKPOINT; SELECT kingres.columnar_compact('c'); VACUUM c;");
load_committed($node, [ 3001, 3500 ]);
crash_and_restart($node, 'immediate', $bg);
verify($node, 3100, '(h) replay with consistency checking');
unlike(slurp_file($node->logfile), qr/inconsistent page found/,
	'(h) no inconsistent page reported during replay');
$node->safe_psql('postgres', 'TRUNCATE c, h;');
load_committed($node, [ 1, 2200 ]);
crash_and_restart($node, 'kill9');
verify($node, 2200, '(h) TRUNCATE and reload replayed with consistency checking');

# pg_waldump cannot load the module, so the resource manager has the generic
# name custom128.  There must be records of it in the WAL we just wrote.
$node->stop;
{
	my $waldir = $node->data_dir . '/pg_wal';
	opendir(my $dh, $waldir) or die "cannot open $waldir: $!";
	my @segs = sort grep { /^[0-9A-F]{24}$/ } readdir($dh);
	closedir($dh);
	my ($out, $err) = ('', '');
	my $ok = IPC::Run::run(
		[ 'pg_waldump', '--path', $waldir, '--rmgr', 'custom128',
		  $segs[0], $segs[-1] ],
		'>', \$out, '2>', \$err);
	# pg_waldump may complain at the end of the last segment; the records are
	# what counts.
	my $n = () = $out =~ /^rmgr: custom128/mg;
	cmp_ok($n, '>', 0, "(h) pg_waldump shows records of the resource manager ($n)");
}
$node->start;

# ---------------------------------------------------------------------------
# (d) prepared transactions across a crash
# ---------------------------------------------------------------------------

$node->stop;
$node->append_conf('postgresql.conf',
	"wal_consistency_checking = ''\nfull_page_writes = on");
$node->start;

# Committed after the crash.
reset_tables($node, 1000);
load_committed($node, [ 1, 1200 ]);
$node->safe_psql('postgres',
	'BEGIN;' . load_sql(5001, 8500) . "PREPARE TRANSACTION 'p1';");
verify($node, 1200, '(d) a prepared transaction is invisible');
crash_and_restart($node, 'immediate');
verify($node, 1200, '(d) still invisible after the crash');
$node->safe_psql('postgres', "COMMIT PREPARED 'p1'");
verify($node, 4700, '(d) COMMIT PREPARED after the crash');

# Rolled back after a crash that came after a checkpoint (the two-phase state
# is then in a file rather than only in the WAL), with a committed
# transaction after the PREPARE.
$node->safe_psql('postgres',
	'BEGIN;' . load_sql(20001, 22500) . "PREPARE TRANSACTION 'p2';");
$node->safe_psql('postgres', 'CHECKPOINT');
load_committed($node, [ 30001, 30900 ]);
crash_and_restart($node, 'kill9');
verify($node, 5600, '(d) prepared state in a file, other transaction committed');
$node->safe_psql('postgres', "ROLLBACK PREPARED 'p2'");
verify($node, 5600, '(d) ROLLBACK PREPARED after the crash');

# Prepared, crash, commit, crash again.
$node->safe_psql('postgres',
	'BEGIN;' . load_sql(40001, 41500) . "PREPARE TRANSACTION 'p3';");
$node->safe_psql('postgres', 'BEGIN;' . load_sql(50001, 50700) . "PREPARE TRANSACTION 'p4';");
crash_and_restart($node, 'immediate');
$node->safe_psql('postgres', "COMMIT PREPARED 'p4'; ROLLBACK PREPARED 'p3';");
crash_and_restart($node, 'kill9');
verify($node, 6300, '(d) two prepared transactions, one committed, one rolled back');
liveness($node, 6300, '(d)');

# ---------------------------------------------------------------------------
# (f) TRUNCATE, ALTER TABLE SET ACCESS METHOD, VACUUM FULL, compact, cut short
# ---------------------------------------------------------------------------

# A table rewrite (SET ACCESS METHOD, VACUUM FULL) leaves the stripe rows of
# the old file until the next VACUUM or columnar_reconcile() (design 5.5), and
# columnar_check() reports them; that is not damage, so reconcile first.
sub reconcile_then_verify
{
	my ($node, $rows, $label) = @_;
	$node->safe_psql('postgres', 'SELECT kingres.columnar_reconcile()');
	verify($node, $rows, $label);
}

sub amname
{
	my ($node) = @_;
	return $node->safe_psql('postgres',
		"SELECT a.amname FROM pg_class r JOIN pg_am a ON a.oid = r.relam WHERE r.oid = 'c'::regclass");
}

my $N = 60000;
reset_tables($node, 10000);
load_committed($node, [ 1, $N ]);
verify($node, $N, '(f) large table loaded');

# TRUNCATE not committed, then committed.
$bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN; TRUNCATE c; TRUNCATE h;');
crash_and_restart($node, 'kill9', $bg);
verify($node, $N, '(f) crash with an uncommitted TRUNCATE');
$bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN; TRUNCATE c; TRUNCATE h; COMMIT;');
crash_and_restart($node, 'kill9', $bg);
verify($node, 0, '(f) crash after a committed TRUNCATE');
load_committed($node, [ 1, $N ]);

# TRUNCATE after rows were inserted in the same transaction (the buffer is
# dropped, the file is replaced).
$bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN;' . load_sql(900001, 902000) . 'TRUNCATE c; TRUNCATE h; '
	  . load_sql(1, 1300) . 'COMMIT;');
crash_and_restart($node, 'immediate', $bg);
verify($node, 1300, '(f) crash after INSERT, TRUNCATE, INSERT in one transaction');
reset_tables($node, 10000);
load_committed($node, [ 1, $N ]);

# SET ACCESS METHOD, both ways, uncommitted and committed.
$bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN; ALTER TABLE c SET ACCESS METHOD heap;');
crash_and_restart($node, 'kill9', $bg);
is(amname($node), 'columnar', '(f) uncommitted SET ACCESS METHOD heap: still columnar');
verify($node, $N, '(f) data after an uncommitted rewrite to heap');

$bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN; ALTER TABLE c SET ACCESS METHOD heap; COMMIT;');
crash_and_restart($node, 'kill9', $bg);
is(amname($node), 'heap', '(f) committed SET ACCESS METHOD heap: heap');
is($node->safe_psql('postgres', 'SELECT count(*) FROM c'), $N,
	'(f) rows after a committed rewrite to heap');

$bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN; ALTER TABLE c SET ACCESS METHOD columnar;');
crash_and_restart($node, 'immediate', $bg);
is(amname($node), 'heap', '(f) uncommitted SET ACCESS METHOD columnar: still heap');
$bg = $node->background_psql('postgres');
$bg->query_safe('BEGIN; ALTER TABLE c SET ACCESS METHOD columnar; COMMIT;');
crash_and_restart($node, 'kill9', $bg);
is(amname($node), 'columnar', '(f) committed SET ACCESS METHOD columnar');
reconcile_then_verify($node, $N, '(f) data after the rewrite to columnar and a crash');

# Commands cut short while they run.  The crash can fall anywhere, before or
# after the commit of the command; the data must be whole either way.
for my $cmd ('VACUUM FULL c', "SELECT kingres.columnar_compact('c', 1000000)",
	'ALTER TABLE c SET ACCESS METHOD heap', 'VACUUM c')
{
	for my $how ('kill9', 'immediate')
	{
		$bg = $node->background_psql('postgres');
		$bg->query_until(qr/started/, "\\echo started\n$cmd;\n");
		crash_and_restart($node, $how, $bg);
		reconcile_then_verify($node, $N, "(f) $how while '$cmd' runs");
		$node->safe_psql('postgres', 'ALTER TABLE c SET ACCESS METHOD columnar')
		  if amname($node) ne 'columnar';
	}
}
verify($node, $N, '(f) after all the interrupted commands');
liveness($node, $N, '(f)');

# ---------------------------------------------------------------------------
# (e) wal_level = minimal
# ---------------------------------------------------------------------------

$node->stop;
my $min = new_node('colmin', "wal_level = minimal\nmax_wal_senders = 0");

# Created and loaded in one transaction, committed, then a crash: a table
# created in the transaction skips WAL for heap, so check the AM did not.
$min->safe_psql('postgres', q{
	BEGIN;
	CREATE TABLE c (id int, pad text, n int) USING columnar;
	CREATE TABLE h (id int, pad text, n int);
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
	} . load_sql(1, 4500) . 'COMMIT;');
crash_and_restart($min, 'immediate');
verify($min, 4500, '(e) minimal: created and loaded in one transaction, immediate stop');

# Same with kill -9 and CREATE TABLE AS.
$min->safe_psql('postgres', q{
	BEGIN;
	CREATE TABLE c2 USING columnar AS SELECT g AS id, md5(g::text) AS pad, g AS n
	  FROM generate_series(1, 3300) g;
	COMMIT;});
crash_and_restart($min, 'kill9');
is($min->safe_psql('postgres', 'SELECT count(*), sum(n) FROM c2'), '3300|5446650',
	'(e) minimal: CREATE TABLE AS then kill -9');

# Created, loaded, crash before commit: the table must not exist.
$bg = $min->background_psql('postgres');
$bg->query_safe(q{
	BEGIN;
	CREATE TABLE c3 (id int, pad text, n int) USING columnar;
	INSERT INTO c3 SELECT g, md5(g::text), g FROM generate_series(1, 5000) g;
	});
crash_and_restart($min, 'kill9', $bg);
is($min->safe_psql('postgres', q{SELECT to_regclass('c3') IS NULL}), 't',
	'(e) minimal: table of a transaction that never committed is gone');
is($min->safe_psql('postgres', 'SELECT count(*) FROM kingres.columnar_check()'),
	'0', '(e) minimal: columnar_check() clean after it');

# Created and loaded in one transaction, the transaction truncates and loads
# again, then commits.
$min->safe_psql('postgres', q{
	BEGIN;
	CREATE TABLE c4 (id int) USING columnar;
	INSERT INTO c4 SELECT g FROM generate_series(1, 3000) g;
	TRUNCATE c4;
	INSERT INTO c4 SELECT g FROM generate_series(1, 1234) g;
	COMMIT;});
crash_and_restart($min, 'immediate');
is($min->safe_psql('postgres', 'SELECT count(*) FROM c4'), '1234',
	'(e) minimal: create, load, TRUNCATE, load, commit');

# Existing table, loaded later, a crash: ordinary logging applies.
load_committed($min, [ 5001, 6000 ]);
crash_and_restart($min, 'kill9');
verify($min, 5500, '(e) minimal: loaded later into an existing table');
$min->stop;

# ---------------------------------------------------------------------------
# (g) crash loop
# ---------------------------------------------------------------------------

$node->start;
reset_tables($node, 1000);
srand(20260101);
my ($total, $next) = (0, 1);
my @kinds = ('immediate', 'kill9', 'backend');
for my $i (1 .. 20)
{
	my $kind = $kinds[ $i % 3 ];

	my $committed = 1 + int(rand(2600));
	load_committed($node, [ $next, $next + $committed - 1 ]);
	$total += $committed;
	$next += $committed;

	# An open transaction of 1 to 3000 rows that never commits.
	$bg = $node->background_psql('postgres');
	my $bpid = $bg->query_safe('SELECT pg_backend_pid()');
	my $open = 1 + int(rand(3000));
	$bg->query_safe('BEGIN;' . load_sql($next + 100000, $next + 100000 + $open - 1));

	if ($kind eq 'backend')
	{
		crash_backend($node, $bpid, $bg);
	}
	else
	{
		crash_and_restart($node, $kind, $bg);
	}
	verify($node, $total, "(g) iteration $i ($kind): $total committed rows");
}
liveness($node, $total, '(g)');

# No crash of the server was a bug of ours: only the planned kills may show up
# as a failure, and an assertion or a PANIC would be a defect.
my $log = slurp_file($node->logfile);
unlike($log, qr/TRAP:|PANIC:/, 'no assertion failure or PANIC in the main node log');
$node->stop;
unlike(slurp_file($min->logfile), qr/TRAP:|PANIC:/,
	'no assertion failure or PANIC in the minimal node log');

done_testing();
