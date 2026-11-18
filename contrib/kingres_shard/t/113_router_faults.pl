# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/113_router_faults.pl
#
# Step 3 of phase 2b (design/shard-phase2b.md, 11.4, 16.2 and 17): the failures of a bulk write.
# These are the router part of the design's 064.  The fault points are the bulk_* points of the
# batch sender (fault.c) and the commit points of 2a.
#
#   A. an error at every point of the sender: the statement fails with that error, no group holds a
#      row of it, nothing is prepared, and the session (and the next statement) works;
#   B. a cancel at every wait, inside a PL/pgSQL block that catches it, so that the same transaction
#      runs the next bulk insert on the connections the cancelled one used;
#   C. a node that does not read what is sent (the send is a latch wait that a cancel ends) and one
#      that does not answer (the wait for the answer is one too), both frozen with SIGSTOP;
#   D. a group that dies while the origin is between two groups; the origin that dies after the
#      prepare and after its own commit (recovery resolves them: nothing, then everything).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql);

my ($n1, $n2, $n3) = make_cluster('rf', 3);
$n1->safe_psql('postgres', q{
	CREATE TABLE public.fl (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.fl', 'id', bucket_count => 12);
});
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
my $tmp = $PostgreSQL::Test::Utils::tmp_check;

sub total
{
	return $n1->safe_psql('postgres', 'SELECT count(*) FROM public.fl');
}

sub clean_state
{
	my ($label, $want) = @_;
	is(total(), $want, "$label: the table holds $want rows");
	is(prepared_count($_), 0, "$label: nothing is prepared on " . $_->name) for ($n1, $n2, $n3);
}

my $ROWS = 90;
my $BULK = "SET kingres_shard.insert_batch_rows = 10; INSERT INTO public.fl SELECT g, 'f' || g FROM generate_series(1, $ROWS) g;";

# ---------------------------------------------------------------------------
# A. an error at every point
# ---------------------------------------------------------------------------
for my $point (qw(bulk_before_first_batch bulk_before_probe bulk_before_local_batch bulk_before_send bulk_after_send bulk_before_wait
				  bulk_after_first_batch bulk_before_last_flush bulk_between_groups bulk_after_last_batch
				  bulk_before_recheck bulk_before_commit before_prepare after_first_prepare after_prepare after_log))
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:error'; $BULK");
	isnt($rc, 0, "an error at $point fails the statement");
	like($err, qr/injected failure at \Q$point\E/, '... with that error (the point was reached)');
	clean_state("after an error at $point", 0);
	# a point of the sender that was reached on the origin leaves the origin usable at once
	my ($rc2, $o2, $e2) = run_sql($n1, $BULK);
	is($rc2, 0, "the next bulk insert works after $point") or diag($e2);
	is(total(), $ROWS, '... and inserts all its rows');
	$n1->safe_psql('postgres', 'TRUNCATE public.fl');
}

# an error at a point, and a later statement of the same session and transaction
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.insert_batch_rows = 10;
		DO \$\$ BEGIN
		  SET kingres_shard.fault_injection = 'bulk_between_groups:error';
		  BEGIN INSERT INTO public.fl SELECT g, 'a' FROM generate_series(1, $ROWS) g; EXCEPTION WHEN internal_error THEN RAISE NOTICE 'caught the injected error'; END;
		  SET kingres_shard.fault_injection = '';
		  INSERT INTO public.fl SELECT g, 'b' FROM generate_series(1, $ROWS) g;
		END \$\$");
	is($rc, 0, 'an injected error caught in a block, and a bulk insert in the same transaction') or diag($err);
	like($err, qr/caught the injected error/, '... the error was caught');
	clean_state('after the block', $ROWS);
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.fl WHERE v = 'a'}), 0, '... and only the rows of the second statement are there');
	$n1->safe_psql('postgres', 'TRUNCATE public.fl');
}

# ---------------------------------------------------------------------------
# B. a cancel at every wait
# ---------------------------------------------------------------------------
{
	my $n = 0;
	for my $point (qw(bulk_before_first_batch bulk_before_probe bulk_before_local_batch bulk_before_send bulk_after_send bulk_before_wait
					  bulk_before_last_flush bulk_between_groups bulk_after_last_batch bulk_before_recheck
					  bulk_before_commit))
	{
		my $gate = "$tmp/gate_rf_" . $n++;
		append_to_file($gate, 'x');
		my $sess = start_psql($n1, "SET kingres_shard.insert_batch_rows = 10;
			SET kingres_shard.fault_injection = '$point:wait:$gate';
			DO \$\$ BEGIN
			  BEGIN INSERT INTO public.fl SELECT g, 'a' FROM generate_series(1, $ROWS) g; EXCEPTION WHEN query_canceled THEN RAISE NOTICE 'cancelled at $point'; END;
			  INSERT INTO public.fl SELECT g, 'b' FROM generate_series(1, $ROWS) g;
			END \$\$");
		ok(wait_for_fault_file("$gate.reached", 30), "the bulk insert waits at $point");
		my $pid = $n1->safe_psql('postgres',
			"SELECT pid FROM pg_stat_activity WHERE query LIKE '%$point:wait:$gate%' AND pid <> pg_backend_pid()");
		$n1->safe_psql('postgres', "SELECT pg_cancel_backend($pid)");
		unlink $gate;
		my ($out, $err) = finish_psql($sess);
		like($err, qr/cancelled at \Q$point\E/, "a cancel at $point is caught by the block");
		unlike($err, qr/ERROR/, '... and the transaction goes on without an error');
		clean_state("after a cancel at $point and the next insert", $ROWS);
		is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.fl WHERE v = 'a'}), 0, '... only the rows of the second statement are there');
		is($n1->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction'"), 0,
			'no session of the origin is left in a transaction');
		$n1->safe_psql('postgres', 'TRUNCATE public.fl');
	}
	# a statement timeout in the middle of a bulk write
	my ($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.insert_batch_rows = 1; SET statement_timeout = 300;
		INSERT INTO public.fl SELECT g, 'slow' FROM generate_series(1, 100000) g WHERE pg_sleep(0.001) IS NOT NULL});
	like($err, qr/canceling statement due to statement timeout/, 'a statement timeout ends a bulk write');
	clean_state('after a statement timeout', 0);
	for my $nd ($n1, $n2, $n3)
	{
		unlike(slurp_file($nd->logfile), qr/terminated by signal|TRAP:/, 'no crash in the log of ' . $nd->name);
	}
}

