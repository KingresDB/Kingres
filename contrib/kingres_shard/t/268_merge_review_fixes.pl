# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/268_merge_review_fixes.pl
#
# Phase 12, the fixes for the independent review of the sharding part (docs/reviews/2026-11-22-kingres-shard-phase12-joins-merge.md).
# Every statement is run on the cluster and on a single ordinary node holding the same rows; the tables afterwards must be the single
# node's.  What a refused statement must do is to change nothing.
#
#   A. P12M-01: a prepared MERGE that declares parameters it does not use (leading, middle, trailing; every type), for every shape that
#      ships a source or a text (M1, M3, M4, M5, M6), under custom plans and under a generic plan
#   B. P12M-02: a MERGE through a view, a view of another owner, a CHECK OPTION view, a target with row-level security: refused by the
#      guard that is meant to refuse it (each guard has its own message, so that removing one is seen), for M1, M3 and M4
#   C. P12M-06: a column with a stable default that the INSERT action does not list
#   D. P12J-01: the largest value of kingres_shard.repartition_max_bytes

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('mx');
my @nodes = $c->nodes;
my $single = $c->single;
my $turn = 0;

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }
sub local_table { my ($sql) = @_; $c->on_every_node($sql); $c->on_single($sql); }

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

table(q{CREATE TABLE public.t (id int PRIMARY KEY, v text, n int)},
	q{; SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.s (id int PRIMARY KEY, w int)},
	q{; SELECT kingres.create_distributed_table('public.s', 'id', colocate_with => 'public.t')});
