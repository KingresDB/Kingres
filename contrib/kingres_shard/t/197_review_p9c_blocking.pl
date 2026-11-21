# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/197_review_p9c_blocking.pl: the blocking undistribute and alter against the findings of the review of increments 9.4, 9.5, 9.8 and
# 9.9 (reviews/2026-11-21-kingres-shard-phase9-increments-4-9.md).
#
#   A. P9C-01  a REPEATABLE READ or SERIALIZABLE transaction is refused before the lock, with an open writer, with the default isolation
#              of the session, and in the copy itself; in READ COMMITTED a writer that commits while the call waits for its lock is in
#              the new table (count, sum and a digest of every row)
#   B. P9C-02  tables of tens of thousands of rows: blocking alter, undistribute to another group, a reference table
#   C. P9C-04  a function of a role that is not a superuser, reached through a table that the call takes with it
#   D. P9C-06  alter colocate_with a group that a bucket move or a table operation uses, or that has a bucket that is not active
#   E. P9C-07  a deferrable unique constraint: the online forms refuse before they start
#   F. P9C-08  the storage parameters of the TOAST table are carried (distribute, alter, undistribute)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_waiter start_psql finish_psql wait_for_fault_file move_state);

my ($n1, $n2, $n3) = make_cluster('bl', 3, logical => 1);
my @nodes = ($n1, $n2, $n3);
# a query that fails is a wrong answer, not the end of the test
sub q1 { my ($node, $sql) = @_; my ($rc, $out, $err) = run_sql($node, $sql); chomp $out; return $rc == 0 ? $out : "ERROR: $err"; }

sub dig
{
	my ($node, $t) = @_;
	return q1($node, "SELECT count(*) || '|' || sum(id) || '|' || md5(string_agg(tenant || ':' || id || ':' || coalesce(v::text, '-'), ',' ORDER BY tenant, id)) FROM public.$t");
}

# the call waits for the lock of the writer on one of the nodes
sub wait_any_waiter
{
	for (my $i = 0; $i < 300; $i++)
	{
		for my $n (@nodes)
		{
			return 1 if q1($n, q{SELECT count(*) > 0 FROM pg_locks WHERE NOT granted}) eq 't';
		}
		select(undef, undef, undef, 0.1);
	}
	return 0;
}

sub plain_table
{
	my ($t, $rows, $buckets) = @_;
	q1($n1, "CREATE TABLE public.$t (tenant int NOT NULL, id int NOT NULL, v text, PRIMARY KEY (tenant, id)); "
		. "INSERT INTO public.$t SELECT g % 7, g, 'row ' || g || repeat('x', g % 30) FROM generate_series(1, $rows) g; "
		. "SELECT kingres.create_distributed_table('public.$t', 'tenant', bucket_count => $buckets)");
}