# ---------------------------------------------------------------------------
# C. a node that does not read, and one that does not answer
# ---------------------------------------------------------------------------
sub frozen_node_pids
{
	my ($node) = @_;
	return split /\n/, $node->safe_psql('postgres', "SELECT pid FROM pg_stat_activity WHERE application_name = 'kingres_shard'");
}

for my $case (['bulk_before_send', 'a node that does not read what is sent'],
			  ['bulk_after_send', 'a node that does not answer'])
{
	my ($point, $what) = @$case;
	my $gate = "$tmp/gate_rf_freeze_$point";
	append_to_file($gate, 'x');
	# one batch of about 18 MB for each group: bigger than the socket buffers, so the send must wait
	my $sess = start_psql($n1, "SET kingres_shard.insert_batch_bytes = '64MB'; SET kingres_shard.insert_batch_rows = 100000;
		SET kingres_shard.fault_injection = '$point:wait:$gate';
		INSERT INTO public.fl SELECT g, repeat('z', 600000) FROM generate_series(1, 90) g;");
	ok(wait_for_fault_file("$gate.reached", 60), "the bulk insert waits at $point");
	my @pids = frozen_node_pids($n2);
	cmp_ok(scalar(@pids), '>=', 1, 'the session of the origin on node 2 is found');
	kill 'STOP', @pids;
	unlink $gate;
	sleep(1.5);
	my $pid = $n1->safe_psql('postgres',
		"SELECT pid FROM pg_stat_activity WHERE query LIKE '%$point:wait:$gate%' AND pid <> pg_backend_pid()");
	my $t0 = time;
	$n1->safe_psql('postgres', "SELECT pg_cancel_backend($pid)");
	my ($out, $err) = finish_psql($sess);
	my $took = time - $t0;
	kill 'CONT', @pids;
	like($err, qr/canceling statement due to user request/, "$what: a cancel ends the statement");
	cmp_ok($took, '<', 30, "... within a bounded time ($took seconds)");
	sleep(1);
	clean_state("after $what", 0);
}

# ---------------------------------------------------------------------------
# D. a group that dies; the origin that dies
# ---------------------------------------------------------------------------
{
	my $gate = "$tmp/gate_rf_die";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, "SET kingres_shard.insert_batch_rows = 10;
		SET kingres_shard.fault_injection = 'bulk_before_last_flush:wait:$gate';
		INSERT INTO public.fl SELECT g, 'f' FROM generate_series(1, $ROWS) g;");
	ok(wait_for_fault_file("$gate.reached", 30), 'the bulk insert waits before its last flush');
	$n3->stop('immediate');
	unlink $gate;
	my ($out, $err) = finish_psql($sess);
	isnt($err, '', 'a group that died fails the statement');
	like($err, qr/connect|connection|lost|terminat|FATAL|server closed|failed/i, '... with a connection error');
	$n3->start;
	$n3->poll_query_until('postgres', 'SELECT true');
	clean_state('after a group died in the middle of a write', 0);
	for my $nd ($n1, $n2) { is(prepared_count($nd), 0, 'nothing prepared'); }
}

sub crash_origin
{
	my ($point) = @_;
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.insert_batch_rows = 10; SET kingres_shard.fault_injection = '$point:kill';
		INSERT INTO public.fl SELECT g, 'f' FROM generate_series(1, $ROWS) g;");
	isnt($rc, 0, "the origin dies at $point");
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
}

{
	crash_origin('after_prepare');
	cmp_ok(prepared_count($n2) + prepared_count($n3), '>=', 1, 'transactions are left prepared on the groups');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	clean_state('after recovery of a crash before the decision was logged', 0);
}
{
	crash_origin('after_commit');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	clean_state('after recovery of a crash after the local commit', $ROWS);
	my $in_group = 0;
	for my $i (0 .. 2)
	{
		my $nd = ($n1, $n2, $n3)[$i];
		my $mine = $nd->safe_psql('postgres', qq{SET kingres_shard.enable_routing = off;
			SELECT count(*) FROM public.fl k JOIN kingres.bucket_map m ON m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, k.id) WHERE m.group_id = $i});
		my $all = $nd->safe_psql('postgres', 'SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.fl');
		is($mine, $all, 'every row of node ' . ($i + 1) . ' belongs to its group');
		$in_group += $all;
	}
	is($in_group, $ROWS, 'and every row is in exactly one group');
}

done_testing();
