# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/204_split_bucket.pl: kingres.split_bucket() (design/shard-phase10.md, section 3; increment P10.5).
#
#   A. a split in two, in four, a child again: layout, bounds on the owning node, every row from every node, check_cluster(), the orphans
#   B. writers during a split: inserts, updates and deletes from every node while it runs; the table equals the model afterwards
#   C. bypasses during the copy: a direct write into the old partition, one with session_replication_role = replica, a COPY into it
#   D. refusals: no primary key, a user-partitioned table, a bucket that is not active, the same group twice, DDL during a split, parts, a move of the bucket
#   E. cancel; a killed session before the switch (undone), after it (finished), the recovery worker alone
#   F. a split bucket moves, and a table made later has its layout
#   G. the recovery worker leaves a split alone between its record and the live mark (fault point split_after_begin)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use POSIX ();
use Test::More;
use Time::HiRes qw(usleep);
use KingresShardTest qw(make_cluster run_sql start_psql finish_psql wait_for_fault_file);

my $tmp = PostgreSQL::Test::Utils::tempdir();
my @nodes = make_cluster('sp', 3, logical => 1, recovery_interval => 2,
	extra => "max_connections = 100\nkingres_shard.node_timeout = 5\nkingres_shard.move_cleanup_delay = 1\n");
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

q1($n1, q{
	CREATE TABLE public.s1 (id int PRIMARY KEY, v text, k2 int NOT NULL DEFAULT 0);
	CREATE TABLE public.s2 (id int PRIMARY KEY, w int NOT NULL);
	INSERT INTO public.s1 SELECT g, 'v' || g, g % 7 FROM generate_series(1, 3000) g;
	INSERT INTO public.s2 SELECT g, g * 3 FROM generate_series(1, 3000) g;
	SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 8);
	SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s1');
});
my $C = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.s1'::regclass});
sub digest
{
	my ($n) = @_;
	return q1($n, q{SELECT (SELECT count(*) || ',' || sum(id) || ',' || sum(length(v)) || ',' || sum(k2) FROM public.s1) || '|' || (SELECT count(*) || ',' || sum(w) FROM public.s2)
	                || '|' || (SELECT count(*) FROM public.s1 s JOIN public.s2 t USING (id))});
}
my $base = digest($n1);
sub owner { my ($b) = @_; return q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"); }
sub layout { return q1($n1, "SELECT string_agg(bucket || '/' || modulus || ':g' || group_id || ':' || state, ' ' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = $C"); }
sub epoch { return q1($n1, "SELECT placement_epoch FROM kingres.colocation WHERE colocation_id = $C"); }
sub clean { return q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || coalesce(object, ''), '; '), '') FROM kingres.check_cluster()}); }
sub bounds
{
	my ($tbl) = @_;
	return join(' ', grep { $_ ne '' } map { q1($_, qq{
		SELECT coalesce(string_agg(regexp_replace(c.relname, '^.*_b', '') || '=' || regexp_replace(pg_get_expr(c.relpartbound, c.oid), '.*modulus (\\d+), remainder (\\d+).*', '\\1/\\2'), ' ' ORDER BY (regexp_match(c.relname, '_b([0-9]+)\$'))[1]::int), '')
		  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'public.$tbl'::regclass}) } @nodes);
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
sub misplaced { my ($n, $t) = @_; return q1($n, "SELECT public.misplaced('public.$t'::regclass, $C)"); }
sub verify_all
{
	my ($what) = @_;
	is(clean(), '', "$what: check_cluster() finds nothing");
	for my $n (@nodes)
	{
		is(digest($n), $base, "$what: node " . $n->name . " reads every row");
		is(misplaced($n, 's1') . misplaced($n, 's2'), '00', "$what: node " . $n->name . ": every row is in the partition of its bucket");
	}
}
# the sessions that wait at a fault point
sub stopped
{
	my ($point, $sql, $name) = @_;
	my $file = "$tmp/wait_$name";
	open(my $fh, '>', $file) or die;
	close($fh);
	my $pid = fork();
	die 'fork' unless defined $pid;
	if ($pid == 0)
	{
		my $s = "SET kingres_shard.fault_injection = '$point:wait:$file'; $sql";
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$s\" > $tmp/out_$name 2>&1");
		POSIX::_exit(0);
	}
	return ($pid, $file);
}
sub release { my ($pid, $file) = @_; unlink($file); waitpid($pid, 0); }
sub out { my ($name) = @_; open(my $fh, '<', "$tmp/out_$name") or return ''; local $/; my $s = <$fh>; close($fh); return $s; }

