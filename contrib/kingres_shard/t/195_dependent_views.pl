# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/195_dependent_views.pl: kingres_shard.dependent_views = 'recreate' (design/shard-phase9.md, increment 9.8): the plain views that use a table
# are dropped in the switch that replaces the table and made again on the new one, with their definition, owner, options (security_barrier,
# check_option), privileges and comments, on every node that has them.
#
#   A. the default ('refuse'): a view stops the operation and nothing changes
#   B. every operation recreates: create_distributed_table, alter, undistribute (blocking), and the three online ones; views on views; a view on
#      another node than the map group primary; the view of a role that has no other right; the views work and are identical afterwards
#   C. a view that cannot be recreated (a materialized view on it, a trigger on a view, a privilege on a column of a view) refuses before anything changes
#   D. an error in the switch leaves the views as they were; a reader on another node waits for the switch and then reads through the view

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql);

my ($n1, $n2, $n3) = make_cluster('dv', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }
$_->safe_psql('postgres', q{CREATE ROLE vw_owner; CREATE ROLE vw_reader}) for @nodes;

my $REC = 'SET kingres_shard.dependent_views = recreate; ';

# everything that makes a view what it is, in one string, in a fixed order
my $fpsql = q{
SELECT coalesce(string_agg(c.relname || ':' || pg_get_viewdef(c.oid) || ':' || pg_get_userbyid(c.relowner)
         || ':' || coalesce((SELECT string_agg(x::text, ',' ORDER BY x::text) FROM unnest(c.relacl) x), '-')
         || ':' || coalesce(c.reloptions::text, '-') || ':' || coalesce(obj_description(c.oid, 'pg_class'), '-')
         || ':' || coalesce((SELECT string_agg(a.attname || '=' || col_description(c.oid, a.attnum), ',' ORDER BY a.attnum) FROM pg_attribute a
                              WHERE a.attrelid = c.oid AND a.attnum > 0 AND col_description(c.oid, a.attnum) IS NOT NULL), '-'), E'\n' ORDER BY c.relname), '')
  FROM pg_class c WHERE c.relkind = 'v' AND c.relnamespace = 'public'::regnamespace AND c.relname LIKE 'v\_%'};
sub fp { my ($n) = @_; return q1($n, $fpsql); }

my $tn = 0;
# a table with views on it: v_base (security barrier, owner, grants with grant option, comments), v_top on v_base (check option), v_agg
sub new_table
{
	my ($name) = @_;
	$n1->safe_psql('postgres', qq{
		CREATE TABLE public.$name (id int PRIMARY KEY, customer int NOT NULL, qty int, note text);
		INSERT INTO public.$name SELECT g, g % 13, g % 50, 'n' || g FROM generate_series(1, 300) g;
		GRANT SELECT ON public.$name TO vw_owner;
		CREATE VIEW public.v_${name}_base WITH (security_barrier = true, security_invoker = true) AS SELECT id, customer, qty FROM public.$name WHERE qty > 0;
		CREATE VIEW public.v_${name}_atop WITH (check_option = local) AS SELECT id, qty FROM public.v_${name}_base WHERE qty < 1000;
		CREATE VIEW public.v_${name}_agg AS SELECT customer, count(*) AS c, sum(qty) AS s FROM public.$name GROUP BY customer;
		ALTER VIEW public.v_${name}_base OWNER TO vw_owner;
		GRANT SELECT ON public.v_${name}_base TO vw_reader WITH GRANT OPTION;
		GRANT SELECT ON public.v_${name}_atop TO PUBLIC;
		GRANT SELECT, UPDATE ON public.v_${name}_agg TO vw_reader;
		COMMENT ON VIEW public.v_${name}_base IS 'the base view';
		COMMENT ON COLUMN public.v_${name}_base.qty IS 'the quantity';
	});
}
sub answers { my ($n, $name) = @_; return q1($n, "SELECT (SELECT count(*) || ',' || sum(qty) FROM public.v_${name}_base) || '|' || (SELECT count(*) FROM public.v_${name}_atop) || '|' || (SELECT sum(c) FROM public.v_${name}_agg)"); }
sub bound
{
	my ($n, $view, $table) = @_;
	return q1($n, "SELECT count(*) FROM pg_depend d JOIN pg_rewrite rw ON rw.oid = d.objid WHERE d.classid = 'pg_rewrite'::regclass AND d.refclassid = 'pg_class'::regclass AND d.refobjid = 'public.$table'::regclass AND rw.ev_class = 'public.$view'::regclass") > 0 ? 'bound' : 'unbound';
}
sub only_views { my ($n, $name) = @_; return q1($n, "SELECT string_agg(relname, ',' ORDER BY relname) FROM pg_class WHERE relkind = 'v' AND relname LIKE 'v\\_${name}\\_%'"); }

