# contrib/kingres_columnar/t/067_vacuum_compacts.pl
#
# C13-05: the cost of a stripe per row is bounded.  On a table with an index a subtransaction
# that inserted rows and commits while its parent holds rows of the table writes a stripe of its
# own (decision 3 of C6a), so a PL/pgSQL loop with an exception block per row makes a stripe and
# a page per row.  VACUUM now merges the small stripes when at least
# kingres_columnar.vacuum_compact_stripes (128) are smaller than a sixteenth of the stripe size.
#  1. 400 one-row subtransactions make 400 stripes; one VACUUM leaves a few; the rows are those of
#     a heap twin, the indexes answer as before, the replaced stripes' blocks are listed by the next VACUUM.
#  2. Fewer small stripes than the setting are left alone; 0 turns it off.
#  3. An older snapshot that lives through the VACUUM reads its rows (the stripes it sees stay).
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

my $node = new_node('vcs');

sub stripes { my ($t) = @_; return q1($node, "SELECT count(*) FROM kingres.columnar_stripes('$t')"); }

sub savepoint_loop
{
	my ($t, $n) = @_;
	q1($node, "CREATE TABLE $t (id int, k int, v text) USING columnar; CREATE UNIQUE INDEX ${t}_id ON $t (id); CREATE INDEX ${t}_k ON $t (k); CREATE TABLE ${t}h (id int, k int, v text)");
	q1($node, "DO \$\$ BEGIN
		INSERT INTO $t VALUES (0, 0, 'outer'); INSERT INTO ${t}h VALUES (0, 0, 'outer');
		FOR i IN 1..$n LOOP
		  BEGIN INSERT INTO $t VALUES (i, i % 17, 'row' || i); INSERT INTO ${t}h VALUES (i, i % 17, 'row' || i);
		  EXCEPTION WHEN OTHERS THEN NULL; END;
		END LOOP; END \$\$");
}

# 1. a stripe per row, then one VACUUM
savepoint_loop('sp', 400);
my $before = stripes('sp');
cmp_ok($before, '>=', 399, "1: 400 one-row subtransactions made $before stripes");
my $size_before = q1($node, "SELECT pg_relation_size('sp')");
cmp_ok($size_before, '>', 400 * 8192, "1: ... and a page each ($size_before bytes)");
q1($node, "VACUUM sp");
my $after = stripes('sp');
cmp_ok($after, '<=', 5, "1: one VACUUM merged them: $after stripes visible");
is(q1($node, "SELECT count(*) FROM (SELECT id, k, v FROM sp EXCEPT SELECT id, k, v FROM sph) s") . '|' . q1($node, "SELECT count(*) FROM (SELECT id, k, v FROM sph EXCEPT SELECT id, k, v FROM sp) s"),
	'0|0', '1: the rows are those of the heap twin');
is(q1($node, "SET enable_seqscan = off; SET enable_bitmapscan = off; SELECT count(*) || ':' || sum(id) FROM sp WHERE k = 5"),
   q1($node, "SELECT count(*) || ':' || sum(id) FROM sph WHERE k = 5"), '1: the index answers as the twin does');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('sp')"), 0, '1: columnar_check_indexes finds nothing');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '1: columnar_check finds nothing');
q1($node, "VACUUM sp");
my $cnt = q1($node, "SELECT get_byte(p, 132) FROM (SELECT get_raw_page('sp', 0) p) s");
cmp_ok($cnt, '>=', 1, "1: the next VACUUM lists the replaced stripes' blocks as free ($cnt extent)");
q1($node, "INSERT INTO sp SELECT 10000 + g, 1, 'later' FROM generate_series(1, 300) g");
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, '1: columnar_check finds nothing after a load into the freed blocks');

# 2. fewer than the setting, and the setting at 0
savepoint_loop('few', 60);
q1($node, "VACUUM few");
cmp_ok(stripes('few'), '>=', 59, '2: 60 small stripes are fewer than 128: a VACUUM leaves them');
savepoint_loop('offt', 200);
q1($node, "SET kingres_columnar.vacuum_compact_stripes = 0; VACUUM offt");
cmp_ok(stripes('offt'), '>=', 199, '2: with the setting at 0 a VACUUM leaves 200 small stripes');
q1($node, "VACUUM offt");
cmp_ok(stripes('offt'), '<=', 5, '2: with the default (128) the same table is merged');

# 3. an older snapshot
savepoint_loop('old', 300);
my $s = $node->background_psql('postgres', on_error_stop => 0);
$s->query("BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1;");
q1($node, "VACUUM old");
my $n = $s->query("SELECT count(*) || ':' || sum(id) FROM old;");
chomp $n;
is($n, q1($node, "SELECT count(*) || ':' || sum(id) FROM oldh"), '3: a snapshot older than the VACUUM reads its rows');
$s->query("SET enable_seqscan = off; SET enable_bitmapscan = off; SET kingres_columnar.enable_custom_scan = off;");
my $ni = $s->query("SELECT count(*) || ':' || sum(id) FROM old WHERE k < 17;");
chomp $ni;
is($ni, q1($node, "SELECT count(*) || ':' || sum(id) FROM oldh WHERE k < 17"), '3: and through an index');
$s->query("COMMIT;");
$s->quit;

$node->stop;
done_testing();
