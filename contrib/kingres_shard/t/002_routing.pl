# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Three-node cluster: statement routing (design/shard-mvp.md, "Routing" and
# "Tests required for phase 1", multi-node part 2): forwarding by key from
# every node, EXPLAIN, update and delete, prepared statements, transactions,
# the "span shard groups" error, rollback of forwarded writes, the phase 1
# write limits, loop prevention and the stale-map retry.

use strict;
use warnings FATAL => 'all';
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

sub make_node
{
	my ($name) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init;
	$node->append_conf('postgresql.conf',
		"shared_preload_libraries = 'kingres_shard'
max_prepared_transactions = 20
kingres_shard.recovery_interval = 0");
	$node->start;
	return $node;
}

my @nodes = map { make_node("r$_") } (1 .. 3);
my ($n1, $n2, $n3) = @nodes;

$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres',
	sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
for my $n ($n2, $n3)
{
	$n1->safe_psql('postgres',
		sprintf("SELECT kingres.add_node('%s', %d)", $n->host, $n->port));
}

# Tables live in public: the OS user is usually named like a schema.
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	CREATE TABLE public.kv2 (id int PRIMARY KEY, extra text);
	CREATE TABLE public.loc (i int);
});
$n1->safe_psql('postgres', q{
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
	SELECT kingres.create_distributed_table('public.kv2', 'id', colocate_with => 'public.kv');
});
# A local, non-distributed table on every node.
$_->safe_psql('postgres', 'CREATE TABLE IF NOT EXISTS public.loc (i int)')
  for ($n2, $n3);

# Three keys owned by each group, found with the bucket function.
my %keys;
for my $g (0 .. 2)
{
	my $list = $n1->safe_psql('postgres', qq{
		SELECT string_agg(k::text, ',' ORDER BY k) FROM (
		  SELECT k FROM generate_series(1, 1000) k
		   WHERE (SELECT m.group_id FROM kingres.bucket_map m
		           WHERE m.colocation_id = 1
		             AND m.bucket = kingres.bucket_for(1, k)) = $g
		   LIMIT 3) s});
	$keys{$g} = [ split /,/, $list ];
}
is(scalar(@{ $keys{$_} }), 3, "found three keys of group $_") for (0 .. 2);

sub bucket_of
{
	my ($k) = @_;
	return $n1->safe_psql('postgres',
		"SELECT kingres.bucket_for('public.kv'::regclass, $k)");
}

# Run SQL and return (exit code, stdout, stderr).
sub run_sql
{
	my ($node, $sql, %extra) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql('postgres', $sql, stdout => \$stdout, stderr => \$stderr, %extra);
	return ($rc, $stdout, $stderr);
}

# ---------------------------------------------------------------------------
# Insert and select by key, from every node, for keys of every group.
# ---------------------------------------------------------------------------
for my $i (0 .. 2)
{
	my $node = $nodes[$i];
	for my $g (0 .. 2)
	{
		my $k = $keys{$g}[$i];
		$node->safe_psql('postgres',
			"INSERT INTO public.kv VALUES ($k, 'from-node-$i-group-$g')");
	}
}
for my $i (0 .. 2)
{
	my $node = $nodes[$i];
	for my $g (0 .. 2)
	{
		for my $j (0 .. 2)
		{
			my $k = $keys{$g}[$j];
			is( $node->safe_psql('postgres',
					"SELECT val FROM public.kv WHERE id = $k"),
				"from-node-$j-group-$g",
				"node $i reads key $k of group $g");
		}
	}
}

# Each row is physically stored only on the node of its group.
for my $g (0 .. 2)
{
	my $k = $keys{$g}[0];
	my $part = sprintf("public.kv_b%04d", bucket_of($k));
	is( $nodes[$g]->safe_psql('postgres', "SELECT count(*) FROM $part WHERE id = $k"),
		'1', "key $k is stored on the node of group $g");
	for my $other (grep { $_ != $g } 0 .. 2)
	{
		is( $nodes[$other]->safe_psql('postgres',
				"SELECT to_regclass('$part') IS NULL"),
			't', "the partition of key $k does not exist on node $other");
	}
}

