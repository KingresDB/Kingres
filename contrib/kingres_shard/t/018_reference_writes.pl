# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/018_reference_writes.pl
#
# Review findings S2-05 and S2-06:
#  - a write to a reference table is refused if its result can depend on the
#    physical layout of the node (system columns, LIMIT without a total order,
#    window functions, order-sensitive aggregates, an UPDATE ... FROM that can
#    match several rows to one row);
#  - writers of a reference table are serialized cluster-wide, so a statement
#    that reads the table it writes (or another reference table) sees the same
#    rows on every node.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run ();
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('rw', 3);
my @nodes = ($n1, $n2, $n3);

$n1->safe_psql('postgres', q{
	CREATE TABLE public.r (id int PRIMARY KEY, v int);
	CREATE TABLE public.r2 (id int PRIMARY KEY, v int);
	CREATE TABLE public.rf (id int PRIMARY KEY, f float8);
	CREATE TABLE public.nu (id int, w int);
	INSERT INTO public.r SELECT g, g FROM generate_series(1, 20) g;
	INSERT INTO public.r2 SELECT g, g FROM generate_series(1, 5) g;
	INSERT INTO public.rf SELECT g, g / 3.0 FROM generate_series(1, 5) g;
	INSERT INTO public.nu VALUES (1, 1), (1, 2), (2, 3);
	SELECT kingres.create_reference_table('public.r');
	SELECT kingres.create_reference_table('public.r2');
	SELECT kingres.create_reference_table('public.rf');
	SELECT kingres.create_reference_table('public.nu');
});

sub table_text
{
	my ($node, $table) = @_;
	return $node->safe_psql('postgres',
		"SELECT coalesce(string_agg(t::text, ',' ORDER BY t::text), '') FROM public.$table t");
}

sub same_everywhere
{
	my ($label, @tables) = @_;
	for my $t (@tables)
	{
		my $want = table_text($n1, $t);
		is(table_text($_, $t), $want, "$label: $t is the same on " . $_->name) for ($n2, $n3);
	}
}

# ---------------------------------------------------------------------------
# S2-05: layout-dependent statements are refused; the copies stay equal.
# ---------------------------------------------------------------------------
# Make the physical layout of node 2 differ: delete a row and vacuum there only,
# then put the row back (the copies hold the same rows again).
$n2->safe_psql('postgres', 'DELETE FROM public.r WHERE id = 20; VACUUM public.r');
$n2->safe_psql('postgres', 'INSERT INTO public.r VALUES (20, 20)');

my @refused = (
	[ 'a system column (ctid)',      q{UPDATE public.r SET v = -1 WHERE ctid = '(0,3)'}, qr/system column/ ],
	[ 'a system column (tableoid)',  q{UPDATE public.r SET v = -1 WHERE tableoid = 0 AND id = 3}, qr/system column/ ],
	[ 'LIMIT without a total order', q{INSERT INTO public.r2 SELECT id + 100, v FROM public.r LIMIT 1}, qr/LIMIT or OFFSET/ ],
	[ 'a window function',           q{INSERT INTO public.r2 SELECT id + 200, row_number() OVER () FROM public.r}, qr/window function/ ],
	[ 'array_agg without an order',  q{UPDATE public.nu SET w = (SELECT array_length(array_agg(id), 1) FROM public.r) WHERE id = 2}, qr/order of the rows/ ],
	[ 'a float sum',                 q{UPDATE public.rf SET f = (SELECT sum(f) FROM public.rf) WHERE id = 1}, qr/order of the rows/ ],
	[ 'DISTINCT ON',                 q{INSERT INTO public.r2 SELECT DISTINCT ON (v % 3) id + 300, v FROM public.r}, qr/DISTINCT ON/ ],
	[ 'TABLESAMPLE',                 q{INSERT INTO public.r2 SELECT id + 400, v FROM public.r TABLESAMPLE SYSTEM (50)}, qr/samples a table/ ],
	[ 'UPDATE FROM with several matches', q{UPDATE public.r SET v = nu.w FROM public.nu WHERE nu.id = r.id % 3}, qr/several rows/ ],
);
for my $t (@refused)
{
	my ($rc, $out, $err) = run_sql($n1, $t->[1]);
	isnt($rc, 0, "S2-05: refused: $t->[0]");
	like($err, qr/cannot be run on every node/, "S2-05: refused with the standard message: $t->[0]");
	like($err, $t->[2], "S2-05: the reason names the construct: $t->[0]");
}
same_everywhere('after the refused statements', 'r', 'r2', 'nu', 'rf');

