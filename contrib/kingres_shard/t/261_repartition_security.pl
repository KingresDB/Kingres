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

done_testing();