# ---------------------------------------------------------------------------
# EXPLAIN shows the forwarding, the node and the remote SQL.
# ---------------------------------------------------------------------------
{
	my $k = $keys{1}[0];    # owned by node index 1
	my $plan = $n2->safe_psql('postgres',
		"EXPLAIN SELECT * FROM public.kv WHERE id = $k");
	unlike($plan, qr/Custom Scan/, 'EXPLAIN on the owning node is a local plan');
	$plan = $n3->safe_psql('postgres',
		"EXPLAIN SELECT * FROM public.kv WHERE id = $k");
	like($plan, qr/Custom Scan \(Kingres Remote Statement\)/,
		'EXPLAIN on another node shows the remote statement scan');
	like($plan, qr/Node: \Q@{[ $n2->host ]}:@{[ $n2->port ]}\E \(group 1, bucket @{[ bucket_of($k) ]}\)/,
		'EXPLAIN names the target node');
	like($plan, qr/Remote SQL: SELECT id, val FROM public\.kv WHERE \(id = $k\)/,
		'EXPLAIN shows the remote SQL');
	$plan = $n3->safe_psql('postgres',
		"EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) SELECT * FROM public.kv WHERE id = $k");
	like($plan, qr/Custom Scan \(Kingres Remote Statement\).*actual rows=1/s,
		'EXPLAIN ANALYZE runs the remote statement');
}

# ---------------------------------------------------------------------------
# Update and delete by key, RETURNING, row counts.
# ---------------------------------------------------------------------------
sub rowcount
{
	my ($node, $dml) = @_;
	my ($rc, $out, $err) = run_sql($node, qq{
		DO \$\$ DECLARE n bigint; BEGIN $dml; GET DIAGNOSTICS n = ROW_COUNT;
		RAISE NOTICE 'rows %', n; END \$\$;});
	return $err =~ /rows (\d+)/ ? $1 : "no count: $err";
}

{
	my $k = $keys{2}[0];
	is(rowcount($n1, "UPDATE public.kv SET val = 'updated' WHERE id = $k"), 1,
		'UPDATE of a remote row reports one row');
	is($n2->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $k"),
		'updated', 'the update is visible from another node');
	is($n1->safe_psql('postgres',
			"UPDATE public.kv SET val = val || '!' WHERE id = $k RETURNING id, val"),
		"$k|updated!", 'UPDATE ... RETURNING returns the remote row');
	is(rowcount($n1, "UPDATE public.kv SET val = 'none' WHERE id = 999999"), 0,
		'UPDATE of no row reports zero rows');
	is(rowcount($n1, "DELETE FROM public.kv WHERE id = $k"), 1,
		'DELETE of a remote row reports one row');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k"),
		'0', 'the deleted row is gone');
	is($n1->safe_psql('postgres',
			"INSERT INTO public.kv VALUES ($k, 'again') RETURNING id, val"),
		"$k|again", 'INSERT ... RETURNING returns the remote row');
	is($n1->safe_psql('postgres',
			"DELETE FROM public.kv WHERE id = $k RETURNING val"),
		'again', 'DELETE ... RETURNING returns the remote row');
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($k, 'from-node-0-group-2')");
	my ($rc, $out, $err) = run_sql($n1, "INSERT INTO public.kv VALUES ($keys{2}[1], 'dup')");
	like($err, qr/duplicate key value violates unique constraint/,
		'a remote constraint violation is reported with the remote message');
	is($n1->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $keys{2}[1]"),
		"from-node-1-group-2", 'the row of the failed insert is unchanged');
}

