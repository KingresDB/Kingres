# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/147_online_faults.pl: failures and crashes at every point of an online operation (design/shard-phase9.md,
# "Tests", t/043).  After each case the table is whole in exactly one layout (as it was, or distributed),
# its rows are the rows it had, nothing of the operation is left on any node (shadow tables, log, capture
# triggers, claim, prepared transactions) and check_cluster() is clean.
#
#   1. an error at every point before the switch commits: the operation is undone, the table is as it was
#   2. an error at the points after the switch: the table is distributed, the operation is finished
#   3. the session that runs the operation is killed at its points: the recovery worker undoes or finishes
#   4. the map group primary crashes inside a step and inside the commit of the swap
#   5. a node that is lost during the copy: its leftovers are cleaned when it is back

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Digest::MD5 qw(md5_hex);
use KingresShardTest qw(make_cluster run_sql prepared_count start_psql finish_psql wait_for_fault_file);

my ($n1, $n2, $n3) = make_cluster('of', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.rebuild_batch_rows = 50\nkingres_shard.rebuild_switch_timeout = 3000\n"
		. "kingres_shard.rebuild_lock_timeout = 100\nkingres_shard.node_timeout = 3\nkingres_shard.recovery_interval = 2\n");
	$n->reload;
}

my $case = 0;

sub new_table
{
	my ($name) = @_;
	$n1->safe_psql('postgres', qq{
		CREATE TABLE public.$name (tenant int NOT NULL, id int NOT NULL, v text, PRIMARY KEY (tenant, id));
		INSERT INTO public.$name SELECT g % 6, g, 'v' || g FROM generate_series(1, 220) g;
	});
	return md5_hex(q1($n1, "SELECT string_agg(tenant || ',' || id || ',' || v, ';' ORDER BY tenant, id) FROM public.$name"));
}

sub content
{
	my ($name) = @_;
	return md5_hex(q1($n1, "SELECT string_agg(tenant || ',' || id || ',' || v, ';' ORDER BY tenant, id) FROM public.$name"));
}

sub leftovers
{
	my $n = 0;
	for my $node (@nodes)
	{
		$n += q1($node, "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%' OR relname LIKE 'tlog\\_%'");
		$n += q1($node, "SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'kingres\\_capture%' OR tgname LIKE 'kingres\\_notrunc%'");
		$n += prepared_count($node);
	}
	$n += q1($n1, "SELECT count(*) FROM kingres.table_operation_lock");
	return $n;
}

# the test cluster does not restart a crashed node: the test starts it again
sub restart_n1
{
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
}

# wait until the node answers again and every operation is over
sub settle
{
	my ($label) = @_;
	$n1->poll_query_until('postgres', 'SELECT 1', '1');
	my $ok = $n1->poll_query_until('postgres',
		"SELECT count(*) = 0 FROM kingres.table_operation WHERE phase NOT IN ('done', 'failed', 'cancelled')");
	ok($ok, "$label: no operation is left unfinished");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
	for my $n (@nodes)
	{
		$n->poll_query_until('postgres', 'SELECT count(*) = 0 FROM pg_prepared_xacts');
	}
}

# the table is whole in one layout, with its rows, and nothing of the operation is left
sub check_state
{
	my ($label, $name, $want, $expect_layout) = @_;
	my $dist = q1($n1, "SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.$name'::regclass");
	is($dist, $expect_layout eq 'distributed' ? '1' : '0', "$label: the table is $expect_layout") if defined $expect_layout;
	is(content($name), $want, "$label: its rows are the rows it had");
	is(leftovers(), '0', "$label: nothing of the operation is left on any node");
	if ($dist eq '1')
	{
		my ($tot, $wrong) = (0, 0);
		for my $n (@nodes)
		{
			my ($a, $b) = split /,/, q1($n, "SELECT coalesce(sum(n), 0) || ',' || coalesce(sum(wrong), 0) FROM public.local_rows('public.$name', 'tenant')");
			$tot += $a; $wrong += $b;
		}
		is("$tot,$wrong", '220,0', "$label: the 220 rows are in the partitions of their buckets");
		is(q1($n2, "SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.$name'::regclass"), '1', "$label: and the other nodes know the table");
	}
	else
	{
		is(q1($n1, "SELECT count(*) FROM pg_class WHERE relname = '${name}_b0000'"), '0', "$label: no bucket partition of it exists");
	}
	is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', "$label: check_cluster() is clean");
}

$_->safe_psql('postgres', q{
	CREATE FUNCTION public.local_rows(t regclass, key name) RETURNS TABLE (part text, bucket int, n bigint, wrong bigint)
	LANGUAGE plpgsql AS $$
	DECLARE r record;
	BEGIN
	  FOR r IN SELECT c.oid::regclass AS p, (regexp_match(c.relname, '_b([0-9]{4})$'))[1]::int AS b
	             FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = t ORDER BY c.relname
	  LOOP
	    part := r.p::text; bucket := r.b;
	    EXECUTE format('SELECT count(*), count(*) FILTER (WHERE kingres.bucket_for(%L::regclass, %I) <> %s) FROM ONLY %s', t, key, r.b, r.p)
	       INTO n, wrong;
	    RETURN NEXT;
	  END LOOP;
	END $$;
}) for @nodes;

