# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/148_online_cancel_exclusion.pl: cancel in every phase, resume by another session, and the exclusion of
# table operations, bucket moves and DDL (design/shard-phase9.md, "Tests", t/047).
#
#   1. a cancel while a session runs the operation, in every phase: it undoes itself, nothing is left
#   2. a cancel of an operation nobody runs, in every phase before the switch
#   3. a cancel after the switch only says so
#   4. another session continues an operation, and two sessions do not run one
#   5. a move of a claimed group, an operation on a group that moves, two operations on a group or a table
#   6. a statement planned before the swap meets the new table: a prepared statement is planned again

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count start_psql finish_psql wait_for_fault_file);

my ($n1, $n2, $n3) = make_cluster('ox', 3, logical => 1);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.rebuild_batch_rows = 50\nkingres_shard.rebuild_switch_timeout = 3000\n"
		. "kingres_shard.rebuild_lock_timeout = 100\nkingres_shard.node_timeout = 10\n");
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
	return $n + q1($n1, "SELECT count(*) FROM kingres.table_operation_lock");
}

sub begin_op
{
	my ($name, $with) = @_;
	my $cw = defined $with ? ", \"colocate_with\": \"" . q1($n1, "SELECT '$with'::regclass::oid") . "\"" : ", \"bucket_count\": 12";
	return q1($n1, "SELECT kingres.begin_table_operation('distribute', 'public.$name', '{\"column\": \"tenant\"$cw}')");
}

# ---------------------------------------------------------------------------
# 1. a cancel while a session runs the operation
# ---------------------------------------------------------------------------
my %phase_point = (
	shadow => 'rebuild_after_shadow', copying => 'rebuild_copy_chunk', catching_up => 'rebuild_replay_chunk',
);
for my $phase (qw(shadow copying catching_up))
{
	my $name = 'r' . ++$case;
	new_table($name);
	my $point = $phase_point{$phase};
	my $op = begin_op($name);
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_cancel_$case";
	append_to_file($gate, 'x');
	my $h = start_psql($n1, "SET kingres_shard.fault_injection = '$point:wait:$gate'; SELECT kingres.table_operation_advance($op, 'cleaning')");
	ok(wait_for_fault_file("$gate.reached", 60), "cancel in $phase: the operation is held at $point");
	my $c = start_psql($n1, "SELECT kingres.cancel_table_operation($op)");
	ok($n1->poll_query_until('postgres', "SELECT cancel_requested FROM kingres.table_operation WHERE op_id = $op"), "cancel in $phase: the request is recorded");
	unlink $gate;
	my ($out, $err) = finish_psql($h);
	like($err, qr/table operation $op was cancelled/, "cancel in $phase: the session that runs it stops with that message");
	finish_psql($c);
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'cancelled', "cancel in $phase: the operation is cancelled");
	is(leftovers(), '0', "cancel in $phase: nothing is left, the claim is released");
	is(q1($n1, "SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.$name'::regclass"), '0', "cancel in $phase: the table is not distributed");
	is(q1($n1, "SELECT count(*) FROM public.$name"), '220', "cancel in $phase: and has its rows");
	# TRUNCATE and DDL work again
	my ($rc, $o, $e) = run_sql($n1, "ALTER TABLE public.$name ADD COLUMN extra int");
	is($rc, 0, "cancel in $phase: DDL on the table is allowed again") or diag($e);
	is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', "cancel in $phase: check_cluster() is clean");
}

# the switching phase: attempts that cannot get the lock (a prepared transaction holds the source) go on until
# the cancel arrives; the cancel waits for the attempt that is running (they share the row of the operation), is
# seen between two attempts, and the session that runs the operation undoes it
{
	my $name = 'r' . ++$case;
	new_table($name);
	my $op = begin_op($name);
	is(q1($n1, "SELECT kingres.table_operation_advance($op, 'catching_up')"), 'switching', 'cancel in switching: ready for the switch');
	$n1->safe_psql('postgres', "BEGIN; INSERT INTO public.$name VALUES (1, 7000, 'held'); PREPARE TRANSACTION 'p93cancel';");
	my $h = start_psql($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')");
	ok($n1->poll_query_until('postgres', "SELECT attempts >= 1 FROM kingres.table_operation WHERE op_id = $op"), 'cancel in switching: the attempts fail on the prepared transaction');
	my $c = start_psql($n1, "SELECT kingres.cancel_table_operation($op)");
	ok($n1->poll_query_until('postgres', "SELECT cancel_requested FROM kingres.table_operation WHERE op_id = $op"), 'cancel in switching: the request is recorded between two attempts');
	# the undo needs the strongest lock on the table to drop the capture trigger: it waits for the prepared transaction
	sleep(1);
	$n1->safe_psql('postgres', "COMMIT PREPARED 'p93cancel'");
	my ($out, $err) = finish_psql($h);
	like($err, qr/table operation $op was cancelled/, 'cancel in switching: the session that runs it stops with that message');
	finish_psql($c);
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'cancelled', 'cancel in switching: the operation is cancelled');
	is(leftovers(), '0', 'cancel in switching: nothing is left, the claim is released');
	is(q1($n1, "SELECT count(*) FROM public.$name"), '221', 'cancel in switching: the table has its rows and the one of the prepared transaction');
	is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', 'cancel in switching: check_cluster() is clean');
}

