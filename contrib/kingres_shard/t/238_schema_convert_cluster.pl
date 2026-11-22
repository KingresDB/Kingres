# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/238_schema_convert_cluster.pl
#
# Phase 11 (design/shard-phase11.md sections 8.1 and 8.2), increment 6: a schema that already has objects and rows becomes a distributed
# schema in place (distribute_schema), and becomes local again (undistribute_schema), on three nodes.
#
#   A. the conversion of a schema with types, sequences, tables (keys, foreign keys, a partitioned table), a view, functions, a trigger,
#      owners, privileges and comments: the rows stay, every other node gets the structure, empty, and the same fingerprints
#   B. the schema works from every node afterwards (reads, writes, the trigger, a function with ship_functions)
#   C. a schema whose rows are on this node cannot go to another group at once; a schema without rows can
#   D. what is refused: unsupported objects, a node that cannot take the structure (nothing is left anywhere)
#   E. undistribute_schema: shells must be empty, the owner keeps its rows, the others drop the schema; a round trip; from a non-owner
#   F. the same schema again, and check_cluster is quiet

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count start_psql finish_psql);

my @nodes = make_cluster('cv', 3);
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
	for my $n (@nodes)
	{
		is(q1($n, $sql), $expected, $n->name . ": $name");
	}
}

for my $n (@nodes)
{
	q1($n, q{CREATE ROLE cvowner; CREATE ROLE cvreader LOGIN});
}
q1($n1, q{SELECT kingres.set_sharding_mode('both')});
q1($n1, q{CREATE TABLE public.ref (id int PRIMARY KEY, name text); SELECT kingres.create_reference_table('public.ref'); INSERT INTO public.ref VALUES (1, 'r1'), (2, 'r2')});

