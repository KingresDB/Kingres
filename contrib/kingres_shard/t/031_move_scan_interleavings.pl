# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The map version rule (design/shard-phase4.md, "Implementation notes and
# deviations"): a statement that reads or writes several shard groups while a
# bucket moves either sees every bucket exactly once or fails with the retryable
# stale-map error KS001; it never returns a wrong answer, and it never fails with
# anything else.  This test forces the interleavings one by one instead of
# waiting for them.
#
# The switch of a move is one two-phase transaction: the map group primary (node
# 1, the coordinator) commits first, then COMMIT PREPARED reaches every other
# participant (the source, the target, and every other node, which only takes the
# new catalog) one after the other.  The fault point "after_commit@switch" stops
# the coordinator after its own commit and before the first COMMIT PREPARED; the
# test then commits the prepared transactions itself, one node at a time, in each
# possible order, so that every set of "committed" nodes is a state it can look at.
# Statements from every node (the source, the target, a third group, the map group
# primary, which is also the source or the target in some moves) are started in
# each state; scans are also started with a pause point in the scan, in one state,
# and let go in a later one (a fragment opened on a node before that node's commit,
# its version checked after it, and so on).  Every answer must be the true one, or
# KS001.
#
# KS_INTERLEAVINGS=full runs the whole matrix (every order of the commits for every
# kind of statement, four pause points of the scan, five pause points inside the
# switch; about 5 minutes); the default runs every order of the commits for scans and
# one order for the other kinds (about 2 minutes), so that the suite that runs all the
# tests at once is not slowed down by it.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use Time::HiRes qw(sleep time);
use KingresShardTest qw(make_cluster wait_for_fault_file);

# Run at a lower priority, servers included: this test starts hundreds of sessions,
# and the suites that run beside it (in the in-tree run, all at once) have tests that
# depend on timing (a recovery worker that wakes every second, the number of writes
# in a fixed time) and must not be starved by it.
POSIX::nice(10);

my @nodes = make_cluster('si', 3, logical => 1, extra => "max_connections = 300\n");
my ($n1) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();

