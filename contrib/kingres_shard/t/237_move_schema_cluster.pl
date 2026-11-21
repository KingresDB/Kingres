# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/237_move_schema_cluster.pl
#
# Phase 11 (design/shard-phase11.md section 9), increment 5: move_schema, on three nodes with logical replication.  A distributed schema with ordinary,
# partitioned and keyed tables, a sequence and a reference table moves from one shard group to another and back.
#
#   A. a move: every row from every node before and after, the owner is the target, the old copy is emptied, nothing of the replication is left
#   B. writes and reads from every node keep working across the move (a writer runs while it copies); the sequence goes on from where it was
#   C. a move that fails before the switch is undone: the shells of the target are empty again, the schema is where it was, nothing is left
#   D. what is refused: a schema without a primary key table, an unlogged table, a target with a row in a shell, the same group, a second move
#   E. the epoch rises, the catalog is the same on every node, check_cluster is quiet, move_bucket on the schema group is the same move

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(sleep);
use KingresShardTest qw(make_cluster run_sql prepared_count move_state start_psql finish_psql wait_for_fault_file);

my @nodes = make_cluster('ms', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

sub fails
{
	my ($n, $sql, $re, $name) = @_;
	my ($rc, $out, $err) = run_sql($n, $sql);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "... message: $name") or diag($err);
}

q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});
q1($n1, q{CREATE TABLE public.ref (id int PRIMARY KEY, name text); SELECT kingres.create_reference_table('public.ref'); INSERT INTO public.ref VALUES (1, 'r1'), (2, 'r2')});
q1($n1, q{SET kingres_shard.schema_placement_group = 1; CREATE SCHEMA mv});
q1($n1, q{
	CREATE SEQUENCE mv.sq START 100;
	CREATE TABLE mv.a (id serial PRIMARY KEY, v text, r int REFERENCES public.ref (id));
	CREATE TABLE mv.b (id int PRIMARY KEY, a_id int REFERENCES mv.a (id), note text);
	CREATE TABLE mv.p (id int, d date, PRIMARY KEY (id, d)) PARTITION BY RANGE (d);
	CREATE TABLE mv.p1 PARTITION OF mv.p FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
	CREATE TABLE mv.p2 PARTITION OF mv.p FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
});
is(q1($n2, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'mv'}), 1, 'the schema is held by node 2');
q1($n1, q{INSERT INTO mv.a (v, r) SELECT 'row ' || g, 1 + g % 2 FROM generate_series(1, 120) g});
q1($n1, q{INSERT INTO mv.b SELECT g, g, 'n' || g FROM generate_series(1, 60) g});
q1($n1, q{INSERT INTO mv.p SELECT g, date '2024-01-01' + g * 5 FROM generate_series(1, 100) g});
q1($n1, q{SELECT nextval('mv.sq'), nextval('mv.sq'), nextval('mv.sq')});

sub totals
{
	my ($n) = @_;
	return q1($n, q{SELECT (SELECT count(*) || ':' || sum(id) FROM mv.a) || '|' || (SELECT count(*) || ':' || sum(a_id) FROM mv.b) || '|' || (SELECT count(*) || ':' || sum(id) FROM mv.p)
	                || '|' || (SELECT count(*) FROM mv.a a JOIN mv.b b ON b.a_id = a.id JOIN public.ref r ON r.id = a.r)});
}
sub same_everywhere
{
	my ($label, $want) = @_;
	is(totals($_), $want, "$label: the same answer from " . $_->name) for @nodes;
}
my $want = totals($n1);
is($want, '120:7260|60:1830|100:5050|60', 'the rows are the ones written');
same_everywhere('before the move', $want);

my $coloc = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'mv'::regnamespace});
my $epoch0 = q1($n1, "SELECT placement_epoch FROM kingres.colocation WHERE colocation_id = $coloc");

