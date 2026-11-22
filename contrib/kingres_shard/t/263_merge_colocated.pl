# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/263_merge_colocated.pl
#
# Phase 12, MERGE on distributed tables, shape M1 (design/analytics-phase12-queries.md, 7.3, 14.2): a MERGE whose target and whose
# USING tables are of one colocation group and joined on their distribution keys (or whose USING reads reference tables only) is shipped
# whole to every group that owns a bucket of the target.  Every statement is run on the three nodes of the cluster (the node
# rotates) and on a single ordinary node holding the same rows; the command tag count, the RETURNING rows (as a set) and the rows of
# every table afterwards must be the single node's.  A statement that passes because nothing was shipped fails on the path
# assertion, which names the custom scan and its "Pushed" line.
#
#   A. UPDATE, DELETE, INSERT, DO NOTHING, RETURNING with merge_action(), a join in USING, a reference table in USING
#   B. a group with no source row is not skipped: WHEN NOT MATCHED BY SOURCE acts on its target rows
#   C. errors: "cannot affect row a second time", an error on one group leaves nothing changed anywhere
#   D. a prepared statement, a transaction that rolls back, the switch, the refusals that name the missing condition
#   E. every group holds exactly the rows of its buckets afterwards

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('mc');
my @nodes = $c->nodes;
my $single = $c->single;
my $PUSHED = 'Pushed: colocated MERGE';
my $turn = 0;

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# the statement on the single node and on the next node of the cluster: the same count; the tables are compared by the caller
sub merge_dml
{
	my ($name, $sql, @tables) = @_;
	my $k = $turn++ % 3;
	like(plan_of($nodes[$k], $sql), qr/\Q$PUSHED\E/, "$name: planned as a colocated MERGE (node " . ($k + 1) . ")");
	my $rows = $c->compare_dml($name, $sql, node => $k);
	$c->compare_tables($_, order_by => 'id', name => "$name: $_") for @tables;
	return $rows;
}

# a MERGE with RETURNING: the rows as a set, and the tables afterwards
sub merge_returning
{
	my ($name, $sql, @tables) = @_;
	my $k = $turn++ % 3;
	like(plan_of($nodes[$k], $sql), qr/\Q$PUSHED\E/, "$name: planned as a colocated MERGE (node " . ($k + 1) . ")");
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[$k], $sql);
	is($rs, 0, "$name: the single node runs the statement") or diag($es);
	is($rd, 0, "$name: the cluster runs the statement") or diag($ed);
	is(KingresShardCompare::normalize($od, 0), KingresShardCompare::normalize($os, 0), "$name: the RETURNING rows are the single node's");
	$c->compare_tables($_, order_by => 'id', name => "$name: $_") for @tables;
}

# a statement that fails the same way on both, and changes nothing
sub merge_fails
{
	my ($name, $sql, $re, @tables) = @_;
	my $k = $turn++ % 3;
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[$k], $sql);
	isnt($rs, 0, "$name: the single node refuses it");
	isnt($rd, 0, "$name: the cluster refuses it");
	like($ed, $re, "$name: the error");
	like($es, $re, "$name: the error of the single node");
	$c->compare_tables($_, order_by => 'id', name => "$name: $_ unchanged") for @tables;
}

table(q{CREATE TABLE public.t (id int PRIMARY KEY, v text, n int)},
	q{; SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.s (id int PRIMARY KEY, v text, n int)},
	q{; SELECT kingres.create_distributed_table('public.s', 'id', colocate_with => 'public.t')});
