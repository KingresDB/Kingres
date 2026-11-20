# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/151_sketch_pushdown.pl
#
# Step 8 of phase 2b (design/shard-phase2b.md, 5.5): the functions, types and aggregates of an extension on the list
# that is compiled into kingres_shard (today kingres_sketch, version 1.0) are shipped to the groups, as partial aggregates
# and, when the query is closed, whole.  The statement carries the extension and its version in the marker
# (/* kingres:partial-fragment place=<n> ext=kingres_sketch:1.0 */); a group that has another version, or none, answers KS001
# with a detail that says it is not a retry before the upgrade is finished, and a group does not accept a statement
# that uses an extension its marker does not name.  The setting kingres_shard.shippable_extensions narrows the list.
#
#   A. kingres_sketch aggregates as partial aggregates and as closed queries: the answers are the single node's (the
#      merge of a HyperLogLog is exact) or within the error of the sketch (t-digest), with the path assertion;
#   B. what is not shipped: the setting none, a version that is not on the list, a function of a user;
#   C. the setting: names, narrowing, an error for a name that is not on the list;
#   D. forged and mismatched fragments: another version on a group, a marker without the extension, a marker that
#      names an extension the statement does not use, an extension that is not installed.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('sk', extra => "kingres_shard.enable_partial_aggregation = on\n");
my ($n1, $n2, $n3) = $c->nodes;
my @nodes = ($n1, $n2, $n3);
my $FS = 'Kingres Fragment Scan';
my $K = 'kingres_sketch';

$c->on_every_node(q{CREATE EXTENSION kingres_sketch});
$c->on_single(q{CREATE EXTENSION kingres_sketch});

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

