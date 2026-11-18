# contrib/kingres_columnar/t/036_check_view.pl
#
# columnar_check(), columnar_check(true) and columnar_check_clean(): what they look at, and what they
# never do (C10-01, C10-02, C10-08).
#
# C10-01.  The functions read the stripe catalog as of now, whatever the isolation level of the
# caller.  Read with the caller's own snapshot, in REPEATABLE READ or SERIALIZABLE they said "clean"
# for a damaged table whose stripes the old snapshot cannot see: a table made after the snapshot, or
# stripes committed after it.  A table that is dropped or made while the call runs is a not_checked
# row, never counted as clean.
#
# C10-02.  The check never hides a query cancel, a statement timeout or any error that is not about
# the table being read (they end the call), and every lock it takes is taken without waiting: the
# stripe catalog, its index and its TOAST table are locked conditionally, and a catalog that cannot
# be locked gives a not_checked row (and columnar_check_clean() false) instead of a wait behind an
# administrator's LOCK TABLE, REINDEX or a prepared transaction that never ends.
#
# C10-08.  The locks the check leaves: an ACCESS SHARE lock on every columnar table until the end
# of the caller's transaction, and none on the catalog.
#
# The setting kingres_columnar.check_gate (a developer setting: the key of an advisory lock the
# check waits on at the start of the look at each table) makes the moments "while the call runs"
# exact: the test holds the advisory lock, waits until the call is waiting on it, drops and makes
# tables, and lets go.  No verdict depends on how long anything takes; the bounded waits are for a
# case that fails.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $BLCKSZ = 8192;

my $node = PostgreSQL::Test::Cluster->new('colcheckview');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_columnar'\nmax_prepared_transactions = 10\nautovacuum = off\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

# every helper says what it was doing when it fails (a failure in a meson run shows only the end of the log)
sub q1
{
	my ($sql) = @_;
	my $r = eval { $node->safe_psql('postgres', $sql) };
	die "t/036: q1 failed running [$sql]: $@" if $@;
	return $r;
}

# Wait until the backend with this pid is waiting at the gate (wait event PgSleep: the gate waits in steps of 10 ms,
# so the event is seen at one of the polls, and the call stays there until the advisory lock is released).  The pid is
# the one the call's own session reported: asking for "the backend that sleeps" again afterwards finds nothing when it
# happens to be between two steps (the cause of a failure of 2 runs in 60: pg_cancel_backend() of an empty pid).
sub at_gate
{
	my ($pid, $what) = @_;
	$node->poll_query_until('postgres', "SELECT count(*) = 1 FROM pg_stat_activity WHERE pid = $pid AND wait_event = 'PgSleep'")
	  or die "t/036: the call (pid $pid) did not reach the gate: $what";
}

sub bgq
{
	my ($s, $sql) = @_;
	my $r = eval { $s->query_safe($sql) };
	die "t/036: a background session failed running [$sql]: $@" if $@;
	return $r;
}

sub bg_value
{
	my ($s, $sql) = @_;
	my $v = eval { bgq($s, $sql) };
	die "t/036: a background session failed running [$sql]: $@" if $@;
	$v =~ s/\s+//g;
	return $v;
}

