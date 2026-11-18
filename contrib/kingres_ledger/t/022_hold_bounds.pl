# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/022_hold_bounds.pl
#
# The edges of the limit on holding a lock (kingres_ledger.lock_hold_limit),
# as they are since a role that is not a superuser takes a lock of the extension
# only in a statement of its own (L19-01, t/027):
#
#   L15-02  A prepared transaction keeps its locks until somebody ends it.  A
#           role that is not a superuser cannot hold a lock in a transaction
#           block, so it cannot prepare a transaction that holds one: the call
#           is refused (25001) and nothing is prepared.  A superuser may, and the
#           lock is then held like any lock of a prepared transaction.
#   L15-12  The session that the limit ends gets a line in the server log that
#           names the role, the lock and the limit.
#   L15-06  The timer is switched off at the end of the transaction (a session
#           that called a function once and then stays connected is not ended),
#           and a call that was refused leaves no timer behind.
#
# Other ways a lock or the timer could outlive what bounds it, and why they do
# not: a WITH HOLD cursor holds no lock after the commit (tested); the code takes
# no advisory lock (nothing to test: see below that no session-level lock of the
# extension exists after any case); the functions are PARALLEL UNSAFE, so a
# parallel worker never takes a lock for its leader (tested through pg_proc).
#
# The limit is 2 s for the role "app"; the sessions that must survive sleep for
# 3 s, which is longer than the limit, so a timer that was not switched off ends
# them whatever the speed of the machine.  The cases of the earlier versions of
# this file that held a lock across statements (a savepoint that took it and was
# rolled back, COMMIT AND CHAIN after it, two locks in a transaction block) have
# no statement left to run: they are refusals, in t/027.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('hold_bounds');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "max_prepared_transactions = 10\n");
$node->start;

my $BUILDER = 19276;			# 0x4B4C
my $REGISTRY = 19277;			# 0x4B4D
my $VERIFY = 19278;				# 0x4B4E

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO app;
	GRANT EXECUTE ON FUNCTION kingres.ledger_verify_recorded(bigint, bigint),
		kingres.ledger_enable(regclass, text) TO app;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a');
	SELECT kingres.ledger_build_block();
	CREATE TABLE apptab (id int);
	ALTER TABLE apptab OWNER TO app;
	CREATE TABLE apptab2 (id int);
	ALTER TABLE apptab2 OWNER TO app;
	ALTER ROLE app SET kingres_ledger.lock_hold_limit = '2s';
});

sub as_app
{
	my ($sql) = @_;
	my ($ret, $out, $err) = $node->psql('d', $sql, extra_params => [ '-U', 'app' ], on_error_stop => 0);
	return ($ret, $out, $err);
}

sub lock_count
{
	my ($subid) = @_;
	return $node->safe_psql('d', qq{SELECT count(*) FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid = $subid});
}

sub prepared_count
{
	return $node->safe_psql('d', q{SELECT count(*) FROM pg_prepared_xacts});
}

# Every lock of the extension is gone, and a superuser's recorded verification
# is served at once (a lock_timeout that is generous decides nothing but the
# failure of a test where a lock is never let go).
sub nothing_held
{
	my $held = $node->safe_psql('d', q{SELECT count(*) FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid > 0});
	my ($ret, $out, $err) = $node->psql('d', q{SET lock_timeout = '60s';
		SELECT kingres.ledger_verify_recorded() > 0});
	return ($held eq '0' && $ret == 0 && $out =~ /^t$/m) ? 'nothing held' : "held $held, ret $ret: $err";
}

my $ended = qr/FATAL:  terminating connection due to administrator command/;
my $refused = qr/cannot run inside a transaction block for a role that is not a superuser/;

