# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/184_engine_rules.pl: rules of the online engine that the first tests did not exercise (review of phase 9, P9B-09, P9B-10:
# the mutants M09, M10, M11, M12, M20 and M21 survived).
#
#   A. P9B-09: a role that holds only a column privilege cannot read the retained old copy
#   B. M09: a copy chunk that is repeated (the position of the copy was lost) does not fail on the rows it already stored
#   C. M10: the switch attempt takes the row of the operation, so a cancel and a switch cannot run together
#   D. M11, M12: the worker leaves alone an operation whose session is alive, and one that was written a moment ago
#   E. M20: the capture trigger refuses a log table that is not one of the operations'
#   F. M21: a switch that committed although its answer was lost is a switch

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql);

my ($n1, $n2) = make_cluster('er', 2);
my @nodes = ($n1, $n2);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.rebuild_batch_rows = 50\nkingres_shard.node_timeout = 5\n");
	$n->reload;
}

sub adv
{
	my ($op, $phase, $pre) = @_;
	my ($rc, $out, $err) = run_sql($n1, ($pre // '') . "SELECT kingres.table_operation_advance($op, '$phase')");
	$out =~ s/\s+$//;
	return $rc == 0 ? $out : "ERROR: $err";
}

sub new_table
{
	my ($t, $rows) = @_;
	$n1->safe_psql('postgres', "CREATE TABLE public.$t (id int PRIMARY KEY, v text, secret text); INSERT INTO public.$t SELECT g, 'v' || g, 's' FROM generate_series(1, " . ($rows // 200) . ") g");
}

sub begin_op
{
	my ($t) = @_;
	return q1($n1, "SELECT kingres.begin_table_operation('distribute', 'public.$t', '{\"column\": \"id\", \"bucket_count\": 6}')");
}

# ---------------------------------------------------------------------------
# A. a column privilege on the retained old copy
# ---------------------------------------------------------------------------
$_->safe_psql('postgres', 'CREATE ROLE col9') for @nodes;
new_table('pa');
$n1->safe_psql('postgres', 'GRANT SELECT (secret) ON public.pa TO col9');
is(q1($n1, q{SELECT has_column_privilege('col9', 'public.pa', 'secret', 'SELECT')}), 't', 'A: the role can read the column of the table');
{
	my $op = begin_op('pa');
	is(adv($op, 'cleaning'), 'done', 'A: the table is distributed online');
	my $old = q1($n1, "SELECT old_name FROM kingres.table_operation_item WHERE op_id = $op");
	ok($old ne '', "A: the old copy is kept ($old)");
	is(q1($n1, "SELECT has_column_privilege('col9', 'kingres.$old', 'secret', 'SELECT')"), 'f', 'A: the role cannot read the old copy');
	is(q1($n1, "SELECT has_any_column_privilege('col9', 'kingres.$old', 'SELECT')"), 'f', 'A: nor any other column of it');
	is(q1($n1, q{SELECT has_column_privilege('col9', 'public.pa', 'secret', 'SELECT')}), 't', 'A: the new table keeps the privilege');
}

# ---------------------------------------------------------------------------
# B. a repeated copy chunk
# ---------------------------------------------------------------------------
new_table('pb', 300);
{
	my $op = begin_op('pb');
	is(adv($op, 'copying'), 'catching_up', 'B: the copy is done');
	$n1->safe_psql('postgres', "UPDATE kingres.table_operation SET spec = spec - 'last' WHERE op_id = $op");
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.internal_op_copy_step($op)");
	is($rc, 0, 'B: the first chunk is copied a second time without an error') or diag($err);
	like($out, qr/"read": 50/, 'B: it read its rows again');
	is(adv($op, 'cleaning'), 'done', 'B: the operation ends');
	is(q1($n1, 'SELECT count(*) FROM public.pb'), '300', 'B: and the table has every row once');
}

# ---------------------------------------------------------------------------
# C. the switch attempt and the row of the operation
# ---------------------------------------------------------------------------
new_table('pc');
{
	my $op = begin_op('pc');
	is(adv($op, 'catching_up'), 'switching', 'C: the copy and the replay are done');
	# the attempt is held after it took the row of the operation and the lock of the table
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_pc_$$";
	append_to_file($gate, 'x');
	my ($hout, $herr) = ('', '');
	my $hh = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
		'-c', "SET kingres_shard.fault_injection = 'rebuild_switch_after_lock:wait:$gate'",
		'-c', "SELECT kingres.table_operation_advance($op, 'switching')" ], '>', \$hout, '2>', \$herr);
	ok(wait_for_fault_file("$gate.reached", 60), 'C: the attempt is held after its locks');
	# a request to stop cannot get the row of the operation meanwhile: the two are serialized
	my ($rc, $out, $err) = run_sql($n1, "SET lock_timeout = '1s'; SELECT kingres.internal_op_request_cancel($op)");
	isnt($rc, 0, 'C: a request to stop waits for the attempt (and runs into the lock timeout)');
	like($err, qr/lock timeout|could not obtain lock/, 'C: the reason is the lock on the row') or diag($err);
	unlink $gate;
	$hh->finish;
	is($herr, '', 'C: the attempt goes on when it is let go') or diag($herr);
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'cleaning', 'C: the switch happened');
	is(adv($op, 'cleaning'), 'done', 'C: the operation ends');
}

# ---------------------------------------------------------------------------
# D. the worker and an operation that is alive or was written a moment ago
# ---------------------------------------------------------------------------
new_table('pd1');
new_table('pd2');
{
	# D1: no session, written a moment ago: left alone
	my $op = begin_op('pd1');
	is(q1($n1, 'SELECT kingres.resume_table_operations()'), '0', 'D: an operation written a moment ago is left alone');
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'preparing', 'D: it is still preparing');
	$n1->safe_psql('postgres', "UPDATE kingres.table_operation SET updated_at = now() - interval '1 hour' WHERE op_id = $op");
	is(q1($n1, 'SELECT kingres.resume_table_operations()'), '1', 'D: when it is old and nobody runs it, it is undone');
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'failed', 'D: it is failed');

	# D2: a live session, written long ago: left alone
	my $op2 = begin_op('pd2');
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_pd2_$$";
	append_to_file($gate, 'x');
	my ($hout, $herr) = ('', '');
	my $hh = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
		'-c', "SET kingres_shard.fault_injection = 'rebuild_after_capture:wait:$gate'",
		'-c', "SELECT kingres.table_operation_advance($op2, 'copying')" ], '>', \$hout, '2>', \$herr);
	ok(wait_for_fault_file("$gate.reached", 60), 'D: an operation is running in a session');
	$n1->safe_psql('postgres', "UPDATE kingres.table_operation SET updated_at = now() - interval '1 hour' WHERE op_id = $op2");
	is(q1($n1, "SELECT kingres.internal_op_alive($op2)"), 't', 'D: its live mark is held');
	is(q1($n1, 'SELECT kingres.resume_table_operations()'), '0', 'D: an operation whose session is alive is left alone');
	isnt(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op2"), 'failed', 'D: it is not undone');
	unlink $gate;
	$hh->finish;
	is(adv($op2, 'cleaning'), 'done', 'D: and it goes on to the end');
}

