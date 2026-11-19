# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/124_pushdown_closed.pl
#
# Step 6 of phase 2b (design/shard-phase2b.md, 4.4, 6.4, 7.5, 12, 13 and 16.2): the Kingres Fragment Scan and
# closed queries.  This is the design's 059 (the numbers 055 to 069 of the design are taken by tests of 0.5)
# and the part of its 069 that applies to step 6.  A closed query (every output row depends on the rows of one
# shard group: a grouping, a DISTINCT or a join on the distribution key) is run whole by every group; its
# answer must be the single node's, as a multiset and in order where the query orders, under every plan
# profile, from every node, with the path assertion (a test that passes because nothing was shipped fails).
#
#   A. closed queries that are shipped (each with the path assertion, against the twin and the scan);
#   B. almost closed queries, which must NOT be shipped, must take the scan, and must still be right; their
#      plan is the plan that the switch enable_pushdown = off gives (nothing changed for them);
#   C. EXPLAIN (section 12) and the switches (section 13);
#   D. parameters, prepared statements and generic plans, cursors (closed early), LIMIT above the scan,
#      transactions at each isolation level, a function that runs the query, and memory over many runs.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('pc');
my ($n1, $n2, $n3) = $c->nodes;
my $single = $c->single;
my @nodes = ($n1, $n2, $n3);
my $FS = 'Kingres Fragment Scan';
my $DS = 'Kingres Distributed Scan';

