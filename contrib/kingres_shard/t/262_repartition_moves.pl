# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/262_repartition_moves.pl
#
# Phase 12, increment J3 (design/analytics-phase12-queries.md, 4.10 and 14.2 "062_repartition_moves"): a bucket that moves, a bucket that
# splits, a cancel and an error while a repartitioned join runs.  The rule of every section: the statement gives exactly the rows that the
# same statement gives when nothing happens (and that the row-moving scan gives, with enable_repartition_join = off), or fails with the
# retryable KS001; no row is returned twice or missed; the statement repeated works; nothing is left behind (no session in a transaction,
# no cursor, no prepared transaction).
#
#   A. the statements and their answers when nothing happens (repartitioned, and by the scan)
#   B. split_bucket of the colocation group of the tables that stay, and of the one of the moved table
#   C. fault points rp_after_ship and rp_before_fragment: an error, a cancel, a move that commits there (the moved table's group and the
#      other group), a split that commits there
#   D. a loop of bucket moves while the three nodes run the statements with the option on and off
#   E. a long read transaction that holds the switch of a move back while the repartitioned join runs

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use POSIX ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql release_node);

my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my $KS = qr/KS001|bucket map|map changed|no longer what it was|failed while a distributed statement|already uses node|partitions of table|stale/;
my $RJ = 'Pushed: repartitioned join';

my @nodes = make_cluster('rm', 3, logical => 1, recovery_interval => 2,
	extra => "kingres_shard.enable_repartition_join = force\nkingres_shard.enable_partial_aggregation = on\nkingres_shard.move_cleanup_delay = 1\nmax_connections = 100\n");
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

# o (the moved table) is joined to cu (stays) by o.cust = cu.id, and to fr (stays, a colocation group of its own) by o.far = fr.id
q1($n1, q{
	CREATE TABLE public.o (id int PRIMARY KEY, cust int, far int, amount int, note text);
	SELECT kingres.create_distributed_table('public.o', 'id', bucket_count => 12);
	CREATE TABLE public.cu (id int PRIMARY KEY, region int, name text);
	SELECT kingres.create_distributed_table('public.cu', 'id', bucket_count => 10);
	CREATE TABLE public.fr (id int PRIMARY KEY, w int);
	SELECT kingres.create_distributed_table('public.fr', 'id', bucket_count => 8);
	INSERT INTO public.cu SELECT g, g % 5, 'c' || g FROM generate_series(1, 150) g;
	INSERT INTO public.fr SELECT g, g % 7 FROM generate_series(1, 90) g;
	INSERT INTO public.o SELECT g, CASE WHEN g % 9 = 0 THEN NULL ELSE 1 + g % 160 END, 1 + g % 100, g % 97, 'n' || (g % 20) FROM generate_series(1, 1500) g;
});
$_->safe_psql('postgres', 'ANALYZE') for @nodes;
my $CO = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.o'::regclass});
my $CC = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.cu'::regclass});
my $CF = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.fr'::regclass});
isnt($CC, $CO, 'cu is in a colocation group of its own, so is fr');

