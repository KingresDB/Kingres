# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The test of the comparison harness (t/KingresShardCompare.pm, design 16.2).
# Every later test of phase 2b stands on the harness, so the harness has to
# fail when it should: here each helper is run against data that differs from
# the single node's in exactly one way, and the test passes only if the helper
# reports it.  A control (the same call on equal data) shows that the helper
# also passes when it should.
#
# expect_failures() runs a call whose failures are expected: they are printed
# as "not ok ... # TODO expected failure" and do not fail this file, and the
# test that follows checks that the call did report them (how many, and under
# which test name).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('hs');
my @nodes = $c->nodes;

sub ins
{
	my ($table, @rows) = @_;
	return join('', map { "INSERT INTO public.$table VALUES ($_);\n" } @rows);
}

# ---------------------------------------------------------------------------
# Schema and data.  Every table is distributed in the cluster; the tables with
# a difference differ in the single node only.
# ---------------------------------------------------------------------------
$c->on_both(q{
	CREATE TABLE public.t (id int PRIMARY KEY, g int, v text, f float8, n numeric(10,2));
	CREATE TABLE public.t_row1 (id int PRIMARY KEY, v text);
	CREATE TABLE public.t_row2 (id int PRIMARY KEY, v text);
	CREATE TABLE public.t_row3 (id int PRIMARY KEY, v text);
	CREATE TABLE public.t_null (id int PRIMARY KEY, v text);
	CREATE TABLE public.t_null_ok (id int PRIMARY KEY, v text);
	CREATE TABLE public.t_null_txt (id int PRIMARY KEY, v text);
	CREATE TABLE public.t_fl (id int PRIMARY KEY, f float8);
	CREATE TABLE public.t_fl2 (id int PRIMARY KEY, f float8);
});
$c->on_cluster(q{CREATE TABLE public.t_typ (id int PRIMARY KEY, n numeric(10,3))});
$c->on_single(q{CREATE TABLE public.t_typ (id int PRIMARY KEY, n numeric(10,2))});
$c->on_cluster(q{
	SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 6);
	SELECT kingres.create_distributed_table('public.t_row1', 'id', colocate_with => 'public.t');
	SELECT kingres.create_distributed_table('public.t_row2', 'id', colocate_with => 'public.t');
	SELECT kingres.create_distributed_table('public.t_row3', 'id', colocate_with => 'public.t');
	SELECT kingres.create_distributed_table('public.t_null', 'id', colocate_with => 'public.t');
	SELECT kingres.create_distributed_table('public.t_null_ok', 'id', colocate_with => 'public.t');
	SELECT kingres.create_distributed_table('public.t_null_txt', 'id', colocate_with => 'public.t');
	SELECT kingres.create_distributed_table('public.t_fl', 'id', colocate_with => 'public.t');
	SELECT kingres.create_distributed_table('public.t_fl2', 'id', colocate_with => 'public.t');
	SELECT kingres.create_distributed_table('public.t_typ', 'id', colocate_with => 'public.t');
});

# A table that exists on every node and is not distributed, and functions
# that return something else on the single node than on the cluster.
my $local = q{
	CREATE TABLE public.loc (id int, x text);
	CREATE FUNCTION public.flag_val() RETURNS text LANGUAGE sql STABLE
	  AS $$ SELECT coalesce(current_setting('ks.flag', true), 'off') $$;
};
$c->on_every_node($local);
$c->on_single($local);
$c->on_every_node(q{
	CREATE FUNCTION public.ord_key(x int) RETURNS int LANGUAGE sql STABLE AS $$ SELECT -x $$;
	CREATE FUNCTION public.arr() RETURNS text[] LANGUAGE sql STABLE AS $$ SELECT '{c,a,b}'::text[] $$;
	CREATE FUNCTION public.sagg() RETURNS text LANGUAGE sql STABLE AS $$ SELECT 'c, a, b' $$;
});
$c->on_single(q{
	CREATE FUNCTION public.ord_key(x int) RETURNS int LANGUAGE sql STABLE AS $$ SELECT x $$;
	CREATE FUNCTION public.arr() RETURNS text[] LANGUAGE sql STABLE AS $$ SELECT '{a,b,c}'::text[] $$;
	CREATE FUNCTION public.sagg() RETURNS text LANGUAGE sql STABLE AS $$ SELECT 'a, b, c' $$;
});

