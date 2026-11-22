# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/231_schema_scan_cluster.pl
#
# Phase 11 (design/shard-phase11.md), increment 2, on three nodes: the tables of a distributed schema that another
# shard group holds (the "shells") are read through the distributed scan in its single-source form, never as the
# empty local copy (6.6, 6.8).  The schemas are registered by hand with the node-local functions of increment 1; the
# shell on every node that does not own a schema holds junk rows, so that a plan that reads the local copy shows.
#
#   A. reads of a shell from both other nodes and from the owner: counts, aggregates, joins of two schemas of two
#      owners, joins with a local table and with a hash-distributed table, subqueries, CTEs, windows, parameters,
#      prepared statements, EXPLAIN (a scan with one remote group on a shell, none on the owner)
#   B. writes: refused on a shell, local on the owner; a read of a shell into a local table
#   C. COPY: the relation form is refused on a shell and the query form works
#   D. the end-of-planning check: a plan that reads a shell (a SQL function expanded by the planner, with the scan
#      switched off) is an error and never an empty answer
#   E. a map that differs between the nodes ends the statement with the retryable error, not with an answer
#   F. the complete scan stays the fallback: a statement that no other path takes is answered by the scan

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('ss', 3);
my ($n1, $n2, $n3) = @nodes;

# This test is about the single-source scan, which answers everything that path S (t/235) does not take: path S is off in every
# session, so that every statement below is planned with the scan (the comparison of the two paths is in t/235).
for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.enable_schema_routing = off\n");
	$n->reload;
}

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

# the structure of both schemas on every node, the rows of ten on node 2 (group 1) and of ten3 on node 3 (group 2),
# junk rows in every shell
for my $n (@nodes)
{
	q1($n, q{
		CREATE SCHEMA ten;
		CREATE TABLE ten.a (id int PRIMARY KEY, v text);
		CREATE TABLE ten.b (id int PRIMARY KEY, a_id int);
		CREATE SCHEMA ten3;
		CREATE TABLE ten3.c (id int PRIMARY KEY, w int);
		CREATE TABLE public.loc (id int PRIMARY KEY, tag text);
		INSERT INTO public.loc SELECT g, 'loc ' || g FROM generate_series(1, 5) g;
	});
}
q1($n2, q{INSERT INTO ten.a SELECT g, 'row ' || g FROM generate_series(1, 50) g;
          INSERT INTO ten.b SELECT g, ((g - 1) % 50) + 1 FROM generate_series(1, 100) g;});
q1($n3, q{INSERT INTO ten3.c SELECT g, g * 10 FROM generate_series(1, 10) g});
for my $n ($n1, $n3) { q1($n, q{INSERT INTO ten.a VALUES (1000, 'junk'); INSERT INTO ten.b VALUES (1000, 1000)}); }
for my $n ($n1, $n2) { q1($n, q{INSERT INTO ten3.c VALUES (1000, 1000)}); }

# registration, the node-local functions of increment 1 on the map primary, then the push
my $g2 = q1($n1, q{SELECT group_id FROM kingres.node WHERE node_id = 2});
my $g3 = q1($n1, q{SELECT group_id FROM kingres.node WHERE node_id = 3});
my $c1 = q1($n1, q{SELECT nextval('kingres.colocation_id_seq')});
my $c2 = q1($n1, q{SELECT nextval('kingres.colocation_id_seq')});
q1($n1, "SELECT kingres.internal_register_schema('ten', $c1, $g2)");
q1($n1, q{SELECT kingres.internal_register_schema_table('ten.a'), kingres.internal_register_schema_table('ten.b')});
q1($n1, "SELECT kingres.internal_register_schema('ten3', $c2, $g3)");
q1($n1, q{SELECT kingres.internal_register_schema_table('ten3.c')});
q1($n1, 'SELECT kingres.sync_catalog()');
isnt($g2, $g3, 'the two schemas have two owners');

# no hash-distributed table exists yet: the schema tables alone make the planner look at the statement
for my $n (@nodes)
{
	is(q1($n, q{SELECT count(*) FROM ten.a}), 50, $n->name . ': with no hash-distributed table in the cluster, a shell is read from the owner');
	is(q1($n, q{SELECT count(*) FROM ten3.c}), 10, $n->name . ': ... and the other schema too');
}

