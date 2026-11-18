# contrib/kingres_columnar/t/031_entry_points.pl
#
# C8-01.  Every entry point of the extension that PostgreSQL or another module can
# reach, at every moment where an error is forbidden (the commit, prepare and abort
# of a transaction, the commit and abort of a subtransaction, the three phases of the
# release of a resource owner, a critical section) and at the moments where one is
# allowed: what it does, and that the server stays up.
#
# The defect: relation_nontransactional_truncate was not guarded.  Called by another
# module at XACT_EVENT_COMMIT it ended in an ERROR and a PANIC; at the commit of a
# subtransaction the backend looped; on PostgreSQL 19 at the abort and the
# subtransaction events the file was truncated and the stripe rows stayed, so that
# every read failed with "columnar table is corrupt".
#
# The decision for each entry point is in kc_am_decisions (kc_am.c) and in the
# README ("Writes while a transaction is ending", the table of entry points):
#
#   guarded    refused with a WARNING, nothing changed (and, where what the call was
#              to do cannot be skipped, the transaction fails at its next pre-commit
#              if it is still open: "fails" below);
#   reads      works where PostgreSQL lets a heap table be read;
#   nostorage  touches neither the table nor the catalog (nothing to test).
#
# The test module (t/kcevents) calls each entry point at the moment the setting
# kcevents.at names, selected by kcevents.op.  The same file walks the members of
# the routine and fails for one that has no decision recorded, so a callback that is
# added later cannot be forgotten.  Both orders of loading the module and the
# extension run.
#
# What is not run, and why:
#   - vacuum, setnew, copydata and cluster at the moments where an error is allowed: they
#     replace the file of the table or run in a transaction of their own (VACUUM holds
#     its own lock there); the commands that call them
#     (TRUNCATE, ALTER TABLE, VACUUM FULL, SET TABLESPACE) have regression tests,
#     and a module that calls them directly there would only damage the table.
#   - reads (scan) at the moments where an error is forbidden, on a build with
#     assertions: the relation cache of PostgreSQL asserts a transaction in
#     progress, for heap as for columnar.  They are run on a build without.
#   - scan_analyze_next_block and scan_analyze_next_tuple: only ANALYZE reaches them,
#     through a read stream that it makes in a transaction in progress (regression
#     tests columnar_analyze).
#   - the callbacks of the planner and the executor (custom scan), the event
#     triggers and the redo of the resource manager: none of them can run where an
#     error is forbidden (they need a transaction in progress, or recovery).
#
# C9-01.  A critical section entered at XACT_EVENT_PRE_COMMIT or XACT_EVENT_PRE_PREPARE
# by a callback that runs after the extension's own is not "alive": nothing of the
# extension runs again before the commit record, so no error can be raised for the
# refusal, and the transaction commits (prepares) without the call; the warning says so
# ("after this extension's own pre-commit ... callback had run").  With the order
# reversed the same critical section is alive and the transaction fails.  Both cells
# are run for every entry point, in both orders, and the rule that holds for every
# cell is checked in each: the primary message names the moment, and the detail of
# the warning says what is true of that cell (the transaction fails at its next
# pre-commit; it cannot be failed any more; it commits without the call; the call may
# be skipped and nothing is lost).
#
# C9-03.  columnar_check(false), columnar_check(true) and columnar_check_clean() begin a
# subtransaction for each table, which PostgreSQL cannot do where a transaction is
# not in progress, in a critical section, and between the pre-commit and the end of a
# subtransaction: they are refused there with a warning, nothing is checked, and the
# session lives (without the refusal it ends with a FATAL, and at the abort of a
# subtransaction leaves a lock behind).  They work at XACT_EVENT_PRE_COMMIT and
# PRE_PREPARE.  The SQL functions that read are called through the function manager at
# the same moments on a build without assertions (the relation cache asserts a
# transaction in progress, for heap as for columnar), and a table that is locked
# exclusively by another session is not waited for there (a wait that ends in a
# deadlock error is a PANIC where an error is forbidden).  Not run, and why: the
# start of a subtransaction (SUBXACT_EVENT_START_SUB) and, in the order "before", its
# pre-commit: the extension cannot tell that it is there, and PostgreSQL's own FATAL
# for a subtransaction begun there applies (heap's exception blocks end the session
# the same way).
#
# C10-03.  A critical section inside a subtransaction that a callback of a later module began, after
# the extension's own pre-commit, is alive (the extension's callbacks for the subtransaction run, its
# pre-commit raises): the cells "in a critical section inside a subtransaction begun at pre_commit /
# pre_prepare" (module setting kcevents.sub), for insert, truncate and vacuum, in both orders.
#
# C10-05.  relation_nontransactional_truncate is carried out only for a table made, or given a new
# file, in the transaction (the cells with TRUNCATE and with DROP/CREATE in the script); for any other
# table it is an error where an error is allowed (the loop over the moments, and the review's
# sequence with 30000 rows), and the usual warning where it is not.
#
# C10-07.  What is not run on a build with assertions is counted and named at the end of the output
# of each order of loading (diag), and the README lists it cell by cell (the reads at the moments where
# PostgreSQL's own assertions stop the server for heap as well); those cells run on a build without
# assertions.
#
# It needs a compiler and the server's headers (KcProbe.pm).
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

# a case that fails because the code under test is wrong must not hang the file on a lock that is never released
$ENV{PGOPTIONS} = '-c statement_timeout=240s';

my $dir = build_module('kcevents');

# the cells that are not run on a build with assertions (C10-07), counted for each order of loading and
# named at the end of the run
my %skipped;

