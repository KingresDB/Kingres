# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/028_timer_survives.pl
#
# L19-01 of the sixth review of the guard: the timer that bounds how long a role
# that is not a superuser holds a lock of the extension (kingres_ledger.lock_hold_limit)
# was switched off by the error handler of the main loop of PostgreSQL, which
# disables every timeout of the backend before it aborts the transaction, and the
# round that answered L17-01 restarted it in the abort of a subtransaction, which
# the second error in an aborted subtransaction did not call.  The decision of
# the sixth round is to stop depending on the timer surviving errors: a role that
# is not a superuser takes the lock only in a statement of its own, so every
# error that reaches the main loop ends the transaction and the lock goes with it
# (t/027), and an error that does not reach it (caught inside the statement)
# disables nothing.
#
# This file attacks what remains: a statement that holds the lock at a known point
# (it waits for an advisory lock that the test holds: the gate) and what can
# happen to it there.  Each case has one of two verdicts, and neither is decided
# by a fixed sleep alone:
#
#   ended    the statement is caught at the gate, the role holds the lock, and the
#            session is ended by the limit (waited for as a condition: the backend
#            is gone), with the error that the case provokes caught or ignored by
#            the statement first;
#   survives the statement ends by its own error (or finishes), the lock is gone
#            and the session is alive: a statement that runs after the limit has
#            passed is served.  The wait before it is only there so that a timer
#            that was left on has fired by then; the verdict is the statement
#            that finished.
#
# The cases: an error caught by an EXCEPTION block after the call (three times,
# and in a nested function), statement_timeout, lock_timeout, a cancel, an error
# in a nested function, a COMMIT and a ROLLBACK inside a procedure called by CALL,
# the extended protocol, and a pipeline whose client never sends Sync.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use IPC::Run ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(sleep);

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('timer_survives');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my $psql = $node->installed_command('psql');
my $BUILDER = 19276;
my $REGISTRY = 19277;
my $VERIFY = 19278;
my $LIMIT = 2;					# seconds
my $super = $node->safe_psql('postgres', 'SELECT current_user');

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE DATABASE d});
$node->safe_psql('d', qq{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO app;
	GRANT EXECUTE ON FUNCTION kingres.ledger_build_block(), kingres.ledger_generate_digest(),
		kingres.ledger_verify_recorded(bigint, bigint), kingres.ledger_enable(regclass, text) TO app;
	ALTER ROLE app SET kingres_ledger.lock_hold_limit = '${LIMIT}s';

	CREATE FUNCTION public.boom() RETURNS int LANGUAGE plpgsql AS \$\$ BEGIN RAISE EXCEPTION 'boom'; END \$\$;
	-- catches two errors, then waits at the gate
	CREATE FUNCTION public.catch_then_gate() RETURNS int LANGUAGE plpgsql AS \$\$
	BEGIN
		BEGIN PERFORM public.boom(); EXCEPTION WHEN OTHERS THEN NULL; END;
		BEGIN PERFORM 1 / (SELECT 0); EXCEPTION WHEN division_by_zero THEN NULL; END;
		PERFORM pg_advisory_lock(777);
		RETURN 1;
	END \$\$;
	CREATE PROCEDURE public.p_commit() LANGUAGE plpgsql AS \$\$
	BEGIN
		PERFORM kingres.ledger_verify_recorded();
		COMMIT;
		PERFORM pg_sleep(${LIMIT} + 1.5);
		RAISE NOTICE 'alive_after_commit';
	END \$\$;
	CREATE PROCEDURE public.p_rollback() LANGUAGE plpgsql AS \$\$
	BEGIN
		PERFORM kingres.ledger_verify_recorded();
		ROLLBACK;
		PERFORM pg_sleep(${LIMIT} + 1.5);
		RAISE NOTICE 'alive_after_rollback';
	END \$\$;
	CREATE PROCEDURE public.p_commit_loop() LANGUAGE plpgsql AS \$\$
	DECLARE i int;
	BEGIN
		FOR i IN 1..4 LOOP
			PERFORM kingres.ledger_verify_recorded();
			COMMIT;
		END LOOP;
		PERFORM pg_sleep(${LIMIT} + 1.5);
		RAISE NOTICE 'alive_after_loop';
	END \$\$;
	-- a lock taken again after the commit is a new hold, and is limited
	CREATE PROCEDURE public.p_commit_then_gate() LANGUAGE plpgsql AS \$\$
	BEGIN
		PERFORM kingres.ledger_verify_recorded();
		COMMIT;
		PERFORM kingres.ledger_verify_recorded();
		PERFORM pg_advisory_lock(777);
		RAISE NOTICE 'not_ended';
	END \$\$;
	GRANT EXECUTE ON FUNCTION public.boom(), public.catch_then_gate() TO app;
	GRANT EXECUTE ON PROCEDURE public.p_commit(), public.p_rollback(), public.p_commit_loop(),
		public.p_commit_then_gate() TO app;
});

