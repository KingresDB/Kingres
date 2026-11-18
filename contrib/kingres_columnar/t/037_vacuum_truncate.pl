# contrib/kingres_columnar/t/037_vacuum_truncate.pl
#
# C10-06.  VACUUM of a columnar table takes ACCESS EXCLUSIVE on it only when there is an unreferenced
# tail worth giving back, by the rule heap's vacuum uses (vacuumlazy.c, should_attempt_truncation and
# lazy_truncate_heap, the same in PostgreSQL 18 and 19): a tail of at least 1000 pages or a sixteenth of
# the file, never while the wraparound failsafe is on, the lock asked for without waiting and tried again
# every 50 ms for 5 seconds, the file measured again under the lock, and the options honoured
# (vacuum_truncate of the table, VACUUM (TRUNCATE false)).  On a logged table that lock is written to the
# WAL and replayed on a hot standby, where it cancels every query that holds the table: before this a
# vacuum with nothing to truncate, and autovacuum on every visit, ended the long queries on the standby
# that the table is for.
#
#   1. a hot standby query that holds the table across a VACUUM with nothing to truncate is not
#      cancelled (a heap twin shows the same sequence does not cancel there), and replay goes on;
#   2. a table with an aborted tail is still truncated, on the primary and the standby, and a query on
#      the standby that holds it is then cancelled, as before (t/002 has the long tail case);
#   3. the thresholds: a tail below a sixteenth of the file is left, a larger one is given back;
#   4. the options: VACUUM (TRUNCATE false) and the table option vacuum_truncate = false leave the tail,
#      VACUUM (TRUNCATE true) takes it again;
#   5. the retries: a reader of the table that goes away while the vacuum waits for its lock (the wait
#      event VacuumTruncate) lets the truncation happen; a reader that stays makes the vacuum give up
#      after its 5 seconds, without an error, and the tail stays.
#
# Waits are for events (the replay, the wait event, the end of a statement); the bounded loops are for a
# case that fails.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $BLCKSZ = 8192;

my $primary = PostgreSQL::Test::Cluster->new('colvactrunc_primary');
$primary->init(allows_streaming => 1);
$primary->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_columnar'\nautovacuum = off\n");
$primary->start;
$primary->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub q1
{
	my ($sql) = @_;
	my $r = eval { $primary->safe_psql('postgres', $sql) };
	die "t/037: q1 failed running [$sql]: $@" if $@;
	return $r;
}

sub bgq
{
	my ($s, $sql) = @_;
	my $r = eval { $s->query_safe($sql) };
	die "t/037: a background session failed running [$sql]: $@" if $@;
	return $r;
}
sub blocks { my ($t) = @_; return q1("SELECT pg_relation_size('$t') / $BLCKSZ"); }

# rows that do not compress to nothing, so that a load has a size that can be told
sub load_sql
{
	my ($t, $lo, $hi) = @_;
	return "INSERT INTO $t SELECT g, md5(g::text) || md5((g + 1)::text) FROM generate_series($lo, $hi) g";
}