is(clean(), '', 'the cluster is clean at the start');

# ---------------------------------------------------------------------------
# A. splits
# ---------------------------------------------------------------------------
{
	my $g = owner(2);
	my $e = epoch();
	# the recovery worker drops the orphans of a finished split after move_cleanup_delay (1 s for the other sections); here the test looks at them first, which a loaded machine
	# does later than that, so the delay is long until they are checked
	q1($n1, q{ALTER SYSTEM SET kingres_shard.move_cleanup_delay = 600; SELECT pg_reload_conf()});
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, 2, 2)");
	is($rc, 0, 'A: split_bucket(2, 2)') or diag($err);
	my $id = $out; $id =~ s/\s+//g;
	is(q1($n1, "SELECT phase FROM kingres.bucket_split WHERE split_id = $id"), 'cleaning', 'A: the split is in the cleaning phase');
	is(q1($n1, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $C"), '9', 'A: nine buckets');
	like(layout(), qr{\b2/16:g$g:active\b}, 'A: bucket 2 has modulus 16');
	like(layout(), qr{\b10/16:g$g:active\b}, 'A: and bucket 10, its twin, is on the same group');
	is(q1($n1, "SELECT max_modulus FROM kingres.colocation WHERE colocation_id = $C"), '16', 'A: max_modulus is 16');
	is(epoch(), $e + 1, 'A: the epoch counts the switch');
	is(q1($_, "SELECT max_modulus || ',' || placement_epoch FROM kingres.colocation WHERE colocation_id = $C"), '16,' . ($e + 1), 'A: and every node has them') for ($n2, $n3);
	like(all_bounds_s1(), qr{\b0002=16/2\b}, 'A: the partition of bucket 2 has the new bounds');
	like(all_bounds_s1(), qr{\b0010=16/10\b}, 'A: and so has the new one');
	like(all_bounds_s2(), qr{\b0010=16/10\b}, 'A: in the other table of the group too');
	verify_all('A1');
	is(q1($nodes[$g], q{SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan_s%' AND relkind = 'r'}), '2', 'A: the old partitions wait as orphans on the owner');
	is(q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)}), '1', 'A: the cleanup drops them');
	is(q1($nodes[$g], q{SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'orphan_s%'}), '0', 'A: they are gone');
	q1($n1, q{ALTER SYSTEM RESET kingres_shard.move_cleanup_delay; SELECT pg_reload_conf()});
	is(q1($n1, "SELECT phase FROM kingres.bucket_split WHERE split_id = $id"), 'done', 'A: the split is done');
	verify_all('A2');

	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, 4, 4)");
	is($rc, 0, 'A: split_bucket(4, 4)') or diag($err);
	is(q1($n1, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $C AND modulus = 32"), '4', 'A: four buckets of modulus 32');
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	verify_all('A3');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, 10, 2)");
	is($rc, 0, 'A: a child is split again') or diag($err);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	is(q1($n1, "SELECT string_agg(bucket || '/' || modulus, ' ' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = $C"),
	   '0/8 1/8 2/16 3/8 4/32 5/8 6/8 7/8 10/32 12/32 20/32 26/32 28/32', 'A: the moduli 8, 16 and 32 side by side');
	verify_all('A4');
}
sub all_bounds_s1 { return bounds('s1'); }
sub all_bounds_s2 { return bounds('s2'); }

# ---------------------------------------------------------------------------
# B. writers during a split.  The model is what the table must hold afterwards.
# ---------------------------------------------------------------------------
my %model;     # id => v of s1
$model{$_->[0]} = $_->[1] for map { [ split /\|/ ] } split /\n/, q1($n1, q{SELECT id || '|' || v FROM public.s1});
sub keys_of { my ($b, $lo, $hi) = @_; return split /\n/, q1($n1, "SELECT g FROM generate_series($lo, $hi) g WHERE kingres.bucket_for('public.s1'::regclass, g) = $b ORDER BY g"); }
my $wn = 0;
sub write_one
{
	my ($sql, $apply) = @_;
	my $node = $nodes[$wn++ % 3];
	for my $try (1 .. 5)
	{
		my ($rc, $out, $err) = run_sql($node, $sql);
		if ($rc == 0) { $apply->(); return 1; }
		last unless $err =~ /KS001|stale|placement|deadlock/i;
		usleep(200_000);
	}
	return 0;
}
sub model_check
{
	my ($what) = @_;
	my %got = map { my ($k, $v) = split /\|/; ($k => $v) } split /\n/, q1($n1, q{SELECT id || '|' || v FROM public.s1});
	my $bad = 0;
	my $shown = 0;
	for my $k (sort { $a <=> $b } keys %model) { unless (defined $got{$k} && $got{$k} eq $model{$k}) { $bad++; diag("id $k: model '$model{$k}', table '" . ($got{$k} // 'missing') . "'") if $shown++ < 8; } }
	for my $k (sort { $a <=> $b } keys %got) { unless (exists $model{$k}) { $bad++; diag("id $k: not in the model, table '$got{$k}'") if $shown++ < 8; } }
	is($bad, 0, "$what: the table equals the model (" . scalar(keys %model) . " rows)");
	is(q1($n1, q{SELECT count(*) FROM public.s1}), scalar(keys %model), "$what: and has no row twice or missing");
	$base = digest($n1);
}
sub do_writes
{
	my ($b, $round) = @_;
	my @k = keys_of($b, 1, 3000);
	for my $i (0 .. 24)
	{
		my $key = $k[$i * 3 % @k];
		write_one("UPDATE public.s1 SET v = 'u$round-$i' WHERE id = $key", sub { $model{$key} = "u$round-$i" if exists $model{$key}; });
	}
	for my $i (0 .. 14)
	{
		my $key = $k[($i * 5 + 1) % @k];
		write_one("DELETE FROM public.s1 WHERE id = $key", sub { delete $model{$key}; }) if exists $model{$key};
	}
	for my $i (1 .. 20)
	{
		my $key = 10000 * $round + $i * 8 + $b;
		next unless q1($n1, "SELECT kingres.bucket_for('public.s1'::regclass, $key)") == $b;
		write_one("INSERT INTO public.s1 VALUES ($key, 'n$round-$i', 0)", sub { $model{$key} = "n$round-$i"; });
	}
	# the same key deleted and inserted again
	my $key = $k[7];
	write_one("DELETE FROM public.s1 WHERE id = $key; INSERT INTO public.s1 VALUES ($key, 'again$round', 1)", sub { $model{$key} = "again$round"; });
}
{
	# between the triggers and the backfill
	my ($pid, $file) = stopped('split_after_triggers', "SELECT kingres.split_bucket($C, 5, 2, 'none')", 'b1');
	ok(wait_for_fault_file("$file.reached", 60), 'B: the split of bucket 5 reached the point after its triggers');
	do_writes(5, 1);
	release($pid, $file);
	like(out('b1'), qr/\n\s+\d+\n\(1 row\)/, 'B: the split completes') or diag(out('b1'));
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	model_check('B1');
	is(clean(), '', 'B1: check_cluster() finds nothing');
	# between the backfill and the switch
	($pid, $file) = stopped('split_after_backfill', "SELECT kingres.split_bucket($C, 6, 4, 'none')", 'b2');
	ok(wait_for_fault_file("$file.reached", 60), 'B: the split of bucket 6 reached the point after its backfill');
	do_writes(6, 2);
	release($pid, $file);
	like(out('b2'), qr/\n\s+\d+\n\(1 row\)/, 'B: the split completes') or diag(out('b2'));
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	model_check('B2');
	# with no pause: writers go on while the split runs
	my $h = start_psql($n1, "SELECT kingres.split_bucket($C, 7, 2, 'none')");
	my $round = 3;
	my $writes = 0;
	while ($h->{h}->pumpable && $round < 40)
	{
		eval { $h->{h}->pump_nb; 1 };
		do_writes(7, $round++);
		$writes++;
	}
	my ($sout, $serr) = finish_psql($h);
	like($sout, qr/^\d+\s*$/, "B: the split of bucket 7 completes with $writes rounds of writes meanwhile") or diag($serr);
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	model_check('B3');
	verify_all('B4');
}

# ---------------------------------------------------------------------------
# C. bypasses during the copy: whatever writes the old partition, the children get it
# ---------------------------------------------------------------------------
{
	my $b = 0;
	my $g = owner($b);
	my @k = keys_of($b, 20000, 22000);
	my ($pid, $file) = stopped('split_after_backfill', "SELECT kingres.split_bucket($C, $b, 2, 'none')", 'c1');
	ok(wait_for_fault_file("$file.reached", 60), 'C: the split of bucket 0 reached the point after its backfill');
	my ($rc, $out, $err) = run_sql($nodes[$g], "INSERT INTO public.s1_b0000 VALUES ($k[0], 'direct', 0)");
	like($err, qr/cannot write a bucket partition of a distributed table directly/, 'C: a direct write into the old partition is refused');
	# a superuser who switched the routing off gets past that check: the trigger still copies the write
	($rc, $out, $err) = run_sql($nodes[$g], "SET kingres_shard.enable_routing = off; INSERT INTO public.s1_b0000 VALUES ($k[0], 'direct', 0)");
	is($rc, 0, 'C: a direct insert into the old partition with the routing off') or diag($err);
	$model{$k[0]} = 'direct';
	($rc, $out, $err) = run_sql($nodes[$g], "SET kingres_shard.enable_routing = off; SET session_replication_role = replica; INSERT INTO public.s1_b0000 VALUES ($k[1], 'replica', 0); UPDATE public.s1_b0000 SET v = 'replica2' WHERE id = $k[1]");
	is($rc, 0, 'C: an insert and an update with session_replication_role = replica') or diag($err);
	$model{$k[1]} = 'replica2';
	open(my $fh, '>', "$tmp/copy.csv") or die;
	print $fh "$k[$_],copied$_,0\n" for 2 .. 6;
	close($fh);
	($rc, $out, $err) = run_sql($nodes[$g], "SET kingres_shard.enable_routing = off; COPY public.s1_b0000 FROM '$tmp/copy.csv' WITH (FORMAT csv)");
	is($rc, 0, 'C: a COPY into the old partition') or diag($err);
	$model{$k[$_]} = "copied$_" for 2 .. 6;
	my $s2_b0 = q1($n1, "SELECT count(*) FROM public.s2 WHERE kingres.bucket_for($C, id) = 0");
	($rc, $out, $err) = run_sql($nodes[$g], "SET kingres_shard.enable_routing = off; TRUNCATE public.s2_b0000; INSERT INTO public.s2_b0000 SELECT g, g * 3 FROM generate_series(1, 3000) g WHERE kingres.bucket_for($C, g) = 0 AND g % 10 = 0");
	is($rc, 0, 'C: a TRUNCATE of the old partition of the other table and a partial reload of it') or diag($err);
	my $s2_keep = q1($nodes[$g], q{SELECT count(*) FROM public.s2_b0000});
	# a write of the table itself with session_replication_role = replica (triggers off, but not ENABLE ALWAYS ones)
	my @k2 = keys_of($b, 23000, 24000);
	($rc, $out, $err) = run_sql($nodes[$g], "SET session_replication_role = replica; INSERT INTO public.s1 VALUES ($k2[0], 'replica-routed', 0)");
	like($err, qr/cannot be written while session_replication_role is "replica"/, 'C: a write of the table itself with session_replication_role = replica is refused');
	release($pid, $file);
	like(out('c1'), qr/\n\s+\d+\n\(1 row\)/, 'C: the split completes') or diag(out('c1'));
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	model_check('C');
	is(q1($n1, q{SELECT count(*) FROM public.s2}), 3000 - $s2_b0 + $s2_keep, 'C: the other table has the rows of its reload and no more (the truncate reached the children)');
	is(clean(), '', 'C: check_cluster() finds nothing');
}
{
	# the children are not what they should be when the switch looks: a row of the partition missing from its child, a stray row in the other;
	# the counts agree, so only verify 'full' brings them in line
	my $b = 3;
	my $g = owner($b);
	my ($pid, $file) = stopped('split_after_backfill', "SELECT kingres.split_bucket($C, $b, 2, 'full')", 'c2');
	ok(wait_for_fault_file("$file.reached", 60), 'C: the split of bucket 3 reached the point after its backfill');
	my $sid = q1($n1, q{SELECT max(split_id) FROM kingres.bucket_split});
	my @inb = keys_of($b, 1, 3000);
	my ($miss) = grep { exists $model{$_} } @inb;
	my $stray = (grep { !exists $model{$_} } keys_of($b, 30000, 31000))[0];
	for my $r (3, 11)
	{
		run_sql($nodes[$g], "DELETE FROM public.s1_s${sid}_$r WHERE id = $miss");
		run_sql($nodes[$g], "INSERT INTO public.s1_s${sid}_$r VALUES ($stray, 'stray', 0)");
		# a row that exists in the child with other values (P10R-14): 'full' makes the children equal in content
		my $other = q1($nodes[$g], "SELECT id FROM public.s1_s${sid}_$r WHERE id <> $miss ORDER BY id LIMIT 1");
		run_sql($nodes[$g], "UPDATE public.s1_s${sid}_$r SET v = 'wrong value' WHERE id = $other");
	}
	release($pid, $file);
	like(out('c2'), qr/\n\s+\d+\n\(1 row\)/, 'C: the split completes') or diag(out('c2'));
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	model_check('C2');
is(q1($nodes[$g], q{SELECT coalesce(sum(n_tup_upd), 0) FROM (SELECT pg_stat_force_next_flush()) f, pg_stat_all_tables WHERE relname IN ('s1_b0003', 's1_b0011')}), 2 * 2, "C2: verify 'full' wrote only the rows whose values differed (the two wrong values, one per child; the equal rows were not rewritten, P10F-02)");
	is(clean(), '', 'C2: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# D. refusals
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err);
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, 1, 3)");
	like($err, qr/parts must be 2, 4, 8 or 16/, 'D: three parts are refused');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, 1, 2, 'maybe')");
	like($err, qr/verify must be count, full or none/, 'D: so is an unknown verify');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, 99, 2)");
	like($err, qr/bucket 99 of colocation group \d+ does not exist/, 'D: and a bucket that does not exist');
	($rc, $out, $err) = run_sql($n2, "SELECT kingres.split_bucket($C, 1, 2)");
	isnt($rc, 0, 'D: only the map group primary splits');
	q1($n1, q{CREATE ROLE eve LOGIN});
	($rc, $out, $err) = run_sql($n1, "SET ROLE eve; SELECT kingres.split_bucket($C, 1, 2)");
	isnt($rc, 0, 'D: only a superuser splits');
	# a table without a primary key, a table partitioned by the user
	q1($n1, q{CREATE TABLE public.np (id int, v text); SELECT kingres.create_distributed_table('public.np', 'id', colocate_with => 'public.s1')});
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, 1, 2)");
	like($err, qr/table public\.np has no primary key/, 'D: a table without a primary key refuses the split');
	q1($n1, q{SELECT kingres.undistribute_table('public.np')});
	q1($n1, q{CREATE TABLE public.up (id int, d date, PRIMARY KEY (id, d)) PARTITION BY RANGE (d); CREATE TABLE public.up_2024 PARTITION OF public.up FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
	          SELECT kingres.create_distributed_table('public.up', 'id', colocate_with => 'public.s1')});
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, 1, 2)");
	like($err, qr/partitioned by the user/, 'D: a table partitioned by the user refuses it');
	q1($n1, q{DROP TABLE public.up});
	is(clean(), '', 'D: check_cluster() finds nothing, nothing was left');
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_split WHERE phase NOT IN ('done', 'failed', 'cancelled')}), '0', 'D: and no record of an unfinished split');
	# a bucket that is moving
	my $b = 3;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my ($pid, $file) = stopped('move_after_catchup', "SELECT kingres.move_bucket($C, $b, $tgt)", 'd1');
	ok(wait_for_fault_file("$file.reached", 60), 'D: a move of bucket 3 waits');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, $b, 2)");
	like($err, qr/is in state "moving"/, 'D: the bucket that moves is not split');
	release($pid, $file);
	ok($n1->poll_query_until('postgres', q{SELECT phase IN ('cleaning', 'done') FROM kingres.bucket_move ORDER BY move_id DESC LIMIT 1}), 'D: the move completes');
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	# a split that runs: a second one, a move of its bucket, DDL on its group
	($pid, $file) = stopped('split_after_triggers', "SELECT kingres.split_bucket($C, 1, 2)", 'd2');
	ok(wait_for_fault_file("$file.reached", 60), 'D: a split of bucket 1 waits');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, 2, 2)");
	like($err, qr/already being split/, 'D: the colocation group is not split twice');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, 1, " . ((owner(1) + 1) % 3) . ")");
	like($err, qr/is in state "splitting"/, 'D: the bucket that is split does not move');
	q1($n1, q{CREATE TABLE public.late (id int PRIMARY KEY)});
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.late', 'id', colocate_with => 'public.s1')});
	like($err, qr/is being split/, 'D: a table is not added to the group meanwhile');
	my $sid = q1($n1, q{SELECT split_id FROM kingres.bucket_split WHERE phase NOT IN ('done', 'failed', 'cancelled')});
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.cancel_bucket_split($sid)");
	is($rc, 0, 'D: the split is cancelled') or diag($err);
	release($pid, $file);
	like(out('d2'), qr/was cancelled/, 'D: and its session says so');
	is(q1($n1, q{SELECT phase FROM kingres.bucket_split ORDER BY split_id DESC LIMIT 1}), 'cancelled', 'D: the record says cancelled');
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 1"), 'active', 'D: the bucket is active again');
	is(clean(), '', 'D: check_cluster() finds nothing: no trigger, no child is left');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.cancel_bucket_split($sid)");
	like($err, qr/is cancelled/, 'D: a split that has ended cannot be cancelled');
	q1($n1, q{DROP TABLE public.late});
	model_check('D');
}

