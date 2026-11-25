# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/242_schema_ledger_cluster.pl
#
# Phase 11 (design/shard-phase11.md 11.1, section 16 step 11): ledger tables (kingres_ledger) in a distributed schema, on three nodes with logical
# replication.  This test needs contrib/kingres_ledger built and installed into the same installation as kingres_shard.
#
#   A. ledger_enable on the owner works; on a node that is not the owner it is refused and names the owner; the ledger of the owner covers the
#      rows that other nodes write
#   B. the pin: a schema with a ledger table cannot be moved, undistributed or dropped
#   C. a schema with a ledger table is distributed in place (the ledger stays with the data), and check_cluster reports a ledger on a node that
#      is not the owner (ledger_on_non_owner)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('lg', 3, logical => 1,
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

q1($_, q{CREATE EXTENSION kingres_ledger}) for @nodes;
q1($n1, q{SELECT kingres.set_sharding_mode('both')});

# ---------------------------------------------------------------------------
# A. the owner
# ---------------------------------------------------------------------------
q1($n1, q{CREATE SCHEMA ls; SELECT kingres.distribute_schema('ls', 1)});
q1($n1, q{CREATE TABLE ls.t (id int PRIMARY KEY, v text)});
fails($n1, q{SELECT kingres.ledger_enable('ls.t')}, qr/is in a distributed schema that shard group 1 holds/, 'ledger_enable on the map group primary, which holds a shell');
{
	my ($rc, $out, $err) = run_sql($n3, q{SELECT kingres.ledger_enable('ls.t')});
	like($err, qr/Run kingres\.ledger_enable on node 2, the primary of shard group 1/, '... the hint names the owner') or diag($err);
}
is(q1($n1, q{SELECT count(*) FROM kingres.ledger_table WHERE relid IN (to_regclass('ls.t'), to_regclass('lm.t'), to_regclass('lm.u'))}), 0, 'nothing was registered on the shell');
q1($n2, q{SELECT kingres.ledger_enable('ls.t')});
is(q1($n2, q{SELECT count(*) FROM kingres.ledger_table WHERE relid IN (to_regclass('ls.t'), to_regclass('lm.t'), to_regclass('lm.u'))}), 1, 'on the owner it works');
is(q1($n1, q{SELECT count(*) FROM kingres.ledger_table WHERE relid IN (to_regclass('ls.t'), to_regclass('lm.t'), to_regclass('lm.u'))}) + q1($n3, q{SELECT count(*) FROM kingres.ledger_table WHERE relid IN (to_regclass('ls.t'), to_regclass('lm.t'), to_regclass('lm.u'))}), 0, '... and only there');
q1($n3, q{INSERT INTO ls.t SELECT g, 'row' || g FROM generate_series(1, 20) g});
q1($n1, q{INSERT INTO ls.t VALUES (100, 'from n1')});
is(q1($n2, q{SELECT count(*) FROM kingres.ledger_entry}), 21, 'the rows that other nodes write are in the ledger of the owner (the capture trigger fired there)');
is(q1($n1, q{SELECT count(*) FROM kingres.ledger_entry}) + q1($n3, q{SELECT count(*) FROM kingres.ledger_entry}), 0, '... and in no other');
is(q1($n2, q{SELECT count(*) FROM pg_trigger WHERE tgrelid = 'ls.t'::regclass AND tgname LIKE 'kingres_ledger_%'}), 3, 'the owner has the three ledger triggers');
is(q1($n3, q{SELECT count(*) FROM pg_trigger WHERE tgrelid = 'ls.t'::regclass AND NOT tgisinternal}), 0, 'a shell has none');
fails($n3, q{UPDATE ls.t SET v = 'changed' WHERE id = 1}, qr/./, 'an append-only ledger table refuses an update shipped from another node');
fails($n1, q{DELETE FROM ls.t WHERE id = 2}, qr/./, '... and a delete');
{
	my ($rc, $out, $err) = run_sql($n2, q{SELECT count(*) FROM kingres.ledger_verify()});
	is($rc, 0, 'ledger_verify() runs on the owner') or diag($err);
}
q1($n1, q{CREATE TABLE ls.plain (id int PRIMARY KEY)});
is(q1($n2, q{SELECT to_regclass('ls.plain') IS NOT NULL}), 't', 'DDL on the schema still goes to every node');

