# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/035_no_openssl.pl
#
# A server without OpenSSL: client signatures need it to verify an Ed25519
# signature, and everything else of the extension must keep working.  The test
# builds the extension a second time with -DKL_NO_OPENSSL (src/sign.c then
# compiles as it does against a server built without OpenSSL), with PGXS and the
# make of the build tree, into a directory of its own, and loads that library
# in place of the real one (the control file of the copy names it by its path).
# It checks that ledger_attest raises 0A000 and that the registry, the policy
# and everything else work.  The test is skipped, and says why, when PGXS or
# make is not there.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use FindBin;
use File::Basename;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $pg_config = $ENV{PG_CONFIG} // 'pg_config';
my $pgxs = `$pg_config --pgxs 2>/dev/null`;
chomp $pgxs;
plan skip_all => 'PGXS (pg_config --pgxs) is not available in this installation'
  unless $pgxs ne '' && -f $pgxs;
plan skip_all => 'make is not available'
  unless system('make --version >/dev/null 2>&1') == 0;

my $src = dirname($FindBin::RealBin);		# contrib/kingres_ledger
my $work = PostgreSQL::Test::Utils::tempdir;
my $copy = "$work/ext";
# Only the sources: the directory of the test may be a build directory (PGXS) that holds
# objects, results and the data of the test itself.
mkdir $copy or die "cannot make $copy: $!";
system('cp', '-R', "$src/src", "$copy/src") == 0 or die "cannot copy the sources";
system("find '$copy' -name '*.o' -delete");
system("cp '$src'/Makefile '$src'/kingres_ledger.control '$src'/kingres_ledger--*.sql '$copy'/") == 0
  or die "cannot copy the Makefile and the scripts";

my $log = "$work/build.log";
my $rc = system("make -C '$copy' USE_PGXS=1 PG_CONFIG='$pg_config' "
	  . "PG_CPPFLAGS='-I$copy/src -DKL_NO_OPENSSL' kingres_ledger.so >'$log' 2>&1");
ok($rc == 0, 'the extension builds as for a server without OpenSSL (-DKL_NO_OPENSSL)')
  or diag(`tail -20 '$log'`);
ok(-f "$copy/kingres_ledger.so" || -f "$copy/kingres_ledger.dylib", 'and gives a library');
my $so = "$copy/kingres_ledger.so";

# The extension of the copy: its control file names the library by path.
mkdir "$work/extension";
my $control = slurp_file("$copy/kingres_ledger.control");
$control =~ s{module_pathname = '[^']*'}{module_pathname = '$work/kingres_ledger'};
append_to_file("$work/extension/kingres_ledger.control", $control);
system("cp '$copy'/kingres_ledger--*.sql '$work/extension/'") == 0 or die;
system("cp '$so' '$work/kingres_ledger.so'") == 0 or die;

$ENV{PGOPTIONS} = '-c search_path=public';
my $node = PostgreSQL::Test::Cluster->new('nossl');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = '$work/kingres_ledger'\n"
	  . "extension_control_path = '$work:\$system'\n"
	  . "dynamic_library_path = '$work:\$libdir'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;
$node->safe_psql('postgres', q{CREATE DATABASE d});
$node->safe_psql('d', q{CREATE EXTENSION kingres_ledger; GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');});
is($node->safe_psql('d', q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'}), '0.3',
	'the extension of the copy is loaded');

my $key = $node->safe_psql('d', q{SELECT encode(kingres.ledger_client_key_register(decode(repeat('0a', 32), 'hex'), NULL, 'no ssl'), 'hex')});
is(length($key), 32, 'a key can be registered without OpenSSL (the registry is data)');
$node->safe_psql('d', q{SELECT kingres.ledger_client_key_revoke(decode('} . $key . q{', 'hex'), 'test')});
is($node->safe_psql('d', q{SELECT length(kingres.ledger_attestation_message(decode(repeat('00', 32), 'hex'), 4, 'I',
	decode(repeat('01', 32), 'hex'), decode(repeat('02', 16), 'hex'), decode(repeat('03', 16), 'hex')))}),
	'136', 'the message can be computed');

my ($ret, $out, $err) = $node->psql('d', q{
	BEGIN;
	INSERT INTO docs VALUES (1, 'one');
	SELECT kingres.ledger_attest('docs', kingres.ledger_row_hash(d.*), 'I', decode(repeat('04', 16), 'hex'),
		decode(repeat('05', 16), 'hex'), decode(repeat('06', 64), 'hex')) FROM docs d;
	ROLLBACK;
}, extra_params => [ '-v', 'VERBOSITY=verbose' ]);
like($err, qr/ERROR:\s+0A000:.*client signatures need a server built with OpenSSL/s,
	'ledger_attest raises 0A000: "client signatures need a server built with OpenSSL"');

# The policy works without OpenSSL, and then no write to that table can be attested.
$node->safe_psql('d', q{SELECT kingres.ledger_set_signature_policy('docs', 'required')});
($ret, $out, $err) = $node->psql('d', q{INSERT INTO docs VALUES (2, 'two')}, extra_params => [ '-v', 'VERBOSITY=sqlstate' ]);
like($err, qr/KL010/, 'a table with the policy required cannot be written there: KL010 (nothing can attest)');
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies');
$node->safe_psql('d', q{SELECT kingres.ledger_build_block()});
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'and after a block');

done_testing();
