# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/112_row_router.pl
#
# Step 3 of phase 2b (design/shard-phase2b.md, section 17): the row router for INSERT.  These are
# the router part of the design's 056 and the insert part of its 058 (the numbers 056 to 069 of
# the design are taken by tests of 0.5).  Every answer is compared with a single ordinary node that
# gets the same statements, and with the same rows inserted one statement at a time (the row-by-row
# path of 2a, which no router touches); a test that passes because nothing was routed fails on the
# plan assertion, which names the custom scan.
#
#   A. the plans: which statements are routed, which stay on the paths of 2a, the switch, the refusals;
#   B. the data: multi-row VALUES, INSERT ... SELECT (from a distributed, a local and the same table),
#      a computed key, parameters and generic plans, batches of a few rows and of a few bytes;
#   C. RETURNING, ON CONFLICT DO NOTHING, defaults, identity, generated columns, constraints;
#   D. values that must cross intact (hostile text, every type), under every session profile;
#   E. errors leave nothing behind: a unique violation on a group, a check, a savepoint.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql prepared_count);
use KingresShardCompare;

my $c = KingresShardCompare->new('rr');
my ($n1, $n2, $n3) = $c->nodes;
my $single = $c->single;

# A table on the cluster (distributed by the clause $dist) and on the single node.
sub table
{
	my ($create, $dist) = @_;
	$c->on_cluster($create . ($dist // ''));
	$c->on_single($create);
}

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# The rows a node holds in its own partitions, and how many of them belong to its group: a row in
# a wrong group is a bug that the totals do not show.
sub local_rows
{
	my ($node, $table, $group, $key) = @_;
	$key //= 'id';
	return $node->safe_psql('postgres', qq{
		SET kingres_shard.enable_routing = off;
		SELECT count(*) || '|' || count(*) FILTER (WHERE m.group_id = $group)
		  FROM $table k JOIN kingres.bucket_map m
		    ON m.colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = '$table'::regclass)
		   AND m.bucket = kingres.bucket_for('$table'::regclass, k.$key)});
}

sub placement_ok
{
	my ($table, $name, $key) = @_;
	my $total = 0;
	my $i = 0;
	for my $n ($n1, $n2, $n3)
	{
		my ($all, $mine) = split /\|/, local_rows($n, $table, $i, $key);
		is($mine, $all, "$name: every row of node " . ($i + 1) . " belongs to its group");
		$total += $all;
		$i++;
	}
	return $total;
}

sub sorted
{
	return join("\n", sort split /\n/, $_[0]);
}

# ---------------------------------------------------------------------------
# A. the plans
# ---------------------------------------------------------------------------
table(q{CREATE TABLE public.kv (id int PRIMARY KEY, v text, n int DEFAULT 7)},
	q{; SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.loc (id int, w text)});

my $router = 'Kingres Row Router';
like(plan_of($n1, q{INSERT INTO public.kv (id, v) VALUES (1, 'a'), (2, 'b')}), qr/\Q$router\E/,
	'a multi-row VALUES is planned as the row router');
like(plan_of($n1, q{INSERT INTO public.kv SELECT g, 'x' FROM generate_series(1, 10) g}), qr/\Q$router\E/,
	'INSERT ... SELECT is planned as the row router');
like(plan_of($n1, q{INSERT INTO public.kv VALUES ((random() * 0)::int + 5, 'x')}), qr/\Q$router\E/,
	'a single row whose key is not a constant is planned as the row router');
my $remote_key = $n1->safe_psql('postgres', q{SELECT g FROM generate_series(1, 100) g
	WHERE (SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, g)) = 1 LIMIT 1});
my $local_key = $n1->safe_psql('postgres', q{SELECT g FROM generate_series(1, 100) g
	WHERE (SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, g)) = 0 LIMIT 1});
