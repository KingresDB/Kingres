# contrib/kingres_timepart/t/005_convert_concurrent.pl
#
# Writers and readers during the conversion of a partition to columnar
# storage (design 10.4, 10.5, 10.7).  No row is lost and none is doubled, by
# count and by a checksum of every row, in each of these:
#
#   * a writer that names the partition ("INSERT INTO ev_p1") waits for the
#     copy, then goes on against the new partition;
#   * a writer through the parent holds the parent in ROW EXCLUSIVE while it
#     waits for the partition, the swap needs the parent in ACCESS EXCLUSIVE:
#     the cycle is broken by the short wait of the swap, not by the deadlock
#     detector; the conversion gives way (swap_lock_timeout), its copy is
#     discarded, the writer's row goes into the old partition, and the next
#     attempt converts it with the row.  The same through the policy: the
#     action is recorded as failed, the run is partial, the next run is due
#     within five minutes, and the next run converts;
#   * a long reader of the parent does the same to the swap;
#   * readers never see zero or two copies of the partition across the swap;
#   * a conversion that is cancelled by statement_timeout while it copies
#     leaves the old partition and no stray table.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use IPC::Run ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(usleep time);

my $node = PostgreSQL::Test::Cluster->new('conv_conc');
$node->init;
$node->append_conf('postgresql.conf', q{
fsync = off
shared_preload_libraries = 'kingres_columnar'
kingres_timepart.trust_table_owners = on
kingres_timepart.create_lock_timeout_ms = 10000
});
$node->start;

$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE EXTENSION kingres_timepart;
ALTER DATABASE postgres SET search_path = public;
ALTER DATABASE postgres SET timezone = 'UTC';
});

# Three days of rows; the first partition is the one that is converted, and it
# is big enough for the copy to take a moment.
my $N = 1000000;
$node->safe_psql('postgres', qq{
CREATE TABLE ev (id bigint, ts timestamptz NOT NULL, dev int, payload text) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('ev', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
INSERT INTO ev
SELECT i, '2026-03-01 UTC'::timestamptz + (i % 86400) * interval '1 second',
       i % 17, md5(i::text) || repeat('x', i % 40)
  FROM generate_series(1, $N) i;
INSERT INTO ev
SELECT i, '2026-03-02 UTC'::timestamptz + (i % 86400) * interval '1 second', i % 17, md5(i::text)
  FROM generate_series(2000001, 2020000) i;
INSERT INTO ev
SELECT i, '2026-03-03 UTC'::timestamptz + (i % 86400) * interval '1 second', i % 17, md5(i::text)
  FROM generate_series(3000001, 3020000) i;
-- what the writers insert, to know what the table must hold in the end
CREATE TABLE inserted (LIKE ev);
});

# count and a checksum of every row, from the rows' text
my $digest_sql = q{SELECT count(*) || '|' || coalesce(sum(hashtext(t::text)::numeric), 0) FROM %s t};

sub q1
{
	my ($sql) = @_;
	return $node->safe_psql('postgres', $sql);
}

sub digest_of
{
	my ($rel) = @_;
	return q1(sprintf($digest_sql, $rel));
}

# count and checksum of the table, and of the rows the writers have put in
# 'inserted' together with the rows of the start: what ev must hold.
my $base = digest_of('ev');
sub expected
{
	my ($c0, $s0) = split /\|/, $base;
	my ($c1, $s1) = split /\|/, digest_of('inserted');
	return ($c0 + $c1) . '|' . ($s0 + $s1);
}

# Start psql on a script, in the background; return a handle.
sub spawn
{
	my ($script) = @_;
	my $h = { out => '', err => '', in => $script };
	$h->{run} = IPC::Run::start(
		[ 'psql', '-XAtq', '-v', 'ON_ERROR_STOP=1', '-d', $node->connstr('postgres'), '-f', '-' ],
		'<', \$h->{in}, '>', \$h->{out}, '2>', \$h->{err}, IPC::Run::timeout(300));
	# the script is sent to psql now, not when the harness is finished
	$h->{run}->pump_nb;
	return $h;
}

sub finish
{
	my ($h) = @_;
	$h->{run}->finish;
	$h->{status} = $h->{run}->result(0);
	return $h;
}

# Wait until a query returns a positive number; give up after a time.
sub wait_for
{
	my ($sql, $what, $tries) = @_;
	$tries //= 1200;
	for (1 .. $tries)
	{
		my $r = q1($sql);
		return 1 if $r ne '0' && $r ne '';
		usleep(10_000);
	}
	fail("timed out waiting for $what");
	return 0;
}

# the conversion has the partition locked (it is copying)
sub copying_sql
{
	my ($part) = @_;
	return qq{SELECT count(*) FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
		WHERE c.relname = '$part' AND l.mode = 'ShareRowExclusiveLock' AND l.granted};
}

# a session is waiting for a lock, and its query matches
sub waiting_sql
{
	my ($like) = @_;
	return qq{SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '$like'};
}

sub methods
{
	return q1(q{SELECT string_agg(c.relname || ':' || a.amname, ',' ORDER BY c.relname)
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_am a ON a.oid = c.relam
		WHERE i.inhparent = 'ev'::regclass});
}

sub strays
{
	return q1(q{SELECT count(*) FROM pg_class WHERE relname LIKE '%\_\_kc'});
}

# Put the first partition back in heap, for the next scenario.
sub back_to_heap
{
	q1(q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});
}

