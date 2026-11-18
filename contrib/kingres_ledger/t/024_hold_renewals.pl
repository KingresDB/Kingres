# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/024_hold_renewals.pl
#
# L17-01 of the fifth review of the guard: the limit on holding a lock
# (kingres_ledger.lock_hold_limit) was renewed by work that was rolled back, and
# for a lock other than the one the work stood for, so a role that owned a table
# and could call ledger_enable and a builder (or ledger_verify_recorded) kept
# the builders' lock for as long as it liked.
#
# The property that the code must keep, and that these tests attack, not only
# the statements of the review: a role that is not a superuser never holds a
# lock of the extension for longer than lock_hold_limit plus the time of the
# real, kept work of ledger_enable on the tables it entered in the transaction,
# whatever it does with savepoints, exception blocks, loops, the other functions
# of the extension and other locks.  Three rules keep it (write.c):
#
#   (a) a renewal only while the registry's lock is the only lock of the
#       extension that the transaction holds;
#   (b) a renewal is the time that the rows really took, and it is undone when
#       the subtransaction that made it aborts;
#   (c) within one transaction a table's rows are credited once: rows entered
#       again after a rollback buy nothing.
#
# Each rule has a test that fails when only that rule is taken out: (a) a
# builder's lock held while a large table is entered, (b) a renewal inside a
# savepoint that is rolled back while the parent holds the registry's lock, (c)
# the same table entered again after a rollback.  Then the statements of the
# review, the log line of the ended session (L17-05), and a property test that
# runs random sequences of SAVEPOINT, RELEASE, ROLLBACK TO, the writers and
# sleeps, and checks after every step that a session that holds nothing is
# alive and that a session that has held a lock for longer than the limit is
# gone.
#
# The sizes are derived from a measurement: a table of the same size is entered
# by a superuser first, and the limits are fractions of that time, so the
# verdicts do not depend on the speed of the machine.  A "still alive" verdict is
# never decided by a fixed sleep alone: it is a statement that finished.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use IPC::Run ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('hold_renewals');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my $ROWS = 100000;
my $psql = $node->installed_command('psql');

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE DATABASE d});
$node->safe_psql('d', qq{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO app;
	GRANT EXECUTE ON FUNCTION kingres.ledger_generate_digest(),
		kingres.ledger_verify_recorded(bigint, bigint),
		kingres.ledger_enable(regclass, text) TO app;
	CREATE TABLE ref AS SELECT g AS id, repeat('x', 20) AS body FROM generate_series(1, $ROWS) g;
	CREATE TABLE big_a AS SELECT g AS id, repeat('x', 20) AS body FROM generate_series(1, $ROWS) g;
	CREATE TABLE big_b AS SELECT g AS id, repeat('x', 20) AS body FROM generate_series(1, $ROWS) g;
	CREATE TABLE big_ok AS SELECT g AS id, repeat('x', 20) AS body FROM generate_series(1, $ROWS) g;
	CREATE TABLE med_c AS SELECT g AS id, repeat('x', 20) AS body FROM generate_series(1, $ROWS) g;
	ALTER TABLE big_a OWNER TO app;
	ALTER TABLE big_b OWNER TO app;
	ALTER TABLE big_ok OWNER TO app;
	ALTER TABLE med_c OWNER TO app;
});
for my $i (1 .. 60)
{
	$node->safe_psql('d', "CREATE TABLE p$i AS SELECT g AS id FROM generate_series(1, 3) g; ALTER TABLE p$i OWNER TO app");
}

# The property test runs in a database of its own whose ledger is small: a
# recorded verification there takes no time worth counting.
$node->safe_psql('postgres', q{CREATE DATABASE dp});
$node->safe_psql('dp', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO app;
	GRANT EXECUTE ON FUNCTION kingres.ledger_generate_digest(),
		kingres.ledger_verify_recorded(bigint, bigint),
		kingres.ledger_enable(regclass, text) TO app;
});
for my $i (1 .. 60)
{
	$node->safe_psql('dp', "CREATE TABLE p$i AS SELECT g AS id FROM generate_series(1, 3) g; ALTER TABLE p$i OWNER TO app");
}

