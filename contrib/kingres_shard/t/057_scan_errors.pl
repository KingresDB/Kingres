# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/057_scan_errors.pl
#
# An error that ends a distributed scan and is caught (review finding S6-02 of
# kingres/docs/reviews/2026-11-15-kingres-shard-0.4-port.md).  A scan asks for its
# next batch before the executor needs it, so when an error ends the statement a
# FETCH is in flight on the connection to another node, and the connection points
# at the scan's state.  At the abort of a subtransaction (ROLLBACK TO SAVEPOINT, the
# EXCEPTION clause of PL/pgSQL) the connection stayed open with that pointer, the
# state was freed with the statement's memory, and the next statement that used the
# connection called a function with it: the backend crashed (use after free).
#
# Every case runs the statements that fail and then statements that need the same
# connections.  A crash shows as a lost connection and the log of the node; the
# counts after it say that the connections work and the answers are right.  The
# fetch size is small, so that every scan has several batches and a batch in flight.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql prepared_count);

my @nodes = make_cluster('se', 3);
my ($n1, $n2, $n3) = @nodes;
my $ROWS = 900;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

# Run a script the way psql runs a file: a failed statement does not stop it.
sub script
{
	my ($sql) = @_;
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.scan_fetch_size = 10;\n$sql", on_error_stop => 0);
	return ($out, $err);
}

sub crashed
{
	my ($err) = @_;
	return $err =~ /server closed the connection|terminated by signal|connection to server .* failed|lost connection/ ? 1 : 0;
}

# The postmaster log of every node is free of crashes at the end of each part.
sub no_crash_in_logs
{
	my ($what) = @_;
	for my $n (@nodes)
	{
		my $log = slurp_file($n->logfile);
		unlike($log, qr/terminated by signal|TRAP:|AddressSanitizer/, "$what: no crash in the log of " . $n->name);
	}
}

$n1->safe_psql('postgres', q{
	CREATE TABLE public.d (id int PRIMARY KEY, v int);
	SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 6);
});
q1($n1, join('', map { "INSERT INTO public.d VALUES ($_, $_);\n" } 1 .. $ROWS));
is(q1($n1, 'SELECT count(*) FROM public.d'), $ROWS, 'the table has its rows');
my ($bad0) = keys_of_group($n1, 1, 0, 1, 300);	# a key that node 1 holds
my ($bad1) = keys_of_group($n1, 1, 1, 1, 300);	# one of node 2
my ($bad2) = keys_of_group($n1, 1, 2, 1, 300);	# one of node 3

# ---------------------------------------------------------------------------
# The sequence of the review: a failed scan, ROLLBACK TO, then a statement that
# needs the other nodes.
# ---------------------------------------------------------------------------
for my $case (
	[ 'an error of this node, sorted', "SELECT id, 100/(v-$bad0) FROM public.d ORDER BY id" ],
	[ 'an error of this node', "SELECT id, 100/(v-$bad0) FROM public.d" ],
	[ 'an error of node 2 (a shipped restriction)', "SELECT count(*) FROM public.d WHERE 100/(id-$bad1) > 0" ],
	[ 'an error of node 3 (a shipped restriction)', "SELECT id FROM public.d WHERE 100/(id-$bad2) > 0" ],
  )
{
	my ($out, $err) = script(qq{
		BEGIN;
		SAVEPOINT s;
		$case->[1];
		ROLLBACK TO s;
		SELECT 'after1';
		SELECT count(*) FROM public.d;
		SELECT count(*) FROM public.d WHERE v > 0;
		COMMIT;
	});
	ok(!crashed($err), "$case->[0]: the backend survives") or diag $err;
	like($err, qr/division by zero/, "$case->[0]: the error is reported");
	like($out, qr/after1\n$ROWS\n$ROWS(\n|$)/, "$case->[0]: the statements after ROLLBACK TO need the nodes and work");
}
is(prepared_count($_), '0', 'nothing is left prepared on ' . $_->name) for @nodes;
no_crash_in_logs('the review sequence');