sub run_online
{
	my ($name, $fault) = @_;
	my $set = defined $fault ? "SET kingres_shard.fault_injection = '$fault'; " : '';
	return run_sql($n1, "${set}SELECT kingres.create_distributed_table_concurrently('public.$name', 'tenant', bucket_count => 12)");
}

# ---------------------------------------------------------------------------
# 1. an error before the switch commits
# ---------------------------------------------------------------------------
my $only = $ENV{T147_ONLY} // '';
my @before = $only ne '' ? () : qw(rebuild_after_preparing rebuild_after_shadow rebuild_after_capture rebuild_copy_chunk rebuild_after_copy
  rebuild_replay_chunk rebuild_before_switch rebuild_switch_after_lock rebuild_switch_after_final_replay
  rebuild_switch_before_swap rebuild_switch_after_local_swap rebuild_switch_after_remote_swap rebuild_switch_after_prepare
  before_prepare after_first_prepare);
for my $point (@before)
{
	my $name = 'e' . ++$case;
	my $want = new_table($name);
	# the faults of the commit protocol belong to the swap only (a chunk commits without them being named)
	my $spec = ($point =~ /^(before_prepare|after_first_prepare)$/) ? "$point\@tswitch:error" : "$point:error";
	my ($rc, $out, $err) = run_online($name, $spec);
	isnt($rc, 0, "error at $point: the call fails");
	like($err, qr/injected failure at \Q$point\E/, "error at $point: it is the injected error");
	check_state("error at $point", $name, $want, 'plain');
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = (SELECT max(op_id) FROM kingres.table_operation)"), 'failed', "error at $point: the operation is recorded as failed");
	# and the table can be distributed afterwards
	($rc, $out, $err) = run_online($name);
	is($rc, 0, "error at $point: the next call succeeds") or diag($err);
	check_state("error at $point, then done", $name, $want, 'distributed');
}

# ---------------------------------------------------------------------------
# 2. an error after the switch: the table is distributed, the operation is in cleaning and another call
#    finishes it
# ---------------------------------------------------------------------------
for my $point ($only ne '' ? () : qw(rebuild_after_switch rebuild_verify rebuild_cleanup))
{
	my $name = 'a' . ++$case;
	my $want = new_table($name);
	my ($rc, $out, $err) = run_online($name, "$point:error");
	isnt($rc, 0, "error at $point: the call fails");
	my $op = q1($n1, "SELECT max(op_id) FROM kingres.table_operation");
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'cleaning', "error at $point: the operation is in cleaning");
	is(q1($n1, "SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.$name'::regclass"), '1', "error at $point: the table is distributed");
	is(content($name), $want, "error at $point: its rows are the rows it had");
	is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', "error at $point: another call finishes it");
	if ($point eq 'rebuild_cleanup')
	{
		($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = 'rebuild_cleanup:error'; SELECT kingres.cleanup_table_operations(true)");
		isnt($rc, 0, 'error at rebuild_cleanup: cleanup_table_operations() fails');
		is(q1($n1, "SELECT count(*) FROM kingres.table_old_copy"), '1', 'error at rebuild_cleanup: the old copy is still recorded');
	}
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
	check_state("after $point", $name, $want, 'distributed');
}

# ---------------------------------------------------------------------------
# 3. the session that runs the operation is killed; the recovery worker resumes
# ---------------------------------------------------------------------------
for my $point ($only ne '' ? () : qw(rebuild_after_preparing rebuild_after_shadow rebuild_after_capture rebuild_after_copy rebuild_after_switch))
{
	my $name = 'k' . ++$case;
	my $want = new_table($name);
	my ($rc, $out, $err) = run_online($name, "$point:kill");
	isnt($rc, 0, "kill at $point: the session is gone");
	restart_n1();
	settle("kill at $point");
	my $layout = $point eq 'rebuild_after_switch' ? 'distributed' : 'plain';
	check_state("kill at $point", $name, $want, $layout);
	my $phase = q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = (SELECT max(op_id) FROM kingres.table_operation)");
	is($phase, $layout eq 'plain' ? 'failed' : 'done', "kill at $point: the worker " . ($layout eq 'plain' ? 'undid' : 'finished') . ' the operation');
	if ($layout eq 'plain')
	{
		($rc, $out, $err) = run_online($name);
		is($rc, 0, "kill at $point: the next call succeeds") or diag($err);
		$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
		check_state("kill at $point, then done", $name, $want, 'distributed');
	}
}