#--------------------------------------------------------------------------
# L15-02: PREPARE TRANSACTION
#--------------------------------------------------------------------------
{
	my ($ret, $out, $err) = as_app("BEGIN;\nSELECT kingres.ledger_verify_recorded() > 0;\nPREPARE TRANSACTION 'p_verify';");
	like($err, qr/ledger_verify_recorded $refused/,
		'a role that is not a superuser cannot take the verifications\' lock in a transaction it could prepare');
	is(prepared_count(), '0', 'nothing was prepared');
	is(nothing_held(), 'nothing held', 'and no lock is left: every other writer is served');

	($ret, $out, $err) = as_app("BEGIN;\nSELECT kingres.ledger_enable('apptab');\nPREPARE TRANSACTION 'p_enable';");
	like($err, qr/ledger_enable $refused/, 'nor the registry\'s lock (ledger_enable)');
	is(prepared_count(), '0', 'nothing was prepared');
	is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_table WHERE relid = 'apptab'::regclass}), '0',
		'the registration is gone with the transaction');
	is(nothing_held(), 'nothing held', 'no lock is left');

	# What stays allowed: a transaction of that role that holds no lock of the extension.
	($ret, $out, $err) = as_app("BEGIN;\nUPDATE apptab SET id = id;\nPREPARE TRANSACTION 'p_plain';");
	is($ret, 0, 'the same role can prepare a transaction that holds no lock of the extension');
	is(prepared_count(), '1', 'it is prepared');
	$node->safe_psql('d', q{COMMIT PREPARED 'p_plain'});

	# A refused call in a savepoint, then a PREPARE: nothing was taken, so it is allowed.
	($ret, $out, $err) = as_app("BEGIN;\nSAVEPOINT s;\nSELECT kingres.ledger_verify_recorded() > 0;\nROLLBACK TO s;\nPREPARE TRANSACTION 'p_after_sp';");
	is($ret, 0, 'a transaction whose call was refused holds nothing and can be prepared');
	$node->safe_psql('d', q{ROLLBACK PREPARED 'p_after_sp'});
	is(nothing_held(), 'nothing held', 'and nothing is held when it is rolled back');
}
{
	# A superuser is not limited and may prepare: the lock is then held until the
	# prepared transaction ends, as any lock of a prepared transaction (README).
	my ($ret, $out, $err) = $node->psql('d', "BEGIN;\nSELECT kingres.ledger_verify_recorded() > 0;\nPREPARE TRANSACTION 'p_super';");
	is($ret, 0, 'a superuser can prepare a transaction that holds a lock of the extension');
	is($node->safe_psql('d', qq{SELECT count(*) FROM pg_locks WHERE locktype = 'object'
		AND classid = 'pg_extension'::regclass AND objsubid = $VERIFY AND granted AND pid IS NULL}), '1',
		'the prepared transaction holds the lock');
	$node->safe_psql('d', q{COMMIT PREPARED 'p_super'});
	is(lock_count($VERIFY), '0', 'which goes when it is committed');
}

