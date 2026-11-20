# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/181_capture_keys.pl: what the capture trigger records, and how the replay reads it back (review of phase 9, P9B-02,
# P9B-08, P9B-03).
#
#   A. a float key changed by a session with extra_float_digits below 1 is captured exactly (the key was cut to "0.3" and
#      the change was not replayed); float4 and a float key that is not the distribution column
#   B. a row of more than 256 MB can be written while an operation runs (the trigger copied the whole row into a jsonb)
#   C. a replay chunk that moves a unique value from one row to another does not fail on the unique index that is not the key
#      (the rows of one value that changed hands are deleted and inserted together)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('ck', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

# one phase of an operation; a failure is the text of the error, so that a test goes on and says what failed
sub adv
{
	my ($op, $phase) = @_;
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.table_operation_advance($op, '$phase')");
	$out =~ s/\s+$//;
	return $rc == 0 ? $out : "ERROR: $err";
}

for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.rebuild_batch_rows = 100\nkingres_shard.node_timeout = 10\n");
	$n->reload;
}

# ---------------------------------------------------------------------------
# A. float keys
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.fx (k float8 PRIMARY KEY, v text);
	INSERT INTO public.fx VALUES (0.1::float8 + 0.2::float8, 'orig'), (1.5, 'x'), (0.1::float8 + 0.7::float8, 'orig2');
	CREATE TABLE public.fx4 (k float4 PRIMARY KEY, v text);
	INSERT INTO public.fx4 VALUES (0.1::float4 + 0.2::float4, 'orig'), (1.5, 'x'), (16777217, 'big');
	CREATE TABLE public.fxs (tenant int NOT NULL, k float8 NOT NULL, v text, PRIMARY KEY (tenant, k));
	INSERT INTO public.fxs VALUES (1, 0.1::float8 + 0.2::float8, 'orig'), (1, 1.5, 'x'), (2, 0.1::float8 + 0.7::float8, 'orig2');
});
my %op;
$op{fx} = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.fx', '{"column": "k", "bucket_count": 12}')});
$op{fx4} = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.fx4', '{"column": "k", "bucket_count": 12}')});
is(adv($op{fx}, 'copying'), 'catching_up', 'A: the copy of the float8 table is done');
{
	my ($rc, $out, $err) = run_sql($n1, q{SET extra_float_digits = 0;
		UPDATE public.fx SET v = 'changed' WHERE k = 0.1::float8 + 0.2::float8;
		DELETE FROM public.fx WHERE k = 0.1::float8 + 0.7::float8;
		INSERT INTO public.fx VALUES (0.1::float8 * 7, 'new');
		UPDATE public.fx SET k = 2.0000000000000004 WHERE k = 1.5;});
	is($rc, 0, 'A: the writer with extra_float_digits = 0 runs') or diag($err);
}
is(q1($n1, "SELECT count(*) FROM kingres.tlog_$op{fx}_1"), '5', 'A: the changes were logged');
is(adv($op{fx}, 'switching'), 'cleaning', 'A: the switch goes through');
is(adv($op{fx}, 'cleaning'), 'done', 'A: the operation ends, verified') or diag('verification failed');
is(q1($n1, "SELECT verified FROM kingres.table_operation WHERE op_id = $op{fx}"), 'ok', 'A: the verification says ok');
is(q1($n1, q{SELECT count(*) || '|' || count(*) FILTER (WHERE k = 0.1::float8 + 0.2::float8 AND v = 'changed') || '|' ||
		count(*) FILTER (WHERE k = 0.1::float8 + 0.7::float8) || '|' || count(*) FILTER (WHERE k = 0.1::float8 * 7 AND v = 'new') || '|' ||
		count(*) FILTER (WHERE k = 2.0000000000000004 AND v = 'x') || '|' || count(*) FILTER (WHERE k = 1.5) FROM public.fx}),
	'3|1|0|1|1|0', 'A: float8: update, delete, insert and key change are replayed exactly');

