# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/032_attest_policy.pl
#
# Client signatures (0.3, part B; src/sign.c, the policy in src/capture.c):
#
#   * the three tables of client signatures are registered by the install
#     script under the hook, and protected like the other tables of the ledger;
#   * a signature made by a client with a key of its own (the openssl command;
#     the message is encoded here, independently of the server) is accepted
#     inside the transaction that wrote the entry, proved by the ordinary
#     inclusion proof, and every error KL011 to KL014 is raised for its case;
#   * the policy "required": a transaction that leaves an entry without an
#     attestation fails at COMMIT and at PREPARE TRANSACTION with KL010, an
#     UPDATE needs two attestations, a savepoint that is rolled back takes its
#     entries and its attestations with it;
#   * crash recovery and a standby;
#   * ledger_verify: a deleted, changed or unknown attestation, an unattested
#     entry from the switch on and none before it.
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

my $node = PostgreSQL::Test::Cluster->new('attest');
$node->init(allows_streaming => 1);
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "max_prepared_transactions = 10\n");
$node->start;

my $dir = PostgreSQL::Test::Utils::tempdir;
my $k1 = new_key($dir, 'k1');		# any role
my $k2 = new_key($dir, 'k2');		# role app only
my $k3 = new_key($dir, 'k3');		# role other only

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE ROLE other LOGIN; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT EXECUTE ON FUNCTION kingres.ledger_attest(regclass, bytea, "char", bytea, bytea, bytea),
		kingres.ledger_set_signature_policy(regclass, text) TO app, other;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	CREATE TABLE items (id int PRIMARY KEY, body text);
	CREATE TABLE late (id int PRIMARY KEY);
	ALTER TABLE docs OWNER TO app;
	ALTER TABLE items OWNER TO app;
	ALTER TABLE late OWNER TO app;
	SELECT kingres.ledger_enable('docs');
	SELECT kingres.ledger_enable('items', 'updatable');
	SELECT kingres.ledger_enable('late');
});

sub db { return $node->safe_psql('d', @_); }

# A change of the registry that only someone with the catalog can make: the blocking trigger is
# switched off (pg_trigger), the statement runs under the setting of a superuser, the trigger is on again.
sub registry_by_hand
{
	my ($sql) = @_;
	db(q{UPDATE pg_trigger SET tgenabled = 'D' WHERE tgrelid = 'kingres.ledger_table'::regclass AND tgname = 'kingres_ledger_block_modify'});
	db("SET kingres_ledger.allow_manual_write = on; $sql");
	db(q{UPDATE pg_trigger SET tgenabled = 'A' WHERE tgrelid = 'kingres.ledger_table'::regclass AND tgname = 'kingres_ledger_block_modify'});
}

my $chain = unhex_of(db(q{SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain}));
my %tid = (
	docs  => db(q{SELECT table_id FROM kingres.ledger_table WHERE relid = 'docs'::regclass}),
	late  => db(q{SELECT table_id FROM kingres.ledger_table WHERE relid = 'late'::regclass}),
	items => db(q{SELECT table_id FROM kingres.ledger_table WHERE relid = 'items'::regclass}));

#--------------------------------------------------------------------------
# The install script registered the tables of client signatures, under the hook.
#--------------------------------------------------------------------------
is(db(q{SELECT string_agg(relid::text || ':' || mode || ':' || signature_policy, ',' ORDER BY table_id)
	FROM kingres.ledger_table WHERE table_id <= 3}),
	'kingres.ledger_client_key:append_only:optional,kingres.ledger_client_key_revocation:append_only:optional,kingres.ledger_attestation:append_only:optional',
	'the three tables of client signatures are registered as append-only ledger tables, with the policy optional');
is(db(q{SELECT count(*) FROM pg_trigger WHERE tgname = 'kingres_ledger_capture' AND tgenabled = 'A'
	AND tgrelid IN ('kingres.ledger_client_key'::regclass, 'kingres.ledger_client_key_revocation'::regclass,
	                'kingres.ledger_attestation'::regclass)}),
	'3', 'each has the capture trigger, ENABLE ALWAYS');
