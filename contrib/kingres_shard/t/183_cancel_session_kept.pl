# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/183_cancel_session_kept.pl: an online call that is cancelled (Ctrl-C, a statement timeout, pg_cancel_backend) while its client
# stays connected leaves nothing behind (review of phase 9, P9B-05).  The wrapper and the phase function caught every error but
# query_canceled, so the undo did not run and the live mark of the operation (a lock of the session) stayed: nobody could cancel it
# and every writer of the table paid for the capture until the client closed the session.
#
#   A. create_distributed_table_concurrently() cancelled at several points: the call ends with the cancel, the operation is
#      failed, the session is still there and holds nothing, the capture trigger and the log are gone, the table can be changed
#   B. the same for create_reference_table_concurrently()
#   C. table_operation_advance() cancelled: the live mark is released, so cancel_table_operation() from another session undoes

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file);

my ($n1, $n2) = make_cluster('cs', 2);
my @nodes = ($n1, $n2);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.rebuild_batch_rows = 100\nkingres_shard.node_timeout = 5\n");
	$n->reload;
}

sub no_leftovers
{
	my ($name, $t) = @_;
	is(q1($n1, "SELECT relkind FROM pg_class WHERE oid = 'public.$t'::regclass"), 'r', "$name: the table is the plain table");
	is(q1($_, "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%' OR relname LIKE 'tlog\\_%' OR relname ~ '^${t}_b[0-9]{4}\$'"), '0',
		"$name: no shadow, log or partition on " . $_->name) for @nodes;
	is(q1($n1, "SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'kingres\\_capture\\_%' OR tgname LIKE 'kingres\\_notrunc\\_%'"), '0', "$name: no capture trigger");
	is(q1($n1, 'SELECT count(*) FROM kingres.table_operation_lock'), '0', "$name: the group is not claimed");
	is(prepared_count($_), '0', "$name: nothing is prepared on " . $_->name) for @nodes;
}

# one session that stays: the call starts in it, a fault point holds it, another session cancels it
sub cancelled_call
{
	my ($name, $table, $call, $point) = @_;

	$n1->safe_psql('postgres', "CREATE TABLE public.$table (id int PRIMARY KEY, v text); INSERT INTO public.$table SELECT g, 'v' FROM generate_series(1, 200) g");
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_${table}_$$";
	append_to_file($gate, 'x');
	my $s = $n1->background_psql('postgres', on_error_stop => 0);
	my $pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$s->query_until(qr/started/, "\\echo started\nSET kingres_shard.fault_injection = '$point:wait:$gate';\nSELECT $call;\n");
	ok(wait_for_fault_file("$gate.reached", 60), "$name: the call is held at $point");
	is(q1($n1, "SELECT pg_cancel_backend($pid)"), 't', "$name: the call is cancelled");
	unlink $gate;
	my $out = $s->query("SELECT 'alive'");
	like($out, qr/alive/, "$name: the session is still there");
	ok($n1->poll_query_until('postgres', "SELECT NOT EXISTS (SELECT 1 FROM kingres.table_operation WHERE phase NOT IN ('failed', 'cancelled', 'done'))"),
		"$name: the operation ended");
	is(q1($n1, "SELECT count(*) FROM pg_locks WHERE pid = $pid AND locktype = 'advisory'"), '0', "$name: the session holds no live mark");
	no_leftovers($name, $table);
	is(q1($n1, "SELECT count(*) FROM public.$table"), '200', "$name: the rows are all there");
	$n1->safe_psql('postgres', "CREATE INDEX ON public.$table (v)");
	$s->quit;
}

cancelled_call('A shadow', 'ca1', "kingres.create_distributed_table_concurrently('public.ca1', 'id', bucket_count => 4)", 'rebuild_after_shadow');
cancelled_call('A capture', 'ca2', "kingres.create_distributed_table_concurrently('public.ca2', 'id', bucket_count => 4)", 'rebuild_after_capture');
cancelled_call('A copy', 'ca3', "kingres.create_distributed_table_concurrently('public.ca3', 'id', bucket_count => 4)", 'rebuild_after_copy');
cancelled_call('B capture', 'cb1', "kingres.create_reference_table_concurrently('public.cb1')", 'rebuild_after_capture');

# C. the phase function
$n1->safe_psql('postgres', "CREATE TABLE public.cc1 (id int PRIMARY KEY, v text); INSERT INTO public.cc1 SELECT g, 'v' FROM generate_series(1, 200) g");
{
	my $op = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.cc1', '{"column": "id", "bucket_count": 4}')});
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_cc1_$$";
	append_to_file($gate, 'x');
	my $s = $n1->background_psql('postgres', on_error_stop => 0);
	my $pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$s->query_until(qr/started/, "\\echo started\nSET kingres_shard.fault_injection = 'rebuild_after_capture:wait:$gate';\nSELECT kingres.table_operation_advance($op, 'copying');\n");
	ok(wait_for_fault_file("$gate.reached", 60), 'C: the phase function is held');
	is(q1($n1, "SELECT pg_cancel_backend($pid)"), 't', 'C: it is cancelled');
	unlink $gate;
	like($s->query("SELECT 'alive'"), qr/alive/, 'C: the session is still there');
	is(q1($n1, "SELECT kingres.internal_op_alive($op)"), 'f', 'C: the live mark is released');
	q1($n1, "SELECT kingres.cancel_table_operation($op)");
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'cancelled', 'C: the operation is cancelled');
	no_leftovers('C', 'cc1');
	$s->quit;
}

done_testing();
