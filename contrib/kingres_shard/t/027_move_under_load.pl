# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Moves under load (design/shard-phase4.md, "Tests required"):
#   * writers on the moving bucket and on other buckets, from all three nodes,
#     run throughout several moves; afterwards the tables equal a model kept by
#     the writers, and no statement failed except with the retryable stale-map
#     error, which the writer repeats;
#   * a long transaction that wrote the bucket holds the switch back, the switch
#     timeout returns the move to catching up, and it completes after the
#     transaction ends;
#   * how long the writers of the moving bucket waited is measured and reported.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster move_state wait_for_fault_file);

my @nodes = make_cluster('ld', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();
my $stop = "$tmp/stop";

$n1->safe_psql('postgres', q{
	CREATE TABLE public.orders (id int PRIMARY KEY, customer int NOT NULL);
	CREATE TABLE public.items (id int NOT NULL, order_id int NOT NULL, PRIMARY KEY (id, order_id));
	SELECT kingres.create_distributed_table('public.orders', 'id', bucket_count => 6);
	SELECT kingres.create_distributed_table('public.items', 'order_id', colocate_with => 'public.orders');
	ALTER TABLE public.items ADD CONSTRAINT items_order_fk FOREIGN KEY (order_id) REFERENCES public.orders (id);
});

# The keys of the first bucket to move.
my $bucket0 = 1;
my %moving = map { $_ => 1 } split /,/, $n1->safe_psql('postgres',
	"SELECT string_agg(g::text, ',') FROM generate_series(1, 240) g WHERE kingres.bucket_for(1, g) = $bucket0");

# One writer: a process that owns the keys k with k % 3 == w, runs random
# inserts, updates and deletes of its orders (and an item for each) through
# node w, and keeps the model of what it did.  A statement that fails with the
# stale-map error is repeated; any other error is counted.
sub writer
{
	my ($w, $node) = @_;
	srand(1000 + $w);
	my @keys = grep { $_ % 3 == $w } 1 .. 240;
	my (%order, %item);
	my ($maxlat_moving, $maxlat_other, $unexpected, $retries, $ops) = (0, 0, 0, 0, 0);
	my $run = sub {
		my ($k, $sql) = @_;
		for my $try (1 .. 30)
		{
			my $t0 = time();
			my $out = `psql -X -q -At -h @{[ $node->host ]} -p @{[ $node->port ]} -d postgres -c "$sql" 2>&1`;
			my $rc = $? >> 8;
			my $lat = time() - $t0;
			if ($moving{$k}) { $maxlat_moving = $lat if $lat > $maxlat_moving; }
			else { $maxlat_other = $lat if $lat > $maxlat_other; }
			return 1 if $rc == 0;
			if ($out =~ /bucket map|stale|map changed|is not on this node/) { $retries++; sleep(0.05); next; }
			$unexpected++;
			open(my $e, '>>', "$tmp/werr$w") or die;
			print $e "$sql: $out\n";
			close($e);
			return 0;
		}
		return 0;
	};
	while (!-e $stop)
	{
		my $k = $keys[int(rand(@keys))];
		$ops++;
		if (!exists $order{$k})
		{
			my $c = int(rand(1000));
			if ($run->($k, "INSERT INTO public.orders VALUES ($k, $c)")) { $order{$k} = $c; }
			if (exists $order{$k} && !$item{$k} && $run->($k, "INSERT INTO public.items VALUES ($k, $k)")) { $item{$k} = 1; }
		}
		elsif (rand() < 0.5)
		{
			my $c = int(rand(1000));
			$order{$k} = $c if $run->($k, "UPDATE public.orders SET customer = $c WHERE id = $k");
		}
		else
		{
			if ($item{$k}) { delete $item{$k} if $run->($k, "DELETE FROM public.items WHERE order_id = $k"); }
			if (!$item{$k}) { delete $order{$k} if $run->($k, "DELETE FROM public.orders WHERE id = $k"); }
		}
	}
	open(my $m, '>', "$tmp/model$w") or die;
	print $m "O $_ $order{$_}\n" for sort { $a <=> $b } keys %order;
	print $m "I $_\n" for sort { $a <=> $b } keys %item;
	print $m "S $maxlat_moving $maxlat_other $unexpected $retries $ops\n";
	close($m);
}

my @pids;
for my $w (0 .. 2)
{
	my $pid = fork();
	die "fork" unless defined $pid;
	if ($pid == 0) { writer($w, $nodes[$w]); POSIX::_exit(0); }
	push @pids, $pid;
}

# Four moves while they run: the bucket of the measured keys first, then others.
sleep(1.5);
my @moved;
for my $round (0 .. 3)
{
	my $bucket = ($bucket0 + $round * 2) % 6;
	my $src = $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket");
	my $tgt = ($src + 1) % 3;
	my $t0 = time();
	my $id = $n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $bucket, $tgt)");
	push @moved, sprintf("%d (bucket %d, %.1f s)", $id, $bucket, time() - $t0);
	sleep(0.5);
}
sleep(1.0);
open(my $sfh, '>', $stop) or die;
close($sfh);
waitpid($_, 0) for @pids;
diag("moves: " . join('; ', @moved));

