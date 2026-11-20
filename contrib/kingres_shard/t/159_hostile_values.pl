# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/159_hostile_values.pl
#
# The design's 066_hostile_values (design/shard-phase2b.md, 16.4): identifiers and values cross every deparser intact, for every
# statement shape of phase 2b, and the answer is the single node's.  t/114, t/126, t/130 and t/156 (section F) cover single
# shapes with a few names; this test is the matrix of the design.
#
#   A. names: schemas, tables, columns, types, sequences and constraints with hostile names (quotes, blanks, a reserved word,
#      a newline, a comment and a statement inside a name, a name of 63 bytes, a 4-byte UTF-8 character, "$user", "public",
#      mixed case); every statement shape runs over every name: a scan, a join, a partial aggregate, a closed aggregate, an
#      ordered merge with a limit, a router INSERT with RETURNING and ON CONFLICT DO NOTHING, and COPY with a column list;
#   B. values: strings with quotes, backslashes, control characters, braces, commas, the texts NULL and \N, the empty string,
#      2-byte and 4-byte characters and a string of 1 MB; bytea with the 256 byte values; jsonb with nested quotes; arrays with
#      NULL and quoted elements; composite, range and enum values with quotes; timestamptz before the year 1 and infinity;
#      float8 -0, NaN and 1e308; numeric of 1000 digits; money.  Each goes through a scan, a join, a partial aggregate, an
#      ordered merge, the router and COPY, and is read back by md5 of its text form;
#   C. objects in public that shadow built-in names (a function sum, an operator = on text, a domain text), a SET search_path and
#      SET standard_conforming_strings = off in the session: the result is the single node's.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('hv', extra => "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_join = on\n");
my ($n1, $n2, $n3) = $c->nodes;
my $single = $c->single;
my $utf8 = $n1->safe_psql('postgres', 'SHOW server_encoding') eq 'UTF8' && $single->safe_psql('postgres', 'SHOW server_encoding') eq 'UTF8';