$n1->safe_psql('postgres', q{
	CREATE TABLE public.t (id int PRIMARY KEY, v int NOT NULL);
	CREATE TABLE public.t2 (id int PRIMARY KEY, w int NOT NULL);
	CREATE TABLE public.r (id int PRIMARY KEY, w int NOT NULL);
	SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 12);
	SELECT kingres.create_distributed_table('public.t2', 'id', colocate_with => 'public.t');
	SELECT kingres.create_reference_table('public.r');
});
{
	my $sql = '';
	$sql .= "INSERT INTO public.t VALUES ($_, $_ * 3);\n" for 1 .. 240;
	$sql .= "INSERT INTO public.t2 VALUES ($_, $_ * 5);\n" for 1 .. 240;
	$sql .= "INSERT INTO public.r VALUES ($_, $_);\n" for 1 .. 240;
	$n1->safe_psql('postgres', $sql);
}
my $sumid = 240 * 241 / 2;
my $full = (($ENV{KS_INTERLEAVINGS} // '') eq 'full');

# ---- the statements ----------------------------------------------------------

my $key;		# a key of the bucket that moves, set per move
my $key2;		# another, for the routed write

# Each kind: the SQL, and a judge: (output, error) -> exact answer or not.
my %kinds = (
	scan => {
		sql => q{SELECT count(*) || ',' || sum(v) FROM public.t;},
		want => sub { '240,' . 3 * $sumid },
	},
	join_ref => {
		sql => q{SELECT count(*) || ',' || sum(t.v + r.w) FROM public.t JOIN public.r ON r.id = t.id;},
		want => sub { '240,' . 4 * $sumid },
	},
	join_colo => {
		sql => q{SELECT count(*) || ',' || sum(a.v + b.w) FROM public.t a JOIN public.t2 b ON b.id = a.id;},
		want => sub { '240,' . 8 * $sumid },
	},
	copyq => {
		sql => q{COPY (SELECT id, v FROM public.t) TO STDOUT;},
		# The rows, summed up by the judge.
		sum => 1,
		want => sub { '240,' . 3 * $sumid },
	},
	point => {
		sql => sub { "SELECT v FROM public.t WHERE id = $key;" },
		want => sub { 3 * $key },
	},
	point_write => {
		sql => sub { "UPDATE public.t SET v = v + 1000000 WHERE id = $key2 RETURNING v;" },
		write => 1,
	},
	fanout_write => {
		sql => q{UPDATE public.t SET v = v + 1000000 RETURNING id;},
		write => 1,
	},
);

# ---- helpers ---------------------------------------------------------------

my $seq = 0;
my %stats = (readers => 0, paused => 0, moves => 0);
my %by;				# "kind/role" => { correct, retryable, wrong, failed }
my (@wrong, @failed);

# Run a SQL script with psql in a process of its own; returns a handle.
sub spawn
{
	my ($node, $sql, $label) = @_;
	my $id = ++$seq;
	my $script = "$tmp/p$id.sql";
	open(my $fh, '>', $script) or die;
	print $fh "\\set VERBOSITY verbose\n$sql";
	close($fh);
	my $pid = fork();
	die "fork" unless defined $pid;
	if ($pid == 0)
	{
		open(STDOUT, '>', "$tmp/p$id.out") or POSIX::_exit(2);
		open(STDERR, '>', "$tmp/p$id.err") or POSIX::_exit(2);
		exec('psql', '-X', '-q', '-At', '-h', $node->host, '-p', $node->port,
			'-d', 'postgres', '-f', $script)
		  or POSIX::_exit(2);
	}
	return { pid => $pid, id => $id, label => $label, done => 0 };
}

sub finish
{
	my ($h, $timeout) = @_;
	return if $h->{done};
	$timeout //= 120;
	my $end = time() + $timeout;
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
		sleep(0.02);
	}
	$h->{done} = 1;
	for my $k (qw(out err))
	{
		my $s = '';
		if (open(my $fh, '<', "$tmp/p$h->{id}.$k")) { local $/; $s = <$fh>; close($fh); }
		$h->{$k} = $s;
	}
}

# A statement from one node of the kind $kind, optionally paused at a point (of
# the scan or of the fan-out) until the file is removed.  $move describes the
# move the statement runs against: { S, T }.
sub statement
{
	my ($origin, $kind, $point, $move, $label) = @_;
	my $k = $kinds{$kind};
	my $sql = '';
	my $file;
	if (defined $point)
	{
		$file = "$tmp/pause" . (++$seq);
		open(my $fh, '>', $file) or die;
		close($fh);
		$sql .= "SET kingres_shard.fault_injection = '$point:wait:$file';\n";
	}
	my $stmt = ref($k->{sql}) ? $k->{sql}->() : $k->{sql};
	$sql .= "$stmt\n";
	my $h = spawn($nodes[$origin], $sql, $label);
	$h->{pause_file} = $file;
	$h->{origin} = $origin;
	$h->{kind} = $kind;
	$h->{role} = ($origin == $move->{S}) ? 'source' : ($origin == $move->{T}) ? 'target' : 'third';
	$h->{role} .= '+mapgroup' if $origin == 0;
	$stats{readers}++;
	return $h;
}

# Wait until the paused statements have reached their pause point (or the time
# is up: one that waits for a lock held by a prepared transaction cannot).
sub settle
{
	my ($readers, $max) = @_;
	my $end = time() + $max;
	while (time() < $end)
	{
		my $all = 1;
		for my $r (@$readers)
		{
			next unless defined $r->{pause_file};
			next if $r->{reached};
			if (-e "$r->{pause_file}.reached") { $r->{reached} = 1; $stats{paused}++; }
			else { $all = 0; }
		}
		last if $all;
		sleep(0.01);
	}
}

sub release
{
	my ($r) = @_;
	unlink($r->{pause_file}) if defined $r->{pause_file};
}

# One statement's answer: correct, retryable, wrong or failed.
sub classify
{
	my ($r) = @_;
	finish($r);
	my $k = $kinds{$r->{kind}};
	my $out = $r->{out};
	my $err = $r->{err};
	$out =~ s/\s+$//;
	my $result;
	if ($r->{timeout})
	{
		$result = 'failed';
		$err = 'the statement did not finish';
	}
	elsif ($err =~ /KS001/ && $out eq '')
	{
		$result = 'retryable';
	}
	elsif ($err ne '')
	{
		$result = 'failed';
	}
	elsif ($k->{write})
	{
		# A write: the number of rows (a fan-out) or the new value (routed).
		if ($r->{kind} eq 'fanout_write')
		{
			my @lines = split /\n/, $out;
			$result = (@lines == 240) ? 'correct' : 'wrong';
		}
		else
		{
			$result = ($out =~ /^\d+$/ && ($out - 3 * $key2) % 1000000 == 0) ? 'correct' : 'wrong';
		}
		$r->{ok_write} = 1 if $result eq 'correct';
	}
	elsif ($k->{sum})
	{
		my ($n, $s) = (0, 0);
		for my $line (split /\n/, $out)
		{
			my @f = split /\t/, $line;
			$n++;
			$s += $f[1];
		}
		$result = ("$n,$s" eq $k->{want}->()) ? 'correct' : 'wrong';
		$out = "$n,$s";
	}
	else
	{
		$result = ($out eq $k->{want}->()) ? 'correct' : 'wrong';
	}
	$by{"$r->{kind}/$r->{role}"}{$result}++;
	$by{"$r->{kind}/all"}{$result}++;
	if ($result eq 'wrong' || $result eq 'failed')
	{
		my $e = $err;
		$e =~ s/\s+/ /g;
		my $msg = "$r->{label}: $result, got '$out' $e";
		push @{ $result eq 'wrong' ? \@wrong : \@failed }, $msg;
	}
	return $result;
}

sub permutations
{
	my @l = @_;
	return ([]) unless @l;
	my @r;
	for my $i (0 .. $#l)
	{
		my @rest = @l;
		my ($x) = splice(@rest, $i, 1);
		push @r, [$x, @$_] for permutations(@rest);
	}
	return @r;
}

sub group_buckets
{
	my ($g) = @_;
	return split /,/, $n1->safe_psql('postgres',
		"SELECT coalesce(string_agg(bucket::text, ',' ORDER BY bucket), '') FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = $g");
}

# A bucket of group $g, which keeps at least one more; also sets the keys that
# the routed statements use.
sub bucket_on
{
	my ($g) = @_;
	my @b = group_buckets($g);
	if (@b < 2)
	{
		# Bring one over from the group with most buckets, without any test around it.
		my ($rich) = sort { scalar(() = group_buckets($b)) <=> scalar(() = group_buckets($a)) } grep { $_ != $g } (0 .. 2);
		my ($mv) = group_buckets($rich);
		$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $mv, $g)");
		$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
		@b = group_buckets($g);
	}
	my $bucket = $b[0];
	my @keys = split /,/, $n1->safe_psql('postgres',
		"SELECT string_agg(id::text, ',' ORDER BY id) FROM (SELECT id FROM generate_series(1, 240) id WHERE kingres.bucket_for(1, id) = $bucket LIMIT 2) s");
	($key, $key2) = @keys;
	return $bucket;
}

sub prepared_nodes
{
	return grep { $nodes[$_]->safe_psql('postgres', q{SELECT count(*) FROM pg_prepared_xacts WHERE gid LIKE 'kingres\_%'}) > 0 } (0 .. 2);
}

sub commit_prepared_on
{
	my ($i) = @_;
	my @gids = split /\n/, $nodes[$i]->safe_psql('postgres', q{SELECT gid FROM pg_prepared_xacts WHERE gid LIKE 'kingres\_%'});
	$nodes[$i]->safe_psql('postgres', "COMMIT PREPARED '$_'") for @gids;
}

# After a move: every table is whole from every node, nothing is prepared, the
# bucket is on the target, and the writes of the statements that succeeded were
# done exactly once.
sub check_after_move
{
	my ($label, $bucket, $target, $statements) = @_;
	$n1->safe_psql('postgres', 'DELETE FROM kingres.dtx_log');
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');

	my $done_point = grep { $_->{ok_write} && $_->{kind} eq 'point_write' } @$statements;
	my $done_fan = grep { $_->{ok_write} && $_->{kind} eq 'fanout_write' } @$statements;
	my $had_writes = grep { $kinds{$_->{kind}}{write} } @$statements;
	my $raw = $n1->safe_psql('postgres', "SELECT v FROM public.t WHERE id = $key2");
	is($raw, 3 * $key2 + 1000000 * $done_point + 1000000 * $done_fan,
		"$label: the routed write that succeeded was applied exactly once each ($done_point writes)")
	  if $had_writes;
	if ($had_writes)
	{
		is($n1->safe_psql('postgres', "SELECT count(DISTINCT v - 3 * id) || ',' || max(v - 3 * id) FROM public.t WHERE id <> $key2"),
			"1," . (1000000 * $done_fan), "$label: every row got the fan-out write the same number of times ($done_fan)");
	}
	# Back to the starting point, for the next move.
	$n1->safe_psql('postgres', 'UPDATE public.t SET v = id * 3 WHERE v <> id * 3') if $had_writes;

	my @t = map { $_->safe_psql('postgres', q{SELECT count(*) || ',' || sum(v) FROM public.t}) } @nodes;
	is(join('|', @t), join('|', ('240,' . 3 * $sumid) x 3), "$label: the table is whole from every node");
	my @t2 = map { $_->safe_psql('postgres', q{SELECT count(*) || ',' || sum(w) FROM public.t2}) } @nodes;
	is(join('|', @t2), join('|', ('240,' . 5 * $sumid) x 3), "$label: the colocated table is whole from every node");
	my @left = prepared_nodes();
	is(scalar(@left), 0, "$label: nothing is left prepared");
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket"),
		$target, "$label: the bucket is on the target");
	$stats{moves}++;
}

