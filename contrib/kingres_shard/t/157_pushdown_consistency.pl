# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/157_pushdown_consistency.pl
#
# The design's 063_pushdown_consistency (design/shard-phase2b.md, 10.1, 10.2, 10.4 and 16.2) for the paths of steps 7 to 10:
# the partial aggregation, the ordered merge with a limit (also a limit given as a parameter), the rows of a join and an
# object of an extension.  The closed query of step 6 has its own part of 063 in t/125.  The rule of every section: a
# statement either gives the rows it gives when nothing happens, or states that the map changed (KS001); it never gives
# rows of a state that did not exist on a group, a row twice, or a row less.
#
#   A. a writer on the groups while each path runs, from every node.  The writer inserts pairs of rows in one group in one
#      transaction (w = +1 and -1, two rows of s and two rows of o for one key), so that every state of a group has an even
#      count and a zero sum: a result of a state that never existed breaks it;
#   B. a bucket moves back and forth while every path runs on every node: the answer, or KS001, never another answer;
#   C. a bucket moves between the plan and the run of each path (the statement waits at a fault point);
#   D. a transaction that read through a fragment holds the switch of a move back, and the move goes on when it ends
#      (design 10.2, fact 3);
#   E. prepared statements: six executions, a generic plan, and a plan made before a move;
#   F. a transaction that sees its own writes through each path.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use IPC::Run ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time sleep);
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql);

my $tmp = $PostgreSQL::Test::Utils::tmp_check;
my $KS = qr/KS001|bucket map|map changed|no longer what it was|failed while a distributed statement|already uses node|partitions of table/;
my $IDS = 400;

my @nodes = make_cluster('pc', 3, logical => 1,
	extra => "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_join = on\nkingres_shard.enable_pushdown_sort = on\n");
