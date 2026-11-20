# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/235_schema_route_cluster.pl
#
# Phase 11 (design/shard-phase11.md), increment 4: path S, the statement that is run whole by the owner of a distributed schema
# (6.2), on three nodes.  The schema ra is held by node 2, the schema rb by node 3; every statement below is entered on a node that
# does not hold the tables it names, unless it says otherwise.
#
#   A. writes of every shape from a node that does not hold the schema: they land on the owner
#   B. the same reads with kingres_shard.enable_schema_routing on and off give the same answer (ADR 0022)
#   C. transactions: a rollback leaves nothing, a commit that wrote two owners is two-phase
#   D. prepared statements and generic plans, PL/pgSQL
#   E. sequences of a schema are the owner's
#   F. functions: ship_functions, and what the scan does when they are not shipped
#   G. what path S does not take: other kinds of tables, two owners, MERGE, a scrollable cursor
#   H. privileges and the setting that switches the path off
#   I. a map that differs between nodes

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('rt', 3);
my ($n1, $n2, $n3) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

sub fails
{
	my ($n, $sql, $re, $name, %opt) = @_;
	my ($rc, $out, $err) = run_sql($n, $sql, %opt);
	isnt($rc, 0, "refused: $name");
	like($err, $re, "... message: $name") or diag($err);
}

for my $n (@nodes)
{
	q1($n, q{CREATE ROLE alice LOGIN});
	q1($n, q{CREATE TABLE public.loc (id int PRIMARY KEY, tag text)});
}
q1($n1, q{INSERT INTO public.loc VALUES (1, 'one'), (2, 'two'), (3, 'three')});
q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});
q1($n1, q{SET kingres_shard.schema_placement_group = 1; CREATE SCHEMA ra});
q1($n1, q{SET kingres_shard.schema_placement_group = 2; CREATE SCHEMA rb});
q1($n1, q{
	CREATE TABLE ra.t (id serial PRIMARY KEY, v text, n int);
	CREATE TABLE ra.u (id int PRIMARY KEY, t_id int);
	CREATE TABLE rb.w (id int PRIMARY KEY, x int);
	CREATE TABLE public.ref (id int PRIMARY KEY, name text);
	SELECT kingres.create_reference_table('public.ref');
	INSERT INTO public.ref VALUES (1, 'r1'), (2, 'r2');
	CREATE TABLE public.h (id int PRIMARY KEY, v int);
	SELECT kingres.create_distributed_table('public.h', 'id', bucket_count => 4);
	INSERT INTO public.h SELECT g, g FROM generate_series(1, 10) g;
});
is(q1($n1, q{SELECT string_agg(schema_name || ':' || group_id, ',' ORDER BY schema_name) FROM kingres.schemas}), 'ra:1,rb:2', 'ra is held by node 2, rb by node 3');

