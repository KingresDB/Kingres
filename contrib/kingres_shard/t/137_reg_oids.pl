# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/137_reg_oids.pl
#
# Review P2D-01 of step 5: a value of a reg* type (regclass, regproc, regtype, ...) is an OID, and the
# same object has another OID on every node.  The meaning of such a value is the origin's, on every
# path that the statement can take: the groups must never be asked to resolve an OID or a name of
# their own.  t/134 uses objects of the system catalog, whose OIDs are the same everywhere; here the
# object is a table that exists on every node with a different OID (node 1 made nine tables before it),
# the rows hold the OID that the origin gave, and every node in turn is the origin of the statements:
#
#   the row router (INSERT ... SELECT, a regclass parameter in the select list),
#   (the 2a statement that names one key goes to its group as text; see the note at the end of the loop),
#   the 2a distributed scan, the fragment scan (a closed query), the colocated INSERT ... SELECT.
#
# The answers are those of the origin's OID.  The test fails for the mutant that lets a parameter of a
# reg* type through the shippability rule (shippable.c, T_Param).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @n = make_cluster('ro', 3);
my $n1 = $n[0];
$n1->safe_psql('postgres', 'CREATE TABLE public.junk' . $_ . ' (x int)') for (1 .. 9);
$_->safe_psql('postgres', 'CREATE TABLE public.refobj (x int)') for @n;
my @oid = map { $_->safe_psql('postgres', q{SELECT 'public.refobj'::regclass::oid}) } @n;
isnt($oid[0], $oid[1], 'the same table has another OID on node 2');
isnt($oid[0], $oid[2], '... and on node 3');

$n1->safe_psql('postgres', q{
	CREATE TABLE public.d (id int PRIMARY KEY, g oid);
	SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 12);
	CREATE TABLE public.e (id int PRIMARY KEY, g oid);
	SELECT kingres.create_distributed_table('public.e', 'id', colocate_with => 'public.d');
});
$_->safe_psql('postgres', 'ANALYZE') for @n;

my $G = 'SET plan_cache_mode = force_generic_plan; ';

sub q1
{
	my ($node, $sql, $label, $want) = @_;
	my ($rc, $o, $e) = run_sql($node, $sql);
	is($rc, 0, "$label: no error") or diag($e);
	is($o, $want, "$label: the answer is the origin's");
	return $o;
}

for my $i (0 .. 2)
{
	my $o = $n[$i];
	my $name = $o->name;
	my $mine = $oid[$i];
	my $base = $i * 100;

	# the row router: twelve rows, each holds the OID that this node gave the table
	q1($o, $G . q{PREPARE ri(regclass) AS INSERT INTO public.d SELECT g + } . $base . q{, $1::oid FROM generate_series(1, 12) g; EXECUTE ri('public.refobj')},
		"$name router: INSERT ... SELECT with a regclass parameter",
		'');
	q1($o, qq{SELECT count(DISTINCT g) || '|' || min(g) FROM public.d WHERE id > $base AND id <= $base + 12}, "$name router: the stored OIDs", "1|$mine");

	# the distributed scan: a parameter of type regclass compared with a stored OID
	q1($o, $G . q{PREPARE sc(regclass) AS SELECT count(*) FROM public.d WHERE g = $1::oid AND id > } . $base . q{ AND id <= } . ($base + 12) . q{; EXECUTE sc('public.refobj')},
		"$name scan: a regclass parameter against a stored OID", 12);
	q1($o, $G . q{PREPARE sr(regclass) AS SELECT count(*) FROM public.d WHERE g::oid::regclass = $1 AND id > } . $base . q{ AND id <= } . ($base + 12) . q{; EXECUTE sr('public.refobj')},
		"$name scan: a regclass column expression against a regclass parameter", 12);
	q1($o, $G . q{PREPARE sa(regclass[]) AS SELECT count(*) FROM public.d WHERE g::oid::regclass = ANY ($1) AND id > } . $base . q{ AND id <= } . ($base + 12) . q{; EXECUTE sa(ARRAY['public.refobj']::regclass[])},
		"$name scan: a parameter that is an array of regclass", 12);

	# the fragment scan: a closed query with the parameter
	q1($o, $G . q{PREPARE cl(regclass) AS SELECT count(*) FROM (SELECT id FROM public.d WHERE g = $1::oid AND id > } . $base . q{ AND id <= } . ($base + 12) . q{ GROUP BY id) s; EXECUTE cl('public.refobj')},
		"$name closed query: a regclass parameter", 12);

	# the colocated INSERT ... SELECT that every group runs whole
	q1($o, $G . q{PREPARE is1(regclass) AS INSERT INTO public.e SELECT id, g FROM public.d WHERE g = $1::oid AND id > } . $base . q{ AND id <= } . ($base + 12) . q{; EXECUTE is1('public.refobj')},
		"$name colocated INSERT ... SELECT: a regclass parameter", '');
	q1($o, qq{SELECT count(*) FROM public.e WHERE id > $base AND id <= $base + 12}, "$name colocated INSERT ... SELECT: the rows that were copied", 12);

	# Not here: a statement that names one key goes to one group as the text the user wrote, and that group
	# resolves the names in it itself (the 2a path, unchanged: the meaning of a reg* value there is the name).
	# A raw OID that this node computed and stored is not an object of that group; it is stated in the README.
}

done_testing();
