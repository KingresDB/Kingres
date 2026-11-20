# contrib/kingres_columnar/t/064_decode_missing.pl
#
# C13-03: rows inserted before an ALTER TABLE ... ADD COLUMN ... DEFAULT in the same transaction
# are decoded with the column's default, as a scan reads them (and as the decoder completes a
# heap tuple), not with NULL.  A publisher and a subscriber would otherwise differ.
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

my $node = new_node('dmiss', init => { allows_streaming => 'logical' }, conf => "wal_level = logical\n");
q1($node, q{
	CREATE TABLE d (a int, b text) USING columnar;
	CREATE TABLE dh (a int, b text);
	SELECT pg_create_logical_replication_slot('td', 'test_decoding');
});

sub decoded
{
	return q1($node, "SELECT coalesce(string_agg(data, E'\\n' ORDER BY lsn), '') FROM pg_logical_slot_get_changes('td', NULL, NULL) WHERE data LIKE 'table %.d%: INSERT:%'");
}

# the rows are inserted, then the column is added with a default, in one transaction
q1($node, "BEGIN; INSERT INTO d VALUES (1, 'x'), (2, 'y'); INSERT INTO dh VALUES (1, 'x'), (2, 'y'); ALTER TABLE d ADD COLUMN z int DEFAULT 5; ALTER TABLE dh ADD COLUMN z int DEFAULT 5; COMMIT");
is(q1($node, "SELECT string_agg(a || ':' || z, ',' ORDER BY a) FROM d"), '1:5,2:5', 'a scan gives the default');
my $dec = decoded();
like($dec, qr/table \w+\.d: INSERT: a\[integer\]:1 b\[text\]:'x' z\[integer\]:5\b/, 'the decoded row has the default, not NULL');
like($dec, qr/table \w+\.d: INSERT: a\[integer\]:2 b\[text\]:'y' z\[integer\]:5\b/, '... for every row');
unlike($dec, qr/table \w+\.d: INSERT: [^\n]*z\[integer\]:null/, '... and no NULL');
like($dec, qr/table \w+\.dh: INSERT: a\[integer\]:1 b\[text\]:'x'$/m, 'a heap row is decoded as it was inserted, with no column z (a subscriber gives it its own default); a columnar row carries what a scan returns');

# a default of another type, a column added and dropped, and a row inserted after the ALTER
q1($node, "BEGIN; INSERT INTO d VALUES (3, 'p'); ALTER TABLE d ADD COLUMN w text DEFAULT 'dflt'; ALTER TABLE d ADD COLUMN gone int DEFAULT 9; ALTER TABLE d DROP COLUMN gone; INSERT INTO d VALUES (4, 'q', 6, 'later'); COMMIT");
$dec = decoded();
like($dec, qr/table \w+\.d: INSERT: a\[integer\]:3 b\[text\]:'p' z\[integer\]:5 w\[text\]:'dflt'/, 'a text default, and a dropped column that has no value');
like($dec, qr/table \w+\.d: INSERT: a\[integer\]:4 b\[text\]:'q' z\[integer\]:6 w\[text\]:'later'/, 'a row inserted after the ALTER is as written');
is(q1($node, "SELECT string_agg(a || ':' || z || ':' || w, ',' ORDER BY a) FROM d WHERE a >= 3"), '3:5:dflt,4:6:later', 'a scan agrees');

$node->stop;
done_testing();