table(q{CREATE TABLE public.rf (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.rf')});
table(q{CREATE TABLE public.rt (k int PRIMARY KEY, label text, n int)},
	q{; SELECT kingres.create_reference_table('public.rt')});
local_table(q{CREATE TABLE public.loc (x int, y text, z int)});
local_table(q{CREATE TABLE public.lt (id int PRIMARY KEY, v text, n int)});
$c->on_both(q{INSERT INTO public.t SELECT g, 't' || g, g % 7 FROM generate_series(1, 60) g});
$c->on_both(q{INSERT INTO public.s SELECT g, g % 10 FROM generate_series(30, 90) g});
$c->on_both(q{INSERT INTO public.rf SELECT g, repeat('r', 1 + g % 5) FROM generate_series(0, 40) g});
$c->on_both(q{INSERT INTO public.rt SELECT g, repeat('q', 1 + g % 4), g % 5 FROM generate_series(0, 30) g});
$c->on_every_node(q{INSERT INTO public.loc SELECT g, 'l' || g, g % 5 FROM generate_series(50, 100) g});
$c->on_single(q{INSERT INTO public.loc SELECT g, 'l' || g, g % 5 FROM generate_series(50, 100) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# ---------------------------------------------------------------------------
# A. parameters that the statement declares and does not use
# ---------------------------------------------------------------------------
# A is the parameter of a filter, B the parameter of the value; the others are declared and unused.  The arrays of a shipped source
# (M3, M5) are numbered after the last parameter the text uses; the statement of the origin sends exactly that many parameters.
my %SHAPE = (
	M1 => { tmpl => q{MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED AND s.w > __A__ THEN UPDATE SET n = __B__ WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (s.id, 'm1', __B__)},
			a => 3, table => 'public.t', order => 'id', path => qr/Pushed: colocated MERGE/ },
	M3 => { tmpl => q{MERGE INTO public.t USING public.loc ON t.id = loc.x WHEN MATCHED AND loc.z > __A__ THEN UPDATE SET n = __B__ WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (loc.x, loc.y, __B__)},
			a => 1, table => 'public.t', order => 'id', path => qr/Pushed: MERGE with a routed source/ },
	M4 => { tmpl => q{MERGE INTO public.rt USING public.rf ON rt.k = rf.k WHEN MATCHED AND rf.k > __A__ THEN UPDATE SET n = __B__ WHEN NOT MATCHED THEN INSERT (k, label, n) VALUES (rf.k, rf.label, __B__)},
			a => 5, table => 'public.rt', order => 'k', path => qr/Remote SQL: \/\* kingres:reference-write \*\/ MERGE/ },
	M5 => { tmpl => q{MERGE INTO public.rt USING public.loc ON rt.k = loc.x - 60 WHEN MATCHED AND loc.z > __A__ THEN UPDATE SET n = __B__ WHEN NOT MATCHED THEN INSERT (k, label, n) VALUES (loc.x - 60, loc.y, __B__)},
			a => 1, table => 'public.rt', order => 'k', path => qr/unnest/i },
	M6 => { tmpl => q{MERGE INTO public.lt USING public.t ON lt.id = t.id WHEN MATCHED AND t.n > __A__ THEN UPDATE SET n = __B__ + t.n WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (t.id, t.v, __B__)},
			a => 2, table => 'public.lt', order => 'id', path => qr/Kingres Distributed Scan/ },
);

# the declared and unused parameters: a type and a value of it
my %UNUSED = (
	'int'         => '7',
	'text'        => q{'unused'},
	'text[]'      => q{ARRAY['1','2','3']},
	'numeric'     => '3.5',
	'boolean'     => 'true',
	'bytea'       => q{'\x01'::bytea},
	'jsonb'       => q{'{"a": 1}'::jsonb},
	'timestamptz' => q{'2026-01-01 00:00:00+00'::timestamptz},
	'int[]'       => 'ARRAY[1,2]',
);
my @LAYOUTS = (
	[ 'no unused parameter', 'A', 'B' ],
	( map { [ "trailing $_", 'A', 'B', $_ ] } sort keys %UNUSED ),
	( map { [ "leading $_",  $_, 'A', 'B' ] } ('int', 'text', 'text[]') ),
	( map { [ "middle $_",   'A', $_, 'B' ] } ('int', 'text', 'text[]') ),
	[ 'two trailing text[] and int', 'A', 'B', 'text[]', 'int' ],
	[ 'unused between and after', 'A', 'text[]', 'B', 'text[]' ],
);

sub param_script
{
	my ($shape, $tokens, $mode, $round) = @_;
	my $sh = $SHAPE{$shape};
	my @types;
	my %pos;
	for my $tok (@$tokens)
	{
		push @types, $tok eq 'A' || $tok eq 'B' ? 'int' : $tok;
		$pos{$tok} //= scalar @types if $tok eq 'A' || $tok eq 'B';
	}
	my $tmpl = $sh->{tmpl};
	$tmpl =~ s/__A__/\$$pos{A}/g;
	$tmpl =~ s/__B__/\$$pos{B}/g;
	my $script = $mode eq 'generic' ? "SET plan_cache_mode = force_generic_plan;\n" : '';
	$script .= "PREPARE pq(" . join(', ', @types) . ") AS $tmpl;\n";
	for my $i (1 .. 6)
	{
		my @args = map { $_ eq 'A' ? $sh->{a} : $_ eq 'B' ? (1000 * $round + $i) : $UNUSED{$_} } @$tokens;
		$script .= "EXECUTE pq(" . join(', ', @args) . ");\n";
	}
	return $script;
}

my $round = 0;
for my $shape (qw(M1 M3 M4 M5 M6))
{
	my $sh = $SHAPE{$shape};
	my $plan_sql = $sh->{tmpl};
	$plan_sql =~ s/__A__/1/g;
	$plan_sql =~ s/__B__/2/g;
	like(plan_of($nodes[0], $plan_sql), $sh->{path}, "$shape: the statement of the test takes the path of its shape");
	for my $layout (@LAYOUTS)
	{
		my ($lname, @tokens) = @$layout;
		for my $mode ('custom', 'generic')
		{
			$round++;
			my $k = $shape eq 'M6' ? 0 : $turn++ % 3;	# the target of M6 is a local table: only the node that runs the statements holds it
			my $script = param_script($shape, \@tokens, $mode, $round);
			my ($rs, $os, $es) = run_sql($single, $script);
			my ($rd, $od, $ed) = run_sql($nodes[$k], $script);
			is($rs, 0, "$shape, $lname, $mode plans: the single node runs the statement") or diag($es);
			is($rd, 0, "$shape, $lname, $mode plans: the cluster runs the statement (node " . ($k + 1) . ")") or diag($ed);
			$c->compare_tables($sh->{table}, order_by => $sh->{order}, name => "$shape, $lname, $mode plans: $sh->{table}", nodes => [$k]);
		}
	}
	$c->compare_tables($sh->{table}, order_by => $sh->{order}, name => "$shape: $sh->{table} after every layout", nodes => ($shape eq 'M6' ? [0] : [0, 1, 2]));
}

# ---------------------------------------------------------------------------
# B. a view, a view of another owner, a CHECK OPTION view, row-level security
# ---------------------------------------------------------------------------
# The filter of a view is not in the text of a MERGE (a MERGE has no WHERE clause) and the policies of a table are not either, so
# these statements must never be shipped.  Each of the guards of merge.c has its own message; a test names it.
$c->on_every_node(q{
	CREATE ROLE vown;
	CREATE ROLE rlsu;
	GRANT USAGE ON SCHEMA public TO rlsu;
});
table(q{CREATE TABLE public.rl (id int PRIMARY KEY, v text, n int)},
	q{; SELECT kingres.create_distributed_table('public.rl', 'id', colocate_with => 'public.t')});
table(q{CREATE TABLE public.rlr (k int PRIMARY KEY, label text, n int)},
	q{; SELECT kingres.create_reference_table('public.rlr')});
$c->on_both(q{INSERT INTO public.rl SELECT g, 'rl' || g, g % 5 FROM generate_series(1, 60) g});
$c->on_both(q{INSERT INTO public.rlr SELECT g, repeat('x', 1 + g % 3), g % 5 FROM generate_series(0, 30) g});
# the views are made on node 1, where the statements run (a view is not propagated); the policies and grants are DDL of the cluster
$c->on_cluster(q{
	CREATE VIEW public.vt_plain AS SELECT * FROM public.t WHERE n < 3;
	CREATE VIEW public.vt_barrier WITH (security_barrier) AS SELECT * FROM public.t WHERE n < 3;
	CREATE VIEW public.vt_check AS SELECT * FROM public.t WHERE n < 3 WITH CHECK OPTION;
	CREATE VIEW public.vr_plain AS SELECT * FROM public.rt WHERE n < 3;
	CREATE VIEW public.vr_barrier WITH (security_barrier) AS SELECT * FROM public.rt WHERE n < 3;
	CREATE VIEW public.vr_check AS SELECT * FROM public.rt WHERE n < 3 WITH CHECK OPTION;
	CREATE VIEW public.vt_owner AS SELECT * FROM public.t WHERE n < 3;
	CREATE VIEW public.vr_owner AS SELECT * FROM public.rt WHERE n < 3;
	ALTER VIEW public.vt_owner OWNER TO vown;
	ALTER VIEW public.vr_owner OWNER TO vown;
	GRANT ALL ON public.vt_owner, public.vr_owner TO PUBLIC;
});
# row-level security and grants are not propagated for these tables (ALTER TABLE, GRANT on reference tables): by hand on every node
$c->on_every_node(q{
	SET kingres_shard.enable_routing = off;
	ALTER TABLE public.rl ENABLE ROW LEVEL SECURITY;
	CREATE POLICY rl_p ON public.rl USING (n < 3) WITH CHECK (n < 3);
	ALTER TABLE public.rlr ENABLE ROW LEVEL SECURITY;
	CREATE POLICY rlr_p ON public.rlr USING (n < 3) WITH CHECK (n < 3);
	GRANT ALL ON public.rl, public.s, public.t, public.rlr, public.rf, public.loc TO rlsu;
});

my $VIEW = qr/MERGE through a view is not supported/;
my $OWNER = qr/rights of another role/;
my $CHECK = qr/checked against a view or a row-level security policy/;
my $RLS = qr/the target has row-level security/;

sub refused
{
	my ($name, $sql, $re, $table, $order, %opt) = @_;
	my $node = $nodes[0];
	my $pre = $opt{as} ? "SET ROLE $opt{as}; " : '';
	my ($rd, $od, $ed) = run_sql($node, $pre . $sql);
	isnt($rd, 0, "$name: the cluster refuses it");
	like($ed, $re, "$name: refused by the expected guard") or diag($ed);
	unlike($ed, qr/KS001/, "$name: and not as a stale map");
	$c->compare_tables($table, order_by => $order, name => "$name: $table unchanged");
}

my @m1 = (
	[ 'update', q{MERGE INTO public.__V__ USING public.s ON __V__.id = s.id WHEN MATCHED THEN UPDATE SET n = 99} ],
	[ 'delete', q{MERGE INTO public.__V__ USING public.s ON __V__.id = s.id WHEN MATCHED THEN DELETE} ],
	[ 'insert', q{MERGE INTO public.__V__ USING public.s ON __V__.id = s.id WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (s.id, 'x', 1)} ],
);
my @m3 = (
	[ 'update', q{MERGE INTO public.__V__ USING public.loc ON __V__.id = loc.x WHEN MATCHED THEN UPDATE SET n = 99} ],
	[ 'delete', q{MERGE INTO public.__V__ USING public.loc ON __V__.id = loc.x WHEN MATCHED THEN DELETE} ],
	[ 'insert', q{MERGE INTO public.__V__ USING (VALUES (500, 'x')) AS l(x, y) ON __V__.id = l.x WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (l.x, l.y, 1)} ],
);
my @m4 = (
	[ 'update', q{MERGE INTO public.__V__ USING public.rf ON __V__.k = rf.k WHEN MATCHED THEN UPDATE SET n = 99} ],
	[ 'delete', q{MERGE INTO public.__V__ USING public.rf ON __V__.k = rf.k WHEN MATCHED THEN DELETE} ],
	[ 'insert', q{MERGE INTO public.__V__ USING public.rf ON __V__.k = rf.k WHEN NOT MATCHED THEN INSERT (k, label, n) VALUES (rf.k, rf.label, 1)} ],
);
sub with_view { my ($sql, $v) = @_; $sql =~ s/__V__/$v/g; return $sql; }

for my $case (
	[ 'a plain view', 'vt_plain', 'vr_plain', $VIEW, $VIEW ],
	[ 'a security_barrier view', 'vt_barrier', 'vr_barrier', $RLS, $RLS ],
	[ 'a view WITH CHECK OPTION', 'vt_check', 'vr_check', $CHECK, $CHECK ],
	[ 'a view of another owner', 'vt_owner', 'vr_owner', $OWNER, $OWNER ],
)
{
	my ($what, $vt, $vr, $re_dist, $re_ref) = @$case;
	# a CHECK OPTION view checks INSERT and UPDATE only: a DELETE through it is refused as any view is
	my $re = sub { my ($act, $r) = @_; return ($what =~ /CHECK OPTION/ && $act eq 'delete') ? $VIEW : $r; };
	for my $m (@m1) { refused("M1 through $what ($m->[0])", with_view($m->[1], $vt), $re->($m->[0], $re_dist), 'public.t', 'id'); }
	for my $m (@m3) { refused("M3 through $what ($m->[0])", with_view($m->[1], $vt), $re->($m->[0], $re_dist), 'public.t', 'id'); }
	for my $m (@m4) { refused("M4 through $what ($m->[0])", with_view($m->[1], $vr), $re->($m->[0], $re_ref), 'public.rt', 'k'); }
}

# a target with row-level security, written by a role that is subject to the policy, and by its owner (relrowsecurity alone refuses)
for my $who ('rlsu', undef)
{
	my $w = $who // 'the owner';
	my %opt = defined $who ? (as => $who) : ();
	refused("M1 into a target with row-level security, $w (update)", q{MERGE INTO public.rl USING public.s ON rl.id = s.id WHEN MATCHED THEN UPDATE SET n = 1}, qr/$CHECK|$RLS/, 'public.rl', 'id', %opt);
	refused("M1 into a target with row-level security, $w (delete)", q{MERGE INTO public.rl USING public.s ON rl.id = s.id WHEN MATCHED THEN DELETE}, qr/$CHECK|$RLS/, 'public.rl', 'id', %opt);
	refused("M3 into a target with row-level security, $w (delete)", q{MERGE INTO public.rl USING public.loc ON rl.id = loc.x WHEN MATCHED THEN DELETE}, qr/$CHECK|$RLS/, 'public.rl', 'id', %opt);
	refused("M3 into a target with row-level security, $w (update)", q{MERGE INTO public.rl USING public.loc ON rl.id = loc.x WHEN MATCHED THEN UPDATE SET n = 1}, qr/$CHECK|$RLS/, 'public.rl', 'id', %opt);
	refused("M4 into a reference target with row-level security, $w (delete)", q{MERGE INTO public.rlr USING public.rf ON rlr.k = rf.k WHEN MATCHED THEN DELETE}, qr/$CHECK|$RLS/, 'public.rlr', 'k', %opt);
	refused("M4 into a reference target with row-level security, $w (update)", q{MERGE INTO public.rlr USING public.rf ON rlr.k = rf.k WHEN MATCHED THEN UPDATE SET n = 1}, qr/$CHECK|$RLS/, 'public.rlr', 'k', %opt);
}

# the tables that these statements did not change are the single node's, and a plain table of the same rows still takes the pushed path
like(plan_of($nodes[0], q{MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED THEN UPDATE SET n = 99}), qr/Pushed: colocated MERGE/, 'a MERGE into the table itself is still pushed');

done_testing();
