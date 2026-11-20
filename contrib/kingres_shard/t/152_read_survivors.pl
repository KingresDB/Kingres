# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/152_read_survivors.pl
#
# The review of step 5 (section 5, mutants) left rules of pushdown.c that no test of a read reached: the key rule of a FULL JOIN
# (M18), the window partition rule (M20), the sub-select rule (M02), and the aggregate and DISTINCT rules for a read.  The
# statements here are reads of the closed-query path (partial aggregation off, so that what is not closed is the scan's): each is
# run on every node under the plan profiles, its answer is the single node's, and what the rules refuse must not be shipped.  The
# mutants of those rules are run against this file and t/124 to t/127 (the report names the result of each).
#
#   A. FULL JOIN: on the keys, on a non-key, on both, with a restriction, with a reference table, three tables
#   B. windows over a closed grouping, partitioned by the key and by another column
#   C. sub-selects and a view

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(not_shipped_ok run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('rv');
my ($n1, $n2, $n3) = $c->nodes;
my $FS = 'Kingres Fragment Scan';

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

table(q{CREATE TABLE public.s1 (id int, a int, b int)},
	q{; SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.s2 (id int, c int, d int)},
	q{; SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s1')});
table(q{CREATE TABLE public.s3 (id int, e int)},
	q{; SELECT kingres.create_distributed_table('public.s3', 'id', colocate_with => 'public.s1')});
table(q{CREATE TABLE public.r (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.r')});
$c->on_both(q{INSERT INTO public.r VALUES (0, 'L0'), (1, 'L1'), (2, 'L2'), (3, 'L3'), (4, 'L4'), (5, 'L5'), (6, 'L6')});
# keys that are in one table only, in both, twice; values that match across keys
$c->on_both(q{INSERT INTO public.s1 SELECT g, g % 17, g % 5 FROM generate_series(1, 3000) g, generate_series(1, 2) dup});
$c->on_both(q{INSERT INTO public.s2 SELECT g, g % 17, g % 7 FROM generate_series(1000, 4000, 3) g, generate_series(1, 2) dup});
$c->on_both(q{INSERT INTO public.s3 SELECT g, g % 11 FROM generate_series(500, 3500, 2) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# ---------------------------------------------------------------------------
# A. FULL JOIN
# ---------------------------------------------------------------------------
my @full = (
	[ 'on the key', q{SELECT s1.id, s2.id, s1.a, s2.c FROM public.s1 FULL JOIN public.s2 ON s2.id = s1.id}, 0, undef ],
	[ 'on a non-key', q{SELECT s1.id, s2.id FROM public.s1 FULL JOIN public.s2 ON s2.c = s1.a WHERE s1.id < 60 OR s2.id < 1100 OR s1.id IS NULL}, 0, qr/FULL JOIN|key/ ],
	[ 'on the key and a non-key', q{SELECT s1.id, s2.id FROM public.s1 FULL JOIN public.s2 ON s2.id = s1.id AND s2.c = s1.a}, 0, undef ],
	[ 'on a non-key only, counted', q{SELECT count(*), count(s1.id), count(s2.id) FROM public.s1 FULL JOIN public.s2 ON s2.c = s1.a AND s2.d = s1.b}, 0, qr/FULL JOIN|key|aggregate/ ],
	[ 'on a non-key with a restriction on the keys', q{SELECT s1.id, s2.id FROM public.s1 FULL JOIN public.s2 ON s2.c = s1.a WHERE s1.id = s2.id}, 0, undef ],
	[ 'on the key, grouped by the key of one side', q{SELECT s1.id, count(s2.c), count(*) FROM public.s1 FULL JOIN public.s2 ON s2.id = s1.id GROUP BY s1.id}, 0, undef ],
	[ 'with a reference table', q{SELECT s1.id, r.label FROM public.s1 FULL JOIN public.r ON r.k = s1.a % 7 WHERE s1.id < 50 OR r.k IS NULL}, 0, qr/FULL JOIN/ ],
	[ 'three tables, the third on the key', q{SELECT s1.id, s2.id, s3.id FROM public.s1 FULL JOIN public.s2 ON s2.id = s1.id FULL JOIN public.s3 ON s3.id = s1.id}, 0, undef ],
);
for my $q (@full)
{
	my ($name, $sql, $ordered, $why) = @$q;
	$c->compare("full join $name", $sql, ordered => $ordered, plans => ['pushed', 'scan'], profiles => 0, normalize => 1);
	if (defined $why)
	{
		not_shipped_ok(plan_of($n1, $sql), "full join $name: is not shipped");
	}
}
# a FULL JOIN whose ON clause does not equate the keys is never shipped: the statements of this list, when they are shipped, may
# only be the ones that equate them
for my $sql (
	q{SELECT s1.id, s2.id FROM public.s1 FULL JOIN public.s2 ON s2.c = s1.a},
	q{SELECT s1.id, s2.id FROM public.s1 FULL JOIN public.s2 ON s2.c = s1.a AND s2.d = s1.b},
	q{SELECT count(*) FROM public.s1 FULL JOIN public.s2 ON s2.c > s1.a})
{
	not_shipped_ok(plan_of($n1, $sql), "a FULL JOIN that does not equate the keys is not shipped: $sql");
}

# ---------------------------------------------------------------------------
# B. windows
# ---------------------------------------------------------------------------
my @win = (
	[ 'partitioned by the key', q{SELECT id, a, row_number() OVER (PARTITION BY id ORDER BY a) FROM public.s1 WHERE id < 40} ],
	[ 'partitioned by another column', q{SELECT id, a, sum(b) OVER (PARTITION BY a) FROM public.s1 WHERE id < 40} ],
	[ 'no partition', q{SELECT id, rank() OVER (ORDER BY a, id) FROM public.s1 WHERE id < 40} ],
	[ 'over a grouping by the key', q{SELECT id, count(*), rank() OVER (ORDER BY count(*), id) FROM public.s1 WHERE id < 200 GROUP BY id} ],
);
for my $q (@win)
{
	my ($name, $sql) = @$q;
	$c->compare("window $name", $sql, ordered => 0, plans => ['pushed', 'scan'], profiles => 0, normalize => 1);
	not_shipped_ok(plan_of($n1, $sql), "window $name: is not shipped");
}

# ---------------------------------------------------------------------------
# C. sub-selects and views
# ---------------------------------------------------------------------------
$c->on_every_node(q{CREATE VIEW public.v1 AS SELECT id, a FROM public.s1 WHERE a > 3});
$c->on_single(q{CREATE VIEW public.v1 AS SELECT id, a FROM public.s1 WHERE a > 3});
my @sub = (
	[ 'in the WHERE clause', q{SELECT s1.id, count(*) FROM public.s1 WHERE s1.a IN (SELECT c FROM public.s2 WHERE d = 1) GROUP BY s1.id} ],
	[ 'a scalar one in the list', q{SELECT s1.id, (SELECT max(c) FROM public.s2 WHERE s2.id = s1.id) FROM public.s1 WHERE s1.id < 50} ],
	[ 'EXISTS', q{SELECT s1.id, count(*) FROM public.s1 WHERE EXISTS (SELECT 1 FROM public.s2 WHERE s2.id = s1.id) GROUP BY s1.id} ],
	[ 'in FROM', q{SELECT x.id, count(*) FROM (SELECT id, a FROM public.s1) x GROUP BY x.id} ],
	[ 'a view', q{SELECT id, count(*) FROM public.v1 GROUP BY id} ],
	[ 'a view joined on the key', q{SELECT v1.id, s2.c FROM public.v1 JOIN public.s2 ON s2.id = v1.id} ],
);
for my $q (@sub)
{
	my ($name, $sql) = @$q;
	$c->compare("sub-select $name", $sql, ordered => 0, plans => ['pushed', 'scan'], profiles => 0, normalize => 1);
	not_shipped_ok(plan_of($n1, $sql), "sub-select $name: is not shipped");
}

done_testing();
