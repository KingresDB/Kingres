# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/134_review_step5.pl
#
# The review of step 5 (reviews/2026-11-19-kingres-shard-phase2b-step-5.md):
#   P2D-01  a parameter, an array constant, a row field of a reg* type is never shipped to the groups (each would
#           resolve the name to its own object); the answer is the single node's on the scan and the fragment scan
#   P2D-03  FROM ONLY a distributed table is refused on every path (select, update, delete), a reference table is fine
#   P2D-05  a fragment with place=0 is a stale placement, not "no check"
#   P2D-08  step 6 against the cases where pushdown.h was right only for an INSERT: HAVING without an aggregate,
#           ONLY, LIMIT, ORDER BY

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('rs');
my ($n1, $n2, $n3) = $c->nodes;
my @nodes = ($n1, $n2, $n3);

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }
table(q{CREATE TABLE public.s1 (id int, a int, b int)}, q{; SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.rf (k int PRIMARY KEY, w text)}, q{; SELECT kingres.create_reference_table('public.rf')});
$c->on_both(q{INSERT INTO public.s1 SELECT g, g % 7, g % 5 FROM generate_series(1, 600) g, generate_series(1, 2) dup});
$c->on_both(q{INSERT INTO public.rf VALUES (1, 'a'), (2, 'b')});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

sub remote_sql
{
	my ($node, $pre, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "$pre EXPLAIN (VERBOSE, COSTS OFF) $sql");
	return $out =~ /Remote SQL: (.*)$/m ? $1 : "ERROR: $err";
}

# P2D-01
my $generic = 'SET plan_cache_mode = force_generic_plan; ';
my $prep = q{PREPARE pr(regclass) AS SELECT count(*) FROM public.s1 WHERE a = ($1::oid::int8 % 7)::int; };
my ($rs, $os) = run_sql($c->single, q{PREPARE pr(regclass) AS SELECT count(*) FROM public.s1 WHERE a = ($1::oid::int8 % 7)::int; EXECUTE pr('pg_class')});
for my $n (@nodes)
{
	like(remote_sql($n, $generic . $prep, q{EXECUTE pr('pg_class')}), qr/^SELECT [^W]*$/, 'P2D-01: a regclass parameter is not in the text the groups run (' . $n->name . ')');
	my ($rd, $od) = run_sql($n, $generic . $prep . q{EXECUTE pr('pg_class')});
	is($od, $os, 'P2D-01: ... and the answer is the single node\'s');
}
my $arr = q{SELECT count(*) FROM public.s1 WHERE a::oid = ANY (ARRAY['pg_class'::regclass::oid, 'pg_type'::regclass::oid]) OR a::int = ANY (ARRAY[1, 2])};
$c->compare('P2D-01: an array of reg* constants', q{SELECT count(*) FROM public.s1 WHERE a = ANY (ARRAY['pg_class'::regclass::oid::int % 7, 3])}, profiles => 0);
like(remote_sql($n1, '', q{SELECT count(*) FROM public.s1 WHERE a = 3}), qr/WHERE/, 'the control: a plain restriction is shipped');

# P2D-03
for my $n (@nodes)
{
	my ($rc, $out, $err) = run_sql($n, q{SELECT count(*) FROM ONLY public.s1});
	isnt($rc, 0, 'P2D-03: FROM ONLY a distributed table is refused (' . $n->name . ')');
	like($err, qr/FROM ONLY is not supported for distributed table "s1"/, '... with its message');
}
for my $sql (q{UPDATE ONLY public.s1 SET b = 1}, q{DELETE FROM ONLY public.s1}, q{SELECT s1.id FROM ONLY public.s1 JOIN public.rf ON rf.k = s1.id},
	q{SELECT id, count(*) FROM ONLY public.s1 GROUP BY id})
{
	my ($rc, $out, $err) = run_sql($n1, $sql);
	like($err, qr/FROM ONLY is not supported/, "P2D-03: refused: $sql");
}
$c->compare('P2D-03: ONLY on a reference table works', q{SELECT * FROM ONLY public.rf ORDER BY 1}, ordered => 1, profiles => 0);
is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.s1}), 1200, 'P2D-03: nothing was changed');

# P2D-05: a fragment with place=0
{
	my $tok = $n2->safe_psql('postgres', q{SELECT kingres.internal_forward_token(current_user, 2)});
	local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
	for my $m ('query-fragment', 'insert-select-fragment')
	{
		my $body = $m eq 'query-fragment' ? 'DECLARE ks_f NO SCROLL CURSOR FOR SELECT id, count(*) FROM public.s1 GROUP BY id'
			: 'INSERT INTO public.s1 SELECT id, a, b FROM public.s1 WHERE id < 0';
		my ($rc, $out, $err) = run_sql($n2, "\\set VERBOSITY verbose\nBEGIN;\n/* kingres:$m place=0 */ $body;\nROLLBACK;");
		isnt($rc, 0, "P2D-05: $m with place=0 is refused");
		like($err, qr/KS001/, '... as a stale placement');
	}
}

# P2D-08: shapes that were right only for an INSERT, against the closed query of step 6
$c->compare('P2D-08: HAVING without an aggregate', q{SELECT id FROM public.s1 GROUP BY id HAVING id % 3 = 0 ORDER BY 1}, ordered => 1, profiles => 0, plans => ['pushed', 'scan']);
$c->compare('P2D-08: HAVING on a group column and an aggregate', q{SELECT id, count(*) FROM public.s1 GROUP BY id HAVING count(*) > 1 AND id > 590 ORDER BY 1}, ordered => 1, profiles => 0, plans => ['pushed', 'scan']);
$c->compare('P2D-08: LIMIT and OFFSET above a closed query', q{SELECT id, count(*) FROM public.s1 GROUP BY id ORDER BY id OFFSET 5 LIMIT 7}, ordered => 1, profiles => 0, plans => ['pushed', 'scan'], path => 'Kingres Fragment Scan');
$c->compare('P2D-08: an aggregate without GROUP BY is not closed', q{SELECT count(*), max(a) FROM public.s1}, profiles => 0, plans => ['pushed', 'scan']);
$c->compare('P2D-08: DISTINCT without the key is not closed', q{SELECT DISTINCT a FROM public.s1}, profiles => 0, plans => ['pushed', 'scan']);

done_testing();
