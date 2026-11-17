# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/080_switchover_check.pl
#
# Review S9-07: a planned switchover that promises no loss checks, as a step that can
# fail, that NEW has everything OLD wrote.  With NEW disconnected and not named in OLD's
# list (the allowed "or is caught up"), three acknowledged rows were lost with no record.
#
#   OLD: SELECT kingres.switchover_mark()          -- a WAL record, flushed; returns its position M
#   OLD: stopped (pg_ctl stop -m fast)
#   NEW: SELECT kingres.switchover_replayed(M)     -- true when NEW has replayed the mark (only that)
#   NEW: SELECT kingres.switchover_verify(X, 'shut down') -- X: "Latest checkpoint location" that
#                                                     pg_controldata prints for the stopped OLD
#
# Both are tested here: with NEW streaming (true), and with NEW disconnected before the
# last writes (false, and the rows are indeed missing on NEW once it is promoted).  The
# function also works after NEW was promoted, and on the map group's NEW.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run;
use KingresShardTest qw(make_cluster make_standby register_standby wait_catchup
  promote_standby run_sql);

my $seq = 0;

sub setup
{
	my ($group) = @_;
	my $prefix = 'sw' . (++$seq);
	my @n = make_cluster($prefix, 2, replication => 1);
	$n[0]->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my $old = $n[$group];
	my $new = make_standby($old, "${prefix}_s");
	my $id = register_standby($n[0], $new, $group);
	wait_catchup($old, $new);
	return ($prefix, \@n, $old, $new, $id);
}

sub checkpoint_location
{
	my ($node) = @_;
	my ($stdout, $stderr);
	IPC::Run::run([ 'pg_controldata', '-D', $node->data_dir ], '>', \$stdout, '2>', \$stderr);
	$stdout =~ /^Latest checkpoint location:\s+([0-9A-F]+\/[0-9A-F]+)/m or die "no checkpoint location in: $stdout";
	return $1;
}

foreach my $group (1, 0)
{
	# --- NEW streams: the check passes, and so does the one after the stop ----------------
	{
		my ($prefix, $n, $old, $new, $id) = setup($group);
		$old->safe_psql('postgres', 'CREATE TABLE public.t1 (a int); INSERT INTO public.t1 SELECT generate_series(1, 100)');
		my $mark = $old->safe_psql('postgres', 'SELECT kingres.switchover_mark()');
		like($mark, qr/^[0-9A-F]+\/[0-9A-F]+$/, "[$prefix] the mark is a WAL position");
		ok($new->poll_query_until('postgres', "SELECT kingres.switchover_replayed('$mark')"),
			"[$prefix] NEW has replayed the mark: the check is true");
		$old->stop('fast');
		my $x = checkpoint_location($old);
		ok($new->poll_query_until('postgres', "SELECT kingres.switchover_verify('$x', 'shut down')"),
			"[$prefix] and it has replayed the shutdown checkpoint of OLD (group $group)");
		promote_standby($new);
		is($new->safe_psql('postgres', "SELECT kingres.switchover_replayed('$mark')"), 't',
			"[$prefix] the check also works after NEW is promoted");
		is($new->safe_psql('postgres', 'SELECT count(*) FROM public.t1'), '100', "[$prefix] nothing is missing");
	}

	# --- NEW was disconnected before the last writes: the check fails, and the rows are lost
	{
		my ($prefix, $n, $old, $new, $id) = setup($group);
		$old->safe_psql('postgres', 'CREATE TABLE public.t1 (a int); INSERT INTO public.t1 SELECT generate_series(1, 100)');
		wait_catchup($old, $new);
		$new->append_conf('postgresql.conf', "primary_conninfo = 'host=/nonexistent port=1 application_name=" . $new->name . "'\n");
		$new->reload;
		ok($new->poll_query_until('postgres', "SELECT NOT EXISTS (SELECT 1 FROM pg_stat_wal_receiver)"),
			"[$prefix] NEW is disconnected from OLD");
		$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 103)');
		my $mark = $old->safe_psql('postgres', 'SELECT kingres.switchover_mark()');
		$old->stop('fast');
		my $x = checkpoint_location($old);
		is($new->safe_psql('postgres', "SELECT kingres.switchover_replayed('$mark')"), 'f',
			"[$prefix] the check on NEW is false: it did not get the writes");
		is($new->safe_psql('postgres', "SELECT kingres.switchover_verify('$x', 'shut down')"), 'f',
			"[$prefix] and so is the check against the shutdown checkpoint of the stopped OLD");
		promote_standby($new);
		is($new->safe_psql('postgres', 'SELECT count(*) FROM public.t1'), '100',
			"[$prefix] the three acknowledged rows are indeed gone from NEW (what the check protects against)");
		is($new->safe_psql('postgres', "SELECT kingres.switchover_replayed('$mark')"), 'f', "[$prefix] and it still says no after the promotion");
	}
}

# --- who may call them ---------------------------------------------------------------------
{
	my ($prefix, $n, $old, $new, $id) = setup(1);
	$old->safe_psql('postgres', 'CREATE ROLE bob LOGIN');
	my ($rc, $out, $err) = run_sql($old, "SET ROLE bob; SELECT kingres.switchover_mark()");
	like($err, qr/permission denied/, "[$prefix] an ordinary role cannot write a mark");
	($rc, $out, $err) = run_sql($new, "SELECT kingres.switchover_mark()");
	like($err, qr/hot standby|in recovery|read-only/i, "[$prefix] a standby cannot write one");
}

done_testing();
