# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/093_control_scope.pl
#
# Review S11-01, S11-02, S11-12.  The deadline of the control plane (node_timeout per statement,
# and the lock_timeout of the node session) exists for the calls of the control plane only.  The
# invariants:
#
#   1. A statement of a user is never cancelled, shortened or dropped by it: after a control call in
#      the same transaction, in an earlier transaction of the session, inside a subtransaction that
#      was rolled back, inside a function with a SET clause, or after a control call that failed
#      before it made a connection, a user statement to a node that does not answer for longer than
#      node_timeout waits, and succeeds when the node answers.  The same transaction without the
#      control call is the control experiment: same rows, same time class.
#   2. A node session that held the writes of the transaction is never closed without the
#      transaction knowing: the commit fails with an error that names the node.
#   3. A node session never carries the lock_timeout of a control call into a user statement.
#
# A node that does not answer is made two ways: its client backends are stopped (SIGSTOP) for a fixed
# time and continued, and a row lock is held on the node for longer than node_timeout.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster keys_of_group run_sql prepared_count start_psql finish_psql);

my $T = 2;			# node_timeout
my $HOLD = 6;		# seconds a node does not answer or a lock is held: well above T and above 0.75 T
my $seq = 0;

sub setup
{
	my $prefix = 'cs' . (++$seq);
	my @n = make_cluster($prefix, 3);
	$_->append_conf('postgresql.conf', "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = 2\n") for @n;
	$_->reload for @n;
	$n[0]->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my @k1 = keys_of_group($n[0], 1, 1, 2);
	my @k2 = keys_of_group($n[0], 1, 2, 4);
	return ($prefix, \@n, \@k1, \@k2);
}

sub client_pids
{
	my ($node) = @_;
	my $pids = $node->safe_psql('postgres',
		"SELECT pid FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()");
	return grep { /^\d+$/ } split /\n/, $pids;
}

# Stop the client backends of $victim now; a child continues them after $secs.  Returns the pids.
sub stop_for
{
	my ($victim, $secs) = @_;
	my @pids = client_pids($victim);
	kill 'STOP', $_ for @pids;
	my $child = fork();
	die "fork: $!" unless defined $child;
	if ($child == 0)
	{
		sleep $secs;
		kill 'CONT', $_ for @pids;
		POSIX::_exit(0);
	}
	return ($child, @pids);
}

sub reap
{
	my ($child, @pids) = @_;
	waitpid($child, 0);
	kill 'CONT', $_ for @pids;
}

# Hold a row lock of $key on $node for $secs seconds (a session of its own).
sub hold_row
{
	my ($node, $key, $secs) = @_;
	my $h = start_psql($node, "SET kingres_shard.enable_routing = off; BEGIN; SELECT * FROM public.kv WHERE id = $key FOR UPDATE; SELECT pg_sleep($secs); COMMIT;");
	for (my $i = 0; $i < 100; $i++)
	{
		last if $node->safe_psql('postgres', "SELECT count(*) FROM pg_locks WHERE locktype = 'transactionid' AND granted") ne '0';
		sleep 0.1;
	}
	return $h;
}

sub rows_of
{
	my ($node) = @_;
	# the rows physically on this node (a query on the table would be distributed)
	return join(',', split /\n/, $node->safe_psql('postgres',
		"SET kingres_shard.enable_routing = off;\nSELECT id || ':' || val FROM public.kv ORDER BY id"));
}

# A session on $node; $pre is run in it first.
sub session
{
	my ($node) = @_;
	my $bg = $node->background_psql('postgres', on_error_stop => 0, timeout => 120);
	$bg->query_safe("\\set VERBOSITY verbose");
	return $bg;
}

sub sq
{
	my ($bg, $sql) = @_;
	$bg->{stderr} = '';
	my $t0 = time;
	my $out = $bg->query($sql);
	return (time - $t0, $out, $bg->{stderr});
}