# ---------------------------------------------------------------------------
# D. what is refused (before the move that works: the shells of the target are untouched by a refused move)
# ---------------------------------------------------------------------------
fails($n1, q{SELECT kingres.move_schema('mv', 1)}, qr/already on shard group 1/, 'the schema is already there');
q1($n3, q{SET kingres_shard.enable_routing = off; INSERT INTO mv.a (id, v) VALUES (9999, 'stray')});
fails($n1, q{SELECT kingres.move_schema('mv', 2)}, qr/table mv\.a of the schema is missing or not empty on the target node 3/, 'a row in a shell of the target');
is(q1($n1, "SELECT count(*) FROM kingres.bucket_move WHERE colocation_id = $coloc"), 0, '... and no move was recorded');
q1($n3, q{SET kingres_shard.enable_routing = off; DELETE FROM mv.a WHERE id = 9999});
q1($n1, q{CREATE TABLE mv.nokey (x int)});
fails($n1, q{SELECT kingres.move_schema('mv', 2)}, qr/table mv\.nokey has no primary key and no full replica identity/, 'a table without a key');
q1($n1, q{ALTER TABLE mv.nokey REPLICA IDENTITY FULL});
q1($n1, q{CREATE UNLOGGED TABLE mv.unl (x int PRIMARY KEY)});
fails($n1, q{SELECT kingres.move_schema('mv', 2)}, qr/table mv\.unl is not logged/, 'an unlogged table');
q1($n1, q{DROP TABLE mv.unl});
fails($n1, q{SELECT kingres.move_schema('nothing_here', 2)}, qr/does not exist|is not a distributed schema/, 'a schema that is not distributed');
q1($n1, q{CREATE SCHEMA mv_local_x; });

# ---------------------------------------------------------------------------
# C. a move that fails before the switch is undone
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.fault_injection = 'move_after_subscription:error'; SET kingres_shard.node_timeout = 5; SELECT kingres.move_schema('mv', 2)});
	isnt($rc, 0, 'a move that fails while it copies');
	my $id = q1($n1, "SELECT max(move_id) FROM kingres.bucket_move WHERE colocation_id = $coloc");
	like(q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $id"), qr/^(failed|cancelled)$/, '... is recorded as failed') or diag(q1($n1, 'SELECT * FROM kingres.bucket_move'));
	is(q1($n1, q{SELECT group_id || ':' || state FROM kingres.schemas WHERE schema_name = 'mv'}), '1:active', '... and the schema is where it was, active');
	same_everywhere('after the failed move', $want);
	is(q1($n3, "SET kingres_shard.enable_routing = off; SELECT (SELECT count(*) FROM mv.a) + (SELECT count(*) FROM mv.b) + (SELECT count(*) FROM mv.p)"), 0, '... the shells of the target are empty again');
	is(q1($n3, q{SELECT count(*) FROM pg_subscription}), 0, '... no subscription is left');
	is(q1($n2, q{SELECT count(*) FROM pg_replication_slots}), 0, '... no slot is left');
	is(q1($n2, q{SELECT count(*) FROM pg_publication}), 0, '... no publication');
}
q1($n1, q{DROP TABLE mv.nokey});
same_everywhere('after the table without a key is dropped', $want);

