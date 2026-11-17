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
);

# the operations: op => [ warning text, fails the transaction when refused while it is alive,
#   how many "done" lines, error text where an error is allowed (undef: it works there),
#   kind: g guarded, r reads, s reads only the file, nothing else ]
my %ops = (
	ntrunc => [ 'truncate', 1, 1, undef, 'g' ],
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
		fn_safe => "SELECT kcevents_fn('kingres_columnar_reconcile', 0); SELECT kcevents_fn('kingres_columnar_finish_rewrites', 0); SELECT kcevents_fn('kingres_columnar_note_rewrite', 1);",
	};
	# run a script at a moment with an operation; returns (stdout, stderr, log text)
	my $run = sub
	{
		my ($at, $script, $op, $extra) = @_;
		my $realop = $op =~ /^fn_/ ? 'fn' : $op;
		my $reg = $op =~ /^fn_/ ? $fnlist->{$op} : '';
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
			my ($at, $script, $mkind, $want_ok, $want_fail) = @$m;
			my $name = "$op at " . ($at eq 'none' ? 'a critical section' : $at);

			# reads that allocate and open relations: not on a build with assertions (see above);
			# at a release phase they need the buffer pin that PostgreSQL itself forbids there
			# (size and estimate read the file's length: they allocate in PostgreSQL's smgr, which a
			# critical section forbids, again PostgreSQL's rule)
			if (($kind eq 'r' && ($assertions || $at =~ /^res_/)) || ($kind eq 's' && $assertions && $at eq 'none'))
			{
				SKIP:
				{
					skip "$order: $name: a read at a moment where an error is forbidden is PostgreSQL's own rule (assertions in the relation cache, the resource owner)", 1;
					ok(1);
				}
				next;
			}
			# size and estimate read the file's length only; at a release phase the smgr is fine too

			$fresh->();
			my ($out, $err, $log) = $run->($at, $script{$script}, $op);
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
				my $done = $op =~ /^fn_/ ? scalar(() = $log =~ /kcevents: fn \d+ done/g) : scalar(() = $log =~ /kcevents: $op done/g);
				is($done, $ndone, "$order: $name: the call returned ($ndone) and raised nothing");
				if ($fails && $mkind eq 'live')
				{
					like($err, qr/ERROR:  a call (?:on columnar table "t"|to \w+) was refused earlier in this transaction, so it cannot commit/,
						"$order: $name: the transaction that is still alive fails with an error at its pre-commit");
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

			# a truncate between the two phases of the commit of a subtransaction (the module's
			# callback runs after the access method's first phase) is refused with an error,
			# which is allowed there: the buffers are prepared for the hand-over and must not change
			my $err_here = $err_allowed;
			$err_here = 'columnar table cannot be truncated while a subtransaction is being committed'
			  if $op eq 'ntrunc' && $at eq 'pre_commit_sub' && $order eq 'after';

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
	# a truncate that is carried out where it may be (C8-01, the allowed side): the table
	# is empty and readable, and a later write works
	# ------------------------------------------------------------------
	{
		$fresh->();
		my ($out, $err, $log) = $run->('pre_commit', "BEGIN; SELECT kcevents_open('t'); COMMIT;", 'ntrunc');
		is($rows->(), '', "$order: relation_nontransactional_truncate at pre-commit: the table is empty");
		is($q->("SELECT count(*) FROM kingres.columnar_check()"), 0, "$order: ... and the file and the stripe rows agree");
		$q->("INSERT INTO t VALUES (7)");
		is($rows->(), '7', "$order: ... and it is written again");
		is($q->("SELECT rows FROM kingres.columnar_info('t')"), 1, "$order: ... the catalog counts the new row only");
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

	$node->stop;
}

done_testing();
