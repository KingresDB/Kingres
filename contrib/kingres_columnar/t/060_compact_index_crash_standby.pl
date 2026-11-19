# contrib/kingres_columnar/t/060_compact_index_crash_standby.pl
#
# C6d: compaction of a table with indexes, in a crash and on a standby.
#  1. A compaction that is open (not committed) when the server stops in the immediate mode:
#     after the restart the table has its rows, in their old stripes, and every index
#     finds them; the entries the compaction made name numbers of stripes that never
#     committed and are filtered; a duplicate key is still refused, the compaction can be run
#     again and commits, VACUUM removes the entries of the numbers that no stripe has, and
#     columnar_check_indexes finds nothing at any of these points.  Then a crash right after a
#     compaction committed: the copies are the table's rows and the indexes find them.
#  2. A hot standby replays compactions (and the vacuums after them) while a session on the
#     standby reads the table by its indexes in a loop: no error, every answer is the
#     count of rows, and afterwards the standby gives the primary's answers.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $primary = new_node('cip', init => { allows_streaming => 1 },
	conf => "wal_keep_size = '300MB'\nwal_consistency_checking = 'KingresColumnar, btree'\n");
$primary->safe_psql('postgres', q{
	CREATE TABLE t (k int, v text) USING columnar;
	CREATE UNIQUE INDEX t_k ON t (k);
	CREATE INDEX t_v ON t (v, k);
});
$primary->safe_psql('postgres', "INSERT INTO t SELECT $_ * 100 + g, 'row' || (g % 20) FROM generate_series(1, 100) g") for 0 .. 11;

sub via_index
{
	my ($n, $where) = @_;
	return q1($n, "SET enable_seqscan = off; SET enable_bitmapscan = off; SELECT count(*) || ':' || coalesce(sum(k), 0) FROM t WHERE $where");
}
my $all_idx = "k BETWEEN 0 AND 100000000";
my $want = q1($primary, "SET enable_indexscan = off; SELECT count(*) || ':' || sum(k) FROM t");
my $want_v = q1($primary, "SET enable_indexscan = off; SELECT count(*) || ':' || sum(k) FROM t WHERE v = 'row7'");

# ---------------------------------------------------------------------------
# 1. the crash
# ---------------------------------------------------------------------------
$primary->backup('bk0');    # for the standby of part 2: taken before the crashes
my $c = $primary->background_psql('postgres', on_error_stop => 0);
$c->query("BEGIN; SELECT kingres.columnar_compact('t', 1000000);");
is(q1($primary, "SELECT count(*) FROM kingres.columnar_stripes('t')"), 12, 'the open compaction is not visible: 12 stripes');
$primary->stop('immediate');
$primary->start;
is(q1($primary, "SELECT count(*) FROM kingres.columnar_stripes('t')"), 12, 'after the crash the table has its 12 stripes');
is(via_index($primary, $all_idx), $want, 'every row is found by the unique index');
is(via_index($primary, "v = 'row7'"), $want_v, 'and by the other index');
is(q1($primary, "SELECT count(*) FROM kingres.columnar_check_indexes('t')"), 0, 'columnar_check_indexes finds nothing');
my ($rc, $out, $err) = $primary->psql('postgres', "INSERT INTO t VALUES (150, 'dup')");
like($err, qr/duplicate key/, 'a duplicate is refused');
is(q1($primary, "SELECT kingres.columnar_compact('t', 1000000) > 0"), 't', 'the compaction can be run again');
is(q1($primary, "SELECT count(*) FROM kingres.columnar_stripes('t')"), 1, 'and the stripes are one');
is(via_index($primary, $all_idx), $want, 'every row is found by the index after it');
$primary->stop('immediate');
$primary->start;
is(via_index($primary, $all_idx), $want, 'after a crash right after the commit the copies are found by the index');
is(via_index($primary, "v = 'row7'"), $want_v, '... and by the other index');
q1($primary, "VACUUM t");
is(via_index($primary, $all_idx), $want, 'after VACUUM');
is(q1($primary, "SELECT count(*) FROM kingres.columnar_check_indexes('t')"), 0, 'columnar_check_indexes finds nothing');
is(q1($primary, "SELECT count(*) - count(DISTINCT k) FROM t"), 0, 'no key twice');
($rc, $out, $err) = $primary->psql('postgres', "INSERT INTO t VALUES (250, 'dup')");
like($err, qr/duplicate key/, 'a duplicate of a copied row is refused');

# ---------------------------------------------------------------------------
# 2. the standby
# ---------------------------------------------------------------------------
$primary->safe_psql('postgres', "TRUNCATE t");
$primary->safe_psql('postgres', "INSERT INTO t SELECT $_ * 100 + g, 'row' || (g % 20) FROM generate_series(1, 100) g") for 0 .. 11;
$primary->backup('bk1');
my $standby = PostgreSQL::Test::Cluster->new('cis');
$standby->init_from_backup($primary, 'bk1', has_streaming => 1);
$standby->append_conf('postgresql.conf', "hot_standby = on\nmax_standby_streaming_delay = '5s'\nhot_standby_feedback = on\n");
$standby->start;
$primary->wait_for_catchup($standby);
my $n0 = q1($primary, "SELECT count(*) FROM t");
my $reader = $standby->background_psql('postgres', on_error_stop => 0);
$reader->query("SET enable_seqscan = off; SET enable_bitmapscan = off;");
my ($errs, $bad) = ('', 0);
for my $round (1 .. 8)
{
	# new small stripes, then a compaction, then a vacuum, while the reader reads
	$primary->safe_psql('postgres', "INSERT INTO t SELECT 100000 * $round + $_ * 10 + g, 'row' || (g % 20) FROM generate_series(1, 10) g") for 0 .. 7;
	$n0 += 80;
	$primary->safe_psql('postgres', "SELECT kingres.columnar_compact('t', 1000000)");
	for (1 .. 4)
	{
		$reader->{stderr} = '';
		my $r = $reader->query("SELECT count(*) FROM t WHERE k BETWEEN 0 AND 100000000;");
		$errs .= $reader->{stderr};
		$r =~ /(\d+)/;
		$bad++ if !defined $1 || $1 > $n0 || $1 < $n0 - 80;
	}
	$primary->safe_psql('postgres', "VACUUM t") if $round % 2 == 0;
}
$primary->wait_for_catchup($standby);
is($errs, '', 'the reader on the standby got no error');
is($bad, 0, 'every answer of the reader was a count of rows the table had');
is(via_index($standby, $all_idx), via_index($primary, $all_idx), 'the standby gives the primary\'s answer by the unique index');
is(via_index($standby, "v = 'row7'"), via_index($primary, "v = 'row7'"), '... and by the other index');
is(q1($standby, "SELECT count(*) FROM kingres.columnar_check_indexes('t')"), 0, 'columnar_check_indexes finds nothing on the standby');
$reader->quit;
$standby->stop;
$primary->stop;
done_testing();