# ---------------------------------------------------------------------------
# E. a killed session: before the switch the worker undoes the split, after it the worker finishes it
# ---------------------------------------------------------------------------
sub kill_split
{
	my $pid = q1($n1, q{SELECT pid FROM pg_stat_activity WHERE query LIKE '%split_bucket(%' AND pid <> pg_backend_pid() AND state = 'active' AND query NOT LIKE '%pg_stat_activity%' LIMIT 1});
	q1($n1, "SELECT pg_terminate_backend($pid)");
}
{
	my ($pid, $file) = stopped('split_after_backfill', "SELECT kingres.split_bucket($C, 2, 2)", 'e1');
	ok(wait_for_fault_file("$file.reached", 60), 'E: a split of bucket 2 waits before its switch');
	kill_split();
	unlink($file);
	waitpid($pid, 0);
	ok($n1->poll_query_until('postgres', q{SELECT phase = 'failed' FROM kingres.bucket_split ORDER BY split_id DESC LIMIT 1}), 'E: the worker undoes a split whose session died');
	is(q1($n1, "SELECT state FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 2"), 'active', 'E: the bucket is active');
	is(clean(), '', 'E: nothing is left on the owner');
	model_check('E1');
	($pid, $file) = stopped('split_after_switch', "SELECT kingres.split_bucket($C, 2, 2)", 'e2');
	ok(wait_for_fault_file("$file.reached", 60), 'E: a split of bucket 2 waits after its switch');
	is(q1($n1, "SELECT modulus FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 2"), '16', 'E: the catalog still has the old layout (bucket 2 was split before)');
	kill_split();
	unlink($file);
	waitpid($pid, 0);
	ok($n1->poll_query_until('postgres', q{SELECT phase IN ('cleaning', 'done') FROM kingres.bucket_split ORDER BY split_id DESC LIMIT 1}), 'E: the worker finishes a split whose partitions are switched');
	is(q1($n1, "SELECT modulus FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 2"), '32', 'E: the catalog has the children');
	ok($n1->poll_query_until('postgres', q{SELECT phase = 'done' FROM kingres.bucket_split ORDER BY split_id DESC LIMIT 1}), 'E: and the worker drops the orphans');
	model_check('E2');
	verify_all('E3');
}

