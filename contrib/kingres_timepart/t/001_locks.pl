# contrib/kingres_timepart/t/001_locks.pl
#
# Locks and waits.  The waits of a run are bounded by settings, so no step
# of this test depends on timing: the other session holds its lock until the
# test releases it, the run's waits are a few milliseconds, and the run is
# expected to come back with the refusals recorded.
#
#   * a long transaction that holds the parent in ACCESS EXCLUSIVE does not
#     make a run hang: reading the partitions waits for that lock, the wait is
#     bounded, the run fails as a whole with the lock error, and is retried
#     within five minutes;
#   * a long transaction that holds the parent in SHARE UPDATE EXCLUSIVE does
#     not either: the create actions fail with lock_timeout after the number
#     of attempts the setting says, the run is "partial", nothing is half
#     done, and the next run, once the lock is gone, finishes the work;
#   * a transaction that has read or written the parent (ACCESS SHARE and
#     ROW EXCLUSIVE) does not block creation of partitions, even with the
#     shortest lock timeout, and creation does not block it;
#   * a reader of the parent blocks the detach of an old partition, bounded
#     by the same settings; the failed drop leaves the partition attached;
#     the next run drops it;
#   * drop_old_time_partitions and create_time_partitions called by hand give
#     up with lock_not_available and put lock_timeout back;
#   * a second session cannot begin a run of a table that another session is
#     running (55P03); when that session is gone, the next run marks the
#     abandoned one as interrupted;
#   * a role that holds the advisory locks that the earlier versions used does
#     not stop the runs of the table, and cannot touch the lock table;
#   * the attach of a partition of a table that has a foreign key waits for
#     writers of the referenced table, bounded by the same settings.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time);

my $node = PostgreSQL::Test::Cluster->new('locks');
$node->init;
$node->append_conf('postgresql.conf', q{
kingres_timepart.create_lock_timeout_ms = 50
kingres_timepart.retire_lock_timeout_ms = 50
kingres_timepart.lock_retries = 3
kingres_timepart.retry_sleep_min_ms = 5
kingres_timepart.retry_sleep_max_ms = 10
});
$node->start;