# ---------------------------------------------------------------------------
# E. the log name of the capture trigger
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{CREATE TABLE public.pe (id int PRIMARY KEY); CREATE TABLE public.pe_victim (op char, k1 int)});
$n1->safe_psql('postgres', q{CREATE TRIGGER pe_t AFTER INSERT ON public.pe FOR EACH ROW EXECUTE FUNCTION kingres.internal_capture_trigger('public.pe_victim', 'k1', '($1).id')});
{
	my ($rc, $out, $err) = run_sql($n1, 'INSERT INTO public.pe VALUES (1)');
	isnt($rc, 0, 'E: a capture trigger with a log that is not one of the operations raises an error');
	like($err, qr/has no log table of table operations/, 'E: and says so');
	is(q1($n1, 'SELECT count(*) FROM public.pe_victim'), '0', 'E: nothing was written to the other table');
}

# ---------------------------------------------------------------------------
# G. a change that arrives while the attempt waits for its locks can move a unique value between two rows: the final replay
#    (in chunks of one entry) violates the unique index, the attempt is given up and the next one goes through
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.pg7 (tenant int NOT NULL, id int NOT NULL, email text, v text, PRIMARY KEY (tenant, id), UNIQUE (tenant, email));
	INSERT INTO public.pg7 VALUES (1, 1, 'a', 'x'), (1, 2, 'b', 'x');
});
{
	my $op = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.pg7', '{"column": "tenant", "bucket_count": 6}')});
	is(adv($op, 'catching_up'), 'switching', 'G: the copy and the replay are done');
	$_->safe_psql('postgres', 'ALTER DATABASE postgres SET kingres_shard.rebuild_batch_rows = 1') for @nodes;
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_pg7_$$";
	append_to_file($gate, 'x');
	my ($hout, $herr) = ('', '');
	my $hh = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
		'-c', "SET kingres_shard.fault_injection = 'rebuild_before_switch:wait:$gate'",
		'-c', "SELECT kingres.table_operation_advance($op, 'switching')" ], '>', \$hout, '2>', \$herr);
	ok(wait_for_fault_file("$gate.reached", 60), 'G: the attempt is held before it takes its locks');
	$n1->safe_psql('postgres', q{UPDATE public.pg7 SET v = 'y' WHERE id = 2; UPDATE public.pg7 SET email = 'z' WHERE id = 1; UPDATE public.pg7 SET email = 'a' WHERE id = 2;});
	unlink $gate;
	$hh->finish;
	is($herr, '', 'G: the call does not fail on the unique index') or diag($herr);
	like($hout, qr/cleaning/, 'G: the operation switched');
	is(adv($op, 'cleaning'), 'done', 'G: the operation ends');
	$_->safe_psql('postgres', 'ALTER DATABASE postgres RESET kingres_shard.rebuild_batch_rows') for @nodes;
	is(q1($n1, q{SELECT string_agg(id || ':' || email || ':' || v, ',' ORDER BY id) FROM public.pg7}), '1:z:x,2:a:y', 'G: the rows are as the writers left them');
}

