# contrib/kingres_columnar/t/011_memory.pl
#
# The memory of a transaction with very many subtransactions.
#
# Every subtransaction that inserts into a columnar table starts a write
# buffer of its own; when it commits, its rows are moved into the buffer of
# its parent and its memory is given back, with or without a cursor open on
# the table.  The peak size of the backend (VmHWM of /proc) over 100 000
# subtransactions, the usual shape of a row-by-row load with an exception
# block per row, must stay small, and the rows must all be there.  (Before
# the fix such a loop needed more than a gigabyte, and four with a cursor.)
#
# KC_SUBXACT_N sets the number of subtransactions (default 100000); it exists
# to run the same test against the code before the fix without exhausting the
# machine.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

plan skip_all => 'needs /proc' unless -r '/proc/self/status';

my $N = $ENV{KC_SUBXACT_N} // 100000;
my $LIMIT_KB = 150 * 1024;

my $node = PostgreSQL::Test::Cluster->new('colmem');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
});
$node->start;
$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE TABLE cn (a int) USING columnar;
CREATE TABLE ct (a int, t text, f float8) USING columnar;
CREATE TABLE cc (a int) USING columnar;
});

my $hwm = q{(regexp_match(pg_read_file('/proc/self/status'), 'VmHWM:\s+(\d+) kB'))[1]::bigint};

sub run_case
{
	my ($name, $setup, $loop, $check) = @_;
	my $out = $node->safe_psql('postgres', qq{
SELECT $hwm AS base \\gset
BEGIN;
$setup
DO \$\$ BEGIN FOR i IN 1..$N LOOP
  BEGIN $loop; EXCEPTION WHEN OTHERS THEN NULL; END;
END LOOP; END \$\$;
SELECT $hwm - :base;
$check
COMMIT;
});
	my @l = split /\n/, $out;
	my $delta = shift @l;
	diag("$name: the backend's peak grew by " . int($delta / 1024) . " MB");
	cmp_ok($delta, '<', $LIMIT_KB, "$name: peak memory grew by under 150 MB");
	return @l;
}

my @r = run_case('one int column', '',
	'INSERT INTO cn VALUES (i)', 'SELECT count(*), sum(a) FROM cn;');
is($r[0], join('|', $N, $N * ($N + 1) / 2), 'one int column: every row is there');

@r = run_case('text and float columns', '',
	q{INSERT INTO ct VALUES (i, md5(i::text), i / 2.0)},
	'SELECT count(*), count(DISTINCT t), sum(length(t)), sum(f) FROM ct;');
is($r[0], join('|', $N, $N, 32 * $N, $N * ($N + 1) / 4),
	'text and float columns: every row and every value is there');

@r = run_case('with a cursor open on the table',
	'INSERT INTO cc VALUES (0); DECLARE cur CURSOR FOR SELECT a FROM cc;',
	'INSERT INTO cc VALUES (i)',
	'SELECT count(*), sum(a) FROM cc; FETCH ALL FROM cur;');
is($r[0], join('|', $N + 1, $N * ($N + 1) / 2), 'with a cursor: every row is there');
is($r[1], '0', 'with a cursor: the cursor sees the rows of its own snapshot only');

is($node->safe_psql('postgres', 'SELECT count(*) FROM cc'), $N + 1,
	'the rows were committed');

$node->stop;
done_testing();