# --- 1. a node that does not answer, with and without a control call before the user statement ----
foreach my $control ('', 'SELECT kingres.sync_catalog();', 'SELECT kingres.rotate_secret();',
	"SELECT kingres.create_reference_table('public.ref%d');")
{
	my ($prefix, $n, $k1, $k2) = setup();
	my ($map, $g1, $g2) = @$n;
	my $label = $control eq '' ? 'no control call' : $control;
	my $sql = $control;
	if ($sql =~ /ref%d/)
	{
		$map->safe_psql('postgres', 'CREATE TABLE public.ref1 (a int primary key)');
		$sql = "SELECT kingres.create_reference_table('public.ref1');";
	}
	my $bg = session($map);
	sq($bg, 'BEGIN');
	sq($bg, $sql) if $sql ne '';
	sq($bg, "INSERT INTO public.kv VALUES ($k2->[0], 'first')");
	sq($bg, "INSERT INTO public.kv VALUES ($k1->[0], 'on-node2')");
	my ($child, @pids) = stop_for($g2, $HOLD);
	my ($t, $out, $err) = sq($bg, "INSERT INTO public.kv VALUES ($k2->[1], 'second')");
	reap($child, @pids);
	ok($t >= $HOLD - 1, "[$prefix] ($label) the user INSERT waited for the node that did not answer: $t s");
	unlike($err, qr/did not answer within/, "[$prefix] ($label) and no deadline warning ($err)");
	($t, $out, $err) = sq($bg, 'COMMIT');
	is($err, '', "[$prefix] ($label) COMMIT succeeds without a message");
	is(rows_of($g2), "$k2->[0]:first,$k2->[1]:second", "[$prefix] ($label) both rows of node 3 are there");
	is(rows_of($g1), "$k1->[0]:on-node2", "[$prefix] ($label) the row of node 2 is there");
	is(prepared_count($g1) + prepared_count($g2), 0, "[$prefix] ($label) nothing prepared");
	$bg->quit;
}

# --- 2. a row lock held longer than node_timeout: the user statement waits and succeeds ---------------
{
	my ($prefix, $n, $k1, $k2) = setup();
	my ($map, $g1, $g2) = @$n;
	$map->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'orig')") for @$k2;
	my @modes = (
		[ 'no control call', '' ],
		[ 'control call in the transaction', 'SELECT kingres.sync_catalog();' ],
	);
	my $round = 0;
	foreach my $m (@modes)
	{
		for my $i (1 .. 6)
		{
			my $key = $k2->[$round++ % 4];
			my $hold = hold_row($g2, $key, $HOLD);
			my $bg = session($map);
			sq($bg, 'BEGIN');
			sq($bg, $m->[1]) if $m->[1] ne '';
			my ($t, $out, $err) = sq($bg, "UPDATE public.kv SET val = 'user-$round' WHERE id = $key");
			ok($t >= $HOLD - 2, "[$prefix] ($m->[0], round $i) the UPDATE waited for the lock: $t s");
			unlike($err, qr/did not answer|55P03|lock not available/, "[$prefix] ($m->[0], round $i) with no deadline and no lock timeout ($err)");
			($t, $out, $err) = sq($bg, 'COMMIT');
			is($err, '', "[$prefix] ($m->[0], round $i) COMMIT without a message");
			$bg->quit;
			eval { finish_psql($hold) };
			is($g2->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $key"), "user-$round",
				"[$prefix] ($m->[0], round $i) the user's value is the one that is stored");
		}
	}
}

