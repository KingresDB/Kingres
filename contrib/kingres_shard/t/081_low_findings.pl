# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/081_low_findings.pl
#
# Review S9-08, S9-09 and S9-10.
#
# S9-08  A catalog change in the wrong order (remove_node before the name of the standby
#        is taken out of the primary's list) is refused at once, with a named answer and
#        before anything changes, instead of after 2 x node_timeout + 5 seconds with the
#        error of a failed prepare.
# S9-09  internal_sync_confirm does not accept an unproved node-session claim on a node
#        that is not part of a cluster.
# S9-10  The operator's way out of a map promotion that the rule of the majority refuses
#        for ever: a function that needs the exact acknowledgement.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster make_standby register_standby wait_catchup
  set_sync promote_standby run_sql);

# --- S9-09 -----------------------------------------------------------------------------
{
	my $n = make_node('lf_alone');
	$n->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
	$n->safe_psql('postgres', 'CREATE ROLE bob LOGIN');
	my ($rc, $out, $err);
	{
		local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on';
		($rc, $out, $err) = run_sql($n, 'SELECT kingres.internal_sync_confirm(0)', extra_params => [ '-U', 'bob' ]);
	}
	isnt($rc, 0, '[S9-09] a plain role that claims to be a node session is refused on a node outside a cluster');
	like($err, qr/permission denied for function internal_sync_confirm/, '[S9-09] with the permission error');
	($rc, $out, $err) = run_sql($n, 'SELECT kingres.internal_sync_confirm(0)');
	is($out, 't', '[S9-09] a superuser still gets the answer');
	$n->stop;
}

# --- S9-08 -----------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('lf8', 3, replication => 1, extra => "kingres_shard.node_timeout = 20\n");
	my $s2 = make_standby($n2, 'lf8_s2');
	my $id = register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	wait_catchup($n2, $s2);
	$s2->stop;
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.remove_node($id)");
	isnt($rc, 0, '[S9-08] remove_node before the list is cleared is refused');
	like($err, qr/node 2 cannot commit: its synchronous standbys do not confirm/,
		'[S9-08] with the named answer, and not after the wait of a failed prepare');
	unlike($err, qr/did not confirm that its part/, '[S9-08] which is not the error of a prepare');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE node_id = $id"), '1', '[S9-08] nothing was changed');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts') . $n2->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'),
		'00', '[S9-08] and nothing was left prepared');
	set_sync($n2);
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.remove_node($id)");
	is($rc, 0, '[S9-08] in the right order it works') or diag($err);
}

# --- S9-10 -----------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('lf10', 2, replication => 1);
	my $sm = make_standby($n1, 'lf10_sm');
	my $id = register_standby($n1, $sm, 0);
	wait_catchup($n1, $sm);
	$n1->stop('immediate');
	$n2->stop('immediate');
	promote_standby($sm);
	my ($rc, $out, $err) = run_sql($sm, "SELECT kingres.promote_node($id)");
	like($err, qr/only 0 of the 1 other shard groups can be reached/, '[S9-10] the rule refuses: the other group will not come back');
	($rc, $out, $err) = run_sql($sm, "SELECT kingres.promote_map_group_without_majority($id, 'yes')");
	isnt($rc, 0, '[S9-10] the override refuses without the exact acknowledgement');
	like($err, qr/acknowledgement is not the required text/, '[S9-10] and says so');
	($rc, $out, $err) = run_sql($sm, "SELECT kingres.promote_map_group_without_majority($id, "
		  . "'I have fenced every other node of the cluster and accept that the cluster can split')");
	is($rc, 0, '[S9-10] with the acknowledgement it promotes') or diag($err);
	like($out, qr/"term": 100[0-9]/, '[S9-10] and the term jumps by 1000');
	like($err, qr/without the rule of the majority/, '[S9-10] with a warning that says what was skipped');
	is($sm->safe_psql('postgres', 'SELECT term >= 1000 AND NOT superseded FROM kingres.catalog_status'), 't', '[S9-10] the node is the map primary');
	($rc, $out, $err) = run_sql($sm, "SELECT kingres.promote_map_group_without_majority(2, "
		  . "'I have fenced every other node of the cluster and accept that the cluster can split')");
	like($err, qr/for the map group only/, '[S9-10] it is for the map group only');
}

done_testing();
