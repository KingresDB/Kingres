# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/034_internal_tables.pl
#
# The three tables of client signatures (ledger_client_key,
# ledger_client_key_revocation, ledger_attestation) as ledger tables of the
# extension itself, registered by the install script under the hook (the first
# step of the extension work of part B, the "spike"), and what that means for
# the guard:
#
#   * a database that holds none of them in use can drop the extension, as
#     before; once one holds a row (a registered key) nothing of the ledger can
#     be dropped, directly or through a dependency, by any role;
#   * no role writes them but the code of the extension: not a member of
#     pg_write_all_data, not a member of the owner role with INHERIT, not the
#     owner of the extension, not a superuser without the setting, not the apply
#     worker and the table synchronisation worker of logical replication, not the
#     script of another extension; a superuser with the setting leaves the record
#     of a write by hand;
#   * the one statement that the hook lets through while the install script runs
#     (the arming of the capture trigger of one of these tables) is let through
#     only there and only as that text;
#   * the policy of a table is changed by ledger_set_signature_policy only.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('internal');
$node->init(allows_streaming => 'logical');
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my $super = $node->safe_psql('postgres', 'SELECT current_user');
my @tables = qw(ledger_client_key ledger_client_key_revocation ledger_attestation);

sub err_of
{
	my ($db, $sql, @extra) = @_;
	my ($ret, $out, $err) = $node->psql($db, $sql, extra_params => [ @extra ]);
	return $err;
}

$node->safe_psql('postgres', qq{
	CREATE ROLE inh LOGIN;
	GRANT "$super" TO inh WITH INHERIT TRUE, SET FALSE;
	CREATE ROLE wad LOGIN IN ROLE pg_write_all_data;
	CREATE ROLE maint LOGIN IN ROLE pg_maintain;
	CREATE ROLE plain LOGIN;
	CREATE DATABASE empty;
	CREATE DATABASE used;
	CREATE DATABASE ss;
});

#--------------------------------------------------------------------------
# A database that holds none of them in use
#--------------------------------------------------------------------------
$node->safe_psql('empty', 'CREATE EXTENSION kingres_ledger');
is($node->safe_psql('empty', q{SELECT count(*) FROM kingres.ledger_table}), '3',
	'the install script registered the three tables');
is($node->safe_psql('empty', q{SELECT count(*) FROM kingres.ledger_entry}), '0', 'and wrote no entry');
is($node->safe_psql('empty', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'a new database verifies');
$node->safe_psql('empty', 'DROP EXTENSION kingres_ledger');
is($node->safe_psql('empty', q{SELECT count(*) FROM pg_extension WHERE extname = 'kingres_ledger'}), '0',
	'an extension with no table under ledger control and no row in the three tables is dropped as before');
$node->safe_psql('empty', 'CREATE EXTENSION kingres_ledger');
is($node->safe_psql('empty', q{SELECT count(*) FROM kingres.ledger_table}), '3', 'and created again');
is(err_of('empty', 'DROP EXTENSION plpgsql CASCADE') =~ /ERROR/ ? 'refused' : 'ok', 'ok',
	'DROP EXTENSION plpgsql CASCADE is the ordinary statement there too');

#--------------------------------------------------------------------------
# One row, and nothing is dropped any more
#--------------------------------------------------------------------------
$node->safe_psql('used', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	SELECT kingres.ledger_client_key_register(decode(repeat('0a', 32), 'hex'), NULL, 'a key');
});
foreach my $stmt (
	'DROP EXTENSION kingres_ledger',
	'DROP EXTENSION kingres_ledger CASCADE',
	'DROP EXTENSION plpgsql CASCADE',
	'DROP SCHEMA kingres CASCADE',
	"DROP OWNED BY \"$super\"")
{
	like(err_of('used', $stmt), qr/ERROR:.*(ledger|belongs to|requires it)/,
		"with a registered key: $stmt is refused for the superuser");
	like(err_of('used', $stmt, '-U', 'inh'), qr/ERROR:/, "and for a member of the owner role: $stmt");
}
is($node->safe_psql('used', q{SELECT count(*) FROM pg_extension WHERE extname = 'kingres_ledger'}), '1',
	'the extension is still there');