# ---------------------------------------------------------------------------
# A. the conversion
# ---------------------------------------------------------------------------
q1($n1, q{
	CREATE SCHEMA conv;
	COMMENT ON SCHEMA conv IS 'a schema with data';
	CREATE TYPE conv.mood AS ENUM ('sad', 'ok', 'happy');
	CREATE DOMAIN conv.posint AS int CHECK (VALUE > 0);
	CREATE TYPE conv.pair AS (x int, y text);
	CREATE SEQUENCE conv.sq START 500 INCREMENT 5;
	CREATE FUNCTION conv.twice(i int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT i * 2';
	CREATE TABLE conv.a (id serial PRIMARY KEY, v text NOT NULL DEFAULT 'x', m conv.mood DEFAULT 'ok', n conv.posint, r int REFERENCES public.ref (id),
	                     CONSTRAINT a_v_len CHECK (length(v) < 50));
	CREATE INDEX a_v_idx ON conv.a (v);
	CREATE UNIQUE INDEX a_m_n ON conv.a (m, n) WHERE n IS NOT NULL;
	CREATE TABLE conv.b (id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, a_id int REFERENCES conv.a (id), note text, d date,
	                     g int GENERATED ALWAYS AS (id * 2) STORED);
	CREATE TABLE conv.p (id int, d date, PRIMARY KEY (id, d)) PARTITION BY RANGE (d);
	CREATE TABLE conv.p1 PARTITION OF conv.p FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
	CREATE TABLE conv.p2 PARTITION OF conv.p FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
	CREATE VIEW conv.v1 AS SELECT a.id, a.v, b.note FROM conv.a a LEFT JOIN conv.b b ON b.a_id = a.id;
	CREATE VIEW conv.v2 WITH (security_barrier = true) AS SELECT id, conv.twice(id) AS t FROM conv.v1;
	CREATE FUNCTION conv.stamp() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.note := coalesce(NEW.note, '') || '!'; RETURN NEW; END $$;
	CREATE TRIGGER b_stamp BEFORE INSERT ON conv.b FOR EACH ROW EXECUTE FUNCTION conv.stamp();
	CREATE FUNCTION conv.rows_of_a() RETURNS SETOF conv.a LANGUAGE sql STABLE AS 'SELECT * FROM conv.a';
	COMMENT ON TABLE conv.a IS 'the a table';
	COMMENT ON COLUMN conv.a.v IS 'the v column';
	COMMENT ON VIEW conv.v1 IS 'the first view';
	ALTER TABLE conv.b OWNER TO cvowner;
	GRANT SELECT ON conv.a TO cvreader;
	GRANT USAGE ON SCHEMA conv TO cvreader;
	GRANT EXECUTE ON FUNCTION conv.twice(int) TO cvreader;
	INSERT INTO conv.a (v, n, r) SELECT 'row ' || g, g, 1 + g % 2 FROM generate_series(1, 120) g;
	INSERT INTO conv.b (a_id, note, d) SELECT g, 'n' || g, date '2024-01-01' + g FROM generate_series(1, 60) g;
	INSERT INTO conv.p SELECT g, date '2024-01-01' + g * 5 FROM generate_series(1, 100) g;
	SELECT nextval('conv.sq'), nextval('conv.sq');
});

sub totals
{
	my ($n) = @_;
	return q1($n, q{SELECT (SELECT count(*) || ':' || sum(id) FROM conv.a) || '|' || (SELECT count(*) || ':' || sum(a_id) FROM conv.b)
	                || '|' || (SELECT count(*) || ':' || sum(id) FROM conv.p) || '|' || (SELECT count(*) FROM conv.v2)});
}
my $want = totals($n1);
is($want, '120:7260|60:1830|100:5050|120', 'the rows are the ones written');
my $fp_before = q1($n1, q{SELECT string_agg(c.relname || ':' || kingres.internal_table_fingerprint(c.oid), ',' ORDER BY c.relname)
                           FROM pg_class c WHERE c.relnamespace = 'conv'::regnamespace AND c.relkind IN ('r', 'p')});

my $coloc = q1($n1, q{SELECT kingres.distribute_schema('conv')});
like($coloc, qr/^\d+$/, 'distribute_schema returns the colocation id');
is(q1($n1, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'conv'}), 0, 'the owner is the group of this node, which holds the rows');
same_everywhere(q{SELECT group_id || ':' || table_count || ':' || state FROM kingres.schemas WHERE schema_name = 'conv'}, '0:5:active', 'the catalog says: group 0, five tables, active');
same_everywhere(q{SELECT count(*) FROM kingres.dist_table WHERE method = 'schema' AND colocation_id = (SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'conv'::regnamespace)}, 5, 'five schema tables');
same_everywhere(q{SELECT string_agg(c.relname || ':' || kingres.internal_table_fingerprint(c.oid), ',' ORDER BY c.relname)
                    FROM pg_class c WHERE c.relnamespace = 'conv'::regnamespace AND c.relkind IN ('r', 'p')}, $fp_before, 'every table has the structure it had');
is(totals($n1), $want, 'the owner keeps every row');
for my $n ($n2, $n3)
{
	is(q1($n, q{SET kingres_shard.enable_routing = off; SELECT (SELECT count(*) FROM conv.a) + (SELECT count(*) FROM conv.b) + (SELECT count(*) FROM conv.p)}), 0, $n->name . ': the shells are empty');
	is(q1($n, q{SELECT count(*) FROM pg_type t WHERE t.typnamespace = 'conv'::regnamespace AND t.typname IN ('mood', 'posint', 'pair')}), 3, $n->name . ': the types are there');
	is(q1($n, q{SELECT enum_range(NULL::conv.mood)}), '{sad,ok,happy}', $n->name . ': the enum has its labels');
	is(q1($n, q{SELECT relkind::text || ':' || relispartition::text FROM pg_class WHERE oid = 'conv.p2'::regclass}), 'r:true', $n->name . ': a partition');
	is(q1($n, q{SELECT pg_get_expr(relpartbound, oid) FROM pg_class WHERE oid = 'conv.p1'::regclass}), "FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')", $n->name . ': the bounds of a partition');
	is(q1($n, q{SELECT pg_get_viewdef('conv.v2'::regclass, true) = pg_get_viewdef('conv.v2'::regclass, true)}), 't', $n->name . ': the view');
	is(q1($n, q{SELECT count(*) FROM pg_trigger WHERE tgrelid = 'conv.b'::regclass AND NOT tgisinternal}), 1, $n->name . ': the trigger');
	is(q1($n, q{SELECT relowner::regrole FROM pg_class WHERE oid = 'conv.b'::regclass}), 'cvowner', $n->name . ': the owner of a table');
	is(q1($n, q{SELECT has_table_privilege('cvreader', 'conv.a', 'SELECT') || ':' || has_table_privilege('cvreader', 'conv.b', 'SELECT')}), 'true:false', $n->name . ': the privileges');
	is(q1($n, q{SELECT has_function_privilege('cvreader', 'conv.twice(int)', 'EXECUTE')}), 't', $n->name . ': the privilege on a function');
	is(q1($n, q{SELECT obj_description('conv.a'::regclass) || '|' || col_description('conv.a'::regclass, 2) || '|' || obj_description('conv.v1'::regclass) || '|' || obj_description('conv'::regnamespace)}),
	   'the a table|the v column|the first view|a schema with data', $n->name . ': the comments');
	is(q1($n, q{SELECT count(*) FROM pg_sequences WHERE schemaname = 'conv' AND sequencename = 'sq' AND increment_by = 5 AND start_value = 500}), 1, $n->name . ': the sequence with its options');
}
is(prepared_count($_), 0, $_->name . ': no prepared transaction is left') for @nodes;

