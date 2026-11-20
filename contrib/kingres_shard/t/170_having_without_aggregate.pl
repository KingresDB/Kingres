# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/170_having_without_aggregate.pl
#
# Review P2E-01 (critical): a HAVING clause that has no aggregate is dropped by the partial aggregation.  subquery_planner moves
# such a conjunct from HAVING into WHERE; the text that the groups receive is made from the statement before the planner ran, so it
# had neither the conjunct in HAVING nor in WHERE, and the final aggregation of this node (which applies what is left in HAVING)
# did not have it either: `SELECT c, count(*) FROM m GROUP BY c HAVING c < 3` returned every group.
#
# Every form of HAVING must give the single node's answer, on every path that can carry it: the partial aggregation (a statement
# that is not closed), the closed query (the text keeps its HAVING) and a join (the rows of a join below a grouping, whose HAVING
# this node applies).  The partial aggregation is not used when the HAVING has a conjunct that the planner moves; the statement is
# then answered by the scan, and the test says so by the plan.  What stays in HAVING (an aggregate, a volatile function, a
# sub-select, also in an OR with the others) is applied by the final aggregation of this node, and the partial aggregation is
# still used.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('hv', extra => "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_join = on\n");
my ($n1, $n2, $n3) = $c->nodes;
my @nodes = ($n1, $n2, $n3);
my $single = $c->single;
my $FS = 'Kingres Fragment Scan';

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

