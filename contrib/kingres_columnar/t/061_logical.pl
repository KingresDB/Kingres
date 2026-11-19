# contrib/kingres_columnar/t/061_logical.pl
#
# C7: logical decoding of the inserts of columnar tables (design 7.4 and 9.3, "006_logical").
# The publisher (wal_level = logical) has columnar tables in a publication; the subscriber has
# the same tables (columnar, one with a unique index) and a subscription with two-phase
# decoding.  After every step the subscriber's tables are compared with the publisher's row by
# row (the text of every row, in key order), not by a count or a hash:
#   1. the initial copy and later inserts (one row, many rows, COPY, a batch of 30 000 rows over
#      many stripes, values of many types, nulls, a 2 MB value);
#   2. subtransactions: a savepoint rolled back, nested ones, a release, a rollback of the whole
#      transaction, and a flush by memory pressure inside a subtransaction that is then rolled
#      back (its rows are in the log and must not reach the subscriber);
#   3. prepared transactions: nothing reaches the subscriber's tables before COMMIT PREPARED
#      (it holds the prepared transaction), a ROLLBACK PREPARED sends nothing;
#   4. the setting that reserves row numbers, a table with an index on the publisher;
#   5. concurrent sessions (pgbench);
#   6. TRUNCATE;
#   7. a crash of the publisher in the middle of a transaction and in the middle of a flush: after
#      the restart the subscription goes on, the committed rows are there once, the open
#      transaction's are not;
#   8. the log: LOGICAL_INSERT records exist (pg_walinspect), and test_decoding shows the rows;
#   9. a table that is not published has no effect on the subscriber.
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

my $pub = new_node('lpub', init => { allows_streaming => 'logical' },
	conf => "wal_level = logical\nmax_wal_senders = 10\nmax_replication_slots = 10\n");
my $sub = new_node('lsub', conf => "wal_level = logical\nmax_logical_replication_workers = 8\nmax_worker_processes = 16\n");
# a slot on the subscriber that watches what its own applied rows look like to a decoder (origin)
q1($sub, "SELECT pg_create_logical_replication_slot('td2', 'test_decoding')");

my $cols = q{id int, g int, v text, n numeric, a int[], j jsonb, t timestamptz, b bytea};
for my $n ($pub, $sub)
{
	q1($n, qq{
		CREATE TABLE c ($cols) USING columnar;
		SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
		CREATE TABLE ci ($cols) USING columnar;
		CREATE TABLE np (id int, v text) USING columnar;
	});
}
q1($sub, "CREATE UNIQUE INDEX c_id ON c (id)");
q1($pub, "CREATE UNIQUE INDEX ci_id ON ci (id); CREATE TABLE np2 (id int)");
q1($sub, "CREATE UNIQUE INDEX ci_id ON ci (id)");

# rows made with every column filled in different ways
sub rows_sql
{
	my ($tbl, $lo, $hi, $tag) = @_;
	return "INSERT INTO $tbl SELECT g, g % 7, CASE WHEN g % 11 = 0 THEN NULL ELSE '$tag' || g || repeat('x', g % 50) END,
	  CASE WHEN g % 13 = 0 THEN NULL ELSE g * 1.25 END, CASE WHEN g % 5 = 0 THEN NULL ELSE ARRAY[g, g + 1, NULL] END,
	  jsonb_build_object('k', g, 's', '$tag'), '2026-01-01 00:00:00+00'::timestamptz + (g || ' seconds')::interval,
	  CASE WHEN g % 17 = 0 THEN NULL ELSE decode(md5(g::text), 'hex') END
	  FROM generate_series($lo, $hi) g";
}

# the text of every row in key order
sub dumprows
{
	my ($node, $tbl) = @_;
	return q1($node, "SELECT coalesce(string_agg(t::text, E'\\n' ORDER BY id), '') FROM $tbl t");
}

sub same
{
	my ($what, @tbls) = @_;
	$pub->wait_for_catchup('sub1');
	for my $t (@tbls)
	{
		my ($p, $s) = (dumprows($pub, $t), dumprows($sub, $t));
		my ($np, $ns) = (q1($pub, "SELECT count(*) FROM $t"), q1($sub, "SELECT count(*) FROM $t"));
		is($ns, $np, "$what: $t has $np rows on both");
		ok($p eq $s, "$what: every row of $t is the publisher's row") or diag("first difference near row: " . join(' | ', grep { defined } (split /\n/, $p)[0 .. 1]));
	}
}

