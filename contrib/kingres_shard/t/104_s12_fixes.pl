# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/104_s12_fixes.pl
#
# Review S12, the findings that need no forwarder:
#   S12-06  a database that installed 0.5 before kingres.pending_node.node_id existed: add_node and
#           recovery say what to run, instead of "column does not exist";
#   S12-05  the two causes of "claims to have been opened by another node" are told apart by the
#           SQLSTATE the node itself reports (KS014: its proof does not match its secret; 28000: it
#           holds no secret at all), not by the words of the message;
#   notes   recover_prepared_transactions() in a REPEATABLE READ transaction (a role whose default is
#           that level) answers 25001 and says what to do;
#   Not covered by the review: ALTER ROLE ... SET lock_timeout on the node (the reviewer used ALTER
#           DATABASE), and a lock wait on a node without one: the deadline of node_timeout ends it with
#           KS007, the node's own lock_timeout with 55P03, in neither case does the lock_timeout of a
#           call reach a statement of a user.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_node make_cluster run_sql);

my $T = 3;
my $conf = "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = 2\n";
my ($n1, $n2) = make_cluster('fx', 2, extra => $conf);
my $n3 = make_node('fx_3', extra => $conf);

sub call
{
	my ($node, $sql) = @_;
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($node, "\\set VERBOSITY verbose\n$sql", timeout => 120);
	return (time - $t0, $rc, $out, $err);
}

# --- S12-06: the column that was added to 0.5 in place -----------------------------------------------
{
	$n1->safe_psql('postgres', 'ALTER TABLE kingres.pending_node DROP COLUMN node_id');
	my $add = sprintf("SELECT kingres.add_node('%s', %d)", $n3->host, $n3->port);
	my (undef, $rc, $out, $err) = call($n1, $add);
	isnt($rc, 0, 'S12-06: add_node on a database whose pending_node has no node_id is refused');
	like($err, qr/42703/, 'S12-06: with the SQLSTATE of an undefined column');
	like($err, qr/kingres\.pending_node has no column node_id/, 'S12-06: that names the table and the column');
	like($err, qr/ALTER TABLE kingres\.pending_node ADD COLUMN node_id/, 'S12-06: and the statement to run');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE port = " . $n3->port), '0', 'S12-06: nothing was registered');
	(undef, $rc, $out, $err) = call($n1, 'SELECT kingres.recover_prepared_transactions()');
	isnt($rc, 0, 'S12-06: so is recovery');
	like($err, qr/ALTER TABLE kingres\.pending_node ADD COLUMN node_id/, 'S12-06: with the same statement to run');
	$n1->safe_psql('postgres', 'ALTER TABLE kingres.pending_node ADD COLUMN node_id pg_catalog.int4');
	(undef, $rc, $out, $err) = call($n1, $add);
	is($rc, 0, "S12-06: after the statement the call works ($err)");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE port = " . $n3->port), '1', 'S12-06: and registers the node');
}

