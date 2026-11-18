# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/033_attest_concurrency.pl
#
# Client signatures with more than one session at a time (src/sign.c):
#
#   * the same (key, nonce) in two transactions that overlap: the second waits
#     for the first and gets KL014 when it commits, and succeeds when it rolls
#     back;
#   * a key that is revoked and committed while a REPEATABLE READ transaction
#     that began before it is open: the transaction is refused (KL012), because
#     the key is read with the latest snapshot;
#   * the same key registered by two sessions at once: one registers it, the
#     other is told it is registered already;
#   * four writers on a table with the policy required, each attesting every
#     row it writes, while blocks are built: every row has its attestation, no
#     call fails, the ledger verifies, and every attestation row is in a block.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run;
use KlSign qw(have_openssl new_key sign_message message_bytes hex_of unhex_of random_nonce);

plan skip_all => 'the openssl command (3.0 or later) is needed to make Ed25519 signatures'
  unless have_openssl();

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('attest_conc');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "deadlock_timeout = 300ms\n");
$node->start;

my $dir = PostgreSQL::Test::Utils::tempdir;
my $k1 = new_key($dir, 'k1');
my $k2 = new_key($dir, 'k2');

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT EXECUTE ON FUNCTION kingres.ledger_attest(regclass, bytea, "char", bytea, bytea, bytea),
		kingres.ledger_set_signature_policy(regclass, text),
		kingres.ledger_client_key_register(bytea, name, text) TO app;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	ALTER TABLE docs OWNER TO app;
	SELECT kingres.ledger_enable('docs');
});

sub db { return $node->safe_psql('d', @_); }

my $chain = unhex_of(db(q{SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain}));
my $table_id = db(q{SELECT table_id FROM kingres.ledger_table WHERE relid = 'docs'::regclass});

foreach my $k ($k1, $k2)
{
	db(sprintf(q{SELECT kingres.ledger_client_key_register(decode('%s', 'hex'), NULL, '%s')}, hex_of($k->{pub}), $k->{name}));
}

sub session_as
{
	my ($role) = @_;
	local $ENV{PGUSER} = $role;
	my $s = $node->background_psql('d', on_error_stop => 0);
	$s->query_safe(q{\set VERBOSITY sqlstate});
	return $s;
}

sub row_hash
{
	my ($id, $body) = @_;
	return db(sprintf(q{SELECT encode(kingres.ledger_row_hash(ROW(%d, '%s')::docs), 'hex')}, $id, $body));
}

sub attest_sql
{
	my ($key, $hash_hex, $nonce) = @_;
	$nonce //= random_nonce();
	my $msg = message_bytes(chain_id => $chain, table_id => $table_id, op => 'I',
		row_hash => unhex_of($hash_hex), key_id => $key->{key_id}, nonce => $nonce);
	my $sig = sign_message($key, $msg);
	return sprintf(q{SELECT kingres.ledger_attest('docs', decode('%s', 'hex'), 'I', decode('%s', 'hex'), decode('%s', 'hex'), decode('%s', 'hex'))},
		$hash_hex, hex_of($key->{key_id}), hex_of($nonce), hex_of($sig));
}

sub waiting_backends
{
	return $node->poll_query_until('d', q{SELECT count(*) >= 1 FROM pg_stat_activity
		WHERE wait_event_type = 'Lock' AND datname = 'd'});
}

#--------------------------------------------------------------------------
# The same key and nonce in two transactions
#--------------------------------------------------------------------------
for my $round (1 .. 2)
{
	my $nonce = chr(64 + $round) x 16;
	my $a = session_as('app');
	my $b = session_as('app');
	my ($ida, $idb) = ($round * 10 + 1, $round * 10 + 2);
	$a->query_safe('BEGIN');
	$a->query_safe(qq{INSERT INTO docs VALUES ($ida, 'a')});
	$a->query_safe(attest_sql($k1, row_hash($ida, 'a'), $nonce));
	$b->query_safe('BEGIN');
	$b->query_safe(qq{INSERT INTO docs VALUES ($idb, 'b')});
	$b->query_until(qr/b_started/, "\\echo b_started\n" . attest_sql($k1, row_hash($idb, 'b'), $nonce) . ";\n");
	ok(waiting_backends(), "round $round: the second call waits for the first, which has the same key and nonce");
	if ($round == 1)
	{
		$a->query_safe('COMMIT');
		$b->{stderr} = '';
		$b->query('SELECT 1');
		like($b->{stderr}, qr/KL014/, 'the first committed: the second gets KL014');
		$b->{stderr} = '';
		$b->query('ROLLBACK');
		is(db(q{SELECT count(*) FROM kingres.ledger_attestation}), '1', 'one attestation');
	}
	else
	{
		$a->query_safe('ROLLBACK');
		$b->{stderr} = '';
		$b->query('SELECT 1');
		is($b->{stderr}, '', 'the first rolled back: the second is accepted');
		$b->query_safe('COMMIT');
		is(db(q{SELECT count(*) FROM kingres.ledger_attestation}), '2', 'two attestations');
	}
	$a->quit;
	$b->quit;
}
# the nonce of the second round belongs to the second key's turn: it is a different (key, nonce) pair
is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies');

