# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/283_orphan_dependents.pl: objects of the tenant that depend on a bucket partition do not stop the cleanup for ever, and the cleanup never drops them.
#
# The owner of a distributed table can make objects that depend on one of its bucket partitions: a view, a materialized view, a table with a foreign key to it, a
# function that takes its row type.  After the bucket moves (or splits) they stay with the old copy, the orphan table of schema kingres.  A plain DROP TABLE refuses
# it, and the cleanup failed with that error at every pass of the recovery worker (the move stayed in "cleaning" and nothing said why).  The cleanup does not use
# CASCADE: it would drop the objects of a tenant as a superuser.  It leaves the orphan, says in the record of the move (or split) what depends on it, and
# check_cluster() reports it (orphan_blocked); when the owner has dropped the objects the next pass drops the orphan.
#
#   A. a move: the four kinds of dependent; the cleanup leaves the orphan and every object of the tenant, without a warning; the record and check_cluster() name them
#   B. the owner drops them one by one: the orphan stays while one is left, goes at the next pass after the last, the move is done and the check is clean
#   C. the recovery worker does the same by itself (a pass is the same call)
#   D. a split: the old partition with a view on it; same record, same check, same end
#   E. what the extension refuses on a partition (triggers, rules, policies, indexes) is still refused, so none of them can be a dependent

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @nodes = make_cluster('od', 3, logical => 1, recovery_interval => 0,
	extra => "max_connections = 100\nkingres_shard.node_timeout = 5\nkingres_shard.move_cleanup_delay = 1\n");
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }
# as the tenant, on one node
sub tenant { my ($node, $sql) = @_; return run_sql($node, "SET ROLE tenant; $sql"); }

q1($_, q{CREATE ROLE tenant LOGIN; GRANT CREATE ON SCHEMA public TO tenant}) for @nodes;
q1($n1, q{
	CREATE TABLE public.m1 (id int PRIMARY KEY, v text);
	INSERT INTO public.m1 SELECT g, 'v' || g FROM generate_series(1, 400) g;
	SELECT kingres.create_distributed_table('public.m1', 'id', bucket_count => 8);
	ALTER TABLE public.m1 OWNER TO tenant;
});
my $C = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.m1'::regclass});
sub owner { my ($b) = @_; return q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $C AND bucket = $b"); }
sub other { my ($b) = @_; return (owner($b) + 1) % 3; }
sub clean { return q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || coalesce(object, ''), '; '), '') FROM kingres.check_cluster()}); }
sub blocked { return q1($n1, q{SELECT coalesce(string_agg(object || ': ' || detail, ' | '), '') FROM kingres.check_cluster() WHERE check_name = 'orphan_blocked'}); }
sub orphans { my ($n, $prefix) = @_; return q1($n, "SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE '$prefix%' AND relkind IN ('r', 'p') AND NOT relispartition"); }
sub objects
{
	my ($n) = @_;
	return q1($n, q{SELECT (SELECT count(*) FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname IN ('v1', 'mv1', 'child_fk'))
	                     || ',' || (SELECT count(*) FROM pg_proc WHERE proname = 'rowfn')});
}
my $digest = q1($n1, q{SELECT count(*) || ',' || sum(id) FROM public.m1});

