# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/029_enable_credit.pl
#
# L19-02 of the sixth review of the guard, and the rules that stayed of the
# renewals of L17-01.  ledger_enable of a table that holds many rows enters
# them in batches of a thousand, and the time a batch really took is added to the
# period of kingres_ledger.lock_hold_limit, so that an honest large table is not
# ended by the limit.  Two rules are left (the rest went with the locks that a
# role could hold across statements):
#
#   (a) the time is credited only while the registry's lock is the only lock of the
#       extension that the transaction holds: with the builders' lock held a large
#       table would keep the block builder and the capture waiting;
#   (b) the total that one transaction may be credited is bounded by
#       kingres_ledger.enable_credit_limit, a superuser's setting, so that a loop
#       that creates and enables table after table does not hold the registry's
#       lock for as long as the role likes (L19-02).  The call that passes the bound
#       fails with an error that says so; a table that needs longer is enabled by a
#       superuser.
#
# No measured duration decides anything here.  "A large table" is made by the
# developer setting kingres_ledger.enable_batch_delay (a superuser's, set for the
# role with ALTER ROLE): ledger_register_table waits at least that long after each
# batch, whatever the load of the machine, inside the time that the batch is
# credited for.  Eight batches of 600 ms are at least 4.8 s against a limit of
# 3 s, so the statement that completes could not have completed without the credit,
# and the statement that was ended would have completed with it.  The cases that
# need the registry's lock held at a known point use a role whose batch delay is
# a minute and whose limit is longer, and a cancel at the end.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('enable_credit');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my $BUILDER = 19276;
my $REGISTRY = 19277;
my $super = $node->safe_psql('postgres', 'SELECT current_user');

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE ROLE app2 LOGIN; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO app, app2;
	GRANT EXECUTE ON FUNCTION kingres.ledger_build_block(), kingres.ledger_generate_digest(),
		kingres.ledger_verify_recorded(bigint, bigint), kingres.ledger_enable(regclass, text) TO app, app2;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a');
	SELECT kingres.ledger_build_block();
	-- the role of the credit cases: a limit of 3 s, a wait of 600 ms after each batch
	ALTER ROLE app SET kingres_ledger.lock_hold_limit = '3s';
	ALTER ROLE app SET kingres_ledger.enable_batch_delay = '600ms';
	-- the role that holds the registry's lock at a known point: a long limit, a wait of a minute
	ALTER ROLE app2 SET kingres_ledger.lock_hold_limit = '300s';
	ALTER ROLE app2 SET kingres_ledger.enable_batch_delay = '60s';
});
my $ntab = 0;
sub big_table
{
	my ($rows, $owner) = @_;
	my $t = 'big' . (++$ntab);
	$node->safe_psql('d', "CREATE TABLE $t AS SELECT g AS id, repeat('x', 20) AS body FROM generate_series(1, $rows) g;
		ALTER TABLE $t OWNER TO " . ($owner // 'app'));
	return $t;
}

sub as_role
{
	my ($role, $sql, @more) = @_;
	my ($ret, $out, $err) = $node->psql('d', $sql, extra_params => [ '-U', $role, @more ]);
	return ($ret, $out, $err);
}

sub nothing_held
{
	return $node->safe_psql('d', q{SELECT count(*) FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid > 0}) eq '0';
}

sub entered
{
	my ($t) = @_;
	return $node->safe_psql('d', qq{SELECT count(*) FROM kingres.ledger_entry e
		JOIN kingres.ledger_table lt ON lt.table_id = e.table_id WHERE lt.relid = '$t'::regclass});
}

sub registered
{
	my ($t) = @_;
	return $node->safe_psql('d', qq{SELECT count(*) FROM kingres.ledger_table WHERE relid = '$t'::regclass});
}

my $ended = qr/FATAL:  terminating connection due to administrator command/;

#--------------------------------------------------------------------------
# The credit: a ledger_enable that takes longer than the limit, and makes
# progress, is not ended.
#--------------------------------------------------------------------------
{
	my $t = big_table(8000);
	my ($ret, $out, $err) = as_role('app', "SELECT kingres.ledger_enable('$t') IS NULL");
	is($ret, 0, 'a ledger_enable of eight batches that wait 600 ms each (at least 4.8 s) with a limit of 3 s completes: the time of the batches is credited') or diag($err);
	unlike($err, $ended, 'and the session is not ended');
	is(entered($t), '8000', 'every row was entered');
	ok(nothing_held(), 'and nothing is held afterwards');
}

#--------------------------------------------------------------------------
# Rule (a): no credit while another lock of the extension is held.  The
# builders' lock is taken, then the same kind of table is entered: ended.
#--------------------------------------------------------------------------
{
	my $t = big_table(8000);
	my $log_start = -s $node->logfile;
	my ($ret, $out, $err) = as_role('app', qq{DO \$\$ BEGIN
		PERFORM kingres.ledger_generate_digest();
		PERFORM kingres.ledger_enable('$t');
		RAISE NOTICE 'still_alive';
		END \$\$});
	like($err, $ended, 'rule (a): the time of the batches is not credited while the builders\' lock is held: the session is ended');
	unlike($err, qr/still_alive/, 'it did not go on');
	ok($node->wait_for_log(qr/ended the session of role "app": it held the block builders' lock and the registry's lock of the extension for longer than/, $log_start),
		'the log names both locks');
	ok($node->wait_for_log(qr/HINT:  .*kingres_ledger\.enable_credit_limit/, $log_start),
		'L19-07: and the hint says what to do about a large table');
	is(registered($t), '0', 'the registration was rolled back with the session');
	ok(nothing_held(), 'nothing is held afterwards');
}

#--------------------------------------------------------------------------
# Rule (b): the total of the credit of one transaction is bounded.  A limit
# of 2 s of credit: the batches (at least 600 ms each) pass it at the fourth
# batch at the latest, and the call fails with the error that says so.
#--------------------------------------------------------------------------
{
	$node->safe_psql('d', q{ALTER ROLE app SET kingres_ledger.enable_credit_limit = '2s'});
	my $t = big_table(8000);
	my ($ret, $out, $err) = as_role('app', "SELECT kingres.ledger_enable('$t') IS NULL");
	like($err, qr/ERROR:  entering the rows of the tables of this transaction took longer than kingres_ledger\.enable_credit_limit \(2000 ms\) allows a role that is not a superuser/,
		'rule (b): a table that needs more than the credit limit fails with an error that says why');
	like($err, qr/HINT:  A table that needs longer is enabled by a superuser/, 'and says what to do');
	unlike($err, $ended, 'the session is not ended: the transaction is');
	is(registered($t), '0', 'the registration was rolled back');
	is(entered($t), '0', 'and no row was entered');
	ok(nothing_held(), 'nothing is held afterwards');

	# The loop of the review: a table after a table in one statement.
	my $before = $node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_table});
	($ret, $out, $err) = as_role('app', q{DO $$ DECLARE i int; BEGIN
		FOR i IN 1..12 LOOP
			EXECUTE format('CREATE TABLE public.lp%s AS SELECT g AS id FROM generate_series(1, 2000) g', i);
			PERFORM kingres.ledger_enable(format('public.lp%s', i)::regclass);
		END LOOP;
		RAISE NOTICE 'loop_done';
		END $$});
	like($err, qr/ERROR:  entering the rows of the tables of this transaction took longer than kingres_ledger\.enable_credit_limit/,
		'the loop that creates and enables table after table is bounded by the credit limit');
	unlike($err, qr/loop_done/, 'and it did not run to its end');
	is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_table}), $before, 'every table of the loop was rolled back');
	ok(nothing_held(), 'nothing is held afterwards');

	# The controls: no bound, and a bound that is not reached.
	$node->safe_psql('d', q{ALTER ROLE app SET kingres_ledger.enable_credit_limit = 0});
	$t = big_table(8000);
	($ret, $out, $err) = as_role('app', "SELECT kingres.ledger_enable('$t') IS NULL");
	is($ret, 0, 'control: with no bound (0) the same table is entered') or diag($err);
	is(entered($t), '8000', 'and every row is there');
	$node->safe_psql('d', q{ALTER ROLE app SET kingres_ledger.enable_credit_limit = '60s'});
	$t = big_table(8000);
	($ret, $out, $err) = as_role('app', "SELECT kingres.ledger_enable('$t') IS NULL");
	is($ret, 0, 'control: with a bound that is not reached the same table is entered') or diag($err);
	$node->safe_psql('d', q{ALTER ROLE app RESET kingres_ledger.enable_credit_limit});
}