# --- add_node of a node whose database already has a table of the cluster ------------------------------------
# The refusal of the node aborts its transaction; the ROLLBACK that follows runs the hook of the extension on
# a node that has just got the extension and holds no secret yet: it must not read the catalog in an aborted
# transaction (it failed an assertion and ended the backend of the node).
{
	$n1->safe_psql('postgres', q{CREATE TABLE public.fx_t (id int PRIMARY KEY); SELECT kingres.create_distributed_table('public.fx_t', 'id', bucket_count => 4)});
	my $n4 = make_node('fx_4', extra => $conf);
	$n4->safe_psql('postgres', 'CREATE TABLE public.fx_t (id int PRIMARY KEY)');
	my (undef, $rc, $out, $err) = call($n1, sprintf("SELECT kingres.add_node('%s', %d)", $n4->host, $n4->port));
	isnt($rc, 0, 'add_node of a node that already has a table of the cluster is refused');
	like($err, qr/already exists/, "with the node's own error ($err)");
	is($n4->safe_psql('postgres', 'SELECT 1'), '1', 'and the node is still up (its backend did not fail an assertion)');
	is($n4->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'), '0', 'with nothing prepared');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.node WHERE port = " . $n4->port), '0', 'and nothing registered');
	$n4->stop;
	$n1->safe_psql('postgres', 'DROP TABLE public.fx_t');
}

# --- S12-05: the node says which refusal it is -------------------------------------------------------------
{
	$n2->safe_psql('postgres', 'CREATE ROLE bob LOGIN');
	my $wrong = $n2->safe_psql('postgres', q{SELECT kingres.internal_forward_token('bob', 99)});
	{
		local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . $wrong;
		my ($rc, $out, $err) = run_sql($n2, 'SELECT 1', extra_params => [ '-U', 'bob', '-v', 'VERBOSITY=verbose' ]);
		isnt($rc, 0, 'S12-05: a proof that does not match the secret is refused');
		like($err, qr/KS014/, "S12-05: with the SQLSTATE KS014 ($err)");
		like($err, qr/cannot prove it/, 'S12-05: and the words that were always there');
	}
	my $secret = $n2->safe_psql('postgres', 'SELECT encode(secret, \'hex\') FROM kingres.cluster_secret');
	$n2->safe_psql('postgres', 'DELETE FROM kingres.cluster_secret');
	{
		local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on';
		my ($rc, $out, $err) = run_sql($n2, 'SELECT 1', extra_params => [ '-U', 'bob', '-v', 'VERBOSITY=verbose' ]);
		isnt($rc, 0, 'S12-05: a node that belongs to a cluster and holds no secret refuses the claim of an ordinary role');
		like($err, qr/28000/, "S12-05: with 28000, not KS014 ($err)");
		unlike($err, qr/KS014/, 'S12-05: which a control plane must not read as "rebuild the node"');
		like($err, qr/holds no cluster secret/, 'S12-05: and the detail says what is missing');
	}
	$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
	is($n2->safe_psql('postgres', 'SELECT encode(secret, \'hex\') FROM kingres.cluster_secret'), $secret, 'S12-05: sync_catalog restored the secret');
}

# --- the isolation level of the call (the notes of the review) ----------------------------------------------
{
	my (undef, $rc, $out, $err) = call($n1, "SET default_transaction_isolation = 'repeatable read';\nSELECT kingres.recover_prepared_transactions()");
	isnt($rc, 0, 'a role whose default is REPEATABLE READ cannot call recover_prepared_transactions()');
	like($err, qr/25001/, 'with 25001');
	like($err, qr/must run in a READ COMMITTED transaction/, 'and says why');
	(undef, $rc, $out, $err) = call($n1, "SET default_transaction_isolation = 'serializable';\nSELECT kingres.recover_prepared_transactions()");
	like($err, qr/25001/, 'a SERIALIZABLE default is refused the same way');
	(undef, $rc, $out, $err) = call($n1, "SET default_transaction_isolation = 'repeatable read';\nBEGIN ISOLATION LEVEL READ COMMITTED;\nSELECT kingres.recover_prepared_transactions();\nCOMMIT");
	is($rc, 0, "the way out: the call in a READ COMMITTED transaction works under that default ($err)");
}

# --- a lock wait on a node (the lock_timeout of a call is not carried into the node's transaction) ----------
{
	# A session of its own holds a SHARE lock on kingres.map_version of node 2 until it is closed: what only reads
	# (the login, the loading of the catalog, the first statements of a call) goes through, and what writes the
	# table, as the statement that applies the catalog does, waits.
	my $holder = sub {
		my $bg = $n2->background_psql('postgres');
		$bg->query_safe('BEGIN');
		$bg->query_safe('LOCK TABLE kingres.map_version IN SHARE MODE');
		is($n2->safe_psql('postgres', "SELECT count(*) >= 1 FROM pg_locks l JOIN pg_class c ON c.oid = l.relation WHERE c.relname = 'map_version' AND l.mode = 'ShareLock' AND l.granted"),
			't', 'the lock is held on node 2');
		return $bg;
	};
	my $release = sub { my ($bg) = @_; $bg->quit; };
	my $sql = 'SELECT kingres.sync_catalog()';

	# no lock_timeout on the node: the deadline of node_timeout ends the wait
	my $h = $holder->();
	my ($t, $rc, $out, $err) = call($n1, $sql);
	isnt($rc, 0, 'a lock wait on a node ends the call');
	like($err, qr/KS007/, "with KS007: the node did not answer within node_timeout ($err)");
	like($err, qr/waiting for its answer to: SELECT kingres.internal_apply/, 'to the statement that applies the catalog, not to the first command of the session');
	cmp_ok($t, '>=', $T - 0.5, sprintf('after node_timeout, not before (%.1f s)', $t));
	cmp_ok($t, '<=', $T + 6, sprintf('and within the bound (%.1f s)', $t));
	$release->($h);

	# the administrator's lock_timeout on the node's role: the node answers 55P03 by itself
	$n2->safe_psql('postgres', q{DO $$ BEGIN EXECUTE format('ALTER ROLE %I SET lock_timeout = ''1s''', current_user); END $$});
	$h = $holder->();
	($t, $rc, $out, $err) = call($n1, $sql);
	isnt($rc, 0, 'with a lock_timeout of the node: the call ends as well');
	like($err, qr/55P03/, "with the node's own 55P03 ($err)");
	like($err, qr/lock not available on node 2/, 'naming the node');
	$release->($h);
	$n2->safe_psql('postgres', q{DO $$ BEGIN EXECUTE format('ALTER ROLE %I RESET lock_timeout', current_user); END $$});

	# nothing of the call stayed on the node: no prepared transaction, and the call works when the lock is gone
	is($n2->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'), '0', 'nothing is left prepared');
	($t, $rc, $out, $err) = call($n1, $sql);
	is($rc, 0, "the same call succeeds when the lock is gone ($err)");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0', 'check_cluster() is empty');
}

done_testing();
