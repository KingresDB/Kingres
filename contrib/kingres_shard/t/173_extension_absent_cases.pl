# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/173_extension_absent_cases.pl
#
# Review P2F-01, P2F-02 and P2F-03 of round 2b11.  The KS001 for an extension that a group does not have must not depend on the
# error that the group's parser gives: a node that never had the extension says `3F000 schema "kingres_sketch" does not exist` (no
# schema), a node that had it and dropped it says `42883` (DROP EXTENSION leaves the schema behind), a node without the schema says
# 3F000 again.  The sender asks the catalog of every group in the probe before the DECLARE, so all of them give the KS001 with the
# node, the extension and the version.  And an error of the DECLARE that is not about the extension keeps its own SQLSTATE and message.
#
#   A. never installed (both kinds of fragment, the origin from every node);
#   B. installed and dropped, with the schema left behind and with the schema dropped too;
#   C. an extension at another version: the group's own check, with both versions (unchanged);
#   D. a DECLARE that fails on a group for another reason (a column that the group does not have) while the marker names an extension:
#      the group's error comes through, not a missing extension;
#   E. the answer when the extension is installed, and the escape.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('xa', 3, extra => "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_sort = on\n");
my ($n1, $n2, $n3) = @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.s (id int, c int, g int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12);
	INSERT INTO public.s SELECT i, c, i % 5 FROM generate_series(1, 240) i, generate_series(1, 3) c;
	CREATE TABLE public.s3 (id int, c int, g int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.s3', 'id', bucket_count => 12);
	INSERT INTO public.s3 SELECT i, c, i % 5 FROM generate_series(1, 240) i, generate_series(1, 3) c;
});
$_->safe_psql('postgres', 'ANALYZE') for @nodes;

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

my @q = (
	[ 'a partial aggregation', q{SELECT g, kingres_sketch.approx_count_distinct(c) FROM public.s GROUP BY g ORDER BY g}, qr/Finalize/ ],
	[ 'a closed query', q{SELECT id, kingres_sketch.approx_count_distinct(c) FROM public.s GROUP BY id ORDER BY id LIMIT 50}, qr/Pushed: closed/ ],
);

sub ks001
{
	my ($label, $node, $sql, $what) = @_;
	my ($rc, $out, $err) = run_sql($node, "\\set VERBOSITY verbose\n$sql");
	isnt($rc, 0, "$label: fails");
	like($err, qr/KS001/, "$label: with the retryable KS001") or diag($err);
	unlike($err, qr/3F000|schema "kingres_sketch" does not exist|function kingres_sketch/, "$label: not with the raw error of the parse") or diag($err);
	like($err, qr/does not have an extension that the statement uses \(kingres_sketch:1\.0\)/, "$label: naming the extension and its version") or diag($err);
	like($err, qr/:\d+ does not have/, "$label: and the node");
	like($err, qr/is not installed in the database of that node/, "$label: saying it is not installed") or diag($err);
	like($err, qr/shippable_extensions = 'none'/, "$label: and the escape in the hint");
	is($out, '', "$label: no row was printed");
}

# ---------------------------------------------------------------------------
# E (first part): the reference answers, with the extension on every node
# ---------------------------------------------------------------------------
my %want;
$_->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch') for @nodes;
for my $q (@q)
{
	like(plan_of($n1, $q->[1]), $q->[2], "$q->[0]: shipped when every node has the extension");
	my ($rc, $out, $err) = run_sql($n1, $q->[1]);
	is($rc, 0, "$q->[0]: runs") or diag($err);
	$want{ $q->[0] } = $out;
}

# ---------------------------------------------------------------------------
# A. never installed: the node has no schema either
# ---------------------------------------------------------------------------
$n3->safe_psql('postgres', 'DROP EXTENSION kingres_sketch');
$n3->safe_psql('postgres', 'DROP SCHEMA IF EXISTS kingres_sketch CASCADE');
is($n3->safe_psql('postgres', q{SELECT count(*) FROM pg_namespace WHERE nspname = 'kingres_sketch'}), 0, 'node 3 has no schema kingres_sketch (as a node that never had the extension)');
{
	my ($rc, $out, $err) = run_sql($n3, 'SELECT kingres_sketch.approx_count_distinct(1)');
	like($err, qr/schema "kingres_sketch" does not exist/, 'the parse of such a node says 3F000, which is what the conversion by SQLSTATE did not cover');
}
for my $q (@q)
{
	like(plan_of($n1, $q->[1]), $q->[2], "$q->[0]: still planned as that path (the origin has the extension)");
	ks001("A: $q->[0], never installed, from node 1", $n1, $q->[1]);
	ks001("A: $q->[0], never installed, from node 2", $n2, $q->[1]);
	my ($rc, $o2, $e2) = run_sql($n1, 'SELECT count(*) FROM public.s');
	is($o2, '720', "A: $q->[0]: the next statement works");
	($rc, my $out, my $err) = run_sql($n1, "SET kingres_shard.shippable_extensions = 'none'; $q->[1]");
	is($rc, 0, "A: $q->[0]: with shippable_extensions = 'none' it runs") or diag($err);
	is($out, $want{ $q->[0] }, '... and gives the answer of every node having the extension');
}

