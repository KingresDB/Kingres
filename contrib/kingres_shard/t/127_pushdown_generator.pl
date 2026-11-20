# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/127_pushdown_generator.pl
#
# Step 6 of phase 2b (design/shard-phase2b.md, 16.2 and 16.3): a seeded generator of closed and almost closed
# SELECT statements, run against a single ordinary node that gets the same statements (the twin), in the style of
# t/123 and of the fuzz of the review of steps 1 and 2.  This is the step 6 part of the design's 069.
#
# A statement of a closed shape (a grouping by the key, a DISTINCT with the key, a join on the key, with
# ORDER BY and LIMIT above) must give the rows of the twin, as a multiset and in order where the order is
# total, from a node chosen by the generator, under the 'pushed' plan profile and under the 'scan' profile.  It is
# counted as shipped when EXPLAIN names the Kingres Fragment Scan, and most of them must be (the planner may
# prefer the scan when the data is small, so the rule is: at least 70 percent).  A statement of an almost closed
# shape (a grouping by another column, a key from the nullable side of an outer join, another colocation group,
# a window function, a float aggregate, a sub-select, a stable function, a local table, ...) must NOT be
# shipped, must take the scan, must give the same plan as enable_pushdown = off and must still be right.  A
# statement that fails fails on both with the same error.
#
# Counts per run: KS_COUNT statements (default 400): 22 closed shapes and 17 almost closed shapes, each shape with
# its slots (restriction, aggregate list, HAVING, order and limit).  KS_SEED sets the seed (printed).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('pgen', extra => "kingres_shard.enable_partial_aggregation = off" . "
");
my @nodes = $c->nodes;
my $single = $c->single;
my $FS = 'Kingres Fragment Scan';

