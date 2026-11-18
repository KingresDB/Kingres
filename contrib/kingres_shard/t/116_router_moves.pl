# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/116_router_moves.pl
#
# Steps 3 and 4 of phase 2b (design/shard-phase2b.md, 10.2, 10.3 and 11.5): a bulk write while a
# bucket moves and while a group changes its primary.  The rule is that a row goes only to the group
# that owns its bucket when the statement commits: a statement that sent rows to a map that changed
# fails with the retryable KS001 (or the tuple routing of a group refuses the row), and nothing of it is
# left; one whose rows were already in the source group when the switch came makes the switch wait for
# it, and the moved bucket then carries them.  These are the move and failover part of the design's 064.
#
#   A. a bucket moves between two other groups after the plan and before the first batch;
#   B. a bucket of the origin's own group moves while the statement is open: the switch waits for it;
#   C. a bucket moves while a batch to its group is in flight / after the first batch: the switch waits for
#      the writing transaction, and the rows end up in the group that owns the bucket at the end;
#   D. COPY in the same window as A;
#   E. a group changes its primary (promote_node) between the plan and the first batch;
#   F. a bucket moves into a group the statement wrote, after the last batch;
#   G. a transaction that wrote a group through its old primary meets the new one in a bulk write.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run ();
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster keys_of_group run_sql prepared_count wait_for_fault_file
  start_psql finish_psql make_standby register_standby wait_catchup set_sync promote_standby);

my $tmp = $PostgreSQL::Test::Utils::tmp_check;

sub check_rows
{
	my ($nodes, $table, $want, $label) = @_;
	my $n1 = $nodes->[0];
	is($n1->safe_psql('postgres', "SELECT count(*) FROM $table"), $want, "$label: the table holds $want rows");
	my ($wrong, $all) = (0, 0);
	my $i = 0;
	for my $nd (@$nodes)
	{
		my $mine = $nd->safe_psql('postgres', qq{SET kingres_shard.enable_routing = off;
			SELECT count(*) FROM $table k JOIN kingres.bucket_map m ON m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, k.id) WHERE m.group_id = $i});
		my $total = $nd->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT count(*) FROM $table");
		$wrong += $total - $mine;
		$all += $total;
		$i++;
	}
	is($wrong, 0, "$label: every row is in the group that owns its bucket now");
	is($all, $want, "$label: and in one group only");
	is(prepared_count($_), 0, "$label: nothing is prepared on " . $_->name) for @$nodes;
}

sub setup3
{
	my ($prefix) = @_;
	my @nodes = make_cluster($prefix, 3, logical => 1);
	$nodes[0]->safe_psql('postgres', q{
		CREATE TABLE public.mv (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.mv', 'id', bucket_count => 12);
	});
	$nodes[0]->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	return @nodes;
}

# The id of a bucket of $group (the smallest) of the colocation group 1.
sub bucket_of
{
	my ($node, $group) = @_;
	return $node->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = $group");
}

# Run $stmt on node $nodes[0] up to the fault point $point, run $during, then let it go; returns (out, err).
sub write_with_move
{
	my ($nodes, $point, $setup_sql, $stmt, $during, $tag) = @_;
	my $n1 = $nodes->[0];
	my $gate = "$tmp/gate_mv_$tag";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, "$setup_sql SET kingres_shard.fault_injection = '$point:wait:$gate'; $stmt");
	ok(wait_for_fault_file("$gate.reached", 60), "the write waits at $point ($tag)");
	$during->();
	unlink $gate;
	return finish_psql($sess);
}