# ---- the stepped switch ----------------------------------------------------
#
# Move one bucket from group $S to group $T; stop the coordinator after its own
# commit; commit the prepared transactions of the other nodes by hand in the order
# $perm (an index into the permutations); start statements in every state.  With
# a pause point, also statements of kind $pkind that stop at that point and are
# let go in each later state.

sub stepped_move
{
	my ($S, $T, $perm, %opt) = @_;
	my @kinds = @{ $opt{kinds} || ['scan'] };
	my ($pkind, $point) = ($opt{pkind}, $opt{point});
	my $bucket = bucket_on($S);
	my $move = { S => $S, T => $T };
	my $mfile = "$tmp/mover" . (++$seq);
	open(my $fh, '>', $mfile) or die;
	close($fh);
	my $mover = spawn($n1,
		"SET kingres_shard.fault_injection = 'after_commit\@switch:wait:$mfile';\n"
		  . "SELECT kingres.move_bucket(1, $bucket, $T);\n",
		'mover');
	ok(wait_for_fault_file("$mfile.reached", 60), "move of bucket $bucket $S>$T reached the commit");
	my @prep = prepared_nodes();
	my @perms = permutations(@prep);
	my @order = @{ $perms[$perm % @perms] };
	my $label = "bucket $bucket $S>$T commit order " . join('', map { $_ + 1 } @order);
	my @all;
	for my $k (0 .. scalar(@order))
	{
		my @here;
		for my $o (0 .. 2)
		{
			for my $kind (@kinds)
			{
				push @here, statement($o, $kind, undef, $move, "$label, $kind from node " . ($o + 1) . " in state $k");
			}
			if (defined $point)
			{
				for my $b ($k .. scalar(@order))
				{
					my $r = statement($o, $pkind, $point, $move,
						"$label, $pkind paused at $point from node " . ($o + 1) . " started in state $k, let go in state $b");
					$r->{release_at} = $b;
					push @here, $r;
				}
			}
		}
		push @all, @here;
		settle([grep { defined $_->{pause_file} } @here], 0.5);
		release($_) for grep { defined $_->{release_at} && $_->{release_at} == $k } @all;
		sleep(0.05);
		commit_prepared_on($order[$k]) if $k < @order;
	}
	unlink($mfile);
	finish($mover);
	classify($_) for @all;
	check_after_move($label, $bucket, $T, \@all);
}