# ---------------------------------------------------------------------------
# A. writes from a node that does not hold the schema
# ---------------------------------------------------------------------------
for my $ent ([ $n1, 100 ], [ $n3, 200 ])
{
	my ($n, $b) = @$ent;
	my $nm = $n->name;
	q1($n, "INSERT INTO ra.t (id, v, n) VALUES ($b + 1, 'a', 1), ($b + 2, 'b', 2)");
	is(q1($n2, "SELECT count(*) FROM ra.t WHERE id BETWEEN $b AND $b + 9"), 2, "$nm: a multi-row INSERT lands on the owner");
	is(q1($n, "SELECT count(*) FROM ra.t WHERE id BETWEEN $b AND $b + 9"), 2, "$nm: and is read back from here");
	q1($n, "UPDATE ra.t SET n = n + 10 WHERE id = $b + 1");
	is(q1($n2, "SELECT n FROM ra.t WHERE id = $b + 1"), 11, "$nm: an UPDATE");
	q1($n, "INSERT INTO ra.t SELECT id + 1000, v || 'x', n FROM ra.t WHERE id BETWEEN $b AND $b + 9");
	is(q1($n2, 'SELECT count(*) FROM ra.t WHERE id BETWEEN ' . ($b + 1000) . ' AND ' . ($b + 1009)), 2, "$nm: an INSERT ... SELECT of the same schema");
	is(q1($n, "INSERT INTO ra.t (id, v) VALUES ($b + 3, 'ret') RETURNING id || ':' || v"), ($b + 3) . ':ret', "$nm: INSERT ... RETURNING returns the row");
	q1($n, "INSERT INTO ra.t (id, v) VALUES ($b + 1, 'dup') ON CONFLICT (id) DO UPDATE SET v = EXCLUDED.v");
	is(q1($n2, "SELECT v FROM ra.t WHERE id = $b + 1"), 'dup', "$nm: ON CONFLICT DO UPDATE");
	q1($n, "INSERT INTO ra.u VALUES ($b + 1, $b + 1), ($b + 2, $b + 2)");
	q1($n, "UPDATE ra.t SET v = 'joined' FROM ra.u WHERE ra.u.t_id = ra.t.id AND ra.u.id = $b + 1");
	is(q1($n2, "SELECT v FROM ra.t WHERE id = $b + 1"), 'joined', "$nm: UPDATE ... FROM a table of the schema");
	q1($n, "DELETE FROM ra.t USING ra.u WHERE ra.u.t_id = ra.t.id AND ra.u.id = $b + 2");
	is(q1($n2, "SELECT count(*) FROM ra.t WHERE id = $b + 2"), 0, "$nm: DELETE ... USING");
	q1($n, "WITH d AS (DELETE FROM ra.t WHERE id = $b + 3 RETURNING id) INSERT INTO ra.u SELECT id + 5000, id FROM d");
	is(q1($n2, 'SELECT count(*) FROM ra.u WHERE id = ' . ($b + 3 + 5000)), 1, "$nm: a data-modifying CTE");
	is(q1($n, "SELECT id FROM ra.t WHERE id = $b + 1 FOR UPDATE"), $b + 1, "$nm: SELECT ... FOR UPDATE");
	is(q1($n, "SELECT count(*) FROM (SELECT id, row_number() OVER (ORDER BY id) rn FROM ra.t WHERE id BETWEEN $b AND $b + 9) s WHERE rn = 1"), 1, "$nm: a window function runs on the owner");
	q1($n, "DELETE FROM ra.t WHERE id BETWEEN $b AND $b + 1100; DELETE FROM ra.u WHERE id BETWEEN $b AND $b + 6000");
	is(q1($n2, "SELECT count(*) FROM ra.t WHERE id BETWEEN $b AND $b + 1100"), 0, "$nm: DELETE of what the node wrote");
}
# the plan is one statement for the owner
{
	my $plan = q1($n1, q{EXPLAIN (COSTS off) SELECT * FROM ra.t WHERE id = 3});
	like($plan, qr/Kingres Remote Statement/, 'EXPLAIN on a node that does not hold the schema: the statement is shipped');
	like($plan, qr/Node: .* \(group 1, bucket 0\)/, '... to the group that holds it');
	my $own = q1($n2, q{EXPLAIN (COSTS off) SELECT * FROM ra.t WHERE id = 3});
	unlike($own, qr/Kingres/, 'EXPLAIN on the owner: the planner of PostgreSQL alone');
	my $off = q1($n1, q{SET kingres_shard.enable_schema_routing = off; EXPLAIN (COSTS off) SELECT * FROM ra.t WHERE id = 3});
	like($off, qr/Kingres Distributed Scan/, 'with routing off: the distributed scan');
}