# Aggregates, CTEs, subqueries and ordering on one key go to the owner.
{
	my $k = $keys{1}[1];
	is( $n3->safe_psql('postgres',
			"SELECT count(*), max(val) FROM public.kv WHERE id = $k"),
		"1|from-node-1-group-1", 'aggregate on one key');
	is( $n3->safe_psql('postgres',
			"WITH x AS (SELECT val FROM public.kv WHERE id = $k) SELECT upper(val) FROM x"),
		'FROM-NODE-1-GROUP-1', 'CTE on one key');
	is( $n3->safe_psql('postgres',
			"SELECT val FROM public.kv WHERE id = $k ORDER BY val LIMIT 1"),
		'from-node-1-group-1', 'ORDER BY and LIMIT on one key');
	is( $n3->safe_psql('postgres',
			"SELECT (SELECT val FROM public.kv WHERE id = $k) || '-sub'"),
		'from-node-1-group-1-sub', 'a scalar subquery on one key');
	is( $n3->safe_psql('postgres',
			"SELECT id FROM public.kv WHERE id = $k AND val LIKE 'from%' AND id IN (SELECT id FROM public.kv WHERE id = $k)"),
		$k, 'a restriction among other conditions');
	is( $n3->safe_psql('postgres',
			"SELECT val FROM public.kv WHERE id = ${k}::bigint"),
		'from-node-1-group-1', 'a key of another integer type');
	is( $n3->safe_psql('postgres',
			"SELECT val FROM public.kv WHERE $k = id"),
		'from-node-1-group-1', 'the key on the left side');
	is( $n3->safe_psql('postgres',
			"SELECT val FROM public.kv WHERE id = $k FOR UPDATE"),
		'from-node-1-group-1', 'SELECT FOR UPDATE on one key');
}

# Colocated join: both tables restricted to the same key.
{
	my $k = $keys{0}[1];
	$n2->safe_psql('postgres', "INSERT INTO public.kv2 VALUES ($k, 'extra-$k')");
	is( $n3->safe_psql('postgres',
			"SELECT a.val, b.extra FROM public.kv a JOIN public.kv2 b ON a.id = b.id WHERE a.id = $k AND b.id = $k"),
		"from-node-1-group-0|extra-$k", 'a colocated join restricted on both tables');
	my ($rc, $out, $err) = run_sql($n3,
		"SELECT a.val, b.extra FROM public.kv a JOIN public.kv2 b ON a.id = b.id WHERE a.id = $k");
	is($err, '', 'a join with one unrestricted table runs: the distributed scan reads the other groups');
	is($out, "from-node-1-group-0|extra-$k", 'and gives the same row as the restricted join');
}

# ---------------------------------------------------------------------------
# Statements that span shard groups: SELECT works (the distributed scan,
# t/013_dist_scan.pl compares it with a single node); a write that cannot be
# routed to one group is still refused.
# ---------------------------------------------------------------------------
{
	my ($k0, $k1) = ($keys{0}[0], $keys{1}[0]);
	my $local = sub {
		my ($where) = @_;
		my $n = 0;
		$n += $_->safe_psql('postgres',
			"SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.kv WHERE $where")
		  for @nodes;
		return $n;
	};
	for my $case (
		[ 'a statement without a key', "SELECT count(*) FROM public.kv", $local->('true') ],
		[ 'two keys in different buckets', "SELECT count(*) FROM public.kv WHERE id IN ($k0, $k1)", 2 ],
		[ 'OR of two keys', "SELECT count(*) FROM public.kv WHERE id = $k0 OR id = $k1", 2 ],
		[ 'a range', "SELECT count(*) FROM public.kv WHERE id < 10", $local->('id < 10') ],
	  )
	{
		my ($rc, $out, $err) = run_sql($n3, $case->[1]);
		is($err, '', "runs: $case->[0]");
		is($out, $case->[2], "the answer for the whole table: $case->[0]");
	}
	for my $case (
		[ 'a multi-row INSERT', "INSERT INTO public.kv VALUES (-1, 'a'), (-2, 'b')" ],
		[ 'INSERT ... SELECT', "INSERT INTO public.kv SELECT g, 'x' FROM generate_series(-10, -5) g" ],
	  )
	{
		my ($rc, $out, $err) = run_sql($n3, $case->[1]);
		like($err, qr/statements that span shard groups are not supported yet/,
			"refused: $case->[0]");
	}

	# Two keys that fall in the same bucket are fine.
	my ($ka, $kb) = @{ $keys{1} }[ 0, 1 ];
	my ($ba, $bb) = (bucket_of($ka), bucket_of($kb));
	if ($ba == $bb)
	{
		is( $n3->safe_psql('postgres',
				"SELECT count(*) FROM public.kv WHERE id = $ka OR id = $kb"),
			2, 'keys of one bucket can be combined');
	}
	# Statements on local relations only are never affected.
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.loc"), '0',
		'a local table is not routed');
}

