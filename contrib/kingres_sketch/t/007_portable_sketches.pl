# contrib/kingres_sketch/t/007_portable_sketches.pl
#
# A sketch depends on the values only.
#
#   * A sketch is built of a column of every kind of type (scalars, enums,
#     arrays, ranges, multiranges, composite types, domains), the database is
#     dumped and restored into a database where the same objects have other
#     OIDs, and the stored sketches are byte for byte what the aggregate
#     builds from the restored data (no element hashes by an OID).
#   * topn refuses strings with a nondeterministic collation, where equal
#     values are not equal bytes (skipped when the server has no such collation).
#   * The working memory of a call on a value of the largest size.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $src = PostgreSQL::Test::Cluster->new('portable_src');
$src->init;
$src->start;
my $dst = PostgreSQL::Test::Cluster->new('portable_dst');
$dst->init;
$dst->start;

# the columns: name, type, three SQL values (text of a literal of that type)
my @cols = (
	[ 'c_int2', 'int2', '1', '-7', '300' ],
	[ 'c_int4', 'int4', '1', '-7', '70000' ],
	[ 'c_int8', 'int8', '1', '-7', '5000000000' ],
	[ 'c_numeric', 'numeric', '1.5', '2.50', '-1e20' ],
	[ 'c_float4', 'float4', '1.5', '-2.25', '1e20' ],
	[ 'c_float8', 'float8', '1.5', '-0', '1e300' ],
	[ 'c_text', 'text', "'a'", "'b'", "'é'" ],
	[ 'c_varchar', 'varchar(5)', "'a'", "'b'", "'abc'" ],
	[ 'c_bpchar', 'char(4)', "'a'", "'b '", "'abc'" ],
	[ 'c_bool', 'bool', 'true', 'false', 'true' ],
	[ 'c_uuid', 'uuid', "'123e4567-e89b-12d3-a456-426614174000'", "'00000000-0000-0000-0000-000000000001'", "'ffffffff-ffff-ffff-ffff-ffffffffffff'" ],
	[ 'c_date', 'date', "'2026-01-01'", "'1999-12-31'", "'infinity'" ],
	[ 'c_timestamptz', 'timestamptz', "'2026-01-01 00:00:00+00'", "'1999-12-31 12:00:00+00'", "'-infinity'" ],
	[ 'c_interval', 'interval', "'1 day'", "'30 days'", "'1 month'" ],
	[ 'c_inet', 'inet', "'10.0.0.1'", "'::1'", "'192.168.0.0/16'" ],
	[ 'c_jsonb', 'jsonb', "'{\"a\": 1}'", "'[1, 2]'", "'\"x\"'" ],
	[ 'c_bytea', 'bytea', "'\\x00ff'", "'\\x'", "'\\x41'" ],
	[ 'c_enum', 'colr', "'r'", "'g'", "'b'" ],
	[ 'c_enum_array', 'colr[]', "'{r,g}'", "'{b}'", "'{r,NULL,b}'" ],
	[ 'c_enum_range', 'colr_range', "'[r,g]'", "'[g,g]'", "'[g,b)'" ],
	[ 'c_enum_domain', 'dcolr', "'r'", "'g'", "'b'" ],
	[ 'c_composite', 'pt', "'(1,r,x)'", "'(2,g,y)'", "'(,b,)'" ],
	[ 'c_composite_array', 'pt[]', "'{\"(1,r,x)\",\"(2,g,y)\"}'", "'{\"(1,r,x)\"}'", "'{}'" ],
	[ 'c_int_array', 'int4[]', "'{1,2}'", "'{{1,2},{3,4}}'", "'{NULL}'" ],
	[ 'c_text_array', 'text[]', "'{a,b}'", "'{\"a b\"}'", "'{}'" ],
	[ 'c_int_range', 'int4range', "'[1,5)'", "'empty'", "'(,3]'" ],
	[ 'c_ts_range', 'tstzrange', "'[2026-01-01,2026-02-01)'", "'empty'", "'[2026-03-01,)'" ],
	[ 'c_multirange', 'int4multirange', "'{[1,3),[5,7)}'", "'{}'", "'{[1,9)}'" ],
	[ 'c_enum_multirange', 'colr_multirange', "'{[r,g]}'", "'{}'", "'{[g,g],[b,b]}'" ],
);