table(q{CREATE TABLE public.m (id int, c int, bits int, PRIMARY KEY (id, c))},
	q{; SELECT kingres.create_distributed_table('public.m', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.m2 (id int, c int, x int, PRIMARY KEY (id, c))},
	q{; SELECT kingres.create_distributed_table('public.m2', 'id', colocate_with => 'public.m')});
$c->on_both(q{INSERT INTO public.m SELECT g, c, (g * 7 + c) % 10 FROM generate_series(1, 300) g, generate_series(1, 4) c});
$c->on_both(q{INSERT INTO public.m2 SELECT g, c, (g * 3) % 11 FROM generate_series(1, 300) g, generate_series(1, 2) c});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# [ name, statement, ordered, what the plan must say: 'partial' (the partial aggregation), 'closed', 'scan' (neither) or undef ]
my @cases = (
	# a condition without an aggregate: the planner moves it into WHERE
	[ 'a grouping column in HAVING',                    q{SELECT c, count(*) FROM public.m GROUP BY c HAVING c < 3 ORDER BY c}, 1, 'scan' ],
	[ 'the same without the column in the list',        q{SELECT count(*) FROM public.m GROUP BY c HAVING c < 3}, 0, 'scan' ],
	[ 'an expression of the grouping column',           q{SELECT c + 1, count(*) FROM public.m GROUP BY c HAVING c + 1 < 4 ORDER BY 1}, 1, 'scan' ],
	[ 'a column that the HAVING makes a constant',      q{SELECT c, bits % 2 AS p, count(*) FROM public.m GROUP BY c, bits % 2 HAVING c = 2 ORDER BY 1, 2}, 1, 'scan' ],
	[ 'a grouping expression',                          q{SELECT bits % 2, count(*) FROM public.m GROUP BY bits % 2 HAVING bits % 2 = 0}, 0, 'scan' ],
	[ 'a window above the grouping',                    q{SELECT id, sum(count(*)) OVER (ORDER BY id) FROM public.m GROUP BY id HAVING id < 6 ORDER BY id}, 1, 'scan' ],
	[ 'a condition and an aggregate (AND)',             q{SELECT c, count(*) FROM public.m GROUP BY c HAVING c < 4 AND count(*) > 5 ORDER BY c}, 1, 'scan' ],
	[ 'an aggregate first, then a condition (AND)',      q{SELECT c, count(*) FROM public.m GROUP BY c HAVING count(*) > 5 AND c < 4 ORDER BY c}, 1, 'scan' ],
	[ 'two conditions',                                 q{SELECT c, count(*) FROM public.m GROUP BY c HAVING c > 1 AND c < 4 ORDER BY c}, 1, 'scan' ],
	[ 'a condition that is false',                      q{SELECT c, count(*) FROM public.m GROUP BY c HAVING false}, 0, 'scan' ],
	[ 'a condition that is NULL',                       q{SELECT c, count(*) FROM public.m GROUP BY c HAVING NULL::bool}, 0, 'scan' ],
	[ 'a stable function of the grouping column',       q{SELECT c, count(*) FROM public.m GROUP BY c HAVING abs(c - 2) < 2 ORDER BY c}, 1, 'scan' ],
	[ 'a condition by IN',                              q{SELECT c, count(*) FROM public.m GROUP BY c HAVING c IN (1, 3) ORDER BY c}, 1, 'scan' ],
	# what stays in HAVING: the partial aggregation applies it above the final aggregate
	[ 'an aggregate',                                   q{SELECT c, count(*) FROM public.m GROUP BY c HAVING count(*) > 5 ORDER BY c}, 1, 'partial' ],
	[ 'an aggregate that fails every group',            q{SELECT c, count(*) FROM public.m GROUP BY c HAVING count(*) > 100000 ORDER BY c}, 1, 'partial' ],
	[ 'an aggregate not in the list',                   q{SELECT c, count(*) FROM public.m GROUP BY c HAVING max(bits) = 9 AND sum(id) > 0 ORDER BY c}, 1, 'partial' ],
	[ 'OR between a condition and an aggregate',        q{SELECT c, count(*) FROM public.m GROUP BY c HAVING c < 2 OR count(*) > 100000 ORDER BY c}, 1, 'partial' ],
	[ 'OR, the aggregate side true',                    q{SELECT c, count(*) FROM public.m GROUP BY c HAVING c = 1 OR sum(bits) >= 0 ORDER BY c}, 1, 'partial' ],
	[ 'HAVING true',                                    q{SELECT c, count(*) FROM public.m GROUP BY c HAVING true ORDER BY c}, 1, 'partial' ],
	[ 'a volatile function (always true)',              q{SELECT c, count(*) FROM public.m GROUP BY c HAVING random() < 2 ORDER BY c}, 1, 'partial' ],
	[ 'a volatile function and an aggregate',           q{SELECT c, count(*) FROM public.m GROUP BY c HAVING random() < 2 AND count(*) > 5 ORDER BY c}, 1, 'partial' ],
	[ 'a sub-select',                                   q{SELECT c, count(*) FROM public.m GROUP BY c HAVING c < (SELECT 3) ORDER BY c}, 1, undef ],
	# a scalar aggregate with HAVING
	[ 'a scalar aggregate and an aggregate',            q{SELECT count(*) FROM public.m HAVING count(*) > 5}, 0, 'partial' ],
	[ 'a scalar aggregate and a false constant',        q{SELECT count(*) FROM public.m HAVING false}, 0, 'scan' ],
	# the closed query keeps its HAVING in the text
	[ 'closed: the key',                                q{SELECT id, count(*) FROM public.m GROUP BY id HAVING id % 5 = 0 ORDER BY id}, 1, undef ],
	[ 'closed: the key and an aggregate',               q{SELECT id, count(*) FROM public.m GROUP BY id HAVING id % 2 = 0 AND count(*) > 3 AND id < 20 ORDER BY id}, 1, 'closed' ],
	[ 'closed: OR',                                     q{SELECT id, count(*) FROM public.m GROUP BY id HAVING id < 3 OR count(*) > 100 ORDER BY id}, 1, 'closed' ],
	[ 'closed: a constant', q{SELECT id, count(*) FROM public.m GROUP BY id HAVING false}, 0, undef ],
	# a join below the grouping
	[ 'a join: a condition',                            q{SELECT m.c, count(*) FROM public.m JOIN public.m2 ON m2.id = m.id GROUP BY m.c HAVING m.c < 3 ORDER BY 1}, 1, undef ],
	[ 'a join: an aggregate',                           q{SELECT m.c, count(*), max(m2.x) FROM public.m JOIN public.m2 ON m2.id = m.id GROUP BY m.c HAVING max(m2.x) > 5 ORDER BY 1}, 1, undef ],
	[ 'a join: a window above the grouping',            q{SELECT m.c, count(*), rank() OVER (ORDER BY count(*), m.c) FROM public.m JOIN public.m2 ON m2.id = m.id GROUP BY m.c HAVING m.c > 1 ORDER BY 1}, 1, undef ],
);
for my $q (@cases)
{
	my ($name, $sql, $ordered, $path) = @$q;
	$c->compare("having: $name", $sql, ordered => $ordered, plans => ['pushed', 'nopartial', 'scan'], profiles => 0, normalize => 1);
	next unless defined $path;
	my $plan = plan_of($n1, $sql);
	if ($path eq 'partial')
	{
		like($plan, qr/Finalize/, "having: $name: the partial aggregation is used");
	}
	elsif ($path eq 'closed')
	{
		like($plan, qr/\Q$FS\E/, "having: $name: the closed query is shipped");
		unlike($plan, qr/Finalize/, '... and not as a partial aggregation');
	}
	else
	{
		unlike($plan, qr/Finalize|\Q$FS\E/, "having: $name: not pushed (a condition that the planner moves out of HAVING): the scan answers");
	}
}

# prepared statements: a parameter in HAVING, custom and generic plans, from every node
{
	my $G = 'SET plan_cache_mode = force_generic_plan; ';
	my $C = 'SET plan_cache_mode = force_custom_plan; ';
	for my $stmt (
		q{PREPARE h(int) AS SELECT c, count(*) FROM public.m GROUP BY c HAVING c < $1 ORDER BY c},
		q{PREPARE h(int) AS SELECT c, count(*) FROM public.m GROUP BY c HAVING c < $1 AND count(*) > 5 ORDER BY c},
		q{PREPARE h(int) AS SELECT c, count(*) FROM public.m GROUP BY c HAVING c < 2 OR count(*) > $1 ORDER BY c},
		q{PREPARE h(int) AS SELECT id, count(*) FROM public.m GROUP BY id HAVING id < $1 ORDER BY id})
	{
		for my $arg (0, 3, 5, 100000)
		{
			my ($rs, $os, $es) = run_sql($single, "$stmt; EXECUTE h($arg)");
			for my $nd (@nodes)
			{
				for my $mode ($C, $G)
				{
					my ($rc, $out, $err) = run_sql($nd, "$mode$stmt; EXECUTE h($arg)");
					is($out, $os, "prepared: $stmt: EXECUTE h($arg) on " . $nd->name . ($mode eq $G ? ' (generic plan)' : '') . " gives the single node's rows") or diag($err);
				}
			}
		}
	}
}

# the switch: with the partial aggregation off the answer is the same
{
	my $sql = q{SELECT c, count(*) FROM public.m GROUP BY c HAVING c < 3 ORDER BY c};
	my ($rs, $os) = run_sql($single, $sql);
	my ($rc, $out) = run_sql($n2, "SET kingres_shard.enable_partial_aggregation = off; $sql");
	is($out, $os, 'enable_partial_aggregation = off: the same answer');
	is($os, "1|300\n2|300", 'and the answer is the two groups of the single node');
}

done_testing();
