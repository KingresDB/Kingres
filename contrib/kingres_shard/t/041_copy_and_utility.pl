# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/041_copy_and_utility.pl
#
# COPY <table> TO and the utility statements that read or write a distributed
# or reference table without going through the planner hook directly:
#  - COPY <distributed table> TO answers from every shard group (it is run as
#    COPY (SELECT <columns> FROM <table>) TO), with the same output as the
#    same table on a single node; a reference table, a bucket partition named
#    directly, COPY (SELECT ...) TO, a server-side file or program, \copy, a
#    column list, other options and a cluster of one group are covered;
#  - the audit of CREATE TABLE AS, SELECT INTO, materialized views, cursors,
#    EXPLAIN ANALYZE of a write, views, VACUUM, ANALYZE, REINDEX, CLUSTER and
#    COPY FROM: each gives a whole answer or is refused.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql group_of_key);

my @nodes = make_cluster('cp', 3);
my ($n1, $n2, $n3) = @nodes;
my $plain = PostgreSQL::Test::Cluster->new('cpplain');
$plain->init;
$plain->start;

my $ddl = q{
	CREATE TABLE public.dtab (id int PRIMARY KEY, name text, gen int GENERATED ALWAYS AS (id * 2) STORED);
	CREATE TABLE public.rtab (id int PRIMARY KEY, name text);
};
my $rows = join('',
	map { "INSERT INTO public.dtab (id, name) VALUES ($_, " . ($_ % 10 == 0 ? 'NULL' : "E'x,\"q\"\\t$_'") . ");\n" } (1 .. 60));
my $rrows = join('', map { "INSERT INTO public.rtab VALUES ($_, 'r$_');\n" } (1 .. 7));

$plain->safe_psql('postgres', $ddl . $rows . $rrows);
$n1->safe_psql('postgres', $ddl . $rrows . q{
	SELECT kingres.create_reference_table('public.rtab');
	SELECT kingres.create_distributed_table('public.dtab', 'id');
});
$n1->safe_psql('postgres', $rows);
is($n2->safe_psql('postgres', 'SELECT count(*) FROM public.dtab'), '60', 'the distributed table is loaded');
is($n1->safe_psql('postgres', q{SELECT count(DISTINCT group_id) FROM kingres.bucket_map}), '3',
	'the buckets are spread over three groups');

sub sorted_copy
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, $sql);
	return "ERROR: $err" if $rc != 0;
	return join("\n", sort split(/\n/, $out));
}

sub line_count
{
	my ($text) = @_;
	return scalar(() = $text =~ /\n/g) + ($text =~ /[^\n]\z/ ? 1 : 0);
}

# Differential: the same COPY on the single node and on each node of the cluster.
my @copies = (
	[ 'the whole table',            q{COPY public.dtab TO STDOUT} ],
	[ 'a column list',              q{COPY public.dtab (name, id) TO STDOUT} ],
	[ 'a generated column listed',  q{COPY public.dtab (id, gen) TO STDOUT} ],
	[ 'a reference table',          q{COPY public.rtab TO STDOUT} ],
	[ 'COPY (SELECT ...)',          q{COPY (SELECT id, name FROM public.dtab WHERE id % 3 = 0) TO STDOUT} ],
	[ 'CSV',                        q{COPY public.dtab TO STDOUT WITH (FORMAT csv, NULL 'NULLED', FORCE_QUOTE *)} ],
	[ 'text with a null string',    q{COPY public.dtab TO STDOUT WITH (NULL '-')} ],
);
for my $c (@copies)
{
	my ($what, $sql) = @$c;
	my $want = sorted_copy($plain, $sql);
	for my $n (@nodes)
	{
		is(sorted_copy($n, $sql), $want, "COPY TO, $what, on " . $n->name . ": the rows of every group");
	}
}
is(line_count((run_sql($n2, q{COPY public.dtab TO STDOUT}))[1]), 60,
	'COPY of the whole table returns all 60 rows, not the rows of one node');

