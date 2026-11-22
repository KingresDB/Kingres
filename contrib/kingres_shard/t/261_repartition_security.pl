# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/261_repartition_security.pl
#
# Phase 12, increment J1: what a group accepts of the text of a repartitioned join (design/analytics-phase12-queries.md, 4.4 and 3.1).
# The group runs the text under the rules of every closed fragment (the proof of a node, the placement, the shippability walker), and
# one more: the only sub-select that stands for a table that another node moved is a SELECT of casts of the columns of unnest() over
# array parameters, and the number of them is the one in the marker (rp=<n>).  A forged text must be refused, and a refusal must not
# say that the map is stale (a repeat does not help).
#
#   A. the text of the origin runs on a group when the arrays come with it
#   B. the marker and the text disagree: no rp, a wrong count, rp on a text without a moved table, a marker that is not valid
#   C. a sub-select of another shape in the place of the moved table
#   D. the placement of the sender is not the group's: KS001

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my ($n1, $n2) = make_cluster('rs', 2, extra => "kingres_shard.enable_repartition_join = force\nkingres_shard.enable_partial_aggregation = on\n");
$n1->safe_psql('postgres', q{
	CREATE TABLE public.o (id int PRIMARY KEY, cust int, amount int);
	SELECT kingres.create_distributed_table('public.o', 'id', bucket_count => 12);
	CREATE TABLE public.cu (id int PRIMARY KEY, name text);
	SELECT kingres.create_distributed_table('public.cu', 'id', colocate_with => 'public.o');
	INSERT INTO public.cu SELECT g, 'c' || g FROM generate_series(1, 100) g;
	INSERT INTO public.o SELECT g, 1 + g % 100, g % 7 FROM generate_series(1, 500) g;
});
$_->safe_psql('postgres', 'ANALYZE') for ($n1, $n2);

sub as_node
{
	my ($node, $id, $sql, $opts) = @_;
	my $tok = $node->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, $id)");
	local $ENV{PGOPTIONS} = $opts // "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
	$sql .= ';' unless $sql =~ m{\\g\z};
	return run_sql($node, "\\set VERBOSITY verbose\nBEGIN;\n$sql\nROLLBACK;");
}

my $PLACE = $n1->safe_psql('postgres', q{SELECT kingres.internal_placement_hash(1)});
my ($rc, $plan, $err) = run_sql($n1, q{EXPLAIN (VERBOSE, COSTS OFF) SELECT o.id, cu.name FROM public.o JOIN public.cu ON cu.id = o.cust});
my ($text) = $plan =~ /Remote SQL: (.*)$/m;
ok(defined $text, 'the statement is repartitioned and its text is shown') or diag($plan . $err);
like($text, qr/UNNEST\(\$1, \$2\) u\(c1, c2\)/, 'the text has the moved table as a sub-select over unnest of two array parameters') or diag($text);

# psql binds its parameters without a type (the node that sends the text declares text[]), so the test says the type in the text
$text =~ s/UNNEST\(\$1, \$2\)/UNNEST(\$1::text[], \$2::text[])/ or die 'the shape of the text changed';

# the statement is run as the one forwarded statement of a session; the arrays are bound with \bind
sub fragment
{
	my ($marker_tail, $body, @args) = @_;
	return "/* kingres:query-fragment place=$PLACE$marker_tail */ DECLARE ks_f NO SCROLL CURSOR FOR $body\n\\bind " . join(' ', map { "'$_'" } @args) . " \\g";
}
my $ids = '{1,2,3,4,5,6,7,8,9,10,11,12}';
my $custs = '{3,4,5,6,7,8,9,10,11,12,13,14}';

# A
($rc, my $out, $err) = as_node($n2, 2, fragment(' rp=1', $text, $ids, $custs));
is($rc, 0, 'A: the text with its marker and its arrays runs on a group') or diag($err);

