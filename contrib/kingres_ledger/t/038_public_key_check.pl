# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/038_public_key_check.pl
#
# L21-01 of the review of client signatures: plain Ed25519 verification accepts
# a signature that nobody made under a public key of small order (for the
# identity point R = identity, S = 0 verifies for every message), and one point
# has several encodings, so one point could have several key identifiers.  The
# registry now accepts only the canonical encoding of a point of the curve that
# does not have small order (src/ed25519_check.c, 22023 at registration), and
# ledger_attest refuses such a key again if a row of one exists (KL012), on every
# call of a session (L22-03).  The cache of judged keys (L22-02) judges each key
# once per session for more keys than it used to hold, and holds nothing about
# a refused key or about revocation.
#
# The vectors are the eight points of order 1, 2, 4 and 8 in their canonical
# encodings, the encodings that name the same points in other ways (y not below
# 2^255 - 19, the sign bit set on a zero x), values that are not on the curve,
# and a key that is good.  They were computed with Python integers, from the
# definitions of the curve, independently of the C code.
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

my $node = PostgreSQL::Test::Cluster->new('key_check');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;
my $dir = PostgreSQL::Test::Utils::tempdir;

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT EXECUTE ON FUNCTION kingres.ledger_attest(regclass, bytea, "char", bytea, bytea, bytea) TO app;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	ALTER TABLE docs OWNER TO app;
	SELECT kingres.ledger_enable('docs');
});
sub db { return $node->safe_psql('d', @_); }

sub register
{
	my ($hex) = @_;
	return $node->psql('d', sprintf(q{SELECT kingres.ledger_client_key_register(decode('%s', 'hex'), NULL, 'k')}, $hex));
}

my $z31 = '00' x 31;
my @bad = (
	# the eight points of order dividing 8, canonical encodings
	[ 'the identity point (order 1)', '01' . $z31, 'small order' ],
	[ 'the point (0, -1) (order 2)', 'ec' . ('ff' x 30) . '7f', 'small order' ],
	[ 'the all-zero key: y = 0, x positive (order 4)', '00' x 32, 'small order' ],
	[ 'y = 0, x negative (order 4)', ('00' x 31) . '80', 'small order' ],
	[ 'an order 8 point', '26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05', 'small order' ],
	[ 'an order 8 point (the sign bit set)', '26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85', 'small order' ],
	[ 'an order 8 point (the other y)', 'c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a', 'small order' ],
	[ 'an order 8 point (the other y, the sign bit set)', 'c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa', 'small order' ],
	# the same points under other encodings
	[ 'the identity with the sign bit set (x = 0 has no sign)', '01' . ('00' x 30) . '80', 'canonical' ],
	[ 'the point (0, -1) with the sign bit set', 'ec' . ('ff' x 31), 'canonical' ],
	[ 'y = p + 1, an alias of the identity', 'ee' . ('ff' x 30) . '7f', 'canonical' ],
	[ 'y = p + 1 with the sign bit set', 'ee' . ('ff' x 31), 'canonical' ],
	[ 'y = p, an alias of y = 0', 'ed' . ('ff' x 30) . '7f', 'canonical' ],
	[ 'y = p with the sign bit set', 'ed' . ('ff' x 31), 'canonical' ],
	[ 'y = 2^255 - 1', 'ff' x 32, 'canonical' ],
	[ 'y = p + 3, an alias of the good point y = 3', 'f0' . ('ff' x 30) . '7f', 'canonical' ],
	# not on the curve
	[ 'y = 2 (no x)', '02' . $z31, 'point of the curve' ],
	[ 'y = 7 (no x)', '07' . $z31, 'point of the curve' ],
	[ 'y = 2 with the sign bit set', '02' . ('00' x 30) . '80', 'point of the curve' ],
);
foreach my $v (@bad)
{
	my ($label, $hex, $why) = @$v;
	my ($ret, $out, $err) = register($hex);
	isnt($ret, 0, "refused: $label");
	like($err, qr/ERROR:\s+public_key is not an acceptable Ed25519 public key/, "  with the message of the registry ($label)");
	like($err, qr/\Q$why\E/, "  and the reason: $why");
}
is(db(q{SELECT count(*) FROM kingres.ledger_client_key}), '0', 'nothing was registered by any of them');
is(db(q{SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table t USING (table_id)
	WHERE t.relid = 'kingres.ledger_client_key'::regclass}), '0', 'and no entry was made');
{
	my ($ret, $out, $err) = register('01' . $z31);
	my $st = $node->psql('d', q{\set VERBOSITY sqlstate
SELECT kingres.ledger_client_key_register(decode('01' || repeat('00', 31), 'hex'), NULL, 'k')}, stderr => \my $e2);
	like($e2, qr/22023/, 'with SQLSTATE 22023');
}

