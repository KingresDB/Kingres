# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/023_scoped_bypass.pl
#
# Review finding S2-14: the bypass of routing and of the drop guard is limited to
# what the extension itself issues.  User code that runs while an authorized DDL
# statement executes (a function in a CHECK constraint) is routed like any other
# statement and cannot drop a distributed table behind the guard's back.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my ($n1, $n2, $n3) = make_cluster('sb', 3);
my @nodes = ($n1, $n2, $n3);

$n1->safe_psql('postgres', q{
	CREATE TABLE public.dd (id int PRIMARY KEY, v int);
	CREATE TABLE public.victim (id int PRIMARY KEY);
	CREATE TABLE public.rr (id int PRIMARY KEY, v int);
	SELECT kingres.create_distributed_table('public.dd', 'id', bucket_count => 6);
	SELECT kingres.create_distributed_table('public.victim', 'id', bucket_count => 6);
	SELECT kingres.create_reference_table('public.rr');
});
$n1->safe_psql('postgres', join('', map { "INSERT INTO public.dd VALUES ($_, $_);\n" } (1 .. 12)));

# User code that acts only on the node where the statement was issued.
my $fns = q{
	CREATE FUNCTION public.f_write(x int) RETURNS bool LANGUAGE plpgsql VOLATILE AS $$
	BEGIN
		IF (SELECT node_id FROM kingres.local_node) = 1 THEN
			INSERT INTO public.rr VALUES (7, 7) ON CONFLICT DO NOTHING;
		END IF;
		RETURN true;
	END $$;
	CREATE FUNCTION public.f_drop(x int) RETURNS bool LANGUAGE plpgsql VOLATILE AS $$
	BEGIN
		IF (SELECT node_id FROM kingres.local_node) = 1 AND to_regclass('public.victim') IS NOT NULL THEN
			EXECUTE 'DROP TABLE public.victim';
		END IF;
		RETURN true;
	END $$;
};
$_->safe_psql('postgres', $fns) for @nodes;

# A write to a reference table made by user code during an authorized DDL is routed:
# it reaches every node, not only this one.
{
	my ($rc, $out, $err) = run_sql($n1, 'ALTER TABLE public.dd ADD CONSTRAINT dd_w CHECK (public.f_write(id))');
	is($err, '', 'a CHECK constraint with a volatile function is added');
	for my $n (@nodes)
	{
		is($n->safe_psql('postgres', 'SELECT count(*) FROM public.rr WHERE id = 7'), '1',
			'S2-14: the write of the function reached ' . $n->name);
	}
}

# A DROP of a distributed table made by user code during an authorized DDL is a
# statement like any other: the hook sees it and propagates it, so the table is
# gone on every node (before the fix it passed the drop guard unseen and was
# dropped on this node only).
{
	my ($rc, $out, $err) = run_sql($n1, 'ALTER TABLE public.dd ADD CONSTRAINT dd_d CHECK (public.f_drop(id))');
	is($err, '', 'the constraint with a function that drops a table is added');
	for my $n (@nodes)
	{
		is($n->safe_psql('postgres', q{SELECT to_regclass('public.victim') IS NULL}), 't',
			'S2-14: the drop made by user code reached ' . $n->name);
		is($n->safe_psql('postgres', q{SELECT count(*) FROM kingres.dist_table d WHERE NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.oid = d.relid)}), '0',
			'and the catalog of ' . $n->name . ' has no row for a missing table');
		is($n->safe_psql('postgres', q{SELECT count(*) FROM pg_class WHERE relname ~ '^victim'}), '0',
			'and no partition of it is left on ' . $n->name);
	}
}

# The statements the extension and PostgreSQL issue for a DDL still work: a foreign key.
{
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.child (id int PRIMARY KEY, dd_id int);
		SELECT kingres.create_distributed_table('public.child', 'id', colocate_with => 'public.dd');
	});
	my ($rc, $out, $err) = run_sql($n1, 'ALTER TABLE public.child ADD CONSTRAINT child_fk FOREIGN KEY (id) REFERENCES public.dd (id)');
	is($err, '', 'a foreign key between colocated tables is still validated and added');
	$n1->safe_psql('postgres', 'ALTER TABLE public.child RENAME TO child2; ALTER TABLE public.child2 RENAME TO child');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM pg_class WHERE relname ~ '^child_b[0-9]{4}$'}), '2',
		'renames of a distributed table still rename the partitions on node 1');
}

done_testing();