is(adv($op{fx4}, 'copying'), 'catching_up', 'A: the copy of the float4 table is done');
run_sql($n1, q{SET extra_float_digits = 0; UPDATE public.fx4 SET v = 'changed' WHERE k = 0.1::float4 + 0.2::float4; DELETE FROM public.fx4 WHERE k = 16777216; INSERT INTO public.fx4 VALUES (0.7::float4, 'new');});
is(adv($op{fx4}, 'switching'), 'cleaning', 'A: float4: the switch goes through');
is(adv($op{fx4}, 'cleaning'), 'done', 'A: float4: the operation ends');
is(q1($n1, q{SELECT count(*) || '|' || count(*) FILTER (WHERE v = 'changed') || '|' || count(*) FILTER (WHERE v = 'big') || '|' || count(*) FILTER (WHERE v = 'new') FROM public.fx4}),
	'3|1|0|1', 'A: float4: the changes are replayed');

# a float that is not the distribution column
$op{fxs} = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.fxs', '{"column": "tenant", "bucket_count": 8}')});
is(adv($op{fxs}, 'copying'), 'catching_up', 'A: the copy of the composite-key table is done');
run_sql($n1, q{SET extra_float_digits = -3; UPDATE public.fxs SET v = 'changed' WHERE k = 0.1::float8 + 0.2::float8; DELETE FROM public.fxs WHERE k = 0.1::float8 + 0.7::float8;});
is(adv($op{fxs}, 'switching'), 'cleaning', 'A: composite key: the switch goes through');
is(adv($op{fxs}, 'cleaning'), 'done', 'A: composite key: the operation ends');
is(q1($n1, q{SELECT count(*) || '|' || count(*) FILTER (WHERE v = 'changed') FROM public.fxs}), '2|1', 'A: composite key: the changes are replayed');

# a session that runs the replay with a low setting: the keys cross the nodes as text
$n1->safe_psql('postgres', q{CREATE TABLE public.fxr (k float8 PRIMARY KEY, v text); INSERT INTO public.fxr SELECT g + 0.1::float8 / 3, 'v' FROM generate_series(1, 300) g});
$op{fxr} = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.fxr', '{"column": "k", "bucket_count": 12}')});
is(adv($op{fxr}, 'copying'), 'catching_up', 'A: the copy with many float keys is done');
$n1->safe_psql('postgres', q{UPDATE public.fxr SET v = 'changed' WHERE k < 100; DELETE FROM public.fxr WHERE k > 250});
{
	my ($rc, $out, $err) = run_sql($n1, "SET extra_float_digits = -5; SELECT kingres.table_operation_advance($op{fxr}, 'switching'); SELECT kingres.table_operation_advance($op{fxr}, 'cleaning')");
	is($rc, 0, 'A: the replay and the switch run in a session with extra_float_digits = -5') or diag($err);
}
is(q1($n1, q{SELECT count(*) || '|' || count(*) FILTER (WHERE v = 'changed') FROM public.fxr}), '249|99', 'A: the keys of the changes crossed the nodes exactly');

# the sessions of the engine (copy, replay) start with the settings of the database: a low extra_float_digits there must not cut the keys
$_->safe_psql('postgres', 'ALTER DATABASE postgres SET extra_float_digits = -5') for @nodes;
$n1->safe_psql('postgres', q{CREATE TABLE public.fxd (k float8 PRIMARY KEY, v text); INSERT INTO public.fxd SELECT g + 0.1::float8 / 3, 'v' FROM generate_series(1, 300) g});
$op{fxd} = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.fxd', '{"column": "k", "bucket_count": 12}')});
is(adv($op{fxd}, 'copying'), 'catching_up', 'A: the copy of float keys with extra_float_digits = -5 in the database is done');
$n1->safe_psql('postgres', q{UPDATE public.fxd SET v = 'changed' WHERE k < 100; DELETE FROM public.fxd WHERE k > 250});
is(adv($op{fxd}, 'switching'), 'cleaning', 'A: the replay and the switch go through');
is(adv($op{fxd}, 'cleaning'), 'done', 'A: the operation ends, verified');
is(q1($n1, q{SELECT count(*) || '|' || count(*) FILTER (WHERE v = 'changed') FROM public.fxd}), '249|99', 'A: the changes of the float keys are replayed exactly');
$_->safe_psql('postgres', 'ALTER DATABASE postgres RESET extra_float_digits') for @nodes;

