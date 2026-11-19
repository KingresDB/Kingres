# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/123_insel_generator.pl
#
# Step 5 of phase 2b (design/shard-phase2b.md, 16.2 and 16.3): a seeded generator of colocated and
# almost-colocated INSERT ... SELECT statements, run against a single ordinary node that gets the same
# statements (the twin).  A statement of a colocated shape must be shipped whole and give the rows, the
# RETURNING output and the error of the twin; a statement of an almost-colocated shape (a key that is
# computed, a join on another column, a key from the nullable side of an outer join, another colocation
# group, a grouping that is not by the key, LIMIT, a sub-select, a local table, a stable or volatile
# function, ...) must NOT be shipped, must take the fallback (the row router) and must still be right.
#
# The statements are made by KingresShardCompare::Generator (KS_SEED sets the seed; the seed is printed, so a
# failure is repeated by setting it).  Each statement is run from a node chosen by the generator, after the
# target table is reset to a fixed state (so ON CONFLICT has rows to meet), and then the whole target table
# on the cluster is compared with the twin's.  A statement that fails fails on both with the same error.
#
# Counts, per run: KS_COUNT statements (default 600): 18 colocated shapes and 14 almost-colocated shapes, each
# shape with its slots (restriction, ON CONFLICT clause, RETURNING).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql prepared_count);
use KingresShardCompare;

my $c = KingresShardCompare->new('ig');
my @nodes = $c->nodes;
my $single = $c->single;

