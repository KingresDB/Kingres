# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/203_move_progress.pl: kingres.move_progress() (design/shard-phase10.md, section 6.3, "Percent of a move"; increment P10.4).
# A move is stopped at a fault point in each phase of interest and the function is asked how far it is.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use POSIX ();
use Test::More;
use KingresShardTest qw(make_cluster run_sql wait_for_fault_file);

my $tmp = PostgreSQL::Test::Utils::tempdir();
my @nodes = make_cluster('mp', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }
q1($n1, q{
	CREATE TABLE public.t1 (id int PRIMARY KEY, v text);
	INSERT INTO public.t1 SELECT g, repeat('x', 400) FROM generate_series(1, 3000) g;
	SELECT kingres.create_distributed_table('public.t1', 'id', bucket_count => 6);
});
my $C = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.t1'::regclass});

sub stopped_move
{
	my ($point, $b, $tgt, $name) = @_;
	my $file = "$tmp/wait_$name";
	open(my $fh, '>', $file) or die;
	close($fh);
	my $pid = fork();
	die 'fork' unless defined $pid;
	if ($pid == 0)
	{
		my $sql = "SET kingres_shard.fault_injection = '$point:wait:$file'; SELECT kingres.move_bucket($C, $b, $tgt)";
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$sql\" > $tmp/out_$name 2>&1");
		POSIX::_exit(0);
	}
	return ($pid, $file);
}
sub release { my ($pid, $file) = @_; unlink($file); waitpid($pid, 0); }

is(q1($n1, q{SELECT count(*) FROM kingres.move_progress()}), '0', 'no move, no row');

# the copy has been made and the move waits to switch
{
	my $b = 1;
	my $src = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b");
	my $tgt = ($src + 1) % 3;
	my ($pid, $file) = stopped_move('move_after_catchup', $b, $tgt, 'catchup');
	ok(wait_for_fault_file("$file.reached", 60), 'the move reached the end of the catch-up');
	my @r = split /[|]/, q1($n1, q{SELECT move_id || '|' || colocation_id || '|' || bucket || '|' || source_group || '|' || target_group || '|' || phase || '|' || (source_bytes > 0) || '|' || (target_bytes > 0) || '|' || percent FROM kingres.move_progress()});
	is($r[1], $C, 'the colocation group');
	is($r[2], $b, 'the bucket');
	is($r[3], $src, 'the source group');
	is($r[4], $tgt, 'the target group');
	is($r[5], 'catching_up', 'the phase');
	is($r[6], 'true', 'the bucket has bytes on the source');
	is($r[7], 'true', 'and its copy has bytes on the target');
	is($r[8], '90', 'a move that has caught up is at 90 per cent');
	release($pid, $file);
	ok($n1->poll_query_until('postgres', q{SELECT phase IN ('cleaning', 'done') FROM kingres.bucket_move ORDER BY move_id DESC LIMIT 1}), 'the move completes');
}
q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
is(q1($n1, q{SELECT count(*) FROM kingres.move_progress()}), '0', 'a finished move is not listed');

# the copy is under way: the tables on the target exist, the percent follows their size
{
	my $b = 2;
	my $src = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b");
	my $tgt = ($src + 1) % 3;
	my ($pid, $file) = stopped_move('move_after_target_tables', $b, $tgt, 'tables');
	ok(wait_for_fault_file("$file.reached", 60), 'the move reached the creation of the tables on the target');
	my @r = split /[|]/, q1($n1, q{SELECT phase || '|' || percent || '|' || coalesce(target_bytes::text, 'null') || '|' || (source_bytes > 0) FROM kingres.move_progress()});
	is($r[0], 'copying', 'the phase is copying');
	cmp_ok($r[1], '<=', 89, 'the percent of a copy is at most 89');
	cmp_ok($r[1], '>=', 0, 'and at least 0');
	like($r[2], qr/^\d+$/, 'the tables on the target have a size (the copy has not begun, so it is small)');
	is($r[3], 'true', 'the bucket has bytes on the source');
	# a role that is not a superuser may not ask the nodes
	q1($n1, q{CREATE ROLE eve LOGIN});
	my ($rc, $out, $err) = run_sql($n1, q{SET ROLE eve; SELECT * FROM kingres.move_progress()});
	isnt($rc, 0, 'a role that is not a superuser may not ask');
	release($pid, $file);
	ok($n1->poll_query_until('postgres', q{SELECT phase IN ('cleaning', 'done') FROM kingres.bucket_move ORDER BY move_id DESC LIMIT 1}), 'the move completes');
}
q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'check_cluster() finds nothing');
is(q1($n1, q{SELECT count(*) || ',' || sum(id) FROM public.t1}), '3000,4501500', 'every row is there');

done_testing();