sub table
{
	my ($create, $dist) = @_;
	$c->on_cluster($create . ($dist // ''));
	$c->on_single($create);
}

table(q{CREATE TABLE public.s1 (id int, a int, b int, t text, n numeric(8,2), f float8)},
	q{; SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.s2 (id int PRIMARY KEY, c int, t text)},
	q{; SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s1')});
table(q{CREATE TABLE public.far (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.far', 'id', bucket_count => 7)});
table(q{CREATE TABLE public.r (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.r')});
$c->on_every_node(q{CREATE TABLE public.loc (x int, y int)});
$c->on_single(q{CREATE TABLE public.loc (x int, y int)});
$c->on_both(q{INSERT INTO public.r VALUES (0, 'L0'), (1, 'L1'), (2, 'L2'), (3, 'L3'), (4, 'L4'), (5, 'L5'), (6, 'L6')});
$c->on_both(q{INSERT INTO public.s1 SELECT g, CASE WHEN g % 17 = 0 THEN NULL ELSE (g * 7) % 50 END, CASE WHEN g % 19 = 0 THEN NULL ELSE (g * 13) % 30 END,
	CASE WHEN g % 23 = 0 THEN NULL ELSE 'v' || (g % 300) END, round((g % 997) / 7.0, 2), (g % 100) / 3.0 FROM generate_series(1, 4000) g, generate_series(1, 2) dup});
$c->on_both(q{INSERT INTO public.s2 SELECT g, CASE WHEN g % 11 = 0 THEN NULL ELSE g % 9 END, CASE WHEN g % 13 = 0 THEN NULL ELSE 's' || g END FROM generate_series(1, 4000, 2) g});
$c->on_both(q{INSERT INTO public.far SELECT g, 'f' || g FROM generate_series(1, 80) g});
$c->on_every_node(q{INSERT INTO public.loc SELECT g, g % 5 FROM generate_series(1, 40) g});
$c->on_single(q{INSERT INTO public.loc SELECT g, g % 5 FROM generate_series(1, 40) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# ---------------------------------------------------------------------------
# the shapes
# ---------------------------------------------------------------------------
my @pred = ('true', 's1.id <= 900', 's1.id BETWEEN 400 AND 2500', 's1.id % 3 = 0', 's1.a > 20', 's1.t LIKE \'v1%\'', '(s1.a + s1.b) % 2 = 0',
	's1.a IS NULL OR s1.b IS NULL', 's1.id IN (1, 2, 3, 50, 100, 101, 2500, 3999)', 's1.b < 10 AND s1.id <> 5', 's1.id = ANY (ARRAY[7, 8, 9, 2000])',
	's1.n > 40', 's1.id > 3500', 's1.a BETWEEN 5 AND 15');
my @agg = ('count(*)', 'count(s1.a), sum(s1.b)', 'min(s1.t), max(s1.a)', 'sum(s1.n), count(DISTINCT s1.b)', 'bool_or(s1.a > 40), min(s1.b)',
	'avg(s1.a), max(s1.n)', 'count(*) FILTER (WHERE s1.a > 10), sum(s1.a * 2)');
my @having = ('', '', 'HAVING count(*) >= 1', 'HAVING max(s1.a) > 5', 'HAVING sum(s1.b) IS NOT NULL');
my @oltail = ('', ' ORDER BY 1', ' ORDER BY 1 DESC LIMIT 25', ' ORDER BY 1 OFFSET 30 LIMIT 40', ' LIMIT 12');
my @oltail_total = (' ORDER BY 1', ' ORDER BY 1 DESC LIMIT 25', ' ORDER BY 1 OFFSET 30 LIMIT 40');
my %SLOTS = (pred => \@pred, agg => \@agg, having => \@having, tail => \@oltail, ttail => \@oltail_total,
	pred2 => [ map { s/s1\./s2./gr } grep { !/s1\.(?:a|b|t|n)/ } @pred ], predx => [ map { s/s1\./x./gr } @pred ]);

my @shapes = (
	# ---- closed: shipped (most of them)
	[ 'ship:group_key',      "SELECT s1.id, {agg} FROM public.s1 WHERE ({pred}) GROUP BY s1.id {having}{ttail}", 1 ],
	[ 'ship:group_key_u',    "SELECT s1.id, {agg} FROM public.s1 WHERE ({pred}) GROUP BY s1.id {having}", 0 ],
	[ 'ship:group_key_col',  "SELECT s1.id, s1.a, count(*), sum(s1.b) FROM public.s1 WHERE ({pred}) GROUP BY s1.id, s1.a {having}", 0 ],
	[ 'ship:distinct',       "SELECT DISTINCT s1.id, s1.a FROM public.s1 WHERE ({pred})", 0 ],
	[ 'ship:distinct_order', "SELECT DISTINCT s1.id, s1.b, s1.t FROM public.s1 WHERE ({pred}){ttail}", 1 ],
	[ 'ship:join',           "SELECT s1.id, s2.c, s2.t FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE ({pred}){ttail}", 1 ],
	[ 'ship:join_u',         "SELECT s1.id, s2.c, s1.a FROM public.s1, public.s2 WHERE s1.id = s2.id AND ({pred})", 0 ],
	[ 'ship:join_group',     "SELECT s1.id, count(s2.c), sum(s2.c), min(s2.t) FROM public.s1 LEFT JOIN public.s2 ON s2.id = s1.id WHERE ({pred}) GROUP BY s1.id {having}", 0 ],
	[ 'ship:join_group_o',   "SELECT s1.id, count(*), max(s2.c) FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE ({pred}) GROUP BY s1.id{ttail}", 1 ],
	[ 'ship:right_join',     "SELECT s1.id, s2.c, s1.b FROM public.s2 RIGHT JOIN public.s1 ON s2.id = s1.id WHERE ({pred}) GROUP BY s1.id, s2.c, s1.b", 0 ],
	[ 'ship:ref_group',      "SELECT s1.id, r.label, count(*) FROM public.s1 JOIN public.r ON r.k = s1.a % 7 WHERE ({pred}) GROUP BY s1.id, r.label", 0 ],
	[ 'ship:ref_join_dist',  "SELECT s1.id, s2.c, r.label FROM public.s1 JOIN public.s2 ON s2.id = s1.id JOIN public.r ON r.k = coalesce(s2.c, 0) % 7 WHERE ({pred})", 0 ],
	[ 'ship:three_way',      "SELECT s1.id, s2.c, z.b FROM public.s1 JOIN public.s2 ON s2.id = s1.id JOIN public.s1 z ON z.id = s1.id WHERE ({pred}) AND z.b < 20", 0 ],
	[ 'ship:self_join',      "SELECT x.id, y.a, y.b FROM public.s1 x JOIN public.s1 y ON y.id = x.id AND y.a >= x.a WHERE ({predx})", 0 ],
	[ 'ship:expressions',    "SELECT s1.id, sum(s1.a + s1.b), max(upper(s1.t)), count(*) FILTER (WHERE s1.n > 10) FROM public.s1 WHERE ({pred}) GROUP BY s1.id", 0 ],
	[ 'ship:case',           "SELECT s1.id, sum(CASE WHEN s1.a > 10 THEN s1.a ELSE s1.b END), min(left(coalesce(s1.t, ''), 3)) FROM public.s1 WHERE ({pred}) GROUP BY s1.id", 0 ],
	[ 'ship:junk_order',     "SELECT s1.t FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE ({pred}) ORDER BY s1.id DESC, s2.c, s1.t", 1 ],
	[ 'ship:order_agg',      "SELECT s1.id FROM public.s1 WHERE ({pred}) GROUP BY s1.id ORDER BY max(s1.b), s1.id DESC", 1 ],
	[ 'ship:literals',       "SELECT s1.id, 7, NULL, 'lit', count(*) FROM public.s1 WHERE ({pred}) GROUP BY s1.id", 0 ],
	[ 'ship:parameter_like', "SELECT s1.id, count(*), sum(s1.a) FROM public.s1 WHERE ({pred}) AND s1.b > 3 AND s1.t <> 'v5' GROUP BY s1.id", 0 ],
	[ 'ship:text_cmp',       "SELECT s1.id, min(s1.t), max(s1.t) FROM public.s1 WHERE s1.t > 'v2' AND ({pred}) GROUP BY s1.id", 0 ],
	[ 'ship:numeric',        "SELECT s1.id, sum(s1.n), avg(s1.n), min(s1.n) FROM public.s1 WHERE ({pred}) GROUP BY s1.id", 0 ],
	# ---- almost closed: not shipped
	[ 'fall:group_non_key',  "SELECT s1.a, count(*), max(s1.id) FROM public.s1 WHERE ({pred}) GROUP BY s1.a ORDER BY s1.a NULLS LAST", 1 ],
	[ 'fall:aggregate_all',  "SELECT count(*), sum(s1.a), max(s1.t) FROM public.s1 WHERE ({pred})", 0 ],
	[ 'fall:distinct_non_key', "SELECT DISTINCT s1.a, s1.b FROM public.s1 WHERE ({pred})", 0 ],
	[ 'fall:window',         "SELECT s1.id, row_number() OVER (PARTITION BY s1.id ORDER BY s1.a) FROM public.s1 WHERE ({pred}) AND s1.id < 400 ORDER BY 1", 1 ],
	[ 'fall:float_agg',      "SELECT s1.id, sum(s1.f) FROM public.s1 WHERE ({pred}) GROUP BY s1.id ORDER BY 1", 1 ],
	[ 'fall:order_sensitive', "SELECT s1.id, string_agg(s1.t, ',' ORDER BY s1.t) FROM public.s1 WHERE ({pred}) AND s1.id < 300 GROUP BY s1.id ORDER BY 1", 1 ],
	[ 'fall:stable',         "SELECT s1.id, count(*) FROM public.s1 WHERE ({pred}) AND now() > timestamptz '2000-01-01' GROUP BY s1.id ORDER BY 1 LIMIT 60", 1 ],
	[ 'fall:sub_select',     "SELECT s1.id, count(*) FROM public.s1 WHERE ({pred}) AND s1.id IN (SELECT id FROM public.s2 WHERE c > 3) GROUP BY s1.id ORDER BY 1", 1 ],
	[ 'fall:join_non_key',   "SELECT s1.id, s2.c FROM public.s1 JOIN public.s2 ON s2.id = s1.a WHERE ({pred}) AND s1.id < 800 ORDER BY 1, 2", 1 ],
	[ 'fall:other_group',    "SELECT s1.id, far.w FROM public.s1 JOIN public.far ON far.id = s1.id WHERE ({pred}) ORDER BY 1", 1 ],
	[ 'fall:nullable_key',   "SELECT s2.id, count(*) FROM public.s1 LEFT JOIN public.s2 ON s2.id = s1.id WHERE ({pred}) GROUP BY s2.id ORDER BY 1 NULLS LAST LIMIT 40", 1 ],
	[ 'fall:local_table',    "SELECT s1.id, count(*) FROM public.s1 JOIN public.loc ON loc.x = s1.b WHERE ({pred}) GROUP BY s1.id ORDER BY 1 LIMIT 40", 1 ],
	[ 'fall:key_expression', "SELECT s1.id % 10, count(*), sum(s1.b) FROM public.s1 WHERE ({pred}) GROUP BY s1.id % 10 ORDER BY 1", 1 ],
	[ 'fall:cast_via_text',  "SELECT s1.id, max(s1.a::text) FROM public.s1 WHERE ({pred}) GROUP BY s1.id ORDER BY 1 LIMIT 50", 1 ],
	[ 'fall:one_table',      "SELECT s1.id, s1.a FROM public.s1 WHERE ({pred}) ORDER BY 1 LIMIT 30", 1 ],
	[ 'fall:grouping_sets',  "SELECT s1.id, s1.a, count(*) FROM public.s1 WHERE ({pred}) AND s1.id < 100 GROUP BY GROUPING SETS ((s1.id), (s1.id, s1.a)) ORDER BY 1, 2 NULLS LAST, 3", 1 ],
	[ 'fall:cte',            "WITH x AS (SELECT id, a FROM public.s1 WHERE ({pred})) SELECT id, count(*) FROM x GROUP BY id ORDER BY 1 LIMIT 50", 1 ],
);

my @templates = map { { name => $_->[0], sql => $_->[1], ordered => $_->[2], slots => \%SLOTS } } @shapes;
my $g = $c->generator(templates => \@templates);
my $COUNT = $ENV{KS_COUNT} // 400;

# ---------------------------------------------------------------------------
# run
# ---------------------------------------------------------------------------
sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

my %count;
my %shipped;
my @q = $g->queries($COUNT);
my $i = 0;
for my $q (@q)
{
	$i++;
	my $node = $g->next_int(3);
	my $sql = $q->{sql};
	my $text = length($sql) > 90 ? substr($sql, 0, 90) . '...' : $sql;
	$count{ $q->{name} }++;
	my $plan = plan_of($nodes[$node], $sql);
	my $is_shipped = $plan =~ /\Q$FS\E/ ? 1 : 0;
	$shipped{ $q->{name} } += $is_shipped;
	if ($q->{name} =~ /^fall:/)
	{
		is($is_shipped, 0, "generated $i ($q->{name}) is not shipped: $text");
		is($plan, plan_of($nodes[$node], $sql, 'SET kingres_shard.enable_pushdown = off; '), "generated $i ($q->{name}) has the plan of enable_pushdown = off");
	}
	$c->compare("generated $i ($q->{name}) from node " . ($node + 1) . ": $text", $sql, ordered => $q->{ordered}, nodes => [$node], profiles => 0,
		plans => ['pushed', 'scan'], normalize => 1);
}
my ($nship, $nshipped) = (0, 0);
for my $name (sort keys %count)
{
	next unless $name =~ /^ship:/;
	$nship += $count{$name};
	$nshipped += $shipped{$name};
}
note(sprintf("statements: %d; closed shapes: %d of which shipped %d; almost closed: %d", $COUNT, $nship, $nshipped, $COUNT - $nship));
note(join(', ', map { "$_ $shipped{$_}/$count{$_}" } grep { /^ship:/ } sort keys %count));
cmp_ok($nshipped, '>=', int(0.7 * $nship), "at least 70 percent of the closed statements were shipped ($nshipped of $nship)");
cmp_ok($nship, '>', 0, 'the run had closed statements');

done_testing();
