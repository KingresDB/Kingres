# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/023_writers.pl
#
# The writers of the ledger records at the same time (L15-03, L15-04, L15-05,
# L15-06, L15-08, L15-09 of the fourth review of the guard).  The identifiers
# of the rows that ledger_generate_digest, ledger_verify_recorded,
# ledger_remove_stray, ledger_enable and a write by hand make are the next free
# ones, taken under the lock of the writer of that table; the test starts two
# writers at a time, which no earlier test did.
#
#   L15-03  The wait for the verifications' lock ends at once at a cancel, a
#           statement_timeout, a lock_timeout and a terminate; it is not caught
#           and is not recorded as a failed verification.
#   L15-04  Every writer works, or says why not, at every isolation level of
#           the caller, with a concurrent writer that committed after the
#           snapshot of the caller: ledger_verify_recorded and the record of a
#           write by hand read the largest committed identifier now; the
#           builders, ledger_enable and ledger_remove_stray require READ
#           COMMITTED, with a clear error.
#   L15-05  A row that a superuser writes by hand takes the lock of that table's
#           writer first: the honest writer waits for it and then works.
#   L15-08  An identifier that a transaction took is given again after it rolled
#           back (this is what the README says an identifier is).
#   L15-09  The largest removed digest is read through an index.
#   Lock order  A stress run of a few thousand rounds of every writer, a
#           superuser's hand-made rows and ledger_remove_stray in one
#           transaction, a recorded verification with the builder: no deadlock,
#           no duplicate key, no error.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('writers');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "deadlock_timeout = 300ms\n"
	  . "log_min_messages = warning\n");
$node->start;

my $super = $node->safe_psql('postgres', 'SELECT current_user');
my $BUILDER = 19276;			# 0x4B4C
my $REGISTRY = 19277;			# 0x4B4D
my $VERIFY = 19278;				# 0x4B4E
my $REMOVED = 19279;			# 0x4B4F

$node->safe_psql('postgres', q{CREATE ROLE aud LOGIN; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT EXECUTE ON FUNCTION kingres.ledger_generate_digest(), kingres.ledger_build_block(),
		kingres.ledger_verify_recorded(bigint, bigint) TO aud;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a');
	SELECT kingres.ledger_build_block();
});

sub session_as
{
	my ($role) = @_;
	local $ENV{PGUSER} = $role;
	my $s = $node->background_psql('d', on_error_stop => 0);
	return $s;
}

sub as_role
{
	my ($role, $sql) = @_;
	return $node->safe_psql('d', $sql, extra_params => [ '-U', $role ]);
}

sub quit_quietly
{
	my ($s) = @_;
	eval { $s->quit };
	return;
}

# Wait until some backend waits for the lock of the extension with this
# sub-object number; the bound only fails a test whose backend never waits.
sub waits_for
{
	my ($subid) = @_;
	return $node->poll_query_until('d', qq{SELECT count(*) >= 1 FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass
		  AND objsubid = $subid AND NOT granted});
}

sub holds
{
	my ($subid) = @_;
	return $node->poll_query_until('d', qq{SELECT count(*) = 1 FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass
		  AND objsubid = $subid AND granted});
}

sub nobody_waits
{
	return $node->poll_query_until('d', q{SELECT count(*) = 0 FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND NOT granted});
}

my $HAND_DIGEST = q{INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
	SELECT now(), 'hand', b.height, b.block_hash, '{}'::jsonb
	  FROM kingres.ledger_blocks b ORDER BY b.height DESC LIMIT 1};
my $HAND_VERIFICATION = q{INSERT INTO kingres.ledger_verification
	(started_at, finished_at, started_by, from_height, to_height, state, problem_count, blocks_checked, rows_checked)
	VALUES (now(), now(), 'hand', 0, 0, 'clean', 0, 0, 0)};
my $HAND_REMOVED = q{INSERT INTO kingres.ledger_removed (removed_at, removed_by, reason, source_table, row_data)
	VALUES (now(), 'hand', 'a row written by hand', 'ledger_verification', '{}'::jsonb)};