# The model.
my (%order, %item);
my ($lat_m, $lat_o, $unexpected, $retries, $ops) = (0, 0, 0, 0, 0);
my %per;
for my $w (0 .. 2)
{
	open(my $m, '<', "$tmp/model$w") or die "no model of writer $w";
	while (my $l = <$m>)
	{
		chomp $l;
		if ($l =~ /^O (\d+) (\d+)$/) { $order{$1} = $2; }
		elsif ($l =~ /^I (\d+)$/) { $item{$1} = 1; }
		elsif ($l =~ /^S (\S+) (\S+) (\d+) (\d+) (\d+)$/)
		{
			$lat_m = $1 if $1 > $lat_m;
			$lat_o = $2 if $2 > $lat_o;
			$unexpected += $3; $retries += $4; $ops += $5; $per{$w} = $5;
		}
	}
	close($m);
}
# What this is for: the moves ran while the writers wrote, and every writer did write.  How many
# operations they managed depends on the load of the machine, which is not what the test is about
# (it used to assert more than 200 and failed on a machine whose load average was above 50).
ok((grep { ($per{$_} // 0) > 0 } 0 .. 2) == 3, "every writer ran during the moves ($ops operations in all)");
is($unexpected, 0, 'no statement failed except with the retryable stale-map error') or do {
	for my $w (0 .. 2) { if (open(my $e, '<', "$tmp/werr$w")) { diag($_) while <$e>; close($e); } }
};
diag(sprintf("stale-map retries: %d; longest statement on the moving bucket %.0f ms, on other buckets %.0f ms (each includes starting psql)",
		$retries, $lat_m * 1000, $lat_o * 1000));
cmp_ok($lat_m, '<', 8, 'no writer of the moving bucket waited for longer than the switch timeout');

my $want_o = join(',', map { "$_:$order{$_}" } sort { $a <=> $b } keys %order);
my $want_i = join(',', sort { $a <=> $b } keys %item);
for my $node (@nodes)
{
	is($node->safe_psql('postgres', q{SELECT coalesce(string_agg(id || ':' || customer, ',' ORDER BY id), '') FROM public.orders}),
		$want_o, 'orders equal the model from ' . $node->name);
	is($node->safe_psql('postgres', q{SELECT coalesce(string_agg(order_id::text, ',' ORDER BY order_id), '') FROM public.items}),
		$want_i, 'items equal the model from ' . $node->name);
}
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('failed', 'cancelled')"), '0', 'no move failed');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)') for 1 .. 3;
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.bucket_move WHERE phase = 'done'"), '4', 'the four moves finish') or diag($n1->safe_psql('postgres', q{SELECT move_id || ' ' || phase || ' ' || coalesce(detail, '') FROM kingres.bucket_move ORDER BY move_id}));

# A long transaction that wrote the bucket holds the switch back.
{
	my $bucket = 3;
	my $src = $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket");
	my $tgt = ($src + 1) % 3;
	my $key = $n1->safe_psql('postgres', "SELECT g FROM generate_series(1000, 2000) g WHERE kingres.bucket_for(1, g) = $bucket LIMIT 1");
	$n1->safe_psql('postgres', "INSERT INTO public.orders VALUES ($key, 7)");
	my $script = "$tmp/long.sql";
	my $hold = "$tmp/hold_switch";
	open(my $fh, '>', $script) or die;
	open(my $hfh, '>', $hold) or die;
	close($hfh);
	print $fh "SET kingres_shard.move_switch_timeout = '1500ms';\nSET kingres_shard.fault_injection = 'move_after_catchup:wait:$hold';\nSELECT kingres.move_bucket(1, $bucket, $tgt);\n";
	close($fh);
	my $pid = fork();
	die "fork" unless defined $pid;
	if ($pid == 0)
	{
		system("psql -X -q -At -h " . $n1->host . " -p " . $n1->port . " -d postgres -f $script > $tmp/long.out 2>&1");
		POSIX::_exit(0);
	}
	# The transaction must start after the initial copy is complete and before the
	# first attempt to switch: the copy of each table creates a slot of its own on
	# the source, and a slot waits for the transactions that are running when it is
	# created, so a transaction that starts during the copy holds the copy back and
	# the move never reaches the switch.  The move stops by itself at the end of the
	# catch-up (a fault point), which is exactly that moment, however slow the host.
	ok(wait_for_fault_file("$hold.reached", 120), 'the move reached the end of the catch-up');
	my $holder = $nodes[$src]->background_psql('postgres');
	$holder->query_safe("BEGIN; UPDATE public.orders SET customer = 8 WHERE id = $key;");
	unlink($hold);
	my $seen = 0;
	for (1 .. 300)
	{
		my $d = $n1->safe_psql('postgres', "SELECT coalesce(string_agg(detail, '|'), '') FROM kingres.bucket_move WHERE bucket = $bucket AND phase IN ('catching_up', 'switching')");
		if ($d =~ /switch attempt \d+ gave up/) { $seen = 1; last; }
		sleep(0.1);
	}
	ok($seen, 'the move went back to catching up when the switch timed out');
	like($n1->safe_psql('postgres', "SELECT phase FROM kingres.bucket_move WHERE bucket = $bucket ORDER BY move_id DESC LIMIT 1"),
		qr/^(catching_up|switching)$/, 'and is still not switched while the transaction is open');
	$holder->query_safe('COMMIT');
	$holder->quit;
	waitpid($pid, 0);
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket"),
		"$tgt", 'the move completed after the transaction ended');
	is($n1->safe_psql('postgres', "SELECT customer FROM public.orders WHERE id = $key"), '8',
		'the row written by the long transaction is on the new owner');
	is($nodes[$tgt]->safe_psql('postgres', "SELECT customer FROM public.orders_b0003 WHERE id = $key"), '8', 'it is stored there');
}

done_testing();