#--------------------------------------------------------------------------
# A psql session driven one statement at a time.  sess_send waits for the end
# of the statement; sess_start sends it and returns, sess_finish waits.
#--------------------------------------------------------------------------
sub sess_open
{
	my ($user) = @_;
	my %s = (in => '', out => '', err => '', n => 0);
	$s{h} = IPC::Run::start(
		[ $psql, '-XAtq', '-v', 'ON_ERROR_STOP=0', '-d', $node->connstr('d') . " user=$user", '-f', '-' ],
		'<' => \$s{in}, '>' => \$s{out}, '2>' => \$s{err},
		IPC::Run::timeout(300));
	my $r = sess_send(\%s, 'SELECT pg_backend_pid()');
	$s{pid} = $r->{out} =~ /^(\d+)$/m ? $1 : die "no pid: $r->{out} $r->{err}";
	return \%s;
}

sub sess_start
{
	my ($s, $sql, $raw) = @_;
	$s->{marker} = 'KLSEP' . (++$s->{n});
	$s->{out} = '';
	$s->{err} = '';
	# A raw command is a line of meta-commands (a pipeline), which a ";" would spoil.
	$s->{in} .= ($raw ? "$sql\n" : "$sql;\n") . "\\echo $s->{marker}\n\\warn $s->{marker}\n";
	$s->{h}->pump_nb;
	return;
}

sub sess_finish
{
	my ($s) = @_;
	while ($s->{out} !~ /$s->{marker}\n/ || $s->{err} !~ /$s->{marker}\n/)
	{
		last unless $s->{h}->pumpable;
		$s->{h}->pump;
	}
	my $gone = ($s->{out} !~ /$s->{marker}\n/) ? 1 : 0;
	return { out => $s->{out}, err => $s->{err}, gone => $gone };
}

sub sess_send
{
	my ($s, $sql, $raw) = @_;
	sess_start($s, $sql, $raw);
	return sess_finish($s);
}

sub sess_close
{
	my ($s) = @_;
	eval { $s->{h}->kill_kill; };
	return;
}

sub ext_locks
{
	my ($pid) = @_;
	return $node->safe_psql('d', qq{SELECT coalesce(string_agg(objsubid::text, ',' ORDER BY objsubid), '')
		FROM pg_locks WHERE locktype = 'object' AND classid = 'pg_extension'::regclass
		AND objsubid > 0 AND pid = $pid AND granted});
}

sub nothing_held
{
	return $node->safe_psql('d', q{SELECT count(*) FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid > 0}) eq '0';
}

# The condition waits.  The bounds are generous: they fail a test whose condition
# never comes, and decide nothing else.
sub wait_holds
{
	my ($pid, $subid) = @_;
	return $node->poll_query_until('d', qq{SELECT count(*) = 1 FROM pg_locks
		WHERE pid = $pid AND locktype = 'object' AND classid = 'pg_extension'::regclass
		AND objsubid = $subid AND granted});
}

sub wait_at_gate
{
	my ($pid) = @_;
	return $node->poll_query_until('d', qq{SELECT count(*) = 1 FROM pg_locks
		WHERE pid = $pid AND locktype = 'advisory' AND NOT granted});
}

sub backend_gone
{
	my ($pid) = @_;
	return $node->poll_query_until('d', qq{SELECT count(*) = 0 FROM pg_stat_activity WHERE pid = $pid});
}

my $gate = sess_open($super);

sub close_gate
{
	sess_send($gate, 'SELECT pg_advisory_lock(777)');
	return;
}

sub open_gate
{
	sess_send($gate, 'SELECT pg_advisory_unlock_all()');
	return;
}

my $ended = qr/FATAL:  terminating connection due to administrator command/;

