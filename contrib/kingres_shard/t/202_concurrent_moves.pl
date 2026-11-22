# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/202_concurrent_moves.pl: several bucket moves at the same time (design/shard-phase10.md, sections 6.3 and 8.5; increment P10.4).
# The scheduler of the control plane runs several moves at once: moves of different colocation groups, and moves of one colocation group between
# disjoint pairs of shard groups.  Moves that would lock the same tables on the same node are not started together by the scheduler, but a mistake
# there must not damage anything, so the test also runs them together.
#
#   A. one colocation group, disjoint pairs of groups: both moves complete, with readers running meanwhile
#   B. two colocation groups, the same source and target groups: both complete
#   C. one colocation group, the same source and target: each move completes or fails cleanly (nothing is left, the bucket is where it was),
#      and the failed one succeeds when it is started again
#   D. the same bucket twice: the second is refused, the first completes
#   E. four moves at once over two colocation groups
#   F. the epoch counts every switch, the placement hash changes with every one, check_cluster() finds nothing at the end

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(usleep);
use KingresShardTest qw(make_cluster run_sql start_psql finish_psql);

my @nodes = make_cluster('cm', 4, logical => 1, extra => "max_connections = 120
");
my ($n1, $n2, $n3, $n4) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

q1($n1, q{
	CREATE TABLE public.a1 (id int PRIMARY KEY, v text);
	CREATE TABLE public.a2 (id int PRIMARY KEY, w int);
	CREATE TABLE public.b1 (id int PRIMARY KEY, v text);
	INSERT INTO public.a1 SELECT g, 'a' || g FROM generate_series(1, 4000) g;
	INSERT INTO public.a2 SELECT g, g FROM generate_series(1, 4000) g;
	INSERT INTO public.b1 SELECT g, 'b' || g FROM generate_series(1, 3000) g;
	SELECT kingres.create_distributed_table('public.a1', 'id', bucket_count => 8);
	SELECT kingres.create_distributed_table('public.a2', 'id', colocate_with => 'public.a1');
	SELECT kingres.create_distributed_table('public.b1', 'id', bucket_count => 6);
});
my $A = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.a1'::regclass});
my $B = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.b1'::regclass});
my $want = q1($n1, q{SELECT (SELECT count(*) || ',' || sum(id) FROM public.a1) || '|' || (SELECT count(*) || ',' || sum(w) FROM public.a2) || '|' || (SELECT count(*) || ',' || sum(id) FROM public.b1)});
sub digest { my ($n) = @_; return q1($n, q{SELECT (SELECT count(*) || ',' || sum(id) FROM public.a1) || '|' || (SELECT count(*) || ',' || sum(w) FROM public.a2) || '|' || (SELECT count(*) || ',' || sum(id) FROM public.b1)}); }
sub owner { my ($c, $b) = @_; return q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $c AND bucket = $b"); }
sub epoch { my ($c) = @_; return q1($n1, "SELECT placement_epoch FROM kingres.colocation WHERE colocation_id = $c"); }
sub clean { return q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}); }
sub phash { my ($c) = @_; return q1($n1, "SELECT kingres.internal_placement_hash($c)"); }

# run the moves together (each in a session of its own on the map group primary), with a reader going meanwhile;
# returns the list of [rc-ok, error text] in the order of the moves
sub together
{
	my (@moves) = @_;	# [colocation, bucket, target]
	my @h = map { start_psql($n1, "SELECT kingres.move_bucket($_->[0], $_->[1], $_->[2])") } @moves;
	my $reads = 0;
	my $bad = 0;
	while (grep { $_->{h}->pumpable } @h)
	{
		for my $h (@h) { eval { $h->{h}->pump_nb if $h->{h}->pumpable; 1 }; }
		for my $n ($n2, $n3, $n4)
		{
			my ($rc, $out, $err) = run_sql($n, q{SELECT count(*) FROM public.a1});
			$reads++;
			# a read that overlaps a switch may fail with the retryable KS001; it must never be a wrong answer
			if ($rc == 0) { if ($out ne '4000') { $bad++; diag("wrong answer: $out"); } }
			elsif ($err !~ /KS001|stale|placement/i) { $bad++; diag("unexpected error: $err"); }
		}
		usleep(100_000);
	}
	my @res = map { my ($out, $err) = finish_psql($_); [ $out =~ /^\d+$/m ? 1 : 0, $err ] } @h;
	return ($bad, $reads, @res);
}
sub settle { q1($n1, q{SELECT kingres.resume_bucket_moves()}); q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)}); }

