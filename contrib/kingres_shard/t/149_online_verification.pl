# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/149_online_verification.pl: the verification after the switch (design/shard-phase9.md, "Verification
# after the switch").  The old copy is compared with the new table; a difference keeps the old copy and is
# reported.  What is checked:
#
#   A. a table changed behind the capture trigger's back (the trigger disabled by editing the catalog, as only
#      a superuser can) is detected, the old copy is kept, check_cluster() reports it
#   B. a money column and other types whose text is not the same everywhere give equal digests
#   C. kingres_shard.rebuild_verify = off skips the check
#   D. a table that is written to after the switch is not compared (the comparison would mean nothing)
#   E. the same checks for a reference table

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('ov', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.rebuild_batch_rows = 100\nkingres_shard.node_timeout = 10\n");
	$n->reload;
}

# ---------------------------------------------------------------------------
# A. a change that the capture trigger does not see
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.va (tenant int NOT NULL, id int NOT NULL, v text, PRIMARY KEY (tenant, id));
	INSERT INTO public.va SELECT g % 4, g, 'v' || g FROM generate_series(1, 300) g;
});
my $op = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.va', '{"column": "tenant", "bucket_count": 8}')});
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'copying')"), 'catching_up', 'A: the copy is done');
# behind the trigger's back: a superuser disables it in the catalog, then the table changes
$n1->safe_psql('postgres', "UPDATE pg_trigger SET tgenabled = 'D' WHERE tgname = 'kingres_capture_$op'");
$n1->safe_psql('postgres', "INSERT INTO public.va VALUES (1, 5000, 'behind'); UPDATE public.va SET v = 'changed' WHERE id = 7;");
is(q1($n1, "SELECT count(*) FROM kingres.tlog_${op}_1"), '0', 'A: nothing was captured');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'switching')"), 'cleaning', 'A: the switch goes through (the proof does not cover this)');
my ($rc, $out, $err) = run_sql($n1, "\\set VERBOSITY verbose\n" . "SELECT kingres.table_operation_advance($op, 'cleaning')");
isnt($rc, 0, 'A: the verification fails');
like($err, qr/\bKS021\b/, 'A: with its own SQLSTATE KS021, which the control plane matches instead of the text');
like($err, qr/verification of table operation $op failed: the new table holds 300 rows \(the old copy has 301\) or a different content/, 'A: and says what differs');
is(q1($n1, "SELECT verified FROM kingres.table_operation WHERE op_id = $op"), 'failed', 'A: the operation records it');
is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'cleaning', 'A: the operation does not end');
my $old = q1($n1, "SELECT old_name FROM kingres.table_operation_item WHERE op_id = $op");
is(q1($n1, "SELECT count(*) FROM kingres.$old"), '301', 'A: the old copy is kept, with the row the new table lacks');
is(q1($n1, "SELECT kingres.cleanup_table_operations()"), '0', 'A: cleanup does not drop it');
is(q1($n1, "SELECT check_name FROM kingres.check_cluster()"), 'table_verification_failed', 'A: check_cluster() reports it');
is(q1($n1, "SELECT kingres.cleanup_table_operations(true)"), '1', 'A: only force drops it');
is(q1($n1, "SELECT count(*) FROM pg_class WHERE relname = '$old'"), '0', 'A: it is gone');
$n1->safe_psql('postgres', "UPDATE kingres.table_operation SET phase = 'done' WHERE op_id = $op");
$n1->safe_psql('postgres', "DELETE FROM kingres.table_operation_lock WHERE op_id = $op");