# ---------------------------------------------------------------------------
# 2. a cancel of an operation that nobody runs
# ---------------------------------------------------------------------------
for my $upto (undef, 'shadow', 'copying', 'catching_up')
{
	my $name = 'i' . ++$case;
	new_table($name);
	my $op = begin_op($name);
	my $label = defined $upto ? "after $upto" : 'before the shadow';
	q1($n1, "SELECT kingres.table_operation_advance($op, '$upto')") if defined $upto;
	$n1->safe_psql('postgres', "SELECT kingres.cancel_table_operation($op)");
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'cancelled', "cancel $label: the operation is cancelled");
	is(leftovers(), '0', "cancel $label: nothing is left");
	is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', "cancel $label: check_cluster() is clean");
	# the claim of a new group is gone with the group
	is(q1($n1, "SELECT count(*) FROM kingres.colocation c WHERE NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.colocation_id = c.colocation_id)"), '0', "cancel $label: the group that the operation made is gone");
	# a cancel that is repeated is harmless
	$n1->safe_psql('postgres', "SELECT kingres.cancel_table_operation($op)");
}

# ---------------------------------------------------------------------------
# 3. a cancel after the switch
# ---------------------------------------------------------------------------
{
	my $name = 's' . ++$case;
	new_table($name);
	my $op = begin_op($name);
	is(q1($n1, "SELECT kingres.table_operation_advance($op, 'switching')"), 'cleaning', 'after the switch: the operation is in cleaning');
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.cancel_table_operation($op)");
	is($rc, 0, 'after the switch: a cancel is not an error');
	like($err, qr/has switched: nothing is undone/, 'after the switch: it says that nothing is undone');
	is(q1($n1, "SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.$name'::regclass"), '1', 'after the switch: the table stays distributed');
	is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'after the switch: the operation is finished');
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
}

# ---------------------------------------------------------------------------
# 4. another session continues; two sessions do not run one operation
# ---------------------------------------------------------------------------
{
	my $name = 'o' . ++$case;
	new_table($name);
	my $op = begin_op($name);
	is(q1($n1, "SELECT kingres.table_operation_advance($op, 'copying')"), 'catching_up', 'resume: the first session ends in catching_up');
	# a second, new session continues it
	is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'resume: a session of another process finishes it');
	is(q1($n1, "SELECT count(*) FROM public.$name"), '220', 'resume: the table has its rows');
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
	# the operation is over: a call changes nothing
	is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'a finished operation answers done');

	my $name2 = 'o' . ++$case;
	new_table($name2);
	my $op2 = begin_op($name2);
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_two_$case";
	append_to_file($gate, 'x');
	my $h = start_psql($n1, "SET kingres_shard.fault_injection = 'rebuild_copy_chunk:wait:$gate'; SELECT kingres.table_operation_advance($op2, 'cleaning')");
	ok(wait_for_fault_file("$gate.reached", 60), 'two sessions: the first is held in its copy');
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.table_operation_advance($op2, 'cleaning')");
	like($err, qr/table operation $op2 is being run by another session/, 'two sessions: the second is refused');
	my ($r2, undef, $err2) = run_sql($n1, "SELECT kingres.resume_table_operations()");
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op2"), 'copying', 'two sessions: resume leaves a live operation alone');
	unlink $gate;
	my ($o, $e) = finish_psql($h);
	is($e, '', 'two sessions: the first goes on') or diag($e);
	is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op2"), 'done', 'two sessions: and finishes');
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
}

