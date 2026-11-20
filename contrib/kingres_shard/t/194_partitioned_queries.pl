# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/194_partitioned_queries.pl: the queries of a user-partitioned distributed table give the answers of the same table on one node, from every node
# (design/shard-phase9.md, increment 9.5: the planner prunes at both levels; the scan, the fragments and the pushdown see buckets, not leaves).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my ($n1, $n2, $n3) = make_cluster('pq', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

my $ddl = q{
	CREATE TABLE public.TBL (customer int NOT NULL, created date NOT NULL, id int NOT NULL, qty int, note text, PRIMARY KEY (customer, created, id)) PARTITION BY RANGE (created);
	CREATE TABLE public.TBL_p1 PARTITION OF public.TBL FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
	CREATE TABLE public.TBL_p2 PARTITION OF public.TBL FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
	CREATE TABLE public.TBL_p3 PARTITION OF public.TBL FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');
	CREATE TABLE public.TBL_def PARTITION OF public.TBL DEFAULT;
	CREATE INDEX TBL_qty ON public.TBL (qty);
	INSERT INTO public.TBL SELECT g % 23, date '2026-01-01' + (g % 110), g, g % 60, 'n' || (g % 7) FROM generate_series(1, 2000) g;
};
(my $d1 = $ddl) =~ s/TBL/pq/g;
(my $d2 = $ddl) =~ s/TBL/lq/g;   # the same table, never distributed
$n1->safe_psql('postgres', $d1 . $d2 . q{
	SELECT kingres.create_distributed_table('public.pq', 'customer', bucket_count => 6);
	CREATE TABLE public.pr (customer int PRIMARY KEY, name text); INSERT INTO public.pr SELECT g, 'c' || g FROM generate_series(0, 22) g;
	SELECT kingres.create_reference_table('public.pr');
	CREATE TABLE public.pj (customer int NOT NULL, id int NOT NULL, v int, PRIMARY KEY (customer, id)); INSERT INTO public.pj SELECT g % 23, g, g FROM generate_series(1, 300) g;
	SELECT kingres.create_distributed_table('public.pj', 'customer', colocate_with => 'public.pq');
	CREATE TABLE public.pj_l (customer int NOT NULL, id int NOT NULL, v int, PRIMARY KEY (customer, id)); INSERT INTO public.pj_l SELECT * FROM public.pj;
});
$_->safe_psql('postgres', 'ANALYZE public.pq') for @nodes;

