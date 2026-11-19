# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/122_insel_faults_moves.pl
#
# Step 5 of phase 2b (design/shard-phase2b.md, 10.2, 10.3, 11.7 and 16.2): faults, cancels, moves and
# failover of the colocated INSERT ... SELECT shipped whole (P6).  This is the P6 part of the design's
# 064 (moves and failover) and of its 058 (faults); the numbers 056 to 069 of the design are taken by
# tests of 0.5.  The fault points are insel_before_group, insel_after_group and insel_before_recheck of
# fanout.c, fanout_after_run, and the commit points of 2a.  The rule in every section: a statement
# either gives the rows of the single statement, each in the group that owns its bucket, or fails
# (KS001 when the map changed) and leaves nothing: no row in any group, no prepared transaction.
#
#   A. an error at every point, with and without RETURNING;
#   B. a cancel at every wait, and at a real wait on a group (a lock), by cancel and by timeout;
#   C. a group that does not answer (SIGSTOP) while it runs the fragment;
#   D. a group that dies; the origin that dies after the prepare and after its own commit;
#   E. a bucket that moves between the plan and the run, while a group runs, after every group ran;
#      a prepared statement planned before a move;
#   F. a group that changes its primary between the plan and the run;
#   G. a transaction that wrote a group through its old primary meets the new one;
#   H. memory: a result of 200000 rows with a small work_mem.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql
  make_standby register_standby wait_catchup set_sync promote_standby release_node);

my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my $ROWS = 240;
my $P6 = q{INSERT INTO public.dst SELECT id, v FROM public.src};
my $KS = qr/KS001|bucket map|map changed|no longer what it was|failed while a distributed statement|already uses node/;

