# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/107_s13_fixes.pl
#
# The fifth review of the call sequences (S13):
#   S13-01  the statements that end the work of a node session wait at most node_timeout, whatever the control
#           depth is: RELEASE SAVEPOINT after a call that is inside a savepoint or an exception block; the same
#           for the RELEASE of a session that carried only the statements of a user, while the COMMIT of such a
#           session keeps waiting as long as the node takes.  (The COMMIT TRANSACTION of a session that carried the
#           work of a call and only read is bounded too, but no call of the extension makes such a session: its
#           calls write on the origin, so their remote writers are prepared, not committed.  Nothing here reaches it.)
#   S13-02  kingres_shard.control_call_timeout (KS015): a bound for the whole of a call, opt in.
#   S13-06  the warning of remove_node for a node it let go says that the node still holds the secret.
#   N1      an error that names a node registered by a socket directory names its id and its address.
#   by reading in the review: the text of the stuck nodes of a call is freed with the call.
# The bounds are the README's: a call ends within 2 x node_timeout + 8 s plus node_connect_timeout.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KsProxy;
use KsSweep qw(text_chunks settle);
use KingresShardTest qw(make_node run_sql keys_of_group);

my $T = 2;						# node_timeout
my $C = 2;						# node_connect_timeout
my $BOUND = 2 * $T + 8 + $C;	# the README's bound for a node that stops answering
my $conf = "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = $C\n";
my $tmp = $PostgreSQL::Test::Utils::tmp_check;