# ---------------------------------------------------------------------------
# Prepared statements with parameters.
# ---------------------------------------------------------------------------
{
	my @ks = map { $keys{ $_ % 3 }[ int($_ / 3) % 3 ] } 0 .. 8;
	my $script = "PREPARE q (int) AS SELECT val FROM public.kv WHERE id = \$1;\n"
	  . join('', map { "EXECUTE q($_);\n" } @ks);
	my $out = $n1->safe_psql('postgres', $script);
	my @got = split /\n/, $out;
	is(scalar(@got), 9, 'nine executions of a prepared SELECT (more than five)');
	my @want = map { $n1->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $_") } @ks;
	is_deeply(\@got, \@want, 'prepared executions return the rows of their keys');

	my $kn = $keys{2}[2];
	$n1->safe_psql('postgres', "DELETE FROM public.kv WHERE id IN ($kn)");
	my $ins = "PREPARE p (int, text) AS INSERT INTO public.kv VALUES (\$1, \$2);\n";
	my @new = map { 5000 + $_ } 0 .. 8;
	my $grpcount = $n1->safe_psql('postgres', "SELECT 1");
	$out = $n2->safe_psql('postgres', $ins . join('', map { "EXECUTE p($_, 'prep-$_');\n" } @new));
	my @back = map { $n3->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $_") } @new;
	is_deeply(\@back, [ map { "prep-$_" } @new ], 'a prepared INSERT with parameters routes every key');

	# A generic plan cannot route: it is refused, and auto mode never uses one.
	my ($rc, $o, $err) = run_sql($n1, "SET plan_cache_mode = force_generic_plan;\n"
		  . "PREPARE g (int) AS SELECT val FROM public.kv WHERE id = \$1;\nEXECUTE g($keys{1}[0]);");
	like($err, qr/a generic plan cannot route a statement/, 'a forced generic plan is refused');
	($rc, $o, $err) = run_sql($n1, "SET plan_cache_mode = force_custom_plan;\n"
		  . "PREPARE g (int) AS SELECT val FROM public.kv WHERE id = \$1;\nEXECUTE g($keys{1}[0]);");
	is($err, '', 'a forced custom plan works');
	like($o, qr/from-node-/, 'a forced custom plan returns the row');

	# Parameters of other types, NULL and text.
	$n1->safe_psql('postgres', 'CREATE TABLE IF NOT EXISTS public.tk (name text PRIMARY KEY, n numeric, ts timestamptz)');
	$n1->safe_psql('postgres',
		"SELECT kingres.create_distributed_table('public.tk', 'name', bucket_count => 6)");
	my $sc = q{
		PREPARE ins (text, numeric, timestamptz) AS INSERT INTO public.tk VALUES ($1, $2, $3);
		SET timezone = 'UTC';
		EXECUTE ins('alpha', 1.250, '2026-03-04 05:06:07+00');
		EXECUTE ins('beta', NULL, '2026-03-04 05:06:07+03');
		EXECUTE ins('gamma''s', 3, NULL);
		PREPARE sel (text) AS SELECT name, n, ts FROM public.tk WHERE name = $1;
		EXECUTE sel('alpha');
		EXECUTE sel('beta');
		EXECUTE sel('gamma''s');
	};
	is( $n2->safe_psql('postgres', $sc),
		"alpha|1.250|2026-03-04 05:06:07+00\nbeta||2026-03-04 02:06:07+00\ngamma's|3|",
		'text keys and numeric, NULL and timestamptz parameters round-trip');
}

# ---------------------------------------------------------------------------
# Transactions.
# ---------------------------------------------------------------------------
{
	my ($ka, $kb) = ($keys{0}[0], $keys{2}[0]);
	is( $n2->safe_psql('postgres',
			"BEGIN; SELECT val FROM public.kv WHERE id = $ka; SELECT val FROM public.kv WHERE id = $kb; COMMIT;"),
		"from-node-0-group-0\nfrom-node-0-group-2",
		'a transaction can read from two other groups');
	is( $n2->safe_psql('postgres',
			"BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT val FROM public.kv WHERE id = $ka; SELECT val FROM public.kv WHERE id = $kb; COMMIT;"),
		"from-node-0-group-0\nfrom-node-0-group-2",
		'a repeatable read transaction can read from two groups');

	# Rollback of a forwarded write.
	my $kw = 7001;
	$kw++ while ($n1->safe_psql('postgres', "SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, $kw)") ne '2');
	my ($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($kw, 'rolled back'); SELECT val FROM public.kv WHERE id = $kw; ROLLBACK;");
	is($out, 'rolled back', 'a forwarded write is visible inside its transaction');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $kw"),
		'0', 'a rolled back forwarded write is gone');
	$n1->safe_psql('postgres', "BEGIN; INSERT INTO public.kv VALUES ($kw, 'committed'); COMMIT;");
	is($n3->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $kw"),
		'committed', 'a committed forwarded write is durable');

	# An error inside a transaction rolls the remote side back too.
	my $kx = $kw + 1;
	$kx++ while ($n1->safe_psql('postgres', "SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, $kx)") ne '2');
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($kx, 'first'); INSERT INTO public.kv VALUES ($kw, 'dup'); COMMIT;");
	like($err, qr/duplicate key value/, 'an error inside a transaction is reported');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $kx"),
		'0', 'the earlier write of the failed transaction was rolled back');

	# Two keys of the same group inside one transaction with a savepoint.
	my @same;
	for my $c (7100 .. 7400)
	{
		push @same, $c
		  if $n1->safe_psql('postgres', "SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, $c)") eq '2';
		last if @same == 2;
	}
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($same[0], 'sp-kept'); SAVEPOINT s1; INSERT INTO public.kv VALUES ($same[1], 'sp-dropped'); ROLLBACK TO s1; COMMIT;");
	is($err, '', 'a savepoint inside a forwarded transaction works');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $same[0]"), '1',
		'the write before the savepoint is kept');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $same[1]"), '0',
		'the write after the savepoint was rolled back');

	# Read-only transactions refuse forwarded writes.
	($rc, $out, $err) = run_sql($n1,
		"BEGIN READ ONLY; INSERT INTO public.kv VALUES ($kx, 'ro'); COMMIT;");
	like($err, qr/read-only transaction/, 'a forwarded write in a read-only transaction is refused');
}

