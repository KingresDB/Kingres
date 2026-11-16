# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/074_move_tablesync_hold.pl
#
# The question left by the review of 0.5 (S8, "Not reproduced"): the hang of
# t/028_move_faults.pl on PostgreSQL 19 was thought to come from a table-sync
# worker of a restarted target that needs the slot which the cleanup of a move drops
# by name.  The worker is held, deterministically, at the point in question: the
# tables of the bucket on the target are locked SHARE after the slot of the move
# exists, so CREATE SUBSCRIPTION (which only reads them) goes through, and the table-sync
# worker of the target creates its slot on the source and then waits for ROW EXCLUSIVE in
# its copy.  Then the move gives up and is undone while the worker waits
# (cleanup_while_held), or the target is restarted first so that the worker starts
# again (target_restarted).  Nothing may be left behind either way: no slot (the sync
# slots included), subscription, publication, origin or marker.
#
# This file is made from t/028_move_faults.pl: the helpers are the same.


use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use Time::HiRes qw(sleep);
use KingresShardTest qw(make_node make_cluster run_sql move_state wait_for_fault_file start_psql finish_psql);

my @nodes = make_cluster('th', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();

$n1->safe_psql('postgres', q{
	CREATE TABLE public.orders (id int PRIMARY KEY, customer int NOT NULL);
	CREATE TABLE public.items (id int NOT NULL, order_id int NOT NULL, PRIMARY KEY (id, order_id));
	SELECT kingres.create_distributed_table('public.orders', 'id', bucket_count => 6);
	SELECT kingres.create_distributed_table('public.items', 'order_id', colocate_with => 'public.orders');
	ALTER TABLE public.items ADD CONSTRAINT items_order_fk FOREIGN KEY (order_id) REFERENCES public.orders (id);
});
{
	my $sql = '';
	$sql .= "INSERT INTO public.orders VALUES ($_, $_ % 9);\n" for 1 .. 60;
	$sql .= "INSERT INTO public.items VALUES ($_, $_);\n" for 1 .. 60;
	$n1->safe_psql('postgres', $sql);
}
my $want = '60,1830,60,1830';

sub totals
{
	my ($node) = @_;
	return $node->safe_psql('postgres', q{
		SELECT (SELECT count(*) FROM public.orders) || ',' || (SELECT sum(id) FROM public.orders)
		       || ',' || (SELECT count(*) FROM public.items) || ',' || (SELECT sum(order_id) FROM public.items)});
}

sub restart
{
	my ($node) = @_;
	$node->stop('immediate', fail_ok => 1);
	$node->start;
}

sub resume_all
{
	my $n = 0;
	for (1 .. 7)
	{
		my $r = $n1->safe_psql('postgres', "SET kingres_shard.node_timeout = 3;\nSELECT kingres.resume_bucket_moves()");
		$n += $r;
		last if $n1->safe_psql('postgres', "SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('preparing','copying','catching_up','switching')") eq '0';
		sleep(2);
	}
	return $n;
}

# Everything that a finished or undone move must have left: the bucket whole on
# exactly one group, nothing of the replication anywhere, a clean cluster.
sub assert_clean
{
	my ($label) = @_;
	$n1->safe_psql('postgres', "SELECT kingres.cleanup_moved_buckets(true)");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('preparing','copying','catching_up','switching')"),
		'0', "$label: no move is unfinished");
	for my $node (@nodes)
	{
		my $n = $node->name;
		diag($n . ' slots: ' . $node->safe_psql('postgres', q{SELECT coalesce(string_agg(slot_name || ' ' || active, ', '), '') FROM pg_replication_slots})) if $node->safe_psql('postgres', 'SELECT count(*) FROM pg_replication_slots') ne '0';
		is($node->safe_psql('postgres', q{SELECT (SELECT count(*) FROM pg_replication_slots) || ',' || (SELECT count(*) FROM pg_subscription)
			|| ',' || (SELECT count(*) FROM pg_publication) || ',' || (SELECT count(*) FROM pg_replication_origin)
			|| ',' || (SELECT count(*) FROM pg_prepared_xacts) || ',' || (SELECT count(*) FROM kingres.move_marker)}),
			'0,0,0,0,0,0', "$label: no slot, subscription, publication, origin, prepared transaction or marker on $n");
		is(totals($node), $want, "$label: every row exactly once from $n");
		is($node->safe_psql('postgres', q{SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan%'}),
			'0', "$label: no orphan table on $n");
	}
	is($n1->safe_psql('postgres', q{SELECT coalesce(string_agg(check_name || ' ' || node_id || ' ' || object || ' ' || detail, E'\n'), '') FROM kingres.check_cluster()}),
		'', "$label: check_cluster finds nothing");
	# the bucket is attached on exactly one node
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.bucket_map WHERE state <> 'active'}), '0', "$label: no bucket is left moving");
}