# The tests name their tables without a schema: make sure they land in public
# whatever the name of the role that runs them (a schema named like the role
# comes first in the default search_path, and the extension creates one).
$node->safe_psql('postgres', q{CREATE EXTENSION kingres_timepart; ALTER DATABASE postgres SET search_path = public;});
$node->safe_psql('postgres', q{
SET timezone = 'UTC';
CREATE TABLE ev (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('ev', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('ev', '1 day', create_ahead => '3 days', retention => '3 days');
});

# Run a statement script with a time limit: a run that hangs fails the test
# after the limit instead of blocking the suite.
sub run_sql
{
	my ($sql) = @_;
	my ($out, $err);
	my $ret = $node->psql('postgres', "SET timezone = 'UTC';\n$sql",
		stdout => \$out, stderr => \$err, timeout => 120);
	return ($ret, $out, $err);
}

sub q1
{
	my ($sql) = @_;
	return $node->safe_psql('postgres', "SET timezone = 'UTC'; $sql");
}

sub parts
{
	return q1(q{SELECT string_agg(c.relname, ',' ORDER BY c.relname)
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'ev'::regclass});
}

# ---- 1a. ACCESS EXCLUSIVE held by a long transaction: the run does not hang.
# Reading the bounds of the partitions already waits for that lock, so the run
# fails as a whole, after the bounded wait, and is retried soon.
my $holder = $node->background_psql('postgres', on_error_stop => 0);
$holder->query_safe(q{BEGIN; LOCK TABLE ev IN ACCESS EXCLUSIVE MODE;});

my $t0 = time();
my ($ret, $out, $err) = run_sql(q{CALL kingres.timepart_run('ev', '2026-03-03 12:00 UTC', '{create}');});
my $elapsed = time() - $t0;
is($ret, 0, 'a run under an ACCESS EXCLUSIVE lock of another session comes back');
cmp_ok($elapsed, '<', 100, 'and does not hang');
is(q1(q{SELECT state || ' ' || left(error, 5) FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}),
	'failed 55P03', 'the run failed with the lock error');
is(q1(q{SELECT count(*) FROM kingres.timepart_run_action}), '0', 'nothing was done');
is(parts(), 'ev_p20260301,ev_p20260302,ev_p20260303', 'no partition was created');
is(q1(q{SELECT count(*) FROM pg_class WHERE relname LIKE 'ev\_p%' AND NOT relispartition AND relkind = 'r'}),
	'0', 'no stray table is left');
is(q1(q{SELECT p.next_run_at = r.as_of + interval '5 minutes' FROM kingres.timepart_policy p, kingres.timepart_run r
		 WHERE r.run_id = (SELECT max(run_id) FROM kingres.timepart_run)}),
	't', 'the next run is due within 5 minutes');
is(q1(q{SELECT problem FROM kingres.timepart_check('2 days', '2026-03-03 12:00 UTC')
		 WHERE table_name = 'ev'}),
	'lock_timeout', 'the check gives up on the table after the bounded wait and says so');

# The by-hand functions give up too, with the error of the lock, and leave
# lock_timeout as it was.
($ret, $out, $err) = run_sql(q{SELECT kingres.create_time_partitions('ev', '1 day', '2026-03-05 UTC', '2026-03-01 UTC');});
isnt($ret, 0, 'create_time_partitions by hand fails while the parent is locked');
like($err, qr/lock timeout|lock_timeout|could not obtain lock/, 'with the lock timeout error');
($ret, $out, $err) = run_sql(q{SELECT kingres.drop_old_time_partitions('ev', '2026-03-03 UTC');});
isnt($ret, 0, 'drop_old_time_partitions by hand fails while the parent is locked');
is(q1(q{SHOW lock_timeout}), '0', 'lock_timeout is as it was');
$holder->query_safe(q{COMMIT;});
is(q1(q{SELECT problem FROM kingres.timepart_check('2 days', '2026-03-03 12:00 UTC')
		 WHERE table_name = 'ev' ORDER BY problem}),
	"ahead_low\nlast_run_failed", 'once the lock is gone the check reports the table and the failed run');

# ---- 1b. SHARE UPDATE EXCLUSIVE held (what a concurrent ATTACH, VACUUM or
# ANALYZE of the parent takes): the bounds can be read, the attach has to
# wait.  Each create action is tried as often as the setting says and fails
# with the lock error; the run is partial.
$holder->query_safe(q{BEGIN; LOCK TABLE ev IN SHARE UPDATE EXCLUSIVE MODE;});
$t0 = time();
($ret, $out, $err) = run_sql(q{CALL kingres.timepart_run('ev', '2026-03-03 12:00 UTC', '{create}');});
$elapsed = time() - $t0;
is($ret, 0, 'a run under a SHARE UPDATE EXCLUSIVE lock of another session comes back');
cmp_ok($elapsed, '<', 100, 'and does not hang');
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}),
	'partial', 'the run is partial');
is(q1(q{SELECT count(*) FROM kingres.timepart_run_action a
		 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run)
		   AND a.kind = 'create' AND a.state = 'failed'
		   AND a.detail LIKE 'lock_timeout:%(attempt 3 of 3)'}),
	'3', 'every create action failed after 3 attempts, as the setting says');
is(q1(q{SELECT count(*) FROM kingres.timepart_run_action a
		 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run)
		   AND a.state = 'done'}),
	'0', 'nothing was done and nothing is half done');
is(parts(), 'ev_p20260301,ev_p20260302,ev_p20260303', 'no partition was created');
is(q1(q{SELECT count(*) FROM pg_class WHERE relname LIKE 'ev\_p%' AND NOT relispartition AND relkind = 'r'}),
	'0', 'no stray table is left: the new table was rolled back with the failed attach');
is(q1(q{SELECT p.next_run_at = r.as_of + interval '5 minutes' FROM kingres.timepart_policy p, kingres.timepart_run r
		 WHERE r.run_id = (SELECT max(run_id) FROM kingres.timepart_run)}),
	't', 'the next run is due within 5 minutes after a lock timeout');
$holder->query_safe(q{COMMIT;});
$holder->quit;

($ret, $out, $err) = run_sql(q{CALL kingres.timepart_run('ev', '2026-03-03 12:00 UTC', '{create}');});
is($ret, 0, 'the next run, with the lock gone, runs');
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}),
	'done', 'and is done');
is(parts(), join(',', map { "ev_p202603$_" } qw(01 02 03 04 05 06)),
	'the missing partitions exist now');

# ---- 2. Readers and writers of the parent do not block creation.
$node->safe_psql('postgres', q{SET timezone = 'UTC';
	SELECT kingres.timepart_set_policy('ev', '1 day', create_ahead => '5 days', retention => '30 days');});
