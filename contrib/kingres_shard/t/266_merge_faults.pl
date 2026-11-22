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
#   E. (review P12M-04) the same faults for the MERGE into a reference table (M4, M5): an error at every point, a cancel, a node that
#      dies, an origin that dies; nothing is prepared and every node's copy is the state before or the state after
#   F. (review P12M-03) a bucket that moves to a group that was not a target: after the source was read (only the origin's own group
#      runs) and before it is read (the source row of a group that was not planned)

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
		CREATE TABLE public.rsrc (k int PRIMARY KEY, v text);
		SELECT kingres.create_reference_table('public.rsrc');
			INSERT INTO public.rsrc VALUES @{[ join(", ", map { "($_, 'v$_')" } (1 .. $ROWS)) ]};
		CREATE TABLE public.rdst (k int PRIMARY KEY, v text);
		SELECT kingres.create_reference_table('public.rdst');
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
	my ($n1, $point, $stmt, $during, $tag, $pre, $mode) = @_;
	$mode //= 'wait';
	my $gate = "$tmp/gate_mf_$tag";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, ($pre // '') . " SET kingres_shard.fault_injection = '$point:$mode:$gate'; $stmt");
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

# ---------------------------------------------------------------------------
# E. the MERGE into a reference table: the same faults (review P12M-04)
# ---------------------------------------------------------------------------
# M4 reads a reference table, M5 reads a local table and ships it to every node.  The statement runs on every node; after any fault
# every node's copy is the state before (no row) or the state after ($ROWS rows, the same on every node), and nothing is prepared.
{
	my @rn = setup3('mr');
	my ($r1, $r2, $r3) = @rn;
	# M5 takes the writer lock of every table of the statement on every node, so the local source must exist on every node (see the report of the review fixes)
	$_->safe_psql('postgres', "CREATE TABLE public.loc (x int, y text); INSERT INTO public.loc SELECT g, 'v' || g FROM generate_series(1, $ROWS) g") for ($r2, $r3);
	my $R4 = q{MERGE INTO public.rdst USING public.rsrc ON rdst.k = rsrc.k WHEN NOT MATCHED THEN INSERT (k, v) VALUES (rsrc.k, rsrc.v)};
	my $R5 = q{MERGE INTO public.rdst USING public.loc ON rdst.k = loc.x WHEN NOT MATCHED THEN INSERT (k, v) VALUES (loc.x, loc.y)};
	like(plan_of($r1, $R4), qr/Remote SQL: \/\* kingres:reference-write \*\/ MERGE/, 'M4: the statement of the tests is written to every node');
	like(plan_of($r1, $R5), qr/Remote SQL: \/\* kingres:reference-write \*\/ MERGE.*unnest/is, 'M5: the statement of the tests ships its source to every node');
	my $after = $r1->safe_psql('postgres', q{SELECT count(*) || '/' || md5(string_agg(k || ':' || v, ',' ORDER BY k)) FROM public.rsrc});
	my $ref_state = sub {
		my ($nd) = @_;
		return $nd->safe_psql('postgres', q{SELECT count(*) || '/' || coalesce(md5(string_agg(k || ':' || v, ',' ORDER BY k)), '') FROM public.rdst});
	};
	my $ref_clean = sub {
		my ($want, $label) = @_;
		my $expect = $want ? $after : '0/';
		is($ref_state->($_), $expect, "$label: the copy on " . $_->name . ($want ? " is the state after" : " is the state before")) for @rn;
		is(prepared_count($_), 0, "$label: nothing is prepared on " . $_->name) for @rn;
	};
	my $ref_gate = sub {
		my ($node, $point, $stmt, $during, $tag) = @_;
		my $gate = "$tmp/gate_mr_$tag";
		append_to_file($gate, 'x');
		my $sess = start_psql($node, "SET kingres_shard.fault_injection = '$point:wait:$gate'; $stmt");
		ok(wait_for_fault_file("$gate.reached", 60), "the statement waits at $point ($tag)");
		$during->();
		unlink $gate;
		return finish_psql($sess);
	};

	# an error at every point
	for my $shape (['M4', $R4], ['M5', $R5])
	{
		my ($sname, $stmt) = @$shape;
		for my $variant ('', ' RETURNING rdst.k')
		{
			for my $point (@points)
			{
				my ($rc, $out, $err) = run_sql($r1, "SET kingres_shard.fault_injection = '$point:error'; $stmt$variant");
				isnt($rc, 0, "$sname: an error at $point fails the statement$variant");
				like($err, qr/injected failure at \Q$point\E/, '... with that error (the point was reached)');
				$ref_clean->(0, "$sname: after an error at $point$variant");
				my ($rc2, $o2, $e2) = run_sql($r1, "$stmt$variant");
				is($rc2, 0, "$sname: the next statement works after $point") or diag($e2);
				$ref_clean->(1, "$sname: after the next statement ($point)");
				$r1->safe_psql('postgres', 'DELETE FROM public.rdst');
			}
		}
	}

	# a cancel at every wait
	{
		my $n = 0;
		for my $shape (['M4', $R4], ['M5', $R5])
		{
			my ($sname, $stmt) = @$shape;
			for my $point (qw(merge_before_first_group merge_between_groups merge_before_commit))
			{
				my $gate = "$tmp/gate_mrb_" . $n++;
				append_to_file($gate, 'x');
				my $sess = start_psql($r1, "SET kingres_shard.fault_injection = '$point:wait:$gate';
					DO \$\$ BEGIN
					  BEGIN $stmt; EXCEPTION WHEN query_canceled THEN RAISE NOTICE 'cancelled at $point'; END;
					  $stmt;
					END \$\$");
				ok(wait_for_fault_file("$gate.reached", 30), "$sname: the statement waits at $point");
				my $pid = $r1->safe_psql('postgres',
					"SELECT pid FROM pg_stat_activity WHERE query LIKE '%$point:wait:$gate%' AND pid <> pg_backend_pid()");
				$r1->safe_psql('postgres', "SELECT pg_cancel_backend($pid)");
				unlink $gate;
				my ($out, $err) = finish_psql($sess);
				like($err, qr/cancelled at \Q$point\E/, "$sname: a cancel at $point is caught by the block");
				unlike($err, qr/ERROR/, '... and the transaction goes on without an error');
				$ref_clean->(1, "$sname: after a cancel at $point and the next statement");
				$r1->safe_psql('postgres', 'DELETE FROM public.rdst');
			}
		}
	}

	# a node that dies in the middle
	for my $shape (['M4', $R4], ['M5', $R5])
	{
		my ($sname, $stmt) = @$shape;
		my ($out, $err) = $ref_gate->($r1, 'merge_between_groups', $stmt, sub { $r3->stop('immediate') }, "die_$sname");
		isnt($err, '', "$sname: a node that died fails the statement");
		like($err, qr/connect|connection|lost|terminat|FATAL|server closed|failed/i, '... with a connection error');
		$r3->start;
		$r3->poll_query_until('postgres', 'SELECT true');
		$ref_clean->(0, "$sname: after a node died in the middle of the statement");
	}

	# the origin dies after the prepare, and after its own commit
	for my $x (['M4', $R4, 'after_prepare', 0], ['M5', $R5, 'after_commit', 1])
	{
		my ($sname, $stmt, $point, $want) = @$x;
		my ($rc, $out, $err) = run_sql($r1, "SET kingres_shard.fault_injection = '$point:kill'; $stmt");
		isnt($rc, 0, "$sname: the origin dies at $point");
		$r1->stop('immediate', fail_ok => 1);
		$r1->start;
		$r1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
		$ref_clean->($want, "$sname: after recovery of a crash at $point");
		$r1->safe_psql('postgres', 'DELETE FROM public.rdst');
	}
	release_node($_) for @rn;
}

# ---------------------------------------------------------------------------
# F. a bucket moves to a group that was not a target (review P12M-03)
# ---------------------------------------------------------------------------
# Group 2 is emptied first (its buckets go to group 1), so no bucket of the colocation group is on it; then a bucket of group 1 moves
# to group 2 while the statement waits.  A statement planned before the move cannot be right about that bucket: it fails with the
# retryable error and changes nothing.
#   first_group: the point after the source was read; the source holds keys of group 0 only (this node), so group 1 is skipped, and
#                the statement runs on the origin's own group only
#   collect:     the point before the source is read; the source holds two keys of the bucket that moves, whose group is now one
#                that the statement was not planned for
for my $kind (['first_group', 'merge_before_first_group'], ['collect', 'merge_before_collect'])
{
	my ($tag, $point) = @$kind;
	my @m = setup3("mz$tag", logical => 1);
	my $m1 = $m[0];
	my $b2 = $m1->safe_psql('postgres', "SELECT string_agg(bucket::text, ' ') FROM kingres.bucket_map WHERE colocation_id = $SRC_COLOC AND group_id = 2");
	$m1->safe_psql('postgres', "SELECT kingres.move_bucket($SRC_COLOC, $_, 1)") for split / /, $b2;
	is($m1->safe_psql('postgres', "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $SRC_COLOC AND group_id = 2"), 0,
		"$tag: group 2 owns no bucket of the colocation group");
	my $victim = bucket_of($m1, 1);
	my @k0 = KingresShardTest::keys_of_group($m1, $SRC_COLOC, 0, 3, 1);
	my @kv;
	if ($tag eq 'collect')
	{
		my $keys = $m1->safe_psql('postgres', "SELECT string_agg(g::text, ' ') FROM (SELECT g FROM generate_series(1, 1000) g WHERE kingres.bucket_for($SRC_COLOC, g) = $victim ORDER BY g LIMIT 2) q");
		@kv = split / /, $keys;
	}
	my $list = join(', ', map { "($_, 'k$_')" } (@k0, @kv));
	my $stmt = qq{MERGE INTO public.dst USING (VALUES $list) AS v(k, name) ON dst.id = v.k WHEN NOT MATCHED THEN INSERT (id, v) VALUES (v.k, v.name)};
	like(plan_of($m1, $stmt), qr/Pushed: MERGE with a routed source/, "$tag: the statement is a MERGE with a routed source");
	my ($out, $err) = with_gate($m1, $point, $stmt, sub { $m1->safe_psql('postgres', "SELECT kingres.move_bucket($SRC_COLOC, $victim, 2)") },
		"z$tag", '', $tag eq 'collect' ? 'waitinv' : 'wait');
	like($err, $KS, "$tag: the statement fails with the retryable error");
	like($err, qr/the bucket map changed while the source of a MERGE was read/,
		"$tag: it is the check of the source rows (a row of a group that was not planned is never dropped)") if $tag eq 'collect';
	clean_state(\@m, 'public.dst', 0, "$tag: after the failed statement");
	my ($rc, $o2, $e2) = run_sql($m1, $stmt);
	is($rc, 0, "$tag: the statement repeated is planned with the new map and works") or diag($e2);
	clean_state(\@m, 'public.dst', scalar(@k0) + scalar(@kv), "$tag: after the repeat");
	release_node($_) for @m;
}

done_testing();