# ---------------------------------------------------------------------------
# Writes on several groups in one transaction (phase 2a: two-phase commit
# replaced the phase 1 limits, which refused all of these).
# ---------------------------------------------------------------------------
{
	my ($ka, $kb) = (8001, 8001);
	$ka++ while ($n1->safe_psql('postgres', "SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, $ka)") ne '1');
	$kb++ while ($n1->safe_psql('postgres', "SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, $kb)") ne '2');
	my $kl = 8001;
	$kl++ while ($n1->safe_psql('postgres', "SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, $kl)") ne '0');

	my ($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kb, 'b'); COMMIT;");
	is($err, '', 'writing on two remote groups in one transaction commits');
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $ka"), '1',
		'the write on the first remote group is there');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $kb"), '1',
		'the write on the second remote group is there');
	$n1->safe_psql('postgres', "DELETE FROM public.kv WHERE id = $ka");
	$n1->safe_psql('postgres', "DELETE FROM public.kv WHERE id = $kb");

	($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($kl, 'local'); INSERT INTO public.kv VALUES ($ka, 'remote'); COMMIT;");
	is($err, '', 'a remote write after a local write commits');
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; DELETE FROM public.kv WHERE id = $ka; DELETE FROM public.kv WHERE id = $kl; COMMIT;");
	is($err, '', 'a local write after a remote write commits');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $ka") . $n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $kl"), '00',
		'both of those writes took effect');
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($ka, 'remote'); INSERT INTO public.loc VALUES (1); COMMIT;");
	is($err, '', 'a local write to an ordinary table after a remote write commits');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.loc"), '1', 'the local write is there');
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $ka"), '1',
		'the remote write is there');
	$n1->safe_psql('postgres', "DELETE FROM public.loc; DELETE FROM public.kv WHERE id = $ka");

	# A failing statement after a remote write rolls both back.
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.kv VALUES ($ka, 'remote'); INSERT INTO public.loc VALUES (1); SELECT 1/0; COMMIT;");
	like($err, qr/division by zero/, 'an error after writes is reported');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.loc"), '0', 'nothing was written locally');
	is($n2->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $ka"), '0',
		'nothing was written remotely either');

	# Reads before, after and between writes are fine.
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; SELECT count(*) FROM public.kv WHERE id = $kb; INSERT INTO public.kv VALUES ($ka, 'one-group'); "
		  . "SELECT val FROM public.kv WHERE id = $kb; SELECT val FROM public.kv WHERE id = $kl; COMMIT;");
	is($err, '', 'reads from other groups around one remote write are allowed');
	$n1->safe_psql('postgres', "DELETE FROM public.kv WHERE id = $ka");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM pg_prepared_xacts"), '0',
		'no prepared transaction is left on the origin');
	is($n2->safe_psql('postgres', "SELECT count(*) FROM pg_prepared_xacts"), '0',
		'no prepared transaction is left on node 2');
}

