# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/045_server_major.pl
#
# One PostgreSQL major per cluster (ADR 0023, point 7): add_node() refuses a node
# of another major than this one.  Nodes of two majors are formed in
# t/046_two_majors.pl, which needs two installations; here the major that this
# node reports is changed by the fault setting server_major:value:<n> (fault.c),
# so that the refusal is tested on every single-major installation too.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster run_sql);

my ($n1, $n2) = make_cluster('sm', 2);
my $n3 = make_node('sm3');
my $major = $n1->pg_version->major;

# The state a node reports carries its major.
is($n1->safe_psql('postgres', q{SELECT kingres.internal_local_state() ->> 'server_major'}),
	"$major", 'the state of a node has the PostgreSQL major');
is($n2->safe_psql('postgres', q{SELECT (kingres.internal_local_state() ->> 'server_major')::int = current_setting('server_version_num')::int / 10000}),
	't', 'the major in the state is the one of the server_version_num');

# A node that this node takes for another major is refused, whichever way it differs.
for my $fake ($major - 1, $major + 1)
{
	my ($rc, $out, $err) = run_sql($n1, sprintf(
		"\\set VERBOSITY verbose\nSET kingres_shard.fault_injection = 'server_major:value:%d';\nSELECT kingres.add_node('%s', %d)",
		$fake, $n3->host, $n3->port));
	isnt($rc, 0, "add_node of a node of another major ($fake against $major) fails");
	like($err, qr/55000/, 'with object_not_in_prerequisite_state');
	like($err, qr/node \S+:\d+ runs PostgreSQL $major, this node runs PostgreSQL $fake/, 'and says which majors differ');
	like($err, qr/must run the same major version/, 'and what the rule is');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.node}), '2', 'the node is not registered');
	is($n3->safe_psql('postgres', q{SELECT count(*) FROM pg_extension WHERE extname = 'kingres_shard'}),
		'0', 'nothing was created on the new node');
}

# The real major is no change: a node of the same major is taken.
my ($rc, $out, $err) = run_sql($n1, sprintf(
	"SET kingres_shard.fault_injection = 'server_major:value:%d';\nSELECT kingres.add_node('%s', %d)",
	$major, $n3->host, $n3->port));
is($rc, 0, 'add_node of a node of the same major works') or diag $err;
is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.node}), '3', 'the node is registered');

# A cluster of one major has nothing to report.
is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'server_major_differs'}),
	'0', 'check_cluster has no server_major_differs in a cluster of one major');

done_testing();
