# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Two-phase commit and its recovery (design/shard-phase2a.md, section 1):
# commits and rollbacks that write on two and three nodes, a prepare failure,
# and every crash window of the protocol, driven with the fault injection
# setting kingres_shard.fault_injection and by stopping nodes.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql prepared_count
  wait_prepared_count);

my ($n1, $n2, $n3) = make_cluster('tp', 3);

$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	CREATE TABLE public.loc (i int);
});
$_->safe_psql('postgres', 'CREATE TABLE IF NOT EXISTS public.loc (i int)') for ($n2, $n3);

# The setup used two-phase commit: clear its decisions.
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');

# Keys owned by each shard group (group g lives on node g + 1).
my %k;
$k{$_} = [ keys_of_group($n1, 1, $_, 20) ] for (0 .. 2);
is(scalar(@{ $k{$_} }), 20, "twenty keys of group $_") for (0 .. 2);
my $next = 0;					# next unused key index of every group
sub fresh
{
	my ($g) = @_;
	return $k{$g}[$next];
}

# A row, read through node 1 (forwarded when it belongs to another group): only
# committed rows are visible.
sub count_of
{
	my ($key) = @_;
	return $n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $key");
}

sub no_prepared
{
	my ($label) = @_;
	is(prepared_count($_), '0', "$label: no prepared transaction on node $_->name")
	  for ($n1, $n2, $n3);
}

# ---------------------------------------------------------------------------
# Commit and rollback.
# ---------------------------------------------------------------------------
{
	my ($a, $b) = (fresh(1), fresh(2));
	my ($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($a, 'a'); INSERT INTO public.kv VALUES ($b, 'b'); COMMIT;");
	is($err, '', 'a transaction writing on two remote nodes commits');
	is(count_of($a), '1', 'the write on node 2 is there');
	is(count_of($b), '1', 'the write on node 3 is there');
	no_prepared('after a two-node commit');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '2',
		'the decision of the commit is logged on the origin');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '0',
		'recovery finds nothing to resolve after a clean commit');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '0',
		'recovery removes the logged decisions of finished transactions');
	$next++;
}
{
	my ($a, $b, $c) = (fresh(0), fresh(1), fresh(2));
	my ($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($a, 'a'); INSERT INTO public.kv VALUES ($b, 'b'); INSERT INTO public.kv VALUES ($c, 'c'); COMMIT;");
	is($err, '', 'a transaction writing locally and on two nodes commits');
	is(count_of($_), '1', "key $_ is there") for ($a, $b, $c);
	no_prepared('after a three-node commit');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	$next++;
}
{
	# The origin is node 2 (group 1): its own group is local.
	my ($a, $b, $c) = (fresh(1), fresh(0), fresh(2));
	my ($rc, $out, $err) = run_sql($n2,
		"BEGIN; INSERT INTO public.kv VALUES ($a, 'a'); INSERT INTO public.kv VALUES ($b, 'b'); INSERT INTO public.kv VALUES ($c, 'c'); COMMIT;");
	is($err, '', 'a transaction that originates on another node commits');
	is(count_of($_), '1', "key $_ is there") for ($a, $b, $c);
	no_prepared('after a commit that originated on node 2');
	$n2->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	$next++;
}
{
	my ($a, $b, $c) = (fresh(0), fresh(1), fresh(2));
	my ($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($a, 'a'); INSERT INTO public.kv VALUES ($b, 'b'); INSERT INTO public.kv VALUES ($c, 'c'); ROLLBACK;");
	is($err, '', 'a rollback after writes on three nodes');
	is(count_of($_), '0', "key $_ is not there") for ($a, $b, $c);
	no_prepared('after a rollback');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '0',
		'a rollback logs nothing');

	# An error after writes rolls everything back too.
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($b, 'b'); INSERT INTO public.kv VALUES ($c, 'c'); SELECT 1/0; COMMIT;");
	like($err, qr/division by zero/, 'an error between the writes and the commit');
	is(count_of($_), '0', "key $_ is not there") for ($b, $c);
	no_prepared('after an error');

	# A single remote writer does not use two-phase commit.
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; SELECT count(*) FROM public.kv WHERE id = $b; INSERT INTO public.kv VALUES ($c, 'c'); COMMIT;");
	is($err, '', 'one remote writer and one remote reader commit');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '0',
		'a single writer does not log a decision (no two-phase commit)');
	is(count_of($c), '1', 'the single writer committed');
	$n1->safe_psql('postgres', "DELETE FROM public.kv WHERE id = $c");
	$next++;
}

