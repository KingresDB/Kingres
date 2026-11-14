
# contrib/kingres_ledger/t/002_restart_standby.pl
#
# The ledger survives a clean restart and a crash, and a streaming standby
# holds an identical ledger that verifies on its own.
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
max_prepared_transactions = 5
));
$primary->start;

$primary->safe_psql(
	'postgres', q(
CREATE EXTENSION kingres_ledger;
CREATE TABLE public.items (id int PRIMARY KEY, label text, amount numeric(10,2), at timestamptz);
INSERT INTO items VALUES (1, 'existing row', 1.50, '2026-01-01 00:00:00+00');
SELECT kingres.ledger_enable('public.items');
INSERT INTO items VALUES (2, 'two', 2.50, '2026-01-02 00:00:00+00'), (3, 'three', 3.50, NULL);
));
my $d0 = $primary->safe_psql('postgres', 'SELECT kingres.ledger_generate_digest()');

# The standby is built from a base backup of a primary that already has a
# ledger, and gets the library through the copied configuration.
$primary->backup('b1');
my $standby = PostgreSQL::Test::Cluster->new('standby');
$standby->init_from_backup($primary, 'b1', has_streaming => 1);
$standby->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'");
$standby->start;

$primary->safe_psql(
	'postgres', q(
INSERT INTO items VALUES (4, 'four', 4.50, '2026-01-04 00:00:00+00');
BEGIN;
INSERT INTO items VALUES (5, 'five', 5.50, NULL), (6, 'six', 6.50, NULL);
COMMIT;
));
my $d1 = $primary->safe_psql('postgres', 'SELECT kingres.ledger_generate_digest()');
$primary->safe_psql('postgres', q(INSERT INTO items VALUES (7, 'seven', 7.50, NULL)));
$primary->wait_for_catchup($standby);

# Contents of the ledger tables in a canonical form.
my $fingerprint = q(
SELECT md5(string_agg(x, ';' ORDER BY k)) FROM (
  SELECT 'b' || height AS k, concat_ws(',', height, hash_version, encode(prev_hash,'hex'),
         encode(tx_merkle_root,'hex'), tx_count, encode(block_hash,'hex')) AS x
    FROM kingres.ledger_blocks
  UNION ALL
  SELECT 't' || ledger_txid, concat_ws(',', ledger_txid, block_height, tx_index, entry_count,
         encode(merkle_root,'hex'), user_name) FROM kingres.ledger_tx
  UNION ALL
  SELECT 'e' || ledger_txid || '.' || seq, concat_ws(',', ledger_txid, seq, table_id, op,
         encode(row_hash,'hex')) FROM kingres.ledger_entry
  UNION ALL
  SELECT 'p' || ledger_txid, concat_ws(',', ledger_txid, user_name) FROM kingres.ledger_pending
  UNION ALL
  SELECT 'l' || table_id, concat_ws(',', table_id, mode) FROM kingres.ledger_table
  UNION ALL
  SELECT 'c', encode(chain_id, 'hex') FROM kingres.ledger_chain
) s);

my $fp_primary = $primary->safe_psql('postgres', $fingerprint);
my $fp_standby = $standby->safe_psql('postgres', $fingerprint);
is($fp_standby, $fp_primary, 'standby has an identical ledger');
isnt($fp_primary, '', 'the fingerprint is not empty');

is($standby->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the ledger verifies on the standby');
is($standby->safe_psql('postgres', "SELECT kingres.ledger_verify_digest('$d1')"),
	't', 'the digest verifies on the standby');
is( $standby->safe_psql('postgres',
		qq(SELECT kingres.ledger_verify_proof(
		   kingres.ledger_inclusion_proof('items', kingres.ledger_row_hash(i)), '$d1')
		   FROM items i WHERE id = 5)),
	't', 'an inclusion proof built on the standby verifies');
is( $standby->safe_psql('postgres',
		q(SELECT count(*) FROM kingres.ledger_pending)),
	'1', 'the pending transaction is visible on the standby');

# Building blocks needs a writable server.
my ($ret, $stdout, $stderr) =
  $standby->psql('postgres', 'SELECT kingres.ledger_build_block()');
isnt($ret, 0, 'a standby cannot build blocks');
like($stderr, qr/during recovery|read-only/, 'with an error that says the server is in recovery');

($ret, $stdout, $stderr) =
  $standby->psql('postgres', q(SELECT kingres.ledger_enable('public.items')));
isnt($ret, 0, 'a standby cannot put a table under ledger control');

# The standby has the hook as well.
($ret, $stdout, $stderr) =
  $standby->psql('postgres', 'ALTER TABLE items DISABLE TRIGGER ALL');
isnt($ret, 0, 'the protection hook runs on the standby');

# Clean restart of the primary.
$primary->restart;
is($primary->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the ledger verifies after a restart');
is($primary->safe_psql('postgres', "SELECT kingres.ledger_verify_digest('$d0')"),
	't', 'the old digest verifies after a restart');
$primary->safe_psql('postgres', q(INSERT INTO items VALUES (8, 'eight', 8.50, NULL)));
my $d2 = $primary->safe_psql('postgres', 'SELECT kingres.ledger_generate_digest()');
is($primary->safe_psql('postgres', "SELECT kingres.ledger_verify_digest('$d1')"),
	't', 'the older digest still verifies after new blocks');
is($primary->safe_psql('postgres', "SELECT kingres.ledger_verify_digest('$d2')"),
	't', 'the new digest verifies');

# Crash of the primary with a committed but not yet built transaction and a
# prepared transaction.
$primary->safe_psql('postgres', q(INSERT INTO items VALUES (9, 'nine', 9.50, NULL)));
$primary->safe_psql('postgres', q(
BEGIN;
INSERT INTO items VALUES (10, 'ten', 10.50, NULL);
PREPARE TRANSACTION 'crash_test';
));
$primary->stop('immediate');
$primary->start;
is($primary->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the ledger verifies after crash recovery');
is($primary->safe_psql('postgres', q(SELECT count(*) FROM kingres.ledger_pending)),
	'1', 'the committed transaction is still queued after crash recovery');
$primary->safe_psql('postgres', q(COMMIT PREPARED 'crash_test'));
my $d3 = $primary->safe_psql('postgres', 'SELECT kingres.ledger_generate_digest()');
is($primary->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the ledger verifies after the prepared transaction committed');
is($primary->safe_psql('postgres', 'SELECT count(*) FROM items'), '10',
	'all rows are there');
is($primary->safe_psql('postgres',
		q(SELECT count(*) FROM kingres.ledger_entry)), '10',
	'one entry per row');
is($primary->safe_psql('postgres',
		q(SELECT bool_and(height = expected) FROM
		  (SELECT height, row_number() OVER (ORDER BY height) - 1 AS expected
		     FROM kingres.ledger_blocks) s)),
	't', 'block heights are dense');
is($primary->safe_psql('postgres', "SELECT kingres.ledger_verify_digest('$d3')"),
	't', 'the last digest verifies');

$primary->wait_for_catchup($standby);
is($standby->safe_psql('postgres', $fingerprint),
	$primary->safe_psql('postgres', $fingerprint),
	'the standby still has an identical ledger');

$standby->stop;
$primary->stop;
done_testing();
