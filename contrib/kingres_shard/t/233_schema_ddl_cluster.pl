# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/233_schema_ddl_cluster.pl
#
# Phase 11 (design/shard-phase11.md), increment 3: DDL on a distributed schema, on three nodes (7.1 to 7.8, 7.12).
# A statement whose object is in a distributed schema is run on every node in one two-phase transaction, the tables it
# makes are registered everywhere, and the structure is the same everywhere (the fingerprint).  The schemas are made by
# CREATE SCHEMA in a database that distributes new schemas by default.
#
#   A. CREATE SCHEMA distributes; the groups; the rule of the map group primary
#   B. tables, keys, defaults, sequences: created on every node and registered on every node
#   C. ALTER TABLE, indexes, views, sequences, functions, types, comments, grants, triggers, partitions, TRUNCATE
#   D. renames and the owner of a schema
#   E. what is refused in a distributed schema (7.12 and 7.8)
#   F. DROP TABLE and DROP SCHEMA leave nothing on any node
#   G. an ordinary role: the creation of a schema and of tables
#   H. atomicity: a node that cannot run the statement, and a structure that differs

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('dd', 3);
my ($n1, $n2, $n3) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

# the same answer on every node
sub same_everywhere
{
	my ($sql, $expected, $name) = @_;
	for my $n (@nodes)
	{
		is(q1($n, $sql), $expected, $n->name . ": $name");
	}
}

sub fails
{
	my ($n, $sql, $re, $name, %opt) = @_;
	my ($rc, $out, $err) = run_sql($n, $sql, %opt);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "... message: $name") or diag($err);
}

for my $n (@nodes)
{
	q1($n, q{CREATE ROLE alice LOGIN; GRANT CREATE ON DATABASE postgres TO alice});
	q1($n, q{CREATE TABLE public.loc (id int PRIMARY KEY)});
}
q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});
q1($n1, q{CREATE TABLE public.ref (id int PRIMARY KEY, name text); SELECT kingres.create_reference_table('public.ref')});

# ---------------------------------------------------------------------------
# A. CREATE SCHEMA
# ---------------------------------------------------------------------------
q1($n1, q{CREATE SCHEMA t1});
q1($n1, q{CREATE SCHEMA t2});
q1($n1, q{CREATE SCHEMA t3});
same_everywhere(q{SELECT string_agg(schema_name || ':' || group_id, ',' ORDER BY schema_name) FROM kingres.schemas}, 't1:0,t2:1,t3:2',
	'CREATE SCHEMA distributes a new schema, on the group with the fewest schemas');
same_everywhere(q{SELECT count(*) FROM pg_namespace WHERE nspname IN ('t1', 't2', 't3')}, 3, 'the schemas exist on every node');
fails($n2, q{CREATE SCHEMA t9}, qr/must run on the map group primary/, 'CREATE SCHEMA on a node that is not the map group primary');
is(q1($n2, q{SELECT count(*) FROM pg_namespace WHERE nspname = 't9'}), 0, '... and nothing was created there');
fails($n1, q{CREATE SCHEMA t9 CREATE TABLE t9.x (id int)}, qr/CREATE SCHEMA with elements is not supported/, 'CREATE SCHEMA with elements');
q1($n1, q{SET kingres_shard.distribute_new_schemas = off; CREATE SCHEMA local1});
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'local1'::regnamespace}), 0, 'a superuser can create a local schema with the session setting');
is(q1($n2, q{SELECT count(*) FROM pg_namespace WHERE nspname = 'local1'}), 0, '... which exists on this node only');
q1($n1, q{DROP SCHEMA local1});
q1($n1, q{CREATE SCHEMA IF NOT EXISTS t1});
same_everywhere(q{SELECT count(*) FROM kingres.dist_schema}, 3, 'CREATE SCHEMA IF NOT EXISTS of a schema that exists changes nothing');

# ---------------------------------------------------------------------------
# B. tables
# ---------------------------------------------------------------------------
q1($n1, q{
	CREATE TABLE t1.a (id serial PRIMARY KEY, v text NOT NULL DEFAULT 'x', n int CHECK (n > 0));
	CREATE TABLE t1.b (id int PRIMARY KEY, a_id int REFERENCES t1.a (id), note text);
	CREATE TABLE t1.r (rid int REFERENCES public.ref (id));
});
same_everywhere(q{SELECT string_agg(to_regclass(x)::text, ',' ORDER BY x) FROM unnest(ARRAY['t1.a', 't1.b', 't1.r']) x}, 't1.a,t1.b,t1.r', 'the tables exist');
same_everywhere(q{SELECT string_agg(relid::text || ':' || method, ',' ORDER BY relid::text) FROM kingres.dist_table WHERE relid::text LIKE 't1.%'},
	't1.a:schema,t1.b:schema,t1.r:schema', 'the tables are registered as schema tables');
