# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/105_fragment_foundation.pl
#
# Step 2 of phase 2b (design/shard-phase2b.md, section 17; parts B to F of the
# test 055_fragment_foundation of section 16.2, whose part A is t/098): the
# placement check, pruning, the local part of a fan-out write, the reads that mix
# a distributed table with a local table, and the cancel in every wait of the
# distributed scan.
#
#   B. pruning: a read asks only the groups that can own a key it names (an IN
#      list, = ANY, an OR, a parameter of a generic plan); shown with a node that is
#      down, which only a read that needs it can notice, and with the plan;
#   C. the placement check of the scan: a bucket that leaves and comes back between
#      the first probe and the end of the start (review finding S4-01), on a pair of
#      groups that does not include the origin and on one that does; a node that
#      runs another version of the extension;
#   D. the local part of a fan-out write is recognized by a nonce, after the
#      partition lists are as new as the catalog, and nothing else is;
#   E. a read that mixes a distributed table with a local table, a scrollable
#      cursor, and a money value are answered like a single node answers them;
#   F. a cancel in every wait of the start of a scan.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql wait_for_fault_file
  start_psql finish_psql);
use KingresShardCompare;

my $src = "$FindBin::RealBin/../src";

sub slurp
{
	my ($path) = @_;
	open(my $fh, '<', $path) or die "could not open $path: $!";
	local $/;
	my $text = <$fh>;
	close($fh);
	return $text;
}

# ---------------------------------------------------------------------------
# B. pruning
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('fb', 3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, v int NOT NULL);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	my @k = map { [ keys_of_group($n1, 1, $_, 3) ] } (0, 1, 2);
	for my $g (0 .. 2)
	{
		$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 1)") for @{ $k[$g] };
	}
	my ($a1, $a2) = @{ $k[0] };
	my ($b1, $b2) = @{ $k[1] };
	my ($c1) = @{ $k[2] };

	my $plan = sub {
		my ($sql) = @_;
		my ($rc, $out, $err) = run_sql($n1, "EXPLAIN (COSTS OFF) $sql");
		return $out;
	};
	like($plan->("SELECT count(*) FROM public.kv WHERE id IN ($a1, $b1, $b2)"),
		qr/Remote groups: 1 of 2 \(pruned by id\)/, 'an IN list that names two groups asks one remote group');
	like($plan->("SELECT count(*) FROM public.kv WHERE id IN ($a1, $b1, $c1)"),
		qr/Remote groups: 2\n/, 'an IN list that names every group asks every remote group (no pruning line)');
	like($plan->("SELECT count(*) FROM public.kv WHERE id = ANY (ARRAY[$a1, $a2])"),
		qr/Remote groups: 0 of 2 \(pruned by id\)/, '= ANY of keys of the origin\'s group asks no remote group');
	like($plan->("SELECT count(*) FROM public.kv WHERE id = $a1 OR id = $b1"),
		qr/Remote groups: 1 of 2/, 'an OR of two keys is pruned to the union');
	unlike($plan->("SELECT count(*) FROM public.kv WHERE id + 0 IN ($a1, $b1)"),
		qr/pruned by/, 'an expression of the key is not pruned');
	unlike($plan->("SELECT count(*) FROM public.kv WHERE id IN ($a1, $b1) OR v = 5"),
		qr/pruned by/, 'an OR with an arm that does not name the key is not pruned');

	my $want = 3;
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id IN ($a1, $a2, $b1)"), $want,
		'before a node is stopped: the answer');

	# A node that is down is noticed only by a read that needs it.
	$n3->stop('fast');
	my @pruned = (
		[ 'an IN list', "SELECT count(*) FROM public.kv WHERE id IN ($a1, $a2, $b1)", $want ],
		[ 'a = ANY array', "SELECT count(*) FROM public.kv WHERE id = ANY (ARRAY[$a1, $b1, $b2])", $want ],
		[ 'an OR of keys', "SELECT count(*) FROM public.kv WHERE id = $a1 OR id = $b1 OR id = $b2", $want ],
		[ 'keys in two columns of a join',
		  "SELECT count(*) FROM public.kv x JOIN public.kv y ON x.id = y.id WHERE x.id IN ($a1, $b1) AND y.id IN ($a1, $b1)", 2 ],
	);
	for my $q (@pruned)
	{
		my ($rc, $out, $err) = run_sql($n1, $q->[1]);
		is($out, $q->[2], "$q->[0]: answered while the group that cannot own the keys is down") or diag($err);
		($rc, $out, $err) = run_sql($n2, $q->[1]);
		is($out, $q->[2], "$q->[0]: the same from the node of another group") or diag($err);
	}
	{
		# Parameters of a generic plan: the groups are chosen when the scan starts.
		my ($rc, $out, $err) = run_sql($n1, qq{
			SET plan_cache_mode = force_generic_plan;
			PREPARE pa(int[]) AS SELECT count(*) FROM public.kv WHERE id = ANY (\$1);
			EXECUTE pa('{$a1,$b1}');
			EXECUTE pa('{$a1,$a2,$b1,$b2}');
			PREPARE pb(int, int) AS SELECT count(*) FROM public.kv WHERE id = \$1 OR id = \$2;
			EXECUTE pb($a1, $b1);
			EXECUTE pb($b1, $b2);
		});
		is($out, "2\n4\n2\n2", 'a generic plan prunes by the value of its parameters') or diag($err);
		($rc, $out, $err) = run_sql($n1, qq{
			SET plan_cache_mode = force_generic_plan;
			PREPARE pc(int[]) AS SELECT count(*) FROM public.kv WHERE id = ANY (\$1);
			EXECUTE pc('{$a1,$c1}');
		});
		isnt($rc, 0, 'the same generic plan asks the group that is down when a parameter names its key');
		like($err, qr/could not connect|connection/, 'and says so');
	}
	# What is not pruned needs every group, the one that is down too.
	for my $sql ("SELECT count(*) FROM public.kv WHERE id + 0 IN ($a1, $b1)",
		"SELECT count(*) FROM public.kv WHERE id IN ($a1, $c1)")
	{
		my ($rc, $out, $err) = run_sql($n1, $sql);
		isnt($rc, 0, "needs the node that is down: $sql");
	}
	$n3->start;
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), 9, 'with the node back, the whole table is read');

	# A pruned read and an unpruned one give the same answer for every key set.
	for my $keys ("$a1", "$b1, $c1", "$a1, $b1, $c1", "$a1, 999999")
	{
		is($n1->safe_psql('postgres', "SELECT count(*), coalesce(sum(v), 0) FROM public.kv WHERE id IN ($keys)"),
			$n1->safe_psql('postgres', "SELECT count(*), coalesce(sum(v), 0) FROM public.kv WHERE id + 0 IN ($keys)"),
			"pruning changes the cost and not the answer: IN ($keys)");
	}
}

