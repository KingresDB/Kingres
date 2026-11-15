# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The window on the target of a move (design/shard-phase4.md, "A routed statement on
# a target that is not the map group primary"): a session of the target node reads the
# catalog cache of the extension after a switch has committed on the node, but its
# relcache entry of the table (the partition list) is older than that commit, because
# the session has not processed the switch's invalidation messages since.  The
# statement is for a bucket that has just arrived on the node; planned with the old
# partition list it would find no partition and answer "no rows".
#
# The window is forced.  A backend processes invalidation messages when it starts a
# transaction, when it takes a lock it does not hold and when a syscache miss opens a
# catalog.  The session therefore (1) reads everything its statement needs in an
# explicit transaction, with routing off so that nothing loads the catalog cache,
# which leaves it holding the locks of the catalog tables and a partition list built
# before the switch, and (2) stops in the planner hook (a fault point) after it has
# parsed its statement and before it reads the catalog cache.  The move completes on
# every node meanwhile, and the session is released.  The answer must be the row.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use Time::HiRes qw(sleep);
use KingresShardTest qw(make_cluster keys_of_group wait_for_file);

my @nodes = make_cluster('tw', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();

$n1->safe_psql('postgres', q{
	CREATE TABLE public.t (id int PRIMARY KEY, v int NOT NULL);
	SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 6);
});
{
	my $sql = '';
	$sql .= "INSERT INTO public.t VALUES ($_, $_ * 3);\n" for 1 .. 120;
	$n1->safe_psql('postgres', $sql);
}

# The target is node 2, shard group 1: not the map group primary.  The bucket
# comes from group 2.
my $target = 1;
my ($key) = keys_of_group($n1, 1, 2, 1);
my ($warm) = keys_of_group($n1, 1, $target, 1);
my $bucket = $n1->safe_psql('postgres', "SELECT kingres.bucket_for(1, $key)");
my ($mv_hold, $go1, $plan_hold) = map { "$tmp/$_" } qw(mv_hold go1 plan_hold);
sub touch { my ($f) = @_; open(my $t, '>', $f) or die; close($t); }
touch($mv_hold);
touch($plan_hold);

# The session of the target.  The statement that follows BEGIN turns routing back on
# when it ends (the scalar subquery runs when the statement starts to execute), after
# the planner has decided, with routing off, not to look at the catalog cache.
my $script = "$tmp/session.sql";
open(my $fh, '>', $script) or die;
print $fh "\\pset format unaligned\n\\pset tuples_only on\n"
  . "SELECT 'WARM ' || count(*) FROM public.t WHERE id = $warm;\n"
  . "\\! while [ ! -e $go1 ]; do sleep 0.05; done\n"
  . "SET kingres_shard.enable_routing = off;\n"
  . "BEGIN;\n"
  . "SELECT 'LOCAL ' || count(*), (SELECT set_config('kingres_shard.enable_routing', 'on', false))"
  . " FROM public.t, kingres.node, kingres.colocation, kingres.dist_table, kingres.bucket_map,"
  . " kingres.map_version, kingres.local_node WHERE public.t.id = $warm;\n"
  . "SET kingres_shard.fault_injection = 'route_before_catalog:wait:$plan_hold';\n"
  . "SELECT 'ANSWER ' || count(*) FROM public.t WHERE id = $key;\n"
  . "COMMIT;\n";
close($fh);
my $session = fork();
die "fork" unless defined $session;
if ($session == 0)
{
	system("psql -X -q -h " . $n2->host . " -p " . $n2->port . " -d postgres -f $script > $tmp/session.out 2>&1");
	POSIX::_exit(0);
}
for (1 .. 400)
{
	last if slurp("$tmp/session.out") =~ /WARM/;
	sleep(0.05);
}
like(slurp("$tmp/session.out"), qr/^WARM 1$/m, 'the session of the target has read the table once');

# The move, stopped when the copy is complete.
my $mover = fork();
die "fork" unless defined $mover;
if ($mover == 0)
{
	my $sql = "SET kingres_shard.fault_injection = 'move_after_catchup:wait:$mv_hold'; SELECT kingres.move_bucket(1, $bucket, $target)";
	system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$sql\" > $tmp/mover.out 2>&1");
	POSIX::_exit(0);
}
ok(wait_for_file("$mv_hold.reached", 120), 'the move reached the end of the catch-up');

# The session processes the messages of the move so far (its catalog cache is dropped),
# reads what it needs, and stops in the planner hook with its statement parsed.
touch($go1);
ok(wait_for_file("$plan_hold.reached", 60), 'the statement of the session stopped in the planner hook');

# The switch commits on every node while the statement waits.
unlink($mv_hold);
waitpid($mover, 0);
is($n2->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket"),
	"$target", 'the switch has committed on the target');
is($n2->safe_psql('postgres', "SELECT count(*) FROM pg_inherits WHERE inhparent = 'public.t'::regclass"),
	$n2->safe_psql('postgres', "SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = $target"),
	'and the partition is attached there');

unlink($plan_hold);
waitpid($session, 0);
is(slurp("$tmp/session.out") =~ /^ANSWER (\d+)$/m ? $1 : 'none', '1',
	'the statement of the target returns the row of the bucket that has just arrived')
  or diag(slurp("$tmp/session.out"));

# And from a fresh session the same.
is($n2->safe_psql('postgres', "SELECT count(*) FROM public.t WHERE id = $key"), '1', 'a new session returns it too');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');

done_testing();

sub slurp
{
	my ($file) = @_;
	open(my $in, '<', $file) or return '';
	local $/;
	my $text = <$in>;
	close($in);
	return $text // '';
}
