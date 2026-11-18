# contrib/kingres_columnar/t/038_truncate_frozen_id.pl
#
# C10-04.  relfrozenxid of a columnar table must never claim more than the file holds, and the line that
# autovacuum logs for a visit must say what pg_class holds.
#
# The metapage of a columnar file holds up to two normal transaction ids: that of the transaction that
# created the file, and the table's own (table_xid), which TRUNCATE and a rewrite keep from the old file.
# The id handed to PostgreSQL for the new file was the id of the truncating transaction, newer than the
# table's own that the new metapage still holds, so until the next vacuum relfrozenxid claimed that every id
# before it was frozen while the file held an older one (datfrozenxid and the server's warnings ran late by
# the distance between the creation and the truncate).  VACUUM FULL (the rewrite) returned OldestXmin the
# same way.  And vac_update_relstats only moves relfrozenxid forward, so the id the vacuum passed in, which the
# log line printed, was not always what pg_class held afterwards.
#
#   1. TRUNCATE after many ids were consumed: relfrozenxid is not newer than either id of the metapage;
#   2. VACUUM FULL of an old table: the same;
#   3. the review's second run: a prepared transaction holds the horizon back, a table is made after it and
#      truncated later, autovacuum visits it (anti-wraparound): every line it logs for the table says the
#      relfrozenxid that pg_class holds, which is the table's own id, not the truncating transaction's;
#   4. a table whose relfrozenxid is already ahead of the ids its file holds (what an earlier build left behind
#      after a TRUNCATE): the line prints what is stored, not the id the vacuum passed in.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('coltruncxid');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
max_prepared_transactions = 5
autovacuum = on
autovacuum_naptime = 1s
autovacuum_freeze_max_age = 100000
autovacuum_vacuum_insert_threshold = 100000000
log_autovacuum_min_duration = 0
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar; CREATE TABLE tmpx (a int); CREATE TABLE other (a int)');

sub q1
{
	my ($sql) = @_;
	my $r = eval { $node->safe_psql('postgres', $sql) };
	die "t/038: q1 failed running [$sql]: $@" if $@;
	return $r;
}

