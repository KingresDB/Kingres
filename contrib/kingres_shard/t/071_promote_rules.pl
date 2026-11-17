# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/071_promote_rules.pl
#
# Review S8-02: a promoted standby has the synchronous_standby_names of the primary it
# was copied from, which may name a standby that does not exist; promote_node used to
# hang, or to end with the catalog change deferred and the answer "promoted".  It
# refuses before it changes anything, and says what to set.
#
# Review S8-04: the map group is promoted only by a node that reaches the primary of
# a strict majority of the groups other than the map group.  Two sides of a partition
# cannot both promote it; an even electorate that splits in two halves promotes
# nowhere; a cluster of one group has no electorate.  The "partitions" are made by
# stopping the nodes of the other side: a node that cannot be reached is what the
# rule looks at.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql make_standby
  register_standby wait_catchup set_sync promote_standby);

sub roles
{
	my ($node) = @_;
	return $node->safe_psql('postgres',
		"SELECT string_agg(node_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node");
}

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

sub clear_sync
{
	my ($node) = @_;
	$node->append_conf('postgresql.conf', "synchronous_standby_names = ''\n");
	$node->reload;
}

# ---------------------------------------------------------------------------
# A. a promoted node of an ordinary group inherits a list that names nobody alive
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('pa', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, 'pa_s2');
	register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	wait_catchup($n2, $s2);
	# A second standby made now has the list that names the first.
	my $s2b = make_standby($n2, 'pa_s2b');
	my $idb = register_standby($n1, $s2b, 1);
	wait_catchup($n2, $s2b);
	$s2->stop;
	$n2->stop('immediate');
	promote_standby($s2b);
	my $old = roles($n1);
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($idb)");
	isnt($rc, 0, '[A] promote_node refuses a node that cannot commit');
	like($err, qr/cannot commit: its synchronous standbys do not confirm/, '[A] and says why');
	like($err, qr/synchronous_standby_names/, '[A] and what to change');
	is(roles($n1), $old, '[A] nothing was changed');
	is(prepared($n1) . prepared($n3) . prepared($s2b), '000', '[A] nothing is left prepared');
	clear_sync($s2b);
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($idb)");
	is($rc, 0, '[A] with the list fixed the call goes through');
	like($out, qr/"status": "promoted".*"unreached"/s, '[A] and answers promoted');
	like($out, qr/"final": false/, '[A] which is not final: the call is repeated until it answers already');
	is(prepared($n1) . prepared($n3) . prepared($s2b), '000', '[A] with everything committed on every node');
	is(roles($n3), roles($n1), '[A] node 3 has the same catalog');
	like($n1->safe_psql('postgres', "SELECT kingres.promote_node($idb)"), qr/"status": "already".*"unreached": \[2, 4\]/,
		'[A] a repeated call says already, and still lists the old primary that was never told (S8-05)');
}

# ---------------------------------------------------------------------------
# B. the same for the map group, which runs the call on the promoted node itself
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('pb', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s1 = make_standby($n1, 'pb_s1');
	register_standby($n1, $s1, 0);
	set_sync($n1, $s1);
	wait_catchup($n1, $s1);
	my $s1b = make_standby($n1, 'pb_s1b');
	my $idb = register_standby($n1, $s1b, 0);
	wait_catchup($n1, $s1b);
	$s1->stop;
	$n1->stop('immediate');
	promote_standby($s1b);
	my $old = roles($s1b);
	my ($rc, $out, $err) = run_sql($s1b, "\\set VERBOSITY verbose\nSELECT kingres.promote_node($idb)");
	isnt($rc, 0, '[B] the map group is not promoted by a node that cannot commit');
	like($err, qr/this node cannot commit: its synchronous standbys do not confirm/, '[B] and the message says so');
	like($err, qr/KS009/, "[B] SQLSTATE KS009");
	is(roles($s1b), $old, '[B] nothing was changed');
	is(prepared($n2) . prepared($n3) . prepared($s1b), '000', '[B] nothing is left prepared');
	clear_sync($s1b);
	($rc, $out, $err) = run_sql($s1b, "SELECT kingres.promote_node($idb)");
	is($rc, 0, '[B] with the list fixed it is');
	like($out, qr/"status": "promoted"/, '[B] promoted');
	is(prepared($n2) . prepared($n3) . prepared($s1b), '000', '[B] and committed everywhere');
}

