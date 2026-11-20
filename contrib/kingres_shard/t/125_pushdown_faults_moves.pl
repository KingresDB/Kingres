# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/125_pushdown_faults_moves.pl
#
# Step 6 of phase 2b (design/shard-phase2b.md, 4.3, 4.4, 4.6, 10.1 to 10.4 and 16.2): the Kingres Fragment
# Scan against faults, cancels, a stream that is read for a long time, a bucket that moves, a change of primary
# and a table that changes under a prepared plan.  It is the step 6 part of the design's 063 (consistency),
# 064 (failover) and 058 (faults); the numbers 055 to 069 of the design are taken by tests of 0.5.  The rule of
# every section: a statement either gives exactly the rows that the same statement gives when nothing
# happens, or fails (KS001 when the map or a primary changed), and leaves nothing behind: no session of a node
# in a transaction, no cursor, no prepared transaction; the statement repeated works.
#
#   A. an error at every fault point of the fragment scan, and the next statement;
#   B. a cancel at every wait, and a real wait on a group (a lock), cancelled and timed out;
#   C. a stream read in small batches: a LIMIT that stops it, a group that dies in the middle of it,
#      a transaction that reads it at REPEATABLE READ while another one writes;
#   D. a bucket moves: between the plan and the start, while the stream is read, a prepared plan across a move,
#      a table dropped and made again, and made a table of another colocation group;
#   E. a change of primary: between the plan and the start, in the middle of the stream, a transaction that
#      read the old primary;
#   F. memory: a result of 200000 rows with a small work_mem.

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
my $Q = q{SELECT id, count(*), sum(g) FROM public.s GROUP BY id};
my $KS = qr/KS001|bucket map|map changed|no longer what it was|failed while a distributed statement|already uses node|partitions of table/;

sub setup3
{
	my ($prefix, %opt) = @_;
	my @n = make_cluster($prefix, 3, extra => "kingres_shard.enable_partial_aggregation = off
", %opt);
	$n[0]->safe_psql('postgres', qq{
		CREATE TABLE public.s (id int, g int, v text, c int DEFAULT 1, PRIMARY KEY (id, c));
		SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12);
		INSERT INTO public.s SELECT g, g % 7, 'v' || g, dup FROM generate_series(1, $ROWS) g, generate_series(1, 2) dup;
	});
	$_->safe_psql('postgres', 'ANALYZE') for @n;
	return @n;
}

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

sub rows_of { my ($out) = @_; return join("\n", sort grep { /^\d+\|/ } split /\n/, $out); }

# the answer of the closed query when nothing happens
sub baseline
{
	my ($node) = @_;
	my ($rc, $out, $err) = run_sql($node, $Q);
	die "the baseline fails: $err" if $rc != 0;
	return rows_of($out);
}