same_everywhere(q{SELECT table_count FROM kingres.schemas WHERE schema_name = 't1'}, 3, 'kingres.schemas counts them');
same_everywhere(q{SELECT count(*) FROM pg_class WHERE relname = 'a_id_seq' AND relnamespace = 't1'::regnamespace}, 1, 'the sequence of the serial column exists everywhere');
{
	my %fp = map { $_->name => q1($_, q{SELECT kingres.internal_table_fingerprint('t1.a')}) } @nodes;
	is(scalar(keys %{{ map { $_ => 1 } values %fp }}), 1, 'the structure of the table is the same on every node') or diag(join(', ', map { "$_=$fp{$_}" } sort keys %fp));
}
fails($n1, q{CREATE TABLE t1.c (x int REFERENCES public.loc (id))}, qr/foreign key between a table of a distributed schema and a table outside the schema/, 'a foreign key to a local table');
fails($n1, q{CREATE TABLE public.d (x int REFERENCES t1.a (id))}, qr/foreign key between a table of a distributed schema and a table outside the schema/, 'a foreign key from a local table to a schema table');
fails($n1, q{ALTER TABLE public.loc ADD FOREIGN KEY (id) REFERENCES t1.a (id)}, qr/foreign key between a table of a distributed schema and a table outside the schema/, 'ALTER TABLE: a foreign key from a local table to a schema table');
fails($n1, q{CREATE TABLE t1.c2 (x int REFERENCES t2.nothing (id))}, qr/does not exist/, 'a foreign key to a table that does not exist (the error of PostgreSQL)');
q1($n1, q{CREATE TABLE t2.k (id int PRIMARY KEY)});
fails($n1, q{CREATE TABLE t1.c3 (x int REFERENCES t2.k (id))}, qr/foreign key between a table of a distributed schema and a table outside the schema/, 'a foreign key to a table of another distributed schema');
fails($n2, q{CREATE TABLE t1.q (id int)}, qr/must run on the map group primary/, 'CREATE TABLE on a node that is not the map group primary');
same_everywhere(q{SELECT count(*) FROM pg_class WHERE relname IN ('c', 'c2', 'c3', 'q', 'd') AND relnamespace IN ('t1'::regnamespace, 'public'::regnamespace)}, 0, 'the refused tables exist nowhere');

# rows live on the owner (group 0 = node 1); every node reads them through the scan (increment 2)
q1($n1, q{INSERT INTO t1.a (v, n) VALUES ('p', 1), ('q', 2), ('r', 3)});
q1($n1, q{INSERT INTO t1.b VALUES (1, 1, 'one'), (2, 2, 'two')});
same_everywhere(q{SELECT count(*) FROM t1.a}, 3, 'the rows of a new table are read from the owner everywhere');
same_everywhere(q{SELECT count(*) FROM t1.a a JOIN t1.b b ON b.a_id = a.id}, 2, 'a join of two tables of the schema');

