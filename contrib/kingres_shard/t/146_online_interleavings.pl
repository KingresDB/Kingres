# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/146_online_interleavings.pl: forced interleavings of the online variant (design/shard-phase9.md,
# "Tests", t/042).  A fault point that waits holds an operation at a known moment while other sessions act:
#
#   A. a row inserted, updated and deleted between the copy's read and its insert: the replay repairs it
#   B. a writer that comes while the switch holds the lock waits, and goes on against the new table
#   C. a prepared transaction that holds the source delays the switch; the attempts give up, name it, and
#      the operation continues when it ends
#   D. a long reader delays the switch the same way
#   E. a writer with session_replication_role = replica is captured (the trigger is ENABLE ALWAYS)
#   F. a COPY into the source is captured
#   G. a reader of the source is never blocked by the capture or the copy

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_waiter start_psql finish_psql wait_for_fault_file);

my ($n1, $n2, $n3) = make_cluster('oi', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.rebuild_batch_rows = 100\nkingres_shard.rebuild_switch_timeout = 3000\n"
		. "kingres_shard.rebuild_lock_timeout = 100\nkingres_shard.node_timeout = 10\n");
	$n->reload;
}

sub new_table
{
	my ($name, $rows) = @_;
	$n1->safe_psql('postgres', qq{
		CREATE TABLE public.$name (tenant int NOT NULL, id int NOT NULL, v text, PRIMARY KEY (tenant, id));
		INSERT INTO public.$name SELECT g % 5, g, 'v' || g FROM generate_series(1, $rows) g;
	});
}

sub begin_op
{
	my ($name, @extra) = @_;
	return q1($n1, "SELECT kingres.begin_table_operation('distribute', 'public.$name', '{\"column\": \"tenant\", \"bucket_count\": 12}')");
}

# a session that runs SQL with a fault point that waits, in the background
sub with_gate
{
	my ($sql, $point, $during) = @_;
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_${point}_$$";
	append_to_file($gate, 'x');
	my $h = start_psql($n1, "SET kingres_shard.fault_injection = '$point:wait:$gate'; $sql");
	ok(wait_for_fault_file("$gate.reached", 60), "the operation is held at $point");
	$during->();
	unlink $gate;
	my ($out, $err) = finish_psql($h);
	return ($out, $err);
}

# ---------------------------------------------------------------------------
# A. changes between the read of a chunk and its insert
# ---------------------------------------------------------------------------
new_table('ia', 400);
my $op = begin_op('ia');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'shadow')"), 'copying', 'A: the shadow is made');
{
	my ($out, $err) = with_gate("SELECT kingres.table_operation_advance($op, 'copying')", 'bulk_before_first_batch', sub {
		# rows of the first chunk (keys 1..) are in the buffer of the sender now
		$n1->safe_psql('postgres', q{
			UPDATE public.ia SET v = 'changed' WHERE id = 3;
			DELETE FROM public.ia WHERE id = 4;
			INSERT INTO public.ia VALUES (2, 7000, 'new');
			UPDATE public.ia SET v = 'changed2' WHERE id = 350;
			DELETE FROM public.ia WHERE id = 351;
			UPDATE public.ia SET id = id + 100000 WHERE id = 6;
			UPDATE public.ia SET tenant = tenant + 1 WHERE id = 8;
		});
	});
	is($err, '', 'A: the copy went on after the changes') or diag($err);
}
is(q1($n1, "SELECT count(*) FROM kingres.tlog_${op}_1"), '9', 'A: nine entries are in the log (a change of a key logs the old key and the new one)');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'A: the operation ends');
is(q1($n1, "SELECT v FROM public.ia WHERE id = 3"), 'changed', 'A: the update of a row that was read is there');
is(q1($n1, "SELECT count(*) FROM public.ia WHERE id = 4"), '0', 'A: the ghost of a deleted row is gone');
is(q1($n1, "SELECT v FROM public.ia WHERE id = 7000"), 'new', 'A: the insert is there');
is(q1($n1, "SELECT v FROM public.ia WHERE id = 350"), 'changed2', 'A: an update after the first chunk is there');
is(q1($n1, "SELECT count(*) FROM public.ia WHERE id = 351"), '0', 'A: a delete after the first chunk is there');
is(q1($n1, "SELECT count(*) FROM public.ia"), '399', 'A: 400 - 2 deleted + 1 inserted rows');
is(q1($n1, "SELECT count(*) FROM public.ia WHERE id = 6"), '0', 'A: the old key of a changed key is gone');
is(q1($n1, "SELECT count(*) FROM public.ia WHERE id = 100006"), '1', 'A: and the new key is there');
is(q1($n1, "SELECT tenant FROM public.ia WHERE id = 8"), q1($n1, "SELECT (8 % 5) + 1"), 'A: a row whose distribution column changed is at its new place');
is(q1($n1, "SELECT count(*) FROM public.ia WHERE id = 8"), '1', 'A: and only there');
is(q1($n1, "SELECT verified FROM kingres.table_operation WHERE op_id = $op"), 'ok', 'A: and the verification agrees');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');

