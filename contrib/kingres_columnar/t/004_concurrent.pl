# contrib/kingres_columnar/t/004_concurrent.pl
#
# Concurrent loaders, readers and maintenance on one columnar table.
#
# Eight loader sessions insert rows with unique ids in known ranges, in many
# transactions of kinds that stress the write buffer differently: plain
# INSERT, subtransactions (SAVEPOINT released and rolled back), COPY FROM
# STDIN, and transactions bigger than a stripe, so that stripes are written
# in the middle of a transaction and then rolled back.  Some transactions
# roll back.  Four readers and a maintenance session (VACUUM and
# columnar_compact()) run at the same time.
#
# Every committed transaction inserts a multiple of K rows, so in READ
# COMMITTED a reader must never see a count that is not a multiple of K, and
# the count it sees must never go down (the workload only inserts).  In the
# end the set of ids must be exactly the set of committed ids, without
# duplicates, and the stripes' page ranges must be contiguous and disjoint
# (design V7).  Each session is a psql running a generated script, so the
# sessions are real concurrent backends; a lock gate lets them all start
# together, so the overlap does not depend on how fast psql starts.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use Digest::MD5 qw(md5_hex);
use IPC::Run;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $K = 50;                # rows per ordinary committed transaction
my $TXNS = 500;            # transactions per loader (big loaders do fewer)
my $BIG_TXNS = 30;

my $node = PostgreSQL::Test::Cluster->new('colconc');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
max_connections = 40
});
$node->start;
$node->safe_psql('postgres', q{
	CREATE EXTENSION kingres_columnar;
	CREATE TABLE c (id bigint, loader int, txn int, pad text) USING columnar;
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
});

# ---------------------------------------------------------------------------
# Generate the scripts.  %committed collects the ids that must exist at the end.
# ---------------------------------------------------------------------------

my %committed;
my $GATE = "SELECT pg_advisory_lock_shared(4242); SELECT pg_advisory_unlock_shared(4242);\n";

# The ids of transaction $k of loader $l start at a base far from every other.
sub base { my ($l, $k) = @_; return ($l + 1) * 10_000_000 + $k * 3_000; }

sub ins
{
	my ($l, $k, $lo, $n) = @_;
	my $hi = $lo + $n - 1;
	return "INSERT INTO c SELECT i, $l, $k, md5(i::text) FROM generate_series($lo, $hi) i;\n";
}

# A transaction sees its own rows, buffered or written.
sub own_check
{
	my ($l, $k, $want) = @_;
	return "SELECT CASE WHEN count(*) = $want THEN 'own ok' ELSE 'own bad $l/$k ' || count(*) END "
	  . "FROM c WHERE loader = $l AND txn = $k;\n";
}

sub mark
{
	my ($l, $k, $lo, $n) = @_;
	$committed{$_} = 1 for ($lo .. $lo + $n - 1);
}

my %script;    # name => sql