# ---------------------------------------------------------------------------
# A. P9C-01: the isolation level, and the writer that commits while the call waits
# ---------------------------------------------------------------------------
my @calls = (
	['alter',  'kingres.alter_distributed_table()', "SELECT kingres.alter_distributed_table('public.%s', bucket_count => 8)"],
	['undist', 'kingres.undistribute_table()', "SELECT kingres.undistribute_table('public.%s')"],
);
for my $c (@calls)
{
	my ($name, $fn, $tpl) = @$c;

	# read committed: the writer that is open when the call starts commits while the call waits for the lock on its node
	my $t = "rc_$name";
	plain_table($t, 100, 4);
	my $before = dig($n1, $t);
	my $w = $n2->background_psql('postgres');
	$w->query_safe("BEGIN; INSERT INTO public.$t SELECT g % 7, g, 'late ' || g FROM generate_series(1001, 1050) g;");
	my $a = start_psql($n1, sprintf($tpl, $t));
	ok(wait_any_waiter(), "A $name: the call waits for the lock of the writer");
	$w->query_safe('COMMIT;');
	$w->quit;
	my ($o, $e) = finish_psql($a);
	is($e, '', "A $name: the call succeeds in READ COMMITTED") or diag($e);
	is(q1($n1, "SELECT count(*) || '|' || sum(id) FROM public.$t"), '150|56325', "A $name: the 50 rows that the writer committed during the wait are in the table");
	is(q1($n1, "SELECT count(*) FROM public.$t WHERE v LIKE 'late %'"), '50', "A $name: with their values");

	# repeatable read and serializable: refused with the reason, before the lock; nothing changed, the writer's rows are not lost
	for my $level ('REPEATABLE READ', 'SERIALIZABLE')
	{
		$t = 'iso_' . $name . '_' . ($level =~ /^R/ ? 'rr' : 'ser');
		plain_table($t, 100, 4);
		$w = $n2->background_psql('postgres');
		$w->query_safe("BEGIN; INSERT INTO public.$t SELECT g % 7, g, 'late ' || g FROM generate_series(1001, 1050) g;");
		# the writer commits while the call is made (before the fix the call waited for it, then copied the rows as of its own old snapshot)
		my $a2 = start_psql($n1, "BEGIN ISOLATION LEVEL $level; SELECT 1; " . sprintf($tpl, $t) . '; COMMIT;');
		select(undef, undef, undef, 1.5);
		$w->query_safe('COMMIT;');
		$w->quit;
		my ($o2, $err) = finish_psql($a2);
		isnt($err, '', "A $name: refused in $level");
		like($err, qr/\Q$fn\E cannot run in a REPEATABLE READ or SERIALIZABLE transaction/, "A $name: $level: with the reason");
		is(q1($n1, "SELECT count(*) || '|' || sum(id) FROM public.$t"), '150|56325', "A $name: $level: the writer's rows are in the table");
		is(q1($n1, "SELECT count(*) || '|' || (SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.$t'::regclass) FROM pg_class WHERE relname LIKE 'kingres\\_old\\_%' OR relname LIKE 'kingres\\_shadow\\_%'"),
		   '0|1', "A $name: $level: the table is distributed as before, nothing is left over");
	}

	# the isolation level of the session, with a single statement
	$t = "def_$name";
	plain_table($t, 100, 4);
	{
		local $ENV{PGOPTIONS} = '-c default_transaction_isolation=serializable';
		my ($rc, $out, $err) = run_sql($n1, sprintf($tpl, $t));
		isnt($rc, 0, "A $name: a call in a session whose default is SERIALIZABLE is refused");
		like($err, qr/\Q$fn\E cannot run in a REPEATABLE READ or SERIALIZABLE transaction/, "A $name: with the reason");
	}
	is(q1($n1, "SELECT count(*) FROM public.$t"), '100', "A $name: and unchanged");

	# the SQLSTATE is 25001 (active_sql_transaction), as for the create call (P9F-04)
	{
		my ($rc, $out, $err) = run_sql($n1, "BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1; " . sprintf($tpl, "rc_$name") . '; COMMIT;', extra_params => [ '-v', 'VERBOSITY=verbose' ]);
		like($err, qr/ERROR:  25001:/, "A $name: the SQLSTATE of the refusal is 25001");
	}
}

# the copy itself refuses a transaction snapshot: the last line, whoever calls it
{
	plain_table('cp_rr', 20, 4);
	my ($rc, $out, $err) = run_sql($n1, q{BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1; SELECT kingres.internal_copy_routed('public.cp_rr'::regclass, 'public', 'nowhere', 'every_node'); COMMIT;});
	isnt($rc, 0, 'A: the copy function is refused in REPEATABLE READ');
	like($err, qr/cannot run in a REPEATABLE READ or SERIALIZABLE transaction/, 'A: with the reason, before it looks for the destination');
	unlike($err, qr/nowhere/, 'A: the destination is not looked at');
}

