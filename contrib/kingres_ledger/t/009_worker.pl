# contrib/kingres_ledger/t/009_worker.pl
#
# The block-building workers.
#
#   * a committed transaction ends up in a block within a bounded time
#     without any call, in every database that has the extension (two
#     databases are both served), and a database without the extension and
#     a database with nothing pending cost nothing;
#   * kingres_ledger.block_interval = 0 switches the workers off, a reload
#     with another value switches them on again;
#   * the workers survive a restart (what was pending is picked up), and
#     pick up a prepared transaction that is committed later;
#   * a block bounded by max_block_transactions is followed by more blocks
#     until nothing is pending;
#   * a worker that finds the builder lock taken gives up quietly and the
#     next round builds the block;
#   * a standby runs no worker and does not build; after promotion the
#     promoted server builds blocks.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: PostgreSQL

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $primary = PostgreSQL::Test::Cluster->new('primary');
$primary->init(allows_streaming => 1);
$primary->append_conf(
	'postgresql.conf', qq(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = '500ms'
max_prepared_transactions = 5
max_worker_processes = 16
));
$primary->start;

my $setup = q(
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.w (id int PRIMARY KEY, v text);
	SELECT kingres.ledger_enable('public.w');
);
foreach my $db ('db1', 'db2', 'plain')
{
	$primary->safe_psql('postgres', "CREATE DATABASE $db");
}
$primary->safe_psql('db1', $setup);
$primary->safe_psql('db2', $setup);
# db1 also has an updatable table.
$primary->safe_psql('db1', q(
	CREATE TABLE public.u (id int PRIMARY KEY, v text);
	SELECT kingres.ledger_enable('public.u', 'updatable');
));

# Wait for the enabling transactions to be in a block, so that what follows
# starts from nothing pending.
foreach my $db ('db1', 'db2')
{
	$primary->poll_query_until($db,
		q(SELECT count(*) = 0 FROM kingres.ledger_pending), 't')
	  or die "the enabling transaction of $db was never built into a block";
}

my $launcher = $primary->safe_psql('postgres',
	q(SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'kingres_ledger launcher'));
is($launcher, '1', 'one launcher runs');

#--------------------------------------------------------------------------
# A committed transaction is in a block, without any call; both databases.
#--------------------------------------------------------------------------
my %before;
foreach my $db ('db1', 'db2')
{
	$before{$db} = $primary->safe_psql($db, 'SELECT count(*) FROM kingres.ledger_blocks');
	$primary->safe_psql($db, q(INSERT INTO public.w VALUES (1, 'a')));
}
$primary->safe_psql('db1', q(INSERT INTO public.u VALUES (1, 'a'); UPDATE public.u SET v = 'b'; DELETE FROM public.u));
foreach my $db ('db1', 'db2')
{
	ok( $primary->poll_query_until($db,
			q(SELECT count(*) = 0 FROM kingres.ledger_pending), 't'),
		"$db: the pending transactions were built into a block without a call");
	cmp_ok($primary->safe_psql($db, 'SELECT count(*) FROM kingres.ledger_blocks'),
		'>', $before{$db}, "$db: a block exists");
	is($primary->safe_psql($db, 'SELECT count(*) FROM kingres.ledger_verify()'),
		'0', "$db: the ledger verifies");
}
is($primary->safe_psql('db1', 'SELECT count(*) FROM public.u_ledger_history'),
	'2', 'db1: the updatable table was captured and built');

# A database without the extension, and the log: nothing went wrong.
my $log = slurp_file($primary->logfile);
unlike($log, qr/kingres_ledger.*ERROR|ERROR.*kingres_ledger/, 'no error in the log from the workers');

#--------------------------------------------------------------------------
# block_interval = 0 switches the workers off; a reload switches them on.
#--------------------------------------------------------------------------
$primary->safe_psql('postgres', q(ALTER SYSTEM SET kingres_ledger.block_interval = 0));
$primary->reload;
sleep 2;
my $height = $primary->safe_psql('db1', 'SELECT coalesce(max(height), -1) FROM kingres.ledger_blocks');
$primary->safe_psql('db1', q(INSERT INTO public.w VALUES (2, 'b')));
sleep 3;
is($primary->safe_psql('db1', 'SELECT count(*) FROM kingres.ledger_pending'),
	'1', 'with block_interval = 0 the transaction stays pending');
is($primary->safe_psql('db1', 'SELECT coalesce(max(height), -1) FROM kingres.ledger_blocks'),
	$height, 'and no block was built');
$primary->safe_psql('postgres', q(ALTER SYSTEM SET kingres_ledger.block_interval = '500ms'));
$primary->reload;
ok( $primary->poll_query_until('db1',
		q(SELECT count(*) = 0 FROM kingres.ledger_pending), 't'),
	'after a reload with an interval the pending transaction is built');

