# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/282_move_start_lock.pl: the start of a bucket move (the same shape as the split fix of t/204 G).
#
# move_bucket() has the record of a move written and committed by a session of its own, and only then takes the live mark.  Between the two the move had
# a record and nobody held the mark; the only thing that kept the recovery worker from undoing it under its session was a grace time of two node
# timeouts counted from the last touch of the record.  A session that stalls for longer lost its move.  Now the session holds a lock of (colocation
# group, bucket) from before the record is written until it holds the mark.
#
#   A. a move stopped between its record and its mark (fault point move_after_begin), its record old enough to be past the grace: it is alive, a recovery pass
#      leaves it alone, check_cluster() does not call it stalled, the move goes on and ends cleanly
#   B. a cancel and an error at each point where a lock is held leave the session without a lock, the move undone and the next move working
#   C. a second move of the same bucket waits for the lock of the first, then finds the bucket being moved
#   D. the waits are bounded: another session holds the lock of the start, or the mark of a move, longer than node_timeout
#   E. the probe of the lock of the start does not leave a lock behind
#   F. a second cancel while the undo runs, a lock taken twice, a starter that dies between record and mark, a real error of the begin

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use POSIX ();
use Test::More;
use Time::HiRes qw(usleep);
use KingresShardTest qw(make_cluster run_sql wait_for_fault_file);

my $tmp = PostgreSQL::Test::Utils::tempdir();
# no recovery worker: the test calls resume_bucket_moves() itself, at the moment it looks at
my @nodes = make_cluster('ms', 3, logical => 1, recovery_interval => 0,
	extra => "max_connections = 100\nkingres_shard.node_timeout = 5\nkingres_shard.move_cleanup_delay = 1\n");
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

q1($n1, q{
	CREATE TABLE public.m1 (id int PRIMARY KEY, v text);
	INSERT INTO public.m1 SELECT g, 'v' || g FROM generate_series(1, 600) g;
	SELECT kingres.create_distributed_table('public.m1', 'id', bucket_count => 6);
});
my $C = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.m1'::regclass});
sub owner { my ($b) = @_; return q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"); }
sub other { my ($b) = @_; return (owner($b) + 1) % 3; }
sub clean { return q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || coalesce(object, ''), '; '), '') FROM kingres.check_cluster()}); }
sub digest { my ($n) = @_; return q1($n, q{SELECT count(*) || ',' || sum(id) || ',' || sum(length(v)) FROM public.m1}); }
my $base = digest($n1);
sub advisory_locks { return q1($n1, q{SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid()}); }
sub last_move { return q1($n1, q{SELECT max(move_id) FROM kingres.bucket_move}); }

# the sessions that wait at a fault point
sub stopped
{
	my ($point, $sql, $name) = @_;
	my $file = "$tmp/wait_$name";
	open(my $fh, '>', $file) or die;
	close($fh);
	my $pid = fork();
	die 'fork' unless defined $pid;
	if ($pid == 0)
	{
		my $s = "SET kingres_shard.fault_injection = '$point:wait:$file'; $sql";
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$s\" > $tmp/out_$name 2>&1");
		POSIX::_exit(0);
	}
	return ($pid, $file);
}
sub release { my ($pid, $file) = @_; unlink($file); waitpid($pid, 0); }
sub out { my ($name) = @_; open(my $fh, '<', "$tmp/out_$name") or return ''; local $/; my $s = <$fh>; close($fh); return $s; }
sub move_pid
{
	return q1($n1, q{SELECT pid FROM pg_stat_activity WHERE query LIKE '%move_bucket(%' AND pid <> pg_backend_pid() AND state = 'active' AND query NOT LIKE '%pg_stat_activity%' ORDER BY backend_start LIMIT 1});
}
# a session that holds a statement's locks for a while: returns its pid
sub holder
{
	my ($sql, $name) = @_;
	my $pid = fork();
	die 'fork' unless defined $pid;
	if ($pid == 0)
	{
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$sql; SELECT pg_sleep(120)\" > $tmp/out_$name 2>&1");
		POSIX::_exit(0);
	}
	return $pid;
}
sub end_holder
{
	my ($pid) = @_;
	q1($n1, q{SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE '%pg_sleep(120)%' AND pid <> pg_backend_pid()});
	waitpid($pid, 0);
}

