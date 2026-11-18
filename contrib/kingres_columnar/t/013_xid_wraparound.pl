# contrib/kingres_columnar/t/013_xid_wraparound.pl
#
# A columnar table through two laps of the transaction id space.
#
# The metapage of a columnar file holds the id of the transaction that created
# it.  Left alone it would, after exactly one lap (4 294 967 296 transactions),
# name whichever transaction then runs with the same 32-bit number, and every
# read of the table would fail with a serialization failure while that
# transaction lives.  VACUUM freezes the id and tells PostgreSQL the oldest
# one that is left (relfrozenxid), so that autovacuum's protection against
# wraparound reaches the table.  This test consumes ids with the
# xid_wraparound test module (src/test/modules/xid_wraparound of the
# PostgreSQL tree; skipped where it is not installed), with autovacuum doing
# the freezing, and at the same point of each lap holds a transaction whose
# id has the number the table was created with.
#
# It takes several minutes; KC_SKIP_WRAPAROUND=1 skips it.
#
# The waits for autovacuum are bounded for a busy machine (15 minutes each, not the
# 3 of poll_query_until), autovacuum logs every run (log_autovacuum_min_duration = 0),
# and a wait that gives up prints the state of the server (sessions, locks, what can hold
# the oldest xmin back, the age and the autovacuum statistics of the tables, the autovacuum
# lines of the log) and says nothing about the cause.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

plan skip_all => 'KC_SKIP_WRAPAROUND is set' if $ENV{KC_SKIP_WRAPAROUND};

my $node = PostgreSQL::Test::Cluster->new('colwrap');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
autovacuum = on
autovacuum_naptime = 1s
autovacuum_freeze_max_age = 150000000
vacuum_freeze_min_age = 1000000
log_min_messages = warning
log_autovacuum_min_duration = 0
});
$node->start;

my $avail = $node->safe_psql('postgres',
	q{SELECT count(*) FROM pg_available_extensions WHERE name = 'xid_wraparound'});
if ($avail ne '1')
{
	$node->stop;
	plan skip_all => 'the xid_wraparound test module is not installed';
}

$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE EXTENSION xid_wraparound;
CREATE TABLE c (a int, t text) USING columnar;
INSERT INTO c SELECT g, 'x' || g FROM generate_series(1, 2000) g;
CREATE TABLE h (a int);
CREATE TABLE c0 (a int);
INSERT INTO c0 SELECT generate_series(1, 100);
ALTER TABLE c0 SET ACCESS METHOD columnar;
});
my $x = $node->safe_psql('postgres',
	q{SELECT xmin::text::bigint FROM pg_class WHERE relname = 'c'});
my $has_fn = $node->safe_psql('postgres',
	q{SELECT count(*) FROM pg_proc WHERE proname = 'columnar_creating_xid'});
cmp_ok($x, '>', 3, "the table was created by transaction $x");

# What the server looks like, printed when a wait gives up: every session with its
# transaction id, xmin and wait event, the locks of the tables and the ungranted ones, what can
# hold the oldest xmin back (slots, prepared transactions), the age of every table, what the
# statistics say autovacuum did (the columnar vacuum prints no "automatic vacuum of table"
# line, the statistics are reported by it) and the autovacuum lines of the server log.
sub dump_state
{
	my @queries = (
		[ 'sessions', q{SELECT pid, backend_type, state, wait_event_type, wait_event, backend_xid, backend_xmin, left(query, 50) FROM pg_stat_activity ORDER BY pid} ],
		[ 'locks of the tables, and every lock not granted', q{SELECT l.pid, l.locktype, coalesce(l.relation::regclass::text, '-') rel, l.mode, l.granted FROM pg_locks l WHERE l.locktype <> 'virtualxid' AND (NOT l.granted OR l.relation IN ('c'::regclass, 'h'::regclass, 'c0'::regclass)) ORDER BY 1, 3} ],
		[ 'replication slots', q{SELECT slot_name, xmin, catalog_xmin FROM pg_replication_slots} ],
		[ 'prepared transactions', q{SELECT gid, transaction FROM pg_prepared_xacts} ],
		[ 'the oldest ids', q{SELECT 'txid_current ' || pg_current_xact_id()::text || ', datfrozenxid age ' || (SELECT age(datfrozenxid) FROM pg_database WHERE datname = 'postgres') || ', oldest backend xmin age ' || coalesce((SELECT max(age(backend_xmin))::text FROM pg_stat_activity), 'none')} ],
		[ 'tables with the age of relfrozenxid and what the statistics say of autovacuum', q{SELECT c.oid::regclass, c.relkind, c.relam, age(c.relfrozenxid) AS age, s.autovacuum_count, s.last_autovacuum, s.vacuum_count FROM pg_class c LEFT JOIN pg_stat_all_tables s ON s.relid = c.oid WHERE c.relkind IN ('r', 't', 'm') AND age(c.relfrozenxid) > 50000000 ORDER BY 4 DESC LIMIT 12} ],
	);
	my $out = "load: " . (-r '/proc/loadavg' ? slurp_file('/proc/loadavg') : "unknown\n");
	foreach my $q (@queries)
	{
		my ($ret, $o, $e) = $node->psql('postgres', $q->[1]);
		$out .= "-- $q->[0]\n" . ($ret == 0 ? $o : "(failed: $e)") . "\n";
	}
	my @log = grep { /autovacuum|wraparound|vacuum/i } split /\n/, slurp_file($node->logfile);
	@log = @log[-25 .. -1] if @log > 25;
	$out .= "-- the last autovacuum lines of the server log\n" . (@log ? join("\n", @log) : 'none') . "\n";
	diag($out);
}

