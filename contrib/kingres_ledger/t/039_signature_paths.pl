# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/039_signature_paths.pl
#
# What the review of client signatures listed as not checked, for a table with
# the policy "required":
#
#   * COPY: the rows go through the capture trigger like any insert, so a COPY
#     in a transaction that does not attest them fails at COMMIT (KL010), and
#     attested row by row it commits;
#   * partitioned tables: a ledger table cannot be a partitioned table, a
#     partition or a parent, so the policy has no partitioned case (README);
#   * logical replication: the apply worker's commit reaches the same check, so
#     a subscriber cannot hold a table with the policy required fed by a
#     subscription (the rows are not applied, the error is KL010, and the
#     subscription keeps failing until the table is dropped from it); a table
#     with the policy optional on the same subscriber is fed, and its entries
#     are made.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KlSign qw(have_openssl new_key sign_message message_bytes hex_of unhex_of random_nonce);

plan skip_all => 'the openssl command (3.0 or later) is needed to make Ed25519 signatures'
  unless have_openssl();

$ENV{PGOPTIONS} = '-c search_path=public';

my $publisher = PostgreSQL::Test::Cluster->new('sig_pub');
$publisher->init(allows_streaming => 'logical');
$publisher->start;

my $node = PostgreSQL::Test::Cluster->new('sig_paths');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my $dir = PostgreSQL::Test::Utils::tempdir;
my $k1 = new_key($dir, 'k1');

$node->safe_psql('postgres', q{CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	SELECT kingres.ledger_set_signature_policy('docs', 'required');
});
sub db { return $node->safe_psql('d', @_); }

my $chain = unhex_of(db(q{SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain}));
my $docs_id = db(q{SELECT table_id FROM kingres.ledger_table WHERE relid = 'docs'::regclass});
db(sprintf(q{SELECT kingres.ledger_client_key_register(decode('%s', 'hex'), NULL, 'k1')}, hex_of($k1->{pub})));

sub attest_sql
{
	my ($id, $body) = @_;
	my $hash = db(sprintf(q{SELECT encode(kingres.ledger_row_hash(ROW(%d, '%s')::docs), 'hex')}, $id, $body));
	my $nonce = random_nonce();
	my $msg = message_bytes(chain_id => $chain, table_id => $docs_id, op => 'I',
		row_hash => unhex_of($hash), key_id => $k1->{key_id}, nonce => $nonce);
	my $sig = sign_message($k1, $msg);
	return sprintf(q{SELECT kingres.ledger_attest('docs'::regclass, decode('%s', 'hex'), 'I', decode('%s', 'hex'), decode('%s', 'hex'), decode('%s', 'hex'));},
		$hash, hex_of($k1->{key_id}), hex_of($nonce), hex_of($sig));
}

#--------------------------------------------------------------------------
# COPY into a table with the policy required
#--------------------------------------------------------------------------
{
	my ($ret, $out, $err) = $node->psql('d', "\\set VERBOSITY sqlstate\nBEGIN;\nCOPY docs (id, body) FROM STDIN;\n1\tone\n2\ttwo\n\\.\nCOMMIT;\n");
	isnt($ret, 0, 'COPY into a required table, not attested: the COMMIT fails');
	like($err, qr/ERROR:\s+KL010/, 'with KL010');
	is(db('SELECT count(*) FROM docs'), '0', 'and no row was written');

	my $script = "\\set VERBOSITY sqlstate\nBEGIN;\nCOPY docs (id, body) FROM STDIN;\n1\tone\n2\ttwo\n\\.\n"
	  . attest_sql(1, 'one') . "\n" . attest_sql(2, 'two') . "\nCOMMIT;\n";
	($ret, $out, $err) = $node->psql('d', $script);
	is($ret, 0, 'COPY with an attestation for each row commits') or diag($err);
	is(db('SELECT count(*) FROM docs'), '2', 'both rows are there');

	$script = "\\set VERBOSITY sqlstate\nBEGIN;\nCOPY docs (id, body) FROM STDIN;\n3\tthree\n4\tfour\n\\.\n" . attest_sql(3, 'three') . "\nCOMMIT;\n";
	($ret, $out, $err) = $node->psql('d', $script);
	isnt($ret, 0, 'one row of two attested: the COMMIT fails');
	like($err, qr/ERROR:\s+KL010/, 'with KL010');
	is(db('SELECT count(*) FROM docs'), '2', 'and nothing of the second COPY is there');
	db('SELECT kingres.ledger_build_block()');
	is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies');
}

