# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/206_split_writers.pl: writers against a split (design/shard-phase10.md, "The fixes for the review of phase 10": P10R-02, -03, -04, -14, and the tests that the
# design promised and t/204 lacked).
#
#   A. writes of roles that are not superusers while the bucket is copied: the owner of the tables and a role with the privileges of the application, from every node
#   B. inserts at random keys at a steady rate while the copy runs: the copy converges and no row is lost
#   C. a row that a writer holds is not lost: the pass that finds it held goes on until the writer ends (verify 'none')
#   D. the switch meets a deadlock and is tried again; the writers are not the victims
#   E. a writer that keeps the switch out until its attempts run out: the split fails with the writer named and is undone

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use POSIX ();
use Test::More;
use IPC::Run ();
use Time::HiRes qw(usleep time);
use KingresShardTest qw(make_cluster run_sql wait_for_fault_file);

my $tmp = PostgreSQL::Test::Utils::tempdir();
my @nodes = make_cluster('sx', 3, logical => 1, recovery_interval => 0,
	extra => "max_connections = 100\nkingres_shard.node_timeout = 10\nkingres_shard.move_cleanup_delay = 1\n");
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

q1($n1, q{
	CREATE TABLE public.s1 (id int PRIMARY KEY, v text);
	CREATE TABLE public.s2 (id int PRIMARY KEY, w int NOT NULL);
	INSERT INTO public.s1 SELECT g, 'v' || g FROM generate_series(1, 3000) g;
	INSERT INTO public.s2 SELECT g, g * 3 FROM generate_series(1, 3000) g;
	SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 8);
	SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s1');
});
q1($n1, q{ALTER TABLE public.s2 ADD CONSTRAINT s2_fk FOREIGN KEY (id) REFERENCES public.s1 (id)});
my $C = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.s1'::regclass});
sub owner { my ($b) = @_; return q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"); }
sub node_of_group
{
	my ($gr) = @_;
	my $port = q1($n1, "SELECT port FROM kingres.node WHERE group_id = $gr AND role = 'primary'");
	for my $n (@nodes) { return $n if $n->port == $port; }
	die "no node for group $gr";
}
sub clean { return q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || coalesce(object, '') || ' ' || coalesce(detail, ''), '; '), '') FROM kingres.check_cluster()}); }
sub last_split { return q1($n1, q{SELECT split_id FROM kingres.bucket_split ORDER BY split_id DESC LIMIT 1}); }
sub phase_of { my ($id) = @_; return q1($n1, "SELECT phase FROM kingres.bucket_split WHERE split_id = $id"); }
sub keys_of { my ($b, $lo, $hi) = @_; return split /\n/, q1($n1, "SELECT g FROM generate_series($lo, $hi) g WHERE kingres.bucket_for('public.s1'::regclass, g) = $b ORDER BY g"); }

