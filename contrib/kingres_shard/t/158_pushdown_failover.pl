# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/158_pushdown_failover.pl
#
# The design's 064_pushdown_failover (design/shard-phase2b.md, 10.3 and 16.2) for the paths of steps 7 to 10: the partial
# aggregation, the ordered merge with a limit, the rows of a join and an object of an extension.  The closed query has its
# part in t/125 (section E), the router's and the colocated INSERT ... SELECT's in t/116 and t/122 (with the points bulk_*,
# insel_* and fanout_*).  The rule of every scenario: the statement either gives the single-node answer or raises the retryable
# error (KS001 and its relatives), never a partial answer; the same statement repeated succeeds; nothing is left prepared;
# check_cluster() reports no lost part.
#
#   A. a planned switchover (the old primary is alive and goes on running) at each fault point of the fragment scan
#      (frag_after_probe_before, frag_after_declare, frag_after_probe_after, frag_first_fetch), for every path, with a
#      prepared statement that was planned before the change and is executed after it;
#   B. a hard failover (the old primary is killed in the middle of a fetch: frag_wait_batch with small batches) for every path;
#   C. a transaction that wrote through the old primary and then reads through the new one, for every path.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql
  make_standby register_standby wait_catchup set_sync promote_standby release_node);

my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my $IDS = 300;
my $KS = qr/KS001|bucket map|map changed|no longer what it was|failed while a distributed statement|already uses node|partitions of table|no longer the primary/;

my @paths = (
	{ name => 'partial aggregation', plan => qr/Finalize/,
	  sql => q{SELECT g, count(*), sum(w) FROM public.s GROUP BY g ORDER BY g} },
	{ name => 'ordered merge with a limit', plan => qr/Merge keys/,
	  sql => q{SELECT id, count(*) AS cnt FROM public.s GROUP BY id ORDER BY cnt DESC, id DESC LIMIT 9} },
	{ name => 'join rows', plan => qr/Pushed: join rows/,
	  sql => q{SELECT s.id, o.x, row_number() OVER (ORDER BY o.x, s.id, s.c, o.c) FROM public.s JOIN public.o ON o.id = s.id WHERE s.id <= 40} },
	{ name => 'an object of an extension', plan => qr/Finalize/,
	  sql => q{SELECT g, kingres_sketch.approx_count_distinct(w) FROM public.s GROUP BY g ORDER BY g} },
);

# the lines of a result: a command tag (SET) that psql prints for a statement of a script is not a row
sub norm { my ($out) = @_; return join("\n", grep { length && $_ ne 'SET' } split /\n/, $out); }

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# a cluster of two groups, the second with a synchronous standby
sub setup2
{
	my ($prefix) = @_;
	my ($a, $b) = make_cluster($prefix, 2, replication => 1,
		extra => "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_join = on\nkingres_shard.enable_pushdown_sort = on\n");
	$a->safe_psql('postgres', qq{
		CREATE TABLE public.s (id int, c int, w int, g int, PRIMARY KEY (id, c));
		SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12);
		CREATE TABLE public.o (id int, c int, x int, PRIMARY KEY (id, c));
		SELECT kingres.create_distributed_table('public.o', 'id', colocate_with => 'public.s');
		INSERT INTO public.s SELECT i, c, CASE c WHEN 1 THEN 1 ELSE -1 END, i % 7 FROM generate_series(1, $IDS) i, generate_series(1, 2) c;
		INSERT INTO public.o SELECT i, c, (i * c) % 13 FROM generate_series(1, $IDS) i, generate_series(1, 2) c;
	});
	$_->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch') for ($a, $b);
	my $s = make_standby($b, "${prefix}_s2");
	my $id = register_standby($a, $s, 1);
	set_sync($b, $s);
	wait_catchup($b, $s);
	$a->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	$_->safe_psql('postgres', 'ANALYZE') for ($a, $b);
	return ($a, $b, $s, $id);
}

sub with_gate
{
	my ($n, $point, $stmt, $during, $tag, $pre) = @_;
	my $gate = "$tmp/gate_pf_$tag";
	append_to_file($gate, 'x');
	my $sess = start_psql($n, ($pre // '') . " SET kingres_shard.fault_injection = '$point:wait:$gate'; $stmt");
	ok(wait_for_fault_file("$gate.reached", 60), "the statement waits at $point ($tag)");
	$during->();
	unlink $gate;
	return finish_psql($sess);
}

sub change_primary
{
	my ($f1, $f2, $s2, $id, $hard) = @_;
	if ($hard) { $f2->stop('immediate'); }
	else { $f2->safe_psql('postgres', 'CHECKPOINT'); }
	promote_standby($s2);
	set_sync($f2) unless $hard;
	$f1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
}

# after a scenario: the repeated statement, the prepared one, nothing prepared, no lost part
sub after
{
	my ($label, $f1, $s2, $p, $want, $sess, $nm) = @_;
	my ($rc, $out, $err) = run_sql($f1, $p->{sql});
	is(norm($out), norm($want), "$label: the statement repeated gives the baseline") or diag($err);
	$sess->{stderr} = '';
	my $got = $sess->query("EXECUTE $nm");
	my $e = $sess->{stderr};
	$sess->{stderr} = '';
	if ($e =~ /ERROR/)
	{
		like($e, $KS, "$label: the prepared statement planned before the change fails with the retryable error");
		is(norm($sess->query_safe("EXECUTE $nm")), norm($want), "$label: and works when it is repeated");
	}
	else { is(norm($got), norm($want), "$label: the prepared statement planned before the change was right"); }
	$sess->quit;
	$f1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared_count($_), 0, "$label: nothing is prepared on " . $_->name) for ($f1, $s2);
	is($f1->safe_psql('postgres', q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name LIKE 'participant_lost%' OR check_name LIKE 'prepared_rolled%'}),
		'0', "$label: check_cluster() reports no lost part");
}

