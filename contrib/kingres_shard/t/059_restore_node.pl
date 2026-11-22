# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/059_restore_node.pl
#
# The procedure of the README section "Restoring a node from a logical dump"
# (review finding S6-08 of kingres/docs/reviews/2026-11-15-kingres-shard-0.4-port.md:
# the first step said to restore into a database that has no kingres_shard, but the
# dump of a node creates the extension, and the reviewer did not run steps 2 and 3).
# Every step of the section is run here, in this order:
#
#	1. a dump of a node restored into a database elsewhere: the dump creates the
#	   extension, which has no catalog there, so the partitions are plain tables;
#	   the tables that a bucket move left (kingres.orphan_*) are in the dump unless
#	   they are left out with -T;
#	2. a node that lost its database is put back in place: the dump restored into
#	   an empty database at the same address, then kingres.sync_catalog() on the map
#	   group primary;
#	3. the partitions of a node that is in a cluster are repaired from a data-only
#	   dump with routing off;
#	4. what does not work: add_node() of a node that holds the tables already.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster run_sql);

my $tmp = PostgreSQL::Test::Utils::tempdir();
sub q1 { my ($n, $sql, %o) = @_; return $n->safe_psql($o{db} // 'postgres', $sql); }

my ($n1, $n2, $n3) = make_cluster('rs', 3, logical => 1);
q1($n1, q{
	CREATE TABLE public.r (id int PRIMARY KEY, name text);
	SELECT kingres.create_reference_table('public.r');
	INSERT INTO public.r VALUES (1, 'a'), (2, 'b'), (3, 'c');
	CREATE TABLE public.d (id int PRIMARY KEY, rid int, v text);
	SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 6);
	ALTER TABLE public.d ADD FOREIGN KEY (rid) REFERENCES public.r(id);
});
q1($n1, join('', map { "INSERT INTO public.d VALUES ($_, " . (1 + $_ % 3) . ", 'v$_');\n" } 1 .. 60));
is(q1($n1, 'SELECT count(*) FROM public.d'), '60', 'the cluster has its rows');

# A bucket moves away from node 2 and leaves its orphan table there.
my $bucket = q1($n1, q{SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = 1});
q1($n1, "SELECT kingres.move_bucket(1, $bucket, 2)");
is(q1($n2, q{SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan%' AND relkind = 'r'}), '1',
	'the move left an orphan table on node 2');
is(q1($n1, 'SELECT count(*) FROM public.d'), '60', 'the move lost nothing');
my $local2 = q1($n2, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.d});
my $local3 = q1($n3, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.d});

# ---------------------------------------------------------------------------
# 1. The dump of node 2, restored elsewhere.
# ---------------------------------------------------------------------------
my $dump = "$tmp/node2.sql";
command_ok([ 'pg_dump', '-h', $n2->host, '-p', $n2->port, '-d', 'postgres', '-f', $dump ], 'pg_dump of node 2');
like(slurp_file($dump), qr/^CREATE EXTENSION IF NOT EXISTS kingres_shard/m, 'the dump creates the extension');
like(slurp_file($dump), qr/CREATE TABLE kingres\.orphan_/, '...and holds the orphan table of the move');
my $clean = "$tmp/node2_clean.sql";
command_ok([ 'pg_dump', '-h', $n2->host, '-p', $n2->port, '-d', 'postgres', '-T', 'kingres.orphan_*', '-f', $clean ],
	'pg_dump of node 2 without the orphan tables');
unlike(slurp_file($clean), qr/kingres\.orphan_/, '...which are not in it with -T');
my $x = make_node('rsx');
q1($x, 'CREATE DATABASE restored1');
my ($rc, $out, $err) = run_sql($x, '', extra_params => [ '-d', 'restored1', '-v', 'ON_ERROR_STOP=1', '-f', $clean ]);
is($rc, 0, 'step 1: the dump is restored into a database that has no kingres_shard') or diag $err;
is(q1($x, q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_shard'}, db => 'restored1'), '0.9', 'step 1: the dump made the extension');
is(q1($x, 'SELECT count(*) FROM kingres.node', db => 'restored1'), '0', 'step 1: it has no catalog');
is(q1($x, 'SELECT count(*) FROM public.d', db => 'restored1'), $local2, 'step 1: the rows of the node are there');
is(q1($x, q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'public.d'::regclass}, db => 'restored1'), '1', 'step 1: the partitions are tables that the restored d has as partitions, with no cluster behind them');

# ---------------------------------------------------------------------------
# 2. A node that lost its database is put back in place.
# ---------------------------------------------------------------------------
is(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || node_id, ', '), '') FROM kingres.check_cluster()}), '',
	'the cluster is clean before the node is lost');
