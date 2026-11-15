# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The distributed scan (design/shard-phase2a.md, section 5) and the main
# correctness guard of phase 2a: the same data is loaded into a three-node
# cluster and into one ordinary node, and every query of the list below must
# give the same result on each node of the cluster as on the single node.
# KingresShardCompare.pm is the harness; add queries to the list freely.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql prepared_count keys_of_group);
use KingresShardCompare;

my $c = KingresShardCompare->new('ds', extra => "log_statement = 'all'\n");
my @nodes = $c->nodes;

# ---------------------------------------------------------------------------
# Schema and data, identical in the cluster and on the single node.
# ---------------------------------------------------------------------------
my $ddl = q{
	CREATE TABLE public.t1 (id int PRIMARY KEY, g int, v text, n numeric(10,2));
	CREATE TABLE public.t2 (id int PRIMARY KEY, a int, b text);
	CREATE TABLE public.t3 (k int PRIMARY KEY, grp int, w text);
	CREATE TABLE public.refx (id int PRIMARY KEY, name text);
	CREATE TABLE public.big (id int PRIMARY KEY, pad text);
};
$c->on_both($ddl);
$c->on_cluster(q{
	SELECT kingres.create_distributed_table('public.t1', 'id', bucket_count => 12);
	SELECT kingres.create_distributed_table('public.t2', 'id', colocate_with => 'public.t1');
	SELECT kingres.create_distributed_table('public.t3', 'k', bucket_count => 8);
	SELECT kingres.create_distributed_table('public.big', 'id', bucket_count => 12);
	SELECT kingres.create_reference_table('public.refx');
});
# A table and a function that exist on every node, not distributed.
my $local = q{
	CREATE TABLE public.loc (id int, x text);
	INSERT INTO public.loc VALUES (0, 'x0'), (1, 'x1'), (2, 'x2'), (3, 'x3'), (4, 'x4'), (5, 'x5'), (6, 'x6');
	CREATE FUNCTION public.plus_one(x int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT x + 1';
};
$c->on_every_node($local);
$c->on_single($local);

sub rows_sql
{
	my ($table, @rows) = @_;
	return join('', map { "INSERT INTO public.$table VALUES ($_);\n" } @rows);
}

my @t1 = map { sprintf("%d, %d, %s, %.2f", $_, $_ % 7, ($_ % 11 == 0 ? 'NULL' : "'v" . ($_ % 13) . "'"), $_ * 1.25) } (1 .. 240);
my @t2 = map { sprintf("%d, %d, %s", $_, $_ % 5, ($_ % 9 == 0 ? 'NULL' : "'b" . ($_ % 9) . "'")) } (1 .. 120);
my @t3 = map { sprintf("%d, %d, 'w%d'", $_, $_ % 4, $_) } (1 .. 60);
my @refx = map { sprintf("%d, 'n%d'", $_, $_) } (0 .. 6);
$c->script_on_both(rows_sql('t1', @t1) . rows_sql('t2', @t2) . rows_sql('t3', @t3) . rows_sql('refx', @refx));

is($c->on_cluster('SELECT count(*) FROM public.t1 WHERE id = 7'), '1', 'rows are routed to their groups');
is($c->on_single('SELECT count(*) FROM public.t1'), '240', 'the single node holds the data');
is(join(',', map { $_->safe_psql('postgres', "SELECT count(*) FROM pg_class WHERE relname ~ '^t1_b[0-9]{4}\$' AND relkind = 'r'") } @nodes),
	'4,4,4', 'the cluster has the data in 4 + 4 + 4 buckets');

# ---------------------------------------------------------------------------
# The queries.  [ name, sql, ordered ]
# ---------------------------------------------------------------------------
my @queries = (
	[ 'all rows',                       'SELECT * FROM public.t1', 0 ],
	[ 'filter without a key',           'SELECT id, v FROM public.t1 WHERE g = 3', 0 ],
	[ 'filter on text',                 q{SELECT id FROM public.t1 WHERE v = 'v5' AND g > 1}, 0 ],
	[ 'filter with a function',         q{SELECT id FROM public.t1 WHERE lower(v) LIKE 'v1%'}, 0 ],
	[ 'filter with a range',            'SELECT id FROM public.t1 WHERE n BETWEEN 10 AND 100', 0 ],
	[ 'filter with arithmetic',         'SELECT id FROM public.t1 WHERE id % 7 = 3', 0 ],
	[ 'filter with IN',                 'SELECT id FROM public.t1 WHERE g IN (1, 2, 3) AND id < 100', 0 ],
	[ 'IS NULL',                        'SELECT id FROM public.t1 WHERE v IS NULL', 0 ],
	[ 'COALESCE and CASE',              q{SELECT id, COALESCE(v, 'none'), CASE WHEN g > 3 THEN 'hi' ELSE 'lo' END FROM public.t1 WHERE id <= 30}, 0 ],
	[ 'a restriction that is not shipped', 'SELECT id FROM public.t1 WHERE public.plus_one(g) = 4', 0 ],
	[ 'key or non-key',                 'SELECT id FROM public.t1 WHERE id = 5 OR g = 2', 0 ],
	[ 'key in a range',                 'SELECT count(*) FROM public.t1 WHERE id < 50', 0 ],
	[ 'count and sums',                 'SELECT count(*), count(v), sum(n), min(v), max(v), avg(g) FROM public.t1', 0 ],
	[ 'whole row',                      'SELECT t1 FROM public.t1 WHERE g = 1', 0 ],
	[ 'row_to_json',                    'SELECT row_to_json(t1) FROM public.t1 WHERE id % 40 = 1', 0 ],
	[ 'GROUP BY',                       'SELECT g, count(*), sum(n), avg(n) FROM public.t1 GROUP BY g ORDER BY g', 1 ],
	[ 'GROUP BY HAVING',                'SELECT v, count(*) FROM public.t1 GROUP BY v HAVING count(*) > 17 ORDER BY v', 1 ],
	[ 'DISTINCT',                       'SELECT DISTINCT g, v FROM public.t1 WHERE id < 100', 0 ],
	[ 'DISTINCT ON',                    'SELECT DISTINCT ON (g) g, id FROM public.t1 ORDER BY g, id', 1 ],
	[ 'ORDER BY LIMIT',                 'SELECT id FROM public.t1 ORDER BY v DESC NULLS LAST, id LIMIT 7 OFFSET 3', 1 ],
	[ 'ORDER BY expression',            'SELECT id, n FROM public.t1 ORDER BY (n * 3) % 17, id LIMIT 20', 1 ],
	[ 'top-N per group',                'SELECT g, max(id) FROM public.t1 GROUP BY g ORDER BY max(id) DESC LIMIT 3', 1 ],
	[ 'join on a non-key column',       'SELECT t1.id, t2.id FROM public.t1 JOIN public.t2 ON t1.g = t2.a WHERE t1.id < 30 AND t2.id < 30', 0 ],
	[ 'join on a non-key, aggregated',  'SELECT t2.b, count(*), sum(t1.n) FROM public.t1 JOIN public.t2 ON t1.g = t2.a GROUP BY t2.b ORDER BY t2.b NULLS LAST', 1 ],
	[ 'join on the key',                'SELECT t1.id, t2.b FROM public.t1 JOIN public.t2 ON t1.id = t2.id WHERE t2.a = 2', 0 ],
	[ 'join of different colocation groups', 'SELECT t1.id, t3.w FROM public.t1 JOIN public.t3 ON t1.id = t3.k WHERE t3.grp = 1', 0 ],
	[ 'join with an ordinary table',    'SELECT t1.id, loc.x FROM public.t1 JOIN public.loc ON loc.id = t1.g WHERE t1.id < 40', 0 ],
	[ 'join with a reference table',    'SELECT t1.id, refx.name FROM public.t1 JOIN public.refx ON refx.id = t1.g WHERE t1.id < 40', 0 ],
	[ 'three-way join',                 'SELECT t1.id, t2.b, refx.name FROM public.t1 JOIN public.t2 ON t2.id = t1.id JOIN public.refx ON refx.id = t2.a WHERE t1.id < 60', 0 ],
	[ 'left join',                      'SELECT t1.id, t2.b FROM public.t1 LEFT JOIN public.t2 ON t1.g = t2.a AND t2.id < 4 WHERE t1.id < 20', 0 ],
	[ 'right and full join',            'SELECT t2.id, t3.k FROM public.t2 FULL JOIN public.t3 ON t2.id = t3.k + 100 WHERE t2.id IS NULL OR t3.k IS NULL OR t2.id < 5', 0 ],
	[ 'self join',                      'SELECT a.id, b.id FROM public.t1 a JOIN public.t1 b ON a.g = b.id WHERE a.id < 30', 0 ],
	[ 'cross join with a small table',  'SELECT count(*) FROM public.t3 CROSS JOIN public.refx', 0 ],
	[ 'lateral',                        'SELECT t3.k, s.c FROM public.t3, LATERAL (SELECT count(*) AS c FROM public.t1 WHERE t1.g = t3.grp) s WHERE t3.k < 6', 0 ],
	[ 'subselect in WHERE',             q{SELECT id FROM public.t1 WHERE g IN (SELECT a FROM public.t2 WHERE b LIKE 'b1%')}, 0 ],
	[ 'NOT IN subselect',               'SELECT id FROM public.t1 WHERE g NOT IN (SELECT a FROM public.t2 WHERE id < 3) AND id < 50', 0 ],
	[ 'EXISTS',                         q{SELECT id FROM public.t1 WHERE EXISTS (SELECT 1 FROM public.t2 WHERE t2.a = t1.g AND t2.b = 'b3') AND id < 50}, 0 ],
	[ 'scalar subselect',               'SELECT id, (SELECT max(id) FROM public.t2 WHERE t2.a = t1.g) FROM public.t1 WHERE id < 25', 0 ],
	[ 'correlated subselect',           'SELECT id, (SELECT count(*) FROM public.t2 WHERE t2.a = t1.g) FROM public.t1 ORDER BY id LIMIT 30', 1 ],
	[ 'subselect in FROM',              'SELECT s.g, s.c FROM (SELECT g, count(*) c FROM public.t1 GROUP BY g) s WHERE s.c > 34 ORDER BY 1', 1 ],
	[ 'CTE',                            'WITH s AS (SELECT g, count(*) c FROM public.t1 GROUP BY g) SELECT * FROM s WHERE c > 2 ORDER BY g', 1 ],
	[ 'CTE used twice',                 'WITH s AS (SELECT * FROM public.t1 WHERE id < 40) SELECT count(*), (SELECT sum(n) FROM s) FROM s', 0 ],
	[ 'recursive CTE over a table',     'WITH RECURSIVE r(id, d) AS (SELECT k, 0 FROM public.t3 WHERE k = 1 UNION ALL SELECT t3.k, r.d + 1 FROM r JOIN public.t3 ON t3.k = r.id * 2 WHERE r.d < 4) SELECT * FROM r', 0 ],
	[ 'window function',                'SELECT id, row_number() OVER (PARTITION BY g ORDER BY id), sum(n) OVER (PARTITION BY g) FROM public.t1 WHERE id < 40 ORDER BY id', 1 ],
	[ 'rank and lag',                   'SELECT id, rank() OVER (ORDER BY g, id), lag(id) OVER (ORDER BY id) FROM public.t1 WHERE id < 30 ORDER BY id', 1 ],
	[ 'UNION',                          'SELECT g FROM public.t1 UNION SELECT a FROM public.t2 ORDER BY 1', 1 ],
	[ 'UNION ALL of distributed tables', 'SELECT count(*), sum(x) FROM (SELECT id AS x FROM public.t1 UNION ALL SELECT id FROM public.t2) u', 0 ],
	[ 'INTERSECT',                      'SELECT g FROM public.t1 INTERSECT SELECT a FROM public.t2 ORDER BY 1', 1 ],
	[ 'EXCEPT',                         'SELECT id FROM public.t1 WHERE id < 20 EXCEPT SELECT id FROM public.t2 WHERE id < 10 ORDER BY 1', 1 ],
	[ 'VALUES join',                    'SELECT t1.id, v.x FROM public.t1 JOIN (VALUES (1, 100), (9, 900), (200, 2000)) v(i, x) ON v.i = t1.id', 0 ],
	[ 'set-returning function join',    'SELECT t1.id FROM public.t1 JOIN generate_series(1, 240, 17) s ON s = t1.id', 0 ],
	[ 'aggregate with FILTER',          'SELECT count(*) FILTER (WHERE g = 2), count(*) FILTER (WHERE v IS NULL) FROM public.t1', 0 ],
	[ 'string_agg ordered',             q{SELECT string_agg(id::text, ',' ORDER BY id) FROM public.t1 WHERE g = 6 AND id < 100}, 0 ],
	[ 'array_agg',                      'SELECT array_agg(k ORDER BY k) FROM public.t3 WHERE grp = 2', 0 ],
	[ 'bool and bit aggregates',        'SELECT bool_and(g < 7), bool_or(id = 3), bit_or(g) FROM public.t1', 0 ],
	[ 'empty result',                   'SELECT * FROM public.t1 WHERE g = 99', 0 ],
	[ 'constant false',                 'SELECT * FROM public.t1 WHERE false', 0 ],
	[ 'count of an empty set',          'SELECT count(*), sum(n) FROM public.t1 WHERE id < 0', 0 ],
	[ 'view over a distributed table',  'SELECT count(*), sum(n) FROM public.v_t1', 0 ],
	[ 'table alias and column alias',   'SELECT x.id AS ident FROM public.t1 AS x WHERE x.g = 6 AND x.id < 40', 0 ],
	[ 'FILTER on parameters via a function', 'SELECT public.count_g(2)', 0 ],
);

$c->on_both('CREATE VIEW public.v_t1 AS SELECT id, g, n FROM public.t1 WHERE g < 5');
# The view exists on node 1 only: create it on the other nodes too.
$_->safe_psql('postgres', 'CREATE VIEW public.v_t1 AS SELECT id, g, n FROM public.t1 WHERE g < 5') for @nodes[ 1 .. 2 ];
my $fn = q{CREATE FUNCTION public.count_g(p int) RETURNS bigint LANGUAGE plpgsql AS $$ BEGIN RETURN (SELECT count(*) FROM public.t1 WHERE g = p); END $$};
$c->on_every_node($fn);
$c->on_single($fn);

$c->compare($_->[0], $_->[1], ordered => $_->[2]) for @queries;

# ---------------------------------------------------------------------------
# Prepared statements, run enough times for a generic plan to be considered.
# ---------------------------------------------------------------------------
{
	my $script = "PREPARE p1(int) AS SELECT count(*), sum(n) FROM public.t1 WHERE g = \$1;\n"
	  . join('', map { "EXECUTE p1(" . ($_ % 7) . ");\n" } (1 .. 9))
	  . "PREPARE p2(text, int) AS SELECT id FROM public.t1 WHERE v = \$1 AND id < \$2 ORDER BY id;\n"
	  . join('', map { "EXECUTE p2('v" . $_ . "', 60);\n" } (1 .. 8))
	  . "PREPARE p3(int) AS SELECT t1.id, t2.b FROM public.t1 JOIN public.t2 ON t1.g = t2.a WHERE t2.id = \$1 AND t1.id < 20 ORDER BY 1;\n"
	  . join('', map { "EXECUTE p3($_);\n" } (1 .. 8))
	  . "SET plan_cache_mode = force_generic_plan;\n"
	  . "PREPARE p4(int) AS SELECT count(*) FROM public.t1 WHERE g = \$1;\n"
	  . "EXECUTE p4(1);\nEXECUTE p4(2);\n"
	  . "PREPARE p5(int) AS SELECT count(*) FROM public.t1 WHERE id = \$1;\n"
	  . "SET plan_cache_mode = auto;\n"
	  . "EXECUTE p5(7);\nEXECUTE p5(8);\nEXECUTE p5(9);\nEXECUTE p5(10);\nEXECUTE p5(11);\nEXECUTE p5(12);\nEXECUTE p5(13);\n";
	$c->compare('prepared statements run many times', $script, ordered => 1);
}

# ---------------------------------------------------------------------------
# Cursors, nested loops (rescans), transactions and functions.
# ---------------------------------------------------------------------------
$c->compare('a scrollable cursor',
	"BEGIN;\nDECLARE c SCROLL CURSOR FOR SELECT id FROM public.t1 WHERE g = 2 ORDER BY id;\nFETCH 3 FROM c;\nFETCH BACKWARD 1 FROM c;\nFETCH 2 FROM c;\nMOVE ABSOLUTE 1 IN c;\nFETCH 2 FROM c;\nCOMMIT;", ordered => 1);
$c->compare('a nested loop that rescans the distributed scan',
	"SET enable_hashjoin = off;\nSET enable_mergejoin = off;\nSELECT t1.id, t2.id FROM public.t1 JOIN public.t2 ON t1.g = t2.a WHERE t1.id < 15 AND t2.id < 15;");
$c->compare('a merge join', "SET enable_hashjoin = off;\nSET enable_nestloop = off;\nSELECT t1.id, t2.id FROM public.t1 JOIN public.t2 ON t1.g = t2.a WHERE t1.id < 15 AND t2.id < 15;");
$c->compare('read your own writes in a transaction',
	"BEGIN;\nINSERT INTO public.t3 VALUES (1001, 3, 'new');
INSERT INTO public.t3 VALUES (1002, 3, 'new2');\nSELECT count(*) FROM public.t3;\nSELECT w FROM public.t3 WHERE grp = 3 AND k > 1000 ORDER BY k;\nROLLBACK;\nSELECT count(*) FROM public.t3;", ordered => 1);
$c->compare('a query inside a function and a DO block',
	"CREATE TEMP TABLE r (c bigint);\nDO \$\$ BEGIN INSERT INTO r SELECT count(*) FROM public.t1 WHERE g = 3; END \$\$;\nSELECT * FROM r;\nSELECT public.count_g(4);");
$c->compare('EXPLAIN shows the scan', 'SELECT 1 WHERE (SELECT count(*) FROM public.t1) > 0');
$c->compare('LIMIT without ORDER BY returns only existing rows',
	'SELECT count(*) FROM (SELECT * FROM public.t1 LIMIT 5) s');
$c->compare('a second statement after a LIMIT in one transaction',
	"BEGIN;\nSELECT count(*) FROM (SELECT * FROM public.t1 LIMIT 2) s;\nSELECT count(*), sum(n) FROM public.t1;\nSELECT count(*) FROM (SELECT * FROM public.t1 WHERE g = 1 LIMIT 3) s;\nSELECT count(*) FROM public.t2;\nCOMMIT;");
$c->compare('a plain-SQL function expanded into the statement',
	"CREATE OR REPLACE FUNCTION public.sql_rows(p int) RETURNS SETOF int LANGUAGE sql STABLE AS 'SELECT id FROM public.t1 WHERE g = p';\nSELECT count(*) FROM public.sql_rows(4);");

# EXPLAIN on the cluster only.
{
	my ($rc, $out, $err) = run_sql($nodes[2], "EXPLAIN (COSTS OFF) SELECT t1.id FROM public.t1 JOIN public.t2 ON t1.g = t2.a WHERE t1.v = 'v3'");
	like($out, qr/Custom Scan \(Kingres Distributed Scan\)/, 'EXPLAIN shows the distributed scan');
	like($out, qr/Remote groups: 2/, 'EXPLAIN shows the number of remote groups');
	like($out, qr/Remote SQL: SELECT .* FROM public\.t1 AS ks_t WHERE \(\(ks_t\.v\) = 'v3'::text\)|Remote SQL: SELECT .* FROM public\.t1 AS ks_t WHERE/s,
		'EXPLAIN shows the remote SQL with the restriction that is shipped');
	($rc, $out, $err) = run_sql($nodes[0], "EXPLAIN (COSTS OFF) SELECT t1.id FROM public.t1 WHERE t1.id = 5");
	like($out, qr/Kingres Remote Statement|Seq Scan|Index Scan|Append/, 'a statement restricted by key is not planned with the scan');
	unlike($out, qr/Distributed Scan/, 'a statement restricted by key does not use the distributed scan');
}

# ---------------------------------------------------------------------------
# Switches and limits.
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($nodes[0], "SET kingres_shard.enable_distributed_scan = off; SELECT count(*) FROM public.t1");
	like($err, qr/statements that span shard groups are not supported yet/, 'with the scan switched off the phase 1 error is raised');
	($rc, $out, $err) = run_sql($nodes[0], 'SELECT * FROM public.t1 FOR UPDATE');
	like($err, qr/row-level locking on a statement that spans shard groups is not supported yet/, 'row locking on a statement that spans groups is refused');
	($rc, $out, $err) = run_sql($nodes[0], 'SELECT ctid FROM public.t1');
	like($err, qr/system columns of distributed table .* are not supported/, 'system columns are refused');
	($rc, $out, $err) = run_sql($nodes[0], 'INSERT INTO public.t3 SELECT id + 5000, 1, v FROM public.t1');
	like($err, qr/statements that span shard groups are not supported yet/, 'INSERT ... SELECT that spans groups is still refused');
	($rc, $out, $err) = run_sql($nodes[0], "INSERT INTO public.t3 VALUES (5001, 1, 'a'), (5002, 1, 'b')");
	like($err, qr/statements that span shard groups are not supported yet/, 'a multi-row INSERT that spans groups is still refused');
	is($c->on_cluster("SELECT count(*) FROM public.t3 WHERE k > 5000"), '0', 'nothing was inserted by the refused statements');
}

# ---------------------------------------------------------------------------
# Large result: batched fetching with a small work_mem.
# ---------------------------------------------------------------------------
{
	my @big = map { sprintf("%d, '%s'", $_, 'pad' x 20 . $_) } (1 .. 3000);
	$c->script_on_both(rows_sql('big', @big));
	my $log_before = -s $nodes[1]->logfile;
	$c->compare('a large result with a small fetch size and work_mem',
		"SET kingres_shard.scan_fetch_size = 7;\nSET work_mem = '64kB';\nSELECT count(*), sum(id), max(pad), min(pad) FROM public.big;",
		nodes => [0]);
	$c->compare('a large result ordered and limited',
		"SET kingres_shard.scan_fetch_size = 7;\nSET work_mem = '64kB';\nSELECT id FROM public.big ORDER BY pad DESC, id LIMIT 12;", ordered => 1, nodes => [2]);
	$c->compare('a large join with a small work_mem',
		"SET kingres_shard.scan_fetch_size = 50;\nSET work_mem = '64kB';\nSELECT count(*) FROM public.big b JOIN public.t1 ON t1.id = b.id % 240 + 1;");
	my $log = slurp_file($nodes[1]->logfile, $log_before);
	my $fetches = () = $log =~ /FETCH FORWARD 7 FROM ks_scan_/g;
	cmp_ok($fetches, '>=', 100, "the other group's rows were read in batches ($fetches FETCH commands of 7 rows)");
}

# A group that is down: an error, never a partial answer.
{
	$nodes[2]->stop;
	my ($rc, $out, $err) = run_sql($nodes[0], 'SELECT count(*) FROM public.t1');
	isnt($rc, 0, 'a scan with a group down fails');
	like($err, qr/could not connect to node/, 'with a connection error');
	is($out, '', 'and returns no partial answer');
	my ($k_up) = keys_of_group($nodes[0], 1, 0, 1);
	($rc, $out, $err) = run_sql($nodes[0], "SELECT count(*) FROM public.t1 WHERE id = $k_up");
	is($err, '', 'a statement restricted to a group that is up still works');
	$nodes[2]->start;
	$c->compare('the scan works again when the group is back', 'SELECT count(*), sum(n) FROM public.t1', nodes => [0]);
}

# Nothing is left open on the other nodes.
is(prepared_count($_), '0', "nothing is prepared on $_->name") for @nodes;
is($_->safe_psql('postgres', "SELECT count(*) FROM pg_cursors"), '0', 'no cursor is left') for @nodes;
is($_->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE state LIKE 'idle in transaction%'"), '0',
	'no remote transaction is left open on ' . $_->name) for @nodes;

done_testing();