#--------------------------------------------------------------------------
# A revocation that commits while a transaction with an older snapshot is open
#--------------------------------------------------------------------------
{
	my $a = session_as('app');
	$a->query_safe('BEGIN ISOLATION LEVEL REPEATABLE READ');
	$a->query_safe('SELECT count(*) FROM docs');		# the snapshot
	db(sprintf(q{SELECT kingres.ledger_client_key_revoke(decode('%s', 'hex'), 'compromised')}, hex_of($k2->{key_id})));
	$a->query_safe(q{INSERT INTO docs VALUES (31, 'late')});
	$a->{stderr} = '';
	$a->query(attest_sql($k2, row_hash(31, 'late')));
	like($a->{stderr}, qr/KL012/, 'a revoked key is refused to a REPEATABLE READ transaction that began before the revocation');
	$a->query('ROLLBACK');
	$a->quit;
}

#--------------------------------------------------------------------------
# The same key registered by two sessions at once
#--------------------------------------------------------------------------
{
	my $k3 = new_key($dir, 'k3');
	my $reg = sprintf(q{SELECT kingres.ledger_client_key_register(decode('%s', 'hex'), NULL, 'k3')}, hex_of($k3->{pub}));
	my $a = session_as('app');
	my $b = session_as('app');
	$a->query_safe('BEGIN');
	$a->query_safe($reg);
	$b->query_until(qr/b_started/, "\\echo b_started\n$reg;\n");
	ok(waiting_backends(), 'the second registration of the same key waits for the first');
	$a->query_safe('COMMIT');
	$b->{stderr} = '';
	$b->query('SELECT 1');
	like($b->{stderr}, qr/23505/, 'and is told that the key is registered already');
	is(db(sprintf(q{SELECT count(*) FROM kingres.ledger_client_key WHERE key_id = decode('%s', 'hex')}, hex_of($k3->{key_id}))), '1',
		'the key is registered once');
	$a->quit;
	$b->quit;
}

#--------------------------------------------------------------------------
# Four writers on a table with the policy required, and the builder
#--------------------------------------------------------------------------
{
	$node->safe_psql('d', q{SELECT kingres.ledger_set_signature_policy('docs', 'required')}, extra_params => [ '-U', 'app' ]);
	my $per = 25;
	my @procs;
	my @err;
	for my $w (0 .. 3)
	{
		my $file = "$dir/writer$w.sql";
		open(my $fh, '>', $file) or die;
		print $fh "\\set ON_ERROR_STOP on\n";
		for my $i (1 .. $per)
		{
			my $id = 1000 + $w * 100 + $i;
			my $body = "w$w-$i";
			print $fh "BEGIN;\nINSERT INTO docs VALUES ($id, '$body');\n" . attest_sql($k1, row_hash($id, $body)) . ";\nCOMMIT;\n";
		}
		close($fh);
		push @err, '';
	}
	my $builder = "$dir/builder.sql";
	open(my $bf, '>', $builder) or die;
	print $bf "\\set ON_ERROR_STOP on\n";
	print $bf "SELECT kingres.ledger_build_block();\nSELECT pg_sleep(0.1);\n" for 1 .. 40;
	close($bf);

	my @h;
	for my $w (0 .. 3)
	{
		my $h = IPC::Run::start(
			[ 'psql', '-X', '-q', '-d', $node->connstr('d'), '-U', 'app', '-f', "$dir/writer$w.sql" ],
			'>', \my $o, '2>', \$err[$w]);
		push @h, $h;
	}
	my $bh = IPC::Run::start(
		[ 'psql', '-X', '-q', '-d', $node->connstr('d'), '-f', $builder ],
		'>', \my $bo, '2>', \my $be);
	$_->finish for @h;
	$bh->finish;
	is(join('', @err), '', 'four writers, each attesting every row, with the builder running: no call failed');
	is($be // '', '', 'and the builder did not fail');
	is(db(q{SELECT count(*) FROM docs WHERE id > 1000}), 4 * $per, 'every row was written');
	is(db(q{SELECT count(*) FROM kingres.ledger_attestation}), 2 + 4 * $per,
		'with one attestation each (and the two of the first test)');
	db(q{SELECT kingres.ledger_build_block()});
	is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies');
	is(db(q{SELECT count(*) FROM kingres.ledger_pending}), '0', 'and nothing is left pending');
}

done_testing();
