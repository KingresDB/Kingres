# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/172_group_rules_survivors.pl
#
# Review P2E-04: rules of steps 7 and 9 that no test separated from their absence (the mutants M08, M12, M13, M14 and M31 of the
# review survived every shipped test).  A group must refuse, with 0A000, a fragment that the origin would never send:
#
#   A. a closed fragment with a negative LIMIT, with an OFFSET, with FETCH FIRST ... WITH TIES, with a LIMIT that is not a
#      bigint constant or parameter;
#   B. a partial fragment with DISTINCT (and, with the same text otherwise, the accepted form);
#   C. the sum of count and offset of a LIMIT given as parameters that does not fit in a bigint: this node must not send a
#      number that wrapped (the answer is the single node's rows or the single node's error), with custom and generic plans, from
#      every node.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql prepared_count);
use KingresShardCompare;

my $c = KingresShardCompare->new('gr', extra => "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_sort = on\n");
my ($n1, $n2, $n3) = $c->nodes;
my @nodes = ($n1, $n2, $n3);
my $single = $c->single;

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

sub as_node
{
	my ($node, $id, $sql) = @_;
	my $tok = $node->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, $id)");
	local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
	return run_sql($node, "\\set VERBOSITY verbose\nBEGIN;\n$sql;\nROLLBACK;");
}

table(q{CREATE TABLE public.d (id int, k int, g int)},
	q{; SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 12)});
$c->on_both(q{INSERT INTO public.d SELECT g % 300, g, g % 7 FROM generate_series(1, 3000) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# ---------------------------------------------------------------------------
# A and B. forged fragments
# ---------------------------------------------------------------------------
my $PLACE = $n1->safe_psql('postgres', q{SELECT kingres.internal_placement_hash(1)});
my ($closed) = plan_of($n1, q{SELECT id, count(*) FROM public.d GROUP BY id}) =~ /Remote SQL: (.*)$/m;
my ($partial) = plan_of($n1, q{SELECT g, count(*) FROM public.d GROUP BY g}) =~ /Remote SQL: (.*)$/m;
ok(defined $closed && defined $partial, 'the statements are shipped (closed and partial)') or BAIL_OUT('not shipped');
my $q = sub { my ($kind, $text) = @_; return "/* kingres:$kind-fragment place=$PLACE */ DECLARE ks_f NO SCROLL CURSOR FOR $text"; };

{
	my ($rc, $out, $err) = as_node($n2, 2, $q->('query', $closed));
	is($rc, 0, 'the text of the origin is accepted as a closed fragment') or diag($err);
	($rc, $out, $err) = as_node($n2, 2, $q->('query', "$closed ORDER BY id LIMIT 5"));
	is($rc, 0, 'a closed fragment with ORDER BY and one constant LIMIT is accepted') or diag($err);
	for my $t (
		[ 'a negative LIMIT', "$closed ORDER BY id LIMIT -1" ],
		[ 'a LIMIT and an OFFSET', "$closed ORDER BY id LIMIT 5 OFFSET 2" ],
		[ 'an OFFSET alone', "$closed ORDER BY id OFFSET 2" ],
		[ 'FETCH FIRST ... WITH TIES', "$closed ORDER BY id FETCH FIRST 5 ROWS WITH TIES" ],
		[ 'a LIMIT that is a sub-select', "$closed ORDER BY id LIMIT (SELECT 5)" ],
		[ 'a LIMIT of NULL', "$closed ORDER BY id LIMIT NULL" ])
	{
		($rc, $out, $err) = as_node($n2, 2, $q->('query', $t->[1]));
		isnt($rc, 0, "a closed fragment with $t->[0] is refused");
		like($err, qr/0A000|does not accept the fragment/, '... with 0A000') or diag($err);
		($rc, $out, $err) = as_node($n2, 2, $q->('partial', $t->[1]));
		isnt($rc, 0, "the same text as a partial fragment is refused");
	}
	# a partial fragment: the accepted form, and the same with DISTINCT
	($rc, $out, $err) = as_node($n2, 2, $q->('partial', $partial));
	is($rc, 0, 'the text of the origin is accepted as a partial fragment') or diag($err);
	(my $distinct = $partial) =~ s/^\s*SELECT\b/SELECT DISTINCT/;
	isnt($distinct, $partial, 'a text with DISTINCT was made');
	($rc, $out, $err) = as_node($n2, 2, $q->('partial', $distinct));
	isnt($rc, 0, 'a partial fragment with DISTINCT is refused');
	like($err, qr/0A000|does not accept the fragment/, '... with 0A000') or diag($err);
	($rc, $out, $err) = as_node($n2, 2, $q->('partial', "SELECT DISTINCT ON (g) g, count(*) FROM public.d GROUP BY g"));
	isnt($rc, 0, 'a partial fragment with DISTINCT ON is refused');
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;
}

# ---------------------------------------------------------------------------
# C. the sum of count and offset that does not fit
# ---------------------------------------------------------------------------
{
	my $BASE = q{SELECT id, count(*) AS cnt FROM public.d GROUP BY id ORDER BY cnt DESC, id};
	my $MAX = '9223372036854775807';
	my $G = 'SET plan_cache_mode = force_generic_plan; ';
	my $C = 'SET plan_cache_mode = force_custom_plan; ';
	my $pre = "PREPARE lo(bigint, bigint) AS $BASE LIMIT \$1 OFFSET \$2;";
	for my $args ("$MAX, 1", "1, $MAX", "$MAX, $MAX", "$MAX, 0", "0, $MAX", "5, 295", "5, -1", "-1, 5")
	{
		my ($rs, $os, $es) = run_sql($single, "$pre EXECUTE lo($args)");
		for my $nd (@nodes)
		{
			for my $mode ($C, $G)
			{
				my ($rc, $out, $err) = run_sql($nd, "$mode$pre EXECUTE lo($args)");
				is($rc, $rs, $nd->name . ": EXECUTE lo($args): the same status as the single node" . ($mode eq $G ? ' (generic plan)' : ''));
				if ($rs == 0) { is($out, $os, '... and the same rows') or diag($err); }
				else
				{
					my ($want) = $es =~ /ERROR:\s+(.*)/;
					like($err, qr/\Q$want\E/, '... and the same error');
					unlike($err, qr/remote node/, '... said by this node, not by a group');
				}
			}
		}
	}
	# the sum in an expression: the single node's error or answer
	my $pre2 = "PREPARE le(bigint) AS $BASE LIMIT \$1 + 1;";
	my ($rs, $os, $es) = run_sql($single, "$pre2 EXECUTE le($MAX)");
	for my $nd (@nodes)
	{
		my ($rc, $out, $err) = run_sql($nd, "$G$pre2 EXECUTE le($MAX)");
		is($rc, $rs, $nd->name . ': LIMIT $1 + 1 with the largest bigint: the same status as the single node');
		my ($want) = $es =~ /ERROR:\s+(.*)/;
		like($err, qr/\Q$want\E/, '... and the same error') if $rs != 0;
	}
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;
}

done_testing();
