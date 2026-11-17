# contrib/kingres_columnar/t/027_ending_writes.pl
#
# C7-01.  What a write does at every moment of the end of a transaction or a
# subtransaction, and at the other moments where PostgreSQL forbids an error: the
# server stays up, the transaction ends as it would have, and what was committed
# is exactly what the access method could write.
#
# The defect: a write that reached the access method when the transaction state
# was no longer "in progress" (a callback of another module at XACT_EVENT_COMMIT,
# XACT_EVENT_PREPARE, XACT_EVENT_ABORT, SUBXACT_EVENT_COMMIT_SUB,
# SUBXACT_EVENT_ABORT_SUB) got an ERROR, which PostgreSQL turns into a PANIC
# ("cannot abort transaction ... it was already committed"; "ERRORDATA_STACK_SIZE
# exceeded") or a failed assertion: the server stopped.
#
# The test module (t/kcevents/kcevents.c) holds a table open and writes into it,
# with table_tuple_insert, table_multi_insert and the other table access method
# calls, at the moment the setting kcevents.at names: the events of the
# transaction and subtransaction callbacks, the three phases of the release of a
# resource owner, and a critical section.  The same module on a heap table shows
# what PostgreSQL does there: with assertions it stops the server at every moment
# below that the table says "forbidden" (the README has the table), so the heap
# twin is compared only where heap writes.  Both orders of loading the two
# libraries run (the module's callbacks run before or after the access method's).
#
# It needs a compiler and the server's headers; where it cannot be built the test
# is skipped with the reason.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use IPC::Run ();
use lib $FindBin::RealBin;
use KcProbe qw(build_module);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

# a case that fails because the code under test is wrong must not hang the file on a lock that is never released
$ENV{PGOPTIONS} = '-c statement_timeout=240s';

my $dir = build_module('kcevents');

my $refused = qr/WARNING:  (insert|multi insert|delete|update|row lock|speculative insert) refused on columnar table "t": /;

# the scripts: the user writes 100 (and 101 in a savepoint), the module writes 1
my %body = (
	commit => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); COMMIT;",
	prepare => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); PREPARE TRANSACTION 'x';",
	abort => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); SELECT 1/0; ROLLBACK;",
	sub => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); SAVEPOINT s; INSERT INTO %T% VALUES (101); RELEASE s; COMMIT;",
	subabort => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); SAVEPOINT s; INSERT INTO %T% VALUES (101); ROLLBACK TO s; COMMIT;",
	crit => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); SELECT kcevents_crit(); COMMIT;",
);

# event => [ script, rows of the columnar table afterwards, rows heap would have (undef: heap stops),
#   live: the top-level transaction is still alive there, so the refusal fails it at its pre-commit (C8-02) ]
my @cases = (
	[ 'pre_commit', 'commit', '1,100', '1,100' ],
	[ 'pre_prepare', 'prepare', '1,100', '1,100' ],
	[ 'start_sub', 'sub', '1,100,101', '1,100,101' ],
	[ 'pre_commit_sub', 'sub', '1,100,101', '1,100,101' ],
	[ 'commit', 'commit', '100', undef ],
	[ 'prepare', 'prepare', '100', undef ],
	[ 'abort', 'abort', '', undef ],
	[ 'commit_sub', 'sub', '', undef, 1 ],
	[ 'abort_sub', 'subabort', '100', undef ],
	[ 'res_before_locks', 'commit', '100', undef ],
	[ 'res_locks', 'commit', '100', undef ],
	[ 'res_after_locks', 'commit', '100', undef ],
	[ 'none', 'crit', '', undef, 1 ],
);

