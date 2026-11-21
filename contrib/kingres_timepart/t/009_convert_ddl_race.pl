# contrib/kingres_timepart/t/009_convert_ddl_race.pl
#
# A conversion that waits for its lock on the partition reads the catalog after
# the lock, not before: a session that holds a lock on the partition when the
# conversion starts can change the partition before it commits, and the
# conversion must copy the partition as it is when it gets the lock.
#
#   * a column added and filled, and a unique index created, on the partition
#     while the conversion waits: the copy has the column's values and the new
#     table has the index (and the uniqueness is enforced);
#   * a column dropped while the conversion waits: the conversion copies the
#     columns that are left;
#   * the partition renamed while the conversion waits: a retryable error
#     (schema_changed), the partition is as it was, and the next call works.
#
# And three things around the swap that the other tests do not pin:
#
#   * the tablespace of the partition, and of each of its indexes, goes to the new
#     partition, both ways;
#   * a reader of the table that holds ACCESS SHARE on it makes the swap give
#     way after the short wait (about half a second), not after the lock
#     timeout of the conversion, and the conversion says that it gave way;
#   * a writer through the table that waits for the partition makes the swap
#     give way before it asks for its lock (the message says which session
#     the conversion saw).
#
# And the changes that need no lock on the table (review T13F-01, T13F-02, T13F-07):
#
#   * a GRANT, a REVOKE, and the rename, comment and storage parameters of an
#     index, committed while the swap waits for its lock: the conversion gives
#     way (schema_changed) and the partition keeps the change; the next call
#     carries it;
#   * the cap of the swap wait: 500 ms, whatever deadlock_timeout is;
#   * the name of the partition taken by another table while the conversion
#     waits for its lock: the check of the lock that is held refuses it.
#
# And a limit that the README names, so that a change in it is noticed: a
# REPEATABLE READ transaction whose snapshot is older than the swap and that
# has not read the table yet finds the converted partition empty.
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

my $node = PostgreSQL::Test::Cluster->new('conv_ddl');
$node->init;
$node->append_conf('postgresql.conf', q{
fsync = off
shared_preload_libraries = 'kingres_columnar'
kingres_timepart.trust_table_owners = on
});
$node->start;

$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE EXTENSION kingres_timepart;
ALTER DATABASE postgres SET search_path = public;
ALTER DATABASE postgres SET timezone = 'UTC';
});

sub q1
{
	my ($sql) = @_;
	return $node->safe_psql('postgres', $sql);
}

