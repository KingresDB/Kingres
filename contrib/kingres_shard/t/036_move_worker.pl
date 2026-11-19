# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The recovery worker and moves (review finding S4-02): the worker of the map
# group primary resumes a move whose session died and drops the orphan tables of
# a finished move after the delay, as README and the design say, and never
# touches a move whose session is alive.  kingres.check_cluster() reports a
# move that nobody runs and orphan tables that are overdue, for a cluster whose
# worker is switched off.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use KingresShardTest qw(make_cluster run_sql wait_for_fault_file);

my $tmp = PostgreSQL::Test::Utils::tempdir();
my $settings = "kingres_shard.node_timeout = 3\nkingres_shard.move_cleanup_delay = 1\n";

sub setup_cluster
{
	my ($prefix, $interval) = @_;
	my @nodes = make_cluster($prefix, 3, logical => 1, recovery_interval => $interval, extra => $settings);
	$nodes[0]->safe_psql('postgres', q{
		CREATE TABLE public.orders (id int PRIMARY KEY, customer int NOT NULL);
		SELECT kingres.create_distributed_table('public.orders', 'id', bucket_count => 6);
	});
	$nodes[0]->safe_psql('postgres', join('', map { "INSERT INTO public.orders VALUES ($_, $_ % 9);\n" } 1 .. 60));
	return @nodes;
}

sub owner
{
	my ($n1, $b) = @_;
	return $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b");
}

# Start a move in a background psql that stops at a fault point; returns (pid of
# the psql, the flag file).  The move is at that point when "$file.reached" exists.
sub start_stopped_move
{
	my ($n1, $point, $b, $tgt, $name) = @_;
	my $file = "$tmp/wait_$name";
	open(my $fh, '>', $file) or die;
	close($fh);
	my $pid = fork();
	die 'fork' unless defined $pid;
	if ($pid == 0)
	{
		my $sql = "SET kingres_shard.fault_injection = '$point:wait:$file'; SELECT kingres.move_bucket(1, $b, $tgt)";
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$sql\" > $tmp/out_$name 2>&1");
		POSIX::_exit(0);
	}
	return ($pid, $file);
}

sub move_backend_pid
{
	my ($n1) = @_;
	return $n1->safe_psql('postgres', q{SELECT pid FROM pg_stat_activity WHERE query LIKE '%kingres.move_bucket(%' AND pid <> pg_backend_pid() AND backend_type = 'client backend' LIMIT 1});
}

