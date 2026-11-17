# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/022_hold_bounds.pl
#
# The edges of the limit on holding a lock (kingres_ledger.lock_hold_limit)
# that the fourth review of the guard found:
#
#   L15-02  PREPARE TRANSACTION switched the timer off and the prepared
#           transaction kept the registry's and the verifications' locks for ever.
#           A transaction of a role that is not a superuser that holds a lock of
#           the extension cannot be prepared; a superuser's can, and the lock is
#           then held like any lock of a prepared transaction.
#   L15-07  The timer was not switched off when a savepoint that took the last
#           lock was rolled back, so a session that held nothing was ended.  The
#           locks are tracked per subtransaction.
#   L15-12  The session that the limit ends gets a line in the server log that
#           names the role, the lock and the limit.
#   L15-06  The timer is switched off at the end of the transaction (a session
#           that called a function once and then stays connected is not ended),
#           also by COMMIT AND CHAIN and ROLLBACK AND CHAIN, and a chained
#           transaction that takes a lock again is limited again.
#
# Other ways a lock or the timer could outlive what bounds it, and why they do
# not: a WITH HOLD cursor holds no lock after the commit (tested); the code takes
# no advisory lock (nothing to test: see below that no session-level lock of the
# extension exists after any case); the functions are PARALLEL UNSAFE, so a
# parallel worker never takes a lock for its leader (tested through pg_proc).
#
# The limit is 2 s for the role "app"; the sessions that must survive sleep for
# 3 s, which is longer than the limit, so a timer that was not switched off ends
# them whatever the speed of the machine.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time);

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
	my ($ret, $out, $err) = $node->psql('d', $sql, extra_params => [ '-U', 'app' ]);
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

#--------------------------------------------------------------------------
# L15-02: PREPARE TRANSACTION
#--------------------------------------------------------------------------
{
	my ($ret, $out, $err) = as_app("BEGIN;\nSELECT kingres.ledger_verify_recorded() > 0;\nPREPARE TRANSACTION 'p_verify';");
	like($err, qr/cannot PREPARE a transaction that holds the verifications' lock of the ledger extension/,
		'a role that is not a superuser cannot prepare a transaction that holds the verifications\' lock');
	like($err, qr/DETAIL:.*lock_hold_limit/, 'the error says why');
	is(prepared_count(), '0', 'nothing was prepared');
	is(nothing_held(), 'nothing held', 'and no lock is left: every other writer is served');

	($ret, $out, $err) = as_app("BEGIN;\nSELECT kingres.ledger_enable('apptab');\nPREPARE TRANSACTION 'p_enable';");
	like($err, qr/cannot PREPARE a transaction that holds the registry's lock of the ledger extension/,
		'nor one that holds the registry\'s lock (ledger_enable)');
	is(prepared_count(), '0', 'nothing was prepared');
	is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_table WHERE relid = 'apptab'::regclass}), '0',
		'the registration is gone with the transaction');
	is(nothing_held(), 'nothing held', 'no lock is left');

	# What stays allowed: a transaction of that role that holds no lock of the extension.
	($ret, $out, $err) = as_app("BEGIN;\nUPDATE apptab SET id = id;\nPREPARE TRANSACTION 'p_plain';");
	is($ret, 0, 'the same role can prepare a transaction that holds no lock of the extension');
	is(prepared_count(), '1', 'it is prepared');
	$node->safe_psql('d', q{COMMIT PREPARED 'p_plain'});

	# A savepoint rolled back first: the lock went with it, so the PREPARE is allowed.
	($ret, $out, $err) = as_app("BEGIN;\nSAVEPOINT s;\nSELECT kingres.ledger_verify_recorded() > 0;\nROLLBACK TO s;\nPREPARE TRANSACTION 'p_after_sp';");
	is($ret, 0, 'a transaction whose lock was released with a savepoint can be prepared');
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
# L15-07: subtransactions; and the timer at the end of every kind of transaction
#--------------------------------------------------------------------------
my $log_start = -s $node->logfile;
{
	my ($ret, $out, $err) = as_app("SELECT kingres.ledger_verify_recorded() > 0;\nSELECT pg_sleep(3);\nSELECT 'still_here';");
	is($ret, 0, 'a call that committed, and a session that stays connected past the limit: not ended');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("BEGIN;\nSAVEPOINT s;\nSELECT kingres.ledger_verify_recorded() > 0;\nROLLBACK TO s;\nSELECT pg_sleep(3);\nSELECT 'still_here';\nCOMMIT;");
	is($ret, 0, 'L15-07: a savepoint that took the lock is rolled back: the lock is gone, the session is not ended');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("BEGIN;\nSAVEPOINT a;\nSAVEPOINT b;\nSELECT kingres.ledger_verify_recorded() > 0;\nRELEASE b;\nROLLBACK TO a;\nSELECT pg_sleep(3);\nSELECT 'still_here';\nCOMMIT;");
	is($ret, 0, 'a lock taken in a released savepoint belongs to its parent and goes with it');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("BEGIN;\nDO \$\$ BEGIN BEGIN PERFORM kingres.ledger_verify_recorded(); RAISE EXCEPTION 'x'; EXCEPTION WHEN OTHERS THEN NULL; END; END \$\$;\nSELECT pg_sleep(3);\nSELECT 'still_here';\nCOMMIT;");
	is($ret, 0, 'an EXCEPTION block that took the lock and failed: the lock is gone, the session is not ended');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("BEGIN;\nSELECT kingres.ledger_verify_recorded() > 0;\nCOMMIT AND CHAIN;\nSELECT pg_sleep(3);\nSELECT 'still_here';\nCOMMIT;");
	is($ret, 0, 'COMMIT AND CHAIN: the chained transaction holds nothing, the timer is off');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("BEGIN;\nSELECT kingres.ledger_verify_recorded() > 0;\nROLLBACK AND CHAIN;\nSELECT pg_sleep(3);\nSELECT 'still_here';\nCOMMIT;");
	is($ret, 0, 'ROLLBACK AND CHAIN: the same');
	like($out, qr/still_here/, 'the session went on');

	($ret, $out, $err) = as_app("BEGIN;\nDECLARE c CURSOR WITH HOLD FOR SELECT 1;\nSELECT kingres.ledger_verify_recorded() > 0;\nCOMMIT;\nSELECT pg_sleep(3);\nFETCH c;\nSELECT 'still_here';");
	is($ret, 0, 'a WITH HOLD cursor that outlives the transaction holds no lock of the extension: not ended');
	like($out, qr/still_here/, 'the session went on');
	is(lock_count($VERIFY), '0', 'no lock of the extension is held by it');
}