# A write in the same transaction after the rollback: the remote savepoints match.
{
	my ($out, $err) = script(qq{
		BEGIN;
		INSERT INTO public.d VALUES (10001, 1);
		SAVEPOINT s;
		SELECT id, 100/(v-$bad1) FROM public.d;
		ROLLBACK TO s;
		INSERT INTO public.d VALUES (10002, 2);
		SAVEPOINT s2;
		INSERT INTO public.d VALUES (10003, 3);
		ROLLBACK TO s2;
		SELECT count(*) FROM public.d;
		COMMIT;
		SELECT count(*) FROM public.d WHERE id > 10000;
		DELETE FROM public.d WHERE id > 10000;
	});
	ok(!crashed($err), 'a write after the rollback: the backend survives') or diag $err;
	like($out, qr/(^|\n)902\n2$/, 'a write after the rollback: the rows of the transaction are there and the one rolled back is not') or diag "$out\n$err";
	is(q1($n1, 'SELECT count(*) FROM public.d'), $ROWS, 'the table is as it was');
	is(prepared_count($_), '0', 'nothing is left prepared on ' . $_->name) for @nodes;
}

# ---------------------------------------------------------------------------
# PL/pgSQL: an EXCEPTION clause around the loop of a scan.
# ---------------------------------------------------------------------------
{
	my ($out, $err) = script(q{
		DO $$ DECLARE r record; n int;
		BEGIN
		  BEGIN
		    FOR r IN SELECT id FROM public.d LOOP PERFORM 1/0; END LOOP;
		  EXCEPTION WHEN division_by_zero THEN RAISE NOTICE 'caught'; END;
		  SELECT count(*) INTO n FROM public.d;
		  RAISE NOTICE 'count %', n;
		END $$;
		SELECT count(*) FROM public.d;
	});
	ok(!crashed($err), 'EXCEPTION around a loop: the backend survives') or diag $err;
	like($err, qr/caught/, 'EXCEPTION around a loop: the error was handled');
	like($err, qr/count $ROWS/, 'EXCEPTION around a loop: a scan after it works inside the block');
	like($out, qr/^$ROWS$/m, 'EXCEPTION around a loop: and after the block');

	# The block alone in a DO, with the failing scan; the commit of the DO works.
	($out, $err) = script(q{
		DO $$ DECLARE r record;
		BEGIN
		  BEGIN
		    FOR r IN SELECT id FROM public.d LOOP PERFORM 1/0; END LOOP;
		  EXCEPTION WHEN division_by_zero THEN NULL; END;
		END $$;
		SELECT count(*) FROM public.d;
	});
	unlike($err, qr/unexpected nested remote transaction/, 'a DO whose block handled the error commits');
	ok(!crashed($err), 'a DO whose block handled the error: the backend survives') or diag $err;
	like($out, qr/^$ROWS$/m, 'a DO whose block handled the error: the next statement works');
}

# ---------------------------------------------------------------------------
# Nested exception blocks, a scan in each, the outer one still being read.
# ---------------------------------------------------------------------------
{
	q1($n1, q{
		CREATE FUNCTION public.nest(depth int) RETURNS int LANGUAGE plpgsql AS $$
		DECLARE r record; n int := 0;
		BEGIN
		  BEGIN
		    FOR r IN SELECT id FROM public.d LOOP
		      IF depth > 0 THEN n := n + public.nest(depth - 1); END IF;
		      PERFORM 1/0;
		    END LOOP;
		  EXCEPTION WHEN division_by_zero THEN n := n + 1; END;
		  RETURN n;
		END $$;
	});
	my ($out, $err) = script(q{
		SELECT public.nest(3);
		SELECT sum(public.nest(2)) FROM generate_series(1, 200);
		SELECT count(*) FROM public.d;
	});
	ok(!crashed($err), 'nested blocks: the backend survives') or diag $err;
	like($out, qr/^4\n600\n$ROWS$/m, 'nested blocks: every level handled its error, and the scans work afterwards') or diag "$out\n$err";
	no_crash_in_logs('nested blocks');
}