# --- 3. a control call that failed early leaves nothing behind (S11-02) ------------------------------
# Each case: a call that fails, then (in the same session, later or in the next transaction) an UPDATE
# that must wait for a row lock held longer than 0.75 x node_timeout and succeed.
{
	my ($prefix, $n, $k1, $k2) = setup();
	my ($map, $g1, $g2) = @$n;
	$map->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'orig')") for @$k2;
	$g1->safe_psql('postgres', "SELECT 1");
	my $wrong = 'SELECT kingres.sync_catalog();';		# on node 2: KS010
	my @cases = (
		[ 'KS010 (wrong node), next transaction', $g1, [ $wrong ], [ 'BEGIN' ] ],
		[ 'KS010 swallowed by a PL/pgSQL block, same transaction', $g1,
		  [ 'BEGIN', "DO \$\$ BEGIN BEGIN PERFORM kingres.sync_catalog(); EXCEPTION WHEN OTHERS THEN NULL; END; END \$\$;" ], [] ],
		[ 'a control call in a rolled back subtransaction', $map,
		  [ 'BEGIN', 'SAVEPOINT s1', 'SELECT kingres.sync_catalog()', 'ROLLBACK TO s1' ], [] ],
		[ 'a control call in a function with a SET clause', $map,
		  [ q{CREATE OR REPLACE FUNCTION public.cc() RETURNS void LANGUAGE plpgsql SET search_path = public AS $$ BEGIN PERFORM kingres.sync_catalog(); END $$},
			'BEGIN', 'SELECT public.cc()' ], [] ],
		[ 'a control call, then the next transaction', $map,
		  [ 'SELECT kingres.sync_catalog()' ], [ 'BEGIN' ] ],
	);
	my $round = 0;
	foreach my $c (@cases)
	{
		my ($name, $on, $before, $after) = @$c;
		my $key = $k2->[$round++ % 4];
		my $bg = session($on);
		sq($bg, $_) for @$before;
		sq($bg, $_) for @$after;
		# the node session of $on to node 3 must exist for a lock wait to be bounded by it
		my $hold = hold_row($g2, $key, $HOLD);
		my ($t, $out, $err) = sq($bg, "UPDATE public.kv SET val = 'user-$round' WHERE id = $key");
		ok($t >= $HOLD - 2, "[$prefix] ($name) the UPDATE waited for the lock: $t s");
		unlike($err, qr/did not answer|55P03|lock not available/, "[$prefix] ($name) and it is not cut short ($err)");
		sq($bg, 'COMMIT');
		$bg->quit;
		eval { finish_psql($hold) };
		is($g2->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $key"), "user-$round", "[$prefix] ($name) the user's value is stored");
	}

	# KS008: another call holds the catalog; this call fails before it made a connection
	{
		my $name = 'KS008, next transaction';
		my $key = $k2->[$round++ % 4];
		my $holder = $map->background_psql('postgres', on_error_stop => 0);
		$holder->query_safe('BEGIN; LOCK TABLE kingres.map_version IN EXCLUSIVE MODE');
		my $bg = session($map);
		my ($t, $out, $err) = sq($bg, 'SELECT kingres.sync_catalog()');
		like($err, qr/KS008/, "[$prefix] ($name) the call is refused with KS008 ($err)");
		$holder->query_safe('ROLLBACK');
		$holder->quit;
		sq($bg, 'BEGIN');
		my $hold = hold_row($g2, $key, $HOLD);
		($t, $out, $err) = sq($bg, "UPDATE public.kv SET val = 'user-$round' WHERE id = $key");
		ok($t >= $HOLD - 2, "[$prefix] ($name) the UPDATE waited for the lock: $t s");
		unlike($err, qr/did not answer|55P03|lock not available/, "[$prefix] ($name) and it is not cut short ($err)");
		sq($bg, 'COMMIT');
		$bg->quit;
		eval { finish_psql($hold) };
		is($g2->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $key"), "user-$round", "[$prefix] ($name) the user's value is stored");
	}
}

# --- 4. a cursor opened before a control call, and one opened after it, fetched later ---------------
{
	my ($prefix, $n, $k1, $k2) = setup();
	my ($map, $g1, $g2) = @$n;
	$map->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'v')") for (@$k2, @$k1);
	my $bg = session($map);
	sq($bg, 'BEGIN');
	sq($bg, 'DECLARE c1 CURSOR FOR SELECT id FROM public.kv ORDER BY id');
	sq($bg, 'SELECT kingres.sync_catalog()');
	sq($bg, 'DECLARE c2 CURSOR FOR SELECT id FROM public.kv ORDER BY id');
	foreach my $c ('c1', 'c2')
	{
		my ($child, @pids) = stop_for($g2, $HOLD);
		my ($t, $out, $err) = sq($bg, "FETCH ALL FROM $c");
		reap($child, @pids);
		ok($t >= $HOLD - 1, "[$prefix] the first FETCH of $c after the control call waited for the node: $t s");
		unlike($err, qr/did not answer|returned 0 columns/, "[$prefix] $c: with no deadline and no misleading error ($err)");
		is(scalar(grep { /^\s*\d+\s*$/ } split /\n/, $out), 6, "[$prefix] $c: and returned every row ($out)");
	}
	sq($bg, 'ROLLBACK');
	$bg->quit;
}

