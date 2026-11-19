# contrib/kingres_columnar/t/052_reserve_memory.pl
#
# C6b: what reservation costs a backend in memory.  1500 partitions written in one
# transaction under a 1 MB allowance (the scenario of t/025), with and without
# reserved row numbers.  A reservation adds no rows slots or values to a buffer; what it
# adds is the update of its own catalog row, which the transaction remembers as a
# combo command id (about 0.7 kB of private memory per reserved stripe written in
# the transaction).  Bounded here: the memory of the contexts and the anonymous
# memory of the process grow by at most 3 MB more than without it, for 1500 relations.
# (The shared memory a backend touches, which the process size also counts, is the
# catalog's and the files' pages and is not a cost of the backend.)
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1);
use PostgreSQL::Test::Utils;
use Test::More;

my $node = new_node('rmem');
q1($node, q{
CREATE TABLE pt (a int, b text) PARTITION BY HASH (a);
DO $$ BEGIN FOR i IN 0..1499 LOOP
  EXECUTE format('CREATE TABLE pt_%s PARTITION OF pt FOR VALUES WITH (modulus 1500, remainder %s) USING columnar', i, i);
END LOOP; END $$;});

my %m;
for my $on ('off', 'on')
{
	my ($rc, $out, $err) = $node->psql('postgres', "SET kingres_columnar.reserve_row_numbers = $on; SET kingres_columnar.write_buffer_memory = '1MB'; BEGIN;
INSERT INTO pt SELECT g, 'row' || g FROM generate_series(1, 30000) g;
SELECT (SELECT sum(total_bytes) FROM pg_backend_memory_contexts) || '|' ||
       (SELECT substring(pg_read_file('/proc/self/status') from 'RssAnon:[ \t]*([0-9]+)'));
COMMIT;", on_error_stop => 1);
	is($rc, 0, "30 000 rows into 1 500 partitions, reservation $on") or diag($err);
	$m{$on} = [ split /\|/, $out ];
	is(q1($node, "SELECT count(*) FROM pt"), 30000, '... all rows are there');
	$node->psql('postgres', "TRUNCATE pt");
}
my $ctx = $m{on}[0] - $m{off}[0];
my $anon = ($m{on}[1] - $m{off}[1]) * 1024;
cmp_ok($ctx, '<=', 3 * 1024 * 1024, "the memory contexts grow by $ctx bytes more with reservation (at most 3 MB for 1500 relations)");
cmp_ok($anon, '<=', 3 * 1024 * 1024, "the anonymous memory of the process grows by $anon bytes more (at most 3 MB)");
$node->stop;
done_testing();