#--------------------------------------------------------------------------
# A good key is accepted, and has one identifier
#--------------------------------------------------------------------------
my $good = new_key($dir, 'good');
{
	my ($ret, $out, $err) = register(hex_of($good->{pub}));
	is($ret, 0, 'a key made by openssl is accepted');
	is($out, '\x' . hex_of($good->{key_id}), 'its identifier is SHA-256(public key)[:16]');
	($ret, $out, $err) = register('03' . $z31);
	is($ret, 0, 'another point of the curve (y = 3) is accepted too: the check asks for a point of large order, not for a known private key');
	($ret, $out, $err) = register('f0' . ('ff' x 30) . '7f');
	isnt($ret, 0, 'and the alias y = p + 3 of that point is refused, so that the point has one identifier');
}

#--------------------------------------------------------------------------
# ledger_attest refuses such a key again: a row that exists already.  The
# refusal is repeated on every call of the session (L22-03): a refused key is
# never remembered as acceptable, or the forgery would work from the second
# call on.
#--------------------------------------------------------------------------
my $chain = unhex_of(db(q{SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain}));
my $table_id = db(q{SELECT table_id FROM kingres.ledger_table WHERE relid = 'docs'::regclass});
my $forged_hash = db(q{SELECT encode(kingres.ledger_row_hash(ROW(1, 'forged')::docs), 'hex')});

# A row that the registry would not have written is put there by hand, by a superuser with the setting.
sub hand_key
{
	my ($pub) = @_;
	my $kid = db(qq{SELECT encode(substr(sha256(decode('$pub', 'hex')), 1, 16), 'hex')});
	my ($ret, $out, $err) = $node->psql('d', qq{SET kingres_ledger.allow_manual_write = on;
		INSERT INTO kingres.ledger_client_key (key_id, algorithm, public_key, role_name, label, registered_by, registered_at)
		VALUES (decode('$kid', 'hex'), 'ed25519', decode('$pub', 'hex'), NULL, 'by hand', current_user, now())});
	is($ret, 0, "a row for the key $pub is put in by hand") or diag($err);
	return $kid;
}

sub attest_stmt
{
	my ($hash, $kid, $sig_hex) = @_;
	return sprintf(q{SELECT kingres.ledger_attest('docs'::regclass, decode('%s', 'hex'), 'I', decode('%s', 'hex'), decode('%s', 'hex'), decode('%s', 'hex'))},
		$hash, $kid, hex_of(random_nonce()), $sig_hex);
}

sub app_session
{
	my ($verbosity) = @_;
	local $ENV{PGUSER} = 'app';
	my $s = $node->background_psql('d', on_error_stop => 0);
	$s->query_safe("\\set VERBOSITY $verbosity");
	return $s;
}

# The state of the error of one statement.
sub state_of
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	$s->query($sql);
	my ($state) = $s->{stderr} =~ /ERROR:\s+(\S+)/;
	$s->{stderr} = '';
	return $state // '';
}