#--------------------------------------------------------------------------
# Who sets the settings, and who is not bounded.
#--------------------------------------------------------------------------
{
	foreach my $guc (qw(kingres_ledger.enable_credit_limit kingres_ledger.enable_batch_delay))
	{
		my ($ret, $out, $err) = as_role('app', "SET $guc = 0");
		like($err, qr/permission denied to set parameter "\Q$guc\E"/, "the role that is bounded cannot set $guc");
	}
	is($node->safe_psql('d', q{SHOW kingres_ledger.enable_credit_limit}), '30min', 'the default of the credit limit is 30 minutes');

	# A superuser is not bounded by the credit limit, and is not delayed
	# (the delay is the role's setting).
	my $t = big_table(3000);
	my ($ret, $out, $err) = $node->psql('d', "SET kingres_ledger.enable_credit_limit = 1; SELECT kingres.ledger_enable('$t') IS NULL");
	is($ret, 0, 'a superuser is outside the credit limit') or diag($err);
	is(entered($t), '3000', 'and the rows are entered');
}

#--------------------------------------------------------------------------
# What the registry's lock held by a role during a large entering blocks, and
# what it does not (the README states it).  The role app2 waits a minute after
# the first batch, holding the registry's lock: the gate.  Another ledger_enable
# waits for it; the block builder and the capture do not.
#--------------------------------------------------------------------------
{
	my $t = big_table(1500, 'app2');
	my $t2 = big_table(10, 'app');
	my $v;
	{
		local $ENV{PGUSER} = 'app2';
		$v = $node->background_psql('d', on_error_stop => 0);
	}
	my $victim_pid = $v->query_safe('SELECT pg_backend_pid()');
	$victim_pid =~ s/\s+//g;
	$v->query_until(qr/entering/, "\\echo entering\nSELECT kingres.ledger_enable('$t') IS NULL;\n");
	ok($node->poll_query_until('d', qq{SELECT count(*) = 1 FROM pg_locks WHERE pid = $victim_pid AND locktype = 'object'
		AND classid = 'pg_extension'::regclass AND objsubid = $REGISTRY AND granted}),
		'the role holds the registry\'s lock while it enters the rows (and waits at the gate)');

	my ($ret, $out, $err) = $node->psql('d', qq{SET lock_timeout = '1s'; SELECT kingres.ledger_enable('$t2')});
	like($err, qr/lock timeout/, 'another ledger_enable waits for it');
	($ret, $out, $err) = $node->psql('d', q{SET lock_timeout = '1s'; INSERT INTO docs VALUES (2, 'b'); SELECT kingres.ledger_build_block() IS NOT NULL});
	is($err, '', 'the capture and the block builder are not stopped by it');
	like($out, qr/^t$/m, 'a block was built');

	$node->safe_psql('d', "SELECT pg_cancel_backend($victim_pid)");
	ok($node->poll_query_until('d', q{SELECT count(*) = 0 FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid > 0}),
		'a cancel ends the wait, and the lock goes');
	eval { $v->quit };
}

is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify() WHERE check_name NOT IN ('operator_removal', 'manual_write')}),
	'0', 'the ledger verifies');

done_testing();