# use up n transaction ids (each block of the loop is a subtransaction that writes, so it has an id)
sub consume
{
	my ($n) = @_;
	q1("CREATE TEMP TABLE IF NOT EXISTS u (a int);
		DO \$\$ BEGIN FOR i IN 1..$n LOOP BEGIN INSERT INTO u VALUES (i); EXCEPTION WHEN OTHERS THEN NULL; END; END LOOP; END \$\$;");
}

sub xid { return q1($_[0]) + 0; }

# ------------------------------------------------------------------------
# 1. TRUNCATE (autovacuum is kept away from the tables of this part)
# ------------------------------------------------------------------------
q1("CREATE TABLE tr (a int) USING columnar WITH (autovacuum_enabled = false); INSERT INTO tr VALUES (1)");
my $tx = xid("SELECT kingres.columnar_table_xid('tr')::text::bigint");
cmp_ok($tx, '>', 3, "tr: the table was created by transaction $tx");
consume(2000);
q1('TRUNCATE tr');
my $rf = xid("SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'tr'");
my $cx = xid("SELECT kingres.columnar_creating_xid('tr')::text::bigint");
my $tx2 = xid("SELECT kingres.columnar_table_xid('tr')::text::bigint");
cmp_ok($cx, '>', $tx + 1500, "tr: the new file was created by a transaction well after the table ($cx)");
is($tx2, $tx, "tr: the table's own id in the new metapage is the old one ($tx)");
cmp_ok($rf, '<=', $tx2, "tr: relfrozenxid ($rf) is not newer than the table's own id in the metapage ($tx2): it claims no more than the file holds");
cmp_ok($rf, '<=', $cx, "tr: ... nor than the id of the creating transaction ($cx)");
is(q1("SELECT age(relfrozenxid) >= $cx - $tx FROM pg_class WHERE relname = 'tr'"), 't', 'tr: its age shows the older id (datfrozenxid does not run late)');

# a table with no old id (made in the same transaction as its truncate, or a first file): the new id as before
q1("BEGIN; CREATE TABLE tn (a int) USING columnar WITH (autovacuum_enabled = false); TRUNCATE tn; COMMIT");
cmp_ok(xid("SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'tn'"), '<=', xid("SELECT kingres.columnar_creating_xid('tn')::text::bigint"),
	'tn: a table made and truncated in one transaction: relfrozenxid is that transaction');

# ------------------------------------------------------------------------
# 2. VACUUM FULL: the rewrite keeps the table's own id, so relfrozenxid does not pass it
# ------------------------------------------------------------------------
q1("CREATE TABLE vf (a int) USING columnar WITH (autovacuum_enabled = false); INSERT INTO vf SELECT generate_series(1, 100)");
my $vx = xid("SELECT kingres.columnar_table_xid('vf')::text::bigint");
consume(2000);
q1('VACUUM FULL vf');
my $vrf = xid("SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'vf'");
my $vtx = xid("SELECT kingres.columnar_table_xid('vf')::text::bigint");
is($vtx, $vx, "vf: the rewrite kept the table's own id ($vx)");
cmp_ok($vrf, '<=', $vtx, "vf: relfrozenxid ($vrf) after VACUUM FULL is not newer than that id ($vtx)");
is(q1('SELECT count(*) FROM vf'), 100, 'vf: the table has its rows');

# ------------------------------------------------------------------------
# 3. the autovacuum line says what pg_class holds (a prepared transaction holds the horizon back)
# ------------------------------------------------------------------------
$node->safe_psql('postgres', "BEGIN; INSERT INTO other VALUES (-1); PREPARE TRANSACTION 'hold'");
my $px = xid("SELECT transaction::text::bigint FROM pg_prepared_xacts WHERE gid = 'hold'");
q1("CREATE TABLE ct (a int) USING columnar; INSERT INTO ct VALUES (1)");
my $ctx = xid("SELECT kingres.columnar_table_xid('ct')::text::bigint");
cmp_ok($ctx, '>', $px, "ct: made after the prepared transaction ($ctx after $px), which holds the horizon back");
consume(1000);
q1('TRUNCATE ct');
my $ct_rf = xid("SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'ct'");
cmp_ok($ct_rf, '<=', $ctx, "ct: after the truncate relfrozenxid ($ct_rf) is not newer than the table's own id ($ctx)");
# more ids than autovacuum_freeze_max_age: the anti-wraparound autovacuum visits every table
consume(60000);
consume(60000);
my $deadline = time() + 300;
my $seen = 0;
while (time() < $deadline)
{
	$seen = q1(q{SELECT autovacuum_count FROM pg_stat_all_tables WHERE relname = 'ct'});
	last if $seen > 0;
	select(undef, undef, undef, 0.5);
}
cmp_ok($seen, '>', 0, 'ct: autovacuum visited the table (anti-wraparound)');
$node->poll_query_until('postgres', q{SELECT autovacuum_count > 1 FROM pg_stat_all_tables WHERE relname = 'ct'})
  or die "t/038: autovacuum did not visit ct a second time";
my $stored = xid("SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'ct'");
my @lines = slurp_file($node->logfile) =~ /automatic aggressive vacuum to prevent wraparound of table "postgres\.\w+\.ct": columnar: \d+ rows, \d+ blocks, relfrozenxid (\d+), creation id (\w+)/g;
cmp_ok(scalar(@lines) / 2, '>=', 1, 'ct: the log has lines for the visits');
my %v;
for (my $i = 0; $i < @lines; $i += 2) { $v{ $lines[$i] } = 1; }
is(join(',', sort keys %v), $stored, "ct: every line says the relfrozenxid that pg_class holds ($stored)");
is($stored, $ctx, "ct: ... which is the table's own id: the prepared transaction holds the horizon back, nothing could be frozen");
$node->safe_psql('postgres', "COMMIT PREPARED 'hold'");

# ------------------------------------------------------------------------
# 4. a table whose relfrozenxid is ahead of what its file holds (left by an earlier build's TRUNCATE)
# ------------------------------------------------------------------------
$node->safe_psql('postgres', "BEGIN; INSERT INTO other VALUES (-2); PREPARE TRANSACTION 'hold2'");
q1("CREATE TABLE lg (a int) USING columnar WITH (autovacuum_vacuum_insert_threshold = 10); INSERT INTO lg VALUES (1)");
my $lx = xid("SELECT kingres.columnar_table_xid('lg')::text::bigint");
consume(300);
q1("UPDATE pg_class SET relfrozenxid = ((pg_current_xact_id()::text::bigint) % 4294967296)::text::xid WHERE relname = 'lg'");
my $planted = xid("SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'lg'");
cmp_ok($planted, '>', $lx, "lg: relfrozenxid ($planted) is ahead of the table's own id ($lx), as an earlier build's TRUNCATE left it");
my $off = -s $node->logfile;
q1("INSERT INTO lg SELECT generate_series(1, 100)");
$node->poll_query_until('postgres', q{SELECT autovacuum_count > 0 FROM pg_stat_all_tables WHERE relname = 'lg'})
  or die "t/038: autovacuum did not visit lg";
my @l2 = slurp_file($node->logfile, $off) =~ /automatic vacuum of table "postgres\.\w+\.lg": columnar: \d+ rows, \d+ blocks, relfrozenxid (\d+), creation id (\w+)/g;
is($l2[0], $planted, 'lg: the line prints what pg_class holds, not the id the vacuum passed in');
is(xid("SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'lg'"), $planted, 'lg: ... and pg_class still holds it (relfrozenxid only moves forward)');
$node->safe_psql('postgres', "COMMIT PREPARED 'hold2'");

$node->stop;
done_testing();
