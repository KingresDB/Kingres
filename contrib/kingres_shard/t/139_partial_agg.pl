# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/139_partial_agg.pl
#
# Step 7 of phase 2b (design/shard-phase2b.md, 7 and 16.2): partial aggregation.  An aggregate or a grouping that is
# not closed (it does not contain a distribution key) is run in two steps: every group aggregates its own rows
# and returns the transition states (the marker /* kingres:partial-fragment place=<n> */), and the node that runs the
# statement combines them (Finalize ... over the Kingres Fragment Scan).  The answer must be the single node's,
# under every plan profile (pushed, nopartial, scan), from every node, with the path assertion.
#
#   A. aggregates of every type of the partial-capable class of 7.1, with FILTER, HAVING, several aggregates
#      sharing a state, GROUP BY an expression, scalar aggregates over an empty and a non-empty table (A7),
#      pruning, a join, a reference table, ORDER BY and LIMIT above the aggregate;
#   B. what is not split (A4, A9: DISTINCT inside a call, ordered-set, grouping sets, no combine function, money,
#      a window, DISTINCT above) and still gives the right answer by the scan;
#   C. the plan strategies on the groups (hashed, sorted, a constant group expression, plain);
#   D. parameters and prepared statements (generic plans), cursors, EXPLAIN, the switches;
#   E. a forged fragment: the marker kind partial is accepted for what the origin ships and refused (0A000) for
#      a statement with HAVING, ORDER BY, LIMIT, an aggregate that cannot be split or a table that no rule accepts.
#
# Float aggregates and the aggregates whose result depends on the order of the rows are combined in the order the groups
# answer (A3): the comparison rounds a float to 9 digits and sorts the elements of an array_agg or string_agg.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql make_cluster prepared_count wait_for_fault_file start_psql finish_psql release_node);
use KingresShardCompare;

