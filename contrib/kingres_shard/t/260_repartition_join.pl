# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/260_repartition_join.pl
#
# Phase 12, increment J1 (design/analytics-phase12-queries.md, section 4): a join of distributed tables on a column that is not the
# distribution column.  One table is read by its own scan, its rows are hashed to the group that owns the bucket of the join key in the
# colocation layout of the other table, and every group joins what it received with its own buckets; this node plans what is above the
# join.  The switch is on "force" here, so that the cost does not decide which statements use the path.  Every answer must be the single
# node's, from every node, and equal to the answer of the row-moving scan.
#
#   A. joins that are repartitioned: one routed table, filters on both sides, aggregates above, three tables, a self join, a reference table
#   B. NULL join keys, duplicates, an empty side, a key that matches nothing
#   C. joins that are not repartitioned and fall back to the complete scan: an outer join, two routed tables, a join on an expression
#   D. the switches and the size bound

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('rj', extra => "kingres_shard.enable_repartition_join = force
");
my ($n1, $n2, $n3) = $c->nodes;
my @nodes = ($n1, $n2, $n3);
my $FS = 'Kingres Fragment Scan';
my $RJ = 'Pushed: repartitioned join';

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

table(q{CREATE TABLE public.o (id int PRIMARY KEY, cust int, amount int, note text)},
	q{; SELECT kingres.create_distributed_table('public.o', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.cu (id int PRIMARY KEY, region int, name text)},
	q{; SELECT kingres.create_distributed_table('public.cu', 'id', colocate_with => 'public.o')});
table(q{CREATE TABLE public.it (id int PRIMARY KEY, ord int, sku int)},
	q{; SELECT kingres.create_distributed_table('public.it', 'id', colocate_with => 'public.o')});
table(q{CREATE TABLE public.rg (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.rg')});
$c->on_both(q{INSERT INTO public.rg VALUES (0, 'R0'), (1, 'R1'), (2, 'R2'), (3, 'R3'), (4, 'R4')});
$c->on_both(q{INSERT INTO public.cu SELECT g, g % 5, 'c' || g FROM generate_series(1, 300) g});
# a quarter of the orders have no customer (NULL), some name a customer that does not exist, the rest one of the 300
$c->on_both(q{INSERT INTO public.o SELECT g, CASE WHEN g % 4 = 0 THEN NULL WHEN g % 11 = 0 THEN 1000 + g ELSE 1 + g % 300 END, g % 97, 'n' || (g % 20) FROM generate_series(1, 3000) g});
$c->on_both(q{INSERT INTO public.it SELECT g, 1 + (g * 7) % 3000, g % 13 FROM generate_series(1, 1500) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

my @PLANS = ('pushed', 'scan');

# ---------------------------------------------------------------------------
# A. repartitioned
# ---------------------------------------------------------------------------
my @rep = (
	[ 'orders to customers', q{SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust}, 0 ],
	[ 'a filter on each side', q{SELECT o.id, cu.region FROM public.o JOIN public.cu ON cu.id = o.cust WHERE o.amount > 50 AND cu.region = 3}, 0 ],
	[ 'a count by a column of the moved table', q{SELECT o.note, count(*) FROM public.o JOIN public.cu ON cu.id = o.cust GROUP BY o.note ORDER BY 1}, 1 ],
	[ 'a sum by a column of the other table', q{SELECT cu.region, sum(o.amount), count(*) FROM public.o JOIN public.cu ON cu.id = o.cust GROUP BY cu.region ORDER BY 1}, 1 ],
	[ 'an aggregate without grouping', q{SELECT count(*), sum(o.amount), max(cu.id) FROM public.o JOIN public.cu ON cu.id = o.cust}, 0 ],
	[ 'a sort and a limit', q{SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust ORDER BY o.amount DESC, o.id LIMIT 12}, 1 ],
	[ 'the join condition in WHERE', q{SELECT o.id, cu.name FROM public.o, public.cu WHERE cu.id = o.cust AND o.id < 500}, 0 ],
	[ 'the moved table is the one written second', q{SELECT cu.id, o.id FROM public.cu JOIN public.o ON o.cust = cu.id WHERE cu.id < 40}, 0 ],
	[ 'three tables, one moved', q{SELECT o.id, cu.name, it.sku FROM public.o JOIN public.cu ON cu.id = o.cust JOIN public.it ON it.id = cu.id}, 0 ],
	[ 'a reference table beside', q{SELECT o.id, rg.label FROM public.o JOIN public.cu ON cu.id = o.cust JOIN public.rg ON rg.k = cu.region WHERE o.id < 400}, 0 ],
	[ 'a self join on a non-key', q{SELECT x.id, y.id AS yid FROM public.o x JOIN public.o y ON y.id = x.cust WHERE x.id < 300}, 0 ],
	[ 'a count of distinct values above', q{SELECT count(DISTINCT cu.name) FROM public.o JOIN public.cu ON cu.id = o.cust}, 0 ],
	[ 'a window above', q{SELECT o.id, rank() OVER (PARTITION BY cu.region ORDER BY o.id) FROM public.o JOIN public.cu ON cu.id = o.cust WHERE o.id < 200}, 0 ],
	[ 'the column of the moved table in the select list only', q{SELECT cu.id, o.note FROM public.o JOIN public.cu ON cu.id = o.cust WHERE cu.id < 25}, 0 ],
	[ 'a restriction on the key of the other table', q{SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust WHERE cu.id = 17}, 0 ],
);
for my $q (@rep)
{
	my ($name, $sql, $ordered) = @$q;
	$c->compare("repartition: $name", $sql, ordered => $ordered, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, $RJ ]);
}

# ---------------------------------------------------------------------------
# B. the data
# ---------------------------------------------------------------------------
table(q{CREATE TABLE public.e (id int PRIMARY KEY, cust int)},
	q{; SELECT kingres.create_distributed_table('public.e', 'id', colocate_with => 'public.o')});
table(q{CREATE TABLE public.nn (id int PRIMARY KEY, cust int)},
	q{; SELECT kingres.create_distributed_table('public.nn', 'id', colocate_with => 'public.o')});
$c->on_both(q{INSERT INTO public.nn SELECT g, NULL FROM generate_series(1, 200) g});
$c->on_both(q{INSERT INTO public.e VALUES (1, 5), (2, 5), (3, 5), (4, 6)});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});
# every kind of value travels as text and comes back by its type: quotes, backslashes, commas, braces, a newline, a multi-byte
# character, the empty string, the string NULL, NULL itself, the limits of the numbers, an exact float, a time zone, binary data
table(q{CREATE TABLE public.ty (id int PRIMARY KEY, cust int, t text, n numeric(12,3), f8 float8, f4 float4, d date, tz timestamptz, ts timestamp, b bytea,
		j jsonb, ta text[], ia int[], u uuid, bo boolean, i8 bigint, i2 smallint, iv interval)},
	q{; SELECT kingres.create_distributed_table('public.ty', 'id', colocate_with => 'public.o')});
$c->on_both(q{INSERT INTO public.ty VALUES
	(1, 5, 'plain', 1.500, 0.1, 0.1, '2026-01-31', '2026-01-31 23:59:59.123456+03:30', '2026-01-31 23:59:59.123456', '\xdeadbeef', '{"a": [1, 2, {"b": null}]}', ARRAY['x,y', 'z"q', '{w}', NULL], ARRAY[1, NULL, 3], 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', true, 9223372036854775807, -32768, '1 year 2 mons 3 days 04:05:06.7'),
	(2, 6, E'quote " back \\\\ comma , brace {} nl \n tab \t end', -0.001, 1e308, 1e-30, '0001-01-01', 'infinity', '-infinity', '\x', '[]', ARRAY[]::text[], ARRAY[]::int[], NULL, false, -9223372036854775808, 32767, '-1 days'),
	(3, 7, '', NULL, 'NaN', 'NaN', NULL, NULL, NULL, NULL, 'null', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
	(4, 8, 'NULL', 99999999.999, 3.141592653589793, 3.1415927, '9999-12-31', '2026-06-01 12:00:00-11', '2026-06-01 12:00:00', '\x00ff', '{"k": "\u00e9\u4e2d"}', ARRAY['', 'NULL', E'\u00e9\u4e2d'], ARRAY[-1, 0, 1], 'ffffffff-ffff-ffff-ffff-ffffffffffff', true, 0, 0, '0'),
	(5, 9, E'\u00e9\u4e2d\U0001F600', 0, -0.0, -0.0, '2000-02-29', '2000-02-29 00:00:00+00', '2000-02-29 00:00:00', '\x5c', '"s"', ARRAY['a'], ARRAY[7], '00000000-0000-0000-0000-000000000000', false, 1, 1, '1 mon')});
$c->on_both(q{INSERT INTO public.cu SELECT g, g % 5, 'c' || g FROM generate_series(301, 310) g});
# keys of three kinds of the distribution: a bigint column to an int key, and a text key
table(q{CREATE TABLE public.ob (id int PRIMARY KEY, cust bigint)},
	q{; SELECT kingres.create_distributed_table('public.ob', 'id', colocate_with => 'public.o')});
$c->on_both(q{INSERT INTO public.ob SELECT g, g % 400 FROM generate_series(1, 600) g});
table(q{CREATE TABLE public.tk (code text PRIMARY KEY, v int)},
	q{; SELECT kingres.create_distributed_table('public.tk', 'code', bucket_count => 10)});
table(q{CREATE TABLE public.tl (id int PRIMARY KEY, code text, w int)},
	q{; SELECT kingres.create_distributed_table('public.tl', 'id', colocate_with => 'public.o')});
$c->on_both(q{INSERT INTO public.tk SELECT 'k' || g, g FROM generate_series(1, 120) g});
$c->on_both(q{INSERT INTO public.tk VALUES (E'00e94e2d', 1), ('a,b', 2), ('NULL', 3), ('', 4)});
$c->on_both(q{INSERT INTO public.tl SELECT g, CASE WHEN g % 9 = 0 THEN NULL WHEN g % 7 = 0 THEN E'00e94e2d' WHEN g % 11 = 0 THEN 'a,b' WHEN g % 13 = 0 THEN '' ELSE 'k' || (g % 150) END, g FROM generate_series(1, 400) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});
my @typed = (
	[ 'every kind of value', q{SELECT ty.*, cu.name FROM public.ty JOIN public.cu ON cu.id = ty.cust + 295} ],
	[ 'a bigint to an int key', q{SELECT ob.id, cu.name FROM public.ob JOIN public.cu ON cu.id = ob.cust} ],
	[ 'a text key', q{SELECT tl.id, tk.v FROM public.tl JOIN public.tk ON tk.code = tl.code} ],
);
for my $q (@typed)
{
	my ($name, $sql) = @$q;
	$c->compare("repartition typed: $name", $sql, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1);
}
$c->compare('repartition typed: every kind of value, repartitioned', q{SELECT ty.*, cu.name FROM public.ty JOIN public.cu ON cu.id = ty.cust}, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, $RJ ]);
$c->compare('repartition typed: a bigint to an int key, repartitioned', q{SELECT ob.id, cu.name FROM public.ob JOIN public.cu ON cu.id = ob.cust}, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, $RJ ]);
$c->compare('repartition typed: a text key, repartitioned', q{SELECT tl.id, tk.v FROM public.tl JOIN public.tk ON tk.code = tl.code}, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, $RJ ]);
# the table that is moved can be in another colocation group: its rows go by the layout of the group of the table that stays
table(q{CREATE TABLE public.far (id int PRIMARY KEY, w int)},
	q{; SELECT kingres.create_distributed_table('public.far', 'id', bucket_count => 7)});
$c->on_both(q{INSERT INTO public.far SELECT g, g % 9 FROM generate_series(1, 250) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});
for my $sql (q{SELECT far.id, far.w, o.id FROM public.far JOIN public.o ON o.cust = far.id WHERE o.id < 900},
			 q{SELECT o.id, far.w FROM public.o JOIN public.far ON far.id = o.cust})
{
	$c->compare('repartition: the layout of another colocation group', $sql, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, $RJ ]);
}
$c->compare('repartition: two colocation groups that stay are refused', q{SELECT o.id, far.w, cu.name FROM public.o JOIN public.far ON far.id = o.id JOIN public.cu ON cu.id = o.cust}, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1);

# a type whose text depends on the settings of the session (money) is not moved: the complete scan answers
table(q{CREATE TABLE public.mn (id int PRIMARY KEY, cust int, m money)},
	q{; SELECT kingres.create_distributed_table('public.mn', 'id', colocate_with => 'public.o')});
$c->on_both(q{INSERT INTO public.mn SELECT g, g % 50, g * 1.25 FROM generate_series(1, 100) g});
{
	my $sql = q{SELECT mn.id, mn.m, cu.name FROM public.mn JOIN public.cu ON cu.id = mn.cust};
	$c->compare('repartition typed: a money column falls back', $sql, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1);
	unlike(plan_of($n1, $sql), qr/\Q$RJ\E/, '... and no repartitioned join is planned');
}

my @data = (
	[ 'every key of the moved side is NULL', q{SELECT nn.id, cu.name FROM public.nn JOIN public.cu ON cu.id = nn.cust} ],
	[ 'an empty moved side', q{SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust WHERE o.amount > 1000} ],
	[ 'several rows with one key, and a key with none', q{SELECT e.id, cu.name FROM public.e JOIN public.cu ON cu.id = e.cust} ],
	[ 'keys that match nothing', q{SELECT count(*) FROM public.o JOIN public.cu ON cu.id = o.cust WHERE o.cust > 1000} ],
	[ 'the other table is empty', q{SELECT o.id FROM public.o JOIN public.cu ON cu.id = o.cust WHERE cu.id < 0} ],
);
for my $q (@data)
{
	my ($name, $sql) = @$q;
	$c->compare("repartition data: $name", $sql, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1);
}

# ---------------------------------------------------------------------------
# C. fall back to the complete scan
# ---------------------------------------------------------------------------
my @not = (
	[ 'a left join with the moved table preserved', q{SELECT o.id, cu.name FROM public.o LEFT JOIN public.cu ON cu.id = o.cust WHERE o.id < 200} ],
	[ 'a left join with the other table preserved', q{SELECT cu.id, o.id FROM public.cu LEFT JOIN public.o ON o.cust = cu.id WHERE cu.id < 60} ],
	[ 'a full join', q{SELECT o.id, cu.id FROM public.o FULL JOIN public.cu ON cu.id = o.cust WHERE o.id < 100 OR cu.id < 100} ],
	[ 'a join on an expression', q{SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust + 1 WHERE o.id < 300} ],
	[ 'a join on a column of both that is not equal', q{SELECT o.id, cu.id FROM public.o JOIN public.cu ON cu.region < o.amount % 5 WHERE o.id < 20 AND cu.id < 20} ],
	[ 'two tables that would both have to move (one is for J2)', q{SELECT o.id, cu.id, e.id FROM public.o JOIN public.cu ON cu.id = o.cust JOIN public.e ON e.cust = cu.id WHERE o.id < 200} ],
	[ 'two tables that both join by other columns', q{SELECT o.id, it.id FROM public.o JOIN public.it ON it.sku = o.amount % 13 WHERE o.id < 50 AND it.id < 50} ],
);
for my $q (@not)
{
	my ($name, $sql) = @$q;
	$c->compare("fallback: $name", $sql, ordered => 0, plans => \@PLANS, profiles => 0, normalize => 1);
	unlike(plan_of($n1, $sql), qr/\Q$RJ\E/, "fallback: $name: no repartitioned join");
}

# ---------------------------------------------------------------------------
# D. the switches and the bound
# ---------------------------------------------------------------------------
{
	my $sql = q{SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust};
	like(plan_of($n1, $sql), qr/\Q$RJ\E/, 'force: the join is repartitioned');
	unlike(plan_of($n1, $sql, 'SET kingres_shard.enable_repartition_join = off; '), qr/\Q$RJ\E/, 'off: it is not');
	unlike(plan_of($n1, $sql, 'SET kingres_shard.enable_pushdown = off; '), qr/\Q$RJ\E/, 'enable_pushdown = off: it is not');

	# about 1.6 MB of rows to move against a bound of 1 MB
	table(q{CREATE TABLE public.bg (id int PRIMARY KEY, cust int, pad text)},
		q{; SELECT kingres.create_distributed_table('public.bg', 'id', colocate_with => 'public.o')});
	$c->on_both(q{INSERT INTO public.bg SELECT g, 1 + g % 300, repeat('x', 400) FROM generate_series(1, 4000) g});
	$c->on_every_node(q{ANALYZE});
	$c->on_single(q{ANALYZE});
	my $big = q{SELECT bg.id, bg.pad, cu.name FROM public.bg JOIN public.cu ON cu.id = bg.cust};
	my ($rc, $out, $err) = run_sql($n1, 'SET kingres_shard.repartition_max_bytes = 1024; ' . $big);
	isnt($rc, 0, 'the moved rows above repartition_max_bytes: an error, never a wrong answer');
	like($err, qr/repartition_max_bytes/, '... which names the setting');
	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_repartition_join = on; SET kingres_shard.repartition_max_bytes = 1024; $big");
	is($rc, 0, 'by cost, a moved side that is estimated above the bound is not chosen: the complete scan answers') or diag($err);
	my ($rs0, $os0) = run_sql($c->single, $big);
	is(join("\n", sort split /\n/, $out), join("\n", sort split /\n/, $os0), '... with the single node\'s answer');
	($rc, $out, $err) = run_sql($n1, $big);
	is($rc, 0, 'with the default bound the same statement is repartitioned') or diag($err);
	is(join("\n", sort split /\n/, $out), join("\n", sort split /\n/, $os0), '... with the single node\'s answer');

	($rc, $out, $err) = run_sql($n1, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) $sql");
	is($rc, 0, 'EXPLAIN ANALYZE runs') or diag($err);

	my $G = 'SET plan_cache_mode = force_generic_plan; ';
	my $pre = q{PREPARE rp(int, int) AS SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust WHERE o.amount > $1 AND cu.region = $2; };
	my ($rs, $os) = run_sql($c->single, $pre . 'EXECUTE rp(50, 3)');
	for my $nd (@nodes)
	{
		for my $mode ('', $G)
		{
			my ($rc2, $out2, $err2) = run_sql($nd, $mode . $pre . 'EXECUTE rp(50, 3)');
			is($rc2, 0, 'a prepared repartitioned join runs on ' . $nd->name . ($mode ? ' (generic plan)' : '')) or diag($err2);
			is(join("\n", sort split /\n/, $out2), join("\n", sort split /\n/, $os), '... and its answer is the single node\'s');
		}
	}
}

done_testing();