# ---------------------------------------------------------------------------
# B. the same answer on both paths
# ---------------------------------------------------------------------------
q1($n2, q{INSERT INTO ra.t (id, v, n) SELECT g, 'row ' || g, g % 7 FROM generate_series(1, 60) g});
q1($n2, q{INSERT INTO ra.u SELECT g, g FROM generate_series(1, 40) g});
q1($n3, q{INSERT INTO rb.w SELECT g, g * 2 FROM generate_series(1, 30) g});
my @compare = (
	q{SELECT count(*), sum(n), min(v), max(v) FROM ra.t},
	q{SELECT n, count(*) FROM ra.t GROUP BY n ORDER BY n},
	q{SELECT t.id, u.id FROM ra.t t JOIN ra.u u ON u.t_id = t.id WHERE t.id < 8 ORDER BY 1},
	q{SELECT count(*) FROM ra.t WHERE id IN (SELECT t_id FROM ra.u)},
	q{WITH x AS (SELECT id FROM ra.t WHERE n = 3) SELECT count(*) FROM x JOIN ra.u u ON u.t_id = x.id},
	q{SELECT id, lag(id) OVER (ORDER BY id) FROM ra.t WHERE id < 6 ORDER BY id},
	q{SELECT count(*) FROM ra.t t JOIN public.ref r ON r.id = t.n},
	q{SELECT count(*) FROM ra.t t JOIN public.loc l ON l.id = t.n},
	q{SELECT count(*) FROM ra.t t JOIN rb.w w ON w.id = t.id},
	q{SELECT count(*) FROM ra.t t JOIN public.h h ON h.id = t.id},
	q{SELECT (SELECT count(*) FROM ra.t) + (SELECT count(*) FROM rb.w)},
	q{SELECT id FROM ra.t UNION ALL SELECT id FROM rb.w ORDER BY 1 LIMIT 5},
	q{SELECT count(DISTINCT n) FROM ra.t},
	q{SELECT string_agg(v, ',' ORDER BY id) FROM ra.t WHERE id < 5},
	q{SELECT count(*) FROM ra.t WHERE v LIKE 'row 1%'},
);
for my $n (@nodes)
{
	for my $sql (@compare)
	{
		my $on = q1($n, $sql);
		my $off = q1($n, "SET kingres_shard.enable_schema_routing = off;\n$sql");
		is($on, $off, $n->name . ": the same answer with routing on and off: $sql");
	}
}
{
	my $sum = 0;
	$sum += $_ % 7 for 1 .. 60;
	is(q1($n1, q{SELECT count(*) || '|' || sum(n) FROM ra.t}), "60|$sum", 'the numbers are the ones inserted');
}

# ---------------------------------------------------------------------------
# C. transactions
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, q{BEGIN; INSERT INTO ra.t (id, v) VALUES (9001, 'tx'); SELECT count(*) FROM ra.t WHERE id = 9001; ROLLBACK;});
	is($out, '1', 'a transaction reads its own write on the owner') or diag($err);
	is(q1($n2, q{SELECT count(*) FROM ra.t WHERE id = 9001}), 0, 'a rollback leaves nothing on the owner');
	($rc, $out, $err) = run_sql($n1, q{BEGIN; INSERT INTO ra.t (id, v) VALUES (9002, 'a'); INSERT INTO rb.w VALUES (9002, 1); COMMIT;});
	is($rc, 0, 'a transaction that writes two owners commits') or diag($err);
	is(q1($n2, q{SELECT count(*) FROM ra.t WHERE id = 9002}) . q1($n3, q{SELECT count(*) FROM rb.w WHERE id = 9002}), '11', '... on both');
	($rc, $out, $err) = run_sql($n1, q{BEGIN; INSERT INTO ra.t (id, v) VALUES (9003, 'a'); INSERT INTO rb.w VALUES (9003, 1); ROLLBACK;});
	is(q1($n2, q{SELECT count(*) FROM ra.t WHERE id = 9003}) . q1($n3, q{SELECT count(*) FROM rb.w WHERE id = 9003}), '00', 'a rollback of both leaves neither');
	($rc, $out, $err) = run_sql($n1, q{BEGIN; INSERT INTO ra.t (id, v) VALUES (9004, 'a'); INSERT INTO rb.w VALUES (1, 1); COMMIT;});
	isnt($rc, 0, 'a transaction whose second write fails (a duplicate key on the other owner)');
	is(q1($n2, q{SELECT count(*) FROM ra.t WHERE id = 9004}), 0, '... leaves nothing of the first write');
	($rc, $out, $err) = run_sql($n1, q{BEGIN; INSERT INTO ra.t (id, v) VALUES (9005, 'a'); SAVEPOINT s; INSERT INTO ra.t (id, v) VALUES (9005, 'dup'); ROLLBACK TO s; INSERT INTO ra.t (id, v) VALUES (9006, 'b'); COMMIT;}, on_error_stop => 0);
	is($rc, 0, 'a savepoint around a failed write') or diag($err);
	is(q1($n2, q{SELECT count(*) FROM ra.t WHERE id IN (9005, 9006)}), 2, '... keeps what came before and after it');
	q1($n2, q{DELETE FROM ra.t WHERE id >= 9000});
	q1($n3, q{DELETE FROM rb.w WHERE id >= 9000});
	for my $n (@nodes) { is(prepared_count($n), 0, 'nothing is prepared on ' . $n->name); }
}

