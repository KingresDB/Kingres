# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/100_sweep_calls.pl
#
# Review S12-01: a control call that returned success for work that was not done on a node.  The
# eager RESET lock_timeout at the end of a call closed a node session that still carried the call's
# own remote transaction when the node did not answer that one statement; the commit then skipped
# the session, and remove_node, rotate_secret, add_node and sync_catalog answered success with no
# warning at one position of the silence each.  The reviewer found the holes at single positions.
#
# This file sweeps the position: a forwarder (t/KsProxy.pm) in front of one node of the cluster goes
# silent after the n-th chunk of every session, for EVERY n from 0 (the startup packet is not
# answered) to the last chunk that the call sends, for add_node, remove_node, sync_catalog,
# rotate_secret, create_distributed_table, create_reference_table and
# recover_prepared_transactions (promote_node is t/101).  t/KsSweep.pm judges every answer: an
# error and no mixture of old and new, or success with everything done on every node, or success
# with the WARNING the README names and everything done after the documented repeat (one recovery
# pass).  The last position (nothing is swallowed) is the clean run.
#
# S13-01 and S13-05: the same sweep runs in scenarios, chosen by the environment variable KS_SWEEP_SCENARIO (t/108 and
# t/109 set it and run this file): silent (the default: the forwarder goes silent, in autocommit, every position),
# modes (the connection is closed after chunk n, the node's answer to chunk n is lost, the node answers chunk n with an
# error: a sample of the positions, every one with KINGRES_LONG_TESTS=1) and variants (the call inside SAVEPOINT /
# RELEASE, inside a PL/pgSQL block with an EXCEPTION clause, inside a transaction block that the caller commits: the
# sessions of the call end their work with the statements of a user's transaction).  The bound that every position
# is held to is the README's: 2 x node_timeout + 8 s plus node_connect_timeout.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KsProxy;
use KsSweep qw(sweep_call chunk_max sweeps);
use KingresShardTest qw(make_node run_sql);

my $T = 2;						# node_timeout
my $C = 2;						# node_connect_timeout
my $conf = "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n";
my $seq = 0;
my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my $BOUND = 2 * $T + 8 + $C;		# the README's bound for a node that stops answering

# --- the cluster: MAP, a plain node, and a node behind the forwarder --------------------------------
my $n1 = make_node('sw_1', extra => $conf);
my $n2 = make_node('sw_2', extra => $conf);
my $n3 = make_node('sw_3', tcp => 1, extra => $conf);
$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres', sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('%s', %d)", $n2->host, $n2->port));
my $proxy = KsProxy->new(backend_port => $n3->port, host => '127.0.0.1', ctl => "$tmp/proxy_sw3");
$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d)", $proxy->port));
# a node that add_node and remove_node work on, behind a forwarder of its own
my $n4 = make_node('sw_4', tcp => 1, extra => $conf);
my $proxy4 = KsProxy->new(backend_port => $n4->port, host => '127.0.0.1', ctl => "$tmp/proxy_sw4");
my @nodes = ($n1, $n2, $n3, $n4);

sub call
{
	my ($sql) = @_;
	return run_sql($n1, "\\set VERBOSITY verbose\n$sql", timeout => 120);
}

sub qn
{
	my ($n, $sql) = @_;
	# a node that holds a prepared transaction of the call may hold locks: a state that cannot be read is 'locked'
	my ($rc, $out, $err) = run_sql($n, "SET lock_timeout = '1s'; $sql");
	return $rc == 0 ? $out : 'locked';
}

