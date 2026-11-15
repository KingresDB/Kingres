# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/021_owner_and_views.pl
#
# Review findings S2-13 and S2-15.
#
# S2-13: OWNER TO on a distributed table reaches the bucket partitions (and
# their indexes) on every node, so that the new owner can run the supported DDL.
#
# S2-15: a view over a distributed table is checked, by PostgreSQL, with the
# rights of the view's owner.  A fragment run on another node uses the rights of
# the session role, so a statement that needs another shard group through such
# a view is refused with a clear message (never run with the wrong rights);
# a security_invoker view and a statement whose buckets are all local work.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql);

my ($n1, $n2, $n3) = make_cluster('ov', 3);
my @nodes = ($n1, $n2, $n3);
$_->safe_psql('postgres', 'CREATE ROLE bob LOGIN; CREATE ROLE carol LOGIN; GRANT CREATE ON SCHEMA public TO bob') for @nodes;

$n1->safe_psql('postgres', q{
	CREATE TABLE public.t1 (id int PRIMARY KEY, a int);
	CREATE TABLE public.d3 (id int PRIMARY KEY, v text);
	SELECT kingres.create_distributed_table('public.t1', 'id', bucket_count => 12);
	SELECT kingres.create_distributed_table('public.d3', 'id', bucket_count => 6);
});
$n1->safe_psql('postgres', join('', map { "INSERT INTO public.t1 VALUES ($_, $_);\n" } (1 .. 24)));
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');

# ---------------------------------------------------------------------------
# S2-13
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', 'ALTER TABLE public.d3 OWNER TO bob');
	for my $n (@nodes)
	{
		is($n->safe_psql('postgres', q{SELECT count(*) FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner
			WHERE c.relname ~ '^d3(_b[0-9]{4})?(_pkey)?$|^d3_b[0-9]{4}_pkey$' AND r.rolname <> 'bob'}),
			'0', 'S2-13: the table, its partitions and their indexes are owned by the new owner on ' . $n->name);
	}
	my ($rc, $out, $err) = run_sql($n1, 'ALTER TABLE public.d3 ADD COLUMN zz int', extra_params => [ '-U', 'bob' ]);
	is($err, '', 'S2-13: the new owner can add a column') or diag($err);
	for my $n (@nodes)
	{
		is($n->safe_psql('postgres', q{SELECT count(*) FROM information_schema.columns WHERE table_name = 'd3' AND column_name = 'zz'}),
			'1', 'the column exists on ' . $n->name);
	}
	($rc, $out, $err) = run_sql($n1, 'CREATE INDEX d3_v_idx ON public.d3 (v)', extra_params => [ '-U', 'bob' ]);
	is($err, '', 'S2-13: and create an index');
}

# ---------------------------------------------------------------------------
# S2-15
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', q{
		CREATE VIEW public.vsec AS SELECT id, a FROM public.t1;
		CREATE VIEW public.vinv WITH (security_invoker = true) AS SELECT id, a FROM public.t1;
		GRANT SELECT ON public.vsec, public.vinv TO bob;
		GRANT SELECT ON public.t1 TO carol;
		GRANT SELECT ON public.vinv TO carol;
	});
	$_->safe_psql('postgres', q{
		CREATE OR REPLACE VIEW public.vsec AS SELECT id, a FROM public.t1;
		CREATE OR REPLACE VIEW public.vinv WITH (security_invoker = true) AS SELECT id, a FROM public.t1;
		GRANT SELECT ON public.vsec, public.vinv TO bob;
		GRANT SELECT ON public.vinv TO carol;
	}) for ($n2, $n3);

	my ($rc, $out, $err) = run_sql($n1, 'SELECT count(*) FROM public.vsec', extra_params => [ '-U', 'bob' ]);
	isnt($rc, 0, 'S2-15: a view of another owner over a distributed table, needing other groups, is refused');
	like($err, qr/is read through a view owned by role/, 'S2-15: with a clear message');
	unlike($err, qr/permission denied for table t1/, 'S2-15: not a permission error from another node');
	like($err, qr/security_invoker/, 'S2-15: that names the way out');

	# The key behind the view is not seen by the router: the statement is planned as a scan of
	# every group, so it is refused too (the owner of the view reads it on a single node).
	my ($kl) = keys_of_group($n1, 1, 0, 1);
	($rc, $out, $err) = run_sql($n1, "SELECT a FROM public.vsec WHERE id = $kl", extra_params => [ '-U', 'bob' ]);
	like($err, qr/is read through a view owned by role/, 'a restriction by key through the view does not help');
	my ($ownerrc, $ownerout, $ownererr) = run_sql($n1, "SELECT a FROM public.vsec WHERE id = $kl");
	is($ownerout, "$kl", 'the owner of the view reads the same row without a refusal');

	# A bucket of another group: forwarded, so the same refusal.
	my ($kr) = keys_of_group($n1, 1, 1, 1);
	($rc, $out, $err) = run_sql($n1, "SELECT a FROM public.vsec WHERE id = $kr", extra_params => [ '-U', 'bob' ]);
	like($err, qr/is read through a view owned by role/, 'S2-15: the same for a statement forwarded to another group');

	# A security_invoker view uses the rights of the caller on every node.
	($rc, $out, $err) = run_sql($n1, 'SELECT count(*) FROM public.vinv', extra_params => [ '-U', 'carol' ]);
	is($err, '', 'a security_invoker view works for a role with rights on the table');
	is($out, '24', 'and reads every group');
	($rc, $out, $err) = run_sql($n1, 'SELECT count(*) FROM public.vinv', extra_params => [ '-U', 'bob' ]);
	like($err, qr/permission denied for (table|view) (t1|vinv)/, 'a role without rights on the table is refused, as on a single node');
}

done_testing();
