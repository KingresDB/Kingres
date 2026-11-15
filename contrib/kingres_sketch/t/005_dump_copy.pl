# contrib/kingres_sketch/t/005_dump_copy.pl
#
# Stored sketch columns survive the ways a table is moved:
#   * pg_dump (plain and custom format, data as COPY and as INSERT) restored
#     into a database of another server, where the extension is created by
#     the dump;
#   * COPY in the text and the binary format, to a file and back;
#   * the extension is dumped as CREATE EXTENSION, not as its functions.
# In every case the restored values are the same bytes, and the functions
# that read them give the same answers.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $src = PostgreSQL::Test::Cluster->new('dump_src');
$src->init;
$src->start;
my $dst = PostgreSQL::Test::Cluster->new('dump_dst');
$dst->init;
$dst->start;

$src->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');
$src->safe_psql('postgres', <<'EOS');
SET search_path = public, kingres_sketch;
SELECT setseed(0.9);
-- a rollup table with one sketch of each type per row, NULLs and empty
-- sketches included, with sparse and dense hlls and a topn that overflowed
CREATE TABLE rollup (
    customer_id bigint NOT NULL, hour timestamptz NOT NULL, views bigint NOT NULL,
    visitors kingres_sketch.hll, top_pages kingres_sketch.topn, latency kingres_sketch.tdigest,
    PRIMARY KEY (customer_id, hour));
INSERT INTO rollup
  SELECT c, timestamptz '2026-01-01 00:00:00+00' + h * interval '1 hour', count(*),
         hll_add_agg(v, 12), topn_add_agg(page, 20), tdigest_agg(lat, 50)
    FROM (SELECT c, h, (random() * 100000)::bigint AS v, 'page' || (exp(random() * ln(300)))::int AS page,
                 random() * 500 AS lat
            FROM generate_series(1, 4) c, generate_series(0, 5) h, generate_series(1, 40 * c * c) r) x
   GROUP BY c, h;
INSERT INTO rollup VALUES (100, '2026-02-01+00', 0, hll_empty(10), topn_empty(5), tdigest_empty(10));
INSERT INTO rollup VALUES (101, '2026-02-01+00', 0, NULL, NULL, NULL);
INSERT INTO rollup VALUES (102, '2026-02-01+00', 1, hll_add(hll_empty(18), 'a'::text), topn_add(topn_empty(1), 'é'::text),
                           tdigest_add(tdigest_empty(10000), -1.5e300, 3));
-- one very large hll (dense, precision 18) and one topn with long items
INSERT INTO rollup VALUES (103, '2026-02-01+00', 2,
    (SELECT hll_add_agg(i, 18) FROM generate_series(1, 300000) i),
    (SELECT topn_add_agg(repeat(chr(65 + i % 20), 900 + i % 100), 30) FROM generate_series(1, 3000) i),
    (SELECT tdigest_agg(i::float8 * i, 500) FROM generate_series(1, 50000) i));
EOS

my $fingerprint = <<'EOS';
SELECT md5(string_agg(customer_id || '/' || hour || '/' || views || '/' || coalesce(visitors::text, '-') || '/'
                      || coalesce(top_pages::text, '-') || '/' || coalesce(latency::text, '-'),
                      ',' ORDER BY customer_id, hour)) || ' ' || count(*) FROM rollup;
EOS
my $answers = <<'EOS';
SET search_path = public, kingres_sketch;
SELECT md5(string_agg(coalesce(hll_cardinality(visitors)::text, '-') || ':' || coalesce(topn_total(top_pages)::text, '-')
                      || ':' || coalesce(tdigest_quantile(latency, 0.5)::text, '-'), ',' ORDER BY customer_id, hour))
  FROM rollup;
EOS
my $expected_fp = $src->safe_psql('postgres', $fingerprint);
my $expected_answers = $src->safe_psql('postgres', $answers);
like($expected_fp, qr/ 28$/, 'the source table has 28 rows');

my $dir = PostgreSQL::Test::Utils::tempdir;

sub check_restored
{
	my ($db, $what) = @_;
	is($dst->safe_psql($db, $fingerprint), $expected_fp, "$what: the stored bytes are identical");
	is($dst->safe_psql($db, $answers), $expected_answers, "$what: the functions answer the same");
}

# plain format, data as COPY
$src->command_ok([ 'pg_dump', '-f', "$dir/plain.sql", '-d', $src->connstr('postgres') ], 'pg_dump, plain format');
my $dump = slurp_file("$dir/plain.sql");
like($dump, qr/CREATE EXTENSION IF NOT EXISTS kingres_sketch/, 'the dump creates the extension');
unlike($dump, qr/CREATE FUNCTION kingres_sketch\./, 'the dump does not contain the functions');
unlike($dump, qr/CREATE TYPE kingres_sketch\./, 'the dump does not contain the types');
$dst->safe_psql('postgres', 'CREATE DATABASE d_plain');
$dst->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $dst->connstr('d_plain'), '-f', "$dir/plain.sql" ],
	'restore of the plain dump');
check_restored('d_plain', 'plain dump');

# plain format, data as INSERT statements
$src->command_ok([ 'pg_dump', '--column-inserts', '-f', "$dir/inserts.sql", '-d', $src->connstr('postgres') ],
	'pg_dump with column inserts');