# The first read of a table sets the hint bits of its rows and writes the pages: a
# table that was read once is faster to enter than one that was not.  Every table
# is read once before the measurement, so that entering the same table twice in
# a test takes about the same time, and the table of the measurement takes the
# same time as the others.
$node->safe_psql('d', "VACUUM $_") for qw(ref big_a big_b big_ok med_c);

# How long entering $ROWS rows takes, with nothing to stop it (a superuser).
my $t0 = time();
$node->safe_psql('d', q{SELECT kingres.ledger_enable('ref')});
my $D = int((time() - $t0) * 1000);
note("entering $ROWS rows takes $D ms");
cmp_ok($D, '>', 600, "precondition: entering $ROWS rows takes long enough to tell the limits apart ($D ms)");

my $BUILDER = 19276;
my $REGISTRY = 19277;
my $VERIFY = 19278;

sub set_limit
{
	my ($ms) = @_;
	$ms = 1 if $ms < 1;
	$node->safe_psql('d', "ALTER ROLE app SET kingres_ledger.lock_hold_limit = '${ms}ms'");
	return;
}

sub as_app
{
	my ($sql, $db) = @_;
	my ($ret, $out, $err) = $node->psql($db // 'd', $sql, extra_params => [ '-U', 'app' ], on_error_stop => 0);
	return ($ret, $out, $err);
}

sub nothing_held
{
	return $node->safe_psql('d', q{SELECT count(*) FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid > 0}) eq '0';
}

my $ended = qr/FATAL:  terminating connection due to administrator command/;

# A psql session that the test drives one statement at a time.
sub sess_open
{
	my ($user, $db) = @_;
	my %s = (in => '', out => '', err => '', n => 0);
	$s{h} = IPC::Run::start(
		[ $psql, '-XAtq', '-v', 'ON_ERROR_STOP=0', '-d', $node->connstr($db) . " user=$user", '-f', '-' ],
		'<' => \$s{in}, '>' => \$s{out}, '2>' => \$s{err},
		IPC::Run::timeout(300));
	my $r = sess_send(\%s, 'SELECT pg_backend_pid()');
	$s{pid} = $r->{out} =~ /^(\d+)$/m ? $1 : die "no pid: $r->{out} $r->{err}";
	return \%s;
}

# Send one statement; returns {out, err, gone}.
sub sess_send
{
	my ($s, $sql) = @_;
	my $marker = 'KLSEP' . (++$s->{n});
	$s->{out} = '';
	$s->{err} = '';
	$s->{in} .= "$sql;\n\\echo $marker\n";
	while ($s->{out} !~ /$marker\n/)
	{
		last unless $s->{h}->pumpable;
		$s->{h}->pump;
	}
	my $gone = ($s->{out} !~ /$marker\n/) ? 1 : 0;
	return { out => $s->{out}, err => $s->{err}, gone => $gone };
}

sub sess_close
{
	my ($s) = @_;
	eval { $s->{h}->kill_kill; };
	return;
}

# The session's pid is no longer in pg_stat_activity (after the process went).
sub backend_gone
{
	my ($pid) = @_;
	for (1 .. 100)
	{
		return 1 if $node->safe_psql('d', "SELECT count(*) FROM pg_stat_activity WHERE pid = $pid") eq '0';
		sleep(0.1);
	}
	return 0;
}

sub held_by
{
	my ($pid) = @_;
	my $r = $node->safe_psql('d', qq{SELECT coalesce(string_agg(objsubid::text, ',' ORDER BY objsubid), '')
		FROM pg_locks WHERE locktype = 'object' AND classid = 'pg_extension'::regclass
		AND objsubid > 0 AND pid = $pid AND granted});
	return $r;
}


#--------------------------------------------------------------------------
# A real large enable still completes (the control of every test below): the
# limit is a half of the time it takes, and nothing but the registry's lock is
# held.
#--------------------------------------------------------------------------
my $half = int($D / 2);
set_limit($half);
{
	my ($ret, $out, $err) = as_app(q{SELECT kingres.ledger_enable('big_ok') IS NULL AS entered});
	is($ret, 0, "a ledger_enable that takes longer than the limit of $half ms, with nothing else held, completes") or diag($err);
	unlike($err, qr/FATAL/, 'and the session is not ended');
	is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_entry e
		JOIN kingres.ledger_table t ON t.table_id = e.table_id WHERE t.relid = 'big_ok'::regclass}),
		"$ROWS", 'every row was entered');
}

