# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/193_partitioned_move.pl: a bucket move when the tables of the group are user-partitioned (design/shard-phase9.md, increment 9.5, "What a
# bucket move moves"): the bucket partition is a tree; the copy on the target is the same tree (a leaf per user partition, with its bound and its
# access method), the publication of the root carries the rows and the changes of every leaf, the switch attaches and detaches the root, the orphan
# on the source takes its leaves with it, and a bucket that comes back finds its names free.
#
#   A. the move: rows once from every node, the tree on the target, the orphan with its leaves, sizes, nothing of the replication left
#   B. the bucket moves back at once (the names of the leaves are free); a plain table colocated with the partitioned one moves with it
#   C. a writer on another node races the move; no change is lost
#   D. an error at three points of the move leaves no stray table, leaf or replication object

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use KingresShardTest qw(make_cluster run_sql move_state start_psql finish_psql);

my @nodes = make_cluster('pm', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

$n1->safe_psql('postgres', q{
	CREATE TABLE public.pm (customer int NOT NULL, created date NOT NULL, id int NOT NULL, qty int, PRIMARY KEY (customer, created, id)) PARTITION BY RANGE (created);
	CREATE TABLE public.pm_p1 PARTITION OF public.pm FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
	CREATE TABLE public.pm_p2 PARTITION OF public.pm FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
	CREATE TABLE public.pm_def PARTITION OF public.pm DEFAULT;
	INSERT INTO public.pm SELECT g % 13, date '2026-01-01' + (g % 80), g, g % 40 FROM generate_series(1, 600) g;
	SELECT kingres.create_distributed_table('public.pm', 'customer', bucket_count => 6);
	CREATE TABLE public.pi (customer int NOT NULL, id int NOT NULL, v text, PRIMARY KEY (customer, id));
	INSERT INTO public.pi SELECT g % 13, g, 'v' || g FROM generate_series(1, 200) g;
	SELECT kingres.create_distributed_table('public.pi', 'customer', colocate_with => 'public.pm');
	CREATE TABLE public.twin (customer int, created date, id int, qty int);
	INSERT INTO public.twin SELECT * FROM public.pm;
});
my $coloc = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.pm'::regclass});

sub totals
{
	my ($node) = @_;
	return q1($node, q{SELECT (SELECT count(*) || ',' || sum(id) || ',' || sum(qty) FROM public.pm) || '|' || (SELECT count(*) || ',' || sum(id) FROM public.pi)});
}
sub same_everywhere { my ($label, $want) = @_; is(totals($_), $want, "$label: every row exactly once from " . $_->name) for @nodes; }
# on every node: the leaves are inside the bucket partitions that the node owns, three per bucket partition
sub tree_ok
{
	my ($label) = @_;
	for my $n (@nodes)
	{
		is(q1($n, q{SELECT count(*) FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid JOIN pg_class p ON p.oid = i.inhparent
		             WHERE c.relname ~ '^pm_(p1|p2|def)_b[0-9]{4}$' AND c.relkind = 'r' AND p.relname ~ '^pm_b[0-9]{4}$'}),
		   q1($n, q{SELECT 3 * count(*) FROM pg_class WHERE relname ~ '^pm_b[0-9]{4}$' AND relkind = 'p' AND relispartition}), "$label: three leaves in every bucket partition of " . $n->name);
		is(q1($n, q{SELECT count(*) FROM pg_class WHERE relname ~ '^pm_(p1|p2|def)_b[0-9]{4}$' AND NOT relispartition}), '0', "$label: no stray leaf on " . $n->name);
	}
}
sub nothing_left
{
	my ($label) = @_;
	for my $n (@nodes)
	{
		is(q1($n, q{SELECT (SELECT count(*) FROM pg_replication_slots) || ',' || (SELECT count(*) FROM pg_subscription) || ',' || (SELECT count(*) FROM pg_publication)
		            || ',' || (SELECT count(*) FROM pg_replication_origin) || ',' || (SELECT count(*) FROM pg_prepared_xacts)}), '0,0,0,0,0', "$label: no slot, subscription, publication, origin or prepared transaction on " . $n->name);
	}
}
my $want = totals($n1);
same_everywhere('before', $want);
tree_ok('before');

# the bucket of customer 1 and its group
my $bucket = q1($n1, "SELECT kingres.bucket_for($coloc, 1)");
my $source = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $coloc AND bucket = $bucket");
my $target = ($source + 1) % 3;
my ($src, $tgt) = ($nodes[$source], $nodes[$target]);
my $bp = sprintf('pm_b%04d', $bucket);
my $rows = q1($n1, "SELECT count(*) FROM public.pm WHERE kingres.bucket_for($coloc, customer) = $bucket");
cmp_ok($rows, '>', 0, "bucket $bucket (group $source) holds rows");
$_->safe_psql('postgres', q{CREATE ROLE pm_owner}) for @nodes;
$n1->safe_psql('postgres', q{ALTER TABLE public.pm OWNER TO pm_owner});

# ---------------------------------------------------------------------------
# A. the move
# ---------------------------------------------------------------------------
my $id = q1($n1, "SELECT kingres.move_bucket($coloc, $bucket, $target)");
like($id, qr/^\d+$/, "move_bucket returned the id of the move ($id)");
like(move_state($n1, $id), qr/^cleaning\|/, 'the move is in the cleaning phase');
is(q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $coloc AND bucket = $bucket"), $target, 'the map has the bucket on the target');
same_everywhere('after the move', $want);
is(q1($tgt, "SELECT relkind::text || '|' || relispartition::text FROM pg_class WHERE oid = to_regclass('public.$bp')"), 'p|true', 'the bucket partition on the target is a partitioned table, attached');
is(q1($tgt, "SELECT count(*) FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid WHERE i.inhparent = 'public.$bp'::regclass"), '3', 'with its three leaves');
is(q1($tgt, "SELECT string_agg(pg_get_expr(c.relpartbound, c.oid), ';' ORDER BY c.relname) FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid WHERE i.inhparent = 'public.$bp'::regclass"),
   q{DEFAULT;FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')}, 'with the bounds of the source');
is(q1($tgt, "SELECT count(*) FROM pg_class c WHERE c.relname ~ '^pm_(p1|p2|def)_b0*$bucket\$' AND c.relowner = 'pm_owner'::regrole"), '3', 'and the owner of the table');
is(q1($tgt, "SELECT count(*) FROM pg_constraint WHERE conname = 'ks_bucket_check' AND conrelid IN (SELECT oid FROM pg_class WHERE relname ~ '^pm_.*_b0*$bucket\$')"), '0', 'the check of the move is gone from the tree');
is(q1($src, "SELECT count(*) FROM pg_class WHERE relname ~ '^pm_.*_b0*$bucket\$' AND relnamespace = 'public'::regnamespace"), '0', 'the source has neither the bucket partition nor its leaves in the schema of the table');
is(q1($src, "SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname ~ '^orphan_${id}_[0-9]+\$' AND relkind = 'p'"), '1', 'the source keeps the orphan, a partitioned table');
is(q1($src, "SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname ~ '^orphan_${id}_[0-9]+_l[0-9]+\$' AND relkind = 'r'"), '3', 'with its leaves under names of the orphan');
is(q1($tgt, "SELECT partition_size > 0 FROM kingres.buckets WHERE table_name = 'public.pm'::regclass AND bucket = $bucket AND partition_size IS NOT NULL"), 't', 'kingres.buckets shows the size of the tree on the target');
tree_ok('after the move');
nothing_left('after the move');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name <> 'orphan_table'}), '0', 'check_cluster() finds nothing but the orphan of the delay');

# ---------------------------------------------------------------------------
# B. back at once
# ---------------------------------------------------------------------------
my $id2 = q1($n1, "SELECT kingres.move_bucket($coloc, $bucket, $source)");
like($id2, qr/^\d+$/, 'the bucket moves back to the source before the orphan was dropped') or diag($id2);
same_everywhere('after the move back', $want);
tree_ok('after the move back');
is(q1($src, "SELECT relkind::text || '|' || relispartition::text FROM pg_class WHERE oid = to_regclass('public.$bp')"), 'p|true', 'the source has the tree again');
is(q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)}) > 0 ? 1 : 0, '1', 'the cleanup finishes the moves');
is(q1($_, q{SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname ~ '^orphan_'}), '0', 'no orphan, no leaf of one, on ' . $_->name) for @nodes;
nothing_left('after the cleanup');

