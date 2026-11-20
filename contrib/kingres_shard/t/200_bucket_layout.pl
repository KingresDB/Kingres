# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/200_bucket_layout.pl: every bucket has its own modulus (design/shard-phase10.md, sections 1.1 to 1.3; increment P10.1).
# Nothing in this version splits a bucket by itself, so the test splits buckets by hand, as the later split does (a standalone
# copy of each half is attached in place of the bucket partition) and then asks every part of the extension that routes by bucket
# to agree with PostgreSQL's own partition routing on the mixed moduli.
#
#   A. a cluster of three groups with two colocated tables; the baseline answers of a set of statements
#   B. a bucket split in two, another in four, a child split again: after each step every node gives the baseline answers, every
#      row is in the partition that kingres.bucket_for() names, check_cluster() finds nothing
#   C. writes from every node on the split layout: single-row, multi-row INSERT, INSERT ... SELECT, COPY, UPDATE, DELETE
#   D. moves of a split child: it keeps its own modulus on the target; the placement epoch counts every switch and a bucket that
#      moves away and back does not restore the placement hash (S4-01)
#   E. a table created colocated with the split group follows its layout; a table altered to a new group like it gets the same buckets
#   F. what check_cluster() says about a partition whose bounds differ from the bucket map, and about a gap in the layout

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @nodes = make_cluster('lay', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

q1($n1, q{
	CREATE TABLE public.s1 (id int PRIMARY KEY, v text NOT NULL, k2 int NOT NULL DEFAULT 0);
	CREATE TABLE public.s2 (id int PRIMARY KEY, w int NOT NULL);
	INSERT INTO public.s1 SELECT g, 'v' || g, g % 13 FROM generate_series(1, 3000) g;
	INSERT INTO public.s2 SELECT g, g * 2 FROM generate_series(1, 3000) g WHERE g % 3 <> 0;
	SELECT kingres.create_distributed_table('public.s1', 'id', bucket_count => 8);
	SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s1');
});
my $C = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.s1'::regclass});

# the answers of a set of statements from one node, as one string
sub digest
{
	my ($node) = @_;
	my @keys = map { 1 + $_ * 97 } 0 .. 30;
	my $in = join(',', @keys);
	my @qs = (
		q{SELECT count(*) || ',' || sum(id) || ',' || sum(length(v)) FROM public.s1},
		q{SELECT string_agg(m || ':' || c || ':' || s, ' ' ORDER BY m) FROM (SELECT id % 7 AS m, count(*) AS c, sum(id) AS s FROM public.s1 GROUP BY 1) x},
		q{SELECT count(*) || ',' || sum(s2.w) FROM public.s1 JOIN public.s2 ON s1.id = s2.id},
		q{SELECT string_agg(id::text, ',' ORDER BY id) FROM (SELECT id FROM public.s1 ORDER BY id LIMIT 5 OFFSET 1000) x},
		"SELECT string_agg(v, ',' ORDER BY id) FROM public.s1 WHERE id IN ($in)",
		"SELECT count(*) FROM public.s1 WHERE id = ANY(ARRAY[$in]) AND k2 >= 0",
		q{SELECT count(*) FROM public.s2 WHERE id BETWEEN 100 AND 400},
	);
	my @out = map { q1($node, $_) } @qs;
	# single-key statements, one at a time, each routed by its bucket
	push @out, join(',', map { q1($node, "SELECT v FROM public.s1 WHERE id = $_") } @keys);
	push @out, join(',', map { q1($node, "SELECT w FROM public.s2 WHERE id = $_") } grep { $_ % 3 } @keys);
	return join("\n", @out);
}
my $base = digest($n1);
is(digest($_), $base, 'A: the baseline is the same from every node') for ($n2, $n3);

# rows of a table on a node that are not in the partition bucket_for() names
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
sub misplaced
{
	my ($node, $tbl) = @_;
	return q1($node, "SELECT public.misplaced('public.$tbl'::regclass, $C)");
}
# the bounds of the bucket partitions of a table on a node
sub bounds
{
	my ($node, $tbl) = @_;
	return q1($node, qq{
		SELECT coalesce(string_agg(regexp_replace(c.relname, '^.*_b', '') || '=' || regexp_replace(pg_get_expr(c.relpartbound, c.oid), '.*modulus (\\d+), remainder (\\d+).*', '\\1/\\2'), ' ' ORDER BY (regexp_match(c.relname, '_b([0-9]+)\$'))[1]::int), '')
		  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'public.$tbl'::regclass});
}
sub all_bounds { my ($tbl) = @_; return join(' ', grep { $_ ne '' } map { bounds($_, $tbl) } @nodes); }
sub catalog_layout
{
	my ($node) = @_;
	return q1($node, qq{SELECT string_agg(bucket || '/' || modulus || ':g' || group_id, ' ' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = $C});
}
sub epoch { my ($node) = @_; return q1($node, "SELECT placement_epoch FROM kingres.colocation WHERE colocation_id = $C"); }
sub clean { return q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}); }
sub verify_all
{
	my ($what) = @_;
	is(clean(), '0', "$what: check_cluster() finds nothing") or diag(q1($n1, q{SELECT check_name || ' ' || object || ' ' || detail FROM kingres.check_cluster()}));
	for my $i (0 .. 2)
	{
		is(misplaced($nodes[$i], 's1'), '0', "$what: node " . ($i + 1) . ": every row of s1 is in the partition of its bucket");
		is(misplaced($nodes[$i], 's2'), '0', "$what: node " . ($i + 1) . ": every row of s2 is in the partition of its bucket");
	}
	is(catalog_layout($_), catalog_layout($n1), "$what: the catalog of node " . ($_ == $n2 ? 2 : 3) . " has the layout of node 1") for ($n2, $n3);
}
is(clean(), '0', 'A: check_cluster() finds nothing at the start');
is(q1($n1, "SELECT max_modulus || ',' || placement_epoch || ',' || bucket_count FROM kingres.colocation WHERE colocation_id = $C"), '8,0,8', 'A: the group has max_modulus 8, epoch 0');
is(scalar(() = all_bounds('s1') =~ /=8\//g), 8, 'A: eight bucket partitions, all of modulus 8');

# A split by hand, as the split of the later increments does it: each half becomes a standalone table with the rows of its
# hash slots, the old partition is detached, the halves are attached with the new modulus, and the catalog gets the new rows.
sub hand_split
{
	my ($bucket, $parts) = @_;
	my ($group, $mod) = split /\|/, q1($n1, "SELECT group_id || '|' || modulus FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $bucket");
	my $owner = $nodes[$group];
	my $newmod = $mod * $parts;
	my @tables = split /\n/, q1($n1, "SELECT format('%I.%I|%s', n.nspname, c.relname, d.dist_column) FROM kingres.dist_table d JOIN pg_class c ON c.oid = d.relid JOIN pg_namespace n ON n.oid = c.relnamespace WHERE d.colocation_id = $C ORDER BY c.relname");
	my $i = 0;
	for my $t (@tables)
	{
		my ($tbl, $col) = split /\|/, $t;
		$i++;
		my $orphan = "orphan_hand_${bucket}_$i";
		my $sql = "BEGIN;\nSELECT kingres.internal_detach_bucket('$tbl'::regclass, $bucket, '$orphan');\n";
		for my $j (0 .. $parts - 1)
		{
			my $r = $bucket + $j * $mod;
			my $name = sprintf('%s_b%04d', $tbl, $r);
			$sql .= "CREATE TABLE $name (LIKE $tbl INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES);\n";
			$sql .= "INSERT INTO $name SELECT * FROM kingres.$orphan WHERE satisfies_hash_partition('$tbl'::regclass::oid, $newmod, $r, $col);\n";
			$sql .= "SELECT kingres.internal_attach_bucket('$tbl'::regclass, $r, $newmod);\n";
		}
		$sql .= "COMMIT;\n";
		my ($rc, $out, $err) = run_sql($owner, $sql);
		is($rc, 0, "B: node of group $group: the partitions of $tbl for bucket $bucket are replaced by $parts") or diag($err);
	}
	my $cat = "BEGIN;\nDELETE FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $bucket;\n"
	  . "INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id, modulus) SELECT $C, $bucket + j * $mod, $group, $newmod FROM generate_series(0, " . ($parts - 1) . ") j;\n"
	  . "UPDATE kingres.colocation SET max_modulus = greatest(max_modulus, $newmod), placement_epoch = placement_epoch + 1 WHERE colocation_id = $C;\nCOMMIT;\nSELECT kingres.sync_catalog();\n";
	my ($rc, $out, $err) = run_sql($n1, $cat);
	is($rc, 0, "B: the bucket map says bucket $bucket is $parts buckets of modulus $newmod") or diag($err);
}

# ---------------------------------------------------------------------------
# B. splits
# ---------------------------------------------------------------------------
my $owner_of_2 = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 2");
hand_split(2, 2);
is(epoch($n1), '1', 'B: the epoch is 1 after the first split');
is(q1($n1, "SELECT max_modulus FROM kingres.colocation WHERE colocation_id = $C"), '16', 'B: max_modulus is 16');
is(q1($n1, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $C"), '9', 'B: nine buckets after splitting one in two');
is(digest($_), $base, 'B: after the first split every node gives the baseline answers') for @nodes;
verify_all('B1');
is(q1($n1, "SELECT modulus || '/' || remainder FROM kingres.bucket_range($C, 10)"), '16/10', 'B: bucket_range of the new bucket');

hand_split(4, 4);
is(q1($n1, "SELECT max_modulus FROM kingres.colocation WHERE colocation_id = $C"), '32', 'B: max_modulus is 32 after the split in four');
is(q1($n1, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $C"), '12', 'B: twelve buckets');
is(digest($_), $base, 'B: after the split in four every node gives the baseline answers') for @nodes;
verify_all('B2');

hand_split(10, 2);    # a child of the first split is split again
is(q1($n1, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $C"), '13', 'B: thirteen buckets');
is(q1($n1, "SELECT string_agg(bucket || '/' || modulus, ' ' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = $C"),
   '0/8 1/8 2/16 3/8 4/32 5/8 6/8 7/8 10/32 12/32 20/32 26/32 28/32', 'B: the moduli: 8, 16 and 32 side by side');
is(epoch($n1), '3', 'B: the epoch counts the three splits');
is(epoch($_), '3', 'B: and every node has it') for ($n2, $n3);
is(digest($_), $base, 'B: after the second split of a child every node gives the baseline answers') for @nodes;
verify_all('B3');
is(q1($n1, "SELECT count(*) FROM kingres.buckets WHERE modulus = 32"), '12', 'B: the view of the buckets has the modulus (6 buckets of modulus 32, in two tables)');

# ---------------------------------------------------------------------------
# C. writes from every node on the split layout
# ---------------------------------------------------------------------------
my $tmp = PostgreSQL::Test::Utils::tempdir();
open(my $fh, '>', "$tmp/copy.csv") or die;
print $fh "$_,c$_,0\n" for 20001 .. 20300;
close($fh);
{
	my @w = (
		[$n1, q{INSERT INTO public.s1 VALUES (10001, 'a', 1)}],
		[$n2, q{INSERT INTO public.s1 SELECT g, 'm' || g, 2 FROM generate_series(11000, 11299) g}],
		[$n3, q{INSERT INTO public.s1 (id, v) VALUES (12001, 'x'), (12002, 'y'), (12003, 'z'), (12004, 'q'), (12005, 'r'), (12006, 's')}],
		[$n1, q{INSERT INTO public.s1 SELECT id + 30000, 'sel' || id, 3 FROM public.s1 WHERE id BETWEEN 1 AND 400}],
		[$n2, "COPY public.s1 FROM '$tmp/copy.csv' WITH (FORMAT csv)"],
		[$n3, q{UPDATE public.s1 SET v = v || '!' WHERE id BETWEEN 1 AND 3000 AND id % 10 = 0}],
		[$n1, q{UPDATE public.s1 SET k2 = k2 + 1 WHERE id IN (5, 6, 7, 8, 9, 10, 11, 12)}],
		[$n2, q{DELETE FROM public.s1 WHERE id BETWEEN 2900 AND 2999}],
		[$n3, q{INSERT INTO public.s2 SELECT g, 1 FROM generate_series(40000, 40099) g}],
		[$n1, q{DELETE FROM public.s2 WHERE id BETWEEN 1 AND 30}],
	);
	my $i = 0;
	for my $w (@w) { $i++; my ($rc, $out, $err) = run_sql($w->[0], $w->[1]); is($rc, 0, "C: write $i succeeds") or diag($err); }
}
is(q1($n1, q{SELECT count(*) FROM public.s1}), 3000 + 1 + 300 + 6 + 400 + 300 - 100, 'C: the number of rows of s1');
is(q1($n2, q{SELECT count(*) FROM public.s1 WHERE id >= 30000 AND id <= 30400}), '400', 'C: the rows of INSERT ... SELECT are there');
is(q1($n3, q{SELECT count(*) FROM public.s1 WHERE id BETWEEN 20001 AND 20300}), '300', 'C: the rows of COPY are there');
is(q1($n1, q{SELECT count(*) FROM public.s1 WHERE v LIKE '%!'}), '290', 'C: the updates are there');
is(q1($n1, q{SELECT sum(k2) FROM public.s1 WHERE id IN (5, 6, 7, 8, 9, 10, 11, 12)}), q1($n1, q{SELECT sum(id % 13 + 1) FROM generate_series(5, 12) id}), 'C: UPDATE of single keys');
verify_all('C');
# the same statement from each node gives the same answer
my $after = digest($n1);
is(digest($_), $after, 'C: every node gives the same answers after the writes') for ($n2, $n3);

# ---------------------------------------------------------------------------
# D. moves of a split child, and the epoch
# ---------------------------------------------------------------------------
{
	my $e0 = epoch($n1);
	my $src = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 10");
	my $tgt = ($src + 1) % 3;
	my $h0 = q1($n1, "SELECT kingres.internal_placement_hash($C)");
	my $before_rows = q1($n1, q{SELECT count(*) || ',' || sum(id) FROM public.s1});
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, 10, $tgt)");
	is($rc, 0, "D: the child 10 (modulus 32) moves from group $src to group $tgt") or diag($err);
	is(epoch($n1), $e0 + 1, 'D: the epoch counts the switch of the move');
	is(epoch($_), $e0 + 1, 'D: and every node has it') for ($n2, $n3);
	is(bounds($nodes[$tgt], 's1') =~ /(?:^| )0010=32\/10(?: |$)/ ? 1 : 0, 1, 'D: the copy on the target is attached with the modulus of the bucket (32, 10)');
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(q1($n1, q{SELECT count(*) || ',' || sum(id) FROM public.s1}), $before_rows, 'D: every row is there after the move');
	verify_all('D1');
	is(digest($_), $after, 'D: every node gives the answers of before the move') for @nodes;
	# and back: the placement is the same as at the start, the placement hash is not
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, 10, $src)");
	is($rc, 0, "D: and the child moves back to group $src") or diag($err);
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(epoch($n1), $e0 + 2, 'D: two switches, two counts');
	my $h2 = q1($n1, "SELECT kingres.internal_placement_hash($C)");
	isnt($h2, $h0, 'D: the placement hash after the bucket came back is not the one before it left (S4-01)');
	is(q1($n2, "SELECT kingres.internal_placement_hash($C)"), $h2, 'D: and every node computes the same one');
	verify_all('D2');
	# a move of a bucket of modulus 8 and of the first child in the middle
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, 12, " . ((q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 12") + 2) % 3) . ")");
	is($rc, 0, 'D: bucket 12 (modulus 32) moves') or diag($err);
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, 3, " . ((q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 3") + 1) % 3) . ")");
	is($rc, 0, 'D: bucket 3 (modulus 8) moves') or diag($err);
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	verify_all('D3');
	is(digest($_), $after, 'D: every node gives the same answers after more moves') for @nodes;

	# the plan of the rebalance counts the buckets of the group, not its base bucket count
	my $have = q1($n1, "SELECT string_agg(group_id || ':' || c, ' ' ORDER BY group_id) FROM (SELECT group_id, count(*) AS c FROM kingres.bucket_map WHERE colocation_id = $C GROUP BY 1) x");
	my @plan = split(/
/, q1($n1, "SELECT bucket || '|' || target_group FROM kingres.rebalance_plan() WHERE colocation_id = $C ORDER BY bucket"));
	for my $m (@plan)
	{
		my ($b, $g) = split(/[|]/, $m);
		($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $g)");
		is($rc, 0, "D: the planned move of bucket $b to group $g") or diag($err);
	}
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(q1($n1, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $C"), '13', 'D: still thirteen buckets');
	is(q1($n1, "SELECT max(c) - min(c) FROM (SELECT count(*) AS c FROM kingres.bucket_map WHERE colocation_id = $C GROUP BY group_id) x") <= 1 ? 1 : 0, 1, "D: the plan balances the 13 buckets over the three groups (was $have)");
	is(q1($n1, "SELECT count(*) FROM kingres.rebalance_plan() WHERE colocation_id = $C"), '0', 'D: and there is nothing more to move');
	verify_all('D4');
	is(digest($_), $after, 'D: every node gives the same answers after the rebalance') for @nodes;
}

# ---------------------------------------------------------------------------
# E. new tables follow the layout
# ---------------------------------------------------------------------------
{
	q1($n1, q{CREATE TABLE public.s3 (id int PRIMARY KEY, v text, k2 int NOT NULL DEFAULT 0);
	          INSERT INTO public.s3 SELECT g, 'p' || g, g % 5 FROM generate_series(1, 500) g});
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.s3', 'id', colocate_with => 'public.s1')});
	is($rc, 0, 'E: create_distributed_table colocated with the split group') or diag($err);
	is(all_bounds('s3'), all_bounds('s1'), 'E: s3 has the buckets of s1, with the same moduli');
	is(q1($n3, q{SELECT count(*) || ',' || sum(id) FROM public.s3}), '500,125250', 'E: its rows are all there');
	is(misplaced($_, 's3'), '0', 'E: every row of s3 is in the partition of its bucket') for @nodes;
	is(clean(), '0', 'E: check_cluster() finds nothing');
	# a table that is created without a group to join and has the same type and bucket count joins the group by itself
	q1($n1, q{CREATE TABLE public.s4 (id int PRIMARY KEY, v text)});
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.s4', 'id', bucket_count => 8)});
	is($rc, 0, 'E: create_distributed_table with the same bucket count') or diag($err);
	is(q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.s4'::regclass}), $C, 'E: it is placed in the colocation group of s1 (same type and count)');
	is(all_bounds('s4'), all_bounds('s1'), 'E: with the buckets of s1');
	# alter to another distribution column, in a group of its own that is like the old one
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.alter_distributed_table('public.s4', distribution_column => 'id', colocate_with => 'none')});
	is($rc, 0, 'E: alter_distributed_table to a group of its own') or diag($err);
	my $c4 = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.s4'::regclass});
	isnt($c4, $C, 'E: it has a group of its own now');
	is(q1($n1, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $c4"), q1($n1, "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $C"), 'E: the new group has the same number of buckets as the old');
	is(q1($n1, "SELECT string_agg(bucket || '/' || modulus || ':g' || group_id, ' ' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = $c4"),
	   q1($n1, "SELECT string_agg(bucket || '/' || modulus || ':g' || group_id, ' ' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = $C"), 'E: with the same buckets, moduli and owners');
	is(clean(), '0', 'E: check_cluster() finds nothing after the alter');
	# the online creation makes its tables from the layout of the group too (the copy writes through the row router)
	q1($n1, q{CREATE TABLE public.s6 (id int PRIMARY KEY, v text, k2 int NOT NULL DEFAULT 0);
	          INSERT INTO public.s6 SELECT g, 'o' || g, 0 FROM generate_series(1, 600) g});
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table_concurrently('public.s6', 'id', colocate_with => 'public.s1')});
	is($rc, 0, 'E: create_distributed_table_concurrently colocated with the split group') or diag($err);
	is(all_bounds('s6'), all_bounds('s1'), 'E: online: s6 has the buckets of s1, with the same moduli');
	is(q1($n2, q{SELECT count(*) || ',' || sum(id) FROM public.s6}), '600,180300', 'E: online: its rows are all there');
	is(misplaced($_, 's6'), '0', 'E: online: every row of s6 is in the partition of its bucket') for @nodes;
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.undistribute_table_concurrently('public.s6')});
	is($rc, 0, 'E: undistribute_table_concurrently') or diag($err);
	is(q1($n1, q{SELECT count(*) || ',' || sum(id) FROM public.s6}), '600,180300', 'E: online: the rows of the ordinary table');
	q1($n1, q{SELECT kingres.undistribute_table('public.s4')});
	q1($n1, q{SELECT kingres.undistribute_table('public.s3')});
	is(clean(), '0', 'E: check_cluster() finds nothing after the undistribute');
}