# ---------------------------------------------------------------------------
# Cursors declared before and after the savepoint.
# ---------------------------------------------------------------------------
{
	my ($out, $err) = script(qq{
		BEGIN;
		DECLARE c0 CURSOR FOR SELECT id FROM public.d;
		\\echo c0a
		FETCH 5 FROM c0;
		SAVEPOINT s;
		DECLARE c1 CURSOR FOR SELECT id, 100/(id-$bad1) FROM public.d;
		FETCH 3 FROM c1;
		\\echo c1err
		FETCH 2000 FROM c1;
		ROLLBACK TO s;
		\\echo c0b
		FETCH 5 FROM c0;
		\\echo c1gone
		SAVEPOINT s1;
		FETCH 1 FROM c1;
		ROLLBACK TO s1;
		SAVEPOINT s2;
		DECLARE c2 CURSOR FOR SELECT id FROM public.d;
		\\echo c2a
		FETCH 2 FROM c2;
		RELEASE s2;
		\\echo c2b
		FETCH 2 FROM c2;
		CLOSE c0;
		CLOSE c2;
		\\echo total
		SELECT count(*) FROM public.d;
		COMMIT;
	});
	ok(!crashed($err), 'cursors: the backend survives') or diag $err;
	like($err, qr/division by zero/, 'cursors: the error of the cursor opened after the savepoint');
	like($err, qr/cursor "c1" does not exist/, 'cursors: the cursor opened after the savepoint is gone');
	my ($a) = $out =~ /c0a\n((?:\d+\n){5})/;
	my ($b) = $out =~ /c0b\n((?:\d+\n){5})/;
	ok(defined $a && defined $b, 'cursors: the cursor opened before the savepoint gave five rows before and five after') or diag "$out\n$err";
	my %seen = map { $_ => 1 } split /\n/, ($a // '') . ($b // '');
	is(scalar(keys %seen), 10, 'cursors: ...ten different rows, it went on where it was');
	like($out, qr/c2a\n\d+\n\d+\nc2b\n\d+\n\d+\ntotal\n$ROWS(\n|$)/, 'cursors: a cursor opened in a released savepoint goes on, and the count after it is right');
	is(prepared_count($_), '0', 'nothing is left prepared on ' . $_->name) for @nodes;
}

# ---------------------------------------------------------------------------
# An error in the middle of fetching from several groups, many times in one backend.
# ---------------------------------------------------------------------------
for my $case (
	[ 'a remote error', "WHERE 100/(id-$bad2) > 0" ],
	[ 'an error of this node', "WHERE 100/(id-$bad0) > 0" ],
	[ 'an error in the loop body', "WHERE true" ],
  )
{
	my $body = $case->[0] eq 'an error in the loop body' ? 'PERFORM 1/(r.id - ' . $bad1 . ');' : 'm := m + 1;';
	my ($out, $err) = script(qq{
		DO \$\$ DECLARE r record; m int := 0; caught int := 0; n int;
		BEGIN
		  FOR i IN 1 .. 1500 LOOP
		    BEGIN
		      FOR r IN SELECT id FROM public.d $case->[1] LOOP $body END LOOP;
		    EXCEPTION WHEN division_by_zero THEN caught := caught + 1; END;
		    IF i % 100 = 0 THEN
		      SELECT count(*) INTO n FROM public.d;
		      IF n <> $ROWS THEN RAISE EXCEPTION 'wrong count %', n; END IF;
		    END IF;
		  END LOOP;
		  RAISE NOTICE 'caught %', caught;
		END \$\$;
		SELECT count(*) FROM public.d;
	});
	ok(!crashed($err), "$case->[0], 1500 times in one backend: the backend survives") or diag $err;
	like($err, qr/caught 1500/, "$case->[0]: every error was handled");
	like($out, qr/^$ROWS$/m, "$case->[0]: and the scan after them works");
}
no_crash_in_logs('many repetitions');

# ---------------------------------------------------------------------------
# A cancel that a handler catches: a statement_timeout in the middle of a scan.
# ---------------------------------------------------------------------------
{
	my ($out, $err) = script(q{
		SET statement_timeout = '400ms';
		DO $$ DECLARE n int;
		BEGIN
		  BEGIN
		    SELECT count(*) INTO n FROM public.d WHERE pg_sleep(0.004) IS NOT NULL;
		    RAISE NOTICE 'finished %', n;
		  EXCEPTION WHEN query_canceled THEN RAISE NOTICE 'canceled'; END;
		  SELECT count(*) INTO n FROM public.d;
		  RAISE NOTICE 'count %', n;
		END $$;
		RESET statement_timeout;
		SELECT count(*) FROM public.d;
	});
	ok(!crashed($err), 'a timeout caught by a handler: the backend survives') or diag $err;
	like($err, qr/canceled/, 'a timeout caught by a handler: the statement was cancelled in the middle of the scan');
	like($err, qr/count $ROWS/, 'a timeout caught by a handler: the next scan, in the same block, works');
	like($out, qr/^$ROWS$/m, 'a timeout caught by a handler: and after the block');
}

# ---------------------------------------------------------------------------
# An error injected at each point of the start of a scan, caught in a savepoint.
# ---------------------------------------------------------------------------
for my $point (qw(scan_begin scan_started scan_after_placement scan_declared_g1 scan_declared_g2
		scan_after_declare scan_after_check))
{
	my ($out, $err) = script(qq{
		DO \$\$ DECLARE n int; caught int := 0;
		BEGIN
		  FOR i IN 1 .. 60 LOOP
		    PERFORM set_config('kingres_shard.fault_injection', '$point:error', false);
		    BEGIN
		      SELECT count(*) INTO n FROM public.d;
		    EXCEPTION WHEN OTHERS THEN caught := caught + 1; END;
		    PERFORM set_config('kingres_shard.fault_injection', '', false);
		    SELECT count(*) INTO n FROM public.d;
		    IF n <> $ROWS THEN RAISE EXCEPTION 'wrong count %', n; END IF;
		  END LOOP;
		  RAISE NOTICE 'caught %', caught;
		END \$\$;
		SELECT count(*) FROM public.d;
	});
	ok(!crashed($err), "an error at $point, caught in a block, 60 times: the backend survives") or diag $err;
	like($err, qr/caught 60/, "an error at $point: every one was caught");
	like($out, qr/^$ROWS$/m, "an error at $point: the scans after them work");
}
no_crash_in_logs('injected errors');

# ---------------------------------------------------------------------------
# Two scans of one statement that read from the same nodes at the same time, and a
# statement that fails while both have a fetch in flight.
# ---------------------------------------------------------------------------
{
	my ($out, $err) = script(qq{
		SELECT count(*) FROM public.d a JOIN public.d b ON a.v = b.v;
		BEGIN;
		SAVEPOINT s;
		SELECT count(*) FROM public.d a JOIN public.d b ON a.v + 0 = b.v + 100/(b.v - $bad2);
		ROLLBACK TO s;
		SELECT count(*) FROM public.d a JOIN public.d b ON a.v = b.v;
		COMMIT;
	});
	ok(!crashed($err), 'two scans on the same nodes: the backend survives') or diag $err;
	like($out, qr/^$ROWS\n$ROWS$/m, 'two scans on the same nodes: the joins answer, before and after the failed one') or diag "$out\n$err";
}

# ---------------------------------------------------------------------------
# Cursors open at the end of a transaction, closed early, dropped with a rollback,
# and a session that ends in the middle of a scan.
# ---------------------------------------------------------------------------
{
	my ($out, $err) = script(q{
		BEGIN; DECLARE c CURSOR FOR SELECT id FROM public.d; FETCH 3 FROM c; COMMIT;
		SELECT count(*) FROM public.d;
		BEGIN; DECLARE c CURSOR FOR SELECT id FROM public.d; FETCH 3 FROM c; CLOSE c; SELECT count(*) FROM public.d; COMMIT;
		BEGIN; DECLARE c CURSOR FOR SELECT id FROM public.d; FETCH 3 FROM c; ROLLBACK;
		SELECT count(*) FROM public.d;
		DECLARE h CURSOR WITH HOLD FOR SELECT id FROM public.d;
		FETCH 3 FROM h;
		CLOSE h;
		SELECT count(*) FROM public.d;
		BEGIN; SELECT id FROM public.d LIMIT 3; SELECT count(*) FROM public.d; COMMIT;
	});
	ok(!crashed($err), 'cursors left open, closed, rolled back: the backend survives') or diag $err;
	my @counts = $out =~ /^$ROWS$/mg;
	is(scalar(@counts), 5, 'every count after them is right');
	my ($rc, $o, $e) = run_sql($n1, "BEGIN;\nSET kingres_shard.scan_fetch_size = 10;\nDECLARE c CURSOR FOR SELECT id FROM public.d;\nFETCH 3 FROM c;\n\\q\n", on_error_stop => 0);
	for my $n ($n2, $n3)
	{
		ok($n->poll_query_until('postgres', q{SELECT count(*) = 0 FROM pg_stat_activity WHERE state LIKE 'idle in transaction%'}),
			'a session that ended in the middle of a scan leaves no open transaction on ' . $n->name);
	}
	is(q1($n1, 'SELECT count(*) FROM public.d'), $ROWS, 'and the next session counts the rows');
}
no_crash_in_logs('cursors');

is(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || node_id || ' ' || object, ', '), '') FROM kingres.check_cluster()}),
	'', 'check_cluster finds nothing at the end');

done_testing();
