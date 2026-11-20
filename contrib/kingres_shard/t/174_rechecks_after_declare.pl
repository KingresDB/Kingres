# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/174_rechecks_after_declare.pl
#
# Review P2F-07 (mutants M8 and M9 of the review survived every test): after the cursors of a fragment scan are open, every group is
# asked for its placement again (kingres_shard_probe_read_after) and this node recomputes its own (fs_local_check, the check of the own
# part).  A real move cannot overtake the statement (its switch waits for the cursors), so, as t/136 does for the fan-out, the
# catalog of a node is changed by hand behind the statement, at the point where it waits (frag_after_declare), and the statement must
# fail with the retryable KS001 and return nothing; with the map as it was the statement gives the baseline.
#
#   A. a bucket of a REMOTE group's map is in another group when the group is asked again: the recheck of that group;
#   B. the same in this node's OWN map, after its own cursor is open and the groups were asked again (frag_before_own_check): the check of the own part;
# each for a closed query and for a partial aggregation.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql);

my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my @nodes = make_cluster('rc', 3, extra => "kingres_shard.enable_partial_aggregation = on\n");
my ($n1, $n2, $n3) = @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.d (id int, k int, g int);
	SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 12);
	INSERT INTO public.d SELECT g % 300, g, g % 7 FROM generate_series(1, 3000) g;
});
$_->safe_psql('postgres', 'ANALYZE') for @nodes;
my $COLOC = "(SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.d'::regclass)";
my $B1 = $n1->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = $COLOC AND group_id = 1");
my $B0 = $n1->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = $COLOC AND group_id = 0");

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# the bucket $b of the colocation group is in group $to in the catalog of $node (by hand, no triggers)
sub set_group
{
	my ($node, $b, $to) = @_;
	$node->safe_psql('postgres', "SET session_replication_role = replica; UPDATE kingres.bucket_map SET group_id = $to WHERE colocation_id = $COLOC AND bucket = $b");
}

sub with_gate
{
	my ($point, $stmt, $during, $tag) = @_;
	my $gate = "$tmp/gate_rc_$tag";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, "SET kingres_shard.fault_injection = '$point:wait:$gate'; $stmt");
	ok(wait_for_fault_file("$gate.reached", 60), "the statement waits at $point ($tag)");
	$during->();
	unlink $gate;
	return finish_psql($sess);
}

my @q = (
	[ 'a closed query', q{SELECT id, count(*) FROM public.d GROUP BY id ORDER BY id LIMIT 20}, qr/Pushed: closed/ ],
	[ 'a partial aggregation', q{SELECT g, count(*), sum(k) FROM public.d GROUP BY g ORDER BY g}, qr/Finalize/ ],
);
my $i = 0;
for my $q (@q)
{
	my ($name, $sql, $path) = @$q;
	like(plan_of($n1, $sql), $path, "$name: shipped");
	my ($rc, $want, $err) = run_sql($n1, $sql);
	is($rc, 0, "$name: the baseline runs") or diag($err);
	$want =~ s/^SET\n//;

	# A. a remote group's own map says that a bucket is elsewhere when it is asked again
	{
		my ($out, $e) = with_gate('frag_after_declare', $sql, sub { set_group($n2, $B1, 2); }, 'a' . $i);
		set_group($n2, $B1, 1);
		like($e, qr/KS001|the bucket map changed/, "A: $name: a remote group whose map changed after the DECLARE fails the statement") or diag($e);
		unlike($out, qr/\d/, 'A: ... and returned no row');
		my ($rc2, $o2, $e2) = run_sql($n1, $sql);
		is($o2, $want, 'A: ... and with the map as it was the statement gives the baseline') or diag($e2);
	}
	# B. this node's own map changes in the same window: the check of the own part
	{
		my ($out, $e) = with_gate('frag_before_own_check', $sql, sub { set_group($n1, $B0, 2); }, 'b' . $i);
		set_group($n1, $B0, 0);
		like($e, qr/the bucket map changed while the statement was reading it/, "B: $name: this node's own map changed after its own cursor is open and the groups were asked again fails the statement") or diag($e);
		unlike($out, qr/\d/, 'B: ... and returned no row');
		my ($rc2, $o2, $e2) = run_sql($n1, $sql);
		is($o2, $want, 'B: ... and with the map as it was the statement gives the baseline') or diag($e2);
	}
	$i++;
}
is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;
is($n1->safe_psql('postgres', q{SELECT count(*) FROM pg_cursors}), 0, 'no cursor is left on node 1');

done_testing();