# ---------------------------------------------------------------------------
# 1. the initial copy and inserts
# ---------------------------------------------------------------------------
q1($pub, rows_sql('c', 1, 500, 'init'));
my $lsn0 = q1($pub, "SELECT pg_current_wal_lsn()");
q1($pub, "CREATE PUBLICATION pub1 FOR TABLE c, ci");
$sub->safe_psql('postgres', "CREATE SUBSCRIPTION sub1 CONNECTION '" . $pub->connstr . "' PUBLICATION pub1 WITH (two_phase = true)");
$sub->wait_for_subscription_sync($pub, 'sub1');
same('initial copy', 'c');

q1($pub, "INSERT INTO c (id, v) VALUES (1001, 'one row')");
q1($pub, "INSERT INTO c (id, v) VALUES (1002, 'two'), (1003, 'rows')");
q1($pub, rows_sql('c', 2000, 2600, 'ins'));
same('inserts', 'c');

q1($pub, rows_sql('c', 10000, 40000, 'big'));
same('30 001 rows over many stripes', 'c');
q1($pub, "INSERT INTO c (id, v) SELECT 50000, repeat('L', 2 * 1024 * 1024)");
same('a 2 MB value', 'c');
$pub->safe_psql('postgres', "COPY (SELECT g, g % 3, 'copy' || g FROM generate_series(60000, 60999) g) TO '" . $pub->basedir . "/c.csv' CSV");
$pub->safe_psql('postgres', "COPY c (id, g, v) FROM '" . $pub->basedir . "/c.csv' CSV");
same('COPY', 'c');

# ---------------------------------------------------------------------------
# 2. subtransactions
# ---------------------------------------------------------------------------
q1($pub, "BEGIN; " . rows_sql('c', 70001, 70010, 'a') . "; SAVEPOINT s1; " . rows_sql('c', 70011, 70020, 'b')
	. "; ROLLBACK TO s1; " . rows_sql('c', 70021, 70030, 'c') . "; SAVEPOINT s2; " . rows_sql('c', 70031, 70040, 'd')
	. "; SAVEPOINT s3; " . rows_sql('c', 70041, 70050, 'e') . "; ROLLBACK TO s3; RELEASE s2; COMMIT");
same('savepoints', 'c');
q1($pub, "BEGIN; " . rows_sql('c', 71001, 71100, 'r') . "; ROLLBACK");
same('a rolled back transaction', 'c');
# rows flushed inside a subtransaction by memory pressure, which is then rolled back
q1($pub, "SET kingres_columnar.write_buffer_memory = '1MB'; BEGIN; " . rows_sql('c', 72001, 72010, 'keep')
	. "; SAVEPOINT s; INSERT INTO c (id, v) SELECT 72100 + g, repeat('m', 5000) FROM generate_series(1, 2000) g; ROLLBACK TO s; "
	. rows_sql('c', 72011, 72020, 'keep2') . "; COMMIT");
same('flush by memory in a rolled back subtransaction', 'c');
is(q1($sub, "SELECT count(*) FROM c WHERE id BETWEEN 72101 AND 74100"), 0, 'the rows of the rolled back subtransaction are not on the subscriber');

# ---------------------------------------------------------------------------
# 3. prepared transactions
# ---------------------------------------------------------------------------
q1($pub, "BEGIN; " . rows_sql('c', 80001, 80050, 'prep') . "; PREPARE TRANSACTION 'g1'");
$pub->wait_for_catchup('sub1');
my $seen = 0;
for (1 .. 100)
{
	last if ($seen = q1($sub, "SELECT count(*) FROM pg_prepared_xacts")) >= 1;
	select(undef, undef, undef, 0.1);
}
is($seen, 1, '3: the subscriber holds the prepared transaction');
is(q1($sub, "SELECT count(*) FROM c WHERE id BETWEEN 80001 AND 80050"), 0, '3: its rows are not visible on the subscriber before COMMIT PREPARED');
q1($pub, "COMMIT PREPARED 'g1'");
same('commit prepared', 'c');
q1($pub, "BEGIN; " . rows_sql('c', 81001, 81050, 'prep') . "; PREPARE TRANSACTION 'g2'");
$pub->wait_for_catchup('sub1');
q1($pub, "ROLLBACK PREPARED 'g2'");
same('rollback prepared', 'c');
is(q1($sub, "SELECT count(*) FROM c WHERE id BETWEEN 81001 AND 81050"), 0, '3: the rolled back prepared transaction sent no row');
# a prepared transaction with a savepoint and a rolled back part
q1($pub, "BEGIN; " . rows_sql('c', 82001, 82010, 'p') . "; SAVEPOINT s; " . rows_sql('c', 82011, 82020, 'q') . "; ROLLBACK TO s; PREPARE TRANSACTION 'g3'");
q1($pub, "COMMIT PREPARED 'g3'");
same('prepared with a savepoint', 'c');