#--------------------------------------------------------------------------
# L15-06: the timer at the end of every kind of transaction, and after a
# refusal.  Each session below stays connected for 3 s, longer than the limit.
#--------------------------------------------------------------------------
my $log_start = -s $node->logfile;
{
	my ($ret, $out, $err) = as_app("SELECT kingres.ledger_verify_recorded() > 0;\nSELECT pg_sleep(3);\nSELECT 'still_here';");
	is($ret, 0, 'a call that committed, and a session that stays connected past the limit: not ended');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("BEGIN;\nSAVEPOINT s;\nSELECT kingres.ledger_verify_recorded() > 0;\nROLLBACK TO s;\nSELECT pg_sleep(3);\nSELECT 'still_here';\nCOMMIT;");
	like($err, qr/ledger_verify_recorded $refused/, 'a call in a savepoint is refused');
	is($ret, 0, 'and the refusal leaves no timer behind: the session is not ended');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("BEGIN;\nDO \$\$ BEGIN BEGIN PERFORM kingres.ledger_verify_recorded(); RAISE EXCEPTION 'x'; EXCEPTION WHEN OTHERS THEN NULL; END; END \$\$;\nSELECT pg_sleep(3);\nSELECT 'still_here';\nCOMMIT;");
	is($ret, 0, 'a call in an EXCEPTION block is refused, and leaves no timer behind');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("SELECT kingres.ledger_verify_recorded() > 0;\nBEGIN;\nCOMMIT AND CHAIN;\nSELECT pg_sleep(3);\nSELECT 'still_here';\nCOMMIT;");
	is($ret, 0, 'COMMIT AND CHAIN after a call that committed: nothing is held, the timer is off');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("SELECT kingres.ledger_verify_recorded() > 0;\nBEGIN;\nROLLBACK AND CHAIN;\nSELECT pg_sleep(3);\nSELECT 'still_here';\nCOMMIT;");
	is($ret, 0, 'ROLLBACK AND CHAIN: the same');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("BEGIN;\nDECLARE c CURSOR WITH HOLD FOR SELECT 1;\nCOMMIT;\nSELECT kingres.ledger_verify_recorded() > 0;\nSELECT pg_sleep(3);\nFETCH c;\nSELECT 'still_here';");
	is($ret, 0, 'a WITH HOLD cursor that outlives the transaction holds no lock of the extension: not ended');
	like($out, qr/still_here/, 'the session went on');
	is(lock_count($VERIFY), '0', 'no lock of the extension is held by it');
}

#--------------------------------------------------------------------------
# The cases that must still be ended (controls), and the log line (L15-12): a
# statement that holds a lock for longer than the limit.
#--------------------------------------------------------------------------
{
	my ($ret, $out, $err) = as_app("SELECT kingres.ledger_verify_recorded() > 0, pg_sleep(60);\nSELECT 'not_reached';");
	like($err, $ended, 'a statement that holds the verifications\' lock past the limit: the session is ended');
	unlike($out, qr/not_reached/, 'before it went on');
	ok($node->wait_for_log(qr/kingres_ledger: ended the session of role "app": it held the verifications' lock of the extension for longer than kingres_ledger\.lock_hold_limit \(2000 ms\)/, $log_start),
		'L15-12: the server log says that the limit did it: the role, the lock and the limit');
	ok($node->wait_for_log(qr/HINT:  A role that is not a superuser may hold a lock of the extension for the time of one statement only/, $log_start),
		'L19-07: and what to do about it');
	is(nothing_held(), 'nothing held', 'the lock went with the session');

	$log_start = -s $node->logfile;
	($ret, $out, $err) = as_app("SELECT kingres.ledger_enable('apptab2'), kingres.ledger_verify_recorded() > 0, pg_sleep(60);\nSELECT 'not_reached';");
	like($err, $ended, 'ledger_enable and a recorded verification together, held past the limit: ended');
	ok($node->wait_for_log(qr/it held the registry's lock and the verifications' lock of the extension for longer than/, $log_start),
		'the log line names both locks');
	is(nothing_held(), 'nothing held', 'both locks went');
	is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_table WHERE relid = 'apptab2'::regclass}), '0',
		'and the registration that was not committed is gone');
}

#--------------------------------------------------------------------------
# Nothing in the code can run in a parallel worker
#--------------------------------------------------------------------------
is($node->safe_psql('d', q{SELECT string_agg(proname || '=' || proparallel::text, ',' ORDER BY proname)
	FROM pg_proc WHERE pronamespace = 'kingres'::regnamespace
	AND proname IN ('ledger_build_block', 'ledger_generate_digest', 'ledger_verify_recorded',
	                'ledger_remove_stray', 'ledger_enable', 'ledger_lock_table', 'ledger_register_table')}),
	'ledger_build_block=u,ledger_enable=u,ledger_generate_digest=u,ledger_lock_table=u,ledger_register_table=u,ledger_remove_stray=u,ledger_verify_recorded=u',
	'every function that takes a lock of the extension is PARALLEL UNSAFE');

done_testing();