sub qi { my ($n) = @_; $n =~ s/"/""/g; return '"' . $n . '"'; }

# ---------------------------------------------------------------------------
# A. names
# ---------------------------------------------------------------------------
my @names = (
	'a"b', 'a b', 'select', "t\nnl", 'x*/ ; DROP TABLE y; /*',
	'n' . ('0123456789' x 5) . '012345',	# 57 bytes: the longest table name (6 bytes are added for the bucket partitions)
	'm' . ('0123456789' x 6) . '01',	# 63 bytes: the longest name of anything else (not a table here)
	'$user', 'public', 'MixedCase', "it's",
);
push @names, 'e' . "\xF0\x9F\x98\x80" if $utf8;
is(length($names[5]), 57, 'one name has 57 bytes');
is(length($names[6]), 63, 'one name has 63 bytes');

my $ROWS = 300;
for my $i (0 .. $#names)
{
	my $N = $names[$i];
	next if length($N) > 57;	# a table name that bucket partition names cannot be made from
	my $Q = qi($N);
	my $COL = qi($names[ ($i + 4) % @names ]);
	my $TY = qi($names[ ($i + 1) % @names ]);
	my $SQ = qi($names[ ($i + 2) % @names ]);
	my $CK = qi($names[ ($i + 3) % @names ]);
	my $T = "$Q.$Q";
	my $TL = $T =~ s/'/''/gr;
	my $ddl = "CREATE SCHEMA IF NOT EXISTS $Q; CREATE TYPE $Q.$TY AS (a int, b text); CREATE SEQUENCE $Q.$SQ;";
	$c->on_every_node($ddl);
	$c->on_single($ddl);
	my $tbl = "CREATE TABLE $T (id int, $COL text, v int, ty $Q.$TY, CONSTRAINT $CK CHECK (v >= 0), PRIMARY KEY (id, $COL))";
	$c->on_cluster($tbl . "; SELECT kingres.create_distributed_table('$TL', 'id', bucket_count => 12)");
	$c->on_single($tbl);
	my $ins = "INSERT INTO $T SELECT g, E'it''s \\\\ back\\\\slash \"q\" ' || (g % 17), g % 5, ROW(g, 'x')::$Q.$TY FROM generate_series(1, $ROWS) g";
	$c->on_both($ins);
	$c->on_every_node('ANALYZE');
	$c->on_single('ANALYZE');

	my $tag = "name " . ($i + 1);
	$c->compare("$tag: scan", "SELECT id, $COL, v FROM $T WHERE v < 2 ORDER BY id, 2", ordered => 1, plans => ['pushed', 'scan'], profiles => 0);
	$c->compare("$tag: join", "SELECT a.id, b.$COL FROM $T a JOIN $T b ON a.id = b.id WHERE a.v = 1 ORDER BY 1, 2", ordered => 1, plans => ['pushed', 'scan'], profiles => 0);
	$c->compare("$tag: partial aggregate", "SELECT v, count(*), max($COL) FROM $T GROUP BY v ORDER BY v", ordered => 1, plans => ['pushed', 'scan'], profiles => 0,
		path => 'Finalize');
	$c->compare("$tag: closed aggregate", "SELECT id, count(*), max($COL) FROM $T GROUP BY id ORDER BY id", ordered => 1, plans => ['pushed', 'scan'], profiles => 0,
		path => 'Kingres Fragment Scan');
	$c->compare("$tag: ordered merge with a limit", "SELECT id, $COL FROM $T ORDER BY $COL COLLATE \"C\", id LIMIT 7", ordered => 1, plans => ['pushed', 'scan'], profiles => 0);

	# a router INSERT with RETURNING and ON CONFLICT DO NOTHING (a repeated key is dropped, so a repeated run returns the rest)
	my $r = "INSERT INTO $T SELECT g, E'new ' || g, 1, ROW(g, 'y')::$Q.$TY FROM generate_series($ROWS - 4, $ROWS + 6) g ON CONFLICT DO NOTHING RETURNING id, $COL";
	my ($rs, $os, $es) = run_sql($single, $r);
	my ($rd, $od, $ed) = run_sql($n1, $r);
	is($rs, 0, "$tag: the single node runs the INSERT with RETURNING") or diag($es);
	is($rd, 0, "$tag: the cluster runs the INSERT with RETURNING") or diag($ed);
	is(join("\n", sort split /\n/, $od), join("\n", sort split /\n/, $os), "$tag: RETURNING gives the single node's rows");
	$c->compare_tables($T, name => "$tag: after the INSERT", order_by => "id, $COL");

	# COPY with a column list
	my $data = join('', map { "$_\tcopy\\\\x\\n$_\n" } ($ROWS + 10 .. $ROWS + 14));
	my ($cc, $cs) = $c->copy_in_both($T, $data, columns => [ 'id', $COL ]);
	is($cc->[0], $cs->[0], "$tag: COPY with a column list: the same status") or diag("$cc->[2] / $cs->[2]");
	$c->compare_tables($T, name => "$tag: after the COPY", order_by => "id, $COL");
}

# ---------------------------------------------------------------------------
# B. values
# ---------------------------------------------------------------------------
my $types = q{CREATE TYPE public.vcomp AS (a int, b text); CREATE TYPE public.vtr AS RANGE (subtype = text); CREATE TYPE public.venum AS ENUM ('a''b', 'c"d', 'e f', 'NULL', 'x\y');};
$c->on_every_node($types);
$c->on_single($types);
my $vcols = q{id int, s text, b bytea, j jsonb, a text[], ty public.vcomp, rg public.vtr, e public.venum, ts timestamptz, f float8, n numeric, m money};
for my $t (qw(vals vals_r))
{
	$c->on_cluster("CREATE TABLE public.$t ($vcols, PRIMARY KEY (id)); SELECT kingres.create_distributed_table('public.$t', 'id', bucket_count => " . ($t eq 'vals_r' ? 7 : 12) . ')');
	$c->on_single("CREATE TABLE public.$t ($vcols, PRIMARY KEY (id))");
}
{
	my $four = $utf8 ? 'chr(128512)' : "'four'";
	my $two = $utf8 ? 'chr(233)' : "'two'";
	my $ins = qq{INSERT INTO public.vals
SELECT i,
  (ARRAY[\$\$it's\$\$, \$\$back\\slash\$\$, \$\$dbl\\\\slash\$\$, 'line1' || chr(10) || 'line2', 'tab' || chr(9) || 'here', 'cr' || chr(13) || 'here', '{', '}', ',', '"', 'NULL', \$\$\\N\$\$, '', $two, $four, 'plain'])[i],
  (SELECT decode(string_agg(lpad(to_hex(g), 2, '0'), '' ORDER BY g), 'hex') FROM generate_series(0, 255) g),
  (ARRAY['{"a": "q\\"uote", "b": ["x\\\\y", {"c": "it''s"}]}', '{"k": null}', '[]', '"\\\\"'])[1 + i % 4]::jsonb,
  (SELECT arr FROM (VALUES (1, ARRAY['a b', NULL, 'quo"te', 'back\\slash', ',', '{x}', 'NULL', '']), (2, ARRAY['q''uote']), (0, ARRAY[NULL::text])) v(k, arr) WHERE k = i % 3),
  ROW(i, \$\$it's "x"\$\$)::public.vcomp,
  (ARRAY[public.vtr('a"b', 'c''d'), public.vtr('', 'z'), 'empty'::public.vtr])[1 + i % 3],
  (enum_range(NULL::public.venum))[1 + i % 5],
  (ARRAY['0044-03-15 12:00:00+00 BC'::timestamptz, 'infinity', '-infinity', '2026-01-02 03:04:05.678901+05:30'])[1 + i % 4],
  (ARRAY['-0'::float8, 'NaN', 1e308, 'Infinity', -1e308, 0.1, 0.30000000000000004])[1 + i % 7],
  (ARRAY[repeat('9', 1000)::numeric, 0.000000000000000000000000000001::numeric, -1::numeric])[1 + i % 3],
  (i * 1234.56)::numeric::money
FROM generate_series(1, 16) i};
	$c->on_both($ins);
	$c->on_both(q{UPDATE public.vals SET s = repeat('x', 1048576) WHERE id = 16});
	$c->on_every_node('ANALYZE');
	$c->on_single('ANALYZE');

	$c->compare('values: the whole table by the scan', q{SELECT id, md5(s), md5(b::text), md5(j::text), md5(a::text), md5(ty::text), md5(rg::text), e::text, ts::text, f::text, md5(n::text), m::text FROM public.vals ORDER BY id},
		ordered => 1, plans => ['pushed', 'scan']);
	$c->compare('values: the rows themselves', q{SELECT id, s, a, ty, rg, e, ts, f FROM public.vals WHERE id < 16 ORDER BY id}, ordered => 1, plans => ['pushed', 'scan'], normalize => 1);
	$c->compare('values: bytea and numeric', q{SELECT id, encode(b, 'hex') = (SELECT string_agg(lpad(to_hex(g), 2, '0'), '' ORDER BY g) FROM generate_series(0, 255) g), n FROM public.vals WHERE id % 3 = 0 ORDER BY id}, ordered => 1, plans => ['pushed', 'scan']);
	# constants that the origin deparses into the text of the fragment: the transmission modes must make them mean the same to the groups
	my $hex256 = join('', map { sprintf('%02x', $_) } (0 .. 255));
	$c->compare('values: a float8 constant that needs 17 digits', q{SELECT id, f::text FROM public.vals WHERE f = 0.30000000000000004::float8 ORDER BY id}, ordered => 1, plans => ['pushed', 'scan']);
	$c->compare('values: a bytea constant with the 256 byte values', "SELECT id FROM public.vals WHERE b = decode('$hex256', 'hex') ORDER BY id", ordered => 1, plans => ['pushed', 'scan']);
	$c->compare('values: a closed aggregate restricted by a float8 and a bytea constant', "SELECT id, count(*) FROM public.vals WHERE (f = 0.30000000000000004::float8 AND b = decode('$hex256', 'hex')) OR id < 4 GROUP BY id ORDER BY id", ordered => 1, plans => ['pushed', 'scan']);
	$c->compare('values: a partial aggregate', q{SELECT count(*), max(s COLLATE "C"), min(s COLLATE "C"), max(b), min(b), max(a), min(a), max(e), min(e), max(ts), min(ts), max(f), min(f), min(n), max(n) FROM public.vals},
		plans => ['pushed', 'scan'], normalize => 1);
	$c->compare('values: money stays on the scan and is right', q{SELECT max(m), min(m), count(*) FROM public.vals}, plans => ['pushed', 'scan']);
	$c->compare('values: a closed aggregate', q{SELECT id, count(*), max(s COLLATE "C"), max(ts), max(f) FROM public.vals GROUP BY id ORDER BY id}, ordered => 1, plans => ['pushed', 'scan'], normalize => 1);
	$c->compare('values: an ordered merge by a string', q{SELECT id, md5(s) FROM public.vals ORDER BY s COLLATE "C", id LIMIT 6}, ordered => 1, plans => ['pushed', 'scan']);
	$c->compare('values: an ordered merge by timestamptz', q{SELECT id, ts::text FROM public.vals ORDER BY ts, id LIMIT 9}, ordered => 1, plans => ['pushed', 'scan']);
	$c->compare('values: an ordered merge by float8 (-0, NaN, infinity)', q{SELECT id, f::text FROM public.vals ORDER BY f DESC NULLS LAST, id LIMIT 9}, ordered => 1, plans => ['pushed', 'scan']);
	$c->compare('values: a join', q{SELECT a.id, md5(a.s), md5(b.a::text), b.e FROM public.vals a JOIN public.vals b ON a.id = b.id WHERE a.id % 3 = 0 ORDER BY 1}, ordered => 1, plans => ['pushed', 'scan']);

	# the router: every value is inserted through it (from another colocation group) and read back by md5 of its text form
	my $r = q{INSERT INTO public.vals_r SELECT * FROM public.vals ON CONFLICT DO NOTHING RETURNING id, md5(s)};
	my ($rs, $os, $es) = run_sql($single, $r);
	my ($rd, $od, $ed) = run_sql($n2, $r);
	is($rd, 0, 'values: the router inserts every value') or diag($ed);
	is(join("\n", sort split /\n/, $od), join("\n", sort split /\n/, $os), '... and RETURNING gives the single node\'s md5 of the strings');
	$c->compare_tables('public.vals_r', name => 'values: after the router', query => q{SELECT id, md5(s), md5(b::text), md5(j::text), md5(a::text), md5(ty::text), md5(rg::text), e::text, ts::text, f::text, md5(n::text), m::text FROM public.vals_r ORDER BY id}, order_by => 'id');
	# and the groups' own text of each value equals the origin's (md5 on the group, by a closed query)
	$c->compare('values: the md5 of the text form on the groups', q{SELECT id, md5(s || b::text || j::text || a::text || ty::text || rg::text || e::text || ts::text || f::text || n::text) FROM public.vals_r GROUP BY id, s, b, j, a, ty, rg, e, ts, f, n ORDER BY id}, ordered => 1, plans => ['pushed', 'scan']);

	# COPY of hostile strings and arrays, with a column list
	my @cs = ("it's", 'back\\\\slash', 'dbl\\\\\\\\slash', 'line1\\nline2', 'tab\\there', 'cr\\rhere', '{', '}', ',', '"', 'NULL', '\\\\N', '', 'plain');
	my $i = 100;
	my $data = join('', map { $i++ . "\t$_\t" . '{"a b",NULL,"quo\\\\"te","back\\\\\\\\slash",","}' . "\n" } @cs);
	my ($cc, $css) = $c->copy_in_both('public.vals_r', $data, columns => [ 'id', 's', 'a' ]);
	is($cc->[0], $css->[0], 'values: COPY with a column list: the same status') or diag("$cc->[2] / $css->[2]");
	$c->compare_tables('public.vals_r', name => 'values: after the COPY', query => q{SELECT id, md5(s), md5(a::text) FROM public.vals_r WHERE id >= 100 ORDER BY id}, order_by => 'id');
}

# ---------------------------------------------------------------------------
# C. objects that shadow built-in names, and session settings
# ---------------------------------------------------------------------------
{
	my $shadow = q{CREATE FUNCTION public.sum(int) RETURNS bigint LANGUAGE sql IMMUTABLE AS 'SELECT 42::bigint';
		CREATE FUNCTION public.texteq2(text, text) RETURNS bool LANGUAGE sql IMMUTABLE AS 'SELECT false';
		CREATE OPERATOR public.= (LEFTARG = text, RIGHTARG = text, FUNCTION = public.texteq2);
		CREATE DOMAIN public.text AS pg_catalog.text};
	$c->on_every_node($shadow);
	$c->on_single($shadow);
	my $T = 'public.vals_r';
	$c->compare('shadow: sum(int) under a search_path that puts public first', q{SET search_path = public, pg_catalog; SELECT id, sum(id) FROM public.vals_r WHERE id < 5 ORDER BY id}, ordered => 1, profiles => 0);
	$c->compare('shadow: an equality on text under that search_path', q{SET search_path = public, pg_catalog; SELECT count(*) FROM public.vals_r WHERE s = 'plain'}, profiles => 0);
	$c->compare('shadow: the same with the group by the key', q{SET search_path = public, pg_catalog; SELECT id, count(*) FROM public.vals_r WHERE id < 100 GROUP BY id ORDER BY id}, ordered => 1, profiles => 0);
	$c->compare('shadow: a cast to the domain named text', q{SET search_path = public, pg_catalog; SELECT id, pg_typeof(s::text)::text FROM public.vals_r WHERE id < 5 ORDER BY id}, ordered => 1, profiles => 0);
	$c->compare('shadow: a join and an ordered merge', q{SET search_path = public, pg_catalog; SELECT a.id, md5(a.s) FROM public.vals a JOIN public.vals_r b ON a.id = b.id WHERE a.id < 100 ORDER BY a.s COLLATE "C", a.id LIMIT 5}, ordered => 1, profiles => 0);
	my $scs_off = ((run_sql($single, 'SET standard_conforming_strings = off'))[0] == 0);
	note('standard_conforming_strings = off is not accepted by this server') unless $scs_off;
	for my $set ($scs_off ? ('SET standard_conforming_strings = off', 'SET standard_conforming_strings = off; SET escape_string_warning = off') : (), q{SET search_path = "$user", public})
	{
		$c->compare("session: $set: a restriction with backslashes", "$set; SELECT id FROM public.vals WHERE s LIKE E'%\\\\\\\\%' OR s = 'it''s' ORDER BY id", ordered => 1, profiles => 0);
		$c->compare("session: $set: a partial aggregate", "$set; SELECT count(*), max(s COLLATE \"C\") FROM public.vals", profiles => 0);
	}
	my $drop = q{DROP OPERATOR public.= (text, text); DROP FUNCTION public.texteq2(text, text); DROP FUNCTION public.sum(int); DROP DOMAIN public.text};
	$c->on_every_node($drop);
	$c->on_single($drop);
}

$c->summary if $c->can('summary');
done_testing();
