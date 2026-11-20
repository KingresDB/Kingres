# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/136_defences.pl
#
# Review P2D-02 of step 5: the defences that protect a statement against a catalog that changed after
# it was planned had no test that can reach them (the plan-start placement check, the recheck after the
# last group, the relation signature at the start of the run, the origin's parse of its own text).
# A real move cannot overtake the statement (the switch of a move waits for the locks the statement
# holds), so the test changes the catalog of the origin by hand, behind the statement, at the point where
# the statement waits (the fault action waitinv: it waits for a file, and then takes the invalidations
# that committed meanwhile).  The groups are not touched: they agree with the plan, and the origin's
# own catalog is the only thing that says the plan is wrong.  Each section names its mutant, the
# change of one line of the code that the test must fail for.
#
#   A. insel_before_start_check: the placement check at the start of the run   (planner/fanout.c M07)
#   B. insel_before_recheck:     the placement recheck after the last group    (fanout.c M09)
#   C. insel_before_rels_check:  the relation signature of the plan             (fanout.c M10)
#   D. insel_before_text_parse:  the origin's parse of its own text, INSERT     (planner.c M27)
#   E. query_before_text_parse:  the same for a closed SELECT                   (planner.c, step 6)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql wait_for_fault_file start_psql finish_psql);

my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my $ROWS = 240;
my $P6 = q{INSERT INTO public.dst SELECT id, v FROM public.src};

my @n = make_cluster('df', 3);
my ($n1, $n2, $n3) = @n;
$n1->safe_psql('postgres', qq{
	CREATE TABLE public.src (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.src', 'id', bucket_count => 12);
	CREATE TABLE public.dst (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.dst', 'id', colocate_with => 'public.src');
	INSERT INTO public.src SELECT g, 'v' || g FROM generate_series(1, $ROWS) g;
});
$n1->safe_psql('postgres', 'ANALYZE');
my $COLOC = "(SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.src'::regclass)";
my $BUCKET = $n1->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = $COLOC AND group_id = 1");
my $GROUP = $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $COLOC AND bucket = $BUCKET");

like(plain($n1, "EXPLAIN (COSTS OFF) $P6"), qr/Kingres Fan-out Write/, 'the statement of the tests is shipped whole');

sub plain
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, $sql);
	return $rc == 0 ? $out : "ERROR: $err";
}

# raw rows of a table on the nodes, without routing: how many each group holds
sub raw_total
{
	my ($table) = @_;
	my $sum = 0;
	$sum += $_->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT count(*) FROM $table") for @n;
	return $sum;
}