my $types = <<'EOS';
CREATE TYPE colr AS ENUM ('r', 'g', 'b');
CREATE TYPE colr_range AS RANGE (subtype = colr, multirange_type_name = colr_multirange);
CREATE TYPE pt AS (a int, c colr, t text);
CREATE DOMAIN dcolr AS colr;
EOS
my $table = 'CREATE TABLE v (id serial PRIMARY KEY, ' . join(', ', map { "$_->[0] $_->[1]" } @cols) . ');';
my $inserts = '';
foreach my $i (2 .. 4)
{
	$inserts .= 'INSERT INTO v (' . join(', ', map { $_->[0] } @cols) . ') VALUES ('
	  . join(', ', map { "$_->[$i]" } @cols) . ");\n";
}
# every value twice, so that the sketches count distinct values, and a NULL row
my $names = join(', ', map { $_->[0] } @cols);
$inserts .= "INSERT INTO v ($names) SELECT $names FROM v;\nINSERT INTO v (id) VALUES (DEFAULT);\n";

$src->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');
$src->safe_psql('postgres', "SET search_path = public, kingres_sketch;\n$types\n$table\n$inserts");

# one stored sketch per column, an anonymous record column as well
my $stores = join("\n", map {
	"INSERT INTO sk SELECT '$_->[0]', hll_add_agg($_->[0]), approx_count_distinct($_->[0]) FROM v;"
} @cols);
$stores .= "\nINSERT INTO sk SELECT 'record', hll_add_agg(ROW(c_int4, c_enum, c_text)), approx_count_distinct(ROW(c_int4, c_enum, c_text)) FROM v;";
$src->safe_psql('postgres', "SET search_path = public, kingres_sketch;\nCREATE TABLE sk (name text PRIMARY KEY, h hll, n bigint);\n$stores");

# every column gave a sketch, none is empty
is($src->safe_psql('postgres', 'SELECT count(*), count(*) FILTER (WHERE kingres_sketch.hll_is_empty(h)) FROM sk'),
	(scalar(@cols) + 1) . '|0', 'a sketch for every column and a record');

my $fingerprint = 'SELECT md5(string_agg(name || h::text || n, \',\' ORDER BY name)) FROM sk';

# the same objects with other OIDs: junk first, then the dump
my $dir = PostgreSQL::Test::Utils::tempdir;
$src->command_ok([ 'pg_dump', '-f', "$dir/dump.sql", '-d', $src->connstr('postgres') ], 'pg_dump');
$dst->safe_psql('postgres', 'CREATE DATABASE restored');
$dst->safe_psql('restored', "CREATE TYPE junk AS ENUM (" . join(', ', map { "'j$_'" } 1 .. 300) . ");\n"
	  . join("\n", map { "CREATE TABLE junk_$_ (a int);" } 1 .. 40));
$dst->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $dst->connstr('restored'), '-f', "$dir/dump.sql" ],
	'restore into a database with other OIDs');

my $o1 = $src->safe_psql('postgres', "SELECT oid FROM pg_enum WHERE enumlabel = 'r' AND enumtypid = 'colr'::regtype");
my $o2 = $dst->safe_psql('restored', "SELECT oid FROM pg_enum WHERE enumlabel = 'r' AND enumtypid = 'colr'::regtype");
isnt($o1, $o2, 'the enum label has another OID in the restored database');

is($dst->safe_psql('restored', $fingerprint), $src->safe_psql('postgres', $fingerprint),
	'the stored sketches of every column are the same bytes after the restore');

# rebuilt from the restored data, in the restored database, one column at a time
my $bad = 0;
foreach my $c (@cols, [ 'record' ])
{
	my $expr = $c->[0] eq 'record' ? 'ROW(c_int4, c_enum, c_text)' : $c->[0];
	my $ok = $dst->safe_psql('restored', <<"EOS");
SET search_path = public, kingres_sketch;
SELECT (SELECT hll_add_agg($expr)::bytea FROM v) = (SELECT h::bytea FROM sk WHERE name = '$c->[0]')
   AND (SELECT approx_count_distinct($expr) FROM v) = (SELECT n FROM sk WHERE name = '$c->[0]');
EOS
	if ($ok ne 't') { $bad++; diag("column $c->[0]: a rebuilt sketch differs from the stored one after the restore"); }
}
is($bad, 0, 'a sketch rebuilt in the restored database equals the stored one, for every column');

