# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/056_ri_distributed.pl
#
# Foreign keys from a distributed table to a reference table (review finding S6-01
# of kingres/docs/reviews/2026-11-15-kingres-shard-0.4-port.md).  ri_triggers.c
# leaves ONLY out of the statements of its actions when the referencing table is
# partitioned, which every distributed table is, so the actions of every such key
# were routed as statements of the user and failed.  The rows of the referencing
# table are on every shard group, and a write to a reference table runs on every
# node, so every node does its own part:
#
#   NO ACTION, RESTRICT              refuse when a referencing row exists on any group
#   CASCADE, SET NULL, SET DEFAULT   act on the rows of every group
#   ... on delete and on update, deferred checks, self-referencing reference tables,
#   a reference table referenced by a reference table and a distributed table at
#   once, keys of several columns, referencing tables in two colocation groups, a
#   chain reference -> distributed -> colocated distributed, and a bucket move.
#
# The statements that are planned for this node's rows are recognised by the
# security context that ri_triggers.c sets, not by their text: a statement of a
# user that begins with the old text is routed as before.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use POSIX ();
use IPC::Run ();
use Time::HiRes qw(sleep time);
use KingresShardTest qw(make_cluster keys_of_group run_sql wait_for_fault_file prepared_count);

my @nodes = make_cluster('ri', 3, logical => 1);
my ($n1, $n2, $n3) = @nodes;
my $tmp = PostgreSQL::Test::Utils::tempdir();

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

