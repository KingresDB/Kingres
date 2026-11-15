# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# A write straight into a bucket partition and a bucket move (review finding
# S4-03).  The switch of a move proves that the copy has everything with a marker
# row that is committed on the source after its locks are granted: the marker is
# replicated after every transaction that wrote the bucket before it.  A write
# that goes into the partition itself (INSERT INTO orders_b0002 ...) takes no
# lock on the parent, so locking the parents does not wait for it; the switch
# therefore also locks the partitions of the bucket, and the extension refuses
# the write in the first place.
#
# 1. The planner refuses a write that names a bucket partition (the defence in
#    depth), for a superuser as well; with routing switched off, the way an
#    administrator repeats a statement on every node by hand, it is allowed, which
#    is what the next part needs.
# 2. A transaction that has written a partition directly and is still open when
#    the switch starts keeps the switch out until it ends; when the replication is
#    behind (here: the subscription is disabled while the transaction commits),
#    the row is still on the target after the move, never only in the orphan table
#    of the old group.  Without the lock of the partition the marker reaches the
#    target while the transaction is open, the detach waits for it, the switch
#    commits and drops the subscription before the row was replayed: an
#    acknowledged row is lost.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use Time::HiRes qw(sleep time);
use KingresShardTest qw(make_cluster run_sql wait_for_fault_file);

