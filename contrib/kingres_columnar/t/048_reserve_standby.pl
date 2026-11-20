# contrib/kingres_columnar/t/048_reserve_standby.pl
#
# C6a: reservations on a standby and in a base backup.
#
#   1. a hot standby replays the reservations (the metapage images of the ALLOC and
#      of the RETURN record, the heap rows of the reserved and of the written stripe
#      rows) and reads the same rows with the same TIDs as the primary, for
#      transactions with savepoints, flushes that a read caused, PREPARE
#      TRANSACTION and COMMIT PREPARED, and rolled back ones; its metapage hands out
#      the same next row number; the log of the primary holds the RETURN records
#      (pg_walinspect); wal_consistency_checking for the resource manager is on, so
#      redo is compared with the images it restores;
#   2. a base backup taken while reservations are open (a writer that waits after
#      its reservation, one that waits with its pages written) restores to a clean
#      state: the committed rows, no reserved row visible, columnar_check clean, new
#      rows with new TIDs; the writers finish on the primary and the restored node
#      does not have their rows.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1 rsql fp same_as_twin tids_ok check_empty wait_waiting reserve_on);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $primary = new_node('rsp', init => { allows_streaming => 1 },
	conf => "wal_keep_size = '200MB'\nwal_consistency_checking = 'KingresColumnar'\n");
my $KEY = 779;

$primary->safe_psql('postgres', q{
	CREATE EXTENSION pg_walinspect;
	CREATE TABLE c (a int, b text) USING columnar;
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE ch (a int, b text);
	CREATE TABLE ret (a int, t tid);
	CREATE SEQUENCE sq;
});
$primary->backup('bk0');
my $standby = PostgreSQL::Test::Cluster->new('rss');
$standby->init_from_backup($primary, 'bk0', has_streaming => 1);
$standby->append_conf('postgresql.conf', "hot_standby = on\nmax_standby_streaming_delay = '1s'\n");
$standby->start;

my $start_lsn = q1($primary, "SELECT pg_current_wal_lsn()");

sub ins
{
	my ($n) = @_;
	return "WITH s AS (SELECT nextval('sq')::int a FROM generate_series(1, $n)), i AS (INSERT INTO c SELECT a, 'v' || a FROM s RETURNING a, ctid), j AS (INSERT INTO ch SELECT a, 'v' || a FROM s) INSERT INTO ret SELECT a, ctid FROM i;";
}

# ---------------------------------------------------------------------------
# 1. the standby
# ---------------------------------------------------------------------------
my @txns = (
	"BEGIN; " . ins(10) . " COMMIT;",
	"BEGIN; " . ins(1500) . " COMMIT;",
	"BEGIN; " . ins(30) . " SAVEPOINT x; " . ins(40) . " SAVEPOINT y; " . ins(5) . " ROLLBACK TO y; " . ins(6) . " RELEASE x; " . ins(7) . " COMMIT;",
	"BEGIN; " . ins(300) . " SET LOCAL debug_parallel_query = on; SELECT count(*) FROM c; SET LOCAL debug_parallel_query = off; " . ins(200) . " COMMIT;",
	"BEGIN; " . ins(50) . " PREPARE TRANSACTION 'sp1';",
	"COMMIT PREPARED 'sp1';",
	"BEGIN; " . ins(100) . " ROLLBACK;",
	"BEGIN; " . ins(60) . " PREPARE TRANSACTION 'sp2';",
	"ROLLBACK PREPARED 'sp2';",
	"BEGIN; " . ins(2200) . " COMMIT;",
);
for my $t (@txns)
{
	my ($rc, $out, $err) = rsql($primary, $t);
	is($rc, 0, 'primary: ' . substr($t, 0, 40)) or diag($err);
}
$primary->wait_for_catchup($standby);

sub standby_check
{
	my ($what) = @_;
	is(fp($standby, 'c'), fp($primary, 'c'), "$what: the standby reads the rows of the primary");
	is(fp($standby, 'c'), fp($primary, 'ch'), "$what: ... which are those of the heap twin");
	is(q1($standby, "SELECT count(DISTINCT ctid::text) FROM c"), q1($standby, "SELECT count(*) FROM c"), "$what: no two rows share a TID on the standby");
	is(q1($standby, "SELECT count(*) FROM c x JOIN ret r USING (a) WHERE x.ctid::text = r.t::text"), q1($standby, "SELECT count(*) FROM c"), "$what: every row on the standby has the TID its insert returned on the primary");
	is(q1($standby, "SELECT string_agg(a::text || ':' || ctid::text, ',' ORDER BY a) FROM c"), q1($primary, "SELECT string_agg(a::text || ':' || ctid::text, ',' ORDER BY a) FROM c"), "$what: the TIDs are the same on both");
	is(q1($standby, "SELECT stripes_str('c')"), q1($primary, "SELECT stripes_str('c')"), "$what: the stripes are the same on both");
	is(q1($standby, "SELECT meta_next_row('c')"), q1($primary, "SELECT meta_next_row('c')"), "$what: the metapage hands out the same next row number on both");
	check_empty($standby, "$what, standby");
}
standby_check('after the load');

