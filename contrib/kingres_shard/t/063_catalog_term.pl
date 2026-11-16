# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/063_catalog_term.pl
#
# Reviews S7-06 and S7-04: the catalog term and the promotion that does not need
# every node.
#
#   A. A map group primary that was not fenced and is restarted after the promotion
#      of its standby still holds the old catalog.  A catalog change that it makes is
#      refused by every other node (the term of its catalog is older), and it learns
#      that it has been superseded: it refuses catalog changes from then on.
#   B. Two promotions of the map group in a row: the term goes up each time, and the
#      two former primaries are both refused.
#   C. A primary of an ordinary group that cannot be reached when another group is
#      promoted does not stop the promotion, is reported, and fetches the new
#      catalog from the first node it talks to once it is back.
#   D. A map group promoted from a minority is refused; from a majority it goes
#      through although some nodes cannot be reached.
#   E. A plain client cannot make the claim that a node session makes.

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

sub status
{
	my ($node) = @_;
	return $node->safe_psql('postgres',
		"SELECT term || '/' || CASE WHEN superseded THEN 't' ELSE 'f' END FROM kingres.catalog_status");
}

# ---------------------------------------------------------------------------
# A. an old map primary comes back unfenced
# ---------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('ta', 2, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s1 = make_standby($n1, 'ta_s1');
	my $id = register_standby($n1, $s1, 0);
	wait_catchup($n1, $s1);
	is(status($n1), '0/f', '[A] the term starts at 0');
	$n1->stop('immediate');
	promote_standby($s1);
	my $res = $s1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	like($res, qr/"status": "promoted"/, '[A] promote_node says what it did');
	like($res, qr/"term": 1/, '[A] and the term it made');
	is(status($s1), '1/f', '[A] the new map primary is at term 1');
	is(status($n2), '1/f', '[A] and so is the other node');

	# The old map primary is restarted, not fenced.
	$n1->start;
	is(status($n1), '0/f', '[A] the old map primary holds the old catalog');
	my $roles_before = roles($n2);
	my ($rc, $out, $err) = run_sql($n1, q{
		CREATE TABLE public.kv2 (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv2', 'id');
	});
	isnt($rc, 0, '[A] a catalog change on the old map primary fails');
	like($err, qr/superseded|term/, '[A] and says why');
	is(roles($n2), $roles_before, '[A] the other node kept its catalog');
	is($n2->safe_psql('postgres', 'SELECT count(*) FROM kingres.dist_table'), '1', '[A] and has no table of the old map primary');
	is($s1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dist_table'), '1', '[A] nor has the new map primary');
	ok($n1->poll_query_until('postgres', 'SELECT superseded FROM kingres.catalog_status'),
		'[A] the old map primary records that it has been superseded');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.sync_catalog()");
	like($err, qr/superseded as the map group primary/, '[A] and refuses every catalog change from then on');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.add_node('127.0.0.1', 1)");
	like($err, qr/superseded as the map group primary/, '[A] add_node too');
	is(roles($n2), $roles_before, '[A] still nothing changed elsewhere');
	# The new map primary keeps working.
	$s1->safe_psql('postgres', q{
		CREATE TABLE public.kv3 (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv3', 'id');
	});
	is($n2->safe_psql('postgres', 'SELECT count(*) FROM kingres.dist_table'), '2', '[A] the new map primary changes the catalog');
}

# ---------------------------------------------------------------------------
# B. two promotions of the map group in a row
# ---------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('tb', 2, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s1 = make_standby($n1, 'tb_s1');
	my $id1 = register_standby($n1, $s1, 0);
	wait_catchup($n1, $s1);
	$n1->stop('immediate');
	promote_standby($s1);
	$s1->safe_psql('postgres', "SELECT kingres.promote_node($id1)");
	# A standby of the new map primary, made from it.
	my $s1b = make_standby($s1, 'tb_s1b');
	my $id2 = register_standby($s1, $s1b, 0);
	wait_catchup($s1, $s1b);
	$s1->stop('immediate');
	promote_standby($s1b);
	my $res = $s1b->safe_psql('postgres', "SELECT kingres.promote_node($id2)");
	like($res, qr/"term": 2/, '[B] the second promotion makes term 2');
	is(status($s1b), '2/f', '[B] the second map primary is at term 2');
	is(status($n2), '2/f', '[B] and so is the other node');
	is(roles($n2), "1:standby,2:primary,$id1:standby,$id2:primary", '[B] the catalog names the last map primary');
	# Both former primaries are refused when they come back.
	$s1->start;
	$n1->start;
	for my $old ([ 'the first', $n1 ], [ 'the second', $s1 ])
	{
		my ($what, $node) = @$old;
		my ($rc, $out, $err) = run_sql($node, q{
			CREATE TABLE public.kvx (id int PRIMARY KEY, val text);
			SELECT kingres.create_distributed_table('public.kvx', 'id');
		});
		isnt($rc, 0, "[B] $what former map primary cannot change the catalog");
	}
	is($n2->safe_psql('postgres', 'SELECT count(*) FROM kingres.dist_table'), '1', '[B] nothing reached the other node');
	is($s1b->safe_psql('postgres', 'SELECT count(*) FROM kingres.dist_table'), '1', '[B] nor the map primary');
}

# ---------------------------------------------------------------------------
# C. an ordinary primary that cannot be reached comes back after a promotion
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('tc', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, 'tc_s2');
	my $id = register_standby($n1, $s2, 1);
	wait_catchup($n2, $s2);
	my ($k0) = keys_of_group($n1, 1, 0, 1);
	my ($k1) = keys_of_group($n1, 1, 1, 1);
	my ($k2) = keys_of_group($n1, 1, 2, 1);
	# Group 1 loses its primary, and node 3 cannot be reached at that moment.
	$n2->stop('immediate');
	$n3->stop('immediate');
	promote_standby($s2);
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($id)");
	is($rc, 0, '[C] promote_node goes through although another primary is unreachable');
	like($out, qr/"unreached": \[2, 3\]/, '[C] and reports the nodes it could not tell');
	like($err, qr/node 3 .* cannot be reached/, '[C] with a warning');
	is(status($n1), '1/f', '[C] the term went up');
	$n3->start;
	is(status($n3), '0/f', '[C] the node that was away holds the old catalog');
	is(roles($n3), "1:primary,2:primary,3:primary,$id:standby", '[C] which still names the dead primary of group 1');
	# A statement of the old node reaches a node that knows the new term: refused, and the node learns.
	($rc, $out, $err) = run_sql($n3, "INSERT INTO public.kv VALUES ($k0, 'from3')");
	isnt($rc, 0, '[C] its first statement that needs another node is refused');
	like($err, qr/term/, '[C] with the term as the reason');
	ok($n3->poll_query_until('postgres', "SELECT term = 1 FROM kingres.catalog_status"),
		'[C] and the node takes the new catalog from the node that refused it');
	is(roles($n3), "1:primary,2:standby,3:primary,$id:primary", '[C] which names the promoted node');
	($rc, $out, $err) = run_sql($n3, "INSERT INTO public.kv VALUES ($k1, 'to_new_primary')");
	is($rc, 0, '[C] a statement for group 1 now goes to the new primary');
	is($s2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k1"), '1', '[C] and was written there');
	# sync_catalog repairs a node that was missed and did not learn by itself (nothing left to repair here).
	is($n1->safe_psql('postgres', "SELECT kingres.promote_node($id)"), '{"term": 1, "final": true, "status": "already", "unreached": [2]}',
		'[C] a repeated promote_node finds the promotion complete');
}

# ---------------------------------------------------------------------------
# D. the map group from a minority and from a majority
# ---------------------------------------------------------------------------
{
	# Four groups: the electorate of the map group is the three others (S8-04).
	my ($n1, $n2, $n3, $n4) = make_cluster('td', 4, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s1 = make_standby($n1, 'td_s1');
	my $id = register_standby($n1, $s1, 0);
	wait_catchup($n1, $s1);
	$n1->stop('immediate');
	$n2->stop('immediate');
	$n3->stop('immediate');
	$n4->stop('immediate');
	promote_standby($s1);
	my ($rc, $out, $err) = run_sql($s1, "SELECT kingres.promote_node($id)");
	isnt($rc, 0, '[D] a standby that reaches no other group does not become the map primary');
	like($err, qr/minority/, '[D] and says that it is a minority');
	is(roles($s1), "1:primary,2:primary,3:primary,4:primary,$id:standby", '[D] its catalog is unchanged');
	$n3->start;
	($rc, $out, $err) = run_sql($s1, "SELECT kingres.promote_node($id)");
	isnt($rc, 0, '[D] one of the three other groups is not a majority of them');
	$n4->start;
	($rc, $out, $err) = run_sql($s1, "SELECT kingres.promote_node($id)");
	is($rc, 0, '[D] two of the three other groups are');
	like($out, qr/"unreached": \[1, 2\]/, '[D] and the old map primary and node 2 are reported');
	is(roles($n3), "1:standby,2:primary,3:primary,4:primary,$id:primary", '[D] node 3 has the new catalog');
	is(status($n3), '1/f', '[D] at the new term');
	# Node 2 comes back with the old catalog; sync_catalog on the new map primary repairs it.
	$n2->start;
	is(status($n2), '0/f', '[D] the node that was away is behind');
	$s1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
	is(status($n2), '1/f', '[D] sync_catalog gives it the new catalog');
	is(roles($n2), "1:standby,2:primary,3:primary,4:primary,$id:primary", '[D] and the roles');
}

# ---------------------------------------------------------------------------
# F. two groups lose their primaries together: each standby is promoted although the other group is down
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('tf', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, 'tf_s2');
	my $s3 = make_standby($n3, 'tf_s3');
	my $id2 = register_standby($n1, $s2, 1);
	my $id3 = register_standby($n1, $s3, 2);
	wait_catchup($n2, $s2);
	wait_catchup($n3, $s3);
	$n2->stop('immediate');
	$n3->stop('immediate');
	promote_standby($s2);
	promote_standby($s3);
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($id2)");
	is($rc, 0, '[F] the first standby is promoted while the other group is down too');
	like($out, qr/"unreached": \[2, 3\]/, '[F] the nodes that were not told are listed');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.promote_node($id3)");
	is($rc, 0, '[F] and so is the second');
	like($out, qr/"term": 2/, '[F] each promotion raised the term');
	is(roles($n1), "1:primary,2:standby,3:standby,$id2:primary,$id3:primary", '[F] the catalog names both new primaries');
	is(roles($s3), "1:primary,2:standby,3:standby,$id2:primary,$id3:primary", '[F] the last promoted node has it too');
}

# ---------------------------------------------------------------------------
# E. a plain client cannot make the claim of a node session
# ---------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('te', 2);
	my ($rc, $out, $err) = run_sql($n2, "SET kingres_shard.forward_term = '99'");
	isnt($rc, 0, '[E] a session that is not a node session cannot set the term claim');
	($rc, $out, $err) = run_sql($n2, "SET kingres_shard.forward_origin = '1'");
	isnt($rc, 0, '[E] nor the origin');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.catalog_status WHERE NOT superseded"), '1', '[E] and nothing changed');
}

done_testing();
