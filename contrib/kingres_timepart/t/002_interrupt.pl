# contrib/kingres_timepart/t/002_interrupt.pl
#
# A run interrupted half way, and repeated.  The runs are idempotent: what an
# interrupted run did stays, what it did not do is done by the next run, and
# the result is the one an uninterrupted run gives.
#
#   * a backend that is terminated in the middle of the create phase leaves
#     partitions that are complete and attached (every partition is created
#     and attached in one transaction), no stray table, and a run that is
#     still marked running; the next run marks it interrupted, creates the
#     rest and ends with the same partitions as an uninterrupted run;
#   * the same for the retire phase;
#   * an immediate shutdown (a crash) in the middle of a run: the same;
#   * a run repeated after it has finished does nothing.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: PostgreSQL

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(usleep);

my $node = PostgreSQL::Test::Cluster->new('interrupt');
$node->init;
$node->append_conf('postgresql.conf', "fsync = off\n");
$node->start;

# Tables without a schema go to public, whatever the name of the role.
$node->safe_psql('postgres', q{CREATE EXTENSION kingres_timepart; ALTER DATABASE postgres SET search_path = public;});
$node->safe_psql('postgres', q{
CREATE TABLE ev (id int, ts timestamptz NOT NULL, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
CREATE TABLE ref (id int, ts timestamptz NOT NULL, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
});

sub q1
{
	my ($sql) = @_;
	return $node->safe_psql('postgres', "SET timezone = 'UTC'; $sql");
}

sub nparts
{
	my ($t) = @_;
	return q1(qq{SELECT count(*) FROM pg_inherits WHERE inhparent = '$t'::regclass});
}

sub stray
{
	return q1(q{SELECT count(*) FROM pg_class WHERE relkind = 'r' AND NOT relispartition
		AND (relname LIKE 'ev\_p%' OR relname LIKE 'ref\_p%')});
}

# The same policy on two tables: "ref" is run without interruption and is the
# model of what "ev" must end up as.
foreach my $t (qw(ev ref))
{
	q1(qq{SELECT kingres.timepart_set_policy('$t', '1 hour', create_ahead => '400 hours', retention => '6 hours', run_every => '1 hour')});
}

# Start a run in a background session and wait until it has created some
# partitions but not all; return the pid of its backend.
sub start_run_and_wait
{
	my ($bg, $table, $asof, $phase, $watch_sql, $min) = @_;
	my $pid = $bg->query_safe(q{SELECT pg_backend_pid();});
	chomp $pid;
	# The run goes on in the background: send it and come back at once.
	$bg->query_until(qr/run_started/,
		"SET timezone = 'UTC';\n\\echo run_started\nCALL kingres.timepart_run('$table', '$asof', '{$phase}');\n");
	for (1 .. 600)
	{
		my $n = q1($watch_sql);
		return ($pid, $n) if $n >= $min;
		usleep(50_000);
	}
	return ($pid, -1);
}

# ---- 1. Terminate the backend in the middle of the create phase.
$node->safe_psql('postgres', q{SET timezone = 'UTC'; CALL kingres.timepart_run('ref', '2026-03-10 00:00 UTC');});
my $expected = nparts('ref');
cmp_ok($expected, '>', 300, 'the model run created hundreds of partitions');

my $bg = $node->background_psql('postgres', on_error_stop => 0);
my ($pid, $n) = start_run_and_wait($bg, 'ev', '2026-03-10 00:00 UTC', 'create',
	q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'ev'::regclass}, 20);
cmp_ok($n, '>=', 20, 'the run is under way');
$node->safe_psql('postgres', "SELECT pg_terminate_backend($pid)");
# Wait for the backend to be gone.
for (1 .. 200)
{
	last if $node->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE pid = $pid") eq '0';
	usleep(50_000);
}
eval { $bg->quit; };
my $left = nparts('ev');
cmp_ok($left, '<', $expected, 'the interrupted run did not finish');
is(stray(), '0', 'no stray table: every partition is created and attached in one transaction');
is(q1(q{SELECT count(*) FROM kingres.timepart_run WHERE table_name = 'ev' AND state = 'running'}),
	'1', 'its run is still marked running');
is(q1(q{SELECT count(*) FROM (
	SELECT lo, lag(hi) OVER (ORDER BY lo) AS prev_hi FROM kingres.timepart__children('ev'::regclass::oid) c WHERE NOT c.is_default) g
	WHERE prev_hi IS NOT NULL AND prev_hi <> lo}), '0', 'the partitions that exist have no gap between them');

# The next run takes over.
$node->safe_psql('postgres', q{SET timezone = 'UTC'; CALL kingres.timepart_run('ev', '2026-03-10 00:00 UTC');});
is(nparts('ev'), $expected, 'the next run ends with the partitions of an uninterrupted run');
is(q1(q{SELECT string_agg(state || ':' || coalesce(error, ''), ',' ORDER BY run_id) FROM kingres.timepart_run WHERE table_name = 'ev'}),
	'failed:interrupted,done:', 'the interrupted run is marked, the next is done');
is(q1(q{SELECT (SELECT array_agg(substr(c.relname, 4) ORDER BY c.relname) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'ev'::regclass)
	= (SELECT array_agg(substr(c.relname, 5) ORDER BY c.relname) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'ref'::regclass)}),
	't', 'the same set of partitions');
is(q1(q{SELECT (SELECT array_agg(lo ORDER BY lo) FROM kingres.timepart__children('ev'::regclass::oid) c)
	= (SELECT array_agg(lo ORDER BY lo) FROM kingres.timepart__children('ref'::regclass::oid) c)}),
	't', 'with the same bounds');
is(stray(), '0', 'still no stray table');

# Repeating a finished run does nothing.
$node->safe_psql('postgres', q{SET timezone = 'UTC'; CALL kingres.timepart_run('ev', '2026-03-10 00:00 UTC');});
is(q1(q{SELECT count(*) FROM kingres.timepart_run_action a WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'ev')}),
	'0', 'a repeated run has nothing to do');