my @nodes = make_cluster('dw', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();

$n1->safe_psql('postgres', q{
	CREATE TABLE public.orders (id int PRIMARY KEY, customer int NOT NULL);
	SELECT kingres.create_distributed_table('public.orders', 'id', bucket_count => 6);
});
{
	my $sql = '';
	$sql .= "INSERT INTO public.orders VALUES ($_, $_ % 9);\n" for 1 .. 60;
	$n1->safe_psql('postgres', $sql);
}

# A bucket of group 1 (node 2), the partition that holds it there, and a key that
# is not in the table and belongs to that bucket.
my $bucket = $n1->safe_psql('postgres',
	q{SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = 1});
my $part = sprintf('public.orders_b%04d', $bucket);
my $newkey = $n1->safe_psql('postgres',
	"SELECT min(k) FROM generate_series(1000, 3000) k WHERE kingres.bucket_for(1, k) = $bucket");

# ---------------------------------------------------------------------------
# 1. The refusal.
# ---------------------------------------------------------------------------
for my $case (
	[ 'INSERT', "INSERT INTO $part VALUES ($newkey, 1)" ],
	[ 'UPDATE', "UPDATE $part SET customer = 1" ],
	[ 'DELETE', "DELETE FROM $part" ],
	[ 'a data-modifying WITH', "WITH x AS (DELETE FROM $part RETURNING id) SELECT count(*) FROM x" ],
  )
{
	my ($rc, $out, $err) = run_sql($n2, $case->[1]);
	like($err, qr/cannot write a bucket partition of a distributed table directly/,
		"$case->[0] into a bucket partition is refused (as a superuser)");
}
{
	my ($rc, $out, $err) = run_sql($n2, "SELECT count(*) FROM $part");
	is($rc, 0, 'a read of a bucket partition is not refused');
	my ($rc2, $out2, $err2) = run_sql($n2,
		"SET kingres_shard.enable_routing = off; INSERT INTO $part VALUES ($newkey, 1); DELETE FROM $part WHERE id = $newkey");
	is($rc2, 0, 'with kingres_shard.enable_routing = off (a superuser repeating a statement by hand) the write is allowed');
}
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.orders'), '60', 'the table is as it was');

# ---------------------------------------------------------------------------
# 2. A direct writer that is open when the switch starts.
# ---------------------------------------------------------------------------
my $seq = 0;

# psql in a process of its own: { pid, out, err }.
sub spawn
{
	my ($node, $sql) = @_;
	my $id = ++$seq;
	my $script = "$tmp/p$id.sql";
	open(my $fh, '>', $script) or die;
	print $fh $sql;
	close($fh);
	my $pid = fork();
	die "fork" unless defined $pid;
	if ($pid == 0)
	{
		open(STDOUT, '>', "$tmp/p$id.out") or POSIX::_exit(2);
		open(STDERR, '>', "$tmp/p$id.err") or POSIX::_exit(2);
		exec('psql', '-X', '-q', '-At', '-h', $node->host, '-p', $node->port, '-d', 'postgres', '-f', $script)
		  or POSIX::_exit(2);
	}
	return { pid => $pid, id => $id };
}

sub finish
{
	my ($h, $timeout) = @_;
	my $end = time() + ($timeout // 120);
	while (1)
	{
		my $r = waitpid($h->{pid}, POSIX::WNOHANG());
		last if $r == $h->{pid} || $r < 0;
		if (time() > $end)
		{
			kill('KILL', $h->{pid});
			waitpid($h->{pid}, 0);
			$h->{timeout} = 1;
			last;
		}
		sleep(0.05);
	}
	for my $k (qw(out err))
	{
		my $s = '';
		if (open(my $fh, '<', "$tmp/p$h->{id}.$k")) { local $/; $s = <$fh>; close($fh); }
		$h->{$k} = $s;
	}
}

my $flag = "$tmp/after_catchup";
open(my $fh, '>', $flag) or die;
close($fh);

# The move stops after the copy is complete and before the switch starts: the
# writer must begin after that (the slot of the move waits for the transactions
# that are running when it is made).
my $mover = spawn($n1,
	"SET kingres_shard.fault_injection = 'move_after_catchup:wait:$flag';\n"
	  . "SET kingres_shard.move_switch_timeout = '20s';\n"
	  . "SELECT kingres.move_bucket(1, $bucket, 2);\n");
ok(wait_for_fault_file("$flag.reached", 120), 'the copy of the bucket is complete');

# The writer: routing off (the refusal of part 1 is what an ordinary session
# meets), a transaction that has written the partition and stays open.
my $writer = $n2->background_psql('postgres');
$writer->query_safe('SET kingres_shard.enable_routing = off');
$writer->query_safe("BEGIN; INSERT INTO $part VALUES ($newkey, 777)");
my $writer_pid = $writer->query_safe('SELECT pg_backend_pid()');
unlink($flag);

# Wait until something tells where the switch is: either a request for the lock
# of the partition is queued behind the writer (the way the first version of the
# switch went: it had locked the parents, which the direct writer does not hold,
# and the marker was committed while the writer was open), or an attempt has been
# abandoned because of the writer and says so.
my $where = '';
my $gave_up = '';
for (1 .. 1200)
{
	my $queued = $n2->safe_psql('postgres',
		"SELECT count(*) FROM pg_locks WHERE NOT granted AND locktype = 'relation' AND relation IN (SELECT c.oid FROM pg_class c WHERE c.relname LIKE 'orders%')");
	if ($queued > 0)
	{
		$where = 'queued';
		last;
	}
	my $detail = $n1->safe_psql('postgres',
		q{SELECT coalesce(detail, '') FROM kingres.bucket_move ORDER BY move_id DESC LIMIT 1});
	if ($detail =~ /^switch attempt \d+ gave up/)
	{
		$where = 'gave_up';
		$gave_up = $detail;
		last;
	}
	sleep(0.05);
}
isnt($where, '', 'the move reached the writer');
is($where, 'gave_up', 'the switch does not queue behind the open direct writer: it abandons the attempt (the first version queued for the detach, after it had proved the replay)');
like($gave_up, qr/pid \Q$writer_pid\E\b/, 'the abandoned attempt names the session that is in the way')
  if $where eq 'gave_up';

# The replication falls behind: the subscription of the move is disabled while
# the writer commits (the same sequence whichever way the switch went).
my $sub = $n3->safe_psql('postgres', q{SELECT subname FROM pg_subscription LIMIT 1});
$n3->safe_psql('postgres', "ALTER SUBSCRIPTION $sub DISABLE");
$writer->query_safe('COMMIT');
$writer->quit;

# The replication stays behind until the switch has either finished without it
# (the first version of the switch dropped the subscription at once) or is waiting
# for the marker of its next attempt, which the subscription has to replay.
for (1 .. 1200)
{
	my $dropped = $n1->safe_psql('postgres',
		q{SELECT replication_dropped FROM kingres.bucket_move ORDER BY move_id DESC LIMIT 1});
	my $on_source = $n2->safe_psql('postgres', 'SELECT count(*) FROM kingres.move_marker');
	my $on_target = $n3->safe_psql('postgres', 'SELECT count(*) FROM kingres.move_marker');
	last if $dropped eq 't' || ($on_source > 0 && $on_target == 0);
	sleep(0.05);
}
# The apply worker catches up (when the move still has its subscription).
run_sql($n3, "ALTER SUBSCRIPTION $sub ENABLE");

finish($mover);
is($mover->{err}, '', 'the move completed') or diag($mover->{err});

for my $n (@nodes)
{
	is($n->safe_psql('postgres', "SELECT count(*) FROM public.orders WHERE id = $newkey"), '1',
		'the row written straight into the partition is in the table, seen from ' . $n->name);
}
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.orders'), '61', 'every other row is there exactly once');
is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket"),
	'2', 'the bucket is on the target');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
is($n1->safe_psql('postgres', q{SELECT coalesce(string_agg(check_name || ' ' || node_id || ' ' || object, ', '), '') FROM kingres.check_cluster()}),
	'', 'check_cluster finds nothing');


# ---------------------------------------------------------------------------
# 3. A direct writer that arrives while the switch holds the partition: it waits
#    for the lock and, when it gets it, the partition is gone (renamed away with
#    the detach): its statement fails and nothing is acknowledged into the orphan.
# ---------------------------------------------------------------------------
{
	my $bucket2 = $n1->safe_psql('postgres',
		q{SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = 1});
	my $part2 = sprintf('public.orders_b%04d', $bucket2);
	my $key2 = $n1->safe_psql('postgres',
		"SELECT min(k) FROM generate_series(4000, 6000) k WHERE kingres.bucket_for(1, k) = $bucket2");
	my $hold = "$tmp/after_lock";
	open(my $hf, '>', $hold) or die;
	close($hf);
	my $mover2 = spawn($n1,
		"SET kingres_shard.fault_injection = 'move_switch_after_lock:wait:$hold';\n"
		  . "SELECT kingres.move_bucket(1, $bucket2, 2);\n");
	ok(wait_for_fault_file("$hold.reached", 120), 'the second move holds the locks of its switch');
	my $late = spawn($n2, "SET kingres_shard.enable_routing = off;\nINSERT INTO $part2 VALUES ($key2, 5);\n");
	ok($n2->poll_query_until('postgres',
			"SELECT count(*) > 0 FROM pg_locks WHERE NOT granted AND locktype = 'relation' AND relation IN (SELECT c.oid FROM pg_class c WHERE c.relname LIKE 'orders%')"),
		'the direct writer waits for the lock of the partition');
	unlink($hold);
	finish($mover2);
	finish($late);
	is($mover2->{err}, '', 'the second move completed');
	like($late->{err}, qr/does not exist/, 'the late direct writer failed: the partition was moved away under its name');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.orders WHERE id = $key2"), '0',
		'and no row was acknowledged into the orphan table');
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
}

done_testing();
