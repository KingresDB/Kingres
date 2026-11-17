# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/097_verify_checks.pl
#
# Review S11-10: three checks of kingres.switchover_verify() were not established by any test,
# because another check always answered first (mutants M2, M3, M4 survived t/085).  Each case here
# is built so that only the check it names can answer:
#
#   M2 "this node has replayed WAL beyond the shutdown checkpoint": the position is a real shutdown
#      checkpoint of the old primary that was restarted afterwards (S11-08): the record is there and
#      is a shutdown checkpoint, and the standby follows the primary far beyond it;
#   M4 "this node left the timeline of the checkpoint ... not at its end": the same position on the
#      standby after it was promoted: the record is read and is a shutdown checkpoint, and only the
#      history of the timeline says that WAL follows it;
#   M3 "the record at this position is not a shutdown checkpoint": the position of an online
#      checkpoint, on a standby that was stopped by recovery_target_lsn exactly at its end, so that
#      the replayed position equals the end of the record and nothing else can refuse it.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run;
use KingresShardTest qw(make_cluster make_standby wait_catchup promote_standby run_sql);

sub controldata
{
	my ($node) = @_;
	my ($out, $err);
	IPC::Run::run([ 'pg_controldata', '-D', $node->data_dir ], '>', \$out, '2>', \$err);
	$out =~ /^Database cluster state:\s+(.+?)\s*$/m or die "no state in: $out";
	my $state = $1;
	$out =~ /^Latest checkpoint location:\s+([0-9A-F]+\/[0-9A-F]+)/m or die "no checkpoint location in: $out";
	return ($state, $1);
}

sub verify
{
	my ($node, $x) = @_;
	my ($rc, $out, $err) = run_sql($node, "\\set VERBOSITY verbose\nSELECT kingres.switchover_verify('$x', 'shut down')");
	return ($out =~ /^(t|f)$/m ? $1 : ($err =~ /KS006/ ? 'KS006' : 'other'), $err);
}

# --- M2 and M4: a stale position ------------------------------------------------------------------
{
	my ($old) = make_cluster('vc1', 1, replication => 1);
	$old->safe_psql('postgres', 'CREATE TABLE public.t1 (a int); INSERT INTO public.t1 SELECT generate_series(1, 100)');
	my $new = make_standby($old, 'vc1_s');
	wait_catchup($old, $new);
	$old->stop('fast');
	my ($state, $x1) = controldata($old);
	is($state, 'shut down', '[vc1] the old primary stopped cleanly');
	$old->start;
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 400)');
	wait_catchup($old, $new);
	my ($ans, $err) = verify($new, $x1);
	is($ans, 'KS006', '[vc1] the stale position of an earlier clean stop is refused on the standby');
	like($err, qr/replayed WAL beyond the shutdown checkpoint/, "[vc1] by the check of the replayed position ($err)");
	promote_standby($new);
	($ans, $err) = verify($new, $x1);
	is($ans, 'KS006', '[vc1] and after promotion');
	like($err, qr/left the timeline of the shutdown checkpoint/, "[vc1] by the check of the end of the timeline ($err)");
}

# --- M3: an online checkpoint at which the standby stopped ----------------------------------------
{
	my ($p) = make_cluster('vc2', 1, replication => 1);
	$p->safe_psql('postgres', 'CREATE TABLE public.t1 (a int); INSERT INTO public.t1 SELECT generate_series(1, 100)');
	$p->backup('vc2_bk');
	$p->safe_psql('postgres', 'CHECKPOINT');
	my $x = $p->safe_psql('postgres', 'SELECT checkpoint_lsn FROM pg_control_checkpoint()');
	$p->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 200)');
	my $sb = PostgreSQL::Test::Cluster->new('vc2_s');
	$sb->init_from_backup($p, 'vc2_bk', has_streaming => 1);
	$sb->append_conf('postgresql.conf', "recovery_target_lsn = '$x'\nrecovery_target_action = 'pause'\nrecovery_target_inclusive = on\n");
	$sb->start;
	ok($sb->poll_query_until('postgres', "SELECT pg_is_wal_replay_paused() AND pg_last_wal_replay_lsn() > '$x'::pg_lsn"),
		'[vc2] the standby stopped right after the online checkpoint');
	my ($ans, $err) = verify($sb, $x);
	is($ans, 'KS006', '[vc2] the position of an online checkpoint is refused');
	like($err, qr/not a shutdown checkpoint/, "[vc2] by the check of the kind of the record ($err)");
}

done_testing();