$dst->safe_psql('postgres', 'CREATE DATABASE d_ins');
$dst->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $dst->connstr('d_ins'), '-f', "$dir/inserts.sql" ],
	'restore of the dump with inserts');
check_restored('d_ins', 'dump with inserts');

# custom format
$src->command_ok([ 'pg_dump', '-Fc', '-f', "$dir/custom.dump", '-d', $src->connstr('postgres') ], 'pg_dump, custom format');
$dst->safe_psql('postgres', 'CREATE DATABASE d_custom');
$dst->command_ok([ 'pg_restore', '-d', $dst->connstr('d_custom'), "$dir/custom.dump" ], 'pg_restore of the custom dump');
check_restored('d_custom', 'custom dump');

# the schema of the dump has the columns with their types
$src->command_like([ 'pg_dump', '--schema-only', '-t', 'rollup', '-d', $src->connstr('postgres') ],
	qr/visitors kingres_sketch\.hll,\s+top_pages kingres_sketch\.topn,\s+latency kingres_sketch\.tdigest/,
	'the dumped table definition names the sketch types with their schema');

# COPY, text and binary, through files, in the source database and into the other server
foreach my $format ('text', 'binary')
{
	my $file = "$dir/rollup.$format";
	$src->safe_psql('postgres', "COPY rollup TO '$file' (FORMAT $format)");
	$src->safe_psql('postgres', "CREATE TABLE rollup_$format (LIKE rollup INCLUDING ALL)");
	$src->safe_psql('postgres', "COPY rollup_$format FROM '$file' (FORMAT $format)");
	is($src->safe_psql('postgres', "SELECT count(*) FROM rollup r JOIN rollup_$format c USING (customer_id, hour) "
		  . "WHERE r.visitors::bytea IS NOT DISTINCT FROM c.visitors::bytea "
		  . "AND r.top_pages::bytea IS NOT DISTINCT FROM c.top_pages::bytea "
		  . "AND r.latency::bytea IS NOT DISTINCT FROM c.latency::bytea"),
		'28', "COPY $format: 28 rows round trip within the server, bytes identical");

	# into the other server (the binary format carries the type's send output)
	$dst->safe_psql('postgres', "CREATE DATABASE d_copy_$format");
	$dst->safe_psql("d_copy_$format", 'CREATE EXTENSION kingres_sketch');
	$dst->safe_psql("d_copy_$format", <<'EOS');
CREATE TABLE rollup (
    customer_id bigint NOT NULL, hour timestamptz NOT NULL, views bigint NOT NULL,
    visitors kingres_sketch.hll, top_pages kingres_sketch.topn, latency kingres_sketch.tdigest,
    PRIMARY KEY (customer_id, hour));
EOS
	$dst->safe_psql("d_copy_$format", "COPY rollup FROM '$file' (FORMAT $format)");
	check_restored("d_copy_$format", "COPY $format into another server");
}

# CSV of a sketch column: the text form is a plain string with no quoting problems
$src->safe_psql('postgres', "COPY rollup TO '$dir/rollup.csv' (FORMAT csv)");
$dst->safe_psql('postgres', 'CREATE DATABASE d_csv');
$dst->safe_psql('d_csv', 'CREATE EXTENSION kingres_sketch');
$dst->safe_psql('d_csv', <<'EOS');
CREATE TABLE rollup (
    customer_id bigint NOT NULL, hour timestamptz NOT NULL, views bigint NOT NULL,
    visitors kingres_sketch.hll, top_pages kingres_sketch.topn, latency kingres_sketch.tdigest,
    PRIMARY KEY (customer_id, hour));
EOS
$dst->safe_psql('d_csv', "COPY rollup FROM '$dir/rollup.csv' (FORMAT csv)");
check_restored('d_csv', 'CSV copy');

# the binary format is the bytes of the value: a hll sent in binary is the
# bytea of the value, with no wrapper
is($src->safe_psql('postgres', "SELECT encode(kingres_sketch.hll_send(visitors), 'hex') = encode(visitors::bytea, 'hex') "
	  . "FROM rollup WHERE customer_id = 1 AND hour = '2026-01-01 00:00:00+00'"),
	't', 'send is the bytes of the value');

# a corrupted binary file is refused, and the table stays as it was
my $good = slurp_file("$dir/rollup.binary");
my $bad = $good;
# flip the magic byte of the first hll value (magic, format 1, kind 1)
my $pos = index($good, "\x4b\x01\x01");
die "no hll value found in the binary file" if $pos < 0;
substr($bad, $pos, 1) = chr(ord(substr($bad, $pos, 1)) ^ 0xff);
open(my $fh, '>:raw', "$dir/rollup.corrupt") or die;
print $fh $bad;
close($fh);
$src->safe_psql('postgres', 'CREATE TABLE rollup_bad (LIKE rollup INCLUDING ALL)');
my ($ret, $stdout, $stderr) = $src->psql('postgres', "COPY rollup_bad FROM '$dir/rollup.corrupt' (FORMAT binary)");
isnt($ret, 0, 'COPY of a corrupted binary file fails');
like($stderr, qr/invalid binary input for type|invalid input|insufficient data|incorrect binary data format/,
	'with an error from the input function');
is($src->safe_psql('postgres', 'SELECT count(*) FROM rollup_bad'), '0', 'and nothing was stored');

$src->stop;
$dst->stop;
done_testing();