# MAP, a node registered by its socket directory, and a node behind a forwarder
my $n1 = make_node('s13_1', extra => $conf);
my $n2 = make_node('s13_2', extra => $conf);
my $n3 = make_node('s13_3', tcp => 1, extra => $conf);
$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres', sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('%s', %d)", $n2->host, $n2->port));
my $proxy = KsProxy->new(backend_port => $n3->port, host => '127.0.0.1', ctl => "$tmp/proxy_s13_3");
my $pport = $proxy->port;
$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d)", $pport));
$n1->safe_psql('postgres', q{
	CREATE TABLE public.s13_t (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.s13_t', 'id', bucket_count => 6);
});
$n1->safe_psql('postgres', join('', map { "INSERT INTO public.s13_t VALUES ($_, 'x');
" } 1 .. 60));
my @nodes = ($n1, $n2, $n3);
my $node3 = qr/node \d+ \(127\.0\.0\.1:$pport\)/;

sub call
{
	my ($sql, %extra) = @_;
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($n1, "\\set VERBOSITY verbose\n$sql", timeout => 120, %extra);
	return (time - $t0, $rc, $out, $err);
}

sub prepared_total
{
	my $t = 0;
	$t += $_->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts') for @nodes;
	return $t;
}

sub open_proxy
{
	$proxy->set(swallow_after => '', delay => 0, delay_turn => 0);
}

# The chunks of the sessions to the node behind the forwarder that carry a statement, for a clean run of $sql.
sub chunks_of
{
	my ($sql, $re) = @_;
	my $tlog = "$tmp/s13_text_$$";
	unlink $tlog;
	open_proxy();
	$proxy->set(text_log => $tlog);
	my ($t, $rc, $out, $err) = call($sql);
	$proxy->set(text_log => '');
	is($rc, 0, "the clean run succeeds: " . ($sql =~ s/\n/ /gr) . " ($err)");
	my @c = text_chunks($tlog, $re);
	ok(scalar(@c), "and sends a statement that matches $re to the node (chunks @c)");
	return @c;
}

# The call goes silent at the statement that the chunk $c carries (swallowed from $c on): it must end within the README's
# bound with an error that names the node, another call that changes the catalog must not be refused, and one recovery
# pass leaves nothing prepared.
sub silent_at
{
	my ($label, $sql, $c, %opt) = @_;
	open_proxy();
	$proxy->set(swallow_after => $c - 1);
	my ($t, $rc, $out, $err) = call($sql);
	open_proxy();
	settle({ nodes => \@nodes });
	isnt($rc, 0, "$label: the statement that is not answered ends the call with an error") or diag($out);
	cmp_ok($t, '<=', $BOUND, sprintf("$label: and within the bound (%.1f s, bound %d s)", $t, $BOUND));
	like($err, qr/KS007/, "$label: with KS007");
	like($err, $node3, "$label: and the error names the node, its id and its address");
	unlike($err, qr/KS008/, "$label: it is not a refusal of another call");
	my ($t2, $rc2, $out2, $err2) = call('SELECT kingres.sync_catalog()');
	is($rc2, 0, "$label: another call that changes the catalog is not refused with KS008 afterwards ($err2)");
	my ($t3, $rc3, $out3, $err3) = call('SELECT kingres.recover_prepared_transactions()');
	is($rc3, 0, "$label: one recovery pass runs ($err3)");
	is(prepared_total(), 0, "$label: nothing is left prepared");
	return $err;
}

# --- S13-01: RELEASE SAVEPOINT of a session that carried the work of a call ----------------------------------------------
{
	my $sql = "BEGIN;\nSAVEPOINT a;\nSELECT kingres.sync_catalog();\nRELEASE a;\nCOMMIT;";
	my @c = chunks_of($sql, qr/RELEASE SAVEPOINT/);
	silent_at('sync_catalog inside a savepoint, silent at RELEASE SAVEPOINT', $sql, $c[0]);
}
{
	my $sql = q{DO $ks$ BEGIN BEGIN PERFORM kingres.sync_catalog(); EXCEPTION WHEN OTHERS THEN RAISE; END; END $ks$};
	my @c = chunks_of($sql, qr/RELEASE SAVEPOINT/);
	silent_at('sync_catalog in a PL/pgSQL block with an EXCEPTION clause, silent at RELEASE SAVEPOINT', $sql, $c[0]);
}

{
	# ROLLBACK TO SAVEPOINT of the subtransaction that the call ran in (it aborts): an abort cannot raise an error, so
	# a WARNING names the node, the session is closed, and the COMMIT of the transaction fails with an error that names it
	my $sql = "BEGIN;\nSAVEPOINT a;\nSELECT kingres.sync_catalog();\nROLLBACK TO a;\nCOMMIT;";
	my @c = chunks_of($sql, qr/ROLLBACK TO SAVEPOINT/);
	open_proxy();
	$proxy->set(swallow_after => $c[0] - 1);
	my ($t, $rc, $out, $err) = call($sql);
	open_proxy();
	settle({ nodes => \@nodes });
	cmp_ok($t, '<=', $BOUND, sprintf('ROLLBACK TO SAVEPOINT not answered: the transaction ends within the bound (%.1f s, bound %d s)', $t, $BOUND));
	like($err, qr/WARNING:  KS007: node \d+ \(127\.0\.0\.1:$pport\) did not complete ROLLBACK TO SAVEPOINT within \d+ seconds; its session was closed/, 'a WARNING names the node and says what was not answered');
	isnt($rc, 0, 'the transaction cannot be committed');
	like($err, qr/ERROR:  08006: node \d+ \(127\.0\.0\.1:$pport\) did not complete ROLLBACK TO SAVEPOINT, and its session was closed/, 'the COMMIT fails with an error that names the node');
	my ($t2, $rc2, $out2, $err2) = call('SELECT kingres.sync_catalog()');
	is($rc2, 0, "another call that changes the catalog is not refused with KS008 afterwards ($err2)");
	my ($t3, $rc3, $out3, $err3) = call('SELECT kingres.recover_prepared_transactions()');
	is($rc3, 0, "one recovery pass runs ($err3)");
	is(prepared_total(), 0, 'nothing is left prepared');
}
# --- S13-01, a session that carried only the statements of a user ------------------------------------------------------
{
	my $sql = "BEGIN;\nSAVEPOINT a;\nSELECT count(*) FROM public.s13_t;\nRELEASE a;\nCOMMIT;";
	my @c = chunks_of($sql, qr/RELEASE SAVEPOINT/);
	silent_at('a read of a user inside a savepoint, silent at RELEASE SAVEPOINT', $sql, $c[0]);
}
{
	# the COMMIT of a session that carried only user work waits as long as the node takes (it may be a deferred
	# constraint): it is not bounded by node_timeout, and nothing in the extension ends it
	my $sql = "BEGIN;\nSELECT count(*) FROM public.s13_t;\nCOMMIT;";
	my @c = chunks_of($sql, qr/COMMIT TRANSACTION/);
	open_proxy();
	$proxy->set(swallow_after => $c[0] - 1);
	my $timed_out = 0;
	my $t0 = time;
	my ($rc, $out, $err) = run_sql($n1, $sql, timeout => 3 * $BOUND, timed_out => \$timed_out);
	my $t = time - $t0;
	ok($timed_out, sprintf('a COMMIT of a session of a user that is not answered is still waiting after %.0f s (3 x the bound): the rule of user work is kept', $t));
	open_proxy();
	$n1->safe_psql('postgres', q{SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'COMMIT%' AND pid <> pg_backend_pid()});
	settle({ nodes => \@nodes });
	ok($n1->poll_query_until('postgres', q{SELECT count(*) = 0 FROM pg_stat_activity WHERE query LIKE 'COMMIT%' AND state = 'active'}), 'the wait ended when the backend was ended from another session');
	my ($t2, $rc2, $out2, $err2) = call('SELECT kingres.sync_catalog()');
	is($rc2, 0, "another call is not refused afterwards ($err2)");
}

# --- S13-02: the bound for the whole of a call ------------------------------------------------------------------------------
{
	# a node whose every answer takes 0.9 x node_timeout: the call succeeds, late, as the README says
	my $T3 = 3;
	my $secret = sub { my ($n) = @_; return $n->safe_psql('postgres', q{SELECT encode(sha256(secret), 'hex') FROM kingres.cluster_secret}); };
	my $same = sub { my %h = map { $secret->($_) => 1 } @nodes; return keys(%h) == 1; };
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	my $before = $secret->($n1);
	my $tl = "$tmp/s13_turns_$$";
	unlink $tl;
	$proxy->set(delay_turn => 0.9 * $T3, skip_turns => 2, turn_log => $tl);
	my ($t, $rc, $out, $err) = call("SET kingres_shard.node_timeout = $T3;\nSELECT kingres.rotate_secret()");
	my $s = 0;
	if (open(my $h, '<', $tl)) { $s++ while <$h>; close $h; }
	diag(sprintf('rotate_secret to a node that answers at 0.9 x node_timeout: %d statements, %.1f s', $s, $t));
	is($rc, 0, "without the setting the call succeeds, late ($err)");
	unlike($err, qr/KS007|KS015|WARNING/, 'and says nothing');
	isnt($secret->($n1), $before, 'the secret was rotated');
	ok($same->(), 'on every node');
	cmp_ok($t, '>', 3 * $T3, 'it took longer than a few node_timeouts, which is what the bound per statement allows');

	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	$before = $secret->($n1);
	my $limit = 2 * $T3;
	($t, $rc, $out, $err) = call("SET kingres_shard.node_timeout = $T3;\nSET kingres_shard.control_call_timeout = $limit;\nSELECT kingres.rotate_secret()");
	isnt($rc, 0, "with control_call_timeout = $limit the same call fails");
	like($err, qr/KS015/, 'with KS015');
	like($err, qr/took longer than kingres_shard.control_call_timeout \(\Q$limit\E seconds\) as a whole/, 'whose text says that the call took too long as a whole');
	unlike($err, qr/KS007|did not answer within/, 'and does not say that a node did not answer');
	like($err, qr/Do not fence a node/, 'and says not to fence one');
	# the README: the limit plus the cancel of one statement (min(node_connect_timeout, T / 3), at least 1 s)
	cmp_ok($t, '<=', $limit + 1 + 2, sprintf('it ended within the limit plus one cancel (%.1f s, limit %d s)', $t, $limit));
	$proxy->set(delay_turn => 0, turn_log => '');
	settle({ nodes => \@nodes });
	is($secret->($n1), $before, 'the state is the one of a failed call: the secret of MAP is unchanged');
	ok($same->(), 'and every node holds it');
	my ($t3, $rc3, $out3, $err3) = call('SELECT kingres.recover_prepared_transactions()');
	is($rc3, 0, "one recovery pass runs ($err3)");
	is(prepared_total(), 0, 'and nothing is left prepared');
	($t, $rc, $out, $err) = call('SELECT kingres.rotate_secret()');
	is($rc, 0, "a repeat without the setting succeeds ($err)");
	isnt($secret->($n1), $before, 'and the secret is rotated');
	ok($same->(), 'on every node');

	# the limit is checked between statements as well: a limit that is used up before the first statement
	($t, $rc, $out, $err) = call("SET kingres_shard.control_call_timeout = 1;\nSELECT pg_sleep(1.2), kingres.sync_catalog()");
	is($rc, 0, "a call that has used its time before it starts is not ended by the limit of a call that did not start yet ($err)");
	unlike($err, qr/KS015/, 'and says nothing of the limit');
	my ($t4, $rc4, $out4, $err4) = call('SELECT kingres.sync_catalog()');
	is($rc4, 0, "and the next call without the setting works ($err4)");
}

# --- S13-06: the warning of remove_node for a node it let go ------------------------------------------------------------
{
	my $n4 = make_node('s13_4', tcp => 1, extra => $conf);
	my $p4 = KsProxy->new(backend_port => $n4->port, host => '127.0.0.1', ctl => "$tmp/proxy_s13_4");
	my ($t, $rc, $out, $err) = call(sprintf("SELECT kingres.add_node('127.0.0.1', %d)", $p4->port));
	is($rc, 0, "a fourth node is added ($err)");
	$p4->set(swallow_all => 1);
	($t, $rc, $out, $err) = call(sprintf("SELECT kingres.remove_node((SELECT node_id FROM kingres.node WHERE host = '127.0.0.1' AND port = %d))", $p4->port));
	is($rc, 0, "remove_node of a node that accepts the session and says nothing works ($err)");
	like($err, qr/WARNING.*could not reach node \d+ \(127\.0\.0\.1:\Q@{[ $p4->port ]}\E\)/s, 'with a warning that names the node, its id and its address');
	like($err, qr/still holds its catalog and the cluster secret/, 'that says the node still holds the cluster secret');
	like($err, qr/kingres\.rotate_secret\(\)/, 'and that rotate_secret() shuts it out');
	$p4->stop;
	$n4->stop('immediate', fail_ok => 1);
	$n1->safe_psql('postgres', 'DELETE FROM kingres.pending_node');
}

# --- N1: a node registered by a socket directory, in an error ---------------------------------------------------------------
{
	my $id2 = $n1->safe_psql('postgres', sprintf("SELECT node_id FROM kingres.node WHERE port = %d", $n2->port));
	my @k0 = keys_of_group($n1, 1, 0, 1);
	my @k1 = keys_of_group($n1, 1, 1, 1, 1000);
	$n2->safe_psql('postgres', q{
		SET kingres_shard.enable_routing = off;
		CREATE FUNCTION public.s13_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'deferred check failed'; END $$;
		DO $d$ DECLARE r record; BEGIN
		  FOR r IN SELECT inhrelid::regclass AS p FROM pg_inherits WHERE inhparent = 'public.s13_t'::regclass LOOP
		    EXECUTE format('CREATE CONSTRAINT TRIGGER s13_fail AFTER INSERT ON %s DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.s13_fail()', r.p);
		  END LOOP; END $d$;
	});
	my ($t, $rc, $out, $err) = call("BEGIN;\nINSERT INTO public.s13_t VALUES ($k0[0] + 100000, 'a');\nINSERT INTO public.s13_t VALUES ($k1[0] + 100000, 'b');\nCOMMIT;");
	isnt($rc, 0, 'a transaction whose prepare fails on the node registered by a socket directory fails');
	my $h = $n2->host;
	my $p = $n2->port;
	like($err, qr/could not prepare the transaction on node \Q$id2\E \(\Q$h:$p\E\):/, 'and the error names the node by its id and its address as registered');
	unlike($err, qr/on node \Q$h:$p\E:/, 'not by the socket directory alone');
	$n2->safe_psql('postgres', q{
		SET kingres_shard.enable_routing = off;
		DO $d$ DECLARE r record; BEGIN
		  FOR r IN SELECT inhrelid::regclass AS p FROM pg_inherits WHERE inhparent = 'public.s13_t'::regclass LOOP
		    EXECUTE format('DROP TRIGGER s13_fail ON %s', r.p);
		  END LOOP; END $d$;
	});
	is(prepared_total(), 0, 'nothing is left prepared');
}

# --- the text of a stuck node is freed with the call (a few dozen bytes per event) ---------------------------------------
{
	# every call finds the node behind the forwarder stuck once (it accepts the session and says nothing); the memory
	# of TopMemoryContext in use must not grow with the number of calls
	$proxy->set(swallow_all => 1);
	my $loop = sub {
		my ($n) = @_;
		return q{SET kingres_shard.node_timeout = 1;
			DO $m$ DECLARE a bigint; b bigint; BEGIN
			  FOR i IN 1..} . $n . q{ LOOP BEGIN PERFORM kingres.sync_catalog(); EXCEPTION WHEN OTHERS THEN NULL; END; END LOOP;
			  SELECT used_bytes INTO a FROM pg_backend_memory_contexts WHERE name = 'TopMemoryContext';
			  FOR i IN 1..} . $n . q{ LOOP BEGIN PERFORM kingres.sync_catalog(); EXCEPTION WHEN OTHERS THEN NULL; END; END LOOP;
			  SELECT used_bytes INTO b FROM pg_backend_memory_contexts WHERE name = 'TopMemoryContext';
			  RAISE NOTICE 'growth %', b - a;
			END $m$};
	};
	my $events = $ENV{KINGRES_LONG_TESTS} ? 80 : 40;
	my ($rc, $out, $err) = run_sql($n1, $loop->($events), timeout => 600);
	is($rc, 0, "$events + $events calls that find a node stuck run ($err)");
	my ($growth) = ($err =~ /growth (-?\d+)/);
	ok(defined $growth, 'the growth of TopMemoryContext was read');
	# a leaked text is at least 64 bytes: $events events would leave at least 64 x $events bytes behind
	cmp_ok($growth // 1e9, '<', 32 * $events, "the memory in use does not grow with the events: $events more events grew it by " . ($growth // '?') . ' bytes');
	open_proxy();
}

$proxy->stop;
done_testing();
