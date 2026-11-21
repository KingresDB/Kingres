# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/205_split_recovery.pl: the recovery of a split whose session dies inside the switch or inside the undo, the live mark, the bound of the switch, the
# limit on bucket numbers and the check of the bounds before a move (design/shard-phase10.md, "The fixes for the review of phase 10": P10R-01, -05, -06, -08).
#
#   A. the coordinator of a split dies while the owner's switch waits at a fault point (after its locks, after the verification, after the detach, after the
#      attach); the recovery is called meanwhile.  It must wait for the switch, find the partitions switched and finish the split.  Afterwards every row is
#      reachable from every node, a move of the bucket carries all of it and a duplicate key is refused.
#   B. the undo is stopped at each of its fault points and its session or the owner's backend is killed: nothing is half done, the next recovery completes
#      the undo, and a new split of the bucket works.
#   C. an error after the switch releases the live mark of the split, so that the recovery settles it while the session is still open.
#   D. the switch is bounded by move_switch_timeout: a verification that does not end is cut, tried again, and the split fails after the attempts.
#   E. bucket numbers are limited to 9999: a split that would number a bucket above that is refused before anything is recorded.
#   F. a move refuses a bucket whose partitions have other bounds than the bucket map.
#   G. between the switch and the catalog change check_cluster() reports no damage.

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
my @nodes = make_cluster('sw', 3, logical => 1, recovery_interval => 0,
	extra => "max_connections = 100\nkingres_shard.node_timeout = 5\nkingres_shard.move_cleanup_delay = 1\n");
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
my $C = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.s1'::regclass});
sub digest
{
	my ($n) = @_;
	return q1($n, q{SELECT (SELECT count(*) || ',' || sum(id) || ',' || sum(length(v)) FROM public.s1) || '|' || (SELECT count(*) || ',' || sum(w) FROM public.s2)
	                || '|' || (SELECT count(*) FROM public.s1 s JOIN public.s2 t USING (id))});
}
my $base = digest($n1);
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
sub bounds_on
{
	my ($n, $tbl) = @_;
	return q1($n, qq{
		SELECT coalesce(string_agg(regexp_replace(c.relname, '^.*_b', '') || '=' || regexp_replace(pg_get_expr(c.relpartbound, c.oid), '.*modulus (\\d+), remainder (\\d+).*', '\\1/\\2'), ' ' ORDER BY (regexp_match(c.relname, '_b([0-9]+)\$'))[1]::int), '')
		  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'public.$tbl'::regclass});
}
$_->safe_psql('postgres', q{
	CREATE FUNCTION public.misplaced(tbl regclass, coloc int) RETURNS bigint LANGUAGE plpgsql AS $f$
	DECLARE r record; n bigint; total bigint := 0;
	BEGIN
	  FOR r IN SELECT i.inhrelid::regclass AS part, (regexp_match(c.relname, '_b([0-9]+)$'))[1]::int AS b
	             FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = tbl LOOP
	    EXECUTE format('SELECT count(*) FROM %s WHERE kingres.bucket_for(%s, id) <> %s', r.part, coloc, r.b) INTO n;
	    total := total + n;
	  END LOOP;
	  RETURN total;
	END $f$}) for @nodes;
sub verify_all
{
	my ($what) = @_;
	is(clean(), '', "$what: check_cluster() finds nothing");
	for my $n (@nodes)
	{
		is(digest($n), $base, "$what: node " . $n->name . " reads every row");
		is(q1($n, "SELECT public.misplaced('public.s1'::regclass, $C) + public.misplaced('public.s2'::regclass, $C)"), '0', "$what: node " . $n->name . ": every row is in the partition of its bucket");
	}
}

# a session that is kept open
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

# a session on $node that runs $sql with a fault point that waits for a file; the child is the psql process
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
sub out { my ($name) = @_; open(my $fh, '<', "$tmp/out_$name") or return ''; local $/; my $s = <$fh>; close($fh); return $s; }
sub terminate_like
{
	my ($node, $like) = @_;
	my $pid = q1($node, qq{SELECT pid FROM pg_stat_activity WHERE query LIKE '$like' AND pid <> pg_backend_pid() AND state = 'active' AND query NOT LIKE '%pg_stat_activity%' LIMIT 1});
	q1($node, "SELECT pg_terminate_backend($pid)") if $pid ne '';
	return $pid;
}