is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'a new database verifies');
is(db(q{SELECT append_only_tables || ' ' || updatable_tables || ' ' || client_keys_active || ' ' || attestations || ' ' || required_tables
	FROM kingres.ledger_status}), '2 1 0 0 0',
	'ledger_status counts the tables of the user only, and has the new columns');
{
	my ($ret, $out, $err) = $node->psql('d', q{SELECT kingres.ledger_register_internal('kingres.ledger_attestation'::regclass)});
	like($err, qr/only the install or upgrade script/, 'ledger_register_internal refuses a caller outside the script, a superuser too');
	($ret, $out, $err) = $node->psql('d', q{GRANT EXECUTE ON FUNCTION kingres.ledger_register_internal(regclass) TO app});
	like($err, qr/KL002|internal function/, 'and nobody is granted EXECUTE on it');
}
foreach my $t ('ledger_client_key', 'ledger_client_key_revocation', 'ledger_attestation')
{
	my ($ret, $out, $err) = $node->psql('d', "DROP TABLE kingres.$t");
	like($err, qr/cannot drop table "$t"/, "DROP TABLE $t is refused");
	($ret, $out, $err) = $node->psql('d', "TRUNCATE kingres.$t");
	like($err, qr/append-only ledger table/, "TRUNCATE $t is refused");
	($ret, $out, $err) = $node->psql('d', "ALTER TABLE kingres.$t ADD COLUMN x int");
	like($err, qr/KL002|cannot alter table/, "ALTER TABLE $t is refused");
	($ret, $out, $err) = $node->psql('d', "DELETE FROM kingres.$t");
	like($err, qr/append-only ledger table/, "DELETE from $t is refused");
}

#--------------------------------------------------------------------------
# Keys
#--------------------------------------------------------------------------
sub register
{
	my ($key, $role, $label) = @_;
	my $r = defined $role ? "'$role'" : 'NULL';
	return db(sprintf(q{SELECT encode(kingres.ledger_client_key_register(decode('%s', 'hex'), %s, '%s'), 'hex')},
			hex_of($key->{pub}), $r, $label));
}
is(register($k1, undef, 'key one'), hex_of($k1->{key_id}),
	'a registered key has the identifier SHA-256(public key)[:16], computed independently here');
is(register($k2, 'app', 'key two'), hex_of($k2->{key_id}), 'a key limited to a role');
is(register($k3, 'other', 'key three'), hex_of($k3->{key_id}), 'and another');
is(db(q{SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table t USING (table_id)
	WHERE t.relid = 'kingres.ledger_client_key'::regclass}), '3',
	'each registration is an entry of the chain (written by the capture trigger)');

#--------------------------------------------------------------------------
# A session of the role app, with the state of the ledger it needs
#--------------------------------------------------------------------------
sub session_as
{
	my ($role) = @_;
	local $ENV{PGUSER} = $role;
	my $s = $node->background_psql('d', on_error_stop => 0);
	$s->query_safe(q{\set VERBOSITY sqlstate});
	return $s;
}

# The hash of a row of docs/items as the server computes it (a stateless function).
sub row_hash
{
	my ($table, $id, $body) = @_;
	return db(sprintf(q{SELECT encode(kingres.ledger_row_hash(ROW(%d, '%s')::%s), 'hex')}, $id, $body, $table));
}

# The SQL of one call to ledger_attest, signed with $key.  Overrides: nonce, key_id,
# chain, table_id, signature (raw bytes), message_op.
sub attest_sql
{
	my ($key, $table, $op, $hash_hex, %o) = @_;
	my $nonce = $o{nonce} // random_nonce();
	my $kid = $o{key_id} // $key->{key_id};
	my $msg = message_bytes(
		chain_id => $o{chain} // $chain,
		table_id => $o{table_id} // $tid{$table},
		op => $o{message_op} // $op,
		row_hash => unhex_of($hash_hex),
		key_id => $kid,
		nonce => $nonce);
	my $sig = $o{signature} // sign_message($key, $msg);
	return sprintf(q{SELECT kingres.ledger_attest('%s'::regclass, decode('%s', 'hex'), '%s', decode('%s', 'hex'), decode('%s', 'hex'), decode('%s', 'hex'))},
		$table, $hash_hex, $op, hex_of($kid), hex_of($nonce), hex_of($sig));
}