table(q{CREATE TABLE public.o (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.o', 'id', colocate_with => 'public.t')});
table(q{CREATE TABLE public.dup (id int, x int)},
	q{; SELECT kingres.create_distributed_table('public.dup', 'id', colocate_with => 'public.t')});
table(q{CREATE TABLE public.r (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.r')});
table(q{CREATE TABLE public.re (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.re')});
$c->on_both(q{INSERT INTO public.r VALUES (0, 'R0'), (1, 'R1'), (2, 'R2'), (3, 'R3')});
$c->on_both(q{INSERT INTO public.t SELECT g, 't' || g, g % 7 FROM generate_series(1, 150) g});
$c->on_both(q{INSERT INTO public.s SELECT g, 's' || g, g % 100 FROM generate_series(100, 250) g});
$c->on_both(q{INSERT INTO public.o SELECT g, 'o' || g FROM generate_series(100, 220) g});
$c->on_both(q{INSERT INTO public.dup VALUES (110, 1), (110, 2), (120, 3)});
table(q{CREATE TABLE public.te (id int PRIMARY KEY, v text, n int)},
	q{; SELECT kingres.create_distributed_table('public.te', 'id', colocate_with => 'public.t')});
table(q{CREATE TABLE public.se (id int PRIMARY KEY, v text, n int)},
	q{; SELECT kingres.create_distributed_table('public.se', 'id', colocate_with => 'public.t')});
$c->on_both(q{INSERT INTO public.te SELECT g, 'e' || g, g FROM generate_series(1, 60) g});
$c->on_both(q{INSERT INTO public.se SELECT g, 'e' || g, g FROM generate_series(1, 60) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# ---------------------------------------------------------------------------
# A. the shapes
# ---------------------------------------------------------------------------
merge_dml('update the matched rows',
	q{MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v || 'u', n = t.n + s.n}, 'public.t');
merge_dml('delete by a condition, insert the rest',
	q{MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED AND s.n > 90 THEN DELETE WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (s.id, s.v, s.n)}, 'public.t');
merge_dml('an insert only',
	q{MERGE INTO public.t USING public.o ON t.id = o.id WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (o.id, o.w, 0)}, 'public.t');
merge_dml('DO NOTHING in both branches',
	q{MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED AND s.n = 1 THEN DO NOTHING WHEN MATCHED THEN UPDATE SET n = t.n + 1}, 'public.t');
merge_dml('a join of two tables in USING',
	q{MERGE INTO public.t USING (public.s JOIN public.o ON s.id = o.id) ON t.id = s.id WHEN MATCHED THEN UPDATE SET v = o.w || s.v}, 'public.t');
merge_dml('a reference table in USING joined to the target by a non-key column',
	q{MERGE INTO public.t USING public.r ON t.n = r.k WHEN MATCHED THEN UPDATE SET v = r.label}, 'public.t');
merge_dml('a source row that matches no target row, an INSERT of a bare key',
	q{MERGE INTO public.s USING public.o ON s.id = o.id WHEN NOT MATCHED THEN INSERT VALUES (o.id, o.w, 1)}, 'public.s');
merge_returning('RETURNING with merge_action()',
	q{MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED AND s.n < 20 THEN UPDATE SET n = s.n + 1000 WHEN MATCHED THEN DELETE WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (s.id, s.v, 5) RETURNING merge_action(), t.id, t.n}, 'public.t');

# ---------------------------------------------------------------------------
# B. a group with no source row is not skipped
# ---------------------------------------------------------------------------
merge_dml('NOT MATCHED BY SOURCE with an empty reference source: every group acts on its target rows',
	q{MERGE INTO public.t USING public.re ON t.n = re.k WHEN NOT MATCHED BY SOURCE AND t.id % 2 = 0 THEN UPDATE SET v = 'gone'}, 'public.t');
merge_dml('NOT MATCHED BY SOURCE with a source that covers some keys only',
	q{MERGE INTO public.t USING public.o ON t.id = o.id WHEN MATCHED THEN UPDATE SET n = -1 WHEN NOT MATCHED BY SOURCE AND t.id < 60 THEN DELETE}, 'public.t');
merge_returning('NOT MATCHED BY SOURCE with RETURNING',
	q{MERGE INTO public.t USING public.o ON t.id = o.id WHEN NOT MATCHED BY SOURCE AND t.id > 140 THEN UPDATE SET n = 99 RETURNING merge_action(), t.id, t.n}, 'public.t');

# ---------------------------------------------------------------------------
# C. errors
# ---------------------------------------------------------------------------
merge_fails('a target row that matches two source rows',
	q{MERGE INTO public.t USING public.dup ON t.id = dup.id WHEN MATCHED THEN UPDATE SET n = dup.x},
	qr/MERGE command cannot affect row a second time/, 'public.t');
# the error is raised by each group in turn: the groups that ran before it have their changes rolled back, and those after never run
{
	my $coloc = $nodes[0]->safe_psql('postgres', q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.te'::regclass});
	for my $g (0 .. 2)
	{
		my ($key) = KingresShardTest::keys_of_group($nodes[0], $coloc, $g, 1, 1);
		merge_fails("an error raised by group $g (key $key) leaves no change on the others",
			qq{MERGE INTO public.te USING public.se ON te.id = se.id WHEN MATCHED THEN UPDATE SET n = 1000 / (se.id - $key), v = 'changed'},
			qr/division by zero/, 'public.te');
	}
}

# ---------------------------------------------------------------------------
# D. prepared statements, a rolled back transaction, the switch, the refusals
# ---------------------------------------------------------------------------
{
	my $sql = q{PREPARE mp(int) AS MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED AND s.n > $1 THEN UPDATE SET n = 7; EXECUTE mp(10); EXECUTE mp(60); EXECUTE mp(10)};
	my $k = $turn++ % 3;
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[$k], $sql);
	is($rs, 0, 'a prepared MERGE: the single node runs it') or diag($es);
	is($rd, 0, 'a prepared MERGE: the cluster runs it (a custom and then a generic plan)') or diag($ed);
	$c->compare_tables('public.t', order_by => 'id', name => 'a prepared MERGE');
}
{
	my $sql = q{BEGIN; MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED THEN DELETE; SELECT count(*) FROM public.t; ROLLBACK};
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[1], $sql);
	is($rd, 0, 'a MERGE in a transaction that rolls back') or diag($ed);
	is($od, $os, 'the count inside the transaction is the single node\'s');
	$c->compare_tables('public.t', order_by => 'id', name => 'a rolled back MERGE changes nothing');
}
{
	my ($rc, $out, $err) = run_sql($nodes[0], q{SET kingres_shard.enable_distributed_merge = off; MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED THEN DELETE});
	isnt($rc, 0, 'with the switch off the MERGE is refused');
	like($err, qr/statements that span shard groups are not supported yet/, 'with the switch off the message is the one of phase 2a');
}
for my $case (
	[ 'a WITH clause', q{WITH c AS (SELECT 1) MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED THEN DELETE}, qr/it has a WITH clause/ ],
	[ 'an UPDATE of the key', q{MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED THEN UPDATE SET id = s.id + 1000}, qr/assigns the distribution column/ ],
	[ 'an INSERT with another key', q{MERGE INTO public.t USING public.s ON t.id = s.id WHEN NOT MATCHED THEN INSERT (id, v) VALUES (s.id + 1, 'x')}, qr/not the key that the statement is colocated or routed by/ ],
	[ 'a volatile value', q{MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED THEN UPDATE SET n = (random() * 10)::int}, qr/volatile value/ ],
	[ 'a sub-select in a condition', q{MERGE INTO public.t USING public.s ON t.id = s.id WHEN MATCHED AND t.n > (SELECT 1) THEN DELETE}, qr/sub-select in an expression/ ],
	[ 'no key equality', q{MERGE INTO public.t USING public.s ON t.n = s.n WHEN MATCHED THEN DELETE}, qr/the ON clause of a MERGE into a distributed table must equate its distribution column/ ],
)
{
	my ($name, $sql, $re) = @$case;
	my ($rc, $out, $err) = run_sql($nodes[2], $sql);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "refused: $name: the message names the condition");
}
$c->compare_tables('public.t', order_by => 'id', name => 'the refusals changed nothing');

# ---------------------------------------------------------------------------
# E. every group holds exactly the rows of its buckets
# ---------------------------------------------------------------------------
for my $i (0 .. $#nodes)
{
	my $n = $nodes[$i];
	my $out = $n->safe_psql('postgres', qq{
		SET kingres_shard.enable_routing = off;
		SELECT count(*) || '|' || count(*) FILTER (WHERE m.group_id = $i)
		  FROM public.t k JOIN kingres.bucket_map m
		    ON m.colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.t'::regclass)
		   AND m.bucket = kingres.bucket_for('public.t'::regclass, k.id)});
	my ($all, $mine) = split /\|/, $out;
	is($mine, $all, 'node ' . ($i + 1) . ' holds only rows of its own buckets in public.t (' . $all . ' rows)');
}

done_testing();
