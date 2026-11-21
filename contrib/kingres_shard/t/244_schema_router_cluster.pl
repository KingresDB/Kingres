# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/244_schema_router_cluster.pl
#
# Phase 11 (design/shard-phase11.md 6.7 and 6.8 item 3, section 16 step 9): writes into a table of a distributed schema that read other schemas or other kinds of
# table, through the row router of 2b with one destination (the owner), on three nodes.
#
#   A. INSERT ... SELECT from a local table, a hash-distributed table, a table of another owner, a function: the rows are read here and go to the owner
#   B. COPY FROM into a shell
#   C. CREATE TABLE AS and SELECT INTO into a distributed schema
#   D. prepared statements, errors that leave nothing behind, the owner moves while a statement is planned

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('rt', 3);
my ($n1, $n2, $n3) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

sub fails
{
	my ($n, $sql, $re, $name) = @_;
	my ($rc, $out, $err) = run_sql($n, $sql);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "... message: $name") or diag($err);
}

sub same_everywhere
{
	my ($sql, $expected, $name) = @_;
	is(q1($_, $sql), $expected, $_->name . ": $name") for @nodes;
}

q1($n1, q{SELECT kingres.set_sharding_mode('both')});
q1($n1, q{CREATE SCHEMA ra; CREATE SCHEMA rb; SELECT kingres.distribute_schema('ra', 1); SELECT kingres.distribute_schema('rb', 2)});
q1($n1, q{
	CREATE TABLE ra.t (id int PRIMARY KEY, v text, n int DEFAULT 5, s serial);
	CREATE TABLE rb.w (id int PRIMARY KEY, v text);
	INSERT INTO rb.w SELECT g, 'w' || g FROM generate_series(1, 20) g;
	CREATE TABLE public.h (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.h', 'id', bucket_count => 8);
	INSERT INTO public.h SELECT g, 'h' || g FROM generate_series(1, 40) g;
});
q1($_, q{CREATE TABLE public.loc (id int PRIMARY KEY, tag text); INSERT INTO public.loc SELECT g, 'loc' || g FROM generate_series(1, 30) g}) for @nodes;
sub owner_count { my ($sql) = @_; return q1($n2, "SET kingres_shard.enable_routing = off; $sql"); }

# ---------------------------------------------------------------------------
# A. INSERT ... SELECT
# ---------------------------------------------------------------------------
for my $n ($n1, $n3)
{
	my $nm = $n->name;
	q1($n2, q{SET kingres_shard.enable_routing = off; TRUNCATE ra.t});
	q1($n, q{INSERT INTO ra.t (id, v) SELECT id, tag FROM public.loc});
	is(owner_count(q{SELECT count(*) FROM ra.t}), 30, "$nm: INSERT ... SELECT from a local table: every row is on the owner");
	is(q1($n, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM ra.t}), 0, "$nm: ... and none is in the shell here");
	is(owner_count(q{SELECT count(DISTINCT s) FROM ra.t}), 30, "$nm: the serial column is the owner's sequence (every value once)");
	is(owner_count(q{SELECT count(*) FROM ra.t WHERE n = 5}), 30, "$nm: and the constant default is applied by the owner");
	q1($n2, q{SET kingres_shard.enable_routing = off; TRUNCATE ra.t});
	q1($n, q{INSERT INTO ra.t (id, v) SELECT id + 100, v FROM public.h});
	is(owner_count(q{SELECT count(*) FROM ra.t}), 40, "$nm: from a hash-distributed table (read through the scan)");
	q1($n, q{INSERT INTO ra.t (id, v) SELECT id + 500, v FROM rb.w});
	is(owner_count(q{SELECT count(*) FROM ra.t WHERE id > 500}), 20, "$nm: from a table of another owner");
	q1($n, q{INSERT INTO ra.t (id, v) SELECT g + 900, 'g' FROM generate_series(1, 7) g});
	is(owner_count(q{SELECT count(*) FROM ra.t WHERE v = 'g'}), 7, "$nm: from a function that is not a table");
	is(q1($n, q{INSERT INTO ra.t (id, v) SELECT id + 2000, tag FROM public.loc WHERE id <= 3 RETURNING id}), "2001\n2002\n2003", "$nm: with RETURNING");
	q1($n, q{INSERT INTO ra.t (id, v) SELECT id + 2000, 'dup' FROM public.loc WHERE id <= 5 ON CONFLICT DO NOTHING});
	is(owner_count(q{SELECT count(*) FROM ra.t WHERE id BETWEEN 2001 AND 2005}), 5, "$nm: ON CONFLICT DO NOTHING");
	is(owner_count(q{SELECT count(*) FROM ra.t WHERE v = 'dup'}), 2, "$nm: ... only the new rows");
	fails($n, q{INSERT INTO ra.t (id, v) SELECT id + 100, tag FROM public.loc}, qr/duplicate key value violates unique constraint/, "$nm: a duplicate key is the error of the owner");
	is(owner_count(q{SELECT count(*) FROM ra.t}), 40 + 20 + 7 + 5, "$nm: ... and nothing of that statement is in the table");
}
q1($n2, q{SET kingres_shard.enable_routing = off; TRUNCATE ra.t});
fails($n1, q{INSERT INTO ra.t (id, v) SELECT id, tag FROM public.loc ON CONFLICT (id) DO UPDATE SET v = 'x'}, qr/ON CONFLICT DO UPDATE is not supported yet/, 'ON CONFLICT DO UPDATE keeps its refusal');
fails($n1, q{UPDATE ra.t SET v = l.tag FROM public.loc l WHERE l.id = ra.t.id}, qr/./, 'an UPDATE that reads a local table is still refused');
is(prepared_count($_), 0, $_->name . ': nothing is prepared') for @nodes;

