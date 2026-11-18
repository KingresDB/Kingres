# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/095_standby_secret.pl
#
# Review S11-04: the first recovery pass that reached every primary dropped the previous cluster
# secret, although a standby that was down for the rotation still held the older one.  That
# standby, promoted afterwards, could not be reached by any node, and promote_node() answered
# 08006 "could not ask node N whether it is in recovery" for ever.  The invariant: a node that can
# still be promoted can always be reached, or the refusal says exactly why and what to do.
#
#   1. the review's steps: the standby is down for the rotation and for the recovery pass, the old
#      primary stops, the standby is started and promoted: promote_node() works;
#   2. two rotations in a row with the standby down for both: the second is refused (KS014) and
#      names the standby, and works once the standby has replayed the first;
#   3. a standby that missed the rotation and is not promoted: nothing breaks, the previous secret
#      is dropped by the first pass that finds the standby on the current secret;
#   4. a node that really holds a secret that nobody else has (the previous one was dropped by
#      hand): promote_node() refuses with KS014 and says what happened, and does not repeat as 08006.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster make_standby register_standby wait_catchup promote_standby run_sql prepared_count);

my $seq = 0;

sub setup
{
	my $prefix = 'ss' . (++$seq);
	my ($map, $g1, $g2) = make_cluster($prefix, 3, replication => 1);
	$_->append_conf('postgresql.conf', "kingres_shard.node_timeout = 5\nkingres_shard.node_connect_timeout = 3\n") for ($map, $g1, $g2);
	$_->reload for ($map, $g1, $g2);
	my $sb = make_standby($g1, "${prefix}_s");
	my $id = register_standby($map, $sb, 1);
	wait_catchup($g1, $sb);
	return ($prefix, $map, $g1, $g2, $sb, $id);
}

sub has_previous { return $_[0]->safe_psql('postgres', 'SELECT previous IS NOT NULL FROM kingres.cluster_secret'); }

# --- 1. the review's steps ---------------------------------------------------------------------
{
	my ($prefix, $map, $g1, $g2, $sb, $id) = setup();
	$sb->stop('fast');
	$map->safe_psql('postgres', 'SELECT kingres.rotate_secret()');
	my ($rc, $out, $err) = run_sql($map, 'SELECT kingres.recover_prepared_transactions()');
	is($rc, 0, "[$prefix] the recovery pass runs with the standby down ($err)");
	is(has_previous($map), 't', "[$prefix] and keeps the previous secret: the standby has not confirmed the current one");
	is(has_previous($g1), 't', "[$prefix] on the primary of the standby too");
	$g1->stop('immediate');
	$sb->start;
	promote_standby($sb);
	($rc, $out, $err) = run_sql($map, "\\set VERBOSITY verbose\nSELECT kingres.promote_node($id)");
	is($rc, 0, "[$prefix] promote_node of the standby that missed the rotation works ($err)");
	like($out, qr/promoted/, "[$prefix] and says so ($out)");
	($rc, $out, $err) = run_sql($map, "SELECT kingres.promote_node($id)");
	like($out, qr/already/, "[$prefix] repeated: already ($out)");
	$sb->stop('immediate');
	$g2->stop('immediate');
}

# --- 2. two rotations in a row, the standby down for both ---------------------------------------
{
	my ($prefix, $map, $g1, $g2, $sb, $id) = setup();
	$sb->stop('fast');
	$map->safe_psql('postgres', 'SELECT kingres.rotate_secret()');
	my ($rc, $out, $err) = run_sql($map, "\\set VERBOSITY verbose\nSELECT kingres.rotate_secret()");
	isnt($rc, 0, "[$prefix] the second rotation is refused while the standby has not confirmed the first");
	like($err, qr/KS014/, "[$prefix] with KS014 ($err)");
	like($err, qr/standby $id \(/, "[$prefix] naming the standby");
	$sb->start;
	wait_catchup($g1, $sb);
	($rc, $out, $err) = run_sql($map, 'SELECT kingres.recover_prepared_transactions()');
	is($rc, 0, "[$prefix] recovery runs once the standby is back ($err)");
	is(has_previous($map), 'f', "[$prefix] and drops the previous secret: the standby is on the current one");
	($rc, $out, $err) = run_sql($map, 'SELECT kingres.rotate_secret()');
	is($rc, 0, "[$prefix] the second rotation works now ($err)");
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', "[$prefix] check_cluster is empty");
}

# --- 3. a standby that missed the rotation and is not promoted ----------------------------------
{
	my ($prefix, $map, $g1, $g2, $sb, $id) = setup();
	$sb->stop('fast');
	$map->safe_psql('postgres', 'SELECT kingres.rotate_secret()');
	run_sql($map, 'SELECT kingres.recover_prepared_transactions()');
	is(has_previous($map), 't', "[$prefix] the previous secret is kept while the standby is down");
	$sb->start;
	wait_catchup($g1, $sb);
	my ($rc, $out, $err) = run_sql($map, 'SELECT kingres.recover_prepared_transactions()');
	is($rc, 0, "[$prefix] the next pass runs ($err)");
	is(has_previous($_), 'f', "[$prefix] and the previous secret is gone") for ($map, $g1, $g2);
	($rc, $out, $err) = run_sql($map, 'SELECT kingres.sync_catalog()');
	is($rc, 0, "[$prefix] sync_catalog works ($err)");
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', "[$prefix] check_cluster is empty");
	is(prepared_count($map) + prepared_count($g1) + prepared_count($g2), 0, "[$prefix] nothing is prepared");
}

# --- 4. a node that holds a secret that nobody has ------------------------------------------------
{
	my ($prefix, $map, $g1, $g2, $sb, $id) = setup();
	$sb->stop('fast');
	$map->safe_psql('postgres', 'SELECT kingres.rotate_secret()');
	# what the earlier code did: the previous secret is gone everywhere
	$_->safe_psql('postgres', 'SELECT kingres.internal_drop_previous_secret()') for ($map, $g2);
	$g1->stop('immediate');
	$sb->start;
	promote_standby($sb);
	my ($rc, $out, $err) = run_sql($map, "\\set VERBOSITY verbose\nSELECT kingres.promote_node($id)");
	isnt($rc, 0, "[$prefix] promote_node refuses the node that holds another secret");
	like($err, qr/KS014/, "[$prefix] with KS014, not 08006 ($err)");
	like($err, qr/hold cluster secrets that do not match, and either may be the one that is behind/, "[$prefix] and says what is wrong, naming both possibilities (S13-04)");
	like($err, qr/ask a third node of the cluster/, "[$prefix] and how to tell them apart before anything is rebuilt");
	like($err, qr/base backup|remove_node/, "[$prefix] and what to do");
	unlike($err, qr/08006/, "[$prefix] not the bare connection failure");
	$sb->stop('immediate');
	$g2->stop('immediate');
}

done_testing();