# ---------------------------------------------------------------------------
# B. the schema works from every node
# ---------------------------------------------------------------------------
same_everywhere(q{SELECT count(*) FROM conv.a}, 120, 'a read from every node goes to the owner');
is(totals($_), $want, $_->name . ': a join of the schema and a view, the same answer') for @nodes;
q1($n3, q{INSERT INTO conv.b (a_id, note) VALUES (1, 'from n3')});
is(q1($n2, q{SELECT note FROM conv.b WHERE a_id = 1 AND note LIKE 'from%'}), 'from n3!', 'a write from another node runs on the owner (the trigger fired)');
is(q1($n1, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM conv.b WHERE note = 'from n3!'}), 1, '... and the row is on the owner');
is(q1($n3, q{SELECT nextval('conv.sq')}), 510, 'a sequence goes on from its value, from any node');
fails($n2, q{INSERT INTO conv.a (v, n) VALUES ('z', -1)}, qr/posint|violates check constraint/, 'the domain check holds');
q1($n1, q{ALTER TYPE conv.pair OWNER TO cvowner});
same_everywhere(q{SELECT typowner::regrole FROM pg_type WHERE oid = 'conv.pair'::regtype}, 'cvowner', 'ALTER TYPE ... OWNER TO reaches every node');
q1($n1, q{SELECT kingres.alter_distributed_schema('conv', true)});
is(q1($n3, q{SELECT conv.twice(21)}), 42, 'a function of the schema (ship_functions)');
is(q1($n2, q{SELECT count(*) FROM conv.rows_of_a()}), 120, 'a function that returns the rows of a table');
q1($n1, q{ALTER TABLE conv.a ADD COLUMN extra int DEFAULT 7});
same_everywhere(q{SELECT count(*) FROM pg_attribute WHERE attrelid = 'conv.a'::regclass AND attname = 'extra'}, 1, 'DDL after the conversion reaches every node');
q1($n1, q{ALTER TABLE conv.a DROP COLUMN extra});