# ---------------------------------------------------------------------------
# The worker is on.
# ---------------------------------------------------------------------------
{
	my @nodes = setup_cluster('mw', 1);
	my ($n1, $n2, $n3) = @nodes;

	# A finished move: the worker drops the orphan tables after the delay.
	my $src = owner($n1, 0);
	my $tgt = ($src + 1) % 3;
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, 0, $tgt)");
	ok($n1->poll_query_until('postgres', q{SELECT phase = 'done' FROM kingres.bucket_move WHERE move_id = 1}),
		'the worker finishes the cleaning of a move after the delay');
	is($nodes[$src]->safe_psql('postgres', q{SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan%'}),
		'0', 'the orphan tables are gone from the source');

	# A move whose session was killed: the worker undoes it, and the bucket can be moved again.
	my $b = 1;
	$src = owner($n1, $b);
	$tgt = ($src + 1) % 3;
	my ($pid, $file) = start_stopped_move($n1, 'move_after_subscription', $b, $tgt, 'kill');
	ok(wait_for_fault_file("$file.reached", 60), 'the move to be killed reached the copy');
	my $bpid = move_backend_pid($n1);
	$n1->safe_psql('postgres', "SELECT pg_terminate_backend($bpid)");
	unlink($file);
	waitpid($pid, 0);
	ok($n1->poll_query_until('postgres', q{SELECT phase = 'failed' FROM kingres.bucket_move WHERE move_id = 2}),
		'the worker undoes a move whose session died');
	is($n1->safe_psql('postgres', "SELECT state FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"),
		'active', 'the bucket is active again');
	for my $node (@nodes)
	{
		is($node->safe_psql('postgres', q{SELECT (SELECT count(*) FROM pg_replication_slots) || ',' || (SELECT count(*) FROM pg_subscription)
			|| ',' || (SELECT count(*) FROM pg_publication)}), '0,0,0', 'nothing of the replication is left on ' . $node->name);
	}
	is($n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, $tgt) > 0"), 't', 'the bucket can be moved again');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.orders'), '60', 'every row is there once');

	# A move that is alive is not touched, however many passes the worker makes.
	$b = 2;
	$src = owner($n1, $b);
	$tgt = ($src + 1) % 3;
	($pid, $file) = start_stopped_move($n1, 'move_after_catchup', $b, $tgt, 'live');
	ok(wait_for_fault_file("$file.reached", 60), 'the live move reached the end of the catch-up');
	# The node timeout is 3 s and the worker runs every second: ten seconds are several passes
	# (the check is that nothing happens, so no event can be waited for).
	sleep(10);
	is($n1->safe_psql('postgres', 'SELECT phase FROM kingres.bucket_move WHERE move_id = 4'),
		'catching_up', 'the worker leaves a live move alone');
	unlink($file);
	waitpid($pid, 0);
	ok($n1->poll_query_until('postgres', q{SELECT phase IN ('cleaning', 'done') FROM kingres.bucket_move WHERE move_id = 4}),
		'the live move completed');
	ok($n1->poll_query_until('postgres', q{SELECT phase = 'done' FROM kingres.bucket_move WHERE move_id = 4}),
		'and the worker cleaned it up');
	is($n1->safe_psql('postgres', q{SELECT coalesce(string_agg(check_name || ' ' || node_id, ','), '') FROM kingres.check_cluster()}),
		'', 'check_cluster finds nothing');
}

# ---------------------------------------------------------------------------
# The worker is off: check_cluster tells what it would have done.
# ---------------------------------------------------------------------------
{
	my @nodes = setup_cluster('mc', 0);
	my ($n1) = @nodes;

	sub problems
	{
		my ($timeout) = @_;
		$timeout //= 3;
		return $n1->safe_psql('postgres', "SET kingres_shard.node_timeout = $timeout;
" . q{SELECT coalesce(string_agg(check_name || ':' || node_id, ' ' ORDER BY check_name, node_id), '') FROM kingres.check_cluster()});
	}

	# A killed session: the move is interrupted, nobody runs it.
	my $b = 1;
	my $src = owner($n1, $b);
	my $tgt = ($src + 1) % 3;
	my ($pid, $file) = start_stopped_move($n1, 'move_after_subscription', $b, $tgt, 'stalled');
	ok(wait_for_fault_file("$file.reached", 60), 'the move to be killed reached the copy');
	is(problems(), '', 'a move that is running is not a problem');
	my $bpid = move_backend_pid($n1);
	$n1->safe_psql('postgres', "SELECT pg_terminate_backend($bpid)");
	unlink($file);
	waitpid($pid, 0);
	is(problems(3600), '', 'a move that stopped a moment ago is not reported yet');
	$n1->safe_psql('postgres', "UPDATE kingres.bucket_move SET updated_at = updated_at - interval '1 hour' WHERE move_id = 1");
	is(problems(), 'stalled_move:1', 'a move that nobody runs and that was not touched for a long time is reported');
	$n1->safe_psql('postgres', 'SELECT kingres.resume_bucket_moves()');
	is(problems(), '', 'resume_bucket_moves clears it');

	# A finished move whose orphan tables nobody dropped.
	$b = 2;
	$src = owner($n1, $b);
	$tgt = ($src + 1) % 3;
	$n1->safe_psql('postgres', "SET kingres_shard.move_cleanup_delay = '10min'; SELECT kingres.move_bucket(1, $b, $tgt)");
	is(problems(), '', 'orphan tables inside their delay are not a problem');
	$n1->safe_psql('postgres', "UPDATE kingres.bucket_move SET cleanup_after = cleanup_after - interval '1 hour', updated_at = updated_at - interval '1 hour' WHERE move_id = 2");
	is(problems(), 'orphan_table:' . ($src + 1) . ' stalled_move:1', 'overdue orphan tables are reported, with the move that owns them');
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets()');
	is(problems(), '', 'cleanup_moved_buckets clears it');

	# -----------------------------------------------------------------------
	# S4-05: a node whose SQL objects are older than the library.
	# -----------------------------------------------------------------------
	my $n3 = $nodes[2];
	my $moves = $n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.bucket_move');
	$n3->safe_psql('postgres', 'ALTER FUNCTION kingres.internal_placement_hash(int, regclass) RENAME TO internal_placement_hash_away');
	my ($rc, $out, $err) = run_sql($n1, 'SELECT count(*) FROM public.orders');
	isnt($rc, 0, 'a scan fails when another node lacks a helper function');
	like($err, qr/node \S+:\d+ runs an older version of the kingres_shard SQL objects/,
		'the error names the node and the cause');
	like($err, qr/ALTER EXTENSION kingres_shard UPDATE/, 'and the remedy');
	unlike($err, qr/^ERROR:\s+function kingres\.internal_placement_hash/m, 'instead of "function does not exist"');
	$n3->safe_psql('postgres', 'ALTER FUNCTION kingres.internal_placement_hash_away(int, regclass) RENAME TO internal_placement_hash');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.orders'), '60', 'the scan works again');

	$n3->safe_psql('postgres', "UPDATE pg_catalog.pg_extension SET extversion = '0.2' WHERE extname = 'kingres_shard'");
	$b = 3;
	$src = owner($n1, $b);
	$tgt = ($src + 1) % 3;
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket(1, $b, $tgt)");
	isnt($rc, 0, 'a move is refused when a node has another version');
	like($err, qr/node 3 \(\S+:\d+\) has kingres_shard 0\.2, a bucket move needs 0\.6 on every node/,
		'the error says which node and which version');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.bucket_move'), $moves, 'and nothing was recorded');
	is(owner($n1, $b), $src, 'the bucket did not move');
	$n3->safe_psql('postgres', "UPDATE pg_catalog.pg_extension SET extversion = '0.6' WHERE extname = 'kingres_shard'");
	is($n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, $tgt) > 0"), 't', 'with the version restored the move runs');

	# -----------------------------------------------------------------------
	# S4-06 and S4-07: who may call the helpers and read the move records.
	# -----------------------------------------------------------------------
	$_->safe_psql('postgres', 'CREATE ROLE eve LOGIN') for @nodes;
	my @as_eve = (extra_params => [ '-U', 'eve' ]);
	for my $call ('SELECT kingres.internal_map_version()', 'SELECT kingres.internal_placement_hash(1)',
		"SELECT kingres.internal_placement_hash(1, 'public.orders'::regclass)",
		"SELECT kingres.internal_placement_hash(1, 'pg_class'::regclass)")
	{
		($rc, $out, $err) = run_sql($n1, $call, @as_eve);
		isnt($rc, 0, "an ordinary role cannot call: $call");
		like($err, qr/permission denied for the internal functions/, '... and is told so');
	}
	$n1->safe_psql('postgres', 'GRANT SELECT ON public.orders TO eve');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.internal_placement_hash(1, 'public.orders'::regclass) IS NOT NULL", @as_eve);
	is($out, 't', 'a role that may read the table may ask for its placement');
	($rc, $out, $err) = run_sql($n1, 'SELECT count(*) FROM public.orders', @as_eve);
	is($out, '60', 'a scan by that role works (its sessions on the other nodes may call the helpers)');
	($rc, $out, $err) = run_sql($n1, 'SELECT count(*) FROM kingres.bucket_move', @as_eve);
	isnt($rc, 0, 'an ordinary role cannot read the move records');
	like($err, qr/permission denied for table bucket_move/, '... and is told so');
	($rc, $out, $err) = run_sql($n1, 'SELECT count(*), min(phase) FROM kingres.bucket_move_status', @as_eve);
	is($rc, 0, 'it can read the progress of the moves');
	like($out, qr/^\d+\|/, '... through the view');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM information_schema.columns WHERE table_schema = 'kingres'
		AND table_name = 'bucket_move_status' AND column_name IN ('detail', 'slot_name', 'sub_oid')}), '0',
		'the view shows no error text, slot or subscription');
}

done_testing();