# ---------------------------------------------------------------------------
# B. P9C-02: tens of thousands of rows
# ---------------------------------------------------------------------------
plain_table('big', 30000, 8);
{
	my $d = dig($n1, 'big');
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table('public.big', bucket_count => 12)});
	is($rc, 0, 'B: blocking alter of 30000 rows') or diag($err);
	is(dig($n1, 'big'), $d, 'B: every row is there after the alter (count, sum, digest)');
	is(dig($n3, 'big'), $d, 'B: and the same from another node');
	is(q1($n1, q{SELECT c.bucket_count FROM kingres.dist_table t JOIN kingres.colocation c USING (colocation_id) WHERE t.relid = 'public.big'::regclass}), '12', 'B: with 12 buckets');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'B: check_cluster() finds nothing');

	my $g2 = q1($n1, 'SELECT group_id FROM kingres.node WHERE node_id = 2');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.big', target_group => $g2)");
	is($rc, 0, 'B: undistribute of 30000 rows to the primary of another group') or diag($err);
	is(dig($n2, 'big'), $d, 'B: every row is on node 2');
	is(q1($n1, q{SELECT count(*) FROM pg_class WHERE relname LIKE 'big%' AND relkind IN ('r', 'p')}), '0', 'B: nothing of it is left on node 1');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'B: check_cluster() finds nothing');
}
{
	q1($n1, q{CREATE TABLE public.bigref (id int PRIMARY KEY, v text); INSERT INTO public.bigref SELECT g, repeat('r', 40) FROM generate_series(1, 6000) g; SELECT kingres.create_reference_table('public.bigref')});
	is(q1($_, 'SELECT count(*) FROM public.bigref'), '6000', 'B: a reference table of 6000 rows is whole on ' . $_->name) for @nodes;
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.undistribute_table('public.bigref')});
	is($rc, 0, 'B: the reference table is undistributed') or diag($err);
	is(q1($n1, 'SELECT count(*) FROM public.bigref'), '6000', 'B: with every row');
}

# ---------------------------------------------------------------------------
# C. P9C-04: code of a role that is not a superuser, reached through a table that the call takes with it
# ---------------------------------------------------------------------------
$_->safe_psql('postgres', q{
	CREATE ROLE ev9;
	CREATE TABLE public.ev9_seen (who text, su text);
	GRANT INSERT ON public.ev9_seen TO PUBLIC;
	CREATE FUNCTION public.ev9_chk(int) RETURNS bool LANGUAGE plpgsql AS $f$
	BEGIN INSERT INTO public.ev9_seen VALUES (current_user, current_setting('is_superuser')); RETURN true; END $f$;
	ALTER FUNCTION public.ev9_chk(int) OWNER TO ev9;
}) for @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ca9 (tenant int NOT NULL, id int NOT NULL, PRIMARY KEY (tenant, id));
	INSERT INTO public.ca9 SELECT g % 5, g FROM generate_series(1, 40) g;
	SELECT kingres.create_distributed_table('public.ca9', 'tenant', bucket_count => 4);
	CREATE TABLE public.cb9 (tenant int NOT NULL, id int NOT NULL, PRIMARY KEY (tenant, id));
	INSERT INTO public.cb9 SELECT g % 5, g FROM generate_series(1, 40) g;
	SELECT kingres.create_distributed_table('public.cb9', 'tenant', colocate_with => 'public.ca9');
	ALTER TABLE public.cb9 ADD CONSTRAINT cb9_fk FOREIGN KEY (tenant, id) REFERENCES public.ca9 (tenant, id);
	ALTER TABLE public.cb9 ADD CONSTRAINT cb9_chk CHECK (public.ev9_chk(id));
});
$_->safe_psql('postgres', 'TRUNCATE public.ev9_seen') for @nodes;    # what the setup ran as the superuser is not what the calls run
sub codes { my ($kind, $rel, $opts) = @_; return q1($n1, "SELECT coalesce(string_agg(code || ':' || object, ',' ORDER BY code, object), '') FROM kingres.table_change_report('$kind', '$rel', '$opts') WHERE severity = 'blocker'"); }
like(codes('alter', 'public.cb9', '{"bucket_count":8}'), qr/function_owner_not_superuser:public\.ev9_chk\(integer\)/, 'C: the report names the code for the table that has it');
unlike(codes('alter', 'public.ca9', '{"bucket_count":8}'), qr/function_owner_not_superuser/, 'C: nothing is said of the code of a table that the call does not take with it');
is(codes('alter', 'public.ca9', '{"bucket_count":8,"cascade_to_colocated":true}'), 'function_owner_not_superuser:public.ev9_chk(integer)',
   'C: with cascade_to_colocated the code of the other table is named');
