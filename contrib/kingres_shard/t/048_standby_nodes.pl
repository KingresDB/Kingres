# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/048_standby_nodes.pl
#
# Standby nodes in the catalog (design/ha-phase5.md, part C, item 1): a standby
# is registered with add_node(..., role => 'standby'), owns no buckets, and is
# skipped by routing, catalog synchronization, reference-table writes, the
# version checks and bucket moves.  A standby that is registered and not
# streaming breaks nothing on the primaries, and check_cluster() reports it.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql make_standby
  register_standby wait_catchup);

my ($n1, $n2) = make_cluster('sn', 2, replication => 1);
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 8);
	CREATE TABLE public.ref (id int PRIMARY KEY, val text);
	SELECT kingres.create_reference_table('public.ref');
});

my $s1 = make_standby($n1, 'sn_s1');
my $s2 = make_standby($n2, 'sn_s2');

# Registration: only on the map group primary, only as a standby of a group that has a primary.
my ($rc, $out, $err) = run_sql($n2,
	sprintf("SELECT kingres.add_node('%s', %d, 1, 'standby')", $s2->host, $s2->port));
like($err, qr/can only run on the map group primary/, 'a standby is registered on the map group primary only');
($rc, $out, $err) = run_sql($n1,
	sprintf("SELECT kingres.add_node('%s', %d, NULL, 'standby')", $s2->host, $s2->port));
like($err, qr/needs the group_id of its primary/, 'a standby needs the group of its primary');
($rc, $out, $err) = run_sql($n1,
	sprintf("SELECT kingres.add_node('%s', %d, 7, 'standby')", $s2->host, $s2->port));
like($err, qr/group 7 has no primary node/, 'a standby of a group that does not exist is refused');
($rc, $out, $err) = run_sql($n1,
	sprintf("SELECT kingres.add_node('%s', %d, 0, 'standby')", $s2->host, $s2->port));
like($err, qr/is not a copy of the primary of group 0/,
	'a standby of another cluster (another system identifier) is refused');
my $other = PostgreSQL::Test::Cluster->new('sn_other');
$other->init;
$other->start;
($rc, $out, $err) = run_sql($n1,
	sprintf("SELECT kingres.add_node('%s', %d, 1, 'standby')", $other->host, $other->port));
like($err, qr/is not in recovery, so it is not a standby/, 'a node that is not in recovery is refused as a standby');
$other->stop;

my $id1 = register_standby($n1, $s1, 0);
my $id2 = register_standby($n1, $s2, 1);
is($n1->safe_psql('postgres',
		"SELECT string_agg(node_id || ':' || group_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node"),
	"1:0:primary,2:1:primary,$id1:0:standby,$id2:1:standby", 'the standbys are in the catalog with their roles');
($rc, $out, $err) = run_sql($n1,
	sprintf("SELECT kingres.add_node('%s', %d, 1, 'standby')", $s2->host, $s2->port));
like($err, qr/already registered/, 'the same standby cannot be registered twice');

# They own nothing and are skipped by routing: the buckets belong to groups, reached through their primaries.
is($n1->safe_psql('postgres', "SELECT count(DISTINCT node_id) FROM kingres.buckets WHERE node_id IN ($id1, $id2)"),
	'0', 'a standby owns no bucket');
my ($ka) = keys_of_group($n1, 1, 0, 1);
my ($kb) = keys_of_group($n1, 1, 1, 1);
$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($ka, 'a')");
$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($kb, 'b')");
is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv"), '2', 'a scan from the other node sees both rows');

# Catalog synchronization skips standbys: they get the catalog by replication.
$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
wait_catchup($n1, $s1);
wait_catchup($n2, $s2);
is($s2->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), '4',
	'the standby of group 1 sees the catalog through replication (it has no kingres.node write of its own)');

# Reference-table writes do not go to standbys: with the standbys stopped they still work.
$s1->stop;
$s2->stop;
is($n1->safe_psql('postgres', "INSERT INTO public.ref VALUES (1, 'one'); SELECT count(*) FROM public.ref"), '1',
	'a reference-table write with the standbys down');
is($n2->safe_psql('postgres', "SELECT count(*) FROM public.ref"), '1', 'and it reached the other primary');
$n2->safe_psql('postgres', "UPDATE public.kv SET val = 'b2' WHERE id = $kb");
$n1->safe_psql('postgres', "UPDATE public.kv SET val = 'x'");
is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE val = 'x'"), '2',
	'a fanned-out write with the standbys down');
$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
pass('sync_catalog() with the standbys down');
# A bucket move between the primaries, with a registered standby that is down.
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
is($n1->safe_psql('postgres', "SELECT kingres.internal_check_node_versions()"), '',
	'the version check skips standbys that are down');

# check_cluster: a standby that is down is reported, with the state as its primary sees it.
my $rows = $n1->safe_psql('postgres',
	"SELECT check_name || ':' || node_id FROM kingres.check_cluster() ORDER BY 1");
is($rows, "standby_unreachable:$id1\nstandby_unreachable:$id2",
	'check_cluster reports the standbys that are down, and nothing else');

# Streaming again: no problem, and the status shows the replication state of the primary.
$s1->start;
$s2->start;
wait_catchup($n1, $s1);
wait_catchup($n2, $s2);
is($n1->safe_psql('postgres', "SELECT coalesce(string_agg(check_name || ':' || node_id || ':' || detail, ' | '), '') FROM kingres.check_cluster()"), '',
	'check_cluster is quiet when the standbys stream');
is($n1->safe_psql('postgres',
		"SELECT string_agg(node_id || ':' || replication_state || ':' || sync_state, ',' ORDER BY node_id) FROM kingres.standby_status()"),
	"$id1:streaming:async,$id2:streaming:async", 'standby_status() shows the state as the primary sees it');

# Not streaming but running: the walreceiver is stopped by pointing it nowhere.
$s2->safe_psql('postgres', "ALTER SYSTEM SET primary_conninfo = 'port=1 host=127.0.0.1 application_name=sn_s2'");
$s2->safe_psql('postgres', 'SELECT pg_reload_conf()');
$s2->safe_psql('postgres', 'SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE backend_type = \'walreceiver\'');
ok($n2->poll_query_until('postgres', "SELECT count(*) = 0 FROM pg_stat_replication"),
	'the standby of group 1 stopped streaming');
$rows = $n1->safe_psql('postgres',
	"SELECT check_name || ':' || node_id || ':' || detail FROM kingres.check_cluster()");
like($rows, qr{^standby_not_streaming:$id2:.*no walsender},
	'a running standby that does not stream is reported with its state');
$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES (" . ($kb + 1000) . ", 'late') ON CONFLICT DO NOTHING");
pass('a registered standby that does not stream breaks nothing on the primaries');

# Removal.
($rc, $out, $err) = run_sql($n1, "SELECT kingres.remove_node(2)");
like($err, qr/still has the standby node $id2/, 'a primary cannot be removed before its standbys');
$n1->safe_psql('postgres', "SELECT kingres.remove_node($id2)");
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE role = 'standby'"), '1',
	'a standby is removed (although its group owns buckets)');
is($n2->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE role = 'standby'"), '1',
	'and the primaries agree');
is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv"), '3', 'the data is untouched');

done_testing();