# ---------------------------------------------------------------------------
# C. the family of statements
# ---------------------------------------------------------------------------
q1($n1, q{ALTER TABLE t1.a ADD COLUMN w int DEFAULT 5});
q1($n1, q{ALTER TABLE t1.a ADD CONSTRAINT a_w_ck CHECK (w < 100)});
q1($n1, q{ALTER TABLE t1.a ALTER COLUMN n SET DEFAULT 7});
q1($n1, q{ALTER TABLE t1.b RENAME COLUMN note TO note2});
q1($n1, q{ALTER TABLE t1.a ALTER COLUMN w TYPE bigint});
q1($n1, q{ALTER TABLE t1.b DROP COLUMN note2});
for my $t ('t1.a', 't1.b')
{
	my %fp = map { $_->name => q1($_, "SELECT kingres.internal_table_fingerprint('$t')") } @nodes;
	is(scalar(keys %{{ map { $_ => 1 } values %fp }}), 1, "ALTER TABLE: the structure of $t is the same on every node") or diag(join(', ', map { "$_=$fp{$_}" } sort keys %fp));
}
same_everywhere(q{SELECT string_agg(attname || ':' || format_type(atttypid, atttypmod), ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid = 't1.a'::regclass AND attnum > 0 AND NOT attisdropped},
	'id:integer,v:text,n:integer,w:bigint', 'the columns after the ALTER TABLE statements');
same_everywhere(q{SELECT count(*) FROM pg_constraint WHERE conrelid = 't1.a'::regclass AND conname = 'a_w_ck'}, 1, 'the new constraint is there');
is(q1($n1, q{SELECT w FROM t1.a WHERE id = 1}), 5, 'the default of the new column was applied to the rows of the owner');

q1($n1, q{CREATE INDEX a_v_idx ON t1.a (v)});
q1($n1, q{CREATE UNIQUE INDEX a_n_uidx ON t1.a (n) WHERE n IS NOT NULL});
same_everywhere(q{SELECT string_agg(indexrelid::regclass::text, ',' ORDER BY indexrelid::regclass::text) FROM pg_index WHERE indrelid = 't1.a'::regclass}, 't1.a_n_uidx,t1.a_pkey,t1.a_v_idx', 'indexes exist on every node');
fails($n1, q{CREATE INDEX CONCURRENTLY a_w_idx ON t1.a (w)}, qr/CREATE INDEX CONCURRENTLY is not supported in a distributed schema yet/, 'CREATE INDEX CONCURRENTLY');
q1($n1, q{DROP INDEX t1.a_v_idx});
same_everywhere(q{SELECT count(*) FROM pg_class WHERE relname = 'a_v_idx'}, 0, 'DROP INDEX is run on every node');

q1($n1, q{CREATE VIEW t1.va AS SELECT id, v FROM t1.a});
same_everywhere(q{SELECT count(*) FROM t1.va}, 3, 'a view of the schema reads the rows of the owner from every node');
q1($n1, q{CREATE OR REPLACE VIEW t1.va AS SELECT id, v, n FROM t1.a});
same_everywhere(q{SELECT count(*) FROM pg_attribute WHERE attrelid = 't1.va'::regclass AND attnum > 0}, 3, 'CREATE OR REPLACE VIEW is run on every node');

q1($n1, q{CREATE SEQUENCE t1.sq START 10; ALTER SEQUENCE t1.sq INCREMENT BY 5});
same_everywhere(q{SELECT increment_by FROM pg_sequences WHERE schemaname = 't1' AND sequencename = 'sq'}, 5, 'a sequence and its ALTER SEQUENCE');

q1($n1, q{CREATE FUNCTION t1.f(int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 1'});
same_everywhere(q{SELECT t1.f(1)}, 2, 'a function of the schema is callable on every node');
q1($n1, q{CREATE OR REPLACE FUNCTION t1.f(int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 2'});
same_everywhere(q{SELECT t1.f(1)}, 3, 'CREATE OR REPLACE FUNCTION is run on every node');

q1($n1, q{CREATE TYPE t1.mood AS ENUM ('sad', 'ok'); ALTER TYPE t1.mood ADD VALUE 'happy'; CREATE DOMAIN t1.dpos AS int CHECK (VALUE > 0); CREATE TYPE t1.pair AS (x int, y int)});
same_everywhere(q{SELECT enum_range(null::t1.mood)::text}, '{sad,ok,happy}', 'an enum type and ALTER TYPE ... ADD VALUE');
same_everywhere(q{SELECT count(*) FROM pg_type WHERE typnamespace = 't1'::regnamespace AND typname IN ('dpos', 'pair')}, 2, 'a domain and a composite type');

q1($n1, q{COMMENT ON TABLE t1.a IS 'the a table'; COMMENT ON COLUMN t1.a.v IS 'the v column'});
same_everywhere(q{SELECT obj_description('t1.a'::regclass) || '/' || col_description('t1.a'::regclass, 2)}, 'the a table/the v column', 'COMMENT');

q1($n1, q{GRANT USAGE ON SCHEMA t1 TO alice; GRANT SELECT ON t1.a TO alice});
same_everywhere(q{SELECT has_schema_privilege('alice', 't1', 'USAGE') AND has_table_privilege('alice', 't1.a', 'SELECT') AND NOT has_table_privilege('alice', 't1.b', 'SELECT')}, 't', 'GRANT on the schema and on a table');
q1($n1, q{GRANT SELECT ON ALL TABLES IN SCHEMA t1 TO alice});
same_everywhere(q{SELECT has_table_privilege('alice', 't1.b', 'SELECT')}, 't', 'GRANT ... ON ALL TABLES IN SCHEMA');
q1($n1, q{REVOKE SELECT ON t1.a FROM alice});
same_everywhere(q{SELECT has_table_privilege('alice', 't1.a', 'SELECT')}, 'f', 'REVOKE');
{
	my ($rc, $out, $err) = run_sql($n2, q{SELECT count(*) FROM t1.b}, extra_params => [ '-U', 'alice' ]);
	is($out, '2', 'an ordinary role with the privilege reads the table of the schema from a node that does not hold it') or diag($err);
	($rc, $out, $err) = run_sql($n2, q{SELECT count(*) FROM t1.a}, extra_params => [ '-U', 'alice' ]);
	isnt($rc, 0, 'a role without the privilege is refused (at the origin, before the scan)');
}

q1($n1, q{
	CREATE TABLE t1.log (id serial PRIMARY KEY, what text);
	CREATE FUNCTION t1.tf() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO t1.log (what) VALUES ('inserted ' || NEW.id); RETURN NEW; END $$;
	CREATE TRIGGER a_log AFTER INSERT ON t1.a FOR EACH ROW EXECUTE FUNCTION t1.tf();
});
same_everywhere(q{SELECT count(*) FROM pg_trigger WHERE tgrelid = 't1.a'::regclass AND tgname = 'a_log'}, 1, 'a trigger exists on every node');
q1($n1, q{INSERT INTO t1.a (v, n) VALUES ('t', 4)});
is(q1($n2, q{SELECT what FROM t1.log}), 'inserted 4', 'the trigger fired on the owner');

q1($n1, q{
	CREATE TABLE t1.p (id int, d date) PARTITION BY RANGE (d);
	CREATE TABLE t1.p1 PARTITION OF t1.p FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
	CREATE TABLE t1.p2 (id int, d date);
	ALTER TABLE t1.p ATTACH PARTITION t1.p2 FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
});
same_everywhere(q{SELECT string_agg(relid::text, ',' ORDER BY relid::text) FROM kingres.dist_table WHERE relid::text LIKE 't1.p%'}, 't1.p,t1.p1,t1.p2', 'a partitioned table and its partitions are all registered');
q1($n1, q{INSERT INTO t1.p VALUES (1, '2024-05-05'), (2, '2025-05-05')});
same_everywhere(q{SELECT count(*) FROM t1.p}, 2, 'rows of a partitioned table of the schema, read from every node');
fails($n1, q{CREATE TABLE t2.px PARTITION OF t1.p FOR VALUES FROM ('2030-01-01') TO ('2031-01-01')}, qr/cannot inherit from, or be a partition of, a table of another schema/, 'a partition in another schema');

# TRUNCATE is run on every node, the shells too
q1($n2, q{SET kingres_shard.enable_routing = off; INSERT INTO t1.b VALUES (900, NULL)});
q1($n1, q{TRUNCATE t1.b});
same_everywhere(q{SELECT count(*) FROM pg_class c WHERE c.oid = 't1.b'::regclass}, 1, 'TRUNCATE leaves the table');
is(q1($n2, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM t1.b}), 0, 'TRUNCATE emptied the stray row of the shell too');
is(q1($n1, q{SELECT count(*) FROM t1.b}), 0, '... and the rows of the owner');

# ---------------------------------------------------------------------------
# D. renames and owners
# ---------------------------------------------------------------------------
q1($n1, q{ALTER TABLE t1.r RENAME TO r2});
same_everywhere(q{SELECT to_regclass('t1.r2')::text}, 't1.r2', 'a renamed table, on every node');
q1($n1, q{ALTER TABLE t1.r2 OWNER TO alice});
same_everywhere(q{SELECT relowner::regrole::text FROM pg_class WHERE oid = 't1.r2'::regclass}, 'alice', 'OWNER TO is run on every node');
q1($n1, q{ALTER SCHEMA t3 RENAME TO t3x});
same_everywhere(q{SELECT string_agg(schema_name, ',' ORDER BY schema_name) FROM kingres.schemas}, 't1,t2,t3x', 'a renamed distributed schema keeps its row (the catalog follows the OID)');
q1($n1, q{ALTER SCHEMA t3x RENAME TO t3});
q1($n1, q{ALTER SCHEMA t2 OWNER TO alice});
same_everywhere(q{SELECT nspowner::regrole::text FROM pg_namespace WHERE nspname = 't2'}, 'alice', 'ALTER SCHEMA ... OWNER TO is run on every node');

# ---------------------------------------------------------------------------
# E. what is refused
# ---------------------------------------------------------------------------
q1($n1, q{CREATE SEQUENCE public.lsq});
for my $case (
	[ 'a materialized view', q{CREATE MATERIALIZED VIEW t1.mv AS SELECT 1 AS x}, qr/CREATE MATERIALIZED VIEW is not supported in a distributed schema yet/ ],
	[ 'CREATE TABLE AS', q{CREATE TABLE t1.ctas AS SELECT 1 AS x}, qr/CREATE TABLE AS or SELECT INTO is not supported in a distributed schema yet/ ],
	[ 'SELECT INTO', q{SELECT 1 AS x INTO t1.si}, qr/CREATE TABLE AS or SELECT INTO is not supported in a distributed schema yet/ ],
	[ 'a rule', q{CREATE RULE r1 AS ON INSERT TO t1.b DO ALSO NOTHING}, qr/CREATE RULE is not supported in a distributed schema yet/ ],
	[ 'a policy', q{CREATE POLICY p1 ON t1.b USING (true)}, qr/CREATE POLICY is not supported in a distributed schema yet/ ],
	[ 'row-level security', q{ALTER TABLE t1.b ENABLE ROW LEVEL SECURITY}, qr/row-level security is not supported in a distributed schema yet/ ],
	[ 'extended statistics', q{CREATE STATISTICS st1 ON id, a_id FROM t1.b}, qr/CREATE STATISTICS is not supported in a distributed schema yet/ ],
	[ 'a range type', q{CREATE TYPE t1.rng AS RANGE (subtype = int)}, qr/CREATE TYPE \.\.\. AS RANGE is not supported in a distributed schema yet/ ],
	[ 'an aggregate', q{CREATE AGGREGATE t1.agg(int) (sfunc = int4pl, stype = int)}, qr/not supported in a distributed schema yet/ ],
	[ 'SET SCHEMA out (a view)', q{ALTER VIEW t1.va SET SCHEMA public}, qr/SET SCHEMA into or out of a distributed schema is not supported yet/ ],
	[ 'SET SCHEMA in (a sequence)', q{ALTER SEQUENCE public.lsq SET SCHEMA t1}, qr/SET SCHEMA into or out of a distributed schema is not supported yet/ ],
	[ 'TRUNCATE CASCADE', q{TRUNCATE t1.a CASCADE}, qr/TRUNCATE \.\.\. CASCADE is not supported in a distributed schema yet/ ],
	[ 'a statement that names a distributed schema and another', q{DROP TABLE t1.b, public.loc}, qr/names a distributed schema and another schema/ ],
  )
{
	fails($n1, $case->[1], $case->[2], $case->[0]);
}
same_everywhere(q{SELECT count(*) FROM pg_class WHERE relname IN ('mv', 'ctas', 'si', 'rng', 'agg', 'st1') AND relnamespace = 't1'::regnamespace}, 0, 'the refused statements left nothing anywhere');
is(q1($n1, q{SELECT count(*) FROM public.loc}), 0, '... and the local table is where it was');
same_everywhere(q{SELECT to_regclass('t1.b') IS NOT NULL}, 't', 'a refused DROP of a mix dropped nothing');

# ---------------------------------------------------------------------------
# F. drops
# ---------------------------------------------------------------------------
q1($n1, q{DROP VIEW t1.va; DROP TABLE t1.b});
same_everywhere(q{SELECT to_regclass('t1.b') IS NULL AND to_regclass('t1.va') IS NULL}, 't', 'DROP TABLE and DROP VIEW are run on every node');
same_everywhere(q{SELECT string_agg(relid::text, ',' ORDER BY relid::text) FROM kingres.dist_table WHERE relid::text LIKE 't1.%'},
	't1.a,t1.log,t1.p,t1.p1,t1.p2,t1.r2', 'the registration of the dropped table is gone on every node');
q1($n1, q{CREATE TABLE t3.z (id int)});
same_everywhere(q{SELECT count(*) FROM kingres.dist_schema}, 3, 'three schemas');
fails($n1, q{DROP SCHEMA t3}, qr/cannot drop schema t3 because other objects depend on it/, 'DROP SCHEMA of a schema with a table, without CASCADE');
q1($n1, q{DROP SCHEMA t3 CASCADE});
same_everywhere(q{SELECT count(*) FROM pg_namespace WHERE nspname = 't3'}, 0, 'DROP SCHEMA ... CASCADE is run on every node');
same_everywhere(q{SELECT count(*) FROM kingres.dist_schema}, 2, '... the schema is no longer registered anywhere');
same_everywhere(q{SELECT count(*) FROM kingres.colocation WHERE kind = 'schema'}, 2, '... its colocation group is gone');
same_everywhere(q{SELECT count(*) FROM kingres.bucket_map b JOIN kingres.colocation c USING (colocation_id) WHERE c.kind = 'schema'}, 2, '... and its bucket');
same_everywhere(q{SELECT count(*) FROM kingres.dist_table WHERE relid::text LIKE 't3.%'}, 0, '... and its tables');
q1($n1, q{CREATE SCHEMA t4; DROP SCHEMA t4});
same_everywhere(q{SELECT count(*) FROM kingres.dist_schema}, 2, 'a distributed schema that is created and dropped leaves nothing');

# ---------------------------------------------------------------------------
# G. an ordinary role
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, q{CREATE SCHEMA ta}, extra_params => [ '-U', 'alice' ]);
	is($rc, 0, 'an ordinary role with CREATE on the database creates a schema') or diag($err);
	same_everywhere(q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'ta'::regnamespace}, 1, '... which is distributed, on every node');
	same_everywhere(q{SELECT nspowner::regrole::text FROM pg_namespace WHERE nspname = 'ta'}, 'alice', '... and owned by that role everywhere');
	($rc, $out, $err) = run_sql($n1, q{CREATE TABLE ta.x (id int PRIMARY KEY)}, extra_params => [ '-U', 'alice' ]);
	is($rc, 0, 'the role creates a table in its schema') or diag($err);
	same_everywhere(q{SELECT relid::text || ':' || method FROM kingres.dist_table WHERE relid::text = 'ta.x'}, 'ta.x:schema', '... which is registered on every node');
	($rc, $out, $err) = run_sql($n1, q{CREATE TABLE t1.mine (id int)}, extra_params => [ '-U', 'alice' ]);
	isnt($rc, 0, 'the role cannot create a table in a schema it has no right on');
	($rc, $out, $err) = run_sql($n1, q{DROP SCHEMA ta CASCADE}, extra_params => [ '-U', 'alice' ]);
	is($rc, 0, 'the role drops its schema') or diag($err);
	same_everywhere(q{SELECT count(*) FROM kingres.dist_schema WHERE nsp::text = 'ta'}, 0, '... and the registration is gone everywhere');
}

# ---------------------------------------------------------------------------
# H. atomicity
# ---------------------------------------------------------------------------
q1($n3, q{SET kingres_shard.enable_routing = off; CREATE TABLE t1.dup (id int)});
{
	my ($rc, $out, $err) = run_sql($n1, q{CREATE TABLE t1.dup (id int)});
	isnt($rc, 0, 'a node that already has the table fails the statement');
	like($err, qr/relation "dup" already exists/, '... with the error of that node') or diag($err);
	is(q1($n1, q{SELECT to_regclass('t1.dup') IS NULL}), 't', '... and the table is not here either');
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid::text = 't1.dup'}), 0, '... or registered');
	is(q1($n2, q{SELECT to_regclass('t1.dup') IS NULL}), 't', '... or on the node that could have made it');
}
q1($n3, q{SET kingres_shard.enable_routing = off; DROP TABLE t1.dup});
for my $n (@nodes)
{
	q1($n, q{CREATE DOMAIN public.thing AS int});
}
q1($n3, q{SET kingres_shard.enable_routing = off; CREATE SCHEMA xs; CREATE DOMAIN xs.thing AS int});
{
	my ($rc, $out, $err) = run_sql($n1, q{SET search_path = xs, public; CREATE TABLE t1.fp (a thing)});
	isnt($rc, 0, 'a table that resolves a type differently on a node is refused');
	like($err, qr/the structure of table t1\.fp differs on node 3/, '... by its fingerprint') or diag($err);
	is(q1($n1, q{SELECT to_regclass('t1.fp') IS NULL}), 't', '... and rolled back here');
	is(q1($n2, q{SELECT to_regclass('t1.fp') IS NULL}), 't', '... and on the others');
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid::text = 't1.fp'}), 0, '... with no registration left');
}
for my $n (@nodes) { is(prepared_count($n), 0, 'nothing is prepared on ' . $n->name); }
{
	my %ver = map { $_->name => q1($_, 'SELECT version FROM kingres.map_version') } @nodes;
	is(scalar(keys %{{ map { $_ => 1 } values %ver }}), 1, 'the map version is the same on every node') or diag(join(', ', map { "$_=$ver{$_}" } sort keys %ver));
}

done_testing();