# One query that is expected to fail: the SQLSTATE line of the error, or ''.
sub failure_of
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	$s->query($sql);
	my ($state) = $s->{stderr} =~ /ERROR:\s+(\S+)/;
	$s->{stderr} = '';
	return $state // '';
}

# One call that is expected to fail inside a transaction that goes on: a savepoint takes the error.
sub fails_in_txn
{
	my ($s, $sql) = @_;
	$s->query_safe('SAVEPOINT kl_e');
	my $state = failure_of($s, $sql);
	$s->query_safe('ROLLBACK TO kl_e');
	return $state;
}

# The server computes the message too: it equals the one encoded here.
{
	my $h = row_hash('docs', 1, 'a');
	my $n = random_nonce();
	my $mine = hex_of(message_bytes(chain_id => $chain, table_id => $tid{docs}, op => 'I',
			row_hash => unhex_of($h), key_id => $k1->{key_id}, nonce => $n));
	is(db(sprintf(q{SELECT encode(kingres.ledger_attestation_message(decode('%s', 'hex'), %d, 'I', decode('%s', 'hex'), decode('%s', 'hex'), decode('%s', 'hex')), 'hex')},
				hex_of($chain), $tid{docs}, $h, hex_of($k1->{key_id}), hex_of($n))),
		$mine, 'ledger_attestation_message is the encoding of the design, byte for byte, written independently here');
	is(length($mine) / 2, 136, 'and has 136 bytes');
}

my $app = session_as('app');

#--------------------------------------------------------------------------
# The honest path
#--------------------------------------------------------------------------
{
	my $h = row_hash('docs', 1, 'one');
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (1, 'one')});
	$app->query_safe(attest_sql($k1, 'docs', 'I', $h));
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM kingres.ledger_attestation}), '1', 'an attested write: one attestation row');
	is(db(q{SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table t USING (table_id)
		WHERE t.relid = 'kingres.ledger_attestation'::regclass}), '1',
		'whose own entry is in the chain');
	db(q{SELECT kingres.ledger_build_block()});
	is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies with the attestation in a block');
	my $dig = db(q{SELECT kingres.ledger_generate_digest()});
	is(db(qq{SELECT kingres.ledger_verify_proof(kingres.ledger_inclusion_proof('kingres.ledger_attestation',
		kingres.ledger_row_hash(a.*)), '$dig') FROM kingres.ledger_attestation a}), 't',
		'the attestation has an ordinary inclusion proof that verifies against a digest');
	is(db(q{SELECT encode(a.key_id, 'hex') || ' ' || a.seq FROM kingres.ledger_attestation a}),
		hex_of($k1->{key_id}) . ' 0', 'the row names the key and the entry (seq 0)');
}