# ---------------------------------------------------------------------------
# F. a split bucket moves; a table made now has the layout
# ---------------------------------------------------------------------------
{
	my $b = 10;
	my $src = owner($b);
	my $tgt = ($src + 1) % 3;
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $tgt)");
	is($rc, 0, 'F: a child of a split moves') or diag($err);
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(owner($b), $tgt, 'F: and is on its target');
	q1($n1, q{CREATE TABLE public.s3 (id int PRIMARY KEY, v text); INSERT INTO public.s3 SELECT g, 'x' FROM generate_series(1, 500) g});
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.s3', 'id', colocate_with => 'public.s1')});
	is($rc, 0, 'F: a table is added to the split group') or diag($err);
	is(bounds('s3'), bounds('s1'), 'F: it has the buckets of s1, with the same moduli');
	is(q1($n3, q{SELECT count(*) FROM public.s3}), '500', 'F: and its rows');
	is(clean(), '', 'F: check_cluster() finds nothing');
	# what check_cluster() says about what a split leaves behind
	q1($n2, q{CREATE TABLE public.s1_s999_1 (id int); COMMENT ON TABLE public.s1_s999_1 IS 'kingres_shard bucket split 999'});
	like(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || object, ','), '') FROM kingres.check_cluster()}), qr/split_child_table s1_s999_1/, 'F: a child table of a split that is not running is a finding');
	q1($n2, q{DROP TABLE public.s1_s999_1});
	q1($n3, q{CREATE FUNCTION kingres.split_capture_999_1() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END';
	          CREATE TABLE public.loose (id int);
	          CREATE TRIGGER ks_split_capture_row AFTER INSERT ON public.loose FOR EACH ROW EXECUTE FUNCTION kingres.split_capture_999_1()});
	like(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || object, ','), '') FROM kingres.check_cluster()}), qr/split_trigger split_capture_999_1/, 'F: and so is a capture trigger');
	q1($n3, q{DROP TABLE public.loose; DROP FUNCTION kingres.split_capture_999_1()});
	is(clean(), '', 'F: clean again');
	model_check('F');
}


