# contrib/kingres_ledger/t/010_review2.pl
#
# Reproductions of the findings of the second review that need workers, more
# than one server or the shell (kingres/docs/reviews/2026-11-14-kingres-ledger-0.2.md),
# and what was found out about the areas the review did not examine:
#
#   L2-03   settings that a database owner attaches to the database do not stop
#           the block-building worker; a worker that fails is retried with a
#           growing delay and shows up in ledger_status
#   L2-06   the launcher does not visit a database without the extension at
#           every sweep, does not leak, and a worker that does not finish does
#           not keep the other databases waiting
#   L2-08   a verification that is cancelled is recorded
#   not examined: max_worker_processes used up, logical replication apply
#           (conflicts, TRUNCATE) into an updatable table, pg_dump and restore
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(usleep);

# Sessions of this test must not be hurt by the settings it attaches to the
# databases: client options win over the settings of a database.
my $client_options = join(' ',
	'-c transaction_timeout=0',
	'-c idle_in_transaction_session_timeout=0',
	'-c idle_session_timeout=0',
	'-c statement_timeout=0',
	'-c lock_timeout=0',
	'-c default_transaction_read_only=off',
	'-c default_transaction_isolation=read\\ committed',
	'-c default_transaction_deferrable=off',
	'-c search_path=public,pg_catalog',
	'-c synchronous_commit=local',
	'-c standard_conforming_strings=on');

# Wait until the query returns the expected text; the seconds are the limit.
sub wait_for
{
	my ($node, $db, $sql, $expected, $seconds) = @_;
	my $got = '';
	for (my $i = 0; $i < $seconds * 4; $i++)
	{
		$got = $node->safe_psql($db, $sql);
		return 1 if $got eq $expected;
		usleep(250_000);
	}
	diag("wait_for: last answer of [$sql] was [$got], expected [$expected]");
	return 0;
}

sub new_node
{
	my ($name, $conf) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init(allows_streaming => 'logical');
	$node->append_conf('postgresql.conf', $conf);
	$node->start;
	return $node;
}

sub log_count
{
	my ($node, $re) = @_;
	my $log = slurp_file($node->logfile);
	my @m = ($log =~ /$re/g);
	return scalar @m;
}

#==========================================================================
# L2-03: database settings, a failing worker, its delay and its report
#==========================================================================
my $h = new_node('hostile', q(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = '300ms'
max_prepared_transactions = 5
));
$ENV{PGOPTIONS} = $client_options;

$h->safe_psql('postgres', q(CREATE ROLE dbo LOGIN));
$h->safe_psql('postgres', q(CREATE DATABASE w1 OWNER dbo));
$h->safe_psql('w1', q(
	CREATE EXTENSION kingres_ledger;
	GRANT CREATE ON SCHEMA public TO dbo;
	CREATE TABLE public.t (id serial PRIMARY KEY, v text);
	SELECT kingres.ledger_enable('public.t');
	CREATE TABLE public.u (id int PRIMARY KEY, v text);
	SELECT kingres.ledger_enable('public.u', 'updatable');
));
ok(wait_for($h, 'w1', 'SELECT count(*) FROM kingres.ledger_pending', '0', 30),
	'the enabling transactions are built into blocks');

