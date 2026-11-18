# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/027_own_statement.pl
#
# L19-01 of the sixth review of the guard, and the decision that followed three
# rounds of patches to the timer: a role that is not a superuser never holds a
# lock of the extension across statements.  Every function that takes a lock
# (ledger_build_block and ledger_generate_digest: the builders'; ledger_enable
# and ledger_register_table: the registry's; ledger_verify_recorded: the
# verifications'; ledger_remove_stray and a write by hand: the removals', and
# they are a superuser's) may be called by such a role only as a statement of
# its own: SQLSTATE 25001 in a transaction block, in a multi-command string, in a
# savepoint, in an EXCEPTION block (or any subtransaction that a function of the
# statement began) and after the first statement of a pipeline.  A superuser is
# not restricted.
#
# Each context is run for each function, and each refusal is checked to leave no
# lock behind.  Then the one way that the rule cannot see the context change:
# a pipeline that sends BEGIN (which turns the implicit block into a regular
# one) and a SAVEPOINT after the call.  An error inside the savepoint would
# disable the timer of the backend (the error handler of the main loop does it)
# and the transaction would go on with the lock: the statements that open a
# block, a savepoint or a prepared transaction are refused while a role that is
# not a superuser holds a lock, and the transaction is rolled back.  Last, the
# order check of ledger_enable runs before the wait for the table (L19-04).
#
# There is no measured duration in this file: every verdict is a state that the
# server reports (a lock that is held or not, an error code, a session that is
# idle) after the statement has returned.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('own_statement');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "max_prepared_transactions = 5\n");
$node->start;

my $BUILDER = 19276;
my $REGISTRY = 19277;
my $VERIFY = 19278;

my $super = $node->safe_psql('postgres', 'SELECT current_user');
$node->safe_psql('postgres', qq{CREATE ROLE app LOGIN; CREATE ROLE inh LOGIN;
	GRANT "$super" TO inh WITH INHERIT TRUE, SET FALSE; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO app;
	GRANT EXECUTE ON FUNCTION kingres.ledger_build_block(), kingres.ledger_generate_digest(),
		kingres.ledger_verify_recorded(bigint, bigint), kingres.ledger_enable(regclass, text) TO app;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a');
	SELECT kingres.ledger_build_block();
});
my $ntab = 0;
sub fresh_table
{
	my $t = 'u' . (++$ntab);
	$node->safe_psql('d', "CREATE TABLE $t AS SELECT g AS id FROM generate_series(1, 3) g; ALTER TABLE $t OWNER TO app");
	return $t;
}

sub session_as
{
	my ($role) = @_;
	local $ENV{PGUSER} = $role;
	my $s = $node->background_psql('d', on_error_stop => 0);
	my $pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	return ($s, $pid);
}

sub ext_locks
{
	my ($pid) = @_;
	return $node->safe_psql('d', qq{SELECT coalesce(string_agg(objsubid::text, ',' ORDER BY objsubid), '')
		FROM pg_locks WHERE locktype = 'object' AND classid = 'pg_extension'::regclass
		AND objsubid > 0 AND pid = $pid AND granted});
}

sub nothing_held
{
	return $node->safe_psql('d', q{SELECT count(*) FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid > 0}) eq '0';
}

# What a function is called with: a statement, and a statement for a fresh
# table each time for the two that take a table.
my @functions = (
	[ 'ledger_build_block',      sub { 'SELECT kingres.ledger_build_block()' } ],
	[ 'ledger_generate_digest',  sub { 'SELECT kingres.ledger_generate_digest()' } ],
	[ 'ledger_verify_recorded',  sub { 'SELECT kingres.ledger_verify_recorded()' } ],
	[ 'ledger_enable',           sub { "SELECT kingres.ledger_enable('" . fresh_table() . "')" } ],
	[ 'ledger_register_table',   sub { "SELECT kingres.ledger_register_table('" . fresh_table() . "')" } ],
);

my ($app0, $pid0) = session_as('app');
# ledger_register_table cannot be granted to a role (the hook refuses the grant): the role that
# can call it is a member of the owner role that inherits its privileges and cannot become it.
my ($inh, $inh_pid) = session_as('inh');