# the moments: name (the setting), script, rows afterwards for a call that does not
# fail the transaction, rows for one that does, kind
#   forbidden: an error is forbidden there; live: ... and the top-level transaction is
#   still alive (the call is remembered and the transaction fails at its pre-commit);
#   allowed: an error is allowed
my %script = (
	commit => "BEGIN; SELECT kcevents_open('t'); %F% INSERT INTO t VALUES (100); COMMIT;",
	prepare => "BEGIN; SELECT kcevents_open('t'); %F% INSERT INTO t VALUES (100); PREPARE TRANSACTION 'x';",
	abort => "BEGIN; SELECT kcevents_open('t'); %F% INSERT INTO t VALUES (100); SELECT 1/0; ROLLBACK;",
	sub => "BEGIN; SELECT kcevents_open('t'); %F% INSERT INTO t VALUES (100); SAVEPOINT s; INSERT INTO t VALUES (101); RELEASE s; COMMIT;",
	subabort => "BEGIN; SELECT kcevents_open('t'); %F% INSERT INTO t VALUES (100); SAVEPOINT s; INSERT INTO t VALUES (101); ROLLBACK TO s; COMMIT;",
	crit => "BEGIN; SELECT kcevents_open('t'); %F% INSERT INTO t VALUES (100); SELECT kcevents_crit(); COMMIT;",
);
my $base = '1,2,3,4,5';
my @moments = (
	# at, script, kind, rows if the call does not fail the transaction, rows if it does
	[ 'commit', 'commit', 'decided', "$base,100", "$base,100" ],
	[ 'prepare', 'prepare', 'decided', "$base,100", "$base,100" ],
	[ 'abort', 'abort', 'decided', $base, $base ],
	[ 'abort_sub', 'subabort', 'decided', "$base,100", "$base,100" ],
	[ 'res_before_locks', 'commit', 'decided', "$base,100", "$base,100" ],
	[ 'res_locks', 'commit', 'decided', "$base,100", "$base,100" ],
	[ 'res_after_locks', 'commit', 'decided', "$base,100", "$base,100" ],
	[ 'commit_sub', 'sub', 'live', "$base,100,101", $base ],
	[ 'none', 'crit', 'live', "$base,100", $base ],
	# C9-01: a critical section entered at the pre-commit (pre-prepare) event of the top-level
	# transaction by the module: after the extension's own callback (the order "after") the refusal
	# is final, before it (the order "before") the transaction is alive and fails there
	[ 'pre_commit', 'commit', 'precrit', "$base,100", $base, "SET kcevents.crit = on;\n", 'a critical section at pre_commit' ],
	[ 'pre_prepare', 'prepare', 'precrit', "$base,100", $base, "SET kcevents.crit = on;\n", 'a critical section at pre_prepare' ],
);

# the operations: op => [ warning text, fails the transaction when refused while it is alive,
#   how many "done" lines, error text where an error is allowed (undef: it works there),
#   kind: g guarded, r reads, s reads only the file, nothing else ]
my %ops = (
	# C10-05: on a table that existed before the transaction, where an error is allowed, an error (the
	# table of this file is always an old one there; the table made in the transaction is below)
	ntrunc => [ 'truncate', 1, 1, 'cannot truncate columnar table "t" in place: it was not created or given a new file in this transaction', 'g' ],
	setnew => [ 'new file', 1, 1, undef, 'g', 'noallowed' ],
	copydata => [ 'file copy', 1, 1, undef, 'g', 'noallowed' ],
	cluster => [ 'rewrite', 1, 1, undef, 'g', 'noallowed' ],
	vacuum => [ 'vacuum', 0, 1, undef, 'g', 'noallowed' ],
	idxfetch => [ 'index fetch', 1, 1, 'columnar tables have no indexes in this version', 'g' ],
	fetchrow => [ 'fetch by TID', 1, 1, 'columnar tables do not support fetching a row by its TID', 'g' ],
	tidvalid => [ 'TID check', 1, 1, 'columnar tables do not support fetching a row by its TID', 'g' ],
	idxdel => [ 'index delete', 1, 1, 'columnar tables have no indexes in this version', 'g' ],
	idxbuild => [ 'index build', 1, 1, 'indexes on columnar tables are not supported', 'g' ],
	idxvalidate => [ 'index validation', 1, 1, 'indexes on columnar tables are not supported', 'g' ],
	sample => [ 'sampling', 1, 1, 'TABLESAMPLE is not supported on columnar tables', 'g' ],
	objhook => [ 'drop note', 0, 1, undef, 'g' ],
	fn_write => [ 'columnar_(compact|set_options)', 1, 2, undef, 'g' ],
	fn_safe => [ 'columnar_(reconcile|finish_rewrites|note_rewrite)', 0, 3, undef, 'g' ],
	scan => [ undef, 0, 1, undef, 'r' ],
	size => [ undef, 0, 1, undef, 's' ],
	estimate => [ undef, 0, 1, undef, 's' ],
);