q1($n2, 'DROP DATABASE postgres WITH (FORCE)', db => 'template1');
q1($n2, 'CREATE DATABASE postgres', db => 'template1');
($rc, $out, $err) = run_sql($n2, '', extra_params => [ '-d', 'postgres', '-v', 'ON_ERROR_STOP=1', '-f', $clean ]);
is($rc, 0, 'step 2: the dump is restored into the empty database of the node') or diag $err;
like(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || node_id, ', '), '') FROM kingres.check_cluster()}), qr/\S/,
	'step 2: until the catalog is pushed, check_cluster names the node');
is(join('/', map { q1($n2, "SELECT count(*) FROM kingres.$_") } qw(node local_node cluster_secret)), '0/0/0', 'step 2: the restored node has no catalog, no identity and no secret');
q1($n1, 'SELECT kingres.sync_catalog()');
is(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || node_id, ', '), '') FROM kingres.check_cluster()}), '',
	'step 2: and then check_cluster finds nothing') or diag(q1($n1, q{SELECT check_name, node_id, detail FROM kingres.check_cluster()}));
is(q1($n1, 'SELECT count(*) FROM public.d'), '60', 'step 2: the rows of the node are back in the cluster');
is(q1($n2, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.d}), $local2, 'step 2: node 2 holds its rows');
is(q1($n2, 'SELECT count(*) FROM kingres.local_node'), '1', 'step 2: and knows which node it is');
is(q1($n3, 'SELECT count(*) FROM public.d'), '60', 'step 2: a scan from another node sees them');
($rc, $out, $err) = run_sql($n3, q{INSERT INTO public.r VALUES (4, 'd'); INSERT INTO public.d VALUES (1001, 4, 'new')});
is($rc, 0, 'step 2: a write to a reference table and to a distributed table works') or diag $err;
is(join(',', map { q1($_, 'SELECT count(*) FROM public.r') } ($n1, $n2, $n3)), '4,4,4', 'step 2: on every node');
q1($n1, q{DELETE FROM public.d WHERE id = 1001; DELETE FROM public.r WHERE id = 4});

# ---------------------------------------------------------------------------
# 3. The partitions of a node in a cluster are repaired from a data-only dump.
# ---------------------------------------------------------------------------
my $data = "$tmp/node3_data.sql";
command_ok([ 'pg_dump', '-h', $n3->host, '-p', $n3->port, '-d', 'postgres', '--data-only', '--disable-triggers', '-t', 'public.d_b*', '-f', $data ],
	'pg_dump --data-only of the partitions of node 3');
q1($n3, q{SET kingres_shard.enable_routing = off; DELETE FROM public.d});
is(q1($n3, q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.d}), '0', 'the partitions of node 3 are emptied (by hand)');
is(q1($n1, 'SELECT count(*) FROM public.d'), 60 - $local3, 'the cluster sees the loss');
($rc, $out, $err) = run_sql($n3, '', extra_params => [ '-d', 'postgres', '-v', 'ON_ERROR_STOP=1', '-f', $data ]);
isnt($rc, 0, 'step 3: the data-only dump is refused while routing is on');
like($err, qr/bucket partition|not supported/, '...by the extension');
my $script = "$tmp/node3_data_routing_off.sql";
append_to_file($script, "SET kingres_shard.enable_routing = off;\n" . slurp_file($data));
($rc, $out, $err) = run_sql($n3, '', extra_params => [ '-d', 'postgres', '-v', 'ON_ERROR_STOP=1', '-f', $script ]);
is($rc, 0, 'step 3: with kingres_shard.enable_routing = off at its top it loads') or diag $err;
is(q1($n1, 'SELECT count(*) FROM public.d'), '60', 'step 3: the cluster has its rows again');
is(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || node_id, ', '), '') FROM kingres.check_cluster()}), '', 'step 3: check_cluster finds nothing');

# ---------------------------------------------------------------------------
# 4. What does not work: add_node() of a node that holds the tables already.
# ---------------------------------------------------------------------------
my $y = make_node('rsy');
($rc, $out, $err) = run_sql($y, '', extra_params => [ '-d', 'postgres', '-v', 'ON_ERROR_STOP=1', '-f', $clean ]);
is($rc, 0, 'a dump restored into a new node') or diag $err;
($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $y->host, $y->port));
isnt($rc, 0, 'step 4: add_node() of a node that has the tables already fails');
like($err, qr/already exists/, '...because they exist');
is(q1($n1, 'SELECT count(*) FROM kingres.node'), '3', 'step 4: and registers nothing');
my $z = make_node('rsz');
($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $z->host, $z->port));
is($rc, 0, 'step 4: add_node() of an empty node works') or diag $err;
is(q1($z, q{SELECT count(*) FROM public.r}), '3', 'step 4: it got the reference table');

done_testing();