#--------------------------------------------------------------------------
# The control: as a statement of its own, each one that a role may call works
# and holds nothing when it has returned.
#--------------------------------------------------------------------------
foreach my $f (@functions)
{
	next if $f->[0] eq 'ledger_register_table';	# a superuser's function; the control is the refusal below
	$app0->{stderr} = '';
	my $sql = $f->[1]->();
	my $out = $app0->query($sql);
	unlike($app0->{stderr}, qr/ERROR/, "$f->[0]: as a statement of its own it works");
	is(ext_locks($pid0), '', "$f->[0]: and the session holds no lock when it has returned");
}

#--------------------------------------------------------------------------
# The contexts: each is refused with 25001 for each function, and leaves no lock.
#--------------------------------------------------------------------------
my $refused = qr/cannot run inside a transaction block for a role that is not a superuser/;
foreach my $f (@functions)
{
	my ($name, $call) = @$f;
	my $role = $name eq 'ledger_register_table' ? 'inh' : 'app';
	my $app = $role eq 'inh' ? $inh : $app0;
	my $pid = $role eq 'inh' ? $inh_pid : $pid0;

	# a transaction block
	$app->{stderr} = '';
	$app->query('BEGIN');
	$app->query($call->());
	like($app->{stderr}, qr/ERROR:  $name $refused/, "$name: refused in a transaction block");
	is(ext_locks($pid), '', "$name: no lock after the refusal in a transaction block");
	$app->query('ROLLBACK');

	# a savepoint
	$app->{stderr} = '';
	$app->query('BEGIN');
	$app->query('SAVEPOINT s');
	$app->query($call->());
	like($app->{stderr}, qr/ERROR:  $name $refused/, "$name: refused in a savepoint");
	is(ext_locks($pid), '', "$name: no lock after the refusal in a savepoint");
	$app->query('ROLLBACK');

	# a multi-command string (an implicit block)
	# psql sends stdin one statement at a time; -c sends the string as one message
	my ($ret, $out, $err) = $node->psql('d', '', extra_params => [ '-U', $role, '-c', $call->() . '; SELECT 1' ]);
	like($err, qr/ERROR:  $name $refused/, "$name: refused in a multi-command string");
	ok(nothing_held(), "$name: nothing held after the multi-command string");

	# an EXCEPTION block of a DO block, the error caught
	$app->{stderr} = '';
	my $c = $call->();
	(my $perform = $c) =~ s/^SELECT /PERFORM /;
	$out = $app->query(qq{DO \$\$ BEGIN BEGIN $perform; RAISE NOTICE 'not_refused'; EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'state=%', SQLSTATE; END; END \$\$});
	like($app->{stderr}, qr/NOTICE:  state=25001/, "$name: refused in an EXCEPTION block (SQLSTATE 25001)");
	unlike($app->{stderr}, qr/not_refused/, "$name: and the call did not go through");
	is(ext_locks($pid), '', "$name: no lock after the EXCEPTION block");

	# a subtransaction that a function of the same statement began: a function with an
	# EXCEPTION clause that is called by a function without one, from a SELECT
	$app->{stderr} = '';
	$c = $call->();
	($perform = $c) =~ s/^SELECT /PERFORM /;
	$app->query(qq{CREATE OR REPLACE FUNCTION public.f_inner() RETURNS text LANGUAGE plpgsql AS \$f\$
		BEGIN
			BEGIN $perform; RETURN 'went_through';
			EXCEPTION WHEN OTHERS THEN RETURN 'state=' || SQLSTATE; END;
		END \$f\$});
	$app->query(q{CREATE OR REPLACE FUNCTION public.f_outer() RETURNS text LANGUAGE sql AS 'SELECT public.f_inner()'});
	$out = $app->query('SELECT public.f_outer()');
	like($out, qr/state=25001/, "$name: refused in a subtransaction that a function of the statement began");
	is(ext_locks($pid), '', "$name: no lock after it");

	# a call that is not the first statement of a pipeline
	($ret, $out, $err) = $node->psql('d',
		"\\startpipeline\nSELECT 1 \\bind \\sendpipeline\n"
		  . $call->() . " \\bind \\sendpipeline\n\\endpipeline\n",
		extra_params => [ '-U', $role ]);
	like($err, qr/ERROR:  $name $refused/, "$name: refused after the first statement of a pipeline");
	ok(nothing_held(), "$name: nothing held after the pipeline");
}