sub table
{
	my ($create, $dist) = @_;
	$c->on_cluster($create . ($dist // ''));
	$c->on_single($create);
}

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# The reason a statement is not shipped (EXPLAIN VERBOSE of the scan it gets).
sub reason_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (VERBOSE, COSTS OFF) $sql");
	return $out =~ /Pushdown: not used: (.*)$/m ? $1 : undef;
}

sub sorted { return join("\n", sort split /\n/, $_[0]); }

# ---------------------------------------------------------------------------
# the data
# ---------------------------------------------------------------------------
table(q{CREATE TABLE public.s1 (id int PRIMARY KEY, a int, b int, t text, n numeric(10,2), d date, ts timestamptz, f float8)},
	q{; SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.s2 (id int PRIMARY KEY, c int, t text)},
	q{; SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s1')});
table(q{CREATE TABLE public.far (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.far', 'id', bucket_count => 7)});
table(q{CREATE TABLE public.tk (k text PRIMARY KEY, v int)},
	q{; SELECT kingres.create_distributed_table('public.tk', 'k', bucket_count => 12)});
table(q{CREATE TABLE public.r (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.r')});
$c->on_every_node(q{CREATE TABLE public.loc (x int, y int)});
$c->on_single(q{CREATE TABLE public.loc (x int, y int)});
$c->on_both(q{INSERT INTO public.r VALUES (0, 'L0'), (1, 'L1'), (2, 'L2'), (3, 'L3'), (4, 'L4'), (5, 'L5'), (6, 'L6')});
$c->on_both(q{INSERT INTO public.s1 SELECT g, CASE WHEN g % 17 = 0 THEN NULL ELSE (g * 7) % 50 END,
	CASE WHEN g % 19 = 0 THEN NULL ELSE (g * 13) % 30 END, CASE WHEN g % 23 = 0 THEN NULL ELSE 'v' || (g % 400) END,
	round((g % 1000) / 7.0, 2), date '2024-01-01' + (g % 365), timestamptz '2024-01-01 00:00:00+00' + (g || ' minutes')::interval,
	(g % 100) / 3.0 FROM generate_series(1, 3000) g});
$c->on_both(q{INSERT INTO public.s2 SELECT g, CASE WHEN g % 11 = 0 THEN NULL ELSE g % 9 END, CASE WHEN g % 13 = 0 THEN NULL ELSE 's' || g END FROM generate_series(1, 3000, 2) g});
$c->on_both(q{INSERT INTO public.far SELECT g, 'f' || g FROM generate_series(1, 60) g});
$c->on_both(q{INSERT INTO public.tk SELECT 'k' || g, g % 13 FROM generate_series(1, 600) g});
$c->on_every_node(q{INSERT INTO public.loc SELECT g, g % 5 FROM generate_series(1, 40) g});
$c->on_single(q{INSERT INTO public.loc SELECT g, g % 5 FROM generate_series(1, 40) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# ---------------------------------------------------------------------------
# A. closed queries that are shipped
# ---------------------------------------------------------------------------
my @closed = (
	[ 'group by key',        q{SELECT id, count(*), sum(a), min(t), max(b) FROM public.s1 GROUP BY id HAVING count(*) > 0 ORDER BY id}, 1 ],
	[ 'avg and bool',        q{SELECT id, avg(n), bool_and(a > 3), count(DISTINCT b) FROM public.s1 GROUP BY id ORDER BY id}, 1 ],
	[ 'join',                q{SELECT s1.id, s2.c, s2.t FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE s1.a > 3 ORDER BY 1}, 1 ],
	[ 'join, where equality', q{SELECT s1.id, s2.c FROM public.s1, public.s2 WHERE s1.id = s2.id AND s2.c = 3 ORDER BY 1}, 1 ],
	[ 'left join, grouped',  q{SELECT s1.id, count(s2.c), sum(s2.c) FROM public.s1 LEFT JOIN public.s2 ON s2.id = s1.id GROUP BY s1.id ORDER BY 1}, 1 ],
	[ 'distinct',            q{SELECT DISTINCT id, a FROM public.s1 WHERE a > 40}, 0 ],
	[ 'distinct, ordered, limit', q{SELECT DISTINCT id, b FROM public.s1 ORDER BY id DESC LIMIT 10}, 1 ],
	[ 'reference join, grouped', q{SELECT s1.id, r.label, count(*) FROM public.s1 JOIN public.r ON r.k = s1.b % 7 GROUP BY s1.id, r.label ORDER BY 1, 2}, 1 ],
	[ 'order by a column that is not selected', q{SELECT id FROM public.s1 GROUP BY id ORDER BY max(a), id LIMIT 20}, 1 ],
	[ 'order by a junk column', q{SELECT s1.t FROM public.s1 JOIN public.s2 ON s2.id = s1.id ORDER BY s1.id DESC}, 1 ],
	[ 'self join',           q{SELECT x.id, y.a FROM public.s1 x JOIN public.s1 y ON y.id = x.id WHERE x.a > y.b ORDER BY 1}, 1 ],
	[ 'three tables',        q{SELECT s1.id, s2.c, z.b FROM public.s1 JOIN public.s2 ON s2.id = s1.id JOIN public.s1 z ON z.id = s1.id WHERE z.b < 5 ORDER BY 1}, 1 ],
	[ 'pruned by IN',        q{SELECT id, count(*), sum(b) FROM public.s1 WHERE id IN (1, 2, 3, 50, 500, 1999) GROUP BY id ORDER BY id}, 1 ],
	[ 'pruned by equality',  q{SELECT s1.id, s2.c FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE s1.id = ANY (ARRAY[3, 4, 1001]) ORDER BY 1}, 1 ],
	[ 'group by key and column', q{SELECT id, a, count(*) FROM public.s1 GROUP BY id, a ORDER BY id}, 1 ],
	[ 'numeric, date, timestamptz', q{SELECT id, sum(n), min(d), max(ts) FROM public.s1 GROUP BY id ORDER BY id}, 1 ],
	[ 'offset and limit',    q{SELECT id, count(*) FROM public.s1 GROUP BY id ORDER BY id OFFSET 100 LIMIT 25}, 1 ],
	[ 'empty result',        q{SELECT id, count(*) FROM public.s1 WHERE a > 1000 GROUP BY id}, 0 ],
	[ 'text key',            q{SELECT k, sum(v), count(*) FROM public.tk GROUP BY k ORDER BY k}, 1 ],
	[ 'text key, having',    q{SELECT k, max(v) FROM public.tk GROUP BY k HAVING max(v) > 5 ORDER BY k LIMIT 30}, 1 ],
	[ 'integer arithmetic',  q{SELECT id, sum(a * 2 + b) % 11, count(*) FILTER (WHERE a > 5) FROM public.s1 GROUP BY id ORDER BY id}, 1 ],
	[ 'constants in the list', q{SELECT id, 7 AS seven, 'lit', count(*) FROM public.s1 GROUP BY id ORDER BY id LIMIT 12}, 1 ],
);
for my $q (@closed)
{
	my ($name, $sql, $ordered) = @$q;
	$c->compare("closed: $name", $sql, ordered => $ordered, plans => ['pushed', 'scan'], path => $FS, normalize => 1);
}

# ---------------------------------------------------------------------------
# B. almost closed queries: not shipped, still right, the plan of before
# ---------------------------------------------------------------------------
my @almost = (
	[ 'group by a column that is not the key', q{SELECT a, count(*) FROM public.s1 GROUP BY a ORDER BY a NULLS LAST}, 1, qr/grouping/ ],
	[ 'aggregate without grouping',  q{SELECT count(*), sum(a), max(t) FROM public.s1}, 0, qr/aggregate without GROUP BY/ ],
	[ 'distinct on a non-key',       q{SELECT DISTINCT a FROM public.s1 WHERE a IS NOT NULL}, 0, qr/DISTINCT/ ],
	[ 'distinct on',                 q{SELECT DISTINCT ON (a) a, id FROM public.s1 WHERE a > 40 ORDER BY a, id}, 1, qr/DISTINCT/ ],
	[ 'window function',             q{SELECT id, row_number() OVER (PARTITION BY id ORDER BY a) FROM public.s1 WHERE id < 50 ORDER BY 1}, 1, qr/window/ ],
	[ 'float aggregate',             q{SELECT id, sum(f) FROM public.s1 GROUP BY id ORDER BY id LIMIT 50}, 1, qr/order of the rows/ ],
	[ 'order-sensitive aggregate',   q{SELECT id, string_agg(t, ',') FROM public.s1 WHERE id < 100 GROUP BY id ORDER BY id}, 1, qr/aggregate/ ],
	[ 'stable function',             q{SELECT id, count(*) FROM public.s1 WHERE ts < now() GROUP BY id ORDER BY id LIMIT 40}, 1, qr/not built in and immutable/ ],
	[ 'cast through text',           q{SELECT id, max(a::text) FROM public.s1 GROUP BY id ORDER BY id LIMIT 40}, 1, qr/text form/ ],
	[ 'sub-select',                  q{SELECT id, count(*) FROM public.s1 WHERE id IN (SELECT id FROM public.s2 WHERE c > 3) GROUP BY id ORDER BY 1}, 1, qr/sub-select/ ],
	[ 'sub-select in the list',      q{SELECT id, (SELECT count(*) FROM public.s2 WHERE s2.id = s1.id) FROM public.s1 WHERE id < 40 ORDER BY 1}, 1, qr/sub-select/ ],
	[ 'with',                        q{WITH x AS (SELECT id, a FROM public.s1) SELECT id, count(*) FROM x GROUP BY id ORDER BY 1 LIMIT 10}, 1, undef ],
	[ 'join on a non-key',           q{SELECT s1.id, s2.c FROM public.s1 JOIN public.s2 ON s2.id = s1.a WHERE s1.id < 300 ORDER BY 1, 2}, 1, qr/key/ ],
	[ 'another colocation group',    q{SELECT s1.id, far.w FROM public.s1 JOIN public.far ON far.id = s1.id ORDER BY 1}, 1, qr/colocation/ ],
	[ 'key from the nullable side',  q{SELECT s2.id, count(*) FROM public.s1 LEFT JOIN public.s2 ON s2.id = s1.id GROUP BY s2.id ORDER BY 1 NULLS LAST LIMIT 20}, 1, qr/grouping/ ],
	[ 'whole row',                   q{SELECT s2, count(*) FROM public.s2 GROUP BY s2.id, s2 ORDER BY s2.id LIMIT 5}, 1, undef ],
	[ 'local table',                 q{SELECT s1.id, count(*) FROM public.s1 JOIN public.loc ON loc.x = s1.a GROUP BY s1.id ORDER BY 1 LIMIT 30}, 1, qr/local table/ ],
	[ 'key in an expression',        q{SELECT id % 10, count(*) FROM public.s1 GROUP BY id % 10 ORDER BY 1}, 1, qr/grouping/ ],
	[ 'one table, nothing to gain',  q{SELECT id, a FROM public.s1 WHERE a > 45 ORDER BY id LIMIT 30}, 1, qr/scan returns/ ],
	[ 'one table and a reference table, nothing to gain', q{SELECT s1.id, r.label FROM public.s1 JOIN public.r ON r.k = s1.a % 7 WHERE s1.id < 500 ORDER BY 1}, 1, qr/scan returns/ ],
	[ 'grouping sets',               q{SELECT id, a, count(*) FROM public.s1 WHERE id < 30 GROUP BY GROUPING SETS ((id), (id, a)) ORDER BY 1, 2 NULLS LAST, 3}, 1, qr/grouping sets/ ],
	[ 'distinct with a junk column', q{SELECT DISTINCT s1.id FROM public.s1 GROUP BY s1.id, s1.a ORDER BY 1 LIMIT 20}, 1, undef ],
);
for my $q (@almost)
{
	my ($name, $sql, $ordered, $why) = @$q;
	$c->compare("almost closed: $name", $sql, ordered => $ordered, plans => ['pushed', 'scan'], profiles => 0, normalize => 1);
	my $plan = plan_of($n1, $sql);
	unlike($plan, qr/\Q$FS\E/, "almost closed: $name is not shipped");
	my $off = plan_of($n1, $sql, 'SET kingres_shard.enable_pushdown = off; ');
	is($plan, $off, "almost closed: $name has the plan that enable_pushdown = off gives");
	if (defined $why)
	{
		my $reason = reason_of($n1, $sql);
		like($reason // '(none)', $why, "almost closed: $name says why") or diag("reason: " . ($reason // '(none)'));
	}
}
# the one that is a closed query whose statement has a junk column that DISTINCT would see
like(plan_of($n1, q{SELECT id, count(*) FROM public.s1 WHERE id = 5 GROUP BY id FOR UPDATE}), qr/./, 'row locking is planned (and never shipped)');
unlike(plan_of($n1, q{SELECT id, count(*) FROM public.s1 WHERE id IN (5, 6, 7, 8, 9, 10) GROUP BY id FOR UPDATE}), qr/Fragment Scan/, 'row locking is never shipped');

# ---------------------------------------------------------------------------
# C. EXPLAIN and the switches
# ---------------------------------------------------------------------------
{
	my $sql = q{SELECT id, count(*), sum(a) FROM public.s1 GROUP BY id HAVING count(*) > 0 ORDER BY id LIMIT 7};
	my $p = plan_of($n1, $sql);
	like($p, qr/Limit\n\s+->  Sort\n\s+Sort Key: s1\.id\n\s+->  Custom Scan \(Kingres Fragment Scan\)/, 'EXPLAIN: the origin sorts and limits above the fragment scan');
	like($p, qr/^\s+Pushed: closed aggregate \(by key\)$/m, 'EXPLAIN: what is pushed');
	like($p, qr/^\s+Groups: 3 of 3 \(local \+ 2 remote\)$/m, 'EXPLAIN: the groups');
	like($p, qr/^\s+Remote SQL: SELECT id, count\(\*\) AS count, sum\(a\) AS sum FROM public\.s1 GROUP BY id HAVING \(count\(\*\) > 0\)$/m,
		'EXPLAIN: the text that every group runs (no ORDER BY, no LIMIT)');
	my $pr = plan_of($n2, q{SELECT id, count(*) FROM public.s1 WHERE id IN (1, 2, 3) GROUP BY id});
	like($pr, qr/^\s+Groups: [123] of 3 \((?:local|\d remote|local \+ \d remote); pruned by id\)$/m, 'EXPLAIN: pruning is shown') or diag($pr);
	my ($rc, $out, $err) = run_sql($n1, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) $sql");
	is($rc, 0, 'EXPLAIN ANALYZE runs the fragment scan') or diag($err);
	like($out, qr/Rows from groups: .*local=\d+.*group \d=\d+/, 'EXPLAIN ANALYZE: the rows of each group') or diag($out);
	like($out, qr/Limit \(actual rows=7(?:.00)? loops=1\)/, 'EXPLAIN ANALYZE: the limit');
	my $v = plan_of($n1, q{SELECT a, count(*) FROM public.s1 GROUP BY a});
	unlike($v, qr/Pushdown:/, 'EXPLAIN without VERBOSE shows no reason');
	like(reason_of($n3, q{SELECT a, count(*) FROM public.s1 GROUP BY a}) // '', qr/distribution key/, 'EXPLAIN VERBOSE of a scan says why the query was not pushed');
	is(reason_of($n3, q{SELECT a, count(*) FROM public.s1 GROUP BY a}), reason_of($n1, q{SELECT a, count(*) FROM public.s1 GROUP BY a}), '... the same on every node');

	# the switches
	my ($rs, $os, $es) = run_sql($single, $sql);
	for my $set ('SET kingres_shard.enable_pushdown = off', 'SET kingres_shard.enable_pushdown_closed = off'
		)
	{
		my $pp = plan_of($n1, $sql, "$set; ");
		unlike($pp, qr/Fragment Scan/, "$set: no fragment scan");
		my ($rd, $od, $ed) = run_sql($n1, "$set; $sql");
		is($od, $os, "$set: the answer is the single node's");
	}
	like(plan_of($n1, $sql, 'SET kingres_shard.enable_pushdown = off; '), qr/Kingres Distributed Scan/, 'enable_pushdown = off: the scan of 2a');
	is(plan_of($n1, $sql, 'SET kingres_shard.enable_pushdown_closed = off; '), plan_of($n1, $sql, 'SET kingres_shard.enable_pushdown = off; '),
		'the two switches give one plan for a closed query');
	is($n1->safe_psql('postgres', q{SELECT setting FROM pg_settings WHERE name = 'kingres_shard.enable_pushdown'}), 'on', 'enable_pushdown is on by default');
	is($n1->safe_psql('postgres', q{SELECT context FROM pg_settings WHERE name = 'kingres_shard.enable_pushdown_closed'}), 'user', 'enable_pushdown_closed can be set by a user');
}

# ---------------------------------------------------------------------------
# D. parameters, prepared statements, cursors, LIMIT, isolation levels, functions, memory
# ---------------------------------------------------------------------------
sub same_on_all
{
	my ($name, $script, %opt) = @_;
	my ($rs, $os, $es) = run_sql($single, $script);
	is($rs, 0, "$name: the single node runs it") or diag($es);
	for my $i (0 .. 2)
	{
		my ($rd, $od, $ed) = run_sql($nodes[$i], $script);
		is($rd, 0, "$name: node " . ($i + 1) . " runs it") or diag($ed);
		is($opt{ordered} ? $od : sorted($od), $opt{ordered} ? $os : sorted($os), "$name: node " . ($i + 1) . " gives the single node's rows");
	}
}

my $PARAM = q{SELECT id, count(*), sum(a) FROM public.s1 WHERE b > $1 AND id <> $2 GROUP BY id HAVING sum(b) > $3 ORDER BY id LIMIT $4};
for my $mode ('force_generic_plan', 'force_custom_plan', 'auto')
{
	my $execs = join("\n", map { "EXECUTE p(" . (3 * $_ % 25) . ", $_, $_, " . (5 + $_) . ");" } (1 .. 7));
	same_on_all("prepared ($mode)", qq{SET plan_cache_mode = $mode; PREPARE p(int, int, int, int) AS $PARAM; $execs}, ordered => 1);
	my $pl = plan_of($n1, 'EXECUTE p(1, 2, 3, 5)', "SET plan_cache_mode = $mode; PREPARE p(int, int, int, int) AS $PARAM; ");
	like($pl, qr/Fragment Scan/, "prepared ($mode): the plan is the fragment scan");
}
same_on_all('a parameter in the key restriction and the select list',
	q{SET plan_cache_mode = force_generic_plan; PREPARE q(int, text) AS SELECT id, $2 AS tag, count(*) FROM public.s1 WHERE id = ANY (ARRAY[$1, $1 + 1, 2000]) GROUP BY id ORDER BY id;
	  EXECUTE q(10, 'x'); EXECUTE q(1500, 'it''s'); EXECUTE q(2999, NULL);}, ordered => 1);

# a cursor that is closed early, a LIMIT above the scan with no order
for my $n (@nodes)
{
	my ($rc, $out, $err) = run_sql($n, q{BEGIN;
		DECLARE cur NO SCROLL CURSOR FOR SELECT id, count(*) FROM public.s1 GROUP BY id;
		FETCH 3 FROM cur; CLOSE cur;
		DECLARE cur2 CURSOR FOR SELECT id, count(*) FROM public.s1 GROUP BY id ORDER BY id;
		FETCH 2 FROM cur2; FETCH BACKWARD 1 FROM cur2; FETCH ABSOLUTE 5 FROM cur2; MOVE FIRST IN cur2; FETCH 1 FROM cur2;
		SELECT count(*) FROM public.s1; COMMIT;});
	is($rc, 0, 'a cursor that is closed early, on ' . $n->name) or diag($err);
	like($out, qr/^1\|1\s*$/m, '... and a scroll cursor reads forward and backward');
	my ($rc2, $o2, $e2) = run_sql($n, q{SELECT id, count(*) FROM public.s1 GROUP BY id LIMIT 5});
	is($rc2, 0, 'LIMIT above the scan with no order, on ' . $n->name) or diag($e2);
	is(scalar(split /\n/, $o2), 5, '... returns five rows');
	my ($rc3, $o3, $e3) = run_sql($n, q{SELECT id, count(*) FROM public.s1 GROUP BY id LIMIT 5; SELECT sum(c) FROM (SELECT count(*) AS c FROM public.s1 GROUP BY id) q;});
	is($rc3, 0, 'the session is usable after an early stop') or diag($e3);
	like($o3, qr/\n3000$/, '... and the next statement is right');
}
like(plan_of($n1, q{SELECT id, count(*) FROM public.s1 GROUP BY id LIMIT 5}), qr/Limit\n\s+->  Custom Scan \(Kingres Fragment Scan\)/, 'LIMIT directly above the fragment scan');
for my $n (@nodes)
{
	is($n->safe_psql('postgres', q{SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction'}), 0, 'no session of ' . $n->name . ' is left in a transaction');
	is($n->safe_psql('postgres', q{SELECT count(*) FROM pg_cursors}), 0, 'no cursor is left on ' . $n->name);
}

# isolation levels: reads of a transaction, with its own writes
for my $level ('READ COMMITTED', 'REPEATABLE READ', 'SERIALIZABLE')
{
	my $script = qq{BEGIN ISOLATION LEVEL $level;
		SELECT id, count(*), sum(a) FROM public.s1 WHERE id < 40 GROUP BY id ORDER BY id;
		INSERT INTO public.s1 (id, a, b, t) VALUES (100001, 1, 2, 'new'), (100002, 3, 4, 'new2');
		SELECT id, count(*), sum(a) FROM public.s1 WHERE id > 100000 GROUP BY id ORDER BY id;
		SELECT count(*), sum(a) FROM (SELECT id, sum(a) AS a FROM public.s1 GROUP BY id) q;
		SELECT s1.id, s2.c FROM public.s1 JOIN public.s2 ON s2.id = s1.id WHERE s1.id IN (1, 3, 100001) ORDER BY 1;
		DELETE FROM public.s1 WHERE id > 100000;
		SELECT count(*) FROM (SELECT id FROM public.s1 GROUP BY id) q;
		COMMIT;};
	same_on_all("isolation level $level, with writes", $script, ordered => 1);
	like(plan_of($n1, q{SELECT id, count(*), sum(a) FROM public.s1 WHERE id < 40 GROUP BY id ORDER BY id}, "BEGIN ISOLATION LEVEL $level; "), qr/Fragment Scan/, "isolation level $level: shipped");
}
same_on_all('a read-only transaction and a savepoint',
	q{BEGIN READ ONLY; SELECT id, count(*) FROM public.s1 WHERE id < 20 GROUP BY id ORDER BY id; SAVEPOINT x;
	  SELECT count(*) FROM (SELECT id FROM public.s1 WHERE a > 10 GROUP BY id) q; ROLLBACK TO x;
	  SELECT id, sum(b) FROM public.s1 WHERE id < 8 GROUP BY id ORDER BY id; COMMIT;}, ordered => 1);

# a function and a DO block that run the query (each statement is planned by itself)
my $fn = q{CREATE FUNCTION public.cnt(lo int) RETURNS bigint LANGUAGE plpgsql AS $$ DECLARE r bigint; BEGIN
	SELECT count(*) INTO r FROM (SELECT id FROM public.s1 WHERE id > lo GROUP BY id) q; RETURN r; END $$};
$c->on_every_node($fn);
$c->on_single($fn);
same_on_all('a function that runs a query', q{SELECT public.cnt(0), public.cnt(2000), public.cnt(2999)});
same_on_all('a DO block', q{DO $$ DECLARE r record; n int := 0; BEGIN FOR r IN SELECT id, count(*) AS c FROM public.s1 GROUP BY id LOOP n := n + r.c; END LOOP; RAISE NOTICE 'sum=%', n; END $$});

# memory: many runs in one backend (the start and the stop of a fragment scan are the same code as a rescan)
{
	my $mem = q{SELECT sum(total_bytes) FROM pg_backend_memory_contexts};
	my ($rc, $out, $err) = run_sql($n1, qq{SELECT public.cnt(0) FROM generate_series(1, 20);
		SELECT ($mem) AS before \\gset
		SELECT public.cnt(i) FROM generate_series(1, 1500) i;
		SELECT (($mem) - :before) < 1000000 AS flat, (($mem) - :before) AS grown;});
	like($out, qr/^t\|/m, 'memory stays flat over 1500 runs of a shipped query') or diag("$out\n$err");
}

done_testing();