q1($n1, q{
	CREATE TABLE public.h (id int PRIMARY KEY, v int);
	SELECT kingres.create_distributed_table('public.h', 'id', bucket_count => 6);
	INSERT INTO public.h SELECT g, g FROM generate_series(1, 20) g;
});


# ---------------------------------------------------------------------------
# A. reads
# ---------------------------------------------------------------------------
my @reads = (
	[ 'count', q{SELECT count(*) FROM ten.a}, 50 ],
	[ 'sum and max', q{SELECT sum(id) || '|' || max(v) FROM ten.a}, '1275|row 9' ],
	[ 'a restriction', q{SELECT v FROM ten.a WHERE id = 7}, 'row 7' ],
	[ 'a range, ordered, limited', q{SELECT string_agg(id::text, ',' ORDER BY id) FROM (SELECT id FROM ten.a WHERE id > 45 ORDER BY id LIMIT 3) s}, '46,47,48' ],
	[ 'join of two tables of one schema', q{SELECT count(*) || '|' || sum(a.id) FROM ten.a a JOIN ten.b b ON b.a_id = a.id}, '100|2550' ],
	[ 'join of two schemas with two owners', q{SELECT count(*) || '|' || sum(c.w) FROM ten.a a JOIN ten3.c c ON c.id = a.id}, '10|550' ],
	[ 'join with a local table', q{SELECT count(*) || '|' || min(l.tag) FROM ten.a a JOIN public.loc l ON l.id = a.id}, '5|loc 1' ],
	[ 'join with a hash-distributed table', q{SELECT count(*) || '|' || sum(h.v) FROM ten.a a JOIN public.h h ON h.id = a.id}, '20|210' ],
	[ 'three kinds in one statement', q{SELECT count(*) FROM ten.a a JOIN public.h h ON h.id = a.id JOIN public.loc l ON l.id = a.id JOIN ten3.c c ON c.id = a.id}, 5 ],
	[ 'a subquery', q{SELECT count(*) FROM public.loc l WHERE l.id IN (SELECT id FROM ten.a WHERE id < 4)}, 3 ],
	[ 'a correlated subquery', q{SELECT count(*) FROM public.loc l WHERE EXISTS (SELECT 1 FROM ten.a a WHERE a.id = l.id AND a.v = 'row ' || l.id)}, 5 ],
	[ 'a CTE', q{WITH x AS (SELECT id FROM ten.a WHERE id <= 10) SELECT count(*) FROM x JOIN ten3.c c ON c.id = x.id}, 10 ],
	[ 'a window function', q{SELECT max(rn) FROM (SELECT row_number() OVER (ORDER BY id) rn FROM ten.b) s}, 100 ],
	[ 'grouping', q{SELECT count(*) FROM (SELECT a_id FROM ten.b GROUP BY a_id HAVING count(*) = 2) s}, 50 ],
	[ 'a set operation', q{SELECT count(*) FROM (SELECT id FROM ten.a UNION SELECT id FROM ten3.c) s}, 50 ],
	[ 'DISTINCT', q{SELECT count(DISTINCT a_id) FROM ten.b}, 50 ],
	[ 'no row at all', q{SELECT count(*) FROM ten.a WHERE id > 1000}, 0 ],
	[ 'a constant false restriction', q{SELECT count(*) FROM ten.a WHERE false}, 0 ],
	[ 'the junk of the shell is not an answer', q{SELECT count(*) FROM ten.a WHERE id = 1000}, 0 ],
);
for my $n (@nodes)
{
	my $nm = $n->name;
	for my $r (@reads)
	{
		is(q1($n, $r->[1]), $r->[2], "$nm: $r->[0]");
	}
}

