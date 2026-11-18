# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/025_lock_order.pl
#
# L17-04 of the fifth review of the guard: the order of the locks of the
# extension (a lock on the table of ledger_enable, then the builders', the
# registry's, the verifications', the removals') held inside each function and
# not across calls, so two honest callers that called two writers in one
# statement in opposite orders deadlocked, and one of them got 40P01 after
# deadlock_timeout.  Now a call that would take a lock that is earlier in the
# order than one the transaction holds, and does not hold it already, fails at
# once with an error of the extension (SQLSTATE KL006, a hint that names the
# order), before it waits.
#
# The pairs of the review run in two sessions that are made to overlap by the
# test and not by a fixed sleep: the first session is started, the test waits
# until it holds its first lock (pg_locks), then the second is started.  The
# first fails with KL006 at the call that would be out of order; the second
# completes; neither is a deadlock.  Each pair runs three times.
#
# What is not refused, because it cannot deadlock or because it is the order:
# every sequence that the extension itself and its documented callers use (the
# block builder of the worker takes one lock; a write by hand holds all four,
# so it may be followed by any call; ledger_remove_stray takes all four;
# ledger_enable of several tables in one transaction takes the registry's lock
# once).  The table lock of ledger_enable against DML after a write by hand
# cannot be ordered this way (the README states it); the last test shows the
# deadlock detector finding it, as the README says.
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

my $node = PostgreSQL::Test::Cluster->new('lock_order');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "deadlock_timeout = 1s\n");
$node->start;

my $psql = $node->installed_command('psql');
my $BUILDER = 19276;			# 0x4B4C
my $REGISTRY = 19277;			# 0x4B4D
my $VERIFY = 19278;				# 0x4B4E
my $REMOVED = 19279;			# 0x4B4F

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO app;
	GRANT EXECUTE ON FUNCTION kingres.ledger_generate_digest(), kingres.ledger_build_block(),
		kingres.ledger_verify_recorded(bigint, bigint), kingres.ledger_enable(regclass, text) TO app;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a');
	SELECT kingres.ledger_build_block();
});
for my $i (1 .. 40)
{
	$node->safe_psql('d', "CREATE TABLE a$i (id int); ALTER TABLE a$i OWNER TO app; CREATE TABLE b$i (id int); ALTER TABLE b$i OWNER TO app");
}

