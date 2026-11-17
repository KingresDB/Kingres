# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/090_readd_old_primary.pl
#
# Review S10-06: sequence 6 ("Re-adding an old primary as a standby") sent the operator to pg_rewind
# after a crash and named no fallback; a standby that comes back on another address is a new node.
# Both ways are run as the README writes them, after a failover with an unreachable OLD that has
# WAL which NEW does not have:
#
#   A. pg_rewind: OLD's own crash recovery first (pg_rewind does it, or start and stop it cleanly),
#      then pg_rewind against NEW, then standby.signal and primary_conninfo; the node row is already
#      a standby, nothing is registered; OLD streams, and the rows that only OLD had are gone;
#   B. the fallback, a fresh base backup of NEW on another address: a different node.  The old row is
#      taken out of the lists and removed (remove_node), the new one added (add_node); check_cluster()
#      reports the old row as standby_unreachable until then and nothing afterwards.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run;
use KingresShardTest qw(make_cluster make_standby register_standby wait_catchup promote_standby run_sql);

sub setup
{
	my ($prefix) = @_;
	my @n = make_cluster($prefix, 2, replication => 1);
	$n[1]->append_conf('postgresql.conf', "wal_keep_size = '256MB'\n");
	$n[1]->restart;
	$n[0]->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 6);
	});
	my $old = $n[1];
	my $new = make_standby($old, "${prefix}_new");
	my $id = register_standby($n[0], $new, 1);
	wait_catchup($old, $new);
	$old->safe_psql('postgres', 'CREATE TABLE public.t1 (a int); INSERT INTO public.t1 SELECT generate_series(1, 100)');
	wait_catchup($old, $new);
	# the standby is cut off, and OLD writes what NEW will not have; then OLD crashes
	$new->append_conf('postgresql.conf', "primary_conninfo = 'host=/nonexistent port=1 application_name=" . $new->name . "'\n");
	$new->reload;
	$new->poll_query_until('postgres', "SELECT NOT EXISTS (SELECT 1 FROM pg_stat_wal_receiver)");
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 130)');
	$old->stop('immediate');
	promote_standby($new);
	$new->safe_psql('postgres', "ALTER SYSTEM SET synchronous_standby_names = ''");
	$new->safe_psql('postgres', 'SELECT pg_reload_conf()');
	my ($rc, $out, $err) = run_sql($n[0], "SELECT kingres.promote_node($id)");
	is($rc, 0, "[$prefix] promote_node ($err)");
	run_sql($n[0], "SELECT kingres.promote_node($id)");
	return ($n[0], $old, $new, $id);
}

# --- A. pg_rewind ---------------------------------------------------------------------------
{
	my ($map, $old, $new, $id) = setup('ra');
	$new->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(201, 210)');
	# OLD's crash recovery first: start it as it is and stop it cleanly (pg_rewind would run it in single-user mode)
	# pg_rewind copies the configuration files of the source over those of the target (they live in
	# the data directory): keep OLD's own, and put them back afterwards
	my %conf;
	foreach my $f (qw(postgresql.conf postgresql.auto.conf pg_hba.conf))
	{
		$conf{$f} = PostgreSQL::Test::Utils::slurp_file($old->data_dir . "/$f");
	}
	my ($out, $err);
	my $ok = IPC::Run::run([ 'pg_rewind', '--target-pgdata', $old->data_dir, '--source-server',
			'host=' . $new->host . ' port=' . $new->port . ' dbname=postgres', '--progress' ],
		'>', \$out, '2>', \$err);
	ok($ok, "[ra] pg_rewind of the crashed OLD against NEW works ($err)");
	foreach my $f (keys %conf)
	{
		open(my $fh, '>', $old->data_dir . "/$f") or die "$f: $!";
		print $fh $conf{$f};
		close($fh);
	}
	# OLD becomes a standby of NEW
	$old->enable_streaming($new);
	$old->start;
	wait_catchup($new, $old);
	is($old->safe_psql('postgres', 'SELECT count(*) FROM public.t1'), '110', "[ra] OLD has NEW's rows; the 30 that only OLD had are gone");
	is($map->safe_psql('postgres', "SELECT string_agg(node_id || ':' || role, ',' ORDER BY node_id) FROM kingres.node"),
		"1:primary,2:standby,3:primary", "[ra] the node row of OLD was already a standby; nothing was called");
	is($map->safe_psql('postgres', "SELECT replication_state FROM kingres.standby_status() WHERE node_id = 2"), 'streaming', "[ra] standby_status shows OLD streaming");
	is($old->safe_psql('postgres', 'SELECT superseded FROM kingres.catalog_status'), 'f', "[ra] catalog_status on OLD: not superseded");
	is($map->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name LIKE 'standby%'"), '0', "[ra] check_cluster shows no standby problem");
}

# --- B. the fallback: a base backup on another address is a new node ---------------------------
{
	my ($map, $old, $new, $id) = setup('rb');
	is($map->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'standby_unreachable' AND node_id = 2"), '1',
		"[rb] until it is rebuilt, check_cluster reports the old row as standby_unreachable");
	my $fresh = make_standby($new, 'rb_fresh');
	wait_catchup($new, $fresh);
	is($fresh->safe_psql('postgres', 'SELECT count(*) FROM public.t1'), '100', "[rb] the rebuilt standby has NEW's rows");
	# it is another node: a new row; the old one goes (its name was in no list: NEW's list is empty)
	my ($rc, $out, $err) = run_sql($map, "SELECT kingres.remove_node(2)");
	is($rc, 0, "[rb] remove_node of the old row ($err)");
	my $nid = register_standby($map, $fresh, 1);
	ok($nid > 2, "[rb] the rebuilt standby is a new node ($nid)");
	is($new->safe_psql('postgres', "SELECT kingres.standby_name_matches('" . $fresh->name . "')"), 't', "[rb] its name matches");
	$new->safe_psql('postgres', "ALTER SYSTEM SET synchronous_standby_names = 'ANY 1 (" . $fresh->name . ")'");
	$new->safe_psql('postgres', 'SELECT pg_reload_conf()');
	is($map->safe_psql('postgres', "SELECT replication_state FROM kingres.standby_status() WHERE node_id = $nid"), 'streaming', "[rb] standby_status shows it streaming");
	is($map->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster()"), '0', "[rb] check_cluster is empty");
}

done_testing();