# ---------------------------------------------------------------------------
# F. what check_cluster() says
# ---------------------------------------------------------------------------
{
	# a partition whose bounds differ from the bucket map: an empty table, so that the re-attach has nothing to validate
	q1($n1, q{CREATE TABLE public.s5 (id int PRIMARY KEY); SELECT kingres.create_distributed_table('public.s5', 'id', colocate_with => 'public.s1')});
	my $g = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 28");
	my $o = $nodes[$g];
	q1($o, q{SELECT kingres.internal_detach_bucket('public.s5'::regclass, 28, 'orphan_f_1');
	         CREATE TABLE public.s5_b0028 (LIKE public.s5 INCLUDING ALL);
	         SELECT kingres.internal_attach_bucket('public.s5'::regclass, 28, 64)});
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'bucket_layout'}), '1', 'F: a partition with other bounds than the bucket map is a bucket_layout finding');
	like(q1($n1, q{SELECT detail FROM kingres.check_cluster() WHERE check_name = 'bucket_layout'}), qr/modulus 64, remainder 28.*modulus 32, remainder 28/, 'F: and the finding says both');
	q1($o, q{SELECT kingres.internal_detach_bucket('public.s5'::regclass, 28, 'orphan_f_2');
	         CREATE TABLE public.s5_b0028 (LIKE public.s5 INCLUDING ALL);
	         SELECT kingres.internal_attach_bucket('public.s5'::regclass, 28, 32);
	         DROP TABLE kingres.orphan_f_1, kingres.orphan_f_2});
	is(clean(), '0', 'F: with the bounds restored check_cluster() finds nothing');
	# a gap: a bucket row that is not there
	my $g26 = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 26");
	q1($n1, qq{DELETE FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = 26; SELECT kingres.sync_catalog()});
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'bucket_layout' AND object = 'colocation group } . $C . q{'}), '1', 'F: a gap in the layout is a bucket_layout finding');
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.bucket_for($C, k) FROM generate_series(1, 400) k");
	like($err, qr/has no bucket for slot \d+ of its layout/, 'F: a key in the gap is an error that names the slot, not a wrong bucket');
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.s5 SELECT g FROM generate_series(1, 400) g});
	isnt($rc, 0, 'F: and so is a write of keys in the gap (no row goes to a wrong group)');
	q1($n1, qq{INSERT INTO kingres.bucket_map (colocation_id, bucket, group_id, modulus) VALUES ($C, 26, $g26, 32); SELECT kingres.sync_catalog()});
	is(clean(), '0', 'F: with the bucket row back check_cluster() finds nothing');
}

done_testing();
