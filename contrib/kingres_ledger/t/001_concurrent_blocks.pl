
# contrib/kingres_ledger/t/001_concurrent_blocks.pl
#
# Concurrent writers while blocks are being built: every committed
# transaction ends up in exactly one block, rolled back transactions leave
# nothing, and the ledger verifies cleanly afterwards.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: PostgreSQL

use strict;
use warnings FATAL => 'all';

use IPC::Run;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('main');
$node->init;
$node->append_conf(
	'postgresql.conf', qq(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.max_block_transactions = 100
max_connections = 40
));
$node->start;

$node->safe_psql(
	'postgres', q(
CREATE EXTENSION kingres_ledger;
CREATE TABLE public.w (client int, n int, note text);
CREATE TABLE public.v (client int, n int);
SELECT kingres.ledger_enable('public.w');
SELECT kingres.ledger_enable('public.v');
));

# Writers: three rows into w per transaction, sometimes a second table,
# a savepoint that is sometimes rolled back, and one transaction in eight is
# rolled back as a whole.
my $writer_script = "$PostgreSQL::Test::Utils::tmp_check/writer.sql";
append_to_file($writer_script, q(
\set roll random(1, 8)
\set sub random(1, 4)
BEGIN;
INSERT INTO w SELECT :client_id, g, 'a' FROM generate_series(1, 3) g;
SAVEPOINT s;
INSERT INTO v VALUES (:client_id, 1);
\if :sub = 1
ROLLBACK TO s;
\endif
INSERT INTO v VALUES (:client_id, 2);
\if :roll = 1
ROLLBACK;
\else
COMMIT;
\endif
));

# Builders: close blocks as fast as they can, two at a time.
my $builder_script = "$PostgreSQL::Test::Utils::tmp_check/builder.sql";
append_to_file($builder_script, q(
SELECT kingres.ledger_build_block();
));

my @pgbench = ('pgbench', '-n', '-h', $node->host, '-p', $node->port);
my ($wout, $werr, $bout, $berr);
my $writers = IPC::Run::start(
	[ @pgbench, '-c', '6', '-j', '3', '-T', '4', '-f', $writer_script, 'postgres' ],
	'>', \$wout, '2>', \$werr);
my $builders = IPC::Run::start(
	[ @pgbench, '-c', '2', '-j', '2', '-T', '4', '-f', $builder_script, 'postgres' ],
	'>', \$bout, '2>', \$berr);

ok($writers->finish, 'writers finished') or diag($werr);
ok($builders->finish, 'builders finished') or diag($berr);

# Close everything that is still pending.
my $guard = 0;
while ($node->safe_psql('postgres', 'SELECT kingres.ledger_build_block()') ne ''
	&& $guard++ < 10000)
{
}

sub q1
{
	return $node->safe_psql('postgres', shift);
}

cmp_ok(q1('SELECT count(*) FROM w'), '>', 0, 'writers committed rows');
is( q1(q(SELECT (SELECT count(*) FROM w) + (SELECT count(*) FROM v)
	      = (SELECT count(*) FROM kingres.ledger_entry))),
	't', 'one entry per committed row, none for rolled back rows');
is( q1(q(SELECT count(*) FROM (SELECT DISTINCT ledger_txid FROM kingres.ledger_entry) e
	      WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_tx t WHERE t.ledger_txid = e.ledger_txid))),
	'0', 'every transaction with entries is in a block');
is( q1(q(SELECT count(*) FROM kingres.ledger_tx)),
	q1(q(SELECT count(DISTINCT ledger_txid) FROM kingres.ledger_entry)),
	'no transaction is in a block that has no entries, none is in two');
is(q1('SELECT count(*) FROM kingres.ledger_pending'), '0',
	'nothing is left pending');
is( q1(q(SELECT sum(tx_count) FROM kingres.ledger_blocks)),
	q1(q(SELECT count(*) FROM kingres.ledger_tx)),
	'block transaction counts add up');
cmp_ok(q1('SELECT count(*) FROM kingres.ledger_blocks'), '>', 3,
	'several blocks were built');
is(q1('SELECT max(tx_count) <= 100 FROM kingres.ledger_blocks'), 't',
	'max_block_transactions is respected');
is(q1('SELECT count(*) FROM kingres.ledger_verify()'), '0',
	'the ledger verifies cleanly');

# A digest and a proof after the concurrent run.
my $digest = q1('SELECT kingres.ledger_generate_digest()');
is(q1(qq(SELECT kingres.ledger_verify_digest('$digest'))), 't', 'digest verifies');
is( q1(qq(SELECT kingres.ledger_verify_proof(
		kingres.ledger_inclusion_proof('public.w', kingres.ledger_row_hash(x)), '$digest')
		FROM w x LIMIT 1)),
	't', 'inclusion proof verifies');

$node->stop;
done_testing();
