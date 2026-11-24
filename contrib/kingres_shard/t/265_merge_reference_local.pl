# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/265_merge_reference_local.pl
#
# Phase 12, MERGE on distributed tables (design/analytics-phase12-queries.md, 7.5, 7.6, 14.2):
#
#   A. M6, a local target with a distributed source: each distributed table of USING is read through a sub-select with OFFSET 0 (so that
#      PostgreSQL does not ask the distributed scan for a row identifier), the target keeps its triggers.  Every answer is the single node's.
#   B. M4, a reference table as the target of a MERGE that reads reference tables, VALUES and constants: it runs on every node, in node id
#      order, with the writer locks of the reference tables; the copies on the three nodes are equal to each other and to the single node's.
#   C. M5, a reference table as the target of a MERGE whose source is a distributed table, a local table or a sub-select over them: this node
#      reads the source once and every node merges the same rows.
#   D. a ledger table as the target of M6: its capture trigger fires once for each row, and the ledger verifies.
#   E. forged fragments of a reference-table write that a node refuses; the refusals that name the missing condition.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('mf', extra => "shared_preload_libraries = 'kingres_shard,kingres_ledger'\nkingres_ledger.block_interval = 0\n");
my @nodes = $c->nodes;
my $single = $c->single;
my $turn = 0;

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }
sub local_table { my ($sql) = @_; $c->on_every_node($sql); $c->on_single($sql); }

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# the local target: the same rows on the node that runs the statement and on the single node before each statement
my $LOC_ROWS = q{TRUNCATE public.loc; INSERT INTO public.loc SELECT g, 'l' || g, g % 5 FROM generate_series(50, 130) g};
sub merge_local
{
	my ($name, $sql, %opt) = @_;
	my $k = $turn++ % 3;
	$nodes[$k]->safe_psql('postgres', $LOC_ROWS);
	$single->safe_psql('postgres', $LOC_ROWS);
	my $plan = plan_of($nodes[$k], $sql);
	like($plan, qr/Kingres Distributed Scan/, "$name: the distributed table is read by the distributed scan (node " . ($k + 1) . ")");
	like($plan, qr/Subquery Scan/, "$name: through a sub-select that the planner kept") unless $opt{no_subquery};
	if ($opt{returning})
	{
		my ($rs, $os, $es) = run_sql($single, $sql);
		my ($rd, $od, $ed) = run_sql($nodes[$k], $sql);
		is($rs, 0, "$name: the single node runs the statement") or diag($es);
		is($rd, 0, "$name: the cluster runs the statement") or diag($ed);
		is(KingresShardCompare::normalize($od, 0), KingresShardCompare::normalize($os, 0), "$name: the RETURNING rows are the single node's");
	}
	else
	{
		$c->compare_dml($name, $sql, node => $k);
	}
	$c->compare_tables('public.loc', order_by => 'id', nodes => [$k], name => "$name: public.loc");
}

# a reference target: the statement runs on the next node; every copy must be the single node's
sub merge_ref
{
	my ($name, $sql, %opt) = @_;
	my $k = $turn++ % 3;
	like(plan_of($nodes[$k], $sql), qr/Remote SQL: \/\* kingres:reference-write \*\/ MERGE/, "$name: planned as a write to every node (node " . ($k + 1) . ")");
	if ($opt{shipped})
	{
		like(plan_of($nodes[$k], $sql), qr/unnest/i, "$name: the source is shipped as a sub-select over unnest()");
	}
	if ($opt{returning})
	{
		my ($rs, $os, $es) = run_sql($single, $sql);
		my ($rd, $od, $ed) = run_sql($nodes[$k], $sql);
		is($rs, 0, "$name: the single node runs the statement") or diag($es);
		is($rd, 0, "$name: the cluster runs the statement") or diag($ed);
		is(KingresShardCompare::normalize($od, 0), KingresShardCompare::normalize($os, 0), "$name: the RETURNING rows are the single node's");
	}
	else
	{
		$c->compare_dml($name, $sql, node => $k);
	}
	$c->compare_tables('public.rt', order_by => 'k', name => "$name: public.rt");
}