# ---------------------------------------------------------------------------
# F. a switch that committed although its answer was lost
# ---------------------------------------------------------------------------
new_table('pf');
{
	my $op = begin_op('pf');
	is(adv($op, 'catching_up'), 'switching', 'F: the copy and the replay are done');
	# the attempt is held after its local commit (the swap and the record are durable, the other nodes are not told yet)
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_pf_$$";
	append_to_file($gate, 'x');
	my ($hout, $herr) = ('', '');
	my $hh = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
		'-c', "SET kingres_shard.fault_injection = 'after_commit:wait:$gate'",
		'-c', "SET kingres_shard.node_timeout = 1",
		'-c', "SELECT kingres.table_operation_advance($op, 'switching')" ], '>', \$hout, '2>', \$herr);
	ok(wait_for_fault_file("$gate.reached", 60), 'F: the attempt is held after its local commit');
	# the caller waits for the attempt for as long as the node timeouts allow, and gives up: the answer is lost, the switch is not
	$hh->finish;
	unlink $gate;
	note("F: the call said out=$hout err=$herr");
	is($herr, '', 'F: the call does not report a failed switch');
	like($hout, qr/cleaning/, 'F: it answers that the operation is in cleaning');
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'cleaning', 'F: the operation is in cleaning: the switch happened');
	# the transaction that was prepared on the other node is committed by the recovery
	q1($n1, 'SELECT kingres.recover_prepared_transactions()');
	ok($n1->poll_query_until('postgres', "SELECT relkind = 'p' FROM pg_class WHERE oid = 'public.pf'::regclass"), 'F: the table is distributed');
	is(adv($op, 'cleaning'), 'done', 'F: the operation can end');
	is(q1($n2, 'SELECT count(*) FROM public.pf'), q1($n1, 'SELECT count(*) FROM public.pf'), 'F: both nodes see the same rows');
}

is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for @nodes;
done_testing();
