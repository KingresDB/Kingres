# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/053_restore_point.pl
#
# kingres.create_restore_point() (design/ha-phase5.md, part C, item 5).
#   A. the protocol: a transaction in its second phase is waited for, a
#      transaction that wants to start its commit waits behind the barrier, and
#      both go on when the restore points exist;
#   B. under a multi-node write load: every server is restored (in place) to the
#      restore point with recovery_target_name, and no distributed transaction is
#      half committed: nothing is prepared, and every transaction has both rows or
#      none, while some transactions that committed after the point are missing;
#   C. refusals.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use File::Path qw(rmtree);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::RecursiveCopy;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run ();
use IPC::Cmd qw(can_run);
use KingresShardTest qw(make_cluster keys_of_group run_sql wait_for_fault_file
  start_psql finish_psql);

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

# ---------------------------------------------------------------------------
# A. the protocol
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('ra', 3, replication => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my ($ka, $ka2) = keys_of_group($n1, 1, 0, 2);
	my ($kb, $kb2) = keys_of_group($n1, 1, 1, 2);
	my ($kc) = keys_of_group($n1, 1, 2, 1);
	my $tmp = $PostgreSQL::Test::Utils::tmp_check;

	# T1: committed locally, COMMIT PREPARED not sent yet (the second phase has started).
	my $g1 = "$tmp/gate_ra1";
	append_to_file($g1, 'x');
	my $t1 = start_psql($n1,
		"SET kingres_shard.fault_injection = 'after_commit:wait:$g1'; BEGIN; "
		  . "INSERT INTO public.kv VALUES ($ka, 't1'); INSERT INTO public.kv VALUES ($kb, 't1'); COMMIT;");
	ok(wait_for_fault_file("$g1.reached", 60), 'T1 is between its local commit and COMMIT PREPARED');
	is(prepared($n2), '1', 'T1 is prepared on node 2');

	# The restore point waits for T1.
	my $rp = start_psql($n1, "SELECT node_id || ':' || group_id FROM kingres.create_restore_point('ra_point')");
	ok($n1->poll_query_until('postgres', "SELECT (kingres.internal_restore_barrier('state')->>'active')::bool"),
		'the barrier is up on the map group primary');
	ok($n3->poll_query_until('postgres', "SELECT (kingres.internal_restore_barrier('state')->>'active')::bool"),
		'and on every other node');
	is($n1->safe_psql('postgres', "SELECT kingres.internal_restore_barrier('state')->>'inflight'"), '1',
		'one transaction is in flight, so the restore point has not been made');

	# T2 wants to commit across two nodes: it waits behind the barrier.
	my $g2 = "$tmp/gate_ra2";
	append_to_file($g2, 'x');
	my $t2 = start_psql($n3,
		"SET kingres_shard.fault_injection = 'commit_waits_for_barrier:wait:$g2'; BEGIN; "
		  . "INSERT INTO public.kv VALUES ($kc, 't2'); INSERT INTO public.kv VALUES ($ka2, 't2'); COMMIT;");
	ok(wait_for_fault_file("$g2.reached", 60), 'T2 waits for the barrier before it prepares anything');
	is(prepared($n1) + prepared($n2), '1', 'T2 prepared nothing');

	# T1 finishes its second phase; the restore point is made; T2 goes on.
	unlink $g1;
	my ($o1, $e1) = finish_psql($t1);
	is($e1, '', 'T1 committed');
	unlink $g2;
	my ($orp, $erp) = finish_psql($rp);
	is($orp, "1:0\n2:1\n3:2\n", 'the restore point is made on every primary, and reported with its group');
	is($erp, '', 'without an error');
	my ($o2, $e2) = finish_psql($t2);
	is($e2, '', 'T2 committed after the restore point');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv"), '4', 'all four rows are there');
	is($n1->safe_psql('postgres', "SELECT kingres.internal_restore_barrier('state')->>'active'"), 'false', 'the barrier is down');
	is(prepared($n1) . prepared($n2) . prepared($n3), '000', 'nothing is prepared');
	# The restore point is in the WAL of every node, by its name.
	like($n2->safe_psql('postgres', "SELECT count(*) FROM pg_ls_waldir()"), qr/^[1-9]/, 'the WAL exists');

	# A barrier that was raised by a coordinator that is gone expires by itself.
	$n2->safe_psql('postgres', "SELECT kingres.internal_restore_barrier('on', 200)");
	my $t3 = start_psql($n2, "BEGIN; INSERT INTO public.kv VALUES ($kb2, 't3'); INSERT INTO public.kv VALUES ($ka2 + 0 * 1, 't3') ON CONFLICT DO NOTHING; COMMIT;");
	my ($o3, $e3) = finish_psql($t3);
	is($e3, '', 'a commit that waited behind a barrier whose time was up went on');
	ok($n2->poll_query_until('postgres', "SELECT NOT (kingres.internal_restore_barrier('state')->>'active')::bool"),
		'a barrier whose time is up is no barrier');
}