# ---------------------------------------------------------------------------
# C. a schema with rows and another group; a schema without rows and another group
# ---------------------------------------------------------------------------
q1($n1, q{CREATE SCHEMA moved; CREATE TABLE moved.t (id int PRIMARY KEY, v text); INSERT INTO moved.t VALUES (1, 'one')});
fails($n1, q{SELECT kingres.distribute_schema('moved', 1)}, qr/holds rows on shard group 0, not on group 1/, 'a schema with rows cannot be placed on another group at once');
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema('moved', 1)});
	like($err, qr/Distribute the schema in place .*then kingres\.move_schema\(\) it to group 1/, '... and the hint says what to do') or diag($err);
}
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'moved'::regnamespace}), 0, '... and nothing was changed');
q1($n1, q{DROP SCHEMA moved CASCADE});
# the rows decide the owner, not the placement setting
q1($n1, q{CREATE SCHEMA pinned; CREATE TABLE pinned.t (id int PRIMARY KEY); INSERT INTO pinned.t VALUES (1)});
q1($n1, q{SET kingres_shard.schema_placement_group = 2; SELECT kingres.distribute_schema('pinned')});
same_everywhere(q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'pinned'}, 0, 'a schema with rows stays on the group that holds them, whatever schema_placement_group says');
q1($n1, q{SELECT kingres.undistribute_schema('pinned')});
q1($n1, q{DROP SCHEMA pinned CASCADE});
q1($n1, q{CREATE SCHEMA hollow; CREATE TABLE hollow.t (id int PRIMARY KEY, v text); CREATE SEQUENCE hollow.s});
q1($n1, q{SELECT kingres.distribute_schema('hollow', 1)});
same_everywhere(q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'hollow'}, 1, 'a schema without rows goes to the group that was asked for');
q1($n3, q{INSERT INTO hollow.t VALUES (1, 'one'), (2, 'two')});
is(q1($n2, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM hollow.t}), 2, 'the rows are on the owner, node 2');
is(q1($n1, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM hollow.t}), 0, '... and the table of node 1 is a shell');
same_everywhere(q{SELECT count(*) FROM hollow.t}, 2, '... and every node reads them');

# ---------------------------------------------------------------------------
# D. what is refused
# ---------------------------------------------------------------------------
q1($n1, q{CREATE SCHEMA bad1; CREATE TABLE bad1.t (id int); CREATE MATERIALIZED VIEW bad1.mv AS SELECT 1 AS x});
fails($n1, q{SELECT kingres.distribute_schema('bad1')}, qr/cannot be distributed: materialized view bad1\.mv/, 'a materialized view');
q1($n1, q{DROP SCHEMA bad1 CASCADE; CREATE SCHEMA bad2; CREATE TABLE bad2.pa (id int); CREATE TABLE bad2.ch (x int) INHERITS (bad2.pa)});
fails($n1, q{SELECT kingres.distribute_schema('bad2')}, qr/table inheritance of ch is not supported/, 'table inheritance');
q1($n1, q{DROP SCHEMA bad2 CASCADE; CREATE SCHEMA bad3; CREATE FUNCTION bad3.f(int, int) RETURNS int LANGUAGE sql AS 'SELECT $1 + $2'; CREATE OPERATOR bad3.<+> (LEFTARG = int, RIGHTARG = int, FUNCTION = bad3.f)});
fails($n1, q{SELECT kingres.distribute_schema('bad3')}, qr/operator <\+> is not supported/, 'an operator');
q1($n1, q{DROP SCHEMA bad3 CASCADE; CREATE SCHEMA bad4; CREATE TABLE bad4.t (id int PRIMARY KEY); CREATE UNLOGGED TABLE bad4.u (id int)});
fails($n1, q{SELECT kingres.distribute_schema('bad4')}, qr/cannot be distributed: table bad4\.u: an unlogged table/, 'an unlogged table');
q1($n1, q{DROP SCHEMA bad4 CASCADE});
# a node that cannot take the structure: a role that the other nodes lack
q1($n1, q{CREATE ROLE onlyhere});
q1($n2, q{CREATE ROLE onlyhere});
q1($n1, q{CREATE SCHEMA late; CREATE TABLE late.t (id int PRIMARY KEY); CREATE TABLE late.u (id int PRIMARY KEY); ALTER TABLE late.u OWNER TO onlyhere; INSERT INTO late.t VALUES (1)});
my $schemas_before = q1($n1, q{SELECT count(*) FROM kingres.dist_schema});
fails($n1, q{SELECT kingres.distribute_schema('late')}, qr/onlyhere/, 'a node that lacks the role of a table owner');
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema}), $schemas_before, '... the catalog of this node is unchanged');
is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid = 'late.t'::regclass}), 0, '... and its tables are not registered');
is(q1($n2, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'late'}), 0, '... the node that went first has no schema (the branch was rolled back)');
is(q1($n3, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'late'}), 0, '... and the node that failed has none');
is(q1($n1, q{SELECT count(*) FROM late.t}), 1, '... and the rows are untouched');
is(prepared_count($_), 0, $_->name . ': no prepared transaction is left') for @nodes;
q1($n3, q{CREATE ROLE onlyhere});
q1($n1, q{SELECT kingres.distribute_schema('late')});
same_everywhere(q{SELECT relowner::regrole FROM pg_class WHERE oid = 'late.u'::regclass}, 'onlyhere', 'with the role on every node the conversion works');