my $p = plan_of($n1, qq{INSERT INTO public.kv VALUES ($remote_key, 'x')});
like($p, qr/Kingres Remote Statement/, 'a single row with a constant key of another group is forwarded as before');
unlike($p, qr/\Q$router\E/, '... and is not routed');
unlike(plan_of($n1, qq{INSERT INTO public.kv VALUES ($local_key, 'x')}), qr/\Q$router\E|Remote Statement/,
	'a single row with a constant key of this group is the plain local plan');

# the switch: off is the refusal of 2a, word for word
for my $sql (q{INSERT INTO public.kv VALUES (1, 'a'), (2, 'b')},
			 q{INSERT INTO public.kv SELECT g, 'x' FROM generate_series(1, 3) g},
			 q{INSERT INTO public.kv VALUES ((random() * 0)::int + 5, 'x')})
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_bulk_routing = off; $sql");
	isnt($rc, 0, "with enable_bulk_routing off the statement is refused: $sql");
	like($err, qr/statements that span shard groups are not supported yet/, '... with the message of 2a');
}
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), 0, 'a refused statement inserted nothing');

# forms the router refuses, each with its reason
$c->on_cluster(q{CREATE VIEW public.kvchk AS SELECT id, v FROM public.kv WHERE id > 0 WITH CHECK OPTION});
my @refused = (
	[ q{INSERT INTO public.kv VALUES (1, 'a'), (2, 'b') ON CONFLICT (id) DO UPDATE SET v = excluded.v},
	  qr/ON CONFLICT DO UPDATE is not supported yet for an INSERT that goes to several shard groups/ ],
	[ q{WITH x AS (SELECT 1 AS id) INSERT INTO public.kv SELECT id, 'a' FROM x},
	  qr/a WITH clause on an INSERT into a distributed table of several shard groups is not supported yet/ ],
	[ q{INSERT INTO public.kv VALUES (1, 'a'), (2, 'b') RETURNING id, (SELECT 1)},
	  qr/RETURNING list of this INSERT .* cannot be run by the shard groups/ ],
	[ q{INSERT INTO public.kv VALUES (1, 'a'), (2, 'b') RETURNING old.id, new.id},
	  qr/RETURNING with OLD or NEW is not supported yet/ ],
	[ q{INSERT INTO public.kvchk VALUES (1, 'a'), (2, 'b')},
	  qr/cannot check the rows against a view or a row-level security policy/ ],
);
for my $r (@refused)
{
	my ($rc, $out, $err) = run_sql($n1, $r->[0]);
	isnt($rc, 0, "refused: $r->[0]");
	like($err, $r->[1], '... with a clear reason');
}
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), 0, 'refused statements inserted nothing');

# a local target is not the router's business (the rows are read through the scan), and the
# triggers of foreign keys must not be skipped by a session setting
unlike(plan_of($n1, q{INSERT INTO public.loc SELECT id, v FROM public.kv}), qr/\Q$router\E/,
	'INSERT into a local table from a distributed table is not routed');
{
	my ($rc, $out, $err) = run_sql($n1, q{SET session_replication_role = replica; INSERT INTO public.kv VALUES (1, 'a'), (2, 'b')});
	isnt($rc, 0, 'a bulk write is refused while session_replication_role is not origin');
	like($err, qr/cannot be written while session_replication_role is/, '... with the message of the other writes');
}