is(methods(), 'ev_p20260301:heap,ev_p20260302:heap,ev_p20260303:heap', 'start: three heap partitions');
my $convert = q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'columnar')};

# ---- 1. A writer that names the partition waits, then goes on against the new one.
my $conv = spawn($convert . ";\n");
wait_for(copying_sql('ev_p20260301'), 'the copy to start');
my $w = spawn(q{
BEGIN;
INSERT INTO ev_p20260301 VALUES (9000001, '2026-03-01 12:00 UTC', 1, 'by name');
INSERT INTO inserted VALUES (9000001, '2026-03-01 12:00 UTC', 1, 'by name');
COMMIT;
});
wait_for(waiting_sql('%INSERT INTO ev_p20260301%'), 'the writer to wait');
finish($conv);
is($conv->{status}, 0, '1: the conversion succeeded') or diag $conv->{err};
finish($w);
is($w->{status}, 0, '1: the writer that names the partition went on') or diag $w->{err};
is(methods(), 'ev_p20260301:columnar,ev_p20260302:heap,ev_p20260303:heap', '1: the partition is columnar');
is(q1(q{SELECT count(*) FROM ev WHERE id = 9000001}), '1', '1: the row of the writer is there once');
is(q1(q{SELECT a.amname FROM ev e JOIN pg_class c ON c.oid = e.tableoid JOIN pg_am a ON a.oid = c.relam WHERE e.id = 9000001}),
	'columnar', '1: and it is in the columnar partition');
is(digest_of('ev'), expected(), '1: count and checksum of every row are those of the rows inserted');
is(strays(), '0', '1: no stray table');
back_to_heap();
is(digest_of('ev'), expected(), '1: and back to heap, the same rows');