# ---------------------------------------------------------------------------
# C. refusals
# ---------------------------------------------------------------------------
{
	my ($n1, $n2) = make_cluster('rc', 2, replication => 1);
	my ($rc, $out, $err) = run_sql($n2, "SELECT kingres.create_restore_point('x')");
	like($err, qr/can only run on the map group primary/, 'a restore point is made on the map group primary only');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_restore_point('" . ('x' x 64) . "')");
	like($err, qr/1 to 63 bytes/, 'a name that is too long is refused');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_restore_point(NULL)");
	like($err, qr/1 to 63 bytes/, 'a null name is refused');
	$n2->stop;
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_restore_point('x')");
	isnt($rc, 0, 'a restore point cannot be made when a node cannot be reached');
	is($n1->safe_psql('postgres', "SELECT kingres.internal_restore_barrier('state')->>'active'"), 'false',
		'and the barrier it raised is down again');
}

# ---------------------------------------------------------------------------
# B. a restore point under load, and every server restored to it
# ---------------------------------------------------------------------------
if (!can_run('pgbench'))
{
	note('pgbench is not installed: the restore under load is not run');
}
else
{
	my ($n1, $n2, $n3) = make_cluster('rb', 3, archiving => 1, max_prepared => 100,
		extra => "max_connections = 100\n");
	my @nodes = ($n1, $n2, $n3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, txn int);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	$_->backup('rb') for @nodes;

	my $dir = $PostgreSQL::Test::Utils::tmp_check;
	append_to_file("$dir/rb_txn.sql",
		"\\set a random(1, 100000000)\n\\set b random(100000001, 200000000)\n"
		  . "BEGIN;\nINSERT INTO public.kv VALUES (:a, :a);\nINSERT INTO public.kv VALUES (:b, :a);\nCOMMIT;\n");
	my ($out, $err) = ('', '');
	my $load = IPC::Run::start(
		[ 'pgbench', '-n', '-h', $n2->host, '-p', $n2->port, '-c', 6, '-j', 2, '-T', 12, '-f', "$dir/rb_txn.sql", 'postgres' ],
		'>', \$out, '2>', \$err);
	ok($n1->poll_query_until('postgres', 'SELECT count(*) >= 200 FROM public.kv'), 'the load is running');
	my $points = $n1->safe_psql('postgres', "SELECT count(*) FROM kingres.create_restore_point('rb_point')");
	is($points, '3', 'a restore point was made on every node under load');
	my $at_point = $n1->safe_psql('postgres', "SELECT count(DISTINCT txn) FROM public.kv");
	$load->finish;
	my $final = $n1->safe_psql('postgres', "SELECT count(DISTINCT txn) FROM public.kv");
	cmp_ok($final, '>', $at_point + 20, 'the load went on after the restore point');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');

	# Everything up to the end is archived, then every server is restored to the name.
	for my $n (@nodes)
	{
		my $wal = $n->safe_psql('postgres', 'SELECT pg_walfile_name(pg_switch_wal())');
		$n->poll_query_until('postgres', "SELECT '$wal' <= last_archived_wal FROM pg_stat_archiver");
	}
	$_->stop for @nodes;
	for my $n (@nodes)
	{
		rmtree($n->data_dir);
		PostgreSQL::Test::RecursiveCopy::copypath($n->backup_dir . '/rb', $n->data_dir);
		chmod(0700, $n->data_dir);
		$n->enable_restoring($n, 0);
		$n->append_conf('postgresql.conf', "recovery_target_name = 'rb_point'\nrecovery_target_action = 'promote'\n");
		$n->start;
	}
	for my $n (@nodes)
	{
		ok($n->poll_query_until('postgres', 'SELECT NOT pg_is_in_recovery()'), $n->name . ' reached the restore point and was promoted');
	}
	is(prepared($n1) . prepared($n2) . prepared($n3), '000',
		'restored: no transaction is prepared (none was in its commit when the restore points were made)');
	my $restored = $n1->safe_psql('postgres', "SELECT count(DISTINCT txn) FROM public.kv");
	cmp_ok($restored, '<=', $at_point, 'restored: no more than the transactions that had committed when the restore point returned');
	cmp_ok($restored, '>', 0, 'restored: and some of them');
	cmp_ok($restored, '<', $final, 'restored: the later transactions are not there');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM (SELECT txn FROM public.kv GROUP BY txn HAVING count(*) <> 2) s"), '0',
		'restored: no distributed transaction is half committed');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM (SELECT txn FROM public.kv GROUP BY txn HAVING count(*) <> 2) s"), '0',
		'restored: and the other nodes agree');
	# A restored map primary is on a new timeline: the catalog does not recognise it as the map primary until
	# it has claimed the map group again, with the majority rule (S9-02).
	like($n1->safe_psql('postgres', 'SELECT kingres.promote_node(1)'), qr/"status": "(promoted|completed)"/, 'restored: the map primary claims the map group again');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster()"), '0', 'restored: the cluster check finds nothing wrong');
}

done_testing();
