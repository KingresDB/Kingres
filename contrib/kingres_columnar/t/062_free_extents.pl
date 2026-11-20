# contrib/kingres_columnar/t/062_free_extents.pl
#
# C8: free-extent reuse (design 7.5).  VACUUM lists the blocks of the file that no stripe any
# snapshot can still see occupies, in the metapage, and a writer takes a run of them for its
# stripe before it extends the file.
#  1. A steady load of small inserts, compactions and VACUUMs: the file stops growing (a
#     control table with no VACUUM grows without bound), the rows are those of a heap twin,
#     columnar_check finds nothing (it also checks that the list names no block of a visible stripe).
#  2. An older snapshot (REPEATABLE READ) keeps the stripes a compaction replaced: VACUUM does not
#     list their blocks while it lives and its reads stay right; after it ends VACUUM lists them.
#  3. A failed load in the middle of the file: its blocks are reused by the next load.
#  4. A crash after a stripe took a free run and before it wrote a page: the table is intact after
#     the restart, nothing is lost or used twice, and the next VACUUM lists the run again.
#  5. A hot standby: a query on it with an old snapshot is cancelled (a recovery conflict) before
#     the blocks it may read are written again, never given wrong rows; with hot_standby_feedback
#     the primary keeps the stripes and the query goes on.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1 wait_waiting);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use POSIX ();
use Test::More;

my $KEY = 782;
my $node = new_node('fext', init => { allows_streaming => 1 },
	conf => "wal_keep_size = '300MB'\nwal_consistency_checking = 'KingresColumnar'\n");

sub size { my ($t) = @_; return q1($node, "SELECT pg_relation_size('$t')"); }

q1($node, q{
	CREATE TABLE t (id int, v text) USING columnar;
	SELECT kingres.columnar_set_options('t', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE t2 (id int, v text) USING columnar;
	SELECT kingres.columnar_set_options('t2', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE th (id int, v text);
});

# ---------------------------------------------------------------------------
# 1. a steady load
# ---------------------------------------------------------------------------
my (@sizes, @waste);
for my $round (1 .. 40)
{
	for my $i (0 .. 9)
	{
		my $lo = $round * 10000 + $i * 100;
		for my $tbl ('t', 't2')
		{
			q1($node, "INSERT INTO $tbl SELECT g, md5(g::text) || md5((g + 1)::text) FROM generate_series($lo + 1, $lo + 100) g");
		}
		q1($node, "INSERT INTO th SELECT g, md5(g::text) || md5((g + 1)::text) FROM generate_series($lo + 1, $lo + 100) g");
	}
	for my $tbl ('t', 't2')
	{
		q1($node, "SELECT kingres.columnar_compact('$tbl', 100000)");
	}
	q1($node, "VACUUM t");    # the control table t2 is never vacuumed
	push @sizes, size('t');
	push @waste, q1($node, "SELECT unreferenced_bytes FROM kingres.columnar_info('t')");
}
my ($send, $c2) = ($sizes[-1], size('t2'));
my $control = q1($node, "SELECT unreferenced_bytes FROM kingres.columnar_info('t2')");
my $fr_c = $control / $c2;
q1($node, "VACUUM FULL t");
my $live = size('t');
cmp_ok($send, '<', 3 * $live, "1: after 40 rounds the file of the vacuumed table, $send bytes, is under three times what the same rows need ($live bytes after VACUUM FULL)");
cmp_ok($fr_c, '>', 0.8, sprintf("1: against %.0f percent of the control table that is never vacuumed (%d of %d bytes)", 100 * $fr_c, $control, $c2));
cmp_ok($send * 3, '<', $c2, "1: the file of the vacuumed table is under a third of the control's: $send bytes against $c2");
cmp_ok($send, '<', $c2, "1: and its file is the smaller: $send bytes against $c2");
is(q1($node, "SELECT count(*) FROM (SELECT id, v FROM t EXCEPT SELECT id, v FROM th) s"), 0, '1: the rows are those of the heap twin');
is(q1($node, "SELECT count(*) FROM (SELECT id, v FROM th EXCEPT SELECT id, v FROM t) s"), 0, '1: ... in both directions');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '1: columnar_check finds nothing (the free list overlaps no stripe)');
note("sizes by round: " . join(' ', @sizes[0, 4, 9, 19, 29, 39]));

# ---------------------------------------------------------------------------
# 2. an older snapshot
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE os (id int, v text) USING columnar");
q1($node, "INSERT INTO os SELECT $_ * 100 + g, 'old' || g FROM generate_series(1, 100) g") for 0 .. 9;
my $reader = $node->background_psql('postgres', on_error_stop => 0);
my $before = $reader->query("BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) || ':' || sum(hashtext(v)) FROM os;");
chomp $before;
q1($node, "SELECT kingres.columnar_compact('os', 100000)");
q1($node, "VACUUM os");
q1($node, "INSERT INTO os SELECT 5000 + g, 'new' || g FROM generate_series(1, 300) g");
q1($node, "SELECT kingres.columnar_compact('os', 100000)");
q1($node, "VACUUM os");
my $after = $reader->query("SELECT count(*) || ':' || sum(hashtext(v)) FROM os;");
chomp $after;
is($after, $before, '2: the older snapshot reads its rows after a compaction, two vacuums and new inserts');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '2: columnar_check finds nothing');
$reader->query("COMMIT;");
$reader->quit;
q1($node, "VACUUM os");
q1($node, "INSERT INTO os SELECT 9000 + g, 'later' || g FROM generate_series(1, 300) g");
is(q1($node, "SELECT count(*) FROM os"), 1000 + 300 + 300, '2: all rows are there after the older snapshot ended');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '2: columnar_check finds nothing');

