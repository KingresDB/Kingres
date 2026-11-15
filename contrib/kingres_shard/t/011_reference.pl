# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# Reference tables (design/shard-phase2a.md, section 3): a full copy on every
# node, reads always local, writes on every node in one distributed
# transaction, rows equal everywhere after writes from any node, rollback,
# rejection of writes that would differ between nodes, routing of a join with
# a distributed table, a new node receiving the tables.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster keys_of_group run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('rf', 3);
my @nodes = ($n1, $n2, $n3);

$n1->safe_psql('postgres', q{
	CREATE TABLE public.ref_a (id int PRIMARY KEY, name text, n numeric);
	INSERT INTO public.ref_a VALUES (1, 'one', 1.5), (2, 'two', NULL), (3, 'three', 3.25);
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
});
$n1->safe_psql('postgres', 'SELECT kingres.create_reference_table(\'public.ref_a\')');

sub table_text
{
	my ($node, $table) = @_;
	return $node->safe_psql('postgres',
		"SELECT coalesce(string_agg(t::text, ',' ORDER BY t.id), '') FROM public.$table t");
}

sub assert_same_everywhere
{
	my ($label, $table, @list) = @_;
	@list = @nodes unless @list;
	my $want = table_text($list[0], $table);
	is(table_text($_, $table), $want, "$label: $_->name has the same rows of $table") for @list[ 1 .. $#list ];
	return $want;
}

# ---------------------------------------------------------------------------
# Creation: the rows and the catalog are on every node.
# ---------------------------------------------------------------------------
is(table_text($n1, 'ref_a'), '(1,one,1.5),(2,two,),(3,three,3.25)', 'the rows are on the origin');
assert_same_everywhere('after create_reference_table', 'ref_a');
for my $n (@nodes)
{
	is($n->safe_psql('postgres',
			"SELECT method || ',' || coalesce(dist_column::text, '-') || ',' || coalesce(colocation_id::text, '-') FROM kingres.dist_table WHERE relid = 'public.ref_a'::regclass"),
		'reference,-,-', "the catalog of " . $n->name . " knows the reference table");
	is($n->safe_psql('postgres', "SELECT relkind FROM pg_class WHERE oid = 'public.ref_a'::regclass"),
		'r', $n->name . " has an ordinary table");
}
is(prepared_count($_), '0', "nothing is prepared on $_->name") for @nodes;

my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.bucket_for('public.ref_a'::regclass, 1)");
like($err, qr/reference table and has no buckets/, 'a reference table has no buckets');

# ---------------------------------------------------------------------------
# Writes from any node reach every node.
# ---------------------------------------------------------------------------
$n2->safe_psql('postgres', "INSERT INTO public.ref_a VALUES (10, 'ten', 10)");
assert_same_everywhere('INSERT from node 2', 'ref_a');
$n3->safe_psql('postgres', "UPDATE public.ref_a SET name = upper(name) WHERE id < 3");
assert_same_everywhere('UPDATE from node 3', 'ref_a');
is(table_text($n1, 'ref_a'), '(1,ONE,1.5),(2,TWO,),(3,three,3.25),(10,ten,10)', 'the UPDATE took effect');
is($n1->safe_psql('postgres', "DELETE FROM public.ref_a WHERE id = 10 RETURNING name"), 'ten',
	'DELETE ... RETURNING returns the row once');
assert_same_everywhere('DELETE from node 1', 'ref_a');

# The affected row count is that of one copy.
is($n2->safe_psql('postgres', "UPDATE public.ref_a SET n = 7 WHERE id IN (1, 2) RETURNING id"), "1\n2",
	'a multi-row UPDATE ... RETURNING returns the rows of one copy');
is($n3->safe_psql('postgres', "UPDATE public.ref_a SET n = 9 WHERE id IN (1, 2) RETURNING id"), "1\n2",
	'and from another node');
assert_same_everywhere('after the updates', 'ref_a');

# INSERT ... SELECT from another reference table, ON CONFLICT, parameters.
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ref_b (id int PRIMARY KEY, name text);
	SELECT kingres.create_reference_table('public.ref_b');
});
$n2->safe_psql('postgres', "INSERT INTO public.ref_b SELECT id, name FROM public.ref_a");
assert_same_everywhere('INSERT ... SELECT', 'ref_b');
$n3->safe_psql('postgres', "INSERT INTO public.ref_b VALUES (1, 'conflict') ON CONFLICT (id) DO UPDATE SET name = excluded.name");
assert_same_everywhere('ON CONFLICT', 'ref_b');
is($n2->safe_psql('postgres', "SELECT name FROM public.ref_b WHERE id = 1"), 'conflict', 'the conflict clause ran');
my $prep = $n3->safe_psql('postgres', q{
	PREPARE ins (int, text) AS INSERT INTO public.ref_b VALUES ($1, $2);
	EXECUTE ins(20, 'p1'); EXECUTE ins(21, 'p2'); EXECUTE ins(22, 'p3'); EXECUTE ins(23, 'p4');
	EXECUTE ins(24, 'p5'); EXECUTE ins(25, 'p6'); EXECUTE ins(26, 'p7');
	SELECT count(*) FROM public.ref_b WHERE id >= 20});