is(clean(), '', 'the cluster is clean at the start');

# ---------------------------------------------------------------------------
# A. the coordinator dies while the owner's switch is at a fault point; the recovery must wait for the switch (P10R-01)
# ---------------------------------------------------------------------------
{
	my $i = 0;
	for my $point (qw(split_switch_after_lock split_switch_after_verify split_switch_after_detach split_switch_after_attach))
	{
		my $b = $i++;
		my $g = owner($b);
		my $own = node_of_group($g);
		my ($pid, $file) = stopped($n1, $point, "SELECT kingres.split_bucket($C, $b, 2)", "a$i");
		ok(wait_for_fault_file("$file.reached", 60), "A($point): the switch of bucket $b reached the point on the owner");
		my $id = last_split();
		is(phase_of($id), 'switching', "A($point): the split is switching");
		terminate_like($n1, '%split_bucket(%');
		waitpid($pid, 0);
		my $rs = sess($n1);
		send_sql($rs, 'SELECT kingres.resume_bucket_splits();');
		usleep(2_000_000);
		is(phase_of($id), 'switching', "A($point): the recovery waits for the switch that is still running (it does not undo it)");
		unlike(${ $rs->{out} }, qr/^\d+$/m, "A($point): and has not answered");
		unlink($file);
		ok(wait_out($rs, qr/^\d+\s*$/m, 60), "A($point): the recovery answers once the switch has committed") or diag(${ $rs->{err} });
		is(${ $rs->{out} } =~ /(\d+)/ ? $1 : '', '1', "A($point): and has settled the split");
		close_sess($rs);
		like(phase_of($id), qr/^(cleaning|done)$/, "A($point): the split is finished, not failed");
		is(q1($n1, "SELECT modulus || ':' || state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), '16:active', "A($point): the bucket has the children's modulus");
		is(q1($n1, "SELECT modulus || ':' || state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = " . ($b + 8)), '16:active', "A($point): and its twin is in the map");
		like(bounds_on($own, 's1'), qr/\b000$b=16\/$b\b/, "A($point): the partition of s1 has the new bounds on the owner");
		like(bounds_on($own, 's2'), qr/\b00@{[ sprintf('%02d', $b + 8) ]}=16\/@{[ $b + 8 ]}\b/, "A($point): and so has the twin of s2");
		q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
		verify_all("A($point)");
		# a move of the bucket carries every row: the rows of the twin would stay behind if the partitions and the map disagreed
		my $tgt = ($g + 1) % 3;
		my ($rc, $o, $e) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $tgt)");
		is($rc, 0, "A($point): the bucket moves") or diag($e);
		q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
		verify_all("A($point) after the move");
		my ($twin) = split /\n/, q1($n1, "SELECT id FROM public.s1 WHERE kingres.bucket_for($C, id) = " . ($b + 8) . " LIMIT 1");
		($rc, $o, $e) = run_sql($n2, "INSERT INTO public.s1 VALUES ($twin, 'dup')");
		isnt($rc, 0, "A($point): a key that exists is refused, from every node");
		like($e, qr/duplicate key/, "A($point): as a duplicate key");
	}
}

# ---------------------------------------------------------------------------
# B. the undo is cut at each of its points: by the death of the session that runs it, and by the death of the owner's backend
# ---------------------------------------------------------------------------
{
	my $b = 4;
	my $g = owner($b);
	my $own = node_of_group($g);
	my $round = 0;
	for my $point (qw(split_undo_after_lock split_undo_after_triggers split_undo_after_children))
	{
		for my $kind (qw(owner session))
		{
			$round++;
			my ($pid, $file) = stopped($n1, 'split_after_backfill', "SELECT kingres.split_bucket($C, $b, 2)", "b$round");
			ok(wait_for_fault_file("$file.reached", 60), "B($point, $kind): a split of bucket $b waits before its switch");
			my $id = last_split();
			terminate_like($n1, '%split_bucket(%');
			unlink($file);
			waitpid($pid, 0);
			my ($rpid, $rfile) = stopped($n1, $point, 'SELECT kingres.resume_bucket_splits()', "r$round");
			ok(wait_for_fault_file("$rfile.reached", 60), "B($point, $kind): the undo reached the point on the owner");
			if ($kind eq 'owner')
			{
				ok(terminate_like($own, '%internal_split_undo(%') ne '', "B($point, $kind): the owner's backend that runs the undo is killed");
			}
			else
			{
				ok(terminate_like($n1, '%resume_bucket_splits()%') ne '', "B($point, $kind): the session that runs the recovery is killed");
			}
			unlink($rfile);
			waitpid($rpid, 0);
			usleep(500_000);
			if ($kind eq 'owner')
			{
				# the undo is one transaction: what it did before the kill is rolled back
				is(q1($own, q{SELECT count(*) FROM pg_trigger WHERE tgname = 'ks_split_capture_row'}), '2', "B($point, $kind): the capture triggers are still there");
				isnt(phase_of($id), 'failed', "B($point, $kind): the split is not recorded as failed");
			}
			my ($rc, $o, $e) = run_sql($n1, 'SELECT kingres.resume_bucket_splits()');
			is($o, '1', "B($point, $kind): the next recovery settles it") or diag($e);
			is(phase_of($id), 'failed', "B($point, $kind): the record says failed");
			is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), 'active', "B($point, $kind): the bucket is active");
			is(q1($own, q{SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'ks_split_capture%'}), '0', "B($point, $kind): no trigger is left");
			is(q1($own, q{SELECT count(*) FROM pg_proc WHERE proname LIKE 'split_capture_%'}), '0', "B($point, $kind): no trigger function is left");
			is(q1($own, q{SELECT count(*) FROM pg_class WHERE relname ~ '_s[0-9]+_[0-9]+$' AND relkind = 'r'}), '0', "B($point, $kind): no child is left");
			is(clean(), '', "B($point, $kind): check_cluster() finds nothing");
		}
	}
	my ($rc, $o, $e) = run_sql($n1, "SELECT kingres.split_bucket($C, $b, 2)");
	is($rc, 0, 'B: the bucket is split afterwards') or diag($e);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	verify_all('B');
}