#--------------------------------------------------------------------------
# Every error
#--------------------------------------------------------------------------
{
	my $h = row_hash('docs', 2, 'two');
	$app->query_safe('BEGIN');
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'I', $h)), 'KL011', 'KL011: no entry yet (the row is not written)');
	$app->query_safe('ROLLBACK');

	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (2, 'two')});
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'D', $h)), 'KL011', 'KL011: the operation is not the one of the entry');
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'I', row_hash('docs', 2, 'other'))), 'KL011',
		'KL011: another hash');
	$app->query_safe('ROLLBACK');
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (2, 'two')});
	$app->query_safe(attest_sql($k1, 'docs', 'I', $h));
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'I', $h)), 'KL011', 'KL011: the entry is attested already');
	$app->query_safe('ROLLBACK');

	# KL012
	my $unknown = new_key($dir, 'unknown');
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (2, 'two')});
	is(fails_in_txn($app, attest_sql($unknown, 'docs', 'I', $h)), 'KL012', 'KL012: an unregistered key');
	$app->query_safe('ROLLBACK');
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (2, 'two')});
	is(fails_in_txn($app, attest_sql($k3, 'docs', 'I', $h)), 'KL012', 'KL012: a key that names another role');
	$app->query_safe(attest_sql($k2, 'docs', 'I', $h));
	$app->query_safe('ROLLBACK');

	# KL013
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (2, 'two')});
	my $good = attest_sql($k1, 'docs', 'I', $h, nonce => 'N' x 16);
	my $bad = sign_message($k1, message_bytes(chain_id => $chain, table_id => $tid{docs}, op => 'I',
			row_hash => unhex_of($h), key_id => $k1->{key_id}, nonce => 'N' x 16));
	substr($bad, 5, 1) = chr(ord(substr($bad, 5, 1)) ^ 1);
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'I', $h, nonce => 'N' x 16, signature => $bad)), 'KL013',
		'KL013: one bit of the signature changed');
	my $other_chain = 'C' x 32;
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'I', $h, chain => $other_chain)), 'KL013',
		'KL013: a signature made for another chain');
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'I', $h, table_id => $tid{docs} + 1)), 'KL013',
		'KL013: a signature made for another table');
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'I', $h, message_op => 'D')), 'KL013',
		'KL013: a signature made for the removal of the row');
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'I', $h, signature => 'S' x 63)), 'KL013',
		'KL013: a signature that is not 64 bytes');
	is(fails_in_txn($app, attest_sql($k2, 'docs', 'I', $h, key_id => $k1->{key_id})), 'KL013',
		'KL013: a signature of another key');
	$app->query_safe($good);
	pass('and the good one is accepted after all that (a failed call leaves nothing)');
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM kingres.ledger_attestation}), '2', 'two attestations now');

	# KL014: the same key and nonce for another entry (the signature covers the nonce, so it is valid).
	my $h3 = row_hash('docs', 3, 'three');
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (3, 'three')});
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'I', $h3, nonce => 'N' x 16)), 'KL014',
		'KL014: a nonce that this key used before');
	$app->query_safe(attest_sql($k2, 'docs', 'I', $h3, nonce => 'N' x 16));
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM kingres.ledger_attestation}), '3',
		'the same nonce with another key is another pair, and is accepted');

	# Revoked
	db(sprintf(q{SELECT kingres.ledger_client_key_revoke(decode('%s', 'hex'), 'lost')}, hex_of($k1->{key_id})));
	my $h4 = row_hash('docs', 4, 'four');
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (4, 'four')});
	is(fails_in_txn($app, attest_sql($k1, 'docs', 'I', $h4)), 'KL012', 'KL012: a revoked key');
	$app->query_safe('ROLLBACK');
	is(db(q{SELECT client_keys_active FROM kingres.ledger_status}), '2', 'ledger_status counts the active keys');
	my ($ret, $out, $err) = $node->psql('d', sprintf(q{SELECT kingres.ledger_client_key_revoke(decode('%s', 'hex'), 'again')}, hex_of($k1->{key_id})));
	like($err, qr/revoked already/, 'a key is revoked once');
	($ret, $out, $err) = $node->psql('d', q{SELECT kingres.ledger_client_key_revoke(decode('00', 'hex'), 'x')});
	like($err, qr/key_id must be 16 bytes/, 'a malformed key identifier is refused');
	($ret, $out, $err) = $node->psql('d', sprintf(q{SELECT kingres.ledger_client_key_register(decode('%s', 'hex'), NULL, 'again')}, hex_of($k1->{pub})));
	like($err, qr/registered already/, 'a key, revoked or not, is registered once');
	($ret, $out, $err) = $node->psql('d', q{SELECT kingres.ledger_client_key_register(decode('00', 'hex'), NULL, 'x')});
	like($err, qr/public_key must be 32 bytes/, 'a public key of the wrong length is refused');
	($ret, $out, $err) = $node->psql('d', sprintf(q{SELECT kingres.ledger_client_key_register(decode('%s', 'hex'), 'nobody_here', 'x')}, '01' x 32));
	like($err, qr/role "nobody_here" does not exist/, 'a key for a role that does not exist is refused');
	($ret, $out, $err) = $node->psql('d', sprintf(q{SELECT kingres.ledger_client_key_register(decode('%s', 'hex'), NULL, repeat('x', 201))}, '02' x 32));
	like($err, qr/label is 201 characters/, 'a label of 201 characters is refused');
}

