# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/037_attest_savepoints.pl
#
# L21-05 of the review of client signatures: the entries that a table with the
# policy "required" owes an attestation follow the subtransaction stack
# (src/capture.c, kl_subxact_callback), and the tests of t/032 covered one level
# at a time.  A reviewer's one-line mutant (the attestation of a subtransaction
# that commits is not passed to its parent) survived them.  This file runs every
# placement of one entry and its attestation over two nested subtransactions
# and every way of closing them, as savepoints and as EXCEPTION blocks of a
# PL/pgSQL function, and compares the outcome of COMMIT with a model that is
# written here and not read from the server:
#
#   slots, in the order of the program:
#     0  the top level, before the outer savepoint s
#     1  inside s, before the inner savepoint t
#     2  inside t
#        t is closed: RELEASE (or, for an EXCEPTION block, normal completion) or
#        ROLLBACK TO (an error that the block catches)
#     3  inside s, after t
#        s is closed in the same two ways
#     4  the top level, after s
#
#   The entry is written in slot e and attested in slot a >= e (or never); the
#   attestation is a row of the subtransaction it was made in, and the entry a
#   row of the one that wrote it, so each is there at COMMIT unless t or s, at
#   or below which it was made, was rolled back.  An entry that is there and
#   whose attestation is not is owed one: COMMIT fails with KL010.  An entry that
#   is not there is owed nothing.  Anything else commits.
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

my $node = PostgreSQL::Test::Cluster->new('attest_sp');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my $dir = PostgreSQL::Test::Utils::tempdir;
my $k1 = new_key($dir, 'k1');

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT EXECUTE ON FUNCTION kingres.ledger_attest(regclass, bytea, "char", bytea, bytea, bytea) TO app;
	CREATE TABLE sp (id int PRIMARY KEY, body text);
	ALTER TABLE sp OWNER TO app;
	SELECT kingres.ledger_enable('sp');
	SELECT kingres.ledger_set_signature_policy('sp', 'required');
});

sub db { return $node->safe_psql('d', @_); }

my $chain = unhex_of(db(q{SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain}));
my $table_id = db(q{SELECT table_id FROM kingres.ledger_table WHERE relid = 'sp'::regclass});
db(sprintf(q{SELECT kingres.ledger_client_key_register(decode('%s', 'hex'), NULL, 'k1')}, hex_of($k1->{pub})));

my $app;
{
	local $ENV{PGUSER} = 'app';
	$app = $node->background_psql('d', on_error_stop => 0);
	$app->query_safe(q{\set VERBOSITY sqlstate});
}

sub failure_of
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	$s->query($sql);
	my ($state) = $s->{stderr} =~ /ERROR:\s+(\S+)/;
	$s->{stderr} = '';
	return $state // '';
}

sub attest_stmt
{
	my ($id) = @_;
	my $body = "v$id";
	my $hash = db(sprintf(q{SELECT encode(kingres.ledger_row_hash(ROW(%d, '%s')::sp), 'hex')}, $id, $body));
	my $nonce = random_nonce();
	my $msg = message_bytes(chain_id => $chain, table_id => $table_id, op => 'I',
		row_hash => unhex_of($hash), key_id => $k1->{key_id}, nonce => $nonce);
	my $sig = sign_message($k1, $msg);
	return sprintf(q{kingres.ledger_attest('sp'::regclass, decode('%s', 'hex'), 'I', decode('%s', 'hex'), decode('%s', 'hex'), decode('%s', 'hex'))},
		$hash, hex_of($k1->{key_id}), hex_of($nonce), hex_of($sig));
}

# The model.  The chain of a slot is the list of the subtransactions that a row made there belongs to.
my %chain = (0 => [], 1 => ['s'], 2 => ['s', 't'], 3 => ['s'], 4 => []);

# Is a row made in slot $k still there at $when (a slot number; 5 is COMMIT), given how t and s were closed?
sub there
{
	my ($k, $when, $close_t, $close_s) = @_;
	foreach my $sub (@{ $chain{$k} })
	{
		return 0 if $sub eq 't' && $close_t eq 'rollback' && $when >= 3;
		return 0 if $sub eq 's' && $close_s eq 'rollback' && $when >= 4;
	}
	return 1;
}

my $next_id = 100;
my $checked = 0;

