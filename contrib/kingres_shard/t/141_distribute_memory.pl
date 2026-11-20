# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/141_distribute_memory.pl: the memory of create_distributed_table() does not
# depend on the number of rows (design/shard-phase9.md, "Copy"; the sender of
# 2b keeps two buffers per group).  A table of 1,000,000 rows and one of
# 3,000,000 rows are distributed over three nodes; the peak resident size of the
# backend that runs the call (VmHWM of /proc/<pid>/status, read inside the same
# session after the call) and of the backends of the other nodes that took the
# rows must stay within a few tens of megabytes of each other, and far below the
# size of the table.  The nodes run with small shared buffers so that the
# resident size is the private memory of the backends plus a bounded shared part.
# No process of the test can use more than 2 GB of address space: the test
# starts itself again under "ulimit -v 2000000".

use strict;
use warnings FATAL => 'all';

BEGIN
{
	# A cap on the address space of everything this test starts (the nodes are children of it).
	if (!$ENV{KS_P9_ULIMIT})
	{
		$ENV{KS_P9_ULIMIT} = 1;
		exec('bash', '-c', 'ulimit -v 2000000 && exec "$0" "$@"', $^X, (map { ('-I', $_) } grep { !ref } @INC), $0, @ARGV);
	}
}

use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my ($n1, $n2, $n3) = make_cluster('dm', 3, extra => "shared_buffers = 4MB\nwork_mem = 4MB\nmaintenance_work_mem = 16MB\n");
my @nodes = ($n1, $n2, $n3);

$_->safe_psql('postgres', q{
	CREATE FUNCTION public.local_total(t regclass) RETURNS bigint LANGUAGE plpgsql AS $$
	DECLARE r record; s bigint := 0; cnt bigint;
	BEGIN
	  FOR r IN SELECT c.oid::regclass AS p FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = t
	  LOOP
	    EXECUTE format('SELECT count(*) FROM ONLY %s', r.p) INTO cnt;
	    s := s + cnt;
	  END LOOP;
	  RETURN s;
	END $$;
}) for @nodes;

my $hwm_sql = q{SELECT (regexp_match(pg_read_file('/proc/' || pg_backend_pid() || '/status'), 'VmHWM:\s+([0-9]+) kB'))[1]};
my $hwm_others_sql = q{SELECT coalesce(max((regexp_match(pg_read_file('/proc/' || pid || '/status'), 'VmHWM:\s+([0-9]+) kB'))[1]::bigint), 0)
	FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()};

my %peak;
for my $rows (1_000_000, 3_000_000)
{
	my $t = "m$rows";
	$n1->safe_psql('postgres', qq{
		CREATE TABLE public.$t (id int PRIMARY KEY, grp int, v text);
		INSERT INTO public.$t SELECT g, g % 97, repeat(chr(97 + g % 26), 30) || g FROM generate_series(1, $rows) g;
		ANALYZE public.$t;
	});
	my $size = $n1->safe_psql('postgres', "SELECT pg_total_relation_size('public.$t') / 1024 / 1024");
	my $t0 = time;
	# the same session reads the peak of its own backend and, through a session of its own on each other node,
	# the peaks of the backends there (the one that took the rows is still open: the origin keeps its connections)
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.$t', 'id', bucket_count => 12);\n"
		. "$hwm_sql;\n"
		. "SELECT kingres.internal_exec_scalar(2, \$q\$$hwm_others_sql\$q\$);\n"
		. "SELECT kingres.internal_exec_scalar(3, \$q\$$hwm_others_sql\$q\$);");
	is($rc, 0, "$rows rows are distributed") or diag($err);
	my @lines = grep { /\S/ } split /\n/, $out;
	my ($origin, @theirs) = @lines[-3 .. -1];
	my $others = 0;
	for my $v (@theirs) { $others = $v if $v > $others; }
	$peak{$rows} = { origin => $origin, others => $others };
	diag(sprintf("%d rows (%d MB table): %d s; peak resident size of the origin backend %d MB, of the receiving backends %d MB",
		$rows, $size, time - $t0, $origin / 1024, $others / 1024));
	is($n2->safe_psql('postgres', "SELECT count(*) || ',' || sum(id) FROM public.$t"),
		"$rows," . ($rows * ($rows + 1) / 2), "the whole table is read from another node ($rows rows)");
	# the rows of the three nodes add up to the table: every row once
	my $sum = 0;
	$sum += $_->safe_psql('postgres', "SELECT public.local_total('public.$t')") for @nodes;
	is($sum, $rows, "the rows of the three nodes add up to $rows");
}
my $growth_origin = $peak{3_000_000}{origin} - $peak{1_000_000}{origin};
my $growth_others = $peak{3_000_000}{others} - $peak{1_000_000}{others};
diag(sprintf("growth of the peak from 1M to 3M rows: origin %d MB, receiving backends %d MB", $growth_origin / 1024, $growth_others / 1024));
cmp_ok($growth_origin, '<', 16 * 1024, 'the peak of the origin backend grows by less than 16 MB for three times the rows');
cmp_ok($growth_others, '<', 16 * 1024, 'the peak of the receiving backends grows by less than 16 MB for three times the rows');
cmp_ok($peak{3_000_000}{origin}, '<', 100 * 1024, 'the origin backend peaks below 100 MB for a table of 3,000,000 rows (283 MB)');
cmp_ok($peak{3_000_000}{others}, '<', 100 * 1024, 'the receiving backends peak below 100 MB');

done_testing();