# ---------------------------------------------------------------------------
# B. COPY FROM
# ---------------------------------------------------------------------------
for my $n ($n1, $n3)
{
	my $nm = $n->name;
	q1($n2, q{SET kingres_shard.enable_routing = off; TRUNCATE ra.t});
	my ($rc, $out, $err) = run_sql($n, q{COPY ra.t (id, v) FROM PROGRAM 'seq 1 50 | sed "s/.*/&\tcopy&/"'});
	is($rc, 0, "$nm: COPY FROM into a shell") or diag($err);
	like($out, qr/^$|COPY 50/, "$nm: ... the tag") or diag($out);
	is(owner_count(q{SELECT count(*) || ':' || count(DISTINCT s) FROM ra.t}), '50:50', "$nm: the rows are on the owner, with the serial default");
	is(q1($n, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM ra.t}), 0, "$nm: ... none in the shell");
	($rc, $out, $err) = run_sql($n, q{COPY ra.t (id, v) FROM PROGRAM 'printf "id,v\n1001,csv1\n1002,csv2\n"' (FORMAT csv, HEADER true)});
	is($rc, 0, "$nm: CSV with a header") or diag($err);
	is(owner_count(q{SELECT count(*) FROM ra.t WHERE id > 1000}), 2, "$nm: ... loaded");
	($rc, $out, $err) = run_sql($n, q{COPY ra.t (id, v) FROM PROGRAM 'printf "1\tdup\n"'});
	isnt($rc, 0, "$nm: a duplicate key");
	like($err, qr/duplicate key/, "$nm: ... is the error of the owner") or diag($err);
	fails($n, q{COPY ra.t (id, v) FROM PROGRAM 'printf "2001\tx\n"' WITH (FREEZE)}, qr/FREEZE is not supported/, "$nm: FREEZE");
}
run_sql($n1, q{COPY ra.t (id, v) FROM PROGRAM 'seq 3001 3010 | sed "s/.*/&	x/"'});
run_sql($n3, q{COPY ra.t (id, v) FROM PROGRAM 'seq 3011 3020 | sed "s/.*/&	x/"'});
is(owner_count(q{SELECT count(*) || ':' || count(DISTINCT s) FROM ra.t WHERE id BETWEEN 3001 AND 3020}), '20:20', 'loads from two nodes: the serial values come from the owner sequence (no value twice)');
q1($n2, q{SET kingres_shard.enable_routing = off; TRUNCATE ra.t});
{
	my ($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.enable_copy_routing = off; COPY ra.t (id, v) FROM PROGRAM 'printf "1\tx\n"'});
	isnt($rc, 0, 'with kingres_shard.enable_copy_routing off the load is refused as before');
	like($err, qr/enable_copy_routing off/, '... and says why') or diag($err);
}

