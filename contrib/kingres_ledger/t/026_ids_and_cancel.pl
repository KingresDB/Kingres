# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/026_ids_and_cancel.pl
#
# L17-06, L17-07 and L17-08 of the fifth review of the guard.
#
#   L17-06  A delete by hand of a recorded digest (a superuser with
#           allow_manual_write) records the removed identifier in
#           ledger_removed, as ledger_remove_stray does, so the identifier of the
#           digest does not come back for another row.  ledger_remove_stray
#           itself records each digest once, not twice.
#   L17-07  At the top of the type (a row with the largest identifier, which only
#           a superuser can insert) the writers raise a clear error and do not
#           wrap; deleting such a digest is the way out and is not recorded (a
#           record would leave no identifier for any later digest).
#   L17-08  A cancel that arrives while ledger_verify runs inside
#           ledger_verify_recorded (and not while it waits for the lock): with the
#           verifications' lock held by another session the cancel is raised and
#           nothing is recorded; with the lock free a record with the state
#           "failed" is written.  This is the test that kills the mutant of the
#           review (the failure record written with no lock at all).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use IPC::Run ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('ids_cancel');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my $psql = $node->installed_command('psql');
my $VERIFY = 19278;				# 0x4B4E
my $MAXID = '9223372036854775807';

$node->safe_psql('postgres', q{CREATE DATABASE d; CREATE DATABASE d2; CREATE DATABASE d3});
for my $db ('d', 'd2', 'd3')
{
	$node->safe_psql($db, q{
		CREATE EXTENSION kingres_ledger;
		CREATE TABLE docs (id int PRIMARY KEY, body text);
		SELECT kingres.ledger_enable('docs');
		INSERT INTO docs VALUES (1, 'a');
		SELECT kingres.ledger_build_block();
	});
}

sub by_hand
{
	my ($db, $sql) = @_;
	my ($ret, $out, $err) = $node->psql($db, "SET kingres_ledger.allow_manual_write = on;\n$sql");
	return ($ret, $out, $err);
}

#--------------------------------------------------------------------------
# L17-06
#--------------------------------------------------------------------------
$node->safe_psql('d', q{SELECT kingres.ledger_generate_digest() FROM generate_series(1, 3)});
is($node->safe_psql('d', q{SELECT string_agg(digest_id::text, ',' ORDER BY digest_id) FROM kingres.ledger_digest}), '1,2,3',
	'three recorded digests: 1, 2 and 3');
{
	my ($ret, $out, $err) = by_hand('d', q{DELETE FROM kingres.ledger_digest WHERE digest_id = 3});
	is($ret, 0, 'a superuser with allow_manual_write deletes the digest 3 by hand') or diag($err);
	is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_removed
		WHERE source_table = 'ledger_digest' AND action = 'removed' AND (row_data ->> 'digest_id')::int = 3
		  AND reason LIKE 'manual delete%'}), '1',
		'L17-06: the delete is recorded in ledger_removed, the row as it was');
	is($node->safe_psql('d', q{SELECT (kingres.ledger_generate_digest() ->> 'height') IS NOT NULL}), 't', 'a digest is recorded');
	is($node->safe_psql('d', q{SELECT max(digest_id) FROM kingres.ledger_digest}), '4',
		'the identifier 3 does not come back: the new digest has 4');
	like($node->safe_psql('d', q{SELECT string_agg(check_name, ',' ORDER BY check_name) FROM kingres.ledger_verify()}),
		qr/operator_removal/, 'and ledger_verify reports the removal, as it reports those of ledger_remove_stray');

	# All the digests at once, then a new one.
	($ret, $out, $err) = by_hand('d', q{DELETE FROM kingres.ledger_digest});
	is($ret, 0, 'all the digests are deleted by hand') or diag($err);
	$node->safe_psql('d', q{SELECT kingres.ledger_generate_digest()});
	is($node->safe_psql('d', q{SELECT min(digest_id) FROM kingres.ledger_digest}), '5',
		'the next digest is 5: no identifier of a deleted digest comes back');
}
{
	# ledger_remove_stray records each digest once.
	my ($ret, $out, $err) = by_hand('d', q{INSERT INTO kingres.ledger_digest (digest_id, generated_at, generated_by, height, block_hash, digest)
		OVERRIDING SYSTEM VALUE VALUES (100, now(), 'hand', 77, decode(repeat('00', 32), 'hex'), '{}'::jsonb)});
	is($ret, 0, 'a digest that names no block is written by hand') or diag($err);
	($ret, $out, $err) = by_hand('d', q{SELECT kingres.ledger_remove_stray('a stray digest')});
	like($out, qr/^1$/m, 'ledger_remove_stray removes it') or diag($err);
	is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_removed
		WHERE source_table = 'ledger_digest' AND action = 'removed' AND (row_data ->> 'digest_id')::int = 100}), '1',
		'and records it once (the trigger of the hand delete does not make a second record)');
	$node->safe_psql('d', q{SELECT kingres.ledger_generate_digest()});
	is($node->safe_psql('d', q{SELECT max(digest_id) FROM kingres.ledger_digest}), '101',
		'the identifier 100 does not come back after ledger_remove_stray either');
}