# ---------------------------------------------------------------------------
# 3. a failed load in the middle of the file
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE fl (id int, v text) USING columnar");
q1($node, "INSERT INTO fl SELECT g, md5(g::text) FROM generate_series(1, 150000) g");
my $big = size('fl');
q1($node, "BEGIN; INSERT INTO fl SELECT 1000000 + g, md5(g::text) || md5((g * 3)::text) FROM generate_series(1, 150000) g; ROLLBACK");
my $withgarbage = size('fl');
cmp_ok($withgarbage, '>', $big * 1.4, "the failed load left dead pages: $big bytes grew to $withgarbage");
q1($node, "INSERT INTO fl SELECT 1500000 + g, 'tail' FROM generate_series(1, 100) g");    # a stripe after the garbage
q1($node, "VACUUM fl");
my $v1 = size('fl');
q1($node, "INSERT INTO fl SELECT 3000000 + g, md5(g::text) || md5((g * 3)::text) FROM generate_series(1, 149000) g");
my $v2 = size('fl');
cmp_ok($v2, '<=', $v1 * 1.1, "3: the next load of the same size went into the freed blocks: $v1 bytes, $v2 after it");
is(q1($node, "SELECT count(*) FROM fl"), 150000 + 100 + 149000, '3: the rows are there');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '3: columnar_check finds nothing');

