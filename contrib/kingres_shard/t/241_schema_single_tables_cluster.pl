# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/241_schema_single_tables_cluster.pl
#
# Phase 11 (design/shard-phase11.md 7.9 and 8.3, section 16 step 10): single tables in and out of a distributed schema with ALTER TABLE ... SET SCHEMA, on
# three nodes.  A table moves when no row has to move: into a schema whose owner is the group of the node that runs the statement (or when the table is
# empty), out of a schema (the owner keeps the table and its rows, the other nodes drop their empty shells), between two schemas of one owner.
#
#   A. into a distributed schema: an empty table into a schema of another group, a table with rows into a schema of this group
#   B. what is refused going in: rows that are on the wrong node, foreign keys, kinds of table, triggers, distributed tables
#   C. between two distributed schemas of one owner (the catalog follows), and of different owners (refused)
#   D. out of a distributed schema: from the map group primary that is a shell holder, from the one that is the owner, a shell that holds a row
#   E. the same everywhere: fingerprints, the catalog, check_cluster, no prepared transaction

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('st', 3);
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

q1($_, q{CREATE ROLE sto}) for @nodes;
q1($n1, q{SELECT kingres.set_sharding_mode('both')});
q1($n1, q{CREATE TABLE public.ref (id int PRIMARY KEY, name text); SELECT kingres.create_reference_table('public.ref'); INSERT INTO public.ref VALUES (1, 'r1'), (2, 'r2')});
q1($n1, q{CREATE SCHEMA si1; CREATE SCHEMA si2; CREATE SCHEMA sg0; CREATE SCHEMA sg2});
q1($n1, q{SELECT kingres.distribute_schema('si1', 1); SELECT kingres.distribute_schema('si2', 1); SELECT kingres.distribute_schema('sg0', 0); SELECT kingres.distribute_schema('sg2', 2)});

my $fp = q{SELECT string_agg(c.relname || ':' || kingres.internal_table_fingerprint(c.oid), ',' ORDER BY c.relname) FROM pg_class c WHERE c.relnamespace = '%s'::regnamespace AND c.relkind = 'r'};
my $reg = q{SELECT string_agg(d.relid::text || ':' || d.colocation_id, ',' ORDER BY d.relid::text) FROM kingres.dist_table d WHERE d.method = 'schema'};

# ---------------------------------------------------------------------------
# A. in
# ---------------------------------------------------------------------------
q1($n1, q{CREATE TABLE public.e1 (id serial PRIMARY KEY, v text NOT NULL DEFAULT 'x', r int REFERENCES public.ref (id), CONSTRAINT e1_len CHECK (length(v) < 20));
          CREATE INDEX e1_v ON public.e1 (v); ALTER TABLE public.e1 OWNER TO sto; COMMENT ON TABLE public.e1 IS 'empty one'});
my $fp_e1 = q1($n1, q{SELECT kingres.internal_table_fingerprint('public.e1'::regclass)});
q1($n1, q{ALTER TABLE public.e1 SET SCHEMA si1});
same_everywhere(q{SELECT to_regclass('si1.e1') IS NOT NULL AND to_regclass('public.e1') IS NULL}, 't', 'an empty table went into a schema of another group, on every node');
is(q1($_, q{SELECT kingres.internal_table_fingerprint('si1.e1'::regclass)}), q1($n1, q{SELECT kingres.internal_table_fingerprint('si1.e1'::regclass)}), $_->name . ': with the structure of node 1') for @nodes;
same_everywhere(q{SELECT relowner::regrole FROM pg_class WHERE oid = 'si1.e1'::regclass}, 'sto', 'and its owner');
same_everywhere(q{SELECT d.method || ':' || (SELECT group_id FROM kingres.bucket_map WHERE colocation_id = d.colocation_id AND bucket = 0) FROM kingres.dist_table d WHERE d.relid = 'si1.e1'::regclass}, 'schema:1', 'it is a table of the schema, held by group 1');
same_everywhere(q{SELECT obj_description('si1.e1'::regclass)}, 'empty one', 'with its comment');
q1($n3, q{INSERT INTO si1.e1 (v, r) VALUES ('a', 1), ('b', 2)});
is(q1($n2, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM si1.e1}), 2, 'a write from another node goes to the owner');
is(q1($n1, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM si1.e1}), 0, '... and the table here is a shell');
same_everywhere(q{SELECT count(*) FROM si1.e1}, 2, '... every node reads it');
is(q1($n3, q{SELECT string_agg(id::text, ',' ORDER BY id) FROM si1.e1}), '1,2', 'its serial column comes from the owner\'s sequence');
q1($n1, q{DELETE FROM si1.e1});

