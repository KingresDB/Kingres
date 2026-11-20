# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/153_ordered_merge.pl
#
# Step 9 of phase 2b (design/shard-phase2b.md, 8): the sort and the limit of a closed query go to the groups.  Every group
# returns its rows in the final order, the node that runs the statement merges the sorted streams with a SortSupport per key
# (no Sort above the fragment scan), and a LIMIT of constants is sent as LIMIT count + offset; OFFSET stays at this node.
# The answer must be the single node's, in order, from every node, under the plan profiles, with the path assertion.
#
#   A. ORDER BY every combination of direction and NULLS FIRST/LAST for int, bigint, text, float (NaN, -0, infinities), date,
#      timestamptz, numeric, bool, and a column made of NULLs; several keys; ties (O2, O3)
#   B. LIMIT, OFFSET, both, LIMIT 0, LIMIT ALL, WITH TIES, a limit that is a parameter, an offset past the end (O4, O5)
#   C. what is not pushed: a collation that is not the default, the switch, a sort that is not on the output (the scan sorts)
#   D. a cursor closed early, EXPLAIN, a group that answers late

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('om');
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

# every key repeats (four rows), so that a grouping by it is a gain and the query is run by the groups
table(q{CREATE TABLE public.m (id int, c int, i int, bi bigint, t text, f float8, d date, ts timestamptz, n numeric(10,2), b bool, z int,
		PRIMARY KEY (id, c))},
	q{; SELECT kingres.create_distributed_table('public.m', 'id', bucket_count => 12)});
$c->on_both(q{INSERT INTO public.m
	SELECT g, c,
	  CASE WHEN (g * 7 + c) % 13 = 0 THEN NULL ELSE (g * 31 + c * 17) % 40 END,
	  CASE WHEN (g * 5 + c) % 11 = 0 THEN NULL ELSE (g * 1000003 + c)::bigint * 1000 END,
	  CASE WHEN (g + c) % 9 = 0 THEN NULL ELSE 'k' || ((g * 13 + c * 7) % 60) END,
	  CASE (g * 3 + c) % 9 WHEN 0 THEN 'NaN'::float8 WHEN 1 THEN '-0'::float8 WHEN 2 THEN '0'::float8 WHEN 3 THEN 'Infinity'::float8
	       WHEN 4 THEN '-Infinity'::float8 WHEN 5 THEN NULL ELSE ((g * 7 + c) % 50) / 4.0 END,
	  CASE WHEN (g + c * 3) % 10 = 0 THEN NULL ELSE date '2024-01-01' + ((g * 11 + c) % 120) END,
	  CASE WHEN (g + c * 5) % 12 = 0 THEN NULL ELSE timestamptz '2024-01-01 00:00:00+00' + (((g * 37 + c) % 300) || ' minutes')::interval END,
	  CASE WHEN (g * 3 + c) % 8 = 0 THEN NULL ELSE round(((g * 17 + c * 3) % 400) / 7.0, 2) END,
	  CASE WHEN (g + c) % 7 = 0 THEN NULL ELSE (g + c) % 2 = 0 END,
	  NULL
	FROM generate_series(1, 1200) g, generate_series(1, 4) c});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

my @PLANS = ('pushed', 'scan');

sub check_merge_plan
{
	my ($name, $sql, $pushed_limit) = @_;
	my $p = plan_of($n1, $sql);
	like($p, qr/\Q$FS\E/, "$name: run by the groups");
	like($p, qr/Merge keys: \d+/, "$name: the streams are merged");
	unlike($p, qr/->  Sort|^\s*Sort\b/m, "$name: nothing sorts the rows above the scan");
	my ($text) = $p =~ /Remote SQL: (.*)$/m;
	like($text, qr/ORDER BY/, "$name: the groups sort");
	if (defined $pushed_limit)
	{
		like($text, qr/LIMIT '$pushed_limit'::bigint/, "$name: the groups limit to $pushed_limit");
		unlike($text, qr/OFFSET/, "$name: and never offset");
	}
	else
	{
		unlike($text, qr/LIMIT/, "$name: no limit is sent");
	}
}