my @csv = split(/\n/, (run_sql($n3, q{COPY public.dtab TO STDOUT WITH (FORMAT csv, HEADER)}))[1]);
is($csv[0], 'id,name', 'COPY TO CSV HEADER: the header is the first line');
is(scalar(@csv), 61, 'COPY TO CSV HEADER: the header and 60 rows');

# A bucket partition named directly is what it is: the rows of that bucket on this node.
my ($pn) = grep { $_->safe_psql('postgres', q{SELECT to_regclass('public.dtab_b0000') IS NOT NULL}) eq 't' } @nodes;
ok(defined $pn, 'bucket 0 is on one of the nodes');
my $part_rows = $pn->safe_psql('postgres', q{SELECT count(*) FROM public.dtab_b0000});
is(line_count($pn->safe_psql('postgres', q{COPY public.dtab_b0000 TO STDOUT})), $part_rows,
	'COPY of a bucket partition returns the rows of that partition');
cmp_ok($part_rows, '<', 60, 'a partition holds only part of the table');

# A server-side file, a program and \copy.
my $dir = PostgreSQL::Test::Utils::tempdir;
my $f1 = "$dir/dtab_file.txt";
sub file_lines
{
	my ($path) = @_;
	return -e $path ? line_count(PostgreSQL::Test::Utils::slurp_file($path)) : -1;
}
run_sql($n2, "COPY public.dtab TO '$f1'");
is(file_lines($f1), 60, 'COPY TO a file: 60 rows');
my $f2 = "$dir/dtab_prog.txt";
run_sql($n3, "COPY public.dtab TO PROGRAM 'cat > $f2'");
is(file_lines($f2), 60, 'COPY TO PROGRAM: 60 rows');
my $f3 = "$dir/dtab_psql.txt";
run_sql($n1, "\\copy public.dtab TO '$f3'");
is(file_lines($f3), 60, '\copy TO: 60 rows');
my $f4 = "$dir/dtab_do.txt";
run_sql($n2, qq{DO \$\$ BEGIN EXECUTE 'COPY public.dtab TO ''$f4'''; END \$\$});
is(file_lines($f4), 60, 'COPY TO inside a function: 60 rows');

# Privileges: the SELECT privilege is what COPY TO needs, as for the table form.
$_->safe_psql('postgres', q{CREATE ROLE bob LOGIN; CREATE ROLE eve LOGIN}) for @nodes;
$n1->safe_psql('postgres', q{GRANT SELECT ON public.dtab TO bob});
my ($rc, $out, $err) = run_sql($n3, q{COPY public.dtab TO STDOUT}, extra_params => [ '-U', 'bob' ]);
is($rc, 0, 'a role with SELECT may COPY TO') or diag $err;
is(line_count($out), 60, 'and gets the rows of every group');
($rc, $out, $err) = run_sql($n3, q{COPY public.dtab TO STDOUT}, extra_params => [ '-U', 'eve' ]);
isnt($rc, 0, 'a role without SELECT may not');
like($err, qr/permission denied/, 'the error is permission denied');

# COPY FROM stays refused on a cluster of several groups.
($rc, $out, $err) = run_sql($n1, q{COPY public.dtab FROM STDIN}, stdin => "1\tx\n");
isnt($rc, 0, 'COPY FROM a distributed table is refused');
like($err, qr/COPY FROM is not supported/, 'COPY FROM: the message');
($rc, $out, $err) = run_sql($pn, q{COPY public.dtab_b0000 FROM STDIN}, stdin => "1\tx\n");
isnt($rc, 0, 'COPY FROM a bucket partition is refused');
like($err, qr/COPY FROM is not supported/, 'COPY FROM a bucket partition: the message');

# A cluster of one shard group: COPY TO is local and complete.
my ($solo) = make_cluster('cpsolo', 1);
$solo->safe_psql('postgres', q{
	CREATE TABLE public.s (id int PRIMARY KEY, v int);
	SELECT kingres.create_distributed_table('public.s', 'id');
	INSERT INTO public.s SELECT g, g FROM generate_series(1, 25) g;
});
is(line_count($solo->safe_psql('postgres', q{COPY public.s TO STDOUT})), 25,
	'one shard group: COPY TO returns all rows');

