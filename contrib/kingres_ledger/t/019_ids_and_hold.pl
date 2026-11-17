# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/019_ids_and_hold.pl
#
# The two findings of the third review of the guard that were about the second
# invariant itself (L13-01, L13-02):
#
#   L13-01  The identity sequences of the registry and of the records can be moved
#           by a role that holds nextval and setval on them (every member of
#           pg_write_all_data does, with no grant to revoke), and no hook sees a
#           call of setval.  No writer takes an identifier from a sequence: after
#           the sequences were set back, to their maximum, into the middle of the
#           used values, or far ahead, every writer works, and takes the next free
#           identifier.
#   L13-02  A role that is not a superuser holds the locks of the extension until
#           the end of its transaction.  A pipeline without Sync keeps the
#           transaction open with the backend "active" (no timeout of PostgreSQL
#           reaches it) and a statement that goes on after the call lasts as long as
#           the role lets it (it can switch off statement_timeout and
#           transaction_timeout).  The bound is the extension's own setting,
#           kingres_ledger.lock_hold_limit, which the role cannot change: its
#           session is ended when the limit is up.
#
# Every wait below is for a condition (a lock that appears, a backend that is
# gone) with an upper bound that only fails the test when the condition never
# comes.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('ids_hold');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my $super = $node->safe_psql('postgres', 'SELECT current_user');
my $BUILDER = 19276;			# 0x4B4C
my $REGISTRY = 19277;			# 0x4B4D
my $VERIFY = 19278;				# 0x4B4E

$node->safe_psql('postgres', qq{
	CREATE ROLE inh LOGIN;
	GRANT "$super" TO inh WITH INHERIT TRUE, SET FALSE;
	CREATE ROLE wad LOGIN IN ROLE pg_write_all_data;
	CREATE ROLE app LOGIN;
	CREATE ROLE aud LOGIN;
	CREATE DATABASE d;
});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO inh, wad, app, aud;
	GRANT EXECUTE ON FUNCTION kingres.ledger_build_block(), kingres.ledger_generate_digest(),
		kingres.ledger_verify_recorded(bigint, bigint) TO aud;
	GRANT EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) TO app;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a');
	SELECT kingres.ledger_build_block();
});

sub as_role
{
	my ($role, $sql) = @_;
	return $node->safe_psql('d', $sql, extra_params => [ '-U', $role ]);
}

sub session_as
{
	my ($role) = @_;
	local $ENV{PGUSER} = $role;
	return $node->background_psql('d', on_error_stop => 0);
}

# Wait until the backend is gone; the bound is generous and decides nothing
# but the failure of a test whose backend never goes.
sub backend_gone
{
	my ($pid) = @_;
	return $node->poll_query_until('d',
		qq{SELECT count(*) = 0 FROM pg_stat_activity WHERE pid = $pid});
}

# A session that the server ended is already gone: its quit has nobody to talk to.
sub quit_quietly
{
	my ($s) = @_;
	eval { $s->quit };
	return;
}

sub holds_lock
{
	my ($pid, $subid) = @_;
	return $node->poll_query_until('d', qq{SELECT count(*) = 1 FROM pg_locks
		WHERE pid = $pid AND locktype = 'object' AND classid = 'pg_extension'::regclass
		  AND objsubid = $subid AND granted});
}

sub lock_count
{
	my ($subid) = @_;
	return $node->safe_psql('d', qq{SELECT count(*) FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid = $subid});
}

# What a writer and the block builder do, as a superuser, under a timeout that
# is generous: it decides nothing but the failure of a test where the lock is
# never let go.
sub builder_works
{
	my ($ret, $out, $err) = $node->psql('d', q{SET lock_timeout = '60s';
		INSERT INTO docs VALUES ((SELECT coalesce(max(id), 0) + 1 FROM docs), 'w');
		SELECT kingres.ledger_build_block() IS NOT NULL});
	return ($out =~ /^t$/m && $err eq '') ? 'works' : "stopped: $err";
}

#--------------------------------------------------------------------------
# L13-01: the sequences are moved; the writers work
#--------------------------------------------------------------------------
$node->safe_psql('d', q{
	CREATE FUNCTION public.move_seqs(mode text) RETURNS void LANGUAGE plpgsql AS $$
	DECLARE s record;
	BEGIN
		FOR s IN SELECT sequencename AS n, max_value AS mx FROM pg_sequences
		          WHERE schemaname = 'kingres' ORDER BY 1 LOOP
			IF mode = 'burn' THEN
				PERFORM count(nextval(format('kingres.%I', s.n)::regclass)) FROM generate_series(1, 2000);
			ELSE
				PERFORM setval(format('kingres.%I', s.n)::regclass,
				               CASE mode WHEN 'back' THEN 1 WHEN 'max' THEN s.mx ELSE 3 END);
			END IF;
		END LOOP;
	END $$;
	GRANT EXECUTE ON FUNCTION public.move_seqs(text) TO PUBLIC;
});
is($node->safe_psql('d', q{SELECT count(*) FROM pg_sequences WHERE schemaname = 'kingres'}),
	'4', 'the extension has four identity sequences');

