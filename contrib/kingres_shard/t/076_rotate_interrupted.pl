# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/076_rotate_interrupted.pl
#
# Review S9-03: a node that holds a rotation of the cluster secret prepared can
# always be reached by the node that can finish it.  Until the fix a node that died
# between PREPARE and COMMIT PREPARED of kingres.rotate_secret() came back with the
# old secret while the origin held the new one, every session between them was
# refused in both directions, and recovery needed such a session to commit the
# rotation: only a human running COMMIT PREPARED helped.
#
# Every node keeps the secret it replaced ("previous") and the node that finishes
# the rotation opens its session with it when the node refuses the current one; the
# previous secret is never accepted from anybody.  Each scenario ends with every
# node on the same secret, nothing prepared, check_cluster() empty and a distributed
# transaction through the node that was cut off, with no manual step but the
# recovery call that the worker makes by itself.
#
# 1. a participant dies between its PREPARE and its COMMIT PREPARED (the review's script)
# 2. the origin dies after its own commit, before it told any participant
# 3. the origin dies before its decision: the rotation is rolled back everywhere
# 4. the previous secret is never accepted, and recovery drops it once nobody needs it

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Digest::SHA qw(hmac_sha256_hex);
use KingresShardTest qw(make_cluster keys_of_group wait_for_fault_file start_psql finish_psql run_sql);

my $seq = 0;

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }
sub secret_md5 { return $_[0]->safe_psql('postgres', 'SELECT md5(secret) FROM kingres.cluster_secret'); }

sub setup
{
	my $prefix = 'rs' . (++$seq);
	my @n = make_cluster($prefix, 3);
	$n[0]->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my ($ka) = keys_of_group($n[0], 1, 0, 1);
	my ($kb) = keys_of_group($n[0], 1, 1, 1);
	my ($kc) = keys_of_group($n[0], 1, 2, 1);
	return ($prefix, \@n, $ka, $kb, $kc);
}

# The cluster is whole again: one secret, nothing prepared, nothing to report, and a
# transaction that writes in every group works from every node.
sub check_whole
{
	my ($prefix, $nodes, $ka, $kb, $kc, $gen) = @_;
	my %md5 = map { secret_md5($_) => 1 } @$nodes;
	is(scalar(keys %md5), 1, "[$prefix] every node holds the same secret");
	is(prepared($_), '0', "[$prefix] nothing is prepared on node " . $_->name) for @$nodes;
	is($nodes->[0]->safe_psql('postgres', 'SELECT count(*) FROM kingres.check_cluster()'), '0',
		"[$prefix] check_cluster() shows nothing");
	my $i = 0;
	for my $n (@$nodes)
	{
		my $v = "g${gen}_" . $i++;
		my ($rc, $out, $err) = run_sql($n, "BEGIN; INSERT INTO public.kv VALUES ($ka + 1000 * $i * $gen, '$v'); "
			  . "INSERT INTO public.kv VALUES ($kb + 1000 * $i * $gen, '$v'); "
			  . "INSERT INTO public.kv VALUES ($kc + 1000 * $i * $gen, '$v'); COMMIT;");
		is($rc, 0, "[$prefix] a distributed transaction through node " . $n->name . " commits") or diag($err);
	}
}

# --- 1. a participant dies between PREPARE and COMMIT PREPARED -------------------------
{
	my ($prefix, $nodes, $ka, $kb, $kc) = setup();
	my ($n1, $n2, $n3) = @$nodes;
	my $before = secret_md5($n1);
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_$prefix";
	append_to_file($gate, 'x');
	my $rot = start_psql($n1, "SET kingres_shard.fault_injection = 'after_log:wait:$gate'; SELECT kingres.rotate_secret();");
	ok(wait_for_fault_file("$gate.reached", 60), "[$prefix] the rotation is prepared on the other nodes");
	is(prepared($n2), '1', "[$prefix] node 2 holds the rotation prepared");
	$n2->stop('immediate');
	unlink $gate;
	my ($out, $err) = finish_psql($rot);
	like($err, qr/could not commit prepared transaction .* on node .*recovery will commit it/s,
		"[$prefix] the origin could not tell node 2 to commit");
	isnt(secret_md5($n1), $before, "[$prefix] the origin and node 3 hold the new secret");
	$n2->start;
	is(secret_md5($n2), $before, "[$prefix] node 2 holds the old one and the rotation prepared");
	# What the worker does by itself, within recovery_interval.
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '1',
		"[$prefix] recovery commits the rotation on node 2 although it holds the old secret");
	check_whole($prefix, $nodes, $ka, $kb, $kc, 1);
	is($n1->safe_psql('postgres', 'SELECT previous IS NOT NULL FROM kingres.cluster_secret'), 't',
		"[$prefix] the pass that needed the previous secret keeps it");
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($n1->safe_psql('postgres', 'SELECT previous IS NULL FROM kingres.cluster_secret'), 't',
		"[$prefix] the next pass of the origin's recovery, which reaches every primary with the new secret, drops the previous one");
}