# ---------------------------------------------------------------------------
# C. a writer on another node races the move
# ---------------------------------------------------------------------------
{
	my @stmts;
	for my $k (1 .. 150)
	{
		my $c = 1 + 13 * ($k % 5);
		push @stmts, "INSERT INTO public.pm VALUES ($c, '2026-0" . (1 + $k % 3) . "-1" . ($k % 9) . "', " . (500000 + $k) . ", $k)";
		push @stmts, "UPDATE public.pm SET qty = qty + 1 WHERE id = " . (500000 + $k - 7) if $k > 7;
		push @stmts, "DELETE FROM public.pm WHERE id = " . (500000 + $k - 11) . " AND $k % 4 = 0" if $k > 11;
	}
	# the writer, in a child: a statement that meets the change of the map ("the bucket map changed ... Repeat the statement") is repeated
	my $progress = "$PostgreSQL::Test::Utils::tmp_check/progress_$$";
	append_to_file($progress, '');
	my $pid = fork();
	die "fork: $!" unless defined $pid;
	if ($pid == 0)
	{
		my $bad = 0;
		for my $s (@stmts)
		{
			my $tries = 0;
			while (1)
			{
				my ($rc, $out, $err) = run_sql($n2, $s);
				if ($rc == 0) { append_to_file($progress, 'x'); last; }
				if ($err =~ /bucket map changed/ && ++$tries < 30) { select(undef, undef, undef, 0.2); next; }
				$bad++;
				last;
			}
		}
		POSIX::_exit($bad ? 1 : 0);
	}
	select(undef, undef, undef, 0.3);
	my $before = -s $progress;
	my $id3 = q1($n1, "SELECT kingres.move_bucket($coloc, $bucket, $target)");
	my $during = (-s $progress) - $before;
	like($id3, qr/^\d+$/, 'C: the move runs while a writer on another node works') or diag($id3);
	cmp_ok($during, '>', 0, "C: $during statements of the writer ran while the move ran");
	waitpid($pid, 0);
	is($? >> 8, 0, 'C: the writer ends without an error, repeating what the change of the map refused');
	(my $tsql = join(";\n", @stmts) . ";\n") =~ s/public\.pm/public.twin/g;
	$n1->safe_psql('postgres', $tsql);
	is(q1($n1, q{SELECT count(*) || ',' || sum(id) || ',' || sum(qty) FROM public.pm}), q1($n1, q{SELECT count(*) || ',' || sum(id) || ',' || sum(qty) FROM public.twin}), 'C: no change of the writer is lost');
	is(q1($n3, q{SELECT count(*) || ',' || sum(id) || ',' || sum(qty) FROM public.pm}), q1($n1, q{SELECT count(*) || ',' || sum(id) || ',' || sum(qty) FROM public.twin}), 'C: and a third node sees the same');
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	tree_ok('after the race');
	nothing_left('after the race');
	$want = totals($n1);
}