# ---------------------------------------------------------------------------
# C. CREATE TABLE AS, SELECT INTO
# ---------------------------------------------------------------------------
q1($_, q{CREATE TABLE public.src (id int, price numeric(8,2), code varchar(5), note text); INSERT INTO public.src SELECT g, g * 1.5, 'c' || g, 'n' || g FROM generate_series(1, 25) g}) for @nodes;
q1($n1, q{CREATE TABLE ra.c1 AS SELECT id, price, code FROM public.src WHERE id <= 20});
same_everywhere(q{SELECT count(*) FROM kingres.dist_table WHERE relid = 'ra.c1'::regclass}, 1, 'CREATE TABLE AS into a distributed schema: the table is registered on every node');
is(owner_count(q{SELECT count(*) FROM ra.c1}), 20, '... its rows are on the owner');
same_everywhere(q{SELECT string_agg(a.attname || ':' || format_type(a.atttypid, a.atttypmod), ',' ORDER BY a.attnum) FROM pg_attribute a WHERE a.attrelid = 'ra.c1'::regclass AND a.attnum > 0}, 'id:integer,price:numeric(8,2),code:character varying(5)', '... with the types of the query, typmods included');
same_everywhere(q{SELECT count(*) FROM ra.c1}, 20, '... and every node reads it');
fails($n3, q{CREATE TABLE ra.c2 (a, b) AS SELECT id, note FROM public.src WHERE id <= 5}, qr/must run on the map group primary/, 'CREATE TABLE AS is DDL: from another node it is refused as every CREATE TABLE of a distributed schema');
q1($n1, q{CREATE TABLE ra.c2 (a, b) AS SELECT id, note FROM public.src WHERE id <= 5});
same_everywhere(q{SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid = 'ra.c2'::regclass AND attnum > 0}, 'a,b', 'the column names of the list');
q1($n1, q{CREATE TABLE ra.c3 AS SELECT * FROM public.src WITH NO DATA});
same_everywhere(q{SELECT count(*) FROM ra.c3}, 0, 'WITH NO DATA makes the empty table');
q1($n1, q{CREATE TABLE ra.c4 AS SELECT w.id, h.v FROM rb.w w JOIN public.h h ON h.id = w.id});
same_everywhere(q{SELECT count(*) FROM ra.c4}, 20, 'CREATE TABLE AS from a join of a table of another owner and a hash-distributed table');
q1($n1, q{SELECT id, note INTO ra.c5 FROM public.src WHERE id > 20});
same_everywhere(q{SELECT count(*) FROM ra.c5}, 5, 'SELECT ... INTO');
q1($n1, q{SET search_path = ra; CREATE TABLE c6 AS SELECT 1 AS x});
same_everywhere(q{SELECT to_regclass('ra.c6') IS NOT NULL}, 't', 'a table named without a schema goes to the schema of the path');
fails($n1, q{CREATE UNLOGGED TABLE ra.c7 AS SELECT 1 AS x}, qr/takes no storage option, tablespace, access method, ON COMMIT or UNLOGGED/, 'UNLOGGED');
fails($n1, q{CREATE TABLE ra.c8 WITH (fillfactor = 70) AS SELECT 1 AS x}, qr/takes no storage option/, 'a storage option');
fails($n1, q{CREATE TABLE ra.c1 AS SELECT 1 AS x}, qr/already exists/, 'a table that exists');
same_everywhere(q{SELECT count(*) FROM pg_class WHERE relname IN ('c7', 'c8') AND relnamespace = 'ra'::regnamespace}, 0, '... the refused ones left nothing');
fails($n1, q{CREATE TABLE ra.c9 AS SELECT id, 1/0 AS z FROM public.src}, qr/division by zero/, 'a query that fails');
same_everywhere(q{SELECT to_regclass('ra.c9') IS NULL}, 't', '... the table of the failed statement is not left on any node');
q1($n1, q{CREATE TEMP TABLE tmpx AS SELECT 1 AS x});
is(q1($n1, q{CREATE TEMP TABLE tmpx2 AS SELECT 1 AS x; SELECT count(*) FROM tmpx2}), 1, 'a temporary table is not affected');

# ---------------------------------------------------------------------------
# D. prepared statements, atomicity
# ---------------------------------------------------------------------------
{
	q1($n2, q{SET kingres_shard.enable_routing = off; TRUNCATE ra.t});
	my ($rc, $out, $err) = run_sql($n1, q{
		SET plan_cache_mode = force_generic_plan;
		PREPARE pr (int) AS INSERT INTO ra.t (id, v) SELECT id, tag FROM public.loc WHERE id > $1 AND id <= $1 + 3;
		EXECUTE pr(0); EXECUTE pr(3); EXECUTE pr(6); EXECUTE pr(9); EXECUTE pr(12); EXECUTE pr(15);
	});
	is($rc, 0, 'a prepared INSERT ... SELECT with a generic plan') or diag($err);
	is(owner_count(q{SELECT count(*) FROM ra.t}), 18, '... every execution reached the owner');
}
{
	my ($rc, $out, $err) = run_sql($n1, q{BEGIN; INSERT INTO ra.t (id, v) SELECT id + 5000, tag FROM public.loc; ROLLBACK});
	is($rc, 0, 'an INSERT ... SELECT in a transaction that rolls back') or diag($err);
	is(owner_count(q{SELECT count(*) FROM ra.t WHERE id > 5000}), 0, '... leaves nothing on the owner');
}
is(prepared_count($_), 0, $_->name . ': no prepared transaction is left') for @nodes;
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster is quiet') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));

done_testing();