# ---------------------------------------------------------------------------
# B. a writer that comes while the switch holds the lock
# ---------------------------------------------------------------------------
new_table('ib', 200);
$op = begin_op('ib');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'catching_up')"), 'switching', 'B: everything before the switch is done');
{
	my $writer;
	my ($out, $err) = with_gate("SELECT kingres.table_operation_advance($op, 'switching')", 'rebuild_switch_after_final_replay', sub {
		$writer = start_psql($n1, "INSERT INTO public.ib VALUES (1, 9000, 'late'); SELECT 'written';");
		ok(wait_waiter($n1, 1), 'B: the writer waits for the lock of the switch');
		is(q1($n1, 'SELECT count(*) FROM public.ib'), '200', 'B: a reader is not blocked meanwhile, and does not see the row');
		is(q1($n1, "SELECT count(*) FROM public.ib WHERE id = 9000"), '0', 'B: the row is not there yet');
	});
	is($err, '', 'B: the switch went through') or diag($err);
	my ($wo, $we) = finish_psql($writer);
	is($we, '', 'B: the writer that waited went on against the new table') or diag($we);
	is(q1($n1, "SELECT count(*) FROM public.ib WHERE id = 9000"), '1', 'B: its row is in the new table');
	is(q1($n1, "SELECT kingres.bucket_for('public.ib'::regclass, 1) = (SELECT bucket FROM kingres.bucket_map m JOIN kingres.dist_table d ON d.colocation_id = m.colocation_id WHERE d.relid = 'public.ib'::regclass AND m.bucket = kingres.bucket_for('public.ib'::regclass, 1))"), 't', 'B: in the bucket of its key');
}
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'B: the operation ends');
is(q1($n1, "SELECT count(*) FROM public.ib"), '201', 'B: 201 rows');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');

# ---------------------------------------------------------------------------
# C. a prepared transaction holds the source
# ---------------------------------------------------------------------------
new_table('ic', 100);
$op = begin_op('ic');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'catching_up')"), 'switching', 'C: the replay is done');
$n1->safe_psql('postgres', "BEGIN; INSERT INTO public.ic VALUES (1, 5000, 'prepared'); PREPARE TRANSACTION 'p93hold';");
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.rebuild_switch_max_attempts = 2; SELECT kingres.table_operation_advance($op, 'switching')");
	is($out, 'switching', 'C: the operation stays in switching when the attempts are used up') or diag($err);
	my $detail = q1($n1, "SELECT detail FROM kingres.table_operation WHERE op_id = $op");
	like($detail, qr/attempt 2: could not lock table "?public\.ic"? in EXCLUSIVE mode/, 'C: the detail says what it could not lock');
	like($detail, qr/prepared transaction "p93hold"/, 'C: and names the prepared transaction that holds it');
	is(q1($n1, "SELECT attempts FROM kingres.table_operation WHERE op_id = $op"), '2', 'C: two attempts were made');
}
$n1->safe_psql('postgres', "COMMIT PREPARED 'p93hold'");
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'C: the operation ends after the prepared transaction did');
is(q1($n1, "SELECT count(*) FROM public.ic WHERE id = 5000"), '1', 'C: the row of the prepared transaction is in the new table');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');