# The guard: the role that attests cannot write the tables in any other way.
foreach my $sql (
	q{INSERT INTO kingres.ledger_attestation VALUES (1, 1, 'a', 'b', 'c')},
	q{INSERT INTO kingres.ledger_client_key (key_id, algorithm, public_key, label, registered_by, registered_at) VALUES ('x', 'ed25519', 'y', 'l', 'app', now())},
	q{DELETE FROM kingres.ledger_attestation},
	q{UPDATE kingres.ledger_client_key SET label = 'x'})
{
	my $st = failure_of($app, $sql);
	like($st, qr/^(42501|KL001)$/, "refused for the role that attests: $sql");
}

#--------------------------------------------------------------------------
# The policy "required"
#--------------------------------------------------------------------------
{
	$app->{stderr} = '';
	$app->query_safe(q{SELECT kingres.ledger_set_signature_policy('docs', 'required')});
	is(db(q{SELECT signature_policy || ' ' || (signature_required_from IS NOT NULL) FROM kingres.ledger_table WHERE relid = 'docs'::regclass}),
		'required true', 'the owner switched docs to required, and the switch is dated');
	is(db(q{SELECT required_tables FROM kingres.ledger_status}), '1', 'ledger_status counts it');
	is(failure_of($app, q{SELECT kingres.ledger_set_signature_policy('docs', 'optional')}), '0A000',
		'there is no way back: optional is refused for a required table');
	is(failure_of($app, q{SELECT kingres.ledger_set_signature_policy('docs', 'sometimes')}), '22023', 'an unknown policy');
	{
		my ($r, $o, $e) = $node->psql('d', q{SELECT kingres.ledger_set_signature_policy('kingres.ledger_attestation', 'required')});
		like($e, qr/has no signature policy/, 'the tables of the ledger itself have none (the owner of the extension is refused too)');
	}
	is(failure_of($app, q{SELECT kingres.ledger_set_signature_policy('kingres.ledger_attestation', 'required')}), '42501',
		'and a role that does not own them is refused first');
	is(failure_of($app, q{SELECT kingres.ledger_set_signature_policy('pg_class', 'required')}), '42501',
		'a table the caller does not own');
	my $o = session_as('other');
	is(failure_of($o, q{SELECT kingres.ledger_set_signature_policy('docs', 'required')}), '42501',
		'a role that does not own the table cannot switch it');
	$o->quit;

	my $h5 = row_hash('docs', 5, 'five');
	# no attestation: COMMIT fails with KL010 and nothing is written
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (5, 'five')});
	is(failure_of($app, 'COMMIT'), 'KL010', 'KL010: COMMIT without the attestation of a required table');
	is(db(q{SELECT count(*) FROM docs WHERE id = 5}), '0', 'and the row was not written');
	# with it
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (5, 'five')});
	$app->query_safe(attest_sql($k2, 'docs', 'I', $h5));
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM docs WHERE id = 5}), '1', 'COMMIT with it works');
	# two rows, one attested
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (6, 'six'), (7, 'seven')});
	$app->query_safe(attest_sql($k2, 'docs', 'I', row_hash('docs', 6, 'six')));
	is(failure_of($app, 'COMMIT'), 'KL010', 'KL010: one of two rows attested');
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (6, 'six'), (7, 'seven')});
	$app->query_safe(attest_sql($k2, 'docs', 'I', row_hash('docs', 6, 'six')));
	$app->query_safe(attest_sql($k2, 'docs', 'I', row_hash('docs', 7, 'seven')));
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM docs}), '6', 'both attested: written');
	# a table without the policy is not affected
	$app->query_safe(q{INSERT INTO items VALUES (1, 'x')});
	is(db(q{SELECT count(*) FROM items}), '1', 'a table with the policy optional needs no attestation');
	# a transaction that writes nothing of docs
	$app->query_safe('BEGIN');
	$app->query_safe(q{SELECT 1});
	$app->query_safe('COMMIT');

	# savepoints: the entry and its attestation go together
	my $h8 = row_hash('docs', 8, 'eight');
	$app->query_safe('BEGIN');
	$app->query_safe('SAVEPOINT s');
	$app->query_safe(q{INSERT INTO docs VALUES (8, 'eight')});
	$app->query_safe(attest_sql($k2, 'docs', 'I', $h8));
	$app->query_safe('ROLLBACK TO s');
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM docs WHERE id = 8}), '0', 'a savepoint rolled back takes the entry and its attestation: COMMIT works, nothing is written');
	# the row outside, the attestation inside the savepoint that is rolled back: the entry is owed again
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (8, 'eight')});
	$app->query_safe('SAVEPOINT s');
	$app->query_safe(attest_sql($k2, 'docs', 'I', $h8));
	$app->query_safe('ROLLBACK TO s');
	is(failure_of($app, 'COMMIT'), 'KL010', 'KL010: the attestation went with the savepoint, the entry did not');
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (8, 'eight')});
	$app->query_safe('SAVEPOINT s');
	$app->query_safe(attest_sql($k2, 'docs', 'I', $h8));
	$app->query_safe('ROLLBACK TO s');
	$app->query_safe(attest_sql($k2, 'docs', 'I', $h8));
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM docs WHERE id = 8}), '1', 'attested again after the rollback: COMMIT works');
	# a savepoint that is released keeps both
	my $h9 = row_hash('docs', 9, 'nine');
	$app->query_safe('BEGIN');
	$app->query_safe('SAVEPOINT s');
	$app->query_safe(q{INSERT INTO docs VALUES (9, 'nine')});
	$app->query_safe('SAVEPOINT t');
	$app->query_safe(attest_sql($k2, 'docs', 'I', $h9));
	$app->query_safe('RELEASE t');
	$app->query_safe('RELEASE s');
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM docs WHERE id = 9}), '1', 'released savepoints keep the entry and the attestation');
	# a nested savepoint that was released into one that is rolled back
	$app->query_safe('BEGIN');
	$app->query_safe('SAVEPOINT s');
	$app->query_safe('SAVEPOINT t');
	$app->query_safe(q{INSERT INTO docs VALUES (10, 'ten')});
	$app->query_safe('RELEASE t');
	$app->query_safe('ROLLBACK TO s');
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM docs WHERE id = 10}), '0', 'an entry written in a released subtransaction that a parent rolled back is gone: COMMIT works');
	# an exception block of PL/pgSQL
	db(q{CREATE FUNCTION public.try_insert() RETURNS void LANGUAGE plpgsql AS $f$
		BEGIN BEGIN INSERT INTO docs VALUES (11, 'eleven'); RAISE EXCEPTION 'undo'; EXCEPTION WHEN OTHERS THEN NULL; END; END $f$});
	$app->query_safe(q{SELECT public.try_insert()});
	is(db(q{SELECT count(*) FROM docs WHERE id = 11}), '0', 'an entry written in an EXCEPTION block that caught an error is gone: the statement commits');

	# the policy applies to D as well: an updatable table
	$app->query_safe(q{SELECT kingres.ledger_set_signature_policy('items', 'required')});
	my $hold = row_hash('items', 1, 'x');
	my $hnew = row_hash('items', 1, 'y');
	$app->query_safe('BEGIN');
	$app->query_safe(q{UPDATE items SET body = 'y' WHERE id = 1});
	$app->query_safe(attest_sql($k2, 'items', 'D', $hold));
	is(failure_of($app, 'COMMIT'), 'KL010', 'an UPDATE needs two attestations: the removed version alone is not enough');
	$app->query_safe('BEGIN');
	$app->query_safe(q{UPDATE items SET body = 'y' WHERE id = 1});
	$app->query_safe(attest_sql($k2, 'items', 'D', $hold));
	$app->query_safe(attest_sql($k2, 'items', 'I', $hnew));
	$app->query_safe('COMMIT');
	is(db(q{SELECT body FROM items WHERE id = 1}), 'y', 'both: the update is written');
	$app->query_safe('BEGIN');
	$app->query_safe(q{DELETE FROM items WHERE id = 1});
	is(failure_of($app, 'COMMIT'), 'KL010', 'a DELETE needs the attestation of the removal');
	$app->query_safe('BEGIN');
	$app->query_safe(q{DELETE FROM items WHERE id = 1});
	$app->query_safe(attest_sql($k2, 'items', 'D', $hnew));
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM items}), '0', 'attested: deleted');
}

