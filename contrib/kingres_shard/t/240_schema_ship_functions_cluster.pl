# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/240_schema_ship_functions_cluster.pl
#
# Phase 11 (design/shard-phase11.md 6.4, section 16 step 8): ship_functions, on three nodes.  A statement that calls functions of a distributed
# schema that ships its functions is run whole on the owner, with the search_path of the session that issued it; a function that is not of a
# distributed schema is never shipped; a function body that touches another owner's schema is refused, not answered wrongly.
#
#   A. the flag is off by default: the statement stays at the origin (the scan), and says why when it is a write
#   B. the flag on: reads and writes are shipped, a function body with unqualified names sees the path of the session
#   C. a function that is not of a distributed schema is never shipped (the answer is still right)
#   D. a function of a schema that does not ship keeps the statement at the origin; a body that touches another owner is refused
#   E. rights: the function runs as the calling role; a prepared statement; the flag off again

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('sf', 3);
my ($n1, $n2, $n3) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

sub fails
{
	my ($n, $sql, $re, $name) = @_;
	my ($rc, $out, $err) = run_sql($n, $sql);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "... message: $name") or diag($err);
}

q1($n1, q{SELECT kingres.set_sharding_mode('both')});
q1($_, q{CREATE ROLE sfuser LOGIN}) for @nodes;
q1($n1, q{CREATE SCHEMA fa; CREATE SCHEMA fb});
q1($n1, q{SELECT kingres.distribute_schema('fa', 1); SELECT kingres.distribute_schema('fb', 2)});
q1($n1, q{
	CREATE TABLE fa.t (id int PRIMARY KEY, n int);
	CREATE TABLE fb.w (id int PRIMARY KEY, n int);
	INSERT INTO fa.t SELECT g, g * 10 FROM generate_series(1, 20) g;
	INSERT INTO fb.w SELECT g, g * 100 FROM generate_series(1, 20) g;
});
# a function with unqualified names: its body means what the path of the caller says
q1($n1, q{SET search_path = fa, pg_catalog; CREATE FUNCTION getn(i int) RETURNS int LANGUAGE sql STABLE AS 'SELECT n FROM t WHERE id = i'});
q1($n1, q{CREATE FUNCTION fa.plus(i int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT i + 1000'});
q1($n1, q{CREATE FUNCTION fb.peek(i int) RETURNS int LANGUAGE sql STABLE AS 'SELECT n FROM fb.w WHERE id = i'});
is(q1($n3, q{SELECT count(*) FROM pg_proc WHERE proname IN ('getn', 'plus', 'peek')}), 3, 'the functions are on every node');
q1($n1, q{CREATE FUNCTION public.lf(i int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT i * 2'});

my $plan = sub { my ($n, $sql, $pre) = @_; return q1($n, ($pre // '') . "EXPLAIN (COSTS off) $sql"); };

# ---------------------------------------------------------------------------
# A. off by default
# ---------------------------------------------------------------------------
is(q1($n1, q{SELECT ship_functions FROM kingres.dist_schema WHERE nsp = 'fa'::regnamespace}), 'f', 'the flag is off by default');
like($plan->($n1, q{SELECT fa.plus(n) FROM fa.t WHERE id = 3}), qr/Kingres Distributed Scan/, 'a read that calls a function of the schema is the scan');
is(q1($n1, q{SELECT fa.plus(n) FROM fa.t WHERE id = 3}), 1030, '... and the answer is right');
fails($n1, q{INSERT INTO fa.t SELECT 100, fa.plus(1)}, qr/the schema does not ship functions/, 'a write that calls a function of the schema');

# ---------------------------------------------------------------------------
# B. on
# ---------------------------------------------------------------------------
q1($n1, q{SELECT kingres.alter_distributed_schema('fa', true)});
is(q1($n3, q{SELECT ship_functions FROM kingres.dist_schema WHERE nsp = 'fa'::regnamespace}), 't', 'the flag reached every node');
for my $n (@nodes)
{
	is(q1($n, q{SELECT fa.plus(n) FROM fa.t WHERE id = 3}), 1030, $n->name . ': a read that calls a function of the schema');
}
like($plan->($n1, q{SELECT fa.plus(n) FROM fa.t WHERE id = 3}), qr/Kingres Remote Statement/, 'it is shipped whole');
unlike($plan->($n2, q{SELECT fa.plus(n) FROM fa.t WHERE id = 3}), qr/Kingres/, '... the owner plans it alone');
q1($n1, q{INSERT INTO fa.t SELECT 100, fa.plus(1)});
is(q1($n2, q{SELECT n FROM fa.t WHERE id = 100}), 1001, 'a write that calls a function of the schema is shipped');
# the body resolves t through the path of the caller: the owner runs the statement with the path of the session
for my $n (@nodes)
{
	is(q1($n, q{SET search_path = fa, pg_catalog; SELECT getn(id) FROM fa.t WHERE id = 5}), 50, $n->name . ': a function with an unqualified name, with the path of the session');
}
like($plan->($n1, q{SELECT getn(id) FROM fa.t WHERE id = 5}, q{SET search_path = fa, pg_catalog; }), qr/Kingres Remote Statement/, '... it is shipped');
fails($n1, q{SET search_path = public; SELECT fa.getn(id) FROM fa.t WHERE id = 5}, qr/relation "t" does not exist/, 'with a path that does not name the schema the body does not find its table (as on a direct connection)');
# the temporary schema is not sent
is(q1($n1, q{CREATE TEMP TABLE t (id int, n int); INSERT INTO t VALUES (5, -1); SET search_path = pg_temp, fa, pg_catalog; SELECT fa.getn(id) FROM fa.t WHERE id = 5}), 50, 'the temporary schema of the session is not part of the path that the owner gets');
is(prepared_count($_), 0, $_->name . ': nothing is prepared') for @nodes;

# ---------------------------------------------------------------------------
# C. a function that is not of a distributed schema
# ---------------------------------------------------------------------------
like($plan->($n1, q{SELECT public.lf(n) FROM fa.t WHERE id = 3}), qr/Kingres Distributed Scan/, 'a function of public is not shipped');
is(q1($n1, q{SELECT public.lf(n) FROM fa.t WHERE id = 3}), 60, '... and the statement works: it runs here (the function exists on this node only)');
fails($n1, q{INSERT INTO fa.t SELECT 101, public.lf(2)}, qr/calls a function that is not in a distributed schema/, 'a write that calls it is refused with the reason');

# ---------------------------------------------------------------------------
# D. a schema that does not ship; another owner
# ---------------------------------------------------------------------------
like($plan->($n1, q{SELECT fb.peek(id) FROM fa.t WHERE id = 3}), qr/Kingres Distributed Scan/, 'a function of a schema that does not ship keeps the statement here');
is(q1($n1, q{SELECT fb.peek(id) FROM fa.t WHERE id = 3}), 300, '... and the body is routed from here: the right answer');
q1($n1, q{SELECT kingres.alter_distributed_schema('fb', true)});
fails($n1, q{SELECT fb.peek(id) FROM fa.t WHERE id = 3}, qr/does not hold table "fb\.w"/, 'a body that touches the schema of another owner is refused when the statement is shipped');
q1($n1, q{SELECT kingres.alter_distributed_schema('fb', false)});
is(q1($n1, q{SELECT fb.peek(id) FROM fa.t WHERE id = 3}), 300, 'with the flag off again it answers');

# ---------------------------------------------------------------------------
# E. rights, a prepared statement, the flag off again
# ---------------------------------------------------------------------------
q1($n1, q{GRANT USAGE ON SCHEMA fa TO sfuser; GRANT EXECUTE ON FUNCTION fa.plus(int), fa.getn(int) TO sfuser});
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT fa.plus(n) FROM fa.t WHERE id = 3}, extra_params => [ '-U', 'sfuser' ]);
	isnt($rc, 0, 'a role without SELECT on the table cannot read it through a shipped statement');
	like($err, qr/permission denied/, '... the error is a permission error') or diag($err);
	q1($n1, q{GRANT SELECT ON fa.t TO sfuser});
	($rc, $out, $err) = run_sql($n1, q{SELECT fa.plus(n) FROM fa.t WHERE id = 3}, extra_params => [ '-U', 'sfuser' ]);
	is($out, '1030', 'after the GRANT it can: the function runs with the rights of the caller') or diag($err);
	($rc, $out, $err) = run_sql($n1, q{SET search_path = fa, pg_catalog; SELECT getn(id) FROM fa.t WHERE id = 6}, extra_params => [ '-U', 'sfuser' ]);
	is($out, '60', 'and a function with an unqualified name') or diag($err);
}
{
	my ($rc, $out, $err) = run_sql($n3, q{
		PREPARE pf (int) AS SELECT fa.plus(n) FROM fa.t WHERE id = $1;
		EXECUTE pf(1); EXECUTE pf(2); EXECUTE pf(3); EXECUTE pf(4); EXECUTE pf(5); EXECUTE pf(6); EXECUTE pf(7);
	});
	is($rc, 0, 'a prepared statement that calls a function of the schema') or diag($err);
	is($out, join("\n", map { 1000 + $_ * 10 } 1 .. 7), '... the answers');
}
q1($n1, q{SELECT kingres.alter_distributed_schema('fa', false)});
like($plan->($n1, q{SELECT fa.plus(n) FROM fa.t WHERE id = 3}), qr/Kingres Distributed Scan/, 'the flag off again: the scan');
is(q1($n1, q{SELECT fa.plus(n) FROM fa.t WHERE id = 3}), 1030, '... and the answer is the same');
is(prepared_count($_), 0, $_->name . ': nothing is prepared') for @nodes;

done_testing();