# no session of a node is in a transaction, no cursor is open, nothing is prepared
sub clean_nodes
{
	my ($label, @nodes) = @_;
	for my $n (@nodes)
	{
		my $left = 1;
		for (1 .. 100)
		{
			$left = $n->safe_psql('postgres', q{SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction' AND pid <> pg_backend_pid()});
			last if $left == 0;
			sleep(0.1);
		}
		is($left, 0, "$label: no session of " . $n->name . ' is left in a transaction');
		is($n->safe_psql('postgres', q{SELECT count(*) FROM pg_cursors}), 0, "$label: no cursor is open on " . $n->name);
		is(prepared_count($n), 0, "$label: nothing is prepared on " . $n->name);
	}
}

# Run $stmt on $n1 up to the fault point; run $during; let it go; returns (out, err).
sub with_gate
{
	my ($n1, $point, $stmt, $during, $tag, $pre) = @_;
	my $gate = "$tmp/gate_pf_$tag";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, ($pre // '') . " SET kingres_shard.fault_injection = '$point:wait:$gate'; $stmt");
	ok(wait_for_fault_file("$gate.reached", 60), "the statement waits at $point ($tag)");
	$during->();
	unlink $gate;
	return finish_psql($sess);
}

# ---------------------------------------------------------------------------
# A. an error at every fault point
# ---------------------------------------------------------------------------
my @nodes = setup3('pf');
my ($n1, $n2, $n3) = @nodes;
my $WANT = baseline($n1);
is(scalar(split /\n/, $WANT), $ROWS, 'the baseline has a row for every id');
like(plan_of($n1, $Q), qr/Kingres Fragment Scan/, 'the statement of the tests is shipped');
{
	my @points = qw(frag_before_probe frag_after_probe_before frag_after_declare frag_local_open frag_after_probe_after
					frag_first_fetch frag_declared_g1 frag_declared_g2 frag_before_fetch frag_local_fetch frag_wait_batch);
	for my $point (@points)
	{
		my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.scan_fetch_size = 20; SET kingres_shard.fault_injection = '$point:error'; $Q");
		isnt($rc, 0, "an error at $point fails the statement");
		like($err, qr/injected failure at \Q$point\E/, '... with that error (the point was reached)');
		my ($rc2, $o2, $e2) = run_sql($n1, $Q);
		is(rows_of($o2), $WANT, "the next statement after $point gives the baseline") or diag($e2);
	}
	clean_nodes('after the errors', @nodes);
}

# ---------------------------------------------------------------------------
# B. a cancel at every wait
# ---------------------------------------------------------------------------
{
	my $n = 0;
	for my $point (qw(frag_before_probe frag_after_probe_before frag_after_declare frag_local_open frag_after_probe_after frag_first_fetch frag_before_fetch frag_local_fetch frag_wait_batch))
	{
		my $gate = "$tmp/gate_pfb_" . $n++;
		append_to_file($gate, 'x');
		my $sess = start_psql($n1, "SET kingres_shard.scan_fetch_size = 20; SET kingres_shard.fault_injection = '$point:wait:$gate';
			DO \$\$ BEGIN
			  BEGIN PERFORM count(*) FROM (SELECT 1) one; PERFORM id, count(*) FROM public.s GROUP BY id; EXCEPTION WHEN query_canceled THEN RAISE NOTICE 'cancelled at $point'; END;
			  PERFORM id, count(*), sum(g) FROM public.s GROUP BY id;
			END \$\$");
		ok(wait_for_fault_file("$gate.reached", 30), "the statement waits at $point");
		my $pid = $n1->safe_psql('postgres', "SELECT pid FROM pg_stat_activity WHERE query LIKE '%$point:wait:$gate%' AND pid <> pg_backend_pid()");
		$n1->safe_psql('postgres', "SELECT pg_cancel_backend($pid)");
		unlink $gate;
		my ($out, $err) = finish_psql($sess);
		like($err, qr/cancelled at \Q$point\E/, "a cancel at $point is caught by the block");
		unlike($err, qr/ERROR/, '... and the transaction goes on without an error');
		is(rows_of($n1->safe_psql('postgres', $Q)), $WANT, "the statement after the cancel at $point gives the baseline");
	}
	clean_nodes('after the cancels', @nodes);

	# a real wait: a group is blocked by a lock on its partitions, which a cancel and a timeout must end
	for my $how ('cancel', 'timeout')
	{
		my $lock = start_psql($n2, "SET kingres_shard.enable_routing = off; BEGIN; LOCK TABLE public.s IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(300); COMMIT;");
		my $ok = 0;
		for (1 .. 100)
		{
			$ok = $n2->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE query LIKE '%pg_sleep(300)%' AND state = 'active' AND pid <> pg_backend_pid()") >= 1;
			last if $ok;
			sleep(0.1);
		}
		ok($ok, "node 2 holds the lock ($how)");
		my $pre = $how eq 'timeout' ? 'SET statement_timeout = 1500; ' : '';
		my $sess = start_psql($n1, "$pre$Q");
		my $waiting = 0;
		for (1 .. 200)
		{
			$waiting = $n2->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND pid <> pg_backend_pid()") >= 1;
			last if $waiting;
			sleep(0.1);
		}
		ok($waiting, "the fragment waits for the lock on node 2 ($how)");
		my $t0 = time;
		if ($how eq 'cancel')
		{
			my $pid = $n1->safe_psql('postgres', "SELECT pid FROM pg_stat_activity WHERE query LIKE 'SELECT id, count(*), sum(g)%' AND pid <> pg_backend_pid()");
			$n1->safe_psql('postgres', "SELECT pg_cancel_backend($pid)");
		}
		my ($out, $err) = finish_psql($sess);
		my $took = time - $t0;
		like($err, $how eq 'cancel' ? qr/canceling statement due to user request/ : qr/canceling statement due to statement timeout/, "the statement ends with the error of the $how");
		cmp_ok($took, '<', 30, "... within a bounded time ($took seconds)");
		my $lpid = $n2->safe_psql('postgres', "SELECT pid FROM pg_stat_activity WHERE query LIKE '%pg_sleep(300)%' AND pid <> pg_backend_pid()");
		$n2->safe_psql('postgres', "SELECT pg_terminate_backend($lpid)");
		finish_psql($lock);
		my ($rc, $o, $e) = run_sql($n1, $Q);
		is(rows_of($o), $WANT, "the statement after the $how at a real wait gives the baseline") or diag($e);
		clean_nodes("after the $how", @nodes);
	}
}

# ---------------------------------------------------------------------------
# C. a stream read in small batches
# ---------------------------------------------------------------------------
{
	# a LIMIT that stops the stream, with the batch in flight and the cursors open
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.scan_fetch_size = 3; $Q LIMIT 5");
	is($rc, 0, 'a LIMIT that stops a stream of batches of 3 rows') or diag($err);
	is(scalar(grep { /^\d+\|/ } split /\n/, $out), 5, '... returns its five rows');
	like(plan_of($n1, "$Q LIMIT 5"), qr/Limit\n\s+->  Custom Scan \(Kingres Fragment Scan\)/, '... and the limit is directly above the scan');
	clean_nodes('after the LIMIT', @nodes);
	my ($rc2, $o2, $e2) = run_sql($n1, "SET kingres_shard.scan_fetch_size = 3; BEGIN; DECLARE c NO SCROLL CURSOR FOR $Q; FETCH 4 FROM c; CLOSE c; FETCH 0 FROM nothing_here;");
	like($e2, qr/cursor "nothing_here" does not exist/, 'a cursor closed early, then an error');
	clean_nodes('after the closed cursor', @nodes);
	# the whole result with tiny batches
	my ($rc3, $o3, $e3) = run_sql($n1, "SET kingres_shard.scan_fetch_size = 1; $Q");
	is(rows_of($o3), $WANT, 'the whole result in batches of one row is the baseline');

	# a group that dies in the middle of the stream
	my ($o4, $e4) = with_gate($n1, 'frag_wait_batch', "SET kingres_shard.scan_fetch_size = 2; $Q", sub {
			my $pid = $n2->safe_psql('postgres', q{SELECT pid FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND (query LIKE 'FETCH FORWARD%') ORDER BY backend_start DESC LIMIT 1});
			$n2->safe_psql('postgres', "SELECT pg_terminate_backend($pid)");
		}, 'die');
	like($e4, qr/ERROR/, 'a group whose session dies in the middle of the stream fails the statement');
	my ($rc5, $o5, $e5) = run_sql($n1, $Q);
	is(rows_of($o5), $WANT, '... and the next statement gives the baseline') or diag($e5);
	clean_nodes('after the death of a session', @nodes);

	# REPEATABLE READ: another transaction writes between two reads
	my $r = $n1->background_psql('postgres', on_error_stop => 0);
	$r->query_safe('BEGIN ISOLATION LEVEL REPEATABLE READ');
	my $first = rows_of($r->query_safe($Q));
	$n2->safe_psql('postgres', q{INSERT INTO public.s VALUES (100001, 1, 'late'), (100002, 2, 'late'), (100003, 3, 'late')});
	my $second = rows_of($r->query_safe($Q));
	is($second, $first, 'REPEATABLE READ: the second read of the transaction sees what the first saw');
	$r->query_safe('COMMIT');
	my $third = rows_of($r->query_safe($Q));
	is(scalar(split /\n/, $third), $ROWS + 3, '... and the next statement sees the new rows');
	$r->quit;
	$n1->safe_psql('postgres', q{DELETE FROM public.s WHERE id > 100000});
	clean_nodes('after the isolation test', @nodes);
}
for my $nd (@nodes) { release_node($nd); }

# ---------------------------------------------------------------------------
# D. a bucket moves, a table changes
# ---------------------------------------------------------------------------
sub bucket_of
{
	my ($node, $group) = @_;
	return $node->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = $group");
}
{
	my @m = setup3('pd', logical => 1);
	my $m1 = $m[0];
	my $want = baseline($m1);

	# 1. between the plan and the start, a move between the other two groups
	my $b = bucket_of($m1, 1);
	my ($out, $err) = with_gate($m1, 'frag_before_probe', $Q, sub { $m1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, 2)") }, 'd1');
	if ($err =~ /ERROR/) { like($err, $KS, 'a bucket that moved between the plan and the run fails the statement with the retryable error'); }
	else { is(rows_of($out), $want, 'a bucket that moved between the plan and the run: the statement was right'); }
	my ($rc, $o2, $e2) = run_sql($m1, $Q);
	is(rows_of($o2), $want, 'the statement repeated is planned with the new map and gives the baseline') or diag($e2);

	# 2. while the stream is read: the cursors hold the partitions, the switch waits for the statement
	$b = bucket_of($m1, 0);
	my $mover;
	($out, $err) = with_gate($m1, 'frag_wait_batch', "SET kingres_shard.scan_fetch_size = 2; $Q", sub {
			$mover = start_psql($m1, "SELECT kingres.move_bucket(1, $b, 1)");
			sleep(4);
		}, 'd2');
	unlike($err, qr/ERROR/, 'a move while the stream is read does not disturb the statement');
	is(rows_of($out), $want, '... and every row is read once');
	my ($mo, $me) = finish_psql($mover);
	unlike($me, qr/ERROR/, '... the move completes after it') or diag($me);
	is(baseline($m1), $want, 'after the move the answer is the same');

	# 4. a prepared plan across a move
	my $s = $m1->background_psql('postgres', on_error_stop => 0);
	$s->query_safe(qq{PREPARE pq AS $Q});
	$s->query_safe('EXECUTE pq') for (1 .. 6);
	my $b3 = bucket_of($m1, 1);
	$m1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b3, 2)");
	$s->query_safe('SET plan_cache_mode = force_generic_plan');
	$s->{stderr} = '';
	my $got = $s->query('EXECUTE pq');
	my $e = $s->{stderr};
	$s->{stderr} = '';
	if ($e =~ /ERROR/)
	{
		like($e, $KS, 'the plan made before the move fails with the retryable error');
		is(rows_of($s->query_safe('EXECUTE pq')), $want, '... and works when repeated');
	}
	else { is(rows_of($got), $want, 'the plan made before the move was right'); }
	$s->quit;

	# 5. a table dropped and made again, a table of another colocation group, between the plan and the run
	my $t = $m1->background_psql('postgres', on_error_stop => 0);
	$t->query_safe('SET plan_cache_mode = force_generic_plan');
	$t->query_safe(qq{PREPARE pt AS $Q});
	$t->query_safe('EXECUTE pt') for (1 .. 6);
	$m1->safe_psql('postgres', q{DROP TABLE public.s});
	$m1->safe_psql('postgres', q{CREATE TABLE public.s (id int, g int, v text, c int DEFAULT 1, PRIMARY KEY (id, c)); SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 7);
		INSERT INTO public.s SELECT g, g % 5, 'w' || g, dup FROM generate_series(1, 50) g, generate_series(1, 2) dup});
	$t->{stderr} = '';
	my $after = $t->query('EXECUTE pt');
	$e = $t->{stderr};
	$t->{stderr} = '';
	if ($e =~ /ERROR/) { like($e, qr/$KS|does not exist|no longer/, 'a plan made for a table that was dropped and made again fails, never answers from the old table'); }
	else { is(scalar(grep { /^\d+\|/ } split /\n/, $after), 50, 'a plan made for a table that was dropped and made again answers from the new table'); }
	$t->quit;
	release_node($_) for @m;
}

# ---------------------------------------------------------------------------
# E. a change of primary
# ---------------------------------------------------------------------------
sub setup2
{
	my ($prefix) = @_;
	my ($a, $b) = make_cluster($prefix, 2, replication => 1);
	$a->safe_psql('postgres', qq{
		CREATE TABLE public.s (id int, g int, v text, c int DEFAULT 1, PRIMARY KEY (id, c));
		SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12);
		INSERT INTO public.s SELECT g, g % 7, 'v' || g, dup FROM generate_series(1, $ROWS) g, generate_series(1, 2) dup;
	});
	my $s = make_standby($b, "${prefix}_s2");
	my $id = register_standby($a, $s, 1);
	set_sync($b, $s);
	wait_catchup($b, $s);
	$a->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	$_->safe_psql('postgres', 'ANALYZE') for ($a, $b);
	return ($a, $b, $s, $id);
}
{
	my ($f1, $f2, $s2, $id) = setup2('pe1');
	like(plan_of($f1, $Q), qr/Kingres Fragment Scan/, 'the statement is shipped on the cluster with a standby');
	my $want = baseline($f1);
	my ($out, $err) = with_gate($f1, 'frag_before_probe', $Q, sub {
			$f2->safe_psql('postgres', 'CHECKPOINT');
			promote_standby($s2);
			set_sync($f2);
			$f1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
		}, 'e1');
	like($err, $KS, 'a primary that changed between the plan and the run fails the statement with the retryable error');
	my ($rc, $o2, $e2) = run_sql($f1, $Q);
	is(rows_of($o2), $want, 'the statement repeated goes to the new primary and gives the baseline') or diag($e2);
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for ($f1, $s2);
}
{
	# the node that serves a group stops in the middle of the stream: KS001 (the group has a standby), never a partial answer
	my ($g1, $g2, $s2, $id) = setup2('pe2');
	my $want = baseline($g1);
	my ($out, $err) = with_gate($g1, 'frag_wait_batch', "SET kingres_shard.scan_fetch_size = 2; $Q", sub { $g2->stop('immediate'); }, 'e2');
	like($err, $KS, 'a group that stops in the middle of the stream fails with the retryable error (it has a standby)');
	is(scalar(grep { /^\d+\|/ } split /\n/, $out), 0, '... and printed no row');
}
{
	# a transaction that read through the old primary meets the new one
	my ($g1, $g2, $s2, $id) = setup2('pe3');
	my $b = $g1->background_psql('postgres', on_error_stop => 0);
	$b->query_safe('BEGIN');
	$b->query_safe($Q);
	$g2->safe_psql('postgres', 'CHECKPOINT');
	promote_standby($s2);
	set_sync($g2);
	$g1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	$b->query($Q);
	like($b->{stderr}, qr/already uses node .* of shard group 1|bucket map|map changed|no longer/,
		'a second read of a transaction that read the group through its old primary is refused: one node of a group per transaction');
	$b->{stderr} = '';
	$b->query('ROLLBACK');
	$b->quit;
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for ($g1, $g2, $s2);
}

# ---------------------------------------------------------------------------
# F. memory
# ---------------------------------------------------------------------------
SKIP:
{
	skip 'needs /proc', 3 unless -r '/proc/self/status';
	my @m = setup3('pm');
	my $m1 = $m[0];
	$m1->safe_psql('postgres', q{INSERT INTO public.s SELECT g, g % 7, 'big' || g, dup FROM generate_series(1000, 200999) g, generate_series(1, 2) dup});
	$_->safe_psql('postgres', 'ANALYZE') for @m;
	my $b = $m1->background_psql('postgres', on_error_stop => 1);
	my $pid = $b->query_safe('SELECT pg_backend_pid()');
	chomp $pid;
	my $hwm = sub {
		open(my $fh, '<', "/proc/$pid/status") or return -1;
		while (<$fh>) { return $1 if /^VmHWM:\s+(\d+) kB/ }
		return -1;
	};
	$b->query_safe(q{SET work_mem = '64kB'});
	like(plan_of($m1, $Q), qr/Fragment Scan/, 'the big query is shipped');
	my $before = $hwm->();
	my $out = $b->query_safe($Q);
	my @ids = grep { /^\d+\|/ } split /\n/, $out;
	is(scalar(@ids), 200000 + $ROWS, 'a result of 200240 rows');
	my $after = $hwm->();
	cmp_ok($after - $before, '<', 300 * 1024, "the memory of the origin stays bounded (peak grew by " . ($after - $before) . " kB)");
	$b->quit;
	release_node($_) for @m;
}

done_testing();