my $reader = $node->background_psql('postgres', on_error_stop => 0);
my $writer = $node->background_psql('postgres', on_error_stop => 0);
$reader->query_safe(q{BEGIN; SELECT count(*) FROM ev;});
$writer->query_safe(q{BEGIN; SET timezone = 'UTC'; INSERT INTO ev VALUES (1, '2026-03-02 10:00 UTC');});

# The shortest lock timeout: a lock that has to wait at all would fail.
($ret, $out, $err) = run_sql(q{SET kingres_timepart.create_lock_timeout_ms = 1;
	CALL kingres.timepart_run('ev', '2026-03-05 UTC', '{create}');});
is($ret, 0, 'a run while a reader and a writer hold the parent comes back');
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}),
	'done', 'and succeeds with a 1 ms lock timeout');
is(q1(q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'ev'::regclass}),
	'9', 'partitions up to five days ahead were created');
# The open transactions go on: the writer can still write to the parent and
# to a new partition, the reader can still read.
$writer->query_safe(q{INSERT INTO ev VALUES (2, '2026-03-08 10:00 UTC'); COMMIT;});
$reader->query_safe(q{SELECT count(*) FROM ev; COMMIT;});
is(q1(q{SELECT count(*) FROM ev}), '2', 'the writer\'s rows are there, in old and new partitions');
is(q1(q{SELECT tableoid::regclass::text FROM ev WHERE id = 2}), 'ev_p20260308', 'the row went to a new partition');
$reader->quit;
$writer->quit;

# ---- 3. A reader blocks the detach: bounded, recorded, and retried.
$node->safe_psql('postgres', q{SET timezone = 'UTC';
	SELECT kingres.timepart_set_policy('ev', '1 day', create_ahead => '5 days', retention => '3 days');});
$reader = $node->background_psql('postgres', on_error_stop => 0);
$reader->query_safe(q{BEGIN; SELECT count(*) FROM ev;});
$t0 = time();
($ret, $out, $err) = run_sql(q{CALL kingres.timepart_run('ev', '2026-03-06 12:00 UTC', '{retire}');});
$elapsed = time() - $t0;
is($ret, 0, 'a retirement under a reader comes back');
cmp_ok($elapsed, '<', 100, 'and does not hang');
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}), 'partial', 'the run is partial');
is(q1(q{SELECT string_agg(a.partition_name || ':' || a.state, ',' ORDER BY a.seq) FROM kingres.timepart_run_action a
		 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run)}),
	'ev_p20260301:failed,ev_p20260302:failed',
	'every drop failed, none was tried again in the run');
is(q1(q{SELECT count(*) FROM kingres.timepart_run_action a
		 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run) AND a.detail LIKE 'lock_timeout:%(attempt 3 of 3)'}),
	'2', 'each after three attempts');
is(q1(q{SELECT count(*) FROM pg_inherits WHERE inhrelid = 'ev_p20260301'::regclass}),
	'1', 'the partitions are still attached');
$reader->query_safe(q{COMMIT;});
$reader->quit;
($ret, $out, $err) = run_sql(q{CALL kingres.timepart_run('ev', '2026-03-06 12:00 UTC', '{retire}');});
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}), 'done', 'the next run drops them');
is(q1(q{SELECT count(*) FROM ev WHERE ts < '2026-03-03 UTC'}), '0', 'with their rows');

# ---- 4. One run per table at a time.
my $other = $node->background_psql('postgres', on_error_stop => 0);
my $run_a = $other->query_safe(q{SELECT kingres.timepart_begin_run('ev', '2026-03-06 12:00 UTC', 'manual');});
chomp $run_a;
($ret, $out, $err) = run_sql(q{SELECT kingres.timepart_begin_run('ev', '2026-03-06 12:00 UTC', 'manual');});
isnt($ret, 0, 'a second session cannot begin a run of the table');
like($err, qr/another run of table public\.ev is in progress/, 'it says why');
is(q1(qq{SELECT state FROM kingres.timepart_run WHERE run_id = $run_a}), 'running', 'the first run is still running');
$other->quit;
# The session is gone, its lock with it: the next begin takes over.
my $run_b = q1(q{SELECT kingres.timepart_begin_run('ev', '2026-03-06 12:00 UTC', 'manual')});
is(q1(qq{SELECT state || ':' || error FROM kingres.timepart_run WHERE run_id = $run_a}),
	'failed:interrupted', 'the abandoned run is marked interrupted');