for my $l (0 .. 7)
{
	my $sql = $GATE;
	my $big = $l >= 6;
	my $n = $big ? $BIG_TXNS : $TXNS;
	for my $k (0 .. $n - 1)
	{
		my $b = base($l, $k);
		my $commit = 1;
		$sql .= "BEGIN;\n";
		if ($l <= 1)
		{
			# plain INSERT ... SELECT; every fifth transaction rolls back
			$sql .= ins($l, $k, $b, $K);
			$sql .= own_check($l, $k, $K) if $k % 10 == 0;
			$commit = ($k % 5 != 4);
			mark($l, $k, $b, $K) if $commit;
		}
		elsif ($l <= 3)
		{
			# 20 rows, a rolled back savepoint (15 rows), a released one
			# (15 rows), 15 rows after it: 50 rows committed.
			$sql .= ins($l, $k, $b, 20);
			$sql .= "SAVEPOINT a;\n" . ins($l, $k, $b + 20, 15) . "ROLLBACK TO a;\n";
			$sql .= "SAVEPOINT b;\n" . ins($l, $k, $b + 35, 15) . "RELEASE b;\n";
			$sql .= ins($l, $k, $b + 50, 15);
			$sql .= own_check($l, $k, $K) if $k % 10 == 0;
			$commit = ($k % 7 != 6);
			if ($commit)
			{
				mark($l, $k, $b, 20);
				mark($l, $k, $b + 35, 30);
			}
		}
		elsif ($l <= 5)
		{
			# COPY FROM STDIN, in one or two COPYs; every sixth rolls back
			my @parts = ($k % 2) ? ([ $b, 25 ], [ $b + 25, 25 ]) : ([ $b, 50 ]);
			for my $p (@parts)
			{
				$sql .= "COPY c (id, loader, txn, pad) FROM STDIN;\n";
				$sql .= join("\t", $_, $l, $k, md5_hex($_)) . "\n"
				  for ($p->[0] .. $p->[0] + $p->[1] - 1);
				$sql .= "\\.\n";
			}
			$sql .= own_check($l, $k, $K) if $k % 10 == 0;
			$commit = ($k % 6 != 5);
			mark($l, $k, $b, $K) if $commit;
		}
		elsif ($l == 6)
		{
			# bigger than a stripe (1250 rows, stripes of 1000): a stripe is
			# written before the commit; every fourth transaction rolls back
			# after that.
			$sql .= ins($l, $k, $b, 1250);
			$sql .= own_check($l, $k, 1250);
			$commit = ($k % 4 != 3);
			mark($l, $k, $b, 1250) if $commit;
		}
		else
		{
			# 600 rows, a savepoint whose 1100 rows fill a stripe and are
			# rolled back, a savepoint with 650 rows released: 1250 committed.
			$sql .= ins($l, $k, $b, 600);
			$sql .= "SAVEPOINT s;\n" . ins($l, $k, $b + 600, 1100) . "ROLLBACK TO s;\n";
			$sql .= "SAVEPOINT t;\n" . ins($l, $k, $b + 1700, 650) . "RELEASE t;\n";
			$sql .= own_check($l, $k, 1250);
			$commit = ($k % 4 != 3);
			if ($commit)
			{
				mark($l, $k, $b, 600);
				mark($l, $k, $b + 1700, 650);
			}
		}
		$sql .= $commit ? "COMMIT;\n" : "ROLLBACK;\n";
	}
	$script{"loader$l"} = $sql;
}

# Readers.  Each prints tagged lines that are checked below.
{
	my $s = $GATE;
	$s .= "SELECT 'cnt ' || count(*) || ' ' || count(DISTINCT id) FROM c;\n" x 400;
	$script{reader0} = $s;

	$s = $GATE;
	for (1 .. 80)
	{
		$s .= "BEGIN ISOLATION LEVEL REPEATABLE READ;\n"
		  . "SELECT count(*) AS a FROM c \\gset\n"
		  . "SELECT CASE WHEN count(*) = :a THEN 'rr ok' ELSE 'rr bad ' || :a || ' ' || count(*) END FROM c;\n"
		  . "COMMIT;\n";
	}
	$script{reader1} = $s;

	# per loader counts in one snapshot: a multiple of the transaction size
	$s = $GATE;
	$s .= "SELECT 'per ' || loader || ' ' || count(*) FROM c GROUP BY loader;\n" x 250;
	$script{reader2} = $s;

	# a predicate on id (chunk skipping) and an aggregate over several columns
	$s = $GATE;
	$s .= "SELECT 'cnt ' || count(*) || ' ' || count(*) FROM c WHERE id >= 30000000;\n" x 250;
	$script{reader3} = $s;
}

# Maintenance: compaction and VACUUM run while the others write and read.
{
	my $s = $GATE;
	$s .= "SELECT 'compact ' || kingres.columnar_compact('c', 2000);\nVACUUM c;\n" x 20;
	$script{maint} = $s;
delete $script{maint} if $ENV{KC_NOMAINT};
}

# ---------------------------------------------------------------------------
# Run them all at once.  A session holds the gate until every script waits.
# ---------------------------------------------------------------------------

my $dir = PostgreSQL::Test::Utils::tempdir;
my $gate = $node->background_psql('postgres');
$gate->query_safe('SELECT pg_advisory_lock(4242)');