# parameters and prepared statements, with enough executions for a generic plan
for my $n (@nodes)
{
	my $nm = $n->name;
	my ($rc, $out, $err) = run_sql($n, q{
		PREPARE pa (int) AS SELECT v FROM ten.a WHERE id = $1;
		PREPARE pj (int) AS SELECT count(*) FROM ten.b b JOIN ten.a a ON a.id = b.a_id WHERE b.id <= $1;
		EXECUTE pa(3); EXECUTE pa(4); EXECUTE pa(5); EXECUTE pa(6); EXECUTE pa(7); EXECUTE pa(8); EXECUTE pa(9);
		EXECUTE pj(10); EXECUTE pj(20); EXECUTE pj(30); EXECUTE pj(40); EXECUTE pj(50); EXECUTE pj(60); EXECUTE pj(70);
	});
	is($rc, 0, "$nm: prepared statements on a schema table run") or diag($err);
	is(join(',', split(/\n/, $out)), 'row 3,row 4,row 5,row 6,row 7,row 8,row 9,10,20,30,40,50,60,70', "$nm: ... and answer from the owner (five generic executions)");
}
{
	my ($rc, $out, $err) = run_sql($n1, q{
		DO $$ DECLARE r int; BEGIN
			FOR i IN 1 .. 12 LOOP
				SELECT count(*) INTO r FROM ten.a WHERE id <= i;
				IF r <> i THEN RAISE EXCEPTION 'plpgsql loop: % rows for %', r, i; END IF;
			END LOOP;
		END $$;
	});
	is($rc, 0, 'a PL/pgSQL loop on a shell reads the owner (generic plans of the cache)') or diag($err);
}

# EXPLAIN: a scan with one remote group on a shell, no scan on the owner
{
	my $plan = q1($n1, q{EXPLAIN (COSTS off) SELECT * FROM ten.a WHERE id = 3});
	like($plan, qr/Kingres Distributed Scan/, 'EXPLAIN on a shell: the distributed scan');
	like($plan, qr/Remote groups: 1\b/, '... with one remote group');
	like($plan, qr/Remote SQL: SELECT .* FROM ten\.a AS ks_t WHERE/, '... and the fragment is a plain select of the table with its restriction');
	unlike($plan, qr/Seq Scan|Index Scan|Bitmap/, '... and no scan of the local copy');
	my $own = q1($n2, q{EXPLAIN (COSTS off) SELECT * FROM ten.a WHERE id = 3});
	unlike($own, qr/Kingres/, 'EXPLAIN on the owner: the planner of PostgreSQL alone');
	like($own, qr/(Seq|Index) Scan.* on a\b/, '... on its own table');
}

# ---------------------------------------------------------------------------
# B. writes
# ---------------------------------------------------------------------------
for my $n ($n1, $n3)
{
	my $nm = $n->name;
	for my $w (
		[ 'INSERT', q{INSERT INTO ten.a VALUES (2000, 'x')} ],
		[ 'UPDATE', q{UPDATE ten.a SET v = 'x' WHERE id = 1} ],
		[ 'DELETE', q{DELETE FROM ten.a WHERE id = 1} ],
		[ 'INSERT ... SELECT', q{INSERT INTO ten.a SELECT id + 3000, v FROM ten.a} ],
		[ 'a data-modifying CTE', q{WITH d AS (DELETE FROM ten.a WHERE id = 2 RETURNING id) SELECT * FROM d} ],
		[ 'MERGE', q{MERGE INTO ten.a t USING public.loc l ON t.id = l.id WHEN MATCHED THEN DELETE} ],
	  )
	{
		my ($rc, $out, $err) = run_sql($n, $w->[1]);
		isnt($rc, 0, "$nm: $w->[0] into a shell is refused");
		like($err, qr/a write to table "ten\.a" of a distributed schema, which shard group $g2 holds, is not supported from shard group \d+ yet/,
			"$nm: $w->[0]: the message names the table and the group") or diag($err);
	}
}
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT count(*) FROM ten.a FOR UPDATE});
	isnt($rc, 0, 'FOR UPDATE on a shell is refused');
	($rc, $out, $err) = run_sql($n1, q{UPDATE public.loc l SET tag = a.v FROM ten.a a WHERE a.id = l.id});
	isnt($rc, 0, 'UPDATE of a local table that reads a shell in FROM is refused (the scan has no row identifier)');
	like($err, qr/UPDATE, DELETE and MERGE that read distributed table "a"/, '... with the message of the scan') or diag($err);
}
is(q1($n2, q{SELECT count(*) FROM ten.a}), 50, 'the refused writes changed nothing on the owner');
is(q1($n1, q{SELECT count(*) FROM ten.a WHERE id < 1000}), 50, '... as the other nodes see it');

