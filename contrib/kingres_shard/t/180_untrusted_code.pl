# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/180_untrusted_code.pl: code of a role that is not a superuser is never run by the copy, the replay or the capture with
# raised rights (review of phase 9, P9B-01).  The first fix of P9A-05 followed only the functions that a default, a constraint
# or an index names; a domain check, an operator behind an index expression, a range type and a default of a domain reached the
# copy as well, and the owner's function ran as the superuser who called.
#
#   A. a domain CHECK that calls the function of an unprivileged role: the report names it, both calls refuse before anything
#      is done, and the role is still not a superuser
#   B. an operator of that role behind an index expression; a domain default; a range type with its function
#   C. the same objects owned by a superuser are accepted (the rule is about the owner of the code)
#   D. the capture stores the keys in the base type: a domain over a key column does not run its check in the trigger, and
#      the keys of a domain-keyed table are replayed

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2) = make_cluster('uc', 2);
my @nodes = ($n1, $n2);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

# the objects exist on every node (as an administrator who wants the tables to move would have made them)
sub everywhere { my ($sql) = @_; $_->safe_psql('postgres', $sql) for @nodes; }

everywhere(q{
	CREATE ROLE evil6 LOGIN; GRANT CREATE ON SCHEMA public TO evil6;
	CREATE TABLE public.evil_seen (who text, su text);
	GRANT INSERT ON public.evil_seen TO PUBLIC;
	CREATE FUNCTION public.chk6(int) RETURNS bool LANGUAGE plpgsql AS $f$
	BEGIN
		INSERT INTO public.evil_seen VALUES (current_user, current_setting('is_superuser'));
		RETURN true;
	END $f$;
	ALTER FUNCTION public.chk6(int) OWNER TO evil6;
	CREATE FUNCTION public.opf6(int, int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT $1 + $2';
	ALTER FUNCTION public.opf6(int, int) OWNER TO evil6;
	CREATE FUNCTION public.dflt6() RETURNS int LANGUAGE sql AS 'SELECT 7';
	ALTER FUNCTION public.dflt6() OWNER TO evil6;
	CREATE FUNCTION public.diff6(int, int) RETURNS float8 LANGUAGE sql IMMUTABLE AS 'SELECT ($1 - $2)::float8';
	ALTER FUNCTION public.diff6(int, int) OWNER TO evil6;
});

sub blockers
{
	my ($rel) = @_;
	return q1($n1, "SELECT coalesce(string_agg(code, ',' ORDER BY code), '') FROM kingres.table_change_report('distribute', '$rel', '{\"column\":\"id\"}') WHERE severity = 'blocker'");
}

sub still_plain
{
	my ($name, $t) = @_;
	is(q1($n1, "SELECT relkind FROM pg_class WHERE oid = 'public.$t'::regclass"), 'r', "$name: the table is still the plain table");
	is(q1($_, "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%' OR relname ~ '^${t}_b[0-9]{4}\$'"), '0',
		"$name: nothing is left on " . $_->name) for @nodes;
	is(q1($_, "SELECT count(*) FROM public.evil_seen WHERE su = 'on'"), '0', "$name: the code of the role never ran with superuser rights on " . $_->name) for @nodes;
}

sub refused
{
	my ($name, $t, $setup) = @_;
	$n1->safe_psql('postgres', "SET ROLE evil6; $setup");
	is(blockers("public.$t"), 'function_owner_not_superuser', "$name: the report names the code of the unprivileged role");
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.$t', 'id')");
	like($err, qr/function_owner_not_superuser:|who is not a superuser/s, "$name: the blocking call refuses") or diag($err);
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table_concurrently('public.$t', 'id')");
	like($err, qr/function_owner_not_superuser:|who is not a superuser/s, "$name: the online call refuses") or diag($err);
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.create_reference_table('public.$t')");
	like($err, qr/function_owner_not_superuser:|who is not a superuser/s, "$name: so does the reference table") or diag($err);
	still_plain($name, $t);
}

# A. a domain check
everywhere(q{CREATE DOMAIN public.d6 AS int CHECK (public.chk6(VALUE)); ALTER DOMAIN public.d6 OWNER TO evil6});
refused('domain check', 't6', q{CREATE TABLE public.t6 (id public.d6 PRIMARY KEY, v text); INSERT INTO public.t6 SELECT g, 'a' FROM generate_series(1, 10) g});

# B. an operator behind an index expression
everywhere(q{CREATE OPERATOR public.### (leftarg = int, rightarg = int, function = public.opf6)});
refused('operator in an index expression', 't6b', q{CREATE TABLE public.t6b (id int PRIMARY KEY, a int); CREATE INDEX t6b_x ON public.t6b ((a ### a)); INSERT INTO public.t6b SELECT g, g FROM generate_series(1, 10) g});
# a domain default
everywhere(q{CREATE DOMAIN public.dd6 AS int DEFAULT public.dflt6()});
refused('domain default', 't6c', q{CREATE TABLE public.t6c (id int PRIMARY KEY, a public.dd6); INSERT INTO public.t6c (id) SELECT g FROM generate_series(1, 10) g});
# a range type whose subtype_diff is the function of the role
everywhere(q{CREATE TYPE public.r6 AS RANGE (subtype = int, subtype_diff = public.diff6); ALTER TYPE public.r6 OWNER TO evil6});
refused('range type', 't6d', q{CREATE TABLE public.t6d (id int PRIMARY KEY, r public.r6); INSERT INTO public.t6d SELECT g, int4range(g, g + 5)::text::public.r6 FROM generate_series(1, 10) g});
# the function through a domain over a domain
everywhere(q{CREATE DOMAIN public.dn6 AS public.d6});
refused('nested domain', 't6e', q{CREATE TABLE public.t6e (id public.dn6 PRIMARY KEY); INSERT INTO public.t6e SELECT g FROM generate_series(1, 10) g});

# the hash function of the distribution column: the routing runs it for every row.  A type of a superuser whose default hash
# operator class names a function of the role (a type, an operator and an operator class can only be made by a superuser)
everywhere(q{
	CREATE TYPE public.ht6;
	CREATE FUNCTION public.ht6in(cstring) RETURNS public.ht6 AS 'int4in' LANGUAGE internal IMMUTABLE STRICT;
	CREATE FUNCTION public.ht6out(public.ht6) RETURNS cstring AS 'int4out' LANGUAGE internal IMMUTABLE STRICT;
	CREATE TYPE public.ht6 (INPUT = public.ht6in, OUTPUT = public.ht6out, LIKE = int4);
	CREATE FUNCTION public.ht6eq(public.ht6, public.ht6) RETURNS bool AS 'int4eq' LANGUAGE internal IMMUTABLE STRICT;
	CREATE OPERATOR public.=== (leftarg = public.ht6, rightarg = public.ht6, function = public.ht6eq, commutator = OPERATOR(public.===), hashes);
	CREATE FUNCTION public.ht6hash(public.ht6, int8) RETURNS int8 AS 'hashint4extended' LANGUAGE internal IMMUTABLE STRICT;
	ALTER FUNCTION public.ht6hash(public.ht6, int8) OWNER TO evil6;
	CREATE OPERATOR CLASS public.ht6_ops DEFAULT FOR TYPE public.ht6 USING hash AS OPERATOR 1 public.=== (public.ht6, public.ht6), FUNCTION 2 public.ht6hash(public.ht6, int8);
});
refused('hash function of the distribution column', 't6h', q{CREATE TABLE public.t6h (id public.ht6, v text)});

# C. the same objects of a superuser
everywhere(q{
	CREATE FUNCTION public.ok6(int) RETURNS bool LANGUAGE sql IMMUTABLE AS 'SELECT $1 > 0';
	CREATE FUNCTION public.okop6(int, int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT $1 + $2';
	CREATE OPERATOR public.#### (leftarg = int, rightarg = int, function = public.okop6);
	CREATE DOMAIN public.dok6 AS int CHECK (public.ok6(VALUE));
	CREATE TYPE public.rok6 AS RANGE (subtype = int);
});
$n1->safe_psql('postgres', q{
	CREATE TABLE public.t6ok (id public.dok6 PRIMARY KEY, a int, r public.rok6, v text DEFAULT 'x');
	CREATE INDEX t6ok_x ON public.t6ok ((a #### a));
	INSERT INTO public.t6ok SELECT g, g, int4range(g, g + 3)::text::public.rok6 FROM generate_series(1, 30) g;});
is(blockers('public.t6ok'), '', 'C: the code of a superuser is accepted');
$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.t6ok', 'id', bucket_count => 4)");
is(q1($_, 'SELECT count(*) FROM public.t6ok'), '30', 'C: the table with domain, operator and range type is distributed, seen from ' . $_->name) for @nodes;

# D. the keys of a domain-keyed table are captured in the base type and replayed
$n1->safe_psql('postgres', q{CREATE TABLE public.t6k (id public.dok6 PRIMARY KEY, v text); INSERT INTO public.t6k SELECT g, 'v' FROM generate_series(1, 50) g});
my $op = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.t6k', '{"column": "id", "bucket_count": 4}')});
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'copying')"), 'catching_up', 'D: the copy is done');
is(q1($n1, "SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'kingres.tlog_${op}_1'::regclass AND attname = 'k1'"), 'integer',
	'D: the log stores the key in the base type of the domain');
$n1->safe_psql('postgres', "UPDATE public.t6k SET v = 'changed' WHERE id = 5; DELETE FROM public.t6k WHERE id = 6; INSERT INTO public.t6k VALUES (100, 'new')");
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'switching')"), 'cleaning', 'D: the switch goes through');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'D: and the operation ends');
is(q1($n1, q{SELECT count(*) || '|' || count(*) FILTER (WHERE v = 'changed') || '|' || count(*) FILTER (WHERE id = 6) || '|' || count(*) FILTER (WHERE id = 100) FROM public.t6k}),
	'50|1|0|1', 'D: the changes of a domain-keyed table are in the new table');

is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for @nodes;
done_testing();