#--------------------------------------------------------------------------
# Rule (a): no renewal while another lock of the extension is held.  The
# builders' lock is taken, then a table that takes longer than the limit is
# entered, and the session is ended: before this round the entering renewed the
# limit of the builders' lock.
#--------------------------------------------------------------------------
{
	my ($ret, $out, $err) = as_app(q{DO $$ BEGIN
		PERFORM kingres.ledger_generate_digest();
		PERFORM kingres.ledger_enable('big_a');
		RAISE NOTICE 'still_alive';
		END $$});
	like($err, $ended, "rule (a): ledger_enable of a large table while the builders' lock is held does not renew the limit: the session is ended");
	unlike($err, qr/still_alive/, 'it did not go on');
	ok(nothing_held(), 'and nothing is held after it');
}

#--------------------------------------------------------------------------
# Rule (b): what a subtransaction renewed goes with it when it aborts.  The
# parent holds the registry's lock; inside a savepoint a table that takes twice
# the limit is entered, which the registry's lock alone may do, and the
# savepoint is rolled back: the deadline is what it was before, which has
# passed, and the session is ended.  Before this round the renewal stayed.
#--------------------------------------------------------------------------
{
	my $slept = int($half / 2000) + 1;
	my ($ret, $out, $err) = as_app(qq{BEGIN;
		SELECT kingres.ledger_enable('p1') IS NULL;
		SAVEPOINT s;
		SELECT kingres.ledger_enable('big_b') IS NULL;
		ROLLBACK TO s;
		SELECT pg_sleep(0.3);
		SELECT 'still_alive';
		COMMIT;});
	like($err, $ended, 'rule (b): a renewal made in a savepoint that is rolled back is undone: the session is ended');
	unlike($out, qr/still_alive/, 'it did not go on');
	ok(nothing_held(), 'and nothing is held after it');

	# The same in a PL/pgSQL EXCEPTION block.
	($ret, $out, $err) = as_app(q{DO $$ BEGIN
		PERFORM kingres.ledger_enable('p2');
		BEGIN
			PERFORM kingres.ledger_enable('big_b');
			RAISE EXCEPTION 'undo';
		EXCEPTION WHEN OTHERS THEN NULL;
		END;
		PERFORM pg_sleep(0.3);
		RAISE NOTICE 'still_alive';
		END $$});
	like($err, $ended, 'the same in an EXCEPTION block');
	unlike($err, qr/still_alive/, 'it did not go on');

	# The control: the same large entering, kept (a released savepoint), is the
	# real work that the limit allows for.
	($ret, $out, $err) = as_app(q{BEGIN;
		SELECT kingres.ledger_enable('p3') IS NULL;
		SAVEPOINT s;
		SELECT kingres.ledger_enable('big_b') IS NULL;
		RELEASE s;
		SELECT 'still_alive';
		COMMIT;});
	is($ret, 0, 'control: the same entering in a savepoint that is released is kept, and the session is not ended') or diag($err);
	like($out, qr/still_alive/, 'and it went on');
	$node->safe_psql('d', q{SELECT 1});
}

#--------------------------------------------------------------------------
# Rule (c): the rows of a table are credited once in a transaction.  A table
# that takes D to enter, a limit of 1.4 D: it is entered in a savepoint that is
# rolled back (the renewal is undone, 0.4 D of the period is left) and entered
# again: the second time has to fit in what is left, and does not.  Without
# the rule the second entering is credited as the first was.
#--------------------------------------------------------------------------
{
	set_limit(int($D * 1.4));
	my ($ret, $out, $err) = as_app(q{BEGIN;
		SELECT kingres.ledger_enable('p4') IS NULL;
		SAVEPOINT s;
		SELECT kingres.ledger_enable('med_c') IS NULL;
		ROLLBACK TO s;
		SELECT kingres.ledger_enable('med_c') IS NULL;
		SELECT 'still_alive';
		COMMIT;});
	like($err, $ended, 'rule (c): the same rows entered again after a rollback are not credited again: the session is ended');
	unlike($out, qr/still_alive/, 'it did not go on');
	ok(nothing_held(), 'and nothing is held after it');

	# The control: entered once, in the same transaction, it fits (the first
	# entering is credited).
	($ret, $out, $err) = as_app(q{BEGIN;
		SELECT kingres.ledger_enable('p5') IS NULL;
		SELECT kingres.ledger_enable('med_c') IS NULL;
		SELECT 'still_alive';
		COMMIT;});
	is($ret, 0, 'control: entering it once is credited, and the transaction commits') or diag($err);
	like($out, qr/still_alive/, 'and it went on');
}