# The error carries the detail and the hint that say what to do.
{
	$app0->{stderr} = '';
	$app0->query('BEGIN');
	$app0->query(q{SELECT kingres.ledger_enable('} . fresh_table() . q{')});
	like($app0->{stderr}, qr/HINT:  Call it as a statement of its own, outside BEGIN \.\.\. COMMIT, a savepoint and an EXCEPTION block \(ledger_enable: run it outside the transaction, or as a superuser\)/,
		'the hint says what a migration does instead: run ledger_enable outside the transaction, or as a superuser');
	$app0->query('ROLLBACK');
}

#--------------------------------------------------------------------------
# A superuser is not restricted: every function, in a block, in a savepoint, in
# an EXCEPTION block.
#--------------------------------------------------------------------------
{
	my ($sup, $spid) = session_as($node->safe_psql('postgres', 'SELECT current_user'));
	$sup->query('BEGIN');
	$sup->query('SAVEPOINT s');
	foreach my $f (@functions)
	{
		# Each in a savepoint of its own that is rolled back: a call that fails for
		# another reason (a table that has no capture trigger) does not stop the rest.
		$sup->query('SAVEPOINT x');
		$sup->{stderr} = '';
		$sup->query($f->[1]->());
		unlike($sup->{stderr}, qr/cannot run inside a transaction block/, "a superuser: $f->[0] in a block and a savepoint is not refused");
		$sup->query('ROLLBACK TO x');
	}
	$sup->query('COMMIT');
	$sup->{stderr} = '';
	$sup->query(qq{DO \$\$ BEGIN BEGIN PERFORM kingres.ledger_generate_digest(); EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'state=%', SQLSTATE; END; END \$\$});
	unlike($sup->{stderr}, qr/state=25001/, 'a superuser: a builder in an EXCEPTION block is not refused');
	$sup->quit;
}

#--------------------------------------------------------------------------
# The pipeline that changes the context after the call (L19-01).  The first
# statement of a pipeline is a statement of its own; a BEGIN that follows
# turns the implicit block into a regular one, a SAVEPOINT opens a
# subtransaction, an error in it disables every timer of the backend, and the
# transaction goes on with the lock.  The statements that would open a block
# are refused while the lock is held, and the transaction is rolled back.
# Verdict: after the Sync the session is idle, in no transaction, and holds
# nothing.
#--------------------------------------------------------------------------
sub pipeline_attack
{
	my ($tag, $opener, $lockname, $lockid, $callsql) = @_;
	my ($s, $p) = session_as('app');
	my $script = "\\startpipeline\n"
	  . "$callsql \\bind \\sendpipeline\n"
	  . "$opener \\bind \\sendpipeline\n"
	  . "SAVEPOINT s \\bind \\sendpipeline\n"
	  . "SELECT 1/0 \\bind \\sendpipeline\n"
	  . "\\syncpipeline\n"
	  . "SELECT 1 \\bind \\sendpipeline\n"
	  . "\\syncpipeline\n"
	  . "\\getresults\n"
	  . "\\endpipeline\n"
	  . "\\echo pipeline_done\n";
	$s->query_until(qr/pipeline_done/, $script);
	ok($node->poll_query_until('d', qq{SELECT state LIKE 'idle%' FROM pg_stat_activity WHERE pid = $p}),
		"$tag: the session is idle after the Sync");
	is($node->safe_psql('d', qq{SELECT state FROM pg_stat_activity WHERE pid = $p}), 'idle',
		"$tag: in no transaction, aborted or not");
	is(ext_locks($p), '', "$tag: and it holds no lock of the extension");
	like($s->{stderr}, qr/ERROR:  cannot start a transaction block, a savepoint or a prepared transaction while a role that is not a superuser holds the \Q$lockname\E of the ledger extension/,
		"$tag: the statement that opens a block is refused and names the lock");
	$s->{stderr} = '';
	is($s->query_safe("SELECT 'alive'"), 'alive', "$tag: and the session goes on");
	$s->quit;
	return;
}
pipeline_attack('pipeline: verification, BEGIN, SAVEPOINT, error', 'BEGIN', "verifications' lock", $VERIFY,
	'SELECT kingres.ledger_verify_recorded() > 0');
