# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/017_guard_paths.pl
#
# The invariant of the guard and the paths to it (L10-01, L10-02, L10-03,
# L10-13):
#
#   no statement that a role can issue writes a ledger table, whatever the role
#   inherits or owns, except (a) through the public functions of the extension
#   doing what they are documented to do, and (b) a superuser acting as itself
#   with kingres_ledger.allow_manual_write set in the session.
#
# This file covers what a regression test cannot: an owner that lost SUPERUSER,
# every way a setting can arrive in a session (a connection option, the
# configuration file, ALTER ROLE, ALTER DATABASE, ALTER SYSTEM, a SET clause of a
# function, SET LOCAL, set_config), the checks of the guard trigger that are the
# only check where the executor hook is not called (the logical replication
# table synchronisation and apply workers, with a real subscription), and the
# block builder against locks that other sessions hold on the ledger tables.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

# The superuser is named like the schema of the extension, which would then be
# first in its search_path: tables go into public, as in the other tests.
$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('guardpaths');
$node->init(allows_streaming => 'logical');
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "max_prepared_transactions = 5\n");
$node->start;

my $super = $node->safe_psql('postgres', 'SELECT current_user');

# The error text of a statement, or '' when it ran.
sub err_of
{
	my ($db, $sql, @extra) = @_;
	my ($ret, $out, $err) = $node->psql($db, $sql, @extra);
	return $err;
}

my $blocks_sql = q{INSERT INTO kingres.ledger_blocks (height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at, block_hash)
	VALUES (900, 1, sha256('a'), sha256('b'), 1, now(), sha256('c'))};

sub state_of
{
	my ($n, $db) = @_;
	return $n->safe_psql($db, q{SELECT (SELECT count(*) FROM kingres.ledger_blocks) || ' '
	    || (SELECT count(*) FROM kingres.ledger_entry) || ' '
	    || (SELECT count(*) FROM kingres.ledger_tx) || ' '
	    || (SELECT count(*) FROM kingres.ledger_pending) || ' '
	    || (SELECT count(*) FROM kingres.ledger_digest)});
}

#--------------------------------------------------------------------------
# An owner of the extension that lost SUPERUSER (L10-01)
#--------------------------------------------------------------------------
$node->safe_psql('postgres', 'CREATE ROLE own2 SUPERUSER LOGIN');
$node->safe_psql('postgres', 'CREATE DATABASE dd OWNER own2');
$node->safe_psql('dd', q{
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a'), (2, 'b');
	SELECT kingres.ledger_build_block();
}, extra_params => [ '-U', 'own2' ]);
$node->safe_psql('postgres', 'ALTER ROLE own2 NOSUPERUSER');
is($node->safe_psql('dd', q{SELECT rolsuper FROM pg_roles WHERE rolname = 'own2'}), 'f',
	'the owner of the extension is no longer a superuser');
my $before = state_of($node, 'dd');
my @own2 = (extra_params => [ '-U', 'own2' ]);
foreach my $t (qw(ledger_table ledger_chain ledger_entry ledger_pending ledger_tx ledger_blocks
		ledger_digest ledger_verification ledger_removed))
{
	like(err_of('dd', "INSERT INTO kingres.$t DEFAULT VALUES", @own2),
		qr/cannot write to table "$t"/, "the demoted owner cannot insert into $t");
}
like(err_of('dd', $blocks_sql, @own2), qr/cannot write to table "ledger_blocks"/,
	'nor a block with values of its choice');
like(err_of('dd', 'SET kingres_ledger.allow_manual_write = on; ' . $blocks_sql, @own2),
	qr/permission denied to set parameter|cannot write to table/,
	'and the setting does not help: it is not a superuser');
like(err_of('dd', q{SELECT kingres.ledger_history_sync('docs'::regclass, 'pg_class'::regclass)}, @own2),
	qr/only the extension owner, as a superuser, can call this function/,
	'it cannot call the internal function that changes the columns of a table');
like(err_of('dd', q{SELECT kingres.ledger_remove_stray('x')}, @own2),
	qr/only a superuser can remove rows/, 'nor remove rows of the ledger');
