# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/135_discard_caches.pl
#
# debug_discard_caches = 1 (PostgreSQL's own setting for this; the builds of the tests have assertions on)
# throws away every cache at every lock acquisition and every catalog access.  A server that has
# the extension must run under it exactly as it runs without it: the same answers, no crash.  It did not:
# code held a pointer into the catalog cache of the extension (a node, a distributed table, a
# colocation group) across a call that locks, the cache was loaded again there and freed, and the
# next use read freed memory (add_node, the row router).  The old cache is now kept until the end
# of the transaction, and the planner analyses again when the cache changed under it.
# The whole cluster runs with the setting (so do the control sessions between the nodes).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @n = make_cluster('dc', 3, extra => "debug_discard_caches = 1\n");
my $n1 = $n[0];
is($n1->safe_psql('postgres', 'SHOW debug_discard_caches'), '1', 'the nodes run with debug_discard_caches (the builds of the tests have assertions)')
  or diag('this build has no debug_discard_caches');

# a node that joined under the setting is a member: the later steps need all three
is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), 3, 'add_node under the setting registered every node');

sub sql_ok
{
	my ($sql, $want, $label) = @_;
	my ($rc, $o, $e) = run_sql($n1, $sql);
	is($rc, 0, "$label: no error and no crash") or diag($e);
	is($o, $want, "$label: the answer") if defined $want;
	return $o;
}

sql_ok(q{CREATE TABLE public.t (id int PRIMARY KEY, a int)}, undef, 'create the table');
sql_ok(q{SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 6)}, undef, 'distribute it');
sql_ok(q{CREATE TABLE public.u (id int PRIMARY KEY, a int)}, undef, 'create the second table');
sql_ok(q{SELECT kingres.create_distributed_table('public.u', 'id', colocate_with => 'public.t')}, undef, 'colocate it');
sql_ok(q{INSERT INTO public.t SELECT g, g % 5 FROM generate_series(1, 100) g}, '', 'a multi-row INSERT (the row router)');
sql_ok(q{INSERT INTO public.u SELECT id, a FROM public.t}, '', 'a colocated INSERT ... SELECT (shipped whole)');
sql_ok(q{SELECT count(*) FROM public.u}, 100, 'the rows are there');
sql_ok(q{SELECT count(*) FROM public.t WHERE id = 7}, 1, 'a single-key read');
sql_ok(q{SELECT a, count(*) FROM public.t GROUP BY a ORDER BY a}, "0|20\n1|20\n2|20\n3|20\n4|20", 'a scan with a GROUP BY');
sql_ok(q{SELECT id, count(*) FROM public.t GROUP BY id HAVING id < 4 ORDER BY id}, "1|1\n2|1\n3|1", 'a closed query (the fragment scan)');
sql_ok(q{SELECT count(*) FROM public.t x JOIN public.u y ON x.id = y.id}, 100, 'a join on the keys');
sql_ok(q{PREPARE p(int) AS SELECT count(*) FROM public.t WHERE id = $1; EXECUTE p(5); EXECUTE p(6)}, "1
1", 'a prepared statement');
sql_ok(q{UPDATE public.t SET a = a + 1 WHERE id = 3}, '', 'a single-key UPDATE');
sql_ok(q{DELETE FROM public.u WHERE id < 10}, '', 'a fan-out DELETE');
sql_ok(q{COPY public.t FROM PROGRAM 'echo 2000,1' WITH (FORMAT csv)}, '', 'COPY');
sql_ok(q{TRUNCATE public.u}, '', 'TRUNCATE');
sql_ok(q{SELECT count(*) FROM public.t}, 101, 'the table holds what was written');
done_testing();