pipeline_attack('pipeline: verification, START TRANSACTION', 'START TRANSACTION', "verifications' lock", $VERIFY,
	'SELECT kingres.ledger_verify_recorded() > 0');
pipeline_attack('pipeline: builder, BEGIN', 'BEGIN', "block builders' lock", $BUILDER,
	'SELECT kingres.ledger_build_block() IS NULL');
pipeline_attack('pipeline: ledger_enable, BEGIN', 'BEGIN', "registry's lock", $REGISTRY,
	"SELECT kingres.ledger_enable('" . fresh_table() . "') IS NULL");

# A prepared transaction is the other way a lock could outlive the statement.
{
	my ($s, $p) = session_as('app');
	$s->query_until(qr/pipeline_done/,
		"\\startpipeline\n"
		  . "SELECT kingres.ledger_verify_recorded() > 0 \\bind \\sendpipeline\n"
		  . "PREPARE TRANSACTION 'kl_gid' \\bind \\sendpipeline\n"
		  . "\\syncpipeline\n\\getresults\n\\endpipeline\n\\echo pipeline_done\n");
	ok($node->poll_query_until('d', qq{SELECT state LIKE 'idle%' FROM pg_stat_activity WHERE pid = $p}),
		'pipeline: the session is idle after the Sync (PREPARE)');
	is($node->safe_psql('d', q{SELECT count(*) FROM pg_prepared_xacts}), '0',
		'no prepared transaction exists: a role that is not a superuser cannot prepare a transaction that holds a lock');
	ok(nothing_held(), 'and nothing is held');
	$s->quit;
}

#--------------------------------------------------------------------------
# L19-04: ledger_enable refuses a call that is out of the order of the locks
# before it waits for the lock on the table.  A superuser holds the
# verifications' lock in a block (a role that is not a superuser cannot), and
# another session holds the table: the call has to return KL006 at once, not
# wait.  A lock_timeout of a minute only bounds a test that fails.
#--------------------------------------------------------------------------
{
	my $t = fresh_table();
	my ($holder, $hpid) = session_as($node->safe_psql('postgres', 'SELECT current_user'));
	$holder->query_safe("BEGIN; LOCK TABLE $t IN SHARE ROW EXCLUSIVE MODE");
	my ($sup, $spid) = session_as($node->safe_psql('postgres', 'SELECT current_user'));
	$sup->query_safe("SET lock_timeout = '60s'");
	$sup->query_safe('BEGIN');
	$sup->query_safe('SELECT kingres.ledger_verify_recorded() > 0');
	$sup->{stderr} = '';
	$sup->query("SELECT kingres.ledger_enable('$t')");
	like($sup->{stderr}, qr/ERROR:  cannot take the registry's lock of the ledger extension: this transaction already holds the verifications' lock/,
		'ledger_enable after the verifications\' lock is refused with KL006 before it waits for the table lock');
	is($node->safe_psql('d', qq{SELECT count(*) FROM pg_locks WHERE pid = $spid AND NOT granted}), '0',
		'and the session waits for nothing');
	$sup->query('ROLLBACK');
	$holder->query_safe('ROLLBACK');
	$sup->quit;
	$holder->quit;

	# the same in one statement, for a role that is not a superuser
	$t = fresh_table();
	($holder, $hpid) = session_as($node->safe_psql('postgres', 'SELECT current_user'));
	$holder->query_safe("BEGIN; LOCK TABLE $t IN SHARE ROW EXCLUSIVE MODE");
	local $ENV{PGOPTIONS} = '-c search_path=public -c lock_timeout=60s';
	my ($ret, $out, $err) = $node->psql('d', '', extra_params => [ '-U', 'app', '-c',
		"SELECT kingres.ledger_verify_recorded(), kingres.ledger_enable('$t')" ]);
	like($err, qr/ERROR:  cannot take the registry's lock of the ledger extension/,
		'one statement of a role that is not a superuser: the same refusal, before the wait');
	unlike($err, qr/lock timeout/, 'it did not wait for the table');
	$holder->query_safe('ROLLBACK');
	$holder->quit;
}

$app0->quit;
$inh->quit;
ok(nothing_held(), 'nothing is held at the end');

done_testing();