# ---------------------------------------------------------------------------
# 5. moves, groups and tables
# ---------------------------------------------------------------------------
{
	new_table('base');
	q1($n1, "SELECT kingres.create_distributed_table_concurrently('public.base', 'tenant', bucket_count => 12)");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
	my $coloc = q1($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.base'::regclass");

	new_table('joiner');
	my $op = begin_op('joiner', 'public.base');
	is(q1($n1, "SELECT colocation_id FROM kingres.table_operation_lock WHERE op_id = $op"), $coloc, 'exclusion: the operation claims the group of base');
	# a move of a bucket of the claimed group is refused
	my $from = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $coloc AND bucket = 0");
	my $to = $from == 0 ? 1 : 0;
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.move_bucket($coloc, 0, $to)");
	like($err, qr/colocation group $coloc is used by table operation $op/, 'exclusion: move_bucket() refuses a claimed group');
	# a second operation on the group, a second operation on the table
	new_table('second');
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.begin_table_operation('distribute', 'public.second', '{\"column\": \"tenant\", \"colocate_with\": \"" . q1($n1, "SELECT 'public.base'::regclass::oid") . "\"}')");
	like($err, qr/colocation group $coloc is busy: a table operation uses it/, 'exclusion: a second operation on the group is refused');
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.begin_table_operation('distribute', 'public.joiner', '{\"column\": \"tenant\", \"bucket_count\": 6}')");
	like($err, qr/table "public.joiner" is already being rebuilt by table operation $op/, 'exclusion: a second operation on the table is refused');
	# the blocking variant does not take the group of an operation either
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.second', 'tenant', colocate_with => 'public.base')");
	like($err, qr/colocation group $coloc is busy: a table operation uses it/, 'exclusion: create_distributed_table() into a claimed group is refused');
	$n1->safe_psql('postgres', "SELECT kingres.cancel_table_operation($op)");
	is(leftovers(), '0', 'exclusion: the claim is released by the cancel');

	# a move in progress makes the operation refuse the group
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_move_$$";
	append_to_file($gate, 'x');
	my $mover = start_psql($n1, "SET kingres_shard.fault_injection = 'move_after_preparing:wait:$gate'; SELECT kingres.move_bucket($coloc, 0, $to)");
	if (wait_for_fault_file("$gate.reached", 30))
	{
		(undef, undef, $err) = run_sql($n1, "SELECT kingres.begin_table_operation('distribute', 'public.joiner', '{\"column\": \"tenant\", \"colocate_with\": \"" . q1($n1, "SELECT 'public.base'::regclass::oid") . "\"}')");
		like($err, qr/colocation group $coloc is busy|is being moved|is in state/, 'exclusion: an operation on a group with a bucket in motion is refused');
		unlink $gate;
		finish_psql($mover);
	}
	else
	{
		unlink $gate;
		finish_psql($mover);
		fail('exclusion: the move was held at move_after_preparing');
	}
	is(leftovers(), '0', 'exclusion: nothing is claimed after the move');
}

# ---------------------------------------------------------------------------
# 6. a statement planned before the swap
# ---------------------------------------------------------------------------
{
	my $name = 'p' . ++$case;
	new_table($name);
	my $s = $n1->background_psql('postgres', on_error_stop => 0);
	$s->query_safe("PREPARE pq AS SELECT count(*) FROM public.$name");
	$s->query_safe("PREPARE pi AS INSERT INTO public.$name VALUES (\$1, \$2, 'prepared')");
	is($s->query_safe("EXECUTE pq"), '220', 'a prepared query on the plain table');
	$s->query_safe("EXECUTE pi(1, 9001)");
	q1($n1, "SELECT kingres.create_distributed_table_concurrently('public.$name', 'tenant', bucket_count => 12)");
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
	my $c = $s->query("EXECUTE pq");
	my $e = $s->{stderr}; $s->{stderr} = '';
	if ($e =~ /ERROR/)
	{
		like($e, qr/KS001|no longer|changed|does not exist/, 'a plan made before the swap fails with a retryable error');
		$c = $s->query_safe("EXECUTE pq");
	}
	is($c, '221', 'the prepared query counts the 221 rows of the distributed table');
	$s->query("EXECUTE pi(2, 9002)");
	$e = $s->{stderr}; $s->{stderr} = '';
	$s->query_safe("EXECUTE pi(2, 9002)") if $e =~ /ERROR/;
	is(q1($n1, "SELECT count(*) FROM public.$name WHERE id IN (9001, 9002)"), '2', 'the prepared insert goes through the router after the swap');
	$s->quit;
}

# ---------------------------------------------------------------------------
# 7. the version handshake
# ---------------------------------------------------------------------------
{
	new_table('hv');
	$n3->safe_psql('postgres', "UPDATE pg_extension SET extversion = '0.6' WHERE extname = 'kingres_shard'");
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table_concurrently('public.hv', 'tenant')");
	like($err, qr/node 3 .*0\.6/s, 'handshake: a node with another version is named and the operation does not start');
	is(q1($n1, "SELECT count(*) FROM kingres.table_operation WHERE spec->>'table' = 'hv'"), '0', 'handshake: nothing is recorded');
	is(leftovers(), '0', 'handshake: nothing is left');
	$n3->safe_psql('postgres', "UPDATE pg_extension SET extversion = '0.7' WHERE extname = 'kingres_shard'");
	my ($rc, $o, $e) = run_sql($n1, "SELECT kingres.create_distributed_table_concurrently('public.hv', 'tenant')");
	is($rc, 0, 'handshake: with the version back the operation runs') or diag($e);
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
}

is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', 'check_cluster() is clean at the end');
done_testing();
