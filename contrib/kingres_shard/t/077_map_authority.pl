# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/077_map_authority.pl
#
# Review S9-02: no catalog change of any kind is accepted from a node that is not the
# map primary by a claim that a majority of the electorate supports.  A standby of the
# map group that was promoted in PostgreSQL holds the old map primary's catalog and its
# node id, so the catalog calls it the map primary; until the fix it accepted the
# promotion of an ordinary group without any electorate, and the group it had just
# promoted counted as "reached" for its own promotion of the map group.
#
# 1. the review's script: the map group and group 1 fail together; the promoted map
#    standby refuses to promote group 1, and the map promotion stays refused
# 2. every other function that changes the catalog refuses on such a node
# 3. a promotion that has the majority works, and the node is the map primary after it
# 4. "reached" means reached now: a node that answers but does not prove it is the
#    primary of its group does not count

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group make_standby register_standby
  wait_catchup promote_standby run_sql);

my $seq = 0;

sub setup
{
	my ($count, @standby_groups) = @_;
	my $prefix = 'ma' . (++$seq);
	my @n = make_cluster($prefix, $count, replication => 1);
	$n[0]->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my (%s, %id);
	for my $g (@standby_groups)
	{
		$s{$g} = make_standby($n[$g], "${prefix}_s$g");
		$id{$g} = register_standby($n[0], $s{$g}, $g);
		wait_catchup($n[$g], $s{$g});
	}
	return ($prefix, \@n, \%s, \%id);
}

sub term_of { return $_[0]->safe_psql('postgres', 'SELECT term FROM kingres.catalog_status'); }

sub roles_of
{
	return $_[0]->safe_psql('postgres', q{SELECT string_agg(node_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node});
}

# --- 1. the review's script -------------------------------------------------------------
{
	my ($prefix, $n, $s, $id) = setup(3, 0, 1);
	my ($n1, $n2, $n3) = @$n;
	my ($sm, $s1) = ($s->{0}, $s->{1});
	$n1->stop('immediate');
	$n2->stop('immediate');
	promote_standby($sm);
	promote_standby($s1);
	my $roles = roles_of($sm);

	my ($rc, $out, $err) = run_sql($sm, "SELECT kingres.promote_node($id->{0})");
	like($err, qr/only 1 of the 2 other shard groups can be reached/,
		"[$prefix] the promoted map standby cannot promote the map group: it reaches 1 of 2 groups");
	($rc, $out, $err) = run_sql($sm, "SELECT kingres.promote_node($id->{1})");
	isnt($rc, 0, "[$prefix] and it cannot promote group 1 either (S9-02)");
	like($err, qr/promoted in PostgreSQL after the catalog named it the map group primary/,
		"[$prefix] the refusal says that the catalog has not recognised the promotion");
	is(roles_of($sm), $roles, "[$prefix] nothing was changed in its catalog");
	is(term_of($n3), '0', "[$prefix] and node 3 was not told anything");
	($rc, $out, $err) = run_sql($sm, "SELECT kingres.promote_node($id->{0})");
	like($err, qr/only 1 of the 2 other shard groups can be reached/,
		"[$prefix] the group that was not promoted does not count: the map promotion is still refused");

	# --- 2. every other catalog change is refused on that node ---------------------------
	for my $call (
		"SELECT kingres.add_node('127.0.0.1', 1)",
		"SELECT kingres.remove_node($id->{1})",
		"SELECT kingres.sync_catalog()",
		"SELECT kingres.rotate_secret()",
		"SELECT kingres.set_node_property(3, false)",
		"SELECT kingres.move_bucket(1, 0, 2)",
		"CREATE TABLE public.t2 (id int PRIMARY KEY); SELECT kingres.create_distributed_table('public.t2', 'id')",
		"ALTER TABLE public.kv ADD COLUMN extra int")
	{
		($rc, $out, $err) = run_sql($sm, $call);
		isnt($rc, 0, "[$prefix] refused on the unrecognised node: $call");
		like($err, qr/promoted in PostgreSQL after the catalog named it/, "[$prefix] by the rule, not by a side effect: $call");
	}
	is(roles_of($sm), $roles, "[$prefix] the catalog is still the old one");
}

# --- 3. a promotion that has the majority works, and recognises the node ----------------
{
	my ($prefix, $n, $s, $id) = setup(3, 0);
	my ($n1, $n2, $n3) = @$n;
	my $sm = $s->{0};
	$n1->stop('immediate');
	promote_standby($sm);
	my ($rc, $out, $err) = run_sql($sm, "SELECT kingres.set_node_property(3, true)");
	like($err, qr/promoted in PostgreSQL after the catalog named it/, "[$prefix] before its promotion in the catalog it changes nothing");
	$sm->safe_psql('postgres', "SELECT kingres.promote_node($id->{0})");
	like($sm->safe_psql('postgres', "SELECT kingres.promote_node($id->{0})"), qr/"already"/, "[$prefix] the promotion is final");
	($rc, $out, $err) = run_sql($sm, "SELECT kingres.set_node_property(3, true)");
	is($rc, 0, "[$prefix] after it the node changes the catalog as the map primary") or diag($err);
	is(term_of($n2), '1', "[$prefix] and the others hold the new term");
}

# --- 4. "reached" means reached now -------------------------------------------------------
{
	my ($prefix, $n, $s, $id) = setup(2, 0);
	my ($n1, $n2) = @$n;
	my $sm = $s->{0};
	$n1->stop('immediate');
	promote_standby($sm);
	# Node 2 answers at the address of the primary of group 1, but it is a standby in PostgreSQL.
	$n2->stop;
	append_to_file($n2->data_dir . '/standby.signal', '');
	$n2->start;
	my ($rc, $out, $err) = run_sql($sm, "SELECT kingres.promote_node($id->{0})");
	like($err, qr/only 0 of the 1 other shard groups can be reached/,
		"[$prefix] a node that answers but is in recovery does not count as the primary of its group");
}

done_testing();
