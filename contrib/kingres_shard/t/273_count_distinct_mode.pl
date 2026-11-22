# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/273_count_distinct_mode.pl: kingres_shard.count_distinct_mode (design/analytics-phase12-queries.md, 9.8, increment Q-S5).
# With 'approximate' the planner rewrites count(DISTINCT x) of a level that reads a distributed table into
# kingres_sketch.approx_count_distinct(x, p).  The test compares each rewritten statement with the statement that calls the sketch
# aggregate itself (the same sketch gives the same number), checks the plan, and checks what is left alone: other types, other
# levels, other tables, a database without kingres_sketch.  Needs kingres_sketch built and installed into the same installation.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2) = make_cluster('cdm', 2, extra => "kingres_shard.enable_partial_aggregation = on\n");
sub qs { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }
my $K = 'kingres_sketch';
$_->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch') for ($n1, $n2);

qs($n1, q{
	CREATE TABLE public.ev (id int PRIMARY KEY, u int, s text, grp int, f float8, ts timestamptz);
	INSERT INTO public.ev SELECT g, g % 3000, 'v' || (g % 2500), g % 7, (g % 1000) / 10.0, '2026-01-01'::timestamptz + (g % 900) * interval '1 hour' FROM generate_series(1, 30000) g;
	SELECT kingres.create_distributed_table('public.ev', 'id', bucket_count => 8);
	CREATE TABLE public.loc (id int PRIMARY KEY, u int);
	INSERT INTO public.loc SELECT g, g % 400 FROM generate_series(1, 4000) g;
	CREATE TABLE public.ref (id int PRIMARY KEY, u int);
	INSERT INTO public.ref SELECT g, g % 300 FROM generate_series(1, 3000) g;
	SELECT kingres.create_reference_table('public.ref');
	ANALYZE;
});