sub merge_fails
{
	my ($name, $sql, $re, $table, $order) = @_;
	my $k = $turn++ % 3;
	my ($rd, $od, $ed) = run_sql($nodes[$k], $sql);
	isnt($rd, 0, "$name: the cluster refuses it");
	like($ed, $re, "$name: the error");
	$c->compare_tables($table, order_by => $order, name => "$name: $table unchanged");
}

table(q{CREATE TABLE public.d (id int PRIMARY KEY, v text, n int)},
	q{; SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.r (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.r')});
table(q{CREATE TABLE public.rt (k int PRIMARY KEY, label text, n int)},
	q{; SELECT kingres.create_reference_table('public.rt')});
table(q{CREATE TABLE public.rr (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.rr')});
local_table(q{CREATE TABLE public.loc (id int PRIMARY KEY, v text, n int)});
local_table(q{CREATE TABLE public.src (x int, y text, z int)});
$c->on_both(q{INSERT INTO public.d SELECT g, 'd' || g, g % 7 FROM generate_series(1, 200) g});
$c->on_both(q{INSERT INTO public.r SELECT g, repeat('R', 1 + g % 4) FROM generate_series(0, 8) g});
$c->on_both(q{INSERT INTO public.rr SELECT g, repeat('Q', 1 + g % 3) FROM generate_series(20, 90) g});
$c->on_both(q{INSERT INTO public.rt SELECT g, repeat('t', 1 + g % 5), g % 11 FROM generate_series(1, 120) g});
$c->on_every_node(q{INSERT INTO public.src SELECT g, 's' || g, g % 6 FROM generate_series(100, 180) g});
$c->on_single(q{INSERT INTO public.src SELECT g, 's' || g, g % 6 FROM generate_series(100, 180) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# ---------------------------------------------------------------------------
# A. M6, a local target
# ---------------------------------------------------------------------------
merge_local('update and insert from a distributed table',
	q{MERGE INTO public.loc USING public.d ON loc.id = d.id WHEN MATCHED THEN UPDATE SET v = d.v, n = d.n WHEN NOT MATCHED THEN INSERT VALUES (d.id, d.v, d.n)});
merge_local('delete and NOT MATCHED BY SOURCE with a restricted sub-select as the source',
	q{MERGE INTO public.loc USING (SELECT id, v, n FROM public.d WHERE n > 2 AND id > 60) q ON loc.id = q.id WHEN MATCHED THEN DELETE WHEN NOT MATCHED BY SOURCE THEN UPDATE SET n = -1});
merge_local('a distributed table joined to a reference table in USING',
	q{MERGE INTO public.loc USING (public.d JOIN public.r ON d.n = r.k) ON loc.id = d.id WHEN MATCHED THEN UPDATE SET v = r.label || d.v});
merge_local('an aggregate over a distributed table as the source',
	q{MERGE INTO public.loc USING (SELECT n AS k, count(*) AS c, max(id) AS m FROM public.d GROUP BY n) q ON loc.id = q.m WHEN MATCHED THEN UPDATE SET n = q.c});
merge_local('RETURNING with merge_action() and a column of the source',
	q{MERGE INTO public.loc USING public.d ON loc.id = d.id WHEN MATCHED AND d.n = 3 THEN DELETE WHEN MATCHED THEN UPDATE SET v = d.v WHEN NOT MATCHED AND d.id < 140 THEN INSERT VALUES (d.id, d.v, d.n) RETURNING merge_action(), loc.id, d.n}, returning => 1);
merge_local('a self join of the distributed source on its key',
	q{MERGE INTO public.loc USING (SELECT a.id, b.v FROM public.d a JOIN public.d b ON a.id = b.id WHERE a.id % 2 = 0) q ON loc.id = q.id WHEN MATCHED THEN UPDATE SET v = q.v || '!'});
{
	my $k = $turn++ % 3;
	$nodes[$k]->safe_psql('postgres', $LOC_ROWS);
	$single->safe_psql('postgres', $LOC_ROWS);
	my $sql = q{PREPARE ml(int) AS MERGE INTO public.loc USING public.d ON loc.id = d.id WHEN MATCHED AND d.n > $1 THEN UPDATE SET n = d.n + 1000; EXECUTE ml(2); EXECUTE ml(5); EXECUTE ml(2); EXECUTE ml(0); EXECUTE ml(1); EXECUTE ml(6)};
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[$k], $sql);
	is($rs, 0, 'a prepared MERGE into a local table: the single node runs it') or diag($es);
	is($rd, 0, 'a prepared MERGE into a local table: the cluster runs it (custom and generic plans)') or diag($ed);
	$c->compare_tables('public.loc', order_by => 'id', nodes => [$k], name => 'a prepared MERGE into a local table');
}
# a trigger of the local target runs once for each row, on this node
for my $n (@nodes)
{
	$n->safe_psql('postgres', q{CREATE TABLE public.trg_log (op text, id int);
		CREATE FUNCTION public.trg_fn() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN INSERT INTO public.trg_log VALUES (TG_OP, COALESCE(NEW.id, OLD.id)); RETURN COALESCE(NEW, OLD); END $f$;
		CREATE TABLE public.loct (id int PRIMARY KEY, v text);
		CREATE TRIGGER t_loct AFTER INSERT OR UPDATE OR DELETE ON public.loct FOR EACH ROW EXECUTE FUNCTION public.trg_fn();
		INSERT INTO public.loct SELECT g, 'x' FROM generate_series(1, 20) g;
		TRUNCATE public.trg_log});
}
{
	my ($rc, $out, $err) = run_sql($nodes[1], q{MERGE INTO public.loct USING (SELECT id FROM public.d WHERE id BETWEEN 10 AND 30) q ON loct.id = q.id WHEN MATCHED AND loct.id > 15 THEN DELETE WHEN MATCHED THEN UPDATE SET v = 'y' WHEN NOT MATCHED THEN INSERT VALUES (q.id, 'n')});
	is($rc, 0, 'a MERGE into a local table with a row trigger runs') or diag($err);
	is($nodes[1]->safe_psql('postgres', q{SELECT string_agg(op || ':' || n, ',' ORDER BY op) FROM (SELECT op, count(*) n FROM public.trg_log GROUP BY op) s}), 'DELETE:5,INSERT:10,UPDATE:6',
		'the trigger of the target fired once for each row (6 updated, 5 deleted, 10 inserted)');
	is($nodes[1]->safe_psql('postgres', q{SELECT count(*) FROM public.trg_log}), 21, 'and nothing else');
	is($nodes[0]->safe_psql('postgres', q{SELECT count(*) FROM public.trg_log}), 0, 'no other node ran the trigger');
}

# ---------------------------------------------------------------------------
# B. M4: the statement reads reference tables, VALUES and constants
# ---------------------------------------------------------------------------
merge_ref('VALUES: update and insert',
	q{MERGE INTO public.rt USING (VALUES (1, 'a', 1), (2, 'b', 2), (500, 'z', 3), (501, 'y', 4)) v(k, l, n) ON rt.k = v.k WHEN MATCHED THEN UPDATE SET label = v.l, n = v.n WHEN NOT MATCHED THEN INSERT VALUES (v.k, v.l, v.n)});
merge_ref('another reference table: delete, update and NOT MATCHED BY SOURCE',
	q{MERGE INTO public.rt USING public.rr ON rt.k = rr.k WHEN MATCHED AND rr.k > 80 THEN DELETE WHEN MATCHED THEN UPDATE SET label = rr.label WHEN NOT MATCHED BY SOURCE AND rt.k < 10 THEN DELETE});
merge_ref('a constant as the source row',
	q{MERGE INTO public.rt USING (SELECT 999 AS k, 'c' AS l) v ON rt.k = v.k WHEN NOT MATCHED THEN INSERT (k, label, n) VALUES (v.k, v.l, 0)});
merge_ref('RETURNING with merge_action()',
	q{MERGE INTO public.rt USING public.rr ON rt.k = rr.k WHEN MATCHED AND rr.k < 30 THEN UPDATE SET n = 77 WHEN NOT MATCHED THEN INSERT (k, label, n) VALUES (rr.k, rr.label, 1) RETURNING merge_action(), rt.k, rt.n}, returning => 1);

# ---------------------------------------------------------------------------
# C. M5: the source is read here and sent to every node
# ---------------------------------------------------------------------------
merge_ref('a distributed table as the source',
	q{MERGE INTO public.rt USING public.d ON rt.k = d.id WHEN MATCHED THEN UPDATE SET label = d.v, n = d.n WHEN NOT MATCHED THEN INSERT (k, label, n) VALUES (d.id, d.v, d.n)}, shipped => 1);
merge_ref('a local table as the source',
	q{MERGE INTO public.rt USING public.src ON rt.k = src.x WHEN MATCHED THEN UPDATE SET label = src.y WHEN NOT MATCHED THEN INSERT (k, label, n) VALUES (src.x, src.y, src.z)}, shipped => 1);
merge_ref('an aggregate over a distributed table, and NOT MATCHED BY SOURCE',
	q{MERGE INTO public.rt USING (SELECT n AS k, count(*) AS c FROM public.d GROUP BY n) q ON rt.k = q.k WHEN MATCHED THEN UPDATE SET n = q.c WHEN NOT MATCHED BY SOURCE AND rt.k > 400 THEN DELETE}, shipped => 1);
merge_ref('a restricted sub-select of a distributed table, delete',
	q{MERGE INTO public.rt USING (SELECT id FROM public.d WHERE id > 100 AND id < 130) q ON rt.k = q.id WHEN MATCHED THEN DELETE}, shipped => 1);
merge_ref('RETURNING of the local copy',
	q{MERGE INTO public.rt USING public.src ON rt.k = src.x WHEN MATCHED AND src.z = 1 THEN DELETE WHEN MATCHED THEN UPDATE SET n = src.z WHEN NOT MATCHED THEN INSERT (k, label, n) VALUES (src.x, src.y, src.z) RETURNING merge_action(), rt.k, rt.n}, returning => 1, shipped => 1);
merge_ref('an empty source and NOT MATCHED BY SOURCE reaches every row',
	q{MERGE INTO public.rt USING (SELECT id FROM public.d WHERE id < 0) q ON rt.k = q.id WHEN NOT MATCHED BY SOURCE AND rt.k % 3 = 0 THEN UPDATE SET label = 'orphan'}, shipped => 1);
merge_fails('a target row that matches two source rows (the nodes agree: nothing changed)',
	q{MERGE INTO public.rt USING (SELECT x % 50 AS k, y FROM public.src) q ON rt.k = q.k WHEN MATCHED THEN UPDATE SET label = q.y},
	qr/MERGE command cannot affect row a second time/, 'public.rt', 'k');
{
	my $sql = q{PREPARE mr(int) AS MERGE INTO public.rt USING public.d ON rt.k = d.id WHEN MATCHED AND d.n > $1 THEN UPDATE SET n = 5; EXECUTE mr(3); EXECUTE mr(1); EXECUTE mr(3); EXECUTE mr(0); EXECUTE mr(2); EXECUTE mr(4)};
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[2], $sql);
	is($rs, 0, 'a prepared MERGE into a reference table: the single node runs it') or diag($es);
	is($rd, 0, 'a prepared MERGE into a reference table: the cluster runs it (custom and generic plans)') or diag($ed);
	$c->compare_tables('public.rt', order_by => 'k', name => 'a prepared MERGE into a reference table');
}
{
	my $sql = q{BEGIN; MERGE INTO public.rt USING public.d ON rt.k = d.id WHEN MATCHED THEN DELETE; SELECT count(*) FROM public.rt; ROLLBACK};
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[0], $sql);
	is($rd, 0, 'a MERGE into a reference table in a transaction that rolls back') or diag($ed);
	is($od, $os, 'the count inside the transaction is the single node\'s');
	$c->compare_tables('public.rt', order_by => 'k', name => 'a rolled back MERGE changes no copy');
}

# ---------------------------------------------------------------------------
# D. a ledger table as the target of M6
# ---------------------------------------------------------------------------
for my $n (@nodes)
{
	$n->safe_psql('postgres', q{
		CREATE EXTENSION kingres_ledger;
		CREATE TABLE public.led_u (kind int, id int, v text);
		CREATE TABLE public.led_a (kind int, id int, v text);
		SELECT kingres.ledger_enable('public.led_u', 'updatable');
		SELECT kingres.ledger_enable('public.led_a', 'append_only');
	});
}
{
	my $entries = sub { my ($n, $t) = @_; return $n->safe_psql('postgres', "SELECT count(*) || '/' || count(DISTINCT (e.ledger_txid, e.seq)) FROM kingres.ledger_entry e JOIN kingres.ledger_table lt USING (table_id) WHERE lt.relid = 'public.$t'::regclass"); };
	for my $i (0 .. $#nodes)
	{
		my $n = $nodes[$i];
		my $nm = $n->name;
		my ($b0) = split /\//, $entries->($n, 'led_u');
		my $sql = q{MERGE INTO public.led_u USING (SELECT id, v FROM public.d WHERE id <= 40) q ON led_u.id = q.id WHEN MATCHED THEN UPDATE SET v = 'again' WHEN NOT MATCHED THEN INSERT VALUES (1, q.id, q.v)};
		like(plan_of($n, $sql), qr/Kingres Distributed Scan/, "$nm: the source of the MERGE into a ledger table is read by the distributed scan");
		my ($rc, $out, $err) = run_sql($n, qq{DO \$\$ DECLARE r bigint; BEGIN $sql; GET DIAGNOSTICS r = ROW_COUNT; RAISE NOTICE 'ROWS=%', r; END \$\$});
		is($rc, 0, "$nm: the MERGE into a ledger table runs") or diag($err);
		my ($rows) = $err =~ /ROWS=(\d+)/;
		my ($e1, $d1) = split /\//, $entries->($n, 'led_u');
		is($e1 - $b0, $rows, "$nm: the capture trigger fired once for each row of the MERGE ($rows rows)");
		is($d1, $e1, "$nm: no entry twice");
		# what a plain UPDATE of the same rows puts in the ledger is what the UPDATE action of a MERGE must put there
		my ($u0) = split /\//, $entries->($n, 'led_u');
		($rc, $out, $err) = run_sql($n, q{UPDATE public.led_u SET v = 'plain' WHERE id <= 40});
		is($rc, 0, "$nm: a plain UPDATE of the ledger table runs") or diag($err);
		my ($u1) = split /\//, $entries->($n, 'led_u');
		($rc, $out, $err) = run_sql($n, $sql);
		is($rc, 0, "$nm: the MERGE that updates every row runs") or diag($err);
		my ($e2, $d2) = split /\//, $entries->($n, 'led_u');
		is($e2 - $u1, $u1 - $u0, "$nm: the UPDATE action of the MERGE is captured as a plain UPDATE is (" . ($u1 - $u0) . " entries for 40 rows)");
		is($d2, $e2, "$nm: no entry twice");
		is($n->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'), 0, "$nm: ledger_verify() reports no problem");
	}
	# an append-only ledger refuses an UPDATE and a DELETE, also by MERGE
	my ($rc, $out, $err) = run_sql($nodes[0], q{MERGE INTO public.led_a USING (SELECT id FROM public.d WHERE id <= 5) q ON led_a.id = q.id WHEN NOT MATCHED THEN INSERT VALUES (1, q.id, 'x')});
	is($rc, 0, 'an append-only ledger table accepts the INSERT action') or diag($err);
	($rc, $out, $err) = run_sql($nodes[0], q{MERGE INTO public.led_a USING (SELECT id FROM public.d WHERE id <= 5) q ON led_a.id = q.id WHEN MATCHED THEN UPDATE SET v = 'y'});
	isnt($rc, 0, 'an append-only ledger table still refuses an UPDATE action');
	is($nodes[0]->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'), 0, 'ledger_verify() reports no problem');
}

# ---------------------------------------------------------------------------
# E. forged fragments of a reference-table write, and the refusals
# ---------------------------------------------------------------------------
{
	my $recv = $nodes[1];
	my $rows = sub { return $_[0]->safe_psql('postgres', q{SELECT count(*) || '/' || sum(n) || '/' || md5(string_agg(label, ',' ORDER BY k)) FROM public.rt}); };
	my $before = $rows->($recv);
	my $frag = sub {
		my ($sql, @bind) = @_;
		my $tok = $recv->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, 2)");
		local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
		my $stmt = "/* kingres:reference-write */ $sql" . (@bind ? " \\bind " . join(' ', map { "'$_'" } @bind) . " \\g" : ';');
		return run_sql($recv, "\\set VERBOSITY verbose\nBEGIN;\n$stmt\nROLLBACK;");
	};
	my ($rc, $out, $err) = $frag->(q{MERGE INTO public.rt USING public.rr ON (rt.k = rr.k) WHEN MATCHED THEN UPDATE SET n = 0});
	is($rc, 0, 'a MERGE into a reference table of a shape the node accepts runs (the control)') or diag($err);
	($rc, $out, $err) = $frag->(q{MERGE INTO public.rt USING (SELECT u.c1::pg_catalog.int4 AS x FROM ROWS FROM (pg_catalog.unnest($1::pg_catalog.text[])) AS u(c1)) q ON (rt.k = q.x) WHEN MATCHED THEN UPDATE SET n = 0}, '{1,2}');
	is($rc, 0, 'with a shipped source (the control)') or diag($err);
	for my $case (
		[ 'a local table as the source: this node has a copy of its own', q{MERGE INTO public.rt USING public.src ON (rt.k = src.x) WHEN MATCHED THEN DELETE} ],
		[ 'a distributed table as the source', q{MERGE INTO public.rt USING public.d ON (rt.k = d.id) WHEN MATCHED THEN DELETE} ],
		[ 'a volatile function', q{MERGE INTO public.rt USING public.rr ON (rt.k = rr.k) WHEN MATCHED THEN UPDATE SET n = (random() * 5)::int} ],
		[ 'a sub-select that reads a distributed table', q{MERGE INTO public.rt USING public.rr ON (rt.k = rr.k) WHEN MATCHED AND rt.n > (SELECT max(n) FROM public.d) THEN DELETE} ],
		[ 'a target that is not a reference table', q{MERGE INTO public.loc USING public.rr ON (loc.id = rr.k) WHEN MATCHED THEN DELETE} ],
	)
	{
		my ($name, $sql) = @$case;
		($rc, $out, $err) = $frag->($sql);
		isnt($rc, 0, "a forged reference-write MERGE is refused: $name");
	}
	is($rows->($recv), $before, 'no forged fragment changed a row');
}
for my $case (
	[ 'a volatile function', q{MERGE INTO public.rt USING public.src ON rt.k = src.x WHEN MATCHED THEN UPDATE SET n = (random() * 5)::int}, qr/not built in and immutable/ ],
	[ 'a sub-select in an expression', q{MERGE INTO public.rt USING public.d ON rt.k = d.id WHEN MATCHED AND rt.n > (SELECT 1) THEN DELETE}, qr/sub-select in an expression/ ],
	[ 'a source that is a join', q{MERGE INTO public.rt USING public.src a JOIN public.src b ON a.x = b.x ON rt.k = a.x WHEN MATCHED THEN DELETE}, qr/put it in a sub-select/ ],
	[ 'a WITH clause', q{WITH w AS (SELECT 1) MERGE INTO public.rt USING public.d ON rt.k = d.id WHEN MATCHED THEN DELETE}, qr/WITH clause/ ],
)
{
	my ($name, $sql, $re) = @$case;
	my ($rc, $out, $err) = run_sql($nodes[2], $sql);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "refused: $name: the message names the condition");
}
{
	my ($rc, $out, $err) = run_sql($nodes[0], q{SET kingres_shard.enable_distributed_merge = off; MERGE INTO public.rt USING public.d ON rt.k = d.id WHEN MATCHED THEN DELETE});
	isnt($rc, 0, 'with the switch off a MERGE into a reference table that reads a distributed table is refused');
	like($err, qr/a statement that writes reference table "rt" and reads a distributed table is not supported yet/, 'with the message of phase 2a');
	($rc, $out, $err) = run_sql($nodes[0], q{SET kingres_shard.enable_distributed_merge = off; MERGE INTO public.rt USING (VALUES (1, 'a')) v(k, l) ON rt.k = v.k WHEN MATCHED THEN DELETE});
	isnt($rc, 0, 'with the switch off a MERGE into a reference table is refused');
	like($err, qr/MERGE into a reference table is not supported yet/, 'with the message of phase 2a');
	($rc, $out, $err) = run_sql($nodes[0], q{SET kingres_shard.enable_distributed_merge = off; MERGE INTO public.loc USING public.d ON loc.id = d.id WHEN MATCHED THEN DELETE});
	isnt($rc, 0, 'with the switch off a MERGE into a local table that reads a distributed table is refused');
	like($err, qr/UPDATE, DELETE and MERGE that read distributed table/, 'with the message of phase 2a');
}
$c->compare_tables('public.rt', order_by => 'k', name => 'the refusals changed no copy');

done_testing();
