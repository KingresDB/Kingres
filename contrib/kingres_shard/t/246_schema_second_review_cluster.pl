# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/246_schema_second_review_cluster.pl
#
# The fixes for the second independent review of phase 11 (P11S-01 to P11S-08), on three nodes with logical replication; needs kingres_ledger installed.
#
#   01 ledger_enable while the schema moves        02 distribute_schema of a schema with an empty ledger table
#   03 a shipped function (or a trigger) that reads a local table      04 UNION ALL with an expression column
#   05 CREATE TABLE IF NOT EXISTS ... AS, AS EXECUTE      06 the shell of the node that runs SET SCHEMA, the reset of the path after a router batch
#   07 a user trigger named like the ledger's      08 SET SCHEMA locks the table before it asks for rows

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run ();
use KingresShardTest qw(make_cluster run_sql prepared_count start_psql finish_psql);

my @nodes = make_cluster('sr', 3, logical => 1,
	extra => "shared_preload_libraries = 'kingres_shard,kingres_ledger'\nkingres_ledger.block_interval = 0\n");
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

q1($_, q{CREATE EXTENSION kingres_ledger}) for @nodes;
q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});
q1($_, q{CREATE TABLE public.loc (id int PRIMARY KEY, tag text); INSERT INTO public.loc SELECT g, 'loc' || g FROM generate_series(1, 6) g}) for @nodes;

# ---------------------------------------------------------------------------
# P11S-01: ledger_enable while the schema is being moved
# ---------------------------------------------------------------------------
q1($n1, q{SET kingres_shard.schema_placement_group = 1; CREATE SCHEMA lg});
q1($n1, q{CREATE TABLE lg.t (id int PRIMARY KEY, v text); INSERT INTO lg.t SELECT g, 'v' || g FROM generate_series(1, 10) g});
{
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_lg_$$";
	append_to_file($gate, 'x');
	my ($o, $e) = ('', '');
	my $hh = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
		'-c', "SET kingres_shard.fault_injection = 'move_after_catchup:wait:$gate'", '-c', "SET kingres_shard.node_timeout = 30", '-c', "SELECT kingres.move_schema('lg', 2)" ], '>', \$o, '2>', \$e);
	ok(KingresShardTest::wait_for_fault_file("$gate.reached", 90), 'the move is held after the catch-up');
	fails($n2, q{SELECT kingres.ledger_enable('lg.t')}, qr/is being moved to another shard group/, 'ledger_enable on the owner while the schema moves');
	is(q1($n2, q{SELECT count(*) FROM kingres.ledger_table WHERE relid = 'lg.t'::regclass}), 0, '... nothing was registered');
	unlink $gate;
	IPC::Run::finish($hh);
	is($e, '', 'the held move ends') or diag($e);
}
q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
same_everywhere(q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'lg'}, 2, 'the schema is on group 2');
q1($n1, q{UPDATE lg.t SET v = 'plain' WHERE id = 1});
is(q1($n3, q{SELECT v FROM lg.t WHERE id = 1}), 'plain', 'a table that never got a ledger is an ordinary table');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster is quiet') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));
# after the move the new owner can have one
q1($n3, q{SELECT kingres.ledger_enable('lg.t')});
is(q1($n3, q{SELECT count(*) FROM kingres.ledger_table WHERE relid = 'lg.t'::regclass}), 1, 'on the new owner ledger_enable works');

# ---------------------------------------------------------------------------
# P11S-02: an empty ledger table keeps its schema with the ledger
# ---------------------------------------------------------------------------
q1($n1, q{SET kingres_shard.distribute_new_schemas = off; CREATE SCHEMA lz}); q1($n1, q{CREATE TABLE lz.t (id int PRIMARY KEY, v text); SELECT kingres.ledger_enable('lz.t')});
fails($n1, q{SELECT kingres.distribute_schema('lz', 1)}, qr/holds the ledger table\(s\) t on shard group 0, not on group 1/, 'distribute_schema of a schema with an empty ledger table to another group');
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'lz'::regnamespace}), 0, '... the schema is not distributed');
q1($n1, q{SELECT kingres.distribute_schema('lz')});
same_everywhere(q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'lz'}, 0, 'without a group it stays with the ledger');
q1($n2, q{INSERT INTO lz.t VALUES (1, 'x')});
is(q1($n1, q{SELECT count(*) FROM kingres.ledger_entry}), 1, 'a row written from another node is in the ledger of the owner');
fails($n2, q{UPDATE lz.t SET v = 'y'}, qr/./, 'and the table cannot be changed');