# Every one of these makes the worker fail or stall when it is not overridden.
my @hostile = (
	[ 'transaction_timeout',                  '1ms' ],
	[ 'default_transaction_read_only',        'on' ],
	[ 'idle_in_transaction_session_timeout',  '1ms' ],
	[ 'default_transaction_isolation',        'serializable' ],
	[ 'default_transaction_deferrable',       'on' ],
	[ 'statement_timeout',                    '1ms' ],
	[ 'lock_timeout',                         '1ms' ],
	[ 'search_path',                          "''" ],
	[ 'row_security',                         'on' ],
	[ 'check_function_bodies',                'off' ],
	[ 'standard_conforming_strings',          'off' ],
	[ 'plpgsql.extra_errors',                 "'all'" ],
	[ 'plpgsql.extra_warnings',               "'all'" ],
	[ 'plpgsql.variable_conflict',            'use_column' ],
	[ 'jit',                                  'on' ],
	[ 'jit_above_cost',                       '0' ],
	[ 'temp_tablespaces',                     "'no_such_tablespace'" ],
	[ 'debug_parallel_query',                 'on' ],
	[ 'extra_float_digits',                   '-5' ],
	[ 'bytea_output',                         "'escape'" ],
	[ 'DateStyle',                            "'SQL, DMY'" ],
	[ 'IntervalStyle',                        "'sql_standard'" ],
	[ 'TimeZone',                             "'Asia/Kolkata'" ],
	[ 'client_encoding',                      "'LATIN1'" ],
);
# PostgreSQL 19 forces standard_conforming_strings on: a database cannot be
# set to "off" any more, so that case exists on 18 only.
@hostile = grep { !($_->[0] eq 'standard_conforming_strings' && $h->pg_version->major >= 19) } @hostile;
my $n = 0;
my $super = $h->safe_psql('postgres', 'SELECT current_user');
foreach my $s (@hostile)
{
	my ($name, $value) = @$s;
	$n++;
	(my $plain = $value) =~ s/^'(.*)'$/$1/;
	$h->safe_psql('w1', qq(DO \$\$ BEGIN NULL; END \$\$; ALTER DATABASE w1 SET $name = '$plain'), extra_params => [ '-U', ($name eq 'plpgsql.variable_conflict' ? $super : 'dbo') ]);
	$h->safe_psql('w1', qq(INSERT INTO public.t (v) VALUES ('$name')));
	$h->safe_psql('w1', qq(INSERT INTO public.u VALUES ($n, 'a'); UPDATE public.u SET v = 'b' WHERE id = $n; DELETE FROM public.u WHERE id = $n));
	ok(wait_for($h, 'w1', 'SELECT count(*) FROM kingres.ledger_pending', '0', 20),
		"with $name = $value on the database the transactions are built into a block");
	$h->safe_psql('w1', qq(DO \$\$ BEGIN NULL; END \$\$; ALTER DATABASE w1 RESET $name), extra_params => [ '-U', ($name eq 'plpgsql.variable_conflict' ? $super : 'dbo') ]);
}
is($h->safe_psql('w1', q(SELECT worker_state || ' ' || worker_consecutive_failures FROM kingres.ledger_status)),
	'ok 0', 'the worker of the database reports ok and no failure');
is($h->safe_psql('w1', q(SELECT count(*) FROM kingres.ledger_verify())),
	'0', 'and the ledger verifies');

# A worker that fails (the identity of the chain is missing, which a
# privileged user can bring about with catalog edits): one line in the log
# per attempt, a delay that grows with every failure, and the state in
# ledger_status; when the cause is gone the next attempt builds the block.
$h->safe_psql('postgres', q(CREATE DATABASE fx));
$h->safe_psql('fx', q(
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.t (id int);
	SELECT kingres.ledger_enable('public.t');
));
ok(wait_for($h, 'fx', 'SELECT count(*) FROM kingres.ledger_pending', '0', 30),
	'fx: the enabling transaction is built');
$h->safe_psql('fx', q(UPDATE pg_trigger SET tgenabled = 'D' WHERE tgrelid = 'kingres.ledger_chain'::regclass));
my $chain = $h->safe_psql('fx', q(SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain));
$h->safe_psql('fx', q(SET kingres_ledger.allow_manual_write = on; DELETE FROM kingres.ledger_chain));
my $fails_before = log_count($h, qr/the block worker for database \d+ failed/);
$h->safe_psql('fx', q(INSERT INTO public.t VALUES (1)));
sleep 14;
my $consecutive = $h->safe_psql('fx', q(SELECT worker_consecutive_failures FROM kingres.ledger_status));
cmp_ok($consecutive, '>=', 2, 'a failing worker is retried');
cmp_ok($consecutive, '<=', 8, 'but with a growing delay: at an interval of 0.3 s there would be about 45 attempts in 14 s');
is($h->safe_psql('fx', q(SELECT worker_state FROM kingres.ledger_status)),
	'failed', 'ledger_status shows the failure');
like($h->safe_psql('fx', q(SELECT worker_last_error FROM kingres.ledger_status)),
	qr/identity of the chain is missing/, 'with the error of the last attempt');
ok($h->safe_psql('fx', q(SELECT worker_retry_after > now() FROM kingres.ledger_status)) eq 't',
	'and when the next attempt is made');
my $fails = log_count($h, qr/the block worker for database \d+ failed/) - $fails_before;
cmp_ok($fails, '<=', 8, 'the log has one line per attempt and not one per interval');
is(log_count($h, qr/exited with exit code/), 0, 'no worker exited with an error code');
$h->safe_psql('fx', qq(SET kingres_ledger.allow_manual_write = on; INSERT INTO kingres.ledger_chain (chain_id) VALUES (decode('$chain', 'hex'))));
ok(wait_for($h, 'fx', 'SELECT count(*) FROM kingres.ledger_pending', '0', 90),
	'when the cause is gone the transaction is built by the next attempt');
