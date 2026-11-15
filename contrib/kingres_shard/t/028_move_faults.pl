# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Failure and resumption of a bucket move (design/shard-phase4.md, "Failure and
# resumption"): a crash of the map group primary (the session that runs the
# move) at every phase boundary and inside the switch, a crash of the source and
# of the target, a connection that is lost, cancellation, and the preconditions.
# After kingres.resume_bucket_moves() the bucket is whole on exactly one group
# and nothing is left behind: no slot, publication, subscription, origin,
# stray table or prepared transaction, and kingres.check_cluster() is clean.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use Time::HiRes qw(sleep);
use KingresShardTest qw(make_node make_cluster run_sql move_state wait_for_fault_file);

my @nodes = make_cluster('fl', 3, logical => 1);
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

# ---------------------------------------------------------------------------
# The map group primary crashes at each boundary before the switch: undone.
# ---------------------------------------------------------------------------
my $b = 0;
for my $point (qw(move_after_preparing move_after_target_tables move_after_publication move_after_slot
		move_after_subscription move_after_catchup move_before_switch))
{
	$b = ($b + 1) % 6;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my ($rc) = move_with("$point:kill", $b, $tgt);
	isnt($rc, 0, "$point: the session of the move dies");
	restart($n1);
	my $r = resume_all();
	cmp_ok($r, '>=', 1, "$point: resume_bucket_moves resolves the move");
	is(owner($b), $src, "$point: the bucket is back on group $src");
	like($n1->safe_psql('postgres', "SELECT phase FROM kingres.bucket_move ORDER BY move_id DESC LIMIT 1"), qr/^failed$/, "$point: recorded as failed");
	assert_clean($point);
}

# ---------------------------------------------------------------------------
# Inside the switch.  Before the decision (the local commit): undone; after it:
# the switch stands and the move finishes.
# ---------------------------------------------------------------------------
for my $case (['move_switch_after_lock', 0], ['move_switch_after_marker', 0], ['move_switch_after_attach', 0],
	['move_switch_after_detach', 0], ['move_switch_after_local', 0], ['after_prepare', 0, ':1'], ['after_log', 0, ':1'], ['after_commit', 1, ':1'])
{
	my ($point, $switched, $skip) = @$case;
	$skip //= '';
	$b = ($b + 1) % 6;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my ($rc) = move_with("$point:kill$skip", $b, $tgt);
	isnt($rc, 0, "$point: the session of the move dies");
	restart($n1);
	resume_all();
	is(owner($b), $switched ? $tgt : $src, "$point: the bucket is on group " . ($switched ? $tgt : $src));
	assert_clean($point);
}

# ---------------------------------------------------------------------------
# The target or the source dies in the middle of the copy.
# ---------------------------------------------------------------------------
for my $which ('target', 'source')
{
	$b = ($b + 1) % 6;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my $victim = $which eq 'target' ? $nodes[$tgt] : $nodes[$src];
	my $file = "$tmp/wait_$which";
	open(my $fh, '>', $file) or die; close($fh);
	my $pid = fork();
	die "fork" unless defined $pid;
	if ($pid == 0)
	{
		my $sql = "SET kingres_shard.fault_injection = 'move_after_subscription:wait:$file'; SET kingres_shard.node_timeout = 5; SELECT kingres.move_bucket(1, $b, $tgt)";
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$sql\" > $tmp/child_$which.out 2>&1");
		POSIX::_exit(0);
	}
	ok(wait_for_fault_file("$file.reached", 60), "the $which case: the move reached the copy");
	restart($victim) if $which eq 'target';
	$victim->stop('immediate', fail_ok => 1) if $which eq 'source';
	unlink($file);
	waitpid($pid, 0);
	$victim->start if $which eq 'source';
	resume_all();
	ok(owner($b) == $src || owner($b) == $tgt, "the $which case: the bucket is whole on one group (" . owner($b) . ")");
	assert_clean("$which dies during the copy");
}

# ---------------------------------------------------------------------------
# The target dies inside the switch (attach): the switch is rolled back.
# ---------------------------------------------------------------------------
{
	$b = ($b + 1) % 6;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my ($rc, $out, $err) = move_with('move_attach:kill', $b, $tgt);
	isnt($rc, 0, 'a crash of the target inside the attach fails the move');
	$nodes[$tgt]->stop('immediate', fail_ok => 1);
	$nodes[$tgt]->start;
	sleep(7);
	resume_all();
	is(owner($b), $src, 'the bucket stayed where it was');
	assert_clean('the target crashes inside the switch');
}

