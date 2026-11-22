# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/264_merge_routed.pl
#
# Phase 12, MERGE on distributed tables, shape M3 (design/analytics-phase12-queries.md, 7.4, 14.2): the source of the MERGE is anything
# a SELECT reads (a local table, VALUES, a sub-select with an aggregate, a distributed table of another colocation group or joined on a
# column other than its key, a reference table).  The origin runs the source as a SELECT of its own and sends each row to the group
# that owns the bucket of the key expression of the ON clause; each group runs the MERGE over the rows it got, through a sub-select over
# unnest() of array parameters.  Every statement is run on the three nodes (the node rotates) and on a single ordinary node
# holding the same rows; the count, the RETURNING rows and every table afterwards must be the single node's.
#
#   A. the sources: a local table, VALUES, an aggregate, another colocation group, a join on a non-key column, a reference table
#   B. the data: every kind of value travels as text, NULL keys, the key expression, duplicates in the source, an empty source
#   C. a group with no source row is not skipped (WHEN NOT MATCHED BY SOURCE), a group with no row is skipped otherwise
#   D. the bound merge_max_source_bytes (54000, nothing changed), errors that leave nothing changed, a prepared statement
#   E. a forged fragment is refused by a group and never run

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('mr');
my @nodes = $c->nodes;
my $single = $c->single;
my $PUSHED = 'Pushed: MERGE with a routed source';
my $turn = 0;

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }
sub local_table { my ($sql) = @_; $c->on_every_node($sql); $c->on_single($sql); }

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

sub merge_dml
{
	my ($name, $sql, @tables) = @_;
	my $k = $turn++ % 3;
	like(plan_of($nodes[$k], $sql), qr/\Q$PUSHED\E/, "$name: planned as a MERGE with a routed source (node " . ($k + 1) . ")");
	my $rows = $c->compare_dml($name, $sql, node => $k);
	$c->compare_tables($_, order_by => ($_ eq 'public.nk' ? 'id, v' : 'id'), name => "$name: $_") for @tables;
	return $rows;
}

sub merge_returning
{
	my ($name, $sql, @tables) = @_;
	my $k = $turn++ % 3;
	like(plan_of($nodes[$k], $sql), qr/\Q$PUSHED\E/, "$name: planned as a MERGE with a routed source (node " . ($k + 1) . ")");
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[$k], $sql);
	is($rs, 0, "$name: the single node runs the statement") or diag($es);
	is($rd, 0, "$name: the cluster runs the statement") or diag($ed);
	is(KingresShardCompare::normalize($od, 0), KingresShardCompare::normalize($os, 0), "$name: the RETURNING rows are the single node's");
	$c->compare_tables($_, order_by => ($_ eq 'public.nk' ? 'id, v' : 'id'), name => "$name: $_") for @tables;
}

sub merge_fails
{
	my ($name, $sql, $re, @tables) = @_;
	my $k = $turn++ % 3;
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[$k], $sql);
	isnt($rd, 0, "$name: the cluster refuses it");
	like($ed, $re, "$name: the error");
	isnt($rs, 0, "$name: the single node refuses it") if $es =~ /\S/ || $rs != 0;
	$c->compare_tables($_, order_by => 'id', name => "$name: $_ unchanged") for @tables;
}

table(q{CREATE TABLE public.t (id int PRIMARY KEY, v text, n int)},
	q{; SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.s (id int PRIMARY KEY, g int, w text, h int)},
	q{; SELECT kingres.create_distributed_table('public.s', 'id', colocate_with => 'public.t')});
table(q{CREATE TABLE public.a (id int PRIMARY KEY, x text, m int)},
	q{; SELECT kingres.create_distributed_table('public.a', 'id', bucket_count => 7)});