is(clean(), '', 'the cluster is clean at the start');

# ---------------------------------------------------------------------------
# A. between the record of a move and its live mark the move is alive, however long the session stalls
# ---------------------------------------------------------------------------
{
	my $b = 1;
	my $tgt = other($b);
	my ($pid, $file) = stopped('move_after_begin', "SELECT kingres.move_bucket($C, $b, $tgt)", 'a1');
	ok(wait_for_fault_file("$file.reached", 60), 'A: a move of bucket 1 waits between its record and its live mark');
	my $id = last_move();
	is(q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $id"), 'preparing', 'A: the record is there');
	# the session has stalled for longer than the grace of the worker (two node timeouts) and than the one that check_cluster() allows
	q1($n1, "UPDATE kingres.bucket_move SET updated_at = clock_timestamp() - interval '1 hour' WHERE move_id = $id");
	is(q1($n1, "SELECT kingres.internal_move_alive($id)"), 't', 'A: the move is alive');
	is(q1($n1, q{SELECT kingres.resume_bucket_moves()}), '0', 'A: a recovery pass leaves it alone');
	is(q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $id"), 'preparing', 'A: and the record is as it was');
	unlike(q1($n1, q{SELECT coalesce(string_agg(check_name, ','), '') FROM kingres.check_cluster()}), qr/stalled_move/, 'A: check_cluster() does not call it stalled');
	is(q1($n1, "SELECT kingres.internal_move_group_held($C, $b)"), 't', 'A: the lock of the start is held');
	is(q1($n1, "SELECT kingres.internal_move_group_held($C, " . ($b + 1) . ")"), 'f', 'A: and only for this bucket');
	release($pid, $file);
	unlike(out('a1'), qr/ERROR|WARNING/, 'A: the move goes on and ends without a warning') or diag(out('a1'));
	is(q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $id"), 'cleaning', 'A: it switched');
	is(owner($b), $tgt, 'A: the bucket is on its target');
	is(q1($n1, "SELECT kingres.internal_move_alive($id)"), 'f', 'A: nobody holds its marks any more');
	is(advisory_locks(), '0', 'A: and no lock is left');
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(clean(), '', 'A: check_cluster() finds nothing');
	is(digest($_), $base, 'A: every node reads every row') for @nodes;
}

# ---------------------------------------------------------------------------
# B. a cancel or an error at each point where a lock is held leaves none behind
# ---------------------------------------------------------------------------
{
	my $b = 2;
	for my $point ('move_after_begin', 'move_after_mark', 'move_after_preparing')
	{
		my $name = "b_$point";
		my $tgt = other($b);
		my ($pid, $file) = stopped($point, "SELECT kingres.move_bucket($C, $b, $tgt)", $name);
		ok(wait_for_fault_file("$file.reached", 60), "B: a move of bucket 2 waits at $point");
		is(advisory_locks(), $point eq 'move_after_begin' ? '1' : $point eq 'move_after_mark' ? '2' : '1', "B: $point: the locks it holds");
		q1($n1, 'SELECT pg_cancel_backend(' . move_pid() . ')');
		release($pid, $file);
		like(out($name), qr/canceling statement/, "B: $point: the cancel ends the move");
		is(advisory_locks(), '0', "B: $point: no lock of the start and no mark is left");
		my $id = last_move();
		ok($n1->poll_query_until('postgres', "SELECT phase IN ('failed', 'cancelled') FROM kingres.bucket_move WHERE move_id = $id"), "B: $point: the record is undone");
		is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), 'active', "B: $point: the bucket is active");
	}
	# the same inside one session, which is what a pooled connection is: an error caught by the caller leaves the session holding no lock
	for my $point ('move_after_begin', 'move_after_mark', 'move_after_preparing')
	{
		my $tgt = other($b);
		is(q1($n1, qq{SET kingres_shard.fault_injection = '$point:error';
			DO \$\$ BEGIN PERFORM kingres.move_bucket($C, $b, $tgt); EXCEPTION WHEN OTHERS THEN NULL; END \$\$;
			RESET kingres_shard.fault_injection;
			SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid()}), '0', "B: an error at $point leaves the session without a lock");
		my $id = last_move();
		ok($n1->poll_query_until('postgres', "SELECT phase = 'failed' FROM kingres.bucket_move WHERE move_id = $id"), "B: $point: the record is undone");
	}
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), 'active', 'B: the bucket is active');
	my $tgt = other($b);
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $tgt)");
	is($rc, 0, 'B: the next move of the bucket works') or diag($err);
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(advisory_locks(), '0', 'B: and leaves no lock');
	is(clean(), '', 'B: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# C. a second move of the bucket: refused at once while the first has its record; waits for the lock of the start while another session holds it
# ---------------------------------------------------------------------------
{
	my $b = 3;
	my $tgt = other($b);
	my ($pid, $file) = stopped('move_after_begin', "SELECT kingres.move_bucket($C, $b, $tgt)", 'c1');
	ok(wait_for_fault_file("$file.reached", 60), 'C: a move of bucket 3 waits between its record and its live mark');
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $tgt)");
	like($err, qr/already being moved/, 'C: a second move of the bucket is refused');
	release($pid, $file);
	unlike(out('c1'), qr/ERROR/, 'C: the first move ends') or diag(out('c1'));
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(advisory_locks(), '0', 'C: no lock is left');

	# another session holds the lock of the start (it is being started elsewhere): the move waits, and goes on when the lock is let go
	$b = 5;
	$tgt = other($b);
	my $h = holder("SELECT kingres.internal_move_group_take($C, $b)", 'c3');
	ok($n1->poll_query_until('postgres', q{SELECT count(*) = 1 FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid()}), 'C: another session holds the lock of the start of bucket 5');
	my ($pid2, $file2) = stopped('no_such_point', "SELECT kingres.move_bucket($C, $b, $tgt)", 'c2');
	usleep(1500000);
	is(out('c2'), '', 'C: the move waits');
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('preparing', 'copying', 'catching_up', 'switching')}), '0', 'C: and has written no record');
	end_holder($h);
	release($pid2, $file2);
	unlike(out('c2'), qr/ERROR/, 'C: and then it goes on and ends') or diag(out('c2'));
	is(owner($b), $tgt, 'C: the bucket is on its target');
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(advisory_locks(), '0', 'C: no lock is left');
	is(digest($n3), $base, 'C: every row is there');
}