# ---------------------------------------------------------------------------
# C. the placement check of the scan
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('fc', 3, logical => 1);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.t (id int PRIMARY KEY, v int NOT NULL);
		SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 12);
	});
	$n1->safe_psql('postgres', join('', map { "INSERT INTO public.t VALUES ($_, $_);\n" } 1 .. 120));
	my $total = '120';

	my $bucket_of_group = sub {
		my ($g) = @_;
		return $n1->safe_psql('postgres',
			"SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = $g");
	};
	my $gate_n = 0;
	# The scan from node 1 is stopped after its first probe; $during runs; the scan goes on.
	my $scan_with = sub {
		my ($point, $during) = @_;
		my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_fc_" . $gate_n++;
		append_to_file($gate, 'x');
		my $scan = start_psql($n1,
			"SET kingres_shard.fault_injection = '$point:wait:$gate'; SELECT count(*) FROM public.t;");
		ok(wait_for_fault_file("$gate.reached", 30), "the scan waits at $point");
		$during->();
		unlink $gate;
		return finish_psql($scan);
	};

	# The pair of groups 1 and 2, which does not include the origin (group 0): a bucket of group 1
	# goes to group 2 and comes back while the scan waits after its first probe.
	my $b = $bucket_of_group->(1);
	my ($out, $err) = $scan_with->('scan_after_placement', sub {
		$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, 2)");
		$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, 1)");
	});
	like($err, qr/bucket map changed/, 'S4-01: a bucket that left and came back (not the origin\'s pair): KS001');
	unlike($out, qr/[0-9]/, 'and no answer was returned');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.t'), $total, 'repeated, the scan answers completely');

	# A bucket of group 1 goes to the origin's group while the scan waits: the groups asked are
	# those of the placement of the plan, which no longer is the placement.
	$b = $bucket_of_group->(1);
	($out, $err) = $scan_with->('scan_after_placement', sub {
		$n1->safe_psql('postgres', "SELECT kingres.move_bucket(1, $b, 0)");
	});
	like($err, qr/bucket map changed/, 'a bucket that arrived on the origin: KS001');
	unlike($out, qr/[0-9]/, 'and no answer was returned (the origin pair)');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.t'), $total, 'repeated, the scan answers completely');

	# After the DECLAREs the cursors hold the parent in ACCESS SHARE on the nodes, which a switch waits for
	# (design 10.2): that window cannot be played with a move that has to finish first; the second probe
	# is tested by the fault points of t/031.

	# A node that runs another version of the extension is refused with an error that says so and that
	# no repeat helps: not KS001.
	$n2->safe_psql('postgres', "UPDATE pg_catalog.pg_extension SET extversion = '0.4' WHERE extname = 'kingres_shard'");
	my ($rc, $o, $e) = run_sql($n1, "\\set VERBOSITY verbose\nSELECT count(*) FROM public.t");
	isnt($rc, 0, 'a node of another version: the scan fails');
	like($e, qr/runs kingres_shard 0\.4, this node runs 0\.5/, 'and names the node and the versions');
	unlike($e, qr/KS001/, 'it is not the retryable error');
	$n2->safe_psql('postgres', "UPDATE pg_catalog.pg_extension SET extversion = '0.5' WHERE extname = 'kingres_shard'");
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.t'), $total, 'with the version back the scan answers');
}