#--------------------------------------------------------------------------
# The cases that must still be ended (controls), and the log line (L15-12)
#--------------------------------------------------------------------------
{
	my ($ret, $out, $err) = as_app("BEGIN;\nSAVEPOINT a;\nSELECT kingres.ledger_verify_recorded() > 0;\nSAVEPOINT b;\nSELECT kingres.ledger_verify_recorded() > 0;\nROLLBACK TO b;\nSELECT pg_sleep(60);\nSELECT 'not_reached';");
	like($err, $ended, 'a lock that is still held by the outer savepoint after an inner one is rolled back: the session is ended');
	unlike($out, qr/not_reached/, 'before it went on');
	ok($node->wait_for_log(qr/kingres_ledger: ended the session of role "app": it held the verifications' lock of the extension for longer than kingres_ledger\.lock_hold_limit \(2000 ms\)/, $log_start),
		'L15-12: the server log says that the limit did it: the role, the lock and the limit');
	is(nothing_held(), 'nothing held', 'the lock went with the session');

	$log_start = -s $node->logfile;
	($ret, $out, $err) = as_app("BEGIN;\nSELECT kingres.ledger_verify_recorded() > 0;\nCOMMIT AND CHAIN;\nSELECT kingres.ledger_verify_recorded() > 0;\nSELECT pg_sleep(60);\nSELECT 'not_reached';");
	like($err, $ended, 'a chained transaction that takes a lock again is limited again');
	unlike($out, qr/not_reached/, 'before it went on');
	ok($node->wait_for_log(qr/kingres_ledger: ended the session of role "app"/, $log_start), 'and the log says so');

	$log_start = -s $node->logfile;
	($ret, $out, $err) = as_app("BEGIN;\nSELECT kingres.ledger_enable('apptab2');\nSELECT kingres.ledger_verify_recorded() > 0;\nSELECT pg_sleep(60);");
	like($err, $ended, 'ledger_enable and a recorded verification together: ended');
	ok($node->wait_for_log(qr/it held the registry's lock and the verifications' lock of the extension for longer than/, $log_start),
		'the log line names both locks');
	is(nothing_held(), 'nothing held', 'both locks went');
}

#--------------------------------------------------------------------------
# Work that makes progress gets a new period (L15-06): a table that takes longer
# to enter than the whole limit is entered, and the session is not ended.  The
# precondition is part of the test: the call must have lasted longer than the
# limit, or nothing was shown.
#--------------------------------------------------------------------------
$node->safe_psql('postgres', q{CREATE DATABASE d2});
$node->safe_psql('d2', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO app;
	GRANT EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) TO app;
	CREATE TABLE big AS SELECT g AS id, repeat('x', 20) AS body FROM generate_series(1, 300000) g;
	ALTER TABLE big OWNER TO app;
	ALTER ROLE app IN DATABASE d2 SET kingres_ledger.lock_hold_limit = '1s';
});
{
	my $t0 = time();
	my ($ret, $out, $err) = $node->psql('d2', q{SELECT kingres.ledger_enable('big') IS NULL}, extra_params => [ '-U', 'app' ]);
	my $took = time() - $t0;
	cmp_ok($took, '>', 1, "precondition: entering 300000 rows took longer than the limit of 1 s ($took s)");
	is($ret, 0, 'a ledger_enable that takes longer than the limit but makes progress is not ended') or diag($err);
	is($node->safe_psql('d2', q{SELECT count(*) FROM kingres.ledger_entry}), '300000', 'and every row was entered');
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