# the rows of the join of o with cu (o moves to the groups of cu), and the aggregate of it
my $SJ = q{SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust};
my $SA = q{SELECT cu.region, count(*), sum(o.amount) FROM public.o JOIN public.cu ON cu.id = o.cust GROUP BY cu.region};
my $SF = q{SELECT o.id, fr.w FROM public.o JOIN public.fr ON fr.id = o.far};
# J2: cu has no column that o is joined by on its key, so it is sent whole to every group (broadcast); o stays
my $SB = q{SELECT o.id, cu.name FROM public.o LEFT JOIN public.cu ON cu.id = o.cust + 1};
my $SBA = q{SELECT (o.amount % 7) AS k, count(cu.id), count(*) FROM public.o LEFT JOIN public.cu ON cu.id = o.cust + 1 GROUP BY 1};
my %SQL = (join => $SJ, agg => $SA, far => $SF, bcast => $SB, bcagg => $SBA);

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}
sub rows_of { my ($out) = @_; return join("\n", sort grep { /^[0-9]/ } split /\n/, $out); }
sub answer
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . $sql);
	return ($rc, rows_of($out), $err);
}
my $OFF = 'SET kingres_shard.enable_repartition_join = off; ';
sub clean_nodes
{
	my ($label) = @_;
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

# ---------------------------------------------------------------------------
# A. nothing happens
# ---------------------------------------------------------------------------
my %WANT;
for my $q ([ join => $SJ ], [ agg => $SA ], [ far => $SF ], [ bcast => $SB ], [ bcagg => $SBA ])
{
	my ($name, $sql) = @$q;
	like(plan_of($n1, $sql), qr/\Q$RJ\E/, "A: $name is repartitioned");
	my ($rcs, $scan, $es) = answer($n1, $sql, $OFF);
	is($rcs, 0, "A: $name runs by the scan") or diag($es);
	$WANT{$name} = $scan;
	for my $n (@nodes)
	{
		my ($rc2, $got, $e2) = answer($n, $sql);
		is($rc2, 0, "A: $name runs repartitioned on " . $n->name) or diag($e2);
		is($got, $scan, "A: ... and equals the scan's answer") or diag($e2);
	}
}
cmp_ok(scalar(split /\n/, $WANT{join}), '>', 1000, 'A: the join of the tests has many rows');

# one statement under the rule of the file: the baseline, or the retryable error
sub rule
{
	my ($label, $name, $rc, $got, $err) = @_;
	if ($rc == 0) { return is($got, $WANT{$name}, "$label: the answer is the baseline"); }
	return like($err, $KS, "$label: the statement failed with the retryable error");
}
sub all_three
{
	my ($label) = @_;
	for my $q ([ join => $SJ ], [ agg => $SA ], [ far => $SF ], [ bcast => $SB ], [ bcagg => $SBA ])
	{
		my ($name, $sql) = @$q;
		for my $n (@nodes)
		{
			my ($rc, $got, $err) = answer($n, $sql);
			is($rc, 0, "$label: $name on " . $n->name) or diag($err);
			is($got, $WANT{$name}, "$label: $name on " . $n->name . ' equals the baseline');
		}
		like(plan_of($n1, $sql), qr/\Q$RJ\E/, "$label: $name is still repartitioned");
	}
}

# ---------------------------------------------------------------------------
# B. splits
# ---------------------------------------------------------------------------
sub active_buckets
{
	my ($c) = @_;
	return split /\n/, q1($n1, "SELECT bucket || ':' || group_id FROM kingres.bucket_map WHERE colocation_id = $c AND state = 'active' ORDER BY bucket");
}
{
	# the colocation group of the tables that stay (cu): the destination of every moved row is by the map of this group
	my ($b) = (active_buckets($CC))[2] =~ /^(\d+):/;
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($CC, $b, 2)");
	is($rc, 0, "B: split_bucket of the group of cu (bucket $b)") or diag($err);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	all_three('B: after the split of the group that stays');

	($b) = (active_buckets($CC))[0] =~ /^(\d+):/;
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($CC, $b, 4)");
	is($rc, 0, "B: a second split, in four (bucket $b)") or diag($err);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	all_three('B: after the second split');

	# the colocation group of the moved table (o)
	($b) = (active_buckets($CO))[1] =~ /^(\d+):/;
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($CO, $b, 2)");
	is($rc, 0, "B: split_bucket of the group of the moved table (bucket $b)") or diag($err);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	all_three('B: after the split of the group of the moved table');

	# fr: the moved table o is routed by the map of fr
	($b) = (active_buckets($CF))[3] =~ /^(\d+):/;
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($CF, $b, 2)");
	is($rc, 0, "B: split_bucket of the group of fr (bucket $b)") or diag($err);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	all_three('B: after the split of the group of fr');
	is(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || coalesce(object, ''), '; '), '') FROM kingres.check_cluster()}), '', 'B: the cluster is clean');
}

# ---------------------------------------------------------------------------
# C. fault points
# ---------------------------------------------------------------------------
sub gate_run
{
	my ($point, $sql, $during, $tag) = @_;
	my $gate = "$tmp/gate_rm_$tag";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, "SET kingres_shard.fault_injection = '$point:wait:$gate'; $sql");
	ok(wait_for_fault_file("$gate.reached", 60), "the statement waits at $point ($tag)");
	my $mover = $during->();
	sleep(3) if $mover;
	unlink $gate;
	my ($out, $err) = finish_psql($sess);
	my ($mo, $me) = $mover ? finish_psql($mover) : ('', '');
	return (rows_of($out), $err, $me);
}
sub move_target
{
	my ($c, $i) = @_;
	my @b = active_buckets($c);
	my ($bk, $g) = $b[$i % @b] =~ /^(\d+):(\d+)/;
	my @groups = sort { $a <=> $b } keys %{ { map { /:(\d+)$/ ? ($1 => 1) : () } @b } };
	my ($dst) = grep { $_ != $g } @groups;
	return ($bk, $dst);
}