# ---------------------------------------------------------------------------
# 3b. a run that is one block too short is not taken, a run that fits exactly is
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE probe (id int, v text) USING columnar");
sub blocks_for
{
	my ($n) = @_;
	q1($node, "TRUNCATE probe; INSERT INTO probe SELECT g, md5(g::text) || md5((g * 3)::text) FROM generate_series(1, $n) g");
	return q1($node, "SELECT pg_relation_size('probe') / 8192 - 1");
}
# the number of rows of the loads below that makes a stripe of $want blocks (the sizes grow with the count)
sub rows_for
{
	my ($want) = @_;
	my ($lo, $hi) = (1000, 40000);
	while ($lo < $hi)
	{
		my $mid = int(($lo + $hi) / 2);
		if (blocks_for($mid) < $want) { $lo = $mid + 1; } else { $hi = $mid; }
	}
	return $lo;
}
# the free list of a table, read from its metapage: (number of extents, first block, blocks) of the first
sub free_list
{
	my ($t) = @_;
	return q1($node, "SELECT get_byte(p, 132) || ',' || (get_byte(p, 144) + 256 * get_byte(p, 145) + 65536 * get_byte(p, 146)) || ',' || (get_byte(p, 148) + 256 * get_byte(p, 149) + 65536 * get_byte(p, 150)) FROM (SELECT get_raw_page('$t', 0) p) s");
}
for my $case ('one more', 'exact')
{
	my $t = $case eq 'exact' ? 'fx_exact' : 'fx_more';
	q1($node, "CREATE TABLE $t (id int, v text) USING columnar; CREATE TABLE ${t}_h (id int, v text)");
	# ten small stripes, side by side at the start of the file, then compacted into one at the end: the
	# ten become one free run, with a live stripe right behind it
	for my $i (0 .. 9)
	{
		q1($node, "INSERT INTO $t SELECT $i * 1000 + g, md5(g::text) || md5((g * 3)::text) FROM generate_series(1, 1000) g");
		q1($node, "INSERT INTO ${t}_h SELECT $i * 1000 + g, md5(g::text) || md5((g * 3)::text) FROM generate_series(1, 1000) g");
	}
	q1($node, "SELECT kingres.columnar_compact('$t', 100000)");
	q1($node, "VACUUM $t");
	my ($cnt, $first, $len) = split /,/, free_list($t);
	is("$cnt:$first", "1:1", "3b ($case): the list holds the one run the ten stripes left, $len blocks at the start of the file");
	my $before = size($t) / 8192;
	my $want = $case eq 'exact' ? $len : $len + 1;
	my $n = rows_for($want);
	is(blocks_for($n), $want, "3b ($case): a load of $n rows is a stripe of $want blocks");
	q1($node, "INSERT INTO $t SELECT 800000 + g, md5(g::text) || md5((g * 3)::text) FROM generate_series(1, $n) g");
	q1($node, "INSERT INTO ${t}_h SELECT 800000 + g, md5(g::text) || md5((g * 3)::text) FROM generate_series(1, $n) g");
	my $after = size($t) / 8192;
	if ($case eq 'exact') { is($after, $before, "3b (exact): a stripe that fits the run exactly went into it: the file did not grow"); }
	else { cmp_ok($after, '>=', $before + $want, "3b (one more): a stripe one block longer than the run was written at the end of the file ($before to $after blocks)"); }
	is(q1($node, "SELECT count(*) FROM (SELECT id, v FROM $t EXCEPT SELECT id, v FROM ${t}_h) s") . '|' . q1($node, "SELECT count(*) FROM (SELECT id, v FROM ${t}_h EXCEPT SELECT id, v FROM $t) s"),
		'0|0', "3b ($case): the rows are those of the twin: nothing was written over a stripe");
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, "3b ($case): columnar_check finds nothing");
}

# ---------------------------------------------------------------------------
# 3c. a writer that has written its pages and not yet its catalog row keeps VACUUM from listing them
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE gw (id int, v text) USING columnar; CREATE INDEX gw_id ON gw (id); CREATE TABLE gwh (id int, v text)");
q1($node, "INSERT INTO gw SELECT g, 'base' FROM generate_series(1, 300) g; INSERT INTO gwh SELECT g, 'base' FROM generate_series(1, 300) g");
{
	my $ctlg = $node->background_psql('postgres');
	$ctlg->query_safe("SELECT pg_advisory_lock($KEY, 0);");
	my $wg = $node->background_psql('postgres', on_error_stop => 0);
	# a table with an index reserves its row numbers: the stripe's row is the reserved one, which has no blocks,
	# so at this point (3: pages written, the row not yet updated) the pages belong to no row the catalog shows
	$wg->query_until(qr/^go$/m, "SET kingres_columnar.reserve_gate = $KEY; SET kingres_columnar.reserve_gate_at = 3;\n\\echo go\nINSERT INTO gw SELECT 1000 + g, md5(g::text) || md5((g * 5)::text) FROM generate_series(1, 4000) g;\n");
	ok(wait_waiting($node, 1), '3c: the writer waits after it wrote its pages');
	$node->safe_psql('postgres', "SET kingres_columnar.vacuum_truncate_lock_timeout = 300; VACUUM gw");
	is(free_list('gw'), '0,0,0', '3c: VACUUM listed nothing while the writer was in flight (no run is free)');
	$ctlg->query_safe("SELECT pg_advisory_unlock($KEY, 0);");
	$wg->query("SELECT 1;");
	q1($node, "INSERT INTO gwh SELECT 1000 + g, md5(g::text) || md5((g * 5)::text) FROM generate_series(1, 4000) g");
	# loads that would take the writer's blocks, had they been listed
	for my $r (1 .. 3)
	{
		q1($node, "INSERT INTO gw SELECT 100000 * $r + g, md5((g + $r)::text) || md5((g * 5)::text) FROM generate_series(1, 4000) g");
		q1($node, "INSERT INTO gwh SELECT 100000 * $r + g, md5((g + $r)::text) || md5((g * 5)::text) FROM generate_series(1, 4000) g");
	}
	is(q1($node, "SELECT count(*) FROM (SELECT id, v FROM gw EXCEPT SELECT id, v FROM gwh) s") . '|' . q1($node, "SELECT count(*) FROM (SELECT id, v FROM gwh EXCEPT SELECT id, v FROM gw) s"),
		'0|0', '3c: the rows are those of the twin: the writer\'s stripe was not written over');
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '3c: columnar_check finds nothing');
	$_->quit for ($ctlg, $wg);
}