# ---------------------------------------------------------------------------
# A. every type, direction and nulls
# ---------------------------------------------------------------------------
for my $col (qw(i bi t d ts n b z))
{
	for my $dir ('ASC', 'DESC')
	{
		for my $nulls ('NULLS FIRST', 'NULLS LAST', '')
		{
			my $agg = $col eq 'b' ? "bool_or($col)" : "max($col)";
			my $sql = "SELECT id, $agg AS v FROM public.m GROUP BY id ORDER BY v $dir $nulls, id";
			$c->compare("order by max($col) $dir $nulls", $sql, ordered => 1, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, 'Merge keys' ]);
		}
	}
}
check_merge_plan('one key', q{SELECT id, max(i) AS v FROM public.m GROUP BY id ORDER BY v, id});
for my $q (
	q{SELECT id, max(t) AS a, max(i) AS b FROM public.m GROUP BY id ORDER BY a DESC NULLS LAST, b ASC NULLS FIRST, id},
	q{SELECT id, min(d) AS a, max(bi) AS b, count(*) AS cnt FROM public.m GROUP BY id ORDER BY cnt DESC, a, b DESC, id DESC},
	q{SELECT id, max(n) AS a FROM public.m GROUP BY id ORDER BY 2 DESC, 1 DESC},
	q{SELECT id, count(*), max(ts) FROM public.m GROUP BY id HAVING count(*) > 3 ORDER BY 3 NULLS FIRST, 1},
	q{SELECT id, max(i) FROM public.m GROUP BY id ORDER BY max(z) NULLS FIRST, 1})
{
	$c->compare("several keys: $q", $q, ordered => 1, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, 'Merge keys' ]);
}
# a key that sorts on a column the select list does not have: the junk column is sent and merged on
{
	my $sql = q{SELECT id FROM public.m GROUP BY id ORDER BY max(i) DESC NULLS LAST, id};
	$c->compare('junk sort column', $sql, ordered => 1, plans => \@PLANS, profiles => 0, path => [ $FS, 'Merge keys' ]);
}
# DISTINCT closed, ordered
{
	my $sql = q{SELECT DISTINCT id, c % 2 AS p FROM public.m ORDER BY p DESC, id DESC};
	$c->compare('distinct ordered', $sql, ordered => 1, plans => \@PLANS, profiles => 0, path => [ $FS, 'Merge keys' ]);
}
# a join of two distributed tables, ordered
{
	table(q{CREATE TABLE public.o (id int PRIMARY KEY, w int)}, q{; SELECT kingres.create_distributed_table('public.o', 'id', colocate_with => 'public.m')});
	$c->on_both(q{INSERT INTO public.o SELECT g, (g * 7) % 100 FROM generate_series(1, 1200) g});
	$c->on_every_node(q{ANALYZE});
	$c->on_single(q{ANALYZE});
	my $sql = q{SELECT m.id, m.c, o.w FROM public.m JOIN public.o ON o.id = m.id WHERE m.c = 2 ORDER BY o.w DESC, m.id};
	$c->compare('join ordered', $sql, ordered => 1, plans => \@PLANS, profiles => 0, path => [ $FS, 'Merge keys' ]);
	# floats (NaN, -0, infinities, NULL) as the sort key: a join, because a float aggregate is not shipped
	for my $dir ('ASC', 'DESC')
	{
		for my $nulls ('NULLS FIRST', 'NULLS LAST', '')
		{
			my $fs = "SELECT m.id, m.c, m.f FROM public.m JOIN public.o ON o.id = m.id WHERE m.c <= 2 ORDER BY m.f $dir $nulls, m.id, m.c";
			$c->compare("order by a float $dir $nulls", $fs, ordered => 1, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, 'Merge keys' ]);
		}
	}
}

# ---------------------------------------------------------------------------
# B. limits
# ---------------------------------------------------------------------------
my $BASE = q{SELECT id, max(i) AS v FROM public.m GROUP BY id ORDER BY v DESC NULLS LAST, id};
for my $q (
	[ 'LIMIT 7', "$BASE LIMIT 7", 7 ],
	[ 'LIMIT 7 OFFSET 20', "$BASE LIMIT 7 OFFSET 20", 27 ],
	[ 'OFFSET 1100', "$BASE OFFSET 1100", undef ],
	[ 'LIMIT 0', "$BASE LIMIT 0", 0 ],
	[ 'LIMIT 1', "$BASE LIMIT 1", 1 ],
	[ 'LIMIT 5000', "$BASE LIMIT 5000", 5000 ],
	[ 'OFFSET 5000', "$BASE OFFSET 5000", undef ],
	[ 'LIMIT ALL OFFSET 3', "$BASE LIMIT ALL OFFSET 3", undef ],
	[ 'LIMIT NULL', "$BASE LIMIT NULL", undef ],
	[ 'FETCH FIRST 7 ROWS WITH TIES', "$BASE FETCH FIRST 7 ROWS WITH TIES", undef ],
	[ 'FETCH FIRST 7 ROWS ONLY', "$BASE FETCH FIRST 7 ROWS ONLY", 7 ],
)
{
	my ($name, $sql, $pushed) = @$q;
	$c->compare("limit: $name", $sql, ordered => 1, plans => \@PLANS, profiles => 0, normalize => 1, path => [ $FS, 'Merge keys' ]);
	my $p = plan_of($n1, $sql);
	my ($text) = $p =~ /Remote SQL: (.*)$/m;
	if (defined $pushed)
	{
		like($text, qr/LIMIT '$pushed'::bigint/, "limit: $name: the groups limit to $pushed");
		unlike($text, qr/OFFSET/, "limit: $name: and never offset");
	}
	else
	{
		unlike($text, qr/LIMIT/, "limit: $name: no limit is sent");
	}
}
{
	# the limit that is a parameter is computed when the scan starts and sent as a parameter (step 10 of the round 2b10); the merge too
	my $G = 'SET plan_cache_mode = force_generic_plan; ';
	my $pre = qq{PREPARE lp(int) AS $BASE LIMIT \$1; };
	my ($rs, $os) = run_sql($single, $pre . 'EXECUTE lp(9)');
	for my $nd (@nodes)
	{
		my ($rc, $out, $err) = run_sql($nd, $G . $pre . 'EXECUTE lp(9)');
		is($rc, 0, 'a limit that is a parameter runs on ' . $nd->name) or diag($err);
		is($out, $os, '... and gives the single node\'s rows');
	}
	my ($rc, $plan) = run_sql($n1, $G . $pre . 'EXPLAIN (COSTS OFF) EXECUTE lp(9)');
	like($plan, qr/Merge keys/, '... with the streams merged');
	my ($text) = $plan =~ /Remote SQL: (.*)$/m;
	like($text, qr/LIMIT \$\d+$/, '... and the groups limit by a parameter that this node computed');
	# a negative count is the error of the single node
	my ($rn, $on, $en) = run_sql($n1, $G . $pre . 'EXECUTE lp(-1)');
	isnt($rn, 0, 'a negative limit is an error');
	like($en, qr/LIMIT must not be negative/, '... the single node\'s');
}