# ---------------------------------------------------------------------------
# C. an error after the switch releases the live mark (P10R-06)
# ---------------------------------------------------------------------------
{
	my $b = 5;
	my $s = sess($n1);
	send_sql($s, "SET kingres_shard.fault_injection = 'split_after_switch:error';");
	send_sql($s, "SELECT kingres.split_bucket($C, $b, 2);");
	ok(wait_out($s, qr/injected failure at split_after_switch/, 60), 'C: the split fails after its switch') or diag(${ $s->{err} });
	my $id = last_split();
	is(phase_of($id), 'switching', 'C: the record says switching: the catalog was not written');
	send_sql($s, "RESET kingres_shard.fault_injection;");
	send_sql($s, "SELECT 'alive=' || kingres.internal_split_alive($id);");
	ok(wait_out($s, qr/alive=\w/, 20), 'C: the session asks whether the split is alive');
	like(${ $s->{out} }, qr/alive=f/, 'C: it is not: the mark was released with the error');
	send_sql($s, q{SELECT 'locks=' || count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid();});
	ok(wait_out($s, qr/locks=\d/, 20), 'C: and the session holds no advisory lock');
	like(${ $s->{out} }, qr/locks=0/, 'C: none is leaked');
	# the session is still open
	my ($rc, $o, $e) = run_sql($n1, 'SELECT kingres.resume_bucket_splits()');
	is($o, '1', 'C: the recovery settles the split while the session of the split is open') or diag($e);
	like(phase_of($id), qr/^(cleaning|done)$/, 'C: it is finished');
	close_sess($s);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	verify_all('C');
}