# a read of a shell into a local table is a read
q1($n1, q{CREATE TABLE public.copy1 AS SELECT * FROM ten.a});
is(q1($n1, q{SELECT count(*) FROM public.copy1}), 50, 'CREATE TABLE AS from a shell copies the rows of the owner');
q1($n1, q{INSERT INTO public.copy1 SELECT id + 100, v FROM ten.a});
is(q1($n1, q{SELECT count(*) FROM public.copy1}), 100, 'INSERT ... SELECT from a shell into a local table');
{
	# the limit of the scan of hash tables too: a DELETE of one table that reads another through a semi join needs its row identifier
	my ($rc, $out, $err) = run_sql($n1, q{DELETE FROM public.copy1 WHERE id IN (SELECT id FROM ten.a)});
	isnt($rc, 0, 'DELETE of a local table with a subquery on a shell is refused as it is for a hash table');
	like($err, qr/UPDATE, DELETE and MERGE that read distributed table "a"/, '... with the message of the scan') or diag($err);
	q1($n1, q{DELETE FROM public.copy1 WHERE id > 100});
	is(q1($n1, q{SELECT count(*) FROM public.copy1}), 50, '... and a DELETE without it works');
}

# on the owner a write is the plain write of PostgreSQL, and every node sees it
q1($n2, q{INSERT INTO ten.a VALUES (51, 'row 51'); UPDATE ten.a SET v = 'row 51 changed' WHERE id = 51});
for my $n (@nodes)
{
	is(q1($n, q{SELECT v FROM ten.a WHERE id = 51}), 'row 51 changed', $n->name . ': a write on the owner is read from every node');
}
q1($n2, q{DELETE FROM ten.a WHERE id = 51});
is(q1($n1, q{SELECT count(*) FROM ten.a}), 50, 'and its delete too');

# ---------------------------------------------------------------------------
# C. COPY
# ---------------------------------------------------------------------------
for my $n ($n1, $n3)
{
	my $nm = $n->name;
	my ($rc, $out, $err) = run_sql($n, q{COPY ten.a TO STDOUT});
	isnt($rc, 0, "$nm: COPY of the shell TO is refused");
	like($err, qr/COPY "a" TO would read the empty copy of the table on this node; shard group $g2 holds the rows/, "$nm: ... with the reason") or diag($err);
	like($err, qr/Use COPY \(SELECT \* FROM ten\.a\) TO/, "$nm: ... and the hint names the form that works");
	($rc, $out, $err) = run_sql($n, q{COPY ten.a (id, v) FROM PROGRAM 'printf "9001\tcp1\n9002\tcp2\n"'});
	is($rc, 0, "$nm: COPY FROM into the shell loads the rows") or diag($err);
	is(q1($n2, q{SELECT count(*) FROM ten.a WHERE id IN (9001, 9002)}), 2, "$nm: ... on the owner");
	q1($n2, q{DELETE FROM ten.a WHERE id IN (9001, 9002)});
	($rc, $out, $err) = run_sql($n, q{COPY (SELECT id, v FROM ten.a WHERE id <= 3 ORDER BY id) TO STDOUT});
	is($rc, 0, "$nm: COPY (SELECT ...) TO works") or diag($err);
	is($out, "1\trow 1\n2\trow 2\n3\trow 3", "$nm: ... with the rows of the owner");
}
{
	my ($rc, $out, $err) = run_sql($n2, q{COPY (SELECT count(*) FROM ten.a) TO STDOUT});
	is($out, "50", 'the owner copies its own table');
	($rc, $out, $err) = run_sql($n2, q{COPY ten.a TO STDOUT});
	is($rc, 0, 'COPY TO of the relation is plain on the owner') or diag($err);
	is(scalar(() = $out =~ /\n/g), 49, '... and gives its rows (50 lines)');
}