is(q1(qq{SELECT state FROM kingres.timepart_run WHERE run_id = $run_b}), 'running', 'and the new one runs');
q1(qq{SELECT kingres.timepart_finish_run($run_b)});

# ---- 5. No role can stop the runs of a table by taking a lock: the claim of a
# run is a row lock that only the extension owner can take, and the run row
# with its backend.  A role that holds the advisory locks of the former
# versions (the keys are computable from the name of the table) changes
# nothing.
$node->safe_psql('postgres', q{CREATE ROLE evil LOGIN;});
$node->safe_psql('postgres', q{GRANT USAGE ON SCHEMA kingres TO evil; GRANT USAGE ON SCHEMA public TO evil;});
my $evil = $node->background_psql('postgres', on_error_stop => 0);
$evil->query_safe(q{SET ROLE evil;});
$evil->query_safe(q{SELECT pg_advisory_lock(hashtext('kingres_timepart'),
		hashtext(quote_ident('public') || '.' || quote_ident('ev')));});
$evil->query_safe(q{SELECT pg_advisory_lock(hashtext('kingres_timepart_step'),
		hashtext(quote_ident('public') || '.' || quote_ident('ev')));});
$evil->query_safe(q{BEGIN; SELECT pg_advisory_xact_lock(hashtext('kingres_timepart_step'),
		hashtext(quote_ident('public') || '.' || quote_ident('ev')));});
($ret, $out, $err) = run_sql(q{CALL kingres.timepart_run('ev', '2026-03-12 12:00 UTC', '{create}');});
is($ret, 0, 'a run goes on while a role holds the advisory locks that name the table');
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}), 'done', 'and is done');
cmp_ok(q1(q{SELECT count(*) FROM kingres.timepart_run_action a
		 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run) AND a.state = 'done'}),
	'>', 0, 'it created the partitions that were missing');
($ret, $out, $err) = $node->psql('postgres', q{SET ROLE evil; SELECT * FROM kingres.timepart_lock;},
	stderr => \$err);
like($err, qr/permission denied for table timepart_lock/, 'the role cannot touch the lock table');
($ret, $out, $err) = $node->psql('postgres', q{SET ROLE evil; SELECT * FROM kingres.timepart_policy FOR UPDATE;},
	stdout => \$out, stderr => \$err);
is($out, '', 'it sees no policy of a table it does not own, so it cannot lock the row either');
$evil->query_safe(q{COMMIT;});
$evil->quit;
$node->safe_psql('postgres', q{DROP OWNED BY evil; DROP ROLE evil;});

# ---- 6. A foreign key of the parent: the attach locks the referenced table
# with SHARE ROW EXCLUSIVE, which conflicts with every writer of that table.
# (This is PostgreSQL's rule; the README lists it.)  A writer of the referenced
# table that has not committed makes the create actions wait, bounded, and
# fail with the lock error; without the foreign key the same writer does not
# matter (section 2).
$node->safe_psql('postgres', q{SET timezone = 'UTC';
	CREATE TABLE fk_ref (id int PRIMARY KEY);
	CREATE TABLE fkp (id int REFERENCES fk_ref, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
	SELECT kingres.create_time_partitions('fkp', '1 day', '2026-03-10 UTC', '2026-03-10 UTC');
	SELECT kingres.timepart_set_policy('fkp', '1 day', create_ahead => '2 days');});
$writer = $node->background_psql('postgres', on_error_stop => 0);
$writer->query_safe(q{BEGIN; INSERT INTO fk_ref VALUES (1);});
($ret, $out, $err) = run_sql(q{CALL kingres.timepart_run('fkp', '2026-03-10 12:00 UTC', '{create}');});
is($ret, 0, 'a run for a table with a foreign key comes back while the referenced table has a writer');
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}), 'partial', 'the run is partial');
is(q1(q{SELECT count(*) FROM kingres.timepart_run_action a
		 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run) AND a.state = 'failed'
		   AND a.detail LIKE 'lock_timeout:%'}),
	'2', 'the two create actions failed on the lock timeout');
is(q1(q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'fkp'::regclass}), '1', 'and nothing was created');
$writer->query_safe(q{COMMIT;});
$writer->quit;
($ret, $out, $err) = run_sql(q{CALL kingres.timepart_run('fkp', '2026-03-10 12:00 UTC', '{create}');});
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}), 'done', 'the next run, without the writer, is done');
is(q1(q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'fkp'::regclass}), '3', 'with the partitions');

$node->stop;
done_testing();