# The attestation of D for the removed version has a proof too.
db(q{SELECT kingres.ledger_build_block()});
is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies after all that');

#--------------------------------------------------------------------------
# The policy is read again when it changes inside the transaction
#--------------------------------------------------------------------------
{
	my $hl2 = db(q{SELECT encode(kingres.ledger_row_hash(ROW(2)::late), 'hex')});
	my $go = sub {
		my ($attest) = @_;
		$app->query_safe('BEGIN');
		$app->query_safe(q{INSERT INTO late VALUES (1)});	# the policy is optional: not owed
		$app->query_safe(q{SELECT kingres.ledger_set_signature_policy('late', 'required')});
		$app->query_safe(q{INSERT INTO late VALUES (2)});	# after the switch: owed
		$app->query_safe(attest_sql($k2, 'late', 'I', $hl2)) if $attest;
	};
	$go->(0);
	is(failure_of($app, 'COMMIT'), 'KL010',
		'a row written after the switch, in the transaction that made it, is owed an attestation (the policy that the transaction had read is read again)');
	is(db(q{SELECT signature_policy FROM kingres.ledger_table WHERE relid = 'late'::regclass}), 'optional',
		'and the switch went with the transaction');
	$go->(1);
	$app->query_safe('COMMIT');
	is(db(q{SELECT count(*) FROM late}), '2', 'with the attestation of that row the transaction commits (the row before the switch is not owed one)');
	db(q{SELECT kingres.ledger_build_block()});
	is(db(q{SELECT count(*) FROM kingres.ledger_verify() WHERE check_name = 'unattested_entry'}), '0',
		'and ledger_verify asks for nothing of the transaction that switched the policy, for the row before the switch');
}