# the log of the primary: the reservations and the tails given back
my ($recs, $returns, $descs) = split /\|/, q1($primary, qq{
	SELECT count(*), count(*) FILTER (WHERE record_type = 'RETURN'),
	       count(*) FILTER (WHERE record_type = 'RETURN' AND description LIKE '%given back%')
	  FROM pg_get_wal_records_info('$start_lsn', pg_current_wal_lsn())
	 WHERE resource_manager = 'KingresColumnar'});
cmp_ok($returns, '>', 0, "the log of the primary has RETURN records ($returns of $recs records of the resource manager)");
is($descs, $returns, '... which say what they give back');

# more writes while the standby reads, and the tail of an open reservation
my $bg = $primary->background_psql('postgres');
$bg->query_safe(reserve_on() . "BEGIN; " . ins(5));
my ($rc, $out, $err) = rsql($primary, "BEGIN; " . ins(20) . " COMMIT;");
$primary->wait_for_catchup($standby);
is(q1($standby, "SELECT count(*) FROM c"), q1($primary, "SELECT count(*) FROM c"), 'a writer in progress is not seen by the standby, a committed one is');
$bg->query_safe("COMMIT;");
$bg->quit;
$primary->wait_for_catchup($standby);
standby_check('after a writer that held a reservation while another committed');

# ---------------------------------------------------------------------------
# 2. a base backup with reservations open
# ---------------------------------------------------------------------------
my $ctl = $primary->background_psql('postgres');
$ctl->query_safe("SELECT pg_advisory_lock($KEY, 0);");
my @holders;
for my $point (2, 3)
{
	my $h = $primary->background_psql('postgres');
	my $sql = $point == 2 ? "BEGIN; INSERT INTO c VALUES (990000, 'held');" : "BEGIN; INSERT INTO c VALUES (990001, 'held'); COMMIT;";
	$h->query_until(qr/^go$/m, reserve_on() . "SET kingres_columnar.reserve_gate = $KEY; SET kingres_columnar.reserve_gate_at = $point;\n\\echo go\n$sql\n");
	push @holders, $h;
}
ok(wait_waiting($primary, 2), 'two writers wait with reservations open (one before its first row, one with its pages written)');
$primary->backup('bk1');
my $restored = PostgreSQL::Test::Cluster->new('rsr');
$restored->init_from_backup($primary, 'bk1');
$restored->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_columnar'\nmax_prepared_transactions = 10\n");
$restored->start;
my $want_rows = q1($primary, "SELECT count(*) FROM c");
is(q1($restored, "SELECT count(*) FROM c"), $want_rows, 'the restored node holds the committed rows and none of the writers that were open');
is(q1($restored, "SELECT count(*) FROM c WHERE a >= 990000"), 0, '... not the rows of the open writers');
is(q1($restored, "SELECT count(*) FROM kingres.columnar_stripe WHERE reserved"), 0, '... no reserved row is visible');
check_empty($restored, 'the restored node');
is(q1($restored, "SELECT count(DISTINCT ctid::text) FROM c"), $want_rows, '... and no two rows share a TID');
is(q1($restored, "SELECT count(*) FROM c x JOIN ret r USING (a) WHERE x.ctid::text = r.t::text"), $want_rows, '... every row has the TID its insert returned');
($rc, $out, $err) = rsql($restored, "INSERT INTO c SELECT g, 'new' FROM generate_series(100000, 100100) g;");
is($rc, 0, 'the restored node takes new rows') or diag($err);
is(q1($restored, "SELECT count(DISTINCT ctid::text) = count(*) FROM c"), 't', '... with TIDs of their own');
check_empty($restored, 'the restored node after a write');
$restored->stop;

# the writers finish on the primary: their rows are there, the restored node did not have them
$ctl->query_safe("SELECT pg_advisory_unlock($KEY, 0);");
$holders[0]->query_safe("COMMIT;");
$_->quit for @holders;
$ctl->quit;
is(q1($primary, "SELECT count(*) FROM c WHERE a >= 990000"), 2, 'on the primary the two writers committed');
$primary->wait_for_catchup($standby);
is(q1($standby, "SELECT count(DISTINCT ctid::text) = count(*) FROM c"), 't', 'and the standby has them with TIDs of their own');
check_empty($primary, 'the primary at the end');
check_empty($standby, 'the standby at the end');

my $log = slurp_file($standby->logfile);
unlike($log, qr/PANIC|TRAP:|inconsistent page|is corrupt/, 'the standby log has no PANIC, assertion or inconsistent page');
$log = slurp_file($primary->logfile);
unlike($log, qr/PANIC|TRAP:|inconsistent page|is corrupt/, 'the primary log has no PANIC, assertion or inconsistent page');

$standby->stop;
$primary->stop;
done_testing();
