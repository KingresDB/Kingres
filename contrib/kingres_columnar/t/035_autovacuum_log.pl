# SPDX-License-Identifier: AGPL-3.0-only
# contrib/kingres_columnar/t/035_autovacuum_log.pl
#
# A visit of autovacuum to a columnar table can be seen: the server log has the line
# heap's vacuum writes for autovacuum (log_autovacuum_min_duration), and the statistics
# count the visit.  Before this the log showed only "automatic analyze" for a columnar
# table, and t/013_xid_wraparound.pl took the absence of a vacuum line for the absence of
# a visit.  The test makes autovacuum vacuum a columnar table by the insert threshold and
# looks for the line and for the counter; with log_autovacuum_min_duration = -1 there is
# no line, as for heap.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colavlog');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
autovacuum = on
autovacuum_naptime = 1s
autovacuum_vacuum_insert_threshold = 20
log_autovacuum_min_duration = 0
});
$node->start;
$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE TABLE t (a int) USING columnar;
CREATE TABLE h (a int);
INSERT INTO t SELECT generate_series(1, 3000);
INSERT INTO h SELECT generate_series(1, 3000);
});

$node->poll_query_until('postgres',
	q{SELECT autovacuum_count > 0 FROM pg_stat_all_tables WHERE relname = 't'})
  or diag('autovacuum did not visit the columnar table');
is($node->safe_psql('postgres', q{SELECT autovacuum_count > 0 AND last_autovacuum IS NOT NULL FROM pg_stat_all_tables WHERE relname = 't'}),
	't', 'the statistics count the visit of autovacuum to the columnar table');

$node->poll_query_until('postgres',
	q{SELECT autovacuum_count > 0 FROM pg_stat_all_tables WHERE relname = 'h'});
my $log = slurp_file($node->logfile);
like($log, qr/automatic vacuum of table "postgres\.\w+\.t": columnar: 3000 rows, \d+ blocks, relfrozenxid \d+, creation id (?:frozen|kept)/,
	'the log has a line for the visit, in the words of the heap line');
like($log, qr/automatic vacuum of table "postgres\.\w+\.h": index scans/, 'and the heap table has its own line (the same prefix)');

# without the setting there is no line, as for heap
$node->append_conf('postgresql.conf', "log_autovacuum_min_duration = -1\n");
$node->reload;
my $off = -s $node->logfile;
$node->safe_psql('postgres', 'INSERT INTO t SELECT generate_series(1, 3000)');
$node->poll_query_until('postgres',
	q{SELECT autovacuum_count > 1 FROM pg_stat_all_tables WHERE relname = 't'});
unlike(slurp_file($node->logfile, $off), qr/automatic vacuum of table "postgres\.\w+\.t"/,
	'with log_autovacuum_min_duration = -1 the visit is not logged');

$node->stop;
done_testing();