# What is deterministic is still accepted.
{
	my ($rc, $out, $err) = run_sql($n1, q{UPDATE public.r SET v = r2.v + 1000 FROM public.r2 WHERE r2.id = r.id});
	is($err, '', 'UPDATE ... FROM a reference table matched through its primary key');
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.r2 SELECT id + 500, v FROM public.r ORDER BY v DESC, id});
	is($err, '', 'INSERT ... SELECT ... ORDER BY');
	($rc, $out, $err) = run_sql($n1, q{UPDATE public.nu SET w = (SELECT count(*) FROM public.r) WHERE id = 2});
	is($err, '', 'a count(*) of a reference table');
}
same_everywhere('after the accepted statements', 'r', 'r2', 'nu');

# ---------------------------------------------------------------------------
# S2-06: writers are serialized; a statement that reads what it writes sees one state.
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', 'DELETE FROM public.r2; DELETE FROM public.r; INSERT INTO public.r VALUES (2, 0), (3, 3)');
{
	# The review's reproduction: a slow write on node 1 while another session inserts.
	my $a = $n1->background_psql('postgres', on_error_stop => 0);
	$a->query_until(qr/started/,
		"\\echo started\nUPDATE public.r SET v = (SELECT count(*) FROM public.r s) WHERE id = 2 "
		  . "AND (SELECT count(*) FROM generate_series(1, 30000000)) > 0;\n");
	select(undef, undef, undef, 0.7);
	$n2->safe_psql('postgres', 'INSERT INTO public.r VALUES (99, 99)');
	$a->query('SELECT 1');
	$a->quit;
	same_everywhere('S2-06: a slow write that reads its own table, with a concurrent insert', 'r');
}

# Concurrent writers from all three nodes: a write that reads the table it
# writes, and one that reads another reference table.
{
	$n1->safe_psql('postgres', 'DELETE FROM public.r2; INSERT INTO public.r2 VALUES (1, 0)');
	my $dir = $PostgreSQL::Test::Utils::tmp_check;
	append_to_file("$dir/refw.sql",
		"\\set i random(1000, 1000000000)\n"
		  . "INSERT INTO public.r VALUES (:i, 0) ON CONFLICT DO NOTHING;\n"
		  . "UPDATE public.r SET v = (SELECT count(*) FROM public.r s) WHERE id = 2;\n"
		  . "INSERT INTO public.r2 SELECT 1, v FROM public.r WHERE id = 2 ON CONFLICT (id) DO UPDATE SET v = excluded.v + (SELECT count(*) FROM public.r2);\n");
	my @h;
	my @out;
	for my $i (0 .. 2)
	{
		$out[$i] = '';
		push @h, IPC::Run::start(
			[ 'pgbench', '-n', '-h', $nodes[$i]->host, '-p', $nodes[$i]->port, '-c', 3, '-j', 1, '-T', 8,
				'-f', "$dir/refw.sql", 'postgres' ],
			'>', \$out[$i], '2>', \$out[$i]);
	}
	$_->finish for @h;
	for my $i (0 .. 2)
	{
		unlike($out[$i], qr/ERROR|aborted/, "concurrent reference writers on node " . ($i + 1) . " run without an error")
		  or diag($out[$i]);
	}
	same_everywhere('S2-06: after concurrent writers on all nodes', 'r', 'r2');
	is(prepared_count($_), '0', "nothing is left prepared on " . $_->name) for @nodes;
}

done_testing();