# ---------------------------------------------------------------------------
# A. a bucket moves between the other two groups after the plan and before the first batch
# ---------------------------------------------------------------------------
{
	my @nodes = setup3('ma');
	my $n1 = $nodes[0];
	my $b = bucket_of($n1, 1);
	my ($out, $err) = write_with_move(\@nodes, 'bulk_before_last_flush', 'SET kingres_shard.insert_batch_rows = 100000;',
		q{INSERT INTO public.mv SELECT g, 'a' FROM generate_series(1, 240) g;},
		sub { $n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, 2)") }, 'a');
	like($err, qr/KS001|bucket map changed|map changed while the statement/, 'a bucket that moved between the plan and the first batch fails the statement with the retryable error');
	check_rows(\@nodes, 'public.mv', 0, 'after the failed statement');
	my ($rc, $o2, $e2) = run_sql($n1, q{INSERT INTO public.mv SELECT g, 'a' FROM generate_series(1, 240) g});
	is($rc, 0, 'the statement repeated is planned with the new map and works') or diag($e2);
	check_rows(\@nodes, 'public.mv', 240, 'after the repeat');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.mv WHERE kingres.bucket_for(1, id) = $b"), $n1->safe_psql('postgres', "SELECT count(*) FROM public.mv WHERE kingres.bucket_for(1, id) = $b"), 'the moved bucket is read');
}

# ---------------------------------------------------------------------------
# B. a bucket of the origin's own group moves away in the same window
# ---------------------------------------------------------------------------
{
	my @nodes = setup3('mb');
	my $n1 = $nodes[0];
	my $b = bucket_of($n1, 0);
	my $mover;
	# The statement holds ROW EXCLUSIVE on the table of the origin from its planning on, so the switch of a
	# move of one of the origin's own buckets waits for it (10.2): the write is not disturbed.
	my ($out, $err) = write_with_move(\@nodes, 'bulk_before_last_flush', 'SET kingres_shard.insert_batch_rows = 100000;',
		q{INSERT INTO public.mv SELECT g, 'b' FROM generate_series(1, 240) g;},
		sub {
			$mover = start_psql($n1, "SELECT kingres.move_bucket(1, $b, 1)");
			sleep(4);
		}, 'b');
	unlike($err, qr/ERROR/, 'a write is not disturbed by the move of a bucket of the origin: the switch waits for it');
	my ($mo, $me) = finish_psql($mover);
	unlike($me, qr/ERROR/, '... and the move completes after it') or diag($me);
	check_rows(\@nodes, 'public.mv', 240, 'after the write and the move (origin group)');
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"), 1, 'the bucket is in its new group');
}

# ---------------------------------------------------------------------------
# C. a bucket moves after the first batch: the switch waits for the writer
# ---------------------------------------------------------------------------
{
	my @nodes = setup3('mc');
	my $n1 = $nodes[0];
	my $b = bucket_of($n1, 1);
	my $mover;
	my ($out, $err) = write_with_move(\@nodes, 'bulk_after_first_batch', 'SET kingres_shard.insert_batch_rows = 5;',
		q{INSERT INTO public.mv SELECT g, 'c' FROM generate_series(1, 240) g;},
		sub {
			$mover = start_psql($n1, "SELECT kingres.move_bucket(1, $b, 2)");
			sleep(4);
		}, 'c');
	unlike($err, qr/ERROR/, 'a write whose first batch is in does not fail when a move starts');
	my ($mo, $me) = finish_psql($mover);
	unlike($me, qr/ERROR/, '... and the move completes after it') or diag($me);
	check_rows(\@nodes, 'public.mv', 240, 'after the write and the move');
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"), 2, 'the bucket is in its new group');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.mv WHERE kingres.bucket_for(1, id) = $b"), $n1->safe_psql('postgres', "SELECT count(*) FROM public.mv WHERE kingres.bucket_for(1, id) = $b"), 'and its rows are read');
}