my (%h, %outf);
for my $name (sort keys %script)
{
	my $f = "$dir/$name.sql";
	open(my $fh, '>', $f) or die "cannot write $f: $!";
	print $fh $script{$name};
	close($fh);
	$outf{$name} = "$dir/$name.out";
	$h{$name} = IPC::Run::start(
		[ 'psql', '-X', '-At', '-q', '-v', 'ON_ERROR_STOP=1',
		  '-d', $node->connstr('postgres'), '-f', $f ],
		'>', $outf{$name}, '2>', "$dir/$name.err");
}
my $nscripts = scalar keys %script;
$node->poll_query_until('postgres',
	"SELECT count(*) = $nscripts FROM pg_locks WHERE locktype = 'advisory' AND NOT granted");
$gate->query_safe('SELECT pg_advisory_unlock(4242)');
$gate->quit;

for my $name (sort keys %h)
{
	IPC::Run::finish($h{$name});
	is($h{$name}->result(0), 0, "$name finished without error")
	  or diag(slurp_file("$dir/$name.err"));
}

sub lines { my ($n) = @_; my $t = slurp_file($outf{$n}); return grep { length } split /\n/, $t; }

# ---------------------------------------------------------------------------
# What the sessions saw
# ---------------------------------------------------------------------------

for my $l (0 .. 3, 4, 5, 6, 7)
{
	my @bad = grep { /bad/ } lines("loader$l");
	is(scalar(@bad), 0, "loader $l saw its own rows (buffered and written) correctly")
	  or diag(join("\n", @bad));
}

for my $r ('reader0', 'reader3')
{
	my @c = map { (split ' ')[1] } grep { /^cnt / } lines($r);
	my ($nonmono, $notmult, $dups) = (0, 0, 0);
	my $prev = 0;
	for my $line (grep { /^cnt / } lines($r))
	{
		my (undef, $n, $d) = split ' ', $line;
		$nonmono++ if $n < $prev;
		$prev = $n;
		$notmult++ if $n % $K != 0;
		$dups++ if $n != $d;
	}
	my %distinct = map { $_ => 1 } @c;
	cmp_ok(scalar(keys %distinct), '>', 1, "$r saw the table grow while it ran");
	is(scalar(@c), $r eq 'reader0' ? 400 : 250, "$r got all its results");
	is("$nonmono/$notmult/$dups", '0/0/0',
		"$r: counts never decrease, are multiples of $K, no duplicates");
}
is(scalar(grep { /^rr ok/ } lines('reader1')), 80,
	'reader1: repeatable read gives the same count twice in a transaction');
{
	my @per = grep { /^per / } lines('reader2');
	my $notmult = grep { (split ' ')[2] % $K != 0 } @per;
	cmp_ok(scalar(@per), '>', 0, 'reader2 saw per-loader counts');
	is($notmult, 0, "reader2: every per-loader count is a multiple of $K (no partial transaction)");
}

# ---------------------------------------------------------------------------
# The end state
# ---------------------------------------------------------------------------

my @ids = sort { $a <=> $b } keys %committed;
my $want = scalar(@ids) . '|' . scalar(@ids) . '|' . md5_hex(join(',', @ids));
my $got = $node->safe_psql('postgres',
	q{SELECT count(*) || '|' || count(DISTINCT id) || '|' || md5(string_agg(id::text, ',' ORDER BY id)) FROM c});
