# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/064_promote_resume.pl
#
# Review S7-05: a promote_node that fails at any step leaves a cluster that a
# repeated promote_node, or recovery, brings to a consistent state.  The map
# group primary (node 1, the origin of the catalog change) is killed at every
# fault point of the function and of the commit protocol; node 4 is the
# standby of group 1 that is being promoted.
#
#   before the decision (up to and including after_log): the catalog change is rolled
#   back everywhere, nothing is left prepared, and promote_node done again
#   promotes ("promoted");
#
#   after the decision (after_commit, between_commit_prepared): the promoted node
#   holds the catalog change prepared and does not yet know itself by its new id;
#   recovery of the origin, or promote_node done again ("completed"), commits it.
#   Before the fix every session to node 4 was refused ("cannot prove it").

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql make_standby
  register_standby wait_catchup promote_standby);

my $seq = 0;

sub roles
{
	my ($node) = @_;
	return $node->safe_psql('postgres',
		"SELECT string_agg(node_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node");
}

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

sub setup
{
	my $prefix = 'pr' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, "${prefix}_s2");
	my $id = register_standby($n1, $s2, 1);
	wait_catchup($n2, $s2);
	$n2->stop('immediate');
	promote_standby($s2);
	return ($prefix, $n1, $n3, $s2, $id);
}

# Kill node 1 at a fault point of promote_node, restart it.
sub crash_at
{
	my ($n1, $id, $point) = @_;
	my ($rc, $out, $err) = run_sql($n1,
		"SET kingres_shard.fault_injection = '$point:kill'; SELECT kingres.promote_node($id)");
	isnt($rc, 0, "[$point] node 1 dies at the point");
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
}

my $old_roles = "1:primary,2:primary,3:primary,4:standby";
my $new_roles = "1:primary,2:standby,3:primary,4:primary";

# ---------------------------------------------------------------------------
# before the decision
# ---------------------------------------------------------------------------
{
	my ($prefix, $n1, $n3, $s2, $id) = setup();
	for my $point (qw(promote_after_recover promote_after_sessions promote_before_change
		promote_after_local_change promote_after_first_push promote_after_push
		before_prepare after_first_prepare after_prepare after_log))
	{
		crash_at($n1, $id, $point);
		$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
		is(roles($n1), $old_roles, "[$point] the catalog change was rolled back on the origin");
		is(prepared($n3) . prepared($s2) . prepared($n1), '000', "[$point] nothing is left prepared");
		is(roles($n3), $old_roles, "[$point] and the other nodes did not change");
	}
	my $res = $n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	like($res, qr/"status": "promoted"/, 'a repeated promote_node promotes');
	is(roles($n1), $new_roles, 'node 4 is the primary of group 1');
	is(roles($n3), $new_roles, 'on every node');
	is($s2->safe_psql('postgres', 'SELECT node_id FROM kingres.local_node'), '4', 'and knows itself as node 4');
	is($n1->safe_psql('postgres', "SELECT kingres.promote_node($id)"), '{"term": 1, "final": true, "status": "already", "unreached": [2], "blocked_by": []}',
		'a further call finds nothing to do');
}

# ---------------------------------------------------------------------------
# after the decision
# ---------------------------------------------------------------------------
for my $point (qw(after_commit between_commit_prepared))
{
	my ($prefix, $n1, $n3, $s2, $id) = setup();
	crash_at($n1, $id, $point);
	is(roles($n1), $new_roles, "[$point] the origin committed the catalog change");
	my $left = prepared($n3) + prepared($s2);
	cmp_ok($left, '>=', 1, "[$point] a participant still holds it prepared ($left)");
	if ($point eq 'after_commit')
	{
		# Before the fix every session to node 4 was refused: nothing could finish it.
		my $res = $n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
		like($res, qr/"status": "completed"/, "[$point] promote_node finishes the promotion and says so");
		like($res, qr/"unreached": \[2\]/, "[$point] and still lists the old primary, which was never told (S8-05)");
	}
	else
	{
		is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()') >= 1 ? 'ok' : 'none', 'ok',
			"[$point] recovery of the origin commits what is prepared, on node 4 as well");
		is($n1->safe_psql('postgres', "SELECT kingres.promote_node($id)"), '{"term": 1, "final": true, "status": "already", "unreached": [2], "blocked_by": []}',
			"[$point] and promote_node finds the promotion complete");
	}
	is(prepared($n3) . prepared($s2), '00', "[$point] nothing is left prepared");
	is(roles($s2), $new_roles, "[$point] node 4 holds the new catalog");
	is($s2->safe_psql('postgres', 'SELECT node_id FROM kingres.local_node'), '4', "[$point] and knows itself as node 4");
	is(roles($n3), $new_roles, "[$point] so does node 3");
	$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
	my ($k1) = keys_of_group($n1, 1, 1, 1);
	my ($k0) = keys_of_group($n1, 1, 0, 1);
	$n3->safe_psql('postgres', "BEGIN; INSERT INTO public.kv VALUES ($k1, 'x'); INSERT INTO public.kv VALUES ($k0, 'y'); COMMIT");
	is($s2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k1"), '1',
		"[$point] a distributed write from another node reaches the promoted node");
}

done_testing();