foreach my $mode ('savepoint', 'exception')
{
	foreach my $close_t ('release', 'rollback')
	{
		foreach my $close_s ('release', 'rollback')
		{
			foreach my $e (0 .. 4)
			{
				foreach my $a ('none', $e .. 4)
				{
					# the attestation needs the entry to be there when it is made
					next if $a ne 'none' && !there($e, $a, $close_t, $close_s);

					my $id = ++$next_id;
					my %ops;		# slot => list of statements
					my $ins = sprintf(q{INSERT INTO sp VALUES (%d, 'v%d')}, $id, $id);
					push @{ $ops{$e} }, $ins;
					my $att;
					if ($a ne 'none')
					{
						$att = attest_stmt($id);
						push @{ $ops{$a} }, $att;
					}
					my $label = "$mode: entry in slot $e, attestation " . ($a eq 'none' ? 'never' : "in slot $a")
					  . ", t $close_t, s $close_s";

					my $entry_there = there($e, 5, $close_t, $close_s);
					my $att_there = $a ne 'none' && there($a, 5, $close_t, $close_s);
					my $owed = $entry_there && !$att_there;

					my $got;
					if ($mode eq 'savepoint')
					{
						my @prog = ('BEGIN');
						my $slot = sub { push @prog, @{ $ops{ $_[0] } // [] } };
						my $stmt = sub { my ($s) = @_; $s =~ s/^kingres/SELECT kingres/; return $s; };
						@{ $ops{$_} } = map { $_ =~ /^INSERT/ ? $_ : $stmt->($_) } @{ $ops{$_} // [] } for (0 .. 4);
						$slot->(0);
						push @prog, 'SAVEPOINT s';
						$slot->(1);
						push @prog, 'SAVEPOINT t';
						$slot->(2);
						push @prog, $close_t eq 'release' ? 'RELEASE t' : ('ROLLBACK TO t', 'RELEASE t');
						$slot->(3);
						push @prog, $close_s eq 'release' ? 'RELEASE s' : ('ROLLBACK TO s', 'RELEASE s');
						$slot->(4);
						$app->query_safe($_) for @prog;
						$got = failure_of($app, 'COMMIT');
					}
					else
					{
						my $stmt = sub { my ($s) = @_; return $s =~ /^INSERT/ ? "$s;" : "PERFORM $s;"; };
						my $slot = sub { return join(' ', map { $stmt->($_) } @{ $ops{ $_[0] } // [] }); };
						my $raise_t = $close_t eq 'rollback' ? q{RAISE EXCEPTION 'undo t';} : '';
						my $raise_s = $close_s eq 'rollback' ? q{RAISE EXCEPTION 'undo s';} : '';
						my $body = $slot->(0)
						  . ' BEGIN ' . $slot->(1)
						  . ' BEGIN ' . $slot->(2) . " $raise_t EXCEPTION WHEN OTHERS THEN NULL; END; "
						  . $slot->(3) . " $raise_s EXCEPTION WHEN OTHERS THEN NULL; END; "
						  . $slot->(4);
						$got = failure_of($app, "DO \$do\$ BEGIN $body END \$do\$");
					}
					is($got, $owed ? 'KL010' : '', "$label: " . ($owed ? 'KL010' : 'commits'));
					$checked++;
					my $rows = db("SELECT count(*) FROM sp WHERE id = $id");
					is($rows, ($owed || !$entry_there) ? '0' : '1', "$label: the row is " . (($owed || !$entry_there) ? 'not there' : 'there'));
				}
			}
		}
	}
}
cmp_ok($checked, '>', 100, "the matrix has $checked sequences");

#--------------------------------------------------------------------------
# The sequence of the reviewer's mutant, by name
#--------------------------------------------------------------------------
{
	my $id = ++$next_id;
	$app->query_safe('BEGIN');
	$app->query_safe(sprintf(q{INSERT INTO sp VALUES (%d, 'v%d')}, $id, $id));
	$app->query_safe('SAVEPOINT s');
	$app->query_safe('SAVEPOINT t');
	$app->query_safe('SELECT ' . attest_stmt($id));
	$app->query_safe('RELEASE t');
	$app->query_safe('ROLLBACK TO s');
	is(failure_of($app, 'COMMIT'), 'KL010',
		'the attestation made in t, which was released into s, which was rolled back, is gone: the entry is owed it again');
}

$app->quit;
db('SELECT kingres.ledger_build_block()');
is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies after the whole matrix');

done_testing();