# the structure that the conversion makes must be the structure of the table: a partition with an index of its own is not carried
q1($n1, q{CREATE SCHEMA fpm; CREATE TABLE fpm.p (id int, v text) PARTITION BY RANGE (id); CREATE TABLE fpm.p1 PARTITION OF fpm.p FOR VALUES FROM (0) TO (100);
          CREATE INDEX px ON fpm.p1 (v); INSERT INTO fpm.p VALUES (1, 'a')});
fails($n1, q{SELECT kingres.distribute_schema('fpm')}, qr/table fpm\.p1 has another structure on node \d/, 'a structure that the conversion does not carry is refused after the check');
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'fpm'::regnamespace}), 0, '... the schema is not distributed');
is(q1($_, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'fpm'}), $_ == $n1 ? 1 : 0, $_->name . ': ... and the other nodes have no schema (the whole conversion is one transaction)') for @nodes;
is(prepared_count($_), 0, $_->name . ': no prepared transaction is left') for @nodes;
q1($n1, q{DROP INDEX fpm.px});
q1($n1, q{SELECT kingres.distribute_schema('fpm')});
same_everywhere(q{SELECT count(*) FROM kingres.dist_table WHERE relid::text LIKE 'fpm.%'}, 2, 'without the index of the partition the conversion works');
q1($n1, q{SELECT kingres.undistribute_schema('fpm')});
q1($n1, q{DROP SCHEMA fpm CASCADE});

# a writer that holds a table of the schema: the conversion waits for it (the tables are locked against writes until it commits)
q1($n1, q{CREATE SCHEMA lk; CREATE TABLE lk.t (id int PRIMARY KEY, v text); INSERT INTO lk.t VALUES (1, 'one')});
{
	my $writer = start_psql($n1, q{BEGIN; INSERT INTO lk.t VALUES (2, 'two'); SELECT pg_sleep(3); COMMIT});
	select(undef, undef, undef, 0.7);
	my $t0 = time();
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema('lk')});
	my $took = time() - $t0;
	finish_psql($writer);
	is($rc, 0, 'the conversion of a schema that a writer holds goes through') or diag($err);
	cmp_ok($took, '>=', 1, '... after the writer committed (it waited for the lock)');
	is(q1($n1, q{SELECT count(*) FROM lk.t}), 2, '... and the row of the writer is in the schema');
}
q1($n1, q{SELECT kingres.undistribute_schema('lk')});
q1($n1, q{DROP SCHEMA lk CASCADE});