# ---------------------------------------------------------------------------
# D. the waits are bounded
# ---------------------------------------------------------------------------
{
	my $b = 4;
	my $tgt = other($b);
	my $h = holder("SELECT kingres.internal_move_group_take($C, $b)", 'd1');
	ok($n1->poll_query_until('postgres', q{SELECT count(*) = 1 FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid()}), 'D: another session holds the lock of the start of bucket 4');
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($n1, qq{SET kingres_shard.node_timeout = 3;
		DO \$\$ BEGIN PERFORM kingres.move_bucket($C, $b, $tgt); EXCEPTION WHEN object_in_use THEN RAISE NOTICE 'ks-in-use: %', SQLERRM; END \$\$});
	like($err, qr/ks-in-use: bucket 4 of colocation group \d+ is being started by another session/, 'D: the wait ends with object_in_use');
	cmp_ok(time - $t0, '<', 60, 'D: and does not hang');
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('preparing', 'copying', 'catching_up', 'switching')}), '0', 'D: no record was written');
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), 'active', 'D: the bucket is active');
	end_holder($h);
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $tgt)");
	is($rc, 0, 'D: once the other session lets go the move works') or diag($err);
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});

	# the mark of a move: held by another session for longer than node_timeout
	$h = holder(q{SELECT kingres.internal_move_lock(987654)}, 'd2');
	ok($n1->poll_query_until('postgres', q{SELECT count(*) = 1 FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid()}), 'D: another session holds the mark of a move');
	$t0 = time;
	($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.node_timeout = 2; SELECT kingres.internal_move_lock(987654)});
	like($err, qr/the live mark of bucket move 987654 is held by another session/, 'D: the wait for the mark ends with object_in_use');
	cmp_ok(time - $t0, '<', 60, 'D: and does not hang');
	is(q1($n1, q{SELECT kingres.internal_move_alive(987654)}), 't', 'D: the mark is seen by the probe');
	end_holder($h);
	is(q1($n1, q{SELECT kingres.internal_move_alive(987654)}), 'f', 'D: and gone with the session');
	is(advisory_locks(), '0', 'D: no lock is left');
}