# ---------------------------------------------------------------------------
# A. disjoint pairs of one colocation group
# ---------------------------------------------------------------------------
{
	my ($sa, $sb) = (owner($A, 0), owner($A, 2));
	my ($ta, $tb) = (($sa + 1) % 4, ($sb + 1) % 4);
	isnt($sa, $sb, 'A: the two buckets are on different groups');
	my $e = epoch($A);
	my ($bad, $reads, @res) = together([$A, 0, $ta], [$A, 2, $tb]);
	is($res[0][0], 1, 'A: the first move completes') or diag($res[0][1]);
	is($res[1][0], 1, 'A: the second move completes') or diag($res[1][1]);
	is($bad, 0, "A: no reader got a wrong answer ($reads reads)");
	settle();
	is(owner($A, 0), $ta, 'A: bucket 0 is on its target');
	is(owner($A, 2), $tb, 'A: bucket 2 is on its target');
	is(epoch($A), $e + 2, 'A: two switches, two counts');
	is(digest($_), $want, 'A: every node reads every row') for @nodes;
	is(clean(), '0', 'A: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# B. two colocation groups, the same groups
# ---------------------------------------------------------------------------
{
	my $sa = owner($A, 1);
	my $sb = owner($B, 1);
	my $t = (($sa + 2) % 4);
	$t = ($t == $sb ? ($t + 1) % 4 : $t);
	my ($ea, $eb) = (epoch($A), epoch($B));
	my ($bad, $reads, @res) = together([$A, 1, $t], [$B, 1, $t]);
	is($res[0][0], 1, 'B: the move in the first colocation group completes') or diag($res[0][1]);
	is($res[1][0], 1, 'B: the move in the second colocation group completes') or diag($res[1][1]);
	is($bad, 0, "B: no reader got a wrong answer ($reads reads)");
	settle();
	is(epoch($A), $ea + 1, 'B: one switch in the first group');
	is(epoch($B), $eb + 1, 'B: one switch in the second group');
	is(digest($_), $want, 'B: every node reads every row') for @nodes;
	is(clean(), '0', 'B: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# C. one colocation group, the same source and the same target
# ---------------------------------------------------------------------------
{
	my %by;
	push @{ $by{ owner($A, $_) } }, $_ for 0 .. 7;
	my ($src) = grep { @{ $by{$_} } >= 2 } sort keys %by;
	my ($b1, $b2) = @{ $by{$src} }[0, 1];
	my $tgt = ($src + 1) % 4;
	my ($bad, $reads, @res) = together([$A, $b1, $tgt], [$A, $b2, $tgt]);
	settle();
	my @ok = grep { $_->[0] } @res;
	cmp_ok(scalar(@ok), '>=', 1, 'C: at least one of the two moves completes');
	for my $i (0, 1)
	{
		my $b = $i ? $b2 : $b1;
		if ($res[$i][0]) { is(owner($A, $b), $tgt, "C: bucket $b moved"); }
		else
		{
			is(owner($A, $b), $src, "C: bucket $b did not move, and is where it was");
			my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($A, $b, $tgt)");
			is($rc, 0, "C: and moves when it is started again") or diag($err);
			settle();
		}
	}
	is($bad, 0, "C: no reader got a wrong answer ($reads reads)");
	is(digest($_), $want, 'C: every node reads every row') for @nodes;
	is(clean(), '0', 'C: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# D. the same bucket twice
# ---------------------------------------------------------------------------
{
	my $b = 3;
	my $src = owner($A, $b);
	my $t1 = ($src + 1) % 4;
	my $t2 = ($src + 2) % 4;
	my ($bad, $reads, @res) = together([$A, $b, $t1], [$A, $b, $t2]);
	settle();
	is(scalar(grep { $_->[0] } @res), 1, 'D: exactly one of the two moves of the same bucket completes');
	like(join(' ', map { $_->[1] } @res), qr/already being moved|already on shard group|is moving/, 'D: and the other says why');
	is(digest($_), $want, 'D: every node reads every row') for @nodes;
	is(clean(), '0', 'D: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# E. four at once
# ---------------------------------------------------------------------------
{
	my @mv = ([$A, 4, (owner($A, 4) + 1) % 4], [$A, 6, (owner($A, 6) + 1) % 4], [$B, 2, (owner($B, 2) + 1) % 4], [$B, 3, (owner($B, 3) + 3) % 4]);
	my ($ea, $eb) = (epoch($A), epoch($B));
	my ($bad, $reads, @res) = together(@mv);
	settle();
	my $done = scalar(grep { $_->[0] } @res);
	cmp_ok($done, '>=', 2, "E: at least two of the four moves complete ($done)");
	for my $i (0 .. 3)
	{
		next if $res[$i][0];
		my ($c, $b, $t) = @{ $mv[$i] };
		my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($c, $b, $t)");
		is($rc, 0, "E: the move $i that did not complete runs when it is started again") or diag($err);
	}
	settle();
	is($bad, 0, "E: no reader got a wrong answer ($reads reads)");
	is(digest($_), $want, 'E: every node reads every row') for @nodes;
	is(clean(), '0', 'E: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# F. the epoch and the hash
# ---------------------------------------------------------------------------
{
	my $h = phash($A);
	my $e = epoch($A);
	my $t = (owner($A, 5) + 1) % 4;
	my $back = owner($A, 5);
	run_sql($n1, "SELECT kingres.move_bucket($A, 5, $t)");
	run_sql($n1, "SELECT kingres.move_bucket($A, 5, $back)");
	settle();
	is(epoch($A), $e + 2, 'F: the epoch counts both switches');
	isnt(phash($A), $h, 'F: the placement hash is not the one before the bucket went away and came back');
	is(phash($A), q1($n3, "SELECT kingres.internal_placement_hash($A)"), 'F: and every node computes the same');
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_move WHERE phase NOT IN ('done', 'cancelled', 'failed')}), '0', 'F: no move is left unfinished');
	is(clean(), '0', 'F: check_cluster() finds nothing');
}

done_testing();