# ---------------------------------------------------------------------------
# P11S-03: a shipped function that reads a table of this node only
# ---------------------------------------------------------------------------
q1($n1, q{SET kingres_shard.schema_placement_group = 1; CREATE SCHEMA ra});
q1($n1, q{CREATE TABLE ra.t (id int PRIMARY KEY, v text, n int DEFAULT 5); INSERT INTO ra.t SELECT g, 'r' || g FROM generate_series(1, 4) g;
          CREATE FUNCTION ra.cnt_loc() RETURNS bigint LANGUAGE sql STABLE AS 'SELECT count(*) FROM public.loc';
          CREATE FUNCTION ra.cnt_own() RETURNS bigint LANGUAGE sql STABLE AS 'SELECT count(*) FROM ra.t'});
q1($n3, q{SET kingres_shard.enable_routing = off; DELETE FROM public.loc WHERE id > 2});
q1($n1, q{SELECT kingres.alter_distributed_schema('ra', true)});
is(q1($n3, q{SELECT ra.cnt_own() FROM ra.t LIMIT 1}), 4, 'a shipped function that reads a table of its schema works');
fails($n1, q{SELECT ra.cnt_loc(), count(*) FROM ra.t}, qr/is read by code that a statement of another node runs here/, 'a shipped function that reads a plain local table');
fails($n3, q{SELECT ra.cnt_loc(), count(*) FROM ra.t}, qr/is read by code that a statement of another node runs here/, '... from the other node too');
is(q1($n3, q{SELECT ra.cnt_loc()}), 2, 'the function alone runs here (the call of a function is not shipped)');
q1($n1, q{SELECT kingres.alter_distributed_schema('ra', false)});
is(q1($n3, q{SELECT ra.cnt_loc() FROM ra.t LIMIT 1}), 2, 'without ship_functions the function is run by the origin and reads its own table');
q1($n1, q{CREATE TABLE ra.tg (id int PRIMARY KEY, v text);
          CREATE FUNCTION ra.tgf() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN NEW.v := (SELECT count(*) FROM public.loc)::text; RETURN NEW; END';
          CREATE TRIGGER tg_b BEFORE INSERT ON ra.tg FOR EACH ROW EXECUTE FUNCTION ra.tgf()});
fails($n3, q{INSERT INTO ra.tg (id) VALUES (1)}, qr/is read by code that a statement of another node runs here/, 'a trigger that reads a plain local table, from another node');
fails($n3, q{INSERT INTO ra.tg (id, v) SELECT id + 50, tag FROM public.loc}, qr/is read by code that a statement of another node runs here/, 'the same for rows that the row router sends');
q1($n2, q{INSERT INTO ra.tg (id) VALUES (2)});
is(q1($n2, q{SELECT v FROM ra.tg WHERE id = 2}), 6, 'on the owner it reads the owner table');

# ---------------------------------------------------------------------------
# P11S-04: UNION ALL with an expression column
# ---------------------------------------------------------------------------
q1($n1, q{CREATE TABLE public.h (id int PRIMARY KEY, v text); SELECT kingres.create_distributed_table('public.h', 'id', bucket_count => 8);
          INSERT INTO public.h SELECT g, 'h' || g FROM generate_series(1, 12) g;
          CREATE TABLE public.ref (id int PRIMARY KEY, v text); SELECT kingres.create_reference_table('public.ref'); INSERT INTO public.ref VALUES (1, 'a'), (2, 'b')});
q1($_, q{SET kingres_shard.enable_routing = off; DELETE FROM public.loc; INSERT INTO public.loc SELECT g, 'loc' || g FROM generate_series(1, 6) g}) for @nodes;
my $ua_s = q{SELECT count(*) || ':' || sum(x) FROM (SELECT id + 1 AS x, v FROM ra.t UNION ALL SELECT id + 2000, tag FROM public.loc) u};
my $ua_h = q{SELECT count(*) || ':' || sum(x) FROM (SELECT id + 1 AS x, v FROM public.h UNION ALL SELECT id + 2000, tag FROM public.loc) u};
my $ua_r = q{SELECT count(*) || ':' || sum(x) FROM (SELECT id * 2 AS x, v FROM public.ref UNION ALL SELECT id + 2000, tag FROM public.loc) u};
for my $n (@nodes)
{
	is(q1($n, $ua_s), '10:12035', $n->name . ': UNION ALL with an expression over a schema table'); 
	is(q1($n, $ua_h), '18:12111', $n->name . ': ... over a hash-distributed table');
	is(q1($n, $ua_r), '8:12027', $n->name . ': ... over a reference table');
	is(q1($n, q{SET kingres_shard.enable_schema_routing = off; } . $ua_s), '10:12035', $n->name . ': ... with schema routing off');
}
q1($n1, q{CREATE TABLE ra.u (id int PRIMARY KEY, v text)});
q1($n3, q{INSERT INTO ra.u SELECT id + 1, v FROM ra.t UNION ALL SELECT id + 2000, tag FROM public.loc});
is(q1($n2, q{SELECT count(*) || ':' || sum(id) FROM ra.u}), '10:12035', 'INSERT ... SELECT of a UNION ALL with expressions, through the router');