# --- 5. a session that held the writes of the transaction is never lost silently (S11-01.3) ---------
# The control call inside the transaction runs into a node that does not answer: its deadline closes
# the session that holds the transaction's write, the error is swallowed by a PL/pgSQL block, and the
# transaction must be unable to commit.  (recover_prepared_transactions refuses in such a transaction
# and has nothing to lose.)
foreach my $call ('kingres.sync_catalog()')
{
	my ($prefix, $n, $k1, $k2) = setup();
	my ($map, $g1, $g2) = @$n;
	my $bg = session($map);
	sq($bg, 'BEGIN');
	sq($bg, "INSERT INTO public.kv VALUES ($k2->[0], 'written')");
	sq($bg, "INSERT INTO public.kv VALUES ($k1->[0], 'on-node2')");
	my ($child, @pids) = stop_for($g2, $HOLD);
	my ($t, $out, $err) = sq($bg, "DO \$\$ BEGIN BEGIN PERFORM ${call}; EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'swallowed: %', SQLERRM; END; END \$\$;");
	reap($child, @pids);
	note("[$prefix] the block said: $out $err");
	($t, $out, $err) = sq($bg, 'COMMIT');
	like($err, qr/node 3 \(/, "[$prefix] ($call) the COMMIT fails and names node 3 (out: $out, err: $err)");
	like($err, qr/ERROR/, "[$prefix] ($call) with an error, not a warning");
	is(rows_of($g2), '', "[$prefix] ($call) nothing of the transaction is on node 3");
	is(rows_of($g1), '', "[$prefix] ($call) nor on node 2: the transaction did not commit on some nodes only");
	is($map->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), '0', "[$prefix] ($call) and the table is empty");
	$bg->quit;
}

# --- 6. a statement that was not carried out is an error (S11-12) ----------------------------------------
{
	my ($prefix, $n, $k1, $k2) = setup();
	my ($map, $g1, $g2) = @$n;
	$map->safe_psql('postgres', "INSERT INTO public.kv VALUES ($k2->[0], 'v')");
	my $bg = session($map);
	sq($bg, 'BEGIN');
	sq($bg, "SELECT val FROM public.kv WHERE id = $k2->[0]");
	$g2->safe_psql('postgres', "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'kingres_shard'");
	sleep 0.5;
	my ($t, $out, $err) = sq($bg, "SELECT val FROM public.kv WHERE id = $k2->[0]");
	like($err, qr/ERROR/, "[$prefix] a statement to a node whose session was closed is an error ($err)");
	unlike($err, qr/returned 0 columns/, "[$prefix] and does not blame the node's answer");
	sq($bg, 'ROLLBACK');
	$bg->quit;

	# the same with a write on that node: the transaction cannot commit on the other nodes either
	$bg = session($map);
	sq($bg, 'BEGIN');
	sq($bg, "INSERT INTO public.kv VALUES ($k2->[1], 'w')");
	sq($bg, "INSERT INTO public.kv VALUES ($k1->[0], 'x')");
	$g2->safe_psql('postgres', "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'kingres_shard'");
	sleep 0.5;
	($t, $out, $err) = sq($bg, 'COMMIT');
	like($err, qr/ERROR/, "[$prefix] the commit of a transaction whose write session on a node was closed fails ($err)");
	is(rows_of($g1), '', "[$prefix] and nothing of it is on the other node");
	is(rows_of($g2), "$k2->[0]:v", "[$prefix] nor on the node whose session was lost");
	$bg->quit;
}

done_testing();