#--------------------------------------------------------------------------
# L15-03: the wait for the verifications' lock is cancelable
#--------------------------------------------------------------------------
{
	my $h = session_as($super);
	$h->query_safe('BEGIN; SELECT kingres.ledger_verify_recorded() > 0');
	ok(holds($VERIFY), 'precondition: a transaction holds the verifications\' lock');

	my $timed_out = 0;
	my ($ret, $out, $err) = $node->psql('d',
		q{SET statement_timeout = '2s'; SELECT kingres.ledger_verify_recorded()},
		extra_params => [ '-U', 'aud' ], timeout => 60, timed_out => \$timed_out);
	ok(!$timed_out, 'L15-03: a statement_timeout ends the wait for the lock (it did not at 10 s before)');
	like($err, qr/canceling statement due to statement timeout/, 'with the timeout as the error');

	($ret, $out, $err) = $node->psql('d',
		q{SET lock_timeout = '1s'; SELECT kingres.ledger_verify_recorded()},
		extra_params => [ '-U', 'aud' ], timeout => 60, timed_out => \$timed_out);
	ok(!$timed_out, 'a lock_timeout ends the wait');
	like($err, qr/canceling statement due to lock timeout/, 'with the lock timeout as the error');

	my $c = session_as('aud');
	$c->query_until(qr/c_started/, "\\echo c_started\nSELECT kingres.ledger_verify_recorded();\n");
	ok(waits_for($VERIFY), 'a call waits for the lock');
	my $pid = $node->safe_psql('d', qq{SELECT pid FROM pg_locks WHERE locktype = 'object'
		AND classid = 'pg_extension'::regclass AND objsubid = $VERIFY AND NOT granted});
	$node->safe_psql('d', "SELECT pg_cancel_backend($pid)");
	ok(nobody_waits(), 'pg_cancel_backend ends the wait with one cancel (it needed two)');
	$c->{stderr} = '';
	is($c->query('SELECT 42'), '42', 'and the session is usable');
	quit_quietly($c);

	$c = session_as('aud');
	$pid = $c->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$c->query_until(qr/c_started/, "\\echo c_started\nSELECT kingres.ledger_verify_recorded();\n");
	ok(waits_for($VERIFY), 'another call waits for the lock');
	$node->safe_psql('d', "SELECT pg_terminate_backend($pid)");
	ok($node->poll_query_until('d', qq{SELECT count(*) = 0 FROM pg_stat_activity WHERE pid = $pid}),
		'pg_terminate_backend ends it at once');
	quit_quietly($c);

	$h->query_safe('COMMIT');
	quit_quietly($h);
	is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verification WHERE state = 'failed'}), '0',
		'no verification was recorded as failed for a wait that was cancelled');
	is(as_role('aud', q{SELECT kingres.ledger_verify_recorded() > 0}), 't', 'and the next call is served');
}

#--------------------------------------------------------------------------
# L15-04: every writer at every isolation level, with a concurrent committer
#--------------------------------------------------------------------------
sub fresh_stderr
{
	my ($s) = @_;
	$s->{stderr} = '';
	return;
}