# PREPARE TRANSACTION by the user is still refused after remote work.
{
	my ($a) = (fresh(1));
	my ($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($a, 'a'); PREPARE TRANSACTION 'mine';");
	like($err, qr/cannot PREPARE a transaction that has operated on remote nodes/,
		'PREPARE TRANSACTION after remote work is refused');
	is(count_of($a), '0', 'nothing was left by the refused PREPARE TRANSACTION');
	no_prepared('after the refused PREPARE TRANSACTION');
	$next++;
}

# ---------------------------------------------------------------------------
# A prepare that fails on one node rolls back everything: a deferred
# constraint trigger on a partition of node 3 fails when the transaction is
# prepared there.
# ---------------------------------------------------------------------------
{
	my ($b, $c) = (fresh(1), fresh(2));
	my $bucket = $n1->safe_psql('postgres', "SELECT kingres.bucket_for(1, $c)");
	my $part = sprintf('kv_b%04d', $bucket);
	$n3->safe_psql('postgres', qq{
		SET kingres_shard.enable_routing = off;
		CREATE FUNCTION public.fail_at_prepare() RETURNS trigger LANGUAGE plpgsql AS
		\$\$ BEGIN RAISE EXCEPTION 'deferred check failed'; END \$\$;
		CREATE CONSTRAINT TRIGGER fail_at_prepare AFTER INSERT ON public.$part
		  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.fail_at_prepare();
	});
	my ($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($b, 'b'); INSERT INTO public.kv VALUES ($c, 'c'); COMMIT;");
	like($err, qr/could not prepare the transaction on node .*deferred check failed/s,
		'a failing prepare is reported with the remote error');
	is(count_of($b), '0', 'the write on the node that did prepare is rolled back');
	is(count_of($c), '0', 'the write on the node that failed to prepare is gone');
	no_prepared('after a failed prepare');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '0',
		'a failed prepare logs no decision');
	$n3->safe_psql('postgres', "SET kingres_shard.enable_routing = off; DROP TRIGGER fail_at_prepare ON public.$part");
	$next++;
}

# ---------------------------------------------------------------------------
# Recovery: crash windows.  The test cluster does not restart a crashed node,
# so the test starts the origin again.
# ---------------------------------------------------------------------------
sub crash_scenario
{
	my ($point, $keys) = @_;
	my ($a, $b, $c) = @$keys;
	my ($rc, $out, $err) = run_sql($n1,
		"SET kingres_shard.fault_injection = '$point:kill';\n"
		  . "BEGIN; INSERT INTO public.kv VALUES ($a, 'a'); INSERT INTO public.kv VALUES ($b, 'b'); INSERT INTO public.kv VALUES ($c, 'c'); COMMIT;");
	isnt($rc, 0, "the origin dies at $point");
	# The postmaster exited with the crashed backend (restart_after_crash is off).
	$n1->stop("immediate", fail_ok => 1);
	$n1->start;
	return;
}

# After the prepare and before the local commit: roll back everywhere.
{
	my @keys = (fresh(0), fresh(1), fresh(2));
	crash_scenario('after_prepare', \@keys);
	is(prepared_count($n2), '1', 'a transaction is left prepared on node 2');
	is(prepared_count($n3), '1', 'a transaction is left prepared on node 3');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2',
		'recovery resolves both');
	no_prepared('after recovery of a crash before the local commit');
	is(count_of($_), '0', "key $_ was rolled back") for @keys;
	$next++;
}
{
	my @keys = (fresh(0), fresh(1), fresh(2));
	crash_scenario('after_log', \@keys);
	is(prepared_count($n2), '1', 'prepared on node 2 (decision logged but not committed)');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '0',
		'the logged decision of an uncommitted transaction is gone');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2',
		'recovery rolls both back');
	no_prepared('after recovery of a crash after logging');
	is(count_of($_), '0', "key $_ was rolled back") for @keys;
	$next++;
}

