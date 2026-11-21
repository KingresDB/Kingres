# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/243_schema_review_cluster.pl
#
# The fixes for the independent review of phase 11, increments 1 to 6 (findings P11R-01 to P11R-14), on three nodes with logical replication.
#
#   01 a schema that moves back while the old copy is still being cleaned (the live rows must not be wiped)
#   02 schema names that are not plain lower case
#   03 the privileges, comment and default privileges of an empty schema reach every node
#   04 session built-ins are not evaluated on the owner      05 triggers with unqualified names      07 setval(s, n, false) across a move
#   08 REASSIGN OWNED and CREATE EXTENSION ... SCHEMA        10 UPDATE ... SET id = DEFAULT         11 check_function_bodies = off
#   12 a user operator in x = ANY (...)                       14 the size of a schema is for a superuser

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('rv', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

sub fails
{
	my ($n, $sql, $re, $name) = @_;
	my ($rc, $out, $err) = run_sql($n, $sql);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "... message: $name") or diag($err);
}

sub same_everywhere
{
	my ($sql, $expected, $name) = @_;
	is(q1($_, $sql), $expected, $_->name . ": $name") for @nodes;
}

q1($_, q{CREATE ROLE app LOGIN}) for @nodes;
q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});

# ---------------------------------------------------------------------------
# P11R-01
# ---------------------------------------------------------------------------
q1($n1, q{SET kingres_shard.schema_placement_group = 0; CREATE SCHEMA cl});
q1($n1, q{CREATE TABLE cl.t (id int PRIMARY KEY, v text)});
my $coloc = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'cl'::regnamespace});
q1($n1, q{SET kingres_shard.node_timeout = 20; SELECT kingres.move_schema('cl', 1)});
q1($n1, q{INSERT INTO cl.t SELECT g, 'r' FROM generate_series(1, 100) g});
fails($n1, q{SELECT kingres.move_schema('cl', 0)}, qr/the old copy of the schema of colocation group \d+ on shard group 0 is still being cleaned/, 'a move back while the old copy is still being cleaned');
same_everywhere(q{SELECT count(*) FROM cl.t}, 100, 'the rows are where they were');
q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
{
	my ($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.node_timeout = 20; SELECT kingres.move_schema('cl', 0)});
	is($rc, 0, 'after the cleaning the schema moves back') or diag($err);
}
q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
same_everywhere(q{SELECT count(*) FROM cl.t}, 100, 'and no row was lost');
# the cleaning of a group that holds the schema again is a no-op (defence behind the refusal)
q1($n1, q{SET kingres_shard.node_timeout = 20; SELECT kingres.move_schema('cl', 1)});
q1($n1, "SELECT kingres.internal_set_schema_group($coloc, 0, 'active')");
q1($n1, q{SET kingres_shard.enable_routing = off; INSERT INTO cl.t VALUES (1001, 'kept')});
q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
is(q1($n1, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM cl.t WHERE id = 1001}), 1, 'the cleaning of a group that owns the schema again does not empty it');
q1($n1, "SELECT kingres.internal_set_schema_group($coloc, 1, 'active')");
q1($n1, q{SET kingres_shard.enable_routing = off; DELETE FROM cl.t WHERE id = 1001});

# ---------------------------------------------------------------------------
# P11R-02
# ---------------------------------------------------------------------------
q1($n1, q{CREATE SCHEMA tw; CREATE SCHEMA "TW"; CREATE SCHEMA "Mixed"; CREATE SCHEMA "a.b"; CREATE SCHEMA "a b"; CREATE SCHEMA "q""uote"});
q1($n1, q{CREATE TABLE tw.t (id int PRIMARY KEY, v text); CREATE TABLE "TW".t (id int PRIMARY KEY, v text); CREATE TABLE "Mixed".t (id int PRIMARY KEY);
          CREATE TABLE "a.b".t (id int PRIMARY KEY); CREATE TABLE "a b".t (id int PRIMARY KEY); CREATE TABLE "q""uote".t (id int PRIMARY KEY)});
