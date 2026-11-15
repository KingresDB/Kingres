# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/049_promote_node.pl
#
# kingres.promote_node() (design/ha-phase5.md, part C, item 2): the catalog
# change that makes a promoted standby the primary of its group.
#   A. an ordinary group whose primary is dead;
#   B. an ordinary group whose primary is alive (a switchover): it is demoted,
#      refuses forwarded statements with the retryable KS001 and sends the
#      statements of its clients to the new primary; a scan that was planned
#      against the old primary fails with KS001 and succeeds when repeated;
#   C. the map group: the function runs on the promoted node, the catalogs of
#      the other nodes still name the old map primary;
#   D. the refusals.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql make_standby
  register_standby wait_catchup set_sync promote_standby wait_for_fault_file
  start_psql finish_psql);

sub catalog_roles
{
	my ($node) = @_;
	return $node->safe_psql('postgres',
		"SELECT string_agg(node_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node");
}

# ---------------------------------------------------------------------------
# A. the primary of group 1 is dead
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('pa', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, 'pa_s2');
	my $id = register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	my @k = map { [ keys_of_group($n1, 1, $_, 2) ] } (0, 1, 2);
	for my $g (0 .. 2) { $n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'g$g')") for @{ $k[$g] }; }
	wait_catchup($n2, $s2);
	my $version = $n1->safe_psql('postgres', 'SELECT version FROM kingres.map_version');

	$n2->stop('immediate');
	promote_standby($s2);

	# Refusals first.
	my ($rc, $out, $err) = run_sql($n3, "SELECT kingres.promote_node($id)");
	like($err, qr/can only run on the map group primary/, 'promote_node runs on the map group primary');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node(99)");
	like($err, qr/node 99 does not exist/, 'an unknown node is refused');
	$n1->safe_psql('postgres', "CREATE ROLE pa_user LOGIN");
	($rc, $out, $err) = run_sql($n1, "SET ROLE pa_user; SELECT kingres.promote_node($id)");
	like($err, qr/permission denied/, 'an ordinary role cannot call promote_node');
	($rc, $out, $err) = run_sql($n1, "SET ROLE pa_user; SELECT kingres.create_restore_point('x')");
	like($err, qr/permission denied/, 'an ordinary role cannot create a restore point');

	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	is(catalog_roles($n1), "1:primary,2:standby,3:primary,$id:primary", 'the roles are swapped on the map group primary');
	is(catalog_roles($n3), "1:primary,2:standby,3:primary,$id:primary", 'and on another primary');
	is(catalog_roles($s2), "1:primary,2:standby,3:primary,$id:primary", 'and on the promoted node');
	is($s2->safe_psql('postgres', 'SELECT node_id FROM kingres.local_node'), $id, 'the promoted node knows it is node ' . $id);
	cmp_ok($n1->safe_psql('postgres', 'SELECT version FROM kingres.map_version'), '>', $version, 'the map version is raised');
	is($n1->safe_psql('postgres', 'SELECT version FROM kingres.map_version'),
		$s2->safe_psql('postgres', 'SELECT version FROM kingres.map_version'), 'on every node alike');

	# Routing from every node goes to the new primary.
	for my $n ($n1, $n3, $s2)
	{
		is($n->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), '6', 'a scan from ' . $n->name . ' reads every group');
	}
	$n3->safe_psql('postgres', "UPDATE public.kv SET val = 'new' WHERE id = $k[1][0]");
	is($s2->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $k[1][0]"), 'new', 'a routed write lands on the new primary');
	# A distributed transaction that writes in groups 0 and 1.
	$n1->safe_psql('postgres', "BEGIN; INSERT INTO public.kv VALUES (" . ($k[0][0] + 100000) . ", 'tx'); UPDATE public.kv SET val = 'tx' WHERE id = $k[1][1]; COMMIT");
	is($s2->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $k[1][1]"), 'tx', 'a two-node transaction commits with the new primary');
	is(prepared($n1) . prepared($s2), '00', 'nothing is left prepared');
	ok($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.dtx_log WHERE gid LIKE 'kingres\\_h0\\_1\\_%'") >= 1,
		'the transaction identifier names the origin group and the timeline');
	is($n1->safe_psql('postgres', 'SELECT check_name || \':\' || node_id FROM kingres.check_cluster()'), 'standby_unreachable:2',
		'the cluster check reports only the old primary, which is down and now a standby');

	# A repeated call changes nothing.
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($id)");
	like($err, qr/is already the primary of group 1/, 'promote_node is idempotent (a notice)');
}

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

# ---------------------------------------------------------------------------
# B. a switchover: the old primary is alive
# ---------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('pb', 2, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 8);
	});
	my $s2 = make_standby($n2, 'pb_s2');
	my $id = register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	my @k0 = keys_of_group($n1, 1, 0, 2);
	my @k1 = keys_of_group($n1, 1, 1, 3);
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'x')") for (@k0, @k1);
	wait_catchup($n2, $s2);

	# A scan is planned against the old primary and stops before it asks any node.
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_pb_$$";
	append_to_file($gate, 'x');
	my $scan = start_psql($n1,
		"SET kingres_shard.fault_injection = 'scan_started:wait:$gate'; SELECT count(*) FROM public.kv;");
	ok(wait_for_fault_file("$gate.reached", 30), 'the scan is waiting inside its start');

	# The switchover: the standby has everything (synchronous), the old primary stops accepting
	# writes (here: it is left alive and demoted by the catalog), the standby is promoted.
	$n2->safe_psql('postgres', 'CHECKPOINT');
	promote_standby($s2);
	# The control plane takes the old primary out of synchronous replication (or fences it).
	set_sync($n2);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	is(catalog_roles($n2), "1:primary,2:standby,$id:primary", 'the old primary got the new catalog');

	unlink $gate;
	my ($sout, $serr) = finish_psql($scan);
	like($serr, qr/bucket map changed/, 'the scan that was planned against the old primary failed (KS001, retryable)');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), scalar(@k0) + scalar(@k1),
		'repeated against the new primary it answers completely');

	# The client of the old primary: its statements go to the new one.
	my $kb = (keys_of_group($n1, 1, 1, 1, 300000))[0];
	$n2->safe_psql('postgres', "INSERT INTO public.kv VALUES ($kb, 'via-old')");
	is($s2->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $kb"), 'via-old',
		'a client of the old primary: its write is routed to the new primary');
	is($n2->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.kv WHERE id = $kb"), '0',
		'and nothing was written on the old primary');
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv"), scalar(@k0) + scalar(@k1) + 1,
		'a scan from the old primary reads the new primary, not its own copy');

	# A forwarded session on the old primary (a node with a stale catalog) is refused with KS001.
	my $tok = $n2->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, 2)");
	{
		local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
		my ($rc, $o, $e) = run_sql($n2, "SELECT count(*) FROM public.kv WHERE id = $k1[0]");
		like($e, qr/no longer the primary of its shard group|not on this node|cannot prove/, 'a forwarded statement on the demoted node fails (KS001: stale map)');
		($rc, $o, $e) = run_sql($n2, "SELECT count(*) FROM public.kv WHERE id = $k1[0]");
		is($rc, 3, 'the SQLSTATE class of the failure is the retryable one')
		  if $e =~ /not on this node/;
		# Every kind of fragment is refused, the scan fragment of a distributed scan too.
		($rc, $o, $e) = run_sql($n2, "/* kingres:scan-fragment */ SELECT count(*) FROM public.kv");
		like($e, qr/no longer the primary of its shard group/, 'a scan fragment on the demoted node is refused with KS001');
		($rc, $o, $e) = run_sql($n2, "/* kingres:write-fragment */ UPDATE public.kv SET val = 'x'");
		like($e, qr/no longer the primary of its shard group/, 'and so is a write fragment');
	}
}