for my $iso ('READ COMMITTED', 'REPEATABLE READ', 'SERIALIZABLE')
{
	# ledger_verify_recorded: the second call takes the id after the first one's.
	# a superuser: a role that is not one cannot call it in a transaction block (L19-01)
	my $s1 = session_as($super);
	$s1->query_safe("BEGIN ISOLATION LEVEL $iso; SELECT count(*) FROM kingres.ledger_blocks");
	my $id2 = as_role('aud', q{SELECT kingres.ledger_verify_recorded()});
	fresh_stderr($s1);
	my $id1 = $s1->query('SELECT kingres.ledger_verify_recorded()');
	is($s1->{stderr}, '', "ledger_verify_recorded at $iso after a concurrent one committed: no error");
	is($id1 + 0, $id2 + 1, "and it takes the next identifier ($id2 + 1)");
	$s1->query('COMMIT');
	quit_quietly($s1);

	# ledger_generate_digest (a superuser: a role that is not one cannot call it in a block)
	$s1 = session_as($super);
	$s1->query_safe("BEGIN ISOLATION LEVEL $iso; SELECT count(*) FROM kingres.ledger_blocks");
	as_role('aud', q{SELECT kingres.ledger_generate_digest() IS NOT NULL});
	fresh_stderr($s1);
	$s1->query('SELECT kingres.ledger_generate_digest() IS NOT NULL');
	if ($iso eq 'READ COMMITTED')
	{
		is($s1->{stderr}, '', 'ledger_generate_digest at READ COMMITTED after a concurrent one committed: no error');
	}
	else
	{
		like($s1->{stderr}, qr/requires the READ COMMITTED isolation level/,
			"ledger_generate_digest at $iso: refused, with the reason");
	}
	$s1->query('COMMIT');
	quit_quietly($s1);

	# ledger_enable
	my $t1 = lc("en_a_$iso") =~ s/ /_/gr;
	my $t2 = lc("en_b_$iso") =~ s/ /_/gr;
	$node->safe_psql('d', "CREATE TABLE $t1 (i int); CREATE TABLE $t2 (i int)");
	$s1 = session_as($super);
	$s1->query_safe("BEGIN ISOLATION LEVEL $iso; SELECT count(*) FROM kingres.ledger_blocks");
	$node->safe_psql('d', "SELECT kingres.ledger_enable('$t2')");
	fresh_stderr($s1);
	$s1->query("SELECT kingres.ledger_enable('$t1')");
	if ($iso eq 'READ COMMITTED')
	{
		is($s1->{stderr}, '', 'ledger_enable at READ COMMITTED after a concurrent one committed: no error');
	}
	else
	{
		like($s1->{stderr}, qr/ledger_enable requires the READ COMMITTED isolation level/,
			"ledger_enable at $iso: refused, with the reason");
	}
	$s1->query('COMMIT');
	quit_quietly($s1);

	# ledger_remove_stray: a superuser's act, with the setting on
	$s1 = session_as($super);
	$s1->query_safe("SET kingres_ledger.allow_manual_write = on; BEGIN ISOLATION LEVEL $iso; SELECT count(*) FROM kingres.ledger_blocks");
	fresh_stderr($s1);
	$s1->query(q{SELECT kingres.ledger_remove_stray('nothing to remove')});
	if ($iso eq 'READ COMMITTED')
	{
		is($s1->{stderr}, '', 'ledger_remove_stray at READ COMMITTED: no error');
	}
	else
	{
		like($s1->{stderr}, qr/ledger_remove_stray requires the READ COMMITTED isolation level/,
			"ledger_remove_stray at $iso: refused, with the reason");
	}
	$s1->query('COMMIT');
	quit_quietly($s1);

	# The record that a write by hand leaves: the second writer's snapshot does not see the first's.
	my $m1 = session_as($super);
	my $m2 = session_as($super);
	$m1->query_safe("SET kingres_ledger.allow_manual_write = on; BEGIN ISOLATION LEVEL $iso; SELECT count(*) FROM kingres.ledger_blocks");
	$m2->query_safe("SET kingres_ledger.allow_manual_write = on");
	$m2->query_safe($HAND_DIGEST);
	fresh_stderr($m1);
	$m1->query($HAND_DIGEST);
	is($m1->{stderr}, '', "a write by hand at $iso after another one committed: recorded, no error");
	$m1->query('COMMIT');
	quit_quietly($m1);
	quit_quietly($m2);
}
is($node->safe_psql('d', q{SELECT count(*) = count(DISTINCT removal_id) FROM kingres.ledger_removed}), 't',
	'the records of the writes by hand have distinct identifiers');
is($node->safe_psql('d', q{SELECT count(*) = count(DISTINCT digest_id) FROM kingres.ledger_digest}), 't',
	'the digests have distinct identifiers');
is($node->safe_psql('d', q{SELECT count(*) = count(DISTINCT verification_id) FROM kingres.ledger_verification}), 't',
	'so have the verifications');
is($node->safe_psql('d', q{SELECT count(*) = count(DISTINCT table_id) FROM kingres.ledger_table}), 't',
	'and the registered tables');