# ---------------------------------------------------------------------------
# A. a move whose old copy has objects of the tenant on it
# ---------------------------------------------------------------------------
my ($b, $src, $tgt, $move, $on);
{
	$b = 1;
	$src = owner($b);
	$tgt = other($b);
	$on = $nodes[$src];
	my $part = q1($on, q{SELECT relname FROM pg_class WHERE relname = 'm1_b0001'});
	is($part, 'm1_b0001', 'A: the partition of bucket 1 is on its owner');
	for my $sql ("CREATE VIEW public.v1 AS SELECT id FROM public.$part",
				 "CREATE MATERIALIZED VIEW public.mv1 AS SELECT id FROM public.$part",
				 "CREATE TABLE public.child_fk (id int REFERENCES public.$part (id))",
				 "CREATE FUNCTION public.rowfn(r public.$part) RETURNS int LANGUAGE sql AS 'SELECT 1'")
	{
		my ($rc, $out, $err) = tenant($on, $sql);
		is($rc, 0, "A: the tenant makes: $sql") or diag($err);
	}
	is(objects($on), '3,1', 'A: the four objects exist');
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($C, $b, $tgt)");
	is($rc, 0, 'A: the bucket moves') or diag($err);
	$move = q1($n1, q{SELECT max(move_id) FROM kingres.bucket_move});
	is(owner($b), $tgt, 'A: it is on its target');
	is(orphans($on, "orphan_${move}_"), '1', 'A: the old copy waits as an orphan');

	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is($out, '0', 'A: the cleanup finishes no move') or diag($err);
	unlike($err, qr/WARNING|ERROR/, 'A: and does not warn: it is not an error');
	is(orphans($on, "orphan_${move}_"), '1', 'A: the orphan is still there');
	is(objects($on), '3,1', 'A: and every object of the tenant is still there (nothing is dropped with CASCADE)');
	is(q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $move"), 'cleaning', 'A: the move is still cleaning');
	like(q1($n1, "SELECT detail FROM kingres.bucket_move WHERE move_id = $move"), qr/^the old copy cannot be dropped, objects of its owner depend on it: /, 'A: its record says why');
	my $detail = q1($n1, "SELECT detail FROM kingres.bucket_move WHERE move_id = $move");
	like($detail, qr/v1/, 'A: the record names the view');
	like($detail, qr/mv1/, 'A: the materialized view');
	like($detail, qr/child_fk/, 'A: the foreign key');
	like($detail, qr/rowfn/, 'A: the function');
	like(blocked(), qr/^bucket move $move: the old copy cannot be dropped/, 'A: check_cluster() reports it as orphan_blocked');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'orphan_blocked'}), '1', 'A: once');
	# the tenant's objects are the tenant's: the owner of the view is the tenant
	is(q1($on, q{SELECT relowner::regrole FROM pg_class WHERE relname = 'v1'}), 'tenant', 'A: the view still belongs to the tenant');
}

# ---------------------------------------------------------------------------
# C. the worker's pass is the same call: no error, no warning, the same state, however often
# ---------------------------------------------------------------------------
{
	for my $i (1 .. 3)
	{
		my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.resume_bucket_moves()});
		is($rc, 0, "C: a recovery pass ($i) runs") or diag($err);
		unlike($err, qr/WARNING|ERROR/, "C: and does not warn ($i)");
	}
	is(orphans($on, "orphan_${move}_"), '1', 'C: the orphan is still there');
	is(objects($on), '3,1', 'C: and so is every object of the tenant');
}