sub one { my ($sql, $pre) = @_; return qs($n1, ($pre // '') . $sql); }
my $A = "SET kingres_shard.count_distinct_mode = 'approximate'; ";
sub plan_of
{
	my ($sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($n1, ($pre // '') . "EXPLAIN (VERBOSE, COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# the default is exact, and the settings are what the design says
is(one('SHOW kingres_shard.count_distinct_mode'), 'exact', 'the default is exact');
is(one('SHOW kingres_shard.count_distinct_error_rate'), '0.01', 'the default error rate is 0.01');
is(one('SELECT count(DISTINCT u) FROM public.ev'), '3000', 'exact: count(DISTINCT u) is 3000');
unlike(plan_of('SELECT count(DISTINCT u) FROM public.ev'), qr/approx_count_distinct/, 'exact: the plan has no sketch');
for my $bad ("count_distinct_mode = 'bogus'", 'count_distinct_error_rate = 0.0001', 'count_distinct_error_rate = 0.2')
{
	my (undef, undef, $err) = run_sql($n1, "SET kingres_shard.$bad");
	like($err, qr/invalid value|outside the valid range/, "SET $bad is refused");
}

# the rewrite: the same number as the sketch aggregate called by hand
my @same = (
	['scalar', 'SELECT count(DISTINCT u) FROM public.ev', "SELECT $K.approx_count_distinct(u, 14) FROM public.ev"],
	['text', 'SELECT count(DISTINCT s) FROM public.ev', "SELECT $K.approx_count_distinct(s, 14) FROM public.ev"],
	['timestamptz', 'SELECT count(DISTINCT ts) FROM public.ev', "SELECT $K.approx_count_distinct(ts, 14) FROM public.ev"],
	['float8', 'SELECT count(DISTINCT f) FROM public.ev', "SELECT $K.approx_count_distinct(f, 14) FROM public.ev"],
	['grouped', 'SELECT grp, count(DISTINCT u) FROM public.ev GROUP BY grp ORDER BY grp', "SELECT grp, $K.approx_count_distinct(u, 14) FROM public.ev GROUP BY grp ORDER BY grp"],
	['FILTER', 'SELECT count(DISTINCT u) FILTER (WHERE grp = 2) FROM public.ev', "SELECT $K.approx_count_distinct(u, 14) FILTER (WHERE grp = 2) FROM public.ev"],
	['two in one list', 'SELECT count(DISTINCT u), count(DISTINCT s), count(*) FROM public.ev', "SELECT $K.approx_count_distinct(u, 14), $K.approx_count_distinct(s, 14), count(*) FROM public.ev"],
	['HAVING', 'SELECT grp FROM public.ev GROUP BY grp HAVING count(DISTINCT u) > 2000 ORDER BY grp', "SELECT grp FROM public.ev GROUP BY grp HAVING $K.approx_count_distinct(u, 14) > 2000 ORDER BY grp"],
	['a sub-select', 'SELECT sum(c) FROM (SELECT grp, count(DISTINCT u) AS c FROM public.ev GROUP BY grp) x', "SELECT sum(c) FROM (SELECT grp, $K.approx_count_distinct(u, 14) AS c FROM public.ev GROUP BY grp) x"],
	['a CTE', 'WITH t AS (SELECT grp, count(DISTINCT u) AS c FROM public.ev GROUP BY grp) SELECT max(c) FROM t', "WITH t AS (SELECT grp, $K.approx_count_distinct(u, 14) AS c FROM public.ev GROUP BY grp) SELECT max(c) FROM t"],
	['a scalar sub-select', 'SELECT (SELECT count(DISTINCT u) FROM public.ev)', "SELECT (SELECT $K.approx_count_distinct(u, 14) FROM public.ev)"],
);
for my $c (@same)
{
	my ($name, $sql, $by_hand) = @$c;
	my $want = one($by_hand);
	is(one($sql, $A), $want, "approximate, $name: the same number as the sketch called by hand");
}
my $approx = one('SELECT count(DISTINCT u) FROM public.ev', $A);
cmp_ok(abs($approx - 3000), '<=', 3000 * 0.03, "the estimate ($approx) is within 3% of the exact 3000");

# the plan
my $pl = plan_of('SELECT count(DISTINCT u) FROM public.ev', $A);
like($pl, qr/approx_count_distinct/, 'EXPLAIN shows approx_count_distinct');
unlike($pl, qr/count\(DISTINCT/, '... and no count(DISTINCT');
like($pl, qr/Kingres Fragment Scan|Partial/, '... and the groups answer with partial states (a fragment scan)');

# the error rate sets the precision
is(one("SET kingres_shard.count_distinct_error_rate = 0.005; SELECT count(DISTINCT u) FROM public.ev", $A), one("SELECT $K.approx_count_distinct(u, 16) FROM public.ev"),
   'error rate 0.005 gives precision 16');
is(one("SET kingres_shard.count_distinct_error_rate = 0.1; SELECT count(DISTINCT u) FROM public.ev", $A), one("SELECT $K.approx_count_distinct(u, 7) FROM public.ev"),
   'error rate 0.1 gives precision 7');
is(one("SET kingres_shard.count_distinct_error_rate = 0.0005; SELECT count(DISTINCT u) FROM public.ev", $A), one("SELECT $K.approx_count_distinct(u, 18) FROM public.ev"),
   'error rate 0.0005 is capped at precision 18');

# what is left alone
is(one('SELECT count(DISTINCT u ORDER BY u) FROM public.ev', $A), '3000', 'an aggregate with ORDER BY stays exact');
is(one('SELECT count(DISTINCT (u, grp)) FROM public.ev', $A), one('SELECT count(*) FROM (SELECT DISTINCT u, grp FROM public.ev) x'), 'a row-typed argument stays exact');
is(one('SELECT count(DISTINCT ARRAY[u]) FROM public.ev', $A), '3000', 'an array argument stays exact');
is(one('SELECT count(DISTINCT u) FROM public.loc', $A), '400', 'a local table stays exact');
like(plan_of('SELECT count(DISTINCT u) FROM public.loc', $A), qr/count\(DISTINCT/, '... in the plan too');
is(one('SELECT count(DISTINCT u) FROM public.ref', $A), '300', 'a reference table stays exact');
is(one('SELECT count(*) FROM (SELECT count(DISTINCT u) AS c FROM public.loc) x, (SELECT count(*) FROM public.ev) y', $A), '1', 'a level without a distributed table next to one with');
{
	my $mixed = plan_of('SELECT (SELECT count(DISTINCT u) FROM public.loc), count(DISTINCT u) FROM public.ev', $A);
	like($mixed, qr/count\(DISTINCT/, 'in one statement: the local level is exact');
	like($mixed, qr/approx_count_distinct/, '... and the distributed level is approximate');
}
is(one('SELECT count(DISTINCT u) FROM public.ev WHERE false', $A), '0', 'an empty input counts 0');
is(one('SELECT count(DISTINCT u) FROM public.ev WHERE id = 5', $A), '1', 'a statement routed to one bucket counts 1');

# the plans say the same: what is left alone keeps count(DISTINCT), what is rewritten loses it
for my $c (['an aggregate with ORDER BY', 'SELECT count(DISTINCT u ORDER BY u) FROM public.ev', 0],
           ['a row-typed argument', 'SELECT count(DISTINCT (u, grp)) FROM public.ev', 0],
           ['an array argument', 'SELECT count(DISTINCT ARRAY[u]) FROM public.ev', 0],
           ['HAVING', 'SELECT grp FROM public.ev GROUP BY grp HAVING count(DISTINCT u) > 2000', 1],
           ['a FILTER', 'SELECT count(DISTINCT u) FILTER (WHERE grp = 2) FROM public.ev', 1],
           ['a sub-select', 'SELECT sum(c) FROM (SELECT grp, count(DISTINCT u) AS c FROM public.ev GROUP BY grp) x', 1],
           ['a reference table', 'SELECT count(DISTINCT u) FROM public.ref', 0])
{
	my ($name, $sql, $rewritten) = @$c;
	my $p = plan_of($sql, $A);
	if ($rewritten) { like($p, qr/approx_count_distinct/, "plan: $name is rewritten"); unlike($p, qr/count\(DISTINCT/, "plan: ... and has no count(DISTINCT)"); }
	else { like($p, qr/count\(DISTINCT/, "plan: $name keeps count(DISTINCT)"); unlike($p, qr/approx_count_distinct/, "plan: ... and has no sketch"); }
}


# a session sets it for itself; another session is not affected
is(one('SELECT count(DISTINCT u) FROM public.ev'), '3000', 'a session that did not set it is exact');
# ALTER DATABASE SET: the way the console turns it on
qs($n1, "ALTER DATABASE postgres SET kingres_shard.count_distinct_mode = 'approximate'");
is(one('SELECT count(DISTINCT u) FROM public.ev'), one("SELECT $K.approx_count_distinct(u, 14) FROM public.ev"), 'a database default of approximate reaches a new session');
qs($n1, 'ALTER DATABASE postgres RESET kingres_shard.count_distinct_mode');
is(one('SELECT count(DISTINCT u) FROM public.ev'), '3000', 'and reset it is exact again');

# the types whose hash depends on an object identifier stay exact (review P12S-06)
qs($n1, q{
	CREATE TABLE public.oidt (id int PRIMARY KEY, o oid, rc regclass, ov oidvector);
	INSERT INTO public.oidt SELECT g, g % 10, 'pg_class'::regclass, '1 2'::oidvector FROM generate_series(1, 100) g;
	SELECT kingres.create_distributed_table('public.oidt', 'id', bucket_count => 4);
});
for my $col (qw(o rc ov))
{
	my $sql = "SELECT count(DISTINCT $col) FROM public.oidt";
	unlike(plan_of($sql, $A), qr/approx_count_distinct/, "plan: count(DISTINCT $col) of an oid-like type is not rewritten");
	is(one($sql, $A), one($sql), "count(DISTINCT $col) gives the exact answer in approximate mode");
}

# a role that may not use kingres_sketch is not locked out, and gets the exact answer (review P12S-01)
qs($_, "CREATE ROLE appu LOGIN") for ($n1, $n2);
qs($n1, "GRANT SELECT ON public.ev TO appu");
qs($n1, "ALTER DATABASE postgres SET kingres_shard.count_distinct_mode = 'approximate'");
{
	my ($rc, $out, $err) = run_sql($n1, "SET ROLE appu; SELECT 1; SELECT count(*) FROM public.ev; SELECT count(DISTINCT u) FROM public.ev; SELECT count(DISTINCT u) FROM public.ev;");
	is($rc, 0, 'a role without USAGE on the schema of kingres_sketch: no statement fails') or diag($err);
	is($out =~ s/\s+//gr, '13000030003000', '... and count(DISTINCT) stays exact');
	my $w = () = $err =~ /this role may not use kingres_sketch/g;
	is($w, 1, '... with one warning for the session');
}
qs($_, "GRANT USAGE ON SCHEMA kingres_sketch TO appu") for ($n1, $n2);
is(one("SET ROLE appu; SELECT count(DISTINCT u) FROM public.ev"), one("SELECT $K.approx_count_distinct(u, 14) FROM public.ev"), 'with USAGE on the schema the role gets the sketch');
qs($n1, "REVOKE ALL ON FUNCTION $K.approx_count_distinct(anyelement, int) FROM PUBLIC");
{
	my ($rc, $out, $err) = run_sql($n1, "SET ROLE appu; SELECT count(DISTINCT u) FROM public.ev;");
	is($rc, 0, 'without EXECUTE on the aggregate the statement does not fail') or diag($err);
	is($out =~ s/\s+//gr, '3000', '... and stays exact');
	like($err, qr/this role may not use kingres_sketch/, '... with the warning');
}
qs($n1, "GRANT ALL ON FUNCTION $K.approx_count_distinct(anyelement, int) TO PUBLIC");
qs($n1, "ALTER DATABASE postgres RESET kingres_shard.count_distinct_mode");
qs($_, "REVOKE USAGE ON SCHEMA kingres_sketch FROM appu") for ($n1, $n2);

# a cached plan does not keep the rewrite when the setting changes, in either direction (review P12S-02)
{
	my $cached = "SET plan_cache_mode = force_generic_plan; PREPARE pq AS SELECT count(DISTINCT id) FROM public.ev; ";
	my $exact_id = one('SELECT count(DISTINCT id) FROM public.ev');
	my $approx_id = one("SELECT $K.approx_count_distinct(id, 14) FROM public.ev");
	diag("count(DISTINCT id): exact $exact_id, approximate $approx_id");
	my ($rc, $out, $err) = run_sql($n1, $cached . "EXECUTE pq; SET kingres_shard.count_distinct_mode = 'approximate'; EXECUTE pq; SET kingres_shard.count_distinct_mode = 'exact'; EXECUTE pq;");
	is($rc, 0, 'a prepared statement runs through three changes of the setting') or diag($err);
	is(join(',', split /\s+/, $out), "$exact_id,$approx_id,$exact_id", 'exact, then approximate, then exact again: the generic plan is not kept');
	($rc, $out, $err) = run_sql($n1, $cached . "EXECUTE pq; SET kingres_shard.count_distinct_mode = 'approximate'; EXPLAIN (COSTS OFF) EXECUTE pq; SET kingres_shard.count_distinct_mode = 'exact'; EXPLAIN (COSTS OFF) EXECUTE pq;");
	my @plans = split /QUERY PLAN/, $out;
	is(scalar(grep { /approx_count_distinct/ } @plans), 1, 'EXPLAIN EXECUTE: the plan after SET approximate has the sketch, the other plans do not');
	# a function body is planned once per session: the same
	qs($n1, q{CREATE FUNCTION public.cdf() RETURNS bigint LANGUAGE plpgsql AS $f$ BEGIN RETURN (SELECT count(DISTINCT id) FROM public.ev); END $f$});
	($rc, $out, $err) = run_sql($n1, "SELECT public.cdf(); SELECT public.cdf(); SELECT public.cdf(); SET kingres_shard.count_distinct_mode = 'approximate'; SELECT public.cdf(); SET kingres_shard.count_distinct_mode = 'exact'; SELECT public.cdf();");
	is(join(',', split /\s+/, $out), "$exact_id,$exact_id,$exact_id,$approx_id,$exact_id", 'a plpgsql function follows the setting too');
	# the error rate is part of the plan
	($rc, $out, $err) = run_sql($n1, $cached . "SET kingres_shard.count_distinct_mode = 'approximate'; EXECUTE pq; SET kingres_shard.count_distinct_error_rate = 0.005; EXECUTE pq;");
	my @v = split /\s+/, $out;
	is($v[1], one("SELECT $K.approx_count_distinct(id, 16) FROM public.ev"), 'a change of the error rate drops the cached plan too');
	qs($n1, 'DROP FUNCTION public.cdf()');
}

# one shard group: the answer does not depend on the number of groups (review P12S-06)
{
	my ($s1) = make_cluster('cd1', 1);
	$s1->safe_psql('postgres', q{
		CREATE EXTENSION kingres_sketch;
		CREATE TABLE public.one (id int PRIMARY KEY, u int);
		INSERT INTO public.one SELECT g, g % 500 FROM generate_series(1, 5000) g;
		SELECT kingres.create_distributed_table('public.one', 'id', bucket_count => 4);
	});
	my ($rc, $out, $err) = run_sql($s1, "SET kingres_shard.count_distinct_mode = 'approximate'; EXPLAIN (VERBOSE, COSTS OFF) SELECT count(DISTINCT u) FROM public.one");
	like($out, qr/approx_count_distinct/, 'with one shard group the statement is rewritten too');
	is($s1->safe_psql('postgres', "SET kingres_shard.count_distinct_mode = 'approximate'; SELECT count(DISTINCT u) FROM public.one"),
		$s1->safe_psql('postgres', "SELECT $K.approx_count_distinct(u, 14) FROM public.one"), '... and gives the number of the sketch');
}

# kingres_shard.shippable_extensions = none: the sketch may not be used, so the setting does nothing either
{
	my ($rc, $out, $err) = run_sql($n1, $A . "SET kingres_shard.shippable_extensions = 'none'; SELECT count(DISTINCT u) FROM public.ev");
	is($out =~ s/\s+//gr, '3000', 'with shippable_extensions = none the count stays exact');
	like($err, qr/count_distinct_mode is approximate but kingres_sketch/, '... and says why');
}

# without kingres_sketch the setting does nothing and says so once per session
qs($n1, 'DROP EXTENSION kingres_sketch');
my ($rc, $out, $err) = run_sql($n1, $A . "SELECT count(DISTINCT u) FROM public.ev; SELECT count(DISTINCT u) FROM public.ev;");
is($out =~ s/\s+//gr, '30003000', 'without kingres_sketch the count stays exact');
my $warnings = () = $err =~ /count_distinct_mode is approximate but kingres_sketch is not installed/g;
is($warnings, 1, 'and one warning is raised for the session');
qs($n1, 'CREATE EXTENSION kingres_sketch');

is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for ($n1, $n2);
done_testing();