# ---------------------------------------------------------------------------
# C. three groups: a majority of the two other groups is both of them
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('pc', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	# Two standbys of the map group: one on each side of the partition.
	my $sa = make_standby($n1, 'pc_sa');
	my $sb = make_standby($n1, 'pc_sb');
	my $ida = register_standby($n1, $sa, 0);
	my $idb = register_standby($n1, $sb, 0);
	wait_catchup($n1, $sa);
	wait_catchup($n1, $sb);
	$n1->stop('immediate');
	$n2->stop('immediate');
	$n3->stop('immediate');
	promote_standby($sa);
	promote_standby($sb);
	my $old = roles($sa);

	# Side A: the standby sa and node 2.  Side B: the standby sb and node 3.
	$n2->start;
	my ($rc, $out, $err) = run_sql($sa, "\\set VERBOSITY verbose\nSELECT kingres.promote_node($ida)");
	isnt($rc, 0, '[C] side A (itself and one of the two other groups) does not promote the map group');
	like($err, qr/1 of the 2 other shard groups/, '[C] one of two is half, which is not a majority');
	like($err, qr/KS012/, "[C] SQLSTATE KS012");
	$n2->stop('immediate');
	$n3->start;
	($rc, $out, $err) = run_sql($sb, "SELECT kingres.promote_node($idb)");
	isnt($rc, 0, '[C] nor does side B');
	is(roles($sa), $old, '[C] side A changed nothing');
	is(roles($sb), $old, '[C] and neither did side B');
	$n3->stop('immediate');
	($rc, $out, $err) = run_sql($sb, "SELECT kingres.promote_node($idb)");
	isnt($rc, 0, '[C] a node that reaches no other group does not');
	$n2->start;
	$n3->start;
	($rc, $out, $err) = run_sql($sa, "SELECT kingres.promote_node($ida)");
	is($rc, 0, '[C] healed, a node that reaches both other groups promotes the map group');
	like($out, qr/"unreached": \[1\]/, '[C] and lists only the old primary: the other promoted standby is told');
}

# ---------------------------------------------------------------------------
# D. four groups: two of the three others
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3, $n4) = make_cluster('pd', 4, replication => 1);
	my $s1 = make_standby($n1, 'pd_s1');
	my $id = register_standby($n1, $s1, 0);
	wait_catchup($n1, $s1);
	$n1->stop('immediate');
	$n2->stop('immediate');
	$n3->stop('immediate');
	$n4->stop('immediate');
	promote_standby($s1);
	for my $up ([ $n2 ], [ $n3 ], [ $n4 ])
	{
		$up->[0]->start;
		my ($rc, $out, $err) = run_sql($s1, "SELECT kingres.promote_node($id)");
		isnt($rc, 0, '[D] one of the three other groups is a minority');
		$up->[0]->stop('immediate');
	}
	$n2->start;
	$n3->start;
	my ($rc, $out, $err) = run_sql($s1, "SELECT kingres.promote_node($id)");
	is($rc, 0, '[D] two of the three are a majority');
	like($out, qr/"unreached": \[1, 4\]/, '[D] and the nodes that were not told are listed');
}

# ---------------------------------------------------------------------------
# E. one group and two groups
# ---------------------------------------------------------------------------
{
	my ($n1) = make_cluster('pe', 1, replication => 1);
	my $s1 = make_standby($n1, 'pe_s1');
	my $id = register_standby($n1, $s1, 0);
	wait_catchup($n1, $s1);
	$n1->stop('immediate');
	promote_standby($s1);
	my ($rc, $out, $err) = run_sql($s1, "SELECT kingres.promote_node($id)");
	is($rc, 0, '[E] a cluster of one group has no other group to ask: its standby is promoted');
	like($out, qr/"status": "promoted"/, '[E] and the answer says so');
}
{
	my ($n1, $n2) = make_cluster('pf', 2, replication => 1);
	my $s1 = make_standby($n1, 'pf_s1');
	my $id = register_standby($n1, $s1, 0);
	wait_catchup($n1, $s1);
	$n1->stop('immediate');
	$n2->stop('immediate');
	promote_standby($s1);
	my ($rc, $out, $err) = run_sql($s1, "SELECT kingres.promote_node($id)");
	isnt($rc, 0, '[F] in a cluster of two groups the other group must be reachable');
	$n2->start;
	($rc, $out, $err) = run_sql($s1, "SELECT kingres.promote_node($id)");
	is($rc, 0, '[F] and with it the standby is promoted');
}

# ---------------------------------------------------------------------------
# G. the refusals of the sequences: a standby still in recovery, a standby that is down
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('pg', 3, replication => 1);
	my $s2 = make_standby($n2, 'pg_s2');
	my $id = register_standby($n1, $s2, 1);
	wait_catchup($n2, $s2);
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($id)");
	isnt($rc, 0, '[G] a standby that has not been promoted in PostgreSQL is refused');
	like($err, qr/is still in recovery/, '[G] and the message says so');
	$s2->stop;
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($id)");
	isnt($rc, 0, '[G] a standby that is down is refused');
	like($err, qr/could not connect to node/, '[G] and the message says so');
	is(roles($n1), "1:primary,2:primary,3:primary,$id:standby", '[G] nothing was changed by either');
}

done_testing();