is(state_of($node, 'dd'), $before, 'nothing was written');
# What it can do is what the public functions do.
$node->safe_psql('dd', q{INSERT INTO docs VALUES (3, 'c'); SELECT kingres.ledger_build_block()}, @own2);
$node->safe_psql('dd', q{SELECT kingres.ledger_generate_digest(); SELECT kingres.ledger_verify_recorded()}, @own2);
is($node->safe_psql('dd', 'SELECT count(*) FROM kingres.ledger_verify()'), '0',
	'a demoted owner builds blocks, digests and records through the public functions, and the ledger verifies');

#--------------------------------------------------------------------------
# Every way a setting can arrive in a session (L10-13)
#--------------------------------------------------------------------------
$node->safe_psql('postgres', 'CREATE DATABASE ss');
$node->safe_psql('ss', q{
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a');
	SELECT kingres.ledger_build_block();
});
my $ss_before = state_of($node, 'ss');
my $off = 'cannot write to table "ledger_blocks"';

# A connection option (PGOPTIONS), as a superuser.
{
	local $ENV{PGOPTIONS} = '-c kingres_ledger.allow_manual_write=on';
	my ($ret, $out, $err) = $node->psql('ss', "SHOW kingres_ledger.allow_manual_write; $blocks_sql");
	like($err, qr/can be turned on only with SET in a session|\Q$off\E/,
		'the setting as a connection option (PGOPTIONS) does not open the tables');
	unlike($out, qr/^on$/m, 'and the session did not get the value on');
}
is(state_of($node, 'ss'), $ss_before, 'and nothing was written through it');

# ALTER ROLE, ALTER DATABASE, ALTER SYSTEM.
foreach my $stmt ("ALTER ROLE $super SET kingres_ledger.allow_manual_write = on",
	"ALTER ROLE ALL SET kingres_ledger.allow_manual_write = on",
	"ALTER DATABASE ss SET kingres_ledger.allow_manual_write = on",
	"ALTER SYSTEM SET kingres_ledger.allow_manual_write = on")
{
	like(err_of('ss', $stmt), qr/can be turned on only with SET in a session/, "refused: $stmt");
}
is($node->safe_psql('ss', q{SELECT count(*) FROM pg_db_role_setting WHERE setconfig::text LIKE '%allow_manual_write%'}),
	'0', 'and no setting was stored');

# A function with a SET clause.
like(err_of('ss', q{CREATE FUNCTION public.f_set() RETURNS void LANGUAGE sql
	SET kingres_ledger.allow_manual_write = on AS 'SELECT 1'}),
	qr/can be turned on only with SET in a session/, 'a function with a SET clause naming the setting is refused');

# set_config and SET LOCAL: they are the superuser's own act, in the session,
# and last as long as they were asked to.
is($node->safe_psql('ss', q{SELECT set_config('kingres_ledger.allow_manual_write', 'on', true)}), 'on',
	'set_config(..., true) by a superuser sets the value for the transaction');
is($node->safe_psql('ss', q{BEGIN; SELECT set_config('kingres_ledger.allow_manual_write', 'on', true); SHOW kingres_ledger.allow_manual_write; COMMIT}),
	"on\non", 'inside one transaction');
is($node->safe_psql('ss', q{SHOW kingres_ledger.allow_manual_write}), 'off', 'and it is off in the next session');
like(err_of('ss', "BEGIN; SET LOCAL kingres_ledger.allow_manual_write = on; COMMIT; $blocks_sql"),
	qr/\Q$off\E/, 'SET LOCAL ends with its transaction: the next statement is refused');
like(err_of('ss', "SET kingres_ledger.allow_manual_write = on; RESET kingres_ledger.allow_manual_write; $blocks_sql"),
	qr/\Q$off\E/, 'RESET turns it off');
is(state_of($node, 'ss'), $ss_before, 'nothing was written by any of them');

# The configuration file: the value is refused at start, and the server runs
# with the setting off.
$node->append_conf('postgresql.conf', "kingres_ledger.allow_manual_write = on\n");
$node->restart;
is($node->safe_psql('ss', q{SHOW kingres_ledger.allow_manual_write}), 'off',
	'the setting in postgresql.conf is not taken');