# ---------------------------------------------------------------------------
# The audit: utility statements that read or write a table without the planner hook
# ---------------------------------------------------------------------------
my $want = $n1->safe_psql('postgres', 'SELECT count(*) FROM public.dtab');
for my $n (@nodes)
{
	my $nm = $n->name;
	is($n->safe_psql('postgres', q{CREATE TABLE public.ctas AS SELECT * FROM public.dtab; SELECT count(*) FROM public.ctas; DROP TABLE public.ctas}), $want,
		"CREATE TABLE AS reads every group ($nm)");
	is($n->safe_psql('postgres', q{SELECT * INTO public.sel_into FROM public.dtab; SELECT count(*) FROM public.sel_into; DROP TABLE public.sel_into}), $want,
		"SELECT INTO reads every group ($nm)");
	is($n->safe_psql('postgres', q{CREATE MATERIALIZED VIEW public.mv AS SELECT id, name FROM public.dtab; SELECT count(*) FROM public.mv}), $want,
		"CREATE MATERIALIZED VIEW reads every group ($nm)");
	$n->safe_psql('postgres', q{REFRESH MATERIALIZED VIEW public.mv});
	is($n->safe_psql('postgres', q{SELECT count(*) FROM public.mv; DROP MATERIALIZED VIEW public.mv}), $want,
		"REFRESH MATERIALIZED VIEW reads every group ($nm)");
	is(line_count($n->safe_psql('postgres', q{BEGIN; DECLARE c CURSOR FOR SELECT * FROM public.dtab; FETCH ALL FROM c; COMMIT})), 60,
		"a cursor reads every group ($nm)");
	is($n->safe_psql('postgres', q{CREATE VIEW public.dv AS SELECT * FROM public.dtab; SELECT count(*) FROM public.dv; DROP VIEW public.dv}), $want,
		"a view reads every group ($nm)");
}

# EXPLAIN ANALYZE runs the statement: a routed write and a reference write reach the right nodes.
my ($other_key) = grep { group_of_key($n1, 1, $_) ne group_of_key($n1, 1, 1) } (100 .. 200);
$n1->safe_psql('postgres', "EXPLAIN (ANALYZE, COSTS OFF) INSERT INTO public.dtab (id, name) VALUES ($other_key, 'explained')");
is($n2->safe_psql('postgres', "SELECT name FROM public.dtab WHERE id = $other_key"), 'explained',
	'EXPLAIN ANALYZE of a routed INSERT wrote the row');
$n2->safe_psql('postgres', q{EXPLAIN (ANALYZE, COSTS OFF) UPDATE public.rtab SET name = name || '!' WHERE id = 1});
is($_->safe_psql('postgres', q{SELECT name FROM public.rtab WHERE id = 1}), 'r1!',
	'EXPLAIN ANALYZE of a reference write reached ' . $_->name)
  for @nodes;
$n3->safe_psql('postgres', "DELETE FROM public.dtab WHERE id = $other_key");

# Maintenance commands act on this node's partitions and complete.
for my $stmt ('VACUUM public.dtab', 'VACUUM (FULL) public.dtab', 'ANALYZE public.dtab',
	'REINDEX TABLE public.dtab', 'VACUUM public.rtab', 'BEGIN; LOCK TABLE public.dtab IN SHARE MODE; COMMIT')
{
	($rc, $out, $err) = run_sql($n2, $stmt);
	is($rc, 0, "$stmt runs on one node") or diag $err;
}
($rc, $out, $err) = run_sql($n1, q{CLUSTER public.dtab USING dtab_pkey});
isnt($rc, 0, 'CLUSTER of a distributed table is refused');
like($err, qr/CLUSTER is not supported/, 'CLUSTER: the message');

done_testing();