# ---------------------------------------------------------------------------
# B. the pin
# ---------------------------------------------------------------------------
fails($n1, q{SELECT kingres.move_schema('ls', 2)}, qr/schema_pinned: schema "ls" holds the ledger table\(s\) t on shard group 1/, 'move_schema of a schema with a ledger table');
is(q1($n1, q{SELECT group_id || ':' || state FROM kingres.schemas WHERE schema_name = 'ls'}), '1:active', '... the schema is where it was');
is(q1($n1, q{SELECT count(*) FROM kingres.bucket_move}), 0, '... and no move was recorded');
fails($n1, q{SELECT kingres.undistribute_schema('ls')}, qr/schema_pinned: schema "ls" holds the ledger table\(s\) t/, 'undistribute_schema of a schema with a ledger table');
# the pin has its own SQLSTATE (KS020), which the control plane matches instead of the text; the move and the undistribute carry it
foreach my $call (q{SELECT kingres.undistribute_schema('ls')}, q{SELECT kingres.move_schema('ls', 2)})
{
	my ($rc, $out, $err) = run_sql($n1, "\\set VERBOSITY verbose\n" . $call);
	like($err, qr/\bKS020\b/, "... SQLSTATE KS020: $call");
}
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.undistribute_schema('ls')});
	like($err, qr/Only the per-shard ledger of a later phase/, '... the hint says what would make it possible') or diag($err);
}
fails($n1, q{DROP SCHEMA ls CASCADE}, qr/./, 'DROP SCHEMA of a schema with a ledger table');
is(q1($_, q{SELECT to_regclass('ls.t') IS NOT NULL AND to_regclass('ls.plain') IS NOT NULL}), 't', $_->name . ': nothing was dropped') for @nodes;
is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema WHERE nsp = 'ls'::regnamespace}), 1, '... the schema is still distributed');
is(prepared_count($_), 0, $_->name . ': no prepared transaction is left') for @nodes;
# a schema without a ledger table moves as usual
q1($n1, q{CREATE SCHEMA free1; SELECT kingres.distribute_schema('free1', 1); CREATE TABLE free1.t (id int PRIMARY KEY, v text); INSERT INTO free1.t VALUES (1, 'a')});
{
	my ($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.node_timeout = 20; SELECT kingres.move_schema('free1', 2)});
	is($rc, 0, 'a schema without a ledger table is not pinned') or diag($err);
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
}
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster is quiet') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));

# ---------------------------------------------------------------------------
# C. a schema with a ledger table, distributed in place; a ledger on the wrong node
# ---------------------------------------------------------------------------
q1($n1, q{CREATE SCHEMA lm; CREATE TABLE lm.t (id int PRIMARY KEY, v text); CREATE TABLE lm.u (id int PRIMARY KEY, v text);
          INSERT INTO lm.t VALUES (1, 'one'), (2, 'two'); SELECT kingres.ledger_enable('lm.t'); SELECT kingres.ledger_enable('lm.u', 'updatable')});
my $hist = q1($n1, q{SELECT history_relid::regclass::text FROM kingres.ledger_table WHERE relid = 'lm.u'::regclass});
diag("the history table of lm.u is $hist");
{
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.distribute_schema('lm')});
	is($rc, 0, 'a schema with ledger tables is distributed in place') or diag($err);
}
is(q1($n1, q{SELECT group_id FROM kingres.schemas WHERE schema_name = 'lm'}), 0, 'its owner is the node that holds the ledger');
for my $n ($n2, $n3)
{
	is(q1($n, q{SELECT count(*) FROM pg_trigger WHERE tgrelid IN ('lm.t'::regclass, 'lm.u'::regclass) AND NOT tgisinternal}), 0, $n->name . ': the shells have no ledger trigger');
	is(q1($n, q{SELECT count(*) FROM kingres.ledger_table WHERE relid IN (to_regclass('lm.t'), to_regclass('lm.u'))}), 0, $n->name . ': and nothing is registered in its ledger');
}
is(q1($n1, q{SELECT count(*) FROM kingres.ledger_table WHERE relid IN ('lm.t'::regclass, 'lm.u'::regclass)}), 2, 'the owner still has both as ledger tables');
q1($n2, q{INSERT INTO lm.t VALUES (3, 'three')});
is(q1($n1, q{SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table lt ON lt.table_id = e.table_id WHERE lt.relid = 'lm.t'::regclass}), 3, 'a write from another node is captured on the owner (two old rows entered, one new)');
is(q1($n3, q{SELECT count(*) FROM lm.t}), 3, 'every node reads the rows');
fails($n1, q{SELECT kingres.move_schema('lm', 1)}, qr/schema_pinned/, 'it is pinned too');
# a ledger on the wrong node: the catalog is made to say that another group holds the schema
{
	my $c = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'lm'::regnamespace});
	q1($_, "SELECT kingres.internal_set_schema_group($c, 1, 'active')") for @nodes;
	my $rows = q1($n1, q{SELECT check_name || ':' || node_id || ':' || object FROM kingres.check_cluster() WHERE check_name = 'ledger_on_non_owner' ORDER BY object});
	is($rows, "ledger_on_non_owner:1:lm.t\nledger_on_non_owner:1:lm.u", 'check_cluster reports ledger_on_non_owner for the tables of node 1');
	q1($_, "SELECT kingres.internal_set_schema_group($c, 0, 'active')") for @nodes;
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'ledger_on_non_owner'}), 0, '... and it is quiet when the owner holds the ledger');
}
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), 0, 'check_cluster is quiet') or diag(q1($n1, q{SELECT * FROM kingres.check_cluster()}));
is(prepared_count($_), 0, $_->name . ': no prepared transaction is left at the end') for @nodes;

done_testing();
