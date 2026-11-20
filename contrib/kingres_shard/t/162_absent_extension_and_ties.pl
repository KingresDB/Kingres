# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/162_absent_extension_and_ties.pl
#
# Two pieces that earlier rounds left out and the design asks for:
#
#   A. KS001 for an extension that a group does not have (design 5.5).  A group that lacks the extension fails when it parses the
#      text of the fragment (the function does not exist), before any hook of the extension can see the marker; the node that
#      sent the fragment knows what the statement uses (the marker names the extensions) and raises the KS001 of a version
#      difference, naming the extension and the node.  The escape is shippable_extensions = 'none'.  Both kinds of fragment (the
#      closed query and the partial aggregation) are tested; when the extension is installed on the node the statement works.
#   B. The tie-break of the ordered merge (design 8, O3): rows that compare equal come out in the order of their group id,
#      whichever node runs the statement (the node's own part is not first).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('ae', 3, extra => "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_sort = on\n");
my ($n1, $n2, $n3) = @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.s (id int, c int, g int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12);
	INSERT INTO public.s SELECT i, c, i % 5 FROM generate_series(1, 240) i, generate_series(1, 3) c;
});
$_->safe_psql('postgres', 'ANALYZE') for @nodes;

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# ---------------------------------------------------------------------------
# A. an extension that a group does not have
# ---------------------------------------------------------------------------
{
	$_->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch') for ($n1, $n2);	# not on node 3
	my @q = (
		[ 'a partial aggregation', q{SELECT g, kingres_sketch.approx_count_distinct(c) FROM public.s GROUP BY g ORDER BY g}, qr/Finalize/ ],
		[ 'a closed query', q{SELECT id, kingres_sketch.approx_count_distinct(c) FROM public.s GROUP BY id ORDER BY id LIMIT 50}, qr/Pushed: closed/ ],
	);
	# the answer when every node has the extension is the reference
	my %want;
	$n3->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');
	for my $q (@q)
	{
		like(plan_of($n1, $q->[1]), $q->[2], "$q->[0]: shipped when every node has the extension");
		my ($rc, $out, $err) = run_sql($n1, $q->[1]);
		is($rc, 0, "$q->[0]: runs when every node has the extension") or diag($err);
		$want{ $q->[0] } = $out;
	}
	$n3->safe_psql('postgres', 'DROP EXTENSION kingres_sketch');
	for my $q (@q)
	{
		my ($name, $sql, $path) = @$q;
		like(plan_of($n1, $sql), $path, "$name: still planned as that path (the origin has the extension)");
		my ($rc, $out, $err) = run_sql($n1, "\\set VERBOSITY verbose\n$sql");
		isnt($rc, 0, "$name: a group without the extension fails the statement");
		like($err, qr/KS001/, '... with the retryable KS001, not "function does not exist"') or diag($err);
		like($err, qr/does not have an extension that the statement uses \(kingres_sketch:1\.0\)/, '... naming the extension and its version');
		like($err, qr/ae3|:\d+ does not have/, '... and the node');
		like($err, qr/does not exist/, '... with what the node said, as the detail');
		like($err, qr/shippable_extensions = 'none'/, '... and the escape in the hint');
		is($out, '', "$name: no row was printed");
		# the escape: this node reads the rows and runs the aggregate itself
		($rc, $out, $err) = run_sql($n1, "SET kingres_shard.shippable_extensions = 'none'; $sql");
		is($rc, 0, "$name: with shippable_extensions = 'none' it runs") or diag($err);
		is($out, $want{$name}, '... and gives the answer of every node having the extension');
		# the other nodes of the cluster as origin: node 2 has it
		($rc, $out, $err) = run_sql($n2, "\\set VERBOSITY verbose\n$sql");
		like($err, qr/KS001/, "$name: the same from node 2");
		# the statement after the failure works (the connection is usable)
		my ($rc2, $o2, $e2) = run_sql($n1, 'SELECT count(*) FROM public.s');
		is($o2, '720',"$name: the next statement on the node gives its answer") or diag($e2);
	}
	$n3->safe_psql("postgres", "CREATE EXTENSION kingres_sketch");
	# a genuine error of another kind is not turned into KS001
	{
		my ($rc, $out, $err) = run_sql($n1, "SELECT g, kingres_sketch.approx_count_distinct(c) FROM public.s GROUP BY g HAVING 1/(count(*) - count(*)) > 0");
		isnt($rc, 0, "an error of another kind fails the statement");
		like($err, qr/division by zero/, "... with its own error");
		unlike($err, qr/does not have an extension/, "... and it is not reported as a missing extension");
	}
	for my $q (@q)
	{
		my ($rc, $out, $err) = run_sql($n1, $q->[1]);
		is($rc, 0, "$q->[0]: after the extension is installed on the node the statement runs") or diag($err);
		is($out, $want{ $q->[0] }, '... with the same answer');
	}
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;
	my ($cnt) = $n1->safe_psql('postgres', q{SELECT count(*) FROM pg_cursors});
	is($cnt, 0, 'no cursor is left on node 1');
}

