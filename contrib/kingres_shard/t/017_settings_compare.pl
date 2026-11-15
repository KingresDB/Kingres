# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/017_settings_compare.pl
#
# Review finding S2-03: a restriction that is shipped to the other shard groups
# must mean what it means in the user's session.  Tables with date, float,
# interval, bytea and timestamptz columns; queries whose clauses convert these
# types through their text form; every query is compared with a single node
# under the default session settings and under the profiles of
# KingresShardCompare (DateStyle, IntervalStyle, extra_float_digits, TimeZone,
# bytea_output, lc_*).  Statements shipped whole (a cross-group UPDATE or
# DELETE, a reference-table write) that depend on such a conversion are refused.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('st');
my @nodes = $c->nodes;

$c->on_both(q{
	CREATE TABLE public.t1 (id int PRIMARY KEY, d date, z float8, iv interval, b bytea, ts timestamptz, tx text);
	CREATE TABLE public.refz (id int PRIMARY KEY, d date, z float8, tx text);
});
$c->on_cluster(q{
	SELECT kingres.create_distributed_table('public.t1', 'id', bucket_count => 12);
	SELECT kingres.create_reference_table('public.refz');
});

my $rows = join('', map {
		"INSERT INTO public.t1 VALUES ($_, date '2024-01-01' + $_ - 1, ${_}::float8 / 3, "
		. "($_ || ' days 3 hours 4 minutes')::interval, decode(lpad(to_hex($_ * 257), 4, '0') || '5c', 'hex'), "
		. "timestamptz '2024-03-01 10:00:00+00' + $_ * interval '1 hour', 'r$_');\n"
		. "INSERT INTO public.refz VALUES ($_, date '2024-01-01' + $_ - 1, ${_}::float8 / 3, 'r$_');\n"
	} (1 .. 40));
$c->script_on_both($rows);

my @queries = (
	[ 'date built from text',   q{SELECT id FROM public.t1 WHERE id < 13 AND (id::text || '/01/2024')::date = '2024-01-05'}, 0 ],
	[ 'float to text',          q{SELECT id FROM public.t1 WHERE z::text = '0.333333333333333'}, 0 ],
	[ 'float to text, long',    q{SELECT id FROM public.t1 WHERE z::text = '0.3333333333333333'}, 0 ],
	[ 'date to text and to_char', q{SELECT id, to_char(d, 'FMDay') FROM public.t1 WHERE d::text LIKE '%01/01/2024%' AND id < 30}, 0 ],
	[ 'date to text, ISO',      q{SELECT id FROM public.t1 WHERE d::text LIKE '2024-01-0%'}, 0 ],
	[ 'interval to text',       q{SELECT id FROM public.t1 WHERE iv::text LIKE '%days%'}, 0 ],
	[ 'interval to text, sql',  q{SELECT id FROM public.t1 WHERE iv::text LIKE '+%'}, 0 ],
	[ 'bytea to text, hex',     q{SELECT id FROM public.t1 WHERE b::text LIKE '\\x%'}, 0 ],
	[ 'bytea to text, escape',  q{SELECT id FROM public.t1 WHERE b::text LIKE '%\\\\%'}, 0 ],
	[ 'timestamptz to text',    q{SELECT id FROM public.t1 WHERE ts::text LIKE '%2024-03-02%'}, 0 ],
	[ 'timestamptz to text, short', q{SELECT id FROM public.t1 WHERE ts::text LIKE '%03/02/2024%'}, 0 ],
	[ 'text round trip',        q{SELECT id FROM public.t1 WHERE (z::text)::float8 > 10}, 0 ],
	[ 'a plain date range',     q{SELECT id FROM public.t1 WHERE d >= date '2024-01-10' AND d < date '2024-01-20'}, 0 ],
	[ 'a timestamptz range',    q{SELECT id FROM public.t1 WHERE ts >= timestamptz '2024-03-02 00:00:00+00' AND ts < timestamptz '2024-03-02 12:00:00+00'}, 0 ],
	[ 'all columns',            q{SELECT * FROM public.t1 WHERE id <= 12}, 0 ],
	[ 'reference table, float to text', q{SELECT id FROM public.refz WHERE z::text = '0.333333333333333'}, 0 ],
	[ 'join with the reference table', q{SELECT t.id FROM public.t1 t JOIN public.refz r ON r.id = t.id WHERE r.d::text LIKE '%01/01/2024%'}, 0 ],
);
$c->compare($_->[0], $_->[1], ordered => $_->[2]) for @queries;

# A statement shipped whole that depends on a text conversion is refused (the
# same rule as for a restriction), and so is the same statement on a reference table.
$c->compare_dml('cross-group UPDATE with a float to text restriction',
	q{UPDATE public.t1 SET tx = 'hit' WHERE z::text = '0.333333333333333'},
	settings => 'SET extra_float_digits = 0;',
	expect_error => qr/cannot be shipped/);
$c->compare_dml('cross-group DELETE with a date to text restriction',
	q{DELETE FROM public.t1 WHERE d::text LIKE '%01/01/2024%'},
	settings => "SET DateStyle = 'SQL, DMY';",
	expect_error => qr/cannot be shipped/);
$c->compare_dml('reference UPDATE with a float to text restriction',
	q{UPDATE public.refz SET tx = 'hit' WHERE z::text = '0.333333333333333'},
	settings => 'SET extra_float_digits = 0;',
	expect_error => qr/cannot be run on every node/);
$c->compare('nothing was changed', q{SELECT count(*) FROM public.t1 WHERE tx = 'hit' UNION ALL SELECT count(*) FROM public.refz WHERE tx = 'hit'});

# A shippable statement still is, under any session settings.
$c->compare_dml('cross-group UPDATE with a plain restriction',
	q{UPDATE public.t1 SET tx = 'plain' WHERE d >= date '2024-01-10' AND z < 5},
	settings => "SET DateStyle = 'German, DMY'; SET extra_float_digits = 0;");
$c->compare('the rows after the UPDATE', q{SELECT id, tx FROM public.t1 WHERE tx = 'plain'});

done_testing();