sub owner { my ($b) = @_; return $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"); }

# A move of a bucket with a fault; returns the result of the session.
sub move_with
{
	my ($fault, $bucket, $target) = @_;
	return run_sql($n1, "SET kingres_shard.fault_injection = '$fault';\nSET kingres_shard.node_timeout = 5;\nSELECT kingres.move_bucket(1, $bucket, $target)");
}


# A move that is parked after its slot was made; returns the child pid.
sub park_move
{
	my ($b, $tgt, $file) = @_;
	open(my $fh, '>', $file) or die; close($fh);
	my $pid = fork();
	die "fork" unless defined $pid;
	if ($pid == 0)
	{
		my $sql = "SET kingres_shard.fault_injection = 'move_after_slot:wait:$file'; SET kingres_shard.node_timeout = 5; SELECT kingres.move_bucket(1, $b, $tgt)";
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$sql\" > $file.out 2>&1");
		POSIX::_exit(0);
	}
	return $pid;
}

# Hold the table-sync worker of the target: the copy of a table needs ROW EXCLUSIVE on the
# table of the bucket that the move made on the target; a SHARE lock on those tables stops it
# there, after its slot was made on the source, and lets CREATE SUBSCRIPTION (which only reads
# the tables) through.  The returned handle ends with release_target().
sub hold_target
{
	my ($tgt, $b) = @_;
	my $parts = $nodes[$tgt]->safe_psql('postgres',
		"SELECT string_agg(format('public.%I', kingres.internal_bucket_part_name(t, $b)), ', ') FROM unnest(ARRAY['orders', 'items']) AS t");
	my $names = $nodes[$tgt]->safe_psql('postgres',
		"SELECT string_agg(quote_literal(kingres.internal_bucket_part_name(t, $b)), ', ') FROM unnest(ARRAY['orders', 'items']) AS t");
	my $h = start_psql($nodes[$tgt], "BEGIN; LOCK TABLE $parts IN SHARE MODE; SELECT pg_sleep(120); COMMIT;");
	ok($nodes[$tgt]->poll_query_until('postgres',
			"SELECT count(*) = 2 FROM pg_locks l JOIN pg_class c ON c.oid = l.relation "
			  . "WHERE l.mode = 'ShareLock' AND l.granted AND c.relname IN ($names)"),
		'the tables of the bucket are locked on the target');
	return $h;
}