# a cluster of three nodes with src and dst (colocated, 12 buckets, $ROWS rows in src)
sub setup3
{
	my ($prefix, %opt) = @_;
	my @n = make_cluster($prefix, 3, %opt);
	$n[0]->safe_psql('postgres', qq{
		CREATE TABLE public.src (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.src', 'id', bucket_count => 12);
		CREATE TABLE public.dst (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.dst', 'id', colocate_with => 'public.src');
		INSERT INTO public.src SELECT g, 'v' || g FROM generate_series(1, $ROWS) g;
	});
	$n[0]->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	return @n;
}

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# The table holds $want rows, every one of them in the group that owns its bucket now, once; nothing is prepared.
sub clean_state
{
	my ($nodes, $table, $want, $label) = @_;
	my $n1 = $nodes->[0];
	is($n1->safe_psql('postgres', "SELECT count(*) FROM $table"), $want, "$label: $table holds $want rows");
	my ($all, $wrong) = (0, 0);
	my $i = 0;
	for my $nd (@$nodes)
	{
		my $mine = $nd->safe_psql('postgres', qq{SET kingres_shard.enable_routing = off;
			SELECT count(*) FROM $table k JOIN kingres.bucket_map m ON m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, k.id) WHERE m.group_id = $i});
		my $total = $nd->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT count(*) FROM $table");
		$wrong += $total - $mine;
		$all += $total;
		$i++;
	}
	is($wrong, 0, "$label: every row is in the group that owns its bucket");
	is($all, $want, "$label: and in one group only");
	is(prepared_count($_), 0, "$label: nothing is prepared on " . $_->name) for @$nodes;
}

sub bucket_of
{
	my ($node, $group) = @_;
	return $node->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = $group");
}

# Run $stmt on node 1 up to the fault point; run $during; let it go; returns (out, err).
sub with_gate
{
	my ($n1, $point, $stmt, $during, $tag, $pre) = @_;
	my $gate = "$tmp/gate_is_$tag";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, ($pre // '') . " SET kingres_shard.fault_injection = '$point:wait:$gate'; $stmt");
	ok(wait_for_fault_file("$gate.reached", 60), "the statement waits at $point ($tag)");
	$during->();
	unlink $gate;
	return finish_psql($sess);
}

# ---------------------------------------------------------------------------
# A. an error at every point
# ---------------------------------------------------------------------------
my @nodes = setup3('if');
my ($n1, $n2, $n3) = @nodes;
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
like(plan_of($n1, $P6), qr/Kingres Fan-out Write/, 'the statement of the tests is shipped whole');
my @points = qw(insel_before_group insel_after_group insel_before_recheck fanout_after_run
				before_prepare after_first_prepare after_prepare after_log);
for my $variant ('', ' RETURNING id')
{
	for my $point (@points)
	{
		my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:error'; $P6$variant");
		isnt($rc, 0, "an error at $point fails the statement$variant");
		like($err, qr/injected failure at \Q$point\E/, '... with that error (the point was reached)');
		clean_state(\@nodes, 'public.dst', 0, "after an error at $point$variant");
		my ($rc2, $o2, $e2) = run_sql($n1, "$P6$variant");
		is($rc2, 0, "the next statement works after $point") or diag($e2);
		clean_state(\@nodes, 'public.dst', $ROWS, "after the next statement ($point)");
		$n1->safe_psql('postgres', 'TRUNCATE public.dst');
	}
}

# ---------------------------------------------------------------------------
# B. a cancel at every wait
# ---------------------------------------------------------------------------
{
	my $n = 0;
	for my $point (qw(insel_before_group insel_after_group insel_before_recheck fanout_after_run))
	{
		my $gate = "$tmp/gate_isb_" . $n++;
		append_to_file($gate, 'x');
		my $sess = start_psql($n1, "SET kingres_shard.fault_injection = '$point:wait:$gate';
			DO \$\$ BEGIN
			  BEGIN $P6; EXCEPTION WHEN query_canceled THEN RAISE NOTICE 'cancelled at $point'; END;
			  INSERT INTO public.dst SELECT id, v FROM public.src;
			END \$\$");
		ok(wait_for_fault_file("$gate.reached", 30), "the statement waits at $point");
		my $pid = $n1->safe_psql('postgres',
			"SELECT pid FROM pg_stat_activity WHERE query LIKE '%$point:wait:$gate%' AND pid <> pg_backend_pid()");
		$n1->safe_psql('postgres', "SELECT pg_cancel_backend($pid)");
		unlink $gate;
		my ($out, $err) = finish_psql($sess);
		like($err, qr/cancelled at \Q$point\E/, "a cancel at $point is caught by the block");
		unlike($err, qr/ERROR/, '... and the transaction goes on without an error');
		clean_state(\@nodes, 'public.dst', $ROWS, "after a cancel at $point and the next statement");
		is($n1->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction'"), 0,
			'no session of the origin is left in a transaction');
		$n1->safe_psql('postgres', 'TRUNCATE public.dst');
	}

	# a real wait: a group is blocked by a lock on dst, which a cancel and a timeout must end
	for my $how ('cancel', 'timeout')
	{
		my $lock = start_psql($n2, "SET kingres_shard.enable_routing = off; BEGIN; LOCK TABLE public.dst IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(300); COMMIT;");
		my $ok = 0;
		for (1 .. 100)
		{
			$ok = $n2->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE query LIKE '%pg_sleep(300)%' AND state = 'active' AND pid <> pg_backend_pid()") >= 1;
			last if $ok;
			sleep(0.1);
		}
		ok($ok, "node 2 holds the lock ($how)");
		my $pre = $how eq 'timeout' ? 'SET statement_timeout = 1000; ' : '';
		my $sess = start_psql($n1, "$pre$P6");
		my $waiting = 0;
		for (1 .. 200)
		{
			$waiting = $n2->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%insert-select-fragment%'") >= 1;
			last if $waiting;
			sleep(0.1);
		}
		ok($waiting, "the fragment waits for the lock on node 2 ($how)");
		my $t0 = time;
		if ($how eq 'cancel')
		{
			my $pid = $n1->safe_psql('postgres', "SELECT pid FROM pg_stat_activity WHERE query LIKE 'INSERT INTO public.dst%' AND pid <> pg_backend_pid()");
			$n1->safe_psql('postgres', "SELECT pg_cancel_backend($pid)");
		}
		my ($out, $err) = finish_psql($sess);
		my $took = time - $t0;
		like($err, $how eq 'cancel' ? qr/canceling statement due to user request/ : qr/canceling statement due to statement timeout/,
			"the statement ends with the error of the $how");
		cmp_ok($took, '<', 30, "... within a bounded time ($took seconds)");
		my $left = 1;
		for (1 .. 200)
		{
			$left = $n2->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%insert-select-fragment%'");
			last if $left == 0;
			sleep(0.1);
		}
		is($left, 0, "no backend of node 2 still waits for the lock while it is held ($how)");
		my $lpid = $n2->safe_psql('postgres', "SELECT pid FROM pg_stat_activity WHERE query LIKE '%pg_sleep(300)%' AND pid <> pg_backend_pid()");
		$n2->safe_psql('postgres', "SELECT pg_terminate_backend($lpid)");
		finish_psql($lock);
		clean_state(\@nodes, 'public.dst', 0, "after the $how at a real wait");
	}
}

# ---------------------------------------------------------------------------
# C. a group that does not answer
# ---------------------------------------------------------------------------
{
	my $gate = "$tmp/gate_isc";
	append_to_file($gate, 'x');
	# the first statement opens (and keeps) the session of the origin on node 2, which is then frozen
	my $sess = start_psql($n1, "$P6; DELETE FROM public.dst; SET kingres_shard.fault_injection = 'insel_before_group:wait:$gate'; $P6");
	ok(wait_for_fault_file("$gate.reached", 60), 'the statement waits before its first group');
	my @pids = split /\n/, $n2->safe_psql('postgres', "SELECT pid FROM pg_stat_activity WHERE application_name = 'kingres_shard'");
	cmp_ok(scalar(@pids), '>=', 1, 'the session of the origin on node 2 is found');
	kill 'STOP', @pids;
	unlink $gate;
	sleep(1.5);
	my $pid = $n1->safe_psql('postgres', "SELECT pid FROM pg_stat_activity WHERE query LIKE '%insel_before_group:wait:$gate%' AND pid <> pg_backend_pid()");
	my $t0 = time;
	$n1->safe_psql('postgres', "SELECT pg_cancel_backend($pid)");
	my ($out, $err) = finish_psql($sess);
	my $took = time - $t0;
	kill 'CONT', @pids;
	like($err, qr/canceling statement due to user request/, 'a group that does not answer: a cancel ends the statement');
	cmp_ok($took, '<', 30, "... within a bounded time ($took seconds)");
	sleep(1);
	clean_state(\@nodes, 'public.dst', 0, 'after the frozen group');
}

# ---------------------------------------------------------------------------
# D. a group that dies; the origin that dies
# ---------------------------------------------------------------------------
{
	my ($out, $err) = with_gate($n1, 'insel_after_group', $P6, sub { $n3->stop('immediate') }, 'die');
	isnt($err, '', 'a group that died fails the statement');
	like($err, qr/connect|connection|lost|terminat|FATAL|server closed|failed/i, '... with a connection error');
	$n3->start;
	$n3->poll_query_until('postgres', 'SELECT true');
	clean_state(\@nodes, 'public.dst', 0, 'after a group died in the middle of the statement');
}
sub crash_origin
{
	my ($point) = @_;
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:kill'; $P6");
	isnt($rc, 0, "the origin dies at $point");
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
}
{
	crash_origin('after_prepare');
	cmp_ok(prepared_count($n2) + prepared_count($n3), '>=', 1, 'transactions are left prepared on the groups');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	clean_state(\@nodes, 'public.dst', 0, 'after recovery of a crash before the decision was logged');
}
{
	crash_origin('after_commit');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	clean_state(\@nodes, 'public.dst', $ROWS, 'after recovery of a crash after the local commit');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.src s JOIN public.dst d USING (id) WHERE s.v = d.v'), $ROWS,
		'... dst is the copy of src');
}
for my $nd (@nodes) { release_node($nd); }

# ---------------------------------------------------------------------------
# E. a bucket moves
# ---------------------------------------------------------------------------
{
	# 1. between the other two groups, after the plan and before the first group runs
	my @m = setup3('ie1', logical => 1);
	my $m1 = $m[0];
	my $b = bucket_of($m1, 1);
	my ($out, $err) = with_gate($m1, 'insel_before_group', $P6, sub { $m1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, 2)") }, 'e1');
	like($err, $KS, 'a bucket that moved between the plan and the run fails the statement with the retryable error');
	clean_state(\@m, 'public.dst', 0, 'after the failed statement');
	my ($rc, $o2, $e2) = run_sql($m1, $P6);
	is($rc, 0, 'the statement repeated is planned with the new map and works') or diag($e2);
	clean_state(\@m, 'public.dst', $ROWS, 'after the repeat');
	release_node($_) for @m;
}
{
	# 2. a bucket of the origin's own group moves away while the statement waits after its first group
	my @m = setup3('ie2', logical => 1);
	my $m1 = $m[0];
	my $b = bucket_of($m1, 0);
	my $mover;
	my ($out, $err) = with_gate($m1, 'insel_after_group', $P6, sub {
			$mover = start_psql($m1, "SELECT kingres.move_bucket(1, $b, 1)");
			sleep(4);
		}, 'e2');
	unlike($err, qr/ERROR/, 'the statement is not disturbed by the move of a bucket of the origin: the switch waits for it');
	my ($mo, $me) = finish_psql($mover);
	unlike($me, qr/ERROR/, '... and the move completes after it') or diag($me);
	is($m1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"), 1, 'the bucket is in its new group');
	clean_state(\@m, 'public.dst', $ROWS, 'after the statement and the move');
	release_node($_) for @m;
}
{
	# 3. a bucket moves into a group that the statement has written, after every group ran
	my @m = setup3('ie3', logical => 1);
	my $m1 = $m[0];
	my $b = bucket_of($m1, 2);
	my $mover;
	my ($out, $err) = with_gate($m1, 'insel_before_recheck', $P6, sub {
			$mover = start_psql($m1, "SELECT kingres.move_bucket(1, $b, 1)");
			sleep(4);
		}, 'e3');
	my ($mo, $me) = finish_psql($mover);
	unlike($me, qr/ERROR/, 'the move completes') or diag($me);
	if ($err =~ /ERROR/)
	{
		like($err, $KS, 'the statement failed with the retryable error');
		clean_state(\@m, 'public.dst', 0, 'after the failed statement and the move');
	}
	else
	{
		pass('the statement succeeded');
		clean_state(\@m, 'public.dst', $ROWS, 'after the statement and the move');
	}
	release_node($_) for @m;
}
{
	# 4. a prepared statement planned before a move, executed after it
	my @m = setup3('ie4', logical => 1);
	my $m1 = $m[0];
	my $b = bucket_of($m1, 1);
	my $s = $m1->background_psql('postgres', on_error_stop => 0);
	$s->query_safe(q{PREPARE pi AS INSERT INTO public.dst SELECT id, v FROM public.src ON CONFLICT DO NOTHING});
	$s->query_safe('EXECUTE pi') for (1 .. 6);
	$s->query_safe('DELETE FROM public.dst');
	$m1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, 2)");
	$s->query_safe('SET plan_cache_mode = force_generic_plan');
	$s->{stderr} = '';
	$s->query('EXECUTE pi');
	my $e = $s->{stderr};
	$s->{stderr} = '';
	if ($e =~ /ERROR/)
	{
		like($e, $KS, 'the plan made before the move fails with the retryable error');
		$s->query('EXECUTE pi');
		is($s->{stderr}, '', '... and works when repeated');
	}
	else
	{
		pass('the plan made before the move was right');
	}
	$s->quit;
	clean_state(\@m, 'public.dst', $ROWS, 'after the prepared statement and the move');
	release_node($_) for @m;
}

# ---------------------------------------------------------------------------
# F. a group changes its primary between the plan and the run
# ---------------------------------------------------------------------------
sub setup2
{
	my ($prefix) = @_;
	my ($a, $b) = make_cluster($prefix, 2, replication => 1);
	$a->safe_psql('postgres', qq{
		CREATE TABLE public.src (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.src', 'id', bucket_count => 12);
		CREATE TABLE public.dst (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.dst', 'id', colocate_with => 'public.src');
		CREATE TABLE public.dst2 (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.dst2', 'id', colocate_with => 'public.src');
		INSERT INTO public.src SELECT g, 'v' || g FROM generate_series(1, 120) g;
	});
	my $s = make_standby($b, "${prefix}_s2");
	my $id = register_standby($a, $s, 1);
	set_sync($b, $s);
	wait_catchup($b, $s);
	$a->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	return ($a, $b, $s, $id);
}
{
	my ($f1, $f2, $s2, $id) = setup2('if2');
	like(plan_of($f1, $P6), qr/Kingres Fan-out Write/, 'the statement of the failover test is shipped whole');
	my ($out, $err) = with_gate($f1, 'insel_before_group', $P6, sub {
			$f2->safe_psql('postgres', 'CHECKPOINT');
			promote_standby($s2);
			set_sync($f2);
			$f1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
		}, 'f');
	like($err, $KS, 'a primary that changed between the plan and the run fails the statement with the retryable error');
	is($f1->safe_psql('postgres', 'SELECT count(*) FROM public.dst'), 0, '... and nothing of it is stored');
	my ($rc, $o2, $e2) = run_sql($f1, $P6);
	is($rc, 0, 'the statement repeated goes to the new primary') or diag($e2);
	is($f1->safe_psql('postgres', 'SELECT count(*) FROM public.dst'), 120, '... and stores its rows');
	is($s2->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.dst}) +
	   $f1->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.dst}), 120,
		'in the new primary and in the origin, once');
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for ($f1, $s2);
}

# ---------------------------------------------------------------------------
# G. a transaction that wrote a group through its old primary meets the new one
# ---------------------------------------------------------------------------
{
	my ($g1, $g2, $s2, $id) = setup2('ig');
	my $b = $g1->background_psql('postgres', on_error_stop => 0);
	$b->query_safe('BEGIN');
	$b->query_safe($P6);
	$g2->safe_psql('postgres', 'CHECKPOINT');
	promote_standby($s2);
	set_sync($g2);
	$g1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	$b->query(q{INSERT INTO public.dst2 SELECT id, v FROM public.src});
	like($b->{stderr}, qr/already uses node .* of shard group 1/,
		'a second statement of a transaction that wrote the group through its old primary is refused: one node of a group per transaction');
	$b->{stderr} = '';
	$b->query('ROLLBACK');
	$b->quit;
	is($g1->safe_psql('postgres', 'SELECT count(*) FROM public.dst') + $g1->safe_psql('postgres', 'SELECT count(*) FROM public.dst2'), 0,
		'... and the transaction left nothing');
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for ($g1, $g2, $s2);
}

# ---------------------------------------------------------------------------
# H. memory
# ---------------------------------------------------------------------------
SKIP:
{
	skip 'needs /proc', 4 unless -r '/proc/self/status';
	my @m = setup3('ih');
	my $m1 = $m[0];
	$m1->safe_psql('postgres', q{INSERT INTO public.src SELECT g, 'big' || g FROM generate_series(1000, 200999) g});
	my $b = $m1->background_psql('postgres', on_error_stop => 1);
	my $pid = $b->query_safe('SELECT pg_backend_pid()');
	chomp $pid;
	my $hwm = sub {
		open(my $fh, '<', "/proc/$pid/status") or return -1;
		while (<$fh>) { return $1 if /^VmHWM:\s+(\d+) kB/ }
		return -1;
	};
	$b->query_safe(q{SET work_mem = '64kB'});
	my $before = $hwm->();
	my $out = $b->query_safe(q{INSERT INTO public.dst SELECT id, v FROM public.src WHERE id >= 1000 RETURNING id});
	my @ids = grep { /^\d+$/ } split /\n/, $out;
	is(scalar(@ids), 200000, 'RETURNING gives every one of 200000 rows');
	my $after = $hwm->();
	cmp_ok($after - $before, '<', 300 * 1024, "the memory of the origin stays bounded (peak grew by " . ($after - $before) . " kB)");
	is($m1->safe_psql('postgres', 'SELECT count(*) FROM public.dst'), 200000, 'the table holds the rows');
	$b->quit;
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @m;
}

done_testing();
