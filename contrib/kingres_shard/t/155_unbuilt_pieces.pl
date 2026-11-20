# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/155_unbuilt_pieces.pl
#
# Two pieces of steps 6 to 10 that the design asks for and that earlier rounds left out:
#
#   A. a LIMIT that is not a constant (design 8.2, O5).  A parameter, or an expression of parameters and of stable functions, is
#      computed by this node when the scan starts and sent as one more parameter, `LIMIT $n`, with count + offset; a NULL count, a
#      negative one and a sum that does not fit are sent as NULL (no limit: this node's own Limit node then says what the single
#      node says).  The answer is the single node's, ordered, from every node, with prepared statements, generic plans and cursors.
#   B. a window function and DISTINCT above a partial aggregation (design 7.6, A4 and A6): the text of the groups has neither, this
#      node applies them above the final aggregate.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('up', extra => "kingres_shard.enable_partial_aggregation = on\n");
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

table(q{CREATE TABLE public.m (id int, c int, i int, t text, PRIMARY KEY (id, c))},
	q{; SELECT kingres.create_distributed_table('public.m', 'id', bucket_count => 12)});
$c->on_both(q{INSERT INTO public.m SELECT g, c, (g * 31 + c * 17) % 40, 'k' || ((g * 13 + c) % 60) FROM generate_series(1, 1200) g, generate_series(1, 4) c});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# ---------------------------------------------------------------------------
# A. a LIMIT that is computed when the scan starts
# ---------------------------------------------------------------------------
my $BASE = q{SELECT id, max(i) AS v FROM public.m GROUP BY id ORDER BY v DESC NULLS LAST, id};
{
	my $G = 'SET plan_cache_mode = force_generic_plan; ';
	my $C = 'SET plan_cache_mode = force_custom_plan; ';
	my @cases = (
		[ 'LIMIT $1', "PREPARE lp(int) AS $BASE LIMIT \$1;", [ 'EXECUTE lp(9)', 'EXECUTE lp(0)', 'EXECUTE lp(5000)', 'EXECUTE lp(NULL)' ] ],
		[ 'LIMIT $1 OFFSET $2', "PREPARE lo(int, int) AS $BASE LIMIT \$1 OFFSET \$2;", [ 'EXECUTE lo(7, 20)', 'EXECUTE lo(7, 0)', 'EXECUTE lo(0, 5)', 'EXECUTE lo(NULL, 5)', 'EXECUTE lo(7, NULL)', 'EXECUTE lo(5000, 5000)' ] ],
		[ 'LIMIT 7 OFFSET $1', "PREPARE co(int) AS $BASE LIMIT 7 OFFSET \$1;", [ 'EXECUTE co(30)', 'EXECUTE co(1190)' ] ],
		[ 'LIMIT $1 + 1', "PREPARE le(int) AS $BASE LIMIT \$1 + 1;", [ 'EXECUTE le(6)' ] ],
		[ 'a bigint parameter', "PREPARE lb(bigint) AS $BASE LIMIT \$1;", [ 'EXECUTE lb(11)', 'EXECUTE lb(9223372036854775807)' ] ],
	);
	for my $case (@cases)
	{
		my ($name, $pre, $execs) = @$case;
		for my $mode ($C, $G)
		{
			for my $exec (@$execs)
			{
				my ($rs, $os, $es) = run_sql($single, $pre . ' ' . $exec);
				for my $nd (@nodes)
				{
					my ($rc, $out, $err) = run_sql($nd, $mode . $pre . ' ' . $exec);
					is($rc, 0, "$name: $exec runs on " . $nd->name . ($mode eq $G ? ' (generic plan)' : '')) or diag($err);
					is($out, $os, "$name: $exec gives the single node's rows") or diag($err);
				}
			}
		}
		my ($rc, $plan) = run_sql($n1, $G . $pre . ' EXPLAIN (COSTS OFF) ' . $execs->[0]);
		like($plan, qr/Merge keys/, "$name: the streams are merged");
		my ($text) = $plan =~ /Remote SQL: (.*)$/m;
		like($text, qr/LIMIT \$\d+$/, "$name: the groups limit by a parameter");
	}

	# a negative limit is the error of the single node, said by this node and not by a group
	my ($rn, $on, $en) = run_sql($n1, $G . "PREPARE ln(int) AS $BASE LIMIT \$1; EXECUTE ln(-1)");
	isnt($rn, 0, 'a negative limit is an error');
	like($en, qr/LIMIT must not be negative/, '... the single node\'s');
	unlike($en, qr/remote node/, '... said by this node, not by a group');
	($rn, $on, $en) = run_sql($n1, $G . "PREPARE lm(int, int) AS $BASE LIMIT \$1 OFFSET \$2; EXECUTE lm(5, -1)");
	like($en, qr/OFFSET must not be negative/, 'a negative offset is the error of the single node too');
	unlike($en, qr/remote node/, '... said by this node');

	# an expression of stable functions: computed when the scan starts
	my $pre = q{SET ks.lim = '13'; };
	my $sql = "$BASE LIMIT current_setting('ks.lim')::int";
	my ($rs, $os) = run_sql($single, $pre . $sql);
	for my $nd (@nodes)
	{
		my ($rc, $out, $err) = run_sql($nd, $pre . $sql);
		is($rc, 0, 'LIMIT of a stable function runs on ' . $nd->name) or diag($err);
		is($out, $os, '... and gives the single node\'s rows');
	}
	like(plan_of($n1, $sql, $pre), qr/LIMIT \$\d+/, '... with the limit computed by this node and sent as a parameter');

	# what is not computed here: a sub-select, a column
	my $sub = "$BASE LIMIT (SELECT 5)";
	unlike((run_sql($n1, "EXPLAIN (COSTS OFF) $sub"))[1], qr/LIMIT \$\d+/, 'a LIMIT with a sub-select is not computed at the start');
	$c->compare('a LIMIT with a sub-select', $sub, ordered => 1, plans => ['pushed', 'scan'], profiles => 0);

	# cursors, EXPLAIN ANALYZE and the switch
	my ($rc, $out, $err) = run_sql($n1, "SET ks.lim = '25'; BEGIN; DECLARE cur NO SCROLL CURSOR FOR $BASE LIMIT current_setting('ks.lim')::int; FETCH 4 FROM cur; FETCH 3 FROM cur; CLOSE cur; COMMIT");
	my ($rs2, $os2) = run_sql($single, "$BASE LIMIT 7");
	is($rc, 0, 'a cursor over a limit computed at the start') or diag($err);
	is($out, $os2, '... gives the first rows of the single node\'s answer');
	# the difference from a single node that the README names: a cursor with neither SCROLL nor NO SCROLL over a pushed ordered
	# statement cannot scan backward; a SCROLL cursor can and gives the single node's rows
	($rc, $out, $err) = run_sql($n1, "BEGIN; DECLARE cb CURSOR FOR $BASE LIMIT 20; FETCH 5 FROM cb; FETCH BACKWARD 2 FROM cb; COMMIT");
	isnt($rc, 0, 'a cursor with neither SCROLL nor NO SCROLL over a pushed ordered statement cannot scan backward');
	like($err, qr/cursor can only scan forward/, '... the error says so');
	($rc, $out, $err) = run_sql($n1, "BEGIN; DECLARE cs SCROLL CURSOR FOR $BASE LIMIT 20; FETCH 5 FROM cs; FETCH BACKWARD 2 FROM cs; COMMIT");
	is($rc, 0, 'a SCROLL cursor over the same statement scans backward') or diag($err);
	my ($rs4, $os4) = run_sql($single, "BEGIN; DECLARE cs SCROLL CURSOR FOR $BASE LIMIT 20; FETCH 5 FROM cs; FETCH BACKWARD 2 FROM cs; COMMIT");
	is($out, $os4, '... and gives the single node\'s rows');
	($rc, $out, $err) = run_sql($single, "BEGIN; DECLARE cb CURSOR FOR $BASE LIMIT 20; FETCH 5 FROM cb; FETCH BACKWARD 2 FROM cb; COMMIT");
	is($rc, 0, 'on a single node the same cursor can scan backward (the difference)') or diag($err);
	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_pushdown_sort = off; $G PREPARE lq(int) AS $BASE LIMIT \$1; EXECUTE lq(9)");
	my ($rs3, $os3) = run_sql($single, "$BASE LIMIT 9");
	is($out, $os3, 'enable_pushdown_sort = off: the answer is the same');
	my ($rx, $ox, $ex) = run_sql($n1, $G . "PREPARE la(int) AS $BASE LIMIT \$1; EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) EXECUTE la(9)");
	is($rx, 0, 'EXPLAIN ANALYZE of a limit by parameter runs') or diag($ex);
	like($ox, qr/Rows from groups:/, '... and shows the rows of the groups');
}