# and the counts are the number of distinct values (3 or 2 where two values are equal)
is($dst->safe_psql('restored', "SELECT n FROM sk WHERE name = 'c_enum'"), '3', 'three enum labels count as three');
is($dst->safe_psql('restored', "SELECT n FROM sk WHERE name = 'c_enum_array'"), '3', 'three enum arrays count as three');

# a sketch built in the source database from a column and from the other database's column
# of another enum with the same labels are the same
$dst->safe_psql('restored', <<'EOS');
CREATE TYPE colr_other AS ENUM ('b', 'g', 'r', 'z');
CREATE TABLE e2 (c colr_other);
INSERT INTO e2 VALUES ('r'), ('g'), ('b');
EOS
is($dst->safe_psql('restored', "SET search_path = public, kingres_sketch; SELECT (SELECT hll_add_agg(c)::bytea FROM e2) = (SELECT h::bytea FROM sk WHERE name = 'c_enum')"),
	't', 'an enum with the same labels and other OIDs and order gives the same sketch');

# the types whose hash is an OID are refused
foreach my $t ('oid', 'regclass')
{
	my ($ret, $out, $err) = $src->psql('postgres', "SELECT kingres_sketch.hll_add(kingres_sketch.hll_empty(4), NULL::$t)");
	isnt($ret, 0, "a column of type $t is refused");
	like($err, qr/a sketch does not take type $t/, "with a message that names $t");
}

# ---- topn and a nondeterministic collation ----
my ($ret, $out, $err) = $src->psql('postgres',
	"CREATE COLLATION nd (provider = icu, locale = 'und', deterministic = false)");
SKIP:
{
	skip 'no ICU collation available', 4 if $ret != 0;
	($ret, $out, $err) = $src->psql('postgres', "SELECT kingres_sketch.topn_add(kingres_sketch.topn_empty(), 'a'::text COLLATE nd)");
	isnt($ret, 0, 'topn refuses a string with a nondeterministic collation');
	like($err, qr/nondeterministic collation/, 'with a message that says why');
	is($src->safe_psql('postgres', "CREATE COLLATION nd_ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false); "
		  . "SELECT kingres_sketch.hll_cardinality(kingres_sketch.hll_add(kingres_sketch.hll_add(kingres_sketch.hll_empty(), 'a'::text COLLATE nd_ci), 'A'::text COLLATE nd_ci))"),
		'1', 'hll counts two strings that are equal under a case-insensitive collation once');
	is($src->safe_psql('postgres', "SELECT count(*) FROM kingres_sketch.topn_list(kingres_sketch.topn_add(kingres_sketch.topn_empty(), 'a'::text COLLATE \"C\"))"),
		'1', 'topn takes the same string with a deterministic collation');
}

# ---- the working memory of a call on the largest value ----
# capacity 100000 with items of 1024 bytes: a value of about 104 MB.  The peak
# of the backend (VmHWM) of a union of two such values must stay under a
# bound that the README states.
$src->safe_psql('postgres', <<'EOS');
SET search_path = public, kingres_sketch;
CREATE TABLE big AS
  SELECT topn_add_agg(repeat(md5(i::text), 32), 100000) AS t FROM generate_series(1, 100000) i;
EOS
my $size = $src->safe_psql('postgres', 'SELECT length(t::bytea) FROM big');
cmp_ok($size, '>', 100000000, "the value is of the largest size ($size bytes)");
my $peak = $src->safe_psql('postgres', <<'EOS');
SET search_path = public, kingres_sketch;
SELECT length(topn_union(t, t)::bytea) FROM big \gset
SELECT (regexp_match(pg_read_file('/proc/self/status'), 'VmHWM:\s+(\d+) kB'))[1]::bigint * 1024;
EOS
my $ratio = $peak / $size;
diag(sprintf("peak memory of topn_union on the largest value: %.0f MB, %.1f times the value", $peak / 1048576, $ratio));
cmp_ok($ratio, '<', 5.0, 'the peak of a union of two largest values is under 5 times the value');

$src->stop;
$dst->stop;
done_testing();
