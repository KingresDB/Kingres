# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/083_move_subscription_sync.pl
#
# Review S9, "Not reproduced": the table-sync worker of a move drops its slot on the source
# before the local commit that moves the table from "finished copy" to "ready".  When the
# subscription commits with synchronous_commit = on, that commit waits for the target
# group's standby, and while it waits the table is not ready and the slot is gone: after
# max(10 s, 2 x node_timeout) the move's check declared a healthy move lost and undid it.
# The subscription is made with synchronous_commit = local now: nothing in that commit
# waits for a standby, so the window is as long as a local commit.  What the switch needs
# of the target's durability does not depend on it: the target is a participant of the
# switch and is asked whether everything it flushed is confirmed by its standbys before
# the decision (t/070).
#
# The test holds the move after its subscription exists and reads the setting from the
# target, then lets the move finish and checks the rows and that nothing is left over.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster wait_for_fault_file start_psql finish_psql);

my ($n1, $n2, $n3) = make_cluster('ms', 3, logical => 1);
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 6);
});
$n1->safe_psql('postgres', join('', map { "INSERT INTO public.kv VALUES ($_, 'v$_');
" } 1 .. 60));
my $bucket = 1;
my $src = $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket");
my $tgt = ($src + 1) % 3;
my @nodes = ($n1, $n2, $n3);
my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_ms";
append_to_file($gate, 'x');
my $mv = start_psql($n1,
	"SET kingres_shard.fault_injection = 'move_after_subscription:wait:$gate'; SELECT kingres.move_bucket(1, $bucket, $tgt)");
ok(wait_for_fault_file("$gate.reached", 90), 'the move is parked after it made its subscription');
is($nodes[$tgt]->safe_psql('postgres', 'SELECT subsynccommit FROM pg_subscription'), 'local',
	'the subscription of the move does not wait for the target\'s standby');
unlink $gate;
my ($out, $err) = finish_psql($mv);
is($err, '', 'the move finishes');
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), '60', 'every row is there once');
is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $bucket"), $tgt,
	'and the bucket is on the target');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
is($n1->safe_psql('postgres', q{SELECT coalesce(string_agg(check_name, ','), '') FROM kingres.check_cluster()}), '',
	'check_cluster finds nothing');

done_testing();