# The colocation group of a distributed table.
sub coloc
{
	my ($tbl) = @_;
	return q1($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = '$tbl'::regclass");
}

# One text line of the rows of a table as one node sees it, ordered.
sub dump_of
{
	my ($node, $tbl, $expr) = @_;
	return q1($node, "SELECT coalesce(string_agg($expr, ',' ORDER BY $expr), '') FROM $tbl");
}

# The copies of a reference table, one per node, must be equal.
sub copies_equal
{
	my ($tbl, $expr, $what) = @_;
	my @c = map { dump_of($_, $tbl, $expr) } @nodes;
	is($c[1], $c[0], "$what: the copy of $tbl on node 2 is the copy on node 1");
	is($c[2], $c[0], "$what: the copy of $tbl on node 3 is the copy on node 1");
}

# The rows of a distributed table that each node holds in its own buckets.
sub local_counts
{
	my ($tbl) = @_;
	return join('/', map { q1($_, "SET kingres_shard.enable_routing = off; SELECT count(*) FROM $tbl") } @nodes);
}

# A group of a distributed table reference by pid: "pidxcount" pairs.
sub pids
{
	my ($tbl, $col) = @_;
	$col //= 'pid';
	return q1($n1, "SELECT coalesce(string_agg(coalesce(${col}::text, 'N') || 'x' || n, ',' ORDER BY $col), '') FROM (SELECT $col, count(*) n FROM $tbl GROUP BY $col) q");
}

# Three rows in each shard group of the distributed table $d, the rows of group g
# referencing the row g + 1 of the reference table.
sub fill_groups
{
	my ($d, $cols, $valfmt) = @_;
	my $c = coloc($d);
	my $sql = '';
	for my $g (0 .. 2)
	{
		my @k = keys_of_group($n1, $c, $g, 3, 1 + 1000 * $g);
		$sql .= "INSERT INTO $d ($cols) VALUES (" . sprintf($valfmt, $_, $g + 1) . ");\n" for @k;
	}
	q1($n1, $sql);
}

# A reference table r<s>(id) with the rows 1 .. 6 (and 0 for a default), a
# distributed table d<s>(id, pid) with a foreign key on pid; nine rows, the rows of
# group g referencing the row g + 1 of r (1, 2 and 3 are each referenced from one
# group only, 4, 5 and 6 from none).
sub make_pair
{
	my ($s, $fk, %opt) = @_;
	my $def = $opt{default} ? ' DEFAULT 0' : '';
	q1($n1, qq{
		CREATE TABLE public.r$s (id int PRIMARY KEY);
		SELECT kingres.create_reference_table('public.r$s');
		INSERT INTO public.r$s SELECT g FROM generate_series(1, 6) g;
		CREATE TABLE public.d$s (id int PRIMARY KEY, pid int$def);
		SELECT kingres.create_distributed_table('public.d$s', 'id', bucket_count => 6);
	});
	q1($n1, "INSERT INTO public.r$s VALUES (0)") if $opt{default};
	q1($n1, "ALTER TABLE public.d$s ADD CONSTRAINT d${s}_fk FOREIGN KEY (pid) REFERENCES public.r$s(id) $fk");
	fill_groups("public.d$s", 'id, pid', '%d, %d');
}

my $fkerr = qr/violates (RESTRICT setting of )?foreign key constraint/;

# ---------------------------------------------------------------------------
# NO ACTION and RESTRICT: refuse when a referencing row exists on any group.
# ---------------------------------------------------------------------------
for my $action ('NO ACTION', 'RESTRICT')
{
	my $s = lc($action) =~ s/ //gr;
	make_pair($s, "ON DELETE $action ON UPDATE $action");
	my $before = dump_of($n1, "public.d$s", "id || ':' || pid");
	is(pids("public.d$s"), '1x3,2x3,3x3', "$action: nine rows, three in each group");
	is(local_counts("public.d$s"), '3/3/3', "$action: three rows on each node");

	# Rows 4, 5, 6 are referenced by nobody, whatever node deletes them.
	for my $i (0 .. 2)
	{
		my ($rc, $out, $err) = run_sql($nodes[$i], "DELETE FROM public.r$s WHERE id = " . (4 + $i));
		is($rc, 0, "$action: an unreferenced row is deleted from node " . ($i + 1)) or diag $err;
	}
	is(q1($n1, "SELECT string_agg(id::text, ',' ORDER BY id) FROM public.r$s"), '1,2,3', "$action: the rows are gone");
	q1($n1, "INSERT INTO public.r$s SELECT g FROM generate_series(4, 6) g");
	my ($rc, $out, $err) = run_sql($n2, "UPDATE public.r$s SET id = id + 100 WHERE id = 6");
	is($rc, 0, "$action: the key of an unreferenced row changes") or diag $err;
	q1($n1, "UPDATE public.r$s SET id = 6 WHERE id = 106");

	# A row referenced from one group only is refused whatever node asks.
	for my $pid (1, 2, 3)
	{
		for my $n (@nodes)
		{
			($rc, $out, $err) = run_sql($n, "DELETE FROM public.r$s WHERE id = $pid");
			isnt($rc, 0, "$action: delete of row $pid (referenced from group " . ($pid - 1) . ") is refused from " . $n->name);
			like($err, $fkerr, '...by the foreign key');
		}
		($rc, $out, $err) = run_sql($n2, "UPDATE public.r$s SET id = id + 100 WHERE id = $pid");
		isnt($rc, 0, "$action: a key change of row $pid is refused");
		like($err, $fkerr, '...by the foreign key');
	}
	is(dump_of($n1, "public.d$s", "id || ':' || pid"), $before, "$action: no referencing row changed");
	is(q1($n1, "SELECT string_agg(id::text, ',' ORDER BY id) FROM public.r$s"), '1,2,3,4,5,6', "$action: no row of the reference table changed");
	copies_equal("public.r$s", 'id::text', $action);
	is(prepared_count($_), '0', "$action: nothing is left prepared on " . $_->name) for @nodes;

	# A delete of several rows at once: one of them referenced.
	($rc, $out, $err) = run_sql($n1, "DELETE FROM public.r$s WHERE id >= 3");
	isnt($rc, 0, "$action: a delete of several rows with one referenced row is refused as a whole");
	is(q1($n1, "SELECT count(*) FROM public.r$s"), '6', "$action: and none of them was deleted");

	# Once the referencing rows are gone, the row can go.
	q1($n1, "DELETE FROM public.d$s WHERE pid = 2");
	($rc, $out, $err) = run_sql($n3, "DELETE FROM public.r$s WHERE id = 2");
	is($rc, 0, "$action: when nothing references it any more, the row is deleted") or diag $err;
	copies_equal("public.r$s", 'id::text', "$action after a delete");
}

# ---------------------------------------------------------------------------
# CASCADE
# ---------------------------------------------------------------------------
{
	make_pair('cascade', 'ON DELETE CASCADE ON UPDATE CASCADE');
	my ($rc, $out, $err) = run_sql($n2, "DELETE FROM public.rcascade WHERE id = 2");
	is($rc, 0, 'CASCADE: a referenced row is deleted from node 2') or diag $err;
	is(pids('public.dcascade'), '1x3,3x3', 'CASCADE: the three referencing rows of group 1 are gone, the others are not');
	is(local_counts('public.dcascade'), '3/0/3', 'CASCADE: ...on the node of that group');
	($rc, $out, $err) = run_sql($n3, "UPDATE public.rcascade SET id = 30 WHERE id = 3");
	is($rc, 0, 'CASCADE: a key is changed from node 3') or diag $err;
	is(pids('public.dcascade'), '1x3,30x3', 'CASCADE: the three referencing rows of group 2 follow the key');
	($rc, $out, $err) = run_sql($n1, "UPDATE public.rcascade SET id = id + 10 WHERE id IN (1, 5)");
	is($rc, 0, 'CASCADE: two keys change in one statement') or diag $err;
	is(pids('public.dcascade'), '11x3,30x3', 'CASCADE: ...and the referencing rows follow');
	copies_equal('public.rcascade', 'id::text', 'CASCADE');
	# Both actions in one statement of a function and of a prepared statement.
	q1($n1, q{CREATE FUNCTION public.del_r(int) RETURNS void LANGUAGE plpgsql AS $$ BEGIN DELETE FROM public.rcascade WHERE id = $1; END $$});
	q1($_, q{CREATE FUNCTION public.del_r(int) RETURNS void LANGUAGE plpgsql AS $$ BEGIN DELETE FROM public.rcascade WHERE id = $1; END $$}) for ($n2, $n3);
	($rc, $out, $err) = run_sql($n2, "SELECT public.del_r(30)");
	is($rc, 0, 'CASCADE: from a function') or diag $err;
	is(pids('public.dcascade'), '11x3', 'CASCADE: ...the rows of group 2 are gone');
	($rc, $out, $err) = run_sql($n3, "PREPARE p(int) AS DELETE FROM public.rcascade WHERE id = \$1; EXECUTE p(11); EXECUTE p(4); EXECUTE p(4)");
	is($rc, 0, 'CASCADE: from a prepared statement, executed several times') or diag $err;
	is(q1($n1, 'SELECT count(*) FROM public.dcascade'), '0', 'CASCADE: nothing is left of the referencing rows');
	copies_equal('public.rcascade', 'id::text', 'CASCADE at the end');
}

# ---------------------------------------------------------------------------
# SET NULL and SET DEFAULT
# ---------------------------------------------------------------------------
{
	make_pair('setnull', 'ON DELETE SET NULL ON UPDATE SET NULL');
	my ($rc, $out, $err) = run_sql($n3, "DELETE FROM public.rsetnull WHERE id = 1");
	is($rc, 0, 'SET NULL: a referenced row is deleted') or diag $err;
	is(pids('public.dsetnull'), '2x3,3x3,Nx3', 'SET NULL: the three rows of group 0 have no key now');
	($rc, $out, $err) = run_sql($n2, "UPDATE public.rsetnull SET id = 33 WHERE id = 3");
	is($rc, 0, 'SET NULL: a key is changed') or diag $err;
	is(pids('public.dsetnull'), '2x3,Nx6', 'SET NULL: the three rows of group 2 have none now either');
	is(q1($n1, 'SELECT count(*) FROM public.dsetnull'), '9', 'SET NULL: no row was deleted');

	make_pair('setdef', 'ON DELETE SET DEFAULT ON UPDATE SET DEFAULT', default => 1);
	($rc, $out, $err) = run_sql($n1, "DELETE FROM public.rsetdef WHERE id = 2");
	is($rc, 0, 'SET DEFAULT: a referenced row is deleted') or diag $err;
	is(pids('public.dsetdef'), '0x3,1x3,3x3', 'SET DEFAULT: the three rows of group 1 reference the default');
	($rc, $out, $err) = run_sql($n3, "UPDATE public.rsetdef SET id = 13 WHERE id = 1");
	is($rc, 0, 'SET DEFAULT: a key is changed') or diag $err;
	is(pids('public.dsetdef'), '0x6,3x3', 'SET DEFAULT: the rows of group 0 reference the default too');
	# The default row itself is referenced now, from groups 0 and 1.
	($rc, $out, $err) = run_sql($n2, "DELETE FROM public.rsetdef WHERE id = 0");
	isnt($rc, 0, 'SET DEFAULT: a delete of the default row, which the rows of two groups reference, fails');
	is(pids('public.dsetdef'), '0x6,3x3', 'SET DEFAULT: ...and changes nothing');
	copies_equal('public.rsetdef', 'id::text', 'SET DEFAULT');
	copies_equal('public.rsetnull', 'id::text', 'SET NULL');
}

# ---------------------------------------------------------------------------
# Deferred checks: each node checks its rows when it prepares.
# ---------------------------------------------------------------------------
{
	make_pair('def', 'ON DELETE NO ACTION ON UPDATE NO ACTION DEFERRABLE INITIALLY DEFERRED');
	my $before = dump_of($n1, 'public.ddef', "id || ':' || pid");
	for my $n ($n1, $n2)
	{
		my ($rc, $out, $err) = run_sql($n, "BEGIN; DELETE FROM public.rdef WHERE id = 3; COMMIT");
		isnt($rc, 0, 'deferred: a referenced row (referenced from group 2) is refused at commit, from ' . $n->name);
		like($err, $fkerr, '...by the foreign key');
	}
	is(prepared_count($_), '0', "deferred: nothing is left prepared on " . $_->name) for @nodes;
	is(dump_of($n1, 'public.ddef', "id || ':' || pid"), $before, 'deferred: the referencing rows are as they were');
	is(q1($n1, 'SELECT count(*) FROM public.rdef'), '6', 'deferred: so is the reference table');

	my ($rc, $out, $err) = run_sql($n2, "BEGIN; DELETE FROM public.rdef WHERE id = 3; DELETE FROM public.ddef WHERE pid = 3; COMMIT");
	is($rc, 0, 'deferred: the same delete is fine when the referencing rows go in the same transaction') or diag $err;
	is(pids('public.ddef'), '1x3,2x3', 'deferred: and they are gone');
	($rc, $out, $err) = run_sql($n3, "BEGIN; UPDATE public.rdef SET id = 20 WHERE id = 2; UPDATE public.ddef SET pid = 20 WHERE pid = 2; COMMIT");
	is($rc, 0, 'deferred: a key and the rows that reference it change in one transaction') or diag $err;
	is(pids('public.ddef'), '1x3,20x3', 'deferred: ...the rows are on the new key');
	# The row that is referenced is inserted after the row that references it.
	my ($k) = keys_of_group($n1, coloc('public.ddef'), 1, 1, 5000);
	($rc, $out, $err) = run_sql($n1, "BEGIN; INSERT INTO public.ddef VALUES ($k, 99); INSERT INTO public.rdef VALUES (99); COMMIT");
	is($rc, 0, 'deferred: a row may be inserted before the row it references') or diag $err;
	($rc, $out, $err) = run_sql($n1, "BEGIN; INSERT INTO public.ddef VALUES ($k + 1000000, 98); COMMIT");
	isnt($rc, 0, 'deferred: a row that references nothing is refused at commit');
	like($err, $fkerr, '...by the foreign key');
	# SET CONSTRAINTS reaches this node only; the other nodes check at commit, which is
	# later but not never.
	($rc, $out, $err) = run_sql($n1, "BEGIN; SET CONSTRAINTS ALL IMMEDIATE; DELETE FROM public.rdef WHERE id = 1; COMMIT");
	isnt($rc, 0, 'deferred: SET CONSTRAINTS ALL IMMEDIATE does not let a violation through');
	like($err, $fkerr, '...by the foreign key');
	is(pids('public.ddef'), '1x3,20x3,99x1', 'deferred: the rows are as they were');
	copies_equal('public.rdef', 'id::text', 'deferred');
	is(prepared_count($_), '0', "deferred: nothing is left prepared on " . $_->name) for @nodes;
}

# ---------------------------------------------------------------------------
# A self-referencing reference table.
# ---------------------------------------------------------------------------
for my $act ('CASCADE', 'SET NULL', 'NO ACTION')
{
	my $s = 'self' . lc($act) =~ s/ //gr;
	q1($n1, qq{
		CREATE TABLE public.$s (id int PRIMARY KEY, parent int);
		SELECT kingres.create_reference_table('public.$s');
		INSERT INTO public.$s VALUES (1, NULL), (2, 1), (3, 2), (4, 1), (5, NULL);
		ALTER TABLE public.$s ADD FOREIGN KEY (parent) REFERENCES public.$s(id) ON DELETE $act ON UPDATE $act;
	});
	my ($rc, $out, $err) = run_sql($n2, "DELETE FROM public.$s WHERE id = 1");
	my $want = $act eq 'CASCADE' ? '5:N' : $act eq 'SET NULL' ? '2:N,3:2,4:N,5:N' : '1:N,2:1,3:2,4:1,5:N';
	if ($act eq 'NO ACTION')
	{
		isnt($rc, 0, "self-referencing, $act: the delete of a parent is refused");
		like($err, $fkerr, '...by the foreign key');
	}
	else
	{
		is($rc, 0, "self-referencing, $act: the delete of a parent works") or diag $err;
	}
	copies_equal("public.$s", "id || ':' || coalesce(parent::text, 'N')", "self-referencing, $act");
	is(dump_of($n1, "public.$s", "id || ':' || coalesce(parent::text, 'N')"), join(',', sort split /,/, $want), "self-referencing, $act: the rows");
	if ($act eq 'CASCADE')
	{
		q1($n1, "INSERT INTO public.$s VALUES (6, 5), (7, 6)");
		($rc, $out, $err) = run_sql($n3, "UPDATE public.$s SET id = 60 WHERE id = 6");
		is($rc, 0, 'self-referencing, CASCADE: a key change reaches the child') or diag $err;
		is(dump_of($n2, "public.$s", "id || ':' || coalesce(parent::text, 'N')"), '5:N,60:5,7:60', 'self-referencing, CASCADE: the child follows (read on node 2)');
	}
}

# ---------------------------------------------------------------------------
# A reference table referenced by a reference table and a distributed table.
# ---------------------------------------------------------------------------
{
	q1($n1, q{
		CREATE TABLE public.rp (id int PRIMARY KEY);
		SELECT kingres.create_reference_table('public.rp');
		INSERT INTO public.rp SELECT g FROM generate_series(1, 4) g;
		CREATE TABLE public.rc (id int PRIMARY KEY, p int);
		SELECT kingres.create_reference_table('public.rc');
		INSERT INTO public.rc VALUES (1, 1), (2, 2), (3, 2), (4, 3);
		ALTER TABLE public.rc ADD FOREIGN KEY (p) REFERENCES public.rp ON DELETE CASCADE ON UPDATE CASCADE;
		CREATE TABLE public.dc (id int PRIMARY KEY, p int);
		SELECT kingres.create_distributed_table('public.dc', 'id', bucket_count => 6);
		ALTER TABLE public.dc ADD FOREIGN KEY (p) REFERENCES public.rp ON DELETE CASCADE ON UPDATE CASCADE;
	});
	fill_groups('public.dc', 'id, p', '%d, %d');
	my ($rc, $out, $err) = run_sql($n3, "DELETE FROM public.rp WHERE id = 2");
	is($rc, 0, 'both: a row that a reference table and a distributed table reference is deleted') or diag $err;
	is(dump_of($n1, 'public.rc', 'id::text'), '1,4', 'both: the rows of the reference table are gone on node 1');
	copies_equal('public.rc', 'id::text', 'both, the referencing reference table');
	is(pids('public.dc', 'p'), '1x3,3x3', 'both: so are the rows of the distributed table');
	($rc, $out, $err) = run_sql($n2, "UPDATE public.rp SET id = 10 WHERE id = 3");
	is($rc, 0, 'both: a key change reaches both') or diag $err;
	is(dump_of($n3, 'public.rc', "id || ':' || p"), '1:1,4:10', 'both: the reference table follows (read on node 3)');
	is(pids('public.dc', 'p'), '1x3,10x3', 'both: so does the distributed table');
	copies_equal('public.rp', 'id::text', 'both, the referenced table');
}

# ---------------------------------------------------------------------------
# A key of two columns, MATCH SIMPLE and MATCH FULL.
# ---------------------------------------------------------------------------
for my $match ('', 'MATCH FULL')
{
	my $s = $match eq '' ? 'mc' : 'mf';
	q1($n1, qq{
		CREATE TABLE public.r$s (a int, b int, PRIMARY KEY (a, b));
		SELECT kingres.create_reference_table('public.r$s');
		INSERT INTO public.r$s SELECT a, b FROM generate_series(1, 3) a, generate_series(1, 2) b;
		CREATE TABLE public.d$s (id int PRIMARY KEY, a int, b int);
		SELECT kingres.create_distributed_table('public.d$s', 'id', bucket_count => 6);
		ALTER TABLE public.d$s ADD FOREIGN KEY (a, b) REFERENCES public.r$s (a, b) $match ON DELETE CASCADE ON UPDATE CASCADE;
	});
	# Group g references (g + 1, 1) with two of its rows and (g + 1, 2) with one.
	my $c = coloc("public.d$s");
	my $sql = '';
	for my $g (0 .. 2)
	{
		my @k = keys_of_group($n1, $c, $g, 3, 1 + 1000 * $g);
		$sql .= "INSERT INTO public.d$s VALUES ($k[0], " . ($g + 1) . ", 1);\n"
		  . "INSERT INTO public.d$s VALUES ($k[1], " . ($g + 1) . ", 1);\n"
		  . "INSERT INTO public.d$s VALUES ($k[2], " . ($g + 1) . ", 2);\n";
	}
	q1($n1, $sql);
	my $expr = "coalesce(a::text, 'N') || '/' || coalesce(b::text, 'N')";
	# Only (a, b) as a whole is the key: deleting (2, 2) takes one row of group 1.
	my ($rc, $out, $err) = run_sql($n3, "DELETE FROM public.r$s WHERE a = 2 AND b = 2");
	is($rc, 0, "two columns $match: a row is deleted") or diag $err;
	is(dump_of($n1, "public.d$s", $expr), '1/1,1/1,1/2,2/1,2/1,3/1,3/1,3/2', "two columns $match: only the row of that key went");
	($rc, $out, $err) = run_sql($n1, "UPDATE public.r$s SET b = 7 WHERE a = 3 AND b = 1");
	is($rc, 0, "two columns $match: one column of a key changes") or diag $err;
	is(dump_of($n1, "public.d$s", $expr), '1/1,1/1,1/2,2/1,2/1,3/2,3/7,3/7', "two columns $match: the rows of group 2 follow in that column only");
	copies_equal("public.r$s", "a || '/' || b", "two columns $match");
}
{
	# NO ACTION on two columns; a NULL in one column references nothing (MATCH SIMPLE).
	q1($n1, q{
		CREATE TABLE public.r2c (a int, b int, PRIMARY KEY (a, b));
		SELECT kingres.create_reference_table('public.r2c');
		INSERT INTO public.r2c VALUES (1, 1), (1, 2);
		CREATE TABLE public.d2c (id int PRIMARY KEY, a int, b int);
		SELECT kingres.create_distributed_table('public.d2c', 'id', bucket_count => 6);
		ALTER TABLE public.d2c ADD FOREIGN KEY (a, b) REFERENCES public.r2c (a, b);
	});
	my ($k0) = keys_of_group($n1, coloc('public.d2c'), 0, 1);
	my ($k2) = keys_of_group($n1, coloc('public.d2c'), 2, 2);
	q1($n1, "INSERT INTO public.d2c VALUES ($k0, 1, NULL);
INSERT INTO public.d2c VALUES ($k2, 1, 1);");
	my ($rc, $out, $err) = run_sql($n1, "DELETE FROM public.r2c WHERE a = 1 AND b = 2");
	is($rc, 0, 'two columns: a row that only a row with a NULL in the key "references" is deleted') or diag $err;
	($rc, $out, $err) = run_sql($n1, "DELETE FROM public.r2c WHERE a = 1 AND b = 1");
	isnt($rc, 0, 'two columns: a row referenced from another group is refused');
	like($err, $fkerr, '...by the foreign key');
}

# ---------------------------------------------------------------------------
# One reference table, referenced from two colocation groups.
# ---------------------------------------------------------------------------
{
	make_pair('two', 'ON DELETE CASCADE');
	q1($n1, q{
		CREATE TABLE public.dtwo2 (id int PRIMARY KEY, pid int);
		SELECT kingres.create_distributed_table('public.dtwo2', 'id', bucket_count => 4);
		ALTER TABLE public.dtwo2 ADD FOREIGN KEY (pid) REFERENCES public.rtwo(id) ON DELETE NO ACTION;
	});
	isnt(coloc('public.dtwo2'), coloc('public.dtwo'), 'two groups: the second table is in another colocation group');
	fill_groups('public.dtwo2', 'id, pid', '%d, %d');
	my ($rc, $out, $err) = run_sql($n2, "DELETE FROM public.rtwo WHERE id = 3");
	isnt($rc, 0, 'two groups: the second table (NO ACTION) refuses, though the first one cascades');
	like($err, $fkerr, '...by the foreign key');
	is(pids('public.dtwo'), '1x3,2x3,3x3', 'two groups: and nothing of the first one was deleted');
	q1($n1, "DELETE FROM public.dtwo2 WHERE pid = 3");
	($rc, $out, $err) = run_sql($n2, "DELETE FROM public.rtwo WHERE id = 3");
	is($rc, 0, 'two groups: when the second table has no row, the delete cascades into the first') or diag $err;
	is(pids('public.dtwo'), '1x3,2x3', 'two groups: the first table lost its rows of group 2');
	is(pids('public.dtwo2'), '1x3,2x3', 'two groups: the second one has its other rows');
}

# ---------------------------------------------------------------------------
# A chain: reference -> distributed -> colocated distributed.
# ---------------------------------------------------------------------------
{
	make_pair('chain', 'ON DELETE CASCADE');
	q1($n1, q{
		CREATE TABLE public.dchain2 (id int PRIMARY KEY, note text);
		SELECT kingres.create_distributed_table('public.dchain2', 'id', colocate_with => 'public.dchain');
	});
	q1($n1, join('', map { "INSERT INTO public.dchain2 VALUES ($_, 'x');
" } split(/,/, q1($n1, "SELECT string_agg(id::text, ',') FROM public.dchain"))));
	q1($n1, "ALTER TABLE public.dchain2 ADD FOREIGN KEY (id) REFERENCES public.dchain(id) ON DELETE CASCADE");
	is(q1($n1, 'SELECT count(*) FROM public.dchain2'), '9', 'chain: the colocated table has a row for each');
	my ($rc, $out, $err) = run_sql($n3, "DELETE FROM public.rchain WHERE id = 1");
	is($rc, 0, 'chain: the delete of the reference row') or diag $err;
	is(pids('public.dchain'), '2x3,3x3', 'chain: removes the rows of the first table');
	is(q1($n1, 'SELECT count(*) FROM public.dchain2'), '6', 'chain: and the rows of the second that depend on them');
	is(local_counts('public.dchain2'), '0/3/3', 'chain: on the node of the group');
}

# ---------------------------------------------------------------------------
# A statement of a user that looks like what the triggers run is not one of them.
# ---------------------------------------------------------------------------
{
	q1($n1, q{
		CREATE TABLE public.rimit (id int PRIMARY KEY, val int);
		SELECT kingres.create_reference_table('public.rimit');
		CREATE TABLE public.dimit (id int PRIMARY KEY, v int);
		SELECT kingres.create_distributed_table('public.dimit', 'id', bucket_count => 6);
	});
	q1($n1, join('', map { "INSERT INTO public.dimit VALUES ($_, $_);\n" } 1 .. 30));
	# A function that reads the distributed table the way a foreign key reads it, called
	# by a check constraint of the reference table.  The statement is the user's: it is
	# planned and routed like any other, it spans the groups, and the write fails.
	for my $n (@nodes)
	{
		q1($n, q{CREATE FUNCTION public.looks_like_ri(int) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS
			$$ BEGIN PERFORM 1 FROM public.dimit x WHERE $1 OPERATOR(pg_catalog.=) id FOR KEY SHARE OF x; RETURN true; END $$});
	}
	my ($rc, $out, $err) = run_sql($n1, "ALTER TABLE public.rimit ADD CONSTRAINT c CHECK (public.looks_like_ri(id))");
	is($rc, 0, 'a check constraint of a reference table that calls a function is accepted (as before)') or diag $err;
	($rc, $out, $err) = run_sql($n1, "INSERT INTO public.rimit VALUES (5, 1)");
	isnt($rc, 0, 'a statement that has the shape of a key check, run by a function of the user, is not left to this node: the write fails');
	is(q1($n1, 'SELECT count(*) FROM public.rimit'), '0', '...and nothing was written');
	is(dump_of($n2, 'public.rimit', 'id::text'), '', '...on any node');
	# The statement of the user, run by himself.
	is(q1($n2, "SELECT count(*) FROM public.dimit x WHERE id IN (SELECT id FROM public.dimit)"), '30', 'a user statement reads every group');
}


# ---------------------------------------------------------------------------
# A move of a bucket: the key is enforced while it copies, and after it.
# ---------------------------------------------------------------------------
{
	make_pair('mv', 'ON DELETE CASCADE ON UPDATE CASCADE');
	make_pair('mvn', 'ON DELETE NO ACTION');
	my $c = coloc('public.dmv');
	is(coloc('public.dmvn'), $c, 'move: both tables are in one colocation group');
	my $bucket = q1($n1, "SELECT kingres.bucket_for($c, min(id)) FROM public.dmv WHERE pid = 2");
	my $flag = "$tmp/mv_catchup";
	open(my $fh, '>', $flag) or die;
	close($fh);
	my $mover = IPC::Run::start(
		[ 'psql', '-X', '-q', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres', '-c',
		  "SET kingres_shard.fault_injection = 'move_after_catchup:wait:$flag'; SET kingres_shard.move_switch_timeout = '20s'; SELECT kingres.move_bucket($c, $bucket, 2)" ],
		'>', \my $mout, '2>', \my $merr);
	ok(wait_for_fault_file("$flag.reached", 180), 'move: the copy of the bucket is complete');

	# The rows of the moving bucket are on the source (attached) and on the target (a copy).
	my ($rc, $out, $err) = run_sql($n1, "DELETE FROM public.rmvn WHERE id = 2");
	isnt($rc, 0, 'move: NO ACTION refuses while the bucket is being copied (the row is referenced from group 1)');
	like($err, $fkerr, '...by the foreign key');
	($rc, $out, $err) = run_sql($n1, "DELETE FROM public.rmv WHERE id = 2");
	is($rc, 0, 'move: CASCADE deletes the rows of group 1 while the bucket is being copied') or diag $err;
	is(pids('public.dmv'), '1x3,3x3', 'move: ...everywhere');
	($rc, $out, $err) = run_sql($n3, "UPDATE public.rmv SET id = 50 WHERE id = 3");
	is($rc, 0, 'move: a key changes while the bucket is being copied') or diag $err;
	unlink($flag);
	$mover->finish;
	is($merr, '', 'move: the move completed') or diag $merr;
	is(pids('public.dmv'), '1x3,50x3', 'move: the rows are the same after the move, on every node');
	is(local_counts('public.dmv') =~ /^\d+\/\d+\/\d+$/ ? 1 : 0, 1, 'move: counts per node are readable');
	my $sum = 0;
	$sum += $_ for split /\//, local_counts('public.dmv');
	is($sum, 6, 'move: each row is in one place only');
	# After the move the bucket belongs to node 3, whose copy of the bucket is attached now.
	($rc, $out, $err) = run_sql($n2, "DELETE FROM public.rmvn WHERE id = 3");
	isnt($rc, 0, 'move: NO ACTION still refuses after the move (the row is referenced from group 2)');
	like($err, $fkerr, '...by the foreign key');
	($rc, $out, $err) = run_sql($n2, "DELETE FROM public.rmv WHERE id = 50");
	is($rc, 0, 'move: CASCADE works after the move') or diag $err;
	is(pids('public.dmv'), '1x3', 'move: and removes the rows of the group that moved in');
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_moved_buckets(true)');
	copies_equal('public.rmv', 'id::text', 'move');
}

is(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || node_id || ' ' || object, ', '), '') FROM kingres.check_cluster()}),
	'', 'check_cluster finds nothing at the end');

done_testing();
