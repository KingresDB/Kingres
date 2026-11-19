# contrib/kingres_timepart/t/006_convert_crash.pl
#
# A crash (an immediate stop) at each step of a conversion (design 10.4 and
# 10.7): after the restart the partition is the old heap partition, whole, or
# the complete columnar one, never half, no stray table is left, the columnar
# files and the stripe catalog are clean, and a rerun finishes.
#
# The steps are the DDL statements of the conversion, and an event trigger
# stops the session before each of them: the trigger takes an advisory lock
# (7777, n) for the n-th statement of its session, and the test holds that
# lock in another session while it crashes the server.  Two more points are
# not statements: the middle of the copy (a big partition, the server is
# stopped while the copy holds its lock), and just after the commit.  The same
# for a run of the policy: the run is left "running" and the next run marks it
# interrupted and converts.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(usleep);

my $node = PostgreSQL::Test::Cluster->new('conv_crash');
$node->init;
$node->append_conf('postgresql.conf', q{
fsync = off
shared_preload_libraries = 'kingres_columnar'
max_prepared_transactions = 5
kingres_timepart.trust_table_owners = on
});
$node->start;

$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE EXTENSION kingres_timepart;
ALTER DATABASE postgres SET search_path = public;
ALTER DATABASE postgres SET timezone = 'UTC';
CREATE ROLE reader NOLOGIN;
});

# p1 is small and is converted at every step; p2 is big, for the crash in the
# middle of the copy.
$node->safe_psql('postgres', q{
CREATE TABLE ev (id bigint, ts timestamptz NOT NULL, dev int, payload text) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('ev', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
INSERT INTO ev SELECT i, '2026-03-01 UTC'::timestamptz + (i % 86400) * interval '1 second', i % 9, md5(i::text)
  FROM generate_series(1, 30000) i;
INSERT INTO ev SELECT i, '2026-03-02 UTC'::timestamptz + (i % 86400) * interval '1 second', i % 9, md5(i::text) || repeat('y', i % 50)
  FROM generate_series(100001, 900000) i;
INSERT INTO ev SELECT i, '2026-03-03 UTC'::timestamptz + (i % 86400) * interval '1 second', i % 9, md5(i::text)
  FROM generate_series(1000001, 1005000) i;
GRANT SELECT ON ev_p20260301 TO reader;
GRANT UPDATE (payload) ON ev_p20260301 TO reader;
COMMENT ON TABLE ev_p20260301 IS 'the first day';
ANALYZE ev;
});

sub q1
{
	my ($sql) = @_;
	return $node->safe_psql('postgres', $sql);
}

sub digest_of
{
	my ($rel) = @_;
	return q1(qq{SELECT count(*) || '|' || coalesce(sum(hashtext(t::text)::numeric), 0) FROM $rel t});
}

sub am_of
{
	my ($part) = @_;
	return q1(qq{SELECT a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.relname = '$part'});
}

sub strays
{
	return q1(q{SELECT count(*) FROM pg_class WHERE relname LIKE '%\_\_kc'});
}

my $base = digest_of('ev');
my $base1 = digest_of('ev_p20260301');
my $base2 = digest_of('ev_p20260302');

# The gate: the n-th DDL statement of a session passes through the advisory
# lock (7777, n), which the test can hold; every statement is logged too.
$node->safe_psql('postgres', q{
CREATE TABLE gate_log (n int, tag text);
CREATE FUNCTION tp_gate() RETURNS event_trigger LANGUAGE plpgsql AS $$
DECLARE n int;
BEGIN
    n := coalesce(nullif(current_setting('tp.n', true), ''), '0')::int + 1;
    PERFORM set_config('tp.n', n::text, false);
    INSERT INTO public.gate_log VALUES (n, tg_tag);
    PERFORM pg_advisory_lock(7777, n);
    PERFORM pg_advisory_unlock(7777, n);
END $$;
CREATE EVENT TRIGGER tp_gate ON ddl_command_start EXECUTE FUNCTION tp_gate();
});

my $convert = q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'columnar')};

# A probe: the statements of a conversion, in order.  The attach of the new
# partition scans nothing: the CHECK of the bound that the new table is made
# with implies the partition constraint (the server says so at debug level).
my $off = -s $node->logfile;
q1("SET log_min_messages = debug1; $convert");
ok($node->log_contains(qr/partition constraint for table "ev_p20260301__kc" is implied by existing constraints/, $off),
	'the attach scanned nothing: the CHECK of the bound implies the partition constraint');
my $steps = q1(q{SELECT string_agg(tag, ',' ORDER BY n) FROM gate_log});
my $K = q1(q{SELECT max(n) FROM gate_log});
cmp_ok($K, '>=', 6, "the conversion has $K statements that can be stopped before: $steps");
is(am_of('ev_p20260301'), 'columnar', 'the probe converted the partition');
is(digest_of('ev_p20260301'), $base1, 'with every row');
q1(q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});
is(am_of('ev_p20260301'), 'heap', 'and back');
q1(q{TRUNCATE gate_log});

# Hold the lock of the n-th statement in one session, run the call in another
# until it waits for that lock, and crash the server.
sub crash_at
{
	my ($n, $call) = @_;
	my $holder = $node->background_psql('postgres');
	$holder->query_safe("SELECT pg_advisory_lock(7777, $n)");
	my $runner = $node->background_psql('postgres', on_error_stop => 0);
	$runner->query_until(qr/started/, "\\echo started\n$call;\n");
	my $waiting = 0;
	for (1 .. 1500)
	{
		$waiting = $node->safe_psql('postgres',
			qq{SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND classid = 7777 AND objid = $n});
		last if $waiting ne '0';
		usleep(20_000);
	}
	$node->stop('immediate');
	eval { $runner->quit };
	eval { $holder->quit };
	$node->start;
	return $waiting;
}

