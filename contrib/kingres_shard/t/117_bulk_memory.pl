# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/117_bulk_memory.pl
#
# Steps 3 and 4 of phase 2b (design/shard-phase2b.md, 11.4): the memory of a bulk write does not depend on
# the number of rows.  The origin keeps a buffer per group and one batch in flight per group (about
# two batches per group, each at most insert_batch_bytes), the source cursor's batch and the rows that
# RETURNING has not handed out yet; the groups each run one batch statement at a time.
#
# The test loads a few million rows by INSERT ... SELECT and by COPY, with the settings that make a batch
# large (a megabyte, a thousand rows), and samples the private memory (RssAnon of /proc/<pid>/status:
# the shared buffers of the node are not private memory) of the backend of the origin and of the sessions
# that the groups open for it.  The verdict does not depend on the speed of the machine: it compares the
# peak of a load of one million rows with the peak of a load of four times that, and the peaks with a
# fixed bound well under the 2 GB that no test may use.  KINGRES_MEM_ROWS (default 4000000) sets the
# larger load.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster run_sql);

my $big = $ENV{KINGRES_MEM_ROWS} // 4000000;
my $small = int($big / 4);
my $tmp = $PostgreSQL::Test::Utils::tmp_check;

my ($n1, $n2, $n3) = make_cluster('bm', 3, extra => "shared_buffers = 64MB\nmax_wal_size = 2GB\nfsync = off\nsynchronous_commit = off\ncheckpoint_timeout = 30min\nwork_mem = 8MB\n");
$n1->safe_psql('postgres', q{
	CREATE TABLE public.big (id bigint PRIMARY KEY, v text, n int);
	SELECT kingres.create_distributed_table('public.big', 'id', bucket_count => 24);
});
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');

sub rss_anon_mb
{
	my ($pid) = @_;
	open(my $f, '<', "/proc/$pid/status") or return undef;
	while (my $line = <$f>)
	{
		return $1 / 1024 if $line =~ /^RssAnon:\s+(\d+) kB/;
	}
	return undef;
}

# A sampler process: every 0.2 s the private memory of the origin backend and of the group sessions
# of the origin, until the flag file appears; the peaks go to $out.
sub start_sampler
{
	my ($origin_pid, $flag, $out) = @_;
	my $pid = fork();
	die "fork" unless defined $pid;
	return $pid if $pid;
	my ($peak_origin, $peak_group, $last_refresh) = (0, 0, 0);
	my @group_pids;
	while (!-e $flag)
	{
		if (time - $last_refresh > 1.0)
		{
			@group_pids = ();
			for my $nd ($n2, $n3)
			{
				my $p = `psql -XAtq -h @{[ $nd->host ]} -p @{[ $nd->port ]} -d postgres -c "SELECT pid FROM pg_stat_activity WHERE application_name = 'kingres_shard'" 2>/dev/null`;
				push @group_pids, grep { /^\d+$/ } split /\n/, $p;
			}
			$last_refresh = time;
		}
		my $m = rss_anon_mb($origin_pid);
		$peak_origin = $m if defined $m && $m > $peak_origin;
		for my $gp (@group_pids)
		{
			my $g = rss_anon_mb($gp);
			$peak_group = $g if defined $g && $g > $peak_group;
		}
		sleep(0.2);
	}
	open(my $o, '>', $out) or POSIX::_exit(1);
	printf $o "%.1f %.1f\n", $peak_origin, $peak_group;
	close $o;
	POSIX::_exit(0);
}

my $session = $n1->background_psql('postgres', on_error_stop => 1);
my $origin_pid = $session->query_safe('SELECT pg_backend_pid()');
my $base = rss_anon_mb($origin_pid);
note("the origin backend holds $base MB of private memory before the load");