# ---------------------------------------------------------------------------
# Other statement forms, forwarded from node 1 to a key of group 1.
# ---------------------------------------------------------------------------
{
	my ($k, $k2) = ($keys{1}[1], $keys{1}[2]);
	$n1->safe_psql('postgres', "INSERT INTO public.kv2 VALUES ($k, 'x-$k') ON CONFLICT DO NOTHING");
	is( $n1->safe_psql('postgres',
			"INSERT INTO public.kv VALUES ($k, '+new') ON CONFLICT (id) DO UPDATE SET val = kv.val || excluded.val RETURNING val"),
		'from-node-1-group-1+new', 'INSERT ... ON CONFLICT DO UPDATE ... RETURNING');
	is( $n1->safe_psql('postgres',
			"UPDATE public.kv a SET val = b.extra FROM public.kv2 b WHERE a.id = $k AND b.id = $k AND a.id = b.id RETURNING a.val"),
		"x-$k", 'UPDATE ... FROM a colocated table, both restricted');
	is( $n1->safe_psql('postgres',
			"SELECT val, row_number() OVER () FROM public.kv WHERE id = $k"),
		"x-$k|1", 'a window function');
	is( $n1->safe_psql('postgres',
			"SELECT DISTINCT length(val) FROM public.kv WHERE id = $k"),
		length("x-$k"), 'SELECT DISTINCT');
	is( $n1->safe_psql('postgres',
			"WITH d AS (DELETE FROM public.kv WHERE id = $k2 RETURNING val) SELECT val FROM d"),
		'from-node-2-group-1', 'a data-modifying CTE');
	is( $n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = $k2"), '0',
		'the CTE deleted the row');
	$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($k2, 'from-node-2-group-1')");
	is( $n1->safe_psql('postgres',
			"DELETE FROM public.kv2 USING public.kv WHERE kv2.id = $k AND kv.id = $k AND kv2.id = kv.id RETURNING kv2.extra"),
		"x-$k", 'DELETE ... USING a colocated table');
	is( $n1->safe_psql('postgres',
			"SELECT v.x FROM (VALUES (1), (2)) v(x), public.kv WHERE kv.id = $k ORDER BY v.x"),
		"1\n2", 'a VALUES list joined with a restricted table');
	my ($rc, $out, $err) = run_sql($n1,
		"SELECT * FROM public.kv, public.loc WHERE kv.id = $k");
	like($err, qr/combine distributed tables of another shard group with local tables/,
		'a statement mixing a remote distributed table and a local table is refused');
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; DECLARE c SCROLL CURSOR FOR SELECT * FROM public.kv WHERE id = $k; ROLLBACK;");
	like($err, qr/scrollable cursors/, 'a scrollable cursor on a remote statement is refused');
	is( $n1->safe_psql('postgres',
			"BEGIN; DECLARE c CURSOR FOR SELECT val FROM public.kv WHERE id = $k; FETCH 1 FROM c; CLOSE c; COMMIT;"),
		"x-$k", 'a plain cursor on a remote statement works');
}