# Start psql on a script, in the background; return a handle.
sub spawn
{
	my ($script) = @_;
	my $h = { out => '', err => '', in => $script };
	$h->{run} = IPC::Run::start(
		[ 'psql', '-XAtq', '-v', 'ON_ERROR_STOP=1', '-d', $node->connstr('postgres'), '-f', '-' ],
		'<', \$h->{in}, '>', \$h->{out}, '2>', \$h->{err}, IPC::Run::timeout(120));
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

sub wait_for
{
	my ($sql, $what, $tries) = @_;
	$tries //= 1500;
	for (1 .. $tries)
	{
		my $r = q1($sql);
		return 1 if $r ne '0' && $r ne '';
		usleep(10_000);
	}
	fail("timed out waiting for $what");
	return 0;
}

# a session waits for a lock, and its query matches
sub waiting_sql
{
	my ($like) = @_;
	return qq{SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '$like'};
}

# the conversion has the partition locked (it is copying)
sub copying_sql
{
	my ($part) = @_;
	return qq{SELECT count(*) FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
		WHERE c.relname = '$part' AND l.mode = 'ShareRowExclusiveLock' AND l.granted};
}

sub methods
{
	my ($t) = @_;
	return q1(qq{SELECT string_agg(c.relname || ':' || a.amname, ',' ORDER BY c.relname)
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_am a ON a.oid = c.relam
		WHERE i.inhparent = '$t'::regclass});
}

sub strays
{
	return q1(q{SELECT count(*) FROM pg_class WHERE relname LIKE '%\_\_kc'});
}

# ---- 1. A column added and filled, and a unique index created, while the
#         conversion waits for its lock.
q1(q{
CREATE TABLE ev (id bigint, ts timestamptz NOT NULL, num int, kind text, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
CREATE TABLE ev_p1 PARTITION OF ev FOR VALUES FROM ('2026-01-01 UTC') TO ('2026-02-01 UTC');
CREATE TABLE ev_p2 PARTITION OF ev FOR VALUES FROM ('2026-02-01 UTC') TO ('2026-03-01 UTC');
INSERT INTO ev SELECT i, '2026-01-01 UTC'::timestamptz + (i % 30) * interval '1 day', i % 50, 'k' || i FROM generate_series(1, 6000) i;
INSERT INTO ev SELECT i, '2026-02-01 UTC'::timestamptz + (i % 20) * interval '1 day', i % 50, 'k' || i FROM generate_series(7001, 7500) i;
});
my $a = $node->background_psql('postgres');
$a->query_safe(q{BEGIN; INSERT INTO ev (id, ts, num, kind) VALUES (777777, '2026-01-10 UTC', 1, 'x');});
my $c = spawn(q{
SET kingres_timepart.create_lock_timeout_ms = 60000;
CALL kingres.convert_partition('ev', 'ev_p1', 'columnar');
});
wait_for(waiting_sql('%convert_partition%'), 'the conversion to wait for the lock');
$a->query_safe(q{
ALTER TABLE ev ADD COLUMN extra int;
UPDATE ev SET extra = id::int WHERE ts < '2026-02-01 UTC';
CREATE UNIQUE INDEX p1_late_unique ON ev_p1 (extra);
COMMIT;
});
finish($c);
is($c->{status}, 0, '1: the conversion succeeded') or diag $c->{err};
is(methods('ev'), 'ev_p1:columnar,ev_p2:heap', '1: the partition is columnar');
is(q1(q{SELECT count(*) || '|' || count(extra) FROM ev_p1}), '6001|6001',
	'1: every row has the value of the column that was added while the conversion waited');
is(q1(q{SELECT count(*) FROM ev_p1 WHERE extra IS DISTINCT FROM id::int}), '0', '1: and it is the right value');
is(q1(q{SELECT coalesce((SELECT i.indisunique::text FROM pg_index i WHERE i.indexrelid = to_regclass('p1_late_unique') AND i.indrelid = 'ev_p1'::regclass), 'missing')}),
	'true', '1: the unique index created meanwhile is on the new partition');
my ($out, $err);
my $ret = $node->psql('postgres',
	q{INSERT INTO ev_p1 (id, ts, num, kind, extra) VALUES (900001, '2026-01-12 UTC', 1, 'd', 5)},
	stdout => \$out, stderr => \$err);
isnt($ret, 0, '1: a duplicate of the new unique key is refused');
like($err, qr/duplicate key value violates unique constraint "p1_late_unique"/, '1: by that index');
is(strays(), '0', '1: no stray table');
$a->quit;

# ---- 2. A column dropped while the conversion waits for its lock.
$a = $node->background_psql('postgres');
$a->query_safe(q{BEGIN; INSERT INTO ev (id, ts, num, kind, extra) VALUES (777778, '2026-01-11 UTC', 2, 'y', 777778);});
$c = spawn(q{
SET kingres_timepart.create_lock_timeout_ms = 60000;
CALL kingres.convert_partition('ev', 'ev_p1', 'heap');
});
wait_for(waiting_sql('%convert_partition%'), 'the second conversion to wait for the lock');
$a->query_safe(q{
ALTER TABLE ev DROP COLUMN kind;
COMMIT;
});
finish($c);
is($c->{status}, 0, '2: the conversion succeeded') or diag $c->{err};
is(methods('ev'), 'ev_p1:heap,ev_p2:heap', '2: the partition is heap');
is(q1(q{SELECT count(*) || '|' || count(extra) FROM ev_p1}), '6002|6002', '2: the rows are all there');
is(q1(q{SELECT count(*) FROM pg_attribute WHERE attrelid = 'ev_p1'::regclass AND attname = 'kind' AND NOT attisdropped}),
	'0', '2: the dropped column is not there');
$a->quit;

# ---- 3. The partition renamed while the conversion waits for its lock.
$a = $node->background_psql('postgres');
$a->query_safe(q{BEGIN; INSERT INTO ev (id, ts, num, extra) VALUES (777779, '2026-01-12 UTC', 3, 777779);});
$c = spawn(q{
SET kingres_timepart.create_lock_timeout_ms = 60000;
CALL kingres.convert_partition('ev', 'ev_p1', 'columnar');
});
wait_for(waiting_sql('%convert_partition%'), 'the third conversion to wait for the lock');
$a->query_safe(q{
ALTER TABLE ev_p1 RENAME TO ev_p1_renamed;
COMMIT;
});
finish($c);
isnt($c->{status}, 0, '3: the conversion did not go on');
like($c->{err}, qr/ERROR:  schema_changed: the partition with OID \d+ was renamed or dropped/, '3: it says that the partition was renamed or dropped');
is(methods('ev'), 'ev_p1_renamed:heap,ev_p2:heap', '3: the partition is as it was, with its new name');
is(strays(), '0', '3: no stray table');
is(q1(q{SELECT count(*) FROM ev_p1_renamed}), '6003', '3: and keeps its rows');
$a->quit;
$c = finish(spawn(q{CALL kingres.convert_partition('ev', 'ev_p1_renamed', 'columnar');}));
is($c->{status}, 0, '3: the next call converts') or diag $c->{err};
is(methods('ev'), 'ev_p1_renamed:columnar,ev_p2:heap', '3: the partition is columnar, with its name');
is(q1(q{SELECT count(*) || '|' || count(extra) FROM ev_p1_renamed}), '6003|6003', '3: with every row');

# ---- 3b. The same through the policy: the action is failed with schema_changed,
#          the run is partial and the next run is due within five minutes.
q1(q{
CREATE TABLE evp (id bigint, ts timestamptz NOT NULL, v int, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
CREATE TABLE evp_p1 PARTITION OF evp FOR VALUES FROM ('2026-01-01 UTC') TO ('2026-01-02 UTC');
CREATE TABLE evp_p2 PARTITION OF evp FOR VALUES FROM ('2026-01-02 UTC') TO ('2026-01-03 UTC');
INSERT INTO evp SELECT i, '2026-01-01 UTC'::timestamptz + (i % 2) * interval '1 day', i FROM generate_series(1, 200) i;
SELECT kingres.timepart_set_policy('evp', '1 day', create_ahead => '1 day', columnar_after => '1 day');
});
my $asof = q{'2026-01-20 UTC'};
$a = $node->background_psql('postgres');
$a->query_safe(q{BEGIN; INSERT INTO evp (id, ts, v) VALUES (900001, '2026-01-01 12:00 UTC', 1);});
$c = spawn(qq{
SET kingres_timepart.create_lock_timeout_ms = 60000;
CALL kingres.timepart_run('evp', $asof, '{convert}');
});
wait_for(waiting_sql('%timepart_run%'), 'the run to wait for the lock');
$a->query_safe(q{
ALTER TABLE evp_p1 RENAME TO evp_p1_renamed;
COMMIT;
});
finish($c);
is($c->{status}, 0, '3b: the run itself came back') or diag $c->{err};
is(q1(q{SELECT state FROM kingres.timepart_run WHERE table_name = 'evp' ORDER BY run_id DESC LIMIT 1}), 'partial', '3b: the run is partial');
like(q1(q{SELECT string_agg(a.state || ':' || left(a.detail, 15), ',' ORDER BY a.seq) FROM kingres.timepart_run_action a
	WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'evp') AND a.kind = 'convert'}),
	qr/^failed:schema_changed/, '3b: the first action is failed, with schema_changed');
is(q1(qq{SELECT (next_run_at = ${asof}::timestamptz + interval '5 minutes')::text FROM kingres.timepart_policy WHERE table_name = 'evp'}),
	'true', '3b: the next run is due in five minutes');
like(q1(qq{SELECT string_agg(problem, ',' ORDER BY problem) FROM kingres.timepart_check('1 hour', $asof)}),
	qr/convert_blocked/, '3b: the check says the conversion is blocked');
$a->quit;
q1(q{SELECT kingres.timepart_unset_policy('evp')});

# ---- 4. The tablespace of the partition goes to the new partition, both ways.
my $tsdir = PostgreSQL::Test::Utils::tempdir;
q1(qq{CREATE TABLESPACE tpts LOCATION '$tsdir'});
q1(q{
CREATE TABLE evt (id bigint, ts timestamptz NOT NULL, v int, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
CREATE TABLE evt_p1 PARTITION OF evt FOR VALUES FROM ('2026-01-01 UTC') TO ('2026-02-01 UTC') TABLESPACE tpts;
CREATE TABLE evt_p2 PARTITION OF evt FOR VALUES FROM ('2026-02-01 UTC') TO ('2026-03-01 UTC');
INSERT INTO evt SELECT i, '2026-01-10 UTC', i FROM generate_series(1, 500) i;
});
sub tablespace_of
{
	my ($t) = @_;
	return q1(qq{SELECT coalesce((SELECT spcname FROM pg_tablespace WHERE oid = c.reltablespace), 'default') FROM pg_class c WHERE c.oid = '$t'::regclass});
}
is(tablespace_of('evt_p1'), 'tpts', '4: the partition is in the tablespace');
# a plain index in the tablespace, and one in the default tablespace (the
# tablespace of an index is its own, not the table's)
q1(q{CREATE INDEX evt_ix_ts ON evt_p1 (v) TABLESPACE tpts; CREATE INDEX evt_ix_def ON evt_p1 (id, v)});
sub index_spaces
{
	return q1(q{SELECT string_agg(c.relname || ':' || coalesce((SELECT spcname FROM pg_tablespace WHERE oid = c.reltablespace), 'default'), ',' ORDER BY c.relname)
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indrelid = 'evt_p1'::regclass AND c.relname LIKE 'evt\_ix%'});
}
is(index_spaces(), 'evt_ix_def:default,evt_ix_ts:tpts', '4: the indexes are in their tablespaces');
q1(q{CALL kingres.convert_partition('evt', 'evt_p1', 'columnar')});
is(methods('evt'), 'evt_p1:columnar,evt_p2:heap', '4: converted to columnar');
is(tablespace_of('evt_p1'), 'tpts', '4: the new partition is in the tablespace');
is(index_spaces(), 'evt_ix_def:default,evt_ix_ts:tpts', '4: and each index is in its own tablespace');
is(tablespace_of('evt_p2'), 'default', '4: the other partition is not');
q1(q{CALL kingres.convert_partition('evt', 'evt_p1', 'heap')});
is(methods('evt'), 'evt_p1:heap,evt_p2:heap', '4: and back to heap');
is(tablespace_of('evt_p1'), 'tpts', '4: still in the tablespace');
is(index_spaces(), 'evt_ix_def:default,evt_ix_ts:tpts', '4: and each index still in its own tablespace');
is(q1(q{SELECT count(*) FROM evt}), '500', '4: with every row');

# ---- 5. The swap gives way after the short wait, and says why.
# A partition whose copy takes a while: a stored generated column that sleeps.
q1(q{
CREATE FUNCTION slow_v(i int) RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$ BEGIN PERFORM pg_sleep(0.03); RETURN i * 2; END $$;
CREATE TABLE evs (id bigint, ts timestamptz NOT NULL, v int, w int GENERATED ALWAYS AS (slow_v(v)) STORED, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
CREATE TABLE evs_p1 PARTITION OF evs FOR VALUES FROM ('2026-01-01 UTC') TO ('2026-02-01 UTC');
INSERT INTO evs (id, ts, v) SELECT i, '2026-01-10 UTC', i FROM generate_series(1, 40) i;
});
my $rd = $node->background_psql('postgres');
$rd->query_safe(q{BEGIN; SELECT count(*) FROM evs;});
my $t0 = time;
$ret = $node->psql('postgres',
	q{SET kingres_timepart.create_lock_timeout_ms = 10000; CALL kingres.convert_partition('evs', 'evs_p1', 'columnar')},
	stdout => \$out, stderr => \$err, timeout => 40);
my $took = time - $t0;
isnt($ret, 0, '5: the conversion gave way to the reader');
like($err, qr/ERROR:  swap_lock_timeout: [\w."]+ could not be locked within \d+ ms/, '5: it says that the table could not be locked in time');
cmp_ok($took, '<', 6, sprintf('5: and it did so after the short wait, not the lock timeout (%.1f s)', $took));
is(methods('evs'), 'evs_p1:heap', '5: the partition is as it was');
$rd->query_safe(q{COMMIT});
$rd->quit;

# A writer through the table that waits for the partition: the swap does not ask.
my $conv = spawn(q{SET kingres_timepart.create_lock_timeout_ms = 10000; CALL kingres.convert_partition('evs', 'evs_p1', 'columnar');
});
wait_for(copying_sql('evs_p1'), 'the copy to start');
my $w = spawn(q{INSERT INTO evs (id, ts, v) VALUES (900, '2026-01-11 UTC', 900);
});
wait_for(waiting_sql('%INSERT INTO evs (id, ts, v) VALUES (900%'), 'the writer to wait');
finish($conv);
isnt($conv->{status}, 0, '5: the conversion gave way to the writer');
like($conv->{err}, qr/ERROR:  swap_lock_timeout: a session that holds [\w."]+ waits for the partition [\w."]+;/,
	'5: it saw the writer in the lock table, before it asked for the lock');
unlike($conv->{err}, qr/ERROR:  swap_lock_timeout: [\w."]+ could not be locked within/, '5: it did not wait for the lock');
finish($w);
is($w->{status}, 0, '5: the writer went on') or diag $w->{err};
is(q1(q{SELECT count(*) FROM evs WHERE id = 900}), '1', '5: its row is in the old partition');
is(methods('evs'), 'evs_p1:heap', '5: the partition is heap');
is(strays(), '0', '5: no stray table');

# ---- 7. A change that needs no lock on the table, committed while the swap
#         waits for its lock, is not lost: the conversion gives way and the
#         partition keeps the change.  GRANT, REVOKE, and the rename, comment and
#         storage parameters of an index take no lock that the conversion's lock
#         on the partition keeps out.  The reader holds the table, so the swap
#         waits (up to half a second); the change is made in a session that is
#         already open, the moment the swap is seen waiting.
q1(q{
CREATE ROLE tpg_b NOLOGIN;
CREATE TABLE evg (id bigint, ts timestamptz NOT NULL, v int, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
CREATE TABLE evg_p1 PARTITION OF evg FOR VALUES FROM ('2026-01-01 UTC') TO ('2026-02-01 UTC');
INSERT INTO evg SELECT i, '2026-01-10 UTC', i FROM generate_series(1, 300) i;
CREATE INDEX evg_ix ON evg_p1 (v);
CREATE INDEX evg_ix_b ON evg_p1 (id, v);
GRANT SELECT ON evg_p1 TO tpg_b;
});

sub swap_waiting_sql
{
	my ($t) = @_;
	return qq{SELECT count(*) FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
		WHERE c.relname = '$t' AND l.mode = 'AccessExclusiveLock' AND NOT l.granted};
}

# Run one conversion of evg_p1 to the method $am while $change commits during
# the swap's wait; returns the handle.  A try in which the swap gave way before
# the change was made (a loaded machine) is made again, with a change that
# is put back first by $undo.
sub raced_conversion
{
	my ($am, $change, $undo, $deadlock) = @_;
	my $h;
	for my $try (1 .. 6)
	{
		my $rdr = $node->background_psql('postgres');
		my $chg = $node->background_psql('postgres');
		$rdr->query_safe(q{BEGIN; SELECT count(*) FROM evg;});
		$h = spawn(qq{SET kingres_timepart.create_lock_timeout_ms = 10000;\n}
			. (defined $deadlock ? "SET deadlock_timeout = '$deadlock';\n" : '')
			. qq{CALL kingres.convert_partition('evg', 'evg_p1', '$am');\n});
		my $seen = wait_for(swap_waiting_sql('evg'), 'the swap to wait for the table');
		$chg->query_safe($change) if $seen;
		$rdr->query_safe(q{COMMIT});
		finish($h);
		$rdr->quit;
		$chg->quit;
		last unless $h->{err} =~ /ERROR:  swap_lock_timeout/ && defined $undo;
		diag("try $try: the swap gave way before the change was made; again");
		q1($undo);
	}
	return $h;
}

sub has_priv
{
	return q1(q{SELECT has_table_privilege('tpg_b', 'evg_p1', 'select')});
}

# 7a. REVOKE
my $h = raced_conversion('columnar', q{REVOKE SELECT ON evg_p1 FROM tpg_b}, q{GRANT SELECT ON evg_p1 TO tpg_b});
isnt($h->{status}, 0, '7a: the conversion gave way to the REVOKE');
like($h->{err}, qr/ERROR:  schema_changed: the privileges, or the names, comments or options of the indexes/, '7a: with schema_changed');
is(methods('evg'), 'evg_p1:heap', '7a: the partition is as it was');
is(has_priv(), 'f', '7a: the REVOKE stands (the new table did not bring the privilege back)');
is(strays(), '0', '7a: no stray table');
$h = finish(spawn(q{CALL kingres.convert_partition('evg', 'evg_p1', 'columnar');}));
is($h->{status}, 0, '7a: the next call converts') or diag $h->{err};
is(has_priv(), 'f', '7a: and the new partition does not have the privilege');

# 7b. GRANT
$h = raced_conversion('heap', q{GRANT SELECT ON evg_p1 TO tpg_b}, q{REVOKE SELECT ON evg_p1 FROM tpg_b});
isnt($h->{status}, 0, '7b: the conversion gave way to the GRANT');
like($h->{err}, qr/ERROR:  schema_changed: the privileges/, '7b: with schema_changed');
is(methods('evg'), 'evg_p1:columnar', '7b: the partition is as it was');
is(has_priv(), 't', '7b: the GRANT stands');
$h = finish(spawn(q{CALL kingres.convert_partition('evg', 'evg_p1', 'heap');}));
is($h->{status}, 0, '7b: the next call converts') or diag $h->{err};
is(has_priv(), 't', '7b: and the new partition has the privilege');

# 7c. the rename, the comment and the options of an index
my @idx = (
	[ 'rename',  q{ALTER INDEX evg_ix RENAME TO evg_ix_renamed}, q{ALTER INDEX IF EXISTS evg_ix_renamed RENAME TO evg_ix} ],
	[ 'comment', q{COMMENT ON INDEX evg_ix_b IS 'a late comment'}, q{COMMENT ON INDEX evg_ix_b IS NULL} ],
	[ 'options', q{ALTER INDEX evg_ix_b SET (fillfactor = 50)}, q{ALTER INDEX evg_ix_b RESET (fillfactor)} ],
);
my $am = 'columnar';
for my $c7 (@idx)
{
	my ($what, $sql, $undo) = @$c7;
	$h = raced_conversion($am, $sql, $undo);
	isnt($h->{status}, 0, "7c: the conversion gave way to the index $what");
	like($h->{err}, qr/ERROR:  schema_changed: the privileges, or the names, comments or options of the indexes/, "7c: $what: with schema_changed");
	is(methods('evg'), 'evg_p1:' . ($am eq 'columnar' ? 'heap' : 'columnar'), "7c: $what: the partition is as it was");
	is(strays(), '0', "7c: $what: no stray table");
	# the change stands on the partition, and a call that is not raced carries it
	$h = finish(spawn(qq{CALL kingres.convert_partition('evg', 'evg_p1', '$am');}));
	is($h->{status}, 0, "7c: $what: the next call converts") or diag $h->{err};
	$am = $am eq 'columnar' ? 'heap' : 'columnar';
}
is(q1(q{SELECT string_agg(c.relname || ':' || coalesce(obj_description(c.oid, 'pg_class'), '-') || ':' || coalesce(c.reloptions::text, '-'), ',' ORDER BY c.relname)
	FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indrelid = 'evg_p1'::regclass AND c.relname LIKE 'evg\_ix%'}),
	'evg_ix_b:a late comment:{fillfactor=50},evg_ix_renamed:-:-',
	'7c: the late rename, comment and option are on the converted partition');

# ---- 7d. The cap of the swap wait is 500 ms, though deadlock_timeout is much
#          larger (the wait is the smaller of 500 ms and half of it).
{
	my $rdr = $node->background_psql('postgres');
	$rdr->query_safe(q{BEGIN; SELECT count(*) FROM evg;});
	$h = spawn(qq{SET kingres_timepart.create_lock_timeout_ms = 10000;\nSET deadlock_timeout = '10s';\n}
		. qq{CALL kingres.convert_partition('evg', 'evg_p1', '$am');\n});
	wait_for(swap_waiting_sql('evg'), 'the swap to wait for the table');
	usleep(2_000_000);    # longer than the cap, much shorter than half of ten seconds
	$rdr->query_safe(q{COMMIT});
	finish($h);
	$rdr->quit;
}
isnt($h->{status}, 0, '7d: the conversion gave way to the reader');
like($h->{err}, qr/ERROR:  swap_lock_timeout: [\w."]+ could not be locked within 500 ms/,
	'7d: it waited 500 ms, the cap, not half of a deadlock_timeout of ten seconds');

# ---- 8. The name of the partition is another table when the conversion gets
#         its lock: the lock that was granted is not the partition's.
q1(q{
CREATE TABLE evn (id bigint, ts timestamptz NOT NULL, v int, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
CREATE TABLE evn_p1 PARTITION OF evn FOR VALUES FROM ('2026-01-01 UTC') TO ('2026-02-01 UTC');
INSERT INTO evn SELECT i, '2026-01-10 UTC', i FROM generate_series(1, 100) i;
});
$a = $node->background_psql('postgres');
$a->query_safe(q{BEGIN; INSERT INTO evn (id, ts, v) VALUES (5000, '2026-01-11 UTC', 1);});
$c = spawn(q{
SET kingres_timepart.create_lock_timeout_ms = 60000;
CALL kingres.convert_partition('evn', 'evn_p1', 'columnar');
});
wait_for(waiting_sql('%convert_partition%'), 'the conversion to wait for the lock of the partition');
$a->query_safe(q{
ALTER TABLE evn_p1 RENAME TO evn_p1_old;
CREATE TABLE evn_p1 (id bigint, ts timestamptz, v int);
COMMIT;
});
finish($c);
isnt($c->{status}, 0, '8: the conversion did not go on');
like($c->{err}, qr/ERROR:  schema_changed: the partition with OID \d+ was renamed or dropped while the conversion waited for its lock, and its name is another table now/,
	'8: it says that the name is another table now');
is(q1(q{SELECT string_agg(c.relname || ':' || a.amname, ',' ORDER BY c.relname) FROM pg_class c JOIN pg_am a ON a.oid = c.relam
	WHERE c.relname IN ('evn_p1', 'evn_p1_old')}), 'evn_p1:heap,evn_p1_old:heap', '8: both tables are as they were');
is(strays(), '0', '8: no stray table');
$a->quit;

# ---- 6. The limit: a REPEATABLE READ snapshot older than the swap, on a table
#         that the transaction has not read yet, finds the converted partition empty.
q1(q{
CREATE TABLE evr (id bigint, ts timestamptz NOT NULL, v int, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
CREATE TABLE evr_p1 PARTITION OF evr FOR VALUES FROM ('2026-01-01 UTC') TO ('2026-02-01 UTC');
INSERT INTO evr SELECT i, '2026-01-10 UTC', i FROM generate_series(1, 100) i;
});
$rd = $node->background_psql('postgres');
$rd->query_safe(q{BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1;});
q1(q{CALL kingres.convert_partition('evr', 'evr_p1', 'columnar')});
is($rd->query_safe(q{SELECT count(*) FROM evr}), '0',
	'6: the old snapshot sees the converted partition empty (the README says so)');
$rd->query_safe(q{COMMIT});
is($rd->query_safe(q{SELECT count(*) FROM evr}), '100', '6: a new transaction sees every row');
$rd->quit;

is(q1(q{SELECT kingres.columnar_check_clean()}), 't', 'the columnar files and the stripe catalog are clean');
$node->stop;
done_testing();