sub table
{
	my ($create, $dist) = @_;
	$c->on_cluster($create . ($dist // ''));
	$c->on_single($create);
}

# ---------------------------------------------------------------------------
# the data
# ---------------------------------------------------------------------------
table(q{CREATE TABLE public.s1 (id int PRIMARY KEY, a int, b int, t text)},
	q{; SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.s2 (id int PRIMARY KEY, c int, t text)},
	q{; SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s1')});
table(q{CREATE TABLE public.tg (id int PRIMARY KEY, a int, b int, t text, n int DEFAULT 5)},
	q{; SELECT kingres.create_distributed_table('public.tg', 'id', colocate_with => 'public.s1')});
table(q{CREATE TABLE public.far (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.far', 'id', bucket_count => 7)});
table(q{CREATE TABLE public.r (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.r')});
$c->on_every_node(q{CREATE TABLE public.loc (x int, y int, z text)});
$c->on_single(q{CREATE TABLE public.loc (x int, y int, z text)});
$c->on_both(q{INSERT INTO public.r VALUES (0, 'L0'), (1, 'L1'), (2, 'L2'), (3, 'L3'), (4, 'L4'), (5, 'L5'), (6, 'L6')});
$c->on_both(q{INSERT INTO public.s1 SELECT g, CASE WHEN g % 17 = 0 THEN NULL ELSE (g * 7) % 50 END, CASE WHEN g % 19 = 0 THEN NULL ELSE (g * 13) % 30 END,
	CASE WHEN g % 23 = 0 THEN NULL ELSE 'v' || g::text END FROM generate_series(1, 300) g});
$c->on_both(q{INSERT INTO public.s2 SELECT g, CASE WHEN g % 11 = 0 THEN NULL ELSE g % 9 END, CASE WHEN g % 13 = 0 THEN NULL ELSE 's' || g::text END FROM generate_series(1, 300, 2) g});
$c->on_both(q{INSERT INTO public.far SELECT g, 'f' || g::text FROM generate_series(1, 60) g});
$c->on_every_node(q{INSERT INTO public.loc SELECT g, g % 5, 'l' || g::text FROM generate_series(1, 40) g});
$c->on_single(q{INSERT INTO public.loc SELECT g, g % 5, 'l' || g::text FROM generate_series(1, 40) g});

# ---------------------------------------------------------------------------
# the shapes
# ---------------------------------------------------------------------------
my @range1 = ('s1.id <= 60', 's1.id BETWEEN 40 AND 140', 's1.id % 3 = 0', 's1.id > 200 AND s1.id < 260', 's1.a > 20', 's1.t LIKE \'v1%\'',
	'(s1.a + s1.b) % 2 = 0', 'true', 's1.a IS NULL OR s1.b IS NULL', 's1.id IN (1, 2, 3, 50, 100, 101, 250)', 's1.b < 10 AND s1.id <> 5');
my @conflict = ('', 'ON CONFLICT DO NOTHING', 'ON CONFLICT (id) DO NOTHING', 'ON CONFLICT ON CONSTRAINT tg_pkey DO NOTHING',
	'ON CONFLICT (id) DO UPDATE SET n = tg.n + 1', 'ON CONFLICT (id) DO UPDATE SET a = excluded.a, n = tg.n + excluded.id WHERE tg.id % 2 = 0',
	'ON CONFLICT (id) DO UPDATE SET t = upper(excluded.t), b = coalesce(excluded.b, 0) + tg.b', 'ON CONFLICT (id) DO UPDATE SET n = tg.n * 2 WHERE excluded.a > 10');
my @conflict_plain = ('', 'ON CONFLICT DO NOTHING', 'ON CONFLICT (id) DO NOTHING');
my @ret = ('', '', '', ' RETURNING id, a, n', ' RETURNING id, n + 1, upper(t)', ' RETURNING *');
my $T = 'INSERT INTO public.tg (id, a, b, t)';
my %SLOTS = (range => \@range1, conflict => \@conflict, plain => \@conflict_plain, ret => \@ret);

my @shapes = (
	# ---- colocated: shipped whole
	[ 'ship:copy',          "$T SELECT s1.id, s1.a, s1.b, s1.t FROM public.s1 WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:copy',          "$T SELECT s1.id, s1.a, s1.b, s1.t FROM public.s1 WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:join',          "$T SELECT s1.id, s1.a, s2.c, s2.t FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:join_where',    "$T SELECT s1.id, s1.a, s2.c, s2.t FROM public.s1, public.s2 WHERE s1.id = s2.id AND ({range}) {conflict}{ret}" ],
	[ 'ship:join_ref',      "$T SELECT s1.id, s1.a, r.k, r.label FROM public.s1 JOIN public.r ON r.k = s1.a % 7 WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:three_way',     "$T SELECT s1.id, s2.c, r.k, r.label FROM public.s1 JOIN public.s2 ON s2.id = s1.id JOIN public.r ON r.k = coalesce(s2.c, 0) % 7 WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:left_join',     "$T SELECT s1.id, s1.a, coalesce(s2.c, 0), coalesce(s2.t, 'none') FROM public.s1 LEFT JOIN public.s2 ON s2.id = s1.id AND s2.c > 3 WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:right_join',    "$T SELECT s1.id, s2.c, s1.b, s1.t FROM public.s2 RIGHT JOIN public.s1 ON s2.id = s1.id WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:group_key',     "$T SELECT s1.id, count(*), sum(s1.a), max(s1.t) FROM public.s1 WHERE ({range}) GROUP BY s1.id {plain}{ret}" ],
	[ 'ship:having',        "$T SELECT s1.id, count(s1.a), min(s1.b), max(s1.t) FROM public.s1 WHERE ({range}) GROUP BY s1.id HAVING count(*) >= 1 {plain}{ret}" ],
	[ 'ship:group_join',    "$T SELECT s1.id, count(s2.c), sum(s2.c), min(s2.t) FROM public.s1 LEFT JOIN public.s2 ON s2.id = s1.id WHERE ({range}) GROUP BY s1.id {plain}{ret}" ],
	[ 'ship:distinct',      "$T SELECT DISTINCT s1.id, s1.a, s1.b, s1.t FROM public.s1 WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:expressions',   "$T SELECT s1.id, s1.a + s1.b, s1.a * 2, upper(s1.t) || 'x' FROM public.s1 WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:case',          "$T SELECT s1.id, CASE WHEN s1.a > 10 THEN s1.a ELSE s1.b END, abs(coalesce(s1.b, -1)), left(coalesce(s1.t, ''), 3) FROM public.s1 WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:self_join',     "$T SELECT x.id, x.a, y.b, y.t FROM public.s1 x JOIN public.s1 y ON y.id = x.id AND y.a >= x.a WHERE ({range_x}) {conflict}{ret}" ],
	[ 'ship:literals',      "$T SELECT s1.id, 7, NULL, 'lit' FROM public.s1 WHERE ({range}) {conflict}{ret}" ],
	[ 'ship:from_target',   "$T SELECT tg.id, tg.a, tg.b, tg.t FROM public.tg WHERE tg.id % 2 = 0 {conflict}{ret}" ],
	[ 'ship:other_order',   "INSERT INTO public.tg (t, b, a, id) SELECT s1.t, s1.b, s1.a, s1.id FROM public.s1 WHERE ({range}) {conflict}{ret}" ],
	[ 'fall:literal_reordered', "INSERT INTO public.tg (t, b, id) SELECT 'lit', s1.b, s1.id FROM public.s1 WHERE ({range}) {plain}{ret}" ],
	# ---- almost colocated: the fallback
	[ 'fall:key_expression',  "$T SELECT s1.id + 1000, s1.a, s1.b, s1.t FROM public.s1 WHERE ({range}) {plain}{ret}" ],
	[ 'fall:join_non_key',    "$T SELECT s1.id, s1.a, s2.c, s2.t FROM public.s1 JOIN public.s2 ON s2.id = s1.a WHERE ({range}) {plain}{ret}" ],
	[ 'fall:nullable_key',    "$T SELECT s2.id, s1.a, s1.b, s1.t FROM public.s1 LEFT JOIN public.s2 ON s2.id = s1.id WHERE ({range}) AND s2.id IS NOT NULL {plain}{ret}" ],
	[ 'fall:other_colocation', "$T SELECT s1.id, 1, 2, far.w FROM public.s1 JOIN public.far ON far.id = s1.id WHERE ({range}) {plain}{ret}" ],
	[ 'fall:group_non_key',   "$T SELECT s1.a, count(*), 0, 'g' FROM public.s1 WHERE ({range}) AND s1.a IS NOT NULL GROUP BY s1.a {plain}{ret}" ],
	[ 'fall:limit',           "$T SELECT s1.id, s1.a, s1.b, s1.t FROM public.s1 WHERE ({range}) ORDER BY s1.id LIMIT 7 {plain}{ret}" ],
	[ 'fall:sub_select',      "$T SELECT s1.id, s1.a, s1.b, s1.t FROM public.s1 WHERE ({range}) AND s1.id IN (SELECT id FROM public.s2 WHERE c > 3) {plain}{ret}" ],
	[ 'fall:sub_select_non_key', "$T SELECT s1.id, s1.a, s1.b, s1.t FROM public.s1 WHERE ({range}) AND s1.a IN (SELECT c FROM public.s2 WHERE c > 3) {plain}{ret}" ],
	[ 'fall:local_source',    "$T SELECT loc.x, loc.y, 0, loc.z FROM public.loc {plain}{ret}" ],
	[ 'fall:aggregate_all',   "$T SELECT count(*), sum(s1.a), 0, 'agg' FROM public.s1 WHERE ({range}) {plain}{ret}" ],
	[ 'fall:stable',          "$T SELECT s1.id, s1.a, s1.b, s1.t FROM public.s1 WHERE ({range}) AND now() > timestamptz '2000-01-01' {plain}{ret}" ],
	[ 'fall:volatile',        "$T SELECT s1.id, s1.a, s1.b, s1.t FROM public.s1 WHERE ({range}) AND random() < 2 {plain}{ret}" ],
	[ 'fall:cast_via_text',   "$T SELECT s1.id, s1.a, s1.b, s1.a::text FROM public.s1 WHERE ({range}) {plain}{ret}" ],
	[ 'fall:full_join',       "$T SELECT s1.id, s1.a, s2.c, s2.t FROM public.s1 FULL JOIN public.s2 ON s2.id = s1.id WHERE ({range}) AND s1.id IS NOT NULL {plain}{ret}" ],
	[ 'fall:left_join_non_key', "$T SELECT s1.id, s1.a, s2.c, s2.t FROM public.s1 LEFT JOIN public.s2 ON s2.id = s1.a WHERE ({range}) {plain}{ret}" ],
	[ 'fall:inequality_join', "$T SELECT s1.id, s1.a, s2.c, s2.t FROM public.s1 JOIN public.s2 ON s2.id <= s1.id AND s2.c = 3 WHERE ({range}) AND s1.id < 12 {plain}{ret}" ],
	[ 'fall:whole_row', "$T SELECT s1.id, s1.a, s1.b, s1.t FROM public.s1 WHERE ({range}) AND s1 IS NOT NULL {plain}{ret}" ],
	[ 'fall:left_join_where_key', "$T SELECT s1.id, s1.a, s2.c, s2.t FROM public.s1 LEFT JOIN public.s2 ON s2.c = s1.a WHERE ({range}) AND s1.id = s2.id {plain}{ret}" ],
	[ 'fall:replicated_preserved', "$T SELECT s1.id, s1.a, o.c, o.t FROM public.s1 LEFT JOIN (public.r LEFT JOIN public.s2 o ON o.id = r.k) ON s1.id = o.id WHERE ({range}) {plain}{ret}" ],
	[ 'fall:reference_only',  "$T SELECT r.k + 500, r.k, r.k, r.label FROM public.r {plain}{ret}" ],
);

my %templates;
my @templates;
for my $s (@shapes)
{
	my ($name, $sql) = @$s;
	my %slots = %SLOTS;
	$slots{range_x} =[ map { s/s1\./x./gr } @range1 ];
	push @templates, { name => $name, sql => $sql, slots => \%slots };
}
my $g = $c->generator(templates => \@templates);
my $COUNT = $ENV{KS_COUNT} // 600;

# ---------------------------------------------------------------------------
# run
# ---------------------------------------------------------------------------
sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

sub err_lines
{
	my ($e) = @_;
	return join("\n", map { s/^psql:<stdin>:\d+: //r =~ s/_b\d{4}//gr =~ s/\d+/N/gr }
		grep { /^(?:psql:<stdin>:\d+: )?(?:ERROR|DETAIL):/ } split /\n/, $e);
}

sub local_rows
{
	my ($node, $table, $group) = @_;
	return $node->safe_psql('postgres', qq{
		SET kingres_shard.enable_routing = off;
		SELECT count(*) || '|' || count(*) FILTER (WHERE m.group_id = $group)
		  FROM $table k JOIN kingres.bucket_map m
		    ON m.colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = '$table'::regclass)
		   AND m.bucket = kingres.bucket_for('$table'::regclass, k.id)});
}

sub placement_ok
{
	my ($name) = @_;
	my $total = 0;
	my $i = 0;
	for my $n (@nodes)
	{
		my ($all, $mine) = split /\|/, local_rows($n, 'public.tg', $i);
		is($mine, $all, "$name: every row of node " . ($i + 1) . " belongs to its group");
		$total += $all;
		$i++;
	}
	my $want = $single->safe_psql('postgres', 'SELECT count(*) FROM public.tg');
	is($total, $want, "$name: the rows of the groups are the rows of the twin, each once");
}

my $RESET = q{TRUNCATE public.tg; INSERT INTO public.tg SELECT id, 1, 2, 'seed' || id::text, 5 FROM public.s1 WHERE id % 4 = 0;};
my %count = (ship => 0, fall => 0, ok => 0, failed => 0, with_conflict_update => 0, with_returning => 0);
my %by_shape;

for my $i (1 .. $COUNT)
{
	my $q = $g->query;
	my ($kind, $shape) = split /:/, $q->{name};
	my $sql = $q->{sql};
	$sql =~ s/\s+/ /g;
	$sql =~ s/ ;?$//;
	my $node = $nodes[ $g->next_int(3) ];
	my $label = "#$i $q->{name} from " . $node->name;

	$c->on_both($RESET);
	my $plan = plan_of($node, $sql);
	if ($kind eq 'ship')
	{
		like($plan, qr/Kingres Fan-out Write.*Pushed: colocated INSERT \.\.\. SELECT/s, "$label: shipped whole") or diag($sql);
	}
	else
	{
		unlike($plan, qr/Kingres Fan-out Write/, "$label: not shipped (the fallback takes it)") or diag($sql);
	}
	$count{$kind}++;
	$by_shape{ $q->{name} }++;
	$count{with_conflict_update}++ if $sql =~ /DO UPDATE/;
	$count{with_returning}++ if $sql =~ /RETURNING/;

	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($node, $sql);
	is($rd == 0 ? 0 : 1, $rs == 0 ? 0 : 1, "$label: succeeds or fails as the twin does") or diag("$sql\ncluster: $ed\ntwin: $es");
	if ($rs == 0 && $rd == 0)
	{
		$count{ok}++;
		is(join("\n", sort split /\n/, $od), join("\n", sort split /\n/, $os), "$label: RETURNING gives the rows of the twin");
	}
	else
	{
		$count{failed}++;
		is(err_lines($ed), err_lines($es), "$label: the error is the twin's");
	}
	$c->compare_tables('public.tg', order_by => 'id', name => "$label: the target table",
		nodes => ($i % 6 == 0 ? [0, 1, 2] : [ $g->next_int(3) ]));
	# the shipped statements again through the row router: the same answer (a statement with DO UPDATE has no router)
	if ($kind eq 'ship' && $sql !~ /DO UPDATE/ && $g->next_int(4) == 0 && $rs == 0)
	{
		$c->on_both($RESET);
		my ($r2, $o2, $e2) = run_sql($node, "SET kingres_shard.enable_colocated_insert_select = off; $sql");
		is($r2, 0, "$label: the same statement through the row router works") or diag($e2);
		is(join("\n", sort split /\n/, $o2), join("\n", sort split /\n/, $os), "$label: ... and returns what the twin returns");
		$c->on_single($RESET);
		run_sql($single, $sql);
		$c->compare_tables('public.tg', order_by => 'id', name => "$label: the router's table", nodes => [ $g->next_int(3) ]);
	}
	placement_ok("after #$i") if $i % 90 == 0;
}

note("statements: $COUNT; shipped shapes: $count{ship}; fallback shapes: $count{fall}; succeeded: $count{ok}; failed as the twin: $count{failed}; with DO UPDATE: $count{with_conflict_update}; with RETURNING: $count{with_returning}");
note(join(', ', map { "$_ x $by_shape{$_}" } sort keys %by_shape));
cmp_ok($count{ship}, '>=', $COUNT / 4, 'a good part of the statements is shipped');
cmp_ok($count{fall}, '>=', $COUNT / 4, 'a good part of the statements is almost colocated');
cmp_ok($count{ok}, '>=', $COUNT / 3, 'most statements succeed (the comparison of errors is not the whole test)');
cmp_ok($count{with_conflict_update}, '>=', 10, 'DO UPDATE statements are generated');
placement_ok('at the end');
is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;
done_testing();