my ($n1, $n2, $n3) = @nodes;
$n1->safe_psql('postgres', qq{
	CREATE TABLE public.s (id int, c int, w int, g int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12);
	CREATE TABLE public.o (id int, c int, x int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.o', 'id', colocate_with => 'public.s');
	INSERT INTO public.s SELECT i, c, CASE c WHEN 1 THEN 1 ELSE -1 END, i % 7 FROM generate_series(1, $IDS) i, generate_series(1, 2) c;
	INSERT INTO public.o SELECT i, c, (i * c) % 13 FROM generate_series(1, $IDS) i, generate_series(1, 2) c;
});
$_->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch') for @nodes;
$_->safe_psql('postgres', 'ANALYZE') for @nodes;

# the paths: the statement, the text of its plan, and what a state of a group allows
my @paths = (
	{ name => 'partial aggregation', plan => qr/Finalize/,
	  sql => q{SELECT g, count(*), sum(w) FROM public.s GROUP BY g ORDER BY g} },
	{ name => 'ordered merge with a limit', plan => qr/Merge keys/,
	  sql => q{SELECT id, count(*) AS cnt FROM public.s GROUP BY id ORDER BY cnt DESC, id DESC LIMIT 9} },
	{ name => 'join rows', plan => qr/Pushed: join rows/,
	  sql => q{SELECT s.id, o.x, row_number() OVER (ORDER BY o.x, s.id, s.c, o.c) FROM public.s JOIN public.o ON o.id = s.id WHERE s.id <= 40 OR s.id >= 100000} },
	{ name => 'an object of an extension', plan => qr/Finalize/,
	  sql => q{SELECT g, kingres_sketch.approx_count_distinct(w) FROM public.s GROUP BY g ORDER BY g} },
);

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# the lines of a result: a command tag (SET) that psql prints for a statement of a script is not a row
sub rows { my ($out) = @_; return grep { length && $_ ne 'SET' } split /\n/, $out; }
sub norm { my ($out) = @_; return join("\n", rows($out)); }

# what a state of the table may look like: undef when the result is possible, else why not
sub state_problem
{
	my ($name, $out) = @_;
	my @r = rows($out);
	return 'no rows' unless @r;
	if ($name eq 'partial aggregation')
	{
		for (@r) { my ($g, $c, $s) = split /\|/; return "g=$g has an odd count $c" if $c % 2; return "g=$g has the sum $s" if $s != 0; }
	}
	elsif ($name eq 'ordered merge with a limit')
	{
		my $prev;
		for (@r)
		{
			my ($id, $c) = split /\|/;
			return "id $id has $c rows" if $c != 2;
			return "ids are not in order at $id" if defined $prev && $id >= $prev;
			$prev = $id;
		}
		return 'not 9 rows' if @r != 9;
	}
	elsif ($name eq 'join rows')
	{
		my %n;
		my $last = 0;
		for (@r)
		{
			my ($id, $x, $rn) = split /\|/;
			$n{$id}++;
			return "row numbers are not 1, 2, 3 ... at $rn" if $rn != ++$last;
		}
		for (keys %n) { return "id $_ has $n{$_} join rows" if $n{$_} != 4; }
	}
	else
	{
		for (@r) { my ($g, $c) = split /\|/; return "g=$g has $c distinct values" if $c != 2; }
	}
	return undef;
}

sub start_file
{
	my ($node, $file) = @_;
	my ($out, $err) = ('', '');
	my $h = IPC::Run::start([ 'psql', '-X', '-q', '-At', '-h', $node->host, '-p', $node->port, '-d', 'postgres', '-f', $file ], '>', \$out, '2>', \$err);
	return { h => $h, out => \$out, err => \$err };
}

sub clean_nodes
{
	my ($label) = @_;
	for my $n (@nodes)
	{
		my $left = 1;
		for (1 .. 100)
		{
			$left = $n->safe_psql('postgres', q{SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction' AND pid <> pg_backend_pid()});
			last if $left == 0;
			sleep(0.1);
		}
		is($left, 0, "$label: no session of " . $n->name . ' is left in a transaction');
		is($n->safe_psql('postgres', q{SELECT count(*) FROM pg_cursors}), 0, "$label: no cursor is open on " . $n->name);
		is(prepared_count($n), 0, "$label: nothing is prepared on " . $n->name);
	}
}

sub bucket_of
{
	my ($node, $group) = @_;
	return $node->safe_psql('postgres', "SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = $group");
}

sub group_of_bucket
{
	my ($node, $b) = @_;
	return $node->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b");
}

sub with_gate
{
	my ($n, $point, $stmt, $during, $tag, $pre) = @_;
	my $gate = "$tmp/gate_pc_$tag";
	append_to_file($gate, 'x');
	my $sess = start_psql($n, ($pre // '') . " SET kingres_shard.fault_injection = '$point:wait:$gate'; $stmt");
	ok(wait_for_fault_file("$gate.reached", 60), "the statement waits at $point ($tag)");
	$during->();
	unlink $gate;
	return finish_psql($sess);
}

# the baselines, and the paths that the planner ships
my %WANT;
for my $p (@paths)
{
	like(plan_of($n1, $p->{sql}), $p->{plan}, "the statement of '$p->{name}' is shipped as that path");
	my ($rc, $out, $err) = run_sql($n1, $p->{sql});
	is($rc, 0, "'$p->{name}': the baseline runs") or diag($err);
	my ($rc2, $scan) = run_sql($n1, "SET kingres_shard.enable_pushdown = off; $p->{sql}");
	is(norm($out), norm($scan), "'$p->{name}': the pushed answer is the row-moving scan's");
	$WANT{ $p->{name} } = norm($out);
	is(state_problem($p->{name}, $out), undef, "'$p->{name}': the baseline is a possible state");
}

# ---------------------------------------------------------------------------
# A. a writer on the groups while each path runs
# ---------------------------------------------------------------------------
{
	my $N = 90;
	my $file = "$tmp/writer_pc.sql";
	open(my $fh, '>', $file) or die;
	for my $i (0 .. $N - 1)
	{
		my $id = 100000 + $i;
		my $g = $id % 7;
		print $fh "BEGIN;\nINSERT INTO public.s VALUES ($id, 1, 1, $g), ($id, 2, -1, $g);\nINSERT INTO public.o VALUES ($id, 1, " . ($id % 13) . "), ($id, 2, " . (($id * 2) % 13) . ");\nCOMMIT;\n";
	}
	close $fh;
	my $writer = start_file($n2, $file);
	my @bad;
	my $runs = 0;
	my $alive = sub { $writer->{h}->pump_nb; return $writer->{h}->pumpable; };
	for (my $round = 0; $round < 6 || ($alive->() && $round < 40); $round++)
	{
		for my $nd (@nodes)
		{
			for my $p (@paths)
			{
				my ($rc, $out, $err) = run_sql($nd, $p->{sql});
				$runs++;
				if ($rc != 0) { push @bad, $nd->name . " $p->{name}: error $err"; next; }
				my $why = state_problem($p->{name}, $out);
				push @bad, $nd->name . " $p->{name}: $why" if defined $why;
			}
		}
	}
	my ($wo, $we) = finish_psql($writer);
	unlike($we, qr/ERROR/, 'the writer ran without an error') or diag($we);
	is(scalar(@bad), 0, "$runs runs of the four paths while the writer committed: every result is a state that existed on each group") or diag(join("\n", @bad[0 .. ($#bad < 4 ? $#bad : 4)]));
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.s WHERE id >= 100000}), 2 * $N, 'the writer wrote all its rows');
	for my $p (@paths)
	{
		my ($rc, $out) = run_sql($n3, $p->{sql});
		my ($rc2, $scan) = run_sql($n3, "SET kingres_shard.enable_pushdown = off; $p->{sql}");
		is(norm($out), norm($scan), "'$p->{name}': after the writer the pushed answer is the scan's");
		$WANT{ $p->{name} } = norm($out);
	}
	$_->safe_psql('postgres', 'ANALYZE') for @nodes;
	like(plan_of($n1, $_->{sql}), $_->{plan}, "'$_->{name}': still shipped after the writer") for @paths;
	clean_nodes('after the writer');
}

# ---------------------------------------------------------------------------
# B. a bucket moves back and forth while every path runs on every node
# ---------------------------------------------------------------------------
{
	my $b = bucket_of($n1, 1);
	my $mover = start_psql($n1, "SELECT kingres.move_bucket(1, $b, 2); SELECT kingres.move_bucket(1, $b, 1); SELECT kingres.move_bucket(1, $b, 2)");
	my ($same, $ks, @other) = (0, 0);
	my $alive = sub { $mover->{h}->pump_nb; return $mover->{h}->pumpable; };
	for (my $round = 0; $round < 2 || ($alive->() && $round < 400); $round++)
	{
		for my $nd (@nodes)
		{
			for my $p (@paths)
			{
				my ($rc, $out, $err) = run_sql($nd, $p->{sql});
				if ($rc == 0) { if (norm($out) eq $WANT{ $p->{name} }) { $same++ } else { push @other, $nd->name . " $p->{name}: another answer" } }
				elsif ($err =~ $KS) { $ks++ }
				else { push @other, $nd->name . " $p->{name}: $err" }
			}
		}
	}
	my ($mo, $me) = finish_psql($mover);
	unlike($me, qr/ERROR/, 'the three moves completed') or diag($me);
	is(group_of_bucket($n1, $b), 2, 'the bucket ended in the group the last move named');
	is(scalar(@other), 0, "the answer or KS001 on every run ($same answers, $ks retryable errors), never another answer") or diag(join("\n", @other[0 .. ($#other < 4 ? $#other : 4)]));
	cmp_ok($same, '>', 0, 'some runs completed while the buckets moved');
	for my $p (@paths)
	{
		my ($rc, $out, $err) = run_sql($n2, $p->{sql});
		is(norm($out), $WANT{ $p->{name} }, "'$p->{name}': after the moves the answer is the baseline") or diag($err);
	}
	clean_nodes('after the moves');
}

# ---------------------------------------------------------------------------
# C. a bucket moves between the plan and the run
# ---------------------------------------------------------------------------
{
	$_->safe_psql("postgres", "ANALYZE") for @nodes;
	like(plan_of($n1, $_->{sql}), $_->{plan}, "$_->{name}: shipped before the moves between plan and run") for @paths;
	my $ci = 0;
	for my $p (@paths)
	{
		$_->safe_psql("postgres", "ANALYZE") for @nodes;
		like(plan_of($n1, $p->{sql}), $p->{plan}, "$p->{name}: shipped (statistics are current after the earlier move)");
		my $b = bucket_of($n1, 2);
		my $to = $ci % 2;
		my ($out, $err) = with_gate($n1, 'frag_before_probe', $p->{sql}, sub { $n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, $to)") }, 'c' . $ci++);
		if ($err =~ /ERROR/) { like($err, $KS, "'$p->{name}': a bucket that moved between the plan and the run fails the statement with the retryable error"); }
		else { is(norm($out), $WANT{ $p->{name} }, "'$p->{name}': a bucket that moved between the plan and the run: the statement was right"); }
		my ($rc, $o2, $e2) = run_sql($n1, $p->{sql});
		is(norm($o2), $WANT{ $p->{name} }, "'$p->{name}': the statement repeated gives the baseline") or diag($e2);
	}
	clean_nodes('after the moves between plan and run');
}

# ---------------------------------------------------------------------------
# D. a transaction that read through a fragment holds the switch back
# ---------------------------------------------------------------------------
{
	$_->safe_psql("postgres", "ANALYZE") for @nodes;
	for my $p (@paths[0, 2])
	{
		my $g = group_of_bucket($n1, bucket_of($n1, 0));
		my $b = bucket_of($n1, 0);
		my $target = $g == 1 ? 2 : 1;
		my $holder = $n1->background_psql('postgres', on_error_stop => 0);
		$holder->query_safe('BEGIN ISOLATION LEVEL REPEATABLE READ');
		is(norm($holder->query_safe($p->{sql})), $WANT{ $p->{name} }, "'$p->{name}': the transaction reads through the groups");
		my $mover = start_psql($n1, "SET kingres_shard.move_switch_timeout = '30s'; SELECT kingres.move_bucket(1, $b, $target)");
		sleep(6);
		$mover->{h}->pump_nb;
		ok($mover->{h}->pumpable, "'$p->{name}': the move is still running while the transaction holds its cursors (design 10.2, fact 3)");
		is(group_of_bucket($n1, $b), $g, '... and the bucket has not changed group');
		$holder->query_safe('COMMIT');
		my ($mo, $me) = finish_psql($mover);
		unlike($me, qr/ERROR/, '... the move completes when the transaction ends') or diag($me);
		is(group_of_bucket($n1, $b), $target, '... and the bucket is in its new group');
		$holder->quit;
		my ($rc, $out, $err) = run_sql($n2, $p->{sql});
		is(norm($out), $WANT{ $p->{name} }, "'$p->{name}': after the move the answer is the baseline") or diag($err);
	}
	clean_nodes('after the held switch');
}

# ---------------------------------------------------------------------------
# E. prepared statements
# ---------------------------------------------------------------------------
{
	$_->safe_psql("postgres", "ANALYZE") for @nodes;
	like(plan_of($n1, $_->{sql}), $_->{plan}, "$_->{name}: shipped before the prepared statements") for @paths;
	my @prep = map { { name => $_->{name}, sql => $_->{sql}, pre => '' } } @paths;
	# a limit given as a parameter: the plan holds for every value
	push @prep, { name => 'a limit given as a parameter', sql => q{SELECT id, count(*) AS cnt FROM public.s GROUP BY id ORDER BY cnt DESC, id DESC LIMIT $1},
		args => '(9)', decl => '(int)', want => $WANT{'ordered merge with a limit'} };
	my $i = 0;
	for my $q (@prep)
	{
		my $nm = 'pe' . $i++;
		my $want = $q->{want} // $WANT{ $q->{name} };
		my $decl = $q->{decl} // '';
		my $args = $q->{args} // '';
		my $s = $n1->background_psql('postgres', on_error_stop => 0);
		$s->query_safe("PREPARE $nm$decl AS $q->{sql}");
		my $all = 1;
		for (1 .. 6) { $all &&= norm($s->query_safe("EXECUTE $nm$args")) eq $want; }
		ok($all, "'$q->{name}': six executions of the prepared statement give the baseline");
		$s->query_safe('SET plan_cache_mode = force_generic_plan');
		is(norm($s->query_safe("EXECUTE $nm$args")), $want, "'$q->{name}': a generic plan gives the baseline");
		my $b = bucket_of($n1, 1);
		my $orig = group_of_bucket($n1, $b);
		$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, 0)");
		$s->{stderr} = '';
		my $got = $s->query("EXECUTE $nm$args");
		my $e = $s->{stderr};
		$s->{stderr} = '';
		if ($e =~ /ERROR/)
		{
			like($e, $KS, "'$q->{name}': the plan made before a move fails with the retryable error");
			is(norm($s->query_safe("EXECUTE $nm$args")), $want, '... and works when repeated');
		}
		else { is(norm($got), $want, "'$q->{name}': the plan made before a move was right"); }
		$s->quit;
		$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, " . $orig . ")") if group_of_bucket($n1, $b) != $orig;
	}
	clean_nodes('after the prepared statements');
}

# ---------------------------------------------------------------------------
# F. a transaction that sees its own writes
# ---------------------------------------------------------------------------
{
	my $writes = join('', map { my $id = 200000 + $_; "INSERT INTO public.s VALUES ($id, 1, 1, 3), ($id, 2, -1, 3); INSERT INTO public.o VALUES ($id, 1, 5), ($id, 2, 6);\n" } (1 .. 3));
	my %in;
	for my $p (@paths)
	{
		my ($rc, $out, $err) = run_sql($n1, "BEGIN;\n$writes$p->{sql};\nROLLBACK;");
		is($rc, 0, "'$p->{name}': the transaction reads its own writes") or diag($err);
		$in{ $p->{name} } = $out;
	}
	my @base = rows($WANT{'partial aggregation'});
	my @inp = rows($in{'partial aggregation'});
	my ($c3) = map { (split /\|/)[1] } grep { /^3\|/ } @base;
	my ($c3in) = map { (split /\|/)[1] } grep { /^3\|/ } @inp;
	is($c3in, $c3 + 6, 'partial aggregation: the count of the group of the new rows grew by six (three keys, two rows each)');
	my ($first) = rows($in{'ordered merge with a limit'});
	like($first, qr/^200003\|2$/, 'ordered merge: the newest key of the transaction comes first');
	my %perid;
	$perid{ (split /\|/)[0] }++ for rows($in{'join rows'});
	is($perid{200001} // 0, 4, 'join rows: a new key has its four join rows');
	is(scalar(keys %perid), scalar(keys %{{ map { ((split /\|/)[0] => 1) } rows($WANT{'join rows'}) }}) + 3, '... and the three new keys are in the result');
	is(norm($in{'an object of an extension'}), $WANT{'an object of an extension'}, 'extension: the distinct count of w is unchanged by pairs of +1 and -1');
	for my $p (@paths)
	{
		my ($rc, $out) = run_sql($n1, $p->{sql});
		is(norm($out), $WANT{ $p->{name} }, "'$p->{name}': after the rollback the answer is the baseline");
	}
	clean_nodes('after the transaction that reads its writes');
}

done_testing();