# ---- the unstepped switch with a pause inside it -----------------------------
#
# The coordinator stops at $mpoint; statements are started from every node while
# it is stopped (some wait for locks of the switch), then the move goes on.

sub paused_move
{
	my ($S, $T, $mpoint, @kinds) = @_;
	my $spec = ($mpoint eq 'after_log') ? 'after_log@switch' : $mpoint;
	my $bucket = bucket_on($S);
	my $move = { S => $S, T => $T };
	my $mfile = "$tmp/mover" . (++$seq);
	open(my $fh, '>', $mfile) or die;
	close($fh);
	my $mover = spawn($n1,
		"SET kingres_shard.fault_injection = '$spec:wait:$mfile';\n"
		  . "SELECT kingres.move_bucket(1, $bucket, $T);\n",
		'mover');
	ok(wait_for_fault_file("$mfile.reached", 60), "move of bucket $bucket $S>$T reached $mpoint");
	my $label = "bucket $bucket $S>$T paused at $mpoint";
	my @all;
	for my $o (0 .. 2)
	{
		push @all, statement($o, $_, undef, $move, "$label, $_ from node " . ($o + 1))
		  for @kinds;
	}
	sleep(0.3);
	unlink($mfile);
	finish($mover);
	classify($_) for @all;
	check_after_move($label, $bucket, $T, \@all);
}