my @t = map { sprintf("%d, %d, %s, %s, %.2f", $_, $_ % 3, ($_ % 7 == 0 ? 'NULL' : "'v$_'"), $_ * 0.5, $_ * 1.25) } (1 .. 30);
my @rows = map { "$_, 'v$_'" } (1 .. 12);
$c->script_on_both(ins('t', @t) . ins('t_row1', @rows) . ins('t_row2', @rows) . ins('t_row3', @rows)
	. ins('t_null_ok', "1, NULL", "2, ''", "3, 'x'") . ins('t_typ', "1, 1.25", "2, 2.5"));
# The differences.  One row of the single node's differs in each of these.
$c->on_single(q{
	UPDATE public.t_row1 SET v = 'changed' WHERE id = 7;
	DELETE FROM public.t_row2 WHERE id = 9;
	INSERT INTO public.t_row3 VALUES (99, 'extra');
});
$c->on_cluster(ins('t_null', "1, NULL", "2, ''", "3, 'x'") . ins('t_null_txt', "1, NULL", "2, 'x'")
	. ins('t_fl', "1, 0.30000000000000004", "2, 1.5") . ins('t_fl2', "1, 0.3000001", "2, 1.5"));
$c->on_single(ins('t_null', "1, ''", "2, NULL", "3, 'x'") . ins('t_null_txt', "1, 'NULL'", "2, 'x'")
	. ins('t_fl', "1, 0.3", "2, 1.5") . ins('t_fl2', "1, 0.3", "2, 1.5"));

# ---------------------------------------------------------------------------
# compare(): controls, then each kind of difference.
# ---------------------------------------------------------------------------
$c->compare('control: a filter', 'SELECT id, g, v FROM public.t WHERE g = 1');
$c->compare('control: an ordered result', 'SELECT id FROM public.t ORDER BY id', ordered => 1, profiles => 0);
$c->compare('control: NULL and the empty string on both sides', 'SELECT id, v FROM public.t_null_ok', profiles => 0);
$c->compare('control: an error on every node', 'SELECT 1/0', expect_error => qr/division by zero/);

$c->expect_failures('the error is another than expected',
	sub { $c->compare('error', 'SELECT 1/0', expect_error => qr/no such thing/) },
	names => qr/reports the error/, exactly => 3);