my @queries = (
	'SELECT count(*), sum(qty), min(created), max(created) FROM public.T',
	q{SELECT count(*) FROM public.T WHERE customer = 5},
	q{SELECT count(*) FROM public.T WHERE created >= '2026-02-10' AND created < '2026-02-20'},
	q{SELECT count(*), sum(qty) FROM public.T WHERE customer = 7 AND created >= '2026-03-01'},
	q{SELECT customer, count(*), sum(qty) FROM public.T GROUP BY customer ORDER BY customer},
	q{SELECT created, count(*) FROM public.T WHERE created < '2026-02-01' GROUP BY created ORDER BY created},
	q{SELECT note, count(*), avg(qty)::numeric(8,2) FROM public.T GROUP BY note ORDER BY note},
	q{SELECT * FROM public.T WHERE customer = 3 ORDER BY created, id LIMIT 5},
	q{SELECT id FROM public.T WHERE qty = 17 AND created >= '2026-04-01' ORDER BY id},
	q{SELECT count(*) FROM public.T WHERE created >= '2026-04-01'},
	q{SELECT count(DISTINCT customer) FROM public.T WHERE created BETWEEN '2026-01-15' AND '2026-03-15'},
	q{SELECT t.customer, count(*) FROM public.T t JOIN public.J j ON j.customer = t.customer AND j.id = t.id GROUP BY t.customer ORDER BY 1},
	q{SELECT r.name, count(*) FROM public.T t JOIN public.pr r ON r.customer = t.customer WHERE t.created < '2026-02-01' GROUP BY r.name ORDER BY 1},
	q{SELECT (SELECT count(*) FROM public.T x WHERE x.customer = o.customer AND x.created < '2026-02-01') FROM public.pr o WHERE o.customer < 3 ORDER BY o.customer},
	q{SELECT customer, id, row_number() OVER (PARTITION BY customer ORDER BY created, id) FROM public.T WHERE customer IN (1, 2) AND created < '2026-01-20' ORDER BY customer, id},
	q{SELECT created, sum(qty) OVER (ORDER BY created ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) FROM (SELECT created, sum(qty) AS qty FROM public.T WHERE created < '2026-01-10' GROUP BY created) s ORDER BY created},
);
for my $q (@queries)
{
	(my $dist = $q) =~ s/\bT\b/pq/g; $dist =~ s/\bJ\b/pj/g;
	(my $loc = $q) =~ s/\bT\b/lq/g; $loc =~ s/\bJ\b/pj_l/g;
	my $want = q1($n1, $loc);
	for my $n (@nodes)
	{
		my ($rc, $out, $err) = run_sql($n, $dist);
		is($rc, 0, 'runs on ' . $n->name . ': ' . substr($q, 0, 70)) or diag($err);
		is($out, $want, '  the answer is the one of a single node');
	}
}
# the plans: the statements that the pushdown ships for a plain table are shipped for a user-partitioned one (a fragment names the table)
{
	my $p = (run_sql($n2, q{EXPLAIN (COSTS OFF) SELECT customer, count(*), sum(qty) FROM public.pq GROUP BY customer}))[1];
	like($p, qr/Kingres Fragment Scan/, 'a closed aggregate by the key is a fragment scan');
	like($p, qr/Remote SQL: SELECT customer, count\(\*\) AS count, sum\(qty\) AS sum FROM public\.pq GROUP BY customer/, 'and the remote statement names the table');
	$p = (run_sql($n2, q{EXPLAIN (COSTS OFF) SELECT * FROM public.pq WHERE customer = 3 AND created < '2026-02-01'}))[1];
	like($p, qr/pq_p1_b\d{4}/, 'a key condition and an interval: the leaf of the interval is scanned');
	unlike($p, qr/pq_p2_b|pq_p3_b/, 'and the leaves of the other intervals are pruned');
	like($p, qr/pq_def_b/, 'the default leaf is scanned (it can hold such rows)');
	unlike($p, qr/Remote SQL/, 'and the statement runs on one group only');
}
# changes through the table: from every node, by key and by interval, checked against the single-node table
my @changes = (
	q{UPDATE public.T SET qty = qty + 1 WHERE customer = 4 AND created >= '2026-02-01' AND created < '2026-03-01'},
	q{UPDATE public.T SET note = 'x' WHERE customer = 9},
	q{DELETE FROM public.T WHERE customer = 11 AND created >= '2026-04-01'},
	q{INSERT INTO public.T VALUES (6, '2026-02-14', 900001, 1, 'ins'), (6, '2026-03-14', 900002, 2, 'ins'), (6, '2027-01-01', 900003, 3, 'ins')},
	q{DELETE FROM public.T WHERE id = 900002},
);
my $i = 0;
for my $c (@changes)
{
	(my $dist = $c) =~ s/\bT\b/pq/g; (my $loc = $c) =~ s/\bT\b/lq/g;
	my ($rc, $out, $err) = run_sql($nodes[$i++ % 3], $dist);
	is($rc, 0, 'a change from node ' . (($i - 1) % 3 + 1) . ': ' . substr($c, 0, 60)) or diag($err);
	q1($n1, $loc);
}
my $sig = q{SELECT count(*) || '|' || sum(qty) || '|' || md5(string_agg(customer || ':' || created || ':' || id || ':' || qty || ':' || note, ',' ORDER BY customer, created, id)) FROM public.T};
(my $sd = $sig) =~ s/\bT\b/pq/g; (my $sl = $sig) =~ s/\bT\b/lq/g;
is(q1($_, $sd), q1($n1, $sl), 'after the changes, the table is the single-node table, from ' . $_->name) for @nodes;
# a statement of several partitions and buckets at once, and COPY
{
	my ($rc, $out, $err) = run_sql($n2, q{UPDATE public.pq SET qty = qty + 1});
	is($rc, 0, 'an UPDATE of every row from another node') or diag($err);
	q1($n1, q{UPDATE public.lq SET qty = qty + 1});
	is(q1($n3, $sd), q1($n1, $sl), 'and the table is the single-node table');
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.pq SELECT customer, created + 400, id + 1000000, qty, note FROM public.pq WHERE created < '2026-01-05'});
	is($rc, 0, 'a colocated INSERT ... SELECT into the table itself (rows go to the default partition)') or diag($err);
	q1($n1, q{INSERT INTO public.lq SELECT customer, created + 400, id + 1000000, qty, note FROM public.lq WHERE created < '2026-01-05'});
	is(q1($n2, $sd), q1($n1, $sl), 'and the table is the single-node table');
	($rc, $out, $err) = run_sql($n1, q{TRUNCATE public.pq});
	is($rc, 0, 'TRUNCATE on the map group primary') or diag($err);
	is(q1($_, q{SELECT count(*) FROM public.pq}), '0', 'the table is empty, from ' . $_->name) for @nodes;
}
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'check_cluster() finds nothing');
done_testing();
