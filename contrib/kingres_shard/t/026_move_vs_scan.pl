# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# A statement that reads every shard group (the distributed scan) while buckets
# move (design/shard-phase4.md, "Implementation notes and deviations", the map
# version rule): every answer is exactly the true count and checksum; a scan that
# meets a switch in the middle gets the retryable stale-map error (KS001), never a
# wrong answer.  Readers run on all three nodes, as separate processes, while the
# main process moves one bucket after the other.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use KingresShardTest qw(make_cluster);

my @nodes = make_cluster('sc', 3, logical => 1);
my ($n1) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();

$n1->safe_psql('postgres', q{
	CREATE TABLE public.t (id int PRIMARY KEY, v int NOT NULL);
	SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 12);
});
{
	my $sql = '';
	$sql .= "INSERT INTO public.t VALUES ($_, $_ * 3);\n" for 1 .. 240;
	$n1->safe_psql('postgres', $sql);
}
my $want = '240,' . (3 * 240 * 241 / 2);
my $stop = "$tmp/stop";

# Reader processes: each loops psql scripts that run the scan many times.
my @pids;
for my $i (0 .. 2)
{
	my $node = $nodes[$i];
	my $script = "$tmp/reader$i.sql";
	open(my $fh, '>', $script) or die;
	print $fh "\\pset format unaligned\n\\pset tuples_only on\n"
	  . "SELECT count(*) || ',' || sum(v) FROM public.t \\watch i=0.001 c=400\n";
	close($fh);
	my $pid = fork();
	die "fork" unless defined $pid;
	if ($pid == 0)
	{
		while (!-e $stop)
		{
			system("psql -X -q -h " . $node->host . " -p " . $node->port . " -d postgres -f $script"
				  . " >> $tmp/out$i 2>> $tmp/err$i");
		}
		POSIX::_exit(0);
	}
	push @pids, $pid;
}

# Move buckets around, one after the other, while the readers run.
my $moves = 0;
for my $round (1 .. 48)
{
	my $bucket = ($round * 5) % 12;
	my $src = $n1->safe_psql('postgres',
		"SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket");
	my $tgt = ($src + 1 + $round % 2) % 3;
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $bucket, $tgt)");
	$moves++;
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)') if $round % 4 == 0;
}
open(my $sfh, '>', $stop) or die;
close($sfh);
waitpid($_, 0) for @pids;

my ($reads, $wrong, $errors, $other) = (0, 0, 0, 0);
for my $i (0 .. 2)
{
	if (open(my $o, '<', "$tmp/out$i"))
	{
		while (my $line = <$o>)
		{
			chomp $line;
			next unless $line =~ /^[0-9]+,[0-9]+$/;
			$reads++;
			if ($line ne $want) { $wrong++; diag("reader $i got $line, wanted $want"); }
		}
		close($o);
	}
	if (open(my $e, '<', "$tmp/err$i"))
	{
		while (my $line = <$e>)
		{
			next unless $line =~ /ERROR/;
			if ($line =~ /KS001|stale|map changed|bucket map/) { $errors++; }
			else { $other++; diag("reader $i: $line"); }
		}
		close($e);
	}
}
cmp_ok($reads, '>', 500, "the readers ran many scans ($reads) during $moves moves");
is($wrong, 0, 'no scan returned a wrong count or checksum');
is($other, 0, 'no scan failed with anything but the retryable stale-map error');
diag("scans: $reads, retryable stale-map errors: $errors");
is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.bucket_move WHERE phase IN (\'failed\',\'cancelled\')'), '0', 'no move failed');
is(join(',', map { $_->safe_psql('postgres', 'SELECT count(*) || \',\' || sum(v) FROM public.t') } @nodes),
	"$want,$want,$want", 'and the table is whole at the end');

done_testing();