#--------------------------------------------------------------------------
# A prepared transaction that is committed later; the restart.
#--------------------------------------------------------------------------
$primary->safe_psql('db2', q(BEGIN; INSERT INTO public.w VALUES (10, 'prepared'); PREPARE TRANSACTION 'p1';));
$primary->safe_psql('db2', q(COMMIT PREPARED 'p1'));
ok( $primary->poll_query_until('db2',
		q(SELECT count(*) = 0 FROM kingres.ledger_pending), 't'),
	'a transaction that was prepared and committed later is built');

$primary->safe_psql('postgres', q(ALTER SYSTEM SET kingres_ledger.block_interval = 0));
$primary->reload;
sleep 2;
$primary->safe_psql('db1', q(INSERT INTO public.w VALUES (3, 'c')));
$primary->safe_psql('db2', q(INSERT INTO public.w VALUES (3, 'c')));
$primary->safe_psql('postgres', q(ALTER SYSTEM SET kingres_ledger.block_interval = '500ms'));
$primary->restart;
foreach my $db ('db1', 'db2')
{
	ok( $primary->poll_query_until($db,
			q(SELECT count(*) = 0 FROM kingres.ledger_pending), 't'),
		"$db: what was pending at a restart is built after it");
}

#--------------------------------------------------------------------------
# A bounded block is followed by more blocks.
#--------------------------------------------------------------------------
$primary->safe_psql('postgres', q(ALTER SYSTEM SET kingres_ledger.block_interval = 0));
$primary->reload;
sleep 2;
$primary->safe_psql('db1', q(ALTER DATABASE db1 SET kingres_ledger.max_block_transactions = 2));
$primary->safe_psql('db1', "INSERT INTO public.w VALUES ($_, 'bounded')") for (20 .. 26);
$height = $primary->safe_psql('db1', 'SELECT max(height) FROM kingres.ledger_blocks');
$primary->safe_psql('postgres', q(ALTER SYSTEM SET kingres_ledger.block_interval = '500ms'));
$primary->reload;
ok( $primary->poll_query_until('db1',
		q(SELECT count(*) = 0 FROM kingres.ledger_pending), 't'),
	'seven transactions with at most two to a block are all built');
cmp_ok($primary->safe_psql('db1', 'SELECT max(height) FROM kingres.ledger_blocks'),
	'>=', $height + 4, 'in at least four blocks');
is($primary->safe_psql('db1', 'SELECT max(tx_count) FROM kingres.ledger_blocks WHERE height > ' . $height),
	'2', 'of at most two transactions');

#--------------------------------------------------------------------------
# The builder lock is taken: the worker gives up quietly.
#--------------------------------------------------------------------------
my $holder = $primary->background_psql('db2');
$holder->query_safe('BEGIN; LOCK TABLE kingres.ledger_chain IN EXCLUSIVE MODE;');
$primary->safe_psql('db2', q(INSERT INTO public.w VALUES (30, 'locked')));
sleep 4;
is($primary->safe_psql('db2', 'SELECT count(*) FROM kingres.ledger_pending'),
	'1', 'while the builder lock is held nothing is built');
like(slurp_file($primary->logfile), qr/the block builder is busy in database/,
	'the worker logged that it will retry');
$holder->query_safe('ROLLBACK');
$holder->quit;
ok( $primary->poll_query_until('db2',
		q(SELECT count(*) = 0 FROM kingres.ledger_pending), 't'),
	'after the lock is released the next round builds the block');

#--------------------------------------------------------------------------
# A standby runs no worker; the promoted server builds.
#--------------------------------------------------------------------------
$primary->backup('b1');
my $standby = PostgreSQL::Test::Cluster->new('standby');
$standby->init_from_backup($primary, 'b1', has_streaming => 1);
$standby->start;
$primary->wait_for_catchup($standby);
sleep 3;
is($standby->safe_psql('postgres',
		q(SELECT count(*) FROM pg_stat_activity WHERE backend_type LIKE 'kingres_ledger%')),
	'0', 'a standby has no launcher and no worker');
unlike(slurp_file($standby->logfile), qr/kingres_ledger.*(ERROR|FATAL)/, 'and nothing in its log');
$primary->stop;
$standby->promote;
$standby->safe_psql('db2', q(INSERT INTO public.w VALUES (40, 'promoted')));
ok( $standby->poll_query_until('db2',
		q(SELECT count(*) = 0 FROM kingres.ledger_pending), 't'),
	'the promoted server builds blocks');
is($standby->safe_psql('db2', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'and the ledger verifies');
is($standby->safe_psql('postgres',
		q(SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'kingres_ledger launcher')),
	'1', 'the launcher runs after promotion');
$standby->stop;

done_testing();
