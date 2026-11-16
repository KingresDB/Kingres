# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/082_sequence_answers.pl
#
# The answers and errors that the call sequences of the README (what the control plane
# calls) name and that no other test produces: registering a standby (a call on the wrong
# node, a standby of another group, a standby registered twice, the name that has to match
# before it goes into a list), the errors of promote_node for an unknown node, a NULL, and a
# call on a node that is not MAP, removing a standby that does not exist or whose group
# is a primary with standbys, and a client that cancels promote_node (S9 review, "A call
# that did not finish": the repeat settles it).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster make_standby register_standby wait_catchup
  set_sync promote_standby run_sql wait_for_fault_file start_psql finish_psql);

# --- 1. registering a standby ----------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('sa1', 3, replication => 1);
	my $s2 = make_standby($n2, 'sa1_s2');
	my ($rc, $out, $err) = run_sql($n2, sprintf("SELECT kingres.add_node('%s', %d, 1, 'standby')", $s2->host, $s2->port));
	like($err, qr/this function can only run on the map group primary/, '[1] add_node on a node that is not MAP');
	($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d, 2, 'standby')", $s2->host, $s2->port));
	like($err, qr/is not a copy of the primary of group 2/, '[1] a standby of another group');
	my $id = register_standby($n1, $s2, 1);
	like($id, qr/^\d+$/, '[1] the node id is an integer');
	($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d, 1, 'standby')", $s2->host, $s2->port));
	like($err, qr/is already registered as node $id/, '[1] the same standby twice');
	# Step 2: the name that goes into the list must be the application_name that the primary sees.
	wait_catchup($n2, $s2);
	is($n2->safe_psql('postgres', "SELECT application_name FROM pg_stat_replication WHERE state = 'streaming'"), $s2->name,
		'[1] the primary sees the standby under the name that goes into the list');
	# A typo in the list: the standby stays async, and the check says so.
	$n2->append_conf('postgresql.conf', "synchronous_standby_names = 'ANY 1 (" . $s2->name . "_typo)'\n");
	$n2->reload;
	ok($n1->poll_query_until('postgres', "SELECT sync_state = 'async' FROM kingres.standby_status() WHERE node_id = $id"),
		'[1] standby_status() shows async when the name matches nobody');
	set_sync($n2, $s2);
	ok($n1->poll_query_until('postgres', "SELECT sync_state IN ('quorum', 'sync') FROM kingres.standby_status() WHERE node_id = $id"),
		'[1] and quorum once the name is right');
	set_sync($n2);

	# remove_node's own answers (sequence 8)
	($rc, $out, $err) = run_sql($n1, 'SELECT kingres.remove_node(99)');
	like($err, qr/node 99 does not exist/, '[8] remove_node of a node that does not exist');
	($rc, $out, $err) = run_sql($n1, 'SELECT kingres.remove_node(2)');
	like($err, qr/cannot remove node 2: shard group 1 still has the standby node $id/, '[8] a primary that still has a standby');

	# promote_node's own answers (sequence 2)
	($rc, $out, $err) = run_sql($n1, 'SELECT kingres.promote_node(99)');
	like($err, qr/node 99 does not exist/, '[2] promote_node of a node that does not exist');
	($rc, $out, $err) = run_sql($n1, 'SELECT kingres.promote_node(NULL)');
	is($rc, 0, '[2] promote_node(NULL) is not an error');
	is($out, '', '[2] and returns NULL');
	($rc, $out, $err) = run_sql($n3, "SELECT kingres.promote_node($id)");
	like($err, qr/this function can only run on the map group primary/, '[2] promote_node on a node that is not MAP');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($id)");
	like($err, qr/node $id \([^)]*\) is still in recovery/, '[2] promote_node before pg_promote() has finished');
}

# --- 2. a client that cancels promote_node ------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('sa2', 3, replication => 1);
	my $s2 = make_standby($n2, 'sa2_s2');
	my $id = register_standby($n1, $s2, 1);
	wait_catchup($n2, $s2);
	$n2->stop('immediate');
	promote_standby($s2);
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_sa2";
	append_to_file($gate, 'x');
	my $call = start_psql($n1,
		"SET kingres_shard.fault_injection = 'promote_before_change:wait:$gate'; SELECT kingres.promote_node($id)");
	ok(wait_for_fault_file("$gate.reached", 60), '[9] the call waits before it changes anything');
	$n1->safe_psql('postgres', "SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE query LIKE '%promote_node%' AND pid <> pg_backend_pid()");
	my ($out, $err) = finish_psql($call);
	like($err, qr/canceling statement due to user request/, '[9] the client cancelled it');
	is($n1->safe_psql('postgres', "SELECT role FROM kingres.node WHERE node_id = $id"), 'standby', '[9] nothing was changed');
	is($n3->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts') . $s2->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'),
		'00', '[9] and nothing is left prepared');
	like($n1->safe_psql('postgres', "SELECT kingres.promote_node($id)"), qr/"status": "promoted"/, '[9] the repeat promotes');
	like($n1->safe_psql('postgres', "SELECT kingres.promote_node($id)"), qr/"status": "(already|completed)"/, '[9] and the next ends the loop');
}

done_testing();