# Run $stmt on node 1 until it waits at $point (action waitinv or wait); run $during; let it go.
sub with_gate
{
	my ($point, $action, $stmt, $during, $tag, $pre) = @_;
	my $gate = "$tmp/gate_df_$tag";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, ($pre // '') . " SET kingres_shard.fault_injection = '$point:$action:$gate'; $stmt");
	ok(wait_for_fault_file("$gate.reached", 60), "the statement waits at $point ($tag)");
	$during->();
	unlink $gate;
	return finish_psql($sess);
}

# the bucket map of the origin changes behind the statement: one bucket is in another group
sub skew_map { $n1->safe_psql('postgres', "UPDATE kingres.bucket_map SET group_id = 2 WHERE colocation_id = $COLOC AND bucket = $BUCKET"); }
sub unskew_map { $n1->safe_psql('postgres', "UPDATE kingres.bucket_map SET group_id = $GROUP WHERE colocation_id = $COLOC AND bucket = $BUCKET"); }

# ---------------------------------------------------------------------------
# A. the placement check at the start of the run (the cache of the origin says the plan is old)
# ---------------------------------------------------------------------------
{
	my ($out, $err) = with_gate('insel_before_start_check', 'waitinv', $P6, \&skew_map, 'a');
	unskew_map();
	like($err, qr/the bucket map changed after the statement was planned/, 'A: a bucket map that changed before the run is refused at the start of the run');
	is(raw_total('public.dst'), 0, 'A: nothing was written');
	my ($rc, $o, $e) = run_sql($n1, $P6);
	is($rc, 0, 'A: the statement works when the map is as it was') or diag($e);
	is(raw_total('public.dst'), $ROWS, 'A: ... and writes every row once');
	$n1->safe_psql('postgres', 'TRUNCATE public.dst');
}

# ---------------------------------------------------------------------------
# B. the recheck after the last group (the origin's catalog tables, read as they are now)
# ---------------------------------------------------------------------------
{
	my ($out, $err) = with_gate('insel_before_recheck', 'wait', $P6, \&skew_map, 'b');
	unskew_map();
	like($err, qr/the bucket map changed while the statement was running/, 'B: a bucket map that changed while the groups ran fails the statement');
	is(raw_total('public.dst'), 0, 'B: nothing was written (the statement was rolled back)');
	my ($rc, $o, $e) = run_sql($n1, $P6);
	is($rc, 0, 'B: the statement works when the map is as it was') or diag($e);
	$n1->safe_psql('postgres', 'TRUNCATE public.dst');
}

# ---------------------------------------------------------------------------
# C. the relation signature of the plan
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', 'CREATE TABLE public.dst_saved AS SELECT * FROM kingres.dist_table WHERE relid = \'public.dst\'::regclass');
	my ($out, $err) = with_gate('insel_before_rels_check', 'waitinv', $P6,
		sub { $n1->safe_psql('postgres', q{DELETE FROM kingres.dist_table WHERE relid = 'public.dst'::regclass}); }, 'c');
	$n1->safe_psql('postgres', q{INSERT INTO kingres.dist_table SELECT * FROM public.dst_saved});
	$n1->safe_psql('postgres', 'DROP TABLE public.dst_saved');
	like($err, qr/no longer what it was when this INSERT \.\.\. SELECT was planned/, 'C: a target that is no longer distributed is refused at the start of the run');
	is(raw_total('public.dst'), 0, 'C: nothing was written');
	my ($rc, $o, $e) = run_sql($n1, $P6);
	is($rc, 0, 'C: the statement works when the table is as it was') or diag($e);
	is(raw_total('public.dst'), $ROWS, 'C: ... and writes every row once');
	$n1->safe_psql('postgres', 'TRUNCATE public.dst');
}

# ---------------------------------------------------------------------------
# D. the origin parses its own text: a table that stopped being distributed between the deparse and the parse
#    is seen by the parse and the statement is not shipped; it is planned again with the new catalog
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', 'CREATE TABLE public.dst_saved AS SELECT * FROM kingres.dist_table WHERE relid = \'public.dst\'::regclass');
	my ($out, $err) = with_gate('insel_before_text_parse', 'waitinv', $P6,
		sub { $n1->safe_psql('postgres', q{DELETE FROM kingres.dist_table WHERE relid = 'public.dst'::regclass}); }, 'd');
	$n1->safe_psql('postgres', q{INSERT INTO kingres.dist_table SELECT * FROM public.dst_saved});
	$n1->safe_psql('postgres', 'DROP TABLE public.dst_saved');
	like($err, qr/no partition of relation "dst"/, 'D: the parse refuses the text of a table that is no longer distributed: the statement is not shipped, it is planned again as a local one');
	unlike($err, qr/no longer what it was/, 'D: ... and the defence of the run did not have to catch a shipped plan');
	$n1->safe_psql('postgres', 'TRUNCATE public.dst');
}

# ---------------------------------------------------------------------------
# E. the same for a closed SELECT of step 6
# ---------------------------------------------------------------------------
{
	my $Q = q{SELECT id, count(*) FROM public.src GROUP BY id};
	my $full = $n1->safe_psql('postgres', "SELECT count(*) FROM ($Q) q");
	is($full, $ROWS, 'E: the closed query returns a row per key');
	$n1->safe_psql('postgres', 'CREATE TABLE public.src_saved AS SELECT * FROM kingres.dist_table WHERE relid = \'public.src\'::regclass');
	my ($out, $err) = with_gate('query_before_text_parse', 'waitinv', $Q,
		sub { $n1->safe_psql('postgres', q{DELETE FROM kingres.dist_table WHERE relid = 'public.src'::regclass}); }, 'e');
	$n1->safe_psql('postgres', q{INSERT INTO kingres.dist_table SELECT * FROM public.src_saved});
	$n1->safe_psql('postgres', 'DROP TABLE public.src_saved');
	my $got = scalar(() = $out =~ /^[0-9]+[|][0-9]+$/mg);
	isnt($got, $ROWS, 'E: a table that stopped being distributed is not read through the groups as if it still were');
}

done_testing();