# the same rows, one value different: only the hash of the rows sees it
$n1->safe_psql('postgres', q{
	CREATE TABLE public.va2 (tenant int NOT NULL, id int NOT NULL, v text, PRIMARY KEY (tenant, id));
	INSERT INTO public.va2 SELECT g % 4, g, 'v' || g FROM generate_series(1, 200) g;
});
$op = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.va2', '{"column": "tenant", "bucket_count": 8}')});
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'copying')"), 'catching_up', 'A2: the copy is done');
$n1->safe_psql('postgres', "UPDATE pg_trigger SET tgenabled = 'D' WHERE tgname = 'kingres_capture_$op'");
$n1->safe_psql('postgres', "UPDATE public.va2 SET v = 'different' WHERE id = 11");
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'switching')"), 'cleaning', 'A2: the switch goes through');
($rc, $out, $err) = run_sql($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')");
isnt($rc, 0, 'A2: the verification fails although both tables have 200 rows');
like($err, qr/the new table holds 200 rows \(the old copy has 200\) or a different content/, 'A2: and says so');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
$n1->safe_psql('postgres', "UPDATE kingres.table_operation SET phase = 'done' WHERE op_id = $op");
$n1->safe_psql('postgres', "DELETE FROM kingres.table_operation_lock WHERE op_id = $op");

# ---------------------------------------------------------------------------
# B. types whose text depends on settings
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.vb (tenant int NOT NULL, id int NOT NULL, price money, at timestamptz, f float8, b bytea, i interval,
	                        c char(5), n numeric(10,3), j jsonb, a int[], PRIMARY KEY (tenant, id));
	INSERT INTO public.vb SELECT g % 3, g, (g * 1.37)::numeric::money, timestamptz '2026-03-01 12:00:00+00' + g * interval '1 hour',
	       g / 7.0, decode(lpad(to_hex(g), 4, '0'), 'hex'), g * interval '1 day 2 hours', 'ab', g / 3.0, jsonb_build_object('g', g), ARRAY[g, g + 1]
	  FROM generate_series(1, 150) g;
});
$op = q1($n1, q{SELECT kingres.create_distributed_table_concurrently('public.vb', 'tenant', bucket_count => 6)});
is(q1($n1, "SELECT phase || '|' || verified FROM kingres.table_operation WHERE op_id = $op"), 'done|ok', 'B: money, timestamps, floats, bytea, intervals, arrays: the digests are equal');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');

# ---------------------------------------------------------------------------
# C. verification off
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.vc (tenant int NOT NULL, id int NOT NULL, v text, PRIMARY KEY (tenant, id));
	INSERT INTO public.vc SELECT g % 4, g, 'v' || g FROM generate_series(1, 100) g;
});
$op = q1($n1, "SET kingres_shard.rebuild_verify = off; SELECT kingres.begin_table_operation('distribute', 'public.vc', '{\"column\": \"tenant\", \"bucket_count\": 4}')");
# the setting is read by the session that runs the cleaning phase: set it for every session of the node
for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.rebuild_verify = off\n");
	$n->reload;
}
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'C: the operation ends');
is(q1($n1, "SELECT verified FROM kingres.table_operation WHERE op_id = $op"), 'skipped', 'C: the verification was skipped');
for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.rebuild_verify = on\n");
	$n->reload;
}
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');

# ---------------------------------------------------------------------------
# D. written to after the switch
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.vd (tenant int NOT NULL, id int NOT NULL, v text, PRIMARY KEY (tenant, id));
	INSERT INTO public.vd SELECT g % 4, g, 'v' || g FROM generate_series(1, 100) g;
});
$op = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.vd', '{"column": "tenant", "bucket_count": 4}')});
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'switching')"), 'cleaning', 'D: switched');
$n1->safe_psql('postgres', "INSERT INTO public.vd VALUES (1, 9999, 'after'); UPDATE public.vd SET v = 'after2' WHERE id = 3; DELETE FROM public.vd WHERE id = 4");
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'D: the operation ends');
is(q1($n1, "SELECT verified FROM kingres.table_operation WHERE op_id = $op"), 'skipped', 'D: the comparison is skipped');
like(q1($n1, "SELECT detail FROM kingres.table_operation WHERE op_id = $op") // '', qr/^$|written to after the switch/, 'D: with the reason');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');

# ---------------------------------------------------------------------------
# E. a reference table
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ve (code text PRIMARY KEY, label text, n money);
	INSERT INTO public.ve SELECT 'c' || g, 'label ' || g, (g * 2.5)::numeric::money FROM generate_series(1, 120) g;
});
$op = q1($n1, q{SELECT kingres.create_reference_table_concurrently('public.ve')});
is(q1($n1, "SELECT verified FROM kingres.table_operation WHERE op_id = $op"), 'ok', 'E: the copies of a reference table are verified on every node');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
# a copy that differs on one node is found
$n1->safe_psql('postgres', q{
	CREATE TABLE public.vf (code text PRIMARY KEY, label text);
	INSERT INTO public.vf SELECT 'c' || g, 'label ' || g FROM generate_series(1, 60) g;
});
$op = q1($n1, q{SELECT kingres.begin_table_operation('distribute_reference', 'public.vf', '{}')});
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'catching_up')"), 'switching', 'E: copied');
$n1->safe_psql('postgres', "UPDATE pg_trigger SET tgenabled = 'D' WHERE tgname = 'kingres_capture_$op'");
$n1->safe_psql('postgres', "INSERT INTO public.vf VALUES ('late', 'behind the trigger')");
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'switching')"), 'cleaning', 'E: switched');
($rc, $out, $err) = run_sql($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')");
isnt($rc, 0, 'E: the verification fails on the nodes that lack the row');
like($err, qr/node \d+ holds \d+ rows/, 'E: and names the node');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');

done_testing();