# ---------------------------------------------------------------------------
# A. the default refuses
# ---------------------------------------------------------------------------
{
	new_table('a1');
	my $before = fp($n1);
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.a1', 'id', bucket_count => 3)});
	isnt($rc, 0, 'A: a view stops create_distributed_table by default'); like($err, qr/view public\.v_a1_(base|agg) depends on the table/, 'A: and is named');
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid::text = 'a1'}), '0', 'A: the table is not distributed');
	is(fp($n1), $before, 'A: the views are as they were');
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table_concurrently('public.a1', 'id', bucket_count => 3)});
	isnt($rc, 0, 'A: and the online variant'); like($err, qr/depends on the table/, 'A: with the same reason');
	is(only_views($n1, 'a1'), 'v_a1_agg,v_a1_atop,v_a1_base', 'A: the views are all there');
}

# ---------------------------------------------------------------------------
# B. the operations recreate
# ---------------------------------------------------------------------------
{
	new_table('b1');
	my ($before, $ans) = (fp($n1), answers($n1, 'b1'));
	my ($rc, $out, $err) = run_sql($n1, $REC . q{SELECT kingres.create_distributed_table('public.b1', 'id', bucket_count => 3)});
	is($rc, 0, 'B: create_distributed_table recreates the views') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.b1'::regclass}), '1', 'B: the table is distributed');
	is(fp($n1), $before, 'B: the views are identical: definition, owner, options, privileges, comments');
	is(answers($n1, 'b1'), $ans, 'B: and give the same answers');
	is(q1($n1, q{SELECT count(*) FROM pg_depend d JOIN pg_rewrite rw ON rw.oid = d.objid WHERE d.refobjid = 'public.b1'::regclass AND d.classid = 'pg_rewrite'::regclass AND rw.ev_class <> d.refobjid}) > 0 ? 1 : 0, '1', 'B: they are bound to the new table');
	is(prepared_count($_), '0', 'B: nothing prepared on ' . $_->name) for @nodes;
	# a view on another node than the map group primary (made without the statements of the extension)
	q1($n2, q{SET kingres_shard.enable_routing = off; CREATE VIEW public.v_b1_n2 AS SELECT customer, sum(qty) AS s FROM public.b1 GROUP BY customer; GRANT SELECT ON public.v_b1_n2 TO vw_reader; COMMENT ON VIEW public.v_b1_n2 IS 'on node 2'});
	q1($n2, q{SET kingres_shard.enable_routing = off; CREATE VIEW public.v_b1_n2b AS SELECT * FROM public.v_b1_n2 WHERE s > 0});
	my ($f1, $f2) = (fp($n1), fp($n2));
	my $a2 = q1($n2, q{SELECT count(*) || ',' || sum(s) FROM public.v_b1_n2b});
	($rc, $out, $err) = run_sql($n1, $REC . q{SELECT kingres.alter_distributed_table('public.b1', bucket_count => 6)});
	is($rc, 0, 'B: alter_distributed_table recreates the views, on every node') or diag($err);
	is(q1($n1, q{SELECT bucket_count FROM kingres.colocation WHERE colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.b1'::regclass)}), '6', 'B: the table has six buckets');
	is(fp($n1), $f1, 'B: node 1 has its views, identical');
	is(fp($n2), $f2, 'B: node 2 has its views (a view on a view among them), identical');
	is(q1($n2, q{SELECT count(*) || ',' || sum(s) FROM public.v_b1_n2b}), $a2, 'B: and the view of node 2 gives the same answer');
	is(bound($n2, 'v_b1_n2', 'b1'), 'bound', 'B: the view of node 2 is bound to the new table');
	# the table will be on one node: the views of node 2 cannot follow it
	($rc, $out, $err) = run_sql($n1, $REC . q{SELECT kingres.undistribute_table('public.b1')});
	isnt($rc, 0, 'B: undistribute_table is refused while a view exists on node 2'); like($err, qr/node 2: the views on the table cannot be recreated there: the table will be on node 1 only/, 'B: with the reason');
	is(fp($n1), $f1, 'B: and nothing changed'); is(fp($n2), $f2, 'B: not on node 2 either');
	q1($n2, q{DROP VIEW public.v_b1_n2b; DROP VIEW public.v_b1_n2});
	($rc, $out, $err) = run_sql($n1, $REC . q{SELECT kingres.undistribute_table('public.b1')});
	is($rc, 0, 'B: undistribute_table recreates the views') or diag($err);
	is(fp($n1), $f1, 'B: node 1: identical');
	is(answers($n1, 'b1'), $ans, 'B: the answers are the same');
	q1($n1, q{SELECT kingres.cleanup_table_operations(true)});
}
# the online operations
{
	new_table('b2');
	my ($before, $ans) = (fp($n1), answers($n1, 'b2'));
	my ($rc, $out, $err) = run_sql($n1, $REC . q{SELECT kingres.create_distributed_table_concurrently('public.b2', 'id', bucket_count => 3)});
	is($rc, 0, 'B: create_distributed_table_concurrently recreates the views') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.b2'::regclass}), '1', 'B: online: the table is distributed');
	is(fp($n1), $before, 'B: online: identical');
	is(answers($n1, 'b2'), $ans, 'B: online: the same answers');
	is(bound($n1, 'v_b2_agg', 'b2'), 'bound', 'B: online: a view is bound to the new table, not to the old copy');
	q1($n2, q{SET kingres_shard.enable_routing = off; CREATE VIEW public.v_b2_n2 AS SELECT customer, sum(qty) AS s FROM public.b2 GROUP BY customer; GRANT SELECT ON public.v_b2_n2 TO vw_reader});
	my ($f1, $f2) = (fp($n1), fp($n2));
	($rc, $out, $err) = run_sql($n1, $REC . q{SELECT kingres.alter_distributed_table_concurrently('public.b2', bucket_count => 6)});
	is($rc, 0, 'B: alter_distributed_table_concurrently recreates the views on every node') or diag($err);
	is(fp($n1), $f1, 'B: online alter: node 1 identical');
	is(fp($n2), $f2, 'B: online alter: node 2 identical');
	is(answers($n1, 'b2'), $ans, 'B: online alter: the same answers');
	is(bound($n1, 'v_b2_agg', 'b2'), 'bound', 'B: online alter: a view of node 1 is bound to the new table');
	is(bound($n2, 'v_b2_n2', 'b2'), 'bound', 'B: online alter: a view of node 2 is bound to the new table');
	($rc, $out, $err) = run_sql($n1, $REC . q{SELECT kingres.undistribute_table_concurrently('public.b2')});
	isnt($rc, 0, 'B: undistribute_table_concurrently is refused while a view exists on node 2'); like($err, qr/node 2: the views on the table cannot be recreated there/, 'B: online: with the reason');
	q1($n2, q{DROP VIEW public.v_b2_n2});
	($rc, $out, $err) = run_sql($n1, $REC . q{SELECT kingres.undistribute_table_concurrently('public.b2')});
	is($rc, 0, 'B: undistribute_table_concurrently recreates the views') or diag($err);
	is(fp($n1), $f1, 'B: online undistribute: identical');
	is(answers($n1, 'b2'), $ans, 'B: online undistribute: the same answers');
	q1($n1, q{SELECT kingres.cleanup_table_operations(true)});
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'B: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# C. what cannot be recreated refuses before anything changes
# ---------------------------------------------------------------------------
for my $case (
	['a materialized view on the table', q{CREATE MATERIALIZED VIEW public.mv_c1 AS SELECT customer FROM public.TBL}, 'materialized view public.mv_c1'],
	['a materialized view on a view', q{CREATE MATERIALIZED VIEW public.mv_c2 AS SELECT id FROM public.v_TBL_base}, 'materialized view public.mv_c2'],
	['a trigger on a view', q{CREATE FUNCTION public.vt_fn() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'; CREATE TRIGGER vt INSTEAD OF UPDATE ON public.v_TBL_agg FOR EACH ROW EXECUTE FUNCTION public.vt_fn()}, 'has a trigger'],
	['a privilege on a column of a view', q{GRANT SELECT (customer) ON public.v_TBL_agg TO vw_reader}, 'privileges on columns'],
	['a rule of its own on a view', q{CREATE RULE vr AS ON INSERT TO public.v_TBL_agg DO INSTEAD NOTHING}, 'rule of its own'],
	)
{
	my $name = 'c' . ++$tn;
	new_table($name);
	(my $setup = $case->[1]) =~ s/TBL/$name/g;
	$n1->safe_psql('postgres', $setup);
	my $before = fp($n1);
	my $ops = q1($n1, 'SELECT count(*) FROM kingres.table_operation');
	for my $call ("kingres.create_distributed_table('public.$name', 'id', bucket_count => 3)", "kingres.create_distributed_table_concurrently('public.$name', 'id', bucket_count => 3)")
	{
		my ($rc, $out, $err) = run_sql($n1, $REC . "SELECT $call");
		isnt($rc, 0, "C: $case->[0]: $call is refused");
		like($err, qr/\Q$case->[2]\E/, "C: $case->[0]: the message names it") or diag($err);
	}
	is(q1($n1, qq{SELECT count(*) FROM kingres.dist_table WHERE relid::text = '$name'}), '0', "C: $case->[0]: the table is not distributed");
	is(fp($n1), $before, "C: $case->[0]: the views are as they were");
	is(q1($n1, 'SELECT count(*) FROM kingres.table_operation'), $ops, "C: $case->[0]: no operation was even recorded");
	is(q1($n1, q{SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\_shadow%'}), '0', "C: $case->[0]: no shadow");
}
# the same for a distributed table, with a view on another node
{
	new_table('c9');
	run_sql($n1, $REC . q{SELECT kingres.create_distributed_table('public.c9', 'id', bucket_count => 3)});
	q1($n3, q{SET kingres_shard.enable_routing = off; CREATE MATERIALIZED VIEW public.mv_c9 AS SELECT customer FROM public.c9});
	my $before = fp($n1);
	for my $call ("kingres.alter_distributed_table('public.c9', bucket_count => 6)", "kingres.alter_distributed_table_concurrently('public.c9', bucket_count => 6)", "kingres.undistribute_table('public.c9')", "kingres.undistribute_table_concurrently('public.c9')")
	{
		my ($rc, $out, $err) = run_sql($n1, $REC . "SELECT $call");
		isnt($rc, 0, "C: a materialized view on node 3: $call is refused");
		like($err, qr/node 3: materialized view public\.mv_c9/, 'C: the message names the node and the view') or diag($err);
	}
	is(q1($n1, q{SELECT bucket_count FROM kingres.colocation WHERE colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.c9'::regclass)}), '3', 'C: the table is as it was');
	is(fp($n1), $before, 'C: the views of node 1 are as they were');
	is(q1($n1, q{SELECT count(*) FROM kingres.table_operation WHERE phase NOT IN ('failed', 'cancelled', 'done')}), '0', 'C: no operation was left');
}

# ---------------------------------------------------------------------------
# D. errors in the switch, and a reader that races it
# ---------------------------------------------------------------------------
{
	new_table('d1');
	run_sql($n1, $REC . q{SELECT kingres.create_distributed_table('public.d1', 'id', bucket_count => 3)});
	q1($n2, q{SET kingres_shard.enable_routing = off; CREATE VIEW public.v_d1_n2 AS SELECT customer, sum(qty) AS s FROM public.d1 GROUP BY customer; GRANT SELECT ON public.v_d1_n2 TO vw_reader});
	my ($f1, $f2, $ans) = (fp($n1), fp($n2), answers($n1, 'd1'));
	for my $point (qw(rebuild_switch_after_local_swap rebuild_switch_after_remote_swap rebuild_switch_after_prepare rebuild_after_shadow))
	{
		my ($rc, $out, $err) = run_sql($n1, $REC . "SET kingres_shard.fault_injection = '$point:error'; SELECT kingres.alter_distributed_table_concurrently('public.d1', bucket_count => 6)");
		isnt($rc, 0, "D $point: the call fails");
		is(fp($n1), $f1, "D $point: the views of node 1 are as they were");
		is(fp($n2), $f2, "D $point: and those of node 2");
		is(answers($n1, 'd1'), $ans, "D $point: and give the answers");
	}
	for my $call ("kingres.alter_distributed_table('public.d1', bucket_count => 6)")
	{
		my ($rc, $out, $err) = run_sql($n1, $REC . "SET kingres_shard.fault_injection = 'alter_after_drop:error'; SELECT $call");
		isnt($rc, 0, 'D: an error after the drop of the old tables of the blocking alter');
		is(fp($n1), $f1, 'D: node 1: the views are as they were'); is(fp($n2), $f2, 'D: node 2: the views are as they were');
	}
	# a reader on node 2 reads through its view while the call is held in the switch, before the swap
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_d_$$";
	append_to_file($gate, 'x');
	my ($o, $e) = ('', '');
	my $hh = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
		'-c', "SET kingres_shard.dependent_views = recreate", '-c', "SET kingres_shard.fault_injection = 'rebuild_switch_before_swap:wait:$gate'",
		'-c', q{SELECT kingres.alter_distributed_table_concurrently('public.d1', bucket_count => 6)} ], '>', \$o, '2>', \$e);
	ok(wait_for_fault_file("$gate.reached", 60), 'D: the alter is held in the switch');
	my $r = start_psql($n2, q{SELECT count(*) || ',' || sum(s) FROM public.v_d1_n2});
	my $waited = 0;
	for (my $i = 0; $i < 300 && !$waited; $i++) { for my $n (@nodes) { $waited = 1 if q1($n, 'SELECT count(*) FROM pg_locks WHERE NOT granted') > 0; } select(undef, undef, undef, 0.1); }
	ok($waited, 'D: a reader of the view on node 2 waits for the locks of the switch');
	unlink $gate;
	IPC::Run::finish($hh);
	is($e, '', 'D: the held call ends') or diag($e);
	my ($rout, $rerr) = finish_psql($r);
	is($rerr, '', 'D: the reader that waited goes on, through the new table') or diag($rerr);
	is(q1($n2, q{SELECT count(*) || ',' || sum(s) FROM public.v_d1_n2}), q1($n1, q{SELECT count(*) || ',' || sum(s) FROM (SELECT customer, sum(qty) AS s FROM public.d1 GROUP BY customer) x}), 'D: the view of node 2 gives the answer of the table');
	is(fp($n2), $f2, 'D: and is identical');
	is(fp($n1), $f1, 'D: node 1 too');
}
done_testing();