is($prep, '7', 'a prepared write executed seven times');
assert_same_everywhere('prepared writes', 'ref_b');

# ---------------------------------------------------------------------------
# Rollback.
# ---------------------------------------------------------------------------
($rc, $out, $err) = run_sql($n2, "BEGIN; INSERT INTO public.ref_a VALUES (30, 'x', 1); UPDATE public.ref_a SET name = 'y' WHERE id = 1; ROLLBACK;");
is($err, '', 'a rolled back write');
assert_same_everywhere('after ROLLBACK', 'ref_a');
is($n1->safe_psql('postgres', "SELECT count(*) FROM public.ref_a WHERE id = 30"), '0', 'the row is not there');
($rc, $out, $err) = run_sql($n2, "BEGIN; INSERT INTO public.ref_a VALUES (31, 'x', 1); SAVEPOINT s; INSERT INTO public.ref_a VALUES (32, 'z', 1); ROLLBACK TO s; COMMIT;");
is($err, '', 'a write inside a savepoint');
is($n1->safe_psql('postgres', "SELECT string_agg(id::text, ',' ORDER BY id) FROM public.ref_a WHERE id > 30"), '31',
	'only the write before the rolled back savepoint remains');
assert_same_everywhere('after the savepoint', 'ref_a');
$n1->safe_psql('postgres', "DELETE FROM public.ref_a WHERE id = 31");
is(prepared_count($_), '0', "nothing is prepared on $_->name") for @nodes;

# ---------------------------------------------------------------------------
# Writes that would give different rows on different nodes are refused.
# ---------------------------------------------------------------------------
for my $case (
	[ 'random()', "INSERT INTO public.ref_a VALUES (40, 'r', random())" ],
	[ 'now()', "INSERT INTO public.ref_a VALUES (41, now()::text, 1)" ],
	[ 'nextval', "CREATE SEQUENCE IF NOT EXISTS public.sq; INSERT INTO public.ref_a VALUES (nextval('public.sq'), 'q', 1)" ],
	[ 'a volatile UPDATE', "UPDATE public.ref_a SET n = random()" ],
	[ 'a local table', "CREATE TABLE IF NOT EXISTS public.loc (i int); INSERT INTO public.ref_a SELECT i, 'l', 1 FROM public.loc" ],
	)
{
	($rc, $out, $err) = run_sql($n2, $case->[1]);
	like($err, qr/cannot be run on every node/, "refused: $case->[0]");
}
is($n1->safe_psql('postgres', "SELECT count(*) FROM public.ref_a WHERE id >= 40"), '0', 'nothing of those statements was written');

# A sequence default is a volatile expression too.
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ref_s (id serial PRIMARY KEY, v text);
	SELECT kingres.create_reference_table('public.ref_s');
});
($rc, $out, $err) = run_sql($n2, "INSERT INTO public.ref_s (v) VALUES ('x')");
like($err, qr/cannot be run on every node/, 'a sequence default is refused');
($rc, $out, $err) = run_sql($n2, "INSERT INTO public.ref_s VALUES (1, 'x')");
is($err, '', 'an explicit key works');
assert_same_everywhere('serial table', 'ref_s');

# A reference write cannot read a distributed table.
$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES (1, 'one-val')");
($rc, $out, $err) = run_sql($n1, "UPDATE public.ref_a SET name = (SELECT val FROM public.kv WHERE id = 1) WHERE id = 1");
like($err, qr/writes reference table .* and reads a distributed table is not supported yet/,
	'a write to a reference table that reads a distributed table is refused');

# ---------------------------------------------------------------------------
# Reference tables in queries on distributed tables.
# ---------------------------------------------------------------------------
{
	my %k;
	$k{$_} = [ keys_of_group($n1, 1, $_, 3, 100) ] for (0 .. 2);
	for my $g (0 .. 2)
	{
		$n1->safe_psql('postgres', "INSERT INTO public.ref_b VALUES ($_, 'r$_') ON CONFLICT DO NOTHING") for @{ $k{$g} };
		$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'v$_') ON CONFLICT (id) DO UPDATE SET val = excluded.val") for @{ $k{$g} };
	}
	my $key = $k{2}[0];
	my $sql = "SELECT kv.val, b.name FROM public.kv JOIN public.ref_b b ON b.id = kv.id WHERE kv.id = $key";
	is($n1->safe_psql('postgres', $sql), "v$key|r$key", 'a join of a distributed table with a reference table, by key');
	like($n1->safe_psql('postgres', "EXPLAIN (COSTS OFF) $sql"), qr/Kingres Remote Statement/,
		'the join is routed to the one group that owns the key');
	like($n1->safe_psql('postgres', "EXPLAIN (VERBOSE, COSTS OFF) $sql"), qr/Remote SQL:.*ref_b/s,
		'the reference table is part of the shipped statement');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.ref_b"), $n1->safe_psql('postgres', "SELECT count(*) FROM public.ref_b"),
		'a read of a reference table is local on any node');
	# A transaction that writes the reference table and a distributed table.
	my ($ka, $kb) = ($k{1}[0], $k{2}[1]);
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.ref_a VALUES (50, 'tx', 1); UPDATE public.kv SET val = 'tx' WHERE id = $ka; UPDATE public.kv SET val = 'tx' WHERE id = $kb; COMMIT;");
	is($err, '', 'a reference write and two distributed writes in one transaction');
	assert_same_everywhere('after the mixed transaction', 'ref_a');
	is($n1->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $kb"), 'tx', 'the distributed write is there');
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; INSERT INTO public.ref_a VALUES (51, 'tx2', 1); UPDATE public.kv SET val = 'tx2' WHERE id = $ka; SELECT 1/0; COMMIT;");
	like($err, qr/division by zero/, 'a failure after the writes');
	is($n1->safe_psql('postgres', "SELECT count(*) FROM public.ref_a WHERE id = 51"), '0', 'the reference write is gone');
	is($n1->safe_psql('postgres', "SELECT val FROM public.kv WHERE id = $ka"), 'tx', 'the distributed write is gone');
}