#--------------------------------------------------------------------------
# L17-07
#--------------------------------------------------------------------------
{
	my ($ret, $out, $err) = by_hand('d2', qq{INSERT INTO kingres.ledger_digest (digest_id, generated_at, generated_by, height, block_hash, digest)
		OVERRIDING SYSTEM VALUE SELECT $MAXID, now(), 'hand', b.height, b.block_hash, '{}'::jsonb
		FROM kingres.ledger_blocks b ORDER BY b.height DESC LIMIT 1});
	is($ret, 0, 'a digest with the largest identifier is inserted by a superuser') or diag($err);
	($ret, $out, $err) = $node->psql('d2', q{SELECT kingres.ledger_generate_digest()});
	like($err, qr/ERROR:  no identifier is left after $MAXID, the largest that this table holds/,
		'L17-07: ledger_generate_digest raises a clear error (no "bigint out of range" from the SQL, no wrap)');
	like($err, qr/HINT:  See the README, "Identifiers"/, 'with a hint to the README');
	($ret, $out, $err) = by_hand('d2', qq{DELETE FROM kingres.ledger_digest WHERE digest_id = $MAXID});
	is($ret, 0, 'a superuser deletes that digest by hand') or diag($err);
	is($node->safe_psql('d2', qq{SELECT count(*) FROM kingres.ledger_removed
		WHERE source_table = 'ledger_digest' AND action = 'removed' AND (row_data ->> 'digest_id')::numeric = $MAXID}), '0',
		'the delete is not recorded (the record would take the last identifier from every later digest)');
	is($node->safe_psql('d2', q{SELECT kingres.ledger_generate_digest() IS NOT NULL}), 't', 'and the writer works again: that is the way out');

	# The recorded verifications.
	($ret, $out, $err) = by_hand('d3', qq{INSERT INTO kingres.ledger_verification
		(verification_id, started_at, finished_at, started_by, from_height, to_height, state, problem_count, blocks_checked, rows_checked)
		OVERRIDING SYSTEM VALUE VALUES ($MAXID, now(), now(), 'hand', 0, 0, 'clean', 0, 0, 0)});
	is($ret, 0, 'a verification with the largest identifier is inserted by a superuser') or diag($err);
	($ret, $out, $err) = $node->psql('d3', q{SELECT kingres.ledger_verify_recorded()});
	like($err, qr/ERROR:  no identifier is left after $MAXID/, 'ledger_verify_recorded raises the clear error as well');
	($ret, $out, $err) = by_hand('d3', qq{DELETE FROM kingres.ledger_verification WHERE verification_id = $MAXID});
	like($err, qr/append-only/, 'the row cannot be removed (the table is append-only): the README says what a superuser does about it');
}

#--------------------------------------------------------------------------
# L17-08: a cancel inside the run of ledger_verify
#--------------------------------------------------------------------------
$node->safe_psql('postgres', q{CREATE DATABASE dc});
$node->safe_psql('dc', q{
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE vt AS SELECT g AS id, repeat('x', 20) AS body FROM generate_series(1, 250000) g;
	SELECT kingres.ledger_enable('vt');
	SELECT kingres.ledger_build_block();
});
my $t0 = time();
$node->safe_psql('dc', q{SELECT kingres.ledger_verify_recorded()});
my $dur = int((time() - $t0) * 1000);
note("a recorded verification of this ledger takes $dur ms");
cmp_ok($dur, '>', 500, "precondition: the verification runs long enough to be cancelled inside ($dur ms)");
my $cancel_after = int($dur * 0.4);
$cancel_after = 150 if $cancel_after < 150;
{
	# The lock is free: a record with the state "failed" is written.
	my $before = $node->safe_psql('dc', q{SELECT count(*) FROM kingres.ledger_verification WHERE state = 'failed'});
	my ($ret, $out, $err) = $node->psql('dc', "SET statement_timeout = '${cancel_after}ms';\nSELECT kingres.ledger_verify_recorded()");
	is($ret, 0, 'a verification cancelled inside its run, with the lock free: the call returns') or diag($err);
	like($err, qr/WARNING:  ledger verification \d+ failed: canceling statement due to statement timeout/,
		'and warns that it failed');
	is($node->safe_psql('dc', q{SELECT count(*) FROM kingres.ledger_verification WHERE state = 'failed'}), $before + 1,
		'a record with the state failed is written');
	is($node->safe_psql('dc', q{SELECT problems -> 0 ->> 'sqlstate' FROM kingres.ledger_verification
		WHERE state = 'failed' ORDER BY verification_id DESC LIMIT 1}), '57014', 'with the SQLSTATE of the cancel');
}
{
	# The lock is held by another session: the cancel is raised, nothing is recorded.
	my %h = (out => '', err => '');
	$h{h} = IPC::Run::start(
		[ $psql, '-XAtq', '-d', $node->connstr('dc'), '-c', q{BEGIN; SELECT kingres.ledger_verify_recorded() > 0; SELECT pg_sleep(120); COMMIT;} ],
		'>' => \$h{out}, '2>' => \$h{err}, IPC::Run::timeout(300));
	ok($node->poll_query_until('dc', qq{SELECT count(*) = 1 FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid = $VERIFY AND granted}),
		'another session holds the verifications\' lock');
	my $before = $node->safe_psql('dc', q{SELECT count(*) FROM kingres.ledger_verification});
	my ($ret, $out, $err) = $node->psql('dc', "SET statement_timeout = '${cancel_after}ms';\nSELECT kingres.ledger_verify_recorded()");
	isnt($ret, 0, 'a verification cancelled inside its run, with the lock held by another session: the call fails') or diag($out);
	like($err, qr/ERROR:  canceling statement due to statement timeout/, 'with the cancel (the cancel is raised, not swallowed)');
	unlike($err, qr/WARNING:  ledger verification \d+ failed/, 'and does not claim a record');
	is($node->safe_psql('dc', q{SELECT count(*) FROM kingres.ledger_verification}), $before,
		'nothing was recorded (a record without the lock could take the identifier of the other session)');
	$node->safe_psql('dc', q{SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE query LIKE '%pg_sleep(120)%' AND pid <> pg_backend_pid()});
	eval { $h{h}->finish; };
}

done_testing();
