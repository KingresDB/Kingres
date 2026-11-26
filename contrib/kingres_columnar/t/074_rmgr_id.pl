# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/074_rmgr_id.pl: kingres_columnar logs its WAL under the registered resource manager id 255, named KingresColumnar.
#
#   A. pg_get_wal_resource_managers() lists id 255 as KingresColumnar, not built in
#   B. a columnar insert writes records that pg_waldump shows for the resource manager id 255, and none for the experimental id 128

use strict;
use warnings FATAL => 'all';
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run;

my $node = PostgreSQL::Test::Cluster->new('rm');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_columnar'\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

my $row = $node->safe_psql('postgres',
	q{SELECT rm_id || '|' || rm_name || '|' || rm_builtin FROM pg_get_wal_resource_managers() WHERE rm_name = 'KingresColumnar'});
is($row, '255|KingresColumnar|false', 'A: the resource manager is id 255, KingresColumnar, not built in');
is($node->safe_psql('postgres', q{SELECT count(*) FROM pg_get_wal_resource_managers() WHERE rm_id = 128}), '0',
	'A: nothing is registered under the experimental id 128');

$node->safe_psql('postgres', q{CREATE TABLE t (a int) USING columnar; INSERT INTO t SELECT generate_series(1, 1000)});
$node->stop;

my $waldir = $node->data_dir . '/pg_wal';
opendir(my $dh, $waldir) or die "cannot open $waldir: $!";
my @segs = sort grep { /^[0-9A-F]{24}$/ } readdir($dh);
closedir($dh);

sub records
{
	my ($rmgr) = @_;
	my ($out, $err) = ('', '');
	IPC::Run::run([ 'pg_waldump', '--path', $waldir, '--rmgr', $rmgr, $segs[0], $segs[-1] ], '>', \$out, '2>', \$err);
	return scalar(() = $out =~ /^rmgr: \Q$rmgr\E/mg);
}
cmp_ok(records('custom255'), '>', 0, 'B: pg_waldump shows records of the resource manager id 255');
is(records('custom128'), 0, 'B: and none of the experimental id 128');

done_testing();