# ---------------------------------------------------------------------------
# D. a long reader
# ---------------------------------------------------------------------------
new_table('id1', 100);
$op = begin_op('id1');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'catching_up')"), 'switching', 'D: the replay is done');
{
	my $reader = $n1->background_psql('postgres');
	$reader->query_safe('BEGIN');
	$reader->query_safe('SELECT count(*) FROM public.id1');
	my $pid = q1($n1, "SELECT pid FROM pg_stat_activity WHERE query LIKE 'SELECT count(*) FROM public.id1%' AND state = 'idle in transaction' LIMIT 1");
	my (undef, $out, $err) = run_sql($n1, "SET kingres_shard.rebuild_switch_max_attempts = 2; SELECT kingres.table_operation_advance($op, 'switching')");
	is($out, 'switching', 'D: a long reader delays the switch');
	like(q1($n1, "SELECT detail FROM kingres.table_operation WHERE op_id = $op"), qr/pid $pid /, 'D: the detail names its pid');
	$reader->query_safe('COMMIT');
	$reader->quit;
}
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'D: the operation ends when the reader does');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');

# ---------------------------------------------------------------------------
# E. replica role, F. COPY, G. readers
# ---------------------------------------------------------------------------
new_table('ie', 150);
# a role that has privileges on the table (roles are not synchronized: it exists on every node, or the report refuses)
$_->safe_psql('postgres', 'CREATE ROLE ox_w') for @nodes;
$n1->safe_psql('postgres', "GRANT SELECT, INSERT, UPDATE ON public.ie TO ox_w; GRANT USAGE ON SCHEMA public TO ox_w");
$op = begin_op('ie');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'shadow')"), 'copying', 'E: the capture exists');
$n1->safe_psql('postgres', "SET session_replication_role = replica; INSERT INTO public.ie VALUES (4, 8000, 'replica'); UPDATE public.ie SET v = 'rep' WHERE id = 10;");
is(q1($n1, "SELECT count(*) FROM kingres.tlog_${op}_1"), '2', 'E: changes under session_replication_role = replica are captured');
# a role that has no privilege on the log table writes the source: the capture function is SECURITY DEFINER
{
	my ($rc2, $o2, $e2) = run_sql($n1, "SET ROLE ox_w; INSERT INTO public.ie VALUES (4, 8200, 'by a role'); UPDATE public.ie SET v = 'role' WHERE id = 11");
	is($rc2, 0, 'E: a role with no privilege on the log writes the source without an error') or diag($e2);
	is(q1($n1, "SELECT count(*) FROM kingres.tlog_${op}_1"), '4', 'E: and its changes are logged');
	(undef, undef, $e2) = run_sql($n1, "SET ROLE ox_w; SELECT count(*) FROM kingres.tlog_${op}_1");
	like($e2, qr/permission denied/, 'E: the role cannot read the log');
	(undef, undef, $e2) = run_sql($n1, "SET ROLE ox_w; INSERT INTO kingres.tlog_${op}_1 (op, k1, k2) VALUES ('D', 1, 1)");
	like($e2, qr/permission denied/, 'E: or write it');
}
my ($rc, $cout, $cerr) = run_sql($n1, "COPY public.ie FROM STDIN;\n3\t8100\tcopied\n3\t8101\tcopied\n\\.\n");
is(q1($n1, "SELECT count(*) FROM kingres.tlog_${op}_1"), '6', 'F: the rows of a COPY are captured');
is(q1($n1, 'SELECT count(*) FROM public.ie'), '154', 'G: a reader sees the table as it is while the operation runs');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'E: the operation ends');
is(q1($n1, "SELECT count(*) || ',' || min(v) FROM public.ie WHERE id IN (8000, 8100, 8101, 10) AND v IN ('replica', 'copied', 'rep')"), '4,copied', 'E, F: all four changes are in the new table');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');

is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', 'check_cluster() is clean');
is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for @nodes;
is(q1($n1, "SELECT count(*) FROM kingres.table_operation_lock"), '0', 'no group is claimed');

done_testing();
