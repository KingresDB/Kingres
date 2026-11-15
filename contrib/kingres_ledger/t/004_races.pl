# contrib/kingres_ledger/t/004_races.pl
#
# Review findings that need more than one session: the name-resolution race
# of the protection hook (L-03, a stress test: a deterministic test is not
# possible because the window is a few microseconds wide), rows smuggled in
# through a foreign-table child (L-02), ledger_enable under REPEATABLE READ
# and concurrent writers (L-05), and the denial of service on the block
# builder (L-08).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use IPC::Run;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('races');
$node->init;
$node->append_conf(
	'postgresql.conf', qq(
shared_preload_libraries = 'kingres_ledger'
max_connections = 60
));
$node->start;

sub q1
{
	return $node->safe_psql('postgres', shift);
}

q1(q(
CREATE EXTENSION kingres_ledger;
CREATE EXTENSION file_fdw;
CREATE ROLE racer LOGIN;
CREATE ROLE evil LOGIN;
CREATE SCHEMA rc AUTHORIZATION racer;
CREATE SCHEMA app;
CREATE SCHEMA s8;
CREATE SCHEMA s9;
CREATE SCHEMA s10;
));

#--------------------------------------------------------------------------
# L-03: the hook must not be bypassed by a rename race.  The role "racer"
# is an ordinary role that owns two tables: a harmless one and a ledger
# table.  The name rc.t alternates between them.  Attackers run "disable
# trigger" on rc.t; if the hook looks at the harmless table and the
# statement then runs on the ledger table, the triggers are switched off.
#--------------------------------------------------------------------------
q1(q(
SET ROLE racer;
CREATE TABLE rc.t (id int);
CREATE TABLE rc.o (id int);
RESET ROLE;
SELECT kingres.ledger_enable('rc.o');
));

my $attack = "$PostgreSQL::Test::Utils::tmp_check/attack.sql";
append_to_file($attack, q(
DO $$ BEGIN EXECUTE 'alter table rc.t disable trigger user'; EXCEPTION WHEN others THEN NULL; END $$;
DO $$ BEGIN EXECUTE 'alter table rc.t set unlogged'; EXCEPTION WHEN others THEN NULL; END $$;
));
my $swap = "$PostgreSQL::Test::Utils::tmp_check/swap.sql";
append_to_file($swap, q(
BEGIN;
ALTER TABLE rc.t RENAME TO tmp;
ALTER TABLE rc.o RENAME TO t;
ALTER TABLE rc.tmp RENAME TO o;
COMMIT;
));

# Deadlocks between the renamer and the attackers are retried by pgbench.
my @pgbench = ('pgbench', '-n', '-h', $node->host, '-p', $node->port,
	'--max-tries=1000');
my ($aout, $aerr, $sout, $serr);
my $duration = 45;
my $attackers = IPC::Run::start(
	[ @pgbench, '-U', 'racer', '-c', '4', '-j', '2', '-T', $duration,
		'-f', $attack, 'postgres' ],
	'>', \$aout, '2>', \$aerr);
my $swapper = IPC::Run::start(
	[ @pgbench, '-U', 'racer', '-c', '1', '-T', $duration,
		'-f', $swap, 'postgres' ],
	'>', \$sout, '2>', \$serr);
$attackers->finish;
$swapper->finish;
like($sout, qr/number of transactions actually processed: [1-9]/,
	'the renamer ran')
  or diag($serr);
like($aout, qr/number of transactions actually processed: [1-9]/,
	'the attackers ran')
  or diag($aerr);

is( q1(q(SELECT count(*) FROM pg_trigger g JOIN kingres.ledger_table t ON t.relid = g.tgrelid
	      WHERE g.tgname LIKE 'kingres_ledger%' AND g.tgenabled <> 'A')),
	'0', 'no ledger trigger was disabled by the rename race');
is( q1(q(SELECT c.relpersistence FROM pg_class c JOIN kingres.ledger_table t ON t.relid = c.oid)),
	'p', 'the ledger table is still logged');