is($h->safe_psql('fx', q(SELECT worker_state || ' ' || worker_consecutive_failures FROM kingres.ledger_status)),
	'ok 0', 'and the state is ok again');
$h->safe_psql('fx', q(UPDATE pg_trigger SET tgenabled = 'A' WHERE tgrelid = 'kingres.ledger_chain'::regclass));

#==========================================================================
# L2-08.2: a cancelled verification is recorded
#==========================================================================
$h->safe_psql('w1', q(INSERT INTO public.t (v) SELECT 'row ' || g FROM generate_series(1, 60000) g));
ok(wait_for($h, 'w1', 'SELECT count(*) FROM kingres.ledger_pending', '0', 60), 'w1: 60000 rows are built');
my $before_runs = $h->safe_psql('w1', q(SELECT count(*) FROM kingres.ledger_verification));
my ($ret, $out, $err) = $h->psql('w1', q(
	SET statement_timeout = '40ms';
	SELECT kingres.ledger_verify_recorded();
));
is($ret, 0, 'a cancelled run of ledger_verify_recorded returns');
like($err, qr/ledger verification \d+ failed: canceling statement due to statement timeout/,
	'with a warning');
is($h->safe_psql('w1', q(SELECT count(*) FROM kingres.ledger_verification)) - $before_runs,
	1, 'and one record');
is($h->safe_psql('w1', q(SELECT state || ' ' || (problems -> 0 ->> 'sqlstate') FROM kingres.ledger_verification ORDER BY verification_id DESC LIMIT 1)),
	'failed 57014', 'which says failed and the SQLSTATE of the cancellation');
$h->stop;

#==========================================================================
# L2-06: the launcher
#==========================================================================
my $l = new_node('launcher', q(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = '50ms'
kingres_ledger.worker_timeout = '1500ms'
log_min_messages = debug1
max_prepared_transactions = 5
synchronous_standby_names = 'nobody'
synchronous_commit = local
));
$ENV{PGOPTIONS} = $client_options;
my $empties = 40;
$l->safe_psql('postgres', "CREATE DATABASE e$_") for (1 .. $empties);
foreach my $db ('a', 'b')
{
	$l->safe_psql('postgres', "CREATE DATABASE $db");
	$l->safe_psql($db, q(
		CREATE EXTENSION kingres_ledger;
		CREATE TABLE public.t (id int);
		SELECT kingres.ledger_enable('public.t');
	));
}
ok(wait_for($l, 'a', 'SELECT count(*) FROM kingres.ledger_pending', '0', 60), 'a is built');
ok(wait_for($l, 'b', 'SELECT count(*) FROM kingres.ledger_pending', '0', 60), 'b is built');

# A database without the extension is not visited at every sweep.
my $e1 = $l->safe_psql('postgres', q(SELECT oid FROM pg_database WHERE datname = 'e1'));
my $visits0 = log_count($l, qr/starting background worker process "kingres_ledger block worker for database $e1"/);
sleep 20;
my $visits = log_count($l, qr/starting background worker process "kingres_ledger block worker for database $e1"/) - $visits0;
cmp_ok($visits, '<=', 12, "a database without the extension was visited $visits times in 20 s (every sweep would be about 60)");

# The memory of the launcher does not grow with the rounds (a dirty database
# makes a round look at the list of all databases).
sub launcher_memory
{
	my ($node) = @_;
	my $pid = $node->safe_psql('postgres',
		q(SELECT pid FROM pg_stat_activity WHERE backend_type = 'kingres_ledger launcher'));
	my $mark = -s $node->logfile;
	$node->safe_psql('postgres', "SELECT pg_log_backend_memory_contexts($pid)");
	for (1 .. 40)
	{
		my $log = slurp_file($node->logfile);
		$log = substr($log, $mark);
		return $1 if $log =~ /Grand total: (\d+) bytes/;
		usleep(250_000);
	}
	return -1;
}
my $busy = 0;
my $pump = sub {
	for (1 .. 150)
	{
		$l->safe_psql('a', q(INSERT INTO public.t VALUES (1)));
	}
};
$pump->();
my $m0 = launcher_memory($l);
$pump->() for (1 .. 3);
my $m1 = launcher_memory($l);
cmp_ok($m1 - $m0, "<", 25_000, "the memory of the launcher did not grow with the rounds ($m0 -> $m1 bytes)");

