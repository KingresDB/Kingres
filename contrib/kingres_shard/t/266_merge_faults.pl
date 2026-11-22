# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/266_merge_faults.pl
#
# Phase 12, MERGE on distributed tables (design/analytics-phase12-queries.md, 7.7, 7.8 G4, 14.2): faults, cancels and moves of the
# colocated MERGE (M1) and of the MERGE with a routed source (M3).  The fault points are merge_before_first_group, merge_between_groups
# and merge_before_commit of fanout.c, fanout_after_run, and the commit points of 2a.  The rule in every section: a statement either gives
# the rows of the single statement, each in the group that owns its bucket, or fails (KS001 when the map changed) and leaves nothing: no
# row in any group, no prepared transaction (G4).
#
#   A. an error at every point, for both shapes
#   B. a cancel at every wait
#   C. a group that dies in the middle; the origin that dies after the prepare and after its own commit
#   D. a bucket that moves between the plan and the run, after the first group ran, after every group ran; a prepared statement
#      planned before a move

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql release_node);

my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my $ROWS = 240;
my $M1 = q{MERGE INTO public.dst USING public.src ON dst.id = src.id WHEN NOT MATCHED THEN INSERT (id, v) VALUES (src.id, src.v)};
my $M3 = q{MERGE INTO public.dst USING public.loc ON dst.id = loc.x WHEN NOT MATCHED THEN INSERT (id, v) VALUES (loc.x, loc.y)};
my $KS = qr/KS001|bucket map|map changed|no longer what it was|failed while a distributed statement|already uses node/;

