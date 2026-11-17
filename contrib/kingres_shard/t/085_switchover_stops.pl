# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/085_switchover_stops.pl
#
# Review S10-01: after a stop of the old primary that was not clean, the check of a planned
# switchover answered true while acknowledged rows were missing.  The invariant: the proof
# kingres.switchover_verify(X, state) answers true only when the new primary has every WAL
# record the old primary wrote; every other case is false ("not yet") or the error KS006
# ("cannot be proven": treat the promotion as a failover).  Every way the old primary can stop
# is run with rows written up to the stop, the answer is taken from pg_controldata of the
# stopped node as the README says, the standby is promoted, and the rows are counted: a "t"
# with a missing row fails the file.
#
# Stops: fast, smart, immediate, kill -9 of the postmaster, an immediate stop while a checkpoint
# runs, a fast stop with the standby disconnected, the same with the synchronous list emptied;
# the standby that is not the promoted one, a cascading standby, positions of other timelines
# and other nodes, and the way back (sequence 7: the old primary's place is taken by a standby
# of the new one and the same proof runs on the second switchover).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run;
use KingresShardTest qw(make_cluster make_standby wait_catchup promote_standby run_sql);

my $seq = 0;

# Database cluster state and latest checkpoint location of a stopped node, as pg_controldata
# prints them: the two things the caller hands to the proof.
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

# The answer of the proof on a node: 't', 'f' or 'KS006' (an error of that class), or the text.
sub verify
{
	my ($node, $x, $state) = @_;
	my $s = defined $state ? "'$state'" : 'NULL';
	my ($rc, $out, $err) = run_sql($node, "\\set VERBOSITY verbose\nSELECT kingres.switchover_verify('$x', $s)");
	return $out =~ /^(t|f)$/m ? $1 : ($err =~ /KS006/ ? 'KS006' : "other: $err");
}

# One node that hands over, with $n standbys; $rows rows written and acknowledged on it.
sub setup
{
	my ($nstandby) = @_;
	my $prefix = 'sv' . (++$seq);
	my ($old) = make_cluster($prefix, 1, replication => 1);
	$old->safe_psql('postgres', 'CREATE TABLE public.t1 (a int); INSERT INTO public.t1 SELECT generate_series(1, 100)');
	my @new = map { make_standby($old, "${prefix}_s$_") } (1 .. $nstandby);
	wait_catchup($old, $_) for @new;
	return ($prefix, $old, @new);
}

sub disconnect
{
	my ($new) = @_;
	$new->append_conf('postgresql.conf', "primary_conninfo = 'host=/nonexistent port=1 application_name=" . $new->name . "'\n");
	$new->reload;
	$new->poll_query_until('postgres', "SELECT NOT EXISTS (SELECT 1 FROM pg_stat_wal_receiver)");
}

# After the stop: ask the proof on $new (polling a few seconds while the answer is false),
# promote, count.  Returns (answer, rows on NEW).  Fails the test when "t" meets a missing row.
sub judge
{
	my ($prefix, $label, $new, $old, $acked, $expect, %opt) = @_;
	my ($state, $x) = controldata($old);
	my $ans = verify($new, $x, $state);
	for (my $i = 0; $i < 30 && $ans eq 'f' && !$opt{no_poll}; $i++)
	{
		select(undef, undef, undef, 0.2);
		$ans = verify($new, $x, $state);
	}
	promote_standby($new);
	my $rows = $new->safe_psql('postgres', 'SELECT count(*) FROM public.t1');
	is($ans, $expect, "[$prefix] $label: state \"$state\": the answer is $expect");
	ok($ans ne 't' || $rows == $acked, "[$prefix] $label: a true answer means no acknowledged row is missing ($rows of $acked)");
	return ($ans, $rows, $state, $x);
}

# --- 1. fast and smart stops, the standby streaming: proven ------------------------------------
foreach my $mode ('fast', 'smart')
{
	my ($prefix, $old, $new) = setup(1);
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 150)');
	$old->stop($mode);
	my ($ans, $rows, $state) = judge($prefix, "$mode stop", $new, $old, 150, 't');
	is($state, 'shut down', "[$prefix] the control file says shut down");
	is($rows, 150, "[$prefix] nothing is missing after the promotion");
	# after the promotion the proof still holds (the end of the old timeline is the record's end)
	my ($st, $x) = controldata($old);
	is(verify($new, $x, $st), 't', "[$prefix] and after pg_promote() it is still true");
}

# --- 2. immediate stop, a crash, kill -9: never proven, although nothing is lost here ---------
foreach my $mode ('immediate', 'kill9')
{
	my ($prefix, $old, $new) = setup(1);
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 150)');
	wait_catchup($old, $new);
	if ($mode eq 'immediate') { $old->stop('immediate'); }
	else
	{
		$old->kill9;
		sleep 2;    # the children notice the death of the postmaster
	}
	my ($ans, $rows, $state) = judge($prefix, "$mode stop", $new, $old, 150, 'KS006', no_poll => 1);
	is($state, 'in production', "[$prefix] the control file does not say shut down");
}