# a columnar table of many stripes, written to disk
sub make_table
{
	my ($t, $rows) = @_;
	q1("CREATE TABLE $t (a int, b text) USING columnar;
		SELECT kingres.columnar_set_options('$t', stripe_row_count => 1000, chunk_row_count => 100);
		INSERT INTO $t SELECT g, repeat('x', 50) FROM generate_series(1, $rows) g;");
}

# cut the file of a table to a number of blocks (the table is damaged at the operating system level)
sub cut_file
{
	my ($t, $blocks) = @_;
	q1('CHECKPOINT');
	my $path = $node->data_dir . '/' . q1("SELECT pg_relation_filepath('$t')");
	truncate($path, $blocks * $BLCKSZ) or die "truncate $path: $!";
}

sub names
{
	my ($sql_fn) = @_;
	return q1("SELECT coalesce(string_agg(DISTINCT check_name, ',' ORDER BY check_name), '') FROM $sql_fn");
}

# ------------------------------------------------------------------------
# C10-01: an older REPEATABLE READ or SERIALIZABLE session does not see stripes, or tables, that
# were committed after its snapshot; the check must
# ------------------------------------------------------------------------
foreach my $level ('REPEATABLE READ', 'SERIALIZABLE')
{
	my $l = $level eq 'SERIALIZABLE' ? 's' : 'r';

	# (a) a damaged table made after the snapshot (the review's steps)
	my $old = $node->background_psql('postgres', on_error_stop => 1);
	bgq($old, "BEGIN ISOLATION LEVEL $level; SELECT 1;");
	make_table("d$l", 20000);
	cut_file("d$l", 2);
	is(q1('SELECT kingres.columnar_check_clean()'), 'f', "$level: a fresh session: the damaged table made after the old snapshot is not clean");
	like(names('kingres.columnar_check()'), qr/stripe_beyond_file/, "$level: ... and columnar_check names the damage");
	is(bg_value($old, 'SELECT kingres.columnar_check_clean()'), 'f', "$level: the older session: not clean either (it was clean before the fix)");
	is(bg_value($old, "SELECT count(*) > 0 FROM kingres.columnar_check() WHERE check_name = 'stripe_beyond_file'"), 't',
		"$level: ... and columnar_check shows the damage to it");
	is(bg_value($old, "SELECT count(*) > 0 FROM kingres.columnar_check(true) WHERE check_name = 'stripe_beyond_file'"), 't',
		"$level: columnar_check(true) in the older session repairs what it cannot see with its own snapshot");
	is(bg_value($old, 'SELECT kingres.columnar_check_clean()'), 't', "$level: ... and the table is clean afterwards");
	bgq($old, 'COMMIT');
	$old->quit;
	is(q1('SELECT kingres.columnar_check_clean()'), 't', "$level: ... and for a fresh session once the older session has committed the repair");
	q1("DROP TABLE d$l");

	# (b) damage in stripes committed after the snapshot, in a table that the snapshot knows
	make_table("e$l", 1000);
	my $blocks = q1("SELECT pg_relation_size('e$l') / $BLCKSZ");
	my $old2 = $node->background_psql('postgres', on_error_stop => 1);
	bgq($old2, "BEGIN ISOLATION LEVEL $level; SELECT count(*) FROM e$l;");
	q1("INSERT INTO e$l SELECT g, repeat('y', 50) FROM generate_series(1, 20000) g");
	cut_file("e$l", $blocks);
	is(q1('SELECT kingres.columnar_check_clean()'), 'f', "$level: damage in stripes committed after the snapshot: not clean (fresh session)");
	is(bg_value($old2, 'SELECT kingres.columnar_check_clean()'), 'f', "$level: ... and not clean for the older session (stripes it cannot see)");
	is(bg_value($old2, "SELECT count(*) > 0 FROM kingres.columnar_check() WHERE check_name = 'stripe_beyond_file'"), 't', "$level: ... with the stripes named");
	bgq($old2, 'ROLLBACK');
	$old2->quit;
	q1("SELECT * FROM kingres.columnar_check(true)");
	q1("DROP TABLE e$l");
	is(q1('SELECT kingres.columnar_check_clean()'), 't', "$level: nothing is left");
}

# ------------------------------------------------------------------------
# C10-01: a table dropped or made while the call runs (the gate: the call waits at the start of the
# look at each table until the advisory lock is released)
# ------------------------------------------------------------------------
{
	q1("CREATE TABLE g1 (a int) USING columnar; INSERT INTO g1 VALUES (1);
		CREATE TABLE g2 (a int) USING columnar; INSERT INTO g2 VALUES (1);");
	is(q1('SELECT kingres.columnar_check_clean()'), 't', 'before: the tables are clean');

	foreach my $fn ('SELECT kingres.columnar_check_clean()', "SELECT string_agg(check_name || '/' || detail, ' ;; ' ORDER BY check_name, detail) FROM kingres.columnar_check()")
	{
		my $gate = $node->background_psql('postgres', on_error_stop => 1);
		bgq($gate, 'SELECT pg_advisory_lock(7770, 0)');
		my $call = $node->background_psql('postgres', on_error_stop => 1);
		bgq($call, 'SET kingres_columnar.check_gate = 7770');
		my $cpid = bg_value($call, 'SELECT pg_backend_pid()');
		$call->query_until(qr/^started$/m, "\\echo started\n$fn;\n");
		at_gate($cpid, "dropped/made, $fn");
		q1("DROP TABLE g1; CREATE TABLE g3 (a int) USING columnar;");
		bgq($gate, 'SELECT pg_advisory_unlock(7770, 0)');
		my $res = $call->query_until(qr/^finished$/m, "\\echo finished\n");
		$call->quit;
		$gate->quit;
		if ($fn =~ /clean/)
		{
			like($res, qr/^f$/m, 'a table dropped and a table made while columnar_check_clean() runs: not clean');
		}
		else
		{
			like($res, qr/not_checked\/the table was dropped during the call and was not checked/, 'a table dropped while the call runs: a not_checked row that says so');
			like($res, qr/not_checked\/the table was made during the call and was not checked/, 'a table made while the call runs: a not_checked row that says so');
			unlike($res, qr/stripe_beyond_file|unreadable|page_header_mismatch|orphan_catalog_rows/, '... and nothing else is reported (g2 was checked and is sound)');
		}
		# put the tables back for the second form
		q1("DROP TABLE IF EXISTS g3; CREATE TABLE IF NOT EXISTS g1 (a int) USING columnar; INSERT INTO g1 VALUES (1);");
	}
	is(q1('SELECT kingres.columnar_check_clean()'), 't', 'afterwards, with nothing going on, the check is clean');
	q1('DROP TABLE g1, g2');
}

# ------------------------------------------------------------------------
# C10-02: a cancel, a statement timeout or any other error that is not about the table ends the call.
# The call is held at the gate, inside the look at a table (the place where the old code turned any
# error into an "unreadable" row and went on to the next table, which waited again).
# ------------------------------------------------------------------------
{
	make_table('cn', 5000);
	make_table('cm', 5000);

	# Run $sql with the gate held; $during->(pid) acts on the call while it waits; then the call must end
	# on its own (it is no longer active a short while later, with the gate still held), and the gate is let go.
	# Returns the error text of the call.
	my $key = 7771;
	my $held = sub
	{
		my ($setup, $sql, $during) = @_;
		my $gate = $node->background_psql('postgres', on_error_stop => 1);
		bgq($gate, "SELECT pg_advisory_lock($key, 0)");
		my $call = $node->background_psql('postgres', on_error_stop => 0);
		bgq($call, "SET kingres_columnar.check_gate = $key; $setup");
		my $pid = bg_value($call, 'SELECT pg_backend_pid()');
		$call->{stderr} = '';
		$call->query_until(qr/^started$/m, "\\echo started\n$sql;\n");
		at_gate($pid, "held, $sql");
		$during->($pid);
		# the call ends by itself: within 30 seconds it is idle again, although the gate is still held
		my $ended = 0;
		for (1 .. 300)
		{
			if (q1("SELECT state = 'idle' FROM pg_stat_activity WHERE pid = $pid") eq 't') { $ended = 1; last; }
			select(undef, undef, undef, 0.1);
		}
		bgq($gate, "SELECT pg_advisory_unlock($key, 0)");
		my $res = $call->query_until(qr/^finished$/m, "\\echo finished\n");
		my $err = $call->{stderr};
		$call->quit;
		$gate->quit;
		return ($ended, $res, $err);
	};

	foreach my $fn ('SELECT kingres.columnar_check_clean()', 'SELECT count(*) FROM kingres.columnar_check()', 'SELECT count(*) FROM kingres.columnar_check(true)')
	{
		my ($ended, $res, $err) = $held->('', $fn, sub { q1("SELECT pg_cancel_backend($_[0])"); });
		ok($ended, "a cancel while a table is being looked at ends the call: $fn");
		like($err, qr/canceling statement due to user request/, '... with the cancel');
		unlike($res, qr/^[tf0-9]+$/m, '... and no answer, in particular not "clean"');
	}

	my ($ended, $res, $err) = $held->("SET statement_timeout = '1500ms';", 'SELECT kingres.columnar_check_clean()', sub { });
	ok($ended, 'a statement timeout while a table is being looked at ends the call');
	like($err, qr/canceling statement due to statement timeout/, '... with the timeout');
	unlike($res, qr/^[tf]$/m, '... and no answer, in particular not "clean"');

	is(q1('SELECT kingres.columnar_check_clean()'), 't', 'the tables are clean when nothing was wrong');
	q1('DROP TABLE cn, cm');
}

# ------------------------------------------------------------------------
# C10-02: a catalog that cannot be locked is a not_checked row at once, with and without fix
# ------------------------------------------------------------------------
{
	make_table('lk', 3000);
	my $toast = q1("SELECT reltoastrelid::regclass::text FROM pg_class WHERE oid = 'kingres.columnar_stripe'::regclass");
	my @held = (
		[ 'the stripe catalog locked ACCESS EXCLUSIVE', 'LOCK TABLE kingres.columnar_stripe IN ACCESS EXCLUSIVE MODE' ],
		[ 'the index of the stripe catalog being rebuilt (REINDEX INDEX)', 'REINDEX INDEX kingres.columnar_stripe_pkey' ],
		[ 'the TOAST table of the stripe catalog being reindexed', "REINDEX TABLE $toast" ],
	);
	foreach my $h (@held)
	{
		my ($what, $sql) = @$h;
		$node->safe_psql('postgres', "BEGIN; $sql; PREPARE TRANSACTION 'cat'");
		foreach my $fix ('false', 'true')
		{
			my ($out, $err) = ('', '');
			my $to = 0;
			$node->psql('postgres', "SET statement_timeout = '60s'; SELECT 'ROW=' || check_name || ':' || coalesce(relation::text, 'null') FROM kingres.columnar_check($fix) ORDER BY 1;",
				stdout => \$out, stderr => \$err, on_error_stop => 0, timeout => 120, timed_out => \$to);
			ok(!$to && $err !~ /timeout|canceling/, "$what: columnar_check($fix) does not wait for the lock");
			like($out, qr/ROW=not_checked:null/, "$what: ... it reports not_checked, for no table in particular");
			unlike($out, qr/ROW=(?:stripe_beyond_file|unreadable|page_header_mismatch|orphan_catalog_rows)/, "$what: ... and nothing else");
		}
		my ($out, $err) = ('', '');
		my $to = 0;
		$node->psql('postgres', "SET statement_timeout = '60s'; SELECT kingres.columnar_check_clean()", stdout => \$out, stderr => \$err, on_error_stop => 0, timeout => 120, timed_out => \$to);
		ok(!$to && $err !~ /timeout|canceling/, "$what: columnar_check_clean() does not wait for the lock");
		like($out, qr/^\s*f\s*$/m, "$what: ... it is false");
		$node->safe_psql('postgres', "COMMIT PREPARED 'cat'");
		is(q1('SELECT kingres.columnar_check_clean()'), 't', "$what: after the transaction ended the check is clean");
	}
}

# ------------------------------------------------------------------------
# C10-08: what the check leaves behind: ACCESS SHARE on every columnar table until the caller's
# transaction ends, nothing on the catalog
# ------------------------------------------------------------------------
{
	my $s = $node->background_psql('postgres', on_error_stop => 1);
	bgq($s, 'BEGIN; SELECT kingres.columnar_check_clean();');
	is(bg_value($s, "SELECT count(*) FROM pg_locks WHERE pid = pg_backend_pid() AND relation = 'lk'::regclass AND mode = 'AccessShareLock'"), '1',
		'the check keeps an ACCESS SHARE lock on the columnar table until the end of the transaction');
	is(bg_value($s, "SELECT count(*) FROM pg_locks WHERE pid = pg_backend_pid() AND relation IN ('kingres.columnar_stripe'::regclass, 'kingres.columnar_stripe_pkey'::regclass, 'pg_class'::regclass)"), '0',
		'... and no lock on the stripe catalog, its index or pg_class');
	my ($out, $err) = ('', '');
	$node->psql('postgres', "SET lock_timeout = '300ms'; ALTER TABLE lk ADD COLUMN c int", stdout => \$out, stderr => \$err, on_error_stop => 0);
	like($err, qr/canceling statement due to lock timeout/, '... so DDL on the table waits for the transaction that ran the check');
	bgq($s, 'COMMIT');
	$s->quit;
	($out, $err) = ('', '');
	$node->psql('postgres', "SET lock_timeout = '5s'; ALTER TABLE lk ADD COLUMN c int", stdout => \$out, stderr => \$err, on_error_stop => 0);
	is($err, '', '... and goes through when it is over');
}

$node->stop;
done_testing();