my $c = KingresShardCompare->new('pa', extra => "kingres_shard.enable_partial_aggregation = on
");
my ($n1, $n2, $n3) = $c->nodes;
my @nodes = ($n1, $n2, $n3);
my $single = $c->single;
my $FS = 'Kingres Fragment Scan';

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

table(q{CREATE TABLE public.s1 (id int PRIMARY KEY, a int, b int, t text, n numeric(10,2), d date, ts timestamptz, f float8,
		f4 float4, b8 bigint, sm smallint, bo bool, bt bit(8), iv interval)},
	q{; SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.mn (id int PRIMARY KEY, a int, mo money)},
	q{; SELECT kingres.create_distributed_table('public.mn', 'id', colocate_with => 'public.s1')});
table(q{CREATE TABLE public.s2 (id int PRIMARY KEY, c int, t text)},
	q{; SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s1')});
table(q{CREATE TABLE public.r (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.r')});
table(q{CREATE TABLE public.far (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.far', 'id', bucket_count => 7)});
$c->on_both(q{INSERT INTO public.r VALUES (0, 'L0'), (1, 'L1'), (2, 'L2'), (3, 'L3'), (4, 'L4'), (5, 'L5'), (6, 'L6')});
$c->on_both(q{INSERT INTO public.s1 SELECT g, CASE WHEN g % 17 = 0 THEN NULL ELSE (g * 7) % 50 END,
	CASE WHEN g % 19 = 0 THEN NULL ELSE (g * 13) % 30 END, CASE WHEN g % 23 = 0 THEN NULL ELSE 'v' || (g % 400) END,
	round((g % 1000) / 7.0, 2), date '2024-01-01' + (g % 365), timestamptz '2024-01-01 00:00:00+00' + (g || ' minutes')::interval,
	(g % 100) / 3.0, ((g % 77) / 7.0)::float4, g::bigint * 3000000000, (g % 300)::smallint, g % 3 = 0,
	(g % 256)::int::bit(8), (g % 90 || ' minutes')::interval FROM generate_series(1, 3000) g});
$c->on_both(q{INSERT INTO public.mn SELECT g, g % 5, (g % 500)::numeric::money FROM generate_series(1, 300) g});
$c->on_both(q{INSERT INTO public.s2 SELECT g, CASE WHEN g % 11 = 0 THEN NULL ELSE g % 9 END, CASE WHEN g % 13 = 0 THEN NULL ELSE 's' || g END FROM generate_series(1, 3000, 2) g});
$c->on_both(q{INSERT INTO public.far SELECT g, 'f' || g FROM generate_series(1, 60) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

my @PLANS = ('pushed', 'nopartial', 'scan');

# ---------------------------------------------------------------------------
# A. partial aggregation, shipped
# ---------------------------------------------------------------------------
my @shipped = (
	# [ name, sql, ordered, normalize, no path assertion ]
	[ 'scalar, several types', q{SELECT count(*), count(a), sum(a), sum(b8), min(b), max(t), avg(n), min(d), max(ts) FROM public.s1}, 0 ],
	[ 'min and max of an indexed column (an index probe on the groups, no Agg node)', q{SELECT min(id), max(id) FROM public.s1}, 0 ],
	[ 'min of an indexed column in an expression', q{SELECT min(id) + 1, max(id) * 2 FROM public.s1 WHERE id > 100}, 0 ],
	[ 'min of an indexed column, grouped', q{SELECT a % 3, min(id), max(id) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'group by a column', q{SELECT a, count(*), sum(b), avg(n), min(t), max(d) FROM public.s1 GROUP BY a ORDER BY a NULLS LAST}, 1 ],
	[ 'group by two columns', q{SELECT a, b % 4, count(*), max(n) FROM public.s1 GROUP BY a, b % 4 ORDER BY 1 NULLS LAST, 2 NULLS LAST}, 1 ],
	[ 'group by an expression', q{SELECT a % 5, count(*), sum(b) FROM public.s1 GROUP BY a % 5 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'having on an aggregate of the list', q{SELECT a, count(*) FROM public.s1 GROUP BY a HAVING count(*) > 55 ORDER BY a}, 1 ],
	[ 'having on an aggregate that is not in the list', q{SELECT a FROM public.s1 GROUP BY a HAVING sum(b) > 400 ORDER BY a}, 1 ],
	[ 'filter', q{SELECT a % 7, count(*) FILTER (WHERE b > 10), sum(b) FILTER (WHERE t IS NOT NULL) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'several aggregates of one state', q{SELECT a, sum(b), avg(b), count(b), sum(b) + count(b) FROM public.s1 GROUP BY a ORDER BY a NULLS LAST}, 1 ],
	[ 'integer types', q{SELECT a % 3, sum(sm), sum(b8), avg(sm), min(b8), max(sm), bit_and(sm), bit_or(sm), bit_xor(sm) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'numeric statistics', q{SELECT a % 4, stddev(n), variance(n), stddev_pop(n), var_samp(n) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'float', q{SELECT a % 4, sum(f), avg(f), min(f), max(f), max(f4), stddev(f), corr(f, f4), regr_slope(f, f4) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'bool, bit', q{SELECT a % 4, bool_and(bo), bool_or(bo), every(bo), bit_and(bt), bit_or(bt), bit_xor(bt) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'interval, date, timestamptz', q{SELECT a % 4, sum(iv), avg(iv), min(d), max(ts) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'text', q{SELECT a % 6, min(t), max(t), count(t) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'array_agg and string_agg (the order of the elements is not fixed)', q{SELECT a % 5, array_agg(b), string_agg(t, ',') FROM public.s1 WHERE id < 200 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1,
	  { floats => 'auto', sort_arrays => [2], sort_strings => { 3 => ',' } } ],
	[ 'regression aggregates', q{SELECT a % 3, covar_pop(b, a), regr_count(b, a), regr_avgx(b, a), regr_r2(b, a) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'a grouping with no aggregate', q{SELECT a, b % 3 FROM public.s1 GROUP BY a, b % 3 ORDER BY 1 NULLS LAST, 2 NULLS LAST}, 1 ],
	[ 'a constant in the list', q{SELECT a % 3, 'x', 7, count(*) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'ORDER BY an aggregate and LIMIT', q{SELECT a, sum(b) FROM public.s1 GROUP BY a ORDER BY sum(b) DESC NULLS LAST, a LIMIT 7}, 1 ],
	[ 'a restriction on the table', q{SELECT a, count(*) FROM public.s1 WHERE b > 12 AND t LIKE 'v1%' GROUP BY a ORDER BY a NULLS LAST}, 1 ],
	[ 'pruned by the key', q{SELECT count(*), sum(b), max(a) FROM public.s1 WHERE id IN (1, 2, 3, 50, 500, 1999)}, 0 ],
	[ 'a scalar aggregate over an empty set (A7)', q{SELECT count(*), sum(a), min(t), avg(n), bool_and(bo) FROM public.s1 WHERE a > 1000}, 0 ],
	[ 'a grouped aggregate over an empty set', q{SELECT a, count(*) FROM public.s1 WHERE a > 1000 GROUP BY a}, 0 ],
	[ 'a colocated join', q{SELECT s2.c, count(*), sum(s1.b), max(s1.t) FROM public.s1 JOIN public.s2 ON s2.id = s1.id GROUP BY s2.c ORDER BY 1 NULLS LAST}, 1 ],
	[ 'a join, scalar', q{SELECT count(*), sum(s2.c), avg(s1.n) FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE s1.a > 5}, 0 ],
	[ 'a left join', q{SELECT s2.c, count(s2.id), count(*) FROM public.s1 LEFT JOIN public.s2 ON s2.id = s1.id GROUP BY s2.c ORDER BY 1 NULLS LAST}, 1 ],
	[ 'a reference table', q{SELECT r.label, count(*), sum(s1.b) FROM public.s1 JOIN public.r ON r.k = s1.a % 7 GROUP BY r.label ORDER BY 1}, 1 ],
	[ 'a self join', q{SELECT x.a % 4, count(*) FROM public.s1 x JOIN public.s1 y ON y.id = x.id WHERE y.b > 3 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'a grouping column that is not selected', q{SELECT sum(b) FROM public.s1 GROUP BY a ORDER BY a NULLS LAST}, 1 ],
);
for my $q (@shipped)
{
	my ($name, $sql, $ordered, $norm, $nopath) = @$q;
	$c->compare("partial: $name", $sql, ordered => $ordered, plans => \@PLANS, normalize => $norm // 1,
		($nopath ? () : (path => [ $FS, 'Finalize' ])));
}

# ---------------------------------------------------------------------------
# B. not split: the scan answers, and the answer is right
# ---------------------------------------------------------------------------
my @scan = (
	[ 'count(DISTINCT) on a non-key', q{SELECT a % 5, count(DISTINCT b) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'sum(DISTINCT)', q{SELECT sum(DISTINCT b) FROM public.s1}, 0 ],
	[ 'an ORDER BY inside the call', q{SELECT a % 5, array_agg(b ORDER BY b) FROM public.s1 WHERE id < 100 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'an ordered-set aggregate', q{SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY b) FROM public.s1}, 0 ],
	[ 'a hypothetical-set aggregate', q{SELECT rank(5) WITHIN GROUP (ORDER BY b) FROM public.s1}, 0 ],
	[ 'mode', q{SELECT mode() WITHIN GROUP (ORDER BY b) FROM public.s1}, 0 ],
	[ 'json_agg (no combine function)', q{SELECT a % 3, json_array_length(json_agg(b)) FROM public.s1 WHERE id < 40 GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'grouping sets', q{SELECT a % 3, b % 2, count(*) FROM public.s1 GROUP BY GROUPING SETS ((1), (2), ()) ORDER BY 1 NULLS LAST, 2 NULLS LAST}, 1 ],
	[ 'money', q{SELECT a, sum(mo) FROM public.mn GROUP BY a ORDER BY a}, 1 ],
	[ 'a table of another colocation group', q{SELECT far.w, count(*) FROM public.s1 JOIN public.far ON far.id = s1.id GROUP BY far.w ORDER BY 1 LIMIT 5}, 1 ],
	[ 'a stable function', q{SELECT a % 3, count(*) FROM public.s1 WHERE ts < now() GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
	[ 'a sub-select', q{SELECT a % 3, count(*) FROM public.s1 WHERE id IN (SELECT id FROM public.s2 WHERE c > 3) GROUP BY 1 ORDER BY 1 NULLS LAST}, 1 ],
);
for my $q (@scan)
{
	my ($name, $sql, $ordered, $norm) = @$q;
	$c->compare("not split: $name", $sql, ordered => $ordered, plans => ['pushed', 'scan'], profiles => 0, normalize => $norm // 1);
	my $plan = plan_of($n1, $sql);
	my $off = plan_of($n1, $sql, 'SET kingres_shard.enable_pushdown = off; ');
	unlike($plan, qr/Finalize/, "not split: $name is not aggregated in two steps");
	is($plan, $off, "not split: $name has the plan that enable_pushdown = off gives");
}

# ---------------------------------------------------------------------------
# C. the plan strategies on the groups
# ---------------------------------------------------------------------------
{
	my @q = (
		q{SELECT a, count(*), sum(b), avg(n), min(t) FROM public.s1 GROUP BY a ORDER BY a NULLS LAST},
		q{SELECT count(*), sum(b), avg(n), min(t), max(ts) FROM public.s1},
		q{SELECT 'k', a % 3, count(*), sum(n) FROM public.s1 GROUP BY 1, 2 ORDER BY 2 NULLS LAST},
		q{SELECT a FROM public.s1 GROUP BY a ORDER BY a NULLS LAST},
		q{SELECT a % 7, sum(b) FILTER (WHERE a > 3), count(*) FROM public.s1 GROUP BY 1 ORDER BY 1 NULLS LAST},
	);
	for my $set ([ 'enable_hashagg', 'off' ], [ 'enable_sort', 'off' ], [ 'work_mem', '64kB' ])
	{
		for my $nd (@nodes)
		{
			$nd->safe_psql('postgres', "ALTER SYSTEM SET $set->[0] TO '$set->[1]'");
			$nd->reload;
		}
		for my $sql (@q)
		{
			$c->compare("strategy ($set->[0] = $set->[1]): $sql", $sql, ordered => ($sql =~ /ORDER BY/ ? 1 : 0), plans => ['pushed', 'scan'], profiles => 0, normalize => 1);
		}
		for my $nd (@nodes)
		{
			$nd->safe_psql('postgres', "ALTER SYSTEM RESET $set->[0]");
			$nd->reload;
		}
	}
}

# ---------------------------------------------------------------------------
# C2. the classification of 7.1, recomputed from pg_aggregate, and the plan of every aggregate of the class
# ---------------------------------------------------------------------------
{
	my $cls = sub {
		my ($cond) = @_;
		return $n1->safe_psql('postgres', "SELECT string_agg(DISTINCT p.proname, ' ' ORDER BY p.proname) FROM pg_aggregate a JOIN pg_proc p ON p.oid = a.aggfnoid WHERE a.aggfnoid < 16384 AND $cond");
	};
	my $capable = q{any_value array_agg avg bit_and bit_or bit_xor bool_and bool_or corr count covar_pop covar_samp every max min range_intersect_agg regr_avgx regr_avgy regr_count regr_intercept regr_r2 regr_slope regr_sxx regr_sxy regr_syy stddev stddev_pop stddev_samp string_agg sum var_pop var_samp variance};
	is($cls->(q{a.aggkind = 'n' AND a.aggcombinefn <> 0 AND (a.aggtranstype <> 'internal'::regtype OR (a.aggserialfn <> 0 AND a.aggdeserialfn <> 0))}), $capable,
		'the built-in aggregates that a group can return partial states of are the list of design 7.1 (a change in a later release shows here)');
	is($cls->(q{a.aggkind = 'n' AND a.aggcombinefn = 0}),
		q{json_agg json_agg_strict json_object_agg json_object_agg_strict json_object_agg_unique json_object_agg_unique_strict jsonb_agg jsonb_agg_strict jsonb_object_agg jsonb_object_agg_strict jsonb_object_agg_unique jsonb_object_agg_unique_strict range_agg xmlagg},
		'... the aggregates with no combine function');
	is($cls->(q{a.aggkind <> 'n'}), q{cume_dist dense_rank mode percent_rank percentile_cont percentile_disc rank}, '... the ordered-set and hypothetical-set aggregates');
	is($cls->(q{a.aggkind = 'n' AND a.aggcombinefn <> 0 AND a.aggtranstype = 'internal'::regtype AND (a.aggserialfn = 0 OR a.aggdeserialfn = 0)}), '',
		'... and none of them has an internal state without serialization');

	# one call of every aggregate of the class, over the whole table, from node 1: the plan and the answer
	my %call = (
		any_value => q{any_value(a) IS NOT NULL}, array_agg => q{array_agg(b)}, avg => q{avg(n), avg(b), avg(iv)},
		bit_and => q{bit_and(sm)}, bit_or => q{bit_or(bt)}, bit_xor => q{bit_xor(sm)}, bool_and => q{bool_and(bo)}, bool_or => q{bool_or(bo)},
		corr => q{corr(f, b)}, count => q{count(*), count(t)}, covar_pop => q{covar_pop(b, a)}, covar_samp => q{covar_samp(b, a)},
		every => q{every(bo)}, max => q{max(t), max(n), max(ts)}, min => q{min(t), min(d), min(f)},
		range_intersect_agg => q{range_intersect_agg(int4range(a % 5, a % 5 + 30))},
		regr_avgx => q{regr_avgx(b, a)}, regr_avgy => q{regr_avgy(b, a)}, regr_count => q{regr_count(b, a)}, regr_intercept => q{regr_intercept(b, a)},
		regr_r2 => q{regr_r2(b, a)}, regr_slope => q{regr_slope(b, a)}, regr_sxx => q{regr_sxx(b, a)}, regr_sxy => q{regr_sxy(b, a)}, regr_syy => q{regr_syy(b, a)},
		stddev => q{stddev(n), stddev(b)}, stddev_pop => q{stddev_pop(n)}, stddev_samp => q{stddev_samp(f)},
		string_agg => q{string_agg(t, ',')}, sum => q{sum(a), sum(n), sum(b8), sum(iv), sum(f)},
		var_pop => q{var_pop(n)}, var_samp => q{var_samp(b)}, variance => q{variance(f)},
	);
	is(join(' ', sort keys %call), $capable, 'the test calls every aggregate of the class');
	for my $name (sort keys %call)
	{
		my $sql = "SELECT $call{$name} FROM public.s1";
		my $plan = plan_of($n1, $sql);
		like($plan, qr/Finalize/, "$name: the plan is a partial aggregation");
		like($plan, qr/\Q$FS\E/, "$name: over the fragment scan");
		$c->compare("class: $name", $sql, ordered => 0, plans => ['pushed', 'scan'], profiles => 0, nodes => [0, 2],
			normalize => ($name eq 'array_agg' ? { floats => 'auto', sort_arrays => [1] } : $name eq 'string_agg' ? { floats => 'auto', sort_strings => { 1 => ',' } } : 1));
	}
}

# ---------------------------------------------------------------------------
# D. parameters, prepared statements, cursors, EXPLAIN, the switches
# ---------------------------------------------------------------------------
{
	my $G = 'SET plan_cache_mode = force_generic_plan; ';
	my $pre = q{PREPARE pp(int, text) AS SELECT a % 4, count(*), sum(b), max(t) FROM public.s1 WHERE b > $1 AND t >= $2 GROUP BY 1 ORDER BY 1 NULLS LAST; };
	my $sql = q{EXECUTE pp(7, 'v1')};
	my ($rs, $os) = run_sql($single, $pre . $sql);
	for my $nd (@nodes)
	{
		for my $mode ('', $G)
		{
			my ($rc, $out, $err) = run_sql($nd, $mode . $pre . $sql);
			is($rc, 0, 'a prepared partial aggregation runs on ' . $nd->name . ($mode ? ' (generic plan)' : '')) or diag($err);
			is($out, $os, '... and its answer is the single node\'s');
		}
	}
	like((run_sql($n1, $G . $pre . q{EXPLAIN (COSTS OFF) EXECUTE pp(7, 'v1')}))[1], qr/Finalize/, 'a generic plan is a partial aggregation too');
	my $plan = plan_of($n1, q{SELECT a, count(*) FROM public.s1 WHERE b > 7 AND t >= 'v1' GROUP BY a});
	like($plan, qr/Finalize/, 'EXPLAIN shows Finalize over the fragment scan');
	like($plan, qr/Pushed: partial aggregate/, '... and what is pushed');
	like($plan, qr/Remote SQL: SELECT a, count\(\*\) FROM public\.s1 WHERE .* GROUP BY a/, '... and the text every group runs');
	unlike($plan, qr/HAVING|ORDER BY|LIMIT/, '... which holds nothing that the node that combines applies');
	my ($rc, $o, $e) = run_sql($n1, q{EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) SELECT a, count(*) FROM public.s1 GROUP BY a});
	is($rc, 0, 'EXPLAIN ANALYZE runs') or diag($e);
	like($o, qr/Rows from groups:/, '... and shows the rows of the groups');
	like(plan_of($n1, q{SELECT a, count(*) FROM public.s1 GROUP BY a}, 'SET kingres_shard.enable_partial_aggregation = off; '), qr/Distributed Scan/, 'enable_partial_aggregation = off: the scan answers');
	like(plan_of($n1, q{SELECT a, count(*) FROM public.s1 GROUP BY a}, 'SET kingres_shard.enable_pushdown = off; '), qr/Distributed Scan/, 'enable_pushdown = off: the scan answers');
	like(plan_of($n1, q{SELECT a, count(*) FROM public.s1 GROUP BY a}, 'SET kingres_shard.enable_pushdown_closed = off; '), qr/Finalize/, 'enable_pushdown_closed = off does not switch the partial aggregation off');
	like(plan_of($n1, q{SELECT s1.id, s2.c FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE s1.a > 5}, 'SET kingres_shard.enable_partial_aggregation = off; '), qr/Fragment Scan/, 'enable_partial_aggregation = off does not switch the closed query off');
	unlike(plan_of($n1, q{SELECT s1.id, s2.c FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE s1.a > 5}, 'SET kingres_shard.enable_pushdown_closed = off; '), qr/Fragment Scan/, 'with the closed query off a join on the key is not shipped either');
	# a cursor, read in pieces and closed early
	($rc, $o, $e) = run_sql($n1, q{BEGIN; DECLARE cur NO SCROLL CURSOR FOR SELECT a, count(*) FROM public.s1 GROUP BY a ORDER BY a NULLS LAST; FETCH 3 FROM cur; FETCH 2 FROM cur; CLOSE cur; COMMIT});
	is($rc, 0, 'a cursor over a partial aggregation') or diag($e);
	my ($rs2, $os2) = run_sql($single, q{SELECT a, count(*) FROM public.s1 GROUP BY a ORDER BY a NULLS LAST LIMIT 5});
	is($o, $os2, '... gives the first rows of the single node\'s answer');
}

# ---------------------------------------------------------------------------
# E. forged fragments
# ---------------------------------------------------------------------------
{
	my $PLACE = $n1->safe_psql('postgres', q{SELECT kingres.internal_placement_hash(1)});
	my $as_node = sub {
		my ($node, $id, $sql) = @_;
		my $tok = $node->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, $id)");
		local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
		return run_sql($node, "\\set VERBOSITY verbose\nBEGIN;\n$sql;\nROLLBACK;");
	};
	my $frag = sub { my ($place, $sql) = @_; return "/* kingres:partial-fragment place=$place */ DECLARE ks_forged NO SCROLL CURSOR FOR $sql"; };
	my $plan = plan_of($n1, q{SELECT a, count(*), sum(b) FROM public.s1 GROUP BY a});
	my ($text) = $plan =~ /^\s*Remote SQL: (.*)$/m;
	ok(defined $text, 'the text that the origin ships is known');
	for my $nd ([ $n2, 2 ], [ $n3, 3 ])
	{
		my ($rc, $out, $err) = $as_node->($nd->[0], $nd->[1], $frag->($PLACE, $text));
		is($rc, 0, 'node ' . $nd->[1] . ' accepts the text that the origin ships') or diag($err);
	}
	my ($rc, $out, $err) = $as_node->($n2, 2, $frag->(12345, $text));
	isnt($rc, 0, 'a placement that is not the group\'s is refused');
	like($err, qr/KS001/, '... with the retryable error');
	for my $bad (
		q{SELECT a, count(*) FROM public.s1 GROUP BY a HAVING count(*) > 1},
		q{SELECT a, count(*) FROM public.s1 GROUP BY a ORDER BY a},
		q{SELECT a, count(*) FROM public.s1 GROUP BY a LIMIT 3},
		q{SELECT a, count(DISTINCT b) FROM public.s1 GROUP BY a},
		q{SELECT a, percentile_cont(0.5) WITHIN GROUP (ORDER BY b) FROM public.s1 GROUP BY a},
		q{SELECT a, json_agg(b) FROM public.s1 GROUP BY a},
		q{SELECT a, count(*) FROM public.s1 GROUP BY GROUPING SETS ((a), ())},
		q{SELECT a, rank() OVER (ORDER BY a) FROM public.s1},
		q{SELECT DISTINCT a FROM public.s1},
		q{SELECT a, b FROM public.s1},
		q{SELECT count(*) FROM public.s1 x JOIN public.far y ON y.id = x.id},
		q{SELECT a, sum(b) + 1 FROM public.s1 GROUP BY a},
	)
	{
		($rc, $out, $err) = $as_node->($n2, 2, $frag->($PLACE, $bad));
		isnt($rc, 0, "a forged partial fragment is refused: $bad");
		like($err, qr/0A000/, '... with 0A000');
		unlike($err, qr/KS001/, '... which is not the retryable error');
	}
}

# ---------------------------------------------------------------------------
# F. faults and moves of a partial aggregation (the machinery is the fragment scan's: t/125 has the closed query)
# ---------------------------------------------------------------------------
{
	my $tmp = $PostgreSQL::Test::Utils::tmp_check;
	my @m = make_cluster('pf', 3, logical => 1, extra => "kingres_shard.enable_partial_aggregation = on
");
	my $m1 = $m[0];
	$m1->safe_psql('postgres', q{
		CREATE TABLE public.s (id int PRIMARY KEY, g int, v text);
		SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12);
		INSERT INTO public.s SELECT g, g % 7, 'v' || g FROM generate_series(1, 240) g;
	});
	$_->safe_psql('postgres', 'ANALYZE') for @m;
	my $Q = q{SELECT g, count(*), sum(id), max(v) FROM public.s GROUP BY g ORDER BY g};
	my ($rb, $base, $eb) = run_sql($m1, $Q);
	is($rb, 0, 'the partial aggregation of the fault tests runs') or diag($eb);
	like(plan_of($m1, $Q), qr/Finalize/, '... and is a partial aggregation');
	for my $point (qw(frag_before_probe frag_after_probe_before frag_after_declare frag_local_open frag_after_probe_after
					  frag_first_fetch frag_before_fetch frag_local_fetch frag_wait_batch))
	{
		my ($rc, $out, $err) = run_sql($m1, "SET kingres_shard.scan_fetch_size = 20; SET kingres_shard.fault_injection = '$point:error'; $Q");
		isnt($rc, 0, "an error at $point fails the partial aggregation");
		like($err, qr/injected failure at \Q$point\E/, '... with that error (the point was reached)');
		my ($rc2, $o2, $e2) = run_sql($m1, $Q);
		is($o2, $base, "the next statement after $point gives the baseline") or diag($e2);
	}
	for my $nd (@m)
	{
		is($nd->safe_psql('postgres', q{SELECT count(*) FROM pg_cursors}), 0, 'no cursor is open on ' . $nd->name);
		is(prepared_count($nd), 0, 'nothing is prepared on ' . $nd->name);
	}

	# a bucket moves while the statement waits after its cursors are open, and between a generic plan and its run
	my $coloc = q{(SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.s'::regclass)};
	my $b = $m1->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = $coloc AND group_id = 1");
	my $gate = "$tmp/gate_pa_move";
	append_to_file($gate, 'x');
	my $sess = start_psql($m1, "SET kingres_shard.fault_injection = 'frag_after_declare:wait:$gate'; $Q");
	ok(wait_for_fault_file("$gate.reached", 60), 'the partial aggregation waits after its cursors are open');
	my $mover = start_psql($m1, "SELECT kingres.move_bucket($coloc, $b, 2)");
	sleep(3);
	unlink $gate;
	my ($mo, $me) = finish_psql($mover);
	my ($so, $se) = finish_psql($sess);
	$so =~ s/\A(?:SET\n)+//;
	$so =~ s/\n+\z//;
	if ($se =~ /ERROR/)
	{
		like($se, qr/KS001|bucket map|map changed|changed/, 'the statement that a move overtook fails with the retryable error');
	}
	else
	{
		is($so, $base, 'the statement that a move did not disturb gives the baseline');
	}
	my ($rc3, $o3, $e3) = run_sql($m1, $Q);
	is($o3, $base, 'after the move the answer is the same') or diag($e3);
	my $s = $m1->background_psql('postgres', on_error_stop => 0);
	$s->query_safe('SET plan_cache_mode = force_generic_plan');
	$s->query_safe(q{PREPARE pg1 AS SELECT g, count(*), sum(id) FROM public.s GROUP BY g ORDER BY g});
	$s->query_safe('EXECUTE pg1');
	my $b2 = $m1->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = $coloc AND group_id = 2");
	$m1->safe_psql('postgres', "SELECT kingres.move_bucket($coloc, $b2, 1)");
	$s->{stderr} = '';
	my $after = $s->query('EXECUTE pg1');
	my $e = $s->{stderr};
	$s->{stderr} = '';
	if ($e =~ /ERROR/) { like($e, qr/KS001|bucket map|changed/, 'a plan made before a move fails with the retryable error'); }
	else { like($after, qr/^0\|/m, 'a plan made before a move is replanned and right'); }
	$s->quit;

	# a group that is gone: the statement fails, nothing hangs, and it works when the node is back
	$m[2]->stop('immediate');
	my ($rc4, $o4, $e4) = run_sql($m1, "SET kingres_shard.node_timeout = 5; $Q");
	isnt($rc4, 0, 'a group that is down fails the partial aggregation');
	$m[2]->start;
	my ($rc5, $o5, $e5) = run_sql($m1, $Q);
	is($o5, $base, 'with the group back the answer is the baseline') or diag($e5);
	release_node($_) for @m;
}


done_testing();
