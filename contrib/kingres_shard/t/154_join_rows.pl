# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/154_join_rows.pl
#
# Step 10 of phase 2b (design/shard-phase2b.md, 9): the join of colocated tables is a fragment of its own.  A statement that is
# not closed as a whole (a window function that is not partitioned by the key, an ordered-set aggregate, count(DISTINCT), DISTINCT ON)
# but joins its tables on their distribution keys (and reference tables) has the groups return the rows of the join, with the columns
# the rest of the plan needs; this node does the rest.  The text is a closed query (a join on the key, no aggregate), so the group's
# rule for the fragment of a closed query accepts it.  The answer must be the single node's, under the plan profiles, from every node,
# with the path assertion.
#
#   A. joins that are pushed: inner, left, full between distributed tables on the key, with a reference table, three tables
#   B. joins that are not pushed, with the reason of the table of 9.1: a reference table on the preserved side, a join on a non-key,
#      two colocation groups, a sub-select, the switch
#   C. the text: only the columns that the plan needs, no aggregate, no window; parameters and a generic plan

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('jr', extra => "kingres_shard.enable_pushdown_join = on
");
my ($n1, $n2, $n3) = $c->nodes;
my @nodes = ($n1, $n2, $n3);
my $FS = 'Kingres Fragment Scan';
my $JR = 'Pushed: join rows (on key)';

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

table(q{CREATE TABLE public.s1 (id int PRIMARY KEY, a int, b int, t text)},
	q{; SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.s2 (id int PRIMARY KEY, c int, d int)},
	q{; SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s1')});
table(q{CREATE TABLE public.s3 (id int PRIMARY KEY, e int)},
	q{; SELECT kingres.create_distributed_table('public.s3', 'id', colocate_with => 'public.s1')});
table(q{CREATE TABLE public.far (id int PRIMARY KEY, w int)},
	q{; SELECT kingres.create_distributed_table('public.far', 'id', bucket_count => 7)});
table(q{CREATE TABLE public.r (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.r')});
$c->on_both(q{INSERT INTO public.r VALUES (0, 'L0'), (1, 'L1'), (2, 'L2'), (3, 'L3'), (4, 'L4'), (5, 'L5'), (6, 'L6')});
$c->on_both(q{INSERT INTO public.s1 SELECT g, g % 13, g % 9, 'v' || (g % 50) FROM generate_series(1, 4000) g});
$c->on_both(q{INSERT INTO public.s2 SELECT g, g % 11, g % 7 FROM generate_series(1, 4000, 2) g});
$c->on_both(q{INSERT INTO public.s3 SELECT g, g % 5 FROM generate_series(1, 4000, 3) g});
$c->on_both(q{INSERT INTO public.far SELECT g, g % 5 FROM generate_series(1, 200) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

my @PLANS = ('pushed', 'scan');

# ---------------------------------------------------------------------------
# A. pushed
# ---------------------------------------------------------------------------
my @pushed = (
	[ 'a window over an inner join', q{SELECT s1.id, s2.c, row_number() OVER (ORDER BY s2.c, s1.id) FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE s1.a > 5}, 0 ],
	[ 'an ordered-set aggregate over an inner join', q{SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY s1.b) FROM public.s1 JOIN public.s2 ON s2.id = s1.id}, 0 ],
	[ 'count(DISTINCT) over an inner join', q{SELECT s2.c, count(DISTINCT s1.b) FROM public.s1 JOIN public.s2 ON s2.id = s1.id GROUP BY s2.c ORDER BY 1}, 1 ],
	[ 'DISTINCT ON over an inner join', q{SELECT DISTINCT ON (s2.c) s2.c, s1.id FROM public.s1 JOIN public.s2 ON s2.id = s1.id ORDER BY s2.c, s1.id}, 1 ],
	[ 'mode() over an inner join', q{SELECT mode() WITHIN GROUP (ORDER BY s2.c) FROM public.s1 JOIN public.s2 ON s2.id = s1.id}, 0 ],
	[ 'a window over a left join', q{SELECT s1.id, s2.c, rank() OVER (ORDER BY s2.c NULLS LAST, s1.id) FROM public.s1 LEFT JOIN public.s2 ON s2.id = s1.id WHERE s1.id < 600}, 0 ],
	[ 'a window over a full join on the key', q{SELECT s1.id, s2.id AS id2, dense_rank() OVER (ORDER BY s1.a NULLS LAST, s2.d NULLS LAST) FROM public.s1 FULL JOIN public.s2 ON s2.id = s1.id WHERE s1.id < 300 OR s2.id < 300 OR s1.id IS NULL}, 0 ],
	[ 'a window over a distributed table and a reference table', q{SELECT s1.id, r.label, rank() OVER (PARTITION BY r.label ORDER BY s1.id) FROM public.s1 JOIN public.r ON r.k = s1.b % 7 WHERE s1.id < 400}, 0 ],
	[ 'a distributed table left join a reference table, with a window', q{SELECT s1.id, r.label, row_number() OVER (ORDER BY s1.id) FROM public.s1 LEFT JOIN public.r ON r.k = s1.b WHERE s1.id < 400}, 0 ],
	[ 'three tables, a window', q{SELECT s1.id, s2.c, s3.e, row_number() OVER (ORDER BY s3.e, s1.id) FROM public.s1 JOIN public.s2 ON s2.id = s1.id JOIN public.s3 ON s3.id = s1.id}, 0 ],
	[ 'the key equality in WHERE', q{SELECT s1.id, s2.c, count(*) OVER (PARTITION BY s2.c) FROM public.s1, public.s2 WHERE s2.id = s1.id AND s1.id < 800}, 0 ],
	[ 'a self join, a window', q{SELECT x.id, y.a, row_number() OVER (ORDER BY y.a, x.id) FROM public.s1 x JOIN public.s1 y ON y.id = x.id WHERE x.id < 500}, 0 ],
	[ 'a window and a sort and a limit above', q{SELECT s1.id, s2.c, row_number() OVER (ORDER BY s2.c, s1.id) AS rn FROM public.s1 JOIN public.s2 ON s2.id = s1.id ORDER BY rn LIMIT 15}, 1 ],
	[ 'a sum over a window of the join', q{SELECT s1.id, sum(s2.d) OVER (ORDER BY s1.id) FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE s1.id < 300}, 0 ],
);
for my $q (@pushed)
{
	my ($name, $sql, $ordered) = @$q;
	$c->compare("join rows: $name", $sql, ordered => $ordered, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, $JR ]);
}

# ---------------------------------------------------------------------------
# B. not pushed
# ---------------------------------------------------------------------------
my @not = (
	[ 'a reference table on the preserved side', q{SELECT r.label, s1.id, row_number() OVER (ORDER BY r.k, s1.id) FROM public.r LEFT JOIN public.s1 ON s1.b = r.k AND s1.id < 50} ],
	[ 'a reference table on a full join', q{SELECT r.label, s1.id, row_number() OVER (ORDER BY r.k, s1.id) FROM public.r FULL JOIN public.s1 ON s1.b = r.k AND s1.id < 50} ],
	[ 'a join on a non-key', q{SELECT s1.id, s2.id, row_number() OVER (ORDER BY s1.id, s2.id) FROM public.s1 JOIN public.s2 ON s2.c = s1.a WHERE s1.id < 100 AND s2.id < 100} ],
	[ 'two colocation groups', q{SELECT s1.id, far.w, row_number() OVER (ORDER BY s1.id) FROM public.s1 JOIN public.far ON far.id = s1.id} ],
	[ 'a sub-select', q{SELECT s1.id, row_number() OVER (ORDER BY s1.id) FROM public.s1 WHERE s1.id IN (SELECT id FROM public.s2 WHERE c > 5) AND s1.id < 400} ],
	[ 'a left join on a non-key', q{SELECT s1.id, s2.id, row_number() OVER (ORDER BY s1.id, s2.id) FROM public.s1 LEFT JOIN public.s2 ON s2.c = s1.a AND s2.id < 100 WHERE s1.id < 100} ],
);
for my $q (@not)
{
	my ($name, $sql) = @$q;
	$c->compare("not pushed: $name", $sql, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1);
	unlike(plan_of($n1, $sql), qr/\Q$JR\E/, "not pushed: $name: the join is not a fragment");
}
{
	my $sql = q{SELECT s1.id, s2.c, row_number() OVER (ORDER BY s2.c, s1.id) FROM public.s1 JOIN public.s2 ON s2.id = s1.id};
	unlike(plan_of($n1, $sql, 'SET kingres_shard.enable_pushdown_join = off; '), qr/\Q$JR\E/, 'enable_pushdown_join = off: the rows of each table come to this node');
	unlike(plan_of($n1, $sql, 'SET kingres_shard.enable_pushdown = off; '), qr/\Q$JR\E/, 'enable_pushdown = off: the same');
	like(plan_of($n1, $sql), qr/\Q$JR\E/, 'the default: the join is a fragment');
}

# ---------------------------------------------------------------------------
# C. the text, parameters
# ---------------------------------------------------------------------------
{
	my $sql = q{SELECT s1.id, s2.c, row_number() OVER (ORDER BY s2.c, s1.id) FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE s1.a > 5};
	my $p = plan_of($n1, $sql);
	my ($text) = $p =~ /Remote SQL: (.*)$/m;
	like($text, qr/^SELECT s1\.id, s2\.c FROM \(public\.s1 JOIN public\.s2 ON \(\(s2\.id = s1\.id\)\)\) WHERE \(s1\.a > 5\)$/, 'the text is the FROM and WHERE and the columns the plan needs');
	unlike($text, qr/OVER|ORDER BY|GROUP BY|\*/, '... no window, no sort, no star');
	like($p, qr/WindowAgg/, 'the window is this node\'s');

	my $G = 'SET plan_cache_mode = force_generic_plan; ';
	my $pre = q{PREPARE jp(int, int) AS SELECT s1.id, s2.c, row_number() OVER (ORDER BY s2.c, s1.id) FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE s1.a > $1 AND s2.d < $2; };
	my ($rs, $os) = run_sql($c->single, $pre . 'EXECUTE jp(5, 4)');
	for my $nd (@nodes)
	{
		for my $mode ('', $G)
		{
			my ($rc, $out, $err) = run_sql($nd, $mode . $pre . 'EXECUTE jp(5, 4)');
			is($rc, 0, 'a prepared join runs on ' . $nd->name . ($mode ? ' (generic plan)' : '')) or diag($err);
			is(join("\n", sort split /\n/, $out), join("\n", sort split /\n/, $os), '... and its answer is the single node\'s');
		}
	}
	like((run_sql($n1, $G . $pre . 'EXPLAIN (COSTS OFF) EXECUTE jp(5, 4)'))[1], qr/\Q$JR\E/, 'a generic plan pushes the join too');
	my ($rc, $o, $e) = run_sql($n1, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) $sql");
	is($rc, 0, 'EXPLAIN ANALYZE runs') or diag($e);
	like($o, qr/Rows from groups:/, '... and shows the rows of the groups');
}

done_testing();