# A worker that does not finish keeps the others waiting for at most the
# worker timeout: the commit of the worker of database a waits for a
# synchronous standby that does not exist.
$l->safe_psql('postgres', q(ALTER DATABASE a SET synchronous_commit = on));
ok(wait_for($l, 'a', 'SELECT count(*) FROM kingres.ledger_pending', '0', 30), 'a is empty before the test');
$l->safe_psql('a', q(INSERT INTO public.t VALUES (100)));
$l->safe_psql('b', q(INSERT INTO public.t VALUES (100)));
ok(wait_for($l, 'b', 'SELECT count(*) FROM kingres.ledger_pending', '0', 25),
	'database b is built although the worker of database a waits for a standby that never comes');
ok(wait_for($l, 'a', 'SELECT count(*) FROM kingres.ledger_pending', '0', 25),
	'and the worker of database a was cancelled after the worker timeout and its block is there');
ok(wait_for($l, 'a', q(SELECT worker_state FROM kingres.ledger_status), 'ok', 10), 'a reports ok');
is($l->safe_psql('b', q(SELECT count(*) FROM kingres.ledger_verify())), '0', 'b verifies');
$l->stop;

#==========================================================================
# Not examined by the review: max_worker_processes used up
#==========================================================================
my $m = new_node('slots', q(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = '200ms'
max_worker_processes = 1
max_parallel_workers = 0
max_logical_replication_workers = 0
max_parallel_workers_per_gather = 0
));
$ENV{PGOPTIONS} = $client_options;
$m->safe_psql('postgres', q(
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.t (id int);
	SELECT kingres.ledger_enable('public.t');
));
$m->safe_psql('postgres', q(INSERT INTO public.t VALUES (1)));
sleep 3;
is($m->safe_psql('postgres', q(SELECT count(*) FROM kingres.ledger_pending)), '1',
	'with no worker slot free for the workers the transaction stays pending');
like(slurp_file($m->logfile), qr/no background worker slot free for database/, 'the launcher says why');
cmp_ok(log_count($m, qr/no background worker slot free for database/), '<=', 2,
	'once, and not at every interval');
is($m->safe_psql('postgres', q(SELECT kingres.ledger_generate_digest() IS NOT NULL)), 't',
	'a call of ledger_generate_digest builds the block all the same');
is($m->safe_psql('postgres', q(SELECT count(*) FROM kingres.ledger_pending)), '0', 'and nothing is pending');
$m->stop;

#==========================================================================
# Not examined by the review: logical replication apply into an updatable table
#==========================================================================
my $pub = new_node('pub', q(wal_level = logical));
my $sub = new_node('sub', q(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = 0
));
$ENV{PGOPTIONS} = $client_options;
$pub->safe_psql('postgres', q(
	CREATE TABLE public.lr (id int PRIMARY KEY, v text);
	INSERT INTO public.lr VALUES (1, 'one'), (2, 'two'), (3, 'three');
	CREATE PUBLICATION pl FOR TABLE public.lr;
));
$sub->safe_psql('postgres', q(
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.lr (id int PRIMARY KEY, v text);
	SELECT kingres.ledger_enable('public.lr', 'updatable');
));
my $pubconn = $pub->connstr('postgres');
$pubconn =~ s/'/''/g;
$sub->safe_psql('postgres', "CREATE SUBSCRIPTION sl CONNECTION '$pubconn' PUBLICATION pl");
$sub->wait_for_subscription_sync($pub, 'sl');
is($sub->safe_psql('postgres', q(SELECT count(*) FROM public.lr)), '3', 'the initial copy arrived');
$pub->safe_psql('postgres', q(UPDATE public.lr SET v = 'TWO' WHERE id = 2; DELETE FROM public.lr WHERE id = 3; INSERT INTO public.lr VALUES (4, 'four')));
$pub->wait_for_catchup('sl');
is($sub->safe_psql('postgres', q(SELECT string_agg(v, ',' ORDER BY id) FROM public.lr)), 'one,TWO,four', 'changes are applied');
is($sub->safe_psql('postgres', q(SELECT string_agg(v, ',' ORDER BY ledger_seq) FROM public.lr_ledger_history)), 'two,three', 'and the old versions are in the history table');
is($sub->safe_psql('postgres', q(SELECT count(*) FROM kingres.ledger_verify())), '0', 'the ledger verifies');
# conflict path: the row that the publisher updates is gone on the subscriber
$sub->safe_psql('postgres', q(DELETE FROM public.lr WHERE id = 1));
$pub->safe_psql('postgres', q(UPDATE public.lr SET v = 'ONE' WHERE id = 1));
$pub->wait_for_catchup('sl');
is($sub->safe_psql('postgres', q(SELECT count(*) FROM public.lr WHERE id = 1)), '0', 'an update of a missing row is skipped');
is($sub->safe_psql('postgres', q(SELECT count(*) FROM kingres.ledger_verify())), '0', 'and the ledger still verifies');
# TRUNCATE is replicated and rejected by the ledger: the apply worker fails
# and retries; the subscription must be disabled to stop it.
$pub->safe_psql('postgres', q(TRUNCATE public.lr));
my $deadline = 0;
until (slurp_file($sub->logfile) =~ /updatable ledger table and cannot be truncated/ || $deadline++ > 40)
{
	usleep(250_000);
}
like(slurp_file($sub->logfile), qr/table "lr" is an updatable ledger table and cannot be truncated/,
	'a replicated TRUNCATE is rejected by the ledger and stops the apply of the subscription');
