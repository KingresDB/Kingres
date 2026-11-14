# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# UPDATE and DELETE that span shard groups (design/shard-phase2a.md, section 6),
# compared with a single node holding the same data: the number of rows
# affected and the rows that remain are the same; RETURNING gives the rows of
# all groups; a statement that cannot be shipped whole is refused, and a failure
# on one group undoes the work on the others.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql prepared_count keys_of_group);
use KingresShardCompare;

my $c = KingresShardCompare->new('dw');
my @nodes = $c->nodes;

$c->on_both(q{
	CREATE TABLE public.t1 (id int PRIMARY KEY, g int, v text, n numeric(10,2));
	CREATE TABLE public.t2 (id int PRIMARY KEY, a int);
	CREATE TABLE public.refx (id int PRIMARY KEY, name text);
});
$c->on_cluster(q{
	SELECT kingres.create_distributed_table('public.t1', 'id', bucket_count => 12);
	SELECT kingres.create_distributed_table('public.t2', 'id', colocate_with => 'public.t1');
	SELECT kingres.create_reference_table('public.refx');
});
my $fn = q{CREATE FUNCTION public.plus_one(x int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT x + 1'};
$c->on_every_node($fn);
$c->on_single($fn);

sub rows_sql
{
	my ($table, @rows) = @_;
	return join('', map { "INSERT INTO public.$table VALUES ($_);\n" } @rows);
}

my @t1 = map { sprintf("%d, %d, %s, %.2f", $_, $_ % 7, ($_ % 11 == 0 ? 'NULL' : "'v" . ($_ % 13) . "'"), $_ * 1.25) } (1 .. 180);
my @t2 = map { sprintf("%d, %d", $_, $_ % 5) } (1 .. 60);
my @refx = map { sprintf("%d, 'n%d'", $_, $_) } (0 .. 6);
$c->script_on_both(rows_sql('t1', @t1) . rows_sql('t2', @t2) . rows_sql('refx', @refx));

my @state = (
	[ 'rows of t1', 'SELECT * FROM public.t1', 0 ],
	[ 'aggregates of t1', 'SELECT count(*), sum(n), count(v), min(v), max(v), sum(g) FROM public.t1', 0 ],
);
sub check_state
{
	my ($label) = @_;
	$c->compare("$label: $_->[0]", $_->[1], ordered => $_->[2]) for @state;
}

check_state('initially');

# ---------------------------------------------------------------------------
# UPDATE.
# ---------------------------------------------------------------------------
$c->compare_dml('UPDATE with a filter', 'UPDATE public.t1 SET n = n + 1 WHERE g = 3');
check_state('after UPDATE with a filter');
$c->compare_dml('UPDATE of every row', q{UPDATE public.t1 SET v = coalesce(v, 'none') || '!'});
check_state('after UPDATE of every row');
$c->compare_dml('UPDATE with built-in functions', q{UPDATE public.t1 SET v = upper(v), n = round(n * 1.1, 1) WHERE id % 2 = 0});
check_state('after UPDATE with built-in functions');
$c->compare_dml('UPDATE with CASE', q{UPDATE public.t1 SET g = CASE WHEN g > 4 THEN 0 ELSE g + 1 END});
check_state('after UPDATE with CASE');
$c->compare_dml('UPDATE with a reference table', q{UPDATE public.t1 SET v = refx.name FROM public.refx WHERE refx.id = t1.g AND t1.id < 100});
check_state('after UPDATE with a reference table');
$c->compare_dml('UPDATE with a reference subselect', q{UPDATE public.t1 SET n = (SELECT count(*) FROM public.refx) WHERE g = 2});
check_state('after UPDATE with a reference subselect');
$c->compare_dml('UPDATE that matches nothing', 'UPDATE public.t1 SET n = 0 WHERE g = 99');
check_state('after UPDATE that matches nothing');

# RETURNING gives the rows of all groups.
for my $node (@nodes)
{
	my $sql = "UPDATE public.t1 SET n = n + 0.5 WHERE g = 1 RETURNING id, n";
	my ($rs, $os) = run_sql($c->single, $sql);
	my ($rd, $od, $ed) = run_sql($node, $sql);
	is($rd, 0, 'UPDATE ... RETURNING on ' . $node->name) or diag($ed);
	is(join("\n", sort split /\n/, $od), join("\n", sort split /\n/, $os), 'the rows of all groups are returned');
}
check_state('after UPDATE ... RETURNING');

# A prepared statement, run enough times for a generic plan to be considered.
{
	my $script = "PREPARE u (int, int) AS UPDATE public.t1 SET n = n + \$1 WHERE g = \$2;\n"
	  . join('', map { "EXECUTE u(1, " . ($_ % 7) . ");\n" } (1 .. 9));
	my ($rs, $os, $es) = run_sql($c->single, $script);
	my ($rd, $od, $ed) = run_sql($nodes[1], $script);
	is($rd, 0, 'a prepared UPDATE run 9 times') or diag($ed);
	check_state('after the prepared UPDATE');
}

# ---------------------------------------------------------------------------
# DELETE.
# ---------------------------------------------------------------------------
$c->compare_dml('DELETE with a filter', 'DELETE FROM public.t1 WHERE g = 5');
check_state('after DELETE with a filter');
{
	my $sql = q{DELETE FROM public.t1 WHERE id % 9 = 0 RETURNING id, v};
	my ($rs, $os) = run_sql($c->single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[2], $sql);
	is($rd, 0, 'DELETE ... RETURNING') or diag($ed);
	is(join("\n", sort split /\n/, $od), join("\n", sort split /\n/, $os), 'the deleted rows of all groups are returned');
}
check_state('after DELETE ... RETURNING');
$c->compare_dml('DELETE with a reference table', q{DELETE FROM public.t1 USING public.refx WHERE refx.id = t1.g AND refx.name = 'n1'});
check_state('after DELETE with a reference table');

# ---------------------------------------------------------------------------
# What cannot be shipped is refused, and nothing changes.
# ---------------------------------------------------------------------------
my $before_t1 = $c->on_cluster('SELECT md5(string_agg(t::text, \',\' ORDER BY id)) FROM public.t1 t');
for my $case (
	[ 'a function that is not built in', 'UPDATE public.t1 SET g = public.plus_one(g)', qr/not shippable.*not built in and immutable/s ],
	[ 'a volatile function', 'UPDATE public.t1 SET n = random()', qr/not shippable/ ],
	[ 'now()', 'UPDATE public.t1 SET n = extract(epoch FROM now())', qr/not shippable/ ],
	[ 'a subselect on the same table', 'DELETE FROM public.t1 WHERE n > (SELECT avg(n) FROM public.t1)', qr/reads a distributed table other than as its target/ ],
	[ 'a join with another distributed table', 'UPDATE public.t1 SET n = 0 FROM public.t2 WHERE t2.id = t1.id', qr/reads a distributed table other than as its target/ ],
	[ 'USING another distributed table', 'DELETE FROM public.t1 USING public.t2 WHERE t2.id = t1.id', qr/reads a distributed table other than as its target/ ],
	[ 'a subselect on another distributed table', 'DELETE FROM public.t1 WHERE g IN (SELECT a FROM public.t2)', qr/reads a distributed table other than as its target/ ],
	[ 'the distribution column', 'UPDATE public.t1 SET id = id + 1000', qr/UPDATE of the distribution column/ ],
	[ 'the distribution column with a key', 'UPDATE public.t1 SET id = 5 WHERE id = 3', qr/UPDATE of the distribution column/ ],
	)
{
	$c->compare_dml($case->[0], $case->[1], expect_error => $case->[2]);
}
is($c->on_cluster('SELECT md5(string_agg(t::text, \',\' ORDER BY id)) FROM public.t1 t'), $before_t1, 'the refused statements changed nothing');
{
	my ($rc, $out, $err) = run_sql($nodes[0], 'UPDATE public.t1 SET n = 0 FROM public.t2 WHERE t2.id = t1.id');
	like($err, qr/HINT:.*Restrict it by the distribution column/s, 'the hint names the way out');
	($rc, $out, $err) = run_sql($nodes[0], 'UPDATE public.t1 SET n = 1 WHERE g = 2 FOR UPDATE');
	isnt($rc, 0, 'a syntax error is a syntax error');
}

# ---------------------------------------------------------------------------
# Atomicity: a failure on one group undoes the others.
# ---------------------------------------------------------------------------
{
	$c->on_both('ALTER TABLE public.t1 ADD CONSTRAINT n_small CHECK (n < 5000)');
	check_state('with the constraint');
	# A key of the last group (node 3): the groups before it have applied the change when it fails.
	my ($k) = keys_of_group($nodes[0], 1, 2, 1, 1);
	$c->compare_dml('a failure on the last group', "UPDATE public.t1 SET n = n + 1, v = 'changed' || (CASE WHEN id = $k THEN 1/0 ELSE 0 END)::text",
		expect_error => qr/division by zero/, single_fails => 1);
	check_state('after the failed UPDATE');
	is(prepared_count($_), '0', "nothing is prepared on $_->name") for @nodes;
}

# ---------------------------------------------------------------------------
# Transactions and crashes.
# ---------------------------------------------------------------------------
{
	my ($rs) = run_sql($c->single, "BEGIN; UPDATE public.t1 SET v = 'tx'; DELETE FROM public.t1 WHERE g = 0; ROLLBACK;");
	my ($rd, $od, $ed) = run_sql($nodes[1], "BEGIN; UPDATE public.t1 SET v = 'tx'; DELETE FROM public.t1 WHERE g = 0; ROLLBACK;");
	is($rd, 0, 'a rolled back transaction') or diag($ed);
	check_state('after the rollback');

	($rs) = run_sql($c->single, "BEGIN; UPDATE public.t1 SET v = 'tx2' WHERE g = 1; DELETE FROM public.t1 WHERE g = 2; SELECT count(*) FROM public.t1; COMMIT;");
	my ($r) = run_sql($nodes[2], "BEGIN; UPDATE public.t1 SET v = 'tx2' WHERE g = 1; DELETE FROM public.t1 WHERE g = 2; SELECT count(*) FROM public.t1; COMMIT;");
	is($r, 0, 'a transaction with several statements spanning groups');
	check_state('after the transaction');

	# A crash after the prepare: rolled back; after the commit decision: committed.
	my $count = $c->on_cluster('SELECT count(*) FROM public.t1');
	my ($rc, $out, $err) = run_sql($nodes[0], "SET kingres_shard.fault_injection = 'after_prepare:kill'; DELETE FROM public.t1 WHERE g = 3;");
	isnt($rc, 0, 'the origin dies after prepare');
	$nodes[0]->stop('immediate', fail_ok => 1);
	$nodes[0]->start;
	is($nodes[0]->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2', 'recovery resolves the prepared DELETE');
	is($c->on_cluster('SELECT count(*) FROM public.t1'), $count, 'the DELETE was rolled back everywhere');

	($rc, $out, $err) = run_sql($nodes[0], "SET kingres_shard.fault_injection = 'after_commit:kill'; DELETE FROM public.t1 WHERE g = 3;");
	isnt($rc, 0, 'the origin dies after the commit');
	$nodes[0]->stop('immediate', fail_ok => 1);
	$nodes[0]->start;
	is($nodes[0]->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2', 'recovery commits the DELETE');
	is($c->on_cluster('SELECT count(*) FROM public.t1 WHERE g = 3'), '0', 'the DELETE is committed everywhere');
	# Do the same to the single node, to keep the comparison going.
	$c->on_single('DELETE FROM public.t1 WHERE g = 3');
	check_state('after crash recovery');
}

# ---------------------------------------------------------------------------
# A session that claims to be a forwarded one does not get a statement run on
# the buckets of one node unless it carries the marker of the fan-out.
# ---------------------------------------------------------------------------
{
	my $psql = sub {
		my ($sql) = @_;
		local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on';
		return run_sql($nodes[1], $sql);
	};
	my ($rc, $out, $err) = $psql->('UPDATE public.t1 SET n = 0');
	like($err, qr/cannot prove that the statement touches only buckets of this node/, 'an unmarked write in a forwarded session is refused');
	($rc, $out, $err) = $psql->('SELECT count(*) FROM public.t1');
	like($err, qr/cannot prove that the statement touches only buckets of this node/, 'an unmarked read in a forwarded session is refused');
	check_state('after the refusals in forwarded sessions');
}

done_testing();