# ---------------------------------------------------------------------------
# B. the owner drops the dependents: the orphan goes at the next pass after the last
# ---------------------------------------------------------------------------
{
	my @steps = ('DROP VIEW public.v1', 'DROP MATERIALIZED VIEW public.mv1', 'DROP TABLE public.child_fk', 'DROP FUNCTION public.rowfn');
	my @left = ('mv1', 'child_fk', 'rowfn', undef);
	for my $i (0 .. 3)
	{
		my ($rc, $out, $err) = tenant($on, $steps[$i]);
		is($rc, 0, "B: the tenant drops: $steps[$i]") or diag($err);
		($rc, $out, $err) = run_sql($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
		if (defined $left[$i])
		{
			is($out, '0', "B: the orphan stays while $left[$i] is left");
			like(q1($n1, "SELECT detail FROM kingres.bucket_move WHERE move_id = $move"), qr/$left[$i]/, "B: and the record names $left[$i]");
		}
		else
		{
			is($out, '1', 'B: the orphan goes at the next pass after the last dependent');
		}
	}
	is(orphans($on, "orphan_${move}_"), '0', 'B: the orphan is gone');
	is(q1($n1, "SELECT phase || ',' || coalesce(detail, 'null') FROM kingres.bucket_move WHERE move_id = $move"), 'done,null', 'B: the move is done, with no detail');
	is(blocked(), '', 'B: check_cluster() does not report it any more');
	is(clean(), '', 'B: and finds nothing');
	is(q1($_, q{SELECT count(*) || ',' || sum(id) FROM public.m1}), $digest, 'B: every row is there') for @nodes;
}

# ---------------------------------------------------------------------------
# D. a split
# ---------------------------------------------------------------------------
{
	my $sb = 2;
	my $so = $nodes[owner($sb)];
	my ($rc, $out, $err) = tenant($so, q{CREATE VIEW public.sv1 AS SELECT id FROM public.m1_b0002});
	is($rc, 0, 'D: the tenant makes a view on the partition of bucket 2') or diag($err);
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.split_bucket($C, $sb, 2)");
	is($rc, 0, 'D: the bucket is split') or diag($err);
	my $sid = q1($n1, q{SELECT max(split_id) FROM kingres.bucket_split});
	is(orphans($so, "orphan_s${sid}_"), '1', 'D: the old partition waits as an orphan');
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.cleanup_bucket_splits(true)});
	is($out, '0', 'D: the cleanup finishes no split') or diag($err);
	unlike($err, qr/WARNING|ERROR/, 'D: and does not warn');
	is(orphans($so, "orphan_s${sid}_"), '1', 'D: the orphan is still there');
	is(q1($so, q{SELECT count(*) FROM pg_class WHERE relname = 'sv1'}), '1', 'D: and so is the view');
	is(q1($n1, "SELECT phase FROM kingres.bucket_split WHERE split_id = $sid"), 'cleaning', 'D: the split is still cleaning');
	like(q1($n1, "SELECT detail FROM kingres.bucket_split WHERE split_id = $sid"), qr/^the old partitions cannot be dropped, objects of their owner depend on them: .*sv1/, 'D: the record says why');
	like(blocked(), qr/^bucket split $sid: the old partitions cannot be dropped/, 'D: check_cluster() reports it');
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.resume_bucket_splits(), kingres.resume_bucket_moves()});
	unlike($err, qr/WARNING|ERROR/, 'D: a recovery pass does not warn');
	($rc, $out, $err) = tenant($so, q{DROP VIEW public.sv1});
	is($rc, 0, 'D: the tenant drops the view') or diag($err);
	is(q1($n1, q{SELECT kingres.cleanup_bucket_splits(true)}), '1', 'D: the next pass finishes the split');
	is(orphans($so, "orphan_s${sid}_"), '0', 'D: the orphan is gone');
	is(q1($n1, "SELECT phase || ',' || coalesce(detail, 'null') FROM kingres.bucket_split WHERE split_id = $sid"), 'done,null', 'D: the split is done, with no detail');
	is(clean(), '', 'D: check_cluster() finds nothing');
	is(q1($_, q{SELECT count(*) || ',' || sum(id) FROM public.m1}), $digest, 'D: every row is there') for @nodes;
}

# ---------------------------------------------------------------------------
# E. the tenant cannot attach to a partition what would run code or what the extension refuses
# ---------------------------------------------------------------------------
{
	my $on2 = $nodes[owner(3)];
	for my $sql ("CREATE TRIGGER tr1 BEFORE INSERT ON public.m1_b0003 FOR EACH ROW EXECUTE FUNCTION pg_catalog.suppress_redundant_updates_trigger()",
				 "CREATE RULE r1 AS ON INSERT TO public.m1_b0003 DO ALSO NOTIFY x",
				 "CREATE POLICY p1 ON public.m1_b0003 USING (true)",
				 "CREATE INDEX ix1 ON public.m1_b0003 (v)")
	{
		my ($rc, $out, $err) = tenant($on2, $sql);
		isnt($rc, 0, "E: refused: $sql");
		like($err, qr/not supported on distributed tables/, 'E: by the extension');
	}
	is(clean(), '', 'E: check_cluster() finds nothing');
}

done_testing();
