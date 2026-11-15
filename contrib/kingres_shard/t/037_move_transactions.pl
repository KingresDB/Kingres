# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Transactions that an application keeps open while a bucket moves (the cases the
# review of 0.3 did not cover): COPY in both directions, INSERT ... SELECT, a user
# two-phase transaction, long REPEATABLE READ and SERIALIZABLE transactions and
# a cursor, each held across the switch.  A transaction that holds the tables of
# the group on the source holds the switch back until it ends (the move records
# that an attempt gave up and goes on); one that does not (a reader on the
# target) sees the same rows before and after.  In every case the table has all
# its rows exactly once at the end.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use Time::HiRes qw(sleep);
use KingresShardTest qw(make_cluster run_sql wait_for_fault_file);

my @nodes = make_cluster('mt', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();

$n1->safe_psql('postgres', q{
	CREATE TABLE public.orders (id int PRIMARY KEY, customer int NOT NULL);
	SELECT kingres.create_distributed_table('public.orders', 'id', bucket_count => 6);
});
$_->safe_psql('postgres', 'CREATE TABLE public.archive (id int, customer int)') for @nodes;
$n1->safe_psql('postgres', join('', map { "INSERT INTO public.orders VALUES ($_, $_ % 9);\n" } 1 .. 60));

sub owner
{
	my ($b) = @_;
	return $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b");
}

sub key_in_bucket
{
	my ($b, $from) = @_;
	return $n1->safe_psql('postgres', "SELECT g FROM generate_series($from, $from + 2000) g WHERE kingres.bucket_for(1, g) = $b LIMIT 1");
}

# A move of $bucket that stops at the end of its catch-up.  Returns a hash with
# release() (let it go on to the switch) and finish() (wait for it to end).
my $seq = 0;

sub start_move
{
	my ($bucket, $tgt) = @_;
	my $name = 'mv' . ++$seq;
	my $hold = "$tmp/hold_$name";
	open(my $hfh, '>', $hold) or die;
	close($hfh);
	my $pid = fork();
	die 'fork' unless defined $pid;
	if ($pid == 0)
	{
		my $sql = "SET kingres_shard.move_switch_timeout = '1500ms'; SET kingres_shard.fault_injection = 'move_after_catchup:wait:$hold'; SELECT kingres.move_bucket(1, $bucket, $tgt)";
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$sql\" > $tmp/out_$name 2>&1");
		POSIX::_exit(0);
	}
	ok(wait_for_fault_file("$hold.reached", 120), "$name: the move reached the end of the catch-up");
	return { pid => $pid, hold => $hold, bucket => $bucket, name => $name };
}

# Let the move go and wait until one attempt to switch has given up.
sub wait_gave_up
{
	my ($m) = @_;
	unlink($m->{hold});
	my $seen = 0;
	for (1 .. 300)
	{
		my $d = $n1->safe_psql('postgres', "SELECT coalesce(string_agg(detail, '|'), '') FROM kingres.bucket_move WHERE bucket = $m->{bucket} AND phase IN ('catching_up', 'switching')");
		if ($d =~ /switch attempt \d+ gave up/) { $seen = 1; last; }
		sleep(0.1);
	}
	ok($seen, "$m->{name}: an attempt to switch gave up while the transaction is open");
}

sub finish_move
{
	my ($m, $tgt) = @_;
	waitpid($m->{pid}, 0);
	is(owner($m->{bucket}), "$tgt", "$m->{name}: the move completed on group $tgt");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
}

sub total
{
	my ($node) = @_;
	return $node->safe_psql('postgres', 'SELECT count(*) || \',\' || sum(id) FROM public.orders');
}

my $sum = 0;
$sum += $_ for 1 .. 60;
my $want = "60,$sum";

# ---------------------------------------------------------------------------
# COPY and INSERT ... SELECT while a move is in its last phase before the switch.
# ---------------------------------------------------------------------------
{
	my $b = 0;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my $m = start_move($b, $tgt);
	for my $node (@nodes)
	{
		my ($rc, $out) = run_sql($node, 'COPY (SELECT id FROM public.orders ORDER BY id) TO STDOUT');
		is(scalar(() = $out =~ /^\d+$/mg), 60, 'COPY (SELECT ...) TO returns every row from ' . $node->name . ' during the move');
	}
	my ($rc, $out, $err) = run_sql($n1, "COPY public.orders FROM STDIN;\n2001\t1\n\\.\n");
	isnt($rc, 0, 'COPY FROM into a distributed table is refused during a move');
	like($err, qr/COPY FROM/, '... with its own message');
	is(total($n1), $want, 'and nothing was written');
	$n1->safe_psql('postgres', 'TRUNCATE public.archive');
	$n1->safe_psql('postgres', 'INSERT INTO public.archive SELECT id, customer FROM public.orders');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.archive'), '60', 'INSERT ... SELECT from the distributed table into a local one: every row once');
	unlink($m->{hold});
	finish_move($m, $tgt);
	is(total($_), $want, 'the rows are the same after the move from ' . $_->name) for @nodes;
	$n1->safe_psql('postgres', 'TRUNCATE public.archive');
	$n1->safe_psql('postgres', 'INSERT INTO public.archive SELECT id, customer FROM public.orders');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.archive'), '60', 'and INSERT ... SELECT after it');
}