my $n = 0;
foreach my $role (qw(wad inh))
{
	foreach my $mode (qw(back max middle burn))
	{
		$n++;
		as_role($role, "SELECT public.move_seqs('$mode')");
		my $tag = "$role/$mode";
		my ($ret, $out, $err) = $node->psql('d', qq{
			SET client_min_messages = error;
			CREATE TABLE t$n (id int);
			SELECT kingres.ledger_enable('t$n');
			INSERT INTO t$n VALUES (1);});
		is($err, '', "$tag: ledger_enable works");
		($ret, $out, $err) = $node->psql('d', q{SELECT kingres.ledger_generate_digest() IS NOT NULL});
		is($err, '', "$tag: ledger_generate_digest works");
		($ret, $out, $err) = $node->psql('d', q{SELECT kingres.ledger_verify_recorded() > 0});
		is($err, '', "$tag: ledger_verify_recorded works");
		# a record by hand (the superuser's own write), and its removal
		($ret, $out, $err) = $node->psql('d', q{
			SET kingres_ledger.allow_manual_write = on;
			INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
			  VALUES (now(), 'stray', 99999, sha256('stray'), '{}');
			SELECT kingres.ledger_remove_stray('test');});
		is($err, '', "$tag: a manual write and ledger_remove_stray work");
		is($node->safe_psql('d', q{
			SELECT (SELECT count(*) = max(table_id) FROM kingres.ledger_table)
			    AND (SELECT count(*) = max(verification_id) FROM kingres.ledger_verification)
			    AND (SELECT count(*) = max(removal_id) FROM kingres.ledger_removed)}),
			't', "$tag: the identifiers are the next free ones, with no gap");
		is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify() WHERE check_name NOT IN ('operator_removal', 'manual_write')}),
			'0', "$tag: the ledger verifies");
	}
}
# A role that may call the digest functions, with the sequences at their maximum.
as_role('wad', q{SELECT public.move_seqs('max')});
is(as_role('aud', q{SELECT kingres.ledger_generate_digest() IS NOT NULL}), 't',
	'aud: the digest is recorded with the sequences at their maximum');
like(as_role('aud', q{SELECT kingres.ledger_verify_recorded()}), qr/^\d+$/,
	'aud: and so is a verification');
is($node->safe_psql('d', q{SELECT (SELECT max(digest_id) FROM kingres.ledger_digest)	> (SELECT max((row_data->>'digest_id')::bigint) FROM kingres.ledger_removed	    WHERE source_table = 'ledger_digest' AND action = 'removed')}), 't',	'and the identifier of a digest is above that of every digest that was taken out');
# The sequences follow the identifiers that the writers took: a row that a
# superuser inserts by hand with the default gets the next one.
is($node->safe_psql('d', q{
	SET kingres_ledger.allow_manual_write = on;
	INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
	  SELECT now(), 'hand', height, block_hash, '{}' FROM kingres.ledger_blocks ORDER BY height DESC LIMIT 1;
	SELECT digest_id = (SELECT max(digest_id) FROM kingres.ledger_digest) FROM kingres.ledger_digest WHERE generated_by = 'hand'}),
	't', 'and a row inserted by hand takes the next identifier');

#--------------------------------------------------------------------------
# L13-02: the limit of the extension's own, which the role cannot change
#--------------------------------------------------------------------------
$node->safe_psql('d', q{ALTER ROLE aud SET kingres_ledger.lock_hold_limit = '8s'});
$node->safe_psql('d', q{ALTER ROLE app SET kingres_ledger.lock_hold_limit = '8s'});
my ($sret, $sout, $serr) = $node->psql('d',
	q{SET kingres_ledger.lock_hold_limit = 0}, extra_params => [ '-U', 'aud' ]);
like($serr, qr/permission denied to set parameter "kingres_ledger.lock_hold_limit"/,
	'the role that is limited cannot set the limit');
foreach my $guc (qw(statement_timeout transaction_timeout idle_in_transaction_session_timeout))
{
	($sret, $sout, $serr) = $node->psql('d', "SET $guc = 0", extra_params => [ '-U', 'aud' ]);
	is($serr, '', "the settings of PostgreSQL that look like a remedy are the role's own: $guc");
}