foreach my $order ('after', 'before')
{
	# "after": the module's callbacks run after the access method's
	my $libs = $order eq 'after' ? "$dir/kcevents,kingres_columnar" : "kingres_columnar,$dir/kcevents";
	my $node = PostgreSQL::Test::Cluster->new("colentry_$order");
	$node->init;
	$node->append_conf('postgresql.conf', "shared_preload_libraries = '$libs'\nmax_prepared_transactions = 10\n");
	$node->start;
	$node->safe_psql('postgres', qq{
CREATE EXTENSION kingres_columnar;
CREATE FUNCTION kcevents_open(text) RETURNS void AS '$dir/kcevents', 'kcevents_open' LANGUAGE C;
CREATE FUNCTION kcevents_crit() RETURNS void AS '$dir/kcevents', 'kcevents_crit' LANGUAGE C;
CREATE FUNCTION kcevents_fn(text, int) RETURNS void AS '$dir/kcevents', 'kcevents_fn' LANGUAGE C;
CREATE FUNCTION kcevents_sql(oid, int, text DEFAULT NULL) RETURNS void AS '$dir/kcevents', 'kcevents_sql' LANGUAGE C;
CREATE FUNCTION kcevents_walk(text) RETURNS TABLE (off bigint, member text, decision text, why text)
	AS '$dir/kcevents', 'kcevents_walk' LANGUAGE C;
CREATE FUNCTION kingres.columnar_note_rewrite(rel regclass) RETURNS void
	AS '\$libdir/kingres_columnar', 'kingres_columnar_note_rewrite' LANGUAGE C STRICT SECURITY DEFINER
	SET search_path = pg_catalog, kingres, pg_temp;});
	my $started = $node->safe_psql('postgres', 'SELECT pg_postmaster_start_time()');
	my $assertions = $node->safe_psql('postgres', 'SHOW debug_assertions') eq 'on';
	my $dboid = $node->safe_psql('postgres', "SELECT oid FROM pg_database WHERE datname = 'postgres'");

	my $q = sub { return $node->safe_psql('postgres', $_[0]); };
	my $fresh = sub
	{
		$q->("DROP TABLE IF EXISTS t, h;
			CREATE TABLE t (a int) USING columnar;
			INSERT INTO t SELECT generate_series(1, 5);
			ANALYZE t;");
	};
	my $rows = sub { return $q->("SELECT coalesce(string_agg(a::text, ',' ORDER BY a), '') FROM t"); };
	my $log_from = sub { return -s $node->logfile; };
	my $fnlist = {
		fn_write => "SELECT kcevents_fn('kingres_columnar_compact', 2); SELECT kcevents_fn('kingres_columnar_set_options', 5);",
		sqld_check_false => "SELECT kcevents_sql('kingres.columnar_check(boolean)'::regprocedure::oid, 0, 'kingres_columnar_check');",
		sqld_check_true => "SELECT kcevents_sql('kingres.columnar_check(boolean)'::regprocedure::oid, 1, 'kingres_columnar_check');",
		sqld_check_clean => "SELECT kcevents_sql('kingres.columnar_check_clean()'::regprocedure::oid, 0, 'kingres_columnar_check_clean');",
		sql_check_false => "SELECT kcevents_sql('kingres.columnar_check(boolean)'::regprocedure::oid, 0);",
		sql_check_true => "SELECT kcevents_sql('kingres.columnar_check(boolean)'::regprocedure::oid, 1);",
		sql_check_clean => "SELECT kcevents_sql('kingres.columnar_check_clean()'::regprocedure::oid, 0);",
		sql_options => "SELECT kcevents_sql('kingres.columnar_options(regclass)'::regprocedure::oid, -1);",
		sql_info => "SELECT kcevents_sql('kingres.columnar_info(regclass)'::regprocedure::oid, -1);",
		sql_stripes => "SELECT kcevents_sql('kingres.columnar_stripes(regclass)'::regprocedure::oid, -1);",
		sql_identity => "SELECT kcevents_sql('kingres.columnar_identity(regclass)'::regprocedure::oid, -1);",
		sql_creating_xid => "SELECT kcevents_sql('kingres.columnar_creating_xid(regclass)'::regprocedure::oid, -1);",
		sql_table_xid => "SELECT kcevents_sql('kingres.columnar_table_xid(regclass)'::regprocedure::oid, -1);",
		sql_write_paths => "SELECT kcevents_sql('kingres.columnar_write_paths(boolean)'::regprocedure::oid, 0);",
		fn_safe => "SELECT kcevents_fn('kingres_columnar_reconcile', 0); SELECT kcevents_fn('kingres_columnar_finish_rewrites', 0); SELECT kcevents_fn('kingres_columnar_note_rewrite', 1);",
	};
	# run a script at a moment with an operation; returns (stdout, stderr, log text)
	my $run = sub
	{
		my ($at, $script, $op, $extra) = @_;
		my $realop = $op =~ /^fn_/ ? 'fn' : $op =~ /^sqld?_/ ? 'sqlfn' : $op;
		my $reg = $op =~ /^(?:fn|sqld?)_/ ? $fnlist->{$op} : '';
		(my $s = $script) =~ s/%F%/$reg/;
		my ($out, $err) = ('', '');
		my $off = $log_from->();
		$node->psql('postgres', "SET kcevents.at = '$at'; SET kcevents.op = '$realop';\n" . ($extra // '') . "$s\nRESET kcevents.at;\nRESET kcevents.op;",
			stdout => \$out, stderr => \$err, on_error_stop => 0);
		$node->psql('postgres', "COMMIT PREPARED 'x'", stdout => \my $o2, stderr => \my $e2) if $s =~ /PREPARE TRANSACTION 'x'/;
		return ($out, $err, slurp_file($node->logfile, $off));
	};
	my $alive = sub
	{
		my ($name, $log) = @_;
		is($q->('SELECT pg_postmaster_start_time()'), $started, "$order: $name: the server was not restarted");
		unlike($log, qr/PANIC|TRAP:|terminated by signal/, "$order: $name: no PANIC, no assertion failure, no crash in the log");
	};

	# ------------------------------------------------------------------
	# every callback of the routine has a decision (the walk of the routine's members)
	# ------------------------------------------------------------------
	{
		$fresh->();
		is($q->("SELECT count(*) FROM kcevents_walk('t') WHERE decision IS NULL"), 0,
			"$order: every callback of the columnar routine has a decision recorded");
		cmp_ok($q->("SELECT count(*) FROM kcevents_walk('t')"), '>=', 38, "$order: ... the walk saw the whole routine");
		is($q->("SELECT count(*) FROM kcevents_walk('t') WHERE decision NOT IN ('guarded', 'reads', 'nostorage')"), 0,
			"$order: ... and each decision is one of guarded, reads, nostorage");
		is($q->("SELECT string_agg(member, ',' ORDER BY member) FROM kcevents_walk('t') WHERE member IN ('relation_nontransactional_truncate', 'relation_set_new_filelocator', 'relation_copy_data', 'relation_copy_for_cluster', 'relation_vacuum') AND decision = 'guarded'"),
			'relation_copy_data,relation_copy_for_cluster,relation_nontransactional_truncate,relation_set_new_filelocator,relation_vacuum',
			"$order: ... the five that change the file or the catalog are all guarded");
		$q->("CREATE TABLE h (a int)");
		cmp_ok($q->("SELECT count(*) FROM kcevents_walk('h') WHERE decision IS NULL"), '>', 0,
			"$order: the walk does report a routine whose callbacks have no decision (the heap one)");
	}

	# ------------------------------------------------------------------
	# columnar_note_rewrite (kept for databases made by an earlier script) opens the
	# table with a lock of its own: behind an exclusive lock it waits (here: a lock
	# timeout), as every statement on the table does.  Without the lock it would run
	# on a table that a rewrite is replacing, and only a build with assertions would
	# notice.
	# ------------------------------------------------------------------
	{
		$fresh->();
		my $b = $node->background_psql('postgres', on_error_stop => 1);
		$b->query_safe("BEGIN; LOCK TABLE t IN ACCESS EXCLUSIVE MODE");
		my ($out, $err) = ('', '');
		$node->psql('postgres', "SET lock_timeout = '300ms'; SELECT kingres.columnar_note_rewrite('t')", stdout => \$out, stderr => \$err, on_error_stop => 0);
		like($err, qr/canceling statement due to lock timeout/, "$order: columnar_note_rewrite waits behind an exclusive lock on the table (it takes a lock of its own)");
		$b->query_safe("ROLLBACK");
		$b->quit;
		($out, $err) = ('', '');
		$node->psql('postgres', "SET lock_timeout = '300ms'; SELECT kingres.columnar_note_rewrite('t')", stdout => \$out, stderr => \$err, on_error_stop => 0);
		is($err, '', "$order: ... and runs when the lock is free");
		is($rows->(), $base, "$order: ... and deletes nothing of a file the table still has");
	}

	# ------------------------------------------------------------------
	# the moments where an error is forbidden
	# ------------------------------------------------------------------
	foreach my $op (sort keys %ops)
	{
		my ($what, $fails, $ndone, $err_allowed, $kind) = @{ $ops{$op} };

		foreach my $m (@moments)
		{
			my ($at, $script, $mkind, $want_ok, $want_fail, $extra, $label) = @$m;
			my $late = 0;
			if ($mkind eq 'precrit')
			{
				$late = $order eq 'after';
				$mkind = $late ? 'decided' : 'live';
			}
			my $crit = $at eq 'none' || defined $extra;
			my $name = "$op at " . ($label // ($at eq 'none' ? 'a critical section' : $at));

			# reads that allocate and open relations: not on a build with assertions (see above);
			# at a release phase they need the buffer pin that PostgreSQL itself forbids there
			# (size and estimate read the file's length: they allocate in PostgreSQL's smgr, which a
			# critical section forbids, again PostgreSQL's rule)
			if (($kind eq 'r' && ($assertions || $at =~ /^res_/)) || ($kind eq 's' && $assertions && $crit))
			{
				SKIP:
				{
					$skipped{$order}++;
					skip "$order: $name: a read at a moment where an error is forbidden is PostgreSQL's own rule (assertions in the relation cache, the resource owner)", 1;
					ok(1);
				}
				next;
			}
			# size and estimate read the file's length only; at a release phase the smgr is fine too

			$fresh->();
			my ($out, $err, $log) = $run->($at, $script{$script}, $op, $extra);
			$alive->($name, $log);
			my $survives = !$fails || $mkind eq 'decided';
			is($rows->(), $survives ? $want_ok : $want_fail,
				"$order: $name: the table has exactly the rows that the transaction could write"
				. ($survives ? '' : ' (it failed at its pre-commit, as a transaction that lost a call must)'));
			my $ndone_seen = () = $log =~ /kcevents: (?:$op|fn \d+|\w+) done/g;
			if ($kind eq 'g')
			{
				cmp_ok(scalar(() = $err =~ /WARNING:  $what refused(?: on columnar table "t")?: /g), '>=', 1,
					"$order: $name: refused with a warning that names it");
				# the rule of the round: the text is true of the cell it is given in (C9-01, C9-05).  The
				# primary message names the moment; the detail says what happens to the transaction.
				my $want_primary = $crit ? qr/inside a critical section/
				  : $at eq 'commit_sub' ? qr/at the commit of a subtransaction/
				  : qr/the transaction is not in progress/;
				like($err, qr/WARNING:  $what refused(?: on columnar table "t")?: $want_primary/,
					"$order: $name: the message names the moment");
				my $want_detail = !$fails ? qr/skipping this call loses nothing of the transaction/
				  : $mkind eq 'live' ? qr/the transaction is still open and fails with an error at its next pre-commit/
				  : $late ? qr/after this extension's own pre-commit \(or pre-prepare\) callback had run.*if the transaction commits \(or prepares\) it does so without this call/s
				  : qr/the transaction cannot be failed for it any more: if it commits or prepares it does so without this call/;
				like($err, $want_detail, "$order: $name: the detail says what is true of this cell");
				unlike($err, qr/fails with an error at its next pre-commit/, "$order: $name: ... and promises no error that cannot come")
				  unless $mkind eq 'live' && $fails;
				my $done = $op =~ /^fn_/ ? scalar(() = $log =~ /kcevents: fn \d+ done/g) : scalar(() = $log =~ /kcevents: $op done/g);
				is($done, $ndone, "$order: $name: the call returned ($ndone) and raised nothing");
				if ($fails && $mkind eq 'live')
				{
					like($err, qr/ERROR:  a call (?:on columnar table "t"|to \w+) was refused earlier in this transaction, so it cannot commit/,
						"$order: $name: the transaction that is still alive fails with an error at its pre-commit");
					like($err, qr/DETAIL:  Code of another module \(not a statement of this session\) called the table at a moment where the server forbids an error/,
						"$order: $name: ... and the error says that code of another module did it");
				}
				else
				{
					unlike($err, qr/ERROR:/, "$order: $name: no error: the fate of the transaction was settled" . ($fails ? '' : ' (and nothing was lost)'))
					  unless $at eq 'abort';
				}
			}
			else
			{
				unlike($err, qr/refused on columnar|ERROR:/, "$order: $name: works, no warning" . ($at eq 'abort' ? '' : ', no error'))
				  unless $at eq 'abort';
				my $done = scalar(() = $log =~ /kcevents: $op done/g);
				is($done, 1, "$order: $name: the call returned");
			}
			is($q->("SELECT count(*) FROM kingres.columnar_check()"), 0, "$order: $name: columnar_check has nothing to report");
			if ($op eq 'setnew' || $op eq 'copydata')
			{
				$log =~ /newfile=(\d+)/;
				my $f = $1;
				ok(defined $f && !-e $node->data_dir . "/base/$dboid/$f", "$order: $name: no file was made for the new locator");
			}
		}
	}

	# ------------------------------------------------------------------
	# the moments where an error is allowed
	# ------------------------------------------------------------------
	foreach my $op (sort keys %ops)
	{
		my ($what, $fails, $ndone, $err_allowed, $kind) = @{ $ops{$op} };
		next if $ops{$op}[5];		# setnew, copydata, cluster: see the header
		foreach my $m (['pre_commit', 'commit', 'rows'], ['pre_prepare', 'prepare', 'rows'], ['start_sub', 'sub', 'rows'], ['pre_commit_sub', 'sub', 'rows'])
		{
			my ($at, $script) = @$m;
			my $name = "$op at $at";

			# a truncate of an old table is an error everywhere an error is allowed (C10-05); the
			# cells of a table made in the transaction, with the error between the two phases of the
			# hand-over of a subtransaction, are in the block "a truncate that is carried out" below
			my $err_here = $err_allowed;

			$fresh->();
			my ($out, $err, $log) = $run->($at, $script{$script}, $op);
			$alive->($name, $log);
			if (defined $err_here)
			{
				like($err, qr/ERROR:  \Q$err_here\E/, "$order: $name: an error, where an error is allowed");
				is($rows->(), $base, "$order: $name: ... the transaction aborted with it and the table has what it had");
				unlike($err, qr/refused on columnar/, "$order: $name: ... and no refusal warning");
			}
			else
			{
				unlike($err, qr/ERROR:|refused on columnar/, "$order: $name: carried out: no error and no refusal");
				cmp_ok(scalar(() = $log =~ /kcevents: (?:$op|fn \d+) done/g), '>=', 1, "$order: $name: the call returned");
				is($q->("SELECT count(*) FROM t") =~ /^\d+$/ ? 1 : 0, 1, "$order: $name: the table is readable afterwards");
			}
			is($q->("SELECT count(*) FROM kingres.columnar_check()"), 0, "$order: $name: columnar_check has nothing to report");
		}
	}

	# ------------------------------------------------------------------
	# a truncate that is carried out where it may be (C8-01, the allowed side): only for a table
	# that was made, or given a new file, in this transaction (C10-05: the only case PostgreSQL
	# itself calls it for, and the only one where an abort removes the file anyway).  The table
	# is empty and readable, and a later write works.
	# ------------------------------------------------------------------
	{
		# a table given a new file by TRUNCATE in this transaction, and one made in it
		my %mk = (
			truncate => "TRUNCATE t; INSERT INTO t SELECT generate_series(1, 5);",
			created => "DROP TABLE t; CREATE TABLE t (a int) USING columnar; INSERT INTO t SELECT generate_series(1, 5);",
		);
		foreach my $how ('truncate', 'created')
		{
			$fresh->();
			my ($out, $err, $log) = $run->('pre_commit', "BEGIN; $mk{$how} SELECT kcevents_open('t'); COMMIT;", 'ntrunc');
			unlike($err, qr/ERROR:|refused on columnar/, "$order: relation_nontransactional_truncate at pre-commit on a table ($how) of this transaction: carried out");
			is($rows->(), '', "$order: ... the table is empty");
			is($q->("SELECT count(*) FROM kingres.columnar_check()"), 0, "$order: ... and the file and the stripe rows agree");
			$q->("INSERT INTO t VALUES (7)");
			is($rows->(), '7', "$order: ... and it is written again");
			is($q->("SELECT rows FROM kingres.columnar_info('t')"), 1, "$order: ... the catalog counts the new row only");
		}

		# the other moments where an error is allowed, on such a table: carried out as before
		# (between the two phases of the commit of a subtransaction, after the extension's callback,
		# the error that has always been there)
		foreach my $m (['pre_prepare', 'prepare'], ['start_sub', 'sub'], ['pre_commit_sub', 'sub'])
		{
			my ($at, $script) = @$m;
			(my $s2 = $script{$script}) =~ s/^BEGIN; /BEGIN; $mk{created} /;
			$fresh->();
			my ($out, $err, $log) = $run->($at, $s2, 'ntrunc');
			$alive->("ntrunc of a table made in the transaction at $at", $log);
			if ($at eq 'pre_commit_sub' && $order eq 'after')
			{
				like($err, qr/ERROR:  columnar table cannot be truncated while a subtransaction is being committed/,
					"$order: ntrunc of a table made in the transaction at $at: the error of the first phase of the hand-over");
			}
			else
			{
				unlike($err, qr/ERROR:|refused on columnar/, "$order: ntrunc of a table made in the transaction at $at: carried out");
				cmp_ok(scalar(() = $log =~ /kcevents: ntrunc done/g), '>=', 1, "$order: ... and returned");
				is($q->("SELECT count(*) FROM t") =~ /^\d+$/ ? 1 : 0, 1, "$order: ... and the table is readable");
			}
			is($q->("SELECT kingres.columnar_check_clean()"), 't', "$order: ntrunc of a table made in the transaction at $at: the check is clean");
		}

		# C10-05: the review's sequence.  A table that existed (30000 rows, committed earlier), a module
		# that empties it in place at the pre-commit, and a transaction that then fails (here the
		# refusal itself is the error; the sequence in the review had another module's error after it).
		# Before the fix the abort brought the catalog rows back while the file was cut and the table
		# could not be read.
		$fresh->();
		$q->("INSERT INTO t SELECT generate_series(1000, 30999)");
		my $stripes = $q->("SELECT count(*) FROM kingres.columnar_stripes('t')");
		my $count = $q->("SELECT count(*) FROM t");
		my ($out, $err, $log) = $run->('pre_commit', "BEGIN; SELECT kcevents_open('t'); COMMIT;", 'ntrunc');
		like($err, qr/ERROR:  cannot truncate columnar table "t" in place: it was not created or given a new file in this transaction/,
			"$order: ntrunc of a table that existed before the transaction: an error (C10-05)");
		is($q->("SELECT count(*) FROM t"), $count, "$order: ... the table reads, with all its rows (it is not destroyed outside the transaction)");
		is($q->("SELECT count(*) FROM kingres.columnar_stripes('t')"), $stripes, "$order: ... and has its stripes");
		is($q->("SELECT kingres.columnar_check_clean()"), 't', "$order: ... and the check is clean (no stripe row without its data)");
		# the same where an error is forbidden: refused with a warning, nothing changed
		($out, $err, $log) = $run->('commit', $script{commit}, 'ntrunc');
		like($err, qr/WARNING:  truncate refused on columnar table "t"/, "$order: ... where an error is forbidden: the warning of every refusal");
		is($q->("SELECT count(*) FROM t"), $count + 1, "$order: ... and nothing else changed (the row of the script, committed)");
	}

	# ------------------------------------------------------------------
	# C10-03: a critical section after the extension's own pre-commit, inside a subtransaction that
	# a later module began.  The callbacks of the extension for the subtransaction do run, so the
	# refusal is alive: remembered for the subtransaction, and its pre-commit raises, as for any
	# subtransaction (the top-level cell, a critical section at the pre-commit itself, is decided and
	# commits without the call: moments above).  A call that may be skipped (vacuum) is not made worse
	# by the subtransaction: warning, and the transaction commits.
	# ------------------------------------------------------------------
	foreach my $op ('insert', 'ntrunc', 'vacuum')
	{
		foreach my $m (['pre_commit', 'commit'], ['pre_prepare', 'prepare'])
		{
			my ($at, $script) = @$m;
			my $name = "$op in a critical section inside a subtransaction begun at $at";
			my $what = $op eq 'insert' ? 'insert' : $op eq 'ntrunc' ? 'truncate' : 'vacuum';

			$fresh->();
			my ($out, $err, $log) = $run->($at, $script{$script}, $op, "SET kcevents.crit = on;\nSET kcevents.sub = on;\n");
			$alive->($name, $log);
			like($err, qr/WARNING:  $what refused on columnar table "t": inside a critical section/, "$order: $name: refused with a warning");
			if ($op eq 'vacuum')
			{
				like($err, qr/skipping this call loses nothing of the transaction/, "$order: $name: a call that may be skipped says so");
				unlike($err, qr/ERROR:/, "$order: $name: no error");
				is($rows->(), "$base,100", "$order: $name: the transaction commits");
			}
			else
			{
				like($err, qr/the transaction is still open and fails with an error at its next pre-commit/,
					"$order: $name: the detail says that the transaction is alive");
				unlike($err, qr/Nothing of this extension runs again|it does so without this call/, "$order: $name: ... and not that it commits without the call");
				like($err, qr/ERROR:  a call on columnar table "t" was refused earlier in this transaction, so it cannot commit/,
					"$order: $name: the pre-commit of the subtransaction raises");
				is($rows->(), $base, "$order: $name: the transaction failed and the table has what it had");
			}
			is($q->("SELECT count(*) FROM pg_prepared_xacts"), 0, "$order: $name: no prepared transaction is left");
			is($q->("SELECT kingres.columnar_check_clean()"), 't', "$order: $name: the check is clean");
		}
	}
	# the transaction after it is normal (the state of the refusal went with the aborted subtransaction)
	{
		$fresh->();
		$q->("INSERT INTO t VALUES (200)");
		is($rows->(), "$base,200", "$order: the next transaction of a session that had such a failure writes and commits");
	}

	# ------------------------------------------------------------------
	# a refusal inside a critical section does not allocate (a build with assertions
	# traps an allocation there): the same entry points at the moments where an
	# error is forbidden, inside a critical section of the module's own
	# ------------------------------------------------------------------
	foreach my $op ('ntrunc', 'idxfetch', 'vacuum', 'fn_write', 'objhook', 'tidvalid')
	{
		foreach my $at ('commit', 'abort', 'abort_sub', 'commit_sub')
		{
			my $script = $at eq 'commit' ? 'commit' : $at eq 'abort' ? 'abort' : $at eq 'abort_sub' ? 'subabort' : 'sub';
			$fresh->();
			my ($out, $err, $log) = $run->($at, $script{$script}, $op, "SET kcevents.crit = on;\n");
			$alive->("$op at $at inside a critical section", $log);
			like($err, qr/WARNING:  \S.* refused(?: on columnar table "t")?: /, "$order: $op at $at inside a critical section: refused, allocating nothing");
		}
	}

	# ------------------------------------------------------------------
	# C9-03: the SQL functions called through the function manager, as the executor calls
	# them (a result-set context, an active snapshot), at the moments where PostgreSQL
	# cannot begin a subtransaction.  columnar_check(false), columnar_check(true) and
	# columnar_check_clean() are refused with a warning and the session lives; without
	# the refusal PostgreSQL ends the session with a FATAL (and at the abort of a
	# subtransaction leaves a lock that nothing releases)
	# ------------------------------------------------------------------
	{
		my @cells = (
			# name, at, script, rows afterwards, why it is refused, extra
			[ 'commit', 'commit', 'commit', "$base,100", 'not' ],
			[ 'prepare', 'prepare', 'prepare', "$base,100", 'not' ],
			[ 'abort', 'abort', 'abort', $base, 'not' ],
			[ 'abort_sub', 'abort_sub', 'subabort', "$base,100", 'not' ],
			[ 'res_before_locks', 'res_before_locks', 'commit', "$base,100", 'not' ],
			[ 'res_locks', 'res_locks', 'commit', "$base,100", 'not' ],
			[ 'res_after_locks', 'res_after_locks', 'commit', "$base,100", 'not' ],
			[ 'commit_sub', 'commit_sub', 'sub', "$base,100,101", 'not' ],
			[ 'a critical section', 'none', 'crit', "$base,100", 'crit' ],
			[ 'a critical section at pre_commit', 'pre_commit', 'commit', "$base,100", 'crit', "SET kcevents.crit = on;\n" ],
			[ 'a critical section at pre_prepare', 'pre_prepare', 'prepare', "$base,100", 'crit', "SET kcevents.crit = on;\n" ],
		);
		# the pre-commit of a subtransaction: the extension knows it only when its callback ran first
		push @cells, [ 'pre_commit_sub', 'pre_commit_sub', 'sub', "$base,100,101", 'subcommitting' ] if $order eq 'after';
		my %fname = (sqld_check_false => 'columnar_check', sqld_check_true => 'columnar_check', sqld_check_clean => 'columnar_check_clean', sql_check_false => 'columnar_check', sql_check_true => 'columnar_check', sql_check_clean => 'columnar_check_clean');
		my $callee = sub { my ($op) = @_; return $op =~ /check_false$/ ? 'columnar_check\(false\)' : $op =~ /check_true$/ ? 'columnar_check\(true\)' : 'columnar_check_clean'; };
		# called by the address of the C function, the way a C module calls it (see the header of
		# kcevents.c: the function manager's wrapper reads the catalogs, which a build with
		# assertions refuses where no transaction is in progress)

		foreach my $op ('sqld_check_false', 'sqld_check_true', 'sqld_check_clean')
		{
			foreach my $c (@cells)
			{
				my ($label, $at, $script, $want, $why, $extra) = @$c;
				my $name = "$op at $label";
				my $text = $why eq 'crit' ? 'inside a critical section' : $why eq 'subcommitting' ? 'the subtransaction is committing' : 'the transaction is not in progress';

				$fresh->();
				my ($out, $err, $log) = $run->($at, $script{$script}, $op, $extra);
				$alive->($name, $log);
				unlike($log, qr/FATAL/, "$order: $name: no FATAL: the session was not ended");
				my $cre = $callee->($op);
				like($err, qr/WARNING:  $cre refused: $text/, "$order: $name: refused with a warning that says why");
				like($err, qr/Nothing was checked, and the empty answer of a call that was refused is not an answer that the tables are clean/,
					"$order: $name: ... and that the empty answer is not a clean bill");
				unlike($err, qr/ERROR:/, "$order: $name: no error") unless $at eq 'abort';
				like($log, qr/kcevents: sqlfn $fname{$op} done rows=0 isnull=0 bool=0/, "$order: $name: the call returned, with no rows (false for the strict form)");
				is($rows->(), $want, "$order: $name: the transaction ended as it would have (a check writes nothing)");
				is($q->("SELECT count(*) FROM pg_locks WHERE locktype = 'relation' AND relation = 't'::regclass AND pid IS NULL"), 0,
					"$order: $name: no lock was left behind");
				is($q->("SELECT kingres.columnar_check_clean()"), 't', "$order: $name: the tables are clean afterwards");
			}
		}

		# where PostgreSQL lets a subtransaction begin (the pre-commit and the pre-prepare of the top-level
		# transaction) the functions work
		foreach my $op ('sql_check_false', 'sql_check_true', 'sql_check_clean')
		{
			foreach my $m (['pre_commit', 'commit'], ['pre_prepare', 'prepare'])
			{
				my ($at, $script) = @$m;
				my $name = "$op at $at";
				$fresh->();
				my ($out, $err, $log) = $run->($at, $script{$script}, $op);
				$alive->($name, $log);
				unlike($log, qr/FATAL/, "$order: $name: no FATAL");
				unlike($err, qr/refused|ERROR:/, "$order: $name: works: no refusal, no error");
				like($log, qr/kcevents: sqlfn $fname{$op} done rows=0 isnull=0 bool=@{[ $op eq 'sql_check_clean' ? 1 : 0 ]}/, "$order: $name: it ran, and found the tables clean");
				is($rows->(), "$base,100", "$order: $name: the transaction committed what it wrote");
			}
		}

		# the other SQL functions, which read: through the function manager at the moments where an
		# error is forbidden.  The relation cache of a build with assertions asserts a transaction in
		# progress there (heap's too), so only a build without assertions runs them; each returns and
		# changes nothing
		foreach my $op ('sql_options', 'sql_info', 'sql_stripes', 'sql_identity', 'sql_creating_xid', 'sql_table_xid', 'sql_write_paths')
		{
			foreach my $c (@cells)
			{
				my ($label, $at, $script, $want, $why, $extra) = @$c;
				next if $at =~ /^res_/ || $at eq 'pre_commit_sub';
				my $name = "$op at $label";
				if ($assertions)
				{
					SKIP:
					{
						$skipped{$order}++;
						skip "$order: $name: a read at a moment where an error is forbidden is PostgreSQL's own rule on a build with assertions", 1;
						ok(1);
					}
					next;
				}
				$fresh->();
				my ($out, $err, $log) = $run->($at, $script{$script}, $op, $extra);
				$alive->($name, $log);
				unlike($log, qr/FATAL/, "$order: $name: no FATAL");
				like($log, qr/kcevents: sqlfn \w+ done/, "$order: $name: the call returned");
				unlike($err, qr/ERROR:/, "$order: $name: no error") unless $at eq 'abort';
				is($rows->(), $want, "$order: $name: the table has what the transaction wrote");
				is($q->("SELECT kingres.columnar_check_clean()"), 't', "$order: $name: the tables are clean afterwards");
			}
		}

		# a table that another session holds exclusively is not waited for where an error is forbidden
		# (a wait could end in a deadlock error, a PANIC there): a warning and a null answer.  A wait
		# cannot be cancelled there (the commit holds interrupts), so a call that waits would hang this
		# file: the client is given 30 seconds, and the lock is then released so that the backend goes on.
		{
			$fresh->();
			$q->("DROP TABLE IF EXISTS u; CREATE TABLE u (a int) USING columnar; INSERT INTO u VALUES (1)");
			my $uoid = $q->("SELECT 'u'::regclass::oid");
			my $b = $node->background_psql('postgres', on_error_stop => 1);
			# columnar_stripes returns a set: it cannot be called by its address (it needs the catalogs to
			# find its result type), and the function manager's own wrapper reads them where no transaction
			# is in progress, so it runs only on a build without assertions
			foreach my $fn ('columnar_info', 'columnar_options', 'columnar_stripes', 'columnar_identity', 'columnar_creating_xid', 'columnar_table_xid')
			{
				my $name = "$fn of a table locked exclusively by another session, at commit";
				if ($fn eq 'columnar_stripes' && $assertions)
				{
					SKIP:
					{
						$skipped{$order}++;
						skip "$order: $name: a set-returning function is called through the function manager, which a build with assertions refuses here", 1;
						ok(1);
					}
					next;
				}
				my $sym = $fn eq 'columnar_stripes' ? 'NULL' : "'kingres_$fn'";
				$fresh->();
				my ($out, $err) = ('', '');
				my $off = $log_from->();
				$b->query_safe("BEGIN; LOCK TABLE u IN ACCESS EXCLUSIVE MODE");
				my $timed_out = 0;
				$node->psql('postgres', "SET kcevents.at = 'commit'; SET kcevents.op = 'sqlfn';
BEGIN; SELECT kcevents_open('t'); SELECT kcevents_sql('kingres.$fn(regclass)'::regprocedure::oid, $uoid, $sym); INSERT INTO t VALUES (100); COMMIT;
RESET kcevents.at; RESET kcevents.op;", stdout => \$out, stderr => \$err, on_error_stop => 0, timeout => 30, timed_out => \$timed_out);
				$b->query_safe("ROLLBACK");
				ok(!$timed_out, "$order: $name: returned at once (it did not wait for the lock)");
				$node->poll_query_until('postgres', "SELECT count(*) = 0 FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid() AND state <> 'idle'");
				my $log = slurp_file($node->logfile, $off);
				$alive->($name, $log);
				like($err, qr/WARNING:  $fn refused: the table is locked exclusively by another transaction/, "$order: $name: refused with a warning, not waited for");
				like($log, qr/kcevents: sqlfn $fn done rows=0 isnull=[01]/, "$order: $name: the answer is null or no rows");
				is($rows->(), "$base,100", "$order: $name: the transaction committed");
			}
			$b->quit;
		}
	}

	$node->stop;
	diag($skipped{$order}
		? "$order: $skipped{$order} cells were NOT run: this server is built with assertions, and a read of a table at a moment where an error is forbidden stops it there, for heap as for columnar (PostgreSQL's own rule).  They are run on a server built without assertions (README, \"Writes while a transaction is ending\")."
		: "$order: every cell was run (this server is built without assertions)");
}

done_testing();