# ---------------------------------------------------------------------------
# Cancellation.
# ---------------------------------------------------------------------------
{
	$b = ($b + 1) % 6;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my $file = "$tmp/wait_cancel";
	open(my $fh, '>', $file) or die; close($fh);
	my $pid = fork();
	die "fork" unless defined $pid;
	if ($pid == 0)
	{
		my $sql = "SET kingres_shard.fault_injection = 'move_after_subscription:wait:$file'; SELECT kingres.move_bucket(1, $b, $tgt)";
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$sql\" > $tmp/child_cancel.out 2>&1");
		POSIX::_exit(0);
	}
	ok(wait_for_fault_file("$file.reached", 60), 'cancel: the move reached the copy');
	my $id = $n1->safe_psql('postgres', "SELECT max(move_id) FROM kingres.bucket_move");
	# a second move of the same bucket is refused while it runs
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket(1, $b, $tgt)");
	like($err, qr/already being moved/, 'a second move of the same bucket fails');
	$n1->safe_psql('postgres', "SET kingres_shard.node_timeout = 3;
SELECT kingres.cancel_bucket_move($id)");
	unlink($file);
	waitpid($pid, 0);
	like(move_state($n1, $id), qr/^cancelled\|/, 'the move is cancelled');
	is(owner($b), $src, 'cancel: the bucket stayed on its group');
	assert_clean('cancel');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.cancel_bucket_move($id)");
	like($err, qr/already cancelled/, 'a finished move cannot be cancelled again');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.cancel_bucket_move(99999)");
	like($err, qr/does not exist/, 'nor one that does not exist');

	# cancel of a move whose session is gone: cancel_bucket_move undoes it
	$b = ($b + 1) % 6;
	$src = owner($b);
	$tgt = ($src + 1) % 3;
	($rc) = move_with('move_after_catchup:kill', $b, $tgt);
	isnt($rc, 0, 'cancel of a dead move: the session dies');
	restart($n1);
	$id = $n1->safe_psql('postgres', "SELECT max(move_id) FROM kingres.bucket_move");
	$n1->safe_psql('postgres', "SELECT kingres.cancel_bucket_move($id)");
	like(move_state($n1, $id), qr/^cancelled\|/, 'a dead move is undone by cancel_bucket_move');
	assert_clean('cancel of a dead move');
}

# ---------------------------------------------------------------------------
# Preconditions: each fails cleanly and leaves nothing behind.
# ---------------------------------------------------------------------------
{
	my $before = $n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.bucket_move');

	# a table without a replica identity
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.noident (id int, v int);
		SELECT kingres.create_distributed_table('public.noident', 'id', bucket_count => 4);
	});
	my $colo = $n1->safe_psql('postgres', "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.noident'::regclass");
	my $src = $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $colo AND bucket = 0");
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($colo, 0, " . (($src + 1) % 3) . ")");
	like($err, qr/table "public\.noident" has no replica identity/, 'a table without a replica identity is refused, by name');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.bucket_move'), $before, 'and nothing was recorded');
	is($n1->safe_psql('postgres', "SELECT state FROM kingres.bucket_map WHERE colocation_id = $colo AND bucket = 0"), 'active', 'the bucket is untouched');
	# REPLICA IDENTITY is not propagated to the partitions: the administrator sets it on the node that has the bucket
	$nodes[$src]->safe_psql('postgres', "SET kingres_shard.enable_routing = off;
ALTER TABLE public.noident_b0000 REPLICA IDENTITY FULL");
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket($colo, 0, " . (($src + 1) % 3) . ")");
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $colo AND bucket = 0"), (($src + 1) % 3),
		'with REPLICA IDENTITY FULL the same bucket moves');
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');

	# bad arguments
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket(1, 99, 0)");
	like($err, qr/bucket 99 .* does not exist/, 'a bucket that does not exist');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket(77, 0, 0)");
	like($err, qr/colocation group 77 does not exist/, 'a colocation group that does not exist');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket(1, 0, " . owner(0) . ")");
	like($err, qr/already on shard group/, 'a move to the group that has the bucket');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket(1, 0, 42)");
	like($err, qr/no active primary/, 'a group that does not exist');
}

# wal_level not logical on a node
{
	my @three = make_cluster('nl', 3, logical => 1);
	$three[0]->safe_psql('postgres', q{
		CREATE TABLE public.t (id int PRIMARY KEY);
		SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 3);
	});
	$three[2]->append_conf('postgresql.conf', "wal_level = replica\nmax_wal_senders = 0\nmax_replication_slots = 0\n");
	$three[2]->restart;
	my $g = $three[0]->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = 0");
	my $tg = $g == 2 ? 1 : 2;
	my ($rc, $out, $err) = run_sql($three[0], "SELECT kingres.move_bucket(1, 0, 2)");
	like($err, qr/wal_level|max_logical_replication_workers|max_replication/, 'a node without logical WAL is refused by the setting');
	$three[2]->safe_psql('postgres', 'SELECT 1');
	is($three[0]->safe_psql('postgres', 'SELECT count(*) FROM kingres.bucket_move'), '0', 'nothing recorded');
	is($three[0]->safe_psql('postgres', "SELECT state FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = 0"), 'active', 'the bucket is untouched');
}

done_testing();