# ---- a reader that holds the tables keeps the switch out ----------------------
#
# A scan that has opened its cursors on every node (paused at the point) holds the
# tables there; the switch cannot take its locks, so the move cannot finish, and
# the scan, which reads the old placement, is exactly right.  When the scan is let
# go, the move completes.

sub pinned_move
{
	my ($S, $T, $origin, $kind, $point) = @_;
	my $bucket = bucket_on($S);
	my $move = { S => $S, T => $T };
	my $label = "bucket $bucket $S>$T, $kind from node " . ($origin + 1) . " paused at $point holds the tables";
	my $r = statement($origin, $kind, $point, $move, $label);
	ok(wait_for_fault_file("$r->{pause_file}.reached", 60), "$label: the statement reached its pause point");
	my $mover = spawn($n1, "SELECT kingres.move_bucket(1, $bucket, $T);\n", 'mover');
	sleep(3);
	waitpid($mover->{pid}, POSIX::WNOHANG()) == 0
	  or fail("$label: the move finished while the statement held the tables");
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket"),
		$S, "$label: the bucket is still on the source");
	release($r);
	is(classify($r), ($kind eq 'fanout_write' ? 'correct' : 'correct'), "$label: the statement answered exactly");
	finish($mover);
	check_after_move($label, $bucket, $T, [$r]);
}

# ---- run -------------------------------------------------------------------

my @pairs = ([0, 1], [1, 2], [2, 0], [0, 2], [2, 1], [1, 0]);
my @points = $full ? ('scan_after_declare', 'scan_started', 'scan_after_check', 'scan_begin') : ('scan_after_declare');

# The orders of the commits of the participants: the map group primary commits
# first; when it is also the source or the target it has a prepared transaction of
# its own (the control session of the switch is another session on the same node),
# so three nodes are committed by hand, otherwise two.
sub orders
{
	my ($pair) = @_;
	return (($pair->[0] == 0 || $pair->[1] == 0) ? 6 : 2);
}

# Every pause point inside the switch, before the first prepare: statements
# started there wait for the locks of the switch or go through.
for my $mp ($full ? qw(move_switch_after_lock move_switch_after_attach move_switch_after_detach
	move_switch_after_local after_log) : qw(move_switch_after_detach after_log))
{
	paused_move(@$_, $mp, qw(scan join_colo point)) for ($pairs[0], $pairs[3]);
}
paused_move(@{ $pairs[1] }, $_, qw(point_write fanout_write))
  for ($full ? qw(move_switch_after_lock move_switch_after_detach after_log) : qw(move_switch_after_detach));

# A statement that holds the tables, from every kind of origin, with the move that
# must wait for it.
pinned_move(@$_, 'scan', 'scan_after_check')
  for ($full ? ([1, 2, 0], [1, 2, 1], [1, 2, 2], [0, 1, 2], [2, 0, 1]) : ([1, 2, 0], [1, 2, 2], [0, 1, 2]));
pinned_move(1, 2, 0, 'join_colo', 'scan_after_declare');
pinned_move(0, 2, 1, 'fanout_write', 'fanout_after_run');

