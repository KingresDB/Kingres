# contrib/kingres_columnar/t/058_index_cursor.pl
#
# C6c: what a cursor over an index sees of the rows its own transaction writes later.
# A stripe that the transaction writes in two commands (the buffer holds the rows of both
# when it fills) has rows that an earlier command's scan must not see: the stripe's
# command-id points say how many.  A fetch by row number must apply them (the guard in
# kc_scan_fetch_row, "off >= visible_rows"): a cursor declared after the first command
# and read after the second shows the rows of the first only, as with a heap table, and
# so does a second cursor declared after the second command (all rows).
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

my $node = new_node('icur');
q1($node, q{
	CREATE TABLE c (k int, v text) USING columnar;
	CREATE INDEX c_k ON c (k);
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE TABLE h (k int, v text);
	CREATE INDEX h_k ON h (k);
});

my %res;
for my $t ('c', 'h')
{
	my $out = $node->safe_psql('postgres', qq{
SET enable_seqscan = off; SET enable_bitmapscan = off;
BEGIN;
INSERT INTO $t SELECT g, 'first' FROM generate_series(1, 1500) g;
DECLARE cur1 CURSOR FOR SELECT k FROM $t WHERE k BETWEEN 1 AND 5000 ORDER BY k;
FETCH 3 FROM cur1;
INSERT INTO $t SELECT 1500 + g, 'second' FROM generate_series(1, 1000) g;
DECLARE cur2 CURSOR FOR SELECT k FROM $t WHERE k BETWEEN 1 AND 5000 ORDER BY k;
SELECT 'cur1', count(*) FROM (SELECT 1 FROM generate_series(1,1)) x;
FETCH ALL FROM cur1;
SELECT 'cur2', count(*) FROM (SELECT 1 FROM generate_series(1,1)) x;
FETCH 5000 FROM cur2;
COMMIT;});
	$res{$t} = join(',', map { (split /\n/, $out)[0] } ()) ;
	# the counts of the two cursors, as "n1:n2"
	my @l = split /\n/, $out;
	my ($i1) = grep { $l[$_] eq 'cur1|1' } 0 .. $#l;
	my ($i2) = grep { $l[$_] eq 'cur2|1' } 0 .. $#l;
	$res{$t} = ($i2 - $i1 - 1 + 3) . ':' . (scalar(@l) - $i2 - 1);
}
is($res{h}, '1500:2500', 'the heap table: the first cursor sees 1500 rows, the second 2500');
is($res{c}, $res{h}, 'the columnar table gives the same');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('c')"), 0, 'the index is complete');
$node->stop;
done_testing();