# After the local commit and before COMMIT PREPARED: commit everywhere.
{
	my @keys = (fresh(0), fresh(1), fresh(2));
	crash_scenario('after_commit', \@keys);
	is(prepared_count($n2), '1', 'still prepared on node 2');
	is(prepared_count($n3), '1', 'still prepared on node 3');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $keys[0]"), '1',
		'the local part is committed');
	is(count_of($keys[1]), '0', 'the remote part is not visible yet');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '2',
		'the decision survived the crash');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2',
		'recovery commits both');
	no_prepared('after recovery of a crash after the local commit');
	is(count_of($_), '1', "key $_ was committed") for @keys;
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '0',
		'the decisions are removed once nothing is prepared');
	$next++;
}

# Between the two COMMIT PREPAREDs.
{
	my @keys = (fresh(0), fresh(1), fresh(2));
	crash_scenario('between_commit_prepared', \@keys);
	is(prepared_count($n2) + prepared_count($n3), '1',
		'one of the two is committed, the other still prepared');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '1',
		'recovery commits the remaining one');
	no_prepared('after recovery of a crash between the commits');
	is(count_of($_), '1', "key $_ was committed") for @keys;
	$next++;
}

# The origin is stopped (not killed) while the decision is made: the same
# state is found after a clean restart.
{
	my @keys = (fresh(0), fresh(1), fresh(2));
	crash_scenario('after_prepare', \@keys);
	$n1->stop;
	$n1->start;
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2',
		'recovery after a restart of the origin');
	no_prepared('after a restart of the origin');
	is(count_of($_), '0', "key $_ was rolled back") for @keys;
	$next++;
}

# A transaction that is still running on the origin is left alone.
{
	my @keys = (fresh(0), fresh(1), fresh(2));
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_$$";
	append_to_file($gate, 'x');
	my $h = $n1->background_psql('postgres', on_error_stop => 0);
	$h->query_until(qr/started/,
		"SET kingres_shard.fault_injection = 'after_prepare:wait:$gate';\n"
		  . "BEGIN; INSERT INTO public.kv VALUES ($keys[0], 'a'); INSERT INTO public.kv VALUES ($keys[1], 'b'); INSERT INTO public.kv VALUES ($keys[2], 'c');\n"
		  . "\\echo started\nCOMMIT;\n");
	ok(wait_prepared_count($n2, 1), 'the transaction is prepared on node 2 and waiting');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '0',
		'recovery leaves a transaction that is still in progress on the origin alone');
	is(prepared_count($n2), '1', 'it is still prepared');
	unlink $gate;
	$h->query('SELECT 1');
	$h->quit;
	no_prepared('after the waiting transaction finished');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(count_of($_), '1', "key $_ was committed") for @keys;
	$next++;
}

# A target node that is down while COMMIT PREPARED is sent: the origin commits,
# warns, and recovery finishes the job when the node is back.
{
	my @keys = (fresh(0), fresh(1), fresh(2));
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate2_$$";
	append_to_file($gate, 'x');
	my $h = $n1->background_psql('postgres', on_error_stop => 0);
	$h->query_until(qr/started/,
		"SET kingres_shard.fault_injection = 'after_commit:wait:$gate';\n"
		  . "BEGIN; INSERT INTO public.kv VALUES ($keys[0], 'a'); INSERT INTO public.kv VALUES ($keys[1], 'b'); INSERT INTO public.kv VALUES ($keys[2], 'c');\n"
		  . "\\echo started\nCOMMIT;\n");
	ok(wait_prepared_count($n3, 1), 'prepared on node 3, the origin has committed and waits');
	$n3->stop('immediate');
	unlink $gate;
	my ($out, $failed) = $h->query('SELECT 1');
	$h->quit;
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $keys[0]"), '1',
		'the transaction committed on the origin although a target was down');
	is(count_of($keys[1]), '1', 'and on the target that was up');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '0',
		'recovery skips the node that is down');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '1',
		'the decision for the node that is down is kept');
	$n3->start;
	is(prepared_count($n3), '1', 'the transaction is still prepared on node 3 after its restart');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '1',
		'recovery commits it when the node is back');
	no_prepared('after node 3 came back');
	is(count_of($keys[2]), '1', 'the write on node 3 is committed');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.dtx_log'), '0',
		'the decision is removed');
	$next++;
}

# recover_prepared_transactions() is for superusers and runs on the origin only.
{
	$n1->safe_psql('postgres', "CREATE ROLE plain LOGIN");
	my ($rc, $out, $err) = run_sql($n1, "SET ROLE plain; SELECT kingres.recover_prepared_transactions();");
	like($err, qr/permission denied/, 'an ordinary role cannot run recovery');
}

done_testing();
