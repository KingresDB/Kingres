# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/182_guard_and_locks.pl: what the owner of a table that is being rebuilt can and cannot do, and what a reader can do to the
# administrator's calls (review of phase 9, P9B-04, P9B-06).
#
#   A. the DDL guard of an online operation also refuses what takes the capture trigger away or moves the table: DROP TRIGGER of
#      the capture trigger by the owner, ALTER TABLE ... SET SCHEMA, DROP INDEX and ALTER INDEX of its indexes, CREATE RULE,
#      CREATE POLICY; the operation finishes with every change of a writer in the new table
#   B. a role that can only read a table cannot hold a lock on it through GRANT or REVOKE (the statement of a role without the
#      right to grant took ROW EXCLUSIVE for the rest of its transaction)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_waiter start_psql finish_psql);

my ($n1, $n2) = make_cluster('gl', 2);
my @nodes = ($n1, $n2);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

sub adv
{
	my ($op, $phase) = @_;
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.table_operation_advance($op, '$phase')");
	$out =~ s/\s+$//;
	return $rc == 0 ? $out : "ERROR: $err";
}

$_->safe_psql('postgres', 'CREATE ROLE ownr4 LOGIN; CREATE ROLE ro4; CREATE ROLE rw4') for @nodes;
$n1->safe_psql('postgres', 'CREATE SCHEMA other4; GRANT CREATE, USAGE ON SCHEMA other4 TO ownr4');
$_->safe_psql('postgres', 'GRANT CREATE, USAGE ON SCHEMA public TO ownr4') for @nodes;

# ---------------------------------------------------------------------------
# A. the guard
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.dt2 (id int PRIMARY KEY, v text, w text);
	CREATE INDEX dt2_v ON public.dt2 (v);
	ALTER TABLE public.dt2 OWNER TO ownr4;
	INSERT INTO public.dt2 SELECT g, 'v' || g, 'w' FROM generate_series(1, 40) g;
});
my $op = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.dt2', '{"column": "id", "bucket_count": 6}')});
is(adv($op, 'copying'), 'catching_up', 'A: the copy is done');
my @statements = (
	[ 'DROP TRIGGER of the capture trigger', "DROP TRIGGER kingres_capture_$op ON public.dt2" ],
	[ 'ALTER TABLE ... SET SCHEMA', 'ALTER TABLE public.dt2 SET SCHEMA other4' ],
	[ 'DROP INDEX of the key', 'DROP INDEX public.dt2_pkey' ],
	[ 'DROP INDEX', 'DROP INDEX public.dt2_v' ],
	[ 'ALTER INDEX ... RENAME', 'ALTER INDEX public.dt2_v RENAME TO dt2_v2' ],
	[ 'CREATE RULE', 'CREATE RULE dt2_r AS ON INSERT TO public.dt2 DO ALSO NOTHING' ],
	[ 'CREATE POLICY', 'CREATE POLICY dt2_p ON public.dt2 USING (true)' ],
	[ 'ALTER TABLE ... DISABLE TRIGGER', "ALTER TABLE public.dt2 DISABLE TRIGGER kingres_capture_$op" ],
);
for my $s (@statements)
{
	my ($rc, $out, $err) = run_sql($n1, "SET ROLE ownr4; $s->[1]");
	isnt($rc, 0, "A: $s->[0] is refused");
	like($err, qr/is being rebuilt by table operation $op/, "A: $s->[0]: and says why") or diag($err);
}
is(q1($n1, "SELECT count(*) FROM pg_trigger WHERE tgname = 'kingres_capture_$op' AND tgenabled = 'A'"), '1', 'A: the capture trigger is there and enabled');
$n1->safe_psql('postgres', "INSERT INTO public.dt2 VALUES (100, 'new', 'w'); UPDATE public.dt2 SET w = 'changed' WHERE id = 3; DELETE FROM public.dt2 WHERE id = 4");
is(adv($op, 'switching'), 'cleaning', 'A: the switch goes through');
is(adv($op, 'cleaning'), 'done', 'A: the operation ends');
is(q1($n1, q{SELECT count(*) || '|' || count(*) FILTER (WHERE id = 100) || '|' || count(*) FILTER (WHERE w = 'changed') || '|' || count(*) FILTER (WHERE id = 4) FROM public.dt2}),
	'40|1|1|0', 'A: every change of the writer is in the new table');