# ---------------------------------------------------------------------------
# Loop prevention and the routing switch.
# ---------------------------------------------------------------------------
{
	my $k = $keys{0}[0];    # not on node 2 (group 1)
	my $b = bucket_of($k);
	my ($rc, $out, $err);

	# A forwarded session is one that was opened as such: the flag is fixed at
	# connection time (kingres_shard.forwarded cannot be set afterwards).
	{
		local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on';
		($rc, $out, $err) = run_sql($n2,
			"\\set VERBOSITY verbose\nSELECT * FROM public.kv WHERE id = $k;");
		like($err, qr/ERROR:\s+KS001: bucket $b of public\.kv is not on this node \(stale map version \d+\)/,
			'a forwarded session raises KS001 for a bucket that is not local');
		($rc, $out, $err) = run_sql($n2,
			"SELECT * FROM public.kv WHERE id = $keys{1}[0];");
		is($err, '', 'a forwarded session runs a local bucket');
		($rc, $out, $err) = run_sql($n2,
			"\\set VERBOSITY verbose\nSELECT count(*) >= 0 FROM public.kv;");
		like($err, qr/ERROR:\s+KS001: cannot prove that the statement touches only buckets of this node/,
			'a forwarded session refuses a statement that spans groups instead of answering from the local buckets');
		is($out, '', 'and returns nothing');
	}

	($rc, $out, $err) = run_sql($n2,
		"SET kingres_shard.enable_routing = off;\nINSERT INTO public.kv VALUES ($k, 'x');");
	like($err, qr/no partition of relation "kv" found for row/,
		'with routing off, a row of another group has no partition here');
	($rc, $out, $err) = run_sql($n2,
		"SET kingres_shard.enable_routing = off;\nSELECT count(*) FROM public.kv WHERE id = $k;");
	is($out, '0', 'with routing off, a read of another group finds no rows');
}