like(err_of('ss', $blocks_sql), qr/\Q$off\E/, 'and a superuser is refused');
$node->adjust_conf('postgresql.conf', 'kingres_ledger.allow_manual_write', undef);
$node->restart;

# The record that a manual write leaves (L10-02).
$node->safe_psql('ss', "SET kingres_ledger.allow_manual_write = on; " .
	q{INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
	  SELECT now(), 'hand', height, block_hash, '{}' FROM kingres.ledger_blocks ORDER BY height DESC LIMIT 1});
is($node->safe_psql('ss', q{SELECT source_table || ' ' || action FROM kingres.ledger_removed WHERE action = 'inserted'}),
	'ledger_digest inserted', 'a write by hand leaves a record in ledger_removed');
like($node->safe_psql('ss', q{SELECT check_name || ' ' || object FROM kingres.ledger_verify() WHERE check_name = 'manual_write'}),
	qr/^manual_write kingres\.ledger_digest$/, 'which ledger_verify reports');

#--------------------------------------------------------------------------
# The block builder against locks of other sessions (L10-03)
#--------------------------------------------------------------------------
# A session that holds a lock on a table of the ledger for a long time: a
# superuser may take them (and can stop the server anyway); what the builder
# needs from the tables is ROW EXCLUSIVE, and no lock of the table ledger_chain.
$node->safe_psql('postgres', 'CREATE DATABASE lk');
$node->safe_psql('lk', q{
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a');
});
$node->safe_psql('lk', 'CREATE SEQUENCE docs_seq START 100');
my $honest = q{SET lock_timeout = '3s';
	INSERT INTO docs VALUES (nextval('docs_seq'), 'x');
	SELECT kingres.ledger_build_block() IS NOT NULL};
foreach my $mode ('ROW SHARE', 'ROW EXCLUSIVE', 'SHARE UPDATE EXCLUSIVE', 'SHARE ROW EXCLUSIVE')
{
	my $bg = $node->background_psql('lk', on_error_stop => 0);
	$bg->query_safe("BEGIN; LOCK TABLE kingres.ledger_chain IN $mode MODE;");
	my ($ret, $out, $err) = $node->psql('lk', $honest);
	$bg->query_safe('ROLLBACK;');
	$bg->quit;
	like($out, qr/^t$/m, "the block builder builds while another session holds $mode on ledger_chain");
	unlike($err, qr/lock timeout/, 'and did not wait for it');
}
# What a role that is not the extension could take is refused (the regression
# test ledger_paths runs every mode on every table).  ACCESS SHARE, which any
# reader may take, conflicts with nothing that the capture triggers and the
# builder take.
{
	my $bg = $node->background_psql('lk', on_error_stop => 0);
	$bg->query_safe('BEGIN; LOCK TABLE kingres.ledger_pending IN ACCESS SHARE MODE;');
	my ($ret, $out, $err) = $node->psql('lk', $honest);
	$bg->query_safe('ROLLBACK;');
	$bg->quit;
	like($out, qr/^t$/m, 'ACCESS SHARE on the queue stops nobody');
}
# Advisory locks: the builder takes none (it waits on a lock on the
# extension's own object, which no SQL statement can request), so holding the
# advisory locks that a role can name does not stop it.
{
	my $bg = $node->background_psql('lk', on_error_stop => 0);
	$bg->query_safe(q{SELECT pg_advisory_lock(g) FROM generate_series(0, 300) g});
	$bg->query_safe(q{SELECT pg_advisory_lock(1, g) FROM generate_series(0, 300) g});
	my ($ret, $out, $err) = $node->psql('lk', $honest);
	$bg->quit;
	like($out, qr/^t$/m, 'the block builder is not stopped by advisory locks');
}

#--------------------------------------------------------------------------
# The checks of the guard trigger, where the executor hook is not called:
# logical replication, with a real subscription (L10-13)
#--------------------------------------------------------------------------
my $pub = PostgreSQL::Test::Cluster->new('guardpub');
$pub->init(allows_streaming => 'logical');
$pub->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\nkingres_ledger.block_interval = 0\n");
$pub->start;
my $sub = $node;				# the subscriber is the first node
my $setup = q{
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a'), (2, 'b');
	SELECT kingres.ledger_build_block();
	SELECT kingres.ledger_generate_digest();
	INSERT INTO docs VALUES (3, 'c');
};
$pub->safe_psql('postgres', $setup);
$sub->safe_psql('postgres', 'CREATE DATABASE rs');
$sub->safe_psql('rs', $setup);
my $pubconn = $pub->connstr('postgres');
$pubconn =~ s/'/''/g;