sub measured
{
	my ($label, $sql) = @_;
	my $flag = "$tmp/sampler_flag_$label";
	my $out = "$tmp/sampler_out_$label";
	unlink $flag, $out;
	my $sampler = start_sampler($origin_pid, $flag, $out);
	my $t0 = time;
	$session->query_safe($sql);
	my $took = time - $t0;
	append_to_file($flag, 'x');
	waitpid($sampler, 0);
	open(my $f, '<', $out) or die;
	my ($po, $pg) = split ' ', scalar(<$f>);
	close $f;
	note(sprintf("%s: %.1f s, peak private memory: origin %.1f MB, groups %.1f MB", $label, $took, $po, $pg));
	return ($po, $pg);
}

my $LIMIT = 400;				# MB of private memory per process: well under 2 GB, and far from the size of the data
my ($po1, $pg1) = measured('insert_small', "SET kingres_shard.insert_batch_rows = 1000; SET kingres_shard.insert_batch_bytes = '1MB';
	INSERT INTO public.big SELECT g, md5(g::text) || repeat('x', 60), g % 1000 FROM generate_series(1, $small) g");
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.big'), $small, "$small rows by INSERT ... SELECT");
my ($po2, $pg2) = measured('insert_big', "INSERT INTO public.big SELECT g, md5(g::text) || repeat('x', 60), g % 1000 FROM generate_series($small + 1, $big) g");
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.big'), $big, "$big rows by INSERT ... SELECT");
cmp_ok($po2, '<', $LIMIT, "the origin's private memory peaks at $po2 MB for $big rows (bound $LIMIT MB)");
cmp_ok($pg2, '<', $LIMIT, "the groups' private memory peaks at $pg2 MB (bound $LIMIT MB)");
cmp_ok($po2, '<', $po1 + 64, "four times the rows do not need more memory at the origin ($po1 MB for $small rows, $po2 MB for $big)");
cmp_ok($pg2, '<', $pg1 + 64, "... nor in the groups ($pg1 MB, $pg2 MB)");

# the same with COPY
{
	my $file = "$tmp/big.copy";
	$n1->safe_psql('postgres', "COPY (SELECT g + 100000000, md5(g::text) || repeat('y', 60), g % 1000 FROM generate_series(1, $big) g) TO '$file'");
	my ($pc1, $gc1) = measured('copy_small', "SET kingres_shard.insert_batch_rows = 1000; COPY public.big (id, v, n) FROM PROGRAM 'head -n $small $file'");
	my ($pc2, $gc2) = measured('copy_big', "COPY public.big (id, v, n) FROM PROGRAM 'tail -n +" . ($small + 1) . " $file'");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.big'), 2 * $big, "$big rows by COPY");
	cmp_ok($pc2, '<', $LIMIT, "COPY: the origin's private memory peaks at $pc2 MB (bound $LIMIT MB)");
	cmp_ok($gc2, '<', $LIMIT, "COPY: the groups' private memory peaks at $gc2 MB");
	cmp_ok($pc2, '<', $pc1 + 64, "COPY: three times the rows do not need more memory ($pc1 MB, $pc2 MB)");
	unlink $file;
}

# RETURNING is bounded by work_mem (the rows spill to disk): a large RETURNING does not grow the memory either
{
	my $extra = 600000;
	my ($pr, $gr) = measured('returning', "SET work_mem = '4MB'; INSERT INTO public.big SELECT g + 300000000, md5(g::text) || repeat('r', 60), g % 7 FROM generate_series(1, $extra) g RETURNING id, v");
	cmp_ok($pr, '<', $LIMIT, "RETURNING $extra rows: the origin's private memory peaks at $pr MB (bound $LIMIT MB)");
	cmp_ok($pr, '<', $po2 + 64, "... as for the load without RETURNING ($po2 MB): the rows spill to disk");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.big'), 2 * $big + $extra, "$extra rows more");
	$big += 0;
}

$session->quit;
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.big'), 2 * $big + 600000, 'the table holds every row at the end');
for my $nd ($n1, $n2, $n3)
{
	unlike(slurp_file($nd->logfile), qr/terminated by signal|TRAP:|out of memory/, 'no crash and no memory error in the log of ' . $nd->name);
}

done_testing();