# wait for a condition, up to 15 minutes; on failure print the state (above) and die
sub wait_for
{
	my ($sql, $what) = @_;
	my $deadline = time() + 900;
	while (time() < $deadline)
	{
		return 1 if $node->safe_psql('postgres', $sql) eq 't';
		sleep 1;
	}
	dump_state();
	diag("FAILED: $what (the condition: $sql)");
	die "$what (see the state printed above)";
}

sub current_xid
{
	return $node->safe_psql('postgres', q{SELECT pg_current_xact_id()::text::bigint});
}

# consume ids in steps, letting autovacuum freeze the table between them
sub consume_to
{
	my ($target) = @_;
	while ($target - current_xid() > 100_000_000)
	{
		$node->safe_psql('postgres', 'SELECT consume_xids(100000000)');
		# The table is protected when its age stays at or just above the age at which autovacuum
		# acts (autovacuum_freeze_max_age, 150 million): the limit is 160 million.  It used to be
		# 140 million, below the trigger, which left a band (140 to 150 million) in which the
		# table is not yet due for autovacuum, which therefore does nothing, and in which the
		# test waited for ever: a vacuum that fires 50 to 60 million ids into a batch leaves the
		# table 40 to 50 million old, and the next batch ends at 140 to 150 million.  Heap does
		# the same in that band (checked: both kinds of table stay at 145 million for as long as
		# one waits and are vacuumed once past 150 million).
		wait_for(q{SELECT age(relfrozenxid) < 160000000 FROM pg_class WHERE relname = 'c'},
			'autovacuum did not advance relfrozenxid of the columnar table');
		wait_for(q{SELECT age(datfrozenxid) < 400000000 FROM pg_database WHERE datname = 'postgres'},
			'datfrozenxid did not advance');
	}
	my $left = $target - current_xid();
	$node->safe_psql('postgres', "SELECT consume_xids($left)") if $left > 0;
}

foreach my $lap (1, 2)
{
	my $cur = current_xid();
	my $target = (int($cur / 4294967296) + 1) * 4294967296 + $x - 40;
	consume_to($target);

	# a transaction with the id the table was created with
	my $hold = $node->background_psql('postgres', on_error_stop => 0);
	my $got = -1;
	for my $try (1 .. 100)
	{
		$hold->query_safe('BEGIN');
		my $n = $hold->query_safe(q{SELECT pg_current_xact_id()::text::bigint % 4294967296});
		$n =~ s/\s+//g;
		if ($n == $x) { $got = $n; last; }
		$hold->query_safe('ROLLBACK');
	}
	is($got, $x, "lap $lap: a transaction with the creation id $x is open");

	foreach my $level ('READ COMMITTED', 'REPEATABLE READ')
	{
		my ($ret, $out, $err) = $node->psql('postgres',
			"BEGIN ISOLATION LEVEL $level; SELECT count(*) FROM c; COMMIT");
		is($ret, 0, "lap $lap: a $level read of the table works") or diag($err);
		like($out, qr/200[0-9]/, "lap $lap: and sees every row");
	}
	$node->safe_psql('postgres', "INSERT INTO c VALUES (-$lap, 'lap')");
	$hold->query_safe('ROLLBACK');
	$hold->quit;

	is($node->safe_psql('postgres', q{SELECT relfrozenxid::text::bigint > 2 FROM pg_class WHERE relname = 'c'}),
		't', "lap $lap: relfrozenxid of the table is a real id");
	if ($has_fn eq '1')
	{
		is($node->safe_psql('postgres', q{SELECT kingres.columnar_creating_xid('c')::text::bigint}),
			'2', "lap $lap: the id in the metapage is frozen");
		# the table's creation is a second id in the metapage: frozen the same way,
		# so it cannot name the transaction that holds its number either
		is($node->safe_psql('postgres', q{SELECT kingres.columnar_table_xid('c')::text::bigint}),
			'2', "lap $lap: the table's creation in the metapage is frozen too");
		is($node->safe_psql('postgres', q{SELECT kingres.columnar_table_xid('c0')::text::bigint}),
			'0', "lap $lap: a creation that was never known stays 0, and is no id that can alias");
		is($node->safe_psql('postgres', q{SELECT count(*) FROM c0}), 100,
			"lap $lap: the converted table reads");
	}
	$node->safe_psql('postgres', 'VACUUM c');
	is($node->safe_psql('postgres', 'SELECT count(*) FROM c'), 2000 + $lap,
		"lap $lap: the table has its rows");
}

$node->stop;
done_testing();
