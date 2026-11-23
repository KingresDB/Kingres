# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/252_extension_search_path.pl: the privileged SQL of the extension does not use the search_path of the session (XSP-01, XSP-02, XSP-03).
#
#   The ProcessUtility hook runs a few statements as the bootstrap superuser (the table-operation check of claimed_relid_check, the
#   bucket-move check of kingres_shard_assert_no_move) and a few as the calling role.  A database owner chooses the search_path of the
#   database; an unqualified "=" (an IN list builds one) would then find an operator that the owner planted in its own schema.
#
#   The test is defensive: the planted function only records that it ran, in a table that the owner owns, and returns the true answer.
#   A. the plant works: a statement of the owner that compares two texts under the database search_path calls it (the control)
#   B. DDL by the owner of distributed tables (ADD COLUMN, CREATE INDEX: both checks; CREATE TABLE IF NOT EXISTS ... PARTITION OF
#      and DROP TABLE of a partition name that does not exist: the lookups in the catalog of user partitions) never calls the planted function, and the DDL is applied on every node
#   C. the search_path of the session is the one the owner set, after the DDL (the forced path is gone again)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @nodes = make_cluster('xs', 2);
my ($n1, $n2) = @nodes;
sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }
sub as_own { my ($sql) = @_; return run_sql($n1, $sql, extra_params => [ '-U', 'own' ]); }

$_->safe_psql('postgres', 'CREATE ROLE own LOGIN; GRANT CREATE ON SCHEMA public TO own') for @nodes;
q1($n1, q{
	CREATE TABLE public.xt (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.xt', 'id', bucket_count => 4);
	INSERT INTO public.xt SELECT g, 'v' || g FROM generate_series(1, 20) g;
	ALTER TABLE public.xt OWNER TO own;
	CREATE TABLE public.xp (id int NOT NULL, v text, PRIMARY KEY (id)) PARTITION BY RANGE (id);
	CREATE TABLE public.xp_p1 PARTITION OF public.xp FOR VALUES FROM (0) TO (100);
	SELECT kingres.create_distributed_table('public.xp', 'id', bucket_count => 4);
	ALTER TABLE public.xp OWNER TO own;
});
# own is a database owner and not a superuser, and holds no other privilege
q1($n1, 'ALTER DATABASE postgres OWNER TO own');
is(q1($n1, q{SELECT rolsuper FROM pg_roles WHERE rolname = 'own'}), 'f', 'the owner is not a superuser');

# The checks run only over rows: a statement that finds no row never calls an operator.  History rows of a finished table
# operation, bucket move and bucket split (phase done) make every phase comparison of the checks run, and none of them refuses the DDL.
q1($n1, q{
	INSERT INTO kingres.table_operation (kind, mode, phase, spec, started_at, updated_at)
		VALUES ('alter', 'online', 'done', '{}', now(), now());
	INSERT INTO kingres.table_operation_item (op_id, ordinal, relid)
		SELECT max(op_id), 0, 'public.xt'::regclass FROM kingres.table_operation;
	INSERT INTO kingres.bucket_move (colocation_id, bucket, source_group, target_group, phase, started_at, updated_at)
		SELECT colocation_id, 0, 1, 2, 'done', now(), now() FROM kingres.dist_table WHERE relid = 'public.xt'::regclass;
	INSERT INTO kingres.bucket_split (colocation_id, bucket, group_id, modulus, parts, phase, started_at, updated_at)
		SELECT colocation_id, 0, 1, 4, 2, 'done', now(), now() FROM kingres.dist_table WHERE relid = 'public.xt'::regclass;
});

# the plant: an operator =(text, text) in a schema of the owner; its function records the call and gives the true answer
my ($rc, $out, $err) = as_own(q{
	CREATE SCHEMA evil;
	CREATE TABLE evil.calls (who text);
	CREATE FUNCTION evil.eq(text, text) RETURNS boolean LANGUAGE plpgsql AS $f$
	BEGIN
		INSERT INTO evil.calls VALUES (current_user);
		RETURN pg_catalog.texteq($1, $2);
	END
	$f$;
	CREATE FUNCTION evil.eq(name, text) RETURNS boolean LANGUAGE plpgsql AS $f$
	BEGIN
		INSERT INTO evil.calls VALUES (current_user);
		RETURN pg_catalog.texteq($1::text, $2);
	END
	$f$;
	CREATE OPERATOR evil.= (LEFTARG = text, RIGHTARG = text, FUNCTION = evil.eq);
	CREATE OPERATOR evil.= (LEFTARG = name, RIGHTARG = text, FUNCTION = evil.eq);
});
is($err, '', 'the owner plants the operator in its own schema') or diag($err);
($rc, $out, $err) = as_own(q{ALTER DATABASE postgres SET search_path = evil, pg_catalog});
is($err, '', 'the owner sets the search_path of the database') or diag($err);

# A
($rc, $out, $err) = as_own(q{SELECT 'a'::text = 'a'::text});
is($err, '', 'A: the control statement runs') or diag($err);
cmp_ok(q1($n1, 'SELECT count(*) FROM evil.calls'), '>', 0, 'A: the planted operator is chosen under the database search_path');
q1($n1, 'TRUNCATE evil.calls');

# B
for my $sql ('ALTER TABLE public.xt ADD COLUMN z int',
			 'CREATE INDEX xt_v_idx ON public.xt (v)',
			 'DROP TABLE IF EXISTS public.xt_not_there',
			 'CREATE TABLE IF NOT EXISTS public.xp_p1 PARTITION OF public.xp FOR VALUES FROM (0) TO (100)',
			 'DROP TABLE IF EXISTS public.xp_p9')
{
	($rc, $out, $err) = as_own($sql);
	is($err =~ /ERROR/ ? $err : '', '', "B: $sql") or diag($err);
}
is(q1($n1, 'SELECT count(*) FROM evil.calls'), 0, 'B: the planted operator did not run during the checks of the extension')
  or diag(q1($n1, 'SELECT who, count(*) FROM evil.calls GROUP BY who'));
for my $n (@nodes)
{
	is($n->safe_psql('postgres', q{SELECT count(*) FROM pg_attribute WHERE attrelid = 'public.xt'::regclass AND attname = 'z' AND NOT attisdropped}),
		'1', 'B: the column is on ' . $n->name);
}

# C
($rc, $out, $err) = as_own(q{SHOW search_path});
is($out, 'evil, pg_catalog', 'C: the session keeps the path the owner chose');

q1($n1, 'ALTER DATABASE postgres RESET search_path');
done_testing();
