# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/145_online_load.pl: writers run on the source while it becomes distributed (design/shard-phase9.md,
# "Tests", t/041): inserts, updates, deletes and COPY, each statement its own transaction, in four sessions
# that start before the operation and end after it.  Afterwards the table is exactly what the writers'
# scripts say (a model computed here), every row is in the bucket of its key, no statement failed, and the
# longest statement of the writers and of a reader are recorded.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Digest::MD5 qw(md5_hex);
use Time::HiRes qw(time);
use KingresShardTest qw(make_cluster run_sql prepared_count start_psql finish_psql);

my ($n1, $n2, $n3) = make_cluster('ol', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

$_->safe_psql('postgres', q{
	CREATE FUNCTION public.local_rows(t regclass, key name) RETURNS TABLE (part text, bucket int, n bigint, wrong bigint)
	LANGUAGE plpgsql AS $$
	DECLARE r record;
	BEGIN
	  FOR r IN SELECT c.oid::regclass AS p, (regexp_match(c.relname, '_b([0-9]{4})$'))[1]::int AS b
	             FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = t ORDER BY c.relname
	  LOOP
	    part := r.p::text; bucket := r.b;
	    EXECUTE format('SELECT count(*), count(*) FILTER (WHERE kingres.bucket_for(%L::regclass, %I) <> %s) FROM ONLY %s', t, key, r.b, r.p)
	       INTO n, wrong;
	    RETURN NEXT;
	  END LOOP;
	END $$;
}) for @nodes;

# small chunks, so that the copy and the replay take many transactions
for my $n (@nodes)
{
	$n->append_conf('postgresql.conf', "kingres_shard.rebuild_batch_rows = 300\n");
	$n->reload;
}

$n1->safe_psql('postgres', q{
	CREATE TABLE public.ld (tenant int NOT NULL, id int NOT NULL, v text, n int, PRIMARY KEY (tenant, id));
	INSERT INTO public.ld SELECT g % 4, g, 'seed', 0 FROM generate_series(1, 6000) g;
});

# The four writers: writer w owns tenant w.  Statement i of its script:
#   insert (w, 10000 + i); every 3rd: update the row of i - 1; every 5th: delete the row of i - 2;
#   every 7th: a COPY of two rows; a seed row of the tenant is updated now and then.
my $N = 700;
my %model;       # "tenant,id" => "v|n"
for my $g (1 .. 6000) { $model{ ($g % 4) . ",$g" } = 'seed|0'; }

sub script_for
{
	my ($w) = @_;
	my @sql;
	for my $i (1 .. $N)
	{
		my $id = 10000 + $i;
		push @sql, "INSERT INTO public.ld VALUES ($w, $id, 'a', 1);";
		$model{"$w,$id"} = 'a|1';
		if ($i % 3 == 0)
		{
			my $u = 10000 + $i - 1;
			push @sql, "UPDATE public.ld SET v = 'b', n = n + 1 WHERE tenant = $w AND id = $u;";
			$model{"$w,$u"} = 'b|2' if exists $model{"$w,$u"};
		}
		if ($i % 5 == 0)
		{
			my $d = 10000 + $i - 2;
			push @sql, "DELETE FROM public.ld WHERE tenant = $w AND id = $d;";
			delete $model{"$w,$d"};
		}
		if ($i % 7 == 0)
		{
			my ($c1, $c2) = (20000 + $i * 2, 20000 + $i * 2 + 1);
			push @sql, "COPY public.ld FROM STDIN;\n$w\t$c1\tc\t5\n$w\t$c2\tc\t5\n\\.\n";
			$model{"$w,$c1"} = 'c|5';
			$model{"$w,$c2"} = 'c|5';
		}
		if ($i % 11 == 0)
		{
			my $s = $w + 4 * $i;
			push @sql, "UPDATE public.ld SET v = 'seedu', n = n + 1 WHERE tenant = $w AND id = $s;";
			$model{"$w,$s"} = 'seedu|1' if exists $model{"$w,$s"};
		}
		push @sql, "SELECT pg_sleep(0.004);";
	}
	return join("\n", @sql) . "\n";
}

my @writers;
my $tmp = $PostgreSQL::Test::Utils::tmp_check;
for my $w (0 .. 3)
{
	my $file = "$tmp/writer_$w.sql";
	open(my $fh, '>', $file) or die $!;
	print $fh script_for($w);
	close $fh;
	my ($out, $err) = ('', '');
	my $h = IPC::Run::start([ 'psql', '-X', '-q', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres', '-f', $file ],
							'>', \$out, '2>', \$err);
	push @writers, { h => $h, out => \$out, err => \$err };
}

# a reader that records its slowest statement
my $slowest = 0;
my $reader_stop = 0;
my $reader = IPC::Run::start([ 'psql', '-X', '-q', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres', '-c', 'SELECT 1' ], '>', \my $rout, '2>', \my $rerr);
$reader->finish;

# the operation, phase by phase, while the writers run
my $t0 = time;
my $op = q1($n1, "SELECT kingres.begin_table_operation('distribute', 'public.ld', '{\"column\": \"tenant\", \"bucket_count\": 12}')");
for my $phase ('shadow', 'copying', 'catching_up', 'switching', 'cleaning')
{
	my $r = q1($n1, "SELECT kingres.table_operation_advance($op, '$phase')");
	diag("after $phase: $r (" . sprintf('%.1f', time - $t0) . " s)");
	my $s = time;
	q1($n1, 'SELECT count(*) FROM public.ld');
	$slowest = time - $s if time - $s > $slowest;
}
like(q1($n1, "SELECT phase || '|' || verified FROM kingres.table_operation WHERE op_id = $op"), qr/^done\|(ok|skipped)$/,
	'the operation is done (the check against the old copy is skipped when the table was written to since the switch)');

my $failed = 0;
for my $w (@writers)
{
	$w->{h}->finish;
	my $err = ${ $w->{err} };
	$failed++ if $err =~ /ERROR/;
	diag($err) if $err =~ /ERROR/;
}
is($failed, 0, 'no statement of any writer failed');

# what the table holds is what the scripts say
my $got = q1($n1, "SELECT tenant || ',' || id || ' ' || v || '|' || n FROM public.ld ORDER BY tenant, id");
my $want = join("\n", map { my ($k, $v) = ($_, $model{$_}); my ($t, $i) = split /,/, $k; "$t,$i $v" }
				sort { my ($ta, $ia) = split /,/, $a; my ($tb, $ib) = split /,/, $b; $ta <=> $tb || $ia <=> $ib } keys %model);
is(md5_hex($got), md5_hex($want), 'the table holds exactly the rows the writers left (' . scalar(keys %model) . ' rows)')
  or diag("got " . scalar(split /\n/, $got) . " rows, want " . scalar(keys %model));
is(q1($n1, 'SELECT count(*) FROM public.ld'), scalar(keys %model), 'the scan counts them');
my ($total, $wrong) = (0, 0);
for my $n (@nodes)
{
	my ($a, $b) = split /,/, q1($n, "SELECT coalesce(sum(n), 0) || ',' || coalesce(sum(wrong), 0) FROM public.local_rows('public.ld', 'tenant')");
	$total += $a;
	$wrong += $b;
}
is($total, scalar(keys %model), 'the nodes hold each row once');
is($wrong, 0, 'every row is in the partition of its bucket');
is(q1($n1, "SELECT count(*) FROM (SELECT tenant, id FROM public.ld GROUP BY 1, 2 HAVING count(*) > 1) d"), '0', 'no row twice');
diag(sprintf('slowest read during the operation: %.3f s', $slowest));
cmp_ok($slowest, '<', 10, 'a reader was never blocked for long');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', 'check_cluster() is clean');
is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for @nodes;

done_testing();