# "ended": the statement is caught at the gate holding the lock, and the limit
# ends the session.
sub case_ended
{
	my ($tag, $stmt, $lock, $setup) = @_;
	close_gate();
	my $v = sess_open('app');
	sess_send($v, $setup) if defined $setup;
	sess_start($v, $stmt);
	ok(wait_holds($v->{pid}, $lock), "$tag: the role holds the lock at the gate");
	ok(wait_at_gate($v->{pid}), "$tag: and its statement waits at the gate");
	ok(backend_gone($v->{pid}), "$tag: the limit ends the session");
	open_gate();
	my $r = sess_finish($v);
	like($r->{err}, $ended, "$tag: the client sees the end of the session");
	unlike($r->{err} . $r->{out}, qr/still_alive|not_ended/, "$tag: and the statement did not go on");
	ok(nothing_held(), "$tag: nothing is held after it");
	sess_close($v);
	return;
}

# "survives": the statement ends by its own error at the gate (or by itself); the lock is
# gone; a statement after the limit has passed is served.
sub case_survives
{
	my ($tag, $stmt, $errre, $setup, $action) = @_;
	close_gate();
	my $v = sess_open('app');
	sess_send($v, $setup) if defined $setup;
	sess_start($v, $stmt);
	if (defined $action)
	{
		ok(wait_at_gate($v->{pid}), "$tag: the statement waits at the gate");
		$action->($v->{pid});
	}
	my $r = sess_finish($v);
	open_gate();
	ok(!$r->{gone}, "$tag: the session survives the statement");
	like($r->{err}, $errre, "$tag: with the error that the case provokes");
	is(ext_locks($v->{pid}), '', "$tag: the lock went with the transaction");
	sleep($LIMIT + 1.5);		# a timer that was left on has fired by now
	my $r2 = sess_send($v, q{SELECT 'alive'});
	ok(!$r2->{gone} && $r2->{out} =~ /^alive$/m, "$tag: and a statement after the limit has passed is served (no timer was left on)");
	sess_close($v);
	return;
}

#--------------------------------------------------------------------------
# An error caught inside the statement after the call: it does not reach the
# main loop, so nothing disables the timer.
#--------------------------------------------------------------------------
case_ended('caught errors after the call (three EXCEPTION blocks)',
	q{DO $$ DECLARE i int; BEGIN
		PERFORM kingres.ledger_verify_recorded();
		FOR i IN 1..3 LOOP
			BEGIN PERFORM 1 / (i - i); EXCEPTION WHEN division_by_zero THEN NULL; END;
		END LOOP;
		PERFORM pg_advisory_lock(777);
		RAISE NOTICE 'still_alive';
	END $$}, $VERIFY);
case_ended('caught errors in a nested function after the call',
	q{SELECT kingres.ledger_verify_recorded(), public.catch_then_gate()}, $VERIFY);
case_ended('a caught error that is a cancel (query_canceled) after the call',
	q{DO $$ BEGIN
		PERFORM kingres.ledger_generate_digest();
		BEGIN
			PERFORM pg_cancel_backend(pg_backend_pid());
			PERFORM pg_sleep(5);
		EXCEPTION WHEN query_canceled THEN NULL;
		END;
		PERFORM pg_advisory_lock(777);
		RAISE NOTICE 'still_alive';
	END $$}, $BUILDER);
case_ended('a caught lock_timeout after the call',
	q{DO $$ BEGIN
		PERFORM kingres.ledger_verify_recorded();
		BEGIN
			SET LOCAL lock_timeout = '200ms';
			PERFORM pg_advisory_lock(777);
		EXCEPTION WHEN lock_not_available THEN NULL;
		END;
		PERFORM pg_advisory_lock(777);
		RAISE NOTICE 'still_alive';
	END $$}, $VERIFY);

#--------------------------------------------------------------------------
# An error that reaches the main loop ends the transaction: the lock goes, the
# timer with it, and the session is not ended afterwards.
#--------------------------------------------------------------------------
case_survives('an error in a nested function',
	q{SELECT kingres.ledger_verify_recorded(), public.boom()}, qr/ERROR:  boom/);
case_survives('statement_timeout',
	q{SELECT kingres.ledger_verify_recorded(), pg_advisory_lock(777)},
	qr/ERROR:  canceling statement due to statement timeout/, q{SET statement_timeout = '700ms'});
case_survives('lock_timeout',
	q{SELECT kingres.ledger_generate_digest(), pg_advisory_lock(777)},
	qr/ERROR:  canceling statement due to lock timeout/, q{SET lock_timeout = '700ms'});