# ---------------------------------------------------------------------------
# 4. a crash after a stripe took a free run, before it wrote a page
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE cr (id int, v text) USING columnar; CREATE TABLE crh (id int, v text)");
for my $r (1 .. 3)
{
	q1($node, "INSERT INTO cr SELECT $r * 1000 + g, md5(g::text) FROM generate_series(1, 1000) g");
	q1($node, "INSERT INTO crh SELECT $r * 1000 + g, md5(g::text) FROM generate_series(1, 1000) g");
}
q1($node, "SELECT kingres.columnar_compact('cr', 100000)");
q1($node, "VACUUM cr");
my $ctl = $node->background_psql('postgres');
$ctl->query_safe("SELECT pg_advisory_lock($KEY, 0);");
my $w = $node->background_psql('postgres', on_error_stop => 0);
$w->query_until(qr/^go$/m, "SET kingres_columnar.reserve_gate = $KEY; SET kingres_columnar.reserve_gate_at = 5;\n\\echo go\nINSERT INTO cr SELECT 9000 + g, 'doomed' FROM generate_series(1, 500) g;\n");
ok(wait_waiting($node, 1), '4: the writer waits after it took a free run');
$node->stop('immediate');
$node->start;
is(q1($node, "SELECT count(*) FROM (SELECT id, v FROM cr EXCEPT SELECT id, v FROM crh) s"), 0, '4: after the crash the committed rows are intact');
is(q1($node, "SELECT count(*) FROM cr"), 3000, '4: ... and the doomed rows are not there');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '4: columnar_check finds nothing');
q1($node, "VACUUM cr");
q1($node, "INSERT INTO cr SELECT 20000 + g, md5(g::text) FROM generate_series(1, 1000) g");
q1($node, "INSERT INTO crh SELECT 20000 + g, md5(g::text) FROM generate_series(1, 1000) g");
is(q1($node, "SELECT count(*) FROM (SELECT id, v FROM cr EXCEPT SELECT id, v FROM crh) s"), 0, '4: after VACUUM and a new load the rows are still those of the twin');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '4: columnar_check finds nothing');