# ---------------------------------------------------------------------------
# D. COPY in the window of A
# ---------------------------------------------------------------------------
{
	my @nodes = setup3('md');
	my $n1 = $nodes[0];
	my $b = bucket_of($n1, 1);
	my $gate = "$tmp/gate_mv_d";
	append_to_file($gate, 'x');
	my $file = "$tmp/mv_copy.tsv";
	open(my $fh, '>', $file) or die;
	print $fh "$_\tcopy\n" for (1 .. 240);
	close $fh;
	my $data = '';
	my $sess = start_psql($n1, "SET kingres_shard.fault_injection = 'bulk_before_last_flush:wait:$gate'; SET kingres_shard.insert_batch_rows = 100000; COPY public.mv (id, v) FROM '$file'");
	ok(wait_for_fault_file("$gate.reached", 60), 'COPY waits before its last flush');
	$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, 2)");
	unlink $gate;
	my ($o, $e) = finish_psql($sess);
	like($e, qr/KS001|bucket map changed|map changed while/, 'COPY fails with the retryable error when a bucket moved');
	check_rows(\@nodes, 'public.mv', 0, 'after the failed COPY');
	my ($o2, $e2) = ('', '');
	$data = join('', map { "$_\tcopy\n" } (1 .. 240));
	my $h = IPC::Run::start([ 'psql', '-XAtq', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres', '-c', 'COPY public.mv (id, v) FROM STDIN' ], '<', \$data, '>', \$o2, '2>', \$e2);
	$h->finish;
	is($h->result(0), 0, 'COPY repeated works') or diag($e2);
	check_rows(\@nodes, 'public.mv', 240, 'after the repeated COPY');
}

# ---------------------------------------------------------------------------
# F. a bucket moves into a group the statement has written, after the last batch and before
#    the check at the end (10.2): the statement holds ROW EXCLUSIVE on the table of that group, which
#    the switch of the move waits for, so the statement is not disturbed and the move completes after it
# ---------------------------------------------------------------------------
{
	my @nodes = setup3('mf');
	my $n1 = $nodes[0];
	my @k0 = keys_of_group($n1, 1, 0, 30);
	my @k1 = keys_of_group($n1, 1, 1, 30);
	my $b = bucket_of($n1, 2);
	my $values = join(', ', map { "($_, 'f')" } (@k0, @k1));
	my $mover;
	my ($out, $err) = write_with_move(\@nodes, 'bulk_before_recheck', 'SET kingres_shard.insert_batch_rows = 100000;',
		"INSERT INTO public.mv VALUES $values;",
		sub {
			$mover = start_psql($n1, "SELECT kingres.move_bucket(1, $b, 1)");
			sleep(4);
		}, 'f');
	unlike($err, qr/ERROR/, 'a write is not disturbed by a move into a group it wrote: the switch waits for it');
	my ($mo, $me) = finish_psql($mover);
	unlike($me, qr/ERROR/, '... and the move completes after it') or diag($me);
	check_rows(\@nodes, 'public.mv', 60, 'after the write and the move into a written group');
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"), 1, 'the bucket is in its new group');
}

# ---------------------------------------------------------------------------
# E. a group changes its primary between the plan and the first batch
# ---------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('me', 2, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.mv (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.mv', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, 'me_s2');
	my $id = register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	wait_catchup($n2, $s2);
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	my $gate = "$tmp/gate_mv_e";
	append_to_file($gate, 'x');
	my $sess = start_psql($n1, "SET kingres_shard.insert_batch_rows = 100000; SET kingres_shard.fault_injection = 'bulk_before_last_flush:wait:$gate';
		INSERT INTO public.mv SELECT g, 'e' FROM generate_series(1, 120) g;");
	ok(wait_for_fault_file("$gate.reached", 60), 'the write waits before its last flush');
	$n2->safe_psql('postgres', 'CHECKPOINT');
	promote_standby($s2);
	set_sync($n2);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	unlink $gate;
	my ($out, $err) = finish_psql($sess);
	like($err, qr/KS001|bucket map changed|no longer the primary|failed while a distributed statement|map changed while/, 'a primary that changed between the plan and the first batch fails the statement with the retryable error');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.mv'), 0, '... and nothing of it is stored');
	my ($rc, $o2, $e2) = run_sql($n1, q{INSERT INTO public.mv SELECT g, 'e' FROM generate_series(1, 120) g});
	is($rc, 0, 'the statement repeated goes to the new primary') or diag($e2);
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.mv'), 120, '... and stores its rows');
	is($s2->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.mv}) + $n1->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.mv}), 120,
		'in the new primary and in the origin, once');
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for ($n1, $s2);
}

# ---------------------------------------------------------------------------
# G. a transaction that wrote a group through its old primary meets the new one (the rule of
#    4.4: one node of a group per transaction), in a bulk write
# ---------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('mg', 2, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.mg (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.mg', 'id', bucket_count => 12);
	});
	my $s2 = make_standby($n2, 'mg_s2');
	my $id = register_standby($n1, $s2, 1);
	set_sync($n2, $s2);
	wait_catchup($n2, $s2);
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	my $b = $n1->background_psql('postgres', on_error_stop => 0);
	$b->query_safe('BEGIN');
	$b->query_safe(q{INSERT INTO public.mg SELECT g, 'first' FROM generate_series(1, 60) g});
	$n2->safe_psql('postgres', 'CHECKPOINT');
	promote_standby($s2);
	set_sync($n2);
	$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
	$b->query(q{INSERT INTO public.mg SELECT g + 1000, 'second' FROM generate_series(1, 60) g});
	like($b->{stderr}, qr/already uses node .* of shard group 1/,
		'a bulk write of a transaction that wrote the group through its old primary is refused: one node of a group per transaction');
	$b->{stderr} = '';
	$b->query('ROLLBACK');
	$b->quit;
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.mg'), 0, '... and the transaction left nothing');
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for ($n1, $n2, $s2);
}

done_testing();
