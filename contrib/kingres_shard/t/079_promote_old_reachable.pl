# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/079_promote_old_reachable.pl
#
# Reviews S9-05 and S9-06: the old primary (OLD) is reachable and is told of its own
# demotion.  It is a participant of the promotion's transaction, and its own standby
# is the node that was just promoted, which no longer streams from it; its list
# synchronous_standby_names (unchanged, the normal configuration) names NEW.  Until
# the fix the call waited for OLD's confirmation, failed with KS005 after
# 2 x node_timeout + 5 seconds on every call, and, once OLD's list was cleared,
# left two false participant_lost_committed_transaction incidents.
#
# What the extension does now: a participant that is being demoted is not waited for
# and not asked (it is about to stop being a primary; its confirmation is not what
# makes the promotion durable), its decision rows are marked so that nothing checks
# them for a lost part, and recovery commits what the call left on it without waiting
# for the standby it names.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster make_standby register_standby wait_catchup
  set_sync promote_standby run_sql);

my $seq = 0;

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

sub setup
{
	my $prefix = 'or' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, replication => 1,
		extra => "kingres_shard.node_timeout = 8\n");
	my $s2 = make_standby($n2, "${prefix}_s2");
	my $id2 = register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	wait_catchup($n2, $s2);
	return ($prefix, $n1, $n2, $n3, $s2, $id2);
}

# --- OLD is reachable and still names NEW in its list ------------------------------------
{
	my ($prefix, $n1, $n2, $n3, $s2, $id2) = setup();
	promote_standby($s2);
	set_sync($s2);
	# OLD is not stopped and its synchronous_standby_names is not touched.
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($id2)");
	is($rc, 0, "[$prefix] the call succeeds although OLD's own standby is the node it promoted") or diag($err);
	like($out, qr/"status": "promoted".*"unreached": \[\]/s, "[$prefix] and tells OLD (nothing is unreached)");
	unlike($err, qr/did not confirm/, "[$prefix] with no KS005 on the way");
	like($n1->safe_psql('postgres', "SELECT kingres.promote_node($id2)"), qr/"status": "(completed|already)"/,
		"[$prefix] a repeat finishes");
	like($n1->safe_psql('postgres', "SELECT kingres.promote_node($id2)"), qr/"status": "already"/, "[$prefix] and then it is final");
	is($n2->safe_psql('postgres', "SELECT string_agg(node_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node"),
		$n1->safe_psql('postgres', "SELECT string_agg(node_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node"),
		"[$prefix] OLD holds the new catalog: it is a standby in it");
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	$s2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared($n2) . prepared($s2) . prepared($n1) . prepared($n3), '0000', "[$prefix] nothing is left prepared anywhere");
	diag($n1->safe_psql('postgres', "SELECT gid || ' ' || node_id || ' ' || kind FROM kingres.dtx_incident") . ' | ' . $s2->safe_psql('postgres', "SELECT gid || ' ' || node_id || ' ' || kind FROM kingres.dtx_incident"));
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.dtx_incident") . $s2->safe_psql('postgres', "SELECT count(*) FROM kingres.dtx_incident"),
		'00', "[$prefix] no incident was recorded for the promotion's own transactions (S9-06)");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name LIKE 'participant_lost%' OR check_name LIKE 'prepared_rolled%'"),
		'0', "[$prefix] and check_cluster() shows no lost part");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.dtx_log WHERE node_id = 2 AND NOT durable"), '0',
		"[$prefix] and no decision of OLD waits for a confirmation that cannot come");
}

# --- the same after a repeat that runs recovery first -------------------------------------
{
	my ($prefix, $n1, $n2, $n3, $s2, $id2) = setup();
	promote_standby($s2);
	set_sync($s2);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id2)");
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id2)");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_incident'), '0', "[$prefix] a recovery pass between the calls records nothing");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'participant_lost_committed_transaction'"), '0',
		"[$prefix] and check_cluster() shows no participant_lost_committed_transaction");
}

done_testing();