table(q{CREATE TABLE public.r (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.r')});
local_table(q{CREATE TABLE public.loc (x int, y text, z int)});
$c->on_both(q{INSERT INTO public.r SELECT g, repeat('R', 1 + g % 4) FROM generate_series(0, 40) g});
$c->on_both(q{INSERT INTO public.t SELECT g, 't' || g, g % 7 FROM generate_series(1, 120) g});
$c->on_both(q{INSERT INTO public.s SELECT g, g - 40, 'w' || g, g % 50 FROM generate_series(60, 200) g});
$c->on_both(q{INSERT INTO public.a SELECT g, 'a' || g, g % 9 FROM generate_series(100, 160) g});
$c->on_every_node(q{INSERT INTO public.loc SELECT g, 'l' || g, g % 5 FROM generate_series(90, 170) g});
$c->on_single(q{INSERT INTO public.loc SELECT g, 'l' || g, g % 5 FROM generate_series(90, 170) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});

# ---------------------------------------------------------------------------
# A. the sources
# ---------------------------------------------------------------------------
merge_dml('a local table: update and insert',
	q{MERGE INTO public.t USING public.loc ON t.id = loc.x WHEN MATCHED THEN UPDATE SET v = loc.y, n = loc.z WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (loc.x, loc.y, loc.z)}, 'public.t');
merge_dml('a VALUES list',
	q{MERGE INTO public.t USING (VALUES (1, 'one'), (2, 'two'), (500, 'five hundred'), (501, 'x')) AS v(k, name) ON t.id = v.k WHEN MATCHED THEN UPDATE SET v = v.name WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (v.k, v.name, 0)}, 'public.t');
merge_dml('an aggregate over a distributed table as the source',
	q{MERGE INTO public.t USING (SELECT h AS k, count(*) AS c, max(w) AS mw FROM public.s GROUP BY h) q ON t.id = q.k WHEN MATCHED THEN UPDATE SET n = q.c, v = q.mw WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (q.k, q.mw, q.c)}, 'public.t');
merge_dml('a distributed table of another colocation group',
	q{MERGE INTO public.t USING public.a ON t.id = a.id WHEN MATCHED THEN UPDATE SET v = a.x || '!' , n = a.m WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (a.id, a.x, a.m)}, 'public.t');
merge_dml('a distributed table of the same group, joined on a column other than its key',
	q{MERGE INTO public.t USING public.s ON t.id = s.g WHEN MATCHED THEN UPDATE SET v = s.w}, 'public.t');
merge_dml('a reference table as the source',
	q{MERGE INTO public.t USING public.r ON t.id = r.k WHEN MATCHED THEN UPDATE SET v = r.label WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (r.k, r.label, -1)}, 'public.t');
merge_dml('the key is an expression of the source',
	q{MERGE INTO public.t USING public.loc ON t.id = loc.x + 1000 WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (loc.x + 1000, loc.y, 1)}, 'public.t');
merge_dml('a condition on the source in the WHEN clause, a delete',
	q{MERGE INTO public.t USING public.loc ON t.id = loc.x WHEN MATCHED AND loc.z = 2 THEN DELETE WHEN MATCHED AND loc.z = 3 THEN DO NOTHING WHEN MATCHED THEN UPDATE SET n = loc.z * 100}, 'public.t');
merge_returning('RETURNING with merge_action() and a source column',
	q{MERGE INTO public.t USING public.loc ON t.id = loc.x WHEN MATCHED AND loc.z < 2 THEN UPDATE SET n = loc.z + 5000 WHEN MATCHED THEN DELETE WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (loc.x, loc.y, 9) RETURNING merge_action(), t.id, t.n, loc.y}, 'public.t');

# ---------------------------------------------------------------------------
# B. the data
# ---------------------------------------------------------------------------
table(q{CREATE TABLE public.ty (id int PRIMARY KEY, t text, nm numeric(12,3), dt date, tz timestamptz, b boolean, j jsonb, ta text[], by bytea, f8 float8, i8 bigint)},
	q{; SELECT kingres.create_distributed_table('public.ty', 'id', colocate_with => 'public.t')});
local_table(q{CREATE TABLE public.lty (id int, t text, nm numeric(12,3), dt date, tz timestamptz, b boolean, j jsonb, ta text[], by bytea, f8 float8, i8 bigint)});
{
	my $vals = q{INSERT INTO public.lty VALUES
	(1, 'plain', 1.500, '2026-01-31', '2026-01-31 23:59:59.123456+03:30', true, '{"a": [1, 2, {"b": null}]}', ARRAY['x,y', 'z"q', '{w}', NULL], '\xdeadbeef', 0.1, 9223372036854775807),
	(2, E'quote " back \\\\ comma , brace {} nl \n tab \t end', -0.001, '0001-01-01', 'infinity', false, '[]', ARRAY[]::text[], '\x', 1e308, -9223372036854775808),
	(3, '', NULL, NULL, NULL, NULL, 'null', NULL, NULL, 'NaN', NULL),
	(4, 'NULL', 99999999.999, '9999-12-31', '2026-06-01 12:00:00-11', true, '{"k": "é中"}', ARRAY['', 'NULL', E'é中'], '\x00ff', 3.141592653589793, 0),
	(5, E'é中\U0001F600', 0, '2000-02-29', '2000-02-29 00:00:00+00', false, '"s"', ARRAY['a'], '\x5c', -0.0, 1),
	(6, 'six', 6, '2006-06-06', '2006-06-06 06:06:06+00', true, '6', ARRAY['6'], '\x06', 6.5, 6)};
	$c->on_every_node($vals);
	$c->on_single($vals);
}
$c->on_both(q{INSERT INTO public.ty (id, t, nm) VALUES (2, 'old', 2), (4, 'old', 4), (7, 'unrelated', 7)});
merge_dml('every kind of value travels as text and comes back by its type',
	q{MERGE INTO public.ty USING public.lty ON ty.id = lty.id WHEN MATCHED THEN UPDATE SET t = lty.t, nm = lty.nm, dt = lty.dt, tz = lty.tz, b = lty.b, j = lty.j, ta = lty.ta, by = lty.by, f8 = lty.f8, i8 = lty.i8 WHEN NOT MATCHED THEN INSERT (id, t, nm, dt, tz, b, j, ta, by, f8, i8) VALUES (lty.id, lty.t, lty.nm, lty.dt, lty.tz, lty.b, lty.j, lty.ta, lty.by, lty.f8, lty.i8)}, 'public.ty');

# NULL keys: a source row whose key is NULL matches nothing and is inserted with a NULL key where the single node puts it
table(q{CREATE TABLE public.nk (id int, v text)},
	q{; SELECT kingres.create_distributed_table('public.nk', 'id', colocate_with => 'public.t')});
local_table(q{CREATE TABLE public.lnk (x int, y text)});
$c->on_every_node(q{INSERT INTO public.lnk VALUES (1, 'a'), (NULL, 'n1'), (NULL, 'n2'), (3, 'c')});
$c->on_single(q{INSERT INTO public.lnk VALUES (1, 'a'), (NULL, 'n1'), (NULL, 'n2'), (3, 'c')});
merge_dml('NULL keys with an INSERT action',
	q{MERGE INTO public.nk USING public.lnk ON nk.id = lnk.x WHEN NOT MATCHED THEN INSERT (id, v) VALUES (lnk.x, lnk.y)}, 'public.nk');
merge_dml('NULL keys with no INSERT action',
	q{MERGE INTO public.nk USING public.lnk ON nk.id = lnk.x WHEN MATCHED THEN UPDATE SET v = lnk.y}, 'public.nk');

# an empty source, with and without an action that reaches the target rows
local_table(q{CREATE TABLE public.lempty (x int, y text)});
merge_dml('an empty source and a matched action',
	q{MERGE INTO public.t USING public.lempty ON t.id = lempty.x WHEN MATCHED THEN DELETE}, 'public.t');
merge_dml('an empty source and NOT MATCHED BY SOURCE reaches every target row',
	q{MERGE INTO public.t USING public.lempty ON t.id = lempty.x WHEN NOT MATCHED BY SOURCE AND t.id > 600 THEN UPDATE SET v = 'orphan'}, 'public.t');

# ---------------------------------------------------------------------------
# C. a group with no source row
# ---------------------------------------------------------------------------
{
	# a source of the keys of one group only: the other groups get no row; NOT MATCHED BY SOURCE still acts on their target rows
	my $coloc = $nodes[0]->safe_psql('postgres', q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.t'::regclass});
	my @keys = KingresShardTest::keys_of_group($nodes[0], $coloc, 1, 4, 1);
	my $list = join(', ', map { "($_, 'k$_')" } @keys);
	merge_dml('a source with the keys of one group: the other groups are not asked to do anything without an action that reaches target rows',
		qq{MERGE INTO public.t USING (VALUES $list) AS v(k, name) ON t.id = v.k WHEN MATCHED THEN UPDATE SET v = v.name}, 'public.t');
	merge_dml('the same source with NOT MATCHED BY SOURCE: every group acts on its own target rows',
		qq{MERGE INTO public.t USING (VALUES $list) AS v(k, name) ON t.id = v.k WHEN MATCHED THEN UPDATE SET v = v.name WHEN NOT MATCHED BY SOURCE AND t.id < 30 THEN DELETE}, 'public.t');
}

# ---------------------------------------------------------------------------
# D. the bound, errors, a prepared statement
# ---------------------------------------------------------------------------
{
	my $sql = q{SET kingres_shard.merge_max_source_bytes = 1024; MERGE INTO public.t USING (SELECT g, repeat('x', 1000) AS pad FROM generate_series(2000, 5000) g) q ON t.id = q.g WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (q.g, q.pad, 1)};
	my ($rc, $out, $err) = run_sql($nodes[1], $sql);
	isnt($rc, 0, 'a source above merge_max_source_bytes is refused');
	like($err, qr/more than kingres_shard\.merge_max_source_bytes \(1024 kB\)/, 'the message names the setting');
	like($err, qr/Split the source by key range/, 'the hint says what to do');
	my ($rc2, $o2, $e2) = run_sql($nodes[1], q{SELECT count(*) FROM public.t WHERE id >= 2000});
	is($o2, 0, 'nothing was inserted');
	$c->compare_tables('public.t', order_by => 'id', name => 'the bound: public.t unchanged');
}
{
	my ($rc, $out, $err) = run_sql($nodes[2], q{\set VERBOSITY verbose
SET kingres_shard.merge_max_source_bytes = 1024; MERGE INTO public.t USING (SELECT g, repeat('x', 1000) AS pad FROM generate_series(2000, 5000) g) q ON t.id = q.g WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (q.g, q.pad, 1)});
	like($err, qr/54000/, 'the error code is 54000');
}
merge_fails('a target row that matches two source rows',
	q{MERGE INTO public.t USING (VALUES (10, 'a'), (10, 'b')) AS v(k, name) ON t.id = v.k WHEN MATCHED THEN UPDATE SET v = v.name},
	qr/MERGE command cannot affect row a second time/, 'public.t');
merge_fails('an error on a group leaves nothing changed on the others',
	q{MERGE INTO public.t USING public.loc ON t.id = loc.x WHEN MATCHED THEN UPDATE SET n = 1000 / (loc.x - 130), v = 'changed'},
	qr/division by zero/, 'public.t');
{
	my $sql = q{PREPARE mq(int) AS MERGE INTO public.t USING public.loc ON t.id = loc.x WHEN MATCHED AND loc.z > $1 THEN UPDATE SET n = 8; EXECUTE mq(1); EXECUTE mq(3); EXECUTE mq(1); EXECUTE mq(0); EXECUTE mq(2); EXECUTE mq(4)};
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[0], $sql);
	is($rs, 0, 'a prepared MERGE: the single node runs it') or diag($es);
	is($rd, 0, 'a prepared MERGE: the cluster runs it (custom, then generic plans)') or diag($ed);
	$c->compare_tables('public.t', order_by => 'id', name => 'a prepared MERGE');
}
{
	my $sql = q{BEGIN; MERGE INTO public.t USING public.loc ON t.id = loc.x WHEN MATCHED THEN DELETE; SELECT count(*) FROM public.t; ROLLBACK};
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($nodes[1], $sql);
	is($rd, 0, 'a MERGE in a transaction that rolls back') or diag($ed);
	is($od, $os, 'the count inside the transaction is the single node\'s');
	$c->compare_tables('public.t', order_by => 'id', name => 'a rolled back MERGE changes nothing');
}

# ---------------------------------------------------------------------------
# E. a forged fragment
# ---------------------------------------------------------------------------
# A session that claims to be forwarded must prove it, so a client cannot send a fragment at all; the check the group makes of a text
# it received is shown here on the function that every group runs (the same analysis refuses the text on the origin before it is sent).
for my $case (
	[ 'the ON clause does not equate the key', q{MERGE INTO public.t USING public.loc ON t.n = loc.z WHEN MATCHED THEN DELETE}, qr/must equate its distribution column/ ],
	[ 'a join in USING', q{MERGE INTO public.t USING public.loc l1 JOIN public.loc l2 ON l1.x = l2.x ON t.id = l1.x WHEN MATCHED THEN DELETE}, qr/put it in a sub-select/ ],
	[ 'an INSERT whose key is not the routing key', q{MERGE INTO public.t USING public.loc ON t.id = loc.x WHEN NOT MATCHED THEN INSERT (id, v) VALUES (loc.x + 1, 'z')}, qr/not the key that the statement is colocated or routed by/ ],
	[ 'a volatile value', q{MERGE INTO public.t USING public.loc ON t.id = loc.x WHEN MATCHED THEN UPDATE SET n = (random() * 3)::int}, qr/volatile value/ ],
	[ 'the key of the target is updated', q{MERGE INTO public.t USING public.loc ON t.id = loc.x WHEN MATCHED THEN UPDATE SET id = loc.x + 5000}, qr/assigns the distribution column/ ],
)
{
	my ($name, $sql, $re) = @$case;
	my ($rc, $out, $err) = run_sql($nodes[0], $sql);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "refused: $name: the message names the condition");
}
$c->compare_tables('public.t', order_by => 'id', name => 'the refusals changed nothing');

# What a group does with a MERGE fragment that it was sent: a session that proves it comes from a node of the cluster (the token) is
# still refused when the text is not of a shape the group's own analysis accepts, and a placement that is not the group's is KS001.
{
	my $coloc = $nodes[0]->safe_psql('postgres', q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.t'::regclass});
	my $PLACE = $nodes[0]->safe_psql('postgres', "SELECT kingres.internal_placement_hash($coloc)");
	my $recv = $nodes[1];
	my $rows = sub { return $_[0]->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) || '/' || sum(n) || '/' || md5(string_agg(v, ',' ORDER BY id)) FROM public.t}); };
	my $before = $rows->($recv);

	# the fragment in a transaction that rolls back; $bind: the values of the array parameters, sent with \bind
	my $frag = sub {
		my ($marker, $sql, @bind) = @_;
		my $tok = $recv->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, 2)");
		local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
		my $stmt = "$marker $sql" . (@bind ? " \\bind " . join(' ', map { "'$_'" } @bind) . " \\g" : ';');
		return run_sql($recv, "\\set VERBOSITY verbose\nBEGIN;\n$stmt\nROLLBACK;");
	};
	my $m0 = "/* kingres:merge-fragment place=$PLACE rp=0 */";
	my $m1 = "/* kingres:merge-fragment place=$PLACE rp=1 */";
	my $routed = q{MERGE INTO public.t USING (SELECT u.c1::pg_catalog.int4 AS x, u.c2 AS y FROM ROWS FROM (pg_catalog.unnest($1::pg_catalog.text[]), pg_catalog.unnest($2::pg_catalog.text[])) AS u(c1, c2)) q ON (t.id = q.x) WHEN MATCHED THEN UPDATE SET v = q.y};

	my ($rc, $out, $err) = $frag->($m0, q{MERGE INTO public.t USING public.s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET n = 0});
	is($rc, 0, 'a colocated MERGE fragment of the shape the group accepts runs (the control)') or diag($err);
	($rc, $out, $err) = $frag->($m1, $routed, '{1,2}', '{a,b}');
	is($rc, 0, 'a routed MERGE fragment of the shape the group accepts runs (the control)') or diag($err);

	for my $case (
		[ 'no key equality in ON', $m0, q{MERGE INTO public.t USING public.s ON (t.n = s.h) WHEN MATCHED THEN DELETE}, qr/does not accept the fragment/ ],
		[ 'a source that is not tables, claimed as colocated', $m0, q{MERGE INTO public.t USING (SELECT 1 AS id) s ON (t.id = s.id) WHEN MATCHED THEN DELETE}, qr/does not accept the fragment/ ],
		[ 'a shipped source claimed where the text has none', $m1, q{MERGE INTO public.t USING public.s ON (t.id = s.id) WHEN MATCHED THEN DELETE}, qr/does not accept the fragment/ ],
		[ 'a colocated claim for a text that has a shipped source', $m0, $routed, qr/does not accept the fragment/, '{1}', '{a}' ],
		[ 'an INSERT with another key', $m0, q{MERGE INTO public.t USING public.s ON (t.id = s.id) WHEN NOT MATCHED THEN INSERT (id, v) VALUES ((s.id + 1), 'z')}, qr/does not accept the fragment/ ],
		[ 'a volatile value', $m0, q{MERGE INTO public.t USING public.s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET n = (random() * 5)::int}, qr/does not accept the fragment/ ],
		[ 'the key is updated', $m0, q{MERGE INTO public.t USING public.s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET id = (s.id + 100000)}, qr/does not accept the fragment/ ],
		[ 'a function that is not built in', $m0, q{MERGE INTO public.t USING public.s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET n = pg_backend_pid()}, qr/does not accept the fragment/ ],
		[ 'a marker with a number of shipped sources that does not exist', "/* kingres:merge-fragment place=$PLACE rp=2 */", q{MERGE INTO public.t USING public.s ON (t.id = s.id) WHEN MATCHED THEN DELETE}, qr/does not accept the fragment/ ],
		[ 'another placement than the group has', "/* kingres:merge-fragment place=12345 rp=0 */", q{MERGE INTO public.t USING public.s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET n = 0}, qr/KS001|bucket map of this node is not the one/ ],
	)
	{
		my ($name, $marker, $sql, $re, @bind) = @$case;
		($rc, $out, $err) = $frag->($marker, $sql, @bind);
		isnt($rc, 0, "a forged MERGE fragment is refused: $name");
		like($err, $re, "... with the message of the refusal ($name)") or diag($err);
	}
	# a forged routed fragment with an INSERT key that is not the routing key (the text takes the array parameters of \bind)
	($rc, $out, $err) = $frag->($m1, q{MERGE INTO public.t USING (SELECT u.c1::pg_catalog.int4 AS x FROM ROWS FROM (pg_catalog.unnest($1::pg_catalog.text[])) AS u(c1)) q ON (t.id = q.x) WHEN NOT MATCHED THEN INSERT (id, v) VALUES ((q.x + 1), 'z')}, '{1,2}');
	isnt($rc, 0, 'a forged routed fragment whose INSERT key is not the routing key is refused');
	like($err, qr/does not accept the fragment/, '... with the message of the refusal') or diag($err);
	is($rows->($recv), $before, 'no forged fragment changed a row');
}

# every group holds exactly the rows of its buckets
for my $i (0 .. $#nodes)
{
	for my $tbl ('public.t', 'public.ty', 'public.nk')
	{
		my $key = 'id';
		my $out = $nodes[$i]->safe_psql('postgres', qq{
			SET kingres_shard.enable_routing = off;
			SELECT count(*) || '|' || count(*) FILTER (WHERE m.group_id = $i)
			  FROM $tbl k JOIN kingres.bucket_map m
			    ON m.colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = '$tbl'::regclass)
			   AND m.bucket = kingres.bucket_for('$tbl'::regclass, k.$key)});
		my ($all, $mine) = split /\|/, $out;
		is($mine, $all, "node " . ($i + 1) . " holds only rows of its own buckets in $tbl ($all rows)");
	}
}

done_testing();