# ---------------------------------------------------------------------------
# B. the tie-break by group id
# ---------------------------------------------------------------------------
{
	# every row compares equal on the sort key (count = 3), so the order of the output is the order of the tie-break
	my $sql = q{SELECT id, count(*) AS cnt FROM public.s GROUP BY id ORDER BY cnt};
	for my $nd (@nodes)
	{
		like(plan_of($nd, $sql), qr/Merge keys/, $nd->name . ': the ordered merge is the path');
	}
	my %out;
	for my $nd (@nodes)
	{
		my ($rc, $o, $e) = run_sql($nd, $sql);
		is($rc, 0, $nd->name . ': the statement runs') or diag($e);
		$out{ $nd->name } = $o;
		my @ids = map { (split /\|/)[0] } grep { length } split /\n/, $o;
		is(scalar(@ids), 240, $nd->name . ': 240 rows');
		my $arr = 'ARRAY[' . join(',', @ids) . ']';
		my $seq = $nd->safe_psql('postgres', "SELECT string_agg(m.group_id::text, ',' ORDER BY u.ord) FROM unnest($arr) WITH ORDINALITY AS u(id, ord) JOIN kingres.bucket_map m ON m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, u.id)");
		my @g = split /,/, $seq;
		my $sorted = join(',', sort { $a <=> $b } @g);
		is($seq, $sorted, $nd->name . ': the rows come out in the order of their group ids (the tie-break), not with this node\'s own part first');
	}
	is($out{ $n2->name }, $out{ $n1->name }, 'node 2 gives the same rows in the same order as node 1');
	is($out{ $n3->name }, $out{ $n1->name }, 'node 3 gives the same rows in the same order as node 1');
	my ($rc, $again) = run_sql($n2, $sql);
	is($again, $out{ $n2->name }, 'a repeated query returns the same order');
	# with a limit: the rows come from the lowest group ids first (the order inside a group is the group's own)
	for my $nd (@nodes)
	{
		my ($rl, $lim) = run_sql($nd, "$sql LIMIT 25");
		my @ids = map { (split /\|/)[0] } grep { length } split /\n/, $lim;
		is(scalar(@ids), 25, $nd->name . ': a limit of 25 rows');
		my $seq = $nd->safe_psql('postgres', "SELECT string_agg(m.group_id::text, ',' ORDER BY u.ord) FROM unnest(ARRAY[" . join(',', @ids) . "]) WITH ORDINALITY AS u(id, ord) JOIN kingres.bucket_map m ON m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, u.id)");
		is($seq, join(',', sort { $a <=> $b } split /,/, $seq), $nd->name . ': with a limit the streams are merged in the order of the group ids');
	}
}

done_testing();