# ---------------------------------------------------------------------------
# A and B. the move, with a writer that runs while it copies
# ---------------------------------------------------------------------------
my $writer = start_psql($n1, q{
	DO $$ BEGIN
		FOR i IN 1 .. 150 LOOP
			BEGIN
				INSERT INTO mv.b (id, a_id, note) VALUES (1000 + i, 1 + (i % 100), 'w' || i);
			EXCEPTION WHEN OTHERS THEN
				IF SQLSTATE = 'KS001' THEN
					INSERT INTO mv.b (id, a_id, note) VALUES (1000 + i, 1 + (i % 100), 'w' || i);
				ELSE
					RAISE;
				END IF;
			END;
			PERFORM pg_sleep(0.03);
		END LOOP;
	END $$;
});
sleep(0.3);
my ($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.node_timeout = 20; SELECT kingres.move_schema('mv', 2)});
is($rc, 0, 'move_schema succeeds while a writer runs') or diag($err);
my $mid = $out;
my ($wo, $we) = finish_psql($writer);
is($we, '', 'the writer saw no error (a statement that met the switch was repeated)') or diag($we);
like($mid, qr/^\d+$/, 'it returns the id of the move');
is(q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $mid"), 'cleaning', 'the move is cleaning: the old copy waits for its delay');
is(q1($n1, q{SELECT group_id || ':' || state FROM kingres.schemas WHERE schema_name = 'mv'}), '2:active', 'the schema is held by shard group 2, active');
for my $n (@nodes)
{
	is(q1($n, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'mv'}), 2, $n->name . ': knows the new owner');
}
is(q1($n3, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM mv.b}), 210, 'node 3 has the rows of the schema, with those the writer wrote (60 + 150)');
is(q1($n3, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM mv.b WHERE id > 1000}), 150, '... every one of the 150 writes is there once');
for my $n (@nodes)
{
	is(q1($n, q{SELECT count(*) FROM mv.b}), 210, $n->name . ': reads all 210 rows of mv.b');
	is(q1($n, q{SELECT count(*) || ':' || sum(id) FROM mv.a}), '120:7260', $n->name . ': and mv.a, mv.p are as before');
	is(q1($n, q{SELECT count(*) || ':' || sum(id) FROM mv.p}), '100:5050', $n->name . ': (partitions too)');
}
# the old copy is emptied by the cleaning; the delay is the setting kingres_shard.move_cleanup_delay, forced here
is(q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)}), 1, 'cleanup_moved_buckets(force) finishes the move');
is(q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $mid"), 'done', '... it is done');
is(q1($n2, q{SET kingres_shard.enable_routing = off; SELECT (SELECT count(*) FROM mv.a) + (SELECT count(*) FROM mv.b) + (SELECT count(*) FROM mv.p)}), 0, 'the old copy on node 2 is empty');
is(q1($n3, q{SELECT count(*) FROM pg_subscription}) . q1($n2, q{SELECT count(*) FROM pg_replication_slots}) . q1($n2, q{SELECT count(*) FROM pg_publication}), '000', 'nothing of the replication is left');
cmp_ok(q1($n1, "SELECT placement_epoch FROM kingres.colocation WHERE colocation_id = $coloc"), '>', $epoch0, 'the placement epoch rose');
is(scalar(keys %{{ map { $_ => 1 } map { q1($_, "SELECT placement_epoch FROM kingres.colocation WHERE colocation_id = $coloc") } @nodes }}), 1, '... and is the same on every node');

# ---------------------------------------------------------------------------
# B (continued). writes after the move, from every node; the sequence
# ---------------------------------------------------------------------------
{
	my @ids = map { q1($_, q{SELECT nextval('mv.sq')}) } ($n1, $n2, $n3);
	cmp_ok($ids[0], '>', 102, 'the sequence goes on after the move: the first value is above what the old owner handed out') or diag("@ids");
	is(scalar(keys %{{ map { $_ => 1 } @ids }}), 3, '... and three nodes get three values');
	q1($n2, q{INSERT INTO mv.a (v, r) VALUES ('after 1', 1)});
	q1($n1, q{INSERT INTO mv.a (v, r) VALUES ('after 2', 2)});
	is(q1($n3, q{SELECT count(*) FROM mv.a WHERE v LIKE 'after %'}), 2, 'writes from the other nodes land on the new owner');
	is(q1($n3, q{SELECT count(*) = count(DISTINCT id) FROM mv.a}), 't', '... with the ids of the sequence of the new owner, no id twice');
	is(q1($n2, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM mv.a}), 0, 'the old owner has no row of it');
}

# ---------------------------------------------------------------------------
# E. back again, through move_bucket; the checks
# ---------------------------------------------------------------------------
{
	my ($rc2, $out2, $err2) = run_sql($n1, "SELECT kingres.move_bucket($coloc, 0, 0)");
	is($rc2, 0, 'move_bucket of the schema group is the same move (to group 0)') or diag($err2);
	is(q1($n1, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'mv'}), 0, 'the schema is on group 0');
	is(q1($n1, q{SELECT count(*) FROM mv.a}), 122, 'all its rows came with it (120 and the two written after the first move)');
	is(q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)}), 1, 'cleaned up');
	is(q1($n3, q{SET kingres_shard.enable_routing = off; SELECT (SELECT count(*) FROM mv.a) + (SELECT count(*) FROM mv.b) + (SELECT count(*) FROM mv.p)}), 0, 'node 3 holds nothing of it any more');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster is quiet') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_move WHERE phase NOT IN ('done', 'failed', 'cancelled')}), 0, 'no move is unfinished');
	fails($n1, "SELECT kingres.split_bucket($coloc, 0)", qr/never split/, 'a schema group is still never split');
}
for my $n (@nodes) { is(prepared_count($n), 0, 'nothing is prepared on ' . $n->name); }

done_testing();