$sub->safe_psql('postgres', q(ALTER SUBSCRIPTION sl DISABLE));
is($sub->safe_psql('postgres', q(SELECT count(*) FROM kingres.ledger_verify())), '0', 'nothing was changed meanwhile');
# a ledger table can be published, and so can its history table
$sub->safe_psql('postgres', q(CREATE PUBLICATION ph FOR TABLE public.lr, public.lr_ledger_history));
is($sub->safe_psql('postgres', q(SELECT count(*) FROM pg_publication_tables WHERE pubname = 'ph')), '2',
	'a publication can name a ledger table and its history table');
$sub->safe_psql('postgres', q(ALTER SUBSCRIPTION sl SET (slot_name = NONE)));
$sub->safe_psql('postgres', q(DROP SUBSCRIPTION sl));
$sub->stop;
$pub->stop;

#==========================================================================
# Not examined by the review: pg_dump and restore
#==========================================================================
my $d = new_node('dump', q(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = 0
));
$ENV{PGOPTIONS} = $client_options;
$d->safe_psql('postgres', q(CREATE DATABASE src));
$d->safe_psql('src', q(
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.ao (id int PRIMARY KEY, v text);
	CREATE TABLE public.up (id int PRIMARY KEY, v text);
	SELECT kingres.ledger_enable('public.ao');
	SELECT kingres.ledger_enable('public.up', 'updatable');
	INSERT INTO public.ao VALUES (1, 'a'), (2, 'b');
	INSERT INTO public.up VALUES (1, 'a'), (2, 'b');
	UPDATE public.up SET v = 'c' WHERE id = 1;
	SELECT kingres.ledger_build_block();
));
my $dump = $d->basedir . '/src.sql';
command_ok([ 'pg_dump', '--no-sync', '-f', $dump, '-d', $d->connstr('src') ], 'pg_dump of a database with ledger tables works');
$d->safe_psql('postgres', q(CREATE DATABASE dst));
my ($rout, $rerr) = run_command([ 'psql', '-X', '-q', '-d', $d->connstr('dst'), '-f', $dump ]);
diag("restore stderr: $rerr") if $rerr;
is($d->safe_psql('dst', q(SELECT count(*) FROM public.ao)) . ',' . $d->safe_psql('dst', q(SELECT count(*) FROM public.up)) . ',' .
	$d->safe_psql('dst', q(SELECT count(*) FROM public.up_ledger_history)),
	'2,2,1', 'the rows of the tables and of the history table are restored');
is($d->safe_psql('dst', q(SELECT count(*) FROM kingres.ledger_table)), '0',
	'but the registry of the ledger is empty in the restored database');
cmp_ok($d->safe_psql('dst', q(SELECT count(*) FROM kingres.ledger_verify() WHERE check_name = 'trigger')),
	'>=', 2, 'ledger_verify reports the capture triggers of tables that are not registered');
my ($iret, $iout, $ierr) = $d->psql('dst', q(INSERT INTO public.ao VALUES (3, 'c')));
isnt($iret, 0, 'an insert into a restored ledger table fails (it is not entered in a ledger that does not know it)');
like($ierr, qr/ledger_entry_table_id_fkey|violates foreign key/, 'because the entry refers to a table id that is not registered');
$d->stop;

done_testing();