# ---------------------------------------------------------------------------
# D. the switch is bounded by move_switch_timeout (P10R-08)
# ---------------------------------------------------------------------------
{
	my $b = 6;
	my $t0 = time;
	my ($pid, $file) = stopped($n1, 'split_switch_after_verify',
		"SET kingres_shard.move_switch_timeout = 1000; SET kingres_shard.move_switch_attempts = 3; SELECT kingres.split_bucket($C, $b, 2)", 'd1');
	my $done = 0;
	for (1 .. 400) { if (waitpid($pid, POSIX::WNOHANG) > 0) { $done = 1; last; } usleep(100_000); }
	my $took = time - $t0;
	unlink($file);
	waitpid($pid, 0) unless $done;
	ok($done, sprintf('D: the split gives up although the verification would not end (%.1f s)', $took));
	like(out('d1'), qr/did not finish in 3 attempts/, 'D: and says why') or diag(out('d1'));
	my $id = last_split();
	is(phase_of($id), 'failed', 'D: it is undone and recorded');
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), 'active', 'D: the bucket is active');
	verify_all('D');
}

# ---------------------------------------------------------------------------
# E. bucket numbers stay below 10000 (P10R-05)
# ---------------------------------------------------------------------------
{
	my ($rc, $o, $e) = run_sql($n1, "SELECT kingres.split_bucket($C, 7, 16)");
	is($rc, 0, 'E: bucket 7 is split in 16 (modulus 128)') or diag($e);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	($rc, $o, $e) = run_sql($n1, "SELECT kingres.split_bucket($C, 127, 16)");
	is($rc, 0, 'E: bucket 127 is split in 16 (modulus 2048)') or diag($e);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	my $before = q1($n1, q{SELECT count(*) FROM kingres.bucket_split});
	($rc, $o, $e) = run_sql($n1, "SELECT kingres.split_bucket($C, 2047, 16)");
	isnt($rc, 0, 'E: bucket 2047 cannot be split in 16: the new buckets would be numbered up to 32767');
	like($e, qr/bucket numbers are limited to 9999/, 'E: the message says why');
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_split}), $before, 'E: no record was made');
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 2047"), 'active', 'E: the bucket is active');
	($rc, $o, $e) = run_sql($n1, "SELECT kingres.split_bucket($C, 2047, 2)");
	is($rc, 0, 'E: a split in 2 gives 4095 and is allowed') or diag($e);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	verify_all('E');
}

# ---------------------------------------------------------------------------
# F. a move refuses a bucket whose partitions are not the bucket map's (P10R-01)
# ---------------------------------------------------------------------------
{
	my $b = 6;
	my $g = owner($b);
	# the map and the partitions disagree: the map says modulus 16 for a bucket that is a partition of modulus 8
	q1($n1, "UPDATE kingres.bucket_map SET modulus = 16 WHERE colocation_id = $C AND bucket = $b AND modulus = 8");
	my ($rc, $o, $e) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, " . (($g + 1) % 3) . ")");
	q1($n1, "UPDATE kingres.bucket_map SET modulus = 8 WHERE colocation_id = $C AND bucket = $b AND modulus = 16");
	isnt($rc, 0, 'F: the move is refused');
	like($e, qr/has the bounds "FOR VALUES WITH \(modulus 8, remainder 6\)" and the bucket map says "FOR VALUES WITH \(modulus 16, remainder 6\)"/, 'F: and says what differs') or diag($e);
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"), 'active', 'F: nothing was changed');
	verify_all('F');
}

# ---------------------------------------------------------------------------
# G. between the switch and the catalog change check_cluster() finds no damage (P10R-12)
# ---------------------------------------------------------------------------
{
	my $b = 6;
	my ($pid, $file) = stopped($n1, 'split_after_switch', "SELECT kingres.split_bucket($C, $b, 2)", 'g1');
	ok(wait_for_fault_file("$file.reached", 60), 'G: the split of bucket 6 is switched on the owner and waits before the catalog change');
	my $id = last_split();
	is(phase_of($id), 'switching', 'G: the record says switching');
	like(bounds_on(node_of_group(owner($b)), 's1'), qr/\b0006=16\/6\b/, 'G: the partition has the new bounds while the map has the old ones');
	is(clean(), '', 'G: and check_cluster() says nothing (the group has a split that is not finished)');
	unlink($file);
	waitpid($pid, 0);
	like(out('g1'), qr/\n\s*\d+\n/, 'G: the split completes');
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	verify_all('G');
}

done_testing();
