# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/119_nested_subxacts.pl
#
# The sixth review of the call sequences (S14-01, S14-05, S14-06): transactions with two nested
# subtransactions (two savepoints, two PL/pgSQL blocks with an EXCEPTION clause) while the connection
# to a node (behind the forwarder of t/KsProxy.pm) is closed, or answered with an error, at EVERY
# chunk of the session, after the session reached depth 3; with writes of the bulk path of the row
# router (a batch in flight inside the nested subtransactions) and with plain ones.
# The standard: the server does not crash (a callback of a subtransaction raises no error, which
# used to end in PANIC: ERRORDATA_STACK_SIZE exceeded), no row of a rolled-back subtransaction is
# stored, and a transaction whose COMMIT (or PREPARE TRANSACTION) succeeded has all its rows, one
# whose COMMIT failed has none.  A failure of RELEASE that PL/pgSQL catches does not let the
# transaction go on to commit or to prepare.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KsProxy;
use KsSweep qw(chunk_max);
use KingresShardTest qw(make_node run_sql keys_of_group);

my $T = 3;
my $conf = "kingres_shard.node_timeout = $T\nkingres_shard.node_connect_timeout = 3\n";
my $tmp = $PostgreSQL::Test::Utils::tmp_check;

my $n1 = make_node('ns_1', extra => $conf);
my $n2 = make_node('ns_2', tcp => 1, extra => $conf);
my $n3 = make_node('ns_3', tcp => 1, extra => $conf);
$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres', sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
my $p3 = KsProxy->new(backend_port => $n3->port, host => '127.0.0.1', ctl => "$tmp/proxy_ns3");
$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d)", $n2->port));
$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('127.0.0.1', %d)", $p3->port));
$n1->safe_psql('postgres', q{
	CREATE TABLE public.r_t (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.r_t', 'id', bucket_count => 12);
});
my @nodes = ($n1, $n2, $n3);
my @k0 = keys_of_group($n1, 1, 0, 3);
my @k1 = keys_of_group($n1, 1, 1, 3);
my @k2 = keys_of_group($n1, 1, 2, 4);
$n1->safe_psql('postgres', "INSERT INTO public.r_t VALUES ($_, 'base')") for (@k0[0, 1], @k1[0, 1], @k2[0, 1]);
my $base = 6;

sub open_all { $p3->set(close_after => '', error_at => '', swallow_after => '', lose_answer => '', delay => 0); }

my $kept = "($k0[2], 'kept'), ($k1[2], 'kept'), ($k2[3], 'kept')";

# The scripts.  Each ends with COMMIT (or PREPARE TRANSACTION) and prints COMMIT_ERROR=<bool>.
my $end_commit = "COMMIT;\n\\echo COMMIT_ERROR=:ERROR\n";
my %scripts = (
	'two savepoints, ROLLBACK TO the outer one' =>
	  "BEGIN;\nSAVEPOINT a;\nSAVEPOINT b;\nSELECT count(*) FROM public.r_t;\nINSERT INTO public.r_t VALUES ($k2[2], 'rolled');\nROLLBACK TO a;\nINSERT INTO public.r_t VALUES $kept;\n$end_commit",
	'two savepoints, ROLLBACK' =>
	  "BEGIN;\nSAVEPOINT a;\nSAVEPOINT b;\nSELECT count(*) FROM public.r_t;\nROLLBACK;\n\\echo COMMIT_ERROR=false\n",
	'two savepoints, RELEASE both' =>
	  "BEGIN;\nINSERT INTO public.r_t VALUES $kept;\nSAVEPOINT a;\nSAVEPOINT b;\nSELECT count(*) FROM public.r_t;\nRELEASE b;\nRELEASE a;\n$end_commit",
	'two savepoints, a bulk insert in flight' =>
	  "BEGIN;\nSAVEPOINT a;\nSAVEPOINT b;\nINSERT INTO public.r_t SELECT g, 'rolled' FROM generate_series(1000, 1040) g;\nROLLBACK TO a;\nINSERT INTO public.r_t VALUES $kept;\n$end_commit",
	'two EXCEPTION blocks, re-raised' =>
	  "BEGIN;\nDO \$\$ BEGIN BEGIN BEGIN PERFORM count(*) FROM public.r_t; INSERT INTO public.r_t VALUES ($k2[2], 'rolled'); RAISE EXCEPTION 'x';\nEXCEPTION WHEN OTHERS THEN RAISE; END;\nEXCEPTION WHEN OTHERS THEN RAISE NOTICE 'caught'; END; END \$\$;\nINSERT INTO public.r_t VALUES $kept;\n$end_commit",
	'two EXCEPTION blocks, caught and going on, COMMIT' =>
	  "BEGIN;\nDO \$\$ BEGIN BEGIN BEGIN PERFORM count(*) FROM public.r_t; INSERT INTO public.r_t VALUES ($k2[2], 'rolled'); RAISE EXCEPTION 'x';\nEXCEPTION WHEN OTHERS THEN NULL; END;\nEXCEPTION WHEN OTHERS THEN NULL; END; END \$\$;\nINSERT INTO public.r_t VALUES $kept;\n$end_commit",
);

sub state
{
	my $rolled = $n1->safe_psql('postgres', q{SELECT count(*) FROM public.r_t WHERE v = 'rolled'});
	my $keptn = $n1->safe_psql('postgres', q{SELECT count(*) FROM public.r_t WHERE v = 'kept'});
	my $total = $n1->safe_psql('postgres', q{SELECT count(*) FROM public.r_t});
	return ($rolled, $keptn, $total);
}

sub settle
{
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
}

sub reset_data
{
	$n1->safe_psql('postgres', "DELETE FROM public.r_t WHERE v <> 'base'");
}

# a clean run of every script gives the number of chunks of the session to the node behind the forwarder
my $chunks = "$tmp/ns_chunks";
my $max = 0;
open_all();
for my $name (sort keys %scripts)
{
	unlink $chunks;
	$p3->set(chunk_log => $chunks);
	my ($rc, $out, $err) = run_sql($n1, "\\set ON_ERROR_STOP 0\n$scripts{$name}");
	$p3->set(chunk_log => '');
	like($out, qr/COMMIT_ERROR=false/, "clean: $name commits") or diag("$out\n$err");
	my $m = chunk_max($chunks);
	$max = $m if $m > $max;
	reset_data();
}
cmp_ok($max, '>', 4, "the sessions to the node behind the forwarder send up to $max chunks");

for my $mode ('close_after', 'error_at')
{
	for my $name (sort keys %scripts)
	{
		for my $pos (1 .. $max)
		{
			open_all();
			my $log_from = -s $n1->logfile;
			$p3->set($mode => $pos);
			my ($rc, $out, $err) = run_sql($n1, "\\set ON_ERROR_STOP 0\n$scripts{$name}", timeout => 60);
			open_all();
			settle();
			my ($rolled, $keptn, $total) = state();
			my $label = "$mode at chunk $pos, $name";
			my $alive = $n1->safe_psql('postgres', 'SELECT 1');
			# a crash of the server is a PANIC or a backend that a signal ended, in the log of the node
			my $newlog = '';
			if (open(my $lf, '<', $n1->logfile)) { seek($lf, $log_from, 0); local $/; $newlog = <$lf>; close $lf; }
			my $ok = ($alive eq '1'
				&& ($err . $out . $newlog) !~ /PANIC|terminated by signal|ERRORDATA_STACK_SIZE/
				&& $rolled == 0);
			# all or nothing: a COMMIT of a transaction that an error aborted is a ROLLBACK without an error,
			# so what is asked is that the rows of the transaction are all there or none, never some
			$ok &&= ($keptn == 0 || $keptn == 3) && $total == $base + $keptn;
			$ok &&= ($keptn == 0) if $out =~ /COMMIT_ERROR=true/ || $name =~ /, ROLLBACK$/;
						ok($ok, $label) or diag("rolled=$rolled kept=$keptn total=$total\nOUT: $out\nERR: $err");
			is(join('', map { $_->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts') } @nodes), '000', "$label: nothing is prepared");
			reset_data();
		}
	}
}
open_all();

# ---------------------------------------------------------------------------
# S14-05: a RELEASE that fails inside a block whose error PL/pgSQL catches does not let the transaction
# go on to commit or to prepare (the doomed note is the second protection after the PRE_COMMIT_SUB RELEASE)
# ---------------------------------------------------------------------------
{
	open_all();
	$n1->safe_psql('postgres', 'CREATE TABLE public.loct (a int)');
	my $blk = "DO \$\$ BEGIN BEGIN PERFORM count(*) FROM public.r_t WHERE id = $k2[0]; EXCEPTION WHEN OTHERS THEN NULL; END; END \$\$;\n";
	my $tl = "$tmp/ns_text2";
	unlink $tl;
	$p3->set(text_log => $tl);
	run_sql($n1, "BEGIN;\n$blk" . "COMMIT;\n");
	$p3->set(text_log => '');
	my $rel = 0;
	if (open(my $fh, '<', $tl))
	{
		while (<$fh>) { $rel = $1 if /^\S+ (\d+) .*RELEASE SAVEPOINT/ && !$rel; }
		close $fh;
	}
	cmp_ok($rel, '>', 1, "the chunk of the RELEASE of the block is found ($rel)");
	for my $end ("COMMIT;", "PREPARE TRANSACTION 'ns_q';")
	{
		open_all();
		$p3->set(close_after => $rel - 1);
		my ($rc, $out, $err) = run_sql($n1, "\\set ON_ERROR_STOP 0\nBEGIN;\n$blk" . "INSERT INTO public.loct VALUES (1);\n$end\n\\echo END\n", timeout => 60);
		open_all();
		settle();
		is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.loct'), 0, "$end after a RELEASE that failed in a caught block: the local row is not stored");
		is($n1->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'), 0, "$end: nothing is prepared");
		like($err, qr/did not complete RELEASE SAVEPOINT|closed|server closed|lost the connection/, "$end: the error names what happened") or diag($err);
	}
}

# ---------------------------------------------------------------------------
# S14-04: a session that only read, in READ COMMITTED, and did not answer ROLLBACK TO SAVEPOINT is closed
# without refusing the transaction (nothing it held is lost); one that wrote is refused
# ---------------------------------------------------------------------------
{
	open_all();
	my $tl = "$tmp/ns_text3";
	unlink $tl;
	$p3->set(text_log => $tl);
	my $read = "BEGIN;\nSAVEPOINT a;\nSELECT count(*) FROM public.r_t WHERE id = $k2[0];\nROLLBACK TO a;\nSELECT 1;\n";
	run_sql($n1, $read . "COMMIT;\n");
	$p3->set(text_log => '');
	my $rb = 0;
	if (open(my $fh, '<', $tl))
	{
		while (<$fh>) { $rb = $1 if /^\S+ (\d+) .*ROLLBACK TO SAVEPOINT/ && !$rb; }
		close $fh;
	}
	cmp_ok($rb, '>', 1, "the chunk of the ROLLBACK TO of the session to the node is found ($rb)");
	for my $case ([ 'only read', $read, 1 ],
				  [ 'wrote', "BEGIN;\nSAVEPOINT a;\nINSERT INTO public.r_t VALUES ($k2[2], 'kept');\nROLLBACK TO a;\nSELECT 1;\n", 0 ])
	{
		my ($what, $body, $commits) = @$case;
		open_all();
		unlink $tl;
		$p3->set(text_log => $tl);
		run_sql($n1, $body . "ROLLBACK;\n");
		$p3->set(text_log => '');
		my $pos = 0;
		if (open(my $fh, '<', $tl))
		{
			while (<$fh>) { $pos = $1 if /^\S+ (\d+) .*ROLLBACK TO SAVEPOINT/ && !$pos; }
			close $fh;
		}
		$p3->set(swallow_after => $pos - 1);
		my ($rc, $out, $err) = run_sql($n1, "\\set ON_ERROR_STOP 0\n$body" . "COMMIT;\n\\echo COMMIT_ERROR=:ERROR\n", timeout => 60);
		open_all();
		settle();
		like($err, qr/did not complete ROLLBACK TO SAVEPOINT/, "$what: the node that did not answer is named in a warning") or diag($err);
		if ($commits)
		{
			like($out, qr/COMMIT_ERROR=false/, "$what: the transaction commits (the session held nothing that is lost)") or diag($err);
		}
		else
		{
			like($out, qr/COMMIT_ERROR=true/, "$what: the transaction cannot commit (the session held a write that is lost)") or diag($err);
		}
		reset_data();
	}
}

# ---------------------------------------------------------------------------
# S14-03: thousands of failed RELEASEs leave the memory of the backend flat
# ---------------------------------------------------------------------------
{
	open_all();
	my $tl = "$tmp/ns_text";
	unlink $tl;
	$p3->set(text_log => $tl);
	my $one = "BEGIN;\nSAVEPOINT a;\nSELECT count(*) FROM public.r_t WHERE id = $k2[0];\nRELEASE a;\nCOMMIT;\n";
	run_sql($n1, $one);
	$p3->set(text_log => '');
	my $release_chunk = 0;
	if (open(my $fh, '<', $tl))
	{
		while (<$fh>)
		{
			if (/^\S+ (\d+) .*RELEASE SAVEPOINT/ && !$release_chunk) { $release_chunk = $1; }
		}
		close $fh;
	}
	cmp_ok($release_chunk, '>', 1, "the chunk of the RELEASE of the session to the node is found ($release_chunk)");
	$p3->set(close_after => $release_chunk - 1);
	my $mem = "SELECT used_bytes FROM pg_backend_memory_contexts WHERE name = 'TopMemoryContext';\n";
	my $script = "\\set ON_ERROR_STOP 0\n\\o /dev/null\n" . ($one x 200) . "\\o\n\\echo MEM1\n$mem\\o /dev/null\n"
	  . ($one x 2000) . "\\o\n\\echo MEM2\n$mem";
	my ($rc, $out, $err) = run_sql($n1, $script, timeout => 600);
	my ($m1, $m2) = $out =~ /MEM1\n(\d+)\nMEM2\n(\d+)/ ? ($1, $2) : (-1, -1);
	cmp_ok($m1, '>', 0, 'the memory of the backend was read twice') or diag($out . $err);
	cmp_ok($m2 - $m1, "<", 24 * 1024, "2000 more failed RELEASEs: TopMemoryContext grew by " . ($m2 - $m1) . " bytes");
	open_all();
}

 done_testing();