sub release_target
{
	my ($tgt, $h) = @_;
	$nodes[$tgt]->safe_psql('postgres',
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE '%pg_sleep(120)%' AND pid <> pg_backend_pid()");
	finish_psql($h);
}

# Wait until a worker of the target waits for the lock of one of the bucket's tables.  This is the
# held state on every major: PostgreSQL 18 makes the slot of the table before it asks for the lock,
# PostgreSQL 19 asks first, so the slot is no marker.
sub wait_for_waiter
{
	my ($tgt, $b) = @_;
	my $names = $nodes[$tgt]->safe_psql('postgres',
		"SELECT string_agg(quote_literal(kingres.internal_bucket_part_name(t, $b)), ', ') FROM unnest(ARRAY['orders', 'items']) AS t");
	return $nodes[$tgt]->poll_query_until('postgres',
		"SELECT count(*) >= 1 FROM pg_locks l JOIN pg_class c ON c.oid = l.relation "
		  . "WHERE NOT l.granted AND c.relname IN ($names)");
}

my $bucket;
for my $variant ('cleanup_while_held', 'target_restarted', 'slot_lost', 'target_down')
{
	$bucket = (($bucket // 0) + 1) % 6;
	my $src = owner($bucket);
	my $tgt = ($src + 1) % 3;
	my $file = "$tmp/hold_$variant";
	my $pid = park_move($bucket, $tgt, $file);
	ok(wait_for_fault_file("$file.reached", 60), "[$variant] the move made its slot");
	# The move made its tables on the target before it made the slot.
	my $h = hold_target($tgt, $bucket);
	unlink($file);
	ok(wait_for_waiter($tgt, $bucket), "[$variant] a worker of the target waits for the lock of the bucket's tables");
	if ($variant eq 'target_restarted')
	{
		# The lock goes with the restart; the worker starts again while the move gives up.
		restart($nodes[$tgt]);
	}
	if ($variant eq 'slot_lost')
	{
		# The state that a crash of the target leaves between the worker's drop of its slot and
		# the record that it is done (seen on 19): the table is "finished copy" and its slot is gone.
		# Made without stopping the target, so that the move cannot meet a node that is down: the
		# subscription is disabled (its workers end, nothing counts an error from then on), the error
		# counters are reset, the state is forced and the slot is dropped.  Only then can the move's
		# check be the one that ends the move, on every major.
		my $rel = $nodes[$tgt]->safe_psql('postgres',
			"SELECT srrelid FROM pg_subscription_rel r JOIN pg_class c ON c.oid = r.srrelid WHERE c.relname LIKE 'orders_b%'");
		my $sub = $nodes[$tgt]->safe_psql('postgres', 'SELECT subname FROM pg_subscription');
		$nodes[$tgt]->safe_psql('postgres', "ALTER SUBSCRIPTION $sub DISABLE");
		# No worker of the subscription is left, and none can start: nothing holds or makes a slot from now on.
		ok($nodes[$tgt]->poll_query_until('postgres', 'SELECT count(*) = 0 FROM pg_stat_subscription WHERE pid IS NOT NULL'),
			"[$variant] the workers of the subscription are gone");
		ok($nodes[$src]->poll_query_until('postgres',
				"SELECT count(*) = 0 FROM pg_replication_slots WHERE slot_name ~ '^pg_[0-9]+_sync_' AND active"),
			"[$variant] and the source has no active table-sync slot");
		for (1 .. 50)
		{
			$nodes[$src]->psql('postgres',
				"SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name ~ '^pg_[0-9]+_sync_${rel}_' AND NOT active");
			last if $nodes[$src]->safe_psql('postgres', "SELECT count(*) FROM pg_replication_slots WHERE slot_name ~ '^pg_[0-9]+_sync_${rel}_'") eq '0';
			sleep(0.2);
		}
		is($nodes[$src]->safe_psql('postgres', "SELECT count(*) FROM pg_replication_slots WHERE slot_name ~ '^pg_[0-9]+_sync_${rel}_'"),
			'0', "[$variant] the slot of the table is gone from the source");
		$nodes[$tgt]->safe_psql('postgres', "UPDATE pg_subscription_rel SET srsubstate = 'f' WHERE srrelid = $rel");
		$nodes[$tgt]->safe_psql('postgres', "SELECT pg_stat_reset_subscription_stats(oid) FROM pg_subscription");
		is($nodes[$tgt]->safe_psql('postgres', q{SELECT (kingres.internal_move_subscription_status(subname)->>'errors') FROM pg_subscription}),
			'0', "[$variant] no error is counted: the check of the move is the only thing that can end it");
		# The lock is of no use any more (the subscription is disabled) and the undo of the move needs the tables.
		release_target($tgt, $h);
		waitpid($pid, 0);
		like(slurp_file("$file.out"), qr/lost its replication slot/, "[$variant] the move fails by itself because the slot is gone");
		resume_all();
		ok(owner($bucket) == $src, "[$variant] the bucket stayed where it was");
		assert_clean("[$variant] sync slot lost");
		next;
	}
	if ($variant eq 'target_down')
	{
		# The target is down while the move looks at it: the move ends with the connection error and
		# says that it could not undo itself now; resume_bucket_moves() finishes it once the node is back.
		# The move can neither finish nor miss the stop: its tables are locked and the node is down
		# until the move has ended.
		$nodes[$tgt]->stop('immediate', fail_ok => 1);
		waitpid($pid, 0);
		my $out = slurp_file("$file.out");
		like($out, qr/could not connect to node/, "[$variant] the move fails with the connection error");
		like($out, qr/resume_bucket_moves\(\)/, "[$variant] and says that resume_bucket_moves() finishes the undo");
		$nodes[$tgt]->start;
		isnt($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('preparing','copying','catching_up','switching')"),
			'0', "[$variant] the move is left unfinished until it is resumed");
		resume_all();
		ok(owner($bucket) == $src, "[$variant] the bucket stayed where it was");
		assert_clean("[$variant] target down");
		next;
	}
	# The move waits for the copy for node_timeout, gives up and undoes itself: its cleanup
	# drops the subscription (and with it the worker, and the sync slot) and the slots of the source.
	if ($variant eq 'cleanup_while_held')
	{
		# The move is cancelled while the worker is held, so that its cleanup runs against a worker
		# that waits for the lock (the case of the hypothesis about t/028).
		$n1->safe_psql('postgres', 'SELECT kingres.cancel_bucket_move(max(move_id)) FROM kingres.bucket_move');
	}
	waitpid($pid, 0);
	release_target($tgt, $h) if $variant eq 'cleanup_while_held';
	resume_all();
	ok(owner($bucket) == $src || owner($bucket) == $tgt, "[$variant] the bucket is whole on one group (" . owner($bucket) . ")");
	assert_clean("[$variant] table-sync worker held");
}

done_testing();