same_everywhere(q{SELECT count(*) FROM kingres.dist_table d JOIN pg_class c ON c.oid = d.relid JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname IN ('tw', 'TW', 'Mixed', 'a.b', 'a b', 'q"uote')}, 6, 'every table of a schema with an odd name is registered');
q1($n1, q{INSERT INTO "TW".t VALUES (1, 'from n1')});
q1($n2, q{INSERT INTO "TW".t VALUES (2, 'from n2')});
q1($n1, q{INSERT INTO tw.t VALUES (1, 'lower')});
same_everywhere(q{SELECT string_agg(v, ',' ORDER BY id) FROM "TW".t}, 'from n1,from n2', 'the twin of upper case is one table for every node');
same_everywhere(q{SELECT string_agg(v, ',' ORDER BY id) FROM tw.t}, 'lower', '... and the lower case one another');

# ---------------------------------------------------------------------------
# P11R-03
# ---------------------------------------------------------------------------
q1($n1, q{SET kingres_shard.distribute_new_schemas = off; CREATE SCHEMA e1; GRANT USAGE ON SCHEMA e1 TO app; COMMENT ON SCHEMA e1 IS 'the e1 schema';
          ALTER DEFAULT PRIVILEGES IN SCHEMA e1 GRANT SELECT ON TABLES TO app});
q1($n1, q{SELECT kingres.distribute_schema('e1', 1); CREATE TABLE e1.t (id int)});
same_everywhere(q{SELECT has_schema_privilege('app', 'e1', 'USAGE')}, 't', 'a grant on the schema made before the conversion is on every node');
same_everywhere(q{SELECT obj_description('e1'::regnamespace)}, 'the e1 schema', 'so is the comment');
same_everywhere(q{SELECT count(*) FROM pg_default_acl WHERE defaclnamespace = 'e1'::regnamespace}, 1, 'and the default privileges');
same_everywhere(q{SELECT has_table_privilege('app', 'e1.t', 'SELECT')}, 't', 'a table made afterwards has the default privilege');
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT count(*) FROM e1.t}, extra_params => [ '-U', 'app' ]);
	is($out, '0', 'the role reads it through the owner') or diag($err);
}

# ---------------------------------------------------------------------------
# P11R-04, 05, 10, 11, 12 on a schema held by node 2
# ---------------------------------------------------------------------------
q1($n1, q{SET kingres_shard.schema_placement_group = 1; CREATE SCHEMA s4});
q1($n1, q{CREATE TABLE s4.t (id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v text); CREATE TABLE s4.audit (msg text);
          CREATE FUNCTION s4.trg() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN INSERT INTO audit(msg) VALUES (NEW.v); RETURN NEW; END';
          CREATE TRIGGER t_aud AFTER INSERT ON s4.t FOR EACH ROW EXECUTE FUNCTION s4.trg()});
