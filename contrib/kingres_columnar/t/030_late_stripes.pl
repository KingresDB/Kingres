# contrib/kingres_columnar/t/030_late_stripes.pl
#
# C7-06.  What a row written after the access method's commit-time flush costs, as
# the README states it: a row written one at a time (an INSERT, or the rows of an
# INSERT ... SELECT, by a callback of another module that runs after the access
# method's own pre-commit) is a stripe of its own, 8192 bytes on disk for a row of
# 4 bytes; a COPY or a multi-row insert is one stripe per batch.  VACUUM does not
# join the stripes (an earlier comment of the code said it did); columnar_compact
# does, and then the table reads as fast as one made in one piece, but the space of
# the old stripes is not returned until VACUUM FULL.  The numbers the README gives
# for 100 000 rows (100 000 stripes, 819 200 000 bytes, a scan of 0.52 s against
# 0.006 s after the compaction) were measured by hand; this file pins their shape
# with 5 000 rows.
#
# Needs a compiler and the server's headers for the probe module.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcProbe qw(build_module);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $dir = build_module('kcprobe');

my $node = PostgreSQL::Test::Cluster->new('collate');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = '$dir/kcprobe,kingres_columnar'\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub q1 { return $node->safe_psql('postgres', $_[0]); }

my $N = 5000;
q1("CREATE TABLE late (a int) USING columnar; CREATE TABLE lateh (a int); CREATE TABLE batch (a int) USING columnar");
$node->psql('postgres', "BEGIN; SET LOCAL kcprobe.commit_sql = 'INSERT INTO late SELECT g FROM generate_series(1, $N) g; INSERT INTO lateh SELECT g FROM generate_series(1, $N) g'; SELECT 1; COMMIT;");
is(q1("SELECT count(*) || '/' || sum(a) FROM late"), q1("SELECT count(*) || '/' || sum(a) FROM lateh"), "$N rows written one at a time after the commit-time flush: all there, as in the heap twin");
is(q1("SELECT count(*) FROM kingres.columnar_stripes('late')"), $N, '... one stripe per row (the cost the README states)');
is(q1("SELECT bytes_on_disk / rows FROM kingres.columnar_info('late')"), 8192, '... 8192 bytes on disk for each row');

# a COPY is one write: a stripe per batch, not per row
$node->psql('postgres', "BEGIN; SET LOCAL kcprobe.commit_sql = 'COPY batch (a) FROM PROGRAM \$\$seq 1 $N\$\$'; SELECT 1; COMMIT;");
is(q1("SELECT count(*) FROM batch"), $N, "a late COPY of $N rows: all there");
cmp_ok(q1("SELECT count(*) FROM kingres.columnar_stripes('batch')"), '<=', 5, '... in a few stripes, not one per row');

# VACUUM joins them when there are many (C13-05, kingres_columnar.vacuum_compact_stripes, 128 by default); with the setting at 0 it
# leaves them as they are, and columnar_compact joins them
q1("SET kingres_columnar.vacuum_compact_stripes = 0; VACUUM late");
is(q1("SELECT count(*) FROM kingres.columnar_stripes('late')"), $N, 'VACUUM with the merge turned off leaves the stripes as they are');
my $before = q1("SELECT pg_relation_size('late')");
q1("SELECT kingres.columnar_compact('late', 1000000)");
cmp_ok(q1("SELECT count(*) FROM kingres.columnar_stripes('late')"), '<=', 2, 'columnar_compact joins them');
is(q1("SELECT count(*) || '/' || sum(a) FROM late"), q1("SELECT count(*) || '/' || sum(a) FROM lateh"), '... and changes no row');
is(q1("SELECT pg_relation_size('late')") >= $before ? 'kept' : 'shrunk', 'kept', '... the space of the old stripes is not returned by it');
q1("VACUUM FULL late");
cmp_ok(q1("SELECT pg_relation_size('late')"), '<', $before / 10, '... VACUUM FULL returns it');
is(q1("SELECT count(*) || '/' || sum(a) FROM late"), q1("SELECT count(*) || '/' || sum(a) FROM lateh"), '... and the table reads the same');
is(q1("SELECT count(*) FROM kingres.columnar_check()"), 0, 'columnar_check has nothing to report');

$node->stop;
done_testing();
