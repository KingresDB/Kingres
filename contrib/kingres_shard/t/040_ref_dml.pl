# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/040_ref_dml.pl
#
# Review findings of the 0.2 fix round about statements that are written to
# every node:
#  - S3-08: a string default of a date, time or object-name column is
#    recognized by what it can mean, not by its text, so no spelling of 'now'
#    gives the nodes different values;
#  - S3-10: INSERT ... SELECT and a multi-row INSERT into a table with column
#    defaults (the deparsed statement named the defaulted columns without
#    giving them a value);
#  - S3-14: functions whose result depends on the C library of a node are not
#    shipped (a reference-table write with them is refused, a restriction with
#    them is evaluated by the node that reads);
#  - S3-18: a write through a view of a reference table is refused with the real
#    reason (it used to claim the view was not a reference table).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my ($n1, $n2, $n3) = make_cluster('rd', 3);
my @nodes = ($n1, $n2, $n3);

sub everywhere
{
	my ($sql) = @_;
	$_->safe_psql('postgres', $sql) for @nodes;
}

sub table_text
{
	my ($node, $table) = @_;
	return $node->safe_psql('postgres',
		"SELECT coalesce(string_agg(t::text, ',' ORDER BY t::text), '') FROM $table t");
}

sub same_everywhere
{
	my ($label, @tables) = @_;
	for my $t (@tables)
	{
		my $want = table_text($n1, $t);
		is(table_text($_, $t), $want, "$label: $t is the same on " . $_->name)
		  for ($n2, $n3);
	}
}

sub refused
{
	my ($label, $node, $sql, $re) = @_;
	my ($rc, $out, $err) = run_sql($node, $sql);
	isnt($rc, 0, "$label: refused");
	like($err, $re, "$label: the message says why");
	return $err;
}

everywhere(q{CREATE DOMAIN public.tsdom AS timestamptz});
$n1->safe_psql('postgres', q{
	CREATE TABLE public.r4 (id int PRIMARY KEY, v int, d int DEFAULT 7);
	CREATE TABLE public.r5 (id int PRIMARY KEY, v int);
	CREATE TABLE public.rg (id int PRIMARY KEY, a int, g int GENERATED ALWAYS AS (a * 2) STORED);
	CREATE TABLE public.rid (id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v int);
	CREATE TABLE public.rf (id int PRIMARY KEY, f float8, n numeric);
	CREATE TABLE public.rt (id int PRIMARY KEY, v int);
	CREATE TABLE public.dt (id int PRIMARY KEY, v int, d int DEFAULT 9);
	INSERT INTO public.r5 SELECT g, g * 10 FROM generate_series(1, 5) g;
	INSERT INTO public.rf SELECT g, g / 4.0, g::numeric FROM generate_series(1, 4) g;
	INSERT INTO public.rt SELECT g, g FROM generate_series(1, 3) g;
	SELECT kingres.create_reference_table('public.r4');
	SELECT kingres.create_reference_table('public.r5');
	SELECT kingres.create_reference_table('public.rg');
	SELECT kingres.create_reference_table('public.rid');
	SELECT kingres.create_reference_table('public.rf');
	SELECT kingres.create_reference_table('public.rt');
	SELECT kingres.create_distributed_table('public.dt', 'id');
});

# ---------------------------------------------------------------------------
# S3-10: defaults in INSERT ... SELECT and multi-row INSERT
# ---------------------------------------------------------------------------
my ($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.r4 (id, v) SELECT id + 10, v FROM public.r5});
is($rc, 0, 'S3-10: INSERT ... SELECT into a reference table with a defaulted column') or diag $err;
is($n1->safe_psql('postgres', q{SELECT count(*) || '/' || sum(d) FROM public.r4}), '5/35',
	'S3-10: the default was applied to every row');
same_everywhere('S3-10 INSERT ... SELECT', 'public.r4');

($rc, $out, $err) = run_sql($n2, q{INSERT INTO public.r4 (id, v) VALUES (21, 1), (22, 2), (23, 3)});
is($rc, 0, 'S3-10: multi-row INSERT with a defaulted column, from another node') or diag $err;
same_everywhere('S3-10 multi-row INSERT', 'public.r4');

($rc, $out, $err) = run_sql($n3, q{INSERT INTO public.r4 (id, v) SELECT id + 10, v + 1 FROM public.r5
	ON CONFLICT (id) DO UPDATE SET v = excluded.v, d = r4.d + 1 RETURNING id, d});
is($rc, 0, 'S3-10: INSERT ... SELECT ... ON CONFLICT DO UPDATE ... RETURNING') or diag $err;
is($out, join("\n", '11|8', '12|8', '13|8', '14|8', '15|8'),
	'S3-10: ON CONFLICT used the default and the existing row');