# 1. Initial table synchronisation: the table sync worker copies the rows of
# the publisher's ledger_blocks and ledger_digest into the subscriber's.
$pub->safe_psql('postgres',
	q{CREATE PUBLICATION p_ins FOR TABLE kingres.ledger_blocks, kingres.ledger_digest});
my $rs_before = state_of($sub, 'rs');
my $log_offset = -s $sub->logfile;
$sub->safe_psql('rs',
	"CREATE SUBSCRIPTION s_ins CONNECTION '$pubconn' PUBLICATION p_ins");
ok($sub->wait_for_log(qr/cannot write to table "ledger_blocks"/, $log_offset),
	'the table synchronisation worker is refused by the guard trigger (ledger_blocks)');
ok($sub->wait_for_log(qr/cannot write to table "ledger_digest"/, $log_offset),
	'and for ledger_digest');
is(state_of($sub, 'rs'), $rs_before, 'and the ledger tables of the subscriber are untouched');
$sub->safe_psql('rs', 'ALTER SUBSCRIPTION s_ins DISABLE');
$sub->safe_psql('rs', 'ALTER SUBSCRIPTION s_ins SET (slot_name = NONE)');
$sub->safe_psql('rs', 'DROP SUBSCRIPTION s_ins');

# 2. The apply worker: a row of the queue that the publisher writes after the
# subscription exists (copy_data = false) arrives as an insert.
$pub->safe_psql('postgres', q{CREATE PUBLICATION p_apply FOR TABLE kingres.ledger_pending});
$log_offset = -s $sub->logfile;
$sub->safe_psql('rs',
	"CREATE SUBSCRIPTION s_apply CONNECTION '$pubconn' PUBLICATION p_apply WITH (copy_data = false)");
$pub->safe_psql('postgres', q{INSERT INTO docs VALUES (4, 'd')});
ok($sub->wait_for_log(qr/cannot write to table "ledger_pending"/, $log_offset),
	'the apply worker is refused when it inserts a row of the queue');
is(state_of($sub, 'rs'), $rs_before, 'and nothing arrived');
$sub->safe_psql('rs', 'ALTER SUBSCRIPTION s_apply DISABLE');
$sub->safe_psql('rs', 'ALTER SUBSCRIPTION s_apply SET (slot_name = NONE)');
$sub->safe_psql('rs', 'DROP SUBSCRIPTION s_apply');

# 3. The apply worker, deletes: the same rows exist on both sides (written by
# hand, by a superuser who turned the setting on, on each), and the publisher
# deletes its copy by hand.  The delete arrives on the subscriber, whose
# triggers refuse it: the worker has no setting.
my $hand = q{SET kingres_ledger.allow_manual_write = on;
	INSERT INTO kingres.ledger_pending (ledger_txid, user_name) VALUES (777001, 'x');
	INSERT INTO kingres.ledger_tx (ledger_txid, block_height, tx_index, entry_count, merkle_root, user_name)
	VALUES (777002, 99, 0, 1, sha256('x'), 'x');
	INSERT INTO kingres.ledger_digest (digest_id, generated_at, generated_by, height, block_hash, digest)
	OVERRIDING SYSTEM VALUE VALUES (777003, now(), 'x', 0, sha256('x'), '{}');};
$pub->safe_psql('postgres', $hand);
$sub->safe_psql('rs', $hand);
my $rs_deletes = state_of($sub, 'rs');
my %key = (ledger_pending => 'ledger_txid = 777001', ledger_tx => 'ledger_txid = 777002',
	ledger_digest => 'digest_id = 777003');
my %refusal = (ledger_pending => qr/cannot write to table "ledger_pending"/,
	ledger_tx => qr/table "ledger_tx" is an append-only ledger table/,
	ledger_digest => qr/table "ledger_digest" is an append-only ledger table/);