#--------------------------------------------------------------------------
# A prepared transaction
#--------------------------------------------------------------------------
{
	my $h = row_hash('docs', 20, 'prepared');
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (20, 'prepared')});
	is(failure_of($app, q{PREPARE TRANSACTION 'p_unsigned'}), 'KL010',
		'KL010: PREPARE TRANSACTION with an entry that has no attestation (nobody could add it later)');
	is(db(q{SELECT count(*) FROM pg_prepared_xacts}), '0', 'nothing was prepared');
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (20, 'prepared')});
	$app->query_safe(attest_sql($k2, 'docs', 'I', $h));
	$app->query_safe(q{PREPARE TRANSACTION 'p_signed'});
	is(db(q{SELECT count(*) FROM pg_prepared_xacts}), '1', 'with the attestation it is prepared');
	is(db(q{SELECT count(*) FROM docs WHERE id = 20}), '0', 'and not visible');
	db(q{COMMIT PREPARED 'p_signed'});
	is(db(q{SELECT count(*) FROM docs WHERE id = 20}), '1', 'COMMIT PREPARED from another session');
	db(q{SELECT kingres.ledger_build_block()});
	is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'and the ledger verifies');
}

#--------------------------------------------------------------------------
# ledger_verify: what the tables say
#--------------------------------------------------------------------------
{
	# Rows written before the switch are not asked for: docs has rows with
	# attestations and rows before the switch (docs 1 to 4 were attested).
	is(db(q{SELECT count(*) FROM kingres.ledger_verify() WHERE check_name = 'unattested_entry'}), '0',
		'no unattested_entry: the entries before the switch, and every later one, are attested');
	# an entry without an attestation after the switch: bypass the policy as a superuser would
	registry_by_hand(q{UPDATE kingres.ledger_table SET signature_policy = 'optional', signature_required_from = NULL WHERE relid = 'docs'::regclass});
	$app = session_as('app');
	$app->query_safe(q{INSERT INTO docs VALUES (30, 'unsigned')});
	registry_by_hand(q{UPDATE kingres.ledger_table SET signature_policy = 'required', signature_required_from = 0 WHERE relid = 'docs'::regclass});
	db(q{SELECT kingres.ledger_build_block()});
	is(db(q{SELECT count(*) FROM kingres.ledger_verify() WHERE check_name = 'unattested_entry'}), '1',
		'a row written while the policy was off (a superuser switched it back) is reported once the policy is on again and dated before it');
}

