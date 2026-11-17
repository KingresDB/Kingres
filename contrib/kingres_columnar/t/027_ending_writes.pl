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
use lib $FindBin::RealBin;
use KcProbe qw(build_module);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $dir = build_module('kcevents');
plan skip_all => 'the test module (t/kcevents/kcevents.c) could not be built here: it needs a C compiler and the server headers'
  unless defined $dir;

my $warn = qr/WARNING:  (row|rows|delete|update|row lock) not written to columnar table "t": /;

# the scripts: the user writes 100 (and 101 in a savepoint), the module writes 1
my %body = (
	commit => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); COMMIT;",
	prepare => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); PREPARE TRANSACTION 'x';",
	abort => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); SELECT 1/0; ROLLBACK;",
	sub => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); SAVEPOINT s; INSERT INTO %T% VALUES (101); RELEASE s; COMMIT;",
	subabort => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); SAVEPOINT s; INSERT INTO %T% VALUES (101); ROLLBACK TO s; COMMIT;",
	crit => "BEGIN; SELECT kcevents_open('%T%'); INSERT INTO %T% VALUES (100); SELECT kcevents_crit(); COMMIT;",
);

# event => [ script, rows of the columnar table afterwards, rows heap would have (undef: heap stops) ]
my @cases = (
	[ 'pre_commit', 'commit', '1,100', '1,100' ],
	[ 'pre_prepare', 'prepare', '1,100', '1,100' ],
	[ 'start_sub', 'sub', '1,100,101', '1,100,101' ],
	[ 'pre_commit_sub', 'sub', '1,100,101', '1,100,101' ],
	[ 'commit', 'commit', '100', undef ],
	[ 'prepare', 'prepare', '100', undef ],
	[ 'abort', 'abort', '', undef ],
	[ 'commit_sub', 'sub', '100,101', undef ],
	[ 'abort_sub', 'subabort', '100', undef ],
	[ 'res_before_locks', 'commit', '100', undef ],
	[ 'res_locks', 'commit', '100', undef ],
	[ 'res_after_locks', 'commit', '100', undef ],
	[ 'none', 'crit', '100', undef ],
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
			. "SELECT 'PATH=' || coalesce((SELECT hits FROM kingres.columnar_write_paths() WHERE path = 'refused_ending'), -1);",
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
		my ($at, $script, $want, $heap_want) = @$c;
		my $name = $at eq 'none' ? 'a critical section' : $at;

		$fresh->();
		my $off = $log_from->();
		my ($out, $err) = $run->($at, 't', $body{$script});
		my $safe = defined $heap_want;
		$alive->($name, $off);
		is($rows->('t'), $want, "$order: $name: the table has exactly the rows the access method could write ($want)");
		if ($safe)
		{
			unlike($err, qr/WARNING:  row not written/, "$order: $name: the row is written, no warning");
			my ($hout, $herr) = $run->($at, 'h', $body{$script});
			is($rows->('h'), $rows->('t'), "$order: $name: ... and it is what the heap twin has");
		}
		else
		{
			like($err, qr/WARNING:  row not written to columnar table "t": (the transaction is not in progress|inside a critical section)/,
				"$order: $name: refused with a warning, not an error that the server turns into a PANIC");
			like($out, qr/PATH=1\b/, "$order: $name: ... and counted once as refused");
			unlike($err, qr/ERROR:  columnar table "t" cannot be written now/, "$order: $name: ... never the error of the earlier version");
		}
		is($q->("SELECT count(*) FROM kingres.columnar_check()"), 0, "$order: $name: columnar_check has nothing to report");
		$q->("INSERT INTO t VALUES (7)");
		is($q->("SELECT count(*) FROM t WHERE a = 7"), 1, "$order: $name: the table is written again by an ordinary statement");
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
		like($err, qr/WARNING:  (rows?|delete|update|row lock) not written to columnar table "t": the transaction is not in progress/,
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
	like($err, qr/WARNING:  row not written to columnar table "r"/, "$order: a table made in the transaction: refused at the commit too");
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