# ---------------------------------------------------------------------------
# D. prepared statements and generic plans
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, q{
		SET plan_cache_mode = force_generic_plan;
		PREPARE pi (int, text) AS INSERT INTO ra.t (id, v) VALUES ($1, $2);
		PREPARE ps (int) AS SELECT v FROM ra.t WHERE id = $1;
		EXECUTE pi(8001, 'g1'); EXECUTE pi(8002, 'g2'); EXECUTE pi(8003, 'g3'); EXECUTE pi(8004, 'g4'); EXECUTE pi(8005, 'g5'); EXECUTE pi(8006, 'g6');
		EXECUTE ps(8001); EXECUTE ps(8004); EXECUTE ps(8006);
	});
	is($rc, 0, 'generic plans of a write and a read on a shell') or diag($err);
	is($out, "g1\ng4\ng6", '... the generic plan is routed: no key is needed');
	is(q1($n2, q{SELECT count(*) FROM ra.t WHERE id BETWEEN 8001 AND 8006}), 6, '... and the rows are on the owner');
	($rc, $out, $err) = run_sql($n1, q{
		DO $$ BEGIN
			FOR i IN 1 .. 20 LOOP
				INSERT INTO ra.t (id, v) VALUES (8100 + i, 'loop ' || i);
				IF (SELECT count(*) FROM ra.t WHERE id = 8100 + i) <> 1 THEN RAISE EXCEPTION 'loop % failed', i; END IF;
			END LOOP;
		END $$;
	});
	is($rc, 0, 'a PL/pgSQL loop that writes and reads through path S (plans of the cache)') or diag($err);
	is(q1($n2, q{SELECT count(*) FROM ra.t WHERE id BETWEEN 8101 AND 8120}), 20, '... 20 rows');
	q1($n2, q{DELETE FROM ra.t WHERE id >= 8000});
}

# ---------------------------------------------------------------------------
# E. sequences
# ---------------------------------------------------------------------------
{
	q1($n1, q{SELECT setval('ra.t_id_seq', 1000)});
	is(q1($n2, q{SELECT last_value FROM ra.t_id_seq}), 1000, 'setval of a sequence of the schema, called on a node that does not hold it, is the owner\'s');
	my @ids = map { q1($_, q{SELECT nextval('ra.t_id_seq')}) } ($n1, $n3, $n1, $n2, $n3);
	is(scalar(keys %{{ map { $_ => 1 } @ids }}), 5, 'nextval of a sequence of the schema, called on every node, gives five different numbers') or diag("@ids");
	my @sorted = sort { $a <=> $b } @ids;
	is($sorted[4] - $sorted[0], 4, '... in a row: they come from one sequence, the owner\'s') or diag("@ids");
	is(q1($n2, q{SELECT last_value FROM ra.t_id_seq}), $sorted[4], '... which is where the owner is');
	q1($n1, q{INSERT INTO ra.t (v) VALUES ('s1'), ('s2')});
	q1($n3, q{INSERT INTO ra.t (v) VALUES ('s3')});
	is(q1($n2, q{SELECT count(*) = count(DISTINCT id) FROM ra.t WHERE v IN ('s1', 's2', 's3')}), 't', 'the default of a serial column, from two nodes: the owner\'s sequence, no id twice');
	fails($n1, q{INSERT INTO public.loc SELECT nextval('ra.t_id_seq'), 'x'}, qr/sequence of a distributed schema that shard group 1 holds cannot be used from shard group 0 in this statement/, 'a statement that mixes a local table and a sequence of another group');
	q1($n2, q{DELETE FROM ra.t WHERE v IN ('s1', 's2', 's3')});
}