# B
($rc, $out, $err) = as_node($n2, 2, fragment('', $text, $ids, $custs));
isnt($rc, 0, 'B: a text with a moved table and a marker without rp is refused');
like($err, qr/does not accept the fragment|not the ones the marker counts/, '... as a fragment the node does not accept') or diag($err);
unlike($err, qr/KS001/, '... which is not a stale map');
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=2', $text, $ids, $custs));
isnt($rc, 0, 'B: rp=2 on a text with one moved table is refused');
unlike($err, qr/KS001/, '... not as a stale map');
my $plain = q{SELECT o.id, o.amount FROM public.o WHERE o.amount > 3};
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', $plain));
isnt($rc, 0, 'B: rp=1 on a text without a moved table is refused');
unlike($err, qr/KS001/, '... not as a stale map');
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=0', $text, $ids, $custs));
isnt($rc, 0, 'B: rp=0 is not a valid marker');
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=99', $text, $ids, $custs));
isnt($rc, 0, 'B: rp=99 is not a valid marker');

# C: the moved table replaced by sub-selects that read something else
my @forged = (
	[ 'the sub-select reads a table', q{SELECT u.id::int AS id, u.cust::int AS cust, NULL::int AS amount FROM (SELECT id::text, cust::text FROM public.o) u}, 0 ],
	[ 'the sub-select calls another function', q{SELECT g::text::int AS id, g::text::int AS cust, NULL::int AS amount FROM generate_series(1, 5) g}, 0 ],
	[ 'the sub-select has a WHERE', q{SELECT u.c1::int AS id, u.c2::int AS cust, NULL::int AS amount FROM ROWS FROM (unnest($1::text[]), unnest($2::text[])) AS u(c1, c2) WHERE u.c1 = '1'}, 2 ],
	[ 'the sub-select computes with the columns', q{SELECT (u.c1::int + 1) AS id, u.c2::int AS cust, NULL::int AS amount FROM ROWS FROM (unnest($1::text[]), unnest($2::text[])) AS u(c1, c2)}, 2 ],
	[ 'the sub-select unnests a constant array', q{SELECT u.c1::int AS id, u.c1::int AS cust, NULL::int AS amount FROM unnest('{1,2,3}'::text[]) AS u(c1)}, 0 ],
	[ 'the sub-select has a sub-select of its own', q{SELECT u.c1::int AS id, u.c2::int AS cust, NULL::int AS amount FROM ROWS FROM (unnest($1::text[]), unnest((SELECT array_agg(id::text) FROM public.o))) AS u(c1, c2)}, 1 ],
);
for my $f (@forged)
{
	my ($name, $sub, $nargs) = @$f;
	my $body = "SELECT o.id, cu.name FROM ($sub) o JOIN public.cu ON cu.id = o.cust";
	my @args = ($ids, $custs)[0 .. $nargs - 1];
	($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', $body, @args));
	isnt($rc, 0, "C: $name: refused");
	unlike($err, qr/KS001/, '... not as a stale map');
}

# D
($rc, $out, $err) = as_node($n2, 2, "/* kingres:query-fragment place=1 rp=1 */ DECLARE ks_f NO SCROLL CURSOR FOR $text\n\\bind '$ids' '$custs' \\g");
isnt($rc, 0, 'D: a placement that is not the group\'s is refused');
like($err, qr/KS001|bucket map of this node is not the one/, '... as a stale map, which a repeat can cure') or diag($err);

# E: the first step of a partial aggregation over a repartitioned join (J2): the same rules, and the marker of the partial fragment has rp too
($rc, my $pplan, $err) = run_sql($n1, q{EXPLAIN (VERBOSE, COSTS OFF) SELECT cu.name, count(*), sum(o.amount) FROM public.o JOIN public.cu ON cu.id = o.cust GROUP BY cu.name});
my ($ptext) = $pplan =~ /Remote SQL: (.*)$/m;
ok(defined $ptext && $pplan =~ /partial aggregate/, 'E: the grouped aggregate is a partial aggregation over the repartitioned join') or diag($pplan . $err);
$ptext =~ s/UNNEST\(\$1, \$2\)/UNNEST(\$1::text[], \$2::text[])/ or die 'the shape of the text changed';
sub pfragment
{
	my ($marker_tail, $body, @args) = @_;
	my $sql = "/* kingres:partial-fragment place=$PLACE$marker_tail */ DECLARE ks_f NO SCROLL CURSOR FOR $body";
	return @args ? "$sql\n\\bind " . join(' ', map { "'$_'" } @args) . " \\g" : $sql;
}
($rc, $out, $err) = as_node($n2, 2, pfragment(' rp=1', $ptext, $ids, $custs));
is($rc, 0, 'E: the partial text with its marker and its arrays runs on a group') or diag($err);
($rc, $out, $err) = as_node($n2, 2, pfragment('', $ptext, $ids, $custs));
isnt($rc, 0, 'E: a partial text with a moved table and a marker without rp is refused');
unlike($err, qr/KS001/, '... not as a stale map');
($rc, $out, $err) = as_node($n2, 2, pfragment(' rp=2', $ptext, $ids, $custs));
isnt($rc, 0, 'E: rp=2 on a partial text with one moved table is refused');
unlike($err, qr/KS001/, '... not as a stale map');
($rc, $out, $err) = as_node($n2, 2, pfragment(' rp=1', q{SELECT o.amount, count(*) FROM public.o GROUP BY o.amount}));
isnt($rc, 0, 'E: rp=1 on a partial text without a moved table is refused');
unlike($err, qr/KS001/, '... not as a stale map');
($rc, $out, $err) = as_node($n2, 2, pfragment(' rp=0', $ptext, $ids, $custs));
isnt($rc, 0, 'E: rp=0 is not a valid marker');
for my $f (@forged)
{
	my ($name, $sub, $nargs) = @$f;
	my $body = "SELECT cu.name, count(*) FROM ($sub) o JOIN public.cu ON cu.id = o.cust GROUP BY cu.name";
	my @args = ($ids, $custs)[0 .. $nargs - 1];
	($rc, $out, $err) = as_node($n2, 2, pfragment(' rp=1', $body, @args));
	isnt($rc, 0, "E: $name, in a partial aggregation: refused");
	unlike($err, qr/KS001/, '... not as a stale map');
}
($rc, $out, $err) = as_node($n2, 2, "/* kingres:partial-fragment place=1 rp=1 */ DECLARE ks_f NO SCROLL CURSOR FOR $ptext\n\\bind '$ids' '$custs' \\g");
isnt($rc, 0, 'E: a placement that is not the group\'s is refused');
like($err, qr/KS001|bucket map of this node is not the one/, '... as a stale map, which a repeat can cure') or diag($err);

# F: a relation that is sent whole to every group (broadcast, J2).  The group cannot tell it from a routed one and does not need to: it
# is the same sub-select over unnest of text[] parameters, counted by rp=<n>.  What the group does check, and what a broadcast needs
# more than a routed relation: the tables that stay are aligned with each other, and the sent relation is not a table of the group's.
$n1->safe_psql('postgres', q{
	CREATE TABLE public.it (id int PRIMARY KEY, ord int);
	SELECT kingres.create_distributed_table('public.it', 'id', colocate_with => 'public.o');
	INSERT INTO public.it SELECT g, g % 50 FROM generate_series(1, 300) g;
});
$_->safe_psql('postgres', 'ANALYZE') for ($n1, $n2);
($rc, my $bplan, $err) = run_sql($n1, q{EXPLAIN (VERBOSE, COSTS OFF) SELECT o.id, cu.name FROM public.o LEFT JOIN public.cu ON cu.id = o.cust + 1});
my ($btext) = $bplan =~ /Remote SQL: (.*)$/m;
ok(defined $btext && $bplan =~ /Repartition: cu broadcast/, 'F: the statement is planned with cu sent whole') or diag($bplan . $err);
like($btext, qr/UNNEST\(\$1, \$2\) u\(c1, c2\)/, 'F: the text has the sent table as a sub-select over unnest of two array parameters') or diag($btext);
$btext =~ s/UNNEST\(\$1, \$2\)/UNNEST(\$1::text[], \$2::text[])/ or die 'the shape of the text changed';
my $bids = '{1,2,3,4,5,6,7,8,9,10,11,12}';
my $bnames = '{a,b,c,d,e,f,g,h,i,j,k,l}';

($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', $btext, $bids, $bnames));
is($rc, 0, 'F: the broadcast text with its marker and its arrays runs on a group') or diag($err);
($rc, $out, $err) = as_node($n2, 2, fragment('', $btext, $bids, $bnames));
isnt($rc, 0, 'F: a broadcast text with a marker without rp is refused');
unlike($err, qr/KS001/, '... not as a stale map');
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=2', $btext, $bids, $bnames));
isnt($rc, 0, 'F: rp=2 on a text with one sent table is refused');
unlike($err, qr/KS001/, '... not as a stale map');
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=0', $btext, $bids, $bnames));
isnt($rc, 0, 'F: rp=0 is not a valid marker');
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', q{SELECT o.id, o.amount FROM public.o WHERE o.amount > 3}));
isnt($rc, 0, 'F: rp=1 on a text without a sent table is refused');
unlike($err, qr/KS001/, '... not as a stale map');

# the sent table replaced by sub-selects that read something else
for my $f (@forged)
{
	my ($name, $sub, $nargs) = @$f;
	my $body = "SELECT o.id, cu.name FROM public.o LEFT JOIN ($sub) cu ON cu.id = o.cust + 1";
	my @args = ($bids, $bnames)[0 .. $nargs - 1];
	($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', $body, @args));
	isnt($rc, 0, "F: $name, as a table sent whole: refused");
	unlike($err, qr/KS001/, '... not as a stale map');
}

# the tables that stay must be aligned with each other: a text that joins two distributed tables without their keys, beside a sent
# table, would be a join of rows that are on different groups (the origin never makes such a text; a forged one is refused)
my $unaligned = "SELECT o.id, it.id FROM public.o, public.it, (SELECT u.c1::int AS id, u.c2 AS name FROM UNNEST(\$1::text[], \$2::text[]) u(c1, c2)) cu WHERE cu.id < o.amount";
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', $unaligned, $bids, $bnames));
isnt($rc, 0, 'F: two distributed tables that are not joined on their keys, beside a sent table: refused');
unlike($err, qr/KS001/, '... not as a stale map');
my $aligned = "SELECT o.id, it.id FROM public.o, public.it, (SELECT u.c1::int AS id, u.c2 AS name FROM UNNEST(\$1::text[], \$2::text[]) u(c1, c2)) cu WHERE it.id = o.id AND cu.id < o.amount";
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', $aligned, $bids, $bnames));
is($rc, 0, 'F: the same text with the two tables joined on their keys runs') or diag($err);

# the sent table is the only relation: nothing of the group is read, the text returns the rows that its sender gave it (harmless: it is
# not a way to read anything)
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', "SELECT cu.id FROM (SELECT u.c1::int AS id, u.c2 AS name FROM UNNEST(\$1::text[], \$2::text[]) u(c1, c2)) cu", $bids, $bnames));
is($rc, 0, 'F: a text with a sent table and no table of the group reads nothing of the group and returns what it was given') or diag($err);

# the sent table on the preserved side of an outer join would be emitted once by every group
($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', "SELECT o.id, cu.name FROM (SELECT u.c1::int AS id, u.c2 AS name FROM UNNEST(\$1::text[], \$2::text[]) u(c1, c2)) cu LEFT JOIN public.o ON cu.id = o.cust + 1", $bids, $bnames));
isnt($rc, 0, 'F: a sent table on the preserved side of a LEFT JOIN is refused');
unlike($err, qr/KS001/, '... not as a stale map');

($rc, $out, $err) = as_node($n2, 2, "/* kingres:query-fragment place=1 rp=1 */ DECLARE ks_f NO SCROLL CURSOR FOR $btext\n\\bind '$bids' '$bnames' \\g");
isnt($rc, 0, 'F: a placement that is not the group\'s is refused');
like($err, qr/KS001|bucket map of this node is not the one/, '... as a stale map, which a repeat can cure') or diag($err);

# the first step of a partial aggregation over a statement with a sent table: the same rules, and the marker has rp too
($rc, my $bpplan, $err) = run_sql($n1, q{EXPLAIN (VERBOSE, COSTS OFF) SELECT o.amount, count(cu.id) FROM public.o LEFT JOIN public.cu ON cu.id = o.cust + 1 GROUP BY o.amount});
my ($bptext) = $bpplan =~ /Remote SQL: (.*)$/m;
ok(defined $bptext && $bpplan =~ /partial aggregate/ && $bpplan =~ /Repartition: cu broadcast/, 'F: the grouped aggregate is a partial aggregation with cu sent whole') or diag($bpplan . $err);
# only cu.id is needed above the join here, so one array is shipped: unnest($1) u(c1)
my @bpargs = ($bids);
$bptext =~ s/\bunnest\(\$1\) u\(c1\)/unnest(\$1::text[]) u(c1)/ or die "the shape of the text changed: $bptext";
($rc, $out, $err) = as_node($n2, 2, pfragment(' rp=1', $bptext, @bpargs));
is($rc, 0, 'F: the partial text with its marker and its arrays runs on a group') or diag($err . $bptext);
($rc, $out, $err) = as_node($n2, 2, pfragment('', $bptext, @bpargs));
isnt($rc, 0, 'F: a partial text with a sent table and a marker without rp is refused');
unlike($err, qr/KS001/, '... not as a stale map');
($rc, $out, $err) = as_node($n2, 2, pfragment(' rp=2', $bptext, @bpargs));
isnt($rc, 0, 'F: rp=2 on a partial text with one sent table is refused');
unlike($err, qr/KS001/, '... not as a stale map');

# G: the moved side of a LEFT JOIN (review P12J-02).  The planner offers only the nullable table of a LEFT JOIN as the one to move
# (an optimisation: the group would refuse the other); the group's own rule is what makes it correct: a moved (routed) table on the
# preserved side of an outer join would be emitted by every group for the rows it does not match, so the text is refused.  A fragment
# of that shape is sent to the group directly, with its controls: the same sub-select in an inner join, and on the nullable side.
{
	my $osub = q{SELECT u.c1::int AS id, u.c2::int AS cust, NULL::int AS amount FROM ROWS FROM (unnest($1::text[]), unnest($2::text[])) AS u(c1, c2)};
	($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', "SELECT o.id, cu.name FROM ($osub) o JOIN public.cu ON cu.id = o.cust", $ids, $custs));
	is($rc, 0, 'G: control: the moved table in an inner join runs on a group') or diag($err);
	($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', "SELECT cu.id, o.id FROM public.cu LEFT JOIN ($osub) o ON cu.id = o.cust", $ids, $custs));
	is($rc, 0, 'G: control: the moved table on the nullable side of a LEFT JOIN runs on a group') or diag($err);
	($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', "SELECT o.id, cu.name FROM ($osub) o LEFT JOIN public.cu ON cu.id = o.cust", $ids, $custs));
	isnt($rc, 0, 'G: the moved table on the preserved side of a LEFT JOIN is refused by the group');
	like($err, qr/does not accept the fragment/, '... as a fragment that the node does not accept') or diag($err);
	unlike($err, qr/KS001/, '... not as a stale map');
	($rc, $out, $err) = as_node($n2, 2, fragment(' rp=1', "SELECT o.id, cu.name FROM public.cu RIGHT JOIN ($osub) o ON cu.id = o.cust", $ids, $custs));
	isnt($rc, 0, 'G: the same with a RIGHT JOIN written the other way round is refused');
	unlike($err, qr/KS001/, '... not as a stale map');
}

done_testing();