like(q1($n1, q{SELECT detail FROM kingres.table_change_report('alter', 'public.ca9', '{"bucket_count":8,"cascade_to_colocated":true}') WHERE code = 'function_owner_not_superuser'}),
	qr/public\.cb9/, 'C: and the table that has it');
is(codes('undistribute', 'public.ca9', '{"cascade_via_foreign_keys":true}'), 'function_owner_not_superuser:public.ev9_chk(integer)', 'C: the same for undistribute with cascade_via_foreign_keys');
like(codes('undistribute', 'public.ca9', '{}'), qr/^foreign_key_linked:/, 'C: without the cascade the table is refused for its key only');
unlike(codes('undistribute', 'public.ca9', '{}'), qr/function_owner_not_superuser/, 'C: and the code of the other table is not named');
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table('public.ca9', bucket_count => 8, cascade_to_colocated => true)});
	isnt($rc, 0, 'C: alter with cascade_to_colocated is refused');
	like($err, qr/function_owner_not_superuser|who is not a superuser/, 'C: with the reason');
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.undistribute_table('public.ca9', cascade_via_foreign_keys => true)});
	isnt($rc, 0, 'C: undistribute with cascade_via_foreign_keys is refused');
	like($err, qr/function_owner_not_superuser|who is not a superuser/, 'C: with the reason');
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid IN ('public.ca9'::regclass, 'public.cb9'::regclass)}), '2', 'C: both tables are still distributed');
	is(q1($_, q{SELECT count(*) FROM public.ev9_seen WHERE su = 'on' AND who <> 'postgres'}), '0', 'C: the code of the role did not run with superuser rights on ' . $_->name) for @nodes;
}

