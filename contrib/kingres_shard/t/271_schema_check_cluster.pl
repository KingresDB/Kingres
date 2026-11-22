# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/271_schema_check_cluster.pl
#
# Phase 11 (design/shard-phase11.md 9.10): kingres.check_cluster() looks at the distributed schemas of every node.  A clean cluster, and a cluster after a move,
# have no finding; each way of going wrong is a finding of its own: schema_missing, schema_table_missing, schema_structure_differs, stray_schema_rows,
# schema_catalog_differs.  The checks are structural: a copy that holds fewer rows than its source is not a finding.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @nodes = make_cluster('cc', 3, logical => 1, extra => "kingres_shard.move_cleanup_delay = 3600\n");
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }
sub findings
{
	return q1($n1, q{SELECT coalesce(string_agg(check_name || ' n' || node_id || ' ' || coalesce(object, '-'), '; ' ORDER BY check_name, node_id, object), '') FROM kingres.check_cluster()});
}
sub schema_findings
{
	return q1($n1, q{SELECT coalesce(string_agg(check_name || ' n' || node_id || ' ' || coalesce(object, '-'), '; ' ORDER BY check_name, node_id, object), '')
	                   FROM kingres.check_cluster() WHERE check_name IN ('schema_missing', 'schema_table_missing', 'schema_structure_differs', 'stray_schema_rows', 'schema_catalog_differs')});
}

q1($n1, q{SELECT kingres.set_sharding_mode('schema', true)});
sub node_of_group { my ($g) = @_; for my $n (@nodes) { return $n if q1($n, q{SELECT group_id FROM kingres.node WHERE node_id = (SELECT node_id FROM kingres.local_node)}) == $g; } die "no node for group $g"; }
my ($g1, $g2) = split /\n/, q1($n1, q{SELECT group_id FROM kingres.node ORDER BY node_id LIMIT 2});
my $nb = node_of_group($g2);		# holds ca after its move, not cb
my $nc = (grep { $_ != $nb && $_ != node_of_group($g1) } @nodes)[0];	# holds neither
my $no = node_of_group($g1);		# the owner of cb
q1($n1, qq{SET kingres_shard.schema_placement_group = $g1; CREATE SCHEMA ca; CREATE SCHEMA cb});
q1($n1, q{CREATE TABLE ca.t1 (id int PRIMARY KEY, v text); CREATE TABLE ca.t2 (id int PRIMARY KEY); CREATE TABLE cb.t1 (id int PRIMARY KEY, v text)});
q1($n1, q{INSERT INTO ca.t1 SELECT g, 'r' || g FROM generate_series(1, 30) g; INSERT INTO cb.t1 SELECT g, 'b' || g FROM generate_series(1, 10) g});

is(findings(), '', 'a clean cluster has no finding');

# a move: while the old copy is cleaned later, its rows are not strays; the schema is where the move put it
{
	my $c = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'ca'::regnamespace});
	q1($n1, qq{SELECT kingres.move_bucket($c, 0, $g2)});
	is(findings(), '', 'right after a move (the old copy still holds its rows, the cleaning is due later) there is no finding');
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(findings(), '', 'and after the cleaning');
}

# every way of going wrong, one at a time ($nb holds ca since its move and not cb, $nc holds neither)
my $off = 'SET kingres_shard.enable_routing = off; ';
{
	q1($nb, $off . q{ALTER TABLE cb.t1 ADD COLUMN extra int});
	like(schema_findings(), qr/schema_structure_differs n\d+ cb.t1/, 'a table whose structure differs on a node is found') or diag('findings: [' . schema_findings() . '] all: [' . findings() . ']');
	q1($nb, $off . q{ALTER TABLE cb.t1 DROP COLUMN extra});
	is(schema_findings(), '', '... and it is gone when the structure is the same again');
}
{
	q1($nb, $off . q{INSERT INTO cb.t1 VALUES (1000, 'stray')});
	like(schema_findings(), qr/stray_schema_rows n\d+ cb.t1/, 'a row in the empty copy of a table on a node that does not hold the schema is found') or diag('findings: [' . schema_findings() . '] all: [' . findings() . ']');
	q1($nb, $off . q{DELETE FROM cb.t1 WHERE id = 1000});
	is(schema_findings(), '', '... and it is gone when the row is');
}
{
	q1($nc, $off . q{ALTER TABLE ca.t2 RENAME TO t2_gone});
	like(schema_findings(), qr/schema_table_missing n\d+ ca.t2/, 'a table that a node lacks is found') or diag('findings: [' . schema_findings() . '] all: [' . findings() . ']');
	q1($nc, $off . q{ALTER TABLE ca.t2_gone RENAME TO t2});
	is(schema_findings(), '', '... and it is gone when the table is back');
}
{
	q1($nc, $off . q{UPDATE kingres.colocation SET placement_epoch = placement_epoch + 5 WHERE colocation_id = (SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'cb'::regnamespace)});
	like(schema_findings(), qr/schema_catalog_differs n\d+/, 'a catalog that does not say what the map group says is found') or diag('findings: [' . schema_findings() . '] all: [' . findings() . ']');
	q1($nc, $off . q{UPDATE kingres.colocation SET placement_epoch = placement_epoch - 5 WHERE colocation_id = (SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'cb'::regnamespace)});
	is(schema_findings(), '', '... and it is gone when the catalog is the same again');
}
{
	q1($nb, $off . q{DROP SCHEMA cb CASCADE});
	like(schema_findings(), qr/schema_missing n\d+ cb/, 'a schema that a node lacks is found') or diag('findings: [' . schema_findings() . '] all: [' . findings() . ']');
}

# fewer rows in a copy is no finding: the check is structural
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'stray_schema_rows'}), 0, 'no stray row is reported where there is none');

done_testing();