table(q{CREATE TABLE public.s (id int PRIMARY KEY, g int, v text, x float8)},
	q{; SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.s2 (id int PRIMARY KEY, w int)},
	q{; SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s')});
$c->on_both(q{INSERT INTO public.s SELECT g, g % 7, 'v' || (g % 311), (g % 1000) / 10.0 FROM generate_series(1, 6000) g});
$c->on_both(q{INSERT INTO public.s2 SELECT g, g % 5 FROM generate_series(1, 6000) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# ---------------------------------------------------------------------------
# A. shipped
# ---------------------------------------------------------------------------
my @shipped = (
	[ 'approx_count_distinct, grouped', qq{SELECT g, $K.approx_count_distinct(v) FROM public.s GROUP BY g ORDER BY g}, 1 ],
	[ 'approx_count_distinct, scalar', qq{SELECT $K.approx_count_distinct(v), $K.approx_count_distinct(id) FROM public.s}, 0 ],
	[ 'approx_count_distinct with a precision', qq{SELECT g, $K.approx_count_distinct(v, 12) FROM public.s GROUP BY g ORDER BY g}, 1 ],
	[ 'a sketch as a value, read by a function at this node', qq{SELECT g, $K.hll_cardinality($K.hll_add_agg(v)) FROM public.s GROUP BY g ORDER BY g}, 1 ],
	[ 'hll_precision of a sketch', qq{SELECT $K.hll_precision($K.hll_add_agg(v)) FROM public.s}, 0 ],
	[ 'topn: the total is exact', qq{SELECT g, $K.topn_total($K.topn_add_agg(v)) FROM public.s GROUP BY g ORDER BY g}, 1 ],
	[ 't-digest: the count is exact', qq{SELECT g, $K.tdigest_count($K.tdigest_agg(x)) FROM public.s GROUP BY g ORDER BY g}, 1 ],
	[ 't-digest: min and max are exact', qq{SELECT $K.tdigest_min($K.tdigest_agg(x)), $K.tdigest_max($K.tdigest_agg(x)) FROM public.s}, 0 ],
	[ 'sketches and ordinary aggregates together', qq{SELECT g, count(*), sum(id), $K.approx_count_distinct(v) FROM public.s GROUP BY g ORDER BY g}, 1 ],
	[ 'with HAVING on the sketch', qq{SELECT g FROM public.s GROUP BY g HAVING $K.approx_count_distinct(v) > 300 ORDER BY g}, 1 ],
	[ 'with a restriction', qq{SELECT g, $K.approx_count_distinct(v) FROM public.s WHERE x > 20 GROUP BY g ORDER BY g}, 1 ],
	[ 'a join of two distributed tables', qq{SELECT s2.w, $K.approx_count_distinct(s.v) FROM public.s JOIN public.s2 ON s2.id = s.id GROUP BY s2.w ORDER BY 1}, 1 ],
);
for my $q (@shipped)
{
	my ($name, $sql, $ordered) = @$q;
	$c->compare("sketch: $name", $sql, ordered => $ordered, plans => ['pushed', 'nopartial', 'scan'], profiles => 0, normalize => 1,
		path => [ $FS, 'Finalize' ]);
	like(plan_of($n1, $sql), qr/Remote SQL:/, "sketch: $name ships a text");
}

# the closed query: a grouping by the key of a table that repeats it (the sketch aggregate runs whole on the group)
{
	$c->on_cluster(q{CREATE TABLE public.dup (id int, k int, v text)});
	$c->on_single(q{CREATE TABLE public.dup (id int, k int, v text)});
	$c->on_cluster(q{SELECT kingres.create_distributed_table('public.dup', 'id', colocate_with => 'public.s')});
	$c->on_both(q{INSERT INTO public.dup SELECT g % 500, g, 'd' || (g % 97) FROM generate_series(1, 6000) g});
	$c->on_every_node(q{ANALYZE});
	$c->on_single(q{ANALYZE});
	my $sql = qq{SELECT id, $K.approx_count_distinct(v), count(*) FROM public.dup GROUP BY id ORDER BY id};
	$c->compare('sketch: a closed query', $sql, ordered => 1, plans => ['pushed', 'scan'], profiles => 0, normalize => 1, path => [ $FS ]);
	like(plan_of($n1, $sql), qr/Pushed: closed aggregate/, '... run whole by every group');
	like(plan_of($n1, qq{SELECT id, $K.hll_cardinality($K.hll_add_agg(v)) FROM public.dup GROUP BY id}), qr/closed aggregate/, '... with a sketch read by a function at this node');
}

# a rollup table with a column of the sketch type: it crosses as text, and hll_union_agg combines it on the groups
{
	$c->on_cluster(q{CREATE TABLE public.roll (id int PRIMARY KEY, g int, h kingres_sketch.hll)});
	$c->on_single(q{CREATE TABLE public.roll (id int PRIMARY KEY, g int, h kingres_sketch.hll)});
	$c->on_cluster(q{SELECT kingres.create_distributed_table('public.roll', 'id', colocate_with => 'public.s')});
	$c->on_both(qq{INSERT INTO public.roll SELECT id, g, $K.hll_add($K.hll_empty(), v) FROM public.s WHERE id <= 2000});
	$c->on_every_node(q{ANALYZE});
	$c->on_single(q{ANALYZE});
	my $sql = qq{SELECT g, $K.hll_cardinality($K.hll_union_agg(h)) FROM public.roll GROUP BY g ORDER BY g};
	$c->compare('sketch: a rollup column', $sql, ordered => 1, plans => ['pushed', 'scan'], profiles => 0, normalize => 1, path => [ $FS, 'Finalize' ]);
}

# the t-digest and the top-n merge are not exact: within the error of the sketch
{
	my $sql = qq{SELECT g, $K.approx_percentile(0.5, x), $K.approx_percentile(0.9, x) FROM public.s GROUP BY g ORDER BY g};
	like(plan_of($n1, $sql), qr/Finalize/, 'approx_percentile is a partial aggregation');
	my ($rc, $got, $e) = run_sql($n1, $sql);
	my ($rs, $want, $es) = run_sql($c->single, $sql);
	is($rc, 0, 'approx_percentile runs') or diag($e);
	my @g = split /\n/, $got;
	my @w = split /\n/, $want;
	is(scalar(@g), scalar(@w), '... and returns a row per group');
	my $worst = 0;
	for my $i (0 .. $#g)
	{
		my @a = split /\|/, $g[$i];
		my @b = split /\|/, $w[$i];
		for my $j (1, 2)
		{
			my $d = abs($a[$j] - $b[$j]) / (abs($b[$j]) + 1);
			$worst = $d if $d > $worst;
		}
	}
	cmp_ok($worst, '<', 0.03, "... within 3 percent of the single node's (worst $worst)");
	like(plan_of($n1, qq{SELECT g, $K.topn_total($K.topn_add_agg(v)) FROM public.s GROUP BY g}), qr/Finalize/, 'topn_add_agg is a partial aggregation');
}

# ---------------------------------------------------------------------------
# B. what is not shipped
# ---------------------------------------------------------------------------
{
	my $sql = qq{SELECT g, $K.approx_count_distinct(v) FROM public.s GROUP BY g ORDER BY g};
	my $none = plan_of($n1, $sql, q{SET kingres_shard.shippable_extensions = 'none'; });
	unlike($none, qr/Finalize|\Q$FS\E/, 'shippable_extensions = none: nothing of the extension is shipped');
	like($none, qr/Distributed Scan/, '... the scan answers');
	my ($rc, $o, $e) = run_sql($n1, q{SET kingres_shard.shippable_extensions = 'none'; } . $sql);
	my ($rs, $os) = run_sql($c->single, $sql);
	is($o, $os, '... and its answer is the single node\'s') or diag($e);

	$c->on_every_node(q{CREATE FUNCTION public.plus1(int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 1'});
	$c->on_single(q{CREATE FUNCTION public.plus1(int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 1'});
	my $sql2 = q{SELECT g, count(*) FROM public.s WHERE public.plus1(id) > 100 GROUP BY g ORDER BY g};
	unlike(plan_of($n1, $sql2), qr/Finalize/, 'a function of a user is not shipped, whatever its label');
	$c->compare('a user function: the scan answers', $sql2, ordered => 1, plans => ['pushed', 'scan'], profiles => 0);

	# the version of the extension at this node is not on the list: nothing is shipped
	$n1->safe_psql('postgres', q{UPDATE pg_extension SET extversion = '0.9' WHERE extname = 'kingres_sketch'});
	my $old = plan_of($n1, $sql);
	$n1->safe_psql('postgres', q{UPDATE pg_extension SET extversion = '1.0' WHERE extname = 'kingres_sketch'});
	unlike($old, qr/Finalize/, 'a version that is not on the list is not shipped');
	like(plan_of($n1, $sql), qr/Finalize/, '... and is again when the version is back');
}

# ---------------------------------------------------------------------------
# C. the setting
# ---------------------------------------------------------------------------
{
	for my $ok ('all', 'none', 'kingres_sketch', 'kingres_sketch, kingres_sketch')
	{
		my ($rc, $o, $e) = run_sql($n1, "SET kingres_shard.shippable_extensions = '$ok'");
		is($rc, 0, "the setting accepts '$ok'") or diag($e);
	}
	for my $bad ('hstore', 'none,kingres_sketch', 'kingres_sketch,pg_trgm')
	{
		my ($rc, $o, $e) = run_sql($n1, "SET kingres_shard.shippable_extensions = '$bad'");
		isnt($rc, 0, "the setting refuses '$bad'");
		like($e, qr/is not an extension that kingres_shard can ship/, '... and says why');
	}
	like(plan_of($n1, qq{SELECT g, $K.approx_count_distinct(v) FROM public.s GROUP BY g}, q{SET kingres_shard.shippable_extensions = 'kingres_sketch'; }),
		qr/Finalize/, 'a list that names the extension ships it');
	is($n1->safe_psql('postgres', q{SELECT setting FROM pg_settings WHERE name = 'kingres_shard.shippable_extensions'}), 'all', 'the default is all');
}

# ---------------------------------------------------------------------------
# D. forged and mismatched fragments
# ---------------------------------------------------------------------------
{
	my $PLACE = $n1->safe_psql('postgres', q{SELECT kingres.internal_placement_hash(1)});
	my $as_node = sub {
		my ($node, $id, $sql) = @_;
		my $tok = $node->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, $id)");
		local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
		return run_sql($node, "\\set VERBOSITY verbose\nBEGIN;\n$sql;\nROLLBACK;");
	};
	my $frag = sub { my ($kind, $place, $ext, $sql) = @_; return "/* kingres:$kind-fragment place=$place" . (defined $ext ? " ext=$ext" : '') . " */ DECLARE ks_forged NO SCROLL CURSOR FOR $sql"; };
	my $plan = plan_of($n1, qq{SELECT g, $K.approx_count_distinct(v) FROM public.s GROUP BY g});
	my ($text) = $plan =~ /^\s*Remote SQL: (.*)$/m;
	ok(defined $text, 'the text that the origin ships is known');
	like($text, qr/kingres_sketch\.approx_count_distinct/, '... and names the aggregate with its schema');

	my ($rc, $out, $err) = $as_node->($n2, 2, $frag->('partial', $PLACE, 'kingres_sketch:1.0', $text));
	is($rc, 0, 'a group accepts the text with the extension named at the version it has') or diag($err);

	($rc, $out, $err) = $as_node->($n2, 2, $frag->('partial', $PLACE, 'kingres_sketch:0.9', $text));
	isnt($rc, 0, 'a marker that names another version is refused');
	like($err, qr/KS001/, '... with the retryable class');
	like($err, qr/extension kingres_sketch is at version 1\.0 here and 0\.9 at the node that sent the statement/, '... with the versions in the message');
	like($err, qr/until the upgrade of the extension is finished/, '... and says that a repeat does not help before that');

	($rc, $out, $err) = $as_node->($n2, 2, $frag->('partial', $PLACE, undef, $text));
	isnt($rc, 0, 'a statement that uses the extension and a marker that does not name it is refused');
	like($err, qr/0A000/, '... with 0A000 (a repeat gives the same answer)');
	like($err, qr/marker does not name/, '... and says so');

	($rc, $out, $err) = $as_node->($n2, 2, $frag->('partial', $PLACE, 'kingres_sketch:1.0', q{SELECT g, count(*) FROM public.s GROUP BY g}));
	isnt($rc, 0, 'a marker that names an extension the statement does not use is refused');
	like($err, qr/0A000/, '... with 0A000');

	($rc, $out, $err) = $as_node->($n2, 2, $frag->('partial', $PLACE, 'hstore:1.8', q{SELECT g, count(*) FROM public.s GROUP BY g}));
	isnt($rc, 0, 'a marker that names an extension that is not installed here is refused');
	like($err, qr/KS001/, '... with the retryable class');
	like($err, qr/is not installed here/, '... and says it is not installed');

	($rc, $out, $err) = $as_node->($n2, 2, $frag->('partial', $PLACE, 'kingres_sketch:1.0,;drop', $text));
	isnt($rc, 0, 'a marker with characters that are not those of a name and a version is refused');

	# the closed kind carries the extension too
	my $cplan = plan_of($n1, qq{SELECT id, $K.approx_count_distinct(v), count(*) FROM public.dup GROUP BY id});
	my ($ctext) = $cplan =~ /^\s*Remote SQL: (.*)$/m;
	($rc, $out, $err) = $as_node->($n2, 2, $frag->('query', $PLACE, 'kingres_sketch:1.0', $ctext));
	is($rc, 0, 'a group accepts the text of a closed query with the extension named') or diag($err);
	($rc, $out, $err) = $as_node->($n2, 2, $frag->('query', $PLACE, undef, $ctext));
	isnt($rc, 0, 'and refuses it when the marker does not name the extension');

	# a group at another version: the statement fails with KS001 from that group, and works when the version is back
	$n2->safe_psql('postgres', q{UPDATE pg_extension SET extversion = '0.9' WHERE extname = 'kingres_sketch'});
	($rc, $out, $err) = run_sql($n1, qq{SELECT g, $K.approx_count_distinct(v) FROM public.s GROUP BY g});
	$n2->safe_psql('postgres', q{UPDATE pg_extension SET extversion = '1.0' WHERE extname = 'kingres_sketch'});
	isnt($rc, 0, 'a group that runs another version fails the statement');
	like($err, qr/extension kingres_sketch is at version 0\.9 here and 1\.0 at the node that sent the statement/, '... with the versions of both');
	($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.shippable_extensions = 'none'; } . qq{SELECT g, $K.approx_count_distinct(v) FROM public.s GROUP BY g ORDER BY g});
	is($rc, 0, 'and the escape is the setting none: the node that runs the statement reads the rows') or diag($err);
	($rc, $out, $err) = run_sql($n1, qq{SELECT g, $K.approx_count_distinct(v) FROM public.s GROUP BY g});
	is($rc, 0, 'with the version back the statement works') or diag($err);
}

done_testing();