q1($n1, q{CREATE TABLE public.r0 (id int PRIMARY KEY, v text, g int GENERATED ALWAYS AS IDENTITY); INSERT INTO public.r0 (id, v) SELECT g, 'row' || g FROM generate_series(1, 50) g});
q1($n1, q{ALTER TABLE public.r0 SET SCHEMA sg0});
same_everywhere(q{SELECT count(*) FROM sg0.r0}, 50, 'a table with rows went into a schema of this group: the rows stayed, every node reads them');
is(q1($n2, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM sg0.r0}), 0, '... the other nodes hold shells');
q1($n3, q{INSERT INTO sg0.r0 (id, v) VALUES (100, 'from n3')});
is(q1($n1, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM sg0.r0}), 51, 'a write from another node reaches the owner (the identity column too)');

# ---------------------------------------------------------------------------
# B. refused going in
# ---------------------------------------------------------------------------
q1($n1, q{CREATE TABLE public.r1 (id int PRIMARY KEY); INSERT INTO public.r1 VALUES (1), (2)});
fails($n1, q{ALTER TABLE public.r1 SET SCHEMA si1}, qr/the table holds rows on shard group 0 and the schema is held by group 1/, 'rows on a node that is not the owner');
is(q1($n1, q{SELECT to_regclass('public.r1') IS NOT NULL AND to_regclass('si1.r1') IS NULL}), 't', '... the table is where it was');
same_everywhere(q{SELECT to_regclass('si1.r1') IS NULL}, 't', '... and nothing is on the other nodes');
q1($n1, q{CREATE TABLE public.loc (id int PRIMARY KEY); CREATE TABLE public.fk1 (id int PRIMARY KEY, l int REFERENCES public.loc (id))});
fails($n1, q{ALTER TABLE public.fk1 SET SCHEMA sg0}, qr/the foreign key "fk1_l_fkey" would cross the border/, 'a foreign key to a local table');
q1($n1, q{CREATE TABLE public.fk2 (id int PRIMARY KEY, r int REFERENCES public.ref (id))});
q1($n1, q{ALTER TABLE public.fk2 SET SCHEMA sg0});
same_everywhere(q{SELECT count(*) FROM pg_constraint WHERE conrelid = 'sg0.fk2'::regclass AND contype = 'f'}, 1, 'a foreign key to a reference table is carried');
fails($n1, q{ALTER TABLE public.loc SET SCHEMA sg0}, qr/the foreign key "fk1_l_fkey" would cross the border/, 'a table that another table points to');
q1($n1, q{DROP TABLE public.fk1});
q1($n1, q{CREATE TABLE public.pt (id int, d date) PARTITION BY RANGE (d)});
fails($n1, q{ALTER TABLE public.pt SET SCHEMA sg0}, qr/only an ordinary table/, 'a partitioned table');
q1($n1, q{CREATE TABLE public.tg (id int PRIMARY KEY); CREATE FUNCTION public.tgf() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'; CREATE TRIGGER tg1 BEFORE INSERT ON public.tg FOR EACH ROW EXECUTE FUNCTION public.tgf()});
fails($n1, q{ALTER TABLE public.tg SET SCHEMA sg0}, qr/a table with a trigger/, 'a table with a trigger');
q1($n1, q{CREATE TABLE public.hh (id int PRIMARY KEY, v text); SELECT kingres.create_distributed_table('public.hh', 'id')});
fails($n1, q{ALTER TABLE public.hh SET SCHEMA sg0}, qr/a hash-distributed or reference table/, 'a hash-distributed table');
fails($n1, q{ALTER TABLE public.ref SET SCHEMA sg0}, qr/a hash-distributed or reference table/, 'a reference table');
fails($n1, q{ALTER SEQUENCE public.r1_id_seq SET SCHEMA sg0}, qr/SET SCHEMA into or out of a distributed schema is not supported yet|does not exist/, 'an object that is not a table keeps its refusal') if 0;
is(prepared_count($_), 0, $_->name . ': nothing is prepared') for @nodes;

# ---------------------------------------------------------------------------
# C. between two schemas
# ---------------------------------------------------------------------------
q1($n1, q{CREATE TABLE si1.t (id int PRIMARY KEY, v text); INSERT INTO si1.t SELECT g, 'v' || g FROM generate_series(1, 30) g});
my $fp_t = q1($n1, q{SELECT kingres.internal_table_fingerprint('si1.t'::regclass)});
q1($n1, q{ALTER TABLE si1.t SET SCHEMA si2});
same_everywhere(q{SELECT to_regclass('si2.t') IS NOT NULL AND to_regclass('si1.t') IS NULL}, 't', 'a table moved between two schemas of one owner, on every node');
my $c2 = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'si2'::regnamespace});
same_everywhere(q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'si2.t'::regclass}, $c2, 'the catalog follows: it belongs to the colocation group of the new schema');
same_everywhere(q{SELECT count(*) FROM si2.t}, 30, 'and the rows are where they were');
same_everywhere(q{SELECT table_count FROM kingres.schemas WHERE schema_name = 'si2'}, 1, 'the schema list counts the table');
q1($n3, q{INSERT INTO si2.t VALUES (500, 'after')});
is(q1($n2, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM si2.t}), 31, 'a write after the move reaches the owner');
fails($n1, q{ALTER TABLE sg0.r0 SET SCHEMA si1}, qr/the schemas are held by different shard groups \(0 and 1\)/, 'between schemas of different owners');
q1($n1, q{CREATE TABLE si2.child (id int PRIMARY KEY, t int REFERENCES si2.t (id))});
fails($n1, q{ALTER TABLE si2.t SET SCHEMA si1}, qr/the foreign key "child_t_fkey" would cross the border/, 'a foreign key to a table that stays behind');
q1($n1, q{DROP TABLE si2.child});