# ---------------------------------------------------------------------------
# D. the local part of a fan-out write
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('fd', 3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.kv (id int PRIMARY KEY, v int NOT NULL);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	$n1->safe_psql(q{postgres}, join(q{}, map { "INSERT INTO public.kv VALUES ($_, 1);
" } 1 .. 60));
	my $changed = sub {
		return join(',', map { $_->safe_psql('postgres',
				"SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.kv WHERE v = 2") } ($n1, $n2, $n3));
	};
	$n1->safe_psql('postgres', 'UPDATE public.kv SET v = 2');
	my $per = $changed->();
	is(eval { my $s = 0; $s += $_ for split /,/, $per; $s }, 60, 'a write that spans groups reaches every group (the local part and two remote parts)');
	cmp_ok(scalar(grep { $_ > 0 } split /,/, $per), q{==}, 3, "and every node holds some of the rows ($per)");

	# A statement that carries a comment of the local part but not the nonce of a running one is an
	# ordinary statement: it is routed to every group, not planned for this node only.
	$n1->safe_psql('postgres', 'UPDATE public.kv SET v = 1');
	$n1->safe_psql('postgres', '/* kingres:local 0000000000000000 */ UPDATE public.kv SET v = 2');
	$per = $changed->();
	is(eval { my $s = 0; $s += $_ for split /,/, $per; $s }, 60, 'a forged comment of the local part does not make a statement local');
	$n1->safe_psql('postgres', '/* kingres:local 1 */ UPDATE public.kv SET v = 3');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv WHERE v = 3'), 60, 'whatever its number');

	# The order of the planner hook: the nonce is recognized after the analysis and after the partition
	# lists are made as new as the catalog (design 4.4).
	my $planner = slurp("$src/planner.c");
	my $i_fresh = index($planner, 'kingres_shard_ensure_fresh_partitions(((DistRef *)');
	my $i_nonce = index($planner, 'kingres_shard_local_part_nonce_ok(query_string)');
	cmp_ok($i_fresh, '>', 0, 'planner.c makes the partition lists as new as the catalog after its analysis');
	cmp_ok($i_nonce, '>', $i_fresh, 'and recognizes the local part after that');
	unlike(substr($planner, 0, $i_fresh), qr/kingres_shard_local_part_nonce_ok/, 'and nowhere before');
	unlike($planner . slurp("$src/fanout.c"), qr/kingres_shard_in_fanout_local|fanout_local_sql/,
		'the comparison of whole statement texts is gone');
}

# ---------------------------------------------------------------------------
# E. reads that mix a distributed table with a local table, SCROLL cursors, money
# ---------------------------------------------------------------------------
{
	my $c = KingresShardCompare->new('fe');
	$c->on_cluster(q{
		CREATE TABLE public.kv (id int PRIMARY KEY, val text, price money);
		SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	});
	$c->on_single(q{CREATE TABLE public.kv (id int PRIMARY KEY, val text, price money)});
	$c->on_every_node(q{CREATE TABLE public.loc (i int, tag text)});
	$c->on_single(q{CREATE TABLE public.loc (i int, tag text)});
	my $rows = join('', map { "INSERT INTO public.kv VALUES ($_, 'v$_', " . ($_ % 7) . ");\n" } 1 .. 90);
	$c->on_cluster($rows);
	$c->on_single($rows);
	my $loc = "INSERT INTO public.loc VALUES (3, 'a'), (14, 'b'), (14, 'c'), (200, 'd'), (NULL, 'n');\n";
	$c->on_every_node($loc);
	$c->on_single($loc);

	my @plans = ('scan');
	my $scan = 'Kingres Distributed Scan';
	# A key restriction whose bucket is on another group, joined with a local table (the case 6.5 refused).
	for my $k (3, 14, 25, 41, 77)
	{
		$c->compare("key $k joined with a local table",
			"SELECT kv.id, kv.val, loc.tag FROM public.kv, public.loc WHERE kv.id = $k AND loc.i = kv.id ORDER BY 1, 3",
			ordered => 1, profiles => 0, plans => \@plans);
		$c->compare("key $k, cross join with a local table",
			"SELECT kv.val, loc.tag FROM public.kv, public.loc WHERE kv.id = $k ORDER BY 1, 2",
			ordered => 1, profiles => 0, plans => \@plans);
	}
	$c->compare('an aggregate over a join with a local table',
		q{SELECT loc.tag, count(*), sum(kv.id) FROM public.kv JOIN public.loc ON kv.id = loc.i WHERE kv.id IN (3, 14, 77) GROUP BY loc.tag ORDER BY 1},
		ordered => 1, plans => \@plans, path => $scan);
	$c->compare('a join with a local table, every key',
		q{SELECT count(*), sum(kv.id) FROM public.kv JOIN public.loc ON kv.id = loc.i},
		plans => \@plans, path => $scan);
	$c->compare('a local table in a subquery of a remote-key read',
		q{SELECT val FROM public.kv WHERE id = 14 AND id IN (SELECT i FROM public.loc)},
		profiles => 0, plans => \@plans);

	# A write to a local table that reads a remote key.
	$c->on_every_node('CREATE TABLE public.loc2 (id int, val text)');
	$c->on_single('CREATE TABLE public.loc2 (id int, val text)');
	for my $k (3, 14, 77)
	{
		$c->compare_dml("INSERT INTO a local table SELECT FROM a remote key ($k)",
			"INSERT INTO public.loc2 SELECT id, val FROM public.kv WHERE id = $k");
	}
	$c->compare('the local table after the inserts', 'SELECT id, val FROM public.loc2 ORDER BY 1', ordered => 1, nodes => [0]);

	# A write of a distributed table that also reads a local table stays refused, and the fallback of the
	# step (enable_distributed_scan = off) gives the refusal of 2a back.
	{
		my ($kr) = keys_of_group(($c->nodes)[0], 1, 1, 1, 1);
		my ($rc, $out, $err) = run_sql(($c->nodes)[0],
			"UPDATE public.kv SET val = q{x} FROM public.loc WHERE kv.id = $kr AND loc.i = $kr");
		isnt($rc, 0, q{a write of a distributed table that reads a local table is refused});
		($rc, $out, $err) = run_sql(($c->nodes)[0],
			"SET kingres_shard.enable_distributed_scan = off; SELECT * FROM public.kv, public.loc WHERE kv.id = $kr");
		like($err, qr/another shard group with local tables/,
			q{with the scan switched off the 2a refusal is back (the fallback of the step)});
	}

	# A scrollable cursor on statements of every kind.
	for my $k (3, 14)
	{
		$c->compare("scroll cursor, key $k",
			"BEGIN;\nDECLARE c SCROLL CURSOR FOR SELECT id, val FROM public.kv WHERE id = $k;\nFETCH ALL FROM c;\nFETCH BACKWARD ALL FROM c;\nMOVE ABSOLUTE 0 IN c;\nFETCH 1 FROM c;\nCOMMIT;",
			ordered => 1, profiles => 0);
	}
	$c->compare('scroll cursor on a join with a local table',
		"BEGIN;\nDECLARE c SCROLL CURSOR FOR SELECT kv.id, loc.tag FROM public.kv, public.loc WHERE kv.id = 14 AND loc.i = 14 ORDER BY 2;\nFETCH ALL FROM c;\nFETCH BACKWARD 1 FROM c;\nCOMMIT;",
		ordered => 1, profiles => 0);

	# money is not shipped in a restriction (its text depends on lc_monetary); the answer is the single
	# node's under the session profiles, which set lc_monetary.
	$c->compare('a money restriction on a scan',
		q{SELECT id FROM public.kv WHERE price > '2'::money ORDER BY 1}, ordered => 1, plans => \@plans, path => $scan);
	$c->compare('a money value in the select list',
		q{SELECT id, price FROM public.kv WHERE id IN (3, 14, 77) ORDER BY 1}, ordered => 1, plans => \@plans);
	{
		my ($rc, $out, $err) = run_sql(($c->nodes)[0],
			"EXPLAIN (COSTS OFF) SELECT id FROM public.kv WHERE price > '2'::money AND id > 10");
		like($out, qr/Remote SQL:[^\n]*\(\(id > 10\)\)/, 'the restriction on the key is shipped');
		unlike($out, qr/Remote SQL:[^\n]*WHERE[^\n]*price/, 'the restriction on a money column is not');
	}
}

# ---------------------------------------------------------------------------
# F. a cancel in every wait of the start of a scan
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('ff', 3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.d (id int PRIMARY KEY, v int NOT NULL);
		SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 12);
	});
	$n1->safe_psql(q{postgres}, join(q{}, map { "INSERT INTO public.d VALUES ($_, $_);
" } 1 .. 300));
	my $n = 0;
	for my $point (qw(scan_begin scan_started scan_after_placement scan_after_declare scan_after_check))
	{
		my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_ff_" . $n++;
		append_to_file($gate, 'x');
		my $scan = start_psql($n1,
			"SET kingres_shard.scan_fetch_size = 7; SET kingres_shard.fault_injection = '$point:wait:$gate'; SELECT count(*) FROM public.d;");
		ok(wait_for_fault_file("$gate.reached", 30), "the scan waits at $point");
		my $pid = $n1->safe_psql('postgres',
			"SELECT pid FROM pg_stat_activity WHERE query LIKE '%$point:wait:$gate%' AND pid <> pg_backend_pid()");
		$n1->safe_psql('postgres', "SELECT pg_cancel_backend($pid)");
		my ($out, $err) = finish_psql($scan);
		unlink $gate;
		like($err, qr/canceling statement due to user request/, "a cancel at $point ends the statement");
		is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.d'), 300, "and the next scan answers (after $point)");
	}
	# A cancel (a statement timeout) while rows are being fetched, with a batch in flight.
	my ($rc, $out, $err) = run_sql($n1, q{
		SET kingres_shard.scan_fetch_size = 5; SET statement_timeout = 400; SET enable_hashjoin = off; SET enable_mergejoin = off;
		SELECT count(*) FROM public.d a, public.d b WHERE a.v + b.v >= 0 AND pg_sleep(0.0005) IS NOT NULL;
	});
	like($err, qr/canceling statement due to statement timeout/, 'a timeout in the middle of a scan cancels it');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.d'), 300, 'and the next scan answers');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction'"), 0,
		'no session is left in a transaction on the origin');
	for my $nd ($n2, $n3)
	{
		is($nd->safe_psql('postgres', "SELECT count(*) FROM pg_cursors"), 0, 'no cursor is left on ' . $nd->name);
		unlike(slurp_file($nd->logfile), qr/terminated by signal|TRAP:/, 'no crash in the log of ' . $nd->name);
	}
	unlike(slurp_file($n1->logfile), qr/terminated by signal|TRAP:/, 'no crash in the log of the origin');
}

done_testing();
