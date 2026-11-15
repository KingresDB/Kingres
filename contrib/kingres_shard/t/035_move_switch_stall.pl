# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# A switch that cannot get its locks must not stall the application and must not
# try for ever (review finding S4-04).  The switch used to queue for its locks
# with a lock timeout: while a request was queued every later statement that
# needed a conflicting lock on the same table queued behind it (writers behind
# the SHARE request, every new reader behind the ACCESS EXCLUSIVE request of the
# detach), a long transaction on any bucket of the group's tables made each
# attempt do so, and the attempts never ended.  Now every lock is tried without
# queueing (LOCK ... NOWAIT, repeated for a short time, after a probe that holds
# nothing), an attempt that fails leaves nothing held, and the move gives up after
# kingres_shard.move_switch_attempts attempts with the sessions in the way named.
#
# Two long transactions on the source node, each of another bucket than the one
# that moves: a writer (it holds ROW EXCLUSIVE on the parent of the table) and a
# reader (ACCESS SHARE).  While the move tries and fails, the lock table of the
# source never holds a waiting lock request on the table (sampled all the time the
# move runs), the move fails after its attempts and its record names the session,
# nothing of it is left, and once the transaction has ended the same move completes.

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

my @nodes = make_cluster('st', 3, logical => 1);
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

# Two buckets of group 1: the one that moves, and another one for the long writer.
my ($bucket, $other) = split /,/, $n1->safe_psql('postgres',
	q{SELECT string_agg(bucket::text, ',' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = 1});
my $otherkey = $n1->safe_psql('postgres',
	"SELECT min(k) FROM generate_series(1000, 3000) k WHERE kingres.bucket_for(1, k) = $other");

my $seq = 0;

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
	return { pid => $pid, id => $id, done => 0 };
}

sub finished
{
	my ($h) = @_;
	return 1 if $h->{done};
	my $r = waitpid($h->{pid}, POSIX::WNOHANG());
	return 0 if $r == 0;
	$h->{done} = 1;
	for my $k (qw(out err))
	{
		my $s = '';
		if (open(my $fh, '<', "$tmp/p$h->{id}.$k")) { local $/; $s = <$fh>; close($fh); }
		$h->{$k} = $s;
	}
	return 1;
}

# Run the move (switch timeout 1 s, three attempts) in a process of its own and,
# for as long as it runs, count the samples of the lock table of node 2 that show
# a lock request waiting for a lock on the table (granted = false).  Returns
# (process, samples, waiting samples).
sub move_and_watch
{
	my ($attempts, $start_holder) = @_;
	my $flag = "$tmp/after_catchup" . (++$seq);
	open(my $fh, '>', $flag) or die;
	close($fh);
	my $m = spawn($n1,
		"SET kingres_shard.fault_injection = 'move_after_catchup:wait:$flag';\n"
		  . "SET kingres_shard.move_switch_timeout = '1s';\n"
		  . "SET kingres_shard.move_switch_attempts = $attempts;\n"
		  . "SELECT kingres.move_bucket(1, $bucket, 2);\n");
	# The holder begins after the copy is complete: the slot of the move waits for
	# the transactions that are running when it is made.
	die 'the copy did not complete' unless wait_for_fault_file("$flag.reached", 120);
	my $holder = $start_holder->();
	unlink($flag);
	my ($samples, $waiting) = (0, 0);
	my $watch = $n2->background_psql('postgres');
	my $end = time() + 120;
	until (finished($m))
	{
		if (time() > $end)
		{
			# A move that never ends is the defect this test is about.
			kill('KILL', $m->{pid});
			waitpid($m->{pid}, 0);
			$m->{done} = 1;
			$m->{err} = 'the move was still trying after 120 seconds';
			last;
		}
		my $w = $watch->query_safe(q{SELECT count(*) FROM pg_locks WHERE NOT granted AND locktype = 'relation'
			AND relation IN (SELECT c.oid FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relname LIKE 'orders%')});
		$samples++;
		$waiting += $w;
		sleep(0.02);
	}
	$watch->quit;
	return ($m, $samples, $waiting, $holder);
}

sub nothing_left
{
	my ($label) = @_;
	for my $n (@nodes)
	{
		is($n->safe_psql('postgres', q{SELECT (SELECT count(*) FROM pg_replication_slots) || ',' || (SELECT count(*) FROM pg_subscription)
			|| ',' || (SELECT count(*) FROM pg_publication) || ',' || (SELECT count(*) FROM pg_prepared_xacts)}),
			'0,0,0,0', "$label: no slot, subscription, publication or prepared transaction on " . $n->name);
	}
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.bucket_map WHERE state <> 'active'}), '0',
		"$label: no bucket is left moving");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.orders'), '60', "$label: the table is whole");
}

my @holders = (
	{
		name => 'a long writer of another bucket',
		begin => "BEGIN; INSERT INTO public.orders VALUES ($otherkey, 1)",
	},
	{
		name => 'a long reader',
		begin => "BEGIN; SELECT count(*) FROM public.orders",
	},
);

for my $h (@holders)
{
	my $pid;
	my ($m, $samples, $waiting, $session) = move_and_watch(3, sub {
		my $s = $n2->background_psql('postgres');
		$s->query_safe($h->{begin});
		$pid = $s->query_safe('SELECT pg_backend_pid()');
		return $s;
	});
	cmp_ok($samples, '>', 20, "$h->{name}: the lock table of the source was sampled while the move tried");
	is($waiting, 0, "$h->{name}: no lock request of the move was ever left waiting on the tables of the source");
	like($m->{err}, qr/gave up after 3 attempts/, "$h->{name}: the move gave up after its attempts");
	like($m->{err}, qr/pid \Q$pid\E\b/, "$h->{name}: and names the session that is in the way");
	is($n1->safe_psql('postgres', q{SELECT phase FROM kingres.bucket_move ORDER BY move_id DESC LIMIT 1}),
		'failed', "$h->{name}: the move is failed, not left running");

	$session->query_safe('ROLLBACK');
	$session->quit;
	nothing_left($h->{name});

	# Once the transaction has ended the same move completes.
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $bucket, 2)");
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket"),
		'2', "$h->{name}: after the transaction ended the move completed");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
	# And back, for the next case.
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $bucket, 1)");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
	is($n1->safe_psql('postgres', q{SELECT coalesce(string_agg(check_name || ' ' || node_id || ' ' || object, ', '), '') FROM kingres.check_cluster()}),
		'', "$h->{name}: check_cluster finds nothing");
}

done_testing();
