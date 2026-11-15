# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/046_two_majors.pl
#
# One PostgreSQL major per cluster (ADR 0023, point 7), with real servers of two
# majors: one node of the installation under test and one of the other major.
#
#	- add_node() refuses the node of the other major;
#	- a cluster that was made anyway (add_node() told, through the fault setting
#	  server_major:value:<n>, that this node is of the other major) is reported
#	  by check_cluster() as server_major_differs;
#	- a move of a bucket between the two nodes is refused.
#
# The test needs a second installation of PostgreSQL, the one of the other major
# (18 or 19), with kingres_shard installed in it from this source: the
# environment variables KINGRES_PG18 and KINGRES_PG19 of kingres/dev/ext-majors.sh
# name them (a pg_config program or an installation directory).  Without one it
# skips with the reason in its plan; kingres/dev/ext-majors.sh installs the
# extension into each major in turn, so the run of the second major finds the
# extension of the first one in place.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use File::Basename qw(dirname);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node run_sql);

# The installation of the major that is not the one under test: (major, directory),
# or (undef, reason).
sub other_installation
{
	my $mine = PostgreSQL::Test::Cluster->new('probe')->pg_version->major;
	my $other = $mine == 18 ? 19 : 18;
	# Only an installation that is named explicitly is used.  Guessing one
	# (the default work area of kingres/dev/ext.sh) made this test run, and
	# fail, against whatever old build happened to be there when the suite was
	# run in tree without these variables.
	my $spec = $ENV{"KINGRES_PG$other"};
	return (undef, "no installation of PostgreSQL $other (set KINGRES_PG$other, see kingres/dev/ext-majors.sh)")
	  unless defined $spec && $spec ne '';
	my $dir = -d $spec ? $spec : dirname(dirname($spec));
	my $pg_config = "$dir/bin/pg_config";
	return (undef, "$dir has no pg_config (KINGRES_PG$other)") unless -x $pg_config;
	my $version = `$pg_config --version`;
	return (undef, "$dir is not a PostgreSQL $other installation ($version)")
	  unless $version =~ /^PostgreSQL $other(\.|beta|rc|devel)/;
	chomp(my $libdir = `$pg_config --pkglibdir`);
	chomp(my $sharedir = `$pg_config --sharedir`);
	return (undef, "kingres_shard is not installed in $dir (run kingres/dev/ext-majors.sh first)")
	  unless -e "$libdir/kingres_shard.so" && -e "$sharedir/extension/kingres_shard.control";
	# The script of the version is the one of this source tree.
	my $script = "$FindBin::RealBin/../kingres_shard--0.4.sql";
	if (open(my $a, '<', $script) && open(my $b, '<', "$sharedir/extension/kingres_shard--0.4.sql"))
	{
		local $/;
		return (undef, "the kingres_shard installed in $dir is not built from this source")
		  if <$a> ne <$b>;
	}
	return ($other, $dir);
}

my ($other_major, $other_dir) = other_installation();
plan skip_all => "needs a second PostgreSQL installation: $other_dir" unless defined $other_major;

my $n1 = make_node('tm1', logical => 1);
my $n2 = make_node('tm2', logical => 1, install_path => $other_dir);
my $major = $n1->pg_version->major;
is($n2->pg_version->major, $other_major, "the second node runs PostgreSQL $other_major, the first PostgreSQL $major");

$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres',
	sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));

# The node of the other major is refused.
my ($rc, $out, $err) = run_sql($n1, sprintf(
	"\\set VERBOSITY verbose\nSELECT kingres.add_node('%s', %d)", $n2->host, $n2->port));
isnt($rc, 0, "add_node of a PostgreSQL $other_major node to a PostgreSQL $major cluster fails");
like($err, qr/55000/, 'with object_not_in_prerequisite_state');
like($err, qr/runs PostgreSQL $other_major, this node runs PostgreSQL $major/, 'and says which majors differ');
is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.node}), '1', 'the node is not registered');

# Make the cluster of two majors anyway: the node that adds is told it is of the other major.
($rc, $out, $err) = run_sql($n1, sprintf(
	"SET kingres_shard.fault_injection = 'server_major:value:%d';\nSELECT kingres.add_node('%s', %d)",
	$other_major, $n2->host, $n2->port));
is($rc, 0, 'a cluster of two majors can be made by hand (the check is told otherwise)') or diag $err;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.t (id int PRIMARY KEY, v int);
	SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 4);
});
$n1->safe_psql('postgres', join('', map { "INSERT INTO public.t VALUES ($_, $_);
" } 1 .. 40));

# check_cluster() names the node of the other major.
is($n1->safe_psql('postgres', q{
		SELECT coalesce(string_agg(check_name || ':' || node_id, ' ' ORDER BY check_name, node_id), '')
		  FROM kingres.check_cluster()}),
	'server_major_differs:2', 'check_cluster reports the node of the other major');
like($n1->safe_psql('postgres', q{SELECT detail FROM kingres.check_cluster() WHERE check_name = 'server_major_differs'}),
	qr/runs PostgreSQL $other_major, this node PostgreSQL $major/, 'with the two majors in the detail');
is($n2->safe_psql('postgres', q{SELECT kingres.internal_local_state() ->> 'server_major'}),
	"$other_major", 'the state of the node of the other major carries its major');

# A move between the two nodes is refused, in both directions, and changes nothing.
for my $dir ([ 0, 1 ], [ 1, 0 ])
{
	my ($from, $to) = @$dir;
	my $bucket = $n1->safe_psql('postgres',
		"SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = $from");
	($rc, $out, $err) = run_sql($n1, "\\set VERBOSITY verbose\nSELECT kingres.move_bucket(1, $bucket, $to)");
	isnt($rc, 0, "a move of bucket $bucket from group $from to group $to is refused");
	like($err, qr/55000/, 'with object_not_in_prerequisite_state');
	like($err, qr/different PostgreSQL major versions/, 'and the reason');
	is($n1->safe_psql('postgres', "SELECT state FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket"),
		'active', 'the bucket is still active');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.bucket_move'), '0', 'no move was recorded');
}
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.t'), '40', 'the rows are all there');

done_testing();