# ---------------------------------------------------------------------------
# B. the data
# ---------------------------------------------------------------------------
$c->compare_dml('multi-row VALUES', q{INSERT INTO public.kv (id, v) VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd'), (5, 'e'), (6, 'f'), (7, 'g'), (8, 'h')});
$c->compare_tables('public.kv', order_by => 'id', name => 'after the VALUES');
is(placement_ok('public.kv', 'after the VALUES'), 8, 'every row is in some group, once');

$c->compare_dml('INSERT ... SELECT from generate_series',
	q{INSERT INTO public.kv (id, v) SELECT g, 'g' || g FROM generate_series(100, 399) g});
$c->compare_tables('public.kv', order_by => 'id', name => 'after the SELECT from generate_series');
is(placement_ok('public.kv', 'after the SELECT'), 308, 'three hundred and eight rows in all');

table(q{CREATE TABLE public.src (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.src', 'id', colocate_with => 'public.kv')});
$c->compare_dml('fill the source', q{INSERT INTO public.src (id, w) SELECT g, 'w' || g FROM generate_series(1000, 1099) g});
$c->compare_dml('INSERT ... SELECT from a distributed table (rows move between groups)',
	q{INSERT INTO public.kv (id, v) SELECT id + 5000, w FROM public.src});
$c->compare_dml('INSERT ... SELECT from the table itself',
	q{INSERT INTO public.kv (id, v) SELECT id + 20000, v FROM public.kv WHERE id < 400});
$c->compare_tables('public.kv', order_by => 'id', name => 'after reading the table itself');
$c->on_cluster(q{INSERT INTO public.loc SELECT g, 'l' || g FROM generate_series(1, 30) g});
$c->on_single(q{INSERT INTO public.loc SELECT g, 'l' || g FROM generate_series(1, 30) g});
$c->compare_dml('INSERT ... SELECT from a local table',
	q{INSERT INTO public.kv (id, v) SELECT id + 30000, w FROM public.loc});
$c->compare_dml('INSERT ... SELECT with a join of a distributed and a local table',
	q{INSERT INTO public.kv (id, v) SELECT s.id + 40000, l.w FROM public.src s JOIN public.loc l ON l.id = s.id - 1000 + 1});
$c->compare_tables('public.kv', order_by => 'id', name => 'after the distributed, local and joined sources');
placement_ok('public.kv', 'after the sources');

# a computed key
$c->compare_dml('a single row with a key from a volatile function', q{INSERT INTO public.kv (id, v) VALUES ((random() * 0)::int + 50001, 'volatile')});
$c->compare_dml('a single row with a key from now()', q{INSERT INTO public.kv (id, v) VALUES (extract(year from now())::int + 60000, 'year')});
$c->compare_tables('public.kv', order_by => 'id', name => 'after the computed keys');

# a text key that is the second of three columns, and a column the statement skips before it
table(q{CREATE TABLE public.kk (a int DEFAULT 1, name text PRIMARY KEY, b text)},
	q{; SELECT kingres.create_distributed_table('public.kk', 'name', bucket_count => 12)});
$c->compare_dml('a text key that is not the first column', q{INSERT INTO public.kk (name, b) SELECT 'k' || g, 'b' || g FROM generate_series(1, 120) g});
$c->compare_dml('the same with the columns in another order', q{INSERT INTO public.kk (b, name, a) VALUES ('x', 'o1', 5), ('y', 'o2', 6), ('z', 'o3', 7)});
$c->compare_tables('public.kk', order_by => 'name', name => 'after the text key');
is(placement_ok('public.kk', 'text key', 'name'), 123, 'every row of the text key is in the group of its bucket');

# the same rows, one statement at a time (the paths of 2a), compared with the bulk load
table(q{CREATE TABLE public.kv_rbr (id int PRIMARY KEY, v text, n int DEFAULT 7)},
	q{; SELECT kingres.create_distributed_table('public.kv_rbr', 'id', colocate_with => 'public.kv')});
my $script = join('', map { sprintf("INSERT INTO public.kv_rbr (id, v) VALUES (%d, 'r%d');\n", $_, $_) } (1 .. 150));
my ($rc) = run_sql($n1, $script);
is($rc, 0, 'row-by-row loading works');
$c->on_single($script);
$c->on_cluster(q{INSERT INTO public.kv_rbr (id, v) SELECT g, 'q' || g FROM generate_series(1001, 1150) g});
$c->on_single(q{INSERT INTO public.kv_rbr (id, v) SELECT g, 'q' || g FROM generate_series(1001, 1150) g});
$c->compare_tables('public.kv_rbr', order_by => 'id', name => 'row by row and bulk into one table');
{
	# the bulk-loaded kv holds ids 1..8, 100..399 and more; the rows of kv_rbr that are in both agree
	my $diff = $n1->safe_psql('postgres', q{SELECT count(*) FROM public.kv_rbr r JOIN public.kv k USING (id) WHERE k.v IS NOT NULL AND r.n IS DISTINCT FROM k.n});
	is($diff, 0, 'the default of the row-by-row and of the bulk rows is the same');
}

# batches of a few rows, and of a few bytes
{
	table(q{CREATE TABLE public.bt (id int PRIMARY KEY, v text)},
		q{; SELECT kingres.create_distributed_table('public.bt', 'id', colocate_with => 'public.kv')});
	$c->on_single(q{INSERT INTO public.bt SELECT g, repeat('x', 5000) FROM generate_series(1, 120) g});
	my ($r1, $o1, $e1) = run_sql($n1, "SET kingres_shard.insert_batch_rows = 3; INSERT INTO public.bt SELECT g, repeat('x', 5000) FROM generate_series(1, 120) g");
	is($r1, 0, 'batches of 3 rows') or diag($e1);
	$c->compare_tables('public.bt', order_by => 'id', name => 'batches of three rows');
	$c->on_cluster(q{TRUNCATE public.bt});
	$c->on_single(q{TRUNCATE public.bt});
	$c->on_single(q{INSERT INTO public.bt SELECT g, repeat('y', 9000) FROM generate_series(1, 120) g});
	($r1, $o1, $e1) = run_sql($n1, "SET kingres_shard.insert_batch_bytes = '16kB'; SET kingres_shard.insert_batch_rows = 100000;
		INSERT INTO public.bt SELECT g, repeat('y', 9000) FROM generate_series(1, 120) g");
	is($r1, 0, 'batches of 16 kB') or diag($e1);
	$c->compare_tables('public.bt', order_by => 'id', name => 'batches of sixteen kilobytes');
	placement_ok('public.bt', 'batches');
}

# parameters: a prepared statement, a generic plan, a PL/pgSQL function
{
	table(q{CREATE TABLE public.pm (id int PRIMARY KEY, v text)},
		q{; SELECT kingres.create_distributed_table('public.pm', 'id', colocate_with => 'public.kv')});
	my $sql = q{
		PREPARE ins2 (int, text, int, text) AS INSERT INTO public.pm VALUES ($1, $2), ($3, $4);
		EXECUTE ins2 (1, 'one', 2, 'two');
		SET plan_cache_mode = force_generic_plan;
		EXECUTE ins2 (3, 'three', 4, 'four');
		EXECUTE ins2 (5, 'five', 6, 'six');
		RESET plan_cache_mode;
		CREATE FUNCTION public.pm_fill(a int) RETURNS int LANGUAGE plpgsql AS $f$
		DECLARE n int;
		BEGIN
		  INSERT INTO public.pm SELECT g, 'f' || a FROM generate_series(a * 10, a * 10 + 4) g;
		  GET DIAGNOSTICS n = ROW_COUNT;
		  RETURN n;
		END $f$;
		SELECT public.pm_fill(1) + public.pm_fill(2);};
	my ($rc1, $o1, $e1) = run_sql($n1, $sql);
	my ($rc2, $o2, $e2) = run_sql($single, $sql);
	is($rc1, 0, 'prepared statements, a generic plan and a PL/pgSQL function run') or diag($e1);
	is($o1, $o2, '... and give the count of the single node');
	$c->compare_tables('public.pm', order_by => 'id', name => 'after the prepared statements');
}

# ---------------------------------------------------------------------------
# C. RETURNING, ON CONFLICT, defaults, identity, generated columns, constraints
# ---------------------------------------------------------------------------
{
	my $sql = q{INSERT INTO public.kv (id, v) VALUES (70001, 'ra'), (70002, 'rb'), (70003, 'rc'), (70004, 'rd') RETURNING id, upper(v), n + 1, id * 2};
	my ($rs, $os) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($n1, $sql);
	is($rd, 0, 'RETURNING with expressions') or diag($ed);
	is(sorted($od), sorted($os), '... returns the rows of the single node (the order is not promised)');
	my $star = q{INSERT INTO public.kv (id, v) SELECT g, 'star' FROM generate_series(70101, 70110) g RETURNING *};
	($rs, $os) = run_sql($single, $star);
	($rd, $od, $ed) = run_sql($n1, $star);
	is(sorted($od), sorted($os), 'RETURNING * from INSERT ... SELECT');
	$c->compare_dml('the count of rows of a VALUES', q{INSERT INTO public.kv (id, v) VALUES (70201, 'a'), (70202, 'b'), (70203, 'c')});
	$c->compare_tables('public.kv', order_by => 'id', name => 'after RETURNING');
	my $ids = $n1->safe_psql('postgres', q{INSERT INTO public.kv (id, v) SELECT g, 'ord' FROM generate_series(80001, 80060) g RETURNING id});
	my @ids = split /\n/, $ids;
	is(scalar(@ids), 60, 'RETURNING gives every row once');
	is(join(',', sort { $a <=> $b } @ids), join(',', 80001 .. 80060), '... the rows that were inserted');
}

# ON CONFLICT DO NOTHING
{
	table(q{CREATE TABLE public.oc2 (id int PRIMARY KEY, v text)},
		q{; SELECT kingres.create_distributed_table('public.oc2', 'id', colocate_with => 'public.kv')});
	$c->compare_dml('seed for ON CONFLICT', q{INSERT INTO public.oc2 SELECT g, 'old' FROM generate_series(1, 20) g});
	for my $clause ('ON CONFLICT DO NOTHING', 'ON CONFLICT (id) DO NOTHING', 'ON CONFLICT ON CONSTRAINT oc2_pkey DO NOTHING',
					'ON CONFLICT (id) WHERE id > 0 DO NOTHING')
	{
		my $sql = "INSERT INTO public.oc2 SELECT g, 'new' || g FROM generate_series(10, 40) g $clause";
		$c->compare_dml("$clause: the count of rows skips the conflicts", $sql);
		$c->compare_tables('public.oc2', order_by => 'id', name => "after $clause");
		$c->on_both(q{DELETE FROM public.oc2 WHERE id > 20});
		$c->on_both(q{UPDATE public.oc2 SET v = 'old'});
	}
	my $dup = q{INSERT INTO public.oc2 SELECT (g % 25) + 100, 'd' || g FROM generate_series(1, 100) g ON CONFLICT DO NOTHING};
	run_sql($single, $dup);
	($rc) = run_sql($n1, "SET kingres_shard.insert_batch_rows = 4; $dup");
	is($rc, 0, 'ON CONFLICT DO NOTHING with duplicates in one statement, batches of four rows');
	$c->compare_tables('public.oc2', order_by => 'id', name => 'after duplicates inside one statement');
	my $r = q{INSERT INTO public.oc2 SELECT g, 'rr' FROM generate_series(15, 25) g ON CONFLICT DO NOTHING RETURNING id};
	my ($rs, $os) = run_sql($single, $r);
	my ($rd, $od) = run_sql($n1, $r);
	is(sorted($od), sorted($os), 'RETURNING of ON CONFLICT DO NOTHING gives the inserted rows only');
	# a unique violation without DO NOTHING
	my ($ru, $ou, $eu) = run_sql($n1, q{INSERT INTO public.oc2 SELECT g, 'x' FROM generate_series(18, 30) g});
	isnt($ru, 0, 'a duplicate key without DO NOTHING fails');
	like($eu, qr/duplicate key value violates unique constraint/, '... with the message of PostgreSQL');
	like($eu, qr/Key \(id\)=\(\d+\) already exists/, '... and the key of the group');
	like($eu, qr/bulk insert of public\.oc2, batch \d+ to group \d/, '... and the batch that failed');
	my $before = $single->safe_psql('postgres', 'SELECT count(*) FROM public.oc2');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.oc2'), $before, '... nothing of the statement is left in any group');
	is(prepared_count($_), 0, '... and no prepared transaction') for ($n1, $n2, $n3);
}

# defaults, identity, serial, generated columns
{
	table(q{CREATE TABLE public.df (
			id int PRIMARY KEY,
			a int DEFAULT 5,
			b text DEFAULT 'dflt',
			c int DEFAULT (3 + 4),
			s serial,
			ia bigint GENERATED ALWAYS AS IDENTITY,
			ib bigint GENERATED BY DEFAULT AS IDENTITY,
			g int GENERATED ALWAYS AS (a * 2) STORED
		)},
		q{; SELECT kingres.create_distributed_table('public.df', 'id', colocate_with => 'public.kv')});
	$c->compare_dml('defaults, serial and identity columns are computed once per row', q{INSERT INTO public.df (id) SELECT g FROM generate_series(1, 40) g});
	$c->compare_dml('some columns named', q{INSERT INTO public.df (id, a, ib) VALUES (101, 1, 500), (102, 2, 501), (103, 3, 502)});
	$c->compare_dml('DEFAULT in the VALUES', q{INSERT INTO public.df (id, a, b) VALUES (201, DEFAULT, 'x'), (202, 9, DEFAULT)});
	$c->compare_dml('OVERRIDING SYSTEM VALUE', q{INSERT INTO public.df (id, ia) OVERRIDING SYSTEM VALUE VALUES (301, 9001), (302, 9002)});
	$c->compare_dml('OVERRIDING USER VALUE', q{INSERT INTO public.df (id, ib) OVERRIDING USER VALUE VALUES (303, 9100), (304, 9101)});
	$c->compare_tables('public.df', order_by => 'id', name => 'defaults, identity and generated columns');
	my ($rc1, $o1, $e1) = run_sql($n1, q{INSERT INTO public.df (id, ia) VALUES (401, 1), (402, 2)});
	isnt($rc1, 0, 'a value for a GENERATED ALWAYS identity column without OVERRIDING is refused');
	like($e1, qr/cannot insert a non-DEFAULT value into column "ia"/, '... as on a single node');
	($rc1, $o1, $e1) = run_sql($n1, q{INSERT INTO public.df (id, g) VALUES (403, 1), (404, 2)});
	isnt($rc1, 0, 'a value for a generated column is refused');
	like($e1, qr/cannot insert a non-DEFAULT value into column "g"/, '... as on a single node');
	# the sequences are the origin's: rows inserted from another node draw from that node's sequence
	my ($rc2) = run_sql($n2, q{INSERT INTO public.df (id) SELECT g FROM generate_series(501, 510) g});
	is($rc2, 0, 'an insert from another node');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.df WHERE id BETWEEN 501 AND 510}), 10, '... stores its rows');
	placement_ok('public.df', 'defaults');
}

# constraints and foreign keys on the groups
{
	$c->on_cluster(q{CREATE TABLE public.refd (k int PRIMARY KEY); SELECT kingres.create_reference_table('public.refd'); INSERT INTO public.refd SELECT generate_series(1, 5)});
	$c->on_single(q{CREATE TABLE public.refd (k int PRIMARY KEY); INSERT INTO public.refd SELECT generate_series(1, 5)});
	table(q{CREATE TABLE public.ck (id int PRIMARY KEY, k int NOT NULL, q int CHECK (q >= 0))},
		q{; SELECT kingres.create_distributed_table('public.ck', 'id', colocate_with => 'public.kv')});
	$c->on_cluster(q{ALTER TABLE public.ck ADD FOREIGN KEY (k) REFERENCES public.refd (k)});
	$c->on_single(q{ALTER TABLE public.ck ADD FOREIGN KEY (k) REFERENCES public.refd (k)});
	$c->compare_dml('rows that satisfy the constraints', q{INSERT INTO public.ck SELECT g, 1 + g % 5, g FROM generate_series(1, 30) g});
	my ($rc1, $o1, $e1) = run_sql($n1, q{INSERT INTO public.ck SELECT g, 1, g FROM generate_series(100, 119) g UNION ALL SELECT 200, 99, 1});
	isnt($rc1, 0, 'a foreign key violation on a group fails the statement');
	like($e1, qr/violates foreign key constraint/, '... with the message of PostgreSQL');
	($rc1, $o1, $e1) = run_sql($n1, q{INSERT INTO public.ck SELECT g, 1, g FROM generate_series(100, 119) g UNION ALL SELECT 201, 1, -1});
	isnt($rc1, 0, 'a check violation fails the statement');
	like($e1, qr/violates check constraint/, '... with the message of PostgreSQL');
	($rc1, $o1, $e1) = run_sql($n1, q{INSERT INTO public.ck SELECT g, NULL, g FROM generate_series(100, 119) g});
	isnt($rc1, 0, 'a not null violation fails the statement');
	like($e1, qr/null value in column "k"/, '... with the message of PostgreSQL');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.ck WHERE id >= 100}), 0, '... and none of the rows of the failed statements is stored');
	$c->compare_tables('public.ck', order_by => 'id', name => 'after the failed statements');
}