#--------------------------------------------------------------------------
# L15-05: a row written by hand, and the honest writer of the same table
#--------------------------------------------------------------------------
sub hand_row_and_writer
{
	my ($what, $hand_sql, $subid, $writer_role, $writer_sql, $setting) = @_;
	my $m = session_as($super);
	$m->query_safe('SET kingres_ledger.allow_manual_write = on; BEGIN');
	$m->query_safe($hand_sql);
	my $g = session_as($writer_role);
	$g->query_safe('SET kingres_ledger.allow_manual_write = on') if $setting;
	fresh_stderr($g);
	$g->query_until(qr/g_started/, "\\echo g_started\n$writer_sql;\n\\echo g_done\n");
	ok(waits_for($subid), "$what: the honest writer waits for the lock of the writer, not for the row");
	$m->query_safe('COMMIT');
	$g->query_until(qr/g_done/, '');
	is($g->{stderr}, '', "$what: and works when the row is committed (no duplicate key)");
	quit_quietly($m);
	quit_quietly($g);
	return;
}

hand_row_and_writer('a digest by hand', $HAND_DIGEST, $BUILDER, 'aud',
	'SELECT kingres.ledger_generate_digest() IS NOT NULL', 0);
hand_row_and_writer('a verification by hand', $HAND_VERIFICATION, $VERIFY, 'aud',
	'SELECT kingres.ledger_verify_recorded() > 0', 0);

# A stray digest for ledger_remove_stray to remove, and a record by hand that is open.
$node->safe_psql('d', q{SET kingres_ledger.allow_manual_write = on;
	INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
	VALUES (now(), 'stray', 999999, decode(repeat('cd', 32), 'hex'), '{}'::jsonb)});
hand_row_and_writer('a record of a removal by hand', $HAND_REMOVED, $BUILDER, $super,
	q{SELECT kingres.ledger_remove_stray('t') = 1}, 1);
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_digest WHERE height = 999999}), '0',
	'ledger_remove_stray removed the stray digest');
is($node->safe_psql('d', q{SELECT count(*) = count(DISTINCT removal_id) FROM kingres.ledger_removed}), 't',
	'with a distinct identifier');

#--------------------------------------------------------------------------
# L15-08: what an identifier is
#--------------------------------------------------------------------------
{
	# a superuser: a role that is not one cannot call it in a transaction block (L19-01)
	my $s = session_as($super);
	my $in = $s->query_safe('BEGIN; SELECT kingres.ledger_verify_recorded()');
	$in =~ s/\s+//g;
	$s->query_safe('ROLLBACK');
	my $again = as_role('aud', q{SELECT kingres.ledger_verify_recorded()});
	is($again + 0, $in + 0,
		'L15-08: an identifier that a rolled back transaction took is given again (README, "Identifiers")');
	is($node->safe_psql('d', qq{SELECT count(*) FROM kingres.ledger_verification WHERE verification_id = $in}), '1',
		'and there is one row with it: the number means a row only after the commit');
	quit_quietly($s);
}

#--------------------------------------------------------------------------
# L15-09: the largest removed digest is found through an index
#--------------------------------------------------------------------------
like($node->safe_psql('d', q{SET enable_seqscan = off; SET enable_bitmapscan = off;
	EXPLAIN (COSTS OFF)
	SELECT pg_catalog.max((r.row_data OPERATOR(pg_catalog.->>) 'digest_id')::pg_catalog.int8)
	  FROM kingres.ledger_removed r
	 WHERE r.source_table OPERATOR(pg_catalog.=) 'ledger_digest'
	   AND r.action OPERATOR(pg_catalog.=) 'removed'}),
	qr/ledger_removed_digest_idx/, 'L15-09: ledger_removed_digest_idx serves the query of the digest id');

#--------------------------------------------------------------------------
# The order of the locks: a few thousand rounds of every writer at once
#--------------------------------------------------------------------------
$node->safe_psql('d', q{
	CREATE FUNCTION public.stress_enable(n bigint) RETURNS void LANGUAGE plpgsql AS $$
	BEGIN
		EXECUTE format('CREATE TABLE public.s_%s (i int)', n);
		PERFORM kingres.ledger_enable(format('public.s_%s', n)::pg_catalog.regclass);
	END $$;
	CREATE SEQUENCE public.doc_seq START 1000;
});

