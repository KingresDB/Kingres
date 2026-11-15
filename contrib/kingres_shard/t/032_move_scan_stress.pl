# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Statements that read or write several shard groups while buckets move, from every
# node at once, as many as the machine can run (design/shard-phase4.md, "The map
# version rule").  The deterministic version of this is 031_move_scan_interleavings;
# this one does not control the interleavings, it lets the scheduler find them: on
# every node four processes loop a scan, a join of two distributed tables, point
# reads, and writes (routed and fanned out) of a counter table, while the main
# process moves buckets between all pairs of groups, one after the other, with
# random pauses.  Every answer must be exact and every statement either succeed or
# fail with KS001; every counter write that succeeded must be in the table exactly
# once at the end.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use Time::HiRes qw(sleep time);
use KingresShardTest qw(make_cluster);

# Run at a lower priority, servers included: this test starts hundreds of sessions,
# and the suites that run beside it (in the in-tree run, all at once) have tests that
# depend on timing (a recovery worker that wakes every second, the number of writes
# in a fixed time) and must not be starved by it.
POSIX::nice(10);

my @nodes = make_cluster('st', 3, logical => 1, extra => "max_connections = 200\n");
my ($n1) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();

$n1->safe_psql('postgres', q{
	CREATE TABLE public.t (id int PRIMARY KEY, v int NOT NULL);
	CREATE TABLE public.t2 (id int PRIMARY KEY, w int NOT NULL);
	CREATE TABLE public.u (id int PRIMARY KEY, n int NOT NULL);
	SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 12);
	SELECT kingres.create_distributed_table('public.t2', 'id', colocate_with => 'public.t');
	SELECT kingres.create_distributed_table('public.u', 'id', colocate_with => 'public.t');
});
{
	my $sql = '';
	$sql .= "INSERT INTO public.t VALUES ($_, $_ * 3);\n" for 1 .. 240;
	$sql .= "INSERT INTO public.t2 VALUES ($_, $_ * 5);\n" for 1 .. 240;
	$sql .= "INSERT INTO public.u VALUES ($_, 0);\n" for 1 .. 120;
	$n1->safe_psql('postgres', $sql);
}
my $sumid = 240 * 241 / 2;
my %want = (scan => '240,' . 3 * $sumid, join => '240,' . 8 * $sumid);
my $stop = "$tmp/stop";

# The scripts: each is run again and again by its process until the stop file exists.
my %script = (
	scan => "SELECT count(*) || ',' || sum(v) || ' ' || to_char(pg_catalog.clock_timestamp(), 'HH24:MI:SS.US') FROM public.t \\watch i=0.02 c=30\n",
	# One session that does the rest, a statement after the other: a join of two
	# distributed tables, point reads, routed counter writes and a fanned-out one.
	mix => "SELECT count(*) || ',' || sum(a.v + b.w) FROM public.t a JOIN public.t2 b ON b.id = a.id;\n"
	  . join('', map { "SELECT 'p' || id || ':' || v FROM public.t WHERE id = $_;\n" } (3, 17, 58, 99, 130, 171, 202, 233))
	  . join('', map { "UPDATE public.u SET n = n + 1 WHERE id = $_ RETURNING 'w';\n" } (4, 21, 36, 55, 78, 97, 110))
	  . "",
);
# The fanned-out counter write comes from one node only: two of them at once can
# deadlock on the rows, as any two concurrent multi-row UPDATEs can, which has
# nothing to do with moves.
my $fanout = "UPDATE public.u SET n = n + 1 RETURNING 'w';\n";

my @procs;
my $parent = $$;
for my $i (0 .. 2)
{
	for my $kind (qw(scan mix))
	{
		my $file = "$tmp/$kind$i.sql";
		open(my $fh, '>', $file) or die;
		print $fh "\\set VERBOSITY verbose\n\\pset format unaligned\n\\pset tuples_only on\n$script{$kind}" . ($kind eq 'mix' && $i == 0 ? $fanout : '');
		close($fh);
		my $pid = fork();
		die "fork" unless defined $pid;
		if ($pid == 0)
		{
			# Also stop when the test process is gone, so that a failure of the test does not
			# leave processes that loop on a server that is down.
			while (!-e $stop && getppid() == $parent)
			{
				sleep(0.02);
				system("psql -X -q -h " . $nodes[$i]->host . " -p " . $nodes[$i]->port . " -d postgres -f $file"
					  . " >> $tmp/$kind$i.out 2>> $tmp/$kind$i.err");
			}
			POSIX::_exit(0);
		}
		push @procs, { pid => $pid, kind => $kind, node => $i };
	}
}

