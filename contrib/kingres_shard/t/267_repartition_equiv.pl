# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/267_repartition_equiv.pl
#
# Phase 12 (design/analytics-phase12-queries.md, 14.2 "060_repartition_equiv"): a seeded, generated list of two-, three- and four-table
# joins on columns that are not the distribution column, with the join types the repartitioned join accepts (inner joins, one LEFT JOIN to
# the moved table), reference tables, aggregates, HAVING, order and limit.  The answer under each plan profile must be the single node's
# and the row-moving scan's: repartitioned (the switch is on "force"), repartitioned without the partial aggregation, with
# enable_repartition_join = off, and the scan.  A fixed list of statements is checked to be planned as a repartitioned join, so that the
# test cannot pass by never choosing the path; the generated list reports how many of its statements were repartitioned.
# KS_SEED sets the seed (it is printed); KS_COUNT the number of statements (default 150).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('req', extra => "kingres_shard.enable_repartition_join = force
kingres_shard.enable_partial_aggregation = on
");
my @nodes = $c->nodes;
my $FS = 'Kingres Fragment Scan';
my $RJ = 'Pushed: repartitioned join';

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }

table(q{CREATE TABLE public.o (id int PRIMARY KEY, cust int, far int, amount int, note text, f float8)},
	q{; SELECT kingres.create_distributed_table('public.o', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.cu (id int PRIMARY KEY, region int, name text)},
	q{; SELECT kingres.create_distributed_table('public.cu', 'id', colocate_with => 'public.o')});
table(q{CREATE TABLE public.it (id int PRIMARY KEY, ord int, sku int)},
	q{; SELECT kingres.create_distributed_table('public.it', 'id', colocate_with => 'public.o')});
table(q{CREATE TABLE public.fr (id int PRIMARY KEY, w int, label text)},
	q{; SELECT kingres.create_distributed_table('public.fr', 'id', bucket_count => 7)});
table(q{CREATE TABLE public.rg (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.rg')});
$c->on_both(q{INSERT INTO public.rg VALUES (0, 'R0'), (1, 'R1'), (2, 'R2'), (3, 'R3'), (4, 'R4'), (5, 'R5')});
$c->on_both(q{INSERT INTO public.cu SELECT g, CASE WHEN g % 31 = 0 THEN NULL ELSE g % 6 END, CASE WHEN g % 29 = 0 THEN NULL ELSE 'c' || g END FROM generate_series(1, 240) g});
# a quarter of the orders have no customer, some name one that does not exist, the others one of 240; many rows share a key
$c->on_both(q{INSERT INTO public.o SELECT g, CASE WHEN g % 4 = 0 THEN NULL WHEN g % 11 = 0 THEN 1000 + g ELSE 1 + (g * 7) % 240 END,
	CASE WHEN g % 13 = 0 THEN NULL ELSE 1 + g % 90 END, CASE WHEN g % 17 = 0 THEN NULL ELSE g % 97 END, 'n' || (g % 20), (g % 50) / 4.0 FROM generate_series(1, 2400) g});
$c->on_both(q{INSERT INTO public.it SELECT g, CASE WHEN g % 19 = 0 THEN NULL ELSE 1 + (g * 7) % 2400 END, g % 13 FROM generate_series(1, 1200) g});
$c->on_both(q{INSERT INTO public.fr SELECT g, g % 9, 'f' || g FROM generate_series(1, 80) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

my @PLANS = ('pushed',
	{ name => 'nopartial', settings => [ [ 'kingres_shard.enable_partial_aggregation', 'off' ] ] },
	{ name => 'norepartition', settings => [ [ 'kingres_shard.enable_repartition_join', 'off' ] ] },
	'scan');

# ---------------------------------------------------------------------------
# the fixed list: every statement is planned as a repartitioned join, and agrees
# ---------------------------------------------------------------------------
my @fixed = (
	[ 'two tables', q{SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust} ],
	[ 'two tables, the moved one written second', q{SELECT cu.id, o.id FROM public.cu JOIN public.o ON o.cust = cu.id WHERE cu.id < 90} ],
	[ 'three tables', q{SELECT o.id, cu.name, it.sku FROM public.o JOIN public.cu ON cu.id = o.cust JOIN public.it ON it.id = cu.id} ],
	[ 'four tables with a reference table', q{SELECT o.id, cu.name, it.sku, rg.label FROM public.o JOIN public.cu ON cu.id = o.cust JOIN public.it ON it.id = cu.id JOIN public.rg ON rg.k = cu.region} ],
	[ 'another colocation group stays', q{SELECT o.id, fr.w FROM public.o JOIN public.fr ON fr.id = o.far} ],
	[ 'a LEFT JOIN to the moved table', q{SELECT cu.id, o.id FROM public.cu LEFT JOIN public.o ON o.cust = cu.id} ],
	[ 'an aggregate', q{SELECT cu.region, count(*), sum(o.amount) FROM public.o JOIN public.cu ON cu.id = o.cust GROUP BY cu.region} ],
	[ 'an aggregate, HAVING, order and limit', q{SELECT o.note, count(*) AS n FROM public.o JOIN public.cu ON cu.id = o.cust GROUP BY o.note HAVING count(*) > 5 ORDER BY n DESC, o.note LIMIT 7} ],
	[ 'the moved table by another column', q{SELECT it.id, o.id FROM public.it JOIN public.o ON o.id = it.ord} ],
);
for my $f (@fixed)
{
	my ($name, $sql) = @$f;
	$c->compare("fixed: $name", $sql, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, $RJ ]);
}

# ---------------------------------------------------------------------------
# the generated list
# ---------------------------------------------------------------------------
my @po = ('true', 'o.id <= 800', 'o.amount > 40', 'o.note IN (\'n1\', \'n7\', \'n13\')', 'o.cust IS NULL OR o.amount < 10', 'o.id % 3 = 0',
	'o.far BETWEEN 10 AND 60', 'o.id = 6', 'o.id IN (3, 6, 9, 1001)', 'o.amount IS NULL');
my @pc = ('true', 'cu.id <= 120', 'cu.region = 2', 'cu.region IN (1, 4)', 'cu.name IS NOT NULL', 'cu.id % 2 = 0', 'cu.region IS NULL', 'cu.id = 17');
my @pi = ('true', 'it.sku < 6', 'it.id <= 700', 'it.ord IS NOT NULL');
my @pf = ('true', 'fr.w > 3', 'fr.id <= 50');
my @agg_oc = ('count(*)', 'count(o.amount), sum(o.amount)', 'min(o.note), max(cu.name)', 'avg(o.amount), count(cu.region)',
	'sum(o.f), max(o.f)', 'count(*) FILTER (WHERE o.amount > 30), min(cu.id)', 'count(DISTINCT o.note)', 'bool_or(o.amount > 90), min(o.cust)');
my @agg_co = ('count(o.id), sum(o.amount)', 'count(*), max(o.amount)', 'min(o.note), count(o.cust)');
my @having = ('', '', 'HAVING count(*) >= 1', 'HAVING count(*) > 10', 'HAVING max(o.amount) > 20');
my %SLOTS = (po => \@po, pc => \@pc, pi => \@pi, pf => \@pf, agg_oc => \@agg_oc, agg_co => \@agg_co, having => \@having,
	# an order that is total, so that the first rows are the same on every plan
	tail2 => [ '', ' ORDER BY 1, 2 LIMIT 30', ' ORDER BY 1 DESC, 2 LIMIT 20 OFFSET 5', ' ORDER BY 1, 2' ],
	tail3 => [ '', ' ORDER BY 1, 2, 3 LIMIT 40', ' ORDER BY 1, 2, 3' ],
	tailg => [ '', ' ORDER BY 1', ' ORDER BY 1 DESC LIMIT 3', ' ORDER BY 1 LIMIT 4 OFFSET 1' ],
	jt => [ 'JOIN', 'JOIN', 'JOIN', 'LEFT JOIN' ]);
my @shapes = (
	[ 'rep:two',        "SELECT o.id, cu.name, o.amount FROM public.o JOIN public.cu ON cu.id = o.cust WHERE ({po}) AND ({pc}){tail2}", 1, 1 ],
	[ 'rep:two_where',  "SELECT o.id, cu.region FROM public.o, public.cu WHERE cu.id = o.cust AND ({po}) AND ({pc})", 0, 1 ],
	[ 'rep:two_agg',    "SELECT cu.region, {agg_oc} FROM public.o JOIN public.cu ON cu.id = o.cust WHERE ({po}) AND ({pc}) GROUP BY cu.region {having}{tailg}", 1, 1 ],
	[ 'rep:two_aggo',   "SELECT o.note, {agg_oc} FROM public.o JOIN public.cu ON cu.id = o.cust WHERE ({po}) AND ({pc}) GROUP BY o.note {having}{tailg}", 1, 1 ],
	[ 'rep:agg_all',    "SELECT {agg_oc} FROM public.o JOIN public.cu ON cu.id = o.cust WHERE ({po}) AND ({pc})", 0, 1 ],
	[ 'rep:agg_key',    "SELECT cu.id, {agg_oc} FROM public.o JOIN public.cu ON cu.id = o.cust WHERE ({po}) AND ({pc}) GROUP BY cu.id {having}{tailg}", 1, 1 ],
	[ 'rep:three',      "SELECT o.id, cu.region, it.sku FROM public.o JOIN public.cu ON cu.id = o.cust JOIN public.it ON it.id = cu.id WHERE ({po}) AND ({pc}) AND ({pi}){tail3}", 1, 1 ],
	[ 'rep:three_agg',  "SELECT it.sku, cu.region, count(*), sum(o.amount) FROM public.o JOIN public.cu ON cu.id = o.cust JOIN public.it ON it.id = cu.id WHERE ({po}) AND ({pc}) AND ({pi}) GROUP BY it.sku, cu.region {having}{tail2}", 1, 1 ],
	[ 'rep:four_ref',   "SELECT o.id, cu.name, it.sku, rg.label FROM public.o JOIN public.cu ON cu.id = o.cust JOIN public.it ON it.id = cu.id JOIN public.rg ON rg.k = cu.region WHERE ({po}) AND ({pc}) AND ({pi})", 0, 1 ],
	[ 'rep:four_agg',   "SELECT rg.label, count(*), sum(it.sku), max(o.amount) FROM public.o JOIN public.cu ON cu.id = o.cust JOIN public.it ON it.id = cu.id JOIN public.rg ON rg.k = cu.region WHERE ({po}) AND ({pc}) AND ({pi}) GROUP BY rg.label {having}{tailg}", 1, 1 ],
	[ 'rep:ref_two',    "SELECT o.id, rg.label FROM public.o JOIN public.cu ON cu.id = o.cust JOIN public.rg ON rg.k = cu.region WHERE ({po}) AND ({pc})", 0, 1 ],
	[ 'rep:other_group', "SELECT o.id, fr.w, fr.label FROM public.o JOIN public.fr ON fr.id = o.far WHERE ({po}) AND ({pf}){tail3}", 1, 1 ],
	[ 'rep:other_agg',  "SELECT fr.w, {agg_oc} FROM public.o JOIN public.fr ON fr.id = o.far, public.cu WHERE cu.id = o.cust AND ({po}) AND ({pf}) GROUP BY fr.w {having}{tailg}", 1, 0 ],
	[ 'rep:left',       "SELECT cu.id, o.id, o.amount FROM public.cu LEFT JOIN public.o ON o.cust = cu.id AND ({po}) WHERE ({pc}){tail2}", 1, 1 ],
	[ 'rep:left_agg',   "SELECT cu.region, {agg_co} FROM public.cu LEFT JOIN public.o ON o.cust = cu.id AND ({po}) WHERE ({pc}) GROUP BY cu.region {having}{tailg}", 1, 1 ],
	[ 'rep:left_three', "SELECT cu.id, it.sku, o.id FROM public.cu JOIN public.it ON it.id = cu.id LEFT JOIN public.o ON o.cust = cu.id WHERE ({pc}) AND ({pi}){tail3}", 1, 1 ],
	[ 'rep:self',       "SELECT x.id, y.id FROM public.o x JOIN public.o y ON y.id = x.cust WHERE x.id < 400{tail2}", 1, 1 ],
	[ 'rep:by_other',   "SELECT it.id, o.id, o.amount FROM public.it JOIN public.o ON o.id = it.ord WHERE ({po}) AND ({pi}){tail3}", 1, 1 ],
	[ 'rep:by_other_agg', "SELECT it.sku, {agg_oc} FROM public.it JOIN public.o ON o.id = it.ord, public.cu WHERE cu.id = o.cust AND ({po}) AND ({pi}) GROUP BY it.sku {having}{tailg}", 1, 0 ],
	# ---- the complete scan answers these: a repartitioned join must not change their answers
	[ 'fall:two_left',  "SELECT o.id, cu.name FROM public.o LEFT JOIN public.cu ON cu.id = o.cust WHERE ({po}){tail2}", 1, 0 ],
	[ 'fall:full',      "SELECT o.id, cu.id FROM public.o FULL JOIN public.cu ON cu.id = o.cust WHERE ({po}) OR ({pc}){tail2}", 1, 0 ],
	[ 'fall:expr',      "SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust + 1 WHERE ({po}) AND ({pc}){tail2}", 1, 0 ],
	[ 'fall:both_move', "SELECT o.id, it.id FROM public.o JOIN public.it ON it.sku = o.amount % 13 WHERE o.id < 60 AND it.id < 60{tail2}", 1, 0 ],
	[ 'rep:window',      "SELECT o.id, rank() OVER (PARTITION BY cu.region ORDER BY o.id) FROM public.o JOIN public.cu ON cu.id = o.cust WHERE o.id < 200 AND ({pc}) ORDER BY 1", 1, 1 ],
);
my @templates = map { { name => $_->[0], sql => $_->[1], ordered => 0, slots => \%SLOTS, rep => $_->[3] } } @shapes;
my $g = $c->generator(templates => \@templates);
my $COUNT = $ENV{KS_COUNT} // 150;

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}
my %rep_of = map { $_->[0] => $_->[3] } @shapes;
my (%n, %repartitioned);
my $i = 0;
for my $q ($g->queries($COUNT))
{
	$i++;
	my $node = $g->next_int(3);
	my $sql = $q->{sql};
	my $text = length($sql) > 100 ? substr($sql, 0, 100) . '...' : $sql;
	$n{ $q->{name} }++;
	my $plan = plan_of($nodes[$node], $sql);
	$repartitioned{ $q->{name} }++ if index($plan, $RJ) >= 0;
	unlike($plan, qr/\Q$RJ\E/, "generated $i ($q->{name}) is not repartitioned: $text") if $q->{name} =~ /^fall:/;
	$c->compare("generated $i ($q->{name}) from node " . ($node + 1) . ": $text", $sql, ordered => $q->{ordered}, nodes => [$node], profiles => 0,
		plans => \@PLANS, normalize => 1);
}
my ($nrep, $nrepartitioned) = (0, 0);
for my $name (sort keys %n)
{
	next unless $rep_of{$name};
	$nrep += $n{$name};
	$nrepartitioned += $repartitioned{$name} // 0;
}
note(sprintf('statements: %d; of the shapes that qualify: %d, repartitioned %d', $COUNT, $nrep, $nrepartitioned));
note(join(', ', map { "$_ " . ($repartitioned{$_} // 0) . "/$n{$_}" } grep { $rep_of{$_} } sort keys %n));
cmp_ok($nrepartitioned, '>=', int(0.9 * $nrep), "at least 90 percent of the statements that qualify were repartitioned ($nrepartitioned of $nrep)");
cmp_ok($nrep, '>', 0, 'the run had statements that qualify');

done_testing();