# ---------------------------------------------------------------------------
# P11S-05: CREATE TABLE IF NOT EXISTS ... AS, AS EXECUTE
# ---------------------------------------------------------------------------
q1($n1, q{CREATE TABLE ra.c1 AS SELECT 1 AS x});
{
	my ($rc, $out, $err) = run_sql($n1, q{CREATE TABLE IF NOT EXISTS ra.c1 AS SELECT 2 AS x});
	is($rc, 0, 'CREATE TABLE IF NOT EXISTS ... AS on a table that exists succeeds') or diag($err);
	like($err, qr/relation "c1" already exists, skipping/, '... with the notice of PostgreSQL');
	is(q1($n2, q{SELECT x FROM ra.c1}), 1, '... and does not touch the table');
}
fails($n1, q{PREPARE pp AS SELECT 3 AS x; CREATE TABLE ra.c5 AS EXECUTE pp}, qr/CREATE TABLE AS EXECUTE into a distributed schema is not supported/, 'CREATE TABLE AS EXECUTE');
same_everywhere(q{SELECT to_regclass('ra.c5') IS NULL}, 't', '... the table was not made');

# ---------------------------------------------------------------------------
# P11S-06: the shell of the node that runs SET SCHEMA; the path after a router batch
# ---------------------------------------------------------------------------
q1($n1, q{CREATE TABLE ra.o (id int PRIMARY KEY)});
q1($n1, q{SET kingres_shard.enable_routing = off; INSERT INTO ra.o VALUES (1)});
fails($n1, q{ALTER TABLE ra.o SET SCHEMA public}, qr/holds rows on this node, which is not the owner/, 'a row in the shell of the node that runs SET SCHEMA out');
same_everywhere(q{SELECT to_regclass('ra.o') IS NOT NULL}, 't', '... nothing changed anywhere');
q1($n1, q{SET kingres_shard.enable_routing = off; DELETE FROM ra.o});
{
	my $log0 = slurp_file($n2->logfile);
	q1($n1, q{CREATE TABLE ra.tz (id int PRIMARY KEY, v text); CREATE FUNCTION ra.tzf() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN NEW.v := 1; RETURN NEW; END'; CREATE TRIGGER tz_b BEFORE INSERT ON ra.tz FOR EACH ROW EXECUTE FUNCTION ra.tzf()});
	q1($n1, q{BEGIN; SET LOCAL search_path = ra; INSERT INTO tz (id) SELECT id + 100 FROM public.loc LIMIT 1; SELECT count(*) FROM ra.t; COMMIT});
	my $log = slurp_file($n2->logfile);
	my $n = () = substr($log, length($log0)) =~ /SET LOCAL search_path = pg_catalog; SET LOCAL kingres_shard\.shipped_scope = off/g;
	cmp_ok($n, '>=', 1, 'after a router batch the owner connection gets its fixed path back (the later statements of the transaction do not see the path of the batch)');
}

# ---------------------------------------------------------------------------
# P11S-07: a user trigger named like the ledger's
# ---------------------------------------------------------------------------
q1($n1, q{CREATE TABLE ra.ut (id int PRIMARY KEY, v text);
          CREATE FUNCTION ra.utf() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN NEW.v := ''trig''; RETURN NEW; END';
          CREATE TRIGGER kingres_ledger_mine BEFORE INSERT ON ra.ut FOR EACH ROW EXECUTE FUNCTION ra.utf()});
same_everywhere(q{SELECT count(*) FROM pg_trigger WHERE tgrelid = 'ra.ut'::regclass AND tgname = 'kingres_ledger_mine'}, 1, 'a user trigger with the ledger prefix is on every node');
q1($n1, q{INSERT INTO ra.ut (id) VALUES (1)});
is(q1($n2, q{SELECT v FROM ra.ut WHERE id = 1}), 'trig', '... and fires for the rows of the owner');

# ---------------------------------------------------------------------------
# P11S-08: SET SCHEMA locks the table before it asks for rows
# ---------------------------------------------------------------------------
q1($n1, q{CREATE TABLE public.rw (id int PRIMARY KEY)});
{
	my $writer = start_psql($n1, q{BEGIN; INSERT INTO public.rw VALUES (1); SELECT pg_sleep(3); COMMIT});
	select(undef, undef, undef, 0.8);
	my ($rc, $out, $err) = run_sql($n1, q{ALTER TABLE public.rw SET SCHEMA ra});
	finish_psql($writer);
	isnt($rc, 0, 'SET SCHEMA waits for a writer of the table and then sees its row');
	like($err, qr/the table holds rows on shard group 0 and the schema is held by group 1/, '... so it is refused') or diag($err);
	same_everywhere(q{SELECT to_regclass('ra.rw') IS NULL}, 't', '... and no shell was made');
}

is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster is quiet at the end') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));
is(prepared_count($_), 0, $_->name . ': no prepared transaction is left') for @nodes;

done_testing();