my %results;
# KS_SWEEP_ONLY=rotate,sync,create,recover,node runs only some of the sweeps (for work on one of them)
my %only = map { $_ => 1 } split /,/, ($ENV{KS_SWEEP_ONLY} // '');
sub want { return !%only || $only{$_[0]}; }

# --- rotate_secret: the secret of every node ----------------------------------------------------------
if (want('rotate'))
{
	my $h = sub { return qn($_[0], "SELECT encode(sha256(secret), 'hex') FROM kingres.cluster_secret"); };
	my $old_hash;
	my $c = {
		name => 'rotate_secret', n1 => $n1, proxy => $proxy, nodes => [ $n1, $n2, $n3 ],
		call => \&call, run => 'SELECT kingres.rotate_secret()',
		prepare => sub {
			$old_hash = $h->($n1);
			$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
		},
		state => sub { return { n1 => $h->($n1), n2 => $h->($n2), n3 => $h->($n3) }; },
		kinds => sub {
			my ($s) = @_;
			my %v = map { $_ => 1 } values %$s;
			return 'mixed' if keys(%v) > 1;
			return $s->{n1} eq $old_hash ? 'old' : 'new';
		},
		cleanup => sub { $n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'); },
		bound => $BOUND,
	};
	$c->{prepare}->();
	$results{rotate_secret} = sweeps($c);
}

# --- sync_catalog: the catalog of every node ----------------------------------------------------------
if (want('sync'))
{
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.sw_kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.sw_kv', 'id', bucket_count => 6);
	});
	# every catalog table that sync_catalog writes (node, colocation, dist_table, bucket_map) and the map version
	my $rows = sub {
		return qn($_[0], q{SELECT (SELECT count(*) FROM kingres.node) || '/' || (SELECT count(*) FROM kingres.colocation) || '/'
			|| (SELECT count(*) FROM kingres.dist_table) || '/' || (SELECT count(*) FROM kingres.bucket_map) || '/'
			|| (SELECT CASE WHEN version = 0 THEN 'stale' ELSE 'current' END FROM kingres.map_version)});
	};
	my $c = {
		name => 'sync_catalog', n1 => $n1, proxy => $proxy, nodes => [ $n1, $n2, $n3 ],
		call => \&call, run => 'SELECT kingres.sync_catalog()',
		prepare => sub {
			# the catalog of the two other nodes is made to miss rows of every table that sync_catalog writes
			$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
			foreach my $n ($n2, $n3)
			{
				$n->safe_psql('postgres', 'SET session_replication_role = replica; DELETE FROM kingres.bucket_map; DELETE FROM kingres.dist_table; DELETE FROM kingres.colocation; UPDATE kingres.map_version SET version = 0');
			}
		},
		state => sub { return { n1 => $rows->($n1), n2 => $rows->($n2), n3 => $rows->($n3) }; },
		cleanup => sub { $n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'); },
		bound => $BOUND,
	};
	$results{sync_catalog} = sweeps($c);
}

# --- create_distributed_table and create_reference_table: a table with rows, on every node -------------
foreach my $kind (want('create') ? ('distributed', 'reference') : ())
{
	my $present = sub {
		my ($n) = @_;
		return qn($n, "SELECT coalesce(to_regclass('public.sw_t')::text, '-')");
	};
	my $cat = sub {
		my ($n) = @_;
		return qn($n, "SELECT count(*) FROM kingres.dist_table d WHERE d.relid::oid = coalesce(to_regclass('public.sw_t')::oid, 0)");
	};
	my $drop = sub {
		foreach my $n ($n2, $n3)
		{
			$n->safe_psql('postgres', 'SET kingres_shard.enable_routing = off; DROP TABLE IF EXISTS public.sw_t CASCADE');
		}
		my ($rc, $out, $err) = call('DROP TABLE IF EXISTS public.sw_t CASCADE');
		die "drop failed: $err" if $rc != 0;
	};
	my $c = {
		name => "create_${kind}_table", n1 => $n1, proxy => $proxy, nodes => [ $n1, $n2, $n3 ],
		call => \&call,
		run => $kind eq 'distributed'
		  ? "SELECT kingres.create_distributed_table('public.sw_t', 'id', bucket_count => 6)"
		  : "SELECT kingres.create_reference_table('public.sw_t')",
		prepare => sub {
			$drop->();
			# a distributed table is made from an empty one; a reference table is copied with its rows
			$n1->safe_psql('postgres', 'CREATE TABLE public.sw_t (id int PRIMARY KEY, val text)');
			$n1->safe_psql('postgres', 'INSERT INTO public.sw_t SELECT i, repeat(\'x\', 50) FROM generate_series(1, 300) i')
			  if $kind eq 'reference';
		},
		state => sub {
			return { n1 => $cat->($n1), n2 => $present->($n2) . '/' . $cat->($n2), n3 => $present->($n3) . '/' . $cat->($n3) };
		},
		cleanup => sub { $drop->(); },
		bound => $BOUND,
	};
	$results{ $c->{name} } = sweeps($c);
	$drop->();
}

# --- recover_prepared_transactions: a transaction in doubt on the node behind the forwarder ----------
if (want('recover'))
{
	$n3->safe_psql('postgres', 'CREATE TABLE IF NOT EXISTS public.sw_rec (id int)');
	my $xid = 4000000000;
	my $gid;
	my $c = {
		name => 'recover_prepared_transactions', n1 => $n1, proxy => $proxy, nodes => [ $n1, $n2, $n3 ],
		call => \&call, run => 'SELECT kingres.recover_prepared_transactions()',
		prepare => sub {
			$n3->safe_psql('postgres', 'DELETE FROM public.sw_rec');
			$n1->safe_psql('postgres', 'DELETE FROM kingres.dtx_log');
			my $body = 'kingres_h0_1_999_' . (++$xid) . '_3';
			$gid = $body . '_' . $n1->safe_psql('postgres', "SELECT kingres.internal_gid_tag('$body')");
			$n3->safe_psql('postgres', "BEGIN; INSERT INTO public.sw_rec VALUES (1); PREPARE TRANSACTION '$gid'");
			$n1->safe_psql('postgres', "INSERT INTO kingres.dtx_log VALUES ('$gid', 3, now())");
		},
		state => sub {
			return { n1 => 'x', n2 => 'x',
				n3 => qn($n3, 'SELECT count(*) FROM pg_prepared_xacts') . '/' . qn($n3, 'SELECT count(*) FROM public.sw_rec') };
		},
		cleanup => sub {
			$n3->safe_psql('postgres', "SELECT gid FROM pg_prepared_xacts");
			foreach my $g (split /\n/, $n3->safe_psql('postgres', "SELECT gid FROM pg_prepared_xacts"))
			{
				$n3->safe_psql('postgres', "ROLLBACK PREPARED '$g'");
			}
		},
		bound => $BOUND,
	};
	$results{recover_prepared_transactions} = sweeps($c);
}

# --- add_node and remove_node: a node that registers (or leaves) behind a forwarder -------------------
if (want('node'))
{
	my $p4 = $proxy4->port;
	my $registered = sub { return qn($_[0], "SELECT count(*) FROM kingres.node WHERE host = '127.0.0.1' AND port = $p4"); };
	my $own = sub {
		return qn($n4, q{SELECT CASE WHEN to_regclass('kingres.node') IS NULL THEN 'none' ELSE
			(SELECT count(*)::text FROM kingres.node) || '/' || (SELECT count(*)::text FROM kingres.cluster_secret) || '/' || (SELECT count(*)::text FROM kingres.local_node) END});
	};
	my $state = sub { return { n1 => $registered->($n1), n2 => $registered->($n2), n3 => $registered->($n3), n4 => $own->() }; };
	my $open = sub { $proxy4->set(swallow_after => '', delay => 0, delay_turn => 0); };
	my $reset4 = sub {
		$open->();
		foreach my $g (split /\n/, $n4->safe_psql('postgres', "SELECT gid FROM pg_prepared_xacts"))
		{
			$n4->safe_psql('postgres', "ROLLBACK PREPARED '$g'");
		}
		$n4->safe_psql('postgres', 'SELECT kingres.internal_reset_node()')
		  if $n4->safe_psql('postgres', q{SELECT to_regclass('kingres.node') IS NOT NULL}) eq 't';
		$n1->safe_psql('postgres', 'DELETE FROM kingres.pending_node');
	};
	my $clear = sub {
		# no node 4 in any catalog, nothing on the node itself
		$open->();
		$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
		if ($registered->($n1) ne '0')
		{
			my ($rc, $out, $err) = call(sprintf("SELECT kingres.remove_node((SELECT node_id FROM kingres.node WHERE host = '127.0.0.1' AND port = %d))", $p4));
			die "cleanup remove_node failed: $err" if $rc != 0;
		}
		$reset4->();
		$n4->safe_psql('postgres', 'DROP EXTENSION IF EXISTS kingres_shard CASCADE; DROP SCHEMA IF EXISTS kingres CASCADE; DROP TABLE IF EXISTS public.sw_kv, public.sw_t CASCADE');
	};
	my $add = {
		name => 'add_node', n1 => $n1, proxy => $proxy4, nodes => [ $n1, $n2, $n3, $n4 ],
		call => \&call, run => "SELECT kingres.add_node('127.0.0.1', $p4)",
		prepare => $clear,
		state => $state,
		cleanup => $clear,
		bound => $BOUND,
	};
	$results{add_node} = sweeps($add);

	my $remove = {
		name => 'remove_node', n1 => $n1, proxy => $proxy4, nodes => [ $n1, $n2, $n3, $n4 ],
		call => \&call,
		run => sprintf("SELECT kingres.remove_node((SELECT node_id FROM kingres.node WHERE host = '127.0.0.1' AND port = %d))", $p4),
		prepare => sub {
			$clear->();
			my ($rc, $out, $err) = call("SELECT kingres.add_node('127.0.0.1', $p4)");
			die "prepare add_node failed: $err" if $rc != 0;
		},
		state => $state,
		kinds => sub {
			my ($s) = @_;
			my $gone = ($s->{n1} eq '0' && $s->{n2} eq '0' && $s->{n3} eq '0');
			my $kept = ($s->{n1} eq '1' && $s->{n2} eq '1' && $s->{n3} eq '1');
			my $cleared = ($s->{n4} =~ m{^(0/0/0|none)$});
			return 'new' if $gone && $cleared;
			return 'partial' if $gone;
			return 'old' if $kept && !$cleared;
			return 'mixed';
		},
		partial => 1,
		no_error_like => qr/KS007/,
		cleanup => $clear,
		bound => $BOUND,
	};
	$results{remove_node} = sweeps($remove);
	$clear->();
}

$proxy->stop;
$proxy4->stop;
done_testing();