is($got, $want, 'the ids are exactly the committed ids, each once')
  or diag($node->safe_psql('postgres', q{SELECT loader, txn, count(*) AS dup_ids, min(id), max(id) FROM (SELECT id, loader, txn FROM c GROUP BY id, loader, txn HAVING count(*) > 1) d GROUP BY loader, txn ORDER BY loader, txn}) . "
stripes: " . $node->safe_psql('postgres', q{SELECT count(*) FROM kingres.columnar_stripes('c')}));;
is($node->safe_psql('postgres', q{SELECT count(*) FROM c WHERE pad <> md5(id::text)}),
	'0', 'no row changed its contents');
is($node->safe_psql('postgres', q{SELECT rows FROM kingres.columnar_info('c')}),
	scalar(@ids), 'columnar_info agrees with the count');
is($node->safe_psql('postgres', q{SELECT count(*) FROM kingres.columnar_check()}),
	'0', 'columnar_check() reports nothing');

# Stripe pages of one relation: each stripe is a run [first_block,
# first_block + block_count) that does not touch the metapage, lies inside the
# file, and overlaps no other stripe (V7).
my $stripe_sql = q{
	WITH s AS (SELECT * FROM kingres.columnar_stripe
				WHERE filenode = pg_relation_filenode('c'))
	SELECT (SELECT count(*) FROM s a JOIN s b ON a.stripe_id < b.stripe_id
				AND a.first_block < b.first_block + b.block_count
				AND b.first_block < a.first_block + a.block_count)
		 || '/' || (SELECT count(*) FROM s WHERE first_block < 1 OR block_count < 1)
		 || '/' || (SELECT count(*) FROM s
					 WHERE first_block + block_count > pg_relation_size('c') / 8192)
		 || '/' || (SELECT count(*) FROM s)
};
my ($overlap, $meta, $beyond, $nstripes) = split m{/}, $node->safe_psql('postgres', $stripe_sql);
is("$overlap/$meta/$beyond", '0/0/0',
	'stripe page ranges are disjoint, after the metapage and inside the file');
cmp_ok($nstripes, '>', 20, "there are many stripes ($nstripes)");

# Everything above again after a crash: nothing committed is lost, nothing
# rolled back comes back.
$node->stop('immediate');
$node->start;
is($node->safe_psql('postgres',
	q{SELECT count(*) || '|' || count(DISTINCT id) || '|' || md5(string_agg(id::text, ',' ORDER BY id)) FROM c}),
	$want, 'the same ids after a crash and recovery');
is($node->safe_psql('postgres', $stripe_sql) =~ m{^0/0/0/}, 1,
	'stripes still disjoint after recovery');

# More concurrent loading after recovery: writes go after the unreferenced
# pages left by the rolled-back transactions.
{
	my @h;
	for my $i (1 .. 4)
	{
		my $base = 90_000_000 + $i * 100_000;
		my $sql = '';
		for my $k (0 .. 19)
		{
			$sql .= "BEGIN;\n" . ins($i, $k, $base + $k * 2000, 1100)
			  . (($k % 5 == 4) ? "ROLLBACK;\n" : "COMMIT;\n");
			mark($i, $k, $base + $k * 2000, 1100) if $k % 5 != 4;
		}
		my $f = "$dir/after$i.sql";
		open(my $fh, '>', $f) or die;
		print $fh $sql;
		close($fh);
		push @h, IPC::Run::start(
			[ 'psql', '-X', '-At', '-q', '-v', 'ON_ERROR_STOP=1',
			  '-d', $node->connstr('postgres'), '-f', $f ],
			'>', "$dir/after$i.out", '2>', "$dir/after$i.err");
	}
	IPC::Run::finish($_) for @h;
	is(join(',', map { $_->result(0) } @h), '0,0,0,0', 'loading after recovery works');
}
@ids = sort { $a <=> $b } keys %committed;
is($node->safe_psql('postgres',
	q{SELECT count(*) || '|' || count(DISTINCT id) || '|' || md5(string_agg(id::text, ',' ORDER BY id)) FROM c}),
	scalar(@ids) . '|' . scalar(@ids) . '|' . md5_hex(join(',', @ids)),
	'ids after the second round are the committed ids');
is($node->safe_psql('postgres', $stripe_sql) =~ m{^0/0/0/}, 1,
	'stripes disjoint after the second round');
is($node->safe_psql('postgres', q{SELECT count(*) FROM kingres.columnar_check()}),
	'0', 'columnar_check() reports nothing at the end');

my $log = slurp_file($node->logfile);
unlike($log, qr/TRAP:|PANIC:/, 'no assertion failure or PANIC in the log');
unlike($log, qr/deadlock detected/, 'no deadlock between loaders');

$node->stop;
done_testing();