# 4b. a take of a free run is logged: after a crash the run is not given out again
q1($node, "CREATE TABLE tk (id int, v text) USING columnar; CREATE TABLE tkh (id int, v text)");
for my $r (1 .. 4)
{
	q1($node, "INSERT INTO tk SELECT $r * 1000 + g, md5(g::text) FROM generate_series(1, 1000) g");
	q1($node, "INSERT INTO tkh SELECT $r * 1000 + g, md5(g::text) FROM generate_series(1, 1000) g");
}
q1($node, "SELECT kingres.columnar_compact('tk', 100000)");
q1($node, "VACUUM tk");
q1($node, "INSERT INTO tk SELECT 10000 + g, md5((g + 1)::text) FROM generate_series(1, 1000) g");    # takes a run
q1($node, "INSERT INTO tkh SELECT 10000 + g, md5((g + 1)::text) FROM generate_series(1, 1000) g");
$node->stop('immediate');
$node->start;
q1($node, "INSERT INTO tk SELECT 20000 + g, md5((g + 2)::text) FROM generate_series(1, 1000) g");
q1($node, "INSERT INTO tkh SELECT 20000 + g, md5((g + 2)::text) FROM generate_series(1, 1000) g");
is(q1($node, "SELECT count(*) FROM (SELECT id, v FROM tk EXCEPT SELECT id, v FROM tkh) s") . '|' . q1($node, "SELECT count(*) FROM (SELECT id, v FROM tkh EXCEPT SELECT id, v FROM tk) s"),
	'0|0', '4b: after a crash the run a committed stripe took is not given out again: the rows are those of the twin');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '4b: columnar_check finds nothing');

# ---------------------------------------------------------------------------
# 5. a hot standby
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE sb (id int, v text) USING columnar");
q1($node, "INSERT INTO sb SELECT $_ * 100 + g, md5(g::text) FROM generate_series(1, 100) g") for 0 .. 9;
$node->backup('bk0');
for my $feedback ('off', 'on')
{
	my $stb = PostgreSQL::Test::Cluster->new("fstb_$feedback");
	$stb->init_from_backup($node, 'bk0', has_streaming => 1);
	$stb->append_conf('postgresql.conf', "hot_standby = on\nmax_standby_streaming_delay = '2s'\nhot_standby_feedback = $feedback\n");
	$stb->start;
	$node->wait_for_catchup($stb);
	my $sr = $stb->background_psql('postgres', on_error_stop => 0);
	my $first = $sr->query("BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) || ':' || sum(hashtext(v)) FROM sb;");
	chomp $first;
	# the primary replaces the stripes, vacuums (listing their blocks) and writes over them
	q1($node, "SELECT kingres.columnar_compact('sb', 100000)");
	q1($node, "VACUUM sb");
	q1($node, "INSERT INTO sb SELECT 50000 + g, md5((g + 7)::text) FROM generate_series(1, 400) g");
	q1($node, "INSERT INTO sb SELECT 60000 + g, md5((g + 9)::text) FROM generate_series(1, 400) g");
	$node->wait_for_catchup($stb);
	$sr->{stderr} = '';
	my $second = '';
	# a snapshot conflict ends the session of a transaction that is idle between statements (FATAL)
	my $alive = eval { $second = $sr->query("SELECT count(*) || ':' || sum(hashtext(v)) FROM sb;"); 1 };
	chomp $second;
	my $err = $sr->{stderr};
	my $log = slurp_file($stb->logfile);
	if ($feedback eq 'on')
	{
		ok($alive, "5 (feedback on): the old query goes on");
		is($err, '', "5 (feedback on): ... without an error");
		is($second, $first, '5 (feedback on): ... with the same rows: the primary kept the stripes');
		unlike($log, qr/conflict with recovery/, '5 (feedback on): no recovery conflict');
	}
	else
	{
		ok(!$alive || $err =~ /conflict with recovery/ || $second eq $first,
			"5 (feedback off): the old query was cancelled by a recovery conflict or read its own rows (got '$second', error '$err')");
		like($log, qr/conflict with recovery/, '5 (feedback off): the standby log records the recovery conflict: the blocks were not written over under the query');
		unlike($err . $log, qr/corrupt|checksum|invalid page/i, '5 (feedback off): and nothing reported a damaged table');
	}
	if ($alive) { $sr->quit; } else { eval { $sr->quit; }; }
	# a new query sees the primary's rows
	is(q1($stb, "SELECT count(*) || ':' || sum(hashtext(v)) FROM sb"), q1($node, "SELECT count(*) || ':' || sum(hashtext(v)) FROM sb"),
		"5 ($feedback): a new query on the standby gives the primary's rows");
	is(q1($stb, "SELECT count(*) FROM kingres.columnar_check()"), 0, "5 ($feedback): columnar_check finds nothing on the standby");
	$stb->stop;
	q1($node, "DELETE FROM sb WHERE false") if 0;
}