$primary->safe_psql('postgres', "CREATE TABLE c (id int, t text) USING columnar;
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE h (id int, t text);
	" . load_sql('c', 1, 100000) . ";\n" . load_sql('h', 1, 100000) . ";");
q1('VACUUM c; VACUUM h; VACUUM c; VACUUM h');

$primary->backup('b');
my $standby = PostgreSQL::Test::Cluster->new('colvactrunc_standby');
$standby->init_from_backup($primary, 'b', has_streaming => 1);
$standby->append_conf('postgresql.conf', "hot_standby = on\nmax_standby_streaming_delay = '1s'\nhot_standby_feedback = off\n");
$standby->start;
$primary->wait_for_catchup($standby);

# ------------------------------------------------------------------------
# 1. a standby query holds the table across a VACUUM with nothing to truncate
# ------------------------------------------------------------------------
foreach my $t ('h', 'c')
{
	my $pos = -s $standby->logfile;
	my $sa = $standby->background_psql('postgres', on_error_stop => 0);
	bgq($sa, "BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) FROM $t;");
	my $size = blocks($t);
	q1("VACUUM $t");
	is(blocks($t), $size, "$t: the vacuum had nothing to truncate");
	$primary->wait_for_catchup($standby);
	$sa->{stderr} = '';
	my $n = eval { $sa->query("SELECT count(*) FROM $t;") };
	$n = "the session was ended: $@" unless defined $n;
	$n =~ s/\s+//g;
	is($n, '100000', "$t: the standby query that held the table across the vacuum goes on and sees its rows");
	is($sa->{stderr}, '', "$t: ... it was not cancelled");
	unlike(slurp_file($standby->logfile, $pos), qr/conflict with recovery|terminating connection/, "$t: ... and the standby logged no conflict");
	eval { bgq($sa, 'ROLLBACK') };
	eval { $sa->quit };
}

# ------------------------------------------------------------------------
# 2. an aborted tail is still given back, on both nodes
# ------------------------------------------------------------------------
my $live = blocks('c');
q1('BEGIN; ' . load_sql('c', 200001, 300000) . '; ROLLBACK');
my $with_tail = blocks('c');
cmp_ok($with_tail, '>', $live * 1.2, 'a rolled-back load left a long unreferenced tail');
{
	my $pos = -s $standby->logfile;
	my $sa = $standby->background_psql('postgres', on_error_stop => 0);
	bgq($sa, 'BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) FROM c;');
	q1('VACUUM c');
	is(blocks('c'), $live, 'VACUUM gave the tail back');
	$primary->wait_for_catchup($standby);
	is($standby->safe_psql('postgres', "SELECT pg_relation_size('c') / $BLCKSZ"), $live, 'the standby truncated its file to the same length');
	like(slurp_file($standby->logfile, $pos), qr/conflict with recovery/, 'a query that held the table across a truncation is cancelled, as before');
	eval { $sa->quit };
	is($standby->safe_psql('postgres', 'SELECT count(*) FROM c'), 100000, 'the standby reads the table');
}
is(q1('SELECT kingres.columnar_check_clean()'), 't', 'the table is clean after the truncation');

# ------------------------------------------------------------------------
# 3. the thresholds (a sixteenth of the file, or 1000 pages)
# ------------------------------------------------------------------------
{
	# a tail of one stripe (a thousand rows): far below a sixteenth of the file
	q1('BEGIN; ' . load_sql('c', 300001, 301000) . '; SAVEPOINT s; ' . load_sql('c', 301001, 302000) . '; ROLLBACK');
	my $small = blocks('c');
	cmp_ok($small, '>', $live, 'a short unreferenced tail');
	cmp_ok(($small - $live) * 16, '<', $small, '... below a sixteenth of the file');
	q1('VACUUM c');
	is(blocks('c'), $small, 'a tail below a sixteenth of the file is left (heap leaves it too: the lock is not worth its cost)');
	# the same tail with a long one after it: now it is worth it
	q1('BEGIN; ' . load_sql('c', 400001, 480000) . '; ROLLBACK');
	cmp_ok((blocks('c') - $live) * 16, '>=', blocks('c'), 'a long tail: at least a sixteenth of the file');
	q1('VACUUM c');
	is(blocks('c'), $live, '... is given back, the short one with it');
}

# ------------------------------------------------------------------------
# 4. the options
# ------------------------------------------------------------------------
{
	q1('BEGIN; ' . load_sql('c', 500001, 580000) . '; ROLLBACK');
	my $tail = blocks('c');
	q1('VACUUM (TRUNCATE false) c');
	is(blocks('c'), $tail, 'VACUUM (TRUNCATE false) leaves the tail');
	q1('ALTER TABLE c SET (vacuum_truncate = false)');
	q1('VACUUM c');
	is(blocks('c'), $tail, 'the table option vacuum_truncate = false leaves the tail');
	q1('VACUUM (TRUNCATE true) c');
	is(blocks('c'), $live, 'VACUUM (TRUNCATE true) takes it, whatever the table option says');
	q1('ALTER TABLE c RESET (vacuum_truncate)');
	q1('BEGIN; ' . load_sql('c', 600001, 680000) . '; ROLLBACK');
	q1('VACUUM c');
	is(blocks('c'), $live, 'with the option reset the tail goes again');
}

# ------------------------------------------------------------------------
# 5. the retries
# ------------------------------------------------------------------------
{
	q1('BEGIN; ' . load_sql('c', 700001, 780000) . '; ROLLBACK');
	my $tail = blocks('c');
	cmp_ok($tail, '>', $live, 'a long tail again');
	my $reader = $primary->background_psql('postgres', on_error_stop => 1);
	bgq($reader, 'BEGIN; SELECT count(*) FROM c;');
	my $vac = $primary->background_psql('postgres', on_error_stop => 1);
	$vac->query_until(qr/^go$/m, "\\echo go\nVACUUM c;\n");
	$primary->poll_query_until('postgres', "SELECT count(*) = 1 FROM pg_stat_activity WHERE wait_event = 'VacuumTruncate'")
	  or die 't/037: the vacuum did not wait for the lock (wait event VacuumTruncate)';
	pass('the vacuum waits for the lock (wait event VacuumTruncate) behind a reader, and does not give up at once');
	bgq($reader, 'ROLLBACK');
	$reader->quit;
	$vac->query_until(qr/^done$/m, "\\echo done\n");
	is(blocks('c'), $live, 'the reader went away while the vacuum waited: the tail was given back');
	$vac->quit;

	# a reader that stays: the vacuum gives up after about five seconds, without an error
	q1('BEGIN; ' . load_sql('c', 800001, 880000) . '; ROLLBACK');
	$tail = blocks('c');
	$reader = $primary->background_psql('postgres', on_error_stop => 1);
	bgq($reader, 'BEGIN; SELECT count(*) FROM c;');
	my ($out, $err) = ('', '');
	$primary->psql('postgres', 'VACUUM c', stdout => \$out, stderr => \$err, on_error_stop => 0, timeout => 120);
	is($err, '', 'a vacuum that cannot get the lock gives up without an error');
	is(blocks('c'), $tail, '... and the tail stays for the next vacuum');
	bgq($reader, 'ROLLBACK');
	$reader->quit;
	q1('VACUUM c');
	is(blocks('c'), $live, 'the next vacuum, with no reader, gives it back');
}

is($standby->safe_psql('postgres', 'SELECT count(*) FROM kingres.columnar_check()'), 0, 'the standby: columnar_check has nothing to report');
$primary->wait_for_catchup($standby);
is($standby->safe_psql('postgres', "SELECT pg_relation_size('c') / $BLCKSZ"), $live, 'the standby has the file the primary has');
is($standby->safe_psql('postgres', 'SELECT count(*) FROM c'), 100000, '... and the rows');
unlike(slurp_file($primary->logfile), qr/PANIC|TRAP:/, 'the primary logged no PANIC');
unlike(slurp_file($standby->logfile), qr/PANIC|TRAP:|inconsistent page/, 'the standby logged no PANIC');

$standby->stop;
$primary->stop;
done_testing();
