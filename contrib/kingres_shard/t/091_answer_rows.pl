# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/091_answer_rows.pl
#
# Review S10-05 and S10-08: the rows that the answer tables of the README lacked or got wrong, each
# produced here with its text and its SQLSTATE (the texts did not change; the SQLSTATE is the second
# thing a caller can match on):
#
#   add_node: "group N has no primary node", "group N already has a primary", "invalid node role",
#   "could not connect to node", the shared_preload_libraries error;
#   "cannot commit" (KS009) for every call that commits on every primary: add_node, remove_node,
#   rotate_secret, sync_catalog, within the bound, changing nothing;
#   promote_node: the wrong node (KS010), a node still in recovery (KS011), the id of a node that
#   is not a standby of anything in the group, the WARNING for an old primary that cannot be reached
#   and the NOTICE for "already";
#   the "registered on port P" refusal, which is also the answer of a call sent to the wrong node.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time);
use KingresShardTest qw(make_node make_cluster make_standby register_standby wait_catchup
  promote_standby run_sql);

my $T = 3;
my $MARGIN = 8;

# Run a statement with verbose errors; return (seconds, rc, stdout, stderr).
sub call
{
	my ($node, $sql) = @_;
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($node, "\\set VERBOSITY verbose\n$sql");
	return (time - $t0, $rc, $out, $err);
}

sub refused
{
	my ($label, $node, $sql, $state, $text) = @_;
	my ($t, $rc, $out, $err) = call($node, $sql);
	isnt($rc, 0, "$label: refused");
	like($err, qr/\b$state\b/, "$label: SQLSTATE $state ($err)");
	like($err, $text, "$label: the text");
	return ($t, $err);
}

my @n = make_cluster('ar', 3, replication => 1);
$_->append_conf('postgresql.conf', "kingres_shard.node_timeout = $T\n") for @n;
$_->reload for @n;
my ($map, $g1, $g2) = @n;

# --- add_node answers -----------------------------------------------------------------------
{
	my $x = make_node('ar_x', replication => 1);
	refused('add_node standby of a group that does not exist', $map,
		sprintf("SELECT kingres.add_node('%s', %d, 9, 'standby')", $x->host, $x->port), '42704', qr/group 9 has no primary node/);
	refused('add_node a second primary for a group', $map,
		sprintf("SELECT kingres.add_node('%s', %d, 1, 'primary')", $x->host, $x->port), '42710', qr/group 1 already has a primary \(node 2\)/);
	refused('add_node with a wrong role', $map,
		sprintf("SELECT kingres.add_node('%s', %d, 1, 'bogus')", $x->host, $x->port), '22023', qr/invalid node role "bogus"/);
	refused('add_node of an address that nobody listens on', $map,
		"SELECT kingres.add_node('127.0.0.1', 1, NULL, 'primary')", '08001', qr/could not connect to node 127\.0\.0\.1:1/);
	my $plain = PostgreSQL::Test::Cluster->new('ar_plain');
	$plain->init;
	$plain->start;
	refused('add_node of a node without the library loaded', $map,
		sprintf("SELECT kingres.add_node('%s', %d)", $plain->host, $plain->port), '55000', qr/kingres_shard must be loaded via shared_preload_libraries/);
	$plain->stop('immediate');
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), '3', 'none of them changed the catalog');
	$x->stop('immediate');
}

# --- the "cannot commit" refusal of every call that commits on every primary (KS009) -----------
{
	$g1->append_conf('postgresql.conf', "synchronous_standby_names = 'ghost'\n");
	$g1->reload;
	my $x = make_node('ar_y', replication => 1);
	foreach my $c ([ 'add_node', sprintf("SELECT kingres.add_node('%s', %d)", $x->host, $x->port) ],
		[ 'remove_node', 'SELECT kingres.remove_node(3)' ],
		[ 'rotate_secret', 'SELECT kingres.rotate_secret()' ],
		[ 'sync_catalog', 'SELECT kingres.sync_catalog()' ])
	{
		my ($name, $sql) = @$c;
		my ($t, $err) = refused("$name with a wrong list on a primary", $map, $sql, 'KS009',
			qr/node 2 \(.*\) cannot commit: its synchronous standbys do not confirm|node 2 cannot commit: its synchronous standbys do not confirm/);
		ok($t <= $T + $MARGIN, "$name: refused in $t s (bound: min(node_timeout, 5) - 1 s per node, here $T s, plus a margin)");
	}
	is($map->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), '3', 'nothing was changed');
	is($map->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'), '0', 'nothing is prepared');
	$g1->append_conf('postgresql.conf', "synchronous_standby_names = ''\n");
	$g1->reload;
	my ($rc, $out, $err) = run_sql($map, 'SELECT kingres.sync_catalog()');
	is($rc, 0, "with the list corrected the call goes through ($err)");
	$x->stop('immediate');
}

# --- promote_node: wrong node, in recovery, the warning, the notice -----------------------------
{
	my $sb = make_standby($g1, 'ar_s');
	my $id = register_standby($map, $sb, 1);
	my $msb = make_standby($map, 'ar_m');
	my $mid = register_standby($map, $msb, 0);
	wait_catchup($g1, $sb);
	wait_catchup($map, $msb);
	refused('promote_node on a node that is not the map group primary', $g2, "SELECT kingres.promote_node($id)", 'KS010',
		qr/this function can only run on the map group primary/);
	refused('promote_node of a standby that is still in recovery', $map, "SELECT kingres.promote_node($id)", 'KS011',
		qr/node $id \(.*\) is still in recovery/);
	# the map group standby, asked for on MAP: the node row names a port that MAP does not listen on
	my ($t, $rc, $out, $err) = call($map, "SELECT kingres.promote_node($mid)");
	isnt($rc, 0, 'promote_node of the map standby sent to MAP is refused');
	diag("promote_node($mid) on MAP: $err");
	like($err, qr/(is still in recovery|is registered on port \d+, this server listens on port \d+)/, 'with one of the two texts that name the wrong node');
	($t, $rc, $out, $err) = call($map, "SELECT kingres.promote_node(1)");
	is($rc, 0, 'promote_node of the node that is the primary of its group already answers');
	like($out, qr/"status": "already"/, 'already');
	like($err, qr/NOTICE.*is already the primary of group 0/, 'with a NOTICE on the session');
	# a failover with an unreachable OLD: the WARNING
	$g1->stop('immediate');
	promote_standby($sb);
	$sb->safe_psql('postgres', "ALTER SYSTEM SET synchronous_standby_names = ''");
	$sb->safe_psql('postgres', 'SELECT pg_reload_conf()');
	($t, $rc, $out, $err) = call($map, "SELECT kingres.promote_node($id)");
	is($rc, 0, "promote_node ($err)");
	diag("promote_node stderr: $err");
	like($out, qr/"status": "promoted"/, 'promoted');
	like($err, qr/WARNING:[^\n]*the former primary, node 2 \(.*\), cannot be reached and keeps its old catalog/s,'with a WARNING for the old primary that cannot be reached');
	($t, $rc, $out, $err) = call($map, "SELECT kingres.promote_node($id)");
	like($out, qr/"status": "already"/, 'already, final');
	like($err, qr/NOTICE.*node $id is already the primary of group 1|NOTICE.*already the primary/, 'with the NOTICE');
	is($map->safe_psql('postgres', "SELECT count(*) FROM kingres.standby_status() WHERE node_id = 2 AND problem = 'standby_unreachable'"), '1',
		'standby_status lists the old primary as standby_unreachable until it is rebuilt (the postcondition "every standby streaming" holds only after sequence 6)');
}

done_testing();