# --- 3. the review's reproduction: the standby is behind when OLD is stopped immediately --------
{
	my ($prefix, $old, $new) = setup(1);
	$old->safe_psql('postgres', 'CHECKPOINT');
	my $mark = $old->safe_psql('postgres', 'SELECT kingres.switchover_mark()');
	ok($new->poll_query_until('postgres', "SELECT kingres.switchover_replayed('$mark')"), "[$prefix] NEW replayed the mark");
	disconnect($new);
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 103)');
	$old->stop('immediate');
	my ($state, $x) = controldata($old);
	is($new->safe_psql('postgres', "SELECT kingres.switchover_replayed('$mark')"), 't',
		"[$prefix] the replayed-up-to-the-mark answer is still true: it says nothing about rows after the mark");
	judge($prefix, "immediate stop, NEW behind", $new, $old, 103, 'KS006', no_poll => 1);
	is($new->safe_psql('postgres', 'SELECT count(*) FROM public.t1'), '100', "[$prefix] the three acknowledged rows are indeed missing");
}

# --- 4. an immediate stop while a checkpoint runs (the power loss) ------------------------------
{
	my ($prefix, $old, $new) = setup(1);
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 20000)');
	my $h = IPC::Run::start([ 'psql', '-X', '-At', '-h', $old->host, '-p', $old->port, '-d', 'postgres', '-c', 'CHECKPOINT' ],
		'>', \my $o, '2>', \my $e);
	$old->stop('immediate');
	$h->finish;
	judge($prefix, "immediate stop during a checkpoint", $new, $old, 20000, 'KS006', no_poll => 1);
}

# --- 5. a clean stop while the standby is disconnected: false, and rows are missing -------------
{
	my ($prefix, $old, $new) = setup(1);
	disconnect($new);
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 103)');
	$old->stop('fast');
	my ($ans, $rows) = judge($prefix, "fast stop, standby disconnected", $new, $old, 103, 'f');
	is($rows, 100, "[$prefix] the rows written after the disconnection are missing, and the answer said so");
}

# --- 6. the same with the synchronous list emptied (the writes were acknowledged without NEW) ----
{
	my ($prefix, $old, $new) = setup(1);
	$old->append_conf('postgresql.conf', "synchronous_standby_names = 'ANY 1 (" . $new->name . ")'\n");
	$old->reload;
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 110)');
	disconnect($new);
	$old->append_conf('postgresql.conf', "synchronous_standby_names = ''\n");
	$old->reload;
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(111, 120)');
	$old->stop('fast');
	my ($ans, $rows) = judge($prefix, "fast stop, synchronous list emptied", $new, $old, 120, 'f');
	is($rows, 110, "[$prefix] the rows written after the list was emptied are missing, and the answer said so");
}

# --- 7. two standbys: the one that is behind is not proven, the other is ----------------------
{
	my ($prefix, $old, $sa, $sb) = setup(2);
	disconnect($sb);
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 130)');
	wait_catchup($old, $sa);
	$old->stop('fast');
	my ($state, $x) = controldata($old);
	is(verify($sb, $x, $state), 'f', "[$prefix] the standby that was disconnected is not proven");
	judge($prefix, "the standby that kept streaming", $sa, $old, 130, 't');
	promote_standby($sb);
	is($sb->safe_psql('postgres', 'SELECT count(*) FROM public.t1'), '100', "[$prefix] the lagging standby indeed lacks the rows");
	is(verify($sb, $x, $state), 'f', "[$prefix] and still says no after it was promoted");
}

# --- 8. a cascading standby ------------------------------------------------------------------
{
	my ($prefix, $old, $sa) = setup(1);
	my $c = make_standby($sa, "${prefix}_c");
	wait_catchup($sa, $c);
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 130)');
	wait_catchup($old, $sa);
	wait_catchup($sa, $c);
	$old->stop('fast');
	my ($state, $x) = controldata($old);
	ok($c->poll_query_until('postgres', "SELECT kingres.switchover_verify('$x', '$state')"),
		"[$prefix] the cascading standby that has the shutdown checkpoint is proven");
	disconnect($c);
	my $c2 = $c;
	is(verify($c2, $x, $state), 't', "[$prefix] and stays proven when its upstream goes away");
}
{
	my ($prefix, $old, $sa) = setup(1);
	my $c = make_standby($sa, "${prefix}_c");
	wait_catchup($sa, $c);
	disconnect($c);
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 130)');
	wait_catchup($old, $sa);
	$old->stop('fast');
	my ($state, $x) = controldata($old);
	is(verify($sa, $x, $state), 't', "[$prefix] the first standby is proven");
	is(verify($c, $x, $state), 'f', "[$prefix] the cascading standby that fell behind is not");
}