#--------------------------------------------------------------------------
# The statements of the review.  Limit 3 s; every sequence runs for longer
# than that with a lock held, and each ends the session.
#--------------------------------------------------------------------------
set_limit(3000);
my $log_start = -s $node->logfile;
{
	# The DO block with the EXCEPTION loop and the builders' lock.
	my ($ret, $out, $err) = as_app(q{DO $$ DECLARE i int; BEGIN
		PERFORM kingres.ledger_generate_digest();
		FOR i IN 1..8 LOOP
			PERFORM pg_sleep(1);
			BEGIN
				PERFORM kingres.ledger_enable('p6');
				RAISE EXCEPTION 'undo';
			EXCEPTION WHEN OTHERS THEN NULL;
			END;
		END LOOP;
		RAISE NOTICE 'still_alive';
		END $$});
	like($err, $ended, 'review: the DO block with the EXCEPTION loop and the builders\' lock ends the session at the limit');
	unlike($err, qr/still_alive/, 'it did not go on');
	ok($node->wait_for_log(qr/ended the session of role "app": it held the block builders' lock( and the registry's lock)? of the extension for longer than/, $log_start),
		'the log names the builders\' lock (and the registry\'s too, when the limit fired inside the EXCEPTION block that held it)');
	ok(nothing_held(), 'the lock went with the session');

	# The savepoint loop with the verifications' lock.  The registry's lock is
	# taken first, which is the order (L17-04); a ledger_enable after the
	# verifications' lock is refused (t/025), and the session that is left in
	# the aborted transaction is ended at the limit as well.
	($ret, $out, $err) = as_app(q{BEGIN;
		SELECT kingres.ledger_enable('p7') IS NULL;
		SELECT kingres.ledger_verify_recorded() > 0;
		SAVEPOINT s; SELECT kingres.ledger_enable('p8') IS NULL; ROLLBACK TO s; SELECT pg_sleep(1);
		SAVEPOINT s; SELECT kingres.ledger_enable('p8') IS NULL; ROLLBACK TO s; SELECT pg_sleep(1);
		SAVEPOINT s; SELECT kingres.ledger_enable('p8') IS NULL; ROLLBACK TO s; SELECT pg_sleep(1);
		SAVEPOINT s; SELECT kingres.ledger_enable('p8') IS NULL; ROLLBACK TO s; SELECT pg_sleep(1);
		SAVEPOINT s; SELECT kingres.ledger_enable('p8') IS NULL; ROLLBACK TO s; SELECT pg_sleep(1);
		SELECT 'still_alive';
		COMMIT;});
	like($err, $ended, 'review: the loop of savepoints that enter a table, with the verifications\' lock held, ends the session at the limit');
	unlike($out, qr/still_alive/, 'it did not go on');
	ok(nothing_held(), 'nothing is held after it');

	# The loop with ledger_enable alone.
	($ret, $out, $err) = as_app(q{BEGIN;
		SELECT kingres.ledger_enable('p9') IS NULL;
		SAVEPOINT s; SELECT kingres.ledger_enable('p10') IS NULL; ROLLBACK TO s; SELECT pg_sleep(1);
		SAVEPOINT s; SELECT kingres.ledger_enable('p10') IS NULL; ROLLBACK TO s; SELECT pg_sleep(1);
		SAVEPOINT s; SELECT kingres.ledger_enable('p10') IS NULL; ROLLBACK TO s; SELECT pg_sleep(1);
		SAVEPOINT s; SELECT kingres.ledger_enable('p10') IS NULL; ROLLBACK TO s; SELECT pg_sleep(1);
		SAVEPOINT s; SELECT kingres.ledger_enable('p10') IS NULL; ROLLBACK TO s; SELECT pg_sleep(1);
		SELECT 'still_alive';
		COMMIT;});
	like($err, $ended, 'review: the loop with ledger_enable alone ends the session at the limit');
	unlike($out, qr/still_alive/, 'it did not go on');
	ok(nothing_held(), 'nothing is held after it');

	# The statement of the review that takes the verifications' lock first and
	# then enters a table in a savepoint: the order refuses the call (KL006),
	# and what is left, an aborted transaction that holds the lock, is ended at
	# the limit, though the client sends nothing more.
	my $s = sess_open('app', 'd');
	sess_send($s, 'BEGIN');
	sess_send($s, 'SELECT kingres.ledger_verify_recorded() > 0');
	sess_send($s, 'SAVEPOINT s');
	my $r = sess_send($s, q{SELECT kingres.ledger_enable('p11') IS NULL});
	like($r->{err}, qr/ERROR:  cannot take the registry's lock of the ledger extension/,
		'review: ledger_enable after the verifications\' lock is refused by the order of the locks');
	my $gone = 0;
	for (1 .. 150)
	{
		if ($node->safe_psql('d', "SELECT count(*) FROM pg_stat_activity WHERE pid = $s->{pid}") eq '0') { $gone = 1; last; }
		sleep(0.1);
	}
	ok($gone, 'the aborted transaction that holds the verifications\' lock is ended at the limit, though the client sends nothing');
	sess_close($s);
	ok(nothing_held(), 'nothing is held after it');
}