# ---------------------------------------------------------------------------
# 4. reserved row numbers, an index on the publisher
# ---------------------------------------------------------------------------
q1($pub, "SET kingres_columnar.reserve_row_numbers = on; " . rows_sql('c', 90001, 90300, 'res'));
same('reserved row numbers', 'c');
q1($pub, rows_sql('ci', 1, 3000, 'ix'));
q1($pub, "BEGIN; " . rows_sql('ci', 3001, 3100, 'ix') . "; SAVEPOINT s; " . rows_sql('ci', 3101, 3200, 'ix') . "; ROLLBACK TO s; COMMIT");
same('a table with an index', 'ci');

# ---------------------------------------------------------------------------
# 5. concurrent sessions
# ---------------------------------------------------------------------------
my $script = $pub->basedir . '/lw.pgb';
append_to_file($script, q{\set n random(1, 400)
\set w random(1, 5)
BEGIN;
INSERT INTO c (id, v) SELECT nextval('sq'), 'pgb' || g FROM generate_series(1, :n) g;
\if :w = 1
ROLLBACK;
\else
COMMIT;
\endif
});
q1($pub, "CREATE SEQUENCE sq START 1000000");
$pub->command_ok([ 'pgbench', '-n', '-c', 4, '-j', 4, '-t', 15, '-f', $script, 'postgres' ], 'four sessions insert at once');
same('four sessions', 'c');

# ---------------------------------------------------------------------------
# 5b. the copies that compaction and VACUUM FULL make are no new rows
# ---------------------------------------------------------------------------
cmp_ok(q1($pub, "SELECT count(*) FROM kingres.columnar_stripes('c')"), '>', 20, '5b: the table has many stripes');
q1($pub, "SELECT kingres.columnar_compact('c', 100000)");
same('compaction', 'c');
q1($pub, "VACUUM FULL c");
same('VACUUM FULL', 'c');

# ---------------------------------------------------------------------------
# 6. TRUNCATE
# ---------------------------------------------------------------------------
q1($pub, "TRUNCATE ci");
q1($pub, rows_sql('ci', 1, 50, 'after'));
same('truncate', 'ci');

# ---------------------------------------------------------------------------
# 7. crashes
# ---------------------------------------------------------------------------
my $open = $pub->background_psql('postgres', on_error_stop => 0);
$open->query("BEGIN; " . rows_sql('c', 95001, 95500, 'open'));
q1($pub, rows_sql('c', 96001, 96100, 'committed'));
my $mid = $pub->background_psql('postgres', on_error_stop => 0);
$mid->query("SET kingres_columnar.write_buffer_memory = '1MB'; BEGIN; INSERT INTO c (id, v) SELECT 97000 + g, repeat('z', 4000) FROM generate_series(1, 3000) g;");
$pub->wait_for_catchup('sub1');
$pub->stop('immediate');
$pub->start;
$pub->wait_for_catchup('sub1');
same('after a crash of the publisher', 'c');
is(q1($sub, "SELECT count(*) FROM c WHERE id BETWEEN 95001 AND 95500 OR id BETWEEN 97001 AND 100000"), 0, '7: the open transactions\' rows are not on the subscriber');
q1($pub, rows_sql('c', 98001, 98100, 'later'));
same('after the crash, new rows', 'c');
# a crash right after a commit that flushed many stripes
q1($pub, rows_sql('c', 110000, 125000, 'last'));
$pub->stop('immediate');
$pub->start;
same('after a second crash', 'c');

# ---------------------------------------------------------------------------
# 8. the log, and test_decoding
# ---------------------------------------------------------------------------
q1($pub, "CREATE EXTENSION pg_walinspect");
cmp_ok(q1($pub, "SELECT count(*) FROM pg_get_wal_records_info('$lsn0'::pg_lsn, pg_current_wal_lsn()) WHERE resource_manager = 'KingresColumnar' AND record_type = 'LOGICAL_INSERT'"),
	'>', 10, '8: the log holds LOGICAL_INSERT records');