# ---------------------------------------------------------------------------
# B. a window function and DISTINCT above a partial aggregation
# ---------------------------------------------------------------------------
my @above = (
	[ 'a window above the aggregate', q{SELECT i % 4, count(*), rank() OVER (ORDER BY count(*), i % 4) FROM public.m GROUP BY 1 ORDER BY 1}, 1 ],
	[ 'a window partitioned by a grouping column', q{SELECT i % 5, t, count(*), sum(count(*)) OVER (PARTITION BY i % 5) FROM public.m WHERE i < 20 GROUP BY 1, 2 ORDER BY 1, 2}, 1 ],
	[ 'DISTINCT above the aggregate', q{SELECT DISTINCT count(*) FROM public.m GROUP BY i}, 0 ],
	[ 'DISTINCT of a grouping column and an aggregate', q{SELECT DISTINCT i % 3, max(c) FROM public.m GROUP BY i ORDER BY 1, 2}, 1 ],
	[ 'DISTINCT ON above the aggregate', q{SELECT DISTINCT ON (n) n, i FROM (SELECT i, count(*) AS n FROM public.m GROUP BY i) q ORDER BY n, i}, 1, 1 ],
	[ 'a window and a limit', q{SELECT i, count(*), row_number() OVER (ORDER BY count(*) DESC, i) AS rn FROM public.m GROUP BY i ORDER BY rn LIMIT 11}, 1 ],
);
for my $q (@above)
{
	my ($name, $sql, $ordered, $nopath) = @$q;
	$c->compare("above: $name", $sql, ordered => $ordered, plans => ['pushed', 'nopartial', 'scan'], profiles => 0, normalize => 1,
		($nopath ? () : (path => [ $FS, 'Finalize' ])));
}
{
	my $p = plan_of($n1, q{SELECT i % 4, count(*), rank() OVER (ORDER BY count(*), i % 4) FROM public.m GROUP BY 1});
	my ($text) = $p =~ /Remote SQL: (.*)$/m;
	unlike($text, qr/OVER|DISTINCT/, 'the text of the groups has no window and no DISTINCT');
	like($p, qr/WindowAgg/, '... this node applies the window above the final aggregate');

	# a named window is printed by the deparser as a WINDOW clause of its own: the text of the groups must not have it
	my $pn = plan_of($n1, q{SELECT i % 4, count(*), rank() OVER w FROM public.m GROUP BY 1 WINDOW w AS (ORDER BY count(*), i % 4)});
	my ($textn) = $pn =~ /Remote SQL: (.*)$/m;
	ok(defined $textn, 'a named window: the aggregation is pushed');
	unlike($textn // '', qr/WINDOW|OVER/, '... and the text of the groups has no WINDOW clause');
	$c->compare('above: a named window', q{SELECT i % 4, count(*), rank() OVER w FROM public.m GROUP BY 1 WINDOW w AS (ORDER BY count(*), i % 4) ORDER BY 1},
		ordered => 1, plans => ['pushed', 'nopartial', 'scan'], profiles => 0, normalize => 1);
}

done_testing();