#--------------------------------------------------------------------------
# The statements that stop a ledger table, for the roles that are not superusers
#--------------------------------------------------------------------------
foreach my $t (@tables)
{
	foreach my $r ('inh', 'wad', 'maint', 'plain')
	{
		foreach my $stmt (
			"DROP TABLE kingres.$t",
			"TRUNCATE kingres.$t",
			"ALTER TABLE kingres.$t ADD COLUMN x int",
			"ALTER TABLE kingres.$t DISABLE TRIGGER ALL",
			"ALTER TABLE kingres.$t RENAME TO x",
			"ALTER TABLE kingres.$t SET SCHEMA public",
			"CREATE INDEX ON kingres.$t ((1))",
			"CREATE TRIGGER x BEFORE INSERT ON kingres.$t FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify()",
			"DROP TRIGGER kingres_ledger_capture ON kingres.$t",
			"GRANT INSERT ON kingres.$t TO plain",
			"LOCK TABLE kingres.$t IN ACCESS EXCLUSIVE MODE",
			"INSERT INTO kingres.$t SELECT * FROM kingres.$t",
			"DELETE FROM kingres.$t",
			"UPDATE kingres.$t SET key_id = key_id",
			"CREATE TABLE public.fk_$t (k bytea REFERENCES kingres.$t)")
		{
			my $err = err_of('used', $stmt, '-U', $r);
			like($err, qr/ERROR:/, "$r: $stmt is refused");
		}
	}
	# a superuser without the setting
	foreach my $stmt (
		"DROP TABLE kingres.$t",
		"TRUNCATE kingres.$t",
		"ALTER TABLE kingres.$t ADD COLUMN x int",
		"ALTER TABLE kingres.$t DISABLE TRIGGER ALL",
		"INSERT INTO kingres.$t SELECT * FROM kingres.$t",
		"DELETE FROM kingres.$t")
	{
		like(err_of('used', $stmt), qr/ERROR:/, "the superuser: $stmt is refused");
	}
}
is($node->safe_psql('used', q{SELECT count(*) FROM kingres.ledger_verify()}), '0',
	'nothing of it left a trace: the ledger verifies');

# The privileges: SELECT may be granted, nothing else; PUBLIC has nothing.
foreach my $t (@tables)
{
	is($node->safe_psql('used', "SELECT has_table_privilege('public', 'kingres.$t', 'select')"), 'f',
		"$t is not readable by PUBLIC");
	$node->safe_psql('used', "GRANT SELECT ON kingres.$t TO plain");
	is($node->safe_psql('used', "SELECT count(*) >= 0 FROM kingres.$t", extra_params => [ '-U', 'plain' ]), 't',
		"and SELECT may be granted ($t)");
	like(err_of('used', "GRANT ALL ON kingres.$t TO plain"), qr/ERROR/, "ALL may not ($t)");
	like(err_of('used', "REVOKE SELECT ON kingres.$t FROM PUBLIC", '-U', 'inh'), qr/ERROR/, "a member of the owner cannot revoke from PUBLIC ($t)");
}

#--------------------------------------------------------------------------
# The one statement of the script
#--------------------------------------------------------------------------
{
	my $stmt = q{CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON kingres.ledger_attestation FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture('3')};
	like(err_of('used', $stmt), qr/cannot create a trigger on table "ledger_attestation": it is part of the ledger/,
		'the text that ledger_register_internal runs is refused by the hook when nobody runs it (and it is the hook that refuses it)');
	like(err_of('used', q{SELECT kingres.ledger_register_internal('kingres.ledger_attestation'::regclass)}),
		qr/only the install or upgrade script/, 'ledger_register_internal refuses a superuser outside the script');
	like(err_of('used', q{GRANT EXECUTE ON FUNCTION kingres.ledger_register_internal(regclass) TO plain}),
		qr/ERROR:.*internal function/s, 'and nobody is granted EXECUTE on it');
}

#--------------------------------------------------------------------------
# A write by hand, with the setting
#--------------------------------------------------------------------------
{
	$node->safe_psql('used', q{
		SET kingres_ledger.allow_manual_write = on;
		INSERT INTO kingres.ledger_client_key_revocation VALUES (decode(repeat('0a', 16), 'hex'), 'by hand', 'me', now());
		INSERT INTO kingres.ledger_attestation VALUES (7, 7, decode(repeat('0a', 16), 'hex'), decode(repeat('0b', 16), 'hex'), decode(repeat('0c', 64), 'hex'));
	});
	is($node->safe_psql('used', q{SELECT string_agg(source_table, ',' ORDER BY source_table) FROM kingres.ledger_removed WHERE action = 'inserted'}),
		'ledger_attestation,ledger_client_key_revocation',
		'a superuser who turned the setting on can write the tables, and each write leaves its record');
	is($node->safe_psql('used', q{SELECT string_agg(check_name || ' ' || object, ',' ORDER BY object) FROM kingres.ledger_verify()
		WHERE check_name = 'manual_write'}),
		'manual_write kingres.ledger_attestation,manual_write kingres.ledger_client_key_revocation',
		'and ledger_verify reports them');
	like(err_of('used', q{SET kingres_ledger.allow_manual_write = on; UPDATE kingres.ledger_table SET signature_policy = 'required' WHERE table_id = 3}),
		qr/ERROR:.*append-only/s, 'the policy cannot be changed by an UPDATE, with the setting on either');
	like(err_of('used', q{SET kingres_ledger.allow_manual_write = on; UPDATE kingres.ledger_table SET signature_required_from = 5 WHERE table_id = 3}),
		qr/ERROR:.*append-only/s, 'nor the date of the switch');
}