my $dir = $PostgreSQL::Test::Utils::tmp_check;
append_to_file("$dir/s_digest.sql", "SELECT kingres.ledger_generate_digest() IS NOT NULL;\n");
append_to_file("$dir/s_verify.sql", "SELECT kingres.ledger_verify_recorded() > 0;\n");
append_to_file("$dir/s_hand1.sql", qq{SET kingres_ledger.allow_manual_write = on;
BEGIN;
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
	VALUES (now(), 'stress', 888888, decode(repeat('ee', 32), 'hex'), '{}'::jsonb);
SELECT kingres.ledger_remove_stray('stress') >= 0;
COMMIT;
});
append_to_file("$dir/s_hand2.sql", qq{SET kingres_ledger.allow_manual_write = on;
BEGIN;
$HAND_VERIFICATION;
$HAND_REMOVED;
COMMIT;
});
append_to_file("$dir/s_build.sql", qq{BEGIN;
SELECT kingres.ledger_build_block() IS NULL;
SELECT kingres.ledger_verify_recorded() > 0;
COMMIT;
});
append_to_file("$dir/s_enable.sql", "\\set n random(1, 2000000000)\nSELECT public.stress_enable(:n);\n");
append_to_file("$dir/s_write.sql", "INSERT INTO docs (id, body) VALUES (nextval('public.doc_seq'), 'x');\n");

my @pgbench = ('pgbench', '-n', '-h', $node->host, '-p', $node->port);
my $log_start = -s $node->logfile;
my ($aout, $aerr, $sout, $serr);
my $ra = IPC::Run::start(
	[ @pgbench, '-U', 'aud', '-c', '3', '-j', '3', '-t', '250',
	  '-f', "$dir/s_digest.sql\@3", '-f', "$dir/s_verify.sql\@2", 'd' ],
	'>', \$aout, '2>', \$aerr);
my $rs = IPC::Run::start(
	[ @pgbench, '-c', '5', '-j', '5', '-t', '400',
	  '-f', "$dir/s_hand1.sql\@4", '-f', "$dir/s_hand2.sql\@3", '-f', "$dir/s_build.sql\@3",
	  '-f', "$dir/s_enable.sql\@1", '-f', "$dir/s_write.sql\@4", 'd' ],
	'>', \$sout, '2>', \$serr);
ok($ra->finish, 'the honest role finished its 750 rounds') or diag($aerr);
ok($rs->finish, 'the superuser finished its 2000 rounds') or diag($serr);
unlike($aerr . $serr, qr/aborted|ERROR/, 'no client was stopped by an error');
like($aout, qr/number of transactions actually processed: 750\//, 'all of the 750 rounds were served');
like($sout, qr/number of transactions actually processed: 2000\//, 'all of the 2000 rounds were served');
my $log = slurp_file($node->logfile, $log_start);
unlike($log, qr/deadlock detected/, 'no deadlock in the server log');
unlike($log, qr/duplicate key value/, 'no duplicate key in the server log');

is($node->safe_psql('d', q{SELECT count(*) = count(DISTINCT digest_id) FROM kingres.ledger_digest}), 't',
	'digest identifiers are distinct');
is($node->safe_psql('d', q{SELECT count(*) = count(DISTINCT verification_id) FROM kingres.ledger_verification}), 't',
	'verification identifiers are distinct');
is($node->safe_psql('d', q{SELECT count(*) = count(DISTINCT removal_id) FROM kingres.ledger_removed}), 't',
	'removal identifiers are distinct');
is($node->safe_psql('d', q{SELECT count(*) = count(DISTINCT table_id) FROM kingres.ledger_table}), 't',
	'table identifiers are distinct');
is($node->safe_psql('d', q{SELECT coalesce(string_agg(DISTINCT check_name, ','), '') FROM kingres.ledger_verify()
	WHERE check_name NOT IN ('operator_removal', 'manual_write', 'digest_json')}),
	'', 'the ledger verifies: nothing but the records of the writes by hand');

done_testing();