# ---------------------------------------------------------------------------
# D. values cross intact
# ---------------------------------------------------------------------------
{
	my $types = q{CREATE TYPE public.mood AS ENUM ('sad', 'ok', 'happy'); CREATE DOMAIN public.posint AS int CHECK (VALUE > 0)};
	$c->on_every_node($types);
	$c->on_single($types);
	table(q{CREATE TABLE public.ty (
			id int PRIMARY KEY, t text, vc varchar(20), n numeric(10,3), f8 float8, f4 float4, b boolean,
			d date, ts timestamp, tz timestamptz, iv interval, ba bytea, js jsonb, u uuid, ia int[], ta text[],
			m public.mood, p public.posint, c char(5), i8 bigint, r int4range)},
		q{; SELECT kingres.create_distributed_table('public.ty', 'id', colocate_with => 'public.kv')});
	my $hostile = q{ARRAY['plain', 'with,comma', 'with{brace}', 'with"quote', 'with\\back', 'NULL', '', ' lead and trail ', E'new\nline', E'tab\there', 'uni' || chr(233) || chr(8364), '{}', '"', '\\', ',', 'a b']};
	my $i = 0;
	for my $profile ('RESET ALL', @KingresShardCompare::PROFILES)
	{
		my $base = 1000 * ++$i;
		my $ins = qq{$profile;
			INSERT INTO public.ty
			SELECT $base + k, h, left(h, 20), k * 1.125, k / 7.0, (k / 3.0)::float4, k % 2 = 0,
			       date '2026-03-04' + k, timestamp '2026-03-04 05:06:07.891' + k * interval '1 hour',
			       timestamptz '2026-03-04 05:06:07.891+05' + k * interval '1 day', k * interval '1 day 02:03:04.5',
			       decode(lpad(to_hex(k), 4, '0'), 'hex'), jsonb_build_object('k', k, 's', h), md5(k::text)::uuid,
			       ARRAY[k, NULL, k + 1], ARRAY[h, NULL, 'NULL'], (ARRAY['sad', 'ok', 'happy'])[1 + k % 3]::public.mood,
			       k + 1, 'ab', k::bigint * 1000000007, int4range(k, k + 5)
			  FROM (SELECT k, (SELECT a[1 + k % 16] FROM (SELECT $hostile AS a) s) AS h FROM generate_series(1, 48) k) q;};
		my ($r1, $o1, $e1) = run_sql($n1, $ins);
		my ($r2, $o2, $e2) = run_sql($single, $ins);
		is($r1, 0, "every type under the session profile '" . substr($profile, 0, 30) . "'") or diag($e1);
		is($r2, 0, '... on the single node') or diag($e2);
	}
	$c->compare_tables('public.ty', order_by => 'id', name => 'every type, every profile');
	my ($rc1, $o1, $e1) = run_sql($n1, q{INSERT INTO public.ty (id, t) VALUES (900001, 'a,b"c\d{e}'),(900002, 'NULL'), (900003, NULL)});
	is($rc1, 0, 'hostile values in a VALUES list') or diag($e1);
	is($n1->safe_psql('postgres', q{SELECT t FROM public.ty WHERE id = 900001}), q{a,b"c\d{e}}, '... arrive as they were written');
	is($n1->safe_psql('postgres', q{SELECT (t IS NULL)::text || ',' || coalesce(t, '-') FROM public.ty WHERE id IN (900002, 900003) ORDER BY id}),
		"false,NULL\ntrue,-", '... and the text NULL is not a NULL');
}

# ---------------------------------------------------------------------------
# E. errors leave nothing behind
# ---------------------------------------------------------------------------
{
	my $before = $n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv');
	my ($rc1, $o1, $e1) = run_sql($n1, q{BEGIN; INSERT INTO public.kv SELECT g, 'sp' FROM generate_series(90001, 90100) g;
		SAVEPOINT s; INSERT INTO public.kv SELECT g, 'sp2' FROM generate_series(91001, 91100) g; ROLLBACK TO s;
		INSERT INTO public.kv SELECT g, 'sp3' FROM generate_series(92001, 92050) g; COMMIT});
	is($rc1, 0, 'a savepoint around a router insert') or diag($e1);
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), $before + 150, 'the rolled back statement left no rows, the others are in');
	($rc1, $o1, $e1) = run_sql($n1, q{DO $$ BEGIN
		  BEGIN INSERT INTO public.kv SELECT g, 'blk' FROM generate_series(93001, 93100) g UNION ALL SELECT 1, 'dup'; EXCEPTION WHEN unique_violation THEN RAISE NOTICE 'caught'; END;
		  INSERT INTO public.kv SELECT g, 'after' FROM generate_series(93201, 93210) g;
		END $$});
	is($rc1, 0, 'an error caught in a PL/pgSQL block') or diag($e1);
	like($e1, qr/caught/, '... is caught');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.kv WHERE v = 'blk'}), 0, 'the rows of the failed statement are gone');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.kv WHERE v = 'after'}), 10, 'a later statement of the same transaction works');
	is(prepared_count($_), 0, 'no prepared transaction is left') for ($n1, $n2, $n3);
	($rc1, $o1, $e1) = run_sql($n1, q{BEGIN READ ONLY; INSERT INTO public.kv VALUES (94001, 'ro'), (94002, 'ro'); COMMIT});
	isnt($rc1, 0, 'a read-only transaction cannot insert');
	like($e1, qr/read-only transaction/, '... and says so');
}