# ---------------------------------------------------------------------------
# 6. writers while compactions and vacuums run
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE cw (id int, v text) USING columnar; CREATE TABLE cwh (id int, v text); CREATE SEQUENCE cws");
my $script = $node->basedir . '/cw.pgb';
append_to_file($script, q{\set n random(1, 300)
\set w random(1, 5)
BEGIN;
SELECT setval('cws', nextval('cws') + 1);
INSERT INTO cwh SELECT g, md5(g::text) FROM generate_series(currval('cws') * 1000 + 1, currval('cws') * 1000 + :n) g;
INSERT INTO cw SELECT g, md5(g::text) FROM generate_series(currval('cws') * 1000 + 1, currval('cws') * 1000 + :n) g;
\if :w = 1
ROLLBACK;
\else
COMMIT;
\endif
});
my $pid = fork();
if ($pid == 0)
{
	for (1 .. 40)
	{
		system('psql', '-X', '-q', '-d', 'postgres', '-h', $node->host, '-p', $node->port, '-c', "SELECT kingres.columnar_compact('cw', 100000)", '-c', 'VACUUM cw');
		select(undef, undef, undef, 0.05);
	}
	POSIX::_exit(0);
}
$node->command_ok([ 'pgbench', '-n', '-c', 4, '-j', 4, '-t', 120, '-f', $script, 'postgres' ], '6: four writers while compactions and vacuums run');
waitpid($pid, 0);
is($? >> 8, 0, '6: the loop of compactions and vacuums ended without an error');
q1($node, "VACUUM cw");
is(q1($node, "SELECT count(*) FROM (SELECT id, v FROM cw EXCEPT SELECT id, v FROM cwh) s") . '|' . q1($node, "SELECT count(*) FROM (SELECT id, v FROM cwh EXCEPT SELECT id, v FROM cw) s"),
	'0|0', '6: the rows are those of the heap twin, both ways');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '6: columnar_check finds nothing');

# ---------------------------------------------------------------------------
# 7. a failed load at the end of the file: the tail is given back and the list trimmed with it
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE tr (id int, v text) USING columnar; CREATE TABLE trh (id int, v text)");
q1($node, "INSERT INTO tr SELECT g, md5(g::text) FROM generate_series(1, 20000) g");
q1($node, "INSERT INTO trh SELECT g, md5(g::text) FROM generate_series(1, 20000) g");
my $tr0 = size('tr');
q1($node, "BEGIN; INSERT INTO tr SELECT 100000 + g, md5(g::text) || md5((g * 3)::text) FROM generate_series(1, 150000) g; ROLLBACK");
cmp_ok(size('tr'), '>', $tr0 + 4000000, '7: the failed load is at the end of the file');
q1($node, "VACUUM tr");
cmp_ok(size('tr'), '<', $tr0 + 1000000, '7: VACUUM gave the tail back');
for my $r (1 .. 3)
{
	q1($node, "INSERT INTO tr SELECT 200000 * $r + g, md5((g + $r)::text) || md5(g::text) FROM generate_series(1, 150000) g");
	q1($node, "INSERT INTO trh SELECT 200000 * $r + g, md5((g + $r)::text) || md5(g::text) FROM generate_series(1, 150000) g");
	q1($node, "INSERT INTO tr SELECT 100 * $r + g + 9000000, 'small' FROM generate_series(1, 300) g");
	q1($node, "INSERT INTO trh SELECT 100 * $r + g + 9000000, 'small' FROM generate_series(1, 300) g");
}
is(q1($node, "SELECT count(*) FROM (SELECT id, v FROM tr EXCEPT SELECT id, v FROM trh) s") . '|' . q1($node, "SELECT count(*) FROM (SELECT id, v FROM trh EXCEPT SELECT id, v FROM tr) s"),
	'0|0', '7: after the tail was cut and the file grew again the rows are those of the twin (a stale list would have been written over live stripes)');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '7: columnar_check finds nothing');
$node->stop;
done_testing();