# (a) A client in pipeline mode that sends the call and never Sync.
{
	my $s = session_as('aud');
	my $pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$s->query_until(qr/pipeline_sent/,
		"\\startpipeline\nSELECT kingres.ledger_build_block() \\bind \\sendpipeline\n\\flushrequest\n\\getresults\n\\echo pipeline_sent\n");
	ok(holds_lock($pid, $BUILDER), 'pipeline without Sync: the role holds the lock of the builders');
	is($node->safe_psql('d', qq{SELECT state FROM pg_stat_activity WHERE pid = $pid}), 'active',
		'and its backend is "active", where idle_in_transaction_session_timeout does not reach');
	ok(backend_gone($pid), 'its session is ended by the limit of the extension');
	is(lock_count($BUILDER), '0', 'and the lock of the builders is gone');
	is(builder_works(), 'works', 'so the builder of the server works');
	quit_quietly($s);
}
# (b) A statement that goes on after the call, with every timeout of PostgreSQL
# switched off by the role itself.
{
	my $s = session_as('aud');
	my $pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$s->query_safe('SET statement_timeout = 0; SET transaction_timeout = 0; SET idle_in_transaction_session_timeout = 0');
	$s->query_until(qr/long_started/,
		"\\echo long_started\nSELECT kingres.ledger_build_block() IS NOT NULL, pg_sleep(100000);\n");
	ok(holds_lock($pid, $BUILDER), 'a long statement after the call: the role holds the lock of the builders');
	ok(backend_gone($pid), 'its session is ended by the limit of the extension');
	is(lock_count($BUILDER), '0', 'and the lock is gone');
	is(builder_works(), 'works', 'so the builder of the server works');
	quit_quietly($s);
}
# (c) The recorded verification and ledger_enable hold locks of their own.
{
	my $s = session_as('aud');
	my $pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$s->query_until(qr/v_started/,
		"\\echo v_started\nBEGIN;\nSELECT kingres.ledger_verify_recorded();\nSELECT pg_sleep(100000);\n");
	ok(holds_lock($pid, $VERIFY), 'ledger_verify_recorded in a transaction: the role holds the lock of its identifiers');
	ok(backend_gone($pid), 'its session is ended by the limit of the extension');
	is(lock_count($VERIFY), '0', 'and the lock is gone');
	is(as_role('aud', q{SELECT kingres.ledger_verify_recorded() > 0}), 't', 'so another verification is recorded');
	quit_quietly($s);

	$node->safe_psql('d', q{CREATE TABLE apptab (id int); ALTER TABLE apptab OWNER TO app; CREATE TABLE apptab2 (id int); ALTER TABLE apptab2 OWNER TO app});
	$s = session_as('app');
	$pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$s->query_until(qr/e_started/,
		"\\echo e_started\nBEGIN;\nSELECT kingres.ledger_enable('apptab');\nSELECT pg_sleep(100000);\n");
	ok(holds_lock($pid, $REGISTRY), 'ledger_enable in a transaction: the owner of the table holds the lock of the registry');
	ok(backend_gone($pid), 'its session is ended by the limit of the extension');
	is(lock_count($REGISTRY), '0', 'and the lock is gone');
	is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_table WHERE relid = 'apptab'::regclass}), '0',
		'the registration that was not committed is gone with it');
	is(as_role('app', q{SELECT kingres.ledger_enable('apptab2') IS NOT NULL}), 't', 'and the next ledger_enable works');
	quit_quietly($s);
}
# (d) A call that returns in time is not touched, any number of times.
{
	my $bad = 0;
	for my $i (1 .. 15)
	{
		my ($ret, $out, $err) = $node->psql('d',
			q{SELECT kingres.ledger_build_block() IS NULL AS nothing_pending}, extra_params => [ '-U', 'aud' ]);
		$bad++ if $err ne '';
	}
	is($bad, 0, 'fifteen calls of the builder by aud, each a statement of its own, are all served');
}
# (e) A superuser is outside the limit: the service role of the control plane
# keeps a transaction with the lock open for as long as it needs.
{
	$node->safe_psql('d', qq{ALTER ROLE "$super" SET kingres_ledger.lock_hold_limit = '2s'});
	my $s = session_as($super);
	my $pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$s->query_safe('BEGIN; SELECT kingres.ledger_build_block() IS NULL AS nothing_pending;');
	ok(holds_lock($pid, $BUILDER), 'a superuser in a transaction holds the lock of the builders');
	$s->query_safe('SELECT pg_sleep(6)');
	is($s->query_safe('SELECT 1 + 1'), '2', 'and is not ended when the limit is long past');
	is(lock_count($BUILDER), '1', 'the lock is still held');
	$s->query_safe('COMMIT');
	quit_quietly($s);
	$node->safe_psql('d', qq{ALTER ROLE "$super" RESET kingres_ledger.lock_hold_limit});
}
is($node->safe_psql('d', q{SELECT coalesce(string_agg(check_name || ' ' || coalesce(object, '') || ' ' || detail, '; '), '') FROM kingres.ledger_verify() WHERE check_name NOT IN ('operator_removal', 'manual_write', 'digest_json')}),	'', 'the ledger verifies');

done_testing();
