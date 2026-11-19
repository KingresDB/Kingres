# contrib/kingres_columnar/t/057_unique_standby.pl
#
# C6c: indexes on a standby, and a prepared transaction that holds a key.
#  1. A hot standby replays the index pages and the VACUUM of an index (the reservation
#     and the stripe records come with it; wal_consistency_checking is on for B-tree and
#     for the resource manager).  While the primary inserts, rolls back and vacuums, a
#     session on the standby reads through the index in a loop and gets no error and
#     only the committed rows; afterwards the standby gives the primary's answer for
#     every range, and columnar_check_indexes finds nothing on it.
#  2. A session that waits for a key that a prepared transaction holds is woken by
#     COMMIT PREPARED (duplicate error) or ROLLBACK PREPARED (the key is taken).
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

my $primary = new_node('usp', init => { allows_streaming => 1 },
	conf => "wal_keep_size = '300MB'\nwal_consistency_checking = 'KingresColumnar, btree'\n");
$primary->safe_psql('postgres', q{
	CREATE TABLE t (k int, v text) USING columnar;
	CREATE UNIQUE INDEX t_k ON t (k);
	CREATE INDEX t_v ON t (v, k);
	INSERT INTO t SELECT g, 'live' || (g % 50) FROM generate_series(1, 30000) g;
});
$primary->backup('bk0');
my $standby = PostgreSQL::Test::Cluster->new('usb');
$standby->init_from_backup($primary, 'bk0', has_streaming => 1);
$standby->append_conf('postgresql.conf', "hot_standby = on\nmax_standby_streaming_delay = '5s'\nhot_standby_feedback = on\n");
$standby->start;
$primary->wait_for_catchup($standby);

# the reader on the standby: it asks for the live rows again and again and records every
# answer that differs from 30000 rows (no committed row is ever removed) or any error
my $reader = $standby->background_psql('postgres', on_error_stop => 0);
$reader->query("SET enable_seqscan = off; SET enable_bitmapscan = off;");
my $bad = 0;
my $errs = '';
for my $round (1 .. 6)
{
	$primary->safe_psql('postgres', "BEGIN; INSERT INTO t SELECT 1000000 * $round + g, 'gone' FROM generate_series(1, 60000) g; ROLLBACK;");
	$primary->safe_psql('postgres', "INSERT INTO t SELECT 100000 * $round + g, 'new' FROM generate_series(1, 500) g;");
	$primary->safe_psql('postgres', "VACUUM t;");
	for (1 .. 5)
	{
		$reader->{stderr} = '';
		my $n = $reader->query("SELECT count(*) FROM t WHERE k BETWEEN 1 AND 30000;");
		$errs .= $reader->{stderr};
		$bad++ if $n !~ /\b30000\b/;
	}
}
$primary->wait_for_catchup($standby);
is($errs, '', 'the reader on the standby got no error while the primary vacuumed');
is($bad, 0, 'the reader always found the 30000 live rows');
for my $r ("1 AND 30000", "100001 AND 100500", "500001 AND 700000", "1000001 AND 9000000")
{
	my $q = "SET enable_seqscan = off; SET enable_bitmapscan = off; SELECT count(*) || ':' || coalesce(sum(k), 0) FROM t WHERE k BETWEEN $r";
	is(q1($standby, $q), q1($primary, $q), "range $r: the standby gives the primary's answer through the index");
}
is(q1($standby, "SELECT count(*) FROM kingres.columnar_check_indexes('t')"), 0, 'columnar_check_indexes finds nothing on the standby');
is(q1($primary, "SELECT count(*) FROM kingres.columnar_check_indexes('t')"), 0, '... nor on the primary');
is(q1($standby, "SET enable_seqscan = off; SET enable_bitmapscan = off; SELECT count(*) FROM t WHERE v = 'live7'"),
   q1($standby, "SET enable_indexscan = off; SET enable_bitmapscan = off; SELECT count(*) FROM t WHERE v = 'live7'"),
   'the second index agrees with the table scan on the standby');
$reader->quit;
$standby->stop;

# ---------------------------------------------------------------------------
# 2. a waiter and a prepared transaction
# ---------------------------------------------------------------------------
for my $how ('commit', 'rollback')
{
	my $key = $how eq 'commit' ? 5000001 : 5000002;
	my $holder = $primary->background_psql('postgres', on_error_stop => 0);
	$holder->query("BEGIN; INSERT INTO t VALUES ($key, 'held'); PREPARE TRANSACTION 'pt_$how';");
	my $waiter = $primary->background_psql('postgres', on_error_stop => 0);
	$waiter->{stderr} = '';
	$waiter->query_until(qr/^started$/m, "\\echo started\nINSERT INTO t VALUES ($key, 'waiter');\n");
	my $w = 0;
	for (1 .. 200)
	{
		last if ($w = q1($primary, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'")) >= 1;
		select(undef, undef, undef, 0.05);
	}
	is($w, 1, "$how: the waiter waits for the prepared transaction");
	q1($primary, ($how eq 'commit' ? "COMMIT" : "ROLLBACK") . " PREPARED 'pt_$how'");
	$waiter->query("SELECT 'sync';");
	if ($how eq 'commit') { like($waiter->{stderr}, qr/duplicate key/, 'COMMIT PREPARED: the waiter gets the duplicate error'); }
	else { is($waiter->{stderr}, '', 'ROLLBACK PREPARED: the waiter takes the key'); }
	is(q1($primary, "SELECT count(*) FROM t WHERE k = $key"), 1, "$how: the key is in the table once");
	$holder->quit;
	$waiter->quit;
}
is(q1($primary, "SELECT count(*) - count(DISTINCT k) FROM t"), 0, 'no key twice');
is(q1($primary, "SELECT count(*) FROM kingres.columnar_check_indexes('t')"), 0, 'columnar_check_indexes finds nothing');
$primary->stop;
done_testing();