#--------------------------------------------------------------------------
# Crash recovery
#--------------------------------------------------------------------------
{
	$node->safe_psql('d', q{SELECT kingres.ledger_set_signature_policy('docs', 'required')}, extra_params => [ '-U', 'app' ]);
	my $before = db(q{SELECT count(*) FROM kingres.ledger_attestation});
	my $h = row_hash('docs', 40, 'crash');
	my $s = session_as('app');
	$s->query_safe('BEGIN');
	$s->query_safe(q{INSERT INTO docs VALUES (40, 'crash')});
	$s->query_safe(attest_sql($k2, 'docs', 'I', $h));
	$s->query_safe('COMMIT');
	# one more that is never committed
	my $u = session_as('app');
	$u->query_safe('BEGIN');
	$u->query_safe(q{INSERT INTO docs VALUES (41, 'lost')});
	$u->query_safe(attest_sql($k2, 'docs', 'I', row_hash('docs', 41, 'lost')));
	$node->stop('immediate');
	$node->start;
	is(db(q{SELECT count(*) FROM docs WHERE id = 40}), '1', 'after a crash the committed row is there');
	is(db(q{SELECT count(*) FROM docs WHERE id = 41}), '0', 'the uncommitted one is not');
	is(db(q{SELECT count(*) FROM kingres.ledger_attestation}), $before + 1,
		'and exactly the committed attestation is');
	is(db(q{SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table t USING (table_id)
		WHERE t.relid = 'kingres.ledger_attestation'::regclass}), $before + 1, 'with its entry');
	is(db(q{SELECT count(*) FROM kingres.ledger_verify() WHERE check_name <> 'unattested_entry'}), '0',
		'the ledger verifies after the crash recovery');
}

#--------------------------------------------------------------------------
# A standby
#--------------------------------------------------------------------------
{
	$node->backup('b1');
	my $standby = PostgreSQL::Test::Cluster->new('attest_standby');
	$standby->init_from_backup($node, 'b1', has_streaming => 1);
	$standby->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_ledger'");
	$standby->start;
	my $h = row_hash('docs', 50, 'after');
	my $s = session_as('app');
	$s->query_safe('BEGIN');
	$s->query_safe(q{INSERT INTO docs VALUES (50, 'after')});
	$s->query_safe(attest_sql($k2, 'docs', 'I', $h));
	$s->query_safe('COMMIT');
	$node->wait_for_catchup($standby);
	is($standby->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_attestation}),
		db(q{SELECT count(*) FROM kingres.ledger_attestation}), 'the standby replays the attestations');
	my ($ret, $out, $err) = $standby->psql('d', attest_sql($k2, 'docs', 'I', $h));
	isnt($ret, 0, 'ledger_attest on a standby fails');
	like($err, qr/read-only|recovery|no ledger entry|written no ledger entry/, 'with an error (the server is in recovery)');
	($ret, $out, $err) = $standby->psql('d', q{SELECT kingres.ledger_client_key_revoke(decode('00', 'hex'), 'x')});
	isnt($ret, 0, 'and so do the functions of the registry');
	is($standby->safe_psql('d', q{SELECT client_keys_active || ' ' || attestations FROM kingres.ledger_status}),
		db(q{SELECT client_keys_active || ' ' || attestations FROM kingres.ledger_status}),
		'ledger_status of the standby has the same numbers');
	$standby->stop;
}

done_testing();