# ---------------------------------------------------------------------------
# D. P9C-06: alter colocate_with a group that is in use
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.g2 (tenant int NOT NULL, id int NOT NULL, v int, PRIMARY KEY (tenant, id));
	INSERT INTO public.g2 SELECT g % 13, g, g FROM generate_series(1, 300) g;
	SELECT kingres.create_distributed_table('public.g2', 'tenant', bucket_count => 6);
	CREATE TABLE public.g1 (tenant int NOT NULL, id int NOT NULL, v int, PRIMARY KEY (tenant, id));
	INSERT INTO public.g1 SELECT g % 13, g, g FROM generate_series(1, 300) g;
	SELECT kingres.create_distributed_table('public.g1', 'tenant', colocate_with => 'public.g2');
	SELECT kingres.alter_distributed_table('public.g1', colocate_with => 'none');
});
{
	my $coloc2 = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.g2'::regclass});
	my $coloc1 = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.g1'::regclass});
	isnt($coloc1, $coloc2, 'D: the tables are in groups of their own');
	my $bucket = q1($n1, "SELECT bucket FROM kingres.bucket_map WHERE colocation_id = $coloc2 AND group_id = 0 ORDER BY bucket LIMIT 1");
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_d_$$";
	append_to_file($gate, 'x');
	my $mv = start_psql($n1, "SET kingres_shard.fault_injection = 'move_after_catchup:wait:$gate'; SELECT kingres.move_bucket($coloc2, $bucket, 1)");
	ok(wait_for_fault_file("$gate.reached", 90), 'D: a move of a bucket of the group of g2 is held');
	my $codes = q1($n1, q{SELECT coalesce(string_agg(code, ','), '') FROM kingres.table_change_report('alter', 'public.g1', '{"colocate_with":"public.g2"}') WHERE severity = 'blocker'});
	is($codes, 'group_busy', 'D: the report refuses the group that is being moved');
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table('public.g1', colocate_with => 'public.g2')});
	isnt($rc, 0, 'D: alter colocate_with the group that is being moved is refused');
	like($err, qr/is busy: a bucket is being moved/, 'D: with the reason');
	is(q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.g1'::regclass}), $coloc1, 'D: g1 stays in its group');
	# the group that holds the bucket is not offered by 'default' either
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table('public.g1', colocate_with => 'default')});
	is($rc, 0, 'D: colocate_with default is not refused for its own sake') or diag($err);
	is(q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.g1'::regclass}), $coloc1, 'D: the group that is being moved is not picked');
	unlink $gate;
	my ($mo, $me) = finish_psql($mv);
	is($me, '', 'D: the move finishes') or diag($me);
	for (1 .. 120) { last if q1($n1, "SELECT count(*) FROM kingres.bucket_move WHERE phase NOT IN ('cleaning', 'done', 'failed', 'undone')") eq '0'; select(undef, undef, undef, 0.5); }
	# the orphan table of a finished move waits for its cleanup: the group is free, and 'default' does not need another one (open question of the review of the fixes)
	{
		my $ph = q1($n1, "SELECT phase FROM kingres.bucket_move WHERE colocation_id = $coloc2 ORDER BY move_id DESC LIMIT 1");
		is(q1($n1, "SELECT kingres.internal_group_busy($coloc2) IS NULL"), 't', "D: the group is free while its move is in phase $ph");
		my $orphans = 0;
		$orphans += q1($_, q{SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan%' AND relkind IN ('r', 'p')}) for @nodes;
		cmp_ok($orphans, '>', 0, 'D: an orphan table is waiting for the cleanup') if $ph eq 'cleaning';
		is(q1($n1, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $coloc2 AND state <> 'active'"), '0', 'D: and no bucket of the group is in another state than active');
	}
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table('public.g1', colocate_with => 'public.g2')});
	is($rc, 0, 'D: when the move is over the same call works') or diag($err);
	is(q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.g1'::regclass}), $coloc2, 'D: and g1 joined the group');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'D: check_cluster() finds nothing');

	# a bucket that is not active, with no move recorded (a state that no move holds): the group is busy
	$n1->safe_psql('postgres', q{SELECT kingres.alter_distributed_table('public.g1', colocate_with => 'none')});
	$n1->safe_psql('postgres', "UPDATE kingres.bucket_map SET state = 'moving' WHERE colocation_id = $coloc2 AND bucket = $bucket");
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table('public.g1', colocate_with => 'public.g2')});
	isnt($rc, 0, 'D: alter colocate_with a group with a bucket that is not active is refused');
	like($err, qr/is busy: a bucket is not active/, 'D: with the reason');
	$n1->safe_psql('postgres', "UPDATE kingres.bucket_map SET state = 'active' WHERE colocation_id = $coloc2 AND bucket = $bucket");
	is(q1($n1, q{SELECT kingres.internal_group_busy(} . $coloc2 . q{) IS NULL}), 't', 'D: the group is free again');
}

# ---------------------------------------------------------------------------
# E. P9C-07: a deferrable unique constraint and the online forms
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.dq (g int NOT NULL, a int NOT NULL, b int, PRIMARY KEY (g, a), CONSTRAINT dq_u UNIQUE (g, b) DEFERRABLE INITIALLY DEFERRED);
	INSERT INTO public.dq SELECT g % 5, g, g FROM generate_series(1, 50) g;
	SELECT kingres.create_distributed_table('public.dq', 'g', bucket_count => 4);
	CREATE TABLE public.dq2 (g int NOT NULL, a int NOT NULL, b int, PRIMARY KEY (g, a), CONSTRAINT dq2_u UNIQUE (g, b) DEFERRABLE INITIALLY DEFERRED);
	INSERT INTO public.dq2 SELECT g % 5, g, g FROM generate_series(1, 50) g;
});
is(codes('alter', 'public.dq', '{"mode":"online","bucket_count":8}'), 'deferrable_unique:dq_u', 'E: the report names the constraint for the online alter');
is(codes('alter', 'public.dq', '{"bucket_count":8}'), '', 'E: and says nothing for the blocking alter');
is(codes('undistribute', 'public.dq', '{"mode":"online"}'), 'deferrable_unique:dq_u', 'E: and for the online undistribute');
is(codes('distribute', 'public.dq2', '{"column":"g","mode":"online"}'), 'deferrable_unique:dq2_u', 'E: and for the online distribute');
for my $call ("SELECT kingres.alter_distributed_table_concurrently('public.dq', bucket_count => 8)",
              "SELECT kingres.undistribute_table_concurrently('public.dq')",
              "SELECT kingres.create_distributed_table_concurrently('public.dq2', 'g')")
{
	my ($rc, $out, $err) = run_sql($n1, $call);
	isnt($rc, 0, "E: refused: $call");
	like($err, qr/DEFERRABLE/, 'E: with the reason');
}
is(q1($n1, q{SELECT count(*) FROM kingres.table_operations}), '0', 'E: no operation was started');
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table('public.dq', bucket_count => 8)});
	is($rc, 0, 'E: the blocking alter of the same table works') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM public.dq}), '50', 'E: with every row');
}