# ---------------------------------------------------------------------------
# Stale map: retry after reloading the catalog from the map group.
# ---------------------------------------------------------------------------
{
	my $k = $keys{0}[0];    # owned by node 1 (group 0)
	my $b = bucket_of($k);

	# Node 3 wrongly believes that group 1 owns the bucket.
	$n3->safe_psql('postgres',
		"UPDATE kingres.bucket_map SET group_id = 1 WHERE colocation_id = 1 AND bucket = $b");
	is($n3->safe_psql('postgres',
			"SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"),
		'1', 'node 3 has a stale bucket map');

	is($n3->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $k"),
		'from-node-0-group-0', 'a read with a stale map is retried and succeeds');
	is($n3->safe_psql('postgres',
			"SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"),
		'0', 'the retry reloaded the bucket map of node 3');

	# The same for a write.
	my $kw = 9001;
	$kw++ while ($n1->safe_psql('postgres', "SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, $kw)") ne '0');
	my $bw = bucket_of($kw);
	$n3->safe_psql('postgres',
		"UPDATE kingres.bucket_map SET group_id = 2 WHERE colocation_id = 1 AND bucket = $bw");
	# group 2 is node 3 itself: the statement would run locally, where the
	# partition does not exist.
	my ($rc, $out, $err) = run_sql($n3, "INSERT INTO public.kv VALUES ($kw, 'stale-write')");
	like($err, qr/no partition of relation "kv" found for row/,
		'a stale map that points at the local node cannot be detected by the node itself');
	$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');

	$n3->safe_psql('postgres',
		"UPDATE kingres.bucket_map SET group_id = 1 WHERE colocation_id = 1 AND bucket = $bw");
	($rc, $out, $err) = run_sql($n3, "INSERT INTO public.kv VALUES ($kw, 'stale-write') RETURNING val");
	is($err, '', 'a write with a stale map is retried');
	is($out, 'stale-write', 'the retried write returned its row');
	is($n1->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $kw"),
		'stale-write', 'the retried write landed on the right node');

	# In a transaction that already used the connection the retry is not
	# possible, and the error is passed on.
	my $k1 = $keys{1}[0];
	$n3->safe_psql('postgres',
		"UPDATE kingres.bucket_map SET group_id = 1 WHERE colocation_id = 1 AND bucket = $b");
	($rc, $out, $err) = run_sql($n3,
		"BEGIN; SELECT val FROM public.kv WHERE id = $k1; SELECT val FROM public.kv WHERE id = $k; COMMIT;");
	like($err, qr/bucket $b of public\.kv is not on this node/,
		'a stale map inside a transaction that already used the connection is an error');
	$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
	is($n3->safe_psql('postgres',
			"SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"),
		'0', 'sync_catalog repairs the stale map');
}

# ---------------------------------------------------------------------------
# Data types survive the trip through a remote statement.
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.typed (id int PRIMARY KEY, ts timestamptz, n numeric(12,4), b bytea,
		                           j jsonb, a text[], f float8, d date, iv interval, u uuid, bo bool);
		SELECT kingres.create_distributed_table('public.typed', 'id', bucket_count => 6);
	});
	my $k = 1;
	$k++ while ($n1->safe_psql('postgres', "SELECT m.group_id FROM kingres.bucket_map m JOIN kingres.dist_table d USING (colocation_id) WHERE d.relid = 'public.typed'::regclass AND m.bucket = kingres.bucket_for('public.typed'::regclass, $k)") ne '2');
	$n1->safe_psql('postgres', qq{
		INSERT INTO public.typed VALUES ($k, '2026-11-14 12:34:56.789+02', 12345.6789, '\\xdeadbeef',
		  '{"a": [1, 2, {"b": null}]}', ARRAY['x', 'y z', NULL], 0.1, '2026-02-28', '1 year 2 mons 3 days 04:05:06',
		  'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', true)});
	my $remote = $n1->safe_psql('postgres', "SELECT * FROM public.typed WHERE id = $k");
	my $direct = $n3->safe_psql('postgres',
		"SELECT * FROM public.typed_b" . sprintf('%04d', $n1->safe_psql('postgres', "SELECT kingres.bucket_for('public.typed'::regclass, $k)")) . " WHERE id = $k");
	is($remote, $direct, 'every column type reads back the same through the remote path');
	like($remote, qr/12345\.6789\|\\xdeadbeef/, 'numeric and bytea values are intact');
}

# ---------------------------------------------------------------------------
# A restarted node: the cached connection to it is dead and is replaced.
# ---------------------------------------------------------------------------
{
	my $k = $keys{1}[0];
	# One long-lived session on node 1, so its connection cache survives.
	my $session = $n1->background_psql('postgres');
	my $before = $session->query_safe("SELECT val FROM public.kv WHERE id = $k");
	is($before, $n2->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $k"),
		'a read before the restart');
	$n2->restart;
	my $after = $session->query_safe("SELECT val FROM public.kv WHERE id = $k");
	is($after, $before, 'the same session routes again after the target node restarted');
	$session->quit;
}

foreach my $n (@nodes)
{
	$n->stop;
}

done_testing();