# a node that is not the one the table was created on
{
	my ($rc1, $o1, $e1) = run_sql($n3, q{INSERT INTO public.kv SELECT g, 'n3' FROM generate_series(95001, 95100) g RETURNING id});
	is($rc1, 0, 'an insert from the third node') or diag($e1);
	is(scalar(() = $o1 =~ /\d+/g), 100, '... returns every row');
	placement_ok('public.kv', 'from the third node');
}

# a source whose column type is only binary coercible to the target's (text into varchar, name, bpchar):
# the source SELECT that the router plans again must give the columns' types, not the sources'
{
	table(q{CREATE TABLE public.vc (id int PRIMARY KEY, a varchar(8), b name, c bpchar(4))},
		q{; SELECT kingres.create_distributed_table('public.vc', 'id', bucket_count => 12)});
	table(q{CREATE TABLE public.vsrc (id int, t text)});
	$c->on_both(q{INSERT INTO public.vsrc SELECT g, 'x' || g FROM generate_series(1, 40) g});
	$c->on_both(q{INSERT INTO public.vc SELECT id, t, t, left(t, 2) FROM public.vsrc});
	$c->compare('text sources into varchar, name and bpchar columns',
		q{SELECT id, a, b, '[' || c || ']' FROM public.vc ORDER BY id}, ordered => 1, profiles => 0);
}

done_testing();