foreach my $case (
	[ 'the identity key', '01' . $z31, '01' . ('00' x 63) ],	# R = the identity point, S = 0: valid for every message under that key
	[ 'the all-zero key', '00' x 32, '00' x 64 ])
{
	my ($label, $pub, $sig) = @$case;
	my $kid = hand_key($pub);
	my $app = app_session('sqlstate');

	# two transactions
	foreach my $n (1, 2)
	{
		$app->query_safe('BEGIN');
		$app->query_safe(q{INSERT INTO docs VALUES (1, 'forged')});
		is(state_of($app, attest_stmt($forged_hash, $kid, $sig)), 'KL012',
			"$label: ledger_attest is refused (KL012) in transaction $n of the session, not KL013 and not an acceptance");
		$app->query_safe('ROLLBACK');
	}
	# two savepoints of one transaction
	$app->query_safe('BEGIN');
	$app->query_safe(q{INSERT INTO docs VALUES (1, 'forged')});
	foreach my $n (1, 2, 3)
	{
		$app->query_safe("SAVEPOINT s$n");
		is(state_of($app, attest_stmt($forged_hash, $kid, $sig)), 'KL012',
			"$label: and in savepoint $n of one transaction");
		$app->query_safe("ROLLBACK TO s$n");
	}
	$app->query_safe('ROLLBACK');
	$app->quit;
}
is(db(q{SELECT count(*) FROM kingres.ledger_attestation}), '0', 'no attestation was written by any of the calls');

#--------------------------------------------------------------------------
# The judgment of a key is paid once per key and session (L22-02): twelve keys,
# more than the eight that an earlier cache held, are used in rotation by one
# session.  The server writes a DEBUG1 message for each judgment that the cache
# does not answer, with the count of the session; the test counts them, so the
# verdict does not depend on the speed of the machine.
#--------------------------------------------------------------------------
{
	my @keys = map { new_key($dir, "rot$_") } (1 .. 12);
	foreach my $k (@keys)
	{
		db(sprintf(q{SELECT kingres.ledger_client_key_register(decode('%s', 'hex'), NULL, 'rotation')}, hex_of($k->{pub})));
	}
	my $bad_sig = '00' x 64;		# ends in KL013 after the key check: no entry is made
	my $app = app_session('verbose');
	$app->query('SET client_min_messages = debug1');
	$app->query('BEGIN');
	$app->query(q{INSERT INTO docs VALUES (1, 'forged')});
	$app->{stderr} = '';
	my $calls = 0;
	foreach my $round (1 .. 3)
	{
		foreach my $k (@keys)
		{
			$app->query('SAVEPOINT r');
			$app->query(attest_stmt($forged_hash, hex_of($k->{key_id}), $bad_sig));
			$app->query('ROLLBACK TO r');
			$calls++;
		}
	}
	my @judged = $app->{stderr} =~ /public key judged \(judgment (\d+) of this session\)/g;
	my $errors = () = $app->{stderr} =~ /ERROR:\s+KL013/g;
	is($errors, $calls, "ledger_attest ran the key check and the signature check $calls times (KL013 each time)");
	is(scalar(@judged), 12, 'twelve keys in rotation, three rounds: twelve judgments in the session, not one per call');
	is(join(',', @judged), join(',', 1 .. 12), 'and they are the first twelve of the session');

	# a refused key is judged again on every call
	my $kid = db(q{SELECT encode(key_id, 'hex') FROM kingres.ledger_client_key WHERE label = 'by hand' ORDER BY registered_at LIMIT 1});
	$app->{stderr} = '';
	foreach my $n (1, 2)
	{
		$app->query('SAVEPOINT r');
		$app->query(attest_stmt($forged_hash, $kid, '01' . ('00' x 63)));
		$app->query('ROLLBACK TO r');
	}
	@judged = $app->{stderr} =~ /public key judged \(judgment (\d+) of this session\)/g;
	is(join(',', @judged), '13,14', 'a refused key is judged on every call of the session');
	like($app->{stderr}, qr/KL012/, 'and refused each time');

	# the cache holds nothing about revocation: a key that is revoked is refused at once
	db(sprintf(q{SELECT kingres.ledger_client_key_revoke(decode('%s', 'hex'), 'lost')}, hex_of($keys[0]->{key_id})));
	$app->{stderr} = '';
	$app->query('SAVEPOINT r');
	$app->query(attest_stmt($forged_hash, hex_of($keys[0]->{key_id}), $bad_sig));
	$app->query('ROLLBACK TO r');
	like($app->{stderr}, qr/this client key is revoked/, 'a key that was judged acceptable and is revoked afterwards is refused by the table, not by the cache');
	unlike($app->{stderr}, qr/public key judged/, '  without a new judgment (the cache answered the judgment of the key, not its revocation)');
	$app->query('ROLLBACK');
	$app->quit;
}

done_testing();