# ---------------------------------------------------------------------------
# B2. a fanned-out write that meets a switchover, and one that meets a failover
# ---------------------------------------------------------------------------
for my $alive (1, 0)
{
	my ($n1, $n2) = make_cluster("pd$alive", 2, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 8);
	});
	my $s2 = make_standby($n2, "pd${alive}_s2");
	my $id = register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	my @k = (keys_of_group($n1, 1, 0, 2), keys_of_group($n1, 1, 1, 2));
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'old')") for @k;
	wait_catchup($n2, $s2);
	# A long-lived session of node 1 that has a cached session to node 2.
	my $sess = $alive ? $n1->background_psql('postgres') : undef;
	if ($alive)
	{
		$sess->query_safe('SELECT count(*) FROM public.kv');
		cmp_ok($n2->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'kingres_shard'"), '>=', 1,
			'a session of node 1 holds a cached session to the old primary');
	}
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_pd$alive";
	append_to_file($gate, 'x');
	# UPDATE of every row: it runs on every group (a fan-out), in one two-phase transaction.
	my $upd = start_psql($n1,
		"SET kingres_shard.fault_injection = 'fanout_after_run:wait:$gate'; UPDATE public.kv SET val = 'fanned'");
	ok(wait_for_fault_file("$gate.reached", 30), "fan-out (old primary " . ($alive ? 'alive' : 'dead') . "): the write has run and waits before it commits");
	if ($alive) { $n2->safe_psql('postgres', 'CHECKPOINT'); } else { $n2->stop('immediate'); }
	promote_standby($s2);
	set_sync($n2) if $alive;
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	unlink $gate;
	my ($uo, $ue) = finish_psql($upd);
	if ($alive)
	{
		$sess->query_safe('SELECT count(*) FROM public.kv');
		is($n2->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'kingres_shard'"), '0',
			'the cached sessions to the old primary were dropped (on every node: the statement that follows the catalog change closes them)');
		$sess->quit;
	}
	like($ue, $alive ? qr/bucket map changed/ : qr/ERROR/,
		'fan-out: the write that had run against the old primary fails' . ($alive ? ' with KS001 (stale map)' : ''));
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE val = 'old'"), scalar(@k), 'fan-out: and changed nothing, on any group');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM pg_prepared_xacts"), '0', 'fan-out: nothing is left prepared on the map group');
	$n1->safe_psql('postgres', "UPDATE public.kv SET val = 'fanned'");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE val = 'fanned'"), scalar(@k), 'fan-out: repeated, it updates every row once');
	is($s2->safe_psql('postgres', "SELECT count(*) FROM pg_prepared_xacts"), '0', 'fan-out: and leaves nothing prepared on the new primary');
}