# ---------------------------------------------------------------------------
# D. an error at three points of a move
# ---------------------------------------------------------------------------
for my $point (qw(move_after_target_tables move_after_publication move_after_slot))
{
	my $b = q1($n1, "SELECT kingres.bucket_for($coloc, 2)");
	my $g = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $coloc AND bucket = $b");
	my $t = ($g + 1) % 3;
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:error'; SELECT kingres.move_bucket($coloc, $b, $t)");
	isnt($rc, 0, "D $point: the move fails");
	my $r = 0;
	for (1 .. 5) { $r += q1($n1, "SET kingres_shard.node_timeout = 3; SELECT kingres.resume_bucket_moves()"); last if q1($n1, q{SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('preparing','copying','catching_up','switching')}) eq '0'; sleep 2; }
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('preparing','copying','catching_up','switching')}), '0', "D $point: no move is unfinished");
	tree_ok("D $point");
	nothing_left("D $point");
	is(q1($_, q{SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname ~ '^orphan_'}), '0', "D $point: no orphan on " . $_->name) for @nodes;
	same_everywhere("D $point", $want);
}
# ---------------------------------------------------------------------------
# E. the publication carries the tree through its root, so the number of table-sync workers does not grow with the number of leaves
# ---------------------------------------------------------------------------
{
	my $b = q1($n1, "SELECT kingres.bucket_for($coloc, 3)");
	my $g = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $coloc AND bucket = $b");
	my $t = ($g + 1) % 3;
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_e_$$";
	append_to_file($gate, 'x');
	my ($o, $e) = ('', '');
	my $hh = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
		'-c', "SET kingres_shard.fault_injection = 'move_after_slot:wait:$gate'", '-c', "SELECT kingres.move_bucket($coloc, $b, $t)" ], '>', \$o, '2>', \$e);
	ok(KingresShardTest::wait_for_fault_file("$gate.reached", 60), 'E: the move is held after the slot');
	is(q1($nodes[$g], q{SELECT pubviaroot FROM pg_publication WHERE pubname ~ '^ks_move_'}), 't', 'E: the publication of the source publishes through the root of the tree');
	is(q1($nodes[$g], q{SELECT count(*) FROM pg_publication_tables WHERE pubname ~ '^ks_move_' AND tablename ~ '^pm_b'}), '1', 'E: and names the bucket partition once, not its leaves');
	unlink $gate;
	IPC::Run::finish($hh);
	is($e, '', 'E: the held move ends') or diag($e);
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	same_everywhere('E', $want);
}
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'check_cluster() finds nothing at the end');
done_testing();
