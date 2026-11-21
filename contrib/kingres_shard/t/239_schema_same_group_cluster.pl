# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/239_schema_same_group_cluster.pl
#
# Phase 11 (design/shard-phase11.md section 6.3), increment 6: several distributed schemas on one shard group, on three nodes with logical
# replication.  A statement that names tables of schemas with the same owner is one statement on that owner (path S); a statement that
# names schemas of different owners is read through the distributed scan and its write is refused.
#
#   A. a closed statement over two schemas of one owner, from every node: reads, writes, joins, a CTE, EXPLAIN
#   B. schemas of different owners: a read works, a write is refused with the reason
#   C. a prepared statement over two schemas is replanned after one of them moves away (the same session), and again after it comes back
#   D. a map that is stale on one node: KS001, for a statement over two schemas and for a statement over one
#   E. nothing is left prepared, check_cluster is quiet

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('sg', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

sub fails
{
	my ($n, $sql, $re, $name) = @_;
	my ($rc, $out, $err) = run_sql($n, $sql);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "... message: $name") or diag($err);
}

q1($n1, q{SELECT kingres.set_sharding_mode('both')});
q1($n1, q{CREATE SCHEMA sa; CREATE SCHEMA sb; CREATE SCHEMA sc});
q1($n1, q{SELECT kingres.distribute_schema('sa', 1); SELECT kingres.distribute_schema('sb', 1); SELECT kingres.distribute_schema('sc', 2)});
q1($n1, q{
	CREATE TABLE sa.t (id int PRIMARY KEY, v text);
	CREATE TABLE sb.t (id int PRIMARY KEY, w text);
	CREATE TABLE sc.t (id int PRIMARY KEY, x text);
	INSERT INTO sa.t SELECT g, 'a' || g FROM generate_series(1, 50) g;
	INSERT INTO sb.t SELECT g, 'b' || g FROM generate_series(1, 40) g;
	INSERT INTO sc.t SELECT g, 'c' || g FROM generate_series(1, 30) g;
});
is(q1($n3, q{SELECT string_agg(schema_name || ':' || group_id, ',' ORDER BY schema_name) FROM kingres.schemas}), 'sa:1,sb:1,sc:2', 'two schemas on group 1, one on group 2');

# ---------------------------------------------------------------------------
# A. one owner
# ---------------------------------------------------------------------------
my $join = q{SELECT count(*) || ':' || sum(a.id) FROM sa.t a JOIN sb.t b ON b.id = a.id WHERE a.id > 5};
for my $n (@nodes)
{
	is(q1($n, $join), '35:805', $n->name . ': a join of two schemas of one owner');
}
like(q1($n1, qq{EXPLAIN (COSTS off) $join}), qr/Kingres Remote Statement/, 'EXPLAIN on a node that holds neither: the statement is shipped whole');
unlike(q1($n2, qq{EXPLAIN (COSTS off) $join}), qr/Kingres/, 'EXPLAIN on the owner: the planner of PostgreSQL alone');
for my $n (@nodes)
{
	is(q1($n, q{WITH x AS (SELECT id FROM sb.t WHERE id < 10) SELECT count(*) FROM sa.t WHERE id IN (SELECT id FROM x)}), 9, $n->name . ': a CTE over two schemas');
}
q1($n3, q{INSERT INTO sb.t SELECT id + 1000, v FROM sa.t WHERE id <= 10});
is(q1($n2, q{SELECT count(*) FROM sb.t WHERE id > 1000}), 10, 'INSERT ... SELECT from one schema to another of the same owner, from a node that holds neither');
q1($n1, q{DELETE FROM sb.t b USING sa.t a WHERE a.id = b.id - 1000});
is(q1($n2, q{SELECT count(*) FROM sb.t WHERE id > 1000}), 0, 'DELETE ... USING a table of another schema of the same owner');
q1($n3, q{UPDATE sa.t a SET v = b.w FROM sb.t b WHERE a.id = b.id AND a.id <= 3});
is(q1($n1, q{SELECT string_agg(v, ',' ORDER BY id) FROM sa.t WHERE id <= 3}), 'b1,b2,b3', 'UPDATE ... FROM a table of another schema');
q1($n1, q{UPDATE sa.t SET v = 'a' || id WHERE id <= 3});
# a transaction that writes both schemas is one transaction on the owner: no two-phase commit
{
	my ($rc, $out, $err) = run_sql($n3, q{BEGIN; INSERT INTO sa.t VALUES (2001, 'x'); INSERT INTO sb.t VALUES (2001, 'y'); COMMIT});
	is($rc, 0, 'a transaction that writes two schemas of one owner commits') or diag($err);
	is(q1($n2, q{SELECT (SELECT count(*) FROM sa.t WHERE id = 2001) + (SELECT count(*) FROM sb.t WHERE id = 2001)}), 2, '... both rows are there');
	is(prepared_count($_), 0, $_->name . ': nothing is prepared') for @nodes;
}
q1($n1, q{DELETE FROM sa.t WHERE id = 2001; DELETE FROM sb.t WHERE id = 2001});