# ---------------------------------------------------------------------------
# A user two-phase transaction that wrote the bucket holds the switch back.
# ---------------------------------------------------------------------------
{
	my $b = 1;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my $key = key_in_bucket($b, 1000);
	my $m = start_move($b, $tgt);
	my ($rc, $out, $err) = run_sql($nodes[$src], "BEGIN; INSERT INTO public.orders VALUES ($key, 5); PREPARE TRANSACTION 'user_tx_1';");
	is($rc, 0, 'a user transaction on the source node is prepared') or diag($err);
	wait_gave_up($m);
	is(owner($b), "$src", 'the bucket has not moved while the transaction is prepared');
	$nodes[$src]->safe_psql('postgres', "COMMIT PREPARED 'user_tx_1'");
	finish_move($m, $tgt);
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.orders WHERE id = $key"), '1', 'the row of the prepared transaction is there once');
	is($nodes[$tgt]->safe_psql('postgres', "SELECT count(*) FROM public.orders WHERE id = $key"), '1', 'and on the new owner');
	is(total($n1), '61,' . ($sum + $key), 'the table is whole');
	$n1->safe_psql('postgres', "DELETE FROM public.orders WHERE id = $key");
}

# ---------------------------------------------------------------------------
# A long REPEATABLE READ or SERIALIZABLE transaction that read the table holds
# the switch back, and reads the same rows throughout.
# ---------------------------------------------------------------------------
for my $level ('REPEATABLE READ', 'SERIALIZABLE')
{
	my $b = 2;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my $m = start_move($b, $tgt);
	my $s = $nodes[$src]->background_psql('postgres');
	$s->query_safe("BEGIN ISOLATION LEVEL $level;");
	is($s->query_safe('SELECT count(*) || \',\' || sum(id) FROM public.orders'), $want, "$level: the first read in the transaction");
	wait_gave_up($m);
	is($s->query_safe('SELECT count(*) || \',\' || sum(id) FROM public.orders'), $want, "$level: the same rows while the move waits");
	$s->query_safe('COMMIT');
	$s->quit;
	finish_move($m, $tgt);
	is(total($n1), $want, "$level: the table is whole after the move");
}

# A transaction on the target node that has read the table holds the switch back
# too: its scan keeps transactions open on the other nodes, among them the source.
# The move gives up cleanly (it names the blocker and moves nothing) and, started
# again after the transaction, completes.
for my $level ('REPEATABLE READ', 'SERIALIZABLE')
{
	my $b = 3;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my $m = start_move($b, $tgt);
	my $s = $nodes[$tgt]->background_psql('postgres');
	$s->query_safe("BEGIN ISOLATION LEVEL $level;");
	is($s->query_safe('SELECT count(*) || \',\' || sum(id) FROM public.orders'), $want, "$level on the target: the first read");
	unlink($m->{hold});
	waitpid($m->{pid}, 0);
	is(owner($b), "$src", "$level on the target: the move gave up and the bucket stayed");
	like($n1->safe_psql('postgres', "SELECT phase || ' ' || coalesce(detail, '') FROM kingres.bucket_move WHERE bucket = $b ORDER BY move_id DESC LIMIT 1"),
		qr/^failed .*gave up after \d+ attempts to switch.*pid \d+/, "$level on the target: and says what held it back");
	is($s->query_safe('SELECT count(*) || \',\' || sum(id) FROM public.orders'), $want, "$level on the target: the transaction still reads every row once");
	$s->query_safe('COMMIT');
	$s->quit;
	is($n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, $tgt) > 0"), 't', "$level on the target: the move succeeds once the transaction is over");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
	is(total($n1), $want, "$level on the target: the table is whole");
	# put the bucket back for the next round
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, $src)");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
}

# A REPEATABLE READ or SERIALIZABLE transaction on the target that has read
# only rows of its own buckets keeps no transaction on the source, so the switch
# is not held back and a bucket arrives on its node while it is open.  The rows
# of the arriving copy were written after its snapshot (the transaction starts
# before the move does): its next scan of the
# table must not silently miss them.
for my $level ('REPEATABLE READ', 'SERIALIZABLE')
{
	my $b = 3;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my $own = $n1->safe_psql('postgres', "SELECT b FROM generate_series(0, 5) b WHERE (SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = b) = $tgt LIMIT 1");
	my $ownkey = key_in_bucket($own, 3000);
	my $s = $nodes[$tgt]->background_psql('postgres', on_error_stop => 0);
	$s->query_safe("BEGIN ISOLATION LEVEL $level;");
	$s->query_safe("SELECT customer FROM public.orders WHERE id = $ownkey");
	my $m = start_move($b, $tgt);
	unlink($m->{hold});
	waitpid($m->{pid}, 0);
	is(owner($b), "$tgt", "$level reading its own bucket: the bucket arrived while the transaction was open");
	my $out = $s->query('SELECT count(*) || \',\' || sum(id) FROM public.orders');
	like($s->{stderr}, qr/could not serialize access/,
		"$level reading its own bucket: the next scan fails with a serialization failure instead of missing the rows of the arrived bucket (it returned [$out])");
	$s->query('ROLLBACK');
	$s->quit;
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, $src)");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
}

# A cursor on a third node, read across the switch.
{
	my $b = 4;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my $other = ($src + 2) % 3;
	my $m = start_move($b, $tgt);
	my $s = $nodes[$other]->background_psql('postgres');
	$s->query_safe('BEGIN; DECLARE c CURSOR FOR SELECT id FROM public.orders ORDER BY id;');
	my $first = $s->query_safe('FETCH 5 FROM c');
	is(scalar(split /\n/, $first), 5, 'a cursor on the scan of the table fetched its first rows');
	wait_gave_up($m);
	my $rest = $s->query_safe('FETCH ALL FROM c');
	is(scalar(split /\n/, $first) + scalar(split /\n/, $rest), 60, 'the cursor returns every row once while the move waits');
	$s->query_safe('COMMIT');
	$s->quit;
	finish_move($m, $tgt);
	is(total($n1), $want, 'the table is whole after the move');
}

done_testing();
