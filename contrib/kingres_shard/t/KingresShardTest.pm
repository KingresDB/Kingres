# SPDX-License-Identifier: PostgreSQL
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
use Exporter 'import';

our @EXPORT_OK = qw(make_node make_cluster keys_of_group run_sql group_of_key
  prepared_count wait_prepared_count logical_settings move_state wait_for_file);

# A node that preloads the extension.  Two-phase commit needs prepared
# transactions; recovery_interval is 0 (no background recovery) unless asked.
sub make_node
{
	my ($name, %opt) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	if ($opt{logical})
	{
		$node->init(allows_streaming => 'logical');
	}
	else
	{
		$node->init;
	}
	my $interval = defined $opt{recovery_interval} ? $opt{recovery_interval} : 0;
	my $conf = "shared_preload_libraries = 'kingres_shard'\n"
	  . "max_prepared_transactions = " . ($opt{max_prepared} // 20) . "\n"
	  . "kingres_shard.recovery_interval = $interval\n";
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
sub wait_for_file
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

1;