# ---------------------------------------------------------------------------
# F. functions
# ---------------------------------------------------------------------------
q1($n1, q{CREATE FUNCTION ra.plus(int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 100'});
{
	is(q1($n1, q{SELECT ra.plus(n) FROM ra.t WHERE id = 5}), 105, 'a function of the schema in a read: evaluated here, by the scan (the schema does not ship functions)');
	my $plan = q1($n1, q{EXPLAIN (COSTS off) SELECT ra.plus(n) FROM ra.t WHERE id = 5});
	like($plan, qr/Kingres Distributed Scan/, '... EXPLAIN shows the scan');
	fails($n1, q{INSERT INTO ra.t (id, v, n) SELECT 7001, 'f', ra.plus(1)}, qr/calls a function that is not built in, and the schema does not ship functions/, 'a write that calls a function of the schema, not shipped');
	q1($n1, q{SELECT kingres.alter_distributed_schema('ra', true)});
	is(q1($n3, q{SELECT ship_functions FROM kingres.dist_schema WHERE nsp = 'ra'::regnamespace}), 't', 'ship_functions reached every node');
	q1($n1, q{INSERT INTO ra.t (id, v, n) SELECT 7001, 'f', ra.plus(1)});
	is(q1($n2, q{SELECT n FROM ra.t WHERE id = 7001}), 101, 'with ship_functions the statement runs whole on the owner');
	$plan = q1($n1, q{EXPLAIN (COSTS off) SELECT ra.plus(n) FROM ra.t WHERE id = 5});
	like($plan, qr/Kingres Remote Statement/, '... and a read is shipped too');
	q1($n1, q{SELECT kingres.alter_distributed_schema('ra', false)});
	q1($n2, q{DELETE FROM ra.t WHERE id = 7001});
}

# ---------------------------------------------------------------------------
# G. what path S does not take
# ---------------------------------------------------------------------------
fails($n1, q{INSERT INTO ra.t (id, v) SELECT id + 7500, 'x' FROM rb.w}, qr/different shard groups hold|cannot be run whole on the owner/, 'a write that reads a schema of another owner');
fails($n1, q{UPDATE ra.t SET v = l.tag FROM public.loc l WHERE l.id = ra.t.id}, qr/not supported from shard group 0 yet|UPDATE, DELETE and MERGE that read/, 'a write that reads a local table');
fails($n1, q{INSERT INTO ra.t (id, v) SELECT g + 7600, 'x' FROM public.h g2, generate_series(1, 2) g WHERE g2.id = 1}, qr/is not supported from shard group 0 yet/, 'a write that reads a hash-distributed table');
q1($n1, q{MERGE INTO ra.t t USING ra.u u ON u.t_id = t.id WHEN MATCHED THEN UPDATE SET v = 'm' WHEN NOT MATCHED THEN DO NOTHING});
is(q1($n2, q{SELECT count(*) FROM ra.t WHERE v = 'm'}), 40, 'MERGE of two tables of the schema is shipped whole');
q1($n2, q{UPDATE ra.t SET v = 'row ' || id WHERE v = 'm'});
fails($n1, q{MERGE INTO ra.t t USING public.loc l ON l.id = t.id WHEN MATCHED THEN DELETE}, qr/not supported from shard group 0 yet/, 'MERGE that reads a local table');
is(q1($n1, q{INSERT INTO public.loc SELECT id + 100, v FROM ra.t WHERE id < 3 RETURNING id}), "101\n102", 'a write to a local table that reads a schema is a read of the schema');
q1($n1, q{DELETE FROM public.loc WHERE id > 100});
{
	my ($rc, $out, $err) = run_sql($n1, q{BEGIN; DECLARE c SCROLL CURSOR FOR SELECT id FROM ra.t WHERE id < 4 ORDER BY id; FETCH 3 FROM c; FETCH BACKWARD 1 FROM c; COMMIT;});
	is($rc, 0, 'a scrollable cursor on a shell is planned with the scan') or diag($err);
	is($out, "1\n2\n3\n2", '... and goes backwards');
	($rc, $out, $err) = run_sql($n1, q{BEGIN; DECLARE c CURSOR FOR SELECT id FROM ra.t WHERE id < 4 ORDER BY id; FETCH 2 FROM c; COMMIT;});
	is($out, "1\n2", 'a cursor that is not scrollable is shipped');
}
is(q1($n1, q{SELECT count(*) FROM ra.t t JOIN public.ref r ON r.id = t.n}), q1($n2, q{SELECT count(*) FROM ra.t t JOIN public.ref r ON r.id = t.n}), 'a reference table read together with a table of the schema');
fails($n1, q{UPDATE public.ref r SET name = 'changed' FROM ra.t t WHERE t.id = r.id}, qr/writes a reference table and reads table "ra\.t"/, 'a write to a reference table that reads a table of the schema (it would change one copy)');
for my $n (@nodes) { is(q1($n, q{SELECT string_agg(name, ',' ORDER BY id) FROM public.ref}), 'r1,r2', $n->name . ': the reference table is unchanged'); }
fails($n1, q{WITH x AS (UPDATE public.ref SET name = name RETURNING id) SELECT count(*) FROM ra.t, x}, qr/reference table/, 'a write to a reference table that reads a table of the schema');

# ---------------------------------------------------------------------------
# H. privileges, and the setting
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, q{INSERT INTO ra.t (id, v) VALUES (7700, 'alice')}, extra_params => [ '-U', 'alice' ]);
	isnt($rc, 0, 'a role with no privilege on the table cannot write it through path S');
	like($err, qr/permission denied/, '... the answer is a permission error') or diag($err);
	q1($n1, q{GRANT USAGE ON SCHEMA ra TO alice; GRANT INSERT, SELECT ON ra.t TO alice});
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO ra.t (id, v) VALUES (7700, 'alice')}, extra_params => [ '-U', 'alice' ]);
	is($rc, 0, 'after the GRANT it can') or diag($err);
	is(q1($n2, q{SELECT count(*) FROM ra.t WHERE id = 7700}), 1, '... the row is on the owner');
	($rc, $out, $err) = run_sql($n1, q{DELETE FROM ra.t WHERE id = 7700}, extra_params => [ '-U', 'alice' ]);
	isnt($rc, 0, 'a role that may only insert cannot delete');
	q1($n2, q{DELETE FROM ra.t WHERE id = 7700});
}
fails($n1, q{SET kingres_shard.enable_schema_routing = off; INSERT INTO ra.t (id, v) VALUES (7800, 'off')}, qr/kingres_shard\.enable_schema_routing is off/, 'with routing off a write to a shell is refused and says why');
is(q1($n1, q{SET kingres_shard.enable_schema_routing = off; SELECT count(*) FROM ra.t WHERE id < 4}), 3, 'with routing off a read works (the scan)');