sub sess
{
	my ($node) = @_;
	my ($in, $out, $err) = ('', '', '');
	my $s = { in => \$in, out => \$out, err => \$err, node => $node };
	$s->{h} = IPC::Run::start([ 'psql', '-X', '-At', '-h', $node->host, '-p', $node->port, '-d', 'postgres' ], '<', \$in, '>', \$out, '2>', \$err);
	return $s;
}
sub send_sql { my ($s, $sql) = @_; ${ $s->{in} } .= "$sql\n"; eval { $s->{h}->pump_nb; 1 }; }
sub wait_out
{
	my ($s, $re, $secs) = @_;
	for (1 .. $secs * 20) { eval { $s->{h}->pump_nb; 1 }; return 1 if ${ $s->{out} } =~ $re || ${ $s->{err} } =~ $re; usleep(50_000); }
	return 0;
}
sub close_sess { my ($s) = @_; eval { $s->{h}->kill_kill; 1 }; }
sub stopped
{
	my ($node, $point, $sql, $name) = @_;
	my $file = "$tmp/wait_$name";
	open(my $fh, '>', $file) or die;
	close($fh);
	my $pid = fork();
	die 'fork' unless defined $pid;
	if ($pid == 0)
	{
		my $s = "SET kingres_shard.fault_injection = '$point:wait:$file'; $sql";
		system("psql -X -q -h " . $node->host . " -p " . $node->port . " -d postgres -c \"$s\" > $tmp/out_$name 2>&1");
		POSIX::_exit(0);
	}
	return ($pid, $file);
}
# runs a file of statements in a process of its own (the child of a fork): the process is psql itself, so that it can be ended by its pid
sub exec_psql_file
{
	my ($node, $file, $out) = @_;
	open(STDOUT, '>', $out) or POSIX::_exit(1);
	open(STDERR, '>&', *STDOUT) or POSIX::_exit(1);
	exec('psql', '-X', '-q', '-h', $node->host, '-p', $node->port, '-d', 'postgres', '-f', $file) or POSIX::_exit(1);
}
sub release { my ($pid, $file) = @_; unlink($file); waitpid($pid, 0); }
sub out { my ($name) = @_; open(my $fh, '<', "$tmp/out_$name") or return ''; local $/; my $s = <$fh>; close($fh); return $s; }
sub digest
{
	my ($n) = @_;
	return q1($n, q{SELECT (SELECT count(*) || ',' || sum(id) || ',' || md5(string_agg(v, ',' ORDER BY id)) FROM public.s1) || '|' || (SELECT count(*) || ',' || sum(w) FROM public.s2)});
}

is(clean(), '', 'the cluster is clean at the start');