for my $point (qw(rp_after_ship rp_before_fragment))
{
	for my $name (qw(join agg bcast bcagg))
	{
		my $sql = $SQL{$name};
		my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:error'; $sql");
		isnt($rc, 0, "C: an error at $point fails the $name");
		like($err, qr/injected failure at \Q$point\E/, '... with that error (the point was reached)');
		my ($rc2, $got, $e2) = answer($n1, $sql);
		is($got, $WANT{$name}, "C: the next statement after the error at $point gives the baseline") or diag($e2);
	}
	clean_nodes("C: after the errors at $point");

	# a cancel at the wait
	my $gate = "$tmp/gate_rm_cancel_$point";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, "SET kingres_shard.fault_injection = '$point:wait:$gate'; SELECT count(*) FROM public.o JOIN public.cu ON cu.id = o.cust /* rm cancel */");
	ok(wait_for_fault_file("$gate.reached", 60), "C: the statement waits at $point");
	q1($n1, q{SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE query LIKE '%rm cancel%' AND query NOT LIKE '%pg_cancel_backend%' AND pid <> pg_backend_pid()});
	unlink $gate;
	my ($o2, $e2) = finish_psql($sess);
	like($e2, qr/canceling statement due to user request/, "C: a cancel at $point cancels the repartitioned join") or diag($e2);
	clean_nodes("C: after the cancel at $point");
}

# a move that commits between the shipping and the fragments: the moved table's group, the other group, each at both points
for my $point (qw(rp_after_ship rp_before_fragment))
{
	my $i = 0;
	for my $mv ([ 'the group of the tables that stay (cu)', $CC ], [ 'the group of the moved table (o)', $CO ], [ 'the group of fr', $CF ])
	{
		my ($label, $coloc) = @$mv;
		my $sql = $coloc == $CF ? $SF : $SJ;
		my $name = $coloc == $CF ? 'far' : 'join';
		my ($bk, $dst) = move_target($coloc, $i++);
		my ($got, $err, $me) = gate_run($point, $sql, sub { start_psql($n1, "SELECT kingres.move_bucket($coloc, $bk, $dst)") }, "mv${coloc}_$point");
		unlike($me, qr/ERROR/, "C: the move of bucket $bk of $label completes") or diag($me);
		if ($err =~ /ERROR/) { like($err, $KS, "C: a move of $label that commits at $point: the retryable error, no rows"); }
		else { is($got, $WANT{$name}, "C: a move of $label that commits at $point: the statement was right"); }
		my ($rc2, $g2, $e2) = answer($n1, $sql);
		is($g2, $WANT{$name}, "C: the statement repeated gives the baseline after the move of $label") or diag($e2);
	}
	# the broadcast statement: a move of the group of the table that stays (o) or of the table that is sent (cu)
	for my $mv ([ 'the group of the table that stays (o), broadcast', $CO ], [ 'the group of the table that is sent (cu), broadcast', $CC ])
	{
		my ($label, $coloc) = @$mv;
		my ($bk, $dst) = move_target($coloc, $i++);
		my ($got, $err, $me) = gate_run($point, $SB, sub { start_psql($n1, "SELECT kingres.move_bucket($coloc, $bk, $dst)") }, "mvb${coloc}_$point");
		unlike($me, qr/ERROR/, "C: the move of bucket $bk of $label completes") or diag($me);
		if ($err =~ /ERROR/) { like($err, $KS, "C: a move of $label that commits at $point: the retryable error, no rows"); }
		else { is($got, $WANT{bcast}, "C: a move of $label that commits at $point: the statement was right"); }
		my ($rc2, $g2, $e2) = answer($n1, $SB);
		is($g2, $WANT{bcast}, "C: the broadcast statement repeated gives the baseline after the move of $label") or diag($e2);
	}
	# a split that commits there
	my ($bk) = (active_buckets($CC))[0] =~ /^(\d+):/;
	my ($got, $err, $me) = gate_run($point, $SJ, sub { start_psql($n1, "SELECT kingres.split_bucket($CC, $bk, 2)") }, "split_$point");
	unlike($me, qr/ERROR/, "C: the split of bucket $bk completes") or diag($me);
	if ($err =~ /ERROR/) { like($err, $KS, "C: a split that commits at $point: the retryable error"); }
	else { is($got, $WANT{join}, "C: a split that commits at $point: the statement was right"); }
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	my ($rc2, $g2, $e2) = answer($n1, $SJ);
	is($g2, $WANT{join}, "C: after the split the statement gives the baseline") or diag($e2);
}
clean_nodes('C: at the end');