fails($n1, q{SET search_path = s4, public; INSERT INTO s4.t (v) VALUES (current_schema())}, qr/reads the state of the session/, 'a write that calls current_schema() from another node');
q1($n2, q{SET search_path = s4, public; INSERT INTO t (v) VALUES (current_schema())});
is(q1($n2, q{SELECT v FROM s4.t ORDER BY id LIMIT 1}), 's4', '... on the owner it is the owner session\'s path');
q1($n1, q{SET search_path = s4; INSERT INTO t (v) VALUES ('x')});
is(q1($n1, q{SET app.tenant = '42'; SELECT current_setting('app.tenant', true) FROM s4.t LIMIT 1}), '42', 'a read that calls current_setting() answers with the setting of this session');
is(q1($n1, q{SET kingres_shard.enable_schema_routing = off; SET app.tenant = '42'; SELECT current_setting('app.tenant', true) FROM s4.t LIMIT 1}), '42', '... with routing off too');
q1($n1, q{SET search_path = s4; INSERT INTO t (v) VALUES ('via trigger')});
is(q1($n2, q{SELECT count(*) FROM s4.audit WHERE msg = 'via trigger'}), 1, 'a trigger with an unqualified name works when the insert comes from another node');
fails($n1, q{UPDATE s4.t SET id = DEFAULT WHERE v = 'x'}, qr/it sets an identity column to DEFAULT|can only be updated to DEFAULT/, 'UPDATE ... SET id = DEFAULT of a generated identity from another node');
q1($n1, q{SET check_function_bodies = off; CREATE FUNCTION s4.later() RETURNS int LANGUAGE sql AS 'SELECT count(*)::int FROM s4.notyet'});
same_everywhere(q{SELECT count(*) FROM pg_proc WHERE proname = 'later'}, 1, 'check_function_bodies = off is carried by DDL');
q1($n1, q{CREATE FUNCTION public.eqf(int, int) RETURNS bool LANGUAGE sql IMMUTABLE AS 'SELECT $1 = $2'; CREATE OPERATOR public.=== (LEFTARG = int, RIGHTARG = int, FUNCTION = public.eqf)});
is(q1($n1, q{SELECT count(*) FROM s4.t WHERE id OPERATOR(public.===) ANY (ARRAY[1, 2, 3])}), 3, 'a user operator in x = ANY (...) keeps the statement here (the operator exists on this node only)');

# ---------------------------------------------------------------------------
# P11R-07
# ---------------------------------------------------------------------------
q1($n1, q{SET kingres_shard.schema_placement_group = 0; CREATE SCHEMA q7});
q1($n1, q{CREATE TABLE q7.t (id int PRIMARY KEY); CREATE SEQUENCE q7.sq; CREATE SEQUENCE q7.sq2});
q1($n1, q{SELECT setval('q7.sq', 5000, false); SELECT nextval('q7.sq2')});
q1($n1, q{SET kingres_shard.node_timeout = 20; SELECT kingres.move_schema('q7', 1)});
is(q1($n1, q{SELECT nextval('q7.sq')}), 5000, 'a sequence that was set with setval(s, n, false) goes on at n after a move');
is(q1($n1, q{SELECT nextval('q7.sq2')}), 2, 'a sequence that was used goes on after it');

# ---------------------------------------------------------------------------
# P11R-08
# ---------------------------------------------------------------------------
q1($_, q{CREATE ROLE oldowner}) for @nodes;
q1($n1, q{SET kingres_shard.schema_placement_group = 0; CREATE SCHEMA o8; ALTER SCHEMA o8 OWNER TO oldowner});
fails($n1, q{REASSIGN OWNED BY oldowner TO CURRENT_USER}, qr/REASSIGN OWNED of a role that owns a distributed schema/, 'REASSIGN OWNED of a role that owns a distributed schema');
same_everywhere(q{SELECT nspowner::regrole FROM pg_namespace WHERE nspname = 'o8'}, 'oldowner', '... the owner is the same everywhere');
q1($n1, q{CREATE ROLE idle; REASSIGN OWNED BY idle TO CURRENT_USER}) ;
fails($n1, q{CREATE EXTENSION IF NOT EXISTS plpgsql SCHEMA o8}, qr/CREATE EXTENSION \.\.\. SCHEMA a distributed schema is not supported/, 'CREATE EXTENSION into a distributed schema');

# ---------------------------------------------------------------------------
# P11R-14
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT count(size_bytes) FROM kingres.schemas}, extra_params => [ '-U', 'app' ]);
	is($out, '0', 'a role that is not a superuser sees no size of a schema') or diag($err);
	isnt(q1($n1, q{SELECT count(size_bytes) FROM kingres.schemas}), '0', '... a superuser does');
}

is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster is quiet') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));
is(prepared_count($_), 0, $_->name . ': no prepared transaction is left') for @nodes;

done_testing();