# ---------------------------------------------------------------------------
# B. a value over 256 MB while an operation runs
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{CREATE TABLE public.bx (id int PRIMARY KEY, v text); INSERT INTO public.bx SELECT g, 'v' FROM generate_series(1, 20) g});
$op{bx} = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.bx', '{"column": "id", "bucket_count": 4}')});
is(adv($op{bx}, 'copying'), 'catching_up', 'B: the copy is done');
{
	my ($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.bx VALUES (5001, repeat('y', 270000000))});
	is($rc, 0, 'B: a row with a value of 270 MB can be written while the operation runs') or diag($err);
	is(q1($n1, "SELECT count(*) FROM kingres.tlog_$op{bx}_1 WHERE op = 'I'"), '1', 'B: and is in the log as a key');
	$n1->safe_psql('postgres', 'DELETE FROM public.bx WHERE id = 5001');
}
is(adv($op{bx}, 'switching'), 'cleaning', 'B: the switch goes through');
is(adv($op{bx}, 'cleaning'), 'done', 'B: the operation ends');

# ---------------------------------------------------------------------------
# C. a unique value that changes hands in one chunk
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ux (tenant int NOT NULL, id int NOT NULL, email text, v text, PRIMARY KEY (tenant, id), UNIQUE (tenant, email));
	INSERT INTO public.ux VALUES (1, 1, 'a', 'x'), (1, 2, 'b', 'x');
});
$op{ux} = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.ux', '{"column": "tenant", "bucket_count": 8}')});
is(adv($op{ux}, 'copying'), 'catching_up', 'C: the copy is done');
$n1->safe_psql('postgres', q{UPDATE public.ux SET v = 'y' WHERE id = 2; UPDATE public.ux SET email = 'z' WHERE id = 1; UPDATE public.ux SET email = 'a' WHERE id = 2;});
is(q1($n1, "SELECT count(*) FROM kingres.tlog_$op{ux}_1"), '3', 'C: three entries in the log (B, A, B)');
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.rebuild_batch_rows = 1; SELECT kingres.table_operation_advance($op{ux}, 'catching_up')");
	is($rc, 0, 'C: the replay with chunks of one entry does not fail on the unique index') or diag($err);
}
is(adv($op{ux}, 'switching'), 'cleaning', 'C: the switch goes through');
is(adv($op{ux}, 'cleaning'), 'done', 'C: the operation ends');
is(q1($n1, q{SELECT string_agg(id || ':' || email || ':' || v, ',' ORDER BY id) FROM public.ux}), '1:z:x,2:a:y', 'C: the rows are as the writers left them');

# ---------------------------------------------------------------------------
# D. a verification that cannot be done says so (P9B-11): the table is written to after the switch, so the old copy is not
#    compared; the record says why, and the call that ends the operation warns, also when the tables differ
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{CREATE TABLE public.vd (id int PRIMARY KEY, v text); INSERT INTO public.vd SELECT g, 'v' FROM generate_series(1, 30) g});
$op{vd} = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.vd', '{"column": "id", "bucket_count": 6}')});
is(adv($op{vd}, 'copying'), 'catching_up', 'D: the copy is done');
$n1->safe_psql('postgres', "UPDATE pg_trigger SET tgenabled = 'D' WHERE tgname = 'kingres_capture_$op{vd}'");
$n1->safe_psql('postgres', "INSERT INTO public.vd VALUES (500, 'behind the trigger')");
is(adv($op{vd}, 'switching'), 'cleaning', 'D: the switch goes through');
$n1->safe_psql('postgres', "UPDATE public.vd SET v = 'after' WHERE id = 1");
{
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.table_operation_advance($op{vd}, 'cleaning')");
	is($rc, 0, 'D: the operation ends');
	like($err, qr/the verification of table operation $op{vd} was not done: the table was written to after the switch.*and it differs: the new table holds 30 rows \(the old copy has 31\)/s,
		'D: the call warns, with the reason and the difference') or diag($err);
	is(q1($n1, "SELECT verified FROM kingres.table_operation WHERE op_id = $op{vd}"), 'skipped', 'D: the record says skipped');
	like(q1($n1, "SELECT detail FROM kingres.table_operation WHERE op_id = $op{vd}") // '', qr/it differs/, 'D: with the difference in the record');
}

is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for @nodes;
done_testing();