# A statement run in its own psql process, in the background.
sub start_sql
{
	my ($user, $sql, $prefix) = @_;
	my %p = (out => '', err => '');
	$p{h} = IPC::Run::start(
		[ $psql, '-XAtq', '-v', 'ON_ERROR_STOP=0', '-d', $node->connstr('d') . " user=$user", '-c', ($prefix // '') . $sql ],
		'>' => \$p{out}, '2>' => \$p{err}, IPC::Run::timeout(120));
	return \%p;
}

sub finish_sql
{
	my ($p) = @_;
	$p->{h}->finish;
	return $p;
}

# Wait until a backend holds the lock (granted) with this sub-object number.
sub wait_holds
{
	my ($subid) = @_;
	return $node->poll_query_until('d', qq{SELECT count(*) >= 1 FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass
		  AND objsubid = $subid AND granted});
}

sub nothing_held
{
	return $node->poll_query_until('d', q{SELECT count(*) = 0 FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid > 0});
}

# Two overlapping sessions: the first is started and given time to hold its
# first lock, then the second.
sub pair
{
	my ($name, $first_user, $first_sql, $first_lock, $second_user, $second_sql, $second_prefix) = @_;
	my $p1 = start_sql($first_user, $first_sql);
	wait_holds($first_lock) or BAIL_OUT("$name: the first session never held its lock");
	my $p2 = start_sql($second_user, $second_sql, $second_prefix);
	finish_sql($p1);
	finish_sql($p2);
	return ($p1, $p2);
}

my $round = 0;
for my $r (1 .. 3)
{
	$round = $r;

	# 1. the verifications' lock then the builders', against the builders' then the verifications'
	my ($p1, $p2) = pair("pair 1 round $r",
		'app', q{SELECT kingres.ledger_verify_recorded() > 0, pg_sleep(2), kingres.ledger_generate_digest() IS NOT NULL}, $VERIFY,
		'app', q{SELECT kingres.ledger_generate_digest() IS NOT NULL, pg_sleep(2), kingres.ledger_verify_recorded() > 0});
	like($p1->{err}, qr/ERROR:  cannot take the block builders' lock of the ledger extension: this transaction already holds the verifications' lock/,
		"pair 1, round $r: the session that would take the builders' lock after the verifications' fails at once with the error of the extension");
	like($p1->{err}, qr/HINT:  The order is: the lock on the table of ledger_enable, the block builders' lock, the registry's lock, the verifications' lock, the removals' lock/,
		'and the hint names the order');
	is($p2->{err}, '', "pair 1, round $r: the other session completes without an error");
	unlike($p1->{err} . $p2->{err}, qr/deadlock detected/, 'no deadlock');
	ok(nothing_held(), 'nothing is held after it');

	# 2. the registry's lock (ledger_enable) then the builders', against the reverse
	($p1, $p2) = pair("pair 2 round $r",
		'app', qq{SELECT kingres.ledger_enable('a$r') IS NULL, pg_sleep(2), kingres.ledger_build_block() IS NULL}, $REGISTRY,
		'app', qq{SELECT kingres.ledger_build_block() IS NULL, pg_sleep(2), kingres.ledger_enable('b$r') IS NULL});
	like($p1->{err}, qr/ERROR:  cannot take the block builders' lock of the ledger extension: this transaction already holds the registry's lock/,
		"pair 2, round $r: ledger_enable and then a builder: refused at once");
	is($p2->{err}, '', "pair 2, round $r: the other session completes");
	unlike($p1->{err} . $p2->{err}, qr/deadlock detected/, 'no deadlock');
	is($node->safe_psql('d', "SELECT count(*) FROM kingres.ledger_table WHERE relid = 'b$r'::regclass"), '1',
		'the other session enabled its table');
	is($node->safe_psql('d', "SELECT count(*) FROM kingres.ledger_table WHERE relid = 'a$r'::regclass"), '0',
		'the refused one did not (its transaction was rolled back)');
	ok(nothing_held(), 'nothing is held after it');

	# 3. a write by hand (all four locks, in the order) against a transaction that took the
	#    verifications' lock before the builders'
	($p1, $p2) = pair("pair 3 round $r",
		'app', q{SELECT kingres.ledger_verify_recorded() > 0, pg_sleep(2), kingres.ledger_generate_digest() IS NOT NULL}, $VERIFY,
		$node->safe_psql('postgres', 'SELECT current_user'),
		q{INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
		  SELECT now(), 'hand', b.height, b.block_hash, '{}'::jsonb
		    FROM kingres.ledger_blocks b ORDER BY b.height DESC LIMIT 1},
		'SET kingres_ledger.allow_manual_write = on; ');
	like($p1->{err}, qr/ERROR:  cannot take the block builders' lock of the ledger extension/,
		"pair 3, round $r: the session that holds the verifications' lock and asks for the builders' is refused at once");
	is($p2->{err}, '', "pair 3, round $r: the hand-made row is written");
	unlike($p1->{err} . $p2->{err}, qr/deadlock detected/, 'no deadlock');
	ok(nothing_held(), 'nothing is held after it');
}

#--------------------------------------------------------------------------
# The error: its SQLSTATE, in a block that is caught and in a transaction.
#--------------------------------------------------------------------------
{
	my ($ret, $out, $err) = $node->psql('d', q{DO $$ BEGIN
		PERFORM kingres.ledger_verify_recorded();
		PERFORM kingres.ledger_enable('a10');
		EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'sqlstate=%', SQLSTATE;
		END $$});
	like($err, qr/NOTICE:  sqlstate=KL006/, 'the error has an SQLSTATE of its own, KL006 (a superuser: a role that is not one cannot call a writer inside an EXCEPTION block, L19-01)');

	my @pairs = (
		[ 'verifications then registry', q{kingres.ledger_verify_recorded(), kingres.ledger_enable('a11')} ],
		[ 'registry then builders',      q{kingres.ledger_enable('a12'), kingres.ledger_generate_digest()} ],
		[ 'verifications then builders', q{kingres.ledger_verify_recorded(), kingres.ledger_generate_digest()} ],
	);
	for my $p (@pairs)
	{
		my $t0 = time();
		($ret, $out, $err) = $node->psql('d', "SELECT $p->[1]", extra_params => [ '-U', 'app' ]);
		my $took = time() - $t0;
		like($err, qr/ERROR:  cannot take the .* of the ledger extension/, "$p->[0]: refused, with no other session involved");
		cmp_ok($took, '<', 5, 'and at once (no wait, no deadlock timeout)');
	}
	# The lock is asked for in the order across calls of one transaction too (a superuser: a role that
	# is not one cannot hold a lock across statements, L19-01).
	($ret, $out, $err) = $node->psql('d', qq{BEGIN;\nSELECT kingres.ledger_verify_recorded() > 0;\nSELECT kingres.ledger_enable('a13') IS NULL;\nSELECT kingres.ledger_enable('a14') IS NULL;\nCOMMIT;},
		extra_params => []);
	unlike($err, qr/ERROR:  cannot take the registry's lock.*\n.*ERROR:  cannot take/s, 'one error only');
	like($err, qr/ERROR:  cannot take the registry's lock of the ledger extension: this transaction already holds the verifications' lock/,
		'a registry call after the verifications\' lock, in a transaction block, is refused');
	# Asking again for a lock that is held is not out of order.
	($ret, $out, $err) = $node->psql('d', qq{BEGIN;\nSELECT kingres.ledger_enable('a15') IS NULL;\nSELECT kingres.ledger_enable('a16') IS NULL;\nSELECT kingres.ledger_verify_recorded() > 0;\nSELECT kingres.ledger_verify_recorded() > 0;\nCOMMIT;},
		extra_params => []);
	is($ret, 0, 'the registry\'s lock asked for again, and the verifications\' lock asked for again, are not refused') or diag($err);
	unlike($err, qr/cannot take/, 'no error');
	# A subtransaction that took a later lock and was rolled back leaves no trace.
	($ret, $out, $err) = $node->psql('d', qq{BEGIN;\nSAVEPOINT s;\nSELECT kingres.ledger_verify_recorded() > 0;\nROLLBACK TO s;\nSELECT kingres.ledger_enable('a17') IS NULL;\nCOMMIT;},
		extra_params => []);
	is($ret, 0, 'a lock that was taken in a savepoint that was rolled back is not held: the order is not violated') or diag($err);
	unlike($err, qr/cannot take/, 'no error');
}

#--------------------------------------------------------------------------
# Sequences that must not be refused (the extension's own and its documented
# callers').  A superuser holds no limit; the order is the same for it.
#--------------------------------------------------------------------------
{
	my $super = $node->safe_psql('postgres', 'SELECT current_user');
	my $run = sub {
		my ($what, $sql) = @_;
		my ($ret, $out, $err) = $node->psql('d', $sql);
		is($ret, 0, $what) or diag($err);
		unlike($err, qr/cannot take|ERROR/, "$what: no error");
	};
	$node->safe_psql('d', q{CREATE TABLE s1 (id int); CREATE TABLE s2 (id int); CREATE TABLE s3 (id int);
		CREATE TABLE s4 (id int); CREATE TABLE s5 (id int); CREATE TABLE s6 (id int)});
	$run->('block builder, digest, enable, recorded verification in this order, in one transaction',
		qq{BEGIN;\nSELECT kingres.ledger_build_block();\nSELECT kingres.ledger_generate_digest() IS NULL;\nSELECT kingres.ledger_enable('s1') IS NULL;\nSELECT kingres.ledger_verify_recorded() > 0;\nCOMMIT;});
	$run->('ledger_enable of two tables and a recorded verification',
		qq{BEGIN;\nSELECT kingres.ledger_enable('s2') IS NULL;\nSELECT kingres.ledger_enable('s3') IS NULL;\nSELECT kingres.ledger_verify_recorded() > 0;\nCOMMIT;});
	$run->('a write by hand, then every writer',
		qq{SET kingres_ledger.allow_manual_write = on;\nBEGIN;\nINSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest) SELECT now(), 'hand', b.height, b.block_hash, '{}'::jsonb FROM kingres.ledger_blocks b ORDER BY b.height DESC LIMIT 1;\nSELECT kingres.ledger_generate_digest() IS NULL;\nSELECT kingres.ledger_enable('s4') IS NULL;\nSELECT kingres.ledger_verify_recorded() > 0;\nSELECT kingres.ledger_build_block();\nCOMMIT;});
	$run->('ledger_remove_stray, then every writer',
		qq{SET kingres_ledger.allow_manual_write = on;\nBEGIN;\nSELECT kingres.ledger_remove_stray('a test');\nSELECT kingres.ledger_generate_digest() IS NULL;\nSELECT kingres.ledger_enable('s6') IS NULL;\nSELECT kingres.ledger_verify_recorded() > 0;\nCOMMIT;});
	# The worker takes the builders' lock alone, as a statement of its own (t/009 runs it).
	$run->('the builder alone, in a statement of its own, as the worker calls it', q{SELECT kingres.ledger_build_block()});
}

#--------------------------------------------------------------------------
# What cannot be ordered: the lock on the table of ledger_enable against DML
# after a write by hand (the README states it).  The deadlock detector finds it
# and one of the two gets 40P01; nothing hangs.
#--------------------------------------------------------------------------
{
	$node->safe_psql('d', q{CREATE TABLE pc (id int); ALTER TABLE pc OWNER TO app});
	my $super = $node->safe_psql('postgres', 'SELECT current_user');
	my $p1 = start_sql($super, q{BEGIN;
		INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
		  SELECT now(), 'hand', b.height, b.block_hash, '{}'::jsonb FROM kingres.ledger_blocks b ORDER BY b.height DESC LIMIT 1;
		SELECT pg_sleep(2);
		INSERT INTO pc VALUES (1);
		COMMIT;}, 'SET kingres_ledger.allow_manual_write = on; ');
	wait_holds($REGISTRY) or BAIL_OUT('the hand write never held the registry lock');
	my $p2 = start_sql('app', q{SELECT kingres.ledger_enable('pc') IS NULL});
	finish_sql($p1);
	finish_sql($p2);
	my $deadlocks = () = ($p1->{err} . $p2->{err}) =~ /deadlock detected/g;
	is($deadlocks, 1, 'the table lock cannot be ordered: the deadlock detector aborts one of the two (as the README says), and nothing hangs');
}

done_testing();