case_survives('a cancel (pg_cancel_backend)',
	q{SELECT kingres.ledger_verify_recorded(), pg_advisory_lock(777)},
	qr/ERROR:  canceling statement due to user request/, undef,
	sub { $node->safe_psql('d', "SELECT pg_cancel_backend($_[0])"); });
case_survives('an error after a caught error',
	q{DO $$ BEGIN
		PERFORM kingres.ledger_verify_recorded();
		BEGIN PERFORM 1 / (SELECT 0); EXCEPTION WHEN division_by_zero THEN NULL; END;
		PERFORM public.boom();
	END $$}, qr/ERROR:  boom/);

#--------------------------------------------------------------------------
# A procedure called by CALL that commits: the locks go with the commit, the timer
# with them; a procedure cannot keep a lock across its own transactions.  A lock
# taken again after the commit is a new hold, and is limited.
#--------------------------------------------------------------------------
foreach my $c (['COMMIT', 'p_commit', 'alive_after_commit'], ['ROLLBACK', 'p_rollback', 'alive_after_rollback'],
	['a loop of calls and COMMITs', 'p_commit_loop', 'alive_after_loop'])
{
	my ($what, $proc, $note) = @$c;
	my $v = sess_open('app');
	my $r = sess_send($v, "CALL public.$proc()");
	ok(!$r->{gone}, "procedure with $what after the call: the session survives past the limit (the timer went with the lock)");
	like($r->{err}, qr/NOTICE:  $note/, "procedure with $what: and it ran to its end");
	is(ext_locks($v->{pid}), '', "procedure with $what: nothing is held");
	sess_close($v);
}
case_ended('a lock taken again after a COMMIT in a procedure', q{CALL public.p_commit_then_gate()}, $VERIFY);

#--------------------------------------------------------------------------
# The extended protocol.  A statement of its own through Parse, Bind, Execute and
# Sync works, holds nothing afterwards, and leaves no timer.  A pipeline whose
# client never sends Sync keeps the transaction open with the backend active: the
# limit ends the session.
#--------------------------------------------------------------------------
{
	my $v = sess_open('app');
	my $r = sess_send($v, q{SELECT kingres.ledger_verify_recorded() > 0 \bind \g}, 1);
	ok(!$r->{gone} && $r->{out} =~ /^t$/m, 'extended protocol: a call that ends with Sync is served');
	is(ext_locks($v->{pid}), '', 'extended protocol: and holds nothing afterwards');
	sleep($LIMIT + 1.5);
	$r = sess_send($v, q{SELECT 'alive'});
	ok(!$r->{gone} && $r->{out} =~ /^alive$/m, 'extended protocol: and no timer was left on');
	sess_close($v);
}
{
	my $v = sess_open('app');
	sess_start($v, "\\startpipeline\nSELECT kingres.ledger_verify_recorded() > 0 \\bind \\sendpipeline\n\\flushrequest\n\\getresults", 1);
	ok(wait_holds($v->{pid}, $VERIFY), 'pipeline without Sync: the role holds the lock');
	is($node->safe_psql('d', "SELECT state FROM pg_stat_activity WHERE pid = $v->{pid}"), 'active',
		'pipeline without Sync: and its backend is active, where no timeout of PostgreSQL reaches it');
	ok(backend_gone($v->{pid}), 'pipeline without Sync: the limit ends the session');
	ok(nothing_held(), 'pipeline without Sync: and the lock is gone');
	sess_close($v);
}

# The same pipeline with an error caught and an error that ends the pipeline (the pipeline
# is rolled back at its Sync): the error cases of the main loop reach the Sync as well.
{
	my $v = sess_open('app');
	my $r = sess_send($v, "\\startpipeline\nSELECT kingres.ledger_verify_recorded() > 0 \\bind \\sendpipeline\n"
		. "SELECT 1/0 \\bind \\sendpipeline\n\\syncpipeline\n\\getresults\n\\endpipeline", 1);
	ok(!$r->{gone}, 'pipeline with an error: the session survives');
	like($r->{err}, qr/division by zero/, 'pipeline with an error: the error is raised');
	is(ext_locks($v->{pid}), '', 'pipeline with an error: nothing is held after the Sync');
	sleep($LIMIT + 1.5);
	$r = sess_send($v, q{SELECT 'alive'});
	ok(!$r->{gone} && $r->{out} =~ /^alive$/m, 'pipeline with an error: and no timer was left on');
	sess_close($v);
}

sess_close($gate);
ok(nothing_held(), 'nothing is held at the end');

done_testing();