# --- 2. the origin dies after its own commit ---------------------------------------------
{
	my ($prefix, $nodes, $ka, $kb, $kc) = setup();
	my ($n1, $n2, $n3) = @$nodes;
	my $before = secret_md5($n1);
	my $rot = start_psql($n1, "SET kingres_shard.fault_injection = 'after_commit:kill'; SELECT kingres.rotate_secret();");
	finish_psql($rot);
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	isnt(secret_md5($n1), $before, "[$prefix] the origin committed the new secret before it died");
	is(secret_md5($n2), $before, "[$prefix] the participants hold the old one, prepared");
	is(prepared($n2) + prepared($n3), '2', "[$prefix] the rotation is prepared on both");
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2',
		"[$prefix] recovery of the origin commits both");
	check_whole($prefix, $nodes, $ka, $kb, $kc, 1);
}

# --- 3. the origin dies before its decision ------------------------------------------------
{
	my ($prefix, $nodes, $ka, $kb, $kc) = setup();
	my ($n1, $n2, $n3) = @$nodes;
	my $before = secret_md5($n1);
	my $rot = start_psql($n1, "SET kingres_shard.fault_injection = 'after_log:kill'; SELECT kingres.rotate_secret();");
	finish_psql($rot);
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	is(secret_md5($n1), $before, "[$prefix] the origin did not commit: it holds the old secret");
	is(prepared($n2) + prepared($n3), '2', "[$prefix] the rotation is prepared on both participants");
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2',
		"[$prefix] recovery rolls both back");
	check_whole($prefix, $nodes, $ka, $kb, $kc, 1);
	is(secret_md5($n2), $before, "[$prefix] and every node is on the old secret");
}

# --- 4. the previous secret is never accepted, and it is dropped ----------------------------
{
	my ($prefix, $nodes, $ka, $kb, $kc) = setup();
	my ($n1, $n2, $n3) = @$nodes;
	my $old = secret_md5($n1);
	$n1->safe_psql('postgres', 'SELECT kingres.rotate_secret()');
	isnt(secret_md5($n1), $old, "[$prefix] the rotation worked");
	is($n1->safe_psql('postgres', 'SELECT previous IS NOT NULL FROM kingres.cluster_secret'), 't',
		"[$prefix] the origin keeps the secret it replaced");
	# A session that proves itself with a token made from the previous secret is refused.
	my $user = $n1->safe_psql('postgres', 'SELECT current_user');
	my $prev_hex = $n1->safe_psql('postgres', 'SELECT encode(previous, \'hex\') FROM kingres.cluster_secret');
	my $with = sub {
		my ($hex) = @_;
		my $tok = hmac_sha256_hex("kingres-forward-v2\0$user\0postgres\0" . '2', pack('H*', $hex));
		local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
		return run_sql($n2, 'SELECT 1');
	};
	my $cur_hex = $n2->safe_psql('postgres', 'SELECT encode(secret, \'hex\') FROM kingres.cluster_secret');
	my ($rc_cur) = $with->($cur_hex);
	is($rc_cur, 0, "[$prefix] a session that proves itself with the current secret is accepted");
	my ($rc_prev, $o, $e) = $with->($prev_hex);
	isnt($rc_prev, 0, "[$prefix] a session that proves itself with the previous secret is refused");
	like($e, qr/cannot prove it/, "[$prefix] with the refusal that names the failed proof");
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($n1->safe_psql('postgres', 'SELECT previous IS NULL FROM kingres.cluster_secret'), 't',
		"[$prefix] a recovery pass that reached every node with the new secret drops the previous one");
	check_whole($prefix, $nodes, $ka, $kb, $kc, 1);
}

done_testing();
