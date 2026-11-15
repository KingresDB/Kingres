# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Helpers shared by the TAP tests of phase 2a: building a cluster, finding keys
# owned by a group, and running SQL with its error text.

package KingresShardTest;

use strict;
use warnings FATAL => 'all';
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run ();
use Exporter 'import';

our @EXPORT_OK = qw(make_node make_cluster keys_of_group run_sql group_of_key
  prepared_count wait_prepared_count logical_settings move_state wait_for_fault_file
  make_standby register_standby wait_catchup set_sync promote_standby wait_waiter
  start_psql finish_psql);

# A node that preloads the extension.  Two-phase commit needs prepared
# transactions; recovery_interval is 0 (no background recovery) unless asked.
sub make_node
{
	my ($name, %opt) = @_;
	# install_path: a node of another PostgreSQL installation (t/046_two_majors.pl).
	my $node = PostgreSQL::Test::Cluster->new($name,
		(defined $opt{install_path} ? (install_path => $opt{install_path}) : ()));
	if ($opt{logical})
	{
		$node->init(allows_streaming => 'logical');
	}
	elsif ($opt{archiving})
	{
		# WAL archive and base backups, for restoring to a restore point.
		$node->init(allows_streaming => 1, has_archiving => 1);
	}
	elsif ($opt{replication})
	{
		$node->init(allows_streaming => 1);
	}
	else
	{
		$node->init;
	}
	my $interval = defined $opt{recovery_interval} ? $opt{recovery_interval} : 0;
	my $conf = "shared_preload_libraries = 'kingres_shard'\n"
	  . "max_prepared_transactions = " . ($opt{max_prepared} // 20) . "\n"
	  . "kingres_shard.recovery_interval = $interval\n"
	  . "kingres_shard.deadlock_check_interval = " . ($opt{deadlock_interval} // 0) . "\n"
	  . "kingres_shard.deadlock_confirm_delay = " . ($opt{deadlock_confirm} // 200) . "\n";
	$conf .= logical_settings() if $opt{logical};
	$conf .= $opt{extra} if defined $opt{extra};
	# tcp: the node also listens on 127.0.0.1 and trusts connections from there, so that
	# a cluster can be registered by an address that libpq connects to by TCP (SSL
	# options only apply to TCP connections).
	$conf .= "listen_addresses = '127.0.0.1'\n" if $opt{tcp};
	$node->append_conf('postgresql.conf', $conf);
	$node->append_conf('pg_hba.conf', "host all all 127.0.0.1/32 trust\n") if $opt{tcp};
	$node->start;
	return $node;
}

# Settings of a node that can be the source or the target of a bucket move
# (phase 4): logical WAL, room for the slots, senders and workers of a few moves.
sub logical_settings
{
	return "wal_level = logical\n"
	  . "max_replication_slots = 40\n"
	  . "max_wal_senders = 40\n"
	  . "max_logical_replication_workers = 16\n"
	  . "max_worker_processes = 48\n"
	  . "wal_receiver_status_interval = 1\n";
}

# A cluster of $count nodes, node i being its own shard group (node 1 is the
# map group primary).  Returns the nodes.
sub make_cluster
{
	my ($prefix, $count, %opt) = @_;
	my @nodes = map { make_node("$prefix$_", %opt) } (1 .. $count);
	$nodes[0]->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
	$nodes[0]->safe_psql('postgres',
		sprintf("SELECT kingres.init_cluster('%s', %d)", ($opt{tcp} ? "127.0.0.1" : $nodes[0]->host), $nodes[0]->port));
	for my $i (1 .. $#nodes)
	{
		$nodes[0]->safe_psql('postgres',
			sprintf("SELECT kingres.add_node('%s', %d)", ($opt{tcp} ? "127.0.0.1" : $nodes[$i]->host), $nodes[$i]->port));
	}
	return @nodes;
}

# Run SQL and return (exit code, stdout, stderr).
sub run_sql
{
	my ($node, $sql, %extra) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql('postgres', $sql, stdout => \$stdout, stderr => \$stderr, %extra);
	return ($rc, $stdout, $stderr);
}

# $count keys, taken from $start upwards, whose bucket in the colocation group
# $colocation belongs to shard group $group.
sub keys_of_group
{
	my ($node, $colocation, $group, $count, $start) = @_;
	$start //= 1;
	my $list = $node->safe_psql('postgres', qq{
		SELECT string_agg(k::text, ',' ORDER BY k) FROM (
		  SELECT k FROM generate_series($start, $start + 5000) k
		   WHERE (SELECT m.group_id FROM kingres.bucket_map m
		           WHERE m.colocation_id = $colocation
		             AND m.bucket = kingres.bucket_for($colocation, k)) = $group
		   LIMIT $count) s});
	return split /,/, $list;
}

sub group_of_key
{
	my ($node, $colocation, $key) = @_;
	return $node->safe_psql('postgres', qq{
		SELECT m.group_id FROM kingres.bucket_map m
		 WHERE m.colocation_id = $colocation
		   AND m.bucket = kingres.bucket_for($colocation, $key)});
}

sub prepared_count
{
	my ($node) = @_;
	return $node->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts');
}

sub wait_prepared_count
{
	my ($node, $want) = @_;
	return $node->poll_query_until('postgres',
		"SELECT count(*) = $want FROM pg_prepared_xacts");
}

# The state of a bucket move as one line: phase|detail.
sub move_state
{
	my ($node, $id) = @_;
	return $node->safe_psql('postgres',
		"SELECT phase || '|' || coalesce(detail, '') FROM kingres.bucket_move WHERE move_id = $id");
}

# Wait (at most $timeout seconds) until the fault point that waits on $file has
# been reached (fault.c creates $file.reached).
sub wait_for_fault_file
{
	my ($file, $timeout) = @_;
	$timeout //= 60;
	for (my $i = 0; $i < $timeout * 20; $i++)
	{
		return 1 if -e $file;
		select(undef, undef, undef, 0.05);
	}
	return 0;
}

# A streaming standby of $primary (a node made with replication => 1 or logical => 1): a base
# backup, started in recovery with primary_conninfo.  It preloads the extension like the primary.
sub make_standby
{
	my ($primary, $name, %opt) = @_;
	my $backup = "bk_$name";
	$primary->backup($backup);
	my $sb = PostgreSQL::Test::Cluster->new($name);
	$sb->init_from_backup($primary, $backup, has_streaming => 1);
	$sb->start;
	return $sb;
}

# Register the standby as a standby node of the group of its primary; returns the node id.
sub register_standby
{
	my ($map, $standby, $group) = @_;
	return $map->safe_psql('postgres',
		sprintf("SELECT kingres.add_node('%s', %d, %d, 'standby')", $standby->host, $standby->port, $group));
}

# Wait until the standby has replayed everything the primary has written.
sub wait_catchup
{
	my ($primary, $standby) = @_;
	$primary->wait_for_catchup($standby);
}

# Make the given standbys (or none) the synchronous ones of the primary.
sub set_sync
{
	my ($primary, @standbys) = @_;
	my $names = @standbys ? "ANY 1 (" . join(', ', map { $_->name } @standbys) . ")" : '';
	$primary->append_conf('postgresql.conf', "synchronous_standby_names = '$names'\n");
	$primary->reload;
}

# Promote a standby in PostgreSQL (what the control plane does before it calls
# kingres.promote_node) and wait until it accepts writes.
sub promote_standby
{
	my ($standby) = @_;
	$standby->promote;
	$standby->poll_query_until('postgres', 'SELECT NOT pg_is_in_recovery()');
}

# Wait until at least $count backends of $node wait for a lock.
sub wait_waiter
{
	my ($node, $count) = @_;
	$count //= 1;
	return $node->poll_query_until('postgres',
		"SELECT count(*) >= $count FROM pg_locks WHERE NOT granted");
}

# Run SQL in a psql of its own in the background (several statements are one
# simple query); finish_psql() waits for it and returns (stdout, stderr).
sub start_psql
{
	my ($node, $sql) = @_;
	my ($out, $err) = ('', '');
	my $h = IPC::Run::start(
		[ 'psql', '-X', '-At', '-h', $node->host, '-p', $node->port, '-d', 'postgres', '-c', $sql ],
		'>', \$out, '2>', \$err);
	return { h => $h, out => \$out, err => \$err };
}

sub finish_psql
{
	my ($p) = @_;
	$p->{h}->finish;
	return (${ $p->{out} }, ${ $p->{err} });
}

1;