# ---------------------------------------------------------------------------
# A. writers that are not superusers (P10R-02)
# ---------------------------------------------------------------------------
{
	$_->safe_psql('postgres', q{CREATE ROLE tenant LOGIN; CREATE ROLE app LOGIN; CREATE ROLE spy LOGIN}) for @nodes;
	q1($n1, q{ALTER TABLE public.s1 OWNER TO tenant; ALTER TABLE public.s2 OWNER TO tenant;
	          GRANT SELECT, INSERT, UPDATE, DELETE ON public.s1, public.s2 TO app;
	          GRANT SELECT ON public.s1 TO spy});
	is(q1($_, q{SELECT has_table_privilege('app', 'public.s1', 'INSERT') || ':' || has_table_privilege('spy', 'public.s1', 'INSERT') || ':' || (SELECT relowner::regrole::text FROM pg_class WHERE oid = 'public.s1'::regclass)}), 'true:false:tenant', 'A: the privileges and the owner are on ' . $_->name) for @nodes;
	my $b = 2;
	my $g = owner($b);
	my $own = node_of_group($g);
	my ($pid, $file) = stopped($n1, 'split_after_backfill', "SELECT kingres.split_bucket($C, $b, 2, 'none')", 'a1');
	ok(wait_for_fault_file("$file.reached", 60), 'A: the split of bucket 2 reached the point after its backfill');
	is(q1($own, q{SELECT string_agg(p.proowner::regrole::text || ':' || p.prosecdef::text || ':' || has_function_privilege('app', p.oid, 'EXECUTE')::text, ',' ORDER BY p.proname) FROM pg_proc p WHERE p.proname LIKE 'split_capture_%'}),
	   'tenant:true:false,tenant:true:false', 'A: the capture functions belong to the owner of the tables, run with its rights and are not callable by others');
	is(q1($own, q{SELECT string_agg(DISTINCT relowner::regrole::text, ',') FROM pg_class WHERE relname ~ '^s[12]_s[0-9]+_[0-9]+$'}), 'tenant', 'A: and so do the children');
	my @k = keys_of($b, 10000, 12000);
	my $n = 0;
	my @kept;
	for my $node (@nodes)
	{
		for my $role (qw(app tenant))
		{
			my ($key, $key2, $key3) = @k[$n, $n + 1, $n + 2];
			$n += 3;
			my ($rc, $o, $e) = run_sql($node, "SET ROLE $role; INSERT INTO public.s1 VALUES ($key, 'by $role'); UPDATE public.s1 SET v = 'by $role again' WHERE id = $key");
			is($rc, 0, "A: $role inserts and updates a row of the splitting bucket from " . $node->name) or diag($e);
			($rc, $o, $e) = run_sql($node, "SET ROLE $role; INSERT INTO public.s1 VALUES ($key2, 'kept'), ($key3, 'kept too'); DELETE FROM public.s1 WHERE id = $key2");
			is($rc, 0, "A: $role inserts and deletes from " . $node->name) or diag($e);
			push @kept, $key, $key3;
		}
	}
	my ($rc, $o, $e) = run_sql($n1, "SET ROLE spy; INSERT INTO public.s1 VALUES ($k[40], 'denied')");
	isnt($rc, 0, 'A: a role without the privilege is still refused');
	like($e, qr/permission denied for table s1\b/, 'A: by the table the role writes, not by a table of the split');
	release($pid, $file);
	like(out('a1'), qr/\n\s*\d+\n/, 'A: the split completes') or diag(out('a1'));
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	is(q1($own, q{SELECT string_agg(DISTINCT relowner::regrole::text, ',') FROM pg_class WHERE relname ~ '^s[12]_b00(02|10)$'}), 'tenant', 'A: the new partitions have the owner of the tables');
	is(q1($_, "SELECT count(*) FROM public.s1 WHERE id IN (" . join(',', @kept) . ")"), scalar(@kept), 'A: the rows that were written during the copy are there, seen from ' . $_->name) for @nodes;
	is(q1($n1, "SELECT count(*) FROM public.s1 WHERE id = $k[1]"), '0', 'A: and the row that was deleted is not');
	is(clean(), '', 'A: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# B. inserts at random keys at a steady rate (P10R-03)
# ---------------------------------------------------------------------------
{
	q1($n1, q{
		CREATE TABLE public.r1 (id int PRIMARY KEY, v text);
		INSERT INTO public.r1 SELECT 2 * g, 'v' || g FROM generate_series(1, 100000) g;
		SELECT kingres.create_distributed_table('public.r1', 'id', bucket_count => 4);
	});
	my $C2 = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.r1'::regclass});
	my $b = 2;
	# odd keys of the bucket in random order: the writers insert them for longer than the ten passes of a copy can last, and are ended when the split is done
	my @ids = split /\n/, q1($n1, "SELECT g FROM generate_series(1, 199999, 2) g WHERE kingres.bucket_for('public.r1'::regclass, g) = $b ORDER BY random() LIMIT 12000");
	cmp_ok(scalar(@ids), '>=', 9000, 'B: odd keys of the bucket are available');
	my @mine;
	my @pids;
	for my $w (0 .. 2)
	{
		my $file = "$tmp/w$w.sql";
		open(my $fh, '>', $file) or die;
		$mine[$w] = [ map { $ids[$_] } grep { $_ % 3 == $w } 0 .. $#ids ];
		print $fh "INSERT INTO public.r1 VALUES ($_, 'w');\nSELECT pg_sleep(0.003);\n" for @{ $mine[$w] };
		close($fh);
		my $pid = fork();
		die 'fork' unless defined $pid;
		if ($pid == 0)
		{
			exec_psql_file($n1, $file, "$tmp/w$w.out");
		}
		push @pids, $pid;
	}
	usleep(1_000_000);
	my ($rc, $o, $e) = run_sql($n1, "SELECT kingres.split_bucket($C2, $b, 2)");
	is($rc, 0, 'B: the split completes under inserts at random keys') or diag($e);
	my $id = last_split();
	kill 'TERM', @pids;
	waitpid($_, 0) for @pids;
	usleep(500_000);
	like(phase_of($id), qr/^(cleaning|done)$/, 'B: and is finished');
	my %present = map { $_ => 1 } split /\n/, q1($n1, q{SELECT id FROM public.r1 WHERE id % 2 = 1});
	my $inserted = 0;
	for my $w (0 .. 2)
	{
		my $first_gap;
		my $beyond = 0;
		for my $i (0 .. $#{ $mine[$w] })
		{
			my $k = $mine[$w][$i];
			if ($present{$k}) { $inserted++; $beyond++ if defined $first_gap; }
			else { $first_gap //= $i; }
		}
		cmp_ok($beyond, '<=', 1, "B: writer $w: the rows it inserted are there, in order, none is missing (stopped at " . ($first_gap // 'the end') . ", $beyond after it)");
	}
	cmp_ok($inserted, '>', 400, "B: and the writers were inserting for the whole copy ($inserted rows)");
	is(q1($n1, "SELECT count(*) FROM (SELECT id FROM public.r1 GROUP BY id HAVING count(*) > 1) d"), '0', 'B: no key twice');
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'B: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# C. a row that a writer holds is copied when the writer ends (P10R-03)
# ---------------------------------------------------------------------------
{
	my $b = 3;
	my $g = owner($b);
	my $own = node_of_group($g);
	my ($key) = keys_of($b, 1, 3000);
	my $want = digest($n1);
	my $locker = sess($own);
	send_sql($locker, "BEGIN; SELECT 'locked=' || id FROM public.s1 WHERE id = $key FOR UPDATE;");
	ok(wait_out($locker, qr/locked=$key/, 20), 'C: a transaction holds a row of the bucket');
	my $h = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres', '-c', "SELECT kingres.split_bucket($C, $b, 2, 'none')" ], '>', \my $sout, '2>', \my $serr);
	my $seen = 0;
	for (1 .. 200)
	{
		eval { $h->pump_nb; 1 };
		if (q1($n1, q{SELECT coalesce(detail, '') FROM kingres.bucket_split ORDER BY split_id DESC LIMIT 1}) =~ /, [1-9]\d* held by writers/) { $seen = 1; last; }
		usleep(100_000);
	}
	ok($seen, 'C: the passes see the row held and go on');
	isnt(phase_of(last_split()), 'cleaning', 'C: the split does not switch while a row is held');
	send_sql($locker, 'COMMIT;');
	$h->finish;
	like($sout, qr/^\d+\s*$/, 'C: the split completes when the writer ends') or diag($serr);
	close_sess($locker);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	is(digest($_), $want, 'C: every row is there, the held one included, seen from ' . $_->name) for @nodes;
	is(clean(), '', 'C: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# D. a deadlock with a writer: the switch is the victim and is tried again (P10R-04)
# ---------------------------------------------------------------------------
{
	my $b = 4;
	my $g = owner($b);
	my $own = node_of_group($g);
	my $want = digest($n1);
	# the writer holds ROW EXCLUSIVE on the parent of s2 (not on the partition: the split prepares its triggers meanwhile)
	my $w = sess($own);
	send_sql($w, "BEGIN; LOCK TABLE ONLY public.s2 IN ROW EXCLUSIVE MODE; SELECT 'first=ok';");
	ok(wait_out($w, qr/first=ok/, 20), 'D: a writer holds s2');
	my ($pid, $file) = stopped($n1, 'split_switch_after_lock',
		"SET kingres_shard.move_switch_timeout = 60000; SELECT kingres.split_bucket($C, $b, 2, 'none')", 'd1');
	ok(wait_for_fault_file("$file.reached", 60), 'D: the switch holds its lock on s1 and waits at the point');
	send_sql($w, "LOCK TABLE ONLY public.s1 IN ROW EXCLUSIVE MODE; SELECT 'second=ok';");
	my $waiting = 0;
	for (1 .. 100)
	{
		if (q1($own, q{SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid WHERE NOT l.granted AND l.mode = 'RowExclusiveLock' AND a.query LIKE '%LOCK TABLE ONLY public.s1%'}) >= 1) { $waiting = 1; last; }
		usleep(50_000);
	}
	ok($waiting, 'D: the writer waits for the switch: the writer waits for the switch and the switch will wait for the writer');
	unlink($file);
	ok(wait_out($w, qr/second=ok/, 30), 'D: the writer gets its second lock: the switch was the one that gave way') or diag(${ $w->{err} });
	unlike(${ $w->{err} }, qr/deadlock/, 'D: the writer saw no deadlock');
	send_sql($w, 'COMMIT;');
	waitpid($pid, 0);
	like(out('d1'), qr/\n\s*\d+\n/, 'D: the split completes (the switch was tried again)') or diag(out('d1'));
	like(slurp_file($own->logfile), qr/deadlock detected/, 'D: the owner found the deadlock');
	close_sess($w);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	is(digest($n1), $want, 'D: every row is there');
	is(clean(), '', 'D: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# E. a writer that keeps the switch out: the attempts run out and the writer is named
# ---------------------------------------------------------------------------
{
	my $b = 6;
	my $g = owner($b);
	my $own = node_of_group($g);
	my $want = digest($n1);
	my ($key) = keys_of($b, 1, 3000);
	my ($pid, $file) = stopped($n1, 'split_before_switch',
		"SET kingres_shard.move_switch_timeout = 1000; SET kingres_shard.move_switch_attempts = 3; SELECT kingres.split_bucket($C, $b, 2)", 'e1');
	ok(wait_for_fault_file("$file.reached", 60), 'E: the split of bucket 6 waits before its switch');
	my $w = sess($own);
	send_sql($w, "BEGIN;");
	send_sql($w, "UPDATE public.s1 SET v = v WHERE id = $key;");
	ok($own->poll_query_until('postgres', q{SELECT count(*) >= 1 FROM pg_locks WHERE locktype = 'relation' AND mode = 'RowExclusiveLock' AND granted AND relation = 'public.s1'::regclass}), 'E: a writer holds a row of the bucket');
	my $t0 = time;
	unlink($file);
	waitpid($pid, 0);
	my $took = time - $t0;
	cmp_ok($took, '<', 30, sprintf('E: the split gives up after its attempts (%.1f s)', $took));
	like(out('e1'), qr/did not get its locks in 3 attempts: pid \d+: (BEGIN; )?UPDATE public\.s1/, 'E: and names the writer') or diag(out('e1'));
	# the writer is still there: it keeps the undo out too (the undo has bounded locks as well), so the record waits for the recovery
	isnt(phase_of(last_split()), 'cleaning', 'E: the split did not switch');
	send_sql($w, 'COMMIT;');
	ok($own->poll_query_until('postgres', q{SELECT count(*) = 0 FROM pg_locks WHERE locktype = 'relation' AND mode = 'RowExclusiveLock' AND granted AND relation = 'public.s1'::regclass}), 'E: the writer has ended');
	close_sess($w);
	my ($rc, $o, $e) = run_sql($n1, 'SELECT kingres.resume_bucket_splits()');
	is($o, '1', 'E: the recovery settles it') or diag($e);
	is(phase_of(last_split()), 'failed', 'E: the split is recorded as failed');
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), 'active', 'E: the bucket is active');
	is(clean(), '', 'E: nothing is left of the split');
	is(digest($n1), $want, 'E: every row is there');
	($rc, $o, $e) = run_sql($n1, "SELECT kingres.split_bucket($C, $b, 2)");
	is($rc, 0, 'E: and the bucket is split when the writer is gone') or diag($e);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	is(digest($n1), $want, 'E: every row is there after it');
	is(clean(), '', 'E: check_cluster() finds nothing');
}

done_testing();