# An error that reaches the main loop while a savepoint is open disables every
# timeout of the backend (PostgresMain calls disable_all_timeouts first), and the
# transaction goes on with the locks of its parent: ROLLBACK TO the savepoint, and
# the role held the lock with no timer.  Found while testing this round; not in the
# review.  The abort of the subtransaction starts the timer again.
{
	my ($ret, $out, $err) = as_app(q{BEGIN;
		SELECT kingres.ledger_verify_recorded() > 0;
		SAVEPOINT s;
		SELECT 1/0;
		ROLLBACK TO s;
		SELECT pg_sleep(60);
		SELECT 'still_alive';});
	like($err, qr/division by zero/, 'an error inside a savepoint that holds nothing of its own');
	like($err, $ended, 'the transaction goes on after ROLLBACK TO, and the limit still ends the session that holds the lock');
	unlike($out, qr/still_alive/, 'it did not go on');
	ok(nothing_held(), 'nothing is held after it');
}

#--------------------------------------------------------------------------
# L17-05: the log line lists the locks held when the session is ended.
#--------------------------------------------------------------------------
$log_start = -s $node->logfile;
{
	my ($ret, $out, $err) = as_app(q{BEGIN;
		SAVEPOINT s1;
		SELECT kingres.ledger_enable('p12') IS NULL;
		SAVEPOINT s2;
		SELECT kingres.ledger_verify_recorded() > 0;
		ROLLBACK TO s2;
		SELECT pg_sleep(10);});
	like($err, $ended, 'L17-05: the session that holds only the registry\'s lock after a savepoint released the other is ended');
	ok($node->wait_for_log(qr/ended the session of role "app": it held the registry's lock of the extension for longer than kingres_ledger\.lock_hold_limit \(3000 ms\)/, $log_start),
		'the log line lists the registry\'s lock and not the verifications\' lock, which was released with the savepoint');
}

#--------------------------------------------------------------------------
# The property test.  Random sequences of SAVEPOINT, RELEASE, ROLLBACK TO, the
# writers and sleeps, against a model of what is held (a stack of savepoints,
# each with the locks taken in it).  After every step: the locks that the
# server says are held are the locks the model says; a session that holds
# nothing is alive; a session that has held a lock for longer than the limit
# (by a bound that the clock proves) is gone, and one that has held it for
# less than the limit is alive.  The steps of a trial are drawn from a seed, so
# a failure is reproduced by it; KL_PROP_SEEDS says how many seeds to run.
#--------------------------------------------------------------------------
my $L = 3000;
set_limit($L);

# The model: levels[0] is the top transaction, levels[k] the k-th open
# savepoint; each has the set of locks taken in it.
sub model_held
{
	my ($levels) = @_;
	my %u;
	for my $l (@$levels) { $u{$_} = 1 for keys %{ $l->{locks} } }
	return join(',', sort { $a <=> $b } keys %u);
}

sub run_block_trial
{
	my ($seed) = @_;
	srand($seed);
	my $s = sess_open('app', 'dp');
	my @levels = ({ name => undef, locks => {} });
	my $armed_send;				# a time before the lock that armed the timer was taken
	my $armed_back;				# a time after it was taken
	my @fail;
	my $sp = 0;
	my $tn = 0;
	my $ops = 8 + int(rand(10));
	my @log;
	my $r = sess_send($s, 'BEGIN');
	for my $step (1 .. $ops)
	{
		my $held = model_held(\@levels);
		my $depth = $#levels;
		my @choices = ('savepoint', 'savepoint', 'sleep', 'sleep', 'verify', 'enable');
		push @choices, ('release', 'rollback', 'rollback') if $depth > 0;
		my $c = $choices[ int(rand(@choices)) ];
		my ($sql, $kind);
		my $now = time();
		my $age_hi = defined $armed_send ? ($now - $armed_send) * 1000 : undef;	# an upper bound of the age
		my $age_lo = defined $armed_back ? ($now - $armed_back) * 1000 : undef;	# a lower bound of the age
		if ($c eq 'enable' && ($held =~ /\b$VERIFY\b/ || $tn >= 55))
		{
			$c = 'sleep';			# the order: the registry's lock comes before the verifications'
		}
		if ($c eq 'sleep')
		{
			my $d;
			if (defined $age_hi)
			{
				# A sleep that surely ends before the limit, or surely not.
				my $left_hi = $L - $age_lo;		# at most this long is left
				my $left_lo = $L - $age_hi;		# at least this long is left
				my @opts;
				push @opts, 0.3 if $left_lo > 1200;
				push @opts, 0.6 if $left_lo > 1500;
				push @opts, ($left_hi / 1000 + 2.5) if 1;		# a sleep that outlasts the limit
				$d = $opts[ int(rand(@opts)) ];
			}
			else
			{
				$d = 0.2;
			}
			$sql = "SELECT pg_sleep($d)";
			$kind = ['sleep', $d];
		}
		elsif ($c eq 'savepoint')
		{
			$sp++;
			$sql = "SAVEPOINT sp$sp";
			$kind = ['savepoint', $sp];
		}
		elsif ($c eq 'release')
		{
			my $k = 1 + int(rand($depth));
			$sql = "RELEASE SAVEPOINT $levels[$k]{name}";
			$kind = ['release', $k];
		}
		elsif ($c eq 'rollback')
		{
			my $k = 1 + int(rand($depth));
			$sql = "ROLLBACK TO SAVEPOINT $levels[$k]{name}";
			$kind = ['rollback', $k];
		}
		elsif ($c eq 'verify')
		{
			$sql = 'SELECT kingres.ledger_verify_recorded() > 0';
			$kind = ['lock', $VERIFY];
		}
		else
		{
			$tn++;
			$sql = "SELECT kingres.ledger_enable('p$tn') IS NULL";
			$kind = ['lock', $REGISTRY];
		}
		push @log, $sql;

		my $t_send = time();
		my $res = sess_send($s, $sql);
		my $t_back = time();

		if ($res->{gone})
		{
			# The session ended during this step.
			my $ok = defined $age_lo && $kind->[0] eq 'sleep'
			  && (($t_back - $armed_send) * 1000 >= $L - 400);
			push @fail, "step $step '$sql': the session ended, the model says it held for less than the limit (" . join('; ', @log) . ")"
			  unless $ok;
			push @fail, "step $step: the process is still there" unless backend_gone($s->{pid});
			last;
		}
		if ($res->{err} =~ /ERROR/)
		{
			push @fail, "step $step '$sql': error $res->{err}";
			last;
		}

		# The model.
		if ($kind->[0] eq 'savepoint')
		{
			push @levels, { name => "sp$kind->[1]", locks => {} };
		}
		elsif ($kind->[0] eq 'release')
		{
			my $k = $kind->[1];
			for my $i ($k .. $#levels)
			{
				$levels[ $k - 1 ]{locks}{$_} = 1 for keys %{ $levels[$i]{locks} };
			}
			splice(@levels, $k);
		}
		elsif ($kind->[0] eq 'rollback')
		{
			my $k = $kind->[1];
			splice(@levels, $k + 1);
			$levels[$k]{locks} = {};
		}
		elsif ($kind->[0] eq 'lock')
		{
			$levels[-1]{locks}{ $kind->[1] } = 1;
		}
		my $held_now = model_held(\@levels);
		if ($held_now ne '')
		{
			if (!defined $armed_send)
			{
				$armed_send = $t_send;
				$armed_back = $t_back;
			}
		}
		else
		{
			$armed_send = $armed_back = undef;
		}

		# After the step: what the server holds is what the model holds, the
		# session is alive, and a session that holds a lock is not older than
		# the limit.
		my $actual = held_by($s->{pid});
		push @fail, "step $step '$sql': the server holds [$actual], the model [$held_now] (" . join('; ', @log) . ")"
		  if $actual ne $held_now;
		my $alive = $node->safe_psql('d', "SELECT count(*) FROM pg_stat_activity WHERE pid = $s->{pid}");
		if ($alive ne '1')
		{
			my $age = defined $armed_send ? int((time() - $armed_send) * 1000) : 'none';
			push @fail, "step $step '$sql': the session is gone, held [$held_now], age at most $age ms (" . join('; ', @log) . ")"
			  unless defined $armed_back && (time() - $armed_back) * 1000 >= $L - 300;
			last;
		}
		if ($held_now ne '' && defined $armed_send && (time() - $armed_send) * 1000 > $L - 500)
		{
			last;				# near the limit: nothing more to claim
		}
	}
	# The end: a rollback leaves the session alive and holding nothing.
	my $alive = $node->safe_psql('d', "SELECT count(*) FROM pg_stat_activity WHERE pid = $s->{pid}");
	if ($alive eq '1')
	{
		my $res = sess_send($s, 'ROLLBACK');
		push @fail, "the session is gone after ROLLBACK" if $res->{gone};
		push @fail, "locks held after ROLLBACK: " . held_by($s->{pid}) unless $res->{gone} || held_by($s->{pid}) eq '';
	}
	sess_close($s);
	return @fail;
}

my $seeds = $ENV{KL_PROP_SEEDS} || 12;
for my $seed (1 .. $seeds)
{
	my @fail = run_block_trial($seed);
	is(scalar(@fail), 0, "property (transaction block), seed $seed: the model and the server agree after every step");
	diag(join("\n", @fail)) if @fail;
}

#--------------------------------------------------------------------------
# The same in one statement (a DO block, where the builders may be called):
# nested blocks that complete or are rolled back, the three writers, sleeps.
# The model has the virtual time of the sleeps; the statement is expected to
# end the session when a lock is held for the limit and more, and to finish
# (with the error that the block ends with, which rolls everything back) when
# every lock is held for less than the limit by the margin that the real cost of
# the steps needs.
#--------------------------------------------------------------------------
sub run_do_trial
{
	my ($seed) = @_;
	srand($seed * 7919);
	my @levels = ({ locks => {}, armed_at => undef });
	my $vt = 0;							# virtual seconds
	my $armed_at;						# virtual time the timer was armed
	my $tn = 0;
	my $max_age = 0;
	my $killed = 0;
	my $text = '';
	my $steps = 6 + int(rand(7));
	my $open = 0;
	for my $step (1 .. $steps)
	{
		last if $killed;
		my $held = model_held(\@levels);
		my @choices = ('begin', 'sleep', 'sleep', 'verify', 'enable', 'builder');
		push @choices, ('end_rollback', 'end_release') if $open > 0;
		my $c = $choices[ int(rand(@choices)) ];
		$c = 'sleep' if $c eq 'enable' && $held =~ /\b$VERIFY\b/;
		$c = 'sleep' if $c eq 'builder' && ($held ne '' || $open > 0);	# a builder is refused in a sub-block (25001)
		$c = 'sleep' if ($c eq 'enable' && $tn >= 55);
		if ($c eq 'begin')
		{
			$text .= "BEGIN\n";
			push @levels, { locks => {} };
			$open++;
		}
		elsif ($c eq 'end_rollback' || $c eq 'end_release')
		{
			if ($c eq 'end_rollback')
			{
				$text .= "RAISE EXCEPTION 'undo';\nEXCEPTION WHEN OTHERS THEN NULL;\nEND;\n";
				pop @levels;
			}
			else
			{
				$text .= "EXCEPTION WHEN OTHERS THEN NULL;\nEND;\n";
				my $l = pop @levels;
				$levels[-1]{locks}{$_} = 1 for keys %{ $l->{locks} };
			}
			$open--;
			$armed_at = undef if model_held(\@levels) eq '';
		}
		elsif ($c eq 'sleep')
		{
			# 0.5 s when there is room, otherwise a long one that outlasts the limit
			my $age = defined $armed_at ? $vt - $armed_at : 0;
			my $d = 0.5;
			if (defined $armed_at)
			{
				if ($age + $d > ($L / 1000) - 1.5 && $age + $d < ($L / 1000) + 0.3)
				{
					$d = ($L / 1000) - $age + 1.0;	# outlasts the limit
				}
				elsif (rand() < 0.15)
				{
					$d = ($L / 1000) - $age + 1.0;
				}
			}
			$text .= "PERFORM pg_sleep($d);\n";
			$vt += $d;
			if (defined $armed_at)
			{
				my $a = $vt - $armed_at;
				$max_age = $a if $a > $max_age;
				$killed = 1 if $a >= ($L / 1000) + 0.2;
			}
		}
		else
		{
			my $lock;
			if ($c eq 'verify')
			{
				$text .= "PERFORM kingres.ledger_verify_recorded();\n";
				$lock = $VERIFY;
			}
			elsif ($c eq 'enable')
			{
				$tn++;
				$text .= "PERFORM kingres.ledger_enable('p$tn');\n";
				$lock = $REGISTRY;
			}
			else
			{
				$text .= "PERFORM kingres.ledger_generate_digest();\n";
				$lock = $BUILDER;
			}
			$levels[-1]{locks}{$lock} = 1;
			$armed_at = $vt unless defined $armed_at;
		}
	}
	return undef if $killed && 0;
	# close the open blocks (completing them) and end with an error that rolls everything back
	if (!$killed)
	{
		while ($open-- > 0) { $text .= "EXCEPTION WHEN OTHERS THEN NULL;\nEND;\n"; }
	}
	else
	{
		$text .= "END;\n" x 0;
		while ($open-- > 0) { $text .= "EXCEPTION WHEN OTHERS THEN NULL;\nEND;\n"; }
	}
	# The verdict the model allows: a lock held for the limit and more ends the
	# session; a lock held for less than the limit by 1.5 s lets the statement finish.
	my $verdict;
	if ($killed) { $verdict = 'killed' }
	elsif ($max_age <= ($L / 1000) - 1.5) { $verdict = 'finished' }
	else { $verdict = 'unclear' }
	return ($text, $verdict);
}

sub do_trial
{
	my ($seed) = @_;
	my ($text, $verdict);
	for my $try (0 .. 40)
	{
		($text, $verdict) = run_do_trial($seed + 1000 * $try);
		last if $verdict ne 'unclear';
	}
	my $stmt = "DO \$\$ BEGIN\n$text RAISE EXCEPTION 'the end';\nEND \$\$";
	my ($ret, $out, $err) = as_app($stmt, 'dp');
	my $res = $err =~ /FATAL:  terminating connection due to administrator command/ ? 'killed'
	  : $err =~ /the end/ ? 'finished' : "other: $err";
	is($res, $verdict, "property (one statement), seed $seed: the model says $verdict")
	  or diag($stmt . "\n" . $err);
	ok(nothing_held(), "seed $seed: nothing is held after it");
	return;
}

my $do_seeds = $ENV{KL_PROP_DO_SEEDS} || 8;
do_trial($_) for 1 .. $do_seeds;

done_testing();
