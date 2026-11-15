# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/051_gid_formats.pl
#
# The identifier of a two-phase transaction changed in 0.5 (the origin group and
# the timeline instead of the origin node).  A prepared transaction that 0.4
# prepared and that is still pending when the node is upgraded has the old
# identifier; recovery resolves both formats, by the decision log of the origin,
# and leaves what is not ours alone.  The old format names a node: after a
# failover the node of the old primary is a standby row of the same group, and
# the promoted node resolves its transactions too.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql make_standby register_standby
  wait_catchup promote_standby);

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

# Prepare a transaction with the given gid on a node: it inserts one row.
sub prepare_row
{
	my ($node, $gid, $id) = @_;
	$node->safe_psql('postgres',
		"SET kingres_shard.enable_routing = off; BEGIN; INSERT INTO public.kv VALUES ($id, '$gid'); PREPARE TRANSACTION '$gid'");
}

# ---------------------------------------------------------------------------
# Both formats on a healthy cluster
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('gf', 3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 6);
	});
	my @k = keys_of_group($n1, 1, 1, 8);
	# A transaction number that no backend has: it is over, whatever format the gid has.
	my $x = '4000000000';
	# Old format (kingres_<origin node>_<pid>_<xid>_<target node>), decided: committed.
	prepare_row($n2, "kingres_1_999_${x}1_2", $k[0]);
	$n1->safe_psql('postgres', "INSERT INTO kingres.dtx_log VALUES ('kingres_1_999_${x}1_2', 2, now())");
	# Old format, undecided: rolled back.
	prepare_row($n2, "kingres_1_999_${x}2_2", $k[1]);
	# New format (kingres_g<origin group>_<timeline>_<pid>_<xid>_<target node>), decided and undecided.
	prepare_row($n2, "kingres_g0_1_999_${x}3_2", $k[2]);
	$n1->safe_psql('postgres', "INSERT INTO kingres.dtx_log VALUES ('kingres_g0_1_999_${x}3_2', 2, now())");
	prepare_row($n2, "kingres_g0_1_999_${x}4_2", $k[3]);
	# Not ours: another origin, a user's name, and malformed names.
	prepare_row($n2, "kingres_2_999_${x}5_2", $k[4]);	# origin node 2, resolved by node 2 only
	prepare_row($n2, "kingres_g1_1_999_${x}6_2", $k[5]);	# origin group 1
	prepare_row($n2, "kingres_orders_42", $k[6]);
	prepare_row($n2, "kingres_g0_x_999_${x}7_2", $k[7]);
	is(prepared($n2), '8', 'eight transactions are prepared on node 2');

	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($n2->safe_psql('postgres', "SELECT string_agg(gid, ',' ORDER BY gid) FROM pg_prepared_xacts"),
		"kingres_2_999_${x}5_2,kingres_g0_x_999_${x}7_2,kingres_g1_1_999_${x}6_2,kingres_orders_42",
		'the decided and the undecided ones of group 0 were resolved in both formats; what is not group 0\'s is untouched');
	is($n2->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT string_agg(id::text, ',' ORDER BY id) FROM public.kv"),
		join(',', sort { $a <=> $b } ($k[0], $k[2])), 'old and new format: the decided ones were committed, the others rolled back');
	# The node that is the origin by the old format resolves its own.
	$n2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($n2->safe_psql('postgres', "SELECT string_agg(gid, ',' ORDER BY gid) FROM pg_prepared_xacts"),
		"kingres_g0_x_999_${x}7_2,kingres_orders_42",
		'node 2 resolved what its group originated, in both formats; the malformed name and the user\'s are never touched');
}

# ---------------------------------------------------------------------------
# After a failover
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('gg', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 6);
	});
	my $s2 = make_standby($n2, 'gg_s2');
	my $id = register_standby($n1, $s2, 1);
	my $x = '4000000000';
	my @j = keys_of_group($n1, 1, 2, 2);
	# Prepared on node 3 by node 2 with the 0.4 identifier, and decided by node 2.
	prepare_row($n3, "kingres_2_999_${x}1_3", $j[0]);
	$n2->safe_psql('postgres', "INSERT INTO kingres.dtx_log VALUES ('kingres_2_999_${x}1_3', 3, now())");
	prepare_row($n3, "kingres_2_999_${x}2_3", $j[1]);
	wait_catchup($n2, $s2);
	$n2->stop('immediate');
	promote_standby($s2);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	# Node 1 is not the origin: it leaves both alone.
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared($n3), '2', 'the map group does not resolve what group 1 started');
	$s2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared($n3), '0', 'the promoted node resolved the old-format transactions of the node it replaces');
	is($n3->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT string_agg(id::text, ',') FROM public.kv"),
		$j[0], 'the decided one committed, the other rolled back');
}

done_testing();