# What must hold after any crash before the commit.
sub old_state
{
	my ($what) = @_;
	is(am_of('ev_p20260301'), 'heap', "$what: the partition is the old heap partition");
	is(digest_of('ev_p20260301'), $base1, "$what: with every row");
	is(digest_of('ev'), $base, "$what: and the table has every row");
	is(strays(), '0', "$what: no stray table");
	is(q1(q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'ev'::regclass}), '3', "$what: three partitions");
	is(q1(q{SELECT count(*) || ':' || coalesce(max(obj_description(oid, 'pg_class')), '') FROM pg_class WHERE relname = 'ev_p20260301'}),
		'1:the first day', "$what: one table of that name, with its comment");
	is(q1(q{SELECT relacl::text FROM pg_class WHERE relname = 'ev_p20260301'}) =~ /reader=r\// ? 'yes' : 'no', 'yes',
		"$what: and its privileges");
	is(q1(q{SELECT kingres.columnar_check_clean()}), 't', "$what: the columnar files and the stripe catalog are clean");
}

# ---- 1. A crash before each statement.
for my $n (1 .. $K)
{
	my $tag = (split /,/, $steps)[$n - 1];
	my $waiting = crash_at($n, $convert);
	is($waiting, '1', "statement $n ($tag): the conversion was stopped before it");
	old_state("crash before statement $n ($tag)");
	# a rerun finishes
	q1($convert);
	is(am_of('ev_p20260301'), 'columnar', "after the crash before statement $n: the rerun converts");
	is(digest_of('ev_p20260301'), $base1, "after the crash before statement $n: with every row");
	is(q1(q{SELECT kingres.columnar_check_clean()}), 't', "after the crash before statement $n: clean");
	q1(q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});
}

# ---- 2. A crash in the middle of the copy of a big partition.
{
	my $runner = $node->background_psql('postgres', on_error_stop => 0);
	$runner->query_until(qr/started/, "\\echo started\nCALL kingres.convert_partition('ev', 'ev_p20260302', 'columnar');\n");
	my $copying = 0;
	for (1 .. 1500)
	{
		$copying = $node->safe_psql('postgres', q{SELECT count(*) FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
			WHERE c.relname = 'ev_p20260302' AND l.mode = 'ShareRowExclusiveLock' AND l.granted});
		last if $copying ne '0';
		usleep(5_000);
	}
	is($copying, '1', 'copy: the conversion holds the partition and is copying');
	usleep(150_000);
	$node->stop('immediate');
	eval { $runner->quit };
	$node->start;
	# the copy was cut: the old partition, whole (or, if the copy was over, the new one, whole)
	my $am = am_of('ev_p20260302');
	ok($am eq 'heap' || $am eq 'columnar', "copy: the partition is whole, as $am");
	is(digest_of('ev_p20260302'), $base2, 'copy: with every row');
	is(digest_of('ev'), $base, 'copy: and the table has every row');
	is(strays(), '0', 'copy: no stray table');
	is(q1(q{SELECT kingres.columnar_check_clean()}), 't', 'copy: clean');
	q1(q{CALL kingres.convert_partition('ev', 'ev_p20260302', 'columnar')}) if $am eq 'heap';
	is(am_of('ev_p20260302'), 'columnar', 'copy: the rerun converts');
	is(digest_of('ev_p20260302'), $base2, 'copy: with every row');
	q1(q{CALL kingres.convert_partition('ev', 'ev_p20260302', 'heap')});
}

# ---- 3. A crash just after the commit: the complete columnar partition.
{
	my $holder = $node->background_psql('postgres');
	q1($convert);
	$node->stop('immediate');
	eval { $holder->quit };
	$node->start;
	is(am_of('ev_p20260301'), 'columnar', 'after commit: the partition is columnar');
	is(digest_of('ev_p20260301'), $base1, 'after commit: with every row');
	is(digest_of('ev'), $base, 'after commit: and the table has every row');
	is(strays(), '0', 'after commit: no stray table');
	is(q1(q{SELECT kingres.columnar_check_clean()}), 't', 'after commit: clean');
	is(q1(q{SELECT i.rows FROM kingres.columnar_info('ev_p20260301') i}), q1(q{SELECT count(*) FROM ev_p20260301}),
		'after commit: the rows of the stripes are the rows of the partition');
	q1(q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});
}

# ---- 4. A crash in the middle of a run of the policy: the run is left running,
#         the next run marks it interrupted and converts.
q1(q{SELECT kingres.timepart_set_policy('ev', '1 day', create_ahead => '1 day', columnar_after => '12 hours')});
my $asof = q{'2026-03-02 12:00 UTC'};
for my $n (3, $K)
{
	my $waiting = crash_at($n, qq{CALL kingres.timepart_run('ev', $asof, '{convert}')});
	is($waiting, '1', "policy run: stopped before statement $n");
	old_state("policy run, crash before statement $n");
	is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}), 'running', "policy run $n: the run is left running");
	q1(qq{CALL kingres.timepart_run('ev', $asof, '{convert}')});
	is(q1(q{SELECT string_agg(state || ':' || coalesce(error, ''), ',' ORDER BY run_id) FROM (SELECT * FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 2) r}),
		'failed:interrupted,done:', "policy run $n: the next run marks it interrupted and is done");
	is(am_of('ev_p20260301'), 'columnar', "policy run $n: converted");
	is(digest_of('ev_p20260301'), $base1, "policy run $n: with every row");
	is(q1(q{SELECT a.state FROM kingres.timepart_run_action a WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run) AND a.kind = 'convert'}),
		'done', "policy run $n: the action is recorded");
	q1(q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});
}

$node->stop;
done_testing();