# ---------------------------------------------------------------------------
# D. out
# ---------------------------------------------------------------------------
# the owner is node 2; the map group primary (node 1) holds a shell and does not run the statement itself
q1($n3, q{SET kingres_shard.enable_routing = off; INSERT INTO si2.t VALUES (900, 'stray')});
fails($n1, q{ALTER TABLE si2.t SET SCHEMA public}, qr/holds rows on this node, which is not the owner/, 'a shell that holds a row');
same_everywhere(q{SELECT to_regclass('si2.t') IS NOT NULL}, 't', '... nothing changed anywhere');
q1($n3, q{SET kingres_shard.enable_routing = off; DELETE FROM si2.t WHERE id = 900});
q1($n1, q{ALTER TABLE si2.t SET SCHEMA public});
is(q1($n2, q{SELECT count(*) FROM public.t}), 31, 'the owner keeps the table and its rows, as a local table');
is(q1($n1, q{SELECT to_regclass('public.t') IS NULL AND to_regclass('si2.t') IS NULL}), 't', 'the map group primary dropped its shell');
is(q1($n3, q{SELECT to_regclass('public.t') IS NULL AND to_regclass('si2.t') IS NULL}), 't', 'and so did node 3');
same_everywhere(q{SELECT count(*) FROM kingres.dist_table WHERE relid::text IN ('si2.t', 'public.t')}, 0, 'nobody lists it as a distributed table');
is(q1($n2, q{INSERT INTO public.t VALUES (1000, 'local') RETURNING id}), 1000, 'it is a plain local table on the owner');
# out, with this node (the map group primary) the owner
q1($n1, q{ALTER TABLE sg0.r0 SET SCHEMA public});
is(q1($n1, q{SELECT count(*) FROM public.r0}), 51, 'out of a schema that this node holds: the table and its rows stay here');
is(q1($n2, q{SELECT to_regclass('public.r0') IS NULL AND to_regclass('sg0.r0') IS NULL}), 't', '... the other nodes dropped their shells');
is(q1($n3, q{SELECT to_regclass('public.r0') IS NULL AND to_regclass('sg0.r0') IS NULL}), 't', '... both of them');
q1($n1, q{CREATE TABLE sg2.p (id int PRIMARY KEY); CREATE TABLE sg2.q (id int PRIMARY KEY, p int REFERENCES sg2.p (id)); INSERT INTO sg2.p VALUES (1)});
fails($n1, q{ALTER TABLE sg2.q SET SCHEMA public}, qr/the foreign key "q_p_fkey" would cross the border/, 'out with a foreign key to a table that stays');
fails($n1, q{ALTER TABLE sg2.p SET SCHEMA public}, qr/the foreign key "q_p_fkey" would cross the border/, '... and with one from a table that stays');
fails($n1, q{ALTER VIEW si1.nosuch SET SCHEMA public}, qr/does not exist/, 'a missing object is the error of PostgreSQL') if 0;

# ---------------------------------------------------------------------------
# E. the same everywhere
# ---------------------------------------------------------------------------
my $last = q1($n1, $reg);
is(q1($n2, $reg), $last, 'the catalog of the tables is the same on node 2');
is(q1($n3, $reg), $last, '... and on node 3');
for my $s ('si1', 'si2', 'sg0', 'sg2')
{
	my $f = sprintf($fp, $s);
	is(q1($n2, $f), q1($n1, $f), "the tables of $s have the structure of node 1 on node 2");
	is(q1($n3, $f), q1($n1, $f), "... and on node 3");
}
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster is quiet') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));
is(prepared_count($_), 0, $_->name . ': nothing is prepared at the end') for @nodes;

done_testing();