$c->expect_failures('one value differs',
	sub { $c->compare('value', 'SELECT id, v FROM public.t_row1', profiles => 0) },
	names => qr/equals the single node's/, exactly => 3);
$c->expect_failures('one row is missing on the single node',
	sub { $c->compare('missing', 'SELECT id, v FROM public.t_row2', profiles => 0) },
	names => qr/equals the single node's/, exactly => 3);
$c->expect_failures('one row is extra on the single node',
	sub { $c->compare('extra', 'SELECT id, v FROM public.t_row3', profiles => 0) },
	names => qr/equals the single node's/, exactly => 3);
$c->expect_failures('one value differs, found under the session profiles too',
	sub { $c->compare('profiles', 'SELECT id, v FROM public.t_row1') },
	names => qr/under session profile \d equals the single node's/, exactly => 15);
like($c->{last_hash_line}, qr/^normalized output md5: cluster node [0-9a-f]{32}, single node [0-9a-f]{32}$/,
	'a difference prints the hash of the normalized outputs');

$c->expect_failures('a NULL against the empty string',
	sub { $c->compare('null', 'SELECT id, v FROM public.t_null', profiles => 0) },
	names => qr/equals the single node's/, exactly => 3);
$c->expect_failures('a NULL against the text NULL',
	sub { $c->compare('null text', 'SELECT id, v FROM public.t_null_txt', profiles => 0) },
	names => qr/equals the single node's/, exactly => 3);

$c->expect_failures('the text form of a type differs',
	sub { $c->compare('type', 'SELECT id, n FROM public.t_typ', profiles => 0) },
	names => qr/equals the single node's/, exactly => 3);

# Order: the same rows in another order are equal unless the order matters.
$c->compare('control: the order does not matter', 'SELECT id FROM public.t ORDER BY public.ord_key(id)', profiles => 0);
$c->expect_failures('the order matters and differs',
	sub { $c->compare('order', 'SELECT id FROM public.t ORDER BY public.ord_key(id)', ordered => 1, profiles => 0) },
	names => qr/equals the single node's/, exactly => 3);

# Only the nodes asked for are compared.
$c->compare('control: a node is left out', 'SELECT id, v FROM public.t_row1 WHERE id <> 7', nodes => [1], profiles => 0);

# ---------------------------------------------------------------------------
# The path assertion: a test cannot pass because nothing was pushed.
# ---------------------------------------------------------------------------
$c->compare('control: the scan is the path', 'SELECT id FROM public.t WHERE g = 1',
	profiles => 0, path => 'Kingres Distributed Scan');
$c->compare('control: two paths', 'SELECT id FROM public.t WHERE g = 1',
	profiles => 0, path => [ 'Kingres Distributed Scan', 'Custom Scan' ]);
$c->expect_failures('the path is not the one asserted',
	sub { $c->compare('path', 'SELECT id FROM public.t WHERE g = 1', profiles => 0, path => 'Kingres Fragment Scan') },
	names => qr/plans the query with Kingres Fragment Scan/, exactly => 3);
$c->expect_failures('a query on a local table has no path',
	sub { $c->compare('local path', 'SELECT id, x FROM public.loc', profiles => 0, path => 'Kingres Distributed Scan') },
	names => qr/plans the query with Kingres Distributed Scan/, exactly => 3);
$c->expect_failures('one of two paths is missing',
	sub { $c->compare('two paths', 'SELECT id FROM public.t WHERE g = 1', profiles => 0,
		path => [ 'Kingres Distributed Scan', 'Kingres Fragment Scan' ]) },
	names => qr/plans the query with Kingres Fragment Scan/, exactly => 3);

# ---------------------------------------------------------------------------
# Plan profiles: every profile equals the single node and the scan.
# ---------------------------------------------------------------------------
$c->compare('control: the three profiles agree', 'SELECT id, g, count(*) OVER () FROM public.t WHERE g < 2',
	profiles => 0, plans => [ 'scan', 'pushed', 'nopartial' ]);
$c->compare('control: a profile with a setting of PostgreSQL', 'SELECT g, count(*) FROM public.t GROUP BY g',
	profiles => 0, plans => [ { name => 'noseq', sql => 'SET enable_seqscan = off' }, 'pushed' ]);
$c->expect_failures('a profile gives another result than the single node and the scan',
	sub { $c->compare('profile', 'SELECT public.flag_val()', profiles => 0,
		plans => [ { name => 'flagged', sql => 'SET ks.flag = on' } ]) },
	names => qr/under the plan profile flagged equals/, exactly => 6);
{
	# The scan profile gives another answer than the defaults: the defaults
	# equal the single node but not the scan.
	local $KingresShardCompare::PLAN_PROFILES{scan} = { settings => [], sql => 'SET ks.flag = on' };
	$c->expect_failures('a profile gives another result than the scan',
		sub { $c->compare('scan profile', 'SELECT public.flag_val()', profiles => 0, plans => ['pushed']) },
		names => qr/under the plan profile pushed equals the scan's/, exactly => 3);
}
{
	my @missing = $c->missing_profile_gucs('scan', 'pushed', 'nopartial');
	my %missing = map { $_ => 1 } @missing;
	for my $guc ('kingres_shard.enable_pushdown', 'kingres_shard.enable_partial_aggregation')
	{
		my $exists = $nodes[0]->safe_psql('postgres', "SELECT count(*) FROM pg_settings WHERE name = '$guc'");
		is($missing{$guc} ? 0 : 1, $exists + 0, "the profile setting $guc is reported missing exactly when it does not exist");
	}
}
eval { $c->compare('unknown profile', 'SELECT 1', profiles => 0, plans => ['no such profile']) };
like($@, qr/unknown plan profile/, 'a profile that does not exist is an error');

# ---------------------------------------------------------------------------
# Normalization.
# ---------------------------------------------------------------------------
is(KingresShardCompare::normalize("b\na\nc", 0), "a\nb\nc", 'normalize sorts the lines of an unordered result');
is(KingresShardCompare::normalize("b\na\nc", 1), "b\na\nc", 'normalize keeps the lines of an ordered result');
is(KingresShardCompare::normalize("1|0.30000000000000004\n2|NaN\n3|-0\n4|1e+308\n5|Infinity\n6|0.123456789012\n7|",
		1, { ncols => 2, floats => [2] }),
	"1|0.3\n2|NaN\n3|-0\n4|1e+308\n5|Infinity\n6|0.123456789\n7|",
	'floats are rounded to 9 digits, and NaN, infinity, a negative zero and a NULL are left alone');
is(KingresShardCompare::normalize('1|{c,"a b",NULL,b}', 1, { ncols => 2, sort_arrays => [2] }),
	'1|{"a b",NULL,b,c}', 'the elements of an array are sorted');
is(KingresShardCompare::normalize('1|c, a, b|x', 1, { ncols => 3, sort_strings => { 2 => ', ' } }),
	'1|a, b, c|x', 'the elements of an aggregated string are sorted');
is(KingresShardCompare::normalize("1|a|b\nonly one line", 1, { ncols => 2, floats => [2] }),
	"1|a|b\nonly one line", 'a line that does not split into the columns is left alone');

$c->expect_failures('floats that differ beyond the rounding are not equal without normalization',
	sub { $c->compare('float', 'SELECT id, f FROM public.t_fl', profiles => 0) },
	names => qr/equals the single node's/, exactly => 3);
$c->compare('control: the same floats, rounded to 9 digits', 'SELECT id, f FROM public.t_fl',
	profiles => 0, normalize => 1);
$c->expect_failures('floats that differ within 9 digits stay different when rounded',
	sub { $c->compare('float2', 'SELECT id, f FROM public.t_fl2', profiles => 0, normalize => 1) },
	names => qr/equals the single node's/, exactly => 3);
$c->expect_failures('arrays in another order are not equal without normalization',
	sub { $c->compare('array', 'SELECT public.arr()', profiles => 0) },
	names => qr/equals the single node's/, exactly => 3);
$c->compare('control: the same arrays, sorted', 'SELECT public.arr()', profiles => 0,
	normalize => { sort_arrays => [1] });
$c->compare('control: the same arrays, sorted, with the rows of a table', 'SELECT id, public.arr() FROM public.t WHERE id < 4',
	profiles => 0, normalize => { sort_arrays => [2] });
$c->expect_failures('arrays with another element stay different when sorted',
	sub { $c->compare('array2', q{SELECT CASE WHEN public.ord_key(1) < 0 THEN '{c,a,x}'::text[] ELSE public.arr() END},
		profiles => 0, normalize => { sort_arrays => [1] }) },
	names => qr/equals the single node's/, exactly => 3);
$c->expect_failures('strings in another order are not equal without normalization',
	sub { $c->compare('string', 'SELECT public.sagg()', profiles => 0) },
	names => qr/equals the single node's/, exactly => 3);
$c->compare('control: the same strings, sorted', 'SELECT public.sagg()', profiles => 0,
	normalize => { sort_strings => { 1 => ', ' } });

# ---------------------------------------------------------------------------
# compare_dml().
# ---------------------------------------------------------------------------
$c->compare_dml('control: the same number of rows', 'UPDATE public.t SET g = g WHERE id < 10');
$c->expect_failures('another number of rows',
	sub { $c->compare_dml('dml', 'UPDATE public.t_row2 SET v = v') },
	names => qr/affected the same number of rows/, exactly => 1);

# ---------------------------------------------------------------------------
# copy_in() and copy_in_both().
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = KingresShardCompare::copy_in($nodes[0], 'public.loc', "1\tx1\n2\t\\N\n3\ta\\tb\n");
	is($rc, 0, 'copy_in loads text rows') or diag($err);
	is($nodes[0]->safe_psql('postgres', q{SELECT count(*) || '|' || count(x) || '|' || max(x) FROM public.loc WHERE id = 3 OR id < 3}),
		'3|2|x1', 'the rows of copy_in are there, with the NULL');
	is($nodes[0]->safe_psql('postgres', q{SELECT x FROM public.loc WHERE id = 3}), "a\tb", 'a tab in a value crosses copy_in');

	($rc, $out, $err) = KingresShardCompare::copy_in($nodes[1], 'public.loc', "id,x\n4,\"q,r\"\n", columns => [ 'id', 'x' ],
		options => 'FORMAT csv, HEADER true');
	is($rc, 0, 'copy_in loads CSV with options and a column list') or diag($err);
	is($nodes[1]->safe_psql('postgres', q{SELECT x FROM public.loc WHERE id = 4}), 'q,r', 'the CSV row is there');

	($rc, $out, $err) = KingresShardCompare::copy_in($nodes[0], 'public.loc', "x\tfoo\n");
	isnt($rc, 0, 'copy_in reports a malformed row with a status');
	like($err, qr/invalid input syntax for type integer/, 'and with the message');

	($rc, $out, $err) = KingresShardCompare::copy_in($nodes[0], 'public.no_such_table', "1\n");
	isnt($rc, 0, 'copy_in into a table that does not exist has a status');
	like($err, qr/relation "public.no_such_table" does not exist/, 'and the message');
}
$c->on_every_node(q{TRUNCATE public.loc});
$c->on_single(q{TRUNCATE public.loc});
{
	my ($cl, $si) = $c->copy_in_both('public.loc', "1\ta\n2\tb\n3\t\\N\n");
	is($cl->[0] . $si->[0], '00', 'copy_in_both loads a plain table on both');
	$c->compare_tables('public.loc', nodes => [0], order_by => 'id', name => 'control: the table after copy_in_both');
	$c->on_single(q{UPDATE public.loc SET x = 'z' WHERE id = 2});
	$c->expect_failures('a table that differs after a write',
		sub { $c->compare_tables('public.loc', nodes => [0], order_by => 'id', name => 'loc') },
		names => qr/holds the same rows as the single node/, exactly => 1);
}

# ---------------------------------------------------------------------------
# compare_tables().
# ---------------------------------------------------------------------------
$c->compare_tables('public.t', name => 'control: the whole table');
$c->compare_tables('public.t', name => 'control: the whole table, ordered', order_by => 'id');
$c->compare_tables('public.t', name => 'control: another query', query => 'SELECT id, g FROM public.t WHERE g = 1');
$c->compare_tables('public.t_null_ok', name => 'control: NULL and the empty string');
$c->expect_failures('one row differs',
	sub { $c->compare_tables('public.t_row1', name => 'row') },
	names => qr/holds the same rows as the single node/, exactly => 3);
$c->expect_failures('a row is missing',
	sub { $c->compare_tables('public.t_row2', name => 'missing') },
	names => qr/holds the same rows as the single node/, exactly => 3);
$c->expect_failures('a row is extra',
	sub { $c->compare_tables('public.t_row3', name => 'extra') },
	names => qr/holds the same rows as the single node/, exactly => 3);
$c->expect_failures('a NULL against the empty string in a table',
	sub { $c->compare_tables('public.t_null', name => 'null') },
	names => qr/holds the same rows as the single node/, exactly => 3);
$c->expect_failures('a NULL against the text NULL in a table',
	sub { $c->compare_tables('public.t_null_txt', name => 'null text') },
	names => qr/holds the same rows as the single node/, exactly => 3);
$c->expect_failures('the text form of a type differs in a table',
	sub { $c->compare_tables('public.t_typ', name => 'type') },
	names => qr/holds the same rows as the single node/, exactly => 3);
$c->compare_tables('public.t', name => 'control: the order does not matter',
	query => 'SELECT id FROM public.t ORDER BY public.ord_key(id)');
$c->expect_failures('the order matters and differs in a table',
	sub { $c->compare_tables('public.t', name => 'order', order_by => 'public.ord_key(id)') },
	names => qr/holds the same rows as the single node/, exactly => 3);
$c->expect_failures('a query that fails on the cluster is reported',
	sub { $c->compare_tables('public.no_such', name => 'no table') },
	names => qr/copies the table out/, min => 4);

# ---------------------------------------------------------------------------
# The generator.
# ---------------------------------------------------------------------------
{
	my $g = KingresShardCompare::Generator->new(seed => 42);
	is(join(' ', map { $g->next_int(1000) } (1 .. 8)), '241 53 501 436 345 410 100 776',
		'the generator gives the same numbers on every machine for a seed');
	$g = KingresShardCompare::Generator->new(seed => 7);
	is(join(' ', map { $g->next_int(6) } (1 .. 10)), '4 4 3 2 3 5 0 2 4 4', 'and for another seed');
	is($c->generator(seed => 5)->seed, 5, 'the seed of a generator is the one given (and it is printed)');
	{
		local $ENV{KS_SEED} = 99;
		is(KingresShardCompare::Generator->new->seed, 99, 'KS_SEED sets the seed');
	}
	like(KingresShardCompare::Generator->new->seed, qr/^\d+$/, 'a generator without a seed makes one');

	my @templates = (
		{ name => 'filter', sql => 'SELECT {cols} FROM public.t WHERE {pred}', ordered => 0,
		  slots => { cols => [ 'id', 'id, g', 'id, v, n' ], pred => [ 'g = {digit}', 'id < {num}', 'v IS NULL', 'n > {num}' ],
					 digit => [ '0', '1', '2' ], num => [ map { $_ } (1 .. 30) ] } },
		{ name => 'agg', sql => 'SELECT g, count(*), sum(n) FROM public.t WHERE {pred} GROUP BY g ORDER BY g', ordered => 1,
		  slots => { pred => [ 'id > {num}', 'g <> {digit}' ], digit => [ '0', '1' ], num => [ '3', '9', '20' ] } },
	);
	my $a = KingresShardCompare::Generator->new(seed => 11, templates => \@templates);
	my $b = KingresShardCompare::Generator->new(seed => 11, templates => \@templates);
	my $d = KingresShardCompare::Generator->new(seed => 12, templates => \@templates);
	my @qa = map { $_->{sql} } $a->queries(50);
	is(join("\n", @qa), join("\n", map { $_->{sql} } $b->queries(50)), 'the same seed gives the same queries');
	isnt(join("\n", @qa), join("\n", map { $_->{sql} } $d->queries(50)), 'another seed gives other queries');
	is(scalar(grep { /\{/ } @qa), 0, 'every slot of a template is filled');
	ok((grep { $_->{ordered} } KingresShardCompare::Generator->new(seed => 3, templates => \@templates)->queries(30)) > 0,
		'a template can say that its result is ordered');
	eval { KingresShardCompare::Generator->new(seed => 1, templates => [ { name => 'bad', sql => 'SELECT {nothing}', slots => {} } ])->query };
	like($@, qr/has no slot nothing/, 'a template that names a slot it lacks is an error');
	eval { KingresShardCompare::Generator->new(seed => 1)->query };
	like($@, qr/no templates/, 'a generator without templates is an error');
	eval { KingresShardCompare::Generator->new(seed => 'abc') };
	like($@, qr/must be a number/, 'a seed that is not a number is an error');

	my $n = $c->compare_generated($c->generator(seed => 21, templates => \@templates), 12, profiles => 0);
	is($n, 12, 'twelve generated queries run through compare and agree');

	my @bad = ({ name => 'differs', sql => 'SELECT id, v FROM public.t_row1 WHERE id <= {n}', slots => { n => [ '7', '8', '9' ] } });
	$c->expect_failures('generated queries that differ are reported',
		sub { $c->compare_generated($c->generator(seed => 22, templates => \@bad), 4, profiles => 0) },
		names => qr/^generated \d \(differs\): .*equals the single node's$/, exactly => 12);
}

# ---------------------------------------------------------------------------
# expect_failures() itself: a call that reports nothing makes its test fail.
# ---------------------------------------------------------------------------
{
	my $tb = Test::More->builder;
	$c->{expecting}++;
	$c->expect_failures('a call that reports nothing',
		sub { $c->compare('nothing', 'SELECT id FROM public.t WHERE id < 3', profiles => 0) });
	$c->{expecting}--;
	is(($tb->details)[-1]{actual_ok}, 0, 'expect_failures fails when the call reported no failure');

	$c->{expecting}++;
	$c->expect_failures('a call that dies', sub { die "boom\n" });
	$c->{expecting}--;
	is(($tb->details)[-1]{actual_ok}, 0, 'expect_failures fails when the call dies');

	$c->{expecting}++;
	$c->expect_failures('a call with the wrong count',
		sub { $c->compare('value', 'SELECT id, v FROM public.t_row1', profiles => 0) }, exactly => 2);
	$c->{expecting}--;
	is(($tb->details)[-1]{actual_ok}, 0, 'expect_failures fails when the call reports another number of failures');

	$c->{expecting}++;
	$c->expect_failures('a call with the wrong name',
		sub { $c->compare('value', 'SELECT id, v FROM public.t_row1', profiles => 0) }, names => qr/no such test/);
	$c->{expecting}--;
	is(($tb->details)[-1]{actual_ok}, 0, 'expect_failures fails when no failure has the name asked for');
}

done_testing();