# ---------------------------------------------------------------------------
# I. a map that differs between nodes
# ---------------------------------------------------------------------------
{
	my $c = q1($n1, q{SELECT colocation_id FROM kingres.dist_schema WHERE nsp = 'ra'::regnamespace});
	my $g3 = q1($n1, q{SELECT group_id FROM kingres.node WHERE node_id = 3});
	my $g2 = q1($n1, q{SELECT group_id FROM kingres.node WHERE node_id = 2});
	q1($n1, "SELECT kingres.internal_set_schema_group($c, $g3, 'active')");
	my ($rc, $out, $err) = run_sql($n1, qq{\\set VERBOSITY sqlstate\nSELECT count(*) FROM ra.t});
	isnt($rc, 0, 'node 1 sends the statement to node 3, which does not hold the schema: no answer');
	like($err, qr/KS001/, '... the retryable error') or diag($err);
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO ra.t (id, v) VALUES (7900, 'stale')});
	isnt($rc, 0, 'a write to the wrong node fails too');
	like($err, qr/does not hold table "ra\.t"/, '... and the node says so') or diag($err);
	q1($n1, "SELECT kingres.internal_set_schema_group($c, $g2, 'active')");
	is(q1($n1, q{SELECT count(*) FROM ra.t WHERE id = 7900}), 0, 'with the map the same again the statement is not there');
}
for my $n (@nodes) { is(prepared_count($n), 0, 'nothing is prepared on ' . $n->name); }

done_testing();