# ---------------------------------------------------------------------------
# D. the end-of-planning check
# ---------------------------------------------------------------------------
for my $n (@nodes)
{
	q1($n, q{CREATE FUNCTION public.fa() RETURNS SETOF ten.a LANGUAGE sql STABLE AS 'SELECT * FROM ten.a'});
}
is(q1($n1, q{SELECT count(*) FROM public.fa()}), 50, 'a SQL function that the planner expands into the statement reads the owner (the scan finds the table)');
is(q1($n2, q{SELECT count(*) FROM public.fa()}), 50, '... and on the owner');
{
	my ($rc, $out, $err) = run_sql($n1, qq{\\set VERBOSITY sqlstate\nSET kingres_shard.enable_distributed_scan = off;\nSELECT count(*) FROM public.fa()});
	isnt($rc, 0, 'with the scan off, a plan that would read the empty copy is an error');
	like($err, qr/XX000/, '... with the SQLSTATE of an internal error') or diag($err);
	($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.enable_distributed_scan = off; SELECT count(*) FROM public.fa()});
	like($err, qr/internal error: a plan reads the local copy of "ten\.a", which this node does not own/, '... and the message says so') or diag($err);
	($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.enable_distributed_scan = off; SELECT count(*) FROM ten.a});
	isnt($rc, 0, 'with the scan off, a direct read of a shell is refused');
	like($err, qr/needs the distributed scan/, '... and says what is needed') or diag($err);
	($rc, $out, $err) = run_sql($n2, q{SET kingres_shard.enable_distributed_scan = off; SELECT count(*) FROM ten.a});
	is($rc, 0, 'the owner does not need the scan') or diag($err);
	is($out, "50", '... and answers');
}

# ---------------------------------------------------------------------------
# E. a map that differs between the nodes
# ---------------------------------------------------------------------------
q1($n1, "SELECT kingres.internal_set_schema_group($c1, $g3, 'active')");
{
	my ($rc, $out, $err) = run_sql($n1, qq{\\set VERBOSITY sqlstate\nSELECT count(*) FROM ten.a});
	isnt($rc, 0, 'node 1 believes node 3 holds the schema, node 3 does not: no answer');
	like($err, qr/KS001/, '... the error is the retryable one of a stale map') or diag($err);
	($rc, $out, $err) = run_sql($n1, q{SELECT count(*) FROM ten.a});
	like($err, qr/the bucket map changed while the statement was starting/, '... and its message says the maps differ') or diag($err);
}
q1($n1, "SELECT kingres.internal_set_schema_group($c1, $g2, 'active')");
is(q1($n1, q{SELECT count(*) FROM ten.a}), 50, 'with the map the same again, the answer is back');

# ---------------------------------------------------------------------------
# E2. a session that another node opened (6.8, item 2): the statement of a node whose map is not this one's.  The
# session is made by hand with the token of the cluster secret, as t/019 does; on the owner the statement runs, on a node
# that does not hold the table it is the retryable error and never an answer from the empty copy.
# ---------------------------------------------------------------------------
{
	my $role = q1($n1, 'SELECT current_user');
	my %id = (ss1 => 1, ss2 => 2, ss3 => 3);
	for my $n (@nodes)
	{
		my $tok = q1($n, "SELECT kingres.internal_forward_token('$role', $id{$n->name})");
		local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
		my ($rc, $out, $err) = run_sql($n, qq{\\set VERBOSITY sqlstate\nSELECT count(*) FROM ten.a});
		if ($n == $n2)
		{
			is($out, '50', 'a forwarded statement on the owner runs and answers from its table') or diag($err);
		}
		else
		{
			isnt($rc, 0, $n->name . ': a forwarded statement on a node that does not hold the table fails');
			like($err, qr/KS001/, $n->name . ': ... with the retryable error of a stale map') or diag($err);
			($rc, $out, $err) = run_sql($n, q{SELECT count(*) FROM ten.a});
			like($err, qr/this node does not hold table "ten\.a" of a distributed schema/, $n->name . ': ... and the message says the node does not hold the table') or diag($err);
			unlike($out, qr/^\d+$/, $n->name . ': ... and no count came back (the junk of the shell is not an answer)');
		}
	}
}

# ---------------------------------------------------------------------------
# F. the same statements with the scan as the only path (the complete scan is the fallback)
# ---------------------------------------------------------------------------
for my $n (@nodes)
{
	my ($rc, $out, $err) = run_sql($n, q{SET kingres_shard.scan_fetch_size = 7; SELECT count(*) || '|' || sum(b.id) FROM ten.b b JOIN ten.a a ON a.id = b.a_id});
	is($out, "100|5050", $n->name . ': a small fetch size (many batches) gives the same answer') or diag($err);
}
for my $n (@nodes) { is(prepared_count($n), 0, 'nothing is prepared on ' . $n->name); }

done_testing();