is(q1($n1, "SELECT verified FROM kingres.table_operation WHERE op_id = $op"), 'ok', 'A: and the verification says ok');
# after the operation the owner may do all of it again
{
	my ($rc, $out, $err) = run_sql($n1, 'SET ROLE ownr4; CREATE INDEX dt2_w ON public.dt2 (w)');
	is($rc, 0, 'A: when the operation is done the table can be changed again') or diag($err);
}

# ---------------------------------------------------------------------------
# B. a lock that a reader holds through GRANT
# ---------------------------------------------------------------------------
my $i = 0;
for my $stmt ('GRANT SELECT ON public.lt4_N TO PUBLIC', 'REVOKE SELECT ON public.lt4_N FROM PUBLIC', 'GRANT SELECT ON public.lt4_N TO rw4')
{
	$i++;
	(my $sql = $stmt) =~ s/lt4_N/lt4_$i/;
	$n1->safe_psql('postgres', "CREATE TABLE public.lt4_$i (id int PRIMARY KEY, v text); INSERT INTO public.lt4_$i SELECT g, 'v' FROM generate_series(1, 20) g; GRANT SELECT ON public.lt4_$i TO ro4;");
	my $s = $n1->background_psql('postgres', on_error_stop => 0);
	my $pid = $s->query_safe('SELECT pg_backend_pid()'); $pid =~ s/\s+//g;
	$s->query_safe('SET ROLE ro4; BEGIN;');
	$s->query($sql);
	is(q1($n1, "SELECT count(*) FROM pg_locks l WHERE l.pid = $pid AND l.relation = 'public.lt4_$i'::regclass AND l.mode = 'RowExclusiveLock'"),
		'0', "B: after \"$sql\" by a role that may only read, it holds no ROW EXCLUSIVE lock on the table");
	# (the administrator's call still waits for the uncommitted change of the table's row in pg_class, which a GRANT
	# writes even when it grants nothing: that is PostgreSQL's, and is not tried here)
	$s->query('ROLLBACK;');
	$s->quit;
}
# the owner and a role with the grant option still wait for a table that is being distributed (the lock is the statement's own)
$n1->safe_psql('postgres', q{CREATE TABLE public.lt5 (id int PRIMARY KEY, v text); INSERT INTO public.lt5 SELECT g, 'v' FROM generate_series(1, 20) g; GRANT SELECT ON public.lt5 TO rw4 WITH GRANT OPTION});
{
	my $s = $n1->background_psql('postgres', on_error_stop => 0);
	my $pid = $s->query_safe('SELECT pg_backend_pid()'); $pid =~ s/\s+//g;
	$s->query_safe('SET ROLE rw4; BEGIN;');
	$s->query('GRANT SELECT ON public.lt5 TO ro4');
	is(q1($n1, "SELECT count(*) FROM pg_locks l WHERE l.pid = $pid AND l.relation = 'public.lt5'::regclass AND l.mode = 'RowExclusiveLock'"),
		'1', 'B: a role with the grant option that grants does hold the lock of the statement');
	$s->query('ROLLBACK;');
	$s->quit;
}

# ---------------------------------------------------------------------------
# C. the capture waits for writers for a bounded time (P9B-07: the README says how long), names them, and the call is undone
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{CREATE TABLE public.lw4 (id int PRIMARY KEY, v text); INSERT INTO public.lw4 SELECT g, 'v' FROM generate_series(1, 20) g});
{
	my $writer = start_psql($n1, "BEGIN; UPDATE public.lw4 SET v = 'w' WHERE id = 1; SELECT pg_sleep(12); COMMIT;");
	select(undef, undef, undef, 1.0);
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_distributed_table_concurrently('public.lw4', 'id', bucket_count => 4)");
	my $took = time - $t0;
	isnt($rc, 0, 'C: the online call fails while a writer is in the table');
	like($err, qr/could not lock table public\.lw4 in SHARE ROW EXCLUSIVE mode/, 'C: and says which lock it could not get');
	like($err, qr/Held by: pid \d+/, 'C: and who holds the table');
	cmp_ok($took, '<=', 9, "C: after a bounded wait ($took s)");
	is(q1($n1, q{SELECT relkind FROM pg_class WHERE oid = 'public.lw4'::regclass}), 'r', 'C: the table is as it was');
	is(q1($n1, q{SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\_shadow\_%' OR relname LIKE 'tlog\_%'}), '0', 'C: nothing of the operation is left');
	finish_psql($writer);
}

is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for @nodes;
done_testing();