foreach my $t (qw(ledger_pending ledger_tx ledger_digest))
{
	$pub->safe_psql('postgres',
		"CREATE PUBLICATION p_del_$t FOR TABLE kingres.$t WITH (publish = 'delete')");
	$log_offset = -s $sub->logfile;
	$sub->safe_psql('rs',
		"CREATE SUBSCRIPTION s_del_$t CONNECTION '$pubconn' PUBLICATION p_del_$t WITH (copy_data = false)");
	$pub->safe_psql('postgres',
		"SET kingres_ledger.allow_manual_write = on; DELETE FROM kingres.$t WHERE $key{$t}");
	ok($sub->wait_for_log($refusal{$t}, $log_offset),
		"the apply worker is refused when it deletes a row of $t (the setting is not its)");
	is(state_of($sub, 'rs'), $rs_deletes, "and the row of $t is still there");
	$sub->safe_psql('rs', "ALTER SUBSCRIPTION s_del_$t DISABLE");
	$sub->safe_psql('rs', "ALTER SUBSCRIPTION s_del_$t SET (slot_name = NONE)");
	$sub->safe_psql('rs', "DROP SUBSCRIPTION s_del_$t");
}

#--------------------------------------------------------------------------
# Another extension, and an extension that was dropped and created again (L10-13)
#--------------------------------------------------------------------------
# The script of the extension itself may write the identity of the chain; the
# script of any other extension may not.
my $extdir = PostgreSQL::Test::Utils::tempdir;
mkdir "$extdir/extension";
append_to_file("$extdir/extension/kl_evil.control",
	"comment = 'a second extension'\ndefault_version = '1.0'\nrelocatable = true\n");
append_to_file("$extdir/extension/kl_evil--1.0.sql",
	"INSERT INTO kingres.ledger_blocks DEFAULT VALUES;\n");
$node->append_conf('postgresql.conf', "extension_control_path = '$extdir:\$system'\n");
$node->reload;
like(err_of('ss', 'CREATE EXTENSION kl_evil'), qr/cannot write to table "ledger_blocks"/,
	'the script of another extension cannot write the ledger tables');
is($node->safe_psql('ss', q{SELECT count(*) FROM pg_extension WHERE extname = 'kl_evil'}), '0',
	'and the extension was not created');
# The guard remembers which extension it guards; when the extension is dropped
# and created again in one session (a new OID), it must follow.
$node->safe_psql('postgres', 'CREATE DATABASE cc');
$node->safe_psql('cc', 'CREATE EXTENSION kingres_ledger');
my ($cret, $cout, $cerr) = $node->psql('cc', qq{
	CREATE TEMP TABLE t (i int);
	INSERT INTO t VALUES (1);
	DROP EXTENSION kingres_ledger;
	CREATE EXTENSION kingres_ledger;
	INSERT INTO t VALUES (2);
	$blocks_sql;
});
like($cerr, qr/cannot write to table "ledger_blocks"/,
	'after DROP EXTENSION and CREATE EXTENSION in one session the new tables are guarded');
is($node->safe_psql('cc', 'SELECT count(*) FROM kingres.ledger_blocks'), '0', 'and nothing was written');

#--------------------------------------------------------------------------
# The honest path under the closed guard (L10-14)
#--------------------------------------------------------------------------
$node->safe_psql('lk', q{INSERT INTO docs VALUES (nextval('docs_seq'), 'honest');
	SELECT kingres.ledger_build_block();
	SELECT kingres.ledger_generate_digest();
	SELECT kingres.ledger_verify_recorded();
	CREATE TABLE more (id int, v text);
	SELECT kingres.ledger_enable('more', 'updatable');
	INSERT INTO more VALUES (1, 'a'); UPDATE more SET v = 'b'; DELETE FROM more;
	SELECT kingres.ledger_build_block();});
is($node->safe_psql('lk', q{SHOW kingres_ledger.allow_manual_write}), 'off',
	'the guard is closed in these sessions');
is($node->safe_psql('lk', q{SELECT count(*) FROM kingres.ledger_verify()}), '0',
	'an honest sequence of capture, block, digest, recorded verification and ledger_enable verifies');

$pub->stop;
$node->stop;
done_testing();