# ---------------------------------------------------------------------------
# 4. the map group primary crashes inside a step and inside the commit of the swap
# ---------------------------------------------------------------------------
my @crash = (
	['rebuild_copy_chunk', 'plain'], ['rebuild_replay_chunk', 'plain'], ['rebuild_switch_after_lock', 'plain'],
	['rebuild_switch_after_final_replay', 'plain'], ['rebuild_switch_after_local_swap', 'plain'],
	['rebuild_switch_after_remote_swap', 'plain'],
	['before_prepare@tswitch', 'plain'], ['after_first_prepare@tswitch', 'plain'], ['after_prepare@tswitch', 'plain'],
	['after_log@tswitch', undef], ['after_commit@tswitch', 'distributed'],
);
for my $c ($only ne '' ? () : @crash)
{
	my ($point, $layout) = @$c;
	my $name = 'c' . ++$case;
	my $want = new_table($name);
	# the replay needs something to replay
	my ($rc, $out, $err) = run_online($name, "$point:kill");
	isnt($rc, 0, "crash at $point: the call fails");
	restart_n1();
	settle("crash at $point");
	my $now = q1($n1, "SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.$name'::regclass") eq '1' ? 'distributed' : 'plain';
	if (defined $layout)
	{
		is($now, $layout, "crash at $point: the table is $layout");
	}
	else
	{
		pass("crash at $point: the table is $now (the decision was logged: either layout is right, not a mixture)");
	}
	check_state("crash at $point", $name, $want, $now);
	if ($now eq 'plain')
	{
		($rc, $out, $err) = run_online($name);
		is($rc, 0, "crash at $point: the next call succeeds") or diag($err);
		$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
		check_state("crash at $point, then done", $name, $want, 'distributed');
	}
}

# ---------------------------------------------------------------------------
# 5. a node that is lost while the operation is at a point: its leftovers are cleaned when it is back
# ---------------------------------------------------------------------------
my @lost_points = defined $ENV{T147_POINTS} ? split(/,/, $ENV{T147_POINTS}) : qw(rebuild_after_preparing rebuild_after_shadow
	rebuild_copy_chunk rebuild_replay_chunk rebuild_before_switch rebuild_switch_after_lock rebuild_switch_after_final_replay);
for my $point ($only ne '' && $only ne 'lost' ? () : @lost_points)
{
	my $name = 'n' . ++$case;
	my $want = new_table($name);
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_lost_${case}_$$";
	append_to_file($gate, 'x');
	# two -c: the online variants cannot run inside a transaction block, which "SET ...; SELECT ..." in one -c is
	my ($hout, $herr) = ('', '');
	my $hh = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
		'-c', "SET kingres_shard.fault_injection = '$point:wait:$gate'",
		'-c', "SELECT kingres.create_distributed_table_concurrently('public.$name', 'tenant', bucket_count => 12)" ],
		'>', \$hout, '2>', \$herr);
	my $h = { h => $hh, out => \$hout, err => \$herr };
	my $reached = wait_for_fault_file("$gate.reached", 60);
	ok($reached, "lost node at $point: the operation is held");
	$n3->stop('immediate');
	unlink $gate;
	unlink "$gate.reached";
	my ($out, $err) = finish_psql($h);
	isnt($err, '', "lost node at $point: the call fails");
	note("lost node at $point, the call said: $err");
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = (SELECT max(op_id) FROM kingres.table_operation)"), 'failed', "lost node at $point: the operation is failed");
	like(q1($n1, "SELECT spec->'cleanup_pending' FROM kingres.table_operation WHERE op_id = (SELECT max(op_id) FROM kingres.table_operation)"), qr/\[3\]|\[2, 3\]/,
		"lost node at $point: the node that could not be cleaned is recorded");
	is(q1($n1, "SELECT count(*) FROM kingres.table_operation_lock"), '0', "lost node at $point: the claim is released");
	is(content($name), $want, "lost node at $point: the table is as it was");
	is(q1($n1, "SELECT count(*) FROM kingres.check_cluster() WHERE node_id = 1 OR node_id IS NULL AND check_name <> 'node_unreachable'"), '0',
		"lost node at $point: nothing is left on the nodes that answer");
	$n3->start;
	$n3->poll_query_until('postgres', 'SELECT 1', '1');
	# the recovery worker may have done it first (the node is back): what counts is that nothing is pending afterwards
	q1($n1, "SELECT kingres.resume_table_operations()");
	ok($n1->poll_query_until('postgres', "SELECT jsonb_array_length(coalesce(spec->'cleanup_pending', '[]')) = 0 FROM kingres.table_operation WHERE op_id = (SELECT max(op_id) FROM kingres.table_operation)"),
		"lost node at $point: the operation has nothing pending after resume_table_operations()");
	is(q1($n3, "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%'"), '0', "lost node at $point: the shadow of the node is gone");
	check_state("lost node at $point", $name, $want, 'plain');
	my ($rc, $o, $e) = run_online($name);
	is($rc, 0, "lost node at $point: the next call succeeds") or diag($e);
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
	check_state("lost node at $point, then done", $name, $want, 'distributed');
}

done_testing();