#--------------------------------------------------------------------------
# Another extension's script, and logical replication
#--------------------------------------------------------------------------
{
	my $extdir = PostgreSQL::Test::Utils::tempdir;
	mkdir "$extdir/extension";
	append_to_file("$extdir/extension/kl_evil2.control",
		"comment = 'a second extension'\ndefault_version = '1.0'\nrelocatable = true\n");
	append_to_file("$extdir/extension/kl_evil2--1.0.sql",
		"INSERT INTO kingres.ledger_client_key_revocation VALUES (decode(repeat('0d', 16), 'hex'), 'evil', 'evil', now());\n");
	$node->append_conf('postgresql.conf', "extension_control_path = '$extdir:\$system'\n");
	$node->reload;
	$node->safe_psql('ss', 'CREATE EXTENSION kingres_ledger');
	like(err_of('ss', 'CREATE EXTENSION kl_evil2'), qr/cannot write to table "ledger_client_key_revocation"/,
		'the script of another extension cannot write the tables');
}

{
	my $pub = $node;
	my $sub = PostgreSQL::Test::Cluster->new('internal_sub');
	$sub->init;
	$sub->append_conf('postgresql.conf',
		"shared_preload_libraries = 'kingres_ledger'\nkingres_ledger.block_interval = 0\n");
	$sub->start;
	$sub->safe_psql('postgres', 'CREATE DATABASE rs');
	$sub->safe_psql('rs', 'CREATE EXTENSION kingres_ledger');
	my $pubconn = $pub->connstr('used');
	$pubconn =~ s/'/''/g;
	my $before = $sub->safe_psql('rs', q{SELECT (SELECT count(*) FROM kingres.ledger_client_key) || ' '
		|| (SELECT count(*) FROM kingres.ledger_attestation) || ' ' || (SELECT count(*) FROM kingres.ledger_client_key_revocation)});
	$pub->safe_psql('used', q{CREATE PUBLICATION p_keys FOR TABLE kingres.ledger_client_key, kingres.ledger_attestation});
	my $off = -s $sub->logfile;
	$sub->safe_psql('rs', "CREATE SUBSCRIPTION s_keys CONNECTION '$pubconn' PUBLICATION p_keys");
	ok($sub->wait_for_log(qr/cannot write to table "ledger_(client_key|attestation)"/, $off),
		'the table synchronisation worker is refused by the guard trigger');
	is($sub->safe_psql('rs', q{SELECT (SELECT count(*) FROM kingres.ledger_client_key) || ' '
		|| (SELECT count(*) FROM kingres.ledger_attestation) || ' ' || (SELECT count(*) FROM kingres.ledger_client_key_revocation)}),
		$before, 'and nothing arrived');
	$sub->safe_psql('rs', 'ALTER SUBSCRIPTION s_keys DISABLE');
	$sub->safe_psql('rs', 'ALTER SUBSCRIPTION s_keys SET (slot_name = NONE)');
	$sub->safe_psql('rs', 'DROP SUBSCRIPTION s_keys');

	$pub->safe_psql('used', q{CREATE PUBLICATION p_rev FOR TABLE kingres.ledger_client_key_revocation});
	$off = -s $sub->logfile;
	$sub->safe_psql('rs', "CREATE SUBSCRIPTION s_rev CONNECTION '$pubconn' PUBLICATION p_rev WITH (copy_data = false)");
	$pub->safe_psql('used', q{SET kingres_ledger.allow_manual_write = on;
		INSERT INTO kingres.ledger_client_key_revocation VALUES (decode(repeat('0e', 16), 'hex'), 'for the apply worker', 'me', now())});
	ok($sub->wait_for_log(qr/cannot write to table "ledger_client_key_revocation"/, $off),
		'the apply worker is refused as well');
	$sub->safe_psql('rs', 'ALTER SUBSCRIPTION s_rev DISABLE');
	$sub->safe_psql('rs', 'ALTER SUBSCRIPTION s_rev SET (slot_name = NONE)');
	$sub->safe_psql('rs', 'DROP SUBSCRIPTION s_rev');
	$sub->stop;
}

done_testing();