# ---------------------------------------------------------------------------
# G. between the record of a split and its live mark the recovery worker leaves it alone
# ---------------------------------------------------------------------------
{
	# The session that begins a split writes its record in a session of its own and takes the live mark afterwards.  Stopped between the two (the fault point), the
	# split has a record and no mark: a recovery pass (here called by hand, the worker does the same every 2 s) must still see it alive and not undo it.
	my ($pid, $file) = stopped('split_after_begin', "SELECT kingres.split_bucket($C, 4, 2)", 'g1');
	ok(wait_for_fault_file("$file.reached", 60), 'G: a split of bucket 4 waits between its record and its live mark');
	my $sid = q1($n1, q{SELECT split_id FROM kingres.bucket_split ORDER BY split_id DESC LIMIT 1});
	is(q1($n1, "SELECT phase FROM kingres.bucket_split WHERE split_id = $sid"), 'preparing', 'G: the record is there');
	is(q1($n1, "SELECT kingres.internal_split_alive($sid)"), 't', 'G: the split is alive');
	is(q1($n1, q{SELECT kingres.resume_bucket_splits()}), '0', 'G: a recovery pass leaves it alone');
	is(q1($n1, "SELECT phase FROM kingres.bucket_split WHERE split_id = $sid"), 'preparing', 'G: and the record is as it was');
	release($pid, $file);
	unlike(out('g1'), qr/ERROR|WARNING/, 'G: the split goes on and ends without a warning') or diag(out('g1'));
	like(q1($n1, "SELECT phase FROM kingres.bucket_split WHERE split_id = $sid"), qr/^(cleaning|done)$/, 'G: it is finished');
	is(q1($n1, "SELECT kingres.internal_split_alive($sid)"), 'f', 'G: and nobody holds its mark any more');
	q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	verify_all('G');
}

done_testing();