# ---------------------------------------------------------------------------
# F. P9C-08: the storage parameters of the TOAST table
# ---------------------------------------------------------------------------
sub toast_opts
{
	my ($node, $t) = @_;
	return q1($node, "SELECT coalesce((SELECT t.reloptions::text FROM pg_class c JOIN pg_class t ON t.oid = c.reltoastrelid WHERE c.oid = '$t'::regclass), '-')");
}
$n1->safe_psql('postgres', q{
	CREATE TABLE public.tx (tenant int NOT NULL, id int NOT NULL, v text, PRIMARY KEY (tenant, id)) WITH (fillfactor = 70, toast.autovacuum_enabled = false);
	INSERT INTO public.tx SELECT g % 5, g, repeat('z', 3000) FROM generate_series(1, 60) g;
});
is(toast_opts($n1, 'public.tx'), '{autovacuum_enabled=false}', 'F: the plain table has a TOAST option');
like(q1($n1, q{SELECT detail FROM kingres.table_change_report('distribute', 'public.tx', '{"column":"tenant"}') WHERE code = 'storage_parameters'}), qr/toast\.autovacuum_enabled=false/, 'F: the report names it');
$n1->safe_psql('postgres', q{SELECT kingres.create_distributed_table('public.tx', 'tenant', bucket_count => 4)});
sub toast_everywhere
{
	my ($label, $want) = @_;
	for my $n (@nodes)
	{
		my $got = q1($n, q{SELECT coalesce(string_agg(DISTINCT coalesce((SELECT t.reloptions::text FROM pg_class t WHERE t.oid = c.reltoastrelid), '-') || '/' || coalesce(c.reloptions::text, '-'), ','), '')
		                      FROM pg_class c WHERE c.relname ~ '^tx_b[0-9]{4}$' AND c.relkind = 'r'});
		is($got, $want, "F: $label: the bucket partitions on " . $n->name . ' have the parameters');
	}
}
toast_everywhere('distribute', '{autovacuum_enabled=false}/{fillfactor=70}');
$n1->safe_psql('postgres', q{SELECT kingres.alter_distributed_table('public.tx', bucket_count => 6)});
toast_everywhere('alter', '{autovacuum_enabled=false}/{fillfactor=70}');
{
	# a bucket move makes the partition again on the target
	my $coloc = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.tx'::regclass});
	my $bucket = q1($n1, "SELECT bucket FROM kingres.bucket_map WHERE colocation_id = $coloc AND group_id = 0 ORDER BY bucket LIMIT 1");
	my $id = q1($n1, "SELECT kingres.move_bucket($coloc, $bucket, 1)");
	for (1 .. 120) { last if q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $id") =~ /^(cleaning|done)$/; select(undef, undef, undef, 0.5); }
	like(move_state($n1, $id), qr/^(cleaning|done)\|/, 'F: the bucket moved');
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	toast_everywhere('move', '{autovacuum_enabled=false}/{fillfactor=70}');
}
$n1->safe_psql('postgres', q{SELECT kingres.undistribute_table('public.tx')});
is(toast_opts($n1, 'public.tx'), '{autovacuum_enabled=false}', 'F: undistribute: the TOAST option is back');
is(q1($n1, q{SELECT c.reloptions::text FROM pg_class c WHERE c.oid = 'public.tx'::regclass}), '{fillfactor=70}', 'F: with the table option');
is(q1($n1, q{SELECT count(*) FROM public.tx}), '60', 'F: and every row');

done_testing();
