# contrib/kingres_columnar/t/070_vacuum_order_and_skip.pl
#
# C14-08: an index vacuum takes its lock BEFORE it reads the ranges (t/068 stops it after both,
# so it cannot tell the orders apart).  The vacuum is stopped by the developer gate 8, between the
# lock and the read of the ranges; a writer that gives back the tail of an aborted reservation
# then finds the lock held and leaves the numbers a gap.  With the lock taken after the read the
# vacuum would not hold it at the gate and the tail would go back.
# C14-10: a merge that is skipped because the VACUUM already has a transaction id (here the
# reconcile at its start deleted the catalog rows of a dropped table's file) says so at DEBUG1.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1 wait_waiting);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = new_node('vos');
q1($node, q{
	CREATE TABLE u (id int, k int) USING columnar;
	CREATE UNIQUE INDEX u_k ON u (k);
	INSERT INTO u SELECT g, g FROM generate_series(1, 10) g;
});

# ---------------------------------------------------------------------------
# C14-08
# ---------------------------------------------------------------------------
my $KEY = 783;
my $ctl = $node->background_psql('postgres');
$ctl->query_safe("SELECT pg_advisory_lock($KEY, 0);");

my $w = $node->background_psql('postgres', on_error_stop => 0);
$w->query("BEGIN; INSERT INTO u VALUES (100, 100);");
$w->query("ROLLBACK;");

my $vac = $node->background_psql('postgres', on_error_stop => 0);
$vac->query_until(qr/^go$/m, "SET kingres_columnar.reserve_gate = $KEY; SET kingres_columnar.reserve_gate_at = 8;\n\\echo go\nVACUUM u;\n");
ok(wait_waiting($node, 1), 'the VACUUM waits in the gate 8, holding its lock, before it reads the ranges');

my $n0 = q1($node, "SELECT meta_next_row('u')");
$w->query("INSERT INTO u VALUES (200, 200);");
my $used = q1($node, "SELECT meta_next_row('u')") - $n0;
cmp_ok($used, '>=', 32, "the writer's next insert took $used row numbers: the tail was not given back while the vacuum holds its lock");

$ctl->query_safe("SELECT pg_advisory_unlock($KEY, 0);");
$vac->query("SELECT 'sync';");

my $IDX = "SET enable_seqscan = off; SET enable_bitmapscan = off; SET kingres_columnar.enable_custom_scan = off;";
is(q1($node, "$IDX SELECT count(*) FROM u"), 11, 'the index finds all 11 rows');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('u')"), 0, 'columnar_check_indexes finds nothing');
$_->quit for ($ctl, $w, $vac);

# ---------------------------------------------------------------------------
# C14-10
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE m (id int, k int, v text) USING columnar; CREATE UNIQUE INDEX m_id ON m (id); CREATE TABLE dropme (a int) USING columnar; INSERT INTO dropme SELECT generate_series(1, 100);");
q1($node, "DO \$\$ BEGIN
	INSERT INTO m VALUES (0, 0, 'outer');
	FOR i IN 1..12 LOOP
	  BEGIN INSERT INTO m VALUES (i, i, 'row' || i); EXCEPTION WHEN OTHERS THEN NULL; END;
	END LOOP; END \$\$");
my $stripes = q1($node, "SELECT count(*) FROM kingres.columnar_stripes('m')");
cmp_ok($stripes, '>=', 8, "the table has $stripes small stripes");

# a table dropped by a server without the library leaves catalog rows of a file that is gone
$node->stop;
$node->append_conf('postgresql.conf', "shared_preload_libraries = ''\n");
$node->start;
$node->safe_psql('postgres', 'DROP TABLE dropme');
$node->stop;
$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_columnar'\n");
$node->start;

my ($ret, $out, $err) = $node->psql('postgres',
	"SET client_min_messages = debug1; SET kingres_columnar.vacuum_compact_stripes = 5; VACUUM m;");
is($ret, 0, 'the VACUUM that deletes the orphan rows works');
like($err, qr/the merge of the small stripes of "m" was skipped: this VACUUM already has a transaction id/,
	'the skipped merge is reported at DEBUG1');
is(q1($node, "SELECT count(*) FROM kingres.columnar_stripes('m')"), $stripes, '... and no stripe was merged');

# the next VACUUM has no orphan rows to delete: it merges and says nothing of a skip
($ret, $out, $err) = $node->psql('postgres',
	"SET client_min_messages = debug1; SET kingres_columnar.vacuum_compact_stripes = 5; VACUUM m;");
unlike($err, qr/merge of the small stripes of "m" was skipped/, 'the next VACUUM does not skip the merge');
cmp_ok(q1($node, "SELECT count(*) FROM kingres.columnar_stripes('m')"), '<', $stripes, '... it merged the stripes');

$node->stop;
done_testing();