# ---------------------------------------------------------------------------
# E. the probes leave nothing behind; the unlocks are quiet when the lock is not held
# ---------------------------------------------------------------------------
{
	is(q1($n1, qq{SELECT kingres.internal_move_group_held($C, 0)::text || kingres.internal_move_group_held($C, 0)::text}), 'falsefalse', 'E: a probe of the lock of the start');
	my ($rc, $out, $err) = run_sql($n1, qq{SELECT kingres.internal_move_group_drop($C, 0); SELECT kingres.internal_move_unlock(424242)});
	is($rc, 0, 'E: letting go of what is not held works') or diag($err);
	unlike($err, qr/WARNING/, 'E: and does not warn');
	is(q1($n1, qq{SELECT kingres.internal_move_group_take($C, 0)::text || kingres.internal_move_group_held($C, 0)::text || (SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid())::text}), q{false1}, q{E: a session holds its own lock and does not see it as held by another});
	is(advisory_locks(), '0', 'E: and the end of the session lets go of it');
	is(clean(), '', 'E: check_cluster() finds nothing');
}


# ---------------------------------------------------------------------------
# F. a second cancel while the undo runs, a double take, a dead starter, a real error of the begin
# ---------------------------------------------------------------------------
sub script_session
{
	my ($sql, $name) = @_;
	my $f = "$tmp/script_$name.sql";
	open(my $fh, '>', $f) or die;
	print $fh $sql;
	close($fh);
	my $pid = fork();
	die 'fork' unless defined $pid;
	if ($pid == 0)
	{
		system("psql -X -At -h " . $n1->host . " -p " . $n1->port . " -d postgres -f $f > $tmp/out_$name 2>&1");
		POSIX::_exit(0);
	}
	return $pid;
}
{
	# F1. the move is cancelled at move_after_preparing; the undo (a remote call) waits for the cluster lock that a test session holds; a second cancel arrives
	# while it waits.  The session is pooled (a DO block catches the errors and goes on): it must hold neither the mark nor the lock of the start afterwards.
	my $b = 0;
	my $tgt = other($b);
	my $file = "$tmp/wait_f1";
	open(my $fh, '>', $file) or die;
	close($fh);
	my $pid = script_session(qq{SET kingres_shard.fault_injection = 'move_after_preparing:wait:$file';
DO \$\$ BEGIN PERFORM kingres.move_bucket($C, $b, $tgt); EXCEPTION WHEN OTHERS OR query_canceled THEN RAISE NOTICE 'caught %', SQLERRM; END \$\$;
RESET kingres_shard.fault_injection;
SELECT 'locks=' || count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid();
}, 'f1');
	ok(wait_for_fault_file("$file.reached", 60), 'F1: a move of bucket 0 waits at move_after_preparing');
	my $h = holder(q{LOCK TABLE kingres.map_version IN EXCLUSIVE MODE}, 'f1h');
	ok($n1->poll_query_until('postgres', q{SELECT count(*) = 1 FROM pg_locks WHERE locktype = 'relation' AND relation = 'kingres.map_version'::regclass AND granted AND pid <> pg_backend_pid()}), 'F1: a test session holds the cluster lock');
	my $mp = move_pid();
	q1($n1, "SELECT pg_cancel_backend($mp)");
	unlink($file);
	ok($n1->poll_query_until('postgres', q{SELECT count(*) >= 1 FROM pg_locks WHERE locktype = 'relation' AND relation = 'kingres.map_version'::regclass AND NOT granted}), 'F1: the undo waits for the cluster lock');
	is(q1($n1, "SELECT pid FROM pg_stat_activity WHERE pid = $mp AND state = 'active'"), $mp, 'F1: the first session is still in its handler');
	q1($n1, "SELECT pg_cancel_backend($mp)");
	waitpid($pid, 0);
	like(out('f1'), qr/locks=0/, 'F1: a second cancel during the undo leaves the session without the mark and without the lock of the start') or diag(out('f1'));
	end_holder($h);
	is(advisory_locks(), '0', 'F1: and no session holds a lock');
	my $id = last_move();
	q1($n1, "UPDATE kingres.bucket_move SET updated_at = clock_timestamp() - interval '1 hour' WHERE move_id = $id AND phase NOT IN ('failed', 'cancelled')");
	q1($n1, q{SELECT kingres.resume_bucket_moves()});
	ok($n1->poll_query_until('postgres', "SELECT phase IN ('failed', 'cancelled') FROM kingres.bucket_move WHERE move_id = $id"), 'F1: the move is undone (by the handler or by the recovery)');
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), 'active', 'F1: the bucket is active');
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $tgt)");
	is($rc, 0, 'F1: the next move of the bucket works') or diag($err);
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
}
{
	# F2. a lock taken twice by one session (a leak that survived, a second call) is let go whole
	is(q1($n1, qq{SELECT kingres.internal_move_group_take($C, 1)::text || kingres.internal_move_group_take($C, 1)::text || kingres.internal_move_lock(555001)::text || kingres.internal_move_lock(555001)::text;
	              SELECT kingres.internal_move_group_drop($C, 1)::text || kingres.internal_move_unlock(555001)::text;
	              SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()}), "\n\n0", 'F2: one drop and one unlock let go of a double take');
	is(q1($n1, qq{SELECT kingres.internal_move_group_take($C, 1)::text || kingres.internal_move_lock(555002)::text || kingres.internal_move_lock(555002)::text;
	              SELECT kingres.internal_move_release(555002, $C, 1)::text;
	              SELECT kingres.internal_move_release(NULL, NULL, NULL)::text;
	              SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()}), "\n\n\n0", 'F2: internal_move_release lets go of both, whatever the count, and takes nulls');
}
{
	# F3. the starter dies between its record and its mark: the recovery undoes the move (the lock of the start is gone with the session)
	my $b = 2;
	my $tgt = other($b);
	my ($pid, $file) = stopped('move_after_begin', "SELECT kingres.move_bucket($C, $b, $tgt)", 'f3');
	ok(wait_for_fault_file("$file.reached", 60), 'F3: a move of bucket 2 waits between its record and its live mark');
	my $id = last_move();
	q1($n1, 'SELECT pg_terminate_backend(' . move_pid() . ')');
	release($pid, $file);
	ok($n1->poll_query_until('postgres', "SELECT kingres.internal_move_alive($id) = false"), 'F3: with the session gone nothing holds the move');
	is(q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $id"), 'preparing', 'F3: its record is still there');
	q1($n1, "UPDATE kingres.bucket_move SET updated_at = clock_timestamp() - interval '1 hour' WHERE move_id = $id");
	is(q1($n1, q{SELECT kingres.resume_bucket_moves()}), '1', 'F3: a recovery pass undoes it');
	is(q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $id"), 'failed', 'F3: the record says failed');
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), 'active', 'F3: the bucket is active');
	is(advisory_locks(), '0', 'F3: no lock is left');
}
{
	# F4. a real error of the begin (the target takes no buckets): the handler has no move id, and the session is left without the lock of the start
	my $b = 3;
	my $tgt = other($b);
	q1($n1, "UPDATE kingres.node SET should_have_shards = false WHERE group_id = $tgt");
	my $before = q1($n1, q{SELECT count(*) FROM kingres.bucket_move});
	is(q1($n1, qq{DO \$\$ BEGIN PERFORM kingres.move_bucket($C, $b, $tgt); EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'caught %', SQLERRM; END \$\$;
	              SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()}), '0', 'F4: an error of the begin leaves the session without a lock');
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $tgt)");
	like($err, qr/does not accept buckets|should_have_shards/, 'F4: it is the refusal of the begin');
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_move}), $before, 'F4: no record was written');
	q1($n1, "UPDATE kingres.node SET should_have_shards = true WHERE group_id = $tgt");
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), 'active', 'F4: the bucket is active');
	is(clean(), '', 'F4: check_cluster() finds nothing');
}

done_testing();