# ---------------------------------------------------------------------------
# E. undistribute_schema
# ---------------------------------------------------------------------------
fails($n1, q{SELECT kingres.undistribute_schema('public')}, qr/is not a distributed schema/, 'a schema that is not distributed');
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.undistribute_schema('hollow')}, extra_params => [ '-U', 'cvreader' ]);
	isnt($rc, 0, 'an ordinary role is refused');
	like($err, qr/permission denied|superuser/, '... for lack of privilege') or diag($err);
}
fails($n2, q{SELECT kingres.undistribute_schema('conv')}, qr/map group/, 'a node that is not the map group primary');
{
	my $c = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'conv'::regnamespace});
	my $g = q1($n1, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'conv'});
	q1($n1, "SELECT kingres.internal_set_schema_group($c, $g, 'moving')");
	fails($n1, q{SELECT kingres.undistribute_schema('conv')}, qr/is being moved/, 'a schema whose bucket is moving');
	q1($n1, "SELECT kingres.internal_set_schema_group($c, $g, 'active')");
	q1($n1, "INSERT INTO kingres.bucket_move (colocation_id, bucket, source_group, target_group, phase, started_at, updated_at) VALUES ($c, 0, $g, $g + 1, 'copying', now(), now())");
	fails($n1, q{SELECT kingres.undistribute_schema('conv')}, qr/is being moved/, 'a schema with a move that has not ended');
	q1($n1, "DELETE FROM kingres.bucket_move WHERE colocation_id = $c");
}
q1($n3, q{SET kingres_shard.enable_routing = off; INSERT INTO conv.p VALUES (999, '2024-02-02')});
fails($n1, q{SELECT kingres.undistribute_schema('conv')}, qr/table conv\.p\d? of schema "conv" holds rows on node 3/, 'a row in a shell');
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'conv'::regnamespace}), 1, '... the schema is still distributed');
q1($n3, q{SET kingres_shard.enable_routing = off; DELETE FROM conv.p WHERE id = 999});
q1($n1, q{SELECT kingres.undistribute_schema('conv')});
same_everywhere(q{SELECT count(*) FROM kingres.dist_schema WHERE nsp::text = 'conv'}, 0, 'the schema is not distributed on any node');
same_everywhere(q{SELECT count(*) FROM kingres.dist_table WHERE relid::text LIKE 'conv.%'}, 0, '... no table is registered');
is(q1($n2, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'conv'}), 0, 'node 2 dropped the schema');
is(q1($n3, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'conv'}), 0, 'node 3 dropped the schema');
is(totals($n1), q1($n1, q{SELECT (SELECT count(*) || ':' || sum(id) FROM conv.a) || '|' || (SELECT count(*) || ':' || sum(a_id) FROM conv.b)
                          || '|' || (SELECT count(*) || ':' || sum(id) FROM conv.p) || '|' || (SELECT count(*) FROM conv.v2)}), 'the owner has the schema as a local one');
like(totals($n1), qr/^120:7260\|61:1831\|100:5050\|121$/, '... with every row (and the row that was written from node 3)');
is(q1($n1, q{SELECT count(*) FROM kingres.colocation c WHERE c.kind = 'schema' AND NOT EXISTS (SELECT 1 FROM kingres.dist_schema s WHERE s.colocation_id = c.colocation_id)}), 0, 'no colocation group is left without a schema');
is(q1($n1, q{SELECT count(*) FROM kingres.bucket_map m WHERE NOT EXISTS (SELECT 1 FROM kingres.colocation c WHERE c.colocation_id = m.colocation_id)}), 0, '... nor a bucket');
is(q1($n1, q{SELECT count(*) FROM pg_trigger WHERE tgrelid = 'conv.b'::regclass AND NOT tgisinternal}), 1, 'the trigger of the owner is still there');
# a local schema again: a local DDL is not propagated
q1($n1, q{CREATE TABLE conv.later (id int)});
is(q1($n2, q{SELECT to_regclass('conv.later') IS NULL}), 't', 'a table made in the local schema stays local');
q1($n1, q{DROP TABLE conv.later});

# from a node that is not the owner: the owner is node 2 (the group of 'hollow'), the map group primary is node 1
q1($n1, q{INSERT INTO hollow.t VALUES (3, 'three')});
q1($n1, q{SELECT kingres.undistribute_schema('hollow')});
is(q1($n2, q{SELECT count(*) FROM hollow.t}), 3, 'the owner keeps the rows');
is(q1($n1, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'hollow'}), 0, 'the map group primary, a shell holder, dropped the schema');
is(q1($n3, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'hollow'}), 0, 'so did node 3');
same_everywhere(q{SELECT count(*) FROM kingres.dist_schema WHERE nsp::text IN ('hollow', 'conv')}, 0, 'the catalog is empty of both');

# ---------------------------------------------------------------------------
# F. the same schema again; the cluster is quiet
# ---------------------------------------------------------------------------
q1($n1, q{SELECT kingres.distribute_schema('conv')});
same_everywhere(q{SELECT group_id || ':' || table_count FROM kingres.schemas WHERE schema_name = 'conv'}, '0:5', 'a schema that was undistributed is distributed again');
is(q1($n3, q{SELECT count(*) FROM conv.a}), 120, '... and its rows are read from another node');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster finds nothing') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));
is(prepared_count($_), 0, $_->name . ': no prepared transaction is left at the end') for @nodes;

done_testing();