q1($pub, "SELECT pg_create_logical_replication_slot('td', 'test_decoding')");
q1($pub, "INSERT INTO c (id, v) VALUES (130000, 'decoded')");
like(q1($pub, "SELECT string_agg(data, E'\\n') FROM pg_logical_slot_get_changes('td', NULL, NULL)"), qr/table \w+\.c: INSERT: id\[integer\]:130000 .*v\[text\]:'decoded'/, '8: test_decoding shows the insert of a columnar table');
q1($pub, "SELECT pg_drop_replication_slot('td')");

# temporary and unlogged columnar tables do not exist (the access method refuses them), so every
# columnar table is logically logged: nothing to leave out
my ($trc, $tout, $terr) = $pub->psql('postgres', "CREATE TEMP TABLE tt (id int) USING columnar");
like($terr, qr/cannot be temporary or unlogged/, '8: a temporary columnar table is refused');

# ---------------------------------------------------------------------------
# 9. an unpublished table
# ---------------------------------------------------------------------------
q1($pub, "INSERT INTO np VALUES (1, 'not published')");
$pub->wait_for_catchup('sub1');
is(q1($sub, "SELECT count(*) FROM np"), 0, '9: a table that is not in the publication stays empty on the subscriber');


# ---------------------------------------------------------------------------
# 10. no logical records at wal_level = replica, and a physical standby of the publisher
# ---------------------------------------------------------------------------
my $rep = new_node('lrep');
q1($rep, "CREATE TABLE c ($cols) USING columnar");
my $replsn0 = q1($rep, "SELECT pg_current_wal_lsn()");
q1($rep, "CREATE EXTENSION pg_walinspect");
q1($rep, rows_sql('c', 1, 5000, 'rep'));
is(q1($rep, "SELECT count(*) FROM pg_get_wal_records_info('$replsn0'::pg_lsn, pg_current_wal_lsn()) WHERE resource_manager = 'KingresColumnar' AND record_type = 'LOGICAL_INSERT'"),
	0, '10: a server at wal_level = replica writes no LOGICAL_INSERT record');
cmp_ok(q1($rep, "SELECT count(*) FROM pg_get_wal_records_info('$replsn0'::pg_lsn, pg_current_wal_lsn()) WHERE resource_manager = 'KingresColumnar' AND record_type = 'STRIPE_PAGES'"),
	'>', 0, '10: ... but its stripes are logged as before');
$rep->stop;
# the origin: rows the apply worker wrote on the subscriber are marked with the subscription's
# origin, so a decoder that filters local changes (the way origin = none of a subscription does) skips them
q1($sub, "INSERT INTO c (id, v) VALUES (900001, 'local1'), (900002, 'local2')");
my $loc = q{FROM pg_logical_slot_peek_changes('td2', NULL, NULL, 'only-local', '1') WHERE data LIKE '% INSERT: id[integer]:%'};
my $localq = q1($sub, "SELECT count(*) $loc AND (data LIKE '%id[integer]:900001 %' OR data LIKE '%id[integer]:900002 %')");
is($localq, 2, '10: the decoder sees the subscriber\'s own rows when it asks for local changes');
is(q1($sub, "SELECT count(*) $loc AND data NOT LIKE '%id[integer]:90000_ %' AND data LIKE 'table %.c:%'"), 0,
	'10: ... and none of the rows the apply worker wrote (they carry the replication origin)');
cmp_ok(q1($sub, "SELECT count(*) FROM pg_logical_slot_peek_changes('td2', NULL, NULL, 'only-local', '0') WHERE data LIKE 'table %.c: INSERT:%'"), '>', 1000, '10: without the filter it sees them all');
$pub->backup('pbk');
my $stb = PostgreSQL::Test::Cluster->new('lstb');
$stb->init_from_backup($pub, 'pbk', has_streaming => 1);
$stb->append_conf('postgresql.conf', "hot_standby = on\n");
$stb->start;
q1($pub, rows_sql('c', 140000, 140500, 'stb'));
$pub->wait_for_catchup($stb);
is(dumprows($stb, 'c'), dumprows($pub, 'c'), '10: a physical standby of the publisher has the same rows (it replays the logical records without a page)');
$stb->stop;
$sub->stop;
$pub->stop;
done_testing();