# ---------------------------------------------------------------------------
# C. the map group
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('pc', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s1 = make_standby($n1, 'pc_s1');
	my $id = register_standby($n1, $s1, 0);
	set_sync($n1, $s1);
	my @k = map { [ keys_of_group($n1, 1, $_, 2) ] } (0, 1, 2);
	for my $g (0 .. 2) { $n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'g$g')") for @{ $k[$g] }; }
	wait_catchup($n1, $s1);
	$n1->stop('immediate');
	promote_standby($s1);

	# Not on a node that is not the promoted standby.
	my ($rc, $out, $err) = run_sql($n2, "SELECT kingres.promote_node($id)");
	like($err, qr/not a copy of the primary of group 0|listens on port|in recovery/,
		'the map group case is refused on a node that is not the promoted standby');
	# Before the call the other nodes name the dead map primary.
	is(catalog_roles($n2), "1:primary,2:primary,3:primary,$id:standby", 'the other nodes still name the old map primary');
	($rc, $out, $err) = run_sql($n2, "SELECT count(*) FROM public.kv");
	isnt($rc, 0, 'a statement that needs the map group fails (not wrong) while it has no primary');

	$s1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	is(catalog_roles($s1), "1:standby,2:primary,3:primary,$id:primary", 'the promoted map standby is the map group primary');
	is(catalog_roles($n2), "1:standby,2:primary,3:primary,$id:primary", 'the other nodes learned it from its catalog');
	is(catalog_roles($n3), "1:standby,2:primary,3:primary,$id:primary", 'every other node');
	is($s1->safe_psql('postgres', 'SELECT node_id FROM kingres.local_node'), $id, 'its local node id is its own');
	is($n2->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), '6', 'a scan from another node reads the new map primary');

	# The new map primary writes the catalog and synchronizes it to the others.
	$s1->safe_psql('postgres', q{
		CREATE TABLE public.t2 (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.t2', 'id', bucket_count => 6);
	});
	is($n2->safe_psql('postgres', "SELECT count(*) FROM kingres.dist_table"), '2', 'a catalog change made on the new map primary reaches the others');
	$n3->safe_psql('postgres', "INSERT INTO public.t2 VALUES (1, 'x')");
	is($s1->safe_psql('postgres', "SELECT count(*) FROM public.t2"), '1', 'and statements on the new table work from any node');
	# Distributed transaction of the other nodes with the new map primary.
	$n2->safe_psql('postgres', "BEGIN; INSERT INTO public.kv VALUES (" . ($k[0][0] + 100000) . ", 'tx'); INSERT INTO public.kv VALUES (" . ($k[1][0] + 100000) . ", 'tx'); COMMIT");
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE val = 'tx'"), '2', 'a two-node transaction works');
	is(prepared($s1) . prepared($n2) . prepared($n3), '000', 'nothing is left prepared');
	$s1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($s1->safe_psql('postgres', 'SELECT check_name || \':\' || node_id FROM kingres.check_cluster()'), 'standby_unreachable:1',
		'the cluster check, run on the new map primary, reports only the old map primary, which is down and now a standby');
}

done_testing();