# Move buckets around, one after the other, between every pair of groups.
my $moves = 0;
my @pairs = ([0, 1], [1, 2], [2, 0], [0, 2], [2, 1], [1, 0]);
for my $round (1 .. 24)
{
	my ($s, $t) = @{ $pairs[$round % 6] };
	my ($bucket) = split /,/, $n1->safe_psql('postgres',
		"SELECT string_agg(bucket::text, ',' ORDER BY bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = $s");
	if (!defined $bucket || $bucket eq '')
	{
		next;
	}
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $bucket, $t)");
	$moves++;
	sleep(rand(0.05));
}
# The orphans of all the moves, several of them of one bucket on one node, go at once.
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
open(my $sfh, '>', $stop) or die;
close($sfh);
waitpid($_->{pid}, 0) for @procs;

my $wrong_when = 0;
my %n = (connect => 0, answers => 0, retryable => 0, wrong => 0, other => 0, writes => 0);
for my $p (@procs)
{
	my ($kind, $i) = ($p->{kind}, $p->{node});
	if (open(my $o, '<', "$tmp/$kind$i.out"))
	{
		while (my $line = <$o>)
		{
			chomp $line;
			next if $line eq '';
			$n{answers}++;
			my $ok;
			my $when = '';
			if ($line eq 'w') { $n{writes}++; $ok = 1; }
			elsif ($line =~ /^p(\d+):(\d+)$/) { $ok = ($2 == 3 * $1); }
			elsif ($kind eq 'scan') { (my $answer, $when) = split(/ /, $line); $ok = ($answer eq $want{scan}); }
			else { (my $answer) = split(/ /, $line); $ok = ($answer eq $want{join}); }
			if (!$ok)
			{
				$n{wrong}++;
				diag("$kind node " . ($i + 1) . ": '$line'");
				$wrong_when = 1;
			}
		}
		close($o);
	}
	if (open(my $e, '<', "$tmp/$kind$i.err"))
	{
		while (my $line = <$e>)
		{
			next unless $line =~ /ERROR/;
			if ($line =~ /KS001/) { $n{retryable}++; }
			elsif ($line =~ /could not connect to node/) { $n{connect}++; }
			else { $n{other}++; diag("$kind node " . ($i + 1) . ": $line"); }
		}
		close($e);
	}
}
if ($n{wrong})
{
	diag($n1->safe_psql('postgres', q{SELECT string_agg(move_id || ' bucket ' || bucket || ' ' || source_group || '>' || target_group || ' ' || to_char(started_at, 'HH24:MI:SS.US') || ' - ' || coalesce(to_char(finished_at, 'HH24:MI:SS.US'), ''), E'
' ORDER BY move_id) FROM kingres.bucket_move}));
}
diag("statements answered: $n{answers}, retryable stale-map errors: $n{retryable}, counter writes done: $n{writes}, connection failures: $n{connect}, wrong answers: $n{wrong}, other failures: $n{other}, moves: $moves");
cmp_ok($n{answers}, '>', 300, 'the statements ran many times during the moves');
is($n{wrong}, 0, 'no statement returned a wrong answer');
is($n{other}, 0, 'no statement failed with anything but the retryable stale-map error');
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('failed','cancelled')"), '0', 'no move failed');
is($n1->safe_psql('postgres', 'SELECT coalesce(sum(n), 0) FROM public.u'), $n{writes},
	'every counter write that succeeded was applied exactly once');
is(join(',', map { $_->safe_psql('postgres', 'SELECT count(*) || \',\' || sum(v) FROM public.t') } @nodes),
	join(',', ($want{scan}) x 3), 'and the table is whole at the end');

done_testing();