# ---------------------------------------------------------------------------
# B. installed and dropped: the schema is left behind (42883), then dropped too (3F000)
# ---------------------------------------------------------------------------
$n3->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');
$n3->safe_psql('postgres', 'DROP EXTENSION kingres_sketch');
$n3->safe_psql('postgres', 'CREATE SCHEMA IF NOT EXISTS kingres_sketch');
{
	my ($rc, $out, $err) = run_sql($n3, 'SELECT kingres_sketch.approx_count_distinct(1)');
	like($err, qr/function kingres_sketch\.approx_count_distinct\(integer\) does not exist/, 'the parse of a node that had it and dropped it says 42883');
}
for my $q (@q)
{
	ks001("B: $q->[0], dropped with the schema left behind", $n1, $q->[1]);
}
$n3->safe_psql('postgres', 'DROP SCHEMA kingres_sketch');
for my $q (@q)
{
	ks001("B: $q->[0], dropped with the schema dropped too", $n1, $q->[1]);
}

# ---------------------------------------------------------------------------
# D. an error of the DECLARE that is not about the extension keeps its own SQLSTATE
# ---------------------------------------------------------------------------
$n3->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');
{
	# node 3 has the extension again; the column c of its partitions of s3 is renamed behind the statement (routing off), so the
	# DECLARE of the fragment fails on node 3 with "column c does not exist" (42703) while the marker names the extension
	$n3->safe_psql('postgres', 'SET kingres_shard.enable_routing = off; ALTER TABLE public.s3 RENAME COLUMN c TO c_renamed');
	for my $kind ([ 'a partial aggregation', q{SELECT g, kingres_sketch.approx_count_distinct(c) FROM public.s3 GROUP BY g ORDER BY g}, qr/Finalize/ ],
				  [ 'a closed query', q{SELECT id, kingres_sketch.approx_count_distinct(c) FROM public.s3 GROUP BY id ORDER BY id LIMIT 50}, qr/Pushed: closed/ ])
	{
		like(plan_of($n1, $kind->[1]), $kind->[2], "D: $kind->[0]: shipped (the origin's catalog is intact)");
		my ($rc, $out, $err) = run_sql($n1, "\\set VERBOSITY verbose\n$kind->[1]");
		isnt($rc, 0, "D: $kind->[0]: fails on the node whose column was renamed");
		like($err, qr/42703|column "c" does not exist/, '... with the error of the group: an undefined column') or diag($err);
		unlike($err, qr/KS001|does not have an extension|is not installed/, '... and not as a missing extension or a stale map') or diag($err);
	}
	$n3->safe_psql('postgres', 'SET kingres_shard.enable_routing = off; ALTER TABLE public.s3 RENAME COLUMN c_renamed TO c');
}

# ---------------------------------------------------------------------------
# C. another version: the check of the group, with both versions
# ---------------------------------------------------------------------------
{
	$n2->safe_psql('postgres', q{UPDATE pg_extension SET extversion = '0.9' WHERE extname = 'kingres_sketch'});
	my ($rc, $out, $err) = run_sql($n1, $q[0][1]);
	isnt($rc, 0, 'C: a node at another version of the extension fails the statement');
	like($err, qr/extension kingres_sketch is at version 0\.9 here and 1\.0 at the node that sent the statement/, '... with both versions') or diag($err);
	$n2->safe_psql('postgres', q{UPDATE pg_extension SET extversion = '1.0' WHERE extname = 'kingres_sketch'});
}

# ---------------------------------------------------------------------------
# E. with the extension everywhere the answer is the reference again
# ---------------------------------------------------------------------------
for my $q (@q)
{
	my ($rc, $out, $err) = run_sql($n1, $q->[1]);
	is($rc, 0, "E: $q->[0]: runs after the extension is installed on the node") or diag($err);
	is($out, $want{ $q->[0] }, '... with the same answer');
}
is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;
is($n1->safe_psql('postgres', q{SELECT count(*) FROM pg_cursors}), 0, 'no cursor is left on node 1');

done_testing();
