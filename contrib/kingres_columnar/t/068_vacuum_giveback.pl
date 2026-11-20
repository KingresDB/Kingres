# contrib/kingres_columnar/t/068_vacuum_giveback.pl
#
# C14-01: the give-back of an aborted writer's reservation tail must not race an index vacuum.
# The vacuum reads the metapage and the catalog, then deletes the index entries whose row numbers
# no live claim covers.  The claim row of an aborted writer is dead and does not lower the bound,
# so when the same session aborted, and its next insert took the tail back, and the vacuum had
# its ranges from before, the vacuum deleted the entries of the committed rows (a unique index
# then accepted a duplicate).  Now an index vacuum holds a lock from before it looks until its
# scan is done and a give-back that finds it held leaves the numbers a gap.
# The vacuum is stopped between its ranges and its scan by the developer gate (reserve_gate_at 6).
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

my $node = new_node('vgb');
q1($node, q{
	CREATE TABLE u (id int, k int) USING columnar;
	CREATE UNIQUE INDEX u_k ON u (k);
	INSERT INTO u SELECT g, g FROM generate_series(1, 10) g;
});
my $IDX = "SET enable_seqscan = off; SET enable_bitmapscan = off; SET kingres_columnar.enable_custom_scan = off;";

my $KEY = 782;
my $ctl = $node->background_psql('postgres');
$ctl->query_safe("SELECT pg_advisory_lock($KEY, 0);");

# the session that aborts and then writes
my $w = $node->background_psql('postgres', on_error_stop => 0);
$w->query("BEGIN; INSERT INTO u VALUES (100, 100);");
$w->query("ROLLBACK;");

# the vacuum stops after it has its ranges
my $vac = $node->background_psql('postgres', on_error_stop => 0);
$vac->query_until(qr/^go$/m, "SET kingres_columnar.reserve_gate = $KEY; SET kingres_columnar.reserve_gate_at = 6;\n\\echo go\nVACUUM u;\n");
ok(wait_waiting($node, 1), 'the VACUUM waits in the gate with its ranges');

# the writer's next insert would take the tail of the aborted reservation back
$w->query("INSERT INTO u SELECT 200 + g, 200 + g FROM generate_series(1, 20) g;");
$ctl->query_safe("SELECT pg_advisory_unlock($KEY, 0);");
$vac->query("SELECT 'sync';");

is(q1($node, "$IDX SELECT count(*) FROM u WHERE k BETWEEN 201 AND 220"), 20, 'the index finds the 20 rows committed while the vacuum had old ranges');
is(q1($node, "$IDX SELECT count(*) FROM u"), 30, 'the index finds all 30 rows');
my ($ret, $out, $err) = $node->psql('postgres', "INSERT INTO u VALUES (999, 205)");
isnt($ret, 0, 'the unique index refuses a duplicate of a committed key');
like($err, qr/duplicate key/, '... with the duplicate key error');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('u')"), 0, 'columnar_check_indexes finds nothing');

# without a vacuum the tail is still given back
my $n0 = q1($node, "SELECT meta_next_row('u')");
$w->query("BEGIN; INSERT INTO u VALUES (300, 300);");
$w->query("ROLLBACK;");
$w->query("INSERT INTO u VALUES (301, 301);");
my $used = q1($node, "SELECT meta_next_row('u')") - $n0;
cmp_ok($used, '<', 10, "without a running vacuum an abort and an insert use $used row numbers (the tail goes back)");

$_->quit for ($ctl, $w, $vac);
$node->stop;
done_testing();