same_everywhere('S3-10 ON CONFLICT', 'public.r4');

($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.rg (id, a) SELECT id, v FROM public.r5});
is($rc, 0, 'S3-10: INSERT ... SELECT into a table with a stored generated column') or diag $err;
same_everywhere('S3-10 generated column', 'public.rg');
is($n2->safe_psql('postgres', q{SELECT sum(g) FROM public.rg}), '300', 'S3-10: the generated column is computed on every node');

($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.rid (v) SELECT v FROM public.r5});
isnt($rc, 0, 'S3-10: an identity column is refused (each node would draw its own number)');
like($err, qr/cannot be run on every node/, 'S3-10: the message says why');

# Distributed table: a routed single-row INSERT keeps its default; a
# multi-row INSERT ... SELECT is either run or refused cleanly.
($rc, $out, $err) = run_sql($n2, q{INSERT INTO public.dt (id, v) VALUES (1, 1), (2, 2)});
unlike($err, qr/more target columns/, 'S3-10: multi-row INSERT with a default into a distributed table: no deparse error');
($rc, $out, $err) = run_sql($n2, q{INSERT INTO public.dt (id, v) SELECT id, v FROM public.r5});
unlike($err, qr/more target columns/, 'S3-10: INSERT ... SELECT with a default into a distributed table: no deparse error');
($rc, $out, $err) = run_sql($n2, q{INSERT INTO public.dt (id, v) VALUES (3, 3)});
is($rc, 0, 'S3-10: single-row INSERT into a distributed table') or diag $err;
is($n1->safe_psql('postgres', q{SELECT d FROM public.dt WHERE id = 3}), '9', 'S3-10: its default was applied');

# ---------------------------------------------------------------------------
# S3-14: functions that depend on the C library of a node
# ---------------------------------------------------------------------------
refused('S3-14 ln in a reference write', $n1, q{UPDATE public.rf SET f = ln(f + 1)},
	qr/cannot be run on every node/);
refused('S3-14 exp in a reference write', $n1, q{UPDATE public.rf SET f = exp(f)}, qr/cannot be run on every node/);
refused('S3-14 operator ^ on float8', $n1, q{UPDATE public.rf SET f = f ^ 2.5::float8},
	qr/cannot be run on every node/);
refused('S3-14 a trigonometric function in the WHERE clause', $n1,
	q{DELETE FROM public.rf WHERE sin(f) > 0.5}, qr/cannot be run on every node/);
($rc, $out, $err) = run_sql($n1, q{UPDATE public.rf SET f = f * 2, n = sqrt(n) WHERE id <= 2});
is($rc, 0, 'S3-14: float arithmetic and numeric sqrt (exact on every node) are shipped') or diag $err;
same_everywhere('S3-14 numeric and plain float', 'public.rf');

for my $k (10 .. 40)
{
	$n1->safe_psql('postgres', "INSERT INTO public.dt (id, v) VALUES ($k, $k * $k)");
}
is($n3->safe_psql('postgres', q{SELECT count(*) FROM public.dt WHERE ln(v::float8) > 6}), '20',
	'S3-14: a distributed scan with ln in the restriction is evaluated where it is read, correctly');