# Every ordered pair of groups and every order of the commits: scans from every
# node in every state, and scans paused in one state and let go in a later one.
for my $pair (@pairs)
{
	stepped_move($pair->[0], $pair->[1], $_, kinds => [$full ? qw(scan join_colo copyq point) : 'scan'],
		pkind => 'scan', point => $points[0]) for (0 .. orders($pair) - 1);
}
# The other kinds of read in every state: the join of two distributed tables, a
# COPY of a query, a routed read, and the join with a reference table (every node
# has it, it never moves); every order in the full run, one in the default run.
for my $pair (@pairs)
{
	stepped_move($pair->[0], $pair->[1], $_, kinds => [qw(join_colo copyq point join_ref)])
	  for ($full ? (0 .. orders($pair) - 1) : (0));
}

if ($full)
{
	# The other pause points, and the pause of a join of two distributed tables.
	for my $i (1 .. $#points)
	{
		for my $pair (@pairs)
		{
			stepped_move($pair->[0], $pair->[1], $_, kinds => ['scan'], pkind => 'scan', point => $points[$i])
			  for (0 .. 1);
		}
	}
	for my $pair (@pairs)
	{
		stepped_move($pair->[0], $pair->[1], $_, kinds => ['scan'], pkind => 'join_colo', point => 'scan_after_declare')
		  for (0 .. 1);
	}
}

# Writes: a routed write to a key of the bucket, and a fan-out write, from every
# node in every state; each is applied exactly once, or fails with KS001 and
# changes nothing.
for my $pair (@pairs)
{
	stepped_move($pair->[0], $pair->[1], $_, kinds => [qw(point_write fanout_write)],
		pkind => 'fanout_write', point => 'fanout_after_run')
	  for ($full ? (0 .. orders($pair) - 1) : (0));
}


# ---- a bucket that moves away and back inside the start of a scan (S4-01) ------
#
# The placement of a colocation group is the same before the bucket left and after
# it came back, so a comparison of placements alone cannot see a scan that was
# planned on the node in between: its cursor was declared while the bucket was
# detached, and the rows of the bucket would silently be missing.  Group 2 owns no
# bucket of the table (so no cursor of the scan holds a lock there, and the move
# back can run while the scan is paused).  The scan starts from node 1, asks node 2
# for its placement, and its DECLARE waits for the lock of the first switch; when
# the first move has committed the DECLARE is planned without the bucket, the scan
# stops, the bucket comes back, and the scan goes on: it must fail with KS001, never
# answer 16 rows of 30.

$n1->safe_psql('postgres', q{
	CREATE TABLE public.w (id int PRIMARY KEY, v int NOT NULL);
	SELECT kingres.set_node_property(3, false);
	SELECT kingres.create_distributed_table('public.w', 'id', bucket_count => 2);
	SELECT kingres.set_node_property(3, true);
});
$n1->safe_psql('postgres', join('', map { "INSERT INTO public.w VALUES ($_, $_);
" } 1 .. 30));
my $wcol = $n1->safe_psql('postgres',
	q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.w'::regclass});
$kinds{scan_w} = {
	sql => q{SELECT count(*) || ',' || sum(v) FROM public.w;},
	want => sub { '30,465' },
};

sub w_whole
{
	my ($label) = @_;
	my @t = map { $_->safe_psql('postgres', q{SELECT count(*) || ',' || sum(v) FROM public.w}) } @nodes;
	is(join('|', @t), join('|', ('30,465') x 3), "$label: the table is whole from every node");
}

{
	my ($S, $T) = (1, 2);
	my $bucket = $n1->safe_psql('postgres',
		"SELECT bucket FROM kingres.bucket_map WHERE colocation_id = $wcol AND group_id = $S");
	my $move = { S => $S, T => $T };
	my $label = "away and back: bucket $bucket $S>$T>$S inside the start of a scan";

	my $aflag = "$tmp/away" . (++$seq);
	open(my $fh, '>', $aflag) or die;
	close($fh);
	my $mover = spawn($n1,
		"SET kingres_shard.fault_injection = 'move_switch_after_detach:wait:$aflag';\n"
		  . "SELECT kingres.move_bucket($wcol, $bucket, $T);\n",
		'mover');
	ok(wait_for_fault_file("$aflag.reached", 60), "$label: the first move holds the source in its switch");

	my $scan = statement(0, 'scan_w', "scan_declared_g$S", $move, "$label, scan from node 1");
	ok($nodes[$S]->poll_query_until('postgres',
			q{SELECT count(*) > 0 FROM pg_locks WHERE NOT granted AND locktype = 'relation' AND relation = 'public.w'::regclass}),
		"$label: the DECLARE of the scan waits for the switch's lock");
	unlink($aflag);
	finish($mover);
	ok(wait_for_fault_file("$scan->{pause_file}.reached", 60), "$label: the scan declared its cursor on the detached bucket");
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $wcol AND bucket = $bucket"),
		$T, "$label: the bucket is on the target");

	$n1->safe_psql('postgres', "SELECT kingres.move_bucket($wcol, $bucket, $S)");
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $wcol AND bucket = $bucket"),
		$S, "$label: the bucket is back on the source, the placement is the original one");
	release($scan);
	is(classify($scan), 'retryable', "$label: the scan failed with KS001 and did not answer without the bucket");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
	w_whole($label);
}

# ---- two moves of different buckets between the placement check and the cursor ---
#
# A scan from node 1 over a table whose buckets are on every group pauses after the
# groups have been asked for their placement; a bucket of group 2 moves to group 1
# and one of group 1 to group 2 (every node keeps the number of partitions it had,
# only the placement changes); the scan goes on and must not answer.

$n1->safe_psql('postgres', q{
	CREATE TABLE public.w2 (id int PRIMARY KEY, v int NOT NULL);
	SELECT kingres.create_distributed_table('public.w2', 'id', bucket_count => 6);
});
$n1->safe_psql('postgres', join('', map { "INSERT INTO public.w2 VALUES ($_, $_);
" } 1 .. 60));
my $w2col = $n1->safe_psql('postgres',
	q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.w2'::regclass});
{
	my $label = 'two moves of different buckets inside the start of a scan';
	my $b1 = $n1->safe_psql('postgres',
		"SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = $w2col AND group_id = 1");
	my $b2 = $n1->safe_psql('postgres',
		"SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = $w2col AND group_id = 2");
	$kinds{scan_w2} = {
		sql => q{SELECT count(*) || ',' || sum(v) FROM public.w2;},
		want => sub { '60,1830' },
	};
	my $scan = statement(0, 'scan_w2', 'scan_after_placement', { S => 1, T => 2 }, "$label, scan from node 1");
	ok(wait_for_fault_file("$scan->{pause_file}.reached", 60), "$label: the scan asked the groups for their placement");
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket($w2col, $b1, 2)");
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket($w2col, $b2, 1)");
	release($scan);
	is(classify($scan), 'retryable', "$label: the scan failed with KS001");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
	my @t = map { $_->safe_psql('postgres', q{SELECT count(*) || ',' || sum(v) FROM public.w2}) } @nodes;
	is(join('|', @t), join('|', ('60,1830') x 3), "$label: the table is whole from every node");
}

diag(sprintf('%-32s %8s %10s %6s %6s', 'kind/origin role', 'correct', 'retryable', 'wrong', 'failed'));
for my $k (sort keys %by)
{
	diag(sprintf('%-32s %8d %10d %6d %6d', $k, map { $by{$k}{$_} // 0 } qw(correct retryable wrong failed)));
}
diag("statements: $stats{readers}, paused at their point: $stats{paused}, moves: $stats{moves}");
diag($_) for @wrong[0 .. ($#wrong < 19 ? $#wrong : 19)];
diag($_) for @failed[0 .. ($#failed < 9 ? $#failed : 9)];
my ($correct, $retryable) = (0, 0);
for my $k (grep { m{/all$} } keys %by)
{
	$correct += $by{$k}{correct} // 0;
	$retryable += $by{$k}{retryable} // 0;
}
diag("correct: $correct, retryable (KS001): $retryable, wrong: " . scalar(@wrong) . ", other failures: " . scalar(@failed));
is(scalar(@wrong), 0, 'no statement returned a wrong answer (a row missing or twice, a write lost or doubled)');
is(scalar(@failed), 0, 'no statement failed with anything but KS001');
for my $role (qw(source target third))
{
	cmp_ok($by{"scan/$role"}{correct} // 0, '>', 20, "scans from a $role node were answered");
}
my $mg = 0;
$mg += $by{"scan/$_+mapgroup"}{correct} // 0 for qw(source target third);
cmp_ok($mg, '>', 20, 'scans from the map group primary were answered');

done_testing();