# ---------------------------------------------------------------------------
# A. a planned switchover at each fault point
# ---------------------------------------------------------------------------
my $k = 0;
for my $p (@paths)
{
	for my $point (qw(frag_after_probe_before frag_after_declare frag_after_probe_after frag_first_fetch))
	{
		my $label = "A: '$p->{name}' at $point";
		my ($f1, $f2, $s2, $id) = setup2('pf' . $k++);
		like(plan_of($f1, $p->{sql}), $p->{plan}, "$label: the statement is shipped as that path");
		my ($rc, $want, $err) = run_sql($f1, $p->{sql});
		my $sess = $f1->background_psql('postgres', on_error_stop => 0);
		$sess->query_safe("PREPARE pq AS $p->{sql}");
		$sess->query_safe('EXECUTE pq') for (1 .. 6);
		my ($out, $e) = with_gate($f1, $point, $p->{sql}, sub { change_primary($f1, $f2, $s2, $id, 0) }, 'a' . $k);
		if ($e =~ /ERROR/)
		{
			like($e, $KS, "$label: the statement that met the change fails with the retryable error");
			is(norm($out), '', "$label: and printed no row");
		}
		else { is(norm($out), norm($want), "$label: the statement that met the change gave the single-node answer"); }
		after($label, $f1, $s2, $p, $want, $sess, 'pq');
		release_node($_) for ($f1, $f2, $s2);
	}
}

# ---------------------------------------------------------------------------
# B. a hard failover in the middle of a fetch
# ---------------------------------------------------------------------------
for my $p (@paths)
{
	my $label = "B: '$p->{name}', the old primary killed in the middle of a fetch";
	my ($f1, $f2, $s2, $id) = setup2('pf' . $k++);
	my ($rc, $want, $err) = run_sql($f1, $p->{sql});
	my $sess = $f1->background_psql('postgres', on_error_stop => 0);
	$sess->query_safe("PREPARE pq AS $p->{sql}");
	$sess->query_safe('EXECUTE pq') for (1 .. 6);
	my ($out, $e) = with_gate($f1, 'frag_wait_batch', $p->{sql}, sub { change_primary($f1, $f2, $s2, $id, 1) }, 'b' . $k, 'SET kingres_shard.scan_fetch_size = 2;');
	like($e, $KS, "$label: the statement fails with the retryable error (the group has a standby)");
	is(norm($out), '', "$label: and printed no row");
	after($label, $f1, $s2, $p, $want, $sess, 'pq');
	release_node($_) for ($f1, $s2);
}

# ---------------------------------------------------------------------------
# C. a transaction that wrote through the old primary and reads through the new one
# ---------------------------------------------------------------------------
for my $p (@paths)
{
	my $label = "C: '$p->{name}', a transaction that wrote through the old primary";
	my ($f1, $f2, $s2, $id) = setup2('pf' . $k++);
	my ($rc, $want, $err) = run_sql($f1, $p->{sql});
	my $b = $f1->background_psql('postgres', on_error_stop => 0);
	$b->query_safe('BEGIN');
	$b->query_safe(q{INSERT INTO public.s SELECT 500000 + i, 1, 1, 3 FROM generate_series(1, 40) i});
	change_primary($f1, $f2, $s2, $id, 0);
	$b->{stderr} = '';
	$b->query($p->{sql});
	like($b->{stderr}, qr/already uses node .* of shard group 1|bucket map|map changed|no longer|ERROR/,
		"$label: the read is refused, one node of a group per transaction (never a read of the new primary beside a write of the old)");
	$b->{stderr} = '';
	$b->query('ROLLBACK');
	$b->quit;
	my ($rc2, $out2, $e2) = run_sql($f1, $p->{sql});
	is(norm($out2), norm($want), "$label: after the rollback the statement gives the baseline") or diag($e2);
	is(prepared_count($_), 0, "$label: nothing is prepared on " . $_->name) for ($f1, $s2);
	release_node($_) for ($f1, $f2, $s2);
}

done_testing();