unlike($n3->safe_psql('postgres', q{EXPLAIN (VERBOSE, COSTS OFF) SELECT * FROM public.dt WHERE ln(v::float8) > 6}),
	qr/Remote SQL[^\n]*\bln\(/i, 'S3-14: the restriction with ln is not sent to the other nodes');
refused('S3-14 ln in a fanned-out UPDATE of a distributed table', $n3,
	q{UPDATE public.dt SET v = v + 1 WHERE ln(v::float8) > 6}, qr/cannot be shipped/);

# ---------------------------------------------------------------------------
# S3-08: string defaults of columns whose text means a time or an object
# ---------------------------------------------------------------------------
my @bad = (
	['a plain now',           q{ts timestamptz DEFAULT 'now'}],
	['a tab before now',      q{ts timestamptz DEFAULT E'\tnow'}],
	['a newline after now',   q{ts timestamptz DEFAULT E'now\n'}],
	['a form feed',           q{ts timestamptz DEFAULT E'\fnow'}],
	['today with a time',     q{ts timestamp DEFAULT 'tomorrow 03:00'}],
	['a typed literal',       q{ts timestamptz DEFAULT 'now'::timestamptz}],
	['now cast to a date',    q{dd date DEFAULT 'yesterday'}],
	['allballs',              q{tm time DEFAULT 'allballs'}],
	['timetz',                q{tz timetz DEFAULT 'now'}],
	['a domain over a time',  q{ts public.tsdom DEFAULT 'now'}],
	['an array of times',     q{ts timestamptz[] DEFAULT '{now}'}],
	['a range of times',      q{ts tstzrange DEFAULT '[now,)'}],
	['a name of a relation',  q{c regclass DEFAULT 'pg_class'}],
	['a name of a type',      q{c regtype DEFAULT 'int4'}],
	['text cast to a time',   q{ts timestamptz DEFAULT 'now'::text}],
);
for my $b (@bad)
{
	my ($what, $def) = @$b;
	for my $tab ('public.rt', 'public.dt')
	{
		my ($rc2, $o2, $e2) = run_sql($n1, "ALTER TABLE $tab ADD COLUMN $def");
		isnt($rc2, 0, "S3-08: $what on $tab is refused");
		like($e2, qr/ADD COLUMN with a default/, "S3-08: $what on $tab: the message names the default");
	}
}
for my $n (@nodes)
{
	is($n->safe_psql('postgres', q{SELECT count(*) FROM pg_attribute WHERE attrelid = 'public.rt'::regclass AND attnum > 0 AND NOT attisdropped}),
		'2', 'S3-08: no refused column was added on ' . $n->name);
}

my @good = (
	['an ISO timestamptz', q{ts1 timestamptz DEFAULT '2024-01-02 03:04:05+00'}],
	['a T-separated one',  q{ts2 timestamp DEFAULT '2024-01-02T03:04:05.25'}],
	['a date',             q{dd date DEFAULT '2024-02-03'}],
	['a time',             q{tm time DEFAULT '04:05:06'}],
	['text that says now', q{note text DEFAULT 'now'}],
	['epoch with white space', q{e1 timestamptz DEFAULT ' EPOCH '}],
	['minus infinity',      q{e2 timestamptz DEFAULT E'-infinity\n'}],
	['an integer',         q{i int DEFAULT 5}],
	['a typed integer',    q{j bigint DEFAULT '7'::bigint}],
);
for my $g (@good)
{
	my ($what, $def) = @$g;
	my ($rc2, $o2, $e2) = run_sql($n1, "ALTER TABLE public.rt ADD COLUMN $def");
	is($rc2, 0, "S3-08: $what is accepted") or diag $e2;
}
same_everywhere('S3-08 accepted defaults', 'public.rt');
($rc, $out, $err) = run_sql($n2, q{SELECT count(DISTINCT (ts1, ts2, dd, tm, note, i, j)) FROM public.rt});
is($out, '1', 'S3-08: every row has the same values');

# ---------------------------------------------------------------------------
# S3-18: a write through an updatable view of a reference table is refused
# with the real reason; reads through a view and writes to the table work
# ---------------------------------------------------------------------------
everywhere(q{
	CREATE VIEW public.rv AS SELECT id, v FROM public.r5 WHERE id > 1;
	CREATE VIEW public.rvc AS SELECT id, v FROM public.r5 WHERE id > 1 WITH CHECK OPTION;
	CREATE VIEW public.rvj AS SELECT a.id, a.v FROM public.r5 a JOIN public.r4 b ON b.id = a.id;
});
my $before = table_text($n1, 'public.r5');
refused('S3-18 INSERT through a view', $n2, q{INSERT INTO public.rv VALUES (100, 1000)}, qr/writes through a view/);
refused('S3-18 UPDATE through a view', $n3, q{UPDATE public.rv SET v = v + 1 WHERE id = 2}, qr/writes through a view/);
refused('S3-18 DELETE through a view', $n1, q{DELETE FROM public.rv WHERE id = 2}, qr/writes through a view/);
refused('S3-18 WITH CHECK OPTION', $n1, q{INSERT INTO public.rvc VALUES (101, 1)}, qr/writes through a view/);
refused('S3-18 a view that PostgreSQL cannot update', $n1, q{INSERT INTO public.rvj VALUES (1, 1)},
	qr/cannot insert into view/);
is(table_text($n1, 'public.r5'), $before, 'S3-18: a refused write changed nothing');
same_everywhere('S3-18', 'public.r5');
is($n2->safe_psql('postgres', q{SELECT count(*) FROM public.rv}), '4', 'S3-18: a view of a reference table can be read');
($rc, $out, $err) = run_sql($n2, q{UPDATE public.r5 SET v = v + 1 WHERE id > 1});
is($rc, 0, 'S3-18: a write to the table itself works') or diag $err;
same_everywhere('S3-18 after the write', 'public.r5');

done_testing();