is(q1('SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the ledger verifies after the race');

#--------------------------------------------------------------------------
# L-02: a foreign table that inherits from a ledger table.
#--------------------------------------------------------------------------
q1(q(
CREATE TABLE app.led (id int, note text);
SELECT kingres.ledger_enable('app.led');
INSERT INTO app.led VALUES (1, 'real');
CREATE SERVER fs FOREIGN DATA WRAPPER file_fdw;
));
my $forged = "$PostgreSQL::Test::Utils::tmp_check/forged.csv";
append_to_file($forged, "999,forged\n");
my ($ret, $stdout, $stderr) = $node->psql(
	'postgres',
	"CREATE FOREIGN TABLE app.forged () INHERITS (app.led) SERVER fs OPTIONS (filename '$forged', format 'csv')",
	extra_params => [ '-v', 'VERBOSITY=sqlstate' ]);
isnt($ret, 0, 'a foreign table cannot inherit from a ledger table');
like($stderr, qr/KL002/, 'with SQLSTATE KL002');
is(q1('SELECT count(*) FROM app.led'), '1', 'no row was smuggled in');

#--------------------------------------------------------------------------
# L-05: ledger_enable and snapshots.
#--------------------------------------------------------------------------
q1(q(
CREATE TABLE s8.t (id int);
INSERT INTO s8.t VALUES (1);
));
my $a = $node->background_psql('postgres', on_error_stop => 0);
$a->query_safe(
	q(BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) FROM s8.t;));
q1('INSERT INTO s8.t VALUES (2)');
$a->{stderr} = '';
$a->query(q(SELECT kingres.ledger_enable('s8.t');));
like($a->{stderr}, qr/READ COMMITTED/,
	'ledger_enable refuses a REPEATABLE READ transaction');
$a->query(q(ROLLBACK;));
$a->quit;
is(q1(q(SELECT count(*) FROM kingres.ledger_table WHERE relid = 's8.t'::regclass)),
	'0', 'nothing was registered');

# READ COMMITTED: a row committed after the transaction began, before the
# lock, is entered too.
q1(q(
CREATE TABLE s9.t (id int);
INSERT INTO s9.t VALUES (1);
));
$a = $node->background_psql('postgres', on_error_stop => 0);
$a->query_safe(q(BEGIN; SELECT count(*) FROM s9.t;));
q1('INSERT INTO s9.t VALUES (2)');
$a->query_safe(q(SELECT kingres.ledger_enable('s9.t'); COMMIT;));
$a->quit;
is( q1(q(SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table t USING (table_id)
	      WHERE t.relid = 's9.t'::regclass)),
	'2', 'both rows are in the ledger');
is(q1('SELECT count(*) FROM kingres.ledger_verify()'), '0', 'and it verifies');

# A writer that waits for the lock of ledger_enable is captured afterwards.
q1(q(
CREATE TABLE s10.t (id int);
INSERT INTO s10.t VALUES (1);
));
$a = $node->background_psql('postgres', on_error_stop => 0);
my $b = $node->background_psql('postgres', on_error_stop => 0);
$a->query_safe(q(BEGIN; SELECT kingres.ledger_enable('s10.t');));
$b->query_until(qr/started/, "\\echo started\nINSERT INTO s10.t VALUES (2);\n");
$a->query_safe(q(COMMIT;));
$b->query_safe(q(SELECT 1;));
$a->quit;
$b->quit;
is( q1(q(SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table t USING (table_id)
	      WHERE t.relid = 's10.t'::regclass)),
	'2', 'the writer that waited for ledger_enable was captured');
is(q1('SELECT count(*) FROM kingres.ledger_verify()'), '0', 'and it verifies');

#--------------------------------------------------------------------------
# L-08: an ordinary role cannot stop the block builder.
#--------------------------------------------------------------------------
my $e = $node->background_psql('postgres',
	connstr => $node->connstr('postgres') . ' user=evil',
	on_error_stop => 0);
$e->query_safe(
	q(SELECT pg_advisory_lock(hashtextextended('kingres_ledger.build_block', 0));));
$e->{stderr} = '';
$e->query(q(BEGIN; LOCK TABLE kingres.ledger_chain IN EXCLUSIVE MODE;));
$e->query(q(ROLLBACK;));
like($e->{stderr}, qr/permission denied/,
	'an ordinary role cannot take the lock of the block builder');
$e->{stderr} = '';
$e->query(q(SELECT kingres.ledger_verify_digest('{}');));
like($e->{stderr}, qr/permission denied/,
	'an ordinary role cannot scan the chain');
q1('INSERT INTO app.led VALUES (2, $$second$$)');
($ret, $stdout, $stderr) = $node->psql('postgres',
	q(SET statement_timeout = '10s'; SELECT kingres.ledger_build_block();));
is($ret, 0, 'the builder runs while a role holds the old advisory lock key');
like($stdout, qr/^\d+$/m, 'and builds a block');
$e->quit;

$node->stop;
done_testing();