# ---------------------------------------------------------------------------
# Copies that differ are detected.
# ---------------------------------------------------------------------------
{
	$n3->safe_psql('postgres', "SET kingres_shard.enable_routing = off; DELETE FROM public.ref_a WHERE id = 3");
	($rc, $out, $err) = run_sql($n1, "UPDATE public.ref_a SET name = name WHERE id = 3");
	like($err, qr/the copies of a reference table differ/, 'a statement that affects a different number of rows on two nodes fails');
	$n3->safe_psql('postgres', "SET kingres_shard.enable_routing = off; INSERT INTO public.ref_a VALUES (3, 'three', 3.25)");
	assert_same_everywhere('after repairing the copy', 'ref_a');
}

# ---------------------------------------------------------------------------
# Crash windows of a reference write.
# ---------------------------------------------------------------------------
sub crash
{
	my ($point, $sql) = @_;
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:kill';\n$sql");
	isnt($rc, 0, "the origin dies at $point");
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
}
{
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	crash('after_prepare', "INSERT INTO public.ref_a VALUES (60, 'crash', 1)");
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2', 'recovery rolls back the two remote parts');
	is($_->safe_psql('postgres', "SELECT count(*) FROM public.ref_a WHERE id = 60"), '0', "$_->name has no row 60") for @nodes;
	crash('after_commit', "INSERT INTO public.ref_a VALUES (61, 'crash', 1)");
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2', 'recovery commits the two remote parts');
	is($_->safe_psql('postgres', "SELECT count(*) FROM public.ref_a WHERE id = 61"), '1', "$_->name has row 61") for @nodes;
	assert_same_everywhere('after crash recovery', 'ref_a');
}

# ---------------------------------------------------------------------------
# create_reference_table: refusals, and a new node receives the tables.
# ---------------------------------------------------------------------------
{
	($rc, $out, $err) = run_sql($n2, "SELECT kingres.create_reference_table('public.ref_a')");
	like($err, qr/only run on the map group primary/, 'create_reference_table is for the map group primary');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_reference_table('public.ref_a')");
	like($err, qr/already distributed or a reference table/, 'a reference table cannot be made one twice');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_reference_table('public.kv')");
	like($err, qr/already distributed or a reference table/, 'a distributed table cannot be made a reference table');
	$n1->safe_psql('postgres', 'CREATE TABLE public.has_fk (id int PRIMARY KEY, a int REFERENCES public.ref_a(id))');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_reference_table('public.has_fk')");
	like($err, qr/foreign key constraint .* is not supported yet/, 'a table with a foreign key is refused');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.ref_b', 'id')");
	like($err, qr/already distributed/, 'a reference table cannot be distributed');
	$n1->safe_psql('postgres', 'CREATE TABLE public.plain1 (id int PRIMARY KEY)');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.plain1', 'id', colocate_with => 'public.ref_a')");
	like($err, qr/not a distributed table/, 'a reference table is not a colocation target');

	my $n4 = make_node('rf4');
	$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('%s', %d)", $n4->host, $n4->port));
	is(table_text($n4, 'ref_a'), table_text($n1, 'ref_a'), 'a new node receives the rows of a reference table');
	is(table_text($n4, 'ref_b'), table_text($n1, 'ref_b'), 'and of the other reference table');
	is($n4->safe_psql('postgres', "SELECT count(*) FROM kingres.dist_table WHERE method = 'reference'"), '3',
		'the new node knows the reference tables');
	$n4->safe_psql('postgres', "INSERT INTO public.ref_a VALUES (70, 'from-4', 1)");
	assert_same_everywhere('a write from the new node', 'ref_a', $n1, $n2, $n3, $n4);
	$n1->safe_psql('postgres', "DELETE FROM public.ref_a WHERE id = 70");
	assert_same_everywhere('a delete reaches the new node', 'ref_a', $n1, $n2, $n3, $n4);
}

done_testing();