# ---------------------------------------------------------------------------
# D. moves in a loop while the three nodes run the statements, with the option on and off
# ---------------------------------------------------------------------------
{
	my $stop = "$tmp/stop_moves";
	unlink $stop;
	my $pid = fork();
	die "fork: $!" unless defined $pid;
	if ($pid == 0)
	{
		my $i = 0;
		while (!-e $stop && $i < 40)
		{
			my $coloc = ($CC, $CO, $CF)[$i % 3];
			my ($bk, $dst) = move_target($coloc, $i * 5);
			eval { $n1->safe_psql('postgres', "SELECT kingres.move_bucket($coloc, $bk, $dst)"); 1 };
			$i++;
		}
		POSIX::_exit(0);
	}
	my %count = (ok => 0, ks => 0, wrong => 0, other => 0);
	my $t0 = time;
	my $round = 0;
	while (time - $t0 < 120 && waitpid($pid, POSIX::WNOHANG()) == 0)
	{
		for my $q ([ join => $SJ ], [ agg => $SA ], [ far => $SF ], [ bcast => $SB ], [ bcagg => $SBA ])
		{
			my ($name, $sql) = @$q;
			for my $n (@nodes)
			{
				for my $pre ('', $OFF)
				{
					my ($rc, $got, $err) = answer($n, $sql, $pre);
					if ($rc == 0) { $got eq $WANT{$name} ? $count{ok}++ : do { $count{wrong}++; diag("a wrong answer of $name on " . $n->name . ($pre ? ' (option off)' : '')); }; }
					elsif ($err =~ $KS) { $count{ks}++; }
					else { $count{other}++; diag("an unexpected error: $err"); }
				}
			}
		}
		$round++;
	}
	waitpid($pid, 0);
	diag("D: $round rounds: $count{ok} answers, $count{ks} retryable errors");
	is($count{wrong}, 0, 'D: no answer returned a row twice or missed one');
	is($count{other}, 0, 'D: every failure is the retryable one');
	cmp_ok($count{ok}, '>', 0, 'D: statements were answered while the buckets moved');
	all_three('D: after the moves');
	clean_nodes('D: after the loop');
	# the orphan tables of the last moves are dropped by the recovery worker (every 2 s, after move_cleanup_delay), and check_cluster() reports one only when its drop is overdue by
	# twice the sum of node_timeout and recovery_interval; a machine under load can be slower than that for the last pass, so the check waits for the worker (a leak stays)
	$n1->poll_query_until('postgres', q{SELECT NOT EXISTS (SELECT 1 FROM kingres.check_cluster() WHERE check_name = 'orphan_table')});
	is(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || coalesce(object, ''), '; '), '') FROM kingres.check_cluster()}), '', 'D: the cluster is clean');
}

# ---------------------------------------------------------------------------
# E. a long read transaction holds the switch back
# ---------------------------------------------------------------------------
{
	my $reader = $n2->background_psql('postgres', on_error_stop => 0);
	$reader->query_safe('BEGIN ISOLATION LEVEL REPEATABLE READ');
	$reader->query_safe('SELECT count(*) FROM public.cu');
	$reader->query_safe('SELECT count(*) FROM public.o');
	my ($bk, $dst) = move_target($CC, 1);
	my $mover = start_psql($n1, "SELECT kingres.move_bucket($CC, $bk, $dst)");
	sleep(3);
	for my $q ([ join => $SJ ], [ agg => $SA ], [ bcast => $SB ])
	{
		my ($name, $sql) = @$q;
		my ($rc, $got, $err) = answer($n1, $sql);
		if ($rc == 0) { is($got, $WANT{$name}, "E: $name runs while the reader holds the switch back, and is right"); }
		else { like($err, $KS, "E: $name fails with the retryable error while the switch is held"); }
	}
	my $in = rows_of($reader->query_safe($SJ));
	is($in, $WANT{join}, 'E: the long transaction itself runs the repartitioned join and sees the baseline');
	$reader->query_safe('COMMIT');
	$reader->quit;
	my ($mo, $me) = finish_psql($mover);
	unlike($me, qr/ERROR/, 'E: the move completes after the reader ends') or diag($me);
	all_three('E: after the move');
	clean_nodes('E');
}

done_testing();