# ---------------------------------------------------------------------------
# C. what is not pushed
# ---------------------------------------------------------------------------
{
	my $sql = q{SELECT id, max(t) COLLATE "POSIX" AS v FROM public.m GROUP BY id ORDER BY v, id LIMIT 10};
	my $p = plan_of($n1, $sql);
	like($p, qr/Sort/, 'a collation that is not the default or C: this node sorts');
	unlike($p, qr/Merge keys/, '... and does not merge');
	$c->compare('collation POSIX', $sql, ordered => 1, plans => \@PLANS, profiles => 0);

	my $off = plan_of($n1, $BASE . ' LIMIT 10', 'SET kingres_shard.enable_pushdown_sort = off; ');
	like($off, qr/Sort/, 'enable_pushdown_sort = off: this node sorts');
	unlike($off, qr/Merge keys/, '... and does not merge');
	my ($text) = $off =~ /Remote SQL: (.*)$/m;
	unlike($text, qr/ORDER BY|LIMIT/, '... and the groups are not asked to');
	my ($r1, $o1) = run_sql($n1, 'SET kingres_shard.enable_pushdown_sort = off; ' . $BASE . ' LIMIT 10');
	my ($r2, $o2) = run_sql($single, $BASE . ' LIMIT 10');
	is($o1, $o2, '... and the answer is the same');
}

# ---------------------------------------------------------------------------
# D. a cursor closed early, EXPLAIN, a late group
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, qq{BEGIN; DECLARE cur NO SCROLL CURSOR FOR $BASE; FETCH 5 FROM cur; FETCH 4 FROM cur; CLOSE cur; COMMIT});
	is($rc, 0, 'a cursor over an ordered merge, closed early') or diag($err);
	my ($rs, $os) = run_sql($single, "$BASE LIMIT 9");
	is($out, $os, '... gives the first rows of the single node\'s answer');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM pg_cursors}), 0, '... and leaves no cursor');

	my $p = plan_of($n1, "$BASE LIMIT 5");
	like($p, qr/Limit\n\s+->  Custom Scan \(Kingres Fragment Scan\)/, 'EXPLAIN: the limit of this node is above the merge');
	like($p, qr/Merge keys: 2/, '... which says how many keys it merges on');

	# a group that is held up while the others have rows: the merge waits for it and the answer is the same
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_om";
	append_to_file($gate, 'x');
	my $h = IPC::Run::start([ 'psql', '-XAt', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres', '-c',
		"SET kingres_shard.scan_fetch_size = 7; SET kingres_shard.fault_injection = 'frag_wait_batch:wait:$gate'; $BASE LIMIT 30" ],
		'>', \my $hout, '2>', \my $herr);
	for (1 .. 200) { last if -e "$gate.reached"; select(undef, undef, undef, 0.05); }
	ok(-e "$gate.reached", 'the merge waits for a batch of a group');
	unlink $gate;
	$h->finish;
	my ($rs2, $os2) = run_sql($single, "$BASE LIMIT 30");
	$hout =~ s/\A(?:SET\n)+//;
	$hout =~ s/\n+\z//;
	is($hout, $os2, '... and gives the single node\'s rows when the group answers') or diag($herr);
}

done_testing();