# --- 9. positions and states that are not what the old primary wrote ----------------------------
{
	my ($prefix, $old, $new) = setup(1);
	my $other = PostgreSQL::Test::Cluster->new("${prefix}_other");
	$other->init;
	$other->start;
	$other->safe_psql('postgres', 'CREATE TABLE o AS SELECT generate_series(1, 5000) a');
	$other->stop('fast');
	my (undef, $ox) = controldata($other);
	$old->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 150)');
	$old->safe_psql('postgres', 'CHECKPOINT');
	my (undef, $ordinary) = controldata($old);
	$old->stop('fast');
	my ($state, $x) = controldata($old);
	wait_catchup_stopped($new, $x, $state);
	is(verify($new, $x, $state), 't', "[$prefix] the real shutdown checkpoint is proven");
	is(verify($new, $x, 'in production'), 'KS006', "[$prefix] any state but \"shut down\" is refused");
	is(verify($new, $x, 'shut down in recovery'), 'KS006', "[$prefix] \"shut down in recovery\" is refused too");
	is(verify($new, $x, undef), 'KS006', "[$prefix] a missing state is refused");
	is(verify($new, $ordinary, 'shut down'), 'KS006', "[$prefix] an ordinary checkpoint of OLD (the review's X) is refused although the caller claims shut down");
	is(verify($new, '0/1', 'shut down'), 'KS006', "[$prefix] a position that starts no record is refused");
	is(verify($new, '0/0', 'shut down'), 'KS006', "[$prefix] the invalid position is refused");
	isnt(verify($new, $ox, 'shut down'), 't', "[$prefix] the shutdown checkpoint of another node is never proven");
	isnt(verify($new, '7FFFFFFF/FFFFFFF0', 'shut down'), 't', "[$prefix] a position far ahead is not proven");
	# another timeline: after the promotion NEW writes on timeline 2; a position of that timeline
	# (its end-of-recovery checkpoint) is not OLD's shutdown checkpoint
	promote_standby($new);
	$new->safe_psql('postgres', 'CHECKPOINT');
	my (undef, $newx) = controldata_running($new);
	isnt(verify($new, $newx, 'shut down'), 't', "[$prefix] a position of the new timeline is not proven");
	is(verify($new, $x, $state), 't', "[$prefix] while the real one still is, after the promotion");
}

# --- 10. the way back (sequence 7): P hands over to N and takes the place back, both proven -----
{
	my ($prefix, $p, $n) = setup(1);
	$p->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(101, 150)');
	$p->stop('fast');
	my ($state, $x) = controldata($p);
	ok($n->poll_query_until('postgres', "SELECT kingres.switchover_verify('$x', '$state')"), "[$prefix] first switchover P to N is proven");
	promote_standby($n);
	$n->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(151, 200)');
	# the preferred primary comes back as a standby of N (a fresh copy: same result as pg_rewind)
	my $p2 = make_standby($n, "${prefix}_p2");
	wait_catchup($n, $p2);
	$n->safe_psql('postgres', 'INSERT INTO public.t1 SELECT generate_series(201, 230)');
	wait_catchup($n, $p2);
	$n->stop('fast');
	($state, $x) = controldata($n);
	ok($p2->poll_query_until('postgres', "SELECT kingres.switchover_verify('$x', '$state')"), "[$prefix] the switchover back is proven");
	promote_standby($p2);
	is($p2->safe_psql('postgres', 'SELECT count(*) FROM public.t1'), '230', "[$prefix] 230 rows after the way back");
	is($p2->safe_psql('postgres', "SELECT kingres.switchover_verify('$x', '$state')"), 't', "[$prefix] the proof holds on the promoted preferred primary (timeline 3)");
}

# --- 11. who may call them --------------------------------------------------------------------
{
	my ($prefix, $old, $new) = setup(1);
	$old->safe_psql('postgres', 'CREATE ROLE bob LOGIN');
	wait_catchup($old, $new);
	my ($rc, $out, $err) = run_sql($new, "SET ROLE bob; SELECT kingres.switchover_verify('0/1', 'shut down')");
	like($err, qr/permission denied/, "[$prefix] an ordinary role cannot call the proof");
	($rc, $out, $err) = run_sql($new, "SET ROLE bob; SELECT kingres.switchover_replayed('0/1')");
	like($err, qr/permission denied/, "[$prefix] nor the replayed check");
}

sub controldata_running
{
	my ($node) = @_;
	my $x = $node->safe_psql('postgres', 'SELECT checkpoint_lsn FROM pg_control_checkpoint()');
	return ('in production', $x);
}

# Wait until NEW has replayed what a stopped OLD wrote: the proof says true (the caller polls the same way).
sub wait_catchup_stopped
{
	my ($new, $x, $state) = @_;
	$new->poll_query_until('postgres', "SELECT kingres.switchover_verify('$x', '$state')");
}

done_testing();