# a cluster of three nodes with src and dst (colocated, 12 buckets, $ROWS rows in src) and a local table loc on node 1 with the same rows
sub setup3
{
	my ($prefix, %opt) = @_;
	my @n = make_cluster($prefix, 3, %opt);
	$n[0]->safe_psql('postgres', qq{
		CREATE TABLE public.src (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.src', 'id', bucket_count => 12);
		CREATE TABLE public.dst (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.dst', 'id', colocate_with => 'public.src');
		INSERT INTO public.src SELECT g, 'v' || g FROM generate_series(1, $ROWS) g;
		CREATE TABLE public.loc (x int, y text);
		INSERT INTO public.loc SELECT g, 'v' || g FROM generate_series(1, $ROWS) g;
	});
	$n[0]->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	return @n;
}

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

my $SRC_COLOC = q{(SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.src'::regclass)};

# The table holds $want rows, every one of them in the group that owns its bucket now, once; nothing is prepared.
sub clean_state
{
	my ($nodes, $table, $want, $label) = @_;
	my $n1 = $nodes->[0];
	is($n1->safe_psql('postgres', "SELECT count(*) FROM $table"), $want, "$label: $table holds $want rows");
	my ($all, $wrong) = (0, 0);
	my $i = 0;
	for my $nd (@$nodes)
	{
		my $mine = $nd->safe_psql('postgres', qq{SET kingres_shard.enable_routing = off;
			SELECT count(*) FROM $table k JOIN kingres.bucket_map m ON m.colocation_id = $SRC_COLOC AND m.bucket = kingres.bucket_for($SRC_COLOC, k.id) WHERE m.group_id = $i});
		my $total = $nd->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT count(*) FROM $table");
		$wrong += $total - $mine;
		$all += $total;
		$i++;
	}
	is($wrong, 0, "$label: every row is in the group that owns its bucket");
	is($all, $want, "$label: and in one group only");
	is(prepared_count($_), 0, "$label: nothing is prepared on " . $_->name) for @$nodes;
}

sub bucket_of
{
	my ($node, $group) = @_;
	return $node->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = $SRC_COLOC AND group_id = $group");
}

# Run $stmt on node 1 up to the fault point; run $during; let it go; returns (out, err).
sub with_gate
{
	my ($n1, $point, $stmt, $during, $tag, $pre) = @_;
	my $gate = "$tmp/gate_mf_$tag";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, ($pre // '') . " SET kingres_shard.fault_injection = '$point:wait:$gate'; $stmt");
	ok(wait_for_fault_file("$gate.reached", 60), "the statement waits at $point ($tag)");
	$during->();
	unlink $gate;
	return finish_psql($sess);
}

# ---------------------------------------------------------------------------
# A. an error at every point
# ---------------------------------------------------------------------------
my @nodes = setup3('mf');
my ($n1, $n2, $n3) = @nodes;
like(plan_of($n1, $M1), qr/Pushed: colocated MERGE/, 'the colocated statement of the tests is shipped whole');
like(plan_of($n1, $M3), qr/Pushed: MERGE with a routed source/, 'the routed statement of the tests is shipped with its source');
my @points = qw(merge_before_first_group merge_between_groups merge_before_commit fanout_after_run
				before_prepare after_first_prepare after_prepare after_log);
for my $shape (['colocated', $M1], ['routed', $M3])
{
	my ($sname, $stmt) = @$shape;
	for my $variant ('', ' RETURNING dst.id')
	{
		for my $point (@points)
		{
			my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:error'; $stmt$variant");
			isnt($rc, 0, "$sname: an error at $point fails the statement$variant");
			like($err, qr/injected failure at \Q$point\E/, '... with that error (the point was reached)');
			clean_state(\@nodes, 'public.dst', 0, "$sname: after an error at $point$variant");
			my ($rc2, $o2, $e2) = run_sql($n1, "$stmt$variant");
			is($rc2, 0, "$sname: the next statement works after $point") or diag($e2);
			clean_state(\@nodes, 'public.dst', $ROWS, "$sname: after the next statement ($point)");
			$n1->safe_psql('postgres', 'TRUNCATE public.dst');
		}
	}
}

# ---------------------------------------------------------------------------
# B. a cancel at every wait
# ---------------------------------------------------------------------------
{
	my $n = 0;
	for my $shape (['colocated', $M1], ['routed', $M3])
	{
		my ($sname, $stmt) = @$shape;
		for my $point (qw(merge_before_first_group merge_between_groups merge_before_commit))
		{
			my $gate = "$tmp/gate_mfb_" . $n++;
			append_to_file($gate, 'x');
			my $sess = start_psql($n1, "SET kingres_shard.fault_injection = '$point:wait:$gate';
				DO \$\$ BEGIN
				  BEGIN $stmt; EXCEPTION WHEN query_canceled THEN RAISE NOTICE 'cancelled at $point'; END;
				  $stmt;
				END \$\$");
			ok(wait_for_fault_file("$gate.reached", 30), "$sname: the statement waits at $point");
			my $pid = $n1->safe_psql('postgres',
				"SELECT pid FROM pg_stat_activity WHERE query LIKE '%$point:wait:$gate%' AND pid <> pg_backend_pid()");
			$n1->safe_psql('postgres', "SELECT pg_cancel_backend($pid)");
			unlink $gate;
			my ($out, $err) = finish_psql($sess);
			like($err, qr/cancelled at \Q$point\E/, "$sname: a cancel at $point is caught by the block");
			unlike($err, qr/ERROR/, '... and the transaction goes on without an error');
			clean_state(\@nodes, 'public.dst', $ROWS, "$sname: after a cancel at $point and the next statement");
			$n1->safe_psql('postgres', 'TRUNCATE public.dst');
		}
	}
}

# ---------------------------------------------------------------------------
# C. a group that dies; the origin that dies
# ---------------------------------------------------------------------------
{
	for my $shape (['colocated', $M1], ['routed', $M3])
	{
		my ($sname, $stmt) = @$shape;
		my ($out, $err) = with_gate($n1, 'merge_between_groups', $stmt, sub { $n3->stop('immediate') }, "die_$sname");
		isnt($err, '', "$sname: a group that died fails the statement");
		like($err, qr/connect|connection|lost|terminat|FATAL|server closed|failed/i, '... with a connection error');
		$n3->start;
		$n3->poll_query_until('postgres', 'SELECT true');
		clean_state(\@nodes, 'public.dst', 0, "$sname: after a group died in the middle of the statement");
	}
}
sub crash_origin
{
	my ($stmt, $point) = @_;
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:kill'; $stmt");
	isnt($rc, 0, "the origin dies at $point");
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
}
{
	crash_origin($M1, 'after_prepare');
	cmp_ok(prepared_count($n2) + prepared_count($n3), '>=', 1, 'transactions are left prepared on the groups');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	clean_state(\@nodes, 'public.dst', 0, 'after recovery of a crash before the decision was logged');
}
{
	crash_origin($M3, 'after_commit');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	clean_state(\@nodes, 'public.dst', $ROWS, 'after recovery of a crash after the local commit');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.src s JOIN public.dst d USING (id) WHERE s.v = d.v'), $ROWS,
		'... dst is the copy of src');
}
release_node($_) for @nodes;

# ---------------------------------------------------------------------------
# D. a bucket moves
# ---------------------------------------------------------------------------
for my $shape (['colocated', $M1], ['routed', $M3])
{
	my ($sname, $stmt) = @$shape;
	{
		# 1. between the other two groups, after the plan and before the first group runs
		my @m = setup3("md1$sname", logical => 1);
		my $m1 = $m[0];
		my $b = bucket_of($m1, 1);
		my ($out, $err) = with_gate($m1, 'merge_before_first_group', $stmt, sub { $m1->safe_psql('postgres', "SELECT kingres.move_bucket($SRC_COLOC, $b, 2)") }, "d1$sname");
		like($err, $KS, "$sname: a bucket that moved between the plan and the run fails the statement with the retryable error");
		clean_state(\@m, 'public.dst', 0, "$sname: after the failed statement");
		my ($rc, $o2, $e2) = run_sql($m1, $stmt);
		is($rc, 0, "$sname: the statement repeated is planned with the new map and works") or diag($e2);
		clean_state(\@m, 'public.dst', $ROWS, "$sname: after the repeat");
		release_node($_) for @m;
	}
	{
		# 2. a bucket of the origin's own group moves away while the statement waits after its first group
		my @m = setup3("md2$sname", logical => 1);
		my $m1 = $m[0];
		my $b = bucket_of($m1, 0);
		my $mover;
		my ($out, $err) = with_gate($m1, 'merge_between_groups', $stmt, sub {
				$mover = start_psql($m1, "SELECT kingres.move_bucket($SRC_COLOC, $b, 1)");
				sleep(4);
			}, "d2$sname");
		my ($mo, $me) = finish_psql($mover);
		unlike($me, qr/ERROR/, "$sname: the move completes") or diag($me);
		if ($err =~ /ERROR/)
		{
			like($err, $KS, "$sname: the statement failed with the retryable error");
			clean_state(\@m, 'public.dst', 0, "$sname: after the failed statement and the move");
		}
		else
		{
			pass("$sname: the statement succeeded (the switch waited for it)");
			clean_state(\@m, 'public.dst', $ROWS, "$sname: after the statement and the move");
		}
		release_node($_) for @m;
	}
	{
		# 3. a bucket moves into a group that the statement has written, after every group ran
		my @m = setup3("md3$sname", logical => 1);
		my $m1 = $m[0];
		my $b = bucket_of($m1, 2);
		my $mover;
		my ($out, $err) = with_gate($m1, 'merge_before_commit', $stmt, sub {
				$mover = start_psql($m1, "SELECT kingres.move_bucket($SRC_COLOC, $b, 1)");
				sleep(4);
			}, "d3$sname");
		my ($mo, $me) = finish_psql($mover);
		unlike($me, qr/ERROR/, "$sname: the move completes") or diag($me);
		if ($err =~ /ERROR/)
		{
			like($err, $KS, "$sname: the statement failed with the retryable error");
			clean_state(\@m, 'public.dst', 0, "$sname: after the failed statement and the move");
		}
		else
		{
			pass("$sname: the statement succeeded");
			clean_state(\@m, 'public.dst', $ROWS, "$sname: after the statement and the move");
		}
		release_node($_) for @m;
	}
	{
		# 4. a prepared statement planned before a move, executed after it
		my @m = setup3("md4$sname", logical => 1);
		my $m1 = $m[0];
		my $b = bucket_of($m1, 1);
		my $s = $m1->background_psql('postgres', on_error_stop => 0);
		$s->query_safe("PREPARE pm AS $stmt");
		$s->query_safe('EXECUTE pm') for (1 .. 6);
		$s->query_safe('DELETE FROM public.dst');
		$m1->safe_psql('postgres', "SELECT kingres.move_bucket($SRC_COLOC, $b, 2)");
		$s->query_safe('SET plan_cache_mode = force_generic_plan');
		$s->{stderr} = '';
		$s->query('EXECUTE pm');
		my $e = $s->{stderr};
		$s->{stderr} = '';
		if ($e =~ /ERROR/)
		{
			like($e, $KS, "$sname: the plan made before the move fails with the retryable error");
			$s->query('EXECUTE pm');
			is($s->{stderr}, '', "$sname: ... and works when repeated");
		}
		else
		{
			pass("$sname: the plan made before the move was right");
		}
		$s->quit;
		clean_state(\@m, 'public.dst', $ROWS, "$sname: a prepared statement planned before a move is planned again and works");
		release_node($_) for @m;
	}
}

done_testing();