#--------------------------------------------------------------------------
# Partitioned tables
#--------------------------------------------------------------------------
{
	db(q{CREATE TABLE parted (id int, body text) PARTITION BY RANGE (id);
		CREATE TABLE parted_1 PARTITION OF parted FOR VALUES FROM (0) TO (100);
		CREATE TABLE plain_l (id int, body text);
		SELECT kingres.ledger_enable('plain_l');});
	my ($ret, $out, $err) = $node->psql('d', q{SELECT kingres.ledger_enable('parted')});
	isnt($ret, 0, 'a partitioned table cannot be a ledger table');
	like($err, qr/partitioned table "parted" cannot be a ledger table/, 'the message says so');
	($ret, $out, $err) = $node->psql('d', q{SELECT kingres.ledger_enable('parted_1')});
	isnt($ret, 0, 'a partition cannot be a ledger table');
	($ret, $out, $err) = $node->psql('d', q{ALTER TABLE parted ATTACH PARTITION plain_l FOR VALUES FROM (100) TO (200)});
	isnt($ret, 0, 'a ledger table cannot be attached as a partition');
	like($err, qr/ledger/i, 'the refusal names the ledger');
	($ret, $out, $err) = $node->psql('d', q{SELECT kingres.ledger_set_signature_policy('parted', 'required')});
	isnt($ret, 0, 'so the policy has no partitioned case: the switch of a partitioned table is refused');
	like($err, qr/not under ledger control/, 'because it is not under ledger control');
}

#--------------------------------------------------------------------------
# Logical replication apply into tables of a subscriber
#--------------------------------------------------------------------------
{
	$publisher->safe_psql('postgres', q{
		CREATE TABLE docs_ok (id int PRIMARY KEY, body text);
		CREATE TABLE docs_req (id int PRIMARY KEY, body text);
		CREATE PUBLICATION p_ok FOR TABLE docs_ok;
		CREATE PUBLICATION p_req FOR TABLE docs_req;
	});
	db(q{
		CREATE TABLE docs_ok (id int PRIMARY KEY, body text);
		CREATE TABLE docs_req (id int PRIMARY KEY, body text);
		SELECT kingres.ledger_enable('docs_ok');
		SELECT kingres.ledger_enable('docs_req');
		SELECT kingres.ledger_set_signature_policy('docs_req', 'required');
	});
	my $conn = $publisher->connstr('postgres');
	$conn =~ s/'/''/g;
	db("CREATE SUBSCRIPTION s_ok CONNECTION '$conn' PUBLICATION p_ok WITH (copy_data = false)");
	db("CREATE SUBSCRIPTION s_req CONNECTION '$conn' PUBLICATION p_req WITH (copy_data = false)");

	my $log_start = -s $node->logfile;
	$publisher->safe_psql('postgres', q{
		INSERT INTO docs_ok VALUES (1, 'a');
		INSERT INTO docs_req VALUES (1, 'a');
	});
	ok($node->poll_query_until('d', q{SELECT count(*) = 1 FROM docs_ok}),
		'a table with the policy optional is fed by the subscription');
	is(db(q{SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table t USING (table_id)
		WHERE t.relid = 'docs_ok'::regclass}), '1', 'and its entry was made');
	ok($node->wait_for_log(qr/ERROR:\s+ledger entry without client signature/, $log_start),
		'the apply worker of the required table fails with KL010, the same error as a session');
	is(db('SELECT count(*) FROM docs_req'), '0', 'and the row is not applied: the subscriber cannot hold such a table');
	is(db(q{SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table t USING (table_id)
		WHERE t.relid = 'docs_req'::regclass}), '0', 'with no entry');
	ok($node->poll_query_until('d', q{SELECT apply_error_count > 0 FROM pg_stat_subscription_stats WHERE subname = 's_req'}),
		'the subscription counts the apply error');
	db(q{ALTER SUBSCRIPTION s_req DISABLE});
	db(q{ALTER SUBSCRIPTION s_req SET (slot_name = NONE)});
	db(q{DROP SUBSCRIPTION s_req});
	db(q{DROP SUBSCRIPTION s_ok});
	is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies');
}

done_testing();