is(nparts('ev'), $expected, 'and changes nothing');

# ---- 2. Terminate the backend in the middle of the retire phase.
# A day later, the partitions older than six hours go: hundreds of.
$node->safe_psql('postgres', q{SET timezone = 'UTC'; CALL kingres.timepart_run('ref', '2026-03-19 00:00 UTC', '{retire}');});
my $ref_after = nparts('ref');
cmp_ok($ref_after, '<', $expected, 'the model run retired partitions');
$bg = $node->background_psql('postgres', on_error_stop => 0);
($pid, $n) = start_run_and_wait($bg, 'ev', '2026-03-19 00:00 UTC', 'retire',
	qq{SELECT $expected - count(*) FROM pg_inherits WHERE inhparent = 'ev'::regclass}, 20);
cmp_ok($n, '>=', 20, 'the retirement is under way');
$node->safe_psql('postgres', "SELECT pg_terminate_backend($pid)");
for (1 .. 200)
{
	last if $node->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE pid = $pid") eq '0';
	usleep(50_000);
}
eval { $bg->quit; };
cmp_ok(nparts('ev'), '>', $ref_after, 'the interrupted retirement did not finish');
cmp_ok(nparts('ev'), '<', $expected, 'but did something');
$node->safe_psql('postgres', q{SET timezone = 'UTC'; CALL kingres.timepart_run('ev', '2026-03-19 00:00 UTC', '{retire}');});
is(nparts('ev'), $ref_after, 'the next run retires the rest');
is(q1(q{SELECT (SELECT array_agg(lo ORDER BY lo) FROM kingres.timepart__children('ev'::regclass::oid) c)
	= (SELECT array_agg(lo ORDER BY lo) FROM kingres.timepart__children('ref'::regclass::oid) c)}),
	't', 'with the same bounds as the uninterrupted one');

# ---- 3. A crash of the server in the middle of a run.
$node->safe_psql('postgres', q{
CREATE TABLE ev2 (id int, ts timestamptz NOT NULL, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
CREATE TABLE ref2 (id int, ts timestamptz NOT NULL, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('ev2', '1 hour', create_ahead => '300 hours');
SELECT kingres.timepart_set_policy('ref2', '1 hour', create_ahead => '300 hours');
SET timezone = 'UTC';
CALL kingres.timepart_run('ref2', '2026-03-10 00:00 UTC');
});
my $expected2 = q1(q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'ref2'::regclass});
$bg = $node->background_psql('postgres', on_error_stop => 0);
($pid, $n) = start_run_and_wait($bg, 'ev2', '2026-03-10 00:00 UTC', 'create',
	q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'ev2'::regclass}, 20);
cmp_ok($n, '>=', 20, 'the run is under way again');
$node->stop('immediate');
eval { $bg->quit; };
$node->start;
my $after_crash = q1(q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'ev2'::regclass});
cmp_ok($after_crash, '<', $expected2, 'the crashed run did not finish');
cmp_ok($after_crash, '>=', 1, 'what it committed is there');
is(q1(q{SELECT count(*) FROM pg_class WHERE relkind = 'r' AND NOT relispartition AND relname LIKE 'ev2\_p%'}),
	'0', 'no stray table after crash recovery');
is(q1(q{SELECT count(*) FROM kingres.timepart_run WHERE table_name = 'ev2' AND state = 'running'}),
	'1', 'the run is still marked running');
$node->safe_psql('postgres', q{SET timezone = 'UTC'; CALL kingres.timepart_run('ev2', '2026-03-10 00:00 UTC');});
is(q1(q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'ev2'::regclass}), $expected2,
	'the next run after the crash ends with the partitions of an uninterrupted run');
is(q1(q{SELECT string_agg(state || ':' || coalesce(error, ''), ',' ORDER BY run_id) FROM kingres.timepart_run WHERE table_name = 'ev2'}),
	'failed:interrupted,done:', 'the crashed run is marked interrupted');

$node->stop;
done_testing();
