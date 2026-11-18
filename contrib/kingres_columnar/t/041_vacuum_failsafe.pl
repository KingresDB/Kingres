# contrib/kingres_columnar/t/041_vacuum_failsafe.pl
#
# C11-05.  The wraparound failsafe is decided for a columnar table's own age, the way heap does (the age
# of relfrozenxid and relminmxid against vacuum_failsafe_age, with the floor of 105 percent of
# autovacuum_freeze_max_age), and the frozen id is recorded before the truncation is tried.
#
# Before, "not while the wraparound failsafe is on" looked only at a flag that a heap table of the same
# command might have left set: for a columnar table nothing set it, so a table past the failsafe age was
# cut under an ACCESS EXCLUSIVE lock (which is WAL-logged and so needs a transaction id, exactly where the
# server may refuse to assign one), and the frozen id was recorded only after that, so a truncation that
# failed (a cancel while the lock was awaited) left the age where it was.
#
#   1. past the failsafe age: VACUUM warns as heap does ("bypassing nonessential maintenance"), leaves the
#      unreferenced tail alone, and still advances relfrozenxid; a heap table of the same command is
#      treated the same way;
#   2. below it: the same VACUUM cuts the tail, with no warning;
#   3. the order: a truncation that waits for its lock and is cancelled has already recorded the frozen id.
#      The wait is held open by a reader and ended by a cancel when the wait event shows, so no verdict
#      depends on how long anything takes.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $BLCKSZ = 8192;

my $node = PostgreSQL::Test::Cluster->new('colfailsafe');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_columnar'\nautovacuum = off\nautovacuum_naptime = 1d\nautovacuum_freeze_max_age = 100000\nvacuum_failsafe_age = 0\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub q1
{
	my ($sql) = @_;
	my $r = eval { $node->safe_psql('postgres', $sql) };
	die "t/041: q1 failed running [$sql]: $@" if $@;
	return $r;
}

sub blocks { my ($t) = @_; return q1("SELECT pg_relation_size('$t') / $BLCKSZ"); }
sub age_of { my ($t) = @_; return q1("SELECT age(relfrozenxid) FROM pg_class WHERE oid = '$t'::regclass"); }

# use up n transaction ids, each in a transaction of its own
sub burn
{
	my ($n) = @_;
	q1("DO \$\$ BEGIN FOR i IN 1..$n LOOP PERFORM pg_current_xact_id(); COMMIT; END LOOP; END \$\$");
}

sub load_sql
{
	my ($t, $lo, $hi) = @_;
	return "INSERT INTO $t SELECT g, md5(g::text) || md5((g + 1)::text) FROM generate_series($lo, $hi) g";
}

q1("CREATE TABLE c (id int, t text) USING columnar;
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE h (id int, t text);
	" . load_sql('c', 1, 50000) . ";\n" . load_sql('h', 1, 50000) . ";");
q1('VACUUM c; VACUUM h');
my $live = blocks('c');

# ------------------------------------------------------------------------
# 1. past the failsafe age
# ------------------------------------------------------------------------
q1('BEGIN; ' . load_sql('c', 100001, 180000) . '; ROLLBACK');
q1('BEGIN; ' . load_sql('h', 100001, 180000) . '; ROLLBACK');
my $tail = blocks('c');
cmp_ok(($tail - $live) * 16, '>=', $tail, 'a rolled-back load left a tail of at least a sixteenth of the file (worth cutting)');
my $htail = blocks('h');
# Past autovacuum_freeze_max_age the server starts an autovacuum worker whatever the setting of autovacuum, and it
# would vacuum the two tables before the test does.  It is signalled only when an id that is a multiple of 65536 is
# assigned while the oldest database is past that age, so the ids are used up to just past the failsafe age (105
# percent of autovacuum_freeze_max_age) and no further: no multiple of 65536 lies between the limit and there.
burn(105400 - age_of('c'));
cmp_ok(age_of('c'), '>', 105000, 'the table is past the failsafe age (105 percent of autovacuum_freeze_max_age)');
{
	# one statement for both tables: the end of a VACUUM signals the launcher, whose worker would otherwise
	# vacuum the second table first (the oldest database is past autovacuum_freeze_max_age)
	my ($out, $err) = ('', '');
	$node->psql('postgres', 'VACUUM c, h', stdout => \$out, stderr => \$err, on_error_stop => 0);
	like($err, qr/WARNING:  bypassing nonessential maintenance of table "postgres\.\w+\.c" as a failsafe/, 'VACUUM of the columnar table past the failsafe age warns, as heap does');
	is(blocks('c'), $tail, '... and leaves the unreferenced tail (no ACCESS EXCLUSIVE lock, no truncation)');
	cmp_ok(age_of('c'), '<', 1000, '... and still advances relfrozenxid');
	like($err, qr/WARNING:  bypassing nonessential maintenance of table "postgres\.\w+\.h" as a failsafe/, 'a heap table of the same age is treated the same way');
	is(blocks('h'), $htail, '... its tail is left too');
}

# ------------------------------------------------------------------------
# 2. below it: the same VACUUM cuts the tail
# ------------------------------------------------------------------------
{
	my ($out, $err) = ('', '');
	$node->psql('postgres', 'VACUUM c', stdout => \$out, stderr => \$err, on_error_stop => 0);
	unlike($err, qr/bypassing nonessential maintenance/, 'a young table: no failsafe');
	is(blocks('c'), $live, '... and the tail is given back');
}

# ------------------------------------------------------------------------
# 3. the frozen id is recorded before the truncation is tried
# ------------------------------------------------------------------------
{
	burn(30000);
	q1('BEGIN; ' . load_sql('c', 200001, 280000) . '; ROLLBACK');
	cmp_ok(blocks('c'), '>', $live, 'a tail again');
	cmp_ok(age_of('c'), '>', 20000, 'the table has aged');
	my $reader = $node->background_psql('postgres', on_error_stop => 1);
	$reader->query_safe('BEGIN; SELECT count(*) FROM c;');
	my $vac = $node->background_psql('postgres', on_error_stop => 0);
	$vac->query_safe("SET kingres_columnar.vacuum_truncate_lock_timeout = '2min'");
	my $vpid = $vac->query_safe('SELECT pg_backend_pid()');
	$vpid =~ s/\s+//g;
	$vac->{stderr} = '';
	$vac->query_until(qr/^go$/m, "\\echo go\nVACUUM c;\n");
	$node->poll_query_until('postgres', "SELECT count(*) = 1 FROM pg_stat_activity WHERE pid = $vpid AND wait_event = 'VacuumTruncate'")
	  or die 't/041: the vacuum did not wait for the lock (wait event VacuumTruncate)';
	q1("SELECT pg_cancel_backend($vpid)");
	$vac->query_until(qr/^done$/m, "\\echo done\n");
	like($vac->{stderr}, qr/canceling statement due to user request/, 'the truncation, waiting for its lock, was cancelled');
	cmp_ok(age_of('c'), '<', 1000, '... and the frozen id had been recorded before it (the age is not where it was)');
	cmp_ok(blocks('c'), '>', $live, '... and the tail is still there');
	$vac->quit;
	$reader->query_safe('ROLLBACK');
	$reader->quit;
	q1('VACUUM c');
	is(blocks('c'), $live, 'the next vacuum, with no reader, gives the tail back');
}

is(q1('SELECT kingres.columnar_check_clean()'), 't', 'the table is clean');
$node->stop;
done_testing();
