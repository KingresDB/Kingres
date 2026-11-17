# contrib/kingres_ledger/t/016_dump_upgrade.pl
#
# The write guard against the ways a ledger is copied (review, "Not covered"):
#
#   * pg_dump and a restore into a new database work with the guard: the
#     restore writes nothing into a ledger table except what CREATE EXTENSION
#     writes (the identity of the new chain), because the ledger tables are
#     not dumped; the restored database has a new, empty ledger and says so
#     (ledger_verify names the capture trigger of every table that is not
#     registered);
#   * pg_upgrade (binary upgrade mode: the extension is created empty and its
#     members are added one by one, which the protection hook would refuse
#     everywhere else) carries a ledger over: the chain, the blocks and the
#     entries are the same files, ledger_verify is clean, a digest taken before
#     verifies, the capture and the block builder work.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use IPC::Run;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $old = PostgreSQL::Test::Cluster->new('old');
$old->init;
my $conf = qq(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = 0
);
$old->append_conf('postgresql.conf', $conf);
$old->start;
$old->safe_psql('postgres', 'CREATE DATABASE app');
$old->safe_psql('app', q(
	CREATE EXTENSION kingres_ledger;
	CREATE SCHEMA s;
	CREATE TABLE s.accounts (id int PRIMARY KEY, name text);
	CREATE TABLE s.notes (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('s.accounts');
	SELECT kingres.ledger_enable('s.notes', 'updatable');
	INSERT INTO s.accounts VALUES (1, 'alice'), (2, 'bob');
	INSERT INTO s.notes VALUES (1, 'first');
	SELECT kingres.ledger_build_block();
	UPDATE s.notes SET body = 'second';
	INSERT INTO s.accounts VALUES (3, 'carol');
));
my $digest = $old->safe_psql('app', 'SELECT kingres.ledger_generate_digest()');
is($old->safe_psql('app', 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'the ledger verifies');
my $chain = $old->safe_psql('app', 'SELECT encode(chain_id, \'hex\') FROM kingres.ledger_chain');
my $blocks = $old->safe_psql('app',
	q(SELECT string_agg(height || ':' || encode(block_hash, 'hex'), ',' ORDER BY height) FROM kingres.ledger_blocks));

#--------------------------------------------------------------------------
# pg_dump and restore.
#--------------------------------------------------------------------------
my $dump = $old->basedir . '/app.sql';
command_ok([ 'pg_dump', '--no-sync', '-f', $dump, '-d', $old->connstr('app') ], 'pg_dump works');
$old->safe_psql('postgres', 'CREATE DATABASE restored');
my ($out, $err);
my $ok = IPC::Run::run([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $old->connstr('restored'), '-f', $dump ],
	'>', \$out, '2>', \$err);
ok($ok, 'the dump restores with the guard on') or diag($err);
is($err, '', 'without a message');
is($old->safe_psql('restored', 'SELECT count(*) FROM s.accounts'), '3', 'the rows are restored');
isnt($old->safe_psql('restored', q(SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain)), $chain,
	'the restored database has the identity of a new chain, not that of the dumped one');
is($old->safe_psql('restored', 'SELECT count(*) FROM kingres.ledger_blocks'), '0', 'and no block');
is($old->safe_psql('restored', q(SELECT count(*) FROM kingres.ledger_verify() WHERE check_name = 'trigger')), '2',
	'ledger_verify names the capture trigger of each table that is not registered');
my ($ret, $o2, $e2) = $old->psql('restored', 'INSERT INTO s.accounts VALUES (9, \'x\')');
isnt($ret, 0, 'an insert into a restored table does not enter an entry of the old ledger');

#--------------------------------------------------------------------------
# pg_upgrade.
#--------------------------------------------------------------------------
$old->stop;
my $new = PostgreSQL::Test::Cluster->new('new');
$new->init;
$new->append_conf('postgresql.conf', $conf);
my $bindir = `pg_config --bindir`;
chomp $bindir;
my $workdir = $old->basedir . '/upgrade';
mkdir $workdir;
$ok = IPC::Run::run(
	[ 'pg_upgrade', '--no-sync', '-d', $old->data_dir, '-D', $new->data_dir,
	  '-b', $bindir, '-B', $bindir, '-s', $new->host, '-p', $old->port, '-P', $new->port ],
	'>', \$out, '2>', \$err, init => sub { chdir $workdir or die });
ok($ok, 'pg_upgrade of a cluster with a ledger works (binary upgrade mode)') or diag($out . $err);
$new->start;
is($new->safe_psql('app', 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'the upgraded ledger verifies');
is($new->safe_psql('app', q(SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain)), $chain, 'with the same chain');
is($new->safe_psql('app',
	q(SELECT string_agg(height || ':' || encode(block_hash, 'hex'), ',' ORDER BY height) FROM kingres.ledger_blocks)),
	$blocks, 'and the same blocks');
is($new->safe_psql('app', qq(SELECT kingres.ledger_verify_digest('$digest'::jsonb))), 't',
	'a digest taken before the upgrade verifies');
$new->safe_psql('app', q(INSERT INTO s.accounts VALUES (4, 'dave'); UPDATE s.notes SET body = 'third';));
is($new->safe_psql('app', 'SELECT kingres.ledger_build_block() IS NOT NULL'), 't', 'the capture and the builder work');
is($new->safe_psql('app', 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'and the ledger verifies');
my ($r3, $o3, $e3) = $new->psql('app', q(INSERT INTO kingres.ledger_blocks VALUES (99, 1, sha256('a'), sha256('b'), 1, now(), sha256('c'))),
	extra_params => [ '-v', 'VERBOSITY=terse' ]);
like($e3, qr/cannot write to table/, 'the guard is on in the upgraded cluster');

$new->stop;
done_testing();