foreach my $order ('after', 'before')
{
	# "after": the module's callbacks run after the access method's
	my $libs = $order eq 'after' ? "$dir/kcevents,kingres_columnar" : "kingres_columnar,$dir/kcevents";
	my $node = PostgreSQL::Test::Cluster->new("colending_$order");
	$node->init;
	$node->append_conf('postgresql.conf', "shared_preload_libraries = '$libs'\nmax_prepared_transactions = 10\n");
	$node->start;
	$node->safe_psql('postgres', qq{
CREATE EXTENSION kingres_columnar;
CREATE FUNCTION kcevents_open(text) RETURNS void AS '$dir/kcevents', 'kcevents_open' LANGUAGE C;
CREATE FUNCTION kcevents_crit() RETURNS void AS '$dir/kcevents', 'kcevents_crit' LANGUAGE C;
CREATE FUNCTION kcevents_writes() RETURNS bigint AS '$dir/kcevents', 'kcevents_writes' LANGUAGE C;});
	my $started = $node->safe_psql('postgres', 'SELECT pg_postmaster_start_time()');

	my $q = sub { return $node->safe_psql('postgres', $_[0]); };
	my $fresh = sub
	{
		# a prepared transaction that an earlier case left (only when the code under test is wrong)
		# would hold a lock on the tables for ever; end it, so that a failure is a failure and not a hang
		foreach my $gid (split /\n/, $q->("SELECT gid FROM pg_prepared_xacts"))
		{
			$q->("ROLLBACK PREPARED '$gid'");
		}
		$q->("DROP TABLE IF EXISTS t, h;
			CREATE TABLE t (a int) USING columnar;
			CREATE TABLE h (a int);");
	};
	my $run = sub
	{
		my ($at, $table, $script, $extra) = @_;
		(my $s = $script) =~ s/%T%/$table/g;
		my ($out, $err) = ('', '');
		$node->psql('postgres', "SET kcevents.at = '$at';\n" . ($extra // '') . "$s\nRESET kcevents.at;\nRESET kcevents.op;\n"
			. "SELECT 'PATH=' || coalesce((SELECT hits FROM kingres.columnar_write_paths() WHERE path = 'refused_ending'), -1);\n"
			. "SELECT 'LOST=' || coalesce((SELECT hits FROM kingres.columnar_write_paths() WHERE path = 'lost_raised'), -1);",
			stdout => \$out, stderr => \$err, on_error_stop => 0);
		$node->psql('postgres', "COMMIT PREPARED 'x'", stdout => \my $o2, stderr => \my $e2) if $s =~ /PREPARE TRANSACTION 'x'/;
		return ($out, $err);
	};
	my $rows = sub { return $q->("SELECT coalesce(string_agg(a::text, ',' ORDER BY a), '') FROM $_[0]"); };
	my $log_from = sub { return -s $node->logfile; };
	my $alive = sub
	{
		my ($name, $off) = @_;
		is($q->('SELECT pg_postmaster_start_time()'), $started, "$order: $name: the server was not restarted");
		unlike(slurp_file($node->logfile, $off), qr/PANIC|TRAP:|terminated by signal/, "$order: $name: no PANIC, no assertion failure, no crash in the log");
	};

	# ------------------------------------------------------------------
	# every moment, with an insert
	# ------------------------------------------------------------------
	foreach my $c (@cases)
	{
		my ($at, $script, $want, $heap_want, $live) = @$c;
		my $name = $at eq 'none' ? 'a critical section' : $at;

		$fresh->();
		my $off = $log_from->();
		my ($out, $err) = $run->($at, 't', $body{$script});
		my $safe = defined $heap_want;
		$alive->($name, $off);
		is($rows->('t'), $want, "$order: $name: the table has exactly the rows the access method could write ($want)");
		if ($safe)
		{
			unlike($err, qr/WARNING:  insert refused/, "$order: $name: the row is written, no warning");
			my ($hout, $herr) = $run->($at, 'h', $body{$script});
			is($rows->('h'), $rows->('t'), "$order: $name: ... and it is what the heap twin has");
		}
		else
		{
			like($err, qr/WARNING:  insert refused on columnar table "t": (the transaction is not in progress|inside a critical section)/,
				"$order: $name: refused with a warning, not an error that the server turns into a PANIC");
			if ($live)
			{
				# C8-02: the transaction is still alive, so it fails where an error is allowed instead of
				# committing without the row
				like($err, qr/ERROR:  a call on columnar table "t" was refused earlier in this transaction, so it cannot commit/,
					"$order: $name: the transaction that is still alive fails at its pre-commit instead of committing without the row");
				like($out, qr/LOST=1\b/, "$order: $name: ... counted once as raised");
			}
			else
			{
				unlike($err, qr/ERROR:  a call on columnar table/, "$order: $name: the transaction is decided: a warning and no error");
				like($out, qr/LOST=0\b/, "$order: $name: ... nothing raised");
			}
			like($out, qr/PATH=1\b/, "$order: $name: ... and counted once as refused");
			unlike($err, qr/ERROR:  columnar table "t" cannot be written now/, "$order: $name: ... never the error of the earlier version");
		}
		is($q->("SELECT count(*) FROM kingres.columnar_check()"), 0, "$order: $name: columnar_check has nothing to report");
		$q->("INSERT INTO t VALUES (7)");
		is($q->("SELECT count(*) FROM t WHERE a = 7"), 1, "$order: $name: the table is written again by an ordinary statement");
	}

	# ------------------------------------------------------------------
	# C8-02: where the top-level transaction is still alive (the commit of a
	# subtransaction, a critical section) a refused write makes the transaction fail
	# at the next moment an error is allowed, so that it cannot commit having lost a
	# row; where it is decided (commit, prepare, abort) the warning is all there is.
	# Savepoints nested and released, rolled back, PREPARE TRANSACTION, and the
	# message settings that suppress or keep the warning.
	# ------------------------------------------------------------------
	{
		my $lost = qr/ERROR:  a call on columnar table "t" was refused earlier in this transaction, so it cannot commit/;
		my @live = (
			# name, script, rows afterwards, number of refusal errors, the counter LOST
			[ 'a savepoint inside a savepoint, both released: the outer one fails at its release',
				"BEGIN; SELECT kcevents_open('t'); SAVEPOINT a; SAVEPOINT b; INSERT INTO t VALUES (100); RELEASE b; RELEASE a; COMMIT;", '', 1 ],
			[ 'the same, the outer savepoint released by the COMMIT',
				"BEGIN; SELECT kcevents_open('t'); SAVEPOINT a; SAVEPOINT b; INSERT INTO t VALUES (100); RELEASE b; COMMIT;", '', 1 ],
			[ 'the refusal belongs to a savepoint that is then rolled back: the transaction commits',
				"BEGIN; SELECT kcevents_open('t'); SAVEPOINT a; SAVEPOINT b; INSERT INTO t VALUES (100); RELEASE b; ROLLBACK TO a; INSERT INTO t VALUES (200); COMMIT;", '200', 0 ],
			[ 'the refusal belongs to the savepoint that is rolled back, an older one is released',
				"BEGIN; SELECT kcevents_open('t'); SAVEPOINT a; INSERT INTO t VALUES (100); SAVEPOINT b; INSERT INTO t VALUES (101); RELEASE b; ROLLBACK TO a; COMMIT;", '', 0 ],
			[ 'the next write to a columnar table fails at once, not only the commit',
				"BEGIN; SELECT kcevents_open('t'); SAVEPOINT a; INSERT INTO t VALUES (100); RELEASE a; INSERT INTO t VALUES (7); COMMIT;", '', 1 ],
			[ 'PREPARE TRANSACTION fails at its pre-prepare and no transaction is prepared',
				"BEGIN; SELECT kcevents_open('t'); SAVEPOINT a; INSERT INTO t VALUES (100); RELEASE a; PREPARE TRANSACTION 'y';", '', 1 ],
		);
		foreach my $c (@live)
		{
			my ($what, $script, $want, $nerr) = @$c;
			$fresh->();
			my $off = $log_from->();
			my ($out, $err) = $run->('commit_sub', 't', $script, "SET kcevents.once = on;
");
			$alive->($what, $off);
			is($rows->('t'), $want, "$order: $what: the table has exactly the rows that were kept ($want)");
			my $got = () = $err =~ /$lost/g;
			cmp_ok($got, '>=', $nerr ? 1 : 0, "$order: $what: the refusal error is raised" . ($nerr ? '' : ' by nothing here'));
			is($got, 0, "$order: $what: no error about a refusal") unless $nerr;
			is($q->("SELECT count(*) FROM pg_prepared_xacts"), 0, "$order: $what: no prepared transaction is left") if $script =~ /PREPARE/;
			is($q->("SELECT count(*) FROM kingres.columnar_check()"), 0, "$order: $what: columnar_check has nothing to report");
			$q->("INSERT INTO t VALUES (7)");
			is($q->("SELECT count(*) FROM t WHERE a = 7"), 1, "$order: $what: the table is written again by an ordinary statement");
		}

		# a critical section inside a savepoint that is rolled back, or released
		$fresh->();
		my ($out, $err) = $run->('none', 't', "BEGIN; SELECT kcevents_open('t'); INSERT INTO t VALUES (100); SAVEPOINT a; SELECT kcevents_crit(); ROLLBACK TO a; COMMIT;");
		is($rows->('t'), '100', "$order: a critical section in a savepoint that is rolled back: the transaction commits");
		unlike($err, $lost, "$order: ... without a refusal error");
		$fresh->();
		($out, $err) = $run->('none', 't', "BEGIN; SELECT kcevents_open('t'); INSERT INTO t VALUES (100); SAVEPOINT a; SELECT kcevents_crit(); RELEASE a; COMMIT;");
		is($rows->('t'), '', "$order: a critical section in a savepoint that is released: the transaction fails at its pre-commit");
		like($err, $lost, "$order: ... with the refusal error");

		# the decided moments stay what they were: a warning and nothing else, with a savepoint around
		foreach my $at ('commit', 'prepare', 'abort')
		{
			$fresh->();
			my $script = "BEGIN; SELECT kcevents_open('t'); SAVEPOINT a; INSERT INTO t VALUES (100); RELEASE a; "
				. ($at eq 'commit' ? "COMMIT;" : $at eq 'prepare' ? "PREPARE TRANSACTION 'x';" : "SELECT 1/0; ROLLBACK;");
			($out, $err) = $run->($at, 't', $script);
			is($rows->('t'), $at eq 'abort' ? '' : '100', "$order: a refusal at $at with a released savepoint: decided, the warning is all");
			unlike($err, $lost, "$order: ... no refusal error");
		}

		# what the settings of the messages do: nothing to the rule (the loss is remembered whether or
		# not the warning is sent or logged)
		foreach my $set ("SET client_min_messages = error;", "SET log_min_messages = panic;", "SET client_min_messages = debug5; SET log_min_messages = debug5;")
		{
			$fresh->();
			my $off = $log_from->();
			($out, $err) = $run->('commit_sub', 't', "BEGIN; SELECT kcevents_open('t'); $set INSERT INTO t VALUES (100); SAVEPOINT s; INSERT INTO t VALUES (101); RELEASE s; COMMIT;");
			$alive->("with $set", $off);
			is($rows->('t'), '', "$order: with \"$set\" the transaction still fails at its pre-commit");
			like($err, $lost, "$order: with \"$set\" the error is there");
			if ($set =~ /client_min_messages = error/)
			{
				unlike($err, qr/WARNING:  insert refused/, "$order: ... the warning is not sent to the client");
			}
			if ($set =~ /log_min_messages = panic/)
			{
				unlike(slurp_file($node->logfile, $off), qr/WARNING:  insert refused/, "$order: ... and not written to the log");
			}
		}

		# a log hook of another module that raises an error for the warning: at the commit of a
		# subtransaction that is an error of the subtransaction (it aborts); nothing is remembered,
		# nothing is left over for the next transaction, the server stays up
		{
			$fresh->();
			my $off = $log_from->();
			($out, $err) = $run->('commit_sub', 't', "BEGIN; SELECT kcevents_open('t'); SET kcevents.hook_raise = on; INSERT INTO t VALUES (100); SAVEPOINT s; INSERT INTO t VALUES (101); RELEASE s; COMMIT;");
			$alive->('a log hook that raises for the warning', $off);
			like($err, qr/ERROR:  kcevents: the log hook raises for a refusal of the extension/, "$order: a log hook that raises: its error is what the caller sees");
			unlike($err, $lost, "$order: ... and nothing was remembered of the refusal");
			is($rows->('t'), '', "$order: ... the transaction is rolled back");
			like($out, qr/LOST=0\b/, "$order: ... no refusal error was raised");
			($out, $err) = $run->('commit', 't', $body{commit});
			is($rows->('t'), '100', "$order: ... the next transaction of a new session is ordinary");
		}

		# a client that is gone when the warning is sent (the connection is broken at the
		# commit): the server does not stop and the transaction ends as it would have
		foreach my $at ('commit', 'commit_sub')
		{
			$fresh->();
			my $off = $log_from->();
			my $script = $at eq 'commit'
				? "BEGIN; SELECT kcevents_open('t'); INSERT INTO t VALUES (100); SELECT pg_sleep(4); COMMIT;"
				: "BEGIN; SELECT kcevents_open('t'); INSERT INTO t VALUES (100); SAVEPOINT s; SELECT pg_sleep(4); RELEASE s; COMMIT;";
			# one query message with the whole script, so that the server has the COMMIT before the
			# client goes away (a client that is killed between statements never sends it)
			my ($o, $e) = ('', '');
			my $h = IPC::Run::start(['psql', '-XAtq', '-d', $node->connstr('postgres'), '-c', "SET kcevents.at = '$at'; $script"], '>', \$o, '2>', \$e);
			# wait until the backend is in the transaction, then kill the client: the
			# socket is closed under the backend that is sleeping
			$node->poll_query_until('postgres', "SELECT count(*) > 0 FROM pg_stat_activity WHERE query LIKE '%pg_sleep(4)%' AND pid <> pg_backend_pid()");
			$h->signal('KILL');
			eval { $h->finish; };
			# the backend ends its transaction, finds the client gone and exits
			$node->poll_query_until('postgres', "SELECT count(*) = 0 FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()");
			$alive->("a broken client connection at $at", $off);
			like($q->("SELECT pg_postmaster_start_time()"), qr/./, "$order: the server answers after the client was lost at $at");
			is($rows->('t'), $at eq 'commit' ? '100' : '', "$order: the transaction ended as it would have ($at, a lost client)");
		}
	}

	# ------------------------------------------------------------------
	# the other calls of the access method at a moment where an error is forbidden:
	# a batch, a delete, an update, a row lock, a speculative insert.  Before the
	# end of the transaction they are what they always were (rows, or the error that
	# the table is append-only); at the commit they are refused with a warning.
	# ------------------------------------------------------------------
	foreach my $op ('bulk', 'delete', 'update', 'lock', 'speculative')
	{
		$fresh->();
		my $off = $log_from->();
		my ($out, $err) = $run->('commit', 't', $body{commit}, "SET kcevents.op = '$op'; SET kcevents.count = 50;\n");
		$alive->("$op at XACT_EVENT_COMMIT", $off);
		like($err, qr/WARNING:  (insert|multi insert|delete|update|row lock|speculative insert) refused on columnar table "t": the transaction is not in progress/,
			"$order: $op at XACT_EVENT_COMMIT: refused with a warning");
		is($rows->('t'), '100', "$order: $op at XACT_EVENT_COMMIT: the transaction committed what it had written");
		if ($op =~ /^(delete|update|lock)$/)
		{
			like(slurp_file($node->logfile, $off), qr/kcevents: $op result 1\b/, "$order: $op at XACT_EVENT_COMMIT: the caller is told that nothing matched (TM_Invisible)");
		}

		# before the end of the transaction: the refusal of an append-only table is an error, as before
		$fresh->();
		($out, $err) = $run->('pre_commit', 't', $body{commit}, "SET kcevents.op = '$op'; SET kcevents.count = 50;\n");
		if ($op eq 'bulk')
		{
			is($rows->('t'), join(',', sort { $a <=> $b } (100, 1 .. 50)), "$order: $op at XACT_EVENT_PRE_COMMIT: the batch is written");
		}
		else
		{
			like($err, qr/ERROR:  (columnar tables are append-only|columnar tables do not support row locks|INSERT \.\.\. ON CONFLICT DO UPDATE is not supported)/,
				"$order: $op at XACT_EVENT_PRE_COMMIT: an error, where an error is allowed");
			is($rows->('t'), '', "$order: $op at XACT_EVENT_PRE_COMMIT: ... and the transaction aborted with it");
		}
	}

	# ------------------------------------------------------------------
	# the end of a bulk load (what finishes a COPY) at a moment where an error is forbidden,
	# for a table made in the transaction that has rows pending in a buffer: the flush it
	# would make is a write of stripes and catalog rows, which nothing may do there
	# (with the module's callback before the access method's the buffers still exist)
	# ------------------------------------------------------------------
	foreach my $at ('abort', 'commit', 'abort_sub', 'commit_sub')
	{
		my $sub = $at =~ /_sub$/;
		my $off = $log_from->();
		$q->("DROP TABLE IF EXISTS r");
		my $script = $sub
			? "BEGIN; CREATE TABLE r (a int) USING columnar; SELECT kcevents_open('r'); SAVEPOINT s; INSERT INTO r VALUES (100); " . ($at eq 'abort_sub' ? "ROLLBACK TO s;" : "RELEASE s;") . " COMMIT;"
			: "BEGIN; CREATE TABLE r (a int) USING columnar; SELECT kcevents_open('r'); INSERT INTO r VALUES (100); " . ($at eq 'abort' ? "SELECT 1/0; ROLLBACK;" : "COMMIT;");
		$run->($at, 'r', $script, "SET kcevents.op = 'finish';\n");
		$alive->("the end of a bulk load at $at", $off);
		is($q->("SELECT count(*) FROM pg_class WHERE relname = 'r'"), $at eq 'abort' ? 0 : 1, "$order: the end of a bulk load at $at: the transaction ended as it would have");
		$q->("DROP TABLE IF EXISTS r");
	}

	# ------------------------------------------------------------------
	# a large batch and a table made in the transaction at the commit; the rows the
	# transaction wrote before are kept
	# ------------------------------------------------------------------
	$fresh->();
	my $off = $log_from->();
	my ($out, $err) = $run->('commit', 't', "BEGIN; SELECT kcevents_open('t'); INSERT INTO t SELECT g FROM generate_series(1000, 5999) g; COMMIT;", "SET kcevents.count = 1000;\n");
	$alive->('a batch of 1000 rows at XACT_EVENT_COMMIT after 5000 rows', $off);
	is($q->("SELECT count(*) FROM t"), 5000, "$order: the 5000 rows of the transaction are all there; the late ones were refused");
	$q->("DROP TABLE t");
	($out, $err) = $run->('commit', 'r', "BEGIN; CREATE TABLE r (a int) USING columnar; SELECT kcevents_open('r'); INSERT INTO r VALUES (100); COMMIT;");
	like($err, qr/WARNING:  insert refused on columnar table "r"/, "$order: a table made in the transaction: refused at the commit too");
	is($q->("SELECT count(*) FROM r"), 1, "$order: ... and it has its row");
	$q->("DROP TABLE r");

	# ------------------------------------------------------------------
	# a parallel worker must not write: an error (the state allows one), not rows
	# that the worker buffers and the process then forgets
	# ------------------------------------------------------------------
	$q->("CREATE FUNCTION kcevents_write_now(text, int) RETURNS int AS '$dir/kcevents', 'kcevents_write_now' LANGUAGE C PARALLEL SAFE");
	$fresh->();
	my ($pout, $perr) = ('', '');
	$node->psql('postgres', "SET debug_parallel_query = on; SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0;
SELECT sum(kcevents_write_now('t', g)) FROM generate_series(1, 10) g;", stdout => \$pout, stderr => \$perr, on_error_stop => 0);
	like($perr, qr/cannot insert into columnar table "t" in a parallel worker/, "$order: a write from a parallel worker is an error that names the worker");
	is($rows->('t'), '', "$order: ... and no row is kept");
	is($q->('SELECT pg_postmaster_start_time()'), $started, "$order: ... the server was not restarted");

	# ------------------------------------------------------------------
	# the next session, after all of it: a restart finds everything as it was
	# ------------------------------------------------------------------
	$fresh->();
	$run->('commit', 't', $body{commit});
	$run->('abort_sub', 't', $body{subabort});
	$node->restart;
	is($rows->('t'), '100,100', "$order: after a restart the table has the rows that were committed (one of each script)");
	is($q->("SELECT count(*) FROM kingres.columnar_check()"), 0, "$order: ... and columnar_check is clean");
	$node->stop;
}

done_testing();