# ---------------------------------------------------------------------------
# B. different owners
# ---------------------------------------------------------------------------
my $cross = q{SELECT count(*) || ':' || sum(a.id) FROM sa.t a JOIN sc.t c ON c.id = a.id};
for my $n (@nodes)
{
	is(q1($n, $cross), '30:465', $n->name . ': a read over two schemas of different owners');
}
fails($n1, q{INSERT INTO sa.t SELECT id + 3000, x FROM sc.t}, qr/span|more than one|not supported/, 'a write that reads a schema of another owner');
fails($n3, q{UPDATE sa.t a SET v = c.x FROM sc.t c WHERE a.id = c.id}, qr/span|more than one|not supported/, 'an UPDATE that reads a schema of another owner');
is(q1($n2, q{SELECT count(*) FROM sa.t WHERE id > 3000}), 0, '... nothing was written');

# ---------------------------------------------------------------------------
# C. a prepared statement and a move, in one session
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, q{
		SET kingres_shard.node_timeout = 20;
		PREPARE pj (int) AS SELECT count(*) || ':' || sum(a.id) FROM sa.t a JOIN sb.t b ON b.id = a.id WHERE a.id > $1;
		EXECUTE pj(5); EXECUTE pj(5); EXECUTE pj(5); EXECUTE pj(5); EXECUTE pj(5); EXECUTE pj(5);
		SELECT kingres.move_schema('sb', 2) > 0;
		EXECUTE pj(5);
		EXECUTE pj(5);
		SELECT kingres.cleanup_moved_buckets(true);
		SELECT kingres.move_schema('sb', 1) > 0;
		EXECUTE pj(5);
	});
	is($rc, 0, 'a prepared statement over two schemas runs across two moves') or diag($err);
	my @lines = split /\n/, $out;
	is(scalar(@lines), 12, '... twelve answers') or diag($out);
	is(join('|', @lines[0 .. 5]), join('|', ('35:805') x 6), '... the same answer before the move');
	is(join('|', @lines[7 .. 8]), '35:805|35:805', '... and after sb went to another group (the plan was made again: a scan over two owners)');
	is($lines[11], '35:805', '... and after it came back');
}
is(q1($n1, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'sb'}), 1, 'sb is on group 1 again');
is(q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)}), 1, 'the old copy of the second move is emptied');
like(q1($n1, qq{EXPLAIN (COSTS off) $join}), qr/Kingres Remote Statement/, 'the join over the two schemas is shipped whole again');
{
	my ($rc, $out, $err) = run_sql($n3, q{
		SET plan_cache_mode = force_generic_plan;
		PREPARE pg (int) AS SELECT count(*) FROM sa.t a JOIN sb.t b ON b.id = a.id WHERE a.id > $1;
		EXECUTE pg(1); EXECUTE pg(2); EXECUTE pg(3);
	});
	is($rc, 0, 'a generic plan over two schemas, from a node that holds neither') or diag($err);
	is($out, "39\n38\n37", '... answers');
}

# ---------------------------------------------------------------------------
# D. a stale map
# ---------------------------------------------------------------------------
{
	my $ca = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'sa'::regnamespace});
	my $cb = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'sb'::regnamespace});
	my $g3 = q1($n1, q{SELECT group_id FROM kingres.node WHERE node_id = 3});
	my $g2 = q1($n1, q{SELECT group_id FROM kingres.node WHERE node_id = 2});
	# node 1 believes that sb is on node 3: the statement over sa and sb is a scan, and node 3 is asked for a table it does not hold
	q1($n1, "SELECT kingres.internal_set_schema_group($cb, $g3, 'active')");
	my ($rc, $out, $err) = run_sql($n1, qq{\\set VERBOSITY sqlstate\n$join});
	isnt($rc, 0, 'node 1 asks node 3 for a schema it does not hold: no answer') or diag("out=$out");
	like($err, qr/KS001/, '... the retryable error') or diag($err);
	# node 1 believes that both are on node 3: the whole statement goes to node 3
	q1($n1, "SELECT kingres.internal_set_schema_group($ca, $g3, 'active')");
	($rc, $out, $err) = run_sql($n1, qq{\\set VERBOSITY sqlstate\n$join});
	isnt($rc, 0, 'node 1 sends the whole statement to node 3 that holds neither: no answer');
	like($err, qr/KS001/, '... the retryable error') or diag($err);
	($rc, $out, $err) = run_sql($n1, q{\set VERBOSITY sqlstate
		PREPARE ps (int) AS SELECT count(*) FROM sa.t a JOIN sb.t b ON b.id = a.id WHERE a.id > $1; EXECUTE ps(1)});
	like($err, qr/KS001/, 'a prepared statement over the two schemas meets the same error') or diag($err);
	q1($n1, "SELECT kingres.internal_set_schema_group($ca, $g2, 'active')");
	q1($n1, "SELECT kingres.internal_set_schema_group($cb, $g2, 'active')");
	is(q1($n1, $join), '35:805', 'with the map the same again the statement answers');
}

# ---------------------------------------------------------------------------
# E. nothing left
# ---------------------------------------------------------------------------
is(prepared_count($_), 0, $_->name . ': no prepared transaction is left') for @nodes;
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster is quiet') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));
for my $n (@nodes)
{
	is(q1($n, q{SELECT string_agg(schema_name || ':' || group_id, ',' ORDER BY schema_name) FROM kingres.schemas}), 'sa:1,sb:1,sc:2', $n->name . ': the same catalog');
}

done_testing();