# ---- 2. A writer through the parent: the swap gives way, the row is kept, the next attempt converts it.
$conv = spawn($convert . ";\n");
wait_for(copying_sql('ev_p20260301'), 'the copy to start');
$w = spawn(q{
BEGIN;
INSERT INTO ev VALUES (9000002, '2026-03-01 13:00 UTC', 2, 'through the parent');
INSERT INTO inserted VALUES (9000002, '2026-03-01 13:00 UTC', 2, 'through the parent');
COMMIT;
});
wait_for(waiting_sql('%INSERT INTO ev VALUES (9000002%'), 'the writer to wait');
finish($conv);
isnt($conv->{status}, 0, '2: the conversion gave way');
like($conv->{err}, qr/swap_lock_timeout/, '2: with swap_lock_timeout');
unlike($conv->{err}, qr/ERROR:\s+deadlock detected/, '2: and no deadlock was detected');
finish($w);
is($w->{status}, 0, '2: the writer through the parent went on') or diag $w->{err};
unlike($w->{err}, qr/ERROR:\s+deadlock detected/, '2: it was not the writer that lost');
is(methods(), 'ev_p20260301:heap,ev_p20260302:heap,ev_p20260303:heap', '2: the partition is still heap');
is(strays(), '0', '2: the copy was discarded, no stray table');
is(digest_of('ev'), expected(), '2: no row lost, none doubled: the writer\'s row is in the old partition');
$conv = finish(spawn($convert . ";\n"));
is($conv->{status}, 0, '2: the next attempt converts') or diag $conv->{err};
is(methods(), 'ev_p20260301:columnar,ev_p20260302:heap,ev_p20260303:heap', '2: the partition is columnar');
is(digest_of('ev'), expected(), '2: with the writer\'s row');
back_to_heap();

# ---- 3. The same through the policy: recorded, retried soon, converted by the next run.
q1(q{SELECT kingres.timepart_set_policy('ev', '1 day', create_ahead => '1 day', columnar_after => '12 hours')});
my $asof = q{'2026-03-02 12:00 UTC'};
$conv = spawn(qq{CALL kingres.timepart_run('ev', $asof, '{convert}');\n});
wait_for(copying_sql('ev_p20260301'), 'the copy of the policy run to start');
$w = spawn(q{
BEGIN;
INSERT INTO ev VALUES (9000003, '2026-03-01 14:00 UTC', 3, 'through the parent, policy');
INSERT INTO inserted VALUES (9000003, '2026-03-01 14:00 UTC', 3, 'through the parent, policy');
COMMIT;
});
wait_for(waiting_sql('%INSERT INTO ev VALUES (9000003%'), 'the writer to wait');
finish($conv);
is($conv->{status}, 0, '3: the run itself came back') or diag $conv->{err};
finish($w);
is($w->{status}, 0, '3: the writer went on') or diag $w->{err};
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}), 'partial', '3: the run is partial');
is(q1(q{SELECT a.state || ':' || left(a.detail, 17) FROM kingres.timepart_run_action a
	WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run) AND a.kind = 'convert'}),
	'failed:swap_lock_timeout', '3: the action is failed, with swap_lock_timeout');
is(q1(qq{SELECT (next_run_at = ${asof}::timestamptz + interval '5 minutes')::text FROM kingres.timepart_policy WHERE table_name = 'ev'}),
	'true', '3: the next run is due in five minutes');
like(q1(qq{SELECT string_agg(problem, ',' ORDER BY problem) FROM kingres.timepart_check('1 hour', $asof)}),
	qr/convert_blocked/, '3: the check says the conversion is blocked');
is(methods(), 'ev_p20260301:heap,ev_p20260302:heap,ev_p20260303:heap', '3: still heap');
is(digest_of('ev'), expected(), '3: no row lost, none doubled');
q1(qq{CALL kingres.timepart_run('ev', $asof, '{convert}')});
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}), 'done', '3: the next run is done');
is(methods(), 'ev_p20260301:columnar,ev_p20260302:heap,ev_p20260303:heap', '3: the partition is columnar');
is(digest_of('ev'), expected(), '3: with the writer\'s row');
q1(q{SELECT kingres.timepart_unset_policy('ev')});
back_to_heap();

# ---- 4. A long reader of the parent does the same to the swap.
my $rd = $node->background_psql('postgres');
$rd->query_safe(q{BEGIN; SELECT count(*) FROM ev;});
$conv = finish(spawn($convert . ";\n"));
isnt($conv->{status}, 0, '4: the conversion gave way to the reader');
like($conv->{err}, qr/swap_lock_timeout/, '4: with swap_lock_timeout');
is(methods(), 'ev_p20260301:heap,ev_p20260302:heap,ev_p20260303:heap', '4: still heap');
is($rd->query_safe(q{SELECT count(*) FROM ev}), (split /\|/, expected())[0],
	'4: the reader still sees every row, in its transaction');
$rd->query_safe(q{COMMIT});
$rd->quit;
$conv = finish(spawn($convert . ";\n"));
is($conv->{status}, 0, '4: after the reader, the conversion succeeds') or diag $conv->{err};
is(digest_of('ev'), expected(), '4: the same rows');
back_to_heap();

# ---- 5. Readers across the swap never see zero or two copies of the partition.
my $pre = q1(q{SELECT count(*) || '|' || sum(id) FROM ev});
my $reader = spawn(q{
SET timezone = 'UTC';
SELECT 'r:' || count(*) || '|' || sum(id) FROM ev t
\watch 0.003
});
wait_for(q{SELECT count(*) FROM pg_stat_activity WHERE query LIKE '%FROM ev t%' AND pid <> pg_backend_pid()}, 'the reader to start');
usleep(200_000);
$conv = finish(spawn($convert . ";\n"));
is($conv->{status}, 0, '5: the conversion succeeded with a reader running') or diag $conv->{err};
usleep(300_000);
q1(q{SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE '%FROM ev t%' AND pid <> pg_backend_pid()});
eval { finish($reader); };
my @lines = grep { /^r:/ } split /\n/, $reader->{out};
cmp_ok(scalar(@lines), '>', 6, '5: the reader read many times (' . scalar(@lines) . ')');
my %seen = map { $_ => 1 } @lines;
is(scalar(keys %seen), 1, '5: every read gave the same count and checksum');
is((keys %seen)[0], 'r:' . $pre, '5: and they are the right ones');
back_to_heap();

# ---- 6. A conversion cancelled while it copies leaves the old partition.
my ($out, $err);
my $ret = $node->psql('postgres',
	q{SET statement_timeout = '60ms'; CALL kingres.convert_partition('ev', 'ev_p20260301', 'columnar')},
	stdout => \$out, stderr => \$err);
isnt($ret, 0, '6: the conversion was cancelled');
like($err, qr/statement timeout/, '6: by the statement timeout');
is(methods(), 'ev_p20260301:heap,ev_p20260302:heap,ev_p20260303:heap', '6: the old partition is there');
is(strays(), '0', '6: no stray table');
is(digest_of('ev'), expected(), '6: the same rows');

# ---- 7. Through timepart_run_action, the timeout is an action that failed and the run goes on.
q1(q{SELECT kingres.timepart_set_policy('ev', '1 day', create_ahead => '1 day', columnar_after => '12 hours')});
q1(qq{SELECT kingres.timepart_begin_run('ev', $asof, 'manual')});
my $run = q1(q{SELECT max(run_id) FROM kingres.timepart_run});
$ret = $node->psql('postgres', qq{SET statement_timeout = '60ms'; SELECT kingres.timepart_run_action($run, 'convert')},
	stdout => \$out, stderr => \$err);
is($ret, 0, '7: the call came back') or diag $err;
is(q1(qq{SELECT a.state || ':' || left(a.detail, 17) FROM kingres.timepart_run_action a WHERE a.run_id = $run AND a.kind = 'convert'}),
	'failed:statement_timeout', '7: the action is recorded as failed, statement_timeout');
is(methods(), 'ev_p20260301:heap,ev_p20260302:heap,ev_p20260303:heap', '7: still heap');
is(strays(), '0', '7: no stray table');
q1(qq{SELECT kingres.timepart_finish_run($run)});
q1(qq{CALL kingres.timepart_run('ev', $asof, '{convert}')});
is(methods(), 'ev_p20260301:columnar,ev_p20260302:heap,ev_p20260303:heap', '7: the next run converts');
is(digest_of('ev'), expected(), '7: the same rows');
is(q1(q{SELECT kingres.columnar_check_clean()}), 't', 'the columnar files and the stripe catalog are clean');

$node->stop;
done_testing();
